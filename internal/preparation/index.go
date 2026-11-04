package preparation

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"mytv/internal/domain"
)

// entree est un fichier suivi. Les champs exportes sont persistes dans index.json ; les
// autres sont recalcules a chaque planification.
type entree struct {
	Contenu domain.ContenuID       `json:"contenu"`
	Nom     string                 `json:"nom"` // nom du fichier final dans le dossier
	Etat    domain.EtatPreparation `json:"etat"`
	Octets  int64                  `json:"octets"`
	Total   int64                  `json:"total"` // 0 tant que le fournisseur ne l'a pas dit
	Cause   string                 `json:"cause,omitempty"`
	Serie   domain.ContenuID       `json:"serie,omitempty"`
	Saison  int                    `json:"saison,omitempty"`
	Episode int                    `json:"episode,omitempty"`
	PretLe  time.Time              `json:"pret_le,omitempty"`
	// Reessai : echec TRANSITOIRE (reseau, 5xx, 458) : echeance de la prochaine tentative
	// automatique (US-44 CA2) ; zero pour un echec permanent. Echecs : tentatives
	// transitoires consecutives, qui fixent le recul (15 min, 30 min, 1 h, 2 h, 4 h).
	Reessai time.Time `json:"reessai,omitzero"`
	Echecs  int       `json:"echecs,omitempty"`

	demande     bool // voulu par la derniere planification
	priorite    int  // rang dans le plan (0 = le plus urgent) ; sans objet si !demande
	enLecture   int  // lectures en cours (vod) : jamais efface
	debitKbps   int  // debit du telechargement en cours
	vu          bool // vu par un profil (part >= PartVue) : efface des qu'il n'est plus demande
	budgetPlein bool // derniere tentative refusee faute de place (raison `budget_plein`)
}

func (e *entree) public() domain.FichierPrepare {
	return domain.FichierPrepare{Contenu: e.Contenu, Etat: e.Etat, Octets: e.Octets, Total: e.Total, DebitKbps: e.debitKbps,
		Cause: e.Cause, Saison: e.Saison, Episode: e.Episode, Serie: e.Serie}
}

// detail : l'etat public plus l'echeance de nouvelle tentative d'un echec transitoire.
// La raison d'une attente est ajoutee par l'appelant (elle depend du plan et du fournisseur).
func (e *entree) detail() domain.FichierDetaille {
	d := domain.FichierDetaille{FichierPrepare: e.public()}
	if e.Etat == domain.PreparationEchec {
		d.Reessai = e.Reessai
	}
	return d
}

func (e *entree) partiel() string { return e.Nom + ".partiel" }

const nomIndex = "index.json"

var (
	reExtension = regexp.MustCompile(`^[a-z0-9]{1,5}$`)
	// reNomFichier : ce que ce paquet ecrit dans le dossier ; tout autre fichier est ignore.
	reNomFichier = regexp.MustCompile(`^(film|episode)-[A-Za-z0-9_.-]{1,100}(\.partiel)?$`)
)

// nomFichier derive le nom du fichier de l'identifiant du contenu, jamais du nom donne
// par le fournisseur : « film-iptva-2001.mp4 », « episode-iptva-77123.mkv ».
func nomFichier(id domain.ContenuID, episode bool, extension string) string {
	base := strings.ReplaceAll(string(id), ":", "-")
	if episode {
		return "episode-" + base
	}
	ext := strings.ToLower(strings.TrimPrefix(extension, "."))
	if !reExtension.MatchString(ext) {
		ext = "mp4"
	}
	return "film-" + base + "." + ext
}

// chargerIndex relit index.json et le rapproche du dossier. Verrou non requis (construction).
func (m *Moteur) chargerIndex() error {
	chemin := filepath.Join(m.r.Dossier, nomIndex)
	brut, err := os.ReadFile(chemin)
	var liste []*entree
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return fmt.Errorf("preparation : lecture de l'index : %w", err)
	default:
		if err := json.Unmarshal(brut, &liste); err != nil {
			m.log.Warn("preparation : index illisible, reconstruit", "erreur", err)
			liste = nil
		}
	}
	connus := map[string]bool{}
	for _, e := range liste {
		if e == nil || e.Contenu == "" || !reNomFichier.MatchString(e.Nom) || strings.HasSuffix(e.Nom, ".partiel") {
			continue
		}
		final, partiel := filepath.Join(m.r.Dossier, e.Nom), filepath.Join(m.r.Dossier, e.partiel())
		switch st, err := os.Stat(final); {
		case err == nil && e.Total > 0 && st.Size() == e.Total:
			e.Etat, e.Octets, e.Cause = domain.PreparationPrete, e.Total, ""
			os.Remove(partiel)
			connus[e.Nom] = true
		default:
			os.Remove(final)
			st, err := os.Stat(partiel)
			if err != nil {
				continue // ni fichier ni partiel : oublie
			}
			// Reprise d'un telechargement interrompu par un redemarrage.
			e.Etat, e.Octets, e.Cause = domain.PreparationAttente, st.Size(), ""
			connus[e.partiel()] = true
		}
		m.index[e.Contenu] = e
	}
	// Fichiers du dossier inconnus de l'index : orphelins (index perdu), effaces.
	entrees, err := os.ReadDir(m.r.Dossier)
	if err != nil {
		return fmt.Errorf("preparation : dossier : %w", err)
	}
	for _, d := range entrees {
		if d.IsDir() || d.Name() == nomIndex || !reNomFichier.MatchString(d.Name()) || connus[d.Name()] {
			continue
		}
		os.Remove(filepath.Join(m.r.Dossier, d.Name()))
	}
	m.sauverIndexLocked()
	return nil
}

