package remux

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mytv/internal/domain"
)

// fluxSonde sert UN film sur 127.0.0.1, le temps d'une analyse ffprobe.
//
// POURQUOI : ffprobe lisait l'adresse du fournisseur (`-i http://.../movie/<utilisateur>/
// <mot de passe>/1.mkv`) : les identifiants etaient lisibles de tout utilisateur de la
// machine (`ps`, /proc), et ffprobe suivait SEUL les redirections du fournisseur — y
// compris vers une adresse interne, sans le filtre d'hotes. Il lit maintenant une
// adresse locale a usage unique ; c'est le SERVEUR qui parle au fournisseur (client
// filtre, identifiants, Range). ffprobe voit un serveur HTTP ordinaire avec Range, donc
// se comporte exactement comme avant (index de fin de fichier compris), ce qu'un tube
// n'aurait pas permis.
//
// Garanties : ecoute sur la boucle locale seulement ; chemin secret aleatoire
// (128 bits) ; une seule requete amont a la fois (une connexion par compte chez le
// fournisseur) ; l'ecoute disparait avec l'analyse.
type fluxSonde struct {
	src    domain.SourceOctets
	chemin string
	ln     net.Listener
	srv    *http.Server
	fin    sync.WaitGroup
	url    string

	mu      sync.Mutex // protege courant (le passage de relais entre requetes)
	courant *requeteSonde
	muErr   sync.Mutex // protege erreur
	total   int64      // -1 tant que la taille n'est pas connue
	erreur  error      // premiere erreur de la source
}

func ouvrirFluxSonde(src domain.SourceOctets) (*fluxSonde, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("remux : ecoute locale de la sonde : %w", err)
	}
	jeton := make([]byte, 16)
	if _, err := rand.Read(jeton); err != nil {
		ln.Close()
		return nil, err
	}
	f := &fluxSonde{src: src, ln: ln, total: -1, chemin: "/" + hex.EncodeToString(jeton) + "/film"}
	f.url = "http://" + ln.Addr().String() + f.chemin
	f.srv = &http.Server{Handler: http.HandlerFunc(f.servir), ReadHeaderTimeout: 5 * time.Second}
	// Fermer attend aussi les requetes en cours (leurs lectures amont). Elles sont comptees
	// par CONNEXION, a l'acceptation (dans Serve, donc tant que le compteur de Serve tient)
	// et decomptees a la fermeture, apres la fin du gestionnaire. Un Add(1) au debut du
	// gestionnaire pouvait croiser le Wait de Fermer avec un compteur a zero (Serve deja
	// rendu) : « WaitGroup misuse », ou un Fermer qui n'attendait pas.
	f.srv.ConnState = func(_ net.Conn, e http.ConnState) {
		switch e {
		case http.StateNew:
			f.fin.Add(1)
		case http.StateClosed, http.StateHijacked:
			f.fin.Done()
		}
	}
	f.fin.Add(1)
	go func() { defer f.fin.Done(); _ = f.srv.Serve(ln) }()
	return f, nil
}

// Fermer coupe l'ecoute et les requetes en cours (donc les lectures amont), puis attend.
func (f *fluxSonde) Fermer() {
	_ = f.srv.Close()
	f.fin.Wait()
}

// Erreur : la premiere erreur de la source (404, 407, hors-fichier...), pour que l'appelant
// la rende telle quelle plutot que « ffprobe a echoue ».
func (f *fluxSonde) Erreur() error {
	f.muErr.Lock()
	defer f.muErr.Unlock()
	return f.erreur
}

func (f *fluxSonde) noter(err error) {
	f.muErr.Lock()
	if f.erreur == nil {
		f.erreur = err
	}
	f.muErr.Unlock()
}

// plageDemandee : une seule plage « bytes=a-b », « bytes=a- » ou « bytes=-n ».
type plageDemandee struct {
	presente bool
	debut    int64 // -1 : suffixe
	fin      int64 // -1 : ouverte
	suffixe  int64
}

