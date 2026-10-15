package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"mytv/internal/domain"
)

// ---------------------------------------------------------------- CA1 / CA3 : conformite au contrat

type contrat struct {
	Paths      map[string]map[string]yaml.Node `yaml:"paths"`
	Components struct {
		Schemas    map[string]yaml.Node `yaml:"schemas"`
		Responses  map[string]yaml.Node `yaml:"responses"`
		Parameters map[string]yaml.Node `yaml:"parameters"`
		Headers    map[string]yaml.Node `yaml:"headers"`
	} `yaml:"components"`
}

func lireContrat(t *testing.T) (c contrat, brut []byte) {
	t.Helper()
	brut, err := os.ReadFile("../../docs/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(brut, &c); err != nil {
		t.Fatal(err)
	}
	return c, brut
}

var methodesHTTP = map[string]bool{"get": true, "post": true, "put": true, "delete": true, "patch": true, "head": true}

// CA3 : chaque route reelle existe dans le contrat, et inversement.
func TestContratEtRoutesCorrespondent(t *testing.T) {
	c, _ := lireContrat(t)
	dansContrat := map[string]bool{}
	for chemin, ops := range c.Paths {
		for m := range ops {
			if methodesHTTP[m] {
				dansContrat[strings.ToUpper(m)+" "+chemin] = true
			}
		}
	}
	reelles := map[string]bool{}
	for _, r := range nouveauBanc(t, nil).api.Routes() {
		reelles[r.Methode+" "+r.Chemin] = true
	}
	for r := range reelles {
		if !dansContrat[r] {
			t.Errorf("route reelle absente du contrat : %s", r)
		}
	}
	for r := range dansContrat {
		if !reelles[r] {
			t.Errorf("route du contrat absente du code : %s", r)
		}
	}
}

// CA3 : le contrat est coherent (toutes les $ref resolvent) et le niveau d'acces
// declare par route correspond a la section `security` du contrat.
func TestContratCoherentAvecLesNiveauxDAcces(t *testing.T) {
	c, brut := lireContrat(t)
	exist := func(m map[string]yaml.Node, nom string) bool { _, ok := m[nom]; return ok }
	for _, m := range regexp.MustCompile(`\$ref: "#/components/(\w+)/([\w-]+)"`).FindAllStringSubmatch(string(brut), -1) {
		ok := false
		switch m[1] {
		case "schemas":
			ok = exist(c.Components.Schemas, m[2])
		case "responses":
			ok = exist(c.Components.Responses, m[2])
		case "parameters":
			ok = exist(c.Components.Parameters, m[2])
		case "headers":
			ok = exist(c.Components.Headers, m[2])
		}
		if !ok {
			t.Errorf("reference orpheline : %s", m[0])
		}
	}
	acces := map[string]Acces{}
	for _, r := range nouveauBanc(t, nil).api.Routes() {
		acces[strings.ToLower(r.Methode)+" "+r.Chemin] = r.Acces
	}
	for chemin, ops := range c.Paths {
		for m, node := range ops {
			if !methodesHTTP[m] {
				continue
			}
			var op struct {
				Security  *[]map[string][]string `yaml:"security"`
				Responses map[string]any         `yaml:"responses"`
			}
			if err := node.Decode(&op); err != nil {
				t.Fatal(err)
			}
			if len(op.Responses) == 0 {
				t.Errorf("%s %s sans reponses", m, chemin)
			}
			a, ok := acces[m+" "+chemin]
			if !ok {
				continue
			}
			public := op.Security != nil && len(*op.Security) == 0
			local := false
			if op.Security != nil {
				for _, s := range *op.Security {
					if _, ok := s["local"]; ok {
						local = true
					}
				}
			}
			switch {
			case a == Public && !public, a != Public && public:
				t.Errorf("%s %s : acces %v incoherent avec security", m, chemin, a)
			case a == Lecture && !local, a != Lecture && local:
				t.Errorf("%s %s : compte local incoherent (acces %v)", m, chemin, a)
			}
		}
	}
}

// ---------------------------------------------------------------- CA4 : refus par defaut

func remplirChemin(c string) string {
	r := strings.NewReplacer("{id}", "a:1", "{profil}", "abc", "{fichier}", "1.m3u8", "{chaine}", "1", "{jeton}", "x")
	return Prefixe + r.Replace(c)
}

// CA4 : sans authentification, TOUTES les routes refusent (401), sauf connexion et sante.
func TestRefusParDefautSurToutesLesRoutes(t *testing.T) {
	b := nouveauBanc(t, nil)
	for _, r := range b.api.Routes() {
		rec := b.requete(r.Methode, remplirChemin(r.Chemin), "")
		if r.Acces == Public {
			if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
				t.Errorf("%s %s publique mais refusee : %d", r.Methode, r.Chemin, rec.Code)
			}
			continue
		}
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s sans authentification : %d (attendu 401)", r.Methode, r.Chemin, rec.Code)
		}
	}
}

