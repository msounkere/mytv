package remux

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"mytv/internal/domain"
)

// script ecrit un faux programme (ffmpeg / ffprobe de test) dans un dossier temporaire.
func script(t *testing.T, corps string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "faux")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+corps+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func vivant(pid int) bool { return syscall.Kill(pid, 0) == nil }

func lirePID(t *testing.T, fichier string) int {
	t.Helper()
	var pid int
	attendre(t, func() bool {
		b, err := os.ReadFile(fichier)
		if err != nil {
			return false
		}
		pid, err = strconv.Atoi(strings.TrimSpace(string(b)))
		return err == nil
	}, "fichier de pid")
	return pid
}

// CA6 : faux ffmpeg qui produit des octets puis se termine.
func TestExecOSDemarrerLitLaSortie(t *testing.T) {
	prog := script(t, `printf 'ftypFAUX'; echo "erreur http://u:motdepasse@x.test/a" >&2`)
	p, err := ExecOS{}.Demarrer(context.Background(), prog, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(p.Sortie())
	if err != nil || string(b) != "ftypFAUX" {
		t.Fatalf("sortie %q, %v", b, err)
	}
	if err := p.Attendre(); err != nil {
		t.Errorf("fin normale : %v", err)
	}
	p.Sortie().Close()
}

// Les erreurs du programme ne contiennent jamais l'URL (identifiants).
func TestExecOSErreurSansURL(t *testing.T) {
	prog := script(t, `echo "Error opening http://utilisateur:motdepasse@x.test/movie/u/motdepasse/1.mkv" >&2; exit 3`)
	p, err := ExecOS{}.Demarrer(context.Background(), prog, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, p.Sortie())
	err = p.Attendre()
	if err == nil {
		t.Fatal("echec attendu")
	}
	if strings.Contains(err.Error(), "motdepasse") || !strings.Contains(err.Error(), "<url>") {
		t.Errorf("erreur = %v", err)
	}
	_, err = ExecOS{}.Executer(context.Background(), prog, nil, 100)
	if err == nil || strings.Contains(err.Error(), "motdepasse") {
		t.Errorf("Executer : %v", err)
	}
}

func TestExecOSProgrammeAbsent(t *testing.T) {
	if _, err := (ExecOS{}).Demarrer(context.Background(), "/nexiste/pas", nil, nil); err == nil {
		t.Error("pas d'erreur")
	}
	if _, err := (ExecOS{}).Executer(context.Background(), "/nexiste/pas", nil, 10); err == nil {
		t.Error("pas d'erreur")
	}
}

// CA3 : Arreter tue le processus ET son enfant, rend la main vite, et ne laisse
// aucun zombie.
func TestExecOSArreterTueLeGroupe(t *testing.T) {
	pidf := filepath.Join(t.TempDir(), "enfant.pid")
	prog := script(t, `sleep 300 &
echo $! > `+pidf+`
printf debut
wait`)
	p, err := ExecOS{DelaiArret: time.Second}.Demarrer(context.Background(), prog, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	enfant := lirePID(t, pidf)
	buf := make([]byte, 5)
	if _, err := io.ReadFull(p.Sortie(), buf); err != nil {
		t.Fatal(err)
	}
	debut := time.Now()
	p.Arreter()
	if d := time.Since(debut); d > 3*time.Second {
		t.Errorf("Arreter a pris %v", d)
	}
	attendre(t, func() bool { return !vivant(enfant) }, "enfant tue")
	if err := p.Attendre(); err != nil {
		t.Errorf("Attendre apres Arreter : %v", err)
	}
	p.Arreter() // idempotent
}

// Un programme qui ignore SIGTERM est tue par SIGKILL apres le delai d'arret.
func TestExecOSSigkillSiSigtermIgnore(t *testing.T) {
	prog := script(t, `trap '' TERM
printf pret
while :; do sleep 1; done`)
	p, err := ExecOS{DelaiArret: 100 * time.Millisecond}.Demarrer(context.Background(), prog, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadFull(p.Sortie(), make([]byte, 4))
	debut := time.Now()
	p.Arreter()
	if time.Since(debut) > 3*time.Second {
		t.Error("SIGKILL non envoye")
	}
}

// CA3 : l'annulation du contexte (client parti) tue le groupe de processus.
func TestExecOSContexteAnnuleTueLeProcessus(t *testing.T) {
	pidf := filepath.Join(t.TempDir(), "enfant.pid")
	prog := script(t, `sleep 300 &
echo $! > `+pidf+`
wait`)
	ctx, annule := context.WithCancel(context.Background())
	p, err := ExecOS{DelaiArret: time.Second}.Demarrer(ctx, prog, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	enfant := lirePID(t, pidf)
	annule()
	done := make(chan struct{})
	go func() { p.Attendre(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Attendre ne rend pas la main apres l'annulation")
	}
	attendre(t, func() bool { return !vivant(enfant) }, "enfant tue")
}

func TestExecOSExecuter(t *testing.T) {
	prog := script(t, `printf '{"ok":true}'`)
	b, err := ExecOS{}.Executer(context.Background(), prog, nil, 100)
	if err != nil || string(b) != `{"ok":true}` {
		t.Errorf("%q %v", b, err)
	}
	_, err = ExecOS{}.Executer(context.Background(), prog, nil, 3)
	if !errors.Is(err, ErrSortieTropGrosse) {
		t.Errorf("sortie trop grosse : %v", err)
	}
	lent := script(t, `sleep 300`)
	ctx, annule := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer annule()
	debut := time.Now()
	_, err = ExecOS{DelaiArret: 100 * time.Millisecond}.Executer(ctx, lent, nil, 10)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(debut) > 3*time.Second {
		t.Errorf("delai : %v apres %v", err, time.Since(debut))
	}
}

// Les arguments arrivent tels quels au programme (tableau, pas de shell).
func TestExecOSArgumentsSansShell(t *testing.T) {
	sortie := filepath.Join(t.TempDir(), "args")
	prog := script(t, `for a in "$@"; do printf '%s\n' "$a" >> `+sortie+`; done`)
	piege := "http://x.test/a;touch " + filepath.Join(t.TempDir(), "PWNED") + "$(id)"
	if _, err := (ExecOS{}).Executer(context.Background(), prog, []string{"-i", piege, "&& echo"}, 10); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(sortie)
	if got := strings.Split(strings.TrimSpace(string(b)), "\n"); len(got) != 3 || got[1] != piege {
		t.Errorf("arguments recus : %q", got)
	}
}

// CA3+CA6 : de bout en bout avec un faux ffmpeg, Lanceur reel et ExecOS reel.
func TestLanceurAvecFauxFfmpeg(t *testing.T) {
	argsf := filepath.Join(t.TempDir(), "args")
	prog := script(t, `printf '%s\n' "$@" > `+argsf+`
printf 'ftypmp42'`)
	l, err := NouveauLanceur(Reglages{Ffmpeg: prog, ProcessusMax: 1, FileMax: 0}, ExecOS{})
	if err != nil {
		t.Fatal(err)
	}
	p, err := l.Lancer(context.Background(), tache())
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(p.Sortie())
	if string(b) != "ftypmp42" || p.Attendre() != nil {
		t.Errorf("sortie %q", b)
	}
	attendre(t, func() bool { return l.EnCours() == 0 }, "emplacement rendu a la mort du processus")
	a, _ := os.ReadFile(argsf)
	if !bytes.Contains(a, []byte("-f\nmp4\npipe:1")) {
		t.Errorf("arguments : %s", a)
	}
}

// CA6 : test reel avec les vrais ffmpeg et ffprobe, ignore en -short ou s'ils manquent.
// Le « fournisseur » est un serveur local qui sert un MKV (h264 + eac3) genere par ffmpeg.
func TestReelSondeEtRemux(t *testing.T) {
	if testing.Short() {
		t.Skip("-short")
	}
	ffmpeg, err1 := exec.LookPath("ffmpeg")
	ffprobe, err2 := exec.LookPath("ffprobe")
	if err1 != nil || err2 != nil {
		t.Skip("ffmpeg/ffprobe absents")
	}
	fichier := filepath.Join(t.TempDir(), "film.mkv")
	gen := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "testsrc=size=160x120:rate=10:duration=3",
		"-f", "lavfi", "-i", "sine=duration=3", "-c:v", "libx264", "-c:a", "eac3", "-y", fichier)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("generation impossible (encodeurs absents ?) : %v %s", err, out)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, fichier)
	}))
	defer srv.Close()
	url := srv.URL + "/film.mkv"
	source := func(ctx context.Context, depuis int64) (io.ReadCloser, error) {
		req, _ := http.NewRequestWithContext(ctx, "GET", url, nil)
		if depuis > 0 {
			req.Header.Set("Range", "bytes="+strconv.FormatInt(depuis, 10)+"-")
		}
		rep, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		return &avecTailleTest{ReadCloser: rep.Body, total: depuis + rep.ContentLength}, nil
	}

	s := NouveauSondeur(ReglagesSonde{Ffprobe: ffprobe, Duree: time.Hour, Delai: 20 * time.Second, Max: 5}, ExecOS{}, nil)
	sonde, err := s.Sonder(context.Background(), "iptva:1", source)
	if err != nil {
		t.Fatal(err)
	}
	if sonde.CodecVideo != "h264" || sonde.CodecAudio != "eac3" {
		t.Fatalf("sonde = %+v", sonde)
	}
	d, err := Decider(sonde)
	if err != nil {
		t.Fatal(err)
	}
	if d.Mode != domain.ModeAudioConverti {
		t.Fatalf("decision = %+v", d)
	}
	l, _ := NouveauLanceur(Reglages{Ffmpeg: ffmpeg, ProcessusMax: 1}, ExecOS{})
	p, err := l.Lancer(context.Background(), domain.TacheRemux{Source: source, Mode: d.Mode})
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(p.Sortie())
	if err != nil || len(b) < 100 || string(b[4:8]) != "ftyp" {
		t.Fatalf("sortie : %d octets, %v", len(b), err)
	}
	if err := p.Attendre(); err != nil {
		t.Errorf("ffmpeg : %v", err)
	}
}

