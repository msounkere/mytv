package baux

import (
	"sort"
	"sync"
	"time"
)

// Horloge est la seule source de temps du paquet : les tests la remplacent pour
// jouer 90 secondes d'expiration en quelques microsecondes, sans dormir.
type Horloge interface {
	Maintenant() time.Time
	// Apres envoie l'heure sur le canal une fois `d` ecoule.
	Apres(d time.Duration) <-chan time.Time
}

// HorlogeReelle est l'horloge de production.
type HorlogeReelle struct{}

func (HorlogeReelle) Maintenant() time.Time { return time.Now() }

func (HorlogeReelle) Apres(d time.Duration) <-chan time.Time { return time.After(d) }

// HorlogeManuelle n'avance que si on le lui demande. Exportee pour que d'autres
// paquets (le relais du direct) puissent tester leurs scenarios de patience.
type HorlogeManuelle struct {
	mu        sync.Mutex
	now       time.Time
	minuteurs []*minuteur
}

type minuteur struct {
	quand time.Time
	c     chan time.Time
}

func NouvelleHorlogeManuelle(depart time.Time) *HorlogeManuelle {
	return &HorlogeManuelle{now: depart}
}

func (h *HorlogeManuelle) Maintenant() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

func (h *HorlogeManuelle) Apres(d time.Duration) <-chan time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	m := &minuteur{quand: h.now.Add(d), c: make(chan time.Time, 1)}
	if d <= 0 {
		m.c <- h.now
		return m.c
	}
	h.minuteurs = append(h.minuteurs, m)
	return m.c
}

// Avancer fait progresser l'heure et declenche, dans l'ordre, les minuteurs echus.
func (h *HorlogeManuelle) Avancer(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = h.now.Add(d)
	sort.Slice(h.minuteurs, func(i, j int) bool { return h.minuteurs[i].quand.Before(h.minuteurs[j].quand) })
	restants := h.minuteurs[:0]
	for _, m := range h.minuteurs {
		if !m.quand.After(h.now) {
			m.c <- h.now
		} else {
			restants = append(restants, m)
		}
	}
	h.minuteurs = restants
}

// Minuteurs donne le nombre de minuteurs en attente : un test s'en sert pour
// savoir qu'une goroutine dort bien avant de faire avancer l'heure.
func (h *HorlogeManuelle) Minuteurs() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.minuteurs)
}
