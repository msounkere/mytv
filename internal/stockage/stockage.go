// Package stockage persiste le catalogue dans SQLite (pur Go, sans cgo).
//
// Choix structurants :
//   - mode WAL : les lectures (parcours du catalogue par les clients) ne bloquent
//     pas l'ecriture quotidienne de plusieurs dizaines de milliers de lignes ;
//   - UN seul ecrivain a la fois (verrou Go) : SQLite n'en admet qu'un, et le
//     serialiser chez nous donne une attente propre (annulable par ctx) au lieu
//     d'une erreur « database is locked » ;
//   - le catalogue d'un fournisseur est remplace dans UNE transaction : un echec
//     laisse l'ancien catalogue intact.
package stockage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"

	_ "modernc.org/sqlite" // pilote "sqlite"

	"mytv/internal/domain"
)

// ErrNonTrouve : l'element demande n'existe pas dans le catalogue. Alias du contrat
// commun, pour que les appelants testent errors.Is(err, domain.ErrNonTrouve).
var ErrNonTrouve = domain.ErrNonTrouve

// LimiteParDefaut s'applique quand FiltreChaines.Limite vaut 0.
const LimiteParDefaut = 200

var _ domain.Stockage = (*Base)(nil)

// Base est un domain.Stockage adosse a un fichier SQLite.
type Base struct {
	db *sql.DB
	// ecrivain serialise les ecritures (voir la documentation du paquet).
	ecrivain sync.Mutex
	// apresInsertion, si non nil, est appele apres chaque table inseree : permet
	// aux tests d'injecter une panne au MILIEU d'une ecriture (CA2).
	apresInsertion func(table string) error
}

// Ouvrir ouvre (et cree au besoin) la base, puis applique les migrations.
func Ouvrir(ctx context.Context, chemin string) (*Base, error) {
	q := url.Values{}
	// Les pragmas sont passes dans l'adresse pour s'appliquer a CHAQUE connexion
	// du pool, pas seulement a la premiere.
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(10000)") // filet : attente d'un verrou externe (autre processus)
	q.Add("_pragma", "synchronous(NORMAL)") // sur en WAL, bien plus rapide que FULL
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_txlock", "immediate") // ecrivain : verrou pris des BEGIN, pas de promotion qui echoue
	db, err := sql.Open("sqlite", "file:"+chemin+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("stockage : ouverture : %w", err)
	}
	b := &Base{db: db}
	if err := b.migrer(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return b, nil
}

func (b *Base) Fermer() error { return b.db.Close() }

// migrer applique les migrations manquantes. Rejouable : ne fait rien si tout est a jour.
func (b *Base) migrer(ctx context.Context) error {
	b.ecrivain.Lock()
	defer b.ecrivain.Unlock()
	if _, err := b.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS migrations (
		version INTEGER PRIMARY KEY, nom TEXT NOT NULL, appliquee INTEGER NOT NULL) STRICT`); err != nil {
		return fmt.Errorf("stockage : table migrations : %w", err)
	}
	var courante int
	if err := b.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM migrations`).Scan(&courante); err != nil {
		return fmt.Errorf("stockage : version du schema : %w", err)
	}
	if derniere := migrations[len(migrations)-1].version; courante > derniere {
		return fmt.Errorf("stockage : la base est en version %d, plus recente que ce programme (%d)", courante, derniere)
	}
	for _, m := range migrations {
		if m.version <= courante {
			continue
		}
		if err := b.appliquer(ctx, m); err != nil {
			return fmt.Errorf("stockage : migration %d (%s) : %w", m.version, m.nom, err)
		}
	}
	return nil
}

func (b *Base) appliquer(ctx context.Context, m migration) error {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Un autre processus a pu appliquer la migration entre-temps : on revérifie sous verrou.
	var deja int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM migrations WHERE version = ?`, m.version).Scan(&deja); err != nil {
		return err
	}
	if deja > 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO migrations (version, nom, appliquee) VALUES (?, ?, ?)`,
		m.version, m.nom, time.Now().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

func millis(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UnixMilli()
}

func depuisMillis(n sql.NullInt64) time.Time {
	if !n.Valid {
		return time.Time{}
	}
	return time.UnixMilli(n.Int64).UTC()
}

// EcrireCatalogue remplace le catalogue du fournisseur s.FournisseurID.
// Tout est valide avant de toucher a la base : un element qui appartiendrait a un
// autre fournisseur est une erreur de programmation, pas une donnee a stocker.
func (b *Base) EcrireCatalogue(ctx context.Context, s *domain.Instantane) error {
	if s == nil || s.FournisseurID == "" {
		return errors.New("stockage : instantane sans fournisseur")
	}
	if err := verifier(s); err != nil {
		return err
	}
	b.ecrivain.Lock()
	defer b.ecrivain.Unlock()
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("stockage : debut de transaction : %w", err)
	}
	// Sans Commit abouti, Rollback restitue l'ancien catalogue.
	defer tx.Rollback()
	if err := b.remplacer(ctx, tx, s); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("stockage : validation : %w", err)
	}
	return nil
}

