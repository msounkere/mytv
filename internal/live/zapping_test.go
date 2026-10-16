package live

import (
	"strings"
	"testing"
)

// US-33 CA2 : zapping x10 entre chaines direct / audio / video sur le faux fournisseur.
// A chaque passage : un seul ffmpeg vivant au plus, un seul dossier, bon mode ; a la fin de
// lecture : plus aucun processus (ffmpeg/ffprobe) ni dossier.
func TestZappingDixFoisEntreModes(t *testing.T) {
	b := nouveauBancConversion(t, optionsBanc{formats: map[string]formatTest{"201": formatHEVCEAC3, "202": formatH264AC3, "203": formatH264AAC}})
	suite := []struct{ chaine, mode string }{{"201", "video"}, {"202", "audio"}, {"203", "direct"}}
	for i := 0; i < 10; i++ {
		s := suite[i%3]
		r := b.playlist(s.chaine, "s1", "")
		if r.Code != 200 || r.Header().Get("X-Mytv-Mode") != s.mode {
			t.Fatalf("zapping %d, chaine %s : HTTP %d mode %q", i, s.chaine, r.Code, r.Header().Get("X-Mytv-Mode"))
		}
		if n := processusDe(b.dossier); n > 1 {
			t.Fatalf("zapping %d : %d processus vivants", i, n)
		}
		d := sousDossiers(b.dossier)
		if s.mode == "direct" && len(d) != 0 || s.mode != "direct" && (len(d) != 1 || !strings.HasPrefix(d[0], s.chaine+"-")) {
			t.Fatalf("zapping %d : dossiers %v", i, d)
		}
	}
	b.relais.FinDeLecture("s1", suite[9%3].chaine)
	b.aucunResidu(t)
	if n := processusDe(b.dossier); n != 0 {
		t.Errorf("%d processus restants", n)
	}
}
