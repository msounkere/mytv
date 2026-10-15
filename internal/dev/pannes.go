package dev

import (
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"time"

	"mytv/internal/testutil/fauxiptv"
)

// cibles reproduit ce que le vrai fournisseur refuse ou ralentit : le direct
// (playlist, segments) et les films. L'API de catalogue n'est pas touchee, sinon
// une synchronisation en cours echouerait pour une raison sans rapport.
var cibles = []fauxiptv.Cible{fauxiptv.CiblePlaylist, fauxiptv.CibleSegment, fauxiptv.CibleFilm}

// appliquerBase remet les pannes a l'etat « latence seule ».
func (h *Harnais) appliquerBase() {
	h.faux.Effacer()
	h.programmerLatence()
}

func (h *Harnais) programmerLatence() {
	if h.latence <= 0 {
		return
	}
	for _, c := range cibles {
		h.faux.Programmer(fauxiptv.Panne{Cible: c, Latence: h.latence})
	}
}

// Panne programme, a partir de maintenant, des reponses `statut` pendant `duree`
// sur le direct et les films. Elle passe AVANT la latence de fond (la premiere
// panne qui correspond agit) et porte la meme latence.
func (h *Harnais) Panne(statut int, duree time.Duration) {
	// On reprogramme tout : les pannes de panne passent avant la latence de fond.
	h.faux.Effacer()
	for _, c := range cibles {
		h.faux.Programmer(fauxiptv.Panne{Cible: c, Statut: statut, Duree: duree, Latence: h.latence})
	}
	h.programmerLatence()
}

// Effacer retire les pannes (la latence de fond reste).
func (h *Harnais) Effacer() { h.appliquerBase() }

// demarrerControle ouvre l'API de pilotage, reservee a la boucle locale :
//
//	POST   /panne?statut=407&duree=30s   programme une panne
//	DELETE /panne                        l'efface
//	GET    /compteurs                    requetes recues par le faux fournisseur
func (h *Harnais) demarrerControle(adresse string) error {
	ln, err := net.Listen("tcp", adresse)
	if err != nil {
		return err
	}
	h.ControleURL = "http://" + ln.Addr().String()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /panne", func(w http.ResponseWriter, r *http.Request) {
		st, err1 := strconv.Atoi(r.URL.Query().Get("statut"))
		d, err2 := time.ParseDuration(r.URL.Query().Get("duree"))
		if err1 != nil || err2 != nil || st < 400 || st > 599 || d <= 0 {
			http.Error(w, "statut (400-599) et duree (ex. 30s) requis", http.StatusBadRequest)
			return
		}
		h.Panne(st, d)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /panne", func(w http.ResponseWriter, _ *http.Request) {
		h.Effacer()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /compteurs", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(h.faux.Compteurs())
	})
	h.ctrl = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = h.ctrl.Serve(ln) }()
	return nil
}
