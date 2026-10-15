package vod

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"testing"
	"time"

	"mytv/internal/domain"
	"mytv/internal/testutil/fauxiptv"
)

// US-24 : patience sur 407 et seek en lecture directe.
//
// Le faux fournisseur est en mode « une seule connexion » : il refuse en 407 toute
// demande de film tant qu'une autre lecture de film est ouverte, comme le vrai
// fournisseur qui garde la place d'un client parti.

const (
	patienceTest = 3 * time.Second
	reessaiTest  = 20 * time.Millisecond
	// gros film : assez pour que le serveur reste bloque en ecriture (contre-pression)
	// quand personne ne lit, donc que sa lecture reste « ouverte » chez le faux.
	grosFilm = 32 << 20
)

func avecPatience(r *Reglages) {
	r.PatienceMax = patienceTest
	r.Reessai = reessaiTest
}

func (b *banc) getPlage(id, session, plage string) (*http.Response, error) {
	b.t.Helper()
	req, _ := http.NewRequest("GET", b.http.URL+"/?id="+id, nil)
	req.Header.Set("X-Session", session)
	if plage != "" {
		req.Header.Set("Range", plage)
	}
	return http.DefaultClient.Do(req)
}

// CA1 / CA3 : le fournisseur repond 407 pendant un moment (place d'un ancien client
// encore gardee), puis accepte : le premier acces patiente au lieu de repondre 503.
func TestCA1_PremierAccesPatienteSurLes407PuisReussit(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, avecPatience)
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleFilm, Statut: 407, Duree: 300 * time.Millisecond})
	debut := time.Now()
	rep, corps, err := b.get("iptva:2001", "s1", "")
	if err != nil || rep.StatusCode != 200 {
		t.Fatalf("statut %d, %v (la patience sur 407 manque)", rep.StatusCode, err)
	}
	if !bytes.Equal(corps, b.contenuAttendu()) {
		t.Error("film corrompu apres la patience")
	}
	if d := time.Since(debut); d < 250*time.Millisecond {
		t.Errorf("reponse en %v : n'a pas attendu la fin des 407", d)
	}
	if b.faux.Compteurs().Refus407 < 2 {
		t.Errorf("%d refus 407 seulement : le test ne prouve rien", b.faux.Compteurs().Refus407)
	}
}

// CA1 : patience epuisee -> ErrRefuseConnexions, donc 503 (l'API ajoute Retry-After).
func TestCA1_PatienceEpuiseeDonne503(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, func(r *Reglages) {
		r.PatienceMax = 150 * time.Millisecond
		r.Reessai = reessaiTest
	})
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleFilm, Statut: 407, Duree: 5 * time.Second})
	rep, _, err := b.get("iptva:2001", "s1", "")
	if err != nil || rep.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("statut %d, %v ; attendu 503", rep.StatusCode, err)
	}
	if e := b.derniereErreur(); !errors.Is(e, domain.ErrRefuseConnexions) {
		t.Errorf("erreur = %v ; attendu ErrRefuseConnexions", e)
	}
}

// CA1 : sans PatienceMax (0), pas de patience : le comportement historique est conserve.
func TestCA1_SansPatienceConfigureeUn407DonneLe503Immediat(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleFilm, Statut: 407, Duree: time.Second})
	rep, _, _ := b.get("iptva:2001", "s1", "")
	if rep.StatusCode != http.StatusServiceUnavailable || b.faux.Compteurs().Refus407 != 1 {
		t.Errorf("statut %d, %d refus", rep.StatusCode, b.faux.Compteurs().Refus407)
	}
}

// CA1 / CA3 : coupure en cours de film PUIS 407 a la reprise. La patience de la reprise
// ne consomme pas d'essai de reprise : avec UNE seule reprise autorisee, le film arrive
// quand meme complet.
func TestCA3_CoupurePuis407ALaRepriseNeConsommePasLEssai(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, func(r *Reglages) { avecPatience(r); r.Reprises = 1 })
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleFilm, Coupure: true, Fois: 1})
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleFilm, Statut: 407, Fois: 5})
	rep, corps, err := b.get("iptva:2001", "s1", "")
	if err != nil || rep.StatusCode != 200 {
		t.Fatalf("statut %d, %v", rep.StatusCode, err)
	}
	if !bytes.Equal(corps, b.contenuAttendu()) {
		t.Fatalf("film corrompu (%d octets) : la reprise n'a pas patiente", len(corps))
	}
	if n := b.faux.Compteurs().Refus407; n != 5 {
		t.Errorf("%d refus 407 ; attendu 5", n)
	}
	if b.instr.valeur("vod_reprises_total") != 1 {
		t.Error("une seule reprise attendue")
	}
}

