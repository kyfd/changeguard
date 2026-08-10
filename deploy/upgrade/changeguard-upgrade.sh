#!/usr/bin/env bash
# ChangeGuard 一键升级脚本
#
# 用法：
#   sudo bash changeguard-upgrade.sh \
#     --version 2026.08.10.1 \
#     --archive-url https://github.com/<owner>/<repo>/releases/download/v2026.08.10.1/changeguard-2026.08.10.1.tar.gz \
#     --expected-sha256 <64位哈希> \
#     [--release-root /opt/changeguard/releases] \
#     [--current-link /opt/changeguard/current] \
#     [--service changeguard] \
#     [--health-url http://127.0.0.1:8080/health/ready] \
#     [--keep-archives 3]
#
# 流程：
#   1. 下载升级包并校验 SHA256
#   2. 调用 changeguard-core-install.sh 原子安装到 releases/<release_id>
#   3. 备份当前软链指向的 release
#   4. 切换 current 软链 → systemctl restart
#   5. 健康检查（默认 60s）；失败自动回滚到上一版本
#   6. 清理旧版本（保留最近 N 个）
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PRODUCTION_DIR="$(cd "$SCRIPT_DIR/../production" && pwd)"

version=""
archive_url=""
expected_sha256=""
release_root="/opt/changeguard/releases"
current_link="/opt/changeguard/current"
service_name="changeguard"
health_url="http://127.0.0.1:8080/health/ready"
health_timeout=60
keep_archives=3

usage() {
  sed -n '2,20p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'
  exit 64
}

while [ "$#" -gt 0 ]; do
  case "$1" in
    --version) version="$2"; shift 2 ;;
    --archive-url) archive_url="$2"; shift 2 ;;
    --expected-sha256) expected_sha256="$2"; shift 2 ;;
    --release-root) release_root="$2"; shift 2 ;;
    --current-link) current_link="$2"; shift 2 ;;
    --service) service_name="$2"; shift 2 ;;
    --health-url) health_url="$2"; shift 2 ;;
    --health-timeout) health_timeout="$2"; shift 2 ;;
    --keep-archives) keep_archives="$2"; shift 2 ;;
    -h|--help) usage ;;
    *) printf 'unknown option: %s\n' "$1" >&2; usage ;;
  esac
done

[ -n "$version" ] || { printf '--version is required\n' >&2; usage; }
[ -n "$archive_url" ] || { printf '--archive-url is required\n' >&2; usage; }
[ -n "$expected_sha256" ] || { printf '--expected-sha256 is required\n' >&2; usage; }
[[ "$expected_sha256" =~ ^[0-9a-f]{64}$ ]] || { printf 'expected-sha256 must be 64 hex chars\n' >&2; exit 1; }
[ "$(id -u)" -eq 0 ] || { printf 'must run as root\n' >&2; exit 1; }

release_id="changeguard-${version}"
archive="/tmp/changeguard-${version}.tar.gz"
download_dir="/var/cache/changeguard-upgrades"
install_script="$PRODUCTION_DIR/changeguard-core-install.sh"

[ -f "$install_script" ] || { printf 'install script missing: %s\n' "$install_script" >&2; exit 1; }

printf '==> ChangeGuard upgrade to %s\n' "$version"
printf '    archive: %s\n' "$archive_url"

# 1. 下载
install -d -m 0755 "$download_dir"
if [ -f "$archive" ]; then
  printf '==> Using cached archive %s\n' "$archive"
else
  printf '==> Downloading upgrade archive...\n'
  command -v curl >/dev/null 2>&1 || { printf 'curl is required\n' >&2; exit 1; }
  curl -fL --retry 3 --connect-timeout 15 -o "$archive.part" "$archive_url"
  mv "$archive.part" "$archive"
fi

# 2. 校验下载包 SHA256
actual="$(sha256sum "$archive" | awk '{print $1}')"
if [ "$actual" != "$expected_sha256" ]; then
  printf 'error: archive SHA256 mismatch\n  expected: %s\n  actual:   %s\n' "$expected_sha256" "$actual" >&2
  rm -f "$archive"
  exit 1
fi
printf '==> Archive SHA256 verified (%s)\n' "${actual:0:16}…"

# 3. 原子安装到 releases/<release_id>
printf '==> Installing release %s ...\n' "$release_id"
bash "$install_script" "$archive" "$expected_sha256" "$release_root" "$release_id"

# 4. 记录当前版本（备份用）
current_target="$(readlink -f "$current_link" 2>/dev/null || true)"
current_id="$(basename "$current_target" 2>/dev/null || echo none)"
printf '==> Current release: %s\n' "$current_id"

# 5. 切换软链
printf '==> Switching %s -> %s/%s ...\n' "$current_link" "$release_root" "$release_id"
ln -sfn "$release_root/$release_id" "$current_link"
printf '==> Restarting %s.service ...\n' "$service_name"
systemctl restart "$service_name"

# 6. 健康检查 + 自动回滚
printf '==> Waiting for healthy start (timeout %ss)...\n' "$health_timeout"
healthy=0
for i in $(seq 1 "$health_timeout"); do
  if curl -sf --max-time 2 "$health_url" >/dev/null 2>&1; then
    healthy=1
    break
  fi
  sleep 1
done

if [ "$healthy" -eq 1 ]; then
  printf '==> Health check passed. Upgrade to %s complete.\n' "$version"
  systemctl --no-pager --lines=5 status "$service_name" | tail -6 || true
else
  printf 'error: health check failed after %ss\n' "$health_timeout" >&2
  if [ -n "$current_id" ] && [ "$current_id" != "none" ]; then
    printf '==> Rolling back to %s ...\n' "$current_id"
    ln -sfn "$release_root/$current_id" "$current_link"
    systemctl restart "$service_name"
    sleep 3
    if curl -sf --max-time 5 "$health_url" >/dev/null 2>&1; then
      printf '==> Rollback to %s successful.\n' "$current_id"
    else
      printf 'error: rollback also failed; manual intervention required\n' >&2
      exit 1
    fi
  fi
  exit 1
fi

# 7. 清理旧版本（保留最近 N 个 release）
printf '==> Pruning old releases (keep %s)...\n' "$keep_archives"
ls -1dt "$release_root"/changeguard-* 2>/dev/null | tail -n +"$((keep_archives + 1))" | while read -r old; do
  if [ "$(readlink -f "$current_link")" != "$old" ]; then
    printf '    removing %s\n' "$(basename "$old")"
    rm -rf -- "$old"
  fi
done

printf 'upgrade_status=ok version=%s\n' "$version"
