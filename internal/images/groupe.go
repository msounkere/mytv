package images

import (
	"context"
	"sync"
)

// groupe fusionne les telechargements simultanes d'une meme image (« singleflight ») :
// une page qui affiche vingt fois le meme logo ne fait qu'UNE requete au fournisseur.
//
// Le telechargement n'appartient a aucun demandeur : il tourne detache de leur contexte
// (borne par son propre delai). Sinon le premier client qui ferme l'onglet ferait
// echouer tous les autres, et un defilement rapide ne remplirait jamais le cache.
type groupe struct {
	mu     sync.Mutex
	appels map[string]*appel
}

type appel struct {
	fait chan struct{}
	val  *donnees
	err  error
}

func (g *groupe) faire(ctx context.Context, cle string, f func() (*donnees, error)) (*donnees, error) {
	g.mu.Lock()
	if g.appels == nil {
		g.appels = map[string]*appel{}
	}
	a, existe := g.appels[cle]
	if !existe {
		a = &appel{fait: make(chan struct{})}
		g.appels[cle] = a
		go func() {
			a.val, a.err = f()
			g.mu.Lock()
			delete(g.appels, cle)
			g.mu.Unlock()
			close(a.fait)
		}()
	}
	g.mu.Unlock()
	select {
	case <-a.fait:
		return a.val, a.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
