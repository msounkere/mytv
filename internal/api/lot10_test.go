package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"mytv/internal/domain"
)

// Lot 10, US-44 : contrat suppose par l'interface (raison, position, reessai, budget,
// reessayer, 422 favoris).

type aVoirSortie struct {
	Elements []struct {
		Contenu, Etat, Raison string
		Position              int
		Reessai               *string
		Episodes              []struct {
			Contenu, Etat, Raison string
			Position              int
			Reessai               *string
		}
	}
	Budget struct{ Utilise, Total int64 }
}

// CA1 (contrat) + CA2 (contrat) + CA4 : GET /profil/a_voir expose, pour un film comme pour chaque
// episode, `raison` et `position` d'une attente, `reessai` (RFC 3339) d'un echec transitoire,
// rien de tout cela pour un fichier pret ou un echec permanent ; l'element porte la raison et
// l'echeance de son premier episode ; `budget` = { utilise, total }.
func TestCA1_RaisonPositionReessaiEtBudgetDansLaListe(t *testing.T) {
	echeance := time.Date(2026, 11, 4, 21, 10, 0, 0, time.FixedZone("CET", 3600))
	prep := &preparationFausse{etats: map[domain.AVoirID]domain.EtatAVoirDetaille{
		"vod:a:1": {Etat: domain.PreparationAttente, Fichiers: []domain.FichierDetaille{
			{FichierPrepare: domain.FichierPrepare{Contenu: "a:1", Etat: domain.PreparationAttente}, Raison: domain.RaisonFile, Position: 2}}},
		"vod:a:2": {Etat: domain.PreparationEchec, Fichiers: []domain.FichierDetaille{
			{FichierPrepare: domain.FichierPrepare{Contenu: "a:2", Etat: domain.PreparationEchec, Cause: "HTTP 458"}, Reessai: echeance}}},
		"serie:a:s7": {Etat: domain.PreparationAttente, Fichiers: []domain.FichierDetaille{
			{FichierPrepare: domain.FichierPrepare{Contenu: "a:9.mkv", Etat: domain.PreparationAttente, Saison: 1, Episode: 1, Serie: "a:s7"}, Raison: domain.RaisonLectureEnCours},
			{FichierPrepare: domain.FichierPrepare{Contenu: "a:10.mkv", Etat: domain.PreparationPrete, Octets: 100, Total: 100, Saison: 1, Episode: 2, Serie: "a:s7"}},
			{FichierPrepare: domain.FichierPrepare{Contenu: "a:11.mkv", Etat: domain.PreparationEchec, Cause: "introuvable (404)", Saison: 1, Episode: 3, Serie: "a:s7"}}}},
	}}
	b := nouveauBanc(t, func(o *Options) { o.AVoir = &aVoirFaux{}; o.Preparation = prep })
	b.st.films = []domain.Film{{ID: "a:1", FournisseurID: "a", Numero: "1"}, {ID: "a:2", FournisseurID: "a", Numero: "2"}}
	b.st.series = []domain.SerieTV{{ID: "a:s7", FournisseurID: "a", Numero: "7"}}
	alice := b.connecte("alice", false)
	for _, id := range []string{"vod:a:1", "vod:a:2", "serie:a:s7"} {
		if rec := b.requete("PUT", "/api/v1/profil/a_voir/"+id, "", avec(alice)); rec.Code != 204 {
			t.Fatalf("ajout %s : %d %s", id, rec.Code, rec.Body)
		}
	}
	rec := b.requete("GET", "/api/v1/profil/a_voir", "", avec(alice))
	if rec.Code != 200 {
		t.Fatalf("GET : %d %s", rec.Code, rec.Body)
	}
	var out aVoirSortie
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || len(out.Elements) != 3 {
		t.Fatalf("liste : %v %s", err, rec.Body)
	}
	if e := out.Elements[0]; e.Etat != "attente" || e.Raison != "file" || e.Position != 2 || e.Reessai != nil {
		t.Errorf("film en file : %+v", e)
	}
	if e := out.Elements[1]; e.Etat != "echec" || e.Raison != "" || e.Position != 0 || e.Reessai == nil || *e.Reessai != "2026-11-04T20:10:00Z" {
		t.Errorf("echec transitoire : %+v (reessai attendu en UTC RFC 3339)", e)
	}
	s := out.Elements[2]
	if s.Etat != "attente" || s.Raison != "lecture_en_cours" || s.Position != 0 || len(s.Episodes) != 3 {
		t.Fatalf("serie : %+v", s)
	}
	if ep := s.Episodes[2]; ep.Etat != "echec" || ep.Reessai != nil || strings.Contains(rec.Body.String(), `"reessai":null`) {
		t.Errorf("echec permanent : %+v", ep)
	}
	if ep := s.Episodes[0]; ep.Raison != "lecture_en_cours" || ep.Etat != "attente" {
		t.Errorf("episode en attente : %+v", ep)
	}
	if ep := s.Episodes[1]; ep.Raison != "" || ep.Position != 0 || ep.Reessai != nil || ep.Etat != "pret" {
		t.Errorf("episode pret : %+v", ep)
	}
	if out.Budget.Utilise != 12345 || out.Budget.Total != 10<<30 {
		t.Errorf("budget : %+v", out.Budget)
	}
	// Sans moteur : budget nul, jamais absent (contrat).
	b2 := nouveauBanc(t, func(o *Options) { o.AVoir = &aVoirFaux{} })
	if rec := b2.requete("GET", "/api/v1/profil/a_voir", "", avec(b2.connecte("carl", false))); rec.Code != 200 || !strings.Contains(rec.Body.String(), `"budget":{"utilise":0,"total":0}`) {
		t.Errorf("sans moteur : %d %s", rec.Code, rec.Body)
	}
}

