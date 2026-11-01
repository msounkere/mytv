// Package api expose l'API HTTP v1 de mytv (contrat : docs/openapi.yaml).
//
// Principes :
//   - REFUS PAR DEFAUT : toute route est declaree avec un niveau d'acces ; ce qui n'est
//     pas declare (chemin inconnu, methode inconnue) passe par l'authentification AVANT
//     de repondre 404, pour ne rien reveler a un inconnu ;
//   - le contrat OpenAPI et les routes reelles sont compares par un test (conformite) ;
//   - une dependance absente (EPG, VOD, profil, telemetrie) fait repondre 501
//     « non_disponible » au lieu de supprimer la route : le contrat reste exact.
package api

import (
	"context"
	"errors"
	"log/slog"
	"mytv/internal/occupation"
	"net/http"
	"regexp"
	"sort"
	"sync"
	"time"

	"mytv/internal/config"
	"mytv/internal/domain"
	"mytv/internal/hls"
	"mytv/internal/images"
)

var reChaine = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Prefixe est la racine de toutes les routes.
const Prefixe = "/api/v1"

// Acces est le niveau d'authentification exige par une route.
type Acces int

const (
	Public  Acces = iota // aucune : connexion et sante seulement
	Session              // cookie de session
	Lecture              // cookie, ou compte technique local (lecteurs sans cookie)
	Admin                // cookie d'un administrateur
)

// Route est une route declaree, telle que comparee au contrat.
type Route struct {
	Methode string
	Chemin  string // sans Prefixe, ex. « /chaines/{id} »
	Acces   Acces
}

// Authentification est ce que l'API attend du paquet comptes (*comptes.Service).
type Authentification interface {
	Proteger(http.Handler) http.Handler
	ProtegerAdmin(http.Handler) http.Handler
	Connexion(corpsMax int64) http.Handler
	Deconnexion() http.Handler
	AuthentifierLecture(*http.Request) (string, bool)
	// SessionParID retrouve une session vivante par son identifiant (lien de diffusion) ;
	// ImposerSession rend le contexte ou AuthentifierLecture la reconnaitra (GET/HEAD).
	SessionParID(ctx context.Context, id string) (*domain.Session, error)
	ImposerSession(ctx context.Context, id string) context.Context
}

// Images est le proxy d'images (internal/images).
type Images interface {
	// Reecrire donne l'URL servie par ce service pour une URL d'origine ("" reste "").
	Reecrire(url string) string
	// Empreinte change quand les jetons emis changent (secret, fournisseurs) : elle
	// entre dans les ETag du catalogue, sinon un 304 ferait garder d'anciens jetons.
	Empreinte() string
	Obtenir(ctx context.Context, jeton string) (*images.Image, error)
}

// FinDeLecture est implemente par le relais du direct : la session a fini de lire
// `chaine` ; si elle la lit encore, sa place chez le fournisseur est rendue.
type FinDeLecture interface {
	FinDeLecture(session, chaine string)
}

// Superviseur est ce que l'API demande a la supervision du compte fournisseur (US-25) :
// la date d'expiration connue (ajoutee aux erreurs fournisseur_compte_invalide) et le
// signalement d'un refus vu en lecture.
type Superviseur interface {
	Expiration(fournisseurID string) (time.Time, bool)
	SignalerRefus(fournisseurID string, err error)
}

