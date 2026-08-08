#!/usr/bin/env bash
set -euo pipefail

backup_root="${CHANGEGUARD_BACKUP_DIR:-/opt/changeguard/backups}"
retention="${CHANGEGUARD_BACKUP_RETENTION:-30}"
stamp="$(date -u +%Y%m%d-%H%M%S)"
staging="$backup_root/.staging-$stamp"
snapshot="$backup_root/snapshot-$stamp"

if ! [[ "$retention" =~ ^[0-9]+$ ]] || [ "$retention" -lt 7 ] || [ "$retention" -gt 365 ]; then
  printf 'invalid CHANGEGUARD_BACKUP_RETENTION=%s (expected 7..365)\n' "$retention" >&2
  exit 1
fi

install -d -m 0700 "$backup_root"
if [ -e "$staging" ] || [ -e "$snapshot" ]; then
  printf 'backup target already exists for stamp %s\n' "$stamp" >&2
  exit 1
fi
install -d -m 0700 "$staging/core" "$staging/agent" "$staging/config"

cleanup() {
  if [ -n "${staging:-}" ] && [ -d "$staging" ]; then
    resolved_staging="$(readlink -f -- "$staging" 2>/dev/null || true)"
    resolved_root="$(readlink -f -- "$backup_root" 2>/dev/null || true)"
    case "$resolved_staging" in
      "$resolved_root"/.staging-[0-9]*) rm -rf -- "$resolved_staging" ;;
    esac
  fi
}
trap cleanup EXIT

copy_required() {
  local source="$1"
  local destination="$2"
  if [ ! -r "$source" ]; then
    printf 'required backup source is unreadable: %s\n' "$source" >&2
    exit 1
  fi
  cp -a -- "$source" "$destination"
}

copy_optional() {
  local source="$1"
  local destination="$2"
  if [ -r "$source" ]; then
    cp -a -- "$source" "$destination"
  fi
}

copy_required /opt/changeguard/current/.env "$staging/core/changeguard.env"
chmod 0600 "$staging/core/changeguard.env"

core_copied=0
for attempt in 1 2 3; do
  cp -a -- /opt/changeguard/data/dbguard.json "$staging/core/dbguard.json"
  if python3 -c 'import json,sys; json.load(open(sys.argv[1], encoding="utf-8"))' "$staging/core/dbguard.json" 2>/dev/null; then
    core_copied=1
    break
  fi
  sleep 2
done
[ "$core_copied" -eq 1 ] || { printf 'core JSON backup validation failed\n' >&2; exit 1; }

copy_required /etc/changeguard-agent-gateway.env "$staging/agent/changeguard-agent-gateway.env"
chmod 0600 "$staging/agent/changeguard-agent-gateway.env"
copy_required /opt/changeguard-agent/data/audit.jsonl "$staging/agent/audit.jsonl"
copy_required /opt/changeguard-agent/data/metrics.json "$staging/agent/metrics.json"
copy_optional /opt/changeguard-agent/data/monitor.json "$staging/agent/monitor.json"

python3 - "$staging/agent/audit.jsonl" <<'PY'
import json
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
expected = 1
previous = ""
for number, line in enumerate(path.read_text(encoding="utf-8").splitlines(), 1):
    if not line.strip():
        continue
    record = json.loads(line)
    if record.get("sequence") != expected:
        raise SystemExit(f"audit sequence mismatch at line {number}")
    if record.get("prev_hash", "") != previous:
        raise SystemExit(f"audit previous hash mismatch at line {number}")
    previous = record.get("hash", "")
    if not previous:
        raise SystemExit(f"audit hash missing at line {number}")
    expected += 1
print(f"audit_records={expected - 1}")
PY

python3 -c 'import json,sys; state=json.load(open(sys.argv[1], encoding="utf-8")); assert state["schema"] == "changeguard-agent-metrics/v1" and state["hmac_sha256"]' "$staging/agent/metrics.json"
copy_required /www/server/panel/vhost/nginx/liufengxi.top.conf "$staging/config/liufengxi.top.conf"
copy_required /etc/systemd/system/changeguard-agent-gateway.service "$staging/config/changeguard-agent-gateway.service"
copy_optional /etc/systemd/system/changeguard-agent-monitor.service "$staging/config/changeguard-agent-monitor.service"
copy_optional /etc/systemd/system/changeguard-agent-monitor.timer "$staging/config/changeguard-agent-monitor.timer"

agent_release="$(readlink -f /opt/changeguard-agent/current)"
ui_release="$(readlink -f /opt/changeguard-agent/ui/current)"
core_release="$(readlink -f /opt/changeguard/current)"
printf '%s\n' "$agent_release" > "$staging/config/agent-release.txt"
printf '%s\n' "$ui_release" > "$staging/config/ui-release.txt"
printf '%s\n' "$core_release" > "$staging/config/core-release.txt"
curl -fsS --max-time 5 http://127.0.0.1:18081/health/ready > "$staging/agent/ready.json"
curl -fsS --max-time 5 http://127.0.0.1:18081/health/slo > "$staging/agent/slo.json"

python3 - "$staging/metadata.json" "$stamp" "$core_release" "$agent_release" "$ui_release" <<'PY'
import datetime
import json
import pathlib
import sys

path = pathlib.Path(sys.argv[1])
payload = {
    "schema": "changeguard-backup/v2",
    "stamp": sys.argv[2],
    "created_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
    "core_release": sys.argv[3],
    "agent_release": sys.argv[4],
    "ui_release": sys.argv[5],
}
path.write_text(json.dumps(payload, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
PY

(
  cd "$staging"
  find . -type f ! -name manifest.sha256 -print0 | sort -z | xargs -0 sha256sum > manifest.sha256
  sha256sum -c manifest.sha256 >/dev/null
)

chmod -R go-rwx "$staging"
mv -- "$staging" "$snapshot"
staging=""

mapfile -t snapshots < <(find "$backup_root" -mindepth 1 -maxdepth 1 -type d -name 'snapshot-[0-9]*' -printf '%f\n' | sort -r)
if [ "${#snapshots[@]}" -gt "$retention" ]; then
  resolved_root="$(readlink -f -- "$backup_root")"
  for old_name in "${snapshots[@]:$retention}"; do
    candidate="$backup_root/$old_name"
    resolved_candidate="$(readlink -f -- "$candidate")"
    case "$resolved_candidate" in
      "$resolved_root"/snapshot-[0-9]*) rm -rf -- "$resolved_candidate" ;;
      *) printf 'refusing to prune unexpected path: %s\n' "$resolved_candidate" >&2; exit 1 ;;
    esac
  done
fi

printf 'backup_status=ok snapshot=%s retention=%s\n' "$snapshot" "$retention"
