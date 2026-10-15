// Package images est le proxy d'images signe (US-23).
//
// POURQUOI : 92 % des logos de chaines sont en « http://<IP du fournisseur>/picons/... ».
// Sur une page HTTPS le navigateur les bloque (contenu mixte) et l'adresse du
// fournisseur fuit vers chaque client. Le serveur reecrit donc chaque URL d'image en
// « /api/v1/image/<jeton> » et la sert lui-meme, depuis un cache disque.
//
// Surete : le jeton est CHIFFRE et authentifie (hls.Sceau, derive de
// serveur.secret_signature). La route n'accepte jamais d'URL libre : elle ne telecharge
// que ce que NOUS avons emis, ce qui interdit d'en faire un relais ouvert (SSRF). Meme
// une URL emise est controlee (hotes.Filtre) a la resolution, a la connexion et a
// chaque redirection. Les telechargements ne prennent aucun bail de connexion : ils ne
// comptent pas dans la connexion unique du compte Xtream.
package images

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"mytv/internal/config"
	"mytv/internal/hls"
	"mytv/internal/hotes"
)

// Erreurs typees : l'API choisit le statut HTTP.
var (
	ErrJeton       = errors.New("images : jeton invalide")
	ErrRefuse      = errors.New("images : adresse non autorisee")
	ErrIntrouvable = errors.New("images : image introuvable chez le fournisseur")
	ErrAmont       = errors.New("images : image indisponible chez le fournisseur")
	ErrSature      = errors.New("images : trop de telechargements en cours")
)

const (
	portee = "i"
	// Libelles de derivation : une cle par usage, distincte de celle des segments.
	libelleChiffrement = "mytv/image/jeton/chiffrement/v1"
	libelleNonce       = "mytv/image/jeton/nonce/v1"

	// Abreviations du debut de l'URL dans la charge scellee. POURQUOI : l'export en bloc
	// porte ~65 000 jetons ; le debut d'une URL (schema + hote) est connu du serveur et
	// represente la moitie de sa longueur. Marqueur 0 = URL complete.
	marqueurComplet     = '\x00'
	marqueurFournisseur = '\x01' // suivi de l'id du fournisseur puis \x00 puis le reste
	marqueurTMDB        = '\x02'
	origineTMDB         = "https://image.tmdb.org"
)

// Origine est un fournisseur dont l'adresse est abregee dans les jetons.
type Origine struct {
	ID   string
	Base string // http://hote[:port]
}

type Options struct {
	Config config.Images
	Secret []byte
	// Fournisseurs : leurs adresses de base servent a abreger les jetons.
	Fournisseurs []Origine
	// UserAgent du fournisseur ; vide = agent generique.
	UserAgent string
	// Filtre decide quels hotes sont atteignables (SSRF). Obligatoire.
	Filtre  *hotes.Filtre
	Horloge func() time.Time
	Journal *slog.Logger
}

// Service reecrit les URL d'images et sert les images.
type Service struct {
	cfg     config.Images
	sceau   *hls.Sceau
	origine map[string]string // origine « http://hote » -> id fournisseur
	base    map[string]string // id fournisseur -> origine
	agent   string
	filtre  *hotes.Filtre
	client  *http.Client
	horloge func() time.Time
	log     *slog.Logger

	cache  *cache
	neg    *negatif
	groupe groupe
	places chan struct{}
}

// donnees est une image fraichement telechargee.
type donnees struct {
	typeMime string
	corps    []byte
	modif    time.Time
}

func Nouveau(opt Options) (*Service, error) {
	if opt.Filtre == nil {
		return nil, errors.New("images : filtre d'hotes absent")
	}
	if opt.Horloge == nil {
		opt.Horloge = time.Now
	}
	if opt.Journal == nil {
		opt.Journal = slog.Default()
	}
	c := opt.Config
	if c.ConcurrenceMax < 1 || c.Delai <= 0 || c.TailleMax < 1 {
		return nil, errors.New("images : concurrence_max, delai et taille_max doivent etre > 0 (images.*)")
	}
	sceau, err := hls.NouveauSceau(opt.Secret, libelleChiffrement, libelleNonce)
	if err != nil {
		return nil, err
	}
	cch, err := ouvrirCache(c.CacheDossier, c.CacheOctets, c.CacheValidite.Std(), opt.Horloge)
	if err != nil {
		return nil, err
	}
	s := &Service{cfg: c, sceau: sceau, origine: map[string]string{}, base: map[string]string{}, agent: opt.UserAgent,
		filtre: opt.Filtre, horloge: opt.Horloge, log: opt.Journal, cache: cch,
		neg:    nouveauNegatif(c.CacheNegatif.Std(), c.CacheNegatifMax, opt.Horloge),
		places: make(chan struct{}, c.ConcurrenceMax)}
	for _, f := range opt.Fournisseurs {
		if o, ok := origineDe(f.Base); ok && f.ID != "" && !strings.ContainsRune(f.ID, 0) {
			s.origine[o] = f.ID
			s.base[f.ID] = o
		}
	}
	// Pas de keep-alive : quelques milliers de picons sur des hotes differents
	// laisseraient des connexions inactives sans rien economiser. Pas de cookie, pas de
	// proxy d'environnement (une variable HTTP_PROXY ne doit pas detourner ce trafic).
	s.client = &http.Client{
		Timeout: c.Delai.Std(),
		Transport: &http.Transport{
			DialContext:           opt.Filtre.DialContext(c.Delai.Std()),
			DisableKeepAlives:     true,
			DisableCompression:    true,
			Proxy:                 nil,
			ResponseHeaderTimeout: c.Delai.Std(),
			TLSHandshakeTimeout:   c.Delai.Std(),
		},
		CheckRedirect: opt.Filtre.ControleRedirection(c.RedirectionsMax),
	}
	return s, nil
}

