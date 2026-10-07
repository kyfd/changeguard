#!/usr/bin/env bash
# ChangeGuard 升级 watcher（root 运行）
#
# 轮询 /opt/changeguard/upgrades/：
#   pending/          待处理升级包（Go 服务写入）
#   status.json       升级状态（读写）
#   apply.requested   触发标记（Go 服务写入，内容为包名）
#
# 流程：发现触发标记 → 校验状态 → 安装到 releases/ → 切软链 → 重启 → 健康检查
#       → 写回状态 → 失败自动回滚 → 记录历史。
set -euo pipefail

UPGRADE_ROOT="${DBGUARD_UPGRADE_DIR:-/opt/changeguard/upgrades}"
PENDING_DIR="$UPGRADE_ROOT/pending"
STATUS_FILE="$UPGRADE_ROOT/status.json"
TRIGGER_FILE="$UPGRADE_ROOT/apply.requested"
HISTORY_FILE="$UPGRADE_ROOT/history.json"
RELEASE_ROOT="${CHANGEGUARD_RELEASE_ROOT:-/opt/changeguard/releases}"
CURRENT_LINK="${CHANGEGUARD_CURRENT_LINK:-/opt/changeguard/current}"
SERVICE_NAME="${CHANGEGUARD_SERVICE:-changeguard}"
HEALTH_URL="${CHANGEGUARD_HEALTH_URL:-http://127.0.0.1:8080/health/ready}"
HEALTH_TIMEOUT="${CHANGEGUARD_HEALTH_TIMEOUT:-90}"
INSTALL_SCRIPT="${CHANGEGUARD_INSTALL_SCRIPT:-/usr/local/libexec/changeguard/changeguard-core-install.sh}"
# 升级前只读预检。默认与安装器同目录；未部署该脚本时升级失败关闭，
# 除非显式设置 CHANGEGUARD_SKIP_PREFLIGHT=1 承担未校验的风险。
PREFLIGHT_SCRIPT="${CHANGEGUARD_PREFLIGHT_SCRIPT:-}"
AGENT_HEALTH_URL="${CHANGEGUARD_AGENT_HEALTH_URL:-}"
AGENT_TOKEN_ENV="${CHANGEGUARD_AGENT_TOKEN_ENV:-AGENT_UPSTREAM_TOKEN}"
CORE_ONLY="${CHANGEGUARD_CORE_ONLY:-0}"
SKIP_PREFLIGHT="${CHANGEGUARD_SKIP_PREFLIGHT:-0}"
POLL_INTERVAL="${CHANGEGUARD_POLL_INTERVAL:-2}"
POLL_INTERVAL="${CHANGEGUARD_POLL_INTERVAL:-2}"

log() { printf '[changeguard-upgrade] %s\n' "$*" >&2; }

json_set() {
  python3 - "$STATUS_FILE" "$1" "$2" <<'PY'
import json, sys
path, key, value = sys.argv[1], sys.argv[2], sys.argv[3]
try:
    with open(path, encoding="utf-8") as handle:
        data = json.load(handle)
except Exception:
    data = {}
data[key] = value
with open(path, "w", encoding="utf-8") as handle:
    json.dump(data, handle, ensure_ascii=False, indent=2)
    handle.write("\n")
PY
}

json_get() {
  python3 - "$STATUS_FILE" "$1" <<'PY'
import json, sys
try:
    with open(sys.argv[1], encoding="utf-8") as handle:
        data = json.load(handle)
    print(data.get(sys.argv[2], ""))
except Exception:
    print("")
PY
}

record_history() {
  python3 - "$HISTORY_FILE" "$1" "$2" "$3" "$4" <<'PY'
import json, sys, datetime
path, version, state, message, previous = sys.argv[1], sys.argv[2], sys.argv[3], sys.argv[4], sys.argv[5]
try:
    with open(path, encoding="utf-8") as handle:
        history = json.load(handle)
except Exception:
    history = []
entry = {
    "version": version,
    "state": state,
    "message": message,
    "previous_version": previous,
    "applied_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
}
history = [entry] + history[:19]
with open(path, "w", encoding="utf-8") as handle:
    json.dump(history, handle, ensure_ascii=False, indent=2)
    handle.write("\n")
PY
}

