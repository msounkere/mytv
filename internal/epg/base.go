package epg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"mytv/internal/basesqlite"
	"mytv/internal/domain"
)

var migrations = []basesqlite.Migration{
	{Version: 1, Nom: "epg initial", SQL: `
CREATE TABLE epg_synchros (
	fournisseur_id TEXT PRIMARY KEY,
	pris           INTEGER NOT NULL            -- millisecondes Unix UTC
) STRICT;

-- La cle (fournisseur, canal, debut) rend les doublons de guide impossibles et
-- sert directement les requetes « en cours / suivant / grille » d'une chaine.
CREATE TABLE epg_programmes (
	fournisseur_id TEXT NOT NULL,
	epg_id         TEXT NOT NULL,
	debut          INTEGER NOT NULL,           -- millisecondes Unix UTC
	fin            INTEGER NOT NULL,
	titre          TEXT NOT NULL,
	description    TEXT NOT NULL,
	categorie      TEXT NOT NULL,
	PRIMARY KEY (fournisseur_id, epg_id, debut)
) WITHOUT ROWID, STRICT;
CREATE INDEX epg_programmes_fin ON epg_programmes (fin);

CREATE TABLE epg_rapprochements (
	chaine_id      TEXT PRIMARY KEY,
	fournisseur_id TEXT NOT NULL,
	epg_id         TEXT NOT NULL,              -- vide si ambigu
	methode        TEXT NOT NULL,
	ambigu         INTEGER NOT NULL
) STRICT;
CREATE INDEX epg_rapprochements_fournisseur ON epg_rapprochements (fournisseur_id);
`},
}

var _ domain.EPGStockage = (*Base)(nil)

// Base est un domain.EPGStockage adosse a SQLite.
type Base struct {
	db *basesqlite.Base
	// apresInsertion, si non nil, est appele apres chaque table ecrite : permet
	// aux tests d'injecter une panne au milieu d'une ecriture (CA4).
	apresInsertion func(table string) error
	maintenant     func() time.Time
}

// Ouvrir ouvre (et migre) la base EPG dans le fichier donne. Le fichier peut etre
// celui du catalogue : les tables sont distinctes et la version du schema EPG
// a sa propre table.
func Ouvrir(ctx context.Context, chemin string, h domain.Horloge) (*Base, error) {
	maintenant := time.Now
	if h != nil {
		maintenant = h.Maintenant
	}
	db, err := basesqlite.Ouvrir(ctx, chemin, "epg", migrations, maintenant)
	if err != nil {
		return nil, err
	}
	return &Base{db: db, maintenant: maintenant}, nil
}

func (b *Base) Fermer() error { return b.db.Close() }

func ms(t time.Time) int64       { return t.UTC().UnixMilli() }
func depuisMs(n int64) time.Time { return time.UnixMilli(n).UTC() }

