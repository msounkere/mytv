// Package fauxiptv simule un fournisseur IPTV Xtream pour les tests : API JSON
// (catalogue), playlists HLS a fenetre glissante et segments TS factices.
//
// POURQUOI un faux plutot que des doublures par test : les defauts qui nous ont
// coute cher (hash de chemin different a chaque playlist, 407 apres un zapping,
// coupure en milieu de segment) sont des comportements du FOURNISSEUR. Les
// reproduire au meme endroit pour tous les modules evite que chacun les
// reinvente a sa facon, souvent trop gentiment.
//
// Le serveur n'ecoute que sur 127.0.0.1 (httptest) : aucun trafic sortant.
package fauxiptv

import (
	"bytes"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Options regle le faux fournisseur. Les zeros donnent des valeurs sensees.
type Options struct {
	Utilisateur string // defaut « u »
	MotDePasse  string // defaut « p »

	// Catalogue : reponses brutes de l'API, par action Xtream (« get_live_streams »...).
	// Ce sont des valeurs JSON quelconques (souvent []map[string]any) afin de pouvoir
	// reproduire les bizarreries du vrai fournisseur (nombres en chaines, null...).
	// Une action absente repond une liste vide.
	Catalogue map[string]any

	// InfosFilm / InfosSerie : reponses brutes de get_vod_info (cle = vod_id) et de
	// get_series_info (cle = series_id), JSON quelconque pour reproduire les cas
	// tordus (info en [], champs en chaines, backdrop_path tableau ou chaine...).
	// Un identifiant absent repond `[]`, comme le vrai fournisseur pour un film sans fiche.
	InfosFilm  map[string]any
	InfosSerie map[string]any
	// InfoFilmPar / InfoSeriePar : generateurs consultes pour un identifiant absent des
	// tables ci-dessus (harnais de developpement, gros catalogues). Rendre ok=faux
	// laisse la reponse par defaut. Une valeur StatutHTTP repond ce statut.
	InfoFilmPar  func(id string) (v any, ok bool)
	InfoSeriePar func(id string) (v any, ok bool)

	TailleSegment int // octets d'un segment TS, defaut 4096
	TailleFilm    int // octets d'un film ou d'un episode, defaut TailleSegment
	// ContenuSegment remplace les octets factices d'un segment (nom « <chaine>_<n>.ts »)
	// par de vrais MPEG-TS generes par le test ; ok=faux garde le contenu factice.
	ContenuSegment func(nom string) (corps []byte, ok bool)
	DureeSegment   time.Duration // valeur annoncee dans #EXTINF, defaut 10 s
	FenetreSegment int           // segments par playlist, defaut 5
	PremiereSeq    int           // numero du premier segment de la premiere playlist, defaut 150
	// AvanceParPlaylist : de combien de segments la fenetre glisse a chaque
	// playlist servie pour une chaine (defaut 1). Deterministe, donc sans horloge.
	AvanceParPlaylist int

	// RedirigerPlaylist : la playlist repond d'abord 302 vers un autre « noeud »,
	// comme le vrai fournisseur au premier appel.
	RedirigerPlaylist bool
	// RedirigerFilms : un film (ou episode) repond d'abord 302 vers un autre « noeud », comme
	// le vrai fournisseur. Compteurs.RedirectionsFilms compte ces sauts : un client qui retient
	// l'adresse finale n'en fait qu'un par lecture au lieu d'un par requete.
	RedirigerFilms bool

	// UserInfo : objet « user_info » servi par player_api.php sans action (statut du
	// compte, exp_date, max_connections...). Nil = compte actif, une connexion, et
	// active_cons COHERENT avec l'etat du faux : 1 quand un occupant externe (OccuperPar)
	// ou la regle « une seule connexion » tient la place, 0 sinon. Une valeur explicite
	// est servie telle quelle (le fournisseur reel, lui, annonce toujours 0).
	UserInfo map[string]any

	// XMLTV : guide servi par /xmltv.php (identifiants requis). Vide = 404.
	XMLTV []byte

	// Ecoute : adresse d'ecoute, TOUJOURS sur la boucle locale (defaut 127.0.0.1:0,
	// port ephemere). Une autre adresse que 127.0.0.1/::1 fait paniquer Nouveau :
	// ce faux ne doit jamais etre joignable depuis l'exterieur.
	Ecoute string

	// Horloge sert aux durees de panne et au mode une seule connexion (defaut time.Now).
	Horloge func() time.Time
}

// StatutHTTP, rendu par un generateur d'infos, fait repondre ce statut (404...).
type StatutHTTP int

// Cible dit sur quelles requetes une panne s'applique.
type Cible string

const (
	CibleAPI      Cible = "api"
	CiblePlaylist Cible = "playlist"
	CibleSegment  Cible = "segment"
	CibleFilm     Cible = "film"
	// CibleDetails : get_vod_info et get_series_info seulement (US-28). Une panne
	// CibleAPI les touche aussi (ce sont des appels d'API).
	CibleDetails Cible = "details"
	CibleTout    Cible = "tout"
)

// Panne est un defaut programme. Elle s'applique aux requetes de sa Cible tant
// qu'il reste des Fois (0 = sans limite de nombre) ET que Duree n'est pas ecoulee
// depuis la programmation (0 = sans limite de duree). Une panne epuisee disparait.
type Panne struct {
	Cible Cible
	Fois  int
	Duree time.Duration

	Latence time.Duration // attente avant de repondre (interrompue par l'annulation du client)
	Statut  int           // si non nul : repond ce statut sans corps utile (407, 458, 502, 405...)
	// Coupure : annonce le corps complet puis coupe la connexion au milieu
	// (segments, films). Le client voit un EOF inattendu.
	Coupure bool
	// CorpsVide : repond 200 avec un corps vide.
	CorpsVide bool
	// IgnorerRange : repond 200 et le fichier ENTIER a une requete avec Range (un
	// fournisseur ou un cache mal configure) ; un client qui reprend une lecture
	// doit le refuser plutot que de recoller des octets du mauvais endroit.
	IgnorerRange bool
}

// Compteurs : nombre de requetes recues, par nature.
type Compteurs struct {
	API               int
	Playlists         int
	Segments          int
	Films             int
	RedirectionsFilms int // 302 renvoyes pour un film (option RedirigerFilms)
	Refus407          int // 407 renvoyes, quelle qu'en soit la cause
	Pannes            int // reponses alterees par une panne programmee
	// PlaylistsParChaine et SegmentsParChaine : par numero de chaine.
	PlaylistsParChaine map[string]int
	SegmentsParChaine  map[string]int
	// ParAction : appels d'API par action (« get_live_streams »...).
	ParAction map[string]int
	// DetailsPic : plus grand nombre d'appels get_vod_info/get_series_info EN COURS
	// au meme instant (borne de concurrence du client). DetailsParID : appels par
	// « vod:<id> » ou « serie:<id> ».
	DetailsPic   int
	DetailsParID map[string]int
}

// Trace est une requete recue (sans le mot de passe dans le chemin).
type Trace struct {
	Methode string
	Chemin  string
	Entetes http.Header
}

type panneActive struct {
	Panne
	restantes int
	debut     time.Time
}

// Faux est le serveur. Utiliser Nouveau.
type Faux struct {
	serveur *httptest.Server
	opt     Options

	mu        sync.Mutex
	pannes    []*panneActive
	cpt       Compteurs
	traces    []Trace
	fenetre   map[string]int // chaine -> numero du prochain segment de tete
	nplaylist int            // sert a fabriquer des hash distincts

	detailsEnCours int // appels get_vod_info / get_series_info en cours (pic dans Compteurs)

	externeJusqua time.Time // occupant externe : tient la place jusqu'a cet instant (horloge du faux)

	unique       bool
	uniqueDuree  time.Duration
	uniqueRes    string
	uniqueVu     time.Time
	films        map[string][]byte // contenus deja fabriques
	filmsEnCours int               // lectures de films dont le gestionnaire n'a pas rendu la main

	// Connexions TCP (cote serveur) : ouvertes maintenant et ouvertes depuis le debut.
	connOuvertes map[net.Conn]struct{}
	connTotal    int
}

// Nouveau demarre le faux fournisseur. Il s'arrete tout seul en fin de test
// (t.Cleanup) ; t peut etre un *testing.T ou un *testing.B.
func Nouveau(t interface{ Cleanup(func()) }, opt Options) *Faux {
	if opt.Utilisateur == "" {
		opt.Utilisateur = "u"
	}
	if opt.MotDePasse == "" {
		opt.MotDePasse = "p"
	}
	if opt.TailleSegment <= 0 {
		opt.TailleSegment = 4096
	}
	if opt.DureeSegment <= 0 {
		opt.DureeSegment = 10 * time.Second
	}
	if opt.FenetreSegment <= 0 {
		opt.FenetreSegment = 5
	}
	if opt.PremiereSeq == 0 {
		opt.PremiereSeq = 150
	}
	if opt.AvanceParPlaylist <= 0 {
		opt.AvanceParPlaylist = 1
	}
	if opt.Horloge == nil {
		opt.Horloge = time.Now
	}
	f := &Faux{opt: opt, fenetre: map[string]int{}, connOuvertes: map[net.Conn]struct{}{}}
	f.cpt = f.cptVide()

	mux := http.NewServeMux()
	mux.HandleFunc("/player_api.php", f.api)
	mux.HandleFunc("/xmltv.php", f.xmltv)
	mux.HandleFunc("/img/{fichier}", imageFactice)
	mux.HandleFunc("/live/{u}/{p}/{fichier}", f.playlist)
	mux.HandleFunc("/noeud/live/{u}/{p}/{fichier}", f.playlist) // cible des redirections
	mux.HandleFunc("/hls/{hash}/{fichier}", f.segment)
	mux.HandleFunc("/movie/{u}/{p}/{fichier}", f.film)
	mux.HandleFunc("/series/{u}/{p}/{fichier}", f.film)      // episodes : meme comportement que les films
	mux.HandleFunc("/noeud/movie/{u}/{p}/{fichier}", f.film) // cibles des redirections
	mux.HandleFunc("/noeud/series/{u}/{p}/{fichier}", f.film)
	f.serveur = httptest.NewUnstartedServer(f.enveloppe(mux))
	// POURQUOI compter les connexions : le fournisseur reel garde une place par
	// connexion TCP ouverte ; un client qui laisse trainer ses connexions (keep-alive)
	// la lui fait garder. Les tests doivent pouvoir verifier qu'il n'en reste aucune.
	f.serveur.Config.ConnState = func(c net.Conn, e http.ConnState) {
		f.mu.Lock()
		defer f.mu.Unlock()
		switch e {
		case http.StateNew:
			f.connOuvertes[c] = struct{}{}
			f.connTotal++
		case http.StateClosed, http.StateHijacked:
			delete(f.connOuvertes, c)
		}
	}
	if opt.Ecoute != "" {
		h, _, err := net.SplitHostPort(opt.Ecoute)
		if ip := net.ParseIP(h); err != nil || ip == nil || !ip.IsLoopback() {
			panic("fauxiptv : Options.Ecoute doit etre une adresse de boucle locale (127.0.0.1:port)")
		}
		ln, err := net.Listen("tcp", opt.Ecoute)
		if err != nil {
			panic("fauxiptv : ecoute impossible : " + err.Error())
		}
		f.serveur.Listener.Close()
		f.serveur.Listener = ln
	}
	f.serveur.Start()
	t.Cleanup(f.serveur.Close)
	return f
}

func (f *Faux) cptVide() Compteurs {
	return Compteurs{
		PlaylistsParChaine: map[string]int{},
		SegmentsParChaine:  map[string]int{},
		ParAction:          map[string]int{},
		DetailsParID:       map[string]int{},
	}
}

// DefinirInfoFilm / DefinirInfoSerie changent la reponse de get_vod_info /
// get_series_info d'un identifiant (nil = revient a `[]`).
func (f *Faux) DefinirInfoFilm(id string, v any)  { f.definirInfo(&f.opt.InfosFilm, id, v) }
func (f *Faux) DefinirInfoSerie(id string, v any) { f.definirInfo(&f.opt.InfosSerie, id, v) }

func (f *Faux) definirInfo(m *map[string]any, id string, v any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if *m == nil {
		*m = map[string]any{}
	}
	if v == nil {
		delete(*m, id)
		return
	}
	(*m)[id] = v
}

// DefinirGenerateursInfos installe les generateurs d'infos apres le demarrage (leur
// adresse de base n'est connue qu'une fois le faux lance).
func (f *Faux) DefinirGenerateursInfos(film, serie func(id string) (any, bool)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opt.InfoFilmPar, f.opt.InfoSeriePar = film, serie
}

// DefinirCatalogue remplace le catalogue apres le demarrage (utile quand le
// catalogue doit contenir l'adresse du faux, connue seulement une fois lance).
func (f *Faux) DefinirCatalogue(c map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opt.Catalogue = c
}

// DefinirUserInfo remplace l'objet « user_info » apres le demarrage (nil = valeur par defaut).
func (f *Faux) DefinirUserInfo(u map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.opt.UserInfo = u
}

func (f *Faux) userInfo() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.opt.UserInfo != nil {
		return f.opt.UserInfo
	}
	return map[string]any{"auth": 1, "status": "Active", "max_connections": "1",
		"active_cons": strconv.Itoa(f.connexionsActivesLocked())}
}

