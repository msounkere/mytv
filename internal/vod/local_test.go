package vod

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"mytv/internal/domain"
	"mytv/internal/testutil/fauxiptv"
)

// preparesFaux : un moteur de preparation reduit a sa table des fichiers prets.
type preparesFaux struct {
	mu       sync.Mutex
	fichiers map[domain.ContenuID]string
	tenus    int // verrous de lecture en cours
	liberes  int
}

func (p *preparesFaux) Pret(id domain.ContenuID) (string, func(), bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c, ok := p.fichiers[id]
	if !ok {
		return "", nil, false
	}
	p.tenus++
	return c, func() { p.mu.Lock(); p.tenus--; p.liberes++; p.mu.Unlock() }, true
}

func (p *preparesFaux) etat() (tenus, liberes int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.tenus, p.liberes
}

// bancPrepare : le banc habituel, plus un fichier prepare pour iptva:2001 (le contenu
// exact du faux fournisseur) ; les compteurs du faux repartent de zero.
func bancPrepare(t *testing.T) (*banc, *preparesFaux, []byte) {
	t.Helper()
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	attendu := b.contenuAttendu()
	chemin := filepath.Join(t.TempDir(), "film-iptva-2001.mp4")
	if err := os.WriteFile(chemin, attendu, 0o600); err != nil {
		t.Fatal(err)
	}
	p := &preparesFaux{fichiers: map[domain.ContenuID]string{"iptva:2001": chemin}}
	b.svc.dep.Prepares = p
	b.faux.RazCompteurs()
	return b, p, attendu
}

// CA5 : un film prepare est servi depuis le disque, octet pour octet, en mode direct avec
// `X-Mytv-Source: prepare`, SANS la moindre requete au fournisseur et SANS bail ; Range
// exact (206, Content-Range) ; un seek loin dans le fichier repond en moins de 100 ms ;
// HEAD sans corps ; plage hors fichier : 416 ; le fichier est verrouille pendant la
// lecture et libere apres.
func TestCA5_FilmPrepareServiDepuisLeDisqueSansFournisseurNiBail(t *testing.T) {
	b, p, attendu := bancPrepare(t)
	rep, corps, err := b.get("iptva:2001", "s1", "")
	if err != nil || rep.StatusCode != 200 {
		t.Fatalf("statut %d, %v", rep.StatusCode, err)
	}
	if !bytes.Equal(corps, attendu) {
		t.Errorf("corps different (%d octets)", len(corps))
	}
	if rep.Header.Get("X-Mytv-Source") != "prepare" || rep.Header.Get("X-Mytv-Mode") != "direct" ||
		rep.Header.Get("Accept-Ranges") != "bytes" || rep.Header.Get("Content-Type") != "video/mp4" || rep.ContentLength != tailleFilm {
		t.Errorf("en-tetes : %v", rep.Header)
	}
	if n := b.faux.Compteurs().Films; n != 0 {
		t.Errorf("%d requete(s) au fournisseur pour un fichier prepare", n)
	}
	if acq := b.baux.acquisitions(); len(acq) != 0 {
		t.Errorf("bail acquis pour un fichier prepare : %v", acq)
	}
	if tenus, liberes := p.etat(); tenus != 0 || liberes != 1 {
		t.Errorf("verrou de lecture : tenus %d, liberes %d", tenus, liberes)
	}
	// Seek loin dans le fichier : 206 exact, en moins de 100 ms.
	debut := time.Now()
	rep, corps, err = b.get("iptva:2001", "s1", "bytes=290000-")
	if d := time.Since(debut); err != nil || rep.StatusCode != 206 || d > 100*time.Millisecond {
		t.Fatalf("seek : statut %d, %v, %v", rep.StatusCode, err, d)
	}
	if !bytes.Equal(corps, attendu[290000:]) || rep.Header.Get("Content-Range") != "bytes 290000-299999/300000" || rep.Header.Get("X-Mytv-Source") != "prepare" {
		t.Errorf("seek : %d octets, Content-Range %q", len(corps), rep.Header.Get("Content-Range"))
	}
	rep, corps, _ = b.get("iptva:2001", "s1", "bytes=-500")
	if rep.StatusCode != 206 || !bytes.Equal(corps, attendu[tailleFilm-500:]) {
		t.Errorf("suffixe : statut %d, %d octets", rep.StatusCode, len(corps))
	}
	// HEAD : les en-tetes, aucun octet, toujours sans fournisseur.
	req, _ := http.NewRequest("HEAD", b.http.URL+"/?id=iptva:2001", nil)
	req.Header.Set("X-Session", "s1")
	rh, err := http.DefaultClient.Do(req)
	if err != nil || rh.StatusCode != 200 || rh.Header.Get("X-Mytv-Source") != "prepare" || rh.ContentLength != tailleFilm {
		t.Errorf("HEAD : %v %v", rh, err)
	}
	if rh != nil {
		rh.Body.Close()
	}
	// Hors fichier : 416, rien du fournisseur.
	if rep, _, _ = b.get("iptva:2001", "s1", "bytes=400000-"); rep.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("hors fichier : statut %d", rep.StatusCode)
	}
	if n := b.faux.Compteurs().Films; n != 0 || len(b.baux.acquisitions()) != 0 {
		t.Errorf("fournisseur sollicite : %d requetes, %d baux", n, len(b.baux.acquisitions()))
	}
	// Un contenu SANS fichier prepare suit le chemin habituel : fournisseur et bail.
	delete(p.fichiers, "iptva:2001")
	rep, corps, _ = b.get("iptva:2001", "s2", "")
	if rep.StatusCode != 200 || !bytes.Equal(corps, attendu) || rep.Header.Get("X-Mytv-Source") != "" {
		t.Errorf("sans fichier prepare : statut %d, source %q", rep.StatusCode, rep.Header.Get("X-Mytv-Source"))
	}
	if n := b.faux.Compteurs().Films; n != 1 || len(b.baux.acquisitions()) != 1 {
		t.Errorf("chemin habituel : %d requetes, %d baux", n, len(b.baux.acquisitions()))
	}
}

