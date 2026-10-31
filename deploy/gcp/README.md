# Instance complete de mytv sur la VM Google (banc GCP)

Objectif : ne plus dependre de la connexion de la maison pour les spectateurs hors de la maison.
Chemin cible : spectateur -> Caddy (VM) -> mytv (VM) -> **VPN** -> fournisseur. La VM ne parle
JAMAIS au fournisseur avec son adresse publique (HTTP 458 : adresses de centres de donnees refusees) :
seules les requetes vers le fournisseur sortent par le VPN, via `reseau.proxy_sortie` (voir
docs/exploitation.md), le reste de la VM (Caddy, SSH, mises a jour) garde sa sortie normale.

```
            +------------------------- VM Google (Paris) -------------------------+
spectateur -| Caddy :443 -> mytv :8300 --proxy_sortie--> [netns vpn] tinyproxy --wg0--|--> VPN --> fournisseur
            +----------------------------------------------------------------------+
```

## 1. Prerequis (a fournir)
- Un abonnement VPN avec configuration **WireGuard** (fichier `.conf` : Mullvad, Proton, Nord...),
  sortie en Europe. Le fichier est copie sur la VM dans `/etc/wireguard/vpn.conf` (600, root).
- L'acces SSH a la VM (utilisateur + cle) et `sudo`.
- Les secrets actuels : `/etc/mytv/mytv.env` (IPTVA_USER, IPTVA_PASS, MYTV_SECRET), copies tels quels.

## 2. Installation (sur la VM, en root)
```
# depuis media-server : construire et envoyer le kit
cd /opt/mytv && GOOS=linux GOARCH=amd64 go build -o /tmp/mytv-vm ./cmd/mytv
cd /opt/mytv-ui && NODE_OPTIONS=--tls-max-v1.2 npm run build && tar czf /tmp/mytv-ui-dist.tgz -C /opt/mytv-ui dist
scp /tmp/mytv-vm /tmp/mytv-ui-dist.tgz deploy/gcp/*.sh deploy/gcp/mytv.vm.yaml /etc/mytv/mytv.env <utilisateur>@<vm>:/tmp/
# sur la VM
sudo bash /tmp/vpn-sortie.sh installer          # netns « vpn » + WireGuard + tinyproxy sur 10.200.0.2:8888
sudo bash /tmp/vpn-sortie.sh verifier           # adresse de sortie vue par le VPN, latence
sudo bash /tmp/installer.sh                     # utilisateur mytv, ffmpeg, binaire, config, unite systemd
```
Puis la base : au premier demarrage mytv synchronise le catalogue (au_demarrage: true, ~15 s) et le
guide ; les comptes (utilisateurs, profils, favoris) se recreent avec `mytv-comptes` ou en copiant
`/var/lib/mytv/mytv.db` depuis la maison (80 Mo) AVANT le premier demarrage.

## 3. Bascule de Caddy (VM)
Dans `/etc/caddy/Caddyfile`, remplacer `reverse_proxy 10.77.0.2:8300` par `reverse_proxy 127.0.0.1:8300`
puis `systemctl reload caddy`. Retour arriere : remettre `10.77.0.2:8300`. La maison reste joignable
par le LAN (192.168.50.3:8300) et garde sa propre instance.

## 4. Test d'un flux (UNE connexion chez le fournisseur : prevenir avant)
`bash /tmp/vpn-sortie.sh tester-flux` ouvre UNE playlist du direct par le proxy, affiche le code HTTP
(200 attendu ; 458 = l'adresse de sortie du VPN est refusee : changer de serveur VPN) et referme.

## 5. Limites de la VM e2-small (2 vCPU partages, 2 Go)
Relais du direct, remux des MKV (copie) et conversion audio : OK. Conversion video (HEVC -> H.264,
mode Eco) : 1 flux 480p au plus ; pour plus, passer en e2-standard-2 ou 4 (`live.reencodage_video: false`
sinon). Disque : prevoir 10 Go pour le cache d'images et les segments.
