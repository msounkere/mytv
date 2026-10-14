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
}

type Serveur struct {
	Ecoute        string `yaml:"ecoute"`         // adresse:port d'ecoute
	DomainePublic string `yaml:"domaine_public"` // pour les liens absolus (Cast)
	SecretSigne   Secret `yaml:"secret_signature"`
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
	LocalMotDePasse  Secret `yaml:"local_mot_de_passe"`
}

type Remux struct {
	Ffmpeg       string `yaml:"ffmpeg"`
	Ffprobe      string `yaml:"ffprobe"`
	ProcessusMax int    `yaml:"processus_max"`
	DureeSonde   Duree  `yaml:"duree_sonde"` // validite d'une analyse de codecs
}

type Diagnostic struct {
	Actif        bool  `yaml:"actif"`
	Conservation Duree `yaml:"conservation"`
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
	switch c.Journal.Niveau {
	case "debug", "info", "warn", "error":
	default:
		ajoute("journal.niveau %q invalide (debug, info, warn, error)", c.Journal.Niveau)
	}
	return errors.Join(errs...)
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
	if c.Comptes.LocalMotDePasse.Defini() {
		test("comptes.local_mot_de_passe", c.Comptes.LocalMotDePasse)
	}
	return errors.Join(errs...)
}
