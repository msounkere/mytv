package vod

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"mytv/internal/domain"
	"mytv/internal/testutil/fauxiptv"
)

// chainesFaux : catalogue des chaines pour le replay.
type chainesFaux map[domain.ContenuID]domain.Chaine

func (c chainesFaux) Chaine(_ context.Context, id domain.ContenuID) (*domain.Chaine, error) {
	if ch, ok := c[id]; ok {
		return &ch, nil
	}
	return nil, domain.ErrNonTrouve
}

// 21/11/2026 20:00 UTC ; le serveur du fournisseur est a Amsterdam (UTC+1 en novembre).
var (
	maintenantReplay = time.Date(2026, 11, 21, 20, 0, 0, 0, time.UTC)
	jt13h            = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC) // 13:00 a Amsterdam
	octetsReplay     = bytes.Repeat([]byte("TS-REPLAY-"), 5000)
)

// bancReplay : TF1 (553959) a 2 jours de replay et sert le programme de 13:00 (heure du
// serveur) ; France 3 (553961) annonce 3 jours mais ne sert rien ; Arte (553970) n'en a pas.
func bancReplay(t *testing.T) *banc {
	b := nouveauBanc(t, fauxiptv.Options{
		ServerInfo: map[string]any{"timezone": "Europe/Amsterdam"},
		Replay: func(chaine, debut string, minutes int) ([]byte, bool) {
			return octetsReplay, chaine == "553959"
		},
	}, nil)
	b.svc.dep.Chaines = chainesFaux{
		"iptva:553959": {ID: "iptva:553959", FournisseurID: "iptva", Numero: "553959", ReplayJours: 2},
		"iptva:553961": {ID: "iptva:553961", FournisseurID: "iptva", Numero: "553961", ReplayJours: 3},
		"iptva:553970": {ID: "iptva:553970", FournisseurID: "iptva", Numero: "553970"},
	}
	b.svc.dep.Horloge = func() time.Time { return maintenantReplay }
	// ffmpeg de comedie : recopie la source (le flux du fournisseur) telle quelle.
	b.lanceur.surLancer = func(p *procTest) {
		rc, err := p.tache.Source(context.Background(), 0)
		if err != nil {
			p.errSrc = err
			p.mourir(err)
			return
		}
		defer rc.Close()
		_, err = io.Copy(p.pw, rc)
		p.mourir(err)
	}
	return b
}

func (b *banc) getReplay(id domain.ContenuID, t string) (*http.Response, []byte) {
	b.t.Helper()
	u := b.http.URL + "/?id=" + url.QueryEscape(string(id))
	if t != "" {
		u += "&t=" + t
	}
	rep, err := http.Get(u)
	if err != nil {
		b.t.Fatal(err)
	}
	defer rep.Body.Close()
	corps, _ := io.ReadAll(rep.Body)
	return rep, corps
}

// Le JT de 13 h de TF1 : demande au fournisseur a 13-00 (heure de SON serveur), duree du
// programme + 5 min de marge, en .ts ; audio converti en AAC ; duree annoncee au lecteur.
func TestReplayLuCommeUnFilm(t *testing.T) {
	b := bancReplay(t)
	rep, corps := b.getReplay(IDReplay("iptva", "553959", jt13h, 40), "")
	if rep.StatusCode != 200 || !bytes.Equal(corps, octetsReplay) {
		t.Fatalf("HTTP %d, %d octets : %v", rep.StatusCode, len(corps), b.derniereErreur())
	}
	if got := b.faux.Replays(); len(got) != 1 || got[0] != "553959 2026-11-21:13-00 45 ts" {
		t.Errorf("demandes au fournisseur : %q", got)
	}
	if d := rep.Header.Get("X-Mytv-Duree"); d != "2400" {
		t.Errorf("X-Mytv-Duree = %q, attendu 2400", d)
	}
	p := b.lanceur.liste()[0]
	if p.tache.Mode != domain.ModeAudioConverti || p.tache.Debut != 0 {
		t.Errorf("tache : mode %q debut %s", p.tache.Mode, p.tache.Debut)
	}
}

