package api

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mytv/internal/config"
	"mytv/internal/domain"
	"mytv/internal/hotes"
	"mytv/internal/images"
	"mytv/internal/stockage"
)

// fauxImages : serveur d'images en boucle locale ; compte les requetes recues.
type fauxImages struct {
	srv      *httptest.Server
	requetes atomic.Int32
	reponse  atomic.Pointer[http.HandlerFunc]
}

var png = []byte("\x89PNG\r\n\x1a\n-pixel-")

func nouveauFauxImages(t *testing.T) *fauxImages {
	f := &fauxImages{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requetes.Add(1)
		if h := f.reponse.Load(); h != nil {
			(*h)(w, r)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(png)
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// serviceImages : le VRAI service d'images, 127.0.0.1 autorise par la liste d'hotes de test.
func serviceImages(t *testing.T, f *fauxImages, mod func(*config.Images)) *images.Service {
	t.Helper()
	c := config.Defauts().Images
	c.CacheDossier = filepath.Join(t.TempDir(), "img")
	c.TailleMax = 1 << 10
	if mod != nil {
		mod(&c)
	}
	filtre, err := hotes.Nouveau("http://fournisseur.test", []string{"127.0.0.1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := images.Nouveau(images.Options{Config: c, Secret: []byte("0123456789abcdef0123456789abcdef-secret"),
		Fournisseurs: []images.Origine{{ID: "a", Base: f.srv.URL}}, Filtre: filtre})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func bancImages(t *testing.T) (*banc, *fauxImages, *http.Cookie) {
	t.Helper()
	f := nouveauFauxImages(t)
	svc := serviceImages(t, f, nil)
	b := nouveauBanc(t, func(o *Options) { o.Images = svc })
	b.st.chaines = []domain.Chaine{
		{ID: "a:1", FournisseurID: "a", Numero: "1", Nom: "TF1", Logo: f.srv.URL + "/picons/tf1.png", NumeroChaine: 1, CategorieID: "0"},
		{ID: "a:2", FournisseurID: "a", Numero: "2", Nom: "TF2 sans logo", NumeroChaine: 2, CategorieID: "0"},
	}
	b.st.films = []domain.Film{{ID: "a:10", FournisseurID: "a", Numero: "10", Nom: "Film", Image: "https://image.tmdb.org/t/p/w500/x.jpg", Extension: "mp4"}}
	b.st.series = []domain.SerieTV{{ID: "a:s5", FournisseurID: "a", Numero: "5", Nom: "Serie", Image: f.srv.URL + "/affiches/s5.jpg"}}
	return b, f, b.connecte("alice", false)
}

func corpsJSON(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("%v : %s", err, rec.Body)
	}
	return m
}

func elements(m map[string]any) []map[string]any {
	var out []map[string]any
	for _, e := range m["elements"].([]any) {
		out = append(out, e.(map[string]any))
	}
	return out
}

// US-23 CA1 : logo et image reecrits dans les listes, fiches et recherche ; vide reste
// vide ; aucune adresse d'origine (ni hote tiers) dans aucune reponse.
func TestCA1_LogosEtImagesReecritsPartout(t *testing.T) {
	b, f, c := bancImages(t)
	hote := strings.TrimPrefix(f.srv.URL, "http://")
	for _, chemin := range []string{"/api/v1/chaines", "/api/v1/chaines/a:1", "/api/v1/films", "/api/v1/films/a:10",
		"/api/v1/series", "/api/v1/series/a:s5", "/api/v1/recherche?q=tf", "/api/v1/recherche?q=fi", "/api/v1/recherche?q=se"} {
		rec := b.requete("GET", chemin, "", avec(c))
		if rec.Code != 200 {
			t.Fatalf("%s : %d %s", chemin, rec.Code, rec.Body)
		}
		corps := rec.Body.String()
		if strings.Contains(corps, hote) || strings.Contains(corps, "tmdb") || strings.Contains(corps, "picons") || strings.Contains(corps, "http://") {
			t.Errorf("%s laisse voir l'adresse d'origine : %s", chemin, corps)
		}
	}
	liste := elements(corpsJSON(t, b.requete("GET", "/api/v1/chaines", "", avec(c))))
	if l, _ := liste[0]["logo"].(string); !strings.HasPrefix(l, "/api/v1/image/") {
		t.Errorf("logo : %q", liste[0]["logo"])
	}
	if _, present := liste[1]["logo"]; present {
		t.Errorf("une chaine sans logo ne doit pas en recevoir : %v", liste[1])
	}
	film := corpsJSON(t, b.requete("GET", "/api/v1/films/a:10", "", avec(c)))
	if i, _ := film["image"].(string); !strings.HasPrefix(i, "/api/v1/image/") {
		t.Errorf("image https aussi reecrite (CSP img-src 'self') : %q", film["image"])
	}
	// Meme URL -> meme jeton (stabilite : l'ETag et le cache navigateur en dependent).
	l1 := corpsJSON(t, b.requete("GET", "/api/v1/chaines/a:1", "", avec(c)))["logo"]
	l2 := corpsJSON(t, b.requete("GET", "/api/v1/chaines/a:1", "", avec(c)))["logo"]
	if l1 != l2 {
		t.Error("jeton instable")
	}
}

// US-23 CA1 : sans proxy configure, les URL restent telles quelles (modules sans images).
func TestCA1_SansProxyLesURLRestentInchangees(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.remplir(1)
	c := b.connecte("alice", false)
	if l := corpsJSON(t, b.requete("GET", "/api/v1/chaines/a:100", "", avec(c)))["logo"]; l != "http://img/l.png" {
		t.Errorf("logo : %v", l)
	}
	if rec := b.requete("GET", "/api/v1/image/AAAAAAAAAAAAAAAAAAAAAAAA", "", avec(c)); rec.Code != 501 {
		t.Errorf("route sans proxy : %d", rec.Code)
	}
}

// US-23 CA1 (ETag) : changer le secret ou les fournisseurs change les jetons ; l'ETag du
// catalogue doit changer aussi, sinon un 304 garderait d'anciens jetons.
func TestCA1_ETagCatalogueLieAuxJetons(t *testing.T) {
	b, f, c := bancImages(t)
	etag1 := b.requete("GET", "/api/v1/chaines", "", avec(c)).Header().Get("ETag")
	if etag1 == "" {
		t.Fatal("pas d'ETag")
	}
	b2 := nouveauBanc(t, func(o *Options) {
		s := serviceImages(t, f, nil)
		o.Images = s
		o.Fournisseurs = []string{"a"}
	})
	b2.st.synchro = b.st.synchro
	b2.api.opt.Images = &autreEmpreinte{b2.api.opt.Images}
	c2 := b2.connecte("alice", false)
	etag2 := b2.requete("GET", "/api/v1/chaines", "", avec(c2)).Header().Get("ETag")
	if etag1 == etag2 {
		t.Error("l'ETag ne depend pas des jetons emis")
	}
}

type autreEmpreinte struct{ Images }

func (autreEmpreinte) Empreinte() string { return "autre" }

// US-23 CA7 (bout en bout HTTP) : premiere requete -> amont, seconde -> cache, jeton
// falsifie -> 403, non-image -> 502, trop gros -> 502, sans session -> 401.
func TestCA7_ImageDeBoutEnBout(t *testing.T) {
	b, f, c := bancImages(t)
	logo := corpsJSON(t, b.requete("GET", "/api/v1/chaines/a:1", "", avec(c)))["logo"].(string)

	// Sans session : 401, et l'amont n'est pas contacte.
	if rec := b.requete("GET", logo, ""); rec.Code != 401 || f.requetes.Load() != 0 {
		t.Fatalf("sans session : %d, %d requetes amont", rec.Code, f.requetes.Load())
	}
	// Premiere requete -> amont.
	rec := b.requete("GET", logo, "", avec(c))
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/png" || rec.Body.String() != string(png) {
		t.Fatalf("premiere : %d %v", rec.Code, rec.Header())
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "private, max-age=86400" {
		t.Errorf("Cache-Control : %q", cc)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.Contains(rec.Header().Get("Content-Security-Policy"), "sandbox") {
		t.Errorf("en-tetes de securite : %v", rec.Header())
	}
	etag := rec.Header().Get("ETag")
	if etag == "" {
		t.Fatal("pas d'ETag")
	}
	// Seconde -> cache (compteur du faux serveur) ; 304 sur If-None-Match.
	rec = b.requete("GET", logo, "", avec(c))
	if rec.Code != 200 || f.requetes.Load() != 1 {
		t.Fatalf("seconde : %d, %d requetes amont", rec.Code, f.requetes.Load())
	}
	rec = b.requete("GET", logo, "", avec(c), func(r *http.Request) { r.Header.Set("If-None-Match", etag) })
	if rec.Code != 304 || rec.Body.Len() != 0 {
		t.Fatalf("If-None-Match : %d", rec.Code)
	}
	// HEAD : memes en-tetes, pas de corps.
	if rec = b.requete("HEAD", logo, "", avec(c)); rec.Code != 200 || rec.Body.Len() != 0 {
		t.Errorf("HEAD : %d", rec.Code)
	}
	// Jeton falsifie ou invente -> 403, sans toucher l'amont.
	avant := f.requetes.Load()
	faux := []byte(logo)
	faux[len(faux)-5] ^= 1
	for nom, chemin := range map[string]string{
		"altere":     string(faux),
		"inconnu":    "/api/v1/image/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		"trop_court": "/api/v1/image/abc",
		"url_libre":  "/api/v1/image/" + strings.NewReplacer(":", "", "/", "").Replace(f.srv.URL+"/picons/tf1.png"),
	} {
		if rec := b.requete("GET", chemin, "", avec(c)); rec.Code != 403 {
			t.Errorf("%s : attendu 403, obtenu %d", nom, rec.Code)
		}
	}
	if f.requetes.Load() != avant {
		t.Error("un jeton refuse a atteint l'amont")
	}
	// Non-image -> 502.
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<html>"))
	})
	f.reponse.Store(&h)
	afficheSerie := corpsJSON(t, b.requete("GET", "/api/v1/series/a:s5", "", avec(c)))["image"].(string)
	if rec := b.requete("GET", afficheSerie, "", avec(c)); rec.Code != 502 {
		t.Errorf("non-image : %d %s", rec.Code, rec.Body)
	}
	// Trop gros (limite de test : 1 Kio) -> 502, et rien en cache.
	h2 := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(make([]byte, 4096))
	})
	f.reponse.Store(&h2)
	b.st.chaines[1].Logo = f.srv.URL + "/picons/gros.png"
	gros := corpsJSON(t, b.requete("GET", "/api/v1/chaines/a:2", "", avec(c)))["logo"].(string)
	if rec := b.requete("GET", gros, "", avec(c)); rec.Code != 502 {
		t.Errorf("trop gros : %d", rec.Code)
	}
}

