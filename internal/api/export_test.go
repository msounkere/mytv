package api

import (
	"bufio"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"mytv/internal/domain"
	"mytv/internal/stockage"
)

// bancExport : l'API devant un VRAI stockage SQLite rempli de n chaines, films et series.
func bancExport(t *testing.T, nCh, nFilms, nSeries int) (*banc, *stockage.Base, *http.Cookie) {
	t.Helper()
	ctx := context.Background()
	st, err := stockage.Ouvrir(ctx, filepath.Join(t.TempDir(), "e.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Fermer() })
	if err := st.EcrireCatalogue(ctx, instantaneExport(time.UnixMilli(5000), nCh, nFilms, nSeries)); err != nil {
		t.Fatal(err)
	}
	b := nouveauBanc(t, func(o *Options) { o.Stockage, o.Video, o.Export = st, st, st })
	return b, st, b.connecte("alice", false)
}

func instantaneExport(pris time.Time, nCh, nFilms, nSeries int) *domain.Instantane {
	s := &domain.Instantane{FournisseurID: "a", Pris: pris}
	for _, t := range []domain.Type{domain.TypeLive, domain.TypeFilm, domain.TypeSerie} {
		s.Categories = append(s.Categories,
			domain.Categorie{ID: "1", FournisseurID: "a", Type: t, Nom: "FR| " + string(t), Ordre: 1},
			domain.Categorie{ID: "2", FournisseurID: "a", Type: t, Nom: "[EN] " + string(t), Ordre: 2})
	}
	aj := time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)
	for i := 0; i < nCh; i++ {
		s.Chaines = append(s.Chaines, domain.Chaine{ID: domain.ContenuID("a:" + strconv.Itoa(1000+i)), FournisseurID: "a",
			Numero: strconv.Itoa(1000 + i), Nom: "FR - Chaine " + strconv.Itoa(i), Logo: "http://img.test/l.png", EpgID: "c" + strconv.Itoa(i),
			CategorieID: strconv.Itoa(1 + i%2), NumeroChaine: i + 1, Ordre: i, Ajoute: aj})
	}
	for i := 0; i < nFilms; i++ {
		s.Films = append(s.Films, domain.Film{ID: domain.ContenuID("a:" + strconv.Itoa(200000+i)), FournisseurID: "a",
			Numero: strconv.Itoa(200000 + i), Nom: "Film " + strconv.Itoa(i) + " (2021)", Image: "http://img.test/f.jpg",
			CategorieID: strconv.Itoa(1 + i%2), Extension: "mkv", Note: 6.5, Annee: "2021", Ajoute: aj})
	}
	for i := 0; i < nSeries; i++ {
		s.Series = append(s.Series, domain.SerieTV{ID: domain.ContenuID("a:" + strconv.Itoa(300000+i)), FournisseurID: "a",
			Numero: strconv.Itoa(300000 + i), Nom: "Serie " + strconv.Itoa(i), CategorieID: strconv.Itoa(1 + i%2), Note: 8, Annee: "2019", Ajoute: aj})
	}
	return s
}

// lignes decode un corps NDJSON (decompresse au besoin).
func lignes(t *testing.T, rec *httptest.ResponseRecorder) []map[string]any {
	t.Helper()
	var r io.Reader = rec.Body
	if rec.Header().Get("Content-Encoding") == "gzip" {
		z, err := gzip.NewReader(rec.Body)
		if err != nil {
			t.Fatal(err)
		}
		defer z.Close()
		r = z
	}
	var out []map[string]any
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("ligne non JSON %q : %v", sc.Text(), err)
		}
		out = append(out, m)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func gz(r *http.Request) { r.Header.Set("Accept-Encoding", "gzip") }

// proprietesContrat : proprietes et champs requis d'un schema du contrat.
func proprietesContrat(t *testing.T, schema string) (props map[string]bool, requis []string) {
	t.Helper()
	c, _ := lireContrat(t)
	n, ok := c.Components.Schemas[schema]
	if !ok {
		t.Fatalf("schema %s absent du contrat", schema)
	}
	var s struct {
		Required   []string             `yaml:"required"`
		Properties map[string]yaml.Node `yaml:"properties"`
	}
	if err := n.Decode(&s); err != nil {
		t.Fatal(err)
	}
	props = map[string]bool{}
	for k := range s.Properties {
		props[k] = true
	}
	return props, s.Required
}

// US-16 CA1 : chaque ligne a EXACTEMENT les champs du schema du contrat (aucun en
// trop, tous les requis), X-Elements = nombre de lignes, gzip si demande.
func TestCA1_ExportNDJSONConformeAuContrat(t *testing.T) {
	b, _, c := bancExport(t, 7, 5, 3)
	for typ, cas := range map[string]struct {
		schema string
		n      int
	}{"live": {"Chaine", 7}, "vod": {"Film", 5}, "series": {"Serie", 3}, "categories": {"Categorie", 6}} {
		for _, avecGzip := range []bool{false, true} {
			mods := []func(*http.Request){avec(c)}
			if avecGzip {
				mods = append(mods, gz)
			}
			rec := b.requete("GET", "/api/v1/catalogue/export?type="+typ, "", mods...)
			if rec.Code != 200 {
				t.Fatalf("%s : %d %s", typ, rec.Code, rec.Body)
			}
			if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/x-ndjson") {
				t.Errorf("%s : Content-Type %q", typ, got)
			}
			if (rec.Header().Get("Content-Encoding") == "gzip") != avecGzip {
				t.Errorf("%s gzip=%v : Content-Encoding %q", typ, avecGzip, rec.Header().Get("Content-Encoding"))
			}
			if rec.Header().Get("ETag") == "" || !strings.Contains(rec.Header().Get("Vary"), "Accept-Encoding") {
				t.Errorf("%s : ETag/Vary absents : %v", typ, rec.Header())
			}
			if x := rec.Header().Get("X-Elements"); x != strconv.Itoa(cas.n) {
				t.Errorf("%s : X-Elements %q, attendu %d", typ, x, cas.n)
			}
			ls := lignes(t, rec)
			if len(ls) != cas.n {
				t.Fatalf("%s : %d lignes, attendu %d", typ, len(ls), cas.n)
			}
			props, requis := proprietesContrat(t, cas.schema)
			for _, l := range ls {
				for k := range l {
					if !props[k] {
						t.Errorf("%s : champ %q absent du schema %s", typ, k, cas.schema)
					}
				}
				for _, k := range requis {
					if _, ok := l[k]; !ok {
						t.Errorf("%s : champ requis %q manquant", typ, k)
					}
				}
			}
		}
	}
}

