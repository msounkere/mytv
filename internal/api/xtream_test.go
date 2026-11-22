package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"mytv/internal/config"
	"mytv/internal/domain"
)

// exportFaux parcourt les listes du faux stockage, comme l'export SQLite.
type exportFaux struct {
	st *stockageFaux
	t  domain.TypeExport
}

func (e exportFaux) Elements() int { return 0 }
func (e exportFaux) Fermer() error { return nil }
func (e exportFaux) Lire(emettre func(any) error) error {
	var l []any
	switch e.t {
	case domain.ExportLive:
		for _, c := range e.st.chaines {
			l = append(l, c)
		}
	case domain.ExportVOD:
		for _, c := range e.st.films {
			l = append(l, c)
		}
	case domain.ExportSeries:
		for _, c := range e.st.series {
			l = append(l, c)
		}
	}
	for _, v := range l {
		if err := emettre(v); err != nil {
			return err
		}
	}
	return nil
}

type catalogueFaux struct{ st *stockageFaux }

func (c catalogueFaux) Manifeste(context.Context, string) (domain.Manifeste, error) {
	return domain.Manifeste{}, nil
}
func (c catalogueFaux) OuvrirExport(_ context.Context, t domain.TypeExport, _ string) (domain.Export, error) {
	return exportFaux{st: c.st, t: t}, nil
}

// lectureNative note la requete recue par le service de lecture (session, en-tete client).
type lectureNative struct {
	mu      sync.Mutex
	ids     []domain.ContenuID
	clients []string
	err     error
}

func (l *lectureNative) Servir(w http.ResponseWriter, req *http.Request, _ string, id domain.ContenuID) error {
	l.mu.Lock()
	l.ids = append(l.ids, id)
	l.clients = append(l.clients, req.Header.Get("X-Mytv-Client"))
	err := l.err
	l.mu.Unlock()
	if err != nil {
		return err
	}
	w.Header().Set("Content-Type", "video/x-matroska")
	fmt.Fprint(w, "octets:"+string(id))
	return nil
}

var cfgXtream = config.Xtream{Actif: true, URLPublique: "https://mytv.exemple.com"}

type bancXtream struct {
	*banc
	x          http.Handler
	lecture    *lectureNative
	connexions *comptesCompteur
}

// comptesCompteur compte les connexions (verification du mot de passe) demandees.
type comptesCompteur struct {
	domain.Comptes
	n atomic.Int32
}

func (c *comptesCompteur) Connecter(ctx context.Context, nom, mdp, adresse string) (string, *domain.Session, error) {
	c.n.Add(1)
	return c.Comptes.Connecter(ctx, nom, mdp, adresse)
}

func nouveauBancXtream(t *testing.T) *bancXtream {
	l := &lectureNative{}
	cpt := &comptesCompteur{}
	var st *stockageFaux
	b := nouveauBanc(t, func(o *Options) {
		st = o.Stockage.(*stockageFaux)
		o.Export = catalogueFaux{st: st}
		o.Lecture = l
		cpt.Comptes = o.Comptes
		o.Comptes = cpt
	})
	st.cats = []domain.Categorie{{ID: "10", FournisseurID: "a", Type: domain.TypeLive, Nom: "Info"},
		{ID: "20", FournisseurID: "a", Type: domain.TypeFilm, Nom: "Action"}, {ID: "99", FournisseurID: "b", Type: domain.TypeLive, Nom: "Autre"}}
	st.chaines = []domain.Chaine{{ID: "a:100", FournisseurID: "a", Numero: "100", Nom: "TF1", CategorieID: "10", EpgID: "TF1.fr"},
		{ID: "a:101", FournisseurID: "a", Numero: "101", Nom: "France 2", CategorieID: "11"}}
	st.films = []domain.Film{{ID: "a:5", FournisseurID: "a", Numero: "5", Nom: "Film 5", CategorieID: "20", Extension: "mkv", Note: 7.4}}
	st.series = []domain.SerieTV{{ID: "a:s9", FournisseurID: "a", Numero: "9", Nom: "Serie 9", CategorieID: "30"}}
	if _, err := b.svc.CreerUtilisateur(context.Background(), "formuler", mdp, false); err != nil {
		t.Fatal(err)
	}
	if _, err := b.svc.CreerUtilisateur(context.Background(), "chef", mdp, true); err != nil {
		t.Fatal(err)
	}
	return &bancXtream{banc: b, x: b.api.HandlerXtream(cfgXtream), lecture: l, connexions: cpt}
}

func (b *bancXtream) get(chemin string) *httptest.ResponseRecorder {
	b.t.Helper()
	r := httptest.NewRequest("GET", chemin, nil)
	r.RemoteAddr = "203.0.113.5:4444"
	w := httptest.NewRecorder()
	b.x.ServeHTTP(w, r)
	return w
}

