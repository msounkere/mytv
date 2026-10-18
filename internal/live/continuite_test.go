package live

import (
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mytv/internal/config"
)

// playlistNommee : playlist amont a partir d'un numero de sequence, de noms de segments
// et d'un hash de chemin (le fournisseur en change a chaque playlist).
func playlistNommee(seq int, hash string, noms ...string) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:10\n#EXT-X-MEDIA-SEQUENCE:" + itoa(seq) + "\n")
	for _, n := range noms {
		b.WriteString("#EXTINF:10.0,\n/hls/" + hash + "/" + n + "\n")
	}
	return b.String()
}

func ligneApres(t *testing.T, corps, prefixe string) string {
	t.Helper()
	for _, l := range strings.Split(corps, "\n") {
		if strings.HasPrefix(l, prefixe) {
			return l
		}
	}
	t.Fatalf("ligne %q absente de :\n%s", prefixe, corps)
	return ""
}

// Regression (recette reelle du 18/10, C NEWS FHD) : le fournisseur redemarre son flux a la
// sequence 0 avec de nouveaux segments. Le relais doit presenter une numerotation qui
// CONTINUE (RFC 8216 : jamais decroissante) et une discontinuite devant le premier segment
// du nouveau flux ; recopier le 0 faisait boucler hls.js (segments places a 0, seek a 0).
func TestRedemarrageAmontDonneNumerotationContinueEtDiscontinuite(t *testing.T) {
	r := &Relais{}
	t0 := time.Date(2026, 10, 18, 15, 0, 0, 0, time.UTC)
	ancienne := playlistNommee(1500, "aaaa", "c_1500.ts", "c_1501.ts", "c_1502.ts")
	if o, _ := r.observer("c", ancienne, t0); o.Recul {
		t.Fatal("premiere playlist : pas de recul")
	}
	if s := appliquerContinuite(ancienne, r.sequences["c"]); s != ancienne {
		t.Fatalf("sans redemarrage, la playlist est servie telle quelle :\n%s", s)
	}

	nouvelle := playlistNommee(0, "bbbb", "c_0.ts", "c_1.ts")
	o, _ := r.observer("c", nouvelle, t0.Add(10*time.Second))
	if !o.Recul || !o.Avance {
		t.Fatalf("un retour a 0 avec de nouveaux segments est un redemarrage : %+v", o)
	}
	servie := appliquerContinuite(nouvelle, r.sequences["c"])
	if l := ligneApres(t, servie, "#EXT-X-MEDIA-SEQUENCE:"); l != "#EXT-X-MEDIA-SEQUENCE:1503" {
		t.Errorf("le premier segment du nouveau flux suit le dernier servi (1502) : %q", l)
	}
	if !strings.Contains(servie, "#EXT-X-DISCONTINUITY\n#EXTINF:10.0,\n/hls/bbbb/c_0.ts") {
		t.Errorf("discontinuite attendue devant le premier segment du nouveau flux :\n%s", servie)
	}
	if strings.Count(servie, "#EXT-X-DISCONTINUITY\n") != 1 {
		t.Errorf("une seule discontinuite :\n%s", servie)
	}
	if r.epoque("c") != 1 {
		t.Errorf("epoque 1 attendue, %d", r.epoque("c"))
	}

	// Le flux immobile a 0 (25-53 s en reel) reste servi pareil : meme numero, meme tag.
	o, _ = r.observer("c", nouvelle, t0.Add(40*time.Second))
	if o.Recul || o.Avance || o.ImmobileS < 29 {
		t.Fatalf("immobile depuis 30 s attendu : %+v", o)
	}
	if s := appliquerContinuite(nouvelle, r.sequences["c"]); s != servie {
		t.Errorf("playlist identique attendue tant que l'amont ne bouge pas :\n%s", s)
	}

	// Le flux avance : le segment porteur du tag sort de la fenetre, la discontinuite se
	// compte dans DISCONTINUITY-SEQUENCE (RFC 8216, 4.3.3.3) et le tag disparait.
	suite := playlistNommee(1, "cccc", "c_1.ts", "c_2.ts")
	r.observer("c", suite, t0.Add(50*time.Second))
	servie = appliquerContinuite(suite, r.sequences["c"])
	if l := ligneApres(t, servie, "#EXT-X-MEDIA-SEQUENCE:"); l != "#EXT-X-MEDIA-SEQUENCE:1504" {
		t.Errorf("numerotation continue : %q", l)
	}
	if strings.Contains(servie, "#EXT-X-DISCONTINUITY\n") {
		t.Errorf("le tag est sorti de la fenetre :\n%s", servie)
	}
	if l := ligneApres(t, servie, "#EXT-X-DISCONTINUITY-SEQUENCE:"); l != "#EXT-X-DISCONTINUITY-SEQUENCE:1" {
		t.Errorf("une discontinuite sortie de la fenetre : %q", l)
	}

	// Deuxieme redemarrage : la numerotation continue encore, la discontinuite sortie
	// reste comptee et la nouvelle est annoncee.
	r.observer("c", playlistNommee(0, "dddd", "d_0.ts", "d_1.ts", "d_2.ts"), t0.Add(60*time.Second))
	servie = appliquerContinuite(playlistNommee(0, "dddd", "d_0.ts", "d_1.ts", "d_2.ts"), r.sequences["c"])
	if l := ligneApres(t, servie, "#EXT-X-MEDIA-SEQUENCE:"); l != "#EXT-X-MEDIA-SEQUENCE:1506" {
		t.Errorf("1504 + 2 segments : %q", l)
	}
	if l := ligneApres(t, servie, "#EXT-X-DISCONTINUITY-SEQUENCE:"); l != "#EXT-X-DISCONTINUITY-SEQUENCE:1" {
		t.Errorf("la discontinuite precedente reste comptee : %q", l)
	}
	if !strings.Contains(servie, "#EXT-X-DISCONTINUITY\n#EXTINF:10.0,\n/hls/dddd/d_0.ts") || r.epoque("c") != 2 {
		t.Errorf("nouvelle discontinuite et epoque 2 attendues :\n%s", servie)
	}
}

