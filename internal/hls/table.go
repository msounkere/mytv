package hls

import (
	"sync"
	"time"
)

// table retient, pour chaque segment annonce (cle « portee/nom »), l'adresse REELLE
// la plus recente chez le fournisseur.
//
// POURQUOI : le fournisseur glisse un hash different dans le chemin de chaque
// segment a chaque playlist. Le jeton que nous emettons ne depend que du nom du
// segment (donc il est stable) ; l'adresse a utiliser au moment de la lecture est
// retrouvee ici, et c'est toujours la derniere vue.
//
// Elle est BORNEE (nombre d'entrees) et PURGEE (age) : sans cela une longue
// session de zapping la ferait grossir sans fin.
//
// Les purges parcourent toute la table : O(n) par insertion QUAND elle deborde.
// Choix assume : n vaut quelques centaines (live.table_segments.nombre), un segment
// n'arrive que toutes les quelques secondes, et une structure ordonnee (liste + index)
// ajouterait du code pour un gain mesurable seulement a des milliers d'entrees.
type table struct {
	mu       sync.Mutex
	entrees  map[string]entreeTable
	capacite int
	validite time.Duration
	horloge  func() time.Time
}

type entreeTable struct {
	url string
	vue time.Time
}

func nouvelleTable(capacite int, validite time.Duration, horloge func() time.Time) *table {
	return &table{entrees: map[string]entreeTable{}, capacite: capacite, validite: validite, horloge: horloge}
}

func (t *table) noter(cle, url string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	maintenant := t.horloge()
	t.entrees[cle] = entreeTable{url: url, vue: maintenant}
	if len(t.entrees) <= t.capacite {
		return
	}
	// D'abord les entrees perimees (leur segment n'est plus annonce depuis longtemps)...
	for k, e := range t.entrees {
		if maintenant.Sub(e.vue) > t.validite {
			delete(t.entrees, k)
		}
	}
	// ... puis, si la table reste trop grande, les plus anciennes.
	for len(t.entrees) > t.capacite {
		ancienne, vue := "", maintenant.Add(time.Nanosecond)
		for k, e := range t.entrees {
			if e.vue.Before(vue) {
				ancienne, vue = k, e.vue
			}
		}
		delete(t.entrees, ancienne)
	}
}

func (t *table) chercher(cle string) (string, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	e, ok := t.entrees[cle]
	if !ok {
		return "", false
	}
	if t.horloge().Sub(e.vue) > t.validite {
		delete(t.entrees, cle)
		return "", false
	}
	return e.url, true
}

func (t *table) taille() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.entrees)
}
