package live

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mytv/internal/hls"
	"mytv/internal/remux"
)

// getSous : comme banc.get, avec un prefixe de diffusion impose dans le contexte (ce que fait
// la route cast de l'API pour une requete /api/v1/cast/<jeton>/live/...).
func (b *banc) getSous(prefixe, chemin, session string) *httptest.ResponseRecorder {
	b.t.Helper()
	req := httptest.NewRequest("GET", chemin, nil)
	req.Header.Set("X-Session", session)
	req = req.WithContext(AvecPrefixe(req.Context(), prefixe))
	rec := httptest.NewRecorder()
	b.h.ServeHTTP(rec, req)
	return rec
}

// US-40 CA3 : le prefixe des segments est derive de la requete (contexte), pas d'une
// constante : sous un prefixe impose, la playlist (y compris celle du cache, partagee avec
// les lecteurs a cookie), la sous-playlist d'une playlist maitresse et la cle de chiffrement
// restent sous ce prefixe ; sans prefixe impose, rien ne change.
func TestCA3_PrefixeDesSegmentsDeriveDuContexte(t *testing.T) {
	const cast = "/api/v1/cast/JETON/live"
	mux := http.NewServeMux()
	mux.HandleFunc("/live/", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "#EXTM3U\n#EXT-X-STREAM-INF:BANDWIDTH=1\nhaute/index.m3u8\n")
	})
	mux.HandleFunc("/live/u/p/haute/index.m3u8", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		io.WriteString(w, "#EXTM3U\n#EXT-X-KEY:METHOD=AES-128,URI=\"cle.key\"\n#EXTINF:1,\nseg1.ts\n")
	})
	mux.HandleFunc("/live/u/p/haute/seg1.ts", func(w http.ResponseWriter, r *http.Request) { corpsDe(w, 7) })
	b := nouveauBanc(t, mux, nil)

	// Un lecteur a cookie a deja fait reecrire et mettre en cache la playlist de la chaine.
	normal := chaineSeg(t, b, "100")
	if len(normal) != 1 || !strings.HasPrefix(normal[0], PrefixeParDefaut+"/100/seg/") {
		t.Fatalf("sans prefixe impose, la racine du relais : %v", normal)
	}

	// Le televiseur lit la MEME playlist (cache) sous son prefixe.
	rec := b.getSous(cast, PrefixeParDefaut+"/100.m3u8", "tv")
	if rec.Code != 200 {
		t.Fatalf("playlist sous prefixe : %d %s", rec.Code, rec.Body.String())
	}
	sous := segments(rec.Body.String())
	if len(sous) != 1 || !strings.HasPrefix(sous[0], cast+"/100/seg/") || strings.Contains(rec.Body.String(), PrefixeParDefaut+"/") {
		t.Fatalf("sous-playlist annoncee hors du prefixe impose :\n%s", rec.Body.String())
	}
	// Meme jeton que pour le lecteur a cookie : seul le prefixe change.
	if strings.TrimPrefix(sous[0], cast) != strings.TrimPrefix(normal[0], PrefixeParDefaut) {
		t.Fatalf("le jeton doit etre le meme, seul le prefixe change : %s / %s", sous[0], normal[0])
	}

	// La sous-playlist, lue sous le prefixe, emet segments ET cle sous le prefixe.
	rec = b.getSous(cast, PrefixeParDefaut+strings.TrimPrefix(sous[0], cast), "tv")
	if rec.Code != 200 {
		t.Fatalf("sous-playlist : %d %s", rec.Code, rec.Body.String())
	}
	corps := rec.Body.String()
	seg := segments(corps)
	if len(seg) != 1 || !strings.HasPrefix(seg[0], cast+"/100/seg/") {
		t.Fatalf("segment de la sous-playlist hors prefixe : %v", seg)
	}
	if !strings.Contains(corps, `URI="`+cast+`/100/seg/`) || strings.Contains(corps, PrefixeParDefaut+"/") {
		t.Fatalf("cle de chiffrement hors prefixe :\n%s", corps)
	}
	// Et le segment se lit par son chemin reel.
	if rec := b.get(PrefixeParDefaut+strings.TrimPrefix(seg[0], cast), "tv"); rec.Code != 200 || rec.Body.Len() != 7 {
		t.Errorf("segment : %d, %d octets", rec.Code, rec.Body.Len())
	}

	// Apres le televiseur, un lecteur a cookie voit toujours la racine du relais (le cache
	// n'a pas ete reecrit sous le prefixe du televiseur).
	if apres := chaineSeg(t, b, "100"); len(apres) != 1 || apres[0] != normal[0] {
		t.Fatalf("le cache partage ne doit pas changer : %v puis %v", normal, apres)
	}
}

// US-40 CA3 : les segments de conversion (playlist de ffmpeg) suivent aussi le prefixe de la
// requete.
func TestCA3_SegmentsDeConversionSousLePrefixe(t *testing.T) {
	sceau, err := hls.NouveauSceau(secretTest, "test/a", "test/b")
	if err != nil {
		t.Fatal(err)
	}
	r := &Relais{opt: Options{Prefixe: PrefixeParDefaut, Horloge: time.Now}, tc: &conversions{sceau: sceau}}
	dossier := t.TempDir()
	pl := "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:4\n#EXT-X-MEDIA-SEQUENCE:0\n"
	for i := 0; i < 2; i++ {
		nom := fmt.Sprintf("aaaa1111_%05d.ts", i)
		if err := os.WriteFile(filepath.Join(dossier, nom), []byte("ts"), 0o644); err != nil {
			t.Fatal(err)
		}
		pl += "#EXTINF:4.000000,\n" + nom + "\n"
	}
	if err := os.WriteFile(filepath.Join(dossier, remux.NomPlaylistDirect), []byte(pl), 0o644); err != nil {
		t.Fatal(err)
	}
	tc := &transcodage{chaine: "100", dossier: dossier, prefixe: "aaaa1111"}
	corps, err := r.playlistLocale(AvecPrefixe(context.Background(), "/api/v1/cast/J/live"), tc)
	if err != nil {
		t.Fatal(err)
	}
	seg := segments(corps)
	if len(seg) != 2 || !strings.HasPrefix(seg[0], "/api/v1/cast/J/live/100/seg/") || strings.Contains(corps, PrefixeParDefaut) {
		t.Fatalf("segments de conversion hors prefixe :\n%s", corps)
	}
	if corps, _ = r.playlistLocale(context.Background(), tc); !strings.HasPrefix(segments(corps)[0], PrefixeParDefaut+"/100/seg/") {
		t.Fatalf("sans prefixe impose, la racine du relais :\n%s", corps)
	}
}
