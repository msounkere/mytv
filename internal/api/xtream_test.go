package api

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"fmt"
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
	debuts  []string // parametre t recu
	plages  []string // en-tete Range recu
	err     error
}

func (l *lectureNative) Servir(w http.ResponseWriter, req *http.Request, _ string, id domain.ContenuID) error {
	l.mu.Lock()
	l.ids = append(l.ids, id)
	l.clients = append(l.clients, req.Header.Get("X-Mytv-Client"))
	l.debuts = append(l.debuts, req.URL.Query().Get("t"))
	l.plages = append(l.plages, req.Header.Get("Range"))
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

func nouveauBancXtream(t *testing.T, regler ...func(*Options)) *bancXtream {
	l := &lectureNative{}
	cpt := &comptesCompteur{}
	var st *stockageFaux
	b := nouveauBanc(t, func(o *Options) {
		st = o.Stockage.(*stockageFaux)
		o.Export = catalogueFaux{st: st}
		o.Lecture = l
		cpt.Comptes = o.Comptes
		o.Comptes = cpt
		for _, r := range regler {
			r(o)
		}
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

// Guide XMLTV : XML valide, un canal par identifiant de guide (les variantes partagent le leur),
// le meme identifiant que get_live_streams, programmes rattaches, filtre des langues applique.
func TestXtreamXMLTV(t *testing.T) {
	b := nouveauBancXtream(t)
	b.st.chaines = []domain.Chaine{
		{ID: "a:100", FournisseurID: "a", Numero: "100", Nom: "FR - TF1", CategorieID: "10", EpgID: "TF1.fr"},
		{ID: "a:105", FournisseurID: "a", Numero: "105", Nom: "FR - TF1 HD", CategorieID: "10", EpgID: "TF1.fr"},
		{ID: "a:101", FournisseurID: "a", Numero: "101", Nom: "FR - Sans guide", CategorieID: "10"},
		{ID: "a:103", FournisseurID: "a", Numero: "103", Nom: "AR - Al Jazeera", CategorieID: "12", EpgID: "AJ.ar"},
	}
	var tv struct {
		Canaux []struct {
			ID  string `xml:"id,attr"`
			Nom string `xml:"display-name"`
		} `xml:"channel"`
		Programmes []struct {
			Canal string `xml:"channel,attr"`
			Titre string `xml:"title"`
		} `xml:"programme"`
	}
	w := b.get("/xmltv.php?" + identXtream)
	if err := xml.Unmarshal(w.Body.Bytes(), &tv); w.Code != 200 || err != nil {
		t.Fatalf("%d %v %s", w.Code, err, w.Body)
	}
	ids := []string{}
	for _, c := range tv.Canaux {
		ids = append(ids, c.ID)
	}
	if strings.Join(ids, ",") != "TF1.fr,mytv.101,AJ.ar" {
		t.Errorf("canaux : %v", ids)
	}
	if len(tv.Programmes) == 0 || tv.Programmes[0].Canal != "TF1.fr" {
		t.Errorf("programmes : %+v", tv.Programmes)
	}
	if c := b.get("/player_api.php?" + identXtream + "&action=get_live_streams").Body.String(); !strings.Contains(c, `"epg_channel_id":"mytv.101"`) {
		t.Errorf("get_live_streams doit annoncer le meme identifiant de guide : %s", c)
	}
	admin := b.connecte("root", true)
	if rec := b.requete("PUT", "/api/v1/admin/xtream", `{"prefixes":["FR"],"revision":0}`, avec(admin)); rec.Code != 200 {
		t.Fatal(rec.Body)
	}
	if c := b.get("/xmltv.php?" + identXtream).Body.String(); strings.Contains(c, "AJ.ar") || !strings.Contains(c, "TF1.fr") {
		t.Errorf("guide filtre : %s", c)
	}
	if w := b.get("/xmltv.php?username=formuler&password=faux"); w.Code != 401 {
		t.Errorf("mauvais mot de passe : %d", w.Code)
	}
}

// Categories mytv (etape A2) : les listes d'un profil choisi par l'administrateur, en tete des
// categories, leurs contenus envoyes une seconde fois sous la categorie mytv.
func TestXtreamCategoriesMytv(t *testing.T) {
	liste := &aVoirFaux{listes: map[string][]domain.ElementAVoir{}}
	b := nouveauBancXtream(t, func(o *Options) { o.AVoir = liste })
	b.st.films = append(b.st.films, domain.Film{ID: "a:6", FournisseurID: "a", Numero: "6", Nom: "Film 6", CategorieID: "20", Extension: "mp4"})
	ctx := context.Background()
	u, err := b.svc.CreerUtilisateur(ctx, "root", mdp, true)
	if err != nil {
		t.Fatal(err)
	}
	moi, err := b.svc.CreerProfil(ctx, u.ID, "Moi")
	if err != nil {
		t.Fatal(err)
	}
	rec := b.requete("POST", "/api/v1/connexion", fmt.Sprintf(`{"nom":"root","mot_de_passe":%q}`, mdp))
	admin := rec.Result().Cookies()[0]
	liste.listes[moi.ID] = []domain.ElementAVoir{{ID: domain.AVoirFilm("a:6"), Contenu: "a:6"}, {ID: domain.AVoirSerie("a:s9"), Contenu: "a:s9", Serie: true}}
	_ = b.etat.AjouterFavori(ctx, moi.ID, "a:101")
	_ = b.etat.EnregistrerLecture(ctx, moi.ID, domain.Lecture{Contenu: "a:5", Position: 30 * time.Minute, Duree: 100 * time.Minute, Vu: time.Now()})
	_ = b.etat.EnregistrerLecture(ctx, moi.ID, domain.Lecture{Contenu: "a:9001.mkv", Serie: "a:s9", Position: time.Minute, Duree: 40 * time.Minute, Vu: time.Now()})

	// Sans choix : le profil (vide) de la box, aucune categorie mytv.
	if c := b.get("/player_api.php?" + identXtream + "&action=get_vod_categories").Body.String(); strings.Contains(c, "99000") {
		t.Errorf("sans profil choisi : %s", c)
	}
	var lu struct {
		Profils  []map[string]string `json:"profils"`
		Revision int64               `json:"revision"`
	}
	rec = b.requete("GET", "/api/v1/admin/xtream", "", avec(admin))
	if err := json.Unmarshal(rec.Body.Bytes(), &lu); err != nil || len(lu.Profils) != 2 || lu.Profils[1]["id"] != moi.ID {
		t.Fatalf("profils proposes : %s", rec.Body)
	}
	if rec := b.requete("PUT", "/api/v1/admin/xtream", `{"prefixes":[],"profil_listes":"inconnu","revision":0}`, avec(admin)); rec.Code != 400 {
		t.Errorf("profil d'un autre compte : %d", rec.Code)
	}
	if rec := b.requete("PUT", "/api/v1/admin/xtream", fmt.Sprintf(`{"prefixes":[],"profil_listes":%q,"revision":0}`, moi.ID), avec(admin)); rec.Code != 200 {
		t.Fatalf("choix du profil : %d %s", rec.Code, rec.Body)
	}
	// Un changement de prefixes sans profil_listes garde le profil.
	if rec := b.requete("PUT", "/api/v1/admin/xtream", `{"prefixes":["FR"],"revision":1}`, avec(admin)); rec.Code != 200 || !strings.Contains(rec.Body.String(), moi.ID) {
		t.Fatalf("profil garde : %d %s", rec.Code, rec.Body)
	}
	if rec := b.requete("PUT", "/api/v1/admin/xtream", `{"prefixes":[],"revision":2}`, avec(admin)); rec.Code != 200 {
		t.Fatal(rec.Body)
	}

	cats := b.get("/player_api.php?" + identXtream + "&action=get_vod_categories").Body.String()
	if !strings.HasPrefix(cats, `[{"category_id":"990002","category_name":"★ Ma liste"`) || !strings.Contains(cats, `"990003"`) {
		t.Errorf("categories films : %s", cats)
	}
	var films []map[string]any
	_ = json.Unmarshal(b.get("/player_api.php?"+identXtream+"&action=get_vod_streams").Body.Bytes(), &films)
	if len(films) != 4 || films[0]["name"] != "Film 6" || films[0]["category_id"] != "990002" || films[1]["category_id"] != "990003" {
		t.Errorf("films : %v", films)
	}
	_ = json.Unmarshal(b.get("/player_api.php?"+identXtream+"&action=get_vod_streams&category_id=990003").Body.Bytes(), &films)
	if len(films) != 1 || films[0]["name"] != "Film 5 — reprendre à 30 min" || films[0]["stream_id"] != float64(1000000005) || films[0]["container_extension"] != "mkv" {
		t.Errorf("categorie En cours seule : %v", films)
	}
	// La reprise : le film 5, a la position de l'historique moins 10 s, sans plage d'octets.
	r := httptest.NewRequest("GET", "/movie/"+cheminIdent+"/1000000005.mkv", nil)
	r.Header.Set("Range", "bytes=0-")
	w := httptest.NewRecorder()
	b.x.ServeHTTP(w, r)
	if n := len(b.lecture.ids); w.Code != 200 || n != 1 || b.lecture.ids[0] != "a:5" || b.lecture.debuts[0] != "1790" || b.lecture.plages[0] != "" {
		t.Errorf("reprise : %d %v %v %v", w.Code, b.lecture.ids, b.lecture.debuts, b.lecture.plages)
	}
	// Le film lui-meme part du debut, Range relaye.
	r = httptest.NewRequest("GET", "/movie/"+cheminIdent+"/5.mkv", nil)
	r.Header.Set("Range", "bytes=0-")
	b.x.ServeHTTP(httptest.NewRecorder(), r)
	if b.lecture.debuts[1] != "" || b.lecture.plages[1] != "bytes=0-" {
		t.Errorf("film normal : %v %v", b.lecture.debuts, b.lecture.plages)
	}
	if c := b.get("/player_api.php?" + identXtream + "&action=get_vod_info&vod_id=1000000005").Body.String(); !strings.Contains(c, `"stream_id":1000000005`) || !strings.Contains(c, `"container_extension":"mkv"`) || !strings.Contains(c, `"name":"Film 5"`) {
		t.Errorf("fiche de reprise : %s", c)
	}
	if c := b.get("/player_api.php?" + identXtream + "&action=get_series_categories").Body.String(); !strings.Contains(c, `"990004"`) || !strings.Contains(c, `"990005"`) {
		t.Errorf("categories series : %s", c)
	}
	if c := b.get("/player_api.php?" + identXtream + "&action=get_live_categories").Body.String(); !strings.HasPrefix(c, `[{"category_id":"990001"`) {
		t.Errorf("categories direct : %s", c)
	}
	if c := b.get("/player_api.php?" + identXtream + "&action=get_live_streams&category_id=990001").Body.String(); !strings.Contains(c, "France 2") || strings.Contains(c, "TF1") {
		t.Errorf("mes chaines : %s", c)
	}
}