// CA3 : POST /profil/a_voir/{id}/reessayer (Session, profil actif) : 204 et le moteur est
// sollicite pour cet element ; idempotent ; 404 si l'element n'est pas dans la liste ; 400
// pour un identifiant invalide ; chaque profil ne vise que sa liste.
func TestCA3_RouteReessayer(t *testing.T) {
	prep := &preparationFausse{}
	b := nouveauBanc(t, func(o *Options) { o.AVoir = &aVoirFaux{}; o.Preparation = prep })
	b.st.films = []domain.Film{{ID: "a:1", FournisseurID: "a", Numero: "1"}}
	b.st.series = []domain.SerieTV{{ID: "a:s7", FournisseurID: "a", Numero: "7"}}
	alice, bob := b.connecte("alice", false), b.connecte("bob", false)
	b.requete("PUT", "/api/v1/profil/a_voir/vod:a:1", "", avec(alice))
	b.requete("PUT", "/api/v1/profil/a_voir/serie:a:s7", "", avec(alice))
	for _, c := range []struct {
		id   string
		qui  func(*http.Request)
		code int
	}{
		{"vod:a:1", avec(alice), 204}, {"vod:a:1", avec(alice), 204}, {"serie:a:s7", avec(alice), 204},
		{"vod:a:999", avec(alice), 404}, {"vod:a:1", avec(bob), 404}, {"n'importe:quoi", avec(alice), 400}, {"vod:a:9.mkv", avec(alice), 400},
	} {
		if rec := b.requete("POST", "/api/v1/profil/a_voir/"+c.id+"/reessayer", "", c.qui); rec.Code != c.code {
			t.Errorf("POST %s : %d (attendu %d) %s", c.id, rec.Code, c.code, rec.Body)
		}
	}
	if rec := b.requete("POST", "/api/v1/profil/a_voir/vod:a:1/reessayer", ""); rec.Code != 401 {
		t.Errorf("sans session : %d", rec.Code)
	}
	prep.mu.Lock()
	reessais := append([]string(nil), prep.reessais...)
	prep.mu.Unlock()
	if len(reessais) != 3 || !strings.HasSuffix(reessais[0], " vod:a:1") || !strings.HasSuffix(reessais[2], " serie:a:s7") {
		t.Errorf("appels au moteur : %v", reessais)
	}
	// Sans moteur : 204 quand l'element est la (rien a faire), 404 sinon.
	b2 := nouveauBanc(t, func(o *Options) { o.AVoir = &aVoirFaux{} })
	b2.st.films = []domain.Film{{ID: "a:1", FournisseurID: "a", Numero: "1"}}
	carl := b2.connecte("carl", false)
	b2.requete("PUT", "/api/v1/profil/a_voir/vod:a:1", "", avec(carl))
	if rec := b2.requete("POST", "/api/v1/profil/a_voir/vod:a:1/reessayer", "", avec(carl)); rec.Code != 204 {
		t.Errorf("sans moteur : %d", rec.Code)
	}
}

