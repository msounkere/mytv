package xtream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"mytv/internal/domain"
)

// Debit du fichier d'un film ou d'un episode : `info.bitrate` de get_vod_info /
// get_series_info, en kbit/s (US-42 CA6 : la sonde de disponibilite compare le debit
// mesure a celui du fichier). 0 = le fournisseur ne le dit pas. Appels d'API, aucun flux,
// aucun bail ; l'appelant en borne la frequence (resultat garde 10 min).

// DebitFilm lit get_vod_info et rend info.bitrate (kbit/s), 0 si inconnu.
func (c *Client) DebitFilm(ctx context.Context, numero string) (int, error) {
	corps, err := c.lireCorps(ctx, "get_vod_info", url.Values{"vod_id": {numero}}, infoFilmMax)
	if err != nil {
		return 0, err
	}
	var d struct {
		Info infoJSON `json:"info"`
	}
	b := bytes.TrimSpace(corps)
	if len(b) > 0 && b[0] == '{' {
		if refusAuth(b) {
			return 0, domain.ErrIdentifiants
		}
		if err := json.Unmarshal(b, &d); err != nil {
			return 0, fmt.Errorf("%w : get_vod_info illisible", domain.ErrAmont)
		}
	}
	return max(int(d.Info.Bitrate), 0), nil
}

// DebitEpisode lit get_series_info et rend info.bitrate de l'episode `numeroEpisode`
// (identifiant de l'episode chez le fournisseur, sans extension), 0 si inconnu.
func (c *Client) DebitEpisode(ctx context.Context, numeroSerie, numeroEpisode string) (int, error) {
	corps, err := c.lireCorps(ctx, "get_series_info", url.Values{"series_id": {numeroSerie}}, infoSerieMax)
	if err != nil {
		return 0, err
	}
	var d struct {
		Episodes json.RawMessage `json:"episodes"`
	}
	b := bytes.TrimSpace(corps)
	if len(b) == 0 || b[0] != '{' {
		return 0, nil
	}
	if refusAuth(b) {
		return 0, domain.ErrIdentifiants
	}
	if err := json.Unmarshal(b, &d); err != nil {
		return 0, fmt.Errorf("%w : get_series_info illisible", domain.ErrAmont)
	}
	brut := bytes.TrimSpace(d.Episodes)
	var listes [][]episodeJSON
	switch {
	case len(brut) == 0:
		return 0, nil
	case brut[0] == '{':
		var m map[string][]episodeJSON
		if err := json.Unmarshal(brut, &m); err != nil {
			return 0, nil
		}
		for _, l := range m {
			listes = append(listes, l)
		}
	case brut[0] == '[':
		if err := json.Unmarshal(brut, &listes); err != nil {
			return 0, nil
		}
	}
	for _, l := range listes {
		for _, e := range l {
			if e.ID.String() == numeroEpisode {
				return max(int(e.Info.Bitrate), 0), nil
			}
		}
	}
	return 0, nil
}
