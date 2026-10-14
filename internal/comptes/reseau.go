package comptes

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
)

// reseau decide d'ou vient reellement une requete. Deux usages : la limitation de
// tentatives (par adresse) et le compte technique local, qui n'est valable que
// depuis des plages reseau precises.
type reseau struct {
	plages []netip.Prefix // origines autorisees pour le compte technique
	proxys []netip.Prefix // pairs dont on croit X-Forwarded-For
}

func analyserListe(nom string, l []string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, e := range l {
		e = strings.TrimSpace(e)
		if p, err := netip.ParsePrefix(e); err == nil {
			out = append(out, p.Masked())
			continue
		}
		a, err := netip.ParseAddr(e)
		if err != nil {
			return nil, fmt.Errorf("%s : %q n'est ni une adresse ni une plage CIDR", nom, e)
		}
		a = a.Unmap()
		out = append(out, netip.PrefixFrom(a, a.BitLen()))
	}
	return out, nil
}

func analyserReseau(plages, proxys []string) (reseau, error) {
	var r reseau
	var err error
	if r.plages, err = analyserListe("comptes.plages_locales", plages); err != nil {
		return r, err
	}
	if r.proxys, err = analyserListe("comptes.proxys_fiables", proxys); err != nil {
		return r, err
	}
	return r, nil
}

func contient(l []netip.Prefix, a netip.Addr) bool {
	for _, p := range l {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// entetesRelais : en-tetes que seul un intermediaire ajoute.
var entetesRelais = []string{"X-Forwarded-For", "Forwarded", "X-Real-Ip", "Cf-Connecting-Ip", "True-Client-Ip", "X-Forwarded-Host"}

// origine renvoie l'adresse du client. suspect est vrai quand la requete porte des
// en-tetes d'intermediaire alors que le pair n'est PAS un proxy declare fiable :
// elle a probablement traverse un proxy mal declare, et son adresse apparente
// (celle du proxy, souvent 127.0.0.1) ne prouve rien.
func (r reseau) origine(req *http.Request) (adresse netip.Addr, suspect bool) {
	h, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		h = req.RemoteAddr
	}
	pair, err := netip.ParseAddr(h)
	if err != nil {
		return netip.Addr{}, true
	}
	pair = pair.Unmap()
	if !contient(r.proxys, pair) {
		for _, e := range entetesRelais {
			if req.Header.Get(e) != "" {
				return pair, true
			}
		}
		return pair, false
	}
	// Proxy fiable : on remonte X-Forwarded-For de droite a gauche jusqu'au
	// premier saut qui n'est pas un proxy fiable (les sauts de gauche sont
	// forgeables par le client).
	xff := strings.Join(req.Header.Values("X-Forwarded-For"), ",")
	if xff == "" {
		return pair, false
	}
	sauts := strings.Split(xff, ",")
	for i := len(sauts) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.TrimSpace(sauts[i]))
		if err != nil {
			return pair, true // en-tete illisible : on ne devine pas
		}
		a = a.Unmap()
		if !contient(r.proxys, a) {
			return a, false
		}
	}
	return pair, false
}

// adresseClient est l'adresse (texte) servant a la limitation de tentatives.
func (s *Service) adresseClient(req *http.Request) string {
	a, _ := s.reseau.origine(req)
	if !a.IsValid() {
		return "inconnue"
	}
	return a.String()
}

// locale dit si la requete peut utiliser le compte technique : plage configuree,
// jamais via un intermediaire douteux.
func (s *Service) locale(req *http.Request) bool {
	if s.cfg.LocalUtilisateur == "" || len(s.reseau.plages) == 0 {
		return false
	}
	a, suspect := s.reseau.origine(req)
	return !suspect && a.IsValid() && contient(s.reseau.plages, a)
}
