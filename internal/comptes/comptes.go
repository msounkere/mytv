// Package comptes gere utilisateurs, profils et sessions (US-08).
//
// Choix de securite, en bref :
//   - mots de passe : argon2id, parametres de la config, ecrits dans l'empreinte ;
//   - session : jeton aleatoire de 256 bits dans un cookie, stocke SEULEMENT haché
//     (SHA-256 suffit : le jeton est deja aleatoire, il n'y a rien a deviner) ;
//   - connexion : meme erreur pour compte inconnu et mauvais mot de passe, et un
//     faux calcul d'empreinte pour que le temps de reponse ne distingue pas les deux ;
//   - brute-force : limiteur par compte ET par adresse (voir limiteur.go).
//
// Aucun mot de passe ni jeton ne figure dans une erreur ou un journal.
package comptes

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite" // pilote "sqlite"

	"mytv/internal/config"
	"mytv/internal/domain"
)

// ErrMotDePasseFaible : mot de passe trop court (config comptes.mot_de_passe_min) ou trop long.
var ErrMotDePasseFaible = errors.New("comptes : mot de passe trop court ou trop long")

// ErrNomInvalide : nom d'utilisateur ou de profil refuse.
var ErrNomInvalide = errors.New("comptes : nom invalide")

// ErrIntrouvable : utilisateur, profil ou session absent (ou n'appartenant pas a l'appelant).
var ErrIntrouvable = errors.New("comptes : introuvable")

