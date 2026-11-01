package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"mytv/internal/comptes"
	"mytv/internal/config"
	"mytv/internal/domain"
	"mytv/internal/profil"
	"mytv/internal/stockage"
	"mytv/internal/testutil/fauxiptv"
)

func ecrireConfig(t *testing.T, base, secret string) string {
	return ecrireConfigEcoute(t, base, secret, "127.0.0.1:0")
}

// portLibre reserve un port local puis le rend : le service ecoutera dessus, et le test
// pourra l'interroger (la configuration ne sait pas annoncer un port choisi par le systeme).
func portLibre(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	adresse := l.Addr().String()
	l.Close()
	return adresse
}

func ecrireConfigEcoute(t *testing.T, base, secret, ecoute string) string {
	t.Helper()
	t.Setenv("MYTV_T_SIG", secret)
	t.Setenv("MYTV_T_U", "u")
	t.Setenv("MYTV_T_P", "p")
	d := t.TempDir()
	f := filepath.Join(d, "mytv.yaml")
	os.WriteFile(f, []byte(fmt.Sprintf(`
serveur: { ecoute: "%s", secret_signature: { env: MYTV_T_SIG }, delai_arret: 3s }
stockage: { fichier: %q }
fournisseurs:
  - { id: a, type: xtream, base: %q, utilisateur: { env: MYTV_T_U }, mot_de_passe: { env: MYTV_T_P } }
journal: { niveau: warn }
diagnostic: { dossier: %q }
`, ecoute, filepath.Join(d, "m.db"), base, filepath.Join(d, "diag"))), 0o600)
	return f
}

const secretOK = "0123456789abcdef0123456789abcdef"

func TestVerifierConfig(t *testing.T) {
	var err bytes.Buffer
	if code := executer(context.Background(), []string{"--config", ecrireConfig(t, "http://f.test", secretOK), "--verifier-config"}, &err); code != 0 {
		t.Fatalf("config valide refusee : %d %s", code, err.String())
	}
	err.Reset()
	cfg := ecrireConfig(t, "http://f.test", "court")
	if code := executer(context.Background(), []string{"--config", cfg, "--verifier-config"}, &err); code != 1 || !strings.Contains(err.String(), "secret_signature") {
		t.Fatalf("secret court : %d %s", code, err.String())
	}
	if strings.Contains(err.String(), "court\n") && strings.Contains(err.String(), "= court") {
		t.Fatal("la valeur du secret fuit")
	}
	err.Reset()
	if code := executer(context.Background(), []string{"--config", "/inexistant.yaml"}, &err); code != 1 {
		t.Fatalf("fichier absent : %d", code)
	}
	if code := executer(context.Background(), []string{"--inconnu"}, io.Discard); code != 2 {
		t.Fatalf("option inconnue : %d", code)
	}
}

// Le demarrage echoue avec TOUTES les erreurs (secrets absents, secret court).
func TestDemarrageEchoueAvecToutesLesErreurs(t *testing.T) {
	cfg := ecrireConfig(t, "http://f.test", "court")
	t.Setenv("MYTV_T_U", "")
	t.Setenv("MYTV_T_P", "")
	var err bytes.Buffer
	if code := executer(context.Background(), []string{"--config", cfg}, &err); code != 1 {
		t.Fatalf("code %d", code)
	}
	for _, m := range []string{"secret_signature", "MYTV_T_U", "MYTV_T_P"} {
		if !strings.Contains(err.String(), m) {
			t.Errorf("%q absent de :\n%s", m, err.String())
		}
	}
}

// SIGINT et SIGTERM arretent proprement, comme main les recoit (NotifyContext).
func TestArretSurSignal(t *testing.T) {
	for _, sig := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		t.Run(sig.String(), func(t *testing.T) {
			faux := fauxiptv.Nouveau(t, fauxiptv.Options{Utilisateur: "u", MotDePasse: "p"})
			ecoute := portLibre(t)
			cfg := ecrireConfigEcoute(t, faux.URL(), secretOK, ecoute)
			ctx, arreter := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer arreter()
			var err bytes.Buffer
			fini := make(chan int, 1)
			go func() { fini <- executer(ctx, []string{"--config", cfg}, &err) }()

			// Attendre que le service REPONDE avant de le signaler : un delai fixe prenait, sur
			// un executeur lent, un demarrage interrompu (migration des comptes) pour un arret rate.
			pret := false
			for debut := time.Now(); time.Since(debut) < 20*time.Second && !pret; {
				select {
				case code := <-fini:
					t.Fatalf("termine trop tot : %d %s", code, err.String())
				default:
				}
				if rep, e := http.Get("http://" + ecoute + "/api/v1/sante"); e == nil {
					rep.Body.Close()
					pret = rep.StatusCode == http.StatusOK
				}
				if !pret {
					time.Sleep(50 * time.Millisecond)
				}
			}
			if !pret {
				t.Fatalf("le service ne repond pas apres 20 s : %s", err.String())
			}
			if e := syscall.Kill(os.Getpid(), sig); e != nil {
				t.Fatal(e)
			}
			select {
			case code := <-fini:
				if code != 0 {
					t.Fatalf("code %d : %s", code, err.String())
				}
			case <-time.After(10 * time.Second):
				t.Fatal("pas d'arret apres le signal")
			}
		})
	}
}

