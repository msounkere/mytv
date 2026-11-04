# Audit D — Responsive et navigateurs (mytv-ui, dist du 4 nov. 2026)

Méthode : balayage automatisé Playwright 1.64 (`/opt/mytv-work/audit/D/balayage.mjs`) de 25 écrans/états × 13 gabarits
(320×568, 375×667, 390×844, 414×896, 844×390 paysage, 768×1024, 1024×768 paysage, 1280×800, 1360×860, 1920×1080,
390 et 1360 au zoom CSS 125 %) × 3 moteurs (Chromium 156, Firefox 157, WebKit 27.2 = moteur de tous les navigateurs iPhone),
contre le faux serveur `tests/e2e/serveur-factice.mjs` (aucun hôte autre que 127.0.0.1 ; 0 requête externe sur tout le balayage).
Mesures par écran (`page.evaluate`) : `scrollWidth > clientWidth`, éléments visibles hors fenêtre non coupés par un parent
défilant, cibles < 44×44 px (tactile) ou < 24 px (souris), chevauchements de cibles, texte qui déborde de sa boîte sans
troncature, barres fixes, zones défilantes, police calculée, focus clavier après Tab, API du lecteur (fullscreen, PiP, Remote
Playback, AirPlay, wake lock, orientation), erreurs `console`/`pageerror`. Parcours clés comparés entre moteurs
(`parcours.mjs`) : connexion → profil → accueil → fiche → lecture factice → retour (mini-lecteur) → direct + zapping →
recherche → À voir → réglages → clavier → hors-ligne → IndexedDB en panne. Reproduction ciblée du bug bloquant (`zombie.mjs`).
Captures : `/opt/mytv-work/audit/D/captures/<ecran>-<largeur>-<navigateur>.png` (≈ 900 fichiers) ; mesures brutes :
`resultats-<navigateur>.json` (chromium : `resultats-chromium-complet.json`), `parcours-<navigateur>.json`, résumés `resume-*.txt`.
Couverture WebKit : 6 gabarits (320, 375, 390, 414, 844×390, 768 ; le moteur, lent sous Playwright, a été relancé deux fois) +
parcours 390/1360 + reproductions ciblées ; les écarts WebKit relevés sont des API et un plantage, pas du rendu.
Limite : le gabarit « texte 125 % » (`390t125`) est un artefact (mon émulation cumulait l'échelle) — il est ignoré ; seul le
zoom de page 125 % (`z125`) fait foi. Le faux serveur n'a ni HLS ni EPG : la lecture Direct aboutit à une erreur 404 (état
d'erreur réel du lecteur, exploité tel quel) ; la lecture film est un MP3 de 6 s dans `<video>`.

## 1. Tableau écran × largeur × navigateur → observé

Légende : OK = rien à signaler ; Dn = bug n° n (§2) ; Ex = constat d'expérience (§3). Sauf mention, le rendu est
identique sur les trois moteurs (même DOM, mêmes styles inline, polices système) ; la colonne « moteurs » note les écarts.

| Écran | 320 | 375 / 390 / 414 | 844×390 (tél. paysage) | 768 / 1024×768 (tablette) | 1280 / 1360 / 1920 | 390 zoom 125 % | Moteurs |
|---|---|---|---|---|---|---|---|
| Connexion | OK (champs 34 px haut, D21) | OK | OK | OK | OK | OK | identiques |
| Choix du profil | OK | OK | OK | OK | OK | OK | identiques |
| Accueil | D10 (onglets collés) | OK ; points du carrousel 8×8 px (D21) | rail compressé 55×41 (D22) | flèches 28×28, boutons 36 px (D21) | OK | D10 | Firefox 1360 : une fois « Rien à mettre à la une » après l'import (D18) |
| Menu contextuel « … » (reprise) | OK | OK (44 px/ligne) | OK | OK | OK | OK (largeur fixe 260, D11) | identiques |
| Menu profil (feuille) | OK | OK | panneau ancré en-tête | OK | OK | OK | identiques |
| Direct (liste) | D5 | **D5** titre/compteur écrasés | **D5** nom de chaîne masqué par le badge | **D5** (768 : chevauchement badge/nom ; 1024 : « Chaîn… ») | OK | D5 | identiques |
| Direct : panneau catégories | OK | OK | n/a (colonne) | n/a | n/a | OK | identiques |
| Guide | D12 | D12 (« mercre », stats tronquées) ; bandeau d'erreur EPG D13 | OK | OK | OK | D12 | identiques |
| Films / Séries (grilles) | 3 colonnes, légende « 2000 · … » (E6) | OK | OK | OK (puces 28 px, D21) | OK | OK | identiques |
| Feuille de filtres | OK | OK | n/a | n/a | n/a | OK | identiques |
| Recherche (résultats) | onglets 40 px (D21) | idem | OK | OK | OK | OK | identiques |
| Suggestions de recherche | OK | OK (ligne « Voir les 13 résultats » 41 px) | OK | OK | OK | OK | Entrée ouvre la 1re suggestion au lieu des résultats (E7) |
| Fiche film | OK | OK | OK | OK | OK | OK | identiques |
| Fiche série | **D6** | **D6** (métadonnées, synopsis coupés à droite) | OK | OK | OK | **D6** aggravé (bouton « Retirer » hors écran) | identiques |
| À voir | selects 28 px, libellés sur 3 lignes (D21) | OK | OK | boutons 34 px (D21) | OK | OK | identiques |
| Réglages | boutons 30–36 px (D21) | idem | OK | idem | OK | OK | identiques |
| Lecture film (commandes) | **D2** (Plein écran hors écran), D8 | D8 (vidéo 219 px, 2 flèches retour) | D8 (vidéo 218 px en paysage), **D15** | 2 flèches retour (D8) | OK | **D2** | WebKit : **D4** Plein écran inerte ; Firefox : `?cap=hevc` (D23) |
| Lecture film : plein écran | D2 | OK (Chromium/Firefox) | OK | OK | OK | D2 | **WebKit : rien ne se passe (D4)** |
| Panneau Infos | contenu dans 219 px, défile (E3) | idem | OK | OK | OK | idem | identiques |
| Dialogue Diffuser | coupé, défile (E3) | idem | OK | OK | OK | idem | WebKit (headless) : « indisponible » ; Chromium : Remote Playback |
| Panneau Lecteur (qualité) | feuille OK | OK | ancré | OK | OK | OK | identiques |
| Mini-lecteur (hors du lecteur) | barre 48 px au-dessus des onglets | idem (D20) | coin bas droit | coin bas droit | coin bas droit | idem | identiques |
| Lecture direct + erreur | **D2** (Diffuser/Arrêter/Plein écran hors écran), **D3** | **D2** (Plein écran hors écran, Arrêter coupé), **D3** | **D3** (Réessayer/Retour sous la barre) | **D3** | OK | D2, D3 | identiques |
| Zapping après échec HLS | OK (Chromium/Firefox) | OK | OK | OK | OK | OK | **WebKit : plantage de la page 6/6 gabarits + 2/2 parcours (D16)** |
| Mini-guide | OK (3 lignes visibles) | OK | OK | OK | OK | OK | identiques |
| Variantes | bouton Fermer 24×30 (D21) | idem ; **D7** Échap quitte le lecteur | **D7** | **D7** | **D7** | idem | identiques (D7 partout) |
| Zapping (↓, ↑, liste) | OK | OK | OK | OK | OK | OK | identiques |
| Après « Arrêter le flux » | **D1** | **D1** | **D1** | **D1** | **D1** | **D1** | **D1 sur les 3 moteurs** |
| Clavier (Tab) | focus visible partout sauf le champ de recherche (D9) | D9 | D9 | D9 | D9 | D9 | identiques |
| IndexedDB indisponible | **D14** | D14 | D14 | D14 | D14 | D14 | identiques (message brut) |

Aucun débordement horizontal du document (`scrollWidth > clientWidth`) sur aucun écran ni moteur : la coque est `position:fixed`
et chaque écran coupe ses débordements — ce qui explique aussi pourquoi les textes de D5/D6 sont *coupés* plutôt que la page
qui défile. Police : `Segoe UI Variable` → repli `system-ui` identique sur les trois moteurs ; aucune police web n'est
téléchargée (la police d'icônes `polices/material-symbols-rounded.woff2` reste déclarée mais n'est jamais demandée).

## 2. Bugs

### D1 — BLOQUANT — « Arrêter le flux » laisse le lecteur peint par-dessus tous les écrans (3 moteurs, 13 gabarits)
Reproduction : ouvrir un film (`/lecture/vod:iptva:5001`), attendre l'image → bouton ✕ « Arrêter le flux » (ou : Retour, puis
dans le mini-lecteur « Revenir au lecteur », puis ✕) → l'URL passe à `/films` mais la couche du lecteur reste visible
(`.couche-lecteur` : `visibility: visible`, `transform: none`, `<video>` toujours attaché avec sa source) → naviguer vers
À voir, Réglages, Direct : le lecteur recouvre le contenu ; seul un rechargement complet l'enlève. Obtenu dans chromium,
firefox, webkit (`zombie.mjs`, sortie « croix ferme : couche visible=visible … url=/films »). Attendu : la couche disparaît,
le flux est libéré. À chaque occurrence, une erreur non interceptée `Stale read from <Show>.` (SolidJS) est levée au clic
(journal `parcours-*.json`, étape `mini-revenir`/`fermer`). Variante « Fermer la lecture » depuis le mini-lecteur : même
erreur, la couche est masquée (visibility hidden) mais n'est PAS démontée et la `<video>` garde sa source — sur un abonnement
à une connexion, c'est potentiellement un flux encore tenu. Cause probable : `src/etat.ts:277-283` (`arreterLecture` :
`poserSelection(null)` + `aller()` dans un `batch`) fait tomber le `<Show when={selection()} keyed>` de `src/App.tsx:327-368` ;
pendant le démontage, une lecture de l'accesseur `e()` (prop `suivant`, `lire: () => … e().cle`, ou un `props.entree` lu dans
un `onCleanup` de `src/screens/Lecture.tsx:498-505, 579-581, 627-631`) lève « Stale read », l'exception interrompt la
disposition et le DOM reste en place. Captures : `captures/zombie-croix-1360-chromium.png`, `zombie-croix-390-webkit.png`,
`zombie-croix-390-firefox.png`, `echec-parcours-a-voir-ecran-1360-chromium.png` (écran À voir recouvert), `echec-parcours-reglages-1360-chromium.png`.

