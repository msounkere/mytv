package live

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mytv/internal/config"
	"mytv/internal/domain"
	"mytv/internal/testutil/instrum"
)

func chaineSeg(t *testing.T, b *banc, chaine string) []string {
	t.Helper()
	rec := b.playlist(chaine, "s1")
	if rec.Code != 200 {
		t.Fatalf("playlist : %d %s", rec.Code, rec.Body.String())
	}
	return segments(rec.Body.String())
}

// ---- CA1 : playlist ----

// CA1 : nouveaux essais.
func TestPlaylistNouvelEssaiApresEchec(t *testing.T) {
	var appels atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) {
		if appels.Add(1) <= 2 {
			http.Error(w, "panne", http.StatusBadGateway)
			return
		}
		io.WriteString(w, playlistAmont("aaaa"))
	})
	b := nouveauBanc(t, mux, nil)
	rec := b.playlist("100", "s1")
	if rec.Code != 200 || rec.Header().Get("X-Relais") == "secours" {
		t.Fatalf("2 echecs puis succes : 200 attendu, %d (%s)", rec.Code, rec.Header().Get("X-Relais"))
	}
	if appels.Load() != 3 {
		t.Errorf("3 appels amont attendus, %d", appels.Load())
	}
	if e := b.relais.Etat(); e.NouveauxEssais != 2 || e.EchecsAmont != 2 {
		t.Errorf("statistiques : %+v", e)
	}
}

// CA1 : le nombre d'essais vient de la configuration, pas du code.
func TestPlaylistNombreDEssaisVientDeLaConfiguration(t *testing.T) {
	var appels atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) {
		appels.Add(1)
		http.Error(w, "panne", http.StatusBadGateway)
	})
	b := nouveauBanc(t, mux, func(o *Options) { o.Reseau.EssaisPlaylist = 5 })
	if rec := b.playlist("100", "s1"); rec.Code != http.StatusBadGateway {
		t.Fatalf("502 attendu : %d", rec.Code)
	}
	if appels.Load() != 5 {
		t.Errorf("5 essais configures, %d appels", appels.Load())
	}
}

// CA1 : un refus net (identifiants, introuvable) n'est pas retente.
func TestPlaylistRefusNetPasDeNouvelEssai(t *testing.T) {
	var appels atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) {
		appels.Add(1)
		http.Error(w, "inconnu", http.StatusNotFound)
	})
	b := nouveauBanc(t, mux, nil)
	if rec := b.playlist("100", "s1"); rec.Code != http.StatusBadGateway {
		t.Fatalf("502 attendu : %d", rec.Code)
	}
	if appels.Load() != 1 {
		t.Errorf("un 404 ne se retente pas : %d appels", appels.Load())
	}
}

// CA1 : singleflight, 8 demandes simultanees = 1 requete amont.
func TestPlaylistDemandesSimultaneesRegroupees(t *testing.T) {
	var appels atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) {
		appels.Add(1)
		time.Sleep(150 * time.Millisecond)
		io.WriteString(w, playlistAmont("aaaa"))
	})
	b := nouveauBanc(t, mux, nil)
	var wg sync.WaitGroup
	codes := make(chan int, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes <- b.playlist("100", fmt.Sprintf("s%d", i)).Code
		}(i)
	}
	wg.Wait()
	close(codes)
	for c := range codes {
		if c != 200 {
			t.Fatalf("code %d", c)
		}
	}
	if appels.Load() != 1 {
		t.Errorf("1 appel amont attendu pour 8 demandes, %d", appels.Load())
	}
	if n := b.baux.nbAcquisitions(); n != 8 {
		t.Errorf("chaque session doit avoir son bail (CA4) : %d acquisitions", n)
	}
}

// CA1 : fraicheur live.fraicheur_playlist.
func TestPlaylistFraicheurReutilisee(t *testing.T) {
	var appels atomic.Int32
	b := nouveauBanc(t, amontSimple(&appels), func(o *Options) { o.Live.FraicheurPlaylist = config.Duree(800 * time.Millisecond) })
	b.playlist("100", "s1")
	b.playlist("100", "s1")
	if appels.Load() != 1 {
		t.Fatalf("une playlist fraiche est reutilisee : %d appels", appels.Load())
	}
	b.decalage.Store(int64(time.Second))
	b.playlist("100", "s1")
	if appels.Load() != 2 {
		t.Errorf("une playlist perimee est redemandee : %d appels", appels.Load())
	}
	b.playlist("200", "s1") // une autre chaine n'est jamais servie par celle-ci
	if appels.Load() != 3 {
		t.Errorf("chaque chaine a sa playlist : %d appels", appels.Load())
	}
}

// Le dernier demandeur parti annule la requete amont devenue inutile.
func TestPlaylistAbandonDuDernierDemandeurAnnuleLAmont(t *testing.T) {
	annule := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			close(annule)
		case <-time.After(3 * time.Second):
		}
	})
	b := nouveauBanc(t, mux, nil)
	ctx, stop := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", PrefixeParDefaut+"/100.m3u8", nil).WithContext(ctx)
	req.Header.Set("X-Session", "s1")
	fini := make(chan struct{})
	go func() { b.h.ServeHTTP(httptest.NewRecorder(), req); close(fini) }()
	time.Sleep(50 * time.Millisecond)
	stop()
	select {
	case <-annule:
	case <-time.After(2 * time.Second):
		t.Fatal("la requete amont aurait du etre annulee")
	}
	<-fini
}

