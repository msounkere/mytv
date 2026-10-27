package live

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mytv/internal/remux"
)

// US-38 : mode economie (`qualite=eco`) sur le direct. Vrais ffmpeg/ffprobe, faux fournisseur.

// CA1 : la decision du mode economie. Fonction pure : table.
func TestCA1_DeciderEco(t *testing.T) {
	r := ReglagesDecision{ReencodageVideo: true, HauteurMax: 1080}
	cas := []struct {
		nom    string
		s      remux.SondeDirect
		connue bool
		r      ReglagesDecision
		mode   Mode
		code   string
	}{
		{"h264 lisible, reencode quand meme", remux.SondeDirect{CodecVideo: "h264", Hauteur: 1080, CodecAudio: "aac"}, true, r, ModeVideo, ""},
		{"hevc 720p", remux.SondeDirect{CodecVideo: "hevc", Hauteur: 720, CodecAudio: "eac3"}, true, r, ModeVideo, ""},
		{"chaine jamais sondee", remux.SondeDirect{}, false, r, ModeVideo, ""},
		{"radio", remux.SondeDirect{CodecAudio: "mp3"}, true, r, ModeAudio, ""},
		{"4K hevc : decodage impossible en temps reel", remux.SondeDirect{CodecVideo: "hevc", Hauteur: 2160}, true, r, ModeRefus, "format_hevc_non_lisible"},
		{"4K h264", remux.SondeDirect{CodecVideo: "h264", Hauteur: 2160}, true, r, ModeRefus, "format_non_lisible"},
		{"reencodage desactive", remux.SondeDirect{CodecVideo: "h264", Hauteur: 720}, true, ReglagesDecision{ReencodageVideo: false, HauteurMax: 1080}, ModeRefus, "format_non_lisible"},
	}
	for _, c := range cas {
		t.Run(c.nom, func(t *testing.T) {
			d := DeciderEco(c.s, c.connue, c.r, 480)
			if d.Mode != c.mode || d.Code != c.code || d.Raison == "" {
				t.Errorf("decision = %+v, attendu %s/%q", d, c.mode, c.code)
			}
			if c.mode == ModeVideo && !strings.Contains(d.Raison, "480p") {
				t.Errorf("la raison nomme la cible : %q", d.Raison)
			}
		})
	}
	for v, attendu := range map[string]Qualite{"": QualiteAuto, "auto": QualiteAuto, "eco": QualiteEco, "ECO": QualiteEco} {
		req := httptest.NewRequest("GET", "/x?qualite="+v, nil)
		if q, ok := LireQualite(req); !ok || q != attendu {
			t.Errorf("qualite=%q : %q %v", v, q, ok)
		}
	}
	req := httptest.NewRequest("GET", "/x", nil)
	req.Header.Set("X-Mytv-Qualite", "eco")
	if q, ok := LireQualite(req); !ok || q != QualiteEco {
		t.Errorf("en-tete : %q %v", q, ok)
	}
	if _, ok := LireQualite(httptest.NewRequest("GET", "/x?qualite=ultra", nil)); ok {
		t.Error("valeur inconnue acceptee")
	}
}

// CA1 : une chaine h264+aac (relayee telle quelle d'habitude) demandee en `qualite=eco` est
// convertie par ffmpeg a la cible (ici 240p / 300 kbit/s) : arguments attendus, sortie verifiee
// par ffprobe, en-tetes X-Mytv-Mode: video et X-Mytv-Qualite: eco ; sans le parametre, rien ne change.
func TestCA1_QualiteEcoConvertitUneChaineLisible(t *testing.T) {
	b := nouveauBancConversion(t, optionsBanc{formats: map[string]formatTest{"203": formatH264AAC}})
	if r := b.playlist("203", "s1", ""); r.Code != 200 || r.Header().Get("X-Mytv-Mode") != "direct" || r.Header().Get("X-Mytv-Qualite") != "" {
		t.Fatalf("sans parametre : HTTP %d %v", r.Code, r.Header())
	}
	if r := b.playlist("203", "s1", "?qualite=ultra"); r.Code != 400 || r.Header().Get("X-Mytv-Erreur") != "qualite_invalide" {
		t.Fatalf("qualite inconnue : HTTP %d %s", r.Code, r.Body.String())
	}
	rec := b.playlist("203", "s1", "?qualite=eco")
	if rec.Code != 200 || rec.Header().Get("X-Mytv-Mode") != "video" || rec.Header().Get("X-Mytv-Qualite") != "eco" {
		t.Fatalf("eco : HTTP %d %v %s", rec.Code, rec.Header(), rec.Body.String())
	}
	segs := lignesSegments(rec.Body.String())
	if len(segs) < 2 {
		t.Fatalf("playlist : %q", rec.Body.String())
	}
	ff := b.exe.appels("ffmpeg")
	if len(ff) != 1 {
		t.Fatalf("%d lancements de ffmpeg", len(ff))
	}
	args := strings.Join(ff[0].args, " ")
	for _, attendu := range []string{"-vf scale=-2:240", "-b:v 300k", "-maxrate 300k", "-bufsize 600k", "-b:a 96k", "libx264"} {
		if !strings.Contains(args, attendu) {
			t.Errorf("%q absent de : %s", attendu, args)
		}
	}
	if strings.Contains(args, "-crf") {
		t.Errorf("pas de crf en eco : %s", args)
	}
	verifierArgv(t, ff)
	corps := b.recupererSegment(t, segs[0], "s1")
	s, canaux := b.sondeFichier(t, corps)
	if s.CodecVideo != "h264" || s.Hauteur != 240 || s.CodecAudio != "aac" || canaux != 2 {
		t.Errorf("sortie = %+v, %d canaux (attendu h264 240p + aac stereo)", s, canaux)
	}
	// L'en-tete fait la meme chose que le parametre.
	if r := b.requete(PrefixeParDefaut+"/203.m3u8", "s1", map[string]string{"X-Mytv-Qualite": "eco"}); r.Code != 200 || r.Header().Get("X-Mytv-Qualite") != "eco" {
		t.Errorf("en-tete X-Mytv-Qualite : %d %v", r.Code, r.Header())
	}
	if len(b.exe.appels("ffmpeg")) != 1 {
		t.Error("la meme qualite ne relance pas ffmpeg")
	}
	b.relais.FinDeLecture("s1", "203")
	b.aucunResidu(t)
}

