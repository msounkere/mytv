package vod

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mytv/internal/config"
	"mytv/internal/domain"
	"mytv/internal/remux"
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

// bailFaux compte les rendus ; un meme objet est rendu a une meme session (reutilisation).
type bailFaux struct {
	renouvele, libere atomic.Int32
}

func (b *bailFaux) Renouveler() { b.renouvele.Add(1) }
func (b *bailFaux) Liberer()    { b.libere.Add(1) }

type acquisition struct{ fournisseur, session, ressource string }

type bauxFaux struct {
	mu      sync.Mutex
	demande []acquisition
	baux    map[string]*bailFaux
	err     error
	attente chan struct{} // si non nil : Acquerir attend sa fermeture
}

func (b *bauxFaux) Acquerir(ctx context.Context, f, session, ressource string) (domain.Bail, error) {
	b.mu.Lock()
	b.demande = append(b.demande, acquisition{f, session, ressource})
	attente, err := b.attente, b.err
	b.mu.Unlock()
	if attente != nil {
		select {
		case <-attente:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.baux == nil {
		b.baux = map[string]*bailFaux{}
	}
	if b.baux[session] == nil {
		b.baux[session] = &bailFaux{}
	}
	return b.baux[session], nil
}

func (b *bauxFaux) bail(session string) *bailFaux {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.baux[session] == nil {
		return &bailFaux{}
	}
	return b.baux[session]
}

func (b *bauxFaux) acquisitions() []acquisition {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]acquisition(nil), b.demande...)
}

type sondeurFaux struct {
	mu      sync.Mutex
	sonde   domain.Sonde
	err     error
	appels  []string // identifiants sondes
	sources []domain.SourceOctets
}

func (s *sondeurFaux) Sonder(_ context.Context, cle string, src domain.SourceOctets) (*domain.Sonde, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.appels = append(s.appels, cle)
	s.sources = append(s.sources, src)
	if s.err != nil {
		return nil, s.err
	}
	r := s.sonde
	return &r, nil
}

var (
	sondeMP4 = domain.Sonde{Conteneur: "mov,mp4,m4a,3gp,3g2,mj2", CodecVideo: "h264", CodecAudio: "aac"}
	sondeMKV = domain.Sonde{Conteneur: "matroska,webm", CodecVideo: "h264", CodecAudio: "aac"}
	sondeEAC = domain.Sonde{Conteneur: "matroska,webm", CodecVideo: "h264", CodecAudio: "eac3"}
	sondeHEV = domain.Sonde{Conteneur: "matroska,webm", CodecVideo: "hevc", CodecAudio: "aac"}
)

// procTest : un ffmpeg de comedie dont la sortie est un tube.
type procTest struct {
	pr      *io.PipeReader
	pw      *io.PipeWriter
	termine chan struct{}
	once    sync.Once
	arrets  atomic.Int32
	finErr  error
	tache   domain.TacheRemux
}

func nouveauProcTest(t domain.TacheRemux) *procTest {
	pr, pw := io.Pipe()
	return &procTest{pr: pr, pw: pw, termine: make(chan struct{}), tache: t}
}
func (p *procTest) Sortie() io.ReadCloser { return p.pr }
func (p *procTest) Arreter() {
	p.arrets.Add(1)
	p.mourir(nil)
}
func (p *procTest) mourir(err error) {
	p.once.Do(func() {
		p.finErr = err
		p.pw.CloseWithError(io.EOF)
		close(p.termine)
	})
}
func (p *procTest) Attendre() error { <-p.termine; return p.finErr }

type lanceurFaux struct {
	mu        sync.Mutex
	procs     []*procTest
	err       error
	surLancer func(*procTest)
}

func (l *lanceurFaux) Lancer(_ context.Context, t domain.TacheRemux) (domain.Processus, error) {
	if l.err != nil {
		return nil, l.err
	}
	p := nouveauProcTest(t)
	l.mu.Lock()
	l.procs = append(l.procs, p)
	cb := l.surLancer
	l.mu.Unlock()
	if cb != nil {
		go cb(p)
	}
	return p, nil
}

func (l *lanceurFaux) liste() []*procTest {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]*procTest(nil), l.procs...)
}

