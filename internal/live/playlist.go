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
	"mytv/internal/remux"
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

// noeudRetenu est l'adresse de la playlist d'une chaine sur son noeud de diffusion, apres le
// 302 du serveur central, et l'instant ou elle a ete apprise.
type noeudRetenu struct {
	adresse string
	depuis  time.Time
}

// telecharger recupere la playlist d'une chaine : d'abord sur le noeud de diffusion retenu
// (un seul essai, sans patience), sinon a l'adresse canonique avec nouveaux essais et patience.
//
// POURQUOI retenir le noeud : le fournisseur repond 302 du serveur central vers un noeud de
// diffusion. Redemander l'adresse canonique a chaque playlist repassait par ce serveur central
// a chaque fois : 0,5 s de traitement par requete, et en soiree des delais de 4 s en serie
// (13 d'affilee mesures le 25/10 a 21:30, puis « playlist indisponible »). Un lecteur TS
// continu, lui, ne parle au central qu'une fois. Le noeud, lui, n'a aucune garantie : il meurt
// ou change ; tout echec le fait oublier tout de suite, et l'adresse canonique reprend avec
// les essais et la patience habituels (comme vod.sourceAmont pour les films).
func (r *Relais) telecharger(ctx context.Context, chaine string, renouveler func()) (string, *url.URL, error) {
	if corps, base, ok := r.depuisNoeud(ctx, chaine); ok {
		return corps, base, nil
	}
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
	if err == nil {
		r.retenirNoeud(chaine, cible, base)
	}
	return corps, base, err
}

// depuisNoeud tente la playlist sur le noeud retenu de la chaine. Un seul essai : ni nouveaux
// essais ni patience 407, qui restent a l'adresse canonique. Tout echec (delai, 5xx, 401/403/404,
// 407, corps sans #EXTM3U) oublie le noeud et compte un repli ; l'appelant repart au central.
// Le client parti pendant l'essai n'est pas un echec du noeud.
func (r *Relais) depuisNoeud(ctx context.Context, chaine string) (string, *url.URL, bool) {
	adresse, ok := r.noeudRetenu(chaine)
	if !ok {
		return "", nil, false
	}
	debut := time.Now()
	corps, base, err := r.unePlaylist(ctx, adresse)
	r.st.noterPlaylist(time.Since(debut))
	r.inst.Observer("live_duree_amont_secondes", time.Since(debut).Seconds(), "type", "playlist")
	if err == nil {
		r.st.incrementer(&r.st.noeud)
		r.inst.Compter("live_playlists_noeud_total", 1, "resultat", "ok")
		r.opt.Occupation.Succes(r.demande(ctx, chaine))
		r.journal.Info("playlist", "chaine", chaine, "ms", time.Since(debut).Milliseconds(), "noeud", true)
		return corps, base, true
	}
	if ctx.Err() != nil {
		return "", nil, false
	}
	r.oublierNoeud(chaine)
	r.st.incrementer(&r.st.replis)
	r.inst.Compter("live_playlists_noeud_total", 1, "resultat", "repli")
	// Jamais l'adresse du noeud (elle porte les identifiants) : la chaine et la cause.
	r.journal.Info("playlist : repli sur le serveur central", "chaine", chaine, "ms", time.Since(debut).Milliseconds(),
		"cause", remux.Assainir(err.Error()))
	return "", nil, false
}

// noeudRetenu rend l'adresse retenue d'une chaine si elle a moins de live.retention_noeud.
func (r *Relais) noeudRetenu(chaine string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, ok := r.noeuds[chaine]
	if !ok {
		return "", false
	}
	if r.opt.Horloge().Sub(n.depuis) >= r.opt.Live.RetentionNoeud.Std() {
		delete(r.noeuds, chaine)
		return "", false
	}
	return n.adresse, true
}

// retenirNoeud garde l'adresse finale d'une playlist quand elle differe de l'adresse canonique
// (le central a redirige). Une retention nulle ou negative desactive tout.
func (r *Relais) retenirNoeud(chaine, canonique string, base *url.URL) {
	if base == nil || r.opt.Live.RetentionNoeud <= 0 {
		return
	}
	if c, err := url.Parse(canonique); err != nil || (c.Scheme == base.Scheme && c.Host == base.Host && c.Path == base.Path) {
		return
	}
	now := r.opt.Horloge()
	r.mu.Lock()
	defer r.mu.Unlock()
	r.noeuds[chaine] = noeudRetenu{adresse: base.String(), depuis: now}
	if len(r.noeuds) > 200 { // zapping : les chaines abandonnees ne s'accumulent pas
		for k, n := range r.noeuds {
			if now.Sub(n.depuis) >= r.opt.Live.RetentionNoeud.Std() {
				delete(r.noeuds, k)
			}
		}
	}
}

func (r *Relais) oublierNoeud(chaine string) {
	r.mu.Lock()
	delete(r.noeuds, chaine)
	r.mu.Unlock()
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
		servi := r.hls.Reecrire(r.prefixeSegments(chaine), chaine, base, corps)
		if o, ok := r.observer(chaine, corps, now); ok {
			// Numerotation continue d'une epoque a l'autre du flux amont (continuite.go).
			servi = appliquerContinuite(servi, r.sequences[chaine])
			r.signalerSequence(chaine, o)
		}
		a.pl = playlist{corps: servi, a: now, sonde: analyserPlaylist(corps, base).urlDeSonde()}
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
	if o.Recul {
		// Le fournisseur a redemarre son flux : le lecteur recoit une discontinuite et une
		// numerotation qui continue (voir continuite.go), pas un retour a zero.
		r.inst.Compter("live_redemarrages_amont_total", 1)
		r.journal.Warn("flux du fournisseur redemarre", "chaine", chaine, "sequence", o.Sequence,
			"segments", o.Segments, "epoque", r.sequences[chaine].epoque)
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
