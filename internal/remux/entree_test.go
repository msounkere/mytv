package remux

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mytv/internal/domain"
)

// film : des octets tous differents d'une position a l'autre (un recollage au mauvais
// endroit se voit).
func film(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*7 + i/251)
	}
	return b
}

// amontCoupe : une source qui sert `contenu` et coupe (ErrUnexpectedEOF) apres `coupeA`
// octets lors des `coupures` premieres ouvertures ; elle note chaque ouverture.
type amontCoupe struct {
	contenu    []byte
	coupeA     int
	coupures   int
	ouvertures atomic.Int32
	fermees    atomic.Int32
	departs    []int64
}

func (a *amontCoupe) source() domain.SourceOctets {
	return func(_ context.Context, depuis int64) (io.ReadCloser, error) {
		n := int(a.ouvertures.Add(1))
		a.departs = append(a.departs, depuis)
		reste := a.contenu[depuis:]
		var r io.Reader = bytes.NewReader(reste)
		if n <= a.coupures && len(reste) > a.coupeA {
			r = io.MultiReader(bytes.NewReader(reste[:a.coupeA]), &erreurLecture{io.ErrUnexpectedEOF})
		}
		return &corpsSuivi{Reader: r, fermees: &a.fermees}, nil
	}
}

type erreurLecture struct{ err error }

func (e *erreurLecture) Read([]byte) (int, error) { return 0, e.err }

type corpsSuivi struct {
	io.Reader
	fermees *atomic.Int32
	une     atomic.Bool
}

func (c *corpsSuivi) Close() error {
	if c.une.CompareAndSwap(false, true) {
		c.fermees.Add(1)
	}
	return nil
}

// US-17 CA1 : l'entree reprend l'amont a l'octet suivant apres une coupure : ffmpeg
// voit exactement le film, sans octet saute ni repete.
func TestCA1_EntreeReprendALOctetSuivant(t *testing.T) {
	a := &amontCoupe{contenu: film(100_000), coupeA: 30_000, coupures: 2}
	e := nouvelleEntree(context.Background(), a.source(), 3)
	defer e.Close()
	got, err := io.ReadAll(e)
	if err != nil || !bytes.Equal(got, a.contenu) {
		t.Fatalf("%d octets lus (attendu %d), erreur %v", len(got), len(a.contenu), err)
	}
	if a.ouvertures.Load() != 3 {
		t.Errorf("%d ouvertures, attendu 3 (1 + 2 reprises)", a.ouvertures.Load())
	}
	if a.departs[1] != 30_000 || a.departs[2] != 60_000 {
		t.Errorf("departs des reprises : %v (attendu 30000 puis 60000)", a.departs)
	}
	e.Close()
	if a.fermees.Load() != a.ouvertures.Load() {
		t.Errorf("%d corps ouverts, %d fermes", a.ouvertures.Load(), a.fermees.Load())
	}
}

// Les reprises sont bornees (remux.reprises_max) : au-dela, l'erreur remonte a ffmpeg
// (flux tronque) au lieu de boucler sur un fournisseur qui coupe toujours.
func TestEntreeReprisesEpuisees(t *testing.T) {
	a := &amontCoupe{contenu: film(100_000), coupeA: 10_000, coupures: 99}
	e := nouvelleEntree(context.Background(), a.source(), 2)
	defer e.Close()
	got, err := io.ReadAll(e)
	if !errors.Is(err, io.ErrUnexpectedEOF) || len(got) != 30_000 {
		t.Fatalf("%d octets, erreur %v (attendu 30000 et ErrUnexpectedEOF)", len(got), err)
	}
	if a.ouvertures.Load() != 3 {
		t.Errorf("%d ouvertures", a.ouvertures.Load())
	}
}

// Sans reprise autorisee, la premiere coupure remonte.
func TestEntreeSansReprise(t *testing.T) {
	a := &amontCoupe{contenu: film(1000), coupeA: 100, coupures: 1}
	e := nouvelleEntree(context.Background(), a.source(), 0)
	defer e.Close()
	if _, err := io.ReadAll(e); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("erreur = %v", err)
	}
}

