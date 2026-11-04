# Audit B — Fiches film et série, lecteur, « Continuer à regarder », contenus vus

Relecteur B. Code lu en lecture seule dans `/opt/mytv-ui/src` (bundle `dist` du 4 novembre 2026). Tout a été
rejoué contre le faux serveur `tests/e2e/serveur-factice.mjs` (catalogue 20 chaînes / 30 films / 6 séries,
média factice MP3 de 6 s pour un film et 18 s pour un épisode), en Chromium Playwright, en **bureau 1360×860**
et **téléphone 390×844** (tactile émulé). Aucun contact avec la production ni le fournisseur.

Scripts et captures : `/opt/mytv-work/audit/B/` — `commun.mjs` (socle), `01-fiche-film-lecteur.mjs`,
`02-serie-enchainement.mjs`, `03-continuer-a-regarder.mjs`, `04-erreurs-diffusion.mjs` (variante `REMOTE=non`
= navigateur sans Remote Playback). Journal brut de 550 constats : `constats.jsonl` ; captures
`0N-<gabarit>-NN-<nom>.png` (90 fichiers) ; sorties `0N-<gabarit>.log`.

Conventions du faux serveur utilisées : `__test/a-voir`, `__test/preparation` (états prêt / attente),
`__test/disponibilite` (sonde lente), `__test/expirer-sessions` ; les 503 / 415 / 401 sur le média et le
lien de diffusion (`POST /cast/lien`, absent de ce faux serveur) sont injectés par `page.route`.

---

## 1. Inventaire

Légende : **OK** conforme ; **BUG n** renvoi à la liste de la section 2 ; **UX n** renvoi à la section 3.

### 1.1 Fiche film (`src/screens/FicheFilm.tsx`) — captures `01-bureau-01`, `01-mobile-01`, `01-bureau-03`