func lirePlage(h string) plageDemandee {
	spec, ok := strings.CutPrefix(strings.TrimSpace(h), "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return plageDemandee{}
	}
	a, b, ok := strings.Cut(spec, "-")
	if !ok {
		return plageDemandee{}
	}
	if a == "" {
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 {
			return plageDemandee{}
		}
		return plageDemandee{presente: true, debut: -1, fin: -1, suffixe: n}
	}
	d, err := strconv.ParseInt(a, 10, 64)
	if err != nil || d < 0 {
		return plageDemandee{}
	}
	if b == "" {
		return plageDemandee{presente: true, debut: d, fin: -1}
	}
	e, err := strconv.ParseInt(b, 10, 64)
	if err != nil || e < d {
		return plageDemandee{}
	}
	return plageDemandee{presente: true, debut: d, fin: e}
}

// graceBascule : temps laisse a une requete en cours pour finir d'elle-meme quand une
// nouvelle arrive. Au-dela, elle est consideree comme abandonnee par ffprobe et coupee.
const graceBascule = 100 * time.Millisecond

// requeteSonde : une requete en cours vers l'amont, avec de quoi la couper.
type requeteSonde struct {
	fini    chan struct{}
	annuler context.CancelFunc
	rompre  func() // debloque une ecriture locale bloquee (delai d'ecriture immediat)
	coupee  atomic.Bool
}

// couper abandonne la requete : l'amont est ferme (contexte) et l'ecriture bloquee est liberee.
func (r *requeteSonde) couper() {
	r.coupee.Store(true)
	r.annuler()
	r.rompre()
}

// passer prend le relais de la requete precedente : une seule connexion amont a la fois
// (le fournisseur n'en accorde qu'une), mais sans attendre indefiniment.
//
// POURQUOI : pour se deplacer dans un MKV (index en fin de fichier), ffprobe ouvre une
// NOUVELLE connexion AVANT de fermer l'ancienne, qu'il ne lit plus. Avec un verrou tenu
// pendant tout l'envoi, la nouvelle requete attendait la fin d'un envoi de plusieurs Go
// que personne ne lit : blocage jusqu'au delai de l'analyse (constate en recette reelle).
func (f *fluxSonde) passer(moi *requeteSonde) (precedente *requeteSonde) {
	f.mu.Lock()
	precedente, f.courant = f.courant, moi
	f.mu.Unlock()
	return precedente
}

// attendreLaPrecedente rend vrai quand la precedente est terminee (d'elle-meme ou coupee).
func (f *fluxSonde) attendreLaPrecedente(ctx context.Context, prec *requeteSonde) bool {
	if prec == nil {
		return true
	}
	select {
	case <-prec.fini:
		return true
	case <-time.After(graceBascule):
		prec.couper()
	case <-ctx.Done():
		return false
	}
	select {
	case <-prec.fini:
		return true
	case <-ctx.Done():
		return false
	}
}

