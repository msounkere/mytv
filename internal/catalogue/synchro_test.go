package catalogue

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mytv/internal/domain"
	"mytv/internal/stockage"
	"mytv/internal/testutil/instrum"
)

var t0 = time.Date(2026, 3, 10, 12, 0, 0, 0, time.UTC)

func nouveau(t *testing.T, st domain.Stockage, h Horloge, sources ...Source) *Synchroniseur {
	t.Helper()
	s, err := Nouveau(st, sources, Options{Horloge: h, Lieu: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func actif(f *fauxFournisseur, ageMax time.Duration) Source {
	return Source{DelaiMax: time.Minute, Reprise: time.Hour, Fournisseur: f, Actif: true, AgeMax: ageMax}
}

// CA1 : telecharge puis ecrit, avec l'heure de l'horloge injectee.
func TestSynchroniserTelechargePuisEcrit(t *testing.T) {
	st, h, f := nouveauStockage(), &horlogeFausse{now: t0}, &fauxFournisseur{id: "a", nChaine: 3}
	s := nouveau(t, st, h, actif(f, time.Hour))
	if err := s.Synchroniser(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	if f.appels.Load() != 1 || st.nbEcritures() != 1 {
		t.Fatalf("appels %d, ecritures %d", f.appels.Load(), st.nbEcritures())
	}
	if got := st.catalogue["a"]; len(got.Chaines) != 3 || !got.Pris.Equal(t0) {
		t.Fatalf("catalogue ecrit : %d chaines, pris %v", len(got.Chaines), got.Pris)
	}
}

// CA1 : echec du fournisseur -> l'ancien catalogue est conserve (stockage SQLite reel) et l'erreur est renvoyee.
func TestEchecFournisseurConserveAncienCatalogue(t *testing.T) {
	ctx := context.Background()
	st, err := stockage.Ouvrir(ctx, filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Fermer()
	h, f := &horlogeFausse{now: t0}, &fauxFournisseur{id: "a", nChaine: 2}
	s := nouveau(t, st, h, actif(f, time.Hour))
	if err := s.Synchroniser(ctx, "a"); err != nil {
		t.Fatal(err)
	}

	h.Avancer(48 * time.Hour)
	f.erreur = domain.ErrAmont
	err = s.Synchroniser(ctx, "a")
	if !errors.Is(err, domain.ErrAmont) {
		t.Fatalf("erreur = %v, attendu domain.ErrAmont", err)
	}
	cs, _ := st.Chaines(ctx, domain.FiltreChaines{})
	if len(cs) != 2 {
		t.Fatalf("%d chaines apres l'echec, l'ancien catalogue (2) devait rester", len(cs))
	}
	if got, _ := st.DerniereSynchro(ctx, "a"); !got.Equal(t0) {
		t.Fatalf("derniere synchro = %v, doit rester %v", got, t0)
	}
}

// CA1 : un echec d'ecriture est renvoye (le stockage garantit lui-meme l'atomicite).
func TestEchecEcritureRenvoyeeEtJournalisee(t *testing.T) {
	st, h, f := nouveauStockage(), &horlogeFausse{now: t0}, &fauxFournisseur{id: "a"}
	st.erreur = errors.New("disque plein")
	s := nouveau(t, st, h, actif(f, time.Hour))
	if err := s.Synchroniser(context.Background(), "a"); err == nil || !strings.Contains(err.Error(), "disque plein") {
		t.Fatalf("erreur = %v", err)
	}
}

// CA2 : SiPerime respecte l'age maximal.
func TestSiPerimeNeTelechargePasUnCatalogueFrais(t *testing.T) {
	st, h, f := nouveauStockage(), &horlogeFausse{now: t0}, &fauxFournisseur{id: "a"}
	s := nouveau(t, st, h, actif(f, 24*time.Hour))
	ctx := context.Background()

	if ok, err := s.SiPerime(ctx, "a"); err != nil || !ok {
		t.Fatalf("jamais synchronise : telecharge=%v err=%v, attendu vrai", ok, err)
	}
	h.Avancer(24*time.Hour - time.Second)
	if ok, err := s.SiPerime(ctx, "a"); err != nil || ok {
		t.Fatalf("catalogue plus recent que l'age max : telecharge=%v err=%v, attendu faux", ok, err)
	}
	if f.appels.Load() != 1 {
		t.Fatalf("appels = %d, attendu 1", f.appels.Load())
	}
	h.Avancer(time.Second) // age == AgeMax : perime
	if ok, err := s.SiPerime(ctx, "a"); err != nil || !ok {
		t.Fatalf("catalogue a l'age max : telecharge=%v err=%v, attendu vrai", ok, err)
	}
	if f.appels.Load() != 2 {
		t.Fatalf("appels = %d, attendu 2", f.appels.Load())
	}
}

// CA2 : un echec de SiPerime n'avance pas la date de fraicheur.
func TestSiPerimeApresEchecRetelecharge(t *testing.T) {
	st, h, f := nouveauStockage(), &horlogeFausse{now: t0}, &fauxFournisseur{id: "a", erreur: domain.ErrAmont}
	s := nouveau(t, st, h, actif(f, time.Hour))
	if _, err := s.SiPerime(context.Background(), "a"); err == nil {
		t.Fatal("erreur attendue")
	}
	f.erreur = nil
	if ok, err := s.SiPerime(context.Background(), "a"); err != nil || !ok {
		t.Fatalf("telecharge=%v err=%v", ok, err)
	}
}

// CA3 : synchronisation quotidienne a l'heure LOCALE (fuseau injecte), horloge pilotee a la main.
func TestPlanificationQuotidienneALHeureLocale(t *testing.T) {
	plus2 := time.FixedZone("UTC+2", 2*3600)
	// 03:30 UTC = 05:30 locale : 05:00 locale est passee, la prochaine est demain, dans 23 h 30.
	h := &horlogeFausse{now: time.Date(2026, 3, 10, 3, 30, 0, 0, time.UTC)}
	st, f := nouveauStockage(), &fauxFournisseur{id: "a"}
	s, err := Nouveau(st, []Source{{DelaiMax: time.Minute, Reprise: time.Hour, Fournisseur: f, Actif: true, AgeMax: 24 * time.Hour, Heure: "05:00"}},
		Options{Horloge: h, Lieu: plus2})
	if err != nil {
		t.Fatal(err)
	}
	ctx, annuler := context.WithCancel(context.Background())
	fini := make(chan struct{})
	go func() { s.Planifier(ctx); close(fini) }()

	m := h.minuteurEnAttente(t)
	if m.delai != 23*time.Hour+30*time.Minute {
		t.Fatalf("premiere echeance dans %v, attendu 23h30", m.delai)
	}
	h.Avancer(23*time.Hour + 29*time.Minute)
	time.Sleep(20 * time.Millisecond)
	if f.appels.Load() != 0 {
		t.Fatal("synchronisation declenchee trop tot")
	}
	h.Avancer(time.Minute) // 05:00 locale
	attendre(t, "la synchro de 05:00", func() bool { return st.nbEcritures() == 1 })

	// La suivante est le lendemain, 24 h plus tard, MEME si le catalogue a 24 h moins quelques ms.
	m = h.minuteurEnAttente(t)
	if m.delai != 24*time.Hour {
		t.Fatalf("echeance suivante dans %v, attendu 24h", m.delai)
	}
	h.Avancer(24 * time.Hour)
	attendre(t, "la synchro du lendemain", func() bool { return st.nbEcritures() == 2 })

	annuler()
	<-fini
}

// CA3 : au_demarrage ne telecharge que si le catalogue est perime.
func TestSynchroAuDemarrage(t *testing.T) {
	for _, c := range []struct {
		nom     string
		ancien  time.Duration // age du catalogue present (0 = absent)
		attendu int32
	}{
		{"catalogue absent", 0, 1},
		{"catalogue perime", 30 * time.Hour, 1},
		{"catalogue frais", time.Hour, 0},
	} {
		t.Run(c.nom, func(t *testing.T) {
			st, h, f := nouveauStockage(), &horlogeFausse{now: t0}, &fauxFournisseur{id: "a"}
			if c.ancien > 0 {
				st.synchros["a"] = t0.Add(-c.ancien)
			}
			s, _ := Nouveau(st, []Source{{DelaiMax: time.Minute, Reprise: time.Hour, Fournisseur: f, Actif: true, AgeMax: 24 * time.Hour, AuDemarrage: true}},
				Options{Horloge: h, Lieu: time.UTC})
			// Sans Heure, Planifier rend la main apres la synchro de demarrage.
			done := make(chan struct{})
			go func() { s.Planifier(context.Background()); close(done) }()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("Planifier ne rend pas la main")
			}
			if f.appels.Load() != c.attendu {
				t.Fatalf("appels = %d, attendu %d", f.appels.Load(), c.attendu)
			}
		})
	}
}

// CA3 : apres un echec planifie, nouvel essai apres la reprise, tant que l'echeance suivante n'est pas la.
func TestPlanificationRetenteApresEchec(t *testing.T) {
	h := &horlogeFausse{now: time.Date(2026, 3, 10, 4, 0, 0, 0, time.UTC)}
	st, f := nouveauStockage(), &fauxFournisseur{id: "a", erreur: domain.ErrAmont}
	s, _ := Nouveau(st, []Source{{DelaiMax: time.Minute, Reprise: 30 * time.Minute, Fournisseur: f, Actif: true, AgeMax: 24 * time.Hour, Heure: "05:00"}},
		Options{Horloge: h, Lieu: time.UTC})
	ctx, annuler := context.WithCancel(context.Background())
	fini := make(chan struct{})
	go func() { s.Planifier(ctx); close(fini) }()

	h.minuteurEnAttente(t)
	h.Avancer(time.Hour) // 05:00 : premier essai, echec
	attendre(t, "le premier essai", func() bool { return f.appels.Load() == 1 })
	m := h.minuteurEnAttente(t)
	if m.delai != 30*time.Minute {
		t.Fatalf("reprise dans %v, attendu 30m", m.delai)
	}
	f.erreur = nil
	h.Avancer(30 * time.Minute)
	attendre(t, "la reprise reussie", func() bool { return st.nbEcritures() == 1 })
	annuler()
	<-fini
}

func TestPlanifierRendLaMainALAnnulation(t *testing.T) {
	h := &horlogeFausse{now: t0}
	s, _ := Nouveau(nouveauStockage(), []Source{{DelaiMax: time.Minute, Reprise: time.Hour, Fournisseur: &fauxFournisseur{id: "a"}, Actif: true, Heure: "05:00"}},
		Options{Horloge: h, Lieu: time.UTC})
	ctx, annuler := context.WithCancel(context.Background())
	fini := make(chan struct{})
	go func() { s.Planifier(ctx); close(fini) }()
	h.minuteurEnAttente(t)
	annuler()
	select {
	case <-fini:
	case <-time.After(5 * time.Second):
		t.Fatal("Planifier ne s'arrete pas")
	}
}

// CA4 : N demandes simultanees = UN telechargement.
func TestDemandesSimultaneesPartagentUnSeulTelechargement(t *testing.T) {
	st, h := nouveauStockage(), &horlogeFausse{now: t0}
	f := &fauxFournisseur{id: "a", bloque: make(chan struct{}), entre: make(chan struct{}, 10)}
	s := nouveau(t, st, h, actif(f, time.Hour))
	const n = 20
	var wg sync.WaitGroup
	erreurs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Melange des deux entrees : elles partagent le meme vol.
			if i%2 == 0 {
				erreurs <- s.Synchroniser(context.Background(), "a")
			} else {
				_, err := s.SiPerime(context.Background(), "a")
				erreurs <- err
			}
		}()
	}
	<-f.entre // le telechargement a commence
	attendre(t, "tous les demandeurs en attente", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		v := s.vols["a"]
		return v != nil && v.attendants == n
	})
	close(f.bloque)
	wg.Wait()
	close(erreurs)
	for err := range erreurs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if f.appels.Load() != 1 || st.nbEcritures() != 1 {
		t.Fatalf("appels %d, ecritures %d, attendu 1 et 1", f.appels.Load(), st.nbEcritures())
	}
	// Le vol est termine : une nouvelle demande re-telecharge.
	f.bloque = nil
	if err := s.Synchroniser(context.Background(), "a"); err != nil || f.appels.Load() != 2 {
		t.Fatalf("apres le vol : err %v, appels %d", err, f.appels.Load())
	}
}

// CA4 : le depart d'un demandeur n'interrompt pas le telechargement des autres ; le depart du dernier l'annule.
func TestAnnulationDUnDemandeurEtDuDernier(t *testing.T) {
	st, h := nouveauStockage(), &horlogeFausse{now: t0}
	f := &fauxFournisseur{id: "a", bloque: make(chan struct{}), entre: make(chan struct{}, 10)}
	s := nouveau(t, st, h, actif(f, time.Hour))

	ctx1, annuler1 := context.WithCancel(context.Background())
	e1, e2 := make(chan error, 1), make(chan error, 1)
	go func() { e1 <- s.Synchroniser(ctx1, "a") }()
	<-f.entre
	ctx2, annuler2 := context.WithCancel(context.Background())
	go func() { e2 <- s.Synchroniser(ctx2, "a") }()
	attendre(t, "deux demandeurs", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.vols["a"] != nil && s.vols["a"].attendants == 2
	})

	annuler1() // le premier part : le second garde son telechargement
	if err := <-e1; !errors.Is(err, context.Canceled) {
		t.Fatalf("e1 = %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	if f.annules.Load() != 0 {
		t.Fatal("le telechargement a ete annule alors qu'un demandeur attend encore")
	}
	annuler2() // le dernier part : plus personne n'attend, on arrete de telecharger
	if err := <-e2; !errors.Is(err, context.Canceled) {
		t.Fatalf("e2 = %v", err)
	}
	attendre(t, "l'annulation du telechargement", func() bool { return f.annules.Load() == 1 })
	attendre(t, "la fin du vol", func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return len(s.vols) == 0
	})
	if st.nbEcritures() != 0 {
		t.Fatal("rien ne devait etre ecrit")
	}
	// Une demande ulterieure repart normalement.
	f.bloque = nil
	if err := s.Synchroniser(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
}

// CA4 : les fournisseurs sont independants (un telechargement lent de A ne bloque pas B).
func TestFournisseursSynchroniseSansSeBloquer(t *testing.T) {
	st, h := nouveauStockage(), &horlogeFausse{now: t0}
	a := &fauxFournisseur{id: "a", bloque: make(chan struct{}), entre: make(chan struct{}, 1)}
	b := &fauxFournisseur{id: "b"}
	s := nouveau(t, st, h, actif(a, time.Hour), actif(b, time.Hour))
	ctx, annuler := context.WithCancel(context.Background())
	defer annuler()
	go s.Synchroniser(ctx, "a")
	<-a.entre
	if err := s.Synchroniser(context.Background(), "b"); err != nil {
		t.Fatal(err)
	}
	close(a.bloque)
}

// CA5 : un fournisseur inactif n'est jamais synchronise.
func TestFournisseurInactifJamaisSynchronise(t *testing.T) {
	st, h := nouveauStockage(), &horlogeFausse{now: t0}
	f := &fauxFournisseur{id: "a"}
	s := nouveau(t, st, h, Source{DelaiMax: time.Minute, Reprise: time.Hour, Fournisseur: f, Actif: false, AgeMax: time.Hour, Heure: "05:00", AuDemarrage: true})
	ctx := context.Background()
	if err := s.Synchroniser(ctx, "a"); !errors.Is(err, ErrInactif) {
		t.Fatalf("Synchroniser = %v", err)
	}
	if _, err := s.SiPerime(ctx, "a"); !errors.Is(err, ErrInactif) {
		t.Fatalf("SiPerime = %v", err)
	}
	if err := s.SynchroniserTous(ctx); err != nil {
		t.Fatal(err)
	}
	// Planifier ne lance rien pour lui : retour immediat, sans minuteur ni appel.
	s.Planifier(ctx)
	if f.appels.Load() != 0 || len(h.minuteurs) != 0 {
		t.Fatalf("appels %d, minuteurs %d", f.appels.Load(), len(h.minuteurs))
	}
	if _, err := s.SiPerime(ctx, "inconnu"); !errors.Is(err, ErrInconnu) {
		t.Fatalf("fournisseur inconnu : %v", err)
	}
}

// CA5 : un fournisseur en echec n'empeche pas les autres.
func TestUnEchecNEmpechePasLesAutres(t *testing.T) {
	st, h := nouveauStockage(), &horlogeFausse{now: t0}
	ko := &fauxFournisseur{id: "ko", erreur: domain.ErrRefuseIP}
	ok1, ok2 := &fauxFournisseur{id: "ok1"}, &fauxFournisseur{id: "ok2"}
	inactif := &fauxFournisseur{id: "off"}
	s := nouveau(t, st, h, actif(ko, time.Hour), actif(ok1, time.Hour), actif(ok2, time.Hour),
		Source{DelaiMax: time.Minute, Reprise: time.Hour, Fournisseur: inactif, AgeMax: time.Hour})
	err := s.SynchroniserTous(context.Background())
	if !errors.Is(err, domain.ErrRefuseIP) || !strings.Contains(err.Error(), "ko") {
		t.Fatalf("erreur = %v, attendu celle de « ko »", err)
	}
	for _, id := range []string{"ok1", "ok2"} {
		if _, ok := st.catalogue[id]; !ok {
			t.Errorf("le catalogue de %s n'a pas ete ecrit", id)
		}
	}
	if _, ok := st.catalogue["ko"]; ok {
		t.Error("« ko » ne devait rien ecrire")
	}
	if inactif.appels.Load() != 0 {
		t.Error("le fournisseur inactif a ete appele")
	}
}

// CA5 : en planification, l'echec de l'un n'arrete pas la synchro de l'autre.
func TestPlanificationUnEchecNArretePasLesAutres(t *testing.T) {
	h := &horlogeFausse{now: time.Date(2026, 3, 10, 4, 0, 0, 0, time.UTC)}
	st := nouveauStockage()
	ko, ok := &fauxFournisseur{id: "ko", erreur: domain.ErrAmont}, &fauxFournisseur{id: "ok"}
	s, _ := Nouveau(st, []Source{
		{DelaiMax: time.Minute, Reprise: 10 * time.Minute, Fournisseur: ko, Actif: true, AgeMax: time.Hour, Heure: "05:00"},
		{DelaiMax: time.Minute, Reprise: time.Hour, Fournisseur: ok, Actif: true, AgeMax: time.Hour, Heure: "05:00"},
	}, Options{Horloge: h, Lieu: time.UTC})
	ctx, annuler := context.WithCancel(context.Background())
	fini := make(chan struct{})
	go func() { s.Planifier(ctx); close(fini) }()
	attendre(t, "deux minuteurs", func() bool { h.mu.Lock(); defer h.mu.Unlock(); return len(h.minuteurs) == 2 })
	h.Avancer(time.Hour)
	attendre(t, "la synchro du fournisseur sain", func() bool { return st.nbEcritures() == 1 })
	annuler()
	<-fini
}

// CA6 : duree et volumes journalises, jamais d'identifiants.
func TestJournalDureeEtVolumesSansIdentifiants(t *testing.T) {
	var sortie tampon
	journal := slog.New(slog.NewTextHandler(&sortie, &slog.HandlerOptions{Level: slog.LevelDebug}))
	st, h := nouveauStockage(), &horlogeFausse{now: t0}
	ok := &fauxFournisseur{id: "a", nChaine: 4}
	ko := &fauxFournisseur{id: "b", erreur: domain.ErrIdentifiants}
	s, _ := Nouveau(st, []Source{actif(ok, time.Hour), actif(ko, time.Hour)}, Options{Horloge: h, Journal: journal, Lieu: time.UTC})
	s.SynchroniserTous(context.Background())

	log := sortie.String()
	for _, voulu := range []string{"catalogue synchronise", "fournisseur=a", "duree=", "chaines=4", "films=0", "series=0", "categories=0", "fournisseur=b"} {
		if !strings.Contains(log, voulu) {
			t.Errorf("le journal ne contient pas %q :\n%s", voulu, log)
		}
	}
	for _, interdit := range []string{"MOTDEPASSE", "SECRET", "utilisateur", "http://"} {
		if strings.Contains(log, interdit) {
			t.Errorf("le journal contient %q :\n%s", interdit, log)
		}
	}
}

func TestNouveauRefuseDoublonsEtHeureInvalide(t *testing.T) {
	st := nouveauStockage()
	a := &fauxFournisseur{id: "a"}
	if _, err := Nouveau(st, []Source{actif(a, 0), actif(a, 0)}, Options{}); err == nil {
		t.Error("doublon accepte")
	}
	if _, err := Nouveau(st, []Source{{DelaiMax: time.Minute, Reprise: time.Hour, Fournisseur: a, Heure: "25:99"}}, Options{}); err == nil {
		t.Error("heure invalide acceptee")
	}
	// Pas de reglage cache : DelaiMax et Reprise sont obligatoires.
	for _, src := range []Source{
		{Fournisseur: a, Reprise: time.Hour},
		{Fournisseur: a, DelaiMax: time.Minute},
		{Fournisseur: a, DelaiMax: -time.Second, Reprise: time.Hour},
	} {
		if _, err := Nouveau(st, []Source{src}, Options{}); err == nil {
			t.Errorf("source %+v acceptee", src)
		}
	}
}

// DelaiMax borne un telechargement dont plus personne ne depend... et surtout un
// fournisseur qui ne repond plus : le vol finit en erreur au lieu de rester ouvert.
func TestDelaiMaxBorneUnTelechargementBloque(t *testing.T) {
	st, h := nouveauStockage(), &horlogeFausse{now: t0}
	f := &fauxFournisseur{id: "a", bloque: make(chan struct{})} // ne repond jamais
	s := nouveau(t, st, h, Source{DelaiMax: 50 * time.Millisecond, Reprise: time.Hour, Fournisseur: f, Actif: true, AgeMax: time.Hour})
	err := s.Synchroniser(context.Background(), "a")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("erreur = %v, attendu context.DeadlineExceeded", err)
	}
	if st.nbEcritures() != 0 {
		t.Fatal("rien ne devait etre ecrit")
	}
}

