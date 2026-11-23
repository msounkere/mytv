# Lot 11 — mytv vu comme un fournisseur IPTV (Formuler / MYTVOnline 3)

Objectif : une box IPTV (Formuler, appli MYTVOnline 3, ou tout lecteur Xtream : TiviMate, VLC, Kodi)
se branche sur mytv comme sur un fournisseur Xtream ordinaire, et profite de ce que mytv fait deja :
catalogue, guide, « Ma liste » preparee d'avance, reprises, replay, variantes regroupees.

## Ce que le protocole permet (decision du 22/11/2026)

Xtream est un protocole de LECTURE : la box demande listes, guide et flux. Tout ce qui va de mytv vers
la box passe nativement ; ce qui doit remonter de la box (favoris, positions, « vu ») n'a pas de
commande reconnue par MYTVOnline en Xtream (favoris gardes sur la box). Consequences :

| Fonction | Comment |
|---|---|
| Catalogue, guide, direct, films, series | API Xtream imitee, flux relayes TELS QUELS (la box lit MKV/HEVC/EAC3) |
| « Ma liste », « Reprendre », « Mes chaines » | categories fabriquees par mytv, en tete de liste |
| Prechargement | un film prepare part du disque : demarrage immediat, AUCUNE place prise chez le fournisseur |
| Reprise mytv -> box | « Film X — reprendre a 42 min » : un flux qui commence a la position (remux a la volee) |
| Position, « vu » box -> mytv | deduits des flux relayes (quel contenu, jusqu'ou) |
| Replay, « depuis le debut » | tv_archive + /timeshift relaye (heure convertie, chaines au replay vide cachees) |
| Enregistrements | categorie de films « Enregistrements » ; programmation depuis mytv seulement |
| Favoris box -> mytv | impossible en Xtream ; essai Stalker (etape D) pour savoir si MYTVOnline les envoie |

## Etapes

- **A1 (ce commit et les suivants)** : `/player_api.php` (compte, categories, listes, fiches film et
  serie, guide court), `/get.php` (M3U), `/xmltv.php`, flux `/live/…` (HLS par lien signe),
  `/movie/…` et `/series/…` (relais tel quel, Range). Comptes d'APPAREIL : un compte mytv non
  administrateur (un administrateur est refuse : son mot de passe voyagerait dans les adresses).
- **A2** : flux direct en TS continu (fait) ; categories mytv « ★ Ma liste », « ▶ En cours », « ★ Mes
  chaines » (fait le 23/11) : elles viennent du profil choisi dans Reglages > Box IPTV (le profil du
  compte d'appareil est vide), en tete des categories, leurs contenus envoyes une seconde fois sous la
  categorie mytv (identifiants 990001..990005). Reprise a la position (23/11) : un film de « En
  cours » porte l'identifiant numero + 1 000 000 000 et le nom « — reprendre a 42 min » ; sa lecture
  part de la position de l'historique (relue au moment de lire, moins 10 s), remuxee en MKV
  (ffmpeg -ss, tout recopie, toutes les pistes audio), sans Range ni duree totale. Reste : une
  chaine par groupe de variantes, positions deduites des flux.
- **A3** : relais a la maison (media-server) : la video d'une box de la maison ne fait plus
  l'aller-retour par Hetzner (fournisseur -> maison -> Hetzner -> maison).
- **B** : replay et « depuis le debut » dans le guide de la box.
- **C** : enregistrements exposes en categorie.
- **D (essai)** : faux portail Stalker minimal qui note ce que MYTVOnline envoie (favoris, positions).

## Securite

- Le mot de passe d'un compte d'appareil voyage dans les adresses (convention Xtream) : HTTPS
  obligatoire (Caddy), compte d'appareil dedie, revocable, jamais administrateur.
- Une connexion Xtream ouvre UNE session mytv par compte (gardee en memoire, reverifiee) ; la
  limitation de tentatives de connexion s'applique (meme compteur que l'interface).
- Les adresses du fournisseur ne sortent jamais : flux par liens signes (US-40) ou relais.