// ---- CA2 : secours ----

// CA2 : echec -> derniere bonne playlist, X-Relais: secours, et c'est la vraie playlist.
func TestPlaylistDeSecoursQuandLAmontTombe(t *testing.T) {
	var panne atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) {
		if panne.Load() {
			http.Error(w, "panne", http.StatusBadGateway)
			return
		}
		io.WriteString(w, playlistAmont("aaaa"))
	})
	b := nouveauBanc(t, mux, nil)
	if rec := b.playlist("100", "s1"); rec.Code != 200 {
		t.Fatalf("premier appel : %d", rec.Code)
	}
	panne.Store(true)
	rec := b.playlist("100", "s1")
	if rec.Code != 200 || rec.Header().Get("X-Relais") != "secours" {
		t.Fatalf("secours attendu : %d %q", rec.Code, rec.Header().Get("X-Relais"))
	}
	if !strings.Contains(rec.Body.String(), "#EXT-X-MEDIA-SEQUENCE:5") {
		t.Error("le secours doit etre la vraie playlist, pas un clip de repli")
	}
	if b.relais.Etat().PlaylistsDeSecours != 1 {
		t.Error("le secours doit etre compte")
	}
}

// CA2 : sans secours possible, ou secours perime, 502 franc.
func TestPlaylistSansSecoursOuPerimee502(t *testing.T) {
	var panne atomic.Bool
	panne.Store(true)
	mux := http.NewServeMux()
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) {
		if panne.Load() {
			http.Error(w, "panne", http.StatusBadGateway)
			return
		}
		io.WriteString(w, playlistAmont("aaaa"))
	})
	b := nouveauBanc(t, mux, nil)
	if rec := b.playlist("100", "s1"); rec.Code != http.StatusBadGateway {
		t.Fatalf("jamais de bonne playlist : 502 attendu, %d", rec.Code)
	}
	panne.Store(false)
	if rec := b.playlist("100", "s1"); rec.Code != 200 {
		t.Fatalf("%d", rec.Code)
	}
	panne.Store(true)
	b.decalage.Store(int64(2 * time.Minute)) // au-dela de peremption_secours (1 min en test)
	if rec := b.playlist("100", "s1"); rec.Code != http.StatusBadGateway || rec.Header().Get("X-Relais") != "" {
		t.Fatalf("secours perime : 502 franc attendu, %d %q", rec.Code, rec.Header().Get("X-Relais"))
	}
}

// ---- CA3 : segments ----

// segment lit un segment par le chemin que le relais a emis.
func (b *banc) segment(chemin, session string, entetes ...string) *httptest.ResponseRecorder {
	return b.get(chemin, session, entetes...)
}

// CA3 : copie en flux puis relecture depuis le cache, meme si le fournisseur disparait.
func TestSegmentCopieEtMisEnCache(t *testing.T) {
	b := nouveauBanc(t, amontSimple(nil), nil)
	seg := chaineSeg(t, b, "100")[0]
	rec := b.segment(seg, "s1")
	if rec.Code != 200 || rec.Body.String() != "DONNEES-TS!!" {
		t.Fatalf("segment : %d %q", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("X-Relais") != "" {
		t.Error("la premiere lecture vient du fournisseur")
	}
	b.amont.Close()
	rec = b.segment(seg, "s1")
	if rec.Code != 200 || rec.Header().Get("X-Relais") != "cache" || rec.Body.String() != "DONNEES-TS!!" {
		t.Fatalf("cache attendu : %d %q", rec.Code, rec.Header().Get("X-Relais"))
	}
}

// CA3 (+ US-06 CA3 de bout en bout) : le hash du fournisseur change entre deux
// playlists ; l'URL relayee reste la meme, et le cache la reconnait.
func TestSegmentCacheStableMalgreLeChangementDeHash(t *testing.T) {
	var n atomic.Int32
	var vus atomic.Int32
	var dernierChemin atomic.Value
	mux := http.NewServeMux()
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, playlistAmont(fmt.Sprintf("hash%d", n.Add(1))))
	})
	mux.HandleFunc("/hls/", func(w http.ResponseWriter, r *http.Request) {
		vus.Add(1)
		dernierChemin.Store(r.URL.Path)
		w.Header().Set("Content-Length", "3")
		io.WriteString(w, "abc")
	})
	b := nouveauBanc(t, mux, nil)
	un := chaineSeg(t, b, "100")
	b.segment(un[0], "s1")
	deux := chaineSeg(t, b, "100")
	if strings.Join(un, "|") != strings.Join(deux, "|") {
		t.Fatalf("les URL de segments doivent etre identiques :\n%v\n%v", un, deux)
	}
	rec := b.segment(deux[0], "s1")
	if rec.Header().Get("X-Relais") != "cache" || vus.Load() != 1 {
		t.Errorf("le segment deja relaye doit venir du cache : %q, %d lectures amont", rec.Header().Get("X-Relais"), vus.Load())
	}
	// Le second segment, jamais lu, est lu chez le fournisseur.
	b.segment(deux[1], "s1")
	if vus.Load() != 2 || dernierChemin.Load() != "/hls/hash2/100_6.ts" {
		t.Errorf("%d lectures amont, dernier chemin %v (adresse la plus recente attendue)", vus.Load(), dernierChemin.Load())
	}
}

