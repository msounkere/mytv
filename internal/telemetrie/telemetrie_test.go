package telemetrie

import (
	"bytes"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"mytv/internal/config"
	"mytv/internal/domain"
)

var _ domain.Instrumentation = instrumentation{} // CA5

type horloge struct {
	mu sync.Mutex
	t  time.Time
}

func (h *horloge) now() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.t }
func (h *horloge) avance(d time.Duration) {
	h.mu.Lock()
	h.t = h.t.Add(d)
	h.mu.Unlock()
}

// Authentification de test : l'en-tete X-Session donne la session, X-Admin l'administrateur.
func auth(r *http.Request) (string, bool) {
	s := r.Header.Get("X-Session")
	return s, s != ""
}
func estAdmin(r *http.Request) bool { return r.Header.Get("X-Admin") == "1" }

type banc struct {
	t   *testing.T
	tel *Telemetrie
	h   *horloge
	cfg config.Diagnostic
	rt  http.Handler
}

func nouveauBanc(t *testing.T, ajuste func(*config.Diagnostic)) *banc {
	t.Helper()
	c := config.Defauts().Diagnostic
	c.Dossier = filepath.Join(t.TempDir(), "diag")
	if ajuste != nil {
		ajuste(&c)
	}
	h := &horloge{t: time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)}
	tel, err := Nouveau(Options{Config: c, Horloge: h.now, Authentifier: auth, EstAdmin: estAdmin})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tel.Fermer() })
	return &banc{t: t, tel: tel, h: h, cfg: c, rt: tel.Routes()}
}

func (b *banc) requete(methode, chemin, corps string, entetes ...string) *httptest.ResponseRecorder {
	b.t.Helper()
	req := httptest.NewRequest(methode, chemin, strings.NewReader(corps))
	for i := 0; i+1 < len(entetes); i += 2 {
		req.Header.Set(entetes[i], entetes[i+1])
	}
	rec := httptest.NewRecorder()
	b.rt.ServeHTTP(rec, req)
	return rec
}

func (b *banc) envoyer(session string, corps string) *httptest.ResponseRecorder {
	return b.requete("POST", CheminEvenements, corps, "X-Session", session, "Content-Type", "application/json")
}

func (b *banc) admin(chemin string) *httptest.ResponseRecorder {
	return b.requete("GET", chemin, "", "X-Session", "adm", "X-Admin", "1")
}

func (b *banc) evenements(filtre string) []Evenement {
	b.t.Helper()
	rec := b.admin(CheminEvenements + filtre)
	if rec.Code != 200 {
		b.t.Fatalf("lecture : %d %s", rec.Code, rec.Body)
	}
	var r struct{ Elements []Evenement }
	if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
		b.t.Fatal(err)
	}
	return r.Elements
}

func lot(n int, typ string) string {
	evs := make([]string, n)
	for i := range evs {
		evs[i] = fmt.Sprintf(`{"type":%q,"i":%d}`, typ, i)
	}
	return `{"appareil":"tv-salon","agent":"Mozilla/5.0","evenements":[` + strings.Join(evs, ",") + `]}`
}

// ---------------------------------------------------------------- CA1 : Prometheus

