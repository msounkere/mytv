package stockage

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"mytv/internal/domain"
)

func ouvrirTest(t *testing.T) *Base {
	t.Helper()
	b, err := Ouvrir(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Fermer() })
	return b
}

func chaine(f string, n int, nom, cat string, ordre int) domain.Chaine {
	return domain.Chaine{
		ID: domain.ContenuID(fmt.Sprintf("%s:%d", f, n)), FournisseurID: f, Numero: fmt.Sprint(n),
		Nom: nom, CategorieID: cat, Ordre: ordre,
	}
}

func instantane(f string, pris time.Time, chaines ...domain.Chaine) *domain.Instantane {
	return &domain.Instantane{
		FournisseurID: f, Pris: pris, Chaines: chaines,
		Categories: []domain.Categorie{{ID: "1", FournisseurID: f, Type: domain.TypeLive, Nom: "Cat " + f, Ordre: 1}},
	}
}

func noms(cs []domain.Chaine) []string {
	var r []string
	for _, c := range cs {
		r = append(r, c.Nom)
	}
	return r
}

// CA1 : les migrations sont enregistrees et rejouables.
func TestMigrationsRejouablesSansErreur(t *testing.T) {
	ctx := context.Background()
	chemin := filepath.Join(t.TempDir(), "m.db")
	b, err := Ouvrir(ctx, chemin)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.EcrireCatalogue(ctx, instantane("a", time.Now(), chaine("a", 1, "Une", "1", 1))); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := b.db.QueryRow(`SELECT COUNT(*) FROM migrations`).Scan(&n); err != nil || n != len(migrations) {
		t.Fatalf("migrations enregistrees = %d (err %v), attendu %d", n, err, len(migrations))
	}
	b.Fermer()

	// Reouverture : rien ne doit etre rejoue ni perdu.
	b, err = Ouvrir(ctx, chemin)
	if err != nil {
		t.Fatalf("reouverture : %v", err)
	}
	defer b.Fermer()
	if err := b.migrer(ctx); err != nil {
		t.Fatalf("rejeu explicite : %v", err)
	}
	if err := b.db.QueryRow(`SELECT COUNT(*) FROM migrations`).Scan(&n); err != nil || n != len(migrations) {
		t.Fatalf("apres rejeu : %d migrations (err %v)", n, err)
	}
	if _, err := b.Chaine(ctx, "a:1"); err != nil {
		t.Fatalf("donnees perdues a la reouverture : %v", err)
	}
}

func TestBaseTropRecenteRefusee(t *testing.T) {
	ctx := context.Background()
	b := ouvrirTest(t)
	if _, err := b.db.Exec(`INSERT INTO migrations VALUES (999, 'futur', 0)`); err != nil {
		t.Fatal(err)
	}
	if err := b.migrer(ctx); err == nil {
		t.Fatal("une base plus recente que le programme doit etre refusee")
	}
}

// CA2 : un echec au milieu de l'ecriture laisse l'ancien catalogue intact.
func TestEchecEnCoursDEcritureConserveAncienCatalogue(t *testing.T) {
	ctx := context.Background()
	b := ouvrirTest(t)
	ancien := time.Date(2026, 1, 1, 5, 0, 0, 0, time.UTC)
	if err := b.EcrireCatalogue(ctx, instantane("a", ancien, chaine("a", 1, "Ancienne", "1", 1))); err != nil {
		t.Fatal(err)
	}

	panne := errors.New("panne injectee")
	b.apresInsertion = func(table string) error {
		if table == "chaines" { // les categories sont deja ecrites, les films pas encore
			return panne
		}
		return nil
	}
	err := b.EcrireCatalogue(ctx, instantane("a", ancien.Add(time.Hour), chaine("a", 2, "Nouvelle", "1", 1)))
	if !errors.Is(err, panne) {
		t.Fatalf("erreur = %v, attendu la panne injectee", err)
	}
	b.apresInsertion = nil

	cs, err := b.Chaines(ctx, domain.FiltreChaines{})
	if err != nil || len(cs) != 1 || cs[0].Nom != "Ancienne" {
		t.Fatalf("chaines = %v (err %v), l'ancien catalogue devait rester", noms(cs), err)
	}
	if got, _ := b.DerniereSynchro(ctx, "a"); !got.Equal(ancien) {
		t.Fatalf("derniere synchro = %v, attendu %v", got, ancien)
	}
	if res, _ := b.Chaines(ctx, domain.FiltreChaines{Recherche: "nouvelle"}); len(res) != 0 {
		t.Fatal("l'index de recherche contient des lignes de la transaction annulee")
	}
}

