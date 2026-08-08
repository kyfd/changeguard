#!/usr/bin/env bash
set -euo pipefail

usage() {
  printf 'usage: %s verify SNAPSHOT | %s stage SNAPSHOT DESTINATION\n' "$0" "$0" >&2
  exit 64
}

fail() {
  printf 'restore_error=%s\n' "$1" >&2
  exit 1
}

[ "$#" -ge 2 ] || usage
mode="$1"
snapshot_input="$2"
destination_input="${3:-}"
backup_root="${CHANGEGUARD_BACKUP_DIR:-/opt/changeguard/backups}"
restore_root="${CHANGEGUARD_RESTORE_ROOT:-/opt/changeguard/restore-staging}"

case "$mode" in
  verify) [ "$#" -eq 2 ] || usage ;;
  stage) [ "$#" -eq 3 ] || usage ;;
  *) usage ;;
esac
case "$backup_root" in
  /*) ;;
  *) fail "backup_root_must_be_absolute" ;;
esac
[ -d "$backup_root" ] || fail "backup_root_missing"
[ ! -L "$backup_root" ] || fail "backup_root_must_not_be_symlink"
resolved_backup_root="$(readlink -f -- "$backup_root")"
[ -d "$snapshot_input" ] || fail "snapshot_missing"
[ ! -L "$snapshot_input" ] || fail "snapshot_must_not_be_symlink"
resolved_snapshot="$(readlink -f -- "$snapshot_input")"
snapshot_name="$(basename -- "$resolved_snapshot")"
[[ "$snapshot_name" =~ ^snapshot-[0-9]{8}-[0-9]{6}$ ]] || fail "snapshot_name_invalid"
case "$resolved_snapshot" in
  "$resolved_backup_root"/snapshot-[0-9]*) ;;
  *) fail "snapshot_outside_backup_root" ;;
esac

assert_private_file() {
  local path="$1"
  local label="$2"
  local mode_value
  [ -f "$path" ] || fail "${label}_missing"
  [ ! -L "$path" ] || fail "${label}_must_not_be_symlink"
  mode_value="$(stat -c %a -- "$path")"
  if (( (8#$mode_value & 0400) == 0 )); then
    fail "${label}_not_owner_readable"
  fi
  if (( (8#$mode_value & 077) != 0 )); then
    fail "${label}_permissions_too_broad"
  fi
}

validate_snapshot_content() {
  local root="$1"
  local summary
  [ -f "$root/manifest.sha256" ] || fail "manifest_missing"
  [ -f "$root/metadata.json" ] || fail "metadata_missing"
  if find "$root" -type l -print -quit | grep -q .; then
    fail "snapshot_contains_symlink"
  fi
  if find "$root" -perm /077 -print -quit | grep -q .; then
    fail "snapshot_permissions_too_broad"
  fi
  python3 - "$root" <<'PY'
import pathlib
import re
import sys

root = pathlib.Path(sys.argv[1])
manifest_path = root / "manifest.sha256"
expected = set()
for number, line in enumerate(manifest_path.read_text(encoding="utf-8").splitlines(), 1):
    if len(line) < 68 or not re.fullmatch(r"[0-9a-f]{64}", line[:64]) or line[64:66] not in {"  ", " *"}:
        raise SystemExit(f"invalid manifest line {number}")
    relative = line[66:]
    if not relative.startswith("./"):
        raise SystemExit(f"manifest path is not relative at line {number}")
    pure = pathlib.PurePosixPath(relative[2:])
    if pure.is_absolute() or not pure.parts or any(part in {"", ".", ".."} for part in pure.parts):
        raise SystemExit(f"unsafe manifest path at line {number}")
    normalized = "./" + pure.as_posix()
    if normalized == "./manifest.sha256" or normalized in expected:
        raise SystemExit(f"duplicate or self-referential manifest path at line {number}")
    expected.add(normalized)
actual = {
    "./" + path.relative_to(root).as_posix()
    for path in root.rglob("*")
    if path.is_file() and path.name != "manifest.sha256"
}
if expected != actual:
    missing = sorted(actual - expected)
    extra = sorted(expected - actual)
    raise SystemExit(f"manifest coverage mismatch missing={missing} extra={extra}")
PY
  (
    cd "$root"
    sha256sum -c manifest.sha256 >/dev/null
  )
  assert_private_file "$root/core/changeguard.env" "core_environment"
  if [ -f "$root/agent/changeguard-agent-gateway.env" ]; then
    assert_private_file "$root/agent/changeguard-agent-gateway.env" "agent_environment"
  fi
  summary="$(python3 - "$root" <<'PY'
import hashlib
import json
import pathlib
import re
import sys

root = pathlib.Path(sys.argv[1])

def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()

metadata = json.loads((root / "metadata.json").read_text(encoding="utf-8"))
if metadata.get("schema") != "changeguard-backup/v2":
    raise SystemExit("invalid backup metadata schema")
if metadata.get("scope", "full") != "full":
    raise SystemExit("unsupported backup scope")
if not re.fullmatch(r"[0-9]{8}-[0-9]{6}", str(metadata.get("stamp", ""))):
    raise SystemExit("invalid backup metadata stamp")

required = (
    "core/changeguard.env",
    "core/dbguard.json",
    "agent/changeguard-agent-gateway.env",
    "agent/audit.jsonl",
    "agent/metrics.json",
    "agent/ready.json",
    "agent/slo.json",
    "config/liufengxi.top.conf",
    "config/changeguard-agent-gateway.service",
    "config/agent-release.txt",
    "config/ui-release.txt",
    "config/core-release.txt",
)
for relative in required:
    path = root / relative
    if not path.is_file() or path.is_symlink():
        raise SystemExit(f"required backup member missing: {relative}")

data_path = root / "core/dbguard.json"
json.loads(data_path.read_text(encoding="utf-8"))
witness_path = root / "core/dbguard.rollback-witness.json"
marker_path = root / "core/dbguard.rollback-witness.required"
witness_exists = witness_path.is_file()
marker_exists = marker_path.is_file()
if witness_exists != marker_exists:
    raise SystemExit("migration witness pair is incomplete")

witness_sha256 = ""
marker_sha256 = ""
if witness_exists:
    if marker_path.read_text(encoding="utf-8") != "changeguard-migration-witness-required/v1\n":
        raise SystemExit("invalid migration witness marker")
    document = json.loads(witness_path.read_text(encoding="utf-8"))
    if document.get("schema") != "changeguard-migration-witness/v1":
        raise SystemExit("invalid migration witness schema")
    payload = hashlib.sha256()
    def field(value):
        encoded = str(value).encode("utf-8")
        payload.update(str(len(encoded)).encode("ascii") + b":" + encoded + b"\n")
    def snapshot(label, value):
        field(label)
        field(value["state_sha256"])
        field(len(value["changes"]))
        for entry in value["changes"]:
            field(entry["key"])
            field(entry["sql_sha256"])
            field(entry["rollback_sha256"])
            field(entry["artifact_sha256"])
        field(len(value["artifacts"]))
        for entry in value["artifacts"]:
            field(entry["key"])
            field(entry["artifact_id"])
            field(entry["content_sha256"])
    field(document["schema"])
    snapshot("current", document["current"])
    if document.get("previous") is None:
        field("previous:none")
    else:
        snapshot("previous", document["previous"])
    if payload.hexdigest() != document.get("payload_sha256"):
        raise SystemExit("migration witness payload digest mismatch")
    data_sha256 = digest(data_path)
    candidate_states = {document["current"]["state_sha256"]}
    if document.get("previous") is not None:
        candidate_states.add(document["previous"]["state_sha256"])
    if data_sha256 not in candidate_states:
        raise SystemExit("data file is not paired with the migration witness")
    witness_sha256 = digest(witness_path)
    marker_sha256 = digest(marker_path)

expected_sequence = 1
previous_hash = ""
audit_records = 0
for number, line in enumerate((root / "agent/audit.jsonl").read_text(encoding="utf-8").splitlines(), 1):
    if not line.strip():
        continue
    record = json.loads(line)
    if record.get("sequence") != expected_sequence:
        raise SystemExit(f"audit sequence mismatch at line {number}")
    if record.get("prev_hash", "") != previous_hash:
        raise SystemExit(f"audit previous hash mismatch at line {number}")
    previous_hash = record.get("hash", "")
    if not previous_hash:
        raise SystemExit(f"audit hash missing at line {number}")
    expected_sequence += 1
    audit_records += 1

metrics = json.loads((root / "agent/metrics.json").read_text(encoding="utf-8"))
if metrics.get("schema") != "changeguard-agent-metrics/v1" or not metrics.get("hmac_sha256"):
    raise SystemExit("invalid agent metrics checkpoint")
for relative in ("agent/ready.json", "agent/slo.json"):
    json.loads((root / relative).read_text(encoding="utf-8"))

data_sha256 = digest(data_path)
environment_sha256 = digest(root / "core/changeguard.env")
core_metadata = metadata.get("core", {})
checks = {
    "data_sha256": data_sha256,
    "environment_sha256": environment_sha256,
    "migration_witness_present": witness_exists,
    "migration_witness_sha256": witness_sha256,
    "migration_marker_sha256": marker_sha256,
}
for key, value in checks.items():
    if key in core_metadata and core_metadata[key] != value:
        raise SystemExit(f"backup metadata core digest mismatch: {key}")

summary = {
    "schema": "changeguard-restore-verification/v1",
    "snapshot": "snapshot-" + str(metadata.get("stamp", "")),
    "manifest_sha256": digest(root / "manifest.sha256"),
    "core_data_sha256": data_sha256,
    "core_environment_sha256": environment_sha256,
    "migration_witness_present": witness_exists,
    "migration_witness_sha256": witness_sha256,
    "migration_marker_sha256": marker_sha256,
    "audit_records": audit_records,
    "metrics_checkpoint_verified": True,
    "core_release": metadata.get("core_release", ""),
    "agent_release": metadata.get("agent_release", ""),
    "ui_release": metadata.get("ui_release", ""),
}
print(json.dumps(summary, ensure_ascii=False, sort_keys=True, separators=(",", ":")))
PY
)"
  printf '%s' "$summary"
}

snapshot_summary="$(validate_snapshot_content "$resolved_snapshot")"
summary_snapshot="$(RESTORE_SUMMARY="$snapshot_summary" python3 -c 'import json,os; print(json.loads(os.environ["RESTORE_SUMMARY"])["snapshot"])')"
[ "$summary_snapshot" = "$snapshot_name" ] || fail "snapshot_metadata_name_mismatch"

print_status() {
  local status="$1"
  local destination="${2:-}"
  RESTORE_SUMMARY="$snapshot_summary" RESTORE_STATUS="$status" RESTORE_DESTINATION="$destination" python3 - <<'PY'
import json
import os

summary = json.loads(os.environ["RESTORE_SUMMARY"])
parts = [
    f"restore_status={os.environ['RESTORE_STATUS']}",
    f"snapshot={summary['snapshot']}",
    f"core_data_sha256={summary['core_data_sha256']}",
    f"witness_pair={1 if summary['migration_witness_present'] else 0}",
    f"audit_records={summary['audit_records']}",
]
if os.environ.get("RESTORE_DESTINATION"):
    parts.append(f"destination={os.environ['RESTORE_DESTINATION']}")
print(" ".join(parts))
PY
}

if [ "$mode" = "verify" ]; then
  print_status "verified"
  exit 0
fi

case "$restore_root" in
  /*) ;;
  *) fail "restore_root_must_be_absolute" ;;
esac
[ ! -L "$restore_root" ] || fail "restore_root_must_not_be_symlink"
resolved_restore_candidate="$(readlink -m -- "$restore_root")"
case "$resolved_restore_candidate" in
  /|/opt|/opt/changeguard|/opt/changeguard/data|/etc|/usr|/var) fail "restore_root_is_unsafe" ;;
esac
install -d -m 0700 -- "$resolved_restore_candidate"
resolved_restore_root="$(readlink -f -- "$resolved_restore_candidate")"
case "$destination_input" in
  /*) ;;
  *) fail "restore_destination_must_be_absolute" ;;
esac
resolved_destination="$(readlink -m -- "$destination_input")"
destination_name="$(basename -- "$resolved_destination")"
[[ "$destination_name" =~ ^restore-[A-Za-z0-9][A-Za-z0-9._-]*$ ]] || fail "restore_destination_name_invalid"
[ "$(dirname -- "$resolved_destination")" = "$resolved_restore_root" ] || fail "restore_destination_outside_restore_root"
[ ! -e "$resolved_destination" ] || fail "restore_destination_exists"
staging="$resolved_restore_root/.staging-$destination_name-$$"
[ ! -e "$staging" ] || fail "restore_staging_exists"

cleanup() {
  if [ -n "${staging:-}" ] && [ -d "$staging" ]; then
    resolved_staging="$(readlink -f -- "$staging" 2>/dev/null || true)"
    case "$resolved_staging" in
      "$resolved_restore_root"/.staging-restore-*) rm -rf -- "$resolved_staging" ;;
    esac
  fi
}
trap cleanup EXIT

install -d -m 0700 -- "$staging"
cp -a -- "$resolved_snapshot/." "$staging/"
staged_summary="$(validate_snapshot_content "$staging")"
[ "$staged_summary" = "$snapshot_summary" ] || fail "staged_restore_summary_mismatch"
RESTORE_SUMMARY="$staged_summary" RESTORE_SOURCE="$snapshot_name" RESTORE_OUTPUT="$staging/restore-verification.json" python3 - <<'PY'
import datetime
import json
import os
import pathlib

summary = json.loads(os.environ["RESTORE_SUMMARY"])
report = {
    "schema": "changeguard-restore/v1",
    "status": "staged",
    "source_snapshot": os.environ["RESTORE_SOURCE"],
    "staged_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
    "verification": summary,
}
pathlib.Path(os.environ["RESTORE_OUTPUT"]).write_text(
    json.dumps(report, ensure_ascii=False, indent=2, sort_keys=True) + "\n",
    encoding="utf-8",
)
PY
(
  cd "$staging"
  find . -type f ! -name restore-manifest.sha256 -print0 | sort -z | xargs -0 sha256sum > restore-manifest.sha256
  sha256sum -c restore-manifest.sha256 >/dev/null
)
chmod -R go-rwx -- "$staging"
mv -- "$staging" "$resolved_destination"
staging=""
snapshot_summary="$staged_summary"
print_status "staged" "$resolved_destination"
