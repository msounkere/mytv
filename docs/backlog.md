# Backlog mytv — tenu par le coordinateur

Legende statut : `A FAIRE` · `EN COURS` · `A REVOIR` · `FUSIONNE` · `BLOQUE`.
Chaque US a des CRITERES D'ACCEPTATION numerotes (CA). Un CA n'est « couvert » que
s'il existe un test automatique qui l'exerce (voir docs/couverture.md).

Contexte de la plateforme et mesures : voir docs/contexte.md. Contrats : internal/domain/domain.go.

## Lot 1 (en cours)

### US-01 — Stockage SQLite du catalogue · codeur-a · FUSIONNE
Paquet `internal/stockage`. Implemente `domain.Stockage`. SQLite en Go pur (`modernc.org/sqlite`), mode WAL.
- CA1 Schema cree par migrations versionnees (table `migrations`), rejouables sans erreur.
- CA2 `EcrireCatalogue` remplace atomiquement le catalogue d'UN fournisseur (transaction) ; un echec en cours d'ecriture laisse l'ancien catalogue intact (test avec erreur injectee).
- CA3 Les catalogues de deux fournisseurs coexistent et ne se melangent pas ; les ContenuID sont uniques.
- CA4 `Chaines` : filtre par categorie, recherche texte insensible a la casse et aux accents (FTS5 ou equivalent), pagination, tri stable par ordre.
- CA5 `Categories(type)` ordonnees ; `Chaine(id)` renvoie `nil, ErrNonTrouve` si absente.
- CA6 `DerniereSynchro` renvoie le zero value si jamais ecrit.
- CA7 Bench : ecrire 25 000 chaines + 40 000 films en moins de 5 s, recherche en moins de 50 ms (test de benchmark, resultat consigne dans le message de commit).
- CA8 Acces concurrents (lectures pendant une ecriture) sans erreur `database is locked` (test -race).

### US-02 — Synchronisation du catalogue · codeur-a · FUSIONNE
Paquet `internal/catalogue`. Depend uniquement de `domain.Fournisseur` et `domain.Stockage`.
- CA1 `Synchroniseur.Synchroniser(ctx, fournisseurID)` telecharge puis ecrit ; en cas d'echec du fournisseur l'ancien catalogue est conserve et l'erreur est renvoyee.
- CA2 Respect de `rafraichissement.catalogue` : `SiPerime` ne telecharge pas si le catalogue est plus recent que l'age maximal.
- CA3 Planification quotidienne a `rafraichissement.heure` (fuseau local) et `au_demarrage` ; horloge injectable pour les tests.
- CA4 Deux demandes simultanees pour un meme fournisseur ne font QU'UN telechargement (singleflight).
- CA5 Un fournisseur inactif n'est jamais synchronise ; un fournisseur en echec n'empeche pas les autres.
- CA6 Journalise duree et volumes (slog), sans jamais journaliser d'identifiants.

### US-03 — Client Xtream · codeur-b · FUSIONNE
Paquet `internal/xtream`. Implemente `domain.Fournisseur`.
- CA1 `Catalogue` appelle get_live_categories/get_live_streams/get_vod_*/get_series_* et produit un `Instantane` avec ContenuID `<id>:<stream_id>`.
- CA2 Tolerant au JSON du fournisseur : nombres ecrits en chaines, champs absents ou `null`, `category_id` nombre ou chaine, `added` en chaine de secondes (jeux de donnees de test varies dans testdata/).
- CA3 Statuts traduits en erreurs typees (401/403 identifiants, 404 introuvable, 407 places, 458 IP, 5xx/delai amont) ; verifie sur le faux fournisseur (US-04).
- CA4 `Requete` : redirections suivies (max 10), en-tetes Chrome par defaut ou `user_agent` configure, delais issus de la config (`reseau.*`), `Accept-Encoding` gere, corps jamais lu en entier en memoire.
- CA5 Aucun identifiant dans les erreurs ni les journaux (test qui cherche le mot de passe dans les messages).
- CA6 `URLLive(numero)` produit `<base>/live/<u>/<p>/<numero>.m3u8` avec echappement correct.

