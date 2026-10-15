package epg

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mytv/internal/domain"
)

// bancEnrichi : le service devant le guide enrichi (icones, sous-titres, numeros d'episode).
func bancEnrichi(t *testing.T, guide string) *banc {
	t.Helper()
	brut, err := os.ReadFile("testdata/" + guide)
	if err != nil {
		t.Fatal(err)
	}
	chaines := listeChaines{
		{ID: "f:1", FournisseurID: "f", Nom: "TF1", EpgID: "tf1.fr"},                                    // sans logo -> repli
		{ID: "f:2", FournisseurID: "f", Nom: "France 2", Logo: "http://fournisseur.test/picons/f2.png"}, // logo fournisseur : intouchable
		{ID: "f:3", FournisseurID: "f", Nom: "Eurosport"},                                               // ambigu
		{ID: "f:4", FournisseurID: "f", Nom: "Arte"},                                                    // icone inutilisable
		{ID: "f:5", FournisseurID: "f", Nom: "Canal+", Logo: "   "},                                     // logo vide (espaces) -> repli
		{ID: "f:6", FournisseurID: "f", Nom: "Sans correspondance"},                                     // aucun canal
	}
	b := &banc{t: t, h: &horlogeFausse{now: maintenantEssai}, logs: &tampon{}}
	b.base, err = Ouvrir(context.Background(), filepath.Join(t.TempDir(), "epg.db"), b.h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.base.Fermer() })
	b.f = &fauxFournisseur{id: "f", corps: func() []byte { return brut }}
	src := Source{Fournisseur: b.f, Actif: true, AgeMax: 24 * time.Hour, DelaiMax: time.Minute, Reprise: 10 * time.Minute}
	opt := Options{Horloge: b.h, Journal: slog.New(slog.NewTextHandler(b.logs, nil)), Passe: 6 * time.Hour, Futur: 24 * time.Hour, TailleMax: 64 << 20}
	b.svc, err = Nouveau(chaines, b.base, []Source{src}, opt)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// US-29 CA1 : <icon src> du canal est lu ; seules les adresses http(s) sont retenues.
