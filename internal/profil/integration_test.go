package profil_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"mytv/internal/domain"
	"mytv/internal/profil"
	"mytv/internal/stockage"
)

// CA4 avec le VRAI stockage : apres resynchronisation, un film disparu n'apparait plus
// dans les favoris ni l'historique, une chaine toujours presente reste visible.
func TestOrphelinsAvecLeVraiStockage(t *testing.T) {
	ctx := context.Background()
	chemin := filepath.Join(t.TempDir(), "m.db")
	st, err := stockage.Ouvrir(ctx, chemin)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Fermer()
	inst := func(films ...domain.Film) *domain.Instantane {
		return &domain.Instantane{FournisseurID: "a", Pris: time.Now(),
			Chaines: []domain.Chaine{{ID: "a:1", FournisseurID: "a", Numero: "1", Nom: "Chaine"}}, Films: films}
	}
	film := func(n string) domain.Film {
		return domain.Film{ID: domain.ContenuID("a:" + n), FournisseurID: "a", Numero: n, Nom: "F" + n}
	}
	if err := st.EcrireCatalogue(ctx, inst(film("f1"), film("f2"))); err != nil {
		t.Fatal(err)
	}
	p, err := profil.Ouvrir(ctx, chemin, profil.Options{Catalogue: st, HistoriqueMax: 10, FavorisMax: 10, ReglagesMax: 100})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Fermer()
	for _, id := range []domain.ContenuID{"a:1", "a:f1", "a:f2"} {
		p.AjouterFavori(ctx, "p", id)
		p.EnregistrerLecture(ctx, "p", domain.Lecture{Contenu: id})
	}
	if err := st.EcrireCatalogue(ctx, inst(film("f1"))); err != nil { // f2 disparait
		t.Fatal(err)
	}
	l, err := p.Favoris(ctx, "p")
	if err != nil || len(l) != 2 || l[0] != "a:1" || l[1] != "a:f1" {
		t.Fatalf("favoris = %v, %v", l, err)
	}
	h, err := p.Historique(ctx, "p", 0)
	if err != nil || len(h) != 2 {
		t.Fatalf("historique = %v, %v", h, err)
	}
}
