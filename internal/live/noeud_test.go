package live

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mytv/internal/config"
	"mytv/internal/testutil/fauxiptv"
	"mytv/internal/testutil/instrum"
)

// US-37 : les playlists du direct sont rechargees sur le noeud de diffusion obtenu apres
// le 302 du serveur central, pas sur le central lui-meme. Faux fournisseur, vrai client
// Xtream, vrais baux.

func avecNoeuds(ajuste func(*Options)) func(*Options) {
	return func(o *Options) {
		o.Live.FraicheurPlaylist = 0 // chaque demande recharge la playlist : on compte les requetes amont
		if ajuste != nil {
			ajuste(o)
		}
	}
}

// CA1 : N playlists d'une chaine ne passent qu'UNE fois par le serveur central (le 302 de la
// premiere) ; les suivantes sont lues directement sur le noeud. /etat et la metrique le disent.
func TestCA1_PlaylistsRechargeesSurLeNoeudApresLe302(t *testing.T) {
	rec := &instrum.Enregistreur{}
	x := nouvelleIntegration(t, fauxiptv.Options{RedirigerPlaylist: true}, avecNoeuds(func(o *Options) { o.Instrumentation = rec }))
	var precedent []string
	for i := 0; i < 5; i++ {
		r := x.get(PrefixeParDefaut+"/1001.m3u8", "s1")
		if r.Code != 200 || r.Header().Get("X-Relais") == "secours" {
			t.Fatalf("playlist %d : %d %s", i, r.Code, r.Body.String())
		}
		seg := segments(r.Body.String())
		// Le noeud sert bien une playlist qui avance (fenetre glissante du faux), aux URL stables.
		if i > 0 && (len(seg) == 0 || seg[0] != precedent[1]) {
			t.Errorf("playlist %d : la fenetre doit glisser d'un segment aux memes URL :\n%v\n%v", i, precedent, seg)
		}
		precedent = seg
	}
	c := x.faux.Compteurs()
	if c.RedirectionsPlaylists != 1 || c.PlaylistsNoeud != 5 {
		t.Errorf("1 passage par le central et 5 playlists du noeud attendus : %+v", c)
	}
	if e := x.relais.Etat(); e.PlaylistsNoeud != 4 || e.ReplisNoeud != 0 || e.PlaylistsServies != 5 {
		t.Errorf("etat : %+v", e)
	}
	if rec.Total("live_playlists_noeud_total", "resultat", "ok") != 4 || rec.Total("live_playlists_noeud_total", "resultat", "repli") != 0 {
		t.Errorf("metriques : %+v", rec.Mesures())
	}
	// Le segment est lu chez le fournisseur par l'URL resolue contre le noeud.
	if r := x.get(precedent[0], "s1"); r.Code != 200 || r.Body.Len() != 4096 {
		t.Errorf("segment : %d", r.Code)
	}
	rec.VerifierBornees(t, "s1", "1001", "/noeud/", "/live/")
	x.verifierSecrets(t)
}

// CA1 : le noeud retenu meurt (502, 407, corps qui n'est pas une playlist, ou plus de reponse) :
// oubli immediat et retour au central, en moins d'une seconde, sans consommer ni essai ni
// patience 407 ; metrique `repli` et journal INFO (chaine et cause, jamais d'adresse).
func TestCA1_NoeudMortRepliImmediatSurLeCentral(t *testing.T) {
	for nom, statut := range map[string]int{"502": 502, "407": 407, "corps sans EXTM3U": 0, "noeud muet": -1} {
		t.Run(nom, func(t *testing.T) {
			rec := &instrum.Enregistreur{}
			x := nouvelleIntegration(t, fauxiptv.Options{RedirigerPlaylist: true}, avecNoeuds(func(o *Options) {
				o.Instrumentation = rec
				o.Reseau.DelaiPlaylist = config.Duree(200 * time.Millisecond) // un noeud muet coute au plus ce delai
			}))
			for i := 0; i < 2; i++ {
				if r := x.get(PrefixeParDefaut+"/1001.m3u8", "s1"); r.Code != 200 {
					t.Fatalf("playlist %d : %d", i, r.Code)
				}
			}
			x.faux.TuerNoeud(statut)
			debut := time.Now()
			r := x.get(PrefixeParDefaut+"/1001.m3u8", "s1")
			duree := time.Since(debut)
			if r.Code != 200 || r.Header().Get("X-Relais") == "secours" || !strings.Contains(r.Body.String(), "#EXTM3U") {
				t.Fatalf("apres la mort du noeud, le central doit servir la playlist : %d %q", r.Code, r.Header().Get("X-Relais"))
			}
			if duree > time.Second {
				t.Errorf("repli en %v, attendu < 1 s", duree)
			}
			c := x.faux.Compteurs()
			if c.RedirectionsPlaylists != 2 {
				t.Errorf("le central doit avoir redirige une 2e fois (nouveau noeud) : %+v", c)
			}
			e := x.relais.Etat()
			if e.ReplisNoeud != 1 || e.PlaylistsNoeud != 1 || e.NouveauxEssais != 0 || e.Patiences407 != 0 || e.PlaylistsDeSecours != 0 {
				t.Errorf("un repli, aucun essai ni patience consommes : %+v", e)
			}
			if rec.Total("live_playlists_noeud_total", "resultat", "repli") != 1 {
				t.Errorf("metrique repli : %+v", rec.Mesures())
			}
			j := x.journal.String()
			if !strings.Contains(j, "repli sur le serveur central") || !strings.Contains(j, "chaine=1001") {
				t.Errorf("journal INFO du repli attendu :\n%s", j)
			}
			if strings.Contains(j, "/noeud/") || strings.Contains(j, x.faux.URL()) {
				t.Errorf("le journal ne doit porter aucune adresse du fournisseur :\n%s", j)
			}
			x.verifierSecrets(t, r.Body.String())
			// Le nouveau noeud est retenu a son tour.
			x.get(PrefixeParDefaut+"/1001.m3u8", "s1")
			if c := x.faux.Compteurs(); c.RedirectionsPlaylists != 2 {
				t.Errorf("le nouveau noeud doit etre retenu : %+v", c)
			}
		})
	}
}

