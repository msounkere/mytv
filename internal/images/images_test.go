package images

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mytv/internal/config"
	"mytv/internal/hotes"
)

var secretTest = []byte("0123456789abcdef0123456789abcdef-secret")

// png1x1 : une vraie petite image (les tests ne dependent pas de son decodage).
var png1x1 = []byte("\x89PNG\r\n\x1a\n-pixel-")

// horlogeTest : temps controle par le test.
type horlogeTest struct {
	mu sync.Mutex
	t  time.Time
}

func (h *horlogeTest) maintenant() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.t }
func (h *horlogeTest) avancer(d time.Duration) {
	h.mu.Lock()
	h.t = h.t.Add(d)
	h.mu.Unlock()
}

// banc : un service branche sur un faux serveur d'images en boucle locale. 127.0.0.1
// n'est autorise QUE parce que le test le liste (hotes.Nouveau) : en production le
// filtre le refuse.
type banc struct {
	t        *testing.T
	srv      *httptest.Server
	s        *Service
	horloge  *horlogeTest
	requetes atomic.Int32
	dernier  atomic.Pointer[http.Request]
	handler  atomic.Pointer[http.HandlerFunc]
	dossier  string
}

func cfgTest(dossier string) config.Images {
	c := config.Defauts().Images
	c.CacheDossier = dossier
	c.CacheOctets = 1 << 20
	c.TailleMax = 64 << 10
	c.Delai = config.Duree(3 * time.Second)
	c.CacheNegatif = config.Duree(time.Minute)
	return c
}

func nouveauBanc(t *testing.T, modif func(*config.Images), autorises ...string) *banc {
	t.Helper()
	b := &banc{t: t, dossier: t.TempDir() + "/cache", horloge: &horlogeTest{t: time.Unix(1_700_000_000, 0)}}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.requetes.Add(1)
		b.dernier.Store(r)
		if h := b.handler.Load(); h != nil {
			(*h)(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png1x1)
	}))
	t.Cleanup(b.srv.Close)
	if autorises == nil {
		autorises = []string{"127.0.0.1"}
	}
	b.reconstruire(modif, autorises)
	return b
}

func (b *banc) reconstruire(modif func(*config.Images), autorises []string) {
	b.t.Helper()
	c := cfgTest(b.dossier)
	if modif != nil {
		modif(&c)
	}
	f, err := hotes.Nouveau("http://fournisseur.test", autorises, nil)
	if err != nil {
		b.t.Fatal(err)
	}
	s, err := Nouveau(Options{Config: c, Secret: secretTest, Filtre: f, UserAgent: "AgentFournisseur/1.0",
		Fournisseurs: []Origine{{ID: "iptva", Base: b.srv.URL}}, Horloge: b.horloge.maintenant})
	if err != nil {
		b.t.Fatal(err)
	}
	b.s = s
}

func (b *banc) handle(h http.HandlerFunc) { b.handler.Store(&h) }

func (b *banc) jeton(chemin string) string {
	b.t.Helper()
	return strings.TrimPrefix(b.s.Reecrire(b.srv.URL+chemin), Chemin)
}

func (b *banc) lire(jeton string) (string, []byte, error) {
	img, err := b.s.Obtenir(context.Background(), jeton)
	if err != nil {
		return "", nil, err
	}
	defer img.Fermer()
	corps, _ := io.ReadAll(img.Contenu)
	return img.Type, corps, nil
}

// CA7 : premiere requete -> amont ; deuxieme -> cache (compteur du faux serveur).
func TestPremiereRequeteVaAAmontPuisLeCacheRepond(t *testing.T) {
	b := nouveauBanc(t, nil)
	j := b.jeton("/picons/tf1.png")
	for i := 0; i < 3; i++ {
		typ, corps, err := b.lire(j)
		if err != nil || typ != "image/png" || string(corps) != string(png1x1) {
			t.Fatalf("lecture %d : %q %q %v", i, typ, corps, err)
		}
	}
	if n := b.requetes.Load(); n != 1 {
		t.Fatalf("le fournisseur doit etre interroge une seule fois, %d requetes", n)
	}
}

