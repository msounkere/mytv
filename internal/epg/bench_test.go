package epg

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"mytv/internal/domain"
)

// fournisseurVolume sert un guide genere a la volee (jamais tenu en memoire).
type fournisseurVolume struct {
	fauxFournisseur
	canaux, parCanal int
}

func (f *fournisseurVolume) Requete(ctx context.Context, _ string, _ http.Header) (*http.Response, error) {
	pr, pw := io.Pipe()
	go func() {
		pw.CloseWithError(f.ecrire(ctx, pw))
	}()
	return &http.Response{StatusCode: 200, Body: pr}, nil
}

func (f *fournisseurVolume) ecrire(ctx context.Context, w io.Writer) error {
	debut := utc(10, 6, 0)
	fmt.Fprint(w, "<tv>")
	for c := 0; c < f.canaux; c++ {
		fmt.Fprintf(w, `<channel id="c%d.fr"><display-name>Chaine %d</display-name></channel>`, c, c)
	}
	for c := 0; c < f.canaux; c++ {
		for i := 0; i < f.parCanal; i++ {
			d := debut.Add(time.Duration(i) * time.Hour)
			if _, err := fmt.Fprintf(w, `<programme start="%s +0000" stop="%s +0000" channel="c%d.fr"><title>Emission %d du canal %d</title><desc>Description un peu longue pour ressembler a un vrai guide de programmes.</desc><category>Divers</category></programme>`,
				d.Format("20060102150405"), d.Add(time.Hour).Format("20060102150405"), c, i, c); err != nil {
				return err
			}
		}
	}
	_, err := fmt.Fprint(w, "</tv>")
	return err
}

// CA7 : 25 000 chaines / 500 000 programmes ingeres (telechargement en flux,
// analyse, rapprochement, ecriture SQLite) en moins de 30 s.
//
//	go test ./internal/epg -run xxx -bench Ingestion -benchtime 1x
func BenchmarkIngestion25000Chaines500000Programmes(b *testing.B) {
	const canaux, parCanal = 25000, 20
	chaines := make(listeChaines, canaux)
	for i := range chaines {
		chaines[i] = domain.Chaine{ID: domain.ContenuID(fmt.Sprintf("f:%d", i)), FournisseurID: "f", Nom: fmt.Sprintf("Chaine %d", i), EpgID: fmt.Sprintf("c%d.fr", i)}
	}
	h := &horlogeFausse{now: maintenantEssai}
	for n := 0; n < b.N; n++ {
		base, err := Ouvrir(context.Background(), filepath.Join(b.TempDir(), "epg.db"), h)
		if err != nil {
			b.Fatal(err)
		}
		f := &fournisseurVolume{fauxFournisseur: fauxFournisseur{id: "f"}, canaux: canaux, parCanal: parCanal}
		svc, err := Nouveau(chaines, base, []Source{{Fournisseur: f, Actif: true, AgeMax: time.Hour, DelaiMax: 5 * time.Minute, Reprise: time.Minute}},
			Options{Horloge: h, Journal: slog.New(slog.DiscardHandler), Passe: 6 * time.Hour, Futur: 48 * time.Hour, TailleMax: 4 << 30})
		if err != nil {
			b.Fatal(err)
		}
		debut := time.Now()
		if err := svc.Rafraichir(context.Background(), "f"); err != nil {
			b.Fatal(err)
		}
		duree := time.Since(debut)
		var total int
		base.db.QueryRow(`SELECT COUNT(*) FROM epg_programmes`).Scan(&total)
		b.ReportMetric(duree.Seconds(), "s/ingestion")
		b.ReportMetric(float64(total), "programmes")
		if total != canaux*parCanal {
			b.Fatalf("programmes stockes = %d", total)
		}
		if duree > 30*time.Second {
			b.Fatalf("ingestion en %v (> 30 s)", duree)
		}
		base.Fermer()
	}
}
