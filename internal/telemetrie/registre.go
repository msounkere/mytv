// Package telemetrie mesure et diagnostique : metriques au format Prometheus et
// evenements recents (ceux des modules et ceux que l'interface envoie).
//
// Principes : les modules ne dependent que de domain.Instrumentation ; rien de
// sensible n'est jamais stocke ou expose (identifiants, jetons, URL) ; tout est
// borne (series, evenements, disque, debit) ; a diagnostic.actif = false,
// l'instrumentation est domain.SansInstrumentation (cout nul).
package telemetrie

import (
	"fmt"
	"io"
	"math"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// bucketsParDefaut : secondes (latences) ; assez large pour des octets aussi.
var bucketsParDefaut = []float64{0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120}

var (
	reNom = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	// etiquettesInterdites : nom d'etiquette qui porterait un identifiant, une adresse ou un
	// jeton, donc une cardinalite non bornee ou une donnee personnelle. La paire est ignoree.
	etiquettesInterdites = map[string]bool{
		"session": true, "session_id": true, "utilisateur": true, "user": true, "username": true,
		"url": true, "uri": true, "chemin": true, "path": true, "id": true, "jeton": true, "token": true,
		"ip": true, "adresse": true, "appareil": true, "profil": true, "chaine": true, "contenu": true,
		"film": true, "episode": true,
	}
)

const (
	valeurMax     = 40
	valeurAutre   = "autre"
	etiquettesMax = 6
)

// serie est un jeu d'etiquettes d'une metrique.
type serie struct {
	etiquettes []paire
	valeur     float64  // compteur ou jauge
	buckets    []uint64 // histogramme : non cumulatif, un par borne + un pour +Inf
	somme      float64
	nombre     uint64
}

type paire struct{ k, v string }

type famille struct {
	histo  bool
	jauge  bool
	series map[string]*serie
	trop   *serie // serie de debordement
}

// registre garde les metriques en memoire, series bornees par metrique.
type registre struct {
	mu          sync.Mutex
	seriesMax   int
	buckets     []float64
	familles    map[string]*famille
	famillesMax int
}

func nouveauRegistre(seriesMax int, buckets []float64) *registre {
	if len(buckets) == 0 {
		buckets = bucketsParDefaut
	}
	return &registre{seriesMax: max(seriesMax, 1), buckets: buckets, familles: map[string]*famille{}, famillesMax: 500}
}

// normaliser valide et assainit les etiquettes (cle, valeur, cle, valeur...). Les
// paires dont la cle est interdite ou invalide sont ignorees ; les valeurs sont
// reduites a des caracteres sans danger et tronquees.
func normaliser(e []string) []paire {
	if len(e)%2 != 0 {
		return nil
	}
	var res []paire
	for i := 0; i+1 < len(e) && len(res) < etiquettesMax; i += 2 {
		k, v := e[i], e[i+1]
		if !reNom.MatchString(k) || etiquettesInterdites[k] {
			continue
		}
		res = append(res, paire{k, nettoyerValeur(Masquer(v))})
	}
	sort.Slice(res, func(i, j int) bool { return res[i].k < res[j].k })
	return res
}

func nettoyerValeur(v string) string {
	if len(v) > valeurMax {
		v = v[:valeurMax]
	}
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-', r == '.', r == ':':
			return r
		}
		return '_'
	}, v)
}

func cleSerie(e []paire) string {
	var b strings.Builder
	for _, p := range e {
		b.WriteString(p.k)
		b.WriteByte(0)
		b.WriteString(p.v)
		b.WriteByte(1)
	}
	return b.String()
}

// trouver renvoie la serie (la cree si la borne le permet, sinon la serie de
// debordement « autre »). Verrou tenu.
func (r *registre) trouver(nom string, histo, jauge bool, e []paire) *serie {
	f, ok := r.familles[nom]
	if !ok {
		if len(r.familles) >= r.famillesMax {
			return nil
		}
		f = &famille{histo: histo, jauge: jauge, series: map[string]*serie{}}
		r.familles[nom] = f
	}
	if f.histo != histo || f.jauge != jauge {
		return nil // meme nom utilise pour deux types : ignore
	}
	k := cleSerie(e)
	if s, ok := f.series[k]; ok {
		return s
	}
	nouvelle := func(et []paire) *serie {
		s := &serie{etiquettes: et}
		if histo {
			s.buckets = make([]uint64, len(r.buckets)+1)
		}
		return s
	}
	if len(f.series) < r.seriesMax {
		s := nouvelle(e)
		f.series[k] = s
		return s
	}
	if f.trop == nil {
		autre := make([]paire, len(e))
		for i, p := range e {
			autre[i] = paire{p.k, valeurAutre}
		}
		f.trop = nouvelle(autre)
	}
	return f.trop
}

