package main

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

type tampon struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (t *tampon) Write(p []byte) (int, error) { t.mu.Lock(); defer t.mu.Unlock(); return t.b.Write(p) }
func (t *tampon) String() string              { t.mu.Lock(); defer t.mu.Unlock(); return t.b.String() }

// CA1/CA2 : la commande affiche les URL, sert, puis s'arrete proprement (code 0)
// a l'annulation du contexte (ce que font SIGINT et SIGTERM).
func TestDemarreAfficheLesURLEtS_arreteProprement(t *testing.T) {
	ctx, annuler := context.WithCancel(context.Background())
	defer annuler()
	var out, errb tampon
	code := make(chan int, 1)
	go func() {
		code <- executer(ctx, []string{"--chaines", "20", "--films", "20", "--series", "5", "--latence", "10ms"}, &out, &errb)
	}()
	re := regexp.MustCompile(`mytv \(API \+ interface\) : (http://127\.0\.0\.1:\d+)`)
	var url string
	for i := 0; i < 300 && url == ""; i++ {
		if m := re.FindStringSubmatch(out.String()); m != nil {
			url = m[1]
		}
		time.Sleep(50 * time.Millisecond)
	}
	if url == "" {
		t.Fatalf("pas d'URL affichee. sortie=%q erreur=%q", out.String(), errb.String())
	}
	for _, attendu := range []string{"faux fournisseur", "pilotage des pannes", "utilisateur de test"} {
		if !strings.Contains(out.String(), attendu) {
			t.Errorf("sortie sans %q : %s", attendu, out.String())
		}
	}
	rep, err := http.Get(url + "/api/v1/sante")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, rep.Body)
	rep.Body.Close()
	annuler()
	select {
	case c := <-code:
		if c != 0 {
			t.Errorf("code de sortie %d : %s", c, errb.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatal("la commande ne s'arrete pas")
	}
}

func TestOptionsInvalidesRefusees(t *testing.T) {
	for _, args := range [][]string{
		{"--ecoute", "0.0.0.0:8080"},
		{"--panne", "oups"},
		{"--inconnue"},
		{"--dist", "/chemin/absent"},
	} {
		var out, errb tampon
		if c := executer(context.Background(), args, &out, &errb); c == 0 {
			t.Errorf("%v accepte", args)
		}
	}
}
