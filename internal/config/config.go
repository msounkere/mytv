// Package config charge et valide la configuration de mytv.
//
// Principe : tout ce qui est un REGLAGE (delais, durees, tailles, fournisseurs,
// frequences de rafraichissement) vit dans le fichier de configuration, avec une
// valeur par defaut sensee ; le code ne contient aucun chiffre magique de ce genre.
// Les SECRETS ne sont jamais dans le fichier : il y reference une variable
// d'environnement ou un fichier (« mot_de_passe_env », « mot_de_passe_fichier »).
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duree est une duree ecrite « 90s », « 5m », « 24h » dans le fichier.
type Duree time.Duration

func (d *Duree) UnmarshalYAML(n *yaml.Node) error {
	var s string
	if err := n.Decode(&s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("duree %q invalide (exemples : 800ms, 90s, 5m, 24h)", s)
	}
	if v < 0 {
		return fmt.Errorf("duree %q negative", s)
	}
	*d = Duree(v)
	return nil
}

func (d Duree) Std() time.Duration { return time.Duration(d) }

type Config struct {
	Serveur      Serveur       `yaml:"serveur"`
	Stockage     Stockage      `yaml:"stockage"`
	Fournisseurs []Fournisseur `yaml:"fournisseurs"`
	Live         Live          `yaml:"live"`
	Bail         Bail          `yaml:"bail"`
	Comptes      Comptes       `yaml:"comptes"`
	Remux        Remux         `yaml:"remux"`
	Diagnostic   Diagnostic    `yaml:"diagnostic"`
	Journal      Journal       `yaml:"journal"`
	API          API           `yaml:"api"`
	Profil       Profil        `yaml:"profil"`
	EPG          EPG           `yaml:"epg"`
	Vod          Vod           `yaml:"vod"`
	Images       Images        `yaml:"images"`
	Supervision  Supervision   `yaml:"supervision"`
	Sauvegarde   Sauvegarde    `yaml:"sauvegarde"`
	Metadonnees  Metadonnees   `yaml:"metadonnees"`
}

// Sauvegarde regle la sauvegarde quotidienne de la base (US-26). Le service n'est pas
// arrete : la copie est un instantane coherent (VACUUM INTO) verifie par integrity_check.
type Sauvegarde struct {
	// Heure : « 03:30 » (heure locale du serveur), vide = pas de sauvegarde planifiee
	// (`mytv --sauvegarder` reste utilisable).
	Heure string `yaml:"heure"`
	// Dossier des sauvegardes (cree avec les droits 700). Vide : « sauvegardes » a cote de la base.
	Dossier string `yaml:"dossier"`
	// Conservation : jours pendant lesquels une sauvegarde est gardee (la plus recente l'est toujours).
	Conservation int `yaml:"conservation"`
}

// DossierSauvegardes rend le dossier effectif des sauvegardes.
func (c *Config) DossierSauvegardes() string {
	if c.Sauvegarde.Dossier != "" {
		return c.Sauvegarde.Dossier
	}
	return filepath.Join(filepath.Dir(c.Stockage.Fichier), "sauvegardes")
}

// Images regle le proxy d'images (US-23) : les logos et affiches du fournisseur sont
// servis par ce service, signes, avec un cache disque borne.
type Images struct {
	CacheDossier string `yaml:"cache_dossier"` // dossier du cache (cree, droits 700)
	CacheOctets  int64  `yaml:"cache_octets"`  // taille maximale du cache disque
	// CacheValidite : age au-dela duquel une image du cache est retelechargee (un
	// picon qui change chez le fournisseur finit par etre vu).
	CacheValidite Duree `yaml:"cache_validite"`
	Delai         Duree `yaml:"delai"`      // borne d'un telechargement amont
	TailleMax     int64 `yaml:"taille_max"` // octets d'une image ; au-dela, refusee
	// ConcurrenceMax : telechargements amont simultanes. Les picons ne prennent pas de
	// bail de connexion mais ne doivent pas saturer le fournisseur ni le reseau.
	ConcurrenceMax int `yaml:"concurrence_max"`
	// CacheNegatif : duree pendant laquelle une image absente ou en erreur n'est pas
	// redemandee au fournisseur (0 = pas de cache negatif).
	CacheNegatif    Duree `yaml:"cache_negatif"`
	CacheNegatifMax int   `yaml:"cache_negatif_max"` // entrees gardees (memoire bornee)
	// RedirectionsMax : sauts de redirection suivis (chacun reverifie).
	RedirectionsMax int `yaml:"redirections_max"`
}

// Supervision regle la surveillance du compte fournisseur et la notification (US-25).
type Supervision struct {
	// AlerteExpiration : a moins de cette duree de l'expiration de l'abonnement, un
	// avertissement est journalise et notifie.
	AlerteExpiration Duree `yaml:"alerte_expiration"`
	// EchecsSynchro : nombre d'echecs de synchronisation CONSECUTIFS avant notification.
	EchecsSynchro int `yaml:"echecs_synchro"`
	// PeriodeAlerte : anti-rafale, au plus un message par evenement et fournisseur par periode.
	PeriodeAlerte Duree `yaml:"periode_alerte"`
	// IntervalleInfo : relecture de user_info (appel API sans flux), en plus de la
	// synchronisation, pour que l'expiration reste connue entre deux synchronisations.
	IntervalleInfo Duree `yaml:"intervalle_info"`
	// DelaiSante borne la verification du fournisseur par la sante approfondie.
	DelaiSante Duree `yaml:"delai_sante"`
	// CacheOccupation : duree de validite de la lecture de user_info faite pour diagnostiquer
	// un 407 persistant (active_cons / max_connections). C'est aussi l'intervalle minimal
	// entre deux appels : le diagnostic ne doit jamais aggraver l'occupation du compte.
	CacheOccupation Duree   `yaml:"cache_occupation"`
	Webhook         Webhook `yaml:"webhook"`
}

// Webhook : notification minimale en POST JSON. URL vide = aucune notification.
type Webhook struct {
	URL    string `yaml:"url"`    // https obligatoire ; jamais journalisee (elle peut porter un jeton)
	Secret Secret `yaml:"secret"` // facultatif : signe le corps (HMAC-SHA256, en-tete X-Mytv-Signature)
	Delai  Duree  `yaml:"delai"`  // borne un envoi
}

