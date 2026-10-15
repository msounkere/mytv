// Package appli assemble mytv : configuration, secrets, stockage, fournisseurs,
// baux, synchronisation planifiee, direct, films, guide, comptes, API. La commande
// cmd/mytv n'en est qu'une enveloppe (arguments, signaux) : tout ce qui se teste
// vit ici.
package appli

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"mytv/internal/api"
	"mytv/internal/baux"
	"mytv/internal/catalogue"
	"mytv/internal/comptes"
	"mytv/internal/config"
	"mytv/internal/domain"
	"mytv/internal/epg"
	"mytv/internal/live"
	"mytv/internal/profil"
	"mytv/internal/remux"
	"mytv/internal/stockage"
	"mytv/internal/telemetrie"
	"mytv/internal/vod"
	"mytv/internal/xtream"
)

// Options : tout est facultatif ; les tests injectent l'ecouteur et le journal.
type Options struct {
	Journal *slog.Logger
	// Ecouteur remplace net.Listen(serveur.ecoute) (tests). Nil = ecoute sur serveur.ecoute.
	Ecouteur net.Listener
}

// Appli est l'application assemblee. Lancer la fait vivre ; Fermer libere tout.
type Appli struct {
	cfg     *config.Config
	journal *slog.Logger
	ln      net.Listener
	srv     *http.Server

	stockage   *stockage.Base
	epgBase    *epg.Base
	profils    *profil.Base
	comptesDB  interface{ Close() error }
	comptes    *comptes.Service
	baux       *baux.Gestionnaire
	relais     *live.Relais
	clients    []*xtream.Client
	catalogue  *catalogue.Synchroniseur
	guide      *epg.Service
	telemetrie *telemetrie.Telemetrie

	fermer sync.Once
}

