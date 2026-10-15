// Package remux lance ffmpeg et ffprobe pour les films que le navigateur ne lit
// pas tels quels. Il est le SEUL endroit ou des arguments de ces programmes sont
// fabriques, a partir de champs valides (jamais d'une chaine fournie telle quelle).
package remux

import (
	"context"
	"errors"
	"fmt"
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

// Arguments construit la ligne de commande ffmpeg d'une tache. Fonction pure,
// testee sur table : c'est le point qui garantit l'absence d'injection.
func Arguments(t domain.TacheRemux) ([]string, error) {
	if err := validerURL(t.URLAmont); err != nil {
		return nil, err
	}
	agent, err := agentDepuis(t.Entetes)
	if err != nil {
		return nil, err
	}
	if t.Debut < 0 || t.Debut > decalageMaxHs*time.Hour {
		return nil, fmt.Errorf("%w : position hors limites", ErrArgumentRefuse)
	}
	if t.Mode != domain.ModeRemux && t.Mode != domain.ModeAudioConverti {
		return nil, fmt.Errorf("%w : mode %q non remuxable", ErrArgumentRefuse, t.Mode)
	}
	a := []string{"-hide_banner", "-loglevel", "warning", "-nostdin", "-protocol_whitelist", protocoles}
	if agent != "" {
		a = append(a, "-user_agent", agent)
	}
	// -ss AVANT -i : recherche sur l'entree (rapide, tombe sur une image cle).
	if t.Debut > 500*time.Millisecond {
		a = append(a, "-ss", strconv.FormatFloat(t.Debut.Seconds(), 'f', 3, 64))
	}
	a = append(a, "-i", t.URLAmont, "-map", "0:v:0?", "-map", "0:a:0?", "-sn", "-dn", "-c:v", "copy")
	if t.Mode == domain.ModeAudioConverti {
		a = append(a, "-c:a", "aac", "-b:a", "192k", "-ac", "2")
	} else {
		a = append(a, "-c:a", "copy")
	}
	return append(a, "-movflags", "+frag_keyframe+empty_moov+default_base_moof", "-f", "mp4", "pipe:1"), nil
}

// Lancer demarre ffmpeg, apres avoir obtenu un des processus_max emplacements.
// Le processus est tue quand ctx est annule ET quand Arreter est appele; son
// emplacement est rendu a sa mort, jamais avant.
func (l *Lanceur) Lancer(ctx context.Context, t domain.TacheRemux) (domain.Processus, error) {
	args, err := Arguments(t)
	if err != nil {
		return nil, err
	}
	if err := l.prendre(ctx); err != nil {
		return nil, err
	}
	// Le contexte du processus est propre : seule la mort du processus ou Arreter
	// le termine, mais l'annulation de ctx le coupe aussi.
	pctx, annule := context.WithCancel(ctx)
	p, err := l.exe.Demarrer(pctx, l.r.Ffmpeg, args)
	if err != nil {
		annule()
		<-l.places
		return nil, err
	}
	pl := &processusBorne{Processus: p, annule: annule}
	go func() {
		// Rend l'emplacement a la mort du processus (Attendre est re-appelable).
		_ = p.Attendre()
		annule()
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
	once   sync.Once
}

func (p *processusBorne) Arreter() {
	p.once.Do(p.annule)
	p.Processus.Arreter()
}