// CA2 : un doublon de ContenuID dans l'instantane fait echouer l'ecriture sans rien changer.
func TestDoublonDansInstantaneAnnuleEcriture(t *testing.T) {
	ctx := context.Background()
	b := ouvrirTest(t)
	b.EcrireCatalogue(ctx, instantane("a", time.Now(), chaine("a", 1, "Ancienne", "1", 1)))
	err := b.EcrireCatalogue(ctx, instantane("a", time.Now(), chaine("a", 2, "X", "1", 1), chaine("a", 2, "Y", "1", 2)))
	if err == nil {
		t.Fatal("un ContenuID duplique doit etre refuse")
	}
	if cs, _ := b.Chaines(ctx, domain.FiltreChaines{}); len(cs) != 1 || cs[0].Nom != "Ancienne" {
		t.Fatalf("catalogue modifie malgre l'echec : %v", noms(cs))
	}
}

// CA2 : l'annulation du contexte annule aussi l'ecriture.
func TestContexteAnnuleAnnuleEcriture(t *testing.T) {
	b := ouvrirTest(t)
	ctx, annuler := context.WithCancel(context.Background())
	annuler()
	if err := b.EcrireCatalogue(ctx, instantane("a", time.Now(), chaine("a", 1, "X", "1", 1))); err == nil {
		t.Fatal("ecriture avec contexte annule : une erreur etait attendue")
	}
	if cs, _ := b.Chaines(context.Background(), domain.FiltreChaines{}); len(cs) != 0 {
		t.Fatal("rien ne devait etre ecrit")
	}
}

func TestElementDUnAutreFournisseurRefuse(t *testing.T) {
	b := ouvrirTest(t)
	s := instantane("a", time.Now(), chaine("b", 1, "Intrus", "1", 1))
	if err := b.EcrireCatalogue(context.Background(), s); err == nil {
		t.Fatal("une chaine d'un autre fournisseur doit etre refusee")
	}
}

