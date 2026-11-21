package vod

// Replay (catch-up) du fournisseur, lu comme un film (lot 2 du chantier enregistrement).
//
// Identifiant : « <fournisseur>:replay-<numero de chaine>-<debut, secondes Unix UTC>-<minutes> »,
// par exemple « iptva:replay-553959-1795260000-55 ». Il passe par GET /vod/{id} comme un film :
// meme bail, memes sessions, meme remux, meme ?t= pour se deplacer.
//
// Ce que le fournisseur impose (essais reels du 2026-11-21) :
//   - format .ts seulement (le .m3u8 de timeshift ne renvoie rien d'exploitable) ;
//   - heure de debut a la minute, dans le fuseau de SON serveur (server_info.timezone) ;
//   - aucun saut a l'interieur du flux : se deplacer, c'est redemander a partir d'une autre minute
//     (le reste, moins d'une minute, est saute par ffmpeg) ;
//   - une chaine annoncee avec replay (tv_archive) peut ne rien servir : 200, text/html, 0 octet.
//     C'est domain.ErrReplayIndisponible, pas une panne.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"mytv/internal/domain"
)

const (
	// replayMinutesMax : un programme plus long n'est pas un programme (8 h).
	replayMinutesMax = 480
	// replayMarge : minutes demandees en plus de la fin annoncee par le guide (programme en
	// retard, fin de match). Dispatcharr fait de meme.
	replayMarge = 5
)

// Chaines : ce que le replay lit du catalogue (le stockage SQLite l'implemente).
type Chaines interface {
	Chaine(ctx context.Context, id domain.ContenuID) (*domain.Chaine, error)
}

// replay : une demande de replay validee.
type replay struct {
	numero  string
	debut   time.Time
	minutes int
	fr      domain.FournisseurReplay
}

// IDReplay construit l'identifiant d'un replay (voir domain.IDReplay).
func IDReplay(fournisseur, numero string, debut time.Time, minutes int) domain.ContenuID {
	return domain.IDReplay(fournisseur, numero, debut, minutes)
}

// resoudreReplay valide un identifiant de replay contre le catalogue et l'horloge. Tout ce qui
// n'est pas exactement un replay possible est ErrNonTrouve : rien de la requete n'atteint le
// fournisseur sans avoir ete verifie.
func (s *Service) resoudreReplay(ctx context.Context, id domain.ContenuID, fid, reste string, src source) (*media, error) {
	parts := strings.Split(strings.TrimPrefix(reste, domain.PrefixeReplay), "-")
	if len(parts) != 3 || !reNumero.MatchString(parts[0]) || !reNumero.MatchString(parts[1]) || !reNumero.MatchString(parts[2]) {
		return nil, domain.ErrNonTrouve
	}
	fr, ok := src.f.(domain.FournisseurReplay)
	if !ok || s.dep.Chaines == nil {
		return nil, domain.ErrNonTrouve
	}
	sec, err1 := strconv.ParseInt(parts[1], 10, 64)
	minutes, err2 := strconv.Atoi(parts[2])
	if err1 != nil || err2 != nil || minutes < 1 || minutes > replayMinutesMax {
		return nil, domain.ErrNonTrouve
	}
	debut := time.Unix(sec, 0).UTC()
	c, err := s.dep.Chaines.Chaine(ctx, domain.ContenuID(fid+":"+parts[0]))
	if err != nil {
		return nil, err
	}
	maintenant := s.maintenant()
	switch {
	case c.ReplayJours <= 0:
		return nil, fmt.Errorf("%w : la chaine n'annonce pas de replay", domain.ErrReplayIndisponible)
	case !domain.ReplayPossible(c.ReplayJours, debut, maintenant):
		return nil, fmt.Errorf("%w : programme pas encore diffuse ou plus ancien que le replay de la chaine (%d jours)", domain.ErrReplayIndisponible, c.ReplayJours)
	}
	return &media{id: id, fid: fid, numero: parts[0], src: src,
		replay: &replay{numero: parts[0], debut: debut, minutes: minutes, fr: fr}}, nil
}

func (s *Service) maintenant() time.Time {
	if s.dep.Horloge != nil {
		return s.dep.Horloge()
	}
	return time.Now()
}

// adresse construit l'adresse du fournisseur pour une lecture a la position t, et rend le
// reste a sauter (moins d'une minute) et la duree totale du programme.
func (r *replay) adresse(ctx context.Context, t time.Duration) (url string, reste time.Duration, err error) {
	total := time.Duration(r.minutes) * time.Minute
	if t >= total {
		return "", 0, fmt.Errorf("%w : position au-dela de la fin du programme", ErrDemandeInvalide)
	}
	fuseau, err := r.fr.FuseauServeur(ctx)
	if err != nil {
		return "", 0, err
	}
	saut := t.Truncate(time.Minute)
	minutes := r.minutes - int(saut/time.Minute) + replayMarge
	return r.fr.URLReplay(r.numero, r.debut.Add(saut), minutes, fuseau, "ts"), t - saut, nil
}

// sourceReplay lit le flux de replay du fournisseur, sequentiellement : pas de Range, pas de
// taille (le serveur local le sert alors comme un flux simple a ffmpeg). Une reprise au milieu
// (depuis > 0) n'a pas de sens pour ce flux : refusee.
func (s *Service) sourceReplay(m *media, url string) domain.SourceOctets {
	return func(ctx context.Context, depuis int64) (io.ReadCloser, error) {
		if depuis > 0 {
			return nil, fmt.Errorf("%w : un replay ne reprend pas au milieu du flux", ErrReponseAmont)
		}
		rep, err := s.requeteAmont(ctx, m.src.f, url, nil, time.Now())
		if err != nil {
			return nil, err
		}
		if rep.StatusCode != http.StatusOK {
			rep.Body.Close()
			return nil, fmt.Errorf("%w : replay refuse (HTTP %d)", ErrReponseAmont, rep.StatusCode)
		}
		// Reponse vide du fournisseur (« X-Error-Cache », text/html, 0 octet) : rien en archive.
		if strings.HasPrefix(strings.ToLower(rep.Header.Get("Content-Type")), "text/html") || rep.ContentLength == 0 {
			rep.Body.Close()
			return nil, fmt.Errorf("%w : le fournisseur ne sert rien pour ce programme", domain.ErrReplayIndisponible)
		}
		return rep.Body, nil
	}
}

// estReplay : identifiant de replay (« <fournisseur>:replay-... »).
func estReplay(reste string) bool { return strings.HasPrefix(reste, domain.PrefixeReplay) }

// sondeReplay ouvre le flux du replay, lit un octet et le referme.
func sondeReplay(ctx context.Context, src domain.SourceOctets) error {
	rc, err := src(ctx, 0)
	if err != nil {
		return err
	}
	defer rc.Close()
	if _, err := io.ReadFull(rc, make([]byte, 1)); err != nil {
		return fmt.Errorf("%w : le fournisseur ne sert rien pour ce programme", domain.ErrReplayIndisponible)
	}
	return nil
}