// Un leger recul de la sequence avec les MEMES segments n'est pas un redemarrage : c'est
// un noeud du fournisseur en retard (302 vers un autre serveur). La correspondance
// numero -> segment est la bonne, on ne doit ni renumeroter ni annoncer de discontinuite
// (hls.js recollerait deux fois les memes segments).
func TestReculAvecMemesSegmentsNEstPasUnRedemarrage(t *testing.T) {
	r := &Relais{}
	t0 := time.Now()
	r.observer("c", playlistNommee(1500, "aaaa", "c_1500.ts", "c_1501.ts", "c_1502.ts"), t0)
	retard := playlistNommee(1498, "bbbb", "c_1498.ts", "c_1499.ts", "c_1500.ts")
	o, _ := r.observer("c", retard, t0.Add(5*time.Second))
	if o.Recul {
		t.Fatalf("memes segments : pas un redemarrage : %+v", o)
	}
	if s := appliquerContinuite(retard, r.sequences["c"]); s != retard || r.epoque("c") != 0 {
		t.Errorf("playlist servie telle quelle attendue :\n%s", s)
	}
}

// Les discontinuites PROPRES au fournisseur sont conservees et comptees avec les notres.
func TestDiscontinuitesDuFournisseurConserveesEtCumulees(t *testing.T) {
	r := &Relais{}
	t0 := time.Now()
	avec := "#EXTM3U\n#EXT-X-TARGETDURATION:10\n#EXT-X-MEDIA-SEQUENCE:10\n#EXT-X-DISCONTINUITY-SEQUENCE:3\n" +
		"#EXTINF:10.0,\n/hls/a/x_10.ts\n#EXT-X-DISCONTINUITY\n#EXTINF:10.0,\n/hls/a/x_11.ts\n"
	r.observer("c", avec, t0)
	r.observer("c", playlistNommee(0, "b", "y_0.ts", "y_1.ts"), t0.Add(10*time.Second))
	servie := appliquerContinuite(playlistNommee(0, "b", "y_0.ts", "y_1.ts"), r.sequences["c"])
	// 3 deja sorties chez le fournisseur + 1 dans l'ancienne fenetre, toutes sorties au redemarrage.
	if l := ligneApres(t, servie, "#EXT-X-DISCONTINUITY-SEQUENCE:"); l != "#EXT-X-DISCONTINUITY-SEQUENCE:4" {
		t.Errorf("4 attendu : %q", l)
	}
	if l := ligneApres(t, servie, "#EXT-X-MEDIA-SEQUENCE:"); l != "#EXT-X-MEDIA-SEQUENCE:12" {
		t.Errorf("12 attendu : %q", l)
	}
}