// CA3 : deux fournisseurs coexistent ; remplacer l'un ne touche pas l'autre.
func TestCataloguesDeDeuxFournisseursCoexistent(t *testing.T) {
	ctx := context.Background()
	b := ouvrirTest(t)
	t1 := time.Date(2026, 3, 1, 5, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)
	b.EcrireCatalogue(ctx, instantane("a", t1, chaine("a", 1, "Alpha", "1", 1)))
	b.EcrireCatalogue(ctx, instantane("b", t2, chaine("b", 1, "Beta", "1", 1)))

	// Meme numero chez les deux fournisseurs : les ContenuID restent distincts.
	for id, nom := range map[domain.ContenuID]string{"a:1": "Alpha", "b:1": "Beta"} {
		c, err := b.Chaine(ctx, id)
		if err != nil || c.Nom != nom || string(c.ID[:1]) != c.FournisseurID {
			t.Fatalf("Chaine(%s) = %+v, %v", id, c, err)
		}
	}
	// Remplacement de A.
	b.EcrireCatalogue(ctx, instantane("a", t2, chaine("a", 9, "Alpha2", "1", 1)))
	cs, _ := b.Chaines(ctx, domain.FiltreChaines{})
	if len(cs) != 2 || cs[0].Nom == "Alpha" || cs[1].Nom == "Alpha" {
		t.Fatalf("chaines = %v", noms(cs))
	}
	if _, err := b.Chaine(ctx, "b:1"); err != nil {
		t.Fatal("le catalogue de B ne devait pas bouger")
	}
	if got, _ := b.DerniereSynchro(ctx, "b"); !got.Equal(t2) {
		t.Fatalf("synchro de B = %v", got)
	}
	if got, _ := b.DerniereSynchro(ctx, "a"); !got.Equal(t2) {
		t.Fatalf("synchro de A = %v", got)
	}
	// Les categories des deux fournisseurs sont presentes.
	cats, _ := b.Categories(ctx, domain.TypeLive)
	if len(cats) != 2 {
		t.Fatalf("categories = %+v", cats)
	}
	// Un ContenuID deja pris par un AUTRE fournisseur est refuse.
	if err := b.EcrireCatalogue(ctx, instantane("a", t2, domain.Chaine{ID: "b:1", FournisseurID: "a", Nom: "Vol"})); err == nil {
		t.Fatal("collision de ContenuID entre fournisseurs : erreur attendue")
	}
	if c, _ := b.Chaine(ctx, "b:1"); c == nil || c.Nom != "Beta" {
		t.Fatal("la chaine de B a ete ecrasee")
	}
}

// CA4 : filtre par categorie, recherche sans casse ni accents, pagination, tri stable.
func TestChainesFiltreRechercheEtPagination(t *testing.T) {
	ctx := context.Background()
	b := ouvrirTest(t)
	b.EcrireCatalogue(ctx, instantane("a", time.Now(),
		chaine("a", 5, "Zèbre TV", "2", 1),
		chaine("a", 3, "Éclair Sport", "1", 3),
		chaine("a", 4, "ECLAIR Info", "1", 3), // meme ordre : departage par id
		chaine("a", 1, "France 2", "1", 2),
		chaine("a", 2, "Canal+ Cinéma", "2", 4),
	))
	cas := []struct {
		nom     string
		filtre  domain.FiltreChaines
		attendu []string
	}{
		{"tout, trie par ordre puis id", domain.FiltreChaines{}, []string{"Zèbre TV", "France 2", "Éclair Sport", "ECLAIR Info", "Canal+ Cinéma"}},
		{"categorie", domain.FiltreChaines{CategorieID: "1"}, []string{"France 2", "Éclair Sport", "ECLAIR Info"}},
		{"casse", domain.FiltreChaines{Recherche: "france"}, []string{"France 2"}},
		{"accent dans la donnee", domain.FiltreChaines{Recherche: "eclair"}, []string{"Éclair Sport", "ECLAIR Info"}},
		{"accent dans la recherche", domain.FiltreChaines{Recherche: "ÉCLAIR"}, []string{"Éclair Sport", "ECLAIR Info"}},
		{"accent dans la recherche inverse", domain.FiltreChaines{Recherche: "cinema"}, []string{"Canal+ Cinéma"}},
		{"prefixe", domain.FiltreChaines{Recherche: "fra"}, []string{"France 2"}},
		{"mots combines en ET", domain.FiltreChaines{Recherche: "eclair info"}, []string{"ECLAIR Info"}},
		{"recherche et categorie", domain.FiltreChaines{Recherche: "eclair", CategorieID: "2"}, []string{}},
		{"operateurs FTS neutralises", domain.FiltreChaines{Recherche: `"fra* OR NEAR(`}, []string{}},
		{"sans lettre", domain.FiltreChaines{Recherche: "!!!"}, []string{}},
		{"page 1", domain.FiltreChaines{Limite: 2}, []string{"Zèbre TV", "France 2"}},
		{"page 2", domain.FiltreChaines{Limite: 2, Decalage: 2}, []string{"Éclair Sport", "ECLAIR Info"}},
		{"page 3", domain.FiltreChaines{Limite: 2, Decalage: 4}, []string{"Canal+ Cinéma"}},
		{"au dela", domain.FiltreChaines{Limite: 2, Decalage: 10}, []string{}},
	}
	for _, c := range cas {
		t.Run(c.nom, func(t *testing.T) {
			got, err := b.Chaines(ctx, c.filtre)
			if err != nil {
				t.Fatal(err)
			}
			if fmt.Sprint(noms(got)) != fmt.Sprint(c.attendu) && !(len(got) == 0 && len(c.attendu) == 0) {
				t.Fatalf("obtenu %v, attendu %v", noms(got), c.attendu)
			}
		})
	}
}

