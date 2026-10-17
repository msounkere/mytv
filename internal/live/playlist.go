package live

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"mytv/internal/domain"
)

type playlist struct {
	corps string
	a     time.Time
	// sonde : adresse du segment (ou de la variante) que la sonde du direct lira. Vide si la
	// playlist n'annonce rien d'exploitable.
	sonde string
}

// appel est une telechargement de playlist partage par toutes les demandes
// simultanees d'une meme chaine.
type appel struct {
	fini        chan struct{}
	annuler     context.CancelFunc
	pl          playlist
	err         error
	attentes    int
	renouvelers []func()
}

func (r *Relais) prefixeSegments(chaine string) string {
	return r.opt.Prefixe + "/" + url.PathEscape(chaine) + "/seg"
}

// telecharger recupere la playlist d'une chaine, avec nouveaux essais et patience.
func (r *Relais) telecharger(ctx context.Context, chaine string, renouveler func()) (string, *url.URL, error) {
	cible := r.opt.Fournisseur.URLLive(chaine)
	var corps string
	var base *url.URL
	err := r.avecEssais(ctx, chaine, r.opt.Reseau.EssaisPlaylist, renouveler, func(ctx context.Context) error {
		debut := time.Now()
		c, b, err := r.unePlaylist(ctx, cible)
		r.st.noterPlaylist(time.Since(debut))
		r.inst.Observer("live_duree_amont_secondes", time.Since(debut).Seconds(), "type", "playlist")
		if err != nil {
			r.st.incrementer(&r.st.echecs)
			r.inst.Compter("live_amont_echecs_total", 1, "type", "playlist")
			// Jamais l'URL (elle porte les identifiants) : seulement la chaine.
			r.journal.Warn("playlist echec", "chaine", chaine, "ms", time.Since(debut).Milliseconds(), "erreur", err)
			return err
		}
		corps, base = c, b
		r.opt.Occupation.Succes(r.demande(ctx, chaine))
		r.journal.Info("playlist", "chaine", chaine, "ms", time.Since(debut).Milliseconds())
		return nil
	})
	return corps, base, err
}

func (r *Relais) unePlaylist(ctx context.Context, cible string) (string, *url.URL, error) {
	rep, fin, err := r.requete(ctx, cible, nil, r.opt.Reseau.DelaiPlaylist.Std())
	if err != nil {
		return "", nil, err
	}
	defer fin()
	defer rep.Body.Close()
	corps, err := io.ReadAll(io.LimitReader(rep.Body, int64(r.opt.Live.TaillePlaylistMax)))
	if err != nil {
		return "", nil, fmt.Errorf("%w : lecture de la playlist : %v", domain.ErrAmont, err)
	}
	if rep.StatusCode != http.StatusOK || !strings.Contains(string(corps), "#EXTM3U") {
		return "", nil, fmt.Errorf("%w : playlist refusee (HTTP %d, %d octets)", domain.ErrAmont, rep.StatusCode, len(corps))
	}
	// Apres redirections : sert a resoudre les URL relatives.
	base, _ := url.Parse(cible)
	if rep.Request != nil && rep.Request.URL != nil {
		base = rep.Request.URL
	}
	return string(corps), base, nil
}

// playlistDe regroupe les demandes simultanees d'une meme chaine en une seule
// requete amont. `renouveler` prolonge le bail du demandeur pendant l'attente.
func (r *Relais) playlistDe(ctx context.Context, chaine string, renouveler func()) (playlist, bool, error) {
	r.mu.Lock()
	if p, ok := r.dernier[chaine]; ok && r.opt.Horloge().Sub(p.a) < r.opt.Live.FraicheurPlaylist.Std() {
		r.mu.Unlock()
		return p, false, nil
	}
	a, partage := r.enCours[chaine]
	if !partage {
		a = &appel{fini: make(chan struct{})}
		// L'appel amont survit a l'abandon du client qui l'a declenche (les autres en
		// dependent) mais pas au dernier abandon, ni a l'arret du relais.
		ctxAppel, annuler := context.WithTimeout(r.base, r.dureeMaxPlaylist())
		// La patience se compte depuis l'arrivee de la requete qui declenche l'appel.
		if d, ok := ctx.Value(cleDebut{}).(time.Time); ok {
			ctxAppel = context.WithValue(ctxAppel, cleDebut{}, d)
		}
		// La session qui declenche l'appel sert au diagnostic d'occupation.
		if sess, ok := ctx.Value(cleSession{}).(string); ok {
			ctxAppel = context.WithValue(ctxAppel, cleSession{}, sess)
		}
		a.annuler = annuler
		r.enCours[chaine] = a
		r.taches.Add(1)
		go r.lancer(ctxAppel, chaine, a)
	}
	a.attentes++
	a.renouvelers = append(a.renouvelers, renouveler)
	r.mu.Unlock()

	select {
	case <-a.fini:
		r.quitter(chaine, a, false)
		return r.resultat(chaine, a)
	case <-ctx.Done():
		r.quitter(chaine, a, true)
		return playlist{}, false, ctx.Err()
	}
}

