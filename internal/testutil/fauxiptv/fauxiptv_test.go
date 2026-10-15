package fauxiptv

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func get(t *testing.T, url string) (*http.Response, []byte, error) {
	t.Helper()
	rep, err := http.Get(url)
	if err != nil {
		return nil, nil, err
	}
	defer rep.Body.Close()
	corps, err := io.ReadAll(rep.Body)
	return rep, corps, err
}

func statut(t *testing.T, url string) int {
	t.Helper()
	rep, _, err := get(t, url)
	if err != nil {
		t.Fatalf("GET %s : %v", url, err)
	}
	return rep.StatusCode
}

var reSegment = regexp.MustCompile(`(?m)^/hls/([0-9a-f]+)/(\d+_\d+)\.ts$`)

func segments(corps []byte) (hashs, noms []string) {
	for _, m := range reSegment.FindAllStringSubmatch(string(corps), -1) {
		hashs = append(hashs, m[1])
		noms = append(noms, m[2])
	}
	return
}

// CA1 : l'API sert le catalogue configure.
func TestAPIServLeCatalogueConfigure(t *testing.T) {
	f := Nouveau(t, Options{Catalogue: CatalogueExemple(3, 2, 1)})
	_, corps, err := get(t, f.URL()+"/player_api.php?username=u&password=p&action=get_live_streams")
	if err != nil {
		t.Fatal(err)
	}
	var liste []map[string]any
	if err := json.Unmarshal(corps, &liste); err != nil || len(liste) != 3 {
		t.Fatalf("attendu 3 chaines, obtenu %d (%v)", len(liste), err)
	}
	if got := statut(t, f.URL()+"/player_api.php?username=u&password=mauvais&action=get_live_streams"); got != 401 {
		t.Errorf("mauvais mot de passe : statut %d, attendu 401", got)
	}
	_, corps, _ = get(t, f.URL()+"/player_api.php?username=u&password=p&action=inconnue")
	if strings.TrimSpace(string(corps)) != "[]" {
		t.Errorf("action absente : %q, attendu une liste vide", corps)
	}
}

// CA1 : la playlist glisse d'un segment a chaque demande, la fenetre a la taille voulue.
func TestPlaylistAFenetreGlissante(t *testing.T) {
	f := Nouveau(t, Options{FenetreSegment: 4, PremiereSeq: 10})
	_, p1, _ := get(t, f.URL()+"/live/u/p/77.m3u8")
	_, p2, _ := get(t, f.URL()+"/live/u/p/77.m3u8")
	_, n1 := segments(p1)
	_, n2 := segments(p2)
	if len(n1) != 4 || n1[0] != "77_10" || n1[3] != "77_13" {
		t.Fatalf("premiere fenetre inattendue : %v", n1)
	}
	if n2[0] != "77_11" || n2[3] != "77_14" {
		t.Fatalf("la fenetre devait glisser d'un cran : %v", n2)
	}
	if !strings.Contains(string(p2), "#EXT-X-MEDIA-SEQUENCE:11") {
		t.Errorf("MEDIA-SEQUENCE incoherente :\n%s", p2)
	}
}

// CA1 : segment factice de la taille configuree, Range accepte.
func TestSegmentALaTailleConfiguree(t *testing.T) {
	f := Nouveau(t, Options{TailleSegment: 1000})
	_, corps, err := get(t, f.URL()+"/hls/abc/77_10.ts")
	if err != nil || len(corps) != 1000 || corps[0] != 0x47 {
		t.Fatalf("segment : %d octets, err=%v", len(corps), err)
	}
	req, _ := http.NewRequest("GET", f.URL()+"/hls/abc/77_10.ts", nil)
	req.Header.Set("Range", "bytes=0-9")
	rep, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer rep.Body.Close()
	if rep.StatusCode != 206 {
		t.Errorf("Range : statut %d, attendu 206", rep.StatusCode)
	}
}