func TestMetriquesAuFormatPrometheus(t *testing.T) {
	b := nouveauBanc(t, nil)
	in := b.tel.Instrumentation()
	in.Compter("relais_requetes_total", 1, "statut", "200")
	in.Compter("relais_requetes_total", 2, "statut", "200")
	in.Compter("relais_requetes_total", 1, "statut", "502")
	in.Compter("bail_attente_total", 3)
	in.Observer("relais_segment_secondes", 0.3, "type", "ts")
	in.Observer("relais_segment_secondes", 0.01, "type", "ts")
	in.Observer("relais_segment_secondes", 1000, "type", "ts")
	rec := b.admin(CheminMetriques)
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
	corps := rec.Body.String()
	for _, attendu := range []string{
		"# TYPE relais_requetes_total counter\n",
		`relais_requetes_total{statut="200"} 3` + "\n",
		`relais_requetes_total{statut="502"} 1` + "\n",
		"bail_attente_total 3\n",
		"# TYPE relais_segment_secondes histogram\n",
		`relais_segment_secondes_bucket{type="ts",le="0.01"} 1` + "\n",
		`relais_segment_secondes_bucket{type="ts",le="0.5"} 2` + "\n",
		`relais_segment_secondes_bucket{type="ts",le="120"} 2` + "\n",
		`relais_segment_secondes_bucket{type="ts",le="+Inf"} 3` + "\n",
		`relais_segment_secondes_count{type="ts"} 3` + "\n",
		`relais_segment_secondes_sum{type="ts"} 1000.31` + "\n",
	} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("manque %q dans\n%s", attendu, corps)
		}
	}
	// Deterministe.
	if b.admin(CheminMetriques).Body.String() != corps {
		t.Error("sortie non deterministe")
	}
}

// CA1 : la route est protegee (refus par defaut).
func TestRoutesProtegees(t *testing.T) {
	b := nouveauBanc(t, nil)
	for _, chemin := range []string{CheminMetriques, CheminEvenements} {
		if rec := b.requete("GET", chemin, ""); rec.Code != 401 {
			t.Errorf("%s anonyme : %d", chemin, rec.Code)
		}
		if rec := b.requete("GET", chemin, "", "X-Session", "s1"); rec.Code != 403 {
			t.Errorf("%s utilisateur ordinaire : %d", chemin, rec.Code)
		}
		if rec := b.requete("GET", chemin, "", "X-Admin", "1"); rec.Code != 401 {
			t.Errorf("%s administrateur sans session : %d", chemin, rec.Code)
		}
	}
	if rec := b.requete("POST", CheminEvenements, lot(1, "x"), "Content-Type", "application/json"); rec.Code != 401 {
		t.Errorf("envoi anonyme : %d", rec.Code)
	}
	if rec := b.requete("DELETE", CheminEvenements, "", "X-Session", "adm", "X-Admin", "1"); rec.Code != 405 {
		t.Errorf("DELETE : %d", rec.Code)
	}
	// Sans authentificateur du tout : refus.
	tel, _ := Nouveau(Options{Config: b.cfg})
	rec := httptest.NewRecorder()
	tel.Routes().ServeHTTP(rec, httptest.NewRequest("GET", CheminMetriques, nil))
	if rec.Code != 403 {
		t.Errorf("sans authentificateur : %d", rec.Code)
	}
	tel.Fermer()
}

// ---------------------------------------------------------------- CA2 : cardinalite

func TestCardinaliteBornee(t *testing.T) {
	b := nouveauBanc(t, func(c *config.Diagnostic) { c.SeriesMax = 10 })
	in := b.tel.Instrumentation()
	for i := 0; i < 5000; i++ {
		in.Compter("relais_requetes_total", 1, "statut", strconv.Itoa(i))
		in.Observer("relais_segment_secondes", 0.1, "type", "t"+strconv.Itoa(i))
	}
	if n := b.tel.reg.nombreSeries("relais_requetes_total"); n > 11 {
		t.Errorf("%d series (borne 10 + « autre »)", n)
	}
	if n := b.tel.reg.nombreSeries("relais_segment_secondes"); n > 11 {
		t.Errorf("histogramme : %d series", n)
	}
	corps := b.admin(CheminMetriques).Body.String()
	if !strings.Contains(corps, `relais_requetes_total{statut="autre"} 4990`) {
		t.Errorf("le debordement doit etre agrege dans « autre » :\n%s", corps)
	}
	if strings.Count(corps, "\n") > 400 {
		t.Errorf("sortie trop grosse : %d lignes", strings.Count(corps, "\n"))
	}
}

