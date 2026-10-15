package stockage

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"mytv/internal/domain"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// CA2 : la migration 4 s'applique sur une base existante en version 3 sans rien perdre.
func TestMigrationDetailsSurBaseExistante(t *testing.T) {
	ctx := context.Background()
	chemin := filepath.Join(t.TempDir(), "v3.db")
	db, err := sql.Open("sqlite", "file:"+chemin)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE migrations (version INTEGER PRIMARY KEY, nom TEXT NOT NULL, appliquee INTEGER NOT NULL) STRICT`); err != nil {
		t.Fatal(err)
	}
	for _, m := range migrations[:3] {
		if _, err := db.Exec(m.sql); err != nil {
			t.Fatal(err)
		}
		db.Exec(`INSERT INTO migrations VALUES (?, ?, 0)`, m.version, m.nom)
	}
	db.Exec(`INSERT INTO films (id, fournisseur_id, numero, nom, image, categorie_id, extension, note, annee) VALUES ('a:1','a','1','Avant','','1','mkv',0,'')`)
	db.Close()

	b, err := Ouvrir(ctx, chemin)
	if err != nil {
		t.Fatalf("migration depuis la v3 : %v", err)
	}
	defer b.Fermer()
	if _, err := b.Film(ctx, "a:1"); err != nil {
		t.Errorf("film perdu par la migration : %v", err)
	}
	if err := b.EcrireDetailsFilm(ctx, "a:1", domain.DetailsFilm{Details: domain.Details{Synopsis: "x"}}, t0); err != nil {
		t.Errorf("table des details absente : %v", err)
	}
}

func filmDetails(synopsis, genre string, duree time.Duration) domain.DetailsFilm {
	return domain.DetailsFilm{Details: domain.Details{Synopsis: synopsis, Genre: genre, ImageFond: []string{"https://i.test/f.jpg"}, Note: 7.5}, Duree: duree}
}

// CA2 : ecrire puis relire, avec la date de recuperation ; absent = ErrNonTrouve.
func TestDetailsFilmEcritureLecture(t *testing.T) {
	ctx := context.Background()
	b := ouvrirTest(t)
	if _, _, err := b.DetailsFilm(ctx, "a:1"); !errors.Is(err, domain.ErrNonTrouve) {
		t.Fatalf("absent : %v", err)
	}
	if err := b.EcrireDetailsFilm(ctx, "a:1", filmDetails("Synopsis", "Drame", 90*time.Minute), t0); err != nil {
		t.Fatal(err)
	}
	d, quand, err := b.DetailsFilm(ctx, "a:1")
	if err != nil || d.Synopsis != "Synopsis" || d.Genre != "Drame" || d.Duree != 90*time.Minute || d.Note != 7.5 ||
		len(d.ImageFond) != 1 || !quand.Equal(t0) {
		t.Fatalf("%+v %v %v", d, quand, err)
	}
	// Un film et une serie de meme identifiant ne se melangent pas (types distincts).
	if _, _, err := b.DetailsSerie(ctx, "a:1"); !errors.Is(err, domain.ErrNonTrouve) {
		t.Errorf("serie lue dans un film : %v", err)
	}
}

// CA2 : un fournisseur sans info (details vides) ou qui n'a rempli qu'une partie ne
// vide JAMAIS ce qui est deja stocke.
func TestDetailsNeSontJamaisAppauvris(t *testing.T) {
	ctx := context.Background()
	b := ouvrirTest(t)
	if err := b.EcrireDetailsFilm(ctx, "a:1", filmDetails("Synopsis", "Drame", 90*time.Minute), t0); err != nil {
		t.Fatal(err)
	}
	// Details vides : rien ne change, pas meme la date.
	if err := b.EcrireDetailsFilm(ctx, "a:1", domain.DetailsFilm{}, t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	d, quand, _ := b.DetailsFilm(ctx, "a:1")
	if d.Synopsis != "Synopsis" || !quand.Equal(t0) {
		t.Fatalf("vide a ecrase : %+v %v", d, quand)
	}
	// Fiche partielle (genre seul) : le genre est mis a jour, le reste est conserve.
	if err := b.EcrireDetailsFilm(ctx, "a:1", domain.DetailsFilm{Details: domain.Details{Genre: "Thriller"}}, t0.Add(2*time.Hour)); err != nil {
		t.Fatal(err)
	}
	d, quand, _ = b.DetailsFilm(ctx, "a:1")
	if d.Genre != "Thriller" || d.Synopsis != "Synopsis" || d.Duree != 90*time.Minute || len(d.ImageFond) != 1 || d.Note != 7.5 || !quand.Equal(t0.Add(2*time.Hour)) {
		t.Fatalf("fusion : %+v %v", d, quand)
	}
}

// CA2 : les details survivent a la resynchronisation du catalogue (table distincte,
// EcrireCatalogue remplace films et series).
func TestDetailsSurvivrentALaResynchronisation(t *testing.T) {
	ctx := context.Background()
	b := ouvrirTest(t)
	inst := func() *domain.Instantane {
		return &domain.Instantane{FournisseurID: "a", Pris: t0,
			Films:  []domain.Film{{ID: "a:1", FournisseurID: "a", Numero: "1", Nom: "F"}},
			Series: []domain.SerieTV{{ID: "a:s7", FournisseurID: "a", Numero: "7", Nom: "S"}}}
	}
	if err := b.EcrireCatalogue(ctx, inst()); err != nil {
		t.Fatal(err)
	}
	b.EcrireDetailsFilm(ctx, "a:1", filmDetails("Synopsis", "", 0), t0)
	b.EcrireDetailsSerie(ctx, "a:s7", domain.DetailsSerie{Details: domain.Details{Synopsis: "Serie"}, DureeEpisode: 40 * time.Minute}, t0)
	if err := b.EcrireCatalogue(ctx, inst()); err != nil {
		t.Fatal(err)
	}
	if d, _, err := b.DetailsFilm(ctx, "a:1"); err != nil || d.Synopsis != "Synopsis" {
		t.Errorf("film : %+v %v", d, err)
	}
	if d, _, err := b.DetailsSerie(ctx, "a:s7"); err != nil || d.Synopsis != "Serie" || d.DureeEpisode != 40*time.Minute {
		t.Errorf("serie : %+v %v", d, err)
	}
}

// CA2 : des ecritures concurrentes sur le meme contenu ne perdent aucun champ.
func TestDetailsEcrituresConcurrentes(t *testing.T) {
	ctx := context.Background()
	b := ouvrirTest(t)
	champs := []domain.DetailsFilm{
		{Details: domain.Details{Synopsis: "S"}}, {Details: domain.Details{Genre: "G"}},
		{Details: domain.Details{Casting: "C"}}, {Details: domain.Details{Realisateur: "R"}},
	}
	done := make(chan error, len(champs))
	for _, c := range champs {
		go func() { done <- b.EcrireDetailsFilm(ctx, "a:1", c, t0) }()
	}
	for range champs {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	d, _, _ := b.DetailsFilm(ctx, "a:1")
	if d.Synopsis != "S" || d.Genre != "G" || d.Casting != "C" || d.Realisateur != "R" {
		t.Fatalf("champ perdu : %+v", d)
	}
}

// CA2 : la purge supprime les vieux details et garde les recents.
func TestPurgerDetails(t *testing.T) {
	ctx := context.Background()
	b := ouvrirTest(t)
	b.EcrireDetailsFilm(ctx, "a:1", filmDetails("vieux", "", 0), t0.Add(-100*24*time.Hour))
	b.EcrireDetailsFilm(ctx, "a:2", filmDetails("neuf", "", 0), t0)
	n, err := b.PurgerDetails(ctx, t0.Add(-90*24*time.Hour))
	if err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	if _, _, err := b.DetailsFilm(ctx, "a:1"); !errors.Is(err, domain.ErrNonTrouve) {
		t.Errorf("vieux conserve : %v", err)
	}
	if _, _, err := b.DetailsFilm(ctx, "a:2"); err != nil {
		t.Errorf("recent supprime : %v", err)
	}
}
