package live

import (
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// segmentAmont est un segment annonce par la playlist du fournisseur. Le nom (sans le
// hash de chemin que le fournisseur change a chaque playlist) l'identifie d'une playlist a l'autre.
type segmentAmont struct {
	url string
	nom string
}

// analyse est ce que le transcodage et la sonde retiennent d'une playlist du fournisseur.
type analyse struct {
	segments []segmentAmont
	variante string        // playlist maitresse : adresse de la premiere variante
	cible    time.Duration // EXT-X-TARGETDURATION, 0 si absent
	fin      bool          // EXT-X-ENDLIST : plus de nouveaux segments
}

// analyserPlaylist lit une playlist (media ou maitresse). Les adresses relatives sont
// resolues contre `base` (adresse APRES redirections) ; tout ce qui n'est pas http(s) est ignore.
func analyserPlaylist(corps string, base *url.URL) analyse {
	var a analyse
	prochaineEstVariante := false
	for _, l := range strings.Split(corps, "\n") {
		l = strings.TrimSpace(l)
		switch {
		case l == "":
		case strings.HasPrefix(l, "#EXT-X-STREAM-INF"):
			prochaineEstVariante = true
		case strings.HasPrefix(l, "#EXT-X-TARGETDURATION:"):
			if n, err := strconv.Atoi(strings.TrimPrefix(l, "#EXT-X-TARGETDURATION:")); err == nil && n > 0 && n < 600 {
				a.cible = time.Duration(n) * time.Second
			}
		case strings.HasPrefix(l, "#EXT-X-ENDLIST"):
			a.fin = true
		case strings.HasPrefix(l, "#"):
		default:
			ref, err := url.Parse(l)
			if err != nil {
				continue
			}
			u := base.ResolveReference(ref)
			if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				continue
			}
			if prochaineEstVariante {
				prochaineEstVariante = false
				if a.variante == "" {
					a.variante = u.String()
				}
				continue
			}
			a.segments = append(a.segments, segmentAmont{url: u.String(), nom: path.Base(u.Path)})
		}
	}
	return a
}

// urlDeSonde est l'adresse que la sonde ira lire : le DERNIER segment (le plus recent, donc
// le plus sur d'exister encore) ou, pour une playlist maitresse, sa premiere variante.
func (a analyse) urlDeSonde() string {
	if n := len(a.segments); n > 0 {
		return a.segments[n-1].url
	}
	return a.variante
}
