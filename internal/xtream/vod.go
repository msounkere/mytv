package xtream

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"mytv/internal/domain"
)

var (
	_ domain.FournisseurVOD = (*Client)(nil)
	_ domain.SourceEPG      = (*Client)(nil)
)

// URLEPG : le guide XMLTV du fournisseur. Identifiants echappes ; l'adresse ne
// doit jamais etre journalisee (elle les contient) : on la passe a Requete, qui
// masque ses erreurs.
func (c *Client) URLEPG() string {
	q := url.Values{"username": {c.utilisateur}, "password": {c.motDePasse}}
	return c.base + "/xmltv.php?" + q.Encode()
}

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
	ID        texte           `json:"id"`
	Numero    entier          `json:"episode_num"`
	Saison    entier          `json:"season"`
	Titre     texte           `json:"title"`
	Extension texte           `json:"container_extension"`
	Info      infoEpisodeJSON `json:"info"`
}

func (e episodeJSON) duree() time.Duration {
	if e.Info.DureeSecs > 0 {
		return time.Duration(e.Info.DureeSecs) * time.Second
	}
	// « 00:45:12 » ou « 45:12 »
	return dureeTexte(e.Info.Duree.String())
}

// Saisons liste les saisons et episodes d'une serie (get_series_info). Voir
// SaisonsEtDetails, qui rend en plus les details de la serie.
func (c *Client) Saisons(ctx context.Context, numeroSerie string) ([]domain.Saison, error) {
	s, _, err := c.SaisonsEtDetails(ctx, numeroSerie)
	return s, err
}

// saisonsDepuis decode le champ « episodes ». Le fournisseur l'ecrit tantot en objet
// {"1":[...],"2":[...]}, tantot en tableau de tableaux, parfois en [] quand il n'y en
// a pas ; les nombres sont des chaines ou non. Un episode sans identifiant est
// ignore (injouable).
func (c *Client) saisonsDepuis(episodes json.RawMessage) ([]domain.Saison, error) {
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
	brut := strings.TrimSpace(string(episodes))
	switch {
	case brut == "" || brut == "null" || brut == "[]" || brut == "{}" || brut == "false":
	case brut[0] == '{':
		var m map[string][]episodeJSON
		if err := json.Unmarshal(episodes, &m); err != nil {
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
		if err := json.Unmarshal(episodes, &l); err != nil {
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
				Synopsis: nettoyer(premier(e.Info.Plot, e.Info.Description), synopsisMax),
				Image:    urlImage(premier(e.Info.MovieImage, e.Info.CoverBig, e.Info.Cover)),
				Sortie:   nettoyer(premier(e.Info.ReleaseDate, e.Info.ReleaseDate2, e.Info.ReleaseDate3), 40),
				Note:     e.Info.note(),
			})
		}
		if len(s.Episodes) > 0 {
			res = append(res, s)
		}
	}
	return res, nil
}