// Metadonnees regle les details des films et series (US-28 : synopsis, genre,
// casting...), recuperes a la demande par get_vod_info / get_series_info. Ces appels
// n'ouvrent aucun flux mais pesent sur le fournisseur : ils sont bornes.
type Metadonnees struct {
	// Actif = faux coupe toute recuperation (les details deja stockes restent servis).
	Actif bool `yaml:"actif"`
	// DureeVie : au-dela, des details stockes sont juges perimes et redemandes ;
	// si le fournisseur echoue alors, les anciens restent servis.
	DureeVie Duree `yaml:"duree_vie"`
	// Delai borne UN appel au fournisseur (et donc l'attente d'un utilisateur).
	Delai Duree `yaml:"delai"`
	// ConcurrenceMax : appels simultanes au fournisseur (au-dela, les demandes attendent leur tour).
	ConcurrenceMax int `yaml:"concurrence_max"`
	// AppelsParMinute : plafond de debit ; au-dela, pas d'appel (reponse degradee).
	AppelsParMinute int `yaml:"appels_par_minute"`
	// CoupeCircuit : duree pendant laquelle plus aucun appel n'est fait apres un
	// refus du fournisseur (401, 403, 458).
	CoupeCircuit Duree `yaml:"coupe_circuit"`
	// EchecAttente : apres un echec ou une fiche vide pour un contenu, on ne le
	// redemande pas avant ce delai (evite de marteler un fournisseur en panne).
	EchecAttente Duree `yaml:"echec_attente"`
	// MemoireSaisons : un get_series_info recent est reutilise (liste des saisons +
	// details de la serie partagent le meme appel). 0 = pas de reutilisation.
	MemoireSaisons Duree `yaml:"memoire_saisons"`
}

// Vod regle la lecture des films et episodes (US-17).
type Vod struct {
	// PauseMax : un film dont le client ne consomme plus rien (lecteur en pause) depuis
	// ce delai est coupe, ce qui rend la place du fournisseur (une seule connexion par
	// compte) et ferme sa connexion amont. 0 = jamais. Le lecteur rouvre la lecture
	// (Range en direct, ?t= en remux) quand l'utilisateur reprend.
	PauseMax Duree `yaml:"pause_max"`
	// RelaisBrutHEVC : un client qui annonce `X-Mytv-Client: cast` recoit tel quel un film
	// HEVC que le navigateur ne sait pas lire (televiseur, Chromecast). Defaut faux : a
	// n'activer que sur demande.
	RelaisBrutHEVC bool `yaml:"relais_brut_hevc"`
	// GraceAnnulation : quand une session redemande la MEME ressource en lecture directe
	// (plages concurrentes d'un navigateur : index de fin de fichier, position,
	// prechargement), la lecture precedente a ce delai pour finir d'elle-meme avant d'etre
	// annulee (le fournisseur n'accorde qu'une connexion). Une autre ressource (zapping) et
	// un remux (seek par ?t=) sont annules tout de suite. Defaut 250 ms, > 0.
	GraceAnnulation Duree `yaml:"grace_annulation"`
	// RetentionAdresse : la source lue par ffmpeg retient l'adresse finale d'une
	// redirection du fournisseur (un aller-retour de moins a chacune des 6 a 8 ouvertures
	// d'un saut) pendant au plus ce delai ; un echec sur cette adresse la fait oublier tout
	// de suite. Defaut 10 min, > 0.
	RetentionAdresse Duree `yaml:"retention_adresse"`
}

// EPG regle l'ingestion des guides XMLTV (US-09). La frequence est
// fournisseurs[].rafraichissement.epg.
type EPG struct {
	Passe     Duree  `yaml:"passe"`      // programmes termines depuis moins de ca sont gardes
	Futur     Duree  `yaml:"futur"`      // programmes commencant au-dela sont ignores
	Fuseau    string `yaml:"fuseau"`     // fuseau des dates XMLTV sans decalage (nom IANA)
	TailleMax int64  `yaml:"taille_max"` // octets decompresses lus au plus (garde-fou anti-bombe)
}

// API regle les limites du service HTTP (US-11).
type API struct {
	CorpsMax         int64    `yaml:"corps_max"`          // octets acceptes dans un corps de requete
	DelaiLecture     Duree    `yaml:"delai_lecture"`      // lecture des en-tetes et du corps
	DelaiEcriture    Duree    `yaml:"delai_ecriture"`     // reponses non streamees
	PaginationMax    int      `yaml:"pagination_max"`     // taille maximale d'une page
	PaginationDefaut int      `yaml:"pagination_defaut"`  // taille d'une page sans `limite`
	OriginesCORS     []string `yaml:"origines_cors"`      // vide = CORS ferme
	GrilleFenetreMax Duree    `yaml:"grille_fenetre_max"` // plage maximale d'une grille EPG
	DelaiExport      Duree    `yaml:"delai_export"`       // duree maximale d'un export en bloc du catalogue
}

// Profil borne l'etat par profil (US-13).
type Profil struct {
	HistoriqueMax int `yaml:"historique_max"` // entrees gardees par profil
	FavorisMax    int `yaml:"favoris_max"`
	ReglagesMax   int `yaml:"reglages_max"` // octets de reglages par profil
}

type Serveur struct {
	Ecoute        string `yaml:"ecoute"`         // adresse:port d'ecoute
	DomainePublic string `yaml:"domaine_public"` // pour les liens absolus (Cast)
	SecretSigne   Secret `yaml:"secret_signature"`
	// HotesSegmentsAutorises : par defaut le relais REFUSE de suivre une URL de
	// segment dont l'hote est une adresse privee, locale ou de lien local (SSRF).
	// Les hotes/CIDR listes ici sont acceptes (ex. un fournisseur sur le LAN en test).
	HotesSegmentsAutorises []string `yaml:"hotes_segments_autorises"`
	// UIDossier : dossier de l'interface (build « dist ») servie en plus de l'API.
	// Vide = pas d'interface servie par ce programme.
	UIDossier string `yaml:"ui_dossier"`
	// DelaiArret : a l'arret (SIGINT/SIGTERM), temps laisse aux requetes en cours
	// avant de les couper.
	DelaiArret Duree `yaml:"delai_arret"`
}