// Construire charge les secrets, ouvre les bases, construit chaque composant et
// ouvre l'ecoute. En cas d'echec il libere ce qu'il avait ouvert.
func Construire(ctx context.Context, cfg *config.Config, opt Options) (res *Appli, err error) {
	if err := Verifier(cfg); err != nil {
		return nil, err
	}
	a := &Appli{cfg: cfg, journal: opt.Journal}
	if a.journal == nil {
		a.journal = NouveauJournal(cfg.Journal.Niveau, os.Stderr)
	}
	defer func() {
		if err != nil {
			a.Fermer()
		}
	}()

	secret, _ := cfg.Serveur.SecretSigne.Valeur() // deja verifie par Verifier

	// --- stockage : un seul fichier, une table de migrations par module.
	fichier := cfg.Stockage.Fichier
	if a.stockage, err = stockage.Ouvrir(ctx, fichier); err != nil {
		return nil, err
	}
	if a.epgBase, err = epg.Ouvrir(ctx, fichier, nil); err != nil {
		return nil, err
	}
	if a.profils, err = profil.Ouvrir(ctx, fichier, profil.Options{Catalogue: a.stockage,
		HistoriqueMax: cfg.Profil.HistoriqueMax, FavorisMax: cfg.Profil.FavorisMax, ReglagesMax: cfg.Profil.ReglagesMax}); err != nil {
		return nil, err
	}
	db, err := comptes.OuvrirBase(fichier)
	if err != nil {
		return nil, err
	}
	a.comptesDB = db

	// --- fournisseurs et baux.
	var actifs []config.Fournisseur
	fournisseurs := map[string]domain.Fournisseur{}
	max := map[string]int{}
	for _, f := range cfg.Fournisseurs {
		if !f.EstActif() {
			continue
		}
		u, _ := f.Utilisateur.Valeur()
		p, _ := f.MotDePasse.Valeur()
		c := xtream.Nouveau(f, u, p, xtream.AvecJournal(a.journal.With("fournisseur", f.ID)))
		a.clients = append(a.clients, c)
		fournisseurs[f.ID] = c
		max[f.ID] = f.ConnexionsMax
		actifs = append(actifs, f)
	}
	if a.baux, err = baux.Nouveau(cfg.Bail, max, nil); err != nil {
		return nil, err
	}

	// --- comptes : la deconnexion rend la place de connexion (Terminer).
	if a.comptes, err = comptes.Nouveau(ctx, comptes.Options{DB: db, Config: cfg.Comptes, Terminateur: a.baux,
		Journal: a.journal.With("module", "comptes")}); err != nil {
		return nil, err
	}
	a.comptes.Purger(ctx)

	// --- telemetrie : creee apres les comptes (authentification) et avant les modules
	// qui publient des mesures.
	if a.telemetrie, err = telemetrie.Nouveau(telemetrie.Options{Config: cfg.Diagnostic,
		Authentifier: a.comptes.Authentifier, EstAdmin: a.comptes.EstAdmin, Journal: a.journal.With("module", "telemetrie")}); err != nil {
		return nil, err
	}
	inst := a.telemetrie.Instrumentation()
	a.baux.AvecInstrumentation(inst)

	// --- synchronisation du catalogue et du guide.
	if a.catalogue, err = a.synchroniseur(actifs, fournisseurs); err != nil {
		return nil, err
	}
	if a.guide, err = a.serviceGuide(actifs, fournisseurs); err != nil {
		return nil, err
	}

	// --- direct : le relais est mono-fournisseur (le plus prioritaire).
	direct := fournisseurDuDirect(actifs)
	if a.relais, err = live.Nouveau(live.Options{
		Fournisseur: fournisseurs[direct.ID], Baux: a.baux, Live: cfg.Live, Bail: cfg.Bail, Reseau: direct.Reseau,
		Secret: []byte(secret), Authentifier: a.comptes.AuthentifierLecture, EstAdmin: a.comptes.EstAdmin,
		HotesAutorises: cfg.Serveur.HotesSegmentsAutorises, Instrumentation: inst, Journal: a.journal.With("module", "live"),
	}); err != nil {
		return nil, err
	}

	// --- films et series.
	lecture, err := a.serviceVOD(fournisseurs, direct)
	if err != nil {
		return nil, err
	}

	// --- API.
	vods := map[string]domain.FournisseurVOD{}
	ids := make([]string, 0, len(actifs))
	for _, f := range actifs {
		ids = append(ids, f.ID)
		if v, ok := fournisseurs[f.ID].(domain.FournisseurVOD); ok {
			vods[f.ID] = v
		}
	}
	handlerAPI, err := api.Nouveau(api.Options{
		Config: cfg.API, Profil: cfg.Profil, Fournisseurs: ids, FournisseurLive: direct.ID,
		Stockage: a.stockage, Video: a.stockage, Export: a.stockage, FinLive: a.relais, VOD: vods,
		Comptes: comptesEtProfils{Comptes: a.comptes, profils: a.profils, journal: a.journal}, Auth: a.comptes,
		EPG: a.epgBase, Etat: a.profils, Lecture: lecture, Live: a.relais.Handler(),
		Admin: map[string]http.Handler{
			"metriques":  http.HandlerFunc(a.telemetrie.Metriques),
			"diagnostic": http.HandlerFunc(a.telemetrie.Lire),
		},
		Evenements: http.HandlerFunc(a.telemetrie.Recevoir),
		Journal:    a.journal.With("module", "api"),
	})
	if err != nil {
		return nil, err
	}
	a.srv = api.Serveur(cfg.Serveur.Ecoute, a.racine(handlerAPI.Handler()), cfg.API)

	// --- ecoute : la SEULE de ce programme.
	if a.ln = opt.Ecouteur; a.ln == nil {
		if a.ln, err = net.Listen("tcp", cfg.Serveur.Ecoute); err != nil {
			return nil, fmt.Errorf("ecoute sur %s : %w", cfg.Serveur.Ecoute, err)
		}
	}
	return a, nil
}

