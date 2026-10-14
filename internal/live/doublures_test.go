package live

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mytv/internal/config"
	"mytv/internal/domain"
)

// Doublures minimales : elles ne dependent d'aucun autre module du lot. Les tests
// d'integration avec le vrai faux fournisseur et les vrais baux sont dans
// integration_test.go.

// fournisseurFactice parle a un httptest.Server et traduit les statuts comme le fera
// le client Xtream (US-03 CA3).
type fournisseurFactice struct {
	base   string
	client *http.Client
}

func (f *fournisseurFactice) ID() string { return "test" }
func (f *fournisseurFactice) Catalogue(context.Context) (*domain.Instantane, error) {
	return nil, errors.New("hors sujet")
}
func (f *fournisseurFactice) URLLive(numero string) string {
	return f.base + "/live/u/p/" + numero + ".m3u8"
}

func (f *fournisseurFactice) Requete(ctx context.Context, cible string, entetes http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cible, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range entetes {
		req.Header[k] = v
	}
	rep, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w : reseau", domain.ErrAmont)
	}
	var typee error
	switch c := rep.StatusCode; {
	case c == 407:
		typee = domain.ErrRefuseConnexions
	case c == 458:
		typee = domain.ErrRefuseIP
	case c == 404:
		typee = domain.ErrIntrouvable
	case c == 401 || c == 403:
		typee = domain.ErrIdentifiants
	case c >= 500:
		typee = domain.ErrAmont
	}
	if typee != nil {
		rep.Body.Close()
		return nil, typee
	}
	return rep, nil
}

// bauxFactice compte ce que le relais demande, et respecte la regle « un bail par session ».
type bauxFactice struct {
	mu           sync.Mutex
	baux         map[string]*bailFactice // par session
	acquisitions []string                // « session/ressource »
	err          error
}

type bailFactice struct {
	renouvele atomic.Int32
	libere    atomic.Int32
}

func (b *bailFactice) Renouveler() { b.renouvele.Add(1) }
func (b *bailFactice) Liberer()    { b.libere.Add(1) }

func (x *bauxFactice) Acquerir(ctx context.Context, fournisseurID, session, ressource string) (domain.Bail, error) {
	x.mu.Lock()
	defer x.mu.Unlock()
	if x.err != nil {
		return nil, x.err
	}
	if x.baux == nil {
		x.baux = map[string]*bailFactice{}
	}
	x.acquisitions = append(x.acquisitions, session+"/"+ressource)
	b, ok := x.baux[session]
	if !ok {
		b = &bailFactice{}
		x.baux[session] = b
	}
	return b, nil
}

func (x *bauxFactice) bail(session string) *bailFactice {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.baux[session]
}

func (x *bauxFactice) nbAcquisitions() int {
	x.mu.Lock()
	defer x.mu.Unlock()
	return len(x.acquisitions)
}

var secretTest = []byte("0123456789abcdef0123456789abcdef")

// reglages rend des durees courtes : les tests doivent rester rapides.
func reglages() (config.Live, config.Bail, config.Reseau) {
	var l config.Live
	l.PeremptionSecours = config.Duree(time.Minute)
	l.CacheSegments.Nombre = 14
	l.CacheSegments.Octets = 1 << 20
	l.TableSegments.Nombre = 100
	l.TableSegments.Validite = config.Duree(10 * time.Minute)
	b := config.Bail{
		TTL:         config.Duree(time.Minute),
		PatienceMax: config.Duree(400 * time.Millisecond),
		Reessai:     config.Duree(20 * time.Millisecond),
	}
	n := config.Reseau{
		DelaiPlaylist:    config.Duree(time.Second),
		EssaisPlaylist:   3,
		AttenteEssais:    config.Duree(5 * time.Millisecond),
		DelaiSegment:     config.Duree(2 * time.Second),
		EssaisSegment:    2,
		RequetesAmontMax: 3,
	}
	return l, b, n
}

// authTest : la session est donnee par l'en-tete X-Session ; sans lui, refus.
func authTest(r *http.Request) (string, bool) {
	s := r.Header.Get("X-Session")
	return s, s != ""
}

type banc struct {
	t        *testing.T
	amont    *httptest.Server
	baux     *bauxFactice
	relais   *Relais
	h        http.Handler
	decalage atomic.Int64 // ns ajoutes a l'horloge du relais
}

// nouveauBanc monte un relais devant un fournisseur dont `mux` est le comportement.
func nouveauBanc(t *testing.T, mux http.Handler, ajuste func(*Options)) *banc {
	t.Helper()
	b := &banc{t: t, baux: &bauxFactice{}}
	b.amont = httptest.NewServer(mux)
	t.Cleanup(b.amont.Close)
	l, ba, n := reglages()
	opt := Options{
		Fournisseur:  &fournisseurFactice{base: b.amont.URL, client: b.amont.Client()},
		Baux:         b.baux,
		Live:         l,
		Bail:         ba,
		Reseau:       n,
		Secret:       secretTest,
		Authentifier: authTest,
		Horloge:      func() time.Time { return time.Now().Add(time.Duration(b.decalage.Load())) },
		Journal:      slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if ajuste != nil {
		ajuste(&opt)
	}
	r, err := Nouveau(opt)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(r.Fermer)
	b.relais, b.h = r, r.Handler()
	return b
}

func (b *banc) get(chemin, session string, entetes ...string) *httptest.ResponseRecorder {
	b.t.Helper()
	req := httptest.NewRequest("GET", chemin, nil)
	if session != "" {
		req.Header.Set("X-Session", session)
	}
	for i := 0; i+1 < len(entetes); i += 2 {
		req.Header.Set(entetes[i], entetes[i+1])
	}
	rec := httptest.NewRecorder()
	b.h.ServeHTTP(rec, req)
	return rec
}

func (b *banc) playlist(chaine, session string) *httptest.ResponseRecorder {
	return b.get(PrefixeParDefaut+"/"+chaine+".m3u8", session)
}

// segments extrait les chemins de segments reecrits d'une playlist.
func segments(corps string) []string {
	var r []string
	for _, l := range strings.Split(corps, "\n") {
		if l != "" && !strings.HasPrefix(l, "#") {
			r = append(r, l)
		}
	}
	return r
}

func playlistAmont(hash string) string {
	return "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:10\n#EXT-X-MEDIA-SEQUENCE:5\n" +
		"#EXTINF:10.0,\n/hls/" + hash + "/100_5.ts\n#EXTINF:10.0,\n/hls/" + hash + "/100_6.ts\n"
}

// amontSimple : playlist fixe, segments « DONNEES-TS » ; `playlist` compte les appels.
func amontSimple(appels *atomic.Int32) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) {
		if appels != nil {
			appels.Add(1)
		}
		io.WriteString(w, playlistAmont("aaaa"))
	})
	mux.HandleFunc("/hls/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Content-Length", "12")
		io.WriteString(w, "DONNEES-TS!!")
	})
	return mux
}
