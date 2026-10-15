package domain

// Contrats du lot 4 (US-28) : details des films et des series (synopsis, genre,
// casting...). Ils ne sont PAS dans les listes du fournisseur : ils viennent des
// appels paresseux get_vod_info / get_series_info.

import (
	"context"
	"time"
)

// Details est le tronc commun, tous champs facultatifs (chaine vide = inconnu).
type Details struct {
	Synopsis     string
	Genre        string
	Casting      string
	Realisateur  string
	Pays         string
	Sortie       string   // date de sortie telle qu'ecrite par le fournisseur (« 2019-05-03 »)
	BandeAnnonce string   // adresse https de la bande-annonce (YouTube), vide si inconnue
	ImageFond    []string // arriere-plans (adresses http/https), le meilleur en premier
	Affiche      string   // grande affiche (movie_image / cover_big)
	Note         float64  // 0 = inconnue
	Age          string   // classification (« 12 », « PG-13 »)
}

// Vide : le fournisseur n'a rien dit d'exploitable.
func (d Details) Vide() bool {
	return d.Synopsis == "" && d.Genre == "" && d.Casting == "" && d.Realisateur == "" && d.Pays == "" &&
		d.Sortie == "" && d.BandeAnnonce == "" && len(d.ImageFond) == 0 && d.Affiche == "" && d.Note == 0 && d.Age == ""
}

// Fusionner complete d par `nouveau` : un champ vide de `nouveau` ne reprend JAMAIS
// la place d'une valeur connue (un fournisseur qui repond une fiche incomplete ne
// doit pas appauvrir ce qu'on sait deja).
func (d Details) Fusionner(nouveau Details) Details {
	choisir := func(ancien, neuf string) string {
		if neuf != "" {
			return neuf
		}
		return ancien
	}
	r := Details{
		Synopsis: choisir(d.Synopsis, nouveau.Synopsis), Genre: choisir(d.Genre, nouveau.Genre),
		Casting: choisir(d.Casting, nouveau.Casting), Realisateur: choisir(d.Realisateur, nouveau.Realisateur),
		Pays: choisir(d.Pays, nouveau.Pays), Sortie: choisir(d.Sortie, nouveau.Sortie),
		BandeAnnonce: choisir(d.BandeAnnonce, nouveau.BandeAnnonce), Affiche: choisir(d.Affiche, nouveau.Affiche),
		Age: choisir(d.Age, nouveau.Age), ImageFond: d.ImageFond, Note: d.Note,
	}
	if len(nouveau.ImageFond) > 0 {
		r.ImageFond = nouveau.ImageFond
	}
	if nouveau.Note > 0 {
		r.Note = nouveau.Note
	}
	return r
}

// DetailsFilm : get_vod_info.
type DetailsFilm struct {
	Details
	Duree time.Duration // 0 = inconnue
}

func (d DetailsFilm) Vide() bool { return d.Details.Vide() && d.Duree == 0 }

func (d DetailsFilm) Fusionner(n DetailsFilm) DetailsFilm {
	r := DetailsFilm{Details: d.Details.Fusionner(n.Details), Duree: d.Duree}
	if n.Duree > 0 {
		r.Duree = n.Duree
	}
	return r
}

// DetailsSerie : bloc `info` de get_series_info.
type DetailsSerie struct {
	Details
	DureeEpisode time.Duration // duree moyenne d'un episode ; 0 = inconnue
}

func (d DetailsSerie) Vide() bool { return d.Details.Vide() && d.DureeEpisode == 0 }

func (d DetailsSerie) Fusionner(n DetailsSerie) DetailsSerie {
	r := DetailsSerie{Details: d.Details.Fusionner(n.Details), DureeEpisode: d.DureeEpisode}
	if n.DureeEpisode > 0 {
		r.DureeEpisode = n.DureeEpisode
	}
	return r
}

// FournisseurDetails est implemente par les Fournisseur qui savent donner les
// details d'un film ou d'une serie. Chaque appel est un appel d'API (aucun flux,
// aucun bail) mais il pese sur le fournisseur : l'appelant le borne (voir
// internal/metadonnees). Un fournisseur sans information rend des details vides
// (Vide()) et une erreur nil ; ErrIntrouvable pour un 404.
type FournisseurDetails interface {
	DetailsFilm(ctx context.Context, numero string) (*DetailsFilm, error)
	// SaisonsEtDetails fait UN SEUL appel get_series_info et en tire les saisons
	// (avec les details de chaque episode) et les details de la serie.
	SaisonsEtDetails(ctx context.Context, numeroSerie string) ([]Saison, *DetailsSerie, error)
}

// StockageDetails persiste les details (table distincte du catalogue : une
// resynchronisation du catalogue ne les efface pas). Les lectures renvoient
// ErrNonTrouve quand rien n'est stocke.
type StockageDetails interface {
	DetailsFilm(ctx context.Context, id ContenuID) (*DetailsFilm, time.Time, error)
	DetailsSerie(ctx context.Context, id ContenuID) (*DetailsSerie, time.Time, error)
	// EcrireDetailsFilm fusionne `d` avec ce qui est deja stocke (jamais d'appauvrissement)
	// et date la recuperation. Des details vides ne changent rien (la date reste).
	EcrireDetailsFilm(ctx context.Context, id ContenuID, d DetailsFilm, a time.Time) error
	EcrireDetailsSerie(ctx context.Context, id ContenuID, d DetailsSerie, a time.Time) error
}
