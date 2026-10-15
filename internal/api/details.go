package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"mytv/internal/domain"
	"mytv/internal/metadonnees"
)

// Details est ce que l'API attend du service de metadonnees (US-28). Nil = fonction
// absente : /films/{id} et /series/{id} rendent alors les seuls champs du catalogue.
type Details interface {
	Disponible(fournisseur string) bool
	Film(ctx context.Context, f domain.Film, recuperer bool) metadonnees.ResultatFilm
	Serie(ctx context.Context, s domain.SerieTV, recuperer bool) metadonnees.ResultatSerie
	Saisons(ctx context.Context, s domain.SerieTV) ([]domain.Saison, error)
}

type detailsCommunJSON struct {
	Synopsis     string  `json:"synopsis,omitempty"`
	Genre        string  `json:"genre,omitempty"`
	Casting      string  `json:"casting,omitempty"`
	Realisateur  string  `json:"realisateur,omitempty"`
	Pays         string  `json:"pays,omitempty"`
	Sortie       string  `json:"sortie,omitempty"`
	BandeAnnonce string  `json:"bande_annonce,omitempty"`
	ImageFond    string  `json:"image_fond,omitempty"`
	Affiche      string  `json:"affiche,omitempty"`
	Note         float64 `json:"note,omitempty"`
	Age          string  `json:"age,omitempty"`
}

type detailsFilmJSON struct {
	detailsCommunJSON
	Duree int64 `json:"duree,omitempty"` // secondes
}

type detailsSerieJSON struct {
	detailsCommunJSON
	DureeEpisode int64 `json:"duree_episode,omitempty"` // secondes
}

func (a *API) communJSON(d domain.Details) detailsCommunJSON {
	c := detailsCommunJSON{Synopsis: d.Synopsis, Genre: d.Genre, Casting: d.Casting, Realisateur: d.Realisateur,
		Pays: d.Pays, Sortie: d.Sortie, BandeAnnonce: d.BandeAnnonce, Note: d.Note, Age: d.Age}
	if len(d.ImageFond) > 0 {
		c.ImageFond = a.image(d.ImageFond[0])
	}
	if d.Affiche != "" {
		c.Affiche = a.image(d.Affiche)
	}
	return c
}

// filmDetailJSON : la fiche d'un film = les champs de la liste + les details.
type filmDetailJSON struct {
	filmJSON
	Details       *detailsFilmJSON   `json:"details,omitempty"`
	DetailsStatut metadonnees.Statut `json:"details_statut,omitempty"`
}

type serieDetailJSON struct {
	serieJSON
	Details       *detailsSerieJSON  `json:"details,omitempty"`
	DetailsStatut metadonnees.Statut `json:"details_statut,omitempty"`
}

// lireDetails : ?details=non evite tout appel au fournisseur (la reponse ne porte
// alors que ce qui est deja en cache).
func lireDetails(w http.ResponseWriter, req *http.Request) (recuperer, ok bool) {
	switch req.URL.Query().Get("details") {
	case "", "oui":
		return true, true
	case "non":
		return false, true
	}
	requeteInvalide(w, "details invalide (oui ou non)")
	return false, false
}

// ecrireJSONEtag ecrit une reponse dont le contenu change independamment de la
// synchronisation du catalogue (details arrives entre-temps) : le validateur est
// donc calcule sur le corps lui-meme.
func ecrireJSONEtag(w http.ResponseWriter, req *http.Request, v any) {
	corps, err := json.Marshal(v)
	if err != nil {
		erreur(w, http.StatusInternalServerError, "erreur_interne", "erreur interne")
		return
	}
	somme := sha256.Sum256(corps)
	tag := `W/"` + hex.EncodeToString(somme[:12]) + `"`
	w.Header().Set("ETag", tag)
	w.Header().Set("Cache-Control", "private, no-cache")
	if correspond(req.Header.Get("If-None-Match"), tag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(corps, '\n'))
}

func (a *API) ficheFilm(req *http.Request, f domain.Film, recuperer bool) any {
	base := a.versFilm(f) // l'image de la liste passe deja par le proxy d'images
	if a.opt.Details == nil || !a.opt.Details.Disponible(f.FournisseurID) {
		return base
	}
	r := a.opt.Details.Film(req.Context(), f, recuperer)
	out := filmDetailJSON{filmJSON: base, DetailsStatut: r.Statut}
	if r.Details != nil {
		out.Details = &detailsFilmJSON{a.communJSON(r.Details.Details), int64(r.Details.Duree / time.Second)}
	}
	return out
}

func (a *API) ficheSerie(req *http.Request, s domain.SerieTV, recuperer bool) any {
	base := a.versSerie(s)
	if a.opt.Details == nil || !a.opt.Details.Disponible(s.FournisseurID) {
		return base
	}
	r := a.opt.Details.Serie(req.Context(), s, recuperer)
	out := serieDetailJSON{serieJSON: base, DetailsStatut: r.Statut}
	if r.Details != nil {
		out.Details = &detailsSerieJSON{a.communJSON(r.Details.Details), int64(r.Details.DureeEpisode / time.Second)}
	}
	return out
}
