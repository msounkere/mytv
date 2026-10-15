package live

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"
)

// filtreHotes decide si une URL de segment a le droit d'etre relayee (SSRF).
//
// Les adresses des segments viennent d'une playlist du FOURNISSEUR : si elle est
// compromise ou fausse, elle peut viser 127.0.0.1, un service du reseau local ou
// le service de metadonnees d'un cloud (169.254.169.254). Sont donc refusees par
// defaut les adresses privees, loopback, de lien local, non specifiees, multicast
// et CGNAT. Passent : l'hote du fournisseur lui-meme, et ce que la configuration
// autorise explicitement.
type filtreHotes struct {
	noms     map[string]bool
	plages   []netip.Prefix
	resoudre func(ctx context.Context, hote string) ([]netip.Addr, error)
}

var errHoteRefuse = errors.New("hote refuse")

// interdites : plages non routables que netip ne classe pas toutes comme « privees ».
var interdites = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // CGNAT
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64 : cache n'importe quelle IPv4
}

func nouveauFiltreHotes(urlFournisseur string, autorises []string, resoudre func(context.Context, string) ([]netip.Addr, error)) (*filtreHotes, error) {
	f := &filtreHotes{noms: map[string]bool{}, resoudre: resoudre}
	if f.resoudre == nil {
		f.resoudre = func(ctx context.Context, hote string) ([]netip.Addr, error) {
			return net.DefaultResolver.LookupNetIP(ctx, "ip", hote)
		}
	}
	// L'hote du fournisseur est de confiance : c'est lui qu'on est venu chercher.
	if u, err := url.Parse(urlFournisseur); err == nil && u.Hostname() != "" {
		f.noms[strings.ToLower(u.Hostname())] = true
	}
	for _, e := range autorises {
		e = strings.TrimSpace(e)
		if p, err := netip.ParsePrefix(e); err == nil {
			f.plages = append(f.plages, p.Masked())
		} else if a, err := netip.ParseAddr(e); err == nil {
			a = a.Unmap()
			f.plages = append(f.plages, netip.PrefixFrom(a, a.BitLen()))
		} else if e != "" && !strings.ContainsAny(e, "/ ") {
			f.noms[strings.ToLower(e)] = true
		} else {
			return nil, fmt.Errorf("live : hote autorise %q invalide", e)
		}
	}
	return f, nil
}

func (f *filtreHotes) autorisee(a netip.Addr) bool {
	a = a.Unmap()
	for _, p := range f.plages {
		if p.Contains(a) {
			return true
		}
	}
	if a.IsPrivate() || a.IsLoopback() || a.IsLinkLocalUnicast() || a.IsLinkLocalMulticast() ||
		a.IsInterfaceLocalMulticast() || a.IsMulticast() || a.IsUnspecified() {
		return false
	}
	for _, p := range interdites {
		if p.Contains(a) {
			return false
		}
	}
	return true
}

// verifier valide l'URL d'un segment : schema http(s), pas d'identifiants dans
// l'URL, hote autorise, et TOUTES les adresses auxquelles il se resout autorisees.
// (Limite connue : entre cette resolution et la connexion, un DNS malveillant peut
// repondre autre chose — « rebinding » ; la liste blanche de noms l'evite.)
func (f *filtreHotes) verifier(ctx context.Context, cible string) error {
	u, err := url.Parse(cible)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return fmt.Errorf("%w : adresse inexploitable", errHoteRefuse)
	}
	hote := strings.ToLower(u.Hostname())
	if a, err := netip.ParseAddr(hote); err == nil {
		if !f.autorisee(a) && !f.noms[hote] {
			return fmt.Errorf("%w : %s", errHoteRefuse, hote)
		}
		return nil
	}
	if f.noms[hote] {
		return nil
	}
	adr, err := f.resoudre(ctx, hote)
	if err != nil || len(adr) == 0 {
		return fmt.Errorf("%w : %s non resolu", errHoteRefuse, hote)
	}
	for _, a := range adr {
		if !f.autorisee(a) {
			return fmt.Errorf("%w : %s se resout vers une adresse non autorisee", errHoteRefuse, hote)
		}
	}
	return nil
}
