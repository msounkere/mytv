// Package xtream est le client du fournisseur Xtream Codes. Il implemente
// domain.Fournisseur et il est le SEUL endroit qui connait les identifiants du
// fournisseur : toute erreur et toute ligne de journal qui sort d'ici est
// donc nettoyee (le paquet net/http, lui, met l'URL complete — mot de passe
// compris — dans ses messages d'erreur).
package xtream

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"mytv/internal/config"
	"mytv/internal/domain"
	"mytv/internal/hotes"
)

// agentParDefaut : le fournisseur refuse ou bride les clients qui ne ressemblent
// pas a un navigateur.
const agentParDefaut = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0 Safari/537.36"

// redirectionsMax : le fournisseur renvoie au premier appel un 302 vers un autre
// noeud ; au-dela de quelques sauts c'est une boucle.
const redirectionsMax = 10

// Reglages du transport. Le fournisseur n'autorise qu'une connexion active :
// quelques connexions inactives gardees au chaud suffisent a eviter une
// poignee de main par requete, sans en accumuler.
const (
	connexionsInactivesMax   = 4
	dureeInactiviteConnexion = 60 * time.Second
	// reponseErreurMax borne la lecture d'une reponse JSON qui n'est pas une liste.
	reponseErreurMax = 1 << 20
)

// Client parle a un fournisseur Xtream.
type Client struct {
	id          string
	base        string
	utilisateur string
	motDePasse  string
	agent       string
	http        *http.Client // API (catalogue, guide) : keep-alive
	httpFlux    *http.Client // segments, films, playlists : sans keep-alive si reseau.fermer_connexions_flux
	journal     *slog.Logger
	horloge     func() time.Time
	hotes       *hotes.Filtre // nil = redirections et connexions non filtrees (tests locaux)
}

var _ domain.Fournisseur = (*Client)(nil)

// Option ajuste un Client (tests : journal, horloge).
type Option func(*Client)

// AvecJournal remplace le journal (par defaut : slog.Default()).
func AvecJournal(l *slog.Logger) Option { return func(c *Client) { c.journal = l } }

// AvecFiltreHotes controle ce que le FOURNISSEUR peut nous faire joindre : chaque
// redirection est reverifiee et chaque connexion controle l'adresse IP reelle (voir
// internal/hotes). Sans lui, une redirection du fournisseur vers une adresse interne
// serait suivie. Le filtre est applique a la construction : a passer a Nouveau.
func AvecFiltreHotes(f *hotes.Filtre) Option { return func(c *Client) { c.hotes = f } }

// AvecHorloge remplace l'horloge qui date les instantanes.
func AvecHorloge(h func() time.Time) Option { return func(c *Client) { c.horloge = h } }

// Nouveau cree le client d'un fournisseur. Les identifiants sont deja resolus
// (config.Secret.Valeur) : ce paquet ne lit ni environnement ni fichier.
func Nouveau(f config.Fournisseur, utilisateur, motDePasse string, opts ...Option) *Client {
	r := f.Reseau
	// Pas de delai global (http.Client.Timeout) : il couperait la lecture d'un
	// film en cours. Seuls la connexion et l'attente de l'en-tete sont bornes ici ;
	// la duree totale est portee par le contexte de l'appelant.
	nouveauTransport := func() *http.Transport {
		return &http.Transport{
			DialContext:           (&net.Dialer{Timeout: r.DelaiConnexion.Std()}).DialContext,
			TLSHandshakeTimeout:   r.DelaiConnexion.Std(),
			ResponseHeaderTimeout: r.DelaiEntete.Std(),
			MaxIdleConnsPerHost:   connexionsInactivesMax,
			IdleConnTimeout:       dureeInactiviteConnexion,
		}
	}
	tr, trFlux := nouveauTransport(), nouveauTransport()
	// POURQUOI un transport distinct pour les flux : le fournisseur lie sa connexion
	// unique a la connexion TCP. Une connexion de segment ou de film gardee « au chaud »
	// lui ferait garder la place bien apres le zapping ; le catalogue et le guide, eux,
	// gagnent a reutiliser la leur. DisableKeepAlives envoie `Connection: close`.
	trFlux.DisableKeepAlives = r.FermeLesFlux()
	agent := f.UserAgent
	if agent == "" {
		agent = agentParDefaut
	}
	c := &Client{
		id:          f.ID,
		base:        strings.TrimRight(f.Base, "/"),
		utilisateur: utilisateur,
		motDePasse:  motDePasse,
		agent:       agent,
		journal:     slog.Default(),
		horloge:     time.Now,
		http:        &http.Client{Transport: tr},
		httpFlux:    &http.Client{Transport: trFlux},
	}
	for _, o := range opts {
		o(c)
	}
	redirection := func(req *http.Request, via []*http.Request) error {
		if len(via) > redirectionsMax {
			return fmt.Errorf("plus de %d redirections", redirectionsMax)
		}
		// Le client de Go recopie l'adresse precedente dans Referer : elle porte
		// les identifiants dans son chemin, ils ne doivent pas suivre la redirection.
		req.Header.Del("Referer")
		return nil
	}
	if c.hotes != nil {
		tr.DialContext = c.hotes.DialContext(r.DelaiConnexion.Std())
		trFlux.DialContext = tr.DialContext
		redirection = c.hotes.ControleRedirection(redirectionsMax)
	}
	c.http.CheckRedirect, c.httpFlux.CheckRedirect = redirection, redirection
	return c
}

