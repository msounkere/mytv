// Package profil stocke l'etat propre a chaque profil : favoris, historique de
// lecture avec point de reprise, reglages synchronises entre appareils (US-13).
//
// Choix structurants :
//   - toutes les methodes prennent le profilID et ne touchent QUE ses lignes
//     (condition profil_id dans chaque requete) : l'isolation ne depend pas de
//     l'appelant, qui a pourtant deja verifie l'appartenance a la session ;
//   - les ecritures sont serialisees (un ecrivain SQLite) et chaque methode est UNE
//     transaction : verifier une limite puis inserer ne peut pas etre double par
//     un appel concurrent ;
//   - les reglages portent une revision : deux appareils qui ecrivent depuis la
//     meme base ne s'ecrasent pas en silence (ErrConflit), le perdant relit et
//     reecrit : « dernier ecrit gagne », mais en connaissance de cause ;
//   - un contenu disparu apres une resynchronisation n'est pas supprime (la
//     synchronisation peut etre partielle ou temporaire) : il est ignore a la lecture.
package profil

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"mytv/internal/basesqlite"
	"mytv/internal/domain"
)

var migrations = []basesqlite.Migration{
	{Version: 1, Nom: "etat par profil", SQL: `
-- seq croissant : l'ordre d'ajout (favoris) et de derniere ecriture (historique)
-- est stable, meme pour deux ecritures dans la meme milliseconde.
CREATE TABLE profil_favoris (
	seq        INTEGER PRIMARY KEY AUTOINCREMENT,
	profil_id  TEXT NOT NULL,
	contenu_id TEXT NOT NULL,
	ajoute     INTEGER NOT NULL,            -- millisecondes Unix UTC
	UNIQUE (profil_id, contenu_id)
) STRICT;

CREATE TABLE profil_historique (
	seq        INTEGER PRIMARY KEY AUTOINCREMENT,
	profil_id  TEXT NOT NULL,
	contenu_id TEXT NOT NULL,
	serie_id   TEXT NOT NULL,               -- vide sauf pour un episode
	position   INTEGER NOT NULL,            -- millisecondes
	duree      INTEGER NOT NULL,            -- millisecondes, 0 si inconnue
	vu         INTEGER NOT NULL,            -- millisecondes Unix UTC
	UNIQUE (profil_id, contenu_id)
) STRICT;
CREATE INDEX profil_historique_recent ON profil_historique (profil_id, vu DESC, seq DESC);

CREATE TABLE profil_reglages (
	profil_id TEXT PRIMARY KEY,
	revision  INTEGER NOT NULL,
	valeurs   TEXT NOT NULL,                -- objet JSON {cle: valeur}
	modifie   INTEGER NOT NULL
) STRICT;
`},
	{Version: 2, Nom: "liste a voir (US-42)", SQL: `
-- Liste « A voir » : un element par film ou par SERIE (jamais par episode), ordonnee par
-- rang. a_voir_id : « vod:<contenu> » ou « serie:<contenu> » (domain.AVoirID).
CREATE TABLE profil_a_voir (
	seq             INTEGER PRIMARY KEY AUTOINCREMENT,
	profil_id       TEXT NOT NULL,
	a_voir_id       TEXT NOT NULL,
	rang            INTEGER NOT NULL,
	ajoute          INTEGER NOT NULL,        -- millisecondes Unix UTC
	episodes_avance INTEGER NOT NULL,        -- 0 = defaut de la configuration
	depuis_saison   INTEGER NOT NULL,        -- 0 = a partir de l'episode entame
	depuis_episode  INTEGER NOT NULL,
	UNIQUE (profil_id, a_voir_id)
) STRICT;
CREATE INDEX profil_a_voir_ordre ON profil_a_voir (profil_id, rang, seq);
`},
}

// Catalogue dit quels identifiants existent encore (implemente par stockage.Base).
type Catalogue interface {
	Existants(ctx context.Context, ids []domain.ContenuID) (map[domain.ContenuID]bool, error)
}

// Options regle la Base ; les trois bornes viennent de config.Profil et sont obligatoires.
type Options struct {
	// Catalogue sert a ignorer les orphelins ; nil = aucun filtrage.
	Catalogue Catalogue
	Horloge   domain.Horloge
	// HistoriqueMax, FavorisMax : nombre d'entrees par profil. ReglagesMax : octets
	// (somme des longueurs des cles et des valeurs).
	HistoriqueMax, FavorisMax, ReglagesMax int
}

// Tailles maximales d'un element : evitent qu'une cle ou un identifiant demesure
// contourne ReglagesMax ou gonfle un index.
const (
	maxIdentifiant = 256
	maxCle         = 64
)

var (
	_ domain.EtatProfil = (*Base)(nil)
	_ domain.ListeAVoir = (*Base)(nil)
)

