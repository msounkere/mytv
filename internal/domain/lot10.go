package domain

// Contrats du lot 10 (US-44) : preparation reparable et explicite. Ce fichier est
// AJOUTE sans toucher aux contrats existants (regle du lot) : les types du lot 9
// (FichierPrepare, EtatAVoir) sont ENRICHIS par composition, pas modifies.

import "time"

// RaisonAttente dit pourquoi un fichier planifie n'est pas en cours de telechargement.
type RaisonAttente string

const (
	// RaisonLectureEnCours : une lecture (direct ou VOD) tient la place chez le fournisseur ;
	// la preparation attend qu'elle finisse.
	RaisonLectureEnCours RaisonAttente = "lecture_en_cours"
	// RaisonFile : le fichier attend son tour (Position = rang dans la file, 1 = le prochain).
	RaisonFile RaisonAttente = "file"
	// RaisonBudgetPlein : le fichier ne tient pas dans preparation.budget tant que rien
	// n'est efface.
	RaisonBudgetPlein RaisonAttente = "budget_plein"
	// RaisonRepriseApres : une lecture vient de finir ; la preparation reprend apres
	// preparation.reprise_apres.
	RaisonRepriseApres RaisonAttente = "reprise_apres"
)

// FichierDetaille est un FichierPrepare plus ce qu'il faut pour que l'interface
// explique une attente ou un echec :
//   - Raison et Position (etat `attente` seulement) ;
//   - Reessai (etat `echec` seulement) : echeance de la prochaine tentative automatique
//     d'un echec TRANSITOIRE (reseau, 5xx, 458) ; zero pour un echec permanent (404,
//     fichier incoherent, contenu disparu du catalogue).
type FichierDetaille struct {
	FichierPrepare
	Raison   RaisonAttente
	Position int
	Reessai  time.Time
}

// EtatAVoirDetaille est l'EtatAVoir du lot 9 avec des fichiers detailles. L'etat de
// l'element (Etat, et donc raison / reessai) est celui de son PREMIER fichier.
type EtatAVoirDetaille struct {
	Etat     EtatPreparation
	Termine  bool
	Fichiers []FichierDetaille
}
