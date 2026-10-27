package stockage

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"mytv/internal/domain"
)

// US-36 : version du catalogue fondee sur le CONTENU, et export par difference.
//
// POURQUOI : la version du manifeste etait l'heure de synchronisation. Chaque
// matin, tous les appareils retelechargeaient 17 Mo et reimportaient ~154 000
// entrees, meme quand rien n'avait change. L'empreinte d'un fournisseur ne depend
// que de ses lignes (identifiants, noms, categories, logos, champs exportes) : une
// synchronisation qui ne change rien garde la meme version. Le journal des
// differences (identifiants touches par chaque version) permet de n'exporter que
// ce qui a change depuis une version encore connue.

// HistoriqueParDefaut : versions dont les differences sont conservees quand
// l'appelant n'en precise pas (catalogue.historique_versions).
const HistoriqueParDefaut = 14

// Options regle l'ouverture de la base.
type Options struct {
	// HistoriqueVersions : nombre de versions du catalogue dont les differences
	// sont gardees (export par difference). 0 = HistoriqueParDefaut.
	HistoriqueVersions int
}

// empreinte resume des champs en 16 caracteres hexadecimaux (SHA-256 tronque).
// Le separateur 0x1f ne figure dans aucun champ du fournisseur.
func empreinte(champs ...string) string {
	h := sha256.New()
	for i, c := range champs {
		if i > 0 {
			h.Write([]byte{0x1f})
		}
		h.Write([]byte(c))
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

func millisTexte(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return strconv.FormatInt(t.UnixMilli(), 10)
}

func flottant(f float64) string { return strconv.FormatFloat(f, 'g', -1, 64) }

// Empreintes de ligne : TOUS les champs que l'export renvoie, et rien d'autre.
// L'ordre d'une chaine chez le fournisseur (Ordre) n'est pas exporte : un
// catalogue simplement reordonne garde sa version.
func empreinteChaine(c domain.Chaine) string {
	return empreinte(c.Numero, c.Nom, c.Logo, c.EpgID, c.CategorieID, strconv.Itoa(c.NumeroChaine), millisTexte(c.Ajoute))
}

func empreinteFilm(f domain.Film) string {
	return empreinte(f.Numero, f.Nom, f.Image, f.CategorieID, f.Extension, flottant(f.Note), f.Annee, millisTexte(f.Ajoute))
}

func empreinteSerie(s domain.SerieTV) string {
	return empreinte(s.Numero, s.Nom, s.Image, s.CategorieID, flottant(s.Note), s.Annee, millisTexte(s.Ajoute))
}

// empreintesInstantane : l'empreinte de chaque ligne (dans l'ordre des tranches de
// l'instantane) et celle du fournisseur, calculee sur les lignes TRIEES : elle ne
// depend pas de l'ordre dans lequel le fournisseur repond.
type empreintesInstantane struct {
	chaines, films, series []string
	fournisseur            string
}

func calculerEmpreintes(s *domain.Instantane) empreintesInstantane {
	var e empreintesInstantane
	lignes := make([]string, 0, len(s.Categories)+len(s.Chaines)+len(s.Films)+len(s.Series))
	for _, c := range s.Categories {
		lignes = append(lignes, "c\x1f"+string(c.Type)+"\x1f"+c.ID+"\x1f"+c.Nom+"\x1f"+strconv.Itoa(c.Ordre))
	}
	e.chaines = make([]string, len(s.Chaines))
	for i, c := range s.Chaines {
		e.chaines[i] = empreinteChaine(c)
		lignes = append(lignes, "l\x1f"+string(c.ID)+"\x1f"+e.chaines[i])
	}
	e.films = make([]string, len(s.Films))
	for i, f := range s.Films {
		e.films[i] = empreinteFilm(f)
		lignes = append(lignes, "v\x1f"+string(f.ID)+"\x1f"+e.films[i])
	}
	e.series = make([]string, len(s.Series))
	for i, sr := range s.Series {
		e.series[i] = empreinteSerie(sr)
		lignes = append(lignes, "s\x1f"+string(sr.ID)+"\x1f"+e.series[i])
	}
	sort.Strings(lignes)
	h := sha256.New()
	for _, l := range lignes {
		h.Write([]byte(l))
		h.Write([]byte{'\n'})
	}
	e.fournisseur = hex.EncodeToString(h.Sum(nil)[:12])
	return e
}

type requeteur interface {
	QueryContext(ctx context.Context, q string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, q string, args ...any) *sql.Row
	ExecContext(ctx context.Context, q string, args ...any) (sql.Result, error)
}

// empreinteGlobale resume les empreintes de tous les fournisseurs (triees par
// identifiant) ; vide si aucun catalogue n'a jamais ete ecrit.
func empreinteGlobale(ctx context.Context, q requeteur) (string, error) {
	rows, err := q.QueryContext(ctx, `SELECT fournisseur_id, empreinte FROM synchros ORDER BY fournisseur_id`)
	if err != nil {
		return "", fmt.Errorf("stockage : empreinte globale : %w", err)
	}
	defer rows.Close()
	var parts []string
	for rows.Next() {
		var f, e string
		if err := rows.Scan(&f, &e); err != nil {
			return "", fmt.Errorf("stockage : empreinte globale : %w", err)
		}
		parts = append(parts, f+"="+e)
	}
	if err := rows.Err(); err != nil {
		return "", fmt.Errorf("stockage : empreinte globale : %w", err)
	}
	if len(parts) == 0 {
		return "", nil
	}
	h := sha256.Sum256([]byte(strings.Join(parts, "|")))
	return hex.EncodeToString(h[:12]), nil
}

// VersionCatalogue : empreinte globale courante du catalogue (CA1). Elle ne change
// que si le contenu d'un fournisseur change.
func (b *Base) VersionCatalogue(ctx context.Context) (string, error) {
	return empreinteGlobale(ctx, b.db)
}

// VersionsConnues : empreintes globales dont les differences sont encore
// journalisees, de la plus ancienne a la plus recente (CA3).
func (b *Base) VersionsConnues(ctx context.Context) ([]string, error) {
	rows, err := b.db.QueryContext(ctx, `SELECT version FROM catalogue_versions ORDER BY numero`)
	if err != nil {
		return nil, fmt.Errorf("stockage : versions connues : %w", err)
	}
	defer rows.Close()
	var res []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("stockage : versions connues : %w", err)
		}
		res = append(res, v)
	}
	return res, rows.Err()
}

