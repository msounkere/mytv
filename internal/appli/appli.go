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
	"net/url"
	"os"
	"path/filepath"
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
	"mytv/internal/hotes"
	"mytv/internal/images"
	"mytv/internal/live"
	"mytv/internal/metadonnees"
	"mytv/internal/profil"
	"mytv/internal/remux"
	"mytv/internal/sauvegarde"
	"mytv/internal/stockage"
	"mytv/internal/supervision"
	"mytv/internal/telemetrie"
	"mytv/internal/vod"
	"mytv/internal/xtream"
)

// Options : tout est facultatif ; les tests injectent l'ecouteur et le journal.
type Options struct {
	Journal *slog.Logger
	// Ecouteur remplace net.Listen(serveur.ecoute) (tests). Nil = ecoute sur serveur.ecoute.
	Ecouteur net.Listener
	// ClientWebhook remplace le client HTTP des notifications (tests : certificat de test).
	ClientWebhook supervision.HTTPDoer
}

// Appli est l'application assemblee. Lancer la fait vivre ; Fermer libere tout.
type Appli struct {
	cfg     *config.Config
	journal *slog.Logger
	ln      net.Listener
	srv     *http.Server

	stockage      *stockage.Base
	epgBase       *epg.Base
	profils       *profil.Base
	comptesDB     interface{ Close() error }
	comptes       *comptes.Service
	baux          *baux.Gestionnaire
	relais        *live.Relais
	lanceur       *remux.Lanceur
	clients       []*xtream.Client
	catalogue     *catalogue.Synchroniseur
	guide         *epg.Service
	telemetrie    *telemetrie.Telemetrie
	supervision   *supervision.Moniteur
	sauvegarde    *sauvegarde.Planificateur
	clientWebhook supervision.HTTPDoer

	fermer sync.Once
}

