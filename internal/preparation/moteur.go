// Package preparation tient prets, sur le disque du serveur, les films et les prochains
// episodes de la liste « A voir » de chaque profil (US-42).
//
// POURQUOI : le fournisseur sert tres vite ce que son noeud a deja lu et lentement
// (1,5 a 3 Mbit/s) ce qu'il n'a jamais servi, et il n'accorde qu'UNE connexion durable par
// compte. Aucun reglage du lecteur ne compense : on telecharge d'avance, quand personne ne
// regarde, ce que l'utilisateur a decide de regarder.
//
// Regles qui structurent le paquet :
//   - le SPECTATEUR a toujours la priorite : aucune connexion n'est ouverte tant qu'une
//     lecture (direct ou film) tient une place chez le fournisseur, et le telechargement en
//     cours est coupe (fichier `.partiel` conserve) des qu'une lecture demande une place ;
//     il reprend `reprise_apres` apres la fin de la lecture ;
//   - une seule connexion de preparation a la fois, tous fournisseurs confondus ;
//   - l'URL amont est construite par le fournisseur a partir d'identifiants du catalogue ;
//     ni identifiants ni URL dans les journaux ni dans les noms de fichiers ;
//   - le budget disque est une borne dure : jamais depasse, jamais un fichier en cours de
//     lecture efface.
package preparation

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"mytv/internal/domain"
)

// Occupation dit combien de places (baux) sont tenues chez un fournisseur : zero = aucune
// lecture en cours. Implemente par baux.Gestionnaire (Occupes).
type Occupation interface {
	Occupes(fournisseurID string) int
}

// Reglages du moteur (config.Preparation).
type Reglages struct {
	Dossier        string
	Budget         int64         // octets, > 0
	EpisodesAvance int           // defaut par serie, >= 1
	RepriseApres   time.Duration // pause apres la fin d'une lecture
	// Intervalle entre deux planifications spontanees (defaut 30 s). Un ajout a la liste
	// ou une lecture enregistree reveillent le moteur sans attendre (Reveiller).
	Intervalle time.Duration
	// Tampon de copie (defaut 256 Kio).
	Tampon int
	// MemoireSaisons : duree pendant laquelle la liste des episodes d'une serie est
	// reutilisee sans nouvel appel au fournisseur (defaut 10 min).
	MemoireSaisons time.Duration
}

// Dependances : tout est une interface, rien n'est contacte a la construction.
type Dependances struct {
	Fournisseurs map[string]domain.Fournisseur // chacun doit etre un domain.FournisseurVOD
	Stockage     domain.StockageVideo
	Liste        domain.ListeAVoir
	Etat         domain.EtatProfil // historique (vu = part >= domain.PartVue)
	// Saisons liste les episodes d'une serie ; nil = FournisseurVOD.Saisons du fournisseur.
	// En production : metadonnees.Service.Saisons (appels bornes, memoire partagee).
	Saisons         func(ctx context.Context, s domain.SerieTV) ([]domain.Saison, error)
	Occupation      Occupation
	Horloge         func() time.Time
	Journal         *slog.Logger
	Instrumentation domain.Instrumentation
}

// Moteur planifie et telecharge. Sur pour un usage concurrent.
type Moteur struct {
	r    Reglages
	dep  Dependances
	srcs map[string]domain.FournisseurVOD
	log  *slog.Logger
	inst domain.Instrumentation
	now  func() time.Time

	mu      sync.Mutex
	index   map[domain.ContenuID]*entree
	saisons map[domain.ContenuID]memoireSaisons

	// Telechargement en cours : son fournisseur, son annulation (pause) et le signal
	// de sa fin (connexion fermee).
	courant *telechargement
	// occupes : fournisseurs ou une place est tenue (d'apres le veilleur) ; finLecture :
	// derniere fois ou la place a ete rendue.
	occupes    map[string]bool
	finLecture map[string]time.Time
	suspendu   int // Suspendre en cours (sonde de disponibilite)

	reveil chan struct{}
}

type telechargement struct {
	fid     string
	contenu domain.ContenuID
	annuler context.CancelFunc
	fini    chan struct{} // ferme quand la connexion amont est fermee
}

type memoireSaisons struct {
	saisons []domain.Saison
	jusqua  time.Time
}

