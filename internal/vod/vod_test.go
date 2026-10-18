package vod

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mytv/internal/config"
	"mytv/internal/domain"
	"mytv/internal/remux"
	"mytv/internal/testutil/fauxiptv"
)

// ---------------------------------------------------------------- CA1 : relais direct

// CA1 : lecture complete d'un mp4 lisible navigateur, octet pour octet.
func TestDirectLectureComplete(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	rep, corps, err := b.get("iptva:2001", "s1", "")
	if err != nil || rep.StatusCode != 200 {
		t.Fatalf("statut %d, %v", rep.StatusCode, err)
	}
	if !bytes.Equal(corps, b.contenuAttendu()) {
		t.Errorf("corps different (%d octets)", len(corps))
	}
	if rep.Header.Get("Accept-Ranges") != "bytes" || rep.Header.Get("Content-Type") != "video/mp4" ||
		rep.ContentLength != tailleFilm {
		t.Errorf("en-tetes : %v", rep.Header)
	}
	if rep.Header.Get("Location") != "" || rep.Header.Get("Set-Cookie") != "" {
		t.Error("en-tete du fournisseur recopie")
	}
}

// CA1 : Range transmis, 206 et Content-Range coherents, pour les trois formes.
func TestDirectRange(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	tout := b.contenuAttendu()
	cas := []struct {
		plage      string
		debut, fin int
	}{
		{"bytes=1000-1999", 1000, 1999},
		{"bytes=250000-", 250000, tailleFilm - 1},
		{"bytes=-500", tailleFilm - 500, tailleFilm - 1},
		{"bytes=0-0", 0, 0},
		{"bytes=299990-999999", 299990, tailleFilm - 1}, // fin au-dela : le fournisseur la borne
	}
	for _, c := range cas {
		rep, corps, err := b.get("iptva:2001", "s1", c.plage)
		if err != nil || rep.StatusCode != 206 {
			t.Fatalf("%s : statut %d, %v", c.plage, rep.StatusCode, err)
		}
		if !bytes.Equal(corps, tout[c.debut:c.fin+1]) {
			t.Errorf("%s : octets differents", c.plage)
		}
		want := "bytes " + itoa(c.debut) + "-" + itoa(c.fin) + "/300000"
		if rep.Header.Get("Content-Range") != want || rep.ContentLength != int64(c.fin-c.debut+1) {
			t.Errorf("%s : Content-Range %q, Content-Length %d", c.plage, rep.Header.Get("Content-Range"), rep.ContentLength)
		}
	}
	// Le Range part bien chez le fournisseur (et Accept-Encoding: identity).
	vus := rangesVus(b.faux)
	if vus[len(vus)-1] != "bytes=299990-999999" {
		t.Errorf("Range amont = %v", vus)
	}
}

func itoa(n int) string { return strings.TrimSpace(strings.Repeat(" ", 0) + intStr(n)) }
func intStr(n int) string {
	if n == 0 {
		return "0"
	}
	var s []byte
	for ; n > 0; n /= 10 {
		s = append([]byte{byte('0' + n%10)}, s...)
	}
	return string(s)
}

// Une plage mal formee ou multiple est ignoree (RFC 9110) : lecture complete en 200.
func TestDirectPlageIgnoreeSiIllisible(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	for _, p := range []string{"bytes=a-b", "bytes=0-10,20-30", "octets=1-2", "bytes=9-3"} {
		rep, corps, err := b.get("iptva:2001", "s1", p)
		if err != nil || rep.StatusCode != 200 || len(corps) != tailleFilm {
			t.Errorf("%q : statut %d, %d octets, %v", p, rep.StatusCode, len(corps), err)
		}
	}
}

// CA1 : un 200 a la place d'un 206 est refuse, avant d'ecrire le moindre octet.
func TestDirectRefuseUn200ALaPlaceD(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleFilm, IgnorerRange: true, Fois: 1})
	rep, _, _ := b.get("iptva:2001", "s1", "bytes=1000-")
	if rep.StatusCode != http.StatusBadGateway {
		t.Errorf("statut %d, attendu 502", rep.StatusCode)
	}
	if err := b.derniereErreur(); !errors.Is(err, ErrReponseAmont) {
		t.Errorf("erreur = %v", err)
	}
}

// CA1 : la reponse du fournisseur doit correspondre a la plage demandee.
func TestDirectRefuseUnContentRangeIncoherent(t *testing.T) {
	cas := map[string]func(w http.ResponseWriter){
		"mauvais debut": func(w http.ResponseWriter) {
			w.Header().Set("Content-Range", "bytes 0-999/5000")
			w.Header().Set("Content-Length", "1000")
			w.WriteHeader(206)
			w.Write(make([]byte, 1000))
		},
		"sans Content-Range": func(w http.ResponseWriter) { w.WriteHeader(206); w.Write(make([]byte, 10)) },
		"longueur differente": func(w http.ResponseWriter) {
			w.Header().Set("Content-Range", "bytes 100-199/5000")
			w.Header().Set("Content-Length", "50")
			w.WriteHeader(206)
			w.Write(make([]byte, 50))
		},
		"total inconnu": func(w http.ResponseWriter) {
			w.Header().Set("Content-Range", "bytes 100-199/*")
			w.Header().Set("Content-Length", "100")
			w.WriteHeader(206)
			w.Write(make([]byte, 100))
		},
		"fin au-dela de la demande": func(w http.ResponseWriter) {
			w.Header().Set("Content-Range", "bytes 100-4999/5000")
			w.Header().Set("Content-Length", "4900")
			w.WriteHeader(206)
			w.Write(make([]byte, 4900))
		},
	}
	for nom, rep := range cas {
		t.Run(nom, func(t *testing.T) {
			b, up := banc2(t, rep)
			defer up.Close()
			r, _, _ := b.get("iptva:2001", "s1", "bytes=100-199")
			if r.StatusCode != http.StatusBadGateway {
				t.Errorf("statut %d, attendu 502", r.StatusCode)
			}
		})
	}
	// Plein corps sans Content-Length : lecture non verifiable, refusee.
	b, up := banc2(t, func(w http.ResponseWriter) {
		w.Header().Set("Transfer-Encoding", "chunked")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		w.Write([]byte("abc"))
	})
	defer up.Close()
	if r, _, _ := b.get("iptva:2001", "s1", ""); r.StatusCode != http.StatusBadGateway {
		t.Errorf("sans longueur : statut %d", r.StatusCode)
	}
}

// banc2 : un fournisseur « fait main » dont la reponse aux films est donnee.
func banc2(t *testing.T, film func(http.ResponseWriter)) (*banc, *httptest.Server) {
	t.Helper()
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { film(w) }))
	b.rebrancher(up.URL)
	return b, up
}

// CA1 : coupure du fournisseur en cours de film : reprise par Range depuis
// l'octet suivant, sans trou ni doublon.
func TestDirectReprendApresUneCoupure(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleFilm, Coupure: true, Fois: 1})
	rep, corps, err := b.get("iptva:2001", "s1", "")
	if err != nil || rep.StatusCode != 200 {
		t.Fatalf("statut %d, %v", rep.StatusCode, err)
	}
	if !bytes.Equal(corps, b.contenuAttendu()) {
		t.Fatalf("film corrompu apres reprise (%d octets)", len(corps))
	}
	vus := rangesVus(b.faux)
	if len(vus) < 2 || vus[0] != "" || !strings.HasPrefix(vus[1], "bytes=150000-299999") {
		t.Errorf("Range de reprise = %v (attendu bytes=150000-299999)", vus)
	}
	if b.instr.valeur("vod_reprises_total") != 1 {
		t.Error("reprise non comptee")
	}
}

