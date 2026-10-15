package profil

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mytv/internal/domain"
)

type horlogeFixe struct{ t atomic.Int64 }

func (h *horlogeFixe) Maintenant() time.Time   { return time.UnixMilli(h.t.Load()).UTC() }
func (h *horlogeFixe) Avancer(d time.Duration) { h.t.Add(d.Milliseconds()) }

// catalogueFaux : seuls les ids listes existent.
type catalogueFaux struct {
	mu sync.Mutex
	ok map[domain.ContenuID]bool
}

func (c *catalogueFaux) Existants(_ context.Context, ids []domain.ContenuID) (map[domain.ContenuID]bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := map[domain.ContenuID]bool{}
	for _, id := range ids {
		if c.ok[id] {
			r[id] = true
		}
	}
	return r, nil
}

func (c *catalogueFaux) retirer(id domain.ContenuID) {
	c.mu.Lock()
	delete(c.ok, id)
	c.mu.Unlock()
}

type banc struct {
	*Base
	h   *horlogeFixe
	cat *catalogueFaux
}

func ouvrir(t *testing.T, mod func(*Options)) *banc {
	t.Helper()
	h := &horlogeFixe{}
	h.t.Store(time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC).UnixMilli())
	cat := &catalogueFaux{ok: map[domain.ContenuID]bool{}}
	for i := 0; i < 500; i++ {
		cat.ok[domain.ContenuID(fmt.Sprintf("f:%d", i))] = true
	}
	opt := Options{Catalogue: cat, Horloge: h, HistoriqueMax: 5, FavorisMax: 4, ReglagesMax: 100}
	if mod != nil {
		mod(&opt)
	}
	b, err := Ouvrir(context.Background(), filepath.Join(t.TempDir(), "p.db"), opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Fermer() })
	return &banc{b, h, cat}
}