// CA2 : le hash change a chaque playlist, mais le meme numero garde le meme contenu.
func TestHashDeCheminChangeAChaquePlaylist(t *testing.T) {
	f := Nouveau(t, Options{AvanceParPlaylist: 1})
	vus := map[string]bool{}
	for i := 0; i < 6; i++ {
		_, p, _ := get(t, f.URL()+"/live/u/p/5.m3u8")
		hs, _ := segments(p)
		if len(hs) == 0 {
			t.Fatal("playlist sans segment")
		}
		for j := range hs {
			if hs[j] != hs[0] {
				t.Fatalf("hash different au sein d'une meme playlist : %v", hs)
			}
		}
		if vus[hs[0]] {
			t.Fatalf("hash %s reutilise a la playlist %d", hs[0], i)
		}
		vus[hs[0]] = true
	}
}

// CA2 : tout hash est accepte pour un segment (le fournisseur garde les anciens).
func TestSegmentServiAvecUnAncienHash(t *testing.T) {
	f := Nouveau(t, Options{})
	_, a, _ := get(t, f.URL()+"/hls/aaaa/5_150.ts")
	_, b, _ := get(t, f.URL()+"/hls/bbbb/5_150.ts")
	if !bytes.Equal(a, b) {
		t.Error("un meme numero de segment doit avoir le meme contenu quel que soit le hash")
	}
}

// CA1 : la redirection vers un autre noeud est suivie par un client standard.
func TestPlaylistRedirigeeVersUnAutreNoeud(t *testing.T) {
	f := Nouveau(t, Options{RedirigerPlaylist: true})
	rep, corps, err := get(t, f.URL()+"/live/u/p/5.m3u8")
	if err != nil || rep.StatusCode != 200 || !strings.HasPrefix(rep.Request.URL.Path, "/noeud/") {
		t.Fatalf("redirection non suivie : %v %v", rep, err)
	}
	if !strings.HasPrefix(string(corps), "#EXTM3U") {
		t.Error("playlist attendue apres redirection")
	}
}

// CA3 : statuts programmes, pour N demandes puis retour a la normale.
func TestPanneStatutPourNDemandes(t *testing.T) {
	for _, code := range []int{407, 458, 502, 405} {
		f := Nouveau(t, Options{})
		f.Programmer(Panne{Cible: CiblePlaylist, Statut: code, Fois: 2})
		for i := 0; i < 2; i++ {
			if got := statut(t, f.URL()+"/live/u/p/5.m3u8"); got != code {
				t.Fatalf("demande %d : statut %d, attendu %d", i, got, code)
			}
		}
		if got := statut(t, f.URL()+"/live/u/p/5.m3u8"); got != 200 {
			t.Errorf("apres N demandes : statut %d, attendu 200", got)
		}
	}
}

// CA3 : une panne limitee dans le temps suit l'horloge injectee.
func TestPanneLimiteeDansLeTemps(t *testing.T) {
	var mu sync.Mutex
	now := time.Unix(1000, 0)
	f := Nouveau(t, Options{Horloge: func() time.Time { mu.Lock(); defer mu.Unlock(); return now }})
	f.Programmer(Panne{Cible: CiblePlaylist, Statut: 407, Duree: 30 * time.Second})
	if got := statut(t, f.URL()+"/live/u/p/5.m3u8"); got != 407 {
		t.Fatalf("pendant la panne : %d", got)
	}
	mu.Lock()
	now = now.Add(31 * time.Second)
	mu.Unlock()
	if got := statut(t, f.URL()+"/live/u/p/5.m3u8"); got != 200 {
		t.Errorf("panne expiree : %d, attendu 200", got)
	}
}

// CA3 : la cible limite la panne ; une panne sur les segments laisse les playlists intactes.
func TestPanneVisantLesSegmentsEpargneLesPlaylists(t *testing.T) {
	f := Nouveau(t, Options{})
	f.Programmer(Panne{Cible: CibleSegment, Statut: 502})
	if got := statut(t, f.URL()+"/live/u/p/5.m3u8"); got != 200 {
		t.Errorf("playlist : %d", got)
	}
	if got := statut(t, f.URL()+"/hls/a/5_1.ts"); got != 502 {
		t.Errorf("segment : %d, attendu 502", got)
	}
	f.Effacer()
	if got := statut(t, f.URL()+"/hls/a/5_1.ts"); got != 200 {
		t.Errorf("apres Effacer : %d", got)
	}
}

