package occupation

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"mytv/internal/domain"
)

// CodeOccupe est le code des saturations locales qui ne relevent pas du compte (ffmpeg
// plein, file de details...). Les trois Classe sont reservees aux refus du compte.
const CodeOccupe = "occupe"

var messages = map[string]string{
	string(Ailleurs): "le compte IPTV est utilise sur un autre appareil",
	string(ParMytv):  "une autre session MyTV utilise la connexion du fournisseur",
	string(Occupe):   "connexion du fournisseur occupee, nouvelle tentative a l'echeance indiquee",
	CodeOccupe:       "service occupe, nouvelle tentative a l'echeance indiquee",
}

// EstOccupation dit si err est un refus d'occupation (407 apres patience, ou aucune place
// locale) que Repondre sait traduire.
func EstOccupation(err error) bool {
	return errors.Is(err, domain.ErrRefuseConnexions) || errors.Is(err, domain.ErrPlaceIndisponible)
}

// Repondre ecrit le 503 d'un refus d'occupation et rend vrai ; faux (rien d'ecrit) si err
// n'en est pas un. Le code (corps JSON ET en-tete X-Mytv-Erreur, que hls.js sait lire) vient
// de la classification portee par l'erreur ; sans elle, ErrPlaceIndisponible (aucune place
// chez les baux : une autre session de mytv) est ParMytv et un 407 non diagnostique Occupe.
// Retry-After croit a chaque refus consecutif de la meme (session, ressource).
//
// Ecrire cette reponse ne fait AUCUN appel au fournisseur.
func (s *Suivi) Repondre(w http.ResponseWriter, d Demande, err error) bool {
	if !EstOccupation(err) {
		return false
	}
	classe, ok := ClasseDe(err)
	if !ok {
		classe = Occupe
		if errors.Is(err, domain.ErrPlaceIndisponible) {
			classe = ParMytv
		}
	}
	s.RepondreCode(w, d, string(classe))
	return true
}

// RepondreCode ecrit un 503 avec ce code et le Retry-After croissant de la demande.
func (s *Suivi) RepondreCode(w http.ResponseWriter, d Demande, code string) {
	attente := s.RetryApres(d)
	if s != nil {
		s.inst.Compter("occupation_refus_total", 1, "code", code)
	}
	secondes := int((attente + time.Second - 1) / time.Second)
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Retry-After", strconv.Itoa(secondes))
	h.Set("X-Mytv-Erreur", code)
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{code, messages[code]})
}