// Effectifs compte les chaines, films et series d'UN fournisseur (garde contre une
// synchronisation degradee, US-36 CA2).
func (b *Base) Effectifs(ctx context.Context, fournisseurID string) (map[domain.Type]int, error) {
	res := map[domain.Type]int{}
	for _, t := range []struct {
		typ   domain.Type
		table string
	}{{domain.TypeLive, "chaines"}, {domain.TypeFilm, "films"}, {domain.TypeSerie, "series"}} {
		var n int
		if err := b.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+t.table+` WHERE fournisseur_id = ?`, fournisseurID).Scan(&n); err != nil {
			return nil, fmt.Errorf("stockage : effectifs : %w", err)
		}
		res[t.typ] = n
	}
	return res, nil
}

// empreintesExistantes : empreinte de chaque ligne du fournisseur, par type, telle
// qu'elle est en base AVANT l'ecriture (pour reperer les lignes modifiees).
func empreintesExistantes(ctx context.Context, q requeteur, f string) (map[domain.TypeExport]map[string]string, error) {
	res := map[domain.TypeExport]map[string]string{}
	for _, t := range []struct {
		typ   domain.TypeExport
		table string
	}{{domain.ExportLive, "chaines"}, {domain.ExportVOD, "films"}, {domain.ExportSeries, "series"}} {
		rows, err := q.QueryContext(ctx, `SELECT id, empreinte FROM `+t.table+` WHERE fournisseur_id = ?`, f)
		if err != nil {
			return nil, fmt.Errorf("stockage : empreintes de %s : %w", t.table, err)
		}
		m := map[string]string{}
		for rows.Next() {
			var id, e string
			if err := rows.Scan(&id, &e); err != nil {
				rows.Close()
				return nil, fmt.Errorf("stockage : empreintes de %s : %w", t.table, err)
			}
			m[id] = e
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, fmt.Errorf("stockage : empreintes de %s : %w", t.table, err)
		}
		res[t.typ] = m
	}
	return res, nil
}