// CA4 : le cache survit a un redemarrage (index reconstruit depuis le disque).
func TestCacheSurvitAuRedemarrage(t *testing.T) {
	b := nouveauBanc(t, nil)
	j := b.jeton("/picons/a.png")
	if _, _, err := b.lire(j); err != nil {
		t.Fatal(err)
	}
	b.reconstruire(nil, []string{"127.0.0.1"})
	if _, corps, err := b.lire(j); err != nil || string(corps) != string(png1x1) {
		t.Fatalf("apres redemarrage : %q %v", corps, err)
	}
	if n := b.requetes.Load(); n != 1 {
		t.Fatalf("le cache disque aurait du repondre : %d requetes", n)
	}
}

// CA2 : un jeton modifie, tronque, d'un autre secret ou d'un autre usage est refuse.
func TestJetonFalsifieOuEtrangerRefuse(t *testing.T) {
	b := nouveauBanc(t, nil)
	j := b.jeton("/picons/a.png")
	altere := []byte(j)
	altere[len(altere)/2] ^= 1 // un bit change dans la charge chiffree
	autre := nouveauBanc(t, nil)
	autre.s.sceau, _ = newSceauAutre()
	for nom, jeton := range map[string]string{
		"altere":        string(altere),
		"tronque":       j[:len(j)-4],
		"vide":          "",
		"aleatoire":     "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"url_en_clair":  "http://127.0.0.1/picons/a.png",
		"autre_secret":  strings.TrimPrefix(autre.s.Reecrire(b.srv.URL+"/picons/a.png"), Chemin),
		"point_double":  j + ".a.b",
		"hors_alphabet": "!!!!",
	} {
		if _, _, err := b.lire(jeton); !errors.Is(err, ErrJeton) {
			t.Errorf("%s : attendu ErrJeton, obtenu %v", nom, err)
		}
	}
	if b.requetes.Load() != 0 {
		t.Fatal("un jeton refuse ne doit jamais atteindre le fournisseur")
	}
}

// CA2 : un jeton scelle pour les SEGMENTS HLS n'ouvre pas une image (domaines separes).
func TestJetonDUnAutreUsageRefuse(t *testing.T) {
	b := nouveauBanc(t, nil)
	segment, _ := newSceauSegment()
	jeton := segment.Sceller("image", "\x00"+b.srv.URL+"/x.png")
	if _, _, err := b.lire(jeton); !errors.Is(err, ErrJeton) {
		t.Fatalf("attendu ErrJeton, obtenu %v", err)
	}
}

// CA3 : une page qui n'est pas une image (portail captif, erreur HTML) -> ErrAmont.
func TestReponseNonImageRefusee(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.handle(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>portail</html>"))
	})
	if _, _, err := b.lire(b.jeton("/x.png")); !errors.Is(err, ErrAmont) {
		t.Fatalf("attendu ErrAmont, obtenu %v", err)
	}
	if mimes := []string{"", "image", "application/octet-stream"}; true {
		for _, m := range mimes {
			b.handle(func(w http.ResponseWriter, _ *http.Request) {
				if m != "" {
					w.Header().Set("Content-Type", m)
				}
				w.Header()["Content-Type"] = nilSiVide(m)
				_, _ = w.Write(png1x1)
			})
			if _, _, err := b.lire(b.jeton("/m" + m + ".png")); !errors.Is(err, ErrAmont) {
				t.Errorf("type %q : attendu ErrAmont, obtenu %v", m, err)
			}
		}
	}
}

func nilSiVide(m string) []string {
	if m == "" {
		return []string{""}
	}
	return []string{m}
}

// CA3 : taille maximale, annoncee (Content-Length) ou non (transfert par morceaux).
func TestImageTropGrosseRefusee(t *testing.T) {
	b := nouveauBanc(t, func(c *config.Images) { c.TailleMax = 1000 })
	gros := make([]byte, 5000)
	b.handle(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Length", "5000")
		_, _ = w.Write(gros)
	})
	if _, _, err := b.lire(b.jeton("/annonce.png")); !errors.Is(err, ErrAmont) {
		t.Errorf("Content-Length trop grand : %v", err)
	}
	b.handle(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(gros[:500])
		w.(http.Flusher).Flush() // force le transfert par morceaux : pas de Content-Length
		_, _ = w.Write(gros)
	})
	if _, _, err := b.lire(b.jeton("/morceaux.png")); !errors.Is(err, ErrAmont) {
		t.Errorf("transfert par morceaux trop grand : %v", err)
	}
	if b.s.Octets() != 0 {
		t.Error("une image trop grosse ne doit pas etre mise en cache")
	}
	b.handle(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(gros[:1000]) // pile a la limite : accepte
	})
	if _, corps, err := b.lire(b.jeton("/limite.png")); err != nil || len(corps) != 1000 {
		t.Errorf("a la limite : %d octets, %v", len(corps), err)
	}
}

