package stockage

import (
	"context"
	"fmt"
	"testing"
	"time"

	"mytv/internal/domain"
)

// catalogueExport : deux categories par type, dans un ordre de fournisseur
// volontairement different de l'ordre alphabetique des noms.
func catalogueExport(f string, pris time.Time, nChaines, nFilms, nSeries int) *domain.Instantane {
	s := &domain.Instantane{FournisseurID: f, Pris: pris}
	for _, t := range []domain.Type{domain.TypeLive, domain.TypeFilm, domain.TypeSerie} {
		s.Categories = append(s.Categories,
			domain.Categorie{ID: "2", FournisseurID: f, Type: t, Nom: "Zeta", Ordre: 1},
			domain.Categorie{ID: "1", FournisseurID: f, Type: t, Nom: "Alpha", Ordre: 2})
	}
	aj := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < nChaines; i++ {
		c := chaine(f, 1000+i, fmt.Sprintf("Chaine %d", i), fmt.Sprint(1+i%2), nChaines-i) // ordre inverse du numero
		c.Ajoute = aj
		s.Chaines = append(s.Chaines, c)
	}
	for i := 0; i < nFilms; i++ {
		s.Films = append(s.Films, domain.Film{ID: domain.ContenuID(fmt.Sprintf("%s:%d", f, 2000+i)), FournisseurID: f,
			Numero: fmt.Sprint(2000 + i), Nom: fmt.Sprintf("Film %d", i), CategorieID: fmt.Sprint(1 + i%2), Extension: "mkv", Note: 7.5, Annee: "2020", Ajoute: aj})
	}
	for i := 0; i < nSeries; i++ {
		s.Series = append(s.Series, domain.SerieTV{ID: domain.ContenuID(fmt.Sprintf("%s:%d", f, 3000+i)), FournisseurID: f,
			Numero: fmt.Sprint(3000 + i), Nom: fmt.Sprintf("Serie %d", i), CategorieID: fmt.Sprint(1 + i%2), Annee: "2019", Ajoute: aj})
	}
	return s
}

func exporter(t *testing.T, b *Base, typ domain.TypeExport, live string) (n int, lignes []any) {
	t.Helper()
	e, err := b.OuvrirExport(context.Background(), typ, live)
	if err != nil {
		t.Fatal(err)
	}
	defer e.Fermer()
	if err := e.Lire(func(v any) error { lignes = append(lignes, v); return nil }); err != nil {
		t.Fatal(err)
	}
	return e.Elements(), lignes
}

// US-16 CA1 : le nombre annonce est le nombre de lignes, l'ordre est
// fournisseur / categorie / ordre du fournisseur, et ajoute survit au stockage.
func TestCA1_ExportOrdreEtEffectif(t *testing.T) {
	b := ouvrirTest(t)
	ctx := context.Background()
	if err := b.EcrireCatalogue(ctx, catalogueExport("a", time.Now(), 6, 5, 4)); err != nil {
		t.Fatal(err)
	}
	n, l := exporter(t, b, domain.ExportLive, "")
	if n != 6 || len(l) != 6 {
		t.Fatalf("live : annonce %d, recu %d", n, len(l))
	}
	// Categorie 1 d'abord ; dans une categorie, l'ordre du fournisseur (champ ordre decroissant en numero).
	var ids []string
	for _, v := range l {
		c := v.(domain.Chaine)
		ids = append(ids, fmt.Sprintf("%s/%s", c.CategorieID, c.Numero))
		if c.Ajoute.IsZero() {
			t.Errorf("ajoute perdu pour %s", c.ID)
		}
	}
	attendu := []string{"1/1004", "1/1002", "1/1000", "2/1005", "2/1003", "2/1001"}
	if fmt.Sprint(ids) != fmt.Sprint(attendu) {
		t.Errorf("ordre live = %v, attendu %v", ids, attendu)
	}
	if n, l := exporter(t, b, domain.ExportVOD, ""); n != 5 || len(l) != 5 {
		t.Errorf("vod : %d / %d", n, len(l))
	}
	n, l = exporter(t, b, domain.ExportSeries, "")
	if n != 4 || len(l) != 4 || l[0].(domain.SerieTV).Ajoute.IsZero() {
		t.Errorf("series : %d / %d (ajoute %v)", n, len(l), l[0])
	}
	// Categories des trois types, ordre du fournisseur dans chaque type.
	n, l = exporter(t, b, domain.ExportCategories, "")
	if n != 6 || len(l) != 6 {
		t.Fatalf("categories : %d / %d", n, len(l))
	}
	if c := l[0].(domain.Categorie); c.Type != domain.TypeLive || c.Nom != "Zeta" {
		t.Errorf("premiere categorie = %+v", c)
	}
	if _, err := b.OuvrirExport(ctx, "autre", ""); err == nil {
		t.Error("type d'export inconnu accepte")
	}
}

