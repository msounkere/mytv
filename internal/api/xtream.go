package api

// Couche Xtream (lot 11, etape A1, docs/lot11.md) : mytv se presente comme un fournisseur Xtream
// pour les box IPTV (Formuler / MYTVOnline, TiviMate, VLC...). Rien de nouveau n'est servi ici :
// les listes viennent du catalogue, les flux des gestionnaires existants.
//
//   - Authentification : nom et mot de passe d'un compte mytv d'APPAREIL, a chaque requete
//     (convention Xtream). Ils ouvrent UNE session mytv, gardee en memoire (cle : empreinte du
//     couple) et reverifiee a chaque usage ; la limitation de tentatives de l'interface
//     s'applique. Un compte administrateur est refuse : son mot de passe voyagerait dans les
//     adresses de flux.
//   - Direct : /live/<u>/<p>/<chaine>.m3u8 redirige vers un lien de diffusion signe (US-40) au
//     nom de la session, en declarant a la conversion que la box lit tout (cap=hevc,eac3,ac3).
//   - Films et episodes : relayes TELS QUELS (client « natif » de vod : ni sonde ni ffmpeg),
//     Range compris ; un fichier prepare d'avance part du disque.
//   - Les identifiants Xtream sont les numeros du fournisseur, sans prefixe : un seul fournisseur
//     est expose (celui du direct, sinon le premier).

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"mytv/internal/config"
	"mytv/internal/domain"
	"mytv/internal/occupation"
)

const (
	// xtreamSessionsMax : couples (compte, mot de passe) gardes en memoire.
	xtreamSessionsMax = 64
	// xtreamLienDuree : validite d'un lien de direct remis a une box (une soiree de visionnage ;
	// la box en redemande un a chaque zapping).
	xtreamLienDuree = 24 * time.Hour
	// xtreamCapacites : ce que lit une box IPTV (aucune conversion du direct).
	xtreamCapacites = "hevc,eac3,ac3"
)

var (
	reXtreamNumero  = regexp.MustCompile(`^[0-9]{1,20}$`)
	reXtreamFichier = regexp.MustCompile(`^([0-9]{1,20})(?:\.([A-Za-z0-9]{1,5}))?$`)
	errXtreamAdmin  = errors.New("xtream : compte administrateur refuse")
)

type sessionXtream struct {
	id     string
	profil string
}

type etatXtream struct {
	mu       sync.Mutex
	sessions map[string]sessionXtream // empreinte(nom, mot de passe) -> session
}

// HandlerXtream sert les adresses Xtream a monter A LA RACINE du serveur (voir appli.racine).
// Nil si la couche est inactive (xtream.actif = faux).
func (a *API) HandlerXtream(cfg config.Xtream) http.Handler {
	if !cfg.Actif {
		return nil
	}
	x := &etatXtream{sessions: map[string]sessionXtream{}}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /player_api.php", func(w http.ResponseWriter, r *http.Request) { a.xtreamAPI(w, r, x, cfg) })
	mux.HandleFunc("GET /get.php", func(w http.ResponseWriter, r *http.Request) { a.xtreamM3U(w, r, x, cfg) })
	mux.HandleFunc("GET /live/{u}/{p}/{fichier}", func(w http.ResponseWriter, r *http.Request) { a.xtreamLive(w, r, x) })
	mux.HandleFunc("GET /movie/{u}/{p}/{fichier}", func(w http.ResponseWriter, r *http.Request) { a.xtreamFichier(w, r, x, false) })
	mux.HandleFunc("GET /series/{u}/{p}/{fichier}", func(w http.ResponseWriter, r *http.Request) { a.xtreamFichier(w, r, x, true) })
	return a.recuperer(mux)
}

// ---------------------------------------------------------------- authentification

func empreinteXtream(nom, mdp string) string {
	h := sha256.Sum256([]byte(nom + "\x00" + mdp))
	return hex.EncodeToString(h[:])
}