// CA4 : un chemin ou une methode inconnus ne repondent pas non plus sans authentification.
func TestCheminsEtMethodesInconnusRefuses(t *testing.T) {
	b := nouveauBanc(t, nil)
	cookie := b.connecte("alice", false)
	for _, c := range []struct{ m, p string }{
		{"GET", "/api/v1/inexistant"}, {"GET", "/"}, {"GET", "/etc/passwd"}, {"POST", "/api/v1/chaines"},
		{"DELETE", "/api/v1/sante"}, {"GET", "/api/v1/live"},
	} {
		if rec := b.requete(c.m, c.p, ""); rec.Code != 401 && rec.Code != 307 && c.p != "/api/v1/sante" {
			t.Errorf("%s %s sans session : %d", c.m, c.p, rec.Code)
		}
		if rec := b.requete(c.m, c.p, "", avec(cookie)); rec.Code == 200 {
			t.Errorf("%s %s avec session : 200", c.m, c.p)
		}
	}
	if rec := b.requete("GET", "/api/v1/inexistant", "", avec(cookie)); rec.Code != 404 {
		t.Errorf("chemin inconnu avec session : %d", rec.Code)
	}
}

// CA4 : les routes reservees aux administrateurs refusent un utilisateur ordinaire.
func TestRoutesAdminRefuseesAuxOrdinaires(t *testing.T) {
	b := nouveauBanc(t, nil)
	user, admin := b.connecte("alice", false), b.connecte("root", true)
	for _, r := range b.api.Routes() {
		if r.Acces != Admin {
			continue
		}
		p := remplirChemin(r.Chemin)
		if rec := b.requete(r.Methode, p, "", avec(user)); rec.Code != 403 && p != Prefixe+"/live/etat" {
			t.Errorf("%s : utilisateur ordinaire = %d", p, rec.Code)
		}
		if rec := b.requete(r.Methode, p, "", avec(admin)); rec.Code == 401 || rec.Code == 403 {
			t.Errorf("%s : admin = %d", p, rec.Code)
		}
	}
}

