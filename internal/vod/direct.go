package vod

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"mytv/internal/domain"
)

// direct relaie les octets du fournisseur avec la plage demandee.
//
// On relaie au lieu de rediriger : une redirection mettrait les identifiants dans
// l'en-tete Location, sous les yeux du navigateur.
//
// Garanties, parce qu'un octet mal place ruine silencieusement une video :
//   - une plage demandee n'est servie que si le fournisseur repond 206 avec un
//     Content-Range qui correspond (un 200 plein corps serait recolle au mauvais
//     endroit par le navigateur) ;
//   - les octets sont comptes : jamais plus que la longueur annoncee, et un
//     corps trop court n'est pas dit complet ;
//   - une coupure en cours de route est reprise par une nouvelle requete Range
//     partant exactement de l'octet suivant, verifiee comme la premiere.
func (s *Service) direct(ctx context.Context, w http.ResponseWriter, req *http.Request, m *media, url string, debut time.Time, info infosLecture) error {
	f := m.src.f
	demande := analyserPlage(req.Header.Get("Range"))

	rep, err := s.requeteAmont(ctx, f, url, entetesPlage(demande), debut)
	if err != nil {
		return err
	}
	corps := rep.Body
	defer func() { corps.Close() }() // corps change si on reprend

	debutOctet, finOctet, total, err := verifierReponse(rep, demande)
	if err != nil {
		return err
	}
	longueur := finOctet - debutOctet + 1

	h := w.Header()
	h.Set("Content-Type", typeContenu(m.extension, info.brut))
	h.Set("Accept-Ranges", "bytes")
	h.Set("Content-Length", strconv.FormatInt(longueur, 10))
	h.Set("Cache-Control", "private, no-store")
	info.poser(h)
	statut := http.StatusOK
	if demande.presente {
		statut = http.StatusPartialContent
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", debutOctet, finOctet, total))
	}
	if req.Method == http.MethodHead {
		w.WriteHeader(statut)
		return nil
	}
	w.WriteHeader(statut)
	s.inst.Observer("vod_premier_octet_secondes", time.Since(debut).Seconds(), "mode", string(domain.ModeDirect))

	rc := http.NewResponseController(w)
	buf := make([]byte, s.r.Tampon)
	var copies int64
	reprises := 0
	for copies < longueur {
		n, rerr := corps.Read(buf[:min(int64(len(buf)), longueur-copies)])
		if n > 0 {
			if werr := s.ecrire(ctx, rc, w, buf[:n]); werr != nil {
				return werr // client parti ou en pause : le defer ferme la connexion amont
			}
			copies += int64(n)
			s.inst.Compter("vod_octets_total", float64(n), "mode", string(domain.ModeDirect))
		}
		if rerr == nil || (errors.Is(rerr, io.EOF) && copies == longueur) {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Coupure (ou fin de corps prematuree) : reprendre a l'octet suivant.
		if reprises >= s.r.Reprises {
			return fmt.Errorf("%w : %d octets sur %d, reprises epuisees", ErrCoupure, copies, longueur)
		}
		reprises++
		corps.Close()
		// La patience d'une reprise se compte depuis la coupure, pas depuis le debut du film.
		nouveau, err := s.reprendre(ctx, m, url, debutOctet+copies, finOctet, time.Now())
		if err != nil {
			return fmt.Errorf("%w : %d octets sur %d (%v)", ErrCoupure, copies, longueur, err)
		}
		corps = nouveau
		s.inst.Compter("vod_reprises_total", 1)
		s.inst.Evenement("vod:reprise", "relais direct repris apres une coupure du fournisseur")
	}
	return nil
}

func entetesPlage(p plage) http.Header {
	// Identity : les octets d'une plage sont ceux du fichier original.
	h := http.Header{"Accept-Encoding": {"identity"}}
	if p.presente {
		h.Set("Range", p.entete())
	}
	return h
}

// verifierReponse controle la reponse du fournisseur et renvoie les bornes
// (incluses) et la taille totale des octets qu'on va servir.
func verifierReponse(rep *http.Response, p plage) (debut, fin, total int64, err error) {
	refus := func(raison string) (int64, int64, int64, error) {
		return 0, 0, 0, fmt.Errorf("%w : %s", ErrReponseAmont, raison)
	}
	if rep.Header.Get("Content-Encoding") != "" && rep.Header.Get("Content-Encoding") != "identity" {
		return refus("corps encode")
	}
	if !p.presente {
		if rep.StatusCode != http.StatusOK {
			return refus("statut " + strconv.Itoa(rep.StatusCode) + " pour une lecture complete")
		}
		if rep.ContentLength < 0 {
			return refus("longueur inconnue") // impossible de savoir si la lecture est complete
		}
		return 0, rep.ContentLength - 1, rep.ContentLength, nil
	}
	if rep.StatusCode != http.StatusPartialContent {
		return refus("statut " + strconv.Itoa(rep.StatusCode) + " a la place de 206")
	}
	cr, ok := analyserContentRange(rep.Header.Get("Content-Range"))
	if !ok || !cr.coherente(p) {
		return refus("Content-Range incoherent avec la plage demandee")
	}
	if cr.total < 0 {
		return refus("taille totale inconnue")
	}
	if rep.ContentLength >= 0 && rep.ContentLength != cr.fin-cr.debut+1 {
		return refus("Content-Length different de Content-Range")
	}
	return cr.debut, cr.fin, cr.total, nil
}

// reprendre rouvre le flux a partir de `depuis` jusqu'a `fin` (inclus) et verifie
// que le fournisseur recolle exactement ce qu'on attend.
func (s *Service) reprendre(ctx context.Context, m *media, url string, depuis, fin int64, debut time.Time) (io.ReadCloser, error) {
	p := plage{presente: true, debut: depuis, fin: fin}
	rep, err := s.requeteAmont(ctx, m.src.f, url, entetesPlage(p), debut)
	if err != nil {
		return nil, err
	}
	d, f, _, err := verifierReponse(rep, p)
	if err == nil && (d != depuis || f != fin) {
		err = fmt.Errorf("%w : la reprise ne commence pas ou ne finit pas ou il faut", ErrReponseAmont)
	}
	if err != nil {
		rep.Body.Close()
		return nil, err
	}
	return rep.Body, nil
}

// sourceAmont donne a ffmpeg les octets du film, lus ICI avec le client du
// fournisseur (identifiants, redirections, filtre d'hotes) : l'URL amont ne quitte
// jamais ce processus. Chaque ouverture est verifiee comme une lecture directe :
// 206 et Content-Range exacts quand on reprend en cours de route (un 200 plein
// corps serait recolle au mauvais endroit), longueur connue, corps non encode.
// Un corps plus court que prevu rend io.ErrUnexpectedEOF (le lanceur reprend).
func (s *Service) sourceAmont(m *media, url string) domain.SourceOctets {
	return func(ctx context.Context, depuis int64) (io.ReadCloser, error) {
		p := plage{presente: depuis > 0, debut: depuis, fin: -1}
		// Chaque ouverture a son propre budget de patience (elle peut survenir des heures
		// apres le debut de la requete du client).
		rep, err := s.requeteAmont(ctx, m.src.f, url, entetesPlage(p), time.Now())
		if err != nil {
			return nil, err
		}
		d, f, total, err := verifierReponse(rep, p)
		if err == nil && d != depuis {
			err = fmt.Errorf("%w : la reprise ne commence pas ou il faut", ErrReponseAmont)
		}
		if err != nil {
			rep.Body.Close()
			return nil, err
		}
		return &octetsExacts{corps: rep.Body, restant: f - d + 1, total: total}, nil
	}
}

// octetsExacts ne rend jamais plus que la longueur annoncee et signale un corps
// trop court (coupure) au lieu d'un EOF normal.
type octetsExacts struct {
	corps   io.ReadCloser
	restant int64
	total   int64 // taille du fichier entier (domain.AvecTaille)
}

// Taille : taille TOTALE du fichier (et non ce qui reste a lire).
func (o *octetsExacts) Taille() int64 { return o.total }

func (o *octetsExacts) Read(p []byte) (int, error) {
	if o.restant <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > o.restant {
		p = p[:o.restant]
	}
	n, err := o.corps.Read(p)
	o.restant -= int64(n)
	if errors.Is(err, io.EOF) && o.restant > 0 {
		err = io.ErrUnexpectedEOF
	}
	if o.restant <= 0 && err == nil {
		err = io.EOF // exactement la longueur annoncee : fin
	}
	return n, err
}

func (o *octetsExacts) Close() error { return o.corps.Close() }

// ecrire envoie un bloc au client avec un delai d'ECRITURE de vod.pause_max.
//
// POURQUOI : un lecteur en pause cesse de lire sa connexion ; l'ecriture finit par se
// bloquer (tampons du systeme pleins). Sans delai, la lecture — donc la connexion unique
// du fournisseur — resterait prise tant que l'onglet est ouvert. Le delai ne court que
// pendant qu'une ecriture est bloquee : un client lent mais actif n'est jamais coupe.
// Les erreurs distinguent la pause (ErrPause) du client parti (ctx annule : les autres
// erreurs d'ecriture).
func (s *Service) ecrire(ctx context.Context, rc *http.ResponseController, w http.ResponseWriter, p []byte) error {
	debut := time.Now()
	if s.r.PauseMax > 0 {
		_ = rc.SetWriteDeadline(debut.Add(s.r.PauseMax))
		// Servir fait echouer une ecriture en cours des l'annulation du contexte en
		// avancant le delai a « maintenant » ; il ne faut pas l'ecraser apres coup.
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	_, err := w.Write(p)
	if err == nil {
		err = rc.Flush()
		if errors.Is(err, http.ErrNotSupported) {
			err = nil
		}
	}
	// Pause = notre delai est ecoule pour de bon. Servir avance aussi le delai a
	// « maintenant » quand la lecture est annulee (client parti, zapping, seek) : ce cas
	// echoue tout de suite, et le contexte annule ne permet pas de les distinguer (net/http
	// l'annule des qu'une ecriture echoue).
	if err != nil && errors.Is(err, os.ErrDeadlineExceeded) && s.r.PauseMax > 0 && time.Since(debut) >= s.r.PauseMax*9/10 {
		return ErrPause
	}
	return err
}

// typeContenu : un fichier servi tel quel garde son vrai type (un mkv n'est pas du mp4 :
// un televiseur s'en sert pour choisir son decodeur) ; le relais habituel ne sert que du
// mp4 lisible, annonce comme tel.
func typeContenu(extension string, brut bool) string {
	if !brut {
		return "video/mp4"
	}
	switch strings.ToLower(extension) {
	case "mkv":
		return "video/x-matroska"
	case "mp4", "m4v":
		return "video/mp4"
	case "mov":
		return "video/quicktime"
	case "webm":
		return "video/webm"
	case "ts":
		return "video/mp2t"
	}
	return "application/octet-stream"
}
