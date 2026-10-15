package domain

// Contrats du lot 2 : comptes, EPG, etat par profil, VOD/remux, telemetrie.
//
// Principe commun : chaque dependance externe (horloge, ffmpeg, fichiers, reseau)
// est une interface injectee, pour que les tests n'aient besoin ni de vrai
// processus, ni de vraie horloge, ni de vrai fournisseur.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"time"
)

// Horloge est la source de temps injectable. Les paquets du lot 1 ont la leur
// (meme forme) ; les nouveaux utilisent celle-ci.
type Horloge interface {
	Maintenant() time.Time
}

// ---------------------------------------------------------------- comptes

// Utilisateur est un compte de connexion. Il peut porter plusieurs profils.
type Utilisateur struct {
	ID    string
	Nom   string // identifiant de connexion, unique, insensible a la casse
	Admin bool   // seul un admin lit /etat, la telemetrie et le diagnostic
	Cree  time.Time
}

// Profil est une personne derriere un compte (favoris, historique, reglages
// propres). L'etat par profil (EtatProfil) est indexe par Profil.ID.
type Profil struct {
	ID            string
	UtilisateurID string
	Nom           string
}

// Session est une connexion authentifiee. ID est un identifiant STABLE et SANS
// valeur d'authentification (jamais le cookie) : c'est lui qui sert de « session »
// dans Baux.Acquerir, dans les journaux et dans Terminateur.Terminer.
type Session struct {
	ID            string
	UtilisateurID string
	Nom           string // nom du compte (affichage), pas un identifiant
	ProfilID      string // profil actif ; vide tant qu'aucun n'est choisi
	Admin         bool
	Expire        time.Time
}

var (
	// ErrIdentifiantsInvalides : utilisateur inconnu OU mauvais mot de passe. Un
	// seul message pour les deux, afin de ne pas permettre l'enumeration de comptes.
	ErrIdentifiantsInvalides = errors.New("comptes : identifiants invalides")
	// ErrTropDeTentatives : limitation de tentatives atteinte (HTTP 429).
	ErrTropDeTentatives = errors.New("comptes : trop de tentatives, reessayer plus tard")
	// ErrSessionInvalide : jeton inconnu, expire ou revoque (HTTP 401).
	ErrSessionInvalide = errors.New("comptes : session invalide ou expiree")
	// ErrExiste : nom d'utilisateur ou de profil deja pris.
	ErrExiste = errors.New("comptes : existe deja")
)

// Comptes gere utilisateurs, profils et sessions. Les jetons de session sont des
// secrets : seuls Connecter les renvoie, et ils ne sont stockes que hachees.
type Comptes interface {
	// Connecter verifie les identifiants en temps constant et ouvre une session.
	// adresse sert a la limitation de tentatives (par adresse ET par compte). Renvoie
	// le jeton opaque (>= 128 bits) a mettre dans le cookie.
	Connecter(ctx context.Context, nom, motDePasse, adresse string) (jeton string, s *Session, err error)
	// Session retrouve la session d'un jeton ; ErrSessionInvalide sinon.
	Session(ctx context.Context, jeton string) (*Session, error)
	// Deconnecter revoque la session du jeton. Doit aussi appeler Terminateur.Terminer
	// (place de connexion rendue tout de suite).
	Deconnecter(ctx context.Context, jeton string) error
	// ChoisirProfil change le profil actif d'une session (le profil doit etre
	// celui de l'utilisateur de la session).
	ChoisirProfil(ctx context.Context, sessionID, profilID string) error

	CreerUtilisateur(ctx context.Context, nom, motDePasse string, admin bool) (*Utilisateur, error)
	// ChangerMotDePasse verifie l'ancien et revoque TOUTES les autres sessions de
	// l'utilisateur (sessionCouranteID est conservee).
	ChangerMotDePasse(ctx context.Context, utilisateurID, ancien, nouveau, sessionCouranteID string) error

	Profils(ctx context.Context, utilisateurID string) ([]Profil, error)
	CreerProfil(ctx context.Context, utilisateurID, nom string) (*Profil, error)
	SupprimerProfil(ctx context.Context, utilisateurID, profilID string) error
}

// Terminateur est le signal « cette session n'est plus la » : l'implementation
// libere tous les baux de la session. Implemente par internal/baux (et peut
// l'etre par le relais) ; injecte dans Comptes. Idempotent, sans erreur.
type Terminateur interface {
	Terminer(session string)
}

