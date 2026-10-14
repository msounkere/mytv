package live

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"mytv/internal/baux"
	"mytv/internal/config"
	"mytv/internal/testutil/fauxiptv"
	"mytv/internal/xtream"
)

// Tests d'integration : le VRAI client Xtream, les VRAIS baux et le faux fournisseur
// (US-03, US-05, US-04). Rien n'est simule cote relais.

const (
	utilisateurSecret = "utilisateur-confidentiel"
	motDePasseSecret  = "motdepasse-confidentiel"
)

// tampon est un bytes.Buffer utilisable depuis plusieurs goroutines (le journal).
type tampon struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (t *tampon) Write(p []byte) (int, error) { t.mu.Lock(); defer t.mu.Unlock(); return t.b.Write(p) }
func (t *tampon) String() string              { t.mu.Lock(); defer t.mu.Unlock(); return t.b.String() }

type integ struct {
	faux    *fauxiptv.Faux
	relais  *Relais
	h       http.Handler
	baux    *baux.Gestionnaire
	journal *tampon
}

func nouvelleIntegration(t *testing.T, opt fauxiptv.Options, ajuste func(*Options)) *integ {
	t.Helper()
	opt.Utilisateur, opt.MotDePasse = utilisateurSecret, motDePasseSecret
	faux := fauxiptv.Nouveau(t, opt)
	live, bail, reseau := reglages()
	bail.PatienceMax = config.Duree(2 * time.Second)
	bail.Reessai = config.Duree(30 * time.Millisecond)
	f := config.Fournisseur{ID: "iptva", Base: faux.URL(), ConnexionsMax: 1, Reseau: reseau}
	f.Reseau.DelaiConnexion = config.Duree(time.Second)
	f.Reseau.DelaiEntete = config.Duree(10 * time.Second)
	journal := &tampon{}
	log := slog.New(slog.NewTextHandler(journal, &slog.HandlerOptions{Level: slog.LevelDebug}))
	four := xtream.Nouveau(f, utilisateurSecret, motDePasseSecret, xtream.AvecJournal(log))
	gb, err := baux.Nouveau(bail, map[string]int{"iptva": 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	o := Options{
		Fournisseur: four, Baux: gb, Live: live, Bail: bail, Reseau: reseau,
		Secret: secretTest, Authentifier: authTest, Journal: log,
	}
	if ajuste != nil {
		ajuste(&o)
	}
	r, err := Nouveau(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Fermer)
	return &integ{faux: faux, relais: r, h: r.Handler(), baux: gb, journal: journal}
}

func (x *integ) get(chemin, session string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", chemin, nil)
	req.Header.Set("X-Session", session)
	rec := httptest.NewRecorder()
	x.h.ServeHTTP(rec, req)
	return rec
}

func (x *integ) verifierSecrets(t *testing.T, contextes ...string) {
	t.Helper()
	tout := append([]string{x.journal.String()}, contextes...)
	for _, c := range tout {
		for _, s := range []string{utilisateurSecret, motDePasseSecret} {
			if strings.Contains(c, s) {
				t.Errorf("un identifiant fuit dans : %.200s", c)
			}
		}
	}
}

// CA5 : le scenario complet. Le fournisseur n'a qu'une connexion et garde la place de
// la chaine 1 pendant 300 ms ; la session zappe vers la chaine 2 : le relais PATIENTE
// (407 puis succes), la session garde UN seul bail, et le lecteur recoit un 200.
func TestZappingAvecLeFauxFournisseurUneSeuleConnexion(t *testing.T) {
	x := nouvelleIntegration(t, fauxiptv.Options{}, nil)
	x.faux.UneSeuleConnexion(300 * time.Millisecond)

	rec := x.get(PrefixeParDefaut+"/1001.m3u8", "s1")
	if rec.Code != 200 {
		t.Fatalf("premiere chaine : %d", rec.Code)
	}
	debut := time.Now()
	rec = x.get(PrefixeParDefaut+"/1002.m3u8", "s1") // zapping
	attente := time.Since(debut)
	if rec.Code != 200 {
		t.Fatalf("le zapping doit aboutir apres patience : %d %s", rec.Code, rec.Body.String())
	}
	if attente < 200*time.Millisecond {
		t.Errorf("le relais aurait du attendre la fin de la reservation du fournisseur (~300 ms) : %v", attente)
	}
	c := x.faux.Compteurs()
	if c.Refus407 < 1 || c.PlaylistsParChaine["1002"] < 2 {
		t.Errorf("le faux fournisseur doit avoir refuse (407) puis accepte : %+v", c)
	}
	if n := x.baux.Occupes("iptva"); n != 1 {
		t.Errorf("le zapping reutilise le bail : 1 place occupee attendue, %d", n)
	}
	if e := x.relais.Etat(); e.Patiences407 < 1 || e.NouveauxEssais != 0 {
		t.Errorf("patiences comptees, aucun essai consomme : %+v", e)
	}
	x.verifierSecrets(t, rec.Body.String())
}

// CA5 : si la patience est epuisee, 503 (et non un 502 trompeur), sans tempete de requetes.
func TestZappingPatienceEpuiseeRend503(t *testing.T) {
	x := nouvelleIntegration(t, fauxiptv.Options{}, func(o *Options) {
		o.Bail.PatienceMax = config.Duree(150 * time.Millisecond)
	})
	x.faux.UneSeuleConnexion(5 * time.Second)
	if rec := x.get(PrefixeParDefaut+"/1001.m3u8", "s1"); rec.Code != 200 {
		t.Fatalf("%d", rec.Code)
	}
	rec := x.get(PrefixeParDefaut+"/1002.m3u8", "s1")
	if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("503 + Retry-After attendus : %d", rec.Code)
	}
	if n := x.faux.Compteurs().PlaylistsParChaine["1002"]; n > 8 {
		t.Errorf("la patience retente toutes les bail.reessai, pas en boucle serree : %d requetes", n)
	}
}

// CA4 : le fournisseur n'a qu'une place ; une 2e SESSION attend la patience des baux
// puis recoit 503, et RIEN n'est demande au fournisseur pour elle.
func TestDeuxiemeSessionRefuseSansRequeteAmont(t *testing.T) {
	x := nouvelleIntegration(t, fauxiptv.Options{}, func(o *Options) {
		// Meme reglage de patience cote baux (le gestionnaire est deja construit avec 2 s) :
		// on en reconstruit un plus court pour que le test reste rapide.
		b := o.Bail
		b.PatienceMax = config.Duree(100 * time.Millisecond)
		g, _ := baux.Nouveau(b, map[string]int{"iptva": 1}, nil)
		o.Baux = g
	})
	if rec := x.get(PrefixeParDefaut+"/1001.m3u8", "s1"); rec.Code != 200 {
		t.Fatalf("%d", rec.Code)
	}
	x.faux.RazCompteurs()
	rec := x.get(PrefixeParDefaut+"/1001.m3u8", "s2")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("503 attendu pour la 2e session : %d", rec.Code)
	}
	if c := x.faux.Compteurs(); c.Playlists != 0 || c.Segments != 0 {
		t.Errorf("aucune requete amont sans bail : %+v", c)
	}
}