// ---------------------------------------------------------------- US-26 : sauvegarde et restauration

// US-26 CA2 : `mytv --sauvegarder` ecrit une copie coherente d'une base VRAIE (comptes, profils,
// favoris, historique, reglages, catalogue) ; la procedure de restauration (copier le fichier a la
// place de la base, sans les -wal/-shm) donne une base neuve ou tout est identique et le mot de
// passe fonctionne encore.
func TestSauvegarderPuisRestaurerDansUneBaseNeuve(t *testing.T) {
	ctx := context.Background()
	rep := t.TempDir()
	base := filepath.Join(rep, "m.db")
	cfgFichier := filepath.Join(rep, "mytv.yaml")
	t.Setenv("MYTV_T_SIG", secretOK)
	os.WriteFile(cfgFichier, []byte(fmt.Sprintf(`
serveur: { ecoute: "127.0.0.1:0", secret_signature: { env: MYTV_T_SIG } }
stockage: { fichier: %q }
fournisseurs:
  - { id: a, type: xtream, base: "http://f.test", utilisateur: { env: MYTV_T_U }, mot_de_passe: { env: MYTV_T_P } }
`, base)), 0o600)

	// --- une base vivante, ouverte par les memes modules que le service.
	st, err := stockage.Ouvrir(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Fermer()
	err = st.EcrireCatalogue(ctx, &domain.Instantane{FournisseurID: "a", Pris: time.Now(),
		Categories: []domain.Categorie{{ID: "1", FournisseurID: "a", Type: domain.TypeLive, Nom: "Cat"}},
		Chaines:    []domain.Chaine{{ID: "a:1", FournisseurID: "a", Numero: "1", Nom: "Une", CategorieID: "1"}, {ID: "a:2", FournisseurID: "a", Numero: "2", Nom: "Deux", CategorieID: "1"}}})
	if err != nil {
		t.Fatal(err)
	}
	pr, err := profil.Ouvrir(ctx, base, profil.Options{Catalogue: st, HistoriqueMax: 10, FavorisMax: 10, ReglagesMax: 1000})
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Fermer()
	db, err := comptes.OuvrirBase(base)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	cc := config.Defauts().Comptes
	cc.Argon2.MemoireKio, cc.Argon2.Iterations, cc.Argon2.Parallelisme = 19*1024, 2, 1
	svc, err := comptes.Nouveau(ctx, comptes.Options{DB: db, Config: cc})
	if err != nil {
		t.Fatal(err)
	}
	const mdp = "un mot de passe bien solide"
	alice, err := svc.CreerUtilisateur(ctx, "alice", mdp, true)
	if err != nil {
		t.Fatal(err)
	}
	p, err := svc.CreerProfil(ctx, alice.ID, "Salon")
	if err != nil {
		t.Fatal(err)
	}
	profsAvant, err := svc.Profils(ctx, alice.ID)
	if err != nil {
		t.Fatal(err)
	}
	vu := time.Date(2026, 10, 1, 20, 0, 0, 0, time.UTC)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(pr.AjouterFavori(ctx, p.ID, "a:2"))
	must(pr.EnregistrerLecture(ctx, p.ID, domain.Lecture{Contenu: "a:1", Position: 90 * time.Second, Duree: 3600 * time.Second, Vu: vu}))
	_, err = pr.EcrireReglages(ctx, p.ID, map[string]string{"langue": "fr"}, 0)
	must(err)

	// --- la sauvegarde, service "en marche" (les connexions ci-dessus restent ouvertes).
	dest := filepath.Join(rep, "copie.db")
	var sortie bytes.Buffer
	if code := executer(ctx, []string{"--config", cfgFichier, "--sauvegarder", dest}, &sortie); code != 0 || !strings.Contains(sortie.String(), "integrity_check ok") {
		t.Fatalf("sauvegarde : %d %s", code, sortie.String())
	}
	// Elle ne ressemble pas a la base : pas de -wal a restaurer a cote, droits prives.
	if st, err := os.Stat(dest); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("droits : %v %v", st, err)
	}
	// Ecraser une sauvegarde est refuse, la base n'est jamais creee par la commande.
	sortie.Reset()
	if code := executer(ctx, []string{"--config", cfgFichier, "--sauvegarder", dest}, &sortie); code != 1 || !strings.Contains(sortie.String(), "existe deja") {
		t.Errorf("ecrasement : %d %s", code, sortie.String())
	}
	sortie.Reset()
	os.WriteFile(cfgFichier, []byte(strings.ReplaceAll(mustLire(t, cfgFichier), base, filepath.Join(rep, "absente.db"))), 0o600)
	if code := executer(ctx, []string{"--config", cfgFichier, "--sauvegarder", filepath.Join(rep, "autre.db")}, &sortie); code != 1 {
		t.Errorf("base absente : %d %s", code, sortie.String())
	}
	if _, err := os.Stat(filepath.Join(rep, "absente.db")); err == nil {
		t.Error("la commande a cree une base")
	}

	// --- restauration : la copie devient la base d'un nouveau dossier ; ouverte par les modules.
	neuf := filepath.Join(t.TempDir(), "mytv.db")
	b, err := os.ReadFile(dest)
	must(err)
	must(os.WriteFile(neuf, b, 0o600))
	st2, err := stockage.Ouvrir(ctx, neuf)
	must(err)
	defer st2.Fermer()
	pr2, err := profil.Ouvrir(ctx, neuf, profil.Options{Catalogue: st2, HistoriqueMax: 10, FavorisMax: 10, ReglagesMax: 1000})
	must(err)
	defer pr2.Fermer()
	db2, err := comptes.OuvrirBase(neuf)
	must(err)
	defer db2.Close()
	svc2, err := comptes.Nouveau(ctx, comptes.Options{DB: db2, Config: cc})
	must(err)

	if _, s, err := svc2.Connecter(ctx, "alice", mdp, "192.0.2.1"); err != nil || !s.Admin {
		t.Fatalf("le mot de passe de alice ne marche plus apres restauration : %v", err)
	}
	profs, err := svc2.Profils(ctx, alice.ID)
	if err != nil || !reflect.DeepEqual(profs, profsAvant) || len(profs) != 2 {
		t.Errorf("profils : %v (avant : %v) %v", profs, profsAvant, err)
	}
	fav, _ := pr2.Favoris(ctx, p.ID)
	if len(fav) != 1 || fav[0] != "a:2" {
		t.Errorf("favoris : %v", fav)
	}
	h, _ := pr2.Historique(ctx, p.ID, 0)
	if len(h) != 1 || h[0].Contenu != "a:1" || h[0].Position != 90*time.Second || h[0].Duree != 3600*time.Second || !h[0].Vu.Equal(vu) {
		t.Errorf("historique : %+v", h)
	}
	r, _ := pr2.Reglages(ctx, p.ID)
	if r.Valeurs["langue"] != "fr" {
		t.Errorf("reglages : %+v", r)
	}
	ch, _ := st2.Chaines(ctx, domain.FiltreChaines{})
	if len(ch) != 2 {
		t.Errorf("catalogue : %d chaines", len(ch))
	}
}

