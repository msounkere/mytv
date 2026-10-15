// Package dev est le harnais de developpement : un faux fournisseur IPTV
// (catalogue realiste, guide sur 48 h) ET mytv, lances ensemble dans un seul
// processus, sur des ports de la boucle locale, avec une base SQLite jetable et
// des utilisateurs de test. Les tests de l'interface (Playwright) et la recette
// automatisee s'y branchent sans rien d'autre.
//
// Garanties :
//   - aucune connexion hors de 127.0.0.1 : toutes les adresses d'ecoute sont
//     refusees si elles ne sont pas de boucle locale, le fournisseur est le faux
//     local, et un test inspecte les sockets du processus ;
//   - arret propre et complet (processus, goroutines, dossier temporaire) ;
//   - les identifiants du faux fournisseur sont aleatoires et ne sont affiches
//     nulle part.
package dev

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mytv/internal/appli"
	"mytv/internal/config"
	"mytv/internal/testutil/fauxiptv"
)

// Options du harnais. Les zeros donnent des valeurs sensees.
type Options struct {
	Ecoute      string // mytv : defaut 127.0.0.1:0 (port ephemere)
	Fournisseur string // faux fournisseur : defaut 127.0.0.1:0
	Controle    string // petite API de pilotage des pannes : defaut 127.0.0.1:0

	Dist string // dossier de l'interface (meme origine que l'API) ; vide = pas d'interface

	Chaines int // defaut 2000
	Films   int // defaut 5000
	Series  int // defaut 500
	// EPGChaines : nombre de chaines (les premieres) qui ont un guide. Defaut : toutes, au plus 2000.
	EPGChaines int
	EPGDuree   time.Duration // defaut 48 h

	// Pannes de depart, au format « statut:duree » (ex. « 407:30s »). La duree
	// court a partir de la fin du demarrage.
	Pannes []string
	// Latence ajoutee a chaque reponse du fournisseur sur le direct et les films.
	Latence time.Duration
	// UneConnexion : regle du vrai fournisseur (une chaine a la fois, place gardee
	// cette duree). 0 = desactivee.
	UneConnexion time.Duration

	// CompteLocal active le compte technique : un lecteur sans cookie venant de
	// 127.0.0.1 lit le direct et les films (GET/HEAD), comme un recepteur Cast du LAN.
	CompteLocal bool

	Utilisateur string // defaut « test » (non administrateur)
	Admin       string // defaut « admin »
	MotDePasse  string // defaut « motdepasse-de-dev » (jeu de test : jamais un vrai mot de passe)

	Journal io.Writer // defaut io.Discard
	Verbeux bool
}

// Harnais est un environnement demarre.
type Harnais struct {
	URL            string // mytv (API et interface)
	FournisseurURL string // faux fournisseur
	ControleURL    string // API de pilotage
	Utilisateur    string
	Admin          string
	MotDePasse     string

	faux     *fauxiptv.Faux
	app      *appli.Appli
	ctrl     *http.Server
	latence  time.Duration
	dossier  string
	arreter  context.CancelFunc
	termine  chan struct{} // ferme quand mytv a fini de s'arreter
	erreur   error
	nettoyer []func()
}

// nettoyeur collecte les Cleanup du faux fournisseur (qui attend un *testing.T).
type nettoyeur struct{ fs []func() }

func (n *nettoyeur) Cleanup(f func()) { n.fs = append(n.fs, f) }

// Valider controle les options et applique les valeurs par defaut.
func (o *Options) Valider() error {
	var errs []error
	def := func(p *string, v string) {
		if *p == "" {
			*p = v
		}
	}
	def(&o.Ecoute, "127.0.0.1:0")
	def(&o.Fournisseur, "127.0.0.1:0")
	def(&o.Controle, "127.0.0.1:0")
	def(&o.Utilisateur, "test")
	def(&o.Admin, "admin")
	def(&o.MotDePasse, "motdepasse-de-dev")
	for nom, a := range map[string]string{"ecoute": o.Ecoute, "fournisseur": o.Fournisseur, "controle": o.Controle} {
		if err := adresseBoucle(a); err != nil {
			errs = append(errs, fmt.Errorf("--%s : %w", nom, err))
		}
	}
	if o.Chaines == 0 {
		o.Chaines = 2000
	}
	if o.Films == 0 {
		o.Films = 5000
	}
	if o.Series == 0 {
		o.Series = 500
	}
	if o.Chaines < 1 || o.Films < 0 || o.Series < 0 {
		errs = append(errs, errors.New("nombres de chaines/films/series invalides"))
	}
	if o.EPGChaines == 0 {
		o.EPGChaines = min(o.Chaines, 2000)
	}
	if o.EPGChaines < 0 || o.EPGChaines > o.Chaines {
		errs = append(errs, errors.New("--epg-chaines doit etre entre 0 et le nombre de chaines"))
	}
	if o.EPGDuree == 0 {
		o.EPGDuree = 48 * time.Hour
	}
	if o.Latence < 0 || o.UneConnexion < 0 || o.EPGDuree < 0 {
		errs = append(errs, errors.New("durees negatives"))
	}
	if len(o.MotDePasse) < 10 {
		errs = append(errs, errors.New("le mot de passe de test doit faire au moins 10 caracteres"))
	}
	if o.Utilisateur == o.Admin {
		errs = append(errs, errors.New("utilisateur et administrateur doivent differer"))
	}
	for _, p := range o.Pannes {
		if _, err := AnalyserPanne(p); err != nil {
			errs = append(errs, err)
		}
	}
	if o.Dist != "" {
		if st, err := os.Stat(o.Dist); err != nil || !st.IsDir() {
			errs = append(errs, fmt.Errorf("--dist %q n'est pas un dossier", o.Dist))
		}
	}
	return errors.Join(errs...)
}

