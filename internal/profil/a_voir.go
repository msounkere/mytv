package profil

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"mytv/internal/domain"
)

// Liste « A voir » (US-42 CA1). Memes principes que les favoris : isolation par
// profil dans chaque requete, une transaction par methode, orphelins ignores a la
// lecture. La liste est bornee par FavorisMax (meme nature : une liste d'identifiants
// choisis a la main).

// episodesAvanceMax borne l'option par element (le budget disque est la vraie limite).
const episodesAvanceMax = 20

func verifierAVoir(profilID string, e domain.ElementAVoir) (domain.AVoirID, error) {
	id := e.ID
	if id == "" {
		if e.Serie {
			id = domain.AVoirSerie(e.Contenu)
		} else {
			id = domain.AVoirFilm(e.Contenu)
		}
	}
	if profilID == "" || len(profilID) > maxIdentifiant {
		return "", errors.New("profil : identifiant de profil invalide")
	}
	if _, _, ok := id.Decomposer(); !ok || len(id) > maxIdentifiant {
		return "", errors.New("profil : identifiant « a voir » invalide")
	}
	if e.EpisodesAvance < 0 || e.EpisodesAvance > episodesAvanceMax || e.DepuisSaison < 0 || e.DepuisEpisode < 0 {
		return "", fmt.Errorf("%w : episodes_avance (0 a %d) ou depuis invalides", domain.ErrLimite, episodesAvanceMax)
	}
	return id, nil
}

// AjouterAVoir ajoute en fin de liste ou met a jour les options d'un element deja
// present (son rang et sa date d'ajout sont conserves). Idempotent.
func (b *Base) AjouterAVoir(ctx context.Context, profilID string, e domain.ElementAVoir) error {
	id, err := verifierAVoir(profilID, e)
	if err != nil {
		return err
	}
	return b.ecrire(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE profil_a_voir SET episodes_avance = ?, depuis_saison = ?, depuis_episode = ?
			WHERE profil_id = ? AND a_voir_id = ?`, e.EpisodesAvance, e.DepuisSaison, e.DepuisEpisode, profilID, string(id))
		if err != nil {
			return fmt.Errorf("profil : a voir : %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			return nil
		}
		var total int
		var rang sql.NullInt64
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*), MAX(rang) FROM profil_a_voir WHERE profil_id = ?`, profilID).Scan(&total, &rang); err != nil {
			return fmt.Errorf("profil : a voir : %w", err)
		}
		if total >= b.opt.FavorisMax {
			return fmt.Errorf("%w : %d elements « a voir » au maximum", domain.ErrLimite, b.opt.FavorisMax)
		}
		suivant := int64(0)
		if rang.Valid {
			suivant = rang.Int64 + 1
		}
		_, err = tx.ExecContext(ctx, `INSERT INTO profil_a_voir (profil_id, a_voir_id, rang, ajoute, episodes_avance, depuis_saison, depuis_episode)
			VALUES (?, ?, ?, ?, ?, ?, ?)`, profilID, string(id), suivant, ms(b.opt.Horloge.Maintenant()), e.EpisodesAvance, e.DepuisSaison, e.DepuisEpisode)
		if err != nil {
			return fmt.Errorf("profil : ajout a voir : %w", err)
		}
		return nil
	})
}

