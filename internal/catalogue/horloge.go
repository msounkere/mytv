package catalogue

import "time"

// Horloge isole le code du temps reel : les tests la pilotent a la main, sans
// jamais attendre 05:00 ni dormir.
type Horloge interface {
	Maintenant() time.Time
	// Minuteur declenche son canal une fois, apres d.
	Minuteur(d time.Duration) Minuteur
}

type Minuteur interface {
	C() <-chan time.Time
	Arreter()
}

// HorlogeReelle est l'horloge de production.
type HorlogeReelle struct{}

func (HorlogeReelle) Maintenant() time.Time { return time.Now() }

func (HorlogeReelle) Minuteur(d time.Duration) Minuteur { return minuteurReel{time.NewTimer(d)} }

type minuteurReel struct{ t *time.Timer }

func (m minuteurReel) C() <-chan time.Time { return m.t.C }
func (m minuteurReel) Arreter()            { m.t.Stop() }
