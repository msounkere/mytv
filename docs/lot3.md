# Lot 3 — Adaptation de l'interface a l'API v1 et recette

Deux depots : le serveur Go `/opt/mytv` (branche main) et l'interface SolidJS `/opt/mytv-ui` (branche main, git local).
Contrat : `docs/openapi.yaml` (serveur). Environnement de DEV : pas de retour arriere a preserver, l'ancien
systeme (TuliProx, /opt/mytv-relais, /opt/mytv-flux) sera remplace ; ne JAMAIS les modifier pour autant (ils servent la lecture reelle).
Regle absolue inchangee : aucun test automatique ne contacte le vrai fournisseur ; la recette reelle est faite
a la main par le chef de projet, apres le lot.

## Principe d'adaptation de l'interface
L'interface garde son modele (catalogue importe dans IndexedDB/Dexie, recherche et facettes cote client, EPG en
grille) : seule la COUCHE DE DONNEES change. `xtream.ts`, `auth.ts`, `synchro.ts`, `flux.ts`, `epg.ts`,
`diagnostic.ts`, `import.worker.ts` parlent a `/api/v1`. Les ecrans et composants changent le moins possible.
Identifiants de contenu : desormais `fournisseur:numero` (ex. `iptva:845410`), plus d'identifiants virtuels TuliProx.
Auth : cookie de session (jamais d'identifiants dans l'URL, jamais dans le bundle). Les lecteurs sans cookie
(Cast) passent par le compte technique local ; l'URL de lecture ne doit contenir aucun secret durable.

