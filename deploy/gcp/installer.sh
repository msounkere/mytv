#!/bin/bash
# Installe mytv sur la VM (en root) : utilisateur, dossiers, ffmpeg, binaire, interface, config, unite.
# Attend dans /tmp : mytv-vm (binaire), mytv-ui-dist.tgz, mytv.vm.yaml, mytv.env, et eventuellement mytv.db.
set -euo pipefail
apt-get install -y -q ffmpeg >/dev/null
id mytv >/dev/null 2>&1 || useradd --system --home /var/lib/mytv --shell /usr/sbin/nologin mytv
install -d -m 750 -o mytv -g mytv /var/lib/mytv /var/lib/mytv/hls_live /var/lib/mytv/cache_images /var/lib/mytv/diag /var/lib/mytv/sauvegardes
install -d -m 755 /opt/mytv/bin /opt/mytv-ui /etc/mytv
install -m 755 /tmp/mytv-vm /opt/mytv/bin/mytv
tar xzf /tmp/mytv-ui-dist.tgz -C /opt/mytv-ui
install -m 640 -g mytv /tmp/mytv.vm.yaml /etc/mytv/mytv.yaml
install -m 640 -g mytv /tmp/mytv.env /etc/mytv/mytv.env
rm -f /tmp/mytv.env
if [ -f /tmp/mytv.db ] && [ ! -f /var/lib/mytv/mytv.db ]; then install -m 640 -o mytv -g mytv /tmp/mytv.db /var/lib/mytv/mytv.db; rm -f /tmp/mytv.db; fi
cat > /etc/systemd/system/mytv.service <<'EOF'
[Unit]
Description=mytv : catalogue, direct HLS, VOD et API (instance VM, sortie fournisseur par VPN)
After=network-online.target vpn-sortie.service
Wants=network-online.target vpn-sortie.service
StartLimitIntervalSec=0

[Service]
Type=simple
User=mytv
Group=mytv
WorkingDirectory=/var/lib/mytv
EnvironmentFile=/etc/mytv/mytv.env
ExecStart=/opt/mytv/bin/mytv --config /etc/mytv/mytv.yaml
Restart=always
RestartSec=3
NoNewPrivileges=true
ProtectSystem=strict
ProtectHome=true
PrivateTmp=true
ReadWritePaths=/var/lib/mytv
MemoryMax=1200M
TimeoutStopSec=30

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload && systemctl enable --now mytv.service
sleep 4 && systemctl is-active mytv && curl -s -o /dev/null -w "mytv repond HTTP %{http_code}\n" http://127.0.0.1:8300/
echo "Reste a faire : Caddy -> reverse_proxy 127.0.0.1:8300 (voir README), puis systemctl reload caddy."
