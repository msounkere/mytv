package api

// Diffusion vers un televiseur (US-40) : le lecteur demande un LIEN SIGNE a duree
// limitee, que l'appareil de diffusion (Chromecast, televiseur) va chercher SANS cookie.
//
// Le jeton est scelle par hls.Sceau (chiffre et authentifie, domaine « cast » : un jeton
// de segment ou d'image n'ouvre jamais ce domaine) et porte seulement l'identifiant de
// SESSION, la ressource autorisee (une chaine ou un contenu) et la date d'expiration :
// jamais de secret ni d'adresse du fournisseur. A chaque requete, la session doit encore
// exister (revocation = 401) ; la requete est ensuite servie par les gestionnaires
// existants COMME SI elle venait de cette session (meme bail, meme place chez le
// fournisseur), et les playlists emettent leurs segments sous le meme prefixe signe.

import (
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"mytv/internal/hls"
	"mytv/internal/live"
)

const (
	// Libelles de derivation du domaine « cast » : distincts de ceux des segments, des
	// images et de la conversion.
	libelleCastChiffrement = "mytv/cast/jeton/chiffrement/v1"
	libelleCastNonce       = "mytv/cast/jeton/nonce/v1"
	porteeCast             = "cast"
	castDureeParDefaut     = 12 * time.Hour
	// parametresMax borne la chaine de parametres recopiee dans le lien.
	parametresMax = 256
)

var (
	// reParametresCast : la chaine de parametres telle que recopiee (« cap=hevc,eac3&qualite=auto »).
	reParametresCast = regexp.MustCompile(`^[A-Za-z0-9_.,:=&-]*$`)
	// clesParametresCast : les seuls parametres de lecture qu'un lien peut porter.
	clesParametresCast = map[string]bool{"cap": true, "qualite": true, "forcer": true, "t": true}
	errJetonCast       = errors.New("api : jeton de diffusion invalide")
)

// chargeCast est ce que porte un jeton de diffusion.
type chargeCast struct {
	session   string
	ressource string // « live:<chaine> » ou « vod:<id> »
	expire    time.Time
}

func (c chargeCast) encoder() string {
	return c.session + "\x00" + c.ressource + "\x00" + strconv.FormatInt(c.expire.Unix(), 10)
}

func decoderChargeCast(s string) (chargeCast, error) {
	m := strings.Split(s, "\x00")
	if len(m) != 3 || m[0] == "" || m[1] == "" {
		return chargeCast{}, errJetonCast
	}
	ts, err := strconv.ParseInt(m[2], 10, 64)
	if err != nil {
		return chargeCast{}, errJetonCast
	}
	return chargeCast{session: m[0], ressource: m[1], expire: time.Unix(ts, 0).UTC()}, nil
}

func (a *API) declarerCast() {
	a.route("POST", "/cast/lien", Session, a.lienCast)
	// Niveau Lecture SANS cookie : le jeton du chemin designe la session. Enregistree sans
	// methode pour repondre 405 (et non 404) aux ecritures ; GET et HEAD seulement.
	a.routes = append(a.routes, Route{"GET", "/cast/{jeton}/{chemin}", Lecture})
	a.mux.HandleFunc(Prefixe+"/cast/{jeton}/{chemin...}", a.servirCast)
}

func (a *API) castDuree() time.Duration {
	if a.opt.CastDuree > 0 {
		return a.opt.CastDuree
	}
	return castDureeParDefaut
}

// ressourceCast traduit le champ `lecture` d'une demande de lien en ressource du jeton et en
// chemin relatif sous le prefixe signe. Rien d'autre qu'une chaine ou un contenu du schema.
func ressourceCast(lecture string) (ressource, chemin string, ok bool) {
	if fichier, ok := strings.CutPrefix(lecture, Prefixe+"/live/"); ok {
		chaine, ok := strings.CutSuffix(fichier, ".m3u8")
		if !ok || !reChaine.MatchString(chaine) {
			return "", "", false
		}
		return "live:" + chaine, "live/" + chaine + ".m3u8", true
	}
	if id, ok := strings.CutPrefix(lecture, Prefixe+"/vod/"); ok && reContenuID.MatchString(id) {
		return "vod:" + id, "vod/" + id, true
	}
	return "", "", false
}

// ressourceDuChemin rend la ressource que designe le reste du chemin d'une requete signee :
// la chaine pour sa playlist et ses segments (sous-playlists, cles et segments de conversion
// compris : tous sont emis sous /live/<chaine>/seg/), l'identifiant pour un contenu.
func ressourceDuChemin(chemin string) (string, bool) {
	if reste, ok := strings.CutPrefix(chemin, "live/"); ok {
		if chaine, ok := strings.CutSuffix(reste, ".m3u8"); ok && reChaine.MatchString(chaine) {
			return "live:" + chaine, true
		}
		chaine, seg, ok := strings.Cut(reste, "/seg/")
		if ok && reChaine.MatchString(chaine) && seg != "" && !strings.Contains(seg, "/") {
			return "live:" + chaine, true
		}
		return "", false
	}
	if id, ok := strings.CutPrefix(chemin, "vod/"); ok && reContenuID.MatchString(id) {
		return "vod:" + id, true
	}
	return "", false
}

