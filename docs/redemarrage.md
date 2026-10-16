# Redemarrage : ce qui revient tout seul (verifie le 2026-10-16)

## Serveur de la maison (media-server)
| Element | Reglage | Verifie |
|---|---|---|
| `mytv` (service systemd) | `enabled`, `Restart=always`, `StartLimitIntervalSec=0`, `After=network-online + wg-quick@wg0` | processus tue (SIGKILL) -> relance en < 6 s |
| Tunnel WireGuard `wg-quick@wg0` (10.77.0.2) | `enabled` | actif, la VM joint la maison (ping) |
| Config et secrets | `/etc/mytv/mytv.yaml` (640), `/etc/mytv/mytv.env` (600) | persistants |
| Donnees | `/var/lib/mytv` (SQLite, cache d'images, sauvegardes, `hls_live` nettoye au demarrage) | persistantes |
| Interface | `/opt/mytv-ui/dist` servie par mytv | persistante ; reconstruire avec `NODE_OPTIONS=--tls-max-v1.2 npm run build` |
| ffmpeg / ffprobe | `/usr/bin` (paquet `ffmpeg`) | installes |
| Ancien relais `mytv-relais` et `mytv-relais-tuliprox` (socat) | `disabled` | ne reviennent pas |
| Conteneur `mytv-flux` (ancien remux) | `unless-stopped`, arrete a la main | ne revient pas |
| Jellyfin, Transmission | inchanges | - |

## VM GCP (34.155.68.231)
`caddy` et `wg-quick@wg0` sont `enabled` ; Caddy envoie tout `mytv.msocasa.com` vers `10.77.0.2:8300` (ancienne config : `/etc/caddy/Caddyfile.avant-mytv`).

## Reste a faire (hors de ce depot)
- Caddy de la maison (LXC 111) : toujours vers l'ancienne interface ; fichier pret dans le scratchpad (`Caddyfile-maison.mytv`). AdGuard redirige `mytv.msocasa.com` vers 192.168.50.6.
- TuliProx (LXC 113) : encore demarre au boot, inutilise.

## Apres un redemarrage complet, verifier
1. `systemctl is-active mytv wg-quick@wg0` ; `curl -s http://127.0.0.1:8300/sante`.
2. `/api/v1/admin/sante` (administrateur) : base, stockage, synchro, fournisseur OK.
3. La synchronisation du catalogue repart si le catalogue a plus de 24 h (`au_demarrage`) ; l'EPG attend le catalogue.
