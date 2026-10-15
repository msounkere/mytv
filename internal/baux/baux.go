// Package baux reserve la connexion limitee d'un fournisseur.
//
// POURQUOI : le fournisseur n'accepte qu'une connexion par compte et refuse (407)
// toute autre ressource tant que la precedente occupe sa place (~100 s apres le
// depart du client). Sans arbitre, deux lecteurs — ou un zapping — se marchent
// dessus. Une SESSION (un client) detient donc au plus un bail par fournisseur et,
// en changeant de chaine, REUTILISE ce bail : le fournisseur voit le meme
// utilisateur, pas un second.
package baux

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"mytv/internal/config"
	"mytv/internal/domain"
)

// ErrFournisseurInconnu : aucun reglage de places pour ce fournisseur. C'est une
// erreur de programmation ou de configuration, pas une saturation.
var ErrFournisseurInconnu = errors.New("baux : fournisseur inconnu")

// Gestionnaire implemente domain.Baux en memoire.
type Gestionnaire struct {
	reglages config.Bail
	horloge  Horloge
	inst     domain.Instrumentation
	parFourn map[string]*fournisseur
}

var (
	_ domain.Baux        = (*Gestionnaire)(nil)
	_ domain.Terminateur = (*Gestionnaire)(nil)
)

type fournisseur struct {
	id     string
	mu     sync.Mutex
	max    int
	baux   map[string]*bail // par session
	change chan struct{}    // ferme (puis remplace) a chaque place rendue
}

// Nouveau cree le gestionnaire. `max` donne connexions_max de chaque fournisseur
// (chacun a son propre compteur : saturer l'un ne bloque pas l'autre). Une horloge
// nil donne l'horloge reelle.
func Nouveau(reglages config.Bail, max map[string]int, h Horloge) (*Gestionnaire, error) {
	if h == nil {
		h = HorlogeReelle{}
	}
	g := &Gestionnaire{reglages: reglages, horloge: h, inst: domain.SansInstrumentation{}, parFourn: map[string]*fournisseur{}}
	for id, n := range max {
		if n < 1 {
			return nil, fmt.Errorf("baux : connexions_max du fournisseur %q doit etre >= 1", id)
		}
		g.parFourn[id] = &fournisseur{id: id, max: n, baux: map[string]*bail{}, change: make(chan struct{})}
	}
	return g, nil
}

type bail struct {
	g         *Gestionnaire
	f         *fournisseur
	session   string
	ressource string
	expire    time.Time
	termine   bool // libere ou expire : le bail ne compte plus
}

// purger retire les baux expires. A appeler avec f.mu tenu.
func (g *Gestionnaire) purger(f *fournisseur, now time.Time) {
	for s, b := range f.baux {
		if !now.Before(b.expire) {
			b.termine = true
			delete(f.baux, s)
			g.inst.Compter("baux_expires_total", 1, "fournisseur", g.etiquette(f.id))
		}
	}
}

// reveiller previent les attentes qu'une place s'est peut-etre liberee.
// A appeler avec f.mu tenu.
func (f *fournisseur) reveiller() {
	close(f.change)
	f.change = make(chan struct{})
}

// essayer tente de prendre ou reutiliser une place. Rend aussi le canal de
// changement observe sous le meme verrou : une liberation survenant juste apres
// reveillera donc l'attente (pas de reveil perdu).
func (g *Gestionnaire) essayer(f *fournisseur, session, ressource string) (*bail, <-chan struct{}) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := g.horloge.Maintenant()
	g.purger(f, now)
	if b, ok := f.baux[session]; ok {
		// Zapping : meme session, autre ressource, meme place.
		b.ressource = ressource
		b.expire = now.Add(g.reglages.TTL.Std())
		return b, nil
	}
	if len(f.baux) < f.max {
		b := &bail{g: g, f: f, session: session, ressource: ressource, expire: now.Add(g.reglages.TTL.Std())}
		f.baux[session] = b
		// Taux d'occupation vu par les nouvelles places (histogramme, faute de jauge).
		g.inst.Observer("baux_occupes", float64(len(f.baux)), "fournisseur", g.etiquette(f.id))
		return b, nil
	}
	return nil, f.change
}

