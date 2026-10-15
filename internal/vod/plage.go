package vod

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// plage est la plage d'octets demandee par le client (une seule).
type plage struct {
	presente bool
	debut    int64 // -1 pour un suffixe (« bytes=-500 »)
	fin      int64 // -1 si ouverte (« bytes=100- »)
	suffixe  int64
}

// analyserPlage lit un en-tete Range. Un en-tete absent, mal forme ou a plusieurs
// plages est ignore (RFC 9110 : le serveur peut repondre 200 avec tout le contenu) ;
// la reponse reste alors correcte, et les navigateurs n'en envoient pas.
func analyserPlage(h string) plage {
	h = strings.TrimSpace(h)
	spec, ok := strings.CutPrefix(h, "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return plage{}
	}
	a, b, ok := strings.Cut(spec, "-")
	if !ok {
		return plage{}
	}
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if a == "" { // suffixe
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 {
			return plage{}
		}
		return plage{presente: true, debut: -1, fin: -1, suffixe: n}
	}
	d, err := strconv.ParseInt(a, 10, 64)
	if err != nil || d < 0 {
		return plage{}
	}
	if b == "" {
		return plage{presente: true, debut: d, fin: -1}
	}
	f, err := strconv.ParseInt(b, 10, 64)
	if err != nil || f < d {
		return plage{}
	}
	return plage{presente: true, debut: d, fin: f}
}

// entete : la plage normalisee, telle qu'elle part chez le fournisseur.
func (p plage) entete() string {
	switch {
	case p.debut < 0:
		return fmt.Sprintf("bytes=-%d", p.suffixe)
	case p.fin < 0:
		return fmt.Sprintf("bytes=%d-", p.debut)
	}
	return fmt.Sprintf("bytes=%d-%d", p.debut, p.fin)
}

// contenuPlage : « bytes 100-199/1000 » ou « bytes 100-199/* ».
type contenuPlage struct{ debut, fin, total int64 } // total -1 si inconnu

func analyserContentRange(h string) (contenuPlage, bool) {
	spec, ok := strings.CutPrefix(strings.TrimSpace(h), "bytes ")
	if !ok {
		return contenuPlage{}, false
	}
	bornes, tot, ok := strings.Cut(spec, "/")
	if !ok {
		return contenuPlage{}, false
	}
	a, b, ok := strings.Cut(bornes, "-")
	if !ok {
		return contenuPlage{}, false
	}
	d, err1 := strconv.ParseInt(strings.TrimSpace(a), 10, 64)
	f, err2 := strconv.ParseInt(strings.TrimSpace(b), 10, 64)
	if err1 != nil || err2 != nil || d < 0 || f < d {
		return contenuPlage{}, false
	}
	total := int64(-1)
	if tot = strings.TrimSpace(tot); tot != "*" {
		t, err := strconv.ParseInt(tot, 10, 64)
		if err != nil || t <= f {
			return contenuPlage{}, false
		}
		total = t
	}
	return contenuPlage{debut: d, fin: f, total: total}, true
}

// coherente verifie que la reponse du fournisseur repond bien a la plage demandee.
func (c contenuPlage) coherente(p plage) bool {
	switch {
	case p.debut < 0: // suffixe : la fin du fichier
		return c.total >= 0 && c.fin == c.total-1 && c.debut == max(0, c.total-p.suffixe)
	case p.fin < 0:
		return c.debut == p.debut
	}
	return c.debut == p.debut && c.fin <= p.fin
}

// positionDemandee lit `?t=<secondes>` (seek d'un remux). Un flux fabrique a la
// volee n'a pas d'« octet N » : l'interface choisit l'instant, ffmpeg s'y place.
func positionDemandee(req *http.Request) (time.Duration, error) {
	v := req.URL.Query().Get("t")
	if v == "" {
		return 0, nil
	}
	sec, err := strconv.ParseFloat(v, 64)
	if err != nil || math.IsNaN(sec) || math.IsInf(sec, 0) || sec < 0 || sec > 72*3600 {
		return 0, fmt.Errorf("%w : parametre t", ErrDemandeInvalide)
	}
	return time.Duration(sec * float64(time.Second)), nil
}