type Stockage struct {
	Fichier string `yaml:"fichier"` // base SQLite
}

// Fournisseur est une source de contenu. Plusieurs sont possibles ; chacun a son
// propre verrou de connexions.
type Fournisseur struct {
	ID               string           `yaml:"id"` // prefixe de tous les identifiants de contenu
	Type             string           `yaml:"type"`
	Actif            *bool            `yaml:"actif"`
	Base             string           `yaml:"base"`
	Utilisateur      Secret           `yaml:"utilisateur"`
	MotDePasse       Secret           `yaml:"mot_de_passe"`
	ConnexionsMax    int              `yaml:"connexions_max"`
	Priorite         int              `yaml:"priorite"` // petit = prefere
	UserAgent        string           `yaml:"user_agent"`
	Rafraichissement Rafraichissement `yaml:"rafraichissement"`
	Reseau           Reseau           `yaml:"reseau"`
}

func (f Fournisseur) EstActif() bool { return f.Actif == nil || *f.Actif }

// Rafraichissement : evite de recharger le catalogue a chaque demande.
type Rafraichissement struct {
	Catalogue Duree  `yaml:"catalogue"` // age maximal avant nouvelle synchronisation
	EPG       Duree  `yaml:"epg"`
	Heure     string `yaml:"heure"` // « 05:00 » : synchronisation quotidienne, vide = aucune
	AuDemarre bool   `yaml:"au_demarrage"`
	DelaiMax  Duree  `yaml:"delai_max"` // borne d'un telechargement de catalogue
	Reprise   Duree  `yaml:"reprise"`   // pause avant de retenter une synchro planifiee echouee
}

type Reseau struct {
	DelaiConnexion   Duree `yaml:"delai_connexion"`
	DelaiEntete      Duree `yaml:"delai_entete"`    // attente de la reponse du fournisseur
	DelaiPlaylist    Duree `yaml:"delai_playlist"`  // par essai
	EssaisPlaylist   int   `yaml:"essais_playlist"` // nombre d'essais avant renoncement
	AttenteEssais    Duree `yaml:"attente_essais"`  // pause entre deux essais
	DelaiSegment     Duree `yaml:"delai_segment"`   // total par segment
	EssaisSegment    int   `yaml:"essais_segment"`
	RequetesAmontMax int   `yaml:"requetes_amont_max"` // simultanees, tous flux confondus
	// FermerConnexionsFlux : les requetes de segments, de films et de playlists envoient
	// `Connection: close` et ne gardent aucune connexion au chaud ; l'API (catalogue, guide)
	// garde le keep-alive. Defaut vrai. Le fournisseur reel attribue sa connexion unique a
	// une connexion TCP : une connexion de flux laissee ouverte apres le zapping ou la fin
	// d'un film lui fait garder la place (407) bien apres le depart du client.
	FermerConnexionsFlux *bool `yaml:"fermer_connexions_flux"`
}

// FermeLesFlux : fermer_connexions_flux, vrai quand il n'est pas renseigne.
func (r Reseau) FermeLesFlux() bool { return r.FermerConnexionsFlux == nil || *r.FermerConnexionsFlux }

// Live regle le relais du direct HLS.
type Live struct {
	FraicheurPlaylist Duree `yaml:"fraicheur_playlist"` // une playlist plus recente est reutilisee
	PeremptionSecours Duree `yaml:"peremption_secours"` // age max d'une playlist servie en secours
	CacheSegments     struct {
		Nombre int `yaml:"nombre"`
		Octets int `yaml:"octets"`
	} `yaml:"cache_segments"`
	// Reglages techniques du relais, ex-constantes du code.
	TaillePlaylistMax int `yaml:"taille_playlist_max"` // octets lus d'une playlist
	TailleTampon      int `yaml:"taille_tampon"`       // octets du tampon de copie d'un segment
	FenetreMesures    int `yaml:"fenetre_mesures"`     // echantillons gardes pour mediane/p95
	PartMaxSegment    int `yaml:"part_max_segment"`    // nombre d'essais d'un segment
	// TableSegments : adresses reelles retenues par la reecriture HLS (voir internal/hls).
	TableSegments struct {
		Nombre   int   `yaml:"nombre"`
		Validite Duree `yaml:"validite"` // au-dela, un segment n'est plus annonce par personne
	} `yaml:"table_segments"`

	// Conversion du direct (lot 5). Les navigateurs ne lisent ni l'EAC3/AC3/DTS ni le HEVC :
	// une sonde regarde un segment de la chaine, puis la chaine est relayee telle quelle
	// (h264+aac), avec l'audio converti en AAC, avec la video reencodee en H.264, ou refusee.
	DureeSonde           Duree `yaml:"duree_sonde"`            // validite du verdict d'une chaine
	EchecSonde           Duree `yaml:"echec_sonde"`            // pause avant de resonder une chaine dont la sonde a echoue
	DelaiSonde           Duree `yaml:"delai_sonde"`            // borne d'une premiere sonde (au-dela : relais brut)
	HauteurMaxReencodage int   `yaml:"hauteur_max_reencodage"` // au-dela, un HEVC est refuse (4K : trop lourd sans GPU)
	ReencodageVideo      bool  `yaml:"reencodage_video"`       // faux : tout reencodage video est refuse
	// DossierHLS : un sous-dossier temporaire par transcodage ; relatif au repertoire de travail.
	DossierHLS                string `yaml:"dossier_hls"`
	InactiviteTranscodage     Duree  `yaml:"inactivite_transcodage"`      // sans requete du lecteur, ffmpeg est arrete
	DelaiDemarrageTranscodage Duree  `yaml:"delai_demarrage_transcodage"` // borne entre le lancement de ffmpeg et ses premiers segments
	SegmentsAmorce            int    `yaml:"segments_amorce"`             // segments amont envoyes a ffmpeg des le depart
	SegmentHLS                int    `yaml:"segment_hls"`                 // secondes d'un segment produit
	ListeHLS                  int    `yaml:"liste_hls"`                   // segments annonces par la playlist produite
}

