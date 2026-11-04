package api

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"mytv/internal/domain"
)

// ---------------------------------------------------------------- doublures

type aVoirFaux struct {
	mu     sync.Mutex
	listes map[string][]domain.ElementAVoir
}

func (l *aVoirFaux) AVoir(_ context.Context, p string) ([]domain.ElementAVoir, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]domain.ElementAVoir(nil), l.listes[p]...), nil
}
func (l *aVoirFaux) AjouterAVoir(_ context.Context, p string, e domain.ElementAVoir) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.listes == nil {
		l.listes = map[string][]domain.ElementAVoir{}
	}
	e.Ajoute = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	for i, x := range l.listes[p] {
		if x.ID == e.ID {
			e.Rang, e.Ajoute = x.Rang, x.Ajoute
			l.listes[p][i] = e
			return nil
		}
	}
	if len(l.listes[p]) >= 3 {
		return domain.ErrLimite
	}
	e.Rang = len(l.listes[p])
	l.listes[p] = append(l.listes[p], e)
	return nil
}
func (l *aVoirFaux) RetirerAVoir(_ context.Context, p string, id domain.AVoirID) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	var reste []domain.ElementAVoir
	for _, x := range l.listes[p] {
		if x.ID != id {
			x.Rang = len(reste)
			reste = append(reste, x)
		}
	}
	l.listes[p] = reste
	return nil
}
func (l *aVoirFaux) OrdonnerAVoir(_ context.Context, p string, ids []domain.AVoirID) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	var ordre []domain.ElementAVoir
	for _, id := range ids {
		for _, x := range l.listes[p] {
			if x.ID == id {
				ordre = append(ordre, x)
			}
		}
	}
	for _, x := range l.listes[p] {
		deja := false
		for _, y := range ordre {
			deja = deja || y.ID == x.ID
		}
		if !deja {
			ordre = append(ordre, x)
		}
	}
	for i := range ordre {
		ordre[i].Rang = i
	}
	l.listes[p] = ordre
	return nil
}
func (l *aVoirFaux) ProfilsAVoir(context.Context) ([]string, error) { return nil, nil }

type preparationFausse struct {
	mu       sync.Mutex
	etats    map[domain.AVoirID]domain.EtatAVoirDetaille
	fichiers []domain.FichierDetaille
	reveils  int
	vides    int
	reessais []string
}

func (p *preparationFausse) EtatProfil(context.Context, string) (map[domain.AVoirID]domain.EtatAVoirDetaille, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.etats, nil
}
func (p *preparationFausse) Reveiller() { p.mu.Lock(); p.reveils++; p.mu.Unlock() }
func (p *preparationFausse) Reessayer(_ context.Context, profil string, id domain.AVoirID) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.reessais = append(p.reessais, profil+" "+string(id))
	return nil
}
func (p *preparationFausse) Fichiers() []domain.FichierDetaille { return p.fichiers }
func (p *preparationFausse) Occupe() int64                      { return 12345 }
func (p *preparationFausse) Budget() int64                      { return 10 << 30 }
func (p *preparationFausse) EpisodesAvanceDefaut() int          { return 2 }
func (p *preparationFausse) Vider() int {
	p.mu.Lock()
	p.vides++
	p.mu.Unlock()
	return len(p.fichiers)
}

type sondeFausse struct {
	mu      sync.Mutex
	appels  []string
	verdict domain.Disponibilite
}

func (s *sondeFausse) Disponibilite(_ context.Context, session string, id, serie domain.ContenuID) (domain.Disponibilite, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == "a:404" {
		return domain.Disponibilite{}, domain.ErrNonTrouve
	}
	s.appels = append(s.appels, session+" "+string(id)+" "+string(serie))
	return s.verdict, nil
}

// ---------------------------------------------------------------- CA1

