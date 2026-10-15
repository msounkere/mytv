package domain

// Contrats du lot 3 : export en bloc du catalogue pour l'interface.

import (
	"context"
	"fmt"
	"time"
)

// Manifeste : effectifs du catalogue exporte et date de la derniere synchronisation
// reussie (zero si jamais). Le direct etant mono-fournisseur, Chaines ne compte que
// celles du fournisseur du direct quand il est precise.
type Manifeste struct {
	Categories, Chaines, Films, Series int
	Synchronise                        time.Time
}

// TypeExport : ce que l'on exporte. « categories » rend les categories des TROIS types.
type TypeExport string

const (
	ExportCategories TypeExport = "categories"
	ExportLive       TypeExport = "live"
	ExportVOD        TypeExport = "vod"
	ExportSeries     TypeExport = "series"
)

// Export est un parcours coherent d'un type de contenu : tout est lu dans UNE
// transaction de lecture (une resynchronisation concurrente ne s'y melange pas).
// Le nombre d'elements est connu avant le premier (en-tete X-Elements).
type Export interface {
	// Elements est le nombre de lignes que Lire emettra.
	Elements() int
	// Lire appelle emettre pour chaque ligne, dans l'ordre d'export : une Categorie,
	// une Chaine, un Film ou une SerieTV selon le type. Une erreur d'emettre arrete
	// le parcours et est renvoyee. Respecte l'annulation du contexte d'ouverture.
	Lire(emettre func(any) error) error
	// Fermer libere la transaction ; a appeler toujours, apres Lire.
	Fermer() error
}

// ExportCatalogue est implemente par le stockage SQLite.
type ExportCatalogue interface {
	// Manifeste donne les effectifs ; fournisseurLive restreint les chaines (vide = tous).
	Manifeste(ctx context.Context, fournisseurLive string) (Manifeste, error)
	// OuvrirExport ouvre le parcours d'un type, trie par fournisseur, categorie puis
	// ordre du fournisseur.
	OuvrirExport(ctx context.Context, t TypeExport, fournisseurLive string) (Export, error)
}

// ErrPlageHorsFichier : le fournisseur a repondu 416 a une plage d'octets (hors du
// fichier). L'API repond 416 au client, avec `Content-Range: bytes */<Total>` quand la
// taille est connue. Se teste avec errors.As.
type ErrPlageHorsFichier struct {
	Total int64 // taille du fichier annoncee par le fournisseur ; -1 si inconnue
}

func (e ErrPlageHorsFichier) Error() string {
	if e.Total >= 0 {
		return fmt.Sprintf("plage hors du fichier (%d octets)", e.Total)
	}
	return "plage hors du fichier"
}
