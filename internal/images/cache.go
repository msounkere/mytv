package images

import (
	"bufio"
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// cache est un cache DISQUE borne en octets, a eviction LRU.
//
// POURQUOI le disque : 22 000 logos + des dizaines de milliers d'affiches ne tiennent
// pas dans la memoire d'un petit serveur, et le cache doit survivre a un redemarrage
// (sinon chaque deploiement re-martele le fournisseur). L'index (cle, taille, date)
// vit en memoire ; le contenu reste dans un fichier par image.
//
// Format d'un fichier : « <content-type>\n<octets de l'image> ». Le type voyage avec
// l'image pour qu'un redemarrage n'ait rien d'autre a relire. Le nom du fichier est
// le hash de l'URL : aucun caractere de l'URL n'atteint le systeme de fichiers.
type cache struct {
	dossier  string
	maxOcts  int64
	validite time.Duration
	horloge  func() time.Time

	mu    sync.Mutex
	total int64
	lru   *list.List // devant = le plus recent
	idx   map[string]*list.Element
}

type entreeCache struct {
	cle    string
	taille int64 // octets du fichier entier (en-tete compris) : c'est ce que le disque porte
	modif  time.Time
}

const suffixe = ".img"

// ouvrirCache cree le dossier (droits 700), supprime les restes d'ecritures
// interrompues et reindexe ce qui existe, du plus ancien au plus recent.
func ouvrirCache(dossier string, maxOcts int64, validite time.Duration, horloge func() time.Time) (*cache, error) {
	if err := os.MkdirAll(dossier, 0o700); err != nil {
		return nil, fmt.Errorf("images : dossier du cache : %w", err)
	}
	if err := os.Chmod(dossier, 0o700); err != nil {
		return nil, fmt.Errorf("images : droits du dossier du cache : %w", err)
	}
	c := &cache{dossier: dossier, maxOcts: maxOcts, validite: validite, horloge: horloge, lru: list.New(), idx: map[string]*list.Element{}}
	ents, err := os.ReadDir(dossier)
	if err != nil {
		return nil, fmt.Errorf("images : lecture du cache : %w", err)
	}
	var trouvees []entreeCache
	for _, e := range ents {
		nom := e.Name()
		if strings.HasPrefix(nom, ".tmp-") {
			_ = os.Remove(filepath.Join(dossier, nom))
			continue
		}
		cle, ok := strings.CutSuffix(nom, suffixe)
		if !ok || !e.Type().IsRegular() {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		trouvees = append(trouvees, entreeCache{cle: cle, taille: info.Size(), modif: info.ModTime()})
	}
	sort.Slice(trouvees, func(i, j int) bool { return trouvees[i].modif.Before(trouvees[j].modif) })
	for _, e := range trouvees {
		e := e
		c.idx[e.cle] = c.lru.PushFront(&e)
		c.total += e.taille
	}
	c.purger()
	return c, nil
}

// cleDe : nom stable d'une URL dans le cache.
func cleDe(url string) string {
	h := sha256.Sum256([]byte(url))
	return hex.EncodeToString(h[:16])
}

func (c *cache) chemin(cle string) string { return filepath.Join(c.dossier, cle+suffixe) }

// resultatCache est une image lue dans le cache.
type resultatCache struct {
	Type   string
	Modif  time.Time
	Perime bool
	f      *os.File
	debut  int64
	taille int64
}

func (r *resultatCache) Lecteur() io.ReadSeeker { return io.NewSectionReader(r.f, r.debut, r.taille) }
func (r *resultatCache) Fermer() error          { return r.f.Close() }

// ouvrir rend l'image si elle est en cache (perimee ou non : l'appelant decide).
func (c *cache) ouvrir(cle string) (*resultatCache, bool) {
	c.mu.Lock()
	el, ok := c.idx[cle]
	if !ok {
		c.mu.Unlock()
		return nil, false
	}
	c.lru.MoveToFront(el)
	e := *el.Value.(*entreeCache)
	c.mu.Unlock()

	f, err := os.Open(c.chemin(cle))
	if err != nil {
		c.oublier(cle) // fichier disparu : l'index ne doit pas mentir
		return nil, false
	}
	ligne, err := bufio.NewReaderSize(f, 512).ReadString('\n')
	if err != nil || len(ligne) > 256 || int64(len(ligne)) > e.taille {
		f.Close()
		c.oublier(cle) // fichier tronque ou etranger
		_ = os.Remove(c.chemin(cle))
		return nil, false
	}
	debut := int64(len(ligne))
	return &resultatCache{Type: strings.TrimSpace(ligne), Modif: e.modif, Perime: c.horloge().Sub(e.modif) > c.validite,
		f: f, debut: debut, taille: e.taille - debut}, true
}

// ecrire enregistre une image (ecriture atomique : temporaire puis renommage, un
// lecteur ne voit jamais un fichier a moitie ecrit) et evince le plus ancien au besoin.
func (c *cache) ecrire(cle, typeMime string, corps []byte) error {
	tmp, err := os.CreateTemp(c.dossier, ".tmp-*")
	if err != nil {
		return err
	}
	_, err = tmp.WriteString(typeMime + "\n")
	if err == nil {
		_, err = tmp.Write(corps)
	}
	if e := tmp.Close(); err == nil {
		err = e
	}
	if err == nil {
		err = os.Rename(tmp.Name(), c.chemin(cle))
	}
	if err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	taille := int64(len(typeMime)) + 1 + int64(len(corps))
	c.mu.Lock()
	if el, ok := c.idx[cle]; ok {
		c.total -= el.Value.(*entreeCache).taille
		c.lru.Remove(el)
	}
	c.idx[cle] = c.lru.PushFront(&entreeCache{cle: cle, taille: taille, modif: c.horloge()})
	c.total += taille
	c.mu.Unlock()
	c.purger()
	return nil
}

// purger evince les plus anciens jusqu'a rentrer dans la borne. L'entree qui vient
// d'etre ecrite (devant) est evincee en dernier : si elle seule depasse, c'est la
// configuration qui est incoherente (validee ailleurs).
func (c *cache) purger() {
	for {
		c.mu.Lock()
		if c.total <= c.maxOcts || c.lru.Len() == 0 {
			c.mu.Unlock()
			return
		}
		el := c.lru.Back()
		e := el.Value.(*entreeCache)
		c.lru.Remove(el)
		delete(c.idx, e.cle)
		c.total -= e.taille
		c.mu.Unlock()
		_ = os.Remove(c.chemin(e.cle))
	}
}

func (c *cache) oublier(cle string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.idx[cle]; ok {
		c.total -= el.Value.(*entreeCache).taille
		c.lru.Remove(el)
		delete(c.idx, cle)
	}
}

// octets et nombre : pour les tests et la supervision.
func (c *cache) octets() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}
