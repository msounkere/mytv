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
func (s *Service) remuxer(ctx context.Context, w http.ResponseWriter, req *http.Request, m *media, url string, mode domain.Mode, debutLecture time.Duration, debut time.Time, info infosLecture) error {
	// Aucun en-tete n'est pose avant d'avoir un premier octet : tant que rien n'est
	// parti, l'appelant doit pouvoir repondre une erreur propre.
	entetes := func() {
		h := w.Header()
		h.Set("Content-Type", "video/mp4")
		h.Set("Accept-Ranges", "none")
		h.Set("Cache-Control", "private, no-store")
		info.poser(h)
	}
	if req.Method == http.MethodHead {
		entetes()
		w.WriteHeader(http.StatusOK)
		return nil
	}

	tache := domain.TacheRemux{Source: s.sourceAmont(m, url), Mode: mode, Debut: debutLecture, HEVC: info.hevc}
	var p domain.Processus
	var err error
	if info.eco {
		// Mode economie : reencodage a la cible, meme pour un mp4 lisible (mode « direct » de la
		// decision) ; le lanceur sait le faire, Servir l'a verifie.
		if tache.Mode == domain.ModeDirect {
			tache.Mode = domain.ModeRemux
		}
		p, err = s.dep.Lanceur.(lanceurEco).LancerEco(ctx, tache, s.r.Eco)
	} else {
		p, err = s.dep.Lanceur.Lancer(ctx, tache)
	}
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
		// La vraie cause est souvent chez le fournisseur (407 epuise, 404...) : la source
		// locale que lit ffmpeg l'a notee. La rendre telle quelle donne le bon statut
		// (503 + Retry-After pour une place occupee) au lieu d'un 502 muet.
		if e, ok := p.(interface{ ErreurSource() error }); ok {
			if es := e.ErreurSource(); es != nil {
				return es
			}
		}
		return fmt.Errorf("%w : ffmpeg n'a rien produit (%v)", ErrReponseAmont, rerr)
	}

	entetes()
	w.WriteHeader(http.StatusOK)
	if info.eco {
		mode = modeEco // etiquette des mesures et du journal
	}
	s.inst.Observer("vod_premier_octet_secondes", time.Since(debut).Seconds(), "mode", string(mode))
	s.log.Info("premier octet du film", "mode", string(mode), "debut_s", int(debutLecture.Seconds()), "ms", time.Since(debut).Milliseconds())
	rc := http.NewResponseController(w)
	for {
		if werr := s.ecrire(ctx, rc, w, buf[:n]); werr != nil {
			return werr // client parti ou en pause : le defer arrete ffmpeg
		}
		s.inst.Compter("vod_octets_total", float64(n), "mode", string(mode))
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