// US-14 CA1 : la synchronisation publie ses mesures par l'interface Instrumentation,
// avec des etiquettes bornees (fournisseur de la config, resultat fixe).
func TestInstrumentationDeLaSynchronisation(t *testing.T) {
	rec := &instrum.Enregistreur{}
	st, h, f := nouveauStockage(), &horlogeFausse{now: t0}, &fauxFournisseur{id: "a", nChaine: 2}
	s, err := Nouveau(st, []Source{actif(f, time.Hour)}, Options{Horloge: h, Lieu: time.UTC, Instrumentation: rec})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.Synchroniser(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	f.erreur = errors.New("panne http://u:motdepasse@f.test/x")
	if err := s.Synchroniser(ctx, "a"); err == nil {
		t.Fatal("erreur attendue")
	}
	if rec.Total("synchro_total", "fournisseur", "a", "resultat", "ok") != 1 ||
		rec.Total("synchro_total", "fournisseur", "a", "resultat", "echec_telechargement") != 1 {
		t.Errorf("mesures : %+v", rec.Mesures())
	}
	if rec.Observations("synchro_duree_secondes", "fournisseur", "a") != 2 {
		t.Errorf("durees : %+v", rec.Mesures())
	}
	rec.VerifierBornees(t, "motdepasse", "f.test")
}

type observateurTest struct {
	mu   sync.Mutex
	vues []error
	ids  []string
}

func (o *observateurTest) Synchro(_ context.Context, id string, err error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.vues, o.ids = append(o.vues, err), append(o.ids, id)
}

// US-25 CA2 : l'observateur apprend chaque tentative, avec l'erreur typee du fournisseur
// (le compte refuse est distinguable d'une panne).
func TestObservateurRecoitLeResultatDeChaqueTentative(t *testing.T) {
	obs := &observateurTest{}
	f := &fauxFournisseur{id: "a", nChaine: 1}
	s, err := Nouveau(nouveauStockage(), []Source{actif(f, time.Hour)}, Options{Horloge: &horlogeFausse{now: t0}, Lieu: time.UTC, Observateur: obs})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Synchroniser(context.Background(), "a"); err != nil {
		t.Fatal(err)
	}
	f.erreur = domain.ErrIdentifiants
	if err := s.Synchroniser(context.Background(), "a"); err == nil {
		t.Fatal("echec attendu")
	}
	if len(obs.vues) != 2 || obs.ids[0] != "a" || obs.vues[0] != nil || !errors.Is(obs.vues[1], domain.ErrIdentifiants) {
		t.Errorf("observations : %v", obs.vues)
	}
}
