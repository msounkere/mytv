package preparation

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"mytv/internal/domain"
)

// Sonde de disponibilite (US-42 CA6) : quand personne ne regarde, lit 256 Kio a trois
// positions du fichier (0 %, 50 %, 90 %) par trois connexions SEQUENTIELLES (le
// fournisseur coupe les connexions paralleles) et compare le debit mesure au debit du
// fichier. Elle tient la place pendant la mesure (bail de la session qui demande) et
// suspend la preparation : jamais deux connexions a la fois.
type Sonde struct {
	opt   OptionsSonde
	srcs  map[string]domain.FournisseurVOD
	log   *slog.Logger
	now   func() time.Time
	mu    sync.Mutex
	cache map[domain.ContenuID]domain.Disponibilite
	enVol map[domain.ContenuID]bool
	// serie : une seule mesure a la fois, tous contenus confondus (jamais deux connexions).
	serie sync.Mutex
}

// OptionsSonde : dependances de la sonde.
type OptionsSonde struct {
	Fournisseurs map[string]domain.Fournisseur
	Stockage     domain.StockageVideo
	Baux         domain.Baux
	Occupation   Occupation
	// Prepares dit si un fichier complet existe (moteur) ; nil = jamais.
	Prepares interface{ EstPret(domain.ContenuID) bool }
	// Suspendre coupe la preparation le temps de la mesure (Moteur.Suspendre) ; nil = rien.
	Suspendre func() (reprendre func())
	// Debit rend le debit du fichier en kbit/s d'apres le fournisseur (0 = inconnu) ; nil = inconnu.
	Debit func(ctx context.Context, id, serie domain.ContenuID) int
	// Validite d'un resultat (defaut 10 min) ; Taille lue a chaque position (defaut 256 Kio) ;
	// DebitDefaut quand le fournisseur ne dit rien (defaut 8000 kbit/s) ; Delai total d'une
	// mesure (defaut 20 s).
	Validite    time.Duration
	Taille      int64
	DebitDefaut int
	Delai       time.Duration
	Horloge     func() time.Time
	Journal     *slog.Logger
}

// positions relatives des trois lectures.
var positions = []float64{0, 0.5, 0.9}

