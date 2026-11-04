package api

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"

	"mytv/internal/domain"
)

// Liste « A voir » et preparation d'avance (US-42). Les routes de la liste ont le niveau
// Session et visent toujours le profil ACTIF de la session (comme /profil/favoris).

// Preparation est ce que l'API attend du moteur de preparation (internal/preparation).
// Nil = fonction absente : la liste reste tenue, les etats sont `attente`.
type Preparation interface {
	// EtatProfil : l'etat de preparation de chaque element de la liste d'un profil, avec la
	// raison de chaque attente et l'echeance de chaque echec transitoire (US-44).
	EtatProfil(ctx context.Context, profilID string) (map[domain.AVoirID]domain.EtatAVoirDetaille, error)
	// Reveiller demande une planification sans attendre (liste ou historique modifies).
	Reveiller()
	// Reessayer remet en attente, tout de suite, les fichiers en echec d'un element de la
	// liste du profil ; idempotent (US-44 CA3).
	Reessayer(ctx context.Context, profilID string, id domain.AVoirID) error
	// Fichiers et Occupe : etat des fichiers sur le disque (administration) ; Budget : la
	// borne preparation.budget (octets).
	Fichiers() []domain.FichierDetaille
	Occupe() int64
	Budget() int64
	// EpisodesAvanceDefaut : preparation.episodes_avance, le reglage du serveur applique a
	// une serie dont l'element a `episodes_avance` = 0 (US-44 CA8).
	EpisodesAvanceDefaut() int
	// Vider efface les fichiers qui ne sont pas en cours de lecture ; rend leur nombre.
	Vider() int
}

// Sonde est la sonde de disponibilite (internal/preparation.Sonde). Nil = 501.
type Sonde interface {
	Disponibilite(ctx context.Context, session string, id, serie domain.ContenuID) (domain.Disponibilite, error)
}

// reAVoirID : « vod:<fournisseur>:<numero> » ou « serie:<fournisseur>:<numero> ».
var reAVoirID = regexp.MustCompile(`^(vod|serie):[A-Za-z0-9_-]{1,32}:[A-Za-z0-9_.-]{1,64}$`)

func (a *API) declarerAVoir() {
	a.route("GET", "/profil/a_voir", Session, a.aVoir)
	a.route("PUT", "/profil/a_voir", Session, a.ordonnerAVoir)
	a.route("PUT", "/profil/a_voir/{id}", Session, a.ajouterAVoir)
	a.route("DELETE", "/profil/a_voir/{id}", Session, a.retirerAVoir)
	a.route("POST", "/profil/a_voir/{id}/reessayer", Session, a.reessayerAVoir)
	a.route("GET", "/vod/{id}/disponibilite", Session, a.disponibilite)
	a.route("GET", "/admin/preparation", Admin, a.preparationAdmin)
	a.route("DELETE", "/admin/preparation", Admin, a.viderPreparation)
}

// listeActive : la liste est disponible et un profil est actif.
func (a *API) listeActive(w http.ResponseWriter, req *http.Request) (string, bool) {
	if a.opt.AVoir == nil {
		erreur(w, http.StatusNotImplemented, "non_disponible", "liste « a voir » non activee")
		return "", false
	}
	return a.profilActif(w, req)
}

func aVoirIDDuChemin(w http.ResponseWriter, req *http.Request) (domain.AVoirID, domain.ContenuID, bool, bool) {
	id := domain.AVoirID(req.PathValue("id"))
	if !reAVoirID.MatchString(string(id)) {
		requeteInvalide(w, "identifiant « a voir » invalide (vod:<contenu> ou serie:<contenu>)")
		return "", "", false, false
	}
	contenu, serie, _ := id.Decomposer()
	if !serie && estEpisode(contenu) {
		requeteInvalide(w, "un episode ne s'ajoute pas : ajoutez sa serie avec `depuis`")
		return "", "", false, false
	}
	return id, contenu, serie, true
}