func (c *Client) ID() string { return c.id }

// Fermer ferme les connexions inactives gardees au chaud (arret propre : aucune
// goroutine de transport ne doit survivre au client).
func (c *Client) Fermer() {
	c.http.CloseIdleConnections()
	c.httpFlux.CloseIdleConnections()
}

// URLLive : playlist HLS d'une chaine. Les identifiants sont echappes car un mot
// de passe peut contenir « / », « ? » ou « # ».
func (c *Client) URLLive(numero string) string {
	return fmt.Sprintf("%s/live/%s/%s/%s.m3u8", c.base,
		url.PathEscape(c.utilisateur), url.PathEscape(c.motDePasse), url.PathEscape(numero))
}

// masquer retire les identifiants de n'importe quel texte destine a une erreur
// ou a un journal. Les identifiants trop courts ne sont pas masques : ils
// abimeraient tous les messages sans rien proteger.
func (c *Client) masquer(s string) string {
	for _, secret := range []string{c.utilisateur, c.motDePasse} {
		if len(secret) < 3 {
			continue
		}
		for _, forme := range []string{secret, url.PathEscape(secret), url.QueryEscape(secret)} {
			s = strings.ReplaceAll(s, forme, "***")
		}
	}
	return s
}

// erreurReseau traduit une erreur de transport en erreur typee, sans URL.
func (c *Client) erreurReseau(ctx context.Context, err error) error {
	// L'annulation voulue par l'appelant n'est pas une panne du fournisseur.
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	if errors.Is(err, hotes.ErrRefuse) {
		// Gardee dans la chaine : les appelants distinguent un hote refuse d'une panne.
		return fmt.Errorf("%w : %w : %s", domain.ErrAmont, hotes.ErrRefuse, c.masquer(err.Error()))
	}
	return fmt.Errorf("%w : %s", domain.ErrAmont, c.masquer(err.Error()))
}

// erreurStatut traduit un statut HTTP d'echec ; nil si le statut est un succes.
func erreurStatut(code int) error {
	switch {
	case code >= 200 && code < 300:
		return nil
	case code == http.StatusUnauthorized, code == http.StatusForbidden:
		return domain.ErrIdentifiants
	case code == http.StatusNotFound:
		return domain.ErrIntrouvable
	case code == http.StatusProxyAuthRequired:
		return domain.ErrRefuseConnexions
	case code == 458:
		return domain.ErrRefuseIP
	}
	// 5xx, mais aussi 405, 429 ou un 3xx sans suite : le fournisseur a des
	// ratés ponctuels, a reessayer par l'appelant.
	return fmt.Errorf("%w : HTTP %d", domain.ErrAmont, code)
}

// estAPI : les appels d'API et de guide (player_api.php, xmltv.php) reutilisent leurs
// connexions. Tout le reste (playlists, segments, films, episodes, et toute adresse
// inconnue obtenue d'une redirection ou d'une playlist) est traite comme un flux : se
// tromper dans ce sens ne coute qu'une connexion de plus, pas une place gardee.
func estAPI(u *url.URL) bool {
	return strings.HasSuffix(u.Path, "/player_api.php") || strings.HasSuffix(u.Path, "/xmltv.php")
}

