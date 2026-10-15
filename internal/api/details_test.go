package api

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"mytv/internal/config"
	"mytv/internal/domain"
	"mytv/internal/metadonnees"
	"mytv/internal/stockage"
	"mytv/internal/testutil/fauxiptv"
	"mytv/internal/xtream"
)

// bancDetails : API + VRAI client Xtream + VRAI stockage SQLite des details + VRAI
// service de metadonnees, contre le faux fournisseur (aucun reseau reel).
type bancDetails struct {
	*banc
	faux   *fauxiptv.Faux
	cookie *http.Cookie
}

func nouveauBancDetails(t *testing.T, mod func(*config.Metadonnees), avecService bool, opts ...func(*Options)) *bancDetails {
	t.Helper()
	faux := fauxiptv.Nouveau(t, fauxiptv.Options{
		InfosFilm: map[string]any{
			"2001": map[string]any{"info": map[string]any{"plot": "Un synopsis.", "genre": "Drame", "cast": "A, B", "director": "R",
				"releasedate": "2010-07-16", "youtube_trailer": "YoExt-gQ3dE", "rating": "8.1", "duration_secs": 5400,
				"backdrop_path": []string{"https://img.test/fond.jpg"}}},
			"2002": map[string]any{"info": map[string]any{"plot": "Autre synopsis."}},
		},
		InfosSerie: map[string]any{"3001": map[string]any{
			"info": map[string]any{"plot": "Resume serie", "genre": "Comedie", "episode_run_time": "30"},
			"episodes": map[string]any{"1": []map[string]any{{"id": "9001", "episode_num": 1, "title": "Pilote",
				"container_extension": "mkv", "info": map[string]any{"plot": "Resume ep", "movie_image": "https://img.test/e.jpg", "duration_secs": 1500}}}},
		}},
	})
	st, err := stockage.Ouvrir(context.Background(), filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Fermer() })
	cfg := config.Defauts().Metadonnees
	if mod != nil {
		mod(&cfg)
	}
	cl := xtream.Nouveau(config.Fournisseur{ID: "a", Base: faux.URL(), Reseau: config.Reseau{
		DelaiConnexion: config.Duree(2 * time.Second), DelaiEntete: config.Duree(10 * time.Second)}}, faux.Utilisateur(), faux.MotDePasse())
	t.Cleanup(cl.Fermer)
	var svc Details
	if avecService {
		s, err := metadonnees.Nouveau(metadonnees.Options{Config: cfg, Fournisseurs: map[string]domain.FournisseurDetails{"a": cl}, Stockage: st})
		if err != nil {
			t.Fatal(err)
		}
		svc = s
	}
	b := nouveauBanc(t, func(o *Options) {
		o.Details = svc
		o.VOD = map[string]domain.FournisseurVOD{"a": cl}
		for _, f := range opts {
			f(o)
		}
	})
	b.st.films = []domain.Film{{ID: "a:2001", FournisseurID: "a", Numero: "2001", Nom: "Inception", Image: "http://img.test/liste.jpg", Extension: "mkv", Note: 7.5},
		{ID: "a:2002", FournisseurID: "a", Numero: "2002", Nom: "Autre"}}
	b.st.series = []domain.SerieTV{{ID: "a:s3001", FournisseurID: "a", Numero: "3001", Nom: "Ma serie"}}
	return &bancDetails{banc: b, faux: faux, cookie: b.connecte("alice", false)}
}

func (b *bancDetails) get(chemin string, mod ...func(*http.Request)) (int, map[string]any, map[string][]string) {
	b.t.Helper()
	rec := b.requete("GET", chemin, "", append([]func(*http.Request){avec(b.cookie)}, mod...)...)
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, m, rec.Header()
}

func (b *bancDetails) appels(action string) int { return b.faux.Compteurs().ParAction[action] }

