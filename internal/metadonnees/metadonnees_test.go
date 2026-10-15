package metadonnees

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mytv/internal/config"
	"mytv/internal/domain"
	"mytv/internal/stockage"
	"mytv/internal/testutil/fauxiptv"
	"mytv/internal/xtream"
)

// horloge manuelle : le debit, le coupe-circuit et les delais d'attente se testent
// sans dormir.
type horloge struct {
	mu sync.Mutex
	t  time.Time
}

func (h *horloge) Now() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.t }
func (h *horloge) Avancer(d time.Duration) {
	h.mu.Lock()
	h.t = h.t.Add(d)
	h.mu.Unlock()
}

type banc struct {
	t       *testing.T
	faux    *fauxiptv.Faux
	base    *stockage.Base
	svc     *Service
	h       *horloge
	journal *bytes.Buffer
	pause   atomic.Bool
}

const mdpSecret = "mdp-secret-xyz-123"

func reglages() config.Metadonnees {
	return config.Defauts().Metadonnees
}

func nouveauBanc(t *testing.T, mod func(*config.Metadonnees)) *banc {
	t.Helper()
	b := &banc{t: t, h: &horloge{t: time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)}, journal: &bytes.Buffer{}}
	infos := map[string]any{}
	for i := 1; i <= 30; i++ {
		infos[fmt.Sprint(2000+i)] = map[string]any{"info": map[string]any{"plot": fmt.Sprintf("Synopsis %d", 2000+i), "genre": "Drame", "duration_secs": 3600}}
	}
	b.faux = fauxiptv.Nouveau(t, fauxiptv.Options{MotDePasse: mdpSecret, InfosFilm: infos, InfosSerie: map[string]any{
		"3001": map[string]any{
			"info":     map[string]any{"plot": "Resume de la serie", "genre": "Comedie"},
			"episodes": map[string]any{"1": []map[string]any{{"id": "9001", "episode_num": 1, "title": "Pilote", "info": map[string]any{"plot": "Ep 1"}}}},
		}}})
	var err error
	b.base, err = stockage.Ouvrir(context.Background(), filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.base.Fermer() })
	cfg := reglages()
	if mod != nil {
		mod(&cfg)
	}
	journal := slog.New(slog.NewTextHandler(b.journal, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cl := xtream.Nouveau(config.Fournisseur{ID: "a", Base: b.faux.URL(), Reseau: config.Reseau{
		DelaiConnexion: config.Duree(2 * time.Second), DelaiEntete: config.Duree(10 * time.Second)}},
		b.faux.Utilisateur(), mdpSecret, xtream.AvecJournal(journal))
	t.Cleanup(cl.Fermer)
	b.svc, err = Nouveau(Options{Config: cfg, Fournisseurs: map[string]domain.FournisseurDetails{"a": cl}, Stockage: b.base,
		Pause: b.pause.Load, Horloge: b.h.Now, Journal: journal})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func film(n int) domain.Film {
	return domain.Film{ID: domain.ContenuID(fmt.Sprintf("a:%d", n)), FournisseurID: "a", Numero: fmt.Sprint(n), Nom: "Film"}
}

func (b *banc) appels() int { return b.faux.Compteurs().ParAction["get_vod_info"] }

// CA3/CA6 : 8 demandes simultanees du meme film = UN SEUL appel au fournisseur.
func TestSingleflightHuitDemandesUnAppel(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleDetails, Latence: 300 * time.Millisecond}) // laisse les demandes se chevaucher
	var wg sync.WaitGroup
	res := make([]ResultatFilm, 8)
	for i := range res {
		wg.Add(1)
		go func() { defer wg.Done(); res[i] = b.svc.Film(context.Background(), film(2001), true) }()
	}
	wg.Wait()
	if n := b.appels(); n != 1 {
		t.Fatalf("%d appels au fournisseur pour 8 demandes", n)
	}
	for i, r := range res {
		if r.Statut != Ok || r.Details == nil || r.Details.Synopsis != "Synopsis 2001" {
			t.Errorf("demande %d : %+v", i, r)
		}
	}
}

