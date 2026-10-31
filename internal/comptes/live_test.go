package comptes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
		Secret: []byte("0123456789abcdef0123456789abcdef"), Authentifier: b.AuthentifierLecture, EstAdmin: b.EstAdmin,
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
	if rec.Code != http.StatusForbidden {
		t.Fatalf("utilisateur ordinaire sur /etat : %d (403 attendu)", rec.Code)
	}
	b.utilisateur(t, "root", true)
	jr, _, _ := b.Connecter(context.Background(), "root", mdp, "10.0.0.1")
	req = httptest.NewRequest("GET", "/api/v1/live/etat", nil)
	req.AddCookie(&http.Cookie{Name: b.nomCookie(), Value: jr})
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("administrateur sur /etat : %d", rec.Code)
	}
}

// US-40 CA2 : une session imposee par la route (jeton de diffusion verifie) est reconnue
// par AuthentifierLecture AVANT le cookie et le compte local, pour GET/HEAD seulement ;
// elle ne vaut ni pour Authentifier (ecriture) ni pour EstAdmin.
func TestCA2_SessionImposeeReconnueEnLectureSeulement(t *testing.T) {
	b := nouveauBanc(t, nil)
	for _, m := range []string{"GET", "HEAD"} {
		req := httptest.NewRequest(m, "/api/v1/live/1.m3u8", nil)
		req = req.WithContext(b.ImposerSession(req.Context(), "sess-1"))
		if id, ok := b.AuthentifierLecture(req); !ok || id != "sess-1" {
			t.Errorf("%s : session imposee attendue, %q %v", m, id, ok)
		}
		if _, ok := b.Authentifier(req); ok {
			t.Errorf("%s : la session imposee ne doit pas valoir comme vraie session", m)
		}
		if b.EstAdmin(req) {
			t.Errorf("%s : jamais administrateur", m)
		}
	}
	req := httptest.NewRequest("POST", "/api/v1/live/1/fin", nil)
	req = req.WithContext(b.ImposerSession(req.Context(), "sess-1"))
	if _, ok := b.AuthentifierLecture(req); ok {
		t.Error("une ecriture n'est jamais authentifiee par une session imposee")
	}
	if _, ok := b.AuthentifierLecture(httptest.NewRequest("GET", "/x", nil)); ok {
		t.Error("sans rien : refus")
	}
}

// US-40 CA2 : SessionParID retrouve une session vivante et refuse une session revoquee
// (deconnexion) ou expiree.
func TestCA2_SessionParIDRefuseRevoqueeEtExpiree(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.utilisateur(t, "alice", false)
	ctx := context.Background()
	jeton, sess, err := b.Connecter(ctx, "alice", mdp, "10.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	if s, err := b.SessionParID(ctx, sess.ID); err != nil || s.ID != sess.ID || s.Nom != "alice" {
		t.Fatalf("session vivante : %+v %v", s, err)
	}
	if _, err := b.SessionParID(ctx, "inconnue"); err == nil {
		t.Fatal("identifiant inconnu accepte")
	}
	if err := b.Deconnecter(ctx, jeton); err != nil {
		t.Fatal(err)
	}
	if _, err := b.SessionParID(ctx, sess.ID); err == nil {
		t.Fatal("session revoquee acceptee")
	}
	_, sess2, _ := b.Connecter(ctx, "alice", mdp, "10.0.0.1")
	b.h.Avancer(b.cfg.DureeSession.Std() + time.Minute)
	if _, err := b.SessionParID(ctx, sess2.ID); err == nil {
		t.Fatal("session expiree acceptee")
	}
}
