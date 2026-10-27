package vod

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"mytv/internal/domain"
	"mytv/internal/remux"
	"mytv/internal/testutil/fauxiptv"
)

// US-38 : mode economie (`qualite=eco`) sur les films et les episodes.

func requeteFilm(t *testing.T, b *banc, methode, suffixe string, entetes ...string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(methode, b.http.URL+"/?id=iptva:2001"+suffixe, nil)
	req.Header.Set("X-Session", "s1")
	for i := 0; i+1 < len(entetes); i += 2 {
		req.Header.Set(entetes[i], entetes[i+1])
	}
	rep, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// CA2 : un mp4 H.264 lisible tel quel (relais « direct » d'habitude) demande en qualite=eco est
// reencode par un VRAI ffmpeg a la cible (ici 240p / 300 kbit/s) : hauteur de sortie verifiee par
// ffprobe, X-Mytv-Mode: eco et X-Mytv-Qualite: eco sur GET comme sur HEAD (aucun ffmpeg pour le
// HEAD) ; sans le parametre, le film est toujours relaye octet pour octet.
func TestCA2_QualiteEcoReencodeUnFilmLisibleAvecLeVraiFfmpeg(t *testing.T) {
	if testing.Short() {
		t.Skip("-short")
	}
	ffmpeg, err1 := exec.LookPath("ffmpeg")
	ffprobe, err2 := exec.LookPath("ffprobe")
	if err1 != nil || err2 != nil {
		t.Skip("ffmpeg/ffprobe absents")
	}
	fichier := filepath.Join(t.TempDir(), "film.mp4")
	gen := exec.Command(ffmpeg, "-v", "error", "-f", "lavfi", "-i", "testsrc=size=640x360:rate=25:duration=3",
		"-f", "lavfi", "-i", "sine=duration=3", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p",
		"-c:a", "aac", "-movflags", "+faststart", "-y", fichier)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Skipf("generation impossible (encodeurs absents ?) : %v %s", err, out)
	}
	contenu, err := os.ReadFile(fichier)
	if err != nil {
		t.Fatal(err)
	}
	b := nouveauBanc(t, fauxiptv.Options{ContenuFilm: func(nom string) ([]byte, bool) { return contenu, nom == "2001.mp4" }},
		func(r *Reglages) { r.Eco = remux.CibleEco{Hauteur: 240, DebitKbps: 300} })
	b.sondeur.sonde = sondeMP4
	lanceur, err := remux.NouveauLanceur(remux.Reglages{Ffmpeg: ffmpeg, ProcessusMax: 1, Reprises: 2}, remux.ExecOS{})
	if err != nil {
		t.Fatal(err)
	}
	b.svc.dep.Lanceur = lanceur

	rep, corps, err := b.get("iptva:2001", "s1", "")
	if err != nil || rep.StatusCode != 200 || rep.Header.Get("X-Mytv-Mode") != "direct" || !bytes.Equal(corps, contenu) {
		t.Fatalf("sans parametre, relais direct inchange : %d %v (%d octets)", rep.StatusCode, err, len(corps))
	}
	if r := requeteFilm(t, b, "GET", "&qualite=ultra"); r.StatusCode != 400 {
		t.Errorf("qualite inconnue : %d", r.StatusCode)
	} else {
		r.Body.Close()
	}
	tete := requeteFilm(t, b, "HEAD", "&qualite=eco")
	tete.Body.Close()
	if tete.StatusCode != 200 || tete.Header.Get("X-Mytv-Mode") != "eco" || tete.Header.Get("X-Mytv-Qualite") != "eco" || tete.Header.Get("Accept-Ranges") != "none" {
		t.Errorf("HEAD eco : %d %v", tete.StatusCode, tete.Header)
	}
	if lanceur.EnCours() != 0 {
		t.Error("un HEAD a lance ffmpeg")
	}

	r := requeteFilm(t, b, "GET", "", "X-Mytv-Qualite", "eco")
	defer r.Body.Close()
	if r.StatusCode != 200 || r.Header.Get("X-Mytv-Mode") != "eco" || r.Header.Get("X-Mytv-Qualite") != "eco" || r.Header.Get("Content-Type") != "video/mp4" {
		t.Fatalf("GET eco : %d %v", r.StatusCode, r.Header)
	}
	sortie, err := io.ReadAll(r.Body)
	if err != nil || len(sortie) < 1000 || string(sortie[4:8]) != "ftyp" {
		t.Fatalf("sortie : %d octets, %v", len(sortie), err)
	}
	if bytes.Equal(sortie, contenu) {
		t.Fatal("le film a ete relaye tel quel au lieu d'etre reencode")
	}
	fs := filepath.Join(t.TempDir(), "sortie.mp4")
	if err := os.WriteFile(fs, sortie, 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(ffprobe, "-v", "error", "-print_format", "json", "-show_streams", fs).Output()
	if err != nil {
		t.Fatalf("ffprobe de la sortie : %v", err)
	}
	s, err := remux.AnalyserSondeDirect(out)
	if err != nil {
		t.Fatal(err)
	}
	if s.CodecVideo != "h264" || s.Hauteur != 240 || s.CodecAudio != "aac" {
		t.Errorf("sortie = %+v (attendu h264 240p + aac)", s)
	}
	// Deux lectures comptees en eco : le HEAD (qui en annonce le mode) et le GET.
	if n := b.instr.valeur("vod_lectures_total{mode=eco}"); n != 2 {
		t.Errorf("vod_lectures_total{mode=eco} = %v", n)
	}
	attendre(t, func() bool { return lanceur.EnCours() == 0 }, "fin du ffmpeg")
}

// CA2 : sans cible configuree (live.eco absent des reglages), le parametre est ignore : le film
// suit la decision habituelle, aucun 400, aucun processus.
func TestCA2_SansCibleEcoLeParametreEstIgnore(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil) // ReglagesParDefaut : Eco inactive
	b.sondeur.sonde = sondeMP4
	r := requeteFilm(t, b, "GET", "&qualite=eco")
	defer r.Body.Close()
	if r.StatusCode != 200 || r.Header.Get("X-Mytv-Mode") != "direct" || r.Header.Get("X-Mytv-Qualite") != "" {
		t.Errorf("eco sans cible : %d %v", r.StatusCode, r.Header)
	}
	io.Copy(io.Discard, r.Body)
	if len(b.lanceur.liste()) != 0 {
		t.Error("aucun ffmpeg attendu")
	}
	if _, err := Nouveau(Dependances{Stockage: &stockageFaux{}, Baux: &bauxFaux{}, Sondeur: &sondeurFaux{}, Decideur: remux.Decider, Lanceur: &lanceurFaux{}},
		Reglages{Tampon: 1, RenouvellementBail: 1, Eco: remux.CibleEco{Hauteur: 100, DebitKbps: 1200}}); err == nil {
		t.Error("une cible eco hors bornes doit etre refusee a la construction")
	}
}