// Sante : sans authentification, sans detail.
func TestSante(t *testing.T) {
	rec := nouveauBanc(t, nil).requete("GET", "/api/v1/sante", "")
	if rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"statut":"ok"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

// ---------------------------------------------------------------- catalogue

func (b *banc) remplir(n int) {
	for i := 0; i < n; i++ {
		b.st.chaines = append(b.st.chaines, domain.Chaine{ID: domain.ContenuID(fmt.Sprintf("a:%d", 100+i)), FournisseurID: "a",
			Numero: fmt.Sprint(100 + i), Nom: fmt.Sprintf("Chaine %02d", i), Logo: "http://img/l.png", CategorieID: fmt.Sprint(i % 2), NumeroChaine: i, EpgID: "e"})
	}
	b.st.cats = []domain.Categorie{{ID: "0", FournisseurID: "a", Type: domain.TypeLive, Nom: "Info", Ordre: 1},
		{ID: "1", FournisseurID: "a", Type: domain.TypeLive, Nom: "Sport", Ordre: 2},
		{ID: "7", FournisseurID: "a", Type: domain.TypeFilm, Nom: "Action", Ordre: 1}}
}

type pageChaines struct {
	Elements []map[string]any `json:"elements"`
	Limite   int              `json:"limite"`
	Decalage int              `json:"decalage"`
	Suivant  *int             `json:"suivant"`
}

func get(t *testing.T, b *banc, c *http.Cookie, chemin string) (int, pageChaines) {
	t.Helper()
	rec := b.requete("GET", chemin, "", avec(c))
	var p pageChaines
	json.Unmarshal(rec.Body.Bytes(), &p)
	return rec.Code, p
}

// CA2 : pagination, filtre de categorie, recherche, forme JSON typee sans identifiant fournisseur.
func TestChainesPaginationFiltreRecherche(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.remplir(7)
	c := b.connecte("alice", false)

	code, p := get(t, b, c, "/api/v1/chaines?limite=3")
	if code != 200 || len(p.Elements) != 3 || p.Suivant == nil || *p.Suivant != 3 || p.Limite != 3 {
		t.Fatalf("page 1 : %d %+v", code, p)
	}
	_, p = get(t, b, c, "/api/v1/chaines?limite=3&decalage=6")
	if len(p.Elements) != 1 || p.Suivant != nil {
		t.Fatalf("derniere page : %+v", p)
	}
	_, p = get(t, b, c, "/api/v1/chaines?categorie=a:1")
	if len(p.Elements) != 3 {
		t.Fatalf("categorie : %d elements", len(p.Elements))
	}
	_, p = get(t, b, c, "/api/v1/chaines?q=chaine+03")
	if len(p.Elements) != 1 || p.Elements[0]["id"] != "a:103" {
		t.Fatalf("recherche : %+v", p)
	}
	_, p = get(t, b, c, "/api/v1/chaines?limite=1")
	e := p.Elements[0]
	if e["lecture"] != "/api/v1/live/100.m3u8" || e["categorie"] != "a:0" || e["numero"].(float64) != 0 {
		t.Fatalf("forme : %+v", e)
	}
	// Elements vides = [] et non null.
	rec := b.requete("GET", "/api/v1/chaines?q=zzzz", "", avec(c))
	if !strings.Contains(rec.Body.String(), `"elements":[]`) {
		t.Fatalf("liste vide : %s", rec.Body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("Content-Type %q", ct)
	}
}

// CA5 : la pagination est bornee, les entrees invalides refusees par 400 {code, message}.
func TestPaginationEtParametresInvalides(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.remplir(3)
	c := b.connecte("alice", false)
	for _, q := range []string{"limite=0", "limite=201", "limite=abc", "limite=-1", "decalage=-1", "decalage=x", "decalage=99999999999",
		"categorie=zzz", "categorie=inconnu:1", "categorie=a:", "q=" + strings.Repeat("a", 101)} {
		rec := b.requete("GET", "/api/v1/chaines?"+q, "", avec(c))
		var e map[string]string
		json.Unmarshal(rec.Body.Bytes(), &e)
		if rec.Code != 400 || e["code"] != "requete_invalide" || e["message"] == "" {
			t.Errorf("%s : %d %s", q, rec.Code, rec.Body)
		}
	}
	if rec := b.requete("GET", "/api/v1/chaines?limite=200", "", avec(c)); rec.Code != 200 {
		t.Errorf("limite = max refusee : %d", rec.Code)
	}
	for _, id := range []string{"x", "a:1/../2", "a:1%00", "'a:1", "a:" + strings.Repeat("9", 70)} {
		if rec := b.requete("GET", "/api/v1/chaines/"+id, "", avec(c)); rec.Code != 400 && rec.Code != 404 && rec.Code != 307 {
			t.Errorf("identifiant %q : %d", id, rec.Code)
		}
	}
	if rec := b.requete("GET", "/api/v1/chaines/a:999", "", avec(c)); rec.Code != 404 {
		t.Errorf("chaine absente : %d", rec.Code)
	}
}

// CA2 : categories par type, identifiees par <fournisseur>:<id>.
func TestCategories(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.remplir(1)
	c := b.connecte("alice", false)
	rec := b.requete("GET", "/api/v1/categories?type=live", "", avec(c))
	var p pageChaines
	json.Unmarshal(rec.Body.Bytes(), &p)
	if rec.Code != 200 || len(p.Elements) != 2 || p.Elements[0]["id"] != "a:0" || p.Elements[0]["fournisseur"] != "a" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := b.requete("GET", "/api/v1/categories", "", avec(c)); rec.Code != 400 {
		t.Errorf("type absent : %d", rec.Code)
	}
	if rec := b.requete("GET", "/api/v1/categories?type=xx", "", avec(c)); rec.Code != 400 {
		t.Errorf("type invalide : %d", rec.Code)
	}
}

// ETag : 304 tant que la synchronisation n'a pas change, 200 apres ; l'ETag depend de la requete.
func TestETagEtIfNoneMatch(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.remplir(3)
	c := b.connecte("alice", false)
	rec := b.requete("GET", "/api/v1/chaines", "", avec(c))
	tag := rec.Header().Get("ETag")
	if rec.Code != 200 || !strings.HasPrefix(tag, `W/"`) || rec.Header().Get("Cache-Control") != "private, no-cache" {
		t.Fatalf("%d %q %q", rec.Code, tag, rec.Header().Get("Cache-Control"))
	}
	avant := b.st.requetes
	rec = b.requete("GET", "/api/v1/chaines", "", avec(c), func(r *http.Request) { r.Header.Set("If-None-Match", tag) })
	if rec.Code != 304 || rec.Body.Len() != 0 || rec.Header().Get("ETag") != tag {
		t.Fatalf("304 attendu : %d %q", rec.Code, rec.Body)
	}
	if b.st.requetes != avant {
		t.Error("un 304 ne doit pas lire la liste")
	}
	rec = b.requete("GET", "/api/v1/chaines", "", avec(c), func(r *http.Request) { r.Header.Set("If-None-Match", `"x", `+tag+`, "y"`) })
	if rec.Code != 304 {
		t.Errorf("liste d'etags : %d", rec.Code)
	}
	if rec := b.requete("GET", "/api/v1/chaines?limite=2", "", avec(c), func(r *http.Request) { r.Header.Set("If-None-Match", tag) }); rec.Code != 200 {
		t.Errorf("autre requete, meme etag : %d", rec.Code)
	}
	b.st.mu.Lock()
	b.st.synchro["a"] = time.UnixMilli(5000) // nouvelle synchronisation
	b.st.mu.Unlock()
	rec = b.requete("GET", "/api/v1/chaines", "", avec(c), func(r *http.Request) { r.Header.Set("If-None-Match", tag) })
	if rec.Code != 200 || rec.Header().Get("ETag") == tag {
		t.Errorf("apres synchro : %d", rec.Code)
	}
}

// Plusieurs fournisseurs : la pagination traverse les fournisseurs sans trou ni doublon.
func TestFusionDeFournisseurs(t *testing.T) {
	b := nouveauBanc(t, func(o *Options) { o.Fournisseurs = []string{"a", "b"} })
	for _, f := range []struct {
		id string
		n  int
	}{{"a", 5}, {"b", 4}} {
		for i := 0; i < f.n; i++ {
			b.st.films = append(b.st.films, domain.Film{ID: domain.ContenuID(fmt.Sprintf("%s:%d", f.id, i)), FournisseurID: f.id, Numero: fmt.Sprint(i), Nom: "Film", CategorieID: "1", Extension: "mp4"})
		}
	}
	c := b.connecte("alice", false)
	var tous []string
	off := 0
	for pages := 0; pages < 20; pages++ {
		rec := b.requete("GET", fmt.Sprintf("/api/v1/films?limite=2&decalage=%d", off), "", avec(c))
		var p pageChaines
		json.Unmarshal(rec.Body.Bytes(), &p)
		if rec.Code != 200 {
			t.Fatal(rec.Body)
		}
		for _, e := range p.Elements {
			tous = append(tous, e["id"].(string))
		}
		if p.Suivant == nil {
			break
		}
		off = *p.Suivant
	}
	want := "a:0 a:1 a:2 a:3 a:4 b:0 b:1 b:2 b:3"
	if strings.Join(tous, " ") != want {
		t.Fatalf("obtenu %v", tous)
	}
	// Filtre sur une categorie d'un seul fournisseur.
	rec := b.requete("GET", "/api/v1/films?categorie=b:1", "", avec(c))
	var p pageChaines
	json.Unmarshal(rec.Body.Bytes(), &p)
	if len(p.Elements) != 4 {
		t.Fatalf("categorie b:1 : %d", len(p.Elements))
	}
}

func TestTailleParSondage(t *testing.T) {
	for n := 0; n < 70; n++ {
		got, err := tailleParSondage(t.Context(), func(i int) (bool, error) { return i < n, nil })
		if err != nil || got != n {
			t.Fatalf("n=%d : %d %v", n, got, err)
		}
	}
}

// Les chaines exposees sont celles du fournisseur du direct (relais mono-fournisseur).
func TestChainesLimiteesAuFournisseurDuDirect(t *testing.T) {
	b := nouveauBanc(t, func(o *Options) { o.Fournisseurs = []string{"a", "b"}; o.FournisseurLive = "b" })
	b.st.chaines = []domain.Chaine{{ID: "a:1", FournisseurID: "a", Numero: "1", Nom: "A"}, {ID: "b:2", FournisseurID: "b", Numero: "2", Nom: "B"}}
	c := b.connecte("alice", false)
	_, p := get(t, b, c, "/api/v1/chaines")
	if len(p.Elements) != 1 || p.Elements[0]["id"] != "b:2" {
		t.Fatalf("%+v", p)
	}
	if rec := b.requete("GET", "/api/v1/chaines/a:1", "", avec(c)); rec.Code != 404 {
		t.Errorf("chaine d'un autre fournisseur : %d", rec.Code)
	}
	_, p = get(t, b, c, "/api/v1/chaines?categorie=a:0")
	if len(p.Elements) != 0 {
		t.Errorf("categorie d'un autre fournisseur : %+v", p)
	}
}

func TestFilmsSeriesSaisonsEtRecherche(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.st.films = []domain.Film{{ID: "a:5", FournisseurID: "a", Numero: "5", Nom: "Matrix", CategorieID: "7", Extension: "mkv", Note: 8.5, Ajoute: time.Date(2025, 1, 2, 3, 4, 5, 0, time.UTC)}, {ID: "a:6", FournisseurID: "a", Numero: "6", Nom: "Autre", CategorieID: "7", Extension: "mp4"}}
	b.st.series = []domain.SerieTV{{ID: "a:77", FournisseurID: "a", Numero: "77", Nom: "Matrix Series", CategorieID: "9"}}
	b.st.chaines = []domain.Chaine{{ID: "a:1", FournisseurID: "a", Numero: "1", Nom: "Matrix TV"}}
	c := b.connecte("alice", false)

	rec := b.requete("GET", "/api/v1/films/a:5", "", avec(c))
	var f map[string]any
	json.Unmarshal(rec.Body.Bytes(), &f)
	if rec.Code != 200 || f["lecture"] != "/api/v1/vod/a:5" || f["ajoute"] != "2025-01-02T03:04:05Z" || f["categorie"] != "a:7" {
		t.Fatalf("film : %d %s", rec.Code, rec.Body)
	}
	rec = b.requete("GET", "/api/v1/films/a:6", "", avec(c))
	if !strings.Contains(rec.Body.String(), `"ajoute":null`) {
		t.Fatalf("ajoute inconnu doit etre null : %s", rec.Body)
	}
	if rec := b.requete("GET", "/api/v1/films/a:404", "", avec(c)); rec.Code != 404 {
		t.Errorf("film absent : %d", rec.Code)
	}
	rec = b.requete("GET", "/api/v1/series/a:77", "", avec(c))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"saisons":"/api/v1/series/a:77/saisons"`) {
		t.Fatalf("serie : %d %s", rec.Code, rec.Body)
	}
	rec = b.requete("GET", "/api/v1/series/a:77/saisons", "", avec(c))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"lecture":"/api/v1/vod/a:9.mkv"`) || !strings.Contains(rec.Body.String(), `"duree":90`) {
		t.Fatalf("saisons : %d %s", rec.Code, rec.Body)
	}
	if rec := b.requete("GET", "/api/v1/series/a:1/saisons", "", avec(c)); rec.Code != 404 {
		t.Errorf("saisons d'une serie inconnue : %d", rec.Code)
	}
	rec = b.requete("GET", "/api/v1/recherche?q=matrix", "", avec(c))
	var r struct{ Chaines, Films, Series []any }
	json.Unmarshal(rec.Body.Bytes(), &r)
	if rec.Code != 200 || len(r.Chaines) != 1 || len(r.Films) != 1 || len(r.Series) != 1 {
		t.Fatalf("recherche : %d %s", rec.Code, rec.Body)
	}
	if rec := b.requete("GET", "/api/v1/recherche?q=m", "", avec(c)); rec.Code != 400 {
		t.Errorf("recherche trop courte : %d", rec.Code)
	}
	if rec := b.requete("GET", "/api/v1/recherche", "", avec(c)); rec.Code != 400 {
		t.Errorf("recherche sans q : %d", rec.Code)
	}
}

func TestFilmsNonActives501(t *testing.T) {
	b := nouveauBanc(t, func(o *Options) { o.Video = nil })
	c := b.connecte("alice", false)
	for _, p := range []string{"/api/v1/films", "/api/v1/films/a:1", "/api/v1/series", "/api/v1/series/a:1/saisons"} {
		rec := b.requete("GET", p, "", avec(c))
		if rec.Code != 501 || !strings.Contains(rec.Body.String(), "non_disponible") {
			t.Errorf("%s : %d %s", p, rec.Code, rec.Body)
		}
	}
	if rec := b.requete("GET", "/api/v1/recherche?q=abc", "", avec(c)); rec.Code != 200 {
		t.Errorf("la recherche sans video doit marcher : %d", rec.Code)
	}
}

func TestFournisseurEnErreurDonne502SansDetail(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.st.series = []domain.SerieTV{{ID: "a:1", FournisseurID: "a", Numero: "boom", Nom: "S"}}
	c := b.connecte("alice", false)
	rec := b.requete("GET", "/api/v1/series/a:1/saisons", "", avec(c))
	if rec.Code != 502 || strings.Contains(rec.Body.String(), "amont") || strings.Contains(rec.Body.String(), "fournisseur :") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

// ---------------------------------------------------------------- CA5 / CA6 : limites, en-tetes, CORS

// CA5 : un corps trop gros est refuse (Content-Length annonce ou flux sans taille).
func TestCorpsTropGros(t *testing.T) {
	b := nouveauBanc(t, nil)
	c := b.connecte("alice", false)
	gros := `{"nom":"` + strings.Repeat("a", int(b.cfg.API.CorpsMax)+10) + `"}`
	if rec := b.requete("POST", "/api/v1/compte/profils", gros, avec(c)); rec.Code != 413 {
		t.Errorf("corps annonce : %d", rec.Code)
	}
	rec := b.requete("POST", "/api/v1/compte/profils", gros, avec(c), func(r *http.Request) { r.ContentLength = -1 })
	if rec.Code != 413 {
		t.Errorf("corps sans taille : %d %s", rec.Code, rec.Body)
	}
	if rec := b.requete("POST", "/api/v1/compte/profils", `{"nom":"x","extra":1}`, avec(c)); rec.Code != 400 {
		t.Errorf("champ inconnu : %d", rec.Code)
	}
	if rec := b.requete("POST", "/api/v1/compte/profils", `{"nom":"x"}{"nom":"y"}`, avec(c)); rec.Code != 400 {
		t.Errorf("deux documents : %d", rec.Code)
	}
}

// CA5 : le serveur porte les delais de la configuration.
func TestServeurAvecLesDelaisDeLaConfig(t *testing.T) {
	cfg := nouveauBanc(t, nil).cfg.API
	s := Serveur("127.0.0.1:0", http.NotFoundHandler(), cfg)
	if s.ReadTimeout != cfg.DelaiLecture.Std() || s.ReadHeaderTimeout != cfg.DelaiLecture.Std() || s.WriteTimeout != cfg.DelaiEcriture.Std() || s.WriteTimeout == 0 || s.ReadTimeout == 0 {
		t.Fatalf("delais : %+v", s)
	}
}

// CA6 : en-tetes de securite sur toutes les reponses, y compris les refus.
func TestEntetesDeSecurite(t *testing.T) {
	b := nouveauBanc(t, nil)
	c := b.connecte("alice", false)
	for _, rec := range []interface{ Header() http.Header }{
		b.requete("GET", "/api/v1/sante", ""), b.requete("GET", "/api/v1/chaines", ""), b.requete("GET", "/api/v1/chaines", "", avec(c)),
		b.requete("GET", "/nimporte", ""),
	} {
		h := rec.Header()
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("X-Frame-Options") != "DENY" || h.Get("Referrer-Policy") != "no-referrer" ||
			!strings.Contains(h.Get("Content-Security-Policy"), "default-src 'none'") {
			t.Errorf("en-tetes manquants : %v", h)
		}
	}
}

// CA6 : CORS ferme par defaut ; ouvert uniquement aux origines listees, jamais « * ».
func TestCORSFermeParDefaut(t *testing.T) {
	b := nouveauBanc(t, nil)
	rec := b.requete("GET", "/api/v1/sante", "", func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") })
	for k := range rec.Header() {
		if strings.HasPrefix(k, "Access-Control-") {
			t.Errorf("en-tete CORS %s envoye par defaut", k)
		}
	}
	rec = b.requete("OPTIONS", "/api/v1/chaines", "", func(r *http.Request) {
		r.Header.Set("Origin", "https://evil.example")
		r.Header.Set("Access-Control-Request-Method", "GET")
	})
	if rec.Code != 403 || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("preflight : %d", rec.Code)
	}
}

func TestCORSOriginesListees(t *testing.T) {
	b := nouveauBanc(t, func(o *Options) { o.Config.OriginesCORS = []string{"https://ui.example"} })
	rec := b.requete("GET", "/api/v1/sante", "", func(r *http.Request) { r.Header.Set("Origin", "https://ui.example") })
	if rec.Header().Get("Access-Control-Allow-Origin") != "https://ui.example" || rec.Header().Get("Access-Control-Allow-Credentials") != "true" || !strings.Contains(rec.Header().Get("Vary"), "Origin") {
		t.Errorf("origine listee : %v", rec.Header())
	}
	rec = b.requete("OPTIONS", "/api/v1/chaines", "", func(r *http.Request) {
		r.Header.Set("Origin", "https://ui.example")
		r.Header.Set("Access-Control-Request-Method", "PUT")
	})
	if rec.Code != 204 || !strings.Contains(rec.Header().Get("Access-Control-Allow-Methods"), "PUT") {
		t.Errorf("preflight : %d %v", rec.Code, rec.Header())
	}
	rec = b.requete("GET", "/api/v1/sante", "", func(r *http.Request) { r.Header.Set("Origin", "https://autre.example") })
	if rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Error("origine non listee acceptee")
	}
}

// Anti-CSRF : une ecriture venant d'une autre origine est refusee meme avec le cookie.
func TestEcritureInterSitesRefusee(t *testing.T) {
	b := nouveauBanc(t, nil)
	c := b.connecte("alice", false)
	rec := b.requete("POST", "/api/v1/compte/profils", `{"nom":"x"}`, avec(c), func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") })
	if rec.Code != 403 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	rec = b.requete("POST", "/api/v1/compte/profils", `{"nom":"x"}`, avec(c), func(r *http.Request) { r.Header.Set("Origin", "http://"+r.Host) })
	if rec.Code != 201 {
		t.Fatalf("meme origine : %d %s", rec.Code, rec.Body)
	}
}

// ---------------------------------------------------------------- comptes via l'API

func TestConnexionDeconnexionEtCompte(t *testing.T) {
	b := nouveauBanc(t, nil)
	c := b.connecte("alice", false)
	rec := b.requete("GET", "/api/v1/compte", "", avec(c))
	var cp struct {
		Session struct {
			ID, ProfilID string
			Admin        bool
		}
		Profils []map[string]string
	}
	json.Unmarshal(rec.Body.Bytes(), &cp)
	if rec.Code != 200 || len(cp.Profils) != 1 || cp.Profils[0]["nom"] != "alice" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), c.Value) {
		t.Fatal("le jeton ne doit jamais apparaitre dans une reponse")
	}
	if rec := b.requete("POST", "/api/v1/deconnexion", "", avec(c)); rec.Code != 204 {
		t.Fatalf("deconnexion : %d", rec.Code)
	}
	if rec := b.requete("GET", "/api/v1/compte", "", avec(c)); rec.Code != 401 {
		t.Fatalf("session encore valide : %d", rec.Code)
	}
	rec = b.requete("POST", "/api/v1/connexion", `{"nom":"alice","mot_de_passe":"mauvais mot de passe"}`)
	if rec.Code != 401 {
		t.Fatalf("mauvais mot de passe : %d", rec.Code)
	}
}

