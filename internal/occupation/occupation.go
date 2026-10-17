// Package occupation explique POURQUOI le fournisseur refuse (407) et espace les
// reessais qui en decoulent.
//
// Le fournisseur n'accepte qu'une connexion par compte. Quand il refuse, trois causes
// donnent des conduites opposees pour l'utilisateur :
//   - un AUTRE APPAREIL utilise le compte : il faut le fermer (reessayer ne sert a rien) ;
//   - une autre fenetre de MyTV tient la connexion : il faut la fermer ;
//   - le fournisseur garde encore la place d'une lecture precedente (zapping, ~100 s) :
//     il suffit d'attendre.
//
// REGLE D'OR : le diagnostic ne doit JAMAIS aggraver la situation chez le fournisseur.
// La seule requete qu'il fait est la lecture de user_info (aucun flux), mise en cache,
// regroupee (une seule a la fois) et plafonnee (au plus une par supervision.cache_occupation
// et par fournisseur, echecs compris). Un 503 qui en decoule ne declenche aucun autre appel.
package occupation

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"mytv/internal/config"
	"mytv/internal/domain"
)

// Classe est le code stable renvoye a l'interface (corps JSON et X-Mytv-Erreur).
type Classe string

const (
	// Ailleurs : le fournisseur annonce toutes ses places prises et mytv n'en tient aucune.
	Ailleurs Classe = "compte_utilise_ailleurs"
	// ParMytv : une autre session de mytv tient la connexion.
	ParMytv Classe = "compte_utilise_par_mytv"
	// Occupe : indetermine (user_info indisponible, places libres cote fournisseur, ou
	// place gardee par la lecture precedente de mytv). Le reessai automatique est la bonne reponse.
	Occupe Classe = "compte_occupe"
)

// Info est ce que le diagnostic retient de user_info.
type Info struct {
	ConnexionsMax     int
	ConnexionsActives int
}

// Source decrit ce que l'on sait d'un fournisseur.
type Source struct {
	// Info lit user_info. nil : jamais d'appel, la classification retombe sur Occupe.
	Info func(ctx context.Context) (Info, error)
	// AutresBaux : nombre de baux mytv tenus par d'autres sessions que celle-ci.
	AutresBaux func(session string) int
	// Occupes : nombre de baux mytv actifs (toutes sessions), pour l'etat « mytv ».
	Occupes func() int
}

// Reglages : voir bail.* et supervision.cache_occupation.
type Reglages struct {
	CacheOccupation  time.Duration
	DelaiInfo        time.Duration
	RetryApres       time.Duration
	RetryApresMax    time.Duration
	GardeFournisseur time.Duration
	SuccesPrise      int
	FenetrePrise     time.Duration
}

// ReglagesDepuisConfig reprend les reglages de la configuration.
func ReglagesDepuisConfig(c *config.Config) Reglages {
	return Reglages{
		CacheOccupation: c.Supervision.CacheOccupation.Std(), DelaiInfo: c.Supervision.DelaiSante.Std(),
		RetryApres: c.Bail.RetryApres.Std(), RetryApresMax: c.Bail.RetryApresMax.Std(),
		GardeFournisseur: c.Bail.GardeFournisseur.Std(), SuccesPrise: c.Bail.SuccesPrise, FenetrePrise: c.Bail.FenetrePrise.Std(),
	}
}

// Options de Nouveau.
type Options struct {
	Reglages
	Sources         map[string]Source
	Horloge         func() time.Time
	Journal         *slog.Logger
	Instrumentation domain.Instrumentation
}

// Demande identifie ce qui subit le refus.
type Demande struct {
	Fournisseur string
	Session     string
	Ressource   string // chaine du direct ou film : la cle de Retry-After avec la session
	// Direct : lecture continue d'une chaine. La MEME chaine ne peut alors pas etre la
	// cause d'un refus (c'est la meme connexion), alors que pour un film un seek rouvre
	// une connexion que le fournisseur garde encore.
	Direct bool
}

// oubliAilleurs : sans nouvelle preuve pendant ce delai (plus personne ne essaie), l'etat
// « ailleurs » n'est plus affirme : on ne sait plus rien de l'autre appareil.
const oubliAilleurs = 5 * time.Minute

