package occupation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"runtime"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"mytv/internal/domain"
	"mytv/internal/testutil/instrum"
)

// horloge manuelle simple : seul le suivi la lit.
type horloge struct {
	mu sync.Mutex
	t  time.Time
}

func (h *horloge) now() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.t }
func (h *horloge) avance(d time.Duration) {
	h.mu.Lock()
	h.t = h.t.Add(d)
	h.mu.Unlock()
}

func reglagesTest() Reglages {
	return Reglages{CacheOccupation: 15 * time.Second, DelaiInfo: time.Second, RetryApres: 15 * time.Second,
		RetryApresMax: 60 * time.Second, GardeFournisseur: 100 * time.Second, SuccesPrise: 2, FenetrePrise: 30 * time.Second}
}

type banc struct {
	*Suivi
	h       *horloge
	inst    *instrum.Enregistreur
	appels  atomic.Int32
	info    atomic.Pointer[Info]
	erreur  atomic.Bool
	retient chan struct{} // non nil : Info attend sa fermeture
	autres  atomic.Int32
}

func nouveau(t *testing.T) *banc {
	t.Helper()
	b := &banc{h: &horloge{t: time.Unix(10_000, 0)}, inst: &instrum.Enregistreur{}}
	b.info.Store(&Info{ConnexionsMax: 1, ConnexionsActives: 1})
	b.Suivi = Nouveau(Options{Reglages: reglagesTest(), Horloge: b.h.now, Instrumentation: b.inst,
		Sources: map[string]Source{"f": {
			Info: func(ctx context.Context) (Info, error) {
				b.appels.Add(1)
				if b.retient != nil {
					select {
					case <-b.retient:
					case <-ctx.Done():
						return Info{}, ctx.Err()
					}
				}
				if b.erreur.Load() {
					return Info{}, domain.ErrAmont
				}
				return *b.info.Load(), nil
			},
			AutresBaux: func(string) int { return int(b.autres.Load()) },
		}}})
	t.Cleanup(b.Fermer)
	return b
}

func dem(ressource string) Demande {
	return Demande{Fournisseur: "f", Session: "s", Ressource: ressource, Direct: true}
}

// US-34 CA3 : le diagnostic ne lit user_info qu'une fois pour N demandes simultanees
// (singleflight), puis ne le relit pas pendant cache_occupation, et une panne est aussi
// mise en cache : il n'aggrave jamais la situation chez le fournisseur.
func TestCA3_UserInfoLuUneSeuleFoisParIntervalle(t *testing.T) {
	b := nouveau(t)
	b.retient = make(chan struct{})
	var wg sync.WaitGroup
	classes := make([]Classe, 20)
	for i := range classes {
		wg.Add(1)
		go func() { defer wg.Done(); classes[i] = b.Diagnostiquer(context.Background(), dem("c")) }()
	}
	for b.appels.Load() == 0 {
		runtime.Gosched()
	}
	time.Sleep(20 * time.Millisecond) // laisse les autres s'agglutiner sur la requete en vol
	close(b.retient)
	wg.Wait()
	if n := b.appels.Load(); n != 1 {
		t.Fatalf("%d lectures simultanees de user_info, attendu 1 (singleflight)", n)
	}
	for _, c := range classes {
		if c != Ailleurs {
			t.Fatalf("classe = %s", c)
		}
	}
	b.h.avance(14 * time.Second)
	b.Diagnostiquer(context.Background(), dem("c"))
	if n := b.appels.Load(); n != 1 {
		t.Errorf("relu dans l'intervalle de cache : %d", n)
	}
	b.h.avance(2 * time.Second)
	b.erreur.Store(true)
	if c := b.Diagnostiquer(context.Background(), dem("c")); c != Occupe {
		t.Errorf("user_info en panne : %s", c)
	}
	if n := b.appels.Load(); n != 2 {
		t.Fatalf("apres l'intervalle : %d lectures, attendu 2", n)
	}
	for i := 0; i < 5; i++ { // l'echec est mis en cache : on ne reinterroge pas un fournisseur en peine
		b.Diagnostiquer(context.Background(), dem("c"))
	}
	if n := b.appels.Load(); n != 2 {
		t.Errorf("echec non mis en cache : %d lectures", n)
	}
}