// CA3 : cache borne en nombre.
func TestCacheBorneEnNombre(t *testing.T) {
	b := nouveauBanc(t, amontNSegments(), func(o *Options) { o.Live.CacheSegments.Nombre = 2 })
	for _, s := range chaineSeg(t, b, "100") {
		if rec := b.segment(s, "s1"); rec.Code != 200 {
			t.Fatalf("%d", rec.Code)
		}
	}
	if n, _ := b.relais.segments.taille(); n != 2 {
		t.Errorf("cache borne a 2 segments, %d", n)
	}
}

func amontNSegments() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "#EXTM3U\n#EXTINF:1,\ns1.ts\n#EXTINF:1,\ns2.ts\n#EXTINF:1,\ns3.ts\n")
	})
	mux.HandleFunc("/live/u/p/s1.ts", func(w http.ResponseWriter, r *http.Request) { corpsDe(w, 400) })
	mux.HandleFunc("/live/u/p/s2.ts", func(w http.ResponseWriter, r *http.Request) { corpsDe(w, 400) })
	mux.HandleFunc("/live/u/p/s3.ts", func(w http.ResponseWriter, r *http.Request) { corpsDe(w, 400) })
	return mux
}

func corpsDe(w http.ResponseWriter, n int) {
	w.Header().Set("Content-Length", fmt.Sprint(n))
	w.Write([]byte(strings.Repeat("x", n)))
}

// CA3 : cache borne en octets.
func TestCacheBorneEnOctets(t *testing.T) {
	b := nouveauBanc(t, amontNSegments(), func(o *Options) { o.Live.CacheSegments.Octets = 1000 })
	for _, s := range chaineSeg(t, b, "100") {
		b.segment(s, "s1")
	}
	n, octets := b.relais.segments.taille()
	if octets > 1000 || n != 2 {
		t.Errorf("cache borne a 1000 octets (2 segments de 400) : %d segments, %d octets", n, octets)
	}
}

// CA3 : un segment trop gros pour le cache est relaye mais pas garde.
func TestSegmentTropGrosRelayeSansEtreGarde(t *testing.T) {
	b := nouveauBanc(t, amontNSegments(), func(o *Options) { o.Live.CacheSegments.Octets = 600 }) // moitie = 300 < 400
	seg := chaineSeg(t, b, "100")[0]
	rec := b.segment(seg, "s1")
	if rec.Code != 200 || rec.Body.Len() != 400 {
		t.Fatalf("%d, %d octets", rec.Code, rec.Body.Len())
	}
	if n, _ := b.relais.segments.taille(); n != 0 {
		t.Errorf("un gros segment ne doit pas etre garde : %d", n)
	}
}

// CA3 : Range est transmis au fournisseur, la reponse partielle est relayee telle
// quelle et jamais mise en cache.
func TestSegmentRangeTransmisEtNonCache(t *testing.T) {
	var recu atomic.Value
	mux := http.NewServeMux()
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "#EXTM3U\n#EXTINF:1,\ns1.ts\n")
	})
	mux.HandleFunc("/live/u/p/s1.ts", func(w http.ResponseWriter, r *http.Request) {
		if v := r.Header.Get("Range"); v != "" {
			recu.Store(v)
			w.Header().Set("Content-Range", "bytes 0-3/10")
			w.Header().Set("Content-Length", "4")
			w.WriteHeader(http.StatusPartialContent)
			io.WriteString(w, "0123")
			return
		}
		w.Header().Set("Content-Length", "10")
		io.WriteString(w, "0123456789")
	})
	b := nouveauBanc(t, mux, nil)
	seg := chaineSeg(t, b, "100")[0]
	rec := b.segment(seg, "s1", "Range", "bytes=0-3")
	if rec.Code != http.StatusPartialContent || rec.Body.String() != "0123" || rec.Header().Get("Content-Range") != "bytes 0-3/10" {
		t.Fatalf("reponse partielle : %d %q %q", rec.Code, rec.Body.String(), rec.Header().Get("Content-Range"))
	}
	if recu.Load() != "bytes=0-3" {
		t.Errorf("Range non transmis : %v", recu.Load())
	}
	if n, _ := b.relais.segments.taille(); n != 0 {
		t.Error("une reponse partielle ne se met pas en cache")
	}
	// Un segment complet en cache ne doit pas repondre a une demande d'intervalle.
	b.segment(seg, "s1")
	rec = b.segment(seg, "s1", "Range", "bytes=0-3")
	if rec.Code != http.StatusPartialContent {
		t.Errorf("l'intervalle doit etre servi par le fournisseur : %d", rec.Code)
	}
}

