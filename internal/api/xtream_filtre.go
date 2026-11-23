package api

// Filtre des langues et pays envoyes aux box IPTV (lot 11) : 22 000 chaines, 108 000 films et
// 25 000 series en toutes langues surchargeaient MYTVOnline. L'administrateur choisit, dans
// Reglages > Box IPTV, les PREFIXES de noms a garder (« FR - TF1 », « |EU| … » n'est qu'une
// region : c'est le prefixe du NOM du contenu qui compte, comme pour l'etiquette de pays de
// l'interface). Rien de change pour l'interface web.
//
//   - un contenu dont le nom porte un prefixe est garde si ce prefixe est choisi ;
//   - un contenu sans prefixe suit sa categorie : gardee si elle contient au moins un contenu
//     garde par son prefixe ;
//   - une categorie n'est envoyee que si elle contient au moins un contenu garde.
// Aucun prefixe choisi = tout est envoye (comportement d'origine).

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"mytv/internal/domain"
)

// profilXtream : identifiant reserve sous lequel les reglages des box sont ranges (table des
// reglages de profil ; aucun profil reel ne commence par « _ »).
const profilXtream = "_xtream"

const cleXtreamPrefixes = "prefixes"

// Meme regle que l'etiquette de pays de l'interface (mytv-ui, libelles.ts).
var rePrefixeNom = regexp.MustCompile(`^\s*(?:\|([A-Z]{2,4})\||([A-Z]{2,4})\s*[-:])`)

func prefixeNom(nom string) string {
	m := rePrefixeNom.FindStringSubmatch(nom)
	if m == nil {
		return ""
	}
	if m[1] != "" {
		return m[1]
	}
	return m[2]
}

// filtreXtream : nil = rien a filtrer.
type filtreXtream map[string]bool

func (a *API) filtreXtream(ctx context.Context) filtreXtream {
	if a.opt.Etat == nil {
		return nil
	}
	r, err := a.opt.Etat.Reglages(ctx, profilXtream)
	if err != nil {
		return nil
	}
	f := filtreXtream{}
	for _, p := range strings.Split(r.Valeurs[cleXtreamPrefixes], ",") {
		if p = strings.TrimSpace(p); p != "" {
			f[p] = true
		}
	}
	if len(f) == 0 {
		return nil
	}
	return f
}

// nomEtCategorie : le nom et la categorie d'un element d'export.
func nomEtCategorie(v any) (nom, categorie, fournisseur string, ok bool) {
	switch c := v.(type) {
	case domain.Chaine:
		return c.Nom, c.CategorieID, c.FournisseurID, true
	case domain.Film:
		return c.Nom, c.CategorieID, c.FournisseurID, true
	case domain.SerieTV:
		return c.Nom, c.CategorieID, c.FournisseurID, true
	}
	return "", "", "", false
}

// garde : le contenu passe-t-il le filtre ? (categories retenues deja calculees)
func (f filtreXtream) garde(nom, categorie string, retenues map[string]bool) bool {
	if f == nil {
		return true
	}
	if p := prefixeNom(nom); p != "" {
		return f[p]
	}
	return retenues[categorie]
}

type cacheCategories struct {
	cle      string
	calcule  time.Time
	retenues map[string]bool
}