// Idem avec une plage demandee par le client : la reprise reste dans la plage.
func TestDirectReprendUnePlage(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleFilm, Coupure: true, Fois: 1})
	rep, corps, err := b.get("iptva:2001", "s1", "bytes=100000-199999")
	if err != nil || rep.StatusCode != 206 {
		t.Fatalf("statut %d, %v", rep.StatusCode, err)
	}
	if !bytes.Equal(corps, b.contenuAttendu()[100000:200000]) {
		t.Fatal("plage corrompue apres reprise")
	}
	vus := rangesVus(b.faux)
	if len(vus) < 2 || vus[0] != "bytes=100000-199999" || vus[1] != "bytes=150000-199999" {
		t.Errorf("Range amont = %v", vus)
	}
}

// CA1 : si la reprise est mal repondue (le fournisseur ignore Range), on n'invente
// rien : la reponse est tronquee et l'erreur est ErrCoupure.
func TestDirectRefuseUneRepriseIncorrecte(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleFilm, Coupure: true, Fois: 1})
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleFilm, IgnorerRange: true, Fois: 1})
	rep, corps, err := b.get("iptva:2001", "s1", "")
	if rep.StatusCode != 200 {
		t.Fatalf("statut %d", rep.StatusCode)
	}
	if err == nil || len(corps) != tailleFilm/2 {
		t.Errorf("attendu une reponse tronquee a la moitie : %d octets, err=%v", len(corps), err)
	}
	b.abandonAvecCause("coupure")
}

// Les reprises sont comptees : au-dela de remux.reprises_max, on abandonne.
func TestDirectAbandonneApresLesReprises(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, func(r *Reglages) { r.Reprises = 2 })
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleFilm, Coupure: true}) // sans limite
	_, corps, err := b.get("iptva:2001", "s1", "")
	if err == nil || len(corps) >= tailleFilm {
		t.Errorf("%d octets, err=%v : le film ne doit pas etre complet", len(corps), err)
	}
	b.abandonAvecCause("coupure")
	if n := len(rangesVus(b.faux)); n != 3 {
		t.Errorf("%d requetes amont, attendu 1 + 2 reprises", n)
	}
}

// Un fournisseur qui envoie MOINS que la longueur annoncee n'est pas dit complet :
// la connexion cliente est coupee, avec ou sans reprise reussie.
func TestDirectCompteLesOctets(t *testing.T) {
	b, up := banc2(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Length", "100000")
		w.WriteHeader(200)
		w.Write(make([]byte, 40000))
		panic(http.ErrAbortHandler)
	})
	defer up.Close()
	_, corps, err := b.get("iptva:2001", "s1", "")
	if err == nil || len(corps) != 40000 {
		t.Errorf("%d octets, err=%v", len(corps), err)
	}
}

// CA1 : le client part -> la connexion amont est fermee, le bail est rendu.
func TestDirectClientPartiFermeLAmont(t *testing.T) {
	amontFerme := make(chan struct{})
	b, up := banc2(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Length", "100000000")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		for {
			if _, err := w.Write(make([]byte, 1<<14)); err != nil {
				close(amontFerme)
				return
			}
			time.Sleep(time.Millisecond)
		}
	})
	defer up.Close()
	ctx, annule := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", b.http.URL+"/?id=iptva:2001", nil)
	req.Header.Set("X-Session", "s1")
	rep, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadFull(rep.Body, make([]byte, 40000))
	annule()
	rep.Body.Close()
	select {
	case <-amontFerme:
	case <-time.After(5 * time.Second):
		t.Fatal("la connexion amont est restee ouverte")
	}
	attendre(t, func() bool { return b.baux.bail("s1").libere.Load() == 1 }, "bail rendu")
}

// HEAD : en-tetes seuls, rien ne coule.
func TestDirectHEAD(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	req, _ := http.NewRequest("HEAD", b.http.URL+"/?id=iptva:2001", nil)
	req.Header.Set("X-Session", "s1")
	rep, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	rep.Body.Close()
	if rep.StatusCode != 200 || rep.ContentLength != tailleFilm {
		t.Errorf("statut %d, longueur %d", rep.StatusCode, rep.ContentLength)
	}
}

// Un HEAD ne demande qu'UN octet au fournisseur (Range: bytes=0-0), jamais le film entier :
// l'ancien GET complet aussitot ferme jetait une connexion fournisseur juste avant celle du
// <video>, et le fournisseur garde la place d'une connexion fermee. Les en-tetes sont ceux
// du GET correspondant (longueur, Content-Range pour une plage, 416 hors fichier).
func TestDirectHEADNeDemandeQuUnOctetAuFournisseur(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	tete := func(plage string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest("HEAD", b.http.URL+"/?id=iptva:2001", nil)
		req.Header.Set("X-Session", "s1")
		if plage != "" {
			req.Header.Set("Range", plage)
		}
		rep, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		rep.Body.Close()
		return rep
	}
	rep := tete("")
	if rep.StatusCode != 200 || rep.ContentLength != tailleFilm || rep.Header.Get("Accept-Ranges") != "bytes" || rep.Header.Get("X-Mytv-Mode") != "direct" {
		t.Errorf("HEAD : statut %d, longueur %d, %v", rep.StatusCode, rep.ContentLength, rep.Header)
	}
	if rep = tete("bytes=1000-1999"); rep.StatusCode != 206 || rep.ContentLength != 1000 || rep.Header.Get("Content-Range") != "bytes 1000-1999/300000" {
		t.Errorf("HEAD avec plage : statut %d, longueur %d, Content-Range %q", rep.StatusCode, rep.ContentLength, rep.Header.Get("Content-Range"))
	}
	if rep = tete("bytes=-500"); rep.StatusCode != 206 || rep.ContentLength != 500 || rep.Header.Get("Content-Range") != "bytes 299500-299999/300000" {
		t.Errorf("HEAD suffixe : statut %d, longueur %d, Content-Range %q", rep.StatusCode, rep.ContentLength, rep.Header.Get("Content-Range"))
	}
	if rep = tete(fmt.Sprintf("bytes=%d-", tailleFilm)); rep.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Errorf("HEAD hors fichier : statut %d, attendu 416", rep.StatusCode)
	}
	vus := rangesVus(b.faux)
	if len(vus) != 4 {
		t.Fatalf("%d requetes de film pour 4 HEAD : %v", len(vus), vus)
	}
	for i, r := range vus {
		if r != "bytes=0-0" {
			t.Errorf("HEAD %d : Range amont %q, attendu bytes=0-0 (un seul octet)", i, r)
		}
	}
}

// ---------------------------------------------------------------- CA2/CA3 : remux

func (b *banc) nourrir(donnees string, puisFin bool) func(*procTest) {
	return func(p *procTest) {
		p.pw.Write([]byte(donnees))
		if puisFin {
			p.pw.Close()
			p.mourir(nil)
		}
	}
}

// CA2 : mkv h264+aac -> remux ; la tache decrit CE QUE ffmpeg doit faire, avec
// l'URL construite par le fournisseur, jamais par la requete.
func TestRemuxMKV(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	b.sondeur.sonde = sondeMKV
	b.lanceur.surLancer = b.nourrir("ftypISOM-fragments", true)
	rep, corps, err := b.get("iptva:2002", "s1", "")
	if err != nil || rep.StatusCode != 200 || string(corps) != "ftypISOM-fragments" {
		t.Fatalf("statut %d, %q, %v", rep.StatusCode, corps, err)
	}
	if rep.Header.Get("Content-Type") != "video/mp4" || rep.Header.Get("Accept-Ranges") != "none" {
		t.Errorf("en-tetes %v", rep.Header)
	}
	ps := b.lanceur.liste()
	if len(ps) != 1 {
		t.Fatalf("%d processus", len(ps))
	}
	tache := ps[0].tache
	if tache.Mode != domain.ModeRemux || tache.Source == nil || tache.Debut != 0 {
		t.Errorf("tache = %+v", tache)
	}
	if ps[0].arrets.Load() == 0 {
		t.Error("processus non arrete a la fin")
	}
	if b.faux.Compteurs().Films != 0 {
		t.Error("le relais direct a ete utilise pour un mkv")
	}
}

