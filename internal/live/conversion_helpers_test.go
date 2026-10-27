package live

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"mytv/internal/config"
	"mytv/internal/domain"
	"mytv/internal/remux"
	"mytv/internal/testutil/fauxiptv"
)

// Aides des tests de conversion du direct (US-30, US-31) : de VRAIS MPEG-TS generes par
// ffmpeg, un faux fournisseur qui les sert, de VRAIS ffprobe et ffmpeg lances par le code
// teste, sortie verifiee par ffprobe. Aucun flux reel n'est jamais touche.

type outils struct{ ffmpeg, ffprobe string }

func chercherOutils(t *testing.T) outils {
	t.Helper()
	if testing.Short() {
		t.Skip("-short")
	}
	ffmpeg, e1 := exec.LookPath("ffmpeg")
	ffprobe, e2 := exec.LookPath("ffprobe")
	if e1 != nil || e2 != nil {
		t.Skip("ffmpeg/ffprobe absents")
	}
	return outils{ffmpeg, ffprobe}
}

// formats de test : codecs, taille.
type formatTest struct {
	nom          string
	video, audio []string // options ffmpeg
	taille       string
	secondes     int
}

var (
	formatHEVCEAC3 = formatTest{"hevc_eac3", []string{"-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "keyint=25:min-keyint=25:scenecut=0:log-level=none"}, []string{"-c:a", "eac3"}, "640x360", 40}
	formatH264AC3  = formatTest{"h264_ac3", []string{"-c:v", "libx264", "-preset", "ultrafast", "-g", "25", "-sc_threshold", "0"}, []string{"-c:a", "ac3"}, "640x360", 40}
	formatH264AAC  = formatTest{"h264_aac", []string{"-c:v", "libx264", "-preset", "ultrafast", "-g", "25", "-sc_threshold", "0"}, []string{"-c:a", "aac"}, "640x360", 40}
	// 4K : une seconde suffit a la sonde (elle ne lit que l'en-tete) ; un seul segment.
	formatHEVC4K = formatTest{"hevc_4k", []string{"-c:v", "libx265", "-preset", "ultrafast", "-x265-params", "keyint=25:min-keyint=25:scenecut=0:log-level=none"}, []string{"-c:a", "eac3"}, "3840x2160", 2}
)

var (
	muSegments sync.Mutex
	segmentsDe = map[string][][]byte{}
)

// segmentsGeneres fabrique (une fois par execution) des segments MPEG-TS d'une seconde.
func segmentsGeneres(t *testing.T, o outils, f formatTest) [][]byte {
	t.Helper()
	muSegments.Lock()
	defer muSegments.Unlock()
	if s, ok := segmentsDe[f.nom]; ok {
		return s
	}
	dir, err := os.MkdirTemp("", "mytv-gen-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	args := []string{"-v", "error", "-f", "lavfi", "-i", "testsrc=size=" + f.taille + ":rate=25", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
		"-t", strconv.Itoa(f.secondes)}
	args = append(args, f.video...)
	args = append(args, f.audio...)
	args = append(args, "-pix_fmt", "yuv420p", "-f", "hls", "-hls_time", "1", "-hls_list_size", "0",
		"-hls_segment_filename", filepath.Join(dir, "s%03d.ts"), filepath.Join(dir, "i.m3u8"))
	if out, err := exec.Command(o.ffmpeg, args...).CombinedOutput(); err != nil {
		t.Skipf("generation impossible (encodeur absent ?) : %v %s", err, out)
	}
	fichiers, _ := filepath.Glob(filepath.Join(dir, "s*.ts"))
	sort.Strings(fichiers)
	var res [][]byte
	for _, p := range fichiers {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		res = append(res, b)
	}
	if len(res) == 0 {
		t.Fatal("aucun segment genere")
	}
	segmentsDe[f.nom] = res
	return res
}

// contenuParChaine : le fournisseur sert, pour chaque chaine, les segments du format choisi.
// Un nom « <chaine>_<n>.ts » donne le segment (n-1) modulo le nombre de segments.
func contenuParChaine(formats map[string][][]byte) func(string) ([]byte, bool) {
	return func(nom string) ([]byte, bool) {
		base := strings.TrimSuffix(nom, ".ts")
		i := strings.LastIndexByte(base, '_')
		if i < 0 {
			return nil, false
		}
		segs, ok := formats[base[:i]]
		if !ok {
			return nil, false
		}
		n, err := strconv.Atoi(base[i+1:])
		if err != nil || n < 1 {
			return nil, false
		}
		return segs[(n-1)%len(segs)], true
	}
}

// executeurEnregistre note chaque programme lance (nom + arguments) : c'est la preuve
// que ni URL ni identifiant n'est passe a ffmpeg ou ffprobe.
type executeurEnregistre struct {
	remux.Executeur
	mu    sync.Mutex
	lancs []appelProgramme
}

type appelProgramme struct {
	nom  string
	args []string
}

func (e *executeurEnregistre) noter(nom string, args []string) {
	e.mu.Lock()
	e.lancs = append(e.lancs, appelProgramme{nom, append([]string(nil), args...)})
	e.mu.Unlock()
}

func (e *executeurEnregistre) Executer(ctx context.Context, nom string, args []string, max int) ([]byte, error) {
	e.noter(nom, args)
	return e.Executeur.Executer(ctx, nom, args, max)
}

func (e *executeurEnregistre) Demarrer(ctx context.Context, nom string, args []string, in io.ReadCloser) (domain.Processus, error) {
	e.noter(nom, args)
	return e.Executeur.Demarrer(ctx, nom, args, in)
}

func (e *executeurEnregistre) appels(sousChaine string) []appelProgramme {
	e.mu.Lock()
	defer e.mu.Unlock()
	var r []appelProgramme
	for _, a := range e.lancs {
		if strings.Contains(a.nom, sousChaine) {
			r = append(r, a)
		}
	}
	return r
}

// verifierArgv echoue si un argument porte une URL, un identifiant ou un secret.
func verifierArgv(t *testing.T, appels []appelProgramme) {
	t.Helper()
	for _, a := range appels {
		ligne := strings.Join(a.args, " ")
		for _, interdit := range []string{utilisateurSecret, motDePasseSecret, "/live/", "https://", "u:p@"} {
			if strings.Contains(ligne, interdit) {
				t.Errorf("%s : argument interdit %q dans : %s", a.nom, interdit, ligne)
			}
		}
		for _, arg := range a.args {
			// Seule adresse admise : la source locale a usage unique de la sonde (127.0.0.1).
			if strings.HasPrefix(arg, "http://") && !strings.HasPrefix(arg, "http://127.0.0.1:") {
				t.Errorf("%s : adresse non locale en argument : %s", a.nom, arg)
			}
		}
	}
}

// processusDe compte les processus dont la ligne de commande contient `marque`
// (ici, le dossier de travail du test : aucun autre ffmpeg de la machine n'est compte).
func processusDe(marque string) int {
	entrees, _ := os.ReadDir("/proc")
	n := 0
	for _, e := range entrees {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/cmdline")
		if err != nil {
			continue
		}
		if bytes.Contains(b, []byte(marque)) && (bytes.Contains(b, []byte("ffmpeg")) || bytes.Contains(b, []byte("ffprobe"))) {
			if st, err := os.ReadFile("/proc/" + e.Name() + "/stat"); err == nil && bytes.Contains(st, []byte(") Z ")) {
				continue // zombie recolte par personne : sans effet
			}
			n++
		}
	}
	return n
}

func attendreQue(t *testing.T, delai time.Duration, ok func() bool, quoi string) {
	t.Helper()
	fin := time.Now().Add(delai)
	for time.Now().Before(fin) {
		if ok() {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("delai depasse : %s", quoi)
}

func sousDossiers(dossier string) []string {
	e, _ := os.ReadDir(dossier)
	var r []string
	for _, x := range e {
		r = append(r, x.Name())
	}
	return r
}

// bancConversion assemble relais + vrais ffmpeg/ffprobe + faux fournisseur.
type bancConversion struct {
	*integ
	o       outils
	dossier string // dossier_hls (sert aussi de marque pour compter les processus)
	exe     *executeurEnregistre
}

type optionsBanc struct {
	formats map[string]formatTest // chaine -> format
	ajuste  func(*Options)
	faux    func(*fauxiptv.Options)
	// ffmpeg remplace le binaire (test d'echec au demarrage).
	ffmpeg string
}

func nouveauBancConversion(t *testing.T, ob optionsBanc) *bancConversion {
	t.Helper()
	o := chercherOutils(t)
	formats := map[string][][]byte{}
	for chaine, f := range ob.formats {
		formats[chaine] = segmentsGeneres(t, o, f)
	}
	dossier := filepath.Join(t.TempDir(), "hls_live")
	exe := &executeurEnregistre{Executeur: remux.ExecOS{DelaiArret: 2 * time.Second}}
	fo := fauxiptv.Options{PremiereSeq: 1, FenetreSegment: 5, AvanceParPlaylist: 2, DureeSegment: 100 * time.Millisecond,
		ContenuSegment: contenuParChaine(formats)}
	if ob.faux != nil {
		ob.faux(&fo)
	}
	ffmpeg := o.ffmpeg
	if ob.ffmpeg != "" {
		ffmpeg = ob.ffmpeg
	}
	b := &bancConversion{o: o, dossier: dossier, exe: exe}
	b.integ = nouvelleIntegration(t, fo, func(opt *Options) {
		opt.Live.FraicheurPlaylist = config.Duree(30 * time.Millisecond)
		opt.Live.DureeSonde = config.Duree(10 * time.Minute)
		opt.Live.EchecSonde = config.Duree(time.Minute)
		opt.Live.DelaiSonde = config.Duree(15 * time.Second)
		opt.Live.HauteurMaxReencodage = 1080
		opt.Live.ReencodageVideo = true
		opt.Live.DossierHLS = dossier
		opt.Live.InactiviteTranscodage = config.Duree(time.Minute)
		opt.Live.DelaiDemarrageTranscodage = config.Duree(20 * time.Second)
		opt.Live.SegmentsAmorce = 2
		opt.Live.SegmentHLS = 1
		opt.Live.ListeHLS = 3
		opt.Live.Eco.Hauteur = 240 // cible eco courte pour les tests (source 640x360)
		opt.Live.Eco.DebitKbps = 300
		lanceur, err := remux.NouveauLanceur(remux.Reglages{Ffmpeg: ffmpeg, ProcessusMax: 2, FileMax: 1, AttenteFile: time.Second}, exe)
		if err != nil {
			t.Fatal(err)
		}
		opt.Lanceur = lanceur
		opt.Sondeur = remux.NouveauSondeur(remux.ReglagesSonde{Ffprobe: o.ffprobe, Duree: time.Hour, Delai: 15 * time.Second, Max: 10}, exe, nil)
		if ob.ajuste != nil {
			ob.ajuste(opt)
		}
	})
	t.Cleanup(func() {
		// Apres Fermer (enregistre avant ce nettoyage par nouvelleIntegration) : rien ne reste.
		// Cleanup s'execute en ordre inverse : celui-ci passe AVANT Fermer, on ne verifie donc rien ici.
	})
	return b
}

func (b *bancConversion) requete(chemin, session string, entetes map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("GET", chemin, nil)
	req.Header.Set("X-Session", session)
	for k, v := range entetes {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	b.h.ServeHTTP(rec, req)
	return rec
}

// lignesSegments rend les URL de segments d'une playlist reecrite.
func lignesSegments(corps string) []string {
	var r []string
	for _, l := range strings.Split(corps, "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			r = append(r, l)
		}
	}
	return r
}

// sondeFichier lit codecs, hauteur et canaux d'un fichier avec ffprobe.
func (b *bancConversion) sondeFichier(t *testing.T, corps []byte) (remux.SondeDirect, int) {
	t.Helper()
	fichier := filepath.Join(t.TempDir(), "sortie.ts")
	if err := os.WriteFile(fichier, corps, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(b.o.ffprobe, "-v", "error", "-print_format", "json", "-show_streams", fichier).Output()
	if err != nil {
		t.Fatalf("ffprobe de la sortie : %v", err)
	}
	s, err := remux.AnalyserSondeDirect(out)
	if err != nil {
		t.Fatal(err)
	}
	canaux := 0
	if i := bytes.Index(out, []byte(`"channels": `)); i >= 0 {
		fmt.Sscanf(string(out[i+len(`"channels": `):]), "%d", &canaux)
	}
	return s, canaux
}

// recupererSegment lit le premier segment d'une playlist servie.
func (b *bancConversion) recupererSegment(t *testing.T, chemin, session string) []byte {
	t.Helper()
	rec := b.requete(chemin, session, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("segment %s : HTTP %d %s", chemin, rec.Code, rec.Body.String())
	}
	return rec.Body.Bytes()
}
