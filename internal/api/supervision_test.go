package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"mytv/internal/config"
	"mytv/internal/domain"
	"mytv/internal/supervision"
)

type superviseurFaux struct {
	exp     time.Time
	refus   []error
	ids     []string
	connuId string
}

func (s *superviseurFaux) Expiration(id string) (time.Time, bool) {
	s.connuId = id
	return s.exp, !s.exp.IsZero()
}
func (s *superviseurFaux) SignalerRefus(id string, err error) {
	s.ids, s.refus = append(s.ids, id), append(s.refus, err)
}

// US-25 CA1 : les trois pannes du fournisseur ont chacune leur code (502), le compte refuse porte
// la date d'expiration quand elle est connue, la supervision est prevenue des refus (pas des pannes).
func TestFournisseurCompteAdresseEtPanneSontDistingues(t *testing.T) {
	sup := &superviseurFaux{exp: time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)}
	b := nouveauBanc(t, func(o *Options) { o.Supervision = sup })
	c := b.connecte("alice", false)
	cas := []struct {
		err   error
		code  string
		date  bool
		refus bool
	}{
		{domain.ErrIdentifiants, "fournisseur_compte_invalide", true, true},
		{errors.Join(domain.ErrIdentifiants, errors.New("detail interne")), "fournisseur_compte_invalide", true, true},
		{domain.ErrRefuseIP, "fournisseur_adresse_refusee", false, true},
		{domain.ErrAmont, "fournisseur_indisponible", false, false},
	}
	for _, k := range cas {
		avant := len(sup.refus)
		b.lecture.err = k.err
		rec := b.requete("GET", "/api/v1/vod/a:5", "", avec(c))
		var corps map[string]string
		if err := json.Unmarshal(rec.Body.Bytes(), &corps); err != nil {
			t.Fatal(err, rec.Body)
		}
		if rec.Header().Get("X-Mytv-Erreur") != k.code || (rec.Header().Get("X-Mytv-Expiration") != "") != k.date {
			t.Errorf("%v : en-tetes %v", k.err, rec.Header())
		}
		if rec.Code != 502 || corps["code"] != k.code || corps["message"] == "" {
			t.Errorf("%v : %d %v", k.err, rec.Code, corps)
		}
		if (corps["expiration"] == "2026-10-01T00:00:00Z") != k.date || (!k.date && corps["expiration"] != "") {
			t.Errorf("%v : expiration %q", k.err, corps["expiration"])
		}
		if strings.Contains(rec.Body.String(), "detail interne") || strings.Contains(rec.Body.String(), "fournisseur :") {
			t.Errorf("le corps cite une erreur interne : %s", rec.Body)
		}
		if (len(sup.refus) > avant) != k.refus {
			t.Errorf("%v : signalement %v, attendu %v", k.err, len(sup.refus) > avant, k.refus)
		}
	}
	if sup.connuId != "a" || sup.ids[0] != "a" {
		t.Errorf("fournisseur interroge : %q %v", sup.connuId, sup.ids)
	}
	// Une reponse a HEAD n'a pas de corps : le code reste lisible dans l'en-tete (lecteur HTML5, hls.js).
	b.lecture.err = domain.ErrIdentifiants
	if rec := b.requete("HEAD", "/api/v1/vod/a:5", "", avec(c)); rec.Code != 502 || rec.Header().Get("X-Mytv-Erreur") != "fournisseur_compte_invalide" {
		t.Errorf("HEAD : %d %v", rec.Code, rec.Header())
	}
	// Sans supervision, les memes codes restent distincts.
	b2 := nouveauBanc(t, nil)
	c2 := b2.connecte("alice", false)
	b2.lecture.err = domain.ErrIdentifiants
	if rec := b2.requete("GET", "/api/v1/vod/a:5", "", avec(c2)); rec.Code != 502 || !strings.Contains(rec.Body.String(), "fournisseur_compte_invalide") || strings.Contains(rec.Body.String(), "expiration") {
		t.Errorf("sans supervision : %d %s", rec.Code, rec.Body)
	}
}

