package preparation

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mytv/internal/domain"
	"mytv/internal/testutil/fauxiptv"
)

// ---------------------------------------------------------------- CA2 : planification

// CA2 : un film = son fichier ; un film vu (part >= 0,95) est termine ; une serie = ses
// episodes_avance prochains episodes NON VUS a partir de l'episode entame (apres lui s'il
// est vu), ou de `depuis` ; une serie entierement vue est terminee ; l'option par element
// l'emporte sur le defaut.
func TestCA2_PlanificationFilmsEtSeries(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil, nil)
	ctx := context.Background()
	b.liste.AjouterAVoir(ctx, "p1", domain.ElementAVoir{Contenu: "iptva:2001"})
	b.liste.AjouterAVoir(ctx, "p1", domain.ElementAVoir{Contenu: "iptva:2002"})
	b.liste.AjouterAVoir(ctx, "p1", domain.ElementAVoir{Contenu: "iptva:s77", Serie: true})
	b.etat.vu("p1", "iptva:2002", "")
	// Episode 2 entame (non vu) : on veut 2 et 3.
	b.etat.lecture("p1", episode(2), "iptva:s77", 10, 100)
	etats, err := b.m.EtatProfil(ctx, "p1")
	if err != nil {
		t.Fatal(err)
	}
	if e := etats["vod:iptva:2001"]; e.Termine || len(e.Fichiers) != 1 || e.Fichiers[0].Contenu != "iptva:2001" || e.Etat != domain.PreparationAttente {
		t.Errorf("film : %+v", e)
	}
	if e := etats["vod:iptva:2002"]; !e.Termine {
		t.Errorf("film vu non termine : %+v", e)
	}
	if got := contenus(etats["serie:iptva:s77"]); !egal(got, []string{"iptva:7702.mp4", "iptva:7703.mp4"}) {
		t.Errorf("serie entamee : %v", got)
	}
	if f := etats["serie:iptva:s77"].Fichiers[0]; f.Serie != "iptva:s77" || f.Saison != 1 || f.Episode != 2 {
		t.Errorf("episode sans ses numeros : %+v", f)
	}
	// Episode 2 vu : on veut 3 et 4 ; episode 3 aussi vu : 4 et 5.
	b.etat.vu("p1", episode(2), "iptva:s77")
	etats, _ = b.m.EtatProfil(ctx, "p1")
	if got := contenus(etats["serie:iptva:s77"]); !egal(got, []string{"iptva:7703.mp4", "iptva:7704.mp4"}) {
		t.Errorf("apres episode 2 vu : %v", got)
	}
	b.etat.vu("p1", episode(3), "iptva:s77")
	etats, _ = b.m.EtatProfil(ctx, "p1")
	if got := contenus(etats["serie:iptva:s77"]); !egal(got, []string{"iptva:7704.mp4", "iptva:7705.mp4"}) {
		t.Errorf("apres episode 3 vu : %v", got)
	}
	// `depuis` S01E05 et 3 episodes d'avance : seul le 5 reste.
	b.liste.AjouterAVoir(ctx, "p1", domain.ElementAVoir{Contenu: "iptva:s77", Serie: true, EpisodesAvance: 3, DepuisSaison: 1, DepuisEpisode: 5})
	etats, _ = b.m.EtatProfil(ctx, "p1")
	if got := contenus(etats["serie:iptva:s77"]); !egal(got, []string{"iptva:7705.mp4"}) {
		t.Errorf("depuis S01E05 : %v", got)
	}
	// `depuis` S01E01, 1 d'avance : le premier non vu est le 1.
	b.liste.AjouterAVoir(ctx, "p1", domain.ElementAVoir{Contenu: "iptva:s77", Serie: true, EpisodesAvance: 1, DepuisSaison: 1, DepuisEpisode: 1})
	etats, _ = b.m.EtatProfil(ctx, "p1")
	if got := contenus(etats["serie:iptva:s77"]); !egal(got, []string{"iptva:7701.mp4"}) {
		t.Errorf("depuis S01E01 : %v", got)
	}
	// Tous vus : termine.
	for i := 1; i <= 5; i++ {
		b.etat.vu("p1", episode(i), "iptva:s77")
	}
	etats, _ = b.m.EtatProfil(ctx, "p1")
	if e := etats["serie:iptva:s77"]; !e.Termine || len(e.Fichiers) != 0 {
		t.Errorf("serie vue non terminee : %+v", e)
	}
	// Le plan global suit l'ordre de la liste et ne telecharge qu'une fois un fichier voulu par deux profils.
	b.liste.AjouterAVoir(ctx, "p2", domain.ElementAVoir{Contenu: "iptva:2001"})
	p, err := b.m.planifier(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.voulus) != 1 || p.voulus[0].contenu != "iptva:2001" || !p.vus["iptva:2002"] {
		t.Errorf("plan global : %+v", p)
	}
}

