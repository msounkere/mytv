package hotes

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
	"time"
)

func addr(s string) netip.Addr { return netip.MustParseAddr(s) }

// resolveur fixe : nom -> adresses.
func resolveur(table map[string][]string) Resolveur {
	return func(_ context.Context, h string) ([]netip.Addr, error) {
		var out []netip.Addr
		for _, a := range table[h] {
			out = append(out, addr(a))
		}
		if out == nil {
			return nil, errors.New("nom inconnu")
		}
		return out, nil
	}
}

// rebinding : repond une adresse PUBLIQUE aux `n` premieres resolutions, puis `ensuite`.
type rebinding struct {
	mu      sync.Mutex
	appels  int
	n       int
	ensuite string
}

func (r *rebinding) resoudre(context.Context, string) ([]netip.Addr, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.appels++
	if r.appels <= r.n {
		return []netip.Addr{addr("93.184.216.34")}, nil
	}
	return []netip.Addr{addr(r.ensuite)}, nil
}

// serveurCompteur : un « service interne » qui compte les connexions TCP recues.
func serveurCompteur(t *testing.T) (port string, connexions *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("interne")) }))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			n.Add(1)
		}
	}
	srv.Start()
	t.Cleanup(srv.Close)
	_, p, _ := net.SplitHostPort(srv.Listener.Addr().String())
	return p, &n
}

