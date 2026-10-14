package basesqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

var migs = []Migration{
	{1, "t1", `CREATE TABLE a (x INTEGER) STRICT`},
	{2, "t2", `CREATE TABLE b (y INTEGER) STRICT`},
}

func maintenant() time.Time { return time.Unix(1700000000, 0) }

func TestMigrationsRejouablesEtIndependantes(t *testing.T) {
	ctx := context.Background()
	chemin := filepath.Join(t.TempDir(), "x.db")
	b, err := Ouvrir(ctx, chemin, "un", migs, maintenant)
	if err != nil {
		t.Fatal(err)
	}
	b.Close()
	// Rejouee : aucune erreur, aucune migration en double.
	b, err = Ouvrir(ctx, chemin, "un", migs, maintenant)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	var n int
	if err := b.QueryRow(`SELECT COUNT(*) FROM migrations_un`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("migrations = %d, %v", n, err)
	}
	// Un autre jeu dans le meme fichier a sa propre table de versions.
	b2, err := Ouvrir(ctx, chemin, "deux", []Migration{{1, "autre", `CREATE TABLE c (z INTEGER) STRICT`}}, maintenant)
	if err != nil {
		t.Fatal(err)
	}
	b2.Close()
}

func TestBaseTropRecenteRefusee(t *testing.T) {
	ctx := context.Background()
	chemin := filepath.Join(t.TempDir(), "x.db")
	b, _ := Ouvrir(ctx, chemin, "un", migs, maintenant)
	b.Close()
	if _, err := Ouvrir(ctx, chemin, "un", migs[:1], maintenant); err == nil {
		t.Fatal("une base plus recente que le programme doit etre refusee")
	}
}

func TestNomDeJeuInvalide(t *testing.T) {
	if _, err := Ouvrir(context.Background(), filepath.Join(t.TempDir(), "x.db"), "a;drop", nil, maintenant); err == nil {
		t.Fatal("nom de jeu invalide accepte")
	}
}