func adresseXtream(req *http.Request) string {
	h, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		h = req.RemoteAddr
	}
	// Derriere Caddy (boucle locale) : l'adresse du client est la derniere de X-Forwarded-For.
	if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
		if xf := req.Header.Get("X-Forwarded-For"); xf != "" {
			parts := strings.Split(xf, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	return h
}

// sessionXtream rend la session mytv du couple (nom, mot de passe), en l'ouvrant au besoin.
func (a *API) sessionXtream(ctx context.Context, x *etatXtream, nom, mdp, adresse string) (sessionXtream, error) {
	if nom == "" || mdp == "" {
		return sessionXtream{}, domain.ErrIdentifiantsInvalides
	}
	cle := empreinteXtream(nom, mdp)
	x.mu.Lock()
	s, ok := x.sessions[cle]
	x.mu.Unlock()
	if ok {
		if _, err := a.opt.Auth.SessionParID(ctx, s.id); err == nil {
			return s, nil
		}
		x.mu.Lock()
		delete(x.sessions, cle)
		x.mu.Unlock()
	}
	if a.opt.Comptes == nil {
		return sessionXtream{}, domain.ErrIdentifiantsInvalides
	}
	jeton, sess, err := a.opt.Comptes.Connecter(ctx, nom, mdp, adresse)
	if err != nil {
		return sessionXtream{}, err
	}
	if sess.Admin {
		_ = a.opt.Comptes.Deconnecter(ctx, jeton)
		return sessionXtream{}, errXtreamAdmin
	}
	profil := sess.ProfilID
	if profil == "" {
		// Une box n'a pas d'ecran de choix de profil : le premier du compte, cree au besoin.
		ps, err := a.opt.Comptes.Profils(ctx, sess.UtilisateurID)
		if err != nil {
			return sessionXtream{}, err
		}
		if len(ps) == 0 {
			p, err := a.opt.Comptes.CreerProfil(ctx, sess.UtilisateurID, "Box")
			if err != nil {
				return sessionXtream{}, err
			}
			ps = []domain.Profil{*p}
		}
		profil = ps[0].ID
		if err := a.opt.Comptes.ChoisirProfil(ctx, sess.ID, profil); err != nil {
			return sessionXtream{}, err
		}
	}
	s = sessionXtream{id: sess.ID, profil: profil}
	x.mu.Lock()
	if len(x.sessions) >= xtreamSessionsMax {
		for k := range x.sessions {
			delete(x.sessions, k)
			break
		}
	}
	x.sessions[cle] = s
	x.mu.Unlock()
	a.log.Info("xtream : appareil connecte", "compte", nom, "session", abreger(sess.ID))
	return s, nil
}

// refusXtream : reponse d'un flux sans identifiants valides.
func refusXtream(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, domain.ErrTropDeTentatives):
		w.Header().Set("Retry-After", "60")
		http.Error(w, "trop de tentatives", http.StatusTooManyRequests)
	case errors.Is(err, errXtreamAdmin):
		http.Error(w, "compte administrateur refuse : creer un compte d'appareil", http.StatusForbidden)
	default:
		http.Error(w, "identifiants refuses", http.StatusUnauthorized)
	}
}

// fournisseurXtream : le fournisseur expose (identifiants sans prefixe).
func (a *API) fournisseurXtream() string {
	if a.opt.FournisseurLive != "" {
		return a.opt.FournisseurLive
	}
	if len(a.opt.Fournisseurs) > 0 {
		return a.opt.Fournisseurs[0]
	}
	return ""
}

// ---------------------------------------------------------------- player_api.php