// CA2/CA3/CA6 : le premier appel remplit le cache ; les suivants ne touchent plus le
// fournisseur ; details=non (recuperer=faux) ne declenche JAMAIS d'appel.
func TestCacheEtDetailsNon(t *testing.T) {
	b := nouveauBanc(t, nil)
	r := b.svc.Film(context.Background(), film(2002), false)
	if r.Statut != NonDemande || r.Details != nil || b.appels() != 0 {
		t.Fatalf("details=non sans cache : %+v, %d appels", r, b.appels())
	}
	if r = b.svc.Film(context.Background(), film(2002), true); r.Statut != Ok || b.appels() != 1 {
		t.Fatalf("premier : %+v %d", r, b.appels())
	}
	for i := 0; i < 5; i++ {
		if r = b.svc.Film(context.Background(), film(2002), true); r.Statut != Ok || r.Details.Synopsis != "Synopsis 2002" {
			t.Fatalf("depuis le cache : %+v", r)
		}
	}
	if r = b.svc.Film(context.Background(), film(2002), false); r.Statut != Ok || r.Details == nil {
		t.Errorf("details=non avec cache : %+v", r)
	}
	if b.appels() != 1 {
		t.Fatalf("%d appels, attendu 1", b.appels())
	}
}

// CA2 : au-dela de la duree de vie, les details sont redemandes (une seule fois).
func TestDetailsPerimesRedemandes(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.svc.Film(context.Background(), film(2003), true)
	b.h.Avancer(31 * 24 * time.Hour)
	if r := b.svc.Film(context.Background(), film(2003), true); r.Statut != Ok || b.appels() != 2 {
		t.Fatalf("%+v %d appels", r, b.appels())
	}
	if b.svc.Film(context.Background(), film(2003), true); b.appels() != 2 {
		t.Fatalf("redemande en trop : %d", b.appels())
	}
}

// CA2/CA4/CA6 : fournisseur en panne = degradation propre. Pas de details (mais pas
// d'erreur), et pas de marteau : le meme contenu n'est pas redemande avant echec_attente.
func TestDegradationEtPasDeMartelage(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleDetails, Statut: 502})
	for i := 0; i < 4; i++ {
		if r := b.svc.Film(context.Background(), film(2004), true); r.Statut != Indisponible || r.Details != nil {
			t.Fatalf("essai %d : %+v", i, r)
		}
	}
	if b.appels() != 1 {
		t.Fatalf("%d appels pendant la panne, attendu 1", b.appels())
	}
	b.faux.Effacer()
	b.h.Avancer(3 * time.Minute) // echec_attente (2 min) ecoule : on retente
	if r := b.svc.Film(context.Background(), film(2004), true); r.Statut != Ok || b.appels() != 2 {
		t.Fatalf("apres l'attente : %+v %d", r, b.appels())
	}
}

// CA2 : un echec (ou une fiche vide) ne vide JAMAIS des details deja stockes : les
// anciens restent servis, marques « perime ».
func TestEchecNeVidePasLesDetailsStockes(t *testing.T) {
	for nom, programmer := range map[string]func(*banc){
		"502":        func(b *banc) { b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleDetails, Statut: 502}) },
		"404":        func(b *banc) { b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleDetails, Statut: 404}) },
		"fiche vide": func(b *banc) { b.faux.DefinirInfoFilm("2005", []any{}) },
		"info vide":  func(b *banc) { b.faux.DefinirInfoFilm("2005", map[string]any{"info": []any{}}) },
	} {
		t.Run(nom, func(t *testing.T) {
			b := nouveauBanc(t, nil)
			if r := b.svc.Film(context.Background(), film(2005), true); r.Statut != Ok {
				t.Fatal(r)
			}
			b.h.Avancer(40 * 24 * time.Hour) // perime
			programmer(b)
			r := b.svc.Film(context.Background(), film(2005), true)
			if r.Statut != Perime || r.Details == nil || r.Details.Synopsis != "Synopsis 2005" {
				t.Fatalf("%+v", r)
			}
			// Et le stockage les a toujours.
			if d, _, err := b.base.DetailsFilm(context.Background(), "a:2005"); err != nil || d.Synopsis != "Synopsis 2005" {
				t.Fatalf("stockage vide : %+v %v", d, err)
			}
		})
	}
}