// CA3 : un hote prive (non liste) n'est jamais contacte, meme avec un jeton authentique.
func TestHoteNonAutoriseJamaisContacte(t *testing.T) {
	b := nouveauBanc(t, nil, "10.9.9.9") // 127.0.0.1 n'est PAS autorise ici
	if _, _, err := b.lire(b.jeton("/picons/a.png")); !errors.Is(err, ErrRefuse) {
		t.Fatalf("attendu ErrRefuse, obtenu %v", err)
	}
	if n := b.requetes.Load(); n != 0 {
		t.Fatalf("l'hote refuse a recu %d requetes", n)
	}
}

// CA3 : une redirection vers un hote interdit est reverifiee et refusee.
func TestRedirectionVersHoteInterditRefusee(t *testing.T) {
	interne := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png1x1)
	}))
	defer interne.Close()
	var vus atomic.Int32
	interne.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { vus.Add(1) })
	// Le fournisseur de test est atteignable par son NOM (« localhost » autorise) ; la
	// redirection vise l'adresse 127.0.0.1, non listee.
	b := nouveauBanc(t, nil, "localhost")
	hote := strings.Replace(b.srv.URL, "127.0.0.1", "localhost", 1)
	b.handle(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, interne.URL+"/secret.png", http.StatusFound)
	})
	j := strings.TrimPrefix(b.s.Reecrire(hote+"/a.png"), Chemin)
	if _, _, err := b.lire(j); !errors.Is(err, ErrAmont) {
		t.Fatalf("attendu ErrAmont, obtenu %v", err)
	}
	if vus.Load() != 0 {
		t.Fatal("la redirection interdite a ete suivie")
	}
}

// CA3 : une redirection vers un hote autorise est suivie, sans Referer.
func TestRedirectionAutoriseeSuivieSansReferer(t *testing.T) {
	b := nouveauBanc(t, nil)
	var referer atomic.Value
	b.handle(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/vers" {
			http.Redirect(w, r, "/final.png", http.StatusFound)
			return
		}
		referer.Store(r.Header.Get("Referer"))
		w.Header().Set("Content-Type", "image/webp")
		_, _ = w.Write(png1x1)
	})
	typ, _, err := b.lire(b.jeton("/vers"))
	if err != nil || typ != "image/webp" {
		t.Fatalf("%q %v", typ, err)
	}
	if r, _ := referer.Load().(string); r != "" {
		t.Errorf("Referer envoye : %q", r)
	}
}

// CA3 : ni cookie ni identifiant n'est envoye ; l'agent configure l'est.
func TestRequeteAmontAnonymeAvecLAgentDuFournisseur(t *testing.T) {
	b := nouveauBanc(t, nil)
	if _, _, err := b.lire(b.jeton("/picons/a.png")); err != nil {
		t.Fatal(err)
	}
	r := b.dernier.Load()
	if r.UserAgent() != "AgentFournisseur/1.0" {
		t.Errorf("agent : %q", r.UserAgent())
	}
	for _, h := range []string{"Cookie", "Authorization", "Referer", "X-Forwarded-For"} {
		if r.Header.Get(h) != "" {
			t.Errorf("en-tete %s envoye : %q", h, r.Header.Get(h))
		}
	}
}

