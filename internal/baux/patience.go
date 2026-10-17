package baux

import (
	"context"
	"math/rand/v2"
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
// RETRAIT PROGRESSIF (US-34) : la pause double a chaque 407 consecutif d'une meme demande
// (Reessai, 2x, 4x... jusqu'a ReessaiMax) avec une gigue. POURQUOI : quand un autre appareil
// occupe le compte, reessayer toutes les 3 s pendant 40 s puis recommencer ne fait que
// marteler le fournisseur (190 refus mesures en 30 min) sans rendre la place plus vite ;
// espacer les essais coute la meme patience totale pour bien moins de requetes.
//
// Le delai maximal se compte depuis `debut` (l'arrivee de la requete du client),
// pas depuis le premier 407 : la patience n'est donc PAS cumulee avec l'attente d'une
// place par le gestionnaire de baux (Acquerir), qui a deja pu consommer une partie
// du budget.
type Patience struct {
	Max     time.Duration
	Reessai time.Duration
	// ReessaiMax plafonne la pause progressive ; 0 (ou < Reessai) = pause constante Reessai.
	ReessaiMax time.Duration
	// Gigue : part aleatoire +/- de chaque pause (0,2 = 20 %). 0 = aucune.
	Gigue      float64
	Maintenant func() time.Time // nil = time.Now (tests : horloge injectee)
	// Apres remplace le minuteur reel (tests : HorlogeManuelle.Apres, pour jouer 40 s sans dormir).
	Apres func(d time.Duration) <-chan time.Time
	// Aleatoire rend un nombre dans [0,1) ; nil = math/rand. Tests : valeur fixe.
	Aleatoire func() float64
}

// PatienceDepuisConfig reprend bail.patience_max, reessai, reessai_max et gigue.
func PatienceDepuisConfig(b config.Bail, maintenant func() time.Time) Patience {
	return Patience{Max: b.PatienceMax.Std(), Reessai: b.Reessai.Std(), ReessaiMax: b.ReessaiMax.Std(), Gigue: b.Gigue, Maintenant: maintenant}
}

func (p Patience) maintenant() time.Time {
	if p.Maintenant == nil {
		return time.Now()
	}
	return p.Maintenant()
}

// Delai donne la pause a faire apres le (n+1)-ieme refus d'une meme demande (n = refus
// deja pauses : 0 pour le premier).
func (p Patience) Delai(n int) time.Duration {
	plafond := max(p.ReessaiMax, p.Reessai)
	d := p.Reessai
	for i := 0; i < n && d < plafond; i++ {
		d *= 2
	}
	d = min(d, plafond)
	if p.Gigue > 0 {
		r := rand.Float64()
		if p.Aleatoire != nil {
			r = p.Aleatoire()
		}
		d = time.Duration(float64(d) * (1 + p.Gigue*(2*r-1)))
	}
	return min(d, plafond)
}

// Attendre se place apres un 407 (`refus`), `n` etant le nombre de pauses deja faites pour
// cette demande (0 au premier refus). Rendu nil : la pause a eu lieu, l'appelant
// doit retenter, SANS consommer un essai de reprise. Rendu non nil : renoncer avec
// cette erreur, `refus` lui-meme quand la patience est epuisee (l'API repond alors
// 503 + Retry-After), ou l'erreur du contexte s'il a ete annule pendant la pause.
// `surAttente` (facultatif) est appele avant chaque pause : compteurs, renouvellement
// du bail pour ne pas le perdre en attendant.
func (p Patience) Attendre(ctx context.Context, debut time.Time, refus error, n int, surAttente func()) error {
	d := p.Delai(n)
	if p.maintenant().Add(d).After(debut.Add(p.Max)) {
		return refus
	}
	if surAttente != nil {
		surAttente()
	}
	var c <-chan time.Time
	if p.Apres != nil {
		c = p.Apres(d)
	} else {
		t := time.NewTimer(d)
		defer t.Stop()
		c = t.C
	}
	select {
	case <-c:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