// fournisseurDuDirect : le plus prioritaire (petit nombre), a egalite le premier declare.
func fournisseurDuDirect(actifs []config.Fournisseur) config.Fournisseur {
	best := actifs[0]
	for _, f := range actifs[1:] {
		if f.Priorite < best.Priorite {
			best = f
		}
	}
	return best
}

func (a *Appli) synchroniseur(actifs []config.Fournisseur, fs map[string]domain.Fournisseur) (*catalogue.Synchroniseur, error) {
	var sources []catalogue.Source
	for _, f := range actifs {
		r := f.Rafraichissement
		sources = append(sources, catalogue.Source{Fournisseur: fs[f.ID], Actif: true, AgeMax: r.Catalogue.Std(), Heure: r.Heure,
			AuDemarrage: r.AuDemarre, DelaiMax: r.DelaiMax.Std(), Reprise: r.Reprise.Std()})
	}
	return catalogue.Nouveau(a.stockage, sources, catalogue.Options{Journal: a.journal.With("module", "catalogue"), Instrumentation: a.telemetrie.Instrumentation()})
}

// serviceGuide ne couvre que les fournisseurs qui publient un guide XMLTV.
func (a *Appli) serviceGuide(actifs []config.Fournisseur, fs map[string]domain.Fournisseur) (*epg.Service, error) {
	var sources []epg.Source
	for _, f := range actifs {
		if _, ok := fs[f.ID].(domain.SourceEPG); !ok {
			a.journal.Info("guide non disponible pour ce fournisseur", "fournisseur", f.ID)
			continue
		}
		r := f.Rafraichissement
		sources = append(sources, epg.Source{Fournisseur: fs[f.ID], Actif: true, AgeMax: r.EPG.Std(), DelaiMax: r.DelaiMax.Std(), Reprise: r.Reprise.Std()})
	}
	if len(sources) == 0 {
		return nil, nil
	}
	lieu, err := time.LoadLocation(a.cfg.EPG.Fuseau)
	if err != nil {
		return nil, err
	}
	return epg.Nouveau(a.stockage, a.epgBase, sources, epg.Options{Journal: a.journal.With("module", "epg"), Instrumentation: a.telemetrie.Instrumentation(),
		Passe: a.cfg.EPG.Passe.Std(), Futur: a.cfg.EPG.Futur.Std(), Lieu: lieu, TailleMax: a.cfg.EPG.TailleMax})
}

func (a *Appli) serviceVOD(fs map[string]domain.Fournisseur, direct config.Fournisseur) (domain.ServiceVOD, error) {
	rl, rs, exe := remux.DepuisConfig(a.cfg.Remux)
	lanceur, err := remux.NouveauLanceur(rl, exe)
	if err != nil {
		return nil, err
	}
	return vod.Nouveau(vod.Dependances{
		Fournisseurs: fs, Stockage: a.stockage, Baux: a.baux,
		Sondeur: remux.NouveauSondeur(rs, exe, nil), Decideur: remux.Decider, Lanceur: lanceur,
		Instrumentation: a.telemetrie.Instrumentation(),
	}, vod.DepuisConfig(a.cfg, direct.UserAgent))
}

// racine route : /api/ vers l'API (authentification par defaut), /sante sans
// authentification, le reste vers l'interface si elle est configuree.
func (a *Appli) racine(api http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("/api/", api)
	// /sante : meme reponse que /api/v1/sante, sans detail (pas de version, de chemin ni d'etat interne).
	mux.HandleFunc("GET /sante", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		_, _ = w.Write([]byte(`{"statut":"ok"}` + "\n"))
	})
	if d := a.cfg.Serveur.UIDossier; d != "" {
		mux.Handle("/", interfaceStatique(d, a.cfg.Comptes.CookieSecurise))
	}
	return mux
}

// Adresse est l'adresse reellement ecoutee (utile avec un port 0).
func (a *Appli) Adresse() string { return a.ln.Addr().String() }

