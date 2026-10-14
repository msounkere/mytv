// Package basesqlite ouvre une base SQLite (pur Go) reglee comme celle du
// catalogue et applique des migrations versionnees PROPRES au paquet appelant.
//
// Pourquoi un paquet a part : l'EPG et l'etat par profil vivent dans le meme
// fichier que le catalogue mais ont leur propre cycle de schema. Chacun garde sa
// table de versions (`migrations_<jeu>`), donc une migration d'un paquet ne
// peut jamais entrer en conflit avec celle d'un autre.
package basesqlite

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"regexp"
	"sync"
	"time"

	_ "modernc.org/sqlite" // pilote "sqlite"
)

// Migration est une etape du schema. Les versions sont strictement croissantes ;
// une migration publiee ne se modifie JAMAIS, on en ajoute une.
type Migration struct {
	Version int
	Nom     string
	SQL     string
}

// Base est une connexion poolee plus le verrou d'ecriture.
type Base struct {
	*sql.DB
	// Ecrivain serialise les ecritures : SQLite n'admet qu'un ecrivain, et le
	// faire attendre chez nous est annulable par ctx, contrairement a busy_timeout.
	Ecrivain sync.Mutex
}

var reJeu = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// Ouvrir ouvre le fichier (WAL) et applique les migrations du jeu.
func Ouvrir(ctx context.Context, chemin, jeu string, migs []Migration, maintenant func() time.Time) (*Base, error) {
	if !reJeu.MatchString(jeu) {
		return nil, fmt.Errorf("basesqlite : nom de jeu %q invalide", jeu)
	}
	for i, m := range migs {
		if m.Version != i+1 {
			return nil, fmt.Errorf("basesqlite : migrations de %s : version %d attendue en position %d", jeu, i+1, i)
		}
	}
	q := url.Values{}
	// Les pragmas dans l'adresse s'appliquent a CHAQUE connexion du pool.
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(10000)") // filet : autre paquet / processus sur le meme fichier
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+chemin+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("basesqlite : ouverture : %w", err)
	}
	b := &Base{DB: db}
	if err := b.migrer(ctx, jeu, migs, maintenant); err != nil {
		db.Close()
		return nil, err
	}
	return b, nil
}

func (b *Base) migrer(ctx context.Context, jeu string, migs []Migration, maintenant func() time.Time) error {
	b.Ecrivain.Lock()
	defer b.Ecrivain.Unlock()
	table := "migrations_" + jeu // jeu valide par reJeu : pas d'injection possible
	if _, err := b.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS `+table+` (
		version INTEGER PRIMARY KEY, nom TEXT NOT NULL, appliquee INTEGER NOT NULL) STRICT`); err != nil {
		return fmt.Errorf("basesqlite : table %s : %w", table, err)
	}
	var courante int
	if err := b.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM `+table).Scan(&courante); err != nil {
		return fmt.Errorf("basesqlite : version du schema %s : %w", jeu, err)
	}
	if len(migs) > 0 && courante > migs[len(migs)-1].Version {
		return fmt.Errorf("basesqlite : le schema %s est en version %d, plus recente que ce programme (%d)",
			jeu, courante, migs[len(migs)-1].Version)
	}
	for _, m := range migs {
		if m.Version <= courante {
			continue
		}
		if err := b.appliquer(ctx, table, m, maintenant().UnixMilli()); err != nil {
			return fmt.Errorf("basesqlite : migration %s %d (%s) : %w", jeu, m.Version, m.Nom, err)
		}
	}
	return nil
}

func (b *Base) appliquer(ctx context.Context, table string, m Migration, quand int64) error {
	tx, err := b.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Un autre processus a pu l'appliquer entre-temps : on reverifie sous verrou.
	var deja int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE version = ?`, m.Version).Scan(&deja); err != nil {
		return err
	}
	if deja > 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, m.SQL); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO `+table+` (version, nom, appliquee) VALUES (?, ?, ?)`,
		m.Version, m.Nom, quand); err != nil {
		return err
	}
	return tx.Commit()
}