func (a *API) xtreamAPI(w http.ResponseWriter, req *http.Request, x *etatXtream, cfg config.Xtream) {
	q := req.URL.Query()
	nom, mdp := q.Get("username"), q.Get("password")
	action := q.Get("action")
	a.inst.Compter("xtream_requetes_total", 1, "action", etiquetteAction(action))
	_, err := a.sessionXtream(req.Context(), x, nom, mdp, adresseXtream(req))
	if err != nil {
		if errors.Is(err, domain.ErrTropDeTentatives) || errors.Is(err, errXtreamAdmin) {
			refusXtream(w, err)
			return
		}
		// Convention Xtream : identifiants refuses = 200 et « auth: 0 ».
		ecrireJSON(w, http.StatusOK, map[string]any{"user_info": map[string]any{"auth": 0}})
		return
	}
	ctx := req.Context()
	switch action {
	case "":
		ecrireJSON(w, http.StatusOK, a.xtreamCompte(nom, mdp, cfg))
	case "get_live_categories":
		a.xtreamCategories(w, req, domain.TypeLive)
	case "get_vod_categories":
		a.xtreamCategories(w, req, domain.TypeFilm)
	case "get_series_categories":
		a.xtreamCategories(w, req, domain.TypeSerie)
	case "get_live_streams":
		a.xtreamListe(w, req, domain.ExportLive, q.Get("category_id"))
	case "get_vod_streams":
		a.xtreamListe(w, req, domain.ExportVOD, q.Get("category_id"))
	case "get_series":
		a.xtreamListe(w, req, domain.ExportSeries, q.Get("category_id"))
	case "get_vod_info":
		a.xtreamInfoFilm(w, ctx, q.Get("vod_id"))
	case "get_series_info":
		a.xtreamInfoSerie(w, ctx, q.Get("series_id"))
	case "get_short_epg", "get_simple_data_table":
		a.xtreamGuide(w, ctx, q.Get("stream_id"), q.Get("limit"), action == "get_simple_data_table")
	default:
		ecrireJSON(w, http.StatusOK, []any{})
	}
}

func etiquetteAction(a string) string {
	switch a {
	case "", "get_live_categories", "get_vod_categories", "get_series_categories", "get_live_streams",
		"get_vod_streams", "get_series", "get_vod_info", "get_series_info", "get_short_epg", "get_simple_data_table":
		if a == "" {
			return "compte"
		}
		return a
	}
	return "autre"
}

func (a *API) xtreamCompte(nom, mdp string, cfg config.Xtream) map[string]any {
	u, _ := url.Parse(cfg.URLPublique)
	port := u.Port()
	if port == "" {
		port = map[string]string{"https": "443", "http": "80"}[u.Scheme]
	}
	maintenant := a.maintenant().UTC()
	return map[string]any{
		"user_info": map[string]any{
			"username": nom, "password": mdp, "message": "mytv", "auth": 1, "status": "Active",
			"exp_date": nil, "is_trial": "0", "active_cons": "0", "created_at": "0", "max_connections": "1",
			"allowed_output_formats": []string{"m3u8"},
		},
		"server_info": map[string]any{
			"url": u.Hostname(), "port": port, "https_port": port, "server_protocol": u.Scheme, "rtmp_port": "0",
			// UTC : les heures de replay demandees par la box sont converties par mytv (etape B).
			"timezone": "UTC", "timestamp_now": maintenant.Unix(), "time_now": maintenant.Format("2006-01-02 15:04:05"),
		},
	}
}

func (a *API) xtreamCategories(w http.ResponseWriter, req *http.Request, t domain.Type) {
	cats, err := a.opt.Stockage.Categories(req.Context(), t)
	if err != nil {
		a.erreurAmont(w, err, "categories")
		return
	}
	f := a.fournisseurXtream()
	filtre := a.filtreXtream(req.Context())
	var retenues map[string]bool
	if filtre != nil && a.opt.Export != nil {
		if retenues, err = a.categoriesRetenues(req.Context(), typeExportDe(t), filtre); err != nil {
			a.erreurAmont(w, err, "categories")
			return
		}
	}
	out := make([]map[string]any, 0, len(cats))
	for _, c := range cats {
		if c.FournisseurID != f || (retenues != nil && !retenues[c.ID]) {
			continue
		}
		out = append(out, map[string]any{"category_id": c.ID, "category_name": c.Nom, "parent_id": 0})
	}
	ecrireJSON(w, http.StatusOK, out)
}

// numeroJSON : un numero du fournisseur, rendu en nombre JSON (ce qu'attendent les box).
func numeroJSON(s string) any {
	if reXtreamNumero.MatchString(s) {
		return json.Number(strings.TrimLeft(s, "0") + map[bool]string{true: "0", false: ""}[strings.TrimLeft(s, "0") == ""])
	}
	return s
}

func secondes(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return strconv.FormatInt(t.Unix(), 10)
}

func note5(n float64) float64 { return float64(int(n/2*10+0.5)) / 10 }