// Une ouverture refusee (404, 407...) est retentee dans la limite des reprises, puis remonte.
func TestEntreeOuvertureRefusee(t *testing.T) {
	var n atomic.Int32
	refus := errors.New("refus du fournisseur")
	src := func(_ context.Context, depuis int64) (io.ReadCloser, error) {
		if n.Add(1) < 3 {
			return nil, refus
		}
		return io.NopCloser(strings.NewReader("ok")), nil
	}
	e := nouvelleEntree(context.Background(), src, 2)
	defer e.Close()
	got, err := io.ReadAll(e)
	if err != nil || string(got) != "ok" {
		t.Fatalf("%q %v", got, err)
	}
	e2 := nouvelleEntree(context.Background(), func(context.Context, int64) (io.ReadCloser, error) { return nil, refus }, 1)
	defer e2.Close()
	if _, err := io.ReadAll(e2); !errors.Is(err, refus) {
		t.Errorf("erreur = %v", err)
	}
}

// Fermer l'entree debloque une lecture amont en attente et ne declenche aucune reprise
// (un client parti ne consomme pas de connexion du fournisseur).
func TestEntreeFermerDebloqueEtNeReprendPas(t *testing.T) {
	var ouvertures atomic.Int32
	bloque := make(chan struct{})
	src := func(ctx context.Context, _ int64) (io.ReadCloser, error) {
		ouvertures.Add(1)
		return &corpsBloquant{ctx: ctx, bloque: bloque}, nil
	}
	e := nouvelleEntree(context.Background(), src, 5)
	fin := make(chan error, 1)
	go func() { _, err := e.Read(make([]byte, 10)); fin <- err }()
	attendre(t, func() bool { return ouvertures.Load() == 1 }, "lecture amont ouverte")
	e.Close()
	select {
	case err := <-fin:
		if err == nil {
			t.Error("la lecture bloquee a reussi")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Close ne debloque pas Read")
	}
	if ouvertures.Load() != 1 {
		t.Errorf("%d ouvertures : reprise apres fermeture", ouvertures.Load())
	}
	if _, err := e.Read(make([]byte, 1)); err == nil {
		t.Error("Read apres Close")
	}
}

type corpsBloquant struct {
	ctx    context.Context
	bloque chan struct{}
}

func (c *corpsBloquant) Read([]byte) (int, error) {
	select {
	case <-c.bloque:
		return 0, io.EOF
	case <-c.ctx.Done():
		return 0, c.ctx.Err()
	}
}
func (c *corpsBloquant) Close() error { return nil }

// ---------------------------------------------------------------- avec un vrai processus

// fauxFfmpeg : lit l'adresse locale qu'on lui donne apres `-i` (comme le vrai ffmpeg) et
// la copie vers sa sortie (un « remux » identite), et consigne sa ligne de commande.
func fauxFfmpeg(t *testing.T, dossier string) (chemin, argsf string) {
	t.Helper()
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl absent")
	}
	argsf = filepath.Join(dossier, "args")
	chemin = script(t, `printf '%s\n' "$@" > `+argsf+`
while [ "$1" != "-i" ]; do shift; done
exec curl -s "$2"`)
	return
}