func TestRechercheNeTrouvePasUneChaineRemplacee(t *testing.T) {
	ctx := context.Background()
	b := ouvrirTest(t)
	b.EcrireCatalogue(ctx, instantane("a", time.Now(), chaine("a", 1, "Vieille", "1", 1)))
	b.EcrireCatalogue(ctx, instantane("a", time.Now(), chaine("a", 2, "Neuve", "1", 1)))
	if got, _ := b.Chaines(ctx, domain.FiltreChaines{Recherche: "vieille"}); len(got) != 0 {
		t.Fatalf("l'index garde une chaine supprimee : %v", noms(got))
	}
	if got, _ := b.Chaines(ctx, domain.FiltreChaines{Recherche: "neuve"}); len(got) != 1 {
		t.Fatal("la nouvelle chaine doit etre trouvee")
	}
}

// CA5 : categories ordonnees par type ; Chaine absente = ErrNonTrouve.
func TestCategoriesOrdonneesParType(t *testing.T) {
	ctx := context.Background()
	b := ouvrirTest(t)
	s := &domain.Instantane{FournisseurID: "a", Pris: time.Now(), Categories: []domain.Categorie{
		{ID: "9", FournisseurID: "a", Type: domain.TypeLive, Nom: "Sport", Ordre: 3},
		{ID: "7", FournisseurID: "a", Type: domain.TypeLive, Nom: "Info", Ordre: 1},
		{ID: "8", FournisseurID: "a", Type: domain.TypeLive, Nom: "Cine", Ordre: 2},
		{ID: "7", FournisseurID: "a", Type: domain.TypeFilm, Nom: "Action", Ordre: 1}, // meme id, autre type
	}}
	if err := b.EcrireCatalogue(ctx, s); err != nil {
		t.Fatal(err)
	}
	live, err := b.Categories(ctx, domain.TypeLive)
	if err != nil || len(live) != 3 || live[0].Nom != "Info" || live[1].Nom != "Cine" || live[2].Nom != "Sport" {
		t.Fatalf("live = %+v (%v)", live, err)
	}
	films, _ := b.Categories(ctx, domain.TypeFilm)
	if len(films) != 1 || films[0].Nom != "Action" || films[0].Type != domain.TypeFilm {
		t.Fatalf("films = %+v", films)
	}
	if series, err := b.Categories(ctx, domain.TypeSerie); err != nil || len(series) != 0 {
		t.Fatalf("series = %+v (%v)", series, err)
	}
}

func TestChaineAbsenteRenvoieErrNonTrouve(t *testing.T) {
	b := ouvrirTest(t)
	c, err := b.Chaine(context.Background(), "x:1")
	if c != nil || !errors.Is(err, ErrNonTrouve) {
		t.Fatalf("Chaine = %v, %v", c, err)
	}
}

func TestChaineRelitTousLesChamps(t *testing.T) {
	ctx := context.Background()
	b := ouvrirTest(t)
	voulue := domain.Chaine{ID: "a:845410", FournisseurID: "a", Numero: "845410", Nom: "Ünique", Logo: "http://l/x.png",
		EpgID: "x.fr", CategorieID: "12", NumeroChaine: 7, Ordre: 4}
	b.EcrireCatalogue(ctx, instantane("a", time.Now(), voulue))
	got, err := b.Chaine(ctx, "a:845410")
	if err != nil || *got != voulue {
		t.Fatalf("obtenu %+v (%v), attendu %+v", got, err, voulue)
	}
}