// Bail : reservation de la connexion d'un fournisseur par une lecture.
type Bail struct {
	TTL         Duree `yaml:"ttl"`          // un bail non renouvele expire
	PatienceMax Duree `yaml:"patience_max"` // attente d'une place quand le fournisseur refuse (407)
	Reessai     Duree `yaml:"reessai"`      // premiere pause entre deux tentatives pendant la patience
	// ReessaiMax : plafond du retrait progressif. La pause double a chaque 407 consecutif
	// (reessai, 2x, 4x...) sans depasser cette valeur : on ne martele pas un fournisseur
	// qui garde une place ou qu'un autre appareil occupe.
	ReessaiMax Duree `yaml:"reessai_max"`
	// Gigue : part aleatoire (+/-) appliquee a chaque pause, pour que plusieurs lecteurs
	// ne frappent pas le fournisseur au meme instant. 0 = aucune.
	Gigue float64 `yaml:"gigue"`
	// RetryApres et RetryApresMax : Retry-After des 503 d'occupation, croissant (le double a
	// chaque refus consecutif d'une meme session pour une meme chaine) jusqu'au plafond.
	RetryApres    Duree `yaml:"retry_apres"`
	RetryApresMax Duree `yaml:"retry_apres_max"`
	// GardeFournisseur : apres une lecture de mytv, le fournisseur garde la place ~100 s.
	// Un 407 dans ce delai est le fait de mytv lui-meme (zapping), pas d'un autre appareil.
	GardeFournisseur Duree `yaml:"garde_fournisseur"`
	// SuccesPrise et FenetrePrise : un refus apres SuccesPrise reponses reussies consecutives
	// de la meme chaine, la derniere datant de moins de FenetrePrise, est une prise de place
	// par un autre appareil (et non un segment perime d'un lecteur en pause).
	SuccesPrise  int   `yaml:"succes_prise"`
	FenetrePrise Duree `yaml:"fenetre_prise"`
}

type Comptes struct {
	DureeSession     Duree  `yaml:"duree_session"`
	CookieSecurise   bool   `yaml:"cookie_securise"`
	LocalUtilisateur string `yaml:"local_utilisateur"` // compte technique des lecteurs sans cookie (Cast)
	// PlagesLocales : seules ces plages (CIDR) peuvent utiliser le compte technique
	// local. Vide = compte technique desactive.
	PlagesLocales []string `yaml:"plages_locales"`
	// ProxysFiables : adresses dont on croit X-Forwarded-For (sinon on ignore l'en-tete).
	ProxysFiables []string `yaml:"proxys_fiables"`
	Argon2        struct {
		MemoireKio   uint32 `yaml:"memoire_kio"`
		Iterations   uint32 `yaml:"iterations"`
		Parallelisme uint8  `yaml:"parallelisme"`
	} `yaml:"argon2"`
	Tentatives struct {
		Seuil   int   `yaml:"seuil"`   // echecs toleres dans la fenetre
		Fenetre Duree `yaml:"fenetre"` // duree de comptage et de blocage
	} `yaml:"tentatives"`
	MotDePasseMin int `yaml:"mot_de_passe_min"` // longueur minimale
}

type Remux struct {
	Ffmpeg       string `yaml:"ffmpeg"`
	Ffprobe      string `yaml:"ffprobe"`
	ProcessusMax int    `yaml:"processus_max"`
	DureeSonde   Duree  `yaml:"duree_sonde"`  // validite d'une analyse de codecs
	FileMax      int    `yaml:"file_max"`     // remux en attente de processus libre
	DelaiSonde   Duree  `yaml:"delai_sonde"`  // borne d'un ffprobe
	AttenteFile  Duree  `yaml:"attente_file"` // attente maximale dans la file
	ReprisesMax  int    `yaml:"reprises_max"` // reprises Range d'un relais direct coupe en cours de film
	Tampon       int    `yaml:"tampon"`       // taille du tampon de copie (octets)
	SondeMax     int    `yaml:"sonde_max"`    // entrees du cache de sondes
	DelaiArret   Duree  `yaml:"delai_arret"`  // attente d'un ffmpeg apres SIGTERM avant SIGKILL
}

type Diagnostic struct {
	Actif              bool   `yaml:"actif"`
	Conservation       Duree  `yaml:"conservation"`
	Dossier            string `yaml:"dossier"`   // segments JSONL
	EnvoiMax           int64  `yaml:"envoi_max"` // octets par envoi d'evenements
	EvenementsParEnvoi int    `yaml:"evenements_par_envoi"`
	DebitEvenements    int    `yaml:"debit_evenements"` // par minute et par appareil
	AppareilsMax       int    `yaml:"appareils_max"`    // appareils suivis par le limiteur
	SegmentMax         int64  `yaml:"segment_max"`      // octets avant rotation
	TailleMax          int64  `yaml:"taille_max"`       // total conserve
	TexteMax           int    `yaml:"texte_max"`        // longueur max d'un evenement
	SeriesMax          int    `yaml:"series_max"`       // series max par metrique
}

type Journal struct {
	Niveau string `yaml:"niveau"` // debug, info, warn, error
}

// Secret designe une valeur sensible sans la contenir : variable d'environnement
// ou fichier. Une valeur ecrite en clair est refusee.
type Secret struct {
	Env     string `yaml:"env"`
	Fichier string `yaml:"fichier"`
}

// Valeur lit le secret. Le fichier est tronque de son retour a la ligne final.
func (s Secret) Valeur() (string, error) {
	switch {
	case s.Env != "" && s.Fichier != "":
		return "", errors.New("secret : « env » et « fichier » sont exclusifs")
	case s.Env != "":
		v := os.Getenv(s.Env)
		if v == "" {
			return "", fmt.Errorf("variable d'environnement %s absente ou vide", s.Env)
		}
		return v, nil
	case s.Fichier != "":
		b, err := os.ReadFile(s.Fichier)
		if err != nil {
			return "", fmt.Errorf("fichier secret : %w", err)
		}
		v := strings.TrimRight(string(b), "\r\n")
		if v == "" {
			return "", fmt.Errorf("fichier secret %s vide", s.Fichier)
		}
		return v, nil
	}
	return "", errors.New("secret non defini (« env » ou « fichier »)")
}