wait_healthy() {
  local deadline=$((SECONDS + HEALTH_TIMEOUT))
  while [ "$SECONDS" -lt "$deadline" ]; do
    if curl -sf --max-time 2 "$HEALTH_URL" >/dev/null 2>&1; then
      return 0
    fi
    sleep 1
  done
  return 1
}

switch_release() {
  local temporary="${CURRENT_LINK}.upgrade-$$"
  ln -s -- "$1" "$temporary" && mv -Tf -- "$temporary" "$CURRENT_LINK"
}
trap 'rm -f -- "${CURRENT_LINK}.upgrade-$$"' EXIT

[ "$(id -u)" -eq 0 ] || { log 'must run as root'; exit 1; }
[[ "$HEALTH_TIMEOUT" =~ ^[1-9][0-9]{0,3}$ ]] || { log 'invalid health timeout'; exit 1; }
[[ "$RELEASE_ROOT" == /* && "$CURRENT_LINK" == /* ]] || { log 'release paths must be absolute'; exit 1; }
[ -d "$RELEASE_ROOT" ] && [ ! -L "$RELEASE_ROOT" ] || { log 'invalid release root'; exit 1; }
RELEASE_ROOT="$(readlink -f -- "$RELEASE_ROOT")"
case "$RELEASE_ROOT" in /|/opt|/opt/changeguard|/etc|/usr|/var) log 'unsafe release root'; exit 1 ;; esac
[ "$(stat -c %u -- "$RELEASE_ROOT")" -eq 0 ] || { log 'release root must be owned by root'; exit 1; }
root_mode="$(stat -c %a -- "$RELEASE_ROOT")"
(( (8#$root_mode & 022) == 0 )) || { log 'release root must not be group/world writable'; exit 1; }
command -v flock >/dev/null 2>&1 || { log 'flock is required'; exit 1; }
[ ! -L "$RELEASE_ROOT/.upgrade.lock" ] || { log 'upgrade lock must not be a symlink'; exit 1; }

[ -f "$INSTALL_SCRIPT" ] || INSTALL_SCRIPT="$(find /opt/changeguard -name changeguard-core-install.sh 2>/dev/null | head -1)"
[ -n "$INSTALL_SCRIPT" ] && [ -f "$INSTALL_SCRIPT" ] || { log "install script not found"; exit 1; }

# 预检默认与安装器同目录；缺失即失败关闭，除非显式声明跳过。
if [ -z "$PREFLIGHT_SCRIPT" ]; then
  PREFLIGHT_SCRIPT="$(dirname "$INSTALL_SCRIPT")/changeguard-upgrade-preflight.sh"
fi
if [ "$SKIP_PREFLIGHT" = "1" ]; then
  log "preflight disabled by CHANGEGUARD_SKIP_PREFLIGHT=1; identity and rollback checks will not run"
elif [ ! -f "$PREFLIGHT_SCRIPT" ]; then
  log "preflight script not found: $PREFLIGHT_SCRIPT (set CHANGEGUARD_SKIP_PREFLIGHT=1 to override)"
  exit 1
fi

log "upgrade watcher started root=$UPGRADE_ROOT install=$INSTALL_SCRIPT preflight=$PREFLIGHT_SCRIPT"

while true; do
  # 每轮释放上一次操作的锁，CLI 与 watcher 使用同一把锁。
  exec 9>&-
  if [ ! -f "$TRIGGER_FILE" ]; then
    sleep "$POLL_INTERVAL"
    continue
  fi

  exec 9>"$RELEASE_ROOT/.upgrade.lock"
  if ! flock -n 9; then
    sleep "$POLL_INTERVAL"
    continue
  fi

  archive_name="$(cat "$TRIGGER_FILE" 2>/dev/null || true)"
  rm -f "$TRIGGER_FILE"
  if [[ ! "$archive_name" =~ ^[a-zA-Z0-9][a-zA-Z0-9._-]*\.tar\.gz$ ]]; then
    log "invalid archive name"
    json_set state failed
    json_set message "升级包文件名无效"
    continue
  fi

  archive="$PENDING_DIR/$archive_name"
  if [ ! -f "$archive" ] || [ -L "$archive" ]; then
    log "archive missing: $archive"
    json_set state failed
    json_set message "升级包文件缺失: $archive_name"
    continue
  fi

  # 校验状态文件中的 SHA256（Go 服务上传时写入）
  expected_sha="$(json_get archive_sha256)"
  version="$(json_get version)"
  if [[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]]; then version="${version//./-}"; fi
  if [[ ! "$version" =~ ^[0-9]+-[0-9]+-[0-9]+$ ]]; then
    json_set state failed
    json_set message "升级版本号无效"
    continue
  fi
  actual_sha="$(sha256sum "$archive" | awk '{print $1}')"
  if [[ ! "$expected_sha" =~ ^[0-9a-f]{64}$ ]] || [ "$actual_sha" != "$expected_sha" ]; then
    log "archive sha256 mismatch: $actual_sha vs $expected_sha"
    json_set state failed
    json_set message "升级包校验失败（SHA256 不匹配）"
    continue
  fi

  release_id="changeguard-${version}"
  previous_target="$(readlink -e -- "$CURRENT_LINK" 2>/dev/null || true)"
  previous_id="$(basename "$previous_target")"
  if [ ! -L "$CURRENT_LINK" ] || [ ! -d "$previous_target" ] || [ "$(dirname "$previous_target")" != "$RELEASE_ROOT" ] || [[ "$previous_id" != changeguard-* ]]; then
    json_set state failed
    json_set message "缺少有效的上一版本，拒绝无法回滚的升级"
    record_history "$version" failed "上一版本无效" "$previous_id"
    continue
  fi
  log "applying upgrade version=$version archive=$archive_name"

  # 安装与切换之前先做只读预检：身份、可回滚能力与包身份。
  if [ "$SKIP_PREFLIGHT" != "1" ]; then
    preflight_args=(--core-health-url "$HEALTH_URL" --archive "$archive"
                    --expected-sha256 "$actual_sha" --target-version "${version//-/.}"
                    --release-root "$RELEASE_ROOT" --current-link "$CURRENT_LINK")
    if [ -n "$AGENT_HEALTH_URL" ]; then
      preflight_args+=(--agent-health-url "$AGENT_HEALTH_URL" --agent-token-env "$AGENT_TOKEN_ENV")
    fi
    if [ "$CORE_ONLY" = "1" ]; then
      # 显式声明本部署不含 Agent：预检把 Agent/配套项记为 not_applicable。
      preflight_args+=(--core-only)
    fi
    set +e
    bash "$PREFLIGHT_SCRIPT" "${preflight_args[@]}"
    preflight_status=$?
    set -e
    if [ "$preflight_status" -ne 0 ]; then
      # incomplete (3) 也拒绝安装：未执行的检查不是通过。
      log "preflight did not pass (exit $preflight_status); no install and no switch"
      json_set state failed
      json_set message "升级前预检未通过（exit=$preflight_status），未安装也未切换"
      record_history "$version" failed "预检未通过" "$previous_id"
      continue
    fi
  fi

  json_set state applying
  json_set message "正在安装 $version ..."
  json_set previous_version "$previous_id"

  if ! bash "$INSTALL_SCRIPT" "$archive" "$actual_sha" "$RELEASE_ROOT" "$release_id"; then
    log "install failed"
    json_set state failed
    json_set message "升级包安装失败，请检查日志"
    record_history "$version" failed "安装失败" "$previous_id"
    continue
  fi

  if ! switch_release "$RELEASE_ROOT/$release_id"; then
    json_set state failed
    json_set message "切换版本失败，需人工检查"
    record_history "$version" failed "切换版本失败" "$previous_id"
    continue
  fi

  # 显式处理 restart 的非零退出，不能让 set -e 跳过回滚。
  if systemctl restart "$SERVICE_NAME" && wait_healthy; then
    log "health check passed, upgrade complete"
    json_set state success
    json_set message "升级成功：$version"
    record_history "$version" success "健康检查通过" "$previous_id"
    rm -f "$archive"
  else
    log "restart or health check failed, rolling back to $previous_id"
    json_set message "新版本启动失败，正在回滚到 $previous_id"
    if switch_release "$previous_target" && systemctl restart "$SERVICE_NAME" && wait_healthy; then
      json_set message "升级失败；已回滚到 $previous_id 并通过健康检查"
      json_set state rollback
      record_history "$version" rollback "上一版本健康检查通过" "$previous_id"
    else
      json_set message "升级失败且回滚未恢复健康，需人工处理"
      json_set state failed
      record_history "$version" failed "回滚失败，需人工处理" "$previous_id"
    fi
  fi
done
