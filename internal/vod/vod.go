// Package vod sert les films et les episodes : relais d'octets avec Range quand le
// navigateur sait tout lire, remux ffmpeg sinon. Il remplace le service Python
// /opt/mytv-flux.
//
// Regles qui structurent le paquet :
//   - toute lecture tient UN bail du fournisseur (domain.Baux), pris avant la
//     moindre requete amont (sonde comprise) et rendu a la fin de la derniere
//     lecture de la session ;
//   - l'URL amont est toujours construite par le fournisseur a partir d'un
//     identifiant du catalogue : rien de ce que le client envoie n'y entre ;
//   - une lecture ne survit jamais a son client : annulation du contexte =
//     fermeture de la connexion amont et mort du processus ffmpeg.
package vod

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"mytv/internal/config"
	"mytv/internal/domain"
	"mytv/internal/remux"
)

var (
	// ErrReponseAmont : le fournisseur a repondu quelque chose d'inexploitable
	// (200 a la place de 206, Content-Range incoherent, longueur inconnue...). 502.
	ErrReponseAmont = errors.New("vod : reponse du fournisseur inexploitable")
	// ErrCoupure : la lecture s'est interrompue APRES l'envoi des en-tetes et n'a
	// pas pu etre reprise. La reponse est tronquee ; rien d'autre a envoyer.
	ErrCoupure = errors.New("vod : lecture interrompue")
	// ErrDemandeInvalide : parametre du client incorrect (t=...). 400.
	ErrDemandeInvalide = errors.New("vod : demande invalide")
)

// StatutHTTP donne le statut a repondre pour une erreur de Servir survenue AVANT
// l'envoi des en-tetes. Les messages, eux, ne sont jamais ceux des erreurs :
// c'est a l'API d'ecrire un texte neutre.
func StatutHTTP(err error) int {
	switch {
	case err == nil:
		return http.StatusOK
	case errors.Is(err, context.Canceled):
		return 499 // client parti : personne ne lira la reponse
	case errors.Is(err, domain.ErrNonTrouve), errors.Is(err, domain.ErrIntrouvable):
		return http.StatusNotFound
	case errors.Is(err, ErrDemandeInvalide):
		return http.StatusBadRequest
	case errors.Is(err, domain.ErrNonLisible):
		return http.StatusUnsupportedMediaType
	case errors.Is(err, domain.ErrPlaceIndisponible), errors.Is(err, domain.ErrRefuseConnexions), errors.Is(err, remux.ErrSaturation):
		return http.StatusServiceUnavailable
	case errors.Is(err, domain.ErrRefuseIP), errors.Is(err, domain.ErrIdentifiants), errors.Is(err, domain.ErrAmont),
		errors.Is(err, ErrReponseAmont), errors.Is(err, ErrCoupure), errors.Is(err, context.DeadlineExceeded):
		return http.StatusBadGateway
	}
	return http.StatusInternalServerError
}

// Reglages du service (config.Remux et config.Bail).
type Reglages struct {
	Tampon             int           // remux.tampon : taille du tampon de copie
	Reprises           int           // remux.reprises_max : reprises Range d'un relais coupe
	RenouvellementBail time.Duration // bail.ttl / 3 : le bail est renouvele tant que la lecture dure
	PremierOctet       time.Duration // borne l'attente du premier octet de ffmpeg ; 0 = aucune (pas encore dans la config)
	UserAgent          string        // celui du fournisseur : il refuse ce qui ne ressemble pas a un navigateur
}

// ReglagesParDefaut : valeurs retenues apres les mesures de la plateforme actuelle.
func ReglagesParDefaut() Reglages {
	return Reglages{Tampon: 256 << 10, Reprises: 3, RenouvellementBail: 30 * time.Second}
}

// Dependances : tout ce que le service utilise est une interface.
type Dependances struct {
	Fournisseurs    map[string]domain.Fournisseur // chacun doit aussi etre un domain.FournisseurVOD
	Stockage        domain.StockageVideo
	Baux            domain.Baux
	Sondeur         domain.Sondeur
	Decideur        domain.Decideur
	Lanceur         domain.Lanceur
	Instrumentation domain.Instrumentation // nil = aucune mesure
}

type source struct {
	f   domain.Fournisseur
	vod domain.FournisseurVOD
}

// Service implemente domain.ServiceVOD.
type Service struct {
	r    Reglages
	dep  Dependances
	srcs map[string]source
	inst domain.Instrumentation

	mu     sync.Mutex
	actifs map[string][]*lecture // par fournisseur + session
}

var _ domain.ServiceVOD = (*Service)(nil)