// xtreamListe ecrit en flux le tableau JSON des chaines, films ou series (22 000 chaines : pas
// de tableau en memoire), compresse quand la box l'accepte.
func (a *API) xtreamListe(w http.ResponseWriter, req *http.Request, t domain.TypeExport, categorie string) {
	if a.opt.Export == nil {
		erreur(w, http.StatusNotImplemented, "non_disponible", "export du catalogue non active")
		return
	}
	f := a.fournisseurXtream()
	live := ""
	if t == domain.ExportLive {
		live = a.opt.FournisseurLive
	}
	filtre := a.filtreXtream(req.Context())
	var retenues map[string]bool
	if filtre != nil {
		var err error
		if retenues, err = a.categoriesRetenues(req.Context(), t, filtre); err != nil {
			a.erreurAmont(w, err, "liste")
			return
		}
	}
	ex, err := a.opt.Export.OuvrirExport(req.Context(), t, live)
	if err != nil {
		a.erreurAmont(w, err, "liste")
		return
	}
	defer ex.Fermer()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	var sortie io.Writer = w
	if strings.Contains(req.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Add("Vary", "Accept-Encoding")
		gz := gzip.NewWriter(w)
		defer gz.Close()
		sortie = gz
	}
	enc := json.NewEncoder(sortie)
	premier, n := true, 0
	_, _ = io.WriteString(sortie, "[")
	err = ex.Lire(func(v any) error {
		if nom, cat, _, ok := nomEtCategorie(v); ok && !filtre.garde(nom, cat, retenues) {
			return nil
		}
		var e map[string]any
		switch c := v.(type) {
		case domain.Chaine:
			if c.FournisseurID != f || (categorie != "" && c.CategorieID != categorie) {
				return nil
			}
			n++
			e = map[string]any{"num": n, "name": c.Nom, "stream_type": "live", "stream_id": numeroJSON(c.Numero),
				"stream_icon": c.Logo, "epg_channel_id": c.EpgID, "added": secondes(c.Ajoute), "category_id": c.CategorieID,
				"custom_sid": "", "tv_archive": 0, "direct_source": "", "tv_archive_duration": 0}
		case domain.Film:
			if c.FournisseurID != f || (categorie != "" && c.CategorieID != categorie) {
				return nil
			}
			n++
			e = map[string]any{"num": n, "name": c.Nom, "stream_type": "movie", "stream_id": numeroJSON(c.Numero),
				"stream_icon": c.Image, "rating": strconv.FormatFloat(c.Note, 'f', -1, 64), "rating_5based": note5(c.Note),
				"added": secondes(c.Ajoute), "category_id": c.CategorieID, "container_extension": c.Extension,
				"custom_sid": "", "direct_source": ""}
		case domain.SerieTV:
			if c.FournisseurID != f || (categorie != "" && c.CategorieID != categorie) {
				return nil
			}
			n++
			e = map[string]any{"num": n, "name": c.Nom, "series_id": numeroJSON(c.Numero), "cover": c.Image,
				"plot": "", "cast": "", "director": "", "genre": "", "releaseDate": c.Annee, "last_modified": secondes(c.Ajoute),
				"rating": strconv.FormatFloat(c.Note, 'f', -1, 64), "rating_5based": note5(c.Note),
				"backdrop_path": []string{}, "youtube_trailer": "", "episode_run_time": "", "category_id": c.CategorieID}
		default:
			return nil
		}
		if !premier {
			if _, err := io.WriteString(sortie, ","); err != nil {
				return err
			}
		}
		premier = false
		return enc.Encode(e)
	})
	_, _ = io.WriteString(sortie, "]")
	if err != nil && !contexteAnnule(req.Context()) {
		a.log.Warn("xtream : liste interrompue", "type", string(t), "erreur", err)
	}
}

