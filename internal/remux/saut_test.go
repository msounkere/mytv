package remux

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"mytv/internal/domain"
)

// srcComptee : un film en memoire servi sur Range, qui compte les octets reellement
// remis a ffmpeg.
type srcComptee struct {
	contenu []byte
	remis   atomic.Int64
}

func (s *srcComptee) source() domain.SourceOctets {
	return func(_ context.Context, depuis int64) (io.ReadCloser, error) {
		if depuis > int64(len(s.contenu)) {
			return nil, domain.ErrPlageHorsFichier{Total: int64(len(s.contenu))}
		}
		return &corpsCompte{Reader: bytes.NewReader(s.contenu[depuis:]), s: s, total: int64(len(s.contenu))}, nil
	}
}

type corpsCompte struct {
	io.Reader
	s     *srcComptee
	total int64
}

func (c *corpsCompte) Read(p []byte) (int, error) {
	n, err := c.Reader.Read(p)
	c.s.remis.Add(int64(n))
	return n, err
}
func (c *corpsCompte) Close() error  { return nil }
func (c *corpsCompte) Taille() int64 { return c.total }

// Regression (recette reelle, 2026-10-17) : reprendre une serie a 14 min faisait « tourner
// le lecteur sans s'ouvrir ». ffmpeg lisait un tube : `-ss` LISAIT ET JETAIT tout ce qui
// precede la position, donc des centaines de Mo retelecharges avant la premiere image.
// Avec une source locale qui sert des plages, ffmpeg lit l'index (fin de fichier) puis saute :
// il ne demande qu'une petite partie du film.
func TestSautDansUnFilmNeRetelechargePasToutLeFilm(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg absent")
	}
	dir := t.TempDir()
	mkv := filepath.Join(dir, "film.mkv")
	// 60 s de video H.264 a debit eleve (bruit ajoute) : le film doit peser BIEN plus que
	// les tampons reseau de la boucle locale (quelques Mo), sinon la mesure ne dit rien.
	gen := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=960x540:rate=25,noise=alls=8:allf=t",
		"-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "60", "-c:v", "libx264", "-preset", "ultrafast",
		"-crf", "20", "-g", "50", "-c:a", "aac", "-b:a", "128k", mkv)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("generation du film impossible : %v %s", err, out)
	}
	contenu, err := os.ReadFile(mkv)
	if err != nil || len(contenu) < 40_000_000 {
		t.Skipf("film trop petit pour le test : %d octets", len(contenu))
	}

	src := &srcComptee{contenu: contenu}
	l, err := NouveauLanceur(Reglages{Ffmpeg: ffmpeg, ProcessusMax: 1, Reprises: 2}, ExecOS{})
	if err != nil {
		t.Fatal(err)
	}
	debut := time.Now()
	p, err := l.Lancer(context.Background(), domain.TacheRemux{Source: src.source(), Mode: domain.ModeAudioConverti, Debut: 50 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Arreter()
	tampon := make([]byte, 64<<10)
	n, err := io.ReadFull(p.Sortie(), tampon)
	if err != nil || n < len(tampon) {
		t.Fatalf("pas de sortie (%d octets, %v)", n, err)
	}
	delai := time.Since(debut)
	p.Arreter()

	remis := src.remis.Load()
	part := float64(remis) / float64(len(contenu))
	t.Logf("film %d Ko ; remis a ffmpeg %d Ko (%.0f %%) ; premiere sortie en %s", len(contenu)/1024, remis/1024, part*100, delai)
	// Sauter a 50 s sur 60 s : on attend l'index + les dernieres secondes, bien moins que
	// le film entier. Lire-et-jeter (tube) en remettrait pres de 100 %.
	if part > 0.40 {
		t.Errorf("ffmpeg a recu %.0f %% du film pour demarrer a 50 s (saut impossible : lecture sequentielle ?)", part*100)
	}
}