func TestProfilsEtMotDePasseViaAPI(t *testing.T) {
	b := nouveauBanc(t, nil)
	c := b.connecte("alice", false)
	rec := b.requete("POST", "/api/v1/compte/profils", `{"nom":"Enfants"}`, avec(c))
	var p map[string]string
	json.Unmarshal(rec.Body.Bytes(), &p)
	if rec.Code != 201 || p["id"] == "" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := b.requete("POST", "/api/v1/compte/profils", `{"nom":"enfants"}`, avec(c)); rec.Code != 409 {
		t.Errorf("doublon : %d", rec.Code)
	}
	if rec := b.requete("POST", "/api/v1/compte/profils", `{"nom":"<x>"}`, avec(c)); rec.Code != 400 {
		t.Errorf("nom invalide : %d", rec.Code)
	}
	if rec := b.requete("PUT", "/api/v1/compte/profil-actif", `{"profil_id":"`+p["id"]+`"}`, avec(c)); rec.Code != 204 {
		t.Errorf("choisir : %d", rec.Code)
	}
	if rec := b.requete("PUT", "/api/v1/compte/profil-actif", `{"profil_id":"abcdef"}`, avec(c)); rec.Code != 404 {
		t.Errorf("profil inconnu : %d", rec.Code)
	}
	if rec := b.requete("DELETE", "/api/v1/compte/profils/"+p["id"], "", avec(c)); rec.Code != 204 {
		t.Errorf("suppression : %d", rec.Code)
	}
	if rec := b.requete("DELETE", "/api/v1/compte/profils/"+p["id"], "", avec(c)); rec.Code != 404 {
		t.Errorf("suppression repetee : %d", rec.Code)
	}
	if rec := b.requete("DELETE", "/api/v1/compte/profils/..%2f..", "", avec(c)); rec.Code != 400 && rec.Code != 404 {
		t.Errorf("identifiant hostile : %d", rec.Code)
	}
	// Mot de passe
	autre := b.requete("POST", "/api/v1/connexion", fmt.Sprintf(`{"nom":"alice","mot_de_passe":%q}`, mdp)).Result().Cookies()[0]
	for corps, code := range map[string]int{
		`{"ancien":"faux faux faux","nouveau":"nouveau mot de passe"}`: 403,
		`{"ancien":"` + mdp + `","nouveau":"court"}`:                   400,
		`{"ancien":"` + mdp + `","nouveau":"nouveau mot de passe"}`:    204,
	} {
		if rec := b.requete("POST", "/api/v1/compte/mot-de-passe", corps, avec(c)); rec.Code != code {
			t.Errorf("%s : %d (attendu %d) %s", corps, rec.Code, code, rec.Body)
		}
	}
	if rec := b.requete("GET", "/api/v1/compte", "", avec(autre)); rec.Code != 401 {
		t.Errorf("l'autre session devait etre revoquee : %d", rec.Code)
	}
	if rec := b.requete("GET", "/api/v1/compte", "", avec(c)); rec.Code != 200 {
		t.Errorf("la session courante doit survivre : %d", rec.Code)
	}
}

