package xtream

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mytv/internal/config"
	"mytv/internal/domain"
	"mytv/internal/testutil/fauxiptv"
)

func reseauTest() config.Reseau {
	return config.Reseau{
		DelaiConnexion: config.Duree(2 * time.Second),
		DelaiEntete:    config.Duree(2 * time.Second),
	}
}

func clientPour(base, user, mdp string, opts ...Option) *Client {
	return Nouveau(config.Fournisseur{ID: "iptva", Base: base, Reseau: reseauTest()}, user, mdp, opts...)
}

func clientFaux(f *fauxiptv.Faux, opts ...Option) *Client {
	return clientPour(f.URL(), f.Utilisateur(), f.MotDePasse(), opts...)
}

func donnees(t *testing.T, nom string) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/" + nom)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

type brut []byte

func (b brut) MarshalJSON() ([]byte, error) { return b, nil }

// CA1
func TestCatalogueProduitUnInstantaneAvecContenuID(t *testing.T) {
	f := fauxiptv.Nouveau(t, fauxiptv.Options{Catalogue: fauxiptv.CatalogueExemple(4, 3, 2)})
	s, err := clientFaux(f).Catalogue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.FournisseurID != "iptva" || s.Pris.IsZero() {
		t.Errorf("instantane mal renseigne : %+v", s)
	}
	if len(s.Chaines) != 4 || len(s.Films) != 3 || len(s.Series) != 2 || len(s.Categories) != 6 {
		t.Fatalf("volumes : %d chaines, %d films, %d series, %d categories", len(s.Chaines), len(s.Films), len(s.Series), len(s.Categories))
	}
	c := s.Chaines[0]
	if c.ID != "iptva:1001" || c.Numero != "1001" || c.Nom != "Chaine 1" || c.EpgID != "chaine1.fr" || c.CategorieID != "l1" || c.NumeroChaine != 1 {
		t.Errorf("chaine : %+v", c)
	}
	// US-16 : date d'ajout des chaines (added) et des series (last_modified).
	if c.Ajoute.Unix() != 1700000000 || s.Series[0].Ajoute.Unix() != 1710000000 {
		t.Errorf("ajoute : chaine %v, serie %v", c.Ajoute, s.Series[0].Ajoute)
	}
	if s.Films[1].ID != "iptva:2002" || s.Series[0].ID != "iptva:s3001" {
		t.Errorf("identifiants film/serie : %v %v", s.Films[1].ID, s.Series[0].ID)
	}
	if s.Films[0].Extension != "mkv" || s.Films[0].Note != 7.5 || s.Films[0].Ajoute.Unix() != 1700000000 {
		t.Errorf("film : %+v", s.Films[0])
	}
	// Les six actions ont ete appelees, une fois chacune.
	for _, a := range []string{"get_live_categories", "get_live_streams", "get_vod_categories", "get_vod_streams", "get_series_categories", "get_series"} {
		if n := f.Compteurs().ParAction[a]; n != 1 {
			t.Errorf("action %s appelee %d fois", a, n)
		}
	}
	// Les categories gardent l'ordre du fournisseur, par type.
	for i, c := range s.Categories {
		if c.FournisseurID != "iptva" || c.Ordre != i%2 {
			t.Errorf("categorie %d : %+v", i, c)
		}
	}
}

