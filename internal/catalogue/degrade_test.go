package catalogue

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mytv/internal/config"
	"mytv/internal/domain"
	"mytv/internal/stockage"
	"mytv/internal/testutil/fauxiptv"
	"mytv/internal/testutil/instrum"
	"mytv/internal/xtream"
)

// US-36 CA2 : un catalogue qui perd plus de PerteMax de ses chaines (ou tombe a zero)
// est REFUSE : rien n'est ecrit, l'erreur est ErrDegrade, la metrique porte
// resultat=refus_degrade, l'observateur (supervision, webhook) la recoit comme un
// echec, le journal WARN donne les comptes. Une perte moindre passe, et une base
// vide accepte tout.
func TestCA2_SynchronisationDegradeeRefusee(t *testing.T) {
	ctx := context.Background()
	rec, obs := &instrum.Enregistreur{}, &observateurTest{}
	var sortie tampon
	journal := slog.New(slog.NewTextHandler(&sortie, nil))
	st, h, f := nouveauStockage(), &horlogeFausse{now: t0}, &fauxFournisseur{id: "a", nChaine: 4}
	s, err := Nouveau(st, []Source{actif(f, time.Hour)}, Options{Horloge: h, Lieu: time.UTC, Instrumentation: rec, Observateur: obs, Journal: journal, PerteMax: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	// Base vide : le premier import accepte tout.
	if err := s.Synchroniser(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	// 4 -> 1 chaine : 75 % de perte, refuse.
	f.nChaine = 1
	h.Avancer(time.Hour)
	err = s.Synchroniser(ctx, "a")
	if !errors.Is(err, ErrDegrade) || !strings.Contains(err.Error(), "live 4 -> 1") {
		t.Fatalf("erreur = %v, attendu ErrDegrade avec les comptes", err)
	}
	if st.nbEcritures() != 1 || len(st.catalogue["a"].Chaines) != 4 {
		t.Fatalf("le catalogue degrade a ete ecrit : %d ecritures, %d chaines", st.nbEcritures(), len(st.catalogue["a"].Chaines))
	}
	if got, _ := st.DerniereSynchro(ctx, "a"); !got.Equal(t0) {
		t.Errorf("derniere synchro = %v, doit rester %v", got, t0)
	}
	if rec.Total("synchro_total", "fournisseur", "a", "resultat", "refus_degrade") != 1 {
		t.Errorf("metrique refus_degrade absente : %+v", rec.Mesures())
	}
	obs.mu.Lock()
	if len(obs.vues) != 2 || obs.vues[0] != nil || !errors.Is(obs.vues[1], ErrDegrade) {
		t.Errorf("observateur : %v", obs.vues)
	}
	obs.mu.Unlock()
	if j := sortie.String(); !strings.Contains(j, "catalogue degrade refuse") || !strings.Contains(j, `chaines="4 -> 1"`) || !strings.Contains(j, "level=WARN") {
		t.Errorf("journal : %s", j)
	}
	// 4 -> 3 chaines : 25 % de perte, accepte.
	f.nChaine = 3
	if err := s.Synchroniser(ctx, "a"); err != nil {
		t.Fatalf("perte de 25 %% refusee : %v", err)
	}
	if st.nbEcritures() != 2 || len(st.catalogue["a"].Chaines) != 3 {
		t.Fatalf("catalogue a 3 chaines non ecrit : %d ecritures", st.nbEcritures())
	}
	// Exactement PerteMax (3 -> 2 avec perte_max 1/3) : accepte ; au-dela : refuse.
	s2, _ := Nouveau(st, []Source{actif(f, time.Hour)}, Options{Horloge: h, Lieu: time.UTC, PerteMax: 1.0 / 3})
	f.nChaine = 2
	if err := s2.Synchroniser(ctx, "a"); err != nil {
		t.Fatalf("perte egale a perte_max refusee : %v", err)
	}
	f.nChaine = 1
	if err := s2.Synchroniser(ctx, "a"); !errors.Is(err, ErrDegrade) {
		t.Fatalf("perte au-dela de perte_max acceptee : %v", err)
	}
	// Garde desactivee (PerteMax 0) : tout passe, meme zero.
	s3, _ := Nouveau(st, []Source{actif(f, time.Hour)}, Options{Horloge: h, Lieu: time.UTC})
	f.nChaine = 0 // la doublure donne au moins une chaine ; 2 -> 1 = 50 %, qui serait refuse a 1/3
	if err := s3.Synchroniser(ctx, "a"); err != nil {
		t.Fatalf("garde desactivee : %v", err)
	}
	if _, err := Nouveau(st, []Source{actif(f, time.Hour)}, Options{PerteMax: 1.5}); err == nil {
		t.Error("PerteMax > 1 doit etre refuse")
	}
}

// US-36 CA2, cas reel du 21/10 : le fournisseur repond 0 film (les chaines et series
// sont la). Vrai client Xtream, faux fournisseur HTTP, vrai stockage SQLite : la
// synchronisation est refusee, les films de la veille restent servis ; une fois le
// fournisseur retabli, la synchronisation suivante passe.
func TestCA2_CasReelFournisseurSansFilmsRefuse(t *testing.T) {
	ctx := context.Background()
	faux := fauxiptv.Nouveau(t, fauxiptv.Options{Utilisateur: "alice", MotDePasse: "MOTDEPASSE-SECRET",
		Catalogue: fauxiptv.CatalogueExemple(6, 4, 2)})
	var sortie tampon
	journal := slog.New(slog.NewTextHandler(&sortie, nil))
	client := xtream.Nouveau(config.Fournisseur{ID: "iptva", Base: faux.URL()}, faux.Utilisateur(), faux.MotDePasse(), xtream.AvecJournal(journal))
	st, err := stockage.Ouvrir(ctx, filepath.Join(t.TempDir(), "d.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Fermer()
	h := &horlogeFausse{now: t0}
	rec := &instrum.Enregistreur{}
	s, err := Nouveau(st, []Source{{DelaiMax: time.Minute, Reprise: time.Hour, Fournisseur: client, Actif: true, AgeMax: 24 * time.Hour}},
		Options{Horloge: h, Journal: journal, Lieu: time.UTC, Instrumentation: rec, PerteMax: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Synchroniser(ctx, "iptva"); err != nil {
		t.Fatal(err)
	}
	version, _ := st.VersionCatalogue(ctx)

	// 05:00 le lendemain : get_vod_streams renvoie une liste vide.
	degrade := fauxiptv.CatalogueExemple(6, 4, 2)
	degrade["get_vod_streams"] = []map[string]any{}
	faux.DefinirCatalogue(degrade)
	h.Avancer(24 * time.Hour)
	err = s.Synchroniser(ctx, "iptva")
	if !errors.Is(err, ErrDegrade) || !strings.Contains(err.Error(), "vod 4 -> 0") {
		t.Fatalf("erreur = %v, attendu ErrDegrade (vod 4 -> 0)", err)
	}
	eff, _ := st.Effectifs(ctx, "iptva")
	if eff[domain.TypeFilm] != 4 || eff[domain.TypeLive] != 6 || eff[domain.TypeSerie] != 2 {
		t.Fatalf("catalogue apres refus : %v, attendu 6/4/2", eff)
	}
	if v, _ := st.VersionCatalogue(ctx); v != version {
		t.Errorf("la version a change alors que le catalogue a ete refuse")
	}
	if got, _ := st.DerniereSynchro(ctx, "iptva"); !got.Equal(t0) {
		t.Errorf("derniere synchro = %v, doit rester %v", got, t0)
	}
	if rec.Total("synchro_total", "fournisseur", "iptva", "resultat", "refus_degrade") != 1 {
		t.Errorf("metrique : %+v", rec.Mesures())
	}
	if j := sortie.String(); !strings.Contains(j, "catalogue degrade refuse") || !strings.Contains(j, `films="4 -> 0"`) || strings.Contains(j, "MOTDEPASSE-SECRET") {
		t.Errorf("journal : %s", j)
	}
	// Fournisseur retabli, avec un film de plus : accepte.
	faux.DefinirCatalogue(fauxiptv.CatalogueExemple(6, 5, 2))
	if err := s.Synchroniser(ctx, "iptva"); err != nil {
		t.Fatal(err)
	}
	if eff, _ = st.Effectifs(ctx, "iptva"); eff[domain.TypeFilm] != 5 {
		t.Errorf("catalogue retabli : %v", eff)
	}
}
