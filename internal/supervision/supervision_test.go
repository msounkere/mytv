package supervision

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"mytv/internal/config"
	"mytv/internal/domain"
)

const (
	urlSecrete = "/hook/JETON-URL-42"
	secretTest = "secret-de-signature-test"
)

type horlogeTest struct {
	mu sync.Mutex
	t  time.Time
}

func (h *horlogeTest) now() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.t }
func (h *horlogeTest) avance(d time.Duration) {
	h.mu.Lock()
	h.t = h.t.Add(d)
	h.mu.Unlock()
}

type tampon struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (t *tampon) Write(p []byte) (int, error) { t.mu.Lock(); defer t.mu.Unlock(); return t.b.Write(p) }
func (t *tampon) String() string              { t.mu.Lock(); defer t.mu.Unlock(); return t.b.String() }

type recu struct {
	Evenement string
	Signature string
	Corps     []byte
	JSON      map[string]any
}

type banc struct {
	t       *testing.T
	m       *Moniteur
	h       *horlogeTest
	journal *tampon
	srv     *httptest.Server
	statut  int           // statut repondu par le webhook (0 = 204)
	bloque  chan struct{} // si non nil, le webhook attend sa fermeture

	mu    sync.Mutex
	recus []recu

	jauge   map[string]float64
	info    func(context.Context) (InfoCompte, error)
	base    error
	synchro time.Time
	jmu     sync.Mutex
}

func (b *banc) recusCopie() []recu {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]recu(nil), b.recus...)
}

func (b *banc) evenements() []string {
	b.m.Attendre()
	var noms []string
	for _, r := range b.recusCopie() {
		noms = append(noms, r.Evenement)
	}
	return noms
}