// connexionsActivesLocked : places que le fournisseur considere prises maintenant. Verrou tenu.
func (f *Faux) connexionsActivesLocked() int {
	now := f.opt.Horloge()
	if now.Before(f.externeJusqua) {
		return 1
	}
	if f.unique && (f.filmsEnCours > 0 || (f.uniqueRes != "" && now.Sub(f.uniqueVu) < f.uniqueDuree)) {
		return 1
	}
	return 0
}

// ConnexionsActives : ce que user_info annonce (par defaut) en active_cons.
func (f *Faux) ConnexionsActives() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connexionsActivesLocked()
}

// OccuperPar fait prendre la place du compte par un « appareil externe » pendant `duree`
// (horloge du faux), sans passer par le client testé : toute demande de flux (playlist,
// segment, film) est refusee en 407 et user_info annonce la place prise. C'est la prise de
// place « le dernier arrive » du fournisseur reel, que mytv ne peut ni voir ni empecher.
// Une duree nulle ou negative libere.
func (f *Faux) OccuperPar(duree time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if duree <= 0 {
		f.externeJusqua = time.Time{}
		return
	}
	f.externeJusqua = f.opt.Horloge().Add(duree)
}

// LibererOccupant rend la place tout de suite.
func (f *Faux) LibererOccupant() { f.OccuperPar(0) }

