package remux

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"mytv/internal/domain"
)

func sourceVide(context.Context, int64) (io.ReadCloser, error) { return nil, io.EOF }

func tacheDirectValide() TacheDirect {
	return TacheDirect{Source: sourceVide, Dossier: "/tmp/hls/201-0123456789ab", Prefixe: "0123abcd", SegmentSecondes: 4, ListeSegments: 6}
}

// US-31 CA1 : les arguments d'un direct ne contiennent ni adresse ni identifiant ; l'entree est
// le tube, la sortie un HLS 4 s / liste 6 / delete_segments dans le dossier donne.
func TestArgumentsDirectParModeSansAdresse(t *testing.T) {
	t0 := tacheDirectValide()
	a, err := ArgumentsDirect(t0)
	if err != nil {
		t.Fatal(err)
	}
	ligne := strings.Join(a, " ")
	for _, attendu := range []string{"-i pipe:0", "-c:v copy", "-c:a aac -b:a 192k -ac 2", "-hls_time 4", "-hls_list_size 6",
		"delete_segments+independent_segments", "-protocol_whitelist pipe", "/tmp/hls/201-0123456789ab/0123abcd_%05d.ts", "/tmp/hls/201-0123456789ab/index.m3u8"} {
		if !strings.Contains(ligne, attendu) {
			t.Errorf("%q absent de : %s", attendu, ligne)
		}
	}
	if strings.Contains(ligne, "http") || strings.Contains(ligne, "libx264") {
		t.Errorf("argv : %s", ligne)
	}
	t0.Video = true
	a, _ = ArgumentsDirect(t0)
	ligne = strings.Join(a, " ")
	if !strings.Contains(ligne, "-c:v libx264 -preset veryfast -crf 23 -g 100 -sc_threshold 0") || strings.Contains(ligne, "-c:v copy") {
		t.Errorf("mode video : %s", ligne)
	}
}

// US-38 CA1 : en mode economie, la video est reencodee a la cible (mise a l'echelle a hauteur
// donnee, debit contraint), l'audio en AAC 96 kbit/s, sans crf ; une cible hors bornes est refusee.
func TestCA1_ArgumentsDirectEco(t *testing.T) {
	t0 := tacheDirectValide()
	t0.Eco = CibleEco{Hauteur: 480, DebitKbps: 1200}
	a, err := ArgumentsDirect(t0)
	if err != nil {
		t.Fatal(err)
	}
	ligne := strings.Join(a, " ")
	for _, attendu := range []string{"-c:v libx264 -preset veryfast -vf scale=-2:480 -b:v 1200k -maxrate 1200k -bufsize 2400k -pix_fmt yuv420p -g 100 -sc_threshold 0",
		"-c:a aac -b:a 96k -ac 2", "-i pipe:0", "-hls_time 4"} {
		if !strings.Contains(ligne, attendu) {
			t.Errorf("%q absent de : %s", attendu, ligne)
		}
	}
	if strings.Contains(ligne, "-crf") || strings.Contains(ligne, "-c:v copy") || strings.Contains(ligne, "192k") {
		t.Errorf("mode eco : %s", ligne)
	}
	// Video faux + Eco : la cible l'emporte (Video est implicite).
	t0.Video = false
	if b, _ := ArgumentsDirect(t0); strings.Join(b, " ") != ligne {
		t.Errorf("Eco doit imposer le reencodage video : %v", b)
	}
	for nom, c := range map[string]CibleEco{"hauteur basse": {100, 1200}, "hauteur haute": {2160, 1200}, "debit bas": {480, 10}, "debit haut": {480, 20000}, "sans debit": {480, 0}} {
		t0.Eco = c
		if _, err := ArgumentsDirect(t0); !errors.Is(err, ErrArgumentRefuse) {
			t.Errorf("%s : %v", nom, err)
		}
	}
}

func TestArgumentsDirectRefuseLesValeursDangereuses(t *testing.T) {
	mod := func(f func(*TacheDirect)) TacheDirect { t := tacheDirectValide(); f(&t); return t }
	cas := map[string]TacheDirect{
		"sans source":        mod(func(t *TacheDirect) { t.Source = nil }),
		"dossier relatif":    mod(func(t *TacheDirect) { t.Dossier = "hls/x" }),
		"dossier option":     mod(func(t *TacheDirect) { t.Dossier = "-f" }),
		"dossier pourcent":   mod(func(t *TacheDirect) { t.Dossier = "/tmp/a%d" }),
		"dossier retour":     mod(func(t *TacheDirect) { t.Dossier = "/tmp/a\nb" }),
		"prefixe majuscule":  mod(func(t *TacheDirect) { t.Prefixe = "ABCD1234" }),
		"prefixe separateur": mod(func(t *TacheDirect) { t.Prefixe = "ab/../cd" }),
		"prefixe court":      mod(func(t *TacheDirect) { t.Prefixe = "ab" }),
		"segment nul":        mod(func(t *TacheDirect) { t.SegmentSecondes = 0 }),
		"liste trop longue":  mod(func(t *TacheDirect) { t.ListeSegments = 999 }),
		"liste trop courte":  mod(func(t *TacheDirect) { t.ListeSegments = 1 }),
	}
	for nom, tache := range cas {
		if _, err := ArgumentsDirect(tache); !errors.Is(err, ErrArgumentRefuse) {
			t.Errorf("%s : %v", nom, err)
		}
		l, _ := NouveauLanceur(Reglages{Ffmpeg: "ffmpeg", ProcessusMax: 1}, nil)
		if _, err := l.LancerDirect(context.Background(), tache); !errors.Is(err, ErrArgumentRefuse) {
			t.Errorf("%s (lanceur) : %v", nom, err)
		}
		if l.EnCours() != 0 {
			t.Errorf("%s : un emplacement est reste pris", nom)
		}
	}
}

var _ domain.SourceOctets = sourceVide
