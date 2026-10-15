package epg

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mytv/internal/domain"
)

// fauxFournisseur sert un guide fixe. L'URL contient un mot de passe : aucun test
// ne doit le retrouver dans une erreur ou un journal.
type fauxFournisseur struct {
	id      string
	corps   func() []byte // contenu du guide
	erreur  error         // renvoyee par Requete si non nil
	bloque  chan struct{} // si non nil, la lecture du corps attend sa fermeture (ou ctx)
	entre   chan struct{} // signal a l'entree de Requete
	appels  atomic.Int32
	annules atomic.Int32
}

const motDePasseSecret = "s3cr3t-mdp"

func (f *fauxFournisseur) ID() string { return f.id }
func (f *fauxFournisseur) URLEPG() string {
	return "http://fournisseur.test/xmltv.php?username=u&password=" + motDePasseSecret
}
func (f *fauxFournisseur) URLLive(string) string { return "" }
func (f *fauxFournisseur) Catalogue(context.Context) (*domain.Instantane, error) {
	return nil, nil
}

func (f *fauxFournisseur) Requete(ctx context.Context, url string, _ http.Header) (*http.Response, error) {
	f.appels.Add(1)
	if f.entre != nil {
		f.entre <- struct{}{}
	}
	if f.erreur != nil {
		return nil, f.erreur
	}
	var corps io.ReadCloser = io.NopCloser(bytes.NewReader(f.corps()))
	if f.bloque != nil {
		corps = &corpsBloque{ctx: ctx, debloque: f.bloque, f: f}
	}
	return &http.Response{StatusCode: 200, Body: corps}, nil
}

type corpsBloque struct {
	ctx      context.Context
	debloque chan struct{}
	f        *fauxFournisseur
}

func (c *corpsBloque) Read([]byte) (int, error) {
	select {
	case <-c.debloque:
		return 0, io.EOF
	case <-c.ctx.Done():
		c.f.annules.Add(1)
		return 0, c.ctx.Err()
	}
}
func (c *corpsBloque) Close() error { return nil }

// listeChaines est un ListeurChaines fixe qui respecte le filtre FournisseurID.
type listeChaines []domain.Chaine

func (l listeChaines) Chaines(_ context.Context, f domain.FiltreChaines) ([]domain.Chaine, error) {
	var out []domain.Chaine
	for _, c := range l {
		if f.FournisseurID == "" || c.FournisseurID == f.FournisseurID {
			out = append(out, c)
		}
	}
	if f.Decalage >= len(out) {
		return nil, nil
	}
	out = out[f.Decalage:]
	if f.Limite > 0 && len(out) > f.Limite {
		out = out[:f.Limite]
	}
	return out, nil
}

// horlogeFausse : le temps n'avance que quand le test le decide.
type horlogeFausse struct {
	mu        sync.Mutex
	now       time.Time
	minuteurs []*minuteurFaux
}

type minuteurFaux struct {
	h      *horlogeFausse
	quand  time.Time
	c      chan time.Time
	arrete bool
	tire   bool
}

func (m *minuteurFaux) C() <-chan time.Time { return m.c }
func (m *minuteurFaux) Arreter() {
	m.h.mu.Lock()
	m.arrete = true
	m.h.mu.Unlock()
}

func (h *horlogeFausse) Maintenant() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

func (h *horlogeFausse) Minuteur(d time.Duration) Minuteur {
	h.mu.Lock()
	defer h.mu.Unlock()
	m := &minuteurFaux{h: h, quand: h.now.Add(d), c: make(chan time.Time, 1)}
	h.minuteurs = append(h.minuteurs, m)
	return m
}

func (h *horlogeFausse) Avancer(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = h.now.Add(d)
	for _, m := range h.minuteurs {
		if !m.arrete && !m.tire && !m.quand.After(h.now) {
			m.tire = true
			m.c <- h.now
		}
	}
}

// enAttente : nombre de minuteurs armes (pas tires, pas arretes).
func (h *horlogeFausse) enAttente() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	n := 0
	for _, m := range h.minuteurs {
		if !m.arrete && !m.tire {
			n++
		}
	}
	return n
}

func attendre(t *testing.T, quoi string, cond func() bool) {
	t.Helper()
	fin := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(fin) {
			t.Fatalf("delai depasse : %s", quoi)
		}
		time.Sleep(time.Millisecond)
	}
}

// chainesVariables : un ListeurChaines vide au depart, rempli en cours de test
// (le catalogue qui arrive apres le demarrage).
type chainesVariables struct {
	mu sync.Mutex
	l  listeChaines
}

func (c *chainesVariables) definir(l listeChaines) { c.mu.Lock(); c.l = l; c.mu.Unlock() }

func (c *chainesVariables) Chaines(ctx context.Context, f domain.FiltreChaines) ([]domain.Chaine, error) {
	c.mu.Lock()
	l := c.l
	c.mu.Unlock()
	return l.Chaines(ctx, f)
}
