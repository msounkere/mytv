package live

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mytv/internal/domain"
	"mytv/internal/hls"
	"mytv/internal/remux"
)

// LanceurDirect demarre ffmpeg pour une chaine (remux.Lanceur en production).
type LanceurDirect interface {
	LancerDirect(ctx context.Context, t remux.TacheDirect) (domain.Processus, error)
}

// ErrTranscodageImpossible : ffmpeg n'a pas pu produire de quoi demarrer la lecture. Code
// d'API stable : transcodage_impossible (502).
var ErrTranscodageImpossible = errors.New("live : transcodage impossible")

// echecsSegmentsMax : segments consecutifs perdus avant d'abandonner le transcodage.
const echecsSegmentsMax = 3

// pasSurveillanceMin et sondageDossier : cadence des controles internes (inactivite,
// bail) et de l'attente des premiers segments. Elles ne sont pas des reglages : seul
// leur ordre de grandeur compte.
const sondageDossier = 50 * time.Millisecond

var reNomSegmentLocal = regexp.MustCompile(`^[a-z0-9]{4,16}_[0-9]{5}\.ts$`)

// reDossierTemporaire reconnait les dossiers que NOUS creons (chaine-12 hexa) : le
// nettoyage au demarrage n'efface jamais autre chose, meme si dossier_hls est mal configure.
var reDossierTemporaire = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}-[0-9a-f]{12}$`)

// transcodage est UN ffmpeg qui convertit une chaine, avec son dossier.
type transcodage struct {
	chaine, session string
	mode            Mode
	eco             bool // mode economie (US-38) : un changement de qualite relance ffmpeg
	dossier         string
	prefixe         string
	proc            domain.Processus
	pr              *io.PipeReader
	annule          context.CancelFunc
	ctx             context.Context
	mort            chan struct{} // ferme quand ffmpeg a fini (de lui-meme ou tue)
	fini            chan struct{} // ferme quand tout est nettoye (processus recolte, dossier supprime)
	arret           sync.Once
	pret            atomic.Bool  // premiers segments produits : l'inactivite ne se compte qu'a partir de la
	vue             atomic.Int64 // derniere requete du lecteur (unix nano, horloge du relais)
	bail            domain.Bail
}

func (t *transcodage) vivant() bool {
	select {
	case <-t.mort:
		return false
	default:
		return true
	}
}

func (r *Relais) toucher(t *transcodage) { t.vue.Store(r.opt.Horloge().UnixNano()) }

// demarrage est un lancement en cours, partage par les requetes simultanees d'une meme
// chaine : il survit a l'abandon d'un lecteur impatient (hls.js annule et relance un
// manifeste trop lent, ce qui ne ferait jamais demarrer ffmpeg) mais pas au dernier.
type demarrage struct {
	chaine, session string
	mode            Mode
	eco             bool
	annule          context.CancelFunc
	fini            chan struct{}
	attentes        int
	t               *transcodage
	err             error
	bail            domain.Bail
}

type conversions struct {
	mu        sync.Mutex
	courant   *transcodage
	demarrage *demarrage
	sceau     *hls.Sceau
	dossier   string // absolu
}

// preparerConversions supprime les orphelins d'un arret brutal et
// derive la cle des jetons de segments locaux (un domaine distinct des jetons du direct).
func preparerConversions(opt Options) (*conversions, error) {
	dossier, err := filepath.Abs(opt.Live.DossierHLS)
	if err != nil {
		return nil, fmt.Errorf("live : dossier_hls : %w", err)
	}
	// Le dossier n'est cree qu'au premier transcodage : un service qui ne convertit jamais ne
	// laisse rien sur le disque.
	if entrees, err := os.ReadDir(dossier); err == nil {
		for _, e := range entrees {
			// Lstat via Type : un lien symbolique n'est jamais suivi ni supprime.
			if e.Type().IsDir() && reDossierTemporaire.MatchString(e.Name()) {
				_ = os.RemoveAll(filepath.Join(dossier, e.Name()))
			}
		}
	}
	sceau, err := hls.NouveauSceau(opt.Secret, "mytv/live/transcodage/chiffrement/v1", "mytv/live/transcodage/nonce/v1")
	if err != nil {
		return nil, err
	}
	return &conversions{sceau: sceau, dossier: dossier}, nil
}

func (r *Relais) conversionActive() bool { return r.opt.Lanceur != nil && r.tc != nil }

// cibleEco : la cible du mode economie, d'apres live.eco.*.
func (r *Relais) cibleEco() remux.CibleEco {
	return remux.CibleEco{Hauteur: r.opt.Live.Eco.Hauteur, DebitKbps: r.opt.Live.Eco.DebitKbps}
}

func (r *Relais) transcodageActifDe(chaine string) bool {
	if r.tc == nil {
		return false
	}
	r.tc.mu.Lock()
	defer r.tc.mu.Unlock()
	return r.tc.courant != nil && r.tc.courant.chaine == chaine && r.tc.courant.vivant()
}

func (r *Relais) transcodageDe(chaine string) *transcodage {
	if r.tc == nil {
		return nil
	}
	r.tc.mu.Lock()
	defer r.tc.mu.Unlock()
	if r.tc.courant != nil && r.tc.courant.chaine == chaine {
		return r.tc.courant
	}
	return nil
}

// zapper est appele a chaque playlist, AVANT tout contact avec le fournisseur : il n'a
// qu'une connexion, donc l'ancienne chaine (sonde, lancement ou ffmpeg) doit avoir fini
// de lui parler quand la nouvelle commence.
func (r *Relais) zapper(ctx context.Context, chaine string) {
	r.annulerSondesSauf(chaine)
	if r.tc == nil {
		return
	}
	r.tc.mu.Lock()
	d := r.tc.demarrage
	if d != nil && d.chaine != chaine {
		d.annule()
	} else {
		d = nil
	}
	r.tc.mu.Unlock()
	if d != nil {
		select {
		case <-d.fini:
		case <-ctx.Done():
			return
		}
	}
	r.tc.mu.Lock()
	cur := r.tc.courant
	r.tc.mu.Unlock()
	if cur != nil && cur.chaine != chaine {
		r.arreterTranscodage(cur)
	}
}

// arreterTranscodage tue ffmpeg (groupe de processus), attend sa mort, supprime le
// dossier. Synchrone et idempotent : quand il rend la main, il ne reste rien.
func (r *Relais) arreterTranscodage(t *transcodage) {
	t.arret.Do(func() {
		r.tc.mu.Lock()
		if r.tc.courant == t {
			r.tc.courant = nil
		}
		r.tc.mu.Unlock()
		t.annule()
		t.proc.Arreter()
		t.pr.Close() // debloque l'alimenteur si ffmpeg n'a jamais lu son entree
		if err := os.RemoveAll(t.dossier); err != nil {
			r.journal.Warn("dossier de transcodage non supprime", "chaine", t.chaine, "erreur", err)
		}
		close(t.fini)
	})
	<-t.fini
}

// arreterTranscodageDe : fin de lecture. Sans effet si la session ne lit pas (ou plus) cette chaine.
func (r *Relais) arreterTranscodageDe(session, chaine string) {
	if r.tc == nil {
		return
	}
	r.tc.mu.Lock()
	cur := r.tc.courant
	d := r.tc.demarrage
	r.tc.mu.Unlock()
	if d != nil && d.chaine == chaine && d.session == session {
		d.annule()
	}
	if cur != nil && cur.chaine == chaine && cur.session == session {
		r.arreterTranscodage(cur)
	}
}

// arreterToutTranscodage : arret du service.
func (r *Relais) arreterToutTranscodage() {
	if r.tc == nil {
		return
	}
	r.tc.mu.Lock()
	cur := r.tc.courant
	r.tc.mu.Unlock()
	if cur != nil {
		r.arreterTranscodage(cur)
	}
}

// assurerTranscodage rend le transcodage de la chaine dans ce mode et cette qualite, en le
// lancant au besoin (et en arretant tout autre : un seul ffmpeg a la fois ; un changement de
// qualite en cours de lecture arrete l'ancien ffmpeg avant d'en lancer un autre).
func (r *Relais) assurerTranscodage(ctx context.Context, chaine, session string, mode Mode, eco bool, bail domain.Bail) (*transcodage, error) {
	for {
		r.tc.mu.Lock()
		if cur := r.tc.courant; cur != nil && cur.chaine == chaine && cur.mode == mode && cur.eco == eco && cur.vivant() {
			r.tc.mu.Unlock()
			r.toucher(cur)
			return cur, nil
		}
		if d := r.tc.demarrage; d != nil {
			if d.chaine == chaine && d.mode == mode && d.eco == eco {
				d.attentes++
				r.tc.mu.Unlock()
				return r.attendreDemarrage(ctx, d)
			}
			d.annule() // autre chaine, autre mode ou autre qualite : on le laisse finir de se defaire
			r.tc.mu.Unlock()
			select {
			case <-d.fini:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		cur := r.tc.courant
		if cur != nil {
			r.tc.mu.Unlock()
			r.arreterTranscodage(cur)
			continue
		}
		// La patience (407) se compte depuis l'arrivee de la PREMIERE requete.
		dctx, annule := context.WithTimeout(r.base, r.dureeMaxDemarrage())
		if d, ok := ctx.Value(cleDebut{}).(time.Time); ok {
			dctx = context.WithValue(dctx, cleDebut{}, d)
		}
		dctx = context.WithValue(dctx, cleSession{}, session) // diagnostic d'occupation
		d := &demarrage{chaine: chaine, session: session, mode: mode, eco: eco, annule: annule, fini: make(chan struct{}), attentes: 1, bail: bail}
		r.tc.demarrage = d
		r.tc.mu.Unlock()
		r.taches.Add(1)
		go r.lancerDemarrage(dctx, d)
		return r.attendreDemarrage(ctx, d)
	}
}

// dureeMaxDemarrage : amorce amont (essais et patience) puis demarrage de ffmpeg.
func (r *Relais) dureeMaxDemarrage() time.Duration {
	return r.dureeMaxPlaylist() + r.opt.Reseau.DelaiSegment.Std() + r.opt.Live.DelaiDemarrageTranscodage.Std()
}

func (r *Relais) attendreDemarrage(ctx context.Context, d *demarrage) (*transcodage, error) {
	select {
	case <-d.fini:
		return d.t, d.err
	case <-ctx.Done():
		r.tc.mu.Lock()
		d.attentes--
		if d.attentes == 0 {
			d.annule()
		}
		r.tc.mu.Unlock()
		return nil, ctx.Err()
	}
}

func (r *Relais) lancerDemarrage(ctx context.Context, d *demarrage) {
	defer r.taches.Done()
	defer d.annule()
	t, err := r.demarrerTranscodage(ctx, d)
	r.tc.mu.Lock()
	if err == nil && ctx.Err() == nil {
		r.tc.courant = t
		r.toucher(t)
		t.pret.Store(true)
	} else if err == nil {
		err = ctx.Err()
	}
	if r.tc.demarrage == d {
		r.tc.demarrage = nil
	}
	r.tc.mu.Unlock()
	if err != nil && t != nil {
		r.arreterTranscodage(t)
		t = nil
	}
	d.t, d.err = t, err
	close(d.fini)
}

// amorce : ce qu'il faut pour que ffmpeg ait de quoi lire des le premier octet.
type amorce struct {
	variante string // playlist maitresse : adresse de la variante suivie
	vus      map[string]struct{}
	segments []segmentAmont // a envoyer, dans l'ordre
	premier  *http.Response // premier segment, deja ouvert (le 407 et les refus sont connus avant de lancer ffmpeg)
	fin      func()
	cible    time.Duration
	termine  bool
}

func (a *amorce) fermer() {
	if a.premier != nil {
		a.premier.Body.Close()
		a.fin()
		a.premier = nil
	}
}

// chargerMedia telecharge la playlist de media de la chaine (ou de sa variante).
func (r *Relais) chargerMedia(ctx context.Context, chaine string, variante string, renouveler func()) (analyse, error) {
	if variante == "" {
		corps, base, err := r.telecharger(ctx, chaine, renouveler)
		if err != nil {
			return analyse{}, err
		}
		return analyserPlaylist(corps, base), nil
	}
	if err := r.hotes.VerifierURL(ctx, variante); err != nil {
		return analyse{}, err
	}
	var corps string
	base := analyserURL(variante)
	err := r.avecEssais(ctx, chaine, r.opt.Reseau.EssaisPlaylist, renouveler, func(ctx context.Context) error {
		c, b, err := r.unePlaylist(ctx, variante)
		if err == nil {
			corps, base = c, b
		}
		return err
	})
	if err != nil {
		return analyse{}, err
	}
	return analyserPlaylist(corps, base), nil
}

func (r *Relais) amorcer(ctx context.Context, chaine string, renouveler func()) (*amorce, error) {
	a, err := r.chargerMedia(ctx, chaine, "", renouveler)
	if err != nil {
		return nil, err
	}
	am := &amorce{vus: map[string]struct{}{}}
	if len(a.segments) == 0 && a.variante != "" {
		am.variante = a.variante
		if a, err = r.chargerMedia(ctx, chaine, am.variante, renouveler); err != nil {
			return nil, err
		}
	}
	if len(a.segments) == 0 {
		return nil, fmt.Errorf("%w : playlist sans segment", domain.ErrAmont)
	}
	for _, s := range a.segments {
		am.vus[s.nom] = struct{}{}
	}
	n := min(r.opt.Live.SegmentsAmorce, len(a.segments))
	am.segments = a.segments[len(a.segments)-n:]
	am.cible, am.termine = a.cible, a.fin
	rep, fin, err := r.ouvrirSegment(ctx, chaine, am.segments[0].url, nil, renouveler)
	if err != nil {
		return nil, err
	}
	am.premier, am.fin = rep, fin
	return am, nil
}

func (r *Relais) demarrerTranscodage(ctx context.Context, d *demarrage) (*transcodage, error) {
	renouveler := d.bail.Renouveler
	am, err := r.amorcer(ctx, d.chaine, renouveler)
	if err != nil {
		return nil, err
	}
	alea := make([]byte, 6)
	if _, err := rand.Read(alea); err != nil {
		am.fermer()
		return nil, err
	}
	dossier := filepath.Join(r.tc.dossier, d.chaine+"-"+hex.EncodeToString(alea))
	if err := os.MkdirAll(r.tc.dossier, 0o700); err != nil {
		am.fermer()
		return nil, fmt.Errorf("%w : dossier : %v", ErrTranscodageImpossible, err)
	}
	if err := os.Mkdir(dossier, 0o700); err != nil {
		am.fermer()
		return nil, fmt.Errorf("%w : dossier : %v", ErrTranscodageImpossible, err)
	}
	prefixe := hex.EncodeToString(alea[:4])

	pr, pw := io.Pipe()
	var ouverte atomic.Bool
	source := func(_ context.Context, depuis int64) (io.ReadCloser, error) {
		if depuis != 0 || !ouverte.CompareAndSwap(false, true) {
			return nil, errors.New("live : un direct ne se rouvre pas")
		}
		return pr, nil
	}
	tctx, annule := context.WithCancel(context.WithValue(r.base, cleSession{}, d.session))
	tache := remux.TacheDirect{
		Source: source, Video: d.mode == ModeVideo, Dossier: dossier, Prefixe: prefixe,
		SegmentSecondes: r.opt.Live.SegmentHLS, ListeSegments: r.opt.Live.ListeHLS,
	}
	if d.eco && d.mode == ModeVideo {
		tache.Eco = r.cibleEco()
	}
	proc, err := r.opt.Lanceur.LancerDirect(tctx, tache)
	if err != nil {
		annule()
		pr.Close()
		am.fermer()
		_ = os.RemoveAll(dossier)
		if errors.Is(err, remux.ErrSaturation) {
			return nil, err
		}
		r.journal.Warn("ffmpeg ne demarre pas", "chaine", d.chaine, "erreur", remux.Assainir(err.Error()))
		return nil, fmt.Errorf("%w : %v", ErrTranscodageImpossible, remux.Assainir(err.Error()))
	}
	t := &transcodage{chaine: d.chaine, session: d.session, mode: d.mode, eco: d.eco, dossier: dossier, prefixe: prefixe,
		proc: proc, pr: pr, annule: annule, ctx: tctx, mort: make(chan struct{}), fini: make(chan struct{}), bail: d.bail}
	r.toucher(t)
	r.taches.Add(3)
	go func() {
		defer r.taches.Done()
		if err := proc.Attendre(); err != nil && tctx.Err() == nil {
			r.journal.Warn("ffmpeg termine en erreur", "chaine", t.chaine, "erreur", remux.Assainir(err.Error()))
		}
		close(t.mort)
	}()
	go r.alimenter(t, pw, am)
	go r.surveiller(t)

	// Attente des premiers segments, bornee. ffmpeg qui meurt avant : echec net, pas d'attente inutile.
	delai := time.NewTimer(r.opt.Live.DelaiDemarrageTranscodage.Std())
	defer delai.Stop()
	tic := time.NewTicker(sondageDossier)
	defer tic.Stop()
	for {
		if segmentsPrets(dossier) >= 2 {
			return t, nil
		}
		select {
		case <-t.mort:
			return t, fmt.Errorf("%w : ffmpeg s'est arrete au demarrage", ErrTranscodageImpossible)
		case <-delai.C:
			return t, fmt.Errorf("%w : pas de segment dans le delai", ErrTranscodageImpossible)
		case <-ctx.Done():
			return t, ctx.Err()
		case <-tic.C:
		}
	}
}

// segmentsPrets compte les segments complets (le muxer ecrit d'abord un .tmp) ; la
// playlist n'existe qu'apres le premier.
func segmentsPrets(dossier string) int {
	if _, err := os.Stat(filepath.Join(dossier, remux.NomPlaylistDirect)); err != nil {
		return 0
	}
	entrees, err := os.ReadDir(dossier)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entrees {
		if reNomSegmentLocal.MatchString(e.Name()) {
			n++
		}
	}
	return n
}

// surveiller renouvelle le bail tant que ffmpeg tourne, arrete ffmpeg quand le lecteur
// n'a rien demande depuis live.inactivite_transcodage, ou quand il meurt.
func (r *Relais) surveiller(t *transcodage) {
	defer r.taches.Done()
	inactivite := r.opt.Live.InactiviteTranscodage.Std()
	// La cadence suit aussi le bail : un tick plus lent que TTL/3 ferait expirer la place.
	periode := min(max(min(inactivite/4, r.opt.Bail.TTL.Std()/6), 10*time.Millisecond), 2*time.Second)
	tic := time.NewTicker(periode)
	defer tic.Stop()
	dernier := r.opt.Horloge()
	for {
		select {
		case <-t.mort:
			r.arreterTranscodage(t)
			return
		case <-t.ctx.Done():
			r.arreterTranscodage(t)
			return
		case <-tic.C:
			now := r.opt.Horloge()
			if t.pret.Load() && now.Sub(time.Unix(0, t.vue.Load())) > inactivite {
				r.journal.Info("transcodage arrete : lecteur inactif", "chaine", t.chaine)
				r.arreterTranscodage(t)
				r.libererSi(t.session, t.chaine, t.bail)
				return
			}
			if now.Sub(dernier) >= r.opt.Bail.TTL.Std()/3 {
				t.bail.Renouveler()
				dernier = now
			}
		}
	}
}

// alimenter met les segments du fournisseur bout a bout dans l'entree de ffmpeg. Il est
// le SEUL a parler au fournisseur pendant un transcodage : c'est lui qui tient la
// connexion unique, avec le meme client filtre (hotes, essais, patience 407) que le relais.
func (r *Relais) alimenter(t *transcodage, pw *io.PipeWriter, am *amorce) {
	defer r.taches.Done()
	err := r.boucleAlimentation(t, pw, am)
	am.fermer()
	if err != nil && t.ctx.Err() == nil {
		r.journal.Warn("alimentation du transcodage interrompue", "chaine", t.chaine, "erreur", remux.Assainir(err.Error()))
	}
	_ = pw.CloseWithError(err) // nil : fin de fichier, ffmpeg termine proprement
}

func (r *Relais) boucleAlimentation(t *transcodage, pw *io.PipeWriter, am *amorce) error {
	ctx := t.ctx
	renouveler := t.bail.Renouveler
	file := am.segments
	echecs := 0
	buf := make([]byte, r.opt.Live.TailleTampon)
	for {
		for len(file) > 0 {
			sg := file[0]
			file = file[1:]
			var rep *http.Response
			var fin func()
			if am.premier != nil {
				rep, fin, am.premier = am.premier, am.fin, nil
			} else {
				var err error
				if rep, fin, err = r.ouvrirSegment(ctx, t.chaine, sg.url, nil, renouveler); err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					if echecs++; echecs >= echecsSegmentsMax {
						return err
					}
					r.journal.Warn("segment perdu pendant le transcodage", "chaine", t.chaine, "erreur", remux.Assainir(err.Error()))
					continue
				}
			}
			errLect, errEcr := copierVers(pw, rep.Body, buf)
			rep.Body.Close()
			fin()
			if errEcr != nil {
				return errEcr // ffmpeg est parti : plus rien a alimenter
			}
			if errLect != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if echecs++; echecs >= echecsSegmentsMax {
					return errLect
				}
				continue
			}
			echecs = 0
			renouveler()
			r.inst.Compter("live_transcodage_segments_total", 1, "mode", string(t.mode))
		}
		// Plus rien a envoyer : on attend que la playlist annonce du nouveau.
		for len(file) == 0 {
			if am.termine {
				return nil
			}
			if err := attendre(ctx, max(r.opt.Live.FraicheurPlaylist.Std(), am.cible/2)); err != nil {
				return err
			}
			a, err := r.chargerMedia(ctx, t.chaine, am.variante, renouveler)
			if err != nil {
				return err
			}
			courants := map[string]struct{}{}
			for _, s := range a.segments {
				courants[s.nom] = struct{}{}
				if _, vu := am.vus[s.nom]; !vu {
					file = append(file, s)
					am.vus[s.nom] = struct{}{}
				}
			}
			// Un segment sorti de la fenetre ne revient jamais : la memoire reste bornee.
			for nom := range am.vus {
				if _, ok := courants[nom]; !ok {
					delete(am.vus, nom)
				}
			}
			am.cible = max(am.cible, a.cible)
			am.termine = a.fin
		}
	}
}

// copierVers recopie le corps ; il distingue la lecture amont (segment perdu, on continue)
// de l'ecriture (ffmpeg parti, on s'arrete).
func copierVers(w io.Writer, corps io.Reader, buf []byte) (errLect, errEcr error) {
	for {
		n, err := corps.Read(buf)
		if n > 0 {
			if _, e := w.Write(buf[:n]); e != nil {
				return nil, e
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, nil
			}
			return err, nil
		}
	}
}

// --- service des fichiers produits ------------------------------------------------------

// servirTranscodage repond a la playlist d'une chaine convertie : celle de ffmpeg, dont
// chaque segment recoit un jeton signe LIE a la chaine, stable d'une playlist a l'autre
// (hls.js compare les URL d'un meme numero de segment).
func (r *Relais) playlistLocale(t *transcodage) (string, error) {
	brut, err := os.ReadFile(filepath.Join(t.dossier, remux.NomPlaylistDirect))
	if err != nil {
		return "", err
	}
	prefixe := r.prefixeSegments(t.chaine)
	var sortie strings.Builder
	extinf := ""
	segments := 0
	for _, l := range strings.Split(strings.ReplaceAll(string(brut), "\r\n", "\n"), "\n") {
		l = strings.TrimSpace(l)
		switch {
		case l == "":
		case strings.HasPrefix(l, "#EXTINF"):
			extinf = l
		case strings.HasPrefix(l, "#"):
			sortie.WriteString(l + "\n")
		case reNomSegmentLocal.MatchString(l) && strings.HasPrefix(l, t.prefixe+"_"):
			// Un segment deja supprime par ffmpeg n'est pas annonce (et son EXTINF non plus).
			if _, err := os.Stat(filepath.Join(t.dossier, l)); err != nil {
				extinf = ""
				continue
			}
			sortie.WriteString(extinf + "\n" + prefixe + "/" + r.tc.sceau.Sceller(t.chaine, "tc:"+l) + ".ts\n")
			extinf = ""
			segments++
		default:
			extinf = ""
		}
	}
	if segments == 0 || !strings.HasPrefix(sortie.String(), "#EXTM3U") {
		return "", errors.New("playlist de transcodage vide")
	}
	// Une relance de ffmpeg (ou le passage du relais brut a la conversion) repart a la
	// sequence 0 avec de nouveaux noms : meme continuite que pour l'amont (continuite.go).
	servi := sortie.String()
	r.mu.Lock()
	if _, ok := r.observer(t.chaine, servi, r.opt.Horloge()); ok {
		servi = appliquerContinuite(servi, r.sequences[t.chaine])
	}
	r.mu.Unlock()
	return servi, nil
}

// nomSegmentLocal rend le nom de fichier d'un jeton de segment local, s'il en est un.
func (r *Relais) nomSegmentLocal(chaine, jeton string) (string, bool) {
	if r.tc == nil {
		return "", false
	}
	charge, err := r.tc.sceau.Ouvrir(chaine, jeton)
	if err != nil {
		return "", false
	}
	nom, ok := strings.CutPrefix(charge, "tc:")
	return nom, ok && reNomSegmentLocal.MatchString(nom)
}

func (r *Relais) servirSegmentLocal(w http.ResponseWriter, req *http.Request, chaine, nom string) {
	t := r.transcodageDe(chaine)
	if t == nil || !strings.HasPrefix(nom, t.prefixe+"_") {
		http.Error(w, "segment inconnu (playlist a recharger)", http.StatusNotFound)
		return
	}
	f, err := os.Open(filepath.Join(t.dossier, nom))
	if err != nil {
		http.Error(w, "segment expire", http.StatusNotFound)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.Error(w, "segment expire", http.StatusNotFound)
		return
	}
	r.toucher(t)
	w.Header().Set("Content-Type", "video/mp2t")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, req, nom, st.ModTime(), f)
}