// adresseBoucle refuse tout ce qui n'est pas « adresse de boucle locale:port ».
func adresseBoucle(a string) error {
	h, _, err := net.SplitHostPort(a)
	if err != nil {
		return fmt.Errorf("adresse %q invalide (attendu 127.0.0.1:port)", a)
	}
	if ip := net.ParseIP(h); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("adresse %q refusee : le harnais n'ecoute que sur la boucle locale", a)
	}
	return nil
}

// Panne est une panne demandee : un statut pendant une duree.
type Panne struct {
	Statut int
	Duree  time.Duration
}

// AnalyserPanne lit « 407:30s » (le statut 4xx/5xx, la duree Go).
func AnalyserPanne(s string) (Panne, error) {
	st, d, ok := strings.Cut(s, ":")
	var p Panne
	if !ok {
		return p, fmt.Errorf("panne %q invalide (attendu statut:duree, ex. 407:30s)", s)
	}
	if _, err := fmt.Sscanf(st, "%d", &p.Statut); err != nil || p.Statut < 400 || p.Statut > 599 {
		return p, fmt.Errorf("panne %q : statut %q invalide (400 a 599)", s, st)
	}
	dd, err := time.ParseDuration(d)
	if err != nil || dd <= 0 {
		return p, fmt.Errorf("panne %q : duree %q invalide", s, d)
	}
	p.Duree = dd
	return p, nil
}

