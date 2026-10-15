package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mytv/internal/comptes"
	"mytv/internal/config"
	"mytv/internal/domain"
)

// stockageFaux : catalogue en memoire, ordonne par insertion, avec filtre fournisseur.
type stockageFaux struct {
	mu       sync.Mutex
	chaines  []domain.Chaine
	cats     []domain.Categorie
	films    []domain.Film
	series   []domain.SerieTV
	synchro  map[string]time.Time
	requetes int
}

var _ domain.Stockage = (*stockageFaux)(nil)
var _ domain.StockageVideo = (*stockageFaux)(nil)

func tranche[T any](l []T, ok func(T) bool, lim, dec int) []T {
	var out []T
	for _, e := range l {
		if ok(e) {
			out = append(out, e)
		}
	}
	if dec > len(out) {
		dec = len(out)
	}
	out = out[dec:]
	if lim > 0 && lim < len(out) {
		out = out[:lim]
	}
	return out
}

func (s *stockageFaux) filtre(f domain.FiltreChaines, fid, cat, nom string) bool {
	return (f.FournisseurID == "" || f.FournisseurID == fid) && (f.CategorieID == "" || f.CategorieID == cat) &&
		(f.Recherche == "" || strings.Contains(strings.ToLower(nom), strings.ToLower(f.Recherche)))
}

func (s *stockageFaux) EcrireCatalogue(context.Context, *domain.Instantane) error { return nil }
func (s *stockageFaux) DerniereSynchro(_ context.Context, f string) (time.Time, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.synchro[f], nil
}
func (s *stockageFaux) Chaines(_ context.Context, f domain.FiltreChaines) ([]domain.Chaine, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requetes++
	return tranche(s.chaines, func(c domain.Chaine) bool { return s.filtre(f, c.FournisseurID, c.CategorieID, c.Nom) }, f.Limite, f.Decalage), nil
}
func (s *stockageFaux) Categories(_ context.Context, t domain.Type) ([]domain.Categorie, error) {
	return tranche(s.cats, func(c domain.Categorie) bool { return c.Type == t }, 0, 0), nil
}
func (s *stockageFaux) Chaine(_ context.Context, id domain.ContenuID) (*domain.Chaine, error) {
	for _, c := range s.chaines {
		if c.ID == id {
			return &c, nil
		}
	}
	return nil, domain.ErrNonTrouve
}
func (s *stockageFaux) Fermer() error { return nil }
func (s *stockageFaux) Films(_ context.Context, f domain.FiltreChaines) ([]domain.Film, error) {
	return tranche(s.films, func(c domain.Film) bool { return s.filtre(f, c.FournisseurID, c.CategorieID, c.Nom) }, f.Limite, f.Decalage), nil
}
func (s *stockageFaux) Film(_ context.Context, id domain.ContenuID) (*domain.Film, error) {
	for _, c := range s.films {
		if c.ID == id {
			return &c, nil
		}
	}
	return nil, domain.ErrNonTrouve
}
func (s *stockageFaux) Series(_ context.Context, f domain.FiltreChaines) ([]domain.SerieTV, error) {
	return tranche(s.series, func(c domain.SerieTV) bool { return s.filtre(f, c.FournisseurID, c.CategorieID, c.Nom) }, f.Limite, f.Decalage), nil
}
func (s *stockageFaux) Serie(_ context.Context, id domain.ContenuID) (*domain.SerieTV, error) {
	for _, c := range s.series {
		if c.ID == id {
			return &c, nil
		}
	}
	return nil, domain.ErrNonTrouve
}

type vodFaux struct{ saisons []domain.Saison }

func (v vodFaux) URLFilm(string, string) string    { return "" }
func (v vodFaux) URLEpisode(string, string) string { return "" }
func (v vodFaux) Saisons(_ context.Context, n string) ([]domain.Saison, error) {
	if n == "boom" {
		return nil, domain.ErrAmont
	}
	return v.saisons, nil
}

