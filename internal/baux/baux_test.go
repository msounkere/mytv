package baux

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mytv/internal/config"
	"mytv/internal/domain"
	"mytv/internal/testutil/instrum"
)

var depart = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

func reglages() config.Bail {
	return config.Bail{
		TTL:         config.Duree(90 * time.Second),
		PatienceMax: config.Duree(30 * time.Second),
		Reessai:     config.Duree(3 * time.Second),
	}
}

func nouveau(t *testing.T, max map[string]int) (*Gestionnaire, *HorlogeManuelle) {
	t.Helper()
	h := NouvelleHorlogeManuelle(depart)
	g, err := Nouveau(reglages(), max, h)
	if err != nil {
		t.Fatal(err)
	}
	return g, h
}

type resultat struct {
	b   domain.Bail
	err error
}

// attendreMinuteurs bloque jusqu'a ce que n goroutines dorment sur l'horloge :
// c'est le seul moyen sur de faire avancer l'heure sans course.
func attendreMinuteurs(t *testing.T, h *HorlogeManuelle, n int) {
	t.Helper()
	fin := time.Now().Add(5 * time.Second)
	for h.Minuteurs() < n {
		if time.Now().After(fin) {
			t.Fatalf("%d minuteur(s) attendu(s), %d present(s)", n, h.Minuteurs())
		}
		time.Sleep(time.Millisecond)
	}
}

func acquerirAsync(g *Gestionnaire, ctx context.Context, f, s, r string) <-chan resultat {
	c := make(chan resultat, 1)
	go func() {
		b, err := g.Acquerir(ctx, f, s, r)
		c <- resultat{b, err}
	}()
	return c
}

func attendre(t *testing.T, c <-chan resultat) resultat {
	t.Helper()
	select {
	case r := <-c:
		return r
	case <-time.After(5 * time.Second):
		t.Fatal("Acquerir ne rend pas la main")
		return resultat{}
	}
}

// CA1
func TestAuPlusConnexionsMaxBauxSimultanes(t *testing.T) {
	g, _ := nouveau(t, map[string]int{"a": 2})
	ctx := context.Background()
	for _, s := range []string{"s1", "s2"} {
		if _, err := g.Acquerir(ctx, "a", s, "1"); err != nil {
			t.Fatal(err)
		}
	}
	if g.Occupes("a") != 2 {
		t.Errorf("occupes = %d", g.Occupes("a"))
	}
	// Une troisieme session attend, puis echoue une fois la patience ecoulee.
	c := acquerirAsync(g, ctx, "a", "s3", "1")
	h := g.horloge.(*HorlogeManuelle)
	for i := 0; i < 10; i++ {
		attendreMinuteurs(t, h, 1)
		h.Avancer(3 * time.Second)
	}
	if r := attendre(t, c); !errors.Is(r.err, domain.ErrPlaceIndisponible) {
		t.Errorf("attendu ErrPlaceIndisponible, obtenu %v", r.err)
	}
	if g.Occupes("a") != 2 {
		t.Errorf("l'echec ne doit pas consommer de place : %d", g.Occupes("a"))
	}
}

