package catalogue

import (
	"bytes"
	"context"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mytv/internal/domain"
)

// fauxFournisseur est une doublure minimale de domain.Fournisseur (en attendant,
// puis en complement, de internal/testutil/fauxiptv).
type fauxFournisseur struct {
	id      string
	appels  atomic.Int32
	erreur  error         // renvoyee par Catalogue si non nil
	bloque  chan struct{} // si non nil, Catalogue attend sa fermeture (ou ctx)
	entre   chan struct{} // si non nil, recoit un signal a l'entree de Catalogue
	annules atomic.Int32  // appels interrompus par l'annulation du contexte
	nChaine int
}

func (f *fauxFournisseur) ID() string { return f.id }

func (f *fauxFournisseur) Catalogue(ctx context.Context) (*domain.Instantane, error) {
	f.appels.Add(1)
	if f.entre != nil {
		f.entre <- struct{}{}
	}
	if f.bloque != nil {
		select {
		case <-f.bloque:
		case <-ctx.Done():
			f.annules.Add(1)
			return nil, ctx.Err()
		}
	}
	if f.erreur != nil {
		return nil, f.erreur
	}
	inst := &domain.Instantane{FournisseurID: f.id, Pris: time.Unix(1, 0)}
	n := max(f.nChaine, 1)
	for i := 0; i < n; i++ {
		inst.Chaines = append(inst.Chaines, domain.Chaine{
			ID: domain.ContenuID(f.id + ":" + string(rune('a'+i))), FournisseurID: f.id, Nom: "Chaine", Ordre: i,
		})
	}
	return inst, nil
}

// URLLive porte un « mot de passe » : aucun journal ne doit le contenir.
func (f *fauxFournisseur) URLLive(numero string) string {
	return "http://h/live/utilisateur/MOTDEPASSE-SECRET/" + numero + ".m3u8"
}

func (f *fauxFournisseur) Requete(context.Context, string, http.Header) (*http.Response, error) {
	panic("non utilise par la synchronisation")
}

// stockageMemoire est une doublure de domain.Stockage.
type stockageMemoire struct {
	mu        sync.Mutex
	synchros  map[string]time.Time
	catalogue map[string]*domain.Instantane
	ecritures int
	erreur    error
}

func nouveauStockage() *stockageMemoire {
	return &stockageMemoire{synchros: map[string]time.Time{}, catalogue: map[string]*domain.Instantane{}}
}

func (s *stockageMemoire) EcrireCatalogue(_ context.Context, i *domain.Instantane) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.erreur != nil {
		return s.erreur
	}
	s.ecritures++
	s.synchros[i.FournisseurID] = i.Pris
	s.catalogue[i.FournisseurID] = i
	return nil
}

func (s *stockageMemoire) DerniereSynchro(_ context.Context, id string) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.synchros[id], nil
}

func (s *stockageMemoire) Chaines(context.Context, domain.FiltreChaines) ([]domain.Chaine, error) {
	return nil, nil
}
func (s *stockageMemoire) Categories(context.Context, domain.Type) ([]domain.Categorie, error) {
	return nil, nil
}
func (s *stockageMemoire) Chaine(context.Context, domain.ContenuID) (*domain.Chaine, error) {
	return nil, nil
}
func (s *stockageMemoire) Fermer() error { return nil }

func (s *stockageMemoire) nbEcritures() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ecritures
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
	delai  time.Duration
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
	m := &minuteurFaux{h: h, quand: h.now.Add(d), delai: d, c: make(chan time.Time, 1)}
	h.minuteurs = append(h.minuteurs, m)
	return m
}

// Avancer deplace l'horloge et declenche les minuteurs echus.
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

// minuteurEnAttente attend (au plus 5 s reelles) qu'un minuteur non tire existe et le renvoie.
func (h *horlogeFausse) minuteurEnAttente(t *testing.T) *minuteurFaux {
	t.Helper()
	var m *minuteurFaux
	attendre(t, "un minuteur en attente", func() bool {
		h.mu.Lock()
		defer h.mu.Unlock()
		m = nil
		for _, x := range h.minuteurs {
			if !x.arrete && !x.tire {
				m = x
			}
		}
		return m != nil
	})
	return m
}

func attendre(t *testing.T, quoi string, cond func() bool) {
	t.Helper()
	limite := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(limite) {
			t.Fatalf("delai depasse en attendant : %s", quoi)
		}
		time.Sleep(time.Millisecond)
	}
}

// tampon est un writer sur lequel on peut lire pendant que le journal ecrit.
type tampon struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (t *tampon) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.b.Write(p)
}

func (t *tampon) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.b.String()
}
