# Audit A — Navigation et découverte (mytv-ui, dist du 2026-11-04)

Méthode : application complète (`/opt/mytv-ui/dist`) contre `tests/e2e/serveur-factice.mjs` (127.0.0.1, tout autre hôte bloqué), EPG servi par interception au format de `serveur-fictif.mjs`, direct servi par interception (playlist + segments TS factices) ; aucun vrai flux. Deux gabarits : bureau 1360×860 et téléphone 390×844 (Chromium, `isMobile`, tactile). Scripts et captures : `/opt/mytv-work/audit/A/` (`commun.mjs`, `01-connexion.mjs` … `08-divers.mjs`, journaux `*-bureau.journal.txt` / `*-telephone.journal.txt`, captures `NN-ecran-gabarit-NN-etat.png`). Code lu en lecture seule dans `/opt/mytv-ui/src`.

Légende de la colonne « Observé » : **OK** conforme ; **BUG n** renvoie à la section 2 ; **UX n** renvoie à la section 3 ; **(tél.)** spécifique au téléphone.

## 1. Inventaire

### 1.1 Connexion (`screens/Connexion.tsx`)

| Élément | Attendu | Observé |
|---|---|---|
| Champ Identifiant (autofocus) | Focus à l'ouverture | OK (focus initial sur le champ, bureau et tél.) |
| Champ Mot de passe | Saisie masquée, `autocomplete=current-password` | OK |
| Bouton « Se connecter » | Inactif tant qu'un champ est vide | OK (`disabled` sans saisie) |
| Entrée dans le formulaire | Soumet | OK |
| Mauvais mot de passe | Encadré « Connexion refusée / Identifiant ou mot de passe incorrect. », mot de passe vidé | OK ; le focus ne revient pas sur le mot de passe (UX 1) |
| 429 (temporisation) | « Patienter n s » (lu dans le code, non reproduit : le faux serveur ne temporise pas) | — |
| Service injoignable au chargement | Encadré « Service injoignable » + bouton « Réessayer » | OK (`07-reglages-*-13-horsligne-reglages.png`) ; mais le catalogue local reste inaccessible hors ligne (UX 2) |
| Session expirée (401 sur un appel ou au rechargement) | Retour à la connexion | OK techniquement, **sans aucun message** (BUG 12) |
| Identifiant prérempli après déconnexion | — | Vide (le nom est pourtant retenu en `localStorage` pour l'affichage) |

### 1.2 Choix du profil (`screens/ChoixProfil.tsx`)

| Élément | Attendu | Observé |
|---|---|---|
| Liste des profils (boutons avatar + nom + chevron) | Choisit le profil actif | OK |
| « Créer votre premier profil » / « Nouveau profil » + champ | Crée puis active | OK ; **aucun focus** à l'arrivée sur l'écran, ni sur le champ (UX 1) |
| Bouton « Créer et utiliser » | Inactif si nom vide ou espaces | OK |
| « Se déconnecter » | Retour connexion | OK |
| Profil unique à la connexion | Choisi automatiquement (auth.ts:112) | OK (reconnexion → coque directe) |
| Suppression du profil actif (depuis Réglages) | Retour à « Qui regarde ? » | OK, l'URL reste `/reglages` |

### 1.3 Coque : rail, en-tête, menus (`App.tsx`, `BarreLaterale.tsx`, `EnTete.tsx`, `MenuProfil.tsx`, `PanneauLecteur.tsx`)

| Élément | Attendu | Observé |
|---|---|---|
| Rail : Accueil, Direct, Films, Séries, Guide, À voir, Réglages, Profil | Navigation, entrée active | OK ; point bleu « lecture en cours » sur la rubrique du flux : OK |
| Bouton ≡ « Replier / Déployer la navigation » (bureau) | Bascule 220/72 px, mémorisée (`mytv.barre-deployee`) | OK (`01-connexion-bureau-08-rail-replie.png`) |
| Barre d'onglets du bas (tél.) : 7 cibles, Réglages absent | Réglages via la feuille du profil | OK mais Réglages peu découvrable (UX 3) |
| Titre de l'écran dans l'en-tête | Nom de l'écran | OK bureau ; masqué sur tél. |
| Champ « Recherche universelle » + badge « / » | Voir 1.9 | — |
| Indicateur « 0/1 » | Flux actifs / autorisés | OK techniquement ; illisible sans infobulle (UX 4) |
| Bouton « En lecture : … — revenir au lecteur » (bureau) | Retour au lecteur | OK |
| Bouton ⚙ « Lecteur : qualité et informations techniques » | Panneau Qualité (Auto/1080p/720p/480p) + Mesurer + infos | Le choix de qualité **n'a aucun effet** et n'est pas mémorisé (BUG 2) ; « Mesurer » : OK ; Échap ferme : OK ; clic sur « Replier » avec panneau ouvert : panneau reste ouvert (mineur) ; Tab n'entre pas dans le panneau (UX 5) |
| Avatar du profil (en-tête bureau / onglet tél.) | Menu : profils (radio), « Préférences et profils », « Se déconnecter » | OK ; changement de profil : OK, URL conservée ; Échap ferme : OK |
| Bandeau d'import (`BandeauImport.tsx`) | Visible pendant l'import et en erreur, « Réessayer » | OK (`07-reglages-telephone-01-bandeau-import.png`, `07b-import-bureau-03-import-503-accueil.png`) |
| Bandeau abonnement (`BandeauAbonnement.tsx`, admin seulement) | Texte « expire dans 3 jours (le …) » / « expiré ou refusé », bouton Masquer mémorisé | OK (`07-reglages-bureau-08-bandeau-abonnement.png`, `…-09-bandeau-abonnement-expire.png`) |
| Diagnostic (`Diagnostic.tsx`) | Carte d'erreur avec pile + Réessayer sur les lectures IndexedDB | Non reproduit (aucune erreur IndexedDB provoquée) ; lu dans le code, branché sur Films/Séries et la colonne de catégories |
| Notifications (`Notification.tsx`) | Toast 3,8 s au-dessus du mini-lecteur | Émises uniquement par le lecteur ; **aucun geste de navigation (favori, À voir, marquer vu) ne donne de retour** (UX 6) |

### 1.4 Accueil (`screens/Accueil.tsx`, `ALaUne.tsx`, `RangeeHorizontale.tsx`, `CarteAffiche.tsx`, `CarteReprise.tsx`, `MenuContextuel.tsx`)

| Élément | Attendu | Observé |
|---|---|---|
| À la une : flèches ‹ ›, 5 pastilles, boucle | Navigation manuelle | OK (boucle 5 → 1) |
| À la une : « Regarder » | Lecture | OK |
| À la une : « Voir les détails » | Fiche film / fiche série | OK (`/film/vod:iptva:5000`) |
| À la une : « Bande-annonce » | Lien externe YouTube, nouvel onglet | OK (`target=_blank`) |
| À la une : étoile favori | Bascule, libellé change | OK, aucun retour visuel autre que l'icône (UX 6) |
| À la une : méta (année, durée, note, genre, âge), synopsis, casting | Depuis `/films/{id}` | OK |
| Rangée « À voir » (compteur « n à préparer », Tout voir) | Prêts d'abord, badge Prêt, méta d'état | OK fonctionnellement ; « Prêt » affiché deux fois (badge + méta), compteur « 4 à préparer » alors qu'un élément est prêt et un en échec (UX 7, périmètre C) |
| Carte À voir : clic | Fiche (surDetails) | OK |
| Rangée « Continuer à regarder » (compteur « n en cours ») | Cartes 16:9, barre, « Reste … » | OK |
| Carte reprise : clic | Reprend la lecture | OK |
| Carte reprise : bouton « … » → menu Reprendre / Marquer comme vu / Retirer de la liste | Agit sur la position de l'épisode/film | **À la souris et au doigt, aucune des trois entrées ne fonctionne : le menu se ferme au `pointerdown` et le clic atterrit sur la carte** (BUG 1). Au clavier le menu n'est pas atteignable (BUG 1 bis) |
| Rangée « Chaînes récemment regardées » | Cartes chaîne + programme | OK |
| Rangée « Mes chaînes favorites » + « Tout voir » | Toutes les favorites | « Tout voir » ouvre **Direct entier**, pas les favorites (BUG 9) |
| Rangée « En direct maintenant » | Cartes programme en cours | OK |
| Rangées « Films / Séries récemment ajoutés » + Tout voir | Grille Films/Séries triée par date | OK |
| Carte affiche : clic sur l'affiche | ? | **Lance la lecture** du film sans fiche (UX 8) ; série → fiche |
| Carte affiche : survol → « Lecture rapide » / « Lire le prochain épisode », « Ajouter aux favoris », « Voir les détails » | Actions secondaires | OK bureau ; au doigt elles n'apparaissent qu'après un tap qui a déjà lancé la lecture (UX 8, `02b-accueil-telephone-04-accueil-avec-mini-lecteur.png`) ; « Lire le prochain épisode » d'une série ouvre la fiche, pas une lecture (incohérent avec le libellé) |
| Rangée « Parce que vous avez regardé « … » » | Voisines de catégorie du dernier contenu vu | **Absente après la synchro de l'historique ; n'apparaît qu'au rechargement suivant** (BUG 10) |
| Rangée « Films et séries favoris » | Affiches favorites avec étoile | OK |
| Flèches de rangée (bureau), molette → défilement horizontal | OK | OK |
| Squelettes de chargement | Rangée « Films récemment ajoutés » | OK |
| Accueil vide (catalogue non importé) | « Rien à mettre à la une — importer le catalogue depuis les Réglages. » | OK (premier lancement : « Chargement… » + bandeau) |
| Mini-lecteur (`MiniLecteur.tsx`) : Revenir, Réduire/Agrandir (bureau), Couper le son, Fermer | — | OK ; « Fermer la lecture » **quitte l'Accueil pour `/films`** (ou `/direct`) (BUG 8) ; sous-titre = titre répété pour un film (mineur) ; mini-lecteur maintenu avec « Erreur de lecture » quand le flux a échoué |
| Raccourci « / » | Focus du champ (et `/recherche` depuis l'Accueil) | OK, ignoré dans un champ : OK |
| Clavier : Tab | Parcours rail → en-tête → À la une → cartes | OK mais ~250 arrêts de tabulation, aucune flèche entre cartes (UX 9) |
| Clavier : Entrée sur une affiche | Même effet que le clic | Lance la lecture (UX 8) |

### 1.5 Direct (`screens/Parcours.tsx`, `parcours/*`, `LigneChaine.tsx`, `CarteDirect.tsx`, `LigneCategorie.tsx`, `PanneauCoulissant.tsx`)

| Élément | Attendu | Observé |
|---|---|---|
| Colonne : onglets Direct / Films / Séries (bureau, tablette) | Changent d'onglet | OK |
| Colonne : « Rechercher une catégorie » + effacer | Filtre les catégories, état vide « Rien ne correspond à « zzz ». » | OK |
| Colonne : groupes de pays repliables (aria-expanded) | Repli / dépli | OK ; avec le jeu factice tout est sous « Autres » |
| Colonne : clic catégorie | Liste de la catégorie, titre « FR | Généralistes · 15 chaînes sur 16 » | OK ; catégorie non portée par l'URL : perdue au rechargement (UX 10) |
| Colonne : épingler / désépingler | Section « Épinglées » en tête | OK (`03-direct-bureau-04-categorie-epinglee.png`) ; la catégorie apparaît deux fois (épinglée + pays), état actif doublé |
| Barre : « Masquer la catégorie » / « Réafficher » | Masque, compteur, lien « Afficher la masquée » | OK ; masquer **change de catégorie active sans prévenir** et le bouton « Réafficher » n'est disponible qu'après avoir réaffiché puis resélectionné la masquée (UX 11, `categories.ts:233`) |
| Barre : « Filtrer cette liste » | Filtre local, état vide « Aucune chaîne ne correspond » | OK |
| Barre : bouton « 4K » / « 4K masquée » (mémorisé) | Retire les chaînes « 4K » | OK |
| Barre : sélecteur Liste / Cartes / Guide TV | Liste, cartes, raccourci vers l'écran Guide | OK ; « Guide TV » ressemble à un troisième mode mais quitte l'écran (UX 12) |
| Liste : colonnes N° / Chaîne / En ce moment, badges variantes « 4K · HD · standard », pays, jauge | — | OK ; « N° » est l'index de la ligne et la liste est triée par nom (« Chaîne 12 » avant « Chaîne 2 ») (UX 13) ; aucune mise en évidence de la chaîne en cours de lecture (UX 14) |
| Liste : clic sur une ligne (groupe) | Ouvre la variante recommandée | OK : « Chaîne 0 » ouvre « Chaîne 0 HD » sans l'annoncer (UX 15) |
| Mode Cartes | Programme en cours + nom | OK bureau ; sur tél. les noms sont tronqués à « Chaîn… » (BUG 11, `03-direct-telephone-03-direct-cartes.png`) |
| Barre d'outils tél. : titre + compte | Lisible | Titre réduit à « FR … » et compte sur trois lignes (BUG 11, `03-direct-telephone-01-direct-liste.png`) |
| Panneau Catégories (tél.) : bouton, voile, Échap | Ouvre / ferme | OK ; contient deux champs empilés (« Filtrer cette liste » + « Rechercher une catégorie ») (UX 16) ; pas de focus déplacé dans le panneau |
| Catalogue vide | « Importez le catalogue depuis les Réglages… » | OK (lu dans le code `Vide.tsx`) |
| Clavier : Tab dans la liste, Entrée | Ouvre | OK ; pas de flèches (UX 9) |

### 1.6 Lecteur direct, partie navigation (`screens/Lecture.tsx`, `lecture/zapping.ts`, `ListeZapping.tsx`, `MiniGuide.tsx`, `BandeauChaine.tsx`)

| Élément | Attendu | Observé |
|---|---|---|
| Bandeau chaîne après zap | Nom, pays, programme, 3,5 s | OK |
| Liste de zapping (catégorie, compteur, filtre « ↑ ↓ pour zapper ») | Clic = zap ; filtre n'affecte pas ↑↓ | OK ; **les variantes ne sont pas regroupées** (Chaîne 0, Chaîne 0 HD, Chaîne 2 4K…) alors que la liste Direct l'est (UX 15) |
| ↑ / ↓ / PageUp / PageDown | Zap dans la catégorie, URL remplacée (pas d'entrée d'historique) | OK |
| R « Dernière chaîne » | Retour | OK ; toast « Aucune chaîne précédente » sinon |
| G / bouton « Mini-guide » | Fenêtre de 8 chaînes, en cours + ensuite, fermeture au choix | OK (`03-direct-bureau-14-mini-guide.png`) ; mêmes variantes non regroupées |
| Échap dans le champ de filtre | Quitte le champ | OK |
| Échap sans calque | Quitte le lecteur (lecture continue en mini-lecteur) | OK → `/direct` |
| F favori depuis le lecteur | Bascule | OK, sans toast |
| Flèche « Retour » de l'en-tête (tél.) | Quitter le lecteur vers la liste du contenu | Ramène à `/direct` même pour un film ouvert par lien direct (`App.tsx` `surQuitterLecteur` → `poserEcran('parcours')` = onglet retenu, `etat.ts:118`) — mineur, journal `09-retour-telephone.journal.txt` |
| Flux 404 | Message | « Ce contenu n'existe plus au catalogue. HTTP 404 — manifestLoadError » : diagnostic trompeur pour une chaîne du catalogue (UX 17) |

### 1.7 Guide (`screens/Guide.tsx`)

| Élément | Attendu | Observé |
|---|---|---|
| Boutons ⏪ 3 h / ◀ 30 min / Maintenant / 30 min ▶ / 3 h ⏩, date | Fenêtre glissante, cache | OK (nouveau bloc = un appel ; retour = cache) |
| « Maintenant » (tél. : dans la règle) | Recentre | OK |
| Molette horizontale / Maj+molette | Décale | OK |
| Glisser au doigt | Décale au relâchement | Non reproduit (événements tactiles CDP non pris) — lu dans le code |
| « Filtrer les chaînes » | Filtre + compteur « n chaîne(s) affichée(s) · n avec guide sur n » | OK ; état vide à trois causes (« Aucune chaîne ne correspond — ou le catalogue n'est pas importé, ou aucune chaîne n'annonce de guide. ») (UX 18) |
| Clic sur la chaîne | Lecture | OK |
| Clic sur une case (en cours, future ou passée) | ? | **Lance la lecture de la chaîne**, même pour un programme futur ; aucun détail de programme, description seulement en infobulle (UX 19) |
| Trait « maintenant », case en cours surlignée | OK | OK |
| Erreur EPG (réseau coupé) | « Le guide des programmes est injoignable. — Réessayer ; la navigation reste utilisable sans guide. » | OK hors ligne (bureau) ; un 503 sur un nouveau bloc n'affiche rien, les lignes disent « Aucun programme sur cette plage » (mineur) |
| Clavier | Tab passe par chaque chaîne puis chaque case (137 arrêts pour 14 lignes) ; aucune flèche | UX 9 |
| Tél. : barre de navigation défilante, colonne 132 px, 3 px/min | OK | OK (`05-guide-telephone-01-guide.png`) |

### 1.8 Films / Séries (`parcours/EcranMedia.tsx`, `BarreFiltres.tsx`, `BoutonFiltre.tsx`, `FeuilleFiltres.tsx`, `GrilleAffiches.tsx`)

| Élément | Attendu | Observé |
|---|---|---|
| Titre + compte « 80 films » / « 27 films sur 80 » | — | OK |
| Tri segmenté Date d'ajout / Titre (A–Z) / Année / Popularité | Retrie, conservé par « Effacer » | OK |
| Menus Genre / Langue / Année (unique) / Pays / Service (si peuplé) | Multi-choix, menu reste ouvert ; Année ferme | OK ; Échap ferme ; « Effacer » dans le menu : OK |
| Bascules Nouveautés / Favoris / En cours / Son lisible (films seulement) | Filtrent | OK (Favoris 2, En cours 1, Son lisible 64) |
| Pastilles + « Tout effacer » | Retirent un filtre / tous | OK |
| État vide filtré | « Aucun résultat … » + « Effacer les filtres » | OK |
| Grille virtualisée (bureau 6 colonnes, tél. 3) | Affiche, badge Nouveau, note, légende année · format, infobulle « ajouté le » | OK |
| Affiche : clic | ? | **Lecture directe** du film (UX 8) ; aucune action secondaire (favori, À voir, détails) sur la grille, contrairement à l'Accueil et à la recherche (UX 8) ; série → fiche |
| Feuille « Filtres » (tél.) : Tout effacer, groupes, « Afficher n résultats », voile, Échap | Applique au fil de l'eau | OK (`04-films-telephone-02-films-feuille-filtres.png`) |
| Filtres conservés par onglet (Films ≠ Séries) et au retour | OK (lu `criteres` par type) | OK |
| Clavier : Tab affiche par affiche, Entrée = lecture | — | UX 9 |
| Console | — | « Stale read from <Show> » ×3 pendant le parcours Films tél. (BUG 13) |

### 1.9 Recherche (`EnTete.tsx`, `PanneauSuggestions.tsx`, `Recherche.tsx`, `ResultatsRecherche.tsx`)

| Élément | Attendu | Observé |
|---|---|---|
| Focus du champ vide | « Tapez pour chercher… » ou « Recherches récentes » | OK |
| Saisie (180 ms) | Groupes Chaînes / Films / Séries, 5 par groupe, « Voir les n résultats — Entrée · n ms » | OK |
| **Entrée dans le champ** | Page de résultats (le pied du panneau l'annonce) | **Lance la lecture du premier résultat** (présélectionné) (BUG 3) |
| ↑ ↓ puis Entrée | Ouvre la ligne retenue | OK (film → lecture directe) |
| Clic « Voir les n résultats » | `/recherche?q=` | OK |
| Échap | Ferme le panneau, garde la saisie | OK ; re-cliquer le champ déjà focalisé ne rouvre pas (mineur, `PanneauSuggestions.tsx:221`) |
| Recherches récentes : clic / Entrée / « Retirer » | Rejoue la requête | OK (8 max, persistantes) |
| Aucun résultat | « Aucun résultat pour « zzzz ». … Ouvrir la page de résultats » | OK |
| Accent / casse (« sÉrie ») | Trouve | OK |
| Page : titre, « 60 résultats · 0 ms », onglets Tout/Chaînes/Films/Séries, langues, tri Pertinence/Récents/Mieux notés, « Afficher 24 films de plus » | — | OK ; « · 0 ms » est du bruit technique (UX 20) |
| Carte résultat : survol « Ajouter à « À voir » » / « Voir les détails » ; clic | À voir → PUT ; clic → ? | À voir : OK (serveur `vod:iptva:5000`), sans retour visuel ; **clic = lecture directe** (UX 8) |
| Retour après lecture | Revenir aux résultats | Retombe sur `/films`, requête perdue (BUG 8) |
| Ligne chaîne résultat | Lecture | OK |
| `/recherche` sans requête | « Rien à chercher pour l'instant » | OK |
| Tél. : placeholder « Rechercher », panneau pleine largeur | OK | OK |

### 1.10 Réglages (`screens/Reglages.tsx`), import, hors-ligne

| Élément | Attendu | Observé |
|---|---|---|
| Compte : nom, état synchro (« À jour », « Hors ligne — … »), « Se déconnecter » | — | OK ; déconnexion garde l'URL `/reglages` |
| Profils : liste, « actif », « Utiliser », « Supprimer » (confirm natif), « Ajouter » | — | OK ; `confirm()` natif (UX 21) ; **doublon de nom accepté** (BUG 14, `07-reglages-bureau-08-…` montre deux « Chambre ») |
| Lecture : Auto / Éco (radio) | Mémorisé (`mytv.qualite`) | OK, sans message de confirmation |
| Diffusion : EAC3/AC3, HEVC (switch Oui/Non) | Mémorisés | OK |
| Catalogue : Entrées / Version / Dernier import ; « Vérifier le service » ; « Mettre à jour » ; « Réimporter tout » + barre | Messages verts, boutons inactifs pendant l'import | OK (« Le service répond. », « déjà à jour », « 7 changements appliqués, 745 entrées », « 745 entrées importées, 9 catégories ») |
| Coupure réseau en plein export | Reprise transparente | OK (relance silencieuse) |
| Export 503 | Carte d'erreur « Relais / export indisponible / Réessayer l'import. / HTTP 503 » + bandeau sur tous les écrans + Réessayer | OK |
| Session expirée pendant l'usage | Retour connexion | OK sans message (BUG 12) |
| Thème clair/sombre | `config.ts:12` et synchro `theme` existent | **Aucun contrôle dans l'interface** (lu dans le code, mineur) |
| Clavier : Tab | Ordre logique | OK ; les radios n'ont pas de navigation par flèches (mineur) |
| Hors ligne en session (API coupée) | L'interface reste utilisable | Direct/Films/Séries : OK depuis IndexedDB ; favori ajouté hors ligne puis envoyé au retour : OK ; **aucun indicateur global**, les cartes disent « Aucun programme disponible » comme si le guide était vide (UX 2) |
| Hors ligne au rechargement | ? | Écran « Service injoignable », catalogue local inaccessible (UX 2) |
| Liens profonds `/film/…`, `/serie/…`, `/lecture/…`, `/recherche?q=`, `/a-voir` | Écran correspondant | OK ; inconnus (`/truc`, `/film/vod:iptva:99999`) → Accueil silencieusement |

## 2. Bugs

**BUG 1 — bloquant — Le menu « … » de « Continuer à regarder » est inopérant à la souris et au doigt, et déclenche la carte sous le menu.**
Repro : Accueil avec une reprise semée → clic sur « … » d'une carte → clic sur « Marquer comme vu » (ou « Retirer de la liste »).
Attendu : la carte sort de la rangée, PUT/DELETE `/profil/historique/{id}`.
Obtenu : rien ; la rangée et le serveur sont inchangés (journal `02-accueil-*.journal.txt`, `02b-accueil-*.journal.txt` : « menu encore present apres pointerdown ? 0 », « element sous le curseur apres pointerdown: Reprendre Série Test 3 »). Sur téléphone, le relâchement a atterri sur la carte et **a lancé la lecture** de « Film Test 10 » (position 5 s notée côté serveur, carte sortie de la rangée pour une autre raison que le geste demandé).
Cause : `components/MenuContextuel.tsx:31` `window.addEventListener('pointerdown', props.surFermer, { capture: true })` démonte le menu dès l'appui ; le `onPointerDown={(e) => e.stopPropagation()}` de la ligne 48 est un gestionnaire délégué par Solid (document), donc exécuté après la capture sur `window` ; le `click` des entrées (`:61`) n'est jamais émis. Les seuls tests existants (`tests/e2e/us43.mjs:174`) exercent un bouton analogue de l'écran À voir, pas ce menu. Captures : `02b-accueil-bureau-09-menu-contextuel-ouvert.png`, `02-accueil-bureau-05-continuer-menu.png`.
Nota : un `click()` programmatique sur l'entrée exécute bien l'action (`02b` : « continuer apres click() programmatique ») — la logique `marquerVu`/`oublierReprise` est saine, seul le menu est cassé.

**BUG 1 bis — majeur — Le menu contextuel n'est pas utilisable au clavier / à la télécommande.**
Repro : focus sur « … », Entrée → le menu s'ouvre mais le focus reste sur `body` ; ↓ ne fait rien ; Tab sort du menu (vers un bouton du lecteur masqué ou « À voir »). Cause : `MenuContextuel.tsx` ne déplace pas le focus et n'a pas de navigation par flèches ; les entrées sont des `div tabindex=0` non ordonnées après l'ancre. Journal `02b-accueil-*.journal.txt` (« menu ouvert au clavier: 1 focus: body »).

**BUG 2 — majeur — Le panneau « Lecteur » de l'en-tête propose une qualité (Auto / 1080p / 720p / 480p) qui n'a aucun effet.**
Repro : ⚙ dans l'en-tête → « 480p » → Réglages > Lecture reste « Auto », `localStorage.mytv.qualite` = `auto` ; recharger → le panneau revient à « Auto ».
Cause : `components/PanneauLecteur.tsx:81-84` écrit `poserQualite` de `etat.ts:355`, signal que **personne ne lit** (le lecteur lit `lib/qualite.ts` : `screens/Lecture.tsx:306-312`). Deux réglages de « qualité » coexistent avec des vocabulaires différents (Auto/Éco dans Réglages et dans le lecteur ; Auto/1080p/720p/480p dans l'en-tête). Captures : `01-connexion-bureau-07-panneau-lecteur.png`, journal `07-reglages-*` (« apres 480p dans le panneau, Reglages Lecture eco: false »).

**BUG 3 — majeur — Entrée dans le champ de recherche lance la lecture du premier résultat au lieu d'ouvrir la page de résultats.**
Repro : taper « film » dans l'en-tête (panneau ouvert, première ligne présélectionnée), Entrée.
Attendu : `/recherche?q=film` (le pied du panneau affiche « Voir les 60 résultats — Entrée · 1 ms »).
Obtenu : `/lecture/vod:iptva:5000`, « 1 flux actif sur 1 autorisé » (journal `06-recherche-*.journal.txt`, capture `06-recherche-*-03-entree-lance-lecture.png`). Sur un abonnement à une connexion, une recherche coupe donc ce qui est regardé ailleurs.
Cause : `components/PanneauSuggestions.tsx:66` (`selection` initialisée à 0) et `:199-203` (Entrée ouvre `plat()[selection()]` dès qu'il existe) ; `surOuvrir` = `ouvrir` (`etat.ts:166`) qui lit immédiatement un film.

**BUG 8 — majeur — « Fermer la lecture » et « Arrêter » ramènent à la liste du contenu, pas là où l'on était.**
Repro : Accueil → Lecture rapide d'un film → Retour (mini-lecteur sur l'Accueil) → « Fermer la lecture » → l'écran passe sur `/films`. Même chose depuis la Recherche (requête perdue) et depuis le Guide (→ `/direct`).
Cause : `etat.ts:277-283` `arreterLecture` navigue toujours vers `cheminApresLecture` (`lib/sortie-lecteur.ts:19`), y compris quand on est déjà hors du lecteur. Journaux `02b` (« mini-lecteur apres fermer: 0 url: …/films »), `05` (« apres Fermer la lecture -> url: …/direct »), `06` (« retour -> url: …/films requete conservee: '' »).

**BUG 9 — mineur — « Tout voir » de « Mes chaînes favorites » ouvre tout le Direct.**
`screens/Accueil.tsx:539` `surToutVoir={() => poserOnglet('live')}` : aucun filtre favoris n'existe en Direct (la bascule Favoris n'existe que pour Films/Séries). Attendu : la liste des favorites.

**BUG 10 — mineur — La rangée « Parce que vous avez regardé » ignore l'historique synchronisé.**
Repro : profil neuf, historique côté serveur, connexion → la rangée est absente ; aller-retour Direct → toujours absente ; second rechargement → présente (journal `02b-accueil-telephone.journal.txt` « Parce que (1er chargement apres synchro): 0 … 2e rechargement: 1 »). Sur bureau la course est parfois gagnée.
Cause : `screens/Accueil.tsx:387` `requete(cles.recommandation, parceQue)` sans fraîcheur « geste » ; `cles.recommandation` n'est invalidée qu'à l'import et au changement de profil (`lib/requetes.ts:338,358`), jamais par la synchro de l'historique (`lib/synchro.ts:552` n'invalide que `historique`). Le même défaut affectera la rangée après chaque visionnage tant que la page n'est pas rechargée.

**BUG 11 — majeur (téléphone) — Barre d'outils et cartes du Direct illisibles à 390 px.**
Titre réduit à « FR … », compte « 15 chaînes sur 16 » sur trois lignes (`parcours/BarreOutilsDirect.tsx:65-70`, pas de `white-space: nowrap` ni de priorité au titre face aux 4 boutons de 44 px) ; en mode Cartes le nom de chaîne devient « Chaîn… » (`components/CarteDirect.tsx:119`, pied à 2 colonnes sur une carte de ~175 px). Captures `03-direct-telephone-01-direct-liste.png`, `03-direct-telephone-03-direct-cartes.png`.

**BUG 12 — mineur — Session expirée : retour muet à l'écran de connexion.**
Repro : `__test/expirer-sessions` puis n'importe quel geste serveur ou rechargement → page de connexion sans aucun message (`01-connexion-*-17-session-expiree-rechargement.png`, `…-18-session-expiree-geste.png`).
Cause : `lib/auth.ts:174-176` et `:160-163` repassent à `'anonyme'` ; `screens/Connexion.tsx` n'affiche que les échecs de saisie. Le geste en cours est perdu sans explication.

**BUG 13 — mineur — Erreurs « Stale read from <Show> » (×3) en console pendant le parcours Films téléphone** (feuille de filtres puis ouverture d'une affiche ; journal `04-films-telephone.journal.txt`). Lecture d'un accesseur de `<Show>` après sa fermeture — même famille que la correction notée dans `docs/couverture-ui5.md` (« Lire maintenant »). Candidats : `components/CarteAffiche.tsx:164-195` (`lire()`, `basculer()` lus dans des gestionnaires), `components/BarreFiltres.tsx:159-167`.

**BUG 14 — mineur — Deux profils peuvent porter le même nom.**
Réglages > Ajouter « Chambre » deux fois → deux lignes « Chambre », indiscernables dans le menu du profil (`07-reglages-bureau-08-bandeau-abonnement.png`). `lib/auth.ts:251` et `screens/Reglages.tsx:151` ne vérifient rien ; le serveur non plus.

Mineurs supplémentaires, reproduits : la flèche « Retour » mobile du lecteur renvoie sur l'onglet retenu (`/direct`) et non sur la liste du contenu lu (`App.tsx:268` `surQuitterLecteur={() => poserEcran('parcours')}`) ; après Échap, re-cliquer le champ de recherche déjà focalisé ne rouvre pas les suggestions (`PanneauSuggestions.tsx:221`, uniquement `focusin`) ; mini-lecteur d'un film : sous-titre = titre (`screens/Lecture.tsx:1338`) ; thème clair présent dans `config.ts` et synchronisé mais sans contrôle (lu dans le code) ; un 503 du guide sur un nouveau bloc ne produit aucun message (seule la coupure réseau en produit un).

## 3. Expérience

**UX 1 — Focus et formulaires.** « Qui regarde ? » arrive sans focus ; après un mot de passe refusé le focus ne revient pas sur le champ vidé. Netflix/Disney+ focalisent le premier champ utile et, après refus, le mot de passe ; indispensable à la télécommande.

**UX 2 — Hors ligne invisible puis bloquant.** En session, rien ne dit que l'API est coupée : les cartes affichent « Aucun programme disponible », les favoris se cochent sans mention « sera envoyé au retour du réseau » (seule la ligne de Réglages dit « Hors ligne »). Au rechargement, l'application refuse de s'ouvrir alors que le catalogue, les favoris et l'historique sont en base locale. Netflix et Plex ouvrent l'application hors ligne sur les contenus disponibles et bandent l'écran d'un indicateur discret.

**UX 3 — Réglages introuvables sur téléphone.** La barre d'onglets porte 7 cibles (dont le profil) mais pas Réglages ; il faut deviner que l'avatar ouvre « Préférences et profils ». Apple TV/Disney+ : un onglet « Profil/Réglages » unique, Réglages visible dedans en premier.

**UX 4 — « 0/1 » dans l'en-tête.** Compteur de flux sans libellé ni icône, compréhensible seulement par l'infobulle. Plex affiche « Lecture en cours » avec le titre ; YouTube TV n'affiche rien tant que ce n'est pas un problème. À remplacer par un état lisible (« 1 flux · Film Test 0 ») ou à masquer.

**UX 5 — Panneaux sans gestion du focus.** Panneau Lecteur, menu du profil, menu contextuel, panneau Catégories : le focus reste derrière ; Tab se promène dans la page. Les références (dialogues Fluent/Material, Netflix TV) déplacent le focus dans le panneau et le rendent à l'ancre à la fermeture.

**UX 6 — Aucun retour après un geste.** Favori, À voir, marquer comme vu, retirer, qualité, capacités TV : seule l'icône change, parfois hors champ de vision (rangée qui disparaît). Netflix et Prime Video affichent un toast (« Ajouté à Ma liste ») avec annulation ; Plex confirme « Marqué comme vu ».

**UX 7 — Rangée « À voir » (voir aussi C).** « Prêt » apparaît deux fois sur la carte (badge et méta) et le compteur « 4 à préparer » compte les prêts et les échecs. Netflix (Smart Downloads) sépare « Téléchargé » du reste et ne compte que ce qui reste à faire.

**UX 8 — Le clic sur une affiche lance la lecture, mais pas partout.** Accueil (films récents, favoris, recherche, grille Films) : clic = lecture immédiate ; À la une et rangée À voir : clic = fiche ; grille Films : aucune action secondaire ; recherche : À voir + détails mais pas de lecture au survol ; au doigt, les actions au survol sont inaccessibles (le premier tap lit). Sur un abonnement à une connexion, lire par erreur coupe l'autre écran. Netflix, Disney+, Prime Video, Apple TV : l'affiche ouvre toujours la fiche, la lecture est un bouton explicite ; les actions secondaires vivent sur la fiche ou dans un appui long.

**UX 9 — Navigation clavier/télécommande linéaire.** Pas de flèches entre cartes d'une rangée, entre affiches d'une grille, entre cases du guide ni entre lignes du Direct : tout passe par Tab (250 arrêts sur l'Accueil, 137 sur 14 lignes de guide). Les applications TV (Netflix, YouTube TV, Plex HTPC) implémentent la navigation spatiale : flèches entre éléments, Entrée = ouvrir, Retour = remonter.

**UX 10 — État de Direct non adressable.** Catégorie et mode (Liste/Cartes) ne sont pas dans l'URL : un rechargement ou un lien partagé retombe sur la première catégorie. Plex et YouTube TV encodent la catégorie/section dans l'adresse.

**UX 11 — Masquer une catégorie change de catégorie sans prévenir** et le chemin pour la réafficher (« Afficher la masquée » → la sélectionner → « Réafficher ») est caché. Plex : « Masquer » dans un menu de la catégorie avec un toast « Masquée — Annuler », et une section Réglages qui liste les masquées.

**UX 12 — « Guide TV » dans le sélecteur de mode.** Présenté comme un troisième mode d'affichage de la liste, il quitte l'écran ; au retour le sélecteur est sur l'ancien mode. Un lien « Ouvrir le guide → » distinct du sélecteur (YouTube TV sépare « Direct » et « Guide » en onglets).

**UX 13 — « N° » et tri des chaînes.** La colonne « N° » est l'index de la liste (change avec un filtre) et la liste est triée par nom en comparaison de chaînes (« Chaîne 12 » avant « Chaîne 2 », `parcours/chaines.ts:34` sans `numeric: true`), alors que le catalogue porte `numero` du fournisseur. Tout téléviseur et YouTube TV respectent l'ordre/numéro du bouquet.

**UX 14 — Chaîne en cours non signalée dans Direct.** Avec une lecture en mini-lecteur, la liste ne surligne pas la chaîne jouée (`LigneChaine.tsx` sans état actif) ; seule la liste de zapping du lecteur le fait. Plex Live TV et YouTube TV marquent la chaîne en cours.

**UX 15 — Variantes : regroupées d'un côté, pas de l'autre.** La liste Direct montre une ligne « Chaîne 0 · 4K · HD · standard » et ouvre « Chaîne 0 HD » sans le dire ; la liste de zapping et le mini-guide (`lecture/zapping.ts:115-120,132`) alignent chaque variante : ↓ passe par « Chaîne 0 HD » puis « Chaîne 12 ». Attendu : même regroupement partout et une mention « HD choisie pour votre liaison » dans le bandeau (Netflix annonce la qualité retenue dans ses infos de lecture).

**UX 16 — Deux champs de filtre empilés dans le panneau Catégories (tél.)** : « Filtrer cette liste » (les chaînes) au-dessus de « Rechercher une catégorie ». Un seul champ par panneau ; le filtre de chaînes a sa place dans la barre de la liste, en repliant les boutons.

**UX 17 — Diagnostic de flux trompeur.** Un 404 du manifeste d'une chaîne du catalogue donne « Ce contenu n'existe plus au catalogue — Actualiser le catalogue ». Pour le direct, la cause probable est le relais/fournisseur ; Plex dit « Impossible de lire ce flux » et propose Réessayer en premier.

**UX 18 — État vide du guide à trois causes** (« ou le catalogue n'est pas importé, ou aucune chaîne n'annonce de guide »). L'écran connaît la cause (compteur « 0 affichée · 60 avec guide ») : dire « Aucune chaîne ne correspond à « zzz » » quand c'est le filtre.

**UX 19 — Le guide n'a pas de fiche programme.** Cliquer une case (même future) lance la chaîne ; la description n'existe qu'en infobulle (inaccessible au doigt). YouTube TV, Plex, Apple TV ouvrent une fiche (titre, horaire, description, « Regarder » / « Rappel »).

**UX 20 — Chiffres techniques exposés** : « 60 résultats · 0 ms », « Entrée · 1 ms » dans la recherche ; « HTTP 404 — manifestLoadError » dans l'erreur de lecture. À réserver au panneau technique.

**UX 21 — `confirm()` natif pour supprimer un profil** : bloqué dans certains navigateurs de téléviseur et en PWA, hors charte. Un dialogue de l'application (Netflix : page de confirmation dédiée).

Autres constats : « Lire le prochain épisode » (survol d'une carte série) ouvre en réalité la fiche ; le mini-lecteur bureau n'affiche qu'un logo, pas la vidéo (Plex/YouTube affichent l'image) ; la déconnexion laisse l'URL du dernier écran, la reconnexion y retombe ; libellés « Film · 2010 » sous une carte dont le titre contient déjà l'année.

## 4. Synthèse (pour le coordinateur)

1. **Le menu « … » de « Continuer à regarder » ne fait rien** à la souris ni au doigt (et peut lancer la lecture de la carte) : `MenuContextuel.tsx:31` ferme le menu au `pointerdown`. C'est très probablement l'origine du « gestion des vus incompréhensible ».
2. **Entrée dans la recherche lance le premier résultat** au lieu des résultats (`PanneauSuggestions.tsx:66,199`) : sur un abonnement à un flux, chercher coupe ce qui est regardé.
3. **Cliquer une affiche lit immédiatement** (Accueil, Films, recherche) mais ouvre la fiche ailleurs ; au doigt les actions secondaires sont inaccessibles. Les références ouvrent toujours la fiche d'abord.
4. **Quitter/fermer la lecture change d'écran** (`arreterLecture` → `/films`, `/direct`) et perd la recherche en cours ; aucun geste (favori, À voir, vu) ne donne de retour visuel.
5. **Deux « qualités » contradictoires** : le panneau ⚙ de l'en-tête (1080p/720p/480p) n'a aucun effet (`etat.ts:355`) à côté de Auto/Éco des Réglages et du lecteur.
6. Téléphone : barre d'outils et cartes du Direct illisibles à 390 px ; Réglages introuvables ; hors ligne muet puis bloquant au rechargement.
7. Télécommande : aucune navigation par flèches, panneaux et menus sans gestion du focus.
8. Secondaires mais visibles : « Parce que vous avez regardé » absente après synchro, « Tout voir » des favorites ouvre tout le Direct, session expirée sans message, doublons de profils, zapping non regroupé par variantes.
