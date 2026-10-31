package xtream

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"mytv/internal/config"
	"mytv/internal/testutil/fauxiptv"
)

// proxySortant : un proxy HTTP minimal (requetes en URI absolue) qui compte ce qui passe.
func proxySortant(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !r.URL.IsAbs() {
			http.Error(w, "attendu une URI absolue (requete de proxy)", http.StatusBadRequest)
			return
		}
		n.Add(1)
		req, _ := http.NewRequestWithContext(r.Context(), r.Method, r.URL.String(), r.Body)
		req.Header = r.Header.Clone()
		rep, err := http.DefaultTransport.RoundTrip(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer rep.Body.Close()
		for k, v := range rep.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(rep.StatusCode)
		io.Copy(w, rep.Body)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

// Banc GCP : une instance hebergee chez un hebergeur doit sortir vers le fournisseur par la
// connexion de la maison (reseau.proxy_sortie). API ET flux passent par le proxy ; sans
// reglage, rien n'y passe.
func TestProxySortieRouteAPIEtFluxParLeProxy(t *testing.T) {
	f := fauxiptv.Nouveau(t, fauxiptv.Options{Catalogue: fauxiptv.CatalogueExemple(2, 1, 1)})
	proxy, passes := proxySortant(t)
	res := reseauTest()
	res.ProxySortie = proxy.URL
	c := Nouveau(config.Fournisseur{ID: "iptva", Base: f.URL(), Reseau: res}, f.Utilisateur(), f.MotDePasse())
	if _, err := c.Catalogue(context.Background()); err != nil {
		t.Fatal(err)
	}
	api := passes.Load()
	if api == 0 {
		t.Fatal("le catalogue doit passer par le proxy de sortie")
	}
	rep, err := c.Requete(context.Background(), c.URLLive("1001"), nil)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, rep.Body)
	rep.Body.Close()
	if passes.Load() <= api {
		t.Fatal("la playlist du direct doit aussi passer par le proxy de sortie")
	}
	// Sans reglage : sortie directe, le proxy ne voit rien.
	avant := passes.Load()
	direct := clientFaux(f)
	if _, err := direct.Catalogue(context.Background()); err != nil {
		t.Fatal(err)
	}
	if passes.Load() != avant {
		t.Fatal("sans proxy_sortie, aucune requete ne doit passer par le proxy")
	}
}

func TestProxySortieInvalideRefuseParLaConfiguration(t *testing.T) {
	for _, mauvais := range []string{"ftp://x:1", "socks5://", "pas une adresse", "http://"} {
		r := config.Reseau{ProxySortie: mauvais}
		if _, err := r.ProxySortieURL(); err == nil || !strings.Contains(err.Error(), "proxy_sortie") {
			t.Errorf("%q : erreur attendue, %v", mauvais, err)
		}
	}
	for _, bon := range []string{"http://10.77.0.2:8888", "socks5://10.77.0.2:1080", ""} {
		if _, err := (config.Reseau{ProxySortie: bon}).ProxySortieURL(); err != nil {
			t.Errorf("%q : accepte attendu, %v", bon, err)
		}
	}
}
