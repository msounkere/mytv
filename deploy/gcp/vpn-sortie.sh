#!/bin/bash
# Sortie VPN cloisonnee pour mytv : un espace de noms reseau « vpn » porte l'interface
# WireGuard du VPN et un proxy HTTP (tinyproxy) joignable depuis la VM sur 10.200.0.2:8888.
# Seules les requetes de mytv vers le fournisseur (reseau.proxy_sortie) empruntent ce chemin.
#
# Usage : vpn-sortie.sh installer | demarrer | arreter | verifier | tester-flux
# Prerequis : /etc/wireguard/vpn.conf (configuration WireGuard fournie par le VPN, 600 root).
set -euo pipefail
NS=vpn
CONF=/etc/wireguard/vpn.conf
VETH_HOTE=veth-vpn0
VETH_NS=veth-vpn1
IP_HOTE=10.200.0.1
IP_NS=10.200.0.2
PORT=8888

demarrer() {
  [ -r "$CONF" ] || { echo "manque $CONF" >&2; exit 1; }
  arreter   # repartir de zero : un demarrage a moitie fait laissait veth deplace (« Cannot find device »)
  ip netns add $NS 2>/dev/null || true
  ip link add $VETH_HOTE type veth peer name $VETH_NS 2>/dev/null || true
  ip link set $VETH_NS netns $NS
  ip addr add $IP_HOTE/30 dev $VETH_HOTE 2>/dev/null || true
  ip link set $VETH_HOTE up
  ip -n $NS addr add $IP_NS/30 dev $VETH_NS 2>/dev/null || true
  ip -n $NS link set $VETH_NS up
  ip -n $NS link set lo up
  # Interface WireGuard creee dans l'hote puis deplacee : les paquets chiffres sortent par
  # l'hote (adresse publique), le trafic en clair n'existe que dans l'espace de noms.
  ip link add wgvpn type wireguard 2>/dev/null || true
  wg setconf wgvpn <(sed '/^Address\|^DNS\|^MTU/d' "$CONF")
  ip link set wgvpn netns $NS
  for a in $(sed -n 's/^Address *= *//p' "$CONF" | tr ',' ' '); do ip -n $NS addr add "$a" dev wgvpn 2>/dev/null || true; done
  ip -n $NS link set wgvpn up
  ip -n $NS route replace default dev wgvpn
  mkdir -p /etc/netns/$NS
  dns=$(sed -n 's/^DNS *= *//p' "$CONF" | cut -d, -f1); echo "nameserver ${dns:-1.1.1.1}" > /etc/netns/$NS/resolv.conf
  cat > /etc/tinyproxy/tinyproxy-vpn.conf <<EOF
Port $PORT
Listen $IP_NS
Timeout 60
Allow $IP_HOTE
Allow 127.0.0.1
MaxClients 50
LogLevel Warning
ConnectPort 443
ConnectPort 80
ConnectPort 8080
DisableViaHeader Yes
EOF
  pkill -f "tinyproxy -c /etc/tinyproxy/tinyproxy-vpn.conf" 2>/dev/null || true
  # DNS du VPN pour tinyproxy. `ip netns exec` pose /etc/netns/$NS/resolv.conf par-dessus la CIBLE
  # du lien /etc/resolv.conf (/run/systemd/resolve/resolv.conf sur la VM) ; systemd-resolved remplace
  # ce fichier par renommage (au demarrage, au renouvellement DHCP) et le montage saute : tinyproxy
  # interrogeait alors 169.254.169.254, injoignable depuis le VPN (incident du 2026-11-04 apres le
  # redimensionnement de la VM : plus aucune resolution, catalogue et chaines vides). On monte donc un
  # tmpfs sur le DOSSIER cible, dans un espace de montage prive : resolved n'y touche plus.
  ip netns exec $NS unshare --mount --propagation private sh -c '
    cible=$(readlink -f /etc/resolv.conf); d=$(dirname "$cible")
    if [ "$d" != /etc ]; then
      mount -t tmpfs -o size=64k,mode=755 tmpfs "$d" && cp /etc/netns/'"$NS"'/resolv.conf "$cible"
    fi
    exec tinyproxy -c /etc/tinyproxy/tinyproxy-vpn.conf'
  echo "proxy de sortie VPN : http://$IP_NS:$PORT"
}

arreter() {
  pkill -f "tinyproxy -c /etc/tinyproxy/tinyproxy-vpn.conf" 2>/dev/null || true
  ip -n $NS link del wgvpn 2>/dev/null || true
  ip link del $VETH_HOTE 2>/dev/null || true
  ip netns del $NS 2>/dev/null || true
}

verifier() {
  echo "-- WireGuard (dans l'espace de noms) --"; ip netns exec $NS wg show wgvpn | grep -E "endpoint|handshake|transfer"
  echo "-- adresse de sortie vue de l'exterieur --"; curl -s -m 10 -x http://$IP_NS:$PORT https://ipinfo.io/json | tr -d '\n' | cut -c1-200; echo
  echo "-- latence vers le fournisseur (API seulement, aucun flux) --"
  curl -s -o /dev/null -m 15 -x http://$IP_NS:$PORT -w "connect %{time_connect}s ttfb %{time_starttransfer}s HTTP %{http_code}\n" "http://line.dndnscloud.ru/player_api.php"
}

tester_flux() {
  # UNE playlist du direct, puis fermeture : a lancer seulement quand personne ne regarde.
  set -a; . /etc/mytv/mytv.env; set +a
  chaine=${1:-543234}
  code=$(curl -s -L --max-redirs 3 -o /dev/null -m 25 -x http://$IP_NS:$PORT -w "%{http_code}" "http://line.dndnscloud.ru/live/${IPTVA_USER}/${IPTVA_PASS}/${chaine}.m3u8")
  case "$code" in 200) echo "flux accepte par le VPN (HTTP 200)";; 458) echo "adresse du VPN refusee par le fournisseur (458) : changer de serveur VPN";; *) echo "HTTP $code";; esac
}

installer() {
  apt-get install -y -q wireguard-tools tinyproxy iproute2 curl >/dev/null
  systemctl disable --now tinyproxy 2>/dev/null || true   # on ne garde que l'instance cloisonnee
  # Ubuntu (24.04+) confine tinyproxy par AppArmor a /etc/tinyproxy/tinyproxy.conf : autoriser notre fichier.
  if [ -f /etc/apparmor.d/tinyproxy ]; then
    echo "file r /etc/tinyproxy/tinyproxy-vpn.conf," > /etc/apparmor.d/local/tinyproxy
    apparmor_parser -r /etc/apparmor.d/tinyproxy
  fi
  install -m 755 "$0" /usr/local/sbin/vpn-sortie.sh
  cat > /etc/systemd/system/vpn-sortie.service <<EOF
[Unit]
Description=Sortie VPN cloisonnee pour mytv (netns vpn + tinyproxy)
After=network-online.target
Wants=network-online.target
Before=mytv.service

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/sbin/vpn-sortie.sh demarrer
ExecStop=/usr/local/sbin/vpn-sortie.sh arreter

[Install]
WantedBy=multi-user.target
EOF
  systemctl daemon-reload && systemctl enable --now vpn-sortie.service
  verifier
}

case "${1:-}" in
  installer) installer;; demarrer) demarrer;; arreter) arreter;; verifier) verifier;; tester-flux) tester_flux "${2:-}";;
  *) echo "usage : $0 installer|demarrer|arreter|verifier|tester-flux [chaine]" >&2; exit 2;;
esac
