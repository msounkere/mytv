package telemetrie

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Evenement est une ligne du diagnostic.
type Evenement struct {
	Recu     time.Time      `json:"recu"`
	Source   string         `json:"source"` // « serveur » ou « client »
	Type     string         `json:"type"`
	Message  string         `json:"message,omitempty"`
	Appareil string         `json:"appareil,omitempty"` // empreinte courte, jamais l'identifiant du client
	Agent    string         `json:"agent,omitempty"`
	Donnees  map[string]any `json:"donnees,omitempty"`
}

// reglagesJournal (config.Diagnostic).
type reglagesJournal struct {
	Dossier      string
	Conservation time.Duration
	SegmentMax   int64
	TailleMax    int64
}

// journal stocke les evenements en segments JSONL, bornes de trois facons :
//   - un segment est ferme au-dela de SegmentMax octets, ou apres Conservation/2 :
//     ainsi une donnee ne reste jamais sur le disque plus de 1,5 x Conservation ;
//   - un segment dont le dernier evenement a plus de Conservation est supprime ;
//   - au-dela de TailleMax octets au total, les plus anciens segments sont supprimes.
//
// La lecture, elle, ne renvoie jamais un evenement plus vieux que Conservation.
//
// Fichiers : « diag-<debut>.cours » pour le segment ouvert, « diag-<debut>-<fin>.jsonl »
// une fois ferme (instants en nanosecondes Unix).
type journal struct {
	r       reglagesJournal
	horloge func() time.Time

	mu      sync.Mutex
	f       *os.File
	debut   time.Time
	dernier time.Time
	taille  int64
}

func ouvrirJournal(r reglagesJournal, horloge func() time.Time) (*journal, error) {
	if r.SegmentMax < 1 || r.TailleMax < r.SegmentMax || r.Conservation <= 0 {
		return nil, errors.New("telemetrie : diagnostic.segment_max, taille_max (>= segment_max) et conservation doivent etre positifs")
	}
	if err := os.MkdirAll(r.Dossier, 0o750); err != nil {
		return nil, fmt.Errorf("telemetrie : dossier du diagnostic : %w", err)
	}
	j := &journal{r: r, horloge: horloge}
	// Un segment « .cours » est le reste d'un arret brutal : on le ferme.
	restes, _ := filepath.Glob(filepath.Join(r.Dossier, "diag-*.cours"))
	for _, p := range restes {
		debut, ok := instantNom(filepath.Base(p), ".cours")
		if !ok {
			continue
		}
		fin := dernierInstant(p, debut)
		_ = os.Rename(p, j.nomFerme(debut, fin))
	}
	j.mu.Lock()
	j.purger()
	j.mu.Unlock()
	return j, nil
}

func (j *journal) nomFerme(debut, fin time.Time) string {
	return filepath.Join(j.r.Dossier, fmt.Sprintf("diag-%d-%d.jsonl", debut.UnixNano(), fin.UnixNano()))
}

func instantNom(nom, suffixe string) (time.Time, bool) {
	s := strings.TrimSuffix(strings.TrimPrefix(nom, "diag-"), suffixe)
	n, err := strconv.ParseInt(s, 10, 64)
	return time.Unix(0, n), err == nil
}

func dernierInstant(chemin string, defaut time.Time) time.Time {
	f, err := os.Open(chemin)
	if err != nil {
		return defaut
	}
	defer f.Close()
	dernier := defaut
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var e Evenement
		if json.Unmarshal(sc.Bytes(), &e) == nil && e.Recu.After(dernier) {
			dernier = e.Recu
		}
	}
	return dernier
}

type segment struct {
	chemin     string
	debut, fin time.Time
	taille     int64
	ouvert     bool
}