func (f *Faux) catalogue(action string) (any, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.opt.Catalogue[action]
	return c, ok
}

// URL est l'adresse de base (http://127.0.0.1:port), sans slash final.
func (f *Faux) URL() string { return f.serveur.URL }

// Utilisateur et MotDePasse sont les identifiants acceptes.
func (f *Faux) Utilisateur() string { return f.opt.Utilisateur }
func (f *Faux) MotDePasse() string  { return f.opt.MotDePasse }

// Programmer ajoute une panne. Les pannes s'accumulent (la premiere qui
// correspond a la requete agit) ; Effacer les retire toutes.
func (f *Faux) Programmer(p Panne) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if p.Cible == "" {
		p.Cible = CibleTout
	}
	f.pannes = append(f.pannes, &panneActive{Panne: p, restantes: p.Fois, debut: f.opt.Horloge()})
}

// Effacer retire toutes les pannes programmees.
func (f *Faux) Effacer() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pannes = nil
}

// UneSeuleConnexion active la regle du fournisseur reel : tant que la derniere
// ressource demandee a ete vue il y a moins de `duree`, une AUTRE ressource
// (chaine) est refusee en 407. Un film dont la lecture n'est pas terminee (client
// toujours connecte) occupe aussi la place : toute autre demande de film, meme du
// meme titre (un seek), est refusee en 407 tant que l'ancienne connexion n'est pas
// fermee. Une duree nulle desactive la regle.
func (f *Faux) UneSeuleConnexion(duree time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unique = duree > 0
	f.uniqueDuree = duree
	f.uniqueRes = ""
	f.uniqueVu = time.Time{}
}

