package preparation

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mytv/internal/domain"
	"mytv/internal/testutil/fauxiptv"
)

// Lot 10, US-44 : preparation reparable et explicite.

// horlogeSimulee : une horloge que le test avance a la main (reculs de plusieurs heures).
type horlogeSimulee struct{ ns atomic.Int64 }

func nouvelleHorloge() *horlogeSimulee {
	h := &horlogeSimulee{}
	h.ns.Store(time.Date(2026, 11, 4, 20, 0, 0, 0, time.UTC).UnixNano())
	return h
}
func (h *horlogeSimulee) maintenant() time.Time   { return time.Unix(0, h.ns.Load()).UTC() }
func (h *horlogeSimulee) avancer(d time.Duration) { h.ns.Add(int64(d)) }

// journalMemoire capture le journal du moteur (verification des lignes INFO).
type journalMemoire struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (j *journalMemoire) Write(p []byte) (int, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.buf.Write(p)
}
func (j *journalMemoire) String() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.buf.String()
}

func detailDe(m *Moteur, id domain.ContenuID) domain.FichierDetaille {
	for _, f := range m.Fichiers() {
		if f.Contenu == id {
			return f
		}
	}
	return domain.FichierDetaille{}
}

// ---------------------------------------------------------------- CA1

// CA1 : chaque fichier en attente dit pourquoi : `lecture_en_cours` quand une place est tenue
// chez le fournisseur (aucune requete ne part), `file` + position 1-based dans la file
// globale (trois elements : le 2e et le 3e en file 2 et 3), `reprise_apres` juste apres la
// fin d'une lecture, `budget_plein` quand le fichier ne tient pas ; un fichier pret n'a
// pas de raison et la file se resserre.
func TestCA1_RaisonDUneAttente(t *testing.T) {
	occ := &occupationFixe{n: 1}
	b := nouveauBanc(t, fauxiptv.Options{}, func(r *Reglages) { r.RepriseApres = time.Hour }, occ)
	ctx := context.Background()
	b.liste.AjouterAVoir(ctx, "p1", domain.ElementAVoir{Contenu: "iptva:2001"})
	b.liste.AjouterAVoir(ctx, "p1", domain.ElementAVoir{Contenu: "iptva:2002"})
	b.liste.AjouterAVoir(ctx, "p1", domain.ElementAVoir{Contenu: "iptva:s77", Serie: true})
	b.faux.RazCompteurs()

	// Une lecture est en cours chez le fournisseur : tout attend pour cette raison.
	etats, err := b.m.EtatProfil(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []domain.AVoirID{"vod:iptva:2001", "vod:iptva:2002", "serie:iptva:s77"} {
		e := etats[id]
		if e.Etat != domain.PreparationAttente || len(e.Fichiers) == 0 {
			t.Fatalf("%s : %+v", id, e)
		}
		for _, f := range e.Fichiers {
			if f.Raison != domain.RaisonLectureEnCours || f.Position != 0 {
				t.Errorf("%s : raison pendant une lecture = %q position %d", id, f.Raison, f.Position)
			}
		}
	}
	if n := b.faux.Compteurs().Films; n != 0 {
		t.Errorf("%d requete(s) au fournisseur pendant une lecture", n)
	}

	// Plus de lecture : chacun attend son tour, dans l'ordre de la liste.
	occ.mu.Lock()
	occ.n = 0
	occ.mu.Unlock()
	etats, _ = b.m.EtatProfil(ctx, "p1")
	attendus := []struct {
		id  domain.AVoirID
		pos []int
	}{{"vod:iptva:2001", []int{1}}, {"vod:iptva:2002", []int{2}}, {"serie:iptva:s77", []int{3, 4}}}
	for _, a := range attendus {
		e := etats[a.id]
		if len(e.Fichiers) != len(a.pos) {
			t.Fatalf("%s : %+v", a.id, e)
		}
		for i, f := range e.Fichiers {
			if f.Raison != domain.RaisonFile || f.Position != a.pos[i] {
				t.Errorf("%s fichier %d : raison %q position %d (attendu file %d)", a.id, i, f.Raison, f.Position, a.pos[i])
			}
		}
	}

	// Une lecture vient de finir : reprise_apres (RepriseApres = 1 h).
	b.m.Veilleur()("iptva", 0)
	etats, _ = b.m.EtatProfil(ctx, "p1")
	if f := etats["vod:iptva:2002"].Fichiers[0]; f.Raison != domain.RaisonRepriseApres || f.Position != 0 {
		t.Errorf("apres une lecture : %+v", f)
	}

	// Le premier est pret : plus de raison pour lui, la file se resserre.
	b.m.r.RepriseApres = 0
	b.m.cycle(ctx)
	etats, _ = b.m.EtatProfil(ctx, "p1")
	if f := etats["vod:iptva:2001"].Fichiers[0]; f.Etat != domain.PreparationPrete || f.Raison != "" || f.Position != 0 {
		t.Errorf("pret : %+v", f)
	}
	if f := etats["vod:iptva:2002"].Fichiers[0]; f.Etat != domain.PreparationPrete {
		t.Errorf("2002 : %+v", f)
	}
	if e := etats["serie:iptva:s77"]; e.Fichiers[0].Etat != domain.PreparationPrete || e.Fichiers[1].Etat != domain.PreparationPrete {
		t.Errorf("serie : %+v", e)
	}

	// Budget plein : un nouveau film qui ne tient pas le dit.
	b.m.r.Budget = 4*tailleFilm + tailleFilm/2
	b.stockage.films["iptva:2003"] = domain.Film{ID: "iptva:2003", FournisseurID: "iptva", Numero: "2003", Extension: "mp4"}
	b.liste.AjouterAVoir(ctx, "p1", domain.ElementAVoir{Contenu: "iptva:2003"})
	b.m.cycle(ctx)
	etats, _ = b.m.EtatProfil(ctx, "p1")
	if f := etats["vod:iptva:2003"].Fichiers[0]; f.Etat != domain.PreparationAttente || f.Raison != domain.RaisonBudgetPlein || f.Position != 0 {
		t.Errorf("budget plein : %+v", f)
	}
}

// ---------------------------------------------------------------- CA2

// CA2 : un 458 (adresse refusee) est un echec TRANSITOIRE : etat `echec` avec `reessai`, le
// moteur ne retente qu'a l'echeance (recul 15 min, 30 min, 1 h, 2 h, 4 h, 4 h...), journal
// INFO a chaque nouvelle tentative ; le fichier finit pret ; jamais plus d'une connexion.
// Un 404 est un echec permanent : pas de `reessai`, jamais retente.
func TestCA2_EchecTransitoireRetenteAvecRecul(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil, nil)
	h := nouvelleHorloge()
	b.horloge = h.maintenant
	m := b.moteur(b.m.r)
	journal := &journalMemoire{}
	m.log = slog.New(slog.NewTextHandler(journal, nil))
	ctx := context.Background()
	b.liste.AjouterAVoir(ctx, "p1", domain.ElementAVoir{Contenu: "iptva:2001"})
	attendu := b.contenuAttendu("2001.mp4")
	b.faux.RazCompteurs()
	// Six refus d'affilee, puis le fichier : les reculs 15 min, 30 min, 1 h, 2 h, 4 h, 4 h.
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleFilm, Statut: 458, Fois: 6})
	reculsAttendus := []time.Duration{15 * time.Minute, 30 * time.Minute, time.Hour, 2 * time.Hour, 4 * time.Hour, 4 * time.Hour}
	for i, recul := range reculsAttendus {
		depart := h.maintenant()
		if d := m.cycle(ctx); d != recul {
			t.Errorf("tentative %d : le cycle demande a repasser dans %v (attendu %v)", i+1, d, recul)
		}
		f := detailDe(m, "iptva:2001")
		if f.Etat != domain.PreparationEchec || !f.Reessai.Equal(depart.Add(recul)) || f.Cause == "" {
			t.Fatalf("tentative %d : %+v (reessai attendu %v)", i+1, f, depart.Add(recul))
		}
		if strings.Contains(f.Cause, b.faux.MotDePasse()) || strings.Contains(f.Cause, "http") {
			t.Errorf("cause non assainie : %q", f.Cause)
		}
		if n := b.faux.Compteurs().Films; n != i+1 {
			t.Fatalf("tentative %d : %d requetes", i+1, n)
		}
		// Avant l'echeance : rien ne part, meme sur plusieurs cycles.
		h.avancer(recul - time.Minute)
		m.cycle(ctx)
		m.cycle(ctx)
		if n := b.faux.Compteurs().Films; n != i+1 {
			t.Fatalf("tentative %d : %d requetes avant l'echeance (recul non respecte)", i+1, n)
		}
		h.avancer(time.Minute)
	}
	// Echeance atteinte : nouvelle tentative, qui reussit.
	m.cycle(ctx)
	f := detailDe(m, "iptva:2001")
	if f.Etat != domain.PreparationPrete || !f.Reessai.IsZero() || f.Cause != "" {
		t.Fatalf("apres les reculs : %+v", f)
	}
	corps, _ := os.ReadFile(filepath.Join(b.dossier, "film-iptva-2001.mp4"))
	if !bytes.Equal(corps, attendu) {
		t.Error("fichier incorrect")
	}
	if c := b.faux.Compteurs(); c.Films != 7 || c.FilmsPic != 1 {
		t.Errorf("requetes = %d, pic = %d (attendu 7 et 1)", c.Films, c.FilmsPic)
	}
	if n := strings.Count(journal.String(), "nouvelle tentative apres un echec transitoire"); n != 6 {
		t.Errorf("%d ligne(s) INFO de nouvelle tentative (attendu 6)", n)
	}
	if b.instr.valeur("preparation_fichiers_total{etat=echec_transitoire}") != 6 {
		t.Errorf("metrique : %v", b.instr.compteurs)
	}

	// Permanent : 404, pas de reessai, jamais retente meme des heures plus tard.
	b.liste.AjouterAVoir(ctx, "p1", domain.ElementAVoir{Contenu: "iptva:2002"})
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleFilm, Statut: http.StatusNotFound, Fois: 1})
	if d := m.cycle(ctx); d != 0 {
		t.Errorf("echec permanent : le cycle demande a repasser dans %v", d)
	}
	f = detailDe(m, "iptva:2002")
	if f.Etat != domain.PreparationEchec || !f.Reessai.IsZero() {
		t.Fatalf("404 : %+v", f)
	}
	avant := b.faux.Compteurs().Films
	h.avancer(10 * time.Hour)
	m.cycle(ctx)
	if n := b.faux.Compteurs().Films; n != avant {
		t.Errorf("un echec permanent a ete retente (%d requetes)", n-avant)
	}
	// L'echeance est aussi exposee par l'etat de la liste (un 5xx qui persiste au-dela des
	// essais immediats de l'ouverture est transitoire lui aussi).
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleFilm, Statut: http.StatusBadGateway, Fois: essaisTransitoires + 1})
	b.stockage.films["iptva:2003"] = domain.Film{ID: "iptva:2003", FournisseurID: "iptva", Numero: "2003", Extension: "mp4"}
	b.liste.AjouterAVoir(ctx, "p1", domain.ElementAVoir{Contenu: "iptva:2003"})
	m.cycle(ctx)
	etats, _ := m.EtatProfil(ctx, "p1")
	if e := etats["vod:iptva:2003"]; e.Etat != domain.PreparationEchec || !e.Fichiers[0].Reessai.Equal(h.maintenant().Add(15*time.Minute)) {
		t.Errorf("502 dans l'etat de la liste : %+v", e)
	}
	if e := etats["vod:iptva:2002"]; e.Etat != domain.PreparationEchec || !e.Fichiers[0].Reessai.IsZero() {
		t.Errorf("404 dans l'etat de la liste : %+v", e)
	}
}

