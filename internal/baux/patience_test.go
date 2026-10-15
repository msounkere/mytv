package baux

import (
	"context"
	"errors"
	"testing"
	"time"
)

var errRefus = errors.New("refus")

// US-24 CA1 : tant que le budget n'est pas epuise, Attendre fait la pause et rend nil
// (retenter) ; une fois epuise, il rend le refus d'origine.
func TestCA1_PatienceRetenteJusquAuBudgetPuisRendLeRefus(t *testing.T) {
	maintenant := time.Unix(1000, 0)
	p := Patience{Max: 100 * time.Millisecond, Reessai: 10 * time.Millisecond, Maintenant: func() time.Time { return maintenant }}
	debut := maintenant
	appels := 0
	if err := p.Attendre(context.Background(), debut, errRefus, func() { appels++ }); err != nil || appels != 1 {
		t.Fatalf("premiere attente : %v, %d appels", err, appels)
	}
	maintenant = debut.Add(95 * time.Millisecond) // 95 + 10 > 100 : plus de place pour une pause
	if err := p.Attendre(context.Background(), debut, errRefus, func() { appels++ }); !errors.Is(err, errRefus) || appels != 1 {
		t.Fatalf("budget epuise : %v, %d appels", err, appels)
	}
}

// CA1 : le budget part de l'arrivee de la requete (debut), donc l'attente deja passee a
// obtenir un bail en consomme une partie : les deux patiences ne s'additionnent pas.
func TestCA1_LaPatienceNeSAjouteJamaisACelleDesBaux(t *testing.T) {
	maintenant := time.Unix(1000, 0)
	p := Patience{Max: 30 * time.Second, Reessai: time.Second, Maintenant: func() time.Time { return maintenant }}
	debut := maintenant.Add(-30 * time.Second) // 30 s deja passees avant le premier 407
	if err := p.Attendre(context.Background(), debut, errRefus, nil); !errors.Is(err, errRefus) {
		t.Fatalf("la patience a recommence a zero : %v", err)
	}
}

func TestPatienceSArreteQuandLeContexteEstAnnule(t *testing.T) {
	p := Patience{Max: 10 * time.Minute, Reessai: time.Minute}
	ctx, annule := context.WithCancel(context.Background())
	go func() { time.Sleep(10 * time.Millisecond); annule() }()
	if err := p.Attendre(ctx, time.Now(), errRefus, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("erreur = %v", err)
	}
}