// RetirerAVoir est idempotent.
func (b *Base) RetirerAVoir(ctx context.Context, profilID string, id domain.AVoirID) error {
	if _, err := verifierAVoir(profilID, domain.ElementAVoir{ID: id}); err != nil {
		return err
	}
	return b.ecrire(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM profil_a_voir WHERE profil_id = ? AND a_voir_id = ?`, profilID, string(id)); err != nil {
			return fmt.Errorf("profil : retrait a voir : %w", err)
		}
		return nil
	})
}

// OrdonnerAVoir renumerote les rangs : les identifiants donnes d'abord, dans cet ordre,
// puis les autres dans leur ordre d'avant. Un identifiant inconnu ou repete est ignore.
func (b *Base) OrdonnerAVoir(ctx context.Context, profilID string, ids []domain.AVoirID) error {
	if profilID == "" || len(profilID) > maxIdentifiant {
		return errors.New("profil : identifiant de profil invalide")
	}
	return b.ecrire(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT a_voir_id FROM profil_a_voir WHERE profil_id = ? ORDER BY rang, seq`, profilID)
		if err != nil {
			return fmt.Errorf("profil : ordre a voir : %w", err)
		}
		var actuels []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return fmt.Errorf("profil : ordre a voir : %w", err)
			}
			actuels = append(actuels, id)
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("profil : ordre a voir : %w", err)
		}
		rows.Close()
		connus := make(map[string]bool, len(actuels))
		for _, id := range actuels {
			connus[id] = true
		}
		var ordre []string
		places := map[string]bool{}
		for _, id := range ids {
			if connus[string(id)] && !places[string(id)] {
				ordre = append(ordre, string(id))
				places[string(id)] = true
			}
		}
		for _, id := range actuels {
			if !places[id] {
				ordre = append(ordre, id)
			}
		}
		for rang, id := range ordre {
			if _, err := tx.ExecContext(ctx, `UPDATE profil_a_voir SET rang = ? WHERE profil_id = ? AND a_voir_id = ?`, rang, profilID, id); err != nil {
				return fmt.Errorf("profil : ordre a voir : %w", err)
			}
		}
		return nil
	})
}