// journaliserVersion enregistre la nouvelle version globale et, si un journal
// existait deja (sinon il n'y a rien a comparer), les identifiants touches par
// rapport aux empreintes d'avant. Puis purge au-dela de l'historique garde.
func (b *Base) journaliserVersion(ctx context.Context, tx *sql.Tx, s *domain.Instantane, e empreintesInstantane,
	anciennes map[domain.TypeExport]map[string]string, version string) error {
	res, err := tx.ExecContext(ctx, `INSERT INTO catalogue_versions (version, pris) VALUES (?, ?)`, version, millis(s.Pris))
	if err != nil {
		return fmt.Errorf("stockage : journal des versions : %w", err)
	}
	numero, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("stockage : journal des versions : %w", err)
	}
	if anciennes != nil {
		st, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO catalogue_differences (numero, type, fournisseur_id, id) VALUES (?, ?, ?, ?)`)
		if err != nil {
			return fmt.Errorf("stockage : journal des differences : %w", err)
		}
		defer st.Close()
		noter := func(t domain.TypeExport, id string) error {
			_, err := st.ExecContext(ctx, numero, string(t), s.FournisseurID, id)
			return err
		}
		differences := func(t domain.TypeExport, n int, id func(int) string, emp []string) error {
			vus := make(map[string]bool, n)
			for i := 0; i < n; i++ {
				k := id(i)
				vus[k] = true
				if anciennes[t][k] != emp[i] { // nouvelle, ou modifiee
					if err := noter(t, k); err != nil {
						return err
					}
				}
			}
			for k := range anciennes[t] {
				if !vus[k] { // supprimee
					if err := noter(t, k); err != nil {
						return err
					}
				}
			}
			return nil
		}
		if err := differences(domain.ExportLive, len(s.Chaines), func(i int) string { return string(s.Chaines[i].ID) }, e.chaines); err != nil {
			return fmt.Errorf("stockage : journal des differences : %w", err)
		}
		if err := differences(domain.ExportVOD, len(s.Films), func(i int) string { return string(s.Films[i].ID) }, e.films); err != nil {
			return fmt.Errorf("stockage : journal des differences : %w", err)
		}
		if err := differences(domain.ExportSeries, len(s.Series), func(i int) string { return string(s.Series[i].ID) }, e.series); err != nil {
			return fmt.Errorf("stockage : journal des differences : %w", err)
		}
	}
	// Purge : on garde les `historique` versions les plus recentes. Les differences
	// portant le numero de la plus ancienne gardee sont celles qui y MENENT : inutiles
	// pour partir d'elle, elles partent aussi.
	if _, err := tx.ExecContext(ctx, `DELETE FROM catalogue_versions WHERE numero <= (
			SELECT numero FROM catalogue_versions ORDER BY numero DESC LIMIT 1 OFFSET ?)`, b.historique); err != nil {
		return fmt.Errorf("stockage : purge des versions : %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM catalogue_differences WHERE numero <= (SELECT MIN(numero) FROM catalogue_versions)`); err != nil {
		return fmt.Errorf("stockage : purge des differences : %w", err)
	}
	return nil
}