// Le direct est mono-fournisseur : seules les chaines (et categories live) de ce
// fournisseur sont exportees ; films et series de tous.
func TestExportRestreintLeDirectAuFournisseurDuDirect(t *testing.T) {
	b := ouvrirTest(t)
	ctx := context.Background()
	for _, f := range []string{"a", "b"} {
		if err := b.EcrireCatalogue(ctx, catalogueExport(f, time.Now(), 3, 2, 1)); err != nil {
			t.Fatal(err)
		}
	}
	if n, l := exporter(t, b, domain.ExportLive, "b"); n != 3 || len(l) != 3 || l[0].(domain.Chaine).FournisseurID != "b" {
		t.Errorf("live restreint : %d / %d", n, len(l))
	}
	if n, _ := exporter(t, b, domain.ExportVOD, "b"); n != 4 {
		t.Errorf("films de tous : %d", n)
	}
	// 2 categories live de b + 4 vod + 4 series.
	if n, _ := exporter(t, b, domain.ExportCategories, "b"); n != 2+4+4 {
		t.Errorf("categories : %d", n)
	}
	m, err := b.Manifeste(ctx, "b")
	if err != nil {
		t.Fatal(err)
	}
	if m.Chaines != 3 || m.Films != 4 || m.Series != 2 || m.Categories != 10 || m.Synchronise.IsZero() {
		t.Errorf("manifeste = %+v", m)
	}
}

// US-16 CA1 : une synchronisation qui committe PENDANT l'export ne se melange
// pas : l'export voit l'instantane d'avant, de bout en bout, et ne bloque pas
// l'ecriture.
func TestCA1_ExportIsoleDUneResynchronisation(t *testing.T) {
	b := ouvrirTest(t)
	ctx := context.Background()
	if err := b.EcrireCatalogue(ctx, catalogueExport("a", time.Now(), 50, 0, 0)); err != nil {
		t.Fatal(err)
	}
	e, err := b.OuvrirExport(ctx, domain.ExportLive, "")
	if err != nil {
		t.Fatal(err)
	}
	defer e.Fermer()
	vu := 0
	err = e.Lire(func(v any) error {
		if vu == 10 { // au milieu du parcours, une synchro remplace tout
			fin := make(chan error, 1)
			go func() { fin <- b.EcrireCatalogue(ctx, catalogueExport("a", time.Now(), 7, 0, 0)) }()
			select {
			case err := <-fin:
				if err != nil {
					return err
				}
			case <-time.After(5 * time.Second):
				return fmt.Errorf("l'ecriture est bloquee par la lecture en cours")
			}
		}
		if c := v.(domain.Chaine); c.Nom == "" {
			return fmt.Errorf("ligne vide")
		}
		vu++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if vu != e.Elements() || vu != 50 {
		t.Errorf("l'export a vu %d lignes pour %d annoncees (50 attendues)", vu, e.Elements())
	}
	if n, _ := exporter(t, b, domain.ExportLive, ""); n != 7 {
		t.Errorf("apres synchro : %d", n)
	}
}

// Une erreur d'emettre arrete le parcours et la connexion est rendue au pool.
func TestExportErreurDEmissionEtFermeture(t *testing.T) {
	b := ouvrirTest(t)
	ctx := context.Background()
	if err := b.EcrireCatalogue(ctx, catalogueExport("a", time.Now(), 20, 0, 0)); err != nil {
		t.Fatal(err)
	}
	b.db.SetMaxOpenConns(2)
	for i := 0; i < 10; i++ { // si une connexion fuyait, on manquerait vite de connexions
		e, err := b.OuvrirExport(ctx, domain.ExportLive, "")
		if err != nil {
			t.Fatal(err)
		}
		boom := fmt.Errorf("client parti")
		if err := e.Lire(func(any) error { return boom }); err != boom {
			t.Fatalf("erreur = %v", err)
		}
		if err := e.Fermer(); err != nil {
			t.Fatal(err)
		}
	}
	// Contexte annule : l'ouverture echoue proprement.
	c, annuler := context.WithCancel(ctx)
	annuler()
	if _, err := b.OuvrirExport(c, domain.ExportLive, ""); err == nil {
		t.Error("export ouvert avec un contexte annule")
	}
	if _, err := b.Manifeste(ctx, ""); err != nil {
		t.Errorf("manifeste apres fermetures : %v", err)
	}
}

// Migration 3 sur une base du lot 2 : les chaines existantes gardent leurs donnees,
// ajoute est inconnu (zero).
func TestMigrationAjouteSurBaseExistante(t *testing.T) {
	ctx := context.Background()
	b := ouvrirTest(t)
	if _, err := b.db.Exec(`DELETE FROM migrations WHERE version >= 3`); err != nil {
		t.Fatal(err)
	}
	// Simule une base de la version 2 : colonnes retirees.
	// (les migrations suivantes sont rejouees aussi : la table de la migration 4 est retiree)
	for _, r := range []string{`DROP TABLE details_contenu`, `ALTER TABLE chaines DROP COLUMN ajoute`, `ALTER TABLE series DROP COLUMN ajoute`,
		`INSERT INTO chaines (id, fournisseur_id, numero, nom, logo, epg_id, categorie_id, numero_chaine, ordre) VALUES ('a:1','a','1','Vieille','','','1',1,1)`} {
		if _, err := b.db.Exec(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.migrer(ctx); err != nil {
		t.Fatal(err)
	}
	c, err := b.Chaine(ctx, "a:1")
	if err != nil || c.Nom != "Vieille" || !c.Ajoute.IsZero() {
		t.Errorf("chaine apres migration : %+v %v", c, err)
	}
}