type avecTailleTest struct {
	io.ReadCloser
	total int64
}

func (a *avecTailleTest) Taille() int64 { return a.total }

// Un film MPEG-TS a l'audio AAC (ADTS) recopie dans le mp4 : depuis ffmpeg 8, le muxeur
// refuse l'ADTS (« Malformed AAC bitstream », sortie 255) sans aac_adtstoasc (film
// iptva:1283517, 2026-11-10). Test reel, ignore en -short ou sans ffmpeg/ffprobe.
func TestReelRemuxMpegtsAAC(t *testing.T) {
	if testing.Short() {
		t.Skip("-short")
	}
	ffmpeg, err1 := exec.LookPath("ffmpeg")
	ffprobe, err2 := exec.LookPath("ffprobe")
	if err1 != nil || err2 != nil {
		t.Skip("ffmpeg/ffprobe absents")
	}
	fichier := filepath.Join(t.TempDir(), "film.ts")
	gen := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "testsrc=size=160x120:rate=10:duration=3",
		"-f", "lavfi", "-i", "sine=duration=3", "-c:v", "libx264", "-c:a", "aac", "-f", "mpegts", "-y", fichier)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("generation impossible (encodeurs absents ?) : %v %s", err, out)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, fichier)
	}))
	defer srv.Close()
	source := func(ctx context.Context, depuis int64) (io.ReadCloser, error) {
		req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/film.ts", nil)
		if depuis > 0 {
			req.Header.Set("Range", "bytes="+strconv.FormatInt(depuis, 10)+"-")
		}
		rep, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, err
		}
		return &avecTailleTest{ReadCloser: rep.Body, total: depuis + rep.ContentLength}, nil
	}
	s := NouveauSondeur(ReglagesSonde{Ffprobe: ffprobe, Duree: time.Hour, Delai: 20 * time.Second, Max: 5}, ExecOS{}, nil)
	sonde, err := s.Sonder(context.Background(), "iptva:2", source)
	if err != nil {
		t.Fatal(err)
	}
	if sonde.CodecAudio != "aac" {
		t.Fatalf("sonde = %+v", sonde)
	}
	d, err := Decider(sonde)
	if err != nil || d.Mode != domain.ModeRemux {
		t.Fatalf("decision = %+v, %v", d, err)
	}
	l, _ := NouveauLanceur(Reglages{Ffmpeg: ffmpeg, ProcessusMax: 1}, ExecOS{})
	p, err := l.Lancer(context.Background(), domain.TacheRemux{Source: source, Mode: d.Mode, AudioAAC: true})
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(p.Sortie())
	if err != nil || len(b) < 1000 || string(b[4:8]) != "ftyp" {
		t.Fatalf("sortie : %d octets, %v", len(b), err)
	}
	if err := p.Attendre(); err != nil {
		t.Errorf("ffmpeg : %v", err)
	}
}

