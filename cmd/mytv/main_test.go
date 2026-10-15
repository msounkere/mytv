package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"mytv/internal/testutil/fauxiptv"
)

func ecrireConfig(t *testing.T, base, secret string) string {
	t.Helper()
	t.Setenv("MYTV_T_SIG", secret)
	t.Setenv("MYTV_T_U", "u")
	t.Setenv("MYTV_T_P", "p")
	d := t.TempDir()
	f := filepath.Join(d, "mytv.yaml")
	os.WriteFile(f, []byte(fmt.Sprintf(`
serveur: { ecoute: "127.0.0.1:0", secret_signature: { env: MYTV_T_SIG }, delai_arret: 3s }
stockage: { fichier: %q }
fournisseurs:
  - { id: a, type: xtream, base: %q, utilisateur: { env: MYTV_T_U }, mot_de_passe: { env: MYTV_T_P } }
journal: { niveau: warn }
`, filepath.Join(d, "m.db"), base)), 0o600)
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
			cfg := ecrireConfig(t, faux.URL(), secretOK)
			ctx, arreter := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
			defer arreter()
			var err bytes.Buffer
			fini := make(chan int, 1)
			go func() { fini <- executer(ctx, []string{"--config", cfg}, &err) }()

			// attendre que le service reponde : l'adresse est choisie par le systeme, on la
			// retrouve dans le journal d'erreur ? non (niveau warn) -> on laisse un delai court
			time.Sleep(700 * time.Millisecond)
			select {
			case code := <-fini:
				t.Fatalf("termine trop tot : %d %s", code, err.String())
			default:
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