// CA2 : jamais d'identifiant de session, d'URL, d'utilisateur ou d'adresse en etiquette.
func TestEtiquettesInterdites(t *testing.T) {
	b := nouveauBanc(t, nil)
	in := b.tel.Instrumentation()
	in.Compter("vod_lectures_total", 1,
		"session", "SESSION-SECRETE", "url", "http://f.test/movie/u/p/1.mkv", "user", "alice", "mode", "direct",
		"ip", "10.0.0.1", "jeton", "abc", "appareil", "tv")
	in.Compter("a_impaire", 1, "mode") // nombre impair : etiquettes ignorees, compteur compte tel quel
	in.Compter("Mauvais Nom", 1)       // nom invalide
	in.Compter("neg", -5)              // un compteur ne decroit pas
	in.Compter("v", 1, "mode", "http://f.test/x?password=secret et un texte tres long qui depasse la borne de longueur")
	corps := b.admin(CheminMetriques).Body.String()
	for _, interdit := range []string{"SESSION-SECRETE", "alice", "10.0.0.1", "secret", "jeton", "Mauvais", "neg"} {
		if strings.Contains(corps, interdit) {
			t.Errorf("%q ne doit pas apparaitre :\n%s", interdit, corps)
		}
	}
	if !strings.Contains(corps, `vod_lectures_total{mode="direct"} 1`) {
		t.Errorf("seule l'etiquette permise doit rester :\n%s", corps)
	}
	for _, l := range strings.Split(corps, "\n") {
		if strings.HasPrefix(l, "v{") && len(l) > 80 {
			t.Errorf("valeur d'etiquette non bornee : %q", l)
		}
	}
}

func TestNomsDeMetriquesBornes(t *testing.T) {
	b := nouveauBanc(t, nil)
	in := b.tel.Instrumentation()
	for i := 0; i < 2000; i++ {
		in.Compter("m"+strconv.Itoa(i), 1)
	}
	if n := len(b.tel.reg.familles); n > 500 {
		t.Errorf("%d metriques", n)
	}
	// Meme nom, deux types : le second est ignore, pas de panique.
	in.Compter("duo", 1)
	in.Observer("duo", 1)
}

// ---------------------------------------------------------------- CA3 : diagnostic