// Nouveau cree le dossier, relit l'index et le rapproche des fichiers presents (un
// `.partiel` est repris, un fichier absent est oublie, un fichier inconnu de l'index est
// efface). Aucun telechargement ne part avant Lancer.
func Nouveau(dep Dependances, r Reglages) (*Moteur, error) {
	if r.Dossier == "" || r.Budget <= 0 || r.EpisodesAvance < 1 || r.RepriseApres < 0 {
		return nil, errors.New("preparation : reglages invalides (dossier, budget, episodes_avance, reprise_apres)")
	}
	if r.Intervalle <= 0 {
		r.Intervalle = 30 * time.Second
	}
	if r.Tampon <= 0 {
		r.Tampon = 256 << 10
	}
	if r.MemoireSaisons <= 0 {
		r.MemoireSaisons = 10 * time.Minute
	}
	if dep.Stockage == nil || dep.Liste == nil || dep.Etat == nil || dep.Occupation == nil {
		return nil, errors.New("preparation : dependance manquante (stockage, liste, etat, occupation)")
	}
	m := &Moteur{r: r, dep: dep, srcs: map[string]domain.FournisseurVOD{}, log: dep.Journal, inst: dep.Instrumentation, now: dep.Horloge,
		index: map[domain.ContenuID]*entree{}, saisons: map[domain.ContenuID]memoireSaisons{},
		occupes: map[string]bool{}, finLecture: map[string]time.Time{}, reveil: make(chan struct{}, 1)}
	if m.log == nil {
		m.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if m.inst == nil {
		m.inst = domain.SansInstrumentation{}
	}
	if m.now == nil {
		m.now = time.Now
	}
	for id, f := range dep.Fournisseurs {
		v, ok := f.(domain.FournisseurVOD)
		if !ok {
			return nil, fmt.Errorf("preparation : le fournisseur %q ne sert ni films ni series", id)
		}
		m.srcs[id] = v
	}
	if err := os.MkdirAll(r.Dossier, 0o700); err != nil {
		return nil, fmt.Errorf("preparation : dossier %q : %w", r.Dossier, err)
	}
	if err := m.chargerIndex(); err != nil {
		return nil, err
	}
	return m, nil
}

// Reveiller demande une planification sans attendre l'intervalle (liste modifiee,
// lecture enregistree). Jamais bloquant.
func (m *Moteur) Reveiller() {
	select {
	case m.reveil <- struct{}{}:
	default:
	}
}

// Veilleur rend la fonction a brancher sur baux.Gestionnaire.AvecVeilleur : une place
// prise met le telechargement de ce fournisseur en pause (et ATTEND que sa connexion soit
// fermee, au plus pauseMax, avant de rendre la main : la lecture qui arrive ne trouve
// pas la connexion de la preparation devant elle) ; une place rendue date la fin de
// lecture, dont `reprise_apres` est compte.
func (m *Moteur) Veilleur() func(fournisseurID string, occupes int) {
	return func(fid string, occupes int) {
		m.mu.Lock()
		if occupes > 0 {
			m.occupes[fid] = true
		} else {
			m.occupes[fid] = false
			m.finLecture[fid] = m.now()
		}
		c := m.courant
		m.mu.Unlock()
		if occupes > 0 && c != nil && c.fid == fid {
			m.mettreEnPause(c)
		}
		m.Reveiller()
	}
}

const (
	// pauseMax : attente maximale de la fermeture de la connexion amont apres une demande de pause.
	pauseMax = 3 * time.Second
	// graceFermeture : delai laisse, apres la fermeture de notre cote, pour que le fournisseur
	// constate la fin de la connexion avant qu'une autre (lecture, sonde) s'ouvre : il coupe
	// toute connexion qu'il croit supplementaire.
	graceFermeture = 150 * time.Millisecond
)

// mettreEnPause annule le telechargement et attend la fermeture de sa connexion, puis une
// courte grace. Sans effet (et sans attente) si le telechargement est deja termine.
func (m *Moteur) mettreEnPause(c *telechargement) {
	select {
	case <-c.fini:
		return
	default:
	}
	c.annuler()
	select {
	case <-c.fini:
		time.Sleep(graceFermeture)
	case <-time.After(pauseMax):
		m.log.Warn("preparation : connexion de telechargement pas fermee a temps")
	}
}

// Suspendre coupe le telechargement en cours (s'il y en a un) et empeche tout nouveau
// depart jusqu'a l'appel de la fonction rendue. Sert a la sonde de disponibilite, qui
// mesure par ses propres connexions : jamais deux connexions a la fois chez le fournisseur.
func (m *Moteur) Suspendre() (reprendre func()) {
	m.mu.Lock()
	m.suspendu++
	c := m.courant
	m.mu.Unlock()
	if c != nil {
		m.mettreEnPause(c)
	}
	var une sync.Once
	return func() {
		une.Do(func() {
			m.mu.Lock()
			m.suspendu--
			m.mu.Unlock()
			m.Reveiller()
		})
	}
}

// Lancer fait tourner le moteur jusqu'a l'annulation de ctx : a chaque reveil (ou toutes
// les `Intervalle`), il planifie puis telecharge, dans l'ordre, ce qui manque — tant que
// le spectateur ne demande pas la place.
func (m *Moteur) Lancer(ctx context.Context) {
	minuteur := time.NewTimer(0)
	defer minuteur.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-minuteur.C:
		case <-m.reveil:
			if !minuteur.Stop() {
				select {
				case <-minuteur.C:
				default:
				}
			}
		}
		attente := m.cycle(ctx)
		if attente <= 0 || attente > m.r.Intervalle {
			attente = m.r.Intervalle
		}
		minuteur.Reset(attente)
	}
}

