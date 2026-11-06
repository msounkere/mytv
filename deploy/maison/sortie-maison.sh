#!/bin/bash
# A lancer EN ROOT sur media-server (maison, Abidjan).
# Tunnel WireGuard maison -> Hetzner (10.78.0.2 <-> 10.78.0.1) + relais tinyproxy ecoutant
# UNIQUEMENT sur le tunnel, ouvert seulement au serveur Hetzner.
set -euo pipefail
apt-get install -y -q wireguard-tools tinyproxy >/dev/null
systemctl disable --now tinyproxy 2>/dev/null || true
umask 077
[ -f /etc/wireguard/hetzner.key ] || wg genkey > /etc/wireguard/hetzner.key
cat > /etc/wireguard/wg-hetzner.conf <<EOF
[Interface]
Address = 10.78.0.2/30
PrivateKey = $(cat /etc/wireguard/hetzner.key)

[Peer]
PublicKey = uUEzS+POTPk7k/pm1izcdCcRaowsHprTNapflOs+1gk=
Endpoint = 2.29.137.9:51821
AllowedIPs = 10.78.0.1/32
PersistentKeepalive = 25
EOF
umask 022
cat > /etc/tinyproxy/hetzner.conf <<EOF
Port 8888
Listen 10.78.0.2
Allow 10.78.0.1
Timeout 60
MaxClients 50
LogLevel Warning
ConnectPort 443
ConnectPort 80
ConnectPort 8080
DisableViaHeader Yes
EOF
if [ -f /etc/apparmor.d/tinyproxy ]; then
  echo "file r /etc/tinyproxy/hetzner.conf," > /etc/apparmor.d/local/tinyproxy
  apparmor_parser -r /etc/apparmor.d/tinyproxy
fi
cat > /etc/systemd/system/tinyproxy-hetzner.service <<EOF
[Unit]
Description=Relais mytv pour Hetzner (ecoute sur le tunnel seulement)
After=wg-quick@wg-hetzner.service
Requires=wg-quick@wg-hetzner.service

[Service]
ExecStart=/usr/bin/tinyproxy -d -c /etc/tinyproxy/hetzner.conf
Restart=always
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now wg-quick@wg-hetzner.service tinyproxy-hetzner.service
sleep 2
systemctl is-active wg-quick@wg-hetzner tinyproxy-hetzner
echo
echo "=== CLE A ENVOYER A CLAUDE ==="
wg pubkey < /etc/wireguard/hetzner.key
