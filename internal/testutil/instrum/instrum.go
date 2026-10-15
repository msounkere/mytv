// Package instrum fournit une doublure de domain.Instrumentation pour les tests des
// modules instrumentes : elle garde ce qui est publie et verifie que les etiquettes
// restent a cardinalite bornee (ni session, ni URL, ni identifiant).
package instrum

import (
	"strings"
	"sync"
	"testing"

	"mytv/internal/domain"
)

// Mesure est un appel recu.
type Mesure struct {
	Nom         string
	Histogramme bool
	Valeur      float64
	Etiquettes  map[string]string
}

type Enregistreur struct {
	mu         sync.Mutex
	mesures    []Mesure
	evenements []string
}

var _ domain.Instrumentation = (*Enregistreur)(nil)

func (e *Enregistreur) ajoute(nom string, histo bool, v float64, et []string) {
	m := Mesure{Nom: nom, Histogramme: histo, Valeur: v, Etiquettes: map[string]string{}}
	for i := 0; i+1 < len(et); i += 2 {
		m.Etiquettes[et[i]] = et[i+1]
	}
	if len(et)%2 != 0 {
		m.Etiquettes["!impair"] = "!"
	}
	e.mu.Lock()
	e.mesures = append(e.mesures, m)
	e.mu.Unlock()
}

func (e *Enregistreur) Compter(nom string, delta float64, et ...string) {
	e.ajoute(nom, false, delta, et)
}
func (e *Enregistreur) Observer(nom string, v float64, et ...string) { e.ajoute(nom, true, v, et) }
func (e *Enregistreur) Evenement(t, m string) {
	e.mu.Lock()
	e.evenements = append(e.evenements, t+" "+m)
	e.mu.Unlock()
}

func (e *Enregistreur) Mesures() []Mesure {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]Mesure(nil), e.mesures...)
}

func (e *Enregistreur) Evenements() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.evenements...)
}

// Total additionne les compteurs de ce nom dont les etiquettes contiennent toutes les
// paires demandees (cle, valeur, cle, valeur...).
func (e *Enregistreur) Total(nom string, paires ...string) float64 {
	var t float64
	for _, m := range e.Mesures() {
		if m.Nom != nom || m.Histogramme {
			continue
		}
		ok := true
		for i := 0; i+1 < len(paires); i += 2 {
			if m.Etiquettes[paires[i]] != paires[i+1] {
				ok = false
			}
		}
		if ok {
			t += m.Valeur
		}
	}
	return t
}

// Observations compte les observations d'un histogramme.
func (e *Enregistreur) Observations(nom string, paires ...string) int {
	n := 0
	for _, m := range e.Mesures() {
		if m.Nom == nom && m.Histogramme {
			ok := true
			for i := 0; i+1 < len(paires); i += 2 {
				if m.Etiquettes[paires[i]] != paires[i+1] {
					ok = false
				}
			}
			if ok {
				n++
			}
		}
	}
	return n
}

var cleInterdites = map[string]bool{"session": true, "url": true, "id": true, "chaine": true, "utilisateur": true,
	"jeton": true, "ip": true, "profil": true, "contenu": true, "appareil": true}

// VerifierBornees echoue si une etiquette porte une cle interdite ou une valeur qui
// ressemble a une URL, un chemin, ou contient l'un des `interdits` (identifiant de
// session, mot de passe, numero de chaine...).
func (e *Enregistreur) VerifierBornees(t testing.TB, interdits ...string) {
	t.Helper()
	for _, m := range e.Mesures() {
		for k, v := range m.Etiquettes {
			if cleInterdites[k] || k == "!impair" {
				t.Errorf("%s : etiquette %q interdite", m.Nom, k)
			}
			if strings.ContainsAny(v, "/?&=") || strings.Contains(v, "://") || len(v) > 40 {
				t.Errorf("%s{%s=%q} : valeur qui ressemble a une URL ou non bornee", m.Nom, k, v)
			}
			for _, x := range interdits {
				if x != "" && strings.Contains(v, x) {
					t.Errorf("%s{%s=%q} contient %q", m.Nom, k, v, x)
				}
			}
		}
	}
}