// categoriesRetenues : les categories qui contiennent au moins un contenu garde par son prefixe.
// Un parcours du catalogue (108 000 films : ~0,5 s), garde 10 minutes par filtre.
func (a *API) categoriesRetenues(ctx context.Context, t domain.TypeExport, f filtreXtream) (map[string]bool, error) {
	cles := make([]string, 0, len(f))
	for p := range f {
		cles = append(cles, p)
	}
	sort.Strings(cles)
	cle := strings.Join(cles, ",")
	a.xtreamCache.mu.Lock()
	c, ok := a.xtreamCache.m[t]
	a.xtreamCache.mu.Unlock()
	if ok && c.cle == cle && a.maintenant().Sub(c.calcule) < 10*time.Minute {
		return c.retenues, nil
	}
	retenues := map[string]bool{}
	live := ""
	if t == domain.ExportLive {
		live = a.opt.FournisseurLive
	}
	ex, err := a.opt.Export.OuvrirExport(ctx, t, live)
	if err != nil {
		return nil, err
	}
	defer ex.Fermer()
	fx := a.fournisseurXtream()
	err = ex.Lire(func(v any) error {
		nom, cat, fo, ok := nomEtCategorie(v)
		if ok && fo == fx {
			if p := prefixeNom(nom); p != "" && f[p] {
				retenues[cat] = true
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	a.xtreamCache.mu.Lock()
	if a.xtreamCache.m == nil {
		a.xtreamCache.m = map[domain.TypeExport]cacheCategories{}
	}
	a.xtreamCache.m[t] = cacheCategories{cle: cle, calcule: a.maintenant(), retenues: retenues}
	a.xtreamCache.mu.Unlock()
	return retenues, nil
}

func typeExportDe(t domain.Type) domain.TypeExport {
	switch t {
	case domain.TypeLive:
		return domain.ExportLive
	case domain.TypeFilm:
		return domain.ExportVOD
	}
	return domain.ExportSeries
}

// ---------------------------------------------------------------- administration

type prefixeXtreamJSON struct {
	Code    string `json:"code"`
	Chaines int    `json:"chaines"`
	Films   int    `json:"films"`
	Series  int    `json:"series"`
}

func (a *API) declarerXtreamAdmin() {
	a.route("GET", "/admin/xtream", Admin, a.lireXtreamAdmin)
	a.route("PUT", "/admin/xtream", Admin, a.ecrireXtreamAdmin)
}

// lireXtreamAdmin : GET /admin/xtream. Les prefixes choisis et, pour chaque prefixe present au
// catalogue, le nombre de chaines, films et series (parcours complet : ~1 s).
func (a *API) lireXtreamAdmin(w http.ResponseWriter, req *http.Request) {
	if a.opt.Etat == nil || a.opt.Export == nil {
		erreur(w, http.StatusNotImplemented, "non_disponible", "reglages des box non disponibles")
		return
	}
	r, err := a.opt.Etat.Reglages(req.Context(), profilXtream)
	if err != nil {
		a.erreurAmont(w, err, "reglages")
		return
	}
	comptes := map[string]*prefixeXtreamJSON{}
	fx := a.fournisseurXtream()
	for _, t := range []domain.TypeExport{domain.ExportLive, domain.ExportVOD, domain.ExportSeries} {
		live := ""
		if t == domain.ExportLive {
			live = a.opt.FournisseurLive
		}
		ex, err := a.opt.Export.OuvrirExport(req.Context(), t, live)
		if err != nil {
			a.erreurAmont(w, err, "catalogue")
			return
		}
		err = ex.Lire(func(v any) error {
			nom, _, fo, ok := nomEtCategorie(v)
			p := prefixeNom(nom)
			if !ok || fo != fx || p == "" {
				return nil
			}
			c := comptes[p]
			if c == nil {
				c = &prefixeXtreamJSON{Code: p}
				comptes[p] = c
			}
			switch t {
			case domain.ExportLive:
				c.Chaines++
			case domain.ExportVOD:
				c.Films++
			default:
				c.Series++
			}
			return nil
		})
		ex.Fermer()
		if err != nil {
			a.erreurAmont(w, err, "catalogue")
			return
		}
	}
	dispo := make([]prefixeXtreamJSON, 0, len(comptes))
	for _, c := range comptes {
		dispo = append(dispo, *c)
	}
	sort.Slice(dispo, func(i, j int) bool {
		ti, tj := dispo[i].Chaines+dispo[i].Films+dispo[i].Series, dispo[j].Chaines+dispo[j].Films+dispo[j].Series
		if ti != tj {
			return ti > tj
		}
		return dispo[i].Code < dispo[j].Code
	})
	choisis := []string{}
	for p := range a.filtreXtream(req.Context()) {
		choisis = append(choisis, p)
	}
	sort.Strings(choisis)
	// Profils proposes pour les listes de la box : ceux du compte de l'administrateur.
	profils := []map[string]string{}
	if a.opt.Comptes != nil {
		if ps, err := a.opt.Comptes.Profils(req.Context(), session(req).UtilisateurID); err == nil {
			for _, p := range ps {
				profils = append(profils, map[string]string{"id": p.ID, "nom": p.Nom})
			}
		}
	}
	ecrireJSON(w, http.StatusOK, map[string]any{"prefixes": choisis, "disponibles": dispo, "revision": r.Revision,
		"profil_listes": r.Valeurs[cleXtreamProfil], "profils": profils})
}

var reCodePrefixe = regexp.MustCompile(`^[A-Z]{2,4}$`)

// ecrireXtreamAdmin : PUT /admin/xtream {prefixes, revision}. Liste vide = tout envoyer.
func (a *API) ecrireXtreamAdmin(w http.ResponseWriter, req *http.Request) {
	if a.opt.Etat == nil {
		erreur(w, http.StatusNotImplemented, "non_disponible", "reglages des box non disponibles")
		return
	}
	var in struct {
		Prefixes []string `json:"prefixes"`
		// ProfilListes : absent = inchange, "" = profil propre de la box.
		ProfilListes *string `json:"profil_listes"`
		Revision     int64   `json:"revision"`
	}
	if !lireJSON(w, req, &in) {
		return
	}
	if len(in.Prefixes) > 200 {
		requeteInvalide(w, "trop de prefixes")
		return
	}
	vus := map[string]bool{}
	propres := []string{}
	for _, p := range in.Prefixes {
		if !reCodePrefixe.MatchString(p) {
			requeteInvalide(w, "prefixe invalide : 2 a 4 lettres majuscules")
			return
		}
		if !vus[p] {
			vus[p] = true
			propres = append(propres, p)
		}
	}
	sort.Strings(propres)
	courant, err := a.opt.Etat.Reglages(req.Context(), profilXtream)
	if err != nil {
		a.erreurAmont(w, err, "reglages")
		return
	}
	valeurs := map[string]string{cleXtreamPrefixes: strings.Join(propres, ",")}
	profil := courant.Valeurs[cleXtreamProfil]
	if in.ProfilListes != nil {
		profil = *in.ProfilListes
		if profil != "" && !a.profilDuCompte(req, profil) {
			requeteInvalide(w, "profil inconnu : choisir un profil de votre compte")
			return
		}
	}
	if profil != "" {
		valeurs[cleXtreamProfil] = profil
	}
	r, err := a.opt.Etat.EcrireReglages(req.Context(), profilXtream, valeurs, in.Revision)
	if errors.Is(err, domain.ErrConflit) {
		erreur(w, http.StatusConflict, "conflit", "reglages modifies entre-temps : relire puis recommencer")
		return
	}
	if err != nil {
		a.erreurAmont(w, err, "reglages")
		return
	}
	a.log.Info("xtream : reglages des box", "prefixes", strings.Join(propres, ","), "profil_listes", abreger(profil), "revision", strconv.FormatInt(r.Revision, 10))
	ecrireJSON(w, http.StatusOK, map[string]any{"prefixes": propres, "profil_listes": profil, "revision": r.Revision})
}

// profilDuCompte : le profil appartient-il au compte de la session (administrateur) ?
func (a *API) profilDuCompte(req *http.Request, id string) bool {
	if a.opt.Comptes == nil {
		return false
	}
	ps, err := a.opt.Comptes.Profils(req.Context(), session(req).UtilisateurID)
	if err != nil {
		return false
	}
	for _, p := range ps {
		if p.ID == id {
			return true
		}
	}
	return false
}