// CA2
func TestCatalogueToleranteAuxJSONVaries(t *testing.T) {
	f := fauxiptv.Nouveau(t, fauxiptv.Options{Catalogue: map[string]any{
		"get_live_categories": brut(donnees(t, "categories_variees.json")),
		"get_live_streams":    brut(donnees(t, "chaines_variees.json")),
		"get_vod_streams":     brut(donnees(t, "films_varies.json")),
		"get_series":          brut(donnees(t, "series_variees.json")),
		"get_vod_categories":  nil, // null JSON
	}})
	s, err := clientFaux(f).Catalogue(context.Background())
	if err != nil {
		t.Fatalf("un JSON bizarre ne doit pas faire echouer le catalogue : %v", err)
	}

	// Categories : l'entree sans identifiant est ignoree, le nom null devient vide.
	if len(s.Categories) != 3 || s.Categories[1].ID != "11" || s.Categories[2].Nom != "" {
		t.Errorf("categories : %+v", s.Categories)
	}

	// Chaines : 5 gardees sur 7 (deux sans stream_id).
	if len(s.Chaines) != 5 {
		t.Fatalf("%d chaines, attendu 5 : %+v", len(s.Chaines), s.Chaines)
	}
	if c := s.Chaines[0]; c.Nom != "TF1 HD" || c.Numero != "845410" || c.CategorieID != "10" {
		t.Errorf("chaine nominale : %+v", c)
	}
	if c := s.Chaines[1]; c.NumeroChaine != 2 || c.Numero != "845411" || c.CategorieID != "11" || c.Logo != "" || c.EpgID != "" {
		t.Errorf("nombres en chaines / null : %+v", c)
	}
	if c := s.Chaines[2]; c.Numero != "845412" || c.CategorieID != "" {
		t.Errorf("identifiant flottant : %+v", c)
	}
	if c := s.Chaines[3]; c.Nom != "404" || c.Logo != "" {
		t.Errorf("nom numerique / logo booleen : %+v", c)
	}
	for i, c := range s.Chaines {
		if c.Ordre != i {
			t.Errorf("ordre de la chaine %d : %d", i, c.Ordre)
		}
	}

	// Films.
	if len(s.Films) != 5 {
		t.Fatalf("%d films", len(s.Films))
	}
	b := s.Films[1]
	if b.Note != 6 || b.Ajoute.Unix() != 1650000000 || b.CategorieID != "21" || b.Annee != "2021" {
		t.Errorf("film B : %+v", b)
	}
	if c := s.Films[2]; c.Note != 0 || !c.Ajoute.IsZero() || c.Extension != "" || c.Annee != "1999" {
		t.Errorf("film C (null) : %+v", c)
	}
	if d, e := s.Films[3], s.Films[4]; d.Annee != "" || !d.Ajoute.IsZero() || e.Note != 0 || !e.Ajoute.IsZero() {
		t.Errorf("films D/E (valeurs illisibles) : %+v %+v", d, e)
	}

	// Series : l'annee vient de « year » nombre ou de « releaseDate ».
	if len(s.Series) != 2 || s.Series[0].Annee != "2020" || s.Series[1].Annee != "2018" || s.Series[1].Note != 9 || s.Series[1].Image != "" {
		t.Errorf("series : %+v", s.Series)
	}
}

// CA2 : une reponse qui n'est pas une liste est une erreur, jamais un catalogue vide
// (qui effacerait le bon catalogue en base).
func TestCatalogueRefuseUneReponseQuiNEstPasUneListe(t *testing.T) {
	for nom, corps := range map[string]string{
		"objet":     `{"autre":1}`,
		"faux":      `false`,
		"tronque":   `[{"stream_id":1},{"stream_id":2`,
		"illisible": `<html>`,
		"vide":      ``,
	} {
		t.Run(nom, func(t *testing.T) {
			f := fauxiptv.Nouveau(t, fauxiptv.Options{Catalogue: map[string]any{"get_live_categories": brut(corps)}})
			s, err := clientFaux(f).Catalogue(context.Background())
			if !errors.Is(err, domain.ErrAmont) || s != nil {
				t.Errorf("attendu ErrAmont et pas d'instantane, obtenu %v / %v", err, s)
			}
		})
	}
}

// CA3 : un 200 portant « auth: 0 » est un refus d'identifiants.
func TestReponse200AvecAuthZeroDonneErrIdentifiants(t *testing.T) {
	f := fauxiptv.Nouveau(t, fauxiptv.Options{Catalogue: map[string]any{"get_live_categories": brut(`{"user_info":{"auth":0}}`)}})
	if _, err := clientFaux(f).Catalogue(context.Background()); !errors.Is(err, domain.ErrIdentifiants) {
		t.Errorf("attendu ErrIdentifiants, obtenu %v", err)
	}
}

