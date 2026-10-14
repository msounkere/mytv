// Package live relaie le direct HLS entre les lecteurs et le fournisseur.
//
// POURQUOI ce paquet existe. Un proxy naif telecharge la playlist en UN essai ;
// si ce telechargement expire (mesure : 5 a 7 s de temps en temps), il sert aussitot
// une playlist de repli d'un segment et le lecteur cale. Ici :
//   - delais courts et nouveaux essais avant de renoncer ;
//   - une seule requete amont pour plusieurs demandes simultanees ;
//   - en cas d'echec, la DERNIERE BONNE playlist est servie (ses segments restent
//     valables des dizaines de secondes) ;
//   - le fournisseur n'autorise qu'UNE connexion : toute requete amont passe par un
//     bail, et un refus 407 (place encore gardee apres un zapping) est attendu, pas
//     renvoye au lecteur ;
//   - tout est chronometre.
package live

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"sync"
	"time"

	"mytv/internal/config"
	"mytv/internal/domain"
	"mytv/internal/hls"
)

// PrefixeParDefaut est la racine des routes du direct.
const PrefixeParDefaut = "/api/v1/live"

// partMaxSegment : un segment ne doit pas, a lui seul, occuper plus de 1/2 du cache
// en octets, sinon un seul gros segment chasse tous les autres.
const partMaxSegment = 2

// Options rassemble tout ce dont le relais depend : aucun reglage n'est code ici.
type Options struct {
	Fournisseur domain.Fournisseur
	Baux        domain.Baux
	Live        config.Live
	Bail        config.Bail
	Reseau      config.Reseau
	// Secret signe les URL de segments (config serveur.secret_signature).
	Secret []byte
	// Authentifier reconnait l'appelant et rend l'identifiant de sa SESSION (celle
	// qui porte le bail). Absent : toutes les requetes sont refusees (CA7).
	Authentifier func(*http.Request) (session string, ok bool)
	Prefixe      string // racine des routes ; PrefixeParDefaut si vide
	Journal      *slog.Logger
	Horloge      func() time.Time
}

type Relais struct {
	opt      Options
	hls      *hls.Reecrivain
	journal  *slog.Logger
	amont    chan struct{} // semaphore des requetes amont
	segments *cacheSegments
	st       stats

	base   context.Context // vit autant que le relais : les appels partages en dependent
	arret  context.CancelFunc
	taches sync.WaitGroup

	mu      sync.Mutex
	dernier map[string]playlist // derniere bonne playlist, par chaine
	enCours map[string]*appel
}

func Nouveau(opt Options) (*Relais, error) {
	var manques []error
	exige := func(ok bool, msg string) {
		if !ok {
			manques = append(manques, errors.New(msg))
		}
	}
	exige(opt.Fournisseur != nil, "live : fournisseur absent")
	exige(opt.Baux != nil, "live : baux absents")
	exige(opt.Live.CacheSegments.Nombre > 0 && opt.Live.CacheSegments.Octets > 0, "live : cache_segments non configure")
	exige(opt.Reseau.EssaisPlaylist >= 1 && opt.Reseau.EssaisSegment >= 1, "live : essais_playlist et essais_segment doivent etre >= 1")
	exige(opt.Reseau.DelaiPlaylist > 0 && opt.Reseau.DelaiSegment > 0, "live : delai_playlist et delai_segment doivent etre > 0")
	exige(opt.Reseau.RequetesAmontMax >= 1, "live : requetes_amont_max doit etre >= 1")
	exige(opt.Bail.Reessai > 0, "live : bail.reessai doit etre > 0 (sinon attente active)")
	if err := errors.Join(manques...); err != nil {
		return nil, err
	}
	reecrivain, err := hls.Nouveau(opt.Secret, hls.Options{Horloge: opt.Horloge})
	if err != nil {
		return nil, err
	}
	if opt.Prefixe == "" {
		opt.Prefixe = PrefixeParDefaut
	}
	if opt.Journal == nil {
		opt.Journal = slog.Default()
	}
	if opt.Horloge == nil {
		opt.Horloge = time.Now
	}
	base, arret := context.WithCancel(context.Background())
	return &Relais{
		opt:      opt,
		hls:      reecrivain,
		journal:  opt.Journal,
		amont:    make(chan struct{}, opt.Reseau.RequetesAmontMax),
		segments: nouveauCache(opt.Live.CacheSegments.Nombre, opt.Live.CacheSegments.Octets),
		base:     base,
		arret:    arret,
		dernier:  map[string]playlist{},
		enCours:  map[string]*appel{},
	}, nil
}

// Fermer annule les appels amont partages et attend leur fin (aucune goroutine
// ne survit au relais).
func (r *Relais) Fermer() {
	r.arret()
	r.taches.Wait()
}

// Etat donne les statistiques courantes (les memes que /etat).
func (r *Relais) Etat() Resume {
	res := r.st.resume()
	res.SegmentsEnCache, res.OctetsSegmentsCache = r.segments.taille()
	return res
}

var reChaine = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type cleSession struct{}

// Handler rend les routes du relais. TOUT passe d'abord par l'authentification,
// y compris les chemins inconnus : refus par defaut.
func (r *Relais) Handler() http.Handler {
	p := r.opt.Prefixe
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+p+"/etat", r.routeEtat)
	mux.HandleFunc("GET "+p+"/{fichier}", r.routePlaylist)
	mux.HandleFunc("GET "+p+"/{chaine}/seg/{jeton}", r.routeSegment)
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		var session string
		ok := false
		if r.opt.Authentifier != nil {
			session, ok = r.opt.Authentifier(req)
		}
		if !ok || session == "" {
			http.Error(w, "authentification requise", http.StatusUnauthorized)
			return
		}
		mux.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), cleSession{}, session)))
	})
}

func sessionDe(req *http.Request) string {
	s, _ := req.Context().Value(cleSession{}).(string)
	return s
}

// bail reserve la connexion du fournisseur pour cette session (ou reutilise celle
// qu'elle a deja : zapping) et la renouvelle. Il renvoie false apres avoir repondu.
func (r *Relais) bail(w http.ResponseWriter, req *http.Request, chaine string) (domain.Bail, bool) {
	b, err := r.opt.Baux.Acquerir(req.Context(), r.opt.Fournisseur.ID(), sessionDe(req), chaine)
	switch {
	case err == nil:
		b.Renouveler()
		return b, true
	case req.Context().Err() != nil:
		// Le client est parti pendant l'attente : personne a qui repondre.
	case errors.Is(err, domain.ErrPlaceIndisponible):
		w.Header().Set("Retry-After", r.retryApres())
		http.Error(w, "connexion du fournisseur occupee", http.StatusServiceUnavailable)
	default:
		r.journal.Error("bail refuse", "chaine", chaine, "erreur", err)
		http.Error(w, "reservation impossible", http.StatusInternalServerError)
	}
	return nil, false
}