// CA1 : routes de la liste (niveau Session, profil actif) : ajout idempotent en fin de liste
// avec options, lecture ordonnee avec l'etat de preparation et les episodes concernes,
// ordre, retrait, validation des identifiants, 404 pour un contenu inconnu, 422 a la limite ;
// un element termine sort de la liste a la lecture ; la preparation est reveillee.
func TestCA1_RoutesListeAVoir(t *testing.T) {
	liste := &aVoirFaux{}
	prep := &preparationFausse{etats: map[domain.AVoirID]domain.EtatAVoirDetaille{
		"serie:a:s7": {Etat: domain.PreparationPrete, Fichiers: []domain.FichierDetaille{
			{FichierPrepare: domain.FichierPrepare{Contenu: "a:9.mkv", Etat: domain.PreparationPrete, Octets: 100, Total: 100, Saison: 1, Episode: 1, Serie: "a:s7"}},
			{FichierPrepare: domain.FichierPrepare{Contenu: "a:10.mkv", Etat: domain.PreparationEnCours, Octets: 40, Total: 100, DebitKbps: 2500, Saison: 1, Episode: 2, Serie: "a:s7"}}}},
		"vod:a:2": {Termine: true},
	}}
	b := nouveauBanc(t, func(o *Options) { o.AVoir = liste; o.Preparation = prep })
	b.st.films = []domain.Film{{ID: "a:1", FournisseurID: "a", Numero: "1"}, {ID: "a:2", FournisseurID: "a", Numero: "2"}, {ID: "a:3", FournisseurID: "a", Numero: "3"}}
	b.st.series = []domain.SerieTV{{ID: "a:s7", FournisseurID: "a", Numero: "7"}}
	alice, bob := b.connecte("alice", false), b.connecte("bob", false)

	if rec := b.requete("PUT", "/api/v1/profil/a_voir/vod:a:1", "", avec(alice)); rec.Code != 204 {
		t.Fatalf("ajout film : %d %s", rec.Code, rec.Body)
	}
	b.requete("PUT", "/api/v1/profil/a_voir/vod:a:1", "", avec(alice)) // idempotent
	if rec := b.requete("PUT", "/api/v1/profil/a_voir/serie:a:s7", `{"episodes_avance":3,"depuis":{"saison":1,"episode":2}}`, avec(alice)); rec.Code != 204 {
		t.Fatalf("ajout serie : %d %s", rec.Code, rec.Body)
	}
	if rec := b.requete("PUT", "/api/v1/profil/a_voir/vod:a:2", "", avec(alice)); rec.Code != 204 {
		t.Fatalf("ajout film 2 : %d %s", rec.Code, rec.Body)
	}
	// Refus : prefixe inconnu, episode, contenu absent du catalogue, options sur un film, limite.
	for _, c := range []struct {
		id, corps string
		code      int
	}{
		{"chaine:a:1", "", 400}, {"vod:a:9.mkv", "", 400}, {"vod:a:999", "", 404}, {"serie:a:s999", "", 404},
		{"vod:a:1", `{"episodes_avance":2}`, 400}, {"serie:a:s7", `{"episodes_avance":99}`, 400}, {"vod:a:3", "", 422},
	} {
		if rec := b.requete("PUT", "/api/v1/profil/a_voir/"+c.id, c.corps, avec(alice)); rec.Code != c.code {
			t.Errorf("PUT %s %s : %d (attendu %d) %s", c.id, c.corps, rec.Code, c.code, rec.Body)
		}
	}
	var out struct {
		Elements []struct {
			Contenu, Etat, Cause string
			Rang                 int
			EpisodesAvance       int `json:"episodes_avance"`
			Depuis               *struct{ Saison, Episode int }
			Octets, Total        *int64
			Episodes             []struct {
				Contenu, Etat   string
				Octets, Total   *int64
				DebitKbps       *int `json:"debit_kbps"`
				Saison, Episode int
			}
		}
	}
	rec := b.requete("GET", "/api/v1/profil/a_voir", "", avec(alice))
	if rec.Code != 200 {
		t.Fatalf("GET : %d %s", rec.Code, rec.Body)
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	// vod:a:2 est termine : retire a la lecture.
	if len(out.Elements) != 2 || out.Elements[0].Contenu != "vod:a:1" || out.Elements[1].Contenu != "serie:a:s7" {
		t.Fatalf("liste : %s", rec.Body)
	}
	if e := out.Elements[0]; e.Etat != "attente" || e.Rang != 1 || e.Episodes != nil || e.Octets != nil || strings.Contains(rec.Body.String(), `"episodes":[]`) || strings.Contains(rec.Body.String(), `"id"`) {
		t.Errorf("film : %+v (%s)", e, rec.Body)
	}
	s := out.Elements[1]
	if s.Rang != 2 || s.Etat != "pret" || s.EpisodesAvance != 3 || s.Depuis == nil || s.Depuis.Saison != 1 || s.Depuis.Episode != 2 || len(s.Episodes) != 2 {
		t.Errorf("serie : %+v", s)
	}
	if f := s.Episodes[1]; f.Contenu != "a:10.mkv" || f.Etat != "en_cours" || f.Octets == nil || *f.Octets != 40 || *f.Total != 100 || *f.DebitKbps != 2500 || f.Episode != 2 || f.Saison != 1 {
		t.Errorf("episode en cours : %+v", f)
	}
	liste.mu.Lock()
	for p, l := range liste.listes {
		if len(l) != 2 {
			t.Errorf("element termine non retire (%s) : %v", p, l)
		}
	}
	liste.mu.Unlock()
	// Ordre.
	if rec := b.requete("PUT", "/api/v1/profil/a_voir", `{"ids":["serie:a:s7","vod:a:inconnu"]}`, avec(alice)); rec.Code != 204 {
		t.Fatalf("ordre : %d %s", rec.Code, rec.Body)
	}
	rec = b.requete("GET", "/api/v1/profil/a_voir", "", avec(alice))
	json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Elements) != 2 || out.Elements[0].Contenu != "serie:a:s7" || out.Elements[0].Rang != 1 || out.Elements[1].Contenu != "vod:a:1" {
		t.Errorf("apres ordre : %s", rec.Body)
	}
	if rec := b.requete("PUT", "/api/v1/profil/a_voir", `{"ids":["n'importe quoi"]}`, avec(alice)); rec.Code != 400 {
		t.Errorf("ordre invalide : %d", rec.Code)
	}
	// Bob ne voit rien ; retrait idempotent ; la preparation a ete reveillee a chaque ecriture.
	if rec := b.requete("GET", "/api/v1/profil/a_voir", "", avec(bob)); !strings.Contains(rec.Body.String(), `"elements":[]`) {
		t.Errorf("bob : %s", rec.Body)
	}
	if rec := b.requete("DELETE", "/api/v1/profil/a_voir/vod:a:1", "", avec(alice)); rec.Code != 204 {
		t.Errorf("retrait : %d", rec.Code)
	}
	if rec := b.requete("DELETE", "/api/v1/profil/a_voir/vod:a:1", "", avec(alice)); rec.Code != 204 {
		t.Errorf("retrait idempotent : %d", rec.Code)
	}
	if rec := b.requete("DELETE", "/api/v1/profil/a_voir/x", "", avec(alice)); rec.Code != 400 {
		t.Errorf("retrait invalide : %d", rec.Code)
	}
	prep.mu.Lock()
	reveils := prep.reveils
	prep.mu.Unlock()
	if reveils < 5 {
		t.Errorf("preparation reveillee %d fois", reveils)
	}
	// Sans liste branchee : 501.
	b2 := nouveauBanc(t, nil)
	if rec := b2.requete("GET", "/api/v1/profil/a_voir", "", avec(b2.connecte("carl", false))); rec.Code != 501 {
		t.Errorf("sans liste : %d", rec.Code)
	}
}