func ids(l []domain.ContenuID) (r []string) {
	for _, x := range l {
		r = append(r, string(x))
	}
	return
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

var ctx = context.Background()

// CA1 : ajout / retrait idempotents, ordre d'ajout stable.
func TestFavorisIdempotentsEtOrdreStable(t *testing.T) {
	b := ouvrir(t, nil)
	for _, c := range []domain.ContenuID{"f:3", "f:1", "f:2", "f:1", "f:3"} { // doublons
		if err := b.AjouterFavori(ctx, "p1", c); err != nil {
			t.Fatal(err)
		}
	}
	l, err := b.Favoris(ctx, "p1")
	if err != nil || !egal(ids(l), []string{"f:3", "f:1", "f:2"}) {
		t.Fatalf("favoris = %v, %v", ids(l), err)
	}
	b.RetirerFavori(ctx, "p1", "f:1")
	if err := b.RetirerFavori(ctx, "p1", "f:1"); err != nil { // deja retire
		t.Fatal(err)
	}
	if err := b.RetirerFavori(ctx, "p1", "f:jamais"); err != nil {
		t.Fatal(err)
	}
	b.AjouterFavori(ctx, "p1", "f:1") // reajoute : en fin de liste
	l, _ = b.Favoris(ctx, "p1")
	if !egal(ids(l), []string{"f:3", "f:2", "f:1"}) {
		t.Fatalf("favoris = %v", ids(l))
	}
	if l, err := b.Favoris(ctx, "vide"); err != nil || l == nil || len(l) != 0 {
		t.Fatalf("profil sans favori : %v %v", l, err)
	}
}

// CA1 + limites : au-dela de FavorisMax, ErrLimite ; re-ajouter un existant reste permis.
func TestFavorisLimite(t *testing.T) {
	b := ouvrir(t, nil)
	for i := 0; i < 4; i++ {
		if err := b.AjouterFavori(ctx, "p1", domain.ContenuID(fmt.Sprintf("f:%d", i))); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.AjouterFavori(ctx, "p1", "f:10"); !errors.Is(err, domain.ErrLimite) {
		t.Fatalf("erreur = %v", err)
	}
	if err := b.AjouterFavori(ctx, "p1", "f:0"); err != nil {
		t.Fatalf("un favori existant ne compte pas deux fois : %v", err)
	}
	if err := b.AjouterFavori(ctx, "p2", "f:10"); err != nil {
		t.Fatalf("la limite est PAR profil : %v", err)
	}
}

// CA2 : historique du plus recent au plus ancien, reprise mise a jour, borne et purge.
func TestHistoriqueRepriseBorneEtPurge(t *testing.T) {
	b := ouvrir(t, nil)
	for i := 0; i < 7; i++ {
		b.h.Avancer(time.Minute)
		if err := b.EnregistrerLecture(ctx, "p1", domain.Lecture{Contenu: domain.ContenuID(fmt.Sprintf("f:%d", i)),
			Position: time.Duration(i) * time.Second, Duree: time.Hour}); err != nil {
			t.Fatal(err)
		}
	}
	h, err := b.Historique(ctx, "p1", 0)
	if err != nil || !egal(idsL(h), []string{"f:6", "f:5", "f:4", "f:3", "f:2"}) { // f:0 et f:1 purgees
		t.Fatalf("historique = %v, %v", idsL(h), err)
	}
	// Reprendre un film deja vu : mise a jour (pas de doublon), il remonte en tete.
	b.h.Avancer(time.Minute)
	b.EnregistrerLecture(ctx, "p1", domain.Lecture{Contenu: "f:3", Position: 90 * time.Second, Duree: time.Hour})
	h, _ = b.Historique(ctx, "p1", 2)
	if !egal(idsL(h), []string{"f:3", "f:6"}) || h[0].Position != 90*time.Second || h[0].Duree != time.Hour {
		t.Fatalf("historique = %+v", h)
	}
	if h[0].Vu.IsZero() {
		t.Fatal("Vu non renseigne par l'horloge")
	}
	if all, _ := b.Historique(ctx, "p1", 0); len(all) != 5 {
		t.Fatalf("%d entrees, borne 5", len(all))
	}
	b.OublierLecture(ctx, "p1", "f:3")
	if err := b.OublierLecture(ctx, "p1", "f:3"); err != nil {
		t.Fatal(err)
	}
	if all, _ := b.Historique(ctx, "p1", 0); len(all) != 4 {
		t.Fatalf("apres oubli : %d", len(all))
	}
}

func idsL(l []domain.Lecture) (r []string) {
	for _, x := range l {
		r = append(r, string(x.Contenu))
	}
	return
}

// CA2 : deux lectures dans la meme milliseconde gardent l'ordre d'ecriture ; positions incoherentes bornees.
func TestHistoriqueMemeInstantEtPositionsIncoherentes(t *testing.T) {
	b := ouvrir(t, nil)
	b.EnregistrerLecture(ctx, "p", domain.Lecture{Contenu: "f:1"})
	b.EnregistrerLecture(ctx, "p", domain.Lecture{Contenu: "f:2"})
	b.EnregistrerLecture(ctx, "p", domain.Lecture{Contenu: "f:1", Position: -5 * time.Second})
	h, _ := b.Historique(ctx, "p", 0)
	if !egal(idsL(h), []string{"f:1", "f:2"}) || h[0].Position != 0 {
		t.Fatalf("%+v", h)
	}
	b.EnregistrerLecture(ctx, "p", domain.Lecture{Contenu: "f:3", Position: 2 * time.Hour, Duree: time.Hour})
	if h, _ := b.Historique(ctx, "p", 1); h[0].Position != time.Hour {
		t.Fatalf("reprise au-dela de la fin : %v", h[0].Position)
	}
	if err := b.EnregistrerLecture(ctx, "p", domain.Lecture{}); err == nil {
		t.Fatal("contenu vide accepte")
	}
}

// CA3 : un profil ne lit ni n'ecrit l'etat d'un autre, y compris par des identifiants proches.
func TestIsolationEntreProfils(t *testing.T) {
	b := ouvrir(t, nil)
	b.AjouterFavori(ctx, "A", "f:1")
	b.EnregistrerLecture(ctx, "A", domain.Lecture{Contenu: "f:1", Position: time.Second})
	r, err := b.EcrireReglages(ctx, "A", map[string]string{"theme": "sombre"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	// B ne voit rien de A.
	if l, _ := b.Favoris(ctx, "B"); len(l) != 0 {
		t.Fatalf("B voit les favoris de A : %v", l)
	}
	if h, _ := b.Historique(ctx, "B", 0); len(h) != 0 {
		t.Fatalf("B voit l'historique de A : %v", h)
	}
	if rg, _ := b.Reglages(ctx, "B"); len(rg.Valeurs) != 0 || rg.Revision != 0 {
		t.Fatalf("B voit les reglages de A : %+v", rg)
	}
	// B n'ecrit pas chez A : retrait/oubli de B n'atteignent pas A ; la revision de A n'est pas celle de B.
	b.RetirerFavori(ctx, "B", "f:1")
	b.OublierLecture(ctx, "B", "f:1")
	if _, err := b.EcrireReglages(ctx, "B", map[string]string{"x": "y"}, r.Revision); !errors.Is(err, domain.ErrConflit) {
		t.Fatalf("B ecrit avec la revision de A : %v", err)
	}
	if l, _ := b.Favoris(ctx, "A"); len(l) != 1 {
		t.Fatal("favori de A retire par B")
	}
	if h, _ := b.Historique(ctx, "A", 0); len(h) != 1 {
		t.Fatal("historique de A efface par B")
	}
	if rg, _ := b.Reglages(ctx, "A"); rg.Valeurs["theme"] != "sombre" || rg.Revision != 1 {
		t.Fatalf("reglages de A alteres : %+v", rg)
	}
	// Un identifiant de profil injecte (jokers SQL) ne correspond qu'a lui-meme.
	if l, _ := b.Favoris(ctx, "%"); len(l) != 0 {
		t.Fatal("joker SQL interprete")
	}
	// EffacerProfil ne touche que le profil vise.
	b.AjouterFavori(ctx, "B", "f:2")
	if err := b.EffacerProfil(ctx, "A"); err != nil {
		t.Fatal(err)
	}
	if l, _ := b.Favoris(ctx, "A"); len(l) != 0 {
		t.Fatal("A pas efface")
	}
	if l, _ := b.Favoris(ctx, "B"); len(l) != 1 {
		t.Fatal("B efface par erreur")
	}
}

// CA4 : un contenu disparu apres resynchronisation est ignore a la lecture (sans erreur) ;
// pour un episode c'est la disparition de la SERIE qui compte.
func TestOrphelinsIgnoresALaLecture(t *testing.T) {
	b := ouvrir(t, nil)
	b.cat.ok["f:serie"] = true
	for _, c := range []domain.ContenuID{"f:1", "f:2", "f:3"} {
		b.AjouterFavori(ctx, "p", c)
		b.h.Avancer(time.Minute)
		b.EnregistrerLecture(ctx, "p", domain.Lecture{Contenu: c})
	}
	b.h.Avancer(time.Minute)
	b.EnregistrerLecture(ctx, "p", domain.Lecture{Contenu: "f:ep-9", Serie: "f:serie", Position: time.Minute})
	// L'episode n'est pas dans le catalogue, sa serie si : il est visible.
	if h, _ := b.Historique(ctx, "p", 1); !egal(idsL(h), []string{"f:ep-9"}) || h[0].Serie != "f:serie" {
		t.Fatalf("episode = %+v", h)
	}
	b.cat.retirer("f:2")
	b.cat.retirer("f:serie")
	l, err := b.Favoris(ctx, "p")
	if err != nil || !egal(ids(l), []string{"f:1", "f:3"}) {
		t.Fatalf("favoris = %v, %v", ids(l), err)
	}
	h, err := b.Historique(ctx, "p", 0)
	if err != nil || !egal(idsL(h), []string{"f:3", "f:1"}) {
		t.Fatalf("historique = %v, %v", idsL(h), err)
	}
	// La limite s'applique APRES l'elimination des orphelins.
	if h, _ := b.Historique(ctx, "p", 2); len(h) != 2 {
		t.Fatalf("limite 2 : %v", idsL(h))
	}
	// Le contenu qui revient (synchro partielle) redevient visible : rien n'a ete supprime.
	b.cat.ok["f:2"] = true
	if l, _ := b.Favoris(ctx, "p"); len(l) != 3 {
		t.Fatalf("favoris apres retour = %v", ids(l))
	}
}

// CA5 : ecritures concurrentes atomiques (-race) : la limite n'est jamais depassee,
// l'historique reste borne, une seule ecriture de reglages gagne par revision.
func TestEcrituresConcurrentes(t *testing.T) {
	b := ouvrir(t, func(o *Options) { o.FavorisMax = 20; o.HistoriqueMax = 10 })
	var wg sync.WaitGroup
	var limites, gagnants atomic.Int32
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c := domain.ContenuID(fmt.Sprintf("f:%d", i))
			if err := b.AjouterFavori(ctx, "p", c); errors.Is(err, domain.ErrLimite) {
				limites.Add(1)
			} else if err != nil {
				t.Error(err)
			}
			if err := b.EnregistrerLecture(ctx, "p", domain.Lecture{Contenu: c}); err != nil {
				t.Error(err)
			}
			if _, err := b.EcrireReglages(ctx, "p", map[string]string{"k": fmt.Sprint(i)}, 0); err == nil {
				gagnants.Add(1)
			} else if !errors.Is(err, domain.ErrConflit) {
				t.Error(err)
			}
			b.Favoris(ctx, "p")
			b.Historique(ctx, "p", 3)
		}()
	}
	wg.Wait()
	if l, _ := b.Favoris(ctx, "p"); len(l) != 20 || limites.Load() != 40 {
		t.Fatalf("%d favoris, %d refus (attendu 20 / 40)", len(l), limites.Load())
	}
	if h, _ := b.Historique(ctx, "p", 0); len(h) != 10 {
		t.Fatalf("historique = %d", len(h))
	}
	if gagnants.Load() != 1 {
		t.Fatalf("%d ecritures de reglages ont reussi avec la revision 0", gagnants.Load())
	}
	if r, _ := b.Reglages(ctx, "p"); r.Revision != 1 {
		t.Fatalf("revision = %d", r.Revision)
	}
}

// Deux appareils : A et B lisent la revision 3, A ecrit, B est refuse AVEC l'etat courant ;
// B relit, refait son ecriture : « dernier ecrit gagne », en connaissance de cause.
func TestConflitEntreDeuxAppareils(t *testing.T) {
	b := ouvrir(t, nil)
	r0, _ := b.Reglages(ctx, "p")
	if r0.Revision != 0 || r0.Valeurs == nil || !r0.Modifie.IsZero() {
		t.Fatalf("reglages initiaux = %+v", r0)
	}
	b.h.Avancer(time.Minute)
	a, err := b.EcrireReglages(ctx, "p", map[string]string{"langue": "fr", "theme": "clair"}, r0.Revision)
	if err != nil || a.Revision != 1 || !a.Modifie.Equal(b.h.Maintenant()) {
		t.Fatalf("appareil A : %+v, %v", a, err)
	}
	b.h.Avancer(time.Minute)
	cur, err := b.EcrireReglages(ctx, "p", map[string]string{"theme": "sombre"}, r0.Revision) // B, revision perimee
	if !errors.Is(err, domain.ErrConflit) {
		t.Fatalf("appareil B : %v", err)
	}
	if cur.Revision != 1 || cur.Valeurs["langue"] != "fr" || !cur.Modifie.Equal(a.Modifie) {
		t.Fatalf("l'etat courant doit accompagner le conflit : %+v", cur)
	}
	if lu, _ := b.Reglages(ctx, "p"); lu.Valeurs["theme"] != "clair" {
		t.Fatal("l'ecriture refusee a quand meme modifie l'etat")
	}
	// B fusionne et reessaie avec la bonne revision.
	fus := map[string]string{"langue": cur.Valeurs["langue"], "theme": "sombre"}
	b2, err := b.EcrireReglages(ctx, "p", fus, cur.Revision)
	if err != nil || b2.Revision != 2 || b2.Valeurs["theme"] != "sombre" {
		t.Fatalf("reessai de B : %+v, %v", b2, err)
	}
	// Les valeurs rendues ne partagent pas la memoire de l'appelant.
	fus["theme"] = "modifie apres coup"
	if lu, _ := b.Reglages(ctx, "p"); lu.Valeurs["theme"] != "sombre" || b2.Valeurs["theme"] != "sombre" {
		t.Fatal("aliasing de la map de l'appelant")
	}
	// Revision future ou negative : conflit aussi.
	for _, rev := range []int64{99, -1} {
		if _, err := b.EcrireReglages(ctx, "p", nil, rev); !errors.Is(err, domain.ErrConflit) {
			t.Fatalf("rev %d : %v", rev, err)
		}
	}
	// Valeurs nil : reglages vides valides.
	if r, err := b.EcrireReglages(ctx, "p", nil, 2); err != nil || len(r.Valeurs) != 0 || r.Revision != 3 {
		t.Fatalf("%+v %v", r, err)
	}
}

// Limites de taille des reglages : refus avant ecriture, etat intact.
func TestReglagesLimitesDeTaille(t *testing.T) {
	b := ouvrir(t, nil) // ReglagesMax = 100 octets
	ok, err := b.EcrireReglages(ctx, "p", map[string]string{"a": "bonjour"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	cas := map[string]map[string]string{
		"trop gros":  {"gros": string(make([]byte, 101))},
		"cle vide":   {"": "x"},
		"cle longue": {string(make([]byte, 65)): "x"},
	}
	for nom, v := range cas {
		if _, err := b.EcrireReglages(ctx, "p", v, ok.Revision); !errors.Is(err, domain.ErrLimite) {
			t.Errorf("%s : %v", nom, err)
		}
	}
	if _, err := b.EcrireReglages(ctx, "p", map[string]string{"a": string(make([]byte, 98))}, ok.Revision); err != nil { // 1+98 <= 100
		t.Errorf("a la limite exacte : %v", err)
	}
	if r, _ := b.Reglages(ctx, "p"); r.Revision != 2 {
		t.Errorf("revision = %d : un refus ne doit pas l'incrementer", r.Revision)
	}
}

func TestOuvrirRefuseDesBornesNulles(t *testing.T) {
	if _, err := Ouvrir(ctx, filepath.Join(t.TempDir(), "x.db"), Options{}); err == nil {
		t.Fatal("bornes nulles acceptees")
	}
}

// Les donnees survivent a la fermeture (migration rejouee a la reouverture).
func TestPersistanceEtReouverture(t *testing.T) {
	chemin := filepath.Join(t.TempDir(), "p.db")
	opt := Options{HistoriqueMax: 5, FavorisMax: 5, ReglagesMax: 100}
	b, err := Ouvrir(ctx, chemin, opt)
	if err != nil {
		t.Fatal(err)
	}
	b.AjouterFavori(ctx, "p", "f:1")
	b.Fermer()
	b, err = Ouvrir(ctx, chemin, opt)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Fermer()
	if l, _ := b.Favoris(ctx, "p"); len(l) != 1 { // sans Catalogue : pas de filtrage
		t.Fatalf("favoris = %v", l)
	}
}

func TestContexteAnnule(t *testing.T) {
	b := ouvrir(t, nil)
	c, stop := context.WithCancel(ctx)
	stop()
	if err := b.AjouterFavori(c, "p", "f:1"); err == nil {
		t.Fatal("contexte annule : erreur attendue")
	}
}