// CA3 : la fiche d'un film porte les details, recuperes a l'ouverture et mis en cache
// (un seul appel au fournisseur), avec un ETag sur le corps (304 si rien n'a change).
func TestFilmAvecDetailsEtCache(t *testing.T) {
	b := nouveauBancDetails(t, nil, true)
	code, m, h := b.get("/api/v1/films/a:2001")
	d, _ := m["details"].(map[string]any)
	if code != 200 || m["details_statut"] != "ok" || d["synopsis"] != "Un synopsis." || d["genre"] != "Drame" || d["casting"] != "A, B" ||
		d["realisateur"] != "R" || d["sortie"] != "2010-07-16" || d["duree"].(float64) != 5400 || d["note"].(float64) != 8.1 ||
		d["bande_annonce"] != "https://www.youtube.com/watch?v=YoExt-gQ3dE" || d["image_fond"] != "https://img.test/fond.jpg" {
		t.Fatalf("%d %+v", code, m)
	}
	if m["nom"] != "Inception" || m["extension"] != "mkv" || m["lecture"] != "/api/v1/vod/a:2001" {
		t.Errorf("champs de la liste absents : %+v", m)
	}
	tag := h["Etag"][0]
	code, _, _ = b.get("/api/v1/films/a:2001", func(r *http.Request) { r.Header.Set("If-None-Match", tag) })
	if code != http.StatusNotModified {
		t.Errorf("304 attendu : %d", code)
	}
	b.get("/api/v1/films/a:2001")
	if n := b.appels("get_vod_info"); n != 1 {
		t.Fatalf("%d appels au fournisseur, attendu 1", n)
	}
}

// CA3 : ?details=non ne declenche aucun appel ; valeur inconnue = 400.
func TestFilmDetailsNonEtParametreInvalide(t *testing.T) {
	b := nouveauBancDetails(t, nil, true)
	code, m, _ := b.get("/api/v1/films/a:2001?details=non")
	if code != 200 || m["details_statut"] != "non_demande" || m["details"] != nil || m["nom"] != "Inception" {
		t.Fatalf("%d %+v", code, m)
	}
	if b.appels("get_vod_info") != 0 {
		t.Fatal("details=non a appele le fournisseur")
	}
	// Apres une ouverture normale, details=non sert le cache.
	b.get("/api/v1/films/a:2001")
	if _, m, _ := b.get("/api/v1/films/a:2001?details=non"); m["details_statut"] != "ok" || m["details"] == nil {
		t.Errorf("details=non devrait servir le cache : %+v", m)
	}
	if code, _, _ := b.get("/api/v1/films/a:2001?details=peutetre"); code != 400 {
		t.Errorf("details invalide : %d", code)
	}
	if b.appels("get_vod_info") != 1 {
		t.Errorf("%d appels", b.appels("get_vod_info"))
	}
}

// CA3/CA4 : AUCUN appel au fournisseur pour afficher des listes ou une recherche.
func TestListesNeDeclenchentAucunAppel(t *testing.T) {
	b := nouveauBancDetails(t, nil, true)
	for _, c := range []string{"/api/v1/films", "/api/v1/series", "/api/v1/recherche?q=Incep", "/api/v1/chaines", "/api/v1/films?limite=1&decalage=1"} {
		if code, _, _ := b.get(c); code != 200 {
			t.Errorf("%s : %d", c, code)
		}
	}
	if b.faux.Compteurs().API != 0 {
		t.Fatalf("%d appels a l'API du fournisseur pour des listes", b.faux.Compteurs().API)
	}
}

// CA3/CA6 : 8 ouvertures simultanees de la meme fiche = 1 appel.
func TestFicheOuverteHuitFoisUnAppel(t *testing.T) {
	b := nouveauBancDetails(t, nil, true)
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleDetails, Latence: 250 * time.Millisecond})
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if code, m, _ := b.get("/api/v1/films/a:2001"); code != 200 || m["details_statut"] != "ok" {
				t.Errorf("%d %v", code, m["details_statut"])
			}
		}()
	}
	wg.Wait()
	if n := b.appels("get_vod_info"); n != 1 {
		t.Fatalf("%d appels", n)
	}
}

// CA4 : fournisseur en erreur = la fiche repond QUAND MEME, avec les champs de la liste
// (degradation propre), pour chaque type de panne.
func TestFilmDegradationQuandLeFournisseurEchoue(t *testing.T) {
	for nom, panne := range map[string]fauxiptv.Panne{
		"502": {Statut: 502}, "404": {Statut: 404}, "458": {Statut: 458}, "401": {Statut: 401},
		"lent": {Latence: 3 * time.Second},
	} {
		t.Run(nom, func(t *testing.T) {
			b := nouveauBancDetails(t, func(c *config.Metadonnees) { c.Delai = config.Duree(300 * time.Millisecond) }, true)
			panne.Cible = fauxiptv.CibleDetails
			b.faux.Programmer(panne)
			debut := time.Now()
			code, m, _ := b.get("/api/v1/films/a:2001")
			if code != 200 || m["nom"] != "Inception" || m["details"] != nil || time.Since(debut) > 2*time.Second {
				t.Fatalf("%d %+v apres %v", code, m, time.Since(debut))
			}
			if st := m["details_statut"]; st != "indisponible" && st != "absent" {
				t.Errorf("statut %v", st)
			}
		})
	}
}