func TestCA1_AnalyseLitLesIconesDesCanaux(t *testing.T) {
	brut, _ := os.ReadFile("testdata/guide_enrichi.xml")
	cs, _, _, err := analyser(t, strings.NewReader(string(brut)), ParamsAnalyse{De: fenDe, A: fenA.Add(24 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	logos := map[string]string{}
	for _, c := range cs {
		logos[c.ID] = c.Logo
	}
	attendu := map[string]string{
		"tf1.fr":       "http://logos.test/tf1.png",
		"france2.fr":   "https://logos.test/f2.png",
		"arte.fr":      "",                                 // data: refuse
		"canalplus.fr": "https://logos.test/canalplus.png", // chemin relatif ignore, espaces retires
	}
	for id, l := range attendu {
		if logos[id] != l {
			t.Errorf("logo de %s = %q, attendu %q", id, logos[id], l)
		}
	}
}

// US-29 CA1 : apres rapprochement, les chaines SANS logo recoivent celui du guide ;
// jamais d'ecrasement d'un logo fournisseur, jamais pour une chaine ambigue.
func TestCA1_LogoDuGuideEnRepliSansEcraserLeFournisseur(t *testing.T) {
	b := bancEnrichi(t, "guide_enrichi.xml")
	if err := b.svc.Rafraichir(context.Background(), "f"); err != nil {
		t.Fatal(err)
	}
	logos, err := b.base.LogosEPG(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	attendu := map[domain.ContenuID]string{"f:1": "http://logos.test/tf1.png", "f:5": "https://logos.test/canalplus.png"}
	if len(logos) != len(attendu) {
		t.Fatalf("logos = %v, attendu %v", logos, attendu)
	}
	for id, l := range attendu {
		if logos[id] != l {
			t.Errorf("%s = %q, attendu %q", id, logos[id], l)
		}
	}
	for _, id := range []domain.ContenuID{"f:2", "f:3", "f:4", "f:6"} {
		if l, ok := logos[id]; ok {
			t.Errorf("%s ne doit pas recevoir de logo du guide (%q)", id, l)
		}
	}
	// Lecture filtree par chaines (lots) : memes valeurs, chaines inconnues absentes.
	sel, _ := b.base.LogosEPG(context.Background(), []domain.ContenuID{"f:1", "f:2", "inconnue"})
	if len(sel) != 1 || sel["f:1"] != "http://logos.test/tf1.png" {
		t.Errorf("selection = %v", sel)
	}
	// Une synchronisation sans icone remplace les logos (rien ne reste d'un ancien guide).
	b.f.corps = func() []byte {
		return []byte(`<tv><channel id="tf1.fr"><display-name>TF1</display-name></channel>` +
			`<programme start="20260310090000 +0000" stop="20260310100000 +0000" channel="tf1.fr"><title>X</title></programme></tv>`)
	}
	b.h.now = b.h.now.Add(48 * time.Hour)
	if err := b.svc.Rafraichir(context.Background(), "f"); err != nil {
		t.Fatal(err)
	}
	if logos, _ = b.base.LogosEPG(context.Background(), nil); len(logos) != 0 {
		t.Errorf("logos d'un ancien guide conserves : %v", logos)
	}
}

// US-29 CA2 : sous-titre, numero d'episode et annee sont gardes, bornes, relus par
// « en cours et suivant » comme par la grille.
func TestCA2_SousTitreEpisodeEtAnneeConserves(t *testing.T) {
	b := bancEnrichi(t, "guide_enrichi.xml")
	if err := b.svc.Rafraichir(context.Background(), "f"); err != nil {
		t.Fatal(err)
	}
	g := b.grille("f:1")
	if len(g) != 5 {
		t.Fatalf("programmes de TF1 : %d", len(g))
	}
	type attendu struct{ sous, episode, annee string }
	cas := []attendu{
		{"Le feu aux poudres", "S02E05", "2008"},    // libelle d'ecran prefere
		{"", "S01E12", "2019"},                      // xmltv_ns compte depuis zero ; date AAAAMMJJ
		{strings.Repeat("x", maxSousTitre), "", ""}, // sous-titre borne ; dd_progid et date illisible ignores
		{"", "E04", ""},                             // sans saison ; annee 1500 invraisemblable
		{"", "E01", ""},                             // libelle de 200 caracteres ignore ; saison 99999 refusee ; episode -4 refuse
	}
	// Dernier cas : xmltv_ns "99999.-4." -> ni saison ni episode valides.
	cas[4].episode = ""
	for i, c := range cas {
		p := g[i]
		if p.SousTitre != c.sous || p.Episode != c.episode || p.Annee != c.annee {
			t.Errorf("programme %d (%s) : sous-titre %q episode %q annee %q ; attendu %q %q %q",
				i, p.Titre, p.SousTitre, p.Episode, p.Annee, c.sous, c.episode, c.annee)
		}
	}
	cur, _, err := b.base.EnCoursEtSuivant(context.Background(), "f:1", utc(10, 9, 30))
	if err != nil || cur == nil || cur.SousTitre != "Le feu aux poudres" || cur.Episode != "S02E05" || cur.Annee != "2008" {
		t.Errorf("en cours = %+v %v", cur, err)
	}
}

// US-29 CA2 : conversion xmltv_ns -> « SxxEyy » (valeurs limites).
func TestEpisodeDepuisXMLTVNS(t *testing.T) {
	for in, att := range map[string]string{
		"0.0.":          "S01E01",
		"1.4.":          "S02E05",
		" 0 . 11 . 0/1": "S01E12",
		"2.5/12.":       "S03E06",
		". 3 .":         "E04",
		"4.":            "",
		"":              "",
		"a.b.c":         "",
		"9998.9998.":    "S9999E9999",
		"9999.0.":       "E01",
		"-1.2.":         "E03",
	} {
		if got := depuisXMLTVNS(in); got != att {
			t.Errorf("depuisXMLTVNS(%q) = %q, attendu %q", in, got, att)
		}
	}
}

// Une base creee AVANT l'US-29 (migration 1 seule) est migree sans perdre ses programmes.
func TestMigrationConserveLesProgrammesExistants(t *testing.T) {
	chemin := filepath.Join(t.TempDir(), "epg.db")
	ctx := context.Background()
	anc := migrations
	migrations = anc[:1] // l'ancien schema
	b, err := Ouvrir(ctx, chemin, nil)
	if err != nil {
		migrations = anc
		t.Fatal(err)
	}
	_, err = b.db.ExecContext(ctx, `INSERT INTO epg_programmes (fournisseur_id, epg_id, debut, fin, titre, description, categorie) VALUES ('f','a',1000,2000,'vieux','','')`)
	b.Fermer()
	migrations = anc
	if err != nil {
		t.Fatal(err)
	}
	b, err = Ouvrir(ctx, chemin, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Fermer()
	var titre, episode string
	if err := b.db.QueryRowContext(ctx, `SELECT titre, episode FROM epg_programmes`).Scan(&titre, &episode); err != nil || titre != "vieux" || episode != "" {
		t.Fatalf("%q %q %v", titre, episode, err)
	}
}