// CA2 : avec le faux lanceur, la tache part par LancerEco avec la cible, meme pour un mkv (remux)
// et un episode ; un fichier HEVC refuse d'habitude (415) reste refuse : la sonde juge d'abord.
func TestCA2_LaTacheEcoPorteLaCible(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, func(r *Reglages) { r.Eco = remux.CibleEco{Hauteur: 480, DebitKbps: 1200} })
	b.sondeur.sonde = sondeMKV
	b.lanceur.surLancer = b.nourrir("ftypISOM-eco", true)
	r := requeteFilm(t, b, "GET", "&qualite=eco")
	corps, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 200 || r.Header.Get("X-Mytv-Mode") != "eco" || string(corps) != "ftypISOM-eco" {
		t.Fatalf("%d %v %q", r.StatusCode, r.Header, corps)
	}
	ps := b.lanceur.liste()
	if len(ps) != 1 || ps[0].cible != (remux.CibleEco{Hauteur: 480, DebitKbps: 1200}) || ps[0].tache.Mode != domain.ModeRemux {
		t.Errorf("processus = %+v", ps)
	}
	b.sondeur.sonde = sondeHEV
	if r := requeteFilm(t, b, "GET", "&qualite=eco"); r.StatusCode != 415 {
		t.Errorf("HEVC en eco : %d (la sonde juge d'abord)", r.StatusCode)
	} else {
		r.Body.Close()
	}
}

// CA3 : un changement de qualite en cours de lecture (eco -> auto -> eco, par une nouvelle requete
// de la meme session) remplace le remux en cours : l'ancien ffmpeg est mort AVANT que le nouveau
// demarre, un seul processus a la fois, et le bail n'est rendu qu'a la toute fin.
func TestCA3_ChangementDeQualiteRemplaceLeRemuxEnCours(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, func(r *Reglages) { r.Eco = remux.CibleEco{Hauteur: 480, DebitKbps: 1200} })
	b.sondeur.sonde = sondeMKV
	b.lanceur.surLancer = func(p *procTest) {
		p.pw.Write([]byte("flux"))
		<-p.termine
	}
	ouvrir := func(suffixe string) *http.Response {
		r := requeteFilm(t, b, "GET", suffixe)
		io.ReadFull(r.Body, make([]byte, 4))
		return r
	}
	r1 := ouvrir("&qualite=eco")
	defer r1.Body.Close()
	r2 := ouvrir("&t=600") // retour en auto
	defer r2.Body.Close()
	r3 := ouvrir("&t=600&qualite=eco")
	defer r3.Body.Close()
	ps := b.lanceur.liste()
	if len(ps) != 3 || ps[0].arrets.Load() == 0 || ps[1].arrets.Load() == 0 {
		t.Fatalf("%d processus ; les deux premiers doivent etre arretes", len(ps))
	}
	if ps[2].arrets.Load() != 0 {
		t.Error("le processus courant a ete arrete")
	}
	if ps[0].cible.Hauteur != 480 || ps[1].cible.Hauteur != 0 || ps[2].cible.Hauteur != 480 {
		t.Errorf("cibles : %v %v %v", ps[0].cible, ps[1].cible, ps[2].cible)
	}
	if r1.Header.Get("X-Mytv-Mode") != "eco" || r2.Header.Get("X-Mytv-Mode") != "remux" || r3.Header.Get("X-Mytv-Mode") != "eco" {
		t.Errorf("modes : %s %s %s", r1.Header.Get("X-Mytv-Mode"), r2.Header.Get("X-Mytv-Mode"), r3.Header.Get("X-Mytv-Mode"))
	}
	if n := b.baux.bail("s1").libere.Load(); n != 0 {
		t.Errorf("bail rendu %d fois pendant la lecture", n)
	}
	// Les lectures remplacees ne passent pas pour des films termines.
	for i, r := range []*http.Response{r1, r2} {
		if _, err := io.ReadAll(r.Body); err == nil {
			t.Errorf("lecture %d remplacee : parait terminee au lieu d'etre coupee", i+1)
		}
	}
	r3.Body.Close()
	attendre(t, func() bool { return ps[2].arrets.Load() > 0 && b.baux.bail("s1").libere.Load() == 1 }, "fin : un seul rendu du bail")
}
