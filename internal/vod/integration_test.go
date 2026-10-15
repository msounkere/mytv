package vod

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"mytv/internal/config"
	"mytv/internal/remux"
	"mytv/internal/testutil/fauxiptv"
)

// CA3+CA6 : chaine complete avec le VRAI lanceur et le VRAI executeur, un faux
// ffprobe et un faux ffmpeg (scripts). Quand le client part, le faux ffmpeg ET
// son enfant meurent, et le bail est rendu.
func TestIntegrationFauxFfmpegClientParti(t *testing.T) {
	dir := t.TempDir()
	pidf := filepath.Join(dir, "enfant.pid")
	ecrire := func(nom, corps string) string {
		p := filepath.Join(dir, nom)
		if err := os.WriteFile(p, []byte("#!/bin/sh\n"+corps+"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	ffprobe := ecrire("ffprobe", `printf '{"streams":[{"codec_type":"video","codec_name":"h264"},{"codec_type":"audio","codec_name":"eac3"}],"format":{"format_name":"matroska,webm","duration":"60"}}'`)
	ffmpeg := ecrire("ffmpeg", `sleep 300 &
echo $! > `+pidf+`
while :; do printf 'fragment'; sleep 0.01; done`)

	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	c := config.Defauts()
	c.Remux.Ffmpeg, c.Remux.Ffprobe, c.Remux.ProcessusMax = ffmpeg, ffprobe, 1
	c.Remux.DelaiArret = config.Duree(time.Second)
	lr, sr, ex := remux.DepuisConfig(c.Remux)
	lanceur, err := remux.NouveauLanceur(lr, ex)
	if err != nil {
		t.Fatal(err)
	}
	b.svc.dep.Sondeur = remux.NouveauSondeur(sr, ex, nil)
	b.svc.dep.Lanceur = lanceur

	ctx, annule := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", b.http.URL+"/?id=iptva:2002", nil)
	req.Header.Set("X-Session", "s1")
	rep, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	if _, err := io.ReadFull(rep.Body, buf); err != nil || !strings.HasPrefix(string(buf), "fragment") {
		t.Fatalf("flux = %q, %v", buf, err)
	}
	var enfant int
	attendre(t, func() bool {
		raw, err := os.ReadFile(pidf)
		if err != nil {
			return false
		}
		enfant, err = strconv.Atoi(strings.TrimSpace(string(raw)))
		return err == nil
	}, "pid de l'enfant")
	if lanceur.EnCours() != 1 {
		t.Errorf("processus en cours = %d", lanceur.EnCours())
	}
	annule()
	rep.Body.Close()
	attendre(t, func() bool { return lanceur.EnCours() == 0 }, "emplacement ffmpeg rendu")
	attendre(t, func() bool { return syscall.Kill(enfant, 0) != nil }, "enfant de ffmpeg tue")
	attendre(t, func() bool { return b.baux.bail("s1").libere.Load() == 1 }, "bail rendu")
}