// CA1 : le noeud mort est OUBLIE, pas seulement contourne : si le central tombe a son tour
// (playlist de secours), les demandes suivantes ne retentent plus le noeud mort.
func TestCA1_NoeudMortOublieMemeSiLeCentralTombe(t *testing.T) {
	x := nouvelleIntegration(t, fauxiptv.Options{RedirigerPlaylist: true}, avecNoeuds(nil))
	for i := 0; i < 2; i++ {
		if r := x.get(PrefixeParDefaut+"/1001.m3u8", "s1"); r.Code != 200 {
			t.Fatalf("%d", r.Code)
		}
	}
	x.faux.TuerNoeud(502)
	x.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CiblePlaylist, Statut: 502}) // le central aussi, sans limite
	for i := 0; i < 2; i++ {
		if r := x.get(PrefixeParDefaut+"/1001.m3u8", "s1"); r.Code != 200 || r.Header().Get("X-Relais") != "secours" {
			t.Fatalf("secours attendu : %d %q", r.Code, r.Header().Get("X-Relais"))
		}
	}
	surLeNoeudMort := 0
	for _, tr := range x.faux.Traces() {
		if strings.HasPrefix(tr.Chemin, "/noeud/0/") {
			surLeNoeudMort++
		}
	}
	// Deux playlists servies par le noeud 0, puis UN seul echec : la deuxieme demande en secours
	// ne l'a plus retente.
	if surLeNoeudMort != 3 {
		t.Errorf("%d requetes vers le noeud mort, attendu 3 (2 succes + 1 echec qui le fait oublier)", surLeNoeudMort)
	}
	if e := x.relais.Etat(); e.ReplisNoeud != 1 {
		t.Errorf("un seul repli : %+v", e)
	}
}

// CA1 : la retention est bornee par live.retention_noeud : au-dela, la playlist repasse par
// le central (qui designe le noeud du moment).
func TestCA1_RetentionDuNoeudBornee(t *testing.T) {
	var decalage atomic.Int64
	x := nouvelleIntegration(t, fauxiptv.Options{RedirigerPlaylist: true}, avecNoeuds(func(o *Options) {
		o.Live.RetentionNoeud = config.Duree(2 * time.Minute)
		o.Horloge = func() time.Time { return time.Now().Add(time.Duration(decalage.Load())) }
	}))
	for i := 0; i < 3; i++ {
		if r := x.get(PrefixeParDefaut+"/1001.m3u8", "s1"); r.Code != 200 {
			t.Fatalf("%d", r.Code)
		}
	}
	decalage.Store(int64(119 * time.Second))
	x.get(PrefixeParDefaut+"/1001.m3u8", "s1")
	if c := x.faux.Compteurs(); c.RedirectionsPlaylists != 1 {
		t.Fatalf("avant la fin de la retention, un seul passage par le central : %+v", c)
	}
	decalage.Store(int64(121 * time.Second))
	x.get(PrefixeParDefaut+"/1001.m3u8", "s1")
	x.get(PrefixeParDefaut+"/1001.m3u8", "s1")
	if c := x.faux.Compteurs(); c.RedirectionsPlaylists != 2 {
		t.Errorf("retention echue : un 2e passage par le central, puis le noeud a nouveau : %+v", c)
	}
	if e := x.relais.Etat(); e.ReplisNoeud != 0 {
		t.Errorf("l'expiration n'est pas un repli : %+v", e)
	}
}