// Base est un domain.EtatProfil adosse a SQLite.
type Base struct {
	db  *basesqlite.Base
	opt Options
}

// Ouvrir ouvre (et migre) l'etat des profils dans le fichier donne, qui peut etre
// celui du catalogue.
func Ouvrir(ctx context.Context, chemin string, opt Options) (*Base, error) {
	if opt.HistoriqueMax < 1 || opt.FavorisMax < 1 || opt.ReglagesMax < 1 {
		return nil, errors.New("profil : HistoriqueMax, FavorisMax et ReglagesMax doivent etre >= 1")
	}
	if opt.Horloge == nil {
		opt.Horloge = horlogeReelle{}
	}
	db, err := basesqlite.Ouvrir(ctx, chemin, "profil", migrations, opt.Horloge.Maintenant)
	if err != nil {
		return nil, err
	}
	return &Base{db: db, opt: opt}, nil
}

type horlogeReelle struct{}

func (horlogeReelle) Maintenant() time.Time { return time.Now() }

func (b *Base) Fermer() error { return b.db.Close() }

func ms(t time.Time) int64       { return t.UTC().UnixMilli() }
func depuisMs(n int64) time.Time { return time.UnixMilli(n).UTC() }

func verifier(profilID string, c domain.ContenuID) error {
	if profilID == "" || len(profilID) > maxIdentifiant {
		return errors.New("profil : identifiant de profil invalide")
	}
	if c == "" || len(c) > maxIdentifiant {
		return errors.New("profil : identifiant de contenu invalide")
	}
	return nil
}

// ecrire execute f dans une transaction, sous le verrou d'ecriture.
func (b *Base) ecrire(ctx context.Context, f func(tx *sql.Tx) error) error {
	b.db.Ecrivain.Lock()
	defer b.db.Ecrivain.Unlock()
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("profil : transaction : %w", err)
	}
	defer tx.Rollback()
	if err := f(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("profil : validation : %w", err)
	}
	return nil
}

// EffacerProfil supprime tout l'etat d'un profil (a appeler quand le profil est supprime).
func (b *Base) EffacerProfil(ctx context.Context, profilID string) error {
	if profilID == "" {
		return errors.New("profil : identifiant de profil invalide")
	}
	return b.ecrire(ctx, func(tx *sql.Tx) error {
		for _, t := range []string{"profil_favoris", "profil_historique", "profil_reglages", "profil_a_voir"} {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+t+` WHERE profil_id = ?`, profilID); err != nil {
				return fmt.Errorf("profil : effacement %s : %w", t, err)
			}
		}
		return nil
	})
}

// ------------------------------------------------------------------ favoris

// AjouterFavori est idempotent : un favori deja present garde sa place.
func (b *Base) AjouterFavori(ctx context.Context, profilID string, c domain.ContenuID) error {
	if err := verifier(profilID, c); err != nil {
		return err
	}
	return b.ecrire(ctx, func(tx *sql.Tx) error {
		var deja int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM profil_favoris WHERE profil_id = ? AND contenu_id = ?`,
			profilID, string(c)).Scan(&deja); err != nil {
			return fmt.Errorf("profil : favori : %w", err)
		}
		if deja > 0 {
			return nil
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM profil_favoris WHERE profil_id = ?`, profilID).Scan(&n); err != nil {
			return fmt.Errorf("profil : favoris : %w", err)
		}
		if n >= b.opt.FavorisMax {
			return fmt.Errorf("%w : %d favoris au maximum", domain.ErrLimite, b.opt.FavorisMax)
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO profil_favoris (profil_id, contenu_id, ajoute) VALUES (?, ?, ?)`,
			profilID, string(c), ms(b.opt.Horloge.Maintenant()))
		if err != nil {
			return fmt.Errorf("profil : ajout du favori : %w", err)
		}
		return nil
	})
}

// RetirerFavori est idempotent.
func (b *Base) RetirerFavori(ctx context.Context, profilID string, c domain.ContenuID) error {
	if err := verifier(profilID, c); err != nil {
		return err
	}
	return b.ecrire(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM profil_favoris WHERE profil_id = ? AND contenu_id = ?`,
			profilID, string(c)); err != nil {
			return fmt.Errorf("profil : retrait du favori : %w", err)
		}
		return nil
	})
}

