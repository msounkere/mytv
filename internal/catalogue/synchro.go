// Package catalogue synchronise le catalogue des fournisseurs vers le stockage.
//
// Le paquet ne depend que des contrats domain.Fournisseur et domain.Stockage : il
// ne connait ni Xtream ni SQLite, et ne lit pas la configuration (l'assemblage
// traduit config.Fournisseur en Source).
//
// Principes :
//   - un telechargement de catalogue est long (plusieurs secondes) et lourd pour le
//     fournisseur : pour un meme fournisseur, les demandes simultanees partagent UN
//     seul telechargement ;
//   - on n'ecrit qu'un catalogue COMPLET : si le telechargement echoue, le stockage
//     n'est pas touche et l'ancien catalogue continue d'etre servi ;
//   - un fournisseur en panne n'en retarde ni n'en bloque un autre.
package catalogue

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"mytv/internal/domain"
)

var (
	// ErrInconnu : aucun fournisseur de cet identifiant n'est enregistre.
	ErrInconnu = errors.New("catalogue : fournisseur inconnu")
	// ErrInactif : un fournisseur desactive n'est jamais synchronise, meme sur demande explicite.
	ErrInactif = errors.New("catalogue : fournisseur inactif")
)

// Source associe un fournisseur a ses reglages de rafraichissement.
type Source struct {
	Fournisseur domain.Fournisseur
	Actif       bool
	// AgeMax : age au-dela duquel le catalogue est « perime » (rafraichissement.catalogue).
	AgeMax time.Duration
	// Heure : « 05:00 », synchronisation quotidienne a l'heure locale ; vide = aucune.
	Heure string
	// AuDemarrage : synchroniser au lancement de Planifier si le catalogue est perime.
	AuDemarrage bool
	// DelaiMax borne un telechargement (rafraichissement.delai_max). Le vol est
	// detache du contexte de ses demandeurs : sans borne, un fournisseur qui ne
	// repond plus le garderait ouvert indefiniment. Doit etre > 0.
	DelaiMax time.Duration
	// Reprise : pause avant de retenter un echec de synchronisation planifiee, tant
	// que l'echeance suivante n'est pas arrivee (rafraichissement.reprise). Doit etre > 0.
	Reprise time.Duration
}

// Options regle le Synchroniseur ; tous les champs sont facultatifs.
type Options struct {
	Horloge Horloge
	Journal *slog.Logger
	// Lieu : fuseau de Source.Heure (defaut : heure locale du serveur).
	Lieu *time.Location
}

// Synchroniseur telecharge les catalogues et les ecrit dans le stockage.
type Synchroniseur struct {
	stockage domain.Stockage
	sources  map[string]Source
	ordre    []string // ids tries : parcours deterministe
	horloge  Horloge
	journal  *slog.Logger
	lieu     *time.Location

	mu   sync.Mutex
	vols map[string]*vol
}

// Nouveau cree un Synchroniseur. Deux sources de meme identifiant sont une erreur.
func Nouveau(st domain.Stockage, sources []Source, opt Options) (*Synchroniseur, error) {
	s := &Synchroniseur{
		stockage: st, sources: map[string]Source{}, vols: map[string]*vol{},
		horloge: opt.Horloge, journal: opt.Journal, lieu: opt.Lieu,
	}
	if s.horloge == nil {
		s.horloge = HorlogeReelle{}
	}
	if s.journal == nil {
		s.journal = slog.New(slog.DiscardHandler)
	}
	if s.lieu == nil {
		s.lieu = time.Local
	}
	for _, src := range sources {
		if src.Fournisseur == nil {
			return nil, errors.New("catalogue : source sans fournisseur")
		}
		id := src.Fournisseur.ID()
		if _, dup := s.sources[id]; dup {
			return nil, fmt.Errorf("catalogue : fournisseur %q en double", id)
		}
		// Ces durees sont des reglages (config) : pas de valeur par defaut cachee ici.
		if src.DelaiMax <= 0 || src.Reprise <= 0 {
			return nil, fmt.Errorf("catalogue : fournisseur %q : DelaiMax et Reprise doivent etre > 0", id)
		}
		if src.Heure != "" {
			if _, err := time.Parse("15:04", src.Heure); err != nil {
				return nil, fmt.Errorf("catalogue : fournisseur %q : heure %q invalide (HH:MM)", id, src.Heure)
			}
		}
		s.sources[id] = src
		s.ordre = append(s.ordre, id)
	}
	sort.Strings(s.ordre)
	return s, nil
}