// CA3 : latence, interrompue par l'annulation du client.
func TestLatenceRespecteLAnnulationDuClient(t *testing.T) {
	f := Nouveau(t, Options{})
	f.Programmer(Panne{Cible: CiblePlaylist, Latence: 5 * time.Second})
	ctx, annuler := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer annuler()
	req, _ := http.NewRequestWithContext(ctx, "GET", f.URL()+"/live/u/p/5.m3u8", nil)
	debut := time.Now()
	if rep, err := http.DefaultClient.Do(req); err == nil {
		rep.Body.Close()
		t.Fatal("le delai du client aurait du expirer")
	}
	if time.Since(debut) > 2*time.Second {
		t.Error("l'annulation n'a pas ete prise en compte rapidement")
	}
}

// CA3 : coupure en milieu de segment = EOF inattendu cote client.
func TestCoupureEnMilieuDeSegment(t *testing.T) {
	f := Nouveau(t, Options{TailleSegment: 2000})
	f.Programmer(Panne{Cible: CibleSegment, Coupure: true, Fois: 1})
	_, corps, err := get(t, f.URL()+"/hls/a/5_1.ts")
	if err == nil {
		t.Fatalf("lecture complete (%d octets) alors qu'une coupure etait programmee", len(corps))
	}
	if len(corps) == 0 || len(corps) >= 2000 {
		t.Errorf("attendu un corps partiel, obtenu %d octets", len(corps))
	}
	if _, corps, err = get(t, f.URL()+"/hls/a/5_1.ts"); err != nil || len(corps) != 2000 {
		t.Errorf("apres la coupure unique : %d octets, err=%v", len(corps), err)
	}
}

// CA3 : corps vide.
func TestCorpsVide(t *testing.T) {
	f := Nouveau(t, Options{})
	f.Programmer(Panne{Cible: CiblePlaylist, CorpsVide: true, Fois: 1})
	rep, corps, _ := get(t, f.URL()+"/live/u/p/5.m3u8")
	if rep.StatusCode != 200 || len(corps) != 0 {
		t.Errorf("statut %d, %d octets : attendu 200 et corps vide", rep.StatusCode, len(corps))
	}
}

// CA4 : zapping refuse tant que la place de la precedente chaine n'a pas expire.
func TestUneSeuleConnexionRefuseUneAutreChaine(t *testing.T) {
	var mu sync.Mutex
	now := time.Unix(1000, 0)
	avancer := func(d time.Duration) { mu.Lock(); now = now.Add(d); mu.Unlock() }
	f := Nouveau(t, Options{Horloge: func() time.Time { mu.Lock(); defer mu.Unlock(); return now }})
	f.UneSeuleConnexion(100 * time.Second)

	if got := statut(t, f.URL()+"/live/u/p/1.m3u8"); got != 200 {
		t.Fatalf("premiere chaine : %d", got)
	}
	// La meme chaine (playlist ET segment) reste autorisee.
	if got := statut(t, f.URL()+"/live/u/p/1.m3u8"); got != 200 {
		t.Errorf("meme chaine : %d", got)
	}
	if got := statut(t, f.URL()+"/hls/a/1_150.ts"); got != 200 {
		t.Errorf("segment de la meme chaine : %d", got)
	}
	// Une autre chaine : 407, playlist comme segment.
	avancer(40 * time.Second)
	if got := statut(t, f.URL()+"/live/u/p/2.m3u8"); got != 407 {
		t.Errorf("autre chaine : %d, attendu 407", got)
	}
	if got := statut(t, f.URL()+"/hls/a/2_150.ts"); got != 407 {
		t.Errorf("segment d'une autre chaine : %d, attendu 407", got)
	}
	// Un refus ne prolonge pas la place de la chaine en cours : elle expire 100 s apres le dernier usage.
	avancer(61 * time.Second)
	if got := statut(t, f.URL()+"/live/u/p/2.m3u8"); got != 200 {
		t.Errorf("apres expiration : %d, attendu 200", got)
	}
	if f.Compteurs().Refus407 != 2 {
		t.Errorf("Refus407 = %d, attendu 2", f.Compteurs().Refus407)
	}
}

