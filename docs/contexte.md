# Contexte et mesures (a lire avant de coder)

- Un fournisseur IPTV Xtream. **Une seule connexion simultanee par compte.** Il refuse (HTTP 458) les adresses de datacenter.
- Apres qu'un client quitte une chaine, le fournisseur garde la place ~100 s : demander une AUTRE chaine pendant ce temps donne **HTTP 407**. Le zapping doit donc reutiliser le bail et patienter.
- Le fournisseur met un **hash different dans le chemin de chaque segment a chaque playlist** (`/hls/<hash>/845410_153.ts`). hls.js compare l'URL d'un meme numero de segment entre deux playlists et declare « media sequence mismatch » si elle change : les URL reecrites doivent etre stables.
- Latences mesurees : playlist 0,45-0,7 s ; segment 0,2-0,9 s ; ratés ponctuels du fournisseur (6 s sans reponse puis 502).
- Premier appel : la reponse d'une playlist peut etre une redirection 302 vers un autre noeud ; suivre.
- Le code de reference du relais actuel est dans /opt/mytv-relais/*.go (LIRE hls.go, main.go, cache.go, relais_test.go ; NE PAS lire .env ni .annuaire : secrets).
- Style : identifiants en francais sans accents, commentaires qui expliquent le POURQUOI, tests nommes par comportement, aucune dependance superflue.

## Risques

- **Filtre d'hotes des segments (SSRF) : controle de l'URL INITIALE seulement.** `live` refuse une URL de segment vers une adresse privee, loopback ou de lien local (sauf liste `serveur.hotes_segments_autorises`), mais une redirection HTTP du fournisseur vers une telle adresse est suivie par `xtream.Requete` sans nouveau controle. Un fournisseur compromis pourrait donc atteindre un service interne par redirection. Parade a prevoir : controler chaque saut de redirection dans le client HTTP du fournisseur (ou le dialer). Resolution DNS : un « rebinding » entre la verification et la connexion reste possible ; preferer des noms listes explicitement.

## HEVC, Chromecast et televiseurs (US-24 CA4)

Les navigateurs ne lisent pas le HEVC (H.265) et mytv ne transcode pas la video : un film HEVC recoit
`415` avec le code `format_hevc_non_lisible` (les autres formats illisibles : `format_non_lisible`). Beaucoup
de televiseurs et de Chromecast, eux, decodent le HEVC en direct. L'option `vod.relais_brut_hevc` (defaut
**faux**, a n'activer que sur demande) fait servir le fichier tel quel, sans jugement, quand le client le
demande par l'en-tete `X-Mytv-Client: cast` ; sans l'en-tete, ou pour tout autre format illisible, le 415
reste. Le fichier est alors relaye comme une lecture directe (Range, une seule connexion amont).
