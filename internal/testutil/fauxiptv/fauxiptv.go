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
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
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

	TailleSegment  int           // octets d'un segment TS, defaut 4096
	DureeSegment   time.Duration // valeur annoncee dans #EXTINF, defaut 10 s
	FenetreSegment int           // segments par playlist, defaut 5
	PremiereSeq    int           // numero du premier segment de la premiere playlist, defaut 150
	// AvanceParPlaylist : de combien de segments la fenetre glisse a chaque
	// playlist servie pour une chaine (defaut 1). Deterministe, donc sans horloge.
	AvanceParPlaylist int

	// RedirigerPlaylist : la playlist repond d'abord 302 vers un autre « noeud »,
	// comme le vrai fournisseur au premier appel.
	RedirigerPlaylist bool

	// Horloge sert aux durees de panne et au mode une seule connexion (defaut time.Now).
	Horloge func() time.Time
}

// Cible dit sur quelles requetes une panne s'applique.
type Cible string

const (
	CibleAPI      Cible = "api"
	CiblePlaylist Cible = "playlist"
	CibleSegment  Cible = "segment"
	CibleFilm     Cible = "film"
	CibleTout     Cible = "tout"
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
}

// Compteurs : nombre de requetes recues, par nature.
type Compteurs struct {
	API       int
	Playlists int
	Segments  int
	Films     int
	Refus407  int // 407 renvoyes, quelle qu'en soit la cause
	Pannes    int // reponses alterees par une panne programmee
	// PlaylistsParChaine et SegmentsParChaine : par numero de chaine.
	PlaylistsParChaine map[string]int
	SegmentsParChaine  map[string]int
	// ParAction : appels d'API par action (« get_live_streams »...).
	ParAction map[string]int
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

	unique      bool
	uniqueDuree time.Duration
	uniqueRes   string
	uniqueVu    time.Time
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
	f := &Faux{opt: opt, fenetre: map[string]int{}}
	f.cpt = f.cptVide()

	mux := http.NewServeMux()
	mux.HandleFunc("/player_api.php", f.api)
	mux.HandleFunc("/live/{u}/{p}/{fichier}", f.playlist)
	mux.HandleFunc("/noeud/live/{u}/{p}/{fichier}", f.playlist) // cible des redirections
	mux.HandleFunc("/hls/{hash}/{fichier}", f.segment)
	mux.HandleFunc("/movie/{u}/{p}/{fichier}", f.film)
	f.serveur = httptest.NewServer(f.enveloppe(mux))
	t.Cleanup(f.serveur.Close)
	return f
}

func (f *Faux) cptVide() Compteurs {
	return Compteurs{
		PlaylistsParChaine: map[string]int{},
		SegmentsParChaine:  map[string]int{},
		ParAction:          map[string]int{},
	}
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
// (chaine) est refusee en 407. Une duree nulle desactive la regle.
func (f *Faux) UneSeuleConnexion(duree time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.unique = duree > 0
	f.uniqueDuree = duree
	f.uniqueRes = ""
	f.uniqueVu = time.Time{}
}

// Compteurs rend une copie des compteurs.
func (f *Faux) Compteurs() Compteurs {
	f.mu.Lock()
	defer f.mu.Unlock()
	c := f.cpt
	c.PlaylistsParChaine = copie(c.PlaylistsParChaine)
	c.SegmentsParChaine = copie(c.SegmentsParChaine)
	c.ParAction = copie(c.ParAction)
	return c
}

// RazCompteurs remet les compteurs et les traces a zero.
func (f *Faux) RazCompteurs() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cpt = f.cptVide()
	f.traces = nil
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
		} else if p := strings.Split(strings.TrimPrefix(chemin, "/"), "/"); len(p) >= 4 && (p[0] == "live" || p[0] == "movie") {
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
		if choisie == nil && (p.Cible == CibleTout || p.Cible == cible) {
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
	case choisie.Coupure:
		return false, true
	}
	return false, false
}

// verifierUnique applique la regle « une seule connexion » ; true = refuse (407 deja ecrit).
func (f *Faux) verifierUnique(w http.ResponseWriter, ressource string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.unique {
		return false
	}
	now := f.opt.Horloge()
	if f.uniqueRes != "" && f.uniqueRes != ressource && now.Sub(f.uniqueVu) < f.uniqueDuree {
		f.cpt.Refus407++
		http.Error(w, "407", http.StatusProxyAuthRequired)
		return true
	}
	f.uniqueRes, f.uniqueVu = ressource, now
	return false
}

func (f *Faux) identifiantsOK(u, p string) bool {
	return u == f.opt.Utilisateur && p == f.opt.MotDePasse
}

// api repond a /player_api.php?username=&password=&action=.
func (f *Faux) api(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	action := q.Get("action")
	f.mu.Lock()
	f.cpt.API++
	f.cpt.ParAction[action]++
	f.mu.Unlock()
	if t, _ := f.appliquerPannes(w, r, CibleAPI); t {
		return
	}
	if !f.identifiantsOK(q.Get("username"), q.Get("password")) {
		http.Error(w, "identifiants refuses", http.StatusUnauthorized)
		return
	}
	var v any = []any{}
	if action == "" {
		v = map[string]any{"user_info": map[string]any{"auth": 1, "status": "Active", "max_connections": "1"}}
	} else if c, ok := f.opt.Catalogue[action]; ok {
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
	servir(w, r, corps, "video/mp2t", coupure)
}

func (f *Faux) film(w http.ResponseWriter, r *http.Request) {
	if !f.identifiantsOK(r.PathValue("u"), r.PathValue("p")) {
		http.Error(w, "identifiants refuses", http.StatusUnauthorized)
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
	servir(w, r, f.contenuSegment(nom), "video/mp4", coupure)
}

// servir envoie `corps` (avec prise en charge de Range via ServeContent) ou, en
// cas de coupure, annonce la taille complete puis abandonne la connexion apres
// la moitie : c'est ce que voit un client quand le fournisseur s'interrompt.
func servir(w http.ResponseWriter, r *http.Request, corps []byte, typ string, coupure bool) {
	w.Header().Set("Content-Type", typ)
	if coupure {
		w.Header().Set("Content-Length", strconv.Itoa(len(corps)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(corps[:len(corps)/2])
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
		panic(http.ErrAbortHandler)
	}
	http.ServeContent(w, r, "", time.Time{}, bytesReader(corps))
}