// Comptes donne acces au service de comptes (creation du premier utilisateur, tests).
func (a *Appli) Comptes() *comptes.Service { return a.comptes }

// Synchroniser force la synchronisation de tous les catalogues (et guides).
func (a *Appli) Synchroniser(ctx context.Context) error {
	err := a.catalogue.SynchroniserTous(ctx)
	if a.guide != nil {
		err = errors.Join(err, a.guide.RafraichirTous(ctx))
	}
	return err
}

// Lancer sert les requetes et fait tourner les synchronisations planifiees jusqu'a
// l'annulation de ctx, puis arrete proprement : plus de nouvelle connexion, requetes
// en cours terminees (serveur.delai_arret) puis coupees, taches planifiees attendues.
func (a *Appli) Lancer(ctx context.Context) error {
	var taches sync.WaitGroup
	tache := func(f func(context.Context)) {
		taches.Add(1)
		go func() { defer taches.Done(); f(ctx) }()
	}
	tache(a.catalogue.Planifier)
	if a.guide != nil {
		tache(a.guide.Planifier)
	}

	erreur := make(chan error, 1)
	go func() { erreur <- a.srv.Serve(a.ln) }()
	a.journal.Info("demarre", "ecoute", a.Adresse(), "fournisseurs", len(a.clients))

	var err error
	select {
	case err = <-erreur: // le serveur est tombe seul
	case <-ctx.Done():
		a.journal.Info("arret demande")
		arret, annuler := context.WithTimeout(context.Background(), a.cfg.Serveur.DelaiArret.Std())
		defer annuler()
		if e := a.srv.Shutdown(arret); e != nil {
			a.journal.Warn("requetes coupees a l'arret", "delai", a.cfg.Serveur.DelaiArret.Std())
			a.srv.Close()
		}
		err = <-erreur
	}
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	taches.Wait()
	a.Fermer()
	a.journal.Info("arrete")
	return err
}

// Fermer libere tout ce qui a ete ouvert ; idempotent.
func (a *Appli) Fermer() {
	a.fermer.Do(func() {
		if a.srv != nil {
			a.srv.Close()
		}
		if a.ln != nil {
			a.ln.Close()
		}
		if a.relais != nil {
			a.relais.Fermer()
		}
		for _, c := range a.clients {
			c.Fermer()
		}
		if a.telemetrie != nil {
			a.telemetrie.Fermer()
		}
		if a.comptesDB != nil {
			a.comptesDB.Close()
		}
		if a.profils != nil {
			a.profils.Fermer()
		}
		if a.epgBase != nil {
			a.epgBase.Fermer()
		}
		if a.stockage != nil {
			a.stockage.Fermer()
		}
	})
}

// NouveauJournal : journal structure (JSON) au niveau de la configuration.
func NouveauJournal(niveau string, w *os.File) *slog.Logger {
	var n slog.Level
	switch strings.ToLower(niveau) {
	case "debug":
		n = slog.LevelDebug
	case "warn":
		n = slog.LevelWarn
	case "error":
		n = slog.LevelError
	default:
		n = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: n}))
}

// comptesEtProfils ajoute au service de comptes le menage de l'etat d'un profil
// supprime (favoris, historique, reglages) : sans cela, il resterait orphelin.
type comptesEtProfils struct {
	domain.Comptes
	profils *profil.Base
	journal *slog.Logger
}

func (c comptesEtProfils) SupprimerProfil(ctx context.Context, utilisateurID, profilID string) error {
	if err := c.Comptes.SupprimerProfil(ctx, utilisateurID, profilID); err != nil {
		return err
	}
	if err := c.profils.EffacerProfil(ctx, profilID); err != nil {
		// Le profil n'existe plus : l'etat restant est inaccessible, on le journalise.
		c.journal.Error("etat du profil supprime non efface", "erreur", err)
	}
	return nil
}
