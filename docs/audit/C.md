# Audit C — modèle « vus / prêt / À voir » de bout en bout, et modèle cible

Relecteur C · 2026-11-04 · contre `tests/e2e/serveur-factice.mjs` (dist du 2026-11-04), Chromium Playwright,
gabarits bureau 1360×860 et téléphone 390×844 (tactile). Scripts et captures : `/opt/mytv-work/audit/C/`
(`01-fiches.mjs`, `02-recherche-accueil.mjs`, `03-ecran-a-voir.mjs`, `04-enchainement.mjs`,
`05-menu-contextuel.mjs`, `06-captures-rangee.mjs`, `07-menu-pointerdown.mjs`, 105 captures dans
`captures/`, préfixées `bureau-` ou `mobile-`). Code lu en lecture seule dans `/opt/mytv-ui/src` et
`/opt/mytv/internal`. Aucune requête hors 127.0.0.1 (vérifié par `ctx.route`).

Vocabulaire : « liste » = la liste « À voir » (US-43) ; « reprise » = position mémorisée (`db.reprises`) ;
« vu » = part ≥ 0,95 ; « prêt » = fichier tenu par le serveur ; « sonde » = `GET /vod/{id}/disponibilite`.

---

## 1. INVENTAIRE

Légende de la colonne Observé : **OK** conforme et compréhensible · **BUG n** écart (voir §2) · **UX n** prévu mais déroutant (voir §3).

### 1.1 Fiche film (`FicheFilm.tsx`)

| Élément | Action attendue | Observé (bureau / téléphone) |
|---|---|---|
| Bouton « À voir » (icône +) | Ajoute le film à la liste ; PUT `/profil/a_voir/vod:…` | OK : passe à « ✓ Retirer de « À voir » » ; aucun autre retour (pas de message) — UX 1. Info-bulle « le serveur prépare le fichier d'avance » seulement au survol (UX 2) |
| Bouton « Retirer de « À voir » » | Retire ; DELETE | OK, sans confirmation ni annulation (UX 3) |
| Ligne de sonde « Disponible immédiatement » | Informer | OK ; signification du mot « immédiatement » non expliquée (UX 4) |
| Ligne de sonde « Lent (2,7 Mbit/s pour 4,4 nécessaires) : préparer d'avance ? » + bouton « À voir » bleu | Proposer la préparation | Deux boutons « À voir » sur la fiche (BUG 11) ; une fois ajouté, la question reste affichée sans réponse ni bouton (BUG 10) |
| Ligne de sonde « Prêt » + badge « Prêt » à côté du titre | Dire que le fichier est sur le disque | Dit deux fois (capture `bureau-fiche-film-pret.png`) — UX 5 ; info-bulle du badge seulement au survol |
| Film en liste, **en cours de préparation** (42 %) | L'utilisateur devrait voir l'avancement | Rien : ni badge ni pourcentage, la sonde dit même « Disponible immédiatement » (capture `bureau-fiche-film-en-cours.png`) — UX 6 |
| Film en liste, **en attente** ou **en échec** | idem | Rien n'est dit sur la fiche — UX 6 |
| Film **entamé** (reprise mémorisée) | Dire où l'on en est, proposer « Reprendre » | Rien : bouton « Regarder », aucune barre ni « reste 42 min » (capture `bureau-fiche-film-entame.png`) — UX 7 |
| Film **vu** (terminé ou marqué vu) | Dire « Vu », proposer « Regarder à nouveau » | Rien ; le film sort de la liste côté serveur au prochain `GET` (`internal/api/a_voir.go:164`) sans que l'utilisateur en soit informé ; le bouton reste « Retirer de « À voir » » jusqu'à 120 s (`synchro.ts:593`) puis redevient « À voir » tout seul — UX 8 |
| Bouton « Ajouter aux favoris » (étoile) | Favori | OK techniquement ; quatrième concept à côté de « À voir » (UX 9) |
| Sonde pendant une lecture en cours | Aucune sonde, rien d'affiché | OK (conforme CA5) ; mais la fiche ne dit pas pourquoi la ligne manque — UX 4 |
| Clavier (Tab) | Atteindre Regarder / À voir | Tab part d'abord dans le rail (8 arrêts avant la fiche) ; les boutons de la fiche sont atteignables ensuite — OK |
| Hors ligne | Le geste doit être conservé | OK (ligne Dexie + journal, PUT au retour ; conforme `horsLigne`) |

### 1.2 Fiche série (`FicheSerie.tsx`)

| Élément | Action attendue | Observé |
|---|---|---|
| Bouton « À voir » de l'en-tête | Ajoute la série (sans `depuis`) | OK |
| Bouton « À voir » de la **ligne de sonde** (en-tête) | Même bouton ? | Non : celui-ci ajoute avec `depuis` = prochain épisode non vu (`FicheSerie.tsx:544`) ; deux boutons identiques côte à côte avec deux effets (BUG 11 ; capture `mobile-fiche-serie-hors-liste.png` : deux « + À voir » empilés) |
| Ligne de sonde « S01E01 · Lent (…) : préparer d'avance ? » | Sonde du prochain épisode non vu | OK à l'ouverture ; **une sonde de plus à chaque onglet de saison** (BUG 17) ; contradiction quand l'épisode est déjà « Prêt » (BUG 10, capture `bureau-fiche-serie-ep1-pret.png`) |
| Bouton « À voir à partir d'ici » sur chaque épisode (bureau) | Ajoute la série avec `depuis` = cet épisode | OK ; le bouton de l'épisode retenu devient « ✓ À voir », celui de la série « Retirer » ; cliquer un autre épisode déplace le départ (OK) ; **re-cliquer l'épisode coché retire toute la série** (BUG 18) |
| Même bouton sur téléphone | idem | **Absent** (`FicheSerie.tsx:415`, `!estMobile()`) — BUG 5 |
| Pastille « Vu » + ligne grisée | État vu de l'épisode | OK, lisible |
| Texte « Repris en cours » (épisode entamé) | Dire « en cours, reste N min » | Libellé étrange, pas de barre ni de temps restant (UX 10) |
| Badge « Prêt » sur la ligne d'épisode | Fichier tenu | OK ; sur téléphone il pousse la durée hors de l'écran (BUG 15) |
| En-tête quand un épisode est prêt | Badge sur la série ? | Aucun badge dans l'en-tête (`estPret` ne connaît que les épisodes, `a-voir-serveur.ts:66`) ; l'accueil, lui, badge la carte série (UX 11) |
| En-tête sur téléphone avec « Retirer de « À voir » » | Tenir dans 390 px | Débordement horizontal : méta et sonde coupées (« nécessai… ») — BUG 12 |
| Clic sur un épisode | Lecture | OK ; aucun bouton « Reprendre S01E02 » / « Lire le prochain épisode » dans l'en-tête (UX 12) |
| Marquer un épisode vu / non vu | Geste attendu (Plex, Apple) | N'existe pas (UX 13) |

### 1.3 Résultats de recherche (`ResultatsRecherche.tsx`, `CarteAffiche.tsx`)

| Élément | Action attendue | Observé |
|---|---|---|
| Carte (clic) | Ouvrir la fiche | **Lance la lecture** (`surOuvrir` = `ouvrir`, `etat.ts:172`) — UX 14 |
| « Ajouter à « À voir » » (survol, icône +) | Ajouter | OK à la souris ; icône devient ✓ ; aucun autre retour (UX 1) |
| Même bouton au doigt (téléphone) | Ajouter | **Inaccessible** : le tap lance la lecture (`/lecture/vod:iptva:5003`) — BUG 3 |
| Même bouton au clavier | Ajouter | **Inaccessible** : `tabindex="-1"` (`CarteAffiche.tsx:52`), Tab saute au rail — BUG 4 |
| « Voir les détails » (survol) | Fiche | OK à la souris seulement (BUG 3, 4) |
| Badge « Prêt » sur la carte | Fichier tenu | OK |
| Carte de série | « Ajouter à « À voir » » | OK (série entière) |

### 1.4 Écran « À voir » (`AVoir.tsx`)