type depuisJSON struct {
	Saison  int `json:"saison"`
	Episode int `json:"episode"`
}

// aVoirEpisodeJSON : un episode retenu pour une serie (contrat arbitre du lot 9), avec la
// raison de son attente ou l'echeance de sa nouvelle tentative (lot 10).
type aVoirEpisodeJSON struct {
	Contenu   domain.ContenuID       `json:"contenu"`
	Saison    int                    `json:"saison"`
	Episode   int                    `json:"episode"`
	Etat      domain.EtatPreparation `json:"etat"`
	Octets    *int64                 `json:"octets,omitempty"`
	Total     *int64                 `json:"total,omitempty"`
	DebitKbps *int                   `json:"debit_kbps,omitempty"`
	Cause     string                 `json:"cause,omitempty"`
	Raison    domain.RaisonAttente   `json:"raison,omitempty"`
	Position  int                    `json:"position,omitempty"`
	Reessai   *time.Time             `json:"reessai,omitempty"`
}

// aVoirElementJSON : `contenu` porte l'identifiant « a voir » lui-meme (`vod:...` ou
// `serie:...`) ; pour un film, l'avancement est a plat ; pour une serie, `episodes` liste
// les episodes retenus et `etat` est celui du premier.
type aVoirElementJSON struct {
	Contenu        domain.AVoirID         `json:"contenu"`
	Rang           int                    `json:"rang"`
	Ajoute         time.Time              `json:"ajoute"`
	EpisodesAvance int                    `json:"episodes_avance"`
	Depuis         *depuisJSON            `json:"depuis,omitempty"`
	Etat           domain.EtatPreparation `json:"etat"`
	Octets         *int64                 `json:"octets,omitempty"`
	Total          *int64                 `json:"total,omitempty"`
	DebitKbps      *int                   `json:"debit_kbps,omitempty"`
	Cause          string                 `json:"cause,omitempty"`
	Raison         domain.RaisonAttente   `json:"raison,omitempty"`
	Position       int                    `json:"position,omitempty"`
	Reessai        *time.Time             `json:"reessai,omitempty"`
	Episodes       []aVoirEpisodeJSON     `json:"episodes,omitempty"`
}

// budgetJSON : ce que la preparation occupe (prets et partiels, tous profils) et sa borne.
type budgetJSON struct {
	Utilise int64 `json:"utilise"`
	Total   int64 `json:"total"`
}

// fichierPrepareJSON : un fichier suivi par la preparation (administration).
type fichierPrepareJSON struct {
	Contenu   domain.ContenuID       `json:"contenu,omitempty"`
	Etat      domain.EtatPreparation `json:"etat"`
	Octets    int64                  `json:"octets"`
	Total     int64                  `json:"total"`
	DebitKbps int                    `json:"debit_kbps,omitempty"`
	Cause     string                 `json:"cause,omitempty"`
	Saison    int                    `json:"saison,omitempty"`
	Episode   int                    `json:"episode,omitempty"`
	Serie     domain.ContenuID       `json:"serie,omitempty"`
	Reessai   *time.Time             `json:"reessai,omitempty"`
}

func versFichierPrepare(f domain.FichierDetaille) fichierPrepareJSON {
	return fichierPrepareJSON{Contenu: f.Contenu, Etat: f.Etat, Octets: f.Octets, Total: f.Total, DebitKbps: f.DebitKbps,
		Cause: f.Cause, Saison: f.Saison, Episode: f.Episode, Serie: f.Serie, Reessai: echeance(f)}
}

// echeance : la date de nouvelle tentative d'un echec transitoire, en UTC ; nil sinon.
func echeance(f domain.FichierDetaille) *time.Time {
	if f.Reessai.IsZero() {
		return nil
	}
	t := f.Reessai.UTC()
	return &t
}