// segments liste les segments fermes (plus ancien d'abord) puis l'ouvert.
func (j *journal) segments() []segment {
	var res []segment
	ents, _ := os.ReadDir(j.r.Dossier)
	for _, e := range ents {
		n := e.Name()
		if !strings.HasPrefix(n, "diag-") || !strings.HasSuffix(n, ".jsonl") {
			continue
		}
		corps := strings.TrimSuffix(strings.TrimPrefix(n, "diag-"), ".jsonl")
		a, b, ok := strings.Cut(corps, "-")
		d, err1 := strconv.ParseInt(a, 10, 64)
		f, err2 := strconv.ParseInt(b, 10, 64)
		if !ok || err1 != nil || err2 != nil {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		res = append(res, segment{chemin: filepath.Join(j.r.Dossier, n), debut: time.Unix(0, d), fin: time.Unix(0, f), taille: info.Size()})
	}
	sort.Slice(res, func(a, b int) bool { return res[a].debut.Before(res[b].debut) })
	if j.f != nil {
		res = append(res, segment{chemin: j.f.Name(), debut: j.debut, fin: j.dernier, taille: j.taille, ouvert: true})
	}
	return res
}

// fermer ferme le segment ouvert (verrou tenu).
func (j *journal) fermer() {
	if j.f == nil {
		return
	}
	nom := j.f.Name()
	_ = j.f.Close()
	j.f = nil
	_ = os.Rename(nom, j.nomFerme(j.debut, j.dernier))
}

// purger supprime les segments expires puis les plus anciens au-dela de TailleMax
// (verrou tenu).
func (j *journal) purger() {
	limite := j.horloge().Add(-j.r.Conservation)
	segs := j.segments()
	var total int64
	for _, s := range segs {
		total += s.taille
	}
	for _, s := range segs {
		if s.ouvert {
			continue
		}
		if s.fin.Before(limite) || total > j.r.TailleMax {
			if os.Remove(s.chemin) == nil {
				total -= s.taille
			}
		}
	}
}

// ecrire ajoute les evenements (deja assainis). Les lignes d'un lot ne sont jamais
// repartis sur deux segments.
func (j *journal) ecrire(evs []Evenement) error {
	if len(evs) == 0 {
		return nil
	}
	var buf []byte
	for _, e := range evs {
		l, err := json.Marshal(e)
		if err != nil {
			continue
		}
		buf = append(append(buf, l...), '\n')
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	now := j.horloge()
	if j.f != nil && (j.taille+int64(len(buf)) > j.r.SegmentMax || now.Sub(j.debut) >= j.r.Conservation/2) {
		j.fermer()
		j.purger()
	}
	if j.f == nil {
		debut := now
		for {
			p := filepath.Join(j.r.Dossier, fmt.Sprintf("diag-%d.cours", debut.UnixNano()))
			f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_WRONLY|os.O_APPEND, 0o640)
			if errors.Is(err, os.ErrExist) {
				debut = debut.Add(time.Nanosecond)
				continue
			}
			if err != nil {
				return fmt.Errorf("telemetrie : segment : %w", err)
			}
			j.f, j.debut, j.dernier, j.taille = f, debut, debut, 0
			break
		}
		j.purger()
	}
	n, err := j.f.Write(buf)
	j.taille += int64(n)
	for _, e := range evs {
		if e.Recu.After(j.dernier) {
			j.dernier = e.Recu
		}
	}
	if j.dernier.Before(now) && n > 0 {
		j.dernier = now
	}
	return err
}

// Filtre de lecture.
type Filtre struct {
	Type   string // prefixe (« hls », « video:erreur »...) ; vide = tous
	Limite int    // 0 = defaut
}

const (
	limiteDefaut = 200
	limiteMax    = 1000
)

// lire renvoie les evenements recents, du plus recent au plus ancien.
func (j *journal) lire(f Filtre) ([]Evenement, error) {
	limite := f.Limite
	if limite <= 0 {
		limite = limiteDefaut
	}
	limite = min(limite, limiteMax)
	j.mu.Lock()
	defer j.mu.Unlock()
	depuis := j.horloge().Add(-j.r.Conservation)
	segs := j.segments()
	var res []Evenement
	for i := len(segs) - 1; i >= 0; i-- { // du plus recent au plus ancien
		if segs[i].fin.Before(depuis) {
			continue
		}
		evs, err := lireSegment(segs[i].chemin, depuis, f.Type)
		if err != nil {
			continue
		}
		for k := len(evs) - 1; k >= 0; k-- {
			res = append(res, evs[k])
			if len(res) >= limite {
				return res, nil
			}
		}
	}
	return res, nil
}

func lireSegment(chemin string, depuis time.Time, prefixe string) ([]Evenement, error) {
	fh, err := os.Open(chemin)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	var res []Evenement
	sc := bufio.NewScanner(fh)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var e Evenement
		if json.Unmarshal(sc.Bytes(), &e) != nil || e.Recu.Before(depuis) {
			continue // ligne abimee ou expiree
		}
		if prefixe != "" && !strings.HasPrefix(e.Type, prefixe) {
			continue
		}
		res = append(res, e)
	}
	return res, nil
}

func (j *journal) fermerTout() error {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.fermer()
	return nil
}

// octets : taille totale sur disque (tests, diagnostic).
func (j *journal) octets() int64 {
	j.mu.Lock()
	defer j.mu.Unlock()
	var t int64
	for _, s := range j.segments() {
		t += s.taille
	}
	return t
}