func verifier(s *domain.Instantane) error {
	f := s.FournisseurID
	for _, c := range s.Categories {
		if c.FournisseurID != f {
			return fmt.Errorf("stockage : categorie %q d'un autre fournisseur", c.ID)
		}
		switch c.Type {
		case domain.TypeLive, domain.TypeFilm, domain.TypeSerie:
		default:
			return fmt.Errorf("stockage : categorie %q de type inconnu %q", c.ID, c.Type)
		}
	}
	for _, c := range s.Chaines {
		if c.FournisseurID != f {
			return fmt.Errorf("stockage : chaine %q d'un autre fournisseur", c.ID)
		}
	}
	for _, c := range s.Films {
		if c.FournisseurID != f {
			return fmt.Errorf("stockage : film %q d'un autre fournisseur", c.ID)
		}
	}
	for _, c := range s.Series {
		if c.FournisseurID != f {
			return fmt.Errorf("stockage : serie %q d'un autre fournisseur", c.ID)
		}
	}
	return nil
}

func (b *Base) remplacer(ctx context.Context, tx *sql.Tx, s *domain.Instantane) error {
	f := s.FournisseurID
	for _, t := range []string{"categories", "chaines", "films", "series"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+t+` WHERE fournisseur_id = ?`, f); err != nil {
			return fmt.Errorf("stockage : purge %s : %w", t, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO synchros (fournisseur_id, pris) VALUES (?, ?)
		ON CONFLICT(fournisseur_id) DO UPDATE SET pris = excluded.pris`, f, millis(s.Pris)); err != nil {
		return fmt.Errorf("stockage : synchro : %w", err)
	}

	etapes := []struct {
		table string
		sql   string
		n     int
		ligne func(i int) []any
	}{
		{"categories", `INSERT INTO categories (fournisseur_id, type, id, nom, ordre) VALUES (?,?,?,?,?)`,
			len(s.Categories), func(i int) []any {
				c := s.Categories[i]
				return []any{c.FournisseurID, string(c.Type), c.ID, c.Nom, c.Ordre}
			}},
		{"chaines", `INSERT INTO chaines (id, fournisseur_id, numero, nom, logo, epg_id, categorie_id, numero_chaine, ordre) VALUES (?,?,?,?,?,?,?,?,?)`,
			len(s.Chaines), func(i int) []any {
				c := s.Chaines[i]
				return []any{string(c.ID), c.FournisseurID, c.Numero, c.Nom, c.Logo, c.EpgID, c.CategorieID, c.NumeroChaine, c.Ordre}
			}},
		{"films", `INSERT INTO films (id, fournisseur_id, numero, nom, image, categorie_id, extension, note, annee, ajoute) VALUES (?,?,?,?,?,?,?,?,?,?)`,
			len(s.Films), func(i int) []any {
				c := s.Films[i]
				return []any{string(c.ID), c.FournisseurID, c.Numero, c.Nom, c.Image, c.CategorieID, c.Extension, c.Note, c.Annee, millis(c.Ajoute)}
			}},
		{"series", `INSERT INTO series (id, fournisseur_id, numero, nom, image, categorie_id, note, annee) VALUES (?,?,?,?,?,?,?,?)`,
			len(s.Series), func(i int) []any {
				c := s.Series[i]
				return []any{string(c.ID), c.FournisseurID, c.Numero, c.Nom, c.Image, c.CategorieID, c.Note, c.Annee}
			}},
	}
	for _, e := range etapes {
		if err := inserer(ctx, tx, e.sql, e.n, e.ligne); err != nil {
			return fmt.Errorf("stockage : ecriture %s : %w", e.table, err)
		}
		if b.apresInsertion != nil {
			if err := b.apresInsertion(e.table); err != nil {
				return err
			}
		}
	}
	return nil
}

func inserer(ctx context.Context, tx *sql.Tx, requete string, n int, ligne func(int) []any) error {
	if n == 0 {
		return nil
	}
	st, err := tx.PrepareContext(ctx, requete)
	if err != nil {
		return err
	}
	defer st.Close()
	for i := 0; i < n; i++ {
		if _, err := st.ExecContext(ctx, ligne(i)...); err != nil {
			return err
		}
	}
	return nil
}

// DerniereSynchro renvoie l'instant de l'instantane ecrit, ou le zero value.
func (b *Base) DerniereSynchro(ctx context.Context, fournisseurID string) (time.Time, error) {
	var n sql.NullInt64
	err := b.db.QueryRowContext(ctx, `SELECT pris FROM synchros WHERE fournisseur_id = ?`, fournisseurID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("stockage : derniere synchro : %w", err)
	}
	return depuisMillis(n), nil
}

const colonnesChaine = `c.id, c.fournisseur_id, c.numero, c.nom, c.logo, c.epg_id, c.categorie_id, c.numero_chaine, c.ordre`

type scanner interface{ Scan(...any) error }

func lireChaine(r scanner) (domain.Chaine, error) {
	var c domain.Chaine
	var id string
	err := r.Scan(&id, &c.FournisseurID, &c.Numero, &c.Nom, &c.Logo, &c.EpgID, &c.CategorieID, &c.NumeroChaine, &c.Ordre)
	c.ID = domain.ContenuID(id)
	return c, err
}

// requeteFTS transforme le texte libre de l'utilisateur en requete FTS5 sure :
// chaque mot devient une chaine entre guillemets avec prefixe (« fra » trouve
// « France 2 ») et les mots sont combines en ET. Les guillemets et operateurs
// saisis (NEAR, -, *, :) ne sont donc jamais interpretes. Faux si aucun mot.
func requeteFTS(texte string) (string, bool) {
	mots := strings.FieldsFunc(texte, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	if len(mots) == 0 {
		return "", false
	}
	for i, m := range mots {
		mots[i] = `"` + m + `"*`
	}
	return strings.Join(mots, " "), true
}

// Chaines liste les chaines, triees par ordre puis par id (stable : la pagination
// ne duplique ni ne saute de ligne entre deux pages).
func (b *Base) Chaines(ctx context.Context, f domain.FiltreChaines) ([]domain.Chaine, error) {
	var (
		cond []string
		args []any
	)
	if f.CategorieID != "" {
		cond = append(cond, `c.categorie_id = ?`)
		args = append(args, f.CategorieID)
	}
	if strings.TrimSpace(f.Recherche) != "" {
		q, ok := requeteFTS(f.Recherche)
		if !ok {
			return []domain.Chaine{}, nil // du texte sans lettre ni chiffre ne peut rien trouver
		}
		cond = append(cond, `c.rowid IN (SELECT rowid FROM chaines_fts WHERE chaines_fts MATCH ?)`)
		args = append(args, q)
	}
	limite := f.Limite
	if limite <= 0 {
		limite = LimiteParDefaut
	}
	decalage := max(f.Decalage, 0)
	req := `SELECT ` + colonnesChaine + ` FROM chaines c`
	if len(cond) > 0 {
		req += ` WHERE ` + strings.Join(cond, ` AND `)
	}
	req += ` ORDER BY c.ordre, c.id LIMIT ? OFFSET ?`
	args = append(args, limite, decalage)

	rows, err := b.db.QueryContext(ctx, req, args...)
	if err != nil {
		return nil, fmt.Errorf("stockage : chaines : %w", err)
	}
	defer rows.Close()
	res := []domain.Chaine{}
	for rows.Next() {
		c, err := lireChaine(rows)
		if err != nil {
			return nil, fmt.Errorf("stockage : chaines : %w", err)
		}
		res = append(res, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("stockage : chaines : %w", err)
	}
	return res, nil
}

// Categories liste les categories d'un type, par ordre puis fournisseur puis id.
func (b *Base) Categories(ctx context.Context, t domain.Type) ([]domain.Categorie, error) {
	rows, err := b.db.QueryContext(ctx, `SELECT fournisseur_id, type, id, nom, ordre FROM categories
		WHERE type = ? ORDER BY ordre, fournisseur_id, id`, string(t))
	if err != nil {
		return nil, fmt.Errorf("stockage : categories : %w", err)
	}
	defer rows.Close()
	res := []domain.Categorie{}
	for rows.Next() {
		var c domain.Categorie
		var typ string
		if err := rows.Scan(&c.FournisseurID, &typ, &c.ID, &c.Nom, &c.Ordre); err != nil {
			return nil, fmt.Errorf("stockage : categories : %w", err)
		}
		c.Type = domain.Type(typ)
		res = append(res, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("stockage : categories : %w", err)
	}
	return res, nil
}

// Chaine renvoie une chaine, ou nil et ErrNonTrouve.
func (b *Base) Chaine(ctx context.Context, id domain.ContenuID) (*domain.Chaine, error) {
	c, err := lireChaine(b.db.QueryRowContext(ctx, `SELECT `+colonnesChaine+` FROM chaines c WHERE c.id = ?`, string(id)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNonTrouve
	}
	if err != nil {
		return nil, fmt.Errorf("stockage : chaine : %w", err)
	}
	return &c, nil
}