// CA6 (API) : PUT /profil/favoris/{id} refuse un film, une serie et un episode
// (422 `favori_chaines_seulement`) ; une chaine reste acceptee ; le retrait d'un ancien favori
// de film reste possible (nettoyage, idempotent).
func TestCA6_FavorisReservesAuxChaines(t *testing.T) {
	b := nouveauBanc(t, nil)
	b.st.chaines = []domain.Chaine{{ID: "a:100", FournisseurID: "a", Numero: "100", Nom: "Chaine"}}
	b.st.films = []domain.Film{{ID: "a:1", FournisseurID: "a", Numero: "1"}}
	b.st.series = []domain.SerieTV{{ID: "a:s7", FournisseurID: "a", Numero: "7"}}
	alice := b.connecte("alice", false)
	if rec := b.requete("PUT", "/api/v1/profil/favoris/a:100", "", avec(alice)); rec.Code != 204 {
		t.Fatalf("chaine : %d %s", rec.Code, rec.Body)
	}
	for _, id := range []string{"a:1", "a:s7", "a:9.mkv"} {
		rec := b.requete("PUT", "/api/v1/profil/favoris/"+id, "", avec(alice))
		if rec.Code != 422 || !strings.Contains(rec.Body.String(), `"code":"favori_chaines_seulement"`) {
			t.Errorf("favori %s : %d %s", id, rec.Code, rec.Body)
		}
	}
	var p pageEtIDs
	rec := b.requete("GET", "/api/v1/profil/favoris", "", avec(alice))
	json.Unmarshal(rec.Body.Bytes(), &p)
	if len(p.Elements) != 1 || p.Elements[0] != "a:100" {
		t.Errorf("favoris : %s", rec.Body)
	}
	if rec := b.requete("DELETE", "/api/v1/profil/favoris/a:1", "", avec(alice)); rec.Code != 204 {
		t.Errorf("retrait d'un film : %d", rec.Code)
	}
}

// CA8 : `GET /profil/a_voir` donne `episodes_avance_defaut` (le reglage du serveur) ; l'option
// `episodes_avance` d'un element reste 0 tant que l'utilisateur n'a rien choisi, et vaut ce
// qu'il a choisi ensuite ; 0 sans moteur.
func TestCA8_EpisodesAvanceDefautEtOptionDeLElement(t *testing.T) {
	b := nouveauBanc(t, func(o *Options) { o.AVoir = &aVoirFaux{}; o.Preparation = &preparationFausse{} })
	b.st.series = []domain.SerieTV{{ID: "a:s7", FournisseurID: "a", Numero: "7"}}
	alice := b.connecte("alice", false)
	var out struct {
		Elements []struct {
			EpisodesAvance int `json:"episodes_avance"`
		}
		EpisodesAvanceDefaut int `json:"episodes_avance_defaut"`
	}
	lire := func() {
		t.Helper()
		rec := b.requete("GET", "/api/v1/profil/a_voir", "", avec(alice))
		if rec.Code != 200 || json.Unmarshal(rec.Body.Bytes(), &out) != nil {
			t.Fatalf("GET : %d %s", rec.Code, rec.Body)
		}
	}
	if rec := b.requete("PUT", "/api/v1/profil/a_voir/serie:a:s7", "", avec(alice)); rec.Code != 204 {
		t.Fatalf("ajout : %d %s", rec.Code, rec.Body)
	}
	lire()
	if out.EpisodesAvanceDefaut != 2 || len(out.Elements) != 1 || out.Elements[0].EpisodesAvance != 0 {
		t.Errorf("sans option : defaut %d, element %+v", out.EpisodesAvanceDefaut, out.Elements)
	}
	if rec := b.requete("PUT", "/api/v1/profil/a_voir/serie:a:s7", `{"episodes_avance":3}`, avec(alice)); rec.Code != 204 {
		t.Fatalf("option : %d %s", rec.Code, rec.Body)
	}
	lire()
	if out.EpisodesAvanceDefaut != 2 || out.Elements[0].EpisodesAvance != 3 {
		t.Errorf("apres l'option : defaut %d, element %+v", out.EpisodesAvanceDefaut, out.Elements)
	}
	b2 := nouveauBanc(t, func(o *Options) { o.AVoir = &aVoirFaux{} })
	if rec := b2.requete("GET", "/api/v1/profil/a_voir", "", avec(b2.connecte("carl", false))); !strings.Contains(rec.Body.String(), `"episodes_avance_defaut":0`) {
		t.Errorf("sans moteur : %s", rec.Body)
	}
}
