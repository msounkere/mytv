package comptes

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mytv/internal/config"
	"mytv/internal/domain"
)

type horlogeTest struct {
	mu sync.Mutex
	t  time.Time
}

func (h *horlogeTest) Maintenant() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.t }
func (h *horlogeTest) Avancer(d time.Duration) {
	h.mu.Lock()
	h.t = h.t.Add(d)
	h.mu.Unlock()
}

type termTest struct {
	mu  sync.Mutex
	ids []string
}

func (t *termTest) Terminer(s string) { t.mu.Lock(); t.ids = append(t.ids, s); t.mu.Unlock() }
func (t *termTest) vus() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.ids...)
}

type banc struct {
	*Service
	h       *horlogeTest
	term    *termTest
	journal *bytes.Buffer
	chemin  string
}

func configTest() config.Comptes {
	c := config.Defauts().Comptes
	// Parametres minuscules : les tests verifient la logique, pas la robustesse d'argon2.
	c.Argon2.MemoireKio, c.Argon2.Iterations, c.Argon2.Parallelisme = 64, 1, 1
	c.CookieSecurise = true
	return c
}

func nouveauBanc(t *testing.T, mod func(*config.Comptes)) *banc {
	t.Helper()
	c := configTest()
	if mod != nil {
		mod(&c)
	}
	chemin := filepath.Join(t.TempDir(), "c.db")
	db, err := OuvrirBase(chemin)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	b := &banc{h: &horlogeTest{t: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}, term: &termTest{},
		journal: &bytes.Buffer{}, chemin: chemin}
	b.Service, err = Nouveau(context.Background(), Options{DB: db, Config: c, Terminateur: b.term, Horloge: b.h,
		Journal: slog.New(slog.NewTextHandler(b.journal, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const mdp = "correct horse battery"

func (b *banc) utilisateur(t *testing.T, nom string, admin bool) *domain.Utilisateur {
	t.Helper()
	u, err := b.CreerUtilisateur(context.Background(), nom, mdp, admin)
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// CA1 : le schema est versionne, rejouable, et refuse une base plus recente.
func TestMigrationsVersionneesEtRejouables(t *testing.T) {
	b := nouveauBanc(t, nil)
	if err := b.migrer(context.Background()); err != nil {
		t.Fatalf("rejeu : %v", err)
	}
	var v int
	if err := b.db.QueryRow(`SELECT MAX(version) FROM comptes_migrations`).Scan(&v); err != nil || v != migrations[len(migrations)-1].version {
		t.Fatalf("version = %d, %v", v, err)
	}
	if _, err := b.db.Exec(`INSERT INTO comptes_migrations VALUES (999, 'futur', 0)`); err != nil {
		t.Fatal(err)
	}
	if err := b.migrer(context.Background()); err == nil {
		t.Fatal("une base plus recente que le programme doit etre refusee")
	}
}

// CA1 : le mot de passe n'est jamais stocke en clair, l'empreinte porte les parametres de la config.
func TestMotDePasseJamaisEnClairEtParametresDeLaConfig(t *testing.T) {
	b := nouveauBanc(t, nil)
	u := b.utilisateur(t, "alice", false)
	var emp string
	if err := b.db.QueryRow(`SELECT empreinte FROM utilisateurs WHERE id = ?`, u.ID).Scan(&emp); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(emp, mdp) || !strings.HasPrefix(emp, "$argon2id$v=19$m=64,t=1,p=1$") {
		t.Fatalf("empreinte inattendue : %q", emp)
	}
	// Deux comptes au meme mot de passe n'ont pas la meme empreinte (sel).
	u2 := b.utilisateur(t, "bob", false)
	var emp2 string
	b.db.QueryRow(`SELECT empreinte FROM utilisateurs WHERE id = ?`, u2.ID).Scan(&emp2)
	if emp == emp2 {
		t.Fatal("le sel doit etre propre a chaque compte")
	}
}

// CA1 : une empreinte calculee avec d'anciens parametres reste valable puis est renouvelee.
func TestEmpreinteAncienneRenouvelee(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.utilisateur(t, "alice", false)
	b.params = parametres{128, 2, 1} // la config change
	if _, _, err := b.Connecter(context.Background(), "alice", mdp, "10.0.0.1"); err != nil {
		t.Fatal(err)
	}
	var emp string
	b.db.QueryRow(`SELECT empreinte FROM utilisateurs`).Scan(&emp)
	if !strings.Contains(emp, "m=128,t=2,p=1") {
		t.Fatalf("empreinte non renouvelee : %q", emp)
	}
}

func TestNomsEtMotsDePasseRefuses(t *testing.T) {
	b := nouveauBanc(t, nil)
	ctx := context.Background()
	for _, n := range []string{"", " ", "a\nb", "x'; DROP TABLE utilisateurs;--", strings.Repeat("a", 80), "-tiret"} {
		if _, err := b.CreerUtilisateur(ctx, n, mdp, false); !errors.Is(err, ErrNomInvalide) {
			t.Errorf("nom %q : err = %v", n, err)
		}
	}
	if _, err := b.CreerUtilisateur(ctx, "alice", "court", false); !errors.Is(err, ErrMotDePasseFaible) {
		t.Errorf("mot de passe court : %v", err)
	}
	b.utilisateur(t, "Alice", false)
	if _, err := b.CreerUtilisateur(ctx, "ALICE", mdp, false); !errors.Is(err, domain.ErrExiste) {
		t.Errorf("doublon insensible a la casse : %v", err)
	}
}

// CA2 : utilisateur inconnu et mauvais mot de passe donnent la MEME erreur.
func TestErreurIdentiquePourInconnuEtMauvaisMotDePasse(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.utilisateur(t, "alice", false)
	_, _, e1 := b.Connecter(context.Background(), "alice", "mauvais mot de passe", "10.0.0.1")
	_, _, e2 := b.Connecter(context.Background(), "inconnu", "mauvais mot de passe", "10.0.0.2")
	if !errors.Is(e1, domain.ErrIdentifiantsInvalides) || e1 != e2 || e1.Error() != e2.Error() {
		t.Fatalf("e1 = %v, e2 = %v", e1, e2)
	}
}

// CA2 : le compte inconnu paie aussi un calcul d'empreinte (temps comparable).
func TestTempsDeReponseComparablePourCompteInconnu(t *testing.T) {
	b := nouveauBanc(t, func(c *config.Comptes) {
		c.Argon2.MemoireKio, c.Argon2.Iterations = 16*1024, 2 // quelques ms : mesurable
		c.Tentatives.Seuil = 1000
	})
	b.utilisateur(t, "alice", false)
	mesure := func(nom string) time.Duration {
		best := time.Hour
		for i := 0; i < 5; i++ {
			d := time.Now()
			b.Connecter(context.Background(), nom, "mauvais mot de passe", "10.0.0.1")
			if e := time.Since(d); e < best {
				best = e
			}
		}
		return best
	}
	connu, inconnu := mesure("alice"), mesure("fantome")
	if inconnu < connu/2 || inconnu > connu*2 {
		t.Fatalf("temps distinguables : connu %v, inconnu %v", connu, inconnu)
	}
	if inconnu < time.Millisecond {
		t.Fatalf("le faux calcul semble absent : %v", inconnu)
	}
}

// CA2 : blocage par COMPTE, meme depuis des adresses toujours differentes.
func TestBlocageParCompte(t *testing.T) {
	b := nouveauBanc(t, nil) // seuil 5
	b.utilisateur(t, "alice", false)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		adr := "10.0.1." + string(rune('1'+i))
		if _, _, err := b.Connecter(ctx, "alice", "mauvais mot de passe", adr); !errors.Is(err, domain.ErrIdentifiantsInvalides) {
			t.Fatalf("essai %d : %v", i, err)
		}
	}
	// Meme le BON mot de passe est refuse pendant le blocage.
	if _, _, err := b.Connecter(ctx, "alice", mdp, "10.0.9.9"); !errors.Is(err, domain.ErrTropDeTentatives) {
		t.Fatalf("compte non bloque : %v", err)
	}
	// Un autre compte, autre adresse, n'est pas touche.
	b.utilisateur(t, "bob", false)
	if _, _, err := b.Connecter(ctx, "bob", mdp, "10.0.9.8"); err != nil {
		t.Fatalf("bob : %v", err)
	}
}

// CA2 : blocage par ADRESSE, meme en essayant des comptes differents.
func TestBlocageParAdresse(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.utilisateur(t, "alice", false)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		b.Connecter(ctx, "compte"+string(rune('a'+i)), "x", "192.0.2.7")
	}
	if _, _, err := b.Connecter(ctx, "alice", mdp, "192.0.2.7"); !errors.Is(err, domain.ErrTropDeTentatives) {
		t.Fatalf("adresse non bloquee : %v", err)
	}
	if _, _, err := b.Connecter(ctx, "alice", mdp, "192.0.2.8"); err != nil {
		t.Fatalf("autre adresse : %v", err)
	}
}

// CA2 : un attaquant IPv6 ne contourne pas la limite en changeant de bas d'adresse.
func TestBlocageIPv6ParPrefixe(t *testing.T) {
	b := nouveauBanc(t, nil)
	for i := 0; i < 5; i++ {
		b.Connecter(context.Background(), "c"+string(rune('a'+i)), "x", "2001:db8:1:2::"+string(rune('1'+i)))
	}
	if _, _, err := b.Connecter(context.Background(), "z", "x", "2001:db8:1:2:ffff::9"); !errors.Is(err, domain.ErrTropDeTentatives) {
		t.Fatalf("le /64 doit etre bloque : %v", err)
	}
}

// CA2 : le delai croit a chaque blocage, et retombe apres accalmie.
func TestDelaiCroissantPuisOubli(t *testing.T) {
	b := nouveauBanc(t, nil) // fenetre 15 min
	ctx := context.Background()
	echouer := func() {
		for i := 0; i < 5; i++ {
			b.Connecter(ctx, "alice", "x", "10.0.0.1")
		}
	}
	bloque := func() bool {
		_, _, err := b.Connecter(ctx, "alice", "x", "10.0.0.1")
		return errors.Is(err, domain.ErrTropDeTentatives)
	}
	fen := b.cfg.Tentatives.Fenetre.Std()
	echouer()
	if !bloque() {
		t.Fatal("blocage 1 attendu")
	}
	b.h.Avancer(fen + time.Second)
	if bloque() { // consomme une tentative (donc 1 sur 5)
		t.Fatal("blocage 1 devrait etre fini apres une fenetre")
	}
	for i := 0; i < 4; i++ {
		b.Connecter(ctx, "alice", "x", "10.0.0.1")
	}
	// 2e blocage : dure 2 fenetres.
	b.h.Avancer(fen + time.Second)
	if !bloque() {
		t.Fatal("le 2e blocage doit durer plus d'une fenetre")
	}
	b.h.Avancer(fen)
	if bloque() {
		t.Fatal("le 2e blocage doit finir apres 2 fenetres")
	}
	// Accalmie longue : on repart du palier 0.
	b.h.Avancer(20 * fen)
	echouer()
	b.h.Avancer(fen + time.Second)
	if bloque() {
		t.Fatal("apres une longue accalmie le blocage redevient d'une fenetre")
	}
}

// CA2 : une rafale parallele ne depasse pas le seuil (la tentative est comptee AVANT la verification).
func TestRafaleParalleleBornee(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.utilisateur(t, "alice", false)
	var wg sync.WaitGroup
	var mu sync.Mutex
	verifiees := 0
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := b.Connecter(context.Background(), "alice", "x", "10.0.0.1")
			if errors.Is(err, domain.ErrIdentifiantsInvalides) {
				mu.Lock()
				verifiees++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if verifiees > 5 {
		t.Fatalf("%d mots de passe essayes pour un seuil de 5", verifiees)
	}
}

// CA2 : une connexion reussie remet le compteur du compte a zero.
func TestSuccesRemetLeCompteAZero(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.utilisateur(t, "alice", false)
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		b.Connecter(ctx, "alice", "x", "10.0.0.1")
	}
	if _, _, err := b.Connecter(ctx, "alice", mdp, "10.0.0.2"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, _, err := b.Connecter(ctx, "alice", "x", "10.0.0.3"); !errors.Is(err, domain.ErrIdentifiantsInvalides) {
			t.Fatalf("essai %d : %v", i, err)
		}
	}
}

// CA3 : jeton >= 128 bits, stocke haché, session valide jusqu'a l'expiration seulement.
func TestSessionJetonHacheEtExpiration(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.utilisateur(t, "alice", false)
	ctx := context.Background()
	jeton, sess, err := b.Connecter(ctx, "alice", mdp, "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if len(jeton) < 22 { // 16 octets en base64 = 22 caracteres
		t.Fatalf("jeton trop court : %d", len(jeton))
	}
	rows, _ := b.db.Query(`SELECT jeton_hash FROM sessions`)
	var h string
	rows.Next()
	rows.Scan(&h)
	rows.Close()
	if h == jeton || strings.Contains(h, jeton) || h != hacherJeton(jeton) {
		t.Fatal("le jeton ne doit pas etre stocke en clair")
	}
	j2, s2, _ := b.Connecter(ctx, "alice", mdp, "10.0.0.1")
	if j2 == jeton || s2.ID == sess.ID {
		t.Fatal("jetons et identifiants doivent etre uniques")
	}
	if sess.ID == jeton || strings.Contains(sess.ID, jeton) {
		t.Fatal("l'identifiant de session ne doit pas contenir le jeton")
	}
	got, err := b.Session(ctx, jeton)
	if err != nil || got.ID != sess.ID || got.ProfilID == "" {
		t.Fatalf("session : %+v, %v", got, err)
	}
	b.h.Avancer(b.cfg.DureeSession.Std() - time.Second)
	if _, err := b.Session(ctx, jeton); err != nil {
		t.Fatalf("encore valide : %v", err)
	}
	b.h.Avancer(2 * time.Second)
	if _, err := b.Session(ctx, jeton); !errors.Is(err, domain.ErrSessionInvalide) {
		t.Fatalf("session expiree acceptee : %v", err)
	}
	if ids := b.term.vus(); len(ids) == 0 {
		t.Fatal("une session expiree doit etre terminee (baux rendus)")
	}
}

// CA3 : attributs du cookie.
func TestAttributsDuCookie(t *testing.T) {
	for _, secure := range []bool{true, false} {
		b := nouveauBanc(t, func(c *config.Comptes) { c.CookieSecurise = secure })
		b.utilisateur(t, "alice", false)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("POST", "/connexion", strings.NewReader(`{"nom":"alice","mot_de_passe":"`+mdp+`"}`))
		req.Header.Set("Content-Type", "application/json")
		b.Connexion(1024).ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("code %d : %s", rec.Code, rec.Body)
		}
		c := rec.Result().Cookies()
		if len(c) != 1 || !c[0].HttpOnly || c[0].SameSite != http.SameSiteLaxMode || c[0].Secure != secure || c[0].Path != "/" {
			t.Fatalf("cookie : %+v", c)
		}
		if secure && !strings.HasPrefix(c[0].Name, "__Host-") {
			t.Fatalf("nom : %s", c[0].Name)
		}
		if strings.Contains(rec.Body.String(), c[0].Value) {
			t.Fatal("le jeton ne doit pas etre dans le corps de la reponse")
		}
		if !c[0].Expires.After(b.h.Maintenant()) {
			t.Fatal("expiration absente")
		}
	}
}

// CA3 : la deconnexion est immediate et rend la place de connexion.
func TestDeconnexionImmediateEtTerminer(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.utilisateur(t, "alice", false)
	ctx := context.Background()
	jeton, sess, _ := b.Connecter(ctx, "alice", mdp, "10.0.0.1")
	if err := b.Deconnecter(ctx, jeton); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Session(ctx, jeton); !errors.Is(err, domain.ErrSessionInvalide) {
		t.Fatalf("session encore valide : %v", err)
	}
	if ids := b.term.vus(); len(ids) != 1 || ids[0] != sess.ID {
		t.Fatalf("Terminer = %v", ids)
	}
	if err := b.Deconnecter(ctx, jeton); err != nil { // idempotent, sans second Terminer
		t.Fatal(err)
	}
	if len(b.term.vus()) != 1 {
		t.Fatal("double Terminer")
	}
}

func requeteDe(b *banc, methode, adresse string, mod func(*http.Request)) *http.Request {
	r := httptest.NewRequest(methode, "/x", nil)
	r.RemoteAddr = adresse
	if mod != nil {
		mod(r)
	}
	return r
}

// CA5 : refus par defaut ; session valide acceptee ; expiree ou revoquee refusee.
func TestAuthentifierRefusParDefaut(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.utilisateur(t, "alice", false)
	ctx := context.Background()
	if _, ok := b.Authentifier(requeteDe(b, "GET", "10.0.0.1:1", nil)); ok {
		t.Fatal("requete sans cookie acceptee")
	}
	avecCookie := func(j string) *http.Request {
		return requeteDe(b, "GET", "10.0.0.1:1", func(r *http.Request) {
			r.AddCookie(&http.Cookie{Name: b.nomCookie(), Value: j})
		})
	}
	if _, ok := b.Authentifier(avecCookie("n-importe-quoi")); ok {
		t.Fatal("jeton inconnu accepte")
	}
	jeton, sess, _ := b.Connecter(ctx, "alice", mdp, "10.0.0.1")
	if id, ok := b.Authentifier(avecCookie(jeton)); !ok || id != sess.ID {
		t.Fatalf("session valide refusee : %q %v", id, ok)
	}
	b.Deconnecter(ctx, jeton)
	if _, ok := b.Authentifier(avecCookie(jeton)); ok {
		t.Fatal("session revoquee acceptee")
	}
	jeton, _, _ = b.Connecter(ctx, "alice", mdp, "10.0.0.1")
	b.h.Avancer(b.cfg.DureeSession.Std() + time.Second)
	if _, ok := b.Authentifier(avecCookie(jeton)); ok {
		t.Fatal("session expiree acceptee")
	}
}

// CA5 : l'identifiant rendu au relais n'est jamais le jeton, et le role admin vient du compte.
func TestAdminEtIdentifiantDeSession(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.utilisateur(t, "root", true)
	b.utilisateur(t, "alice", false)
	ctx := context.Background()
	jr, _, _ := b.Connecter(ctx, "root", mdp, "10.0.0.1")
	ja, _, _ := b.Connecter(ctx, "alice", mdp, "10.0.0.1")
	req := func(j string) *http.Request {
		return requeteDe(b, "GET", "10.0.0.1:1", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: b.nomCookie(), Value: j}) })
	}
	if !b.EstAdmin(req(jr)) || b.EstAdmin(req(ja)) || b.EstAdmin(requeteDe(b, "GET", "10.0.0.1:1", nil)) {
		t.Fatal("role admin mal evalue")
	}
	// Proteger / ProtegerAdmin
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s, present := SessionDe(r.Context()); !present || s.UtilisateurID == "" {
			t.Error("session absente du contexte")
		}
		w.WriteHeader(204)
	})
	cas := []struct {
		h    http.Handler
		j    string
		code int
	}{
		{b.Proteger(ok), "", 401}, {b.Proteger(ok), ja, 204},
		{b.ProtegerAdmin(ok), "", 401}, {b.ProtegerAdmin(ok), ja, 403}, {b.ProtegerAdmin(ok), jr, 204},
	}
	for i, c := range cas {
		rec := httptest.NewRecorder()
		c.h.ServeHTTP(rec, req(c.j))
		if rec.Code != c.code {
			t.Errorf("cas %d : code %d, attendu %d", i, rec.Code, c.code)
		}
	}
}