// AvecInstrumentation branche la publication de mesures (a appeler avant usage).
// Etiquettes : le seul « fournisseur » (identifiant de configuration, borne) et un
// resultat parmi quelques valeurs fixes ; jamais de session ni de ressource.
func (g *Gestionnaire) AvecInstrumentation(i domain.Instrumentation) *Gestionnaire {
	if i == nil {
		i = domain.SansInstrumentation{}
	}
	g.inst = i
	return g
}

// Acquerir rend le bail de la session, en attendant une place si besoin : on
// reessaie toutes les `reessai`, ou des qu'une place est rendue, jusqu'a
// `patience_max`. L'attente s'arrete des que ctx est annule.
func (g *Gestionnaire) Acquerir(ctx context.Context, fournisseurID, session, ressource string) (domain.Bail, error) {
	debut := g.horloge.Maintenant()
	b, err := g.acquerir(ctx, fournisseurID, session, ressource)
	resultat := "ok"
	switch {
	case errors.Is(err, domain.ErrPlaceIndisponible):
		resultat = "refuse"
	case err != nil:
		resultat = "annule"
	}
	g.inst.Compter("baux_acquisitions_total", 1, "fournisseur", g.etiquette(fournisseurID), "resultat", resultat)
	g.inst.Observer("baux_attente_secondes", g.horloge.Maintenant().Sub(debut).Seconds(), "fournisseur", g.etiquette(fournisseurID))
	return b, err
}

// etiquette : un fournisseur inconnu ne cree pas une nouvelle serie par valeur.
func (g *Gestionnaire) etiquette(id string) string {
	if _, ok := g.parFourn[id]; ok {
		return id
	}
	return "inconnu"
}

func (g *Gestionnaire) acquerir(ctx context.Context, fournisseurID, session, ressource string) (domain.Bail, error) {
	f, ok := g.parFourn[fournisseurID]
	if !ok {
		return nil, fmt.Errorf("%w : %q", ErrFournisseurInconnu, fournisseurID)
	}
	limite := g.horloge.Maintenant().Add(g.reglages.PatienceMax.Std())
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		b, change := g.essayer(f, session, ressource)
		if b != nil {
			return b, nil
		}
		reste := limite.Sub(g.horloge.Maintenant())
		if reste <= 0 {
			return nil, domain.ErrPlaceIndisponible
		}
		pause := min(g.reglages.Reessai.Std(), reste)
		if pause <= 0 {
			pause = reste
		}
		select {
		case <-g.horloge.Apres(pause):
		case <-change:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// Renouveler prolonge le bail d'un TTL. Un bail deja expire ou libere ne
// ressuscite pas : sa place a pu etre donnee a quelqu'un d'autre.
func (b *bail) Renouveler() {
	b.f.mu.Lock()
	defer b.f.mu.Unlock()
	now := b.g.horloge.Maintenant()
	if b.termine || !now.Before(b.expire) {
		return
	}
	b.expire = now.Add(b.g.reglages.TTL.Std())
}

// Liberer rend la place immediatement. Idempotent, et sans effet sur la place
// qu'une autre session (ou la meme, plus tard) aurait prise depuis.
func (b *bail) Liberer() {
	b.f.mu.Lock()
	defer b.f.mu.Unlock()
	if b.termine {
		return
	}
	b.termine = true
	if b.f.baux[b.session] == b {
		delete(b.f.baux, b.session)
	}
	b.f.reveiller()
}

// Occupes donne le nombre de places occupees (hors baux expires) : utile aux
// tests et a l'etat de diagnostic.
func (g *Gestionnaire) Occupes(fournisseurID string) int {
	f, ok := g.parFourn[fournisseurID]
	if !ok {
		return 0
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	g.purger(f, g.horloge.Maintenant())
	return len(f.baux)
}

// Terminer libere tous les baux d'une session, chez tous les fournisseurs (la
// session est deconnectee : sa place doit servir tout de suite a quelqu'un
// d'autre). Idempotent, sans erreur, sans effet sur les autres sessions.
func (g *Gestionnaire) Terminer(session string) {
	for _, f := range g.parFourn {
		f.mu.Lock()
		if b, ok := f.baux[session]; ok {
			b.termine = true
			delete(f.baux, session)
			f.reveiller()
			g.inst.Compter("baux_terminaisons_total", 1, "fournisseur", g.etiquette(f.id))
		}
		f.mu.Unlock()
	}
}