// CA3 : 2 essais pour un segment.
func TestSegmentDeuxEssais(t *testing.T) {
	var appels atomic.Int32
	var echouer atomic.Int32
	echouer.Store(1)
	mux := http.NewServeMux()
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "#EXTM3U\n#EXTINF:1,\ns1.ts\n") })
	mux.HandleFunc("/live/u/p/s1.ts", func(w http.ResponseWriter, r *http.Request) {
		appels.Add(1)
		if echouer.Add(-1) >= 0 {
			http.Error(w, "panne", http.StatusBadGateway)
			return
		}
		corpsDe(w, 5)
	})
	b := nouveauBanc(t, mux, nil)
	seg := chaineSeg(t, b, "100")[0]
	if rec := b.segment(seg, "s1"); rec.Code != 200 || appels.Load() != 2 {
		t.Fatalf("un echec puis un succes : %d, %d appels", rec.Code, appels.Load())
	}
	appels.Store(0)
	echouer.Store(10)
	b2 := nouveauBanc(t, mux, nil)
	seg2 := chaineSeg(t, b2, "100")[0]
	appels.Store(0)
	if rec := b2.segment(seg2, "s1"); rec.Code != http.StatusBadGateway || appels.Load() != 2 {
		t.Fatalf("deux echecs : 502 apres 2 essais, %d apres %d appels", rec.Code, appels.Load())
	}
}

// Segment inconnu ou jeton falsifie : refus.
func TestSegmentJetonFalsifieOuInconnu(t *testing.T) {
	b := nouveauBanc(t, amontSimple(nil), nil)
	seg := chaineSeg(t, b, "100")[0]
	if rec := b.segment(seg[:len(seg)-8]+"AAAA.ts", "s1"); rec.Code != http.StatusForbidden {
		t.Errorf("signature modifiee : 403 attendu, %d", rec.Code)
	}
	if rec := b.segment(PrefixeParDefaut+"/100/seg/aHR0cDovL3BpcmF0ZS5leGVtcGxlL3g", "s1"); rec.Code != http.StatusForbidden {
		t.Errorf("URL brute : 403 attendu, %d", rec.Code)
	}
}

// US-06 CA4 de bout en bout : une playlist maitresse est reecrite, la sous-playlist
// lue par le relais est reecrite a son tour, et ses segments sont relayes.
func TestPlaylistMaitresseDeBoutEnBout(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nhaute/index.m3u8\n")
	})
	mux.HandleFunc("/live/u/p/haute/index.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		io.WriteString(w, "#EXTM3U\n#EXTINF:1,\nseg1.ts\n")
	})
	mux.HandleFunc("/live/u/p/haute/seg1.ts", func(w http.ResponseWriter, r *http.Request) { corpsDe(w, 7) })
	b := nouveauBanc(t, mux, nil)
	sous := chaineSeg(t, b, "100")[0]
	rec := b.get(sous, "s1")
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "seg1.ts\n") && !strings.Contains(rec.Body.String(), PrefixeParDefaut) {
		t.Fatalf("sous-playlist : %d %q", rec.Code, rec.Body.String())
	}
	seg := segments(rec.Body.String())
	if len(seg) != 1 || !strings.HasPrefix(seg[0], PrefixeParDefaut+"/100/seg/") {
		t.Fatalf("segment reecrit attendu : %v", seg)
	}
	if rec := b.get(seg[0], "s1"); rec.Code != 200 || rec.Body.Len() != 7 {
		t.Errorf("segment de la sous-playlist : %d, %d octets", rec.Code, rec.Body.Len())
	}
}

// ---- CA4 : baux ----

// CA4 : chaque requete amont est precedee d'un bail renouvele.
func TestRequetesAmontPassentParUnBailRenouvele(t *testing.T) {
	b := nouveauBanc(t, amontSimple(nil), nil)
	seg := chaineSeg(t, b, "100")[0]
	b.segment(seg, "s1")
	if len(b.baux.acquisitions) != 2 || b.baux.acquisitions[0] != "s1/100" {
		t.Errorf("acquisitions : %v", b.baux.acquisitions)
	}
	if n := b.baux.bail("s1").renouvele.Load(); n < 2 {
		t.Errorf("le bail doit etre renouvele a chaque requete : %d", n)
	}
}

// CA4 : sans place, rien ne part chez le fournisseur.
func TestSansPlaceAucuneRequeteAmontEt503(t *testing.T) {
	var appels atomic.Int32
	b := nouveauBanc(t, amontSimple(&appels), nil)
	b.baux.err = domain.ErrPlaceIndisponible
	rec := b.playlist("100", "s1")
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Errorf("503 avec Retry-After attendu : %d", rec.Code)
	}
	if appels.Load() != 0 {
		t.Error("aucune requete amont sans bail")
	}
}