// US-23 CA4 : image absente -> 404 puis cache negatif (une seule requete amont).
func TestCA4_ImageAbsente404EtCacheNegatif(t *testing.T) {
	b, f, c := bancImages(t)
	h := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) })
	f.reponse.Store(&h)
	logo := corpsJSON(t, b.requete("GET", "/api/v1/chaines/a:1", "", avec(c)))["logo"].(string)
	for i := 0; i < 4; i++ {
		rec := b.requete("GET", logo, "", avec(c))
		if rec.Code != 404 || corpsJSON(t, rec)["code"] != "image_introuvable" {
			t.Fatalf("%d : %s", rec.Code, rec.Body)
		}
	}
	if n := f.requetes.Load(); n != 1 {
		t.Fatalf("%d requetes amont pour une image absente", n)
	}
}

// US-23 CA3 : un jeton valide dont l'hote n'est plus autorise est refuse (403), sans connexion.
func TestCA3_HoteNonAutoriseRefuse(t *testing.T) {
	f := nouveauFauxImages(t)
	c := config.Defauts().Images
	c.CacheDossier = filepath.Join(t.TempDir(), "img")
	filtre, _ := hotes.Nouveau("http://fournisseur.test", nil, nil) // 127.0.0.1 NON autorise
	svc, err := images.Nouveau(images.Options{Config: c, Secret: []byte("0123456789abcdef0123456789abcdef-secret"),
		Fournisseurs: []images.Origine{{ID: "a", Base: f.srv.URL}}, Filtre: filtre})
	if err != nil {
		t.Fatal(err)
	}
	b := nouveauBanc(t, func(o *Options) { o.Images = svc })
	b.st.chaines = []domain.Chaine{{ID: "a:1", FournisseurID: "a", Numero: "1", Nom: "X", Logo: f.srv.URL + "/p.png"}}
	ck := b.connecte("alice", false)
	logo := corpsJSON(t, b.requete("GET", "/api/v1/chaines/a:1", "", avec(ck)))["logo"].(string)
	rec := b.requete("GET", logo, "", avec(ck))
	if rec.Code != 403 || corpsJSON(t, rec)["code"] != "hote_refuse" || f.requetes.Load() != 0 {
		t.Fatalf("%d %s, %d requetes amont", rec.Code, rec.Body, f.requetes.Load())
	}
}

