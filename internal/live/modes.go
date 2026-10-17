package live

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"mytv/internal/occupation"
	"mytv/internal/remux"
)

func (r *Relais) routePlaylist(w http.ResponseWriter, req *http.Request) {
	chaine, ok := strings.CutSuffix(req.PathValue("fichier"), ".m3u8")
	if !ok || !reChaine.MatchString(chaine) {
		http.Error(w, "chaine invalide", http.StatusBadRequest)
		return
	}
	// ?forcer= est verifie avant de reserver quoi que ce soit chez le fournisseur.
	forcer := Mode(req.URL.Query().Get("forcer"))
	if forcer != "" && forcer != ModeAudio && forcer != ModeVideo {
		// « direct » et « refus » ne se forcent pas : le premier ne repare aucune lecture en
		// echec, le second serait une panne imposee.
		reponseJSON(w, http.StatusBadRequest, "forcer_invalide", "forcer accepte « audio » ou « video »", "")
		return
	}
	caps := LireCapacites(req)
	bail, ok := r.bail(w, req, chaine)
	if !ok {
		return
	}
	debut := time.Now()
	ctx := req.Context()
	// Avant tout contact avec le fournisseur : il n'a qu'une connexion.
	r.zapper(ctx, chaine)

	var (
		pl      playlist
		secours bool
		dejaPl  bool
		v       verdict
		connu   bool
		mode    = ModeDirect
		raison  string
	)
	obtenirPlaylist := func() bool {
		if dejaPl {
			return true
		}
		var err error
		if pl, secours, err = r.playlistDe(ctx, chaine, bail.Renouveler); err != nil {
			r.repondreErreurAmont(w, req, chaine, err)
			return false
		}
		dejaPl = true
		return true
	}
	switch {
	case forcer != "":
		mode, raison = forcer, "mode impose par le client"
		if k, ok := r.verdictConnu(chaine); ok {
			v, connu = r.avecDecision(k, caps), true
			// Imposer ne leve pas l'interdiction de reencoder : ni le reglage, ni une 4K connue.
			if mode == ModeVideo && v.d.Mode == ModeRefus {
				mode, raison = ModeRefus, v.d.Raison
			}
		} else if mode == ModeVideo && !r.opt.Live.ReencodageVideo {
			mode, raison = ModeRefus, "le reencodage video est desactive sur ce serveur"
		}
	case r.sondeActive():
		k, ok := r.verdictConnu(chaine)
		if ok && (!r.verdictPerime(k) || r.transcodageActifDe(chaine)) {
			// Chaine deja sondee (ou convertie en ce moment, ffmpeg tient la connexion) : aucun delai ajoute.
			v, connu = r.avecDecision(k, caps), true
		} else {
			// Inconnue ou a revoir : la playlist d'abord, la sonde en a besoin. Un verdict
			// perime d'une chaine en direct est rendu tout de suite et resonde derriere.
			if !obtenirPlaylist() {
				return
			}
			v = r.verdictPour(ctx, chaine, pl.sonde, caps, bail.Renouveler)
			connu = !v.echec
		}
		mode, raison = v.d.Mode, v.d.Raison
	}
	if (mode == ModeAudio || mode == ModeVideo) && !r.conversionActive() {
		mode, raison = ModeDirect, "conversion non configuree"
	}
	r.inst.Compter("live_modes_total", 1, "mode", string(mode))
	r.journal.Info("mode du direct", "chaine", chaine, "mode", string(mode), "capacites", caps.String(), "raison", raison)
	entetesMode(w, mode, v, connu)

	switch mode {
	case ModeRefus:
		code := v.d.Code
		if code == "" {
			code = "format_non_lisible"
		}
		reponseJSON(w, http.StatusUnsupportedMediaType, code, raison, string(ModeRefus))
		return
	case ModeAudio, ModeVideo:
		t, err := r.assurerTranscodage(ctx, chaine, sessionDe(req), mode, bail)
		if err != nil {
			r.repondreErreurTranscodage(w, req, chaine, mode, err)
			return
		}
		corps, err := r.playlistLocale(t)
		if err != nil {
			r.repondreErreurTranscodage(w, req, chaine, mode, ErrTranscodageImpossible)
			return
		}
		bail.Renouveler()
		r.toucher(t)
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Relais-Ms", strconv.FormatInt(time.Since(debut).Milliseconds(), 10))
		io.WriteString(w, corps)
		return
	}

	// Direct. Un transcodage de CETTE chaine qui tourne encore (le verdict a change) s'arrete.
	r.arreterTranscodageDe(sessionDe(req), chaine)
	if !obtenirPlaylist() {
		return
	}
	r.st.incrementer(&r.st.servies)
	if !secours {
		r.inst.Compter("live_playlists_total", 1, "resultat", "ok")
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	w.Header().Set("Cache-Control", "no-store")
	if secours {
		w.Header().Set("X-Relais", "secours")
	}
	w.Header().Set("X-Relais-Ms", strconv.FormatInt(time.Since(debut).Milliseconds(), 10))
	io.WriteString(w, pl.corps)
}

func (r *Relais) verdictPerime(v verdict) bool { return !r.opt.Horloge().Before(v.jusqu) }

// entetesMode : X-Mytv-Mode toujours ; codecs et hauteur quand une sonde les a donnes (l'interface
// s'en sert pour son indicateur). Ils ne viennent que d'un nom de codec filtre.
func entetesMode(w http.ResponseWriter, mode Mode, v verdict, connu bool) {
	h := w.Header()
	h.Set("X-Mytv-Mode", string(mode))
	if !connu {
		return
	}
	video, audio := nomCodecEntete(v.sonde.CodecVideo), nomCodecEntete(v.sonde.CodecAudio)
	h.Set("X-Mytv-Codec", video+"+"+audio)
	if v.sonde.Hauteur > 0 {
		h.Set("X-Mytv-Hauteur", strconv.Itoa(v.sonde.Hauteur))
	}
}

func nomCodecEntete(c string) string {
	c = strings.ToLower(c)
	if c == "" || !nomCodecSur.MatchString(c) {
		return "-"
	}
	return c
}

// reponseJSON : meme corps {code, message} que le reste de l'API ; le code est aussi dans
// X-Mytv-Erreur, que hls.js peut lire quand il ne rend pas le corps d'une erreur.
func reponseJSON(w http.ResponseWriter, statut int, code, message, mode string) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Mytv-Erreur", code)
	if mode != "" {
		h.Set("X-Mytv-Mode", mode)
	}
	w.WriteHeader(statut)
	_ = json.NewEncoder(w).Encode(struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}{code, message})
}

