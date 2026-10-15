package live

import (
	"sort"
	"sync"
	"time"
)

// stats chronometre le relais : l'ancien proxy ne disait pas qui ralentissait.
type stats struct {
	mu        sync.Mutex
	fenetre   int       // mesures gardees (config live.fenetre_mesures)
	playlists []float64 // ms, une mesure par requete amont
	segments  []float64 // ms, une mesure par segment relaye en entier
	echecs    int       // requetes amont de playlist en echec
	essais    int       // nouveaux essais declenches
	secours   int       // playlists de secours servies
	servies   int       // playlists servies
	patiences int       // attentes apres un refus 407
}

func (s *stats) noterPlaylist(d time.Duration) { s.noter(&s.playlists, d) }
func (s *stats) noterSegment(d time.Duration)  { s.noter(&s.segments, d) }

func (s *stats) noter(cible *[]float64, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	*cible = append(*cible, float64(d.Microseconds())/1000)
	if len(*cible) > s.fenetre {
		*cible = (*cible)[len(*cible)-s.fenetre:]
	}
}

func (s *stats) incrementer(champ *int) {
	s.mu.Lock()
	*champ++
	s.mu.Unlock()
}

// Resume est la forme exposee par /etat.
type Resume struct {
	PlaylistsServies    int                `json:"playlists_servies"`
	EchecsAmont         int                `json:"echecs_amont"`
	NouveauxEssais      int                `json:"nouveaux_essais"`
	PlaylistsDeSecours  int                `json:"playlists_de_secours"`
	Patiences407        int                `json:"patiences_407"`
	PlaylistMs          map[string]float64 `json:"playlist_ms"`
	SegmentMs           map[string]float64 `json:"segment_ms"`
	SegmentsEnCache     int                `json:"segments_en_cache"`
	OctetsSegmentsCache int                `json:"octets_segments_en_cache"`
}

func (s *stats) resume() Resume {
	s.mu.Lock()
	defer s.mu.Unlock()
	return Resume{
		PlaylistsServies:   s.servies,
		EchecsAmont:        s.echecs,
		NouveauxEssais:     s.essais,
		PlaylistsDeSecours: s.secours,
		Patiences407:       s.patiences,
		PlaylistMs:         distribution(s.playlists),
		SegmentMs:          distribution(s.segments),
	}
}

func distribution(v []float64) map[string]float64 {
	return map[string]float64{"mediane": centile(v, .5), "p95": centile(v, .95), "max": centile(v, 1)}
}

// centile : rang le plus proche, sur une copie triee.
func centile(v []float64, p float64) float64 {
	if len(v) == 0 {
		return 0
	}
	c := append([]float64(nil), v...)
	sort.Float64s(c)
	i := int(float64(len(c))*p) - 1
	if i < 0 {
		i = 0
	}
	return c[i]
}