// US-23 CA1 (export) : l'export NDJSON porte les memes URL reecrites.
func TestCA1_ExportNDJSONReecrit(t *testing.T) {
	f := nouveauFauxImages(t)
	svc := serviceImages(t, f, nil)
	b, st, c := bancExport(t, 3, 2, 2)
	_ = st
	b.api.opt.Images = svc
	for _, typ := range []string{"live", "vod", "series"} {
		rec := b.requete("GET", "/api/v1/catalogue/export?type="+typ, "", avec(c), gz)
		if rec.Code != 200 {
			t.Fatalf("%s : %d", typ, rec.Code)
		}
		for _, l := range lignes(t, rec) {
			for _, champ := range []string{"logo", "image"} {
				if v, ok := l[champ].(string); ok && !strings.HasPrefix(v, "/api/v1/image/") {
					t.Errorf("%s : %s = %q", typ, champ, v)
				}
			}
		}
	}
}

// US-23 CA6 : l'export de 25 000 chaines + 40 000 films avec URL reecrites (une URL
// DISTINCTE par element, comme les vrais picons) reste sous 3 s, et la taille ne derive pas.
func TestCA6_ExportReecritMoinsDeTroisSecondes(t *testing.T) {
	if testing.Short() {
		t.Skip("volume")
	}
	nChaines, nFilms := 25000, 40000
	if avecRace {
		nChaines, nFilms = 1500, 2500
	}
	f := nouveauFauxImages(t)
	svc := serviceImages(t, f, nil)
	ctx := context.Background()
	st, err := stockage.Ouvrir(ctx, filepath.Join(t.TempDir(), "e.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Fermer() })
	inst := instantaneExport(time.UnixMilli(5000), nChaines, nFilms, 10)
	for i := range inst.Chaines {
		inst.Chaines[i].Logo = f.srv.URL + "/picons/chaine-" + strconv.Itoa(i) + "-hd.png"
	}
	for i := range inst.Films {
		inst.Films[i].Image = "https://image.tmdb.org/t/p/w600_and_h900_bestv2/" + strconv.FormatInt(int64(i)*7919+1<<40, 36) + ".jpg"
	}
	if err := st.EcrireCatalogue(ctx, inst); err != nil {
		t.Fatal(err)
	}
	b := nouveauBanc(t, func(o *Options) { o.Stockage, o.Video, o.Export, o.Images = st, st, st, svc })
	c := b.connecte("alice", false)
	srv := httptest.NewServer(b.h)
	defer srv.Close()
	cl := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	get := func(typ string) (n int, brut, comp int64) {
		req, _ := http.NewRequest("GET", srv.URL+"/api/v1/catalogue/export?type="+typ, nil)
		req.AddCookie(c)
		req.Header.Set("Accept-Encoding", "gzip")
		rep, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer rep.Body.Close()
		cnt := &compteurOctets{r: rep.Body}
		z, err := gzip.NewReader(cnt)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(z)
		sc.Buffer(make([]byte, 1<<20), 1<<20)
		for sc.Scan() {
			var x struct {
				Logo  string `json:"logo"`
				Image string `json:"image"`
			}
			if err := json.Unmarshal(sc.Bytes(), &x); err != nil || !strings.HasPrefix(x.Logo+x.Image, "/api/v1/image/") {
				t.Fatalf("ligne invalide : %v %s", err, sc.Text())
			}
			n++
			brut += int64(len(sc.Bytes())) + 1
		}
		_, _ = io.Copy(io.Discard, rep.Body)
		return n, brut, cnt.n
	}
	debut := time.Now()
	nl, bl, cl1 := get("live")
	nv, bv, cv := get("vod")
	d := time.Since(debut)
	if nl != nChaines || nv != nFilms {
		t.Fatalf("lignes : %d chaines, %d films", nl, nv)
	}
	t.Logf("export reecrit : %s ; chaines %d o/ligne (%d o gzip/ligne), films %d o/ligne (%d o gzip/ligne)", d,
		bl/int64(nl), cl1/int64(nl), bv/int64(nv), cv/int64(nv))
	if d > 3*time.Second && !avecRace {
		t.Errorf("export en %s, budget 3 s", d)
	}
	// Jeton compact : une ligne de chaine ne depasse pas ~300 octets, un film ~330.
	if bl/int64(nl) > 300 || bv/int64(nv) > 330 {
		t.Errorf("la taille des lignes derive : %d et %d octets par ligne", bl/int64(nl), bv/int64(nv))
	}
}