// CA2 : une fiche rafraichie incomplete complete l'ancienne sans la vider.
func TestFicheIncompleteFusionneAvecLAncienne(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.svc.Film(context.Background(), film(2006), true)
	b.h.Avancer(40 * 24 * time.Hour)
	b.faux.DefinirInfoFilm("2006", map[string]any{"info": map[string]any{"cast": "Nouveau casting"}})
	r := b.svc.Film(context.Background(), film(2006), true)
	if r.Statut != Ok || r.Details.Casting != "Nouveau casting" || r.Details.Synopsis != "Synopsis 2006" || r.Details.Genre != "Drame" {
		t.Fatalf("%+v", r.Details)
	}
}

// CA3 : un film sans fiche (404 ou [] ) donne « absent » et n'est pas redemande a chaque ouverture.
func TestFilmSansFicheAbsentSansMartelage(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.faux.DefinirInfoFilm("2007", []any{})
	for i := 0; i < 3; i++ {
		if r := b.svc.Film(context.Background(), film(2007), true); r.Statut != Absent || r.Details != nil {
			t.Fatalf("%+v", r)
		}
	}
	if b.appels() != 1 {
		t.Fatalf("%d appels", b.appels())
	}
}

// CA4/CA6 : coupe-circuit. Apres un 458 (ou 401/403) plus AUCUN appel, pour aucun
// contenu, pendant coupe_circuit ; puis UNE sonde de reprise.
func TestCoupeCircuit(t *testing.T) {
	for _, statut := range []int{401, 403, 458} {
		t.Run(fmt.Sprint(statut), func(t *testing.T) {
			b := nouveauBanc(t, nil)
			b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleDetails, Statut: statut, Fois: 1})
			if r := b.svc.Film(context.Background(), film(2001), true); r.Statut != Indisponible {
				t.Fatalf("%+v", r)
			}
			for n := 2002; n < 2012; n++ {
				if r := b.svc.Film(context.Background(), film(n), true); r.Statut != Indisponible {
					t.Fatalf("film %d : %+v", n, r)
				}
			}
			if b.appels() != 1 {
				t.Fatalf("%d appels pendant le coupe-circuit, attendu 1", b.appels())
			}
			// Le coupe-circuit couvre aussi les saisons : erreur, sans appel.
			if _, err := b.svc.Saisons(context.Background(), domain.SerieTV{ID: "a:s3001", FournisseurID: "a", Numero: "3001"}); err == nil {
				t.Error("saisons pendant le coupe-circuit")
			}
			if b.faux.Compteurs().ParAction["get_series_info"] != 0 {
				t.Error("appel de saisons pendant le coupe-circuit")
			}
			b.h.Avancer(11 * time.Minute)
			if r := b.svc.Film(context.Background(), film(2003), true); r.Statut != Ok {
				t.Fatalf("apres le coupe-circuit : %+v", r)
			}
			if b.appels() != 2 {
				t.Fatalf("%d appels", b.appels())
			}
		})
	}
}

// CA4 : apres expiration, UNE SEULE sonde part meme si dix demandes arrivent ensemble ;
// si elle est de nouveau refusee, le circuit se reouvre.
func TestCoupeCircuitUneSeuleSondeDeReprise(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleDetails, Statut: 458, Fois: 1})
	b.svc.Film(context.Background(), film(2001), true)
	b.h.Avancer(11 * time.Minute)
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleDetails, Statut: 458, Latence: 200 * time.Millisecond})
	var wg sync.WaitGroup
	for n := 2010; n < 2020; n++ {
		wg.Add(1)
		go func() { defer wg.Done(); b.svc.Film(context.Background(), film(n), true) }()
	}
	wg.Wait()
	if b.appels() != 2 { // le premier refus + UNE sonde
		t.Fatalf("%d appels, attendu 2", b.appels())
	}
	if r := b.svc.Film(context.Background(), film(2021), true); r.Statut != Indisponible || b.appels() != 2 {
		t.Fatalf("circuit non reouvert : %+v %d", r, b.appels())
	}
}

