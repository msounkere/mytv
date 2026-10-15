package api

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mytv/internal/domain"
	"mytv/internal/epg"
)

// bancLogosEPG : l'API devant une VRAIE base EPG qui porte des logos de repli et un
// programme detaille, et le VRAI proxy d'images.
func bancLogosEPG(t *testing.T) (*banc, *http.Cookie, *fauxImages) {
	t.Helper()
	ctx := context.Background()
	base, err := epg.Ouvrir(ctx, filepath.Join(t.TempDir(), "epg.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { base.Fermer() })
	debut := time.Date(2026, 1, 1, 11, 0, 0, 0, time.UTC)
	inst := &domain.InstantaneEPG{FournisseurID: "a", Pris: debut,
		Rapprochements: []domain.Rapprochement{{Chaine: "a:1", EpgID: "tf1.fr", Methode: domain.RapprocheParEpgID}},
		// a:2 a un logo du fournisseur ET un logo du guide : seul celui du fournisseur compte.
		Logos: []domain.LogoEPG{{Chaine: "a:1", Logo: "http://logos.test/tf1.png"}, {Chaine: "a:2", Logo: "http://logos.test/f2-guide.png"}},
		Programmes: []domain.Programme{{FournisseurID: "a", EpgID: "tf1.fr", Debut: debut, Fin: debut.Add(2 * time.Hour),
			Titre: "Les Experts", SousTitre: "Le feu aux poudres", Episode: "S02E05", Annee: "2008"}}}
	if err := base.EcrireEPG(ctx, inst); err != nil {
		t.Fatal(err)
	}
	f := nouveauFauxImages(t)
	b := nouveauBanc(t, func(o *Options) { o.EPG = base; o.Images = serviceImages(t, f, nil) })
	b.st.chaines = []domain.Chaine{
		{ID: "a:1", FournisseurID: "a", Numero: "1", Nom: "TF1", NumeroChaine: 1},                                          // repli du guide
		{ID: "a:2", FournisseurID: "a", Numero: "2", Nom: "France 2", Logo: f.srv.URL + "/picons/f2.png", NumeroChaine: 2}, // fournisseur
		{ID: "a:3", FournisseurID: "a", Numero: "3", Nom: "Sans rien", NumeroChaine: 3},                                    // aucun
	}
	return b, b.connecte("alice", false), f
}

// US-29 CA1 : le logo du guide remplace l'absence de logo, dans toutes les sorties, et
// PASSE PAR LE PROXY D'IMAGES ; un logo du fournisseur n'est jamais remplace.
func TestCA1_LogoDuGuideEnRepliViaLeProxy(t *testing.T) {
	b, c, f := bancLogosEPG(t)
	logoDe := func(chemin string, id string) (string, bool) {
		rec := b.requete("GET", chemin, "", avec(c), gz)
		if rec.Code != 200 {
			t.Fatalf("%s : %d", chemin, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "logos.test") {
			t.Errorf("%s laisse voir l'adresse du guide", chemin)
		}
		var els []map[string]any
		switch {
		case strings.Contains(chemin, "recherche"):
			var m struct{ Chaines []map[string]any }
			_ = json.Unmarshal(rec.Body.Bytes(), &m)
			els = m.Chaines
		default:
			els = elements(corpsJSON(t, rec))
		}
		for _, e := range els {
			if e["id"] == id {
				l, ok := e["logo"].(string)
				return l, ok
			}
		}
		t.Fatalf("%s : %s absent", chemin, id)
		return "", false
	}
	for _, chemin := range []string{"/api/v1/chaines", "/api/v1/recherche?q=tf1"} {
		if l, ok := logoDe(chemin, "a:1"); !ok || !strings.HasPrefix(l, "/api/v1/image/") {
			t.Errorf("%s : repli du guide absent (%q)", chemin, l)
		}
	}
	rec := b.requete("GET", "/api/v1/chaines/a:1", "", avec(c))
	if l, _ := corpsJSON(t, rec)["logo"].(string); !strings.HasPrefix(l, "/api/v1/image/") {
		t.Errorf("fiche : %q", l)
	}
	// Le logo du fournisseur n'est pas remplace, et celui du repli est un autre jeton.
	l2, _ := logoDe("/api/v1/chaines", "a:2")
	l1, _ := logoDe("/api/v1/chaines", "a:1")
	if l2 == "" || l2 == l1 {
		t.Errorf("logos : a:2=%q a:1=%q", l2, l1)
	}
	if rec := b.requete("GET", l2, "", avec(c)); rec.Code != 200 || f.dernierChemin() != "/picons/f2.png" {
		t.Errorf("a:2 doit servir SON logo (/picons/f2.png), le proxy a demande %q (%d)", f.dernierChemin(), rec.Code)
	}
	// Aucun logo nulle part : champ absent.
	if _, ok := logoDe("/api/v1/chaines", "a:3"); ok {
		t.Error("a:3 n'a aucun logo, le champ doit etre absent")
	}
}

// US-29 CA1 : le jeton du repli sert bien l'image (le proxy ne le refuse pas).
func TestCA1_LogoDeRepliServiParLeProxy(t *testing.T) {
	f := nouveauFauxImages(t)
	svc := serviceImages(t, f, nil)
	base := f.srv.URL
	b := nouveauBanc(t, func(o *Options) {
		o.Images = svc
		o.EPG = epgFaux{logos: map[domain.ContenuID]string{"a:1": base + "/guide/tf1.png"}}
	})
	b.st.chaines = []domain.Chaine{{ID: "a:1", FournisseurID: "a", Numero: "1", Nom: "TF1"}}
	c := b.connecte("alice", false)
	logo := corpsJSON(t, b.requete("GET", "/api/v1/chaines/a:1", "", avec(c)))["logo"].(string)
	rec := b.requete("GET", logo, "", avec(c))
	if rec.Code != 200 || rec.Body.String() != string(png) {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	if got := f.dernierChemin(); got != "/guide/tf1.png" {
		t.Errorf("le proxy a demande %q", got)
	}
}

// US-29 CA1 : la liste des logos est gardee en memoire (une lecture par synchro du
// guide), et les ETag du catalogue changent quand un logo change -- pas sinon.
func TestCA1_LogosEnMemoireEtETag(t *testing.T) {
	lectures := 0
	logos := map[domain.ContenuID]string{"a:1": "http://logos.test/a.png"}
	sync1 := time.Unix(1000, 0)
	epgf := &epgMobile{epgFaux: epgFaux{lectures: &lectures, logos: logos, synchro: sync1}}
	b := nouveauBanc(t, func(o *Options) { o.EPG = epgf })
	b.st.chaines = []domain.Chaine{{ID: "a:1", FournisseurID: "a", Numero: "1", Nom: "TF1"}}
	c := b.connecte("alice", false)
	etag := func() string { return b.requete("GET", "/api/v1/chaines", "", avec(c)).Header().Get("ETag") }
	e1 := etag()
	for i := 0; i < 20; i++ {
		b.requete("GET", "/api/v1/chaines", "", avec(c))
		b.requete("GET", "/api/v1/catalogue/export?type=live", "", avec(c))
	}
	if lectures != 1 {
		t.Fatalf("%d lectures de la base pour 41 requetes : la liste doit rester en memoire", lectures)
	}
	// Nouvelle synchro du guide, MEMES logos : l'ETag ne change pas (pas de reimport inutile).
	epgf.maj(func(e *epgFaux) { e.synchro = time.Unix(2000, 0) })
	if e2 := etag(); e2 != e1 {
		t.Error("une synchro sans changement de logo ne doit pas invalider les ETag")
	}
	if lectures != 2 {
		t.Errorf("la nouvelle synchro doit recharger la liste une fois : %d", lectures)
	}
	// Un logo change : l'ETag change.
	epgf.maj(func(e *epgFaux) {
		e.synchro = time.Unix(3000, 0)
		e.logos = map[domain.ContenuID]string{"a:1": "http://logos.test/b.png"}
	})
	if e3 := etag(); e3 == e1 {
		t.Error("un logo a change : l'ETag du catalogue doit changer")
	}
}

// epgMobile : epgFaux dont on change les donnees en cours de test.
type epgMobile struct{ epgFaux }

func (e *epgMobile) maj(f func(*epgFaux)) { f(&e.epgFaux) }

// US-29 CA2 : sous-titre, episode et annee sortent de « maintenant » et de la grille ;
// absents quand le guide ne les donne pas.
func TestCA2_RoutesEPGExposentSousTitreEpisodeAnnee(t *testing.T) {
	b, c, _ := bancLogosEPG(t)
	rec := b.requete("GET", "/api/v1/epg/chaines/a:1/maintenant", "", avec(c))
	var m struct {
		Courant map[string]any `json:"courant"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil || rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	_ = m
	// Heure fixe de l'API de test : 2026-01-01 12:00 UTC, dans le programme 11:00-13:00.
	if m.Courant["sous_titre"] != "Le feu aux poudres" || m.Courant["episode"] != "S02E05" || m.Courant["annee"] != "2008" {
		t.Errorf("maintenant = %v", m.Courant)
	}
	de := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	rec = b.requete("GET", chemin([]string{"a:1"}, de, de.Add(6*time.Hour)), "", avec(c))
	var g struct {
		Chaines map[string][]map[string]any `json:"chaines"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &g); err != nil || len(g.Chaines["a:1"]) != 1 {
		t.Fatalf("grille : %d %s", rec.Code, rec.Body)
	}
	if p := g.Chaines["a:1"][0]; p["sous_titre"] != "Le feu aux poudres" || p["episode"] != "S02E05" || p["annee"] != "2008" {
		t.Errorf("grille = %v", p)
	}
	// Sans detail : champs absents (pas de chaines vides dans le JSON).
	b2 := nouveauBanc(t, nil)
	b2.remplir(1)
	c2 := b2.connecte("alice", false)
	corps := b2.requete("GET", "/api/v1/epg/chaines/a:100/maintenant", "", avec(c2)).Body.String()
	if strings.Contains(corps, "sous_titre") || strings.Contains(corps, "episode") || strings.Contains(corps, "annee") {
		t.Errorf("champs vides serialises : %s", corps)
	}
}

// US-29 CA2 : le contrat decrit exactement les champs du programme.
func TestCA2_ContratProgrammeDecritLesNouveauxChamps(t *testing.T) {
	props, requis := proprietesContrat(t, "Programme")
	for _, champ := range []string{"sous_titre", "episode", "annee"} {
		if !props[champ] {
			t.Errorf("champ %s absent du contrat Programme", champ)
		}
		for _, r := range requis {
			if r == champ {
				t.Errorf("%s ne doit pas etre requis", champ)
			}
		}
	}
}

// US-29 CA1 : l'export NDJSON (import de l'interface) porte aussi le repli.
func TestCA1_ExportPorteLeLogoDeRepli(t *testing.T) {
	ctx := context.Background()
	base, err := epg.Ouvrir(ctx, filepath.Join(t.TempDir(), "epg.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { base.Fermer() })
	inst := instantaneExport(time.UnixMilli(5000), 3, 0, 0)
	inst.Chaines[0].Logo = ""                          // a:1000 : repli
	inst.Chaines[1].Logo = "http://img.test/garde.png" // a:1001 : fournisseur
	inst.Chaines[2].Logo = ""                          // a:1002 : pas de repli
	if err := base.EcrireEPG(ctx, &domain.InstantaneEPG{FournisseurID: "a", Pris: time.UnixMilli(5000),
		Rapprochements: []domain.Rapprochement{{Chaine: "a:1000", EpgID: "x", Methode: domain.RapprocheParEpgID}, {Chaine: "a:1001", EpgID: "y", Methode: domain.RapprocheParEpgID}},
		Logos:          []domain.LogoEPG{{Chaine: "a:1000", Logo: "http://logos.test/x.png"}, {Chaine: "a:1001", Logo: "http://logos.test/y.png"}}}); err != nil {
		t.Fatal(err)
	}
	b, st, c := bancExport(t, 0, 0, 0)
	if err := st.EcrireCatalogue(ctx, inst); err != nil {
		t.Fatal(err)
	}
	b.api.opt.EPG = base
	b.api.opt.Images = serviceImages(t, nouveauFauxImages(t), nil)
	rec := b.requete("GET", "/api/v1/catalogue/export?type=live", "", avec(c), gz)
	got := map[string]string{}
	for _, l := range lignes(t, rec) {
		got[l["id"].(string)], _ = l["logo"].(string)
	}
	if !strings.HasPrefix(got["a:1000"], "/api/v1/image/") || !strings.HasPrefix(got["a:1001"], "/api/v1/image/") || got["a:1002"] != "" {
		t.Fatalf("logos exportes : %v", got)
	}
	if got["a:1000"] == got["a:1001"] {
		t.Error("a:1001 garde SON logo, pas celui du guide")
	}
}