// Favoris dans l'ordre d'ajout, sans les identifiants dont le contenu a disparu.
func (b *Base) Favoris(ctx context.Context, profilID string) ([]domain.ContenuID, error) {
	if profilID == "" {
		return nil, errors.New("profil : identifiant de profil invalide")
	}
	rows, err := b.db.QueryContext(ctx, `SELECT contenu_id FROM profil_favoris WHERE profil_id = ? ORDER BY seq`, profilID)
	if err != nil {
		return nil, fmt.Errorf("profil : favoris : %w", err)
	}
	defer rows.Close()
	ids := []domain.ContenuID{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("profil : favoris : %w", err)
		}
		ids = append(ids, domain.ContenuID(id))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("profil : favoris : %w", err)
	}
	rows.Close()
	existants, err := b.existants(ctx, ids)
	if err != nil {
		return nil, err
	}
	if existants == nil {
		return ids, nil
	}
	vivants := ids[:0]
	for _, id := range ids {
		if existants[id] {
			vivants = append(vivants, id)
		}
	}
	return vivants, nil
}

// existants renvoie nil si aucun catalogue n'est branche (pas de filtrage).
func (b *Base) existants(ctx context.Context, ids []domain.ContenuID) (map[domain.ContenuID]bool, error) {
	if b.opt.Catalogue == nil || len(ids) == 0 {
		if b.opt.Catalogue != nil {
			return map[domain.ContenuID]bool{}, nil
		}
		return nil, nil
	}
	m, err := b.opt.Catalogue.Existants(ctx, ids)
	if err != nil {
		return nil, fmt.Errorf("profil : verification du catalogue : %w", err)
	}
	return m, nil
}

// ------------------------------------------------------------------ historique

// EnregistrerLecture met a jour (ou cree) l'entree du contenu et la place en tete ;
// au-dela de HistoriqueMax les plus anciennes sont purgees.
func (b *Base) EnregistrerLecture(ctx context.Context, profilID string, l domain.Lecture) error {
	if err := verifier(profilID, l.Contenu); err != nil {
		return err
	}
	if len(l.Serie) > maxIdentifiant {
		return errors.New("profil : identifiant de serie invalide")
	}
	vu := l.Vu
	if vu.IsZero() {
		vu = b.opt.Horloge.Maintenant()
	}
	duree := max(l.Duree, 0)
	position := max(l.Position, 0)
	if duree > 0 {
		position = min(position, duree) // reprise au-dela de la fin : incoherent, on borne
	}
	return b.ecrire(ctx, func(tx *sql.Tx) error {
		// Supprimer puis inserer : la ligne reprend un seq neuf, donc l'ordre
		// « derniere ecriture » reste exact meme si deux Vu sont identiques.
		if _, err := tx.ExecContext(ctx, `DELETE FROM profil_historique WHERE profil_id = ? AND contenu_id = ?`,
			profilID, string(l.Contenu)); err != nil {
			return fmt.Errorf("profil : historique : %w", err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO profil_historique (profil_id, contenu_id, serie_id, position, duree, vu)
			VALUES (?, ?, ?, ?, ?, ?)`, profilID, string(l.Contenu), string(l.Serie),
			position.Milliseconds(), duree.Milliseconds(), ms(vu)); err != nil {
			return fmt.Errorf("profil : historique : %w", err)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM profil_historique WHERE profil_id = ? AND seq NOT IN (
			SELECT seq FROM profil_historique WHERE profil_id = ? ORDER BY vu DESC, seq DESC LIMIT ?)`,
			profilID, profilID, b.opt.HistoriqueMax); err != nil {
			return fmt.Errorf("profil : purge de l'historique : %w", err)
		}
		return nil
	})
}

// OublierLecture est idempotent.
func (b *Base) OublierLecture(ctx context.Context, profilID string, c domain.ContenuID) error {
	if err := verifier(profilID, c); err != nil {
		return err
	}
	return b.ecrire(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `DELETE FROM profil_historique WHERE profil_id = ? AND contenu_id = ?`,
			profilID, string(c)); err != nil {
			return fmt.Errorf("profil : oubli de la lecture : %w", err)
		}
		return nil
	})
}