// CA1 : le client part pendant la patience -> l'attente s'arrete (pas de goroutine ni de
// requete qui continue : sansFuite verifie en fin de test).
func TestCA3_ClientPartiPendantLaPatienceArreteLAttente(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, avecPatience)
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleFilm, Statut: 407, Duree: 10 * time.Second})
	ctx, annule := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", b.http.URL+"/?id=iptva:2001", nil)
	req.Header.Set("X-Session", "s1")
	go func() {
		// Pas de t.Fatal ici : cette goroutine peut survivre au test si la patience manque.
		for fin := time.Now().Add(5 * time.Second); b.faux.Compteurs().Refus407 < 2 && time.Now().Before(fin); {
			time.Sleep(2 * time.Millisecond)
		}
		annule()
	}()
	if _, err := http.DefaultClient.Do(req); err == nil {
		t.Fatal("la requete aurait du etre annulee")
	}
	vu := b.faux.Compteurs().Refus407
	time.Sleep(10 * reessaiTest)
	if apres := b.faux.Compteurs().Refus407; apres > vu+1 {
		t.Errorf("le serveur continue de retenter apres le depart du client (%d -> %d)", vu, apres)
	}
}

// ouvrirBloque lit `lu` octets puis laisse le corps ouvert : le serveur se bloque en
// ecriture (contre-pression), sa lecture reste en cours chez le faux fournisseur.
func (b *banc) ouvrirBloque(plage string, lu int) *http.Response {
	b.t.Helper()
	rep, err := b.getPlage("iptva:2001", "s1", plage)
	if err != nil {
		b.t.Fatal(err)
	}
	b.t.Cleanup(func() { rep.Body.Close() })
	if rep.StatusCode != 200 && rep.StatusCode != 206 {
		b.t.Fatalf("lecture %q : statut %d", plage, rep.StatusCode)
	}
	if _, err := io.ReadFull(rep.Body, make([]byte, lu)); err != nil {
		b.t.Fatal(err)
	}
	return rep
}

// CA2 / CA3 : un seek (nouvelle plage) alors que l'ancienne lecture directe est bloquee
// par la contre-pression. Le fournisseur n'a qu'une connexion : sans annulation de
// l'ancienne lecture il repond 407 et le lecteur reste fige.
func TestCA2_SeekCoupeLaLectureDirecteBloqueeAvantDOuvrirLAmont(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{TailleFilm: grosFilm}, avecPatience)
	b.faux.UneSeuleConnexion(100 * time.Second)
	b.ouvrirBloque("bytes=0-", 1024)
	attendre(t, func() bool { return b.faux.FilmsEnCours() == 1 }, "ancienne lecture ouverte chez le fournisseur")
	time.Sleep(100 * time.Millisecond) // la contre-pression s'etablit : elle ne finit pas toute seule
	if b.faux.FilmsEnCours() != 1 {
		t.Fatal("l'ancienne lecture a fini seule : le test ne prouve rien")
	}

	rep := b.ouvrirBloque("bytes=2000000-", 4096)
	if rep.StatusCode != http.StatusPartialContent {
		t.Fatalf("statut %d apres le seek", rep.StatusCode)
	}
	if n := b.faux.FilmsEnCours(); n != 1 {
		t.Errorf("%d lectures ouvertes chez le fournisseur ; une seule connexion attendue", n)
	}
}

// CA2 : seek x10 de suite, chaque ancienne lecture restant bloquee : aucun 503 residuel,
// le contenu de la derniere plage est exact, une seule connexion amont a la fois.
func TestCA2_DixSeeksDeSuiteSansAucun503(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{TailleFilm: grosFilm}, avecPatience)
	b.faux.UneSeuleConnexion(100 * time.Second)
	b.ouvrirBloque("bytes=0-", 1024)
	attendre(t, func() bool { return b.faux.FilmsEnCours() == 1 }, "premiere lecture")
	attendu := b.contenuFaux(grosFilm)
	for i := 1; i <= 10; i++ {
		debut := i * 1_000_000
		rep, err := b.getPlage("iptva:2001", "s1", "bytes="+strconv.Itoa(debut)+"-")
		if err != nil || rep.StatusCode != http.StatusPartialContent {
			t.Fatalf("seek %d : statut %v, %v", i, rep, err)
		}
		tete := make([]byte, 2048)
		if _, err := io.ReadFull(rep.Body, tete); err != nil {
			t.Fatalf("seek %d : %v", i, err)
		}
		if !bytes.Equal(tete, attendu[debut:debut+2048]) {
			t.Fatalf("seek %d : octets du mauvais endroit", i)
		}
		t.Cleanup(func() { rep.Body.Close() })
		if n := b.faux.FilmsEnCours(); n > 1 {
			t.Fatalf("seek %d : %d connexions amont simultanees", i, n)
		}
	}
	if n := b.faux.Compteurs().Refus407; n != 0 {
		t.Logf("%d refus 407 absorbes par la patience", n)
	}
}

