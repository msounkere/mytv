package api

import (
	"bytes"
	"context"
	"encoding/json"
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
	"mytv/internal/live"
	"mytv/internal/testutil/instrum"
)

// ---------------------------------------------------------------- doublures : un VRAI relais du direct

// fournisseurRelais : le fournisseur du relais, devant un serveur httptest.
type fournisseurRelais struct {
	base   string
	client *http.Client
}

func (f *fournisseurRelais) ID() string { return "a" }
func (f *fournisseurRelais) Catalogue(context.Context) (*domain.Instantane, error) {
	return &domain.Instantane{}, nil
}
func (f *fournisseurRelais) URLLive(numero string) string {
	return f.base + "/live/u/p/" + numero + ".m3u8"
}
func (f *fournisseurRelais) Requete(ctx context.Context, cible string, entetes http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", cible, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range entetes {
		req.Header[k] = v
	}
	rep, err := f.client.Do(req)
	if err != nil {
		return nil, domain.ErrAmont
	}
	if rep.StatusCode >= 400 {
		rep.Body.Close()
		return nil, fmt.Errorf("%w : HTTP %d", domain.ErrAmont, rep.StatusCode)
	}
	return rep, nil
}

// bauxRelais note la session de chaque acquisition : c'est ce qui prouve que la requete du
// televiseur est servie COMME la session qui a demande le lien.
type bauxRelais struct {
	mu       sync.Mutex
	sessions []string
}

type bailRelais struct{}

func (bailRelais) Renouveler() {}
func (bailRelais) Liberer()    {}

func (b *bauxRelais) Acquerir(_ context.Context, _, session, _ string) (domain.Bail, error) {
	b.mu.Lock()
	b.sessions = append(b.sessions, session)
	b.mu.Unlock()
	return bailRelais{}, nil
}

func (b *bauxRelais) vues() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.sessions...)
}

const playlistFournisseur = "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:10\n#EXT-X-MEDIA-SEQUENCE:5\n" +
	"#EXTINF:10.0,\n/hls/x/100_5.ts\n#EXTINF:10.0,\n/hls/x/100_6.ts\n"

// bancCast : l'API avec un vrai relais devant un faux fournisseur, une horloge reglable,
// l'instrumentation enregistree et le journal capture.
type bancCast struct {
	*banc
	baux     *bauxRelais
	decalage atomic.Int64
	inst     *instrum.Enregistreur
	journal  *bytes.Buffer
	segments atomic.Int32 // segments demandes au fournisseur
}