// Se deplacer : on redemande a partir de la minute voulue (le fournisseur ne saute pas
// dans le flux), ffmpeg saute le reste (< 1 min).
func TestReplaySautALaMinutePuisReste(t *testing.T) {
	b := bancReplay(t)
	rep, _ := b.getReplay(IDReplay("iptva", "553959", jt13h, 40), "725") // 12 min 5 s
	if rep.StatusCode != 200 {
		t.Fatalf("HTTP %d : %v", rep.StatusCode, b.derniereErreur())
	}
	if got := b.faux.Replays(); len(got) != 1 || got[0] != "553959 2026-11-21:13-12 33 ts" {
		t.Errorf("demandes au fournisseur : %q", got)
	}
	if p := b.lanceur.liste()[0]; p.tache.Debut != 5*time.Second {
		t.Errorf("reste a sauter : %s", p.tache.Debut)
	}
	if rep, _ := b.getReplay(IDReplay("iptva", "553959", jt13h, 40), "2400"); rep.StatusCode != 400 {
		t.Errorf("position a la fin du programme : HTTP %d, attendu 400", rep.StatusCode)
	}
}

// Tout ce qui n'est pas un replay possible est refuse AVANT d'appeler le fournisseur.
func TestReplayRefusSansAppelAuFournisseur(t *testing.T) {
	b := bancReplay(t)
	cas := []struct {
		nom     string
		id      domain.ContenuID
		statut  int
		replayI bool // erreur ErrReplayIndisponible attendue
	}{
		{"chaine sans replay", IDReplay("iptva", "553970", jt13h, 40), 404, true},
		{"chaine inconnue", IDReplay("iptva", "999", jt13h, 40), 404, false},
		{"pas encore diffuse", IDReplay("iptva", "553959", maintenantReplay.Add(time.Hour), 40), 404, true},
		{"trop ancien (TF1 : 2 jours)", IDReplay("iptva", "553959", maintenantReplay.Add(-49*time.Hour), 40), 404, true},
		{"duree nulle", IDReplay("iptva", "553959", jt13h, 0), 404, false},
		{"duree excessive", IDReplay("iptva", "553959", jt13h, 481), 404, false},
		{"forme invalide", "iptva:replay-553959-abc-40", 404, false},
		{"morceau en trop", "iptva:replay-553959-1795260000-40-1", 404, false},
	}
	for _, c := range cas {
		rep, _ := b.getReplay(c.id, "")
		if rep.StatusCode != c.statut {
			t.Errorf("%s : HTTP %d, attendu %d", c.nom, rep.StatusCode, c.statut)
		}
		if err := b.derniereErreur(); c.replayI != errors.Is(err, domain.ErrReplayIndisponible) {
			t.Errorf("%s : erreur %v", c.nom, err)
		}
	}
	if got := b.faux.Replays(); len(got) != 0 {
		t.Errorf("le fournisseur a ete appele : %q", got)
	}
	if n := len(b.baux.acquisitions()); n != 0 {
		t.Errorf("%d bail pris pour des demandes refusees", n)
	}
}

// France 3 : annoncee avec replay, le fournisseur repond 200 text/html vide. C'est
// « replay indisponible » (404 cote API), pas une panne du fournisseur.
func TestReplayVideDuFournisseurEstIndisponible(t *testing.T) {
	b := bancReplay(t)
	rep, _ := b.getReplay(IDReplay("iptva", "553961", jt13h, 40), "")
	if rep.StatusCode == 200 {
		t.Fatal("un replay vide ne doit pas etre servi")
	}
	if err := b.derniereErreur(); !errors.Is(err, domain.ErrReplayIndisponible) {
		t.Errorf("erreur %v, attendu ErrReplayIndisponible", err)
	}
	if got := b.faux.Replays(); len(got) != 1 || !strings.HasPrefix(got[0], "553961 ") {
		t.Errorf("demandes au fournisseur : %q", got)
	}
}

// HEAD (ce que lit l'interface) : 200 et duree pour TF1 sans lancer ffmpeg ; 404 pour France 3
// (rien chez le fournisseur) au lieu d'un 200 trompeur suivi d'un echec muet du <video>.
func TestReplayHEADVerifieLeFournisseur(t *testing.T) {
	b := bancReplay(t)
	tete := func(id domain.ContenuID) *http.Response {
		req, _ := http.NewRequest("HEAD", b.http.URL+"/?id="+url.QueryEscape(string(id)), nil)
		rep, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		rep.Body.Close()
		return rep
	}
	if rep := tete(IDReplay("iptva", "553959", jt13h, 40)); rep.StatusCode != 200 || rep.Header.Get("X-Mytv-Duree") != "2400" {
		t.Errorf("TF1 : HTTP %d, duree %q", rep.StatusCode, rep.Header.Get("X-Mytv-Duree"))
	}
	if n := len(b.lanceur.liste()); n != 0 {
		t.Errorf("HEAD a lance %d ffmpeg", n)
	}
	if rep := tete(IDReplay("iptva", "553961", jt13h, 40)); rep.StatusCode != 404 {
		t.Errorf("France 3 : HTTP %d, attendu 404", rep.StatusCode)
	}
}