// De bout en bout : la playlist SERVIE par le relais (reecrite, puis de secours) porte la
// numerotation continue, et un segment dont le nom est reutilise par le nouveau flux n'est
// jamais servi depuis le cache de l'ancien.
func TestRelaisServtNumerotationContinueEtCacheParEpoque(t *testing.T) {
	var etape atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) {
		switch etape.Load() {
		case 0:
			io.WriteString(w, playlistNommee(5, "aaaa", "100_5.ts", "100_6.ts"))
		case 1:
			io.WriteString(w, playlistNommee(0, "bbbb", "100_0.ts", "100_1.ts")) // redemarrage
		default:
			io.WriteString(w, playlistNommee(4, "cccc", "100_4.ts", "100_5.ts")) // le nom 100_5 revient
		}
	})
	mux.HandleFunc("/hls/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp2t")
		if etape.Load() == 0 {
			io.WriteString(w, "EPOQUE-A")
		} else {
			io.WriteString(w, "EPOQUE-B")
		}
	})
	b := nouveauBanc(t, mux, nil)
	rec := b.playlist("100", "s1")
	if !strings.Contains(rec.Body.String(), "#EXT-X-MEDIA-SEQUENCE:5\n") || strings.Contains(rec.Body.String(), "DISCONTINUITY") {
		t.Fatalf("premiere playlist servie telle quelle :\n%s", rec.Body.String())
	}
	seg5 := segments(rec.Body.String())[0]
	if rec := b.get(seg5, "s1"); rec.Code != 200 || rec.Body.String() != "EPOQUE-A" {
		t.Fatalf("segment de l'epoque A : %d %q", rec.Code, rec.Body.String())
	}
	if rec := b.get(seg5, "s1"); rec.Header().Get("X-Relais") != "cache" {
		t.Fatalf("le deuxieme appel vient du cache")
	}

	etape.Store(1)
	rec = b.playlist("100", "s1")
	corps := rec.Body.String()
	if !strings.Contains(corps, "#EXT-X-MEDIA-SEQUENCE:7\n") || !strings.Contains(corps, "#EXT-X-DISCONTINUITY\n#EXTINF") {
		t.Fatalf("numerotation continue (7) et discontinuite attendues :\n%s", corps)
	}

	etape.Store(2)
	rec = b.playlist("100", "s1")
	corps = rec.Body.String()
	if !strings.Contains(corps, "#EXT-X-MEDIA-SEQUENCE:11\n") || !strings.Contains(corps, "#EXT-X-DISCONTINUITY-SEQUENCE:1\n") {
		t.Fatalf("7 + 4 = 11 et une discontinuite sortie :\n%s", corps)
	}
	seg5b := segments(corps)[1]
	if rec := b.get(seg5b, "s1"); rec.Code != 200 || rec.Body.String() != "EPOQUE-B" {
		t.Fatalf("meme nom, autre epoque : le cache de l'epoque A ne doit pas repondre : %d %q (%s)",
			rec.Code, rec.Body.String(), rec.Header().Get("X-Relais"))
	}
}

// Un segment refuse (407) patiente au plus bail.patience_segment, plus court que la
// patience des playlists : au-dela, 503 + Retry-After, que le lecteur sait honorer, au lieu
// d'une attente qu'il abandonne de toute facon au bout de ~15 s (puis relance six fois).
func TestPatienceDUnSegmentEstBorneeParPatienceSegment(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, playlistAmont("aaaa")) })
	mux.HandleFunc("/hls/", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "place occupee", 407) })
	b := nouveauBanc(t, mux, func(o *Options) {
		o.Bail.PatienceMax = config.Duree(2 * time.Second)
		o.Bail.PatienceSegment = config.Duree(100 * time.Millisecond)
	})
	seg := chaineSeg(t, b, "100")[0]
	debut := time.Now()
	rec := b.get(seg, "s1")
	d := time.Since(debut)
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("503 + Retry-After attendus : %d %v", rec.Code, rec.Header())
	}
	if d > 600*time.Millisecond {
		t.Errorf("patience du segment bornee a 100 ms attendue, %v", d)
	}
}