type lectureFausse struct {
	mu       sync.Mutex
	sessions []string
	err      error
}

func (l *lectureFausse) Servir(w http.ResponseWriter, _ *http.Request, session string, id domain.ContenuID) error {
	l.mu.Lock()
	l.sessions = append(l.sessions, session)
	l.mu.Unlock()
	if l.err != nil {
		return l.err
	}
	w.Header().Set("Content-Type", "video/mp4")
	fmt.Fprint(w, "film:"+string(id))
	return nil
}

type etatFaux struct {
	mu       sync.Mutex
	favoris  map[string][]domain.ContenuID
	reglages map[string]domain.Reglages
	lectures map[string][]domain.Lecture
}

func nouvelEtat() *etatFaux {
	return &etatFaux{favoris: map[string][]domain.ContenuID{}, reglages: map[string]domain.Reglages{}, lectures: map[string][]domain.Lecture{}}
}

func (e *etatFaux) AjouterFavori(_ context.Context, p string, c domain.ContenuID) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, x := range e.favoris[p] {
		if x == c {
			return nil
		}
	}
	e.favoris[p] = append(e.favoris[p], c)
	return nil
}
func (e *etatFaux) RetirerFavori(_ context.Context, p string, c domain.ContenuID) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	var n []domain.ContenuID
	for _, x := range e.favoris[p] {
		if x != c {
			n = append(n, x)
		}
	}
	e.favoris[p] = n
	return nil
}
func (e *etatFaux) Favoris(_ context.Context, p string) ([]domain.ContenuID, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]domain.ContenuID(nil), e.favoris[p]...), nil
}
func (e *etatFaux) EnregistrerLecture(_ context.Context, p string, l domain.Lecture) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lectures[p] = append([]domain.Lecture{l}, e.lectures[p]...)
	return nil
}
func (e *etatFaux) Historique(_ context.Context, p string, lim int) ([]domain.Lecture, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	l := e.lectures[p]
	if lim > 0 && lim < len(l) {
		l = l[:lim]
	}
	return append([]domain.Lecture(nil), l...), nil
}
func (e *etatFaux) OublierLecture(_ context.Context, p string, c domain.ContenuID) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	var n []domain.Lecture
	for _, x := range e.lectures[p] {
		if x.Contenu != c {
			n = append(n, x)
		}
	}
	e.lectures[p] = n
	return nil
}
func (e *etatFaux) Reglages(_ context.Context, p string) (domain.Reglages, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.reglages[p], nil
}
func (e *etatFaux) EcrireReglages(_ context.Context, p string, v map[string]string, rev int64) (domain.Reglages, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.reglages[p].Revision != rev {
		return domain.Reglages{}, domain.ErrConflit
	}
	r := domain.Reglages{Valeurs: v, Revision: rev + 1}
	e.reglages[p] = r
	return r, nil
}

type epgFaux struct{}

func (epgFaux) EcrireEPG(context.Context, *domain.InstantaneEPG) error { return nil }
func (epgFaux) DerniereSynchroEPG(context.Context, string) (time.Time, error) {
	return time.Time{}, nil
}
func (epgFaux) EnCoursEtSuivant(_ context.Context, c domain.ContenuID, a time.Time) (*domain.Programme, *domain.Programme, error) {
	return &domain.Programme{EpgID: string(c), Debut: a.Add(-time.Hour), Fin: a.Add(time.Hour), Titre: "Journal"}, nil, nil
}
func (epgFaux) Grille(_ context.Context, ids []domain.ContenuID, de, a time.Time) (map[domain.ContenuID][]domain.Programme, error) {
	return map[domain.ContenuID][]domain.Programme{ids[0]: {{Debut: de, Fin: a, Titre: "Film"}}}, nil
}

// banc assemble l'API avec un VRAI service de comptes.
type banc struct {
	t       *testing.T
	api     *API
	h       http.Handler
	st      *stockageFaux
	etat    *etatFaux
	lecture *lectureFausse
	cfg     config.Config
	svc     *comptes.Service
	live    *liveFaux
}

