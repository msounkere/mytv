package vod

import (
	"context"
	"errors"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"mytv/internal/domain"
	"mytv/internal/occupation"
	"mytv/internal/remux"
	"mytv/internal/testutil/fauxiptv"
)

// servirAvecOccupation rejoue Servir avec un diagnostic branche (le banc ne l'a pas).
func servirAvecOccupation(t *testing.T, b *banc, rg Reglages, lectures *atomic.Int32, id string) error {
	t.Helper()
	suivi := occupation.Nouveau(occupation.Options{
		Reglages: occupation.Reglages{CacheOccupation: 15 * time.Second, DelaiInfo: time.Second, RetryApres: 15 * time.Second,
			RetryApresMax: time.Minute, GardeFournisseur: time.Minute, SuccesPrise: 2, FenetrePrise: 30 * time.Second},
		Sources: map[string]occupation.Source{"iptva": {Info: func(context.Context) (occupation.Info, error) {
			lectures.Add(1)
			return occupation.Info{ConnexionsMax: 1, ConnexionsActives: 1}, nil
		}}},
	})
	t.Cleanup(suivi.Fermer)
	svc, err := Nouveau(Dependances{
		Fournisseurs: map[string]domain.Fournisseur{"iptva": b.client}, Stockage: b.stockage, Baux: b.baux,
		Sondeur: b.sondeur, Decideur: remux.Decider, Lanceur: b.lanceur, Instrumentation: b.instr, Occupation: suivi,
	}, rg)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/", nil)
	return svc.Servir(httptest.NewRecorder(), req, "s1", domain.ContenuID(id))
}

// US-34 CA1/CA3 : sur un 407 persistant, la patience du film s'espace (peu de requetes au
// fournisseur), user_info est lu UNE fois des le deuxieme refus, et l'erreur rendue porte la
// classification pour que l'API reponde compte_utilise_ailleurs.
func TestCA3_Film407PersistantEspaceLesEssaisEtClasseLOccupation(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleFilm, Statut: 407, Duree: time.Minute})
	rg := ReglagesParDefaut()
	rg.PatienceMax, rg.Reessai, rg.ReessaiMax = 400*time.Millisecond, 30*time.Millisecond, 200*time.Millisecond
	var lectures atomic.Int32
	err := servirAvecOccupation(t, b, rg, &lectures, "iptva:2001")
	classe, ok := occupation.ClasseDe(err)
	if !ok || classe != occupation.Ailleurs || !errors.Is(err, domain.ErrRefuseConnexions) {
		t.Fatalf("erreur = %v (classe %q)", err, classe)
	}
	if n := b.faux.Compteurs().Refus407; n > 5 {
		t.Errorf("%d requetes au fournisseur sur 400 ms : pas de retrait progressif", n)
	}
	if lectures.Load() != 1 {
		t.Errorf("user_info lu %d fois", lectures.Load())
	}
}

// Controle du temoin : pause constante -> beaucoup plus de requetes.
func TestCA1_Film407SansRetraitMartele(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleFilm, Statut: 407, Duree: time.Minute})
	rg := ReglagesParDefaut()
	rg.PatienceMax, rg.Reessai = 400*time.Millisecond, 30*time.Millisecond
	var lectures atomic.Int32
	_ = servirAvecOccupation(t, b, rg, &lectures, "iptva:2001")
	if n := b.faux.Compteurs().Refus407; n < 8 {
		t.Errorf("temoin : %d requetes seulement", n)
	}
}
