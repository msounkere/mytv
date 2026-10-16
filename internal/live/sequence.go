package live

import (
	"regexp"
	"strconv"
	"time"
)

var (
	reSequence = regexp.MustCompile(`(?m)^#EXT-X-MEDIA-SEQUENCE:\s*(\d+)`)
	reDuree    = regexp.MustCompile(`(?m)^#EXTINF:\s*([0-9.]+)`)
)

// suiviSequence retient, par chaine, le dernier numero de sequence vu dans la playlist
// du FOURNISSEUR et depuis quand il n'a plus bouge.
//
// POURQUOI : en recette reelle le lecteur se bloquait (tampon vide) apres 30 a 60 s sans
// aucune demande de segment, alors que les playlists etaient rechargees sans erreur. Sans
// ce suivi on ne sait pas dire si c'est le fournisseur qui n'a pas produit de nouveau
// segment, ou si le defaut est chez nous.
type suiviSequence struct {
	sequence int64
	depuis   time.Time // instant ou ce numero est apparu
}

// observation resume ce qu'on apprend d'un telechargement de playlist amont.
type observation struct {
	Sequence  int64
	Segments  int
	Dernier   float64 // duree moyenne d'un segment (s), 0 si inconnue
	Avance    bool    // la sequence a change depuis la playlist precedente
	ImmobileS float64 // secondes pendant lesquelles la sequence n'a pas bouge
	Connue    bool    // une playlist precedente existait
}

func lireSequence(corps string) (seq int64, segments int, moyenne float64, ok bool) {
	if m := reSequence.FindStringSubmatch(corps); m != nil {
		seq, _ = strconv.ParseInt(m[1], 10, 64)
		ok = true
	}
	var somme float64
	for _, m := range reDuree.FindAllStringSubmatch(corps, -1) {
		d, _ := strconv.ParseFloat(m[1], 64)
		somme += d
		segments++
	}
	if segments > 0 {
		moyenne = somme / float64(segments)
	}
	return
}

// observer met a jour le suivi de la chaine ; l'appelant tient r.mu.
func (r *Relais) observer(chaine, corps string, now time.Time) (observation, bool) {
	seq, segments, moyenne, ok := lireSequence(corps)
	if !ok {
		return observation{}, false
	}
	if r.sequences == nil {
		r.sequences = map[string]suiviSequence{}
	}
	prec, vu := r.sequences[chaine]
	o := observation{Sequence: seq, Segments: segments, Dernier: moyenne, Connue: vu}
	switch {
	case !vu || seq != prec.sequence:
		r.sequences[chaine] = suiviSequence{sequence: seq, depuis: now}
		o.Avance = vu
	default:
		o.ImmobileS = now.Sub(prec.depuis).Seconds()
	}
	if len(r.sequences) > 200 { // zapping : on ne garde pas les chaines abandonnees
		for k, v := range r.sequences {
			if now.Sub(v.depuis) > 10*time.Minute {
				delete(r.sequences, k)
			}
		}
	}
	return o, true
}