func TestEvenementsDuClientEtDuServeur(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.tel.Instrumentation().Evenement("vod:decision", "mkv remuxe")
	b.h.avance(time.Second)
	rec := b.envoyer("s1", `{"appareil":"tv-salon","agent":"Mozilla/5.0 (X11)","evenements":[
		{"type":"echantillon","tampon":4.2,"niveau":3,"ok":true,"vide":null},
		{"type":"hls:erreur","message":"manifestLoadError","fatal":false,"details":{"code":404,"tentatives":[1,2,3]}},
		{"type":"video:stalled","t":123.5},
		{"type":"image:retour","n":1},
		{"type":"MAUVAIS TYPE"}, {"pas":"de type"}, "pas un objet", 12]}`)
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"ignores":4,"recus":4}` {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	evs := b.evenements("")
	if len(evs) != 5 || evs[0].Type != "image:retour" || evs[4].Type != "vod:decision" {
		t.Fatalf("evenements = %+v (du plus recent au plus ancien)", evs)
	}
	if evs[4].Source != "serveur" || evs[3].Source != "client" || evs[1].Type != "video:stalled" {
		t.Errorf("sources/types : %+v", evs)
	}
	if got := b.evenements("?type=hls"); len(got) != 1 || got[0].Message != "manifestLoadError" || got[0].Donnees["fatal"] != false {
		t.Errorf("filtre par prefixe : %+v", got)
	}
	if got := b.evenements("?limite=2"); len(got) != 2 {
		t.Errorf("limite : %d", len(got))
	}
}

// CA3 : aucune donnee sensible n'est stockee, NI dans la reponse NI sur le disque.
func TestAucuneDonneeSensible(t *testing.T) {
	b := nouveauBanc(t, nil)
	corps := `{"appareil":"APPAREIL-UNIQUE-42","agent":"Mozilla/5.0 jeton=AGENTSECRET","evenements":[
	 {"type":"hls:erreur","message":"echec http://alice:MOTDEPASSE@fourn.test/live/alice/MOTDEPASSE/12.m3u8?token=JETONQUERY&password=PWQUERY",
	  "url":"https://mytv.test/api/v1/live/seg/abcdefghijklmnopqrstuvwxyzABCDEF0123456789.ts?sig=SIGNATURE",
	  "profond":{"a":{"b":{"c":{"d":"trop profond"}}}},
	  "entete":"Authorization: Bearer BEARERSECRET","cookie":"session=COOKIESECRET",
	  "chemin":"/movie/alice/MOTDEPASSE/55.mkv","aussi":"password=PWTEXTE"}]}`
	if rec := b.envoyer("SESSION-ID-777", corps); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	b.tel.Instrumentation().Evenement("vod:erreur", "sonde http://bob:SERVEURPW@h.test/movie/bob/SERVEURPW/1.mkv?x=1 echouee")
	b.tel.Fermer()
	tout := b.admin(CheminEvenements).Body.String()
	var disque []byte
	ents, _ := os.ReadDir(b.cfg.Dossier)
	for _, e := range ents {
		raw, _ := os.ReadFile(filepath.Join(b.cfg.Dossier, e.Name()))
		disque = append(disque, raw...)
	}
	if len(disque) == 0 {
		t.Fatal("rien sur le disque")
	}
	for _, source := range []string{tout, string(disque)} {
		for _, secret := range []string{"MOTDEPASSE", "alice", "JETONQUERY", "PWQUERY", "SIGNATURE", "abcdefghijklmnopqrstuvwxyz",
			"BEARERSECRET", "COOKIESECRET", "PWTEXTE", "AGENTSECRET", "SERVEURPW", "SESSION-ID-777", "APPAREIL-UNIQUE-42", "trop profond"} {
			if strings.Contains(source, secret) {
				t.Errorf("%q fuit :\n%s", secret, source)
			}
		}
	}
}

// CA3 : conservation — les evenements trop vieux ne sont ni lus ni gardes.
func TestConservationLecturePurgeEtRotation(t *testing.T) {
	b := nouveauBanc(t, func(c *config.Diagnostic) { c.Conservation = config.Duree(4 * time.Hour) })
	in := b.tel.Instrumentation()
	in.Evenement("a:ancien", "t0")
	b.h.avance(time.Hour)
	in.Evenement("a:un_peu_moins_ancien", "t1")
	b.h.avance(3*time.Hour + time.Minute) // t0 a 4 h 1 min : perime
	if got := b.evenements(""); len(got) != 1 || got[0].Type != "a:un_peu_moins_ancien" {
		t.Fatalf("lecture : %+v", got)
	}
	// Un segment dure au plus Conservation/2 : ecrire apres 2 h ouvre un nouveau
	// segment, et la purge supprime ceux dont la fin est perimee.
	in.Evenement("a:recent", "t2")
	b.h.avance(5 * time.Hour)
	in.Evenement("a:tres_recent", "t3")
	segs := b.tel.jour.segments()
	for _, s := range segs {
		if !s.ouvert && s.fin.Before(b.h.now().Add(-4*time.Hour)) {
			t.Errorf("segment perime conserve : %+v", s)
		}
	}
	if got := b.evenements(""); len(got) != 1 || got[0].Type != "a:tres_recent" {
		t.Errorf("apres 5 h : %+v", got)
	}
}

// CA3 : taille totale bornee (rotation par taille, suppression des plus anciens).
func TestTailleTotaleBornee(t *testing.T) {
	b := nouveauBanc(t, func(c *config.Diagnostic) { c.SegmentMax = 2000; c.TailleMax = 8000 })
	in := b.tel.Instrumentation()
	for i := 0; i < 500; i++ {
		b.h.avance(time.Second)
		in.Evenement("a:bruit", strings.Repeat("x", 100)+strconv.Itoa(i))
		if o := b.tel.jour.octets(); o > 8000+2000 {
			t.Fatalf("%d octets sur disque apres %d evenements (borne 8000 + un segment)", o, i)
		}
	}
	ents, _ := os.ReadDir(b.cfg.Dossier)
	if len(ents) < 2 {
		t.Errorf("pas de rotation : %d fichiers", len(ents))
	}
	evs := b.evenements("?limite=1000")
	if len(evs) == 0 || !strings.HasSuffix(evs[0].Message, "499") {
		t.Errorf("le plus recent doit etre lu en premier : %+v", evs[:1])
	}
}

// Un segment laisse ouvert par un arret brutal est ferme et reste lisible.
func TestRepriseApresArretBrutal(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.tel.Instrumentation().Evenement("a:avant", "x")
	// Pas de Fermer : on rouvre le meme dossier (arret brutal).
	tel2, err := Nouveau(Options{Config: b.cfg, Horloge: b.h.now, Authentifier: auth, EstAdmin: estAdmin})
	if err != nil {
		t.Fatal(err)
	}
	defer tel2.Fermer()
	tel2.Instrumentation().Evenement("a:apres", "y")
	evs, _ := tel2.jour.lire(Filtre{})
	if len(evs) != 2 {
		t.Errorf("evenements = %+v", evs)
	}
	// Ligne abimee ignoree.
	ents, _ := os.ReadDir(b.cfg.Dossier)
	f, _ := os.OpenFile(filepath.Join(b.cfg.Dossier, ents[0].Name()), os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("{pas du json\n")
	f.Close()
	if evs, _ := tel2.jour.lire(Filtre{}); len(evs) != 2 {
		t.Errorf("ligne abimee : %d", len(evs))
	}
}

func TestReglagesInvalides(t *testing.T) {
	c := config.Defauts().Diagnostic
	c.Dossier = t.TempDir()
	c.TailleMax = c.SegmentMax - 1
	if _, err := Nouveau(Options{Config: c}); err == nil {
		t.Error("taille_max < segment_max accepte")
	}
	c = config.Defauts().Diagnostic
	c.Dossier = t.TempDir()
	c.DebitEvenements = 0
	if _, err := Nouveau(Options{Config: c}); err == nil {
		t.Error("debit 0 accepte")
	}
}

// ---------------------------------------------------------------- limites de reception

func TestLimiteDeTailleDeLEnvoi(t *testing.T) {
	b := nouveauBanc(t, func(c *config.Diagnostic) { c.EnvoiMax = 2000 })
	gros := lot(500, "echantillon")
	if rec := b.envoyer("s1", gros); rec.Code != 413 {
		t.Errorf("Content-Length trop grand : %d", rec.Code)
	}
	// Sans Content-Length (flux) : borne aussi.
	req := httptest.NewRequest("POST", CheminEvenements, io.NopCloser(strings.NewReader(gros)))
	req.ContentLength = -1
	req.Header.Set("X-Session", "s1")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	b.rt.ServeHTTP(rec, req)
	if rec.Code != 413 {
		t.Errorf("corps sans longueur : %d", rec.Code)
	}
	if b.evenements("") != nil && len(b.evenements("")) != 0 {
		t.Error("evenements stockes malgre le refus")
	}
}

func TestNombreDEvenementsParEnvoi(t *testing.T) {
	b := nouveauBanc(t, func(c *config.Diagnostic) { c.EvenementsParEnvoi = 5 })
	rec := b.envoyer("s1", lot(20, "echantillon"))
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"ignores":15,"recus":5}` {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

func TestEntreesInvalides(t *testing.T) {
	b := nouveauBanc(t, nil)
	cas := []struct {
		nom, corps, ct string
		code           int
	}{
		{"json invalide", `{pas du json`, "application/json", 400},
		{"sans evenements", `{"appareil":"x"}`, "application/json", 400},
		{"evenements pas une liste", `{"evenements":{"a":1}}`, "application/json", 400},
		{"mauvais type de contenu", lot(1, "echantillon"), "text/plain", 415},
		{"formulaire (CSRF)", lot(1, "echantillon"), "application/x-www-form-urlencoded", 415},
		{"sans type de contenu", lot(1, "echantillon"), "", 415},
	}
	for _, c := range cas {
		rec := b.requete("POST", CheminEvenements, c.corps, "X-Session", "s1", "Content-Type", c.ct)
		if rec.Code != c.code {
			t.Errorf("%s : %d, attendu %d", c.nom, rec.Code, c.code)
		}
	}
	if rec := b.envoyer("s1", `{"evenements":[]}`); rec.Code != 200 {
		t.Errorf("liste vide : %d", rec.Code)
	}
}

// Debit par session : seau a jetons, 429 + Retry-After, recharge avec le temps,
// sessions independantes.
func TestDebitParAppareil(t *testing.T) {
	b := nouveauBanc(t, func(c *config.Diagnostic) { c.DebitEvenements = 60; c.EvenementsParEnvoi = 100 })
	// Le seau contient 60 jetons : 50 passent, puis 10, puis plus rien.
	if rec := b.envoyer("s1", lot(50, "echantillon")); !strings.Contains(rec.Body.String(), `"recus":50`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	rec := b.envoyer("s1", lot(30, "echantillon"))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"recus":10`) {
		t.Fatalf("envoi partiel : %d %s", rec.Code, rec.Body)
	}
	rec = b.envoyer("s1", lot(1, "echantillon"))
	if rec.Code != 429 || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("saturation : %d %v", rec.Code, rec.Header())
	}
	// Une autre session n'est pas gênée.
	if rec := b.envoyer("s2", lot(20, "echantillon")); rec.Code != 200 {
		t.Errorf("autre session : %d", rec.Code)
	}
	// 1 jeton par seconde.
	b.h.avance(5 * time.Second)
	if rec := b.envoyer("s1", lot(10, "echantillon")); !strings.Contains(rec.Body.String(), `"recus":5`) {
		t.Errorf("recharge : %s", rec.Body)
	}
	// Le seau ne depasse jamais sa capacite.
	b.h.avance(time.Hour)
	if rec := b.envoyer("s1", lot(100, "echantillon")); !strings.Contains(rec.Body.String(), `"recus":60`) {
		t.Errorf("capacite : %s", rec.Body)
	}
}

func TestLimiteurBorneEnMemoire(t *testing.T) {
	b := nouveauBanc(t, func(c *config.Diagnostic) { c.AppareilsMax = 50 })
	for i := 0; i < 500; i++ {
		b.h.avance(time.Millisecond)
		b.envoyer("s"+strconv.Itoa(i), lot(1, "echantillon"))
	}
	if n := len(b.tel.limiteur.seaux); n > 50 {
		t.Errorf("%d sessions suivies (borne 50)", n)
	}
}

func TestMasquerTable(t *testing.T) {
	cas := map[string]string{
		"http://u:p@h.test/x":                                  "http://***@h.test/x",
		"http://h.test/live/alice/pw/1.m3u8":                   "http://h.test/live/***/***/1.m3u8",
		"/movie/alice/pw/5.mkv?token=abc":                      "/movie/***/***/5.mkv?…",
		"Authorization: Bearer abc.def-ghi":                    "Authorization=*** ***",
		"password=hunter2 et jeton: xyz":                       "password=*** et jeton=***",
		"/api/v1/live/abcdefghijklmnopqrstuvwxyz0123456789.ts": "/api/v1/live/…",
		"texte normal":                                         "texte normal",
	}
	for in, want := range cas {
		if got := Masquer(in); got != want {
			t.Errorf("Masquer(%q) = %q, attendu %q", in, got, want)
		}
	}
	if got := tronquer("ééééé", 5); got != "éé" {
		t.Errorf("tronquer coupe un caractere : %q", got)
	}
}

// ---------------------------------------------------------------- CA4 : cout nul

func TestInactifCoutNul(t *testing.T) {
	c := config.Defauts().Diagnostic
	c.Actif = false
	c.Dossier = filepath.Join(t.TempDir(), "jamais-cree")
	tel, err := Nouveau(Options{Config: c, Authentifier: auth, EstAdmin: estAdmin})
	if err != nil {
		t.Fatal(err)
	}
	if tel.Actif() {
		t.Fatal("actif")
	}
	in := tel.Instrumentation()
	if _, ok := in.(domain.SansInstrumentation); !ok {
		t.Fatalf("instrumentation = %T, attendu SansInstrumentation", in)
	}
	// Aucune allocation du cote du diagnostic. (Une liste d'etiquettes variadique
	// passee a travers une interface alloue chez l'APPELANT, c'est Go : sans etiquette,
	// l'appel est strictement gratuit.)
	if n := testing.AllocsPerRun(1000, func() {
		in.Compter("relais_requetes_total", 1)
		in.Observer("relais_segment_secondes", 0.2)
		in.Evenement("a:b", "message")
	}); n != 0 {
		t.Errorf("%v allocations par appel inactif", n)
	}
	if _, err := os.Stat(c.Dossier); err == nil {
		t.Error("dossier cree alors que le diagnostic est inactif")
	}
	rt := tel.Routes()
	for _, r := range []*http.Request{
		httptest.NewRequest("GET", CheminMetriques, nil), httptest.NewRequest("GET", CheminEvenements, nil),
	} {
		r.Header.Set("X-Session", "adm")
		r.Header.Set("X-Admin", "1")
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, r)
		if rec.Code != 501 {
			t.Errorf("%s inactif : %d", r.URL.Path, rec.Code)
		}
	}
	post := httptest.NewRequest("POST", CheminEvenements, strings.NewReader(lot(1, "x")))
	post.Header.Set("X-Session", "s1")
	post.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	rt.ServeHTTP(rec, post)
	if rec.Code != 501 {
		t.Errorf("envoi inactif : %d", rec.Code)
	}
	if err := tel.Fermer(); err != nil {
		t.Error(err)
	}
}

// CA4 (benchmark) : go test -bench Instrumentation -benchmem
func BenchmarkInstrumentationInactive(b *testing.B) {
	c := config.Defauts().Diagnostic
	c.Actif = false
	tel, _ := Nouveau(Options{Config: c})
	in := tel.Instrumentation()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		in.Compter("relais_requetes_total", 1)
		in.Observer("relais_segment_secondes", 0.2)
	}
}

func BenchmarkInstrumentationActive(b *testing.B) {
	c := config.Defauts().Diagnostic
	c.Dossier = b.TempDir()
	tel, err := Nouveau(Options{Config: c})
	if err != nil {
		b.Fatal(err)
	}
	defer tel.Fermer()
	in := tel.Instrumentation()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		in.Compter("relais_requetes_total", 1, "statut", "200")
		in.Observer("relais_segment_secondes", 0.2, "type", "ts")
	}
}

// ---------------------------------------------------------------- CA5 : instrumentation par interface

// CA5 : aucun autre paquet n'importe telemetrie (ils dependent de domain.Instrumentation).
func TestPersonneNImporteTelemetrie(t *testing.T) {
	racine := filepath.Join("..", "..")
	var fautifs []string
	filepath.Walk(racine, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(p, ".go") {
			return nil
		}
		if strings.Contains(filepath.ToSlash(p), "internal/telemetrie/") || strings.Contains(filepath.ToSlash(p), "cmd/") ||
			strings.Contains(filepath.ToSlash(p), "internal/appli/") {
			return nil // l'assemblage (cmd, internal/appli) a le droit de brancher
		}
		f, err := parser.ParseFile(token.NewFileSet(), p, nil, parser.ImportsOnly)
		if err != nil {
			return nil
		}
		for _, im := range f.Imports {
			if strings.Contains(im.Path.Value, "internal/telemetrie") {
				fautifs = append(fautifs, p)
			}
		}
		return nil
	})
	if len(fautifs) > 0 {
		t.Errorf("importent telemetrie : %v", fautifs)
	}
}

// ---------------------------------------------------------------- concurrence

func TestAccesConcurrents(t *testing.T) {
	b := nouveauBanc(t, func(c *config.Diagnostic) { c.SegmentMax = 5000; c.TailleMax = 20000; c.DebitEvenements = 100000 })
	in := b.tel.Instrumentation()
	var wg sync.WaitGroup
	avant := runtime.NumGoroutine()
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				in.Compter("c_total", 1, "g", strconv.Itoa(g%3))
				in.Observer("h_secondes", float64(i)/10, "g", strconv.Itoa(g%3))
				in.Evenement("a:b", "m"+strconv.Itoa(i))
				b.envoyer("s"+strconv.Itoa(g), lot(2, "echantillon"))
				if i%20 == 0 {
					b.admin(CheminMetriques)
					b.admin(CheminEvenements)
				}
			}
		}(g)
	}
	wg.Wait()
	if !bytes.Contains(b.admin(CheminMetriques).Body.Bytes(), []byte(`c_total{g="0"}`)) {
		t.Error("compteur absent")
	}
	// Les goroutines du serveur de test se terminent juste apres la reponse : on leur laisse
	// jusqu'a 2 s avant de conclure a une fuite (comparaison immediate = test instable sous
	// charge, constate plusieurs fois). Une vraie fuite depasse toujours ce delai.
	delai := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > avant && time.Now().Before(delai) {
		time.Sleep(20 * time.Millisecond)
	}
	if runtime.NumGoroutine() > avant {
		t.Errorf("goroutines : %d -> %d", avant, runtime.NumGoroutine())
	}
}

// Les erreurs suivent le format normalise de l'API : {code, message} avec des codes stables.
func TestCodesDErreurStables(t *testing.T) {
	b := nouveauBanc(t, func(c *config.Diagnostic) { c.EnvoiMax = 300; c.DebitEvenements = 1 })
	code := func(rec *httptest.ResponseRecorder) string {
		var e struct{ Code, Message string }
		if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Message == "" {
			t.Fatalf("corps d'erreur : %s", rec.Body)
		}
		return e.Code
	}
	cas := []struct {
		rec  *httptest.ResponseRecorder
		want string
	}{
		{b.requete("GET", CheminMetriques, ""), "non_authentifie"},
		{b.requete("GET", CheminMetriques, "", "X-Session", "s"), "interdit"},
		{b.requete("POST", CheminEvenements, "{}", "X-Session", "s", "Content-Type", "text/plain"), "format_non_json"},
		{b.requete("POST", CheminEvenements, "{pas", "X-Session", "s", "Content-Type", "application/json"), "requete_invalide"},
		{b.envoyer("s", lot(100, "echantillon")), "corps_trop_gros"},
	}
	for _, c := range cas {
		if got := code(c.rec); got != c.want {
			t.Errorf("code %q, attendu %q (statut %d)", got, c.want, c.rec.Code)
		}
	}
	b.envoyer("t", lot(1, "echantillon"))
	if rec := b.envoyer("t", lot(1, "echantillon")); rec.Code != 429 || code(rec) != "trop_de_requetes" {
		t.Errorf("429 : %d %s", rec.Code, rec.Body)
	}
}

// US-25 CA2 : une jauge monte ET descend (le temps restant avant l'expiration), sortie typee gauge,
// valeur negative acceptee (abonnement deja expire).
func TestJaugeSeMetAJourEtPeutEtreNegative(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.tel.Jauge("fournisseur_expire_dans_secondes", 86400, "fournisseur", "iptva")
	b.tel.Jauge("fournisseur_expire_dans_secondes", -30, "fournisseur", "iptva")
	corps := b.admin(CheminMetriques).Body.String()
	for _, attendu := range []string{"# TYPE fournisseur_expire_dans_secondes gauge\n", `fournisseur_expire_dans_secondes{fournisseur="iptva"} -30` + "\n"} {
		if !strings.Contains(corps, attendu) {
			t.Errorf("manque %q dans\n%s", attendu, corps)
		}
	}
	if strings.Contains(corps, "} 86400") {
		t.Error("la jauge doit remplacer la valeur, pas s'y ajouter")
	}
	// Diagnostic inactif : sans effet, sans panique.
	inactif := nouveauBanc(t, func(c *config.Diagnostic) { c.Actif = false })
	inactif.tel.Jauge("x_total", 1)
}
