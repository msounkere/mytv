package live

import (
	"bytes"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func playlistSeq(seq int, n int, duree string) string {
	var b strings.Builder
	b.WriteString("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:12\n#EXT-X-MEDIA-SEQUENCE:")
	b.WriteString(itoa(seq))
	b.WriteString("\n")
	for i := 0; i < n; i++ {
		b.WriteString("#EXTINF:" + duree + ",\n/hls/x/100_" + itoa(seq+i) + ".ts\n")
	}
	return b.String()
}

func itoa(n int) string { return strconv.Itoa(n) }

func TestLireSequenceEtDureeMoyenne(t *testing.T) {
	seq, n, moy, ok := lireSequence(playlistSeq(770, 3, "9.6"))
	if !ok || seq != 770 || n != 3 || moy < 9.59 || moy > 9.61 {
		t.Fatalf("seq=%d n=%d moy=%v ok=%v", seq, n, moy, ok)
	}
	if _, _, _, ok := lireSequence("#EXTM3U\n#EXTINF:5,\na.ts\n"); ok {
		t.Fatal("sans MEDIA-SEQUENCE, la sequence est inconnue")
	}
}

// Regression (recette reelle) : on doit pouvoir DIRE que la playlist du fournisseur
// n'avance plus, avec le numero de sequence, au lieu de deviner.
func TestObserverDetecteUnePlaylistImmobile(t *testing.T) {
	r := &Relais{}
	t0 := time.Date(2026, 10, 16, 1, 0, 0, 0, time.UTC)

	o, ok := r.observer("c", playlistSeq(10, 3, "10"), t0)
	if !ok || o.Connue || o.Avance {
		t.Fatalf("premiere observation : %+v", o)
	}
	o, _ = r.observer("c", playlistSeq(11, 3, "10"), t0.Add(10*time.Second))
	if !o.Avance || o.ImmobileS != 0 {
		t.Fatalf("la sequence a avance : %+v", o)
	}
	o, _ = r.observer("c", playlistSeq(11, 3, "10"), t0.Add(40*time.Second))
	if o.Avance || o.ImmobileS < 29.9 || o.ImmobileS > 30.1 {
		t.Fatalf("immobile depuis 30 s attendu : %+v", o)
	}
	// Meme numero mais un segment de plus : la fenetre se remplit (flux redemarre), ca avance.
	o, _ = r.observer("c", playlistSeq(11, 4, "10"), t0.Add(50*time.Second))
	if !o.Avance || o.ImmobileS != 0 {
		t.Fatalf("une fenetre qui grandit n'est pas immobile : %+v", o)
	}
	// Une autre chaine est suivie separement.
	if o2, _ := r.observer("autre", playlistSeq(11, 3, "10"), t0.Add(40*time.Second)); o2.Connue {
		t.Fatal("le suivi doit etre par chaine")
	}
}

func TestSignalerSequenceAvertitSeulementSiImmobile(t *testing.T) {
	b := nouveauBancSeq(t)
	b.r.signalerSequence("c", observation{Sequence: 5, Segments: 6, Dernier: 10, ImmobileS: 15})
	if strings.Contains(b.logs.String(), "immobile") {
		t.Fatalf("15 s < 2 segments : pas d'avertissement :\n%s", b.logs.String())
	}
	b.r.signalerSequence("c", observation{Sequence: 5, Segments: 6, Dernier: 10, ImmobileS: 45})
	if !strings.Contains(b.logs.String(), "playlist du fournisseur immobile") || !strings.Contains(b.logs.String(), "sequence=5") {
		t.Fatalf("avertissement attendu avec le numero de sequence :\n%s", b.logs.String())
	}
}

// tamponJournal : un io.Writer lisible pendant que slog ecrit.
type tamponJournal struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (t *tamponJournal) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.b.Write(p)
}
func (t *tamponJournal) String() string { t.mu.Lock(); defer t.mu.Unlock(); return t.b.String() }

type bancSeq struct {
	r    *Relais
	logs *tamponJournal
}

func nouveauBancSeq(t *testing.T) *bancSeq {
	t.Helper()
	logs := &tamponJournal{}
	b := nouveauBanc(t, amontSimple(nil), func(o *Options) {
		o.Journal = slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	})
	return &bancSeq{r: b.relais, logs: logs}
}
