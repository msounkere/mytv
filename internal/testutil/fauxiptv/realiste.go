package fauxiptv

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Le catalogue « realiste » imite la forme des vrais catalogues Xtream sans en
// contenir aucun : noms prefixes par le pays (« FR| TF1 HD »), categories par
// pays et par genre, qualites dans le nom, annees et notes variees, extensions
// de conteneur melangees. Il sert aux tests de volume (>= 2000 chaines, 5000
// films, 500 series) et aux facettes de l'interface. Tout est DETERMINISTE
// (aucune horloge, aucun hasard) : deux executions donnent le meme catalogue.

type pays struct{ code, nom string }

var paysDeLaFabrique = []pays{{"FR", "France"}, {"BE", "Belgique"}, {"UK", "Royaume-Uni"}, {"ES", "Espagne"}, {"IT", "Italie"}, {"DE", "Allemagne"}, {"PT", "Portugal"}, {"AR", "Arabe"}}

var genresChaines = []string{"GENERALISTE", "SPORT", "CINEMA", "INFO", "ENFANTS", "DOCUMENTAIRES", "MUSIQUE"}
var genresFilms = []string{"ACTION", "COMEDIE", "DRAME", "THRILLER", "ANIMATION", "SCIENCE-FICTION", "HORREUR", "ROMANCE"}
var genresSeries = []string{"POLICIER", "COMEDIE", "DRAME", "FANTASTIQUE", "ANIMATION", "DOCUMENTAIRE"}
var qualites = []string{"HD", "FHD", "4K", "SD", "HEVC"}
var extensions = []string{"mkv", "mp4", "mp4", "avi", "mkv", "mp4"}
var motsA = []string{"Le Dernier", "La Nuit", "Les Ombres", "Un Ete", "Retour a", "L'Ile", "Le Secret", "Les Enfants de", "Mission", "La Route", "Voyage vers", "Le Silence"}
var motsB = []string{"du Nord", "d'Argent", "Oublie", "de Minuit", "du Desert", "Interdit", "des Pierres", "Perdu", "de Cristal", "Sauvage", "Rouge", "Eternel"}

// melange : suite deterministe (congruence lineaire) pour varier sans hasard.
func melange(i, sel int) int {
	return int((uint64(i)*2654435761 + uint64(sel)*40503 + 12345) >> 7 % 1000003)
}

func categoriesRealistes(prefixe string, genres []string) (cats []map[string]any, ids []string) {
	n := 0
	for _, p := range paysDeLaFabrique {
		for _, g := range genres {
			n++
			id := prefixe + strconv.Itoa(n)
			cats = append(cats, map[string]any{"category_id": id, "category_name": p.code + "| " + g, "parent_id": 0})
			ids = append(ids, id)
		}
	}
	return
}

