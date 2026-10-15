package xtream

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"mytv/internal/domain"
	"mytv/internal/testutil/fauxiptv"
)

func detailsFilmDe(t *testing.T, info any) (*domain.DetailsFilm, error) {
	t.Helper()
	f := fauxiptv.Nouveau(t, fauxiptv.Options{InfosFilm: map[string]any{"2001": info}})
	return clientFaux(f).DetailsFilm(context.Background(), "2001")
}

// CA1 : un get_vod_info complet donne synopsis, genre, duree, casting, realisateur,
// bande-annonce, image de fond, note et date de sortie.
func TestDetailsFilmComplet(t *testing.T) {
	d, err := detailsFilmDe(t, map[string]any{"info": map[string]any{
		"plot": "Un voleur entre dans les reves.", "genre": "Science-fiction, Thriller",
		"cast": "A, B, C", "director": "Christopher Nolan", "country": "USA",
		"releasedate": "2010-07-16", "youtube_trailer": "YoExt-gQ3dE", "rating": "8.8",
		"backdrop_path": []string{"https://img.test/fond1.jpg", "https://img.test/fond2.jpg"},
		"movie_image":   "https://img.test/affiche.jpg", "duration_secs": 8880, "duration": "02:28:00", "age": "12",
	}, "movie_data": map[string]any{"stream_id": 2001}})
	if err != nil {
		t.Fatal(err)
	}
	if d.Synopsis != "Un voleur entre dans les reves." || d.Genre != "Science-fiction, Thriller" || d.Casting != "A, B, C" ||
		d.Realisateur != "Christopher Nolan" || d.Pays != "USA" || d.Sortie != "2010-07-16" || d.Note != 8.8 || d.Age != "12" {
		t.Errorf("champs texte : %+v", d)
	}
	if d.Duree != 8880*time.Second {
		t.Errorf("duree : %v", d.Duree)
	}
	if d.BandeAnnonce != "https://www.youtube.com/watch?v=YoExt-gQ3dE" {
		t.Errorf("bande-annonce : %q", d.BandeAnnonce)
	}
	if len(d.ImageFond) != 2 || d.ImageFond[0] != "https://img.test/fond1.jpg" || d.Affiche != "https://img.test/affiche.jpg" {
		t.Errorf("images : %+v", d)
	}
}