func Nouveau(dep Dependances, r Reglages) (*Service, error) {
	if r.Tampon < 1 || r.Reprises < 0 || r.RenouvellementBail <= 0 {
		return nil, errors.New("vod : reglages invalides (tampon, reprises, renouvellement_bail)")
	}
	if dep.Stockage == nil || dep.Baux == nil || dep.Sondeur == nil || dep.Decideur == nil || dep.Lanceur == nil {
		return nil, errors.New("vod : dependance manquante")
	}
	s := &Service{r: r, dep: dep, srcs: map[string]source{}, inst: dep.Instrumentation,
		actifs: map[string][]*lecture{}}
	if s.inst == nil {
		s.inst = domain.SansInstrumentation{}
	}
	for id, f := range dep.Fournisseurs {
		v, ok := f.(domain.FournisseurVOD)
		if !ok {
			return nil, fmt.Errorf("vod : le fournisseur %q ne sert ni films ni series", id)
		}
		s.srcs[id] = source{f: f, vod: v}
	}
	return s, nil
}

type media struct {
	id        domain.ContenuID
	fid       string
	numero    string
	extension string
	episode   bool
	src       source
}

// url : construite par le fournisseur, jamais par la requete.
func (m *media) url() string {
	if m.episode {
		return m.src.vod.URLEpisode(m.numero, m.extension)
	}
	return m.src.vod.URLFilm(m.numero, m.extension)
}

var (
	reNumero = regexp.MustCompile(`^[0-9]{1,20}$`)
	// extensionsEpisode : liste blanche. L'extension d'un episode fait partie de son
	// identifiant (« iptva:77123.mkv ») et finit dans une URL amont puis dans un
	// argument ffmpeg : rien d'autre que ces valeurs n'est accepte.
	extensionsEpisode = map[string]bool{"mp4": true, "mkv": true, "avi": true, "mov": true, "m4v": true,
		"ts": true, "webm": true, "mpg": true, "mpeg": true, "flv": true, "wmv": true}
)

// resoudre retrouve ce qu'il faut pour construire l'URL : le catalogue pour un
// film, l'identifiant lui-meme (strictement valide) pour un episode.
func (s *Service) resoudre(ctx context.Context, id domain.ContenuID) (*media, error) {
	fid, reste, ok := strings.Cut(string(id), ":")
	src, connu := s.srcs[fid]
	if !ok || !connu {
		return nil, domain.ErrNonTrouve
	}
	if numero, ext, episode := strings.Cut(reste, "."); episode {
		if !reNumero.MatchString(numero) || !extensionsEpisode[ext] {
			return nil, domain.ErrNonTrouve
		}
		return &media{id: id, fid: fid, numero: numero, extension: ext, episode: true, src: src}, nil
	}
	if !reNumero.MatchString(reste) {
		return nil, domain.ErrNonTrouve
	}
	film, err := s.dep.Stockage.Film(ctx, id)
	if err != nil {
		return nil, err
	}
	return &media{id: id, fid: fid, numero: film.Numero, extension: film.Extension, src: src}, nil
}

// ---------------------------------------------------------------- lectures actives

// lecture est une requete en cours. Les lectures d'une meme session partagent le
// bail du fournisseur ; il n'est rendu que lorsque la derniere se termine.
type lecture struct {
	ressource string
	mode      domain.Mode // vide tant que la sonde n'a pas decide
	annule    context.CancelFunc
	fini      chan struct{}
	bail      domain.Bail
}

func cleSession(fid, session string) string { return fid + "\x00" + session }

// entrer enregistre la lecture et annule celles qui lui cedent la place :
// une autre ressource de la meme session (zapping) — le fournisseur n'autorise
// qu'une connexion. Renvoie les lectures annulees, a attendre.
func (s *Service) entrer(cle string, l *lecture) []*lecture {
	s.mu.Lock()
	defer s.mu.Unlock()
	var cedent []*lecture
	for _, o := range s.actifs[cle] {
		if o.ressource != l.ressource {
			o.annule()
			cedent = append(cedent, o)
		}
	}
	s.actifs[cle] = append(s.actifs[cle], l)
	return cedent
}

// remplacer annule les remux deja en cours de la meme ressource : un seek ouvre
// une nouvelle requete, et deux ffmpeg sur le meme titre feraient deux connexions
// amont chez un fournisseur qui n'en accorde qu'une.
func (s *Service) remplacer(cle string, l *lecture) []*lecture {
	s.mu.Lock()
	defer s.mu.Unlock()
	var cedent []*lecture
	for _, o := range s.actifs[cle] {
		if o != l && o.ressource == l.ressource && o.mode != domain.ModeDirect {
			o.annule()
			cedent = append(cedent, o)
		}
	}
	return cedent
}

func (s *Service) fixerMode(l *lecture, m domain.Mode) {
	s.mu.Lock()
	l.mode = m
	s.mu.Unlock()
}

// sortir retire la lecture ; le bail est rendu si c'etait la derniere.
func (s *Service) sortir(cle string, l *lecture) {
	s.mu.Lock()
	reste := s.actifs[cle][:0:0]
	for _, o := range s.actifs[cle] {
		if o != l {
			reste = append(reste, o)
		}
	}
	if len(reste) == 0 {
		delete(s.actifs, cle)
	} else {
		s.actifs[cle] = reste
	}
	derniere := len(reste) == 0
	s.mu.Unlock()
	if derniere && l.bail != nil {
		l.bail.Liberer()
	}
	close(l.fini)
}