// CA2 : audio incompatible -> audio converti.
func TestRemuxAudioConverti(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	b.sondeur.sonde = sondeEAC
	b.lanceur.surLancer = b.nourrir("x", true)
	b.get("iptva:2002", "s1", "")
	if ps := b.lanceur.liste(); len(ps) != 1 || ps[0].tache.Mode != domain.ModeAudioConverti {
		t.Errorf("processus = %+v", ps)
	}
}

// Le seek se fait par ?t= ; Range est ignore (200, pas de 206).
func TestRemuxSeekParT(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	b.sondeur.sonde = sondeMKV
	b.lanceur.surLancer = b.nourrir("x", true)
	req, _ := http.NewRequest("GET", b.http.URL+"/?id=iptva:2002&t=5400.5", nil)
	req.Header.Set("X-Session", "s1")
	req.Header.Set("Range", "bytes=1000-")
	rep, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	rep.Body.Close()
	if rep.StatusCode != 200 {
		t.Errorf("statut %d", rep.StatusCode)
	}
	if ps := b.lanceur.liste(); len(ps) != 1 || ps[0].tache.Debut != 5400500*time.Millisecond {
		t.Errorf("tache = %+v", ps[0].tache)
	}
}

// CA5 : t invalide -> 400, rien n'est lance, aucun bail pris.
func TestRemuxRefuseUnTInvalide(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	b.sondeur.sonde = sondeMKV
	for _, v := range []string{"abc", "-5", "NaN", "Inf", "1e99", "999999"} {
		req, _ := http.NewRequest("GET", b.http.URL+"/?id=iptva:2002&t="+v, nil)
		rep, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		rep.Body.Close()
		if rep.StatusCode != 400 {
			t.Errorf("t=%s : statut %d", v, rep.StatusCode)
		}
	}
	if len(b.lanceur.liste()) != 0 || len(b.baux.acquisitions()) != 0 {
		t.Error("effets de bord malgre une demande invalide")
	}
}

// CA2 : HEVC -> 415, aucun processus, bail rendu.
func TestFormatNonLisible(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	b.sondeur.sonde = sondeHEV
	rep, _, _ := b.get("iptva:2002", "s1", "")
	if rep.StatusCode != http.StatusUnsupportedMediaType {
		t.Errorf("statut %d", rep.StatusCode)
	}
	if len(b.lanceur.liste()) != 0 {
		t.Error("ffmpeg lance pour un format refuse")
	}
	attendre(t, func() bool { return b.baux.bail("s1").libere.Load() == 1 }, "bail rendu")
}

// CA3 : saturation de ffmpeg -> 503, le bail est rendu.
func TestRemuxSaturation(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	b.sondeur.sonde = sondeMKV
	b.lanceur.err = remux.ErrSaturation
	rep, _, _ := b.get("iptva:2002", "s1", "")
	if rep.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("statut %d", rep.StatusCode)
	}
	attendre(t, func() bool { return b.baux.bail("s1").libere.Load() == 1 }, "bail rendu")
}

// ffmpeg meurt avant le premier octet : 502 net, pas une reponse vide en 200.
func TestRemuxMortAvantLePremierOctet(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	b.sondeur.sonde = sondeMKV
	b.lanceur.surLancer = func(p *procTest) { p.mourir(errors.New("ffmpeg : exit status 1")) }
	rep, _, _ := b.get("iptva:2002", "s1", "")
	if rep.StatusCode != http.StatusBadGateway {
		t.Errorf("statut %d", rep.StatusCode)
	}
}

// ffmpeg meurt en erreur apres avoir produit : la reponse est signalee en echec ET le
// client la voit tronquee. Regression (recette reelle) : la reponse d'un remux est en
// chunked ; un retour normal du gestionnaire ecrivait le terminateur et le navigateur
// prenait le film coupe pour un film termine (`ended`, pas d'`error`, donc pas de reprise).
// Un vrai client net/http contre un vrai serveur (pas un ResponseRecorder, qui ne voit pas
// la difference) doit obtenir une erreur de lecture, pas un EOF propre.
func TestRemuxMortEnCoursDeRoute(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	b.sondeur.sonde = sondeMKV
	b.lanceur.surLancer = func(p *procTest) { p.pw.Write([]byte("debut")); p.mourir(errors.New("ffmpeg : exit status 1")) }
	rep, corps, err := b.get("iptva:2002", "s1", "")
	if rep.StatusCode != 200 || string(corps) != "debut" {
		t.Errorf("statut %d, corps %q", rep.StatusCode, corps)
	}
	if err == nil {
		t.Error("le client a vu une fin de film propre : la coupure de ffmpeg est invisible")
	} else if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Logf("erreur vue par le client : %v", err)
	}
	b.abandonAvecCause("coupure")
}

// Meme exigence de bout en bout, avec le vrai lanceur et un faux ffmpeg qui recopie la
// source locale : l'amont se coupe apres la derniere reprise permise, le processus sort
// en erreur, et la reponse au client est tronquee (erreur reseau), jamais « terminee ».
func TestRemuxAmontCoupeApresLesReprisesTronqueLaReponse(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl absent")
	}
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	b.sondeur.sonde = sondeMKV
	prog := filepath.Join(t.TempDir(), "faux-ffmpeg")
	// curl sans -f : il recopie ce qu'il recoit et sort en erreur (18) sur un corps tronque.
	script := "#!/bin/sh\nwhile [ \"$1\" != \"-i\" ]; do shift; done\nexec curl -s \"$2\"\n"
	if err := os.WriteFile(prog, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	lanceur, err := remux.NouveauLanceur(remux.Reglages{Ffmpeg: prog, ProcessusMax: 1, Reprises: 1}, remux.ExecOS{})
	if err != nil {
		t.Fatal(err)
	}
	b.svc.dep.Lanceur = lanceur
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleFilm, Coupure: true}) // a chaque ouverture : les reprises s'epuisent

	rep, corps, err := b.get("iptva:2002", "s1", "")
	if rep.StatusCode != 200 || len(corps) == 0 {
		t.Fatalf("statut %d, %d octets : la coupure doit survenir apres le premier octet", rep.StatusCode, len(corps))
	}
	if err == nil {
		t.Error("le client a vu une fin de film propre alors que l'amont s'est coupe apres les reprises")
	}
	b.abandonAvecCause("coupure")
}

// ffmpeg muet : delai du premier octet -> arret du processus et 502.
func TestRemuxPremierOctetTropLent(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, func(r *Reglages) { r.PremierOctet = 50 * time.Millisecond })
	b.sondeur.sonde = sondeMKV // le processus ne produit rien
	rep, _, _ := b.get("iptva:2002", "s1", "")
	if rep.StatusCode != http.StatusBadGateway {
		t.Errorf("statut %d", rep.StatusCode)
	}
	if ps := b.lanceur.liste(); len(ps) != 1 || ps[0].arrets.Load() == 0 {
		t.Error("processus non arrete")
	}
}

// CA3 : le client part -> ffmpeg est arrete et le bail rendu.
func TestRemuxClientPartiArreteFfmpeg(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	b.sondeur.sonde = sondeMKV
	b.lanceur.surLancer = func(p *procTest) {
		for {
			if _, err := p.pw.Write(make([]byte, 8192)); err != nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}
	ctx, annule := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", b.http.URL+"/?id=iptva:2002", nil)
	req.Header.Set("X-Session", "s1")
	rep, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadFull(rep.Body, make([]byte, 30000))
	annule()
	rep.Body.Close()
	attendre(t, func() bool {
		ps := b.lanceur.liste()
		return len(ps) == 1 && ps[0].arrets.Load() > 0 && b.baux.bail("s1").libere.Load() == 1
	}, "ffmpeg arrete et bail rendu")
}

// ---------------------------------------------------------------- CA4 : baux

// CA4 : le bail est pris AVANT toute requete amont, pour la bonne ressource, et
// rendu a la fin (direct comme remux).
func TestBailPrisPuisRendu(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	b.get("iptva:2001", "s1", "")
	acq := b.baux.acquisitions()
	if len(acq) != 1 || acq[0] != (acquisition{"iptva", "s1", "iptva:2001"}) {
		t.Fatalf("acquisitions = %+v", acq)
	}
	attendre(t, func() bool { return b.baux.bail("s1").libere.Load() == 1 }, "bail rendu (direct)")

	b.sondeur.sonde = sondeMKV
	b.lanceur.surLancer = b.nourrir("x", true)
	b.get("iptva:2002", "s2", "")
	attendre(t, func() bool { return b.baux.bail("s2").libere.Load() == 1 }, "bail rendu (remux)")
}

// CA4 : pas de place -> erreur, aucune requete amont, rien a rendre.
func TestPasDePlace(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	b.baux.err = domain.ErrPlaceIndisponible
	rep, _, _ := b.get("iptva:2001", "s1", "")
	if rep.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("statut %d", rep.StatusCode)
	}
	if len(b.sondeur.appels) != 0 || b.faux.Compteurs().Films != 0 {
		t.Error("requete amont sans bail")
	}
}