// Options : toutes les dependances. Seuls Config, Stockage, Comptes et Auth sont
// obligatoires.
type Options struct {
	Config config.API
	Profil config.Profil
	// FournisseurLive : le fournisseur du direct (relais mono-fournisseur). Vide = tous.
	FournisseurLive string
	Fournisseurs    []string // identifiants, dans l'ordre de priorite (validite des ETag et fusion)

	Stockage domain.Stockage
	Video    domain.StockageVideo
	VOD      map[string]domain.FournisseurVOD // saisons, par fournisseur
	// Details : synopsis, genre, casting... a la demande (US-28). Facultatif.
	Details Details
	Comptes domain.Comptes
	Auth    Authentification
	EPG     domain.EPGStockage
	Etat    domain.EtatProfil
	Lecture domain.ServiceVOD
	// AVoir : liste « A voir » par profil (US-42). Absent : les routes repondent 501.
	AVoir domain.ListeAVoir
	// Preparation : moteur de preparation d'avance (etats, administration). Facultatif.
	Preparation Preparation
	// Sonde : sonde de disponibilite d'un film (GET /vod/{id}/disponibilite). Facultatif.
	Sonde Sonde
	// Export sert /catalogue/version et /catalogue/export (stockage SQLite).
	Export domain.ExportCatalogue
	// FinLive rend la place du fournisseur quand l'interface ferme le lecteur
	// (POST /live/{chaine}/fin). Absent : la route repond 204 sans rien faire.
	FinLive FinDeLecture

	// Live est le relais du direct, deja protege par son propre authentificateur
	// (cookie ou compte local, /etat reserve aux administrateurs).
	Live http.Handler
	// Admin : gestionnaires reserves aux administrateurs, par nom de route
	// (« metriques » = GET /metriques, « diagnostic » = GET /diagnostic/evenements,
	// « fournisseurs » = GET /admin/fournisseurs, « sante » = GET /admin/sante).
	Admin map[string]http.Handler
	// Evenements recoit POST /diagnostic/evenements (tout utilisateur connecte :
	// c'est l'interface qui envoie ce qu'elle subit). telemetrie.Recevoir.
	Evenements http.Handler

	// Images reecrit les URL de logos/affiches et sert GET /image/{jeton} (US-23).
	// Absent : les URL sont renvoyees telles quelles et la route repond 501.
	Images Images

	// Secret signe les liens de diffusion (serveur.secret_signature, le meme que les
	// segments et les images, domaine distinct). Absent : /cast/* repond 501.
	Secret []byte
	// CastDuree : validite d'un lien de diffusion (comptes.cast_duree) ; 12 h si nulle.
	CastDuree time.Duration
	// Instrumentation publie les mesures de l'API (nil = aucune).
	Instrumentation domain.Instrumentation

	// Supervision : facultatif (nil = erreurs sans date, rien n'est signale).
	Supervision Superviseur
	// Occupation : facultatif. Sans lui, les 503 d'occupation d'un film gardent le code
	// compte_occupe et un Retry-After constant.
	Occupation *occupation.Suivi

	Journal *slog.Logger
	Horloge func() time.Time
}

type API struct {
	opt    Options
	log    *slog.Logger
	routes []Route
	mux    *http.ServeMux
	pubs   map[string]bool // fournisseurs connus
	cast   *hls.Sceau      // jetons des liens de diffusion (nil : non active)
	inst   domain.Instrumentation

	// logos : repli de logos du guide, garde en memoire (voir logosEPG).
	logos struct {
		sync.Mutex
		cle       string
		valide    bool
		par       map[domain.ContenuID]string
		empreinte string
	}
}

// Nouveau construit l'API et enregistre toutes ses routes.
func Nouveau(opt Options) (*API, error) {
	var manques []error
	exige := func(ok bool, msg string) {
		if !ok {
			manques = append(manques, errors.New(msg))
		}
	}
	exige(opt.Stockage != nil, "api : stockage absent")
	exige(opt.Comptes != nil, "api : comptes absents")
	exige(opt.Auth != nil, "api : authentification absente")
	exige(len(opt.Fournisseurs) > 0, "api : aucun fournisseur declare")
	exige(opt.Config.PaginationMax >= 1 && opt.Config.PaginationDefaut >= 1 && opt.Config.PaginationDefaut <= opt.Config.PaginationMax,
		"api : pagination_defaut doit etre entre 1 et pagination_max")
	exige(opt.Config.CorpsMax > 0, "api : corps_max doit etre > 0")
	exige(opt.Config.GrilleFenetreMax > 0, "api : grille_fenetre_max doit etre > 0")
	if err := errors.Join(manques...); err != nil {
		return nil, err
	}
	if opt.Journal == nil {
		opt.Journal = slog.Default()
	}
	if opt.Horloge == nil {
		opt.Horloge = time.Now
	}
	if opt.Instrumentation == nil {
		opt.Instrumentation = domain.SansInstrumentation{}
	}
	cast, err := nouveauSceauCast(opt.Secret)
	if err != nil {
		return nil, err
	}
	a := &API{opt: opt, log: opt.Journal, mux: http.NewServeMux(), pubs: map[string]bool{}, cast: cast, inst: opt.Instrumentation}
	for _, f := range opt.Fournisseurs {
		a.pubs[f] = true
	}
	a.declarer()
	return a, nil
}

// Routes liste les routes declarees (utilise par le test de conformite).
func (a *API) Routes() []Route {
	out := append([]Route(nil), a.routes...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Chemin != out[j].Chemin {
			return out[i].Chemin < out[j].Chemin
		}
		return out[i].Methode < out[j].Methode
	})
	return out
}

// route enregistre une route avec son niveau d'acces.
func (a *API) route(methode, chemin string, acces Acces, h http.HandlerFunc) {
	a.routes = append(a.routes, Route{methode, chemin, acces})
	var handler http.Handler = h
	switch acces {
	case Session:
		handler = a.opt.Auth.Proteger(handler)
	case Admin:
		handler = a.opt.Auth.ProtegerAdmin(handler)
	}
	a.mux.Handle(methode+" "+Prefixe+chemin, handler)
}

