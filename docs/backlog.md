# Backlog mytv — tenu par le coordinateur

Legende statut : `A FAIRE` · `EN COURS` · `A REVOIR` · `FUSIONNE` · `BLOQUE`.
Chaque US a des CRITERES D'ACCEPTATION numerotes (CA). Un CA n'est « couvert » que
s'il existe un test automatique qui l'exerce (voir docs/couverture.md).

Contexte de la plateforme et mesures : voir docs/contexte.md. Contrats : internal/domain/domain.go.

## Lot 1 (en cours)

### US-01 — Stockage SQLite du catalogue · codeur-a · A FAIRE
Paquet `internal/stockage`. Implemente `domain.Stockage`. SQLite en Go pur (`modernc.org/sqlite`), mode WAL.
- CA1 Schema cree par migrations versionnees (table `migrations`), rejouables sans erreur.
- CA2 `EcrireCatalogue` remplace atomiquement le catalogue d'UN fournisseur (transaction) ; un echec en cours d'ecriture laisse l'ancien catalogue intact (test avec erreur injectee).
- CA3 Les catalogues de deux fournisseurs coexistent et ne se melangent pas ; les ContenuID sont uniques.
- CA4 `Chaines` : filtre par categorie, recherche texte insensible a la casse et aux accents (FTS5 ou equivalent), pagination, tri stable par ordre.
- CA5 `Categories(type)` ordonnees ; `Chaine(id)` renvoie `nil, ErrNonTrouve` si absente.
- CA6 `DerniereSynchro` renvoie le zero value si jamais ecrit.
- CA7 Bench : ecrire 25 000 chaines + 40 000 films en moins de 5 s, recherche en moins de 50 ms (test de benchmark, resultat consigne dans le message de commit).
- CA8 Acces concurrents (lectures pendant une ecriture) sans erreur `database is locked` (test -race).

### US-02 — Synchronisation du catalogue · codeur-a · A FAIRE (apres US-01)
Paquet `internal/catalogue`. Depend uniquement de `domain.Fournisseur` et `domain.Stockage`.
- CA1 `Synchroniseur.Synchroniser(ctx, fournisseurID)` telecharge puis ecrit ; en cas d'echec du fournisseur l'ancien catalogue est conserve et l'erreur est renvoyee.
- CA2 Respect de `rafraichissement.catalogue` : `SiPerime` ne telecharge pas si le catalogue est plus recent que l'age maximal.
- CA3 Planification quotidienne a `rafraichissement.heure` (fuseau local) et `au_demarrage` ; horloge injectable pour les tests.
- CA4 Deux demandes simultanees pour un meme fournisseur ne font QU'UN telechargement (singleflight).
- CA5 Un fournisseur inactif n'est jamais synchronise ; un fournisseur en echec n'empeche pas les autres.
- CA6 Journalise duree et volumes (slog), sans jamais journaliser d'identifiants.

### US-03 — Client Xtream · codeur-b · A FAIRE
Paquet `internal/xtream`. Implemente `domain.Fournisseur`.
- CA1 `Catalogue` appelle get_live_categories/get_live_streams/get_vod_*/get_series_* et produit un `Instantane` avec ContenuID `<id>:<stream_id>`.
- CA2 Tolerant au JSON du fournisseur : nombres ecrits en chaines, champs absents ou `null`, `category_id` nombre ou chaine, `added` en chaine de secondes (jeux de donnees de test varies dans testdata/).
- CA3 Statuts traduits en erreurs typees (401/403 identifiants, 404 introuvable, 407 places, 458 IP, 5xx/delai amont) ; verifie sur le faux fournisseur (US-04).
- CA4 `Requete` : redirections suivies (max 10), en-tetes Chrome par defaut ou `user_agent` configure, delais issus de la config (`reseau.*`), `Accept-Encoding` gere, corps jamais lu en entier en memoire.
- CA5 Aucun identifiant dans les erreurs ni les journaux (test qui cherche le mot de passe dans les messages).
- CA6 `URLLive(numero)` produit `<base>/live/<u>/<p>/<numero>.m3u8` avec echappement correct.

