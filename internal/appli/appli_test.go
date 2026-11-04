package appli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mytv/internal/comptes"
	"mytv/internal/config"
	"mytv/internal/sauvegarde"
	"mytv/internal/testutil/fauxiptv"
)

const (
	secretSignature = "0123456789abcdef0123456789abcdef-SIGNATURE"
	motDePasseUser  = "mot de passe de test tres solide"
	utilisateurIPTV = "iptv-utilisateur-secret"
	motDePasseIPTV  = "iptv-mot-de-passe-secret"
)

// tampon : journal capture, sur lequel on cherche les secrets.
type tampon struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (t *tampon) Write(p []byte) (int, error) { t.mu.Lock(); defer t.mu.Unlock(); return t.b.Write(p) }
func (t *tampon) String() string              { t.mu.Lock(); defer t.mu.Unlock(); return t.b.String() }

// confTest ecrit une configuration YAML complete (secrets dans l'environnement).
func confTest(t *testing.T, base string, extra string) *config.Config {
	t.Helper()
	t.Setenv("MYTV_T_SIGNATURE", secretSignature)
	t.Setenv("MYTV_T_IPTV_U", utilisateurIPTV)
	t.Setenv("MYTV_T_IPTV_P", motDePasseIPTV)
	yaml := fmt.Sprintf(`
serveur: { ecoute: "127.0.0.1:0", secret_signature: { env: MYTV_T_SIGNATURE }, delai_arret: 5s }
stockage: { fichier: %q }
fournisseurs:
  - id: iptva
    type: xtream
    base: %q
    utilisateur: { env: MYTV_T_IPTV_U }
    mot_de_passe: { env: MYTV_T_IPTV_P }
    connexions_max: 1
    rafraichissement: { au_demarrage: false }
bail: { ttl: 90s, patience_max: 5s, reessai: 50ms }
comptes: { cookie_securise: false }
journal: { niveau: debug }
diagnostic: { actif: true, dossier: %q }
images: { cache_dossier: %q }
preparation: { dossier: %q }
%s
`, filepath.Join(t.TempDir(), "mytv.db"), base, filepath.Join(t.TempDir(), "diag"), filepath.Join(t.TempDir(), "images"), filepath.Join(t.TempDir(), "prepares"), extra)
	c, err := config.Lire(strings.NewReader(yaml))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

type demarre struct {
	a       *Appli
	url     string
	arreter context.CancelFunc
	fini    chan error
	journal *tampon
	client  *http.Client
}

func lancer(t *testing.T, cfg *config.Config) *demarre {
	t.Helper()
	return lancerAvec(t, cfg, Options{})
}

func lancerAvec(t *testing.T, cfg *config.Config, opt Options) *demarre {
	t.Helper()
	j := &tampon{}
	ctx, arreter := context.WithCancel(context.Background())
	opt.Journal = NouveauJournal2(j)
	a, err := Construire(ctx, cfg, opt)
	if err != nil {
		arreter()
		t.Fatal(err)
	}
	d := &demarre{a: a, url: "http://" + a.Adresse(), arreter: arreter, fini: make(chan error, 1), journal: j}
	go func() { d.fini <- a.Lancer(ctx) }()
	jar, _ := cookiejar.New(nil)
	d.client = &http.Client{Jar: jar, Timeout: 10 * time.Second}
	t.Cleanup(func() { d.arret(t) })
	return d
}

// arret demande l'arret et attend que Lancer rende la main (idempotent).
func (d *demarre) arret(t *testing.T) error {
	d.arreter()
	select {
	case err := <-d.fini:
		d.fini <- err
		return err
	case <-time.After(15 * time.Second):
		t.Fatal("Lancer ne rend pas la main apres l'annulation")
		return nil
	}
}

func (d *demarre) req(t *testing.T, methode, chemin, corps string) (int, []byte) {
	t.Helper()
	var r io.Reader
	if corps != "" {
		r = strings.NewReader(corps)
	}
	req, _ := http.NewRequest(methode, d.url+chemin, r)
	if corps != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rep, err := d.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer rep.Body.Close()
	b, _ := io.ReadAll(rep.Body)
	return rep.StatusCode, b
}

func (d *demarre) connecter(t *testing.T, nom string, admin bool) {
	t.Helper()
	if _, err := d.a.Comptes().CreerUtilisateur(context.Background(), nom, motDePasseUser, admin); err != nil {
		t.Fatal(err)
	}
	if code, corps := d.req(t, "POST", "/api/v1/connexion", fmt.Sprintf(`{"nom":%q,"mot_de_passe":%q}`, nom, motDePasseUser)); code != 200 {
		t.Fatalf("connexion : %d %s", code, corps)
	}
}

// ---------------------------------------------------------------- CA1 / CA6 : verification

func TestVerifierRassembleToutesLesErreurs(t *testing.T) {
	t.Setenv("MYTV_T_SIGNATURE", "trop court")
	cfg, err := config.Lire(strings.NewReader(`
serveur: { secret_signature: { env: MYTV_T_SIGNATURE }, ui_dossier: /inexistant/dist }
fournisseurs:
  - { id: a, type: xtream, base: "http://f.test", utilisateur: { env: MYTV_T_ABSENT_U }, mot_de_passe: { env: MYTV_T_ABSENT_P } }
`))
	if err != nil {
		t.Fatal(err)
	}
	err = Verifier(cfg)
	if err == nil {
		t.Fatal("configuration invalide acceptee")
	}
	for _, m := range []string{"secret_signature fait", "MYTV_T_ABSENT_U", "MYTV_T_ABSENT_P", "ui_dossier"} {
		if !strings.Contains(err.Error(), m) {
			t.Errorf("erreur %q absente de :\n%v", m, err)
		}
	}
	if strings.Contains(err.Error(), "trop court") {
		t.Error("la valeur du secret ne doit pas figurer dans l'erreur")
	}
	if _, err := Construire(context.Background(), cfg, Options{}); err == nil {
		t.Error("Construire doit refuser la meme configuration")
	}
}

// CA6 : un secret de signature trop court est refuse, de la longueur minimale il passe.
func TestSecretDeSignatureTropCourtRefuse(t *testing.T) {
	f := fauxiptv.Nouveau(t, fauxiptv.Options{Utilisateur: utilisateurIPTV, MotDePasse: motDePasseIPTV})
	cfg := confTest(t, f.URL(), "")
	t.Setenv("MYTV_T_SIGNATURE", strings.Repeat("x", SecretSigneMin-1))
	if err := Verifier(cfg); err == nil || !strings.Contains(err.Error(), "secret_signature") {
		t.Fatalf("secret court accepte : %v", err)
	}
	t.Setenv("MYTV_T_SIGNATURE", strings.Repeat("x", SecretSigneMin))
	if err := Verifier(cfg); err != nil {
		t.Fatalf("secret de longueur minimale refuse : %v", err)
	}
}

// ---------------------------------------------------------------- CA3 : bout en bout

var reLigneSegment = regexp.MustCompile(`(?m)^(/api/v1/live/\S+)$`)

func TestBoutEnBoutConnexionChainesDirectZapping(t *testing.T) {
	faux := fauxiptv.Nouveau(t, fauxiptv.Options{Utilisateur: utilisateurIPTV, MotDePasse: motDePasseIPTV,
		Catalogue: fauxiptv.CatalogueExemple(3, 2, 1)})
	faux.UneSeuleConnexion(time.Second) // la regle du vrai fournisseur : une chaine a la fois
	d := lancer(t, confTest(t, faux.URL(), ""))

	// Refus par defaut avant connexion.
	if code, _ := d.req(t, "GET", "/api/v1/chaines", ""); code != 401 {
		t.Fatalf("sans session : %d", code)
	}
	d.connecter(t, "alice", false)
	if err := d.a.catalogue.SynchroniserTous(context.Background()); err != nil { // le faux fournisseur ne publie pas de guide
		t.Fatal(err)
	}

	// Liste de chaines.
	code, corps := d.req(t, "GET", "/api/v1/chaines", "")
	var liste struct {
		Elements []struct{ ID, Nom, Lecture string }
	}
	json.Unmarshal(corps, &liste)
	if code != 200 || len(liste.Elements) != 3 {
		t.Fatalf("chaines : %d %s", code, corps)
	}
	if strings.Contains(string(corps), utilisateurIPTV) || strings.Contains(string(corps), motDePasseIPTV) || strings.Contains(string(corps), faux.URL()) {
		t.Fatal("la reponse expose le fournisseur")
	}

	// Lecture du direct : playlist puis segment.
	lire := func(chemin string) (int, string) {
		c, b := d.req(t, "GET", chemin, "")
		return c, string(b)
	}
	code, pl := lire(liste.Elements[0].Lecture)
	if code != 200 || !strings.Contains(pl, "#EXTM3U") {
		t.Fatalf("playlist : %d %s", code, pl)
	}
	segs := reLigneSegment.FindAllString(pl, -1)
	if len(segs) == 0 {
		t.Fatalf("aucun segment dans :\n%s", pl)
	}
	if code, seg := lire(segs[0]); code != 200 || len(seg) != 4096 {
		t.Fatalf("segment : %d, %d octets", code, len(seg))
	}
	if strings.Contains(pl, faux.URL()) || strings.Contains(pl, utilisateurIPTV) {
		t.Fatal("la playlist reecrite expose le fournisseur")
	}

	// Zapping : une AUTRE chaine avec la meme session reutilise le bail (sinon 407 du fournisseur).
	faux.RazCompteurs()
	if code, pl2 := lire(liste.Elements[1].Lecture); code != 200 || !strings.Contains(pl2, "#EXTM3U") {
		t.Fatalf("zapping : %d %s", code, pl2)
	}
	if n := d.a.baux.Occupes("iptva"); n != 1 {
		t.Fatalf("un seul bail attendu apres le zapping : %d", n)
	}

	// /etat (live) : refuse a un utilisateur ordinaire.
	if code, _ := lire("/api/v1/live/etat"); code != 403 {
		t.Fatalf("/etat pour un ordinaire : %d", code)
	}

	// Deconnexion : la place de connexion est rendue tout de suite.
	if code, _ := d.req(t, "POST", "/api/v1/deconnexion", ""); code != 204 {
		t.Fatalf("deconnexion : %d", code)
	}
	if n := d.a.baux.Occupes("iptva"); n != 0 {
		t.Fatalf("le bail doit etre rendu a la deconnexion : %d", n)
	}
	if code, _ := lire(liste.Elements[0].Lecture); code != 401 {
		t.Fatalf("lecture apres deconnexion : %d", code)
	}

	// CA5 : aucun secret dans le journal.
	d.arret(t)
	j := d.journal.String()
	if !strings.Contains(j, `"msg":"demarre"`) {
		t.Errorf("journal structure attendu :\n%s", j)
	}
	for _, s := range []string{secretSignature, utilisateurIPTV, motDePasseIPTV, motDePasseUser} {
		if strings.Contains(j, s) {
			t.Fatalf("secret %q dans le journal", s)
		}
	}
	for _, l := range strings.Split(strings.TrimSpace(j), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(l), &m) != nil {
			t.Fatalf("ligne de journal non JSON : %s", l)
		}
	}
}