func contenus(e domain.EtatAVoir) (r []string) {
	for _, f := range e.Fichiers {
		r = append(r, string(f.Contenu))
	}
	return
}

func egal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- CA2 : telechargement

// CA2 : telechargement par le client du fournisseur, `.partiel` pendant l'ecriture, reprise
// par Range apres une coupure, taille verifiee, nom derive de l'identifiant, etat pret,
// metriques, index persistant ; aucun identifiant dans les noms de fichiers.
func TestCA2_TelechargementRepriseEtFichierPret(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil, nil)
	ctx := context.Background()
	b.liste.AjouterAVoir(ctx, "p1", domain.ElementAVoir{Contenu: "iptva:2001"})
	attendu := b.contenuAttendu("2001.mp4")
	b.faux.RazCompteurs()
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleFilm, Coupure: true, Fois: 1})
	b.m.cycle(ctx)

	chemin := filepath.Join(b.dossier, "film-iptva-2001.mp4")
	corps, err := os.ReadFile(chemin)
	if err != nil {
		t.Fatalf("fichier pret absent : %v (dossier : %v)", err, noms(b.dossier))
	}
	if !bytes.Equal(corps, attendu) {
		t.Error("contenu different apres reprise")
	}
	if _, err := os.Stat(chemin + ".partiel"); err == nil {
		t.Error("le .partiel subsiste apres le renommage")
	}
	for _, n := range noms(b.dossier) {
		if strings.Contains(n, b.faux.Utilisateur()) || strings.Contains(n, b.faux.MotDePasse()) {
			t.Errorf("identifiant dans un nom de fichier : %s", n)
		}
	}
	vus := rangesVus(b.faux)
	if len(vus) != 2 || vus[0] != "" || vus[1] != "bytes=150000-" {
		t.Errorf("Range amont = %v (attendu une reprise a 150000)", vus)
	}
	f := b.m.Fichiers()
	if len(f) != 1 || f[0].Etat != domain.PreparationPrete || f[0].Octets != tailleFilm || f[0].Total != tailleFilm {
		t.Errorf("etat = %+v", f)
	}
	if !b.m.EstPret("iptva:2001") || b.m.EstPret("iptva:2002") {
		t.Error("EstPret")
	}
	if b.instr.valeur("preparation_octets_total") != tailleFilm || b.instr.valeur("preparation_fichiers_total{etat=pret}") != 1 || b.instr.valeur("preparation_reprises_total") != 1 {
		t.Errorf("metriques : %v", b.instr.compteurs)
	}
	// Pret : chemin et verrou de lecture.
	chemin2, liberer, ok := b.m.Pret("iptva:2001")
	if !ok || chemin2 != chemin {
		t.Fatalf("Pret = %q %v", chemin2, ok)
	}
	liberer()
	// Index persistant : un nouveau moteur sur le meme dossier retrouve le fichier pret.
	m2 := b.moteur(b.m.r)
	if !m2.EstPret("iptva:2001") {
		t.Error("fichier pret oublie apres redemarrage")
	}
	var idx []map[string]any
	brut, _ := os.ReadFile(filepath.Join(b.dossier, nomIndex))
	if err := json.Unmarshal(brut, &idx); err != nil || len(idx) != 1 || idx[0]["etat"] != "pret" {
		t.Errorf("index.json : %s %v", brut, err)
	}
}