// US-17 CA2 : quelles adresses sont autorisees.
func TestAutoriseeTable(t *testing.T) {
	f, err := Nouveau("http://fournisseur.test", []string{"192.168.50.0/24", "10.1.2.3", "lan.test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for a, ok := range map[string]bool{
		"93.184.216.34": true, "8.8.8.8": true, "2606:4700::1111": true,
		"127.0.0.1": false, "127.9.9.9": false, "::1": false, "10.0.0.5": false, "10.1.2.3": true, "192.168.1.1": false,
		"192.168.50.9": true, "169.254.169.254": false, "172.16.0.1": false, "172.31.255.255": false, "100.64.0.1": false,
		"0.0.0.0": false, "224.0.0.1": false, "fe80::1": false, "fc00::1": false, "198.18.0.1": false, "240.0.0.1": false,
		"::ffff:127.0.0.1": false, "::ffff:10.0.0.1": false, "::ffff:93.184.216.34": true, "64:ff9b::7f00:1": false,
	} {
		if got := f.Autorisee(addr(a)); got != ok {
			t.Errorf("%s : autorisee = %v, attendu %v", a, got, ok)
		}
	}
	for _, mauvais := range []string{"pas valide/", "a b", "http://x/y"} {
		if _, err := Nouveau("http://f.test", []string{mauvais}, nil); err == nil {
			t.Errorf("%q accepte", mauvais)
		}
	}
}

// US-17 CA2 : une connexion vers une adresse interne n'est JAMAIS ouverte — on le
// prouve par le compteur de connexions du service « interne », pas par l'erreur.
func TestCA2_DialRefuseLesAdressesInternesSansConnecter(t *testing.T) {
	port, n := serveurCompteur(t)
	f, _ := Nouveau("http://fournisseur.test", nil, resolveur(map[string][]string{
		"interne.test": {"127.0.0.1"}, "mixte.test": {"93.184.216.34", "127.0.0.1"}, "v6.test": {"::1"},
	}))
	dial := f.DialContext(time.Second)
	for _, hote := range []string{"127.0.0.1", "interne.test", "mixte.test", "v6.test", "[::1]", "::ffff:127.0.0.1", "localhost-inconnu.test"} {
		hote = strings.Trim(hote, "[]")
		c, err := dial(context.Background(), "tcp", net.JoinHostPort(hote, port))
		if err == nil {
			c.Close()
			t.Errorf("%s : connexion ouverte", hote)
			continue
		}
		if !errors.Is(err, ErrRefuse) {
			t.Errorf("%s : erreur %v (ErrRefuse attendue)", hote, err)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if n.Load() != 0 {
		t.Fatalf("%d connexion(s) ont atteint le service interne", n.Load())
	}
}

// Un hote liste par la configuration, ou le fournisseur lui-meme, se joint normalement ; un
// nom autorise par une PLAGE se joint a l'adresse resolue par NOTRE resolveur (pas de
// seconde resolution).
func TestCA2_DialAutorise(t *testing.T) {
	port, n := serveurCompteur(t)
	// 1. Plage autorisee + nom factice : la connexion va a 127.0.0.1, adresse du resolveur factice.
	f, _ := Nouveau("http://fournisseur.test", []string{"127.0.0.0/8"}, resolveur(map[string][]string{"interne.test": {"127.0.0.1"}}))
	c, err := f.DialContext(time.Second)(context.Background(), "tcp", net.JoinHostPort("interne.test", port))
	if err != nil {
		t.Fatalf("hote autorise par plage : %v", err)
	}
	c.Close()
	// 2. L'hote du fournisseur est de confiance (ici : une adresse litterale locale).
	f, _ = Nouveau("http://127.0.0.1:"+port, nil, nil)
	c, err = f.DialContext(time.Second)(context.Background(), "tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		t.Fatalf("hote du fournisseur : %v", err)
	}
	c.Close()
	// 3. Un nom liste par la configuration est de confiance (resolution du systeme).
	f, _ = Nouveau("http://fournisseur.test", []string{"localhost"}, nil)
	c, err = f.DialContext(time.Second)(context.Background(), "tcp", net.JoinHostPort("localhost", port))
	if err != nil {
		t.Fatalf("nom liste : %v", err)
	}
	c.Close()
	time.Sleep(50 * time.Millisecond)
	if n.Load() != 3 {
		t.Errorf("%d connexions, attendu 3", n.Load())
	}
}

// US-17 CA2 : DNS rebinding. Le nom se resout vers une adresse publique quand on
// verifie l'adresse annoncee, puis vers 127.0.0.1 quand on se connecte : la connexion
// est refusee et le service interne ne voit rien.
func TestCA2_DNSRebindingRefuseALaConnexion(t *testing.T) {
	port, n := serveurCompteur(t)
	r := &rebinding{n: 1, ensuite: "127.0.0.1"}
	f, _ := Nouveau("http://fournisseur.test", nil, r.resoudre)
	if err := f.VerifierURL(context.Background(), "http://rebind.test:"+port+"/x"); err != nil {
		t.Fatalf("la verification precoce doit passer (adresse publique) : %v", err)
	}
	c, err := f.DialContext(time.Second)(context.Background(), "tcp", net.JoinHostPort("rebind.test", port))
	if err == nil {
		c.Close()
		t.Fatal("connexion ouverte apres le rebinding")
	}
	if !errors.Is(err, ErrRefuse) {
		t.Errorf("erreur = %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if n.Load() != 0 {
		t.Fatalf("le service interne a recu %d connexion(s)", n.Load())
	}
}

// Le net.Dialer controle aussi l'adresse reellement utilisee (filet).
func TestControleConnexion(t *testing.T) {
	f, _ := Nouveau("http://fournisseur.test", nil, nil)
	for a, ok := range map[string]bool{"93.184.216.34:80": true, "127.0.0.1:80": false, "[::1]:80": false, "10.0.0.1:9": false,
		"pas-une-adresse": false, "nom.test:80": false, "[::ffff:192.168.1.1]:80": false} {
		err := f.controleConnexion("tcp", a, nil)
		if (err == nil) != ok {
			t.Errorf("%s : erreur = %v, autorisee attendue = %v", a, err, ok)
		}
	}
}

func TestVerifierURLRefusEtAcceptations(t *testing.T) {
	f, _ := Nouveau("http://fournisseur.test", []string{"lan.test"}, resolveur(map[string][]string{
		"public.test": {"93.184.216.34"}, "prive.test": {"10.0.0.1"}, "mixte.test": {"93.184.216.34", "127.0.0.1"}}))
	ok := []string{"http://public.test/x", "https://public.test:8443/x", "http://fournisseur.test/live/1", "http://lan.test/x", "http://93.184.216.34/x"}
	ko := []string{"http://prive.test/x", "http://mixte.test/x", "http://inconnu.test/x", "http://127.0.0.1/x", "http://[::1]/x",
		"ftp://public.test/x", "file:///etc/passwd", "http://user:pass@public.test/x", "http:///x", "public.test/x", "http://[::ffff:10.0.0.1]/x"}
	for _, u := range ok {
		if err := f.VerifierURL(context.Background(), u); err != nil {
			t.Errorf("%s refuse : %v", u, err)
		}
	}
	for _, u := range ko {
		if err := f.VerifierURL(context.Background(), u); !errors.Is(err, ErrRefuse) {
			t.Errorf("%s : erreur = %v, attendu ErrRefuse", u, err)
		}
	}
}

// US-17 CA2 : chaque saut est reverifie, le Referer est retire, le message ne cite aucune adresse.
func TestControleRedirection(t *testing.T) {
	f, _ := Nouveau("http://fournisseur.test", nil, resolveur(map[string][]string{"public.test": {"93.184.216.34"}, "prive.test": {"10.0.0.1"}}))
	ctrl := f.ControleRedirection(3)
	nouvelle := func(u string) *http.Request {
		r, _ := http.NewRequest("GET", u, nil)
		r.Header.Set("Referer", "http://fournisseur.test/live/UTILISATEUR/MOTDEPASSE/1.m3u8")
		return r
	}
	via := func(n int) []*http.Request { return make([]*http.Request, n) }

	r := nouvelle("http://public.test/ok")
	if err := ctrl(r, via(1)); err != nil {
		t.Fatalf("saut vers un hote public : %v", err)
	}
	if r.Header.Get("Referer") != "" {
		t.Error("le Referer (identifiants dans le chemin) a ete conserve")
	}
	for _, u := range []string{"http://prive.test/secret-jeton-xyz", "http://127.0.0.1:9999/secret-jeton-xyz", "http://[::1]/secret-jeton-xyz"} {
		err := ctrl(nouvelle(u), via(1))
		if !errors.Is(err, ErrRefuse) {
			t.Errorf("%s : %v", u, err)
		} else if strings.Contains(err.Error(), "secret-jeton-xyz") || strings.Contains(err.Error(), "prive.test") || strings.Contains(err.Error(), "127.0.0.1") {
			t.Errorf("le message cite la destination : %v", err)
		}
	}
	if err := ctrl(nouvelle("http://public.test/ok"), via(4)); err == nil {
		t.Error("plus de 3 sauts accepte")
	}
}
