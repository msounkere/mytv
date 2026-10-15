// Package metadonnees recupere A LA DEMANDE les details des films et series
// (synopsis, genre, duree, casting...) aupres du fournisseur (get_vod_info,
// get_series_info) et les garde longtemps.
//
// Ces appels n'ouvrent aucun flux et ne comptent pas dans la connexion unique, mais
// le fournisseur est fragile et peut nous bannir : ils sont donc
//   - JAMAIS declenches par une liste, seulement par l'ouverture d'une fiche ;
//   - fusionnes (un seul appel par contenu a la fois, singleflight) ;
//   - bornes en concurrence et en debit ;
//   - suspendus par un coupe-circuit apres un refus 401/403/458 ;
//   - suspendus pendant l'import du catalogue.
//
// Un echec ne vide jamais ce qui est deja stocke : l'appelant recoit alors les
// anciens details, ou rien, mais toujours de quoi afficher la fiche de la liste.
package metadonnees

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"mytv/internal/config"
	"mytv/internal/domain"
)

// Statut dit d'ou viennent les details rendus, pour que l'interface choisisse le message.
type Statut string

const (
	Ok           Statut = "ok"           // frais (stockes depuis moins de duree_vie, ou recuperes a l'instant)
	Perime       Statut = "perime"       // anciens details, le fournisseur n'a pas pu les rafraichir
	Indisponible Statut = "indisponible" // echec fournisseur, coupe-circuit, debit : rien de plus que la liste
	Absent       Statut = "absent"       // le fournisseur n'a pas de fiche pour ce contenu
	NonDemande   Statut = "non_demande"  // details=non et rien en cache : aucun appel n'a ete fait
)

// Erreurs de decision (aucune n'est une panne du fournisseur). Elles enveloppent des
// erreurs typees de domain pour que l'API les traduise comme avant.
var (
	ErrDebit     = fmt.Errorf("metadonnees : debit maximal atteint : %w", domain.ErrPlaceIndisponible)
	ErrPause     = fmt.Errorf("metadonnees : import du catalogue en cours : %w", domain.ErrPlaceIndisponible)
	ErrDesactive = errors.New("metadonnees : recuperation desactivee")
)

// Options : Config, Fournisseurs et Stockage sont obligatoires.
type Options struct {
	Config       config.Metadonnees
	Fournisseurs map[string]domain.FournisseurDetails // par identifiant de fournisseur
	Stockage     domain.StockageDetails
	// Pause rend vrai pendant l'import du catalogue : aucun appel n'est alors fait.
	Pause           func() bool
	Horloge         func() time.Time
	Journal         *slog.Logger
	Instrumentation domain.Instrumentation
}

// Service. Utiliser Nouveau.
type Service struct {
	cfg      config.Metadonnees
	fourn    map[string]domain.FournisseurDetails
	stock    domain.StockageDetails
	pause    func() bool
	horloge  func() time.Time
	log      *slog.Logger
	inst     domain.Instrumentation
	groupe   groupe
	places   chan struct{} // concurrence amont
	mu       sync.Mutex
	appels   []time.Time // instants des appels de la derniere minute
	ouvert   time.Time   // coupe-circuit ouvert jusqu'a (zero = ferme)
	sonde    bool        // une sonde de reprise est en cours
	derniere error       // refus qui a ouvert le coupe-circuit
	echecs   map[string]echec
	memoire  map[string]saisonsMemoire
}

type echec struct {
	jusqua time.Time
	statut Statut
}

type saisonsMemoire struct {
	jusqua  time.Time
	saisons []domain.Saison
	details *domain.DetailsSerie
}

const (
	echecsMax  = 4096
	memoireMax = 32
)

// Nouveau valide les dependances.
func Nouveau(opt Options) (*Service, error) {
	c := opt.Config
	if opt.Stockage == nil {
		return nil, errors.New("metadonnees : stockage absent")
	}
	if c.ConcurrenceMax < 1 || c.AppelsParMinute < 1 || c.Delai <= 0 || c.DureeVie <= 0 || c.CoupeCircuit <= 0 {
		return nil, errors.New("metadonnees : reglages invalides (concurrence_max, appels_par_minute, delai, duree_vie, coupe_circuit)")
	}
	s := &Service{cfg: c, fourn: opt.Fournisseurs, stock: opt.Stockage, pause: opt.Pause, horloge: opt.Horloge,
		log: opt.Journal, inst: opt.Instrumentation, places: make(chan struct{}, c.ConcurrenceMax),
		echecs: map[string]echec{}, memoire: map[string]saisonsMemoire{}}
	if s.horloge == nil {
		s.horloge = time.Now
	}
	if s.log == nil {
		s.log = slog.New(slog.DiscardHandler)
	}
	if s.inst == nil {
		s.inst = domain.SansInstrumentation{}
	}
	return s, nil
}

