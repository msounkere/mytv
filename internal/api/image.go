package api

import (
	"errors"
	"net/http"
	"regexp"

	"mytv/internal/images"
)

// reJeton : alphabet base64url (le jeton est opaque). Borne la longueur avant tout dechiffrement.
var reJeton = regexp.MustCompile(`^[A-Za-z0-9_-]{16,1000}$`)

// image reecrit une URL d'image d'origine en URL de ce service.
func (a *API) image(url string) string {
	if a.opt.Images == nil {
		return url
	}
	return a.opt.Images.Reecrire(url)
}

// empreinteImages : partie des ETag liee aux jetons emis.
func (a *API) empreinteImages() string {
	if a.opt.Images == nil {
		return ""
	}
	return a.opt.Images.Empreinte()
}

// imageServie sert une image par son jeton. La route exige une session ; elle ne prend
// JAMAIS d'URL : le jeton n'ouvre que ce que ce service a emis.
func (a *API) imageServie(w http.ResponseWriter, req *http.Request) {
	if a.opt.Images == nil {
		erreur(w, http.StatusNotImplemented, "non_disponible", "proxy d'images non active")
		return
	}
	jeton := req.PathValue("jeton")
	if !reJeton.MatchString(jeton) {
		erreur(w, http.StatusForbidden, "jeton_invalide", "jeton d'image invalide")
		return
	}
	img, err := a.opt.Images.Obtenir(req.Context(), jeton)
	switch {
	case err == nil:
	case errors.Is(err, images.ErrJeton):
		erreur(w, http.StatusForbidden, "jeton_invalide", "jeton d'image invalide")
		return
	case errors.Is(err, images.ErrRefuse):
		erreur(w, http.StatusForbidden, "hote_refuse", "adresse d'image non autorisee")
		return
	case errors.Is(err, images.ErrIntrouvable):
		erreur(w, http.StatusNotFound, "image_introuvable", "image introuvable")
		return
	case errors.Is(err, images.ErrSature):
		w.Header().Set("Retry-After", "2")
		erreur(w, http.StatusServiceUnavailable, "occupe", "trop de telechargements d'images en cours")
		return
	case contexteAnnule(req.Context()):
		return // le client est parti
	default:
		erreur(w, http.StatusBadGateway, "image_indisponible", "image indisponible")
		return
	}
	defer img.Fermer()

	h := w.Header()
	h.Set("Content-Type", img.Type)
	h.Set("ETag", img.ETag)
	// private : l'image n'est pas partageable par un cache intermediaire (elle est
	// derriere une session). Un jour : le jeton est stable, donc le navigateur ne
	// redemande pas ; le 304 couvre le reste.
	h.Set("Cache-Control", "private, max-age=86400")
	// Une image est ouverte directement dans un onglet si l'on colle son adresse : un
	// SVG y executerait ses scripts DANS NOTRE ORIGINE. « sandbox » les neutralise.
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; sandbox")
	// ServeContent gere If-None-Match (304), If-Modified-Since, HEAD et Range.
	http.ServeContent(w, req, "", img.Modif, img.Contenu)
}