// peremption : une serie (session, ressource) inactive plus longtemps est oubliee.
const peremption = 15 * time.Minute

type cle struct{ fournisseur, session, ressource string }

type serie struct {
	echecs        int // 503 consecutifs ; remis a zero par un succes
	succes        int // succes consecutifs depuis le dernier refus
	dernierSucces time.Time
	vu            time.Time
}

type succesAmont struct {
	quand              time.Time
	session, ressource string
}

type appel struct {
	fini chan struct{}
	info Info
	err  error
}

type etatFournisseur struct {
	dernier       succesAmont
	ailleursDe    time.Time // debut de l'episode « autre appareil » ; zero = aucun
	ailleursVu    time.Time // derniere preuve (refus, diagnostic) que l'autre appareil est la
	dernierePrise time.Time
	publie        string
	depuis        time.Time
	// Cache de user_info.
	info  Info
	err   error
	lueLe time.Time
	enVol *appel
}

// Suivi est le diagnostic et la memoire des refus. Il est sur pour un usage concurrent.
type Suivi struct {
	r       Reglages
	srcs    map[string]Source
	horloge func() time.Time
	log     *slog.Logger
	inst    domain.Instrumentation

	base   context.Context
	arret  context.CancelFunc
	taches sync.WaitGroup

	mu      sync.Mutex
	series  map[cle]*serie
	fourn   map[string]*etatFournisseur
	purgeLe time.Time
}

// Nouveau cree le suivi. Fermer arrete les lectures de user_info en cours.
func Nouveau(o Options) *Suivi {
	if o.Horloge == nil {
		o.Horloge = time.Now
	}
	if o.Journal == nil {
		o.Journal = slog.New(slog.DiscardHandler)
	}
	if o.Instrumentation == nil {
		o.Instrumentation = domain.SansInstrumentation{}
	}
	if o.CacheOccupation <= 0 {
		o.CacheOccupation = 15 * time.Second
	}
	if o.DelaiInfo <= 0 {
		o.DelaiInfo = 8 * time.Second
	}
	if o.RetryApres <= 0 {
		o.RetryApres = 15 * time.Second
	}
	if o.RetryApresMax < o.RetryApres {
		o.RetryApresMax = o.RetryApres
	}
	if o.SuccesPrise < 1 {
		o.SuccesPrise = 2
	}
	base, arret := context.WithCancel(context.Background())
	return &Suivi{r: o.Reglages, srcs: o.Sources, horloge: o.Horloge, log: o.Journal, inst: o.Instrumentation,
		base: base, arret: arret, series: map[cle]*serie{}, fourn: map[string]*etatFournisseur{}}
}

// Fermer annule les lectures de user_info en cours et les attend.
func (s *Suivi) Fermer() {
	if s == nil {
		return
	}
	s.arret()
	s.taches.Wait()
}

func (s *Suivi) etat(f string) *etatFournisseur {
	e := s.fourn[f]
	if e == nil {
		e = &etatFournisseur{}
		s.fourn[f] = e
	}
	return e
}

// purger oublie les series inactives (memoire bornee). Verrou tenu.
func (s *Suivi) purger(now time.Time) {
	if now.Sub(s.purgeLe) < time.Minute {
		return
	}
	s.purgeLe = now
	for k, v := range s.series {
		if now.Sub(v.vu) > peremption {
			delete(s.series, k)
		}
	}
}

// Succes note qu'une requete a reussi chez le fournisseur : la serie des refus repart de
// zero (Retry-After redevient 15 s) et un episode « autre appareil » est clos, la place
// etant de nouveau a mytv. Cout : un verrou et une table, aucun appel reseau.
func (s *Suivi) Succes(d Demande) {
	if s == nil {
		return
	}
	now := s.horloge()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purger(now)
	k := cle{d.Fournisseur, d.Session, d.Ressource}
	v := s.series[k]
	if v == nil {
		v = &serie{}
		s.series[k] = v
	}
	if now.Sub(v.dernierSucces) > s.r.FenetrePrise {
		v.succes = 0 // pas consecutifs : une pause plus longue que la fenetre
	}
	v.succes++
	v.echecs = 0
	v.dernierSucces, v.vu = now, now
	e := s.etat(d.Fournisseur)
	e.dernier = succesAmont{quand: now, session: d.Session, ressource: d.Ressource}
	if !e.ailleursDe.IsZero() {
		s.log.Info("place retrouvee chez le fournisseur", "fournisseur", d.Fournisseur)
		e.ailleursDe = time.Time{}
	}
}