// CA2 : une coupure repetee (au-dela des reprises Range d'un cycle) est transitoire : le
// `.partiel` est conserve et repris par Range a la tentative suivante.
func TestCA2_CoupuresRepeteesGardentLePartiel(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil, nil)
	h := nouvelleHorloge()
	b.horloge = h.maintenant
	m := b.moteur(b.m.r)
	ctx := context.Background()
	b.liste.AjouterAVoir(ctx, "p1", domain.ElementAVoir{Contenu: "iptva:2001"})
	attendu := b.contenuAttendu("2001.mp4")
	b.faux.RazCompteurs()
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleFilm, Coupure: true, Fois: reprisesMax + 1})
	m.cycle(ctx)
	f := detailDe(m, "iptva:2001")
	if f.Etat != domain.PreparationEchec || f.Reessai.IsZero() || f.Octets == 0 {
		t.Fatalf("coupures repetees : %+v", f)
	}
	if _, err := os.Stat(filepath.Join(b.dossier, "film-iptva-2001.mp4.partiel")); err != nil {
		t.Fatal("le .partiel a ete efface par un echec transitoire")
	}
	h.avancer(15 * time.Minute)
	m.cycle(ctx)
	corps, _ := os.ReadFile(filepath.Join(b.dossier, "film-iptva-2001.mp4"))
	if !bytes.Equal(corps, attendu) || detailDe(m, "iptva:2001").Etat != domain.PreparationPrete {
		t.Error("fichier non repris apres l'echeance")
	}
	if vus := rangesVus(b.faux); len(vus) < 2 || vus[len(vus)-1] == "" || vus[len(vus)-1] == "bytes=0-" {
		t.Errorf("la reprise ne part pas du partiel : %v", vus)
	}
}

