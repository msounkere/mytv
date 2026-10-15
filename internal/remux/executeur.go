package remux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"sync"
	"syscall"
	"time"

	"mytv/internal/domain"
)

// Executeur lance des programmes externes. C'est la SEULE porte vers os/exec :
// les tests la remplacent par une doublure, et les arguments sont toujours un
// tableau (jamais de shell, donc jamais d'interpretation de metacaracteres).
type Executeur interface {
	// Executer lance le programme, attend sa fin et renvoie sa sortie standard,
	// tronquee a maxSortie octets (au-dela : erreur, pour ne pas gonfler la memoire).
	Executer(ctx context.Context, nom string, args []string, maxSortie int) ([]byte, error)
	// Demarrer lance le programme et rend la main tout de suite ; la sortie
	// standard se lit au fil de l'eau. Annuler ctx tue le programme ET ses enfants.
	// entree (peut etre nil) est copiee dans l'entree standard du programme ; elle est
	// fermee a la mort du programme, ce qui debloque une lecture en attente.
	Demarrer(ctx context.Context, nom string, args []string, entree io.ReadCloser) (domain.Processus, error)
}

// ErrSortieTropGrosse : le programme a produit plus que la borne demandee.
var ErrSortieTropGrosse = errors.New("remux : sortie du programme trop volumineuse")

// stderrMax borne ce qu'on garde de la sortie d'erreur d'un programme (pour le diagnostic).
const stderrMax = 2048

// urlDansTexte retire toute adresse d'un message : ffmpeg et ffprobe recopient
// l'URL amont — identifiants compris — dans leurs erreurs.
var urlDansTexte = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://\S+`)

// Assainir masque les URL d'un texte destine a une erreur ou a un journal.
func Assainir(s string) string { return urlDansTexte.ReplaceAllString(s, "<url>") }

// ExecOS est l'Executeur reel. DelaiArret est l'attente entre SIGTERM et SIGKILL.
type ExecOS struct {
	DelaiArret time.Duration
}

var _ Executeur = ExecOS{}

func (e ExecOS) delai() time.Duration {
	if e.DelaiArret <= 0 {
		return 5 * time.Second
	}
	return e.DelaiArret
}

// tamponBorne garde la FIN de ce qu'on lui ecrit (la cause d'un echec est en bas).
type tamponBorne struct {
	mu  sync.Mutex
	buf []byte
}

func (t *tamponBorne) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > stderrMax {
		t.buf = t.buf[len(t.buf)-stderrMax:]
	}
	return len(p), nil
}

func (t *tamponBorne) texte() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return Assainir(string(bytes.TrimSpace(t.buf)))
}

// preparer configure la commande : groupe de processus propre (pour tuer toute la
// descendance, sinon un ffmpeg qui a lance un enfant garderait la connexion du
// fournisseur ouverte) et arret du groupe a l'annulation du contexte.
func (e ExecOS) preparer(ctx context.Context, nom string, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, nom, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return tuerGroupe(cmd, syscall.SIGKILL) }
	cmd.WaitDelay = e.delai()
	return cmd
}