func nouveauBanc(t *testing.T, ajuste func(*config.Supervision)) *banc {
	t.Helper()
	b := &banc{t: t, h: &horlogeTest{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}, journal: &tampon{}, jauge: map[string]float64{}}
	b.synchro = b.h.t
	b.info = func(context.Context) (InfoCompte, error) {
		return InfoCompte{Statut: "Active", Expiration: b.h.now().Add(90 * 24 * time.Hour), ConnexionsMax: 1}, nil
	}
	b.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if b.bloque != nil {
			<-b.bloque
		}
		corps, _ := io.ReadAll(r.Body)
		var j map[string]any
		_ = json.Unmarshal(corps, &j)
		b.mu.Lock()
		b.recus = append(b.recus, recu{Evenement: r.Header.Get("X-Mytv-Evenement"), Signature: r.Header.Get("X-Mytv-Signature"), Corps: corps, JSON: j})
		b.mu.Unlock()
		if b.statut != 0 {
			w.WriteHeader(b.statut)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(b.srv.Close)
	cfg := config.Defauts().Supervision
	cfg.Webhook.URL = b.srv.URL + urlSecrete
	if ajuste != nil {
		ajuste(&cfg)
	}
	m, err := Nouveau(Options{
		Config: cfg, Horloge: b.h.now, Journal: slog.New(slog.NewJSONHandler(b.journal, nil)),
		Sources: []Source{{ID: "iptva", AgeMax: 24 * time.Hour, Info: func(ctx context.Context) (InfoCompte, error) { return b.info(ctx) }}},
		Jauge: func(nom string, v float64, e ...string) {
			b.jmu.Lock()
			b.jauge[nom+strings.Join(e, ",")] = v
			b.jmu.Unlock()
		},
		SecretWebhook: secretTest, ClientHTTP: b.srv.Client(),
		DerniereSynchro: func(context.Context, string) (time.Time, error) { return b.synchro, nil },
		Base:            func(context.Context) error { return b.base },
		Stockage:        func() error { return nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Fermer)
	b.m = m
	return b
}

func (b *banc) ligne() LigneFournisseur {
	b.t.Helper()
	return b.m.Lignes(context.Background())[0]
}

// CA1 : les erreurs du fournisseur sont rangees en trois classes distinctes.
func TestClasseDistingueCompteAdresseEtPanne(t *testing.T) {
	cas := []struct {
		err  error
		etat Etat
		code string
	}{
		{nil, EtatOK, "fournisseur_indisponible"},
		{domain.ErrIdentifiants, EtatCompteInvalide, "fournisseur_compte_invalide"},
		{fmt.Errorf("telechargement : %w", domain.ErrRefuseIP), EtatAdresseRefusee, "fournisseur_adresse_refusee"},
		{domain.ErrAmont, EtatIndisponible, "fournisseur_indisponible"},
		{errors.New("autre"), EtatIndisponible, "fournisseur_indisponible"},
	}
	for _, c := range cas {
		if got := Classe(c.err); got != c.etat {
			t.Errorf("%v : %s, attendu %s", c.err, got, c.etat)
		}
		if c.err != nil && Classe(c.err).Code() != c.code {
			t.Errorf("%v : code %s, attendu %s", c.err, Classe(c.err).Code(), c.code)
		}
	}
}

// CA2 : la synchronisation reussie lit user_info ; l'etat expose statut, expiration, connexions, ages.
func TestSynchroReussieLitUserInfoEtExposeLEtat(t *testing.T) {
	b := nouveauBanc(t, nil)
	if l := b.ligne(); l.Etat != EtatInconnu {
		t.Errorf("avant toute lecture : %s", l.Etat)
	}
	b.h.avance(2 * time.Hour)
	b.m.Synchro(context.Background(), "iptva", nil)
	l := b.ligne()
	if l.Etat != EtatOK || l.StatutCompte != "Active" || l.ConnexionsMax != 1 || l.Expiration == nil || l.ExpireDans == nil || *l.ExpireDans < 89*86400 {
		t.Errorf("ligne : %+v", l)
	}
	if l.AgeSynchro == nil || *l.AgeSynchro != 7200 || l.AlerteExpiration || l.DerniereErreur != nil || l.InfoLueLe == nil {
		t.Errorf("ages / erreur : %+v", l)
	}
	if v := b.jauge["fournisseur_expire_dans_secondesfournisseur,iptva"]; v < 89*86400 {
		t.Errorf("jauge : %v", v)
	}
	if e := b.evenements(); len(e) != 0 {
		t.Errorf("rien a notifier : %v", e)
	}
}

// CA2 : un echec garde la derniere erreur (code, age) ; il n'est pas confondu avec un refus.
func TestEchecDeSynchroEstConserveAvecSonAge(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.m.Synchro(context.Background(), "iptva", domain.ErrAmont)
	b.h.avance(5 * time.Minute)
	l := b.ligne()
	if l.Etat != EtatIndisponible || l.EchecsConsecutifs != 1 || l.DerniereErreur == nil ||
		l.DerniereErreur.Code != "fournisseur_indisponible" || l.DerniereErreur.AgeSecs != 300 {
		t.Errorf("ligne : %+v", l)
	}
}

// CA2 : a l'approche de l'expiration, avertissement dans le journal (une fois par periode)
// et metrique negative apres l'expiration.
func TestExpirationProcheJournaliseUneFoisParPeriode(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.info = func(context.Context) (InfoCompte, error) {
		return InfoCompte{Statut: "Active", Expiration: b.h.now().Add(3 * 24 * time.Hour)}, nil
	}
	b.m.Synchro(context.Background(), "iptva", nil)
	b.m.Synchro(context.Background(), "iptva", nil)
	if n := strings.Count(b.journal.String(), `"evenement":"expiration_proche"`); n != 1 {
		t.Fatalf("%d avertissements, attendu 1 :\n%s", n, b.journal.String())
	}
	if !strings.Contains(b.journal.String(), `"level":"WARN"`) || !b.ligneAlerte() {
		t.Errorf("avertissement attendu")
	}
	b.h.avance(25 * time.Hour)
	b.m.RafraichirInfo(context.Background())
	if n := strings.Count(b.journal.String(), `"evenement":"expiration_proche"`); n != 2 {
		t.Errorf("rappel apres la periode : %d", n)
	}
	// Expire : metrique negative, etat compte_invalide, evenement compte_refuse.
	b.info = func(context.Context) (InfoCompte, error) {
		return InfoCompte{Statut: "Active", Expiration: b.h.now().Add(-time.Hour)}, nil
	}
	b.m.RafraichirInfo(context.Background())
	if v := b.jauge["fournisseur_expire_dans_secondesfournisseur,iptva"]; v != -3600 {
		t.Errorf("jauge apres expiration : %v", v)
	}
	if l := b.ligne(); l.Etat != EtatCompteInvalide {
		t.Errorf("etat : %s", l.Etat)
	}
}

func (b *banc) ligneAlerte() bool { return b.ligne().AlerteExpiration }

// CA4 : le webhook recoit expiration_proche, JSON signe, une seule fois par periode (horloge injectee).
func TestWebhookExpirationProcheSigneEtAntiRafale(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.info = func(context.Context) (InfoCompte, error) {
		return InfoCompte{Statut: "Active", Expiration: b.h.now().Add(48 * time.Hour)}, nil
	}
	for range 5 {
		b.m.RafraichirInfo(context.Background())
	}
	if e := b.evenements(); len(e) != 1 || e[0] != "expiration_proche" {
		t.Fatalf("evenements : %v", e)
	}
	r := b.recusCopie()[0]
	h := hmac.New(sha256.New, []byte(secretTest))
	h.Write(r.Corps)
	if r.Signature != "sha256="+hex.EncodeToString(h.Sum(nil)) {
		t.Errorf("signature : %q", r.Signature)
	}
	if r.JSON["evenement"] != "expiration_proche" || r.JSON["fournisseur"] != "iptva" || r.JSON["expire_dans_secondes"] != float64(172800) {
		t.Errorf("corps : %s", r.Corps)
	}
	b.h.avance(23 * time.Hour)
	b.m.RafraichirInfo(context.Background())
	if e := b.evenements(); len(e) != 1 {
		t.Errorf("encore dans la periode : %v", e)
	}
	b.h.avance(2 * time.Hour)
	b.m.RafraichirInfo(context.Background())
	if e := b.evenements(); len(e) != 2 {
		t.Errorf("la periode est ecoulee : %v", e)
	}
}

// CA4 : echec de synchronisation repete N fois (config) : rien avant, un message au N-ieme, pas de rafale.
func TestWebhookEchecsRepetesApresNEssais(t *testing.T) {
	b := nouveauBanc(t, func(c *config.Supervision) { c.EchecsSynchro = 3 })
	for range 2 {
		b.m.Synchro(context.Background(), "iptva", domain.ErrAmont)
	}
	if e := b.evenements(); len(e) != 0 {
		t.Fatalf("avant N echecs : %v", e)
	}
	for range 4 {
		b.m.Synchro(context.Background(), "iptva", domain.ErrAmont)
	}
	e := b.evenements()
	if len(e) != 1 || e[0] != "echec_synchro" {
		t.Fatalf("apres N echecs : %v", e)
	}
	if b.recusCopie()[0].JSON["echecs_consecutifs"] != float64(3) {
		t.Errorf("corps : %s", b.recusCopie()[0].Corps)
	}
}

// CA4 : un refus d'identifiants ou d'adresse est notifie tout de suite (compte_refuse), puis le retour
// a la normale une fois le compte retabli.
func TestWebhookCompteRefuseEtRetourALaNormale(t *testing.T) {
	for _, cas := range []struct {
		err  error
		code string
	}{{domain.ErrIdentifiants, "fournisseur_compte_invalide"}, {domain.ErrRefuseIP, "fournisseur_adresse_refusee"}} {
		t.Run(cas.code, func(t *testing.T) {
			b := nouveauBanc(t, nil)
			b.m.Synchro(context.Background(), "iptva", cas.err)
			if l := b.ligne(); l.DerniereErreur == nil || l.DerniereErreur.Code != cas.code || string(l.Etat) != strings.TrimPrefix(cas.code, "fournisseur_") {
				t.Errorf("ligne : %+v", l)
			}
			if e := b.evenements(); len(e) != 1 || e[0] != "compte_refuse" {
				t.Fatalf("evenements : %v", e)
			}
			b.m.Synchro(context.Background(), "iptva", nil)
			e := b.evenements()
			if len(e) != 2 || e[1] != "retour_normal" {
				t.Errorf("retour a la normale : %v", e)
			}
			// Pas de nouveau « retour » sans nouvelle alerte.
			b.m.Synchro(context.Background(), "iptva", nil)
			if e := b.evenements(); len(e) != 2 {
				t.Errorf("retour_normal repete : %v", e)
			}
		})
	}
}

// CA4 : un refus vu en lecture (segment, playlist) alerte comme un refus de synchronisation.
func TestRefusVuEnLectureAlerte(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.m.SignalerRefus("iptva", domain.ErrAmont) // une panne passagere n'est pas un refus
	b.m.SignalerRefus("inconnu", domain.ErrIdentifiants)
	if e := b.evenements(); len(e) != 0 {
		t.Fatalf("%v", e)
	}
	for range 3 {
		b.m.SignalerRefus("iptva", fmt.Errorf("lecture : %w", domain.ErrRefuseIP))
	}
	if e := b.evenements(); len(e) != 1 || e[0] != "compte_refuse" {
		t.Errorf("%v", e)
	}
	if l := b.ligne(); l.Etat != EtatAdresseRefusee {
		t.Errorf("etat : %s", l.Etat)
	}
}

// CA4 : un abonnement dont le statut n'est plus « Active » est un compte refuse.
func TestStatutExpireDeUserInfoEstUnCompteInvalide(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.info = func(context.Context) (InfoCompte, error) { return InfoCompte{Statut: "Expired"}, nil }
	b.m.Synchro(context.Background(), "iptva", nil)
	if l := b.ligne(); l.Etat != EtatCompteInvalide {
		t.Errorf("etat : %s", l.Etat)
	}
	if e := b.evenements(); len(e) != 1 || e[0] != "compte_refuse" {
		t.Errorf("%v", e)
	}
}

// CA4 : un webhook lent ne bloque jamais l'appelant ; son echec est journalise sans URL ni secret.
func TestWebhookLentOuEnEchecNeBloquePasEtNeFuitRien(t *testing.T) {
	b := nouveauBanc(t, func(c *config.Supervision) { c.Webhook.Delai = config.Duree(200 * time.Millisecond) })
	b.bloque = make(chan struct{})
	debut := time.Now()
	b.m.Synchro(context.Background(), "iptva", domain.ErrIdentifiants)
	if d := time.Since(debut); d > 150*time.Millisecond {
		t.Errorf("l'appelant a attendu le webhook : %v", d)
	}
	b.m.Attendre() // le delai (200 ms) coupe l'envoi
	close(b.bloque)
	j := b.journal.String()
	if !strings.Contains(j, `"cause":"delai_depasse"`) {
		t.Errorf("echec non journalise :\n%s", j)
	}
	// Webhook qui repond 500 : meme discipline.
	b2 := nouveauBanc(t, nil)
	b2.statut = 500
	b2.m.Synchro(context.Background(), "iptva", domain.ErrIdentifiants)
	b2.m.Attendre()
	for nom, j := range map[string]string{"delai": j, "500": b2.journal.String()} {
		for _, interdit := range []string{secretTest, "JETON-URL-42", "127.0.0.1", "https://"} {
			if strings.Contains(j, interdit) {
				t.Errorf("%s : le journal contient %q :\n%s", nom, interdit, j)
			}
		}
	}
	if !strings.Contains(b2.journal.String(), `"cause":"statut_500"`) {
		t.Errorf("echec 500 non journalise :\n%s", b2.journal.String())
	}
}

// CA4 : pas d'URL configuree = aucune notification, mais journal et etat restent.
func TestSansWebhookLEtatEtLeJournalFonctionnent(t *testing.T) {
	b := nouveauBanc(t, func(c *config.Supervision) { c.Webhook.URL = "" })
	b.m.Synchro(context.Background(), "iptva", domain.ErrIdentifiants)
	b.m.Attendre()
	if len(b.recusCopie()) != 0 || !strings.Contains(b.journal.String(), `"evenement":"compte_refuse"`) {
		t.Errorf("recus %d, journal :\n%s", len(b.recusCopie()), b.journal.String())
	}
}

// CA4 : un redirection du webhook n'est pas suivie.
func TestWebhookNeSuitPasLesRedirections(t *testing.T) {
	var atteint bool
	cible := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { atteint = true }))
	defer cible.Close()
	redir := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, cible.URL, http.StatusTemporaryRedirect)
	}))
	defer redir.Close()
	cfg := config.Defauts().Supervision
	cfg.Webhook.URL = redir.URL
	// Client par defaut du paquet, mais qui fait confiance au certificat de test.
	m, _ := Nouveau(Options{Config: cfg, Sources: []Source{{ID: "x"}}, ClientHTTP: nil})
	n := m.notif
	n.client = clientSansRedirection(redir.Client())
	m.SignalerRefus("x", domain.ErrIdentifiants)
	m.Attendre()
	if atteint {
		t.Error("la redirection a ete suivie")
	}
}

