package epg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"mytv/internal/domain"
)

var (
	// ErrInconnu : aucun fournisseur de cet identifiant n'est enregistre.
	ErrInconnu = errors.New("epg : fournisseur inconnu")
	// ErrInactif : un fournisseur desactive n'est jamais rafraichi.
	ErrInactif = errors.New("epg : fournisseur inactif")
	// ErrSansChaines : aucune chaine en base pour ce fournisseur (catalogue pas encore
	// synchronise). Rien n'est telecharge ni enregistre : un guide ingere sans chaine a
	// rapprocher serait vide mais declare « a jour » pendant tout AgeMax.
	ErrSansChaines = errors.New("epg : aucune chaine en base, le catalogue n'est pas encore synchronise")
	// ErrSansGuide : le fournisseur ne publie pas de guide XMLTV.
	ErrSansGuide = errors.New("epg : le fournisseur n'a pas de guide XMLTV")
)

// ListeurChaines est la partie de domain.Stockage dont l'EPG a besoin.
type ListeurChaines interface {
	Chaines(ctx context.Context, f domain.FiltreChaines) ([]domain.Chaine, error)
}

// Purgeur est facultatif : un EPGStockage qui sait purger les programmes termines.
type Purgeur interface {
	Purger(ctx context.Context, avant time.Time) (int64, error)
}

// Source associe un fournisseur a ses reglages.
type Source struct {
	Fournisseur domain.Fournisseur // doit aussi implementer domain.SourceEPG
	Actif       bool
	// AgeMax : age au-dela duquel l'EPG est perime (rafraichissement.epg).
	AgeMax time.Duration
	// DelaiMax borne un rafraichissement complet (le vol est detache de ses demandeurs).
	DelaiMax time.Duration
	// Reprise : pause avant de retenter un echec de rafraichissement planifie.
	Reprise time.Duration
}

// Options regle le Service ; Passe, Futur et TailleMax sont les seuls obligatoires.
type Options struct {
	Horloge Horloge
	Journal *slog.Logger
	// Passe / Futur : fenetre gardee autour de « maintenant » (epg.passe, epg.futur).
	Passe, Futur time.Duration
	// Lieu : fuseau des dates XMLTV sans decalage (epg.fuseau). Defaut UTC.
	Lieu *time.Location
	// TailleMax : octets decompresses acceptes (epg.taille_max).
	TailleMax int64
	// IntervallePurge : periodicite de la purge entre deux rafraichissements (defaut 1 h).
	IntervallePurge time.Duration
	// Instrumentation publie epg_total{fournisseur,resultat} et
	// epg_duree_secondes{fournisseur} (nil = rien).
	Instrumentation domain.Instrumentation
}

// Service telecharge, rapproche et stocke les guides.
type Service struct {
	chaines ListeurChaines
	stock   domain.EPGStockage
	sources map[string]Source
	ordre   []string
	opt     Options
	horloge Horloge
	journal *slog.Logger

	mu   sync.Mutex
	vols map[string]*vol
}

// Nouveau cree un Service.
func Nouveau(chaines ListeurChaines, st domain.EPGStockage, sources []Source, opt Options) (*Service, error) {
	if opt.Futur <= 0 || opt.Passe < 0 || opt.TailleMax <= 0 {
		return nil, errors.New("epg : Passe >= 0, Futur > 0 et TailleMax > 0 sont requis")
	}
	s := &Service{chaines: chaines, stock: st, sources: map[string]Source{}, vols: map[string]*vol{},
		opt: opt, horloge: opt.Horloge, journal: opt.Journal}
	if s.horloge == nil {
		s.horloge = HorlogeReelle{}
	}
	if s.journal == nil {
		s.journal = slog.New(slog.DiscardHandler)
	}
	if s.opt.Instrumentation == nil {
		s.opt.Instrumentation = domain.SansInstrumentation{}
	}
	if s.opt.IntervallePurge <= 0 {
		s.opt.IntervallePurge = time.Hour
	}
	for _, src := range sources {
		if src.Fournisseur == nil {
			return nil, errors.New("epg : source sans fournisseur")
		}
		id := src.Fournisseur.ID()
		if _, dup := s.sources[id]; dup {
			return nil, fmt.Errorf("epg : fournisseur %q en double", id)
		}
		if src.DelaiMax <= 0 || src.Reprise <= 0 {
			return nil, fmt.Errorf("epg : fournisseur %q : DelaiMax et Reprise doivent etre > 0", id)
		}
		if _, ok := src.Fournisseur.(domain.SourceEPG); !ok && src.Actif {
			return nil, fmt.Errorf("epg : fournisseur %q : %w", id, ErrSansGuide)
		}
		s.sources[id] = src
		s.ordre = append(s.ordre, id)
	}
	sort.Strings(s.ordre)
	return s, nil
}