### D2 — MAJEUR — Barre de commandes du lecteur plus large que l'écran : « Plein écran », « Arrêter », « Diffuser » sortent du cadre
Reproduction : téléphone 390×844, lecture d'une chaîne (Direct) → 10 boutons de 44 px (`ControlesLecteur.tsx:67` `cible = 44`,
`:115/209` `flex: 0 0 auto`, aucun retour à la ligne ni réduction) = 440 px + marges > 390 : « Plein écran » (x = 382) est
hors du cadre `overflow:hidden`, « Arrêter » est coupé en deux (`lecture-direct-390-chromium.png`, `-414-`). À 320 px, en
Direct, Diffuser, Arrêter et Plein écran disparaissent (`lecture-direct-320-chromium.png`) ; en film à 320 px et à 390 px zoom
125 %, « Plein écran » disparaît (`lecture-film-320-webkit.png`, `lecture-film-390z125-chromium.png`). Mesure
`resultats-chromium-complet.json` → `gabarits.390.ecrans.lecture-direct` (boutons `Plein écran@…`). Attendu : toutes les
commandes atteignables (débordement en menu « … », seconde rangée, ou boutons essentiels seulement). Les trois moteurs.

### D3 — MAJEUR — Hors bureau, la carte d'erreur de lecture passe sous la barre de commandes : « Réessayer » et « Retour » inatteignables
Reproduction : 390×844 ou 844×390 ou 768×1024, lire une chaîne dont le flux échoue (ici 404) → la carte `role=alert`
(`ErreurLecture.tsx:47-56`, `position:absolute; inset:0` dans le cadre vidéo 16:9 plafonné à `56dvh`,
`Lecture.tsx:1001`) est plus haute que le cadre (219 px à 390) ; les boutons sont recouverts par la barre de commandes
(`z-index: 2`, `ControlesLecteur.tsx:81`) qui reste affichée tant qu'il y a une erreur. Mesure : chevauchements
`button:Couper le son x button:Réessayer`, `input:Volume x button:Retour` (844×390) ; captures `lecture-direct-390-chromium.png`,
`lecture-direct-844x390-chromium.png`, `lecture-direct-375-firefox.png`. Au bureau (1360) la carte tient au-dessus de la barre.
Attendu : l'erreur et ses actions au-dessus des commandes, ou commandes masquées pendant l'erreur.