// US-34 CA3 : les trois classes.
func TestCA3_Classification(t *testing.T) {
	cas := []struct {
		nom    string
		prepa  func(b *banc)
		attend Classe
		appels int32
	}{
		{"toutes les places prises, mytv n'en tient aucune", func(b *banc) {}, Ailleurs, 1},
		{"une autre session mytv tient un bail", func(b *banc) { b.autres.Store(1) }, ParMytv, 0},
		{"places libres cote fournisseur", func(b *banc) { b.info.Store(&Info{ConnexionsMax: 1, ConnexionsActives: 0}) }, Occupe, 1},
		{"user_info en panne", func(b *banc) { b.erreur.Store(true) }, Occupe, 1},
		{"max_connections absent", func(b *banc) { b.info.Store(&Info{}) }, Occupe, 1},
		{"mytv vient d'utiliser le compte pour autre chose (place gardee)", func(b *banc) {
			b.Succes(dem("autre"))
			b.h.avance(10 * time.Second)
		}, Occupe, 0},
		{"la place gardee par mytv est oubliee apres la garde", func(b *banc) {
			b.Succes(dem("autre"))
			b.h.avance(101 * time.Second)
		}, Ailleurs, 1},
		{"meme chaine qui reussissait : un autre l'a prise", func(b *banc) { b.Succes(dem("c")) }, Ailleurs, 1},
	}
	for _, c := range cas {
		t.Run(c.nom, func(t *testing.T) {
			b := nouveau(t)
			c.prepa(b)
			if got := b.Diagnostiquer(context.Background(), dem("c")); got != c.attend {
				t.Errorf("classe = %s, attendu %s", got, c.attend)
			}
			if n := b.appels.Load(); n != c.appels {
				t.Errorf("%d lectures de user_info, attendu %d", n, c.appels)
			}
		})
	}
}

// Pour un film, un seek rouvre une connexion que le fournisseur garde encore : meme
// ressource ne veut PAS dire « un autre l'a prise ».
func TestCA3_FilmMemeRessourceRecentePlaceGardee(t *testing.T) {
	b := nouveau(t)
	d := Demande{Fournisseur: "f", Session: "s", Ressource: "f:1"}
	b.Succes(d)
	if got := b.Diagnostiquer(context.Background(), d); got != Occupe {
		t.Errorf("seek : %s", got)
	}
}

// Un client qui part pendant la lecture de user_info n'interrompt pas la requete des autres
// et ne laisse aucune goroutine.
func TestCA3_ClientParti_PasDeFuiteEtLesAutresRecoiventLeResultat(t *testing.T) {
	avant := runtime.NumGoroutine()
	b := nouveau(t)
	b.retient = make(chan struct{})
	ctx, annule := context.WithCancel(context.Background())
	fini := make(chan Classe, 1)
	go func() { fini <- b.Diagnostiquer(ctx, dem("c")) }()
	for b.appels.Load() == 0 {
		runtime.Gosched()
	}
	autre := make(chan Classe, 1)
	go func() { autre <- b.Diagnostiquer(context.Background(), dem("c")) }()
	time.Sleep(20 * time.Millisecond)
	annule()
	if c := <-fini; c != Occupe {
		t.Errorf("client parti : %s", c)
	}
	close(b.retient)
	if c := <-autre; c != Ailleurs {
		t.Errorf("l'autre demandeur doit avoir le resultat : %s", c)
	}
	b.Fermer()
	time.Sleep(50 * time.Millisecond)
	if apres := runtime.NumGoroutine(); apres > avant+1 {
		t.Errorf("goroutines : %d avant, %d apres", avant, apres)
	}
}