// CA4 : un client qui part pendant l'attente d'une place n'est pas servi ensuite.
func TestAnnulationPendantLAttenteDuBail(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	b.baux.attente = make(chan struct{})
	ctx, annule := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer annule()
	req, _ := http.NewRequestWithContext(ctx, "GET", b.http.URL+"/?id=iptva:2001", nil)
	req.Header.Set("X-Session", "s1")
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("reponse alors qu'aucune place n'est libre")
	}
	if e := b.derniereErreur(); !errors.Is(e, context.Canceled) {
		t.Errorf("erreur = %v", e)
	}
	if b.faux.Compteurs().Films != 0 {
		t.Error("film demande sans place")
	}
}

// CA4 : un seek (nouvelle requete ?t= de la meme session) remplace le remux en
// cours : l'ancien ffmpeg est mort AVANT que le nouveau ne demarre, et le bail
// n'est rendu qu'a la toute fin.
func TestSeekRemplaceLeRemuxEnCours(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	b.sondeur.sonde = sondeMKV
	b.lanceur.surLancer = func(p *procTest) {
		p.pw.Write([]byte("flux"))
		<-p.termine
	}
	get := func(url string) (*http.Response, error) {
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("X-Session", "s1")
		return http.DefaultClient.Do(req)
	}
	r1, err := get(b.http.URL + "/?id=iptva:2002")
	if err != nil {
		t.Fatal(err)
	}
	defer r1.Body.Close()
	io.ReadFull(r1.Body, make([]byte, 4))

	r2, err := get(b.http.URL + "/?id=iptva:2002&t=600")
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Body.Close()
	io.ReadFull(r2.Body, make([]byte, 4))

	ps := b.lanceur.liste()
	if len(ps) != 2 || ps[0].arrets.Load() == 0 {
		t.Fatalf("%d processus ; le premier doit etre arrete", len(ps))
	}
	if ps[1].arrets.Load() != 0 {
		t.Error("le nouveau processus a ete arrete")
	}
	if n := b.baux.bail("s1").libere.Load(); n != 0 {
		t.Errorf("bail rendu %d fois pendant la lecture", n)
	}
	// L'ancienne reponse (remplacee, pas finie) ne doit pas passer pour un film termine.
	if _, err := io.ReadAll(r1.Body); err == nil {
		t.Error("l'ancienne lecture remuxee parait terminee au lieu d'etre coupee")
	}
	r2.Body.Close()
	attendre(t, func() bool { return ps[1].arrets.Load() > 0 && b.baux.bail("s1").libere.Load() == 1 }, "fin : un seul rendu du bail")
}

// CA4 : zapping — une autre ressource de la meme session remplace la lecture directe
// en cours ; la session garde son bail.
func TestZappingRemplaceLaLectureEnCours(t *testing.T) {
	amontFerme := make(chan struct{})
	b, up := banc2(t, func(w http.ResponseWriter) {
		w.Header().Set("Content-Length", "100000000")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		for {
			if _, err := w.Write(make([]byte, 1<<14)); err != nil {
				close(amontFerme)
				return
			}
			time.Sleep(time.Millisecond)
		}
	})
	defer up.Close()
	req, _ := http.NewRequest("GET", b.http.URL+"/?id=iptva:2001", nil)
	req.Header.Set("X-Session", "s1")
	r1, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer r1.Body.Close()
	io.ReadFull(r1.Body, make([]byte, 20000))

	b.sondeur.sonde = sondeMKV
	b.lanceur.surLancer = b.nourrir("autre", true)
	b.rebrancher(b.faux.URL()) // le second film vient du vrai faux fournisseur
	_, corps, err := b.get("iptva:2002", "s1", "")
	if err != nil || string(corps) != "autre" {
		t.Fatalf("second film : %q, %v", corps, err)
	}
	select {
	case <-amontFerme:
	case <-time.After(5 * time.Second):
		t.Fatal("la premiere lecture n'a pas ete interrompue")
	}
	acq := b.baux.acquisitions()
	if len(acq) != 2 || acq[0].session != acq[1].session {
		t.Errorf("acquisitions = %+v", acq)
	}
	attendre(t, func() bool { return b.baux.bail("s1").libere.Load() == 1 }, "un seul rendu pour deux lectures")
}

// US-24 CA2 (remplace l'ancien « coexistent ») : le fournisseur n'a qu'UNE connexion, deux
// lectures directes de la meme ressource ne peuvent donc pas coexister. La plus recente
// (le seek, ou le navigateur qui lit la fin du mp4 pour l'index) l'emporte, elle est
// servie exactement ; l'ancienne est coupee proprement (corps tronque, pas de blocage).
func TestCA2_LaDerniereLectureDirecteDeLaMemeRessourceRemplaceLAncienne(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{TailleFilm: grosFilm}, avecPatience)
	ancienne := b.ouvrirBloque("bytes=0-", 1024)
	attendu := b.contenuFaux(grosFilm)

	rep, err := b.getPlage("iptva:2001", "s1", "bytes=2000000-")
	if err != nil || rep.StatusCode != 206 {
		t.Fatalf("seek : %v, %v", rep, err)
	}
	defer rep.Body.Close()
	corps, err := io.ReadAll(rep.Body)
	if err != nil || !bytes.Equal(corps, attendu[2000000:]) {
		t.Fatalf("la derniere plage n'est pas exacte (%d octets, %v)", len(corps), err)
	}
	if _, err := io.ReadAll(ancienne.Body); err == nil {
		t.Error("l'ancienne lecture a ete servie jusqu'au bout au lieu d'etre coupee")
	}
	attendre(t, func() bool { return b.baux.bail("s1").libere.Load() >= 1 }, "bail rendu")
}

// Deux sessions differentes ne se genent pas (chacune son bail).
func TestSessionsIndependantes(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	b.get("iptva:2001", "s1", "")
	b.get("iptva:2001", "s2", "")
	attendre(t, func() bool {
		return b.baux.bail("s1").libere.Load() == 1 && b.baux.bail("s2").libere.Load() == 1
	}, "baux rendus")
}

// Le bail est renouvele pendant une longue lecture.
func TestBailRenouvelePendantLaLecture(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, func(r *Reglages) { r.RenouvellementBail = 10 * time.Millisecond })
	b.sondeur.sonde = sondeMKV
	b.lanceur.surLancer = func(p *procTest) { p.pw.Write([]byte("x")); <-p.termine }
	ctx, annule := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", b.http.URL+"/?id=iptva:2002", nil)
	req.Header.Set("X-Session", "s1")
	rep, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	attendre(t, func() bool { return b.baux.bail("s1").renouvele.Load() >= 3 }, "renouvellements")
	annule()
	rep.Body.Close()
	attendre(t, func() bool { return b.baux.bail("s1").libere.Load() == 1 }, "bail rendu")
	n := b.baux.bail("s1").renouvele.Load()
	time.Sleep(50 * time.Millisecond)
	if b.baux.bail("s1").renouvele.Load() != n {
		t.Error("renouvellements apres la fin de la lecture")
	}
}