func (r *registre) compter(nom string, delta float64, e []string) {
	if !reNom.MatchString(nom) || delta < 0 || math.IsNaN(delta) || math.IsInf(delta, 0) {
		return
	}
	et := normaliser(e)
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.trouver(nom, false, false, et); s != nil {
		s.valeur += delta
	}
}

// fixer pose la valeur d'une jauge (une valeur qui monte et descend, comme le temps
// restant avant l'expiration d'un abonnement).
func (r *registre) fixer(nom string, v float64, e []string) {
	if !reNom.MatchString(nom) || math.IsNaN(v) || math.IsInf(v, 0) {
		return
	}
	et := normaliser(e)
	r.mu.Lock()
	defer r.mu.Unlock()
	if s := r.trouver(nom, false, true, et); s != nil {
		s.valeur = v
	}
}

func (r *registre) observer(nom string, v float64, e []string) {
	if !reNom.MatchString(nom) || math.IsNaN(v) || math.IsInf(v, 0) {
		return
	}
	et := normaliser(e)
	r.mu.Lock()
	defer r.mu.Unlock()
	s := r.trouver(nom, true, false, et)
	if s == nil {
		return
	}
	i := sort.SearchFloat64s(r.buckets, v) // premiere borne >= v
	s.buckets[i]++
	s.somme += v
	s.nombre++
}

// nombreSeries : total de series (tests, diagnostic).
func (r *registre) nombreSeries(nom string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	f, ok := r.familles[nom]
	if !ok {
		return 0
	}
	n := len(f.series)
	if f.trop != nil {
		n++
	}
	return n
}

func echapper(v string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(v)
}

func libelle(e []paire, extra ...paire) string {
	tout := append(append([]paire(nil), e...), extra...)
	if len(tout) == 0 {
		return ""
	}
	parts := make([]string, len(tout))
	for i, p := range tout {
		parts[i] = fmt.Sprintf(`%s="%s"`, p.k, echapper(p.v))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

// ecrire produit le format texte de Prometheus, deterministe (noms et series tries).
func (r *registre) ecrire(w io.Writer) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	noms := make([]string, 0, len(r.familles))
	for n := range r.familles {
		noms = append(noms, n)
	}
	sort.Strings(noms)
	for _, n := range noms {
		f := r.familles[n]
		typ := "counter"
		if f.histo {
			typ = "histogram"
		} else if f.jauge {
			typ = "gauge"
		}
		if _, err := fmt.Fprintf(w, "# TYPE %s %s\n", n, typ); err != nil {
			return err
		}
		cles := make([]string, 0, len(f.series))
		for k := range f.series {
			cles = append(cles, k)
		}
		sort.Strings(cles)
		series := make([]*serie, 0, len(cles)+1)
		for _, k := range cles {
			series = append(series, f.series[k])
		}
		if f.trop != nil {
			series = append(series, f.trop)
		}
		for _, s := range series {
			if !f.histo {
				if _, err := fmt.Fprintf(w, "%s%s %s\n", n, libelle(s.etiquettes), formater(s.valeur)); err != nil {
					return err
				}
				continue
			}
			var cumul uint64
			for i, b := range r.buckets {
				cumul += s.buckets[i]
				fmt.Fprintf(w, "%s_bucket%s %d\n", n, libelle(s.etiquettes, paire{"le", formater(b)}), cumul)
			}
			cumul += s.buckets[len(r.buckets)]
			fmt.Fprintf(w, "%s_bucket%s %d\n", n, libelle(s.etiquettes, paire{"le", "+Inf"}), cumul)
			fmt.Fprintf(w, "%s_sum%s %s\n", n, libelle(s.etiquettes), formater(s.somme))
			if _, err := fmt.Fprintf(w, "%s_count%s %d\n", n, libelle(s.etiquettes), s.nombre); err != nil {
				return err
			}
		}
	}
	return nil
}

func formater(f float64) string {
	return fmt.Sprintf("%g", f)
}