// cycle planifie puis telecharge ce qu'il peut. Rend le delai avant le prochain cycle
// spontane (0 = l'intervalle).
func (m *Moteur) cycle(ctx context.Context) time.Duration {
	plan, err := m.planifier(ctx)
	if err != nil {
		m.log.Warn("preparation : planification", "erreur", err)
		return 0
	}
	m.appliquer(plan)
	for _, v := range plan.voulus {
		if ctx.Err() != nil {
			return 0
		}
		e := m.entreeDe(v.contenu)
		if e == nil || e.Etat == domain.PreparationPrete || (e.Etat == domain.PreparationEchec && !m.aRetenter(e)) {
			continue
		}
		if attente, ok := m.peutTelecharger(v.fid); !ok {
			return attente
		}
		switch err := m.telecharger(ctx, e, v); {
		case err == nil:
		case errors.Is(err, errPause):
			// Un spectateur est arrive, ou le fournisseur a refuse la place : on repasse
			// apres reprise_apres (le veilleur reveille des que la place est rendue).
			return m.r.RepriseApres
		case errors.Is(err, errBudget):
			// Pas de place pour celui-ci ; les suivants (plus petits ?) sont tentes.
		default:
			// Echec de ce fichier (permanent, ou transitoire : retente a `reessai`) : on
			// passe au suivant.
		}
	}
	return m.prochainReessai()
}

// aRetenter : l'echeance d'une nouvelle tentative automatique est atteinte (echec
// transitoire). Journal INFO a chaque nouvelle tentative (US-44 CA2).
func (m *Moteur) aRetenter(e *entree) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e.Reessai.IsZero() || m.now().Before(e.Reessai) {
		return false
	}
	m.log.Info("preparation : nouvelle tentative apres un echec transitoire", "contenu", string(e.Contenu), "tentative", e.Echecs+1, "cause", e.Cause)
	return true
}

// prochainReessai : delai avant la plus proche echeance de nouvelle tentative d'un fichier
// encore demande (0 si aucune : l'intervalle s'applique).
func (m *Moteur) prochainReessai() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	var d time.Duration
	for _, e := range m.index {
		if !e.demande || e.Etat != domain.PreparationEchec || e.Reessai.IsZero() {
			continue
		}
		if reste := max(e.Reessai.Sub(m.now()), time.Millisecond); d == 0 || reste < d {
			d = reste
		}
	}
	return d
}