func (s Secret) Defini() bool { return s.Env != "" || s.Fichier != "" }

// Defauts : valeurs retenues apres les mesures de la plateforme actuelle.
func Defauts() Config {
	var c Config
	c.Serveur.Ecoute = "127.0.0.1:8080"
	c.Serveur.DelaiArret = Duree(20 * time.Second)
	c.Stockage.Fichier = "mytv.db"
	c.Live.FraicheurPlaylist = Duree(800 * time.Millisecond)
	c.Live.PeremptionSecours = Duree(75 * time.Second)
	c.Live.CacheSegments.Nombre = 14
	c.Live.CacheSegments.Octets = 96 << 20
	c.Live.TableSegments.Nombre = 600
	c.Live.TableSegments.Validite = Duree(10 * time.Minute)
	c.Live.DureeSonde = Duree(10 * time.Minute)
	c.Live.EchecSonde = Duree(time.Minute)
	c.Live.DelaiSonde = Duree(8 * time.Second)
	c.Live.HauteurMaxReencodage = 1080
	c.Live.ReencodageVideo = true
	c.Live.DossierHLS = "hls_live"
	c.Live.InactiviteTranscodage = Duree(60 * time.Second)
	c.Live.DelaiDemarrageTranscodage = Duree(30 * time.Second)
	c.Live.SegmentsAmorce = 2
	c.Live.SegmentHLS = 4
	c.Live.ListeHLS = 6
	c.Bail.TTL = Duree(90 * time.Second)
	c.Bail.PatienceMax = Duree(2 * time.Minute)
	c.Bail.Reessai = Duree(3 * time.Second)
	c.Bail.ReessaiMax = Duree(30 * time.Second)
	c.Bail.Gigue = 0.2
	c.Bail.RetryApres = Duree(15 * time.Second)
	c.Bail.RetryApresMax = Duree(60 * time.Second)
	c.Bail.GardeFournisseur = Duree(100 * time.Second)
	c.Bail.SuccesPrise = 2
	c.Bail.FenetrePrise = Duree(30 * time.Second)
	c.Comptes.DureeSession = Duree(30 * 24 * time.Hour)
	c.Comptes.CookieSecurise = true
	c.Remux.Ffmpeg = "ffmpeg"
	c.Remux.Ffprobe = "ffprobe"
	c.Remux.ProcessusMax = 2
	c.Remux.DureeSonde = Duree(7 * 24 * time.Hour)
	c.Remux.FileMax = 4
	c.Remux.DelaiSonde = Duree(20 * time.Second)
	c.Remux.AttenteFile = Duree(30 * time.Second)
	c.Live.TaillePlaylistMax = 2 << 20
	c.Live.TailleTampon = 64 << 10
	c.Live.FenetreMesures = 300
	c.Live.PartMaxSegment = 2
	c.Comptes.Argon2.MemoireKio = 64 * 1024
	c.Comptes.Argon2.Iterations = 3
	c.Comptes.Argon2.Parallelisme = 2
	c.Comptes.Tentatives.Seuil = 5
	c.Comptes.Tentatives.Fenetre = Duree(15 * time.Minute)
	c.Comptes.MotDePasseMin = 10
	c.API.CorpsMax = 64 << 10
	c.API.DelaiLecture = Duree(10 * time.Second)
	c.API.DelaiEcriture = Duree(30 * time.Second)
	c.API.PaginationMax = 200
	c.API.PaginationDefaut = 50
	c.EPG.Passe = Duree(6 * time.Hour)
	c.EPG.Futur = Duree(7 * 24 * time.Hour)
	c.EPG.Fuseau = "UTC"
	c.EPG.TailleMax = 1 << 30
	c.API.GrilleFenetreMax = Duree(48 * time.Hour)
	c.API.DelaiExport = Duree(5 * time.Minute)
	c.Profil.HistoriqueMax = 200
	c.Profil.FavorisMax = 2000
	c.Profil.ReglagesMax = 16 << 10
	c.Remux.ReprisesMax = 3
	c.Vod.PauseMax = Duree(5 * time.Minute)
	c.Vod.GraceAnnulation = Duree(250 * time.Millisecond)
	c.Vod.RetentionAdresse = Duree(10 * time.Minute)
	c.Metadonnees = Metadonnees{Actif: true, DureeVie: Duree(30 * 24 * time.Hour), Delai: Duree(8 * time.Second),
		ConcurrenceMax: 2, AppelsParMinute: 30, CoupeCircuit: Duree(10 * time.Minute),
		EchecAttente: Duree(2 * time.Minute), MemoireSaisons: Duree(30 * time.Second)}
	c.Remux.Tampon = 256 << 10
	c.Remux.SondeMax = 500
	c.Remux.DelaiArret = Duree(5 * time.Second)
	c.Diagnostic.Dossier = "diag"
	c.Diagnostic.EnvoiMax = 64 << 10
	c.Diagnostic.EvenementsParEnvoi = 80
	c.Diagnostic.DebitEvenements = 240
	c.Diagnostic.AppareilsMax = 1000
	c.Diagnostic.SegmentMax = 1 << 20
	c.Diagnostic.TailleMax = 8 << 20
	c.Diagnostic.TexteMax = 300
	c.Diagnostic.SeriesMax = 50
	c.Diagnostic.Actif = true
	c.Diagnostic.Conservation = Duree(72 * time.Hour)
	c.Images.CacheDossier = "cache_images"
	c.Images.CacheOctets = 256 << 20
	c.Images.CacheValidite = Duree(7 * 24 * time.Hour)
	c.Images.Delai = Duree(8 * time.Second)
	c.Images.TailleMax = 2 << 20
	c.Images.ConcurrenceMax = 4
	c.Images.CacheNegatif = Duree(5 * time.Minute)
	c.Images.CacheNegatifMax = 5000
	c.Images.RedirectionsMax = 3
	c.Journal.Niveau = "info"
	c.Supervision.AlerteExpiration = Duree(7 * 24 * time.Hour)
	c.Supervision.EchecsSynchro = 3
	c.Supervision.PeriodeAlerte = Duree(24 * time.Hour)
	c.Supervision.IntervalleInfo = Duree(6 * time.Hour)
	c.Supervision.DelaiSante = Duree(8 * time.Second)
	c.Supervision.CacheOccupation = Duree(15 * time.Second)
	c.Supervision.Webhook.Delai = Duree(5 * time.Second)
	c.Sauvegarde.Heure = "03:30"
	c.Sauvegarde.Conservation = 14
	return c
}

