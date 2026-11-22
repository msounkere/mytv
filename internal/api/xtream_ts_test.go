package api

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"mytv/internal/domain"
)

// relaisDirectFaux : un direct dont la fenetre de 5 segments avance d'un cran a chaque playlist,
// qui exige une session (comme le vrai relais) et note les fins de lecture.
type relaisDirectFaux struct {
	mu       sync.Mutex
	auth     func(*http.Request) (string, bool)
	appels   int
	occupe   bool
	fige     bool // la fenetre n'avance plus (le fournisseur tarde a publier)
	fins     []string
	sessions map[string]bool
}

func (r *relaisDirectFaux) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	s, ok := r.auth(req)
	if !ok {
		http.Error(w, "session requise", http.StatusUnauthorized)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions[s] = true
	if strings.HasSuffix(req.URL.Path, ".m3u8") {
		if r.occupe {
			w.Header().Set("Retry-After", "15")
			http.Error(w, "occupe", http.StatusServiceUnavailable)
			return
		}
		if !r.fige || r.appels == 0 {
			r.appels++
		}
		seq := 100 + r.appels
		fmt.Fprintf(w, "#EXTM3U\n#EXT-X-TARGETDURATION:1\n#EXT-X-MEDIA-SEQUENCE:%d\n", seq)
		for i := 0; i < 5; i++ {
			fmt.Fprintf(w, "#EXTINF:1,\n/api/v1/live/100/seg/s%d\n", seq+i)
		}
		return
	}
	_, nom, _ := strings.Cut(req.URL.Path, "/seg/")
	fmt.Fprintf(w, "SEG%s;", strings.TrimPrefix(nom, "s"))
}

func (r *relaisDirectFaux) FinDeLecture(session, chaine string) {
	r.mu.Lock()
	r.fins = append(r.fins, chaine)
	r.mu.Unlock()
}

func nouveauBancTS(t *testing.T) (*bancXtream, *relaisDirectFaux) {
	rel := &relaisDirectFaux{sessions: map[string]bool{}}
	l := &lectureNative{}
	var st *stockageFaux
	b := nouveauBanc(t, func(o *Options) {
		st = o.Stockage.(*stockageFaux)
		o.Export = catalogueFaux{st: st}
		o.Lecture = l
		o.Live = rel
		o.FinLive = rel
	})
	rel.auth = b.svc.AuthentifierLecture
	st.chaines = []domain.Chaine{{ID: "a:100", FournisseurID: "a", Numero: "100", Nom: "FR - TF1", CategorieID: "10"}}
	if _, err := b.svc.CreerUtilisateur(context.Background(), "formuler", mdp, false); err != nil {
		t.Fatal(err)
	}
	return &bancXtream{banc: b, x: b.api.HandlerXtream(cfgXtream), lecture: l}, rel
}

// Le direct en « .ts » : un flux continu, segments dans l'ordre sans doublon ni trou, demarre a
// deux segments du bord du direct, au nom de la session de la box ; la place est rendue quand
// la box coupe.
func TestXtreamDirectTSContinu(t *testing.T) {
	b, rel := nouveauBancTS(t)
	srv := httptest.NewServer(b.x)
	defer srv.Close()
	ctx, couper := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/live/"+cheminIdent+"/100.ts", nil)
	rep, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if rep.StatusCode != 200 || rep.Header.Get("Content-Type") != "video/mp2t" {
		t.Fatalf("%d %q", rep.StatusCode, rep.Header.Get("Content-Type"))
	}
	lu := bufio.NewReader(rep.Body)
	var segs []string
	limite := time.Now().Add(10 * time.Second)
	for len(segs) < 7 && time.Now().Before(limite) {
		s, err := lu.ReadString(';')
		if err != nil {
			break
		}
		// Des paquets nuls (bourrage) peuvent preceder un segment : seul « SEGn » compte.
		if i := strings.Index(s, "SEG"); i >= 0 {
			segs = append(segs, strings.TrimSuffix(s[i:], ";"))
		}
	}
	couper()
	rep.Body.Close()
	if len(segs) < 7 {
		t.Fatalf("segments recus : %v", segs)
	}
	// Premiere playlist : sequences 101..105 ; depart a 3 segments du bord -> 103.
	if segs[0] != "SEG103" {
		t.Errorf("premier segment %s, attendu SEG103 (trois segments derriere le direct)", segs[0])
	}
	for i := 1; i < len(segs); i++ {
		var a, b int
		fmt.Sscanf(segs[i-1], "SEG%d", &a)
		fmt.Sscanf(segs[i], "SEG%d", &b)
		if b != a+1 {
			t.Errorf("ordre rompu : %v", segs)
			break
		}
	}
	attendreQue(t, func() bool { rel.mu.Lock(); defer rel.mu.Unlock(); return len(rel.fins) == 1 }, "place rendue au depart de la box")
	if len(rel.sessions) != 1 {
		t.Errorf("sessions vues par le relais : %v", rel.sessions)
	}
}