// CA4 : image absente (404) ou en erreur (500) -> erreur typee, memorisee un court
// moment pour ne pas marteler le fournisseur, puis redemandee.
func TestCacheNegatifEvitedeMartelerLeFournisseur(t *testing.T) {
	b := nouveauBanc(t, nil)
	statut := atomic.Int32{}
	statut.Store(404)
	b.handle(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(int(statut.Load())) })
	j := b.jeton("/absent.png")
	for i := 0; i < 5; i++ {
		if _, _, err := b.lire(j); !errors.Is(err, ErrIntrouvable) {
			t.Fatalf("attendu ErrIntrouvable, obtenu %v", err)
		}
	}
	if n := b.requetes.Load(); n != 1 {
		t.Fatalf("404 repete : %d requetes amont au lieu d'une", n)
	}
	statut.Store(500)
	b.horloge.avancer(2 * time.Minute) // le cache negatif a expire
	if _, _, err := b.lire(j); !errors.Is(err, ErrAmont) {
		t.Fatalf("500 : attendu ErrAmont, obtenu %v", err)
	}
	if _, _, err := b.lire(j); !errors.Is(err, ErrAmont) {
		t.Fatalf("500 repete : %v", err)
	}
	if n := b.requetes.Load(); n != 2 {
		t.Fatalf("apres expiration : %d requetes, attendu 2", n)
	}
	// Le fournisseur se retablit : apres expiration l'image apparait.
	b.handle(nil)
	b.handler.Store(nil)
	b.horloge.avancer(2 * time.Minute)
	if _, corps, err := b.lire(j); err != nil || len(corps) == 0 {
		t.Fatalf("apres retablissement : %v", err)
	}
}

// CA4 : le cache negatif est borne en nombre d'entrees.
func TestCacheNegatifBorne(t *testing.T) {
	b := nouveauBanc(t, func(c *config.Images) { c.CacheNegatifMax = 10 })
	b.handle(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) })
	for i := 0; i < 50; i++ {
		_, _, _ = b.lire(b.jeton("/n" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + ".png"))
	}
	if n := b.s.neg.taille(); n > 10 {
		t.Fatalf("cache negatif non borne : %d entrees", n)
	}
}

// CA4 : singleflight -- vingt demandes simultanees d'une meme image = une requete amont.
func TestDemandesSimultanéesFusionnees(t *testing.T) {
	b := nouveauBanc(t, nil)
	porte := make(chan struct{})
	b.handle(func(w http.ResponseWriter, _ *http.Request) {
		<-porte
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png1x1)
	})
	j := b.jeton("/vite.png")
	var wg sync.WaitGroup
	erreurs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := b.lire(j)
			erreurs <- err
		}()
	}
	time.Sleep(150 * time.Millisecond) // laisse tout le monde s'inscrire
	close(porte)
	wg.Wait()
	close(erreurs)
	for err := range erreurs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if n := b.requetes.Load(); n != 1 {
		t.Fatalf("%d requetes amont pour une seule image", n)
	}
}

// CA4 : un client qui s'en va n'annule pas le telechargement des autres.
func TestAnnulationDUnClientNeCasseriePasLesAutres(t *testing.T) {
	b := nouveauBanc(t, nil)
	porte := make(chan struct{})
	b.handle(func(w http.ResponseWriter, _ *http.Request) {
		<-porte
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png1x1)
	})
	j := b.jeton("/lent.png")
	ctx, annuler := context.WithCancel(context.Background())
	premier := make(chan error, 1)
	go func() { _, err := b.s.Obtenir(ctx, j); premier <- err }()
	time.Sleep(50 * time.Millisecond)
	second := make(chan error, 1)
	go func() { _, _, err := b.lire(j); second <- err }()
	time.Sleep(50 * time.Millisecond)
	annuler()
	if err := <-premier; !errors.Is(err, context.Canceled) {
		t.Fatalf("premier : %v", err)
	}
	close(porte)
	if err := <-second; err != nil {
		t.Fatalf("le second client a recu une erreur : %v", err)
	}
}

// CA5 : la concurrence amont est bornee ; une demande de plus attend sa place puis
// echoue en ErrSature plutot que de saturer le fournisseur.
func TestConcurrenceAmontBornee(t *testing.T) {
	b := nouveauBanc(t, func(c *config.Images) { c.ConcurrenceMax = 2; c.Delai = config.Duree(600 * time.Millisecond) })
	var enCours, maxVu atomic.Int32
	b.handle(func(w http.ResponseWriter, _ *http.Request) {
		n := enCours.Add(1)
		for {
			m := maxVu.Load()
			if n <= m || maxVu.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(150 * time.Millisecond)
		enCours.Add(-1)
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png1x1)
	})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, _ = b.lire(b.jeton("/c" + string(rune('a'+i)) + ".png"))
		}(i)
	}
	wg.Wait()
	if m := maxVu.Load(); m > 2 {
		t.Fatalf("%d telechargements simultanes, borne 2", m)
	}
	if maxVu.Load() < 2 {
		t.Fatal("le test ne sollicite pas la concurrence")
	}
}