### US-04 — Faux fournisseur de test · codeur-b · A FAIRE (a livrer EN PREMIER, les autres en dependent)
Paquet `internal/testutil/fauxiptv`.
- CA1 Serveur `httptest` qui simule l'API Xtream (catalogue configurable) ET le HLS : playlist avec fenetre glissante, segments TS factices de taille configurable.
- CA2 Le hash de chemin des segments change a CHAQUE playlist (`/hls/<hash>/<id>_<n>.ts`), comme le vrai fournisseur.
- CA3 Pannes programmables : latence, 407 pour N demandes ou pendant une duree, 458, 502, coupure en milieu de segment, corps vide, 405.
- CA4 Mode « une seule connexion » : refuse (407) une requete de playlist/segment d'une AUTRE ressource tant que la precedente n'a pas expire (duree configurable), pour reproduire le zapping.
- CA5 Compteurs d'appels exposes pour les assertions.

### US-05 — Baux de connexion · codeur-b · A FAIRE
Paquet `internal/baux`. Implemente `domain.Baux` en memoire.
- CA1 Au plus `connexions_max` baux simultanes par fournisseur.
- CA2 Une session qui acquiert une AUTRE ressource reutilise son bail (zapping) sans compter deux places.
- CA3 Un bail non renouvele expire apres `bail.ttl` ; `Liberer` est immediat et idempotent.
- CA4 Si aucune place : attente jusqu'a `bail.patience_max` en reessayant toutes les `bail.reessai`, puis `ErrPlaceIndisponible` ; annulation par `ctx` respectee.
- CA5 Horloge injectable ; tests de concurrence sous `-race` (aucune fuite de goroutine, compteur jamais > max).
- CA6 Fournisseurs independants : saturer l'un ne bloque pas l'autre.

### US-06 — Reecriture HLS signee · codeur-c · A FAIRE
Paquet `internal/hls`. Port de /opt/mytv-relais/hls.go.
- CA1 Chaque URL de segment (et attribut `URI="..."`) est reecrite vers une URL du service, signee par HMAC ; verification refuse tout jeton falsifie.
- CA2 URLs RELATIVES a la racine (`/api/v1/...`), donc valables depuis le domaine public, le LAN ou un Chromecast.
- CA3 **Stabilite** : pour un meme numero de segment, la ligne reecrite est IDENTIQUE d'une playlist a l'autre meme si le hash de chemin du fournisseur change (regression hls.js « media sequence mismatch »). Table nom de segment -> derniere URL reelle, bornee et purgee.
- CA4 Playlists maitresses : les sous-playlists sont reecrites a leur tour.
- CA5 Aucune URL externe n'est jamais lisible sans signature (pas de relais ouvert).

### US-07 — Relais du direct · codeur-c · A FAIRE (apres US-06)
Paquet `internal/live`. Port de /opt/mytv-relais/main.go, adapte aux contrats `domain`.
- CA1 Playlist : nouveaux essais (config), une seule requete amont pour N demandes simultanees (singleflight), fraicheur `live.fraicheur_playlist`.
- CA2 Echec : la derniere bonne playlist est servie en secours si plus jeune que `live.peremption_secours`, avec en-tete `X-Relais: secours` ; sinon 502 franc.
- CA3 Segments : copie en flux vers le client, mise en cache bornee (nombre ET octets), relecture servie depuis le cache avec `X-Relais: cache`, 2 essais, `Range` transmis.
- CA4 Toute requete amont passe par un bail (`domain.Baux`) renouvele ; la fin de lecture (client parti) libere le bail.
- CA5 407 : le service patiente (config) au lieu d'echouer au premier refus, et le zapping reutilise le bail (scenario complet avec le faux fournisseur en mode « une seule connexion »).
- CA6 Statistiques (`/etat`) : mediane/p95/max playlists et segments, nombre d'essais, secours.
- CA7 Aucune route ne repond sans authentification valide (refus par defaut).

## Lot 2 (a planifier par le coordinateur apres fusion du lot 1)
US-08 Comptes et sessions · US-09 EPG (XMLTV, rapprochement) · US-10 VOD/series avec Range et remux (reprise du service Python) · US-11 Contrat OpenAPI v1 et routes de catalogue · US-12 Assemblage `cmd/mytv` · US-13 Etat par profil (favoris, historique) · US-14 Telemetrie.
