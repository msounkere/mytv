# Lot 10 — Corrections de l'interface et refonte « Ma liste / Reprendre / Vu »

Origine : audit complet de l'interface du 2026-11-04 (quatre relecteurs, rapports et captures dans
/opt/mytv-work/audit/{A,B,C,D}.md et /opt/mytv-work/audit/{A,B,C,D}/ : scripts Playwright reutilisables
comme base de tests). L'utilisateur juge la preparation d'avance (lot 9) reussie mais « la gestion des
vus / pret / ajout a la liste incomprehensible » et l'interface « pleine de bugs ». Decision : corriger TOUS
les bugs bloquants et majeurs, et adopter le modele cible du rapport C (§5), fonde sur Netflix, Plex,
Apple TV, Disney+ et Prime Video. Les numeros « A3 », « B1 », « C7 », « D16 » renvoient aux bugs des rapports.

Regles inchangees : docs/processus.md, CA numerotes couverts par de VRAIS tests (commentaire « CAn »),
verification par mutation rapportee, `gofmt -l`, `go vet`, `staticcheck`, `go test -race ./...` ;
UI : `tsc`, tests unitaires, e2e contre le faux serveur, `npm run build` ; jamais de contact avec le vrai
fournisseur ni la production, jamais de secret lu. Les codeurs UI lisent les rapports d'audit et REUTILISENT
leurs scripts (copie dans tests/e2e, adaptes en verifications) : un bug corrige = un test qui echouait avant.
Chaque codeur reste dans SES fichiers (tableau de propriete en fin de document) ; un besoin dans le fichier
d'un autre = fichier `/opt/mytv-work/statut/<codeur>.contrat.demande`, le coordinateur arbitre.
Worktrees : /opt/mytv-work/c10-a (Go, feat/lot10-a), /opt/mytv-work/ui-6 (feat/ui-6), ui-7 (feat/ui-7),
ui-8 (feat/ui-8). Les trois codeurs UI partent du meme main (feb306e) ; le coordinateur fusionne dans
l'ordre ui-7, ui-6, ui-8 et resout les conflits.

## Modele cible (decide ; vocabulaire commun a tous)

