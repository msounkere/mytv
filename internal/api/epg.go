package api

import (
	"net/http"
	"strings"
	"time"

	"mytv/internal/domain"
)

type programmeJSON struct {
	Debut       time.Time `json:"debut"`
	Fin         time.Time `json:"fin"`
	Titre       string    `json:"titre"`
	Description string    `json:"description,omitempty"`
	Categorie   string    `json:"categorie,omitempty"`
	SousTitre   string    `json:"sous_titre,omitempty"`
	Episode     string    `json:"episode,omitempty"`
	Annee       string    `json:"annee,omitempty"`
	// Replay : identifiant de lecture (GET /vod/{id}) quand le programme peut etre revu chez le
	// fournisseur (chaine avec replay_jours, commence, dans la fenetre). Absent sinon. Le
	// fournisseur peut quand meme ne rien servir : le HEAD le dit (404 replay_indisponible).
	Replay string `json:"replay,omitempty"`
}

func versProgramme(p *domain.Programme) *programmeJSON {
	if p == nil {
		return nil
	}
	return &programmeJSON{Debut: p.Debut.UTC(), Fin: p.Fin.UTC(), Titre: p.Titre, Description: p.Description, Categorie: p.Categorie, SousTitre: p.SousTitre, Episode: p.Episode, Annee: p.Annee}
}

func (a *API) declarerEPG() {
	a.route("GET", "/epg/chaines/{id}/maintenant", Session, a.epgMaintenant)
	a.route("GET", "/epg/grille", Session, a.epgGrille)
}

func (a *API) epgMaintenant(w http.ResponseWriter, req *http.Request) {
	if a.opt.EPG == nil {
		erreur(w, http.StatusNotImplemented, "non_disponible", "guide des programmes non active")
		return
	}
	id, ok := idDuChemin(w, req)
	if !ok {
		return
	}
	if _, err := a.opt.Stockage.Chaine(req.Context(), id); err != nil {
		a.erreurAmont(w, err, "chaine")
		return
	}
	c, s, err := a.opt.EPG.EnCoursEtSuivant(req.Context(), id, a.maintenant())
	if err != nil {
		a.erreurAmont(w, err, "programme")
		return
	}
	ecrireJSON(w, http.StatusOK, map[string]*programmeJSON{"courant": versProgramme(c), "suivant": versProgramme(s)})
}

func (a *API) epgGrille(w http.ResponseWriter, req *http.Request) {
	if a.opt.EPG == nil {
		erreur(w, http.StatusNotImplemented, "non_disponible", "guide des programmes non active")
		return
	}
	q := req.URL.Query()
	de, err1 := time.Parse(time.RFC3339, q.Get("de"))
	fin, err2 := time.Parse(time.RFC3339, q.Get("a"))
	if err1 != nil || err2 != nil || !fin.After(de) || fin.Sub(de) > a.opt.Config.GrilleFenetreMax.Std() {
		requeteInvalide(w, "plage invalide : de et a en RFC 3339, a apres de, dans la fenetre maximale")
		return
	}
	brut := q.Get("chaines")
	if brut == "" || len(brut) > 4096 {
		requeteInvalide(w, "chaines requises")
		return
	}
	var ids []domain.ContenuID
	vus := map[string]bool{}
	for _, c := range strings.Split(brut, ",") {
		if !reContenuID.MatchString(c) {
			requeteInvalide(w, "identifiant de chaine invalide")
			return
		}
		if !vus[c] {
			vus[c] = true
			ids = append(ids, domain.ContenuID(c))
		}
	}
	if len(ids) > a.opt.Config.PaginationMax {
		requeteInvalide(w, "trop de chaines demandees")
		return
	}
	g, err := a.opt.EPG.Grille(req.Context(), ids, de.UTC(), fin.UTC())
	if err != nil {
		a.erreurAmont(w, err, "grille")
		return
	}
	maintenant := a.maintenant()
	out := make(map[string][]programmeJSON, len(ids))
	for _, id := range ids {
		l := make([]programmeJSON, 0, len(g[id]))
		jours, numero, fournisseur := a.replayDe(req, id, g[id], maintenant)
		for i := range g[id] {
			p := versProgramme(&g[id][i])
			if domain.ReplayPossible(jours, p.Debut, maintenant) {
				minutes := int((p.Fin.Sub(p.Debut) + time.Minute - 1) / time.Minute)
				if minutes >= 1 && minutes <= 480 {
					p.Replay = string(domain.IDReplay(fournisseur, numero, p.Debut, minutes))
				}
			}
			l = append(l, *p)
		}
		out[string(id)] = l
	}
	ecrireJSONCompresse(w, req, map[string]any{"chaines": out})
}

// replayDe : jours de replay de la chaine (0 si aucun ou inconnue), son numero et son
// fournisseur. Le catalogue n'est lu que si un programme de la plage a deja commence.
func (a *API) replayDe(req *http.Request, id domain.ContenuID, ps []domain.Programme, maintenant time.Time) (int, string, string) {
	if len(ps) == 0 || ps[0].Debut.After(maintenant) {
		return 0, "", ""
	}
	c, err := a.opt.Stockage.Chaine(req.Context(), id)
	if err != nil || c.ReplayJours <= 0 {
		return 0, "", ""
	}
	return c.ReplayJours, c.Numero, c.FournisseurID
}
