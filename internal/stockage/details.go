package stockage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"mytv/internal/domain"
)

var _ domain.StockageDetails = (*Base)(nil)

// detailsJSON : forme stockee. Les noms sont volontairement stables et courts ; un
// champ ajoute plus tard est simplement absent des anciennes lignes (valeur zero).
type detailsJSON struct {
	Synopsis     string   `json:"synopsis,omitempty"`
	Genre        string   `json:"genre,omitempty"`
	Casting      string   `json:"casting,omitempty"`
	Realisateur  string   `json:"realisateur,omitempty"`
	Pays         string   `json:"pays,omitempty"`
	Sortie       string   `json:"sortie,omitempty"`
	BandeAnnonce string   `json:"bande_annonce,omitempty"`
	ImageFond    []string `json:"image_fond,omitempty"`
	Affiche      string   `json:"affiche,omitempty"`
	Note         float64  `json:"note,omitempty"`
	Age          string   `json:"age,omitempty"`
	DureeSecs    int64    `json:"duree_secs,omitempty"`
}

func versJSON(d domain.Details, duree time.Duration) detailsJSON {
	return detailsJSON{Synopsis: d.Synopsis, Genre: d.Genre, Casting: d.Casting, Realisateur: d.Realisateur,
		Pays: d.Pays, Sortie: d.Sortie, BandeAnnonce: d.BandeAnnonce, ImageFond: d.ImageFond,
		Affiche: d.Affiche, Note: d.Note, Age: d.Age, DureeSecs: int64(duree / time.Second)}
}

func (j detailsJSON) details() domain.Details {
	return domain.Details{Synopsis: j.Synopsis, Genre: j.Genre, Casting: j.Casting, Realisateur: j.Realisateur,
		Pays: j.Pays, Sortie: j.Sortie, BandeAnnonce: j.BandeAnnonce, ImageFond: j.ImageFond,
		Affiche: j.Affiche, Note: j.Note, Age: j.Age}
}

func (b *Base) lireDetails(ctx context.Context, q interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}, id domain.ContenuID, typ string) (detailsJSON, time.Time, error) {
	var donnees string
	var ms int64
	err := q.QueryRowContext(ctx, `SELECT donnees, recupere FROM details_contenu WHERE id = ? AND type = ?`, string(id), typ).Scan(&donnees, &ms)
	if errors.Is(err, sql.ErrNoRows) {
		return detailsJSON{}, time.Time{}, ErrNonTrouve
	}
	if err != nil {
		return detailsJSON{}, time.Time{}, fmt.Errorf("stockage : details : %w", err)
	}
	var j detailsJSON
	if err := json.Unmarshal([]byte(donnees), &j); err != nil {
		// Ligne illisible : traitee comme absente, elle sera reecrite par la prochaine recuperation.
		return detailsJSON{}, time.Time{}, ErrNonTrouve
	}
	return j, time.UnixMilli(ms).UTC(), nil
}

// DetailsFilm renvoie les details stockes et la date de leur recuperation.
func (b *Base) DetailsFilm(ctx context.Context, id domain.ContenuID) (*domain.DetailsFilm, time.Time, error) {
	j, t, err := b.lireDetails(ctx, b.db, id, string(domain.TypeFilm))
	if err != nil {
		return nil, time.Time{}, err
	}
	return &domain.DetailsFilm{Details: j.details(), Duree: time.Duration(j.DureeSecs) * time.Second}, t, nil
}

func (b *Base) DetailsSerie(ctx context.Context, id domain.ContenuID) (*domain.DetailsSerie, time.Time, error) {
	j, t, err := b.lireDetails(ctx, b.db, id, string(domain.TypeSerie))
	if err != nil {
		return nil, time.Time{}, err
	}
	return &domain.DetailsSerie{Details: j.details(), DureeEpisode: time.Duration(j.DureeSecs) * time.Second}, t, nil
}

// ecrireDetails fusionne puis ecrit, dans UNE transaction sous le verrou d'ecriture :
// deux recuperations concurrentes ne s'ecrasent pas, et des details vides (fournisseur
// sans fiche, reponse tronquee) ne touchent a rien — pas meme la date, pour que la
// prochaine demande retente plus tot que dans 30 jours.
func (b *Base) ecrireDetails(ctx context.Context, id domain.ContenuID, typ string, neuf detailsJSON, vide bool, a time.Time) error {
	if vide {
		return nil
	}
	b.ecrivain.Lock()
	defer b.ecrivain.Unlock()
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("stockage : details : %w", err)
	}
	defer tx.Rollback()
	return b.ecrireFusion(ctx, tx, id, typ, neuf, a)
}

func (b *Base) ecrireFusion(ctx context.Context, tx *sql.Tx, id domain.ContenuID, typ string, neuf detailsJSON, a time.Time) error {
	ancien, _, err := b.lireDetails(ctx, tx, id, typ)
	if err != nil && !errors.Is(err, ErrNonTrouve) {
		return err
	}
	fin := neuf
	if err == nil {
		fin = fusionner(ancien, neuf)
	}
	corps, err := json.Marshal(fin)
	if err != nil {
		return fmt.Errorf("stockage : details : %w", err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO details_contenu (id, type, donnees, recupere) VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET type = excluded.type, donnees = excluded.donnees, recupere = excluded.recupere`,
		string(id), typ, string(corps), a.UnixMilli()); err != nil {
		return fmt.Errorf("stockage : details : %w", err)
	}
	return tx.Commit()
}

// fusionner : un champ vide du neuf ne remplace jamais un champ connu.
func fusionner(ancien, neuf detailsJSON) detailsJSON {
	da := domain.DetailsFilm{Details: ancien.details(), Duree: time.Duration(ancien.DureeSecs) * time.Second}
	dn := domain.DetailsFilm{Details: neuf.details(), Duree: time.Duration(neuf.DureeSecs) * time.Second}
	f := da.Fusionner(dn)
	return versJSON(f.Details, f.Duree)
}

func (b *Base) EcrireDetailsFilm(ctx context.Context, id domain.ContenuID, d domain.DetailsFilm, a time.Time) error {
	return b.ecrireDetails(ctx, id, string(domain.TypeFilm), versJSON(d.Details, d.Duree), d.Vide(), a)
}

func (b *Base) EcrireDetailsSerie(ctx context.Context, id domain.ContenuID, d domain.DetailsSerie, a time.Time) error {
	return b.ecrireDetails(ctx, id, string(domain.TypeSerie), versJSON(d.Details, d.DureeEpisode), d.Vide(), a)
}

// PurgerDetails supprime les details recuperes avant `avant` (menage : un contenu
// disparu du catalogue ne laisse pas de ligne eternelle). Renvoie le nombre de lignes.
func (b *Base) PurgerDetails(ctx context.Context, avant time.Time) (int64, error) {
	b.ecrivain.Lock()
	defer b.ecrivain.Unlock()
	r, err := b.db.ExecContext(ctx, `DELETE FROM details_contenu WHERE recupere < ?`, avant.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("stockage : purge des details : %w", err)
	}
	return r.RowsAffected()
}
