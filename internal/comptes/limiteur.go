package comptes

import (
	"net/netip"
	"sync"
	"time"
)

// limiteur freine la devinette de mots de passe, PAR COMPTE et PAR ADRESSE : le
// premier arrete un attaquant qui change d'adresse, le second celui qui essaie
// beaucoup de comptes depuis la meme machine.
//
// Le delai CROIT : le premier blocage dure une fenetre, chaque blocage suivant
// (sans accalmie) double, jusqu'a 8 fenetres.
//
// Une tentative est comptee AVANT la verification du mot de passe (tenter), puis
// annulee si elle reussit : sinon une rafale de requetes paralleles passerait
// toutes la barriere avant que la premiere n'ait echoue.
type limiteur struct {
	mu      sync.Mutex
	seuil   int
	fenetre time.Duration
	horloge func() time.Time
	m       map[string]*etatLimite
}

type etatLimite struct {
	essais  []time.Time
	bloque  time.Time // blocage actif jusqu'a
	paliers int
	dernier time.Time
}

const (
	paliersMax    = 3 // 2^3 = 8 fenetres
	entreesMax    = 100_000
	oubliFenetres = 8
)

func nouveauLimiteur(seuil int, fenetre time.Duration, horloge func() time.Time) *limiteur {
	return &limiteur{seuil: seuil, fenetre: fenetre, horloge: horloge, m: map[string]*etatLimite{}}
}

// tenter enregistre une tentative sur chaque cle. Faux (et rien d'enregistre) si
// l'une des cles est bloquee.
func (l *limiteur) tenter(cles ...string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.horloge()
	l.purger(now)
	for _, c := range cles {
		if e := l.m[c]; e != nil && now.Before(e.bloque) {
			return false
		}
	}
	for _, c := range cles {
		e := l.m[c]
		if e == nil {
			e = &etatLimite{}
			l.m[c] = e
		}
		if now.Sub(e.dernier) > oubliFenetres*l.fenetre {
			e.paliers = 0
		}
		e.dernier = now
		e.essais = rognerEssais(e.essais, now.Add(-l.fenetre))
		e.essais = append(e.essais, now)
		if len(e.essais) >= l.seuil {
			// Le seuil est atteint : cette tentative passe, la suivante sera refusee.
			d := l.fenetre << min(e.paliers, paliersMax)
			e.bloque = now.Add(d)
			e.paliers++
			e.essais = e.essais[:0]
		}
	}
	return true
}

// reussir annule la tentative provisoire : le compte repart a zero (son proprietaire
// vient de prouver qu'il connait le mot de passe), l'adresse reprend seulement la
// tentative (un attaquant ne doit pas remettre son compteur a zero avec un compte
// a lui).
func (l *limiteur) reussir(cleCompte, cleAdresse string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.m, cleCompte)
	if e := l.m[cleAdresse]; e != nil && len(e.essais) > 0 {
		e.essais = e.essais[:len(e.essais)-1]
	}
}

func rognerEssais(v []time.Time, depuis time.Time) []time.Time {
	i := 0
	for i < len(v) && v[i].Before(depuis) {
		i++
	}
	return v[i:]
}

// purger oublie les entrees sans blocage actif ni activite recente ; si la table
// reste trop grosse (inondation d'adresses), elle abandonne les moins utiles.
func (l *limiteur) purger(now time.Time) {
	if len(l.m) < 1024 {
		return
	}
	for c, e := range l.m {
		if !now.Before(e.bloque) && now.Sub(e.dernier) > oubliFenetres*l.fenetre {
			delete(l.m, c)
		}
	}
	for c, e := range l.m {
		if len(l.m) < entreesMax {
			break
		}
		if !now.Before(e.bloque) {
			delete(l.m, c)
		}
	}
}

// cleAdresse regroupe une adresse IPv6 par /64 : un client IPv6 dispose de 2^64
// adresses, la limitation par adresse exacte n'arreterait personne.
func cleAdresse(adresse string) string {
	a, err := netip.ParseAddr(adresse)
	if err != nil {
		return "a:" + adresse
	}
	a = a.Unmap()
	if a.Is6() {
		p, _ := a.Prefix(64)
		return "a:" + p.String()
	}
	return "a:" + a.String()
}
