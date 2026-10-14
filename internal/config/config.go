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
	"net/netip"
	"os"
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
}

// API regle les limites du service HTTP (US-11).
type API struct {
	CorpsMax         int64    `yaml:"corps_max"`         // octets acceptes dans un corps de requete
	DelaiLecture     Duree    `yaml:"delai_lecture"`     // lecture des en-tetes et du corps
	DelaiEcriture    Duree    `yaml:"delai_ecriture"`    // reponses non streamees
	PaginationMax    int      `yaml:"pagination_max"`    // taille maximale d'une page
	PaginationDefaut int      `yaml:"pagination_defaut"` // taille d'une page sans `limite`
	OriginesCORS     []string `yaml:"origines_cors"`     // vide = CORS ferme
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
}

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
}

// Bail : reservation de la connexion d'un fournisseur par une lecture.
type Bail struct {
	TTL         Duree `yaml:"ttl"`          // un bail non renouvele expire
	PatienceMax Duree `yaml:"patience_max"` // attente d'une place quand le fournisseur refuse (407)
	Reessai     Duree `yaml:"reessai"`      // pause entre deux tentatives pendant la patience
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
	c.Stockage.Fichier = "mytv.db"
	c.Live.FraicheurPlaylist = Duree(800 * time.Millisecond)
	c.Live.PeremptionSecours = Duree(75 * time.Second)
	c.Live.CacheSegments.Nombre = 14
	c.Live.CacheSegments.Octets = 96 << 20
	c.Live.TableSegments.Nombre = 600
	c.Live.TableSegments.Validite = Duree(10 * time.Minute)
	c.Bail.TTL = Duree(90 * time.Second)
	c.Bail.PatienceMax = Duree(2 * time.Minute)
	c.Bail.Reessai = Duree(3 * time.Second)
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
	c.Profil.HistoriqueMax = 200
	c.Profil.FavorisMax = 2000
	c.Profil.ReglagesMax = 16 << 10
	c.Remux.ReprisesMax = 3
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
	c.Journal.Niveau = "info"
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
	switch c.Journal.Niveau {
	case "debug", "info", "warn", "error":
	default:
		ajoute("journal.niveau %q invalide (debug, info, warn, error)", c.Journal.Niveau)
	}
	return errors.Join(errs...)
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
	for _, f := range c.Fournisseurs {
		if !f.EstActif() {
			continue
		}
		test("fournisseurs."+f.ID+".utilisateur", f.Utilisateur)
		test("fournisseurs."+f.ID+".mot_de_passe", f.MotDePasse)
	}
	return errors.Join(errs...)
}