func (a *API) xtreamInfoFilm(w http.ResponseWriter, ctx context.Context, numero string) {
	if a.opt.Video == nil || !reXtreamNumero.MatchString(numero) {
		ecrireJSON(w, http.StatusOK, map[string]any{"info": map[string]any{}, "movie_data": map[string]any{}})
		return
	}
	f, err := a.opt.Video.Film(ctx, domain.ContenuID(a.fournisseurXtream()+":"+numero))
	if err != nil {
		ecrireJSON(w, http.StatusOK, map[string]any{"info": map[string]any{}, "movie_data": map[string]any{}})
		return
	}
	info := map[string]any{"name": f.Nom, "o_name": f.Nom, "movie_image": f.Image, "cover_big": f.Image,
		"releasedate": f.Annee, "rating": strconv.FormatFloat(f.Note, 'f', -1, 64)}
	if a.opt.Details != nil && a.opt.Details.Disponible(f.FournisseurID) {
		if r := a.opt.Details.Film(ctx, *f, true); r.Details != nil {
			d := r.Details
			info["plot"], info["description"] = d.Synopsis, d.Synopsis
			info["genre"], info["cast"], info["actors"], info["director"] = d.Genre, d.Casting, d.Casting, d.Realisateur
			info["country"], info["youtube_trailer"], info["backdrop_path"] = d.Pays, d.BandeAnnonce, d.ImageFond
			if d.Sortie != "" {
				info["releasedate"] = d.Sortie
			}
			if d.Affiche != "" {
				info["movie_image"], info["cover_big"] = d.Affiche, d.Affiche
			}
			if r.Details.Duree > 0 {
				s := int(r.Details.Duree / time.Second)
				info["duration_secs"], info["duration"] = s, fmt.Sprintf("%02d:%02d:%02d", s/3600, s/60%60, s%60)
			}
		}
	}
	ecrireJSON(w, http.StatusOK, map[string]any{"info": info, "movie_data": map[string]any{
		"stream_id": numeroJSON(f.Numero), "name": f.Nom, "added": secondes(f.Ajoute), "category_id": f.CategorieID,
		"container_extension": f.Extension, "custom_sid": "", "direct_source": ""}})
}

func (a *API) xtreamInfoSerie(w http.ResponseWriter, ctx context.Context, numero string) {
	vide := map[string]any{"seasons": []any{}, "info": map[string]any{}, "episodes": map[string]any{}}
	if a.opt.Video == nil || !reXtreamNumero.MatchString(numero) {
		ecrireJSON(w, http.StatusOK, vide)
		return
	}
	f := a.fournisseurXtream()
	s, err := a.opt.Video.Serie(ctx, domain.ContenuID(f+":s"+numero))
	if err != nil {
		ecrireJSON(w, http.StatusOK, vide)
		return
	}
	var ss []domain.Saison
	if a.opt.Details != nil && a.opt.Details.Disponible(s.FournisseurID) {
		ss, err = a.opt.Details.Saisons(ctx, *s)
	} else if fv := a.opt.VOD[s.FournisseurID]; fv != nil {
		ss, err = fv.Saisons(ctx, s.Numero)
	}
	if err != nil {
		a.log.Warn("xtream : saisons indisponibles", "serie", string(s.ID), "erreur", err)
	}
	info := map[string]any{"name": s.Nom, "cover": s.Image, "releaseDate": s.Annee, "rating": strconv.FormatFloat(s.Note, 'f', -1, 64),
		"rating_5based": note5(s.Note), "category_id": s.CategorieID, "backdrop_path": []string{}}
	if a.opt.Details != nil && a.opt.Details.Disponible(s.FournisseurID) {
		if r := a.opt.Details.Serie(ctx, *s, false); r.Details != nil {
			d := r.Details
			info["plot"], info["genre"], info["cast"], info["director"] = d.Synopsis, d.Genre, d.Casting, d.Realisateur
			info["youtube_trailer"], info["backdrop_path"] = d.BandeAnnonce, d.ImageFond
			if d.DureeEpisode > 0 {
				info["episode_run_time"] = strconv.Itoa(int(d.DureeEpisode / time.Minute))
			}
		}
	}
	saisons := make([]map[string]any, 0, len(ss))
	episodes := map[string][]map[string]any{}
	for _, sa := range ss {
		saisons = append(saisons, map[string]any{"season_number": sa.Numero, "name": fmt.Sprintf("Saison %d", sa.Numero),
			"episode_count": len(sa.Episodes), "cover": s.Image})
		cle := strconv.Itoa(sa.Numero)
		for _, e := range sa.Episodes {
			_, reste, _ := strings.Cut(string(e.ID), ":")
			id, _, _ := strings.Cut(reste, ".")
			episodes[cle] = append(episodes[cle], map[string]any{
				"id": id, "episode_num": e.Numero, "title": e.Titre, "container_extension": e.Extension, "season": sa.Numero,
				"custom_sid": "", "added": "", "direct_source": "",
				"info": map[string]any{"duration_secs": int(e.Duree / time.Second), "plot": e.Synopsis, "movie_image": e.Image,
					"releasedate": e.Sortie, "rating": e.Note}})
		}
	}
	ecrireJSON(w, http.StatusOK, map[string]any{"seasons": saisons, "info": info, "episodes": episodes})
}