// CA6 : zero value tant que rien n'a ete ecrit.
func TestDerniereSynchroZeroSiJamaisEcrit(t *testing.T) {
	b := ouvrirTest(t)
	got, err := b.DerniereSynchro(context.Background(), "inconnu")
	if err != nil || !got.IsZero() {
		t.Fatalf("obtenu %v, %v", got, err)
	}
}

func TestDerniereSynchroConserveLInstant(t *testing.T) {
	ctx := context.Background()
	b := ouvrirTest(t)
	pris := time.Date(2026, 5, 6, 7, 8, 9, 0, time.FixedZone("x", 3600))
	b.EcrireCatalogue(ctx, instantane("a", pris))
	if got, _ := b.DerniereSynchro(ctx, "a"); !got.Equal(pris) {
		t.Fatalf("obtenu %v, attendu %v", got, pris)
	}
}

func TestFilmsEtSeriesSontEcrits(t *testing.T) {
	ctx := context.Background()
	b := ouvrirTest(t)
	s := instantane("a", time.Now())
	s.Films = []domain.Film{{ID: "a:1", FournisseurID: "a", Numero: "1", Nom: "F", Extension: "mkv", Note: 7.5, Ajoute: time.Now()}}
	s.Series = []domain.SerieTV{{ID: "a:2", FournisseurID: "a", Numero: "2", Nom: "S"}}
	if err := b.EcrireCatalogue(ctx, s); err != nil {
		t.Fatal(err)
	}
	var nf, ns int
	b.db.QueryRow(`SELECT COUNT(*) FROM films`).Scan(&nf)
	b.db.QueryRow(`SELECT COUNT(*) FROM series`).Scan(&ns)
	if nf != 1 || ns != 1 {
		t.Fatalf("films %d, series %d", nf, ns)
	}
	// Remplacement : les anciens films disparaissent.
	b.EcrireCatalogue(ctx, instantane("a", time.Now()))
	b.db.QueryRow(`SELECT COUNT(*) FROM films`).Scan(&nf)
	if nf != 0 {
		t.Fatalf("films restants = %d", nf)
	}
}

// CA8 : lectures pendant des ecritures repetees, sans « database is locked ».
func TestLecturesPendantEcritureSansVerrou(t *testing.T) {
	ctx := context.Background()
	b := ouvrirTest(t)
	// Le code SQLite transpile est ~50 fois plus lent sous -race : lot reduit, la
	// concurrence (ce que -race verifie) reste la meme.
	taille := 1500
	if !verifierSeuils {
		taille = 150
	}
	b.EcrireCatalogue(ctx, instantane("a", time.Now(), lot("a", 0, taille)...))

	fin := make(chan struct{})
	var wg sync.WaitGroup
	erreurs := make(chan error, 64)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-fin:
					return
				default:
				}
				// Pause courte : des lecteurs en boucle serree affameraient l'ecrivain
				// (surtout sous -race) sans rien prouver de plus.
				time.Sleep(time.Millisecond)
				if _, err := b.Chaines(ctx, domain.FiltreChaines{Recherche: "chaine", Limite: 20}); err != nil {
					erreurs <- err
					return
				}
				if _, err := b.DerniereSynchro(ctx, "a"); err != nil {
					erreurs <- err
					return
				}
			}
		}()
	}
	// Deux ecrivains concurrents (fournisseurs differents) : ils se serialisent.
	var ew sync.WaitGroup
	for _, f := range []string{"a", "b"} {
		ew.Add(1)
		go func() {
			defer ew.Done()
			for i := 0; i < 4; i++ {
				if err := b.EcrireCatalogue(ctx, instantane(f, time.Now(), lot(f, i, taille)...)); err != nil {
					erreurs <- err
					return
				}
			}
		}()
	}
	ew.Wait()
	close(fin)
	wg.Wait()
	close(erreurs)
	for err := range erreurs {
		t.Errorf("erreur concurrente : %v", err)
	}
}