// avancement : octets, total et debit ne sont donnes que lorsqu'ils signifient quelque chose
// (telechargement entame ou fichier pret).
func avancement(f domain.FichierPrepare) (octets, total *int64, debit *int) {
	if f.Etat == domain.PreparationEnCours || f.Etat == domain.PreparationPrete || f.Octets > 0 || f.Total > 0 {
		o, t := f.Octets, f.Total
		octets, total = &o, &t
	}
	if f.DebitKbps > 0 {
		d := f.DebitKbps
		debit = &d
	}
	return
}

// aVoir rend la liste ordonnee avec l'etat de preparation de chaque element. Un film
// termine (vu) ou une serie dont tous les episodes sont vus sort de la liste ici meme,
// avec un journal INFO.
func (a *API) aVoir(w http.ResponseWriter, req *http.Request) {
	p, ok := a.listeActive(w, req)
	if !ok {
		return
	}
	liste, err := a.opt.AVoir.AVoir(req.Context(), p)
	if err != nil {
		a.erreurProfil(w, err)
		return
	}
	var etats map[domain.AVoirID]domain.EtatAVoirDetaille
	if a.opt.Preparation != nil {
		if etats, err = a.opt.Preparation.EtatProfil(req.Context(), p); err != nil {
			a.log.Warn("etat de preparation indisponible", "erreur", err)
			etats = nil
		}
	}
	out := make([]aVoirElementJSON, 0, len(liste))
	for _, e := range liste {
		etat := domain.EtatAVoirDetaille{Etat: domain.PreparationAttente}
		if et, ok := etats[e.ID]; ok {
			etat = et
		}
		if etat.Termine {
			if err := a.opt.AVoir.RetirerAVoir(req.Context(), p, e.ID); err != nil {
				a.log.Warn("retrait d'un element termine", "erreur", err)
			} else {
				a.log.Info("element « a voir » termine, retire de la liste", "element", string(e.ID))
				a.inst.Compter("a_voir_termines_total", 1)
			}
			continue
		}
		el := aVoirElementJSON{Contenu: e.ID, Rang: len(out) + 1, Ajoute: e.Ajoute.UTC(), EpisodesAvance: e.EpisodesAvance, Etat: etat.Etat}
		switch {
		case e.Serie:
			if e.DepuisSaison > 0 {
				el.Depuis = &depuisJSON{e.DepuisSaison, e.DepuisEpisode}
			}
			for _, f := range etat.Fichiers {
				ep := aVoirEpisodeJSON{Contenu: f.Contenu, Saison: f.Saison, Episode: f.Episode, Etat: f.Etat, Cause: f.Cause,
					Raison: f.Raison, Position: f.Position, Reessai: echeance(f)}
				ep.Octets, ep.Total, ep.DebitKbps = avancement(f.FichierPrepare)
				el.Episodes = append(el.Episodes, ep)
			}
			if len(etat.Fichiers) > 0 {
				// L'element porte la raison et l'echeance de son premier episode (comme l'etat).
				el.Raison, el.Position, el.Reessai = etat.Fichiers[0].Raison, etat.Fichiers[0].Position, echeance(etat.Fichiers[0])
			}
			if len(etat.Fichiers) == 0 {
				el.Episodes = []aVoirEpisodeJSON{}
			}
			if len(etat.Fichiers) > 0 && etat.Fichiers[0].Contenu == "" {
				// Serie sans episode planifiable : la cause est portee par l'element.
				el.Cause, el.Episodes = etat.Fichiers[0].Cause, []aVoirEpisodeJSON{}
			}
		case len(etat.Fichiers) > 0:
			f := etat.Fichiers[0]
			el.Octets, el.Total, el.DebitKbps = avancement(f.FichierPrepare)
			el.Cause, el.Raison, el.Position, el.Reessai = f.Cause, f.Raison, f.Position, echeance(f)
		}
		out = append(out, el)
	}
	budget, avanceDefaut := budgetJSON{}, 0
	if a.opt.Preparation != nil {
		budget = budgetJSON{Utilise: a.opt.Preparation.Occupe(), Total: a.opt.Preparation.Budget()}
		avanceDefaut = a.opt.Preparation.EpisodesAvanceDefaut()
	}
	ecrireJSON(w, http.StatusOK, map[string]any{"elements": out, "budget": budget, "episodes_avance_defaut": avanceDefaut})
}