// US-17 CA1 : de bout en bout avec un vrai processus et le vrai Lanceur. Les octets du
// film arrivent a ffmpeg par une adresse LOCALE a usage unique, y compris au travers
// d'une coupure de l'amont (reprise Range cote Go), et la ligne de commande ne contient
// ni hote du fournisseur ni identifiants.
func TestCA1_LanceurAlimenteFfmpegParAdresseLocale(t *testing.T) {
	prog, argsf := fauxFfmpeg(t, t.TempDir())
	a := &amontCoupe{contenu: film(200_000), coupeA: 70_000, coupures: 1}
	l, err := NouveauLanceur(Reglages{Ffmpeg: prog, ProcessusMax: 1, Reprises: 2}, ExecOS{})
	if err != nil {
		t.Fatal(err)
	}
	p, err := l.Lancer(context.Background(), domain.TacheRemux{Source: a.source(), Mode: domain.ModeAudioConverti, Debut: 90 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	sortie, err := io.ReadAll(p.Sortie())
	if err != nil || !bytes.Equal(sortie, a.contenu) {
		t.Fatalf("ffmpeg a recu/rendu %d octets (attendu %d), erreur %v", len(sortie), len(a.contenu), err)
	}
	if err := p.Attendre(); err != nil {
		t.Errorf("fin : %v", err)
	}
	attendre(t, func() bool { return l.EnCours() == 0 }, "emplacement rendu")
	if a.ouvertures.Load() != 2 || a.departs[1] != 70_000 {
		t.Errorf("ouvertures %d, departs %v", a.ouvertures.Load(), a.departs)
	}
	args, _ := os.ReadFile(argsf)
	ligne := string(args)
	if strings.Contains(ligne, "pipe:0") || !strings.Contains(ligne, "http://127.0.0.1:") {
		t.Errorf("ligne de commande : %q", ligne)
	}
	for _, interdit := range []string{"@", "movie", "fournisseur", "https"} {
		if strings.Contains(ligne, interdit) {
			t.Errorf("la ligne de commande contient %q : %q", interdit, ligne)
		}
	}
}

// Un ffmpeg qui meurt sans avoir tout lu ne laisse ni connexion amont ouverte ni goroutine.
func TestCA1_FfmpegMortAvantLaFinLibereLAmont(t *testing.T) {
	avant := runtime.NumGoroutine()
	prog := script(t, `while [ "$1" != "-i" ]; do shift; done
curl -s "$2" | head -c 1000 > /dev/null; exit 1`)
	a := &amontCoupe{contenu: film(5_000_000)}
	l, _ := NouveauLanceur(Reglages{Ffmpeg: prog, ProcessusMax: 1}, ExecOS{})
	p, err := l.Lancer(context.Background(), domain.TacheRemux{Source: a.source(), Mode: domain.ModeRemux})
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, p.Sortie())
	if err := p.Attendre(); err == nil {
		t.Error("l'echec de ffmpeg doit remonter")
	}
	attendre(t, func() bool {
		return l.EnCours() == 0 && a.fermees.Load() == a.ouvertures.Load() && a.ouvertures.Load() >= 1
	}, "amont ferme, emplacement rendu")
	p.Arreter()
	sansGoroutine(t, avant)
}

// Arreter (client parti) coupe un amont qui ne repond plus et un ffmpeg qui attend : rien ne reste.
func TestCA1_ArreterCoupeUnAmontBloque(t *testing.T) {
	avant := runtime.NumGoroutine()
	prog := script(t, `while [ "$1" != "-i" ]; do shift; done
exec curl -s "$2"`)
	bloque := make(chan struct{})
	var ouv atomic.Int32
	src := func(ctx context.Context, _ int64) (io.ReadCloser, error) {
		ouv.Add(1)
		return &corpsBloquant{ctx: ctx, bloque: bloque}, nil
	}
	l, _ := NouveauLanceur(Reglages{Ffmpeg: prog, ProcessusMax: 1, Reprises: 3}, ExecOS{DelaiArret: time.Second})
	p, err := l.Lancer(context.Background(), domain.TacheRemux{Source: src, Mode: domain.ModeRemux})
	if err != nil {
		t.Fatal(err)
	}
	attendre(t, func() bool { return ouv.Load() == 1 }, "amont ouvert")
	debut := time.Now()
	p.Arreter()
	if d := time.Since(debut); d > 3*time.Second {
		t.Errorf("Arreter a pris %s", d)
	}
	attendre(t, func() bool { return l.EnCours() == 0 }, "emplacement rendu")
	if ouv.Load() != 1 {
		t.Errorf("%d ouvertures : reprise apres Arreter", ouv.Load())
	}
	sansGoroutine(t, avant)
}

func sansGoroutine(t *testing.T, avant int) {
	t.Helper()
	fin := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > avant {
		if time.Now().After(fin) {
			buf := make([]byte, 1<<16)
			t.Fatalf("goroutines restantes : %d avant, %d apres\n%s", avant, runtime.NumGoroutine(), buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// La fin du film n'est pas perdue quand ffmpeg se termine avant que le client ait vide le
// tube : le lanceur constate la mort du processus tout de suite, mais ne ferme pas la
// sortie sous les pieds du consommateur (jusqu'a 64 Ko se perdaient en fin de lecture).
func TestFinDeSortieNonPerdueSiLeClientEstLent(t *testing.T) {
	prog := script(t, `head -c 60000 /dev/zero | tr '\0' 'x'`)
	l, _ := NouveauLanceur(Reglages{Ffmpeg: prog, ProcessusMax: 1}, ExecOS{})
	p, err := l.Lancer(context.Background(), domain.TacheRemux{Source: sourceOctets("peu importe"), Mode: domain.ModeRemux})
	if err != nil {
		t.Fatal(err)
	}
	attendre(t, func() bool { return l.EnCours() == 0 }, "processus mort et emplacement rendu")
	time.Sleep(100 * time.Millisecond) // le client n'a encore rien lu
	b, err := io.ReadAll(p.Sortie())
	if err != nil || len(b) != 60000 {
		t.Fatalf("%d octets lus au lieu de 60000 : %v", len(b), err)
	}
	p.Sortie().Close()
	p.Arreter()
}

func goroutines() int { return runtime.NumGoroutine() }