// ---------------------------------------------------------------- EPG

// Programme est une emission d'une chaine (identifiee par l'epg_id du fournisseur).
type Programme struct {
	FournisseurID string
	EpgID         string
	Debut, Fin    time.Time // toujours en UTC en interne ; le fuseau d'affichage est celui du client
	Titre         string
	Description   string
	Categorie     string
	// Facultatifs (US-29) : vides quand le guide ne les donne pas.
	SousTitre string // <sub-title>
	Episode   string // numero lisible, ex. « S02E05 » (<episode-num>)
	Annee     string // <date>, 4 chiffres
}

// LogoEPG est le logo d'un canal du guide (<icon src>), deja rapproche a une chaine.
// Il ne sert que de REPLI : un logo donne par le fournisseur n'est jamais ecrase.
type LogoEPG struct {
	Chaine ContenuID
	Logo   string
}

// Methode de rapprochement d'une chaine avec un epg_id.
type MethodeRapprochement string

const (
	RapprocheParEpgID MethodeRapprochement = "epg_id"
	RapprocheParNom   MethodeRapprochement = "nom"
)

// Rapprochement lie une chaine a un epg_id. Ambigu = plusieurs candidats : on le
// signale et on ne devine pas (Chaine ne recoit alors aucun programme).
type Rapprochement struct {
	Chaine  ContenuID
	EpgID   string // vide si ambigu ou introuvable
	Methode MethodeRapprochement
	Ambigu  bool
}

// InstantaneEPG est l'EPG complet d'UN fournisseur a un instant donne.
type InstantaneEPG struct {
	FournisseurID  string
	Pris           time.Time
	Programmes     []Programme
	Rapprochements []Rapprochement
	// Logos : repli pour les chaines sans logo du fournisseur (jamais pour une
	// chaine au rapprochement ambigu).
	Logos []LogoEPG
}

// SourceEPG est implemente par les Fournisseur qui publient un guide XMLTV ; le
// telechargement passe par Fournisseur.Requete (identifiants, redirections, delais).
type SourceEPG interface {
	URLEPG() string
}

// EPGStockage persiste l'EPG. Comme Stockage.EcrireCatalogue, l'ecriture remplace
// ATOMIQUEMENT l'EPG du fournisseur (echec = ancien EPG intact).
type EPGStockage interface {
	EcrireEPG(ctx context.Context, e *InstantaneEPG) error
	DerniereSynchroEPG(ctx context.Context, fournisseurID string) (time.Time, error) // zero si jamais
	// EnCoursEtSuivant renvoie le programme en cours a l'instant donne et le suivant
	// (nil si aucun). La chaine est resolue via ses Rapprochements.
	EnCoursEtSuivant(ctx context.Context, chaine ContenuID, a time.Time) (courant, suivant *Programme, err error)
	// Grille renvoie, par chaine, les programmes qui chevauchent [de, a[, ordonnes par debut.
	Grille(ctx context.Context, chaines []ContenuID, de, a time.Time) (map[ContenuID][]Programme, error)
	// LogosEPG renvoie le logo du guide des chaines demandees qui en ont un
	// (chaines == nil : toutes).
	LogosEPG(ctx context.Context, chaines []ContenuID) (map[ContenuID]string, error)
}

// ---------------------------------------------------------------- etat par profil

// Reglages sont les preferences d'un profil, synchronisees entre appareils.
// Revision sert a l'ecriture optimiste : deux appareils qui ecrivent en meme temps
// ne s'ecrasent pas en silence.
type Reglages struct {
	Valeurs  map[string]string
	Revision int64
	Modifie  time.Time // zero si jamais ecrit
}

// ErrConflit : EcrireReglages avec une revision perimee.
var ErrConflit = errors.New("profil : reglages modifies entre-temps")

// ErrLimite : favoris, historique ou reglages au-dela des bornes de config.Profil
// (l'API la traduit en 413/422).
var ErrLimite = errors.New("profil : limite atteinte")

