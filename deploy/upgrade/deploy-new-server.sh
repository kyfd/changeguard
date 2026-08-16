#!/usr/bin/env bash
# ChangeGuard 新服务器部署脚本（Ubuntu）
set -euo pipefail

# 对外主机地址：部署前 export PUBLIC_HOST=<你的服务器IP>，默认仅本机验证
PUBLIC_HOST="${PUBLIC_HOST:-127.0.0.1}"

echo "==> 1. 安装基础软件"
export DEBIAN_FRONTEND=noninteractive
apt-get update -qq
apt-get install -y -qq curl nginx postgresql-client jq 2>/dev/null || apt-get install -y -qq curl nginx jq

echo "==> 2. 创建 changeguard 用户"
id -u changeguard >/dev/null 2>&1 || useradd --system --home /opt/changeguard --shell /usr/sbin/nologin changeguard

echo "==> 3. 目录结构"
install -d -m 0755 /opt/changeguard/releases
install -d -m 0755 /opt/changeguard/data
install -d -m 0755 /opt/changeguard/upgrades/pending
install -d -m 0755 /etc/changeguard
chown -R changeguard:changeguard /opt/changeguard/data /opt/changeguard/upgrades

echo "==> 4. 安装 release"
REL_ID="v2026.08.10.newserver.1"
install -d -m 0755 "/opt/changeguard/releases/$REL_ID"
cp -r "/tmp/$REL_ID/." "/opt/changeguard/releases/$REL_ID/"
chown -R root:root "/opt/changeguard/releases/$REL_ID"
chmod 0755 "/opt/changeguard/releases/$REL_ID/dbguard"
chmod 0644 "/opt/changeguard/releases/$REL_ID/"*.json "/opt/changeguard/releases/$REL_ID/"SHA256SUMS
ln -sfn "/opt/changeguard/releases/$REL_ID" /opt/changeguard/current
cd "/opt/changeguard/releases/$REL_ID" && sha256sum -c SHA256SUMS

echo "==> 5. 生成 core.env"
cat > /etc/changeguard/core.env <<EOF
DBGUARD_LISTEN_ADDRESS=127.0.0.1:8080
DBGUARD_PUBLIC_URL=http://${PUBLIC_HOST}
DBGUARD_TRUST_PROXY_HEADERS=true

DBGUARD_AUTH_MODE=local
DBGUARD_AUTH_REGISTRATION_ENABLED=true
DBGUARD_AUTH_SECURE_COOKIE=false
DBGUARD_AUTH_SESSION_TTL=12h
DBGUARD_ENABLE_DEMO_ACCOUNTS=true
DBGUARD_ENABLE_DEMO_DATA=true

DBGUARD_SESSION_MODE=memory
DBGUARD_STORE_MODE=file
DBGUARD_DATA_FILE=/opt/changeguard/data/dbguard.json
DBGUARD_MIGRATION_WITNESS_FILE=/opt/changeguard/data/dbguard.json.rollback-witness.json

DBGUARD_EXPERIMENT_MODE=simulated
DBGUARD_WORKERS=2
DBGUARD_UPGRADE_DIR=/opt/changeguard/upgrades
EOF
chown root:changeguard /etc/changeguard/core.env
chmod 0640 /etc/changeguard/core.env

echo "==> 6. systemd 服务"
cat > /etc/systemd/system/changeguard.service <<'EOF'
[Unit]
Description=ChangeGuard production change governance core
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
User=changeguard
Group=changeguard
WorkingDirectory=/opt/changeguard/current
Environment=DBGUARD_ENV_FILE=/etc/changeguard/core.env
EnvironmentFile=/etc/changeguard/core.env
ExecStartPre=/bin/sh -c 'cd /opt/changeguard/current && sha256sum -c SHA256SUMS >/dev/null'
ExecStart=/opt/changeguard/current/dbguard
Restart=on-failure
RestartSec=3
TimeoutStartSec=60
TimeoutStopSec=15
KillSignal=SIGTERM
LimitNOFILE=65536
UMask=0077
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ReadOnlyPaths=/opt/changeguard/releases /etc/changeguard
ReadWritePaths=/opt/changeguard/data /opt/changeguard/upgrades

[Install]
WantedBy=multi-user.target
EOF

echo "==> 7. 启动主服务"
systemctl daemon-reload
systemctl enable changeguard
systemctl start changeguard
sleep 4
systemctl is-active changeguard
curl -sf http://127.0.0.1:8080/health/live && echo " health-live-ok"
curl -sf http://127.0.0.1:8080/health/ready && echo " health-ready-ok"

echo "==> 8. 安装升级 watcher"
install -d -m 0755 /usr/local/libexec/changeguard
cp /tmp/changeguard-upgrade-watcher.sh /usr/local/libexec/changeguard/changeguard-upgrade-watcher.sh
chmod 0755 /usr/local/libexec/changeguard/changeguard-upgrade-watcher.sh
cp /tmp/changeguard-upgrade-watcher.service /etc/systemd/system/changeguard-upgrade-watcher.service
systemctl daemon-reload
systemctl enable --now changeguard-upgrade-watcher
systemctl is-active changeguard-upgrade-watcher

echo "==> 9. Nginx 反向代理"
cat > /etc/nginx/sites-available/changeguard <<'EOF'
server {
    listen 80;
    server_name _;
    client_max_body_size 600m;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host $host;
        proxy_set_header X-Real-IP $remote_addr;
        proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_read_timeout 120s;
        proxy_buffering off;
    }
}
EOF
ln -sf /etc/nginx/sites-available/changeguard /etc/nginx/sites-enabled/changeguard
rm -f /etc/nginx/sites-enabled/default
nginx -t && systemctl enable nginx && systemctl restart nginx
sleep 2
curl -sf -H "Host: ${PUBLIC_HOST}" http://127.0.0.1/ | head -c 120 && echo " nginx-ok"

echo "deploy=passed"
