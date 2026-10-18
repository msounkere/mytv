// Package remux lance ffmpeg et ffprobe pour les films que le navigateur ne lit
// pas tels quels. Il est le SEUL endroit ou des arguments de ces programmes sont
// fabriques, a partir de champs valides (jamais d'une chaine fournie telle quelle).
package remux

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"mytv/internal/domain"
)

var (
	// ErrSaturation : tous les processus sont pris et la file d'attente est pleine
	// ou l'attente maximale est ecoulee (l'API repond 503 + Retry-After).
	ErrSaturation = errors.New("remux : trop de remux simultanes")
)

// Reglages du lanceur (config.Remux).
type Reglages struct {
	Ffmpeg       string
	ProcessusMax int           // remux.processus_max
	FileMax      int           // remux.file_max : demandes en attente d'un processus libre
	AttenteFile  time.Duration // remux.attente_file
	Reprises     int           // remux.reprises_max : reouvertures de l'amont apres une coupure
}

// Lanceur implemente domain.Lanceur.
type Lanceur struct {
	r       Reglages
	exe     Executeur
	places  chan struct{}
	attente atomic.Int64
}

var _ domain.Lanceur = (*Lanceur)(nil)

func NouveauLanceur(r Reglages, exe Executeur) (*Lanceur, error) {
	if r.ProcessusMax < 1 {
		return nil, errors.New("remux : processus_max doit etre >= 1")
	}
	if r.Ffmpeg == "" {
		return nil, errors.New("remux : chemin de ffmpeg vide")
	}
	if exe == nil {
		exe = ExecOS{}
	}
	return &Lanceur{r: r, exe: exe, places: make(chan struct{}, r.ProcessusMax)}, nil
}

// protocolesFilm : ffmpeg ne peut ouvrir QUE de l'http local (voir Arguments).
const protocolesFilm = "http,tcp"

// urlValidation sert a controler une tache avant de reserver quoi que ce soit.
const urlValidation = "http://127.0.0.1:1/00000000000000000000000000000000/film"

// Arguments construit la ligne de commande ffmpeg d'une tache de FILM. Fonction
// pure, testee sur table : c'est le point qui garantit l'absence d'injection ET
// l'absence de secret.
//
// POURQUOI une adresse LOCALE : une URL amont porte les identifiants du fournisseur.
// Passee en argument (`-i http://u:p@...`), elle est lisible de tout utilisateur de la
// machine (/proc/<pid>/cmdline, `ps`) et apparait dans les erreurs de ffmpeg. Le serveur
// lit donc lui-meme l'amont (identifiants, redirections, filtre d'hotes, Range, reprise)
// et ne montre a ffmpeg qu'un serveur HTTP local a usage unique (fluxSonde), sur
// 127.0.0.1, avec un chemin secret aleatoire.
//
// POURQUOI pas un tube (pipe:0), comme avant : un tube n'est pas « seekable ». `-ss`
// devait alors LIRE ET JETER tout le film jusqu'a la position : reprendre une serie a
// 14 min coutait des centaines de Mo retelecharges avant la premiere image (constate en
// recette reelle : lecteur qui « tourne sans s'ouvrir »). Avec une source qui sert des
// plages, ffmpeg lit l'index du conteneur, saute directement a la position, et un mp4
// dont l'index est en fin de fichier devient remuxable.
func Arguments(t domain.TacheRemux, urlLocale string) ([]string, error) {
	if t.Source == nil {
		return nil, fmt.Errorf("%w : source d'octets absente", ErrArgumentRefuse)
	}
	if t.Debut < 0 || t.Debut > decalageMaxHs*time.Hour {
		return nil, fmt.Errorf("%w : position hors limites", ErrArgumentRefuse)
	}
	if t.Mode != domain.ModeRemux && t.Mode != domain.ModeAudioConverti {
		return nil, fmt.Errorf("%w : mode %q non remuxable", ErrArgumentRefuse, t.Mode)
	}
	if !adresseLocaleSure(urlLocale) {
		return nil, fmt.Errorf("%w : l'entree doit etre l'adresse locale de la source", ErrArgumentRefuse)
	}
	// -protocol_whitelist http,tcp : ffmpeg ne peut ouvrir que de l'http ; combine a
	// l'adresse locale, meme un conteneur « a playlist » (hls, concat) ne peut pas lui
	// faire ouvrir un fichier ou un autre hote sans passer par notre source.
	a := []string{"-hide_banner", "-loglevel", "warning", "-nostdin", "-protocol_whitelist", protocolesFilm}
	// -ss AVANT -i : saut sur l'entree (par plage, grace a l'index du conteneur).
	if t.Debut > 500*time.Millisecond {
		a = append(a, "-ss", strconv.FormatFloat(t.Debut.Seconds(), 'f', 3, 64))
	}
	a = append(a, "-i", urlLocale, "-map", "0:v:0?", "-map", "0:a:0?", "-sn", "-dn", "-c:v", "copy")
	if t.HEVC {
		a = append(a, "-tag:v", "hvc1")
	}
	if t.Mode == domain.ModeAudioConverti {
		a = append(a, "-c:a", "aac", "-b:a", "192k", "-ac", "2")
	} else {
		a = append(a, "-c:a", "copy")
	}
	return append(a, "-movflags", "+frag_keyframe+empty_moov+default_base_moof", "-f", "mp4", "pipe:1"), nil
}

