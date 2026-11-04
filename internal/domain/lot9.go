package domain

// Contrats du lot 9 (US-42) : liste « A voir » par profil et preparation d'avance
// des fichiers sur le disque du serveur. Ce fichier est AJOUTE sans toucher aux
// contrats existants (regle du lot).

import (
	"context"
	"strings"
	"time"
)

// AVoirID identifie un element de la liste « A voir » : « vod:<ContenuID du film> »
// (« vod:iptva:2001 ») ou « serie:<ContenuID de la serie> » (« serie:iptva:s77 »).
// Une serie est UN element : la preparation, elle, travaille episode par episode.
type AVoirID string

const (
	prefixeAVoirFilm  = "vod:"
	prefixeAVoirSerie = "serie:"
)

// AVoirFilm / AVoirSerie construisent l'identifiant d'un element.
func AVoirFilm(c ContenuID) AVoirID  { return AVoirID(prefixeAVoirFilm + string(c)) }
func AVoirSerie(c ContenuID) AVoirID { return AVoirID(prefixeAVoirSerie + string(c)) }

// Decomposer rend le contenu vise et sa nature ; ok = faux si le prefixe est inconnu.
func (id AVoirID) Decomposer() (contenu ContenuID, serie, ok bool) {
	if c, trouve := strings.CutPrefix(string(id), prefixeAVoirSerie); trouve && c != "" {
		return ContenuID(c), true, true
	}
	if c, trouve := strings.CutPrefix(string(id), prefixeAVoirFilm); trouve && c != "" {
		return ContenuID(c), false, true
	}
	return "", false, false
}

// ElementAVoir est une entree de la liste d'un profil.
type ElementAVoir struct {
	ID      AVoirID
	Contenu ContenuID
	Serie   bool
	Rang    int       // position dans la liste (0 = premier)
	Ajoute  time.Time // date d'ajout
	// EpisodesAvance : episodes non vus a tenir prets pour une serie ; 0 = defaut de la
	// configuration (preparation.episodes_avance).
	EpisodesAvance int
	// DepuisSaison / DepuisEpisode : point de depart indique a la main pour une serie ;
	// 0 = a partir de l'episode entame (historique), sinon du premier.
	DepuisSaison, DepuisEpisode int
}

// ListeAVoir persiste la liste « A voir » de chaque profil. Comme EtatProfil, chaque
// methode ne touche que les lignes du profil passe.
type ListeAVoir interface {
	// AVoir rend la liste ordonnee (rang croissant). Les contenus disparus du
	// catalogue sont ignores.
	AVoir(ctx context.Context, profilID string) ([]ElementAVoir, error)
	// AjouterAVoir ajoute l'element en fin de liste, ou met a jour ses options s'il
	// y est deja (le rang est conserve). Idempotent.
	AjouterAVoir(ctx context.Context, profilID string, e ElementAVoir) error
	// RetirerAVoir est idempotent.
	RetirerAVoir(ctx context.Context, profilID string, id AVoirID) error
	// OrdonnerAVoir place les elements dans l'ordre des identifiants donnes ; ceux
	// qui n'y figurent pas suivent, dans leur ordre d'avant. Un identifiant inconnu
	// est ignore.
	OrdonnerAVoir(ctx context.Context, profilID string, ids []AVoirID) error
	// ProfilsAVoir liste les profils dont la liste n'est pas vide (moteur de preparation).
	ProfilsAVoir(ctx context.Context) ([]string, error)
}

// PartVue : au-dela de cette part de la duree, un contenu est VU (film termine, episode
// termine). C'est la regle commune de la liste et de la preparation.
const PartVue = 0.9

// Vue dit si une entree d'historique compte comme vue (duree connue et part >= PartVue).
func (l Lecture) Vue() bool {
	return l.Duree > 0 && float64(l.Position) >= PartVue*float64(l.Duree)
}

// EtatPreparation d'un fichier a tenir pret.
type EtatPreparation string

const (
	PreparationAttente EtatPreparation = "attente"  // planifie, pas encore telecharge (ou en pause)
	PreparationEnCours EtatPreparation = "en_cours" // telechargement en cours
	PreparationPrete   EtatPreparation = "pret"     // fichier complet sur le disque
	PreparationEchec   EtatPreparation = "echec"    // abandonne (cause assainie)
)

// FichierPrepare est l'etat d'un fichier suivi par la preparation.
type FichierPrepare struct {
	Contenu   ContenuID
	Etat      EtatPreparation
	Octets    int64 // octets deja sur le disque
	Total     int64 // taille annoncee par le fournisseur ; 0 si inconnue
	DebitKbps int   // debit du telechargement en cours (ou moyen du dernier)
	Cause     string
	// Saison / Episode : pour un episode de serie ; 0 sinon.
	Saison, Episode int
	// Serie : la serie parente d'un episode (vide pour un film).
	Serie ContenuID
}

// EtatAVoir est l'etat de preparation d'un element de la liste.
type EtatAVoir struct {
	Etat EtatPreparation
	// Termine : film vu, ou serie dont tous les episodes sont vus : l'element sort de la liste.
	Termine bool
	// Fichiers : le fichier du film, ou les episodes retenus pour la serie, dans l'ordre.
	Fichiers []FichierPrepare
}

// Disponibilite est le resultat de la sonde GET /vod/{id}/disponibilite.
type Disponibilite struct {
	Etat             string // pret | immediat | lent | inconnu
	DebitKbps        int    // debit mesure (0 si aucune mesure)
	DebitFichierKbps int    // debit du fichier retenu pour le verdict (0 si aucune mesure)
	Mesure           time.Time
}