// Lecture est une entree d'historique : ce qu'on regarde et ou on en est.
type Lecture struct {
	Contenu ContenuID
	// Serie est la serie parente quand Contenu est un EPISODE (les episodes ne sont
	// pas dans le stockage) : la disparition de la serie rend l'entree orpheline.
	Serie    ContenuID
	Position time.Duration // point de reprise
	Duree    time.Duration // 0 si inconnue (direct)
	Vu       time.Time
}

// EtatProfil est l'etat propre a un profil. Toutes les methodes sont limitees au
// profil passe : l'appelant (API) a deja verifie qu'il appartient a la session.
type EtatProfil interface {
	// AjouterFavori / RetirerFavori sont idempotents.
	AjouterFavori(ctx context.Context, profilID string, c ContenuID) error
	RetirerFavori(ctx context.Context, profilID string, c ContenuID) error
	// Favoris dans l'ordre d'ajout (stable). Les identifiants orphelins (contenu
	// disparu apres une resynchronisation) sont ignores.
	Favoris(ctx context.Context, profilID string) ([]ContenuID, error)

	// EnregistrerLecture met a jour (ou cree) l'entree du contenu ; l'historique
	// est borne et purge des plus anciennes.
	EnregistrerLecture(ctx context.Context, profilID string, l Lecture) error
	// Historique du plus recent au plus ancien, limite <= 0 = tout.
	Historique(ctx context.Context, profilID string, limite int) ([]Lecture, error)
	OublierLecture(ctx context.Context, profilID string, c ContenuID) error

	Reglages(ctx context.Context, profilID string) (Reglages, error)
	// EcrireReglages remplace les valeurs si rev == Revision courante, sinon ErrConflit.
	// Renvoie les reglages a jour (nouvelle revision).
	EcrireReglages(ctx context.Context, profilID string, valeurs map[string]string, rev int64) (Reglages, error)
}

// ---------------------------------------------------------------- VOD / remux

// StockageVideo complete Stockage pour les films et series. Implemente par le meme
// stockage SQLite. Le filtre FournisseurID s'applique comme pour les chaines.
type StockageVideo interface {
	Films(ctx context.Context, f FiltreChaines) ([]Film, error)
	Film(ctx context.Context, id ContenuID) (*Film, error) // ErrNonTrouve si absent
	Series(ctx context.Context, f FiltreChaines) ([]SerieTV, error)
	Serie(ctx context.Context, id ContenuID) (*SerieTV, error)
}

// Episode / Saison viennent de get_series_info.
type Episode struct {
	// ID = « <fournisseur>:<id episode>.<extension> » (ex. « iptva:77123.mkv »). L'extension
	// est dans l'identifiant pour que Servir fonctionne sans memoire (redemarrage).
	ID        ContenuID
	Numero    int
	Titre     string
	Extension string
	Duree     time.Duration
	// Facultatifs (lot 4, US-28) : tires de get_series_info, vides si le fournisseur se tait.
	Synopsis string
	Image    string
	Sortie   string
	Note     float64
}

type Saison struct {
	Numero   int
	Episodes []Episode
}

// FournisseurVOD est implemente par les Fournisseur qui servent films et series.
// Les URL sont construites ICI a partir d'identifiants du catalogue : aucune URL
// fournie par un utilisateur n'atteint le fournisseur ni ffmpeg.
type FournisseurVOD interface {
	URLFilm(numero, extension string) string
	URLEpisode(numero, extension string) string
	Saisons(ctx context.Context, numeroSerie string) ([]Saison, error)
}

// Sonde est ce que ffprobe a trouve dans un fichier.
type Sonde struct {
	Conteneur  string // « mov,mp4,m4a... » / « matroska,webm »
	CodecVideo string // h264, hevc...
	CodecAudio string // aac, eac3...
	Duree      time.Duration
}

// Sondeur analyse un contenu (ffprobe). L'implementation met le resultat en cache
// `remux.duree_sonde` : appeler Sonder souvent ne relance pas ffprobe.
//
// `cle` identifie le contenu pour le cache (son ContenuID : jamais une URL). `src`
// donne ses octets, lus par le SERVEUR avec les identifiants du fournisseur : ffprobe,
// lui, ne recoit aucune adresse du fournisseur (voir SourceOctets).
type Sondeur interface {
	Sonder(ctx context.Context, cle string, src SourceOctets) (*Sonde, error)
}

// Mode de lecture d'un film.
type Mode string