// US-16 : ajoute est fourni pour les chaines et les series (null si inconnu).
func TestCA3_AjouteChainesEtSeries(t *testing.T) {
	b, _, c := bancExport(t, 2, 0, 2)
	for _, typ := range []string{"live", "series"} {
		ls := lignes(t, b.requete("GET", "/api/v1/catalogue/export?type="+typ, "", avec(c)))
		if len(ls) != 2 || ls[0]["ajoute"] != "2024-05-01T12:00:00Z" {
			t.Errorf("%s : ajoute = %v", typ, ls)
		}
	}
	// Les listes paginees portent aussi le champ (meme schema que l'export).
	rec := b.requete("GET", "/api/v1/chaines", "", avec(c))
	if !strings.Contains(rec.Body.String(), `"ajoute":"2024-05-01T12:00:00Z"`) {
		t.Errorf("/chaines sans ajoute : %s", rec.Body)
	}
	rec = b.requete("GET", "/api/v1/series", "", avec(c))
	if !strings.Contains(rec.Body.String(), `"ajoute":"2024-05-01T12:00:00Z"`) {
		t.Errorf("/series sans ajoute : %s", rec.Body)
	}
}

// US-16 CA1 : ETag lie a la derniere synchronisation, commun a /version et a tous les
// exports ; 304 sans corps tant qu'elle ne change pas ; nouveau tag apres synchro.
func TestCA1_ExportETagLieALaSynchro(t *testing.T) {
	b, st, c := bancExport(t, 3, 3, 3)
	v := b.requete("GET", "/api/v1/catalogue/version", "", avec(c))
	if v.Code != 200 {
		t.Fatalf("version : %d %s", v.Code, v.Body)
	}
	var m struct {
		Version     string  `json:"version"`
		Categories  int     `json:"categories"`
		Chaines     int     `json:"chaines"`
		Films       int     `json:"films"`
		Series      int     `json:"series"`
		Synchronise *string `json:"synchronise"`
	}
	if err := json.Unmarshal(v.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m.Chaines != 3 || m.Films != 3 || m.Series != 3 || m.Categories != 6 || m.Synchronise == nil || *m.Synchronise != "1970-01-01T00:00:05Z" {
		t.Errorf("manifeste = %+v", m)
	}
	tag := v.Header().Get("ETag")
	if tag != `W/"`+m.Version+`"` {
		t.Errorf("ETag %q != version %q", tag, m.Version)
	}
	for _, chemin := range []string{"/api/v1/catalogue/version", "/api/v1/catalogue/export?type=live", "/api/v1/catalogue/export?type=vod",
		"/api/v1/catalogue/export?type=series", "/api/v1/catalogue/export?type=categories"} {
		rec := b.requete("GET", chemin, "", avec(c), func(r *http.Request) { r.Header.Set("If-None-Match", tag) })
		if rec.Code != 304 || rec.Body.Len() != 0 {
			t.Errorf("%s avec le bon tag : %d, corps %d octets", chemin, rec.Code, rec.Body.Len())
		}
		if rec.Header().Get("ETag") != tag {
			t.Errorf("%s : ETag du 304 %q", chemin, rec.Header().Get("ETag"))
		}
	}
	// Une synchronisation reussie change la version.
	if err := st.EcrireCatalogue(context.Background(), instantaneExport(time.UnixMilli(9000), 3, 3, 3)); err != nil {
		t.Fatal(err)
	}
	rec := b.requete("GET", "/api/v1/catalogue/export?type=live", "", avec(c), func(r *http.Request) { r.Header.Set("If-None-Match", tag) })
	if rec.Code != 200 || rec.Header().Get("ETag") == tag {
		t.Errorf("apres synchro : %d, ETag %q", rec.Code, rec.Header().Get("ETag"))
	}
}

func TestExportRefusEtErreurs(t *testing.T) {
	b, _, c := bancExport(t, 1, 1, 1)
	for _, chemin := range []string{"/api/v1/catalogue/version", "/api/v1/catalogue/export?type=live"} {
		if rec := b.requete("GET", chemin, ""); rec.Code != 401 {
			t.Errorf("%s sans session : %d", chemin, rec.Code)
		}
	}
	for _, q := range []string{"", "?type=", "?type=films", "?type=live%0d", "?type=LIVE"} {
		if rec := b.requete("GET", "/api/v1/catalogue/export"+q, "", avec(c)); rec.Code != 400 {
			t.Errorf("export%s : %d au lieu de 400", q, rec.Code)
		}
	}
	// Dependance absente : 501 « non_disponible », la route reste au contrat.
	b2 := nouveauBanc(t, nil)
	c2 := b2.connecte("bob", false)
	if rec := b2.requete("GET", "/api/v1/catalogue/export?type=live", "", avec(c2)); rec.Code != 501 {
		t.Errorf("sans export : %d", rec.Code)
	}
	if rec := b2.requete("GET", "/api/v1/catalogue/version", "", avec(c2)); rec.Code != 501 {
		t.Errorf("version sans export : %d", rec.Code)
	}
}

func TestAccepteGzip(t *testing.T) {
	for entete, attendu := range map[string]bool{"": false, "gzip": true, "br, gzip;q=0.8": true, "GZIP": true, "gzip;q=0": false,
		"identity": false, "deflate, gzip, br": true, "gzipx": false} {
		r := httptest.NewRequest("GET", "/", nil)
		r.Header.Set("Accept-Encoding", entete)
		if accepteGzip(r) != attendu {
			t.Errorf("%q : attendu %v", entete, attendu)
		}
	}
}

// Le direct est mono-fournisseur : l'export live et les categories live ne parlent
// que du fournisseur du direct (comme /chaines et /categories).
func TestExportLiveRestreintAuFournisseurDuDirect(t *testing.T) {
	ctx := context.Background()
	st, err := stockage.Ouvrir(ctx, filepath.Join(t.TempDir(), "e.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Fermer()
	for _, f := range []string{"a", "b"} {
		s := instantaneExport(time.UnixMilli(1000), 2, 1, 1)
		s.FournisseurID = f
		for i := range s.Categories {
			s.Categories[i].FournisseurID = f
		}
		for i := range s.Chaines {
			s.Chaines[i].FournisseurID, s.Chaines[i].ID = f, domain.ContenuID(fmt.Sprintf("%s:%s", f, s.Chaines[i].Numero))
		}
		for i := range s.Films {
			s.Films[i].FournisseurID, s.Films[i].ID = f, domain.ContenuID(fmt.Sprintf("%s:%s", f, s.Films[i].Numero))
		}
		for i := range s.Series {
			s.Series[i].FournisseurID, s.Series[i].ID = f, domain.ContenuID(fmt.Sprintf("%s:%s", f, s.Series[i].Numero))
		}
		if err := st.EcrireCatalogue(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	b := nouveauBanc(t, func(o *Options) {
		o.Stockage, o.Video, o.Export = st, st, st
		o.Fournisseurs, o.FournisseurLive = []string{"a", "b"}, "b"
	})
	c := b.connecte("alice", false)
	ls := lignes(t, b.requete("GET", "/api/v1/catalogue/export?type=live", "", avec(c)))
	if len(ls) != 2 || !strings.HasPrefix(ls[0]["id"].(string), "b:") {
		t.Errorf("live : %v", ls)
	}
	if ls := lignes(t, b.requete("GET", "/api/v1/catalogue/export?type=vod", "", avec(c))); len(ls) != 2 {
		t.Errorf("vod de tous les fournisseurs : %d", len(ls))
	}
	var m struct{ Chaines, Films, Categories int }
	rec := b.requete("GET", "/api/v1/catalogue/version", "", avec(c))
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil || m.Chaines != 2 || m.Films != 2 || m.Categories != 2+4+4 {
		t.Errorf("manifeste : %s", rec.Body)
	}
}

// US-16 CA1 : budget de l'import initial (25 000 chaines + 40 000 films) sur un vrai
// serveur HTTP en boucle locale, gzip, en moins de 3 s ; la memoire du serveur reste
// bornee (flux, pas de copie complete).
func TestCA1_ExportVolumeEnMoinsDeTroisSecondes(t *testing.T) {
	if testing.Short() {
		t.Skip("volume")
	}
	nChaines, nFilms := 25000, 40000
	if avecRace { // le detecteur de courses ralentit l'insertion de 20 fois : meme chemin, volume reduit
		nChaines, nFilms = 1500, 2500
	}
	b, _, c := bancExport(t, nChaines, nFilms, 500)
	srv := httptest.NewServer(b.h)
	defer srv.Close()
	cl := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	get := func(typ string) (n int, octets int64) {
		req, _ := http.NewRequest("GET", srv.URL+"/api/v1/catalogue/export?type="+typ, nil)
		req.AddCookie(c)
		req.Header.Set("Accept-Encoding", "gzip") // pose a la main : le client ne decompresse alors pas pour nous
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
				ID string `json:"id"`
			}
			if err := json.Unmarshal(sc.Bytes(), &x); err != nil || x.ID == "" {
				t.Fatalf("ligne invalide : %v", err)
			}
			n++
		}
		if err := sc.Err(); err != nil {
			t.Fatal(err)
		}
		if want := rep.Header.Get("X-Elements"); want != strconv.Itoa(n) {
			t.Errorf("%s : X-Elements %s, %d lignes", typ, want, n)
		}
		return n, cnt.n
	}
	debut := time.Now()
	nl, ol := get("live")
	nv, ov := get("vod")
	d := time.Since(debut)
	if nl != nChaines || nv != nFilms {
		t.Fatalf("lignes : %d chaines, %d films", nl, nv)
	}
	t.Logf("import initial : %s pour %d + %d lignes, %d + %d octets compresses", d, nl, nv, ol, ov)
	if d > 3*time.Second && !avecRace {
		t.Errorf("import initial en %s, budget 3 s", d)
	}
}

type compteurOctets struct {
	r io.Reader
	n int64
}

func (c *compteurOctets) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// ---------------------------------------------------------------- limites et fin de lecture

// US-16 : GET /compte donne les bornes de la configuration, conformes au schema.
func TestLimitesDansCompte(t *testing.T) {
	b := nouveauBanc(t, nil)
	c := b.connecte("alice", false)
	rec := b.requete("GET", "/api/v1/compte", "", avec(c))
	var out struct {
		Limites map[string]int64 `json:"limites"`
	}
	if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
		t.Fatalf("compte : %d %s", rec.Code, rec.Body)
	}
	cfg := b.cfg
	attendu := map[string]int64{
		"pagination_max": int64(cfg.API.PaginationMax), "grille_fenetre": int64(cfg.API.GrilleFenetreMax.Std().Seconds()),
		"favoris_max": int64(cfg.Profil.FavorisMax), "historique_max": int64(cfg.Profil.HistoriqueMax),
		"reglages_octets_max": int64(cfg.Profil.ReglagesMax),
	}
	for k, v := range attendu {
		if v <= 0 || out.Limites[k] != v {
			t.Errorf("limites[%s] = %d, attendu %d", k, out.Limites[k], v)
		}
	}
	if len(out.Limites) != len(attendu) {
		t.Errorf("limites = %v", out.Limites)
	}
	// Meme jeu de cles que le contrat (schema Compte.limites).
	c0, _ := lireContrat(t)
	var s struct {
		Properties map[string]struct {
			Properties map[string]yaml.Node `yaml:"properties"`
		} `yaml:"properties"`
	}
	n := c0.Components.Schemas["Compte"]
	if err := n.Decode(&s); err != nil {
		t.Fatal(err)
	}
	for k := range s.Properties["limites"].Properties {
		if _, ok := out.Limites[k]; !ok {
			t.Errorf("limite %q du contrat absente de la reponse", k)
		}
	}
	for k := range out.Limites {
		if _, ok := s.Properties["limites"].Properties[k]; !ok {
			t.Errorf("limite %q de la reponse absente du contrat", k)
		}
	}
}

type finEnregistree struct{ appels [][2]string }

func (f *finEnregistree) FinDeLecture(session, chaine string) {
	f.appels = append(f.appels, [2]string{session, chaine})
}

// US-16 : POST /live/{chaine}/fin — 204, session seulement, idempotent, origine verifiee.
func TestFinDeLecture(t *testing.T) {
	fin := &finEnregistree{}
	b := nouveauBanc(t, func(o *Options) { o.FinLive = fin })
	c := b.connecte("alice", false)
	for i := 0; i < 2; i++ { // idempotent
		if rec := b.requete("POST", "/api/v1/live/845410/fin", "", avec(c)); rec.Code != 204 || rec.Body.Len() != 0 {
			t.Fatalf("fin : %d %s", rec.Code, rec.Body)
		}
	}
	if len(fin.appels) != 2 || fin.appels[0][1] != "845410" || fin.appels[0][0] == "" {
		t.Errorf("appels = %v", fin.appels)
	}
	// La session passee est celle du cookie, pas un parametre du client.
	rec := b.requete("GET", "/api/v1/compte", "", avec(c))
	if !strings.Contains(rec.Body.String(), `"id":"`+fin.appels[0][0]+`"`) {
		t.Errorf("session de /fin differente de celle du compte : %s", rec.Body)
	}
	n := len(fin.appels)
	if rec := b.requete("POST", "/api/v1/live/845410/fin", ""); rec.Code != 401 {
		t.Errorf("sans session : %d", rec.Code)
	}
	if rec := b.requete("POST", "/api/v1/live/a.b/fin", "", avec(c)); rec.Code != 400 {
		t.Errorf("chaine invalide : %d", rec.Code)
	}
	if rec := b.requete("POST", "/api/v1/live/845410/fin", "", avec(c), func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }); rec.Code != 403 {
		t.Errorf("origine etrangere : %d", rec.Code)
	}
	b.requete("GET", "/api/v1/live/845410/fin", "", avec(c)) // un GET ne rend jamais la place
	if len(fin.appels) != n {
		t.Errorf("appels refuses mais executes : %v", fin.appels[n:])
	}
	// Sans relais branche : 204 quand meme (rien a rendre).
	b2 := nouveauBanc(t, nil)
	c2 := b2.connecte("bob", false)
	if rec := b2.requete("POST", "/api/v1/live/1/fin", "", avec(c2)); rec.Code != 204 {
		t.Errorf("sans relais : %d", rec.Code)
	}
}

// ecrivainEspion note, a chaque ligne lue dans le stockage, combien d'octets ont deja
// ete envoyes au client.
type exportEspion struct {
	domain.Export
	envoyes    func() int
	apresLigne []int // octets deja envoyes quand la ligne i est lue
}

func (e *exportEspion) Lire(emettre func(any) error) error {
	return e.Export.Lire(func(v any) error {
		e.apresLigne = append(e.apresLigne, e.envoyes())
		return emettre(v)
	})
}

type stockExportEspion struct {
	domain.ExportCatalogue
	espion *exportEspion
	rec    *compteEcritures
}

func (s *stockExportEspion) OuvrirExport(ctx context.Context, t domain.TypeExport, live string) (domain.Export, error) {
	e, err := s.ExportCatalogue.OuvrirExport(ctx, t, live)
	s.espion = &exportEspion{Export: e, envoyes: s.rec.total}
	return s.espion, err
}

// compteEcritures est un ResponseWriter qui compte les octets recus.
type compteEcritures struct {
	*httptest.ResponseRecorder
	n int
}

func (c *compteEcritures) Write(p []byte) (int, error) {
	c.n += len(p)
	return c.ResponseRecorder.Write(p)
}
func (c *compteEcritures) total() int { return c.n }

// US-16 CA1 : le flux n'est PAS materialise. Les octets partent vers le client
// pendant la lecture du stockage (jamais « tout lire, puis tout ecrire »), et le
// tampon d'ecriture reste borne : a chaque ligne lue, ce qui est lu mais pas encore
// envoye ne depasse pas la taille du tampon plus une ligne.
func TestCA1_ExportEnFluxSansMaterialisation(t *testing.T) {
	n := 20000
	if avecRace {
		n = 3000 // sous -race l'insertion est 20 fois plus lente ; 3000 lignes font toujours des dizaines de tampons
	}
	_, st, _ := bancExport(t, 0, n, 0)
	espion := &stockExportEspion{ExportCatalogue: st}
	b2 := nouveauBanc(t, func(o *Options) { o.Stockage, o.Video, o.Export = st, st, espion })
	c := b2.connecte("alice", false)

	req := httptest.NewRequest("GET", "/api/v1/catalogue/export?type=vod", nil)
	req.AddCookie(c)
	req.RemoteAddr = "203.0.113.5:4444"
	espion.rec = &compteEcritures{ResponseRecorder: httptest.NewRecorder()}
	b2.h.ServeHTTP(espion.rec, req)
	if espion.rec.Code != 200 {
		t.Fatalf("export : %d", espion.rec.Code)
	}
	l := espion.espion.apresLigne
	if len(l) != n {
		t.Fatalf("%d lignes lues", len(l))
	}
	total := espion.rec.n
	if l[0] != 0 || l[len(l)-1] == 0 {
		t.Errorf("rien d'envoye avant la fin : debut %d, fin %d", l[0], l[len(l)-1])
	}
	// Apres la moitie des lignes, plus de la moitie de ce qui est envoye au total l'est deja
	// (a un tampon pres) : c'est un flux, pas une copie differee.
	const tampon = 64 << 10
	if mi := l[len(l)/2]; mi < total/2-2*tampon {
		t.Errorf("a mi-parcours %d octets envoyes sur %d : le corps est retenu en memoire", mi, total)
	}
	// Retard borne : octets lus (estimes lineairement) - octets envoyes.
	ligne := total / len(l)
	for i := 1000; i < len(l); i += 1000 {
		if retard := i*ligne - l[i]; retard > 2*tampon+10*ligne {
			t.Fatalf("ligne %d : %d octets lus mais pas envoyes (tampon %d)", i, retard, tampon)
		}
	}
}

// ---------------------------------------------------------------- US-17 CA4 : 416

// US-17 CA4 : une plage hors du fichier est une erreur du CLIENT (416 + Content-Range
// bytes */taille), pas une panne amont (502).
func TestCA4_PlageHorsFichierRepond416(t *testing.T) {
	b := nouveauBanc(t, nil)
	c := b.connecte("alice", false)
	b.lecture.err = fmt.Errorf("lecture: %w", domain.ErrPlageHorsFichier{Total: 1000})
	rec := b.requete("GET", "/api/v1/vod/a:5", "", avec(c), func(r *http.Request) { r.Header.Set("Range", "bytes=5000-") })
	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("statut %d au lieu de 416 : %s", rec.Code, rec.Body)
	}
	if got := rec.Header().Get("Content-Range"); got != "bytes */1000" {
		t.Errorf("Content-Range = %q", got)
	}
	if !strings.Contains(rec.Body.String(), `"code":"plage_invalide"`) {
		t.Errorf("corps = %s", rec.Body)
	}
	// Taille inconnue : pas de Content-Range, toujours 416.
	b.lecture.err = domain.ErrPlageHorsFichier{Total: -1}
	rec = b.requete("GET", "/api/v1/vod/a:5", "", avec(c))
	if rec.Code != 416 || rec.Header().Get("Content-Range") != "" {
		t.Errorf("taille inconnue : %d %v", rec.Code, rec.Header())
	}
	// Une vraie panne amont reste un 502.
	b.lecture.err = domain.ErrAmont
	if rec := b.requete("GET", "/api/v1/vod/a:5", "", avec(c)); rec.Code != 502 {
		t.Errorf("panne amont : %d", rec.Code)
	}
}