func secretAleatoire() string {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Demarrer lance le faux fournisseur, mytv et l'API de pilotage. Il rend la main
// quand le catalogue et le guide sont synchronises. Annuler ctx ou appeler
// Fermer arrete tout.
func Demarrer(ctx context.Context, o Options) (h *Harnais, err error) {
	if err := o.Valider(); err != nil {
		return nil, err
	}
	journal := o.Journal
	if journal == nil {
		journal = io.Discard
	}
	niveau := slog.LevelWarn
	if o.Verbeux {
		niveau = slog.LevelDebug
	}
	log := slog.New(slog.NewJSONHandler(journal, &slog.HandlerOptions{Level: niveau}))

	h = &Harnais{Utilisateur: o.Utilisateur, Admin: o.Admin, MotDePasse: o.MotDePasse, latence: o.Latence, termine: make(chan struct{})}
	defer func() {
		if err != nil {
			h.Fermer()
		}
	}()
	if h.dossier, err = os.MkdirTemp("", "mytv-dev-"); err != nil {
		return nil, err
	}

	// --- faux fournisseur (identifiants aleatoires, jamais affiches).
	iptvU, iptvP := "u"+secretAleatoire()[:8], secretAleatoire()
	nt := &nettoyeur{}
	h.faux = fauxiptv.Nouveau(nt, fauxiptv.Options{
		Utilisateur: iptvU, MotDePasse: iptvP, Ecoute: o.Fournisseur,
		TailleFilm: 1 << 20, XMLTV: fauxiptv.EPGRealiste(o.EPGChaines, time.Now().Add(-6*time.Hour), o.EPGDuree+6*time.Hour),
	})
	h.nettoyer = append(h.nettoyer, nt.fs...)
	h.FournisseurURL = h.faux.URL()
	h.faux.DefinirCatalogue(fauxiptv.CatalogueRealiste(h.FournisseurURL, o.Chaines, o.Films, o.Series))
	if o.UneConnexion > 0 {
		h.faux.UneSeuleConnexion(o.UneConnexion)
	}

	// --- configuration de mytv : secrets dans des fichiers 0600 du dossier temporaire.
	secret := func(nom, valeur string) (string, error) {
		p := filepath.Join(h.dossier, nom)
		return p, os.WriteFile(p, []byte(valeur), 0o600)
	}
	pSig, err1 := secret("signature", secretAleatoire()+secretAleatoire())
	pU, err2 := secret("iptv_u", iptvU)
	pP, err3 := secret("iptv_p", iptvP)
	if err := errors.Join(err1, err2, err3); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", o.Ecoute)
	if err != nil {
		return nil, fmt.Errorf("ecoute sur %s : %w", o.Ecoute, err)
	}
	h.URL = "http://" + ln.Addr().String()
	ui := ""
	if o.Dist != "" {
		if ui, err = filepath.Abs(o.Dist); err != nil {
			ln.Close()
			return nil, err
		}
	}
	// Compte technique (Cast, lecteurs sans cookie) : DESACTIVE par defaut, sinon les
	// requetes sans cookie venant de 127.0.0.1 seraient lues et masqueraient les 401
	// des scenarios de session expiree.
	compteLocal := ""
	if o.CompteLocal {
		compteLocal = `, plages_locales: ["127.0.0.1/32"], local_utilisateur: "` + o.Utilisateur + `"`
	}
	yaml := fmt.Sprintf(`
serveur:
  ecoute: %q
  secret_signature: { fichier: %q }
  hotes_segments_autorises: ["127.0.0.1"]
  ui_dossier: %q
  delai_arret: 5s
stockage: { fichier: %q }
fournisseurs:
  - id: iptva
    type: xtream
    base: %q
    utilisateur: { fichier: %q }
    mot_de_passe: { fichier: %q }
    connexions_max: 1
    rafraichissement: { au_demarrage: false, delai_max: 60s }
bail: { ttl: 90s, patience_max: 40s, reessai: 500ms }
comptes: { cookie_securise: false%s }
journal: { niveau: %s }
diagnostic: { dossier: %q }
`, ln.Addr().String(), pSig, ui, filepath.Join(h.dossier, "mytv.db"), h.FournisseurURL, pU, pP, compteLocal,
		map[bool]string{true: "debug", false: "warn"}[o.Verbeux], filepath.Join(h.dossier, "diag"))
	cfg, err := config.Lire(strings.NewReader(yaml))
	if err != nil {
		ln.Close()
		return nil, err
	}
	// Hachage allege : le minimum accepte par la validation (OWASP), car le harnais
	// cree des comptes a chaque lancement et doit demarrer vite.
	cfg.Comptes.Argon2.MemoireKio, cfg.Comptes.Argon2.Iterations = 19*1024, 2

	app, err := appli.Construire(ctx, cfg, appli.Options{Journal: log, Ecouteur: ln})
	if err != nil {
		ln.Close()
		return nil, err
	}
	h.app = app
	for _, c := range []struct {
		nom   string
		admin bool
	}{{o.Utilisateur, false}, {o.Admin, true}} {
		if _, err := app.Comptes().CreerUtilisateur(ctx, c.nom, o.MotDePasse, c.admin); err != nil {
			return nil, fmt.Errorf("utilisateur %s : %w", c.nom, err)
		}
	}
	if err := app.Synchroniser(ctx); err != nil {
		return nil, fmt.Errorf("synchronisation initiale : %w", err)
	}

	runCtx, arreter := context.WithCancel(ctx)
	h.arreter = arreter
	go func() { h.erreur = app.Lancer(runCtx); close(h.termine) }()

	if err := h.demarrerControle(o.Controle); err != nil {
		return nil, err
	}
	h.appliquerBase()
	for _, s := range o.Pannes {
		p, _ := AnalyserPanne(s)
		h.Panne(p.Statut, p.Duree)
	}
	return h, nil
}

// Attendre rend la main quand mytv s'arrete (annulation du contexte ou Fermer).
func (h *Harnais) Attendre() error { <-h.termine; return h.erreur }

// Fermer arrete tout et supprime le dossier temporaire ; idempotent.
func (h *Harnais) Fermer() {
	if h.arreter != nil {
		h.arreter()
		h.arreter = nil
		select {
		case <-h.termine:
		case <-time.After(15 * time.Second):
		}
	} else if h.app != nil {
		h.app.Fermer()
	}
	h.app = nil
	if h.ctrl != nil {
		_ = h.ctrl.Close()
		h.ctrl = nil
	}
	for i := len(h.nettoyer) - 1; i >= 0; i-- {
		h.nettoyer[i]()
	}
	h.nettoyer = nil
	if h.dossier != "" {
		_ = os.RemoveAll(h.dossier)
		h.dossier = ""
	}
}

// Faux donne le faux fournisseur (compteurs, pannes) aux tests.
func (h *Harnais) Faux() *fauxiptv.Faux { return h.faux }