// Refus note qu'une requete a ete refusee par le fournisseur (407, ou 401/403 sur un
// segment). Si la meme chaine venait de reussir plusieurs fois de suite, c'est un autre
// appareil qui a pris la place en pleine lecture : journal (une fois par episode),
// metrique, etat expose a l'administration. Vrai quand un nouvel episode commence.
//
// Ne declenche aucun appel reseau.
func (s *Suivi) Refus(d Demande, cause string) bool {
	if s == nil {
		return false
	}
	now := s.horloge()
	s.mu.Lock()
	defer s.mu.Unlock()
	v := s.series[cle{d.Fournisseur, d.Session, d.Ressource}]
	if v == nil || v.succes < s.r.SuccesPrise || now.Sub(v.dernierSucces) > s.r.FenetrePrise {
		return false
	}
	v.succes = 0 // un nouvel episode exige une nouvelle serie de succes
	e := s.etat(d.Fournisseur)
	if !e.ailleursDe.IsZero() {
		return false
	}
	e.ailleursDe, e.ailleursVu, e.dernierePrise = now, now, now
	// Ni session ni URL dans le journal : la chaine et le fournisseur suffisent.
	s.log.Warn("connexion reprise par un autre appareil", "fournisseur", d.Fournisseur, "chaine", d.Ressource, "cause", cause)
	s.inst.Compter("live_prises_de_place_total", 1, "fournisseur", d.Fournisseur)
	s.inst.Evenement("occupation:prise", "connexion reprise par un autre appareil")
	return true
}

// RetryApres compte un refus de plus pour (session, chaine) et rend le Retry-After a
// annoncer : RetryApres, le double, le double..., jusqu'au plafond.
func (s *Suivi) RetryApres(d Demande) time.Duration {
	if s == nil {
		return 15 * time.Second
	}
	now := s.horloge()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.purger(now)
	k := cle{d.Fournisseur, d.Session, d.Ressource}
	v := s.series[k]
	if v == nil {
		v = &serie{}
		s.series[k] = v
	}
	v.vu = now
	delai := s.r.RetryApres
	for i := 0; i < v.echecs && delai < s.r.RetryApresMax; i++ {
		delai *= 2
	}
	v.echecs++
	return min(delai, s.r.RetryApresMax)
}

// Etat est ce que GET /admin/fournisseurs expose.
type Etat struct {
	Occupation    string // libre | mytv | ailleurs
	Depuis        *time.Time
	DernierePrise *time.Time
}

// Etat donne l'occupation courante d'un fournisseur.
func (s *Suivi) Etat(fournisseur string) Etat {
	if s == nil {
		return Etat{Occupation: "libre"}
	}
	now := s.horloge()
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.etat(fournisseur)
	if !e.ailleursDe.IsZero() && now.Sub(e.ailleursVu) > oubliAilleurs {
		e.ailleursDe = time.Time{}
	}
	courant := "libre"
	switch {
	case !e.ailleursDe.IsZero():
		courant = "ailleurs"
	case s.srcs[fournisseur].Occupes != nil && s.srcs[fournisseur].Occupes() > 0:
		courant = "mytv"
	}
	if courant != e.publie {
		debut := now
		if courant == "ailleurs" {
			debut = e.ailleursDe
		}
		if e.publie == "" && courant == "libre" {
			debut = time.Time{} // libre depuis toujours
		}
		e.publie, e.depuis = courant, debut
	}
	r := Etat{Occupation: courant}
	if !e.depuis.IsZero() {
		t := e.depuis.UTC()
		r.Depuis = &t
	}
	if !e.dernierePrise.IsZero() {
		t := e.dernierePrise.UTC()
		r.DernierePrise = &t
	}
	return r
}