### US-04 — Faux fournisseur de test · codeur-b · FUSIONNE (a livrer EN PREMIER, les autres en dependent)
Paquet `internal/testutil/fauxiptv`.
- CA1 Serveur `httptest` qui simule l'API Xtream (catalogue configurable) ET le HLS : playlist avec fenetre glissante, segments TS factices de taille configurable.
- CA2 Le hash de chemin des segments change a CHAQUE playlist (`/hls/<hash>/<id>_<n>.ts`), comme le vrai fournisseur.
- CA3 Pannes programmables : latence, 407 pour N demandes ou pendant une duree, 458, 502, coupure en milieu de segment, corps vide, 405.
- CA4 Mode « une seule connexion » : refuse (407) une requete de playlist/segment d'une AUTRE ressource tant que la precedente n'a pas expire (duree configurable), pour reproduire le zapping.
- CA5 Compteurs d'appels exposes pour les assertions.

### US-05 — Baux de connexion · codeur-b · FUSIONNE
Paquet `internal/baux`. Implemente `domain.Baux` en memoire.
- CA1 Au plus `connexions_max` baux simultanes par fournisseur.
- CA2 Une session qui acquiert une AUTRE ressource reutilise son bail (zapping) sans compter deux places.
- CA3 Un bail non renouvele expire apres `bail.ttl` ; `Liberer` est immediat et idempotent.
- CA4 Si aucune place : attente jusqu'a `bail.patience_max` en reessayant toutes les `bail.reessai`, puis `ErrPlaceIndisponible` ; annulation par `ctx` respectee.
- CA5 Horloge injectable ; tests de concurrence sous `-race` (aucune fuite de goroutine, compteur jamais > max).
- CA6 Fournisseurs independants : saturer l'un ne bloque pas l'autre.

### US-06 — Reecriture HLS signee · codeur-c · FUSIONNE
Paquet `internal/hls`. Port de /opt/mytv-relais/hls.go.
- CA1 Chaque URL de segment (et attribut `URI="..."`) est reecrite vers une URL du service, signee par HMAC ; verification refuse tout jeton falsifie.
- CA2 URLs RELATIVES a la racine (`/api/v1/...`), donc valables depuis le domaine public, le LAN ou un Chromecast.
- CA3 **Stabilite** : pour un meme numero de segment, la ligne reecrite est IDENTIQUE d'une playlist a l'autre meme si le hash de chemin du fournisseur change (regression hls.js « media sequence mismatch »). Table nom de segment -> derniere URL reelle, bornee et purgee.
- CA4 Playlists maitresses : les sous-playlists sont reecrites a leur tour.
- CA5 Aucune URL externe n'est jamais lisible sans signature (pas de relais ouvert).

### US-07 — Relais du direct · codeur-c · FUSIONNE
Paquet `internal/live`. Port de /opt/mytv-relais/main.go, adapte aux contrats `domain`.
- CA1 Playlist : nouveaux essais (config), une seule requete amont pour N demandes simultanees (singleflight), fraicheur `live.fraicheur_playlist`.
- CA2 Echec : la derniere bonne playlist est servie en secours si plus jeune que `live.peremption_secours`, avec en-tete `X-Relais: secours` ; sinon 502 franc.
- CA3 Segments : copie en flux vers le client, mise en cache bornee (nombre ET octets), relecture servie depuis le cache avec `X-Relais: cache`, 2 essais, `Range` transmis.
- CA4 Toute requete amont passe par un bail (`domain.Baux`) renouvele ; la fin de lecture (client parti) libere le bail.
- CA5 407 : le service patiente (config) au lieu d'echouer au premier refus, et le zapping reutilise le bail (scenario complet avec le faux fournisseur en mode « une seule connexion »).
- CA6 Statistiques (`/etat`) : mediane/p95/max playlists et segments, nombre d'essais, secours.
- CA7 Aucune route ne repond sans authentification valide (refus par defaut).

