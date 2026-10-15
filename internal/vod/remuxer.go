package vod

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"mytv/internal/domain"
)

// remuxer sert un mp4 fragmente produit par ffmpeg.
//
// Un flux fabrique a la volee n'a ni longueur ni octet N : on ne repond donc
// jamais 206 (Accept-Ranges: none) et on ignore Range, ce que la RFC autorise.
// Le deplacement se fait par `?t=<secondes>` : une nouvelle requete, qui remplace
// la precedente (voir Service.remplacer).
//
// Les en-tetes ne partent qu'avec le premier bloc : si ffmpeg meurt avant
// d'avoir produit quoi que ce soit, l'appelant peut encore repondre 502.
func (s *Service) remuxer(ctx context.Context, w http.ResponseWriter, req *http.Request, url string, mode domain.Mode, debutLecture time.Duration, debut time.Time) error {
	// Aucun en-tete n'est pose avant d'avoir un premier octet : tant que rien n'est
	// parti, l'appelant doit pouvoir repondre une erreur propre.
	entetes := func() {
		h := w.Header()
		h.Set("Content-Type", "video/mp4")
		h.Set("Accept-Ranges", "none")
		h.Set("Cache-Control", "private, no-store")
	}
	if req.Method == http.MethodHead {
		entetes()
		w.WriteHeader(http.StatusOK)
		return nil
	}

	p, err := s.dep.Lanceur.Lancer(ctx, domain.TacheRemux{
		URLAmont: url, Entetes: s.entetesAmont(), Mode: mode, Debut: debutLecture,
	})
	if err != nil {
		return err
	}
	// Quoi qu'il arrive — fin normale, client parti, erreur — le processus est tue
	// et recolte avant de rendre la main : c'est lui qui tient la connexion amont.
	defer p.Arreter()
	stopCtx := context.AfterFunc(ctx, p.Arreter)
	defer stopCtx()
	sortie := p.Sortie()
	defer sortie.Close()

	var minuteur *time.Timer
	if s.r.PremierOctet > 0 {
		minuteur = time.AfterFunc(s.r.PremierOctet, p.Arreter)
		defer minuteur.Stop()
	}
	buf := make([]byte, s.r.Tampon)
	n, rerr := lirePremier(sortie, buf)
	if minuteur != nil {
		minuteur.Stop()
	}
	if n == 0 {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := p.Attendre(); err != nil {
			rerr = err
		}
		return fmt.Errorf("%w : ffmpeg n'a rien produit (%v)", ErrReponseAmont, rerr)
	}

	entetes()
	w.WriteHeader(http.StatusOK)
	s.inst.Observer("vod_premier_octet_secondes", time.Since(debut).Seconds(), "mode", string(mode))
	rc := http.NewResponseController(w)
	for {
		if _, werr := w.Write(buf[:n]); werr != nil {
			return werr
		}
		s.inst.Compter("vod_octets_total", float64(n), "mode", string(mode))
		_ = rc.Flush()
		n, rerr = sortie.Read(buf)
		if n == 0 && rerr != nil {
			break
		}
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !errors.Is(rerr, io.EOF) {
		return fmt.Errorf("%w : lecture de ffmpeg", ErrCoupure)
	}
	if err := p.Attendre(); err != nil {
		return fmt.Errorf("%w : ffmpeg s'est termine en erreur (%v)", ErrCoupure, err)
	}
	return nil
}

// lirePremier lit jusqu'a obtenir au moins un octet (ou une erreur).
func lirePremier(r io.Reader, buf []byte) (int, error) {
	for {
		n, err := r.Read(buf)
		if n > 0 || err != nil {
			return n, err
		}
	}
}