// US-34 CA2 : Retry-After croissant 15, 30, 60, 60 ; chaque (session, chaine) a sa serie ; un succes remet a zero.
func TestCA2_RetryApresCroissantParSessionEtChaine(t *testing.T) {
	b := nouveau(t)
	var vus []time.Duration
	for i := 0; i < 5; i++ {
		vus = append(vus, b.RetryApres(dem("c")))
	}
	if want := []time.Duration{15, 30, 60, 60, 60}; !slices.EqualFunc(vus, want, func(a, w time.Duration) bool { return a == w*time.Second }) {
		t.Errorf("suite = %v", vus)
	}
	if d := b.RetryApres(dem("autre")); d != 15*time.Second {
		t.Errorf("autre chaine : %v", d)
	}
	autreSession := dem("c")
	autreSession.Session = "t"
	if d := b.RetryApres(autreSession); d != 15*time.Second {
		t.Errorf("autre session : %v", d)
	}
	b.Succes(dem("c"))
	if d := b.RetryApres(dem("c")); d != 15*time.Second {
		t.Errorf("apres un succes : %v", d)
	}
}

// US-34 CA4 : detection d'une prise de place.
func TestCA4_PriseDePlace(t *testing.T) {
	b := nouveau(t)
	d := dem("c")
	if b.Refus(d, "407") {
		t.Fatal("refus sans lecture precedente : pas une prise (zapping)")
	}
	b.Succes(d)
	if b.Refus(d, "407") {
		t.Fatal("un seul succes ne suffit pas")
	}
	b.Succes(d)
	b.Succes(d)
	if !b.Refus(d, "407") {
		t.Fatal("refus apres des succes consecutifs : prise de place")
	}
	if b.Refus(d, "407") {
		t.Fatal("une fois par episode")
	}
	e := b.Etat("f")
	if e.Occupation != "ailleurs" || e.Depuis == nil || e.DernierePrise == nil {
		t.Fatalf("etat : %+v", e)
	}
	if n := b.inst.Total("live_prises_de_place_total", "fournisseur", "f"); n != 1 {
		t.Errorf("metrique = %v", n)
	}
	b.Succes(d) // la place revient
	if e = b.Etat("f"); e.Occupation != "libre" || e.DernierePrise == nil {
		t.Fatalf("apres la reprise : %+v", e)
	}
	// Nouvel episode possible apres une nouvelle serie de succes.
	b.Succes(d)
	if !b.Refus(d, "403") {
		t.Fatal("deuxieme episode non detecte")
	}
	if n := b.inst.Total("live_prises_de_place_total"); n != 2 {
		t.Errorf("metrique = %v", n)
	}
}

// US-34 CA4 : des succes trop anciens (lecteur en pause) ne font pas une prise de place.
func TestCA4_SuccesTropAnciensNeFontPasUnePrise(t *testing.T) {
	b := nouveau(t)
	d := dem("c")
	b.Succes(d)
	b.Succes(d)
	b.h.avance(31 * time.Second)
	if b.Refus(d, "403") {
		t.Fatal("segment perime d'un lecteur en pause")
	}
}

// L'etat « ailleurs » n'est plus affirme quand plus aucune preuve n'arrive.
func TestEtatAilleursEstOublieSansNouvelleLecture(t *testing.T) {
	b := nouveau(t)
	b.Diagnostiquer(context.Background(), dem("c"))
	if e := b.Etat("f"); e.Occupation != "ailleurs" {
		t.Fatalf("%+v", e)
	}
	b.h.avance(6 * time.Minute)
	if e := b.Etat("f"); e.Occupation != "libre" {
		t.Errorf("%+v", e)
	}
}

func TestEtatLibreDepuisToujoursPuisMytv(t *testing.T) {
	var occupes atomic.Int32
	h := &horloge{t: time.Unix(10_000, 0)}
	s := Nouveau(Options{Reglages: reglagesTest(), Horloge: h.now, Sources: map[string]Source{"f": {Occupes: func() int { return int(occupes.Load()) }}}})
	defer s.Fermer()
	if e := s.Etat("f"); e.Occupation != "libre" || e.Depuis != nil {
		t.Errorf("%+v", e)
	}
	occupes.Store(1)
	h.avance(time.Minute)
	e := s.Etat("f")
	if e.Occupation != "mytv" || e.Depuis == nil || !e.Depuis.Equal(h.now()) {
		t.Errorf("%+v", e)
	}
}

