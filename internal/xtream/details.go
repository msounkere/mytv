package xtream

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"

	"mytv/internal/domain"
)

var _ domain.FournisseurDetails = (*Client)(nil)

// Bornes : ce texte vient d'un tiers et finit dans l'interface et dans la base.
const (
	infoFilmMax     = 1 << 20 // get_vod_info pese quelques Kio ; 1 Mio est deja large
	synopsisMax     = 4000    // caracteres
	champCourtMax   = 500
	imagesFondMax   = 5
	urlImageMax     = 1024
	dureeMaxPlausib = 24 * time.Hour
)

// listeTexte accepte une chaine, un tableau de chaines (backdrop_path) ou n'importe
// quoi d'autre (ignore) : le fournisseur ecrit les deux formes, et `[]` pour « rien ».
type listeTexte []string

func (l *listeTexte) UnmarshalJSON(b []byte) error {
	b = bytes.TrimSpace(b)
	*l = nil
	if len(b) == 0 {
		return nil
	}
	switch b[0] {
	case '[':
		var brut []json.RawMessage
		if err := json.Unmarshal(b, &brut); err != nil {
			return nil
		}
		for _, r := range brut {
			var t texte
			if t.UnmarshalJSON(r) == nil && t != "" {
				*l = append(*l, t.String())
			}
		}
	case '"':
		var t texte
		if t.UnmarshalJSON(b) == nil && t != "" {
			*l = []string{t.String()}
		}
	}
	return nil
}

// infoJSON : le bloc `info` de get_vod_info ET de get_series_info (champs communs).
type infoJSON struct {
	Plot         texte      `json:"plot"`
	Description  texte      `json:"description"`
	Genre        texte      `json:"genre"`
	Cast         texte      `json:"cast"`
	Actors       texte      `json:"actors"`
	Director     texte      `json:"director"`
	Country      texte      `json:"country"`
	ReleaseDate  texte      `json:"releasedate"`
	ReleaseDate2 texte      `json:"release_date"`
	ReleaseDate3 texte      `json:"releaseDate"`
	Trailer      texte      `json:"youtube_trailer"`
	Backdrop     listeTexte `json:"backdrop_path"`
	MovieImage   texte      `json:"movie_image"`
	CoverBig     texte      `json:"cover_big"`
	Cover        texte      `json:"cover"`
	Rating       decimal    `json:"rating"`
	Rating5      decimal    `json:"rating_5based"`
	Age          texte      `json:"age"`
	MPAA         texte      `json:"mpaa_rating"`
	DureeSecs    entier     `json:"duration_secs"`
	Duree        texte      `json:"duration"`
	RunTime      entier     `json:"episode_run_time"` // minutes
	Bitrate      entier     `json:"bitrate"`          // kbit/s du fichier (US-42 CA6)
}

// UnmarshalJSON : `info` vaut parfois `[]` ou `false` quand le fournisseur n'a rien.
func (i *infoJSON) UnmarshalJSON(b []byte) error {
	*i = infoJSON{}
	if b = bytes.TrimSpace(b); len(b) == 0 || b[0] != '{' {
		return nil
	}
	type brut infoJSON // sans la methode : pas de recursion
	var x brut
	if err := json.Unmarshal(b, &x); err != nil {
		return nil // un champ inattendu ne doit pas faire perdre le reste de la fiche
	}
	*i = infoJSON(x)
	return nil
}

func premier(valeurs ...texte) string {
	for _, v := range valeurs {
		if v != "" {
			return v.String()
		}
	}
	return ""
}

// nettoyer retire les caracteres de controle et borne la longueur (en caracteres).
func nettoyer(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) || r == ' ' || r == ' ' {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > max {
		s = strings.TrimSpace(string(r[:max]))
	}
	return s
}