// Disponible : ce fournisseur sait-il donner des details ?
func (s *Service) Disponible(fournisseur string) bool {
	return s.cfg.Actif && s.fourn[fournisseur] != nil
}

// ---------------------------------------------------------------- admission

// admettre decide si un appel au fournisseur est permis MAINTENANT. Elle ne
// reserve rien ; reserverDebit le fait au moment de l'appel.
func (s *Service) admettre() error {
	if !s.cfg.Actif {
		return ErrDesactive
	}
	if s.pause != nil && s.pause() {
		s.inst.Compter("metadonnees_appels_total", 1, "resultat", "pause")
		return ErrPause
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.horloge()
	if !s.ouvert.IsZero() {
		if now.Before(s.ouvert) || s.sonde {
			s.inst.Compter("metadonnees_appels_total", 1, "resultat", "coupe_circuit")
			return fmt.Errorf("metadonnees : appels suspendus apres un refus du fournisseur : %w", s.derniere)
		}
		s.sonde = true // le delai est passe : UN appel de reprise, les autres attendent son verdict
	}
	return nil
}

// reserverDebit compte l'appel dans la minute glissante, ou le refuse.
func (s *Service) reserverDebit() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.horloge()
	garde := s.appels[:0]
	for _, t := range s.appels {
		if now.Sub(t) < time.Minute {
			garde = append(garde, t)
		}
	}
	s.appels = garde
	if len(s.appels) >= s.cfg.AppelsParMinute {
		s.inst.Compter("metadonnees_appels_total", 1, "resultat", "debit")
		s.sonde = false // l'appel n'aura pas lieu : la sonde de reprise non plus
		return ErrDebit
	}
	s.appels = append(s.appels, now)
	return nil
}

// verdict met a jour le coupe-circuit apres un appel reel.
func (s *Service) verdict(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sonde = false
	if errors.Is(err, domain.ErrIdentifiants) || errors.Is(err, domain.ErrRefuseIP) {
		if s.ouvert.IsZero() || !s.horloge().Before(s.ouvert) {
			s.log.Warn("details : fournisseur a refuse la requete, appels suspendus", "duree", s.cfg.CoupeCircuit.Std(), "erreur", err)
		}
		s.ouvert = s.horloge().Add(s.cfg.CoupeCircuit.Std())
		s.derniere = err
		return
	}
	if err == nil || errors.Is(err, domain.ErrIntrouvable) {
		s.ouvert = time.Time{} // le fournisseur nous repond normalement
	}
}

// appeler execute `f` sous les bornes : concurrence, debit, delai. Le contexte de `f`
// est detache de l'appelant (voir groupe) et borne par metadonnees.delai.
func (s *Service) appeler(ctx context.Context, f func(context.Context) (any, error)) (any, error) {
	if err := s.admettre(); err != nil {
		return nil, err
	}
	ctx, annuler := context.WithTimeout(context.WithoutCancel(ctx), s.cfg.Delai.Std())
	defer annuler()
	select {
	case s.places <- struct{}{}:
	case <-ctx.Done():
		s.mu.Lock()
		s.sonde = false
		s.mu.Unlock()
		s.inst.Compter("metadonnees_appels_total", 1, "resultat", "attente")
		return nil, fmt.Errorf("metadonnees : file d'attente pleine : %w", domain.ErrPlaceIndisponible)
	}
	defer func() { <-s.places }()
	if err := s.reserverDebit(); err != nil {
		return nil, err
	}
	debut := s.horloge()
	v, err := f(ctx)
	s.verdict(err)
	s.inst.Observer("metadonnees_appel_secondes", s.horloge().Sub(debut).Seconds())
	switch {
	case err == nil:
		s.inst.Compter("metadonnees_appels_total", 1, "resultat", "ok")
	case errors.Is(err, domain.ErrIntrouvable):
		s.inst.Compter("metadonnees_appels_total", 1, "resultat", "introuvable")
	default:
		s.inst.Compter("metadonnees_appels_total", 1, "resultat", "echec")
	}
	return v, err
}

// ---------------------------------------------------------------- echecs recents

func (s *Service) echecRecent(cle string) (Statut, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.echecs[cle]
	if !ok {
		return "", false
	}
	if !s.horloge().Before(e.jusqua) {
		delete(s.echecs, cle)
		return "", false
	}
	return e.statut, true
}