func TestSaturationRendErrSatureSansCacheNegatif(t *testing.T) {
	b := nouveauBanc(t, func(c *config.Images) { c.ConcurrenceMax = 1; c.Delai = config.Duree(300 * time.Millisecond) })
	b.handle(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(250 * time.Millisecond) // occupe la seule place
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png1x1)
	})
	res := make(chan error, 3)
	for i := 0; i < 3; i++ {
		go func(i int) {
			_, _, err := b.lire(b.jeton("/s" + string(rune('a'+i)) + ".png"))
			res <- err
		}(i)
		time.Sleep(20 * time.Millisecond)
	}
	var sature, ok int
	for i := 0; i < 3; i++ {
		switch err := <-res; {
		case err == nil:
			ok++
		case errors.Is(err, ErrSature):
			sature++
		default:
			t.Errorf("erreur inattendue : %v", err)
		}
	}
	if sature == 0 || ok == 0 {
		t.Fatalf("attendu au moins une image servie et une saturation : ok=%d saturation=%d", ok, sature)
	}
	if b.s.neg.taille() != 0 {
		t.Fatal("une saturation locale ne doit pas etre memorisee comme une erreur du fournisseur")
	}
}

// CA4 : le cache disque ne depasse jamais sa borne, evince les plus anciens, et la
// lecture d'une image la rajeunit.
func TestCacheDisqueBorneEnOctetsAvecEvictionLRU(t *testing.T) {
	const taille = 1000
	corps := make([]byte, taille)
	b := nouveauBanc(t, func(c *config.Images) { c.CacheOctets = 3500; c.TailleMax = 2000 })
	b.handle(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(corps)
	})
	a, bb, c, d := b.jeton("/a.png"), b.jeton("/b.png"), b.jeton("/c.png"), b.jeton("/d.png")
	for _, j := range []string{a, bb, c} {
		if _, _, err := b.lire(j); err != nil {
			t.Fatal(err)
		}
	}
	_, _, _ = b.lire(a) // a redevient le plus recent ; b est le plus ancien
	if _, _, err := b.lire(d); err != nil {
		t.Fatal(err)
	}
	if o := b.s.Octets(); o > 3500 {
		t.Fatalf("cache a %d octets, borne 3500", o)
	}
	avant := b.requetes.Load()
	_, _, _ = b.lire(a)
	_, _, _ = b.lire(c)
	_, _, _ = b.lire(d)
	if b.requetes.Load() != avant {
		t.Fatal("a, c et d devaient etre en cache")
	}
	_, _, _ = b.lire(bb)
	if b.requetes.Load() != avant+1 {
		t.Fatal("b (le plus ancien) devait avoir ete evince")
	}
}

// CA4 : une image perimee est retelechargee ; si le fournisseur est en panne, on sert
// l'ancienne plutot que rien.
func TestImagePerimeeRetelechargeeOuServieEnSecours(t *testing.T) {
	b := nouveauBanc(t, func(c *config.Images) { c.CacheValidite = config.Duree(time.Hour) })
	j := b.jeton("/a.png")
	img1, _ := b.s.Obtenir(context.Background(), j)
	etag1 := img1.ETag
	img1.Fermer()
	b.horloge.avancer(2 * time.Hour)
	img2, err := b.s.Obtenir(context.Background(), j)
	if err != nil || b.requetes.Load() != 2 || img2.ETag == etag1 {
		t.Fatalf("retelechargement : %v requetes=%d etag=%s/%s", err, b.requetes.Load(), etag1, img2.ETag)
	}
	img2.Fermer()
	b.horloge.avancer(2 * time.Hour)
	b.handle(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(503) })
	if _, corps, err := b.lire(j); err != nil || string(corps) != string(png1x1) {
		t.Fatalf("secours : %q %v", corps, err)
	}
	if _, corps, err := b.lire(j); err != nil || string(corps) != string(png1x1) {
		t.Fatalf("secours (cache negatif actif) : %q %v", corps, err)
	}
}