// CA4 : le client qui part en plein segment libere le bail tout de suite.
func TestClientPartiLibereLeBail(t *testing.T) {
	demarre := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "#EXTM3U\n#EXTINF:1,\ns1.ts\n") })
	mux.HandleFunc("/live/u/p/s1.ts", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		w.Write([]byte("debut"))
		w.(http.Flusher).Flush()
		close(demarre)
		select { // le segment ne finit jamais
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	b := nouveauBanc(t, mux, nil)
	seg := chaineSeg(t, b, "100")[0]
	srv := httptest.NewServer(b.h)
	defer srv.Close()
	ctx, stop := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+seg, nil)
	req.Header.Set("X-Session", "s1")
	rep, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	<-demarre
	buf := make([]byte, 5)
	io.ReadFull(rep.Body, buf)
	stop() // le lecteur quitte la chaine
	rep.Body.Close()
	attendreVrai(t, "le bail doit etre libere", func() bool { return b.baux.bail("s1").libere.Load() == 1 })
}

// CA4 : en zappant, l'annulation tardive du segment de l'ancienne chaine ne libere PAS
// le bail, que la nouvelle chaine utilise.
func TestAnnulationDeLAncienneChaineApresZappingNeLiberePasLeBail(t *testing.T) {
	demarre := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/live/u/p/100.m3u8", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "#EXTM3U\n#EXTINF:1,\ns1.ts\n") })
	mux.HandleFunc("/live/u/p/200.m3u8", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "#EXTM3U\n#EXTINF:1,\ns2.ts\n") })
	mux.HandleFunc("/live/u/p/s1.ts", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		w.Write([]byte("debut"))
		w.(http.Flusher).Flush()
		close(demarre)
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	b := nouveauBanc(t, mux, nil)
	seg := chaineSeg(t, b, "100")[0]
	srv := httptest.NewServer(b.h)
	defer srv.Close()
	ctx, stop := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+seg, nil)
	req.Header.Set("X-Session", "s1")
	rep, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	<-demarre
	if rec := b.playlist("200", "s1"); rec.Code != 200 { // zapping : meme session, autre chaine
		t.Fatalf("%d", rec.Code)
	}
	stop() // le lecteur abandonne le segment de l'ancienne chaine
	rep.Body.Close()
	time.Sleep(150 * time.Millisecond)
	if n := b.baux.bail("s1").libere.Load(); n != 0 {
		t.Fatalf("le bail sert la nouvelle chaine : il ne doit pas etre libere (%d)", n)
	}
}

func attendreVrai(t *testing.T, quoi string, cond func() bool) {
	t.Helper()
	fin := time.Now().Add(3 * time.Second)
	for time.Now().Before(fin) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal(quoi)
}

// ---- CA5 : 407 (avec doublures ; le scenario complet est dans integration_test.go) ----

// CA5 : un 407 n'est pas renvoye au premier refus : le service patiente et renouvelle le bail.
func TestPlaylistPatienteSur407(t *testing.T) {
	var appels atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) {
		if appels.Add(1) <= 4 {
			http.Error(w, "place occupee", 407)
			return
		}
		io.WriteString(w, playlistAmont("aaaa"))
	})
	b := nouveauBanc(t, mux, nil)
	rec := b.playlist("100", "s1")
	if rec.Code != 200 {
		t.Fatalf("la patience doit aboutir : %d", rec.Code)
	}
	if appels.Load() != 5 {
		t.Errorf("5 appels attendus, %d", appels.Load())
	}
	e := b.relais.Etat()
	if e.Patiences407 != 4 || e.NouveauxEssais != 0 {
		t.Errorf("4 patiences, aucun essai consomme : %+v", e)
	}
	if n := b.baux.bail("s1").renouvele.Load(); n < 5 {
		t.Errorf("le bail doit etre renouvele pendant l'attente : %d", n)
	}
}

// CA5 : la patience a une fin (bail.patience_max) : 503, pas d'attente infinie.
func TestPlaylistPatienceEpuisee503(t *testing.T) {
	var appels atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) {
		appels.Add(1)
		http.Error(w, "place occupee", 407)
	})
	b := nouveauBanc(t, mux, nil)
	debut := time.Now()
	rec := b.playlist("100", "s1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("503 attendu : %d", rec.Code)
	}
	if d := time.Since(debut); d < 300*time.Millisecond || d > 2*time.Second {
		t.Errorf("la patience doit durer ~patience_max (400 ms) : %v", d)
	}
	if appels.Load() < 5 {
		t.Errorf("la patience retente toutes les bail.reessai : %d appels", appels.Load())
	}
}

// CA5 : meme chose pour un segment.
func TestSegmentPatienteSur407(t *testing.T) {
	var appels atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "#EXTM3U\n#EXTINF:1,\ns1.ts\n") })
	mux.HandleFunc("/live/u/p/s1.ts", func(w http.ResponseWriter, r *http.Request) {
		if appels.Add(1) <= 3 {
			http.Error(w, "place occupee", 407)
			return
		}
		corpsDe(w, 5)
	})
	b := nouveauBanc(t, mux, nil)
	seg := chaineSeg(t, b, "100")[0]
	if rec := b.segment(seg, "s1"); rec.Code != 200 || appels.Load() != 4 {
		t.Fatalf("%d apres %d appels", rec.Code, appels.Load())
	}
}