// CA1, CA5 : sous charge concurrente, le nombre de baux simultanes ne depasse jamais le maximum.
func TestConcurrenceNeDepasseJamaisLeMaximum(t *testing.T) {
	const max, sessions, tours = 3, 24, 15
	g, err := Nouveau(config.Bail{
		TTL: config.Duree(time.Minute), PatienceMax: config.Duree(30 * time.Second), Reessai: config.Duree(2 * time.Millisecond),
	}, map[string]int{"a": max}, nil)
	if err != nil {
		t.Fatal(err)
	}
	avant := runtime.NumGoroutine()
	var dedans, pic, acquis atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < sessions; i++ {
		wg.Add(1)
		go func(s string) {
			defer wg.Done()
			for j := 0; j < tours; j++ {
				b, err := g.Acquerir(context.Background(), "a", s, "r")
				if err != nil {
					t.Errorf("session %s : %v", s, err)
					return
				}
				n := dedans.Add(1)
				for { // pic = max(pic, n)
					p := pic.Load()
					if n <= p || pic.CompareAndSwap(p, n) {
						break
					}
				}
				acquis.Add(1)
				time.Sleep(100 * time.Microsecond)
				b.Renouveler()
				dedans.Add(-1)
				b.Liberer()
			}
		}(string(rune('A' + i)))
	}
	wg.Wait()
	if pic.Load() > max {
		t.Errorf("jusqu'a %d baux simultanes pour un maximum de %d", pic.Load(), max)
	}
	if acquis.Load() != sessions*tours {
		t.Errorf("%d acquisitions sur %d", acquis.Load(), sessions*tours)
	}
	if g.Occupes("a") != 0 {
		t.Errorf("%d places encore occupees", g.Occupes("a"))
	}
	verifierPasDeFuite(t, avant)
}

