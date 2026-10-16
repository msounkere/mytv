package live

import (
	"net/http/httptest"
	"strings"
	"testing"

	"mytv/internal/remux"
)

// US-30 CA2 et CA5 : la decision est une fonction pure, testee sur la table COMPLETE
// (codecs x capacites x reglages).
func TestDeciderLiveTable(t *testing.T) {
	std := ReglagesDecision{ReencodageVideo: true, HauteurMax: 1080}
	sansReenc := ReglagesDecision{ReencodageVideo: false, HauteurMax: 1080}
	rien := Capacites{}
	tout := Capacites{HEVC: true, EAC3: true, AC3: true}
	cas := []struct {
		nom   string
		s     remux.SondeDirect
		c     Capacites
		r     ReglagesDecision
		mode  Mode
		code  string
		texte []string // fragments attendus dans la raison
	}{
		{"h264+aac", remux.SondeDirect{CodecVideo: "h264", Hauteur: 1080, CodecAudio: "aac"}, rien, std, ModeDirect, "", nil},
		{"h264+mp3", remux.SondeDirect{CodecVideo: "h264", Hauteur: 576, CodecAudio: "mp3"}, rien, std, ModeDirect, "", nil},
		{"h264 sans son", remux.SondeDirect{CodecVideo: "h264", Hauteur: 576}, rien, std, ModeDirect, "", nil},
		{"h264+ac3 sans capacite", remux.SondeDirect{CodecVideo: "h264", Hauteur: 720, CodecAudio: "ac3"}, rien, std, ModeAudio, "", nil},
		{"h264+ac3 capable ac3", remux.SondeDirect{CodecVideo: "h264", Hauteur: 720, CodecAudio: "ac3"}, Capacites{AC3: true}, std, ModeDirect, "", nil},
		{"h264+ac3 capable eac3 seulement", remux.SondeDirect{CodecVideo: "h264", Hauteur: 720, CodecAudio: "ac3"}, Capacites{EAC3: true}, std, ModeAudio, "", nil},
		{"h264+eac3 sans capacite", remux.SondeDirect{CodecVideo: "h264", Hauteur: 720, CodecAudio: "eac3"}, rien, std, ModeAudio, "", nil},
		{"h264+eac3 capable", remux.SondeDirect{CodecVideo: "h264", Hauteur: 720, CodecAudio: "eac3"}, Capacites{EAC3: true}, std, ModeDirect, "", nil},
		{"h264+dts meme capable de tout", remux.SondeDirect{CodecVideo: "h264", Hauteur: 720, CodecAudio: "dts"}, tout, std, ModeAudio, "", nil},
		{"h264+truehd", remux.SondeDirect{CodecVideo: "h264", Hauteur: 720, CodecAudio: "truehd"}, rien, std, ModeAudio, "", nil},
		{"h264+mp2", remux.SondeDirect{CodecVideo: "h264", Hauteur: 720, CodecAudio: "mp2"}, rien, std, ModeAudio, "", nil},
		{"hevc 1080 incapable", remux.SondeDirect{CodecVideo: "hevc", Hauteur: 1080, CodecAudio: "aac"}, rien, std, ModeVideo, "", []string{"hevc", "1080p"}},
		{"hevc 360+eac3 incapable", remux.SondeDirect{CodecVideo: "hevc", Hauteur: 360, CodecAudio: "eac3"}, rien, std, ModeVideo, "", nil},
		{"hevc 1081 incapable", remux.SondeDirect{CodecVideo: "hevc", Hauteur: 1081, CodecAudio: "aac"}, rien, std, ModeRefus, "format_hevc_non_lisible", []string{"hevc", "1081p"}},
		{"hevc 4K incapable", remux.SondeDirect{CodecVideo: "hevc", Hauteur: 2160, CodecAudio: "eac3"}, rien, std, ModeRefus, "format_hevc_non_lisible", []string{"hevc", "2160p"}},
		{"hevc 4K capable hevc seulement", remux.SondeDirect{CodecVideo: "hevc", Hauteur: 2160, CodecAudio: "eac3"}, Capacites{HEVC: true}, std, ModeAudio, "", nil},
		{"hevc 4K capable hevc+eac3", remux.SondeDirect{CodecVideo: "hevc", Hauteur: 2160, CodecAudio: "eac3"}, Capacites{HEVC: true, EAC3: true}, std, ModeDirect, "", nil},
		{"hevc 4K capable hevc, reencodage interdit", remux.SondeDirect{CodecVideo: "hevc", Hauteur: 2160, CodecAudio: "aac"}, Capacites{HEVC: true}, sansReenc, ModeDirect, "", nil},
		{"h265 est hevc", remux.SondeDirect{CodecVideo: "h265", Hauteur: 2160, CodecAudio: "aac"}, rien, std, ModeRefus, "format_hevc_non_lisible", nil},
		{"hevc 720 incapable, reencodage interdit", remux.SondeDirect{CodecVideo: "hevc", Hauteur: 720, CodecAudio: "aac"}, rien, sansReenc, ModeRefus, "format_hevc_non_lisible", []string{"desactive"}},
		{"hevc hauteur inconnue", remux.SondeDirect{CodecVideo: "hevc", CodecAudio: "aac"}, rien, std, ModeVideo, "", nil},
		{"mpeg2 576", remux.SondeDirect{CodecVideo: "mpeg2video", Hauteur: 576, CodecAudio: "mp2"}, rien, std, ModeVideo, "", nil},
		{"mpeg2 capable hevc ne change rien", remux.SondeDirect{CodecVideo: "mpeg2video", Hauteur: 576, CodecAudio: "mp2"}, Capacites{HEVC: true}, std, ModeVideo, "", nil},
		{"mpeg2 2160 refuse autrement", remux.SondeDirect{CodecVideo: "mpeg2video", Hauteur: 2160}, rien, std, ModeRefus, "format_non_lisible", nil},
		{"radio aac", remux.SondeDirect{CodecAudio: "aac"}, rien, std, ModeDirect, "", nil},
		{"radio mp3", remux.SondeDirect{CodecAudio: "mp3"}, rien, std, ModeDirect, "", nil},
		{"radio mp2", remux.SondeDirect{CodecAudio: "mp2"}, rien, std, ModeAudio, "", nil},
		{"codec hostile", remux.SondeDirect{CodecVideo: "<script>", Hauteur: 4000}, rien, std, ModeRefus, "format_non_lisible", []string{"inconnu"}},
	}
	for _, c := range cas {
		t.Run(c.nom, func(t *testing.T) {
			d := DeciderLive(c.s, c.c, c.r)
			if d.Mode != c.mode || d.Code != c.code {
				t.Fatalf("decision = %+v, attendu %s/%s", d, c.mode, c.code)
			}
			for _, f := range c.texte {
				if !strings.Contains(d.Raison, f) {
					t.Errorf("raison %q sans %q", d.Raison, f)
				}
			}
			if strings.Contains(d.Raison, "<") {
				t.Errorf("raison non assainie : %q", d.Raison)
			}
		})
	}
}