// CA5 : compteurs et traces (sans identifiants).
func TestCompteursEtTraces(t *testing.T) {
	f := Nouveau(t, Options{Catalogue: CatalogueExemple(1, 0, 0)})
	get(t, f.URL()+"/player_api.php?username=u&password=p&action=get_live_streams")
	get(t, f.URL()+"/live/u/p/9.m3u8")
	get(t, f.URL()+"/live/u/p/9.m3u8")
	get(t, f.URL()+"/hls/a/9_150.ts")
	c := f.Compteurs()
	if c.API != 1 || c.ParAction["get_live_streams"] != 1 || c.Playlists != 2 || c.PlaylistsParChaine["9"] != 2 ||
		c.Segments != 1 || c.SegmentsParChaine["9"] != 1 {
		t.Errorf("compteurs inattendus : %+v", c)
	}
	for _, tr := range f.Traces() {
		if strings.Contains(tr.Chemin, "/u/p/") {
			t.Errorf("identifiants dans la trace : %s", tr.Chemin)
		}
	}
	f.RazCompteurs()
	if c := f.Compteurs(); c.Playlists != 0 || len(f.Traces()) != 0 {
		t.Errorf("RazCompteurs n'a pas remis a zero : %+v", c)
	}
}

// CA5 : acces concurrents sous -race.
func TestAccesConcurrents(t *testing.T) {
	f := Nouveau(t, Options{})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				get(t, f.URL()+"/live/u/p/3.m3u8")
				_ = f.Compteurs()
			}
		}()
	}
	f.Programmer(Panne{Cible: CibleSegment, Statut: 502, Fois: 3})
	wg.Wait()
	if got := f.Compteurs().PlaylistsParChaine["3"]; got != 160 {
		t.Errorf("playlists comptees : %d, attendu 160", got)
	}
}

func getRange(t *testing.T, url, plage string) (*http.Response, []byte, error) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Range", plage)
	rep, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer rep.Body.Close()
	corps, err := io.ReadAll(rep.Body)
	return rep, corps, err
}

// Films et episodes : Range honore (206), taille configurable, contenu stable.
func TestFilmEtEpisodeAvecRange(t *testing.T) {
	f := Nouveau(t, Options{TailleFilm: 10000})
	for _, chemin := range []string{"/movie/u/p/7.mp4", "/series/u/p/7.mp4"} {
		_, tout, err := get(t, f.URL()+chemin)
		if err != nil || len(tout) != 10000 {
			t.Fatalf("%s : %d octets, %v", chemin, len(tout), err)
		}
		rep, part, err := getRange(t, f.URL()+chemin, "bytes=1000-1999")
		if err != nil || rep.StatusCode != 206 || !bytes.Equal(part, tout[1000:2000]) {
			t.Errorf("%s plage : statut %d, %v", chemin, rep.StatusCode, err)
		}
		if cr := rep.Header.Get("Content-Range"); cr != "bytes 1000-1999/10000" {
			t.Errorf("Content-Range = %q", cr)
		}
	}
	if statut(t, f.URL()+"/movie/mauvais/mdp/7.mp4") != 401 {
		t.Error("identifiants non verifies")
	}
}

// CA3 : un fournisseur qui ignore Range repond 200 et le fichier entier.
func TestPanneIgnorerRange(t *testing.T) {
	f := Nouveau(t, Options{TailleFilm: 5000})
	f.Programmer(Panne{Cible: CibleFilm, IgnorerRange: true, Fois: 1})
	rep, corps, _ := getRange(t, f.URL()+"/movie/u/p/7.mp4", "bytes=100-")
	if rep.StatusCode != 200 || len(corps) != 5000 {
		t.Errorf("statut %d, %d octets", rep.StatusCode, len(corps))
	}
	if rep, _, _ = getRange(t, f.URL()+"/movie/u/p/7.mp4", "bytes=100-"); rep.StatusCode != 206 {
		t.Errorf("apres la panne : statut %d", rep.StatusCode)
	}
}

// CA3 : la coupure d'une reponse a plage coupe la moitie de LA PLAGE.
func TestCoupureSurUnePlage(t *testing.T) {
	f := Nouveau(t, Options{TailleFilm: 10000})
	f.Programmer(Panne{Cible: CibleFilm, Coupure: true, Fois: 1})
	rep, corps, err := getRange(t, f.URL()+"/movie/u/p/7.mp4", "bytes=2000-5999")
	if err == nil || rep.StatusCode != 206 || len(corps) != 2000 {
		t.Errorf("statut %d, %d octets, err=%v (attendu 206, 2000 octets, erreur)", rep.StatusCode, len(corps), err)
	}
}