// Reessayer remet en attente, tout de suite, les fichiers en echec d'un element de la
// liste d'un profil (serie : ses episodes planifies) et efface leur echeance ; idempotent
// (US-44 CA3). L'existence de l'element dans la liste est verifiee par l'appelant.
func (m *Moteur) Reessayer(ctx context.Context, profil string, id domain.AVoirID) error {
	pp, _, err := m.planifierProfil(ctx, profil)
	if err != nil {
		return err
	}
	n := 0
	m.mu.Lock()
	for _, el := range pp.elements {
		if el.id != id {
			continue
		}
		for _, v := range el.fichiers {
			e := m.index[v.contenu]
			if e == nil || e.Etat != domain.PreparationEchec {
				continue
			}
			e.Etat, e.Cause, e.Reessai, e.Echecs = domain.PreparationAttente, "", time.Time{}, 0
			n++
		}
	}
	if n > 0 {
		m.sauverIndexLocked()
	}
	m.mu.Unlock()
	if n > 0 {
		m.log.Info("preparation : nouvelle tentative demandee", "element", string(id), "fichiers", n)
		m.Reveiller()
	}
	return nil
}

// Budget : la borne de preparation.budget, en octets (US-44 CA4).
func (m *Moteur) Budget() int64 { return m.r.Budget }

// peutTelecharger : aucune place tenue chez ce fournisseur (veilleur ET interrogation
// directe), pas de sonde en cours, et reprise_apres ecoule depuis la derniere fin de
// lecture. Sinon rend le delai a attendre (0 = jusqu'au prochain reveil).
func (m *Moteur) peutTelecharger(fid string) (time.Duration, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.suspendu > 0 {
		return 0, false
	}
	if m.occupes[fid] || m.dep.Occupation.Occupes(fid) > 0 {
		return 0, false
	}
	if fin, ok := m.finLecture[fid]; ok {
		if reste := m.r.RepriseApres - m.now().Sub(fin); reste > 0 {
			return reste, false
		}
	}
	return 0, true
}

// Pret rend le chemin du fichier complet d'un contenu et une fonction a appeler en fin de
// lecture (tant qu'elle n'est pas appelee, le fichier n'est jamais efface). ok = faux si le
// fichier est absent ou partiel : le chemin habituel (fournisseur) s'applique.
func (m *Moteur) Pret(id domain.ContenuID) (chemin string, liberer func(), ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.index[id]
	if e == nil || e.Etat != domain.PreparationPrete {
		return "", nil, false
	}
	chemin = filepath.Join(m.r.Dossier, e.Nom)
	if st, err := os.Stat(chemin); err != nil || st.Size() != e.Total {
		// Le fichier a disparu ou change sous nos pieds : l'index l'oublie.
		delete(m.index, id)
		m.sauverIndexLocked()
		return "", nil, false
	}
	e.enLecture++
	var une sync.Once
	return chemin, func() {
		une.Do(func() {
			m.mu.Lock()
			e.enLecture--
			m.mu.Unlock()
		})
	}, true
}

// EstPret : un fichier complet existe pour ce contenu.
func (m *Moteur) EstPret(id domain.ContenuID) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := m.index[id]
	return e != nil && e.Etat == domain.PreparationPrete
}

// Fichiers liste l'etat de tous les fichiers suivis (administration), par nom, avec
// l'echeance de nouvelle tentative des echecs transitoires.
func (m *Moteur) Fichiers() []domain.FichierDetaille {
	m.mu.Lock()
	defer m.mu.Unlock()
	res := make([]domain.FichierDetaille, 0, len(m.index))
	for _, e := range m.index {
		res = append(res, e.detail())
	}
	trierFichiers(res)
	return res
}

// Vider efface tous les fichiers qui ne sont pas en cours de lecture (telechargement en
// cours coupe). Rend le nombre de fichiers effaces.
func (m *Moteur) Vider() int {
	m.mu.Lock()
	c := m.courant
	m.mu.Unlock()
	if c != nil {
		m.mettreEnPause(c)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, e := range m.index {
		if e.enLecture > 0 {
			continue
		}
		m.effacerLocked(e)
		delete(m.index, id)
		n++
	}
	m.sauverIndexLocked()
	m.log.Info("preparation : fichiers effaces a la demande", "fichiers", n)
	return n
}

// Occupe : octets sur le disque (prets et partiels).
func (m *Moteur) Occupe() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.occupeLocked(nil)
}

func (m *Moteur) entreeDe(id domain.ContenuID) *entree {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.index[id]
}