// adresseLocaleSure : http://127.0.0.1:<port>/<jeton de 32 chiffres hexa>/film, rien d'autre.
var reAdresseLocale = regexp.MustCompile(`^http://127\.0\.0\.1:[0-9]{1,5}/[0-9a-f]{32}/film$`)

func adresseLocaleSure(u string) bool {
	return reAdresseLocale.MatchString(u)
}

// Lancer demarre ffmpeg, apres avoir obtenu un des processus_max emplacements.
// Le processus est tue quand ctx est annule ET quand Arreter est appele; son
// emplacement est rendu a sa mort, jamais avant.
func (l *Lanceur) Lancer(ctx context.Context, t domain.TacheRemux) (domain.Processus, error) {
	if _, err := Arguments(t, urlValidation); err != nil { // refuse avant de reserver quoi que ce soit
		return nil, err
	}
	if err := l.prendre(ctx); err != nil {
		return nil, err
	}
	// Le contexte du processus est propre : seule la mort du processus ou Arreter le
	// termine, mais l'annulation de ctx le coupe aussi. Il porte aussi la lecture de
	// l'amont : l'annuler ferme la connexion du fournisseur.
	pctx, annule := context.WithCancel(ctx)
	flux, err := ouvrirFluxSonde(avecReprises(t.Source, l.r.Reprises))
	if err != nil {
		annule()
		<-l.places
		return nil, err
	}
	args, err := Arguments(t, flux.url)
	if err != nil {
		flux.Fermer()
		annule()
		<-l.places
		return nil, err
	}
	p, err := l.exe.Demarrer(pctx, l.r.Ffmpeg, args, nil)
	if err != nil {
		flux.Fermer()
		annule()
		<-l.places
		return nil, err
	}
	pl := &processusBorne{Processus: p, annule: annule, fermer: flux.Fermer}
	go func() {
		// Rend l'emplacement a la mort du processus (Attendre est re-appelable).
		_ = p.Attendre()
		annule()
		flux.Fermer()
		<-l.places
	}()
	return pl, nil
}

// lancer est le corps commun du remux d'un film et du transcodage d'un direct.
func (l *Lanceur) lancer(ctx context.Context, args []string, src domain.SourceOctets, reprises int) (domain.Processus, error) {
	if err := l.prendre(ctx); err != nil {
		return nil, err
	}
	// Le contexte du processus est propre : seule la mort du processus ou Arreter
	// le termine, mais l'annulation de ctx le coupe aussi. Il porte aussi la lecture
	// de l'amont : l'annuler ferme la connexion du fournisseur.
	pctx, annule := context.WithCancel(ctx)
	entree := nouvelleEntree(pctx, src, reprises)
	p, err := l.exe.Demarrer(pctx, l.r.Ffmpeg, args, entree)
	if err != nil {
		annule()
		entree.Close()
		<-l.places
		return nil, err
	}
	pl := &processusBorne{Processus: p, annule: annule, fermer: func() { entree.Close() }}
	go func() {
		// Rend l'emplacement a la mort du processus (Attendre est re-appelable).
		_ = p.Attendre()
		annule()
		entree.Close()
		<-l.places
	}()
	return pl, nil
}

func (l *Lanceur) prendre(ctx context.Context) error {
	select {
	case l.places <- struct{}{}:
		return nil
	default:
	}
	if l.r.FileMax <= 0 || l.attente.Add(1) > int64(l.r.FileMax) {
		if l.r.FileMax > 0 {
			l.attente.Add(-1)
		}
		return ErrSaturation
	}
	defer l.attente.Add(-1)
	t := time.NewTimer(l.r.AttenteFile)
	defer t.Stop()
	select {
	case l.places <- struct{}{}:
		return nil
	case <-t.C:
		return ErrSaturation
	case <-ctx.Done():
		return ctx.Err()
	}
}

// EnAttente et EnCours exposent la charge (diagnostic, tests).
func (l *Lanceur) EnAttente() int { return int(l.attente.Load()) }
func (l *Lanceur) EnCours() int   { return len(l.places) }

type processusBorne struct {
	domain.Processus
	annule context.CancelFunc
	fermer func() // coupe la lecture de l'amont (l'adresse locale de ffmpeg, ou l'entree par tube)
	once   sync.Once
}

func (p *processusBorne) Arreter() {
	p.once.Do(p.annule)
	p.Processus.Arreter()
	p.fermer() // la connexion amont ne survit pas au processus
}