// urlImage ne garde que http(s) : le champ finit dans un attribut d'image de la page.
func urlImage(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > urlImageMax || strings.ContainsAny(s, " \t\r\n\"'<>\\") {
		return ""
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return s
}

var (
	reIDYoutube  = regexp.MustCompile(`^[A-Za-z0-9_-]{11}$`)
	reHoteYoutub = regexp.MustCompile(`^(www\.|m\.)?(youtube\.com|youtu\.be)$`)
)

// bandeAnnonce ramene « youtube_trailer » a une adresse YouTube https sure. Le
// fournisseur donne tantot l'identifiant (« dQw4w9WgXcQ »), tantot l'adresse
// complete ; tout le reste (autre site, javascript:) est ecarte.
func bandeAnnonce(s string) string {
	s = strings.TrimSpace(s)
	if reIDYoutube.MatchString(s) {
		return "https://www.youtube.com/watch?v=" + s
	}
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || !reHoteYoutub.MatchString(strings.ToLower(u.Host)) {
		return ""
	}
	id := u.Query().Get("v")
	if strings.HasSuffix(strings.ToLower(u.Host), "youtu.be") {
		id = strings.TrimPrefix(u.Path, "/")
	} else if id == "" {
		for _, pref := range []string{"/embed/", "/shorts/", "/v/"} {
			if v, ok := strings.CutPrefix(u.Path, pref); ok {
				id = v
			}
		}
	}
	if !reIDYoutube.MatchString(id) {
		return ""
	}
	return "https://www.youtube.com/watch?v=" + id
}

// dureeTexte lit « 01:35:12 », « 95:00 » ou « 5700 ».
func dureeTexte(s string) time.Duration {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	if !strings.Contains(s, ":") {
		if n, err := strconv.ParseInt(s, 10, 64); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
		return 0
	}
	total, n := int64(0), 0
	for _, p := range strings.Split(s, ":") {
		v, err := strconv.ParseInt(strings.TrimSpace(p), 10, 64)
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

func (i infoJSON) duree() time.Duration {
	d := time.Duration(0)
	switch {
	case i.DureeSecs > 0:
		d = time.Duration(i.DureeSecs) * time.Second
	case dureeTexte(i.Duree.String()) > 0:
		d = dureeTexte(i.Duree.String())
	case i.RunTime > 0:
		d = time.Duration(i.RunTime) * time.Minute
	}
	if d < 0 || d > dureeMaxPlausib {
		return 0 // « 99999 » minutes n'est pas une duree : mieux vaut ne rien afficher
	}
	return d
}

func (i infoJSON) note() float64 {
	n := float64(i.Rating)
	if n <= 0 {
		n = float64(i.Rating5) * 2
	}
	if n < 0 || n > 10 {
		return 0
	}
	return n
}

func (i infoJSON) details() domain.Details {
	d := domain.Details{
		Synopsis:     nettoyer(premier(i.Plot, i.Description), synopsisMax),
		Genre:        nettoyer(i.Genre.String(), champCourtMax),
		Casting:      nettoyer(premier(i.Cast, i.Actors), champCourtMax*2),
		Realisateur:  nettoyer(i.Director.String(), champCourtMax),
		Pays:         nettoyer(i.Country.String(), champCourtMax),
		Sortie:       nettoyer(premier(i.ReleaseDate, i.ReleaseDate2, i.ReleaseDate3), 40),
		BandeAnnonce: bandeAnnonce(i.Trailer.String()),
		Affiche:      urlImage(premier(i.MovieImage, i.CoverBig, i.Cover)),
		Note:         i.note(),
		Age:          nettoyer(premier(i.Age, i.MPAA), 20),
	}
	for _, u := range i.Backdrop {
		if u = urlImage(u); u != "" && len(d.ImageFond) < imagesFondMax {
			d.ImageFond = append(d.ImageFond, u)
		}
	}
	return d
}

// lireCorps lit une reponse JSON bornee. Un objet `{"user_info":{"auth":0}}` est le
// refus d'authentification du fournisseur (statut 200) : ErrIdentifiants.
func (c *Client) lireCorps(ctx context.Context, action string, params url.Values, max int64) ([]byte, error) {
	q := url.Values{"username": {c.utilisateur}, "password": {c.motDePasse}, "action": {action}}
	for k, v := range params {
		q[k] = v
	}
	rep, err := c.Requete(ctx, c.base+"/player_api.php?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	defer rep.Body.Close()
	corps, err := io.ReadAll(io.LimitReader(rep.Body, max+1))
	if err != nil {
		return nil, c.erreurReseau(ctx, err)
	}
	if int64(len(corps)) > max {
		return nil, fmt.Errorf("%w : %s trop volumineux", domain.ErrAmont, action)
	}
	return corps, nil
}

// refusAuth : `{"user_info":{"auth":0}}` sans le champ attendu.
func refusAuth(corps []byte) bool {
	var r struct {
		UserInfo struct {
			Auth texte `json:"auth"`
		} `json:"user_info"`
	}
	return json.Unmarshal(corps, &r) == nil && r.UserInfo.Auth == "0"
}

// DetailsFilm lit get_vod_info : synopsis, genre, duree, casting... d'UN film. Aucun
// flux n'est ouvert. Le fournisseur repond `[]`, `{}` ou `{"info":[]}` quand il n'a
// rien : on rend alors des details vides, sans erreur.
func (c *Client) DetailsFilm(ctx context.Context, numero string) (*domain.DetailsFilm, error) {
	corps, err := c.lireCorps(ctx, "get_vod_info", url.Values{"vod_id": {numero}}, infoFilmMax)
	if err != nil {
		return nil, err
	}
	var d struct {
		Info infoJSON `json:"info"`
	}
	b := bytes.TrimSpace(corps)
	if len(b) > 0 && b[0] == '{' {
		if refusAuth(b) {
			return nil, domain.ErrIdentifiants
		}
		if err := json.Unmarshal(b, &d); err != nil {
			return nil, fmt.Errorf("%w : get_vod_info illisible", domain.ErrAmont)
		}
	} else if len(b) == 0 || !(b[0] == '[' || string(b) == "null" || string(b) == "false") {
		return nil, fmt.Errorf("%w : get_vod_info illisible", domain.ErrAmont)
	}
	return &domain.DetailsFilm{Details: d.Info.details(), Duree: d.Info.duree()}, nil
}

// infoEpisodeJSON : le `info` d'un episode (souvent `[]` quand il est vide).
type infoEpisodeJSON struct {
	infoJSON
}

func (i *infoEpisodeJSON) UnmarshalJSON(b []byte) error { return i.infoJSON.UnmarshalJSON(b) }

// SaisonsEtDetails lit get_series_info UNE fois : saisons et episodes (avec leur
// synopsis, image, date et duree) et le bloc `info` de la serie.
func (c *Client) SaisonsEtDetails(ctx context.Context, numeroSerie string) ([]domain.Saison, *domain.DetailsSerie, error) {
	corps, err := c.lireCorps(ctx, "get_series_info", url.Values{"series_id": {numeroSerie}}, infoSerieMax)
	if err != nil {
		return nil, nil, err
	}
	var d struct {
		Episodes json.RawMessage `json:"episodes"`
		Info     infoJSON        `json:"info"`
	}
	b := bytes.TrimSpace(corps)
	switch {
	case len(b) > 0 && b[0] == '{':
		if refusAuth(b) {
			return nil, nil, domain.ErrIdentifiants
		}
		if err := json.Unmarshal(b, &d); err != nil {
			return nil, nil, fmt.Errorf("%w : get_series_info illisible", domain.ErrAmont)
		}
	case len(b) > 0 && (b[0] == '[' || string(b) == "null" || string(b) == "false"):
		// Serie inconnue ou sans contenu : le fournisseur repond une liste vide.
	default:
		return nil, nil, fmt.Errorf("%w : get_series_info illisible", domain.ErrAmont)
	}
	saisons, err := c.saisonsDepuis(d.Episodes)
	if err != nil {
		return nil, nil, err
	}
	return saisons, &domain.DetailsSerie{Details: d.Info.details(), DureeEpisode: d.Info.duree()}, nil
}