var reNom = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} ._@+-]{0,62}$`)

type horlogeSysteme struct{}

func (horlogeSysteme) Maintenant() time.Time { return time.Now() }

// Options : tout ce dont le service depend.
type Options struct {
	DB          *sql.DB
	Config      config.Comptes
	Terminateur domain.Terminateur // optionnel : libere les baux d'une session revoquee
	Horloge     domain.Horloge     // optionnel
	Journal     *slog.Logger       // optionnel
}

type Service struct {
	db      *sql.DB
	cfg     config.Comptes
	term    domain.Terminateur
	horloge domain.Horloge
	journal *slog.Logger
	lim     *limiteur
	params  parametres
	// faux : empreinte jetable, verifiee quand le compte n'existe pas.
	faux string
	// calculs borne le nombre de hachages simultanes (chacun coute plusieurs Mio).
	calculs chan struct{}
	reseau  reseau
}

var _ domain.Comptes = (*Service)(nil)

// OuvrirBase ouvre un fichier SQLite avec les memes reglages que le stockage.
func OuvrirBase(chemin string) (*sql.DB, error) {
	q := url.Values{}
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "busy_timeout(10000)")
	q.Add("_pragma", "synchronous(NORMAL)")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+chemin+"?"+q.Encode())
	if err != nil {
		return nil, fmt.Errorf("comptes : ouverture : %w", err)
	}
	return db, nil
}

// Nouveau applique les migrations puis rend le service.
func Nouveau(ctx context.Context, o Options) (*Service, error) {
	c := o.Config
	if c.Tentatives.Seuil < 1 || c.Tentatives.Fenetre <= 0 || c.DureeSession <= 0 {
		return nil, errors.New("comptes : tentatives.seuil, tentatives.fenetre et duree_session doivent etre positifs")
	}
	if c.Argon2.MemoireKio == 0 || c.Argon2.Iterations == 0 || c.Argon2.Parallelisme == 0 {
		return nil, errors.New("comptes : parametres argon2 incomplets")
	}
	r, err := analyserReseau(c.PlagesLocales, c.ProxysFiables)
	if err != nil {
		return nil, err
	}
	s := &Service{
		db: o.DB, cfg: c, term: o.Terminateur, horloge: o.Horloge, journal: o.Journal, reseau: r,
		params: parametres{c.Argon2.MemoireKio, c.Argon2.Iterations, c.Argon2.Parallelisme},
	}
	if s.horloge == nil {
		s.horloge = horlogeSysteme{}
	}
	if s.journal == nil {
		s.journal = slog.New(slog.DiscardHandler)
	}
	s.lim = nouveauLimiteur(c.Tentatives.Seuil, c.Tentatives.Fenetre.Std(), s.horloge.Maintenant)
	s.calculs = make(chan struct{}, max(2, runtime.NumCPU()))
	if err := s.migrer(ctx); err != nil {
		return nil, err
	}
	// Mot de passe aleatoire : personne ne peut le deviner, il sert juste a payer le meme calcul.
	if s.faux, err = hacher(aleatoire(16), s.params); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Service) migrer(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS comptes_migrations (
		version INTEGER PRIMARY KEY, nom TEXT NOT NULL, appliquee INTEGER NOT NULL) STRICT`); err != nil {
		return fmt.Errorf("comptes : table de migrations : %w", err)
	}
	var courante int
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(version), 0) FROM comptes_migrations`).Scan(&courante); err != nil {
		return fmt.Errorf("comptes : version du schema : %w", err)
	}
	if derniere := migrations[len(migrations)-1].version; courante > derniere {
		return fmt.Errorf("comptes : base en version %d, plus recente que ce programme (%d)", courante, derniere)
	}
	for _, m := range migrations {
		if m.version <= courante {
			continue
		}
		if err := s.appliquerMigration(ctx, m); err != nil {
			return fmt.Errorf("comptes : migration %d (%s) : %w", m.version, m.nom, err)
		}
	}
	return nil
}

func (s *Service) appliquerMigration(ctx context.Context, m migration) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var deja int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM comptes_migrations WHERE version = ?`, m.version).Scan(&deja); err != nil {
		return err
	}
	if deja > 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, m.sql); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO comptes_migrations (version, nom, appliquee) VALUES (?, ?, ?)`,
		m.version, m.nom, s.horloge.Maintenant().UnixMilli()); err != nil {
		return err
	}
	return tx.Commit()
}

// ---------------------------------------------------------------- utilitaires

func aleatoire(octets int) string {
	b := make([]byte, octets)
	if _, err := rand.Read(b); err != nil {
		panic("comptes : source aleatoire indisponible : " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func nouvelID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic("comptes : source aleatoire indisponible : " + err.Error())
	}
	return hex.EncodeToString(b)
}

func hacherJeton(jeton string) string {
	h := sha256.Sum256([]byte(jeton))
	return hex.EncodeToString(h[:])
}

func cleNom(nom string) string { return strings.ToLower(strings.TrimSpace(nom)) }

func validerNom(nom string) error {
	nom = strings.TrimSpace(nom)
	if !utf8.ValidString(nom) || !reNom.MatchString(nom) {
		return ErrNomInvalide
	}
	return nil
}

func (s *Service) validerMotDePasse(mdp string) error {
	if len(mdp) > longueurMax || utf8.RuneCountInString(mdp) < s.cfg.MotDePasseMin {
		return ErrMotDePasseFaible
	}
	return nil
}

// calculer prend un jeton de la file des calculs d'empreinte (borne la memoire).
func calculer[T any](ctx context.Context, s *Service, f func() T) (T, error) {
	select {
	case s.calculs <- struct{}{}:
		defer func() { <-s.calculs }()
		return f(), nil
	case <-ctx.Done():
		var zero T
		return zero, ctx.Err()
	}
}

func (s *Service) hacher(ctx context.Context, mdp string) (string, error) {
	type r struct {
		h   string
		err error
	}
	v, err := calculer(ctx, s, func() r { h, e := hacher(mdp, s.params); return r{h, e} })
	if err != nil {
		return "", err
	}
	return v.h, v.err
}

func (s *Service) verifier(ctx context.Context, mdp, empreinte string) (bool, parametres, error) {
	type r struct {
		ok bool
		p  parametres
	}
	v, err := calculer(ctx, s, func() r { ok, p := verifier(mdp, empreinte); return r{ok, p} })
	return v.ok, v.p, err
}

// ---------------------------------------------------------------- utilisateurs

func (s *Service) CreerUtilisateur(ctx context.Context, nom, motDePasse string, admin bool) (*domain.Utilisateur, error) {
	if err := validerNom(nom); err != nil {
		return nil, err
	}
	if err := s.validerMotDePasse(motDePasse); err != nil {
		return nil, err
	}
	emp, err := s.hacher(ctx, motDePasse)
	if err != nil {
		return nil, err
	}
	nom = strings.TrimSpace(nom)
	u := &domain.Utilisateur{ID: nouvelID(), Nom: nom, Admin: admin, Cree: s.horloge.Maintenant().UTC()}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("comptes : creation : %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO utilisateurs (id, nom, nom_cle, empreinte, admin, cree) VALUES (?, ?, ?, ?, ?, ?)`,
		u.ID, u.Nom, cleNom(nom), emp, boolEntier(admin), u.Cree.UnixMilli()); err != nil {
		if estUnicite(err) {
			return nil, ErrExiste
		}
		return nil, fmt.Errorf("comptes : creation : %w", err)
	}
	// Un premier profil au nom du compte : on peut se connecter et lire tout de suite.
	if _, err := tx.ExecContext(ctx, `INSERT INTO profils (id, utilisateur_id, nom, nom_cle, cree) VALUES (?, ?, ?, ?, ?)`,
		nouvelID(), u.ID, u.Nom, cleNom(nom), u.Cree.UnixMilli()); err != nil {
		return nil, fmt.Errorf("comptes : creation du profil : %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("comptes : creation : %w", err)
	}
	s.journal.Info("utilisateur cree", "utilisateur", u.ID, "admin", admin)
	return u, nil
}