func mustLire(t *testing.T, f string) string {
	t.Helper()
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// US-26 CA3 : --verifier-config signale l'ecoute sur toutes les interfaces sans proxy ni plage
// locale, sans refuser de demarrer (code 0).
func TestVerifierConfigAvertitPourUneEcouteSurToutesLesInterfaces(t *testing.T) {
	cfg := ecrireConfig(t, "http://f.test", secretOK)
	b, _ := os.ReadFile(cfg)
	os.WriteFile(cfg, []byte(strings.Replace(string(b), "127.0.0.1:0", "0.0.0.0:8080", 1)), 0o600)
	var err bytes.Buffer
	if code := executer(context.Background(), []string{"--config", cfg, "--verifier-config"}, &err); code != 0 {
		t.Fatalf("l'option doit rester possible : %d %s", code, err.String())
	}
	if !strings.Contains(err.String(), "avertissement") || !strings.Contains(err.String(), "proxys_fiables") || !strings.Contains(err.String(), "configuration valide") {
		t.Errorf("sortie :\n%s", err.String())
	}
	err.Reset()
	b2 := strings.Replace(string(b), "journal:", "comptes: { proxys_fiables: [\"10.0.0.1\"] }\njournal:", 1)
	os.WriteFile(cfg, []byte(strings.Replace(b2, "127.0.0.1:0", "0.0.0.0:8080", 1)), 0o600)
	executer(context.Background(), []string{"--config", cfg, "--verifier-config"}, &err)
	if strings.Contains(err.String(), "avertissement") {
		t.Errorf("avertissement alors qu'un proxy est declare :\n%s", err.String())
	}
	if code := executer(context.Background(), []string{"--config", cfg, "--verifier-config", "--sauvegarder", "x.db"}, io.Discard); code != 2 {
		t.Errorf("options incompatibles : %d", code)
	}
}