// CA4 : jamais d'autre schema que http(s) (pas de relais ouvert vers file://).
func TestRequeteRefuseLesSchemasAutresQueHTTP(t *testing.T) {
	cl := clientPour("http://x", "u", "p")
	for _, u := range []string{"file:///etc/passwd", "ftp://h/x", "gopher://h/"} {
		if rep, err := cl.Requete(context.Background(), u, nil); err == nil || rep != nil {
			t.Errorf("%s accepte", u)
		}
	}
}

// CA3
func TestStatutsTraduitsEnErreursTypees(t *testing.T) {
	cas := []struct {
		statut int
		erreur error
	}{
		{401, domain.ErrIdentifiants}, {403, domain.ErrIdentifiants},
		{404, domain.ErrIntrouvable},
		{407, domain.ErrRefuseConnexions},
		{458, domain.ErrRefuseIP},
		{500, domain.ErrAmont}, {502, domain.ErrAmont}, {503, domain.ErrAmont}, {405, domain.ErrAmont},
	}
	for _, c := range cas {
		t.Run(fmt.Sprint(c.statut), func(t *testing.T) {
			f := fauxiptv.Nouveau(t, fauxiptv.Options{})
			f.Programmer(fauxiptv.Panne{Cible: fauxiptv.CiblePlaylist, Statut: c.statut})
			cl := clientFaux(f)
			rep, err := cl.Requete(context.Background(), cl.URLLive("5"), nil)
			if rep != nil {
				rep.Body.Close()
				t.Fatal("la reponse doit etre nil sur une erreur")
			}
			if !errors.Is(err, c.erreur) {
				t.Errorf("statut %d : erreur %v, attendu %v", c.statut, err, c.erreur)
			}
		})
	}
}

// CA3 : la meme traduction vaut pour le catalogue.
func TestCatalogueTraduitLes458(t *testing.T) {
	f := fauxiptv.Nouveau(t, fauxiptv.Options{})
	f.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleAPI, Statut: 458})
	if _, err := clientFaux(f).Catalogue(context.Background()); !errors.Is(err, domain.ErrRefuseIP) {
		t.Errorf("attendu ErrRefuseIP, obtenu %v", err)
	}
}

func TestMauvaisMotDePasseDonneErrIdentifiants(t *testing.T) {
	f := fauxiptv.Nouveau(t, fauxiptv.Options{})
	if _, err := clientPour(f.URL(), "u", "faux").Catalogue(context.Background()); !errors.Is(err, domain.ErrIdentifiants) {
		t.Errorf("attendu ErrIdentifiants, obtenu %v", err)
	}
}

// CA3 : delai d'attente de l'en-tete depasse = erreur amont (pas une annulation).
func TestDelaiEnteteDepasseDonneErrAmont(t *testing.T) {
	f := fauxiptv.Nouveau(t, fauxiptv.Options{})
	f.Programmer(fauxiptv.Panne{Cible: fauxiptv.CiblePlaylist, Latence: 3 * time.Second})
	cl := Nouveau(config.Fournisseur{ID: "iptva", Base: f.URL(), Reseau: config.Reseau{DelaiEntete: config.Duree(150 * time.Millisecond)}}, "u", "p")
	debut := time.Now()
	_, err := cl.Requete(context.Background(), cl.URLLive("5"), nil)
	if !errors.Is(err, domain.ErrAmont) || time.Since(debut) > 2*time.Second {
		t.Errorf("erreur %v apres %v", err, time.Since(debut))
	}
}