func TestAdministrateurLitEtat(t *testing.T) {
	faux := fauxiptv.Nouveau(t, fauxiptv.Options{Utilisateur: utilisateurIPTV, MotDePasse: motDePasseIPTV})
	d := lancer(t, confTest(t, faux.URL(), ""))
	d.connecter(t, "root", true)
	if code, corps := d.req(t, "GET", "/api/v1/live/etat", ""); code != 200 || !strings.Contains(string(corps), "playlists_servies") {
		t.Fatalf("%d %s", code, corps)
	}
}

// ---------------------------------------------------------------- CA4 : sante

func TestSanteSansAuthentificationNiDetail(t *testing.T) {
	faux := fauxiptv.Nouveau(t, fauxiptv.Options{Utilisateur: utilisateurIPTV, MotDePasse: motDePasseIPTV})
	d := lancer(t, confTest(t, faux.URL(), ""))
	for _, chemin := range []string{"/sante", "/api/v1/sante"} {
		code, corps := d.req(t, "GET", chemin, "")
		if code != 200 || strings.TrimSpace(string(corps)) != `{"statut":"ok"}` {
			t.Errorf("%s : %d %s", chemin, code, corps)
		}
	}
	if code, _ := d.req(t, "GET", "/", ""); code != 404 {
		t.Errorf("racine sans interface : %d", code)
	}
}

// ---------------------------------------------------------------- CA6 : une seule ecoute