func defautsFournisseur(f *Fournisseur) {
	if f.ConnexionsMax == 0 {
		f.ConnexionsMax = 1
	}
	if f.Priorite == 0 {
		f.Priorite = 1
	}
	r := &f.Rafraichissement
	if r.Catalogue == 0 {
		r.Catalogue = Duree(24 * time.Hour)
	}
	if r.DelaiMax == 0 {
		r.DelaiMax = Duree(10 * time.Minute)
	}
	if r.Reprise == 0 {
		r.Reprise = Duree(30 * time.Minute)
	}
	if r.EPG == 0 {
		r.EPG = Duree(6 * time.Hour)
	}
	n := &f.Reseau
	if n.DelaiConnexion == 0 {
		n.DelaiConnexion = Duree(5 * time.Second)
	}
	if n.DelaiEntete == 0 {
		n.DelaiEntete = Duree(6 * time.Second)
	}
	if n.DelaiPlaylist == 0 {
		n.DelaiPlaylist = Duree(4 * time.Second)
	}
	if n.EssaisPlaylist == 0 {
		n.EssaisPlaylist = 3
	}
	if n.AttenteEssais == 0 {
		n.AttenteEssais = Duree(250 * time.Millisecond)
	}
	if n.DelaiSegment == 0 {
		n.DelaiSegment = Duree(20 * time.Second)
	}
	if n.EssaisSegment == 0 {
		n.EssaisSegment = 2
	}
	if n.RequetesAmontMax == 0 {
		n.RequetesAmontMax = f.ConnexionsMax * 3
	}
}

// Lire decode la configuration. Toute cle inconnue est une erreur : une faute de
// frappe ne doit pas laisser un reglage silencieusement ignore.
func Lire(r io.Reader) (*Config, error) {
	c := Defauts()
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("configuration : %w", err)
	}
	for i := range c.Fournisseurs {
		defautsFournisseur(&c.Fournisseurs[i])
	}
	if err := c.Valider(); err != nil {
		return nil, err
	}
	return &c, nil
}

func Charger(chemin string) (*Config, error) {
	b, err := os.ReadFile(chemin)
	if err != nil {
		return nil, fmt.Errorf("configuration : %w", err)
	}
	return Lire(bytes.NewReader(b))
}

// Valider rassemble TOUTES les erreurs, pour les corriger en une seule fois.
func (c *Config) Valider() error {
	var errs []error
	ajoute := func(f string, a ...any) { errs = append(errs, fmt.Errorf(f, a...)) }

	if c.Serveur.Ecoute == "" {
		ajoute("serveur.ecoute est vide")
	} else if _, port, err := net.SplitHostPort(c.Serveur.Ecoute); err != nil {
		ajoute("serveur.ecoute %q invalide (attendu adresse:port, ex. 127.0.0.1:8080)", c.Serveur.Ecoute)
	} else if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		ajoute("serveur.ecoute %q : port invalide", c.Serveur.Ecoute)
	}
	if h := c.Sauvegarde.Heure; h != "" {
		if _, err := time.Parse("15:04", h); err != nil {
			ajoute("sauvegarde.heure %q invalide (format HH:MM, ou vide pour desactiver)", h)
		}
	}
	if c.Sauvegarde.Conservation < 1 {
		ajoute("sauvegarde.conservation doit etre >= 1 (jours)")
	}
	if strings.ContainsRune(c.Sauvegarde.Dossier, 0) {
		ajoute("sauvegarde.dossier contient un caractere nul")
	}
	if !c.Serveur.SecretSigne.Defini() {
		ajoute("serveur.secret_signature est requis (env ou fichier) : il signe les URL de segments")
	}
	vus := map[string]bool{}
	actifs := 0
	for i, f := range c.Fournisseurs {
		p := fmt.Sprintf("fournisseurs[%d]", i)
		if f.ID == "" {
			ajoute("%s.id est requis", p)
		}
		if strings.ContainsAny(f.ID, ": /\\") {
			ajoute("%s.id %q ne doit pas contenir « : », « / » ni d'espace (il prefixe les identifiants)", p, f.ID)
		}
		if f.ID != "" && vus[f.ID] {
			ajoute("%s.id %q est en double", p, f.ID)
		}
		vus[f.ID] = true
		if f.Type != "xtream" {
			ajoute("%s.type %q non pris en charge (xtream)", p, f.Type)
		}
		if !strings.HasPrefix(f.Base, "http://") && !strings.HasPrefix(f.Base, "https://") {
			ajoute("%s.base doit commencer par http:// ou https://", p)
		}
		if !f.Utilisateur.Defini() || !f.MotDePasse.Defini() {
			ajoute("%s : utilisateur et mot_de_passe sont requis (env ou fichier)", p)
		}
		if f.ConnexionsMax < 1 {
			ajoute("%s.connexions_max doit etre >= 1", p)
		}
		if h := f.Rafraichissement.Heure; h != "" {
			if _, err := time.Parse("15:04", h); err != nil {
				ajoute("%s.rafraichissement.heure %q invalide (format HH:MM)", p, h)
			}
		}
		if f.EstActif() {
			actifs++
		}
	}
	if actifs == 0 {
		ajoute("aucun fournisseur actif")
	}
	c.validerComptes(ajoute)
	if c.Serveur.DelaiArret <= 0 {
		ajoute("serveur.delai_arret doit etre > 0")
	}
	a := c.API
	if a.CorpsMax < 1 || a.DelaiLecture <= 0 || a.DelaiEcriture <= 0 || a.GrilleFenetreMax <= 0 {
		ajoute("api.corps_max, delai_lecture, delai_ecriture et grille_fenetre_max doivent etre > 0")
	}
	if a.DelaiExport <= 0 {
		ajoute("api.delai_export doit etre > 0")
	}
	if c.Vod.GraceAnnulation <= 0 {
		ajoute("vod.grace_annulation doit etre > 0 (250ms par defaut)")
	}
	if c.Vod.RetentionAdresse <= 0 {
		ajoute("vod.retention_adresse doit etre > 0 (10m par defaut)")
	}
	if a.PaginationMax < 1 || a.PaginationDefaut < 1 || a.PaginationDefaut > a.PaginationMax {
		ajoute("api.pagination_defaut doit etre entre 1 et pagination_max")
	}
	for _, o := range a.OriginesCORS {
		if o == "*" || !(strings.HasPrefix(o, "https://") || strings.HasPrefix(o, "http://")) || strings.HasSuffix(o, "/") {
			ajoute("api.origines_cors : %q invalide (origine exacte « https://hote[:port] », jamais « * »)", o)
		}
	}
	l := c.Live
	if l.TaillePlaylistMax < 1 || l.TailleTampon < 1 || l.FenetreMesures < 1 || l.PartMaxSegment < 1 {
		ajoute("live.taille_playlist_max, taille_tampon, fenetre_mesures et part_max_segment doivent etre >= 1")
	}
	c.validerConversionDirect(ajoute)
	for _, h := range c.Serveur.HotesSegmentsAutorises {
		if strings.TrimSpace(h) == "" || strings.ContainsAny(h, " ") {
			ajoute("serveur.hotes_segments_autorises : %q invalide", h)
		}
	}
	if c.EPG.Passe < 0 || c.EPG.Futur <= 0 {
		ajoute("epg.passe doit etre >= 0 et epg.futur > 0")
	}
	if c.EPG.TailleMax <= 0 {
		ajoute("epg.taille_max doit etre > 0")
	}
	if _, err := time.LoadLocation(c.EPG.Fuseau); err != nil {
		ajoute("epg.fuseau %q inconnu", c.EPG.Fuseau)
	}
	c.validerImages(ajoute)
	c.validerSupervision(ajoute)
	c.validerBail(ajoute)
	m := c.Metadonnees
	if m.DureeVie <= 0 || m.Delai <= 0 || m.CoupeCircuit <= 0 {
		ajoute("metadonnees.duree_vie, delai et coupe_circuit doivent etre > 0")
	}
	if m.ConcurrenceMax < 1 || m.ConcurrenceMax > 16 {
		ajoute("metadonnees.concurrence_max doit etre entre 1 et 16")
	}
	if m.AppelsParMinute < 1 || m.AppelsParMinute > 600 {
		ajoute("metadonnees.appels_par_minute doit etre entre 1 et 600")
	}
	if m.Delai.Std() > 2*time.Minute {
		ajoute("metadonnees.delai ne doit pas depasser 2m (un utilisateur attend derriere)")
	}
	switch c.Journal.Niveau {
	case "debug", "info", "warn", "error":
	default:
		ajoute("journal.niveau %q invalide (debug, info, warn, error)", c.Journal.Niveau)
	}
	return errors.Join(errs...)
}