// validerParametresCast : la chaine est recopiee TELLE QUELLE dans le lien, apres controle
// (parametres de lecture connus, valeurs sans caractere de structure d'URL).
func validerParametresCast(p string) bool {
	if p == "" {
		return true
	}
	if len(p) > parametresMax || !reParametresCast.MatchString(p) {
		return false
	}
	valeurs, err := url.ParseQuery(p)
	if err != nil {
		return false
	}
	for cle := range valeurs {
		if !clesParametresCast[cle] {
			return false
		}
	}
	return true
}

// lienCast : POST /cast/lien (session). Repond le lien signe et sa date d'expiration.
func (a *API) lienCast(w http.ResponseWriter, req *http.Request) {
	if a.cast == nil {
		erreur(w, http.StatusNotImplemented, "non_disponible", "diffusion non activee (secret de signature absent)")
		return
	}
	var in struct {
		Lecture    string `json:"lecture"`
		Parametres string `json:"parametres"`
	}
	if !lireJSON(w, req, &in) {
		return
	}
	ressource, chemin, ok := ressourceCast(in.Lecture)
	if !ok {
		requeteInvalide(w, "lecture doit etre /api/v1/live/<chaine>.m3u8 ou /api/v1/vod/<id>")
		return
	}
	if !validerParametresCast(in.Parametres) {
		requeteInvalide(w, "parametres invalides (cap, qualite, forcer, t)")
		return
	}
	sess := session(req)
	expire := a.maintenant().Add(a.castDuree()).Truncate(time.Second).UTC()
	jeton := a.cast.Sceller(porteeCast, chargeCast{session: sess.ID, ressource: ressource, expire: expire}.encoder())
	u := Prefixe + "/cast/" + jeton + "/" + chemin
	if in.Parametres != "" {
		u += "?" + in.Parametres
	}
	type_, _, _ := strings.Cut(ressource, ":")
	a.inst.Compter("cast_liens_total", 1, "type", type_)
	// Jamais le jeton : la session abregee, la ressource et l'expiration suffisent.
	a.log.Info("lien de diffusion emis", "session", abreger(sess.ID), "ressource", ressource, "expire", expire.Format(time.RFC3339))
	ecrireJSON(w, http.StatusOK, map[string]string{"url": u, "expire": expire.Format(time.RFC3339)})
}

func abreger(s string) string {
	if len(s) > 8 {
		return s[:8]
	}
	return s
}

// servirCast : GET/HEAD /cast/{jeton}/{chemin...}. Le jeton est ouvert et verifie
// (authenticite, expiration, ressource du chemin, session encore vivante), puis la requete
// est confiee aux gestionnaires existants sous le chemin normal, avec la session imposee
// dans le contexte et, pour le direct, le prefixe signe sous lequel emettre les segments.
func (a *API) servirCast(w http.ResponseWriter, req *http.Request) {
	if a.cast == nil {
		erreur(w, http.StatusNotImplemented, "non_disponible", "diffusion non activee (secret de signature absent)")
		return
	}
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		erreur(w, http.StatusMethodNotAllowed, "methode_refusee", "GET ou HEAD seulement")
		return
	}
	refus := func(message string) {
		erreur(w, http.StatusUnauthorized, "jeton_invalide", message)
	}
	jeton := req.PathValue("jeton")
	charge, err := a.cast.Ouvrir(porteeCast, jeton)
	if err != nil {
		refus("lien de diffusion invalide")
		return
	}
	c, err := decoderChargeCast(charge)
	if err != nil {
		refus("lien de diffusion invalide")
		return
	}
	if a.maintenant().After(c.expire) {
		refus("lien de diffusion expire")
		return
	}
	chemin := req.PathValue("chemin")
	ressource, ok := ressourceDuChemin(chemin)
	if !ok {
		erreur(w, http.StatusNotFound, "introuvable", "route inconnue")
		return
	}
	if ressource != c.ressource {
		refus("lien de diffusion emis pour une autre ressource")
		return
	}
	if _, err := a.opt.Auth.SessionParID(req.Context(), c.session); err != nil {
		refus("session terminee")
		return
	}
	ctx := a.opt.Auth.ImposerSession(req.Context(), c.session)
	if strings.HasPrefix(chemin, "live/") {
		ctx = live.AvecPrefixe(ctx, Prefixe+"/cast/"+jeton+"/live")
	}
	r2 := req.Clone(ctx)
	r2.URL.Path = Prefixe + "/" + chemin
	r2.URL.RawPath = ""
	a.mux.ServeHTTP(w, r2)
}

func nouveauSceauCast(secret []byte) (*hls.Sceau, error) {
	if len(secret) == 0 {
		return nil, nil // diffusion non activee : les routes repondent 501
	}
	return hls.NouveauSceau(secret, libelleCastChiffrement, libelleCastNonce)
}
