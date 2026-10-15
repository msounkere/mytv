// Package hotes decide a quelles adresses le serveur a le droit de parler quand c'est
// le FOURNISSEUR qui les designe (segments d'une playlist, redirections HTTP) : sans
// cela, un fournisseur compromis ferait atteindre au serveur un service interne (SSRF).
//
// Le controle a trois niveaux, du plus tot au plus sur :
//   - VerifierURL : l'adresse annoncee (hote litteral ou nom resolu) ;
//   - ControleRedirection : chaque saut de redirection HTTP est reverifie ;
//   - DialContext : l'adresse IP est controlee AU MOMENT DE LA CONNEXION, et c'est a
//     cette adresse controlee que l'on se connecte (pas de seconde resolution) : un DNS
//     malveillant qui repond une adresse publique a la verification puis une adresse
//     interne a la connexion (« rebinding ») ne passe pas.
package hotes

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// Filtre decide si une URL de segment a le droit d'etre relayee (SSRF).
//
// Les adresses des segments viennent d'une playlist du FOURNISSEUR : si elle est
// compromise ou fausse, elle peut viser 127.0.0.1, un service du reseau local ou
// le service de metadonnees d'un cloud (169.254.169.254). Sont donc refusees par
// defaut les adresses privees, loopback, de lien local, non specifiees, multicast
// et CGNAT. Passent : l'hote du fournisseur lui-meme, et ce que la configuration
// autorise explicitement.
type Filtre struct {
	noms     map[string]bool
	plages   []netip.Prefix
	resoudre Resolveur
}

// Resolveur traduit un nom en adresses (net.DefaultResolver par defaut ; faux dans les tests).
type Resolveur func(ctx context.Context, hote string) ([]netip.Addr, error)

var ErrRefuse = errors.New("hote refuse")

// interdites : plages non routables que netip ne classe pas toutes comme « privees ».
var interdites = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"), // CGNAT
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"), // NAT64 : cache n'importe quelle IPv4
}

// Nouveau construit le filtre : l'hote de urlFournisseur est de confiance, ainsi que
// les noms, adresses et plages CIDR de `autorises`. resoudre nil = DNS du systeme.
func Nouveau(urlFournisseur string, autorises []string, resoudre Resolveur) (*Filtre, error) {
	f := &Filtre{noms: map[string]bool{}, resoudre: resoudre}
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
			return nil, fmt.Errorf("hotes : hote autorise %q invalide", e)
		}
	}
	return f, nil
}

func (f *Filtre) Autorisee(a netip.Addr) bool {
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

// VerifierURL valide une adresse designee par le fournisseur : schema http(s), pas
// d'identifiants dans l'URL, hote autorise, et TOUTES les adresses auxquelles il se
// resout autorisees. C'est un controle PRECOCE (bonne erreur, pas de bail pris pour
// rien) ; il ne suffit pas seul : entre cette resolution et la connexion un DNS
// malveillant peut repondre autre chose (« rebinding »). Le controle qui compte est celui
// de DialContext.
func (f *Filtre) VerifierURL(ctx context.Context, cible string) error {
	u, err := url.Parse(cible)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil {
		return fmt.Errorf("%w : adresse inexploitable", ErrRefuse)
	}
	hote := strings.ToLower(u.Hostname())
	if a, err := netip.ParseAddr(hote); err == nil {
		if !f.Autorisee(a) && !f.noms[hote] {
			return fmt.Errorf("%w : %s", ErrRefuse, hote)
		}
		return nil
	}
	if f.noms[hote] {
		return nil
	}
	adr, err := f.resoudre(ctx, hote)
	if err != nil || len(adr) == 0 {
		return fmt.Errorf("%w : %s non resolu", ErrRefuse, hote)
	}
	for _, a := range adr {
		if !f.Autorisee(a) {
			return fmt.Errorf("%w : %s se resout vers une adresse non autorisee", ErrRefuse, hote)
		}
	}
	return nil
}

// ---------------------------------------------------------------- redirections et connexion

// ControleRedirection est un http.Client.CheckRedirect : au plus `max` sauts, chaque
// saut reverifie comme une adresse de segment, et aucun en-tete Referer.
//
// POURQUOI le Referer : le client de Go y recopie l'adresse de la requete precedente
// pour chaque redirection. Or l'adresse du fournisseur porte ses identifiants dans le
// chemin (`/live/<utilisateur>/<mot de passe>/...`) : ils partiraient, en clair, vers
// l'hote de la redirection.
//
// Le message d'erreur ne cite ni l'adresse de la redirection ni celle d'origine.
func (f *Filtre) ControleRedirection(max int) func(req *http.Request, via []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) > max {
			return fmt.Errorf("plus de %d redirections", max)
		}
		req.Header.Del("Referer")
		if err := f.VerifierURL(req.Context(), req.URL.String()); err != nil {
			return fmt.Errorf("%w : redirection vers un hote non autorise", ErrRefuse)
		}
		return nil
	}
}

