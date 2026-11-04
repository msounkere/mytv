package profil

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mytv/internal/domain"
)

// CA6 (US-44) : a l'ouverture, chaque favori qui est un film ou une serie passe EN FIN de
// « Ma liste » du meme profil (ordre d'ajout, date d'ajout conservee), sauf s'il y est deja,
// puis quitte les favoris ; une chaine reste favorite ; un identifiant inconnu du catalogue
// reste favori ; journal INFO (profil, nombre migres) ; rejouer ne change rien.
func TestCA6_MigrationDesFavorisVersMaListe(t *testing.T) {
	ctx := context.Background()
	h := &horlogeFixe{}
	t0 := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	h.t.Store(t0.UnixMilli())
	cat := &catalogueFaux{ok: map[domain.ContenuID]bool{"f:1": true, "f:2": true, "f:3": true, "f:4": true, "f:9": true},
		types: map[domain.ContenuID]domain.Type{"f:1": domain.TypeFilm, "f:2": domain.TypeFilm, "f:3": domain.TypeSerie, "f:4": domain.TypeLive}}
	journal := &bytes.Buffer{}
	opt := Options{Catalogue: cat, Horloge: h, HistoriqueMax: 5, FavorisMax: 10, ReglagesMax: 100, Journal: slog.New(slog.NewTextHandler(journal, nil))}
	chemin := filepath.Join(t.TempDir(), "p.db")
	ouvrir := func() *Base {
		t.Helper()
		b, err := Ouvrir(ctx, chemin, opt)
		if err != nil {
			t.Fatal(err)
		}
		return b
	}

	// Semis : p1 a deja le film f:2 dans sa liste ; ses favoris : 2 films, 1 serie, 1 chaine,
	// 1 inconnu du catalogue (f:9 : existe mais sans type connu). p2 : un film favori.
	b := ouvrir()
	if err := b.AjouterAVoir(ctx, "p1", domain.ElementAVoir{Contenu: "f:2"}); err != nil {
		t.Fatal(err)
	}
	for i, id := range []domain.ContenuID{"f:1", "f:4", "f:3", "f:9", "f:2"} {
		h.t.Store(t0.Add(time.Duration(i) * time.Minute).UnixMilli())
		if err := b.AjouterFavori(ctx, "p1", id); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.AjouterFavori(ctx, "p2", "f:1"); err != nil {
		t.Fatal(err)
	}
	b.Fermer()

	// Migration a l'ouverture.
	b = ouvrir()
	fav, _ := b.Favoris(ctx, "p1")
	if !egal(ids(fav), []string{"f:4", "f:9"}) {
		t.Errorf("favoris de p1 apres migration : %v", ids(fav))
	}
	liste, _ := b.AVoir(ctx, "p1")
	var got []string
	for _, e := range liste {
		got = append(got, string(e.ID))
	}
	if !egal(got, []string{"vod:f:2", "vod:f:1", "serie:f:3"}) {
		t.Errorf("liste de p1 : %v", got)
	}
	if len(liste) == 3 && (!liste[1].Ajoute.Equal(t0) || !liste[2].Ajoute.Equal(t0.Add(2*time.Minute)) || liste[1].Rang != 1 || liste[2].Rang != 2) {
		t.Errorf("date d'ajout ou rang non conserves : %+v", liste)
	}
	fav, _ = b.Favoris(ctx, "p2")
	liste2, _ := b.AVoir(ctx, "p2")
	if len(fav) != 0 || len(liste2) != 1 || liste2[0].ID != "vod:f:1" {
		t.Errorf("p2 : favoris %v, liste %+v", ids(fav), liste2)
	}
	j := journal.String()
	if !strings.Contains(j, "profil=p1") || !strings.Contains(j, "migres=3") || !strings.Contains(j, "profil=p2") || !strings.Contains(j, "migres=1") {
		t.Errorf("journal : %s", j)
	}
	b.Fermer()

	// Rejouer : rien ne bouge, rien n'est journalise.
	journal.Reset()
	b = ouvrir()
	defer b.Fermer()
	fav, _ = b.Favoris(ctx, "p1")
	liste, _ = b.AVoir(ctx, "p1")
	if !egal(ids(fav), []string{"f:4", "f:9"}) || len(liste) != 3 || liste[0].ID != "vod:f:2" || liste[2].ID != "serie:f:3" {
		t.Errorf("apres rejeu : favoris %v, liste %+v", ids(fav), liste)
	}
	if journal.Len() != 0 {
		t.Errorf("journal apres rejeu : %s", journal.String())
	}
	// Sans catalogue qui connait les types : aucune migration (les favoris restent).
	sans := Options{Horloge: h, HistoriqueMax: 5, FavorisMax: 10, ReglagesMax: 100}
	cheminQ := filepath.Join(t.TempDir(), "q.db")
	b2, err := Ouvrir(ctx, cheminQ, sans)
	if err != nil {
		t.Fatal(err)
	}
	b2.AjouterFavori(ctx, "p1", "f:1")
	b2.Fermer()
	if b2, err = Ouvrir(ctx, cheminQ, sans); err != nil {
		t.Fatal(err)
	}
	defer b2.Fermer()
	if fav, _ := b2.Favoris(ctx, "p1"); !egal(ids(fav), []string{"f:1"}) {
		t.Errorf("sans catalogue : favoris %v", ids(fav))
	}
}