// reessayerAVoir : POST /profil/a_voir/{id}/reessayer remet en attente, tout de suite, les
// fichiers en echec de l'element (serie : ses episodes planifies) ; 204 meme s'il n'y a
// rien a faire ; 404 si l'element n'est pas dans la liste.
func (a *API) reessayerAVoir(w http.ResponseWriter, req *http.Request) {
	p, ok := a.listeActive(w, req)
	if !ok {
		return
	}
	id, _, _, ok := aVoirIDDuChemin(w, req)
	if !ok {
		return
	}
	liste, err := a.opt.AVoir.AVoir(req.Context(), p)
	if err != nil {
		a.erreurProfil(w, err)
		return
	}
	present := false
	for _, e := range liste {
		present = present || e.ID == id
	}
	if !present {
		erreur(w, http.StatusNotFound, "introuvable", "element absent de la liste")
		return
	}
	if a.opt.Preparation != nil {
		if err := a.opt.Preparation.Reessayer(req.Context(), p, id); err != nil {
			a.erreurAmont(w, err, "preparation")
			return
		}
		a.log.Info("nouvelle tentative de preparation demandee", "element", string(id))
	}
	w.WriteHeader(http.StatusNoContent)
}

// ajouterAVoir ajoute en fin de liste (ou met a jour les options). Le contenu doit exister
// dans le catalogue (film ou serie) : 404 sinon. Idempotent.
func (a *API) ajouterAVoir(w http.ResponseWriter, req *http.Request) {
	p, ok := a.listeActive(w, req)
	if !ok {
		return
	}
	id, contenu, serie, ok := aVoirIDDuChemin(w, req)
	if !ok {
		return
	}
	var in struct {
		EpisodesAvance int         `json:"episodes_avance"`
		Depuis         *depuisJSON `json:"depuis"`
	}
	if req.ContentLength != 0 && req.Body != nil && req.Header.Get("Content-Type") != "" {
		if !lireJSON(w, req, &in) {
			return
		}
	}
	if in.EpisodesAvance < 0 || in.EpisodesAvance > 20 || (in.Depuis != nil && (in.Depuis.Saison < 1 || in.Depuis.Episode < 0)) {
		requeteInvalide(w, "episodes_avance (0 a 20) ou depuis (saison >= 1, episode >= 0) invalides")
		return
	}
	if !serie && (in.EpisodesAvance != 0 || in.Depuis != nil) {
		requeteInvalide(w, "options reservees a une serie")
		return
	}
	if a.opt.Video != nil {
		var err error
		if serie {
			_, err = a.opt.Video.Serie(req.Context(), contenu)
		} else {
			_, err = a.opt.Video.Film(req.Context(), contenu)
		}
		if errors.Is(err, domain.ErrNonTrouve) {
			erreur(w, http.StatusNotFound, "introuvable", "contenu inconnu")
			return
		}
		if err != nil {
			a.erreurAmont(w, err, "contenu")
			return
		}
	}
	e := domain.ElementAVoir{ID: id, Contenu: contenu, Serie: serie, EpisodesAvance: in.EpisodesAvance}
	if in.Depuis != nil {
		e.DepuisSaison, e.DepuisEpisode = in.Depuis.Saison, in.Depuis.Episode
	}
	if err := a.opt.AVoir.AjouterAVoir(req.Context(), p, e); err != nil {
		a.erreurProfil(w, err)
		return
	}
	a.reveillerPreparation()
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) retirerAVoir(w http.ResponseWriter, req *http.Request) {
	p, ok := a.listeActive(w, req)
	if !ok {
		return
	}
	id, _, _, ok := aVoirIDDuChemin(w, req)
	if !ok {
		return
	}
	if err := a.opt.AVoir.RetirerAVoir(req.Context(), p, id); err != nil {
		a.erreurProfil(w, err)
		return
	}
	a.reveillerPreparation()
	w.WriteHeader(http.StatusNoContent)
}