// Place prise chez le fournisseur avant le premier octet : la box recoit le vrai statut.
func TestXtreamDirectTSOccupe(t *testing.T) {
	b, rel := nouveauBancTS(t)
	rel.occupe = true
	w := b.get("/live/" + cheminIdent + "/100.ts")
	if w.Code != 503 || w.Header().Get("Retry-After") != "15" {
		t.Errorf("%d %q", w.Code, w.Header().Get("Retry-After"))
	}
}

func attendreQue(t *testing.T, f func() bool, quoi string) {
	t.Helper()
	for fin := time.Now().Add(5 * time.Second); time.Now().Before(fin); time.Sleep(20 * time.Millisecond) {
		if f() {
			return
		}
	}
	t.Errorf("delai depasse : %s", quoi)
}

// Le fournisseur tarde a publier : la box recoit des paquets nuls (188 octets, PID 0x1FFF) chaque
// seconde au lieu d'un flux muet qu'elle abandonnait (image figee puis reconnexion, 22/11).
func TestXtreamDirectTSBourrageQuandLeDirectTarde(t *testing.T) {
	b, rel := nouveauBancTS(t)
	rel.fige = true
	srv := httptest.NewServer(b.x)
	defer srv.Close()
	ctx, couper := context.WithCancel(context.Background())
	defer couper()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/live/"+cheminIdent+"/100.ts", nil)
	rep, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer rep.Body.Close()
	lu := bufio.NewReader(rep.Body)
	// Les segments de depart (103..105), puis rien de nouveau : du bourrage.
	for i := 0; i < 3; i++ {
		if _, err := lu.ReadString(';'); err != nil {
			t.Fatal(err)
		}
	}
	nul := make([]byte, 188)
	debut := time.Now()
	for n := 0; n < 2; n++ {
		if _, err := io.ReadFull(lu, nul); err != nil {
			t.Fatalf("bourrage %d : %v", n, err)
		}
		if nul[0] != 0x47 || nul[1] != 0x1F || nul[2] != 0xFF || nul[187] != 0xFF {
			t.Fatalf("paquet nul mal forme : % x", nul[:8])
		}
	}
	if d := time.Since(debut); d > 4*time.Second {
		t.Errorf("deux paquets nuls en %s : attendu environ un par seconde", d)
	}
}

// Le delai d'ecriture du serveur (api.delai_ecriture) ne coupe plus le flux sans fin : la box
// etait deconnectee net toutes les 30 s (22/11). Serveur a 1 s de delai, flux lu 3 s.
func TestXtreamDirectTSSurvitAuDelaiDEcritureDuServeur(t *testing.T) {
	b, _ := nouveauBancTS(t)
	srv := httptest.NewUnstartedServer(b.x)
	srv.Config.WriteTimeout = time.Second
	srv.Start()
	defer srv.Close()
	ctx, couper := context.WithCancel(context.Background())
	defer couper()
	req, _ := http.NewRequestWithContext(ctx, "GET", srv.URL+"/live/"+cheminIdent+"/100.ts", nil)
	rep, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer rep.Body.Close()
	lu := bufio.NewReader(rep.Body)
	debut := time.Now()
	n := 0
	for time.Since(debut) < 3*time.Second {
		if _, err := lu.ReadString(';'); err != nil {
			t.Fatalf("flux coupe apres %s (%d segments) : %v", time.Since(debut), n, err)
		}
		n++
	}
}
