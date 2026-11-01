# Lot 9 — « À voir » : liste de lecture preparee d'avance

Constat (mesures du 2026-11-01, docs/couverture.md « recette VM ») : le fournisseur sert tres vite
(50 a 140 Mbit/s) les parties d'un fichier deja lues par son noeud, et lentement (1,5 a 2,9 Mbit/s
par connexion) celles qui ne l'ont jamais ete, pour certains contenus (serie Industry : 4,4 Mbit/s
necessaires). Il coupe toute connexion supplementaire au bout de ~3 s : une seule connexion VOD
durable par compte. Aucun reglage du relais ni du lecteur ne peut compenser : la seule reponse est
de **preparer d'avance** sur le disque de la VM ce que l'utilisateur a decide de regarder.

Decision (utilisateur, 2026-11-01) : une liste « À voir » par profil, remplie A LA MAIN (fiches,
recherche), ordonnee ; une serie y est UN element (pas ses episodes) ; la preparation, elle, travaille
episode par episode : les `episodes_avance` (defaut 2) prochains episodes non vus, a partir de la
position atteinte ou d'un point indique ; un episode vu est efface et le suivant prepare. Budget disque
fixe (defaut 10 Go). Rien n'est ajoute automatiquement a la liste. Le hors-ligne sur telephone est hors lot.
Modele : Netflix « Smart Downloads » (telecharger l'episode suivant, effacer le vu), Plex « N prochains
episodes non vus », mais COTE SERVEUR : c'est lui qui tient la connexion unique au fournisseur.

Regles inchangees : docs/processus.md, CA numerotes couverts par de VRAIS tests (commentaire « CAn »),
verification par mutation rapportee, `gofmt -l`, `go vet`, `staticcheck`, `go test -race ./...` ;
UI : `tsc`, tests unitaires, e2e contre le faux serveur, `npm run build` ; jamais de contact avec le vrai
fournisseur ni la production, jamais de secret lu. Contrats `internal/domain` : demande au coordinateur
(`/opt/mytv-work/statut/<codeur>.contrat.demande`) ; pour ce lot le codeur Go PEUT ajouter des types
dans un nouveau fichier `internal/domain/lot9.go` sans rien modifier d'existant.
Worktrees : /opt/mytv-work/c9-a (Go, feat/lot9-a), /opt/mytv-work/ui-5 (UI, feat/ui-5).

## US-42 — Serveur : liste « À voir » et moteur de preparation (codeur c9-a, Go) · A FAIRE
- CA1 Liste par profil, persistee (migration profil : table `profil_a_voir` : profil, id de contenu
  (`vod:<fid>:<numero>` film ou `serie:<fid>:<numero>`), rang, ajoute, `episodes_avance` (defaut
  `preparation.episodes_avance`), `depuis` (saison/episode facultatifs)). Routes (niveau Session, comme
  /profil/favoris) : `GET /profil/a_voir` (ordonnee, avec l'etat de preparation de chaque element et
  des episodes concernes), `PUT /profil/a_voir/{id}` (ajout en fin de liste ou mise a jour des options,
  idempotent), `DELETE /profil/a_voir/{id}`, `PUT /profil/a_voir` (ordre : liste d'ids). Contrat dans
  docs/openapi.yaml (schemas `AVoir`, `AVoirElement`, etats). Un film termine (part >= 0,95 dans
  l'historique) ou une serie dont tous les episodes sont vus sort de la liste a la prochaine lecture
  de l'etat (`GET`), avec journal INFO.
- CA2 Moteur de preparation (`internal/preparation`) : planifie, pour chaque profil et dans l'ordre de
  sa liste, les fichiers a tenir prets : un film = son fichier ; une serie = ses `episodes_avance`
  prochains episodes NON VUS (historique + reprises : vu = part >= 0,95) a partir de `depuis` sinon de
  l'episode entame, saisons dans l'ordre. Telechargement par le fournisseur du contenu (meme client
  que la VOD : `reseau.proxy_sortie`, identifiants jamais journalises), UNE connexion a la fois,
  reprise par `Range` apres coupure (le fournisseur coupe parfois), taille verifiee a la fin
  (Content-Range total), fichier ecrit sous `preparation.dossier` (defaut /var/lib/mytv/prepares)
  avec un nom derive de l'id (jamais le nom du fournisseur), `.partiel` pendant l'ecriture. Etat par
  element : `attente`, `en_cours` (octets, total, debit), `pret`, `echec` (cause assainie), persiste
  pour survivre a un redemarrage (reprise du `.partiel`). Metriques `preparation_octets_total`,
  `preparation_fichiers_total{etat}`, journal INFO (id, taille, duree, debit moyen).
- CA3 Le spectateur a toujours la priorite : la preparation n'ouvre une connexion que si AUCUNE
  lecture (direct ou VOD) n'est en cours chez ce fournisseur (interroger les baux/occupation
  existants) et se met en pause (connexion fermee, `.partiel` conserve) des qu'une lecture demande une
  place ; elle reprend `preparation.reprise_apres` (defaut 30 s) apres la fin de la lecture. Test avec
  le faux fournisseur : jamais plus d'une connexion simultanee, pause effective en moins de 2 s.
- CA4 Budget et rotation : `preparation.budget` (defaut 10 Go, validation > 0) borne la somme des
  fichiers prets et partiels ; quand il manque de la place : effacer d'abord les episodes vus, puis
  les fichiers qui ne sont plus demandes par aucune liste, puis les plus anciens prets au-dela des
  `episodes_avance` ; jamais un fichier en cours de lecture. Un episode vu est efface a la prochaine
  planification et le suivant est prepare (modele Netflix). Test : budget 3 fichiers, liste de 5,
  lecture des deux premiers -> ils sont effaces, les suivants prepares, jamais plus de 3 sur disque.
- CA5 Lecture depuis le disque : quand un fichier pret existe pour l'id demande, `vod` le sert comme
  source locale (`domain.SourceOctets` + `AvecTaille`, Range exact, meme decision de mode : direct,
  remux, audio converti, eco) SANS fournisseur et SANS bail ; en-tete `X-Mytv-Source: prepare` ;
  si le fichier est partiel ou absent : chemin actuel inchange. Test : la lecture d'un film prepare
  ne fait aucune requete au faux fournisseur et n'acquiert aucun bail ; un seek loin dans le fichier
  repond en moins de 100 ms.
- CA6 Sonde de disponibilite `GET /vod/{id}/disponibilite` (Session) : quand aucune lecture n'est en
  cours, lit 256 Kio a trois positions (0 %, 50 %, 90 %) par trois connexions SEQUENTIELLES (jamais
  en parallele : le fournisseur coupe), et repond `{ "etat": "pret" | "immediat" | "lent" | "inconnu",
  "debit_kbps", "mesure" }` : `pret` = fichier prepare ; `immediat` = debit mesure >= 2 x le debit du
  fichier (metadonnees du fournisseur si connues, sinon 8 Mbit/s) ; `lent` sinon ; `inconnu` si une
  lecture est en cours (aucune mesure). Resultat garde 10 min. Jamais de sonde pendant une lecture
  (test : avec une lecture ouverte, aucune requete de sonde ne part).
- CA7 Configuration `preparation: { actif, dossier, budget, episodes_avance, reprise_apres }`
  validee et documentee (config/mytv.exemple.yaml, docs/exploitation.md : ou sont les fichiers,
  comment lire l'etat, `/admin/preparation` qui liste les fichiers et leur etat, comment vider) ;
  docs/openapi.yaml ; docs/couverture.md (tableau « Lot 9 — US-42 ») ; `go test -race ./...` vert ;
  mutations rapportees pour CA3 (deux connexions) et CA4 (budget depasse) et CA5 (bail acquis).

## US-43 — Interface : liste « À voir », preparation visible, enchainement (codeur ui-5, UI) · A FAIRE
- CA1 Bouton « À voir » (ajouter / retirer) sur la fiche d'un film, d'une serie et d'un episode
  (l'episode ajoute sa serie avec `depuis` = cet episode) et dans les resultats de recherche ; table
  Dexie `a_voir` synchronisee avec le serveur par le mecanisme de `src/lib/synchro.ts` (classe de
  route `a_voir`, tolerant hors ligne, idempotent) ; `GET /profil/a_voir` fait foi au demarrage.
- CA2 Ecran « À voir » (entree du menu principal) : liste ordonnee, reordonnable (glisser, et
  boutons monter/descendre au clavier et a la telecommande), retrait, et pour une serie : episodes
  d'avance et « a partir de » ; chaque element montre son etat de preparation (`attente`, `en cours`
  avec pourcentage et debit, `pret`, `echec` avec la cause) rafraichi toutes les 10 s pendant que
  l'ecran est ouvert ; une serie affiche « S02E05 · pret · suivant en preparation », jamais ses
  douze lignes.
- CA3 Accueil : rangee « À voir » (les prets d'abord, puis l'ordre de la liste), badge « Pret » sur
  les cartes et dans les fiches quand le serveur l'indique ; la lecture d'un contenu pret passe par
  le lecteur habituel (aucune adresse speciale : le serveur choisit la source).
- CA4 Enchainement : a la fin d'un episode, si le suivant existe, compte a rebours de 10 s
  « Episode suivant » annulable (Echap, clic), demarrage automatique ; si le suivant est `pret` le
  compte a rebours le dit ; pour un film termine, proposer le prochain element de la liste sans
  demarrage automatique. Les reprises et « Continuer a regarder » restent coherents (episode suivant
  = nouvelle reprise, precedent marque vu). Test e2e contre le faux serveur.
- CA5 Sonde de disponibilite sur la fiche d'un film et d'un episode, demandee a l'ouverture de la
  fiche seulement quand aucune lecture n'est en cours : « Disponible immediatement », « Lent
  (2,7 Mbit/s pour 4,4 necessaires) : preparer d'avance ? » avec le bouton « À voir », ou « Pret ».
  Telemetrie `a_voir:ajout` / `a_voir:retrait` / `enchainement:suivant` (ids seulement) ;
  `tsc`, tests unitaires, e2e, `npm run build` verts ; docs/couverture-ui5.md (une ligne par CA).

Ordre : US-42 CA1 (contrat) et CA2-CA5 d'abord ; ui-5 code en parallele contre le faux serveur
(tests/e2e/serveur-fictif.mjs et serveur-factice.mjs etendus : routes /profil/a_voir, etats de
preparation simules, /vod/{id}/disponibilite). Le coordinateur fusionne, deploie par GitHub et fait la
recette avec l'utilisateur.