// contenuFaux : les octets que le faux sert pour 2001.mp4 (relus une fois).
func (b *banc) contenuFaux(taille int) []byte {
	b.t.Helper()
	b.faux.UneSeuleConnexion(0)
	defer b.faux.UneSeuleConnexion(100 * time.Second)
	rep, err := http.Get(b.faux.URL() + "/movie/" + b.faux.Utilisateur() + "/" + b.faux.MotDePasse() + "/2001.mp4")
	if err != nil {
		b.t.Fatal(err)
	}
	defer rep.Body.Close()
	c, _ := io.ReadAll(rep.Body)
	if len(c) != taille {
		b.t.Fatalf("%d octets, attendu %d", len(c), taille)
	}
	return c
}

// CA3 : fin de lecture rend la place : une lecture complete laisse le fournisseur libre,
// le titre suivant passe sans patience.
func TestCA3_FinDeLectureRendLaPlace(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, avecPatience)
	b.faux.UneSeuleConnexion(100 * time.Second)
	if rep, _, err := b.get("iptva:2001", "s1", ""); err != nil || rep.StatusCode != 200 {
		t.Fatalf("premiere lecture : %v", err)
	}
	attendre(t, func() bool { return b.faux.FilmsEnCours() == 0 }, "place rendue chez le fournisseur")
	attendre(t, func() bool { return b.baux.bail("s1").libere.Load() == 1 }, "bail rendu")
	if n := b.faux.Compteurs().Refus407; n != 0 {
		t.Errorf("%d refus 407 : la place n'etait pas rendue", n)
	}
}

// CA2 : un HEAD ne coupe pas une lecture en cours de la meme ressource (il n'a pas de
// corps a servir).
func TestCA2_UnHEADNeCoupePasLaLectureEnCours(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{TailleFilm: grosFilm}, func(r *Reglages) {
		r.PatienceMax = 300 * time.Millisecond
		r.Reessai = reessaiTest
	})
	b.faux.UneSeuleConnexion(100 * time.Second)
	rep := b.ouvrirBloque("", 1024)
	_, _ = b.head("iptva:2001", "s1") // 503 apres sa patience : l'important est le reste
	if _, err := io.ReadFull(rep.Body, make([]byte, 4096)); err != nil {
		t.Fatalf("la lecture en cours a ete coupee par le HEAD : %v", err)
	}
}

// ---------------------------------------------------------------- US-24 CA4 : HEVC

func (b *banc) getCast(id, session string, entete string) (*http.Response, []byte) {
	b.t.Helper()
	req, _ := http.NewRequest("GET", b.http.URL+"/?id="+id, nil)
	req.Header.Set("X-Session", session)
	if entete != "" {
		req.Header.Set("X-Mytv-Client", entete)
	}
	rep, err := http.DefaultClient.Do(req)
	if err != nil {
		b.t.Fatal(err)
	}
	defer rep.Body.Close()
	corps, _ := io.ReadAll(rep.Body)
	return rep, corps
}

// CA4 : par defaut (option fausse), un HEVC est refuse en 415 meme si le client dit « cast » ;
// l'erreur porte le code HEVC.
func TestCA4_HEVCRefuseParDefautMemePourCast(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	b.sondeur.sonde = sondeHEV
	rep, _ := b.getCast("iptva:2002", "s1", "cast")
	if rep.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("statut %d", rep.StatusCode)
	}
	var f interface{ CodeAPI() string }
	if e := b.derniereErreur(); !errors.As(e, &f) || f.CodeAPI() != "format_hevc_non_lisible" {
		t.Errorf("erreur sans code exploitable : %v", e)
	}
}

