package stockage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"mytv/internal/domain"
)

var _ domain.StockageVideo = (*Base)(nil)

// filtreVideo construit la clause WHERE commune aux films et aux series. Le second
// resultat est faux si la recherche ne peut rien trouver (aucun mot).
func filtreVideo(f domain.FiltreChaines, table string) (string, []any, bool) {
	var (
		cond []string
		args []any
	)
	if f.FournisseurID != "" {
		cond = append(cond, `t.fournisseur_id = ?`)
		args = append(args, f.FournisseurID)
	}
	if f.CategorieID != "" {
		cond = append(cond, `t.categorie_id = ?`)
		args = append(args, f.CategorieID)
	}
	if strings.TrimSpace(f.Recherche) != "" {
		q, ok := requeteFTS(f.Recherche)
		if !ok {
			return "", nil, false
		}
		cond = append(cond, `t.rowid IN (SELECT rowid FROM `+table+`_fts WHERE `+table+`_fts MATCH ?)`)
		args = append(args, q)
	}
	w := ""
	if len(cond) > 0 {
		w = ` WHERE ` + strings.Join(cond, ` AND `)
	}
	return w, args, true
}

// pagination : tri stable (nom sans casse, puis id) pour que les pages ne
// dupliquent ni ne sautent de ligne.
func pagination(f domain.FiltreChaines, args []any) (string, []any) {
	limite := f.Limite
	if limite <= 0 {
		limite = LimiteParDefaut
	}
	return ` ORDER BY t.nom COLLATE NOCASE, t.id LIMIT ? OFFSET ?`, append(args, limite, max(f.Decalage, 0))
}

const colonnesFilm = `t.id, t.fournisseur_id, t.numero, t.nom, t.image, t.categorie_id, t.extension, t.note, t.annee, t.ajoute`

func lireFilm(r scanner) (domain.Film, error) {
	var f domain.Film
	var id string
	var aj sql.NullInt64
	err := r.Scan(&id, &f.FournisseurID, &f.Numero, &f.Nom, &f.Image, &f.CategorieID, &f.Extension, &f.Note, &f.Annee, &aj)
	f.ID = domain.ContenuID(id)
	if aj.Valid {
		f.Ajoute = time.UnixMilli(aj.Int64).UTC()
	}
	return f, err
}

// Films liste les films (meme filtre que Chaines : fournisseur, categorie, recherche, pagination).
func (b *Base) Films(ctx context.Context, f domain.FiltreChaines) ([]domain.Film, error) {
	w, args, ok := filtreVideo(f, "films")
	if !ok {
		return []domain.Film{}, nil
	}
	fin, args := pagination(f, args)
	rows, err := b.db.QueryContext(ctx, `SELECT `+colonnesFilm+` FROM films t`+w+fin, args...)
	if err != nil {
		return nil, fmt.Errorf("stockage : films : %w", err)
	}
	defer rows.Close()
	res := []domain.Film{}
	for rows.Next() {
		x, err := lireFilm(rows)
		if err != nil {
			return nil, fmt.Errorf("stockage : films : %w", err)
		}
		res = append(res, x)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("stockage : films : %w", err)
	}
	return res, nil
}

// Film renvoie ErrNonTrouve si absent.
func (b *Base) Film(ctx context.Context, id domain.ContenuID) (*domain.Film, error) {
	f, err := lireFilm(b.db.QueryRowContext(ctx, `SELECT `+colonnesFilm+` FROM films t WHERE t.id = ?`, string(id)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNonTrouve
	}
	if err != nil {
		return nil, fmt.Errorf("stockage : film : %w", err)
	}
	return &f, nil
}

const colonnesSerie = `t.id, t.fournisseur_id, t.numero, t.nom, t.image, t.categorie_id, t.note, t.annee, t.ajoute`

func lireSerie(r scanner) (domain.SerieTV, error) {
	var s domain.SerieTV
	var id string
	var aj sql.NullInt64
	err := r.Scan(&id, &s.FournisseurID, &s.Numero, &s.Nom, &s.Image, &s.CategorieID, &s.Note, &s.Annee, &aj)
	s.ID = domain.ContenuID(id)
	s.Ajoute = depuisMillis(aj)
	return s, err
}

// Series liste les series (meme filtre que Chaines).
func (b *Base) Series(ctx context.Context, f domain.FiltreChaines) ([]domain.SerieTV, error) {
	w, args, ok := filtreVideo(f, "series")
	if !ok {
		return []domain.SerieTV{}, nil
	}
	fin, args := pagination(f, args)
	rows, err := b.db.QueryContext(ctx, `SELECT `+colonnesSerie+` FROM series t`+w+fin, args...)
	if err != nil {
		return nil, fmt.Errorf("stockage : series : %w", err)
	}
	defer rows.Close()
	res := []domain.SerieTV{}
	for rows.Next() {
		x, err := lireSerie(rows)
		if err != nil {
			return nil, fmt.Errorf("stockage : series : %w", err)
		}
		res = append(res, x)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("stockage : series : %w", err)
	}
	return res, nil
}

// Serie renvoie ErrNonTrouve si absente.
func (b *Base) Serie(ctx context.Context, id domain.ContenuID) (*domain.SerieTV, error) {
	s, err := lireSerie(b.db.QueryRowContext(ctx, `SELECT `+colonnesSerie+` FROM series t WHERE t.id = ?`, string(id)))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNonTrouve
	}
	if err != nil {
		return nil, fmt.Errorf("stockage : serie : %w", err)
	}
	return &s, nil
}