func tuerGroupe(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd.Process == nil {
		return nil
	}
	err := syscall.Kill(-cmd.Process.Pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

func (e ExecOS) Executer(ctx context.Context, nom string, args []string, maxSortie int) ([]byte, error) {
	cmd := e.preparer(ctx, nom, args)
	var sortie limiteur
	sortie.max = maxSortie
	var erreurs tamponBorne
	cmd.Stdout = &sortie
	cmd.Stderr = &erreurs
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if sortie.depasse {
			return nil, ErrSortieTropGrosse
		}
		return nil, fmt.Errorf("%s : %v : %s", nom, errSansChemin(err), erreurs.texte())
	}
	if sortie.depasse {
		return nil, ErrSortieTropGrosse
	}
	return sortie.buf.Bytes(), nil
}

// errSansChemin : les erreurs d'os/exec ne portent que le nom du programme et le
// code de sortie, mais on borne quand meme ce qu'on en retient.
func errSansChemin(err error) string { return Assainir(err.Error()) }

type limiteur struct {
	buf     bytes.Buffer
	max     int
	depasse bool
}

func (l *limiteur) Write(p []byte) (int, error) {
	if l.buf.Len()+len(p) > l.max {
		l.depasse = true
		return 0, ErrSortieTropGrosse // fait echouer la commande
	}
	return l.buf.Write(p)
}

// processusOS est un programme en cours, sortie standard lue par un tube dont
// le processus parent possede l'extremite de lecture : Wait ne le ferme pas, donc
// aucune donnee n'est perdue entre la fin du programme et la fin de la lecture.
type processusOS struct {
	cmd     *exec.Cmd
	sortie  *os.File
	erreurs *tamponBorne
	delai   time.Duration
	fini    chan struct{}
	err     error
}

func (e ExecOS) Demarrer(ctx context.Context, nom string, args []string, entree io.ReadCloser) (domain.Processus, error) {
	cmd := e.preparer(ctx, nom, args)
	lecture, ecriture, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	// Entree standard : un tube dont NOUS tenons l'extremite d'ecriture. (Donner un
	// io.Reader a cmd.Stdin ferait creer une goroutine par os/exec, que Wait attendrait
	// tant que la lecture amont bloque : on la maitrise ici.)
	var entreeLue, entreeEcrite *os.File
	if entree != nil {
		if entreeLue, entreeEcrite, err = os.Pipe(); err != nil {
			lecture.Close()
			ecriture.Close()
			return nil, err
		}
		cmd.Stdin = entreeLue
	}
	var erreurs tamponBorne
	cmd.Stdout = ecriture
	cmd.Stderr = &erreurs
	if err := cmd.Start(); err != nil {
		lecture.Close()
		ecriture.Close()
		if entree != nil {
			entreeLue.Close()
			entreeEcrite.Close()
			entree.Close()
		}
		return nil, fmt.Errorf("remux : demarrage de %s impossible : %s", nom, errSansChemin(err))
	}
	ecriture.Close() // le fils garde sa copie ; sans cela la lecture ne verrait jamais la fin
	p := &processusOS{cmd: cmd, sortie: lecture, erreurs: &erreurs, delai: e.delai(), fini: make(chan struct{})}
	copie := make(chan struct{})
	if entree != nil {
		entreeLue.Close() // le fils garde la sienne
		go func() {
			defer close(copie)
			defer entreeEcrite.Close() // fin de fichier pour ffmpeg
			// Une ecriture vers un ffmpeg mort rend EPIPE : la copie s'arrete.
			_, _ = io.Copy(entreeEcrite, entree)
		}()
	} else {
		close(copie)
	}
	go func() {
		// Un seul appel a Wait, quoi qu'il arrive : c'est ce qui recolte le
		// processus (pas de zombie) meme si personne n'appelle Attendre.
		err := cmd.Wait()
		if err != nil {
			err = fmt.Errorf("%s : %s : %s", nom, errSansChemin(err), erreurs.texte())
		}
		p.err = err
		if entree != nil {
			entree.Close() // debloque une lecture amont en attente
		}
		<-copie
		close(p.fini)
	}()
	return p, nil
}

func (p *processusOS) Sortie() io.ReadCloser { return p.sortie }

func (p *processusOS) Arreter() {
	select {
	case <-p.fini:
		return
	default:
	}
	_ = tuerGroupe(p.cmd, syscall.SIGTERM)
	t := time.NewTimer(p.delai)
	defer t.Stop()
	select {
	case <-p.fini:
	case <-t.C:
		_ = tuerGroupe(p.cmd, syscall.SIGKILL)
		<-p.fini
	}
	p.sortie.Close()
}

// Attendre bloque jusqu'a la mort du programme. Il NE ferme PAS la sortie : le
// lanceur l'appelle des que le programme meurt, alors que le consommateur n'a pas
// fini de vider le tube (jusqu'a 64 Ko restent en attente quand ffmpeg se termine).
// Fermer ici ferait perdre la fin du film. Le consommateur ferme la sortie
// (Sortie().Close) et Arreter la ferme aussi.
func (p *processusOS) Attendre() error {
	<-p.fini
	if p.err != nil && p.cmd.ProcessState != nil {
		if ws, ok := p.cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			// Tue par nos soins (SIGTERM/SIGKILL) : fin normale du point de vue de l'appelant.
			return nil
		}
	}
	return p.err
}