// liveFaux se comporte comme le vrai relais : il fait lui-meme l'authentification.
type liveFaux struct {
	mu   sync.Mutex
	vues []string
	auth func(*http.Request) (string, bool)
}

func (l *liveFaux) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if _, ok := l.auth(req); !ok {
		http.Error(w, "authentification requise", http.StatusUnauthorized)
		return
	}
	l.mu.Lock()
	l.vues = append(l.vues, req.URL.Path)
	l.mu.Unlock()
	w.WriteHeader(204)
}

const mdp = "un mot de passe solide"

func nouveauBanc(t *testing.T, mod func(*Options)) *banc {
	t.Helper()
	cfg := config.Defauts()
	cfg.Comptes.Argon2.MemoireKio, cfg.Comptes.Argon2.Iterations, cfg.Comptes.Argon2.Parallelisme = 64, 1, 1
	cfg.Comptes.CookieSecurise = false
	db, err := comptes.OuvrirBase(filepath.Join(t.TempDir(), "c.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	svc, err := comptes.Nouveau(context.Background(), comptes.Options{DB: db, Config: cfg.Comptes})
	if err != nil {
		t.Fatal(err)
	}
	b := &banc{t: t, cfg: cfg, svc: svc, st: &stockageFaux{synchro: map[string]time.Time{"a": time.UnixMilli(1000)}},
		etat: nouvelEtat(), lecture: &lectureFausse{}, live: &liveFaux{}}
	opt := Options{
		Config: cfg.API, Profil: cfg.Profil, Fournisseurs: []string{"a"}, Stockage: b.st, Video: b.st,
		VOD:     map[string]domain.FournisseurVOD{"a": vodFaux{saisons: []domain.Saison{{Numero: 1, Episodes: []domain.Episode{{ID: "a:9.mkv", Numero: 1, Titre: "Pilote", Extension: "mkv", Duree: 90 * time.Second}}}}}},
		Comptes: svc, Auth: svc, EPG: epgFaux{}, Etat: b.etat, Lecture: b.lecture, Live: b.live,
		Admin:   map[string]http.Handler{"metriques": http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "m 1") })},
		Horloge: func() time.Time { return time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC) },
	}
	if mod != nil {
		mod(&opt)
	}
	b.live.auth = svc.AuthentifierLecture
	b.api, err = Nouveau(opt)
	if err != nil {
		t.Fatal(err)
	}
	b.h = b.api.Handler()
	return b
}

// requete execute une requete et rend la reponse.
func (b *banc) requete(methode, chemin, corps string, mod ...func(*http.Request)) *httptest.ResponseRecorder {
	b.t.Helper()
	var r *http.Request
	if corps != "" {
		r = httptest.NewRequest(methode, chemin, strings.NewReader(corps))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(methode, chemin, nil)
	}
	r.RemoteAddr = "203.0.113.5:4444"
	for _, m := range mod {
		m(r)
	}
	rec := httptest.NewRecorder()
	b.h.ServeHTTP(rec, r)
	return rec
}

func avec(c *http.Cookie) func(*http.Request) {
	return func(r *http.Request) {
		if c != nil {
			r.AddCookie(c)
		}
	}
}

// connecte cree l'utilisateur et ouvre une session PAR L'API (cookie reel).
func (b *banc) connecte(nom string, admin bool) *http.Cookie {
	b.t.Helper()
	if _, err := b.svc.CreerUtilisateur(context.Background(), nom, mdp, admin); err != nil {
		b.t.Fatal(err)
	}
	rec := b.requete("POST", "/api/v1/connexion", fmt.Sprintf(`{"nom":%q,"mot_de_passe":%q}`, nom, mdp))
	if rec.Code != 200 {
		b.t.Fatalf("connexion : %d %s", rec.Code, rec.Body)
	}
	cs := rec.Result().Cookies()
	if len(cs) != 1 {
		b.t.Fatalf("cookies : %v", cs)
	}
	return cs[0]
}