### D4 — MAJEUR — « Plein écran » ne fait rien sur WebKit, donc sur tout iPhone (Safari, Chrome, Firefox iOS)
`Lecture.tsx:796-799` n'appelle que `cadre.requestFullscreen?.()` sur un `<div>`. Sur WebKit :
`document.fullscreenEnabled=false`, `Element.requestFullscreen` absent, mais `video.webkitEnterFullscreen` présent
(mesure `api` dans `resultats-webkit.json`, 320/375 : `pleinEcran {fs:false, wk:false, vwk:false}` après le clic). Sur iPhone,
seul `video.webkitEnterFullscreen()` existe — jamais utilisé (grep `webkitEnterFullscreen` : 0 occurrence dans `src/`).
Le double-clic (`Lecture.tsx:995`) a le même sort. Chromium/Firefox : fullscreen OK (`fs:true`). Attendu : plein écran natif
vidéo sur iPhone (c'est l'usage principal déclaré).

### D5 — MAJEUR — Barre d'outils du Direct écrasée sur téléphone et tablette ; badge des variantes qui masque le nom de la chaîne
`BarreOutilsDirect.tsx:65` : le bloc titre + compteur est `flex:1; min-width:0` ; tout le reste est `flex:none` (champ 170 px
tablette, 3 boutons 44 px, 4K, sélecteur 3 segments). À 390 et 768 il ne reste ~60 px : le titre devient « FR … » et
« 15 chaînes sur 16 » s'empile sur 3 lignes (mesure `texte-deborde span.num:15 chaînes sur 16 46>36`) :
`direct-390-chromium.png`, `direct-768-chromium.png`. Dans les lignes (`LigneChaine.tsx:22-24`, colonnes tablette
`minmax(0,1fr)`), le badge `4K · HD · standard` (`:44-60`, `flex:none`) prend le pas sur le nom `tronque` : à 844×390 le nom
disparaît complètement (ligne 1 : badge + FR seulement, `direct-844x390-chromium.png`), à 768 il chevauche l'étiquette FR
(`direct-768-chromium.png`), à 1024×768 il reste « Chaîn… » (`direct-1024x768-chromium.png`). Attendu : le nom d'abord, le badge
sacrifié ou replié ; titre/compteur lisibles. Les trois moteurs.

