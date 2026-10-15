package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"mytv/internal/domain"
)

// dureeMaxSecondes borne position et duree acceptees (environ 68 ans : un entier 32 bits).
const dureeMaxSecondes = 1<<31 - 1

func (a *API) declarerProfil() {
	a.route("GET", "/profil/favoris", Session, a.favoris)
	a.route("PUT", "/profil/favoris/{id}", Session, a.ajouterFavori)
	a.route("DELETE", "/profil/favoris/{id}", Session, a.retirerFavori)
	a.route("GET", "/profil/historique", Session, a.historique)
	a.route("PUT", "/profil/historique/{id}", Session, a.enregistrerLecture)
	a.route("DELETE", "/profil/historique/{id}", Session, a.oublierLecture)
	a.route("GET", "/profil/reglages", Session, a.reglages)
	a.route("PUT", "/profil/reglages", Session, a.ecrireReglages)
}

// profilActif : l'etat n'est jamais adresse par un identifiant de l'URL, seulement
// par le profil actif de la SESSION : un profil ne peut pas viser celui d'un autre.
func (a *API) profilActif(w http.ResponseWriter, req *http.Request) (string, bool) {
	if a.opt.Etat == nil {
		erreur(w, http.StatusNotImplemented, "non_disponible", "etat par profil non active")
		return "", false
	}
	p := session(req).ProfilID
	if p == "" {
		erreur(w, http.StatusConflict, "profil_requis", "aucun profil actif : choisissez un profil")
		return "", false
	}
	return p, true
}

func (a *API) erreurProfil(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrLimite):
		erreur(w, http.StatusUnprocessableEntity, "limite_atteinte", "limite atteinte")
	case errors.Is(err, domain.ErrConflit):
		erreur(w, http.StatusConflict, "conflit", "reglages modifies entre-temps : relisez-les")
	default:
		a.erreurAmont(w, err, "profil")
	}
}

func (a *API) favoris(w http.ResponseWriter, req *http.Request) {
	p, ok := a.profilActif(w, req)
	if !ok {
		return
	}
	limite, decalage, ok := a.pagination(w, req)
	if !ok {
		return
	}
	tous, err := a.opt.Etat.Favoris(req.Context(), p)
	if err != nil {
		a.erreurProfil(w, err)
		return
	}
	fin := min(decalage+limite, len(tous))
	debut := min(decalage, fin)
	ecrireJSON(w, http.StatusOK, nouvellePage(tous[debut:fin], limite, decalage, fin < len(tous)))
}

func (a *API) ajouterFavori(w http.ResponseWriter, req *http.Request) {
	a.modifierFavori(w, req, true)
}

func (a *API) retirerFavori(w http.ResponseWriter, req *http.Request) {
	a.modifierFavori(w, req, false)
}

func (a *API) modifierFavori(w http.ResponseWriter, req *http.Request, ajout bool) {
	p, ok := a.profilActif(w, req)
	if !ok {
		return
	}
	id, ok := idDuChemin(w, req)
	if !ok {
		return
	}
	var err error
	if ajout {
		err = a.opt.Etat.AjouterFavori(req.Context(), p, id)
	} else {
		err = a.opt.Etat.RetirerFavori(req.Context(), p, id)
	}
	if err != nil {
		a.erreurProfil(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type lectureJSON struct {
	Contenu  domain.ContenuID `json:"contenu"`
	Position int64            `json:"position"`
	Duree    int64            `json:"duree"`
	Vu       time.Time        `json:"vu"`
}

func (a *API) historique(w http.ResponseWriter, req *http.Request) {
	p, ok := a.profilActif(w, req)
	if !ok {
		return
	}
	limite := a.opt.Config.PaginationDefaut
	if v := req.URL.Query().Get("limite"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > a.opt.Config.PaginationMax {
			requeteInvalide(w, "limite hors bornes")
			return
		}
		limite = n
	}
	ls, err := a.opt.Etat.Historique(req.Context(), p, limite)
	if err != nil {
		a.erreurProfil(w, err)
		return
	}
	out := make([]lectureJSON, len(ls))
	for i, l := range ls {
		out[i] = lectureJSON{l.Contenu, int64(l.Position / time.Second), int64(l.Duree / time.Second), l.Vu.UTC()}
	}
	ecrireJSON(w, http.StatusOK, map[string]any{"elements": out})
}

func (a *API) enregistrerLecture(w http.ResponseWriter, req *http.Request) {
	p, ok := a.profilActif(w, req)
	if !ok {
		return
	}
	id, ok := idDuChemin(w, req)
	if !ok {
		return
	}
	var in struct {
		Position int64 `json:"position"`
		Duree    int64 `json:"duree"`
	}
	if !lireJSON(w, req, &in) {
		return
	}
	if in.Position < 0 || in.Duree < 0 || in.Position > dureeMaxSecondes || in.Duree > dureeMaxSecondes || (in.Duree > 0 && in.Position > in.Duree) {
		requeteInvalide(w, "position ou duree invalide")
		return
	}
	err := a.opt.Etat.EnregistrerLecture(req.Context(), p, domain.Lecture{
		Contenu: id, Position: time.Duration(in.Position) * time.Second, Duree: time.Duration(in.Duree) * time.Second, Vu: a.maintenant().UTC()})
	if err != nil {
		a.erreurProfil(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) oublierLecture(w http.ResponseWriter, req *http.Request) {
	p, ok := a.profilActif(w, req)
	if !ok {
		return
	}
	id, ok := idDuChemin(w, req)
	if !ok {
		return
	}
	if err := a.opt.Etat.OublierLecture(req.Context(), p, id); err != nil {
		a.erreurProfil(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type reglagesJSON struct {
	Valeurs  map[string]string `json:"valeurs"`
	Revision int64             `json:"revision"`
}

func versReglages(r domain.Reglages) reglagesJSON {
	if r.Valeurs == nil {
		r.Valeurs = map[string]string{}
	}
	return reglagesJSON{r.Valeurs, r.Revision}
}

func (a *API) reglages(w http.ResponseWriter, req *http.Request) {
	p, ok := a.profilActif(w, req)
	if !ok {
		return
	}
	r, err := a.opt.Etat.Reglages(req.Context(), p)
	if err != nil {
		a.erreurProfil(w, err)
		return
	}
	ecrireJSON(w, http.StatusOK, versReglages(r))
}

func (a *API) ecrireReglages(w http.ResponseWriter, req *http.Request) {
	p, ok := a.profilActif(w, req)
	if !ok {
		return
	}
	var in reglagesJSON
	if !lireJSON(w, req, &in) {
		return
	}
	taille := 0
	for k, v := range in.Valeurs {
		taille += len(k) + len(v)
	}
	if a.opt.Profil.ReglagesMax > 0 && taille > a.opt.Profil.ReglagesMax {
		erreur(w, http.StatusUnprocessableEntity, "limite_atteinte", "reglages trop volumineux")
		return
	}
	r, err := a.opt.Etat.EcrireReglages(req.Context(), p, in.Valeurs, in.Revision)
	if err != nil {
		a.erreurProfil(w, err)
		return
	}
	ecrireJSON(w, http.StatusOK, versReglages(r))
}