// US-06 CA3 de bout en bout : le faux fournisseur change le hash a chaque playlist ;
// les lignes communes a deux playlists successives sont IDENTIQUES.
func TestUrlsDeSegmentsStablesAvecLeFauxFournisseur(t *testing.T) {
	x := nouvelleIntegration(t, fauxiptv.Options{FenetreSegment: 4}, nil)
	un := segments(x.get(PrefixeParDefaut+"/1001.m3u8", "s1").Body.String())
	deux := segments(x.get(PrefixeParDefaut+"/1001.m3u8", "s1").Body.String())
	if len(un) != 4 || len(deux) != 4 {
		t.Fatalf("4 segments attendus : %d, %d", len(un), len(deux))
	}
	for i := 0; i < 3; i++ {
		if deux[i] != un[i+1] {
			t.Errorf("le segment %d de la 2e playlist doit avoir la meme URL que le %d de la 1re :\n%s\n%s", i, i+1, deux[i], un[i+1])
		}
	}
	if un[0] == un[1] {
		t.Error("deux segments differents ne partagent pas une URL")
	}
	// Et le segment est bien lu chez le fournisseur par l'URL stable.
	rec := x.get(deux[0], "s1")
	if rec.Code != 200 || rec.Body.Len() != 4096 || rec.Body.Bytes()[0] != 0x47 {
		t.Fatalf("segment : %d, %d octets", rec.Code, rec.Body.Len())
	}
}

// CA1/CA2 : pannes programmees du faux fournisseur absorbees par les nouveaux essais.
func TestPannesDePlaylistAbsorbeesParLesEssais(t *testing.T) {
	for nom, panne := range map[string]fauxiptv.Panne{
		"502":        {Cible: fauxiptv.CiblePlaylist, Fois: 2, Statut: 502},
		"458":        {Cible: fauxiptv.CiblePlaylist, Fois: 2, Statut: 458},
		"corps vide": {Cible: fauxiptv.CiblePlaylist, Fois: 2, CorpsVide: true},
		"405":        {Cible: fauxiptv.CiblePlaylist, Fois: 2, Statut: 405},
		"latence":    {Cible: fauxiptv.CiblePlaylist, Fois: 1, Latence: 3 * time.Second},
	} {
		t.Run(nom, func(t *testing.T) {
			x := nouvelleIntegration(t, fauxiptv.Options{}, func(o *Options) { o.Reseau.DelaiPlaylist = config.Duree(300 * time.Millisecond) })
			x.faux.Programmer(panne)
			rec := x.get(PrefixeParDefaut+"/1001.m3u8", "s1")
			if rec.Code != 200 || rec.Header().Get("X-Relais") == "secours" {
				t.Fatalf("%d (%s)", rec.Code, rec.Header().Get("X-Relais"))
			}
			if n := x.faux.Compteurs().Playlists; n < 2 {
				t.Errorf("au moins un nouvel essai attendu : %d requetes", n)
			}
			x.verifierSecrets(t, rec.Body.String())
		})
	}
}

