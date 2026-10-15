package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"mytv/internal/comptes"
	"mytv/internal/domain"
)

type profilJSON struct {
	ID  string `json:"id"`
	Nom string `json:"nom"`
}

type sessionJSON struct {
	ID       string    `json:"id"`
	Admin    bool      `json:"admin"`
	ProfilID string    `json:"profil_id,omitempty"`
	Expire   time.Time `json:"expire"`
}

func (a *API) declarerCompte() {
	// Connexion : sans session, mais soumise a la limitation de tentatives du service.
	a.routes = append(a.routes, Route{"POST", "/connexion", Public})
	a.mux.Handle("POST "+Prefixe+"/connexion", a.opt.Auth.Connexion(a.opt.Config.CorpsMax))
	a.routes = append(a.routes, Route{"POST", "/deconnexion", Session})
	a.mux.Handle("POST "+Prefixe+"/deconnexion", a.opt.Auth.Proteger(a.opt.Auth.Deconnexion()))

	a.route("GET", "/compte", Session, a.compte)
	a.route("POST", "/compte/mot-de-passe", Session, a.changerMotDePasse)
	a.route("GET", "/compte/profils", Session, a.profils)
	a.route("POST", "/compte/profils", Session, a.creerProfil)
	a.route("DELETE", "/compte/profils/{profil}", Session, a.supprimerProfil)
	a.route("PUT", "/compte/profil-actif", Session, a.choisirProfil)
}

// session rend la session placee par l'intergiciel d'authentification.
func session(req *http.Request) *domain.Session {
	s, _ := comptes.SessionDe(req.Context())
	return s
}

// lireJSON decode un corps JSON strict (champs inconnus refuses, un seul document).
func lireJSON(w http.ResponseWriter, req *http.Request, v any) bool {
	dec := json.NewDecoder(req.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var trop *http.MaxBytesError
		if errors.As(err, &trop) {
			erreur(w, http.StatusRequestEntityTooLarge, "corps_trop_gros", "corps de requete trop gros")
			return false
		}
		requeteInvalide(w, "corps JSON invalide")
		return false
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		requeteInvalide(w, "corps JSON invalide")
		return false
	}
	return true
}

func (a *API) compte(w http.ResponseWriter, req *http.Request) {
	s := session(req)
	ps, err := a.opt.Comptes.Profils(req.Context(), s.UtilisateurID)
	if err != nil {
		a.erreurAmont(w, err, "compte")
		return
	}
	out := make([]profilJSON, len(ps))
	for i, p := range ps {
		out[i] = profilJSON{p.ID, p.Nom}
	}
	ecrireJSON(w, http.StatusOK, map[string]any{
		"session": sessionJSON{s.ID, s.Admin, s.ProfilID, s.Expire}, "profils": out, "limites": a.limitesJSON()})
}

// limitesJSON : les bornes de la configuration que l'interface doit respecter, pour
// qu'elle decoupe ses appels sans les deviner (voir schema Compte.limites).
func (a *API) limitesJSON() map[string]int64 {
	return map[string]int64{
		"pagination_max":      int64(a.opt.Config.PaginationMax),
		"grille_fenetre":      int64(a.opt.Config.GrilleFenetreMax.Std() / time.Second),
		"favoris_max":         int64(a.opt.Profil.FavorisMax),
		"historique_max":      int64(a.opt.Profil.HistoriqueMax),
		"reglages_octets_max": int64(a.opt.Profil.ReglagesMax),
	}
}

func (a *API) changerMotDePasse(w http.ResponseWriter, req *http.Request) {
	var in struct {
		Ancien  string `json:"ancien"`
		Nouveau string `json:"nouveau"`
	}
	if !lireJSON(w, req, &in) {
		return
	}
	s := session(req)
	err := a.opt.Comptes.ChangerMotDePasse(req.Context(), s.UtilisateurID, in.Ancien, in.Nouveau, s.ID)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, comptes.ErrMotDePasseFaible):
		erreur(w, http.StatusBadRequest, "mot_de_passe_faible", "mot de passe trop court ou trop long")
	case errors.Is(err, domain.ErrIdentifiantsInvalides):
		// 403 et non 401 : la session reste valide, un 401 ferait deconnecter l'interface.
		erreur(w, http.StatusForbidden, "identifiants_invalides", "ancien mot de passe incorrect")
	case errors.Is(err, domain.ErrTropDeTentatives):
		w.Header().Set("Retry-After", "60")
		erreur(w, http.StatusTooManyRequests, "trop_de_tentatives", "trop de tentatives, reessayez plus tard")
	default:
		a.erreurAmont(w, err, "compte")
	}
}

func (a *API) profils(w http.ResponseWriter, req *http.Request) {
	ps, err := a.opt.Comptes.Profils(req.Context(), session(req).UtilisateurID)
	if err != nil {
		a.erreurAmont(w, err, "profils")
		return
	}
	out := make([]profilJSON, len(ps))
	for i, p := range ps {
		out[i] = profilJSON{p.ID, p.Nom}
	}
	ecrireJSON(w, http.StatusOK, map[string]any{"elements": out})
}

func (a *API) creerProfil(w http.ResponseWriter, req *http.Request) {
	var in struct {
		Nom string `json:"nom"`
	}
	if !lireJSON(w, req, &in) {
		return
	}
	p, err := a.opt.Comptes.CreerProfil(req.Context(), session(req).UtilisateurID, in.Nom)
	switch {
	case err == nil:
		ecrireJSON(w, http.StatusCreated, profilJSON{p.ID, p.Nom})
	case errors.Is(err, comptes.ErrNomInvalide):
		requeteInvalide(w, "nom de profil invalide")
	case errors.Is(err, domain.ErrExiste):
		erreur(w, http.StatusConflict, "existe_deja", "un profil porte deja ce nom")
	default:
		a.erreurAmont(w, err, "profil")
	}
}

func (a *API) supprimerProfil(w http.ResponseWriter, req *http.Request) {
	id := req.PathValue("profil")
	if !idOpaque(id) {
		requeteInvalide(w, "identifiant de profil invalide")
		return
	}
	err := a.opt.Comptes.SupprimerProfil(req.Context(), session(req).UtilisateurID, id)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, comptes.ErrIntrouvable):
		erreur(w, http.StatusNotFound, "introuvable", "profil introuvable")
	default:
		a.erreurAmont(w, err, "profil")
	}
}

func (a *API) choisirProfil(w http.ResponseWriter, req *http.Request) {
	var in struct {
		ProfilID string `json:"profil_id"`
	}
	if !lireJSON(w, req, &in) {
		return
	}
	if !idOpaque(in.ProfilID) {
		requeteInvalide(w, "identifiant de profil invalide")
		return
	}
	err := a.opt.Comptes.ChoisirProfil(req.Context(), session(req).ID, in.ProfilID)
	switch {
	case err == nil:
		w.WriteHeader(http.StatusNoContent)
	case errors.Is(err, comptes.ErrIntrouvable):
		erreur(w, http.StatusNotFound, "introuvable", "profil introuvable")
	default:
		a.erreurAmont(w, err, "profil")
	}
}

// idOpaque : identifiant hexadecimal genere par le service de comptes.
func idOpaque(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	return strings.Trim(s, "0123456789abcdef") == ""
}