// DialContext rend la fonction de connexion d'un http.Transport. Pour un hote de
// confiance (celui du fournisseur, ou liste par la configuration) : connexion
// ordinaire. Pour tout autre : le nom est resolu ICI, TOUTES les adresses sont
// controlees, puis on se connecte a l'une des adresses controlees — jamais a une
// seconde resolution du meme nom. Le net.Dialer controle en plus l'adresse reellement
// utilisee par la connexion (Control), filet si un chemin de code oubliait la verification.
func (f *Filtre) DialContext(delai time.Duration) func(ctx context.Context, network, addr string) (net.Conn, error) {
	confiance := &net.Dialer{Timeout: delai}
	strict := &net.Dialer{Timeout: delai, Control: f.controleConnexion}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		hote, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("%w : adresse inexploitable", ErrRefuse)
		}
		hote = strings.ToLower(hote)
		if f.noms[hote] {
			return confiance.DialContext(ctx, network, addr)
		}
		adr, err := f.adresses(ctx, hote)
		if err != nil {
			return nil, err
		}
		var derniere error
		for _, a := range adr {
			c, err := strict.DialContext(ctx, network, net.JoinHostPort(a.String(), port))
			if err == nil {
				return c, nil
			}
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			derniere = err
		}
		return nil, derniere
	}
}

// adresses resout un hote (ou lit l'adresse litterale) et refuse si UNE adresse est interdite.
func (f *Filtre) adresses(ctx context.Context, hote string) ([]netip.Addr, error) {
	if a, err := netip.ParseAddr(hote); err == nil {
		a = a.Unmap()
		if !f.Autorisee(a) {
			return nil, fmt.Errorf("%w : adresse non autorisee", ErrRefuse)
		}
		return []netip.Addr{a}, nil
	}
	adr, err := f.resoudre(ctx, hote)
	if err != nil || len(adr) == 0 {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w : nom non resolu", ErrRefuse)
	}
	out := make([]netip.Addr, 0, len(adr))
	for _, a := range adr {
		a = a.Unmap()
		if !f.Autorisee(a) {
			return nil, fmt.Errorf("%w : le nom se resout vers une adresse non autorisee", ErrRefuse)
		}
		out = append(out, a)
	}
	return out, nil
}

// controleConnexion est net.Dialer.Control : appele avec l'adresse IP finale, juste
// avant la connexion.
func (f *Filtre) controleConnexion(_, adresse string, _ syscall.RawConn) error {
	hote, _, err := net.SplitHostPort(adresse)
	if err != nil {
		return fmt.Errorf("%w : adresse inexploitable", ErrRefuse)
	}
	a, err := netip.ParseAddr(hote)
	if err != nil || !f.Autorisee(a.Unmap()) {
		return fmt.Errorf("%w : adresse non autorisee a la connexion", ErrRefuse)
	}
	return nil
}
