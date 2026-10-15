package vod

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	if e := b.derniereErreur(); !errors.Is(e, ErrCoupure) {
		t.Errorf("erreur = %v", e)
	}
}

// Les reprises sont comptees : au-dela de remux.reprises_max, on abandonne.
func TestDirectAbandonneApresLesReprises(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, func(r *Reglages) { r.Reprises = 2 })
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleFilm, Coupure: true}) // sans limite
	_, corps, err := b.get("iptva:2001", "s1", "")
	if err == nil || len(corps) >= tailleFilm {
		t.Errorf("%d octets, err=%v : le film ne doit pas etre complet", len(corps), err)
	}
	if e := b.derniereErreur(); !errors.Is(e, ErrCoupure) {
		t.Errorf("erreur = %v", e)
	}
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
	if tache.Mode != domain.ModeRemux || tache.URLAmont != b.client.URLFilm("2002", "mkv") || tache.Debut != 0 {
		t.Errorf("tache = %+v", tache)
	}
	if tache.Entetes.Get("User-Agent") != "Mozilla/5.0 test" {
		t.Errorf("entetes = %v", tache.Entetes)
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

// ffmpeg meurt en erreur apres avoir produit : la reponse est signalee en echec.
func TestRemuxMortEnCoursDeRoute(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	b.sondeur.sonde = sondeMKV
	b.lanceur.surLancer = func(p *procTest) { p.pw.Write([]byte("debut")); p.mourir(errors.New("ffmpeg : exit status 1")) }
	_, corps, _ := b.get("iptva:2002", "s1", "")
	if string(corps) != "debut" {
		t.Errorf("corps %q", corps)
	}
	if e := b.derniereErreur(); !errors.Is(e, ErrCoupure) {
		t.Errorf("erreur = %v", e)
	}
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

// Deux lectures directes de la meme ressource (le navigateur lit la fin du mp4 pour
// trouver l'index pendant qu'il lit le debut) coexistent : aucune n'annule l'autre.
func TestDeuxPlagesDeLaMemeRessourceCoexistent(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	tout := b.contenuAttendu()
	fini := make(chan struct{}, 2)
	var echecs int32
	for _, plage := range []string{"bytes=0-", "bytes=250000-"} {
		go func(p string) {
			defer func() { fini <- struct{}{} }()
			rep, corps, err := b.get("iptva:2001", "s1", p)
			d := 0
			if p != "bytes=0-" {
				d = 250000
			}
			if err != nil || rep.StatusCode != 206 || !bytes.Equal(corps, tout[d:]) {
				echecs++
			}
		}(plage)
	}
	<-fini
	<-fini
	if echecs != 0 {
		t.Errorf("%d lectures en echec", echecs)
	}
	attendre(t, func() bool { return b.baux.bail("s1").libere.Load() == 1 }, "bail rendu une fois")
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
	u := ps[0].tache.URLAmont
	if u != b.client.URLFilm("2003", "mp4") || strings.Contains(u, "evil") || strings.Contains(u, "..") || strings.Contains(u, "?") {
		t.Errorf("URL amont = %q", u)
	}
}

// ---------------------------------------------------------------- CA7 : series

// CA7 : un episode n'est servi qu'apres avoir ete liste (son extension vient de la).
func TestEpisodesApresSaisons(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{Catalogue: map[string]any{"get_series_info": map[string]any{
		"episodes": map[string]any{"1": []map[string]any{{"id": "501", "episode_num": 1, "title": "E1", "container_extension": "mp4"}}},
	}}}, nil)
	if rep, _, _ := b.get("iptva:501", "s1", ""); rep.StatusCode != 404 {
		t.Fatalf("episode jamais liste : statut %d", rep.StatusCode)
	}
	ss, err := b.svc.Saisons(context.Background(), "iptva:3001")
	if err != nil || len(ss) != 1 || ss[0].Episodes[0].ID != "iptva:501" {
		t.Fatalf("saisons = %+v, %v", ss, err)
	}
	rep, corps, err := b.get("iptva:501", "s1", "bytes=10-19")
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
	if _, err := b.svc.Saisons(context.Background(), "iptva:9"); !errors.Is(err, domain.ErrNonTrouve) {
		t.Errorf("serie inconnue : %v", err)
	}
	if _, err := b.svc.Saisons(context.Background(), "x:3001"); !errors.Is(err, domain.ErrNonTrouve) {
		t.Errorf("fournisseur inconnu : %v", err)
	}
}

func TestIndexEpisodesBorne(t *testing.T) {
	x := nouvelIndex(3)
	for i := 0; i < 10; i++ {
		x.mettre(domain.ContenuID("p:"+intStr(i)), "mp4")
	}
	if len(x.ext) != 3 || len(x.ordre) != 3 {
		t.Errorf("index non borne : %d", len(x.ext))
	}
	if _, ok := x.get("p:9"); !ok {
		t.Error("le plus recent a ete evince")
	}
	if _, ok := x.get("p:0"); ok {
		t.Error("le plus ancien est reste")
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
		domain.ErrNonTrouve: 404, domain.ErrIntrouvable: 404, ErrDemandeInvalide: 400, ErrNonLisible: 415,
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