// CA2 : un `.partiel` laisse par un redemarrage est repris la ou il en etait (Range) ; une
// reponse qui ignore Range est refusee (pas de recollage) ; un 404 est un echec assaini.
func TestCA2_RepriseDuPartielApresRedemarrage(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, nil, nil)
	ctx := context.Background()
	b.liste.AjouterAVoir(ctx, "p1", domain.ElementAVoir{Contenu: "iptva:2001"})
	// Un partiel de 100 000 octets exacts et un index qui le decrit.
	attendu := b.contenuAttendu("2001.mp4")
	b.faux.RazCompteurs()
	os.WriteFile(filepath.Join(b.dossier, "film-iptva-2001.mp4.partiel"), attendu[:100_000], 0o600)
	os.WriteFile(filepath.Join(b.dossier, nomIndex), []byte(`[{"contenu":"iptva:2001","nom":"film-iptva-2001.mp4","etat":"en_cours","octets":5,"total":300000}]`), 0o600)
	os.WriteFile(filepath.Join(b.dossier, "film-iptva-9999.mp4"), []byte("orphelin"), 0o600)
	m := b.moteur(b.m.r)
	if f := m.Fichiers(); len(f) != 1 || f[0].Etat != domain.PreparationAttente || f[0].Octets != 100_000 {
		t.Fatalf("apres relecture : %+v", f)
	}
	if _, err := os.Stat(filepath.Join(b.dossier, "film-iptva-9999.mp4")); err == nil {
		t.Error("fichier orphelin conserve")
	}
	m.cycle(ctx)
	corps, _ := os.ReadFile(filepath.Join(b.dossier, "film-iptva-2001.mp4"))
	if !bytes.Equal(corps, attendu) {
		t.Error("fichier incomplet ou corrompu apres reprise")
	}
	if vus := rangesVus(b.faux); len(vus) != 1 || vus[0] != "bytes=100000-" {
		t.Errorf("Range de reprise = %v", vus)
	}

	// Reprise mal repondue (200 au lieu de 206) : refusee, rien n'est recolle.
	b.faux.RazCompteurs()
	os.Remove(filepath.Join(b.dossier, "film-iptva-2001.mp4"))
	os.WriteFile(filepath.Join(b.dossier, "film-iptva-2001.mp4.partiel"), attendu[:100_000], 0o600)
	os.WriteFile(filepath.Join(b.dossier, nomIndex), []byte(`[{"contenu":"iptva:2001","nom":"film-iptva-2001.mp4","etat":"attente","octets":100000,"total":300000}]`), 0o600)
	m = b.moteur(b.m.r)
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleFilm, IgnorerRange: true, Fois: 1})
	m.cycle(ctx)
	if f := m.Fichiers(); len(f) != 1 || f[0].Etat != domain.PreparationEchec || !strings.Contains(f[0].Cause, "206") {
		t.Errorf("200 a la place de 206 accepte : %+v", f)
	}
	// 404 : echec, cause lisible, sans identifiants.
	b.liste.AjouterAVoir(ctx, "p1", domain.ElementAVoir{Contenu: "iptva:2002"})
	b.faux.Programmer(fauxiptv.Panne{Cible: fauxiptv.CibleFilm, Statut: http.StatusNotFound, Fois: 1})
	m.cycle(ctx)
	for _, f := range m.Fichiers() {
		if f.Contenu == "iptva:2002" {
			if f.Etat != domain.PreparationEchec || strings.Contains(f.Cause, b.faux.MotDePasse()) || f.Cause == "" {
				t.Errorf("404 : %+v", f)
			}
		}
	}
}

func noms(dossier string) (r []string) {
	entrees, _ := os.ReadDir(dossier)
	for _, e := range entrees {
		r = append(r, e.Name())
	}
	return
}

// ---------------------------------------------------------------- CA3 : le spectateur d'abord

