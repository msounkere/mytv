package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"mytv/internal/domain"
)

// reContenuID : « <fournisseur>:<numero> » (voir le contrat). Valide TOUT identifiant
// venant d'une URL avant qu'il atteigne le stockage ou un fournisseur.
var reContenuID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,32}:[A-Za-z0-9_.-]{1,64}$`)

const (
	longueurRechercheMax = 100
	longueurRechercheMin = 2 // pour /recherche seulement
)

type categorieJSON struct {
	ID          string      `json:"id"`
	Fournisseur string      `json:"fournisseur"`
	Type        domain.Type `json:"type"`
	Nom         string      `json:"nom"`
	Ordre       int         `json:"ordre"`
}

type chaineJSON struct {
	ID        domain.ContenuID `json:"id"`
	Nom       string           `json:"nom"`
	Logo      string           `json:"logo,omitempty"`
	Numero    int              `json:"numero"`
	EpgID     string           `json:"epg_id,omitempty"`
	Categorie string           `json:"categorie"`
	Ajoute    *time.Time       `json:"ajoute"`
	Lecture   string           `json:"lecture"`
}

type filmJSON struct {
	ID        domain.ContenuID `json:"id"`
	Nom       string           `json:"nom"`
	Image     string           `json:"image,omitempty"`
	Categorie string           `json:"categorie"`
	Extension string           `json:"extension"`
	Note      float64          `json:"note"`
	Annee     string           `json:"annee,omitempty"`
	Ajoute    *time.Time       `json:"ajoute"`
	Lecture   string           `json:"lecture"`
}

type serieJSON struct {
	ID        domain.ContenuID `json:"id"`
	Nom       string           `json:"nom"`
	Image     string           `json:"image,omitempty"`
	Categorie string           `json:"categorie"`
	Note      float64          `json:"note"`
	Annee     string           `json:"annee,omitempty"`
	Ajoute    *time.Time       `json:"ajoute"`
	Saisons   string           `json:"saisons"`
}

func idCategorie(fournisseur, id string) string { return fournisseur + ":" + id }

// enUTC : null quand la date est inconnue.
func enUTC(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

// versChaine : `logos` est le repli issu du guide (US-29), utilise SEULEMENT quand le
// fournisseur ne donne pas de logo ; le resultat passe, comme tout logo, par le proxy d'images.
func (a *API) versChaine(c domain.Chaine, logos map[domain.ContenuID]string) chaineJSON {
	logo := c.Logo
	if strings.TrimSpace(logo) == "" {
		logo = logos[c.ID]
	}
	return chaineJSON{ID: c.ID, Nom: c.Nom, Logo: a.image(logo), Numero: c.NumeroChaine, EpgID: c.EpgID,
		Categorie: idCategorie(c.FournisseurID, c.CategorieID), Ajoute: enUTC(c.Ajoute),
		Lecture: Prefixe + "/live/" + c.Numero + ".m3u8"}
}

func (a *API) versFilm(f domain.Film) filmJSON {
	return filmJSON{ID: f.ID, Nom: f.Nom, Image: a.image(f.Image), Categorie: idCategorie(f.FournisseurID, f.CategorieID),
		Extension: f.Extension, Note: f.Note, Annee: f.Annee, Ajoute: enUTC(f.Ajoute), Lecture: Prefixe + "/vod/" + string(f.ID)}
}

func (a *API) versSerie(s domain.SerieTV) serieJSON {
	return serieJSON{ID: s.ID, Nom: s.Nom, Image: a.image(s.Image), Categorie: idCategorie(s.FournisseurID, s.CategorieID),
		Note: s.Note, Annee: s.Annee, Ajoute: enUTC(s.Ajoute), Saisons: Prefixe + "/series/" + string(s.ID) + "/saisons"}
}

func (a *API) declarerCatalogue() {
	a.route("GET", "/categories", Session, a.categories)
	a.route("GET", "/chaines", Session, a.listeChaines)
	a.route("GET", "/chaines/{id}", Session, a.chaine)
	a.route("GET", "/films", Session, a.listeFilms)
	a.route("GET", "/films/{id}", Session, a.film)
	a.route("GET", "/series", Session, a.listeSeries)
	a.route("GET", "/series/{id}", Session, a.serie)
	a.route("GET", "/series/{id}/saisons", Session, a.saisons)
	a.route("GET", "/recherche", Session, a.recherche)
	a.route("GET", "/catalogue/version", Session, a.catalogueVersion)
	a.route("GET", "/catalogue/export", Session, a.catalogueExport)
}

// ---------------------------------------------------------------- ETag

// etag derive un validateur faible de la derniere synchronisation de chaque
// fournisseur et de la requete exacte : le catalogue ne change qu'a la synchro.
// Renvoie true quand la reponse 304 a deja ete envoyee.
func (a *API) etag(w http.ResponseWriter, req *http.Request) (fini bool) {
	h := sha256.New()
	_, logosEmp := a.logosEPG(req.Context())
	h.Write([]byte(req.URL.Path + "?" + req.URL.RawQuery + "|img=" + a.empreinteImages() + "|logos=" + logosEmp))
	for _, f := range a.opt.Fournisseurs {
		t, err := a.opt.Stockage.DerniereSynchro(req.Context(), f)
		if err != nil {
			a.erreurAmont(w, err, "catalogue")
			return true
		}
		h.Write([]byte("|" + f + "=" + strconv.FormatInt(t.UnixMilli(), 10)))
	}
	tag := `W/"` + hex.EncodeToString(h.Sum(nil)[:12]) + `"`
	w.Header().Set("ETag", tag)
	// private : la reponse n'est pas partageable par un cache intermediaire ;
	// no-cache : le navigateur doit revalider (cheap grace au 304).
	w.Header().Set("Cache-Control", "private, no-cache")
	if correspond(req.Header.Get("If-None-Match"), tag) {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	return false
}

// correspond applique la comparaison faible d'If-None-Match (liste ou « * »).
func correspond(entete, tag string) bool {
	if entete == "" {
		return false
	}
	faible := func(s string) string { return strings.TrimPrefix(strings.TrimSpace(s), "W/") }
	for _, c := range strings.Split(entete, ",") {
		if strings.TrimSpace(c) == "*" || faible(c) == faible(tag) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------- filtres

type filtre struct {
	fournisseur string // vide = tous
	categorie   string // id chez le fournisseur
	recherche   string
}

func (a *API) lireFiltre(w http.ResponseWriter, req *http.Request) (filtre, bool) {
	var f filtre
	q := req.URL.Query()
	if c := q.Get("categorie"); c != "" {
		fid, id, ok := strings.Cut(c, ":")
		if !ok || id == "" || len(c) > 128 || !a.pubs[fid] {
			requeteInvalide(w, "categorie invalide (attendu <fournisseur>:<id>)")
			return f, false
		}
		f.fournisseur, f.categorie = fid, id
	}
	f.recherche = strings.TrimSpace(q.Get("q"))
	if len(f.recherche) > longueurRechercheMax {
		requeteInvalide(w, "recherche trop longue")
		return f, false
	}
	return f, true
}

// fusion pagine une liste qui vit chez PLUSIEURS fournisseurs : les fournisseurs
// sont parcourus dans l'ordre de priorite, le decalage s'applique a l'ensemble. Le
// stockage ne sait pas compter ; on trouve la taille d'une tranche par sondage
// (existence d'un element a un decalage donne), seulement quand le decalage depasse
// ce qu'un fournisseur peut contenir.
func fusion[T any](ctx context.Context, fournisseurs []string, limite, decalage int,
	lire func(fournisseur string, limite, decalage int) ([]T, error)) (out []T, encore bool, err error) {
	reste := decalage
	for _, f := range fournisseurs {
		want := limite + 1 - len(out)
		if want <= 0 {
			break
		}
		els, err := lire(f, want, reste)
		if err != nil {
			return nil, false, err
		}
		if reste > 0 && len(els) == 0 {
			// Le decalage depasse ce fournisseur : combien en a-t-il ?
			n, err := tailleParSondage(ctx, func(i int) (bool, error) {
				e, err := lire(f, 1, i)
				return len(e) > 0, err
			})
			if err != nil {
				return nil, false, err
			}
			reste -= n
			if reste < 0 {
				reste = 0
			}
			continue
		}
		reste = 0
		out = append(out, els...)
	}
	if len(out) > limite {
		return out[:limite], true, nil
	}
	return out, false, nil
}

// tailleParSondage trouve n tel que existe(i) pour i < n et pas pour i >= n.
func tailleParSondage(ctx context.Context, existe func(int) (bool, error)) (int, error) {
	haut := 1
	for {
		if err := ctx.Err(); err != nil {
			return 0, err
		}
		ok, err := existe(haut - 1)
		if err != nil {
			return 0, err
		}
		if !ok {
			break
		}
		if haut > decalageMax {
			return haut, nil
		}
		haut *= 2
	}
	bas := haut / 2 // existe(bas-1) est vrai (ou bas == 0)
	for bas < haut {
		mid := (bas + haut) / 2
		ok, err := existe(mid) // mid existe => n > mid
		if err != nil {
			return 0, err
		}
		if ok {
			bas = mid + 1
		} else {
			haut = mid
		}
	}
	return bas, nil
}

// fournisseursVises : un seul si la categorie en nomme un, sinon tous.
func (a *API) fournisseursVises(f filtre) []string {
	if f.fournisseur != "" {
		return []string{f.fournisseur}
	}
	return a.opt.Fournisseurs
}

// fournisseursChaines : le relais du direct est MONO-fournisseur (lot 2) ; annoncer
// une chaine d'un autre fournisseur serait annoncer une lecture impossible.
func (a *API) fournisseursChaines(f filtre) []string {
	live := a.opt.FournisseurLive
	if live == "" {
		return a.fournisseursVises(f)
	}
	if f.fournisseur != "" && f.fournisseur != live {
		return nil
	}
	return []string{live}
}

// ---------------------------------------------------------------- categories

func (a *API) categories(w http.ResponseWriter, req *http.Request) {
	t := domain.Type(req.URL.Query().Get("type"))
	if t != domain.TypeLive && t != domain.TypeFilm && t != domain.TypeSerie {
		requeteInvalide(w, "type invalide (live, vod, series)")
		return
	}
	limite, decalage, ok := a.pagination(w, req)
	if !ok || a.etag(w, req) {
		return
	}
	cats, err := a.opt.Stockage.Categories(req.Context(), t)
	if err != nil {
		a.erreurAmont(w, err, "categories")
		return
	}
	if t == domain.TypeLive && a.opt.FournisseurLive != "" {
		cats = slices.DeleteFunc(slices.Clone(cats), func(c domain.Categorie) bool { return c.FournisseurID != a.opt.FournisseurLive })
	}
	fin := min(decalage+limite, len(cats))
	debut := min(decalage, fin)
	out := make([]categorieJSON, 0, fin-debut)
	for _, c := range cats[debut:fin] {
		out = append(out, categorieJSON{ID: idCategorie(c.FournisseurID, c.ID), Fournisseur: c.FournisseurID, Type: c.Type, Nom: c.Nom, Ordre: c.Ordre})
	}
	ecrireJSONCache(w, nouvellePage(out, limite, decalage, fin < len(cats)))
}

// ecrireJSONCache ecrit une reponse 200 en conservant l'ETag et le Cache-Control poses.
func ecrireJSONCache(w http.ResponseWriter, v any) { ecrireJSON(w, http.StatusOK, v) }

// ---------------------------------------------------------------- chaines

func (a *API) listeChaines(w http.ResponseWriter, req *http.Request) {
	f, ok := a.lireFiltre(w, req)
	if !ok {
		return
	}
	limite, decalage, ok := a.pagination(w, req)
	if !ok || a.etag(w, req) {
		return
	}
	els, encore, err := fusion(req.Context(), a.fournisseursChaines(f), limite, decalage,
		func(fid string, l, d int) ([]domain.Chaine, error) {
			return a.opt.Stockage.Chaines(req.Context(), domain.FiltreChaines{FournisseurID: fid, CategorieID: f.categorie, Recherche: f.recherche, Limite: l, Decalage: d})
		})
	if err != nil {
		a.erreurAmont(w, err, "chaines")
		return
	}
	logos, _ := a.logosEPG(req.Context())
	out := make([]chaineJSON, len(els))
	for i, c := range els {
		out[i] = a.versChaine(c, logos)
	}
	ecrireJSONCache(w, nouvellePage(out, limite, decalage, encore))
}

// idDuChemin valide l'identifiant de contenu d'une URL.
func idDuChemin(w http.ResponseWriter, req *http.Request) (domain.ContenuID, bool) {
	id := req.PathValue("id")
	if !reContenuID.MatchString(id) {
		requeteInvalide(w, "identifiant de contenu invalide")
		return "", false
	}
	return domain.ContenuID(id), true
}

func (a *API) chaine(w http.ResponseWriter, req *http.Request) {
	id, ok := idDuChemin(w, req)
	if !ok || a.etag(w, req) {
		return
	}
	c, err := a.opt.Stockage.Chaine(req.Context(), id)
	if err == nil && a.opt.FournisseurLive != "" && c.FournisseurID != a.opt.FournisseurLive {
		err = domain.ErrNonTrouve
	}
	if err != nil {
		a.erreurAmont(w, err, "chaine")
		return
	}
	logos, _ := a.logosEPG(req.Context())
	ecrireJSONCache(w, a.versChaine(*c, logos))
}

// ---------------------------------------------------------------- films et series

func (a *API) video(w http.ResponseWriter) (domain.StockageVideo, bool) {
	if a.opt.Video == nil {
		erreur(w, http.StatusNotImplemented, "non_disponible", "films et series non actives")
		return nil, false
	}
	return a.opt.Video, true
}

func (a *API) listeFilms(w http.ResponseWriter, req *http.Request) {
	v, ok := a.video(w)
	if !ok {
		return
	}
	f, ok := a.lireFiltre(w, req)
	if !ok {
		return
	}
	limite, decalage, ok := a.pagination(w, req)
	if !ok || a.etag(w, req) {
		return
	}
	els, encore, err := fusion(req.Context(), a.fournisseursVises(f), limite, decalage,
		func(fid string, l, d int) ([]domain.Film, error) {
			return v.Films(req.Context(), domain.FiltreChaines{FournisseurID: fid, CategorieID: f.categorie, Recherche: f.recherche, Limite: l, Decalage: d})
		})
	if err != nil {
		a.erreurAmont(w, err, "films")
		return
	}
	out := make([]filmJSON, len(els))
	for i, e := range els {
		out[i] = a.versFilm(e)
	}
	ecrireJSONCache(w, nouvellePage(out, limite, decalage, encore))
}

func (a *API) film(w http.ResponseWriter, req *http.Request) {
	v, ok := a.video(w)
	if !ok {
		return
	}
	id, ok := idDuChemin(w, req)
	if !ok || a.etag(w, req) {
		return
	}
	f, err := v.Film(req.Context(), id)
	if err != nil {
		a.erreurAmont(w, err, "film")
		return
	}
	ecrireJSONCache(w, a.versFilm(*f))
}

func (a *API) listeSeries(w http.ResponseWriter, req *http.Request) {
	v, ok := a.video(w)
	if !ok {
		return
	}
	f, ok := a.lireFiltre(w, req)
	if !ok {
		return
	}
	limite, decalage, ok := a.pagination(w, req)
	if !ok || a.etag(w, req) {
		return
	}
	els, encore, err := fusion(req.Context(), a.fournisseursVises(f), limite, decalage,
		func(fid string, l, d int) ([]domain.SerieTV, error) {
			return v.Series(req.Context(), domain.FiltreChaines{FournisseurID: fid, CategorieID: f.categorie, Recherche: f.recherche, Limite: l, Decalage: d})
		})
	if err != nil {
		a.erreurAmont(w, err, "series")
		return
	}
	out := make([]serieJSON, len(els))
	for i, e := range els {
		out[i] = a.versSerie(e)
	}
	ecrireJSONCache(w, nouvellePage(out, limite, decalage, encore))
}

func (a *API) serie(w http.ResponseWriter, req *http.Request) {
	v, ok := a.video(w)
	if !ok {
		return
	}
	id, ok := idDuChemin(w, req)
	if !ok || a.etag(w, req) {
		return
	}
	s, err := v.Serie(req.Context(), id)
	if err != nil {
		a.erreurAmont(w, err, "serie")
		return
	}
	ecrireJSONCache(w, a.versSerie(*s))
}

type episodeJSON struct {
	ID        domain.ContenuID `json:"id"`
	Numero    int              `json:"numero"`
	Titre     string           `json:"titre"`
	Extension string           `json:"extension"`
	Duree     int64            `json:"duree"`
	Lecture   string           `json:"lecture"`
}

type saisonJSON struct {
	Numero   int           `json:"numero"`
	Episodes []episodeJSON `json:"episodes"`
}

func (a *API) saisons(w http.ResponseWriter, req *http.Request) {
	v, ok := a.video(w)
	if !ok {
		return
	}
	id, ok := idDuChemin(w, req)
	if !ok {
		return
	}
	// La serie doit exister dans NOTRE catalogue : l'identifiant de l'URL n'atteint
	// jamais le fournisseur sans avoir ete reconnu.
	s, err := v.Serie(req.Context(), id)
	if err != nil {
		a.erreurAmont(w, err, "serie")
		return
	}
	fv := a.opt.VOD[s.FournisseurID]
	if fv == nil {
		erreur(w, http.StatusNotImplemented, "non_disponible", "saisons non disponibles pour ce fournisseur")
		return
	}
	ss, err := fv.Saisons(req.Context(), s.Numero)
	if err != nil {
		if contexteAnnule(req.Context()) {
			return
		}
		a.erreurAmont(w, err, "saisons")
		return
	}
	out := make([]saisonJSON, len(ss))
	for i, sa := range ss {
		out[i] = saisonJSON{Numero: sa.Numero, Episodes: make([]episodeJSON, len(sa.Episodes))}
		for j, e := range sa.Episodes {
			out[i].Episodes[j] = episodeJSON{ID: e.ID, Numero: e.Numero, Titre: e.Titre, Extension: e.Extension,
				Duree: int64(e.Duree / time.Second), Lecture: Prefixe + "/vod/" + string(e.ID)}
		}
	}
	ecrireJSON(w, http.StatusOK, map[string]any{"elements": out})
}

// ---------------------------------------------------------------- recherche

func (a *API) recherche(w http.ResponseWriter, req *http.Request) {
	q := strings.TrimSpace(req.URL.Query().Get("q"))
	if len(q) < longueurRechercheMin || len(q) > longueurRechercheMax {
		requeteInvalide(w, "q doit faire entre "+strconv.Itoa(longueurRechercheMin)+" et "+strconv.Itoa(longueurRechercheMax)+" caracteres")
		return
	}
	limite, _, ok := a.pagination(w, req)
	if !ok || a.etag(w, req) {
		return
	}
	ctx := req.Context()
	res := struct {
		Chaines []chaineJSON `json:"chaines"`
		Films   []filmJSON   `json:"films"`
		Series  []serieJSON  `json:"series"`
	}{[]chaineJSON{}, []filmJSON{}, []serieJSON{}}

	cs, _, err := fusion(ctx, a.fournisseursChaines(filtre{}), limite, 0, func(fid string, l, d int) ([]domain.Chaine, error) {
		return a.opt.Stockage.Chaines(ctx, domain.FiltreChaines{FournisseurID: fid, Recherche: q, Limite: l, Decalage: d})
	})
	if err != nil {
		a.erreurAmont(w, err, "recherche")
		return
	}
	logos, _ := a.logosEPG(ctx)
	for _, c := range cs {
		res.Chaines = append(res.Chaines, a.versChaine(c, logos))
	}
	if v := a.opt.Video; v != nil {
		fs, _, err := fusion(ctx, a.opt.Fournisseurs, limite, 0, func(fid string, l, d int) ([]domain.Film, error) {
			return v.Films(ctx, domain.FiltreChaines{FournisseurID: fid, Recherche: q, Limite: l, Decalage: d})
		})
		if err != nil {
			a.erreurAmont(w, err, "recherche")
			return
		}
		for _, f := range fs {
			res.Films = append(res.Films, a.versFilm(f))
		}
		ss, _, err := fusion(ctx, a.opt.Fournisseurs, limite, 0, func(fid string, l, d int) ([]domain.SerieTV, error) {
			return v.Series(ctx, domain.FiltreChaines{FournisseurID: fid, Recherche: q, Limite: l, Decalage: d})
		})
		if err != nil {
			a.erreurAmont(w, err, "recherche")
			return
		}
		for _, s := range ss {
			res.Series = append(res.Series, a.versSerie(s))
		}
	}
	ecrireJSONCache(w, res)
}