// ---------------------------------------------------------------- CA5 : entrees

// CA5 : seuls les identifiants du catalogue mènent a une URL amont.
func TestIdentifiantsInconnusOuMalFormes(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	for _, id := range []string{"", "iptva", "iptva:", "autre:2001", "iptva:9999", "iptva:../../etc/passwd",
		"iptva:2001/../x", "iptva:2001?x=http://evil", "http://evil.test/x", "iptva:2001 -i x", ":2001", "iptva:2001\n-f"} {
		rep, _, _ := b.get(id, "s1", "")
		if rep.StatusCode != 404 {
			t.Errorf("id %q : statut %d, attendu 404", id, rep.StatusCode)
		}
	}
	if len(b.sondeur.appels) != 0 || len(b.baux.acquisitions()) != 0 || b.faux.Compteurs().Films != 0 {
		t.Error("une requete amont ou un bail pour un identifiant inconnu")
	}
}

// CA5 : le client ne peut influencer ni l'URL amont (parametres, en-tetes) ni
// l'extension (celle du catalogue est assainie).
func TestLeClientNInfluencePasLURLAmont(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	b.stockage.films["iptva:2003"] = domain.Film{ID: "iptva:2003", FournisseurID: "iptva", Numero: "2003", Extension: "mp4?x=1&y=../../z"}
	b.sondeur.sonde = sondeMKV
	b.lanceur.surLancer = b.nourrir("x", true)
	req, _ := http.NewRequest("GET", b.http.URL+"/?id=iptva:2003&url=http://evil.test/x&host=evil.test", nil)
	req.Header.Set("X-Session", "s1")
	req.Header.Set("Host", "evil.test")
	req.Header.Set("X-Forwarded-Host", "evil.test")
	rep, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	rep.Body.Close()
	ps := b.lanceur.liste()
	if len(ps) != 1 {
		t.Fatalf("%d processus", len(ps))
	}
	// La source lit chez le fournisseur UNIQUEMENT l'adresse construite depuis le catalogue.
	corps, err := ps[0].tache.Source(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, corps)
	corps.Close()
	var vus []string
	for _, tr := range b.faux.Traces() {
		if strings.HasPrefix(tr.Chemin, "/movie/") {
			vus = append(vus, tr.Chemin)
		}
	}
	if len(vus) != 1 || vus[0] != "/movie/***/***/2003.mp4" {
		t.Errorf("adresses demandees au fournisseur : %v", vus)
	}
}

// US-17 CA1 : la source donnee a ffmpeg lit l'amont avec Range (reprise exacte), verifie
// chaque reponse, et ne laisse passer ni un octet de trop ni un corps trop court.
func TestCA1_SourceDuRemuxLitLAmontAvecRange(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	b.sondeur.sonde = sondeMKV
	b.lanceur.surLancer = b.nourrir("x", true)
	b.get("iptva:2002", "s1", "")
	src := b.lanceur.liste()[0].tache.Source
	tout, err := http.Get(b.faux.URL() + "/movie/" + b.faux.Utilisateur() + "/" + b.faux.MotDePasse() + "/2002.mkv")
	if err != nil {
		t.Fatal(err)
	}
	attendu, _ := io.ReadAll(tout.Body)
	tout.Body.Close()
	for _, depuis := range []int64{0, 1, 12_345, int64(len(attendu)) - 1} {
		c, err := src(context.Background(), depuis)
		if err != nil {
			t.Fatalf("depuis %d : %v", depuis, err)
		}
		got, err := io.ReadAll(c)
		c.Close()
		if err != nil || !bytes.Equal(got, attendu[depuis:]) {
			t.Errorf("depuis %d : %d octets, erreur %v", depuis, len(got), err)
		}
	}
	// Au-dela du fichier : refus (416 du fournisseur ou reponse inexploitable), jamais un flux vide.
	if c, err := src(context.Background(), int64(len(attendu))+10); err == nil {
		c.Close()
		t.Error("reprise au-dela du fichier acceptee")
	}
	// Un fournisseur qui ignore Range (200 plein corps) est refuse pour une reprise.
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleFilm, IgnorerRange: true})
	if c, err := src(context.Background(), 5000); err == nil {
		c.Close()
		t.Error("200 a la place de 206 accepte pour une reprise")
	}
	b.faux.Effacer()
	// Un corps coupe en route est signale (le lanceur reprendra), pas pris pour la fin.
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleFilm, Coupure: true, Fois: 1})
	c, err := src(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(c)
	c.Close()
	if err == nil || len(got) >= len(attendu) {
		t.Errorf("coupure non signalee : %d octets, erreur %v", len(got), err)
	}
}

// US-17 CA1 de bout en bout : le vrai Lanceur et un vrai processus (faux ffmpeg qui recopie
// ce qu'il lit a l'adresse locale recue). Le film remuxe arrive octet pour octet ; aucun
// argument du processus ne contient l'hote du fournisseur ni ses identifiants : la seule
// adresse est la source locale a usage unique.
func TestCA1_RemuxDeBoutEnBoutSansURLDansLesArguments(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{Utilisateur: "utilisateur-secret-xyz", MotDePasse: "motdepasse-secret-xyz"}, nil)
	b.sondeur.sonde = sondeMKV
	argsf := filepath.Join(t.TempDir(), "args")
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl absent")
	}
	prog := filepath.Join(t.TempDir(), "faux-ffmpeg")
	// Comme le vrai ffmpeg : il lit l'adresse LOCALE qu'on lui donne apres -i.
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + argsf + "\nwhile [ \"$1\" != \"-i\" ]; do shift; done\nexec curl -s \"$2\"\n"
	if err := os.WriteFile(prog, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	lanceur, err := remux.NouveauLanceur(remux.Reglages{Ffmpeg: prog, ProcessusMax: 1, Reprises: 2}, remux.ExecOS{})
	if err != nil {
		t.Fatal(err)
	}
	b.svc.dep.Lanceur = lanceur

	rep, corps, err := b.get("iptva:2002", "s1", "")
	if err != nil || rep.StatusCode != 200 {
		t.Fatalf("statut %d, %v", rep.StatusCode, err)
	}
	direct, err := http.Get(b.faux.URL() + "/movie/" + b.faux.Utilisateur() + "/" + b.faux.MotDePasse() + "/2002.mkv")
	if err != nil {
		t.Fatal(err)
	}
	attendu, _ := io.ReadAll(direct.Body)
	direct.Body.Close()
	if !bytes.Equal(corps, attendu) {
		t.Fatalf("le film remuxe (%d octets) differe de celui du fournisseur (%d)", len(corps), len(attendu))
	}
	a, err := os.ReadFile(argsf)
	if err != nil {
		t.Fatal(err)
	}
	hote := strings.TrimPrefix(b.faux.URL(), "http://")
	for _, interdit := range []string{hote, b.faux.Utilisateur(), b.faux.MotDePasse(), "@", "movie"} {
		if strings.Contains(string(a), interdit) {
			t.Errorf("la ligne de commande contient %q :\n%s", interdit, a)
		}
	}
	if strings.Contains(string(a), "pipe:0") || !strings.Contains(string(a), "http://127.0.0.1:") {
		t.Errorf("adresse locale attendue (et plus de tube) :\n%s", a)
	}
}

// ---------------------------------------------------------------- CA7 : series

// CA7 : un episode se sert par son identifiant (qui porte l'extension), sous /series/.
func TestEpisodeParSonIdentifiant(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	rep, corps, err := b.get("iptva:501.mp4", "s1", "bytes=10-19")
	if err != nil || rep.StatusCode != 206 || len(corps) != 10 {
		t.Fatalf("episode : statut %d, %d octets, %v", rep.StatusCode, len(corps), err)
	}
	var vu bool
	for _, tr := range b.faux.Traces() {
		vu = vu || tr.Chemin == "/series/***/***/501.mp4"
	}
	if !vu {
		t.Error("l'episode n'a pas ete demande sous /series/")
	}
	// Identifiants d'episode invalides : jamais d'URL.
	for _, id := range []string{"iptva:501.exe", "iptva:501.", "iptva:.mp4", "iptva:5a1.mp4", "iptva:501.mp4.mp4", "iptva:501.mp4?x", "iptva:../1.mp4", "iptva:501.MKV"} {
		if rep, _, _ := b.get(id, "s1", ""); rep.StatusCode != 404 {
			t.Errorf("%q : statut %d", id, rep.StatusCode)
		}
	}
}