func TestEcouteUniquementSurLAdresseConfiguree(t *testing.T) {
	faux := fauxiptv.Nouveau(t, fauxiptv.Options{Utilisateur: utilisateurIPTV, MotDePasse: motDePasseIPTV})
	d := lancer(t, confTest(t, faux.URL(), ""))
	h, _, _ := net.SplitHostPort(d.a.Adresse())
	if h != "127.0.0.1" {
		t.Fatalf("ecoute sur %s", d.a.Adresse())
	}
	// Une adresse deja prise est une erreur de demarrage, pas un repli silencieux.
	cfg := confTest(t, faux.URL(), "")
	cfg.Serveur.Ecoute = d.a.Adresse()
	if _, err := Construire(context.Background(), cfg, Options{Journal: NouveauJournal2(io.Discard)}); err == nil {
		t.Fatal("adresse occupee acceptee")
	}
}

// ---------------------------------------------------------------- interface

func TestInterfaceServieDepuisUnDossier(t *testing.T) {
	ui := t.TempDir()
	os.WriteFile(filepath.Join(ui, "index.html"), []byte("<html>mytv</html>"), 0o600)
	os.MkdirAll(filepath.Join(ui, "assets"), 0o700)
	os.WriteFile(filepath.Join(ui, "assets", "app.js"), []byte("console.log(1)"), 0o600)
	faux := fauxiptv.Nouveau(t, fauxiptv.Options{Utilisateur: utilisateurIPTV, MotDePasse: motDePasseIPTV})
	d := lancer(t, confTest(t, faux.URL(), "")) // ui_dossier injecte ci-dessous
	d.arret(t)
	cfg := confTest(t, faux.URL(), "")
	cfg.Serveur.UIDossier = ui
	d = lancer(t, cfg)

	for chemin, attendu := range map[string]int{"/": 200, "/chaines/42": 200, "/assets/app.js": 200, "/assets/": 404, "/absent.js": 404, "/assets": 200} {
		code, _ := d.req(t, "GET", chemin, "")
		if code != attendu {
			t.Errorf("%s : %d (attendu %d)", chemin, code, attendu)
		}
	}
	code, corps := d.req(t, "GET", "/chaines/42", "")
	if code != 200 || string(corps) != "<html>mytv</html>" {
		t.Errorf("repli vers index.html : %d %s", code, corps)
	}
	// Pas de traversee de dossier, pas de liste de dossier.
	if code, corps := d.req(t, "GET", "/assets/../../etc/passwd", ""); code != 200 && code != 404 || strings.Contains(string(corps), "root:") {
		t.Errorf("traversee : %d", code)
	}
	req, _ := http.NewRequest("GET", d.url+"/", nil)
	rep, _ := d.client.Do(req)
	rep.Body.Close()
	if !strings.Contains(rep.Header.Get("Content-Security-Policy"), "default-src 'self'") || rep.Header.Get("X-Frame-Options") != "DENY" {
		t.Errorf("en-tetes de l'interface : %v", rep.Header)
	}
	// US-23 CA6 : plus aucun hote tiers pour les images (les logos passent par /api/v1/image).
	if csp := rep.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "img-src 'self' data:;") || strings.Contains(csp, "http:") || strings.Contains(csp, "https:") {
		t.Errorf("CSP trop large pour les images : %q", csp)
	}
	// L'API reste protegee et prioritaire sur le repli de l'interface.
	if code, _ := d.req(t, "GET", "/api/v1/chaines", ""); code != 401 {
		t.Errorf("API sous l'interface : %d", code)
	}
	if code, _ := d.req(t, "POST", "/", "{}"); code != 405 {
		t.Errorf("POST sur l'interface : %d", code)
	}
}

// ---------------------------------------------------------------- profils

func TestSuppressionDUnProfilEffaceSonEtat(t *testing.T) {
	faux := fauxiptv.Nouveau(t, fauxiptv.Options{Utilisateur: utilisateurIPTV, MotDePasse: motDePasseIPTV})
	d := lancer(t, confTest(t, faux.URL(), ""))
	d.connecter(t, "alice", false)
	code, corps := d.req(t, "POST", "/api/v1/compte/profils", `{"nom":"Enfants"}`)
	var p struct{ ID string }
	json.Unmarshal(corps, &p)
	if code != 201 {
		t.Fatalf("%d %s", code, corps)
	}
	if code, _ := d.req(t, "PUT", "/api/v1/compte/profil-actif", `{"profil_id":"`+p.ID+`"}`); code != 204 {
		t.Fatal(code)
	}
	if code, _ := d.req(t, "PUT", "/api/v1/profil/favoris/iptva:1001", ""); code != 204 {
		t.Fatal(code)
	}
	if n := compterLignes(t, d.a, p.ID); n != 1 {
		t.Fatalf("le favori doit etre ecrit : %d lignes", n)
	}
	if code, _ := d.req(t, "DELETE", "/api/v1/compte/profils/"+p.ID, ""); code != 204 {
		t.Fatalf("suppression : %d", code)
	}
	if n := compterLignes(t, d.a, p.ID); n != 0 {
		t.Fatalf("%d lignes d'etat restent apres la suppression du profil", n)
	}
}

// ---------------------------------------------------------------- CA2 : arret propre

// goroutinesDuProjet liste les goroutines dont la pile passe par le code de mytv,
// hors test et faux fournisseur.
func goroutinesDuProjet() []string {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	var out []string
	for _, g := range strings.Split(string(buf), "\n\n") {
		if !strings.Contains(g, "mytv/internal/") || strings.Contains(g, "fauxiptv") || strings.Contains(g, "testing.tRunner") ||
			strings.Contains(g, "appli.goroutinesDuProjet") {
			continue
		}
		out = append(out, g)
	}
	return out
}