// US-30 CA5 : capacites lues dans l'en-tete ET dans ?cap=, union, inconnues ignorees, absence = aucune.
func TestLireCapacites(t *testing.T) {
	lire := func(url string, entete ...string) Capacites {
		req := httptest.NewRequest("GET", url, nil)
		for _, e := range entete {
			req.Header.Add("X-Mytv-Capacites", e)
		}
		return LireCapacites(req)
	}
	if c := lire("/x"); c != (Capacites{}) || c.String() != "aucune" {
		t.Errorf("absence = %+v", c)
	}
	if c := lire("/x?cap=hevc,eac3"); !c.HEVC || !c.EAC3 || c.AC3 {
		t.Errorf("parametre = %+v", c)
	}
	if c := lire("/x", "ac3, HEVC"); !c.HEVC || !c.AC3 || c.EAC3 {
		t.Errorf("en-tete = %+v", c)
	}
	if c := lire("/x?cap=eac3", "hevc"); !c.HEVC || !c.EAC3 {
		t.Errorf("union = %+v", c)
	}
	if c := lire("/x?cap=av1,dts,%00,,hevc"); !c.HEVC || c.EAC3 || c.AC3 || c.String() != "hevc" {
		t.Errorf("inconnues ignorees = %+v", c)
	}
}

// US-30 CA2 : l'analyse de la sortie de ffprobe retient hauteur et codecs, ignore les pochettes.
func TestAnalyserSondeDirect(t *testing.T) {
	s, err := remux.AnalyserSondeDirect([]byte(`{"streams":[{"codec_type":"video","codec_name":"mjpeg","height":500},{"codec_type":"video","codec_name":"HEVC","height":2160},{"codec_type":"audio","codec_name":"eac3"},{"codec_type":"audio","codec_name":"aac"}]}`))
	if err != nil || s.CodecVideo != "hevc" || s.Hauteur != 2160 || s.CodecAudio != "eac3" {
		t.Fatalf("%+v %v", s, err)
	}
	if _, err := remux.AnalyserSondeDirect([]byte(`{"streams":[]}`)); err == nil {
		t.Error("aucun flux accepte")
	}
	if _, err := remux.AnalyserSondeDirect([]byte(`pas du json`)); err == nil {
		t.Error("json illisible accepte")
	}
}