// vol est un telechargement en cours, partage par tous ceux qui l'ont demande.
type vol struct {
	fait    chan struct{} // ferme quand err et telecharge sont definitifs
	annuler context.CancelFunc
	// Champs proteges par Synchroniseur.mu :
	force      bool // un demandeur exige un vrai telechargement
	decide     bool // le meneur a tranche : telecharger ou non
	telecharge bool
	attendants int
	abandonne  bool // le dernier attendant est parti : le vol est annule, ne plus s'y joindre
	// Ecrits par le meneur avant de fermer « fait » :
	err error
}

// Synchroniser telecharge et ecrit le catalogue du fournisseur, quel que soit son age.
// Si un telechargement est deja en cours pour ce fournisseur, il est partage.
//
// L'erreur renvoyee enveloppe celle de Fournisseur.Catalogue, journalisee et
// remontee telle quelle : ce paquet suppose (contrat de domain.Fournisseur) qu'elle
// ne contient aucun identifiant.
func (s *Synchroniseur) Synchroniser(ctx context.Context, id string) error {
	_, err := s.executer(ctx, id, true)
	return err
}

// SiPerime ne telecharge que si le catalogue est plus vieux que AgeMax (ou jamais
// synchronise). Renvoie vrai si un telechargement a eu lieu.
func (s *Synchroniseur) SiPerime(ctx context.Context, id string) (bool, error) {
	return s.executer(ctx, id, false)
}

// SynchroniserTous synchronise les fournisseurs actifs en parallele (ils sont
// independants) et renvoie les erreurs regroupees : un echec n'interrompt pas les autres.
func (s *Synchroniseur) SynchroniserTous(ctx context.Context) error {
	return s.pourTous(func(id string) error { return s.Synchroniser(ctx, id) })
}

func (s *Synchroniseur) pourTous(f func(id string) error) error {
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
			if err := f(id); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("fournisseur %s : %w", id, err))
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

func (s *Synchroniseur) executer(ctx context.Context, id string, force bool) (bool, error) {
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
		v, retenter := s.rejoindre(ctx, id, src, force)
		select {
		case <-v.fait:
		case <-ctx.Done():
			s.quitter(v)
			return false, ctx.Err()
		}
		if retenter {
			// Le vol auquel on s'est joint a juge le catalogue frais alors qu'on exigeait
			// un telechargement : on en lance un vrai.
			if !v.telecharge && v.err == nil {
				continue
			}
		}
		if v.err != nil {
			return false, v.err // un echec n'est pas un telechargement reussi
		}
		return v.telecharge, nil
	}
}

// rejoindre s'inscrit sur le vol en cours du fournisseur, ou en demarre un.
// Le second resultat est vrai si l'appelant doit verifier que le vol a bien telecharge.
func (s *Synchroniseur) rejoindre(ctx context.Context, id string, src Source, force bool) (*vol, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.vols[id]
	retenter := false
	if v == nil || v.abandonne {
		// Detache de ctx : le vol sert aussi les autres demandeurs. Il est annule
		// quand le dernier attendant part, ou apres DelaiMax.
		vctx, annuler := context.WithTimeout(context.WithoutCancel(ctx), src.DelaiMax)
		v = &vol{fait: make(chan struct{}), annuler: annuler, force: force}
		s.vols[id] = v
		go s.mener(vctx, id, src, v)
	} else if force {
		if !v.decide {
			v.force = true // pas encore tranche : il telechargera
		} else if !v.telecharge {
			retenter = true // a decide de ne rien faire : on reviendra apres
		}
	}
	v.attendants++
	return v, retenter
}

func (s *Synchroniseur) quitter(v *vol) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v.attendants--
	if v.attendants == 0 {
		v.abandonne = true
		v.annuler() // plus personne n'attend le resultat
	}
}

// mener execute le vol : decision (frais ou perime), telechargement, ecriture.
func (s *Synchroniseur) mener(ctx context.Context, id string, src Source, v *vol) {
	defer v.annuler()
	telecharge, err := s.decider(ctx, src, v)
	if err == nil && telecharge {
		err = s.telecharger(ctx, id, src)
	}
	s.mu.Lock()
	v.err = err
	if s.vols[id] == v { // un vol abandonne a pu etre remplace entre-temps
		delete(s.vols, id)
	}
	s.mu.Unlock()
	close(v.fait)
}