// CA1 : enregistrer une lecture reveille la preparation (un episode vu appelle le suivant).
func TestCA1_LectureEnregistreeReveilleLaPreparation(t *testing.T) {
	prep := &preparationFausse{}
	b := nouveauBanc(t, func(o *Options) { o.AVoir = &aVoirFaux{}; o.Preparation = prep })
	alice := b.connecte("alice", false)
	if rec := b.requete("PUT", "/api/v1/profil/historique/a:1", `{"position":96,"duree":100}`, avec(alice)); rec.Code != 204 {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	prep.mu.Lock()
	defer prep.mu.Unlock()
	if prep.reveils != 1 {
		t.Errorf("reveils = %d", prep.reveils)
	}
}

// ---------------------------------------------------------------- CA6 / CA7

// CA6 : GET /vod/{id}/disponibilite (Session) rend le verdict de la sonde, avec la session
// qui demande et la serie facultative (validee) ; 404 pour un contenu inconnu ; 501 sans sonde.
func TestCA6_RouteDisponibilite(t *testing.T) {
	sonde := &sondeFausse{verdict: domain.Disponibilite{Etat: "lent", DebitKbps: 2700, DebitFichierKbps: 4400, Mesure: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}}
	b := nouveauBanc(t, func(o *Options) { o.Sonde = sonde })
	b.st.series = []domain.SerieTV{{ID: "a:s7", FournisseurID: "a", Numero: "7"}}
	alice := b.connecte("alice", false)
	rec := b.requete("GET", "/api/v1/vod/a:1/disponibilite", "", avec(alice))
	if rec.Code != 200 || rec.Body.String() != `{"debit_kbps":2700,"etat":"lent","mesure":"2026-01-01T12:00:00Z","requis_kbps":4400}`+"\n" {
		t.Fatalf("%d %s", rec.Code, rec.Body)
	}
	// Sans mesure (lecture en cours) : mesure nulle.
	sonde.mu.Lock()
	sonde.verdict = domain.Disponibilite{Etat: "inconnu", Mesure: time.Now()}
	sonde.mu.Unlock()
	if rec := b.requete("GET", "/api/v1/vod/a:1/disponibilite", "", avec(alice)); rec.Code != 200 || rec.Body.String() != `{"debit_kbps":0,"etat":"inconnu","mesure":null,"requis_kbps":0}`+"\n" {
		t.Errorf("inconnu : %d %s", rec.Code, rec.Body)
	}
	if rec := b.requete("GET", "/api/v1/vod/a:9.mkv/disponibilite?serie=a:s7", "", avec(alice)); rec.Code != 200 {
		t.Errorf("episode avec serie : %d %s", rec.Code, rec.Body)
	}
	if rec := b.requete("GET", "/api/v1/vod/a:9.mkv/disponibilite?serie=a:s999", "", avec(alice)); rec.Code != 400 {
		t.Errorf("serie inconnue : %d", rec.Code)
	}
	if rec := b.requete("GET", "/api/v1/vod/a:404/disponibilite", "", avec(alice)); rec.Code != 404 {
		t.Errorf("contenu inconnu : %d", rec.Code)
	}
	sonde.mu.Lock()
	appels := sonde.appels
	sonde.mu.Unlock()
	if len(appels) != 3 || !strings.HasSuffix(appels[0], " a:1 ") || !strings.HasSuffix(appels[2], " a:9.mkv a:s7") || strings.HasPrefix(appels[0], " ") {
		t.Errorf("appels = %v", appels)
	}
	b2 := nouveauBanc(t, nil)
	if rec := b2.requete("GET", "/api/v1/vod/a:1/disponibilite", "", avec(b2.connecte("carl", false))); rec.Code != 501 {
		t.Errorf("sans sonde : %d", rec.Code)
	}
}

// CA7 : /admin/preparation (Admin) liste les fichiers et leur etat ; DELETE les efface ;
// 403 pour un simple utilisateur ; 501 sans moteur.
func TestCA7_AdminPreparation(t *testing.T) {
	prep := &preparationFausse{fichiers: []domain.FichierDetaille{{FichierPrepare: domain.FichierPrepare{Contenu: "a:1", Etat: domain.PreparationPrete, Octets: 10, Total: 10}},
		{FichierPrepare: domain.FichierPrepare{Contenu: "a:9.mkv", Etat: domain.PreparationEchec, Cause: "fichier plus grand que le budget", Serie: "a:s7", Saison: 1, Episode: 1}}}}
	b := nouveauBanc(t, func(o *Options) { o.Preparation = prep })
	admin, alice := b.connecte("root", true), b.connecte("alice", false)
	rec := b.requete("GET", "/api/v1/admin/preparation", "", avec(admin))
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"occupe":12345`) || !strings.Contains(rec.Body.String(), `"cause":"fichier plus grand que le budget"`) {
		t.Errorf("GET : %d %s", rec.Code, rec.Body)
	}
	if rec := b.requete("GET", "/api/v1/admin/preparation", "", avec(alice)); rec.Code != 403 {
		t.Errorf("utilisateur : %d", rec.Code)
	}
	if rec := b.requete("DELETE", "/api/v1/admin/preparation", "", avec(admin)); rec.Code != 200 || rec.Body.String() != `{"effaces":2}`+"\n" {
		t.Errorf("DELETE : %d %s", rec.Code, rec.Body)
	}
	b2 := nouveauBanc(t, nil)
	if rec := b2.requete("GET", "/api/v1/admin/preparation", "", avec(b2.connecte("root2", true))); rec.Code != 501 {
		t.Errorf("sans moteur : %d", rec.Code)
	}
}