// ---------------------------------------------------------------- live, vod, epg, admin

// Le direct est monte sous /live/ et fait lui-meme son authentification : l'API ne le
// court-circuite pas et ne l'expose jamais sans passer par lui.
func TestLiveMonteSousSonPrefixe(t *testing.T) {
	b := nouveauBanc(t, nil)
	if rec := b.requete("GET", "/api/v1/live/12.m3u8", ""); rec.Code != 401 {
		t.Fatalf("le relais refuse sans session : %d", rec.Code)
	}
	c := b.connecte("alice", false)
	if rec := b.requete("GET", "/api/v1/live/12.m3u8", "", avec(c)); rec.Code != 204 {
		t.Fatalf("le relais doit recevoir la requete : %d", rec.Code)
	}
	if len(b.live.vues) != 1 {
		t.Fatalf("vues : %v", b.live.vues)
	}
	b = nouveauBanc(t, func(o *Options) { o.Live = nil })
	if rec := b.requete("GET", "/api/v1/live/12.m3u8", ""); rec.Code != 501 {
		t.Fatalf("direct absent : %d", rec.Code)
	}
}

func TestLectureVODIdentifieLaSession(t *testing.T) {
	b := nouveauBanc(t, nil)
	c := b.connecte("alice", false)
	if rec := b.requete("GET", "/api/v1/vod/a:5", ""); rec.Code != 401 {
		t.Fatalf("sans session : %d", rec.Code)
	}
	rec := b.requete("GET", "/api/v1/vod/a:5", "", avec(c))
	if rec.Code != 200 || rec.Body.String() != "film:a:5" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if len(b.lecture.sessions) != 1 || b.lecture.sessions[0] == "" || b.lecture.sessions[0] == c.Value {
		t.Fatalf("session passee au service : %v", b.lecture.sessions)
	}
	if rec := b.requete("GET", "/api/v1/vod/a:5.mkv", "", avec(c)); rec.Code != 200 {
		t.Errorf("episode avec extension : %d", rec.Code)
	}
	if rec := b.requete("GET", "/api/v1/vod/pas-un-id", "", avec(c)); rec.Code != 400 {
		t.Errorf("identifiant invalide : %d", rec.Code)
	}
	for err, code := range map[error]int{domain.ErrNonTrouve: 404, domain.ErrPlaceIndisponible: 503, domain.ErrAmont: 502, domain.ErrNonLisible: 415} {
		b.lecture.err = err
		if rec := b.requete("GET", "/api/v1/vod/a:5", "", avec(c)); rec.Code != code {
			t.Errorf("%v : %d (attendu %d)", err, rec.Code, code)
		}
	}
	if rec := nouveauBanc(t, func(o *Options) { o.Lecture = nil }).requete("GET", "/api/v1/vod/a:5", ""); rec.Code != 401 {
		t.Errorf("sans service et sans session : %d", rec.Code)
	}
}

