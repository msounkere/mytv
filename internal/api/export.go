package api

import (
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"mytv/internal/domain"
)

// ExportDifferences : ce que le stockage SQLite ajoute a domain.ExportCatalogue pour
// la version fondee sur le contenu et l'export par difference (US-36). Facultatif :
// sans lui, la version retombe sur l'heure de synchronisation et `depuis` repond
// 409 (import complet).
type ExportDifferences interface {
	// VersionCatalogue : empreinte globale du contenu (vide si aucun catalogue).
	VersionCatalogue(ctx context.Context) (string, error)
	// VersionsConnues : empreintes dont les differences sont encore journalisees.
	VersionsConnues(ctx context.Context) ([]string, error)
	// OuvrirDifferences : lignes ajoutees/modifiees depuis `depuis` et identifiants
	// supprimes ; domain.ErrNonTrouve si la version est inconnue ou purgee.
	OuvrirDifferences(ctx context.Context, t domain.TypeExport, fournisseurLive, depuis string) (domain.Export, []string, error)
}

// composantesVersion : ce qui entre dans la version du catalogue EN PLUS de son
// contenu : le sceau des images et les logos de repli du guide (ils changent les
// lignes exportees sans passer par le stockage).
func (a *API) composantesVersion(req *http.Request) string {
	_, logosEmp := a.logosEPG(req.Context())
	return "img=" + a.empreinteImages() + "|logos=" + logosEmp
}

// versionDe compose la version annoncee (ETag) a partir des composantes et de
// l'empreinte du contenu.
func versionDe(composantes, contenu string) string {
	h := sha256.Sum256([]byte(composantes + "|cat=" + contenu))
	return hex.EncodeToString(h[:12])
}

// versionCatalogue est le validateur commun a /catalogue/version et a TOUS les
// exports : il ne depend que du CONTENU du catalogue (empreinte calculee a
// l'ecriture, US-36 CA1), des images et des logos du guide, jamais de l'URL ni de
// l'heure. Tant qu'il ne change pas, l'interface n'a rien a retelecharger.
func (a *API) versionCatalogue(req *http.Request) (string, error) {
	if ed, ok := a.opt.Export.(ExportDifferences); ok {
		contenu, err := ed.VersionCatalogue(req.Context())
		if err != nil {
			return "", err
		}
		return versionDe(a.composantesVersion(req), contenu), nil
	}
	// Stockage sans empreinte : heure de la derniere synchronisation de chaque fournisseur.
	var b strings.Builder
	for _, f := range a.opt.Fournisseurs {
		t, err := a.opt.Stockage.DerniereSynchro(req.Context(), f)
		if err != nil {
			return "", err
		}
		b.WriteString("|" + f + "=" + strconv.FormatInt(t.UnixMilli(), 10))
	}
	return versionDe(a.composantesVersion(req), b.String()), nil
}

// resoudreDepuis retrouve l'empreinte de contenu qui correspond a une version
// annoncee autrefois au client : parmi les versions journalisees (et la courante),
// celle qui, composee avec les images et logos ACTUELS, redonne `depuis`. Si les
// logos ont change entre-temps, aucune ne correspond : import complet (les lignes
// de chaines ont change sans que le journal le sache).
func (a *API) resoudreDepuis(req *http.Request, ed ExportDifferences, depuis, version string) (string, error) {
	contenu, err := ed.VersionCatalogue(req.Context())
	if err != nil {
		return "", err
	}
	if depuis == version {
		return contenu, nil
	}
	connues, err := ed.VersionsConnues(req.Context())
	if err != nil {
		return "", err
	}
	comp := a.composantesVersion(req)
	for i := len(connues) - 1; i >= 0; i-- {
		if versionDe(comp, connues[i]) == depuis {
			return connues[i], nil
		}
	}
	return "", domain.ErrNonTrouve
}