### D6 — MAJEUR — En-tête de la fiche série coupé à droite sur téléphone
390×844 : « 2 saisons · 6 épisodes · Drame, Thriller · 20|19 », « Une histoire de fa|mille », « Disponible immédiatement »
sont coupés par le bord (`fiche-serie-390-chromium.png`, `-375-firefox.png`) ; au zoom 125 % le bouton « Retirer de « À voir » »
sort lui-même de l'écran (`fiche-serie-390z125-chromium.png`). Cause (lu dans le code) : `FicheSerie.tsx:503`
`grid-template-columns: 'auto 1fr'` — une piste `1fr` vaut `minmax(auto,1fr)` : la largeur minimale de son contenu
(`<span class="num">`/boutons `nowrap`) élargit la colonne au-delà de la fenêtre ; il faut `minmax(0,1fr)`. Même famille que la
remarque de `Lecture.tsx:1004` qui, elle, utilise `minmax(0,1fr)`. Trois moteurs.

### D7 — MAJEUR — Échap avec le panneau « Variantes » ouvert quitte le lecteur au lieu de fermer le panneau
Reproduction (clavier/télécommande) : lecture d'une chaîne → V (ou bouton HD) → Échap → URL `/direct`, lecteur quitté
(`resultats-*.json` → `lecture-variantes.apresEchap {url:'/direct', panneauOuvert:false}` sur les 13 gabarits × 3 moteurs).
Cause : `Lecture.tsx:822-826` (`fermerCalque`) et `calques.ts:115-122` ne connaissent ni `variantesLecteur.ouvert()` ni le
rebours de liste ; `calqueOuvert()` (`:816-818`) ne l'inclut pas non plus → `opts.surQuitterEcran()`. Attendu : Échap ferme le
panneau, comme pour Infos/Mini-guide/Diffuser.

### D8 — MAJEUR (expérience) — Sur téléphone et tablette, le lecteur garde la coque : vidéo de 219 px, deux flèches « retour », champ de recherche au-dessus
À 390 : en-tête 52 px (champ de recherche + bouton Lecteur) + flèche `entete-retour` (`EnTete.tsx:90`, visible < 1100 px) +
barre du lecteur avec sa propre flèche `lecteur-retour` (`Lecture.tsx:1052`) → deux retours superposés (mesure
`chevauche button.entete-retour x button.lecteur-retour` à 768/1024 en plein écran) ; la vidéo n'occupe que 219 px de haut
(`lecture-film-390-chromium.png`). À 844×390 (téléphone couché, gabarit « tablette ») : rail 72 px + en-tête 52 px + vidéo 16:9
plafonnée à `56dvh` = 218 px de vidéo pour 390 de haut (`lecture-film-844x390-chromium.png`) : le paysage n'apporte rien, aucun
passage automatique en plein écran, pas de verrou d'orientation. Voir E1.