func TestEPG(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.remplir(1)
	c := b.connecte("alice", false)
	rec := b.requete("GET", "/api/v1/epg/chaines/a:100/maintenant", "", avec(c))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"titre":"Journal"`) || !strings.Contains(rec.Body.String(), `"suivant":null`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if rec := b.requete("GET", "/api/v1/epg/chaines/a:999/maintenant", "", avec(c)); rec.Code != 404 {
		t.Errorf("chaine inconnue : %d", rec.Code)
	}
	ok := "/api/v1/epg/grille?chaines=a:100,a:100&de=2026-01-01T10:00:00Z&a=2026-01-01T12:00:00Z"
	rec = b.requete("GET", ok, "", avec(c))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"a:100":[{"debut"`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	for _, q := range []string{
		"chaines=a:100&de=2026-01-01T12:00:00Z&a=2026-01-01T10:00:00Z", // a avant de
		"chaines=a:100&de=2026-01-01T10:00:00Z&a=2026-01-05T10:00:00Z", // fenetre trop grande
		"chaines=a:100&de=hier&a=demain", "chaines=%27;x&de=2026-01-01T10:00:00Z&a=2026-01-01T11:00:00Z",
		"de=2026-01-01T10:00:00Z&a=2026-01-01T11:00:00Z",
	} {
		if rec := b.requete("GET", "/api/v1/epg/grille?"+q, "", avec(c)); rec.Code != 400 {
			t.Errorf("%s : %d", q, rec.Code)
		}
	}
	if rec := nouveauBanc(t, func(o *Options) { o.EPG = nil }).requete("GET", "/api/v1/epg/grille", ""); rec.Code != 401 {
		t.Errorf("%d", rec.Code)
	}
	b2 := nouveauBanc(t, func(o *Options) { o.EPG = nil })
	c2 := b2.connecte("alice", false)
	if rec := b2.requete("GET", ok, "", avec(c2)); rec.Code != 501 {
		t.Errorf("epg absent : %d", rec.Code)
	}
}

