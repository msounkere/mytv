package epg

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"mytv/internal/domain"
)

func ouvrirTest(t *testing.T) *Base {
	t.Helper()
	b, err := Ouvrir(context.Background(), filepath.Join(t.TempDir(), "epg.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Fermer() })
	return b
}

func prog(epg string, debut, fin time.Time, titre string) domain.Programme {
	return domain.Programme{FournisseurID: "f", EpgID: epg, Debut: debut, Fin: fin, Titre: titre}
}

func rapp(chaine, epg string) domain.Rapprochement {
	return domain.Rapprochement{Chaine: domain.ContenuID(chaine), EpgID: epg, Methode: domain.RapprocheParEpgID}
}

func titres(ps []domain.Programme) []string {
	var r []string
	for _, p := range ps {
		r = append(r, p.Titre)
	}
	return r
}

func egal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func instantaneEssai(titre string) *domain.InstantaneEPG {
	return &domain.InstantaneEPG{
		FournisseurID: "f", Pris: utc(10, 12, 0),
		Programmes: []domain.Programme{
			prog("a", utc(10, 12, 0), utc(10, 13, 0), titre+"-2"),
			prog("a", utc(10, 10, 0), utc(10, 12, 0), titre+"-1"),
			prog("a", utc(10, 13, 0), utc(10, 14, 0), titre+"-3"),
			prog("b", utc(10, 11, 0), utc(10, 15, 0), titre+"-b"),
		},
		Rapprochements: []domain.Rapprochement{rapp("f:1", "a"), rapp("f:2", "b"), rapp("f:3", "a"),
			{Chaine: "f:4", Methode: domain.RapprocheParNom, Ambigu: true}},
	}
}

// CA5 : « en cours et suivant » ; bornes (debut inclus, fin exclue) ; trou ; fin de grille.
func TestEnCoursEtSuivant(t *testing.T) {
	ctx := context.Background()
	b := ouvrirTest(t)
	if err := b.EcrireEPG(ctx, instantaneEssai("p")); err != nil {
		t.Fatal(err)
	}
	cas := []struct {
		a             time.Time
		courant, suiv string
	}{
		{utc(10, 11, 0), "p-1", "p-2"},
		{utc(10, 12, 0), "p-2", "p-3"}, // le debut est inclus : p-1 est fini
		{utc(10, 12, 59), "p-2", "p-3"},
		{utc(10, 13, 30), "p-3", ""}, // dernier programme
		{utc(10, 9, 0), "", "p-1"},   // avant le debut du guide : pas de courant, mais un suivant
		{utc(10, 14, 0), "", ""},     // apres la fin
	}
	for _, c := range cas {
		cur, suiv, err := b.EnCoursEtSuivant(ctx, "f:1", c.a)
		if err != nil {
			t.Fatal(err)
		}
		if nom(cur) != c.courant || nom(suiv) != c.suiv {
			t.Errorf("a=%v : courant=%q suivant=%q ; attendu %q / %q", c.a, nom(cur), nom(suiv), c.courant, c.suiv)
		}
	}
	// Deux chaines (f:1 et f:3) partagent le canal a ; une chaine ambigue ou inconnue n'a rien.
	for _, id := range []domain.ContenuID{"f:4", "f:inconnue"} {
		cur, suiv, err := b.EnCoursEtSuivant(ctx, id, utc(10, 12, 30))
		if err != nil || cur != nil || suiv != nil {
			t.Errorf("%s : %v %v %v", id, cur, suiv, err)
		}
	}
	if cur, _, _ := b.EnCoursEtSuivant(ctx, "f:3", utc(10, 12, 30)); nom(cur) != "p-2" {
		t.Fatal("f:3 partage le canal de f:1")
	}
}

func nom(p *domain.Programme) string {
	if p == nil {
		return ""
	}
	return p.Titre
}

// CA5 : grille par plage, chevauchements, ordre par debut, plusieurs chaines.
func TestGrilleParPlageHoraire(t *testing.T) {
	ctx := context.Background()
	b := ouvrirTest(t)
	if err := b.EcrireEPG(ctx, instantaneEssai("p")); err != nil {
		t.Fatal(err)
	}
	g, err := b.Grille(ctx, []domain.ContenuID{"f:1", "f:2", "f:4", "f:1"}, utc(10, 11, 30), utc(10, 13, 0))
	if err != nil {
		t.Fatal(err)
	}
	// [11:30 ; 13:00[ chevauche p-1 (10-12) et p-2 (12-13) mais PAS p-3 (13-14).
	if !egal(titres(g["f:1"]), []string{"p-1", "p-2"}) {
		t.Fatalf("f:1 = %v", titres(g["f:1"]))
	}
	if !egal(titres(g["f:2"]), []string{"p-b"}) {
		t.Fatalf("f:2 = %v", titres(g["f:2"]))
	}
	if _, ok := g["f:4"]; ok || len(g) != 2 {
		t.Fatalf("une chaine ambigue ne recoit rien : %v", g)
	}
	if g["f:1"][0].FournisseurID != "f" || !g["f:1"][0].Debut.Equal(utc(10, 10, 0)) || g["f:1"][0].Debut.Location() != time.UTC {
		t.Fatalf("champs mal relus : %+v", g["f:1"][0])
	}
	// Plage vide : rien. Plage qui touche la fin d'un programme : pas de chevauchement.
	if g, _ := b.Grille(ctx, []domain.ContenuID{"f:1"}, utc(10, 14, 0), utc(10, 16, 0)); len(g) != 0 {
		t.Fatalf("grille apres la fin = %v", g)
	}
}

func TestGrilleBeaucoupDeChaines(t *testing.T) {
	ctx := context.Background()
	b := ouvrirTest(t)
	e := &domain.InstantaneEPG{FournisseurID: "f", Pris: utc(10, 12, 0)}
	var ids []domain.ContenuID
	for i := 0; i < 1000; i++ { // > un lot de requete
		id := domain.ContenuID("f:c" + itoa(i))
		ids = append(ids, id)
		e.Programmes = append(e.Programmes, prog("e"+itoa(i), utc(10, 10, 0), utc(10, 11, 0), "x"))
		e.Rapprochements = append(e.Rapprochements, rapp(string(id), "e"+itoa(i)))
	}
	if err := b.EcrireEPG(ctx, e); err != nil {
		t.Fatal(err)
	}
	g, err := b.Grille(ctx, ids, utc(10, 0, 0), utc(10, 23, 0))
	if err != nil || len(g) != 1000 {
		t.Fatalf("%d chaines, %v", len(g), err)
	}
}

func itoa(i int) string {
	return string(rune('0'+i/100%10)) + string(rune('0'+i/10%10)) + string(rune('0'+i%10))
}

// CA4 : l'ecriture remplace l'EPG du fournisseur sans toucher a celui d'un autre.
func TestEcritureRemplaceParFournisseur(t *testing.T) {
	ctx := context.Background()
	b := ouvrirTest(t)
	b.EcrireEPG(ctx, instantaneEssai("ancien"))
	autre := &domain.InstantaneEPG{FournisseurID: "g", Pris: utc(10, 12, 0),
		Programmes:     []domain.Programme{{FournisseurID: "g", EpgID: "a", Debut: utc(10, 10, 0), Fin: utc(10, 11, 0), Titre: "autre"}},
		Rapprochements: []domain.Rapprochement{rapp("g:1", "a")}}
	b.EcrireEPG(ctx, autre)

	nouveau := &domain.InstantaneEPG{FournisseurID: "f", Pris: utc(10, 13, 0),
		Programmes: []domain.Programme{prog("a", utc(10, 10, 0), utc(10, 11, 0), "neuf")}, Rapprochements: []domain.Rapprochement{rapp("f:1", "a")}}
	if err := b.EcrireEPG(ctx, nouveau); err != nil {
		t.Fatal(err)
	}
	if c, _, _ := b.EnCoursEtSuivant(ctx, "f:1", utc(10, 10, 30)); nom(c) != "neuf" {
		t.Fatalf("f:1 = %q", nom(c))
	}
	if c, _, _ := b.EnCoursEtSuivant(ctx, "f:2", utc(10, 12, 0)); c != nil {
		t.Fatal("l'ancien rapprochement f:2 devait disparaitre")
	}
	// Le canal « a » existe chez g aussi : pas de melange entre fournisseurs.
	if c, _, _ := b.EnCoursEtSuivant(ctx, "g:1", utc(10, 10, 30)); nom(c) != "autre" {
		t.Fatalf("g:1 = %q", nom(c))
	}
	if d, _ := b.DerniereSynchroEPG(ctx, "f"); !d.Equal(utc(10, 13, 0)) {
		t.Fatalf("synchro = %v", d)
	}
}

// CA4 : un echec au milieu de l'ecriture laisse l'ancien EPG intact.
func TestEchecEnCoursDEcritureConserveAncienEPG(t *testing.T) {
	ctx := context.Background()
	b := ouvrirTest(t)
	b.EcrireEPG(ctx, instantaneEssai("ancien"))
	panne := errors.New("panne injectee")
	b.apresInsertion = func(table string) error {
		if table == "epg_programmes" {
			return panne
		}
		return nil
	}
	neuf := instantaneEssai("neuf")
	neuf.Pris = utc(11, 0, 0)
	if err := b.EcrireEPG(ctx, neuf); !errors.Is(err, panne) {
		t.Fatalf("erreur = %v", err)
	}
	b.apresInsertion = nil
	c, _, _ := b.EnCoursEtSuivant(ctx, "f:1", utc(10, 12, 30))
	if nom(c) != "ancien-2" {
		t.Fatalf("courant = %q : l'ancien EPG a ete touche", nom(c))
	}
	if d, _ := b.DerniereSynchroEPG(ctx, "f"); !d.Equal(utc(10, 12, 0)) {
		t.Fatalf("synchro = %v", d)
	}
}

func TestEcritureRefuseProgrammeInvalideEtContexteAnnule(t *testing.T) {
	ctx := context.Background()
	b := ouvrirTest(t)
	b.EcrireEPG(ctx, instantaneEssai("ancien"))
	mauvais := instantaneEssai("neuf")
	mauvais.Programmes[0].Fin = mauvais.Programmes[0].Debut
	if err := b.EcrireEPG(ctx, mauvais); err == nil {
		t.Fatal("fin <= debut accepte")
	}
	annule, stop := context.WithCancel(ctx)
	stop()
	if err := b.EcrireEPG(annule, instantaneEssai("neuf")); err == nil {
		t.Fatal("contexte annule : erreur attendue")
	}
	if c, _, _ := b.EnCoursEtSuivant(ctx, "f:1", utc(10, 12, 30)); nom(c) != "ancien-2" {
		t.Fatal("l'ancien EPG a ete touche")
	}
}

func TestDoublonsDansLInstantaneNeCassentPasLEcriture(t *testing.T) {
	ctx := context.Background()
	b := ouvrirTest(t)
	e := instantaneEssai("p")
	e.Programmes = append(e.Programmes, e.Programmes[0])
	if err := b.EcrireEPG(ctx, e); err != nil {
		t.Fatal(err)
	}
	g, _ := b.Grille(ctx, []domain.ContenuID{"f:1"}, utc(10, 0, 0), utc(11, 0, 0))
	if len(g["f:1"]) != 3 {
		t.Fatalf("programmes = %v", titres(g["f:1"]))
	}
}

func TestDerniereSynchroEPGZeroSiJamais(t *testing.T) {
	b := ouvrirTest(t)
	if d, err := b.DerniereSynchroEPG(context.Background(), "f"); err != nil || !d.IsZero() {
		t.Fatalf("%v %v", d, err)
	}
}

// Purge des programmes termines.
func TestPurgerSupprimeLesProgrammesTermines(t *testing.T) {
	ctx := context.Background()
	b := ouvrirTest(t)
	b.EcrireEPG(ctx, instantaneEssai("p"))
	n, err := b.Purger(ctx, utc(10, 13, 0)) // p-1 (fin 12:00) et p-2 (fin 13:00) sont termines
	if err != nil || n != 2 {
		t.Fatalf("purges = %d, %v", n, err)
	}
	g, _ := b.Grille(ctx, []domain.ContenuID{"f:1"}, utc(10, 0, 0), utc(11, 0, 0))
	if !egal(titres(g["f:1"]), []string{"p-3"}) {
		t.Fatalf("reste = %v", titres(g["f:1"]))
	}
}

func TestAmbigusListes(t *testing.T) {
	ctx := context.Background()
	b := ouvrirTest(t)
	b.EcrireEPG(ctx, instantaneEssai("p"))
	a, err := b.Ambigus(ctx, "f")
	if err != nil || len(a) != 1 || a[0] != "f:4" {
		t.Fatalf("%v %v", a, err)
	}
}

// Lectures pendant une ecriture : pas de « database is locked » (-race).
func TestLecturesPendantEcriture(t *testing.T) {
	ctx := context.Background()
	b := ouvrirTest(t)
	b.EcrireEPG(ctx, instantaneEssai("p"))
	var wg sync.WaitGroup
	stop := make(chan struct{})
	erreurs := make(chan error, 8)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if _, _, err := b.EnCoursEtSuivant(ctx, "f:1", utc(10, 12, 30)); err != nil {
					erreurs <- err
					return
				}
			}
		}()
	}
	for i := 0; i < 20; i++ {
		if err := b.EcrireEPG(ctx, instantaneEssai("p")); err != nil {
			t.Error(err)
		}
	}
	close(stop)
	wg.Wait()
	close(erreurs)
	for err := range erreurs {
		t.Error(err)
	}
}
