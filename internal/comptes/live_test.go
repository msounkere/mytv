package comptes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"mytv/internal/config"
	"mytv/internal/live"
)

type fournisseurNul struct{ domainFournisseur }
type bauxNul struct{ domainBaux }

// CA5 : l'authentificateur du service se branche tel quel sur le relais du direct.
func TestAuthentificateurBrancheSurLeRelais(t *testing.T) {
	b := nouveauBanc(t, func(c *config.Comptes) {
		c.LocalUtilisateur = "local"
		c.PlagesLocales = []string{"192.168.50.0/24"}
	})
	b.utilisateur(t, "alice", false)
	d := config.Defauts()
	reseau := config.Reseau{DelaiPlaylist: 1, DelaiSegment: 1, EssaisPlaylist: 1, EssaisSegment: 1, RequetesAmontMax: 1}
	r, err := live.Nouveau(live.Options{
		Fournisseur: fournisseurNul{}, Baux: bauxNul{}, Live: d.Live, Bail: d.Bail, Reseau: reseau,
		Secret: []byte("0123456789abcdef0123456789abcdef"), Authentifier: b.AuthentifierLecture,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Fermer()
	h := r.Handler()
	jeton, _, _ := b.Connecter(context.Background(), "alice", mdp, "10.0.0.1")

	req := httptest.NewRequest("GET", "/api/v1/live/etat", nil)
	req.RemoteAddr = "203.0.113.9:1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("sans session : %d", rec.Code)
	}
	req.AddCookie(&http.Cookie{Name: b.nomCookie(), Value: jeton})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("avec session : %d", rec.Code)
	}
}