// ErrExiste est re-exportee pour les appelants de ce paquet.
var ErrExiste = domain.ErrExiste

func estUnicite(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

func boolEntier(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------------------------------------------------------------- connexion

func (s *Service) Connecter(ctx context.Context, nom, motDePasse, adresse string) (string, *domain.Session, error) {
	cleC, cleA := "c:"+cleNom(nom), cleAdresse(adresse)
	if len(cleC) > 80 {
		cleC = cleC[:80] // un nom geant ne doit pas gonfler la table du limiteur
	}
	if !s.lim.tenter(cleC, cleA) {
		return "", nil, domain.ErrTropDeTentatives
	}

	var (
		id, empreinte string
		admin         int
	)
	err := s.db.QueryRowContext(ctx, `SELECT id, empreinte, admin FROM utilisateurs WHERE nom_cle = ?`, cleNom(nom)).
		Scan(&id, &empreinte, &admin)
	connu := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", nil, fmt.Errorf("comptes : lecture du compte : %w", err)
	}
	if !connu {
		empreinte = s.faux
	}
	if len(motDePasse) > longueurMax {
		motDePasse = motDePasse[:longueurMax] // meme cout, jamais accepte pour un compte reel
		connu = false
	}
	ok, p, err := s.verifier(ctx, motDePasse, empreinte)
	if err != nil {
		return "", nil, err
	}
	if !ok || !connu {
		return "", nil, domain.ErrIdentifiantsInvalides
	}
	s.lim.reussir(cleC, cleA)

	if p != s.params {
		s.renouvelerEmpreinte(ctx, id, motDePasse)
	}
	jeton := aleatoire(32)
	sess, err := s.ouvrirSession(ctx, id, admin == 1, hacherJeton(jeton))
	if err != nil {
		return "", nil, err
	}
	s.journal.Info("connexion", "session", sess.ID, "utilisateur", id)
	return jeton, sess, nil
}

// renouvelerEmpreinte remplace une empreinte calculee avec d'anciens parametres.
// Un echec n'empeche pas la connexion : il sera retente a la suivante.
func (s *Service) renouvelerEmpreinte(ctx context.Context, id, mdp string) {
	emp, err := s.hacher(ctx, mdp)
	if err != nil {
		return
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE utilisateurs SET empreinte = ? WHERE id = ?`, emp, id); err != nil {
		s.journal.Warn("empreinte non renouvelee", "utilisateur", id)
	}
}

func (s *Service) ouvrirSession(ctx context.Context, utilisateurID string, admin bool, jetonHash string) (*domain.Session, error) {
	now := s.horloge.Maintenant()
	sess := &domain.Session{ID: nouvelID(), UtilisateurID: utilisateurID, Admin: admin,
		Expire: now.Add(s.cfg.DureeSession.Std()).UTC()}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("comptes : session : %w", err)
	}
	defer tx.Rollback()
	// Profil actif : le plus ancien (celui cree avec le compte).
	var profil sql.NullString
	if err := tx.QueryRowContext(ctx, `SELECT id FROM profils WHERE utilisateur_id = ? ORDER BY rowid LIMIT 1`, utilisateurID).Scan(&profil); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("comptes : session : %w", err)
	}
	sess.ProfilID = profil.String
	var pid any
	if profil.Valid {
		pid = profil.String
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO sessions (id, jeton_hash, utilisateur_id, profil_id, cree, expire) VALUES (?, ?, ?, ?, ?, ?)`,
		sess.ID, jetonHash, utilisateurID, pid, now.UnixMilli(), sess.Expire.UnixMilli()); err != nil {
		return nil, fmt.Errorf("comptes : session : %w", err)
	}
	// Menage au passage : les sessions expirees n'ont plus aucune utilite.
	if _, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE expire <= ?`, now.UnixMilli()); err != nil {
		return nil, fmt.Errorf("comptes : session : %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("comptes : session : %w", err)
	}
	return sess, nil
}

func (s *Service) Session(ctx context.Context, jeton string) (*domain.Session, error) {
	if jeton == "" || len(jeton) > 256 {
		return nil, domain.ErrSessionInvalide
	}
	var (
		sess   domain.Session
		profil sql.NullString
		admin  int
		expire int64
	)
	err := s.db.QueryRowContext(ctx, `SELECT s.id, s.utilisateur_id, s.profil_id, u.admin, s.expire
		FROM sessions s JOIN utilisateurs u ON u.id = s.utilisateur_id WHERE s.jeton_hash = ?`, hacherJeton(jeton)).
		Scan(&sess.ID, &sess.UtilisateurID, &profil, &admin, &expire)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, domain.ErrSessionInvalide
	}
	if err != nil {
		return nil, fmt.Errorf("comptes : lecture de la session : %w", err)
	}
	if !s.horloge.Maintenant().Before(time.UnixMilli(expire)) {
		// Expiree : on la supprime et on libere ses baux, comme une deconnexion.
		s.revoquer(ctx, `id = ?`, sess.ID)
		return nil, domain.ErrSessionInvalide
	}
	sess.ProfilID, sess.Admin, sess.Expire = profil.String, admin == 1, time.UnixMilli(expire).UTC()
	return &sess, nil
}

// revoquer supprime les sessions qui verifient la condition et les termine
// aupres du Terminateur. Les identifiants sont lus AVANT la suppression.
func (s *Service) revoquer(ctx context.Context, condition string, args ...any) int {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM sessions WHERE `+condition, args...)
	if err != nil {
		return 0
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE `+condition, args...); err != nil {
		s.journal.Warn("revocation de sessions impossible")
		return 0
	}
	for _, id := range ids {
		s.terminer(id)
	}
	return len(ids)
}

func (s *Service) terminer(session string) {
	if s.term != nil {
		s.term.Terminer(session)
	}
}

func (s *Service) Deconnecter(ctx context.Context, jeton string) error {
	if jeton == "" {
		return nil
	}
	sess, err := s.Session(ctx, jeton)
	if errors.Is(err, domain.ErrSessionInvalide) {
		return nil // deja deconnecte : idempotent
	}
	if err != nil {
		return err
	}
	s.revoquer(ctx, `id = ?`, sess.ID)
	s.journal.Info("deconnexion", "session", sess.ID)
	return nil
}

func (s *Service) ChoisirProfil(ctx context.Context, sessionID, profilID string) error {
	// Un seul UPDATE : le profil doit appartenir a l'utilisateur de la session.
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET profil_id = ?
		WHERE id = ? AND EXISTS (SELECT 1 FROM profils p WHERE p.id = ? AND p.utilisateur_id = sessions.utilisateur_id)`,
		profilID, sessionID, profilID)
	if err != nil {
		return fmt.Errorf("comptes : choix du profil : %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrIntrouvable
	}
	return nil
}

func (s *Service) ChangerMotDePasse(ctx context.Context, utilisateurID, ancien, nouveau, sessionCouranteID string) error {
	if err := s.validerMotDePasse(nouveau); err != nil {
		return err
	}
	cle := "u:" + utilisateurID
	if !s.lim.tenter(cle) { // deviner l'ancien mot de passe depuis une session volee
		return domain.ErrTropDeTentatives
	}
	var empreinte string
	err := s.db.QueryRowContext(ctx, `SELECT empreinte FROM utilisateurs WHERE id = ?`, utilisateurID).Scan(&empreinte)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrIntrouvable
	}
	if err != nil {
		return fmt.Errorf("comptes : lecture du compte : %w", err)
	}
	ok, _, err := s.verifier(ctx, ancien, empreinte)
	if err != nil {
		return err
	}
	if !ok {
		return domain.ErrIdentifiantsInvalides
	}
	s.lim.reussir(cle, cle)
	emp, err := s.hacher(ctx, nouveau)
	if err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE utilisateurs SET empreinte = ? WHERE id = ?`, emp, utilisateurID); err != nil {
		return fmt.Errorf("comptes : changement de mot de passe : %w", err)
	}
	n := s.revoquer(ctx, `utilisateur_id = ? AND id <> ?`, utilisateurID, sessionCouranteID)
	s.journal.Info("mot de passe change", "utilisateur", utilisateurID, "sessions_revoquees", n)
	return nil
}

// ---------------------------------------------------------------- profils

func (s *Service) Profils(ctx context.Context, utilisateurID string) ([]domain.Profil, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, utilisateur_id, nom FROM profils WHERE utilisateur_id = ? ORDER BY rowid`, utilisateurID)
	if err != nil {
		return nil, fmt.Errorf("comptes : profils : %w", err)
	}
	defer rows.Close()
	var out []domain.Profil
	for rows.Next() {
		var p domain.Profil
		if err := rows.Scan(&p.ID, &p.UtilisateurID, &p.Nom); err != nil {
			return nil, fmt.Errorf("comptes : profils : %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Service) CreerProfil(ctx context.Context, utilisateurID, nom string) (*domain.Profil, error) {
	if err := validerNom(nom); err != nil {
		return nil, err
	}
	nom = strings.TrimSpace(nom)
	p := &domain.Profil{ID: nouvelID(), UtilisateurID: utilisateurID, Nom: nom}
	_, err := s.db.ExecContext(ctx, `INSERT INTO profils (id, utilisateur_id, nom, nom_cle, cree)
		SELECT ?, id, ?, ?, ? FROM utilisateurs WHERE id = ?`, p.ID, nom, cleNom(nom), s.horloge.Maintenant().UnixMilli(), utilisateurID)
	if estUnicite(err) {
		return nil, ErrExiste
	}
	if err != nil {
		return nil, fmt.Errorf("comptes : creation du profil : %w", err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM profils WHERE id = ?`, p.ID).Scan(&n); err != nil || n == 0 {
		return nil, ErrIntrouvable // utilisateur inconnu
	}
	return p, nil
}

func (s *Service) SupprimerProfil(ctx context.Context, utilisateurID, profilID string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("comptes : suppression du profil : %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM profils WHERE id = ? AND utilisateur_id = ?`, profilID, utilisateurID)
	if err != nil {
		return fmt.Errorf("comptes : suppression du profil : %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrIntrouvable
	}
	// Les sessions qui l'avaient pour profil actif n'en ont plus.
	if _, err := tx.ExecContext(ctx, `UPDATE sessions SET profil_id = NULL WHERE profil_id = ?`, profilID); err != nil {
		return fmt.Errorf("comptes : suppression du profil : %w", err)
	}
	return tx.Commit()
}

// Purger supprime les sessions expirees (a appeler de temps en temps ; Session le
// fait deja a la lecture, ceci evite l'accumulation de sessions jamais relues).
func (s *Service) Purger(ctx context.Context) int {
	return s.revoquer(ctx, `expire <= ?`, s.horloge.Maintenant().UnixMilli())
}