| Élément | Action attendue | Observé |
|---|---|---|
| « ← Retour » | Revenir à l'écran précédent | `history.back()` : OK (mais incohérent avec la fiche série, BUG 4 / UX 5) |
| Fil d'Ariane « Film » | Information | OK |
| Jaquette, fond flouté | Affichage | OK (image absente → plaque de couleur) |
| Titre, méta (année · durée · ★ note · genre · âge) | Affichage | OK : « 2019 · 1 h 40 · ★ 7,8 · Drame, Thriller · 12+ » |
| Résumé, faits (réalisateur, casting, pays, sortie) | Affichage ; « Réessayer » si indisponible | OK ; hors ligne : « Résumé momentanément indisponible. Réessayer » (`01-bureau-18`) |
| Ligne de disponibilité (sonde) | Une sonde à l'ouverture, hors lecture | OK : « Disponible immédiatement » ; « Lent (…) : préparer d'avance ? » + bouton « À voir » ; 1 requête `/vod/{id}/disponibilite` ; aucune sonde pendant une lecture |
| Badge « Prêt » à côté du titre | Visible si le serveur tient le fichier | OK |
| **Regarder** (principal) | Lancer la lecture | OK → `/lecture/<clé>` ; ne dit jamais « Reprendre » même quand une position existe (UX 3) |
| **À voir / Retirer de « À voir »** | Bascule, `aria-pressed` | OK ; aucun retour visuel autre que le libellé (UX 14) |
| **Ajouter aux / Retirer des favoris** | Bascule | OK ; aucune notification |
| **Bande-annonce** (lien) | Ouvre YouTube dans un nouvel onglet | OK (`target=_blank`) ; bloqué ici (hôte externe) ; sans issue sur un navigateur de téléviseur (UX 21) |
| Indicateur « vu » / progression | Attendu d'après les références | **Absent** : un film commencé à 50 % affiche la même fiche qu'un film jamais ouvert (UX 3) |
| Échap | Fermer la fiche | OK (= Retour) |
| Tab | Atteindre « Regarder » | 12 tabulations (rail, recherche, profil d'abord) avant « ← Retour » puis « Regarder » (UX 22) |
| Mobile 390 px | Boutons 44 px, empilés | OK (`01-mobile-01`) |
| Hors ligne (ouverture depuis l'Accueil) | Fiche lisible depuis IndexedDB | OK (`01-bureau-18`) ; mais ouvrir un écran jamais visité hors ligne fait tomber l'interface (BUG 9) |

### 1.2 Fiche série (`src/screens/FicheSerie.tsx`) — captures `02-bureau-01`, `02-mobile-01`, `02-bureau-07`, `02-bureau-11`

| Élément | Action attendue | Observé |
|---|---|---|
| « ← Retour » / Échap | Revenir d'où l'on vient | **Va vers l'onglet Parcours retenu** : `/direct` quand on arrive depuis l'Accueil ou un lien (BUG 4) |
| Fil d'Ariane « Série · iptva:s5002 » | Information | Identifiant technique du fournisseur affiché (UX 17) |
| Jaquette, titre, méta (saisons · épisodes · genre · année · note/10 · durée par épisode) | Affichage | OK bureau ; **coupé à droite en mobile** (BUG 6) |
| Résumé (zone défilante 7 lignes), faits, Bande-annonce | Affichage | OK |
| Ligne de sonde « S01E01 · Lent (…) : préparer d'avance ? » + « À voir » | Sonde du prochain épisode non vu | OK ; disparaît dès que la série est dans « À voir » ou que l'épisode est vu |
| **À voir / Retirer de « À voir »** (en-tête) | Bascule sur la série | OK |
| **Action principale « Reprendre S01E02 » / « Lire »** | Attendue (Netflix, Disney+, Plex, Apple TV) | **Absente** : aucun bouton de lecture dans l'en-tête, il faut trouver la ligne « Repris en cours » (UX 1) |
| Onglets de saisons (`role=tablist`) | Clic ; flèches ← → au clavier | Clic OK ; flèches sans effet (Tab seulement) ; compteur d'épisodes par onglet OK |
| Ligne d'épisode (bouton) | Lancer l'épisode ; infobulle titre + résumé | OK ; `title` natif = titre + durée + résumé |
| Pastille « Vu » + ligne grisée | Position ≥ 95 % | OK après lecture complète ou enchaînement (`02-bureau-07`) |
| Mention « Repris en cours » | Position > 0 | OK, mais sans barre ni temps restant (UX 1) |
| Badge « Prêt » sur l'épisode | Fichier préparé | OK |
| Badge « EAC3 · téléviseur » | Audio non décodable | Non reproductible (le faux serveur ne donne pas de codec) ; lu dans le code |
| **« À voir à partir d'ici »** (par ligne) | Ajoute la série avec `depuis` = cet épisode | OK bureau : un seul point de départ, se déplace sans message ; recliquer le bouton pressé **retire toute la série** (UX 11) ; **absent en mobile** (`FicheSerie.tsx:415`, UX 11) |
| Marquer un épisode vu / non vu | Attendu (Plex, Netflix) | **Inexistant** sur la fiche (UX 13) |
| Liste virtualisée > 40 épisodes | Fenêtrage | Lu dans le code (6 épisodes seulement ici) |
| Erreur de chargement / fiche vide | Message + « Réessayer » | Lu dans le code (non provoqué) |
| Mobile : largeur | 390 px | **Fiche de 400 px** : méta, synopsis, sonde, durées et pastilles coupés (BUG 6, `02-mobile-01`, `02-mobile-06`) |
| Rechargement sur `/lecture/<épisode>` | Renvoi vers la fiche | OK → `/serie/<clé>` |

### 1.3 Lecteur VOD / épisode (`src/screens/Lecture.tsx`, `components/ControlesLecteur.tsx`, `CalquesLecture.tsx`) — captures `01-bureau-04`, `01-mobile-04`, `02-bureau-03`

Barre supérieure :

| Élément | Attendu | Observé |
|---|---|---|
| **Retour** (flèche, « la lecture continue dans le mini-lecteur ») | Revenir à la fiche d'où l'on vient | Film : **`/films`** (liste), jamais la fiche (UX 4) ; épisode : fiche série OK |
| Pays, titre, catégorie, ligne technique « MKV · direct » | Information | OK (ligne technique masquée en mobile) |
| Mobile : flèche de retour de l'en-tête + flèche du lecteur | Un seul retour | Doublon (UX 9) |

Barre de commandes (vod) : `Pause/Lecture`, `Reculer 10 s`, `Avancer 10 s`, muet, volume, barre de position, `Qualité`, `Infos`, `Diffuser`, `PiP`, `Arrêter le flux`, `Plein écran` ; mobile : sans muet/volume/PiP, cibles 44 px.

| Élément | Attendu | Observé |
|---|---|---|
| Pause / Lecture (bouton, Espace) | Bascule | OK |
| ← / → et boutons ±10 s | Déplacement | OK (direct : `currentTime` ; remux : `?t=` lu dans le code, testé par `us20`) |
| Barre de position (`range`) | Déplacement | OK ; absente sans durée connue (lu dans le code) |
| Muet (bouton, M), volume | Bascule, 0 = muet | OK ; ↑ ↓ sans effet en VOD (UX 8) |
| **Qualité : Auto / Éco** | Rouvre le flux au même point | **Rouvre au début** (BUG 3, `01-bureau-06`) ; notification « Qualité Éco : flux réencodé… » OK |
| **Informations sur le flux** (I) | Panneau latéral, Échap ferme | OK ; pour un épisode : intitulé « Informations sur le film », aucun résumé d'épisode (UX 19) |
| **Diffuser vers un téléviseur** (C) | Dialogue, choix d'appareil, arrêt, copie du lien | OK (voir 1.5) |
| **Image dans l'image** | PiP document (Chrome) ou natif | Chrome : fenêtre PiP OK, retour OK ; **PiP natif refusé → erreur plein cadre « Relais » et « Réessayer » rouvre le flux** (BUG 7, `01-bureau-10`) ; Firefox : même erreur (lu dans le code) |
| **Arrêter le flux** (croix) | Libère la connexion, mémorise la position, sort du lecteur | **La vidéo continue invisible, position non enregistrée, exception** (BUG 1) ; croix collée à « Plein écran » (UX 9) |
| **Plein écran** (bouton, double-clic) | Plein écran du cadre | OK Chromium ; **sans effet sur iPhone** (BUG 8, lu dans le code) |
| Clic simple sur l'image | Pause (références) | Rien : réveille seulement les commandes (UX 8) |
| Masquage des commandes | 3,2 s d'inactivité, réveil à la souris / touche | OK |
| Rappel des touches « ← → 10 s · Espace pause · M muet · F favori · I infos » | Information | OK bureau ; F = favori (UX 8) |
| Tab | Parcours des boutons | Retour > position > lecture > … > plein écran > favori ; commandes masquées restent focalisables (réveil OK) |
| Espace sur un bouton focalisé | Activer le bouton (télécommande) | **Met en pause**, le bouton n'est pas activé (BUG 14) |
| Échap (sans calque) | Quitter l'écran, lecture en mini-lecteur | OK, mais même destination que Retour (`/films`) ; avec le dialogue de reprise ouvert, quitte sans le fermer (BUG 13) |
| Pied « En lecture 00:03 / 00:17 · titre · favori (F) » | Information + favori | OK |
| Colonne droite « Pistes audio / Sous-titres / Catégorie » | Choix des pistes | Toujours vide pour un MP4 (messages techniques permanents) (UX 10) ; sous-titres : `role=radio` lu dans le code |
| Notification « « X » arrêté — l'abonnement n'autorise qu'un flux » | Au remplacement d'un contenu | Émise **aussi à chaque enchaînement automatique d'épisode** (BUG 12) |

Calques :

| Calque | Attendu | Observé |
|---|---|---|
| **Reprendre la lecture ?** (« Reprendre à 01:10 » / « Depuis le début ») | Proposé une fois, > 60 s | Affiché pendant que le film **joue déjà** au point mémorisé derrière (UX 3) ; sous 60 s : reprise au début sans le dire alors que la carte « Continuer » annonçait un reste (UX 3) ; Échap ne le ferme pas (BUG 13) |
| **Épisode suivant dans N s** (« Lire maintenant » / « Annuler ») | À 15 s de la fin, 10 s, annulable Échap/clic, « prêt » si préparé | OK : apparition à 15 s de la fin, mention « prêt (préparé d'avance) », Échap annule, rien ne démarre après (11 s vérifiées), fin de saison → S02E01 lu dans l'API, démarrage automatique OK ; **continue à décompter pendant la pause** (BUG 10) ; mobile : recouvre la barre de titre (`02-mobile-04`) |
| Marquage vu à l'enchaînement | Épisode quitté noté à sa durée | OK (« Lire maintenant » à 3 s → position = durée ; fin de décompte idem ; fin naturelle idem) |
| **Film terminé · suivant dans votre liste « À voir »** (« Regarder » / « Fermer ») | Film fini : proposition sans démarrage | OK pour un film (rien après 11 s, « Regarder » ouvre le suivant, Échap ferme) ; **à la fin du dernier épisode d'une série, propose la série qu'on vient de finir, libellé « Film terminé »** (BUG 11, `02-bureau-12`) |
| Fin sans liste / après annulation du décompte | Proposition de revoir, de revenir | **Rien** : image figée, barre à 00:17 / −00:00 (UX 6) |
| **Reconnexion** (bandeau) | « Tentative n sur 3 · nouvel essai dans N s » ; place occupée : message serveur + attente | OK (`04-bureau-01`, `04-bureau-04`, `01-bureau-19`) ; reprise automatique au point atteint − 2 s, notification « Connexion au relais rétablie » OK ; pas de dialogue de reprise rejoué OK |
| **Erreur** (carte : couche, message, action, détail, tentatives, boutons) | Nommée, actions | 415 : « FORMAT NON LISIBLE … HEVC … », « Diffuser vers le téléviseur » en premier si possible ; réseau : « Le serveur est injoignable… 3 tentatives » ; **« Retour » = arrêt du flux** (UX 18) ; **mobile : boutons sous la barre de commandes** (BUG 5) |
| Proposition « Passer en Éco ? » | Après 30 s de débit juste (direct) | Direct seulement, non rejoué (périmètre A) |
| Direct : bandeau chaîne, mini-guide, variantes, zapping ↑ ↓, R, G, V | — | Périmètre A ; inventoriés dans `Lecture.tsx:839-900`, non rejoués ici |

### 1.4 Mini-lecteur (`components/MiniLecteur.tsx`) — captures `01-bureau-11`, `01-mobile-09`

| Élément | Attendu | Observé |
|---|---|---|
| Vignette / ligne « titre · sous-titre », barre d'avancement | Information, retour au lecteur | OK ; position toujours mémorisée toutes les 5 s pendant le mini-lecteur OK |
| Réduire / agrandir (bureau) | Bascule | OK |
| Muet | Bascule | OK |
| **Fermer la lecture** | Libère la connexion, reste sur l'écran courant | **Même défaut que « Arrêter »** (BUG 1) ; depuis l'Accueil, renvoie vers la fiche de la série (`/serie/…`) au lieu de rester (UX 4) |
| État « Erreur de lecture », « En pause », « Connexion… » | Affichage | OK (`04-bureau-03`) |

### 1.5 Diffusion (`components/DialogueDiffusion.tsx`, `lib/cast.ts`) — captures `04-bureau-05/06/07`, `04b-bureau-01/02`, `01-mobile-08`

| Élément | Attendu | Observé |
|---|---|---|
| Ouverture (bouton, C), fermeture (croix, Échap, C) | — | OK |
| « Choisir un appareil » (Remote Playback) | Lien signé `cap=hevc,eac3,ac3&t=<s>`, sélecteur natif, état « Diffusion en cours — cette page sert de télécommande » | OK ; bouton cast marqué actif |
| Pendant la diffusion : barre locale | Télécommande | Barre inchangée (Pause, ±10 s, position 00:00) ; Espace / Pause sans effet et sans explication (UX 18) |
| « Arrêter la diffusion » | Retour local, position conservée | OK (3 s → reprise à 1,7 s, marge 2 s) |
| Sans Remote Playback : « Copier le lien pour le téléviseur » | Lien absolu copié, « Lien copié — valable jusqu'à HH:MM » | OK ; repli « Lien prêt (à copier ci-dessous) » lu dans le code |
| Erreur 415 sans Remote Playback | Issue proposée | Texte « Le diffuser vers un téléviseur… » mais seuls « Réessayer / Retour » : le lien à copier n'est pas proposé (UX 18) |
| « EAC3 OK » | Sur erreur codec | Lu dans le code |

### 1.6 « Continuer à regarder » (`screens/Accueil.tsx`, `components/CarteReprise.tsx`, `MenuContextuel.tsx`) — captures `03-bureau-01/02/03`, `03-mobile-01/03`

| Élément | Attendu | Observé |
|---|---|---|
| Rangée (12 cartes max, contenus < 95 %, une série = une carte) | — | OK ; rangée vide jamais affichée (`01-bureau-17`) ; pas de « Tout voir » |
| Carte 16:9 : titre, « Film · 2005 » / « Série · épisode en cours », « Reste N min », barre | Information | OK ; **« Reste 0 min »** sous la minute ; une série ne dit pas quel épisode (UX 2) ; `aria-label` « Reprendre Série Test 1, Série · épisode en cours » |
| Clic / Entrée sur une carte | Reprendre | Film : lecteur (puis dialogue de reprise ou reprise au début, UX 3) ; **série : ouvre la fiche, ne reprend pas** (UX 2) |
| Bouton « … » (permanent, 44 px mobile) | Menu | OK (clic, Entrée) |
| Menu : **Reprendre / Marquer comme vu / Retirer de la liste** | Agir | **Inopérant à la souris et au doigt** (le menu se ferme au `pointerdown` avant le clic) ; fonctionne uniquement au clavier (Entrée) (BUG 2, `03-bureau-04`) |
| « Marquer comme vu » (clavier) | Position = durée, carte retirée | OK film ; série : seul l'épisode affiché est marqué, l'épisode précédent en cours remonte (BUG 15, `03-bureau-06`) ; aucun retour, pas de « non vu » (UX 12) |
| « Retirer de la liste » (clavier) | Masquer | Efface les positions (toutes celles de la série) : la progression est perdue (UX 12) |
| Flèches ↑ ↓ dans le menu, Échap | Navigation clavier | Flèches sans effet ; Échap ferme OK |
| Mobile | Menu sous le doigt | OK, chevauche la barre d'onglets (`03-mobile-03`) |

### 1.7 Contenus vus

| Élément | Attendu | Observé |
|---|---|---|
| Écran ou rangée « Vus » / historique | Attendu (Plex, Netflix « Activité ») | **Inexistant.** Seules traces : pastille « Vu » sur les épisodes, retrait silencieux de « Continuer à regarder », filtre « En cours » de la grille Films (périmètre A) ; aucune trace « vu » sur une fiche film ni sur une carte (UX 13) |
| Seuil « vu » | Un seul | 90 % dans le lecteur (`Lecture.tsx:81`), 95 % ailleurs (BUG 16) |

### 1.8 Hors ligne et session expirée

| Cas | Attendu | Observé |
|---|---|---|
| Lecture hors ligne (fiche déjà ouverte) | Bandeau puis erreur nommée, reprise sur « Réessayer » | OK bureau (`01-bureau-19/20`) ; mobile : « Réessayer » inatteignable (BUG 5) |
| Serveur arrêté en pleine lecture | Idem | OK (`04b-bureau-04/05`) |
| Ouvrir un écran jamais visité hors ligne | Message | **« L'interface n'a pas pu démarrer »** plein écran (BUG 9, `01-bureau-17`) |
| Session expirée pendant une lecture | Retour à la connexion, explication | Retour à l'écran de connexion **sans aucun message**, lecteur et mini-lecteur disparus (UX 20, `04-bureau-08`) |
| Position de reprise vers le serveur | Envoi | 30 s après la dernière note (`synchro.ts:590`), rien à la fermeture de l'onglet (lu dans le code) (UX 20) |

---

## 2. Bugs

Sévérité : **bloquant** (empêche l'usage ou met en danger la connexion unique), **majeur** (fonction cassée ou résultat faux), **mineur**.

### BUG 1 — bloquant — « Arrêter le flux » / « Fermer la lecture » : la lecture continue invisible, le flux n'est pas libéré, la position de sortie n'est pas enregistrée
- Reproduction (bureau et mobile, film ou épisode) : fiche d'un film jamais terminé → « Regarder » → laisser jouer 2 s → croix « Arrêter le flux » (ou « Fermer la lecture » du mini-lecteur, ou « Retour » du panneau d'erreur).
- Attendu : l'élément vidéo est détaché (`src` retiré, requête fermée, place rendue), `reprises` reçoit la position atteinte, l'écran revient à la liste.
- Obtenu : l'URL passe à `/films` et le mini-lecteur disparaît, mais `<video>` reste dans le document avec sa source, `paused=false`, `currentTime` qui continue d'avancer (3,92 s → 5,12 s mesurés 1,2 s après l'arrêt) : le son continue et le fournisseur garde la connexion jusqu'à la fin du fichier. `reprises` ne contient rien pour ce film (seule la note périodique de 5 s l'alimente). Exception non interceptée « Stale read from <Show>. » à chaque arrêt (3 à 5 par parcours).
- Cause (lue dans le code) : `Lecture.tsx:605-630` — le nettoyage de l'effet de mémorisation appelle `noter()`, qui lit `duree()` → `direct()` → `props.entree.type` ; la sélection est déjà `null` et l'accesseur du `<Show when={selection()}>` (`App.tsx:319`) lève « Stale read ». Les nettoyages suivants du même propriétaire ne s'exécutent plus, dont celui de l'effet de source (`Lecture.tsx:498`, `attache.detacher()`), qui seul vide `src` et rend la place. Quand le contenu est déjà « terminé » (`cleTerminee`, `Lecture.tsx:614`), `noter()` sort avant la lecture et l'arrêt se passe bien — c'est pourquoi les tests existants ne le voient pas.
- Preuves : `constats.jsonl` id `arret.video-fantome`, `arret.erreur-page`, `position.sortie` ; captures `01-bureau-12-apres-arret.png`, `01-mobile-10-apres-arret.png`.

### BUG 2 — majeur — Le menu « … » de « Continuer à regarder » n'agit ni à la souris ni au doigt
- Reproduction : Accueil → carte de « Continuer à regarder » → « … » → cliquer « Marquer comme vu » (ou « Reprendre », « Retirer de la liste »).
- Attendu : l'action s'exécute, la carte change.
- Obtenu : le menu se ferme, rien ne change (`reprises` inchangée, carte présente, même après rechargement). Même résultat au tap. Avec Tab + Entrée, l'action s'exécute.
- Cause : `MenuContextuel.tsx:31` pose un écouteur `pointerdown` **en capture** sur `window` qui appelle `surFermer` ; la phase de capture précède le `onPointerDown … stopPropagation` du menu (`MenuContextuel.tsx:48`), le menu est démonté avant que le `click` n'atteigne l'entrée. Séquence mesurée : `pointerdown` reçu alors que `[role=menu]` existe → au `mousedown` il n'existe plus → aucun `click`.
- Preuves : `continuer.menu.souris`, `continuer.menu.tap` (bureau et mobile) ; `03-bureau-04-apres-clic-souris-marquer-vu.png`.
- Conséquence directe de la plainte « gestion des vus incompréhensible » : les trois seuls gestes de gestion des vus sont morts pour un utilisateur à la souris ou sur téléphone.

### BUG 3 — majeur — Basculer la qualité Auto/Éco pendant un film le remet au début
- Reproduction : film en cours à 4,2 s → bouton « Qualité : Auto ».
- Attendu : réouverture au même point (comme à la reconnexion ou à l'arrêt d'une diffusion).
- Obtenu : flux rouvert (`?qualite=eco`), lecture reprise à 0,7 s. Même chose au retour en Auto.
- Cause : l'effet de source se relance sur `qualite()` (`Lecture.tsx:368`) et repart de `pointAtteint` (`Lecture.tsx:458-459`), qui n'est mis à jour qu'à un déplacement manuel, une erreur ou une diffusion (`Lecture.tsx:209, 643, 758`), jamais par la lecture elle-même.
- Preuve : `lecteur.qualite.position` ; `01-bureau-06-lecteur-film-eco.png`.

### BUG 4 — majeur — « Retour » et Échap de la fiche série renvoient vers Direct
- Reproduction : ouvrir une fiche série depuis l'Accueil, la recherche ou un lien (sans être passé par l'onglet Séries) → « ← Retour » ou Échap.
- Attendu : écran précédent (comme la fiche film, qui fait `history.back()`).
- Obtenu : `/direct` (liste des chaînes). Depuis le lecteur, « Retour » d'un épisode ramène bien à la fiche, mais la fiche ramène ensuite à Direct.
- Cause : `App.tsx:87` `surRetour={() => poserEcran('parcours')}` → `etat.ts:131-132` `CHEMIN_ONGLET[onglet()]`, onglet retenu initialisé à `'live'`.
- Preuve : `serie.echap`, `serie.retour` (= `/direct`).

### BUG 5 — majeur (mobile) — Le panneau d'erreur est recouvert par la barre de commandes : « Réessayer » et « Retour » inatteignables
- Reproduction (390×844) : provoquer une erreur (hors ligne, 415) dans le lecteur.
- Attendu : la carte d'erreur et ses boutons au-dessus de tout.
- Obtenu : la carte occupe la vidéo de 219 px ; ses boutons tombent sous la barre de commandes (`z-index: 2`, `ControlesLecteur.tsx:84`) alors que la carte n'a pas de `z-index` (`ErreurLecture.tsx:50`). Le clic sur « Réessayer » touche la barre : `horsligne.reprise` KO en mobile.
- Preuves : `04-mobile-02-erreur-415.png`, `01-mobile-18-lecteur-hors-ligne-erreur.png`.

### BUG 6 — majeur (mobile) — Fiche série plus large que l'écran (400 px pour 390) : textes, durées et pastilles coupés
- Reproduction : fiche série en 390 px.
- Obtenu : `header` et lignes d'épisode mesurent 400 px (`clientWidth`), le viewport 390 : méta « … Drame, Thriller · 20 », synopsis, ligne de sonde tronqués ; durée « 40:00 » hors écran ; pastilles « Vu » / « Prêt » à moitié visibles. La fiche film, elle, fait 390 px.
- Cause probable : marges/rembourrage de `FicheSerie.tsx:135` (`marge()`) et grille de l'en-tête `FicheSerie.tsx:501-512` sans `min-width: 0` sur toute la chaîne ; les lignes `FicheSerie.tsx:341-425`.
- Preuves : `02-mobile-01-fiche-serie.png`, `02-mobile-06-fiche-serie-apres-e01-vu.png` ; mesure dans `02-mobile.log` / script de mesure (`header: 400/400, episode: 400/400, vw 390`).

### BUG 7 — majeur — L'échec de l'image dans l'image devient une erreur de lecture plein cadre, classée « Relais », dont « Réessayer » rouvre le flux
- Reproduction : navigateur sans Document PiP (Firefox, Safari, ou Chrome avec l'API retirée) → bouton « Image dans l'image ». Rejoué ici en retirant `documentPictureInPicture`.
- Attendu : bascule PiP ou message discret « Image dans l'image indisponible ».
- Obtenu : carte « RELAIS — L'image dans l'image n'a pas pu s'ouvrir. — Réessayer. — InvalidStateError… » par-dessus la vidéo (qui continue), état « Erreur de lecture » dans le mini-lecteur ; « Réessayer » exécute la relance de panne : 1 réouverture du flux mesurée (une connexion de plus chez le fournisseur). Sur Firefox, `video.requestPictureInPicture` n'existe pas : même carte à chaque pression (lu dans `lib/pip.ts:26-31`, `Lecture.tsx:720-726`).
- Cause : `Lecture.tsx:711-726` (`poserErreur(new ErreurLecteur('relais', …))`), `actionsErreur` `Lecture.tsx:936-946`.
- Preuves : `lecteur.pip.natif.erreur`, `lecteur.pip.natif.reessayer` ; `01-bureau-10-lecteur-film-pip-erreur.png`.

### BUG 8 — majeur (iPhone) — Le plein écran ne fait rien sur iPhone — lu dans le code
- `Lecture.tsx:796-799` : `cadre.requestFullscreen?.()` sur un `<div>`. Safari iOS n'implémente pas `Element.requestFullscreen` (seulement `video.webkitEnterFullscreen()`), l'appel optionnel est silencieusement ignoré. Avec `playsinline` (`Lecture.tsx:1007`), la vidéo reste dans un cadre de 219 px : aucun moyen de regarder un film en plein écran sur l'appareil le plus utilisé. Non reproductible en Chromium ; conforme à la documentation WebKit.

### BUG 9 — majeur — Hors ligne, ouvrir un écran jamais visité fait tomber toute l'interface
- Reproduction : se connecter, aller sur une fiche (`/film/…`) sans passer par l'Accueil, couper le réseau, cliquer « Accueil » dans le rail.
- Attendu : écran depuis le cache, ou message ciblé.
- Obtenu : page entière remplacée par « L'interface n'a pas pu démarrer — TypeError: Failed to fetch dynamically imported module …/Accueil-….js » ; il faut recharger.
- Cause : écrans chargés à la demande (`App.tsx:23-31`), aucun service worker ni préchargement ; l'erreur de chargement de module remonte au portail.
- Preuve : `01-bureau-17-accueil-hors-ligne.png` (première exécution).

### BUG 10 — mineur — Le décompte « Épisode suivant » continue pendant la pause et lance le suivant
- Reproduction : épisode, décompte affiché → Espace (pause) → attendre.
- Obtenu : « dans 10 s » → « dans 8 s » après 2,2 s de pause ; l'épisode suivant démarre à zéro alors que l'utilisateur a mis en pause.
- Cause : `calques.ts:78-88` — `setInterval` indépendant de l'état du média ; `decisionEnAttente` ne sert qu'au masquage des commandes.
- Preuve : `decompte.pause`.

### BUG 11 — mineur — Fin du dernier épisode : « Film terminé · suivant dans votre liste « À voir » » propose la série qu'on vient de finir
- Reproduction : série dans « À voir », lire le dernier épisode jusqu'au bout.
- Obtenu : calque « Film terminé · suivant dans votre liste « À voir » · prêt — Série Test 2 — Ouvrir la série ».
- Cause : `Lecture.tsx:556-566` appelle `prochainAVoirApres(clé de l'épisode)` ; `a-voir-regles.ts` `prochainDansListe` : clé absente → premier élément (la série elle-même) ; libellé figé « Film terminé » (`CalquesLecture.tsx:229`), aria-label « Film terminé ».
- Preuve : `02-bureau-12-fin-dernier-episode.png`, `02-mobile-11`.

### BUG 12 — mineur — Notification alarmante à chaque enchaînement automatique
- « « Série Test 2 — S01E02 » arrêté — l'abonnement n'autorise qu'un flux à la fois » (ton « limite ») s'affiche quand le lecteur enchaîne de lui-même l'épisode suivant, ou sur « Lire maintenant ». Cause : `Lecture.tsx:392-394` ne distingue pas un enchaînement voulu d'un remplacement.
- Preuve : `enchainement.notif`.

### BUG 13 — mineur — Échap pendant « Reprendre la lecture ? » quitte l'écran sans fermer le dialogue
- Obtenu : `/films`, mini-lecteur, le film joue ; au retour, le dialogue est toujours là. Cause : `Lecture.tsx:816-818` `calqueOuvert` ignore `calques.reprise()`, `calques.ts:115-124` `fermerCalque` ne le traite pas.
- Preuve : `reprise.echap`, `01-bureau-16-dialogue-reprise-apres-echap.png`.

### BUG 14 — mineur (télécommande) — Espace sur un bouton focalisé met en pause au lieu d'activer le bouton
- Tab jusqu'à « Arrêter le flux » (ou tout bouton) → Espace : la vidéo bascule en pause, le bouton n'est pas activé (`clavier.ts:50` intercepte Espace et `preventDefault`). Entrée fonctionne. Sur un navigateur de téléviseur où « OK » envoie Entrée c'est acceptable, sur un clavier c'est contraire à l'usage.
- Preuve : `clavier.espace-bouton`.

### BUG 15 — mineur — « Marquer comme vu » sur une carte de série ne marque que l'épisode affiché
- Série avec S01E01 à 50 % et S01E02 à 40 % : « Marquer comme vu » → S01E02 noté vu, la carte reste avec S01E01 (« Reste 0 min ») ; l'utilisateur croit que ça n'a rien fait. Cause : `Accueil.tsx:424-436` (`marquerVu(cleReprise)` sur la seule clé de l'épisode).
- Preuve : `continuer.marquer-vu.serie`, `03-bureau-06-apres-marquer-vu-serie.png`.

### BUG 16 — mineur — Deux seuils « vu » et un reste à « 0 min »
- Lecteur : 90 % (`Lecture.tsx:81`) ; fiche série, reprise, rangée : 95 % (`FicheSerie.tsx:140`, `preferences.ts:73`, `reprises-accueil.ts:20`). Une position venue d'un autre appareil entre 90 et 95 % est « en cours » sur l'Accueil et « vue » au lecteur suivant. `Accueil.tsx:177-181, 198` affiche « Reste 0 min » sous la minute.

### BUG 17 — mineur — Décompte « Épisode suivant » et dialogue de diffusion recouvrent la barre de titre du lecteur en mobile
- `02-mobile-04-decompte-episode-suivant.png`, `01-mobile-08-lecteur-film-diffusion.png` : le calque déborde par le haut du cadre de 219 px (`CalquesLecture.tsx` `borne()` ne limite que la hauteur).

---

## 3. Expérience

### UX 1 — La fiche série n'a pas d'action principale
L'en-tête ne contient que « À voir » et « Bande-annonce » ; pour reprendre, il faut parcourir les lignes et repérer « Repris en cours » (sans barre ni temps restant), ou le prochain épisode non vu par déduction (seule la ligne de sonde, quand elle existe, nomme « S01E01 »). Référence : Netflix, Disney+, Prime Video et Plex ouvrent la fiche sur **un bouton « Reprendre S02E01 · 23 min restantes »** (ou « Lire S01E01 » pour une série jamais ouverte) avec une barre de progression ; Apple TV « À suivre » fait de même. La même règle (`prochainAVoir`, `episodes-suivants.ts`) existe déjà pour la sonde : elle n'est pas montrée à l'utilisateur.

### UX 2 — La carte « Continuer à regarder » d'une série ne reprend rien
Clic et « Reprendre » du menu passent par `ouvrir()` (`etat.ts:172-188`), qui pour une série ouvre la fiche. La carte dit « Série · épisode en cours » sans nommer l'épisode ni le temps restant réel (« Reste 0 min »). Référence : Netflix / Prime / Disney+ : la carte nomme l'épisode (« S2:E4 ») et le clic **lance** l'épisode ; la fiche est accessible par un bouton secondaire.

### UX 3 — La reprise d'un film est demandée au mauvais moment, et contredite sous la minute
Sur la fiche, « Regarder » ne dit pas qu'une position existe. Au lancement, le film s'ouvre et **joue déjà** au point mémorisé derrière un dialogue « Reprendre la lecture ? Reprendre à 01:10 / Depuis le début » (`Lecture.tsx:460` puis `play()` `:489`) ; depuis « Continuer à regarder » on a déjà cliqué « Reprendre » et on le redemande. Sous 60 s (`SEUIL_REPRISE`), la carte affiche une barre et « Reste 1 min » mais le film repart de zéro sans un mot. Référence : Netflix reprend sans question ; Plex et Apple TV posent la question **sur la fiche** (« Reprendre / Recommencer ») avant d'ouvrir le flux — ce qui, ici, économiserait une connexion fournisseur.

### UX 4 — Quitter le lecteur ne ramène pas d'où l'on vient
« Retour » et Échap d'un film mènent à `/films` (`sortie-lecteur.ts:17-20`) même quand on venait de la fiche ou de l'Accueil ; « Fermer la lecture » du mini-lecteur depuis l'Accueil envoie sur la fiche de la série. Référence : YouTube, Netflix, Plex : retour = écran précédent (pile d'historique), le mini-lecteur ne navigue jamais.

### UX 5 — Quatre « retours » différents
Fiche film = `history.back()` ; fiche série = onglet Parcours retenu (BUG 4) ; lecteur = liste du type ; Échap = l'un ou l'autre selon le calque ouvert (BUG 13). Référence : une seule règle (pile de navigation) sur toutes les applications citées.

### UX 6 — Fin de lecture sans issue
Film terminé sans liste « À voir », ou épisode terminé après annulation du décompte : image figée, « 00:17 / −00:00 », aucun bouton. Référence : Netflix propose « Regarder à nouveau » et revient à la fiche après quelques secondes ; YouTube affiche « Revoir » et des suggestions ; Plex revient à la fiche.

### UX 7 — Pas d'« épisode suivant / précédent » ni de liste d'épisodes dans le lecteur
Pour passer à l'épisode suivant avant la fin, il faut Retour → fiche → ligne. Référence : Netflix (bouton « Épisode suivant » + panneau « Épisodes »), Disney+, Plex (⏭).

### UX 8 — Conventions clavier à contre-emploi
F = favori (`clavier.ts:54`) alors que YouTube, Netflix, Plex, Disney+ réservent F au plein écran, qui n'a aucune touche ; ↑ ↓ sans effet en VOD (volume chez YouTube/Plex) ; clic simple sur l'image ne met pas en pause, double-clic = plein écran (`Lecture.tsx:995`). L'indice « ← → 10 s · Espace pause · M muet · F favori · I infos » est tronqué dès que la vidéo rétrécit.

### UX 9 — Mobile : lecteur sans geste
Cadre de 219 px, pas de tap-pour-pause ni de bouton central, pas de plein écran sur iPhone (BUG 8), deux flèches « retour » superposées (en-tête de la coque + lecteur), croix « Arrêter » à 44 px du bouton « Plein écran » (arrêt accidentel = perte de la place fournisseur), colonne « Pistes audio » vide qui occupe tout l'écran sous la vidéo. Référence : YouTube / Netflix mobile : tap = commandes + gros bouton pause central, double-tap latéral = ±10 s, rotation = plein écran.

### UX 10 — Colonne « Pistes audio / Sous-titres » toujours vide
Pour tout MP4 (donc tout remux, 54,6 % des films), la colonne affiche en permanence « Ce navigateur n'expose pas les pistes audio d'un MP4… » et « Aucune piste de sous-titres » (`pistes.ts:17-30`, `ColonnePistes.tsx:23-24`) : 340 px de bureau pour deux phrases techniques. Référence : Netflix / Plex n'affichent le menu « Audio et sous-titres » que s'il y a un choix, dans un calque sur la vidéo.

### UX 11 — « À voir à partir d'ici » : geste puissant, mal signalé, absent du téléphone
Un clic déplace silencieusement le point de départ ; recliquer le bouton pressé retire **toute la série** alors que le libellé compact dit seulement « À voir » ; le bouton n'existe pas en mobile (`FicheSerie.tsx:415`). Référence : Plex « Marquer vu jusqu'ici » dans le menu de l'épisode, avec confirmation ; Netflix n'a pas d'équivalent mais marque les épisodes vus un par un.

### UX 12 — Menu « … » : trois actions, aucune réversible, aucun retour
Pas de « Marquer non vu » (Plex en a un), « Retirer de la liste » efface définitivement les positions (Netflix « Retirer de la rangée » prévient « Cela supprimera aussi l'historique de visionnage » ; Plex garde l'historique), aucune notification « Marqué comme vu — Annuler ». Flèches ↑ ↓ inertes dans le menu.

### UX 13 — Le « vu » n'existe nulle part comme information
Un film vu disparaît simplement de « Continuer à regarder » ; sa fiche et sa carte n'en gardent aucune trace ; il n'y a ni écran « Vus », ni historique, ni « Regarder à nouveau ». Référence : Netflix (coche + « Regarder à nouveau »), Plex (coche « vu », historique), Apple TV (« Vu » sur la fiche).

### UX 14 — Aucun feedback après un geste de liste
« À voir », favori, « Marquer comme vu » ne changent que le libellé du bouton ; en mobile le bouton est parfois hors écran (fiche série). Référence : Netflix / Disney+ : toast « Ajouté à Ma liste » ; Plex : coche animée.

### UX 15 — Le décompte « Épisode suivant » n'identifie pas l'épisode
Titre technique « Série Test 2 — S01E02 », pas de titre d'épisode ni de vignette, pas d'anneau de progression ; mention « prêt (préparé d'avance) » utile mais en jargon de préparation. Référence : Netflix (vignette + titre + anneau), Disney+.

### UX 16 — Jargon de la sonde et des états
« Lent (1,5 Mbit/s pour 4,4 nécessaires) : préparer d'avance ? », « prêt (préparé d'avance) », badge « Prêt » sur une ligne d'épisode sans explication (seule l'infobulle le dit). Référence : Netflix Smart Downloads (« Téléchargé », icône) ; à harmoniser avec le modèle cible du relecteur C.

### UX 17 — Identifiants techniques visibles
Fil d'Ariane « Série · iptva:s5002 » ; sous le logo du lecteur, l'adresse « 127.0.0.1:…/api/v1/vod/… » tant qu'aucune image n'arrive (utile au diagnostic, pas à l'utilisateur).

### UX 18 — Erreurs et diffusion : les libellés ne disent pas ce qu'ils font
« Retour » du panneau d'erreur **arrête** le flux (`Lecture.tsx:946`) ; pendant une diffusion, la barre locale reste affichée avec Pause et ±10 s inertes ; sur une erreur de format sans Remote Playback, le texte conseille « le diffuser vers un téléviseur » mais n'offre que « Réessayer / Retour » (le lien à copier n'est qu'à deux clics ailleurs). Référence : YouTube / Chromecast remplacent la barre par « Lecture sur <appareil> » et des commandes qui pilotent l'appareil.

### UX 19 — Panneau « Informations » d'un épisode
Intitulé « Informations sur le film » (`PanneauInfos.tsx:50`), aucun résumé, titre d'épisode ni numéro ; seulement « Chaîne de traitement » et « Ajouter aux favoris » (`02-bureau-04`).

### UX 20 — Session expirée et synchronisation silencieuses
Session expirée en pleine lecture : écran de connexion nu, sans « Votre session a expiré » ni retour au contenu après reconnexion (`04-bureau-08`). La position de reprise ne part au serveur que 30 s après la dernière note (`synchro.ts:590`) et jamais à la fermeture de l'onglet : regarder 10 min sur le téléphone puis ouvrir la télé donne une reprise jusqu'à 35 s en retard (et, avec BUG 1, jamais le point exact d'arrêt). Référence : Netflix / Plex envoient la position à chaque pause et à la sortie (`sendBeacon`).

### UX 21 — « Bande-annonce » ouvre YouTube hors de l'application
Sans retour possible sur un navigateur de téléviseur, et sans lecteur intégré. Référence : Netflix / Disney+ lisent la bande-annonce dans l'application.

### UX 22 — Navigation télécommande
Douze tabulations (rail, recherche, profil) avant d'atteindre « Regarder » sur une fiche ; onglets de saisons sans flèches ; menu « … » sans flèches ; boutons masqués mais focalisables dans le lecteur. Référence : les applications TV placent le focus initial sur l'action principale et gèrent ← → dans les listes horizontales.

---

## 4. Synthèse pour le coordinateur

1. **« Arrêter le flux » ne l'arrête pas** (BUG 1) : la vidéo continue invisible, la connexion fournisseur reste prise et la position d'arrêt n'est jamais enregistrée — une exception « Stale read from <Show> » à chaque arrêt. C'est la cause la plus probable des « bugs » ressentis (son fantôme, place occupée, reprise fausse).
2. **Le menu « … » de « Continuer à regarder » est mort à la souris et au doigt** (BUG 2) : « Marquer comme vu » et « Retirer » n'agissent qu'au clavier — d'où une gestion des vus « incompréhensible ».
3. **Reprendre une série est impossible en un geste** : fiche sans bouton « Reprendre S02E01 » (UX 1), carte de l'Accueil qui ouvre la fiche au lieu de reprendre (UX 2), film qui redemande « Reprendre ? » alors qu'il joue déjà ou repart de zéro sous la minute (UX 3).
4. **Navigation incohérente** : Retour/Échap de la fiche série vers Direct (BUG 4), sortie du lecteur vers la liste et non la fiche (UX 4), quatre règles de retour (UX 5), fin de lecture sans issue (UX 6).
5. **Mobile / iPhone dégradé** : fiche série débordante (BUG 6), panneau d'erreur dont les boutons sont sous la barre (BUG 5), pas de plein écran iPhone (BUG 8), pas de « À voir à partir d'ici » ni de tap-pause (UX 9, UX 11).
6. Correctifs secondaires nets : Éco remet le film au début (BUG 3), PiP en erreur « Relais » dont « Réessayer » rouvre le flux (BUG 7), décompte qui court en pause (BUG 10), « Film terminé » qui propose la série finie (BUG 11), notification « arrêté » à chaque enchaînement (BUG 12), hors-ligne qui fait tomber l'interface (BUG 9).
7. Le « vu » doit devenir une information visible et réversible (coche sur fiche/carte, « non vu », « Regarder à nouveau », un seul seuil) — à articuler avec le modèle cible du relecteur C.
