package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"mytv/internal/domain"
)

func ecrireJSON(w http.ResponseWriter, statut int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if w.Header().Get("Cache-Control") == "" {
		w.Header().Set("Cache-Control", "no-store")
	}
	w.WriteHeader(statut)
	_ = json.NewEncoder(w).Encode(v)
}

type corpsErreur struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// erreur ecrit l'erreur normalisee {code, message}. Le message est ecrit a la main
// par l'appelant : jamais le texte d'une erreur interne (qui pourrait citer une URL
// ou un identifiant).
func erreur(w http.ResponseWriter, statut int, code, message string) {
	w.Header().Del("ETag")
	w.Header().Set("Cache-Control", "no-store")
	ecrireJSON(w, statut, corpsErreur{code, message})
}

func requeteInvalide(w http.ResponseWriter, message string) {
	erreur(w, http.StatusBadRequest, "requete_invalide", message)
}

// erreurAmont traduit une erreur de dependance en reponse HTTP.
func (a *API) erreurAmont(w http.ResponseWriter, err error, quoi string) {
	switch {
	case errors.Is(err, domain.ErrNonTrouve), errors.Is(err, domain.ErrIntrouvable):
		erreur(w, http.StatusNotFound, "introuvable", quoi+" introuvable")
	case errors.Is(err, domain.ErrPlaceIndisponible), errors.Is(err, domain.ErrRefuseConnexions):
		w.Header().Set("Retry-After", "3")
		erreur(w, http.StatusServiceUnavailable, "occupe", "connexion du fournisseur occupee")
	case errors.Is(err, domain.ErrAmont), errors.Is(err, domain.ErrRefuseIP), errors.Is(err, domain.ErrIdentifiants):
		a.log.Warn("fournisseur en erreur", "erreur", err)
		erreur(w, http.StatusBadGateway, "fournisseur_indisponible", "fournisseur indisponible")
	default:
		a.log.Error("erreur interne", "quoi", quoi, "erreur", err)
		erreur(w, http.StatusInternalServerError, "erreur_interne", "erreur interne")
	}
}

// page est l'enveloppe de pagination.
type page[T any] struct {
	Elements []T  `json:"elements"`
	Limite   int  `json:"limite"`
	Decalage int  `json:"decalage"`
	Suivant  *int `json:"suivant"`
}

func nouvellePage[T any](el []T, limite, decalage int, encore bool) page[T] {
	if el == nil {
		el = []T{}
	}
	p := page[T]{Elements: el, Limite: limite, Decalage: decalage}
	if encore {
		s := decalage + len(el)
		p.Suivant = &s
	}
	return p
}

// pagination lit limite et decalage ; hors bornes = 400 (pas de correction silencieuse).
func (a *API) pagination(w http.ResponseWriter, req *http.Request) (limite, decalage int, ok bool) {
	limite, decalage = a.opt.Config.PaginationDefaut, 0
	q := req.URL.Query()
	if v := q.Get("limite"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > a.opt.Config.PaginationMax {
			requeteInvalide(w, "limite hors bornes (1 a "+strconv.Itoa(a.opt.Config.PaginationMax)+")")
			return 0, 0, false
		}
		limite = n
	}
	if v := q.Get("decalage"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 || n > decalageMax {
			requeteInvalide(w, "decalage invalide")
			return 0, 0, false
		}
		decalage = n
	}
	return limite, decalage, true
}

// decalageMax : au-dela, aucune page n'a de sens, et un decalage geant fait scanner la table.
const decalageMax = 10_000_000

// sansDelaiEcriture retire le delai d'ecriture du serveur pour cette reponse (flux
// longs : segments, films).
func sansDelaiEcriture(w http.ResponseWriter) {
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
}