// origineDe : « scheme://hote[:port] » en minuscules, ou faux si l'URL n'est pas http(s).
func origineDe(u string) (string, bool) {
	p, err := url.Parse(u)
	if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" || p.User != nil {
		return "", false
	}
	return p.Scheme + "://" + strings.ToLower(p.Host), true
}

// Reecrire donne l'adresse servie par ce service pour une URL d'image d'origine.
//
// Decision (CA1) : TOUTES les URL http(s) sont reecrites, y compris les https deja
// surs. D'une part la CSP de l'interface devient `img-src 'self' data:` (plus aucun
// hote tiers), d'autre part cela evite de faire connaitre l'IP de chaque client aux
// hebergeurs d'affiches. Chaine vide -> vide. `data:` reste tel quel (autorise par la
// CSP, rien a relayer). Toute autre forme (schema inconnu, URL relative, identifiants
// dans l'URL) -> vide : on n'emet jamais de jeton pour ce qu'on ne servirait pas.
func (s *Service) Reecrire(u string) string {
	u = strings.TrimSpace(u)
	switch {
	case u == "":
		return ""
	case strings.HasPrefix(strings.ToLower(u), "data:image/"):
		return u
	}
	o, ok := origineDe(u)
	if !ok {
		return ""
	}
	// Reste de l'URL apres « schema://hote[:port] » (chemin, requete) : tel quel.
	debut := strings.Index(u, "://") + 3
	rest := u[debut+strings.IndexAny(u[debut:]+"/", "/?#"):]
	var charge string
	switch {
	case s.origine[o] != "":
		charge = string(marqueurFournisseur) + s.origine[o] + "\x00" + rest
	case o == origineTMDB:
		charge = string(marqueurTMDB) + rest
	default:
		charge = string(marqueurComplet) + u
	}
	return Chemin + s.sceau.Sceller(portee, charge)
}

// Chemin est le prefixe des URL reecrites (racine de l'API + route).
const Chemin = "/api/v1/image/"

// resoudre rend l'URL d'origine d'un jeton authentique, ou ErrJeton.
func (s *Service) resoudre(jeton string) (string, error) {
	charge, err := s.sceau.Ouvrir(portee, jeton)
	if err != nil || charge == "" {
		return "", ErrJeton
	}
	var u string
	switch charge[0] {
	case marqueurComplet:
		u = charge[1:]
	case marqueurTMDB:
		u = origineTMDB + charge[1:]
	case marqueurFournisseur:
		id, rest, ok := strings.Cut(charge[1:], "\x00")
		o := s.base[id]
		if !ok || o == "" {
			return "", ErrJeton // fournisseur retire de la configuration
		}
		u = o + rest
	default:
		return "", ErrJeton
	}
	if _, ok := origineDe(u); !ok {
		return "", ErrJeton
	}
	return u, nil
}

// Image est une image prete a etre servie. Fermer est obligatoire.
type Image struct {
	Type    string
	ETag    string
	Modif   time.Time
	Contenu io.ReadSeeker
	ferme   func()
}

func (i *Image) Fermer() {
	if i.ferme != nil {
		i.ferme()
	}
}

// Obtenir rend l'image d'un jeton : cache, sinon un seul telechargement partage.
func (s *Service) Obtenir(ctx context.Context, jeton string) (*Image, error) {
	u, err := s.resoudre(jeton)
	if err != nil {
		return nil, err
	}
	cle := cleDe(u)
	perimee := false
	if r, ok := s.cache.ouvrir(cle); ok {
		if !r.Perime {
			return s.depuisCache(cle, r), nil
		}
		r.Fermer()
		perimee = true
	}
	if err, ok := s.neg.chercher(cle); ok {
		if perimee {
			if r, ok := s.cache.ouvrir(cle); ok {
				return s.depuisCache(cle, r), nil
			}
		}
		return nil, err
	}
	d, err := s.telecharger(ctx, cle, u)
	if err != nil && perimee && !errors.Is(err, context.Canceled) {
		// Mieux vaut un logo vieux d'une semaine que pas de logo quand l'amont est en panne.
		if r, ok := s.cache.ouvrir(cle); ok {
			return s.depuisCache(cle, r), nil
		}
	}
	return s.apres(cle, d, err)
}