// xtreamGuide : get_short_epg (programmes a venir) et get_simple_data_table (la journee, passe
// compris). Titres et descriptions en base64, heures en UTC (server_info.timezone).
func (a *API) xtreamGuide(w http.ResponseWriter, ctx context.Context, numero, limite string, journee bool) {
	vide := map[string]any{"epg_listings": []any{}}
	if a.opt.EPG == nil || !reXtreamNumero.MatchString(numero) {
		ecrireJSON(w, http.StatusOK, vide)
		return
	}
	id := domain.ContenuID(a.fournisseurXtream() + ":" + numero)
	maintenant := a.maintenant().UTC()
	de, jusqua := maintenant, maintenant.Add(24*time.Hour)
	if journee {
		de = maintenant.Add(-72 * time.Hour)
	}
	g, err := a.opt.EPG.Grille(ctx, []domain.ContenuID{id}, de, jusqua)
	if err != nil {
		ecrireJSON(w, http.StatusOK, vide)
		return
	}
	max, _ := strconv.Atoi(limite)
	b64 := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	out := []map[string]any{}
	for i, p := range g[id] {
		if !journee && max > 0 && len(out) >= max {
			break
		}
		out = append(out, map[string]any{
			"id": strconv.Itoa(i + 1), "epg_id": p.EpgID, "title": b64(p.Titre), "description": b64(p.Description), "lang": "",
			"start": p.Debut.UTC().Format("2006-01-02 15:04:05"), "end": p.Fin.UTC().Format("2006-01-02 15:04:05"),
			"start_timestamp": strconv.FormatInt(p.Debut.Unix(), 10), "stop_timestamp": strconv.FormatInt(p.Fin.Unix(), 10),
			"channel_id": p.EpgID, "now_playing": map[bool]int{true: 1, false: 0}[!p.Debut.After(maintenant) && p.Fin.After(maintenant)],
			"has_archive": 0,
		})
	}
	ecrireJSON(w, http.StatusOK, map[string]any{"epg_listings": out})
}

// ---------------------------------------------------------------- get.php (M3U)

