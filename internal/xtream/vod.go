package xtream

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"mytv/internal/domain"
)

var _ domain.FournisseurVOD = (*Client)(nil)

// infoSerieMax borne la reponse de get_series_info (les plus grosses series
// pesent quelques centaines de Kio).
const infoSerieMax = 16 << 20

// extensionParDefaut : le fournisseur sert le meme fichier sous n'importe quelle
// extension connue ; mp4 est celle que tout lecteur comprend.
const extensionParDefaut = "mp4"

var reExtension = regexp.MustCompile(`^[a-zA-Z0-9]{1,5}$`)

func extensionSure(ext string) string {
	ext = strings.TrimPrefix(strings.TrimSpace(ext), ".")
	if !reExtension.MatchString(ext) {
		return extensionParDefaut
	}
	return strings.ToLower(ext)
}

// URLFilm : <base>/movie/<u>/<p>/<numero>.<ext>. Les identifiants et le numero
// sont echappes ; l'extension, qui vient du catalogue, est reduite a des
// caracteres alphanumeriques.
func (c *Client) URLFilm(numero, extension string) string {
	return c.urlMedia("movie", numero, extension)
}

// URLEpisode : <base>/series/<u>/<p>/<numero>.<ext>.
func (c *Client) URLEpisode(numero, extension string) string {
	return c.urlMedia("series", numero, extension)
}

func (c *Client) urlMedia(genre, numero, extension string) string {
	return fmt.Sprintf("%s/%s/%s/%s/%s.%s", c.base, genre,
		url.PathEscape(c.utilisateur), url.PathEscape(c.motDePasse),
		url.PathEscape(numero), extensionSure(extension))
}

type episodeJSON struct {
	ID        texte  `json:"id"`
	Numero    entier `json:"episode_num"`
	Saison    entier `json:"season"`
	Titre     texte  `json:"title"`
	Extension texte  `json:"container_extension"`
	Info      struct {
		DureeSecondes entier `json:"duration_secs"`
		Duree         texte  `json:"duration"`
	} `json:"info"`
}

func (e episodeJSON) duree() time.Duration {
	if e.Info.DureeSecondes > 0 {
		return time.Duration(e.Info.DureeSecondes) * time.Second
	}
	// « 00:45:12 » ou « 45:12 »
	total, n := int64(0), 0
	for _, p := range strings.Split(string(e.Info.Duree), ":") {
		v, err := strconv.ParseInt(p, 10, 64)
		if err != nil || v < 0 {
			return 0
		}
		total = total*60 + v
		n++
	}
	if n < 2 || n > 3 {
		return 0
	}
	return time.Duration(total) * time.Second
}

// Saisons liste les saisons et episodes d'une serie (get_series_info). Le
// fournisseur ecrit « episodes » tantot en objet {"1":[...],"2":[...]}, tantot
// en tableau de tableaux, parfois en [] quand il n'y en a pas ; les nombres sont
// des chaines ou non. Un episode sans identifiant est ignore (injouable).
func (c *Client) Saisons(ctx context.Context, numeroSerie string) ([]domain.Saison, error) {
	q := url.Values{"username": {c.utilisateur}, "password": {c.motDePasse},
		"action": {"get_series_info"}, "series_id": {numeroSerie}}
	rep, err := c.Requete(ctx, c.base+"/player_api.php?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	defer rep.Body.Close()
	corps, err := io.ReadAll(io.LimitReader(rep.Body, infoSerieMax+1))
	if err != nil {
		return nil, c.erreurReseau(ctx, err)
	}
	if len(corps) > infoSerieMax {
		return nil, fmt.Errorf("%w : get_series_info trop volumineux", domain.ErrAmont)
	}
	var d struct {
		Episodes json.RawMessage `json:"episodes"`
	}
	if err := json.Unmarshal(corps, &d); err != nil {
		return nil, fmt.Errorf("%w : get_series_info illisible", domain.ErrAmont)
	}
	parSaison := map[int][]episodeJSON{}
	ajoute := func(saison int, liste []episodeJSON) {
		for _, e := range liste {
			s := saison
			if e.Saison > 0 {
				s = int(e.Saison)
			}
			parSaison[s] = append(parSaison[s], e)
		}
	}
	brut := strings.TrimSpace(string(d.Episodes))
	switch {
	case brut == "" || brut == "null" || brut == "[]" || brut == "{}" || brut == "false":
	case brut[0] == '{':
		var m map[string][]episodeJSON
		if err := json.Unmarshal(d.Episodes, &m); err != nil {
			return nil, fmt.Errorf("%w : episodes illisibles", domain.ErrAmont)
		}
		for k, liste := range m {
			n, err := strconv.Atoi(k)
			if err != nil {
				n = 0
			}
			ajoute(n, liste)
		}
	case brut[0] == '[':
		var l [][]episodeJSON
		if err := json.Unmarshal(d.Episodes, &l); err != nil {
			return nil, fmt.Errorf("%w : episodes illisibles", domain.ErrAmont)
		}
		for i, liste := range l {
			ajoute(i+1, liste)
		}
	default:
		return nil, fmt.Errorf("%w : format d'episodes inattendu", domain.ErrAmont)
	}
	numeros := make([]int, 0, len(parSaison))
	for n := range parSaison {
		numeros = append(numeros, n)
	}
	sort.Ints(numeros)
	var res []domain.Saison
	for _, n := range numeros {
		eps := parSaison[n]
		sort.SliceStable(eps, func(i, j int) bool { return eps[i].Numero < eps[j].Numero })
		s := domain.Saison{Numero: n}
		for _, e := range eps {
			if e.ID == "" {
				continue
			}
			s.Episodes = append(s.Episodes, domain.Episode{
				ID: c.contenuID(e.ID + texte("."+extensionSure(e.Extension.String()))), Numero: int(e.Numero), Titre: e.Titre.String(),
				Extension: e.Extension.String(), Duree: e.duree(),
			})
		}
		if len(s.Episodes) > 0 {
			res = append(res, s)
		}
	}
	return res, nil
}
