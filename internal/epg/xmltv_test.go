package epg

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"mytv/internal/domain"
)

// Fenetre du jeu d'essai testdata/guide.xml.
var (
	fenDe = time.Date(2026, 3, 10, 6, 0, 0, 0, time.UTC)
	fenA  = time.Date(2026, 3, 11, 12, 0, 0, 0, time.UTC)
)

func utc(j, h, m int) time.Time { return time.Date(2026, 3, j, h, m, 0, 0, time.UTC) }

func lireGuide(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/guide.xml")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func analyser(t *testing.T, r io.Reader, p ParamsAnalyse) ([]Canal, []domain.Programme, Stats, error) {
	t.Helper()
	var cs []Canal
	var ps []domain.Programme
	st, err := Analyser(context.Background(), r, p, func(c Canal) { cs = append(cs, c) }, func(p domain.Programme) { ps = append(ps, p) })
	return cs, ps, st, err
}

// CA2 : dates avec fuseau, sans fuseau, sans secondes ; formes de decalage.
func TestParserDateToutesLesFormes(t *testing.T) {
	paris := time.FixedZone("", 3600)
	cas := []struct {
		in   string
		lieu *time.Location
		ok   bool
		att  time.Time
	}{
		{"20260310110000 +0100", nil, true, utc(10, 10, 0)},
		{"20260310110000+0100", nil, true, utc(10, 10, 0)},
		{"20260310070000 -0500", nil, true, utc(10, 12, 0)},
		{"20260310110000 +01:00", nil, true, utc(10, 10, 0)},
		{"20260310110000 +0530", nil, true, utc(10, 5, 30)},
		{"20260310110000 Z", nil, true, utc(10, 11, 0)},
		{"20260310110000", nil, true, utc(10, 11, 0)},   // UTC par defaut
		{"20260310110000", paris, true, utc(10, 10, 0)}, // fuseau par defaut configure
		{"202603101100", nil, true, utc(10, 11, 0)},
		{"2026031011", nil, true, utc(10, 11, 0)},
		{"20260310", nil, true, utc(10, 0, 0)},
		{"20260310110000 +0100", paris, true, utc(10, 10, 0)}, // le decalage explicite l'emporte
		{"", nil, false, time.Time{}},
		{"demain", nil, false, time.Time{}},
		{"2026031", nil, false, time.Time{}},
		{"20261310110000", nil, false, time.Time{}}, // mois 13
		{"20260310110000 +9900", nil, false, time.Time{}},
		{"20260310110000 abc", nil, false, time.Time{}},
	}
	for _, c := range cas {
		got, ok := ParserDate(c.in, c.lieu)
		if ok != c.ok || (ok && !got.Equal(c.att)) {
			t.Errorf("ParserDate(%q) = %v, %v ; attendu %v, %v", c.in, got, ok, c.att, c.ok)
		}
		if ok && got.Location() != time.UTC {
			t.Errorf("ParserDate(%q) doit rendre de l'UTC", c.in)
		}
	}
}

// CA2 : jeu d'essai realiste, cas tordus compris.
func TestAnalyseDuJeuDEssai(t *testing.T) {
	cs, ps, st, err := analyser(t, bytes.NewReader(lireGuide(t)), ParamsAnalyse{De: fenDe, A: fenA})
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 8 || cs[0].ID != "TF1.fr" || len(cs[0].Noms) != 2 {
		t.Fatalf("canaux = %+v", cs)
	}
	// Lus : 17 programmes ; 2 invalides (fin < debut, date illisible) ; 2 hors fenetre.
	if st.Canaux != 8 || st.Programmes != 17 || st.Invalides != 2 || st.HorsFenetre != 2 || st.Emis != 13 {
		t.Fatalf("stats = %+v", st)
	}
	var tf1 []domain.Programme
	for _, p := range ps {
		if p.EpgID == "TF1.fr" {
			tf1 = append(tf1, p)
		}
	}
	// Le doublon est EMIS deux fois par l'analyse (c'est l'ingestion qui le retire) ;
	// l'analyse ne fait que lire.
	if len(tf1) != 5 {
		t.Fatalf("programmes de TF1 = %+v", tf1)
	}
	j := tf1[0]
	if j.Titre != "Journal & meteo" || j.Description != "Les titres de la mi-journee." || j.Categorie != "Information" ||
		!j.Debut.Equal(utc(10, 10, 0)) || !j.Fin.Equal(utc(10, 11, 0)) {
		t.Fatalf("journal = %+v", j)
	}
	if !tf1[2].Debut.Equal(utc(10, 12, 0)) { // -0500
		t.Fatalf("film de midi debute a %v", tf1[2].Debut)
	}
	if sans := tf1[3]; sans.Titre != "Sans fin connue" || !sans.Fin.IsZero() || !sans.Debut.Equal(utc(10, 13, 0)) {
		t.Fatalf("programme sans fin = %+v", sans)
	}
}

