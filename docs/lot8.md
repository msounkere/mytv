# Lot 8 — Diffusion vers un televiseur (Cast) par lien signe, depuis l'instance publique

Constat (recette 2026-10-31, instance VM `mytv.msocasa.com`) : la diffusion (Remote Playback API,
`src/lib/cast.ts`) est desactivee des que la page n'est pas servie depuis le reseau local
(`diffusionPossible`), parce que le televiseur va chercher le flux SANS cookie et que le serveur ne le
reconnaissait qu'a son adresse (compte technique local, `comptes.plages_locales`). Depuis la bascule
tout passe par la VM : la diffusion n'est plus jamais possible. L'utilisateur en a besoin.

Decision : le serveur remet au lecteur un **lien signe a duree limitee**, sans cookie, que l'appareil
de diffusion va chercher par Internet sur la VM. Le compte technique local reste tel quel (LAN,
autres lecteurs) ; le lien signe devient LE chemin de la diffusion, local ou non (un seul chemin).

Regles inchangees : docs/processus.md, CA numerotes couverts par de VRAIS tests (commentaire « CAn »),
verification par mutation (test qui echoue sans le correctif, les deux sorties dans le rapport),
`gofmt -l`, `go vet ./...`, `staticcheck ./...`, `go test -race ./...` ; UI : `tsc`, tests unitaires,
`npm run build` ; jamais de contact avec le vrai fournisseur ni la production, jamais de secret lu.
Worktrees : /opt/mytv-work/c8-a (Go, feat/lot8-a), /opt/mytv-work/ui-4 (UI, feat/ui-4).
Le coordinateur fusionne et deploie.