// CatalogueRealiste fabrique nChaines chaines (numeros 1001...), nFilms films
// (200001...) et nSeries series (200001... : MEME espace de numeros que les films, comme le vrai Xtream). Les images pointent sur <base>/img/<n>.gif
// (base = adresse du faux fournisseur lui-meme) (jamais un hote externe). Les chaines declarent
// un epg_channel_id « chaine<N>.fr », celui de EPGRealiste.
func CatalogueRealiste(base string, nChaines, nFilms, nSeries int) map[string]any {
	catL, idsL := categoriesRealistes("l", genresChaines)
	catV, idsV := categoriesRealistes("v", genresFilms)
	catS, idsS := categoriesRealistes("s", genresSeries)

	chaines := make([]map[string]any, 0, nChaines)
	for i := 0; i < nChaines; i++ {
		p := paysDeLaFabrique[melange(i, 1)%len(paysDeLaFabrique)]
		q := qualites[melange(i, 2)%len(qualites)]
		chaines = append(chaines, map[string]any{
			"num": i + 1, "name": fmt.Sprintf("%s| Chaine %d %s", p.code, i+1, q), "stream_type": "live",
			"stream_id": 1001 + i, "stream_icon": base + "/img/c" + strconv.Itoa(i%50) + ".gif",
			"epg_channel_id": "chaine" + strconv.Itoa(i+1) + ".fr", "added": "1700000000",
			"category_id": idsL[melange(i, 3)%len(idsL)],
		})
	}
	films := make([]map[string]any, 0, nFilms)
	for i := 0; i < nFilms; i++ {
		p := paysDeLaFabrique[melange(i, 4)%len(paysDeLaFabrique)]
		annee := 1980 + melange(i, 5)%46
		note := float64(melange(i, 6)%90+10) / 10 // 1.0 a 9.9
		nom := fmt.Sprintf("%s %s %s (%d)", p.code+"|", motsA[melange(i, 7)%len(motsA)], motsB[melange(i, 8)%len(motsB)], annee)
		if melange(i, 9)%4 == 0 {
			nom += " " + qualites[melange(i, 10)%len(qualites)]
		}
		films = append(films, map[string]any{
			"num": i + 1, "name": nom, "stream_type": "movie", "stream_id": 200001 + i,
			"stream_icon": base + "/img/f" + strconv.Itoa(i%50) + ".gif", "rating": strconv.FormatFloat(note, 'f', 1, 64),
			"added": strconv.Itoa(1600000000 + melange(i, 11)%100000000), "year": strconv.Itoa(annee),
			"category_id": idsV[melange(i, 12)%len(idsV)], "container_extension": extensions[melange(i, 13)%len(extensions)],
		})
	}
	series := make([]map[string]any, 0, nSeries)
	for i := 0; i < nSeries; i++ {
		p := paysDeLaFabrique[melange(i, 14)%len(paysDeLaFabrique)]
		annee := 1995 + melange(i, 15)%31
		series = append(series, map[string]any{
			"num": i + 1, "name": fmt.Sprintf("%s| %s %s", p.code, motsA[melange(i, 16)%len(motsA)], motsB[melange(i, 17)%len(motsB)]),
			"series_id": 200001 + i, "cover": base + "/img/s" + strconv.Itoa(i%50) + ".gif",
			"rating":        strconv.FormatFloat(float64(melange(i, 18)%90+10)/10, 'f', 1, 64),
			"releaseDate":   fmt.Sprintf("%d-%02d-01", annee, 1+melange(i, 19)%12),
			"last_modified": strconv.Itoa(1600000000 + melange(i, 26)%100000000),
			"category_id":   idsS[melange(i, 20)%len(idsS)],
		})
	}
	return map[string]any{
		"get_live_categories": catL, "get_live_streams": chaines,
		"get_vod_categories": catV, "get_vod_streams": films,
		"get_series_categories": catS, "get_series": series,
	}
}

// EPGRealiste fabrique un guide XMLTV : pour chacune des nChaines premieres
// chaines, des programmes de 30 a 120 minutes couvrant [debut, debut+duree[.
// Les dates portent un decalage explicite (UTC).
func EPGRealiste(nChaines int, debut time.Time, duree time.Duration) []byte {
	debut = debut.UTC().Truncate(time.Hour)
	fin := debut.Add(duree)
	var b strings.Builder
	b.WriteString("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<tv generator-info-name=\"fauxiptv\">\n")
	for i := 0; i < nChaines; i++ {
		fmt.Fprintf(&b, "<channel id=\"chaine%d.fr\"><display-name>Chaine %d</display-name></channel>\n", i+1, i+1)
	}
	const f = "20060102150405 +0000"
	for i := 0; i < nChaines; i++ {
		t := debut.Add(-time.Duration(melange(i, 21)%60) * time.Minute) // decale d'une chaine a l'autre
		for k := 0; t.Before(fin); k++ {
			d := time.Duration(30+30*(melange(i*1000+k, 22)%4)) * time.Minute
			titre := fmt.Sprintf("%s %s", motsA[melange(i+k, 23)%len(motsA)], motsB[melange(i*7+k, 24)%len(motsB)])
			fmt.Fprintf(&b, "<programme start=%q stop=%q channel=\"chaine%d.fr\"><title lang=\"fr\">%s</title><desc lang=\"fr\">Programme %d de la chaine %d.</desc><category lang=\"fr\">%s</category></programme>\n",
				t.Format(f), t.Add(d).Format(f), i+1, xmlEchappe(titre), k+1, i+1, genresChaines[melange(i, 25)%len(genresChaines)])
			t = t.Add(d)
		}
	}
	b.WriteString("</tv>\n")
	return []byte(b.String())
}

func xmlEchappe(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", "'", "&apos;", `"`, "&quot;").Replace(s)
}