func nouveauBancCast(t *testing.T, mod func(*Options)) *bancCast {
	t.Helper()
	bc := &bancCast{baux: &bauxRelais{}, inst: &instrum.Enregistreur{}, journal: &bytes.Buffer{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/live/", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, playlistFournisseur) })
	mux.HandleFunc("/hls/", func(w http.ResponseWriter, _ *http.Request) {
		bc.segments.Add(1)
		w.Header().Set("Content-Type", "video/mp2t")
		w.Header().Set("Content-Length", "12")
		io.WriteString(w, "DONNEES-TS!!")
	})
	amont := httptest.NewServer(mux)
	t.Cleanup(amont.Close)
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	horloge := func() time.Time { return base.Add(time.Duration(bc.decalage.Load())) }
	bc.banc = nouveauBanc(t, func(o *Options) {
		d := config.Defauts()
		reseau := config.Reseau{DelaiPlaylist: config.Duree(2 * time.Second), DelaiSegment: config.Duree(2 * time.Second),
			EssaisPlaylist: 1, EssaisSegment: 1, RequetesAmontMax: 2}
		relais, err := live.Nouveau(live.Options{
			Fournisseur: &fournisseurRelais{base: amont.URL, client: amont.Client()}, Baux: bc.baux,
			Live: d.Live, Bail: d.Bail, Reseau: reseau, Secret: o.Secret,
			Authentifier: o.Auth.AuthentifierLecture, Journal: slog.New(slog.NewTextHandler(io.Discard, nil)),
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(relais.Fermer)
		o.Live = relais.Handler()
		o.Horloge = horloge
		o.Instrumentation = bc.inst
		o.Journal = slog.New(slog.NewTextHandler(bc.journal, nil))
		if mod != nil {
			mod(o)
		}
	})
	return bc
}

type lienCast struct {
	URL    string `json:"url"`
	Expire string `json:"expire"`
}

// lien demande un lien signe pour `lecture` avec le cookie `c`.
func (b *bancCast) lien(c *http.Cookie, lecture, parametres string) lienCast {
	b.t.Helper()
	corps := fmt.Sprintf(`{"lecture":%q}`, lecture)
	if parametres != "" {
		corps = fmt.Sprintf(`{"lecture":%q,"parametres":%q}`, lecture, parametres)
	}
	rec := b.requete("POST", "/api/v1/cast/lien", corps, avec(c))
	if rec.Code != 200 {
		b.t.Fatalf("lien : %d %s", rec.Code, rec.Body)
	}
	var l lienCast
	if err := json.Unmarshal(rec.Body.Bytes(), &l); err != nil {
		b.t.Fatal(err)
	}
	return l
}

// sessionDe lit l'identifiant de session d'un cookie (GET /compte).
func (b *bancCast) sessionDe(c *http.Cookie) string {
	b.t.Helper()
	rec := b.requete("GET", "/api/v1/compte", "", avec(c))
	var v struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil || v.Session.ID == "" {
		b.t.Fatalf("compte : %d %s (%v)", rec.Code, rec.Body, err)
	}
	return v.Session.ID
}

// prefixeDe : « /api/v1/cast/<jeton> » d'un lien signe.
func prefixeDe(t *testing.T, u string) string {
	t.Helper()
	i := strings.Index(u, "/live/")
	if i < 0 {
		i = strings.Index(u, "/vod/")
	}
	if !strings.HasPrefix(u, "/api/v1/cast/") || i < 0 {
		t.Fatalf("lien inattendu : %s", u)
	}
	return u[:i]
}

func lignesSegments(corps string) []string {
	var out []string
	for _, l := range strings.Split(corps, "\n") {
		if l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

// ---------------------------------------------------------------- CA1 : POST /cast/lien

// CA1 : le lien signe designe la ressource demandee, expire a comptes.cast_duree, recopie les
// parametres, et ne porte ni session en clair ni adresse du fournisseur.
func TestCA1_LienDeDiffusionSigne(t *testing.T) {
	b := nouveauBancCast(t, nil)
	c := b.connecte("alice", false)
	sess := b.sessionDe(c)

	l := b.lien(c, "/api/v1/live/100.m3u8", "cap=hevc,eac3&qualite=auto")
	p := prefixeDe(t, l.URL)
	if !strings.HasSuffix(l.URL, "/live/100.m3u8?cap=hevc,eac3&qualite=auto") {
		t.Errorf("lien : %s", l.URL)
	}
	if l.Expire != "2026-01-02T00:00:00Z" {
		t.Errorf("expiration = %s, 12 h apres la demande attendues", l.Expire)
	}
	jeton := strings.TrimPrefix(p, "/api/v1/cast/")
	if strings.Contains(jeton, sess) || strings.Contains(strings.ToLower(jeton), "http") || strings.Contains(jeton, "/") {
		t.Errorf("le jeton ne doit rien reveler : %s", jeton)
	}
	if v := b.lien(c, "/api/v1/vod/a:1.mkv", ""); !strings.HasSuffix(v.URL, "/vod/a:1.mkv") || strings.Contains(v.URL, "?") {
		t.Errorf("lien vod : %s", v.URL)
	}
	// Meme demande = meme jeton (charge identique, nonce derive) : les liens restent stables.
	if l2 := b.lien(c, "/api/v1/live/100.m3u8", ""); prefixeDe(t, l2.URL) != p {
		t.Errorf("deux demandes identiques donnent deux jetons : %s / %s", l2.URL, l.URL)
	}
	// Metrique et journal : sans le jeton.
	n := 0
	for _, m := range b.inst.Mesures() {
		if m.Nom == "cast_liens_total" {
			n++
			if m.Etiquettes["type"] != "live" && m.Etiquettes["type"] != "vod" {
				t.Errorf("etiquette type : %v", m.Etiquettes)
			}
		}
	}
	if n != 3 {
		t.Errorf("cast_liens_total : %d mesures, 3 attendues", n)
	}
	j := b.journal.String()
	if !strings.Contains(j, "lien de diffusion emis") || !strings.Contains(j, "ressource=live:100") || !strings.Contains(j, "session="+sess[:8]) {
		t.Errorf("journal : %s", j)
	}
	if strings.Contains(j, jeton) || strings.Contains(j, sess) {
		t.Errorf("le journal ne doit porter ni le jeton ni la session complete : %s", j)
	}
}

// CA1 : la duree vient de la configuration (comptes.cast_duree).
func TestCA1_DureeDuLienConfigurable(t *testing.T) {
	b := nouveauBancCast(t, func(o *Options) { o.CastDuree = 90 * time.Minute })
	c := b.connecte("alice", false)
	if l := b.lien(c, "/api/v1/live/100.m3u8", ""); l.Expire != "2026-01-01T13:30:00Z" {
		t.Errorf("expiration = %s, 90 min attendues", l.Expire)
	}
}

// CA1 : 400 si `lecture` n'est ni une chaine ni un contenu du schema, ou si les parametres
// portent autre chose que des reglages de lecture ; 401 sans session.
func TestCA1_LienRefuseLesRessourcesInconnues(t *testing.T) {
	b := nouveauBancCast(t, nil)
	c := b.connecte("alice", false)
	for _, corps := range []string{
		`{"lecture":"/api/v1/chaines"}`, `{"lecture":"/api/v1/live/etat"}`, `{"lecture":"/api/v1/live/100"}`,
		`{"lecture":"/api/v1/live/../100.m3u8"}`, `{"lecture":"/api/v1/vod/sans-fournisseur"}`,
		`{"lecture":"http://f.test/live/100.m3u8"}`, `{"lecture":"/api/v1/live/100.m3u8?cap=hevc"}`, `{"lecture":""}`,
		`{"lecture":"/api/v1/live/100.m3u8","parametres":"x=1"}`,
		`{"lecture":"/api/v1/live/100.m3u8","parametres":"cap=../x"}`,
		`{"lecture":"/api/v1/live/100.m3u8","parametres":"cap=a#b"}`,
		`{"lecture":"/api/v1/live/100.m3u8","autre":1}`,
	} {
		if rec := b.requete("POST", "/api/v1/cast/lien", corps, avec(c)); rec.Code != 400 {
			t.Errorf("%s : %d (400 attendu) %s", corps, rec.Code, rec.Body)
		}
	}
	if rec := b.requete("POST", "/api/v1/cast/lien", `{"lecture":"/api/v1/live/100.m3u8"}`); rec.Code != 401 {
		t.Errorf("sans session : %d", rec.Code)
	}
	// Sans secret : non disponible, et jamais une panique.
	sans := nouveauBanc(t, func(o *Options) { o.Secret = nil })
	cs := sans.connecte("bob", false)
	if rec := sans.requete("POST", "/api/v1/cast/lien", `{"lecture":"/api/v1/live/100.m3u8"}`, avec(cs)); rec.Code != 501 {
		t.Errorf("sans secret : %d", rec.Code)
	}
	if rec := sans.requete("GET", "/api/v1/cast/x/live/100.m3u8", ""); rec.Code != 501 {
		t.Errorf("sans secret, lecture : %d", rec.Code)
	}
}

// ---------------------------------------------------------------- CA2 : GET/HEAD /cast/<jeton>/...

// CA2 : un jeton expire est refuse (401 jeton_invalide), avant comme apres avoir servi.
func TestCA2_JetonExpireRefuse(t *testing.T) {
	b := nouveauBancCast(t, nil)
	c := b.connecte("alice", false)
	l := b.lien(c, "/api/v1/live/100.m3u8", "")
	if rec := b.requete("GET", l.URL, ""); rec.Code != 200 {
		t.Fatalf("avant expiration : %d %s", rec.Code, rec.Body)
	}
	b.decalage.Store(int64(12*time.Hour + time.Second))
	rec := b.requete("GET", l.URL, "")
	if rec.Code != 401 || rec.Header().Get("X-Mytv-Erreur") != "jeton_invalide" {
		t.Fatalf("apres expiration : %d %s", rec.Code, rec.Body)
	}
	// Un nouveau lien, demande apres, sert de nouveau.
	if l2 := b.lien(c, "/api/v1/live/100.m3u8", ""); b.requete("GET", l2.URL, "").Code != 200 {
		t.Error("un lien frais doit servir")
	}
}

// CA2 : le jeton ne sert que la ressource pour laquelle il a ete emis : une autre chaine, ses
// segments, un contenu, un jeton altere ou d'un autre domaine (segment) sont refuses.
func TestCA2_RessourceEtrangereRefusee(t *testing.T) {
	b := nouveauBancCast(t, nil)
	c := b.connecte("alice", false)
	p := prefixeDe(t, b.lien(c, "/api/v1/live/100.m3u8", "").URL)
	pv := prefixeDe(t, b.lien(c, "/api/v1/vod/a:1.mkv", "").URL)
	for _, cas := range []struct{ chemin, code string }{
		{p + "/live/200.m3u8", "jeton_invalide"},
		{p + "/live/200/seg/abc.ts", "jeton_invalide"},
		{p + "/vod/a:1.mkv", "jeton_invalide"},
		{pv + "/vod/a:2.mkv", "jeton_invalide"},
		{pv + "/live/100.m3u8", "jeton_invalide"},
		{p + "x/live/100.m3u8", "jeton_invalide"},                                        // altere
		{"/api/v1/cast/" + strings.Repeat("A", 40) + "/live/100.m3u8", "jeton_invalide"}, // forge
	} {
		rec := b.requete("GET", cas.chemin, "")
		if rec.Code != 401 || rec.Header().Get("X-Mytv-Erreur") != cas.code {
			t.Errorf("%s : %d %s (401 %s attendu)", cas.chemin, rec.Code, rec.Body, cas.code)
		}
	}
	// Un jeton de SEGMENT (autre domaine du meme secret) n'ouvre pas le domaine cast.
	rec := b.requete("GET", p+"/live/100.m3u8", "")
	seg := lignesSegments(rec.Body.String())[0]
	jetonSeg := seg[strings.LastIndex(seg, "/")+1:]
	if rec := b.requete("GET", "/api/v1/cast/"+strings.TrimSuffix(jetonSeg, ".ts")+"/live/100.m3u8", ""); rec.Code != 401 {
		t.Errorf("jeton de segment en guise de jeton cast : %d", rec.Code)
	}
	// Chemins hors schema sous un jeton valide : introuvable, jamais servi.
	for _, chemin := range []string{p + "/live/etat", p + "/live/100/seg/", p + "/live/100/seg/a/b", p + "/chaines", p + "/live/100.m3u8/x"} {
		if rec := b.requete("GET", chemin, ""); rec.Code != 404 {
			t.Errorf("%s : %d (404 attendu)", chemin, rec.Code)
		}
	}
	// La bonne ressource, elle, est servie.
	if rec := b.requete("GET", p+"/live/100.m3u8", ""); rec.Code != 200 {
		t.Errorf("ressource du jeton : %d", rec.Code)
	}
}

// CA2 : la session du jeton doit encore exister : apres la deconnexion, 401 (le lien ne
// survit pas a sa session).
func TestCA2_SessionRevoqueeRefusee(t *testing.T) {
	b := nouveauBancCast(t, nil)
	c := b.connecte("alice", false)
	l := b.lien(c, "/api/v1/live/100.m3u8", "")
	v := b.lien(c, "/api/v1/vod/a:1.mkv", "")
	if rec := b.requete("GET", l.URL, ""); rec.Code != 200 {
		t.Fatalf("avant : %d", rec.Code)
	}
	if rec := b.requete("POST", "/api/v1/deconnexion", "", avec(c)); rec.Code != 204 {
		t.Fatalf("deconnexion : %d", rec.Code)
	}
	for _, u := range []string{l.URL, v.URL} {
		rec := b.requete("GET", u, "")
		if rec.Code != 401 || rec.Header().Get("X-Mytv-Erreur") != "jeton_invalide" {
			t.Errorf("session revoquee : %d %s", rec.Code, rec.Body)
		}
	}
}

// CA2 : la requete signee est servie par les gestionnaires existants COMME la session du
// jeton : meme bail (le fournisseur voit la session d'alice, jamais le compte local ni une
// session nouvelle), VOD servie pour cette session, HEAD accepte, ecritures refusees (405).
func TestCA2_ServieCommeLaSessionDuJeton(t *testing.T) {
	b := nouveauBancCast(t, nil)
	c := b.connecte("alice", false)
	sess := b.sessionDe(c)
	l := b.lien(c, "/api/v1/live/100.m3u8", "")
	v := b.lien(c, "/api/v1/vod/a:1.mkv", "qualite=auto")

	rec := b.requete("GET", l.URL, "")
	if rec.Code != 200 {
		t.Fatalf("playlist : %d %s", rec.Code, rec.Body)
	}
	for _, s := range lignesSegments(rec.Body.String()) {
		if rec := b.requete("GET", s, ""); rec.Code != 200 {
			t.Fatalf("segment %s : %d", s, rec.Code)
		}
	}
	for _, s := range b.baux.vues() {
		if s != sess {
			t.Errorf("bail pris pour %q, la session d'alice %q attendue", s, sess)
		}
	}
	if len(b.baux.vues()) == 0 {
		t.Error("aucun bail pris")
	}
	if rec := b.requete("HEAD", l.URL, ""); rec.Code != 200 {
		t.Errorf("HEAD playlist : %d", rec.Code)
	}

	rec = b.requete("GET", v.URL, "")
	if rec.Code != 200 || rec.Body.String() != "film:a:1.mkv" {
		t.Fatalf("vod : %d %s", rec.Code, rec.Body)
	}
	if rec := b.requete("HEAD", v.URL, ""); rec.Code != 200 {
		t.Errorf("HEAD vod : %d", rec.Code)
	}
	b.lecture.mu.Lock()
	vues := append([]string(nil), b.lecture.sessions...)
	b.lecture.mu.Unlock()
	if len(vues) != 2 || vues[0] != sess || vues[1] != sess {
		t.Errorf("la VOD doit etre servie pour la session d'alice : %v", vues)
	}
	// Aucune session nouvelle : le compte d'alice n'a toujours qu'une session.
	if b.sessionDe(c) != sess {
		t.Error("la session a change")
	}
	for _, m := range []string{"POST", "PUT", "DELETE"} {
		rec := b.requete(m, l.URL, "")
		if rec.Code != 405 || rec.Header().Get("Allow") != "GET, HEAD" {
			t.Errorf("%s : %d Allow=%q (405 attendu)", m, rec.Code, rec.Header().Get("Allow"))
		}
	}
}

// ---------------------------------------------------------------- CA3 : le televiseur lit tout sous le prefixe

// CA3 : un faux televiseur (aucun cookie, aucun en-tete) lit la playlist signee PUIS ses
// segments avec 200 ; tout ce que la playlist annonce reste sous le prefixe signe ; le meme
// chemin sans jeton repond 401.
func TestCA3_UnTeleviseurSansCookieLitLaPlaylistPuisLesSegments(t *testing.T) {
	b := nouveauBancCast(t, nil)
	c := b.connecte("alice", false)
	l := b.lien(c, "/api/v1/live/100.m3u8", "cap=hevc,eac3,ac3")
	p := prefixeDe(t, l.URL)

	// Le lecteur de l'interface (cookie) a deja la playlist en cache sous la racine normale.
	if rec := b.requete("GET", "/api/v1/live/100.m3u8", "", avec(c)); rec.Code != 200 || !strings.Contains(rec.Body.String(), "/api/v1/live/100/seg/") {
		t.Fatalf("lecteur a cookie : %d %s", rec.Code, rec.Body)
	}

	rec := b.requete("GET", l.URL, "") // le televiseur : rien d'autre que le lien
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), "mpegurl") {
		t.Fatalf("playlist signee : %d %s", rec.Code, rec.Body)
	}
	seg := lignesSegments(rec.Body.String())
	if len(seg) != 2 {
		t.Fatalf("2 segments attendus :\n%s", rec.Body)
	}
	for _, s := range seg {
		if !strings.HasPrefix(s, p+"/live/100/seg/") {
			t.Fatalf("segment annonce hors du prefixe signe : %s", s)
		}
		rec := b.requete("GET", s, "")
		if rec.Code != 200 || rec.Body.String() != "DONNEES-TS!!" {
			t.Fatalf("segment %s : %d %s", s, rec.Code, rec.Body)
		}
		// Le meme segment sans le jeton : refuse.
		if rec := b.requete("GET", strings.TrimPrefix(s, p), ""); rec.Code != 401 {
			t.Errorf("segment sans jeton : %d", rec.Code)
		}
	}
	if strings.Contains(rec.Body.String(), "/api/v1/live/") {
		t.Errorf("une adresse hors prefixe subsiste :\n%s", rec.Body)
	}
	if rec := b.requete("GET", "/api/v1/live/100.m3u8", ""); rec.Code != 401 {
		t.Errorf("playlist sans jeton ni cookie : %d", rec.Code)
	}
	if b.segments.Load() != 2 {
		t.Errorf("le fournisseur doit avoir servi 2 segments, %d", b.segments.Load())
	}
}

// ---------------------------------------------------------------- CA4 : CORS des appareils de diffusion

// CA4 : sous /cast/<jeton>/ seulement, origine ouverte sans credentials, en-tetes exposes,
// 204 au preflight ; le reste de l'API (politique fermee) ne gagne aucun en-tete CORS.
func TestCA4_CORSOuvertSurLesLiensSignesSeulement(t *testing.T) {
	b := nouveauBancCast(t, nil)
	c := b.connecte("alice", false)
	l := b.lien(c, "/api/v1/live/100.m3u8", "")
	origine := func(r *http.Request) { r.Header.Set("Origin", "https://recepteur.example") }

	rec := b.requete("GET", l.URL, "", origine)
	h := rec.Header()
	if rec.Code != 200 || h.Get("Access-Control-Allow-Origin") != "*" || h.Get("Access-Control-Allow-Credentials") != "" {
		t.Fatalf("GET signe : %d %v", rec.Code, h)
	}
	for _, e := range []string{"X-Mytv-Mode", "X-Mytv-Erreur", "X-Mytv-Duree"} {
		if !strings.Contains(h.Get("Access-Control-Expose-Headers"), e) {
			t.Errorf("%s non expose : %q", e, h.Get("Access-Control-Expose-Headers"))
		}
	}
	if h.Get("Cross-Origin-Resource-Policy") != "cross-origin" {
		t.Errorf("CORP = %q", h.Get("Cross-Origin-Resource-Policy"))
	}
	// Un refus aussi porte les en-tetes : le recepteur lit X-Mytv-Erreur.
	if rec := b.requete("GET", prefixeDe(t, l.URL)+"/live/200.m3u8", "", origine); rec.Code != 401 || rec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("refus signe : %d %v", rec.Code, rec.Header())
	}
	rec = b.requete("OPTIONS", l.URL, "", origine, func(r *http.Request) { r.Header.Set("Access-Control-Request-Method", "GET") })
	h = rec.Header()
	if rec.Code != 204 || h.Get("Access-Control-Allow-Methods") != "GET, HEAD" || h.Get("Access-Control-Allow-Headers") != "Range" || h.Get("Access-Control-Allow-Origin") != "*" {
		t.Errorf("preflight : %d %v", rec.Code, h)
	}
	// Hors cast : rien (politique fermee par defaut), et la demande de lien reste une ecriture protegee.
	for _, cas := range []struct{ m, chemin string }{{"GET", "/api/v1/sante"}, {"GET", "/api/v1/live/100.m3u8"}, {"GET", "/api/v1/chaines"}} {
		rec := b.requete(cas.m, cas.chemin, "", avec(c), origine)
		if rec.Header().Get("Access-Control-Allow-Origin") != "" || rec.Header().Get("Access-Control-Expose-Headers") != "" {
			t.Errorf("%s %s : en-tete CORS hors cast : %v", cas.m, cas.chemin, rec.Header())
		}
	}
	if rec := b.requete("OPTIONS", "/api/v1/chaines", "", origine, func(r *http.Request) { r.Header.Set("Access-Control-Request-Method", "GET") }); rec.Code != 403 {
		t.Errorf("preflight hors cast : %d (403 attendu, politique fermee)", rec.Code)
	}
	rec = b.requete("POST", "/api/v1/cast/lien", `{"lecture":"/api/v1/live/100.m3u8"}`, avec(c), origine)
	if rec.Code != 403 || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("demande de lien depuis une origine etrangere : %d %v (403 sans CORS attendu)", rec.Code, rec.Header())
	}
}

// ---------------------------------------------------------------- CA5 : contrat et exploitation

// CA5 : la route signee est declaree de niveau Lecture, sans cookie ; la demande de lien de
// niveau Session (la coherence avec le contrat est verifiee par TestContrat*).
func TestCA5_RoutesCastDeclarees(t *testing.T) {
	b := nouveauBanc(t, nil)
	vues := map[string]Acces{}
	for _, r := range b.api.Routes() {
		vues[r.Methode+" "+r.Chemin] = r.Acces
	}
	if a, ok := vues["GET /cast/{jeton}/{chemin}"]; !ok || a != Lecture {
		t.Errorf("GET /cast/{jeton}/{chemin} : %v %v (Lecture attendu)", a, ok)
	}
	if a, ok := vues["POST /cast/lien"]; !ok || a != Session {
		t.Errorf("POST /cast/lien : %v %v (Session attendu)", a, ok)
	}
	// Un jeton quelconque n'est jamais un cookie : sans lien valide, 401 (refus par defaut).
	if rec := b.requete("GET", "/api/v1/cast/x/live/1.m3u8", ""); rec.Code != 401 {
		t.Errorf("jeton bidon : %d", rec.Code)
	}
	if rec := b.requete("GET", "/api/v1/cast/x/live/1.m3u8", "", avec(b.connecte("alice", false))); rec.Code != 401 {
		t.Errorf("jeton bidon avec cookie : %d (le cookie ne remplace pas le jeton)", rec.Code)
	}
}