// CA1 : une chaine sans redirection ne retient rien et ne change pas de comportement.
func TestCA1_SansRedirectionAucuneRetention(t *testing.T) {
	x := nouvelleIntegration(t, fauxiptv.Options{}, avecNoeuds(nil))
	for i := 0; i < 3; i++ {
		x.get(PrefixeParDefaut+"/1001.m3u8", "s1")
	}
	if e := x.relais.Etat(); e.PlaylistsNoeud != 0 || e.ReplisNoeud != 0 || e.PlaylistsServies != 3 {
		t.Errorf("etat : %+v", e)
	}
	if c := x.faux.Compteurs(); c.PlaylistsNoeud != 0 || c.Playlists != 3 {
		t.Errorf("compteurs : %+v", c)
	}
}

// amontANoeuds : un « serveur central » qui redirige vers le noeud courant ; chaque noeud sert
// sa propre playlist (ou un 503 une fois tue). Les segments sont tous lisibles.
type amontANoeuds struct {
	mu        sync.Mutex
	courant   int
	playlists map[int]string // par noeud
	morts     map[int]bool
	central   atomic.Int32
}

func (a *amontANoeuds) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) {
		a.central.Add(1)
		a.mu.Lock()
		n := a.courant
		a.mu.Unlock()
		http.Redirect(w, r, "/noeud/"+strconv.Itoa(n)+r.URL.Path, http.StatusFound)
	})
	mux.HandleFunc("/noeud/{gen}/live/", func(w http.ResponseWriter, r *http.Request) {
		gen, _ := strconv.Atoi(r.PathValue("gen"))
		a.mu.Lock()
		pl, mort := a.playlists[gen], a.morts[gen]
		a.mu.Unlock()
		if mort {
			http.Error(w, "noeud mort", http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, pl)
	})
	mux.HandleFunc("/hls/", func(w http.ResponseWriter, r *http.Request) { corpsDe(w, 9) })
	return mux
}

func (a *amontANoeuds) tuer(statutSuivant int, playlist string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.morts[a.courant] = true
	a.courant++
	a.playlists[a.courant] = playlist
}

// CA2 : un changement de noeud suit les memes regles de continuite que le reste : un noeud en
// retard qui annonce les MEMES segments n'est pas un redemarrage (aucune discontinuite, meme
// epoque, memes URL de segments) ; un noeud qui repart a zero avec de NOUVEAUX segments en est un
// (numerotation continue et EXT-X-DISCONTINUITY, comme continuite.go le fait sans changement de noeud).
func TestCA2_ChangementDeNoeudSansRedemarrage(t *testing.T) {
	a := &amontANoeuds{playlists: map[int]string{0: playlistNommee(10, "aaaa", "100_10.ts", "100_11.ts", "100_12.ts")}, morts: map[int]bool{}}
	b := nouveauBanc(t, a.handler(), func(o *Options) { o.Live.FraicheurPlaylist = 0 })
	un := b.playlist("100", "s1")
	if un.Code != 200 {
		t.Fatal(un.Code)
	}
	segsUn := segments(un.Body.String())

	// Noeud 1, en retard d'un segment : memes segments, numerotation du fournisseur conservee.
	a.tuer(503, playlistNommee(9, "bbbb", "100_9.ts", "100_10.ts", "100_11.ts"))
	deux := b.playlist("100", "s1")
	if deux.Code != 200 || deux.Header().Get("X-Relais") == "secours" {
		t.Fatalf("apres la mort du noeud 0 : %d %q", deux.Code, deux.Header().Get("X-Relais"))
	}
	if strings.Contains(deux.Body.String(), "#EXT-X-DISCONTINUITY") || b.relais.epoque("100") != 0 {
		t.Errorf("un noeud en retard avec les memes segments n'est pas un redemarrage :\n%s", deux.Body.String())
	}
	segsDeux := segments(deux.Body.String())
	if len(segsDeux) != 3 || segsDeux[1] != segsUn[0] || segsDeux[2] != segsUn[1] {
		t.Errorf("les segments communs gardent leur URL malgre le changement de noeud et de hash :\n%v\n%v", segsUn, segsDeux)
	}
	if e := b.relais.Etat(); e.ReplisNoeud != 1 || e.PlaylistsNoeud != 0 || a.central.Load() != 2 {
		t.Errorf("un repli, deux passages par le central : %+v, central=%d", e, a.central.Load())
	}

	// Noeud 2 : flux redemarre (sequence 0, segments inconnus) : discontinuite et numerotation continue.
	a.tuer(503, playlistNommee(0, "cccc", "100_0.ts", "100_1.ts", "100_2.ts"))
	trois := b.playlist("100", "s1")
	if trois.Code != 200 {
		t.Fatal(trois.Code)
	}
	corps := trois.Body.String()
	if !strings.Contains(corps, "#EXT-X-DISCONTINUITY\n") || b.relais.epoque("100") != 1 {
		t.Errorf("un noeud qui repart a zero avec de nouveaux segments est un redemarrage :\n%s", corps)
	}
	if seq := ligneApres(t, corps, "#EXT-X-MEDIA-SEQUENCE:"); seq != "#EXT-X-MEDIA-SEQUENCE:12" {
		// Dernier servi par le noeud 1 : 9 + 3 - 1 = 11 ; le premier du nouveau flux prend 12.
		t.Errorf("MEDIA-SEQUENCE servie = %s, attendu 12 (suite du dernier segment servi)", seq)
	}
	if e := b.relais.Etat(); e.ReplisNoeud != 2 {
		t.Errorf("deux replis : %+v", e)
	}
}

