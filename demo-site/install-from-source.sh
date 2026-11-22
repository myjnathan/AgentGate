#!/usr/bin/env bash
# AgentGate 演示宣传站一键部署脚本（源码构建版）
# 用法：在 AgentGate 仓库克隆目录内执行  bash demo-site/install-from-source.sh
# 前提：已安装 Go 1.27（脚本会自动识别 /usr/local/go/bin）；Debian/Ubuntu + systemd
# 内容：从源码构建 agentgate 二进制 + 宣传页（demo-site/index.html）+ Caddy 反代/HTTPS
# 幂等：可重复运行；主密钥只生成一次，存 /etc/agentgate-demo/env（不外传）
set -euo pipefail

SERVER_IP="${SERVER_IP:-118.145.114.187}"
DOMAIN="agentgate.com.cn"
DEMO_DOMAIN="demo.agentgate.com.cn"
APP_DIR="/opt/agentgate"
SITE_DIR="/var/www/agentgate"
DATA_DIR="/var/lib/agentgate-demo"
CONF_DIR="/etc/agentgate-demo"
RUN_USER="agdemo"

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

if [ "$(id -u)" -ne 0 ]; then
  echo "请用 root 运行：sudo bash demo-site/install-from-source.sh" >&2
  exit 1
fi

# --- Go 工具链 ---
if ! command -v go >/dev/null 2>&1; then
  export PATH="$PATH:/usr/local/go/bin"
fi
if ! command -v go >/dev/null 2>&1; then
  echo "未找到 go，请先安装 Go 1.27 后重试" >&2
  exit 1
fi
echo "==> Go: $(go version)"

# --- 基础工具（curl 用于下载 Caddy 与自检） ---
if ! command -v curl >/dev/null 2>&1; then
  echo "==> 安装 curl / ca-certificates"
  apt-get update -qq
  DEBIAN_FRONTEND=noninteractive apt-get install -y -qq curl ca-certificates >/dev/null
fi

# --- 运行用户与目录 ---
id "$RUN_USER" >/dev/null 2>&1 || useradd --system --home "$DATA_DIR" --shell /usr/sbin/nologin "$RUN_USER"
mkdir -p "$APP_DIR" "$SITE_DIR" "$DATA_DIR" "$CONF_DIR"
chown -R "$RUN_USER:$RUN_USER" "$DATA_DIR"

# --- 从源码构建 ---
echo "==> 构建 agentgate（源码目录：$REPO_ROOT）"
(cd "$REPO_ROOT" && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o "$APP_DIR/agentgate.new" ./cmd/agentgate)
install -m 0755 "$APP_DIR/agentgate.new" "$APP_DIR/agentgate"
rm -f "$APP_DIR/agentgate.new"
install -m 0644 "$REPO_ROOT/demo-site/index.html" "$SITE_DIR/index.html"
echo "==> 二进制与宣传页已就位"

# --- 环境文件（主密钥只生成一次） ---
if [ ! -f "$CONF_DIR/env" ]; then
  MASTER_KEY="$(head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n')"
  cat > "$CONF_DIR/env" <<EOF
AGENTGATE_ADDR=127.0.0.1:8787
AGENTGATE_DATA=$DATA_DIR
AGENTGATE_MASTER_KEY=$MASTER_KEY
AGENTGATE_DEMO=1
EOF
  chmod 600 "$CONF_DIR/env"
  echo "==> 已生成主密钥并写入 $CONF_DIR/env（仅本机保存）"
fi

# --- systemd：演示服务 ---
cat > /etc/systemd/system/agentgate-demo.service <<EOF
[Unit]
Description=AgentGate Demo (agentgate.com.cn)
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$RUN_USER
Group=$RUN_USER
EnvironmentFile=$CONF_DIR/env
ExecStart=$APP_DIR/agentgate
Restart=on-failure
RestartSec=3
NoNewPrivileges=true
ProtectSystem=strict
ReadWritePaths=$DATA_DIR
PrivateTmp=true

[Install]
WantedBy=multi-user.target
EOF

# --- systemd：每 6 小时重置演示数据（与官方演示站一致的节奏） ---
cat > /etc/systemd/system/agentgate-demo-reset.service <<EOF
[Unit]
Description=Reset AgentGate demo data