// InfoFilmRealiste fabrique les reponses de get_vod_info du catalogue realiste. Les cas
// sont deterministes (rang i = numero - 200001) pour que les tests de l'interface
// sachent ou chercher :
//
//	i%10 == 7  : pas de fiche (`[]`)         i%10 == 8 : HTTP 404
//	i%10 == 9  : fiche partielle (synopsis)  i%50 == 11 : fiche complete mais LENTE (1,2 s)
//	autres     : fiche complete
func InfoFilmRealiste(base string) func(id string) (any, bool) {
	return func(id string) (any, bool) {
		n, err := strconv.Atoi(id)
		if err != nil || n < 200001 {
			return nil, false
		}
		i := n - 200001
		switch i % 10 {
		case 7:
			return []any{}, true
		case 8:
			return StatutHTTP(404), true
		case 9:
			return map[string]any{"info": map[string]any{"plot": fmt.Sprintf("Synopsis partiel du film %d.", n)}}, true
		}
		if i%50 == 11 {
			time.Sleep(1200 * time.Millisecond)
		}
		return map[string]any{"info": map[string]any{
			"plot":  fmt.Sprintf("Synopsis du film %d. %s %s, une histoire de famille et de secrets que rien ne laissait prevoir.", n, motsA[melange(i, 7)%len(motsA)], motsB[melange(i, 8)%len(motsB)]),
			"genre": genresFilms[melange(i, 12)%len(genresFilms)] + ", " + genresFilms[melange(i, 13)%len(genresFilms)],
			"cast":  "Alice Martin, Bruno Durand, Claire Petit", "director": "Denis Lefevre", "country": "France",
			"releasedate": fmt.Sprintf("%d-%02d-15", 1980+melange(i, 5)%46, 1+melange(i, 19)%12), "rating": strconv.FormatFloat(float64(melange(i, 6)%90+10)/10, 'f', 1, 64),
			"youtube_trailer": "dQw4w9WgXcQ", "duration_secs": 4800 + 60*(melange(i, 30)%60), "age": "12",
			"backdrop_path": []string{base + "/img/b" + strconv.Itoa(i%50) + ".gif"}, "movie_image": base + "/img/f" + strconv.Itoa(i%50) + ".gif",
		}}, true
	}
}

// InfoSerieRealiste fabrique get_series_info du catalogue realiste (memes conventions :
// i%10 == 7 sans fiche, 8 = 404, 9 = info partielle). Les series ont 2 saisons de 3 episodes
// (identifiants d'episode « <numero>0<saison><episode> »).
func InfoSerieRealiste(base string) func(id string) (any, bool) {
	return func(id string) (any, bool) {
		n, err := strconv.Atoi(id)
		if err != nil || n < 200001 {
			return nil, false
		}
		i := n - 200001
		switch i % 10 {
		case 7:
			return []any{}, true
		case 8:
			return StatutHTTP(404), true
		}
		episodes := map[string]any{}
		for s := 1; s <= 2; s++ {
			var liste []map[string]any
			for e := 1; e <= 3; e++ {
				liste = append(liste, map[string]any{
					"id": fmt.Sprintf("%d0%d%d", n, s, e), "episode_num": e, "season": s, "title": fmt.Sprintf("Episode %d de la saison %d", e, s),
					"container_extension": "mp4",
					"info": map[string]any{"plot": fmt.Sprintf("Resume de l'episode %d (saison %d).", e, s), "movie_image": base + "/img/e" + strconv.Itoa(e) + ".gif",
						"releasedate": fmt.Sprintf("2020-%02d-0%d", s, e), "duration_secs": 1500 + 60*e, "rating": "7.5"},
				})
			}
			episodes[strconv.Itoa(s)] = liste
		}
		info := map[string]any{"plot": fmt.Sprintf("Resume de la serie %d : deux saisons de secrets de famille.", n)}
		if i%10 != 9 {
			info = map[string]any{"plot": info["plot"], "genre": genresSeries[melange(i, 20)%len(genresSeries)], "cast": "Alice Martin, Bruno Durand",
				"director": "Denis Lefevre", "releaseDate": "2020-01-01", "rating": "8.2", "youtube_trailer": "https://www.youtube.com/watch?v=dQw4w9WgXcQ",
				"backdrop_path": []string{base + "/img/b" + strconv.Itoa(i%50) + ".gif"}, "episode_run_time": "26"}
		}
		return map[string]any{"info": info, "episodes": episodes}, true
	}
}