// CA3 : aucune connexion de preparation tant qu'une lecture tient une place ; le
// telechargement en cours est coupe (en moins de 2 s, .partiel conserve) des qu'une lecture
// demande une place, et reprend par Range `reprise_apres` apres la fin de la lecture. Le
// faux fournisseur n'a jamais vu deux films servis en meme temps.
func TestCA3_LeSpectateurAToujoursLaPriorite(t *testing.T) {
	g := gestionnaireBaux(t)
	b := nouveauBanc(t, fauxiptv.Options{TailleFilm: 2 << 20, DebitFilm: 1 << 20}, func(r *Reglages) { r.RepriseApres = 300 * time.Millisecond }, g)
	g.AvecVeilleur(b.m.Veilleur())
	ctx := context.Background()
	b.liste.AjouterAVoir(ctx, "p1", domain.ElementAVoir{Contenu: "iptva:2001"})
	attendu := b.contenuAttendu("2001.mp4")
	b.faux.RazCompteurs()

	// Une lecture en cours AVANT le lancement : rien ne part.
	bail, err := g.Acquerir(ctx, "iptva", "s1", "iptva:2001")
	if err != nil {
		t.Fatal(err)
	}
	b.lancer(b.m)
	time.Sleep(400 * time.Millisecond)
	if n := b.faux.Compteurs().Films; n != 0 {
		t.Fatalf("%d requete(s) de preparation pendant une lecture", n)
	}
	bail.Liberer()

	// Le telechargement demarre apres reprise_apres (300 ms) : ~2 s de transfert a 1 Mio/s.
	b.attendre(func() bool { return b.faux.FilmsOuverts() == 1 }, 3*time.Second, "telechargement non demarre")
	time.Sleep(300 * time.Millisecond)

	// Un spectateur demande une place : la preparation doit couper sa connexion (< 2 s).
	debut := time.Now()
	bail, err = g.Acquerir(ctx, "iptva", "s1", "iptva:2002")
	if err != nil {
		t.Fatal(err)
	}
	b.attendre(func() bool { return b.faux.FilmsOuverts() == 0 }, 2*time.Second, "preparation pas mise en pause")
	pause := time.Since(debut)
	if b.etatDe("iptva:2001") != domain.PreparationAttente {
		t.Errorf("etat pendant la pause = %s", b.etatDe("iptva:2001"))
	}
	if _, err := os.Stat(filepath.Join(b.dossier, "film-iptva-2001.mp4.partiel")); err != nil {
		t.Error(".partiel efface par la pause")
	}
	// Le spectateur lit (une vraie requete au faux, comme le ferait vod) pendant 500 ms.
	rep, err := b.client.Requete(ctx, b.client.URLFilm("2002", "mp4"), http.Header{"Range": {"bytes=0-65535"}})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	rep.Body.Close()
	if n := b.faux.FilmsOuverts(); n != 1 && n != 0 {
		t.Errorf("films ouverts pendant la lecture = %d", n)
	}
	if n := b.faux.Compteurs().Films; n != 2 { // la preparation (1) + le spectateur (1) : aucune reprise pendant la lecture
		t.Errorf("%d requetes de films pendant la lecture (attendu 2)", n)
	}
	bail.Liberer()
	finLecture := time.Now()

	// Reprise apres reprise_apres, par Range depuis le partiel, jusqu'au fichier complet.
	b.attendre(func() bool { return b.etatDe("iptva:2001") == domain.PreparationPrete }, 8*time.Second, "fichier jamais pret")
	corps, _ := os.ReadFile(filepath.Join(b.dossier, "film-iptva-2001.mp4"))
	if !bytes.Equal(corps, attendu) {
		t.Error("fichier corrompu apres pause et reprise")
	}
	vus := rangesVus(b.faux)
	if len(vus) != 3 || vus[0] != "" || !strings.HasPrefix(vus[2], "bytes=") || vus[2] == "bytes=0-" {
		t.Errorf("Range amont = %v (attendu : plein, lecture du spectateur, reprise)", vus)
	}
	if pic := b.faux.Compteurs().FilmsPic; pic != 1 {
		t.Errorf("FilmsPic = %d : deux connexions simultanees chez le fournisseur", pic)
	}
	if b.instr.valeur("preparation_pauses_total") < 1 {
		t.Error("pause non comptee")
	}
	t.Logf("pause effective en %v, reprise %v apres la fin de la lecture", pause, time.Since(finLecture))
}

// ---------------------------------------------------------------- CA4 : budget et rotation