func TestAdminSansGestionnaire501(t *testing.T) {
	b := nouveauBanc(t, nil)
	admin := b.connecte("root", true)
	if rec := b.requete("GET", "/api/v1/metriques", "", avec(admin)); rec.Code != 200 || rec.Body.String() != "m 1" {
		t.Errorf("metriques : %d", rec.Code)
	}
	if rec := b.requete("GET", "/api/v1/diagnostic", "", avec(admin)); rec.Code != 501 {
		t.Errorf("diagnostic non branche : %d", rec.Code)
	}
}

// ---------------------------------------------------------------- profil

func TestProfilEtatIsoleParProfilActif(t *testing.T) {
	b := nouveauBanc(t, nil)
	alice, bob := b.connecte("alice", false), b.connecte("bob", false)
	if rec := b.requete("PUT", "/api/v1/profil/favoris/a:1", "", avec(alice)); rec.Code != 204 {
		t.Fatalf("%d", rec.Code)
	}
	b.requete("PUT", "/api/v1/profil/favoris/a:1", "", avec(alice)) // idempotent
	b.requete("PUT", "/api/v1/profil/favoris/a:2", "", avec(alice))
	var p pageEtIDs
	rec := b.requete("GET", "/api/v1/profil/favoris", "", avec(alice))
	json.Unmarshal(rec.Body.Bytes(), &p)
	if len(p.Elements) != 2 || p.Elements[0] != "a:1" {
		t.Fatalf("favoris : %s", rec.Body)
	}
	rec = b.requete("GET", "/api/v1/profil/favoris", "", avec(bob))
	if !strings.Contains(rec.Body.String(), `"elements":[]`) {
		t.Fatalf("bob voit les favoris d'alice : %s", rec.Body)
	}
	if rec := b.requete("DELETE", "/api/v1/profil/favoris/a:1", "", avec(alice)); rec.Code != 204 {
		t.Errorf("retrait : %d", rec.Code)
	}
	rec = b.requete("GET", "/api/v1/profil/favoris?limite=1", "", avec(alice))
	json.Unmarshal(rec.Body.Bytes(), &p)
	if len(p.Elements) != 1 || p.Elements[0] != "a:2" {
		t.Fatalf("apres retrait : %s", rec.Body)
	}
}