func clientSansRedirection(c *http.Client) *http.Client {
	c2 := *c
	c2.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &c2
}

// CA3 : sante approfondie : tout va bien = 200 ; chaque defaut la degrade (503) sans le detail interne.
func TestSanteApprofondie(t *testing.T) {
	appel := func(b *banc) (int, reponseSante) {
		rec := httptest.NewRecorder()
		b.m.Sante().ServeHTTP(rec, httptest.NewRequest("GET", "/api/v1/admin/sante", nil))
		var r reponseSante
		if err := json.Unmarshal(rec.Body.Bytes(), &r); err != nil {
			t.Fatal(err)
		}
		return rec.Code, r
	}
	trouve := func(r reponseSante, nom string) Controle {
		for _, c := range r.Controles {
			if c.Nom == nom {
				return c
			}
		}
		t.Fatalf("controle %q absent : %+v", nom, r)
		return Controle{}
	}
	b := nouveauBanc(t, nil)
	if code, r := appel(b); code != 200 || r.Statut != "ok" || len(r.Controles) != 4 {
		t.Fatalf("sain : %d %+v", code, r)
	}
	// Synchro trop ancienne : >= 2 x age max.
	b.synchro = b.h.now().Add(-49 * time.Hour)
	if code, r := appel(b); code != 503 || trouve(r, "synchro:iptva").OK {
		t.Errorf("synchro ancienne : %d %+v", code, r)
	}
	b.synchro = b.h.now().Add(-47 * time.Hour)
	if code, _ := appel(b); code != 200 {
		t.Errorf("synchro de 47 h (< 2 x 24 h) : %d", code)
	}
	b.synchro = time.Time{}
	if _, r := appel(b); trouve(r, "synchro:iptva").Detail != "jamais synchronise" {
		t.Errorf("jamais synchronise : %+v", r)
	}
	b.synchro = b.h.now()
	// Base en panne : le texte de l'erreur n'est pas expose.
	b.base = errors.New("disk I/O error /var/lib/mytv/mytv.db")
	code, r := appel(b)
	if code != 503 || trouve(r, "base").OK || strings.Contains(trouve(r, "base").Detail, "/var/lib") {
		t.Errorf("base : %d %+v", code, r)
	}
	b.base = nil
	// Fournisseur : refus de compte, adresse refusee, panne.
	for err, attendu := range map[error]string{
		domain.ErrIdentifiants: "fournisseur_compte_invalide", domain.ErrRefuseIP: "fournisseur_adresse_refusee", domain.ErrAmont: "fournisseur_indisponible"} {
		b.info = func(context.Context) (InfoCompte, error) { return InfoCompte{}, err }
		code, r := appel(b)
		if c := trouve(r, "fournisseur:iptva"); code != 503 || c.OK || c.Detail != attendu {
			t.Errorf("%v : %d %+v", err, code, c)
		}
	}
	// Abonnement expire d'apres user_info : degrade aussi.
	b.info = func(context.Context) (InfoCompte, error) { return InfoCompte{Statut: "Expired"}, nil }
	if code, r := appel(b); code != 503 || trouve(r, "fournisseur:iptva").Detail != "fournisseur_compte_invalide" {
		t.Errorf("expire : %d %+v", code, r)
	}
}

