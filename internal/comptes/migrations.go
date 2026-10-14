package comptes

// Les versions sont strictement croissantes ; une migration publiee ne se modifie
// JAMAIS, on en ajoute une. La table de suivi est propre au paquet : le schema
// des comptes evolue indepenement de celui du catalogue.
type migration struct {
	version int
	nom     string
	sql     string
}

var migrations = []migration{
	{
		version: 1,
		nom:     "comptes initiaux",
		sql: `
CREATE TABLE utilisateurs (
	id      TEXT PRIMARY KEY,
	nom     TEXT NOT NULL,
	nom_cle TEXT NOT NULL UNIQUE,   -- nom en minuscules : unicite insensible a la casse
	empreinte TEXT NOT NULL,        -- argon2id, jamais le mot de passe
	admin   INTEGER NOT NULL,
	cree    INTEGER NOT NULL        -- millisecondes Unix UTC
) STRICT;

CREATE TABLE profils (
	id             TEXT PRIMARY KEY,
	utilisateur_id TEXT NOT NULL REFERENCES utilisateurs(id) ON DELETE CASCADE,
	nom            TEXT NOT NULL,
	nom_cle        TEXT NOT NULL,
	cree           INTEGER NOT NULL,
	UNIQUE (utilisateur_id, nom_cle)
) STRICT;

-- jeton_hash : SHA-256 du jeton du cookie. Le jeton lui-meme n'est jamais ecrit.
CREATE TABLE sessions (
	id             TEXT PRIMARY KEY,
	jeton_hash     TEXT NOT NULL UNIQUE,
	utilisateur_id TEXT NOT NULL REFERENCES utilisateurs(id) ON DELETE CASCADE,
	profil_id      TEXT,
	cree           INTEGER NOT NULL,
	expire         INTEGER NOT NULL
) STRICT;
CREATE INDEX sessions_utilisateur ON sessions (utilisateur_id);
CREATE INDEX sessions_expire ON sessions (expire);
`,
	},
}
