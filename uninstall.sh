#!/bin/sh
set -eu

[ "$(id -u)" -eq 0 ] || {
  printf '请使用 sudo 运行卸载命令。\n' >&2
  exit 1
}

purge=false
[ "${1:-}" = "--purge" ] && purge=true

systemctl disable --now lightmarks.service >/dev/null 2>&1 || true
rm -f /etc/systemd/system/lightmarks.service /usr/local/bin/lightmarks
systemctl daemon-reload

if [ "$purge" = true ]; then
  rm -f /etc/lightmarks.env
  rm -rf /var/lib/lightmarks
  userdel lightmarks >/dev/null 2>&1 || true
  printf 'Lightmarks、配置和数据已全部删除。\n'
else
  printf 'Lightmarks 已卸载，数据仍保留在 /var/lib/lightmarks。\n'
  printf '彻底删除可重新运行：sudo sh uninstall.sh --purge\n'
fi
