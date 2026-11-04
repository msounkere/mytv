# Audit de l'interface mytv-ui (bugs + experience) — brief commun aux relecteurs A, B, C

Contexte : mytv est une plateforme IPTV personnelle (serveur Go dans /opt/mytv, interface SolidJS + Dexie dans
/opt/mytv-ui, bundle deja construit dans /opt/mytv-ui/dist). L'utilisateur vient de tester la nouvelle liste
« À voir » (lot 9, preparation d'avance sur le serveur) : le resultat (lecture fluide) lui plait, mais il juge
l'interface « pleine de bugs » et « la gestion des vus / pret / ajout a la liste incomprehensible ». Il demande
un passage EN REVUE DE TOUS LES BOUTONS ET OPTIONS, a la recherche de bugs, et une proposition d'optimisation
de l'experience fondee sur les meilleures references (Netflix, Disney+, Apple TV, Plex, Prime Video, YouTube TV).

## Regles absolues
- LECTURE SEULE sur /opt/mytv-ui et /opt/mytv : aucun fichier modifie, aucun commit, aucun `git` qui ecrit.
- Jamais de contact avec le vrai fournisseur ni la production (mytv.msocasa.com, 10.77.0.1, 192.168.50.x) : tout
  se fait contre le FAUX serveur tests/e2e/serveur-factice.mjs (qui sert dist sur 127.0.0.1, port ephemere).
- Aucun secret lu (/etc/mytv/*, *.token, *.env, ~/.ssh).
- Tes scripts et captures vont dans /opt/mytv-work/audit/<TA LETTRE>/ (un lien node_modules y est deja).
  Exemple de script (ESM, Node 22, prefixe NODE_OPTIONS=--tls-max-v1.2) :
    import { chromium } from 'playwright'
    import { demarrerServeurFactice, COMPTE, MOT_DE_PASSE } from '/opt/mytv-ui/tests/e2e/serveur-factice.mjs'
    const s = await demarrerServeurFactice({ dist: '/opt/mytv-ui/dist', taille: { chaines: 40, films: 60, series: 12 } })
    // s.origine = http://127.0.0.1:PORT ; connexion : COMPTE / MOT_DE_PASSE ; puis choix d'un profil.
    // Options du faux serveur : lis l'en-tete de serveur-factice.mjs (etats de preparation simules, sonde, cast, etc.)
    // Bloque tout hote autre que 127.0.0.1 via ctx.route comme dans tests/e2e/us43.mjs.
  Lis tests/e2e/us43.mjs, us36.mjs, us39.mjs, us20-lecture.mjs pour voir comment on se connecte, choisit un profil,
  ouvre une fiche, lance une lecture (media factice, aucun vrai flux).
- Teste en DEUX gabarits : bureau 1360x860 et telephone 390x844 (la majorite de l'usage reel est iPhone Firefox/Chrome,
  Android Chrome, Windows Chrome). Teste aussi la navigation au CLAVIER (Tab/fleches/Entree/Echap) : l'interface vise
  aussi la telecommande (navigateur de televiseur).
- Capture d'ecran de chaque ecran/etat visite (page.screenshot) dans ton dossier, nom explicite.
- Lis le code quand un comportement est douteux : src/screens/*.tsx, src/components/*.tsx, src/lib/*.ts. Cite
  fichier:ligne. Les docs de couverture (docs/couverture-ui4.md, couverture-ui5.md) et les tests disent ce qui etait PREVU ;
  un ecart prevu/observe est un bug, un comportement prevu mais deroutant est un probleme d'experience.

## Livrable : /opt/mytv-work/audit/<LETTRE>.md (en francais, precis, sans bavardage)
1. INVENTAIRE : tableau ecran -> element (bouton, menu, option, geste) -> action attendue -> observe (OK / bug / UX) ;
   couvre TOUT ce qui est cliquable ou reglable dans ton perimetre, y compris les menus contextuels, les etats vides,
   les erreurs, le hors-ligne (coupe le faux serveur : s.fermer() ou equivalent) et la session expiree.
2. BUGS : liste numerotee, severite (bloquant / majeur / mineur), reproduction pas a pas, resultat attendu vs obtenu,
   fichier:ligne et cause probable, capture. Un bug non reproduit en Playwright mais evident dans le code est accepte
   avec la mention « lu dans le code ».
3. EXPERIENCE : constats concrets (libelle ambigu, geste cache, incoherence entre ecrans, etats non expliques,
   surcharge, lenteur percue) et pour chacun la pratique de reference (quelle application fait quoi, et pourquoi).
4. SYNTHESE en 10 lignes maximum pour le coordinateur : les 5 problemes qui gachent le plus l'usage.
Ne propose PAS de code. Ne corrige rien. Sois exhaustif sur l'inventaire, exigeant sur la reproduction.
