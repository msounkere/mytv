package sauvegarde

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// baseEnVol ouvre une base en WAL comme le fait le service (plusieurs connexions).
func baseEnVol(t *testing.T) (*sql.DB, string) {
	t.Helper()
	chemin := filepath.Join(t.TempDir(), "mytv.db")
	db, err := sql.Open("sqlite", "file:"+chemin+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(10000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	for _, q := range []string{
		`CREATE TABLE a (id INTEGER PRIMARY KEY, v TEXT)`,
		`CREATE TABLE b (id INTEGER PRIMARY KEY, v TEXT)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	return db, chemin
}

func compter(t *testing.T, db *sql.DB, table string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func ouvrirCopie(t *testing.T, fichier string) *sql.DB {
	t.Helper()
	db, err := OuvrirLectureSeule(fichier)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// CA1 : la copie est un instantane COHERENT pris pendant que le service ecrit sans cesse
// (deux tables remplies dans une meme transaction ont toujours le meme nombre de lignes),
// elle passe integrity_check, a les droits 0600 et ne laisse aucun fichier temporaire.
func TestCopieCoherenteSansArreterLesEcritures(t *testing.T) {
	db, chemin := baseEnVol(t)
	ctx := context.Background()
	var stop atomic.Bool
	var ecrits atomic.Int64
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; !stop.Load(); i++ {
			tx, err := db.Begin()
			if err != nil {
				return
			}
			tx.Exec(`INSERT INTO a (v) VALUES (?)`, strings.Repeat("x", 200))
			tx.Exec(`INSERT INTO b (v) VALUES (?)`, strings.Repeat("y", 200))
			if tx.Commit() == nil {
				ecrits.Add(1)
			}
		}
	}()
	t.Cleanup(func() { stop.Store(true); wg.Wait() })
	for ecrits.Load() < 200 {
		time.Sleep(time.Millisecond)
	}

	dest := filepath.Join(t.TempDir(), "copie.db")
	avant := ecrits.Load()
	octets, err := Copier(ctx, db, dest)
	if err != nil {
		t.Fatal(err)
	}
	apres := ecrits.Load()
	stop.Store(true)
	wg.Wait()

	if octets <= 0 {
		t.Errorf("taille : %d", octets)
	}
	st, err := os.Stat(dest)
	if err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("droits : %v %v", st, err)
	}
	if _, err := os.Stat(dest + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("fichier temporaire laisse : %v", err)
	}
	copie := ouvrirCopie(t, dest)
	na, nb := compter(t, copie, "a"), compter(t, copie, "b")
	if na != nb {
		t.Fatalf("instantane incoherent : a=%d b=%d", na, nb)
	}
	if int64(na) < avant || int64(na) > apres {
		t.Errorf("l'instantane (%d lignes) doit etre entre %d et %d", na, avant, apres)
	}
	if err := Verifier(ctx, dest); err != nil {
		t.Errorf("integrity_check : %v", err)
	}
	// Une copie de fichier brute du .db seul, pendant les ecritures, n'aurait pas cette garantie :
	// c'est pourquoi la sauvegarde passe par la base (voir la documentation du paquet).
	if _, err := os.Stat(chemin + "-wal"); err != nil {
		t.Logf("pas de -wal (checkpoint deja fait) : %v", err)
	}
}

// CA1 : on n'ecrase jamais une sauvegarde existante.
func TestCopierRefuseDEcraser(t *testing.T) {
	db, _ := baseEnVol(t)
	dest := filepath.Join(t.TempDir(), "copie.db")
	if err := os.WriteFile(dest, []byte("precieux"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Copier(context.Background(), db, dest); !errors.Is(err, ErrExiste) {
		t.Fatalf("attendu ErrExiste : %v", err)
	}
	if b, _ := os.ReadFile(dest); string(b) != "precieux" {
		t.Error("le fichier existant a ete modifie")
	}
}

// CA1 : un fichier abime est reconnu par la verification d'integrite.
func TestVerifierDetecteUneCopieAbimee(t *testing.T) {
	db, _ := baseEnVol(t)
	for range 300 {
		db.Exec(`INSERT INTO a (v) VALUES (?)`, strings.Repeat("z", 500))
	}
	dest := filepath.Join(t.TempDir(), "copie.db")
	if _, err := Copier(context.Background(), db, dest); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(dest)
	for p := 1; p < len(b)/4096; p++ { // l'en-tete de chaque page de b-tree detruit (type, cellules)
		for i := p * 4096; i < p*4096+8; i++ {
			b[i] = 0xFF
		}
	}
	if err := os.WriteFile(dest, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Verifier(context.Background(), dest); err == nil {
		t.Error("un fichier abime a passe la verification")
	}
	// Un fichier qui n'est pas une base non plus.
	if err := os.WriteFile(dest, bytes.Repeat([]byte("n'importe quoi "), 500), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Verifier(context.Background(), dest); err == nil {
		t.Error("un fichier quelconque a passe la verification")
	}
}

// CA1 : une copie qui echoue a integrity_check n'est JAMAIS publiee sous son nom : ni destination,
// ni fichier temporaire (on ne prendrait pas une mauvaise sauvegarde pour une bonne).
func TestCopieCorrompueNEstPasPubliee(t *testing.T) {
	db, _ := baseEnVol(t)
	for range 300 {
		db.Exec(`INSERT INTO a (v) VALUES (?)`, strings.Repeat("z", 500))
	}
	apresCopie = func(tmp string) {
		b, _ := os.ReadFile(tmp)
		for p := 1; p < len(b)/4096; p++ {
			for i := p * 4096; i < p*4096+8; i++ {
				b[i] = 0xFF
			}
		}
		os.WriteFile(tmp, b, 0o600)
	}
	defer func() { apresCopie = nil }()
	dossier := t.TempDir()
	dest := filepath.Join(dossier, "copie.db")
	if _, err := Copier(context.Background(), db, dest); !errors.Is(err, ErrCorrompue) {
		t.Fatalf("attendu ErrCorrompue : %v", err)
	}
	if got := noms(t, dossier); len(got) != 0 {
		t.Errorf("fichiers laisses : %v", got)
	}
}

// CA2 : mytv --sauvegarder ouvre la base en lecture seule : ne la cree pas, ne la migre pas.
func TestOuvrirLectureSeule(t *testing.T) {
	if _, err := OuvrirLectureSeule(filepath.Join(t.TempDir(), "absente.db")); err == nil {
		t.Error("une base absente ne doit pas etre creee")
	}
	db, chemin := baseEnVol(t)
	db.Exec(`INSERT INTO a (v) VALUES ('1')`)
	ro := ouvrirCopie(t, chemin)
	if _, err := ro.Exec(`INSERT INTO a (v) VALUES ('2')`); err == nil {
		t.Error("l'ouverture en lecture seule a accepte une ecriture")
	}
	if compter(t, ro, "a") != 1 {
		t.Error("lecture")
	}
}

// ---------------------------------------------------------------- planification

type horlogeTest struct {
	mu sync.Mutex
	t  time.Time
}

func (h *horlogeTest) now() time.Time { h.mu.Lock(); defer h.mu.Unlock(); return h.t }
func (h *horlogeTest) avance(d time.Duration) {
	h.mu.Lock()
	h.t = h.t.Add(d)
	h.mu.Unlock()
}

type tampon struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (t *tampon) Write(p []byte) (int, error) { t.mu.Lock(); defer t.mu.Unlock(); return t.b.Write(p) }
func (t *tampon) String() string              { t.mu.Lock(); defer t.mu.Unlock(); return t.b.String() }

func copieur(t *testing.T) func(context.Context, string) (int64, error) {
	db, _ := baseEnVol(t)
	db.Exec(`INSERT INTO a (v) VALUES ('1')`)
	return func(ctx context.Context, dest string) (int64, error) { return Copier(ctx, db, dest) }
}

func noms(t *testing.T, dossier string) []string {
	t.Helper()
	es, err := os.ReadDir(dossier)
	if err != nil {
		t.Fatal(err)
	}
	var r []string
	for _, e := range es {
		r = append(r, e.Name())
	}
	return r
}

// CA1 : fichier horodate (UTC), dossier en 700 meme s'il existait plus ouvert, journal.
func TestSauvegarderEcritUnFichierHorodateDansUnDossierPrive(t *testing.T) {
	dossier := filepath.Join(t.TempDir(), "sv")
	if err := os.Mkdir(dossier, 0o755); err != nil { // existant et trop ouvert
		t.Fatal(err)
	}
	h := &horlogeTest{t: time.Date(2026, 10, 15, 3, 30, 0, 0, time.UTC)}
	j := &tampon{}
	p, err := Nouveau(Options{Reglages: Reglages{Dossier: dossier, Conservation: 14}, Copier: copieur(t), Maintenant: h.now,
		Journal: slog.New(slog.NewJSONHandler(j, nil))})
	if err != nil {
		t.Fatal(err)
	}
	f, err := p.Sauvegarder(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(f) != "mytv-20261015T033000Z.db" {
		t.Errorf("nom : %s", f)
	}
	if st, _ := os.Stat(dossier); st.Mode().Perm() != 0o700 {
		t.Errorf("droits du dossier : %v", st.Mode().Perm())
	}
	if st, _ := os.Stat(f); st.Mode().Perm() != 0o600 {
		t.Errorf("droits du fichier : %v", st.Mode().Perm())
	}
	if !strings.Contains(j.String(), `"msg":"sauvegarde ecrite"`) || strings.Contains(j.String(), dossier) {
		t.Errorf("journal (le chemin complet n'a pas a y etre) :\n%s", j.String())
	}
}

// CA1 : conservation en jours ; la plus recente est toujours gardee ; rien d'autre n'est touche.
func TestConservationPurgeLesVieillesSauvegardesSeulement(t *testing.T) {
	dossier := t.TempDir()
	h := &horlogeTest{t: time.Date(2026, 10, 15, 3, 30, 0, 0, time.UTC)}
	for _, n := range []string{
		"mytv-20261014T033000Z.db", // 1 jour : gardee
		"mytv-20261002T033000Z.db", // 13 jours : gardee
		"mytv-20261001T033000Z.db", // 14 jours pile : gardee (limite incluse)
		"mytv-20260930T033000Z.db", // 15 jours : supprimee
		"mytv-20260101T033000Z.db", // tres vieille : supprimee
		"notes.txt", "mytv-manuelle.db", "mytv-20260101T033000Z.db.bak",
	} {
		if err := os.WriteFile(filepath.Join(dossier, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p, _ := Nouveau(Options{Reglages: Reglages{Dossier: dossier, Conservation: 14}, Copier: copieur(t), Maintenant: h.now})
	if _, err := p.Sauvegarder(context.Background()); err != nil {
		t.Fatal(err)
	}
	attendu := "mytv-20260101T033000Z.db.bak mytv-20261001T033000Z.db mytv-20261002T033000Z.db mytv-20261014T033000Z.db mytv-20261015T033000Z.db mytv-manuelle.db notes.txt"
	if got := strings.Join(noms(t, dossier), " "); got != attendu {
		t.Errorf("dossier apres purge :\n got %s\nwant %s", got, attendu)
	}
}

// CA1 : si plus rien ne se sauvegarde, la derniere sauvegarde reste, meme tres vieille.
func TestLaPlusRecenteResteToujours(t *testing.T) {
	dossier := t.TempDir()
	h := &horlogeTest{t: time.Date(2026, 10, 15, 3, 30, 0, 0, time.UTC)}
	os.WriteFile(filepath.Join(dossier, "mytv-20250101T000000Z.db"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(dossier, "mytv-20250102T000000Z.db"), []byte("x"), 0o600)
	p, _ := Nouveau(Options{Reglages: Reglages{Dossier: dossier, Conservation: 1}, Copier: func(context.Context, string) (int64, error) {
		return 0, errors.New("disque plein")
	}, Maintenant: h.now})
	p.purger(h.now())
	if got := strings.Join(noms(t, dossier), " "); got != "mytv-20250102T000000Z.db" {
		t.Errorf("la plus recente doit rester : %s", got)
	}
}

// CA1 : un echec est journalise, compte et signale (webhook) ; la purge ne tourne pas ;
// aucun fichier partiel ne reste.
func TestEchecJournaliseEtSignale(t *testing.T) {
	dossier := t.TempDir()
	vieux := filepath.Join(dossier, "mytv-20200101T000000Z.db")
	os.WriteFile(vieux, []byte("x"), 0o600)
	os.WriteFile(filepath.Join(dossier, "mytv-20200102T000000Z.db"), []byte("x"), 0o600)
	h := &horlogeTest{t: time.Date(2026, 10, 15, 3, 30, 0, 0, time.UTC)}
	j := &tampon{}
	var signale []error
	p, _ := Nouveau(Options{Reglages: Reglages{Dossier: dossier, Conservation: 1},
		Copier:     func(context.Context, string) (int64, error) { return 0, errors.New("disque plein") },
		Maintenant: h.now, Journal: slog.New(slog.NewJSONHandler(j, nil)), EnEchec: func(err error) { signale = append(signale, err) }})
	if _, err := p.Sauvegarder(context.Background()); err == nil {
		t.Fatal("echec attendu")
	}
	if len(signale) != 1 || !strings.Contains(j.String(), `"level":"ERROR"`) || !strings.Contains(j.String(), "disque plein") {
		t.Errorf("signalements %d, journal :\n%s", len(signale), j.String())
	}
	if _, err := os.Stat(vieux); err != nil {
		t.Error("l'ancienne sauvegarde ne doit pas etre purgee apres un echec : on n'a plus rien d'autre")
	}
	// Un arret demande n'est pas une panne.
	ctx, annuler := context.WithCancel(context.Background())
	annuler()
	p.o.Copier = func(ctx context.Context, _ string) (int64, error) { return 0, ctx.Err() }
	p.Sauvegarder(ctx)
	if len(signale) != 1 {
		t.Errorf("l'arret a ete signale comme une panne")
	}
}

// CA1 : planifiee chaque jour a l'heure dite (horloge injectee, aucune attente reelle).
func TestPlanifieeChaqueJourALHeure(t *testing.T) {
	dossier := t.TempDir()
	lieu := time.FixedZone("test", 2*3600) // 03:30 locale = 01:30 UTC
	h := &horlogeTest{t: time.Date(2026, 10, 15, 0, 10, 0, 0, time.UTC)}
	ctx, annuler := context.WithCancel(context.Background())
	defer annuler()
	var attentes []time.Duration
	var nb int
	p, err := Nouveau(Options{Reglages: Reglages{Heure: "03:30", Dossier: dossier, Conservation: 14}, Lieu: lieu, Copier: copieur(t), Maintenant: h.now,
		Attendre: func(ctx context.Context, d time.Duration) bool {
			attentes = append(attentes, d)
			nb++
			if nb > 3 {
				annuler()
				return false
			}
			h.avance(d) // l'horloge saute jusqu'a l'echeance
			return true
		}})
	if err != nil {
		t.Fatal(err)
	}
	p.Planifier(ctx)
	if len(attentes) != 4 || attentes[0] != 80*time.Minute || attentes[1] != 24*time.Hour || attentes[2] != 24*time.Hour {
		t.Errorf("attentes : %v", attentes)
	}
	want := "mytv-20261015T013000Z.db mytv-20261016T013000Z.db mytv-20261017T013000Z.db"
	if got := strings.Join(noms(t, dossier), " "); got != want {
		t.Errorf("sauvegardes :\n got %s\nwant %s", got, want)
	}
	// Sans heure : pas de planification, retour immediat.
	q, _ := Nouveau(Options{Reglages: Reglages{Dossier: dossier, Conservation: 14}, Copier: copieur(t)})
	fini := make(chan struct{})
	go func() { q.Planifier(context.Background()); close(fini) }()
	select {
	case <-fini:
	case <-time.After(2 * time.Second):
		t.Error("Planifier sans heure doit rendre la main")
	}
}

func TestNouveauRefuseDesReglagesInvalides(t *testing.T) {
	ok := func(context.Context, string) (int64, error) { return 0, nil }
	for nom, o := range map[string]Options{
		"sans copie":     {Reglages: Reglages{Dossier: "x", Conservation: 1}},
		"sans dossier":   {Reglages: Reglages{Conservation: 1}, Copier: ok},
		"conservation 0": {Reglages: Reglages{Dossier: "x"}, Copier: ok},
		"heure invalide": {Reglages: Reglages{Dossier: "x", Conservation: 1, Heure: "demain"}, Copier: ok},
	} {
		if _, err := Nouveau(o); err == nil {
			t.Errorf("%s : accepte", nom)
		}
	}
	_ = fmt.Sprint()
}