// ConnexionsOuvertes : connexions TCP actuellement ouvertes vers le faux (une connexion
// inactive gardee en keep-alive compte). Fermeture cote client observee avec un delai
// de quelques millisecondes : les tests attendent le retour a zero.
func (f *Faux) ConnexionsOuvertes() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.connOuvertes)
}

// FilmsEnCours : lectures de films pas encore terminees cote faux (le mode une seule
// connexion les compte ; sinon toujours 0).
func (f *Faux) FilmsEnCours() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.filmsEnCours
}

// ConnexionsTotal : connexions TCP ouvertes depuis le debut (ou la derniere RazCompteurs).
func (f *Faux) ConnexionsTotal() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connTotal
}

// Compteurs rend une copie des compteurs.
func (f *Faux) Compteurs() Compteurs {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.cpt
	c.PlaylistsParChaine = copie(c.PlaylistsParChaine)
	c.SegmentsParChaine = copie(c.SegmentsParChaine)
	c.ParAction = copie(c.ParAction)
	c.DetailsParID = copie(c.DetailsParID)
	return c
}

// RazCompteurs remet les compteurs et les traces a zero.
func (f *Faux) RazCompteurs() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cpt = f.cptVide()
	f.traces = nil
	f.connTotal = len(f.connOuvertes)
}

