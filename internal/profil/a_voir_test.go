package profil

import (
	"context"
	"errors"
	"testing"
	"time"

	"mytv/internal/domain"
)

func aVoirIDs(l []domain.ElementAVoir) (r []string) {
	for _, e := range l {
		r = append(r, string(e.ID))
	}
	return
}

// CA1 : liste par profil persistee, ordonnee, idempotente ; options par element ;
// orphelins ignores ; profils isoles ; migration profil (table profil_a_voir).
func TestCA1_ListeAVoirPersisteeEtOrdonnee(t *testing.T) {
	b := ouvrir(t, nil)
	ctx := context.Background()
	b.cat.mu.Lock()
	b.cat.ok["f:s7"] = true
	b.cat.mu.Unlock()

	film := domain.ElementAVoir{Contenu: "f:1"}
	serie := domain.ElementAVoir{Contenu: "f:s7", Serie: true, EpisodesAvance: 3, DepuisSaison: 2, DepuisEpisode: 4}
	for _, e := range []domain.ElementAVoir{film, serie, {Contenu: "f:2"}} {
		if err := b.AjouterAVoir(ctx, "p1", e); err != nil {
			t.Fatal(err)
		}
	}
	// Idempotent : un nouvel ajout du film ne change ni son rang ni la taille de la liste,
	// mais met a jour ses options.
	if err := b.AjouterAVoir(ctx, "p1", domain.ElementAVoir{Contenu: "f:s7", Serie: true, EpisodesAvance: 1}); err != nil {
		t.Fatal(err)
	}
	l, err := b.AVoir(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if got := aVoirIDs(l); !egal(got, []string{"vod:f:1", "serie:f:s7", "vod:f:2"}) {
		t.Fatalf("liste = %v", got)
	}
	if s := l[1]; !s.Serie || s.Contenu != "f:s7" || s.EpisodesAvance != 1 || s.DepuisSaison != 0 || s.Rang != 1 || s.Ajoute.IsZero() {
		t.Errorf("serie = %+v", s)
	}
	// Ordre : les ids donnes d'abord, les autres suivent ; inconnu ignore.
	if err := b.OrdonnerAVoir(ctx, "p1", []domain.AVoirID{"vod:f:2", "vod:f:999", "serie:f:s7"}); err != nil {
		t.Fatal(err)
	}
	l, _ = b.AVoir(ctx, "p1")
	if got := aVoirIDs(l); !egal(got, []string{"vod:f:2", "serie:f:s7", "vod:f:1"}) || l[0].Rang != 0 || l[2].Rang != 2 {
		t.Fatalf("apres ordre = %v", got)
	}
	// Un autre profil ne voit rien ; les profils ayant une liste sont connus.
	if l2, _ := b.AVoir(ctx, "p2"); len(l2) != 0 {
		t.Errorf("p2 voit la liste de p1 : %v", l2)
	}
	if ps, err := b.ProfilsAVoir(ctx); err != nil || !egal(ps, []string{"p1"}) {
		t.Errorf("profils = %v, %v", ps, err)
	}
	// Orphelin (contenu disparu du catalogue) ignore a la lecture, rangs recomptes.
	b.cat.retirer("f:s7")
	l, _ = b.AVoir(ctx, "p1")
	if got := aVoirIDs(l); !egal(got, []string{"vod:f:2", "vod:f:1"}) || l[1].Rang != 1 {
		t.Fatalf("sans orphelin = %v", got)
	}
	// Retrait idempotent.
	if err := b.RetirerAVoir(ctx, "p1", "vod:f:2"); err != nil {
		t.Fatal(err)
	}
	if err := b.RetirerAVoir(ctx, "p1", "vod:f:2"); err != nil {
		t.Fatal(err)
	}
	if l, _ = b.AVoir(ctx, "p1"); !egal(aVoirIDs(l), []string{"vod:f:1"}) {
		t.Fatalf("apres retrait = %v", aVoirIDs(l))
	}
	// Identifiants et options invalides refuses ; limite (FavorisMax = 4).
	if err := b.AjouterAVoir(ctx, "p1", domain.ElementAVoir{ID: "chaine:f:1"}); err == nil {
		t.Error("prefixe inconnu accepte")
	}
	if err := b.AjouterAVoir(ctx, "p1", domain.ElementAVoir{Contenu: "f:3", EpisodesAvance: 99}); !errors.Is(err, domain.ErrLimite) {
		t.Errorf("episodes_avance 99 : %v", err)
	}
	// L'orphelin « serie:f:s7 » compte encore (il n'est ignore qu'a la lecture).
	for _, c := range []domain.ContenuID{"f:2", "f:3"} {
		if err := b.AjouterAVoir(ctx, "p1", domain.ElementAVoir{Contenu: c}); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.AjouterAVoir(ctx, "p1", domain.ElementAVoir{Contenu: "f:4"}); !errors.Is(err, domain.ErrLimite) {
		t.Errorf("limite : %v", err)
	}
	// EffacerProfil emporte la liste.
	if err := b.EffacerProfil(ctx, "p1"); err != nil {
		t.Fatal(err)
	}
	if ps, _ := b.ProfilsAVoir(ctx); len(ps) != 0 {
		t.Errorf("liste non effacee : %v", ps)
	}
}

// CA1 : la regle « vu » (part >= domain.PartVue = 0,9 de la duree, lot 10 US-44 CA5) est celle du domaine.
func TestCA1_LectureVue(t *testing.T) {
	cas := []struct {
		pos, dur int
		vue      bool
	}{{90, 100, true}, {92, 100, true}, {89, 100, false}, {100, 100, true}, {50, 0, false}, {0, 0, false}}
	for _, c := range cas {
		l := domain.Lecture{Position: dsec(c.pos), Duree: dsec(c.dur)}
		if l.Vue() != c.vue {
			t.Errorf("%d/%d : vue = %v", c.pos, c.dur, l.Vue())
		}
	}
}

func dsec(n int) time.Duration { return time.Duration(n) * time.Second }