// CA4 : option active mais client ordinaire (navigateur) : toujours 415, aucun octet.
func TestCA4_HEVCAvecOptionMaisSansEnteteCastRestRefuse(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, func(r *Reglages) { r.RelaisBrutHEVC = true })
	b.sondeur.sonde = sondeHEV
	for _, entete := range []string{"", "web", "castor"} {
		if rep, corps := b.getCast("iptva:2002", "s1", entete); rep.StatusCode != 415 || len(corps) > 200 {
			t.Errorf("en-tete %q : statut %d", entete, rep.StatusCode)
		}
	}
}

// CA4 : option active + client « cast » : le fichier est servi tel quel (octets exacts,
// type d'apres l'extension, mode direct, aucun ffmpeg), Range compris.
func TestCA4_HEVCAvecOptionEtClientCastEstServiTelQuel(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, func(r *Reglages) { r.RelaisBrutHEVC = true })
	b.sondeur.sonde = sondeHEV
	rep, corps := b.getCast("iptva:2002", "s1", "Cast")
	if rep.StatusCode != 200 {
		t.Fatalf("statut %d", rep.StatusCode)
	}
	attendu, err := http.Get(b.faux.URL() + "/movie/" + b.faux.Utilisateur() + "/" + b.faux.MotDePasse() + "/2002.mkv")
	if err != nil {
		t.Fatal(err)
	}
	defer attendu.Body.Close()
	tout, _ := io.ReadAll(attendu.Body)
	if !bytes.Equal(corps, tout) {
		t.Error("octets differents du fichier du fournisseur")
	}
	if ct := rep.Header.Get("Content-Type"); ct != "video/x-matroska" {
		t.Errorf("Content-Type = %q", ct)
	}
	if rep.Header.Get("X-Mytv-Mode") != "direct" || len(b.lanceur.liste()) != 0 {
		t.Errorf("mode %q, %d ffmpeg", rep.Header.Get("X-Mytv-Mode"), len(b.lanceur.liste()))
	}
	if b.instr.valeur("vod_relais_brut_hevc_total") != 1 {
		t.Error("relais brut non compte")
	}
}

// CA4 : « sans jugement » ne vaut que pour le HEVC ; un autre format illisible reste refuse.
func TestCA4_UnAutreFormatIllisibleRestRefuseMemePourCast(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, func(r *Reglages) { r.RelaisBrutHEVC = true })
	b.sondeur.sonde = domain.Sonde{Conteneur: "avi", CodecVideo: "mpeg2video", CodecAudio: "aac"}
	if rep, _ := b.getCast("iptva:2002", "s1", "cast"); rep.StatusCode != 415 {
		t.Errorf("statut %d", rep.StatusCode)
	}
}

// CA1 : reglages de patience incoherents refuses a la construction.
func TestCA1_ReglagesDePatienceInvalides(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil)
	for nom, r := range map[string]func(*Reglages){
		"patience negative":     func(r *Reglages) { r.PatienceMax = -time.Second },
		"reessai negatif":       func(r *Reglages) { r.Reessai = -time.Second },
		"patience sans reessai": func(r *Reglages) { r.PatienceMax = time.Second; r.Reessai = 0 },
	} {
		rg := ReglagesParDefaut()
		r(&rg)
		if _, err := Nouveau(Dependances{Fournisseurs: map[string]domain.Fournisseur{"iptva": b.client}, Stockage: b.stockage,
			Baux: b.baux, Sondeur: b.sondeur, Decideur: b.svc.dep.Decideur, Lanceur: b.lanceur}, rg); err == nil {
			t.Errorf("%s acceptee", nom)
		}
	}
}

// ---------------------------------------------------------------- US-27 : connexions

// US-27 CA2 : apres une lecture complete, puis apres un seek dont l'ancienne lecture a ete
// coupee, le faux fournisseur n'a plus aucune connexion TCP ouverte.
func TestCA2_AucuneConnexionOuverteApresFinDeLectureOuSeek(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{TailleFilm: grosFilm}, avecPatience)
	b.faux.UneSeuleConnexion(100 * time.Second)
	b.ouvrirBloque("bytes=0-", 1024)
	if b.faux.ConnexionsOuvertes() == 0 {
		t.Fatal("aucune connexion pendant la lecture : le test ne prouve rien")
	}
	rep := b.ouvrirBloque("bytes=2000000-", 1024) // seek : l'ancienne est coupee
	_, _ = io.Copy(io.Discard, rep.Body)          // fin de lecture
	attendre(t, func() bool { return b.faux.ConnexionsOuvertes() == 0 }, "aucune connexion ouverte vers le fournisseur")
}