## US-40 — Serveur : lien de diffusion signe (codeur c8-a, Go) · A FAIRE
- CA1 `POST /api/v1/cast/lien` (niveau Session) avec `{ "lecture": "/api/v1/live/<chaine>.m3u8" | "/api/v1/vod/<id>" , "parametres": "cap=...&qualite=..." (facultatif, recopie tel quel apres validation) }`
  repond `{ "url": "/api/v1/cast/<jeton>/live/<chaine>.m3u8" | "/api/v1/cast/<jeton>/vod/<id>?...", "expire": <RFC 3339> }`.
  Le jeton est scelle par `hls.Sceau` (domaine « cast », libelles distincts) et porte : identifiant de
  SESSION, ressource autorisee (chaine ou identifiant de contenu, rien d'autre), date d'expiration
  (`comptes.cast_duree`, defaut 12h, valide > 0). 400 si `lecture` n'est ni une chaine ni un contenu connu
  du schema ; jamais de secret ni d'URL du fournisseur dans le jeton ou le journal.
- CA2 `GET`/`HEAD /api/v1/cast/<jeton>/...` : le jeton est ouvert et verifie (authenticite, expiration,
  ressource = celle du reste du chemin : la chaine pour `/live/<chaine>.m3u8`, `/live/<chaine>/seg/...` et
  les sous-playlists `/live/<jeton-segment>.m3u8` emises pour cette chaine ; l'identifiant pour `/vod/<id>`) ;
  la session du jeton doit encore exister et etre valide (revocation = 401). La requete est ensuite servie
  par les gestionnaires existants (relais du direct, VOD) **comme si elle venait de cette session** : meme
  bail, meme place unique chez le fournisseur, meme occupation (`cleSession`), aucune session nouvelle.
  Mecanisme : `comptes.AuthentifierLecture` reconnait d'abord une session imposee dans le contexte de la
  requete (posee par la route cast), puis le cookie, puis le compte local. 401 `jeton_invalide` sinon ;
  POST/PUT/DELETE refuses (405).
- CA3 Les adresses emises DANS les playlists servies sous le prefixe signe restent sous ce prefixe :
  segments (`<prefixe>/live/<chaine>/seg/<jeton-segment>.ts`), sous-playlists, segments de conversion
  (`transcodage.go`), cle/init. Un lecteur qui n'envoie aucun cookie (faux televiseur dans le test) lit la
  playlist PUIS les segments avec 200 ; le meme chemin sans jeton repond 401. Le prefixe des segments
  (`prefixeSegments`) est derive de la requete (contexte), pas d'une constante.
- CA4 CORS pour les appareils de diffusion : sur `/api/v1/cast/...` seulement, `Access-Control-Allow-Origin: *`
  (sans credentials), `Access-Control-Expose-Headers` (les X-Mytv-*), et reponse 204 au preflight OPTIONS
  (`Allow-Methods: GET, HEAD`, `Allow-Headers: Range`). Le reste de l'API garde sa politique
  (`api.origines_cors`, ferme par defaut) ; test : une route hors cast ne gagne aucun en-tete CORS.
- CA5 Contrat et exploitation : `docs/openapi.yaml` (section « Cast » reecrite : lien signe, plus de
  compte local obligatoire ; routes `/cast/lien` et `/cast/{jeton}/...`), `config/mytv.exemple.yaml`
  (`comptes.cast_duree`), `docs/exploitation.md` (ce que voit le televiseur, duree, revocation, CORS),
  `internal/api/api_test.go` (table des routes et niveaux : la route cast est de niveau `Lecture` sans
  cookie). Metrique `cast_liens_total` et journal INFO « lien de diffusion emis » (session abregee,
  ressource, expiration ; jamais le jeton).
- CA6 Qualite : `go test -race ./...` vert, mutations rapportees pour CA2 (expiration, ressource
  etrangere, session revoquee) et CA3 (segment sans prefixe), aucune regression des tests du relais.

## US-41 — Interface : diffuser depuis n'importe ou (codeur ui-4, UI) · A FAIRE
- CA1 `diffuser()` (src/screens/Lecture.tsx, src/lib/cast.ts) ne refuse plus hors reseau local :
  il demande `POST /api/v1/cast/lien` pour `lectureEntree(entree)` avec les parametres de diffusion
  (CA2), puis `preparerDiffusion(video, urlSignee)` et le selecteur natif. `diffusionPossible` et
  `origineLocale` disparaissent (plus de notion de reseau local cote interface) ; les tests qui les
  couvraient sont remplaces.
- CA2 Capacites de l'appareil de diffusion, distinctes de celles du navigateur : par defaut
  `cap=hevc,eac3,ac3` (un televiseur decode ces pistes), reglable dans Reglages > Lecteur
  (« Le televiseur decode EAC3/AC3 », « Le televiseur decode HEVC », memorises par appareil comme la
  qualite) ; jamais `qualite=eco` pour la diffusion (le televiseur a le debit, et Eco coute un coeur).
  Test unitaire sur la construction des parametres.
- CA3 Un seul flux ouvert : pendant la diffusion le navigateur n'ouvre PAS le flux localement
  (`preload="none"` sur l'element avant de poser la source signee, aucun `play()` local tant que l'etat
  n'est pas `diffusion`) ; test e2e avec le faux serveur (tests/e2e/serveur-factice.mjs) : apres
  « Diffuser », le faux serveur voit UNE demande de lien et AUCUNE requete de flux depuis le
  navigateur (la Remote Playback API est simulee : `video.remote` factice qui annonce un appareil).
- CA4 Navigateur sans Remote Playback (Firefox) : le bouton reste visible, le panneau explique
  « Diffusion disponible dans Chrome, Edge ou Safari » et propose « Copier le lien pour le televiseur »
  (lien signe, 12 h, a ouvrir dans le navigateur ou le lecteur du televiseur) ; aucun lien copie
  n'apparait dans la telemetrie.
- CA5 Arret de la diffusion : retour a la lecture locale par l'adresse normale (cookie), position
  conservee ; telemetrie `diffusion:debut` / `diffusion:fin` (ressource, duree, navigateur ; jamais le
  jeton). `tsc`, tests unitaires, `npm run build` verts ; ligne de couverture par CA.

Ordre : US-40 d'abord (le contrat), ui-4 code en parallele contre le faux serveur en suivant le contrat
ci-dessus (`/cast/lien` et `/cast/<jeton>/...`), puis recette reelle par le coordinateur avec le
Chromecast de l'utilisateur.