// CA1 : les interdits de la conversion s'appliquent (4K connue : 415, reencodage desactive : 415).
func TestCA1_QualiteEcoRespecteLesInterdits(t *testing.T) {
	b := nouveauBancConversion(t, optionsBanc{formats: map[string]formatTest{"204": formatHEVC4K, "203": formatH264AAC}})
	if r := b.playlist("204", "s1", ""); r.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("4K sondee : %d", r.Code)
	}
	r := b.playlist("204", "s1", "?qualite=eco")
	if r.Code != http.StatusUnsupportedMediaType || b.corpsErreur(t, r) != "format_hevc_non_lisible" || r.Header().Get("X-Mytv-Mode") != "refus" {
		t.Errorf("4K en eco : %d %s", r.Code, r.Body.String())
	}
	b2 := nouveauBancConversion(t, optionsBanc{formats: map[string]formatTest{"203": formatH264AAC},
		ajuste: func(o *Options) { o.Live.ReencodageVideo = false }})
	if r := b2.playlist("203", "s1", "?qualite=eco"); r.Code != http.StatusUnsupportedMediaType || b2.corpsErreur(t, r) != "format_non_lisible" {
		t.Errorf("reencodage desactive : %d %s", r.Code, r.Body.String())
	}
	if len(b.exe.appels("ffmpeg"))+len(b2.exe.appels("ffmpeg")) != 0 {
		t.Error("aucun ffmpeg ne doit etre lance pour un refus")
	}
}

// CA3 : un changement de qualite en cours de lecture (auto -> eco -> auto) rouvre proprement :
// l'ancien ffmpeg est arrete et son dossier supprime avant que la nouvelle playlist soit rendue,
// un seul processus par session a tout moment.
func TestCA3_ChangementDeQualiteRelanceUnSeulFfmpeg(t *testing.T) {
	b := nouveauBancConversion(t, optionsBanc{formats: map[string]formatTest{"201": formatHEVCEAC3}})
	if r := b.playlist("201", "s1", ""); r.Code != 200 || r.Header().Get("X-Mytv-Mode") != "video" || r.Header().Get("X-Mytv-Qualite") != "" {
		t.Fatalf("auto : HTTP %d %v", r.Code, r.Header())
	}
	d1 := sousDossiers(b.dossier)
	r := b.playlist("201", "s1", "?qualite=eco")
	if r.Code != 200 || r.Header().Get("X-Mytv-Qualite") != "eco" {
		t.Fatalf("eco : HTTP %d %v", r.Code, r.Header())
	}
	d2 := sousDossiers(b.dossier)
	if len(d1) != 1 || len(d2) != 1 || d1[0] == d2[0] {
		t.Errorf("un seul dossier, renouvele : %v -> %v", d1, d2)
	}
	if n := processusDe(b.dossier); n != 1 {
		t.Errorf("%d ffmpeg apres le passage en eco", n)
	}
	ff := b.exe.appels("ffmpeg")
	if len(ff) != 2 || strings.Contains(strings.Join(ff[0].args, " "), "scale=") || !strings.Contains(strings.Join(ff[1].args, " "), "scale=-2:240") {
		t.Fatalf("lancements : %d (%v)", len(ff), ff)
	}
	corps := b.recupererSegment(t, lignesSegments(r.Body.String())[0], "s1")
	if s, _ := b.sondeFichier(t, corps); s.Hauteur != 240 {
		t.Errorf("segment eco a %dp", s.Hauteur)
	}
	// Retour en auto : encore un seul ffmpeg, sans mise a l'echelle.
	if r := b.playlist("201", "s1", "?qualite=auto"); r.Code != 200 || r.Header().Get("X-Mytv-Qualite") != "" {
		t.Fatalf("retour auto : HTTP %d %v", r.Code, r.Header())
	}
	if n := processusDe(b.dossier); n != 1 || len(sousDossiers(b.dossier)) != 1 {
		t.Errorf("%d ffmpeg, dossiers %v", n, sousDossiers(b.dossier))
	}
	if ff = b.exe.appels("ffmpeg"); len(ff) != 3 || strings.Contains(strings.Join(ff[2].args, " "), "scale=") {
		t.Errorf("lancements : %d", len(ff))
	}
	b.relais.FinDeLecture("s1", "201")
	b.aucunResidu(t)
}