| Élément | Action attendue | Observé |
|---|---|---|
| Entrée « À voir » du rail / barre basse | Ouvrir `/a-voir` | OK |
| État vide | Expliquer et guider | Texte correct ; phrase d'en-tête « Le serveur prépare d'avance… État relu à 01:50:00 » affichée même vide (UX 15) |
| En-tête « N éléments · Le serveur prépare d'avance, dans cet ordre… État relu à HH:MM:SS » | Expliquer | Jargon (« relu », secondes) — UX 15 |
| Ligne : numéro, vignette, titre, type, ligne d'état | Lire l'état | « En attente », « En cours · 42 % · 2,1 Mbit/s », « Prêt », « Échec : disque plein » — OK ; « En attente » ne dit pas pourquoi (UX 16) ; « Échec » sans bouton « Réessayer » (UX 17) |
| Ligne série « S01E02 · prêt · suivant en préparation » | Un seul repère | OK ; minuscules/majuscules et « en cours »/« en préparation » incohérents (BUG 9) |
| Sélecteur « Épisodes d'avance » (par défaut, 1…5) | Régler | OK ; « par défaut » ne dit pas combien (UX 18) |
| Sélecteur « À partir de » (l'épisode entamé, S01E01…) | Régler le départ | OK ; l'état se met à jour à la relecture suivante (≤ 10 s) — acceptable |
| Bouton ▶ « Regarder » (film) | Lecture | OK |
| Bouton ▶ « Ouvrir la série » | Fiche | OK, mais même icône ▶ que « Regarder » pour une action qui ne lit rien (UX 19) |
| Clic sur le titre / la vignette | Fiche | OK (pas signalé comme cliquable au clavier) |
| « Monter » / « Descendre » | Réordonner | OK, PUT de l'ordre ; premier/dernier grisés |
| Alt+↑ / Alt+↓ sur la ligne focalisée | Réordonner | OK ; raccourci non annoncé (UX 20) |
| Glisser-déposer (bureau) | Réordonner | OK (`dragTo` 4→1) ; impossible au doigt |
| Entrée sur la ligne focalisée | Ouvrir ? | Ne fait rien (UX 20) |
| « Retirer de la liste » (corbeille) | Retirer | OK, sans confirmation ni « Annuler » (UX 3) |
| Touche Suppr sur la ligne | Retirer | Retire sans confirmation (UX 3) |
| Rafraîchissement 10 s | État à jour | OK (« Prêt » affiché < 1 s après la relecture) |
| Hors ligne | Dire que l'état est périmé | Rien : « État relu à » garde l'heure d'avant, pas de bandeau sur cet écran (UX 21) ; retrait local OK et resynchronisé au retour (OK) |
| Session expirée | Retour à la connexion | OK : écran de connexion affiché en < 12 s |
| Téléphone | Tenir dans 390 px | OK (boutons 44 px, sélecteurs empilés) ; 5 éléments = 2 écrans de haut (UX 22) |

### 1.5 Accueil (`Accueil.tsx`, `CarteAffiche.tsx`, `CarteReprise.tsx`, `MenuContextuel.tsx`)

| Élément | Action attendue | Observé |
|---|---|---|
| Rangée « À voir » | Liste, prêts d'abord | OK ; placée **au-dessus** de « Continuer à regarder » et dans un ordre différent de l'écran « À voir » (UX 23) |
| Compteur « N à préparer » | Compter ce qui reste | **Faux** quand tout est prêt (« 2 à préparer », capture `bureau-accueil-rangee-tout-pret.png`) — BUG 8 |
| « Tout voir » | Écran À voir | OK |
| Légende sous la carte (« En attente », « S01E01 · en cours · 35 % · 2,1 mbit/s · suivant en attente », « Échec : disque plein ») | Dire l'état | Remplace l'année ; tronquée sur la série (« 35 … ») ; « mbit/s » en minuscules (BUG 9) |
| Badge « Prêt » sur la carte | Fichier tenu | OK |
| Survol : « Lecture rapide » | Lire par le lecteur habituel | OK (aucune adresse spéciale) ; inaccessible au doigt et au clavier (BUG 3, 4) |
| Survol : « Lire le prochain épisode » (carte série) | Lire l'épisode suivant | **Ouvre la fiche**, rien ne joue (`etat.ts:172-178`) — BUG 16 |
| Survol : étoile favori | Favori | OK ; mélange des concepts sur la rangée « À voir » ; **aucun « Retirer de « À voir » »** sur ces cartes (UX 24) |
| Survol : « Voir les détails » | Fiche | OK |
| Rangée « Continuer à regarder » | Films et séries entamés | Film entamé : OK (« Reste 0 min » pour < 1 min, BUG 14). Série : disparaît dès que l'épisode entamé est terminé même s'il reste des épisodes (BUG 7, capture `bureau-accueil-apres-enchainement.png`) |
| Carte de reprise (clic) | Reprendre | Film : OK, reprend à la position. Série : **ouvre la fiche** (BUG 16, capture `bureau-accueil-continuer-serie-clic.png`) |
| Menu « … » : « Reprendre » | Reprendre | **Ne fait rien** à la souris et au doigt — BUG 1 |
| Menu « … » : « Marquer comme vu » | Position → fin, carte retirée | **Ne fait rien** — BUG 1 ; et s'il fonctionnait, une série marquée vue disparaîtrait au lieu de passer à l'épisode suivant (BUG 7) |
| Menu « … » : « Retirer de la liste » | Oublier la reprise | **Ne fait rien** — BUG 1 ; libellé identique à celui de l'écran « À voir » pour une autre liste (UX 25) |
| Menu « … » au clavier (Entrée) | Ouvrir le menu | **Lance la lecture** — BUG 2 |
| Rangée « Films et séries favoris » | Favoris | OK ; doublon conceptuel avec « À voir » (UX 9) |
| Même film dans « À voir » (prêt) et « Continuer à regarder » | Une seule place ? | Deux cartes pour le même film (UX 26) |

### 1.6 Lecteur — enchaînement et fin (`Lecture.tsx`, `CalquesLecture.tsx`, `calques.ts`)

| Élément | Action attendue | Observé |
|---|---|---|
| Calque « Épisode suivant dans 10 s · prêt (préparé d'avance) / Série — S01E02 / Lire maintenant / Annuler » | Décompte, démarrage auto | OK (bureau) ; sur téléphone le calque est **coupé en haut** (BUG 13, capture `mobile-lecteur-decompte-episode-suivant.png`) |
| « Lire maintenant » | Lance tout de suite, précédent marqué vu | OK (reprise ép. 1 = durée) |
| « Annuler » / Échap | Arrête le décompte | OK ; **à la vraie fin de l'épisode, plus rien** : lecteur figé à 00:17/00:17, aucun bouton « Épisode suivant » (BUG 6) |
| Épisode suivant hors de la saison affichée | Enchaîner | OK (S01E03 lu dans les saisons) |
| Calque « Film terminé · suivant dans votre liste « À voir » · prêt / Film Test 3 / Regarder / Fermer » | Proposer sans lancer | OK ; pour un film **hors liste**, propose le premier de la liste sous le mot « suivant » (BUG 19, mineur) ; « Ouvrir la série » quand le suivant est une série (OK) |
| Fin d'un film : reprise = durée (vu) | Cohérence | OK ; aucune mention « Vu » ailleurs ensuite (UX 8) |
| Lecture d'un contenu prêt | Lecteur habituel | OK (`GET /api/v1/vod/iptva:5003`) ; **rien dans le lecteur ne dit que la source est le fichier préparé** (`X-Mytv-Source` ignoré ; « MKV · direct » affiché) — UX 27 |
| Réouverture d'un film entamé | « Reprendre / Recommencer » | Ouvre directement au point sauvegardé sans calque quand sous le seuil ; prévu (`politique-reprise.ts:119`) |
| Bouton « À voir » dans le lecteur | Ajouter depuis le lecteur | N'existe pas (seule l'étoile favori y est, `Lecture.tsx:1257`) — UX 28 |
| Télémétrie `a_voir:ajout` / `a_voir:retrait` / `enchainement:suivant` | ids seulement | OK |

### 1.7 Ailleurs

| Écran | Élément | Observé |
|---|---|---|
| Parcours Films / Séries (grilles) | bouton « À voir » sur les cartes | N'existe pas (`CarteAffiche` sans `surAVoir` hors recherche) — UX 29 |
| « À la une » (encart) | « À voir » | N'existe pas ; seulement Regarder / Détails / Bande-annonce / étoile — UX 29 |
| Rail / barre basse | Entrée « À voir » | OK (icône liste) |
| Réglages | Réglage lié (épisodes d'avance par défaut, budget) | Aucun ; le « par défaut » de l'écran À voir n'est réglable nulle part côté UI — UX 18 |

---

## 2. BUGS

Sévérité : **bloquant** (le geste prévu est impossible) · **majeur** (prévu mais faux, ou impossible sur un gabarit majoritaire) · **mineur**.

**BUG 1 — bloquant — Le menu « … » de « Continuer à regarder » ne fait rien (souris et doigt).**
Reproduction : entamer un film (lire 2 s, pause, attendre 5 s, revenir à l'Accueil) ; rangée « Continuer à regarder » → « … » → « Marquer comme vu » (ou « Reprendre », ou « Retirer de la liste »).
Attendu : reprise à la fin (carte retirée) / lecteur ouvert / reprise oubliée.
Obtenu : le menu se ferme, rien ne change (`db.reprises` inchangée : `{position:1, duree:5}`, chemin toujours `/`). Script `07-menu-pointerdown.mjs` : « menu encore ouvert après pointerdown sur l'entrée ? NON : fermé avant le clic ».
Cause : `MenuContextuel.tsx:31` enregistre `window.addEventListener('pointerdown', props.surFermer, { capture: true })` ; l'écouteur en phase de capture sur `window` s'exécute AVANT le `onPointerDown={(e) => e.stopPropagation()}` du menu (`:48`), ferme le menu au `pointerdown`, et le `click` (`:61-64`, qui appelle `e.action()`) n'arrive jamais sur un élément démonté. Le commentaire `:30` (« refermer avant que le clic n'atteigne la carte ») décrit exactement le mécanisme qui tue aussi les entrées du menu. Par ricochet, `02-recherche-accueil.mjs` a relevé deux erreurs de page « Stale read from <Show> » (`Accueil.tsx:595-603`, `entrees` lu après la fermeture).
Captures : `bureau-accueil-menu-contextuel.png`, `bureau-menu-contextuel-ouvert.png`, `bureau-menu-marquer-vu-resultat.png`, `mobile-menu-marquer-vu-resultat.png`.

**BUG 2 — majeur — Au clavier / à la télécommande, Entrée sur « … » lance la lecture au lieu d'ouvrir le menu.**
Reproduction : focaliser le bouton « Plus d'options pour Film Test 12 » (Tab), Entrée.
Attendu : menu ouvert. Obtenu : `/lecture/vod:iptva:5012`, aucun menuitem.
Cause : `CarteReprise.tsx:117-123` ouvre le menu sur `keydown` Entrée mais ne fait pas `stopPropagation` ; l'événement remonte au `div role="button"` parent (`:48-53`) qui appelle `surOuvrir`. Combiné au BUG 1, « Marquer comme vu » et « Retirer » sont donc inaccessibles par tout moyen.
Capture : `bureau-menu-contextuel-clavier.png`.

**BUG 3 — majeur — Sur téléphone, impossible d'ajouter à « À voir » (ni d'ouvrir les détails) depuis une carte : le tap lance la lecture.**
Reproduction (390×844 tactile) : recherche « Film Test 3 », tap sur la carte.
Attendu (CA1 : « dans les résultats de recherche ») : accès au bouton « Ajouter à « À voir » ». Obtenu : navigation vers `/lecture/vod:iptva:5003`, le flux s'ouvre (sur un abonnement à connexion unique, un tap exploratoire consomme la connexion).
Cause : `CarteAffiche.tsx:156-162` : les actions ne sont visibles qu'au survol (`opacity: survol() ? 1 : 0`, `pointer-events` idem) ; sur tactile `mouseenter` et `click` arrivent ensemble ; `ResultatsRecherche.tsx:251` + `etat.ts:172` : `surOuvrir` d'un film = lecture. Même chose pour « Lecture rapide », favori et détails sur les cartes de l'Accueil.
Captures : `mobile-recherche-resultats.png`, `mobile-recherche-apres-tap.png`.

**BUG 4 — majeur — Les actions des cartes sont inatteignables au clavier (télécommande).**
Reproduction : recherche, Tab jusqu'à la carte « Film Test 3 » (le calque d'actions s'affiche au focus), Tab : le focus saute au rail (« Replier la navigation »).
Cause : `CarteAffiche.tsx:52` `tabindex="-1"` sur chaque `BoutonSurvol`. Capture : `bureau-recherche-carte-focus-clavier.png`.

**BUG 5 — majeur — « À voir à partir d'ici » n'existe pas sur téléphone.**
Reproduction : fiche « Série Test 1 » en 390 px : aucune ligne d'épisode n'a de bouton (`01-fiches.mjs` : « aucun bouton par épisode »).
Attendu (CA1) : un épisode ajoute sa série avec `depuis`. Cause : `FicheSerie.tsx:415` `<Show when={!estMobile()}>`. Seul contournement : écran « À voir » → sélecteur « À partir de », que rien n'indique. Capture : `mobile-fiche-serie-sans-bouton-episode.png`.

**BUG 6 — majeur — Décompte annulé puis fin d'épisode : impasse.**
Reproduction : lire S01E01 → enchaînement auto S01E02 ; au décompte vers S01E03, Échap ; attendre la fin de S01E02.
Attendu : à la fin, un bouton « Épisode suivant » reste proposé (Netflix, Disney+, Prime affichent la vignette du suivant jusqu'à ce qu'on choisisse). Obtenu : lecteur figé « 00:17 / 00:17 », aucun calque, aucun bouton ; il faut revenir à la fiche.
Cause : `calques.ts:89-98` : `annulerRebours` mémorise le refus pour la clé, `reboursAlaFin` s'y refuse ; `Lecture.tsx:556-566` n'a aucun repli (le calque « ProchainContenu » n'est posé que pour un film). Capture : `bureau-lecteur-fin-episode-2-apres-annulation.png`.

**BUG 7 — majeur — Une série sort de « Continuer à regarder » dès que l'épisode entamé est terminé, même s'il reste des épisodes ; « Marquer comme vu » (s'il marchait) la ferait disparaître.**
Reproduction : S01E01 et S01E02 vus par enchaînement, S01E03 jamais ouvert → Accueil : la rangée n'existe plus (`04-enchainement.mjs` : « Continuer a regarder : (absente) »).
Attendu : la série reste « à reprendre » sur son prochain épisode (Netflix « Continue Watching », Plex « On Deck », Apple TV « À suivre » passent automatiquement à l'épisode suivant).
Cause : `reprises-accueil.ts:46-47` écarte toute ligne > 0,95 et ne connaît pas de « prochain épisode » ; `Accueil.tsx:431-434` `marquerVu(cleReprise)` pousse la position de l'épisode à la fin, donc même effet. Capture : `bureau-accueil-apres-enchainement.png`.

**BUG 8 — mineur — Compteur « N à préparer » faux.**
Deux films prêts → « 2 à préparer ». `Accueil.tsx:487` compte la liste entière. Capture : `bureau-accueil-rangee-tout-pret.png`.

**BUG 9 — mineur — Libellés d'état incohérents dans le résumé de série.**
« S01E01 · en cours · 35 % · 2,1 mbit/s · suivant en attente » : « mbit/s » en minuscules (`a-voir-regles.ts:108` `toLowerCase()` sur « 2,1 Mbit/s »), « en cours » pour le premier épisode mais « en préparation » (`ETAT_PHRASE`, `:49`) pour le suivant : deux mots pour un même état. Textes relevés dans `02-recherche-accueil.mjs` ; capture `bureau-accueil-rangee-a-voir-legendes.png` (légende tronquée « 35 … »).

**BUG 10 — mineur — La sonde contredit l'état réel.**
(a) Film ajouté depuis la ligne « Lent (…) : préparer d'avance ? » : la question reste, sans bouton (`bureau-fiche-film-lent-apres-ajout.png`). (b) Série en liste dont S01E01 porte le badge « Prêt » : l'en-tête dit encore « S01E01 · Lent (1,5 Mbit/s…) : préparer d'avance ? » (`bureau-fiche-serie-ep1-pret.png`, `mobile-fiche-serie-ep1-pret.png`). Cause : `BoutonAVoir.tsx:118` ne cache que le bouton, pas le texte (`a-voir-regles.ts:201`) ; résultat de sonde gardé 10 min (`disponibilite.ts:16`) sans être invalidé par `estPret`.

**BUG 11 — mineur — Deux boutons « À voir » sur la même fiche, et sur la série ils ne font pas la même chose.**
`FicheFilm.tsx:149` (sonde) et `:153` ; `FicheSerie.tsx:540-547` (sonde, avec `depuis` = prochain épisode) et `:558` (série entière). Sur téléphone ils sont empilés l'un sur l'autre (`mobile-fiche-serie-hors-liste.png`).

**BUG 12 — mineur — Téléphone : l'en-tête de la fiche série déborde quand le bouton dit « Retirer de « À voir » ».**
Méta coupée (« Drame, Thriller · 20 ») et ligne de sonde coupée (« nécessai… ») ; `mobile-fiche-serie-ep1-pret.png` vs `mobile-fiche-serie-hors-liste.png` (même fiche, libellé court : pas de débordement). Cause probable : libellé long insécable dans une colonne `auto 1fr` (`FicheSerie.tsx:498`).

**BUG 13 — mineur — Téléphone : le calque « Épisode suivant » est coupé en haut.**
`CalquesLecture.tsx:155` (`bottom: 84px`, calque de 4 lignes) dans une zone vidéo de ~430 px : la première ligne « Épisode suivant dans 10 s · prêt » sort de la zone. Capture : `mobile-lecteur-decompte-episode-suivant.png`.

**BUG 14 — mineur — « Reste 0 min »** pour moins d'une minute (`Accueil.tsx:177-182`). Capture `bureau-accueil-continuer-film.png`. (Anecdotique sur un vrai film, mais « Reste 0 min » juste avant la fin arrive.)

**BUG 15 — mineur — Téléphone : le badge « Prêt » d'une ligne d'épisode pousse la durée hors de l'écran** (`mobile-fiche-serie-ep1-pret.png` : « Prêt » collé au bord droit, « 40:00 » invisible). `FicheSerie.tsx:407-411`.

**BUG 16 — majeur (expérience) — « Lire le prochain épisode », « Reprendre » et la carte de reprise d'une série ouvrent la fiche ; rien ne joue.**
`CarteAffiche.tsx:170` promet de lire ; `Accueil.tsx:427` « Reprendre » ; `etat.ts:172-178` : une série « passe par sa fiche ». Sur la fiche, l'épisode entamé n'est signalé que par le texte « Repris en cours » (`FicheSerie.tsx:369`). Captures : `bureau-accueil-serie-lire-prochain-episode-resultat.png`, `bureau-accueil-continuer-serie-clic.png`.

**BUG 17 — majeur (fournisseur) — Une sonde part à chaque changement d'onglet de saison.**
Fiche « Série Test 1 » : 1 sonde S01E01 à l'ouverture, puis clic « Saison 2 » → 1 sonde S02E01 (`01-fiches.mjs`). Chaque sonde = trois connexions séquentielles chez un fournisseur qui coupe au-delà d'une (lot 9 CA6) ; le lot prévoyait « à l'ouverture de la fiche seulement ». Cause : `FicheSerie.tsx:258-266` `prochain()` dépend de `saisonActive()` et `disponibilite.ts:33` resonde à chaque nouvel identifiant.

**BUG 18 — mineur — Re-cliquer l'épisode coché « ✓ À voir » retire toute la série de la liste.**
`BoutonAVoir.tsx:40` : `present` → `retirerAVoir(cle)` avec la clé de la série. L'utilisateur voulait au mieux « ne plus partir d'ici ». Relevé dans `01-fiches.mjs` (« la série est RETIRÉE de la liste entière »).

**BUG 19 — mineur — Film hors liste terminé : « suivant dans votre liste » propose le premier élément** (`a-voir-regles.ts:128-131`). Le mot « suivant » suppose que le film était dans la liste. Capture `bureau-lecteur-film-hors-liste-termine.png`.

Lu dans le code, non rejoué (le faux serveur n'applique pas la règle) : un film vu ou une série finie est **retirée de la liste par le serveur au `GET` suivant** (`/opt/mytv/internal/api/a_voir.go:139-169`, règle documentée `docs/openapi.yaml:918`) ; côté interface, `appliquerListeServeur` (`a-voir-serveur.ts:109-113`) efface alors la ligne locale sans message, et toute relecture (écran À voir toutes les 10 s, synchro toutes les 120 s) peut faire disparaître une carte sous les yeux de l'utilisateur. Ce n'est pas un bug du contrat, mais c'est le comportement qui rend la liste « incompréhensible » : rien ne dit *pourquoi* un titre a disparu (voir UX 8).

---

## 3. EXPÉRIENCE

Chaque constat cite la pratique d'une référence (vérifiée par recherche web le 2026-11-04 ; sources en fin de document).

**UX 1 — Aucun retour après un ajout/retrait.** Le bouton change d'état, c'est tout ; sur une carte seul le petit « + » devient « ✓ », invisible dès que la souris bouge. Netflix (web, mobile, TV) et Disney+ affichent une confirmation courte ; Apple TV change le bouton en « Dans À suivre » (« In Up Next ») ; YouTube affiche un toast « Enregistré dans À regarder plus tard » avec « Annuler ». → Un toast de 4 s « Ajouté à Ma liste · le serveur le prépare » / « Retiré de Ma liste · Annuler ».

**UX 2 — Le sens de « À voir » (= préparer d'avance sur le serveur) n'est écrit que dans une info-bulle.** Sur téléphone et télécommande il n'y a pas d'info-bulle. Netflix nomme la chose par son effet (« Télécharger », « Téléchargements intelligents ») ; Prime Video « Téléchargements automatiques » avec le nombre d'épisodes conservés. → Nommer la préparation là où elle se passe (état sur la carte, toast, en-tête de l'écran).

**UX 3 — Retrait sans confirmation ni annulation**, y compris par la touche Suppr. Netflix « Retirer de la rangée » propose « Annuler la suppression » ; Disney+ demande confirmation ; YouTube : toast « Annuler ». → « Annuler » dans le toast suffit, pas de dialogue.

**UX 4 — La sonde parle en Mbit/s.** « Disponible immédiatement », « Lent (2,7 Mbit/s pour 4,4 nécessaires) », silence pendant une lecture. Aucune référence n'affiche de débit à l'utilisateur ; Netflix dit « Qualité de téléchargement » et des tailles en Mo ; Plex signale juste « Transcodage ». → Trois phrases humaines : « Lecture fluide », « Risque de coupures : préparer d'avance ? », « Prêt (préparé sur le serveur) » ; le débit en info secondaire.

**UX 5 — « Prêt » dit deux fois** (badge + ligne de sonde) sur la fiche film.

**UX 6 — Sur la fiche, un film en préparation (attente / 42 % / échec) n'a aucun état visible** ; seul « prêt » se voit. Netflix affiche sur la fiche et dans la liste des épisodes l'icône de téléchargement avec anneau de progression, puis la coche « Téléchargé » ; Prime Video idem (« Téléchargement… 42 % »). → Un seul composant d'état réutilisé partout : fiche, carte, ligne d'épisode, écran liste.

**UX 7 — La fiche d'un film entamé ne le dit pas** (pas de barre, pas de « Reprendre »). Netflix, Disney+, Prime, Apple TV montrent une barre de progression et un bouton « Reprendre » (« Resume ») avec le temps restant sur la fiche. → `Reprendre · reste 42 min` en bouton principal, « Recommencer » en secondaire.

**UX 8 — « Vu » n'existe pas pour un film** et un titre vu disparaît de la liste sans explication. Plex coche ✓ les titres vus et propose « Marquer comme non vu » ; Apple TV retire d'« À suivre » ce qui est fini et le garde dans l'historique ; YouTube garde les vidéos vues dans « À regarder plus tard » avec la barre pleine et propose « Supprimer les vidéos regardées » ; Netflix ne retire un titre de « Ma liste » que s'il quitte le service, si on le retire soi-même ou en voyage (page d'aide « Ma liste ») — un titre vu y reste donc ; ce sont ses téléchargements qui sont effacés. → État « Vu ✓ » sur la fiche, les cartes et les épisodes ; « Regarder à nouveau » ; sortie de la liste annoncée (« Vu, retiré de Ma liste · Annuler »).

**UX 9 — Quatre listes proches : À voir, Favoris (films/séries), Continuer à regarder, (vus).** L'utilisateur voit deux boutons côte à côte sur chaque fiche (« À voir », « Ajouter aux favoris ») et deux rangées (« À voir », « Films et séries favoris ») sans règle qui les distingue. Netflix, Disney+, Prime n'ont qu'UNE liste d'intention (« Ma liste » / « Watchlist ») ; Apple TV une seule (« À suivre ») qui mélange intention et progression ; Plex sépare « Watchlist » (intention) et « Continue Watching » (progression), pas de favoris pour les films. → Garder les favoris pour les **chaînes** (où ils ont un sens : zapping), et UNE liste pour les films et séries.

**UX 10 — « Repris en cours »** sur un épisode entamé : mot inhabituel, sans temps restant ni barre. Netflix/Plex : barre de progression sous la vignette de l'épisode, « 23 min restantes ».

**UX 11 — Le badge « Prêt » d'une série n'est pas au même endroit selon l'écran** : carte de l'accueil oui, en-tête de la fiche non, lignes d'épisodes oui.

**UX 12 — La fiche série n'a pas de bouton principal « Reprendre S01E02 » / « Lire S01E01 ».** Toutes les références en ont un (Netflix « Reprendre la lecture S1:E2 », Apple TV « Lire S1 E2 », Plex « Lire · S1 E2 »). L'utilisateur doit retrouver l'épisode dans la liste.

**UX 13 — Pas de « marquer vu / non vu » sur un épisode ni une saison.** Plex (menu « … » par épisode et par saison), Apple TV (« Marquer comme vu »), Jellyfin : coche sur chaque ligne. Indispensable quand on a déjà vu une saison ailleurs : c'est aussi ce qui pilote la préparation « à partir de ».

**UX 14 — Cliquer une carte de film lance la lecture** (recherche, parcours) au lieu d'ouvrir la fiche, alors que sur « À la une » et « À voir » le clic ouvre la fiche. Netflix, Disney+, Prime, Apple, Plex : le clic/tap sur une affiche ouvre toujours la fiche ; la lecture directe est un bouton ▶ explicite (survol sur Netflix web, « Lecture » sur TV). Avec une connexion unique, lancer un flux par erreur coûte cher.

**UX 15 — En-tête de l'écran À voir** : « Le serveur prépare d'avance, dans cet ordre, ce que vous avez choisi. État relu à 01:50:01. » Vocabulaire technique (« relu », secondes), affiché même quand la liste est vide. Netflix « Téléchargements » : « Téléchargements intelligents : activés · 1,2 Go utilisés » ; Prime : « 3 épisodes conservés ». → « Préparés d'avance sur le serveur, dans cet ordre · 2 prêts · 1 en préparation · 4,2 Go sur 10 ».

**UX 16 — « En attente » ne dit pas pourquoi** (une lecture est en cours ? file ? budget plein ?). Netflix affiche « En attente du Wi-Fi » / « En attente » avec la raison dans le détail ; Prime « En file d'attente ». Le serveur sait la cause (CA3 : pause pendant une lecture) ; l'interface devrait la nommer : « En attente (une lecture est en cours) », « Dans la file (2e) ».

**UX 17 — « Échec : disque plein »** sans « Réessayer » ni conseil (retirer quelque chose). Netflix : « Échec du téléchargement · Réessayer » ; Prime : « Espace insuffisant ».

**UX 18 — « Épisodes d'avance : par défaut »** ne dit pas la valeur ; et « À partir de : l'épisode entamé » est la seule trace du réglage `depuis` — invisible depuis la fiche. Netflix (Téléchargements intelligents) : « Télécharger l'épisode suivant » on/off ; Prime : « Nombre d'épisodes à télécharger : 2 ». → « Préparer 2 épisodes d'avance » (−/+) et « À partir de S01E03 » cliquable.

**UX 19 — Même icône ▶ pour « Regarder » (joue) et « Ouvrir la série » (n'ouvre que la fiche).**

**UX 20 — Raccourcis invisibles** : Alt+↑/↓, Suppr ; Entrée sur une ligne ne fait rien. Pour une télécommande (pas de touche Alt), seuls les boutons « Monter/Descendre » existent : 4 boutons par ligne à parcourir. Netflix TV : menu contextuel sur « OK long » ; Apple TV : « clic long ». → Entrée = ouvrir, « … » = menu (Monter, Descendre, Retirer, Options série).

**UX 21 — Hors ligne, l'écran À voir ne dit rien** ; les états affichés sont périmés sans indication. (Le reste de l'application a un bandeau hors-ligne : il n'apparaît pas sur cet écran dans mes essais.)

**UX 22 — Lignes hautes sur téléphone** (5 éléments = deux écrans) : numéro, vignette, titre, état, deux sélecteurs, quatre boutons. Netflix mobile « Téléchargements » : une ligne par titre (vignette, titre, état, un seul « … »).

**UX 23 — Ordre et position de la rangée « À voir » sur l'accueil.** Au-dessus de « Continuer à regarder » (alors que reprendre est l'action la plus probable, `VignetteReprise.tsx:6`) et ordonnée « prêts d'abord » (différent de l'écran). Netflix place « Reprendre la lecture » en haut puis « Ma liste » ; Apple TV « À suivre » en tête. → « Reprendre » d'abord, puis « Ma liste » dans l'ordre de la liste, le badge « Prêt » suffisant à distinguer.

**UX 24 — Les cartes de la rangée « À voir » n'ont pas de « Retirer »** mais ont une étoile favori. Netflix « Ma liste » : au survol, « Retirer de Ma liste » (coche) ; Disney+ : « … » → Retirer.

**UX 25 — « Retirer de la liste »** apparaît dans deux menus pour deux listes différentes (reprise / À voir). Netflix : « Retirer de la rangée » (Continue Watching) vs « Retirer de Ma liste ».

**UX 26 — Un même film prêt et entamé apparaît deux fois** (À voir + Continuer à regarder) avec deux légendes (« Prêt » / « Reste 42 min »). Apple TV fusionne les deux dans « À suivre » (un titre entamé y est, avec sa barre) ; Netflix garde Ma liste et Reprendre séparées mais la carte « Reprendre » porte la progression et l'icône téléchargé.

**UX 27 — Le lecteur ne dit jamais qu'il lit un fichier préparé** (« MKV · direct » alors que l'en-tête `X-Mytv-Source: prepare` est reçu). C'est pourtant la preuve de l'intérêt de la liste. Netflix affiche l'icône « Téléchargé » dans le lecteur hors ligne ; Plex affiche « Lecture directe / Transcodage ». → « Depuis le serveur (préparé) » dans le panneau infos, petit badge « Prêt » près du titre.

**UX 28 — Pas de bouton « À voir » dans le lecteur** (seule l'étoile favori y est) : c'est pourtant pendant une lecture lente qu'on décide de préparer la suite.

**UX 29 — Le bouton « À voir » manque sur les grilles Films/Séries et sur « À la une »** ; CA1 ne le prévoyait pas, mais l'utilisateur découvre un bouton qui existe sur la recherche et pas sur le parcours. Netflix/Disney+ : « + » disponible partout où une affiche apparaît (survol ou fiche).

---

## 4. SYNTHÈSE (coordinateur)

1. Le menu « … » de « Continuer à regarder » est mort : Reprendre / Marquer comme vu / Retirer ne font rien à la souris, au doigt, et Entrée au clavier lance la lecture (BUG 1, 2) — l'utilisateur en conclut que « les vus ne marchent pas ».
2. Sur téléphone (usage majoritaire), « À voir » n'est atteignable que sur la fiche : les cartes lancent la lecture au tap (BUG 3) et « À voir à partir d'ici » n'existe pas (BUG 5) ; au clavier/télécommande, toutes les actions de cartes sont hors d'atteinte (BUG 4).
3. Les séries sont mal servies par le modèle de reprise : une série dont l'épisode entamé est fini sort de « Continuer à regarder » (BUG 7), « Reprendre »/« Lire le prochain épisode » ouvrent la fiche (BUG 16), un décompte annulé laisse le lecteur en impasse (BUG 6).
4. L'état de préparation n'est lisible que sur l'écran À voir ; sur la fiche, un film en attente / 42 % / échec ne dit rien, et un film vu sort de la liste sans message ni état « Vu » (UX 6, 8). Les sondes contredisent l'état réel et repartent à chaque onglet de saison (BUG 10, 17).
5. Quatre concepts (À voir, Favoris, Continuer, vu) avec des libellés qui se recouvrent (« Retirer de la liste » ×2, deux boutons « À voir » par fiche, « N à préparer » faux, « mbit/s ») : le §5 propose un modèle à trois états et une seule liste, aligné sur Netflix/Plex/Apple TV.

---

## 5. MODÈLE CIBLE

### 5.1 Trois choses, trois noms

| Concept | Nom affiché | Nature | Qui y entre | Qui en sort | Référence |
|---|---|---|---|---|---|
| Intention + préparation | **Ma liste** | liste ordonnée, manuelle, par profil ; chaque élément porte un état de préparation | geste « + Ma liste » (fiche, carte, lecteur, recherche, parcours) ; un épisode = sa série « à partir de » | geste « Retirer » ; automatiquement quand le film est vu ou la série finie — **avec toast et « Annuler »** | Netflix « Ma liste » + « Téléchargements intelligents » ; Plex « Watchlist » ; Prime « Watchlist » + « Téléchargements automatiques » |
| Progression | **Reprendre** (rangée) | automatique, jamais éditée à la main sauf « Retirer » | tout film entamé ; toute série ayant un épisode entamé **ou un prochain épisode non vu après un épisode vu** | film vu ; série sans épisode suivant ; « Retirer » (position conservée) ; « Marquer comme vu » fait avancer une série, pas disparaître | Netflix « Reprendre la lecture », Plex « Continue Watching / On Deck », Apple TV « À suivre » |
| État d'un contenu | **Vu ✓** / barre de progression | attribut, pas une liste | part ≥ 0,95, ou « Marquer comme vu » | « Marquer comme non vu », « Regarder à nouveau » n'efface pas le ✓ | Plex, Apple TV, YouTube |

Les **favoris** restent réservés aux **chaînes** (zapping) ; l'étoile disparaît des fiches de films et séries et la rangée « Films et séries favoris » est remplacée par « Ma liste » (les favoris films/séries existants sont versés dans Ma liste, en fin de liste).

**Préparation** = un état porté par chaque élément de Ma liste, jamais une liste à part : `En attente` → `En préparation 42 %` → `Prêt` ou `Échec`. Il s'affiche **au même endroit et sous la même forme** sur la fiche, la carte, la ligne d'épisode, l'écran Ma liste et le lecteur.

### 5.2 Règles

1. **Un tap/clic sur une affiche ouvre toujours la fiche.** La lecture est un bouton ▶ explicite. (Netflix, Disney+, Prime, Apple, Plex.)
2. **Chaque carte a un bouton « … » visible** (pas seulement au survol) sur tactile et au clavier (`tabindex=0`, Entrée/« OK long »), qui ouvre le menu de la carte.
3. **Ma liste, entrée** : « + Ma liste » devient « ✓ Dans ma liste » ; toast 4 s « Ajouté à Ma liste · préparation d'avance sur le serveur ». Un épisode : menu « … » → « Ma liste à partir d'ici (S01E03) » ; re-cliquer l'épisode coché = « Ne plus partir d'ici » (pas un retrait de la série).
4. **Ma liste, sortie** : « Retirer de Ma liste » (bouton, menu, Suppr) → toast « Retiré de Ma liste · Annuler » (6 s). Sortie automatique (film vu, série finie) → toast au moment où l'interface l'apprend : « Vu · retiré de Ma liste · Annuler » ; jamais de disparition silencieuse.
5. **Reprendre** : la carte d'une série pointe sur **l'épisode à lire** (entamé, sinon le prochain non vu) et le lance directement ; sa légende dit « S01E03 · reste 23 min » ou « S01E03 · épisode suivant ». « Marquer comme vu » sur une série = l'épisode courant ✓, la carte passe au suivant. « Retirer » (Netflix « Retirer de la rangée ») cache la série sans toucher aux positions.
6. **Vu** : ✓ sur la fiche film (bouton principal « Regarder à nouveau »), sur chaque ligne d'épisode, sur les cartes (coin, discret) ; « Marquer comme vu / non vu » dans le menu « … » de tout film, épisode et saison.
7. **Préparation visible partout** : chip d'état sur la fiche, la carte et la ligne d'épisode. « En attente » porte sa raison (« une lecture est en cours », « 2e dans la file », « disque plein »). « Échec » porte « Réessayer ». « Prêt » reste tant que le fichier existe ; dans le lecteur, « Lecture depuis le serveur · préparé ».
8. **La sonde ne parle plus en Mbit/s** à la première ligne : « Lecture fluide » / « Risque de coupures — préparer d'avance ? [+ Ma liste] » / « Prêt ». Débit en info-bulle ou panneau infos. Elle part **une fois par fiche** (pas par onglet de saison) et est invalidée quand l'état « Prêt » arrive.
9. **Fin d'épisode** : décompte 10 s annulable ; après annulation, à la vraie fin, le calque reste affiché **sans décompte** : « Épisode suivant · S01E03 [▶ Lire] [Revenir à la série] ». Fin de film : « Film terminé ✓ · Prochain dans Ma liste : Titre · Prêt [▶ Regarder] [Fermer] » ; film hors liste : « Prochain dans Ma liste » sans « suivant ».
10. **Hors ligne** : bandeau « Hors ligne — états de préparation du HH:MM » sur l'écran Ma liste ; les gestes restent possibles.

### 5.3 Où vit chaque bouton

| Emplacement | Boutons |
|---|---|
| Fiche film | `▶ Regarder` / `▶ Reprendre · reste 42 min` / `↻ Regarder à nouveau` (principal, selon l'état) · `+ Ma liste` / `✓ Dans ma liste` · `…` (Marquer comme vu/non vu, Recommencer, Bande-annonce, Diffuser) · chip d'état sous le titre |
| Fiche série (en-tête) | `▶ Lire S01E01` / `▶ Reprendre S01E03 · reste 23 min` · `+ Ma liste` / `✓ Dans ma liste · à partir de S01E03` (cliquable → change le départ) · `…` (Marquer la saison vue, Préparer N épisodes d'avance) |
| Ligne d'épisode | `▶` (toute la ligne) · chip ✓ Vu / barre / Prêt · `…` : Ma liste à partir d'ici · Marquer vu / non vu |
| Carte (toutes rangées, recherche, grilles) | tap = fiche · badge coin : `Prêt` ou `✓` · bouton `…` toujours visible (36/44 px) : Lecture / Ma liste (+ ou Retirer) / Détails ; survol bureau : `▶` et `+` en plus |
| Carte Reprendre | tap = **lecture** de l'épisode/film · `…` : Reprendre · Marquer comme vu · Retirer de Reprendre |
| Écran Ma liste (ligne) | titre (fiche) · chip d'état · `▶ Regarder` / `▶ Lire S01E03` · `…` : Monter · Descendre · Retirer · (série) Préparer N épisodes d'avance · À partir de… ; glisser = poignée `⋮⋮` |
| Lecteur | `+ Ma liste` dans le pied (à côté de l'étoile si on la garde pour les chaînes) · « Préparé sur le serveur » dans le panneau infos |

### 5.4 Maquettes

**Fiche film — trois états (bureau, 1360 px)**

```
┌─ ← Retour   Film ─────────────────────────────────────────────────────────────┐
│ ┌───────┐  Film Test 1                                                       │
│ │       │  2019 · 1 h 40 · ★ 7,8 · Drame, Thriller · 12+                     │
│ │ affi- │  Synopsis…                                                         │
│ │ che   │                                                                    │
│ │       │  ⚠ Risque de coupures (2,7 Mbit/s mesurés, 4,4 nécessaires)       │
│ └───────┘  [▶ Regarder]  [+ Ma liste]  [… ]                                  │
└───────────────────────────────────────────────────────────────────────────────┘
   état A : hors liste, sonde « lent »   (un seul bouton « + Ma liste »)

│ ┌───────┐  Film Test 1   ⟳ En préparation · 42 % · prêt dans ~6 min          │
│ │       │  2019 · 1 h 40 · ★ 7,8                                             │
│ └───────┘  [▶ Regarder]  [✓ Dans ma liste]  [… ]                             │
   état B : dans la liste, en préparation   (chip d'état sous le titre, pas de sonde)

│ ┌───────┐  Film Test 1   ● Prêt · préparé sur le serveur                     │
│ │  ▮▮▮▮▮▮▮▮▮▮░░░░░  reste 42 min                                            │
│ └───────┘  [▶ Reprendre · 42 min]  [Recommencer]  [✓ Dans ma liste]  [… ]    │
   état C : prêt et entamé

│ ┌───────┐  Film Test 1   ✓ Vu le 3 nov.                                      │
│ └───────┘  [↻ Regarder à nouveau]  [+ Ma liste]  [… Marquer comme non vu]    │
   état D : vu (sorti de Ma liste, toast l'a dit au moment de la sortie)
```

**Fiche série — en-tête et lignes (téléphone, 390 px)**

```
┌──────────────────────────────────────┐
│ ← Retour   Série                     │
│ ┌────┐ Série Test 1                  │
│ │    │ 2 saisons · 6 épisodes · 2019 │
│ │    │ ● Prêt jusqu'à S01E02         │
│ └────┘                               │
│ [▶ Reprendre S01E02 · reste 23 min]  │
│ [✓ Dans ma liste · dès S01E02 ▾] [… ]│
│──────────────────────────────────────│
│ Saison 1 (3)   Saison 2 (3)          │
│──────────────────────────────────────│
│ ▣ E01  Épisode 1            ✓ Vu    …│
│ ▣ E02  Épisode 2   ▮▮▮▮░░ 23 min  ●…│   ● = Prêt (chip), … = menu de la ligne
│ ▣ E03  Épisode 3          40:00   ⟳ …│   ⟳ = En préparation
│ ▣ E04  Épisode 4          40:00     …│
└──────────────────────────────────────┘
   menu « … » d'une ligne :  ▶ Lire · ✓ Marquer comme vu · Ma liste à partir d'ici
```

**Carte d'affiche (toutes rangées) — bureau au survol / téléphone au repos**

```
 bureau, survol                      téléphone, repos
┌──────────────┐                    ┌──────────────┐
│Prêt       ✓  │  ← badges coin     │Prêt       ✓  │
│              │                    │              │
│   [ ▶ ] [+]  │  ← actions survol  │              │
│              │                    │          [⋯] │  ← toujours visible, 44 px
│ ▮▮▮▮▮░░░░░░░ │  ← progression     │ ▮▮▮▮▮░░░░░░░ │
└──────────────┘                    └──────────────┘
 Film Test 1                         Film Test 1
 2019 · Drame                        2019 · Drame
   tap/clic sur l'affiche = fiche ; menu ⋯ : ▶ Lecture · + Ma liste / Retirer de Ma liste · Détails
```

**Carte « Reprendre » (16:9)**

```
┌────────────────────────────┐
│ ● Prêt               [⋯]   │   menu ⋯ : ▶ Reprendre · ✓ Marquer comme vu · ✕ Retirer de Reprendre
│                            │
│  Série Test 2              │
│  S01E03 · épisode suivant  │   ou « S01E02 · reste 23 min »
│ ▮▮▮▮▮▮▮▮░░░░░░░░░░░░░░░░░ │
└────────────────────────────┘
   tap = lecture de S01E03 (jamais la fiche)
```

**Accueil — ordre des rangées**

```
 À la une
 Reprendre                3 en cours          (cartes 16:9)
 Ma liste                 5 · 2 prêts   Tout voir   (ordre de la liste ; badge « Prêt » distingue)
   ┌Prêt┐ ┌⟳ 42 %┐ ┌En attente┐ ┌✕ Échec┐ ┌Prêt S01E02┐
   Film 1  Film 3   Série 2     Film 6    Série 3
   2019    2003     2 saisons   2006      2012
 Chaînes récentes · Chaînes favorites · En direct maintenant · Films récents · Séries récentes · Parce que…
```
Légende de carte : **l'année/genre reste**, l'état de préparation est un **badge de coin** (`Prêt`, `⟳ 42 %`, `En attente`, `✕ Échec`), pas une phrase qui remplace la légende.

**Écran « Ma liste » (bureau)**

```
┌─ Ma liste  5 titres · 2 prêts · 1 en préparation · 4,2 Go sur 10 Go ───────────────────────┐
│  Préparés d'avance sur le serveur, dans cet ordre. Mis à jour il y a 8 s.                   │
│                                                                                             │
│ ⋮⋮ 1 ┌──┐ Film Test 3        Film · 2003          ○ En attente · une lecture est en cours   │
│      └──┘                                          [▶ Regarder]  [⋯]                        │
│ ⋮⋮ 2 ┌──┐ Film Test 1        Film · 2001          ⟳ En préparation · 42 % · ~6 min         │
│      └──┘                                          [▶ Regarder]  [⋯]                        │
│ ⋮⋮ 3 ┌──┐ Série Test 2       Série · 2 saisons    ● S01E02 prêt · S01E03 en préparation     │
│      └──┘ dès S01E02 · 2 épisodes d'avance         [▶ Lire S01E02]  [⋯]                     │
│ ⋮⋮ 4 ┌──┐ Film Test 6        Film · 2006          ✕ Échec · disque plein   [Réessayer]      │
│      └──┘                                          [▶ Regarder]  [⋯]                        │
│ ⋮⋮ 5 ┌──┐ Série Test 3       Série · 2 saisons    ✕ S01E01 échec · fournisseur injoignable  │
│      └──┘ 3 épisodes d'avance                      [▶ Lire S01E01]  [⋯]  [Réessayer]        │
└─────────────────────────────────────────────────────────────────────────────────────────────┘
   ⋯ = Monter · Descendre · Retirer de Ma liste · (série) Préparer 2 épisodes d'avance [−][+] · À partir de…
   clavier/télécommande : ↑↓ ligne, Entrée = fiche, OK long / Menu = ⋯ ; Alt+↑↓ et Suppr restent, annoncés dans ⋯
   vide : « Votre liste est vide. Ajoutez un film ou une série avec “+ Ma liste” : le serveur le préparera d'avance pour une lecture sans coupure. »
   hors ligne : bandeau « Hors ligne — états du 14:02 ; vos changements partiront au retour du réseau. »
```

**Écran « Ma liste » (téléphone, une ligne)**

```
┌──────────────────────────────────────┐
│ Ma liste   5 · 2 prêts · 4,2/10 Go   │
│──────────────────────────────────────│
│ ┌──┐ Film Test 3                 [⋯] │
│ └──┘ ○ En attente · lecture en cours │
│ ┌──┐ Film Test 1                 [⋯] │
│ └──┘ ⟳ 42 % · ~6 min                 │
│ ┌──┐ Série Test 2                [⋯] │
│ └──┘ ● S01E02 prêt · suivant ⟳       │
│ ┌──┐ Film Test 6                 [⋯] │
│ └──┘ ✕ Échec · disque plein          │
└──────────────────────────────────────┘
   tap ligne = fiche ; ⋯ = Regarder · Monter · Descendre · Retirer · Réessayer · Options de série
```

**Fin d'épisode / fin de film (lecteur)**

```
 pendant le décompte                         après « Annuler », à la vraie fin
┌──────────────────────────────┐            ┌──────────────────────────────┐
│ Épisode suivant dans 7 s     │            │ Épisode suivant              │
│ ● Prêt — démarre sans attente│            │ Série Test 2 — S01E03        │
│ Série Test 2 — S01E03        │            │ [▶ Lire]  [Revenir à la série]│
│ [▶ Lire maintenant] [Annuler]│            └──────────────────────────────┘
└──────────────────────────────┘
 fin de film
┌──────────────────────────────────────┐
│ Film terminé ✓                       │
│ Prochain dans Ma liste · ● Prêt      │
│ Film Test 3 (2003)                   │
│ [▶ Regarder]  [Fermer]               │
└──────────────────────────────────────┘
   téléphone : calque ancré en bas de la zone vidéo, hauteur ≤ 40 % de la zone, jamais coupé
```

**Toasts (4 s, « Annuler » 6 s)**

```
 [✓ Ajouté à Ma liste · le serveur le prépare d'avance]
 [Retiré de Ma liste                           Annuler]
 [✓ Vu · retiré de Ma liste                    Annuler]
 [✓ S01E02 marqué vu · Reprendre passe à S01E03 Annuler]
 [Retiré de Reprendre                          Annuler]
```

### 5.5 Libellés exacts

| Contexte | Texte |
|---|---|
| Liste | **Ma liste** (rail, rangée, écran, toasts) |
| Bouton ajout | `+ Ma liste` · aria : « Ajouter à Ma liste » · info-bulle : « Préparé d'avance sur le serveur pour une lecture sans coupure » |
| Bouton présent | `✓ Dans ma liste` · aria : « Retirer de Ma liste » |
| Série depuis un épisode | menu ligne : `Ma liste à partir d'ici` ; en-tête : `✓ Dans ma liste · dès S01E02` ; épisode coché : `Point de départ · Ne plus partir d'ici` |
| Retrait | `Retirer de Ma liste` ; toast `Retiré de Ma liste · Annuler` |
| États de préparation | `○ En attente` + raison (`une lecture est en cours` / `2e dans la file` / `disque plein`) · `⟳ En préparation · 42 % · ~6 min` · `● Prêt` · `✕ Échec · disque plein` + `Réessayer` |
| Série (une ligne) | `● S01E02 prêt · S01E03 en préparation` · `○ S02E01 en attente · suivant en attente` |
| Options série | `Préparer 2 épisodes d'avance` (−/+, 1 à 5 ; « 2 (réglage du serveur) » quand non modifié) · `À partir de : l'épisode entamé ▾` |
| Compteurs | rangée : `5 · 2 prêts` ; écran : `5 titres · 2 prêts · 1 en préparation · 4,2 Go sur 10 Go` ; vide : `Votre liste est vide` |
| Sonde | `Lecture fluide` · `Risque de coupures — préparer d'avance ?` + `[+ Ma liste]` · `● Prêt · préparé sur le serveur` ; détail : `2,7 Mbit/s mesurés pour 4,4 nécessaires` |
| Reprise | rangée `Reprendre` ; carte `S01E02 · reste 23 min` / `S01E03 · épisode suivant` / `reste 42 min` ; menu : `Reprendre`, `Marquer comme vu`, `Retirer de Reprendre` |
| Vu | chip `✓ Vu` ; bouton `Regarder à nouveau` ; menu `Marquer comme vu` / `Marquer comme non vu` / `Marquer la saison comme vue` |
| Fiche film, principal | `Regarder` · `Reprendre · 42 min` · `Regarder à nouveau` ; secondaire `Recommencer` |
| Fiche série, principal | `Lire S01E01` · `Reprendre S01E02 · 23 min` · `Épisode suivant S01E03` |
| Lecteur | décompte : `Épisode suivant dans 10 s` + `● Prêt — démarre sans attente` ; boutons `Lire maintenant` / `Annuler` ; après annulation : `Épisode suivant` `Lire` / `Revenir à la série` ; film : `Film terminé ✓` `Prochain dans Ma liste · ● Prêt` `Regarder` / `Fermer` ; panneau infos : `Source : serveur (préparé d'avance)` |
| Hors ligne | `Hors ligne — états du 14:02 ; vos changements partiront au retour du réseau.` |
| Favoris | réservés aux chaînes : `☆ Chaîne favorite` / `★ Retirer des favorites` |

### 5.6 Ce que cela règle, bug par bug

BUG 1-2 : un seul composant de menu, ouvert au `click` (pas au `pointerdown`), accessible au clavier, utilisé par toutes les cartes. BUG 3-4 : bouton « ⋯ » visible et focalisable, tap = fiche. BUG 5, 18 : « Ma liste à partir d'ici » dans le menu de ligne, sur tous les gabarits ; recliquer ne retire pas la série. BUG 6 : calque persistant sans décompte après annulation. BUG 7, 16 : « Reprendre » pointe l'épisode à lire ; « Marquer comme vu » avance. BUG 8-9 : compteurs et libellés uniques (§5.5). BUG 10-11, 17 : une sonde par fiche, invalidée par « Prêt », un seul bouton. BUG 12-13, 15 : libellés courts (`✓ Dans ma liste`), chip compact, calque borné. UX 6-8 : chip d'état et ✓ Vu partout ; sortie annoncée par toast. UX 9 : une seule liste pour films et séries.

---

### Sources consultées (pratiques des références)

- Netflix, « Comment supprimer des titres de la rangée Reprendre la lecture » : https://help.netflix.com/fr-ca/node/115312 ; annonce « Remove from row » (avec « Undo ») : https://about.netflix.com/news/now-you-can-easily-tidy-up-your-continue-watching-row-on-all-devices
- Netflix, « Téléchargements intelligents » (efface l'épisode vu, télécharge le suivant ; pas pour les films) : https://help.netflix.com/fr-ca/node/122916 ; « Télécharger l'épisode suivant » : https://help.netflix.com/fr-ca/node/101262 ; « Ma liste » : https://help.netflix.com/fr-ca/node/10523
- Plex, Watchlist retirée à la lecture (synchro d'état « vu ») : https://forums.plex.tv/t/stop-plex-from-automatically-removing-played-items-from-watchlist/804704 ; « Marquer vu » et Continue Watching / On Deck : https://forums.plex.tv/t/manually-marking-episode-watched-removes-item-from-continue-watching/900368
- Apple TV, « À suivre » (Up Next : ajout automatique, passage à l'épisode suivant, retrait des titres finis, bouton « Dans À suivre ») : https://support.apple.com/en-gb/guide/tvapp-mac/watch-now-atv7bf64f99/1.0/mac ; retrait : https://iphonefaq.org/node/7404
- Prime Video, « Téléchargements automatiques » (prochain épisode non vu, nombre d'épisodes conservés, suppression des vus) : https://digitaltveurope.com/2020/09/10/amazon-prime-video-enables-auto-download-feature ; https://androidauthority.com/how-to-download-amazon-prime-movies-shows-1004580
- Disney+, « Retirer de Reprendre » (confirmation, progression conservée) : https://www.disneyplus.com/explore/articles/remove-from-continue-watching
- YouTube, « À regarder plus tard » et « Supprimer les vidéos regardées » : https://www.makeuseof.com/youtube-remove-watched-videos-watch-later-playlist/
