#!/usr/bin/env bash
# ChangeGuard 核心升级（Python Agent 必须另行同步部署）
#
# 用法：sudo bash deploy/upgrade/changeguard-upgrade.sh \
#   --version 3.1.3 \
#   --archive-url https://github.com/<owner>/<repo>/releases/download/v3.1.3/changeguard-3-1-3.tar.gz \
#   --expected-sha256 <64位哈希>
# 可选：--release-root --current-link --service --health-url
#       --health-timeout（默认 60 秒） --keep-archives（默认 3，至少 2）
#       --agent-health-url --agent-token-env（默认 AGENT_UPSTREAM_TOKEN）
#       --skip-preflight
# 必须已有可回滚版本。脚本不备份数据，不执行数据库迁移。
#
# 下载并通过 SHA256 校验后、安装与切换**之前**默认运行同目录的
# changeguard-upgrade-preflight.sh：核对当前核心与 Agent 的运行身份、可回滚
# 能力与包身份。预检失败（failed）即中止，不切换服务；预检未完成（incomplete，
# 例如未提供 Agent 地址）会打印警告但继续，因为并非每个部署都含 Agent。
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
core_health_url="http://127.0.0.1:8080/health/ready"
agent_health_url="${CHANGEGUARD_AGENT_HEALTH_URL:-}"
agent_token_env="AGENT_UPSTREAM_TOKEN"
core_only=0
skip_preflight=0

fail() { printf 'upgrade_error=%s\n' "$*" >&2; exit 1; }
usage() { sed -n '2,17p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//'; }
while [ "$#" -gt 0 ]; do
  case "$1" in
    -h|--help) usage; exit 0 ;;
    --version|--archive-url|--expected-sha256|--release-root|--current-link|--service|--health-url|--health-timeout|--keep-archives|--agent-health-url|--agent-token-env|--core-health-url)
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
        --agent-health-url) agent_health_url="$2" ;;
        --agent-token-env) agent_token_env="$2" ;;
        --core-health-url) core_health_url="$2" ;;
      esac
      shift 2 ;;
    --skip-preflight) skip_preflight=1; shift ;;
    --core-only) core_only=1; shift ;;
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
# 预检放在安装与切换**之前**：它只读，不写版本目录、不动软链、不重启服务。
# 失败即中止，此时 current 仍指向旧版本，也不会留下失败的新版本目录。
preflight_script="$SCRIPT_DIR/changeguard-upgrade-preflight.sh"
if [ "$skip_preflight" -eq 1 ]; then
  printf '==> Preflight skipped by --skip-preflight (risks not checked)\n'
elif [ ! -f "$preflight_script" ]; then
  fail "preflight script missing: $preflight_script (or pass --skip-preflight)"
else
  printf '==> Running preflight checks\n'
  preflight_args=(
    --core-health-url "$core_health_url"
    --archive "$archive"
    --expected-sha256 "$expected_sha256"
    --target-version "${version//-/.}"
    --release-root "$release_root"
    --current-link "$current_link"
  )
  if [ -n "$agent_health_url" ]; then
    preflight_args+=(--agent-health-url "$agent_health_url" --agent-token-env "$agent_token_env")
  fi
  if [ "$core_only" -eq 1 ]; then
    # 显式声明本部署不含 Agent：预检把 Agent/配套项记为 not_applicable，
    # 其余检查仍然失败关闭；core-only 与 Agent 地址互斥。
    preflight_args+=(--core-only)
  fi
  set +e
  bash "$preflight_script" "${preflight_args[@]}"
  preflight_status=$?
  set -e
  case "$preflight_status" in
    0) ;;
    3) fail "preflight incomplete (exit=3): 未执行的检查不是通过；补齐 --agent-health-url 或声明 --core-only" ;;
    *) fail "preflight failed (exit=$preflight_status); current release unchanged" ;;
  esac
fi

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
