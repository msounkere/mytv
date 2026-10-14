// Package domain porte les TYPES et les CONTRATS partages entre modules.
//
// Regle : ce fichier est le contrat entre les codeurs. Seul le coordinateur le
// modifie (apres accord des deux cotes). Un module depend de ces interfaces, jamais
// de l'implementation d'un autre module : c'est ce qui permet de travailler en
// parallele et de tester avec des doublures.
package domain

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// ContenuID est l'identifiant stable d'un contenu : « <fournisseur>:<numero chez le fournisseur> »,
// par exemple « iptva:845410 ».
type ContenuID string

type Type string

const (
	TypeLive  Type = "live"
	TypeFilm  Type = "vod"
	TypeSerie Type = "series"
)

type Categorie struct {
	ID            string // identifiant chez le fournisseur
	FournisseurID string
	Type          Type
	Nom           string
	Ordre         int
}

type Chaine struct {
	ID            ContenuID
	FournisseurID string
	Numero        string // identifiant chez le fournisseur (« 845410 »)
	Nom           string
	Logo          string
	EpgID         string // epg_channel_id du fournisseur
	CategorieID   string
	NumeroChaine  int
	Ordre         int
}

type Film struct {
	ID            ContenuID
	FournisseurID string
	Numero        string
	Nom           string
	Image         string
	CategorieID   string
	Extension     string // conteneur annonce (mkv, mp4...)
	Note          float64
	Annee         string
	Ajoute        time.Time
}

type SerieTV struct {
	ID            ContenuID
	FournisseurID string
	Numero        string
	Nom           string
	Image         string
	CategorieID   string
	Note          float64
	Annee         string
}

// Instantane est le catalogue complet d'UN fournisseur a un instant donne.
type Instantane struct {
	FournisseurID string
	Pris          time.Time
	Categories    []Categorie
	Chaines       []Chaine
	Films         []Film
	Series        []SerieTV
}

// Erreurs typees renvoyees par un Fournisseur : l'appelant decide quoi en faire.
var (
	ErrRefuseConnexions = errors.New("fournisseur : connexion refusee (407, place occupee)")
	ErrRefuseIP         = errors.New("fournisseur : adresse refusee (458, datacenter)")
	ErrIntrouvable      = errors.New("fournisseur : contenu introuvable (404)")
	ErrAmont            = errors.New("fournisseur : erreur amont (5xx, delai ou reseau)")
	ErrIdentifiants     = errors.New("fournisseur : identifiants refuses (401, 403)")
)

// Fournisseur est un client d'un fournisseur de contenu. Il est le SEUL a connaitre
// les identifiants du fournisseur et le SEUL a lui parler.
type Fournisseur interface {
	ID() string
	// Catalogue telecharge tout le catalogue (peut prendre plusieurs secondes).
	Catalogue(ctx context.Context) (*Instantane, error)
	// URLLive donne l'adresse de la playlist HLS d'une chaine (numero chez le fournisseur).
	URLLive(numero string) string
	// Requete execute une requete amont (playlist, segment, film). Le corps de la
	// reponse doit etre ferme par l'appelant. Les statuts d'erreur sont traduits
	// en erreurs typees ci-dessus (et la reponse est alors nil).
	Requete(ctx context.Context, url string, entetes http.Header) (*http.Response, error)
}

// Baux reserve la connexion limitee d'un fournisseur. Un client (session) n'a
// qu'UN bail par fournisseur : en zappant vers une autre ressource, il REUTILISE
// son bail au lieu d'en demander un second (c'est ce qui evite les refus 407).
type Baux interface {
	// Acquerir attend une place au plus la patience configuree, puis renvoie le bail.
	Acquerir(ctx context.Context, fournisseurID, session, ressource string) (Bail, error)
}

var ErrPlaceIndisponible = errors.New("baux : aucune place libre apres la patience maximale")

type Bail interface {
	// Renouveler prolonge le bail (a appeler a chaque requete de la lecture).
	Renouveler()
	// Liberer rend la place tout de suite (fin de lecture, deconnexion du client).
	Liberer()
}

// ErrNonTrouve : element absent du stockage local (distinct de ErrIntrouvable, qui
// est un 404 du fournisseur).
var ErrNonTrouve = errors.New("stockage : element non trouve")

// Stockage persiste le catalogue. Implementation attendue : SQLite.
type Stockage interface {
	// EcrireCatalogue remplace ATOMIQUEMENT le catalogue d'un fournisseur : en cas
	// d'echec, l'ancien catalogue reste intact.
	EcrireCatalogue(ctx context.Context, s *Instantane) error
	DerniereSynchro(ctx context.Context, fournisseurID string) (time.Time, error) // zero si jamais
	Chaines(ctx context.Context, f FiltreChaines) ([]Chaine, error)
	Categories(ctx context.Context, t Type) ([]Categorie, error)
	Chaine(ctx context.Context, id ContenuID) (*Chaine, error)
	Fermer() error
}

type FiltreChaines struct {
	CategorieID string
	Recherche   string // texte libre (nom)
	Limite      int    // 0 = defaut du stockage
	Decalage    int
}