// CA5 : la meme decision de mode s'applique au fichier prepare : un mkv passe par le
// remux, dont la source lit le DISQUE (Range exact, taille connue), sans fournisseur ni bail.
func TestCA5_FichierPrepareRemuxeDepuisLeDisque(t *testing.T) {
	b, _, attendu := bancPrepare(t)
	b.sondeur.sonde = sondeMKV
	b.lanceur.surLancer = b.nourrir("ftypISOM-fragments", true)
	rep, corps, err := b.get("iptva:2001", "s1", "")
	if err != nil || rep.StatusCode != 200 || string(corps) != "ftypISOM-fragments" {
		t.Fatalf("statut %d, corps %q, %v", rep.StatusCode, corps, err)
	}
	if rep.Header.Get("X-Mytv-Source") != "prepare" || rep.Header.Get("X-Mytv-Mode") != "remux" || rep.Header.Get("Accept-Ranges") != "none" {
		t.Errorf("en-tetes : %v", rep.Header)
	}
	procs := b.lanceur.liste()
	if len(procs) != 1 {
		t.Fatalf("%d ffmpeg", len(procs))
	}
	// La source de la tache lit le fichier local : un octet exact, la taille totale.
	rc, err := procs[0].tache.Source(context.Background(), 1000)
	if err != nil {
		t.Fatal(err)
	}
	lu, _ := io.ReadAll(rc)
	rc.Close()
	if !bytes.Equal(lu, attendu[1000:]) || rc.(domain.AvecTaille).Taille() != tailleFilm {
		t.Errorf("source locale : %d octets, taille %d", len(lu), rc.(domain.AvecTaille).Taille())
	}
	if _, err := procs[0].tache.Source(context.Background(), tailleFilm); err == nil {
		t.Error("ouverture au-dela du fichier acceptee")
	}
	// La sonde a recu la meme source locale.
	if len(b.sondeur.sources) != 1 {
		t.Fatalf("%d sondes", len(b.sondeur.sources))
	}
	if n := b.faux.Compteurs().Films; n != 0 || len(b.baux.acquisitions()) != 0 {
		t.Errorf("fournisseur sollicite : %d requetes, %d baux", n, len(b.baux.acquisitions()))
	}
}
