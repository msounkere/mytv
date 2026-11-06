#!/bin/bash
# A lancer EN ROOT sur media-server (maison), APRES sortie-maison.sh.
# Remplace le relais HTTP tinyproxy par un relais SOCKS5 (microsocks) : tinyproxy 1.11.1 (Debian 12)
# repond en HTTP/1.0 avec « Transfer-Encoding: chunked », que Go (mytv) lit a la lettre : les
# marqueurs de morceaux se retrouvaient dans le JSON (get_series_info « illisible », 2026-11-06).
# SOCKS5 ne fait que transporter les octets. Ecoute UNIQUEMENT sur le tunnel (10.78.0.2), dont le
# seul pair est le serveur Hetzner. La resolution DNS est faite ici (socks5h), depuis la maison.
set -euo pipefail
apt-get install -y -q microsocks >/dev/null
cat > /etc/systemd/system/socks-hetzner.service <<EOF
[Unit]
Description=Relais SOCKS5 mytv pour Hetzner (ecoute sur le tunnel seulement)
After=wg-quick@wg-hetzner.service
Requires=wg-quick@wg-hetzner.service

[Service]
ExecStart=/usr/bin/microsocks -i 10.78.0.2 -p 1080
Restart=always
RestartSec=3
DynamicUser=yes
NoNewPrivileges=yes

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now socks-hetzner.service
systemctl disable --now tinyproxy-hetzner.service 2>/dev/null || true
sleep 1
systemctl is-active socks-hetzner
echo "relais SOCKS5 pret sur 10.78.0.2:1080"