### D9 — MINEUR — Le champ de recherche n'a aucun indicateur de focus clavier
`coque.css:147` `.entete-recherche input { … outline: none }` sans règle `:focus`/`:focus-visible` (le `:focus-visible`
global de `tokens.css` est écrasé). Mesure `clavier` : `input:Recherche universelle | outline=none` sur tous les gabarits et
moteurs, alors que tous les boutons ont `outline solid 2px`. Un utilisateur télécommande/clavier ne voit pas où il est.

### D10 — MINEUR — Barre d'onglets à 320 px et à 390 px zoom 125 % : libellés qui se touchent
7 onglets (6 + profil) de `min-width:44px` (`coque.css:377-378`, libellé 11 px gras `max-width:100%`) : « Accueil » et
« Direct » se collent (`accueil-320-chromium.png`, `films-320-chromium.png`, `accueil-390z125-chromium.png`). Lisible mais
sans gouttière ; à 320 px Apple/Google recommandent 5 onglets max.

### D11 — MINEUR (lu dans le code) — Menu contextuel de largeur fixe
`MenuContextuel.tsx:15` `LARGEUR = 260` px et entrées sur une ligne : avec un réglage de taille de texte du téléphone
(Android « Taille de police », iOS Dynamic Type) ou des libellés plus longs, le texte est coupé. Non reproduit proprement
(mon émulation texte était fautive) ; à vérifier sur appareil avec « Très grand ».

### D12 — MINEUR — Guide sur téléphone : jour tronqué (« mercre »), ligne de statistiques tronquée
`Guide.tsx:281-300` : la rangée de navigation défile (bien) mais le libellé du jour est le dernier enfant d'une rangée
`overflow-x:auto` sans indicateur ; la ligne « 46 chaîne(s) affichée(s) · 46 avec guide sur 46 chaînes en … » est tronquée
par `text-overflow` (`guide-390-chromium.png`, `guide-375-firefox.png`). Le bouton « Revenir à maintenant » mesure 132×43.