// ---------------------------------------------------------------- divers

func TestNouveauValide(t *testing.T) {
	ok := Dependances{Stockage: &stockageFaux{}, Baux: &bauxFaux{}, Sondeur: &sondeurFaux{}, Decideur: remux.Decider, Lanceur: &lanceurFaux{}}
	if _, err := Nouveau(ok, ReglagesParDefaut()); err != nil {
		t.Fatal(err)
	}
	mauvais := ReglagesParDefaut()
	mauvais.Tampon = 0
	if _, err := Nouveau(ok, mauvais); err == nil {
		t.Error("tampon 0 accepte")
	}
	if _, err := Nouveau(Dependances{}, ReglagesParDefaut()); err == nil {
		t.Error("dependances absentes acceptees")
	}
	ok.Fournisseurs = map[string]domain.Fournisseur{"x": fournisseurSansVOD{}}
	if _, err := Nouveau(ok, ReglagesParDefaut()); err == nil {
		t.Error("fournisseur sans VOD accepte")
	}
}

type fournisseurSansVOD struct{ domain.Fournisseur }

func TestStatutHTTP(t *testing.T) {
	cas := map[error]int{
		domain.ErrNonTrouve: 404, domain.ErrIntrouvable: 404, ErrDemandeInvalide: 400, domain.ErrNonLisible: 415,
		domain.ErrPlaceIndisponible: 503, domain.ErrRefuseConnexions: 503, remux.ErrSaturation: 503,
		domain.ErrAmont: 502, domain.ErrRefuseIP: 502, ErrReponseAmont: 502, ErrCoupure: 502,
		context.Canceled: 499, errors.New("autre"): 500,
	}
	for e, want := range cas {
		if got := StatutHTTP(e); got != want {
			t.Errorf("%v : %d, attendu %d", e, got, want)
		}
	}
}

func TestReglagesDepuisConfig(t *testing.T) {
	c := config.Defauts()
	r := DepuisConfig(&c, "ua")
	if r.Tampon != c.Remux.Tampon || r.Reprises != c.Remux.ReprisesMax || r.RenouvellementBail != c.Bail.TTL.Std()/3 || r.UserAgent != "ua" {
		t.Errorf("reglages = %+v", r)
	}
	if r.PatienceMax != c.Bail.PatienceMax.Std() || r.Reessai != c.Bail.Reessai.Std() || r.PatienceMax <= 0 || r.RelaisBrutHEVC {
		t.Errorf("patience %v / reessai %v / brut %v : pas ceux de bail.* et vod.*", r.PatienceMax, r.Reessai, r.RelaisBrutHEVC)
	}
	if r.PremierOctet != 60*time.Second || r.GraceAnnulation != 250*time.Millisecond || r.RetentionAdresse != 10*time.Minute {
		t.Errorf("premier_octet %v / grace_annulation %v / retention_adresse %v : pas ceux de vod.*", r.PremierOctet, r.GraceAnnulation, r.RetentionAdresse)
	}
	ok := Dependances{Stockage: &stockageFaux{}, Baux: &bauxFaux{}, Sondeur: &sondeurFaux{}, Decideur: remux.Decider, Lanceur: &lanceurFaux{}}
	if _, err := Nouveau(ok, r); err != nil {
		t.Errorf("les valeurs par defaut de la config ne conviennent pas : %v", err)
	}
	lr, sr, ex := remux.DepuisConfig(c.Remux)
	if _, err := remux.NouveauLanceur(lr, ex); err != nil || sr.Max < 1 {
		t.Errorf("lanceur %v, sonde %+v", err, sr)
	}
}

// ---------------------------------------------------------------- US-16 CA6 : HEAD, mode et duree

func (b *banc) head(id, session string) (*http.Response, error) {
	req, _ := http.NewRequest("HEAD", b.http.URL+"/?id="+id, nil)
	req.Header.Set("X-Session", session)
	rep, err := http.DefaultClient.Do(req)
	if err == nil {
		rep.Body.Close()
	}
	return rep, err
}

// CA6 : X-Mytv-Mode et X-Mytv-Duree sur HEAD et sur GET, dans les trois modes ;
// aucun ffmpeg n'est lance par un HEAD, aucun octet de film n'est lu.
func TestCA6_ModeEtDureeLisiblesParLeClient(t *testing.T) {
	cas := []struct {
		nom, id string
		sonde   domain.Sonde
		mode    string
		ranges  string
	}{
		{"direct", "iptva:2001", sondeMP4, "direct", "bytes"},
		{"remux", "iptva:2002", sondeMKV, "remux", "none"},
		{"audio", "iptva:2002", sondeEAC, "audio", "none"},
	}
	for _, c := range cas {
		t.Run(c.nom, func(t *testing.T) {
			b := nouveauBanc(t, fauxiptv.Options{}, nil)
			c.sonde.Duree = 5432*time.Second + 600*time.Millisecond
			b.sondeur.sonde = c.sonde
			b.lanceur.surLancer = b.nourrir("x", true)

			rep, err := b.head(c.id, "s1")
			if err != nil || rep.StatusCode != 200 {
				t.Fatalf("HEAD : %v %v", rep, err)
			}
			if rep.Header.Get("X-Mytv-Mode") != c.mode || rep.Header.Get("X-Mytv-Duree") != "5433" || rep.Header.Get("Accept-Ranges") != c.ranges {
				t.Errorf("HEAD en-tetes : %v", rep.Header)
			}
			if n := len(b.lanceur.liste()); n != 0 {
				t.Errorf("HEAD a lance %d processus ffmpeg", n)
			}
			rg, _, err := b.get(c.id, "s1", "")
			if err != nil || rg.StatusCode != 200 || rg.Header.Get("X-Mytv-Mode") != c.mode || rg.Header.Get("X-Mytv-Duree") != "5433" {
				t.Errorf("GET : %d %v", rg.StatusCode, rg.Header)
			}
			if c.mode == "direct" {
				// Un 206 porte aussi le mode (l'interface le relit sur chaque reponse).
				r206, _, _ := b.get(c.id, "s1", "bytes=10-19")
				if r206.StatusCode != 206 || r206.Header.Get("X-Mytv-Mode") != "direct" {
					t.Errorf("206 : %d %v", r206.StatusCode, r206.Header)
				}
			}
		})
	}
}

// CA6 : duree inconnue = pas d'en-tete (jamais « 0 », qui ferait croire a un film vide).
func TestCA6_DureeInconnueAbsente(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	rep, err := b.head("iptva:2001", "s1")
	if err != nil || rep.Header.Get("X-Mytv-Mode") != "direct" {
		t.Fatalf("HEAD : %v %v", rep, err)
	}
	if v, ok := rep.Header["X-Mytv-Duree"]; ok {
		t.Errorf("X-Mytv-Duree = %v alors que la duree est inconnue", v)
	}
}

