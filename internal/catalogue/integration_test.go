package catalogue

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mytv/internal/config"
	"mytv/internal/domain"
	"mytv/internal/stockage"
	"mytv/internal/testutil/fauxiptv"
	"mytv/internal/xtream"
)

// Integration : vrai client Xtream + faux fournisseur HTTP + vrai stockage SQLite.
// Aucun reseau reel : le faux fournisseur ecoute sur 127.0.0.1.
func TestIntegrationXtreamFauxFournisseurEtSQLite(t *testing.T) {
	ctx := context.Background()
	faux := fauxiptv.Nouveau(t, fauxiptv.Options{
		Utilisateur: "alice", MotDePasse: "MOTDEPASSE-SECRET",
		Catalogue: fauxiptv.CatalogueExemple(6, 4, 2),
	})
	var sortie tampon
	journal := slog.New(slog.NewTextHandler(&sortie, &slog.HandlerOptions{Level: slog.LevelDebug}))
	client := xtream.Nouveau(config.Fournisseur{ID: "iptva", Base: faux.URL()}, faux.Utilisateur(), faux.MotDePasse(),
		xtream.AvecJournal(journal))
	st, err := stockage.Ouvrir(ctx, filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Fermer()
	h := &horlogeFausse{now: t0}
	s, err := Nouveau(st, []Source{{Fournisseur: client, Actif: true, AgeMax: 24 * time.Hour}},
		Options{Horloge: h, Journal: journal, Lieu: time.UTC})
	if err != nil {
		t.Fatal(err)
	}

	// La latence garde le vol ouvert assez longtemps pour que les 8 demandes le rejoignent.
	faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleAPI, Latence: 300 * time.Millisecond})
	// CA4 : 8 demandes simultanees = un seul jeu d'appels d'API.
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Synchroniser(ctx, "iptva"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	faux.Effacer()
	if appels := faux.Compteurs().ParAction["get_live_streams"]; appels != 1 {
		t.Fatalf("get_live_streams appele %d fois pour 8 demandes simultanees, attendu 1", appels)
	}

	// CA1 : le catalogue est lisible depuis le stockage.
	cs, err := st.Chaines(ctx, domain.FiltreChaines{})
	if err != nil || len(cs) != 6 {
		t.Fatalf("%d chaines en stockage (%v), attendu 6", len(cs), err)
	}
	if c, err := st.Chaine(ctx, "iptva:1001"); err != nil || c.Nom != "Chaine 1" {
		t.Fatalf("Chaine(iptva:1001) = %+v, %v", c, err)
	}
	if got, _ := st.DerniereSynchro(ctx, "iptva"); !got.Equal(t0) {
		t.Fatalf("derniere synchro = %v", got)
	}

	// CA2 : catalogue frais, aucun appel supplementaire.
	faux.RazCompteurs()
	if ok, err := s.SiPerime(ctx, "iptva"); err != nil || ok {
		t.Fatalf("SiPerime sur catalogue frais : %v %v", ok, err)
	}
	if faux.Compteurs().API != 0 {
		t.Fatal("appel d'API inutile sur un catalogue frais")
	}

	// CA1 : panne amont (502) -> erreur typee, ancien catalogue conserve.
	h.Avancer(48 * time.Hour)
	faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleAPI, Statut: 502})
	if ok, err := s.SiPerime(ctx, "iptva"); !errors.Is(err, domain.ErrAmont) || ok {
		t.Fatalf("SiPerime pendant la panne : %v %v", ok, err)
	}
	if cs, _ := st.Chaines(ctx, domain.FiltreChaines{}); len(cs) != 6 {
		t.Fatalf("%d chaines apres la panne, l'ancien catalogue devait rester", len(cs))
	}
	// 458 : adresse refusee, typee aussi.
	faux.Effacer()
	faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleAPI, Statut: 458})
	if err := s.Synchroniser(ctx, "iptva"); !errors.Is(err, domain.ErrRefuseIP) {
		t.Fatalf("458 : %v", err)
	}

	// Retablissement.
	faux.Effacer()
	if err := s.Synchroniser(ctx, "iptva"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.DerniereSynchro(ctx, "iptva"); !got.Equal(t0.Add(48 * time.Hour)) {
		t.Fatalf("derniere synchro apres retablissement = %v", got)
	}

	// CA6 : le mot de passe n'apparait dans aucun journal.
	if log := sortie.String(); strings.Contains(log, "MOTDEPASSE-SECRET") || !strings.Contains(log, "catalogue synchronise") {
		t.Fatalf("journal inattendu :\n%s", log)
	}
}
