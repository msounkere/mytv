package api

// Categories mytv envoyees aux box IPTV (lot 11, etape A2) : « Ma liste », « En cours » et
// « Mes chaines », en tete des categories de la box. Elles viennent d'UN profil mytv, choisi par
// l'administrateur (Reglages > Box IPTV) : le compte d'appareil de la box a son propre profil,
// vide, alors que les listes vivent sur le profil de la personne. Sans choix : le profil de la
// box.
//
// Xtream ne connait qu'une categorie par element : un contenu de ces categories est envoye une
// seconde fois, avec l'identifiant de la categorie mytv (meme stream_id, la lecture est la meme).

import (
	"context"
	"strings"
	"time"

	"mytv/internal/domain"
)

const cleXtreamProfil = "profil_listes"

// Identifiants des categories mytv : numeriques (des box les lisent en nombre), loin de ceux du
// fournisseur (1 700 au plus le 23/11).
const (
	catMesChaines    = "990001"
	catMaListeFilms  = "990002"
	catEnCoursFilms  = "990003"
	catMaListeSeries = "990004"
	catEnCoursSeries = "990005"
)

// xtreamEnCoursMax : elements au plus dans une categorie « En cours ».
const xtreamEnCoursMax = 40

func estCategorieMytv(c string) bool { return strings.HasPrefix(c, "99000") && len(c) == 6 }

type groupeMytv struct {
	id, nom  string
	contenus []any // domain.Chaine, domain.Film ou domain.SerieTV, dans l'ordre d'affichage
}

// profilListesXtream : le profil dont la box montre les listes.
func (a *API) profilListesXtream(ctx context.Context, sx sessionXtream) string {
	if a.opt.Etat != nil {
		if r, err := a.opt.Etat.Reglages(ctx, profilXtream); err == nil && r.Valeurs[cleXtreamProfil] != "" {
			return r.Valeurs[cleXtreamProfil]
		}
	}
	return sx.profil
}

// groupesMytv rend les categories mytv NON VIDES du type demande. Une erreur de lecture laisse
// simplement la categorie de cote : la box garde le catalogue.
func (a *API) groupesMytv(ctx context.Context, sx sessionXtream, t domain.TypeExport) []groupeMytv {
	if a.opt.Etat == nil {
		return nil
	}
	profil := a.profilListesXtream(ctx, sx)
	f := a.fournisseurXtream()
	var out []groupeMytv
	ajouter := func(g groupeMytv) {
		if len(g.contenus) > 0 {
			out = append(out, g)
		}
	}
	switch t {
	case domain.ExportLive:
		g := groupeMytv{id: catMesChaines, nom: "★ Mes chaînes"}
		ids, _ := a.opt.Etat.Favoris(ctx, profil)
		for _, id := range ids {
			if c, err := a.opt.Stockage.Chaine(ctx, id); err == nil && c.FournisseurID == f {
				g.contenus = append(g.contenus, *c)
			}
		}
		ajouter(g)
	case domain.ExportVOD:
		if a.opt.Video == nil {
			return nil
		}
		liste := groupeMytv{id: catMaListeFilms, nom: "★ Ma liste"}
		for _, e := range a.aVoirXtream(ctx, profil) {
			if e.Serie {
				continue
			}
			if c, err := a.opt.Video.Film(ctx, e.Contenu); err == nil && c.FournisseurID == f {
				liste.contenus = append(liste.contenus, *c)
			}
		}
		ajouter(liste)
		enCours := groupeMytv{id: catEnCoursFilms, nom: "▶ En cours"}
		for _, l := range a.historiqueXtream(ctx, profil) {
			if l.Serie != "" || !entame(l) || len(enCours.contenus) >= xtreamEnCoursMax {
				continue
			}
			if c, err := a.opt.Video.Film(ctx, l.Contenu); err == nil && c.FournisseurID == f {
				enCours.contenus = append(enCours.contenus, *c)
			}
		}
		ajouter(enCours)
	case domain.ExportSeries:
		if a.opt.Video == nil {
			return nil
		}
		liste := groupeMytv{id: catMaListeSeries, nom: "★ Ma liste"}
		for _, e := range a.aVoirXtream(ctx, profil) {
			if !e.Serie {
				continue
			}
			if c, err := a.opt.Video.Serie(ctx, e.Contenu); err == nil && c.FournisseurID == f {
				liste.contenus = append(liste.contenus, *c)
			}
		}
		ajouter(liste)
		// Une serie est « en cours » des qu'un de ses episodes a ete lu : la plus recente d'abord.
		enCours := groupeMytv{id: catEnCoursSeries, nom: "▶ En cours"}
		vues := map[domain.ContenuID]bool{}
		for _, l := range a.historiqueXtream(ctx, profil) {
			if l.Serie == "" || vues[l.Serie] || len(enCours.contenus) >= xtreamEnCoursMax {
				continue
			}
			vues[l.Serie] = true
			if c, err := a.opt.Video.Serie(ctx, l.Serie); err == nil && c.FournisseurID == f {
				enCours.contenus = append(enCours.contenus, *c)
			}
		}
		ajouter(enCours)
	}
	return out
}

func (a *API) aVoirXtream(ctx context.Context, profil string) []domain.ElementAVoir {
	if a.opt.AVoir == nil {
		return nil
	}
	es, _ := a.opt.AVoir.AVoir(ctx, profil)
	return es
}

func (a *API) historiqueXtream(ctx context.Context, profil string) []domain.Lecture {
	ls, _ := a.opt.Etat.Historique(ctx, profil, 0)
	return ls
}

// entame : un film commence (plus de 2 min) et pas fini (moins de 92 % de sa duree).
func entame(l domain.Lecture) bool {
	return l.Duree > 0 && l.Position >= 2*time.Minute && l.Position < l.Duree*92/100
}
