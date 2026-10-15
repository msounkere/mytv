package stockage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"

	"mytv/internal/domain"
)

var _ domain.ExportCatalogue = (*Base)(nil)

// Manifeste donne les effectifs en UN instantane de lecture : les cinq nombres
// viennent de la meme version du catalogue.
func (b *Base) Manifeste(ctx context.Context, fournisseurLive string) (domain.Manifeste, error) {
	var m domain.Manifeste
	conn, err := b.lecture(ctx)
	if err != nil {
		return m, err
	}
	defer conn.fermer()
	for _, t := range []struct {
		dest *int
		req  string
		args []any
	}{
		{&m.Categories, `SELECT COUNT(*) FROM categories WHERE ` + categoriesVisibles(fournisseurLive), argsLive(fournisseurLive)},
		{&m.Chaines, `SELECT COUNT(*) FROM chaines WHERE ` + chainesVisibles(fournisseurLive), argsLive(fournisseurLive)},
		{&m.Films, `SELECT COUNT(*) FROM films`, nil},
		{&m.Series, `SELECT COUNT(*) FROM series`, nil},
	} {
		if err := conn.c.QueryRowContext(ctx, t.req, t.args...).Scan(t.dest); err != nil {
			return m, fmt.Errorf("stockage : manifeste : %w", err)
		}
	}
	var pris sql.NullInt64
	if err := conn.c.QueryRowContext(ctx, `SELECT MAX(pris) FROM synchros`).Scan(&pris); err != nil {
		return m, fmt.Errorf("stockage : manifeste : %w", err)
	}
	m.Synchronise = depuisMillis(pris)
	return m, nil
}

// Les categories du direct et les chaines ne concernent que le fournisseur du
// direct (relais mono-fournisseur) ; films et series, tous les fournisseurs.
func categoriesVisibles(live string) string {
	if live == "" {
		return `1 = 1`
	}
	return `(type <> 'live' OR fournisseur_id = ?)`
}

func chainesVisibles(live string) string {
	if live == "" {
		return `1 = 1`
	}
	return `fournisseur_id = ?`
}

func argsLive(live string) []any {
	if live == "" {
		return nil
	}
	return []any{live}
}

// connLecture est une connexion dediee a un instantane de lecture. BEGIN est
// DIFFERE (et non IMMEDIATE comme le veut _txlock pour les ecritures) : une lecture
// ne doit jamais prendre le verrou d'ecriture ni retarder une synchronisation. En
// WAL, la transaction voit la base telle qu'elle etait a sa premiere lecture, meme
// si une synchronisation committe ensuite.
type connLecture struct{ c *sql.Conn }

func (b *Base) lecture(ctx context.Context) (*connLecture, error) {
	c, err := b.db.Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("stockage : connexion : %w", err)
	}
	if _, err := c.ExecContext(ctx, `BEGIN DEFERRED`); err != nil {
		c.Close()
		return nil, fmt.Errorf("stockage : debut de lecture : %w", err)
	}
	return &connLecture{c: c}, nil
}

// fermer termine l'instantane meme si le contexte de la requete est annule
// (sinon la connexion retournerait au pool avec une transaction ouverte).
func (l *connLecture) fermer() error {
	_, err := l.c.ExecContext(context.WithoutCancel(context.Background()), `ROLLBACK`)
	if err != nil {
		// Connexion dans un etat inconnu : on la jette plutot que de la rendre au pool.
		_ = l.c.Raw(func(any) error { return driver.ErrBadConn })
	}
	return errors.Join(err, l.c.Close())
}

type export struct {
	ctx  context.Context
	l    *connLecture
	n    int
	req  string
	args []any
	lire func(rows *sql.Rows) (any, error)
}

func (e *export) Elements() int { return e.n }
func (e *export) Fermer() error { return e.l.fermer() }

func (e *export) Lire(emettre func(any) error) error {
	rows, err := e.l.c.QueryContext(e.ctx, e.req, e.args...)
	if err != nil {
		return fmt.Errorf("stockage : export : %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		v, err := e.lire(rows)
		if err != nil {
			return fmt.Errorf("stockage : export : %w", err)
		}
		if err := emettre(v); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("stockage : export : %w", err)
	}
	return nil
}

// OuvrirExport compte puis parcourt dans le MEME instantane : le nombre annonce
// (X-Elements) est exactement le nombre de lignes emises.
//
// Ordre : fournisseur, categorie, puis ordre du fournisseur (ordre des chaines ;
// rang d'insertion pour films et series, qui est l'ordre de la reponse du
// fournisseur). Les categories sont triees par type, ordre, fournisseur, id.
func (b *Base) OuvrirExport(ctx context.Context, t domain.TypeExport, live string) (domain.Export, error) {
	e := &export{ctx: ctx}
	var comptage string
	switch t {
	case domain.ExportCategories:
		comptage = `SELECT COUNT(*) FROM categories WHERE ` + categoriesVisibles(live)
		e.req = `SELECT fournisseur_id, type, id, nom, ordre FROM categories WHERE ` + categoriesVisibles(live) +
			` ORDER BY type, ordre, fournisseur_id, id`
		e.args = argsLive(live)
		e.lire = func(r *sql.Rows) (any, error) {
			var c domain.Categorie
			var typ string
			err := r.Scan(&c.FournisseurID, &typ, &c.ID, &c.Nom, &c.Ordre)
			c.Type = domain.Type(typ)
			return c, err
		}
	case domain.ExportLive:
		comptage = `SELECT COUNT(*) FROM chaines WHERE ` + chainesVisibles(live)
		e.req = `SELECT ` + colonnesChaine + ` FROM chaines c WHERE ` + chainesVisibles(live) +
			` ORDER BY c.fournisseur_id, c.categorie_id, c.ordre, c.id`
		e.args = argsLive(live)
		e.lire = func(r *sql.Rows) (any, error) { return lireChaine(r) }
	case domain.ExportVOD:
		comptage = `SELECT COUNT(*) FROM films`
		e.req = `SELECT ` + colonnesFilm + ` FROM films t ORDER BY t.fournisseur_id, t.categorie_id, t.rowid`
		e.lire = func(r *sql.Rows) (any, error) { return lireFilm(r) }
	case domain.ExportSeries:
		comptage = `SELECT COUNT(*) FROM series`
		e.req = `SELECT ` + colonnesSerie + ` FROM series t ORDER BY t.fournisseur_id, t.categorie_id, t.rowid`
		e.lire = func(r *sql.Rows) (any, error) { return lireSerie(r) }
	default:
		return nil, fmt.Errorf("stockage : type d'export %q inconnu", t)
	}
	l, err := b.lecture(ctx)
	if err != nil {
		return nil, err
	}
	e.l = l
	if err := l.c.QueryRowContext(ctx, comptage, e.args...).Scan(&e.n); err != nil {
		l.fermer()
		return nil, fmt.Errorf("stockage : export : %w", err)
	}
	return e, nil
}