func (s *Service) noterEchec(cle string, st Statut) {
	if s.cfg.EchecAttente <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.horloge()
	if len(s.echecs) >= echecsMax {
		for k, e := range s.echecs {
			if !now.Before(e.jusqua) {
				delete(s.echecs, k)
			}
		}
		if len(s.echecs) >= echecsMax {
			clear(s.echecs) // jamais de croissance sans borne
		}
	}
	s.echecs[cle] = echec{jusqua: now.Add(s.cfg.EchecAttente.Std()), statut: st}
}

func (s *Service) oublierEchec(cle string) {
	s.mu.Lock()
	delete(s.echecs, cle)
	s.mu.Unlock()
}

func (s *Service) frais(recupere time.Time) bool {
	return s.horloge().Sub(recupere) < s.cfg.DureeVie.Std()
}

// statutErreur : ce que l'appelant doit dire de l'echec d'une recuperation de details.
func statutErreur(err error) (st Statut, memoriser bool) {
	switch {
	case errors.Is(err, domain.ErrIntrouvable):
		return Absent, true
	case errors.Is(err, ErrDebit), errors.Is(err, ErrPause), errors.Is(err, ErrDesactive),
		errors.Is(err, domain.ErrIdentifiants), errors.Is(err, domain.ErrRefuseIP), errors.Is(err, domain.ErrPlaceIndisponible):
		return Indisponible, false // decision generale, pas un fait sur CE contenu
	}
	return Indisponible, true
}

// ---------------------------------------------------------------- films

// ResultatFilm : details (nil = rien de plus que la liste) et leur provenance.
type ResultatFilm struct {
	Details *domain.DetailsFilm
	Statut  Statut
}

// Film rend les details d'un film. recuperer=faux (details=non) ne fait JAMAIS d'appel.
func (s *Service) Film(ctx context.Context, f domain.Film, recuperer bool) ResultatFilm {
	stocke, quand, err := s.stock.DetailsFilm(ctx, f.ID)
	if err != nil && !errors.Is(err, domain.ErrNonTrouve) {
		s.log.Warn("details : lecture du stockage", "erreur", err)
	}
	a := err == nil && stocke != nil
	if a && s.frais(quand) {
		return ResultatFilm{stocke, Ok}
	}
	degrade := func(st Statut) ResultatFilm {
		if a {
			return ResultatFilm{stocke, Perime}
		}
		return ResultatFilm{nil, st}
	}
	fd := s.fourn[f.FournisseurID]
	if !recuperer {
		return degrade(NonDemande)
	}
	if fd == nil || !s.cfg.Actif {
		return degrade(Indisponible)
	}
	cle := "f|" + string(f.ID)
	if st, ok := s.echecRecent(cle); ok {
		return degrade(st)
	}
	v, err := s.groupe.faire(ctx, cle, func() (any, error) {
		return s.appeler(ctx, func(c context.Context) (any, error) {
			n, err := fd.DetailsFilm(c, f.Numero)
			if err != nil {
				return nil, err
			}
			if n == nil || n.Vide() {
				return domain.DetailsFilm{}, nil
			}
			// Fusion avec l'existant : un champ que le fournisseur ne donne plus reste connu.
			fin := *n
			if a {
				fin = stocke.Fusionner(*n)
			}
			if err := s.stock.EcrireDetailsFilm(c, f.ID, *n, s.horloge()); err != nil {
				s.log.Warn("details : ecriture du stockage", "erreur", err)
			}
			return fin, nil
		})
	})
	if err != nil {
		if ctx.Err() != nil {
			return degrade(Indisponible)
		}
		st, mem := statutErreur(err)
		if mem {
			s.noterEchec(cle, st)
		}
		s.log.Info("details film : pas de details", "film", string(f.ID), "erreur", err)
		return degrade(st)
	}
	d := v.(domain.DetailsFilm)
	if d.Vide() {
		s.noterEchec(cle, Absent)
		return degrade(Absent)
	}
	s.oublierEchec(cle)
	return ResultatFilm{&d, Ok}
}

// ---------------------------------------------------------------- series

// ResultatSerie : comme ResultatFilm.
type ResultatSerie struct {
	Details *domain.DetailsSerie
	Statut  Statut
}

type infoSerie struct {
	saisons []domain.Saison
	details domain.DetailsSerie
	fusion  *domain.DetailsSerie // details fusionnes avec l'existant, rendus a l'appelant
}

