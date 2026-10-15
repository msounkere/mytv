package api

import (
	"context"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mytv/internal/domain"
	"mytv/internal/profil"
	"mytv/internal/stockage"
)

// Integration reelle (stockage SQLite + etat par profil SQLite + comptes SQLite) :
// un episode enregistre dans l'historique par l'API est VISIBLE ensuite ; apres
// la disparition de sa serie (resynchronisation), il ne l'est plus.
func TestHistoriqueEpisodeVisibleAvecLesVraisStockages(t *testing.T) {
	ctx := context.Background()
	chemin := filepath.Join(t.TempDir(), "m.db")
	st, err := stockage.Ouvrir(ctx, chemin)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Fermer()
	inst := func(series ...domain.SerieTV) *domain.Instantane {
		return &domain.Instantane{FournisseurID: "a", Pris: time.Now(), Series: series,
			Chaines: []domain.Chaine{{ID: "a:1", FournisseurID: "a", Numero: "1", Nom: "Chaine"}}}
	}
	serie := domain.SerieTV{ID: "a:77", FournisseurID: "a", Numero: "77", Nom: "Ma serie"}
	if err := st.EcrireCatalogue(ctx, inst(serie)); err != nil {
		t.Fatal(err)
	}
	cfg := nouveauBanc(t, nil).cfg
	pf, err := profil.Ouvrir(ctx, chemin, profil.Options{Catalogue: st, HistoriqueMax: cfg.Profil.HistoriqueMax,
		FavorisMax: cfg.Profil.FavorisMax, ReglagesMax: cfg.Profil.ReglagesMax})
	if err != nil {
		t.Fatal(err)
	}
	defer pf.Fermer()

	b := nouveauBanc(t, func(o *Options) { o.Stockage, o.Video, o.Etat = st, st, pf })
	c := b.connecte("alice", false)

	if rec := b.requete("PUT", "/api/v1/profil/historique/a:9001.mkv", `{"position":42,"duree":3000,"serie":"a:77"}`, avec(c)); rec.Code != 204 {
		t.Fatalf("enregistrement : %d %s", rec.Code, rec.Body)
	}
	rec := b.requete("GET", "/api/v1/profil/historique", "", avec(c))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"contenu":"a:9001.mkv","serie":"a:77","position":42`) {
		t.Fatalf("l'episode doit apparaitre dans l'historique : %d %s", rec.Code, rec.Body)
	}
	// Resynchronisation sans la serie : l'episode devient orphelin et disparait.
	if err := st.EcrireCatalogue(ctx, inst()); err != nil {
		t.Fatal(err)
	}
	rec = b.requete("GET", "/api/v1/profil/historique", "", avec(c))
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "a:9001") {
		t.Fatalf("l'orphelin doit etre ignore : %s", rec.Body)
	}
	// Et l'API refuse un episode dont la serie a disparu.
	if rec := b.requete("PUT", "/api/v1/profil/historique/a:9002.mkv", `{"position":1,"serie":"a:77"}`, avec(c)); rec.Code != http.StatusBadRequest {
		t.Fatalf("serie disparue : %d", rec.Code)
	}
}