// instrFaux enregistre ce que le service publie.
type instrFaux struct {
	mu         sync.Mutex
	compteurs  map[string]float64
	evenements []string
}

// Compter cumule par nom, et aussi par nom + etiquettes (« vod_erreurs_total{cause=coupure} »).
func (i *instrFaux) Compter(nom string, d float64, e ...string) {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.compteurs == nil {
		i.compteurs = map[string]float64{}
	}
	i.compteurs[nom] += d
	if len(e) >= 2 {
		var et []string
		for j := 0; j+1 < len(e); j += 2 {
			et = append(et, e[j]+"="+e[j+1])
		}
		i.compteurs[nom+"{"+strings.Join(et, ",")+"}"] += d
	}
}
func (i *instrFaux) Observer(string, float64, ...string) {}
func (i *instrFaux) Evenement(t, m string) {
	i.mu.Lock()
	i.evenements = append(i.evenements, t+" "+m)
	i.mu.Unlock()
}
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
	baux     *bauxFaux
	sondeur  *sondeurFaux
	lanceur  *lanceurFaux
	instr    *instrFaux
	svc      *Service
	http     *httptest.Server
	erreurs  chan error
}

func reseau() config.Reseau {
	return config.Reseau{DelaiConnexion: config.Duree(2 * time.Second), DelaiEntete: config.Duree(5 * time.Second)}
}

func nouveauBanc(t *testing.T, opt fauxiptv.Options, r func(*Reglages)) *banc {
	t.Helper()
	avant := runtime.NumGoroutine()
	// Enregistre en premier, donc execute en dernier : plus aucune goroutine ne doit
	// survivre au banc (lectures, ffmpeg de comedie, renouvellement de bail...).
	t.Cleanup(func() { sansFuite(t, avant) })
	if opt.TailleFilm == 0 {
		opt.TailleFilm = tailleFilm
	}
	b := &banc{t: t, faux: fauxiptv.Nouveau(t, opt), baux: &bauxFaux{}, sondeur: &sondeurFaux{sonde: sondeMP4},
		lanceur: &lanceurFaux{}, instr: &instrFaux{}, erreurs: make(chan error, 64)}
	b.client = xtream.Nouveau(config.Fournisseur{ID: "iptva", Base: b.faux.URL(), Reseau: reseau()}, b.faux.Utilisateur(), b.faux.MotDePasse())
	b.stockage = &stockageFaux{
		films: map[domain.ContenuID]domain.Film{
			"iptva:2001": {ID: "iptva:2001", FournisseurID: "iptva", Numero: "2001", Extension: "mp4"},
			"iptva:2002": {ID: "iptva:2002", FournisseurID: "iptva", Numero: "2002", Extension: "mkv"},
		},
		series: map[domain.ContenuID]domain.SerieTV{"iptva:3001": {ID: "iptva:3001", FournisseurID: "iptva", Numero: "3001"}},
	}
	rg := ReglagesParDefaut()
	rg.Tampon = 16 << 10
	rg.UserAgent = "Mozilla/5.0 test"
	if r != nil {
		r(&rg)
	}
	svc, err := Nouveau(Dependances{
		Fournisseurs: map[string]domain.Fournisseur{"iptva": b.client}, Stockage: b.stockage, Baux: b.baux,
		Sondeur: b.sondeur, Decideur: remux.Decider, Lanceur: b.lanceur, Instrumentation: b.instr,
	}, rg)
	if err != nil {
		t.Fatal(err)
	}
	b.svc = svc
	b.http = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		defer func() {
			// Une reponse entamee puis en echec est ABANDONNEE par Servir (panic ErrAbortHandler,
			// comme la relance l'API) : le banc le note, et laisse net/http couper la connexion.
			if v := recover(); v != nil {
				if v != http.ErrAbortHandler {
					panic(v)
				}
				select {
				case b.erreurs <- errAbandon:
				default:
				}
				panic(v)
			}
		}()
		err := svc.Servir(w, req, req.Header.Get("X-Session"), domain.ContenuID(req.URL.Query().Get("id")))
		if err != nil {
			select {
			case b.erreurs <- err:
			default:
			}
			// Contrat : si rien n'a ete ecrit, l'appelant repond le statut.
			if w.Header().Get("Content-Type") == "" {
				http.Error(w, "erreur", StatutHTTP(err))
			}
		}
	}))
	t.Cleanup(func() {
		b.http.Close()
		b.client.Fermer()
		http.DefaultClient.CloseIdleConnections()
	})
	return b
}