const identXtream = "username=formuler&password=un+mot+de+passe+solide"
const cheminIdent = "formuler/un%20mot%20de%20passe%20solide"

func TestXtreamInactifNeRepondPas(t *testing.T) {
	b := nouveauBanc(t, nil)
	if h := b.api.HandlerXtream(config.Xtream{}); h != nil {
		t.Fatal("couche Xtream montee alors que xtream.actif est faux")
	}
}

// Compte d'appareil : auth 1 et server_info a l'adresse publique ; identifiants faux : auth 0
// (convention Xtream) ; compte administrateur : refuse (son mot de passe irait dans les adresses).
func TestXtreamAuthentification(t *testing.T) {
	b := nouveauBancXtream(t)
	var r struct {
		UserInfo   map[string]any `json:"user_info"`
		ServerInfo map[string]any `json:"server_info"`
	}
	w := b.get("/player_api.php?" + identXtream)
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil || r.UserInfo["auth"] != float64(1) {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	if r.ServerInfo["url"] != "mytv.exemple.com" || r.ServerInfo["port"] != "443" || r.ServerInfo["server_protocol"] != "https" {
		t.Errorf("server_info : %v", r.ServerInfo)
	}
	for _, q := range []string{"", "username=formuler&password=faux", "username=inconnu&password=x"} {
		if w := b.get("/player_api.php?" + q); !strings.Contains(w.Body.String(), `"auth":0`) {
			t.Errorf("%q : %d %s", q, w.Code, w.Body)
		}
	}
	if w := b.get("/player_api.php?username=chef&password=un+mot+de+passe+solide"); w.Code != 403 {
		t.Errorf("administrateur : %d %s", w.Code, w.Body)
	}
	if w := b.get("/movie/chef/un%20mot%20de%20passe%20solide/5.mkv"); w.Code != 403 {
		t.Errorf("flux d'un administrateur : %d", w.Code)
	}
}

func TestXtreamCategoriesEtListes(t *testing.T) {
	b := nouveauBancXtream(t)
	w := b.get("/player_api.php?" + identXtream + "&action=get_live_categories")
	if !strings.Contains(w.Body.String(), `"category_id":"10"`) || strings.Contains(w.Body.String(), `"99"`) {
		t.Errorf("categories (un seul fournisseur) : %s", w.Body)
	}
	var l []map[string]any
	w = b.get("/player_api.php?" + identXtream + "&action=get_live_streams")
	if err := json.Unmarshal(w.Body.Bytes(), &l); err != nil || len(l) != 2 {
		t.Fatalf("liste du direct : %v %s", err, w.Body)
	}
	if l[0]["stream_id"] != float64(100) || l[0]["epg_channel_id"] != "TF1.fr" || l[0]["stream_type"] != "live" {
		t.Errorf("chaine : %v", l[0])
	}
	w = b.get("/player_api.php?" + identXtream + "&action=get_live_streams&category_id=11")
	if err := json.Unmarshal(w.Body.Bytes(), &l); err != nil || len(l) != 1 || l[0]["name"] != "France 2" {
		t.Errorf("filtre de categorie : %s", w.Body)
	}
	w = b.get("/player_api.php?" + identXtream + "&action=get_vod_streams")
	if !strings.Contains(w.Body.String(), `"container_extension":"mkv"`) || !strings.Contains(w.Body.String(), `"stream_id":5`) {
		t.Errorf("films : %s", w.Body)
	}
	w = b.get("/player_api.php?" + identXtream + "&action=get_series")
	if !strings.Contains(w.Body.String(), `"series_id":9`) {
		t.Errorf("series : %s", w.Body)
	}
	// Liste compressee quand la box l'accepte.
	r := httptest.NewRequest("GET", "/player_api.php?"+identXtream+"&action=get_live_streams", nil)
	r.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	b.x.ServeHTTP(rec, r)
	if rec.Header().Get("Content-Encoding") != "gzip" {
		t.Error("liste non compressee")
	}
}

func TestXtreamFicheSerie(t *testing.T) {
	b := nouveauBancXtream(t)
	var r struct {
		Seasons  []map[string]any            `json:"seasons"`
		Episodes map[string][]map[string]any `json:"episodes"`
	}
	w := b.get("/player_api.php?" + identXtream + "&action=get_series_info&series_id=9")
	if err := json.Unmarshal(w.Body.Bytes(), &r); err != nil || len(r.Seasons) != 1 || len(r.Episodes["1"]) != 1 {
		t.Fatalf("%v %s", err, w.Body)
	}
	if e := r.Episodes["1"][0]; e["id"] != "9" || e["container_extension"] != "mkv" || e["title"] != "Pilote" {
		t.Errorf("episode : %v", e)
	}
}

// Films et episodes : relayes au nom de la session de l'appareil, en client « natif » (tels quels).
func TestXtreamFilmsEtEpisodesEnClientNatif(t *testing.T) {
	b := nouveauBancXtream(t)
	if w := b.get("/movie/" + cheminIdent + "/5.mkv"); w.Code != 200 || w.Body.String() != "octets:a:5" {
		t.Fatalf("film : %d %s", w.Code, w.Body)
	}
	if w := b.get("/series/" + cheminIdent + "/9.mkv"); w.Code != 200 || w.Body.String() != "octets:a:9.mkv" {
		t.Fatalf("episode : %d %s", w.Code, w.Body)
	}
	for _, c := range b.lecture.clients {
		if c != "natif" {
			t.Errorf("client %q, attendu natif", c)
		}
	}
	if w := b.get("/movie/formuler/faux/5.mkv"); w.Code != 401 {
		t.Errorf("mauvais mot de passe : %d", w.Code)
	}
	b.lecture.err = domain.ErrPlaceIndisponible
	if w := b.get("/movie/" + cheminIdent + "/5.mkv"); w.Code != 503 || w.Header().Get("Retry-After") == "" {
		t.Errorf("place occupee : %d", w.Code)
	}
}

// Direct : redirection vers un lien de diffusion signe au nom de la session, que l'API sert ;
// chaine inconnue : 404.
func TestXtreamDirectParLienSigne(t *testing.T) {
	b := nouveauBancXtream(t)
	w := b.get("/live/" + cheminIdent + "/100.m3u8")
	loc := w.Header().Get("Location")
	if w.Code != 302 || !strings.HasPrefix(loc, "/api/v1/cast/") || !strings.Contains(loc, "/live/100.m3u8?cap=hevc,eac3,ac3") {
		t.Fatalf("%d %q", w.Code, loc)
	}
	r := httptest.NewRequest("GET", loc, nil)
	rec := httptest.NewRecorder()
	b.h.ServeHTTP(rec, r)
	if rec.Code != 204 || len(b.live.vues) != 1 || b.live.vues[0] != "/api/v1/live/100.m3u8" {
		t.Errorf("lien signe : %d %v", rec.Code, b.live.vues)
	}
	if w := b.get("/live/" + cheminIdent + "/999.m3u8"); w.Code != 404 {
		t.Errorf("chaine inconnue : %d", w.Code)
	}
}

func TestXtreamM3U(t *testing.T) {
	b := nouveauBancXtream(t)
	w := b.get("/get.php?" + identXtream + "&type=m3u_plus&output=m3u8")
	corps := w.Body.String()
	if w.Code != 200 || !strings.HasPrefix(corps, "#EXTM3U\n") ||
		!strings.Contains(corps, `tvg-id="TF1.fr"`) || !strings.Contains(corps, `group-title="Info"`) ||
		!strings.Contains(corps, "https://mytv.exemple.com/live/"+cheminIdent+"/100.m3u8") {
		t.Errorf("%d %s", w.Code, corps)
	}
	if w := b.get("/get.php?username=formuler&password=faux"); w.Code != 401 {
		t.Errorf("mauvais mot de passe : %d", w.Code)
	}
}

// Une session par appareil, reutilisee : pas une nouvelle connexion (argon2) par requete.
func TestXtreamUneSeuleSessionParAppareil(t *testing.T) {
	b := nouveauBancXtream(t)
	for i := 0; i < 5; i++ {
		b.get("/player_api.php?" + identXtream)
		b.get("/movie/" + cheminIdent + "/5.mkv")
	}
	if n := b.connexions.n.Load(); n != 1 {
		t.Errorf("%d connexions ouvertes, attendu 1", n)
	}
}

// Langues et pays des box (Reglages > Box IPTV) : l'administrateur choisit les prefixes de noms ;
// la box ne recoit que les contenus gardes, et les categories qui en contiennent.
func TestXtreamFiltreLanguesEtPays(t *testing.T) {
	b := nouveauBancXtream(t)
	st := b.st
	st.cats = append(st.cats, domain.Categorie{ID: "12", FournisseurID: "a", Type: domain.TypeLive, Nom: "|AR| NEWS"},
		domain.Categorie{ID: "13", FournisseurID: "a", Type: domain.TypeLive, Nom: "4K UHD"},
		domain.Categorie{ID: "21", FournisseurID: "a", Type: domain.TypeFilm, Nom: "VOD - GERMANY [DE]"})
	st.chaines = []domain.Chaine{
		{ID: "a:100", FournisseurID: "a", Numero: "100", Nom: "FR - TF1", CategorieID: "10"},
		{ID: "a:102", FournisseurID: "a", Numero: "102", Nom: "Sans prefixe", CategorieID: "10"},
		{ID: "a:103", FournisseurID: "a", Numero: "103", Nom: "AR - Al Jazeera", CategorieID: "12"},
		{ID: "a:104", FournisseurID: "a", Numero: "104", Nom: "Nature 4K", CategorieID: "13"},
	}
	st.films = []domain.Film{
		{ID: "a:5", FournisseurID: "a", Numero: "5", Nom: "FR - Le Film", CategorieID: "20", Extension: "mkv"},
		{ID: "a:6", FournisseurID: "a", Numero: "6", Nom: "DE - Der Film", CategorieID: "21", Extension: "mkv"},
	}
	admin := b.connecte("root", true)
	rec := b.requete("GET", "/api/v1/admin/xtream", "", avec(admin))
	var lu struct {
		Prefixes    []string            `json:"prefixes"`
		Disponibles []prefixeXtreamJSON `json:"disponibles"`
		Revision    int64               `json:"revision"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &lu); rec.Code != 200 || err != nil || len(lu.Prefixes) != 0 {
		t.Fatalf("lecture : %d %s", rec.Code, rec.Body)
	}
	comptes := map[string]prefixeXtreamJSON{}
	for _, p := range lu.Disponibles {
		comptes[p.Code] = p
	}
	if comptes["FR"].Chaines != 1 || comptes["FR"].Films != 1 || comptes["AR"].Chaines != 1 || comptes["DE"].Films != 1 {
		t.Errorf("comptes par prefixe : %+v", lu.Disponibles)
	}
	if rec := b.requete("PUT", "/api/v1/admin/xtream", `{"prefixes":["FR"],"revision":0}`, avec(admin)); rec.Code != 200 {
		t.Fatalf("ecriture : %d %s", rec.Code, rec.Body)
	}
	w := b.get("/player_api.php?" + identXtream + "&action=get_live_categories")
	if c := w.Body.String(); !strings.Contains(c, `"category_id":"10"`) || strings.Contains(c, `"12"`) || strings.Contains(c, `"13"`) {
		t.Errorf("categories filtrees : %s", c)
	}
	w = b.get("/player_api.php?" + identXtream + "&action=get_live_streams")
	c := w.Body.String()
	if !strings.Contains(c, "FR - TF1") || !strings.Contains(c, "Sans prefixe") || strings.Contains(c, "Al Jazeera") || strings.Contains(c, "Nature 4K") {
		t.Errorf("chaines filtrees : %s", c)
	}
	w = b.get("/player_api.php?" + identXtream + "&action=get_vod_streams")
	if c := w.Body.String(); !strings.Contains(c, "FR - Le Film") || strings.Contains(c, "Der Film") {
		t.Errorf("films filtres : %s", c)
	}
	if m := b.get("/get.php?" + identXtream).Body.String(); !strings.Contains(m, "FR - TF1") || strings.Contains(m, "Al Jazeera") {
		t.Errorf("M3U filtre : %s", m)
	}
	// Refus : non administrateur, prefixe invalide, revision perimee.
	if rec := b.requete("GET", "/api/v1/admin/xtream", "", avec(b.connecte("alice", false))); rec.Code != 403 {
		t.Errorf("non administrateur : %d", rec.Code)
	}
	if rec := b.requete("PUT", "/api/v1/admin/xtream", `{"prefixes":["fr; x"],"revision":1}`, avec(admin)); rec.Code != 400 {
		t.Errorf("prefixe invalide : %d", rec.Code)
	}
	if rec := b.requete("PUT", "/api/v1/admin/xtream", `{"prefixes":[],"revision":0}`, avec(admin)); rec.Code != 409 {
		t.Errorf("revision perimee : %d", rec.Code)
	}
	// Liste vide : tout revient.
	if rec := b.requete("PUT", "/api/v1/admin/xtream", `{"prefixes":[],"revision":1}`, avec(admin)); rec.Code != 200 {
		t.Fatalf("remise a zero : %d %s", rec.Code, rec.Body)
	}
	if c := b.get("/player_api.php?" + identXtream + "&action=get_live_streams").Body.String(); !strings.Contains(c, "Al Jazeera") {
		t.Errorf("sans filtre, tout doit revenir : %s", c)
	}
}

func TestPrefixeNom(t *testing.T) {
	for nom, p := range map[string]string{"FR - TF1": "FR", "|EU| Canal+": "EU", "IT-[4K] - Un film": "IT", "ENG - Copshop": "ENG",
		"Sans prefixe": "", "4K UHD": "", "fr - minuscule": "", "ABCDE - trop long": ""} {
		if got := prefixeNom(nom); got != p {
			t.Errorf("%q : %q, attendu %q", nom, got, p)
		}
	}
}
