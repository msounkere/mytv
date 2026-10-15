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
	"mytv/internal/hotes"
)

// PrefixeParDefaut est la racine des routes du direct.
const PrefixeParDefaut = "/api/v1/live"

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
	// EstAdmin dit si l'appelant (deja authentifie) est administrateur : seul lui lit
	// /etat. Absent : /etat est refuse a tout le monde.
	EstAdmin func(*http.Request) bool
	// HotesAutorises : hotes, adresses ou plages CIDR que les segments ont le droit
	// de viser en plus des adresses publiques et de l'hote du fournisseur (config
	// serveur.hotes_segments_autorises). Par defaut les adresses privees, loopback et
	// de lien local sont refusees (SSRF : une playlist piegee ne doit pas faire
	// parler le relais a un service interne).
	HotesAutorises []string
	// ResoudreHote remplace la resolution DNS (tests).
	ResoudreHote hotes.Resolveur
	Prefixe      string // racine des routes ; PrefixeParDefaut si vide
	Journal      *slog.Logger
	Horloge      func() time.Time
	// Instrumentation publie les mesures du relais (nil = aucune). Etiquettes a
	// cardinalite bornee : jamais de session, de chaine ni d'URL.
	Instrumentation domain.Instrumentation
}

type Relais struct {
	opt      Options
	hls      *hls.Reecrivain
	journal  *slog.Logger
	amont    chan struct{} // semaphore des requetes amont
	segments *cacheSegments
	st       stats
	inst     domain.Instrumentation // jamais nil apres Nouveau
	hotes    *hotes.Filtre

	base   context.Context // vit autant que le relais : les appels partages en dependent
	arret  context.CancelFunc
	taches sync.WaitGroup

	mu      sync.Mutex
	dernier map[string]playlist // derniere bonne playlist, par chaine
	enCours map[string]*appel
	// courante : chaine que chaque session lit en ce moment (voir libererSi).
	courante map[string]lecture
}

type lecture struct {
	chaine string
	vue    time.Time
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
	exige(opt.Live.TableSegments.Nombre > 0 && opt.Live.TableSegments.Validite > 0, "live : table_segments non configuree")
	exige(opt.Reseau.EssaisPlaylist >= 1 && opt.Reseau.EssaisSegment >= 1, "live : essais_playlist et essais_segment doivent etre >= 1")
	exige(opt.Reseau.DelaiPlaylist > 0 && opt.Reseau.DelaiSegment > 0, "live : delai_playlist et delai_segment doivent etre > 0")
	exige(opt.Live.TaillePlaylistMax > 0 && opt.Live.TailleTampon > 0 && opt.Live.FenetreMesures > 0 && opt.Live.PartMaxSegment >= 1,
		"live : taille_playlist_max, taille_tampon, fenetre_mesures et part_max_segment doivent etre > 0")
	exige(opt.Reseau.RequetesAmontMax >= 1, "live : requetes_amont_max doit etre >= 1")
	exige(opt.Bail.Reessai > 0, "live : bail.reessai doit etre > 0 (sinon attente active)")
	if err := errors.Join(manques...); err != nil {
		return nil, err
	}
	reecrivain, err := hls.Nouveau(opt.Secret, hls.Options{Capacite: opt.Live.TableSegments.Nombre, Validite: opt.Live.TableSegments.Validite.Std(), Horloge: opt.Horloge})
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
	if opt.Instrumentation == nil {
		opt.Instrumentation = domain.SansInstrumentation{}
	}
	filtre, err := hotes.Nouveau(opt.Fournisseur.URLLive("0"), opt.HotesAutorises, opt.ResoudreHote)
	if err != nil {
		return nil, err
	}
	base, arret := context.WithCancel(context.Background())
	return &Relais{
		hotes:    filtre,
		st:       stats{fenetre: opt.Live.FenetreMesures},
		inst:     opt.Instrumentation,
		opt:      opt,
		hls:      reecrivain,
		journal:  opt.Journal,
		amont:    make(chan struct{}, opt.Reseau.RequetesAmontMax),
		segments: nouveauCache(opt.Live.CacheSegments.Nombre, opt.Live.CacheSegments.Octets),
		base:     base,
		arret:    arret,
		dernier:  map[string]playlist{},
		enCours:  map[string]*appel{},
		courante: map[string]lecture{},
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

// cleDebut porte l'instant ou la requete du client est arrivee : la patience (attente
// d'une place chez les baux PUIS patience sur 407) est UNE SEULE, plafonnee a
// bail.patience_max a partir de cet instant, et non deux attentes qui s'additionnent.
type cleDebut struct{}

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
		ctx := context.WithValue(req.Context(), cleSession{}, session)
		ctx = context.WithValue(ctx, cleDebut{}, r.opt.Horloge())
		mux.ServeHTTP(w, req.WithContext(ctx))
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
		r.noterLecture(sessionDe(req), chaine)
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

// noterLecture retient la chaine que la session lit maintenant. Les entrees plus
// vieilles que le TTL du bail sont oubliees : leur bail a expire de toute facon.
func (r *Relais) noterLecture(session, chaine string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := r.opt.Horloge()
	r.courante[session] = lecture{chaine: chaine, vue: now}
	for s, l := range r.courante {
		if now.Sub(l.vue) > r.opt.Bail.TTL.Std() {
			delete(r.courante, s)
		}
	}
}

// libererSi rend la place de la session SEULEMENT si elle lit encore `chaine`.
//
// POURQUOI : quand un lecteur zappe, il annule sa requete de segment de l'ANCIENNE
// chaine alors que son bail (un seul par fournisseur, reutilise) sert deja la
// NOUVELLE. Liberer sur cette annulation couperait la nouvelle lecture.
func (r *Relais) libererSi(session, chaine string, b domain.Bail) {
	r.mu.Lock()
	l, ok := r.courante[session]
	if ok && l.chaine == chaine {
		delete(r.courante, session)
	}
	r.mu.Unlock()
	if ok && l.chaine == chaine {
		b.Liberer()
	}
}

// liberateurSi est implemente par internal/baux : rendre la place seulement si le
// bail sert encore la ressource.
type liberateurSi interface {
	LibererSi(fournisseurID, session, ressource string) bool
}

// FinDeLecture : l'interface a ferme le lecteur de `chaine`. Rend la place du
// fournisseur tout de suite SI la session la lit encore (apres un zapping l'appel
// tardif de l'ancienne chaine ne fait rien). Idempotent, sans erreur.
func (r *Relais) FinDeLecture(session, chaine string) {
	r.mu.Lock()
	l, ok := r.courante[session]
	encore := ok && l.chaine == chaine
	if encore {
		delete(r.courante, session)
	}
	r.mu.Unlock()
	if !encore {
		return
	}
	if b, ok := r.opt.Baux.(liberateurSi); ok {
		b.LibererSi(r.opt.Fournisseur.ID(), session, chaine)
	}
}