// CA3 : l'annulation voulue par l'appelant reste une annulation.
func TestAnnulationDuContexteNEstPasUneErreurAmont(t *testing.T) {
	f := fauxiptv.Nouveau(t, fauxiptv.Options{})
	f.Programmer(fauxiptv.Panne{Cible: fauxiptv.CiblePlaylist, Latence: 3 * time.Second})
	ctx, annuler := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer annuler()
	cl := clientFaux(f)
	_, err := cl.Requete(ctx, cl.URLLive("5"), nil)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, domain.ErrAmont) {
		t.Errorf("attendu DeadlineExceeded seul, obtenu %v", err)
	}
}

// CA4 : redirection vers un autre noeud suivie.
func TestRequeteSuitLesRedirections(t *testing.T) {
	f := fauxiptv.Nouveau(t, fauxiptv.Options{RedirigerPlaylist: true})
	cl := clientFaux(f)
	rep, err := cl.Requete(context.Background(), cl.URLLive("5"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer rep.Body.Close()
	corps, _ := io.ReadAll(rep.Body)
	if !strings.HasPrefix(string(corps), "#EXTM3U") || !strings.HasPrefix(rep.Request.URL.Path, "/noeud/") {
		t.Errorf("redirection non suivie (%s) : %q", rep.Request.URL.Path, corps)
	}
}

// CA4 : jusqu'a 10 redirections, pas une de plus.
func TestRequeteLimiteLesRedirectionsADix(t *testing.T) {
	var vues atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := vues.Add(1)
		if r.URL.Query().Get("fin") != "" && int(n) > 10 {
			io.WriteString(w, "arrive")
			return
		}
		http.Redirect(w, r, fmt.Sprintf("%s/saut%d?%s", srv.URL, n, r.URL.RawQuery), http.StatusFound)
	}))
	defer srv.Close()
	cl := clientPour(srv.URL, "u", "p")

	rep, err := cl.Requete(context.Background(), srv.URL+"/?fin=1", nil)
	if err != nil {
		t.Fatalf("10 redirections doivent passer : %v", err)
	}
	rep.Body.Close()

	vues.Store(0)
	_, err = cl.Requete(context.Background(), srv.URL+"/", nil) // boucle infinie
	if !errors.Is(err, domain.ErrAmont) {
		t.Errorf("la boucle doit donner ErrAmont, obtenu %v", err)
	}
	if got := vues.Load(); got != redirectionsMax+1 {
		t.Errorf("%d requetes envoyees, attendu %d", got, redirectionsMax+1)
	}
}

// CA4 : en-tetes Chrome par defaut, user_agent configure, en-tetes de l'appelant prioritaires.
func TestRequetePoseLesEntetesParDefaut(t *testing.T) {
	f := fauxiptv.Nouveau(t, fauxiptv.Options{})
	cl := clientFaux(f)
	h := http.Header{}
	h.Set("X-Test", "1")
	rep, err := cl.Requete(context.Background(), cl.URLLive("5"), h)
	if err != nil {
		t.Fatal(err)
	}
	rep.Body.Close()
	tr := f.Traces()
	e := tr[len(tr)-1].Entetes
	if !strings.Contains(e.Get("User-Agent"), "Chrome/") || e.Get("Accept") != "*/*" || e.Get("X-Test") != "1" {
		t.Errorf("en-tetes : %v", e)
	}
	if !strings.Contains(e.Get("Accept-Encoding"), "gzip") {
		t.Errorf("Accept-Encoding = %q, gzip attendu pour les playlists", e.Get("Accept-Encoding"))
	}

	cl2 := Nouveau(config.Fournisseur{ID: "iptva", Base: f.URL(), UserAgent: "MonAgent/1.0", Reseau: reseauTest()}, "u", "p")
	rep, _ = cl2.Requete(context.Background(), cl2.URLLive("5"), nil)
	rep.Body.Close()
	tr = f.Traces()
	if got := tr[len(tr)-1].Entetes.Get("User-Agent"); got != "MonAgent/1.0" {
		t.Errorf("user_agent configure ignore : %q", got)
	}

	// Un Range exige le contenu original : pas de compression.
	h = http.Header{"Range": {"bytes=0-9"}}
	rep, _ = cl.Requete(context.Background(), f.URL()+"/hls/a/5_1.ts", h)
	defer rep.Body.Close()
	tr = f.Traces()
	if got := tr[len(tr)-1].Entetes.Get("Accept-Encoding"); got != "identity" {
		t.Errorf("Accept-Encoding avec Range = %q, identity attendu", got)
	}
	if rep.StatusCode != 206 {
		t.Errorf("Range non transmis : %d", rep.StatusCode)
	}
}