// sauverIndexLocked ecrit index.json atomiquement (fichier temporaire puis renommage).
// Verrou tenu (ou construction).
func (m *Moteur) sauverIndexLocked() {
	liste := make([]*entree, 0, len(m.index))
	for _, e := range m.index {
		liste = append(liste, e)
	}
	sort.Slice(liste, func(i, j int) bool { return liste[i].Contenu < liste[j].Contenu })
	brut, err := json.MarshalIndent(liste, "", " ")
	if err != nil {
		return
	}
	tmp := filepath.Join(m.r.Dossier, nomIndex+".tmp")
	if err := os.WriteFile(tmp, brut, 0o600); err != nil {
		m.log.Warn("preparation : ecriture de l'index", "erreur", err)
		return
	}
	if err := os.Rename(tmp, filepath.Join(m.r.Dossier, nomIndex)); err != nil {
		m.log.Warn("preparation : ecriture de l'index", "erreur", err)
	}
}

// effacerLocked supprime les fichiers d'une entree (final et partiel). Verrou tenu.
func (m *Moteur) effacerLocked(e *entree) {
	os.Remove(filepath.Join(m.r.Dossier, e.Nom))
	os.Remove(filepath.Join(m.r.Dossier, e.partiel()))
	e.Octets = 0
}

// occupeLocked : octets sur le disque, hors `sauf`. Verrou tenu.
func (m *Moteur) occupeLocked(sauf *entree) int64 {
	var n int64
	for _, e := range m.index {
		if e != sauf {
			n += e.Octets
		}
	}
	return n
}

// faireDeLaPlace libere ce qu'il faut pour que `pour` puisse atteindre `total` octets,
// dans l'ordre du lot : d'abord les episodes vus, puis les fichiers que plus aucune liste
// ne demande, puis les prets moins urgents que `pour` (les plus anciens d'abord) ; jamais
// un fichier en cours de lecture ni `pour`. Vrai si la place y est.
func (m *Moteur) faireDeLaPlace(pour *entree, total int64) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if total > m.r.Budget {
		return false
	}
	manque := func() int64 { return m.occupeLocked(pour) + total - m.r.Budget }
	if manque() <= 0 {
		return true
	}
	// Candidats par rang d'eviction, puis par anciennete.
	type candidat struct {
		e    *entree
		rang int
	}
	var cands []candidat
	for _, e := range m.index {
		if e == pour || e.enLecture > 0 || e.Octets == 0 {
			continue
		}
		switch {
		case !e.demande && e.vu:
			cands = append(cands, candidat{e, 0})
		case !e.demande:
			cands = append(cands, candidat{e, 1})
		case e.Etat == domain.PreparationPrete && e.priorite > pour.priorite:
			cands = append(cands, candidat{e, 2})
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		a, b := cands[i], cands[j]
		if a.rang != b.rang {
			return a.rang < b.rang
		}
		if a.rang == 2 && a.e.priorite != b.e.priorite {
			return a.e.priorite > b.e.priorite // le moins urgent d'abord
		}
		return a.e.PretLe.Before(b.e.PretLe)
	})
	for _, c := range cands {
		if manque() <= 0 {
			break
		}
		m.log.Info("preparation : fichier efface pour faire de la place", "contenu", string(c.e.Contenu), "octets", c.e.Octets, "etat", string(c.e.Etat))
		m.effacerLocked(c.e)
		delete(m.index, c.e.Contenu)
		m.inst.Compter("preparation_fichiers_total", 1, "etat", "efface")
	}
	m.sauverIndexLocked()
	return manque() <= 0
}

func trierFichiers(l []domain.FichierDetaille) {
	sort.Slice(l, func(i, j int) bool { return l[i].Contenu < l[j].Contenu })
}
