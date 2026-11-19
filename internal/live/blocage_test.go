package live

import (
	"mytv/internal/config"

	"strings"
	"sync"
	"testing"
	"time"
)

// Test reel du 2026-11-19 : connexions_max 2, la session 1 lit une chaine, la session 2
// en demande une autre en boucle et recoit 407 du fournisseur. Un segment de la session 1
// est reste bloque 7 min sans reponse. Ici : chaque segment de s1 doit repondre vite.
func TestSegmentNeBloquePasPendantLesRefusDUneAutreSession(t *testing.T) {
	b := nouvelleOccupation(t, 2, true)
	b.faux.UneSeuleConnexion(time.Hour)
	pl := b.get(PrefixeParDefaut+"/1001.m3u8", "s1")
	segs := cheminsDeSegments(pl.Body.String())
	if pl.Code != 200 || len(segs) < 5 {
		t.Fatalf("playlist : %d %s", pl.Code, pl.Body.String())
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				b.get(PrefixeParDefaut+"/1002.m3u8", "s2")
			}
		}()
	}
	defer func() { close(stop); wg.Wait() }()
	time.Sleep(200 * time.Millisecond)
	for _, s := range segs[:4] {
		fini := make(chan int, 1)
		go func() { fini <- b.get(s, "s1").Code }()
		select {
		case c := <-fini:
			if c != 200 {
				t.Errorf("segment de s1 : %d", c)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("segment de s1 bloque plus de 5 s pendant les refus de s2")
		}
	}
}

// File des requetes amont pleine (places jamais rendues) : un segment ne doit plus attendre
// indefiniment, mais repondre une erreur apres reseau.delai_segment.
func TestSegmentFileAmontSatureeRepondApresLeDelai(t *testing.T) {
	b := nouvelleOccupation(t, 2, true)
	pl := b.get(PrefixeParDefaut+"/1001.m3u8", "s1")
	segs := cheminsDeSegments(pl.Body.String())
	if pl.Code != 200 || len(segs) < 2 {
		t.Fatalf("playlist : %d", pl.Code)
	}
	b.relais.opt.Reseau.DelaiSegment = config.Duree(300 * time.Millisecond)
	for len(b.relais.amont) < cap(b.relais.amont) {
		b.relais.amont <- struct{}{}
	}
	fini := make(chan int, 1)
	debut := time.Now()
	go func() { fini <- b.get(segs[1], "s1").Code }()
	select {
	case c := <-fini:
		if c < 500 {
			t.Errorf("code %d, attendu une erreur", c)
		}
		if d := time.Since(debut); d < 250*time.Millisecond {
			t.Errorf("reponse en %s : le delai n'a pas ete attendu", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("segment toujours bloque sur la file amont pleine")
	}
	if !strings.Contains(b.journal.String(), "file des requetes amont saturee") {
		t.Error("aucun journal de saturation")
	}
}
