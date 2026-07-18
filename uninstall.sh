#!/bin/sh
set -eu

[ "$(id -u)" -eq 0 ] || {
  printf '请使用 sudo 运行卸载命令。\n' >&2
  exit 1
}

systemctl disable --now lightmarks.service >/dev/null 2>&1 || true
rm -f /etc/systemd/system/lightmarks.service
rm -rf /etc/systemd/system/lightmarks.service.d
rm -f /usr/local/bin/lightmarks /etc/lightmarks.env
rm -rf /var/lib/lightmarks
systemctl daemon-reload
systemctl reset-failed lightmarks.service >/dev/null 2>&1 || true

userdel lightmarks >/dev/null 2>&1 || true
groupdel lightmarks >/dev/null 2>&1 || true

printf 'Lightmarks 已完全卸载，程序、服务、配置、账户和全部数据均已删除。\n'
