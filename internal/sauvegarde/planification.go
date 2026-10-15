package sauvegarde

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"

	"mytv/internal/domain"
)

// Reglages : voir config.Sauvegarde.
type Reglages struct {
	Heure        string // « 03:30 », vide = pas de planification
	Dossier      string
	Conservation int // jours
}

// Options du Planificateur ; seuls Reglages et Copier sont obligatoires.
type Options struct {
	Reglages
	// Copier ecrit une copie verifiee dans destination (Base.Copier).
	Copier func(ctx context.Context, destination string) (int64, error)
	// Maintenant et Attendre sont injectables : les tests ne dorment ni n'attendent 03:30.
	Maintenant func() time.Time
	Attendre   func(ctx context.Context, d time.Duration) bool // false si ctx est annule
	Lieu       *time.Location                                  // fuseau de Heure (defaut : local)
	Journal    *slog.Logger
	// EnEchec est prevenu de chaque echec (webhook de la supervision) ; peut etre nil.
	EnEchec         func(err error)
	Instrumentation domain.Instrumentation
}

// Planificateur fait la sauvegarde quotidienne.
type Planificateur struct {
	o   Options
	log *slog.Logger
}

// horodatage dans le nom : UTC, trie comme le temps. Un autre fichier du dossier n'est jamais touche.
const formatNom = "20060102T150405Z"

var reNom = regexp.MustCompile(`^mytv-(\d{8}T\d{6}Z)\.db$`)

// Nouveau valide les reglages et prepare le dossier (cree, droits 700).
func Nouveau(o Options) (*Planificateur, error) {
	if o.Copier == nil {
		return nil, errors.New("sauvegarde : fonction de copie absente")
	}
	if o.Dossier == "" || o.Conservation < 1 {
		return nil, errors.New("sauvegarde : dossier et conservation (>= 1 jour) sont requis")
	}
	if o.Heure != "" {
		if _, err := time.Parse("15:04", o.Heure); err != nil {
			return nil, fmt.Errorf("sauvegarde : heure %q invalide (HH:MM)", o.Heure)
		}
	}
	if o.Maintenant == nil {
		o.Maintenant = time.Now
	}
	if o.Attendre == nil {
		o.Attendre = attendreReel
	}
	if o.Lieu == nil {
		o.Lieu = time.Local
	}
	if o.Journal == nil {
		o.Journal = slog.New(slog.DiscardHandler)
	}
	if o.Instrumentation == nil {
		o.Instrumentation = domain.SansInstrumentation{}
	}
	return &Planificateur{o: o, log: o.Journal}, nil
}

func attendreReel(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(max(d, 0))
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// preparerDossier : droits 700 meme si le dossier existait deja (il contient toute la
// base, mots de passe hachees comprises).
func (p *Planificateur) preparerDossier() error {
	if err := os.MkdirAll(p.o.Dossier, 0o700); err != nil {
		return err
	}
	return os.Chmod(p.o.Dossier, 0o700)
}

// Sauvegarder fait UNE sauvegarde maintenant, puis purge les anciennes. Rend le chemin
// du fichier. Un echec est journalise, compte, et signale a EnEchec.
func (p *Planificateur) Sauvegarder(ctx context.Context) (string, error) {
	debut := p.o.Maintenant()
	fichier, err := p.sauvegarder(ctx, debut)
	if err != nil {
		if ctx.Err() == nil { // un arret demande n'est pas une panne
			p.o.Instrumentation.Compter("sauvegarde_total", 1, "resultat", "echec")
			p.log.Error("sauvegarde echouee", "erreur", err)
			if p.o.EnEchec != nil {
				p.o.EnEchec(err)
			}
		}
		return "", err
	}
	p.o.Instrumentation.Compter("sauvegarde_total", 1, "resultat", "ok")
	p.purger(debut)
	return fichier, nil
}

func (p *Planificateur) sauvegarder(ctx context.Context, maintenant time.Time) (string, error) {
	if err := p.preparerDossier(); err != nil {
		return "", fmt.Errorf("sauvegarde : dossier : %w", err)
	}
	fichier := filepath.Join(p.o.Dossier, "mytv-"+maintenant.UTC().Format(formatNom)+".db")
	octets, err := p.o.Copier(ctx, fichier)
	if err != nil {
		return "", err
	}
	p.log.Info("sauvegarde ecrite", "fichier", filepath.Base(fichier), "octets", octets,
		"duree", p.o.Maintenant().Sub(maintenant).Round(time.Millisecond))
	return fichier, nil
}

// Purger supprime les sauvegardes de plus de Conservation jours, sauf la plus recente
// (toujours gardee : si les sauvegardes s'arretent, la derniere reste). Seuls les
// fichiers « mytv-<horodatage>.db » sont touches, et les restes « .tmp » abandonnes.
func (p *Planificateur) purger(maintenant time.Time) {
	entrees, err := os.ReadDir(p.o.Dossier)
	if err != nil {
		p.log.Warn("sauvegarde : purge impossible", "erreur", err)
		return
	}
	type sv struct {
		nom string
		t   time.Time
	}
	var svs []sv
	for _, e := range entrees {
		if e.IsDir() {
			continue
		}
		nom := e.Name()
		if m := reNom.FindStringSubmatch(nom); m != nil {
			if t, err := time.Parse(formatNom, m[1]); err == nil {
				svs = append(svs, sv{nom, t})
			}
			continue
		}
		// Reste d'une copie interrompue (le service s'est arrete en plein VACUUM).
		if len(nom) > 4 && nom[len(nom)-4:] == ".tmp" && reNom.MatchString(nom[:len(nom)-4]) {
			if info, err := e.Info(); err == nil && maintenant.Sub(info.ModTime()) > time.Hour {
				_ = os.Remove(filepath.Join(p.o.Dossier, nom))
			}
		}
	}
	sort.Slice(svs, func(i, j int) bool { return svs[i].t.After(svs[j].t) })
	limite := maintenant.Add(-time.Duration(p.o.Conservation) * 24 * time.Hour)
	for i, s := range svs {
		if i == 0 || !s.t.Before(limite) {
			continue
		}
		if err := os.Remove(filepath.Join(p.o.Dossier, s.nom)); err != nil {
			p.log.Warn("sauvegarde : ancienne sauvegarde non supprimee", "fichier", s.nom, "erreur", err)
			continue
		}
		p.log.Info("ancienne sauvegarde supprimee", "fichier", s.nom)
	}
}

// Planifier fait la sauvegarde chaque jour a Heure, jusqu'a l'annulation de ctx. Sans
// Heure, rend la main tout de suite.
func (p *Planificateur) Planifier(ctx context.Context) {
	if p.o.Heure == "" {
		return
	}
	for {
		if !p.o.Attendre(ctx, p.prochaine().Sub(p.o.Maintenant())) {
			return
		}
		_, _ = p.Sauvegarder(ctx)
		if ctx.Err() != nil {
			return
		}
	}
}

// prochaine donne la prochaine occurrence STRICTEMENT future de Heure.
func (p *Planificateur) prochaine() time.Time {
	t, _ := time.Parse("15:04", p.o.Heure) // valide a la construction
	now := p.o.Maintenant().In(p.o.Lieu)
	c := time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, p.o.Lieu)
	if !c.After(now) {
		c = time.Date(now.Year(), now.Month(), now.Day()+1, t.Hour(), t.Minute(), 0, 0, p.o.Lieu)
	}
	return c
}