// CA4 : le corps est rendu en flux ; Requete revient avant la fin du corps.
func TestRequeteNeLitPasLeCorpsEnEntier(t *testing.T) {
	liberer := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000000")
		w.Write(bytes.Repeat([]byte("x"), 100))
		w.(http.Flusher).Flush()
		select { // le reste du film n'arrive jamais tant que le test ne le libere pas
		case <-liberer:
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	defer close(liberer)

	cl := clientPour(srv.URL, "u", "p")
	ctx, annuler := context.WithTimeout(context.Background(), 3*time.Second)
	defer annuler()
	rep, err := cl.Requete(ctx, srv.URL+"/film.mkv", nil)
	if err != nil {
		t.Fatalf("Requete aurait du rendre la main des les en-tetes : %v", err)
	}
	defer rep.Body.Close()
	tampon := make([]byte, 50)
	if _, err := io.ReadFull(rep.Body, tampon); err != nil {
		t.Fatal(err)
	}
}

// CA5 : le mot de passe n'apparait ni dans les erreurs ni dans les journaux, quelle que soit la panne.
func TestAucunIdentifiantDansErreursNiJournaux(t *testing.T) {
	const user, mdp = "utilisateur-secret", "Mdp/S3cret?#&x"
	var journal bytes.Buffer
	log := slog.New(slog.NewTextHandler(&journal, &slog.HandlerOptions{Level: slog.LevelDebug}))

	var messages []string
	note := func(err error) {
		if err != nil {
			messages = append(messages, err.Error())
		}
	}

	f := fauxiptv.Nouveau(t, fauxiptv.Options{Utilisateur: user, MotDePasse: mdp, Catalogue: fauxiptv.CatalogueExemple(2, 1, 1)})
	cl := clientFaux(f, AvecJournal(log))
	ctx := context.Background()

	// Succes, puis chaque famille d'echec.
	if _, err := cl.Catalogue(ctx); err != nil {
		t.Fatal(err)
	}
	for _, statut := range []int{401, 404, 407, 458, 500} {
		f.Programmer(fauxiptv.Panne{Cible: fauxiptv.CiblePlaylist, Statut: statut, Fois: 1})
		_, err := cl.Requete(ctx, cl.URLLive("5"), nil)
		note(err)
		f.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleAPI, Statut: statut, Fois: 1})
		_, err = cl.Catalogue(ctx)
		note(err)
	}
	_, err := clientPour(f.URL(), user, "mauvais").Catalogue(ctx)
	note(err)
	f.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleAPI, CorpsVide: true, Fois: 1})
	_, err = cl.Catalogue(ctx)
	note(err)

	// Serveur injoignable : net/http met l'URL (donc le mot de passe) dans l'erreur.
	mort := httptest.NewServer(http.NotFoundHandler())
	adresse := mort.URL
	mort.Close()
	mortCl := clientPour(adresse, user, mdp, AvecJournal(log))
	_, err = mortCl.Catalogue(ctx)
	note(err)
	_, err = mortCl.Requete(ctx, mortCl.URLLive("5"), nil)
	note(err)

	if len(messages) < 10 {
		t.Fatalf("scenario trop pauvre : %d erreurs collectees", len(messages))
	}
	tout := strings.Join(messages, "\n") + "\n" + journal.String()
	for _, secret := range []string{mdp, url.PathEscape(mdp), url.QueryEscape(mdp), user} {
		if strings.Contains(tout, secret) {
			t.Errorf("le secret %q apparait dans les erreurs ou le journal :\n%s", secret, tout)
		}
	}
	if journal.Len() == 0 {
		t.Error("le journal est vide : le test ne prouve rien")
	}
}

