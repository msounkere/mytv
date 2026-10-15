package remux

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"mytv/internal/domain"
)

// ReglagesSonde (config.Remux).
type ReglagesSonde struct {
	Ffprobe string
	Duree   time.Duration // remux.duree_sonde : validite d'une analyse
	Delai   time.Duration // remux.delai_sonde : borne d'un ffprobe
	Max     int           // entrees conservees
}

// sortieSondeMax borne la sortie JSON de ffprobe.
const sortieSondeMax = 1 << 20

// Sondeur implemente domain.Sondeur avec ffprobe et un cache en memoire.
//
// POURQUOI un cache : sonder coute une connexion du fournisseur (qui n'en a
// qu'une) et 1 a 5 s. La cle est l'empreinte de l'URL : l'URL porte les
// identifiants du fournisseur et ne doit ni etre gardee ni apparaitre ailleurs.
type Sondeur struct {
	r       ReglagesSonde
	exe     Executeur
	horloge func() time.Time

	mu    sync.Mutex
	cache map[string]entreeSonde
	vol   map[string]*sondeEnVol
}

type entreeSonde struct {
	s     domain.Sonde
	jusqu time.Time
	pose  time.Time
}

// sondeEnVol regroupe les demandes simultanees de la meme URL : une seule
// connexion au fournisseur.
type sondeEnVol struct {
	fini chan struct{}
	s    *domain.Sonde
	err  error
}

var _ domain.Sondeur = (*Sondeur)(nil)

func NouveauSondeur(r ReglagesSonde, exe Executeur, horloge func() time.Time) *Sondeur {
	if exe == nil {
		exe = ExecOS{}
	}
	if horloge == nil {
		horloge = time.Now
	}
	if r.Max < 1 {
		r.Max = 1
	}
	return &Sondeur{r: r, exe: exe, horloge: horloge, cache: map[string]entreeSonde{}, vol: map[string]*sondeEnVol{}}
}

func empreinte(u string) string {
	h := sha256.Sum256([]byte(u))
	return hex.EncodeToString(h[:])
}

func (s *Sondeur) Sonder(ctx context.Context, url string, entetes http.Header) (*domain.Sonde, error) {
	args, err := argumentsSonde(url, entetes)
	if err != nil {
		return nil, err
	}
	cle := empreinte(url)
	for {
		s.mu.Lock()
		if e, ok := s.cache[cle]; ok && s.horloge().Before(e.jusqu) {
			r := e.s
			s.mu.Unlock()
			return &r, nil
		}
		if v, ok := s.vol[cle]; ok {
			s.mu.Unlock()
			select {
			case <-v.fini:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			if v.err == nil {
				r := *v.s
				return &r, nil
			}
			// L'echec du premier peut venir de SON annulation : on reessaie si la notre vit.
			if ctx.Err() == nil && (v.err == context.Canceled || v.err == context.DeadlineExceeded) {
				continue
			}
			return nil, v.err
		}
		v := &sondeEnVol{fini: make(chan struct{})}
		s.vol[cle] = v
		s.mu.Unlock()

		res, err := s.lancer(ctx, args)

		s.mu.Lock()
		delete(s.vol, cle)
		if err == nil {
			s.garder(cle, *res)
		}
		v.s, v.err = res, err
		s.mu.Unlock()
		close(v.fini)
		if err != nil {
			return nil, err
		}
		r := *res
		return &r, nil
	}
}

// garder (verrou pris) ajoute au cache en restant borne : on retire d'abord les
// entrees perimees, puis la plus ancienne.
func (s *Sondeur) garder(cle string, r domain.Sonde) {
	now := s.horloge()
	if len(s.cache) >= s.r.Max {
		for k, e := range s.cache {
			if !now.Before(e.jusqu) {
				delete(s.cache, k)
			}
		}
	}
	for len(s.cache) >= s.r.Max {
		ancienne, plusVieux := "", now.Add(time.Hour)
		for k, e := range s.cache {
			if e.pose.Before(plusVieux) {
				ancienne, plusVieux = k, e.pose
			}
		}
		delete(s.cache, ancienne)
	}
	s.cache[cle] = entreeSonde{s: r, jusqu: now.Add(s.r.Duree), pose: now}
}

func argumentsSonde(url string, entetes http.Header) ([]string, error) {
	if err := validerURL(url); err != nil {
		return nil, err
	}
	agent, err := agentDepuis(entetes)
	if err != nil {
		return nil, err
	}
	a := []string{"-v", "error", "-protocol_whitelist", protocoles}
	if agent != "" {
		a = append(a, "-user_agent", agent)
	}
	return append(a, "-analyzeduration", "4M", "-probesize", "6M",
		"-print_format", "json", "-show_streams", "-show_format", url), nil
}

func (s *Sondeur) lancer(ctx context.Context, args []string) (*domain.Sonde, error) {
	if s.r.Delai > 0 {
		var annule context.CancelFunc
		ctx, annule = context.WithTimeout(ctx, s.r.Delai)
		defer annule()
	}
	sortie, err := s.exe.Executer(ctx, s.r.Ffprobe, args, sortieSondeMax)
	if err != nil {
		return nil, fmt.Errorf("remux : analyse impossible : %w", err)
	}
	return AnalyserSonde(sortie)
}

// AnalyserSonde lit la sortie JSON de ffprobe.
func AnalyserSonde(sortie []byte) (*domain.Sonde, error) {
	var d struct {
		Streams []struct {
			Type  string `json:"codec_type"`
			Codec string `json:"codec_name"`
		} `json:"streams"`
		Format struct {
			Nom   string `json:"format_name"`
			Duree string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(sortie, &d); err != nil {
		return nil, fmt.Errorf("remux : reponse de ffprobe illisible")
	}
	r := &domain.Sonde{Conteneur: strings.ToLower(d.Format.Nom)}
	for _, st := range d.Streams {
		switch st.Type {
		case "video":
			// Les pochettes sont exposees comme un flux video image : les retenir
			// ferait croire a un format exotique et refuserait un film lisible.
			switch st.Codec {
			case "mjpeg", "png", "bmp":
				continue
			}
			if r.CodecVideo == "" {
				r.CodecVideo = st.Codec
			}
		case "audio":
			if r.CodecAudio == "" {
				r.CodecAudio = st.Codec
			}
		}
	}
	if sec, err := strconv.ParseFloat(d.Format.Duree, 64); err == nil && sec > 0 {
		r.Duree = time.Duration(sec * float64(time.Second))
	}
	if r.CodecVideo == "" && r.CodecAudio == "" {
		return nil, fmt.Errorf("remux : aucun flux reconnu")
	}
	return r, nil
}