// ---------------------------------------------------------------- CA3

// CA3 : Reessayer remet en attente, tout de suite, les fichiers en echec d'un element
// (serie : ses episodes planifies) et efface leur echeance ; idempotent ; les autres
// elements gardent leur recul.
func TestCA3_ReessayerRemetEnAttenteSansRecul(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil, nil)
	h := nouvelleHorloge()
	b.horloge = h.maintenant
	m := b.moteur(b.m.r)
	ctx := context.Background()
	b.liste.AjouterAVoir(ctx, "p1", domain.ElementAVoir{Contenu: "iptva:2001"})
	b.liste.AjouterAVoir(ctx, "p1", domain.ElementAVoir{Contenu: "iptva:s77", Serie: true})
	b.faux.RazCompteurs()
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleFilm, Statut: 458, Fois: 3})
	m.cycle(ctx)
	for _, id := range []domain.ContenuID{"iptva:2001", episode(1), episode(2)} {
		if f := detailDe(m, id); f.Etat != domain.PreparationEchec || f.Reessai.IsZero() {
			t.Fatalf("%s : %+v", id, f)
		}
	}
	if err := m.Reessayer(ctx, "p1", "serie:iptva:s77"); err != nil {
		t.Fatal(err)
	}
	if err := m.Reessayer(ctx, "p1", "serie:iptva:s77"); err != nil { // idempotent
		t.Fatal(err)
	}
	if err := m.Reessayer(ctx, "p1", "vod:iptva:9999"); err != nil { // inconnu : rien a faire
		t.Fatal(err)
	}
	for _, id := range []domain.ContenuID{episode(1), episode(2)} {
		if f := detailDe(m, id); f.Etat != domain.PreparationAttente || !f.Reessai.IsZero() || f.Cause != "" {
			t.Errorf("%s apres Reessayer : %+v", id, f)
		}
	}
	if f := detailDe(m, "iptva:2001"); f.Etat != domain.PreparationEchec || f.Reessai.IsZero() {
		t.Errorf("le film a perdu son recul : %+v", f)
	}
	// Sans avancer l'horloge : les episodes partent, le film attend toujours son echeance.
	m.cycle(ctx)
	if detailDe(m, episode(1)).Etat != domain.PreparationPrete || detailDe(m, episode(2)).Etat != domain.PreparationPrete {
		t.Errorf("episodes non retentes : %+v", m.Fichiers())
	}
	if f := detailDe(m, "iptva:2001"); f.Etat != domain.PreparationEchec {
		t.Errorf("film retente avant l'echeance : %+v", f)
	}
	if n := b.faux.Compteurs().Films; n != 5 {
		t.Errorf("%d requetes (attendu 3 + 2)", n)
	}
	m.Reessayer(ctx, "p1", "vod:iptva:2001")
	m.cycle(ctx)
	if f := detailDe(m, "iptva:2001"); f.Etat != domain.PreparationPrete {
		t.Errorf("film : %+v", f)
	}
}

