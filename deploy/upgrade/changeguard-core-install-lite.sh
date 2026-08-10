#!/usr/bin/env bash
# ChangeGuard release 原子安装（精简版，适配独立部署/新服务器）
# 用法: changeguard-core-install-lite.sh ARCHIVE EXPECTED_SHA256 RELEASE_ROOT RELEASE_ID
set -euo pipefail
umask 022

usage() { printf 'usage: %s ARCHIVE EXPECTED_ARCHIVE_SHA256 RELEASE_ROOT RELEASE_ID\n' "$0" >&2; exit 64; }
fail() { printf 'core_install_error=%s\n' "$1" >&2; exit 1; }

[ "$#" -eq 4 ] || usage
archive="$1"; expected_sha256="$2"; release_root="$3"; release_id="$4"

[ "$(id -u)" -eq 0 ] || fail "installer_must_run_as_root"
[ -f "$archive" ] || fail "archive_missing"
[[ "$expected_sha256" =~ ^[0-9a-f]{64}$ ]] || fail "expected_archive_sha256_invalid"
[[ "$release_id" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$ ]] || fail "release_id_invalid"

actual_sha256="$(sha256sum "$archive" | awk '{print $1}')"
[ "$actual_sha256" = "$expected_sha256" ] || fail "archive_sha256_mismatch"

install -d -m 0755 "$release_root"
target="$release_root/$release_id"
[ ! -e "$target" ] || fail "release_target_exists"

python3 - "$archive" "$release_id" <<'PY'
import pathlib, sys, tarfile
archive, release_id = sys.argv[1], sys.argv[2]
required = {f"{release_id}/dbguard", f"{release_id}/SHA256SUMS", f"{release_id}/release-manifest.json", f"{release_id}/verification.json"}
seen, total = set(), 0
with tarfile.open(archive, mode="r:gz") as handle:
    members = handle.getmembers()
    if not members or len(members) > 256:
        raise SystemExit("archive member count is invalid")
    for member in members:
        name = member.name
        if not name or "\\" in name or any(ord(c) < 32 or ord(c) == 127 for c in name):
            raise SystemExit("unsafe member name")
        pure = pathlib.PurePosixPath(name)
        if pure.is_absolute() or not pure.parts or pure.parts[0] != release_id:
            raise SystemExit("member outside release directory")
        if any(part in {"", ".", ".."} for part in pure.parts):
            raise SystemExit("path traversal")
        if name in seen:
            raise SystemExit("duplicate member")
        seen.add(name)
        if member.issym() or member.islnk() or member.isdev() or member.isfifo():
            raise SystemExit("links or special files not allowed")
        if not (member.isfile() or member.isdir()):
            raise SystemExit("unsupported member type")
        if member.isfile():
            total += member.size
            if member.size > 512 * 1024 * 1024:
                raise SystemExit("member too large")
    if total > 1024 * 1024 * 1024:
        raise SystemExit("archive expands too large")
if not required.issubset(seen):
    raise SystemExit("archive is missing required release members")
PY

staging="$release_root/.install-$release_id-$$"
install -d -m 0700 "$staging"
trap 'rm -rf "$staging"' EXIT
tar --extract --gzip --file "$archive" --directory "$staging" --no-same-owner --no-same-permissions
extracted="$staging/$release_id"
[ -d "$extracted" ] || fail "extracted_release_missing"

chown -R root:root "$extracted"
find "$extracted" -type d -exec chmod 0755 {} +
find "$extracted" -type f -exec chmod 0644 {} +
chmod 0755 "$extracted/dbguard"

python3 - "$extracted" <<'PY'
import json, pathlib, re, sys
root = pathlib.Path(sys.argv[1])
lines = (root / "SHA256SUMS").read_text(encoding="utf-8").splitlines()
for line in lines:
    if len(line) < 67 or not re.fullmatch(r"[0-9a-f]{64}", line[:64]) or line[64:66] not in {"  ", " *"}:
        raise SystemExit("invalid checksum line")
manifest = json.loads((root / "release-manifest.json").read_text(encoding="utf-8"))
verification = json.loads((root / "verification.json").read_text(encoding="utf-8"))
if manifest.get("schema") != "changeguard-core-release/v2":
    raise SystemExit("unexpected manifest schema")
if verification.get("schema") != "changeguard-core-verification/v1" or verification.get("status") != "passed":
    raise SystemExit("verification not passed")
for field in ("version", "tag", "commit"):
    if manifest.get(field) != verification.get(field):
        raise SystemExit(f"identity mismatch: {field}")
artifact = manifest.get("files", {}).get("dbguard", "")
if not re.fullmatch(r"[0-9a-f]{64}", artifact):
    raise SystemExit("invalid artifact digest")
PY

( cd "$extracted" && sha256sum -c SHA256SUMS >/dev/null )
artifact_sha="$(sha256sum "$extracted/dbguard" | awk '{print $1}')"
manifest_artifact="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1]))["files"]["dbguard"])' "$extracted/release-manifest.json")"
[ "$artifact_sha" = "$manifest_artifact" ] || fail "installed_artifact_sha256_mismatch"

mv "$extracted" "$target"
rmdir "$staging" 2>/dev/null || true
trap - EXIT
echo "core_install=passed release=$target"