Trois choses, trois noms, partout (rail, rangees, ecrans, menus, toasts, aria) :
- **Ma liste** : liste ordonnee, manuelle, par profil (ex-« À voir »). Chaque element porte un ETAT DE
  PREPARATION : `○ En attente` + raison · `⟳ En preparation · 42 % · ~6 min` · `● Pret` · `✕ Echec · cause`
  + `Reessayer`. Une serie = un element (« dès S01E03 », « 2 episodes d'avance »). Entree : `+ Ma liste`
  partout ou une affiche apparait (fiche, carte, recherche, grilles, lecteur) ; un episode : menu de ligne
  « Ma liste a partir d'ici ». Sortie : « Retirer de Ma liste » ou automatiquement (film vu, serie finie),
  TOUJOURS avec un toast et « Annuler ». Les FAVORIS ne concernent plus que les chaines (etoile retiree des
  fiches et cartes de films/series, rangee « Films et series favoris » supprimee ; migration serveur).
- **Reprendre** (rangee de l'accueil, ex-« Continuer a regarder ») : automatique. Carte film = position ;
  carte serie = L'EPISODE A LIRE (entame, sinon prochain non vu) et le tap LANCE cet episode. Une serie y
  reste tant qu'il lui reste un episode. Menu « ⋯ » : Reprendre · Marquer comme vu (une serie passe a
  l'episode suivant) · Retirer de Reprendre (positions conservees). Toast + Annuler a chaque geste.
- **Vu ✓** : un etat, pas une liste. Seuil UNIQUE : part >= 0,9 (serveur et interface). Visible sur la
  fiche film (« ✓ Vu le 3 nov. », bouton principal « Regarder a nouveau »), chaque ligne d'episode, le coin
  des cartes ; « Marquer comme vu / non vu » dans le menu « ⋯ » de tout film, episode et saison.
Regles transverses : un tap/clic sur une affiche OUVRE TOUJOURS LA FICHE, la lecture est un bouton ▶
explicite ; chaque carte a un bouton « ⋯ » VISIBLE (pas seulement au survol), 44 px au tactile, focalisable
au clavier (Entree / OK) ; tout geste de liste ou de progression donne un toast (4 s, « Annuler » 6 s) ;
retour = ecran precedent (pile d'historique), une seule regle ; la sonde ne parle plus en Mbit/s en premiere
ligne (« Lecture fluide » / « Risque de coupures — preparer d'avance ? » / « ● Pret · prepare sur le
serveur », debit en detail) et part UNE fois par fiche. Libelles exacts : rapport C §5.5 (fait foi).

## US-44 — Serveur : preparation reparable et explicite, seuil vu, migration des favoris (c10-a, Go)

- CA1 Raison d'une attente : `GET /profil/a_voir` ajoute, pour tout element et tout episode en `attente`,
  `raison` ∈ {`lecture_en_cours`, `file` (+ `position` 1-based dans la file), `budget_plein`,
  `reprise_apres` (la lecture vient de finir, reprise dans `preparation.reprise_apres`)}. Test : une lecture
  ouverte sur le faux fournisseur → `lecture_en_cours` ; trois elements → le 2e et le 3e `file` 2 et 3.
- CA2 Echec transitoire et nouvelle tentative automatique : un echec dont la cause est reseau (coupure,
  delai, 5xx, 458 adresse refusee, 503) garde l'etat `echec` mais porte `reessai` (RFC 3339) et le moteur
  retente a cette echeance avec recul 15 min, 30 min, 1 h, 2 h, 4 h (max), sans fin tant que l'element est
  dans la liste ; un echec permanent (404/410, fichier incoherent, contenu disparu du catalogue) n'a pas de
  `reessai`. Journal INFO a chaque nouvelle tentative. Test : faux fournisseur qui repond 458 deux fois puis
  200 → fichier pret apres deux reculs (horloge simulee), jamais plus d'une connexion.
- CA3 `POST /profil/a_voir/{id}/reessayer` (Session) → 204 : remet tous les fichiers en echec de l'element en
  `attente` immediatement (serie : ses episodes planifies), efface `reessai` ; idempotent (204 si rien a
  faire ; 404 si l'element n'est pas dans la liste). docs/openapi.yaml.
- CA4 Budget visible : `GET /profil/a_voir` ajoute `budget: { utilise, total }` (octets ; utilise = prets +
  partiels de TOUS les profils, comme CA4 du lot 9). Test : deux fichiers prepares → `utilise` = somme.
- CA5 Seuil « vu » unique : constante `domain.PartVue = 0.9` utilisee par la sortie de liste (a_voir.go),
  la planification (plan.go), l'historique et toute autre comparaison (grep 0.95 → zero occurrence hors
  tests de migration). Test : position a 0,92 → episode considere vu, suivant planifie.
- CA6 Migration des favoris : au demarrage, migration de schema du profil : chaque ligne de `profil_favoris`
  dont `contenu_id` est un film ou une serie (prefixes du catalogue : `vod:` / `serie:` tels que la liste les
  attend ; une chaine reste favorite) est ajoutee EN FIN de `profil_a_voir` du meme profil (ordre `ajoute`),
  sauf si deja presente, puis supprimee de `profil_favoris`. Idempotente, journal INFO (profil, nombre
  migres). `PUT /profil/favoris/{id}` refuse desormais un film ou une serie (422 `favori_chaines_seulement`).
  Test : table semee avec 2 films, 1 serie, 1 chaine → liste de 3, favoris = 1 chaine, rejouer = rien.
- CA7 Documentation et couverture : docs/openapi.yaml (AVoirElement.raison, reessai, budget, route
  reessayer, 422 favoris), docs/exploitation.md (nouvelles tentatives, comment lire `reessai`),
  docs/couverture.md (« Lot 10 — US-44 »), `go test -race ./...` vert, mutations rapportees pour CA2 (pas de
  recul → test rouge) et CA6 (migration rejouee → doublons).

Contrat suppose par l'interface (les faux serveurs de l'UI l'implementent tel quel) :
```
GET /profil/a_voir → { elements:[ { …lot 9…, raison?: "lecture_en_cours"|"file"|"budget_plein"|"reprise_apres",
                                    position?: 2, reessai?: "2026-11-04T21:10:00Z",
                                    episodes?:[ { …lot 9…, raison?, position?, reessai? } ] } ],
                       budget: { utilise: 5028363308, total: 10737418240 } }
POST /profil/a_voir/{id}/reessayer → 204 | 404
PUT /profil/favoris/{id} (film ou serie) → 422 { "erreur": "favori_chaines_seulement" }
```

## US-45 — Interface : lecteur et fiches (ui-6)

- CA1 (B1, D1, bloquant) « Arreter le flux », « Fermer la lecture », « Retour » du panneau d'erreur et la
  sortie par Echap DETACHENT la video (src vide, `load()`, place rendue), ENREGISTRENT la position de sortie
  dans `reprises` (puis synchro immediate : `sendBeacon`/`keepalive` a la sortie, a la pause et au
  `pagehide`), demontent la couche du lecteur, sans aucune exception « Stale read » : le nettoyage ne lit
  plus `props.entree` apres la deselection (valeurs capturees dans l'effet). Test e2e (3 moteurs : chromium,
  firefox, webkit) : apres l'arret, aucune `<video>` avec source dans le document, `pageerror` = 0, position
  presente dans `reprises`, couche absente des ecrans suivants (Ma liste, Reglages, Direct).
- CA2 (A8, B4, B UX4-5, A mineur « fleche Retour mobile ») Une seule regle de retour : « Retour », Echap,
  « Fermer la lecture » et la fin de lecture reviennent a l'ECRAN PRECEDENT (pile d'historique ; si la pile
  est vide : la fiche du contenu, sinon l'accueil) ; la requete de recherche est conservee ; le mini-lecteur
  ne navigue jamais. Fiche serie : Retour/Echap = `history.back()` comme la fiche film. Test e2e : accueil →
  fiche → lecture → Retour = fiche ; recherche → lecture → Retour = resultats avec la requete ; guide →
  lecture → Fermer = guide.
- CA3 (B UX1, B UX2, C UX7, C UX12, C 5.3) Action principale des fiches : film = `Regarder` / `Reprendre ·
  42 min` (+ secondaire `Recommencer`) / `Regarder a nouveau` quand vu ; serie = `Lire S01E01` /
  `Reprendre S01E03 · 23 min` / `Episode suivant S01E04`, barre de progression sous le titre, chip d'etat de
  preparation (composant commun fourni par ui-7 : `components/ChipPreparation.tsx`, contrat ci-dessous) ;
  la question « Reprendre / Recommencer » se pose SUR LA FICHE, plus jamais dans le lecteur (B UX3) ; sous
  60 s la fiche dit « Regarder » et la carte ne montre pas de barre (coherence).
- CA4 (C5, C18, C11, C10, C17, B UX11) Fiche serie : menu « ⋯ » sur CHAQUE ligne d'episode, tous gabarits :
  `▶ Lire` · `✓ Marquer comme vu` / `Marquer comme non vu` · `Ma liste a partir d'ici` ; l'episode retenu
  comme point de depart affiche « Point de depart » et son menu propose « Ne plus partir d'ici » (jamais un
  retrait de la serie) ; menu « ⋯ » de la saison : « Marquer la saison comme vue / non vue » ; un seul bouton
  `+ Ma liste` / `✓ Dans ma liste · dès S01E03 ▾` dans l'en-tete (le ▾ change le point de depart) ; la sonde
  part UNE fois par fiche (pas par onglet de saison), est invalidee par l'etat « Pret », et s'affiche avec
  les trois phrases du modele cible ; ligne d'episode : `✓ Vu` / barre + « 23 min » / chip Pret, tenant dans
  390 px (C15). « Marquer vu » = reprise a la duree ; « non vu » = reprise effacee (synchro existante).
- CA5 (C6, B10, B11, B12, C13, C19) Fin de lecture : decompte « Episode suivant dans 10 s » suspendu
  pendant la pause ; apres annulation, a la vraie fin, calque persistant sans decompte « Episode suivant ·
  S01E03 [▶ Lire] [Revenir a la serie] » ; fin du DERNIER episode : « Serie terminee ✓ · Prochain dans Ma
  liste : … » (jamais la serie que l'on vient de finir) ; fin d'un film hors liste : « Prochain dans Ma
  liste » sans le mot « suivant » ; aucune notification « arrete — un seul flux » lors d'un enchainement
  voulu ; calque de fin et dialogue de diffusion bornes a la zone video sur telephone (jamais coupes) ;
  le decompte nomme l'episode (titre) avec sa vignette.
- CA6 (D2, D3, B5, D19, B UX9, D8) Lecteur telephone et tablette : barre de commandes qui tient dans la
  largeur (essentielles : lecture, −10/+10, volume/muet, plein ecran, « ⋯ » qui regroupe Diffuser, Pistes,
  Qualite, Infos, Image dans l'image, Arreter) ; carte d'erreur AU-DESSUS des commandes avec « Reessayer »
  et « Retour » atteignables (test 390×844 et 844×390) ; tap sur la video = afficher/masquer les commandes +
  gros bouton central lecture/pause ; double-tap lateral = ±10 s ; en lecture sur telephone, une seule
  fleche retour (celle du lecteur) ; en paysage la video occupe la hauteur disponible.
- CA7 (D4, B8) Plein ecran iPhone : sur WebKit sans `requestFullscreen`, utiliser `video.webkitEnterFullscreen()`
  ; bouton toujours operant ; touche F = plein ecran (favori retire du lecteur : B UX8), M muet, I infos ;
  Espace sur un bouton focalise active le bouton (B14) ; test unitaire sur la fonction de choix de l'API
  (webkit / standard / absent).
- CA8 (B3, B7, A2, B13, D7) Changer la qualite (Auto/Eco) reprend au point atteint (suivi de `timeupdate`) ;
  le panneau « Lecteur » de l'en-tete n'a plus de qualite fantome : UN seul reglage « Qualite » (Auto / Eco)
  lu par le lecteur, memorise ; echec de l'image dans l'image = message discret (toast) jamais une erreur de
  lecture ni une reouverture du flux ; Echap ferme d'abord tout calque ouvert (Variantes, Reprise, Pistes,
  Infos, Diffuser, Mini-guide) avant de quitter.
- CA9 (D16, D23, B UX10, B UX17, B UX19, A UX17/UX20) WebKit : reproduire avec Playwright webkit le plantage
  au zapping apres un echec HLS (script /opt/mytv-work/audit/D/crash-webkit.mjs) ; si reproduit, corriger
  (recreer l'element video a chaque changement de chaine sur WebKit, ou detacher proprement avant de
  reaffecter) et garder le test ; Firefox : ne declarer `cap=hevc` que si `mediaCapabilities.decodingInfo`
  repond `smooth && powerEfficient` (sinon jamais) ; colonne « Pistes » affichee seulement quand il y a un
  choix ; identifiants internes (`iptva:s5002`, adresses d'API, « HTTP 404 — manifestLoadError ») retires des
  textes visibles (reserves au panneau technique) ; panneau Infos d'un episode : titre, numero, resume.
- CA10 Tests : e2e (chromium + firefox + webkit pour CA1, CA6, CA7), unitaires (regles de fin de lecture,
  choix plein ecran, retour), `tsc`, `npm run build` ; docs/couverture-ui6.md (une ligne par CA, mutations :
  CA1 sans capture des valeurs → test rouge ; CA5 decompte en pause → test rouge).

## US-46 — Interface : Ma liste, Reprendre, Vu, cartes et menus (ui-7)

- CA1 (A1, B2, C1, C2, A1bis, D11) `components/MenuContextuel.tsx` reecrit : ouverture au `click`, fermeture
  sur clic EXTERIEUR (document, phase bouillonnante, apres le clic), Echap, perte de focus ; focus deplace
  sur la premiere entree, fleches ↑↓, Entree/Espace activent, Echap rend le focus a l'ancre ; largeur
  adaptee au contenu (min 220, max 90 vw) ; entrees `role=menuitem`. Test e2e : souris, tactile, clavier →
  « Marquer comme vu » agit (reprises modifiees) ; Entree sur « ⋯ » n'ouvre PAS la lecture.
- CA2 (A UX8, C3, C4, C14, B UX2, C 5.2) `CarteAffiche` : tap/clic = FICHE partout (accueil, grilles, recherche,
  Ma liste) ; bouton « ⋯ » toujours visible (coin, 44 px tactile, 36 px bureau, `tabindex=0`) : `▶ Lecture` ·
  `+ Ma liste` / `Retirer de Ma liste` · `Details` ; au survol bureau, `▶` et `+` en plus ; badge de coin
  `Pret` / `⟳ 42 %` / `En attente` / `✕ Echec` et `✓` vu ; la legende (annee · genre) reste. `etat.ts` :
  `ouvrir()` d'un film/serie = fiche ; la lecture directe est `lire()`. Test : tap sur une carte en 390 px →
  URL de fiche, aucune requete de flux.
- CA3 (B7 lot 9, C7, C16, B15, B UX2, B16, C14) `CarteReprise` et `reprises-accueil.ts` : rangee renommee
  « Reprendre » ; carte serie = episode a lire (entame sinon prochain non vu apres un vu, saisons dans
  l'ordre, via `episodes-suivants.ts`), legende `S01E03 · reste 23 min` / `S01E03 · episode suivant`, tap =
  lecture directe de cet episode ; une serie reste tant qu'il lui reste un episode ; « Marquer comme vu » sur
  une serie marque l'episode courant et la carte passe au suivant ; « Retirer de Reprendre » cache sans
  effacer les positions ; seuil vu 0,9 partout (`preferences.ts`, `reprises-accueil.ts`, FicheSerie via
  constante commune `lib/vu.ts` exportee pour ui-6) ; « Reste 1 min » au lieu de « 0 min ».
- CA4 (C UX1, UX3, UX8, A UX6, B UX12, B UX14) Toasts : composant `components/Toast.tsx` (file, 4 s, bouton
  « Annuler » 6 s, aria-live, un seul a la fois, accessible clavier) et `lib/toasts.ts` ; emis pour : ajout /
  retrait Ma liste, marquer vu / non vu, retirer de Reprendre, sortie automatique d'un titre vu (« ✓ Vu ·
  retire de Ma liste · Annuler » : detection par difference entre deux `GET`), echec de preparation
  (« Echec de preparation : Titre · Reessayer »). « Annuler » refait le geste inverse (PUT / DELETE / reprise).
- CA5 (C 5.1, C 5.5, C8, C9, C UX15-UX22, C UX23-24, C UX26) Ecran et rangee « Ma liste » : en-tete
  `5 titres · 2 prets · 1 en preparation · 4,2 Go sur 10 Go` (+ « Mis a jour il y a 8 s »), ligne = poignee
  `⋮⋮`, vignette, titre (→ fiche), chip d'etat, `▶ Regarder` / `▶ Lire S01E02`, `⋯` (Monter · Descendre ·
  Retirer de Ma liste · Reessayer · (serie) `Preparer N episodes d'avance` −/+ 1..5 avec « 2 (reglage du
  serveur) » · `A partir de…`) ; telephone : une ligne par titre (vignette, titre, chip, `⋯`), reordonnable
  par le menu ; Entree = fiche, Suppr et Alt+↑↓ annonces dans `⋯` ; etat vide et bandeau hors ligne du modele
  cible ; rangee de l'accueil APRES « Reprendre », dans l'ordre de la liste, compteur `5 · 2 prets` ; badge de
  coin, jamais une phrase a la place de la legende ; chip commun `components/ChipPreparation.tsx`
  (props : etat, octets, total, debit_kbps, raison, position, cause, reessai, serie?{saison, episode, suivant}
  ; rend `○ En attente · une lecture est en cours` / `⟳ En preparation · 42 % · ~6 min` / `● Pret` /
  `✕ Echec · disque plein` + `Reessayer` ; export aussi `phraseEtat()` pour les legendes) utilise par ui-6
  et ui-8. Rail / barre basse : « Ma liste ».
- CA6 (C UX9, US-44 CA6, A rangee favoris) Favoris reserves aux chaines : etoile retiree des fiches et cartes
  de films/series et du lecteur VOD, rangee « Films et series favoris » supprimee, bascule « Favoris » des
  grilles Films/Series supprimee ; la synchro ignore les favoris non-chaines ; `BoutonAVoir` renomme
  `BoutonMaListe` (libelles §5.5) et present sur les grilles Films/Series et « À la une » (C UX29) et dans
  le lecteur (C UX28, via le menu `⋯` que ui-6 expose : contrat `surMaListe`).
- CA7 (C10, C17 cote client, C UX4, C UX27) `lib/disponibilite.ts` : cache invalide par l'etat « Pret » ;
  phrases du modele cible ; `a-voir-regles.ts` renomme `ma-liste-regles.ts` : libelles coherents
  (« Mbit/s », un seul mot « en preparation »), `prochainDansListe` ne propose jamais le contenu qui vient de
  finir ni « suivant » pour un film hors liste ; temps restant estime `(total - octets) / debit`.
- CA8 (A10, A9, D18) `requetes.ts` / `synchro.ts` : la synchro de l'historique invalide `recommandation` ;
  la fin d'un import invalide `aLaUne` et les rangees du catalogue (plus jamais « Rien a mettre a la une »
  apres un import reussi) ; « Tout voir » des chaines favorites ouvre le Direct FILTRE sur les favorites.
- CA9 Tests : e2e (menu, cartes, Reprendre serie, toasts avec Annuler, ecran Ma liste bureau et telephone,
  rangee), unitaires (regles Reprendre, phrases du chip, prochainDansListe), faux serveurs etendus au
  contrat US-44 (raison, position, reessai, budget, reessayer, 422 favoris), `tsc`, `npm run build` ;
  docs/couverture-ui7.md (mutations : CA1 fermeture au pointerdown → test rouge ; CA3 seuil 0,95 → rouge).

## US-47 — Interface : coque, navigation, Direct, Guide, Recherche, Reglages, responsive, hors ligne (ui-8)

- CA1 (A3, A mineur recherche) Entree dans le champ de recherche ouvre la PAGE DE RESULTATS (`/recherche?q=`),
  jamais une lecture ; aucune ligne preselectionnee ; ↓ selectionne, Entree sur une ligne = fiche ; cliquer
  un champ deja focalise rouvre les suggestions ; « 60 resultats · 0 ms » et « Entree · 1 ms » sans les ms.
- CA2 (A11, D5, A UX13-UX15, A UX16) Direct telephone et tablette : barre d'outils ou le titre et le compteur
  restent lisibles (boutons repliables dans un menu `⋯` sous 700 px, champ de filtre dans la barre), lignes
  ou le NOM prime sur le badge de variantes (badge replie en `HD` seul puis masque), cartes sans « Chain… » ;
  chaines triees par `numero` du fournisseur (sinon nom naturel `numeric: true`), colonne N° = numero du
  bouquet ; chaine en cours surlignee dans la liste ; zapping et mini-guide regroupes par variantes comme la
  liste (une entree par chaine, variante choisie annoncee dans le bandeau) ; un seul champ par panneau.
- CA3 (D6 cote coque, D10, D12, D15, D20, D21, D22) Responsive : grilles et en-tetes en `minmax(0,1fr)`
  (fiche serie : ui-6 applique, ui-8 verifie la coque), barre d'onglets a 320 px et zoom 125 % avec
  gouttieres (5 onglets : Accueil, Direct, Ma liste, Recherche, Profil ; Films/Series/Guide dans « Plus » ou
  le profil), guide telephone (jour visible, statistiques sur deux lignes), `safe-area-inset-top/left/right`
  appliquees (en-tete fixe, rail, fleche retour, colonne de zapping), contenu qui reserve la hauteur du
  mini-lecteur (derniere ligne atteignable), cibles tactiles >= 44 px sur telephone ET tablette (en-tete,
  fleches, puces, selects, boutons Reglages, lecteur hors ui-6), rail en paysage telephone (844×390) lisible
  (icones seules ou rail replie).
- CA4 (A12, B UX20, A UX1, B9, D14, A UX2) Session et hors ligne : session expiree → ecran de connexion avec
  « Votre session a expire, reconnectez-vous » et retour a l'ecran d'origine apres connexion ; focus initial
  sur le premier champ utile (connexion, choix du profil) et sur le mot de passe apres refus ; hors ligne :
  bandeau discret persistant « Hors ligne » sur TOUS les ecrans, gestes de liste/favori annonces « sera
  envoye au retour du reseau », ouverture de l'application hors ligne sur le catalogue local (plus de
  « Service injoignable » bloquant quand la base locale existe), echec de chargement d'un ecran a la demande =
  message « Ecran indisponible hors ligne · Reessayer » (jamais l'ecran blanc du portail) ; IndexedDB
  bloquee = phrase humaine (« Ce navigateur bloque le stockage local… ») sans exception a l'ecran.
- CA5 (A2 cote en-tete, A UX3, A UX4, A14, A UX21, D9, A UX11-12) Coque et Reglages : panneau « Lecteur » de
  l'en-tete sans qualite fantome (ui-6 retire l'ecriture ; ui-8 supprime l'option du panneau et ne garde
  que ce qui agit) ; compteur « 0/1 » remplace par un etat lisible (« Lecture en cours · Titre » / rien) ;
  Reglages atteignable en un tap sur telephone (dans l'onglet Profil, en premier) ; noms de profils uniques
  (message) ; suppression d'un profil par un dialogue de l'application (pas `confirm()`) ; `:focus-visible`
  sur le champ de recherche ; masquer une categorie = toast « Masquee · Annuler » et liste des masquees dans
  Reglages ; « Guide TV » devient un lien distinct du selecteur de mode.
- CA6 (D13, A UX17, A UX18, D17) Messages : erreurs du guide et de l'accueil avec leurs propres textes
  (« Guide indisponible pour le moment · Reessayer », jamais « contenu n'existe plus au catalogue ») ; etat
  vide du guide qui nomme la cause (filtre vs catalogue vs aucune chaine avec guide) ; erreur de lecture du
  direct : « Impossible de lire ce flux » + Reessayer en premier ; `ResizeObserver` sans boucle (mesure
  differee par `requestAnimationFrame`) → zero `pageerror` sur WebKit (Direct, Films, Series).
- CA7 (A UX10, A UX9 partiel, B UX22, A UX5) Navigation : categorie et mode du Direct dans l'URL ; focus
  deplace dans les panneaux (Lecteur, Profil, Categories, filtres) et rendu a l'ancre a la fermeture ;
  fleches ← → entre les cartes d'une rangee horizontale et entre les onglets de saisons/resultats, ↑ ↓ entre
  les rangees de l'accueil (navigation spatiale minimale ; la grille du guide et les grilles d'affiches
  restent au Tab) ; focus initial d'une fiche sur son action principale.
- CA8 Tests : e2e (recherche, Direct 390/768/844×390, onglets 320, session expiree, hors ligne avec ecran
  jamais visite, guide 404, fleches dans une rangee) sur chromium + firefox + webkit pour CA3 et CA6,
  unitaires (tri des chaines, regroupement des variantes, messages), `tsc`, `npm run build` ;
  docs/couverture-ui8.md (mutations : CA1 preselection remise → rouge ; CA6 boucle ResizeObserver → rouge).

## Hors lot (note pour plus tard)
Navigation spatiale complete telecommande (grilles, guide), fiche programme du guide, bande-annonce lue dans
l'application, mode hors ligne complet (service worker), Formuler (sortie M3U/Xtream), secours de sortie
par la maison en cas de 458 (decision utilisateur en attente).

## Propriete des fichiers (UI)
| Codeur | Fichiers (src/) |
|---|---|
| ui-6 | screens/Lecture.tsx, screens/lecture/*, screens/FicheFilm.tsx, screens/FicheSerie.tsx, components/{CalquesLecture,ControlesLecteur,ErreurLecture,PanneauInfos,PistesLecteur,DialogueDiffusion,MiniLecteur,PanneauLecteur,Variantes,FaitsContenu,ResumeContenu}.tsx, lib/{pip,qualite,capacites,politique-reprise,sortie-lecteur,cast,saisons,series-episodes,details*,lecteur,flux,erreurs-hls}.ts, tests e2e us20/us36/us41/us43-lecteur |
| ui-7 | components/{MenuContextuel,CarteAffiche,CarteReprise,VignetteReprise,BoutonAVoir→BoutonMaListe,ChipPreparation,Toast,ALaUne,RangeeHorizontale,LigneRecente,GrilleAffiches,Badge4K,Notification}.tsx, screens/{AVoir→MaListe,Accueil}.tsx, lib/{a-voir*→ma-liste*,episodes-suivants,reprises-accueil,identifiants,disponibilite,synchro,db,preferences,requetes,vu,toasts,libelles}.ts, etat.ts (fonctions `ouvrir*`/`lire*` seulement), tests e2e us43/us36 (accueil) |
| ui-8 | App.tsx, Portail.tsx, index.html, styles/*, components/{EnTete,BarreLaterale,MenuProfil,PanneauSuggestions,Recherche,ResultatsRecherche,BarreFiltres,FeuilleFiltres,BandeauImport,BandeauAbonnement,PanneauCoulissant,GrilleCartes,CarteDirect,CarteChaine*,LigneChaine,LigneCategorie,ListeZapping,MiniGuide,BandeauChaine,PlaqueChaine,Diagnostic,Couche}.tsx, screens/{Connexion,ChoixProfil,Guide,Parcours,Reglages}.tsx, screens/parcours/*, lib/{auth,errors,session-expiree,chaines-guide,epg,modes-direct,recherche*,facettes,import*,plan-import,ndjson,catalogue,api}.ts, etat.ts (navigation, `arreterLecture`, onglets), tests e2e us19/us21/us28/us32/us35/us39* |
Fichiers partages (faux serveurs tests/e2e/serveur-factice.mjs et serveur-fictif.mjs, tests/e2e/banc.mjs,
package.json) : chacun n'AJOUTE que ce dont il a besoin, sans reecrire l'existant ; le coordinateur fusionne.