// dureeMaxPlaylist borne un appel partage : tous les essais, plus la patience.
func (r *Relais) dureeMaxPlaylist() time.Duration {
	n := r.opt.Reseau
	return time.Duration(n.EssaisPlaylist)*(n.DelaiPlaylist.Std()+n.AttenteEssais.Std()) +
		r.opt.Bail.PatienceMax.Std() + max(r.opt.Bail.Reessai.Std(), r.opt.Bail.ReessaiMax.Std())
}

// quitter retire un demandeur ; le dernier a partir annule l'appel amont devenu inutile.
func (r *Relais) quitter(chaine string, a *appel, abandon bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a.attentes--
	if abandon && a.attentes == 0 {
		a.annuler()
		if r.enCours[chaine] == a {
			delete(r.enCours, chaine) // une nouvelle demande repart d'un appel neuf
		}
	}
}

func (r *Relais) lancer(ctx context.Context, chaine string, a *appel) {
	defer r.taches.Done()
	defer a.annuler()
	renouveler := func() {
		r.mu.Lock()
		fs := append([]func(){}, a.renouvelers...)
		r.mu.Unlock()
		for _, f := range fs {
			f()
		}
	}
	corps, base, err := r.telecharger(ctx, chaine, renouveler)
	r.mu.Lock()
	defer r.mu.Unlock()
	if err == nil {
		now := r.opt.Horloge()
		a.pl = playlist{corps: r.hls.Reecrire(r.prefixeSegments(chaine), chaine, base, corps), a: now,
			sonde: analyserPlaylist(corps, base).urlDeSonde()}
		if o, ok := r.observer(chaine, corps, now); ok {
			r.signalerSequence(chaine, o)
		}
		r.dernier[chaine] = a.pl
		r.purgerSecours(now)
	}
	a.err = err
	if r.enCours[chaine] == a {
		delete(r.enCours, chaine)
	}
	close(a.fini)
}

// signalerSequence journalise l'avancement de la playlist du fournisseur : un direct sain
// avance d'un segment toutes les duree-de-segment secondes. Immobile depuis plus de deux
// durees de segment, c'est le FOURNISSEUR qui ne produit plus (le lecteur finira par
// manquer de tampon) : avertissement, avec le numero de sequence pour le prouver.
func (r *Relais) signalerSequence(chaine string, o observation) {
	r.inst.Compter("live_playlists_amont_total", 1, "avance", strconv.FormatBool(o.Avance))
	seuil := 2 * o.Dernier
	if seuil <= 0 {
		seuil = 25
	}
	if o.ImmobileS > seuil {
		r.inst.Compter("live_playlists_immobiles_total", 1)
		r.journal.Warn("playlist du fournisseur immobile", "chaine", chaine, "sequence", o.Sequence,
			"segments", o.Segments, "immobile_s", int(o.ImmobileS))
		return
	}
	r.journal.Debug("playlist du fournisseur", "chaine", chaine, "sequence", o.Sequence, "segments", o.Segments, "avance", o.Avance)
}

// purgerSecours oublie les playlists trop vieilles pour servir de secours, afin que
// la table ne grossisse pas avec le nombre de chaines zappees.
func (r *Relais) purgerSecours(now time.Time) {
	for k, p := range r.dernier {
		if now.Sub(p.a) > r.opt.Live.PeremptionSecours.Std() {
			delete(r.dernier, k)
		}
	}
}

// resultat applique le secours : mieux vaut la derniere bonne playlist qu'un trou.
func (r *Relais) resultat(chaine string, a *appel) (playlist, bool, error) {
	if a.err == nil {
		return a.pl, false, nil
	}
	r.mu.Lock()
	p, ok := r.dernier[chaine]
	r.mu.Unlock()
	if age := r.opt.Horloge().Sub(p.a); ok && age < r.opt.Live.PeremptionSecours.Std() {
		r.st.incrementer(&r.st.secours)
		r.inst.Compter("live_playlists_total", 1, "resultat", "secours")
		r.journal.Warn("playlist de secours servie", "chaine", chaine, "age_s", int(age.Seconds()), "cause", a.err)
		return p, true, nil
	}
	return playlist{}, false, a.err
}