## Serveur (codeur-g, Go)
### US-15 — Harnais de dev `cmd/mytv-dev` · A FAIRE (A LIVRER EN PREMIER)
- CA1 Demarre le faux fournisseur (catalogue realiste : >= 2000 chaines, 5000 films, 500 series, EPG sur 48 h) ET `mytv` sur des ports configurables, avec un utilisateur de test cree, et affiche les URL.
- CA2 Demarrage < 5 s, arret propre (SIGINT/SIGTERM), base SQLite temporaire, jamais d'hote externe (verifie par un test qui echoue si une connexion sort de 127.0.0.1).
- CA3 Sert l'interface depuis un dossier `dist` donne en option (meme origine que l'API : pas de CORS en dev).
- CA4 Option pannes : `--panne 407:30s`, `--latence 800ms` pour reproduire les cas reels dans les tests de l'interface.
### US-16 — Besoins de l'interface dans l'API · A FAIRE (contrat dans la branche `contrat-lot3`, a fusionner dans la branche de codeur-g)
Le coordinateur arbitre les demandes des codeurs UI (fichiers `ui-N.contrat.demande`). Contrat affine apres lecture de ce que l'interface consomme (`xtream.ts`, `import.worker.ts`, `db.ts`, `facettes.ts`, `epg.ts`, `xmltv.worker.ts`) :
- CA1 `GET /catalogue/version` (manifeste : version = ETag, effectifs, date de synchro ; 304 sur If-None-Match) et `GET /catalogue/export?type=categories|live|vod|series` : NDJSON en flux, gzip, ETag commun, `X-Elements`, 304 ; les lignes ont les schemas `Chaine`/`Film`/`Serie`/`Categorie` ; lecture en une transaction (pas de melange de deux synchros) ; memoire constante. Test de volume : 25 000 chaines + 40 000 films exportes en < 3 s (mesure consignee, hors -race), test que le flux n'est pas materialise (memoire), test 304, test gzip, test de coherence ETag version/export.
- CA2 EPG en bloc : `/epg/grille` existe deja (`chaines`, `de`, `a`). Verifier/ajouter : jusqu'a `api.pagination_max` chaines, fenetre `api.grille_fenetre_max`, chaines inconnues ou sans guide = tableau vide (pas d'erreur), reponse compacte ; test de charge (200 chaines x 6 h).
- CA3 Champs : `ajoute` (nullable) ajoute a `Chaine` et `Serie` (domaine + stockage + synchro Xtream : `added` des chaines, `last_modified` des series) ; `nom` toujours BRUT (jamais de prefixe retire) ; `annee`, `note`, `extension` fournis quand le fournisseur les donne. Les facettes (pays, langue, genre, service, annee) restent derivees cote interface (`facettes.ts`) : decision documentee dans l'OpenAPI.
- CA5 (demande ui-2) `POST /live/{chaine}/fin` (204, rend le bail seulement si la session lit encore `chaine`, idempotent, jamais 503, session seulement) ; `limites` {pagination_max, grille_fenetre (s), favoris_max, historique_max, reglages_octets_max} dans `GET /compte`. Tests : bail rendu immediatement, appel tardif apres zapping sans effet, origine etrangere refusee.
- CA6 (demande ui-2) `GET`/`HEAD /vod/{id}` : en-tetes `X-Mytv-Mode` (direct|remux|audio) et `X-Mytv-Duree` (secondes) ; HEAD = meme statut que GET sans lancer ffmpeg.
- CA4 Cast : pas d'URL signee. Documente dans l'OpenAPI (compte technique local, GET/HEAD) ; le harnais de dev inclut 127.0.0.1 dans `plages_locales` pour tester le chemin sans cookie.
### US-17 — Risques de securite du lot 2 · A FAIRE
- CA1 Le remux ne met plus l'URL amont (identifiants) dans les arguments de ffmpeg : le serveur lit l'amont et le donne a ffmpeg par `pipe:0` (Range gere cote Go) ; test qui verifie qu'aucun argument du processus ne contient l'hote ni les identifiants.
- CA2 Filtre d'hotes : chaque redirection est reverifiee ET l'adresse IP resolue est controlee a la connexion (contre le DNS rebinding) ; tests avec un resolveur factice.
- CA3 Un film en pause rend sa place apres un delai configurable sans activite du client (`vod.pause_max`).
- CA4 Plage hors fichier -> 416 (et non 502).
### US-18 — Import des donnees de l'ancien service (optionnel, en dernier) · A FAIRE
- CA1 Outil `mytv-import` qui lit un export JSON (favoris, historique, reglages de l'ancienne interface) et traduit les anciens identifiants TuliProx en `fournisseur:numero` via un fichier de correspondance fourni ; rapport des elements non traduits ; aucun appel au vrai fournisseur.

## Interface (codeur-u1 et codeur-u2, TypeScript/SolidJS)
Repo `/opt/mytv-ui`, worktrees `/opt/mytv-work/ui-1` et `ui-2` (node_modules deja relie ; `NODE_OPTIONS=--tls-max-v1.2` pour npm/node). `npm run build` (tsc + vite) vert avant toute remise ; tests Playwright (jamais sur le vrai fournisseur, fermer les navigateurs).
### US-19 — Couche de donnees (codeur-u1)
- CA1 Connexion/deconnexion/session par cookie (`/connexion`, `/compte`), ecran Connexion adapte ; plus aucun identifiant de fournisseur nulle part dans le bundle (test qui greppe `dist`).
- CA2 Import du catalogue depuis l'API (en bloc, ETag, reprise sur erreur) vers Dexie ; barre de progression conservee ; import incremental quand l'ETag ne change pas = aucune requete de donnees.
- CA3 Recherche, facettes et listes inchangees dans leur comportement (tests existants verts ou adaptes) avec les nouveaux identifiants.
- CA4 Etat par profil : favoris, historique, reglages via `/profil/*` avec gestion du conflit (`ErrConflit` : fusion puis nouvel essai), hors-ligne tolere.
- CA5 Profils (`/compte/profils`) : choix et changement du profil actif.
### US-20 — Lecture, PiP, Cast, diagnostic (codeur-u2)
- CA1 Direct : lecture via `/api/v1/live/...` (hls.js), gestion des reponses 503 + `Retry-After` (patience : message « place occupee, nouvelle tentative » et reessai automatique, sans reboucler a l'infini) et des 401 (retour a la connexion).
- CA2 VOD/series : `/vod/{id}` en direct avec Range, bascule remux (`?t=` pour le seek), erreur 415 (HEVC) presentee clairement.
- CA3 Fin de lecture/zapping : l'interface previent le serveur (le bail est rendu) ; PiP et Cast conservent leur comportement (Cast sans cookie : URL signee a duree courte fournie par le serveur).
- CA4 Telemetrie vers `POST /diagnostic/evenements` (format du contrat), y compris le detecteur d'images rejouees deja present.
- CA5 Plus aucune reference a `/flux/`, `player_api.php`, `xmltv.php` ni `/live|movie|series/<user>/<pass>` dans `src/` (test qui greppe).
### US-21 — EPG (codeur-u2)
- CA1 Grille et mini-guide alimentes par `/epg/grille` et `/epg/chaines/{id}/maintenant` (le worker XMLTV disparait).
- CA2 Fenetre glissante, fuseau local correct, « en cours / suivant », sans appel par chaine visible.
### US-22 — Recette automatisee de bout en bout (coordinateur + u1/u2)
- CA1 Playwright contre `cmd/mytv-dev` : connexion, import du catalogue, recherche, ouverture d'une fiche, favori, guide, lancement du direct (assertion sur les requetes et l'absence d'erreur hls ; Chromium headless ne lit pas le H.264), zapping rapide x10 sans 407 residuel, deconnexion.
- CA2 Scenarios de panne : 407 pendant 30 s (patience), 502 ponctuel, session expiree.
- CA3 Test de charge d'import (catalogue complet) avec mesure du temps ; budget consigne.
- CA4 Aucun test ne sort de 127.0.0.1.