// Replay (vod/replay.go) : flux MPEG-TS SANS taille ni Range (lu d'un trait, comme le timeshift
// du fournisseur), audio AC3 converti en AAC, depart a 5 s (le reste de moins d'une minute
// apres une redemande a la bonne minute). ffmpeg doit sauter en lisant, sans demander de plage.
func TestReelReplayFluxSansTailleAvecSaut(t *testing.T) {
	if testing.Short() {
		t.Skip("-short")
	}
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg absent")
	}
	fichier := filepath.Join(t.TempDir(), "replay.ts")
	gen := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "testsrc=size=160x120:rate=10:duration=8",
		"-f", "lavfi", "-i", "sine=duration=8", "-c:v", "libx264", "-g", "10", "-c:a", "ac3", "-f", "mpegts", "-y", fichier)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("generation impossible (encodeurs absents ?) : %v %s", err, out)
	}
	var plages atomic.Int32
	source := func(ctx context.Context, depuis int64) (io.ReadCloser, error) {
		if depuis > 0 {
			plages.Add(1)
			return nil, errors.New("pas de reprise sur un replay")
		}
		return os.Open(fichier) // ni taille (pas d'AvecTaille) ni Range
	}
	l, _ := NouveauLanceur(Reglages{Ffmpeg: ffmpeg, ProcessusMax: 1}, ExecOS{})
	p, err := l.Lancer(context.Background(), domain.TacheRemux{Source: source, Mode: domain.ModeAudioConverti, Debut: 5 * time.Second, SautEnLecture: true})
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(p.Sortie())
	if err != nil || len(b) < 1000 || string(b[4:8]) != "ftyp" {
		t.Fatalf("sortie : %d octets, %v", len(b), err)
	}
	if err := p.Attendre(); err != nil {
		t.Errorf("ffmpeg : %v", err)
	}
	if n := plages.Load(); n != 0 {
		t.Errorf("ffmpeg a demande %d reprise(s) au milieu du flux", n)
	}
	sortie := filepath.Join(t.TempDir(), "sortie.mp4")
	if err := os.WriteFile(sortie, b, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(strings.TrimSuffix(ffmpeg, "ffmpeg")+"ffprobe", "-v", "error", "-show_entries",
		"stream=codec_name:format=duration", "-of", "csv=p=0", sortie).CombinedOutput()
	if err != nil {
		t.Fatalf("ffprobe : %v %s", err, out)
	}
	txt := string(out)
	if !strings.Contains(txt, "h264") || !strings.Contains(txt, "aac") {
		t.Errorf("codecs de sortie : %s", txt)
	}
	// 8 s de source, depart a 5 s : ~3 s (a une image-cle pres).
	var duree float64
	for _, l := range strings.Split(strings.TrimSpace(txt), "\n") {
		if v, err := strconv.ParseFloat(strings.TrimSpace(l), 64); err == nil {
			duree = v
		}
	}
	if duree < 2 || duree > 4.5 {
		t.Errorf("duree de sortie %.2f s, attendu ~3 s (saut de 5 s sur 8)", duree)
	}
}
