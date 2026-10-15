package stockage

// migration est une etape du schema. Les versions sont strictement croissantes et
// une migration deja publiee ne se modifie JAMAIS : on en ajoute une nouvelle.
type migration struct {
	version int
	nom     string
	sql     string
}

var migrations = []migration{
	{
		version: 1,
		nom:     "catalogue initial",
		sql: `
CREATE TABLE synchros (
	fournisseur_id TEXT PRIMARY KEY,
	pris           INTEGER            -- millisecondes Unix UTC ; NULL = inconnu
) STRICT;

CREATE TABLE categories (
	fournisseur_id TEXT NOT NULL,
	type           TEXT NOT NULL,
	id             TEXT NOT NULL,
	nom            TEXT NOT NULL,
	ordre          INTEGER NOT NULL,
	PRIMARY KEY (fournisseur_id, type, id)
) STRICT;
CREATE INDEX categories_ordre ON categories (type, ordre, fournisseur_id, id);

-- id est la cle primaire GLOBALE : deux fournisseurs ne peuvent pas partager un
-- ContenuID (il est prefixe par le fournisseur), et un doublon est une erreur
-- qui annule l'ecriture.
CREATE TABLE chaines (
	id             TEXT PRIMARY KEY,
	fournisseur_id TEXT NOT NULL,
	numero         TEXT NOT NULL,
	nom            TEXT NOT NULL,
	logo           TEXT NOT NULL,
	epg_id         TEXT NOT NULL,
	categorie_id   TEXT NOT NULL,
	numero_chaine  INTEGER NOT NULL,
	ordre          INTEGER NOT NULL
) STRICT;
CREATE INDEX chaines_fournisseur ON chaines (fournisseur_id);
CREATE INDEX chaines_ordre ON chaines (ordre, id);
CREATE INDEX chaines_categorie ON chaines (categorie_id, ordre, id);

CREATE TABLE films (
	id             TEXT PRIMARY KEY,
	fournisseur_id TEXT NOT NULL,
	numero         TEXT NOT NULL,
	nom            TEXT NOT NULL,
	image          TEXT NOT NULL,
	categorie_id   TEXT NOT NULL,
	extension      TEXT NOT NULL,
	note           REAL NOT NULL,
	annee          TEXT NOT NULL,
	ajoute         INTEGER            -- millisecondes Unix UTC ; NULL = inconnu
) STRICT;
CREATE INDEX films_fournisseur ON films (fournisseur_id);

CREATE TABLE series (
	id             TEXT PRIMARY KEY,
	fournisseur_id TEXT NOT NULL,
	numero         TEXT NOT NULL,
	nom            TEXT NOT NULL,
	image          TEXT NOT NULL,
	categorie_id   TEXT NOT NULL,
	note           REAL NOT NULL,
	annee          TEXT NOT NULL
) STRICT;
CREATE INDEX series_fournisseur ON series (fournisseur_id);

-- Recherche plein texte sur le nom des chaines. Le tokeniseur unicode61 avec
-- remove_diacritics=2 ignore la casse ET les accents (« Éclair » trouve « eclair »)
-- sans colonne normalisee a tenir a jour a la main.
CREATE VIRTUAL TABLE chaines_fts USING fts5 (
	nom,
	content = 'chaines',
	content_rowid = 'rowid',
	tokenize = 'unicode61 remove_diacritics 2'
);
CREATE TRIGGER chaines_ai AFTER INSERT ON chaines BEGIN
	INSERT INTO chaines_fts (rowid, nom) VALUES (new.rowid, new.nom);
END;
CREATE TRIGGER chaines_ad AFTER DELETE ON chaines BEGIN
	INSERT INTO chaines_fts (chaines_fts, rowid, nom) VALUES ('delete', old.rowid, old.nom);
END;
`,
	},
	{
		version: 2,
		nom:     "recherche plein texte des films et series",
		sql: `
CREATE VIRTUAL TABLE films_fts USING fts5 (
	nom, content = 'films', content_rowid = 'rowid', tokenize = 'unicode61 remove_diacritics 2'
);
CREATE TRIGGER films_ai AFTER INSERT ON films BEGIN
	INSERT INTO films_fts (rowid, nom) VALUES (new.rowid, new.nom);
END;
CREATE TRIGGER films_ad AFTER DELETE ON films BEGIN
	INSERT INTO films_fts (films_fts, rowid, nom) VALUES ('delete', old.rowid, old.nom);
END;
INSERT INTO films_fts (rowid, nom) SELECT rowid, nom FROM films;

CREATE VIRTUAL TABLE series_fts USING fts5 (
	nom, content = 'series', content_rowid = 'rowid', tokenize = 'unicode61 remove_diacritics 2'
);
CREATE TRIGGER series_ai AFTER INSERT ON series BEGIN
	INSERT INTO series_fts (rowid, nom) VALUES (new.rowid, new.nom);
END;
CREATE TRIGGER series_ad AFTER DELETE ON series BEGIN
	INSERT INTO series_fts (series_fts, rowid, nom) VALUES ('delete', old.rowid, old.nom);
END;
INSERT INTO series_fts (rowid, nom) SELECT rowid, nom FROM series;
`,
	},
	{
		version: 3,
		nom:     "date d'ajout des chaines et des series (export en bloc)",
		sql: `
ALTER TABLE chaines ADD COLUMN ajoute INTEGER;  -- millisecondes Unix UTC ; NULL = inconnu
ALTER TABLE series ADD COLUMN ajoute INTEGER;
`,
	},
	{
		version: 4,
		nom:     "details des films et des series (US-28)",
		sql: `
-- Details paresseux (get_vod_info / get_series_info). Table SEPAREE du catalogue et
-- SANS cle etrangere : EcrireCatalogue remplace films et series a chaque synchro, les
-- details deja recuperes doivent survivre. donnees = JSON des champs connus ; recupere
-- = millisecondes Unix UTC de la derniere recuperation reussie et non vide.
CREATE TABLE details_contenu (
	id       TEXT PRIMARY KEY,
	type     TEXT NOT NULL,    -- vod | series
	donnees  TEXT NOT NULL,
	recupere INTEGER NOT NULL
) STRICT;
`,
	},
}
