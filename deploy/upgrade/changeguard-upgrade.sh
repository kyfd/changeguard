#!/usr/bin/env bash
# ChangeGuard 核心升级（Python Agent 必须另行同步部署）
#
# 用法：sudo bash deploy/upgrade/changeguard-upgrade.sh \
#   --version 3.1.3 \
#   --archive-url https://github.com/<owner>/<repo>/releases/download/v3.1.3/changeguard-3-1-3.tar.gz \
#   --expected-sha256 <64位哈希>
# 可选：--release-root --current-link --service --health-url
#       --health-timeout（默认 60 秒） --keep-archives（默认 3，至少 2）
# 必须已有可回滚版本。脚本不备份数据，不执行数据库迁移。
set -euo pipefail
umask 022

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

fail() { printf 'upgrade_error=%s\n' "$*" >&2; exit 1; }
usage() { sed -n '2,10p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; }
while [ "$#" -gt 0 ]; do
  case "$1" in
    -h|--help) usage; exit 0 ;;
    --version|--archive-url|--expected-sha256|--release-root|--current-link|--service|--health-url|--health-timeout|--keep-archives)
      [ "$#" -ge 2 ] && [ -n "$2" ] || fail "missing value for $1"
      case "$1" in
        --version) version="$2" ;;
        --archive-url) archive_url="$2" ;;
        --expected-sha256) expected_sha256="$2" ;;
        --release-root) release_root="$2" ;;
        --current-link) current_link="$2" ;;
        --service) service_name="$2" ;;
        --health-url) health_url="$2" ;;
        --health-timeout) health_timeout="$2" ;;
        --keep-archives) keep_archives="$2" ;;
      esac
      shift 2 ;;
    *) fail "unknown option: $1" ;;
  esac
done

# Release 工作流使用连字符目录，但标签使用语义版本；兼容两种命令行写法。
if [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
  version="${version//./-}"
fi
[[ "$version" =~ ^[0-9]+-[0-9]+-[0-9]+$ ]] || fail 'version must use X.Y.Z or X-Y-Z'
[[ "$expected_sha256" =~ ^[0-9a-f]{64}$ ]] || fail 'expected-sha256 must be 64 hex chars'
[[ "$archive_url" == https://* ]] || fail 'archive-url must use HTTPS'
[[ "$health_url" == http://* || "$health_url" == https://* ]] || fail 'invalid health-url'
[[ "$health_timeout" =~ ^[1-9][0-9]{0,3}$ ]] || fail 'health-timeout must be 1..9999'
[[ "$keep_archives" =~ ^[1-9][0-9]{0,2}$ ]] && [ "$keep_archives" -ge 2 ] || fail 'keep-archives must be 2..999'
[[ "$service_name" =~ ^[a-zA-Z0-9][a-zA-Z0-9_.@-]*$ ]] || fail 'invalid service name'
[[ "$release_root" == /* && "$current_link" == /* ]] || fail 'release-root and current-link must be absolute'
[ "$(id -u)" -eq 0 ] || fail 'must run as root'
for tool in curl python3 sha256sum systemctl flock; do
  command -v "$tool" >/dev/null 2>&1 || fail "$tool is required"
done
[ -d "$release_root" ] && [ ! -L "$release_root" ] || fail 'release-root must be an existing directory, not a symlink'
release_root="$(readlink -f -- "$release_root")"
case "$release_root" in /|/opt|/opt/changeguard|/etc|/usr|/var) fail 'unsafe release-root' ;; esac
[ "$(stat -c %u -- "$release_root")" -eq 0 ] || fail 'release-root must be owned by root'
root_mode="$(stat -c %a -- "$release_root")"
(( (8#$root_mode & 022) == 0 )) || fail 'release-root must not be group/world writable'
# 与 watcher 共用锁；不允许安装、切换和清理交错。
[ ! -L "$release_root/.upgrade.lock" ] || fail 'upgrade lock must not be a symlink'
exec 9>"$release_root/.upgrade.lock"
flock -n 9 || fail 'another upgrade is running'
[ -L "$current_link" ] || fail 'current-link must reference an existing release'
current_target="$(readlink -e -- "$current_link")" || fail 'current release is missing'
[ -d "$current_target" ] && [ "$(dirname "$current_target")" = "$release_root" ] || fail 'current release must be directly inside release-root'
current_id="$(basename "$current_target")"
[[ "$current_id" == changeguard-* ]] || fail 'unexpected current release name'
release_id="changeguard-${version}"
[ ! -e "$release_root/$release_id" ] && [ ! -L "$release_root/$release_id" ] || fail 'release target already exists'
install_script="$PRODUCTION_DIR/changeguard-core-install.sh"
[ -f "$install_script" ] || fail "install script missing: $install_script"

# 私有临时目录放在 release-root 内，避免 root 写入可预测的 /tmp 文件。
download_dir="$(mktemp -d "$release_root/.download-XXXXXX")"
link_tmp="${current_link}.upgrade-$$"
cleanup() {
  rm -f -- "$link_tmp"
  rm -rf -- "$download_dir"
}
trap cleanup EXIT
archive="$download_dir/$release_id.tar.gz"
printf '==> Downloading %s\n' "$release_id"
curl -fL --proto '=https' --proto-redir '=https' --retry 3 --connect-timeout 15 -o "$archive" "$archive_url"
actual="$(sha256sum "$archive" | awk '{print $1}')"
[ "$actual" = "$expected_sha256" ] || fail 'archive SHA256 mismatch'
bash "$install_script" "$archive" "$expected_sha256" "$release_root" "$release_id"

switch_release() {
  ln -s -- "$1" "$link_tmp" && mv -Tf -- "$link_tmp" "$current_link"
}
wait_healthy() {
  local deadline=$((SECONDS + health_timeout))
  while [ "$SECONDS" -lt "$deadline" ]; do
    if curl -sf --max-time 2 "$health_url" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  return 1
}

printf '==> Switching to %s (previous: %s)\n' "$release_id" "$current_id"
switch_release "$release_root/$release_id" || fail 'could not switch current-link'
# restart 必须位于显式条件中；否则 set -e 会在回滚前终止脚本。
if systemctl restart "$service_name" && wait_healthy; then
  printf '==> Health check passed.\n'
else
  printf 'error: restart or health check failed; rolling back to %s\n' "$current_id" >&2
  if switch_release "$current_target" && systemctl restart "$service_name" && wait_healthy; then
    printf 'upgrade_status=rolled_back version=%s previous=%s\n' "$version" "$current_id" >&2
  else
    printf 'upgrade_status=rollback_failed version=%s previous=%s manual_intervention_required=true\n' "$version" "$current_id" >&2
  fi
  exit 1
fi

# 保留当前和上一版本，再按 mtime 留最近版本；只删除真实直属目录。
retained=2
while IFS= read -r -d '' entry; do
  old="${entry#* }"
  [ "$old" != "$current_target" ] && [ "$old" != "$release_root/$release_id" ] || continue
  if [ "$retained" -lt "$keep_archives" ]; then
    retained=$((retained + 1))
  else
    printf '==> Removing old release %s\n' "$(basename "$old")"
    rm -rf -- "$old"
  fi
done < <(find "$release_root" -mindepth 1 -maxdepth 1 -type d -name 'changeguard-*' -printf '%T@ %p\0' | sort -z -nr)
printf 'upgrade_status=ok version=%s\n' "$version"