// CA4 : apres un 458, plus aucun appel pour aucun contenu (coupe-circuit), mais les
// fiches repondent toujours.
func TestCoupeCircuitSurLAPI(t *testing.T) {
	b := nouveauBancDetails(t, nil, true)
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleDetails, Statut: 458, Fois: 1})
	b.get("/api/v1/films/a:2001")
	for _, c := range []string{"/api/v1/films/a:2002", "/api/v1/series/a:s3001"} {
		if code, m, _ := b.get(c); code != 200 || m["details_statut"] != "indisponible" {
			t.Errorf("%s : %d %v", c, code, m["details_statut"])
		}
	}
	if n := b.faux.Compteurs().API; n != 1 {
		t.Fatalf("%d appels, attendu 1 (le 458)", n)
	}
}

// CA3 : sans service de details (fonction absente), la fiche garde sa forme d'avant.
func TestFilmSansServiceDeDetails(t *testing.T) {
	b := nouveauBancDetails(t, nil, false)
	code, m, _ := b.get("/api/v1/films/a:2001")
	if code != 200 || m["details"] != nil || m["details_statut"] != nil || m["nom"] != "Inception" {
		t.Fatalf("%d %+v", code, m)
	}
	if code, _, _ := b.get("/api/v1/films/a:9999"); code != 404 {
		t.Errorf("film inconnu : %d", code)
	}
}

// CA3 : un identifiant inconnu du catalogue n'atteint JAMAIS le fournisseur.
func TestIdentifiantInconnuNAtteintPasLeFournisseur(t *testing.T) {
	b := nouveauBancDetails(t, nil, true)
	for _, c := range []string{"/api/v1/films/a:9999", "/api/v1/series/a:s9999", "/api/v1/films/a:2001%2F..%2Fx"} {
		if code, _, _ := b.get(c); code != 404 && code != 400 {
			t.Errorf("%s : %d", c, code)
		}
	}
	if b.faux.Compteurs().API != 0 {
		t.Fatal("appel au fournisseur pour un identifiant inconnu")
	}
}

// CA3 : la serie porte ses details, ses episodes les leurs, pour UN seul get_series_info.
func TestSerieDetailsEtEpisodes(t *testing.T) {
	b := nouveauBancDetails(t, nil, true)
	code, m, _ := b.get("/api/v1/series/a:s3001")
	d, _ := m["details"].(map[string]any)
	if code != 200 || m["details_statut"] != "ok" || d["synopsis"] != "Resume serie" || d["genre"] != "Comedie" || d["duree_episode"].(float64) != 1800 {
		t.Fatalf("%d %+v", code, m)
	}
	code, m, _ = b.get("/api/v1/series/a:s3001/saisons")
	el, _ := m["elements"].([]any)
	if code != 200 || len(el) != 1 {
		t.Fatalf("%d %+v", code, m)
	}
	ep := el[0].(map[string]any)["episodes"].([]any)[0].(map[string]any)
	if ep["synopsis"] != "Resume ep" || ep["image"] != "https://img.test/e.jpg" || ep["duree"].(float64) != 1500 || ep["titre"] != "Pilote" {
		t.Fatalf("episode : %+v", ep)
	}
	if n := b.appels("get_series_info"); n != 1 {
		t.Fatalf("%d appels get_series_info pour la fiche serie + ses saisons, attendu 1", n)
	}
}

// CA3 : l'erreur du fournisseur sur les saisons reste une erreur (pas de saisons = rien a montrer).
func TestSaisonsEnPanneRepondErreur(t *testing.T) {
	b := nouveauBancDetails(t, nil, true)
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleDetails, Statut: 502})
	if code, _, _ := b.get("/api/v1/series/a:s3001/saisons"); code != 502 {
		t.Fatalf("%d", code)
	}
}

