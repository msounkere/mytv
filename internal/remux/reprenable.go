package remux

import (
	"context"
	"errors"
	"io"
	"sync"

	"mytv/internal/domain"
)

// lectureReprenable est un corps de film ouvert a l'octet `depuis` qui, quand l'amont se
// coupe en route (domain.SourceOctets rend alors une erreur), se rouvre a l'octet suivant,
// au plus `reprises` fois : un octet n'est jamais saute ni repete.
//
// POURQUOI : la source locale que lit ffmpeg (fluxSonde) sert des plages. Avant, ffmpeg
// lisait un tube alimente par `entree`, qui assurait cette reprise ; en passant a une
// adresse locale (pour que `-ss` saute au lieu de tout relire, voir Arguments), on garde
// la meme robustesse ici. Elle expose Taille() (domain.AvecTaille) du corps ouvert, dont
// la source locale a besoin pour repondre aux plages.
type lectureReprenable struct {
	ctx      context.Context
	src      domain.SourceOctets
	reprises int
	debut    int64
	taille   int64

	mu      sync.Mutex
	courant io.ReadCloser
	lus     int64
	essais  int
	ferme   bool
}

// ouvrirReprenable ouvre MAINTENANT le premier corps : une erreur franche (404, 407,
// plage hors fichier) remonte telle quelle a l'appelant, et la taille est connue.
func ouvrirReprenable(ctx context.Context, src domain.SourceOctets, depuis int64, reprises int) (*lectureReprenable, error) {
	c, err := src(ctx, depuis)
	if err != nil {
		return nil, err
	}
	taille := int64(-1)
	if t, ok := c.(domain.AvecTaille); ok {
		taille = t.Taille()
	}
	return &lectureReprenable{ctx: ctx, src: src, reprises: reprises, debut: depuis, taille: taille, courant: c}, nil
}

// Taille : taille totale du film chez le fournisseur, -1 si inconnue.
func (l *lectureReprenable) Taille() int64 { return l.taille }

func (l *lectureReprenable) Read(p []byte) (int, error) {
	for {
		l.mu.Lock()
		if l.ferme {
			l.mu.Unlock()
			return 0, errEntreeFermee
		}
		c := l.courant
		depuis := l.debut + l.lus
		l.mu.Unlock()

		if c == nil {
			nc, err := l.src(l.ctx, depuis)
			if err != nil {
				if l.reessayer() {
					continue
				}
				return 0, err
			}
			l.mu.Lock()
			if l.ferme {
				l.mu.Unlock()
				nc.Close()
				return 0, errEntreeFermee
			}
			l.courant = nc
			l.mu.Unlock()
			c = nc
		}
		n, err := c.Read(p)
		l.mu.Lock()
		l.lus += int64(n)
		l.mu.Unlock()
		if err == nil || errors.Is(err, io.EOF) {
			return n, err
		}
		// Coupure de l'amont : on rouvre a l'octet suivant, si on a le droit.
		l.fermerCourant()
		if n > 0 {
			return n, nil // les octets lus sont remis ; la reprise se fait au Read suivant
		}
		if !l.reessayer() {
			return 0, err
		}
	}
}

func (l *lectureReprenable) fermerCourant() {
	l.mu.Lock()
	c := l.courant
	l.courant = nil
	l.mu.Unlock()
	if c != nil {
		c.Close()
	}
}

// reessayer dit si une nouvelle ouverture est permise (et la compte). Jamais apres
// l'annulation ou la fermeture : un client parti ne consomme pas de reprises.
func (l *lectureReprenable) reessayer() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.ferme || l.ctx.Err() != nil || l.essais >= l.reprises {
		return false
	}
	l.essais++
	return true
}

func (l *lectureReprenable) Close() error {
	l.mu.Lock()
	l.ferme = true
	c := l.courant
	l.courant = nil
	l.mu.Unlock()
	if c != nil {
		return c.Close()
	}
	return nil
}

// avecReprises enveloppe une source : chaque corps ouvert se reprend seul apres une coupure.
func avecReprises(src domain.SourceOctets, reprises int) domain.SourceOctets {
	return func(ctx context.Context, depuis int64) (io.ReadCloser, error) {
		return ouvrirReprenable(ctx, src, depuis, reprises)
	}
}
