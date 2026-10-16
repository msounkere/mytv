package live

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"mytv/internal/domain"
	"mytv/internal/remux"
)

// SondeurDirect analyse les octets d'un segment (remux.Sondeur en production).
type SondeurDirect interface {
	SonderDirect(ctx context.Context, src domain.SourceOctets) (remux.SondeDirect, error)
}

// octetsSondeMax borne ce qu'un segment peut fournir a ffprobe : sa sonde ne lit que
// -probesize (6 Mo) ; un segment 4K de 16 Mo n'a pas a etre telecharge en entier.
const octetsSondeMax = 8 << 20

// verdictsMax borne le cache : une entree par chaine sondee, quelques dizaines d'octets.
const verdictsMax = 5000

// verdict est le resultat gardé d'une sonde (ou de son echec).
type verdict struct {
	sonde remux.SondeDirect
	d     Decision
	jusqu time.Time
	pose  time.Time
	// echec : la sonde n'a rien donne ; la lecture se replie sur le relais brut jusqu'a `jusqu`.
	echec bool
}

// volSonde regroupe les demandes simultanees d'une meme chaine (singleflight).
type volSonde struct {
	fini    chan struct{}
	annuler context.CancelFunc
	v       verdict
}

type sondes struct {
	mu    sync.Mutex
	cache map[string]verdict
	vol   map[string]*volSonde
	// sem : UNE sonde a la fois. Chaque sonde telecharge un segment chez le fournisseur,
	// qui n'accorde qu'une connexion : deux sondes de chaines differentes se disputeraient la place.
	sem chan struct{}
}

func nouvellesSondes() *sondes {
	return &sondes{cache: map[string]verdict{}, vol: map[string]*volSonde{}, sem: make(chan struct{}, 1)}
}

func (r *Relais) sondeActive() bool { return r.opt.Sondeur != nil }

func (r *Relais) reglagesDecision() ReglagesDecision {
	return ReglagesDecision{ReencodageVideo: r.opt.Live.ReencodageVideo, HauteurMax: r.opt.Live.HauteurMaxReencodage}
}

// verdictConnu rend le verdict gardé d'une chaine, perime ou non (en-tetes, garde-fous).
func (r *Relais) verdictConnu(chaine string) (verdict, bool) {
	r.sd.mu.Lock()
	defer r.sd.mu.Unlock()
	v, ok := r.sd.cache[chaine]
	return v, ok && !v.echec
}

// verdictPour donne le verdict d'une chaine SANS retarder une chaine deja sondee.
//
//   - verdict frais : rendu tout de suite ;
//   - verdict perime d'une chaine en direct (ou dont un transcodage tourne deja) : rendu
//     tout de suite aussi, et une sonde repart en arriere-plan (jamais pendant un transcodage,
//     qui tient deja la connexion du fournisseur) ;
//   - echec recent : relais brut, sans nouvelle tentative avant `live.echec_sonde` ;
//   - inconnu : une sonde, bornee par `live.delai_sonde`, dont l'echec laisse lire la chaine.
//
// `adresse` est celle du segment a sonder (playlist deja obtenue) ; vide si l'appelant n'en a pas.
func (r *Relais) verdictPour(ctx context.Context, chaine, adresse string, caps Capacites, renouveler func()) verdict {
	maintenant := r.opt.Horloge()
	r.sd.mu.Lock()
	v, ok := r.sd.cache[chaine]
	if ok {
		v = r.avecDecision(v, caps)
	}
	if ok && maintenant.Before(v.jusqu) {
		r.sd.mu.Unlock()
		return v
	}
	if ok && !v.echec && (v.d.Mode == ModeDirect || r.transcodageActifDe(chaine)) {
		r.sd.mu.Unlock()
		if v.d.Mode == ModeDirect && adresse != "" {
			r.sonderEnArrierePlan(chaine, adresse, renouveler)
		}
		return v
	}
	r.sd.mu.Unlock()
	if adresse == "" {
		return verdict{d: Decision{Mode: ModeDirect, Raison: "aucun segment a sonder"}, echec: true}
	}
	vol := r.lancerSonde(chaine, adresse, renouveler)
	select {
	case <-vol.fini:
		return r.avecDecision(vol.v, caps)
	case <-ctx.Done():
		return verdict{d: Decision{Mode: ModeDirect, Raison: "lecteur parti"}, echec: true}
	}
}