// Requete execute une requete amont. Le corps est rendu en flux (jamais charge
// en memoire) et doit etre ferme par l'appelant.
func (c *Client) Requete(ctx context.Context, cible string, entetes http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cible, nil)
	if err != nil {
		return nil, fmt.Errorf("%w : adresse invalide", domain.ErrAmont)
	}
	// Defense en profondeur : ce client est le puits final de toutes les requetes
	// amont, il ne doit jamais ouvrir file://, ftp:// ou autre.
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return nil, fmt.Errorf("%w : schema d'adresse refuse", domain.ErrAmont)
	}
	req.Header.Set("User-Agent", c.agent)
	req.Header.Set("Accept", "*/*")
	for k, v := range entetes {
		req.Header[http.CanonicalHeaderKey(k)] = v
	}
	// Accept-Encoding : sans en-tete explicite, le transport demande gzip et
	// decompresse tout seul — parfait pour playlists et JSON. Mais avec un Range,
	// les octets demandes sont ceux du fichier ORIGINAL : on exige « identity ».
	if req.Header.Get("Range") != "" && req.Header.Get("Accept-Encoding") == "" {
		req.Header.Set("Accept-Encoding", "identity")
	}

	client := c.httpFlux
	if estAPI(req.URL) {
		client = c.http
	}
	debut := c.horloge()
	rep, err := client.Do(req)
	if err != nil {
		e := c.erreurReseau(ctx, err)
		c.journal.Debug("requete amont en echec", "fournisseur", c.id, "duree_ms", c.horloge().Sub(debut).Milliseconds(), "erreur", e.Error())
		return nil, e
	}
	c.journal.Debug("requete amont", "fournisseur", c.id, "statut", rep.StatusCode, "duree_ms", c.horloge().Sub(debut).Milliseconds())
	if rep.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		// Plage hors du fichier : ce n'est pas une panne du fournisseur, le client a
		// demande un octet qui n'existe pas. La taille (Content-Range: bytes */N) est
		// transmise pour la repondre telle quelle.
		total := int64(-1)
		if v, ok := strings.CutPrefix(strings.TrimSpace(rep.Header.Get("Content-Range")), "bytes */"); ok {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n >= 0 {
				total = n
			}
		}
		_ = rep.Body.Close()
		return nil, domain.ErrPlageHorsFichier{Total: total}
	}
	if e := erreurStatut(rep.StatusCode); e != nil {
		// Un peu de corps est lu pour que la connexion puisse etre reutilisee.
		_, _ = io.CopyN(io.Discard, rep.Body, 4096)
		_ = rep.Body.Close()
		return nil, e
	}
	return rep, nil
}

func (c *Client) urlAPI(action string) string {
	q := url.Values{"username": {c.utilisateur}, "password": {c.motDePasse}}
	if action != "" {
		q.Set("action", action)
	}
	return c.base + "/player_api.php?" + q.Encode()
}

// lireListe decode un tableau JSON element par element : le catalogue de films
// pese plusieurs dizaines de Mo, on ne le garde jamais en entier en memoire.
func lireListe[T any](corps io.Reader, pour func(T)) error {
	br := bufio.NewReader(corps)
	// Un objet a la place d'une liste : si c'est le refus d'authentification
	// `{"user_info":{"auth":0}}`, c'est une erreur d'identifiants, pas de reseau.
	if o, _ := br.Peek(1); len(o) == 1 && o[0] == '{' {
		var rep struct {
			UserInfo struct {
				Auth texte `json:"auth"`
			} `json:"user_info"`
		}
		if json.NewDecoder(io.LimitReader(br, reponseErreurMax)).Decode(&rep) == nil && rep.UserInfo.Auth == "0" {
			return domain.ErrIdentifiants
		}
		return fmt.Errorf("%w : format de reponse inattendu", domain.ErrAmont)
	}
	dec := json.NewDecoder(br)
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("%w : reponse illisible (%v)", domain.ErrAmont, err)
	}
	if tok == nil {
		return nil // « null » : liste vide
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		// Le fournisseur repond parfois `{}` ou `false` pour une categorie vide,
		// mais aussi pour une erreur d'authentification : on ne peut pas trancher,
		// et un catalogue vide ecraserait le bon. On refuse.
		return fmt.Errorf("%w : format de reponse inattendu", domain.ErrAmont)
	}
	for dec.More() {
		var v T
		if err := dec.Decode(&v); err != nil {
			return fmt.Errorf("%w : entree illisible (%v)", domain.ErrAmont, err)
		}
		pour(v)
	}
	if _, err := dec.Token(); err != nil {
		return fmt.Errorf("%w : reponse tronquee (%v)", domain.ErrAmont, err)
	}
	return nil
}

