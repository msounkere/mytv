package stockage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"mytv/internal/domain"
)

func version(t *testing.T, b *Base) string {
	t.Helper()
	v, err := b.VersionCatalogue(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func ecrire(t *testing.T, b *Base, s *domain.Instantane) {
	t.Helper()
	if err := b.EcrireCatalogue(context.Background(), s); err != nil {
		t.Fatal(err)
	}
}

// US-36 CA1 : la version depend du CONTENU, pas de l'heure : deux synchronisations
// identiques (a des heures differentes) gardent la meme version, une entree modifiee
// la change, et un simple reordonnancement du fournisseur ne la change pas.
func TestCA1_VersionDuCatalogueFondeeSurLeContenu(t *testing.T) {
	b := ouvrirTest(t)
	if v := version(t, b); v != "" {
		t.Fatalf("base vide : version %q, attendu vide", v)
	}
	ecrire(t, b, catalogueExport("a", time.Unix(1000, 0), 3, 2, 1))
	v1 := version(t, b)
	if len(v1) != 24 {
		t.Fatalf("version %q : 24 caracteres hexadecimaux attendus", v1)
	}
	ecrire(t, b, catalogueExport("a", time.Unix(5000, 0), 3, 2, 1)) // meme contenu, 4 s plus tard
	if v := version(t, b); v != v1 {
		t.Fatalf("meme contenu, autre heure : version %q, attendu %q", v, v1)
	}
	var n int
	if err := b.db.QueryRow(`SELECT COUNT(*) FROM catalogue_versions`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("une synchro sans changement ne doit pas ajouter de version : %d (%v)", n, err)
	}

	// Un film renomme : version differente.
	s := catalogueExport("a", time.Unix(6000, 0), 3, 2, 1)
	s.Films[1].Nom = "Film 1 (remasterise)"
	ecrire(t, b, s)
	v2 := version(t, b)
	if v2 == v1 {
		t.Fatal("entree modifiee : la version doit changer")
	}
	// Un logo EPG change (champ exporte) : version differente aussi.
	s = catalogueExport("a", time.Unix(7000, 0), 3, 2, 1)
	s.Chaines[0].Logo = "http://logo.test/nouveau.png"
	ecrire(t, b, s)
	if v := version(t, b); v == v1 || v == v2 {
		t.Fatal("logo modifie : la version doit changer")
	}
	// Retour au contenu initial, dans un autre ordre : version initiale retrouvee.
	s = catalogueExport("a", time.Unix(8000, 0), 3, 2, 1)
	s.Chaines[0], s.Chaines[2] = s.Chaines[2], s.Chaines[0]
	s.Films[0], s.Films[1] = s.Films[1], s.Films[0]
	ecrire(t, b, s)
	if v := version(t, b); v != v1 {
		t.Fatalf("contenu retabli (reordonne) : version %q, attendu %q", v, v1)
	}
}

// US-36 CA1 : un second fournisseur entre dans la version globale.
func TestCA1_VersionGlobaleCouvreTousLesFournisseurs(t *testing.T) {
	b := ouvrirTest(t)
	ecrire(t, b, catalogueExport("a", time.Unix(1, 0), 2, 1, 1))
	v1 := version(t, b)
	ecrire(t, b, catalogueExport("b", time.Unix(1, 0), 1, 1, 1))
	if v := version(t, b); v == v1 {
		t.Fatal("un second fournisseur doit changer la version globale")
	}
}

func differences(t *testing.T, b *Base, typ domain.TypeExport, live, depuis string) (ids []string, supprimes []string, err error) {
	t.Helper()
	e, supprimes, err := b.OuvrirDifferences(context.Background(), typ, live, depuis)
	if err != nil {
		return nil, nil, err
	}
	defer e.Fermer()
	err = e.Lire(func(v any) error {
		switch x := v.(type) {
		case domain.Chaine:
			ids = append(ids, string(x.ID))
		case domain.Film:
			ids = append(ids, string(x.ID))
		case domain.SerieTV:
			ids = append(ids, string(x.ID))
		}
		return nil
	})
	if err == nil && e.Elements() != len(ids) {
		t.Fatalf("Elements() = %d, %d lignes lues", e.Elements(), len(ids))
	}
	return ids, supprimes, err
}

// US-36 CA3 : depuis une version connue, seules les lignes ajoutees ou modifiees
// sont exportees, avec la liste des identifiants supprimes ; depuis la version
// courante, rien ; depuis une version inconnue, ErrNonTrouve.
func TestCA3_DifferencesDepuisUneVersionConnue(t *testing.T) {
	b := ouvrirTest(t)
	ecrire(t, b, catalogueExport("a", time.Unix(1, 0), 3, 3, 2))
	v1 := version(t, b)

	s := catalogueExport("a", time.Unix(2, 0), 3, 3, 2)
	s.Films[0].Nom = "Film 0 (version longue)" // modifie
	s.Films = s.Films[:2]                      // a:2002 supprime
	s.Films = append(s.Films, domain.Film{ID: "a:2009", FournisseurID: "a", Numero: "2009", Nom: "Nouveau", CategorieID: "1"})
	s.Chaines[1].Nom = "Chaine 1 HD"
	ecrire(t, b, s)
	v2 := version(t, b)

	ids, supprimes, err := differences(t, b, domain.ExportVOD, "", v1)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 || ids[0] != "a:2000" || ids[1] != "a:2009" {
		t.Errorf("films modifies/ajoutes = %v, attendu [a:2000 a:2009]", ids)
	}
	if len(supprimes) != 1 || supprimes[0] != "a:2002" {
		t.Errorf("films supprimes = %v, attendu [a:2002]", supprimes)
	}
	ids, supprimes, _ = differences(t, b, domain.ExportLive, "a", v1)
	if len(ids) != 1 || ids[0] != "a:1001" || len(supprimes) != 0 {
		t.Errorf("chaines : %v / %v, attendu [a:1001] / []", ids, supprimes)
	}
	ids, supprimes, _ = differences(t, b, domain.ExportSeries, "", v1)
	if len(ids) != 0 || len(supprimes) != 0 {
		t.Errorf("series inchangees : %v / %v", ids, supprimes)
	}
	// Depuis la version courante : rien.
	ids, supprimes, err = differences(t, b, domain.ExportVOD, "", v2)
	if err != nil || len(ids) != 0 || len(supprimes) != 0 {
		t.Errorf("depuis la version courante : %v / %v / %v", ids, supprimes, err)
	}
	// Version inconnue : ErrNonTrouve.
	if _, _, err := differences(t, b, domain.ExportVOD, "", "0123456789abcdef01234567"); !errors.Is(err, domain.ErrNonTrouve) {
		t.Errorf("version inconnue : %v, attendu ErrNonTrouve", err)
	}
	if _, _, err := differences(t, b, domain.ExportVOD, "", ""); !errors.Is(err, domain.ErrNonTrouve) {
		t.Errorf("version vide : %v, attendu ErrNonTrouve", err)
	}
	// Les differences se cumulent : une troisieme version, puis diff depuis la premiere.
	s = catalogueExport("a", time.Unix(3, 0), 3, 3, 2)
	s.Films = s.Films[:2] // a:2002 toujours absent ; a:2009 retire ; a:2000 retabli
	ecrire(t, b, s)
	ids, supprimes, _ = differences(t, b, domain.ExportVOD, "", v1)
	if len(ids) != 1 || ids[0] != "a:2000" || len(supprimes) != 2 || supprimes[0] != "a:2002" || supprimes[1] != "a:2009" {
		t.Errorf("cumul depuis v1 : %v / %v, attendu [a:2000] / [a:2002 a:2009]", ids, supprimes)
	}
	ids, supprimes, _ = differences(t, b, domain.ExportVOD, "", v2)
	if len(ids) != 1 || ids[0] != "a:2000" || len(supprimes) != 1 || supprimes[0] != "a:2009" {
		t.Errorf("depuis v2 : %v / %v, attendu [a:2000] / [a:2009]", ids, supprimes)
	}
	// Les versions connues : v1, v2 et la courante, dans l'ordre.
	vs, err := b.VersionsConnues(context.Background())
	if err != nil || len(vs) != 3 || vs[0] != v1 || vs[1] != v2 || vs[2] != version(t, b) {
		t.Errorf("versions connues = %v (%v)", vs, err)
	}
}

// US-36 CA3 : l'historique est borne (catalogue.historique_versions) : une version
// plus ancienne que les N gardees devient inconnue, et ses differences sont purgees.
func TestCA3_HistoriqueDesVersionsBorne(t *testing.T) {
	ctx := context.Background()
	b, err := OuvrirAvec(ctx, filepath.Join(t.TempDir(), "h.db"), Options{HistoriqueVersions: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Fermer()
	var versions []string
	for i := 0; i < 4; i++ {
		s := catalogueExport("a", time.Unix(int64(i), 0), 2, 2, 1)
		s.Films[0].Nom = "Film " + string(rune('A'+i))
		ecrire(t, b, s)
		versions = append(versions, version(t, b))
	}
	vs, _ := b.VersionsConnues(ctx)
	if len(vs) != 2 || vs[0] != versions[2] || vs[1] != versions[3] {
		t.Fatalf("versions gardees = %v, attendu les deux dernieres %v", vs, versions[2:])
	}
	if _, _, err := differences(t, b, domain.ExportVOD, "", versions[1]); !errors.Is(err, domain.ErrNonTrouve) {
		t.Errorf("version purgee : %v, attendu ErrNonTrouve", err)
	}
	ids, _, err := differences(t, b, domain.ExportVOD, "", versions[2])
	if err != nil || len(ids) != 1 || ids[0] != "a:2000" {
		t.Errorf("depuis l'avant-derniere : %v %v", ids, err)
	}
	var n int
	if err := b.db.QueryRow(`SELECT COUNT(*) FROM catalogue_differences WHERE numero <= (SELECT MIN(numero) FROM catalogue_versions)`).Scan(&n); err != nil || n != 0 {
		t.Errorf("differences non purgees : %d (%v)", n, err)
	}
}

// US-36 CA2 : effectifs par fournisseur (la garde de synchronisation les compare).
func TestCA2_EffectifsParFournisseur(t *testing.T) {
	b := ouvrirTest(t)
	ecrire(t, b, catalogueExport("a", time.Unix(1, 0), 3, 2, 1))
	ecrire(t, b, catalogueExport("b", time.Unix(1, 0), 1, 0, 4))
	e, err := b.Effectifs(context.Background(), "a")
	if err != nil || e[domain.TypeLive] != 3 || e[domain.TypeFilm] != 2 || e[domain.TypeSerie] != 1 {
		t.Errorf("effectifs de a = %v (%v)", e, err)
	}
	e, _ = b.Effectifs(context.Background(), "b")
	if e[domain.TypeLive] != 1 || e[domain.TypeFilm] != 0 || e[domain.TypeSerie] != 4 {
		t.Errorf("effectifs de b = %v", e)
	}
	e, _ = b.Effectifs(context.Background(), "inconnu")
	if e[domain.TypeLive] != 0 || e[domain.TypeFilm] != 0 || e[domain.TypeSerie] != 0 {
		t.Errorf("effectifs d'un fournisseur inconnu = %v", e)
	}
}