// CA3 (contrat) : toutes les cles emises par /films/{id}, /series/{id} et les episodes
// existent dans les schemas du contrat (le test de routes ne voit pas les champs).
func TestFichesConformesAuContrat(t *testing.T) {
	c, _ := lireContrat(t)
	proprietes := func(schema string) map[string]bool {
		node, ok := c.Components.Schemas[schema]
		if !ok {
			t.Fatalf("schema %s absent du contrat", schema)
		}
		var s struct {
			Properties map[string]yaml.Node `yaml:"properties"`
		}
		if err := node.Decode(&s); err != nil {
			t.Fatal(err)
		}
		out := map[string]bool{}
		for k := range s.Properties {
			out[k] = true
		}
		return out
	}
	verifier := func(quoi string, m map[string]any, schema string) {
		t.Helper()
		p := proprietes(schema)
		for k := range m {
			if !p[k] {
				t.Errorf("%s : champ %q emis mais absent du schema %s", quoi, k, schema)
			}
		}
	}
	b := nouveauBancDetails(t, nil, true)
	_, film, _ := b.get("/api/v1/films/a:2001")
	verifier("film", film, "FilmDetail")
	verifier("film.details", film["details"].(map[string]any), "DetailsCommuns")
	_, serie, _ := b.get("/api/v1/series/a:s3001")
	verifier("serie", serie, "SerieDetail")
	verifier("serie.details", serie["details"].(map[string]any), "DetailsCommuns")
	_, ss, _ := b.get("/api/v1/series/a:s3001/saisons")
	verifier("episode", ss["elements"].([]any)[0].(map[string]any)["episodes"].([]any)[0].(map[string]any), "Episode")
	// Les statuts emis sont ceux du contrat.
	var enum struct {
		Enum []string `yaml:"enum"`
	}
	node := c.Components.Schemas["DetailsStatut"]
	if err := node.Decode(&enum); err != nil {
		t.Fatal(err)
	}
	for _, st := range []metadonnees.Statut{metadonnees.Ok, metadonnees.Perime, metadonnees.Indisponible, metadonnees.Absent, metadonnees.NonDemande} {
		trouve := false
		for _, e := range enum.Enum {
			trouve = trouve || e == string(st)
		}
		if !trouve {
			t.Errorf("statut %q absent du contrat", st)
		}
	}
}

// US-23 x US-28 : les images des details (fond, affiche) et des episodes passent par le
// proxy d'images comme celles des listes ; aucune adresse d'origine ne sort.
func TestImagesDesDetailsReecritesParLeProxy(t *testing.T) {
	f := nouveauFauxImages(t)
	svc := serviceImages(t, f, nil)
	b := nouveauBancDetails(t, nil, true, func(o *Options) { o.Images = svc })
	b.faux.DefinirInfoFilm("2001", map[string]any{"info": map[string]any{
		"plot": "x", "backdrop_path": []string{f.srv.URL + "/fond.jpg"}, "movie_image": f.srv.URL + "/affiche.jpg"}})
	b.faux.DefinirInfoSerie("3001", map[string]any{"info": map[string]any{"plot": "s", "cover_big": f.srv.URL + "/c.jpg"},
		"episodes": map[string]any{"1": []map[string]any{{"id": "9001", "episode_num": 1, "title": "E", "info": map[string]any{"movie_image": f.srv.URL + "/e.jpg"}}}}})
	for _, chemin := range []string{"/api/v1/films/a:2001", "/api/v1/series/a:s3001", "/api/v1/series/a:s3001/saisons"} {
		rec := b.requete("GET", chemin, "", avec(b.cookie))
		if rec.Code != 200 {
			t.Fatalf("%s : %d %s", chemin, rec.Code, rec.Body)
		}
		if strings.Contains(rec.Body.String(), f.srv.URL) || strings.Contains(rec.Body.String(), "127.0.0.1") {
			t.Errorf("%s : adresse d'origine dans la reponse : %s", chemin, rec.Body)
		}
	}
	_, m, _ := b.get("/api/v1/films/a:2001")
	d := m["details"].(map[string]any)
	if !strings.HasPrefix(d["image_fond"].(string), "/api/v1/image/") || !strings.HasPrefix(d["affiche"].(string), "/api/v1/image/") {
		t.Errorf("details : %+v", d)
	}
	if !strings.HasPrefix(m["image"].(string), "/api/v1/image/") {
		t.Errorf("image de la liste : %v", m["image"])
	}
	_, ss, _ := b.get("/api/v1/series/a:s3001/saisons")
	ep := ss["elements"].([]any)[0].(map[string]any)["episodes"].([]any)[0].(map[string]any)
	if !strings.HasPrefix(ep["image"].(string), "/api/v1/image/") {
		t.Errorf("episode : %+v", ep)
	}
}