// Construire charge les secrets, ouvre les bases, construit chaque composant et
// ouvre l'ecoute. En cas d'echec il libere ce qu'il avait ouvert.
func Construire(ctx context.Context, cfg *config.Config, opt Options) (res *Appli, err error) {
	if err := Verifier(cfg); err != nil {
		return nil, err
	}
	a := &Appli{cfg: cfg, journal: opt.Journal, clientWebhook: opt.ClientWebhook}
	if a.journal == nil {
		a.journal = NouveauJournal(cfg.Journal.Niveau, os.Stderr)
	}
	defer func() {
		if err != nil {
			a.Fermer()
		}
	}()

	// Reglages valides mais risques : le programme demarre, et le dit.
	for _, w := range cfg.Avertissements() {
		a.journal.Warn(w)
	}

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
		// Ce que le fournisseur peut nous faire joindre (redirections, adresses reelles) :
		// meme politique que les segments du direct (serveur.hotes_segments_autorises).
		filtre, err := hotes.Nouveau(f.Base, cfg.Serveur.HotesSegmentsAutorises, nil)
		if err != nil {
			return nil, fmt.Errorf("fournisseur %s : %w", f.ID, err)
		}
		c := xtream.Nouveau(f, u, p, xtream.AvecJournal(a.journal.With("fournisseur", f.ID)), xtream.AvecFiltreHotes(filtre))
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

	// --- supervision du compte fournisseur : avant la synchronisation (elle l'observe),
	// le direct et l'API (ils lui signalent les refus).
	if a.supervision, err = a.moniteur(actifs, fournisseurs); err != nil {
		return nil, err
	}

	// --- sauvegarde quotidienne de la base ; un echec previent comme les autres alertes.
	if a.sauvegarde, err = a.planificateurSauvegarde(cfg.DossierSauvegardes()); err != nil {
		return nil, err
	}

	// --- synchronisation du catalogue et du guide.
	if a.catalogue, err = a.synchroniseur(actifs, fournisseurs); err != nil {
		return nil, err
	}
	if a.guide, err = a.serviceGuide(actifs, fournisseurs); err != nil {
		return nil, err
	}

	// --- direct : le relais est mono-fournisseur (le plus prioritaire).
	direct := fournisseurDuDirect(actifs)
	// Le lanceur de ffmpeg est UN SEUL pour les films et le direct : remux.processus_max borne
	// l'ensemble (un direct converti compte pour un processus).
	rl, rs, exe := remux.DepuisConfig(cfg.Remux)
	lanceur, err := remux.NouveauLanceur(rl, exe)
	if err != nil {
		return nil, err
	}
	a.lanceur = lanceur
	if a.relais, err = live.Nouveau(live.Options{
		Sondeur: remux.NouveauSondeur(rs, exe, nil), Lanceur: lanceur,
		Fournisseur: fournisseurs[direct.ID], Baux: a.baux, Live: cfg.Live, Bail: cfg.Bail, Reseau: direct.Reseau,
		Secret: []byte(secret), Authentifier: a.comptes.AuthentifierLecture, EstAdmin: a.comptes.EstAdmin,
		HotesAutorises: cfg.Serveur.HotesSegmentsAutorises, Instrumentation: inst, Journal: a.journal.With("module", "live"),
		SignalerRefus: func(err error) { a.supervision.SignalerRefus(direct.ID, err) },
		Expiration:    func() (time.Time, bool) { return a.supervision.Expiration(direct.ID) },
	}); err != nil {
		return nil, err
	}

	// --- films et series.
	lecture, err := a.serviceVOD(fournisseurs, direct)
	if err != nil {
		return nil, err
	}

	// --- images : logos et affiches servis par ce service (contenu mixte, fuite de l'adresse).
	proxyImages, err := a.serviceImages(actifs, direct, []byte(secret))
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
	var details api.Details
	if cfg.Metadonnees.Actif {
		fd := map[string]domain.FournisseurDetails{}
		for _, f := range actifs {
			if d, ok := fournisseurs[f.ID].(domain.FournisseurDetails); ok {
				fd[f.ID] = d
			}
		}
		svc, err := metadonnees.Nouveau(metadonnees.Options{Config: cfg.Metadonnees, Fournisseurs: fd, Stockage: a.stockage,
			Pause: a.catalogue.EnCours, Journal: a.journal.With("module", "metadonnees"), Instrumentation: inst})
		if err != nil {
			return nil, err
		}
		// Menage : des details jamais redemandes depuis 3 fois leur duree de vie ne servent plus.
		if _, err := a.stockage.PurgerDetails(ctx, time.Now().Add(-3*cfg.Metadonnees.DureeVie.Std())); err != nil {
			a.journal.Warn("purge des details", "erreur", err)
		}
		details = svc
	}
	handlerAPI, err := api.Nouveau(api.Options{
		Details: details,
		Config:  cfg.API, Profil: cfg.Profil, Fournisseurs: ids, FournisseurLive: direct.ID,
		Stockage: a.stockage, Video: a.stockage, Export: a.stockage, FinLive: a.relais, VOD: vods,
		Comptes: comptesEtProfils{Comptes: a.comptes, profils: a.profils, journal: a.journal}, Auth: a.comptes,
		EPG: a.epgBase, Etat: a.profils, Lecture: lecture, Live: a.relais.Handler(),
		Admin: map[string]http.Handler{
			"metriques":    http.HandlerFunc(a.telemetrie.Metriques),
			"diagnostic":   http.HandlerFunc(a.telemetrie.Lire),
			"fournisseurs": a.supervision.Fournisseurs(),
			"sante":        a.supervision.Sante(),
		},
		Supervision: a.supervision,
		Evenements:  http.HandlerFunc(a.telemetrie.Recevoir),
		Images:      proxyImages,
		Journal:     a.journal.With("module", "api"),
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

// serviceImages : les hotes des fournisseurs sont de confiance (leurs picons y vivent),
// tout le reste passe par le meme filtre anti-SSRF que les segments.
func (a *Appli) serviceImages(actifs []config.Fournisseur, direct config.Fournisseur, secret []byte) (*images.Service, error) {
	autorises := append([]string(nil), a.cfg.Serveur.HotesSegmentsAutorises...)
	var origines []images.Origine
	for _, f := range actifs {
		if u, err := url.Parse(f.Base); err == nil && u.Hostname() != "" {
			autorises = append(autorises, u.Hostname())
		}
		origines = append(origines, images.Origine{ID: f.ID, Base: f.Base})
	}
	filtre, err := hotes.Nouveau(direct.Base, autorises, nil)
	if err != nil {
		return nil, fmt.Errorf("images : %w", err)
	}
	return images.Nouveau(images.Options{Config: a.cfg.Images, Secret: secret, Fournisseurs: origines, UserAgent: direct.UserAgent,
		Filtre: filtre, Journal: a.journal.With("module", "images")})
}

func (a *Appli) synchroniseur(actifs []config.Fournisseur, fs map[string]domain.Fournisseur) (*catalogue.Synchroniseur, error) {
	var sources []catalogue.Source
	for _, f := range actifs {
		r := f.Rafraichissement
		sources = append(sources, catalogue.Source{Fournisseur: fs[f.ID], Actif: true, AgeMax: r.Catalogue.Std(), Heure: r.Heure,
			AuDemarrage: r.AuDemarre, DelaiMax: r.DelaiMax.Std(), Reprise: r.Reprise.Std()})
	}
	return catalogue.Nouveau(a.stockage, sources, catalogue.Options{Journal: a.journal.With("module", "catalogue"), Instrumentation: a.telemetrie.Instrumentation(), Observateur: a.supervision})
}

// planificateurSauvegarde assemble la sauvegarde quotidienne dans `dossier` ; un echec
// previent comme les autres alertes (journal et webhook).
func (a *Appli) planificateurSauvegarde(dossier string) (*sauvegarde.Planificateur, error) {
	return sauvegarde.Nouveau(sauvegarde.Options{
		Reglages: sauvegarde.Reglages{Heure: a.cfg.Sauvegarde.Heure, Dossier: dossier, Conservation: a.cfg.Sauvegarde.Conservation},
		Copier:   a.stockage.Copier, Journal: a.journal.With("module", "sauvegarde"), Instrumentation: a.telemetrie.Instrumentation(),
		EnEchec: func(error) {
			// Le texte de l'erreur reste dans le journal : le webhook n'a pas besoin d'un chemin.
			a.supervision.SignalerEvenement("sauvegarde_echec", "la sauvegarde de la base a echoue")
		},
	})
}

// moniteur assemble la supervision (US-25). Le secret du webhook est lu ici, une fois
// (Verifier a deja controle qu'il se resout).
func (a *Appli) moniteur(actifs []config.Fournisseur, fs map[string]domain.Fournisseur) (*supervision.Moniteur, error) {
	var sources []supervision.Source
	for _, f := range actifs {
		src := supervision.Source{ID: f.ID, AgeMax: f.Rafraichissement.Catalogue.Std()}
		if x, ok := fs[f.ID].(interface {
			InfoCompte(context.Context) (xtream.InfoCompte, error)
		}); ok {
			src.Info = func(ctx context.Context) (supervision.InfoCompte, error) {
				i, err := x.InfoCompte(ctx)
				return supervision.InfoCompte{Statut: i.Statut, Expiration: i.Expiration,
					ConnexionsMax: i.ConnexionsMax, ConnexionsActives: i.ConnexionsActives}, err
			}
		}
		sources = append(sources, src)
	}
	secret := ""
	if w := a.cfg.Supervision.Webhook; w.URL != "" && w.Secret.Defini() {
		secret, _ = w.Secret.Valeur()
	}
	dossier := filepath.Dir(a.cfg.Stockage.Fichier)
	return supervision.Nouveau(supervision.Options{
		Config: a.cfg.Supervision, Sources: sources, Journal: a.journal.With("module", "supervision"),
		Jauge: a.telemetrie.Jauge, Instrumentation: a.telemetrie.Instrumentation(), SecretWebhook: secret, ClientHTTP: a.clientWebhook,
		DerniereSynchro: a.stockage.DerniereSynchro,
		Base:            a.stockage.Lire,
		Stockage: func() error {
			ctx, annuler := context.WithTimeout(context.Background(), a.cfg.Supervision.DelaiSante.Std())
			defer annuler()
			if err := a.stockage.Ecrire(ctx); err != nil {
				return err
			}
			// Le dossier de la base doit accepter un fichier (le journal WAL y est ecrit).
			f, err := os.CreateTemp(dossier, ".sante-*")
			if err != nil {
				return err
			}
			f.Close()
			return os.Remove(f.Name())
		},
	})
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
	_, rs, exe := remux.DepuisConfig(a.cfg.Remux)
	return vod.Nouveau(vod.Dependances{
		Fournisseurs: fs, Stockage: a.stockage, Baux: a.baux,
		Sondeur: remux.NouveauSondeur(rs, exe, nil), Decideur: remux.Decider, Lanceur: a.lanceur,
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
	tache(a.supervision.Planifier)
	tache(a.sauvegarde.Planifier)
	if a.guide != nil {
		tache(a.guide.Planifier)
	}

	erreur := make(chan error, 1)
	go func() { erreur <- a.srv.Serve(a.ln) }()
	a.journal.Info("demarre", "ecoute", a.Adresse(), "fournisseurs", len(a.clients))
	if h := a.cfg.Sauvegarde.Heure; h != "" {
		a.journal.Info("sauvegarde quotidienne planifiee", "heure", h, "conservation_jours", a.cfg.Sauvegarde.Conservation)
	} else {
		a.journal.Warn("sauvegarde quotidienne desactivee (sauvegarde.heure vide) : mytv --sauvegarder reste possible")
	}

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
		if a.supervision != nil {
			a.supervision.Fermer()
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