// CA2 : la connexion HTTP repond 429 une fois le seuil atteint, 401 identique avant.
func TestConnexionHTTP429(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.utilisateur(t, "alice", false)
	essai := func(nom, corps, ct string) (int, string) {
		req := httptest.NewRequest("POST", "/connexion", strings.NewReader(corps))
		req.RemoteAddr = "198.51.100.4:5555"
		req.Header.Set("Content-Type", ct)
		rec := httptest.NewRecorder()
		b.Connexion(1024).ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	c1, m1 := essai("", `{"nom":"alice","mot_de_passe":"mauvais mot de passe"}`, "application/json")
	c2, m2 := essai("", `{"nom":"fantome","mot_de_passe":"mauvais mot de passe"}`, "application/json")
	if c1 != 401 || c2 != 401 || m1 != m2 {
		t.Fatalf("reponses distinguables : %d %q / %d %q", c1, m1, c2, m2)
	}
	for i := 0; i < 3; i++ {
		essai("", `{"nom":"alice","mot_de_passe":"x"}`, "application/json")
	}
	if c, _ := essai("", `{"nom":"alice","mot_de_passe":"`+mdp+`"}`, "application/json"); c != 429 {
		t.Fatalf("code %d, attendu 429", c)
	}
	if c, _ := essai("", `x`, "text/plain"); c != 415 {
		t.Fatalf("type incorrect : %d", c)
	}
}

func TestConnexionHTTPCorpsTropGrosEtInvalide(t *testing.T) {
	b := nouveauBanc(t, nil)
	for _, corps := range []string{strings.Repeat("a", 5000), `{"nom":`} {
		req := httptest.NewRequest("POST", "/connexion", strings.NewReader(corps))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		b.Connexion(100).ServeHTTP(rec, req)
		if rec.Code != 400 {
			t.Errorf("code %d", rec.Code)
		}
	}
}

// CA4 : le compte technique n'est accepte que depuis les plages configurees.
func TestCompteLocalLimiteAuxPlages(t *testing.T) {
	b := nouveauBanc(t, func(c *config.Comptes) {
		c.LocalUtilisateur = "local"
		c.PlagesLocales = []string{"192.168.50.0/24", "127.0.0.1"}
		c.ProxysFiables = []string{"10.9.9.9"}
	})
	cas := []struct {
		nom     string
		methode string
		adr     string
		xff     string
		attendu bool
	}{
		{"LAN", "GET", "192.168.50.20:4000", "", true},
		{"loopback", "GET", "127.0.0.1:4000", "", true},
		{"exterieur", "GET", "203.0.113.5:4000", "", false},
		{"ecriture refusee", "POST", "192.168.50.20:4000", "", false},
		{"HEAD", "HEAD", "192.168.50.20:4000", "", true},
		// Un proxy NON declare (127.0.0.1 devant un tunnel) : l'adresse apparente ne prouve rien.
		{"proxy non declare", "GET", "127.0.0.1:4000", "203.0.113.5", false},
		// Un client LAN qui forge X-Forwarded-For n'en tire rien.
		{"xff forge depuis le LAN", "GET", "192.168.50.20:4000", "192.168.50.21", false},
		{"proxy fiable, client externe", "GET", "10.9.9.9:4000", "203.0.113.5", false},
		{"proxy fiable, client LAN", "GET", "10.9.9.9:4000", "192.168.50.77", true},
		// Le client externe pretend etre du LAN en tete de liste : seul le saut de droite compte.
		{"proxy fiable, xff forge a gauche", "GET", "10.9.9.9:4000", "192.168.50.77, 203.0.113.5", false},
		{"proxy fiable, xff illisible", "GET", "10.9.9.9:4000", "n'importe quoi", false},
		{"pair illisible", "GET", "pas-une-adresse", "", false},
	}
	for _, c := range cas {
		r := requeteDe(b, c.methode, c.adr, func(r *http.Request) {
			if c.xff != "" {
				r.Header.Set("X-Forwarded-For", c.xff)
			}
		})
		id, ok := b.AuthentifierLecture(r)
		if ok != c.attendu || (ok && id != IDSessionLocale) {
			t.Errorf("%s : ok = %v (id %q), attendu %v", c.nom, ok, id, c.attendu)
		}
		// Le chemin « cookie obligatoire » ne l'accepte jamais, et il n'est jamais admin.
		if _, ok := b.Authentifier(r); ok {
			t.Errorf("%s : Authentifier accepte le compte local", c.nom)
		}
		if b.EstAdmin(r) {
			t.Errorf("%s : le compte local ne doit pas etre admin", c.nom)
		}
	}
}

func TestCompteLocalDesactiveParDefaut(t *testing.T) {
	b := nouveauBanc(t, nil)
	if _, ok := b.AuthentifierLecture(requeteDe(b, "GET", "127.0.0.1:1", nil)); ok {
		t.Fatal("compte local actif sans configuration")
	}
	b = nouveauBanc(t, func(c *config.Comptes) { c.LocalUtilisateur = "local" }) // sans plage
	if _, ok := b.AuthentifierLecture(requeteDe(b, "GET", "127.0.0.1:1", nil)); ok {
		t.Fatal("sans plage, le compte local doit rester ferme")
	}
}

func TestPlagesInvalidesRefusees(t *testing.T) {
	db, _ := OuvrirBase(filepath.Join(t.TempDir(), "c.db"))
	defer db.Close()
	c := configTest()
	c.PlagesLocales = []string{"pas une plage"}
	if _, err := Nouveau(context.Background(), Options{DB: db, Config: c}); err == nil {
		t.Fatal("plage invalide acceptee")
	}
}

// CA6 : plusieurs profils, isoles par compte ; choisir le profil d'autrui est refuse.
func TestProfils(t *testing.T) {
	b := nouveauBanc(t, nil)
	alice, bob := b.utilisateur(t, "alice", false), b.utilisateur(t, "bob", false)
	ctx := context.Background()
	p, err := b.CreerProfil(ctx, alice.ID, "Enfants")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.CreerProfil(ctx, alice.ID, "enfants"); !errors.Is(err, domain.ErrExiste) {
		t.Fatalf("doublon : %v", err)
	}
	if _, err := b.CreerProfil(ctx, "inconnu", "X"); !errors.Is(err, ErrIntrouvable) {
		t.Fatalf("utilisateur inconnu : %v", err)
	}
	if _, err := b.CreerProfil(ctx, bob.ID, "Enfants"); err != nil {
		t.Fatalf("meme nom chez un autre compte : %v", err)
	}
	ps, _ := b.Profils(ctx, alice.ID)
	if len(ps) != 2 || ps[0].Nom != "alice" || ps[1].ID != p.ID {
		t.Fatalf("profils : %+v", ps)
	}
	jeton, sess, _ := b.Connecter(ctx, "alice", mdp, "10.0.0.1")
	if err := b.ChoisirProfil(ctx, sess.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	if s, _ := b.Session(ctx, jeton); s.ProfilID != p.ID {
		t.Fatalf("profil actif = %q", s.ProfilID)
	}
	pb, _ := b.Profils(ctx, bob.ID)
	if err := b.ChoisirProfil(ctx, sess.ID, pb[0].ID); !errors.Is(err, ErrIntrouvable) {
		t.Fatalf("profil d'autrui accepte : %v", err)
	}
	// Suppression : pas celle d'autrui ; celle du profil actif vide le profil actif.
	if err := b.SupprimerProfil(ctx, alice.ID, pb[0].ID); !errors.Is(err, ErrIntrouvable) {
		t.Fatalf("suppression d'autrui : %v", err)
	}
	if err := b.SupprimerProfil(ctx, alice.ID, p.ID); err != nil {
		t.Fatal(err)
	}
	if s, _ := b.Session(ctx, jeton); s.ProfilID != "" {
		t.Fatalf("profil supprime encore actif : %q", s.ProfilID)
	}
	if ps, _ := b.Profils(ctx, alice.ID); len(ps) != 1 {
		t.Fatalf("profils restants : %+v", ps)
	}
}

// CA6 : changer le mot de passe revoque les AUTRES sessions (et rend leurs baux).
func TestChangementDeMotDePasseRevoqueLesAutresSessions(t *testing.T) {
	b := nouveauBanc(t, nil)
	alice := b.utilisateur(t, "alice", false)
	b.utilisateur(t, "bob", false)
	ctx := context.Background()
	j1, s1, _ := b.Connecter(ctx, "alice", mdp, "10.0.0.1")
	j2, s2, _ := b.Connecter(ctx, "alice", mdp, "10.0.0.2")
	jb, _, _ := b.Connecter(ctx, "bob", mdp, "10.0.0.3")

	if err := b.ChangerMotDePasse(ctx, alice.ID, "mauvais ancien", "nouveau mot de passe", s1.ID); !errors.Is(err, domain.ErrIdentifiantsInvalides) {
		t.Fatalf("ancien incorrect : %v", err)
	}
	if err := b.ChangerMotDePasse(ctx, alice.ID, mdp, "court", s1.ID); !errors.Is(err, ErrMotDePasseFaible) {
		t.Fatalf("nouveau trop court : %v", err)
	}
	if err := b.ChangerMotDePasse(ctx, alice.ID, mdp, "nouveau mot de passe", s1.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.Session(ctx, j1); err != nil {
		t.Fatalf("la session courante doit survivre : %v", err)
	}
	if _, err := b.Session(ctx, j2); !errors.Is(err, domain.ErrSessionInvalide) {
		t.Fatalf("l'autre session doit etre revoquee : %v", err)
	}
	if _, err := b.Session(ctx, jb); err != nil {
		t.Fatalf("session d'un autre compte touchee : %v", err)
	}
	if ids := b.term.vus(); len(ids) != 1 || ids[0] != s2.ID {
		t.Fatalf("Terminer = %v, attendu [%s]", ids, s2.ID)
	}
	if _, _, err := b.Connecter(ctx, "alice", mdp, "10.0.0.9"); !errors.Is(err, domain.ErrIdentifiantsInvalides) {
		t.Fatalf("ancien mot de passe encore valide : %v", err)
	}
	if _, _, err := b.Connecter(ctx, "alice", "nouveau mot de passe", "10.0.0.9"); err != nil {
		t.Fatalf("nouveau mot de passe : %v", err)
	}
}

// CA7 : ni le mot de passe ni le jeton dans les erreurs et les journaux.
func TestAucunSecretDansErreursEtJournaux(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.utilisateur(t, "alice", false)
	ctx := context.Background()
	secret := "SECRET-mot-de-passe-0451"
	var sortie []string
	_, _, e1 := b.Connecter(ctx, "alice", secret, "10.0.0.1")
	_, _, e2 := b.Connecter(ctx, "fantome", secret, "10.0.0.1")
	_, e3 := b.CreerUtilisateur(ctx, "alice", secret, false)
	_, e4 := b.CreerUtilisateur(ctx, "x", "c", false)
	e5 := b.ChangerMotDePasse(ctx, "id", secret, secret, "s")
	for _, e := range []error{e1, e2, e3, e4, e5} {
		if e != nil {
			sortie = append(sortie, e.Error())
		}
	}
	jeton, _, err := b.Connecter(ctx, "alice", mdp, "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	b.Session(ctx, jeton)
	b.Deconnecter(ctx, jeton)
	b.ChangerMotDePasse(ctx, "id", mdp, "nouveau mot de passe", "s")
	sortie = append(sortie, b.journal.String())
	// Reponses HTTP aussi
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/c", strings.NewReader(`{"nom":"alice","mot_de_passe":"`+secret+`"}`))
	req.Header.Set("Content-Type", "application/json")
	b.Connexion(1024).ServeHTTP(rec, req)
	sortie = append(sortie, rec.Body.String())
	tout := strings.Join(sortie, "\n")
	for _, s := range []string{secret, mdp, jeton, "nouveau mot de passe"} {
		if strings.Contains(tout, s) {
			t.Fatalf("secret %q retrouve dans la sortie :\n%s", s, tout)
		}
	}
	if !strings.Contains(b.journal.String(), "connexion") {
		t.Fatal("le journal devrait au moins tracer la connexion")
	}
}

func TestMotDePasseGeantRefuse(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.utilisateur(t, "alice", false)
	if _, _, err := b.Connecter(context.Background(), "alice", strings.Repeat("a", 100000), "10.0.0.1"); !errors.Is(err, domain.ErrIdentifiantsInvalides) {
		t.Fatalf("%v", err)
	}
	if _, err := b.CreerUtilisateur(context.Background(), "bob", strings.Repeat("a", 5000), false); !errors.Is(err, ErrMotDePasseFaible) {
		t.Fatalf("%v", err)
	}
}

// Injection SQL : les requetes sont parametrees.
func TestNomAvecSQLNeCasseRien(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.utilisateur(t, "alice", false)
	_, _, err := b.Connecter(context.Background(), "alice' OR '1'='1", "x", "10.0.0.1")
	if !errors.Is(err, domain.ErrIdentifiantsInvalides) {
		t.Fatalf("%v", err)
	}
	if _, err := b.Session(context.Background(), "' OR 1=1 --"); !errors.Is(err, domain.ErrSessionInvalide) {
		t.Fatalf("%v", err)
	}
}

func TestSaisieDuMotDePasse(t *testing.T) {
	b := nouveauBanc(t, nil)
	u, err := b.CreerDepuisEntree(context.Background(), "premier", true, strings.NewReader(mdp+"\r\nreste ignore\n"))
	if err != nil || !u.Admin {
		t.Fatalf("%+v %v", u, err)
	}
	if _, _, err := b.Connecter(context.Background(), "premier", mdp, "10.0.0.1"); err != nil {
		t.Fatalf("connexion avec le mot de passe lu : %v", err)
	}
	for _, in := range []string{"", "\n"} {
		if _, err := b.CreerDepuisEntree(context.Background(), "autre", false, strings.NewReader(in)); err == nil {
			t.Errorf("entree %q acceptee", in)
		}
	}
}

func TestPurger(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.utilisateur(t, "alice", false)
	b.Connecter(context.Background(), "alice", mdp, "10.0.0.1")
	b.h.Avancer(b.cfg.DureeSession.Std() + time.Second)
	if n := b.Purger(context.Background()); n != 1 {
		t.Fatalf("purgees : %d", n)
	}
}

// Sous -race : connexions, lectures et deconnexions concurrentes.
func TestConcurrence(t *testing.T) {
	b := nouveauBanc(t, func(c *config.Comptes) { c.Tentatives.Seuil = 1000 })
	for i := 0; i < 4; i++ {
		b.utilisateur(t, "u"+string(rune('a'+i)), false)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			nom := "u" + string(rune('a'+i%4))
			ctx := context.Background()
			j, _, err := b.Connecter(ctx, nom, mdp, "10.0.0.1")
			if err != nil {
				t.Error(err)
				return
			}
			if _, err := b.Session(ctx, j); err != nil {
				t.Error(err)
			}
			b.Deconnecter(ctx, j)
		}(i)
	}
	wg.Wait()
}