// CA6
func TestURLLiveEchappeLesIdentifiants(t *testing.T) {
	cl := clientPour("http://hote.test:8080/", "jean dupont", "p@ss/w?rd#1")
	want := "http://hote.test:8080/live/jean%20dupont/p@ss%2Fw%3Frd%231/845410.m3u8"
	if got := cl.URLLive("845410"); got != want {
		t.Errorf("URLLive = %q, attendu %q", got, want)
	}
	// Le numero lui-meme est echappe : il ne doit pas pouvoir sortir du chemin.
	if got := cl.URLLive("../x"); strings.Contains(got, "/../") {
		t.Errorf("numero non echappe : %q", got)
	}
}

// CA6 : l'URL produite est reellement jouable avec des identifiants a caracteres speciaux.
func TestURLLiveEstAccepteeParLeFournisseur(t *testing.T) {
	f := fauxiptv.Nouveau(t, fauxiptv.Options{Utilisateur: "jean dupont", MotDePasse: "p@ss/w?rd#1"})
	cl := clientFaux(f)
	rep, err := cl.Requete(context.Background(), cl.URLLive("77"), nil)
	if err != nil {
		t.Fatal(err)
	}
	rep.Body.Close()
}

func TestIdentifiantDuFournisseur(t *testing.T) {
	var _ domain.Fournisseur = clientPour("http://x", "u", "p")
	if clientPour("http://x", "u", "p").ID() != "iptva" {
		t.Error("ID() doit rendre l'identifiant de configuration")
	}
}

// CA4 : `reseau.delai_connexion` borne la poignee de main TLS. Un serveur qui accepte
// la connexion TCP mais ne dit rien ne doit pas bloquer l'appelant.
func TestDelaiConnexionBorneLaPoigneeDeMain(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	fin := make(chan struct{})
	defer close(fin)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { <-fin; c.Close() }()
		}
	}()
	cl := Nouveau(config.Fournisseur{ID: "iptva", Base: "https://" + ln.Addr().String(),
		Reseau: config.Reseau{DelaiConnexion: config.Duree(150 * time.Millisecond), DelaiEntete: config.Duree(time.Minute)}}, "u", "p")
	debut := time.Now()
	_, err = cl.Requete(context.Background(), cl.base+"/x", nil)
	if !errors.Is(err, domain.ErrAmont) {
		t.Fatalf("erreur = %v, attendu ErrAmont", err)
	}
	if d := time.Since(debut); d > 3*time.Second {
		t.Errorf("la poignee de main n'a pas ete bornee : %v", d)
	}
}

// ---------------------------------------------------------------- VOD (US-10)

// CA7 (US-10) : URL des films et episodes, avec echappement.
func TestURLFilmEtEpisode(t *testing.T) {
	cl := clientPour("http://f.test/", "us er", "m/p?#")
	if got, want := cl.URLFilm("2001", "mkv"), "http://f.test/movie/us%20er/m%2Fp%3F%23/2001.mkv"; got != want {
		t.Errorf("film = %q, attendu %q", got, want)
	}
	if got, want := cl.URLEpisode("77", "mp4"), "http://f.test/series/us%20er/m%2Fp%3F%23/77.mp4"; got != want {
		t.Errorf("episode = %q, attendu %q", got, want)
	}
	// Une extension venue du catalogue ne peut pas casser l'URL.
	for _, mauvaise := range []string{"", "../x", "mkv?x=1", "a b", "toolongext", "m/kv"} {
		if got := cl.URLFilm("1", mauvaise); !strings.HasSuffix(got, "/1.mp4") {
			t.Errorf("extension %q => %q", mauvaise, got)
		}
	}
	if got := cl.URLFilm("1", ".MKV"); !strings.HasSuffix(got, "/1.mkv") {
		t.Errorf("extension .MKV => %q", got)
	}
}