func (a *API) xtreamM3U(w http.ResponseWriter, req *http.Request, x *etatXtream, cfg config.Xtream) {
	q := req.URL.Query()
	nom, mdp := q.Get("username"), q.Get("password")
	if _, err := a.sessionXtream(req.Context(), x, nom, mdp, adresseXtream(req)); err != nil {
		refusXtream(w, err)
		return
	}
	if a.opt.Export == nil {
		http.Error(w, "export non active", http.StatusNotImplemented)
		return
	}
	cats := map[string]string{}
	if cs, err := a.opt.Stockage.Categories(req.Context(), domain.TypeLive); err == nil {
		for _, c := range cs {
			cats[c.ID] = c.Nom
		}
	}
	filtre := a.filtreXtream(req.Context())
	var retenues map[string]bool
	if filtre != nil {
		var err error
		if retenues, err = a.categoriesRetenues(req.Context(), domain.ExportLive, filtre); err != nil {
			http.Error(w, "catalogue indisponible", http.StatusBadGateway)
			return
		}
	}
	ex, err := a.opt.Export.OuvrirExport(req.Context(), domain.ExportLive, a.opt.FournisseurLive)
	if err != nil {
		http.Error(w, "catalogue indisponible", http.StatusBadGateway)
		return
	}
	defer ex.Fermer()
	base := strings.TrimRight(cfg.URLPublique, "/")
	u, p := url.PathEscape(nom), url.PathEscape(mdp)
	w.Header().Set("Content-Type", "audio/x-mpegurl; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="mytv.m3u"`)
	w.Header().Set("Cache-Control", "no-store")
	echapper := strings.NewReplacer(`"`, "'", "\n", " ", "\r", " ")
	_, _ = io.WriteString(w, "#EXTM3U\n")
	_ = ex.Lire(func(v any) error {
		c, ok := v.(domain.Chaine)
		if !ok || c.FournisseurID != a.fournisseurXtream() || !filtre.garde(c.Nom, c.CategorieID, retenues) {
			return nil
		}
		_, err := fmt.Fprintf(w, "#EXTINF:-1 tvg-id=\"%s\" tvg-name=\"%s\" tvg-logo=\"%s\" group-title=\"%s\",%s\n%s/live/%s/%s/%s.m3u8\n",
			echapper.Replace(c.EpgID), echapper.Replace(c.Nom), echapper.Replace(c.Logo), echapper.Replace(cats[c.CategorieID]),
			echapper.Replace(c.Nom), base, u, p, c.Numero)
		return err
	})
}

// ---------------------------------------------------------------- flux

// xtreamLive : la playlist du direct, par un lien de diffusion signe au nom de la session.
func (a *API) xtreamLive(w http.ResponseWriter, req *http.Request, x *etatXtream) {
	s, err := a.sessionXtream(req.Context(), x, req.PathValue("u"), req.PathValue("p"), adresseXtream(req))
	if err != nil {
		refusXtream(w, err)
		return
	}
	m := reXtreamFichier.FindStringSubmatch(req.PathValue("fichier"))
	if m == nil || a.cast == nil {
		http.NotFound(w, req)
		return
	}
	numero := m[1]
	if _, err := a.opt.Stockage.Chaine(req.Context(), domain.ContenuID(a.fournisseurXtream()+":"+numero)); err != nil {
		http.NotFound(w, req)
		return
	}
	expire := a.maintenant().Add(xtreamLienDuree).Truncate(time.Second).UTC()
	jeton := a.cast.Sceller(porteeCast, chargeCast{session: s.id, ressource: "live:" + numero, expire: expire}.encoder())
	a.inst.Compter("xtream_flux_total", 1, "type", "live")
	http.Redirect(w, req, Prefixe+"/cast/"+jeton+"/live/"+numero+".m3u8?cap="+xtreamCapacites, http.StatusFound)
}

// xtreamFichier : un film ou un episode, relaye tel quel au nom de la session (client natif).
func (a *API) xtreamFichier(w http.ResponseWriter, req *http.Request, x *etatXtream, episode bool) {
	s, err := a.sessionXtream(req.Context(), x, req.PathValue("u"), req.PathValue("p"), adresseXtream(req))
	if err != nil {
		refusXtream(w, err)
		return
	}
	m := reXtreamFichier.FindStringSubmatch(req.PathValue("fichier"))
	if m == nil || a.opt.Lecture == nil {
		http.NotFound(w, req)
		return
	}
	id := domain.ContenuID(a.fournisseurXtream() + ":" + m[1])
	if episode {
		if m[2] == "" {
			http.NotFound(w, req)
			return
		}
		id = domain.ContenuID(string(id) + "." + strings.ToLower(m[2]))
	}
	type_ := map[bool]string{true: "episode", false: "film"}[episode]
	a.inst.Compter("xtream_flux_total", 1, "type", type_)
	r2 := req.Clone(a.opt.Auth.ImposerSession(req.Context(), s.id))
	r2.Header.Set("X-Mytv-Client", "natif")
	sw := &suiveur{ResponseWriter: w}
	err = a.opt.Lecture.Servir(sw, r2, s.id, id)
	if err == nil || contexteAnnule(req.Context()) || sw.ecrit {
		return
	}
	if occupation.EstOccupation(err) || errors.Is(err, domain.ErrPlaceIndisponible) {
		w.Header().Set("Retry-After", "15")
		http.Error(w, "connexion du fournisseur occupee", http.StatusServiceUnavailable)
		return
	}
	if errors.Is(err, domain.ErrNonTrouve) || errors.Is(err, domain.ErrIntrouvable) {
		http.NotFound(w, req)
		return
	}
	a.log.Warn("xtream : lecture en echec", "contenu", string(id), "erreur", err)
	http.Error(w, "lecture impossible", http.StatusBadGateway)
}