// EcrireEPG remplace atomiquement l'EPG du fournisseur.
func (b *Base) EcrireEPG(ctx context.Context, e *domain.InstantaneEPG) error {
	if e == nil || e.FournisseurID == "" {
		return errors.New("epg : instantane sans fournisseur")
	}
	for i, p := range e.Programmes {
		if p.EpgID == "" || !p.Fin.After(p.Debut) {
			return fmt.Errorf("epg : programme %d invalide (canal vide ou fin <= debut)", i)
		}
	}
	b.db.Ecrivain.Lock()
	defer b.db.Ecrivain.Unlock()
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("epg : transaction : %w", err)
	}
	defer tx.Rollback() // sans effet apres Commit ; sinon l'ancien EPG reste intact
	f := e.FournisseurID
	for _, t := range []string{"epg_programmes", "epg_rapprochements"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+t+` WHERE fournisseur_id = ?`, f); err != nil {
			return fmt.Errorf("epg : purge %s : %w", t, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO epg_synchros (fournisseur_id, pris) VALUES (?, ?)
		ON CONFLICT(fournisseur_id) DO UPDATE SET pris = excluded.pris`, f, ms(e.Pris)); err != nil {
		return fmt.Errorf("epg : synchro : %w", err)
	}
	if err := b.inserer(ctx, tx, "epg_programmes",
		`INSERT OR IGNORE INTO epg_programmes (fournisseur_id, epg_id, debut, fin, titre, description, categorie) VALUES (?,?,?,?,?,?,?)`,
		len(e.Programmes), func(i int) []any {
			p := e.Programmes[i]
			return []any{f, p.EpgID, ms(p.Debut), ms(p.Fin), p.Titre, p.Description, p.Categorie}
		}); err != nil {
		return err
	}
	if err := b.inserer(ctx, tx, "epg_rapprochements",
		`INSERT OR REPLACE INTO epg_rapprochements (chaine_id, fournisseur_id, epg_id, methode, ambigu) VALUES (?,?,?,?,?)`,
		len(e.Rapprochements), func(i int) []any {
			r := e.Rapprochements[i]
			amb := 0
			if r.Ambigu {
				amb = 1
			}
			return []any{string(r.Chaine), f, r.EpgID, string(r.Methode), amb}
		}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("epg : validation : %w", err)
	}
	return nil
}

func (b *Base) inserer(ctx context.Context, tx *sql.Tx, table, requete string, n int, ligne func(int) []any) error {
	if n > 0 {
		st, err := tx.PrepareContext(ctx, requete)
		if err != nil {
			return fmt.Errorf("epg : preparation %s : %w", table, err)
		}
		defer st.Close()
		for i := 0; i < n; i++ {
			if _, err := st.ExecContext(ctx, ligne(i)...); err != nil {
				return fmt.Errorf("epg : ecriture %s : %w", table, err)
			}
		}
	}
	if b.apresInsertion != nil {
		if err := b.apresInsertion(table); err != nil {
			return err
		}
	}
	return nil
}

// DerniereSynchroEPG renvoie l'instant du dernier EPG ecrit, ou le zero value.
func (b *Base) DerniereSynchroEPG(ctx context.Context, fournisseurID string) (time.Time, error) {
	var n int64
	err := b.db.QueryRowContext(ctx, `SELECT pris FROM epg_synchros WHERE fournisseur_id = ?`, fournisseurID).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, fmt.Errorf("epg : derniere synchro : %w", err)
	}
	return depuisMs(n), nil
}

const colonnesProgramme = `p.fournisseur_id, p.epg_id, p.debut, p.fin, p.titre, p.description, p.categorie`

func lireProgramme(r interface{ Scan(...any) error }) (domain.Programme, error) {
	var p domain.Programme
	var d, f int64
	if err := r.Scan(&p.FournisseurID, &p.EpgID, &d, &f, &p.Titre, &p.Description, &p.Categorie); err != nil {
		return p, err
	}
	p.Debut, p.Fin = depuisMs(d), depuisMs(f)
	return p, nil
}

// EnCoursEtSuivant : « en cours » = debut <= a < fin ; « suivant » = premier debut > a.
// Sans programme en cours (trou de grille), le suivant est quand meme renvoye.
func (b *Base) EnCoursEtSuivant(ctx context.Context, chaine domain.ContenuID, a time.Time) (courant, suivant *domain.Programme, err error) {
	const jointure = ` FROM epg_rapprochements r JOIN epg_programmes p
		ON p.fournisseur_id = r.fournisseur_id AND p.epg_id = r.epg_id WHERE r.chaine_id = ? AND r.ambigu = 0 `
	t := ms(a)
	c, err := lireProgramme(b.db.QueryRowContext(ctx,
		`SELECT `+colonnesProgramme+jointure+`AND p.debut <= ? AND p.fin > ? ORDER BY p.debut DESC LIMIT 1`, string(chaine), t, t))
	switch {
	case err == nil:
		courant = &c
	case !errors.Is(err, sql.ErrNoRows):
		return nil, nil, fmt.Errorf("epg : programme en cours : %w", err)
	}
	s, err := lireProgramme(b.db.QueryRowContext(ctx,
		`SELECT `+colonnesProgramme+jointure+`AND p.debut > ? ORDER BY p.debut LIMIT 1`, string(chaine), t))
	switch {
	case err == nil:
		suivant = &s
	case !errors.Is(err, sql.ErrNoRows):
		return nil, nil, fmt.Errorf("epg : programme suivant : %w", err)
	}
	return courant, suivant, nil
}

// tailleLot borne le nombre de parametres d'un IN (...) (la limite de SQLite est
// bien plus haute, mais un lot raisonnable garde des requetes courtes).
const tailleLot = 400

// Grille renvoie les programmes qui chevauchent [de, a[ pour chaque chaine
// demandee, par ordre de debut. Une chaine sans programme est absente du resultat.
func (b *Base) Grille(ctx context.Context, chaines []domain.ContenuID, de, a time.Time) (map[domain.ContenuID][]domain.Programme, error) {
	res := map[domain.ContenuID][]domain.Programme{}
	vus := map[domain.ContenuID]bool{}
	var uniques []domain.ContenuID
	for _, c := range chaines {
		if !vus[c] {
			vus[c] = true
			uniques = append(uniques, c)
		}
	}
	for len(uniques) > 0 {
		n := min(tailleLot, len(uniques))
		lot := uniques[:n]
		uniques = uniques[n:]
		args := make([]any, 0, n+2)
		for _, c := range lot {
			args = append(args, string(c))
		}
		args = append(args, ms(a), ms(de))
		rows, err := b.db.QueryContext(ctx,
			`SELECT r.chaine_id, `+colonnesProgramme+` FROM epg_rapprochements r JOIN epg_programmes p
			 ON p.fournisseur_id = r.fournisseur_id AND p.epg_id = r.epg_id
			 WHERE r.ambigu = 0 AND r.chaine_id IN (`+strings.TrimSuffix(strings.Repeat("?,", n), ",")+`)
			 AND p.debut < ? AND p.fin > ? ORDER BY r.chaine_id, p.debut`, args...)
		if err != nil {
			return nil, fmt.Errorf("epg : grille : %w", err)
		}
		for rows.Next() {
			var id string
			var p domain.Programme
			var d, f int64
			if err := rows.Scan(&id, &p.FournisseurID, &p.EpgID, &d, &f, &p.Titre, &p.Description, &p.Categorie); err != nil {
				rows.Close()
				return nil, fmt.Errorf("epg : grille : %w", err)
			}
			p.Debut, p.Fin = depuisMs(d), depuisMs(f)
			res[domain.ContenuID(id)] = append(res[domain.ContenuID(id)], p)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, fmt.Errorf("epg : grille : %w", err)
		}
	}
	return res, nil
}

// Ambigus liste les chaines du fournisseur dont le nom correspond a plusieurs
// canaux du guide : a signaler a l'administrateur, aucun programme ne leur est servi.
func (b *Base) Ambigus(ctx context.Context, fournisseurID string) ([]domain.ContenuID, error) {
	rows, err := b.db.QueryContext(ctx,
		`SELECT chaine_id FROM epg_rapprochements WHERE fournisseur_id = ? AND ambigu = 1 ORDER BY chaine_id`, fournisseurID)
	if err != nil {
		return nil, fmt.Errorf("epg : ambigus : %w", err)
	}
	defer rows.Close()
	var out []domain.ContenuID
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("epg : ambigus : %w", err)
		}
		out = append(out, domain.ContenuID(id))
	}
	return out, rows.Err()
}

// Purger supprime les programmes termines avant `avant` (tous fournisseurs) et
// renvoie leur nombre. L'ecriture d'un EPG remplace deja tout ; la purge sert entre
// deux rafraichissements, pour que la base ne garde pas des jours de passe.
func (b *Base) Purger(ctx context.Context, avant time.Time) (int64, error) {
	b.db.Ecrivain.Lock()
	defer b.db.Ecrivain.Unlock()
	r, err := b.db.ExecContext(ctx, `DELETE FROM epg_programmes WHERE fin <= ?`, ms(avant))
	if err != nil {
		return 0, fmt.Errorf("epg : purge : %w", err)
	}
	return r.RowsAffected()
}