func saisonsDe(t *testing.T, infos any) ([]domain.Saison, error) {
	t.Helper()
	f := fauxiptv.Nouveau(t, fauxiptv.Options{Catalogue: map[string]any{"get_series_info": infos}})
	return clientFaux(f).Saisons(context.Background(), "3001")
}

// CA7 (US-10) : saisons et episodes, JSON tolerant (objet par saison).
func TestSaisonsObjetParSaison(t *testing.T) {
	ss, err := saisonsDe(t, map[string]any{
		"info": map[string]any{"name": "Serie"},
		"episodes": map[string]any{
			"2": []map[string]any{{"id": 502, "episode_num": "1", "title": "S2E1", "container_extension": "mkv", "info": map[string]any{"duration_secs": "1500"}}},
			"1": []map[string]any{
				{"id": "102", "episode_num": 2, "title": "S1E2", "container_extension": "mp4", "info": map[string]any{"duration": "00:45:30"}},
				{"id": "101", "episode_num": "1", "title": "S1E1", "container_extension": nil, "info": map[string]any{"duration_secs": 2700}},
				{"title": "sans id"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 2 || ss[0].Numero != 1 || ss[1].Numero != 2 {
		t.Fatalf("saisons = %+v", ss)
	}
	e := ss[0].Episodes
	if len(e) != 2 || e[0].ID != "iptva:101.mp4" || e[1].ID != "iptva:102.mp4" || e[0].Numero != 1 {
		t.Fatalf("episodes saison 1 = %+v (tri par numero, episode sans id ignore)", e)
	}
	if e[0].Duree != 45*time.Minute || e[1].Duree != 45*time.Minute+30*time.Second || e[1].Extension != "mp4" {
		t.Errorf("durees/extension = %+v", e)
	}
	if ss[1].Episodes[0].ID != "iptva:502.mkv" || ss[1].Episodes[0].Duree != 25*time.Minute {
		t.Errorf("saison 2 = %+v", ss[1])
	}
}

// CA7 (US-10) : formes bizarres du fournisseur.
func TestSaisonsFormesBizarres(t *testing.T) {
	// tableau de tableaux
	ss, err := saisonsDe(t, map[string]any{"episodes": [][]map[string]any{
		{{"id": 1, "episode_num": 1, "title": "a"}}, {{"id": 2, "episode_num": 1, "title": "b", "season": 4}},
	}})
	if err != nil || len(ss) != 2 || ss[0].Numero != 1 || ss[1].Numero != 4 {
		t.Errorf("tableau : %+v %v", ss, err)
	}
	// aucune saison : [] / {} / null / absent
	for _, v := range []any{[]any{}, map[string]any{}, nil} {
		ss, err = saisonsDe(t, map[string]any{"episodes": v})
		if err != nil || len(ss) != 0 {
			t.Errorf("episodes=%v : %+v %v", v, ss, err)
		}
	}
	if ss, err = saisonsDe(t, map[string]any{"info": 1}); err != nil || len(ss) != 0 {
		t.Errorf("sans cle episodes : %+v %v", ss, err)
	}
	// duree illisible, titre null
	ss, err = saisonsDe(t, map[string]any{"episodes": map[string]any{"1": []map[string]any{{"id": "9", "title": nil, "info": map[string]any{"duration": "n'importe quoi"}}}}})
	if err != nil || len(ss) != 1 || ss[0].Episodes[0].Duree != 0 {
		t.Errorf("duree illisible : %+v %v", ss, err)
	}
	// reponse inexploitable : ErrAmont
	if _, err = saisonsDe(t, "du texte"); !errors.Is(err, domain.ErrAmont) {
		t.Errorf("texte : %v", err)
	}
	if _, err = saisonsDe(t, map[string]any{"episodes": "oups"}); !errors.Is(err, domain.ErrAmont) {
		t.Errorf("episodes texte : %v", err)
	}
}

// Les statuts d'erreur sont traduits, et le mot de passe n'apparait jamais.
func TestSaisonsErreursTypees(t *testing.T) {
	f := fauxiptv.Nouveau(t, fauxiptv.Options{MotDePasse: "tressecret"})
	f.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleAPI, Statut: 407, Fois: 1})
	cl := clientFaux(f)
	if _, err := cl.Saisons(context.Background(), "1"); !errors.Is(err, domain.ErrRefuseConnexions) {
		t.Errorf("407 : %v", err)
	}
	mauvais := clientPour(f.URL(), f.Utilisateur(), "faux")
	_, err := mauvais.Saisons(context.Background(), "1")
	if !errors.Is(err, domain.ErrIdentifiants) || strings.Contains(err.Error(), "faux") {
		t.Errorf("identifiants : %v", err)
	}
	ctx, annule := context.WithCancel(context.Background())
	annule()
	if _, err := cl.Saisons(ctx, "1"); !errors.Is(err, context.Canceled) {
		t.Errorf("contexte annule : %v", err)
	}
}

// US-10 CA9 : une redirection vers un autre schema (file://, ftp://) est refusee.
func TestRedirectionVersUnAutreSchemaRefusee(t *testing.T) {
	for _, cible := range []string{"file:///etc/passwd", "ftp://h.test/x"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, cible, http.StatusFound)
		}))
		cl := clientPour(srv.URL, "u", "p")
		rep, err := cl.Requete(context.Background(), srv.URL+"/x", nil)
		srv.Close()
		if rep != nil {
			rep.Body.Close()
		}
		if !errors.Is(err, domain.ErrAmont) {
			t.Errorf("redirection vers %s : %v", cible, err)
		}
	}
}