type pageEtIDs struct {
	Elements []string `json:"elements"`
}

func TestProfilHistoriqueEtReglages(t *testing.T) {
	b := nouveauBanc(t, nil)
	c := b.connecte("alice", false)
	if rec := b.requete("PUT", "/api/v1/profil/historique/a:5", `{"position":120,"duree":3600}`, avec(c)); rec.Code != 204 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	rec := b.requete("GET", "/api/v1/profil/historique", "", avec(c))
	if !strings.Contains(rec.Body.String(), `"contenu":"a:5","position":120,"duree":3600,"vu":"2026-01-01T12:00:00Z"`) {
		t.Fatalf("historique : %s", rec.Body)
	}
	for _, corps := range []string{`{"position":-1}`, `{"position":10,"duree":5}`, `{"position":99999999999}`, `{}x`} {
		if rec := b.requete("PUT", "/api/v1/profil/historique/a:5", corps, avec(c)); rec.Code != 400 {
			t.Errorf("%s : %d", corps, rec.Code)
		}
	}
	if rec := b.requete("DELETE", "/api/v1/profil/historique/a:5", "", avec(c)); rec.Code != 204 {
		t.Errorf("oubli : %d", rec.Code)
	}
	// Reglages : ecriture optimiste.
	rec = b.requete("GET", "/api/v1/profil/reglages", "", avec(c))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"valeurs":{}`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	rec = b.requete("PUT", "/api/v1/profil/reglages", `{"valeurs":{"langue":"fr"},"revision":0}`, avec(c))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"revision":1`) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	rec = b.requete("PUT", "/api/v1/profil/reglages", `{"valeurs":{"langue":"en"},"revision":0}`, avec(c))
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), `"conflit"`) {
		t.Fatalf("conflit : %d %s", rec.Code, rec.Body)
	}
	gros := fmt.Sprintf(`{"valeurs":{"k":%q},"revision":1}`, strings.Repeat("x", b.cfg.Profil.ReglagesMax))
	if rec := b.requete("PUT", "/api/v1/profil/reglages", gros, avec(c)); rec.Code != 422 {
		t.Errorf("reglages trop gros : %d", rec.Code)
	}
}

func TestProfilRequisQuandAucunProfilActif(t *testing.T) {
	b := nouveauBanc(t, nil)
	c := b.connecte("alice", false)
	// Supprime le seul profil : la session n'a plus de profil actif.
	rec := b.requete("GET", "/api/v1/compte/profils", "", avec(c))
	var l struct{ Elements []map[string]string }
	json.Unmarshal(rec.Body.Bytes(), &l)
	b.requete("DELETE", "/api/v1/compte/profils/"+l.Elements[0]["id"], "", avec(c))
	rec = b.requete("GET", "/api/v1/profil/favoris", "", avec(c))
	if rec.Code != 409 || !strings.Contains(rec.Body.String(), "profil_requis") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

type etatLimite struct{ *etatFaux }

func (etatLimite) AjouterFavori(context.Context, string, domain.ContenuID) error {
	return domain.ErrLimite
}

func TestLimiteDeProfilEn422(t *testing.T) {
	b := nouveauBanc(t, func(o *Options) { o.Etat = etatLimite{nouvelEtat()} })
	c := b.connecte("alice", false)
	rec := b.requete("PUT", "/api/v1/profil/favoris/a:1", "", avec(c))
	if rec.Code != 422 || !strings.Contains(rec.Body.String(), "limite_atteinte") {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
}

func TestOptionsObligatoires(t *testing.T) {
	if _, err := Nouveau(Options{}); err == nil {
		t.Fatal("options vides acceptees")
	}
}