// CA1 : formes d'URL reecrites ou non.
func TestReecritureDesURL(t *testing.T) {
	b := nouveauBanc(t, nil)
	s := b.s
	if s.Reecrire("") != "" || s.Reecrire("   ") != "" {
		t.Error("une chaine vide reste vide")
	}
	if got := s.Reecrire("data:image/gif;base64,R0lGOD"); got != "data:image/gif;base64,R0lGOD" {
		t.Errorf("data: doit rester tel quel : %q", got)
	}
	for _, mauvais := range []string{"ftp://h/x.png", "/picons/x.png", "//h/x.png", "file:///etc/passwd", "javascript:alert(1)",
		"http://user:mdp@h.test/x.png", "http://", "n'importe quoi", "data:text/html,<script>"} {
		if got := s.Reecrire(mauvais); got != "" {
			t.Errorf("%q doit etre ecarte, obtenu %q", mauvais, got)
		}
	}
	for _, u := range []string{"http://203.0.113.9/picons/a.png", "https://image.tmdb.org/t/p/w500/abc.jpg?x=1#f",
		b.srv.URL + "/picons/a b.png", "HTTP://Exemple.TEST:8080/Majuscules.PNG", b.srv.URL} {
		r := s.Reecrire(u)
		if !strings.HasPrefix(r, Chemin) {
			t.Fatalf("%q -> %q", u, r)
		}
		if strings.Contains(r, "203.0.113.9") || strings.Contains(r, "tmdb") || strings.Contains(r, "picons") {
			t.Errorf("le jeton laisse lire l'URL : %q", r)
		}
		back, err := s.resoudre(strings.TrimPrefix(r, Chemin))
		attendu := u
		if u == "HTTP://Exemple.TEST:8080/Majuscules.PNG" {
			attendu = "HTTP://Exemple.TEST:8080/Majuscules.PNG" // chemin intact ; hote conserve tel quel
		}
		if err != nil || back != attendu {
			t.Errorf("aller-retour de %q : %q %v", u, back, err)
		}
		if s.Reecrire(u) != r {
			t.Errorf("le jeton doit etre stable pour %q", u)
		}
	}
}

// CA6 : le jeton reste compact (l'export en bloc en porte des dizaines de milliers).
func TestJetonCompactPourLesURLUsuelles(t *testing.T) {
	b := nouveauBanc(t, nil)
	picon := b.s.Reecrire(b.srv.URL + "/picons/tf1-hd-france.png")
	tmdb := b.s.Reecrire("https://image.tmdb.org/t/p/w600_and_h900_bestv2/kqjL17yufvn9OVLyXYpvtyrFfak.jpg")
	complet := b.s.Reecrire("http://line.fournisseur-exemple.test/picons/tf1-hd-france.png")
	if len(picon) >= len(complet) || len(picon) > len(Chemin)+85 {
		t.Errorf("picon du fournisseur : %d caracteres (complet %d)", len(picon), len(complet))
	}
	if len(tmdb) > len(Chemin)+120 {
		t.Errorf("affiche TMDB : %d caracteres", len(tmdb))
	}
}

// Un jeton emis pour un fournisseur retire de la configuration est refuse.
func TestJetonDUnFournisseurRetireRefuse(t *testing.T) {
	b := nouveauBanc(t, nil)
	j := b.jeton("/a.png")
	delete(b.s.base, "iptva")
	if _, _, err := b.lire(j); !errors.Is(err, ErrJeton) {
		t.Fatalf("attendu ErrJeton, obtenu %v", err)
	}
}

// Le dossier du cache est prive (droits 700) et les restes d'ecriture sont nettoyes.
func TestDossierDuCachePriveEtRestesNettoyes(t *testing.T) {
	b := nouveauBanc(t, nil)
	if _, _, err := b.lire(b.jeton("/a.png")); err != nil {
		t.Fatal(err)
	}
	info, err := osStat(b.dossier)
	if err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("droits du dossier : %v %v", info, err)
	}
	if err := osEcrire(b.dossier+"/.tmp-reste", "x"); err != nil {
		t.Fatal(err)
	}
	b.reconstruire(nil, []string{"127.0.0.1"})
	if _, err := osStat(b.dossier + "/.tmp-reste"); err == nil {
		t.Fatal("reste d'ecriture non supprime")
	}
}

// Aucun secret ni URL dans les erreurs rendues.
func TestErreursNeCitentNiURLNiSecret(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.handle(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(500) })
	_, _, err := b.lire(b.jeton("/picons/prive.png"))
	if err == nil || strings.Contains(err.Error(), "prive") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("erreur trop bavarde : %v", err)
	}
}
