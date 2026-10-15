package metadonnees

import (
	"context"
	"sync"
)

// groupe fusionne les appels simultanes de meme cle en UN seul (singleflight). Le
// travail n'appartient a aucun demandeur : il est lance detache de leurs contextes
// et ne s'arrete que par son propre delai. Un demandeur qui s'impatiente part sans
// annuler les autres, et le resultat arrive quand meme dans le cache.
type groupe struct {
	mu   sync.Mutex
	vols map[string]*vol
}

type vol struct {
	fait chan struct{}
	val  any
	err  error
}

// faire attend le resultat de la cle ; `travail` n'est lance que par le premier.
func (g *groupe) faire(ctx context.Context, cle string, travail func() (any, error)) (any, error) {
	g.mu.Lock()
	if g.vols == nil {
		g.vols = map[string]*vol{}
	}
	v, existe := g.vols[cle]
	if !existe {
		v = &vol{fait: make(chan struct{})}
		g.vols[cle] = v
		go func() {
			v.val, v.err = travail()
			g.mu.Lock()
			delete(g.vols, cle)
			g.mu.Unlock()
			close(v.fait)
		}()
	}
	g.mu.Unlock()
	select {
	case <-v.fait:
		return v.val, v.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