// US-10 (revue) : le client est une SourceEPG ; identifiants echappes, jamais dans une erreur.
func TestURLEPGEchappeEtSansFuite(t *testing.T) {
	cl := clientPour("http://f.test/", "us er", "m/p&q=1#")
	var src domain.SourceEPG = cl
	if got, want := src.URLEPG(), "http://f.test/xmltv.php?password=m%2Fp%26q%3D1%23&username=us+er"; got != want {
		t.Errorf("URLEPG = %q, attendu %q", got, want)
	}
	f := fauxiptv.Nouveau(t, fauxiptv.Options{MotDePasse: "tressecret"})
	cl = clientFaux(f)
	f.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleTout, Statut: 502})
	_, err := cl.Requete(context.Background(), cl.URLEPG(), nil)
	if err == nil || strings.Contains(err.Error(), "tressecret") {
		t.Errorf("erreur = %v", err)
	}
}

// US-16b : series_id et stream_id sont deux espaces de numeros distincts : une serie et
// un film de meme numero ont des ContenuID differents, et Numero reste celui du fournisseur.
func TestSerieEtFilmDeMemeNumeroOntDesIdentifiantsDistincts(t *testing.T) {
	f := fauxiptv.Nouveau(t, fauxiptv.Options{Catalogue: map[string]any{
		"get_vod_streams": []map[string]any{{"name": "Film", "stream_id": 77, "container_extension": "mp4"}},
		"get_series":      []map[string]any{{"name": "Serie", "series_id": "77"}},
	}})
	s, err := clientFaux(f).Catalogue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Films) != 1 || len(s.Series) != 1 {
		t.Fatalf("%d films, %d series", len(s.Films), len(s.Series))
	}
	if s.Films[0].ID != "iptva:77" || s.Series[0].ID != "iptva:s77" {
		t.Errorf("identifiants : %s / %s", s.Films[0].ID, s.Series[0].ID)
	}
	if s.Films[0].Numero != "77" || s.Series[0].Numero != "77" {
		t.Errorf("Numero doit rester celui du fournisseur : %q / %q", s.Films[0].Numero, s.Series[0].Numero)
	}
}
