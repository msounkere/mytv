package live

import (
	"strconv"
	"strings"
)

// appliquerContinuite reecrit la numerotation d'une playlist servie d'apres le suivi de la
// chaine : MEDIA-SEQUENCE decalee (jamais decroissante d'une epoque a l'autre),
// DISCONTINUITY-SEQUENCE cumulee, et un tag EXT-X-DISCONTINUITY devant le premier segment
// d'un flux redemarre tant qu'il est dans la fenetre.
//
// POURQUOI un tag plutot qu'une simple renumerotation : les horodatages du flux redemarre
// repartent de zero (et son codec peut changer). La RFC 8216 (4.3.2.3) exige alors
// EXT-X-DISCONTINUITY ; hls.js s'en sert pour remettre son demultiplexeur a zero et
// coller les nouveaux segments a la suite des anciens, au lieu de les placer a 0.
func appliquerContinuite(corps string, s suiviSequence) string {
	if s.decalage == 0 && s.decalageDS == 0 && s.disco < 0 {
		return corps
	}
	lignes := strings.Split(corps, "\n")
	var sortie strings.Builder
	sortie.Grow(len(corps) + 64)
	dsEcrite := false
	index := int64(-1) // numero servi du prochain segment rencontre
	for _, l := range lignes {
		switch {
		case strings.HasPrefix(l, "#EXT-X-MEDIA-SEQUENCE:"):
			n, _ := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(l, "#EXT-X-MEDIA-SEQUENCE:")), 10, 64)
			index = n + s.decalage
			sortie.WriteString("#EXT-X-MEDIA-SEQUENCE:" + strconv.FormatInt(index, 10) + "\n")
			if !dsEcrite && s.decalageDS != 0 && !strings.Contains(corps, "#EXT-X-DISCONTINUITY-SEQUENCE:") {
				sortie.WriteString("#EXT-X-DISCONTINUITY-SEQUENCE:" + strconv.FormatInt(s.decalageDS, 10) + "\n")
				dsEcrite = true
			}
			continue
		case strings.HasPrefix(l, "#EXT-X-DISCONTINUITY-SEQUENCE:"):
			n, _ := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(l, "#EXT-X-DISCONTINUITY-SEQUENCE:")), 10, 64)
			sortie.WriteString("#EXT-X-DISCONTINUITY-SEQUENCE:" + strconv.FormatInt(n+s.decalageDS, 10) + "\n")
			dsEcrite = true
			continue
		case strings.HasPrefix(l, "#EXTINF"):
			if index >= 0 && index == s.disco {
				sortie.WriteString("#EXT-X-DISCONTINUITY\n")
			}
		case l != "" && !strings.HasPrefix(l, "#"):
			if index >= 0 {
				index++
			}
		}
		sortie.WriteString(l)
		sortie.WriteString("\n")
	}
	return strings.TrimSuffix(sortie.String(), "\n") + "\n"
}
