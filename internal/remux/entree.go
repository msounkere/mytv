package remux

import (
	"context"
	"errors"
	"io"
	"sync"

	"mytv/internal/domain"
)

// entree est l'entree standard de ffmpeg : les octets du film lus chez le
// fournisseur par le SERVEUR (voir Arguments). Elle rouvre l'amont a l'octet suivant
// quand il se coupe en route (domain.SourceOctets rend alors une erreur), au plus
// `reprises` fois : un octet jamais saute ni repete, ffmpeg ne voit qu'un flux continu.
//
// Close est sur et idempotent depuis n'importe quelle goroutine : il coupe la lecture
// en cours (donc la connexion du fournisseur) et debloque un Read en attente.
type entree struct {
	ctx      context.Context
	annule   context.CancelFunc
	source   domain.SourceOctets
	reprises int

	mu      sync.Mutex
	courant io.ReadCloser
	lus     int64 // octets deja remis a ffmpeg = prochain octet a lire
	ferme   bool
	essais  int
}

func nouvelleEntree(ctx context.Context, src domain.SourceOctets, reprises int) *entree {
	c, annule := context.WithCancel(ctx)
	return &entree{ctx: c, annule: annule, source: src, reprises: reprises}
}

var errEntreeFermee = errors.New("remux : entree fermee")

func (e *entree) Read(p []byte) (int, error) {
	for {
		e.mu.Lock()
		if e.ferme {
			e.mu.Unlock()
			return 0, errEntreeFermee
		}
		corps := e.courant
		depuis := e.lus
		e.mu.Unlock()

		if corps == nil {
			c, err := e.source(e.ctx, depuis)
			if err != nil {
				if e.reessayer(err) {
					continue
				}
				return 0, err
			}
			e.mu.Lock()
			if e.ferme {
				e.mu.Unlock()
				c.Close()
				return 0, errEntreeFermee
			}
			e.courant = c
			e.mu.Unlock()
			corps = c
		}
		n, err := corps.Read(p)
		e.mu.Lock()
		e.lus += int64(n)
		e.mu.Unlock()
		if n > 0 || err == nil {
			if err == io.EOF { // fin normale avec des octets
				err = nil
				e.fermerCourant()
				e.marquerFin()
			}
			return n, err
		}
		if errors.Is(err, io.EOF) {
			e.fermerCourant()
			return 0, io.EOF
		}
		// Coupure de l'amont : rouvrir a l'octet suivant, si on a le droit.
		e.fermerCourant()
		if e.reessayer(err) {
			continue
		}
		return 0, err
	}
}

// marquerFin : apres un EOF accompagne d'octets, le Read suivant doit rendre EOF
// sans rouvrir l'amont.
func (e *entree) marquerFin() {
	e.mu.Lock()
	e.courant = eof{}
	e.mu.Unlock()
}

type eof struct{}

func (eof) Read([]byte) (int, error) { return 0, io.EOF }
func (eof) Close() error             { return nil }

func (e *entree) fermerCourant() {
	e.mu.Lock()
	c := e.courant
	e.courant = nil
	e.mu.Unlock()
	if c != nil {
		c.Close()
	}
}

// reessayer dit si une nouvelle ouverture est permise (et la compte). Jamais apres
// l'annulation ou la fermeture : un client parti n'a pas a consommer des reprises.
func (e *entree) reessayer(cause error) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.ferme || e.ctx.Err() != nil || e.essais >= e.reprises {
		return false
	}
	e.essais++
	return true
}

func (e *entree) Close() error {
	e.mu.Lock()
	if e.ferme {
		e.mu.Unlock()
		return nil
	}
	e.ferme = true
	c := e.courant
	e.courant = nil
	e.mu.Unlock()
	e.annule()
	if c != nil {
		c.Close()
	}
	return nil
}