// repondreErreurAmont : l'echec d'une playlist, comme avant le lot 5.
func (r *Relais) repondreErreurAmont(w http.ResponseWriter, req *http.Request, chaine string, err error) {
	if req.Context().Err() != nil {
		return
	}
	r.inst.Compter("live_playlists_total", 1, "resultat", "erreur")
	r.journal.Error("playlist indisponible", "chaine", chaine, "erreur", remux.Assainir(err.Error()))
	if r.opt.Occupation.Repondre(w, r.demande(req.Context(), chaine), err) {
		return
	}
	if r.refusFournisseur(w, err) {
		return
	}
	http.Error(w, "fournisseur injoignable", http.StatusBadGateway)
}

// repondreErreurTranscodage : 503 + Retry-After quand c'est de la patience (407, ffmpeg
// occupe, lancement abandonne par un zapping), 502 transcodage_impossible quand ffmpeg ne produit rien.
func (r *Relais) repondreErreurTranscodage(w http.ResponseWriter, req *http.Request, chaine string, mode Mode, err error) {
	if req.Context().Err() != nil {
		return
	}
	r.inst.Compter("live_transcodage_echecs_total", 1, "mode", string(mode))
	r.journal.Warn("transcodage indisponible", "chaine", chaine, "mode", string(mode), "erreur", remux.Assainir(err.Error()))
	switch {
	case occupation.EstOccupation(err):
		r.opt.Occupation.Repondre(w, r.demande(req.Context(), chaine), err)
	case errors.Is(err, remux.ErrSaturation), errors.Is(err, context.Canceled):
		// Saturation locale (ffmpeg plein, lancement abandonne) : pas un refus du compte.
		r.opt.Occupation.RepondreCode(w, r.demande(req.Context(), chaine), occupation.CodeOccupe)
	case errors.Is(err, ErrTranscodageImpossible):
		reponseJSON(w, http.StatusBadGateway, "transcodage_impossible", "conversion du flux impossible", string(mode))
	default:
		if r.refusFournisseur(w, err) {
			return
		}
		http.Error(w, "fournisseur injoignable", http.StatusBadGateway)
	}
}