// CA5 : le zapping demande la nouvelle chaine avec la MEME session (le bail est reutilise par Baux).
func TestZappingReutiliseLaSession(t *testing.T) {
	b := nouveauBanc(t, amontSimple(nil), nil)
	b.playlist("100", "s1")
	b.playlist("200", "s1")
	if len(b.baux.baux) != 1 {
		t.Errorf("un seul bail pour la session : %d", len(b.baux.baux))
	}
	if a := b.baux.acquisitions; len(a) != 2 || a[0] != "s1/100" || a[1] != "s1/200" {
		t.Errorf("acquisitions : %v", a)
	}
}

// ---- CA6 : statistiques ----

func TestEtatExposeLesStatistiques(t *testing.T) {
	var appels atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) {
		if appels.Add(1) == 1 {
			http.Error(w, "panne", http.StatusBadGateway)
			return
		}
		time.Sleep(10 * time.Millisecond)
		io.WriteString(w, playlistAmont("aaaa"))
	})
	mux.HandleFunc("/hls/", func(w http.ResponseWriter, r *http.Request) { corpsDe(w, 9) })
	b := nouveauBanc(t, mux, nil)
	seg := chaineSeg(t, b, "100")[0]
	b.segment(seg, "s1")
	rec := b.get(PrefixeParDefaut+"/etat", "s1", "X-Admin", "1")
	if rec.Code != 200 {
		t.Fatalf("/etat : %d", rec.Code)
	}
	var brut map[string]any
	json.Unmarshal(rec.Body.Bytes(), &brut)
	for _, cle := range []string{"playlists_servies", "echecs_amont", "nouveaux_essais", "playlists_de_secours", "playlist_ms", "segment_ms", "segments_en_cache"} {
		if _, ok := brut[cle]; !ok {
			t.Errorf("cle %q absente de /etat : %v", cle, brut)
		}
	}
	pl := brut["playlist_ms"].(map[string]any)
	sg := brut["segment_ms"].(map[string]any)
	for _, k := range []string{"mediane", "p95", "max"} {
		if _, ok := pl[k]; !ok {
			t.Errorf("playlist_ms.%s absent", k)
		}
		if _, ok := sg[k]; !ok {
			t.Errorf("segment_ms.%s absent", k)
		}
	}
	if brut["nouveaux_essais"].(float64) != 1 || brut["playlists_servies"].(float64) != 1 || brut["segments_en_cache"].(float64) != 1 {
		t.Errorf("valeurs inattendues : %v", brut)
	}
	if pl["max"].(float64) < pl["mediane"].(float64) || pl["max"].(float64) <= 0 {
		t.Errorf("distribution incoherente : %v", pl)
	}
}

func TestCentile(t *testing.T) {
	v := []float64{5, 1, 4, 2, 3}
	if centile(v, .5) != 2 || centile(v, 1) != 5 || centile(nil, .5) != 0 {
		t.Errorf("mediane %v max %v", centile(v, .5), centile(v, 1))
	}
}

// ---- CA7 : refus par defaut ----

func TestRefusParDefaut(t *testing.T) {
	var appels atomic.Int32
	// Sans authentificateur configure : TOUT est refuse, meme avec un en-tete de session.
	sans := nouveauBanc(t, amontSimple(&appels), func(o *Options) { o.Authentifier = nil })
	b := nouveauBanc(t, amontSimple(&appels), nil)
	seg := chaineSeg(t, b, "100")[0]
	appels.Store(0)
	chemins := []string{
		PrefixeParDefaut + "/100.m3u8", PrefixeParDefaut + "/etat", seg,
		"/", "/inconnu", PrefixeParDefaut + "/", PrefixeParDefaut + "/100/seg/x",
	}
	for _, c := range chemins {
		if rec := sans.get(c, "s1"); rec.Code != http.StatusUnauthorized {
			t.Errorf("sans authentificateur %s : 401 attendu, %d", c, rec.Code)
		}
		if rec := b.get(c, ""); rec.Code != http.StatusUnauthorized {
			t.Errorf("sans session %s : 401 attendu, %d", c, rec.Code)
		}
	}
	if appels.Load() != 0 {
		t.Error("aucune requete amont pour un appelant refuse")
	}
	if n := b.baux.nbAcquisitions(); n != 1 {
		t.Errorf("aucun bail pour un appelant refuse : %d", n)
	}
}

func TestChaineInvalideRefusee(t *testing.T) {
	b := nouveauBanc(t, amontSimple(nil), nil)
	for _, c := range []string{"../x", "a.b", "a%2Fb", strings.Repeat("1", 100)} {
		if rec := b.get(PrefixeParDefaut+"/"+c+".m3u8", "s1"); rec.Code == 200 {
			t.Errorf("%q ne doit pas etre servi", c)
		}
	}
}