func (f *fluxSonde) servir(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path != f.chemin {
		http.NotFound(w, req)
		return
	}
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		http.Error(w, "methode", http.StatusMethodNotAllowed)
		return
	}
	ctx, annuler := context.WithCancel(req.Context())
	rc := http.NewResponseController(w)
	moi := &requeteSonde{fini: make(chan struct{}), annuler: annuler,
		rompre: func() { _ = rc.SetWriteDeadline(time.Now()) }}
	defer func() { annuler(); close(moi.fini) }()
	if !f.attendreLaPrecedente(ctx, f.passer(moi)) {
		return
	}
	p := lirePlage(req.Header.Get("Range"))

	// Premiere requete : la taille totale vient du corps ouvert (domain.AvecTaille). Quand
	// la plage demandee commence a un decalage connu (« bytes=N- », « bytes=N-M »), on ouvre
	// DIRECTEMENT la : ffmpeg (-ss) et ffprobe commencent souvent ainsi, et ouvrir a 0 pour
	// rouvrir a N jetait une connexion fournisseur, dont il garde la place.
	var corps io.ReadCloser
	var ouvert int64 = -1 // decalage deja ouvert dans `corps`
	if f.total < 0 {
		depuis := int64(0)
		if p.presente && p.debut > 0 {
			depuis = p.debut
		}
		c, err := f.src(ctx, depuis)
		if err != nil {
			f.echec(w, err)
			return
		}
		corps, ouvert = c, depuis
		if t, ok := c.(domain.AvecTaille); ok {
			f.total = t.Taille()
		}
		if f.total < 0 && depuis > 0 {
			// Sans taille connue, pas de plage possible : le flux part du debut.
			c.Close()
			corps = nil
			if c, err = f.src(ctx, 0); err != nil {
				f.echec(w, err)
				return
			}
			corps, ouvert = c, 0
		}
	}
	defer func() {
		if corps != nil {
			corps.Close()
		}
	}()

	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Cache-Control", "no-store")
	if f.total < 0 {
		// Taille inconnue : un flux simple, sans Range (ffprobe lira sequentiellement).
		w.WriteHeader(http.StatusOK)
		if req.Method == http.MethodGet {
			f.copier(w, corps, -1, moi)
		}
		return
	}
	debut, fin := int64(0), f.total-1
	if p.presente {
		switch {
		case p.debut < 0:
			debut = max(0, f.total-p.suffixe)
		default:
			debut = p.debut
			if p.fin >= 0 {
				fin = min(p.fin, f.total-1)
			}
		}
	}
	h.Set("Accept-Ranges", "bytes")
	if debut >= f.total || (f.total == 0 && p.presente) {
		h.Set("Content-Range", "bytes */"+strconv.FormatInt(f.total, 10))
		http.Error(w, "plage hors du fichier", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	longueur := fin - debut + 1
	h.Set("Content-Length", strconv.FormatInt(longueur, 10))
	statut := http.StatusOK
	if p.presente {
		statut = http.StatusPartialContent
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", debut, fin, f.total))
	}
	if req.Method == http.MethodHead {
		w.WriteHeader(statut)
		return
	}
	if ouvert != debut {
		if corps != nil {
			corps.Close()
		}
		c, err := f.src(ctx, debut)
		corps = c
		if err != nil {
			corps = nil
			f.echec(w, err)
			return
		}
	}
	w.WriteHeader(statut)
	f.copier(w, corps, longueur, moi)
}

// echec : la source n'a pas pu ouvrir. L'erreur est gardee pour l'appelant; ffprobe voit un 502.
func (f *fluxSonde) echec(w http.ResponseWriter, err error) {
	var plage domain.ErrPlageHorsFichier
	if errors.As(err, &plage) {
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(max(plage.Total, 0), 10))
		http.Error(w, "plage hors du fichier", http.StatusRequestedRangeNotSatisfiable)
		return
	}
	f.noter(err)
	http.Error(w, "amont indisponible", http.StatusBadGateway)
}

// copier envoie au plus n octets (n < 0 : tout). Une coupure de l'amont est notee puis
// la connexion locale est coupee (ffprobe voit un fichier tronque, pas un fichier complet).
func (f *fluxSonde) copier(w http.ResponseWriter, corps io.Reader, n int64, moi *requeteSonde) {
	var err error
	if n < 0 {
		_, err = io.Copy(w, corps)
	} else {
		var ecrit int64
		ecrit, err = io.CopyN(w, corps, n)
		if err == nil && ecrit != n {
			err = io.ErrUnexpectedEOF
		}
	}
	if err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		if !isClientParti(err) && !moi.coupee.Load() {
			f.noter(err)
		}
		panic(http.ErrAbortHandler)
	}
}

// isClientParti : l'ecriture locale a echoue (ffprobe a ferme sa connexion, il en sait assez).
func isClientParti(err error) bool {
	var op *net.OpError
	return errors.As(err, &op) && op.Op == "write"
}