// CA2 : GET /admin/fournisseurs est du JSON stable (champs documentes).
func TestFournisseursJSON(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.m.Synchro(context.Background(), "iptva", nil)
	rec := httptest.NewRecorder()
	b.m.Fournisseurs().ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	var brut struct{ Fournisseurs []map[string]any }
	if err := json.Unmarshal(rec.Body.Bytes(), &brut); err != nil || len(brut.Fournisseurs) != 1 {
		t.Fatal(err, rec.Body)
	}
	for _, k := range []string{"id", "etat", "statut_compte", "expiration", "expire_dans_secondes", "alerte_expiration", "connexions_max",
		"connexions_actives", "info_lue_le", "derniere_synchro_reussie", "age_synchro_secondes", "derniere_erreur", "echecs_consecutifs"} {
		if _, ok := brut.Fournisseurs[0][k]; !ok {
			t.Errorf("champ %q absent : %s", k, rec.Body)
		}
	}
}

// Planifier lit user_info au demarrage (l'expiration est connue sans attendre 05:00).
func TestPlanifierLitUserInfoAuDemarrage(t *testing.T) {
	b := nouveauBanc(t, nil)
	ctx, annuler := context.WithCancel(context.Background())
	fini := make(chan struct{})
	go func() { b.m.Planifier(ctx); close(fini) }()
	deadline := time.Now().Add(3 * time.Second)
	for b.ligne().Etat != EtatOK && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if b.ligne().Etat != EtatOK {
		t.Error("user_info non lu au demarrage")
	}
	annuler()
	<-fini
}

func TestNouveauRefuseDesReglagesInvalides(t *testing.T) {
	cfg := config.Defauts().Supervision
	cfg.PeriodeAlerte = 0
	if _, err := Nouveau(Options{Config: cfg}); err == nil {
		t.Error("periode nulle acceptee")
	}
	cfg = config.Defauts().Supervision
	if _, err := Nouveau(Options{Config: cfg, Sources: []Source{{ID: "a"}, {ID: "a"}}}); err == nil {
		t.Error("doublon accepte")
	}
}