func (s *Service) apres(cle string, d *donnees, err error) (*Image, error) {
	if err != nil {
		return nil, err
	}
	if r, ok := s.cache.ouvrir(cle); ok {
		return s.depuisCache(cle, r), nil
	}
	// Disque indisponible ou image evincee entre-temps : on sert quand meme.
	return &Image{Type: d.typeMime, ETag: etag(cle, d.modif), Modif: d.modif, Contenu: bytes.NewReader(d.corps)}, nil
}

func (s *Service) depuisCache(cle string, r *resultatCache) *Image {
	return &Image{Type: r.Type, ETag: etag(cle, r.Modif), Modif: r.Modif, Contenu: r.Lecteur(), ferme: func() { r.Fermer() }}
}

// etag : change quand l'image est retelechargee (la date d'ecriture en fait partie).
func etag(cle string, modif time.Time) string {
	return `"` + cle[:16] + "-" + strconv.FormatInt(modif.UnixMilli(), 36) + `"`
}

// telecharger fusionne les demandes simultanees, puis memorise le resultat (cache ou
// cache negatif).
func (s *Service) telecharger(ctx context.Context, cle, u string) (*donnees, error) {
	return s.groupe.faire(ctx, cle, func() (*donnees, error) {
		d, err := s.aller(u)
		switch {
		case err == nil:
			if e := s.cache.ecrire(cle, d.typeMime, d.corps); e != nil {
				s.log.Warn("cache d'images non ecrit", "erreur", e)
			}
		case !errors.Is(err, ErrSature):
			s.neg.noter(cle, err)
		}
		return d, err
	})
}

// aller telecharge chez le fournisseur. Detache de la requete du client (voir groupe).
func (s *Service) aller(u string) (*donnees, error) {
	// Concurrence bornee : attendre une place au plus `delai`, puis telecharger avec
	// SON PROPRE delai (l'attente ne mange pas le temps du telechargement).
	attente := time.NewTimer(s.cfg.Delai.Std())
	defer attente.Stop()
	select {
	case s.places <- struct{}{}:
		defer func() { <-s.places }()
	case <-attente.C:
		return nil, ErrSature
	}
	ctx, annuler := context.WithTimeout(context.Background(), s.cfg.Delai.Std())
	defer annuler()

	// Controle precoce (bonne erreur, pas de connexion pour rien). Le controle qui
	// compte est celui de la connexion (DialContext du client) et des redirections.
	if err := s.filtre.VerifierURL(ctx, u); err != nil {
		return nil, ErrRefuse
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, ErrJeton
	}
	// Aucun cookie, aucun identifiant : seulement ce qu'un navigateur anonyme enverrait.
	req.Header.Set("User-Agent", s.agent)
	if s.agent == "" {
		req.Header.Set("User-Agent", "Mozilla/5.0")
	}
	req.Header.Set("Accept", "image/*")
	resp, err := s.client.Do(req)
	if err != nil {
		// Le message d'une erreur reseau cite l'URL : on ne le propage ni ne le journalise.
		var ne net.Error
		s.log.Debug("image : echec amont", "delai", errors.As(err, &ne) && ne.Timeout(), "refus", errors.Is(err, hotes.ErrRefuse))
		return nil, ErrAmont
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		return nil, ErrIntrouvable
	case resp.StatusCode != http.StatusOK:
		return nil, ErrAmont
	}
	typeMime, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(typeMime, "image/") {
		return nil, ErrAmont // une page d'erreur HTML d'un portail captif n'est pas un logo
	}
	if resp.ContentLength > s.cfg.TailleMax {
		return nil, ErrAmont
	}
	corps, err := io.ReadAll(io.LimitReader(resp.Body, s.cfg.TailleMax+1))
	if err != nil || len(corps) == 0 || int64(len(corps)) > s.cfg.TailleMax {
		return nil, ErrAmont
	}
	return &donnees{typeMime: typeMime, corps: corps, modif: s.horloge()}, nil
}

// Octets est la taille actuelle du cache disque.
func (s *Service) Octets() int64 { return s.cache.octets() }

// Empreinte identifie les jetons que ce service emet : elle change avec le secret ou
// la liste des fournisseurs abreges. Les ETag du catalogue l'incluent, sinon un
// « 304 » ferait garder a un client des jetons que le serveur ne comprend plus.
func (s *Service) Empreinte() string {
	ids := make([]string, 0, len(s.base))
	for id, o := range s.base {
		ids = append(ids, id+"="+o)
	}
	sort.Strings(ids)
	h := sha256.Sum256([]byte(s.sceau.Sceller("e", strings.Join(ids, "|"))))
	return hex.EncodeToString(h[:8])
}
