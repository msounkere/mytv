package live

import (
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var (
	reSequence      = regexp.MustCompile(`(?m)^#EXT-X-MEDIA-SEQUENCE:\s*(\d+)`)
	reDiscoSeq      = regexp.MustCompile(`(?m)^#EXT-X-DISCONTINUITY-SEQUENCE:\s*(\d+)`)
	reDuree         = regexp.MustCompile(`(?m)^#EXTINF:\s*([0-9.]+)`)
	reDiscontinuite = regexp.MustCompile(`(?m)^#EXT-X-DISCONTINUITY\s*$`)
)

// suiviSequence retient, par chaine, ce qu'on sait de la DERNIERE playlist amont servie :
// son numero de sequence, depuis quand il n'a plus bouge, et la numerotation CONTINUE que
// le relais presente au lecteur (voir continuite.go).
//
// POURQUOI : en recette reelle le lecteur se bloquait (tampon vide) apres 30 a 60 s sans
// aucune demande de segment, alors que les playlists etaient rechargees sans erreur. Sans
// ce suivi on ne sait pas dire si c'est le fournisseur qui n'a pas produit de nouveau
// segment, ou si le defaut est chez nous. Et quand le fournisseur REDEMARRE son flux
// (MEDIA-SEQUENCE qui retombe a 0, nouveaux noms de segments, horodatages repartant de
// zero), recopier sa numerotation viole la RFC 8216 (la sequence ne decroit jamais) et
// hls.js n'aligne plus rien : il replace les nouveaux segments a la position 0, cherche
// a 0, et boucle (« bufferAppendNoProgress » + seeking pos 0, mesure le 18/10).
type suiviSequence struct {
	sequence int64
	depuis   time.Time // instant ou ce numero est apparu
	segments int       // segments annonces par la derniere playlist
	noms     map[string]struct{}
	discos   int // tags EXT-X-DISCONTINUITY presents dans la derniere playlist SERVIE (les notres compris)
	ds       int64

	continuite
}

// continuite : la numerotation presentee au lecteur, par epoque du flux amont.
type continuite struct {
	decalage   int64 // sequence servie = sequence amont + decalage
	decalageDS int64 // DISCONTINUITY-SEQUENCE servie = celle de l'amont + decalageDS
	disco      int64 // numero SERVI du segment devant lequel on insere notre discontinuite ; -1 aucun
	epoque     int   // nombre de redemarrages du flux amont vus (cle de cache des segments)
}

// lecturePlaylist : ce qu'on lit d'une playlist de media.
type lecturePlaylist struct {
	sequence int64
	ds       int64
	segments int
	moyenne  float64
	noms     map[string]struct{}
	discos   int
	ok       bool // une ligne MEDIA-SEQUENCE existe
}

// observation resume ce qu'on apprend d'un telechargement de playlist amont.
type observation struct {
	Sequence  int64
	Segments  int
	Dernier   float64 // duree moyenne d'un segment (s), 0 si inconnue
	Avance    bool    // la sequence a change depuis la playlist precedente
	Recul     bool    // la sequence a RECULE : le fournisseur a redemarre son flux (nouvelle epoque)
	ImmobileS float64 // secondes pendant lesquelles la sequence n'a pas bouge
	Connue    bool    // une playlist precedente existait
}

func analyserSequence(corps string) lecturePlaylist {
	var l lecturePlaylist
	if m := reSequence.FindStringSubmatch(corps); m != nil {
		l.sequence, _ = strconv.ParseInt(m[1], 10, 64)
		l.ok = true
	}
	if m := reDiscoSeq.FindStringSubmatch(corps); m != nil {
		l.ds, _ = strconv.ParseInt(m[1], 10, 64)
	}
	var somme float64
	for _, m := range reDuree.FindAllStringSubmatch(corps, -1) {
		d, _ := strconv.ParseFloat(m[1], 64)
		somme += d
		l.segments++
	}
	if l.segments > 0 {
		l.moyenne = somme / float64(l.segments)
	}
	l.discos = len(reDiscontinuite.FindAllStringIndex(corps, -1))
	l.noms = map[string]struct{}{}
	for _, ligne := range strings.Split(corps, "\n") {
		ligne = strings.TrimSpace(ligne)
		if ligne == "" || strings.HasPrefix(ligne, "#") {
			continue
		}
		l.noms[nomDeSegment(ligne)] = struct{}{}
	}
	return l
}

// nomDeSegment : le nom de fichier d'une ligne de segment (sans chemin ni parametres),
// stable d'une playlist a l'autre chez un fournisseur qui change le hash du chemin.
func nomDeSegment(ligne string) string {
	ligne, _, _ = strings.Cut(ligne, "?")
	return path.Base(ligne)
}

func lireSequence(corps string) (seq int64, segments int, moyenne float64, ok bool) {
	l := analyserSequence(corps)
	return l.sequence, l.segments, l.moyenne, l.ok
}

// observer met a jour le suivi de la chaine ; l'appelant tient r.mu. Le corps est celui
// qui sera SERVI (meme numerotation que l'amont tant que la continuite n'est pas appliquee).
func (r *Relais) observer(chaine, corps string, now time.Time) (observation, bool) {
	l := analyserSequence(corps)
	if !l.ok {
		return observation{}, false
	}
	if r.sequences == nil {
		r.sequences = map[string]suiviSequence{}
	}
	prec, vu := r.sequences[chaine]
	o := observation{Sequence: l.sequence, Segments: l.segments, Dernier: l.moyenne, Connue: vu}
	s := suiviSequence{sequence: l.sequence, depuis: now, segments: l.segments, noms: l.noms, ds: l.ds, continuite: prec.continuite}
	switch {
	case !vu:
		s.disco = -1
	case l.sequence > prec.sequence:
		o.Avance = true
	case l.sequence < prec.sequence && !memesSegments(prec.noms, l.noms):
		// Redemarrage du flux : aucun segment en commun avec la playlist precedente.
		o.Avance, o.Recul = true, true
		s.continuite = prec.continuite.nouvelleEpoque(prec, l)
	case l.sequence < prec.sequence:
		// Simple retard d'un noeud du fournisseur (302 vers un autre serveur) : memes
		// segments, meme numerotation. La correspondance numero -> segment reste la bonne.
		o.Avance = true
	case l.segments != prec.segments:
		// Meme numero de sequence mais la fenetre grandit : un flux qui vient de
		// redemarrer remplit sa fenetre a la sequence 0 (vu en reel : 1, 2... 6 segments
		// en 50 s). Il AVANCE, il n'est pas immobile.
		o.Avance = true
	default:
		s.depuis = prec.depuis
		o.ImmobileS = now.Sub(prec.depuis).Seconds()
	}
	// Notre discontinuite est sortie de la fenetre : elle se compte desormais dans
	// DISCONTINUITY-SEQUENCE (RFC 8216, 4.3.3.3).
	if s.disco >= 0 && s.disco < l.sequence+s.decalage {
		s.decalageDS++
		s.disco = -1
	}
	s.discos = l.discos
	if s.disco >= 0 {
		s.discos++
	}
	r.sequences[chaine] = s
	if len(r.sequences) > 200 { // zapping : on ne garde pas les chaines abandonnees
		for k, v := range r.sequences {
			if now.Sub(v.depuis) > 10*time.Minute {
				delete(r.sequences, k)
			}
		}
	}
	return o, true
}

// memesSegments dit si deux playlists ont au moins un segment en commun.
func memesSegments(a, b map[string]struct{}) bool {
	for n := range b {
		if _, ok := a[n]; ok {
			return true
		}
	}
	return false
}

// nouvelleEpoque calcule la numerotation d'un flux redemarre : son premier segment prend
// le numero qui suit le dernier servi, precede d'une discontinuite, et les discontinuites
// de l'ancienne fenetre (toutes sorties d'un coup) se comptent dans DISCONTINUITY-SEQUENCE.
func (c continuite) nouvelleEpoque(prec suiviSequence, l lecturePlaylist) continuite {
	dernierServi := prec.sequence + c.decalage + int64(prec.segments) - 1
	premier := dernierServi + 1
	dsServie := prec.ds + c.decalageDS + int64(prec.discos)
	return continuite{
		decalage:   premier - l.sequence,
		decalageDS: dsServie - l.ds,
		disco:      premier,
		epoque:     c.epoque + 1,
	}
}

// epoque : numero de l'epoque courante d'une chaine (0 tant que le flux n'a pas redemarre).
func (r *Relais) epoque(chaine string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.sequences[chaine].epoque
}
