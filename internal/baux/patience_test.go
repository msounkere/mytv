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
	if err := p.Attendre(context.Background(), debut, errRefus, 0, func() { appels++ }); err != nil || appels != 1 {
		t.Fatalf("premiere attente : %v, %d appels", err, appels)
	}
	maintenant = debut.Add(95 * time.Millisecond) // 95 + 10 > 100 : plus de place pour une pause
	if err := p.Attendre(context.Background(), debut, errRefus, 0, func() { appels++ }); !errors.Is(err, errRefus) || appels != 1 {
		t.Fatalf("budget epuise : %v, %d appels", err, appels)
	}
}

// CA1 : le budget part de l'arrivee de la requete (debut), donc l'attente deja passee a
// obtenir un bail en consomme une partie : les deux patiences ne s'additionnent pas.
func TestCA1_LaPatienceNeSAjouteJamaisACelleDesBaux(t *testing.T) {
	maintenant := time.Unix(1000, 0)
	p := Patience{Max: 30 * time.Second, Reessai: time.Second, Maintenant: func() time.Time { return maintenant }}
	debut := maintenant.Add(-30 * time.Second) // 30 s deja passees avant le premier 407
	if err := p.Attendre(context.Background(), debut, errRefus, 0, nil); !errors.Is(err, errRefus) {
		t.Fatalf("la patience a recommence a zero : %v", err)
	}
}

func TestPatienceSArreteQuandLeContexteEstAnnule(t *testing.T) {
	p := Patience{Max: 10 * time.Minute, Reessai: time.Minute}
	ctx, annule := context.WithCancel(context.Background())
	go func() { time.Sleep(10 * time.Millisecond); annule() }()
	if err := p.Attendre(ctx, time.Now(), errRefus, 0, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("erreur = %v", err)
	}
}

// US-34 CA1 : la pause double a chaque refus consecutif et s'arrete au plafond.
func TestCA1_LaPauseDoubleAChaqueRefusJusquAuPlafond(t *testing.T) {
	p := Patience{Max: time.Hour, Reessai: 3 * time.Second, ReessaiMax: 30 * time.Second}
	attendu := []time.Duration{3, 6, 12, 24, 30, 30}
	for n, a := range attendu {
		if d := p.Delai(n); d != a*time.Second {
			t.Errorf("pause %d = %v, attendu %v", n, d, a*time.Second)
		}
	}
	// Sans plafond configure, le comportement d'avant (pause constante) est conserve.
	q := Patience{Max: time.Hour, Reessai: 3 * time.Second}
	if q.Delai(5) != 3*time.Second {
		t.Errorf("sans reessai_max la pause doit rester constante : %v", q.Delai(5))
	}
}

// US-34 CA1 : la gigue reste dans +/- 20 % et ne depasse jamais le plafond.
func TestCA1_LaGigueResteDansSesBornes(t *testing.T) {
	for _, r := range []float64{0, 0.25, 0.5, 0.99999} {
		p := Patience{Max: time.Hour, Reessai: 10 * time.Second, ReessaiMax: 30 * time.Second, Gigue: 0.2, Aleatoire: func() float64 { return r }}
		for n := 0; n < 6; n++ {
			base := min(10*time.Second<<n, 30*time.Second)
			d := p.Delai(n)
			if d < base*8/10-time.Millisecond || d > 30*time.Second {
				t.Errorf("r=%v n=%d : %v hors bornes (base %v)", r, n, d, base)
			}
		}
	}
}

// US-34 CA1 : sur un budget de 40 s, un fournisseur qui refuse toujours recoit au plus 5
// requetes (13 avec la pause fixe de 3 s), gigue maximale comprise, et le budget n'est pas depasse.
func TestCA1_Sur40sAuPlus5RequetesAuFournisseur(t *testing.T) {
	for _, r := range []float64{0, 0.5, 0.99999} {
		h := NouvelleHorlogeManuelle(time.Unix(1000, 0))
		p := Patience{Max: 40 * time.Second, Reessai: 3 * time.Second, ReessaiMax: 30 * time.Second, Gigue: 0.2,
			Maintenant: h.Maintenant, Apres: h.Apres, Aleatoire: func() float64 { return r }}
		debut := h.Maintenant()
		requetes := 0
		fini := make(chan error, 1)
		go func() {
			for n := 0; ; n++ {
				requetes++ // une requete au fournisseur, refusee
				if err := p.Attendre(context.Background(), debut, errRefus, n, nil); err != nil {
					fini <- err
					return
				}
			}
		}()
		for done := false; !done; {
			select {
			case err := <-fini:
				if !errors.Is(err, errRefus) {
					t.Fatalf("fin = %v", err)
				}
				done = true
			default:
				if h.Minuteurs() > 0 {
					h.Avancer(500 * time.Millisecond)
				} else {
					time.Sleep(time.Millisecond)
				}
			}
		}
		if requetes > 5 || requetes < 3 {
			t.Errorf("r=%v : %d requetes sur 40 s, attendu 3 a 5", r, requetes)
		}
		if h.Maintenant().Sub(debut) > 40*time.Second+500*time.Millisecond {
			t.Errorf("r=%v : budget depasse (%v)", r, h.Maintenant().Sub(debut))
		}
	}
}