### D13 — MINEUR — Un échec de chargement du guide (EPG) affiche « Ce contenu n'existe plus au catalogue. — Actualiser le catalogue… »
Le Guide (`Guide.tsx:334`) réutilise la table des erreurs du lecteur (`errors.ts:207-210` : tout 404 → « contenu n'existe
plus ») pour une requête `/epg/grille` ; déclenché ici par le faux serveur (pas d'EPG), mais tout 404/erreur du guide donnera
ce message absurde pour un guide (`guide-390-chromium.png`, `guide-844x390-chromium.png`). Le même mapping sert au bandeau
d'erreur de l'Accueil.

### D14 — MINEUR — IndexedDB indisponible (navigation privée Safari < 17 / stockage bloqué / Firefox privé ancien) : exception brute à l'écran
Avec `indexedDB.open` qui lève `InvalidStateError`, la connexion aboutit mais le bandeau d'import affiche
« Catalogue non mis à jour : DatabaseClosedError: InvalidStateError A mutation operation was attempted on a database that did
not allow mutations. … » (`BandeauImport.tsx:47` affiche `e.message` tel quel) + une erreur non interceptée
(`parcours-*.json` → `idbPanne`, `captures/idb-panne-390-*.png`). Attendu : phrase humaine (« Ce navigateur bloque le stockage
local ; désactivez la navigation privée… »). Identique sur les trois moteurs.

### D15 — MINEUR (lu dans le code) — `viewport-fit=cover` sans zones de sécurité latérales
`index.html:5` demande `viewport-fit=cover` ; seules `env(safe-area-inset-bottom)` (`coque.css:360,370`,
`FeuilleFiltres.tsx:135`) sont gérées. En paysage sur iPhone à encoche, la flèche « Retour » du lecteur (x = 12,
`Lecture.tsx:1052`), le rail de 72 px et la colonne de zapping passent sous l'encoche/l'indicateur ; en mode « ajouter à
l'écran d'accueil », l'en-tête fixe passe sous la barre d'état (pas de `safe-area-inset-top`). Non vérifiable en Playwright.

### D16 — MAJEUR (à confirmer sur appareil) — WebKit : la page plante au zapping après un échec de flux HLS
Sur WebKit 27.2 (Playwright, = moteur Safari/Chrome/Firefox iPhone), lecture d'une chaîne dont le flux HLS échoue (404 ici) puis
changement de chaîne *en place* (bouton « Chaîne suivante », ↓, ou clic dans la liste de zapping) → « Target crashed »
(page morte, écran blanc, flux coupé). Reproduit : balayage 6/6 gabarits (320 → 768, `resultats-webkit.json` → `echecs`
`lecture-zap … Target crashed`), parcours 2/2 (390 et 1360, `parcours-webkit.json`), isolement `crash-webkit.mjs` :
« liste-laterale : PLANTAGE » ; en revanche ouvrir une autre chaîne par son URL (nouvelle sélection) passe, et Variantes + Échap
passe. Chromium et Firefox : aucun plantage. Piste : WebKit lit le HLS nativement (pas hls.js) ; `changerDeChaine`
(`etat.ts:260`) réaffecte la source d'un `<video>` dont le chargement précédent a échoué. À reproduire sur un iPhone réel avant
d'écarter : un onglet Safari qui plante coupe aussi la lecture.

### D17 — MINEUR — WebKit lève des erreurs de page « ResizeObserver loop completed with undelivered notifications » sur Direct, Films, Séries
`GrilleAffiches.tsx:77`, `GrilleCartes.tsx:53`, `ControlesLecteur.tsx:55` : le callback de l'observateur écrit un signal qui
re-pose immédiatement la grille (boucle de mesure). Inoffensif visuellement mais remonté en `pageerror` par Safari (donc dans
la télémétrie / la console des utilisateurs), pas par Chromium ni Firefox.

### D18 — MAJEUR — « Rien à mettre à la une — importer le catalogue depuis les Réglages » après un import réussi, jusqu'au rechargement (3 moteurs)
Reproduction (`alaune.mjs`) : navigateur vierge (nouvel appareil, navigation privée, cache vidé), connexion avec un profil
existant → l'Accueil se monte pendant que l'import tourne → une fois l'import terminé (`catalogue.version` en base), la rangée
« À la une » est absente et le message d'import est affiché ; 3 s plus tard : idem ; aller dans Films puis revenir à l'Accueil :
idem ; seul un rechargement la fait apparaître. Résultat : chromium 2/2 (profil existant), firefox 2/2, webkit 3/3 (y compris au
tout premier profil). Avec un vrai catalogue (150 000 entrées, import de plusieurs secondes) l'utilisateur tombe
systématiquement dans ce cas à chaque nouvel appareil. Cause probable : `Accueil.tsx:388` `requete(cles.aLaUne, alaUne)`
avec fraîcheur `catalogue: Infinity` (`requetes.ts:29-33`) : la réponse vide calculée avant la fin de l'import n'est pas
invalidée par celle-ci (les autres rangées, elles, se remplissent : `echec-parcours-fiche-1360-firefox.png`,
`echec-parcours-fiche-1360-webkit.png`). Le message accuse en plus l'utilisateur de ne pas avoir importé.

### D19 — MINEUR — Les commandes du lecteur ne se révèlent qu'au `mousemove`
`Lecture.tsx:994` `onMouseMove={calques.reveiller}` : au doigt, seul le `mousemove` synthétique d'un tap les fait apparaître ;
aucun tap ne les masque, aucun double-tap ±10 s. Fonctionne (Playwright tactile les a réveillées), mais c'est un comportement
souris transposé (voir E1).

### D20 — MINEUR (lu dans le code) — Mini-lecteur mobile posé sur le contenu
`MiniLecteur.tsx:49` `position:fixed; bottom: var(--hauteur-onglets)` (48 px de haut) alors que `.coque-contenu` ne réserve
que la hauteur des onglets (`coque.css:364`) et que les écrans ne connaissent pas le mini-lecteur : la dernière ligne d'une
liste défilée jusqu'en bas reste sous lui (`parcours-mini-390-chromium.png`, `-firefox`, `-webkit` : le mini-lecteur recouvre la
troisième rangée de la grille Films). La mesure automatique du masquage n'a pas abouti (étape instable), d'où « lu dans le code ».

### D21 — MINEUR — Cibles tactiles sous 44 px sur tablette (gabarit « tablette » 700–1099 px, pourtant tactile) et quelques-unes sur téléphone
Tablette 768/1024 : boutons d'en-tête 36×36, flèches de rangée 32×32, flèches du carrousel 28×28, puces de filtre 28 px, boutons
À voir 34×34, `select` 28 px, boutons Réglages 30–36 px, boutons du lecteur 38 px. Téléphone : champs de Connexion/Profil 34 px,
onglets de résultats 40 px, « Tout voir » 64×20, bouton « Fermer » des Variantes 24×30, « Retour » du lecteur 34×26,
`select` d'À voir 28 px. Listes complètes : `resume-chromium.txt` (« petites(…) »).