// CA2 : panne totale apres une bonne playlist -> secours ; la playlist du faux fournisseur
// reste exploitable (segments encore lisibles apres coup si le fournisseur est revenu).
func TestSecoursAvecLeFauxFournisseur(t *testing.T) {
	x := nouvelleIntegration(t, fauxiptv.Options{}, nil)
	bonne := x.get(PrefixeParDefaut+"/1001.m3u8", "s1")
	x.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CiblePlaylist, Statut: 502})
	rec := x.get(PrefixeParDefaut+"/1001.m3u8", "s1")
	if rec.Code != 200 || rec.Header().Get("X-Relais") != "secours" || rec.Body.String() != bonne.Body.String() {
		t.Fatalf("secours attendu avec la derniere bonne playlist : %d %q", rec.Code, rec.Header().Get("X-Relais"))
	}
	x.verifierSecrets(t, rec.Body.String())
}

// Le fournisseur redirige la playlist vers un autre noeud : les URL relatives sont
// resolues contre l'adresse FINALE et les segments restent lisibles.
func TestPlaylistRedirigeeSuivie(t *testing.T) {
	x := nouvelleIntegration(t, fauxiptv.Options{RedirigerPlaylist: true}, nil)
	rec := x.get(PrefixeParDefaut+"/1001.m3u8", "s1")
	if rec.Code != 200 {
		t.Fatalf("%d", rec.Code)
	}
	seg := segments(rec.Body.String())
	if len(seg) == 0 {
		t.Fatal("aucun segment")
	}
	if r := x.get(seg[0], "s1"); r.Code != 200 || r.Body.Len() != 4096 {
		t.Errorf("segment apres redirection : %d", r.Code)
	}
}

// CA3 : un segment coupe en milieu de transfert n'est JAMAIS mis en cache ; la relecture
// va chercher le segment complet.
func TestSegmentCoupeNEstPasMisEnCache(t *testing.T) {
	x := nouvelleIntegration(t, fauxiptv.Options{}, nil)
	seg := segments(x.get(PrefixeParDefaut+"/1001.m3u8", "s1").Body.String())[0]
	srv := httptest.NewServer(x.h)
	defer srv.Close()
	lire := func() ([]byte, error) {
		req, _ := http.NewRequest("GET", srv.URL+seg, nil)
		req.Header.Set("X-Session", "s1")
		rep, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer rep.Body.Close()
		return io.ReadAll(rep.Body)
	}
	x.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleSegment, Fois: 1, Coupure: true})
	corps, err := lire()
	if err == nil && len(corps) == 4096 {
		t.Fatal("le premier transfert aurait du etre coupe")
	}
	if n, _ := x.relais.segments.taille(); n != 0 {
		t.Fatalf("un segment partiel ne doit pas entrer dans le cache : %d", n)
	}
	corps, err = lire()
	if err != nil || len(corps) != 4096 {
		t.Fatalf("relecture complete attendue : %d octets, %v", len(corps), err)
	}
	if n, _ := x.relais.segments.taille(); n != 1 {
		t.Errorf("le segment complet doit etre en cache : %d", n)
	}
}

// Un segment en panne (502) deux fois -> 502 franc apres 2 essais, rien ne fuit.
func TestSegmentEnPanneRend502SansFuite(t *testing.T) {
	x := nouvelleIntegration(t, fauxiptv.Options{}, nil)
	seg := segments(x.get(PrefixeParDefaut+"/1001.m3u8", "s1").Body.String())[0]
	x.faux.RazCompteurs()
	x.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleSegment, Statut: 502})
	rec := x.get(seg, "s1")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("502 attendu : %d", rec.Code)
	}
	if n := x.faux.Compteurs().Segments; n != 2 {
		t.Errorf("2 essais attendus : %d requetes", n)
	}
	x.verifierSecrets(t, rec.Body.String())
}

// La session arrete la lecture en cours de segment : le vrai bail est rendu tout de suite,
// une autre session peut alors lire sans attendre le TTL.
func TestFinDeLectureLibereLaPlaceDansLesVraisBaux(t *testing.T) {
	// Segment tres gros : il ne tient pas dans les tampons du reseau local, donc le
	// relais est encore en train de l'ecrire quand le lecteur part. Delais larges :
	// le faux fournisseur met du temps a fabriquer 32 Mo sous -race.
	x := nouvelleIntegration(t, fauxiptv.Options{TailleSegment: 32 << 20}, func(o *Options) {
		o.Reseau.DelaiSegment = config.Duree(30 * time.Second)
	})
	seg := segments(x.get(PrefixeParDefaut+"/1001.m3u8", "s1").Body.String())[0]
	srv := httptest.NewServer(x.h)
	defer srv.Close()
	ctx, stop := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+seg, nil)
	req.Header.Set("X-Session", "s1")
	rep, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadFull(rep.Body, make([]byte, 1024))
	if n := x.baux.Occupes("iptva"); n != 1 {
		t.Fatalf("pendant la lecture, 1 place occupee : %d", n)
	}
	stop()
	rep.Body.Close()
	attendreVrai(t, "la place doit etre rendue des que le client part", func() bool { return x.baux.Occupes("iptva") == 0 })
}