// OuvrirDifferences ouvre l'export des lignes d'un type ajoutees ou modifiees depuis
// la version globale `depuis`, et renvoie les identifiants supprimes depuis. Tout
// est lu dans le MEME instantane. Une version inconnue (jamais vue, ou purgee de
// l'historique) rend domain.ErrNonTrouve : le client doit refaire un import complet.
func (b *Base) OuvrirDifferences(ctx context.Context, t domain.TypeExport, live, depuis string) (domain.Export, []string, error) {
	// Les colonnes partagees avec les listes paginees portent leur alias (c pour les
	// chaines, t pour films et series) : on le reprend tel quel.
	var table, alias, ordre, cols string
	var lire func(*sql.Rows) (any, error)
	switch t {
	case domain.ExportLive:
		table, alias, cols, ordre = "chaines", "c", colonnesChaine, `c.fournisseur_id, c.categorie_id, c.ordre, c.id`
		lire = func(r *sql.Rows) (any, error) { return lireChaine(r) }
	case domain.ExportVOD:
		table, alias, cols, ordre = "films", "t", colonnesFilm, `t.fournisseur_id, t.categorie_id, t.rowid`
		lire = func(r *sql.Rows) (any, error) { return lireFilm(r) }
	case domain.ExportSeries:
		table, alias, cols, ordre = "series", "t", colonnesSerie, `t.fournisseur_id, t.categorie_id, t.rowid`
		lire = func(r *sql.Rows) (any, error) { return lireSerie(r) }
	default:
		return nil, nil, fmt.Errorf("stockage : type d'export %q sans difference", t)
	}
	l, err := b.lecture(ctx)
	if err != nil {
		return nil, nil, err
	}
	numero, err := resoudreVersion(ctx, l.c, depuis)
	if err != nil {
		l.fermer()
		return nil, nil, err
	}
	// Restriction au fournisseur du direct : sur les differences ET sur les lignes.
	filtre, args := ``, []any{numero, string(t)}
	if live != "" && t == domain.ExportLive {
		filtre = ` AND d.fournisseur_id = ?`
		args = append(args, live)
	}
	touches := `SELECT d.id FROM catalogue_differences d WHERE d.numero > ? AND d.type = ?` + filtre
	rows, err := l.c.QueryContext(ctx, `SELECT DISTINCT d.id FROM catalogue_differences d LEFT JOIN `+table+` c ON c.id = d.id
		WHERE d.numero > ? AND d.type = ?`+filtre+` AND c.id IS NULL ORDER BY d.id`, args...)
	if err != nil {
		l.fermer()
		return nil, nil, fmt.Errorf("stockage : suppressions : %w", err)
	}
	supprimes := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			l.fermer()
			return nil, nil, fmt.Errorf("stockage : suppressions : %w", err)
		}
		supprimes = append(supprimes, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		l.fermer()
		return nil, nil, fmt.Errorf("stockage : suppressions : %w", err)
	}
	e := &export{ctx: ctx, l: l, lire: lire, args: args}
	e.req = `SELECT ` + cols + ` FROM ` + table + ` ` + alias + ` WHERE ` + alias + `.id IN (` + touches + `) ORDER BY ` + ordre
	if err := l.c.QueryRowContext(ctx, `SELECT COUNT(*) FROM `+table+` WHERE id IN (`+touches+`)`, args...).Scan(&e.n); err != nil {
		l.fermer()
		return nil, nil, fmt.Errorf("stockage : differences : %w", err)
	}
	return e, supprimes, nil
}

// resoudreVersion : numero de journal de la version `depuis`. La version courante,
// meme absente du journal (base migree, pas encore reecrite), vaut « rien depuis ».
func resoudreVersion(ctx context.Context, c *sql.Conn, depuis string) (int64, error) {
	var n sql.NullInt64
	if err := c.QueryRowContext(ctx, `SELECT MAX(numero) FROM catalogue_versions WHERE version = ?`, depuis).Scan(&n); err != nil {
		return 0, fmt.Errorf("stockage : version : %w", err)
	}
	if n.Valid {
		return n.Int64, nil
	}
	courante, err := empreinteGlobale(ctx, c)
	if err != nil {
		return 0, err
	}
	if depuis == "" || depuis != courante {
		return 0, domain.ErrNonTrouve
	}
	if err := c.QueryRowContext(ctx, `SELECT COALESCE(MAX(numero), 0) FROM catalogue_versions`).Scan(&n); err != nil {
		return 0, fmt.Errorf("stockage : version : %w", err)
	}
	return n.Int64, nil
}