// CA3 : pendant une conversion (ffmpeg alimente par la boucle amont de transcodage.go), les
// playlists sont rechargees sur le noeud : UNE seule requete au serveur central par ouverture.
func TestCA3_ConversionSansRequeteDePlusAuCentral(t *testing.T) {
	b := nouveauBancConversion(t, optionsBanc{formats: map[string]formatTest{"201": formatHEVCEAC3},
		faux: func(o *fauxiptv.Options) { o.RedirigerPlaylist = true }})
	rec := b.playlist("201", "s1", "")
	if rec.Code != 200 || rec.Header().Get("X-Mytv-Mode") != "video" {
		t.Fatalf("HTTP %d mode %q : %s", rec.Code, rec.Header().Get("X-Mytv-Mode"), rec.Body.String())
	}
	// La boucle amont recharge la playlist toutes les ~50 ms : on la laisse tourner.
	attendreQue(t, 10*time.Second, func() bool { return b.faux.Compteurs().PlaylistsNoeud >= 8 }, "rechargements de playlist par la conversion")
	for i := 0; i < 3; i++ {
		if r := b.playlist("201", "s1", ""); r.Code != 200 {
			t.Fatalf("playlist locale : %d", r.Code)
		}
	}
	c := b.faux.Compteurs()
	if c.RedirectionsPlaylists != 1 {
		t.Errorf("une seule requete au central par ouverture, %d passages (%d playlists du noeud)", c.RedirectionsPlaylists, c.PlaylistsNoeud)
	}
	// La premiere playlist (celle du 302) est comptee par le faux comme servie par le noeud,
	// mais le relais, lui, l'a obtenue par l'adresse canonique.
	if e := b.relais.Etat(); e.PlaylistsNoeud != c.PlaylistsNoeud-1 || e.ReplisNoeud != 0 {
		t.Errorf("etat : %+v (faux : %d playlists du noeud)", e, c.PlaylistsNoeud)
	}
	b.relais.FinDeLecture("s1", "201")
	b.aucunResidu(t)
}

// CA4 : /etat expose le nombre de playlists servies depuis un noeud et de replis.
func TestCA4_EtatExposePlaylistsNoeudEtReplis(t *testing.T) {
	x := nouvelleIntegration(t, fauxiptv.Options{RedirigerPlaylist: true}, avecNoeuds(nil))
	x.get(PrefixeParDefaut+"/1001.m3u8", "s1")
	x.get(PrefixeParDefaut+"/1001.m3u8", "s1")
	x.faux.TuerNoeud(502)
	x.get(PrefixeParDefaut+"/1001.m3u8", "s1")
	if r := x.get(PrefixeParDefaut+"/etat", "s1"); r.Code != 403 {
		t.Fatalf("/etat sans administrateur : %d", r.Code)
	}
	req := httptest.NewRequest("GET", PrefixeParDefaut+"/etat", nil)
	req.Header.Set("X-Session", "admin")
	req.Header.Set("X-Admin", "1")
	rec := httptest.NewRecorder()
	x.h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"playlists_noeud":1`) || !strings.Contains(rec.Body.String(), `"replis_noeud":1`) {
		t.Errorf("/etat : %d %s", rec.Code, rec.Body.String())
	}
}