func lot(f string, graine, n int) []domain.Chaine {
	cs := make([]domain.Chaine, n)
	for i := range cs {
		cs[i] = chaine(f, i, fmt.Sprintf("Chaîne %d %d", graine, i), fmt.Sprint(i%50), i)
	}
	return cs
}

// donneesCA7 : 25 000 chaines + 40 000 films, comme le catalogue reel.
func donneesCA7() *domain.Instantane {
	s := &domain.Instantane{FournisseurID: "a", Pris: time.Now()}
	for i := 0; i < 50; i++ {
		s.Categories = append(s.Categories, domain.Categorie{ID: fmt.Sprint(i), FournisseurID: "a", Type: domain.TypeLive, Nom: fmt.Sprint("Cat ", i), Ordre: i})
	}
	mots := []string{"France", "Canal", "Sport", "Cinéma", "Info", "Musique", "Arte", "Eurosport", "Documentaire", "Enfants"}
	for i := 0; i < 25000; i++ {
		s.Chaines = append(s.Chaines, domain.Chaine{
			ID: domain.ContenuID(fmt.Sprint("a:", i)), FournisseurID: "a", Numero: fmt.Sprint(i),
			Nom: fmt.Sprintf("FR %s %d HD", mots[i%len(mots)], i), Logo: "http://logo/x.png", EpgID: fmt.Sprint("e", i),
			CategorieID: fmt.Sprint(i % 50), NumeroChaine: i, Ordre: i,
		})
	}
	for i := 0; i < 40000; i++ {
		s.Films = append(s.Films, domain.Film{
			ID: domain.ContenuID(fmt.Sprint("a:f", i)), FournisseurID: "a", Numero: fmt.Sprint("f", i),
			Nom: fmt.Sprintf("Film numéro %d", i), Image: "http://img/x.jpg", CategorieID: fmt.Sprint(i % 50),
			Extension: "mkv", Note: 6.5, Annee: "2020", Ajoute: time.Now(),
		})
	}
	return s
}

// CA7 : ecriture < 5 s, recherche < 50 ms (seuils verifies hors -race).
func TestVolumeReelEcritureEtRecherche(t *testing.T) {
	if testing.Short() || !verifierSeuils {
		t.Skip("volume reel : mesure hors -race et hors -short (voir le benchmark)")
	}
	ctx := context.Background()
	b := ouvrirTest(t)
	s := donneesCA7()
	debut := time.Now()
	if err := b.EcrireCatalogue(ctx, s); err != nil {
		t.Fatal(err)
	}
	ecriture := time.Since(debut)
	debut = time.Now()
	res, err := b.Chaines(ctx, domain.FiltreChaines{Recherche: "cinema", Limite: 50})
	recherche := time.Since(debut)
	if err != nil || len(res) != 50 {
		t.Fatalf("recherche : %d resultats, %v", len(res), err)
	}
	t.Logf("ecriture 25000 chaines + 40000 films : %v ; recherche : %v", ecriture, recherche)
	if verifierSeuils {
		if ecriture > 5*time.Second {
			t.Errorf("ecriture trop lente : %v (> 5 s)", ecriture)
		}
		if recherche > 50*time.Millisecond {
			t.Errorf("recherche trop lente : %v (> 50 ms)", recherche)
		}
	}
}