func banPourSupervision(t *testing.T, info func(context.Context) (supervision.InfoCompte, error)) (*banc, *supervision.Moniteur) {
	t.Helper()
	cfg := config.Defauts().Supervision
	m, err := supervision.Nouveau(supervision.Options{Config: cfg,
		Sources:         []supervision.Source{{ID: "a", AgeMax: 24 * time.Hour, Info: info}},
		DerniereSynchro: func(context.Context, string) (time.Time, error) { return time.Now(), nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Fermer)
	b := nouveauBanc(t, func(o *Options) {
		o.Admin["fournisseurs"] = m.Fournisseurs()
		o.Admin["sante"] = m.Sante()
		o.Supervision = m
	})
	return b, m
}

// US-25 CA2/CA3 : routes d'administration protegees (401 sans session, 403 pour un non-administrateur),
// 501 si la supervision n'est pas branchee ; /sante reste sans detail.
func TestRoutesAdminSupervisionProtegees(t *testing.T) {
	b, _ := banPourSupervision(t, func(context.Context) (supervision.InfoCompte, error) {
		return supervision.InfoCompte{Statut: "Active"}, nil
	})
	simple, admin := b.connecte("alice", false), b.connecte("root", true)
	for _, chemin := range []string{"/api/v1/admin/fournisseurs", "/api/v1/admin/sante"} {
		if rec := b.requete("GET", chemin, ""); rec.Code != 401 {
			t.Errorf("%s sans session : %d", chemin, rec.Code)
		}
		if rec := b.requete("GET", chemin, "", avec(simple)); rec.Code != 403 {
			t.Errorf("%s non admin : %d", chemin, rec.Code)
		}
		if rec := b.requete("GET", chemin, "", avec(admin)); rec.Code != 200 {
			t.Errorf("%s admin : %d %s", chemin, rec.Code, rec.Body)
		}
		b0 := nouveauBanc(t, nil)
		if rec := b0.requete("GET", chemin, "", avec(b0.connecte("r", true))); rec.Code != 501 {
			t.Errorf("%s sans gestionnaire : %d", chemin, rec.Code)
		}
	}
	if rec := b.requete("GET", "/api/v1/sante", ""); rec.Code != 200 || strings.TrimSpace(rec.Body.String()) != `{"statut":"ok"}` {
		t.Errorf("sante publique : %d %s", rec.Code, rec.Body)
	}
}

// US-25 CA2/CA3 : le corps reel a exactement les champs du contrat (schemas EtatFournisseurs,
// SanteApprofondie et Erreur), et la sante passe a 503 quand le compte est refuse.
func TestReponsesSupervisionConformesAuContrat(t *testing.T) {
	refuse := false
	b, m := banPourSupervision(t, func(context.Context) (supervision.InfoCompte, error) {
		if refuse {
			return supervision.InfoCompte{}, domain.ErrIdentifiants
		}
		return supervision.InfoCompte{Statut: "Active", Expiration: time.Now().Add(72 * time.Hour), ConnexionsMax: 1}, nil
	})
	admin := b.connecte("root", true)
	m.Synchro(context.Background(), "a", nil)
	contrat, _ := lireContrat(t)
	cles := func(schema string, chemin ...string) []string {
		var s struct {
			Properties map[string]yaml.Node `yaml:"properties"`
			Items      *struct {
				Properties map[string]yaml.Node `yaml:"properties"`
			} `yaml:"items"`
		}
		n := contrat.Components.Schemas[schema]
		if err := n.Decode(&s); err != nil {
			t.Fatal(err)
		}
		if len(chemin) == 1 { // propriete tableau : ses items
			var item struct {
				Items struct {
					Properties map[string]yaml.Node `yaml:"properties"`
				} `yaml:"items"`
			}
			p := s.Properties[chemin[0]]
			_ = p.Decode(&item)
			return trie(item.Items.Properties)
		}
		return trie(s.Properties)
	}
	var etat struct{ Fournisseurs []map[string]any }
	rec := b.requete("GET", "/api/v1/admin/fournisseurs", "", avec(admin))
	if err := json.Unmarshal(rec.Body.Bytes(), &etat); err != nil || len(etat.Fournisseurs) != 1 {
		t.Fatal(err, rec.Body)
	}
	if reel := trie(etat.Fournisseurs[0]); strings.Join(reel, ",") != strings.Join(cles("EtatFournisseurs", "fournisseurs"), ",") {
		t.Errorf("champs reels %v, contrat %v", reel, cles("EtatFournisseurs", "fournisseurs"))
	}
	if f := etat.Fournisseurs[0]; f["etat"] != "ok" || f["alerte_expiration"] != true {
		t.Errorf("ligne : %v", f)
	}
	var sante struct {
		Statut    string
		Controles []map[string]any
	}
	rec = b.requete("GET", "/api/v1/admin/sante", "", avec(admin))
	if err := json.Unmarshal(rec.Body.Bytes(), &sante); err != nil || rec.Code != 200 || sante.Statut != "ok" {
		t.Fatal(err, rec.Code, rec.Body)
	}
	if reel := trie(sante.Controles[0]); strings.Join(reel, ",") != strings.Join(cles("SanteApprofondie", "controles"), ",") {
		t.Errorf("champs reels %v, contrat %v", reel, cles("SanteApprofondie", "controles"))
	}
	refuse = true
	if rec := b.requete("GET", "/api/v1/admin/sante", "", avec(admin)); rec.Code != http.StatusServiceUnavailable || !strings.Contains(rec.Body.String(), `"degrade"`) ||
		!strings.Contains(rec.Body.String(), "fournisseur_compte_invalide") {
		t.Errorf("compte refuse : %d %s", rec.Code, rec.Body)
	}
	// Schema Erreur : code, message, expiration (et rien d'autre).
	if got := strings.Join(cles("Erreur"), ","); got != "code,expiration,message" {
		t.Errorf("schema Erreur : %s", got)
	}
}

func trie[V any](m map[string]V) []string {
	var k []string
	for c := range m {
		k = append(k, c)
	}
	sort.Strings(k)
	return k
}