## Lot 2 (en cours)

Contrats poses sur main : `internal/domain/lot2.go` (comptes, EPG, etat par profil, VOD/remux, telemetrie) + `FiltreChaines.FournisseurID` + champs de config `api`, `profil`, `comptes.*`, `remux.*`, `live.*`, `serveur.hotes_segments_autorises`.

Repartition : codeur-a = US-02 (en cours), US-09, US-13 · codeur-b = US-10, US-14 · codeur-c = US-08, US-11, puis US-12 (assemblage, avec relecture croisee des autres).
Regle : toute nouvelle interface partagee (comptes, EPG, etat par profil, remux) passe par une demande de contrat (`<codeur>.contrat.demande`) AVANT de coder les appelants ; le coordinateur arbitre. Reglages chiffres : tous dans internal/config (le coordinateur ajoute les champs sur demande).

### US-08 — Comptes et sessions · codeur-c · FUSIONNE
Paquet `internal/comptes`. Fournit l'`Authentifier` du relais (session -> identifiant stable).
- CA1 Comptes et profils stockes en SQLite (migration propre au paquet, table versionnee) ; mots de passe haches (argon2id ou scrypt via x/crypto, parametres en config), jamais en clair, jamais journalises.
- CA2 Connexion : verification en temps constant ; message d'erreur identique pour utilisateur inconnu et mauvais mot de passe ; limitation de tentatives par adresse et par compte (fenetre et seuil en config) avec reponse 429.
- CA3 Session = cookie opaque aleatoire (>= 128 bits), HttpOnly, SameSite=Lax, Secure selon `comptes.cookie_securise` ; stockee hachee ; duree `comptes.duree_session` ; deconnexion immediate.
- CA4 Compte technique local (`comptes.local_utilisateur`) pour les lecteurs sans cookie (Cast) : n'est accepte que depuis les plages reseau configurees, jamais depuis l'exterieur.
- CA5 `Authentifier(req)` renvoie l'identifiant de session ; refus par defaut ; session expiree ou revoquee refusee.
- CA6 Creation / suppression de profils, un compte peut en avoir plusieurs ; changement de mot de passe revoque les autres sessions.
- CA7 Aucun secret dans les erreurs et journaux (test qui cherche le mot de passe et le jeton de session).
- CA8 (dette lot 1) `Deconnecter` appelle `domain.Terminateur.Terminer(session.ID)` injecte ; test avec doublure. Contrat : `domain.Comptes`, `domain.Session`, `domain.Terminateur`. Parametres argon2/tentatives/plages : `config.Comptes`. X-Forwarded-For ignore sauf `comptes.proxys_fiables`.

### US-09 — EPG (XMLTV, rapprochement) · codeur-a · FUSIONNE (CA8 StockageVideo reporte en US-13)
Paquet `internal/epg`.
- CA1 Telechargement XMLTV en flux (jamais en memoire entiere), gzip gere, via `domain.Fournisseur.Requete`.
- CA2 Analyse tolerante (fuseaux, dates `YYYYMMDDHHMMSS +0000`, champs absents, doublons) ; programmes hors fenetre ignores.
- CA3 Rapprochement chaine -> programmes par `EpgID` puis, a defaut, par nom normalise (casse, accents, suffixes HD/FHD) ; les ambigus sont signales, jamais devines au hasard.
- CA4 Stockage SQLite remplace atomiquement par fournisseur (comme US-01) ; echec = ancien EPG conserve.
- CA5 Requetes : « en cours et suivant » pour une chaine, grille par plage horaire ; resultats ordonnes.
- CA6 Frequence `rafraichissement.epg` respectee, horloge injectable, singleflight par fournisseur.
- CA7 Bench : 25 000 chaines / 500 000 programmes ingeres en moins de 30 s (consigne dans le commit).
- CA8 (dette lot 1) Premier commit : `stockage.Chaines` honore `FiltreChaines.FournisseurID` (test avec deux fournisseurs ayant la meme categorie) et implemente `domain.StockageVideo` (Films/Film/Series/Serie, meme filtre) pour US-11. Contrats : `domain.EPGStockage`, `InstantaneEPG`, `SourceEPG` (le client xtream de codeur-b expose `URLEPG()`).

