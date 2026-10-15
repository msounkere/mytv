package fauxiptv

import "strconv"

// CatalogueExemple fabrique un catalogue simple et propre : nChaines chaines
// (numeros 1001...), nFilms films (2001...), nSeries series (3001...), reparties
// dans 2 categories par type. Pour tester la tolerance au JSON bizarre, construire
// Options.Catalogue a la main.
func CatalogueExemple(nChaines, nFilms, nSeries int) map[string]any {
	cats := func(prefixe string) []map[string]any {
		return []map[string]any{
			{"category_id": prefixe + "1", "category_name": "Premiere " + prefixe, "parent_id": 0},
			{"category_id": prefixe + "2", "category_name": "Seconde " + prefixe, "parent_id": 0},
		}
	}
	chaines := make([]map[string]any, 0, nChaines)
	for i := 0; i < nChaines; i++ {
		chaines = append(chaines, map[string]any{
			"num": i + 1, "name": "Chaine " + strconv.Itoa(i+1), "stream_type": "live",
			"stream_id": 1001 + i, "stream_icon": "http://logo.test/" + strconv.Itoa(i) + ".png",
			"epg_channel_id": "chaine" + strconv.Itoa(i+1) + ".fr", "added": "1700000000",
			"category_id": "l" + strconv.Itoa(1+i%2),
		})
	}
	films := make([]map[string]any, 0, nFilms)
	for i := 0; i < nFilms; i++ {
		films = append(films, map[string]any{
			"num": i + 1, "name": "Film " + strconv.Itoa(i+1), "stream_type": "movie",
			"stream_id": 2001 + i, "stream_icon": "http://img.test/f" + strconv.Itoa(i) + ".jpg",
			"rating": "7.5", "added": "1700000000", "category_id": "v" + strconv.Itoa(1+i%2),
			"container_extension": "mkv",
		})
	}
	series := make([]map[string]any, 0, nSeries)
	for i := 0; i < nSeries; i++ {
		series = append(series, map[string]any{
			"num": i + 1, "name": "Serie " + strconv.Itoa(i+1), "series_id": 3001 + i,
			"cover": "http://img.test/s" + strconv.Itoa(i) + ".jpg", "rating": "8",
			"releaseDate": "2020-01-01", "last_modified": "1710000000", "category_id": "s" + strconv.Itoa(1+i%2),
		})
	}
	return map[string]any{
		"get_live_categories": cats("l"), "get_live_streams": chaines,
		"get_vod_categories": cats("v"), "get_vod_streams": films,
		"get_series_categories": cats("s"), "get_series": series,
	}
}