// Un Suivi nil (dev, tests d'autres paquets) ne plante pas et repond avec des valeurs sures.
func TestSuiviNilEstInoffensif(t *testing.T) {
	var s *Suivi
	s.Succes(dem("c"))
	if s.Refus(dem("c"), "407") || s.Diagnostiquer(context.Background(), dem("c")) != Occupe || s.Etat("f").Occupation != "libre" {
		t.Error("valeurs sures attendues")
	}
	s.Fermer()
	rec := httptest.NewRecorder()
	if !s.Repondre(rec, dem("c"), domain.ErrRefuseConnexions) || rec.Code != 503 || rec.Header().Get("Retry-After") != "15" {
		t.Errorf("%d %v", rec.Code, rec.Header())
	}
}

func TestReponseCodesEtErreurs(t *testing.T) {
	b := nouveau(t)
	cas := []struct {
		err  error
		code string
	}{
		{&ErrOccupe{Classe: Ailleurs, Err: domain.ErrRefuseConnexions}, "compte_utilise_ailleurs"},
		{fmt.Errorf("lecture : %w", &ErrOccupe{Classe: ParMytv, Err: domain.ErrRefuseConnexions}), "compte_utilise_par_mytv"},
		{domain.ErrRefuseConnexions, "compte_occupe"},
		{domain.ErrPlaceIndisponible, "compte_utilise_par_mytv"},
	}
	for _, c := range cas {
		rec := httptest.NewRecorder()
		if !b.Repondre(rec, dem(c.code), c.err) {
			t.Fatalf("%v non traite", c.err)
		}
		var corps struct{ Code, Message string }
		_ = json.Unmarshal(rec.Body.Bytes(), &corps)
		if rec.Code != 503 || corps.Code != c.code || rec.Header().Get("X-Mytv-Erreur") != c.code || corps.Message == "" {
			t.Errorf("%v : %d %s %v", c.err, rec.Code, rec.Body.String(), rec.Header())
		}
	}
	if b.Repondre(httptest.NewRecorder(), dem("x"), errors.New("autre")) {
		t.Error("une autre erreur n'est pas une occupation")
	}
	if !errors.Is(&ErrOccupe{Classe: Occupe, Err: domain.ErrRefuseConnexions}, domain.ErrRefuseConnexions) {
		t.Error("ErrOccupe doit rester un ErrRefuseConnexions")
	}
}

// US-34 CA3 : conformite avec docs/openapi.yaml (reponse « Occupe »).
func TestCA3_ReponseConformeAuContrat(t *testing.T) {
	brut, err := os.ReadFile("../../docs/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var c struct {
		Components struct {
			Responses struct {
				Occupe struct {
					Headers map[string]struct {
						Schema struct {
							Enum []yaml.Node `yaml:"enum"`
						} `yaml:"schema"`
					} `yaml:"headers"`
				} `yaml:"Occupe"`
			} `yaml:"responses"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(brut, &c); err != nil {
		t.Fatal(err)
	}
	enum := func(h string) []string {
		var v []string
		for _, n := range c.Components.Responses.Occupe.Headers[h].Schema.Enum {
			v = append(v, n.Value)
		}
		return v
	}
	codes, retries := enum("X-Mytv-Erreur"), enum("Retry-After")
	if len(codes) == 0 || len(retries) == 0 {
		t.Fatalf("contrat sans enum : %v %v", codes, retries)
	}
	b := nouveau(t)
	for _, classe := range []Classe{Ailleurs, ParMytv, Occupe} {
		for i := 0; i < 5; i++ {
			rec := httptest.NewRecorder()
			b.Repondre(rec, dem(string(classe)), &ErrOccupe{Classe: classe, Err: domain.ErrRefuseConnexions})
			if !slices.Contains(codes, rec.Header().Get("X-Mytv-Erreur")) {
				t.Errorf("code %q hors contrat %v", rec.Header().Get("X-Mytv-Erreur"), codes)
			}
			if ra := rec.Header().Get("Retry-After"); !slices.Contains(retries, ra) {
				t.Errorf("Retry-After %q hors contrat %v", ra, retries)
			} else if _, err := strconv.Atoi(ra); err != nil {
				t.Error(err)
			}
		}
	}
	rec := httptest.NewRecorder()
	b.RepondreCode(rec, dem("x"), CodeOccupe)
	if !slices.Contains(codes, rec.Header().Get("X-Mytv-Erreur")) {
		t.Errorf("code occupe hors contrat")
	}
}