// ordonnerAVoir : `{"ids": [...]}`, l'ordre voulu ; les absents suivent dans leur ordre.
func (a *API) ordonnerAVoir(w http.ResponseWriter, req *http.Request) {
	p, ok := a.listeActive(w, req)
	if !ok {
		return
	}
	var in struct {
		IDs []domain.AVoirID `json:"ids"`
	}
	if !lireJSON(w, req, &in) {
		return
	}
	if len(in.IDs) > 1000 {
		requeteInvalide(w, "trop d'identifiants")
		return
	}
	for _, id := range in.IDs {
		if !reAVoirID.MatchString(string(id)) {
			requeteInvalide(w, "identifiant « a voir » invalide")
			return
		}
	}
	if err := a.opt.AVoir.OrdonnerAVoir(req.Context(), p, in.IDs); err != nil {
		a.erreurProfil(w, err)
		return
	}
	a.reveillerPreparation()
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) reveillerPreparation() {
	if a.opt.Preparation != nil {
		a.opt.Preparation.Reveiller()
	}
}

// disponibilite : GET /vod/{id}/disponibilite?serie=<serie> (serie : pour un episode, afin
// de connaitre le debit de son fichier).
func (a *API) disponibilite(w http.ResponseWriter, req *http.Request) {
	if a.opt.Sonde == nil {
		erreur(w, http.StatusNotImplemented, "non_disponible", "sonde de disponibilite non activee")
		return
	}
	id, ok := idDuChemin(w, req)
	if !ok {
		return
	}
	var serie domain.ContenuID
	if s := strings.TrimSpace(req.URL.Query().Get("serie")); s != "" {
		if !reContenuID.MatchString(s) || !a.serieValide(req, id, domain.ContenuID(s)) {
			requeteInvalide(w, "serie invalide")
			return
		}
		serie = domain.ContenuID(s)
	}
	d, err := a.opt.Sonde.Disponibilite(req.Context(), session(req).ID, id, serie)
	if errors.Is(err, domain.ErrNonTrouve) {
		erreur(w, http.StatusNotFound, "introuvable", "contenu inconnu")
		return
	}
	if err != nil {
		a.erreurAmont(w, err, "contenu")
		return
	}
	w.Header().Set("Cache-Control", "private, no-store")
	var mesure *time.Time
	if d.DebitKbps > 0 || d.Etat == "pret" {
		m := d.Mesure.UTC()
		mesure = &m
	}
	ecrireJSON(w, http.StatusOK, map[string]any{"etat": d.Etat, "debit_kbps": d.DebitKbps, "requis_kbps": d.DebitFichierKbps, "mesure": mesure})
}

// preparationAdmin : GET /admin/preparation, les fichiers et leur etat.
func (a *API) preparationAdmin(w http.ResponseWriter, req *http.Request) {
	if a.opt.Preparation == nil {
		erreur(w, http.StatusNotImplemented, "non_disponible", "preparation non activee")
		return
	}
	fs := a.opt.Preparation.Fichiers()
	out := make([]fichierPrepareJSON, 0, len(fs))
	for _, f := range fs {
		out = append(out, versFichierPrepare(f))
	}
	ecrireJSON(w, http.StatusOK, map[string]any{"fichiers": out, "occupe": a.opt.Preparation.Occupe()})
}

// viderPreparation : DELETE /admin/preparation, efface les fichiers (sauf en lecture).
func (a *API) viderPreparation(w http.ResponseWriter, req *http.Request) {
	if a.opt.Preparation == nil {
		erreur(w, http.StatusNotImplemented, "non_disponible", "preparation non activee")
		return
	}
	n := a.opt.Preparation.Vider()
	a.log.Info("preparation videe par un administrateur", "fichiers", n)
	ecrireJSON(w, http.StatusOK, map[string]any{"effaces": n})
}