### D22 — MINEUR — Téléphone en paysage (844×390) : rail de 8 entrées compressé à 41 px de haut
`.rail-item` 54 px × 8 ne tiennent pas dans 390 px : les entrées passent à 55×41 (icône + libellé collés), `Réglages` et
le profil se touchent (`accueil-844x390-chromium.png`). Le gabarit « tablette » n'est pas pensé pour 390 px de haut.

### D23 — À VÉRIFIER — Firefox annonce `?cap=hevc` au serveur, Chromium et WebKit non
`parcours-firefox.json` : source `/api/v1/vod/iptva:5000?cap=hevc` ; `capacites.ts:21-57` (`canPlayType` + `mediaCapabilities`)
conclut que Firefox 157 Linux décode le HEVC. Si le serveur sert alors un MKV HEVC « tel quel » à un Firefox sans décodeur
matériel (Windows sans extension HEVC), la lecture échouera ; la clé `hevc-defaillant` (`:18`) ne couvre que l'échec a posteriori.
Non vérifiable ici (le média factice est un MP3 et Firefox headless n'a pas de sortie audio : `MEDIA_ERR_DECODE` attendu).

## 3. Constats d'expérience (avec référence)

E1 — Lecteur téléphone non immersif. mytv garde en-tête, champ de recherche, rail/onglets et pied « EN LECTURE » autour d'une
vidéo de 219 px (portrait) ou 218 px (paysage) ; pas de rotation → plein écran, pas de tap pour masquer, pas de double-tap
pour sauter, deux flèches retour (D8, D19, D4, D15). Référence : YouTube mobile et Netflix passent en plein écran dès la
rotation en paysage (`screen.orientation` + fullscreen vidéo), un tap masque/affiche les commandes, double-tap gauche/droite
= ±10 s, glisser vers le bas réduit en mini-lecteur (YouTube) ; Disney+ et Prime verrouillent le paysage en lecture. Sur iPhone
cela passe obligatoirement par `video.webkitEnterFullscreen()` (commandes natives) ou par un conteneur plein écran à soi.

E2 — Barre de commandes « tout en même temps ». 10 boutons en Direct sur 390 px (D2). Référence : Netflix mobile n'affiche
en superposition que lecture/pause, ±10 s, plein écran et un menu « Audio et sous-titres » / « Épisodes » ; YouTube met qualité,
sous-titres, PiP, diffusion dans un menu « ⋮ » ; Apple TV iOS groupe en 3 boutons. Les commandes rares (Éco, Infos, Variantes,
Diffuser, Arrêter) ont leur place dans un menu ou une feuille.

E3 — Panneaux du lecteur dans le cadre vidéo. Infos, Diffuser, Variantes et l'erreur sont dessinés *dans* le cadre 16:9
(219 px sur téléphone) et y défilent (`lecture-infos-390-chromium.png`, `lecture-diffusion-390-chromium.png`, D3). Référence :
Netflix/Disney+ ouvrent ces panneaux en feuille pleine hauteur (bottom sheet) par-dessus la vidéo, jamais contraints au cadre ;
YouTube ouvre une feuille modale « Qualité » depuis le bas. Sur téléphone, une feuille `position:fixed` comme le menu profil
(`coque.css:393-405`, déjà bien fait) serait cohérente.

E4 — Direct sur tablette/téléphone : la liste privilégie les badges techniques (« 4K · HD · standard », « 15 chaînes sur
16 ») au détriment du nom de la chaîne et du titre de la catégorie (D5). Référence : Plex, YouTube TV et la liste de chaînes
Netflix affichent toujours logo + nom + programme en cours ; les variantes de qualité sont une info de second rang (petit
« HD » discret, ou choix dans le lecteur) et ne sont jamais prioritaires sur le nom.

E5 — Tablette = souris ? Le gabarit « tablette » (700–1099 px) reprend les cibles bureau (28–36 px, D21) alors qu'un iPad
et un téléphone couché (844×390) y tombent. Référence : Apple HIG 44 pt et Material 48 dp partout où il y a un doigt ; Netflix
iPad garde les mêmes tailles de boutons qu'iPhone. Les seuils devraient dépendre de `pointer: coarse`, pas seulement de la largeur.

E6 — Grille 3 colonnes à 320 px : légendes « 2000 · … » illisibles (`films-320-chromium.png`). Référence : Netflix passe à
2 colonnes sous 360 px ; Disney+ cache la légende sous l'affiche au profit du titre seul.

E7 — Entrée dans la recherche ouvre la première suggestion (`parcours-chromium.json` → `urlApresEntree: /serie/…`) alors que
le pied de la liste dit « Voir les 13 résultats — Entrée ». Référence : YouTube, Netflix, Plex : Entrée = page de résultats ;
la suggestion ne s'ouvre qu'en la sélectionnant (flèches puis Entrée, ou tap).

E8 — Deux vérités sur la qualité : le panneau « Lecteur » de l'en-tête propose Auto/1080p/720p/480p, les Réglages proposent
Auto/Éco, le lecteur a un bouton Éco (`panneau-lecteur-390-chromium.png`, `reglages-390-chromium.png`). Référence : Netflix a un
seul réglage « Qualité de lecture » (auto/basse/moyenne/haute) répété tel quel partout ; YouTube un seul menu « Qualité ».

E9 — Messages techniques exposés : « Série · iptva:s5001 » dans l'en-tête de la fiche série (`fiche-serie-390-chromium.png`),
« HTTP 404 — manifestLoadError », « DatabaseClosedError… » (D14), « Débit mesuré : non mesurable — réponse trop courte »
(panneau Lecteur). Référence : Netflix cache le code d'erreur derrière un identifiant court (« Erreur NW-2-5 ») et une
phrase d'action ; Plex n'affiche jamais un identifiant interne dans un titre.

E10 — Focus clavier/télécommande : hormis le champ de recherche (D9), le focus est visible et l'ordre de tabulation est bon
(rail → en-tête → contenu, Entrée ouvre « Voir les détails », Échap revient : `parcours-*.json` → `clavier`). Référence : YouTube TV
et Netflix TV entourent aussi la carte entière, pas seulement le bouton ; ici le `:focus-visible` 2 px est suffisant au bureau,
un peu fin à 3 m d'un téléviseur.

E11 — Rangées horizontales : barre de défilement masquée (`RangeeHorizontale.tsx:140` `scrollbar-width:none`) et flèches
32 px uniquement hors mobile ; à la souris sans molette horizontale, seules les flèches permettent d'avancer (Firefox et
Chromium confirmés : `rangee-defilement`, `fleches:1`). Référence : Netflix fait pareil mais révèle les flèches au survol de la
rangée sur toute la hauteur ; Disney+ ajoute un défilement par glisser à la souris.

