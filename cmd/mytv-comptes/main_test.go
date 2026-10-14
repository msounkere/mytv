package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mytv/internal/comptes"
	"mytv/internal/config"
)

func ecrireConfig(t *testing.T) (cfg, base string) {
	d := t.TempDir()
	base = filepath.Join(d, "m.db")
	cfg = filepath.Join(d, "c.yaml")
	os.WriteFile(cfg, []byte(`
stockage: { fichier: `+base+` }
serveur: { secret_signature: { env: T_S } }
fournisseurs:
  - { id: a, type: xtream, base: "http://f.test", utilisateur: { env: T_U }, mot_de_passe: { env: T_P } }
`), 0o600)
	return
}

// Le premier utilisateur se cree sans mot de passe en ligne de commande, puis se connecte.
func TestCreerPremierUtilisateurDepuisStdinEtFichier(t *testing.T) {
	cfg, base := ecrireConfig(t)
	var out, errb bytes.Buffer
	const mdp = "un-mot-de-passe-solide"
	if code := executer([]string{"creer", "--config", cfg, "--nom", "alice", "--admin"}, strings.NewReader(mdp+"\n"), &out, &errb); code != 0 {
		t.Fatalf("code %d : %s", code, errb.String())
	}
	f := filepath.Join(t.TempDir(), "mdp")
	os.WriteFile(f, []byte(mdp+"\n"), 0o600)
	if code := executer([]string{"creer", "--config", cfg, "--nom", "bob", "--mot-de-passe-fichier", f}, nil, &out, &errb); code != 0 {
		t.Fatalf("code %d : %s", code, errb.String())
	}
	if strings.Contains(out.String()+errb.String(), mdp) {
		t.Fatal("le mot de passe ne doit apparaitre dans aucune sortie")
	}
	c, _ := config.Charger(cfg)
	db, _ := comptes.OuvrirBase(base)
	defer db.Close()
	svc, err := comptes.Nouveau(context.Background(), comptes.Options{DB: db, Config: c.Comptes})
	if err != nil {
		t.Fatal(err)
	}
	if _, s, err := svc.Connecter(context.Background(), "alice", mdp, "10.0.0.1"); err != nil || !s.Admin {
		t.Fatalf("connexion : %+v %v", s, err)
	}
	if _, s, err := svc.Connecter(context.Background(), "bob", mdp, "10.0.0.1"); err != nil || s.Admin {
		t.Fatalf("connexion : %+v %v", s, err)
	}
}

func TestUsageEtErreurs(t *testing.T) {
	cfg, _ := ecrireConfig(t)
	var out, errb bytes.Buffer
	for _, a := range [][]string{{}, {"autre"}, {"creer", "--config", cfg}, {"creer", "--config", "/inexistant", "--nom", "x"}} {
		if executer(a, strings.NewReader("mot de passe long\n"), &out, &errb) == 0 {
			t.Errorf("%v devrait echouer", a)
		}
	}
	if executer([]string{"creer", "--config", cfg, "--nom", "x"}, strings.NewReader("\n"), &out, &errb) == 0 {
		t.Error("mot de passe vide accepte")
	}
}