// validerConversionDirect : reglages de la sonde et du transcodage du direct. Le dossier
// ne doit contenir ni « % » (ffmpeg l'interpreterait dans -hls_segment_filename) ni
// caractere de controle, et un delai ou un compteur nul rendrait la fonction muette ou bouclante.
func (c *Config) validerConversionDirect(ajoute func(string, ...any)) {
	l := c.Live
	if l.DureeSonde <= 0 || l.EchecSonde <= 0 || l.DelaiSonde <= 0 {
		ajoute("live.duree_sonde, echec_sonde et delai_sonde doivent etre > 0")
	}
	if l.DelaiSonde.Std() > time.Minute {
		ajoute("live.delai_sonde ne doit pas depasser 1m (un lecteur attend derriere)")
	}
	if l.HauteurMaxReencodage < 144 || l.HauteurMaxReencodage > 4320 {
		ajoute("live.hauteur_max_reencodage doit etre entre 144 et 4320")
	}
	if l.DossierHLS == "" || strings.ContainsAny(l.DossierHLS, "%\x00\n\r") {
		ajoute("live.dossier_hls est vide ou contient un caractere interdit (« %% », nul, retour a la ligne)")
	}
	if l.InactiviteTranscodage <= 0 || l.DelaiDemarrageTranscodage <= 0 {
		ajoute("live.inactivite_transcodage et delai_demarrage_transcodage doivent etre > 0")
	}
	if l.DelaiDemarrageTranscodage.Std() > 5*time.Minute {
		ajoute("live.delai_demarrage_transcodage ne doit pas depasser 5m")
	}
	if l.SegmentsAmorce < 1 || l.SegmentsAmorce > 10 {
		ajoute("live.segments_amorce doit etre entre 1 et 10")
	}
	if l.SegmentHLS < 1 || l.SegmentHLS > 30 {
		ajoute("live.segment_hls doit etre entre 1 et 30 (secondes)")
	}
	if l.ListeHLS < 3 || l.ListeHLS > 30 {
		ajoute("live.liste_hls doit etre entre 3 et 30 segments")
	}
}

// validerBail : le retrait progressif doit rester borne et coherent.
func (c *Config) validerBail(ajoute func(string, ...any)) {
	b := c.Bail
	if b.Reessai <= 0 {
		ajoute("bail.reessai doit etre > 0")
	}
	if b.ReessaiMax < b.Reessai {
		ajoute("bail.reessai_max doit etre >= bail.reessai")
	}
	if b.Gigue < 0 || b.Gigue > 0.5 {
		ajoute("bail.gigue doit etre entre 0 et 0.5")
	}
	if b.RetryApres <= 0 || b.RetryApresMax < b.RetryApres {
		ajoute("bail.retry_apres doit etre > 0 et bail.retry_apres_max >= bail.retry_apres")
	}
	if b.GardeFournisseur < 0 {
		ajoute("bail.garde_fournisseur ne peut pas etre negative")
	}
	if b.SuccesPrise < 1 || b.FenetrePrise <= 0 {
		ajoute("bail.succes_prise doit etre >= 1 et bail.fenetre_prise > 0")
	}
}