// infoSerie fait (ou reutilise) UN get_series_info. La liste des saisons et les
// details de la serie en sortent ensemble : ouvrir une fiche ne coute qu'un appel.
func (s *Service) infoSerie(ctx context.Context, sr domain.SerieTV) (*infoSerie, error) {
	fd := s.fourn[sr.FournisseurID]
	if fd == nil {
		return nil, ErrDesactive
	}
	cle := "s|" + string(sr.ID)
	if m, ok := s.memoireSaisons(cle); ok {
		return &infoSerie{saisons: m.saisons, details: *m.details, fusion: m.details}, nil
	}
	v, err := s.groupe.faire(ctx, cle, func() (any, error) {
		return s.appeler(ctx, func(c context.Context) (any, error) {
			ss, d, err := fd.SaisonsEtDetails(c, sr.Numero)
			if err != nil {
				return nil, err
			}
			info := &infoSerie{saisons: ss}
			if d != nil {
				info.details = *d
			}
			fin := info.details
			// Relecture dans le vol : un autre chemin a pu ecrire entre-temps.
			if st, _, err := s.stock.DetailsSerie(c, sr.ID); err == nil && st != nil {
				fin = st.Fusionner(info.details)
			}
			if !info.details.Vide() {
				if err := s.stock.EcrireDetailsSerie(c, sr.ID, info.details, s.horloge()); err != nil {
					s.log.Warn("details : ecriture du stockage", "erreur", err)
				}
			}
			info.fusion = &fin
			s.garderSaisons(cle, info)
			return info, nil
		})
	})
	if err != nil {
		return nil, err
	}
	return v.(*infoSerie), nil
}

func (s *Service) memoireSaisons(cle string) (saisonsMemoire, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.memoire[cle]
	if !ok {
		return m, false
	}
	if !s.horloge().Before(m.jusqua) {
		delete(s.memoire, cle)
		return m, false
	}
	return m, true
}

func (s *Service) garderSaisons(cle string, info *infoSerie) {
	if s.cfg.MemoireSaisons <= 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.horloge()
	if len(s.memoire) >= memoireMax {
		for k, m := range s.memoire {
			if !now.Before(m.jusqua) {
				delete(s.memoire, k)
			}
		}
		if len(s.memoire) >= memoireMax {
			clear(s.memoire)
		}
	}
	d := info.fusion
	if d == nil {
		d = &info.details
	}
	s.memoire[cle] = saisonsMemoire{jusqua: now.Add(s.cfg.MemoireSaisons.Std()), saisons: info.saisons, details: d}
}

// Serie rend les details d'une serie (bloc info de get_series_info).
func (s *Service) Serie(ctx context.Context, sr domain.SerieTV, recuperer bool) ResultatSerie {
	stocke, quand, err := s.stock.DetailsSerie(ctx, sr.ID)
	if err != nil && !errors.Is(err, domain.ErrNonTrouve) {
		s.log.Warn("details : lecture du stockage", "erreur", err)
	}
	a := err == nil && stocke != nil
	if a && s.frais(quand) {
		return ResultatSerie{stocke, Ok}
	}
	degrade := func(st Statut) ResultatSerie {
		if a {
			return ResultatSerie{stocke, Perime}
		}
		return ResultatSerie{nil, st}
	}
	if !recuperer {
		return degrade(NonDemande)
	}
	if s.fourn[sr.FournisseurID] == nil || !s.cfg.Actif {
		return degrade(Indisponible)
	}
	cle := "s|" + string(sr.ID)
	if st, ok := s.echecRecent(cle); ok {
		return degrade(st)
	}
	info, err := s.infoSerie(ctx, sr)
	if err != nil {
		if ctx.Err() != nil {
			return degrade(Indisponible)
		}
		st, mem := statutErreur(err)
		if mem {
			s.noterEchec(cle, st)
		}
		s.log.Info("details serie : pas de details", "serie", string(sr.ID), "erreur", err)
		return degrade(st)
	}
	if info.fusion == nil || info.fusion.Vide() {
		s.noterEchec(cle, Absent)
		return degrade(Absent)
	}
	s.oublierEchec(cle)
	return ResultatSerie{info.fusion, Ok}
}

// Saisons liste saisons et episodes (avec leurs details). Contrairement aux details,
// une erreur est rendue telle quelle : sans la liste, la fiche n'a rien a montrer.
// Elle passe par les memes bornes et partage l'appel avec Serie.
func (s *Service) Saisons(ctx context.Context, sr domain.SerieTV) ([]domain.Saison, error) {
	info, err := s.infoSerie(ctx, sr)
	if err != nil {
		return nil, err
	}
	return info.saisons, nil
}
