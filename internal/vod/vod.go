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
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"mytv/internal/baux"
	"mytv/internal/config"
	"mytv/internal/domain"
	"mytv/internal/occupation"
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
	// ErrPause : le client n'a rien consomme pendant vod.pause_max (lecteur en pause) ;
	// la lecture est coupee pour rendre la place du fournisseur. Ce n'est pas une panne.
	ErrPause = errors.New("vod : lecture coupee, client inactif (pause)")
)

// StatutHTTP donne le statut a repondre pour une erreur de Servir survenue AVANT
// l'envoi des en-tetes. Les messages, eux, ne sont jamais ceux des erreurs :
// c'est a l'API d'ecrire un texte neutre.
func StatutHTTP(err error) int {
	var plage domain.ErrPlageHorsFichier
	switch {
	case err == nil:
		return http.StatusOK
	case errors.As(err, &plage):
		return http.StatusRequestedRangeNotSatisfiable
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
	PremierOctet       time.Duration // vod.premier_octet : borne l'attente du premier octet de ffmpeg ; 0 = aucune
	PauseMax           time.Duration // vod.pause_max : client inactif (lecteur en pause) au-dela, la lecture est coupee ; 0 = jamais
	UserAgent          string        // celui du fournisseur (informatif : c'est son client qui le pose sur chaque requete)
	// PatienceMax et Reessai : bail.patience_max / bail.reessai. Un 407 du fournisseur
	// (place encore gardee apres un seek ou un zapping) est retente toutes les Reessai
	// jusqu'a PatienceMax apres l'arrivee de la requete. PatienceMax 0 = aucune patience.
	PatienceMax time.Duration
	Reessai     time.Duration
	// ReessaiMax et Gigue : retrait progressif (bail.reessai_max, bail.gigue). 0 = pause constante.
	ReessaiMax time.Duration
	Gigue      float64
	// RelaisBrutHEVC : vod.relais_brut_hevc. Un client qui annonce `X-Mytv-Client: cast`
	// recoit tel quel un fichier HEVC que les navigateurs ne lisent pas.
	RelaisBrutHEVC bool
	// GraceAnnulation : vod.grace_annulation. Temps laisse a une lecture directe de la MEME
	// ressource pour finir d'elle-meme quand une nouvelle requete de la session arrive,
	// avant de l'annuler. 0 = annulation immediate (comportement d'avant).
	GraceAnnulation time.Duration
	// RetentionAdresse : vod.retention_adresse. Duree pendant laquelle la source du remux
	// retient l'adresse finale d'une redirection du fournisseur. 0 = jamais retenue.
	RetentionAdresse time.Duration
}

// ReglagesParDefaut : valeurs retenues apres les mesures de la plateforme actuelle.
func ReglagesParDefaut() Reglages {
	return Reglages{Tampon: 256 << 10, Reprises: 3, RenouvellementBail: 30 * time.Second, GraceAnnulation: 250 * time.Millisecond,
		RetentionAdresse: 10 * time.Minute}
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
	// Journal (facultatif) : demarrage, fin et ECHEC de chaque lecture, avec sa cause. Sans lui,
	// une lecture qui echoue repond 502 sans laisser la moindre trace (constate en recette).
	Journal *slog.Logger
	// Occupation (facultatif) diagnostique les 407 persistants d'une lecture.
	Occupation *occupation.Suivi
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
	log  *slog.Logger

	mu     sync.Mutex
	actifs map[string][]*lecture // par fournisseur + session
}

var _ domain.ServiceVOD = (*Service)(nil)

func Nouveau(dep Dependances, r Reglages) (*Service, error) {
	if r.Tampon < 1 || r.Reprises < 0 || r.RenouvellementBail <= 0 || r.PauseMax < 0 || r.PatienceMax < 0 || r.Reessai < 0 ||
		(r.PatienceMax > 0 && r.Reessai == 0) || r.GraceAnnulation < 0 || r.RetentionAdresse < 0 || r.PremierOctet < 0 {
		return nil, errors.New("vod : reglages invalides (tampon, reprises, renouvellement_bail, pause_max, patience_max, reessai, grace_annulation, retention_adresse, premier_octet)")
	}
	if dep.Stockage == nil || dep.Baux == nil || dep.Sondeur == nil || dep.Decideur == nil || dep.Lanceur == nil {
		return nil, errors.New("vod : dependance manquante")
	}
	s := &Service{r: r, dep: dep, srcs: map[string]source{}, inst: dep.Instrumentation,
		actifs: map[string][]*lecture{}}
	if s.inst == nil {
		s.inst = domain.SansInstrumentation{}
	}
	if s.log = dep.Journal; s.log == nil {
		s.log = slog.New(slog.NewTextHandler(io.Discard, nil))
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
	tete      bool // HEAD : ne coupe pas une lecture en cours de la meme ressource
	fini      chan struct{}
	bail      domain.Bail
}

func cleSession(fid, session string) string { return fid + "\x00" + session }

// cession : une lecture qui cede la place a la nouvelle. Sans grace elle est deja
// annulee ; avec grace elle a GraceAnnulation pour finir d'elle-meme avant d'etre annulee.
type cession struct {
	l     *lecture
	grace bool
}

// entrer enregistre la lecture et designe celles qui lui cedent la place. Le fournisseur
// n'autorise qu'UNE connexion, et il garde la place d'un client parti ~100 s : toute
// lecture precedente de la session qui tient encore sa connexion amont doit donc finir
// AVANT que la nouvelle ouvre la sienne, sinon le fournisseur repond 407.
//   - une autre ressource (zapping) cede tout de suite ;
//   - la MEME ressource cede aussi (seek : nouvelle plage en direct, nouveau ?t= en
//     remux) : une lecture directe bloquee par la contre-pression d'un lecteur qui a deja
//     saute ailleurs ne finirait jamais d'elle-meme. Mais une lecture DIRECTE a d'abord une
//     grace (vod.grace_annulation) pour finir seule : un navigateur qui lit un mp4 emet des
//     plages concurrentes legitimes (index en fin de fichier, position posee a
//     `loadedmetadata`, prechargement) ; les annuler a chaque fois donnait des rafales de
//     499, des reessais et un risque de MEDIA_ERR_NETWORK (meme regle que
//     remux/fluxsonde.attendreLaPrecedente). Un remux, lui, ne finit jamais seul : la grace
//     ne serait qu'une latence ajoutee au seek ;
//   - un HEAD ne coupe rien : il ne sert aucun octet.
//
// Renvoie les cessions, a attendre (attendreFin).
func (s *Service) entrer(cle string, l *lecture) []cession {
	s.mu.Lock()
	defer s.mu.Unlock()
	var cedent []cession
	for _, o := range s.actifs[cle] {
		switch {
		case o.ressource != l.ressource:
			o.annule()
			cedent = append(cedent, cession{l: o})
		case l.tete:
		case o.mode == domain.ModeDirect || o.mode == "":
			cedent = append(cedent, cession{l: o, grace: true})
		default:
			o.annule()
			cedent = append(cedent, cession{l: o})
		}
	}
	s.actifs[cle] = append(s.actifs[cle], l)
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

// attendreFin attend la fin des lectures qui cedent la place. Celles qui ont une grace
// disposent, toutes ensemble, de GraceAnnulation pour finir d'elles-memes ; au-dela elles
// sont annulees. Si c'est le nouveau client qui part pendant l'attente, rien n'est annule.
func (s *Service) attendreFin(ctx context.Context, cedent []cession) error {
	var limite <-chan time.Time
	for _, c := range cedent {
		if c.grace {
			if limite == nil {
				limite = time.After(s.r.GraceAnnulation)
			}
			select {
			case <-c.l.fini:
				continue
			case <-limite:
				c.l.annule()
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		select {
		case <-c.l.fini:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// ---------------------------------------------------------------- Servir

// infosLecture : ce que le client ne peut pas lire lui-meme. Un <video> ne voit ni
// l'en-tete Accept-Ranges ni la duree d'un flux remuxe (fabrique a la volee, sans
// longueur) : l'interface les lit ici, sur un HEAD, avant de lancer la lecture.
type infosLecture struct {
	mode  domain.Mode
	duree time.Duration // zero = inconnue
	brut  bool          // fichier servi tel quel (relais_brut_hevc) : type de contenu d'apres l'extension
	hevc  bool          // video HEVC (recopiee en remux : etiquetee hvc1)
}

// poser ecrit X-Mytv-Mode (direct, remux ou audio) et, si elle est connue,
// X-Mytv-Duree (secondes entieres, arrondies au plus proche). A appeler au moment
// d'envoyer les en-tetes : une erreur qui precede ne les porte pas.
func (i infosLecture) poser(h http.Header) {
	mode := "remux"
	switch i.mode {
	case domain.ModeDirect:
		mode = "direct"
	case domain.ModeAudioConverti:
		mode = "audio"
	}
	h.Set("X-Mytv-Mode", mode)
	if i.duree > 0 {
		h.Set("X-Mytv-Duree", strconv.FormatInt(int64((i.duree+time.Second/2)/time.Second), 10))
	}
}

// enteteClient : un lecteur qui sait lire le HEVC l'annonce. Aucune confiance n'est
// necessaire : le seul effet est de servir le fichier brut, que l'utilisateur authentifie
// peut de toute facon obtenir en relais direct.
const enteteClient = "X-Mytv-Client"

// hevcBrutDemande : vod.relais_brut_hevc actif, client « cast », et le refus du juge
// vient bien d'une video HEVC (les autres formats restent juges comme d'habitude).
func (s *Service) hevcBrutDemande(req *http.Request, err error) bool {
	if !s.r.RelaisBrutHEVC || !strings.EqualFold(strings.TrimSpace(req.Header.Get(enteteClient)), "cast") {
		return false
	}
	var f *remux.ErrFormat
	return errors.As(err, &f) && f.HEVC()
}

// traceEnvoi note si les en-tetes sont partis. Au-dela, une erreur ne peut plus devenir
// un statut : seule la coupure franche de la connexion dit au client que le film n'est
// PAS fini (voir Servir).
type traceEnvoi struct {
	http.ResponseWriter
	envoye bool
}

func (t *traceEnvoi) WriteHeader(c int)           { t.envoye = true; t.ResponseWriter.WriteHeader(c) }
func (t *traceEnvoi) Write(b []byte) (int, error) { t.envoye = true; return t.ResponseWriter.Write(b) }
func (t *traceEnvoi) Unwrap() http.ResponseWriter { return t.ResponseWriter }

// Servir ecrit la reponse. Contrat d'erreur : si l'erreur survient avant l'envoi
// des en-tetes, RIEN n'a ete ecrit et l'appelant repond StatutHTTP(err) ; apres
// l'envoi des en-tetes, la lecture qui echoue (coupure, pause, remplacement par un
// seek) ABANDONNE la reponse par panic(http.ErrAbortHandler) : net/http ferme la
// connexion sans terminateur, et Servir ne rend la main que lorsque le client est parti
// de lui-meme (son erreur est alors context.Canceled).
//
// POURQUOI la panique : la reponse d'un remux est en chunked (longueur inconnue). Un
// retour normal du gestionnaire ecrit le terminateur « 0\r\n\r\n » : le navigateur
// prend alors un film coupe (ffmpeg mort, amont perdu apres les reprises, pause depassee)
// pour un film TERMINE (evenement `ended`, pas d'`error`), et ne reprend jamais. C'est la
// regle deja appliquee a la source locale de ffmpeg (remux/fluxsonde.go, copier).
func (s *Service) Servir(w http.ResponseWriter, req *http.Request, session string, id domain.ContenuID) error {
	debut := time.Now()
	t, err := positionDemandee(req)
	if err != nil {
		return err
	}
	ctx, annule := context.WithCancel(req.Context())
	defer annule()
	// Qui lit quoi, pour le diagnostic d'occupation (la patience est plus bas, sans cette requete).
	ctx = context.WithValue(ctx, cleDemande{}, demande{session: session, ressource: string(id)})

	m, err := s.resoudre(ctx, id)
	if err != nil {
		return err
	}
	cle := cleSession(m.fid, session)
	l := &lecture{ressource: string(id), annule: annule, tete: req.Method == http.MethodHead, fini: make(chan struct{})}
	cedent := s.entrer(cle, l)
	defer s.sortir(cle, l)
	if err := s.attendreFin(ctx, cedent); err != nil {
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
	sonde, err := s.dep.Sondeur.Sonder(ctx, string(m.id), s.sourceAmont(m, url))
	if err != nil {
		s.inst.Compter("vod_erreurs_total", 1, "cause", "sonde")
		return err
	}
	dec, err := s.dep.Decideur(sonde)
	brut := false
	if err != nil && s.hevcBrutDemande(req, err) {
		// Un televiseur ou un Chromecast lit souvent le HEVC que le navigateur refuse :
		// le fichier est servi tel quel, sans jugement, sur demande explicite du client.
		dec, err, brut = domain.Decision{Mode: domain.ModeDirect, Raison: "hevc servi tel quel (client cast)"}, nil, true
		s.inst.Compter("vod_relais_brut_hevc_total", 1)
	}
	if err != nil {
		s.inst.Compter("vod_erreurs_total", 1, "cause", "non_lisible")
		return err
	}
	s.fixerMode(l, dec.Mode)
	s.inst.Evenement("vod:decision", dec.Raison)
	s.inst.Compter("vod_lectures_total", 1, "mode", string(dec.Mode))
	s.log.Info("lecture de film", "film", string(m.id), "mode", string(dec.Mode), "debut_s", int(t.Seconds()), "raison", dec.Raison)

	tw := &traceEnvoi{ResponseWriter: w}
	w = tw
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

	info := infosLecture{mode: dec.Mode, duree: sonde.Duree, brut: brut,
		hevc: strings.EqualFold(sonde.CodecVideo, "hevc") || strings.EqualFold(sonde.CodecVideo, "h265")}
	if dec.Mode == domain.ModeDirect {
		err = s.direct(ctx, w, req, m, url, debut, info)
	} else {
		err = s.remuxer(ctx, w, req, m, url, dec.Mode, t, debut, info)
	}
	if err == nil {
		return nil
	}
	pause := errors.Is(err, ErrPause)
	var horsFichier domain.ErrPlageHorsFichier
	switch {
	case pause:
		// Pas une panne : le lecteur est en pause depuis vod.pause_max. La place est
		// rendue au retour ; le journal le dit en clair, distinct d'une coupure.
		s.inst.Compter("vod_pauses_total", 1, "mode", string(dec.Mode))
		s.inst.Evenement("vod:pause", "lecture coupee, client inactif : place rendue")
		s.log.Info("lecture de film coupee (pause)", "film", string(m.id), "mode", string(dec.Mode), "debut_s", int(t.Seconds()),
			"ms", time.Since(debut).Milliseconds())
	case ctx.Err() == nil && !errors.As(err, &horsFichier): // un 416 est une erreur du client
		s.inst.Compter("vod_erreurs_total", 1, "cause", causeErreur(err))
		// La cause precise : sans elle l'utilisateur voit « format non lisible » ou un 502
		// muet. Les erreurs ne portent ni URL du fournisseur ni identifiants (voir xtream) ;
		// Assainir retire ce qu'un message de ffmpeg ou de transport pourrait charrier.
		s.log.Warn("lecture de film en echec", "film", string(m.id), "mode", string(dec.Mode), "debut_s", int(t.Seconds()),
			"cause", causeErreur(err), "ms", time.Since(debut).Milliseconds(), "erreur", remux.Assainir(err.Error()))
	default:
		s.log.Info("lecture de film interrompue", "film", string(m.id), "mode", string(dec.Mode), "ms", time.Since(debut).Milliseconds(),
			"raison", remux.Assainir(err.Error()))
	}
	// Reponse entamee et client toujours la (pause comprise : son contexte est annule par
	// net/http des qu'une ecriture echoue, mais c'est bien nous qui le coupons) : la reponse
	// est abandonnee pour qu'il voie une erreur reseau, jamais une fin de film.
	if tw.envoye && (pause || req.Context().Err() == nil) {
		panic(http.ErrAbortHandler)
	}
	if pause {
		return nil
	}
	return err
}

type cleDemande struct{}

type demande struct{ session, ressource string }

// Echecs transitoires a l'ouverture : 3 nouveaux essais, espaces de 250 ms puis 500 ms puis 750 ms.
const (
	essaisTransitoires = 3
	pauseTransitoire   = 250 * time.Millisecond
)

// requeteAmont fait UNE requete au fournisseur en patientant sur les 407 : c'est la
// patience unique du projet (baux.Patience), comptee depuis l'arrivee de la requete du
// client (`debut`) donc non cumulee avec l'attente d'une place par les baux. Elle sert
// au premier acces, a la reprise apres coupure et a la sonde.
//
// Les pauses s'espacent (retrait progressif) ; des le deuxieme refus l'occupation du
// compte est diagnostiquee, et la patience epuisee rend une erreur qui porte la
// classification (occupation.ErrOccupe).
func (s *Service) requeteAmont(ctx context.Context, f domain.Fournisseur, url string, entetes http.Header, debut time.Time) (*http.Response, error) {
	patience := baux.Patience{Max: s.r.PatienceMax, Reessai: s.r.Reessai, ReessaiMax: s.r.ReessaiMax, Gigue: s.r.Gigue}
	dem, _ := ctx.Value(cleDemande{}).(demande)
	d := occupation.Demande{Fournisseur: f.ID(), Session: dem.session, Ressource: dem.ressource}
	classe := occupation.Occupe
	transitoires := 0
	for refus := 0; ; {
		rep, err := f.Requete(ctx, url, entetes)
		if err == nil {
			s.dep.Occupation.Succes(d)
			return rep, nil
		}
		// Echec TRANSITOIRE a l'ouverture (5xx, coupure) : on retente quelques fois, vite,
		// comme TuliProx (jusqu'a 5 essais en 5 s, provider_stream_factory.rs:529-601).
		// Sans cela, un fournisseur qui vient de fermer une lecture et repond une erreur
		// passagere donnait un 502 immediat (constate en recette reelle, 1,3 s apres un
		// changement de position). Un 404, 401/403 ou 458 n'est pas transitoire. Ces essais
		// ne consomment pas la patience du 407.
		if errors.Is(err, domain.ErrAmont) && transitoires < essaisTransitoires && ctx.Err() == nil {
			transitoires++
			select {
			case <-time.After(time.Duration(transitoires) * pauseTransitoire):
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		if !errors.Is(err, domain.ErrRefuseConnexions) {
			return rep, err
		}
		if refus == 1 {
			classe = s.dep.Occupation.Diagnostiquer(ctx, d)
		}
		refus++
		if e := patience.Attendre(ctx, debut, err, refus-1, func() { s.inst.Compter("vod_patiences_407_total", 1) }); e != nil {
			if e == err { // patience epuisee (et non contexte annule)
				return nil, &occupation.ErrOccupe{Classe: classe, Err: err}
			}
			return nil, e
		}
	}
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

// DepuisConfig derive les reglages du service : remux.*, vod.* et bail.* (le bail est
// renouvele trois fois par TTL). agent est le User-Agent du fournisseur.
func DepuisConfig(c *config.Config, agent string) Reglages {
	return Reglages{
		Tampon: c.Remux.Tampon, Reprises: c.Remux.ReprisesMax,
		RenouvellementBail: c.Bail.TTL.Std() / 3, UserAgent: agent, PauseMax: c.Vod.PauseMax.Std(),
		PatienceMax: c.Bail.PatienceMax.Std(), Reessai: c.Bail.Reessai.Std(), ReessaiMax: c.Bail.ReessaiMax.Std(), Gigue: c.Bail.Gigue, RelaisBrutHEVC: c.Vod.RelaisBrutHEVC,
		GraceAnnulation: c.Vod.GraceAnnulation.Std(), RetentionAdresse: c.Vod.RetentionAdresse.Std(), PremierOctet: c.Vod.PremierOctet.Std(),
	}
}