// etagVersion pose l'ETag et rend true si le client l'a deja (304 envoye).
func etagVersion(w http.ResponseWriter, req *http.Request, version string) bool {
	tag := `W/"` + version + `"`
	w.Header().Set("ETag", tag)
	w.Header().Set("Cache-Control", "private, no-cache")
	w.Header().Add("Vary", "Accept-Encoding")
	if correspond(req.Header.Get("If-None-Match"), tag) {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	return false
}

func (a *API) exportCatalogue(w http.ResponseWriter) (domain.ExportCatalogue, bool) {
	if a.opt.Export == nil {
		erreur(w, http.StatusNotImplemented, "non_disponible", "export du catalogue non active")
		return nil, false
	}
	return a.opt.Export, true
}

// suppressionsJSON : premiere ligne d'un export par difference (schema `Suppressions`).
type suppressionsJSON struct {
	Type string   `json:"type"`
	IDs  []string `json:"ids"`
}

type manifesteJSON struct {
	Version     string     `json:"version"`
	Categories  int        `json:"categories"`
	Chaines     int        `json:"chaines"`
	Films       int        `json:"films"`
	Series      int        `json:"series"`
	Synchronise *time.Time `json:"synchronise"`
}

// catalogueVersion : une requete SQL de comptage, pour decider d'un import sans
// rien telecharger.
func (a *API) catalogueVersion(w http.ResponseWriter, req *http.Request) {
	ex, ok := a.exportCatalogue(w)
	if !ok {
		return
	}
	version, err := a.versionCatalogue(req)
	if err != nil {
		a.erreurAmont(w, err, "catalogue")
		return
	}
	if etagVersion(w, req, version) {
		return
	}
	m, err := ex.Manifeste(req.Context(), a.opt.FournisseurLive)
	if err != nil {
		a.erreurAmont(w, err, "catalogue")
		return
	}
	ecrireJSONCache(w, manifesteJSON{Version: version, Categories: m.Categories, Chaines: m.Chaines,
		Films: m.Films, Series: m.Series, Synchronise: enUTC(m.Synchronise)})
}

// ligneExport convertit un element du stockage en son JSON de l'API : EXACTEMENT les
// schemas des listes paginees.
func (a *API) ligneExport(v any, logos map[domain.ContenuID]string) any {
	switch x := v.(type) {
	case domain.Categorie:
		return categorieJSON{ID: idCategorie(x.FournisseurID, x.ID), Fournisseur: x.FournisseurID, Type: x.Type, Nom: x.Nom, Ordre: x.Ordre}
	case domain.Chaine:
		return a.versChaine(x, logos)
	case domain.Film:
		return a.versFilm(x)
	case domain.SerieTV:
		return a.versSerie(x)
	}
	return nil
}

// catalogueExport envoie un type de contenu en NDJSON, au fil de l'eau : ni le
// catalogue ni la reponse ne sont jamais tenus en memoire (65 000 lignes = quelques
// Mo de tampons). Les en-tetes (ETag, X-Elements) partent avant le corps ; le
// nombre vient du MEME instantane SQLite que les lignes.
func (a *API) catalogueExport(w http.ResponseWriter, req *http.Request) {
	ex, ok := a.exportCatalogue(w)
	if !ok {
		return
	}
	t := domain.TypeExport(req.URL.Query().Get("type"))
	switch t {
	case domain.ExportCategories, domain.ExportLive, domain.ExportVOD, domain.ExportSeries:
	default:
		requeteInvalide(w, "type invalide (categories, live, vod, series)")
		return
	}
	version, err := a.versionCatalogue(req)
	if err != nil {
		a.erreurAmont(w, err, "catalogue")
		return
	}
	if etagVersion(w, req, version) {
		return
	}
	// La version est lue AVANT l'instantane : si une synchronisation committe entre
	// les deux, le corps est plus recent que l'ETag annonce, donc le client reverra
	// un ETag different au prochain controle (jamais l'inverse).
	var (
		exp       domain.Export
		supprimes []string // export par difference seulement (US-36 CA3)
	)
	if depuis := req.URL.Query().Get("depuis"); depuis != "" && t != domain.ExportCategories {
		ed, ok := ex.(ExportDifferences)
		if !ok {
			erreur(w, http.StatusConflict, "version_inconnue", "export par difference non disponible : import complet necessaire")
			return
		}
		contenu, err := a.resoudreDepuis(req, ed, depuis, version)
		if err == nil {
			exp, supprimes, err = ed.OuvrirDifferences(req.Context(), t, a.opt.FournisseurLive, contenu)
		}
		if errors.Is(err, domain.ErrNonTrouve) {
			erreur(w, http.StatusConflict, "version_inconnue", "version inconnue ou trop ancienne : import complet necessaire")
			return
		}
		if err != nil {
			a.erreurAmont(w, err, "catalogue")
			return
		}
	} else {
		if exp, err = ex.OuvrirExport(req.Context(), t, a.opt.FournisseurLive); err != nil {
			a.erreurAmont(w, err, "catalogue")
			return
		}
	}
	defer exp.Fermer()

	// Un export est long mais borne : delai propre plutot que l'absence de delai.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(a.opt.Config.DelaiExport.Std()))

	h := w.Header()
	h.Set("Content-Type", "application/x-ndjson; charset=utf-8")
	h.Set("X-Elements", strconv.Itoa(exp.Elements()))
	if supprimes != nil {
		h.Set("X-Supprimes", strconv.Itoa(len(supprimes)))
	}
	var sortie io.Writer = w
	var gz *gzip.Writer
	if accepteGzip(req) {
		h.Set("Content-Encoding", "gzip")
		gz = gzip.NewWriter(w)
		sortie = gz
	}
	w.WriteHeader(http.StatusOK)
	if req.Method == http.MethodHead {
		return
	}
	var logos map[domain.ContenuID]string
	if t == domain.ExportLive {
		logos, _ = a.logosEPG(req.Context()) // une fois : evite une requete par chaine
	}
	tampon := bufio.NewWriterSize(sortie, 64<<10)
	enc := json.NewEncoder(tampon)
	if supprimes != nil {
		// Premiere ligne d'une difference : les identifiants disparus depuis `depuis`
		// (toujours presente, meme vide : c'est elle qui dit « difference » au client).
		err = enc.Encode(suppressionsJSON{Type: "suppressions", IDs: supprimes})
	}
	if err == nil {
		err = exp.Lire(func(v any) error {
			if e := req.Context().Err(); e != nil {
				return e
			}
			return enc.Encode(a.ligneExport(v, logos)) // ajoute le saut de ligne
		})
	}
	if err == nil {
		err = tampon.Flush()
	}
	if err == nil && gz != nil {
		err = gz.Close()
	}
	if err != nil {
		// Les en-tetes sont partis : le client ne peut pas savoir que le corps est
		// incomplet autrement que par une connexion coupee (gzip tronque, ligne
		// coupee, moins de lignes que X-Elements).
		if req.Context().Err() == nil {
			a.log.Error("export interrompu", "type", string(t), "erreur", err)
		}
		panic(http.ErrAbortHandler)
	}
}

func accepteGzip(req *http.Request) bool {
	for _, c := range strings.Split(req.Header.Get("Accept-Encoding"), ",") {
		c = strings.TrimSpace(c)
		nom, q, _ := strings.Cut(c, ";")
		if strings.EqualFold(strings.TrimSpace(nom), "gzip") {
			q = strings.TrimSpace(q)
			return q != "q=0" && q != "q=0.0"
		}
	}
	return false
}
