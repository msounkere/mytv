// Package sauvegarde copie la base SQLite pendant que le service tourne, la verifie,
// et garde les copies un nombre de jours borne (US-26).
//
// POURQUOI VACUUM INTO et pas une copie de fichier : la base est en mode WAL et des
// ecritures arrivent a tout moment ; copier mytv.db seul, ou mytv.db puis mytv.db-wal,
// peut donner un fichier qui n'a jamais existe (page du WAL manquante). VACUUM INTO lit
// UN instantane coherent dans une seule transaction de lecture, sans bloquer les
// ecrivains, et produit une base compacte.
package sauvegarde

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite" // pilote "sqlite"
)

// ErrExiste : la destination existe deja. On n'ecrase jamais une sauvegarde.
var ErrExiste = errors.New("sauvegarde : la destination existe deja")

// ErrCorrompue : la copie n'a pas passe PRAGMA integrity_check ; elle est supprimee.
var ErrCorrompue = errors.New("sauvegarde : la copie echoue a integrity_check")

// apresCopie : point d'injection des tests (corrompre la copie avant sa verification).
var apresCopie func(chemin string)

// Copier ecrit dans destination (qui ne doit pas exister) une copie coherente de la
// base ouverte par db, verifiee par integrity_check, avec les droits 0600. Le fichier
// n'apparait sous son nom definitif qu'une fois verifie : jamais de demi-sauvegarde
// que l'on prendrait pour une bonne. Rend la taille de la copie.
func Copier(ctx context.Context, db *sql.DB, destination string) (int64, error) {
	if _, err := os.Lstat(destination); err == nil {
		return 0, ErrExiste
	} else if !errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("sauvegarde : %w", err)
	}
	tmp := destination + ".tmp"
	_ = os.Remove(tmp) // reste d'une sauvegarde interrompue
	defer os.Remove(tmp)

	// Le chemin est lie en parametre : aucun risque d'injection par un nom de fichier.
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, tmp); err != nil {
		return 0, fmt.Errorf("sauvegarde : copie : %w", err)
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return 0, fmt.Errorf("sauvegarde : droits : %w", err)
	}
	if apresCopie != nil {
		apresCopie(tmp)
	}
	if err := verifier(ctx, tmp); err != nil {
		return 0, err
	}
	if err := synchroniser(tmp); err != nil {
		return 0, fmt.Errorf("sauvegarde : ecriture sur disque : %w", err)
	}
	if err := os.Rename(tmp, destination); err != nil {
		return 0, fmt.Errorf("sauvegarde : %w", err)
	}
	st, err := os.Stat(destination)
	if err != nil {
		return 0, fmt.Errorf("sauvegarde : %w", err)
	}
	return st.Size(), nil
}

// Verifier controle un fichier de sauvegarde : PRAGMA integrity_check doit repondre « ok ».
// Le fichier n'est ni modifie ni accompagne d'un journal (ouverture immuable).
func Verifier(ctx context.Context, fichier string) error { return verifier(ctx, fichier) }

func verifier(ctx context.Context, fichier string) error {
	abs, err := filepath.Abs(fichier)
	if err != nil {
		return err
	}
	// immutable=1 : aucun verrou, aucun -wal/-shm ecrit a cote d'une sauvegarde.
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: abs, RawQuery: "mode=ro&immutable=1"}).String())
	if err != nil {
		return fmt.Errorf("sauvegarde : verification : %w", err)
	}
	defer db.Close()
	// Un fichier si abime que SQLite ne peut meme pas l'interroger est corrompu, lui aussi
	// (sauf si c'est nous qui avons annule).
	echec := func(err error) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("%w : %v", ErrCorrompue, err)
	}
	rows, err := db.QueryContext(ctx, `PRAGMA integrity_check`)
	if err != nil {
		return echec(err)
	}
	defer rows.Close()
	var lignes []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			return echec(err)
		}
		lignes = append(lignes, l)
	}
	if err := rows.Err(); err != nil {
		return echec(err)
	}
	if len(lignes) != 1 || lignes[0] != "ok" {
		// Les premieres lignes decrivent le defaut ; jamais de donnees de la base.
		return fmt.Errorf("%w : %s", ErrCorrompue, strings.Join(lignes[:min(len(lignes), 3)], " ; "))
	}
	return nil
}

// synchroniser force l'ecriture du fichier sur le disque avant le renommage.
func synchroniser(chemin string) error {
	f, err := os.OpenFile(chemin, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// OuvrirLectureSeule ouvre une base existante sans la modifier : pas de migration, pas
// de creation, pas de verrou d'ecriture. C'est ce que `mytv --sauvegarder` utilise, y
// compris pendant que le service tourne (WAL : la lecture ne bloque personne).
func OuvrirLectureSeule(chemin string) (*sql.DB, error) {
	abs, err := filepath.Abs(chemin)
	if err != nil {
		return nil, err
	}
	if _, err := os.Stat(abs); err != nil {
		return nil, fmt.Errorf("base introuvable : %w", err)
	}
	q := url.Values{}
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(10000)")
	db, err := sql.Open("sqlite", (&url.URL{Scheme: "file", Path: abs, RawQuery: q.Encode()}).String())
	if err != nil {
		return nil, err
	}
	// Un seul reel acces : la connexion est tout de suite verifiee.
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("base illisible : %w", err)
	}
	return db, nil
}
