package baux

import (
	"context"
	"time"

	"mytv/internal/config"
)

// Patience est LA patience sur 407 du projet : live (segments, playlists) et vod
// (films, reprises, sonde) l'utilisent, pour qu'il n'y ait qu'un seul reglage
// (bail.patience_max / bail.reessai) et une seule regle.
//
// POURQUOI : apres un zapping ou un seek, le fournisseur garde la place de
// l'ancienne lecture ~100 s et repond 407 a toute autre demande. Renoncer au premier
// 407 laisse un lecteur fige ; il faut retenter jusqu'a ce que la place soit rendue.
//
// Le delai maximal se compte depuis `debut` (l'arrivee de la requete du client),
// pas depuis le premier 407 : la patience n'est donc PAS cumulee avec l'attente d'une
// place par le gestionnaire de baux (Acquerir), qui a deja pu consommer une partie
// du budget.
type Patience struct {
	Max        time.Duration
	Reessai    time.Duration
	Maintenant func() time.Time // nil = time.Now (tests : horloge injectee)
}

// PatienceDepuisConfig reprend bail.patience_max et bail.reessai.
func PatienceDepuisConfig(b config.Bail, maintenant func() time.Time) Patience {
	return Patience{Max: b.PatienceMax.Std(), Reessai: b.Reessai.Std(), Maintenant: maintenant}
}

func (p Patience) maintenant() time.Time {
	if p.Maintenant == nil {
		return time.Now()
	}
	return p.Maintenant()
}

// Attendre se place apres un 407 (`refus`). Rendu nil : la pause a eu lieu, l'appelant
// doit retenter, SANS consommer un essai de reprise. Rendu non nil : renoncer avec
// cette erreur, `refus` lui-meme quand la patience est epuisee (l'API repond alors
// 503 + Retry-After), ou l'erreur du contexte s'il a ete annule pendant la pause.
// `surAttente` (facultatif) est appele avant chaque pause : compteurs, renouvellement
// du bail pour ne pas le perdre en attendant.
func (p Patience) Attendre(ctx context.Context, debut time.Time, refus error, surAttente func()) error {
	if p.maintenant().Add(p.Reessai).After(debut.Add(p.Max)) {
		return refus
	}
	if surAttente != nil {
		surAttente()
	}
	t := time.NewTimer(p.Reessai)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
