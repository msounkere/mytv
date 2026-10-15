package api

import (
	"bufio"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"mytv/internal/domain"
)

// versionCatalogue est le validateur commun a /catalogue/version et a TOUS les
// exports : il ne depend que de la derniere synchronisation de chaque fournisseur,
// jamais de l'URL. Tant qu'il ne change pas, l'interface n'a rien a retelecharger.
func (a *API) versionCatalogue(req *http.Request) (string, error) {
	h := sha256.New()
	h.Write([]byte("img=" + a.empreinteImages()))
	for _, f := range a.opt.Fournisseurs {
		t, err := a.opt.Stockage.DerniereSynchro(req.Context(), f)
		if err != nil {
			return "", err
		}
		h.Write([]byte("|" + f + "=" + strconv.FormatInt(t.UnixMilli(), 10)))
	}
	return hex.EncodeToString(h.Sum(nil)[:12]), nil
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
func (a *API) ligneExport(v any) any {
	switch x := v.(type) {
	case domain.Categorie:
		return categorieJSON{ID: idCategorie(x.FournisseurID, x.ID), Fournisseur: x.FournisseurID, Type: x.Type, Nom: x.Nom, Ordre: x.Ordre}
	case domain.Chaine:
		return a.versChaine(x)
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
	exp, err := ex.OuvrirExport(req.Context(), t, a.opt.FournisseurLive)
	if err != nil {
		a.erreurAmont(w, err, "catalogue")
		return
	}
	defer exp.Fermer()

	// Un export est long mais borne : delai propre plutot que l'absence de delai.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Now().Add(a.opt.Config.DelaiExport.Std()))

	h := w.Header()
	h.Set("Content-Type", "application/x-ndjson; charset=utf-8")
	h.Set("X-Elements", strconv.Itoa(exp.Elements()))
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
	tampon := bufio.NewWriterSize(sortie, 64<<10)
	enc := json.NewEncoder(tampon)
	err = exp.Lire(func(v any) error {
		if e := req.Context().Err(); e != nil {
			return e
		}
		return enc.Encode(a.ligneExport(v)) // ajoute le saut de ligne
	})
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
