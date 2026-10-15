package images

import (
	"sync"
	"time"
)

// negatif retient quelques instants qu'une image est absente ou en erreur chez le
// fournisseur : sans lui, 20 000 picons morts seraient redemandes a chaque defilement
// de la liste, a un fournisseur qui n'a pas besoin de ce trafic.
//
// BORNE en nombre d'entrees (memoire) : a saturation on purge les expirees, puis on
// sacrifie une entree au hasard (une erreur oubliee tot coute une requete de plus,
// rien d'autre).
type negatif struct {
	duree   time.Duration
	max     int
	horloge func() time.Time

	mu sync.Mutex
	m  map[string]entreeNegative
}

type entreeNegative struct {
	err    error
	expire time.Time
}

func nouveauNegatif(duree time.Duration, max int, horloge func() time.Time) *negatif {
	return &negatif{duree: duree, max: max, horloge: horloge, m: map[string]entreeNegative{}}
}

func (n *negatif) chercher(cle string) (error, bool) {
	if n.duree <= 0 {
		return nil, false
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	e, ok := n.m[cle]
	if !ok {
		return nil, false
	}
	if !n.horloge().Before(e.expire) {
		delete(n.m, cle)
		return nil, false
	}
	return e.err, true
}

func (n *negatif) noter(cle string, err error) {
	if n.duree <= 0 {
		return
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	now := n.horloge()
	if _, existe := n.m[cle]; !existe && len(n.m) >= n.max {
		for k, e := range n.m {
			if !now.Before(e.expire) {
				delete(n.m, k)
			}
		}
		for k := range n.m {
			if len(n.m) < n.max {
				break
			}
			delete(n.m, k)
		}
	}
	n.m[cle] = entreeNegative{err: err, expire: now.Add(n.duree)}
}

func (n *negatif) taille() int {
	n.mu.Lock()
	defer n.mu.Unlock()
	return len(n.m)
}