// CA4 : budget de 3 fichiers, serie de 5 episodes a preparer : 3 prets, 2 en attente, jamais
// plus de 3 sur disque ; les deux premiers vus -> effaces, les suivants prepares.
func TestCA4_BudgetEtRotation(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, func(r *Reglages) { r.Budget = 3 * tailleFilm }, nil)
	ctx := context.Background()
	b.liste.AjouterAVoir(ctx, "p1", domain.ElementAVoir{Contenu: "iptva:s77", Serie: true, EpisodesAvance: 5})

	// Un guetteur compte en continu les fichiers sur le disque.
	var pic atomic.Int32
	arret := make(chan struct{})
	guetteurFini := make(chan struct{})
	go func() {
		defer close(guetteurFini)
		for {
			select {
			case <-arret:
				return
			default:
				pic.Store(max(pic.Load(), int32(b.fichiersSurDisque())))
				time.Sleep(2 * time.Millisecond)
			}
		}
	}()
	defer func() { close(arret); <-guetteurFini }()

	b.lancer(b.m)
	prets := func() (n int) {
		for _, f := range b.m.Fichiers() {
			if f.Etat == domain.PreparationPrete {
				n++
			}
		}
		return
	}
	b.attendre(func() bool { return prets() == 3 }, 10*time.Second, "3 episodes prets")
	time.Sleep(300 * time.Millisecond) // le moteur tente les suivants : ils doivent rester en attente
	if prets() != 3 || b.etatDe(episode(4)) != domain.PreparationAttente || b.etatDe(episode(5)) != domain.PreparationAttente {
		t.Fatalf("etats : %+v", b.m.Fichiers())
	}
	if b.m.Occupe() > 3*tailleFilm {
		t.Errorf("budget depasse : %d octets", b.m.Occupe())
	}
	// Les deux premiers sont vus : effaces a la planification suivante, 4 et 5 prepares.
	b.etat.vu("p1", episode(1), "iptva:s77")
	b.etat.vu("p1", episode(2), "iptva:s77")
	b.m.Reveiller()
	b.attendre(func() bool {
		return b.etatDe(episode(4)) == domain.PreparationPrete && b.etatDe(episode(5)) == domain.PreparationPrete
	}, 10*time.Second, "episodes 4 et 5 prets")
	if b.etatDe(episode(1)) != "" || b.etatDe(episode(2)) != "" {
		t.Errorf("episodes vus non effaces : %+v", b.m.Fichiers())
	}
	for _, n := range []int{1, 2} {
		if _, err := os.Stat(filepath.Join(b.dossier, "episode-iptva-770"+string(rune('0'+n))+".mp4")); err == nil {
			t.Errorf("fichier de l'episode %d encore sur le disque", n)
		}
	}
	if pic.Load() > 3 {
		t.Errorf("jusqu'a %d fichiers sur le disque pour un budget de 3", pic.Load())
	}
	if b.instr.valeur("preparation_fichiers_total{etat=efface}") != 2 {
		t.Errorf("effacements comptes : %v", b.instr.compteurs)
	}
}

// CA4 : un fichier plus grand que le budget est un echec explicite ; un fichier qui n'est
// plus demande par aucune liste cede sa place ; un fichier en cours de lecture n'est jamais efface.
func TestCA4_EvictionDesNonDemandesJamaisEnLecture(t *testing.T) {
	b := nouveauBanc(t, fauxiptv.Options{}, func(r *Reglages) { r.Budget = tailleFilm + tailleFilm/2 }, nil)
	ctx := context.Background()
	b.liste.AjouterAVoir(ctx, "p1", domain.ElementAVoir{Contenu: "iptva:2001"})
	b.m.cycle(ctx)
	if !b.m.EstPret("iptva:2001") {
		t.Fatal("2001 pas pret")
	}
	// 2001 retire de la liste, 2002 ajoute : 2001 (non demande) est efface pour faire la place.
	b.liste.RetirerAVoir(ctx, "p1", "vod:iptva:2001")
	b.liste.AjouterAVoir(ctx, "p1", domain.ElementAVoir{Contenu: "iptva:2002"})
	_, liberer, _ := b.m.Pret("iptva:2001") // ...sauf qu'il est en cours de lecture
	b.m.cycle(ctx)
	if !b.m.EstPret("iptva:2001") || b.etatDe("iptva:2002") != domain.PreparationAttente {
		t.Fatalf("fichier en lecture efface, ou 2002 pas en attente : %+v", b.m.Fichiers())
	}
	liberer()
	b.m.cycle(ctx)
	if b.m.EstPret("iptva:2001") || !b.m.EstPret("iptva:2002") {
		t.Errorf("rotation : %+v", b.m.Fichiers())
	}
	// Plus grand que le budget : echec explicite, rien sur le disque.
	b.m.r.Budget = tailleFilm / 2
	b.liste.AjouterAVoir(ctx, "p1", domain.ElementAVoir{Contenu: "iptva:2001"})
	b.m.cycle(ctx)
	if e := b.etatDe("iptva:2001"); e != domain.PreparationEchec {
		t.Errorf("fichier trop grand : %s", e)
	}
}