func TestConfigurationIncompleteRefusee(t *testing.T) {
	l, ba, n := reglages()
	base := Options{Fournisseur: &fournisseurFactice{}, Baux: &bauxFactice{}, Live: l, Bail: ba, Reseau: n, Secret: secretTest}
	if _, err := Nouveau(base); err != nil {
		t.Fatal(err)
	}
	cas := map[string]func(*Options){
		"fournisseur": func(o *Options) { o.Fournisseur = nil },
		"baux":        func(o *Options) { o.Baux = nil },
		"cache":       func(o *Options) { o.Live.CacheSegments.Octets = 0 },
		"table":       func(o *Options) { o.Live.TableSegments.Nombre = 0 },
		"essais":      func(o *Options) { o.Reseau.EssaisSegment = 0 },
		"delai":       func(o *Options) { o.Reseau.DelaiPlaylist = 0 },
		"amont":       func(o *Options) { o.Reseau.RequetesAmontMax = 0 },
		"reessai":     func(o *Options) { o.Bail.Reessai = 0 },
		"secret":      func(o *Options) { o.Secret = []byte("court") },
	}
	for nom, f := range cas {
		o := base
		f(&o)
		if _, err := Nouveau(o); err == nil {
			t.Errorf("%s : une configuration incomplete doit etre refusee", nom)
		}
	}
}

// Le semaphore borne les requetes amont simultanees (connexion unique du fournisseur).
func TestLimiteDeRequetesAmont(t *testing.T) {
	var enCours, pic atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) {
		n := enCours.Add(1)
		for {
			p := pic.Load()
			if n <= p || pic.CompareAndSwap(p, n) {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
		enCours.Add(-1)
		io.WriteString(w, playlistAmont("aaaa"))
	})
	b := nouveauBanc(t, mux, func(o *Options) { o.Reseau.RequetesAmontMax = 2 })
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); b.playlist(fmt.Sprint(100+i), "s1") }(i)
	}
	wg.Wait()
	if pic.Load() > 2 {
		t.Errorf("pas plus de 2 requetes amont simultanees : pic %d", pic.Load())
	}
}

// Fermer annule les appels en cours et n'oublie aucune goroutine.
func TestFermerAnnuleLesAppelsEnCours(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	b := nouveauBanc(t, mux, nil)
	fini := make(chan int)
	go func() { fini <- b.playlist("100", "s1").Code }()
	time.Sleep(50 * time.Millisecond)
	termine := make(chan struct{})
	go func() { b.relais.Fermer(); close(termine) }()
	select {
	case <-termine:
	case <-time.After(2 * time.Second):
		t.Fatal("Fermer doit rendre la main rapidement")
	}
	if c := <-fini; c != http.StatusBadGateway {
		t.Errorf("le demandeur recoit une erreur franche : %d", c)
	}
}

// US-10 CA11 : la patience des baux et celle du 407 ne s'additionnent pas. Les baux
// ont deja fait attendre 300 ms (horloge injectee) sur 400 ms de patience_max : il
// ne reste que ~100 ms de patience pour les 407, pas 400 de plus.
func TestUneSeulePatienceBauxPuis407(t *testing.T) {
	var appels atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) {
		appels.Add(1)
		http.Error(w, "place occupee", 407)
	})
	b := nouveauBanc(t, mux, nil)
	b.baux.surAcquerir = func() { b.decalage.Add(int64(300 * time.Millisecond)) }
	debut := time.Now()
	rec := b.playlist("100", "s1")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("statut %d, attendu 503", rec.Code)
	}
	if d := time.Since(debut); d > 330*time.Millisecond {
		t.Errorf("patience totale %v : les deux attentes se sont additionnees (plafond 400 ms de l'horloge, dont 300 deja passees)", d)
	}
	// reessai 20 ms : ~5 appels pour ~100 ms ; 400 ms de plus en donneraient ~20.
	if n := appels.Load(); n > 10 {
		t.Errorf("%d appels amont pendant la patience des 407", n)
	}
}

// US-14 CA1 : le relais publie ses mesures par l'interface Instrumentation, avec des
// etiquettes bornees (ni session, ni chaine, ni URL).
func TestInstrumentationDuRelais(t *testing.T) {
	rec := &instrum.Enregistreur{}
	b := nouveauBanc(t, amontSimple(nil), func(o *Options) { o.Instrumentation = rec })
	seg := chaineSeg(t, b, "100")[0]
	b.segment(seg, "session-secrete-42") // depuis l'amont
	b.segment(seg, "session-secrete-42") // depuis le cache
	if n := rec.Total("live_playlists_total", "resultat", "ok"); n != 1 {
		t.Errorf("playlists ok = %v", n)
	}
	if rec.Total("live_segments_total", "source", "amont") != 1 || rec.Total("live_segments_total", "source", "cache") != 1 {
		t.Errorf("segments : %+v", rec.Mesures())
	}
	if rec.Observations("live_duree_amont_secondes", "type", "playlist") < 1 || rec.Observations("live_duree_amont_secondes", "type", "segment") != 1 {
		t.Errorf("durees : %+v", rec.Mesures())
	}
	rec.VerifierBornees(t, "session-secrete-42", "100", "aaaa", "/live/")
}

