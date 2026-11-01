package xtream

import (
	"context"
	"testing"

	"mytv/internal/config"
	"mytv/internal/testutil/fauxiptv"
)

// US-42 CA6 : le debit du fichier vient de info.bitrate (film : get_vod_info ; episode :
// get_series_info), en kbit/s ; 0 quand le fournisseur se tait ou que l'episode est inconnu.
func TestCA6_DebitDuFichierDepuisLesInfos(t *testing.T) {
	f := fauxiptv.Nouveau(t, fauxiptv.Options{
		InfosFilm: map[string]any{"2001": map[string]any{"info": map[string]any{"bitrate": "4400", "duration_secs": 100}},
			"2002": map[string]any{"info": []any{}}},
		InfosSerie: map[string]any{"77": map[string]any{"episodes": map[string]any{
			"1": []any{map[string]any{"id": "7701", "episode_num": 1, "info": map[string]any{"bitrate": 3200}},
				map[string]any{"id": "7702", "episode_num": 2, "info": []any{}}}}}},
	})
	c := Nouveau(config.Fournisseur{ID: "iptva", Base: f.URL(), Reseau: config.Reseau{DelaiConnexion: 2e9, DelaiEntete: 5e9}}, f.Utilisateur(), f.MotDePasse())
	defer c.Fermer()
	ctx := context.Background()
	if d, err := c.DebitFilm(ctx, "2001"); err != nil || d != 4400 {
		t.Errorf("film 2001 : %d %v", d, err)
	}
	if d, err := c.DebitFilm(ctx, "2002"); err != nil || d != 0 {
		t.Errorf("film sans info : %d %v", d, err)
	}
	if d, err := c.DebitFilm(ctx, "9999"); err != nil || d != 0 {
		t.Errorf("film inconnu : %d %v", d, err)
	}
	if d, err := c.DebitEpisode(ctx, "77", "7701"); err != nil || d != 3200 {
		t.Errorf("episode 7701 : %d %v", d, err)
	}
	if d, err := c.DebitEpisode(ctx, "77", "7702"); err != nil || d != 0 {
		t.Errorf("episode sans info : %d %v", d, err)
	}
	if d, err := c.DebitEpisode(ctx, "77", "7799"); err != nil || d != 0 {
		t.Errorf("episode inconnu : %d %v", d, err)
	}
}