// NouvelleSonde construit la sonde.
func NouvelleSonde(o OptionsSonde) (*Sonde, error) {
	if o.Stockage == nil || o.Baux == nil || o.Occupation == nil {
		return nil, errors.New("preparation : sonde : dependance manquante (stockage, baux, occupation)")
	}
	if o.Validite <= 0 {
		o.Validite = 10 * time.Minute
	}
	if o.Taille <= 0 {
		o.Taille = 256 << 10
	}
	if o.DebitDefaut <= 0 {
		o.DebitDefaut = 8000
	}
	if o.Delai <= 0 {
		o.Delai = 20 * time.Second
	}
	if o.Horloge == nil {
		o.Horloge = time.Now
	}
	if o.Journal == nil {
		o.Journal = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	s := &Sonde{opt: o, srcs: map[string]domain.FournisseurVOD{}, log: o.Journal, now: o.Horloge,
		cache: map[domain.ContenuID]domain.Disponibilite{}, enVol: map[domain.ContenuID]bool{}}
	for id, f := range o.Fournisseurs {
		v, ok := f.(domain.FournisseurVOD)
		if !ok {
			return nil, fmt.Errorf("preparation : sonde : le fournisseur %q ne sert ni films ni series", id)
		}
		s.srcs[id] = v
	}
	return s, nil
}

// Disponibilite repond pour un film ou un episode (`serie` : sa serie, pour le debit du
// fichier ; vide pour un film). `session` tient le bail pendant la mesure.
//   - pret : un fichier prepare existe ;
//   - inconnu : une lecture est en cours (aucune mesure), ou la mesure est impossible ;
//   - immediat : debit mesure >= 2 x le debit du fichier ; lent sinon.
//
// Un resultat mesure est garde Validite.
func (s *Sonde) Disponibilite(ctx context.Context, session string, id, serie domain.ContenuID) (domain.Disponibilite, error) {
	if s.opt.Prepares != nil && s.opt.Prepares.EstPret(id) {
		return domain.Disponibilite{Etat: "pret", Mesure: s.now()}, nil
	}
	fid, _, ok := strings.Cut(string(id), ":")
	if !ok || s.srcs[fid] == nil {
		return domain.Disponibilite{}, domain.ErrNonTrouve
	}
	s.mu.Lock()
	if r, ok := s.cache[id]; ok && s.now().Sub(r.Mesure) < s.opt.Validite {
		s.mu.Unlock()
		return r, nil
	}
	if s.enVol[id] {
		s.mu.Unlock()
		return domain.Disponibilite{Etat: "inconnu", Mesure: s.now()}, nil
	}
	s.enVol[id] = true
	s.mu.Unlock()
	defer func() { s.mu.Lock(); delete(s.enVol, id); s.mu.Unlock() }()

	url, err := s.url(ctx, id, fid)
	if err != nil {
		return domain.Disponibilite{}, err
	}
	// Jamais de sonde pendant une lecture : une place tenue = inconnu, sans la moindre requete.
	if s.opt.Occupation.Occupes(fid) > 0 {
		return domain.Disponibilite{Etat: "inconnu", Mesure: s.now()}, nil
	}
	debitFichier := 0
	if s.opt.Debit != nil {
		debitFichier = s.opt.Debit(ctx, id, serie)
	}
	if debitFichier <= 0 {
		debitFichier = s.opt.DebitDefaut
	}
	ctx, annuler := context.WithTimeout(ctx, s.opt.Delai)
	defer annuler()
	s.serie.Lock()
	defer s.serie.Unlock()
	if s.opt.Occupation.Occupes(fid) > 0 { // une lecture a pu commencer pendant l'attente
		return domain.Disponibilite{Etat: "inconnu", Mesure: s.now()}, nil
	}
	if s.opt.Suspendre != nil {
		reprendre := s.opt.Suspendre()
		defer reprendre()
	}
	// La place est tenue pendant la mesure : une lecture qui arrive attend (patience des
	// baux) au lieu de se heurter a nos connexions. Attente courte : si la place vient
	// d'etre prise, c'est « inconnu ».
	ctxBail, fin := context.WithTimeout(ctx, 2*time.Second)
	bail, err := s.opt.Baux.Acquerir(ctxBail, fid, session, "sonde:"+string(id))
	fin()
	if err != nil {
		return domain.Disponibilite{Etat: "inconnu", Mesure: s.now()}, nil
	}
	defer bail.Liberer()
	kbps, err := s.mesurer(ctx, s.opt.Fournisseurs[fid], url)
	if err != nil {
		s.log.Info("sonde de disponibilite : mesure impossible", "contenu", string(id), "erreur", assainir(err))
		return domain.Disponibilite{Etat: "inconnu", Mesure: s.now()}, nil
	}
	r := domain.Disponibilite{Etat: "lent", DebitKbps: kbps, DebitFichierKbps: debitFichier, Mesure: s.now()}
	if kbps >= 2*debitFichier {
		r.Etat = "immediat"
	}
	s.mu.Lock()
	s.cache[id] = r
	if len(s.cache) > 1000 {
		for k, v := range s.cache {
			if s.now().Sub(v.Mesure) >= s.opt.Validite {
				delete(s.cache, k)
			}
		}
	}
	s.mu.Unlock()
	s.log.Info("sonde de disponibilite", "contenu", string(id), "etat", r.Etat, "debit_kbps", kbps, "debit_fichier_kbps", debitFichier)
	return r, nil
}

// url construit l'adresse amont par le fournisseur (catalogue pour un film, identifiant
// strict pour un episode) : rien de ce que le client envoie n'y entre.
func (s *Sonde) url(ctx context.Context, id domain.ContenuID, fid string) (string, error) {
	src := s.srcs[fid]
	reste := strings.TrimPrefix(string(id), fid+":")
	if numero, ext, episode := strings.Cut(reste, "."); episode {
		if numero == "" || strings.ContainsAny(numero+ext, "/\\?#%") {
			return "", domain.ErrNonTrouve
		}
		return src.URLEpisode(numero, ext), nil
	}
	f, err := s.opt.Stockage.Film(ctx, id)
	if err != nil {
		return "", err
	}
	return src.URLFilm(f.Numero, f.Extension), nil
}

// mesurer fait les trois lectures en serie et rend le debit global en kbit/s.
func (s *Sonde) mesurer(ctx context.Context, f domain.Fournisseur, url string) (int, error) {
	var total, lus int64
	var duree time.Duration
	buf := make([]byte, 64<<10)
	for i, p := range positions {
		if total > 0 && i > 0 && int64(p*float64(total)) >= total {
			break
		}
		debut := int64(0)
		if i > 0 {
			debut = int64(p * float64(total))
		}
		fin := debut + s.opt.Taille - 1
		if total > 0 {
			fin = min(fin, total-1)
		}
		h := http.Header{"Accept-Encoding": {"identity"}, "Range": {"bytes=" + strconv.FormatInt(debut, 10) + "-" + strconv.FormatInt(fin, 10)}}
		t0 := s.now()
		rep, err := f.Requete(ctx, url, h)
		if err != nil {
			return 0, err
		}
		n, err := lireTout(rep.Body, buf)
		rep.Body.Close()
		if err != nil {
			return 0, err
		}
		duree += s.now().Sub(t0)
		lus += n
		if i == 0 {
			if rep.StatusCode == http.StatusPartialContent {
				if _, _, tot, ok := analyserContentRange(rep.Header.Get("Content-Range")); ok {
					total = tot
				}
			} else if rep.StatusCode == http.StatusOK && rep.ContentLength > 0 {
				total = rep.ContentLength // Range ignore : la premiere lecture suffit
			}
			if total <= 0 {
				return 0, fmt.Errorf("%w : taille inconnue", errReponse)
			}
		}
	}
	if duree <= 0 || lus == 0 {
		return 0, fmt.Errorf("%w : rien lu", errReponse)
	}
	return int(float64(lus) * 8 / 1000 / duree.Seconds()), nil
}

// lireTout vide le corps (au plus 4 x la taille d'une lecture : un fournisseur qui
// ignore Range enverrait tout le film).
func lireTout(r io.Reader, buf []byte) (int64, error) {
	var n int64
	for n < 4<<20 {
		k, err := r.Read(buf)
		n += int64(k)
		if errors.Is(err, io.EOF) {
			return n, nil
		}
		if err != nil {
			return n, err
		}
	}
	return n, nil
}