// ---------------------------------------------------------------- CA4

// CA4 : budget visible : `utilise` = octets des fichiers prets et partiels (tous profils),
// `total` = preparation.budget.
func TestCA4_BudgetUtiliseEtTotal(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil, nil)
	ctx := context.Background()
	b.liste.AjouterAVoir(ctx, "p1", domain.ElementAVoir{Contenu: "iptva:2001"})
	b.liste.AjouterAVoir(ctx, "p2", domain.ElementAVoir{Contenu: "iptva:2002"})
	if b.m.Occupe() != 0 || b.m.Budget() != 100*tailleFilm {
		t.Fatalf("avant : utilise %d total %d", b.m.Occupe(), b.m.Budget())
	}
	b.m.cycle(ctx)
	if !b.m.EstPret("iptva:2001") || !b.m.EstPret("iptva:2002") {
		t.Fatalf("fichiers : %+v", b.m.Fichiers())
	}
	if b.m.Occupe() != 2*tailleFilm {
		t.Errorf("utilise = %d (attendu la somme %d)", b.m.Occupe(), 2*tailleFilm)
	}
}

// ---------------------------------------------------------------- CA5

// CA5 : seuil « vu » unique domain.PartVue = 0,9 : un episode a 92 % est vu et le suivant
// est planifie ; un film a 92 % est termine ; 89 % n'est pas vu.
func TestCA5_SeuilVuUnique(t *testing.T) {
	if domain.PartVue != 0.9 {
		t.Fatalf("PartVue = %v", domain.PartVue)
	}
	b := nouveauBanc(t, fauxiptv.Options{}, nil, nil)
	ctx := context.Background()
	b.liste.AjouterAVoir(ctx, "p1", domain.ElementAVoir{Contenu: "iptva:s77", Serie: true})
	b.liste.AjouterAVoir(ctx, "p1", domain.ElementAVoir{Contenu: "iptva:2001"})
	b.etat.lecture("p1", episode(1), "iptva:s77", 92, 100)
	b.etat.lecture("p1", "iptva:2001", "", 92, 100)
	etats, err := b.m.EtatProfil(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if got := contenus(etats["serie:iptva:s77"]); !egal(got, []string{"iptva:7702.mp4", "iptva:7703.mp4"}) {
		t.Errorf("episode a 92 %% non considere vu : %v", got)
	}
	if !etats["vod:iptva:2001"].Termine {
		t.Errorf("film a 92 %% non termine : %+v", etats["vod:iptva:2001"])
	}
	b.etat.lecture("p1", episode(1), "iptva:s77", 89, 100)
	b.etat.lecture("p1", "iptva:2001", "", 89, 100)
	etats, _ = b.m.EtatProfil(ctx, "p1")
	if got := contenus(etats["serie:iptva:s77"]); !egal(got, []string{"iptva:7701.mp4", "iptva:7702.mp4"}) {
		t.Errorf("episode a 89 %% considere vu : %v", got)
	}
	if etats["vod:iptva:2001"].Termine {
		t.Error("film a 89 % termine")
	}
}