// Traces rend les requetes recues, dans l'ordre (les 500 dernieres). Le chemin
// est masque : les identifiants y sont remplaces par « *** ».
func (f *Faux) Traces() []Trace {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Trace(nil), f.traces...)
}

func copie(m map[string]int) map[string]int {
	r := make(map[string]int, len(m))
	for k, v := range m {
		r[k] = v
	}
	return r
}

// enveloppe enregistre la trace de chaque requete.
func (f *Faux) enveloppe(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		chemin := r.URL.Path
		if r.URL.Path == "/player_api.php" {
			chemin += "?action=" + r.URL.Query().Get("action")
		} else if p := strings.Split(strings.TrimPrefix(chemin, "/"), "/"); len(p) >= 4 && (p[0] == "live" || p[0] == "movie" || p[0] == "series") {
			p[1], p[2] = "***", "***"
			chemin = "/" + strings.Join(p, "/")
		}
		f.traces = append(f.traces, Trace{Methode: r.Method, Chemin: chemin, Entetes: r.Header.Clone()})
		if len(f.traces) > 500 {
			f.traces = f.traces[len(f.traces)-500:]
		}
		f.mu.Unlock()
		h.ServeHTTP(w, r)
	})
}

// appliquerPannes cherche une panne applicable. Renvoie true si la requete est
// deja traitee (le gestionnaire doit s'arreter). Les coupures sont signalees a
// l'appelant par la valeur retournee dans `coupure`.
func (f *Faux) appliquerPannes(w http.ResponseWriter, r *http.Request, cible Cible) (traite, coupure bool) {
	f.mu.Lock()
	var choisie *panneActive
	now := f.opt.Horloge()
	vivantes := f.pannes[:0]
	for _, p := range f.pannes {
		expiree := p.Duree > 0 && now.Sub(p.debut) >= p.Duree
		if expiree {
			continue
		}
		if choisie == nil && (p.Cible == CibleTout || p.Cible == cible || (cible == CibleDetails && p.Cible == CibleAPI)) {
			choisie = p
			if p.Fois > 0 {
				p.restantes--
				if p.restantes <= 0 {
					continue // epuisee apres cette requete
				}
			}
		}
		vivantes = append(vivantes, p)
	}
	f.pannes = vivantes
	if choisie != nil {
		f.cpt.Pannes++
		if choisie.Statut == http.StatusProxyAuthRequired {
			f.cpt.Refus407++
		}
	}
	f.mu.Unlock()
	if choisie == nil {
		return false, false
	}
	if choisie.Latence > 0 {
		select {
		case <-time.After(choisie.Latence):
		case <-r.Context().Done():
			return true, false
		}
	}
	switch {
	case choisie.Statut != 0:
		http.Error(w, http.StatusText(choisie.Statut), choisie.Statut)
		return true, false
	case choisie.CorpsVide:
		w.WriteHeader(http.StatusOK)
		return true, false
	case choisie.IgnorerRange:
		r.Header.Del("Range")
		return false, false
	case choisie.Coupure:
		return false, true
	}
	return false, false
}