// avecDecision applique les capacites de CET appelant a la sonde de la chaine : la sonde
// est gardee par chaine (elle ne depend pas du navigateur), la decision par (chaine,
// capacites) en est calculee a chaque requete, ce qui ne coute rien.
func (r *Relais) avecDecision(v verdict, caps Capacites) verdict {
	if !v.echec {
		v.d = DeciderLive(v.sonde, caps, r.reglagesDecision())
	}
	return v
}

func (r *Relais) sonderEnArrierePlan(chaine, adresse string, renouveler func()) {
	_ = r.lancerSonde(chaine, adresse, renouveler)
}

// lancerSonde demarre la sonde d'une chaine, ou rejoint celle qui est en cours.
func (r *Relais) lancerSonde(chaine, adresse string, renouveler func()) *volSonde {
	r.sd.mu.Lock()
	if v, ok := r.sd.vol[chaine]; ok {
		r.sd.mu.Unlock()
		return v
	}
	// La sonde survit a la requete qui l'a declenchee (d'autres l'attendent, et son
	// resultat sert aux suivantes) mais pas au relais, ni a son delai.
	ctx, annuler := context.WithTimeout(r.base, r.opt.Live.DelaiSonde.Std())
	v := &volSonde{fini: make(chan struct{}), annuler: annuler}
	r.sd.vol[chaine] = v
	r.sd.mu.Unlock()
	r.taches.Add(1)
	go func() {
		defer r.taches.Done()
		defer annuler()
		res, err := r.sonderSegment(ctx, chaine, adresse, renouveler)
		maintenant := r.opt.Horloge()
		var e verdict
		if err != nil {
			e = verdict{d: Decision{Mode: ModeDirect, Raison: "sonde impossible, relais brut"}, echec: true,
				jusqu: maintenant.Add(r.opt.Live.EchecSonde.Std()), pose: maintenant}
			if ctx.Err() == nil || errors.Is(ctx.Err(), context.DeadlineExceeded) {
				r.inst.Compter("live_sondes_total", 1, "resultat", "echec")
				r.journal.Warn("sonde du direct impossible", "chaine", chaine, "erreur", remux.Assainir(err.Error()))
			}
		} else {
			e = verdict{sonde: res, d: DeciderLive(res, Capacites{}, r.reglagesDecision()),
				jusqu: maintenant.Add(r.opt.Live.DureeSonde.Std()), pose: maintenant}
			r.inst.Compter("live_sondes_total", 1, "resultat", "ok")
			r.journal.Info("sonde du direct", "chaine", chaine, "video", res.CodecVideo, "hauteur", res.Hauteur,
				"audio", res.CodecAudio)
		}
		r.sd.mu.Lock()
		annulee := err != nil && ctx.Err() != nil && !errors.Is(ctx.Err(), context.DeadlineExceeded)
		if ancien, ok := r.sd.cache[chaine]; err != nil && ok && !ancien.echec {
			// Un echec passager ne doit pas faire oublier ce qu'on savait d'une chaine (un HEVC
			// relaye « brut » parce qu'une sonde a expire donnerait des erreurs en boucle).
			ancien.jusqu = maintenant.Add(r.opt.Live.EchecSonde.Std())
			e = ancien
			r.garderVerdict(chaine, e)
		} else if !annulee {
			// Une sonde annulee (zapping, arret) n'apprend rien : on ne memorise pas son « echec ».
			r.garderVerdict(chaine, e)
		}
		delete(r.sd.vol, chaine)
		v.v = e
		r.sd.mu.Unlock()
		close(v.fini)
	}()
	return v
}

// garderVerdict (verrou pris) range un verdict en restant borne : d'abord les perimes, puis le plus ancien.
func (r *Relais) garderVerdict(chaine string, v verdict) {
	c := r.sd.cache
	if _, existe := c[chaine]; !existe && len(c) >= verdictsMax {
		maintenant := r.opt.Horloge()
		for k, e := range c {
			if !maintenant.Before(e.jusqu) {
				delete(c, k)
			}
		}
		for len(c) >= verdictsMax {
			ancienne, plusVieux := "", maintenant.Add(time.Hour)
			for k, e := range c {
				if e.pose.Before(plusVieux) {
					ancienne, plusVieux = k, e.pose
				}
			}
			delete(c, ancienne)
		}
	}
	c[chaine] = v
}

// annulerSondesSauf coupe les sondes des AUTRES chaines : un zapping ne doit pas laisser
// un segment de l'ancienne chaine se telecharger a cote de la nouvelle (une seule connexion).
func (r *Relais) annulerSondesSauf(chaine string) {
	r.sd.mu.Lock()
	defer r.sd.mu.Unlock()
	for c, v := range r.sd.vol {
		if c != chaine {
			v.annuler()
		}
	}
}