// CA4/CA6 : la concurrence amont est bornee (metadonnees.concurrence_max).
func TestConcurrenceAmontBornee(t *testing.T) {
	b := nouveauBanc(t, func(c *config.Metadonnees) {
		c.ConcurrenceMax = 2
		c.AppelsParMinute = 100
		c.Delai = config.Duree(20 * time.Second)
	})
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleDetails, Latence: 100 * time.Millisecond})
	var wg sync.WaitGroup
	var ok atomic.Int32
	for n := 2001; n <= 2012; n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if b.svc.Film(context.Background(), film(n), true).Statut == Ok {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if pic := b.faux.Compteurs().DetailsPic; pic > 2 || pic < 1 {
		t.Fatalf("pic de %d appels simultanes, borne 2", pic)
	}
	if ok.Load() != 12 {
		t.Fatalf("%d/12 reussis : la file d'attente doit servir tout le monde", ok.Load())
	}
}

// CA4/CA6 : debit borne. Au-dela de N appels dans la minute : aucun appel, reponse
// degradee ; la minute suivante, ca repart.
func TestDebitBorne(t *testing.T) {
	b := nouveauBanc(t, func(c *config.Metadonnees) { c.AppelsParMinute = 3 })
	var degrades int
	for n := 2001; n <= 2008; n++ {
		if b.svc.Film(context.Background(), film(n), true).Statut == Indisponible {
			degrades++
		}
	}
	if b.appels() != 3 || degrades != 5 {
		t.Fatalf("%d appels, %d degrades (attendu 3 et 5)", b.appels(), degrades)
	}
	b.h.Avancer(61 * time.Second)
	if r := b.svc.Film(context.Background(), film(2009), true); r.Statut != Ok || b.appels() != 4 {
		t.Fatalf("apres la minute : %+v %d", r, b.appels())
	}
	// Le contenu refuse pour cause de debit N'EST PAS memorise comme en echec : il passe des que possible.
	if r := b.svc.Film(context.Background(), film(2008), true); r.Statut != Ok {
		t.Fatalf("film refuse par le debit toujours bloque : %+v", r)
	}
}

// CA4 : aucun appel pendant l'import du catalogue.
func TestAucunAppelPendantLImport(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.pause.Store(true)
	if r := b.svc.Film(context.Background(), film(2001), true); r.Statut != Indisponible || b.appels() != 0 {
		t.Fatalf("%+v %d", r, b.appels())
	}
	if _, err := b.svc.Saisons(context.Background(), domain.SerieTV{ID: "a:s3001", FournisseurID: "a", Numero: "3001"}); err == nil {
		t.Error("saisons pendant l'import")
	}
	b.pause.Store(false)
	if r := b.svc.Film(context.Background(), film(2001), true); r.Statut != Ok {
		t.Fatalf("apres l'import : %+v", r)
	}
}

// CA3 : delai court. Un fournisseur lent ne fait pas attendre l'utilisateur plus que
// metadonnees.delai ; la reponse est degradee.
func TestDelaiCourt(t *testing.T) {
	b := nouveauBanc(t, func(c *config.Metadonnees) { c.Delai = config.Duree(150 * time.Millisecond) })
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleDetails, Latence: 3 * time.Second})
	debut := time.Now()
	r := b.svc.Film(context.Background(), film(2001), true)
	if r.Statut != Indisponible || time.Since(debut) > 2*time.Second {
		t.Fatalf("%+v apres %v", r, time.Since(debut))
	}
}

// CA3 : un demandeur qui part (contexte annule) n'annule pas l'appel des autres.
func TestUnDemandeurQuiPartNAnnulePasLesAutres(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleDetails, Latence: 300 * time.Millisecond})
	ctx, annuler := context.WithCancel(context.Background())
	fini := make(chan ResultatFilm)
	go func() { fini <- b.svc.Film(context.Background(), film(2001), true) }()
	time.Sleep(50 * time.Millisecond)
	go func() { time.Sleep(50 * time.Millisecond); annuler() }()
	b.svc.Film(ctx, film(2001), true) // rejoint le vol puis s'en va
	if r := <-fini; r.Statut != Ok {
		t.Fatalf("%+v", r)
	}
	if b.appels() != 1 {
		t.Fatalf("%d appels", b.appels())
	}
}