// verifierUnique applique la regle « une seule connexion » ; true = refuse (407 deja ecrit).
func (f *Faux) verifierUnique(w http.ResponseWriter, ressource string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := f.opt.Horloge()
	if now.Before(f.externeJusqua) {
		f.cpt.Refus407++
		http.Error(w, "407", http.StatusProxyAuthRequired)
		return true
	}
	if !f.unique {
		return false
	}
	if f.uniqueRes != "" && f.uniqueRes != ressource && now.Sub(f.uniqueVu) < f.uniqueDuree {
		f.cpt.Refus407++
		http.Error(w, "407", http.StatusProxyAuthRequired)
		return true
	}
	f.uniqueRes, f.uniqueVu = ressource, now
	return false
}

// occuperFilm applique la partie « une lecture a la fois » aux films. rendre est nil
// quand la regle n'est pas active.
func (f *Faux) occuperFilm(w http.ResponseWriter) (rendre func(), refuse bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.unique {
		return nil, false
	}
	if f.filmsEnCours > 0 {
		f.cpt.Refus407++
		http.Error(w, "407", http.StatusProxyAuthRequired)
		return nil, true
	}
	f.filmsEnCours++
	return func() {
		f.mu.Lock()
		f.filmsEnCours--
		f.mu.Unlock()
	}, false
}

func (f *Faux) identifiantsOK(u, p string) bool {
	return u == f.opt.Utilisateur && p == f.opt.MotDePasse
}

