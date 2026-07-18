#!/bin/sh
set -eu

REPOSITORY="${LIGHTMARKS_REPOSITORY:-WXD2233/lightmarks}"
INSTALL_DIR="/usr/local/bin"
BINARY_PATH="$INSTALL_DIR/lightmarks"
DATA_DIR="/var/lib/lightmarks"
ENV_FILE="/etc/lightmarks.env"
SERVICE_FILE="/etc/systemd/system/lightmarks.service"
SERVICE_USER="lightmarks"
PORT="${PORT:-5856}"

fail() {
  printf '安装失败：%s\n' "$1" >&2
  exit 1
}

[ "$(id -u)" -eq 0 ] || fail "请使用 sudo 运行安装命令"

for command_name in curl tar sha256sum systemctl useradd install mktemp grep sed tail od tr; do
  command -v "$command_name" >/dev/null 2>&1 || fail "缺少命令：$command_name"
done

case "$(uname -m)" in
  x86_64|amd64) architecture="amd64" ;;
  aarch64|arm64) architecture="arm64" ;;
  *) fail "暂不支持当前 CPU 架构：$(uname -m)" ;;
esac

asset="lightmarks-linux-${architecture}.tar.gz"
release_url="https://github.com/${REPOSITORY}/releases/latest/download"
temporary_dir="$(mktemp -d)"
trap 'rm -rf "$temporary_dir"' EXIT HUP INT TERM

printf '正在下载 Lightmarks (%s)...\n' "$architecture"
curl -fL --retry 3 --connect-timeout 15 "$release_url/$asset" -o "$temporary_dir/$asset"
curl -fL --retry 3 --connect-timeout 15 "$release_url/checksums.txt" -o "$temporary_dir/checksums.txt"

(
  cd "$temporary_dir"
  checksum_line="$(grep "$asset\$" checksums.txt || true)"
  [ -n "$checksum_line" ] || fail "发布包缺少校验值"
  printf '%s\n' "$checksum_line" | sha256sum -c - >/dev/null
)

tar -xzf "$temporary_dir/$asset" -C "$temporary_dir"
[ -f "$temporary_dir/lightmarks" ] || fail "发布包中没有找到程序文件"
install -d -m 0755 "$INSTALL_DIR"
install -m 0755 "$temporary_dir/lightmarks" "${BINARY_PATH}.new"
mv -f "${BINARY_PATH}.new" "$BINARY_PATH"

if ! id "$SERVICE_USER" >/dev/null 2>&1; then
  useradd --system --home-dir "$DATA_DIR" --shell /usr/sbin/nologin "$SERVICE_USER"
fi
install -d -m 0700 -o "$SERVICE_USER" -g "$SERVICE_USER" "$DATA_DIR"

new_install=false
if [ ! -f "$ENV_FILE" ]; then
  new_install=true
  initial_password="123123"
  session_secret="$(od -An -N32 -tx1 /dev/urandom | tr -d ' \n')"
  umask 077
  cat >"$ENV_FILE" <<EOF
BOOKMARK_PASSWORD=$initial_password
SESSION_SECRET=$session_secret
PORT=$PORT
DATA_FILE=$DATA_DIR/bookmarks.json
MEMORY_LIMIT_MB=48
ALLOW_PRIVATE_TARGETS=false
EOF
else
  saved_port="$(sed -n 's/^PORT=//p' "$ENV_FILE" | tail -n 1)"
  [ -z "$saved_port" ] || PORT="$saved_port"
fi
chown root:"$SERVICE_USER" "$ENV_FILE"
chmod 0640 "$ENV_FILE"

cat >"$SERVICE_FILE" <<EOF
[Unit]
Description=Lightmarks private bookmark dashboard
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=$SERVICE_USER
Group=$SERVICE_USER
EnvironmentFile=$ENV_FILE
WorkingDirectory=$DATA_DIR
ExecStart=$BINARY_PATH
Restart=on-failure
RestartSec=3
UMask=0077
NoNewPrivileges=true
PrivateTmp=true
ProtectSystem=strict
ProtectHome=true
ProtectKernelTunables=true
ProtectControlGroups=true
RestrictSUIDSGID=true
LockPersonality=true
CapabilityBoundingSet=
ReadWritePaths=$DATA_DIR
MemoryHigh=56M
MemoryMax=64M
TasksMax=64
LimitNOFILE=4096

[Install]
WantedBy=multi-user.target
EOF

systemctl daemon-reload
systemctl enable lightmarks.service >/dev/null
systemctl restart lightmarks.service
sleep 2

if curl -fsS "http://127.0.0.1:${PORT}/healthz" >/dev/null; then
  printf '\nLightmarks 已安装并运行在端口 %s。\n' "$PORT"
else
  systemctl --no-pager --full status lightmarks.service || true
  fail "服务未通过健康检查"
fi

if [ "$new_install" = true ]; then
  printf '初始登录密码：%s\n' "$initial_password"
  printf '请登录后立即在设置中修改密码。\n'
else
  printf '现有配置和数据已保留。\n'
fi
printf '访问地址：http://你的VPS地址:%s\n' "$PORT"
printf '查看状态：systemctl status lightmarks\n'
