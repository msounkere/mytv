package xtream

import (
	"context"
	"io"
	"testing"
	"time"

	"mytv/internal/config"
	"mytv/internal/testutil/fauxiptv"
)

// US-27 : hygiene des connexions vers le fournisseur. Le faux fournisseur compte les
// connexions TCP qu'il voit ouvertes ; le fournisseur reel lie sa « connexion unique » a
// elles, donc une connexion qui traine est une place gardee apres le depart du client.

func attendreConnexions(t *testing.T, f *fauxiptv.Faux, n int, quoi string) {
	t.Helper()
	fin := time.Now().Add(3 * time.Second)
	for f.ConnexionsOuvertes() != n {
		if time.Now().After(fin) {
			t.Fatalf("%s : %d connexions ouvertes, attendu %d", quoi, f.ConnexionsOuvertes(), n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func lireTout(t *testing.T, c *Client, url string) {
	t.Helper()
	rep, err := c.Requete(context.Background(), url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, rep.Body); err != nil {
		t.Fatal(err)
	}
	rep.Body.Close()
}

func clientConnexions(f *fauxiptv.Faux, fermer *bool) *Client {
	r := reseauTest()
	r.FermerConnexionsFlux = fermer
	return Nouveau(config.Fournisseur{ID: "iptva", Base: f.URL(), Reseau: r}, f.Utilisateur(), f.MotDePasse())
}

func ptr(b bool) *bool { return &b }

// CA1/CA2 : playlist, segments et film lus jusqu'au bout (zapping, fin de lecture) :
// aucune connexion ne reste ouverte vers le faux fournisseur, option par defaut.
func TestCA2_AucuneConnexionOuverteApresUneLectureDeFlux(t *testing.T) {
	f := fauxiptv.Nouveau(t, fauxiptv.Options{TailleFilm: 50_000})
	c := clientConnexions(f, nil) // defaut : fermer
	t.Cleanup(c.Fermer)
	for _, chemin := range []string{
		"/live/u/p/100.m3u8", "/hls/abc/100_150.ts", "/hls/abc/100_151.ts",
		"/movie/u/p/2001.mp4", "/series/u/p/3001.mkv", "/live/u/p/200.m3u8", // zapping
	} {
		lireTout(t, c, f.URL()+chemin)
	}
	attendreConnexions(t, f, 0, "apres la lecture")
	if f.ConnexionsTotal() < 6 {
		t.Errorf("%d connexions ouvertes pour 6 requetes : le test ne prouve rien", f.ConnexionsTotal())
	}
}

// CA1 : la requete annonce bien `Connection: close`.
func TestCA1_LesRequetesDeFluxAnnoncentConnectionClose(t *testing.T) {
	f := fauxiptv.Nouveau(t, fauxiptv.Options{})
	c := clientConnexions(f, nil)
	t.Cleanup(c.Fermer)
	lireTout(t, c, f.URL()+"/hls/abc/100_150.ts")
	tr := f.Traces()
	if len(tr) != 1 || tr[0].Entetes.Get("Connection") != "close" {
		t.Errorf("traces = %+v", tr)
	}
}

// CA1 : l'API (catalogue, guide) garde le keep-alive : la connexion reste ouverte,
// reutilisee par l'appel suivant.
func TestCA1_LAPIGardeLeKeepAlive(t *testing.T) {
	f := fauxiptv.Nouveau(t, fauxiptv.Options{Catalogue: fauxiptv.CatalogueExemple(3, 2, 1), XMLTV: []byte("<tv></tv>")})
	c := clientConnexions(f, nil)
	t.Cleanup(c.Fermer)
	if _, err := c.Catalogue(context.Background()); err != nil {
		t.Fatal(err)
	}
	lireTout(t, c, c.URLEPG())
	if n := f.ConnexionsOuvertes(); n != 1 {
		t.Errorf("%d connexions ouvertes apres le catalogue et le guide ; une seule, reutilisee, attendue", n)
	}
	if f.ConnexionsTotal() != 1 {
		t.Errorf("%d connexions ouvertes en tout : l'API ne reutilise pas la sienne", f.ConnexionsTotal())
	}
}

// CA1 : fermer_connexions_flux: false rend le comportement d'avant (keep-alive pour les
// flux) : sert aussi de contre-epreuve aux tests ci-dessus.
func TestCA1_OptionDesactiveeLesFluxGardentLeurConnexion(t *testing.T) {
	f := fauxiptv.Nouveau(t, fauxiptv.Options{})
	c := clientConnexions(f, ptr(false))
	t.Cleanup(c.Fermer)
	lireTout(t, c, f.URL()+"/hls/abc/100_150.ts")
	lireTout(t, c, f.URL()+"/hls/abc/100_151.ts")
	if f.ConnexionsOuvertes() != 1 || f.ConnexionsTotal() != 1 {
		t.Errorf("ouvertes %d, total %d ; keep-alive attendu", f.ConnexionsOuvertes(), f.ConnexionsTotal())
	}
	c.Fermer()
	attendreConnexions(t, f, 0, "apres Fermer")
}

// CA1 : une redirection (premier appel d'une playlist) est suivie sans laisser de connexion.
func TestCA2_UneRedirectionNeLaissePasDeConnexion(t *testing.T) {
	f := fauxiptv.Nouveau(t, fauxiptv.Options{RedirigerPlaylist: true})
	c := clientConnexions(f, nil)
	t.Cleanup(c.Fermer)
	lireTout(t, c, f.URL()+"/live/u/p/100.m3u8")
	attendreConnexions(t, f, 0, "apres la redirection")
}