// routeLecture enregistre une route de lecture (flux) : cookie OU compte local, et
// pas de delai d'ecriture (un film dure des heures).
func (a *API) routeLecture(methode, chemin string, h func(w http.ResponseWriter, req *http.Request, session string)) {
	a.routes = append(a.routes, Route{methode, chemin, Lecture})
	a.mux.HandleFunc(methode+" "+Prefixe+chemin, func(w http.ResponseWriter, req *http.Request) {
		session, ok := a.opt.Auth.AuthentifierLecture(req)
		if !ok {
			erreur(w, http.StatusUnauthorized, "non_authentifie", "authentification requise")
			return
		}
		sansDelaiEcriture(w)
		h(w, req, session)
	})
}

func (a *API) declarer() {
	a.route("GET", "/sante", Public, a.sante)
	a.route("POST", "/live/{chaine}/fin", Session, a.finLive)
	a.route("GET", "/image/{jeton}", Session, a.imageServie)
	a.declarerCompte()
	a.declarerCatalogue()
	a.declarerLive()
	a.declarerVOD()
	a.declarerEPG()
	a.declarerProfil()
	a.declarerAVoir()
	a.declarerAdmin()
	a.declarerCast()
	// Tout le reste : authentification d'abord, 404 ensuite.
	a.mux.Handle("/", a.opt.Auth.Proteger(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		erreur(w, http.StatusNotFound, "introuvable", "route inconnue")
	})))
}

func (a *API) sante(w http.ResponseWriter, _ *http.Request) {
	ecrireJSON(w, http.StatusOK, map[string]string{"statut": "ok"})
}

// declarerLive monte le relais du direct. Ses routes sont decrites ici pour le
// contrat ; le relais fait lui-meme l'authentification (compte local compris).
func (a *API) declarerLive() {
	a.routes = append(a.routes,
		Route{"GET", "/live/{fichier}", Lecture},
		Route{"GET", "/live/{chaine}/seg/{jeton}", Lecture},
		Route{"GET", "/live/etat", Admin},
	)
	var h http.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		erreur(w, http.StatusNotImplemented, "non_disponible", "direct non active")
	})
	if a.opt.Live != nil {
		h = a.opt.Live
	}
	a.mux.Handle(Prefixe+"/live/", http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		sansDelaiEcriture(w)
		h.ServeHTTP(w, req)
	}))
}

// finLive : l'interface a ferme le lecteur. Idempotent, jamais 503. Declaree ici
// (et non dans le relais) car elle exige la session : le compte technique local
// ne fait que lire.
func (a *API) finLive(w http.ResponseWriter, req *http.Request) {
	chaine := req.PathValue("chaine")
	if !reChaine.MatchString(chaine) {
		requeteInvalide(w, "chaine invalide")
		return
	}
	if a.opt.FinLive != nil {
		a.opt.FinLive.FinDeLecture(session(req).ID, chaine)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) declarerAdmin() {
	for _, r := range []struct{ nom, chemin string }{
		{"metriques", "/metriques"},
		{"diagnostic", "/diagnostic/evenements"},
		{"fournisseurs", "/admin/fournisseurs"}, // etat du compte fournisseur (US-25)
		{"sante", "/admin/sante"},               // sante approfondie (US-25)
	} {
		h := a.opt.Admin[r.nom]
		a.route("GET", r.chemin, Admin, func(w http.ResponseWriter, req *http.Request) {
			if h == nil {
				erreur(w, http.StatusNotImplemented, "non_disponible", "fonction non activee")
				return
			}
			h.ServeHTTP(w, req)
		})
	}
	a.route("POST", "/diagnostic/evenements", Session, func(w http.ResponseWriter, req *http.Request) {
		if a.opt.Evenements == nil {
			erreur(w, http.StatusNotImplemented, "non_disponible", "fonction non activee")
			return
		}
		a.opt.Evenements.ServeHTTP(w, req)
	})
}

// Handler rend l'API complete, avec ses protections transversales.
func (a *API) Handler() http.Handler {
	var h http.Handler = a.mux
	h = a.limites(h)
	h = a.cors(h)
	h = enteteSecurite(h)
	return a.recuperer(h)
}

func (a *API) recuperer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				a.log.Error("panique dans un gestionnaire", "route", req.Method+" "+req.URL.Path, "valeur", v)
				erreur(w, http.StatusInternalServerError, "erreur_interne", "erreur interne")
			}
		}()
		next.ServeHTTP(w, req)
	})
}

func contexteAnnule(ctx context.Context) bool { return ctx.Err() != nil }

func (a *API) maintenant() time.Time { return a.opt.Horloge() }

func estSur(m string) bool {
	return m == http.MethodGet || m == http.MethodHead || m == http.MethodOptions
}
