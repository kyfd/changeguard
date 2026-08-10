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

[ -f "$INSTALL_SCRIPT" ] || INSTALL_SCRIPT="$(find /opt/changeguard -name changeguard-core-install.sh 2>/dev/null | head -1)"
[ -n "$INSTALL_SCRIPT" ] && [ -f "$INSTALL_SCRIPT" ] || { log "install script not found"; exit 1; }

log "upgrade watcher started root=$UPGRADE_ROOT install=$INSTALL_SCRIPT"

while true; do
  if [ ! -f "$TRIGGER_FILE" ]; then
    sleep "$POLL_INTERVAL"
    continue
  fi

  archive_name="$(cat "$TRIGGER_FILE" 2>/dev/null || true)"
  rm -f "$TRIGGER_FILE"
  if [ -z "$archive_name" ]; then
    log "trigger missing archive name"
    continue
  fi

  archive="$PENDING_DIR/$archive_name"
  if [ ! -f "$archive" ]; then
    log "archive missing: $archive"
    json_set state failed
    json_set message "升级包文件缺失: $archive_name"
    continue
  fi

  # 校验状态文件中的 SHA256（Go 服务上传时写入）
  expected_sha="$(json_get archive_sha256)"
  version="$(json_get version)"
  actual_sha="$(sha256sum "$archive" | awk '{print $1}')"
  if [ -z "$expected_sha" ] || [ "$actual_sha" != "$expected_sha" ]; then
    log "archive sha256 mismatch: $actual_sha vs $expected_sha"
    json_set state failed
    json_set message "升级包校验失败（SHA256 不匹配）"
    continue
  fi

  release_id="changeguard-${version}"
  previous_target="$(readlink -f "$CURRENT_LINK" 2>/dev/null || true)"
  previous_id="$(basename "$previous_target" 2>/dev/null || echo "")"
  log "applying upgrade version=$version archive=$archive_name"

  json_set state applying
  json_set message "正在安装 $version ..."
  json_set previous_version "$previous_id"

  if bash "$INSTALL_SCRIPT" "$archive" "$actual_sha" "$RELEASE_ROOT" "$release_id"; then
    log "install ok, switching symlink"
    ln -sfn "$RELEASE_ROOT/$release_id" "$CURRENT_LINK"
    systemctl restart "$SERVICE_NAME"
  else
    log "install failed"
    json_set state failed
    json_set message "升级包安装失败，请检查日志"
    record_history "$version" failed "安装失败" "$previous_id"
    continue
  fi

  # 健康检查 + 自动回滚
  healthy=0
  for i in $(seq 1 "$HEALTH_TIMEOUT"); do
    if curl -sf --max-time 2 "$HEALTH_URL" >/dev/null 2>&1; then
      healthy=1
      break
    fi
    sleep 1
  done

  if [ "$healthy" -eq 1 ]; then
    log "health check passed, upgrade complete"
    json_set state success
    json_set message "升级成功：$version"
    record_history "$version" success "健康检查通过" "$previous_id"
    rm -f "$archive"
  else
    log "health check failed, rolling back to $previous_id"
    json_set state rollback
    json_set message "健康检查失败，已回滚到 $previous_id"
    if [ -n "$previous_id" ]; then
      ln -sfn "$RELEASE_ROOT/$previous_id" "$CURRENT_LINK"
      systemctl restart "$SERVICE_NAME"
    fi
    record_history "$version" rollback "健康检查失败，自动回滚" "$previous_id"
  fi
done