// errAbandon : Servir a abandonne une reponse entamee (voir le gestionnaire du banc). La
// cause se lit dans les compteurs etiquetes (« vod_erreurs_total{cause=coupure} »).
var errAbandon = errors.New("banc : reponse abandonnee apres l'envoi des en-tetes")

// abandonAvecCause verifie que la derniere lecture a ete abandonnee (reponse tronquee) et
// comptee en erreur avec cette cause.
func (b *banc) abandonAvecCause(cause string) {
	b.t.Helper()
	if e := b.derniereErreur(); e != errAbandon {
		b.t.Errorf("erreur = %v ; attendu une reponse abandonnee (ErrAbortHandler)", e)
	}
	if n := b.instr.valeur("vod_erreurs_total{cause=" + cause + "}"); n != 1 {
		b.t.Errorf("vod_erreurs_total{cause=%s} = %v, attendu 1", cause, n)
	}
}

func (b *banc) derniereErreur() error {
	select {
	case e := <-b.erreurs:
		return e
	case <-time.After(3 * time.Second):
		b.t.Helper()
		b.t.Fatal("aucune erreur de Servir")
		return nil
	}
}

func (b *banc) get(id, session, plage string) (*http.Response, []byte, error) {
	b.t.Helper()
	req, err := http.NewRequest("GET", b.http.URL+"/?id="+url.QueryEscape(id), nil)
	if err != nil {
		b.t.Fatal(err)
	}
	req.Header.Set("X-Session", session)
	if plage != "" {
		req.Header.Set("Range", plage)
	}
	rep, err := http.DefaultClient.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer rep.Body.Close()
	corps, err := io.ReadAll(rep.Body)
	return rep, corps, err
}

func (b *banc) contenuAttendu() []byte {
	b.t.Helper()
	rep, err := http.Get(b.faux.URL() + "/movie/" + b.faux.Utilisateur() + "/" + b.faux.MotDePasse() + "/2001.mp4")
	if err != nil {
		b.t.Fatal(err)
	}
	defer rep.Body.Close()
	c, _ := io.ReadAll(rep.Body)
	return c
}

func attendre(t *testing.T, cond func() bool, quoi string) {
	t.Helper()
	fin := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(fin) {
			t.Fatalf("delai depasse : %s", quoi)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func rangesVus(f *fauxiptv.Faux) []string {
	var r []string
	for _, tr := range f.Traces() {
		if strings.Contains(tr.Chemin, "/movie/") || strings.Contains(tr.Chemin, "/series/") {
			r = append(r, tr.Entetes.Get("Range"))
		}
	}
	return r
}

var _ = errors.Is
var _ = strconv.Itoa

// rebrancher remplace le fournisseur par un client xtream pointe sur une autre base
// (fournisseur « fait main » dont on controle les reponses aux films).
func (b *banc) rebrancher(base string) {
	b.t.Helper()
	cl := xtream.Nouveau(config.Fournisseur{ID: "iptva", Base: base, Reseau: reseau()}, b.faux.Utilisateur(), b.faux.MotDePasse())
	b.client = cl
	b.svc.srcs["iptva"] = source{f: cl, vod: cl}
}

// sansFuite attend que le nombre de goroutines retombe a son niveau initial.
func sansFuite(t *testing.T, avant int) {
	t.Helper()
	fin := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > avant {
		if time.Now().After(fin) {
			buf := make([]byte, 1<<16)
			buf = buf[:runtime.Stack(buf, true)]
			t.Errorf("fuite de goroutines : %d avant, %d apres\n%s", avant, runtime.NumGoroutine(), buf)
			return
		}
		http.DefaultClient.CloseIdleConnections()
		time.Sleep(10 * time.Millisecond)
	}
}
