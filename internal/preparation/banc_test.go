package preparation

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mytv/internal/baux"
	"mytv/internal/config"
	"mytv/internal/domain"
	"mytv/internal/testutil/fauxiptv"
	"mytv/internal/xtream"
)

// ---------------------------------------------------------------- doublures

type stockageFaux struct {
	films  map[domain.ContenuID]domain.Film
	series map[domain.ContenuID]domain.SerieTV
}

func (s *stockageFaux) Films(context.Context, domain.FiltreChaines) ([]domain.Film, error) {
	return nil, nil
}
func (s *stockageFaux) Film(_ context.Context, id domain.ContenuID) (*domain.Film, error) {
	if f, ok := s.films[id]; ok {
		return &f, nil
	}
	return nil, domain.ErrNonTrouve
}
func (s *stockageFaux) Series(context.Context, domain.FiltreChaines) ([]domain.SerieTV, error) {
	return nil, nil
}
func (s *stockageFaux) Serie(_ context.Context, id domain.ContenuID) (*domain.SerieTV, error) {
	if f, ok := s.series[id]; ok {
		return &f, nil
	}
	return nil, domain.ErrNonTrouve
}

// listeFaux : une domain.ListeAVoir en memoire.
type listeFaux struct {
	mu     sync.Mutex
	listes map[string][]domain.ElementAVoir
}

func (l *listeFaux) AVoir(_ context.Context, p string) ([]domain.ElementAVoir, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]domain.ElementAVoir(nil), l.listes[p]...), nil
}
func (l *listeFaux) AjouterAVoir(_ context.Context, p string, e domain.ElementAVoir) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e.ID == "" {
		if e.Serie {
			e.ID = domain.AVoirSerie(e.Contenu)
		} else {
			e.ID = domain.AVoirFilm(e.Contenu)
		}
	}
	for i, x := range l.listes[p] {
		if x.ID == e.ID {
			e.Rang = x.Rang
			l.listes[p][i] = e
			return nil
		}
	}
	e.Rang = len(l.listes[p])
	l.listes[p] = append(l.listes[p], e)
	return nil
}
func (l *listeFaux) RetirerAVoir(_ context.Context, p string, id domain.AVoirID) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	var reste []domain.ElementAVoir
	for _, x := range l.listes[p] {
		if x.ID != id {
			reste = append(reste, x)
		}
	}
	l.listes[p] = reste
	return nil
}
func (l *listeFaux) OrdonnerAVoir(_ context.Context, p string, ids []domain.AVoirID) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	var ordre []domain.ElementAVoir
	for _, id := range ids {
		for _, x := range l.listes[p] {
			if x.ID == id {
				ordre = append(ordre, x)
			}
		}
	}
	for _, x := range l.listes[p] {
		deja := false
		for _, y := range ordre {
			deja = deja || y.ID == x.ID
		}
		if !deja {
			ordre = append(ordre, x)
		}
	}
	l.listes[p] = ordre
	return nil
}
func (l *listeFaux) ProfilsAVoir(context.Context) ([]string, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var ps []string
	for p, x := range l.listes {
		if len(x) > 0 {
			ps = append(ps, p)
		}
	}
	return ps, nil
}

// etatFaux : seul l'historique compte ici.
type etatFaux struct {
	mu                sync.Mutex
	lectures          map[string][]domain.Lecture
	domain.EtatProfil // les autres methodes ne sont pas appelees
}

func (e *etatFaux) Historique(_ context.Context, p string, _ int) ([]domain.Lecture, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]domain.Lecture(nil), e.lectures[p]...), nil
}

// vu enregistre une lecture terminee (part >= 0,95), en tete de l'historique.
func (e *etatFaux) vu(p string, c, serie domain.ContenuID) {
	e.lecture(p, c, serie, 96, 100)
}

