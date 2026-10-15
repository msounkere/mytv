package live

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"mytv/internal/domain"
)

// refusFournisseur repond quand le fournisseur a refuse le COMPTE ou l'ADRESSE (US-25) :
// ce n'est pas une panne passagere, et l'interface doit pouvoir le dire a l'utilisateur
// (« abonnement expire ») au lieu d'un « fournisseur injoignable ». Meme corps
// {code, message} que le reste de l'API, statut 502 (jamais 401/403 : l'interface y
// lit « session expiree » et deconnecterait l'utilisateur). Rend false pour toute autre
// erreur, que l'appelant traite comme avant.
func (r *Relais) refusFournisseur(w http.ResponseWriter, err error) bool {
	var code, message string
	switch {
	case errors.Is(err, domain.ErrIdentifiants):
		code, message = "fournisseur_compte_invalide", "abonnement expire ou refuse par le fournisseur"
	case errors.Is(err, domain.ErrRefuseIP):
		code, message = "fournisseur_adresse_refusee", "adresse du serveur refusee par le fournisseur"
	default:
		return false
	}
	if r.opt.SignalerRefus != nil {
		r.opt.SignalerRefus(err)
	}
	corps := struct {
		Code       string `json:"code"`
		Message    string `json:"message"`
		Expiration string `json:"expiration,omitempty"`
	}{Code: code, Message: message}
	if code == "fournisseur_compte_invalide" && r.opt.Expiration != nil {
		if t, ok := r.opt.Expiration(); ok {
			corps.Expiration = t.UTC().Format(time.RFC3339)
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusBadGateway)
	_ = json.NewEncoder(w).Encode(corps)
	return true
}