// CA1 : JSON tolerant — champs absents ou null, nombres en chaines, backdrop_path
// en chaine, description a la place de plot, duree en texte ou en minutes.
func TestDetailsFilmTolerant(t *testing.T) {
	d, err := detailsFilmDe(t, map[string]any{"info": map[string]any{
		"description": "Synopsis de repli", "genre": nil, "cast": false, "actors": "Acteur 1, Acteur 2",
		"rating": 7.5, "backdrop_path": "https://img.test/seul.jpg", "episode_run_time": "95",
		"releaseDate": "2001-02-03", "duration": "", "director": 12345,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if d.Synopsis != "Synopsis de repli" || d.Casting != "Acteur 1, Acteur 2" || d.Genre != "" || d.Note != 7.5 ||
		d.Sortie != "2001-02-03" || d.Realisateur != "12345" || d.Duree != 95*time.Minute ||
		len(d.ImageFond) != 1 || d.ImageFond[0] != "https://img.test/seul.jpg" {
		t.Errorf("%+v", d)
	}
	// Duree « HH:MM:SS » seule.
	d, _ = detailsFilmDe(t, map[string]any{"info": map[string]any{"duration": "01:30:05"}})
	if d.Duree != 90*time.Minute+5*time.Second {
		t.Errorf("duree texte : %v", d.Duree)
	}
}

// CA1 : le fournisseur repond `[]`, `{}`, `{"info":[]}`, `false`, null quand il n'a pas de
// fiche : details vides, SANS erreur (ce n'est pas une panne).
func TestDetailsFilmSansFiche(t *testing.T) {
	for nom, v := range map[string]any{"liste": []any{}, "objet": map[string]any{}, "info vide": map[string]any{"info": []any{}},
		"faux": false, "nul": nil, "info nul": map[string]any{"info": nil}} {
		d, err := detailsFilmDe(t, v)
		if err != nil || d == nil || !d.Vide() {
			t.Errorf("%s : %+v %v", nom, d, err)
		}
	}
}

// CA1 : les valeurs dangereuses sont ecartees (adresses non http, bande-annonce hors
// YouTube, caracteres de controle, texte demesure).
func TestDetailsFilmValeursDangereuses(t *testing.T) {
	d, err := detailsFilmDe(t, map[string]any{"info": map[string]any{
		"plot": "ok\x00\x07" + strings.Repeat("é", synopsisMax+50), "youtube_trailer": "javascript:alert(1)",
		"backdrop_path": []string{"javascript:alert(1)", "data:text/html,x", "http://img.test/a b.jpg", "https://img.test/bon.jpg"},
		"movie_image":   "file:///etc/passwd",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(d.Synopsis, "\x00\x07") || len([]rune(d.Synopsis)) != synopsisMax {
		t.Errorf("synopsis non borne ou avec controles : %d", len([]rune(d.Synopsis)))
	}
	if d.BandeAnnonce != "" || d.Affiche != "" || len(d.ImageFond) != 1 || d.ImageFond[0] != "https://img.test/bon.jpg" {
		t.Errorf("%+v", d)
	}
	for entree, attendu := range map[string]string{
		"https://www.youtube.com/watch?v=YoExt-gQ3dE&t=3":       "https://www.youtube.com/watch?v=YoExt-gQ3dE",
		"https://youtu.be/YoExt-gQ3dE":                          "https://www.youtube.com/watch?v=YoExt-gQ3dE",
		"https://www.youtube.com/embed/YoExt-gQ3dE":             "https://www.youtube.com/watch?v=YoExt-gQ3dE",
		"https://evil.test/watch?v=YoExt-gQ3dE":                 "",
		"https://www.youtube.com.evil.test/watch?v=YoExt-gQ3dE": "",
		"trop court": "",
	} {
		if got := bandeAnnonce(entree); got != attendu {
			t.Errorf("bandeAnnonce(%q) = %q, attendu %q", entree, got, attendu)
		}
	}
}

// CA1 : les statuts d'erreur du fournisseur deviennent des erreurs typees, sans identifiants.
func TestDetailsFilmErreursTypees(t *testing.T) {
	for statut, attendu := range map[int]error{401: domain.ErrIdentifiants, 403: domain.ErrIdentifiants,
		404: domain.ErrIntrouvable, 458: domain.ErrRefuseIP, 502: domain.ErrAmont} {
		f := fauxiptv.Nouveau(t, fauxiptv.Options{})
		f.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleDetails, Statut: statut})
		_, err := clientFaux(f).DetailsFilm(context.Background(), "2001")
		if !errors.Is(err, attendu) {
			t.Errorf("%d : %v", statut, err)
		}
		if err != nil && strings.Contains(err.Error(), f.MotDePasse()+"&") {
			t.Errorf("identifiants dans l'erreur : %v", err)
		}
	}
	// Refus d'authentification en 200.
	f := fauxiptv.Nouveau(t, fauxiptv.Options{InfosFilm: map[string]any{"1": map[string]any{"user_info": map[string]any{"auth": 0}}}})
	if _, err := clientFaux(f).DetailsFilm(context.Background(), "1"); !errors.Is(err, domain.ErrIdentifiants) {
		t.Errorf("auth=0 : %v", err)
	}
	// Corps illisible : panne amont, pas details vides.
	f = fauxiptv.Nouveau(t, fauxiptv.Options{InfosFilm: map[string]any{"1": "<html>"}})
	if _, err := clientFaux(f).DetailsFilm(context.Background(), "1"); !errors.Is(err, domain.ErrAmont) {
		t.Errorf("illisible : %v", err)
	}
}

// CA1 : une fiche lente respecte le contexte de l'appelant.
func TestDetailsFilmLentRespecteLeContexte(t *testing.T) {
	f := fauxiptv.Nouveau(t, fauxiptv.Options{})
	f.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleDetails, Latence: 5 * time.Second})
	ctx, annuler := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer annuler()
	debut := time.Now()
	_, err := clientFaux(f).DetailsFilm(ctx, "2001")
	if err == nil || time.Since(debut) > 2*time.Second {
		t.Fatalf("%v apres %v", err, time.Since(debut))
	}
}

// CA1 : get_series_info — bloc info de la serie et details par episode, tolerant
// (info d'episode en [], backdrop_path en chaine, nombres en chaines).
func TestSaisonsEtDetailsSerie(t *testing.T) {
	f := fauxiptv.Nouveau(t, fauxiptv.Options{InfosSerie: map[string]any{"3001": map[string]any{
		"info": map[string]any{"plot": "Une famille.", "genre": "Drame", "cast": "X, Y", "director": "Z",
			"releaseDate": "2015-09-01", "rating": "8.1", "youtube_trailer": "https://www.youtube.com/watch?v=abcdefghijk",
			"backdrop_path": "https://img.test/fond.jpg", "episode_run_time": "52"},
		"episodes": map[string]any{
			"1": []map[string]any{
				{"id": "9001", "episode_num": "1", "title": "Pilote", "container_extension": "mkv", "season": 1,
					"info": map[string]any{"plot": "Le debut.", "movie_image": "https://img.test/e1.jpg", "releasedate": "2015-09-01",
						"duration_secs": 3120, "rating": "7.9"}},
				{"id": "9002", "episode_num": 2, "title": "Suite", "container_extension": "mp4", "season": 1, "info": []any{}},
			},
		},
	}}})
	ss, d, err := clientFaux(f).SaisonsEtDetails(context.Background(), "3001")
	if err != nil {
		t.Fatal(err)
	}
	if d.Synopsis != "Une famille." || d.Genre != "Drame" || d.Casting != "X, Y" || d.Realisateur != "Z" || d.Sortie != "2015-09-01" ||
		d.Note != 8.1 || d.DureeEpisode != 52*time.Minute || len(d.ImageFond) != 1 ||
		d.BandeAnnonce != "https://www.youtube.com/watch?v=abcdefghijk" {
		t.Errorf("serie : %+v", d)
	}
	if len(ss) != 1 || len(ss[0].Episodes) != 2 {
		t.Fatalf("saisons : %+v", ss)
	}
	e1, e2 := ss[0].Episodes[0], ss[0].Episodes[1]
	if e1.Synopsis != "Le debut." || e1.Image != "https://img.test/e1.jpg" || e1.Sortie != "2015-09-01" || e1.Duree != 52*time.Minute || e1.Note != 7.9 {
		t.Errorf("episode 1 : %+v", e1)
	}
	if e2.Titre != "Suite" || e2.Synopsis != "" || e2.Image != "" {
		t.Errorf("episode 2 (info []) : %+v", e2)
	}
}

// CA1 : un `info` d'episode en [] (ou false) ne doit plus faire echouer TOUTE la liste
// (c'etait le cas avec l'ancien decodage strict).
func TestSaisonsInfoEpisodeVideNeCasseRien(t *testing.T) {
	ss, err := saisonsDe(t, map[string]any{"episodes": map[string]any{"1": []map[string]any{
		{"id": "1", "episode_num": 1, "title": "A", "info": []any{}},
		{"id": "2", "episode_num": 2, "title": "B", "info": false},
		{"id": "3", "episode_num": 3, "title": "C", "info": "texte"},
	}}})
	if err != nil || len(ss) != 1 || len(ss[0].Episodes) != 3 {
		t.Fatalf("%+v %v", ss, err)
	}
}

// CA1 : serie sans fiche (`[]`) = liste vide, details vides, sans erreur.
func TestSaisonsEtDetailsSerieSansFiche(t *testing.T) {
	f := fauxiptv.Nouveau(t, fauxiptv.Options{})
	ss, d, err := clientFaux(f).SaisonsEtDetails(context.Background(), "3001")
	if err != nil || len(ss) != 0 || d == nil || !d.Vide() {
		t.Fatalf("%+v %+v %v", ss, d, err)
	}
}