func (e *etatFaux) lecture(p string, c, serie domain.ContenuID, pos, dur int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.lectures == nil {
		e.lectures = map[string][]domain.Lecture{}
	}
	var reste []domain.Lecture
	for _, l := range e.lectures[p] {
		if l.Contenu != c {
			reste = append(reste, l)
		}
	}
	l := domain.Lecture{Contenu: c, Serie: serie, Position: time.Duration(pos) * time.Second, Duree: time.Duration(dur) * time.Second, Vu: time.Now()}
	e.lectures[p] = append([]domain.Lecture{l}, reste...)
}

type occupationFixe struct {
	mu sync.Mutex
	n  int
}

func (o *occupationFixe) Occupes(string) int { o.mu.Lock(); defer o.mu.Unlock(); return o.n }

type instrFaux struct {
	mu        sync.Mutex
	compteurs map[string]float64
}

func (i *instrFaux) Compter(nom string, d float64, e ...string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.compteurs == nil {
		i.compteurs = map[string]float64{}
	}
	i.compteurs[nom] += d
	if len(e) >= 2 {
		i.compteurs[nom+"{"+e[0]+"="+e[1]+"}"] += d
	}
}
func (i *instrFaux) Observer(string, float64, ...string) {}
func (i *instrFaux) Evenement(string, string)            {}
func (i *instrFaux) valeur(nom string) float64 {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.compteurs[nom]
}

// ---------------------------------------------------------------- banc

const tailleFilm = 300_000

type banc struct {
	t        *testing.T
	faux     *fauxiptv.Faux
	client   *xtream.Client
	stockage *stockageFaux
	liste    *listeFaux
	etat     *etatFaux
	occ      Occupation
	instr    *instrFaux
	dossier  string
	m        *Moteur
}

func reseau() config.Reseau {
	return config.Reseau{DelaiConnexion: config.Duree(2 * time.Second), DelaiEntete: config.Duree(5 * time.Second)}
}

// saisonsTest : la serie iptva:s77 a 5 episodes en saison 1 (7701..7705, mp4).
func saisonsTest(context.Context, domain.SerieTV) ([]domain.Saison, error) {
	var eps []domain.Episode
	for i := 1; i <= 5; i++ {
		eps = append(eps, domain.Episode{ID: domain.ContenuID(fmt.Sprintf("iptva:770%d.mp4", i)), Numero: i, Extension: "mp4"})
	}
	return []domain.Saison{{Numero: 1, Episodes: eps}}, nil
}

func episode(n int) domain.ContenuID { return domain.ContenuID(fmt.Sprintf("iptva:770%d.mp4", n)) }

func nouveauBanc(t *testing.T, opt fauxiptv.Options, r func(*Reglages), occ Occupation) *banc {
	t.Helper()
	if opt.TailleFilm == 0 {
		opt.TailleFilm = tailleFilm
	}
	// Identifiants reconnaissables : aucun ne doit apparaitre dans un nom de fichier ni une cause.
	opt.Utilisateur, opt.MotDePasse = "utilisateur-secret", "motdepasse-secret"
	b := &banc{t: t, faux: fauxiptv.Nouveau(t, opt), liste: &listeFaux{listes: map[string][]domain.ElementAVoir{}}, etat: &etatFaux{},
		instr: &instrFaux{}, dossier: filepath.Join(t.TempDir(), "prepares")}
	b.client = xtream.Nouveau(config.Fournisseur{ID: "iptva", Base: b.faux.URL(), Reseau: reseau()}, b.faux.Utilisateur(), b.faux.MotDePasse())
	t.Cleanup(b.client.Fermer)
	b.stockage = &stockageFaux{
		films: map[domain.ContenuID]domain.Film{
			"iptva:2001": {ID: "iptva:2001", FournisseurID: "iptva", Numero: "2001", Extension: "mp4"},
			"iptva:2002": {ID: "iptva:2002", FournisseurID: "iptva", Numero: "2002", Extension: "mkv"},
		},
		series: map[domain.ContenuID]domain.SerieTV{"iptva:s77": {ID: "iptva:s77", FournisseurID: "iptva", Numero: "77"}},
	}
	if occ == nil {
		occ = &occupationFixe{}
	}
	b.occ = occ
	rg := Reglages{Dossier: b.dossier, Budget: 100 * tailleFilm, EpisodesAvance: 2, RepriseApres: 0, Intervalle: 200 * time.Millisecond, Tampon: 16 << 10}
	if r != nil {
		r(&rg)
	}
	b.m = b.moteur(rg)
	return b
}

