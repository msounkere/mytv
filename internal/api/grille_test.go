package api

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mytv/internal/domain"
	"mytv/internal/epg"
)

// bancGrille : l'API devant une VRAIE base EPG de nChaines chaines, chacune avec des
// programmes de 30 minutes couvrant 12 h a partir de midi (UTC).
func bancGrille(t *testing.T, nChaines int) (*banc, *http.Cookie, time.Time) {
	t.Helper()
	ctx := context.Background()
	base, err := epg.Ouvrir(ctx, filepath.Join(t.TempDir(), "epg.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { base.Fermer() })
	debut := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	inst := &domain.InstantaneEPG{FournisseurID: "a", Pris: debut}
	for i := 0; i < nChaines; i++ {
		epgID := fmt.Sprintf("c%d.fr", i)
		inst.Rapprochements = append(inst.Rapprochements, domain.Rapprochement{
			Chaine: domain.ContenuID(fmt.Sprintf("a:%d", 1000+i)), EpgID: epgID, Methode: domain.RapprocheParEpgID})
		for k := 0; k < 24; k++ {
			inst.Programmes = append(inst.Programmes, domain.Programme{FournisseurID: "a", EpgID: epgID,
				Debut: debut.Add(time.Duration(k) * 30 * time.Minute), Fin: debut.Add(time.Duration(k+1) * 30 * time.Minute),
				Titre: fmt.Sprintf("Emission %d de la chaine %d", k, i), Description: "Une description assez longue pour ressembler a un vrai guide de programmes.", Categorie: "Divertissement"})
		}
	}
	if err := base.EcrireEPG(ctx, inst); err != nil {
		t.Fatal(err)
	}
	b := nouveauBanc(t, func(o *Options) { o.EPG = base })
	return b, b.connecte("alice", false), debut
}

func chemin(ids []string, de, a time.Time) string {
	return fmt.Sprintf("/api/v1/epg/grille?chaines=%s&de=%s&a=%s", strings.Join(ids, ","), de.Format(time.RFC3339), a.Format(time.RFC3339))
}

// US-16 CA2 : chaines inconnues ou sans guide = tableau vide, jamais une erreur ;
// les programmes d'une chaine arrivent ordonnes ; la plage est [de, a[.
func TestCA2_GrilleChainesInconnuesOuSansGuide(t *testing.T) {
	b, c, debut := bancGrille(t, 3)
	rec := b.requete("GET", chemin([]string{"a:1000", "a:1001", "a:9999", "a:1002"}, debut, debut.Add(2*time.Hour)), "", avec(c))
	if rec.Code != 200 {
		t.Fatalf("grille : %d %s", rec.Code, rec.Body)
	}
	var out struct {
		Chaines map[string][]struct{ Debut, Fin, Titre string }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Chaines) != 4 {
		t.Fatalf("une entree par chaine demandee attendue : %v", out.Chaines)
	}
	if l := out.Chaines["a:9999"]; l == nil || len(l) != 0 {
		t.Errorf("chaine inconnue : %v (attendu [] et non null)", l)
	}
	if !strings.Contains(rec.Body.String(), `"a:9999":[]`) {
		t.Errorf("tableau vide attendu pour la chaine inconnue : %s", rec.Body)
	}
	l := out.Chaines["a:1000"]
	if len(l) != 4 || l[0].Debut != "2026-01-01T12:00:00Z" || l[3].Fin != "2026-01-01T14:00:00Z" {
		t.Errorf("a:1000 (2 h = 4 programmes) : %v", l)
	}
	for i := 1; i < len(l); i++ {
		if l[i].Debut < l[i-1].Debut {
			t.Error("programmes non ordonnes")
		}
	}
	// Limites : trop de chaines, plage trop longue ou inversee.
	ids := make([]string, b.cfg.API.PaginationMax+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("a:%d", i)
	}
	for nom, p := range map[string]string{
		"trop de chaines":   chemin(ids, debut, debut.Add(time.Hour)),
		"plage trop longue": chemin([]string{"a:1000"}, debut, debut.Add(b.cfg.API.GrilleFenetreMax.Std()+time.Second)),
		"plage inversee":    chemin([]string{"a:1000"}, debut.Add(time.Hour), debut),
	} {
		if rec := b.requete("GET", p, "", avec(c)); rec.Code != 400 {
			t.Errorf("%s : %d au lieu de 400", nom, rec.Code)
		}
	}
	// Le maximum exact passe.
	ids = ids[:b.cfg.API.PaginationMax]
	if rec := b.requete("GET", chemin(ids, debut, debut.Add(b.cfg.API.GrilleFenetreMax.Std())), "", avec(c)); rec.Code != 200 {
		t.Errorf("limites exactes refusees : %d %s", rec.Code, rec.Body)
	}
}

// US-16 CA2 : test de charge — 200 chaines x 6 h en UN appel, sur une vraie base EPG :
// reponse correcte, compacte (gzip) et rapide.
func TestCA2_GrilleDeuxCentChainesSixHeures(t *testing.T) {
	b, c, debut := bancGrille(t, 200)
	ids := make([]string, 200)
	for i := range ids {
		ids[i] = fmt.Sprintf("a:%d", 1000+i)
	}
	url := chemin(ids, debut, debut.Add(6*time.Hour))
	srv := httptest.NewServer(b.h)
	defer srv.Close()
	appel := func(gzipOK bool) (corps []byte, taille int, d time.Duration) {
		req, _ := http.NewRequest("GET", srv.URL+url, nil)
		req.AddCookie(c)
		if gzipOK {
			req.Header.Set("Accept-Encoding", "gzip")
		}
		cl := &http.Client{Transport: &http.Transport{DisableKeepAlives: true, DisableCompression: true}}
		t0 := time.Now()
		rep, err := cl.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer rep.Body.Close()
		brut, _ := io.ReadAll(rep.Body)
		d = time.Since(t0)
		taille = len(brut)
		if (rep.Header.Get("Content-Encoding") == "gzip") != gzipOK {
			t.Fatalf("gzip=%v mais Content-Encoding=%q", gzipOK, rep.Header.Get("Content-Encoding"))
		}
		if gzipOK {
			z, err := gzip.NewReader(strings.NewReader(string(brut)))
			if err != nil {
				t.Fatal(err)
			}
			brut, _ = io.ReadAll(z)
		}
		return brut, taille, d
	}
	clair, tClair, _ := appel(false)
	comp, tComp, d := appel(true)
	if string(clair) != string(comp) {
		t.Error("le corps compresse n'est pas identique au corps en clair")
	}
	var out struct {
		Chaines map[string][]json.RawMessage `json:"chaines"`
	}
	if err := json.Unmarshal(comp, &out); err != nil || len(out.Chaines) != 200 {
		t.Fatalf("200 chaines attendues : %d %v", len(out.Chaines), err)
	}
	for id, l := range out.Chaines {
		if len(l) != 12 { // 6 h de programmes de 30 minutes
			t.Fatalf("%s : %d programmes au lieu de 12", id, len(l))
		}
	}
	t.Logf("grille 200 chaines x 6 h : %d octets en clair, %d compresses, %s", tClair, tComp, d)
	if tComp*5 > tClair {
		t.Errorf("compression insuffisante : %d -> %d", tClair, tComp)
	}
	if d > 2*time.Second && !avecRace {
		t.Errorf("grille en %s", d)
	}
}