// Historique du plus recent au plus ancien ; limite <= 0 = tout. Une entree dont le
// contenu (ou, pour un episode, la serie) a disparu est ignoree AVANT la limite.
func (b *Base) Historique(ctx context.Context, profilID string, limite int) ([]domain.Lecture, error) {
	if profilID == "" {
		return nil, errors.New("profil : identifiant de profil invalide")
	}
	rows, err := b.db.QueryContext(ctx, `SELECT contenu_id, serie_id, position, duree, vu FROM profil_historique
		WHERE profil_id = ? ORDER BY vu DESC, seq DESC`, profilID)
	if err != nil {
		return nil, fmt.Errorf("profil : historique : %w", err)
	}
	defer rows.Close()
	res := []domain.Lecture{}
	for rows.Next() {
		var c, s string
		var pos, dur, vu int64
		if err := rows.Scan(&c, &s, &pos, &dur, &vu); err != nil {
			return nil, fmt.Errorf("profil : historique : %w", err)
		}
		res = append(res, domain.Lecture{
			Contenu: domain.ContenuID(c), Serie: domain.ContenuID(s),
			Position: time.Duration(pos) * time.Millisecond, Duree: time.Duration(dur) * time.Millisecond, Vu: depuisMs(vu),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("profil : historique : %w", err)
	}
	rows.Close()
	ids := make([]domain.ContenuID, 0, len(res))
	for _, l := range res {
		ids = append(ids, cible(l))
	}
	existants, err := b.existants(ctx, ids)
	if err != nil {
		return nil, err
	}
	vivants := res[:0]
	for _, l := range res {
		if existants != nil && !existants[cible(l)] {
			continue
		}
		vivants = append(vivants, l)
		if limite > 0 && len(vivants) == limite {
			break
		}
	}
	return vivants, nil
}

// cible est l'identifiant dont l'existence decide si l'entree est orpheline : la
// serie pour un episode (les episodes ne sont pas dans le catalogue), le contenu sinon.
func cible(l domain.Lecture) domain.ContenuID {
	if l.Serie != "" {
		return l.Serie
	}
	return l.Contenu
}

// ------------------------------------------------------------------ reglages

// Reglages renvoie les reglages du profil ; revision 0 et valeurs vides s'il n'en a jamais ecrit.
func (b *Base) Reglages(ctx context.Context, profilID string) (domain.Reglages, error) {
	if profilID == "" {
		return domain.Reglages{}, errors.New("profil : identifiant de profil invalide")
	}
	return lireReglages(b.db.QueryRowContext(ctx,
		`SELECT revision, valeurs, modifie FROM profil_reglages WHERE profil_id = ?`, profilID))
}

func lireReglages(r *sql.Row) (domain.Reglages, error) {
	var rev, modifie int64
	var brut string
	err := r.Scan(&rev, &brut, &modifie)
	if errors.Is(err, sql.ErrNoRows) {
		return domain.Reglages{Valeurs: map[string]string{}}, nil
	}
	if err != nil {
		return domain.Reglages{}, fmt.Errorf("profil : reglages : %w", err)
	}
	v := map[string]string{}
	if err := json.Unmarshal([]byte(brut), &v); err != nil {
		return domain.Reglages{}, fmt.Errorf("profil : reglages illisibles : %w", err)
	}
	return domain.Reglages{Valeurs: v, Revision: rev, Modifie: depuisMs(modifie)}, nil
}

// EcrireReglages remplace TOUTES les valeurs si rev est la revision courante, sinon
// renvoie ErrConflit ACCOMPAGNE des reglages courants (le client fusionne et reessaie).
// Les limites (ErrLimite) sont verifiees avant toute ecriture.
func (b *Base) EcrireReglages(ctx context.Context, profilID string, valeurs map[string]string, rev int64) (domain.Reglages, error) {
	if profilID == "" || len(profilID) > maxIdentifiant {
		return domain.Reglages{}, errors.New("profil : identifiant de profil invalide")
	}
	taille := 0
	for k, v := range valeurs {
		if k == "" || len(k) > maxCle {
			return domain.Reglages{}, fmt.Errorf("%w : cle de reglage vide ou de plus de %d octets", domain.ErrLimite, maxCle)
		}
		taille += len(k) + len(v)
	}
	if taille > b.opt.ReglagesMax {
		return domain.Reglages{}, fmt.Errorf("%w : %d octets de reglages pour %d autorises", domain.ErrLimite, taille, b.opt.ReglagesMax)
	}
	brut, err := json.Marshal(valeurs)
	if err != nil {
		return domain.Reglages{}, fmt.Errorf("profil : reglages : %w", err)
	}
	if valeurs == nil {
		brut = []byte("{}")
	}
	var res domain.Reglages
	err = b.ecrire(ctx, func(tx *sql.Tx) error {
		courant, err := lireReglages(tx.QueryRowContext(ctx,
			`SELECT revision, valeurs, modifie FROM profil_reglages WHERE profil_id = ?`, profilID))
		if err != nil {
			return err
		}
		if courant.Revision != rev {
			res = courant
			return domain.ErrConflit
		}
		now := b.opt.Horloge.Maintenant()
		if _, err := tx.ExecContext(ctx, `INSERT INTO profil_reglages (profil_id, revision, valeurs, modifie) VALUES (?, ?, ?, ?)
			ON CONFLICT(profil_id) DO UPDATE SET revision = excluded.revision, valeurs = excluded.valeurs, modifie = excluded.modifie`,
			profilID, rev+1, string(brut), ms(now)); err != nil {
			return fmt.Errorf("profil : ecriture des reglages : %w", err)
		}
		copie := make(map[string]string, len(valeurs))
		for k, v := range valeurs {
			copie[k] = v
		}
		res = domain.Reglages{Valeurs: copie, Revision: rev + 1, Modifie: depuisMs(ms(now))}
		return nil
	})
	return res, err
}