func TestArretPropreSansGoroutineRestante(t *testing.T) {
	faux := fauxiptv.Nouveau(t, fauxiptv.Options{Utilisateur: utilisateurIPTV, MotDePasse: motDePasseIPTV,
		Catalogue: fauxiptv.CatalogueExemple(2, 1, 1)})
	cfg := confTest(t, faux.URL(), "")
	cfg.Fournisseurs[0].Rafraichissement.AuDemarre = true // la planification tourne pendant l'arret
	cfg.Fournisseurs[0].Rafraichissement.Heure = "03:00"
	d := lancer(t, cfg)
	d.connecter(t, "alice", false)
	d.a.catalogue.SynchroniserTous(context.Background())
	_, corps := d.req(t, "GET", "/api/v1/chaines", "")
	var l struct{ Elements []struct{ Lecture string } }
	json.Unmarshal(corps, &l)
	d.req(t, "GET", l.Elements[0].Lecture, "") // un appel partage du relais est en vie

	if err := d.arret(t); err != nil {
		t.Fatalf("arret : %v", err)
	}
	d.client.CloseIdleConnections()
	http.DefaultClient.CloseIdleConnections()
	var restes []string
	for i := 0; i < 100; i++ {
		if restes = goroutinesDuProjet(); len(restes) == 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if len(restes) > 0 {
		t.Fatalf("%d goroutine(s) survivent a l'arret :\n%s", len(restes), strings.Join(restes, "\n---\n"))
	}
	// Le port est libere.
	if c, err := net.DialTimeout("tcp", d.a.Adresse(), 300*time.Millisecond); err == nil {
		c.Close()
		t.Fatal("le serveur ecoute encore")
	}
}

// L'arret n'attend pas indefiniment une requete en cours : apres delai_arret elle est coupee.
func TestArretCoupeLesRequetesLentesApresLeDelai(t *testing.T) {
	faux := fauxiptv.Nouveau(t, fauxiptv.Options{Utilisateur: utilisateurIPTV, MotDePasse: motDePasseIPTV})
	cfg := confTest(t, faux.URL(), "")
	cfg.Serveur.DelaiArret = config.Duree(300 * time.Millisecond)
	d := lancer(t, cfg)
	// Requete qui ne se termine jamais : on ouvre une connexion et on n'envoie qu'une partie de l'en-tete... puis une vraie requete lente.
	c, err := net.Dial("tcp", d.a.Adresse())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprint(c, "GET /sante HTTP/1.1\r\nHost: x\r\nX-Lent: ")
	debut := time.Now()
	if err := d.arret(t); err != nil {
		t.Fatal(err)
	}
	if time.Since(debut) > 4*time.Second {
		t.Fatalf("arret trop long : %v", time.Since(debut))
	}
}

// NouveauJournal2 : journal JSON vers un io.Writer (tests).
func NouveauJournal2(w io.Writer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// compterLignes compte les lignes d'etat (favoris, historique, reglages) d'un profil.
func compterLignes(t *testing.T, a *Appli, profilID string) int {
	t.Helper()
	db, err := comptes.OuvrirBase(a.cfg.Stockage.Fichier)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	total := 0
	for _, tb := range []string{"profil_favoris", "profil_historique", "profil_reglages"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM `+tb+` WHERE profil_id = ?`, profilID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		total += n
	}
	return total
}

// Telemetrie cablee : metriques du relais visibles de l'administrateur seulement,
// sans identifiant de session ni URL dans les etiquettes ; diagnostic recoit des evenements.
func TestTelemetrieCableeEtProtegee(t *testing.T) {
	faux := fauxiptv.Nouveau(t, fauxiptv.Options{Utilisateur: utilisateurIPTV, MotDePasse: motDePasseIPTV,
		Catalogue: fauxiptv.CatalogueExemple(2, 1, 1)})
	d := lancer(t, confTest(t, faux.URL(), ""))
	d.connecter(t, "alice", false)
	d.a.catalogue.SynchroniserTous(context.Background())
	_, corps := d.req(t, "GET", "/api/v1/chaines", "")
	var l struct{ Elements []struct{ Lecture string } }
	json.Unmarshal(corps, &l)
	if code, _ := d.req(t, "GET", l.Elements[0].Lecture, ""); code != 200 {
		t.Fatalf("lecture : %d", code)
	}
	if code, _ := d.req(t, "GET", "/api/v1/metriques", ""); code != 403 {
		t.Fatalf("metriques pour un ordinaire : %d", code)
	}
	if code, _ := d.req(t, "POST", "/api/v1/diagnostic/evenements", `{"appareil":"salon","evenements":[{"type":"lecteur_erreur","message":"coupure-du-lecteur"}]}`); code/100 != 2 {
		t.Fatalf("envoi d'evenement : %d", code)
	}
	// Un autre client, administrateur.
	jar, _ := cookiejar.New(nil)
	d.client = &http.Client{Jar: jar, Timeout: 10 * time.Second}
	d.connecter(t, "root", true)
	code, corps := d.req(t, "GET", "/api/v1/metriques", "")
	if code != 200 || !strings.Contains(string(corps), "live_") {
		t.Fatalf("metriques admin : %d\n%s", code, corps)
	}
	if strings.Contains(string(corps), "session") || strings.Contains(string(corps), "http://") || strings.Contains(string(corps), utilisateurIPTV) {
		t.Fatalf("etiquette non bornee dans les metriques :\n%s", corps)
	}
	if code, corps := d.req(t, "GET", "/api/v1/diagnostic/evenements", ""); code != 200 || !strings.Contains(string(corps), "coupure-du-lecteur") {
		t.Fatalf("diagnostic admin : %d %s", code, corps)
	}
}

// US-16 de bout en bout : l'export en bloc sert le catalogue synchronise (ajoute compris),
// /compte donne les limites, /live/{chaine}/fin rend la vraie place tout de suite.
func TestBoutEnBoutExportLimitesEtFinDeLecture(t *testing.T) {
	faux := fauxiptv.Nouveau(t, fauxiptv.Options{Utilisateur: utilisateurIPTV, MotDePasse: motDePasseIPTV,
		Catalogue: fauxiptv.CatalogueExemple(4, 3, 2)})
	d := lancer(t, confTest(t, faux.URL(), ""))
	d.connecter(t, "alice", false)
	if err := d.a.catalogue.SynchroniserTous(context.Background()); err != nil {
		t.Fatal(err)
	}

	code, corps := d.req(t, "GET", "/api/v1/catalogue/version", "")
	var m struct {
		Version                            string
		Categories, Chaines, Films, Series int
	}
	if json.Unmarshal(corps, &m) != nil || code != 200 || m.Chaines != 4 || m.Films != 3 || m.Series != 2 || m.Categories != 6 || m.Version == "" {
		t.Fatalf("version : %d %s", code, corps)
	}
	code, corps = d.req(t, "GET", "/api/v1/catalogue/export?type=live", "")
	if code != 200 || strings.Count(string(corps), "\n") != 4 || !strings.Contains(string(corps), `"ajoute":"2023-11-14T22:13:20Z"`) {
		t.Fatalf("export live : %d %s", code, corps)
	}
	code, corps = d.req(t, "GET", "/api/v1/catalogue/export?type=series", "")
	if code != 200 || !strings.Contains(string(corps), `"ajoute":"2024-03-09T16:00:00Z"`) {
		t.Fatalf("export series (last_modified) : %d %s", code, corps)
	}
	for _, s := range []string{utilisateurIPTV, motDePasseIPTV, faux.URL()} {
		if strings.Contains(string(corps), s) {
			t.Fatalf("l'export expose %q", s)
		}
	}

	if code, corps = d.req(t, "GET", "/api/v1/compte", ""); code != 200 || !strings.Contains(string(corps), `"pagination_max":200`) ||
		!strings.Contains(string(corps), `"grille_fenetre":172800`) {
		t.Fatalf("compte : %d %s", code, corps)
	}

	// Lecture puis fin : la place est rendue sans attendre le TTL ni la deconnexion.
	if code, _ = d.req(t, "GET", "/api/v1/live/1001.m3u8", ""); code != 200 {
		t.Fatalf("playlist : %d", code)
	}
	if n := d.a.baux.Occupes("iptva"); n != 1 {
		t.Fatalf("place occupee attendue : %d", n)
	}
	if code, _ = d.req(t, "POST", "/api/v1/live/1002/fin", ""); code != 204 || d.a.baux.Occupes("iptva") != 1 {
		t.Fatalf("fin d'une autre chaine : %d, places %d", code, d.a.baux.Occupes("iptva"))
	}
	if code, _ = d.req(t, "POST", "/api/v1/live/1001/fin", ""); code != 204 || d.a.baux.Occupes("iptva") != 0 {
		t.Fatalf("fin de la chaine lue : %d, places %d", code, d.a.baux.Occupes("iptva"))
	}
}

// US-16b : series_id et stream_id sont deux espaces distincts chez Xtream. Un film et une
// serie de MEME numero coexistent (ids iptva:77 et iptva:s77), avec des favoris et un
// historique distincts, et la disparition de la serie n'orpheline pas le film.
func TestFilmEtSerieDeMemeNumeroCoexistent(t *testing.T) {
	cat := fauxiptv.CatalogueExemple(2, 0, 0)
	cat["get_vod_streams"] = []map[string]any{{"name": "Film 77", "stream_id": 77, "category_id": "v1", "container_extension": "mp4", "added": "1700000000"}}
	cat["get_series"] = []map[string]any{{"name": "Serie 77", "series_id": 77, "category_id": "s1"}}
	faux := fauxiptv.Nouveau(t, fauxiptv.Options{Utilisateur: utilisateurIPTV, MotDePasse: motDePasseIPTV, Catalogue: cat})
	d := lancer(t, confTest(t, faux.URL(), ""))
	d.connecter(t, "alice", false)
	if err := d.a.catalogue.SynchroniserTous(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Les deux existent, sous deux identifiants differents.
	if code, corps := d.req(t, "GET", "/api/v1/films/iptva:77", ""); code != 200 || !strings.Contains(string(corps), `"nom":"Film 77"`) {
		t.Fatalf("film : %d %s", code, corps)
	}
	if code, corps := d.req(t, "GET", "/api/v1/series/iptva:s77", ""); code != 200 || !strings.Contains(string(corps), `"nom":"Serie 77"`) ||
		!strings.Contains(string(corps), `"saisons":"/api/v1/series/iptva:s77/saisons"`) {
		t.Fatalf("serie : %d %s", code, corps)
	}
	// Chacun ne repond que sous son type.
	if code, _ := d.req(t, "GET", "/api/v1/series/iptva:77", ""); code != 404 {
		t.Errorf("serie sous l'identifiant du film : %d", code)
	}
	if code, _ := d.req(t, "GET", "/api/v1/films/iptva:s77", ""); code != 404 {
		t.Errorf("film sous l'identifiant de la serie : %d", code)
	}
	// L'export les rend tous deux, ids distincts.
	_, films := d.req(t, "GET", "/api/v1/catalogue/export?type=vod", "")
	_, series := d.req(t, "GET", "/api/v1/catalogue/export?type=series", "")
	if !strings.Contains(string(films), `"id":"iptva:77"`) || !strings.Contains(string(series), `"id":"iptva:s77"`) {
		t.Errorf("export : films %s / series %s", films, series)
	}

	// « Ma liste » distincte pour chacun : retirer l'un laisse l'autre. (Les favoris ne
	// concernent plus que les chaines depuis le lot 10 : 422 pour le film comme pour la serie.)
	for _, id := range []string{"iptva:77", "iptva:s77"} {
		if code, corps := d.req(t, "PUT", "/api/v1/profil/favoris/"+id, ""); code != 422 || !strings.Contains(string(corps), "favori_chaines_seulement") {
			t.Fatalf("favori %s : %d %s", id, code, corps)
		}
	}
	for _, id := range []string{"vod:iptva:77", "serie:iptva:s77"} {
		if code, corps := d.req(t, "PUT", "/api/v1/profil/a_voir/"+id, ""); code != 204 {
			t.Fatalf("ma liste %s : %d %s", id, code, corps)
		}
	}
	_, corps := d.req(t, "GET", "/api/v1/profil/a_voir", "")
	if !strings.Contains(string(corps), `"contenu":"vod:iptva:77"`) || !strings.Contains(string(corps), `"contenu":"serie:iptva:s77"`) {
		t.Fatalf("les deux elements sont attendus : %s", corps)
	}
	if code, _ := d.req(t, "DELETE", "/api/v1/profil/a_voir/serie:iptva:s77", ""); code != 204 {
		t.Fatal("retrait de la serie de la liste")
	}
	_, corps = d.req(t, "GET", "/api/v1/profil/a_voir", "")
	if !strings.Contains(string(corps), `"contenu":"vod:iptva:77"`) || strings.Contains(string(corps), `"contenu":"serie:iptva:s77"`) {
		t.Fatalf("seul le film doit rester dans la liste : %s", corps)
	}

	// Historique : un episode de la serie s77 et la lecture du film 77, distincts.
	if code, corps := d.req(t, "PUT", "/api/v1/profil/historique/iptva:9001.mkv", `{"position":42,"duree":3000,"serie":"iptva:s77"}`); code != 204 {
		t.Fatalf("episode : %d %s", code, corps)
	}
	if code, corps := d.req(t, "PUT", "/api/v1/profil/historique/iptva:77", `{"position":7,"duree":6000}`); code != 204 {
		t.Fatalf("film : %d %s", code, corps)
	}
	// Un episode ne peut pas se rattacher a la serie « 77 » (c'est le film) ni a un id de film.
	if code, _ := d.req(t, "PUT", "/api/v1/profil/historique/iptva:9002.mkv", `{"position":1,"serie":"iptva:77"}`); code != 400 {
		t.Errorf("episode rattache au film 77 : %d au lieu de 400", code)
	}
	_, corps = d.req(t, "GET", "/api/v1/profil/historique", "")
	if !strings.Contains(string(corps), `"contenu":"iptva:9001.mkv","serie":"iptva:s77"`) || !strings.Contains(string(corps), `"contenu":"iptva:77"`) {
		t.Fatalf("historique : %s", corps)
	}

	// Resynchronisation SANS la serie 77 : l'episode devient orphelin, le film reste (et
	// vice-versa). Une autre serie la remplace : un type qui tombe a zero serait refuse
	// comme catalogue degrade (US-36 CA2).
	sans := map[string]any{}
	for k, v := range cat {
		sans[k] = v
	}
	sans["get_series"] = []map[string]any{{"name": "Serie 78", "series_id": 78, "category_id": "s1"}}
	faux.DefinirCatalogue(sans)
	if err := d.a.catalogue.SynchroniserTous(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, corps = d.req(t, "GET", "/api/v1/profil/historique", "")
	if strings.Contains(string(corps), "9001") || !strings.Contains(string(corps), `"contenu":"iptva:77"`) {
		t.Fatalf("apres la disparition de la serie : l'episode doit disparaitre, le film rester : %s", corps)
	}
	_, corps = d.req(t, "GET", "/api/v1/profil/a_voir", "")
	if !strings.Contains(string(corps), `"contenu":"vod:iptva:77"`) {
		t.Fatalf("le film a disparu de la liste avec la serie : %s", corps)
	}
}

// US-16b : le nom du compte est dans Session (POST /connexion et GET /compte).
func TestSessionPorteLeNomDuCompte(t *testing.T) {
	faux := fauxiptv.Nouveau(t, fauxiptv.Options{Utilisateur: utilisateurIPTV, MotDePasse: motDePasseIPTV})
	d := lancer(t, confTest(t, faux.URL(), ""))
	if _, err := d.a.Comptes().CreerUtilisateur(context.Background(), "Alice-Martin", motDePasseUser, false); err != nil {
		t.Fatal(err)
	}
	code, corps := d.req(t, "POST", "/api/v1/connexion", fmt.Sprintf(`{"nom":"alice-martin","mot_de_passe":%q}`, motDePasseUser))
	if code != 200 || !strings.Contains(string(corps), `"nom":"Alice-Martin"`) {
		t.Fatalf("connexion (le nom est celui de la creation, pas celui saisi) : %d %s", code, corps)
	}
	code, corps = d.req(t, "GET", "/api/v1/compte", "")
	if code != 200 || !strings.Contains(string(corps), `"session":{"id":`) || !strings.Contains(string(corps), `"nom":"Alice-Martin"`) {
		t.Fatalf("compte : %d %s", code, corps)
	}
}

// US-17 CA2 de bout en bout : le fournisseur (joint par « localhost ») redirige la
// playlist vers un service interne (127.0.0.1). Le programme assemble le filtre : la
// redirection n'est pas suivie, le client recoit un 502 propre, le service interne ne
// voit aucune requete.
func TestRedirectionDuFournisseurVersUnServiceInterneNEstPasSuivie(t *testing.T) {
	var interne atomic.Int32
	svcInterne := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		interne.Add(1)
		io.WriteString(w, "#EXTM3U\n#EXTINF:1,\nx.ts\n")
	}))
	defer svcInterne.Close()
	four := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, svcInterne.URL+"/secret-interne", http.StatusFound)
	}))
	defer four.Close()
	_, port, _ := net.SplitHostPort(four.Listener.Addr().String())
	d := lancer(t, confTest(t, "http://localhost:"+port, ""))
	d.connecter(t, "alice", false)

	code, corps := d.req(t, "GET", "/api/v1/live/1001.m3u8", "")
	if code != http.StatusBadGateway {
		t.Fatalf("playlist redirigee vers l'interne : %d %s", code, corps)
	}
	if strings.Contains(string(corps), "secret-interne") || strings.Contains(string(corps), "127.0.0.1") {
		t.Errorf("la reponse cite la destination : %s", corps)
	}
	if n := interne.Load(); n != 0 {
		t.Fatalf("le service interne a recu %d requete(s)", n)
	}
	d.arret(t)
	if strings.Contains(d.journal.String(), "secret-interne") {
		t.Error("l'adresse de la redirection est dans le journal")
	}
}

// ---------------------------------------------------------------- US-25 : supervision de bout en bout

// US-25 CA1-CA4 : avec un faux fournisseur dont l'abonnement expire dans 3 jours, puis dont
// le compte est refuse : user_info est lu au demarrage, /admin/fournisseurs et la metrique le disent,
// le webhook (https, signe) est appele une fois par evenement, le lecteur recoit
// fournisseur_compte_invalide avec la date, et ni le secret ni l'URL du webhook n'atteignent le journal.
func TestSupervisionDeBoutEnBout(t *testing.T) {
	var mu sync.Mutex
	var recus []string
	hook := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		recus = append(recus, r.Header.Get("X-Mytv-Evenement"))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer hook.Close()
	evenements := func() []string { mu.Lock(); defer mu.Unlock(); return append([]string(nil), recus...) }
	t.Setenv("MYTV_T_WEBHOOK", "secret-du-webhook-de-test")
	exp := time.Now().Add(72 * time.Hour).Truncate(time.Second)
	faux := fauxiptv.Nouveau(t, fauxiptv.Options{Utilisateur: utilisateurIPTV, MotDePasse: motDePasseIPTV,
		Catalogue: fauxiptv.CatalogueExemple(2, 1, 1),
		UserInfo:  map[string]any{"auth": 1, "status": "Active", "exp_date": fmt.Sprint(exp.Unix()), "max_connections": "1"}})
	urlHook := hook.URL + "/hook/JETON-DANS-L-URL"
	d := lancerAvec(t, confTest(t, faux.URL(), fmt.Sprintf(
		"supervision: { webhook: { url: %q, secret: { env: MYTV_T_WEBHOOK } }, echecs_synchro: 1 }", urlHook)),
		Options{ClientWebhook: hook.Client()})
	d.connecter(t, "root", true)

	// Lu au demarrage (Planifier), sans attendre la synchronisation de 05:00.
	var ligne map[string]any
	attendre(t, "user_info lu au demarrage", func() bool {
		_, corps := d.req(t, "GET", "/api/v1/admin/fournisseurs", "")
		var r struct{ Fournisseurs []map[string]any }
		json.Unmarshal(corps, &r)
		if len(r.Fournisseurs) == 1 && r.Fournisseurs[0]["etat"] == "ok" {
			ligne = r.Fournisseurs[0]
			return true
		}
		return false
	})
	if ligne["statut_compte"] != "Active" || ligne["alerte_expiration"] != true || ligne["connexions_max"] != float64(1) {
		t.Errorf("ligne : %v", ligne)
	}
	attendre(t, "webhook expiration_proche", func() bool { return len(evenements()) == 1 })
	if e := evenements(); e[0] != "expiration_proche" {
		t.Errorf("evenements : %v", e)
	}
	if _, corps := d.req(t, "GET", "/api/v1/metriques", ""); !strings.Contains(string(corps), `fournisseur_expire_dans_secondes{fournisseur="iptva"}`) {
		t.Errorf("metrique absente :\n%s", corps)
	}
	// Sante approfondie : tout va bien (synchronisation recente, fournisseur joignable).
	if err := d.a.catalogue.SynchroniserTous(context.Background()); err != nil {
		t.Fatal(err)
	}
	if code, corps := d.req(t, "GET", "/api/v1/admin/sante", ""); code != 200 || !strings.Contains(string(corps), `"statut":"ok"`) {
		t.Errorf("sante : %d %s", code, corps)
	}

	// Le fournisseur refuse le compte (401 sur la playlist et sur l'API).
	d.req(t, "GET", "/api/v1/chaines", "")
	_, corps := d.req(t, "GET", "/api/v1/chaines", "")
	var l struct{ Elements []struct{ Lecture string } }
	json.Unmarshal(corps, &l)
	faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CiblePlaylist, Statut: 401})
	code, corps := d.req(t, "GET", l.Elements[0].Lecture, "")
	var e map[string]string
	json.Unmarshal(corps, &e)
	if code != 502 || e["code"] != "fournisseur_compte_invalide" || e["expiration"] != exp.UTC().Format(time.RFC3339) {
		t.Fatalf("lecture avec compte refuse : %d %s", code, corps)
	}
	attendre(t, "webhook compte_refuse", func() bool { return len(evenements()) == 2 })
	if e := evenements(); e[1] != "compte_refuse" {
		t.Errorf("evenements : %v", e)
	}
	faux.Effacer()
	faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleAPI, Statut: 458})
	if err := d.a.catalogue.SynchroniserTous(context.Background()); err == nil {
		t.Fatal("synchronisation refusee attendue")
	}
	_, corps = d.req(t, "GET", "/api/v1/admin/fournisseurs", "")
	if !strings.Contains(string(corps), `"etat":"adresse_refusee"`) || !strings.Contains(string(corps), `"code":"fournisseur_adresse_refusee"`) {
		t.Errorf("etat apres 458 : %s", corps)
	}
	if code, _ := d.req(t, "GET", "/api/v1/admin/sante", ""); code != 503 {
		t.Errorf("sante avec fournisseur refuse : %d", code)
	}

	d.arret(t)
	j := d.journal.String()
	for _, s := range []string{"secret-du-webhook-de-test", "JETON-DANS-L-URL", hook.URL, utilisateurIPTV, motDePasseIPTV} {
		if strings.Contains(j, s) {
			t.Errorf("secret %q dans le journal", s)
		}
	}
	if !strings.Contains(j, `"evenement":"expiration_proche"`) {
		t.Errorf("avertissement d'expiration absent du journal")
	}
}

func attendre(t *testing.T, quoi string, ok func() bool) {
	t.Helper()
	fin := time.Now().Add(5 * time.Second)
	for !ok() {
		if time.Now().After(fin) {
			t.Fatalf("delai depasse : %s", quoi)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ---------------------------------------------------------------- US-26 : sauvegarde du service

// US-26 CA1 : le service sauvegarde sa base vivante dans un dossier prive, verifiee ; un echec
// (dossier impossible) est journalise ET notifie par le webhook, une seule fois par periode.
func TestSauvegardeDuServiceEtEchecNotifie(t *testing.T) {
	var mu sync.Mutex
	var recus []string
	hook := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		recus = append(recus, r.Header.Get("X-Mytv-Evenement"))
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	defer hook.Close()
	faux := fauxiptv.Nouveau(t, fauxiptv.Options{Utilisateur: utilisateurIPTV, MotDePasse: motDePasseIPTV})
	dossier := filepath.Join(t.TempDir(), "sv")
	d := lancerAvec(t, confTest(t, faux.URL(), fmt.Sprintf(
		"sauvegarde: { heure: \"\", dossier: %q, conservation: 3 }\nsupervision: { webhook: { url: %q } }", dossier, hook.URL+"/h")),
		Options{ClientWebhook: hook.Client()})
	d.connecter(t, "alice", false)

	f, err := d.a.sauvegarde.Sauvegarder(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(dossier); err != nil || st.Mode().Perm() != 0o700 {
		t.Errorf("dossier : %v %v", st, err)
	}
	if st, err := os.Stat(f); err != nil || st.Mode().Perm() != 0o600 || st.Size() == 0 {
		t.Errorf("fichier : %v %v", st, err)
	}
	// La copie contient le compte cree PENDANT que le service tournait.
	copie, err := sauvegarde.OuvrirLectureSeule(f)
	if err != nil {
		t.Fatal(err)
	}
	defer copie.Close()
	var n int
	if err := copie.QueryRow(`SELECT COUNT(*) FROM utilisateurs WHERE nom = 'alice'`).Scan(&n); err != nil || n != 1 {
		t.Errorf("alice dans la sauvegarde : %d %v", n, err)
	}

	// Echec : le dossier de sauvegarde est un fichier ordinaire.
	fichier := filepath.Join(t.TempDir(), "pas-un-dossier")
	os.WriteFile(fichier, []byte("x"), 0o600)
	if d.a.sauvegarde, err = d.a.planificateurSauvegarde(fichier); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if _, err := d.a.sauvegarde.Sauvegarder(context.Background()); err == nil {
			t.Fatal("echec attendu")
		}
	}
	attendre(t, "webhook sauvegarde_echec", func() bool { mu.Lock(); defer mu.Unlock(); return len(recus) >= 1 })
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	if len(recus) != 1 || recus[0] != "sauvegarde_echec" {
		t.Errorf("webhook : %v (un seul message par periode, malgre 3 echecs)", recus)
	}
	mu.Unlock()
	d.arret(t)
	if j := d.journal.String(); !strings.Contains(j, `"msg":"sauvegarde echouee"`) {
		t.Errorf("echec absent du journal")
	}
}

// US-26 CA3 : l'ecoute sur toutes les interfaces sans proxy ni plage locale est signalee au
// demarrage (journal), mais le service demarre.
func TestDemarrageAvertitPourUneEcouteSurToutesLesInterfaces(t *testing.T) {
	faux := fauxiptv.Nouveau(t, fauxiptv.Options{Utilisateur: utilisateurIPTV, MotDePasse: motDePasseIPTV})
	cfg := confTest(t, faux.URL(), "")
	cfg.Serveur.Ecoute = "0.0.0.0:8080" // jamais ouvert : l'ecouteur de test est injecte, sur la boucle locale
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	j := &tampon{}
	a, err := Construire(context.Background(), cfg, Options{Journal: NouveauJournal2(j), Ecouteur: ln})
	if err != nil {
		t.Fatal(err)
	}
	a.Fermer()
	if !strings.Contains(j.String(), `"level":"WARN"`) || !strings.Contains(j.String(), "proxys_fiables") {
		t.Errorf("avertissement attendu :\n%s", j.String())
	}
	// Avec un proxy declare : plus d'avertissement.
	cfg2 := confTest(t, faux.URL(), "")
	cfg2.Comptes.ProxysFiables = []string{"10.0.0.1"}
	cfg2.Serveur.Ecoute = "0.0.0.0:8080"
	ln2, _ := net.Listen("tcp", "127.0.0.1:0")
	j2 := &tampon{}
	a2, err := Construire(context.Background(), cfg2, Options{Journal: NouveauJournal2(j2), Ecouteur: ln2})
	if err != nil {
		t.Fatal(err)
	}
	a2.Fermer()
	if strings.Contains(j2.String(), "proxys_fiables") {
		t.Errorf("avertissement inutile :\n%s", j2.String())
	}
}

// US-28 CA2/CA3/CA4 de bout en bout : la fiche d'un film porte ses details, ils
// survivent a la resynchronisation du catalogue, et AUCUN appel de details n'est fait
// pendant l'import du catalogue.
func TestDetailsDeFilmDeBoutEnBout(t *testing.T) {
	cat := fauxiptv.CatalogueExemple(2, 2, 1)
	faux := fauxiptv.Nouveau(t, fauxiptv.Options{Utilisateur: utilisateurIPTV, MotDePasse: motDePasseIPTV, Catalogue: cat,
		InfosFilm: map[string]any{"2001": map[string]any{"info": map[string]any{"plot": "Le synopsis", "genre": "Drame", "duration_secs": 6000}},
			"2002": map[string]any{"info": map[string]any{"plot": "Autre"}}}})
	d := lancer(t, confTest(t, faux.URL(), ""))
	d.connecter(t, "alice", false)
	if err := d.a.catalogue.SynchroniserTous(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Une liste ne touche pas le fournisseur pour les details.
	d.req(t, "GET", "/api/v1/films", "")
	if n := faux.Compteurs().ParAction["get_vod_info"]; n != 0 {
		t.Fatalf("%d appels de details pour une liste", n)
	}
	code, corps := d.req(t, "GET", "/api/v1/films/iptva:2001", "")
	if code != 200 || !strings.Contains(string(corps), `"synopsis":"Le synopsis"`) || !strings.Contains(string(corps), `"details_statut":"ok"`) ||
		!strings.Contains(string(corps), `"duree":6000`) {
		t.Fatalf("%d %s", code, corps)
	}
	// Resynchronisation : les details sont servis depuis la base, sans nouvel appel.
	if err := d.a.catalogue.SynchroniserTous(context.Background()); err != nil {
		t.Fatal(err)
	}
	if code, corps = d.req(t, "GET", "/api/v1/films/iptva:2001", ""); code != 200 || !strings.Contains(string(corps), `"synopsis":"Le synopsis"`) {
		t.Fatalf("apres resynchronisation : %d %s", code, corps)
	}
	if n := faux.Compteurs().ParAction["get_vod_info"]; n != 1 {
		t.Fatalf("%d appels de details, attendu 1", n)
	}

	// Pendant l'import : pas d'appel, fiche degradee ; apres : les details arrivent.
	faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleAPI, Latence: 700 * time.Millisecond, Fois: 1})
	fini := make(chan error, 1)
	go func() { fini <- d.a.catalogue.SynchroniserTous(context.Background()) }()
	time.Sleep(150 * time.Millisecond)
	code, corps = d.req(t, "GET", "/api/v1/films/iptva:2002", "")
	if code != 200 || !strings.Contains(string(corps), `"details_statut":"indisponible"`) || faux.Compteurs().ParAction["get_vod_info"] != 1 {
		t.Fatalf("pendant l'import : %d %s (%d appels)", code, corps, faux.Compteurs().ParAction["get_vod_info"])
	}
	if err := <-fini; err != nil {
		t.Fatal(err)
	}
	if code, corps = d.req(t, "GET", "/api/v1/films/iptva:2002", ""); code != 200 || !strings.Contains(string(corps), `"synopsis":"Autre"`) {
		t.Fatalf("apres l'import : %d %s", code, corps)
	}
	// Aucun identifiant du fournisseur dans le journal.
	if j := d.journal.String(); strings.Contains(j, motDePasseIPTV) {
		t.Error("mot de passe du fournisseur dans le journal")
	}
}