func attendreFin(ctx context.Context, ls []*lecture) error {
	for _, o := range ls {
		select {
		case <-o.fini:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// ---------------------------------------------------------------- Servir

func (s *Service) entetesAmont() http.Header {
	if s.r.UserAgent == "" {
		return nil
	}
	return http.Header{"User-Agent": {s.r.UserAgent}}
}

// Servir ecrit la reponse. Contrat d'erreur : si l'erreur survient avant l'envoi
// des en-tetes, RIEN n'a ete ecrit et l'appelant repond StatutHTTP(err) ; apres
// l'envoi des en-tetes, l'erreur est ErrCoupure (ou celle du client parti) et la
// reponse est deja tronquee.
func (s *Service) Servir(w http.ResponseWriter, req *http.Request, session string, id domain.ContenuID) error {
	debut := time.Now()
	t, err := positionDemandee(req)
	if err != nil {
		return err
	}
	ctx, annule := context.WithCancel(req.Context())
	defer annule()

	m, err := s.resoudre(ctx, id)
	if err != nil {
		return err
	}
	cle := cleSession(m.fid, session)
	l := &lecture{ressource: string(id), annule: annule, fini: make(chan struct{})}
	cedent := s.entrer(cle, l)
	defer s.sortir(cle, l)
	if err := attendreFin(ctx, cedent); err != nil {
		return err
	}

	// Le bail d'abord : la sonde elle-meme occupe la connexion du fournisseur.
	bail, err := s.dep.Baux.Acquerir(ctx, m.fid, session, string(id))
	if err != nil {
		s.inst.Compter("vod_erreurs_total", 1, "cause", "bail")
		return err
	}
	l.bail = bail
	fin := s.renouveler(ctx, bail)
	defer fin()

	url := m.url()
	sonde, err := s.dep.Sondeur.Sonder(ctx, url, s.entetesAmont())
	if err != nil {
		s.inst.Compter("vod_erreurs_total", 1, "cause", "sonde")
		return err
	}
	dec, err := s.dep.Decideur(sonde)
	if err != nil {
		s.inst.Compter("vod_erreurs_total", 1, "cause", "non_lisible")
		return err
	}
	s.fixerMode(l, dec.Mode)
	s.inst.Evenement("vod:decision", dec.Raison)
	s.inst.Compter("vod_lectures_total", 1, "mode", string(dec.Mode))

	// Un flux plus long que le delai d'ecriture du serveur ne doit pas etre coupe...
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})
	// ...mais une lecture annulee (zapping, seek) doit pouvoir l'etre meme si son
	// client ne lit plus : une ecriture bloquee sur un client a l'arret ne verrait
	// jamais l'annulation, et la lecture suivante attendrait pour toujours.
	// Cette goroutine ne touche pas `w` apres le retour de Servir (on l'attend).
	fait := make(chan struct{})
	arret := context.AfterFunc(ctx, func() {
		defer close(fait)
		_ = rc.SetWriteDeadline(time.Now())
	})
	defer func() {
		if !arret() {
			<-fait
		}
	}()

	if dec.Mode == domain.ModeDirect {
		err = s.direct(ctx, w, req, m, url, debut)
	} else {
		if err := attendreFin(ctx, s.remplacer(cle, l)); err != nil {
			return err
		}
		err = s.remuxer(ctx, w, req, url, dec.Mode, t, debut)
	}
	if err != nil && ctx.Err() == nil {
		s.inst.Compter("vod_erreurs_total", 1, "cause", causeErreur(err))
	}
	return err
}

func causeErreur(err error) string {
	switch {
	case errors.Is(err, ErrCoupure):
		return "coupure"
	case errors.Is(err, ErrReponseAmont):
		return "reponse_amont"
	case errors.Is(err, remux.ErrSaturation):
		return "saturation"
	}
	return "autre"
}

// renouveler garde le bail vivant pendant la lecture (un film en pause ne
// renouvelle rien, mais sa connexion est ouverte). La fonction rendue arrete et
// attend la goroutine.
func (s *Service) renouveler(ctx context.Context, b domain.Bail) (fin func()) {
	stop := make(chan struct{})
	fini := make(chan struct{})
	go func() {
		defer close(fini)
		t := time.NewTicker(s.r.RenouvellementBail)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				b.Renouveler()
			case <-stop:
				return
			case <-ctx.Done():
				return
			}
		}
	}()
	return func() { close(stop); <-fini }
}

// DepuisConfig derive les reglages du service : remux.* et bail.ttl (le bail est
// renouvele trois fois par TTL). agent est le User-Agent du fournisseur.
func DepuisConfig(c *config.Config, agent string) Reglages {
	return Reglages{
		Tampon: c.Remux.Tampon, Reprises: c.Remux.ReprisesMax,
		RenouvellementBail: c.Bail.TTL.Std() / 3, UserAgent: agent,
	}
}