// CA3/CA6 : series. Les details et la liste des saisons partagent UN get_series_info ;
// sans memoire, chaque route refait son appel.
func TestSerieEtSaisonsPartagentUnAppel(t *testing.T) {
	b := nouveauBanc(t, nil)
	s := domain.SerieTV{ID: "a:s3001", FournisseurID: "a", Numero: "3001"}
	r := b.svc.Serie(context.Background(), s, true)
	if r.Statut != Ok || r.Details.Synopsis != "Resume de la serie" || r.Details.Genre != "Comedie" {
		t.Fatalf("%+v", r)
	}
	ss, err := b.svc.Saisons(context.Background(), s)
	if err != nil || len(ss) != 1 || ss[0].Episodes[0].Synopsis != "Ep 1" {
		t.Fatalf("%+v %v", ss, err)
	}
	if n := b.faux.Compteurs().ParAction["get_series_info"]; n != 1 {
		t.Fatalf("%d appels get_series_info, attendu 1", n)
	}
	// Depuis le stockage : une serie deja connue ne rappelle plus.
	b.h.Avancer(time.Hour)
	if r := b.svc.Serie(context.Background(), s, true); r.Statut != Ok || b.faux.Compteurs().ParAction["get_series_info"] != 1 {
		t.Fatalf("%+v", r)
	}
	// Mais la liste des saisons, elle, est toujours fraiche une fois la memoire courte ecoulee.
	if _, err := b.svc.Saisons(context.Background(), s); err != nil || b.faux.Compteurs().ParAction["get_series_info"] != 2 {
		t.Fatalf("saisons : %v %d", err, b.faux.Compteurs().ParAction["get_series_info"])
	}
}

// CA3 : l'ouverture d'une fiche serie par le chemin des saisons enregistre aussi les
// details de la serie (aucun appel supplementaire ensuite).
func TestSaisonsAlimententLesDetailsDeLaSerie(t *testing.T) {
	b := nouveauBanc(t, nil)
	s := domain.SerieTV{ID: "a:s3001", FournisseurID: "a", Numero: "3001"}
	if _, err := b.svc.Saisons(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	b.h.Avancer(time.Hour)
	if r := b.svc.Serie(context.Background(), s, false); r.Statut != Ok || r.Details.Synopsis != "Resume de la serie" {
		t.Fatalf("%+v", r)
	}
}

// CA3/CA4 : l'erreur des saisons est rendue telle quelle (la fiche n'a rien d'autre a montrer).
func TestSaisonsEnErreurRemonte(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleDetails, Statut: 502})
	if _, err := b.svc.Saisons(context.Background(), domain.SerieTV{ID: "a:s3001", FournisseurID: "a", Numero: "3001"}); err == nil {
		t.Fatal("erreur attendue")
	}
}

// Securite : ni le mot de passe du fournisseur ni son adresse ne sortent dans le
// journal, meme en echec.
func TestAucunSecretDansLeJournal(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleDetails, Statut: 502, Fois: 2})
	b.svc.Film(context.Background(), film(2001), true)
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleDetails, Statut: 458, Fois: 1})
	b.svc.Film(context.Background(), film(2002), true)
	if j := b.journal.String(); strings.Contains(j, mdpSecret) || strings.Contains(j, "password=") {
		t.Fatalf("secret dans le journal :\n%s", j)
	}
}

// CA4 : metadonnees.actif = faux coupe toute recuperation, mais les details deja
// stockes restent servis.
func TestDesactivation(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.svc.Film(context.Background(), film(2001), true)
	b.svc.cfg.Actif = false
	if r := b.svc.Film(context.Background(), film(2001), true); r.Statut != Ok || r.Details == nil {
		t.Fatalf("stockes : %+v", r)
	}
	if r := b.svc.Film(context.Background(), film(2002), true); r.Statut != Indisponible || b.appels() != 1 {
		t.Fatalf("%+v %d", r, b.appels())
	}
}