// CA2 : le fuseau par defaut s'applique aux dates sans decalage.
func TestAnalyseFuseauParDefaut(t *testing.T) {
	paris := time.FixedZone("", 3600)
	xml := `<tv><programme start="20260310140000" stop="20260310150000" channel="a"><title>x</title></programme></tv>`
	_, ps, _, err := analyser(t, strings.NewReader(xml), ParamsAnalyse{Lieu: paris})
	if err != nil || len(ps) != 1 || !ps[0].Debut.Equal(utc(10, 13, 0)) {
		t.Fatalf("%+v %v", ps, err)
	}
}

// CA1 : gzip detecte a la signature (et non a un nom de fichier ou un en-tete).
func TestAnalyseGzip(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write(lireGuide(t))
	gz.Close()
	_, ps, st, err := analyser(t, &buf, ParamsAnalyse{De: fenDe, A: fenA})
	if err != nil || st.Emis != 13 || len(ps) != 13 {
		t.Fatalf("gzip : %+v %v", st, err)
	}
}

// CA1 : le guide n'est JAMAIS charge en entier. Un flux presque infini, interrompu
// par le test apres 1000 programmes, n'a produit qu'une petite partie du flux.
func TestAnalyseEnFluxSansToutCharger(t *testing.T) {
	g := &generateur{max: 10_000_000}
	ctx, annuler := context.WithCancel(context.Background())
	defer annuler()
	n := 0
	_, err := Analyser(ctx, g, ParamsAnalyse{}, nil, func(domain.Programme) {
		if n++; n == 1000 {
			annuler()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("erreur = %v", err)
	}
	if g.octets > 4<<20 {
		t.Fatalf("%d octets lus pour 1000 programmes : le flux est bufferise", g.octets)
	}
}

// generateur produit un XMLTV de `max` programmes sans jamais le tenir en memoire.
type generateur struct {
	max, i int
	octets int
	reste  []byte
	debut  bool
}

func (g *generateur) Read(p []byte) (int, error) {
	if len(g.reste) == 0 {
		switch {
		case !g.debut:
			g.debut = true
			g.reste = []byte("<tv>")
		case g.i < g.max:
			g.reste = []byte(fmt.Sprintf(`<programme start="20260310100000 +0000" stop="20260310110000 +0000" channel="c%d"><title>t%d</title></programme>`, g.i%50, g.i))
			g.i++
		default:
			return 0, io.EOF
		}
	}
	n := copy(p, g.reste)
	g.reste = g.reste[n:]
	g.octets += n
	return n, nil
}

// CA1 : garde-fou contre un flux (ou une bombe gzip) demesure.
func TestAnalyseTailleMax(t *testing.T) {
	_, _, _, err := analyser(t, &generateur{max: 100000}, ParamsAnalyse{TailleMax: 64 << 10})
	if !errors.Is(err, ErrTropGros) {
		t.Fatalf("erreur = %v", err)
	}
}

func TestAnalyseXMLIllisible(t *testing.T) {
	_, _, _, err := analyser(t, strings.NewReader(`<tv><programme start="2026`), ParamsAnalyse{})
	if err == nil {
		t.Fatal("un flux tronque doit etre une erreur (l'ancien EPG sera conserve)")
	}
}

func TestAnalyseLatin1(t *testing.T) {
	xml := "<?xml version=\"1.0\" encoding=\"ISO-8859-1\"?><tv><channel id=\"a\"><display-name>T\xe9l\xe9 \xc9clair</display-name></channel></tv>"
	cs, _, _, err := analyser(t, strings.NewReader(xml), ParamsAnalyse{})
	if err != nil || len(cs) != 1 || cs[0].Noms[0] != "Télé Éclair" {
		t.Fatalf("%+v %v", cs, err)
	}
}

func TestAnalyseTexteLongTronque(t *testing.T) {
	long := strings.Repeat("é", 5000)
	xml := `<tv><programme start="20260310100000" stop="20260310110000" channel="a"><title>` + long + `</title><desc>` + long + `</desc></programme></tv>`
	_, ps, _, err := analyser(t, strings.NewReader(xml), ParamsAnalyse{})
	if err != nil || len(ps) != 1 || len(ps[0].Titre) > maxTitre || len(ps[0].Description) > maxDescription {
		t.Fatalf("%v %v", ps, err)
	}
	if !strings.HasSuffix(ps[0].Titre, "é") {
		t.Fatal("coupure au milieu d'une rune")
	}
}

func TestAnalyseAnnulee(t *testing.T) {
	ctx, annuler := context.WithCancel(context.Background())
	annuler()
	_, err := Analyser(ctx, strings.NewReader(`<tv/>`), ParamsAnalyse{}, nil, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("erreur = %v", err)
	}
}
