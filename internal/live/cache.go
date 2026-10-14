package live

import "sync"

// cacheSegments garde les derniers segments servis, borne en nombre ET en octets.
//
// POURQUOI : quand le lecteur redemande un segment (nouvel essai apres une erreur
// reseau cote client), le relire chez le fournisseur coute une connexion et du
// temps. Servi d'ici, il arrive tout de suite.
type cacheSegments struct {
	mu       sync.Mutex
	max      int
	maxOctet int
	octets   int
	ordre    []string // du plus ancien au plus recent
	valeurs  map[string]entree
}

type entree struct {
	corps []byte
	typ   string
}

func nouveauCache(max, maxOctet int) *cacheSegments {
	return &cacheSegments{max: max, maxOctet: maxOctet, valeurs: map[string]entree{}}
}

func (c *cacheSegments) lire(cle string) ([]byte, string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.valeurs[cle]
	return e.corps, e.typ, ok
}

func (c *cacheSegments) ecrire(cle string, corps []byte, typ string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.valeurs[cle]; ok {
		return
	}
	// Un segment plus gros que tout le cache ne ferait que le vider pour rien.
	if len(corps) > c.maxOctet {
		return
	}
	c.valeurs[cle] = entree{corps, typ}
	c.ordre = append(c.ordre, cle)
	c.octets += len(corps)
	for len(c.ordre) > c.max || c.octets > c.maxOctet {
		ancien := c.ordre[0]
		c.ordre = c.ordre[1:]
		c.octets -= len(c.valeurs[ancien].corps)
		delete(c.valeurs, ancien)
	}
}

func (c *cacheSegments) taille() (nombre, octets int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.valeurs), c.octets
}