// CA6 : HEAD rend le MEME statut que GET pour chaque refus (415, 404, 503), sans
// poser d'en-tetes de mode sur une erreur.
func TestCA6_HEADMemeStatutQueGET(t *testing.T) {
	type cas struct {
		nom    string
		id     string
		regle  func(b *banc)
		statut int
	}
	for _, c := range []cas{
		{"hevc", "iptva:2002", func(b *banc) { b.sondeur.sonde = sondeHEV }, 415},
		{"inconnu", "iptva:9999", func(*banc) {}, 404},
		{"mal forme", "iptva:abc", func(*banc) {}, 404},
		{"place occupee", "iptva:2001", func(b *banc) { b.baux.err = domain.ErrPlaceIndisponible }, 503},
		{"amont en panne", "iptva:2001", func(b *banc) { b.sondeur.err = domain.ErrAmont }, 502},
	} {
		t.Run(c.nom, func(t *testing.T) {
			b := nouveauBanc(t, fauxiptv.Options{}, nil)
			c.regle(b)
			g, _, _ := b.get(c.id, "s1", "")
			h, err := b.head(c.id, "s1")
			if err != nil {
				t.Fatal(err)
			}
			if g.StatusCode != c.statut || h.StatusCode != c.statut {
				t.Errorf("GET %d, HEAD %d, attendu %d", g.StatusCode, h.StatusCode, c.statut)
			}
			if h.Header.Get("X-Mytv-Mode") != "" {
				t.Errorf("mode annonce sur une erreur : %v", h.Header)
			}
		})
	}
}

// CA6 : un HEAD ne garde pas la place : le bail est rendu des qu'il a repondu.
func TestCA6_HEADRendLaPlace(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	if _, err := b.head("iptva:2001", "s1"); err != nil {
		t.Fatal(err)
	}
	attendre(t, func() bool { return b.baux.bail("s1").libere.Load() == 1 }, "bail rendu apres le HEAD")
}

// ---------------------------------------------------------------- US-17 CA3 : pause

// pauseTest : un client qui lit `lu` octets puis s'arrete de lire (lecteur en pause).
func (b *banc) clientEnPause(id, session, plage string, lu int) (*http.Response, io.ReadCloser) {
	b.t.Helper()
	req, _ := http.NewRequest("GET", b.http.URL+"/?id="+id, nil)
	req.Header.Set("X-Session", session)
	if plage != "" {
		req.Header.Set("Range", plage)
	}
	rep, err := http.DefaultClient.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	if _, err := io.ReadFull(rep.Body, make([]byte, lu)); err != nil {
		b.t.Fatal(err)
	}
	return rep, rep.Body
}

// CA3 : un film (relais direct) dont le client ne lit plus depuis vod.pause_max rend sa place
// et ferme sa connexion amont ; la reprise (Range) rouvre une lecture normale.
func TestCA3_FilmEnPauseRendSaPlace(t *testing.T) {
	const pause = 300 * time.Millisecond
	b := nouveauBanc(t, fauxiptv.Options{TailleFilm: 24 << 20}, func(r *Reglages) { r.PauseMax = pause })
	debut := time.Now()
	rep, corps := b.clientEnPause("iptva:2001", "s1", "", 1024)
	defer rep.Body.Close()
	// Le client est « en pause » : plus aucune lecture. La place est rendue apres pause_max.
	attendre(t, func() bool { return b.baux.bail("s1").libere.Load() == 1 }, "place rendue apres la pause")
	if d := time.Since(debut); d < pause {
		t.Errorf("place rendue apres %s, avant vod.pause_max (%s)", d, pause)
	}
	if b.instr.valeur("vod_pauses_total") != 1 {
		t.Errorf("vod_pauses_total = %v", b.instr.valeur("vod_pauses_total"))
	}
	if b.instr.valeur("vod_erreurs_total") != 0 {
		t.Error("une pause n'est pas une erreur")
	}
	// La connexion du client est fermee (reponse tronquee) ; la lecture reprend avec Range.
	if _, err := io.Copy(io.Discard, corps); err == nil {
		t.Error("le client aurait du voir une reponse tronquee")
	}
	rg, suite, err := b.get("iptva:2001", "s1", "bytes=1024-2047")
	if err != nil || rg.StatusCode != 206 || len(suite) != 1024 {
		t.Fatalf("reprise : %d, %d octets, %v", rg.StatusCode, len(suite), err)
	}
	if n := len(b.baux.acquisitions()); n != 2 {
		t.Errorf("%d acquisitions de bail (une par lecture)", n)
	}
}

// CA3 : un client lent mais ACTIF n'est jamais coupe : le delai ne court que pendant
// qu'une ecriture est bloquee.
func TestCA3_ClientLentMaisActifNEstPasCoupe(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{TailleFilm: 4 << 20}, func(r *Reglages) { r.PauseMax = 400 * time.Millisecond })
	rep, corps := b.clientEnPause("iptva:2001", "s1", "", 1)
	defer rep.Body.Close()
	total := 1
	buf := make([]byte, 256<<10)
	for {
		time.Sleep(150 * time.Millisecond) // plus lent que le fournisseur, plus rapide que pause_max
		n, err := corps.Read(buf)
		total += n
		if err != nil {
			if err != io.EOF {
				t.Fatalf("lecture coupee apres %d octets : %v", total, err)
			}
			break
		}
	}
	if total != 4<<20 {
		t.Errorf("%d octets lus sur %d", total, 4<<20)
	}
	if b.instr.valeur("vod_pauses_total") != 0 {
		t.Error("un client actif a ete pris pour une pause")
	}
}

// CA3 : pause_max = 0 desactive la coupure.
func TestCA3_PauseMaxNulleNeCoupeJamais(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{TailleFilm: 24 << 20}, func(r *Reglages) { r.PauseMax = 0 })
	rep, _ := b.clientEnPause("iptva:2001", "s1", "", 1024)
	defer rep.Body.Close()
	time.Sleep(700 * time.Millisecond)
	if b.baux.bail("s1").libere.Load() != 0 {
		t.Error("la place a ete rendue alors que pause_max vaut 0")
	}
}

// CA3 : meme regle pour le remux : ffmpeg est arrete (il tenait la connexion amont) et la
// place est rendue.
func TestCA3_RemuxEnPauseArreteFfmpegEtRendLaPlace(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, func(r *Reglages) { r.PauseMax = 300 * time.Millisecond })
	b.sondeur.sonde = sondeMKV
	b.lanceur.surLancer = func(p *procTest) {
		// ffmpeg de comedie : produit sans fin tant qu'on lui lit la sortie.
		blocs := bytes.Repeat([]byte("m"), 64<<10)
		for {
			if _, err := p.pw.Write(blocs); err != nil {
				return
			}
		}
	}
	rep, corps := b.clientEnPause("iptva:2002", "s1", "", 1024)
	defer rep.Body.Close()
	attendre(t, func() bool { return b.baux.bail("s1").libere.Load() == 1 }, "place rendue")
	ps := b.lanceur.liste()
	if len(ps) != 1 {
		t.Fatalf("%d processus", len(ps))
	}
	attendre(t, func() bool { return ps[0].arrets.Load() > 0 }, "ffmpeg arrete")
	if b.instr.valeur("vod_pauses_total") != 1 {
		t.Errorf("vod_pauses_total = %v", b.instr.valeur("vod_pauses_total"))
	}
	// Comme en direct : le client voit une reponse tronquee (erreur reseau), jamais une fin
	// de film, meme si le remux est en chunked.
	if _, err := io.Copy(io.Discard, corps); err == nil {
		t.Error("le client aurait du voir une reponse tronquee, pas une fin de film")
	}
}

func TestCA3_ReglagePauseMaxValide(t *testing.T) {
	ok := Dependances{Stockage: &stockageFaux{}, Baux: &bauxFaux{}, Sondeur: &sondeurFaux{}, Decideur: remux.Decider, Lanceur: &lanceurFaux{}}
	r := ReglagesParDefaut()
	r.PauseMax = -time.Second
	if _, err := Nouveau(ok, r); err == nil {
		t.Error("pause_max negatif accepte")
	}
	c := config.Defauts()
	if got := DepuisConfig(&c, "").PauseMax; got != 5*time.Minute {
		t.Errorf("pause_max par defaut = %s", got)
	}
}