// appelListe interroge une action de l'API et applique `pour` a chaque element.
func appelListe[T any](ctx context.Context, c *Client, action string, pour func(T)) error {
	rep, err := c.Requete(ctx, c.urlAPI(action), nil)
	if err != nil {
		return err
	}
	defer rep.Body.Close()
	if err := lireListe(rep.Body, pour); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Les erreurs de decodage ne contiennent jamais l'URL : pas de masquage utile.
		return fmt.Errorf("%s : %w", action, err)
	}
	return nil
}

// Catalogue telecharge categories, chaines, films et series.
func (c *Client) Catalogue(ctx context.Context) (*domain.Instantane, error) {
	debut := c.horloge()
	s := &domain.Instantane{FournisseurID: c.id}

	categories := func(action string, t domain.Type) error {
		ordre := 0
		return appelListe(ctx, c, action, func(j categorieJSON) {
			if j.ID == "" {
				return
			}
			s.Categories = append(s.Categories, domain.Categorie{
				ID: j.ID.String(), FournisseurID: c.id, Type: t, Nom: j.Nom.String(), Ordre: ordre,
			})
			ordre++
		})
	}
	if err := categories("get_live_categories", domain.TypeLive); err != nil {
		return nil, err
	}
	ordre := 0
	err := appelListe(ctx, c, "get_live_streams", func(j chaineJSON) {
		if j.StreamID == "" {
			return // sans numero, la chaine ne peut ni etre identifiee ni jouee
		}
		s.Chaines = append(s.Chaines, domain.Chaine{
			ID: c.contenuID(j.StreamID), FournisseurID: c.id, Numero: j.StreamID.String(),
			Nom: j.Nom.String(), Logo: j.Icone.String(), EpgID: j.EpgID.String(),
			CategorieID: j.CategorieID.String(), NumeroChaine: int(j.Num), Ordre: ordre, Ajoute: j.Ajoute.Heure(),
		})
		ordre++
	})
	if err != nil {
		return nil, err
	}
	if err := categories("get_vod_categories", domain.TypeFilm); err != nil {
		return nil, err
	}
	err = appelListe(ctx, c, "get_vod_streams", func(j filmJSON) {
		if j.StreamID == "" {
			return
		}
		s.Films = append(s.Films, domain.Film{
			ID: c.contenuID(j.StreamID), FournisseurID: c.id, Numero: j.StreamID.String(),
			Nom: j.Nom.String(), Image: j.Icone.String(), CategorieID: j.CategorieID.String(),
			Extension: j.Extension.String(), Note: float64(j.Note),
			Annee: annee(j.Annee, j.Sortie, j.SortieAlt), Ajoute: j.Ajoute.Heure(),
		})
	})
	if err != nil {
		return nil, err
	}
	if err := categories("get_series_categories", domain.TypeSerie); err != nil {
		return nil, err
	}
	err = appelListe(ctx, c, "get_series", func(j serieJSON) {
		if j.SerieID == "" {
			return
		}
		s.Series = append(s.Series, domain.SerieTV{
			ID: c.serieID(j.SerieID), FournisseurID: c.id, Numero: j.SerieID.String(),
			Nom: j.Nom.String(), Image: j.Couverture.String(), CategorieID: j.CategorieID.String(),
			Note: float64(j.Note), Annee: annee(j.Annee, j.Sortie, j.SortieAlt), Ajoute: j.Modifie.Heure(),
		})
	})
	if err != nil {
		return nil, err
	}
	s.Pris = c.horloge()
	c.journal.Info("catalogue telecharge", "fournisseur", c.id,
		"categories", len(s.Categories), "chaines", len(s.Chaines), "films", len(s.Films), "series", len(s.Series),
		"duree_ms", s.Pris.Sub(debut).Milliseconds())
	return s, nil
}

// serieID : « <fournisseur>:s<numero> ». Chez Xtream, series_id et stream_id sont deux
// espaces de numeros DISTINCTS : la serie 77 et le film 77 coexistent. Sans le « s »,
// ils auraient le meme ContenuID (cle primaire du stockage, favoris, historique...).
// Numero reste le numero du fournisseur, sans prefixe.
func (c *Client) serieID(numero texte) domain.ContenuID {
	return domain.ContenuID(c.id + ":s" + numero.String())
}

func (c *Client) contenuID(numero texte) domain.ContenuID {
	return domain.ContenuID(c.id + ":" + numero.String())
}