func BenchmarkEcritureCatalogueReel(b *testing.B) {
	ctx := context.Background()
	s := donneesCA7()
	st, err := Ouvrir(ctx, filepath.Join(b.TempDir(), "b.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer st.Fermer()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := st.EcrireCatalogue(ctx, s); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRechercheChaines(b *testing.B) {
	ctx := context.Background()
	st, err := Ouvrir(ctx, filepath.Join(b.TempDir(), "b.db"))
	if err != nil {
		b.Fatal(err)
	}
	defer st.Fermer()
	if err := st.EcrireCatalogue(ctx, donneesCA7()); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := st.Chaines(ctx, domain.FiltreChaines{Recherche: "cinema", Limite: 50}); err != nil {
			b.Fatal(err)
		}
	}
}

// Le filtre FournisseurID evite de melanger deux fournisseurs qui ont la meme categorie « 1 ».
func TestChainesFiltreParFournisseur(t *testing.T) {
	ctx := context.Background()
	b := ouvrirTest(t)
	t0 := time.Date(2026, 3, 1, 5, 0, 0, 0, time.UTC)
	b.EcrireCatalogue(ctx, instantane("a", t0, chaine("a", 1, "Alpha", "1", 1)))
	b.EcrireCatalogue(ctx, instantane("b", t0, chaine("b", 1, "Beta", "1", 1)))
	cs, err := b.Chaines(ctx, domain.FiltreChaines{FournisseurID: "b", CategorieID: "1"})
	if err != nil || len(cs) != 1 || cs[0].Nom != "Beta" {
		t.Fatalf("chaines de b = %v, %v", noms(cs), err)
	}
}

func TestExistantsIgnoreLesAbsents(t *testing.T) {
	ctx := context.Background()
	b := ouvrirTest(t)
	inst := instantane("a", time.Now(), chaine("a", 1, "Alpha", "1", 1))
	inst.Films = []domain.Film{{ID: "a:f1", FournisseurID: "a", Numero: "f1", Nom: "Film"}}
	inst.Series = []domain.SerieTV{{ID: "a:s1", FournisseurID: "a", Numero: "s1", Nom: "Serie"}}
	if err := b.EcrireCatalogue(ctx, inst); err != nil {
		t.Fatal(err)
	}
	res, err := b.Existants(ctx, []domain.ContenuID{"a:1", "a:f1", "a:s1", "a:disparu"})
	if err != nil || len(res) != 3 || res["a:disparu"] {
		t.Fatalf("existants = %v, %v", res, err)
	}
	// CA6 (US-44) : le type de chaque identifiant, pour la migration des favoris.
	types, err := b.Types(ctx, []domain.ContenuID{"a:1", "a:f1", "a:s1", "a:disparu"})
	if err != nil || len(types) != 3 || types["a:1"] != domain.TypeLive || types["a:f1"] != domain.TypeFilm || types["a:s1"] != domain.TypeSerie {
		t.Fatalf("types = %v, %v", types, err)
	}
}

func catalogueVideo(f string, pris time.Time) *domain.Instantane {
	inst := instantane(f, pris)
	for i, nom := range []string{"Éclair noir", "alpha", "Zèbre", "Eclair blanc"} {
		cat := "1"
		if i%2 == 1 {
			cat = "2"
		}
		n := fmt.Sprintf("%d", i+1)
		inst.Films = append(inst.Films, domain.Film{ID: domain.ContenuID(f + ":" + n), FournisseurID: f, Numero: n, Nom: nom,
			CategorieID: cat, Extension: "mkv", Note: 7.5, Annee: "2020", Ajoute: pris})
		inst.Series = append(inst.Series, domain.SerieTV{ID: domain.ContenuID(f + ":s" + n), FournisseurID: f, Numero: "s" + n, Nom: nom, CategorieID: cat})
	}
	return inst
}

// US-13 CA8 : filtre fournisseur, categorie, recherche sans accents ni casse,
// pagination stable, ErrNonTrouve, champs relus (films et series).
func TestStockageVideoFilmsEtSeries(t *testing.T) {
	ctx := context.Background()
	b := ouvrirTest(t)
	t0 := time.Date(2026, 3, 1, 5, 0, 0, 0, time.UTC)
	if err := b.EcrireCatalogue(ctx, catalogueVideo("a", t0)); err != nil {
		t.Fatal(err)
	}
	if err := b.EcrireCatalogue(ctx, catalogueVideo("b", t0)); err != nil {
		t.Fatal(err)
	}
	nomsF := func(fs []domain.Film) (r []string) {
		for _, f := range fs {
			r = append(r, string(f.ID)+"="+f.Nom)
		}
		return
	}
	// Meme categorie « 1 » chez deux fournisseurs : le filtre fournisseur les separe.
	fs, err := b.Films(ctx, domain.FiltreChaines{FournisseurID: "a", CategorieID: "1"})
	if err != nil || len(fs) != 2 || fs[0].FournisseurID != "a" || fs[1].FournisseurID != "a" {
		t.Fatalf("films a/1 = %v, %v", nomsF(fs), err)
	}
	if all, _ := b.Films(ctx, domain.FiltreChaines{}); len(all) != 8 {
		t.Fatalf("sans filtre : %d films", len(all))
	}
	// Recherche insensible a la casse et aux accents, tri stable, pagination.
	fs, _ = b.Films(ctx, domain.FiltreChaines{FournisseurID: "a", Recherche: "ECLAIR"})
	if len(fs) != 2 {
		t.Fatalf("recherche = %v", nomsF(fs))
	}
	p1, _ := b.Films(ctx, domain.FiltreChaines{FournisseurID: "a", Limite: 3})
	p2, _ := b.Films(ctx, domain.FiltreChaines{FournisseurID: "a", Limite: 3, Decalage: 3})
	if len(p1) != 3 || len(p2) != 1 || p1[0].Nom != "alpha" || p1[2].Nom != "Zèbre" || p2[0].Nom != "Éclair noir" { // tri NOCASE : ASCII seulement, un accent initial passe en fin
		t.Fatalf("pages = %v | %v", nomsF(p1), nomsF(p2))
	}
	if r, _ := b.Films(ctx, domain.FiltreChaines{Recherche: "***"}); len(r) != 0 {
		t.Fatal("recherche sans mot : rien attendu")
	}
	f, err := b.Film(ctx, "a:1")
	if err != nil || f.Nom != "Éclair noir" || f.Extension != "mkv" || f.Note != 7.5 || !f.Ajoute.Equal(t0) {
		t.Fatalf("film = %+v, %v", f, err)
	}
	if _, err := b.Film(ctx, "a:99"); !errors.Is(err, domain.ErrNonTrouve) {
		t.Fatalf("film absent : %v", err)
	}
	ss, _ := b.Series(ctx, domain.FiltreChaines{FournisseurID: "b", CategorieID: "1", Recherche: "zebre"})
	if len(ss) != 1 || ss[0].ID != "b:s3" {
		t.Fatalf("series = %+v", ss)
	}
	if s, err := b.Serie(ctx, "b:s3"); err != nil || s.Nom != "Zèbre" {
		t.Fatalf("serie = %+v, %v", s, err)
	}
	if _, err := b.Serie(ctx, "b:zz"); !errors.Is(err, domain.ErrNonTrouve) {
		t.Fatalf("serie absente : %v", err)
	}
	// Resynchronisation : l'ancien film n'est plus trouvable par la recherche.
	neuf := instantane("a", t0.Add(time.Hour))
	neuf.Films = []domain.Film{{ID: "a:9", FournisseurID: "a", Numero: "9", Nom: "Nouveau"}}
	b.EcrireCatalogue(ctx, neuf)
	if r, _ := b.Films(ctx, domain.FiltreChaines{FournisseurID: "a", Recherche: "eclair"}); len(r) != 0 {
		t.Fatalf("film remplace encore trouve : %v", nomsF(r))
	}
}