// api repond a /player_api.php?username=&password=&action=.
func (f *Faux) api(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	action := q.Get("action")
	details := action == "get_vod_info" || action == "get_series_info"
	f.mu.Lock()
	f.cpt.API++
	f.cpt.ParAction[action]++
	if details {
		f.detailsEnCours++
		f.cpt.DetailsPic = max(f.cpt.DetailsPic, f.detailsEnCours)
		if action == "get_vod_info" {
			f.cpt.DetailsParID["vod:"+q.Get("vod_id")]++
		} else {
			f.cpt.DetailsParID["serie:"+q.Get("series_id")]++
		}
	}
	f.mu.Unlock()
	if details {
		defer func() { f.mu.Lock(); f.detailsEnCours--; f.mu.Unlock() }()
	}
	cible := CibleAPI
	if details {
		cible = CibleDetails
	}
	if t, _ := f.appliquerPannes(w, r, cible); t {
		return
	}
	if !f.identifiantsOK(q.Get("username"), q.Get("password")) {
		http.Error(w, "identifiants refuses", http.StatusUnauthorized)
		return
	}
	var v any = []any{}
	if action == "" {
		v = map[string]any{"user_info": f.userInfo()}
	} else if details {
		f.mu.Lock()
		m, id := f.opt.InfosFilm, q.Get("vod_id")
		if action == "get_series_info" {
			m, id = f.opt.InfosSerie, q.Get("series_id")
		}
		c, ok := m[id]
		gen := f.opt.InfoFilmPar
		if action == "get_series_info" {
			gen = f.opt.InfoSeriePar
		}
		if !ok && gen != nil {
			f.mu.Unlock()
			c, ok = gen(id)
			f.mu.Lock()
		}
		if !ok {
			// Repli sur la reponse unique par action (anciens tests : meme reponse pour tout identifiant).
			c, ok = f.opt.Catalogue[action]
		}
		if ok {
			v = c
		}
		f.mu.Unlock()
		if st, est := v.(StatutHTTP); est {
			http.Error(w, http.StatusText(int(st)), int(st))
			return
		}
	} else if c, ok := f.catalogue(action); ok {
		v = c
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// nomChaine extrait « 845410 » de « 845410.m3u8 » ou de « 845410_153.ts ».
func nomChaine(fichier string) string {
	fichier = strings.TrimSuffix(strings.TrimSuffix(fichier, ".m3u8"), ".ts")
	if i := strings.IndexByte(fichier, '_'); i >= 0 {
		fichier = fichier[:i]
	}
	return fichier
}

func (f *Faux) playlist(w http.ResponseWriter, r *http.Request) {
	if !f.identifiantsOK(r.PathValue("u"), r.PathValue("p")) {
		http.Error(w, "identifiants refuses", http.StatusUnauthorized)
		return
	}
	chaine := nomChaine(r.PathValue("fichier"))
	f.mu.Lock()
	f.cpt.Playlists++
	f.cpt.PlaylistsParChaine[chaine]++
	f.mu.Unlock()
	if t, _ := f.appliquerPannes(w, r, CiblePlaylist); t {
		return
	}
	if f.opt.RedirigerPlaylist && !strings.HasPrefix(r.URL.Path, "/noeud/") {
		http.Redirect(w, r, "/noeud"+r.URL.Path, http.StatusFound)
		return
	}
	if f.verifierUnique(w, chaine) {
		return
	}
	f.mu.Lock()
	tete, ok := f.fenetre[chaine]
	if !ok {
		tete = f.opt.PremiereSeq
	}
	f.fenetre[chaine] = tete + f.opt.AvanceParPlaylist
	f.nplaylist++
	// POURQUOI le hash : il change a CHAQUE playlist, comme chez le fournisseur,
	// pour que les consommateurs prouvent qu'ils ne dependent pas de l'URL.
	somme := sha1.Sum([]byte(fmt.Sprintf("%s/%d/%d", chaine, tete, f.nplaylist)))
	hash := hex.EncodeToString(somme[:])[:12]
	f.mu.Unlock()

	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n")
	fmt.Fprintf(&b, "#EXT-X-TARGETDURATION:%d\n#EXT-X-MEDIA-SEQUENCE:%d\n", int(f.opt.DureeSegment.Seconds()+0.5), tete)
	for n := tete; n < tete+f.opt.FenetreSegment; n++ {
		fmt.Fprintf(&b, "#EXTINF:%.3f,\n/hls/%s/%s_%d.ts\n", f.opt.DureeSegment.Seconds(), hash, chaine, n)
	}
	w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	_, _ = w.Write([]byte(b.String()))
}

// contenuSegment est deterministe : mêmes octets pour un même (chaine, numero).
func (f *Faux) contenuSegment(nom string) []byte {
	corps := make([]byte, f.opt.TailleSegment)
	motif := []byte(nom)
	for i := range corps {
		if i%188 == 0 {
			corps[i] = 0x47 // octet de synchronisation TS
		} else {
			corps[i] = motif[i%len(motif)]
		}
	}
	return corps
}

// contenuFilm : octets deterministes, tous differents d'une position a l'autre
// (un recollage au mauvais endroit se voit), de taille TailleFilm.
func (f *Faux) contenuFilm(nom string) []byte {
	// Memorise : fabriquer des dizaines de Mo octet par octet (sous -race) coutait plus
	// cher que le test qui s'en sert. Le contenu est en lecture seule.
	f.mu.Lock()
	defer f.mu.Unlock()
	if c, ok := f.films[nom]; ok {
		return c
	}
	c := f.fabriquerFilm(nom)
	if f.films == nil {
		f.films = map[string][]byte{}
	}
	f.films[nom] = c
	return c
}

func (f *Faux) fabriquerFilm(nom string) []byte {
	n := f.opt.TailleFilm
	if n <= 0 {
		n = f.opt.TailleSegment
	}
	corps := make([]byte, n)
	graine := 0
	for _, c := range nom {
		graine = graine*31 + int(c)
	}
	for i := range corps {
		corps[i] = byte(graine + i*7 + i/251)
	}
	return corps
}

func (f *Faux) segment(w http.ResponseWriter, r *http.Request) {
	fichier := r.PathValue("fichier")
	chaine := nomChaine(fichier)
	f.mu.Lock()
	f.cpt.Segments++
	f.cpt.SegmentsParChaine[chaine]++
	f.mu.Unlock()
	t, coupure := f.appliquerPannes(w, r, CibleSegment)
	if t {
		return
	}
	if !strings.HasSuffix(fichier, ".ts") || chaine == "" {
		http.NotFound(w, r)
		return
	}
	if f.verifierUnique(w, chaine) {
		return
	}
	corps := f.contenuSegment(fichier)
	if f.opt.ContenuSegment != nil {
		if c, ok := f.opt.ContenuSegment(fichier); ok {
			corps = c
		}
	}
	servir(w, r, corps, "video/mp2t", coupure)
}

func (f *Faux) film(w http.ResponseWriter, r *http.Request) {
	if !f.identifiantsOK(r.PathValue("u"), r.PathValue("p")) {
		http.Error(w, "identifiants refuses", http.StatusUnauthorized)
		return
	}
	if f.opt.RedirigerFilms && !strings.HasPrefix(r.URL.Path, "/noeud/") {
		f.mu.Lock()
		f.cpt.RedirectionsFilms++
		f.mu.Unlock()
		http.Redirect(w, r, "/noeud"+r.URL.Path, http.StatusFound)
		return
	}
	f.mu.Lock()
	f.cpt.Films++
	f.mu.Unlock()
	t, coupure := f.appliquerPannes(w, r, CibleFilm)
	if t {
		return
	}
	nom := r.PathValue("fichier")
	if f.verifierUnique(w, "film:"+nom) {
		return
	}
	if rendre, refuse := f.occuperFilm(w); refuse {
		return
	} else if rendre != nil {
		defer rendre()
	}
	servir(w, r, f.contenuFilm(nom), "video/mp4", coupure)
}

// servir envoie `corps` (avec prise en charge de Range via ServeContent) ou, en
// cas de coupure, annonce la longueur complete puis abandonne la connexion apres
// la moitie de ce qui devait etre envoye (la plage demandee, le cas echeant) :
// c'est ce que voit un client quand le fournisseur s'interrompt.
func servir(w http.ResponseWriter, r *http.Request, corps []byte, typ string, coupure bool) {
	w.Header().Set("Content-Type", typ)
	if coupure {
		w = &coupeur{ResponseWriter: w}
	}
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(corps))
}

// coupeur laisse passer la moitie du corps annonce, puis coupe la connexion.
type coupeur struct {
	http.ResponseWriter
	permis, ecrit int
	init          bool
}

func (c *coupeur) Write(p []byte) (int, error) {
	if !c.init {
		c.init = true
		n, _ := strconv.Atoi(c.Header().Get("Content-Length"))
		c.permis = n / 2
	}
	reste := c.permis - c.ecrit
	if len(p) > reste {
		p = p[:max(reste, 0)]
		_, _ = c.ResponseWriter.Write(p)
		if fl, ok := c.ResponseWriter.(http.Flusher); ok {
			fl.Flush()
		}
		panic(http.ErrAbortHandler)
	}
	c.ecrit += len(p)
	return c.ResponseWriter.Write(p)
}

// xmltv sert le guide configure (Options.XMLTV).
func (f *Faux) xmltv(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f.mu.Lock()
	f.cpt.API++
	f.cpt.ParAction["xmltv"]++
	f.mu.Unlock()
	if t, _ := f.appliquerPannes(w, r, CibleAPI); t {
		return
	}
	if !f.identifiantsOK(q.Get("username"), q.Get("password")) {
		http.Error(w, "identifiants refuses", http.StatusUnauthorized)
		return
	}
	if len(f.opt.XMLTV) == 0 {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write(f.opt.XMLTV)
}

// gif1x1 : le plus petit GIF valide, pour les logos et affiches du catalogue realiste.
var gif1x1 = []byte("GIF89a\x01\x00\x01\x00\x80\x00\x00\x00\x00\x00\xff\xff\xff!\xf9\x04\x01\x00\x00\x00\x00,\x00\x00\x00\x00\x01\x00\x01\x00\x00\x02\x02D\x01\x00;")

func imageFactice(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "image/gif")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(gif1x1)
}