const (
	ModeDirect        Mode = "direct"         // relais d'octets avec Range, rien a convertir
	ModeRemux         Mode = "remux"          // autre conteneur, flux copies (-c copy) en mp4 fragmente
	ModeAudioConverti Mode = "audio_converti" // video copiee, audio re-encode en AAC
)

// Decision est le choix fait a partir d'une Sonde.
type Decision struct {
	Mode   Mode
	Raison string // lisible, pour le diagnostic (sans URL)
}

// Decideur choisit le mode de lecture pour un navigateur. Fonction pure, testable
// sur une table de cas (mp4/h264/aac -> direct ; mkv/h264/aac -> remux ;
// eac3/ac3/dts -> audio converti ; hevc non lisible -> erreur ou transcodage).
//
// ErrNonLisible : aucun mode ne convient (ex. HEVC sans transcodage video) ; l'API
// repond 415.
type Decideur func(*Sonde) (Decision, error)

var ErrNonLisible = errors.New("vod : format non lisible dans un navigateur")

// SourceOctets ouvre les octets d'un film a partir de l'octet `depuis` (jusqu'a la
// fin). Le SERVEUR lit l'amont (identifiants, redirections, filtre d'hotes, Range) et
// les donne a ffmpeg par son entree standard : ni URL ni identifiants dans la ligne de
// commande du processus (visible de tout utilisateur du systeme par /proc et `ps`).
// Une lecture qui se termine avant la fin renvoie io.ErrUnexpectedEOF : l'appelant
// peut alors rouvrir a l'octet suivant (reprise).
type SourceOctets func(ctx context.Context, depuis int64) (io.ReadCloser, error)

// AvecTaille est implemente par les lecteurs rendus par une SourceOctets qui
// connaissent la taille TOTALE du fichier (pas celle de ce qui reste a lire). Le
// sondeur en a besoin pour repondre aux requetes d'intervalle de ffprobe.
type AvecTaille interface {
	Taille() int64
}

// TacheRemux decrit CE QUE ffmpeg doit faire, jamais COMMENT : l'implementation du
// Lanceur construit elle-meme les arguments a partir de ces champs valides.
// C'est le seul point ou des arguments ffmpeg sont fabriques (pas d'injection).
type TacheRemux struct {
	Source SourceOctets  // obligatoire : voir SourceOctets
	Mode   Mode          // ModeRemux ou ModeAudioConverti
	Debut  time.Duration // positionnement (seek) ; >= 0
}

// Processus est un ffmpeg en cours. Sortie est le flux mp4 fragmente.
type Processus interface {
	Sortie() io.ReadCloser
	// Arreter tue le processus et le recolte (pas de zombie) ; idempotent.
	Arreter()
	// Attendre bloque jusqu'a la fin et renvoie l'erreur du processus (nil si normale
	// ou si Arreter a ete appele).
	Attendre() error
}

// Lanceur demarre ffmpeg. Doublure de test : un faux script ou un faux Processus.
type Lanceur interface {
	Lancer(ctx context.Context, t TacheRemux) (Processus, error)
}

// ServiceVOD est ce que l'API appelle pour lire un film ou un episode.
type ServiceVOD interface {
	// Servir ecrit la reponse (Range gere). session identifie le bail fournisseur.
	Servir(w http.ResponseWriter, req *http.Request, session string, id ContenuID) error
}

// ---------------------------------------------------------------- telemetrie

// Instrumentation est ce que les modules appellent pour publier des mesures. Les
// modules dependent de cette interface, jamais de internal/telemetrie. Les
// etiquettes sont des paires cle, valeur (nombre pair) a cardinalite BORNEE : pas
// d'identifiant de session, d'utilisateur ni d'URL.
type Instrumentation interface {
	Compter(nom string, delta float64, etiquettes ...string)
	Observer(nom string, valeur float64, etiquettes ...string) // histogramme (secondes, octets...)
	// Evenement alimente le diagnostic recent. Aucun secret, identifiant ni URL.
	Evenement(type_, message string)
}

// SansInstrumentation ne fait rien : valeur par defaut, cout nul.
type SansInstrumentation struct{}

func (SansInstrumentation) Compter(string, float64, ...string)  {}
func (SansInstrumentation) Observer(string, float64, ...string) {}
func (SansInstrumentation) Evenement(string, string)            {}

var _ Instrumentation = SansInstrumentation{}