// AVoir rend la liste ordonnee, sans les elements dont le contenu a disparu du catalogue.
func (b *Base) AVoir(ctx context.Context, profilID string) ([]domain.ElementAVoir, error) {
	if profilID == "" {
		return nil, errors.New("profil : identifiant de profil invalide")
	}
	rows, err := b.db.QueryContext(ctx, `SELECT a_voir_id, rang, ajoute, episodes_avance, depuis_saison, depuis_episode
		FROM profil_a_voir WHERE profil_id = ? ORDER BY rang, seq`, profilID)
	if err != nil {
		return nil, fmt.Errorf("profil : a voir : %w", err)
	}
	defer rows.Close()
	res := []domain.ElementAVoir{}
	for rows.Next() {
		var id string
		var rang, ajoute, avance, saison, episode int64
		if err := rows.Scan(&id, &rang, &ajoute, &avance, &saison, &episode); err != nil {
			return nil, fmt.Errorf("profil : a voir : %w", err)
		}
		e := domain.ElementAVoir{ID: domain.AVoirID(id), Rang: int(rang), Ajoute: depuisMs(ajoute),
			EpisodesAvance: int(avance), DepuisSaison: int(saison), DepuisEpisode: int(episode)}
		contenu, serie, ok := e.ID.Decomposer()
		if !ok {
			continue
		}
		e.Contenu, e.Serie = contenu, serie
		res = append(res, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("profil : a voir : %w", err)
	}
	rows.Close()
	ids := make([]domain.ContenuID, 0, len(res))
	for _, e := range res {
		ids = append(ids, e.Contenu)
	}
	existants, err := b.existants(ctx, ids)
	if err != nil {
		return nil, err
	}
	if existants == nil {
		return res, nil
	}
	vivants := res[:0]
	for _, e := range res {
		if existants[e.Contenu] {
			e.Rang = len(vivants) // rangs contigus apres filtrage
			vivants = append(vivants, e)
		}
	}
	return vivants, nil
}

// CatalogueType : un Catalogue qui sait le type de chaque identifiant (chaine, film, serie).
type CatalogueType interface {
	Types(ctx context.Context, ids []domain.ContenuID) (map[domain.ContenuID]domain.Type, error)
}

// migrerFavoris (US-44 CA6) : les favoris ne concernent plus que les chaines. A chaque
// ouverture, chaque favori qui est un film ou une serie du catalogue est ajoute EN FIN de
// la liste « Ma liste » du meme profil (ordre d'ajout du favori), sauf s'il y est deja,
// puis retire des favoris. Idempotente : relancee sans effet ; un identifiant que le
// catalogue ne connait pas (catalogue pas encore synchronise, orphelin) reste favori et
// sera revu a l'ouverture suivante. Sans CatalogueType : rien.
func (b *Base) migrerFavoris(ctx context.Context) error {
	cat, ok := b.opt.Catalogue.(CatalogueType)
	if !ok {
		return nil
	}
	type favori struct {
		profil, contenu string
		ajoute          int64
	}
	rows, err := b.db.QueryContext(ctx, `SELECT profil_id, contenu_id, ajoute FROM profil_favoris ORDER BY profil_id, ajoute, seq`)
	if err != nil {
		return fmt.Errorf("profil : migration des favoris : %w", err)
	}
	var favoris []favori
	for rows.Next() {
		var f favori
		if err := rows.Scan(&f.profil, &f.contenu, &f.ajoute); err != nil {
			rows.Close()
			return fmt.Errorf("profil : migration des favoris : %w", err)
		}
		favoris = append(favoris, f)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return fmt.Errorf("profil : migration des favoris : %w", err)
	}
	if len(favoris) == 0 {
		return nil
	}
	ids := make([]domain.ContenuID, 0, len(favoris))
	for _, f := range favoris {
		ids = append(ids, domain.ContenuID(f.contenu))
	}
	types, err := cat.Types(ctx, ids)
	if err != nil {
		return fmt.Errorf("profil : migration des favoris : %w", err)
	}
	migres := map[string]int{}
	err = b.ecrire(ctx, func(tx *sql.Tx) error {
		for _, f := range favoris {
			var id domain.AVoirID
			switch types[domain.ContenuID(f.contenu)] {
			case domain.TypeFilm:
				id = domain.AVoirFilm(domain.ContenuID(f.contenu))
			case domain.TypeSerie:
				id = domain.AVoirSerie(domain.ContenuID(f.contenu))
			default:
				continue // chaine, ou inconnu du catalogue : reste favori
			}
			var deja int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM profil_a_voir WHERE profil_id = ? AND a_voir_id = ?`, f.profil, string(id)).Scan(&deja); err != nil {
				return err
			}
			if deja == 0 {
				var rang sql.NullInt64
				if err := tx.QueryRowContext(ctx, `SELECT MAX(rang) FROM profil_a_voir WHERE profil_id = ?`, f.profil).Scan(&rang); err != nil {
					return err
				}
				suivant := int64(0)
				if rang.Valid {
					suivant = rang.Int64 + 1
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO profil_a_voir (profil_id, a_voir_id, rang, ajoute, episodes_avance, depuis_saison, depuis_episode)
					VALUES (?, ?, ?, ?, 0, 0, 0)`, f.profil, string(id), suivant, f.ajoute); err != nil {
					return err
				}
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM profil_favoris WHERE profil_id = ? AND contenu_id = ?`, f.profil, f.contenu); err != nil {
				return err
			}
			migres[f.profil]++
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("profil : migration des favoris : %w", err)
	}
	for profil, n := range migres {
		b.opt.Journal.Info("profil : favoris de films et de series migres vers « Ma liste »", "profil", profil, "migres", n)
	}
	return nil
}

// ProfilsAVoir : les profils dont la liste n'est pas vide, dans un ordre stable.
func (b *Base) ProfilsAVoir(ctx context.Context) ([]string, error) {
	rows, err := b.db.QueryContext(ctx, `SELECT DISTINCT profil_id FROM profil_a_voir ORDER BY profil_id`)
	if err != nil {
		return nil, fmt.Errorf("profil : profils a voir : %w", err)
	}
	defer rows.Close()
	var res []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("profil : profils a voir : %w", err)
		}
		res = append(res, p)
	}
	return res, rows.Err()
}