// verifierPasDeFuite laisse aux goroutines le temps de se terminer, puis compare.
func verifierPasDeFuite(t *testing.T, avant int) {
	t.Helper()
	fin := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > avant {
		if time.Now().After(fin) {
			buf := make([]byte, 1<<16)
			t.Fatalf("fuite de goroutines : %d avant, %d apres\n%s", avant, runtime.NumGoroutine(), buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// CA2
func TestZappingReutiliseLeBailSansCompterDeuxPlaces(t *testing.T) {
	g, _ := nouveau(t, map[string]int{"a": 1})
	ctx := context.Background()
	b1, err := g.Acquerir(ctx, "a", "s1", "chaine-1")
	if err != nil {
		t.Fatal(err)
	}
	b2, err := g.Acquerir(ctx, "a", "s1", "chaine-2") // maximum atteint, mais c'est la meme session
	if err != nil {
		t.Fatalf("le zapping ne doit pas attendre : %v", err)
	}
	if b1 != b2 {
		t.Error("le meme bail doit etre reutilise")
	}
	if g.Occupes("a") != 1 {
		t.Errorf("occupes = %d, attendu 1", g.Occupes("a"))
	}
	// Une autre session reste bloquee tant que la premiere detient la place.
	g2 := acquerirAsync(g, ctx, "a", "s2", "x")
	h := g.horloge.(*HorlogeManuelle)
	attendreMinuteurs(t, h, 1)
	b2.Liberer() // la liberation reveille l'attente sans attendre le reessai
	if r := attendre(t, g2); r.err != nil {
		t.Errorf("la place rendue aurait du servir s2 : %v", r.err)
	}
}

// CA2 : le zapping prolonge aussi le bail.
func TestZappingRenouvelleLExpiration(t *testing.T) {
	g, h := nouveau(t, map[string]int{"a": 1})
	ctx := context.Background()
	g.Acquerir(ctx, "a", "s1", "1")
	h.Avancer(80 * time.Second)
	g.Acquerir(ctx, "a", "s1", "2")
	h.Avancer(80 * time.Second) // 160 s depuis le debut, mais 80 s depuis le zapping
	if g.Occupes("a") != 1 {
		t.Error("le bail aurait du survivre grace au zapping")
	}
}

// CA3
func TestBailNonRenouveleExpireApresLeTTL(t *testing.T) {
	g, h := nouveau(t, map[string]int{"a": 1})
	b, _ := g.Acquerir(context.Background(), "a", "s1", "1")
	h.Avancer(89 * time.Second)
	if g.Occupes("a") != 1 {
		t.Fatal("bail expire trop tot")
	}
	b.Renouveler() // repart pour 90 s
	h.Avancer(89 * time.Second)
	if g.Occupes("a") != 1 {
		t.Fatal("le renouvellement n'a pas prolonge le bail")
	}
	h.Avancer(time.Second)
	if g.Occupes("a") != 0 {
		t.Error("le bail aurait du expirer 90 s apres le dernier renouvellement")
	}
}

// CA3 : un bail expire ne ressuscite pas en le renouvelant (sa place a pu etre reprise).
func TestRenouvelerUnBailExpireNeLeRessuscitePas(t *testing.T) {
	g, h := nouveau(t, map[string]int{"a": 1})
	ctx := context.Background()
	vieux, _ := g.Acquerir(ctx, "a", "s1", "1")
	h.Avancer(91 * time.Second)
	nouveauBail, err := g.Acquerir(ctx, "a", "s2", "1")
	if err != nil {
		t.Fatal(err)
	}
	vieux.Renouveler()
	vieux.Liberer()
	if g.Occupes("a") != 1 {
		t.Error("le vieux bail a perturbe la place de s2")
	}
	nouveauBail.Liberer()
}

// CA3
func TestLibererEstImmediatEtIdempotent(t *testing.T) {
	g, _ := nouveau(t, map[string]int{"a": 1})
	ctx := context.Background()
	b1, _ := g.Acquerir(ctx, "a", "s1", "1")
	b1.Liberer()
	if g.Occupes("a") != 0 {
		t.Fatal("Liberer doit rendre la place tout de suite")
	}
	b2, err := g.Acquerir(ctx, "a", "s2", "1")
	if err != nil {
		t.Fatal(err)
	}
	b1.Liberer() // deuxieme appel : ne doit pas voler la place de s2
	b1.Liberer()
	if g.Occupes("a") != 1 {
		t.Error("une liberation en double a rendu la place d'une autre session")
	}
	// La meme session reprend une place : l'ancien bail reste inerte.
	b2.Liberer()
	b3, _ := g.Acquerir(ctx, "a", "s2", "2")
	b2.Liberer()
	if g.Occupes("a") != 1 {
		t.Error("l'ancien bail a libere le nouveau")
	}
	b3.Liberer()
}

// CA4 : une place qui expire pendant l'attente est prise au reessai suivant.
func TestAttenteObtientLaPlaceExpireeAuReessaiSuivant(t *testing.T) {
	r := reglages()
	r.PatienceMax = config.Duree(2 * time.Minute)
	h := NouvelleHorlogeManuelle(depart)
	g, _ := Nouveau(r, map[string]int{"a": 1}, h)
	ctx := context.Background()
	g.Acquerir(ctx, "a", "s1", "1") // expire a t+90 s
	c := acquerirAsync(g, ctx, "a", "s2", "1")
	for i := 1; i <= 30; i++ { // t = 3, 6, ... 90 s : un essai toutes les 3 s
		attendreMinuteurs(t, h, 1)
		h.Avancer(3 * time.Second)
		if i < 30 {
			select {
			case r := <-c:
				t.Fatalf("obtenu trop tot a t=%ds : %v", 3*i, r.err)
			case <-time.After(5 * time.Millisecond):
			}
		}
	}
	res := attendre(t, c)
	if res.err != nil {
		t.Fatalf("la place expiree aurait du etre prise : %v", res.err)
	}
	if h.Maintenant().Sub(depart) != 90*time.Second {
		t.Errorf("heure = %v", h.Maintenant().Sub(depart))
	}
}

// CA4 : annulation par ctx, sans fuite de goroutine.
func TestAnnulationDuContexteInterrompUneAttente(t *testing.T) {
	avant := runtime.NumGoroutine()
	g, h := nouveau(t, map[string]int{"a": 1})
	g.Acquerir(context.Background(), "a", "s1", "1")
	ctx, annuler := context.WithCancel(context.Background())
	c := acquerirAsync(g, ctx, "a", "s2", "1")
	attendreMinuteurs(t, h, 1)
	annuler()
	r := attendre(t, c)
	if !errors.Is(r.err, context.Canceled) {
		t.Errorf("attendu context.Canceled, obtenu %v", r.err)
	}
	if g.Occupes("a") != 1 {
		t.Error("une attente annulee ne doit pas laisser de bail")
	}
	// Un contexte deja annule n'attend meme pas.
	if _, err := g.Acquerir(ctx, "a", "s3", "1"); !errors.Is(err, context.Canceled) {
		t.Errorf("contexte annule d'avance : %v", err)
	}
	verifierPasDeFuite(t, avant)
}

// CA4 : patience nulle = refus immediat.
func TestSansPatienceLeRefusEstImmediat(t *testing.T) {
	g, _ := Nouveau(config.Bail{TTL: config.Duree(time.Minute)}, map[string]int{"a": 1}, NouvelleHorlogeManuelle(depart))
	g.Acquerir(context.Background(), "a", "s1", "1")
	if _, err := g.Acquerir(context.Background(), "a", "s2", "1"); !errors.Is(err, domain.ErrPlaceIndisponible) {
		t.Errorf("attendu ErrPlaceIndisponible, obtenu %v", err)
	}
}

// CA6
func TestFournisseursIndependants(t *testing.T) {
	g, _ := nouveau(t, map[string]int{"a": 1, "b": 1})
	ctx := context.Background()
	if _, err := g.Acquerir(ctx, "a", "s1", "1"); err != nil {
		t.Fatal(err)
	}
	// "a" est sature : "b" doit repondre sans attendre (horloge figee : une attente bloquerait).
	if _, err := g.Acquerir(ctx, "b", "s2", "1"); err != nil {
		t.Errorf("saturer a ne doit pas bloquer b : %v", err)
	}
	if g.Occupes("a") != 1 || g.Occupes("b") != 1 {
		t.Errorf("occupes a=%d b=%d", g.Occupes("a"), g.Occupes("b"))
	}
}

func TestFournisseurInconnuEtMaximumInvalide(t *testing.T) {
	g, _ := nouveau(t, map[string]int{"a": 1})
	if _, err := g.Acquerir(context.Background(), "zzz", "s", "r"); !errors.Is(err, ErrFournisseurInconnu) {
		t.Errorf("attendu ErrFournisseurInconnu, obtenu %v", err)
	}
	if _, err := Nouveau(reglages(), map[string]int{"a": 0}, nil); err == nil {
		t.Error("connexions_max = 0 doit etre refuse")
	}
}

// US-10 CA10 : Terminer libere tous les baux de la session (tous fournisseurs),
// reveille une attente, est idempotent et n'affecte pas les autres sessions.
func TestTerminerLiberePartoutEtReveilleLAttente(t *testing.T) {
	g, h := nouveau(t, map[string]int{"a": 1, "b": 1})
	ctx := context.Background()
	if _, err := g.Acquerir(ctx, "a", "s1", "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Acquerir(ctx, "b", "s1", "9"); err != nil {
		t.Fatal(err)
	}
	var term domain.Terminateur = g
	// Une autre session attend la place de s1 chez « a » : Terminer la lui donne
	// sans attendre le TTL ni le reessai (l'horloge manuelle n'avance pas).
	attente := acquerirAsync(g, ctx, "a", "s2", "5")
	attendreMinuteurs(t, h, 1)
	term.Terminer("s1")
	if r := attendre(t, attente); r.err != nil {
		t.Fatalf("la place liberee n'a pas ete donnee : %v", r.err)
	}
	if g.Occupes("b") != 0 || g.Occupes("a") != 1 {
		t.Errorf("occupes a=%d b=%d", g.Occupes("a"), g.Occupes("b"))
	}
	term.Terminer("s1") // idempotent
	term.Terminer("inconnue")
	if g.Occupes("a") != 1 {
		t.Error("Terminer a touche une autre session")
	}
}

// US-14 CA1 : mesures publiees par interface, etiquettes bornees (pas de session).
func TestInstrumentationDesBaux(t *testing.T) {
	rec := &instrum.Enregistreur{}
	g, h := nouveau(t, map[string]int{"a": 1})
	g.AvecInstrumentation(rec)
	ctx := context.Background()
	b, err := g.Acquerir(ctx, "a", "session-secrete-1", "chaine-12")
	if err != nil {
		t.Fatal(err)
	}
	// Place prise : une autre session attend la patience entiere puis est refusee.
	refus := acquerirAsync(g, ctx, "a", "session-secrete-2", "chaine-13")
	attendreMinuteurs(t, h, 1)
	for i := 0; i < 20; i++ {
		h.Avancer(3 * time.Second)
		select {
		case r := <-refus:
			if !errors.Is(r.err, domain.ErrPlaceIndisponible) {
				t.Fatalf("erreur = %v", r.err)
			}
			goto fini
		case <-time.After(20 * time.Millisecond):
		}
	}
	t.Fatal("pas de refus")
fini:
	b.Liberer()
	g.Terminer("session-secrete-1")
	g.Acquerir(ctx, "inconnu-xyz", "s", "r")
	if rec.Total("baux_acquisitions_total", "fournisseur", "a", "resultat", "ok") != 1 ||
		rec.Total("baux_acquisitions_total", "fournisseur", "a", "resultat", "refuse") != 1 {
		t.Errorf("acquisitions : %+v", rec.Mesures())
	}
	if rec.Observations("baux_attente_secondes", "fournisseur", "a") != 2 || rec.Observations("baux_occupes") != 1 {
		t.Errorf("histogrammes : %+v", rec.Mesures())
	}
	if rec.Total("baux_acquisitions_total", "fournisseur", "inconnu", "resultat", "annule") != 1 {
		t.Error("un fournisseur inconnu doit etre agrege sous « inconnu »")
	}
	rec.VerifierBornees(t, "session-secrete", "chaine-1", "inconnu-xyz")
}

// LibererSi rend la place seulement si le bail sert encore la ressource.
func TestLibererSiNeRendQueLaRessourceCourante(t *testing.T) {
	g, h := nouveau(t, map[string]int{"f": 1})
	ctx := context.Background()
	if _, err := g.Acquerir(ctx, "f", "s1", "chaine-1"); err != nil {
		t.Fatal(err)
	}
	if g.LibererSi("f", "s1", "autre") || g.Occupes("f") != 1 {
		t.Error("une autre ressource ne doit rien liberer")
	}
	if g.LibererSi("f", "s2", "chaine-1") || g.Occupes("f") != 1 {
		t.Error("une autre session ne doit rien liberer")
	}
	if g.LibererSi("inconnu", "s1", "chaine-1") {
		t.Error("fournisseur inconnu")
	}
	// Zapping : le bail est reutilise pour la nouvelle ressource.
	if _, err := g.Acquerir(ctx, "f", "s1", "chaine-2"); err != nil {
		t.Fatal(err)
	}
	if g.LibererSi("f", "s1", "chaine-1") || g.Occupes("f") != 1 {
		t.Error("l'ancienne ressource ne doit pas couper la nouvelle")
	}
	if !g.LibererSi("f", "s1", "chaine-2") || g.Occupes("f") != 0 {
		t.Error("la ressource courante doit rendre la place")
	}
	if g.LibererSi("f", "s1", "chaine-2") {
		t.Error("idempotent : deuxieme appel sans effet")
	}
	// Un bail expire n'est pas « libere » : il est deja perdu.
	if _, err := g.Acquerir(ctx, "f", "s1", "chaine-3"); err != nil {
		t.Fatal(err)
	}
	h.Avancer(2 * time.Minute)
	if g.LibererSi("f", "s1", "chaine-3") {
		t.Error("bail expire")
	}
	// La place libre sert tout de suite a quelqu'un d'autre.
	if _, err := g.Acquerir(ctx, "f", "s2", "x"); err != nil {
		t.Fatal(err)
	}
	if !g.LibererSi("f", "s2", "x") {
		t.Error("liberation du bail de s2")
	}
}