type vol struct {
	fait       chan struct{}
	annuler    context.CancelFunc
	force      bool
	attendants int  // protege par Service.mu
	abandonne  bool // idem : plus personne n'attend, le vol est annule
	// ecrits par le meneur avant de fermer « fait » :
	err        error
	telecharge bool
}

// Rafraichir telecharge et stocke le guide, quel que soit son age. Une demande
// simultanee pour le meme fournisseur partage le meme telechargement.
func (s *Service) Rafraichir(ctx context.Context, id string) error {
	_, err := s.executer(ctx, id, true)
	return err
}

// SiPerime ne telecharge que si l'EPG est plus vieux que AgeMax (ou jamais ecrit).
// Renvoie vrai si un telechargement a eu lieu.
func (s *Service) SiPerime(ctx context.Context, id string) (bool, error) {
	return s.executer(ctx, id, false)
}

// RafraichirTous traite les fournisseurs actifs en parallele ; un echec n'arrete pas les autres.
func (s *Service) RafraichirTous(ctx context.Context) error {
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for _, id := range s.ordre {
		if !s.sources[id].Actif {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Rafraichir(ctx, id); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("fournisseur %s : %w", id, err))
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (s *Service) executer(ctx context.Context, id string, force bool) (bool, error) {
	src, ok := s.sources[id]
	if !ok {
		return false, ErrInconnu
	}
	if !src.Actif {
		return false, ErrInactif
	}
	for {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		v := s.rejoindre(ctx, id, src, force)
		select {
		case <-v.fait:
		case <-ctx.Done():
			s.quitter(v)
			return false, ctx.Err()
		}
		// Joint a un vol qui a juge l'EPG frais alors qu'on exigeait un vrai
		// telechargement : on reessaie, le prochain vol sera force.
		if force && v.err == nil && !v.telecharge {
			continue
		}
		return v.telecharge, v.err
	}
}

func (s *Service) rejoindre(ctx context.Context, id string, src Source, force bool) *vol {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.vols[id]
	if v == nil || v.abandonne {
		// Detache de ctx (le vol sert tout le monde) ; annule quand le dernier
		// attendant part, ou apres DelaiMax.
		vctx, annuler := context.WithTimeout(context.WithoutCancel(ctx), src.DelaiMax)
		v = &vol{fait: make(chan struct{}), annuler: annuler, force: force}
		s.vols[id] = v
		go s.mener(vctx, id, src, v)
	}
	v.attendants++
	return v
}

func (s *Service) quitter(v *vol) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v.attendants--
	if v.attendants == 0 {
		v.abandonne = true
		v.annuler()
	}
}

func (s *Service) mener(ctx context.Context, id string, src Source, v *vol) {
	defer v.annuler()
	perime := v.force
	var err error
	if !perime {
		var derniere time.Time
		derniere, err = s.stock.DerniereSynchroEPG(ctx, id)
		if err != nil {
			err = fmt.Errorf("lecture de la derniere synchro EPG : %w", err)
		}
		perime = derniere.IsZero() || s.horloge.Maintenant().Sub(derniere) >= src.AgeMax
	}
	telecharge := false
	if err == nil && perime {
		telecharge = true
		debut := s.horloge.Maintenant()
		err = s.ingerer(ctx, id, src)
		resultat := "ok"
		if err != nil {
			resultat = "echec"
		}
		s.opt.Instrumentation.Compter("epg_total", 1, "fournisseur", id, "resultat", resultat)
		s.opt.Instrumentation.Observer("epg_duree_secondes", s.horloge.Maintenant().Sub(debut).Seconds(), "fournisseur", id)
	}
	s.mu.Lock()
	v.err, v.telecharge = err, telecharge
	if s.vols[id] == v {
		delete(s.vols, id)
	}
	s.mu.Unlock()
	close(v.fait)
}

// ingerer : liste les chaines, telecharge le guide en flux, rapproche, ecrit.
// Le stockage n'est touche qu'a la toute fin : tout echec avant laisse l'ancien EPG.
func (s *Service) ingerer(ctx context.Context, id string, src Source) error {
	debut := s.horloge.Maintenant()
	chaines, err := s.toutesLesChaines(ctx, id)
	if err != nil {
		return err
	}
	if len(chaines) == 0 {
		return ErrSansChaines
	}
	// L'URL contient les identifiants du fournisseur : jamais journalisee, jamais
	// dans une erreur.
	url := src.Fournisseur.(domain.SourceEPG).URLEPG()
	if url == "" {
		return ErrSansGuide
	}
	rep, err := src.Fournisseur.Requete(ctx, url, http.Header{"Accept": {"application/xml, text/xml, application/gzip, */*"}})
	if err != nil {
		s.journal.Warn("telechargement de l'EPG echoue, ancien EPG conserve", "fournisseur", id, "erreur", err)
		return fmt.Errorf("telechargement de l'EPG : %w", err)
	}
	defer rep.Body.Close()

	col := nouveauCollecteur(chaines)
	maintenant := s.horloge.Maintenant()
	st, err := Analyser(ctx, rep.Body, ParamsAnalyse{
		De: maintenant.Add(-s.opt.Passe), A: maintenant.Add(s.opt.Futur),
		Lieu: s.opt.Lieu, TailleMax: s.opt.TailleMax,
	}, col.canal, col.programme)
	if err != nil {
		s.journal.Warn("analyse de l'EPG echouee, ancien EPG conserve", "fournisseur", id, "erreur", err)
		return err
	}
	inst := col.instantane(id, s.horloge.Maintenant(), chaines)
	if err := s.stock.EcrireEPG(ctx, inst); err != nil {
		s.journal.Error("ecriture de l'EPG echouee, ancien EPG conserve", "fournisseur", id, "erreur", err)
		return fmt.Errorf("ecriture de l'EPG : %w", err)
	}
	var parID, parNom, ambigus int
	for _, r := range inst.Rapprochements {
		switch {
		case r.Ambigu:
			ambigus++
		case r.Methode == domain.RapprocheParEpgID:
			parID++
		default:
			parNom++
		}
	}
	s.journal.Info("EPG synchronise", "fournisseur", id, "duree", s.horloge.Maintenant().Sub(debut),
		"canaux", st.Canaux, "programmes_lus", st.Programmes, "hors_fenetre", st.HorsFenetre, "invalides", st.Invalides,
		"programmes_gardes", len(inst.Programmes), "chaines", len(chaines),
		"rapprochees_epg_id", parID, "rapprochees_nom", parNom, "ambigues", ambigus)
	return nil
}

func (s *Service) toutesLesChaines(ctx context.Context, id string) ([]domain.Chaine, error) {
	const page = 1000
	var tout []domain.Chaine
	for {
		l, err := s.chaines.Chaines(ctx, domain.FiltreChaines{FournisseurID: id, Limite: page, Decalage: len(tout)})
		if err != nil {
			return nil, fmt.Errorf("lecture des chaines : %w", err)
		}
		tout = append(tout, l...)
		if len(l) < page {
			return tout, nil
		}
	}
}

// collecteur retient pendant l'analyse les canaux et les seuls programmes utiles.
type collecteur struct {
	idsChaines  map[string]bool // epg_id des chaines
	idsBas      map[string]bool // idem en minuscules
	nomsChaines map[string]bool // noms normalises des chaines
	canaux      []Canal
	utile       map[string]bool // canaux DECLARES : correspondent-ils a une chaine ?
	progs       []domain.Programme
}

func nouveauCollecteur(chaines []domain.Chaine) *collecteur {
	c := &collecteur{idsChaines: map[string]bool{}, idsBas: map[string]bool{}, nomsChaines: map[string]bool{}, utile: map[string]bool{}}
	for _, ch := range chaines {
		if ch.EpgID != "" {
			c.idsChaines[ch.EpgID] = true
			c.idsBas[strings.ToLower(ch.EpgID)] = true
		}
		if k := Normaliser(ch.Nom); k != "" {
			c.nomsChaines[k] = true
		}
	}
	return c
}

func (c *collecteur) canal(cn Canal) {
	c.canaux = append(c.canaux, cn)
	u := c.idsChaines[cn.ID] || c.idsBas[strings.ToLower(cn.ID)]
	for _, n := range cn.Noms {
		u = u || c.nomsChaines[Normaliser(n)]
	}
	c.utile[cn.ID] = c.utile[cn.ID] || u
}

// programme : un canal DECLARE sans chaine correspondante ne servira jamais, ses
// programmes sont ecartes tout de suite (c'est l'essentiel de la memoire d'un guide
// de 20 000 canaux). Un canal pas encore declare reste en attente du tri final.
func (c *collecteur) programme(p domain.Programme) {
	if u, declare := c.utile[p.EpgID]; declare && !u {
		return
	}
	c.progs = append(c.progs, p)
}

// instantane rapproche les chaines, ne garde que les programmes des canaux
// retenus, retire les doublons et deduit les fins manquantes.
func (c *collecteur) instantane(fournisseur string, pris time.Time, chaines []domain.Chaine) *domain.InstantaneEPG {
	// Un guide peut contenir des programmes pour un canal jamais declare : il compte
	// comme un canal (sans nom) pour le rapprochement par epg_id.
	declares := make(map[string]bool, len(c.canaux))
	for _, cn := range c.canaux {
		declares[cn.ID] = true
	}
	canaux := c.canaux
	for _, p := range c.progs {
		if !declares[p.EpgID] {
			declares[p.EpgID] = true
			canaux = append(canaux, Canal{ID: p.EpgID})
		}
	}
	rapp := Rapprocher(chaines, canaux)
	retenus := map[string]bool{}
	for _, r := range rapp {
		if !r.Ambigu {
			retenus[r.EpgID] = true
		}
	}
	progs := c.progs[:0]
	for _, p := range c.progs {
		if retenus[p.EpgID] {
			p.FournisseurID = fournisseur
			progs = append(progs, p)
		}
	}
	sort.SliceStable(progs, func(i, j int) bool {
		if progs[i].EpgID != progs[j].EpgID {
			return progs[i].EpgID < progs[j].EpgID
		}
		return progs[i].Debut.Before(progs[j].Debut)
	})
	out := progs[:0]
	for _, p := range progs {
		if n := len(out); n > 0 && out[n-1].EpgID == p.EpgID && out[n-1].Debut.Equal(p.Debut) {
			// Doublon (meme canal, meme debut) : on garde celui qui a une fin
			// connue plutot que le premier venu.
			if out[n-1].Fin.IsZero() && !p.Fin.IsZero() {
				out[n-1] = p
			}
			continue
		}
		out = append(out, p)
	}
	// Fin absente = debut du programme suivant du meme canal ; le dernier, sans
	// suite connue, est ecarte (on ne sait pas quand il finit).
	fini := out[:0]
	for i, p := range out {
		if p.Fin.IsZero() {
			if i+1 < len(out) && out[i+1].EpgID == p.EpgID {
				p.Fin = out[i+1].Debut
			} else {
				continue
			}
		}
		fini = append(fini, p)
	}
	return &domain.InstantaneEPG{FournisseurID: fournisseur, Pris: pris, Programmes: fini, Rapprochements: rapp}
}

// attenteCatalogue : delai avant de reessayer quand le catalogue n'est pas encore en base.
const attenteCatalogue = 30 * time.Second

// Planifier rafraichit chaque fournisseur actif des que son EPG depasse AgeMax
// (rafraichissement.epg), y compris au demarrage s'il est deja perime, et purge
// les programmes termines. Ne rend la main qu'a l'annulation de ctx, apres l'arret
// de toutes ses goroutines.
func (s *Service) Planifier(ctx context.Context) {
	var wg sync.WaitGroup
	for _, id := range s.ordre {
		src := s.sources[id]
		if !src.Actif {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.suivre(ctx, id, src)
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.purger(ctx)
	}()
	wg.Wait()
}

func (s *Service) suivre(ctx context.Context, id string, src Source) {
	for ctx.Err() == nil {
		// SiPerime : si deux planificateurs ou un appel manuel viennent de rafraichir,
		// on ne retelecharge pas.
		_, err := s.SiPerime(ctx, id)
		attente := src.Reprise
		if err == nil {
			derniere, e := s.stock.DerniereSynchroEPG(ctx, id)
			if e != nil {
				attente = src.Reprise
			} else {
				attente = derniere.Add(src.AgeMax).Sub(s.horloge.Maintenant())
				if attente <= 0 {
					attente = src.Reprise // AgeMax tres court : evite une boucle serree
				}
			}
		} else if errors.Is(err, ErrSansChaines) {
			// Premier demarrage : le catalogue arrive en quelques dizaines de secondes, pas
			// en Reprise (30 min par defaut). On reessaie vite plutot que de laisser un guide vide.
			attente = min(src.Reprise, attenteCatalogue)
			s.journal.Info("EPG en attente du catalogue", "fournisseur", id, "reprise_dans", attente)
		} else if ctx.Err() == nil {
			s.journal.Warn("rafraichissement planifie de l'EPG echoue, nouvel essai", "fournisseur", id, "reprise_dans", src.Reprise)
		}
		if !s.attendre(ctx, attente) {
			return
		}
	}
}

func (s *Service) purger(ctx context.Context) {
	p, ok := s.stock.(Purgeur)
	if !ok {
		return
	}
	for {
		n, err := p.Purger(ctx, s.horloge.Maintenant().Add(-s.opt.Passe))
		if err != nil && ctx.Err() == nil {
			s.journal.Warn("purge de l'EPG echouee", "erreur", err)
		} else if n > 0 {
			s.journal.Info("programmes anciens purges", "nombre", n)
		}
		if !s.attendre(ctx, s.opt.IntervallePurge) {
			return
		}
	}
}

func (s *Service) attendre(ctx context.Context, d time.Duration) bool {
	if d < 0 {
		d = 0
	}
	m := s.horloge.Minuteur(d)
	defer m.Arreter()
	select {
	case <-m.C():
		return true
	case <-ctx.Done():
		return false
	}
}