// validerSupervision : le message n'echo JAMAIS l'URL du webhook (elle peut porter un jeton).
func (c *Config) validerSupervision(ajoute func(string, ...any)) {
	k := c.Supervision
	if k.AlerteExpiration <= 0 || k.PeriodeAlerte <= 0 || k.DelaiSante <= 0 || k.IntervalleInfo <= 0 {
		ajoute("supervision.alerte_expiration, periode_alerte, intervalle_info et delai_sante doivent etre > 0")
	}
	if k.EchecsSynchro < 1 {
		ajoute("supervision.echecs_synchro doit etre >= 1")
	}
	if k.CacheOccupation <= 0 {
		ajoute("supervision.cache_occupation doit etre > 0 (c'est l'intervalle minimal entre deux lectures de user_info)")
	}
	w := k.Webhook
	if w.URL == "" {
		if w.Secret.Defini() {
			ajoute("supervision.webhook.secret sans supervision.webhook.url")
		}
		return
	}
	u, err := url.Parse(w.URL)
	switch {
	case err != nil || u.Host == "" || u.Hostname() == "":
		ajoute("supervision.webhook.url n'est pas une adresse valide")
	case u.Scheme != "https":
		ajoute("supervision.webhook.url doit etre en https://")
	case u.User != nil:
		ajoute("supervision.webhook.url ne doit pas contenir d'identifiants (utiliser supervision.webhook.secret)")
	case u.Fragment != "":
		ajoute("supervision.webhook.url ne doit pas contenir de fragment")
	}
	if w.Delai <= 0 {
		ajoute("supervision.webhook.delai doit etre > 0")
	}
}

// validerComptes refuse des reglages de comptes dangereux. Les parametres argon2
// ne descendent pas sous le minimum recommande par l'OWASP (19 Mio, 2 passes).
func (c *Config) validerComptes(ajoute func(string, ...any)) {
	k := c.Comptes
	if k.Argon2.MemoireKio < 19*1024 || k.Argon2.Iterations < 2 || k.Argon2.Parallelisme < 1 {
		ajoute("comptes.argon2 trop faible (au moins memoire_kio 19456, iterations 2, parallelisme 1)")
	}
	if k.Tentatives.Seuil < 1 || k.Tentatives.Fenetre <= 0 {
		ajoute("comptes.tentatives.seuil doit etre >= 1 et fenetre > 0")
	}
	if k.DureeSession <= 0 {
		ajoute("comptes.duree_session doit etre > 0")
	}
	if k.MotDePasseMin < 8 {
		ajoute("comptes.mot_de_passe_min doit etre >= 8")
	}
	for nom, l := range map[string][]string{"comptes.plages_locales": k.PlagesLocales, "comptes.proxys_fiables": k.ProxysFiables} {
		for _, e := range l {
			if _, err := netip.ParsePrefix(e); err != nil {
				if _, err := netip.ParseAddr(e); err != nil {
					ajoute("%s : %q n'est ni une adresse ni une plage CIDR", nom, e)
				}
			}
		}
	}
	if k.LocalUtilisateur != "" && len(k.PlagesLocales) == 0 {
		ajoute("comptes.local_utilisateur exige comptes.plages_locales (sinon le compte technique ne serait accessible de nulle part)")
	}
}

// Avertissements liste les reglages valides mais risques : le programme demarre quand
// meme (l'option reste possible), mais le dit.
func (c *Config) Avertissements() []string {
	var out []string
	host, _, err := net.SplitHostPort(c.Serveur.Ecoute)
	if err != nil {
		return nil
	}
	// Ecoute sur toutes les interfaces : sans proxy declare ni plage locale, le programme ne
	// sait pas distinguer le reseau local d'Internet (limitation de tentatives par adresse
	// commune, compte technique desactive) : le dire plutot que de laisser croire a une protection.
	if (host == "" || host == "0.0.0.0" || host == "::") && len(c.Comptes.ProxysFiables) == 0 && len(c.Comptes.PlagesLocales) == 0 {
		out = append(out, "serveur.ecoute "+c.Serveur.Ecoute+" ecoute sur toutes les interfaces sans comptes.proxys_fiables ni comptes.plages_locales : "+
			"l'origine des clients ne peut pas etre distinguee (limitation de tentatives commune, compte technique inutilisable) ; "+
			"preferer une adresse precise, ou declarer le reseau local / le proxy")
	}
	return out
}

// Resoudre lit tous les secrets references ; a appeler au demarrage pour echouer
// tout de suite, et non au premier appel qui en aurait besoin.
func (c *Config) Resoudre() error {
	var errs []error
	test := func(nom string, s Secret) {
		if _, err := s.Valeur(); err != nil {
			errs = append(errs, fmt.Errorf("%s : %w", nom, err))
		}
	}
	test("serveur.secret_signature", c.Serveur.SecretSigne)
	if c.Supervision.Webhook.URL != "" && c.Supervision.Webhook.Secret.Defini() {
		test("supervision.webhook.secret", c.Supervision.Webhook.Secret)
	}
	for _, f := range c.Fournisseurs {
		if !f.EstActif() {
			continue
		}
		test("fournisseurs."+f.ID+".utilisateur", f.Utilisateur)
		test("fournisseurs."+f.ID+".mot_de_passe", f.MotDePasse)
	}
	return errors.Join(errs...)
}

// validerImages : un cache qui ne tient pas une seule image, ou une concurrence
// nulle, rendrait le proxy muet sans erreur visible.
func (c *Config) validerImages(ajoute func(string, ...any)) {
	i := c.Images
	if strings.TrimSpace(i.CacheDossier) == "" {
		ajoute("images.cache_dossier est vide")
	}
	if i.TailleMax < 1 {
		ajoute("images.taille_max doit etre >= 1")
	}
	if i.CacheOctets < i.TailleMax {
		ajoute("images.cache_octets doit etre >= images.taille_max (le cache doit tenir au moins une image)")
	}
	if i.CacheValidite <= 0 || i.Delai <= 0 {
		ajoute("images.cache_validite et images.delai doivent etre > 0")
	}
	if i.ConcurrenceMax < 1 {
		ajoute("images.concurrence_max doit etre >= 1")
	}
	if i.CacheNegatifMax < 1 {
		ajoute("images.cache_negatif_max doit etre >= 1")
	}
	if i.RedirectionsMax < 0 || i.RedirectionsMax > 10 {
		ajoute("images.redirections_max doit etre entre 0 et 10")
	}
}
