package xtream

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"mytv/internal/domain"
	"mytv/internal/hotes"
)

// US-17 CA2 : ce que le fournisseur peut nous faire joindre. Le « fournisseur » est joint
// par le nom « localhost » (donc de confiance), le « service interne » par 127.0.0.1 :
// ce n'est pas le meme hote pour le filtre.

type bancHotes struct {
	four     *httptest.Server
	interne  *httptest.Server
	hitsInt  atomic.Int32 // requetes recues par le service interne
	refererI atomic.Value // dernier Referer vu par le service interne
	cible    atomic.Value // vers ou le fournisseur redirige
	base     string       // http://localhost:<port du fournisseur>
}

func nouveauBancHotes(t *testing.T) *bancHotes {
	t.Helper()
	b := &bancHotes{}
	b.interne = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b.hitsInt.Add(1)
		b.refererI.Store(r.Header.Get("Referer"))
		w.Write([]byte("service-interne"))
	}))
	b.four = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/ici") {
			w.Write([]byte("fournisseur"))
			return
		}
		http.Redirect(w, r, b.cible.Load().(string), http.StatusFound)
	}))
	t.Cleanup(b.interne.Close)
	t.Cleanup(b.four.Close)
	_, port, _ := net.SplitHostPort(b.four.Listener.Addr().String())
	b.base = "http://localhost:" + port
	return b
}

func (b *bancHotes) client(t *testing.T, autorises []string, resoudre hotes.Resolveur) *Client {
	t.Helper()
	f, err := hotes.Nouveau(b.base, autorises, resoudre)
	if err != nil {
		t.Fatal(err)
	}
	cl := clientPour(b.base, "utilisateur-secret", "motdepasse-secret", AvecFiltreHotes(f))
	t.Cleanup(cl.Fermer)
	return cl
}

func (b *bancHotes) get(cl *Client, chemin string) (string, error) {
	rep, err := cl.Requete(context.Background(), b.base+chemin, nil)
	if err != nil {
		return "", err
	}
	defer rep.Body.Close()
	var sb strings.Builder
	buf := make([]byte, 64)
	for {
		n, e := rep.Body.Read(buf)
		sb.Write(buf[:n])
		if e != nil {
			break
		}
	}
	return sb.String(), nil
}

// La redirection du fournisseur vers un service interne n'est PAS suivie : aucune
// requete n'atteint ce service, l'erreur est typee et ne cite ni l'adresse ni les identifiants.
func TestCA2_RedirectionVersUnServiceInterneRefusee(t *testing.T) {
	b := nouveauBancHotes(t)
	cl := b.client(t, nil, nil)
	for _, cible := range []string{b.interne.URL + "/admin/secret-xyz", "http://[::1]:1/x", "http://10.0.0.5/x", "http://169.254.169.254/latest/meta-data/"} {
		b.cible.Store(cible)
		_, err := b.get(cl, "/live/utilisateur-secret/motdepasse-secret/1.m3u8")
		if !errors.Is(err, domain.ErrAmont) || !errors.Is(err, hotes.ErrRefuse) {
			t.Errorf("%s : erreur %v (ErrAmont + ErrRefuse attendues)", cible, err)
			continue
		}
		for _, secret := range []string{"secret-xyz", "utilisateur-secret", "motdepasse-secret", "127.0.0.1", "169.254"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s : l'erreur contient %q : %v", cible, secret, err)
			}
		}
	}
	if n := b.hitsInt.Load(); n != 0 {
		t.Fatalf("le service interne a recu %d requete(s)", n)
	}
}

// Un nom qui se resout vers une adresse interne est refuse des la redirection.
func TestCA2_RedirectionVersUnNomInterneRefusee(t *testing.T) {
	b := nouveauBancHotes(t)
	_, portInt, _ := net.SplitHostPort(b.interne.Listener.Addr().String())
	cl := b.client(t, nil, func(context.Context, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil
	})
	b.cible.Store("http://cdn.exemple.test:" + portInt + "/x")
	if _, err := b.get(cl, "/live/1.m3u8"); !errors.Is(err, hotes.ErrRefuse) {
		t.Errorf("erreur = %v", err)
	}
	if b.hitsInt.Load() != 0 {
		t.Fatal("le service interne a ete atteint")
	}
}

// US-17 CA2 : DNS rebinding sur une redirection. Le nom est public au controle de la
// redirection et interne a la connexion : la connexion est refusee.
func TestCA2_RebindingSurRedirectionRefuse(t *testing.T) {
	b := nouveauBancHotes(t)
	_, portInt, _ := net.SplitHostPort(b.interne.Listener.Addr().String())
	var appels atomic.Int32
	var mu sync.Mutex
	cl := b.client(t, nil, func(context.Context, string) ([]netip.Addr, error) {
		mu.Lock()
		defer mu.Unlock()
		if appels.Add(1) == 1 {
			return []netip.Addr{netip.MustParseAddr("93.184.216.34")}, nil // le controle de la redirection
		}
		return []netip.Addr{netip.MustParseAddr("127.0.0.1")}, nil // la connexion
	})
	b.cible.Store("http://rebind.exemple.test:" + portInt + "/x")
	_, err := b.get(cl, "/live/1.m3u8")
	if !errors.Is(err, hotes.ErrRefuse) {
		t.Fatalf("erreur = %v (rebinding non detecte)", err)
	}
	if appels.Load() < 2 {
		t.Errorf("%d resolution(s) : la connexion doit controler a nouveau", appels.Load())
	}
	if b.hitsInt.Load() != 0 {
		t.Fatal("le service interne a ete atteint par le rebinding")
	}
}

// Une redirection vers un hote que la configuration autorise est suivie, SANS Referer
// (l'adresse d'origine contient les identifiants).
func TestCA2_RedirectionAutoriseeSuivieSansReferer(t *testing.T) {
	b := nouveauBancHotes(t)
	cl := b.client(t, []string{"127.0.0.1"}, nil) // le LAN du chef de projet, liste a la main
	b.cible.Store(b.interne.URL + "/noeud2")
	corps, err := b.get(cl, "/live/utilisateur-secret/motdepasse-secret/1.m3u8")
	if err != nil || corps != "service-interne" {
		t.Fatalf("redirection autorisee : %q %v", corps, err)
	}
	if ref, _ := b.refererI.Load().(string); ref != "" {
		t.Errorf("Referer envoye a l'hote de la redirection : %q", ref)
	}
}

// Meme sans filtre (tests locaux), le Referer n'emporte jamais les identifiants.
func TestRedirectionSansFiltreNEnvoiePasLeReferer(t *testing.T) {
	b := nouveauBancHotes(t)
	cl := clientPour(b.base, "u", "p")
	defer cl.Fermer()
	b.cible.Store(b.interne.URL + "/noeud2")
	if _, err := b.get(cl, "/live/u/p/1.m3u8"); err != nil {
		t.Fatal(err)
	}
	if ref, _ := b.refererI.Load().(string); ref != "" {
		t.Errorf("Referer = %q", ref)
	}
}

// Pas de redirection : l'hote du fournisseur (nom de confiance) reste joignable.
func TestCA2_FournisseurJoignableAvecLeFiltre(t *testing.T) {
	b := nouveauBancHotes(t)
	cl := b.client(t, nil, nil)
	if corps, err := b.get(cl, "/ici"); err != nil || corps != "fournisseur" {
		t.Fatalf("%q %v", corps, err)
	}
}