// CA3 : un client qui PART (onglet ferme) pendant qu'il est bloque n'est pas une pause :
// la place est rendue tout de suite, sans attendre pause_max, et rien n'est compte comme pause.
func TestCA3_ClientPartiPendantUneEcritureBloqueeNEstPasUnePause(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{TailleFilm: 24 << 20}, func(r *Reglages) { r.PauseMax = 30 * time.Second })
	rep, _ := b.clientEnPause("iptva:2001", "s1", "", 1024)
	time.Sleep(100 * time.Millisecond)
	debut := time.Now()
	rep.Body.Close() // le client s'en va
	http.DefaultClient.CloseIdleConnections()
	attendre(t, func() bool { return b.baux.bail("s1").libere.Load() == 1 }, "place rendue au depart du client")
	if d := time.Since(debut); d > 5*time.Second {
		t.Errorf("place rendue apres %s (pause_max est de 30 s : le depart doit suffire)", d)
	}
	if b.instr.valeur("vod_pauses_total") != 0 {
		t.Error("un depart de client compte comme une pause")
	}
}

// ---------------------------------------------------------------- US-17 CA4 : 416

// CA4 : une plage hors du fichier donne 416 (erreur du client), pas 502 (panne amont), sans
// compter d'erreur, sans octet de film, et la place est rendue.
func TestCA4_PlageHorsFichierDonne416(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	for _, plage := range []string{
		fmt.Sprintf("bytes=%d-", tailleFilm), // premier octet apres la fin
		fmt.Sprintf("bytes=%d-%d", tailleFilm+10, tailleFilm+20),
		"bytes=99999999999-",
	} {
		rep, corps, err := b.get("iptva:2001", "s1", plage)
		if err != nil || rep.StatusCode != http.StatusRequestedRangeNotSatisfiable {
			t.Errorf("%s : statut %d (%v), attendu 416", plage, rep.StatusCode, err)
		}
		if strings.Contains(string(corps), "film") && len(corps) > 100 {
			t.Errorf("%s : corps inattendu (%d octets)", plage, len(corps))
		}
	}
	if b.instr.valeur("vod_erreurs_total") != 0 {
		t.Error("un 416 est une erreur du client, pas une erreur du service")
	}
	// La derniere plage valide passe encore (206), et la place est bien rendue entre-temps.
	rep, corps, err := b.get("iptva:2001", "s1", fmt.Sprintf("bytes=%d-", tailleFilm-5))
	if err != nil || rep.StatusCode != 206 || len(corps) != 5 {
		t.Errorf("dernieres octets : %d, %d octets, %v", rep.StatusCode, len(corps), err)
	}
	attendre(t, func() bool { return b.baux.bail("s1").libere.Load() == 4 }, "une place rendue par requete")
}

func TestStatutHTTPPlageHorsFichier(t *testing.T) {
	if StatutHTTP(fmt.Errorf("x: %w", domain.ErrPlageHorsFichier{Total: 5})) != 416 {
		t.Error("ErrPlageHorsFichier -> 416")
	}
}

// Regression (recette reelle, 2026-10-18) : ffmpeg ouvre 6 a 8 fois le film pour sauter
// (index, position...). Le fournisseur repond chaque fois 302 vers un noeud : refaire ce saut
// a chaque ouverture coute un aller-retour de plus (~0,35 s a ~300 ms de latence) et portait la
// reprise a plus de 9 s. La source retient l'adresse FINALE et ne repasse par la redirection
// qu'une seule fois par lecture.
func TestSourceDuRemuxRetientLAdresseApresRedirection(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{RedirigerFilms: true}, nil)
	b.sondeur.sonde = sondeMKV
	b.lanceur.surLancer = b.nourrir("x", true)
	b.get("iptva:2002", "s1", "")
	src := b.lanceur.liste()[0].tache.Source
	avant := b.faux.Compteurs().RedirectionsFilms
	for _, depuis := range []int64{0, 5_000, 123, 40_000, 10} {
		c, err := src(context.Background(), depuis)
		if err != nil {
			t.Fatalf("depuis %d : %v", depuis, err)
		}
		io.Copy(io.Discard, c)
		c.Close()
	}
	if sauts := b.faux.Compteurs().RedirectionsFilms - avant; sauts != 1 {
		t.Errorf("%d redirections pour 5 ouvertures : l'adresse finale doit etre retenue apres la premiere", sauts)
	}
}

// Le noeud retenu apres la redirection meurt (407 ou 5xx) : la source replie TOUT DE SUITE
// sur l'adresse canonique (nouvelle redirection), sans consommer la patience du 407 ni les
// nouveaux essais. Avant : 40 s de patience sur une adresse morte, puis un 503 « occupe »
// trompeur (407), ou 1,5 s d'essais inutiles (5xx).
func TestSourceDuRemuxReplieSurLAdresseCanoniqueQuandLeNoeudRetenuMeurt(t *testing.T) {
	for _, statut := range []int{407, 502} {
		t.Run(fmt.Sprint(statut), func(t *testing.T) {
			b := nouveauBanc(t, fauxiptv.Options{RedirigerFilms: true}, avecPatience)
			b.sondeur.sonde = sondeMKV
			b.lanceur.surLancer = b.nourrir("x", true)
			b.get("iptva:2002", "s1", "")
			src := b.lanceur.liste()[0].tache.Source
			c, err := src(context.Background(), 0) // apprend l'adresse du noeud
			if err != nil {
				t.Fatal(err)
			}
			attendu, _ := io.ReadAll(c)
			c.Close()
			b.faux.TuerNoeud(statut)
			avant := b.faux.Compteurs()

			debut := time.Now()
			c, err = src(context.Background(), 5000)
			if err != nil {
				t.Fatalf("ouverture apres la mort du noeud : %v", err)
			}
			got, _ := io.ReadAll(c)
			c.Close()
			if d := time.Since(debut); d > time.Second {
				t.Errorf("repli en %v : il doit etre immediat (ni patience ni nouveaux essais sur le noeud mort)", d)
			}
			if !bytes.Equal(got, attendu[5000:]) {
				t.Errorf("octets du nouveau noeud : %d, attendu %d", len(got), len(attendu)-5000)
			}
			apres := b.faux.Compteurs()
			if apres.RedirectionsFilms-avant.RedirectionsFilms != 1 {
				t.Errorf("%d redirections apres la mort du noeud, attendu 1 (repli canonique)", apres.RedirectionsFilms-avant.RedirectionsFilms)
			}
			if statut == 407 && apres.Refus407-avant.Refus407 != 1 {
				t.Errorf("%d refus 407 : un seul essai sur le noeud mort attendu", apres.Refus407-avant.Refus407)
			}
			// Et le nouveau noeud est retenu a son tour.
			if c, err = src(context.Background(), 10); err == nil {
				io.Copy(io.Discard, c)
				c.Close()
			}
			if n := b.faux.Compteurs().RedirectionsFilms - apres.RedirectionsFilms; n != 0 {
				t.Errorf("%d redirection pour l'ouverture suivante : la nouvelle adresse doit etre retenue", n)
			}
		})
	}
}

// L'adresse retenue est oubliee apres vod.retention_adresse : l'ouverture suivante repasse
// par la redirection du fournisseur.
func TestSourceDuRemuxOublieLAdresseRetenueApresLaRetention(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{RedirigerFilms: true}, func(r *Reglages) { r.RetentionAdresse = 50 * time.Millisecond })
	b.sondeur.sonde = sondeMKV
	b.lanceur.surLancer = b.nourrir("x", true)
	b.get("iptva:2002", "s1", "")
	src := b.lanceur.liste()[0].tache.Source
	ouvrir := func() {
		t.Helper()
		c, err := src(context.Background(), 100)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, c)
		c.Close()
	}
	avant := b.faux.Compteurs().RedirectionsFilms
	ouvrir()
	ouvrir() // dans la retention : adresse retenue
	if n := b.faux.Compteurs().RedirectionsFilms - avant; n != 1 {
		t.Fatalf("%d redirections pour 2 ouvertures rapprochees, attendu 1", n)
	}
	time.Sleep(80 * time.Millisecond)
	ouvrir() // retention ecoulee : nouvelle redirection
	if n := b.faux.Compteurs().RedirectionsFilms - avant; n != 2 {
		t.Errorf("%d redirections, attendu 2 : l'adresse retenue doit etre oubliee apres la retention", n)
	}
}