## 4. Synthèse (pour le coordinateur)
1. **D1 bloquant, tous navigateurs** : « Arrêter le flux » laisse le lecteur peint sur tous les écrans jusqu'à un rechargement
   (erreur « Stale read from <Show> » au démontage, `App.tsx:327-368` / `etat.ts:277-283`) ; la variante mini-lecteur garde la
   `<video>` montée. C'est très probablement une grande part du « plein de bugs » ressenti.
2. **Lecteur téléphone (usage principal)** : commandes plus larges que l'écran (Plein écran/Arrêter inatteignables à 390, D2),
   erreurs de lecture dont les boutons sont sous la barre (D3), « Plein écran » inerte sur iPhone (D4, `requestFullscreen`
   sur un div), vidéo de 219 px entre deux flèches retour et un champ de recherche (D8) — pas de mode immersif ni de paysage utile.
3. **Direct sur téléphone/tablette cassé** (D5) : titre et compteur écrasés, nom de chaîne masqué par le badge des variantes.
4. **Fiche série coupée à droite sur téléphone** (D6, `grid 'auto 1fr'` → `minmax(0,1fr)`), et Échap qui éjecte du lecteur
   quand le panneau Variantes est ouvert (D7).
5. **« À la une » vide avec le message « importer le catalogue » sur tout nouvel appareil** tant qu'on ne recharge pas (D18,
   3 moteurs) ; et sur WebKit (iPhone), **plantage de la page au zapping après un flux HLS en échec** (D16, 8/8 essais, à
   confirmer sur appareil). Sinon les trois moteurs rendent la même chose : les écarts sont des API (fullscreen/PiP/Remote),
   des `pageerror` ResizeObserver propres à Safari (D17), le `cap=hevc` de Firefox (D23) ; aucune différence de police ni de
   défilement. Les gabarits « tablette » traitent un iPad comme un bureau (cibles 28–36 px, D21).