[Service]
Type=oneshot
ExecStart=/bin/bash -c 'systemctl stop agentgate-demo; rm -rf $DATA_DIR/*; systemctl start agentgate-demo'
EOF
cat > /etc/systemd/system/agentgate-demo-reset.timer <<EOF
[Unit]
Description=Reset AgentGate demo data every 6 hours

[Timer]
OnBootSec=6h
OnUnitActiveSec=6h
Unit=agentgate-demo-reset.service

[Install]
WantedBy=timers.target
EOF

systemctl daemon-reload
systemctl enable --now agentgate-demo.service
systemctl enable --now agentgate-demo-reset.timer

# --- Caddy（优先发行版包，其次官方下载） ---
if ! command -v caddy >/dev/null 2>&1; then
  echo "==> 安装 Caddy（apt）"
  apt-get update -qq && apt-get install -y -qq caddy || true
fi
if ! command -v caddy >/dev/null 2>&1; then
  echo "==> 下载 Caddy 二进制"
  curl -fsSL "https://caddyserver.com/api/download?os=linux&arch=amd64" -o /usr/local/bin/caddy
  chmod 0755 /usr/local/bin/caddy
fi
CADDY_BIN="$(command -v caddy)"
echo "==> Caddy: $CADDY_BIN"
id caddy >/dev/null 2>&1 || useradd --system --home /var/lib/caddy --shell /usr/sbin/nologin caddy
mkdir -p /var/lib/caddy /var/log/caddy
chown -R caddy:caddy /var/lib/caddy /var/log/caddy

mkdir -p /etc/caddy
cat > /etc/caddy/Caddyfile <<EOF
$DOMAIN, www.$DOMAIN {
	root * $SITE_DIR
	file_server
	encode gzip
}

$DEMO_DOMAIN {
	reverse_proxy 127.0.0.1:8787
	encode gzip
}

http://$SERVER_IP {
	root * $SITE_DIR
	file_server
	encode gzip
}

http://$SERVER_IP:8080 {
	reverse_proxy 127.0.0.1:8787
	encode gzip
}
EOF

cat > /etc/systemd/system/caddy.service <<EOF
[Unit]
Description=Caddy web server
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
User=caddy
Group=caddy
ExecStart=$CADDY_BIN run --environ --config /etc/caddy/Caddyfile
ExecReload=$CADDY_BIN reload --config /etc/caddy/Caddyfile --force
TimeoutStopSec=5s
LimitNOFILE=1048576
PrivateTmp=true
ProtectSystem=full
AmbientCapabilities=CAP_NET_BIND_SERVICE

[Install]
WantedBy=multi-user.target
EOF
systemctl daemon-reload
systemctl enable --now caddy.service
systemctl restart caddy.service

# --- 防火墙（有 ufw 才动） ---
if command -v ufw >/dev/null 2>&1; then
  ufw allow 22/tcp >/dev/null 2>&1 || true
  ufw allow 80/tcp >/dev/null 2>&1 || true
  ufw allow 443/tcp >/dev/null 2>&1 || true
  ufw allow 8080/tcp >/dev/null 2>&1 || true
fi

# --- 自检 ---
sleep 2
echo "==> 自检"
curl -fsS http://127.0.0.1:8787/healthz && echo " <- AgentGate 演示服务正常"
curl -s -o /dev/null -w "宣传站(HTTP/IP): %{http_code}\n" "http://127.0.0.1/"
systemctl is-active agentgate-demo caddy

cat <<EOF

完成。接下来：
1. 确认云厂商安全组已放行 80 / 443 / 8080 端口；
2. 域名解析（agentgate.com.cn 的 @、www、demo 三条 A 记录）指向 $SERVER_IP 后，
   https://$DOMAIN 与 https://$DEMO_DOMAIN 会自动签发证书（Caddy 自动完成，可能需要几分钟）；
3. 解析生效前可先用 http://$SERVER_IP/ 看宣传站、http://$SERVER_IP:8080/ 打开演示管理台；
4. 演示账号：admin / agentgate-admin（管理员），lixiaofeng / demo12345（员工）；数据每 6 小时自动重置。
EOF