### US-10 — VOD/series : lecture avec Range et remux · codeur-b · A FAIRE
Paquet `internal/vod` (remplace le service Python de remux).
- CA1 Film lisible navigateur (mp4/h264/aac) : relais en flux avec `Range` transmis, 206 correct, `Content-Range` coherent, annulation client = fermeture amont.
- CA2 Sonde (ffprobe) du codec/conteneur, resultat mis en cache `remux.duree_sonde` ; decide entre relais direct et remux.
- CA3 Remux/transcodage audio via ffmpeg (MKV/EAC3/HEVC -> mp4 fragmente), nombre de processus <= `remux.processus_max`, file d'attente bornee, arret propre du processus quand le client part (aucun zombie).
- CA4 Le bail fournisseur (domain.Baux) est pris pour toute lecture et libere a la fin.
- CA5 Aucun chemin construit a partir d'une entree utilisateur n'atteint ffmpeg sans validation (pas d'injection d'arguments, URL amont uniquement issues de l'identifiant du contenu).
- CA6 Tests avec un faux ffmpeg (script de test) ET, si ffmpeg est present, un test reel marque `-short` exclu.
- CA7 Series : liste des saisons/episodes (get_series_info) tolerante au JSON, comme US-03. Contrat : `domain.FournisseurVOD` a implementer sur le client xtream.
- CA8 Contrats : `Sondeur`, `Decideur`, `Lanceur`/`Processus`/`TacheRemux`, `ServiceVOD`. La file d'attente de remux et son attente sont `remux.file_max` / `remux.attente_file`.
- CA9 (dette lot 1) `internal/xtream.Requete` n'accepte que les schemas http/https (test file://, ftp://, redirection vers un autre schema).
- CA10 (dette lot 1) `baux.Terminer(session)` (libere tous les baux de la session, idempotent) implemente `domain.Terminateur` ; test.
- CA11 (dette lot 1) La patience d'un bail et celle du 407 ne s'additionnent pas : une SEULE patience plafonnee a `bail.patience_max` pour une requete (test chronometre avec horloge injectee). A coordonner avec `internal/live` (revue croisee avec codeur-c).

### US-11 — Contrat OpenAPI v1 et routes de catalogue · codeur-c · A FAIRE
Paquets `docs/openapi.yaml` (contrat valide), `internal/api`. Ecart assume : le relais live est mono-fournisseur (le plus prioritaire).
- CA1 Fichier OpenAPI 3 decrivant toutes les routes v1 (catalogue, live, vod, epg, compte, etat), erreurs normalisees `{code, message}`.
- CA2 Routes de catalogue : categories, chaines (filtre/recherche/pagination), film, serie, depuis `domain.Stockage`.
- CA3 Test de conformite : chaque route reelle existe dans le contrat et inversement (test qui compare).
- CA4 Authentification obligatoire sur tout sauf connexion et sante ; refus par defaut (test sur toutes les routes).
- CA5 Limites : taille de corps, delais de lecture/ecriture (config), pagination bornee.
- CA6 En-tetes de securite par defaut, CORS ferme par defaut (`api.origines_cors`).
- CA7 (dette lot 1) `/etat` reserve aux administrateurs (`Session.Admin`), refus 403 pour un simple utilisateur (test). Routes de catalogue : filtre `FournisseurID` toujours pose. Routes film/serie via `domain.StockageVideo`.
- CA8 (dette lot 1) Dans `internal/live`/`internal/hls` : le jeton de segment est LIE a la chaine du chemin (un jeton emis pour la chaine A est refuse sur la chaine B, test) ; filtre d'hotes : une URL de segment vers une adresse privee, loopback ou lien local est refusee sauf `serveur.hotes_segments_autorises` (test, y compris redirection et resolution DNS vers IP privee) ; les constantes de `live` (`taillePlaylistMax`, `tailleTampon`, `fenetreMesures`, `partMaxSegment`) lisent `config.Live.*`.
- CA9 Les routes API de profil (US-13), de comptes (US-08) et de VOD/EPG (US-09/10) sont decrites dans le contrat et passent le test de conformite.

### US-12 — Assemblage `cmd/mytv` · codeur-c (apres US-08, 09, 10, 11, 13, 14) · A FAIRE
- CA1 Charge la configuration, resout les secrets, echoue au demarrage avec toutes les erreurs.
- CA2 Construit fournisseurs, baux, stockage, synchroniseur, relais, API ; arret propre sur signal (aucune goroutine ne survit, test).
- CA3 Test de bout en bout avec le faux fournisseur : connexion, liste de chaines, lecture live, zapping.
- CA4 Sante `/sante` (non authentifiee, sans detail sensible).
- CA5 Journal structure, niveau de config, aucun secret.
- CA6 Aucune ecoute hors `serveur.ecoute` ; refuse un secret_signature trop court.
- CA7 (dette lot 1) Planification de reprise : documentee dans docs/exploitation.md (la reprise `rafraichissement.reprise` n'est pas persistante : apres un redemarrage, `au_demarrage` + `SiPerime` rattrapent) ; test de redemarrage apres echec.
- CA8 Test de bout en bout du binaire contre le faux fournisseur, sans reseau externe (coordinateur).

### US-13 — Etat par profil (favoris, historique) · codeur-a · A FAIRE
Paquet `internal/profil`.
- CA1 Favoris par profil (ajout/retrait idempotents, ordre stable).
- CA2 Historique de lecture avec position de reprise ; borne (nombre configurable) et purge des plus anciens.
- CA3 Isolation : un profil ne lit ni n'ecrit l'etat d'un autre (test).
- CA4 Resilient a la disparition d'un contenu apres resynchronisation (identifiants orphelins ignores a la lecture).
- CA5 Ecritures atomiques et concurrentes sous `-race`.
- CA6 Routes exposees via US-11 (contrat OpenAPI mis a jour).
- CA8 (reporte d'US-09, BLOQUANT pour US-12) `stockage` implemente `domain.StockageVideo` (Films/Film/Series/Serie, filtre FournisseurID, recherche, pagination) avec tests, en PREMIER commit.
- CA7 Reglages synchronises entre appareils : `EcrireReglages` avec revision (ecriture optimiste, `ErrConflit`), taille bornee (`profil.reglages_max`). Contrat : `domain.EtatProfil`.

### US-14 — Telemetrie · codeur-b · A FAIRE
Paquet `internal/telemetrie`.
- CA1 Metriques (compteurs, histogrammes) du relais, des baux et de la synchronisation exposees au format Prometheus sur une route protegee.
- CA2 Aucune etiquette a cardinalite non bornee (pas d'identifiant de session ni d'URL) ; test.
- CA3 Diagnostic : evenements recents (conservation `diagnostic.conservation`) sans identifiant ni jeton.
- CA4 Cout nul quand `diagnostic.actif` est faux (benchmark).
- CA5 Instrumentation par interface (pas de dependance du relais vers telemetrie). Contrat : `domain.Instrumentation` (+ `SansInstrumentation`).
- CA6 La route de metriques et de diagnostic est reservee aux administrateurs.
