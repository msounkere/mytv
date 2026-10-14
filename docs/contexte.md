# Contexte et mesures (a lire avant de coder)

- Un fournisseur IPTV Xtream. **Une seule connexion simultanee par compte.** Il refuse (HTTP 458) les adresses de datacenter.
- Apres qu'un client quitte une chaine, le fournisseur garde la place ~100 s : demander une AUTRE chaine pendant ce temps donne **HTTP 407**. Le zapping doit donc reutiliser le bail et patienter.
- Le fournisseur met un **hash different dans le chemin de chaque segment a chaque playlist** (`/hls/<hash>/845410_153.ts`). hls.js compare l'URL d'un meme numero de segment entre deux playlists et declare « media sequence mismatch » si elle change : les URL reecrites doivent etre stables.
- Latences mesurees : playlist 0,45-0,7 s ; segment 0,2-0,9 s ; ratés ponctuels du fournisseur (6 s sans reponse puis 502).
- Premier appel : la reponse d'une playlist peut etre une redirection 302 vers un autre noeud ; suivre.
- Le code de reference du relais actuel est dans /opt/mytv-relais/*.go (LIRE hls.go, main.go, cache.go, relais_test.go ; NE PAS lire .env ni .annuaire : secrets).
- Style : identifiants en francais sans accents, commentaires qui expliquent le POURQUOI, tests nommes par comportement, aucune dependance superflue.