func (s *Synchroniseur) decider(ctx context.Context, src Source, v *vol) (bool, error) {
	s.mu.Lock()
	force := v.force
	s.mu.Unlock()
	perime := true
	if !force {
		derniere, err := s.stockage.DerniereSynchro(ctx, src.Fournisseur.ID())
		if err != nil {
			return false, fmt.Errorf("lecture de la derniere synchro : %w", err)
		}
		// Jamais synchronise (zero) = perime. AgeMax 0 : toujours perime.
		perime = derniere.IsZero() || s.horloge.Maintenant().Sub(derniere) >= src.AgeMax
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if v.force { // un demandeur forcant est arrive pendant la verification
		perime = true
	}
	v.decide, v.telecharge = true, perime
	return perime, nil
}

func (s *Synchroniseur) telecharger(ctx context.Context, id string, src Source) error {
	debut := s.horloge.Maintenant()
	inst, err := src.Fournisseur.Catalogue(ctx)
	if err != nil {
		// Le stockage n'est pas touche : l'ancien catalogue reste servi.
		s.journal.Warn("synchronisation du catalogue echouee, ancien catalogue conserve",
			"fournisseur", id, "duree", s.horloge.Maintenant().Sub(debut), "erreur", err)
		return fmt.Errorf("telechargement du catalogue : %w", err)
	}
	if inst == nil || inst.FournisseurID != id {
		return errors.New("le fournisseur a renvoye un catalogue qui n'est pas le sien")
	}
	// Copie : l'instantane appartient au fournisseur. L'heure de reference de la
	// fraicheur est celle de NOTRE horloge (celle que SiPerime compare).
	ecrit := *inst
	ecrit.Pris = s.horloge.Maintenant()
	if err := s.stockage.EcrireCatalogue(ctx, &ecrit); err != nil {
		s.journal.Error("ecriture du catalogue echouee, ancien catalogue conserve",
			"fournisseur", id, "erreur", err)
		return fmt.Errorf("ecriture du catalogue : %w", err)
	}
	s.journal.Info("catalogue synchronise",
		"fournisseur", id, "duree", s.horloge.Maintenant().Sub(debut),
		"categories", len(ecrit.Categories), "chaines", len(ecrit.Chaines),
		"films", len(ecrit.Films), "series", len(ecrit.Series))
	return nil
}

// Planifier lance la synchronisation de demarrage puis les synchronisations
// quotidiennes, et ne rend la main qu'a l'annulation de ctx (apres l'arret de
// toutes ses goroutines). Un fournisseur sans Heure ni AuDemarrage n'est pas suivi.
func (s *Synchroniseur) Planifier(ctx context.Context) {
	var wg sync.WaitGroup
	for _, id := range s.ordre {
		src := s.sources[id]
		if !src.Actif || (src.Heure == "" && !src.AuDemarrage) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.suivre(ctx, id, src)
		}()
	}
	wg.Wait()
}

func (s *Synchroniseur) suivre(ctx context.Context, id string, src Source) {
	if src.AuDemarrage {
		// Seulement si perime : un redemarrage en boucle ne doit pas marteler le fournisseur.
		if _, err := s.SiPerime(ctx, id); err != nil && ctx.Err() == nil {
			s.journal.Warn("synchronisation au demarrage echouee", "fournisseur", id, "erreur", err)
		}
	}
	if src.Heure == "" {
		return
	}
	for {
		echeance := s.prochaine(src.Heure)
		if !s.attendre(ctx, echeance.Sub(s.horloge.Maintenant())) {
			return
		}
		// Forcee : a 05:00 le catalogue de la veille a 24 h moins quelques secondes,
		// il ne serait pas « perime » et la synchro du jour sauterait.
		err := s.Synchroniser(ctx, id)
		for err != nil && ctx.Err() == nil {
			suivante := s.prochaine(src.Heure)
			if s.horloge.Maintenant().Add(src.Reprise).After(suivante) {
				break // l'echeance suivante arrive avant la reprise
			}
			s.journal.Warn("synchronisation planifiee echouee, nouvel essai", "fournisseur", id, "reprise_dans", src.Reprise)
			if !s.attendre(ctx, src.Reprise) {
				return
			}
			err = s.Synchroniser(ctx, id)
		}
		if ctx.Err() != nil {
			return
		}
	}
}

// prochaine donne la prochaine occurrence STRICTEMENT future de « HH:MM » dans le lieu.
func (s *Synchroniseur) prochaine(hhmm string) time.Time {
	t, _ := time.Parse("15:04", hhmm) // valide a la construction
	now := s.horloge.Maintenant().In(s.lieu)
	c := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, s.lieu)
	if !c.After(now) {
		c = time.Date(now.Year(), now.Month(), now.Day()+1, t.Hour(), t.Minute(), 0, 0, s.lieu)
	}
	return c
}

func (s *Synchroniseur) attendre(ctx context.Context, d time.Duration) bool {
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