// sonderSegment telecharge UN segment de la chaine (ou, pour une playlist maitresse, de sa
// premiere variante) et le donne a ffprobe par une source locale a usage unique.
func (r *Relais) sonderSegment(ctx context.Context, chaine, adresse string, renouveler func()) (remux.SondeDirect, error) {
	select {
	case r.sd.sem <- struct{}{}:
		defer func() { <-r.sd.sem }()
	case <-ctx.Done():
		return remux.SondeDirect{}, ctx.Err()
	}
	cible, err := r.segmentDeLaSonde(ctx, chaine, adresse, renouveler)
	if err != nil {
		return remux.SondeDirect{}, err
	}
	src := func(ctx context.Context, depuis int64) (io.ReadCloser, error) {
		if depuis > 0 {
			// Un segment n'a pas de Range utile : ffprobe le lit d'un trait.
			return nil, fmt.Errorf("%w : sonde du direct sans reprise", domain.ErrAmont)
		}
		rep, fin, err := r.ouvrirSegment(ctx, cible, nil, renouveler)
		if err != nil {
			return nil, err
		}
		return &segmentSonde{Reader: io.LimitReader(rep.Body, octetsSondeMax), corps: rep.Body, fin: fin}, nil
	}
	return r.opt.Sondeur.SonderDirect(ctx, src)
}

// segmentDeLaSonde suit au plus une playlist maitresse jusqu'a un segment.
func (r *Relais) segmentDeLaSonde(ctx context.Context, chaine, adresse string, renouveler func()) (string, error) {
	for profondeur := 0; ; profondeur++ {
		if !estSousPlaylist(adresse) {
			return adresse, nil
		}
		if profondeur >= 2 {
			return "", fmt.Errorf("%w : playlist maitresse imbriquee", domain.ErrAmont)
		}
		if err := r.hotes.VerifierURL(ctx, adresse); err != nil {
			return "", err
		}
		var corps string
		var base = analyserURL(adresse)
		err := r.avecEssais(ctx, r.opt.Reseau.EssaisPlaylist, renouveler, func(ctx context.Context) error {
			c, b, err := r.unePlaylist(ctx, adresse)
			if err == nil {
				corps, base = c, b
			}
			return err
		})
		if err != nil {
			return "", err
		}
		a := analyserPlaylist(corps, base)
		if u := a.urlDeSonde(); u != "" {
			adresse = u
			continue
		}
		return "", fmt.Errorf("%w : playlist sans segment", domain.ErrAmont)
	}
}

// ouvrirSegment ouvre un segment chez le fournisseur : filtre d'hotes, nouveaux essais,
// patience sur 407. L'appelant ferme le corps PUIS appelle fin.
func (r *Relais) ouvrirSegment(ctx context.Context, cible string, entetes http.Header, renouveler func()) (*http.Response, func(), error) {
	if err := r.hotes.VerifierURL(ctx, cible); err != nil {
		return nil, nil, err
	}
	var rep *http.Response
	var fin context.CancelFunc
	err := r.avecEssais(ctx, r.opt.Reseau.EssaisSegment, renouveler, func(ctx context.Context) error {
		var err error
		rep, fin, err = r.requete(ctx, cible, entetes, r.opt.Reseau.DelaiSegment.Std())
		if err != nil {
			return err
		}
		if rep.StatusCode != http.StatusOK && rep.StatusCode != http.StatusPartialContent {
			code := rep.StatusCode
			rep.Body.Close()
			fin()
			return fmt.Errorf("%w : segment refuse (HTTP %d)", domain.ErrAmont, code)
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	return rep, fin, nil
}

// segmentSonde rend les octets bornes d'un segment et libere la requete amont a la fermeture.
type segmentSonde struct {
	io.Reader
	corps io.Closer
	fin   func()
	once  sync.Once
}

func (s *segmentSonde) Close() error {
	s.once.Do(func() { s.corps.Close(); s.fin() })
	return nil
}

func estSousPlaylist(adresse string) bool {
	return strings.HasSuffix(strings.ToLower(analyserURL(adresse).Path), ".m3u8")
}

// analyserURL ne rend jamais nil : une adresse illisible donne une URL vide, que le filtre d'hotes refuse.
func analyserURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil || u == nil {
		return &url.URL{}
	}
	return u
}