func (b *banc) moteur(rg Reglages) *Moteur {
	b.t.Helper()
	m, err := Nouveau(Dependances{Fournisseurs: map[string]domain.Fournisseur{"iptva": b.client}, Stockage: b.stockage, Liste: b.liste,
		Etat: b.etat, Saisons: saisonsTest, Occupation: b.occ, Instrumentation: b.instr, Journal: journalTest(b.t)}, rg)
	if err != nil {
		b.t.Fatal(err)
	}
	return m
}

// journalTest ecrit le journal du moteur dans le test (horodate, lisible avec -v).
func journalTest(t *testing.T) *slog.Logger {
	return slog.New(slog.NewTextHandler(&ecrivainTest{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

type ecrivainTest struct{ t *testing.T }

func (e *ecrivainTest) Write(p []byte) (int, error) {
	e.t.Logf("%s %s", time.Now().Format("15:04:05.000"), strings.TrimSpace(string(p)))
	return len(p), nil
}

func (b *banc) contenuAttendu(nom string) []byte {
	b.t.Helper()
	rep, err := http.Get(b.faux.URL() + "/movie/" + b.faux.Utilisateur() + "/" + b.faux.MotDePasse() + "/" + nom)
	if err != nil {
		b.t.Fatal(err)
	}
	defer rep.Body.Close()
	c, _ := io.ReadAll(rep.Body)
	return c
}

// rangesVus : les en-tetes Range recus par le faux pour les films, dans l'ordre.
func rangesVus(f *fauxiptv.Faux) []string {
	var r []string
	for _, t := range f.Traces() {
		if strings.Contains(t.Chemin, "/movie/") || strings.Contains(t.Chemin, "/series/") {
			r = append(r, t.Entetes.Get("Range"))
		}
	}
	return r
}

// fichiersSurDisque compte les fichiers de contenu (prets + partiels) du dossier.
func (b *banc) fichiersSurDisque() int {
	n := 0
	entrees, _ := os.ReadDir(b.dossier)
	for _, e := range entrees {
		if e.Name() != nomIndex && !strings.HasSuffix(e.Name(), ".tmp") {
			n++
		}
	}
	return n
}

func (b *banc) attendre(cond func() bool, delai time.Duration, msg string) {
	b.t.Helper()
	limite := time.Now().Add(delai)
	for time.Now().Before(limite) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	b.t.Fatalf("delai depasse : %s", msg)
}

func (b *banc) etatDe(id domain.ContenuID) domain.EtatPreparation {
	for _, f := range b.m.Fichiers() {
		if f.Contenu == id {
			return f.Etat
		}
	}
	return ""
}

// lancer fait tourner le moteur jusqu'a la fin du test.
func (b *banc) lancer(m *Moteur) {
	ctx, annuler := context.WithCancel(context.Background())
	fini := make(chan struct{})
	go func() { defer close(fini); m.Lancer(ctx) }()
	b.t.Cleanup(func() { annuler(); <-fini })
}

// gestionnaireBaux : de vrais baux, veilleur branche sur le moteur (CA3).
func gestionnaireBaux(t *testing.T) *baux.Gestionnaire {
	t.Helper()
	g, err := baux.Nouveau(config.Bail{TTL: config.Duree(90 * time.Second), PatienceMax: config.Duree(time.Second), Reessai: config.Duree(20 * time.Millisecond)},
		map[string]int{"iptva": 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return g
}