func TestInstrumentationDuRelaisSecoursEtErreur(t *testing.T) {
	rec := &instrum.Enregistreur{}
	var casse atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) {
		if casse.Load() {
			http.Error(w, "panne", 502)
			return
		}
		io.WriteString(w, playlistAmont("aaaa"))
	})
	b := nouveauBanc(t, mux, func(o *Options) {
		o.Instrumentation = rec
		o.Live.FraicheurPlaylist = 0
	})
	b.playlist("100", "s1")
	casse.Store(true)
	if r := b.playlist("100", "s1"); r.Header().Get("X-Relais") != "secours" {
		t.Fatalf("secours attendu : %d", r.Code)
	}
	if rec.Total("live_playlists_total", "resultat", "secours") != 1 || rec.Total("live_amont_echecs_total", "type", "playlist") < 1 {
		t.Errorf("secours : %+v", rec.Mesures())
	}
	if r := b.playlist("999", "s1"); r.Code != 502 || rec.Total("live_playlists_total", "resultat", "erreur") != 1 {
		t.Errorf("erreur : %d %+v", r.Code, rec.Mesures())
	}
	rec.VerifierBornees(t, "s1", "999")
}

// US-25 CA1 : un 401/403 amont (abonnement expire) et un 458 (adresse refusee) ne sont pas
// « fournisseur injoignable » : le corps {code, message} les distingue, avec la date d'expiration
// connue, et la supervision est prevenue. Statut 502 (jamais 401, qui deconnecterait l'interface).
func TestPlaylistRefusDuCompteOuDeLAdresseEstDistingue(t *testing.T) {
	for _, cas := range []struct {
		statut int
		code   string
		date   bool
	}{{401, "fournisseur_compte_invalide", true}, {403, "fournisseur_compte_invalide", true}, {458, "fournisseur_adresse_refusee", false}} {
		t.Run(fmt.Sprint(cas.statut), func(t *testing.T) {
			var appels atomic.Int32
			mux := http.NewServeMux()
			mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) {
				appels.Add(1)
				http.Error(w, "refus", cas.statut)
			})
			var signale []error
			exp := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
			b := nouveauBanc(t, mux, func(o *Options) {
				o.SignalerRefus = func(err error) { signale = append(signale, err) }
				o.Expiration = func() (time.Time, bool) { return exp, true }
			})
			rec := b.playlist("100", "s1")
			if rec.Code != http.StatusBadGateway || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
				t.Fatalf("%d %v", rec.Code, rec.Header())
			}
			var corps map[string]string
			if err := json.Unmarshal(rec.Body.Bytes(), &corps); err != nil {
				t.Fatal(err)
			}
			if corps["code"] != cas.code || corps["message"] == "" || rec.Header().Get("X-Mytv-Erreur") != cas.code {
				t.Errorf("corps : %v, en-tete %q", corps, rec.Header().Get("X-Mytv-Erreur"))
			}
			if (rec.Header().Get("X-Mytv-Expiration") == "2026-10-01T00:00:00Z") != cas.date {
				t.Errorf("en-tete d'expiration : %q", rec.Header().Get("X-Mytv-Expiration"))
			}
			if (corps["expiration"] == "2026-10-01T00:00:00Z") != cas.date || (!cas.date && corps["expiration"] != "") {
				t.Errorf("expiration : %v", corps)
			}
			if len(signale) != 1 {
				t.Errorf("%d signalements", len(signale))
			}
			if cas.date && appels.Load() != 1 { // identifiants refuses : pas de nouvel essai (le 458 garde ses essais)
				t.Errorf("%d appels amont", appels.Load())
			}
		})
	}
}

// Regression (recette reelle, 2026-10-15) : un 401/403 sur un SEGMENT n'est pas un refus du
// compte (jeton d'adresse expire) : il ne doit ni afficher « abonnement expire » ni etre
// signale a la supervision (qui declarait le compte refuse pendant 2 h alors que user_info
// le donnait actif). Le 458 (adresse refusee) reste un vrai refus.
func TestSegmentExpireNEstPasUnRefusDuCompte(t *testing.T) {
	for _, tc := range []struct {
		statut  int
		code    string
		signale bool
	}{{401, "segment_expire", false}, {403, "segment_expire", false}, {458, "fournisseur_adresse_refusee", true}} {
		var refuse atomic.Bool
		mux := http.NewServeMux()
		mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, playlistAmont("aaaa")) })
		mux.HandleFunc("/hls/", func(w http.ResponseWriter, r *http.Request) {
			if refuse.Load() {
				http.Error(w, "refus", tc.statut)
			}
		})
		var signale atomic.Bool
		b := nouveauBanc(t, mux, func(o *Options) { o.SignalerRefus = func(error) { signale.Store(true) } })
		seg := chaineSeg(t, b, "100")[0]
		refuse.Store(true)
		rec := b.segment(seg, "s1")
		if rec.Code != http.StatusBadGateway || !strings.Contains(rec.Body.String(), `"`+tc.code+`"`) {
			t.Errorf("HTTP %d : %d %s", tc.statut, rec.Code, rec.Body.String())
		}
		if rec.Header().Get("X-Mytv-Erreur") != tc.code {
			t.Errorf("HTTP %d : en-tete X-Mytv-Erreur = %q", tc.statut, rec.Header().Get("X-Mytv-Erreur"))
		}
		if signale.Load() != tc.signale {
			t.Errorf("HTTP %d : supervision signalee = %v, attendu %v", tc.statut, signale.Load(), tc.signale)
		}
	}
}
