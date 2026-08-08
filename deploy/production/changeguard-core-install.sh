#!/usr/bin/env bash
set -euo pipefail
umask 022

usage() {
  printf 'usage: %s ARCHIVE EXPECTED_ARCHIVE_SHA256 RELEASE_ROOT RELEASE_ID\n' "$0" >&2
  exit 64
}

fail() {
  printf 'core_install_error=%s\n' "$1" >&2
  exit 1
}

[ "$#" -eq 4 ] || usage
archive="$1"
expected_archive_sha256="$2"
release_root="$3"
release_id="$4"

[ "$(id -u)" -eq 0 ] || fail "installer_must_run_as_root"
case "$archive" in
  /*) ;;
  *) fail "archive_path_must_be_absolute" ;;
esac
[ -f "$archive" ] || fail "archive_missing"
[ ! -L "$archive" ] || fail "archive_must_not_be_symlink"
[[ "$expected_archive_sha256" =~ ^[0-9a-f]{64}$ ]] || fail "expected_archive_sha256_invalid"
case "$release_root" in
  /*) ;;
  *) fail "release_root_must_be_absolute" ;;
esac
[[ "$release_id" =~ ^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$ ]] || fail "release_id_invalid"
[ ! -L "$release_root" ] || fail "release_root_must_not_be_symlink"
resolved_release_candidate="$(readlink -m -- "$release_root")"
case "$resolved_release_candidate" in
  /|/opt|/opt/changeguard|/etc|/usr|/var) fail "release_root_is_unsafe" ;;
esac
install -d -m 0755 -- "$resolved_release_candidate"
resolved_release_root="$(readlink -f -- "$resolved_release_candidate")"
[ "$(stat -c %u -- "$resolved_release_root")" -eq 0 ] || fail "release_root_not_owned_by_root"
release_root_mode="$(stat -c %a -- "$resolved_release_root")"
if (( (8#$release_root_mode & 022) != 0 )); then
  fail "release_root_is_group_or_world_writable"
fi
target="$resolved_release_root/$release_id"
[ ! -e "$target" ] || fail "release_target_exists"

actual_archive_sha256="$(sha256sum "$archive" | awk '{print $1}')"
[ "$actual_archive_sha256" = "$expected_archive_sha256" ] || fail "archive_sha256_mismatch"

python3 - "$archive" "$release_id" <<'PY'
import pathlib
import re
import sys
import tarfile

archive = pathlib.Path(sys.argv[1])
release_id = sys.argv[2]
required = {
    f"{release_id}/dbguard",
    f"{release_id}/SHA256SUMS",
    f"{release_id}/release-manifest.json",
    f"{release_id}/verification.json",
    f"{release_id}/source.bundle",
    f"{release_id}/source.tar.gz",
    f"{release_id}/modules.txt",
    f"{release_id}/module-verify.txt",
    f"{release_id}/binary-buildinfo.txt",
    f"{release_id}/bundle-verify.txt",
    f"{release_id}/build.log",
}
seen = set()
total_size = 0
with tarfile.open(archive, mode="r:gz") as handle:
    members = handle.getmembers()
    if not members or len(members) > 256:
        raise SystemExit("archive member count is invalid")
    for member in members:
        name = member.name
        if not name or "\\" in name or any(ord(char) < 32 or ord(char) == 127 for char in name):
            raise SystemExit("archive contains an unsafe member name")
        pure = pathlib.PurePosixPath(name)
        if pure.is_absolute() or not pure.parts or pure.parts[0] != release_id:
            raise SystemExit("archive member is outside release directory")
        if any(part in {"", ".", ".."} for part in pure.parts):
            raise SystemExit("archive member contains path traversal")
        normalized = pure.as_posix()
        if normalized in seen:
            raise SystemExit("archive contains duplicate members")
        seen.add(normalized)
        if member.issym() or member.islnk() or member.isdev() or member.isfifo():
            raise SystemExit("archive contains links or special files")
        if not (member.isfile() or member.isdir()):
            raise SystemExit("archive contains unsupported member type")
        if member.isfile():
            total_size += member.size
            if member.size > 512 * 1024 * 1024:
                raise SystemExit("archive member is too large")
    if total_size > 1024 * 1024 * 1024:
        raise SystemExit("archive expands beyond the allowed size")
if not required.issubset(seen):
    raise SystemExit("archive is missing required release members")
PY

staging="$resolved_release_root/.install-$release_id-$$"
[ ! -e "$staging" ] || fail "installer_staging_exists"
install -d -m 0700 -- "$staging"

cleanup() {
  if [ -n "${staging:-}" ] && [ -d "$staging" ]; then
    resolved_staging="$(readlink -f -- "$staging" 2>/dev/null || true)"
    case "$resolved_staging" in
      "$resolved_release_root"/.install-"$release_id"-*) rm -rf -- "$resolved_staging" ;;
    esac
  fi
}
trap cleanup EXIT

tar --extract --gzip --file "$archive" --directory "$staging" --no-same-owner --no-same-permissions
extracted="$staging/$release_id"
[ -d "$extracted" ] || fail "extracted_release_missing"
[ ! -L "$extracted" ] || fail "extracted_release_is_symlink"
if find "$extracted" -type l -print -quit | grep -q .; then
  fail "extracted_release_contains_symlink"
fi

chown -R root:root -- "$extracted"
find "$extracted" -type d -exec chmod 0755 {} +
find "$extracted" -type f -exec chmod 0644 {} +
chmod 0755 "$extracted/dbguard"

python3 - "$extracted" <<'PY'
import json
import pathlib
import re
import sys

root = pathlib.Path(sys.argv[1])
checksum_path = root / "SHA256SUMS"
expected = set()
for number, line in enumerate(checksum_path.read_text(encoding="utf-8").splitlines(), 1):
    if len(line) < 67 or not re.fullmatch(r"[0-9a-f]{64}", line[:64]) or line[64:66] not in {"  ", " *"}:
        raise SystemExit(f"invalid release checksum line {number}")
    relative = line[66:]
    pure = pathlib.PurePosixPath(relative)
    if pure.is_absolute() or not pure.parts or any(part in {"", ".", ".."} for part in pure.parts):
        raise SystemExit(f"unsafe release checksum path at line {number}")
    normalized = pure.as_posix()
    if normalized == "SHA256SUMS" or normalized in expected:
        raise SystemExit(f"duplicate or self-referential release checksum path at line {number}")
    expected.add(normalized)
actual = {
    path.relative_to(root).as_posix()
    for path in root.rglob("*")
    if path.is_file() and path.name != "SHA256SUMS"
}
if expected != actual:
    raise SystemExit("release checksum coverage mismatch")

manifest = json.loads((root / "release-manifest.json").read_text(encoding="utf-8"))
verification = json.loads((root / "verification.json").read_text(encoding="utf-8"))
if manifest.get("schema") != "changeguard-core-release/v2":
    raise SystemExit("unexpected release manifest schema")
if verification.get("schema") != "changeguard-core-verification/v1" or verification.get("status") != "passed":
    raise SystemExit("release verification evidence is not passed")
for field in ("version", "tag", "commit", "source_sha256"):
    if not manifest.get(field) or manifest.get(field) != verification.get(field):
        raise SystemExit(f"release identity mismatch: {field}")
artifact = manifest.get("files", {}).get("dbguard", "")
if not re.fullmatch(r"[0-9a-f]{64}", artifact):
    raise SystemExit("release manifest artifact digest is invalid")
PY
(
  cd "$extracted"
  sha256sum -c SHA256SUMS >/dev/null
)
artifact_sha256="$(sha256sum "$extracted/dbguard" | awk '{print $1}')"
manifest_artifact="$(python3 -c 'import json,sys; print(json.load(open(sys.argv[1], encoding="utf-8"))["files"]["dbguard"])' "$extracted/release-manifest.json")"
[ "$artifact_sha256" = "$manifest_artifact" ] || fail "installed_artifact_sha256_mismatch"
if find "$extracted" -perm /022 -print -quit | grep -q .; then
  fail "installed_release_is_group_or_world_writable"
fi

mv -- "$extracted" "$target"
rmdir -- "$staging"
staging=""
[ "$(stat -c %a -- "$target")" = "755" ] || fail "installed_release_directory_mode_invalid"
[ "$(stat -c %a -- "$target/dbguard")" = "755" ] || fail "installed_binary_mode_invalid"
if find "$target" -perm /022 -print -quit | grep -q .; then
  fail "final_release_is_group_or_world_writable"
fi

python3 - "$target/release-manifest.json" "$target" "$actual_archive_sha256" "$artifact_sha256" <<'PY'
import json
import sys

manifest = json.load(open(sys.argv[1], encoding="utf-8"))
print(
    "core_install=passed"
    f" release={sys.argv[2]}"
    f" version={manifest['version']}"
    f" commit={manifest['commit']}"
    f" archive_sha256={sys.argv[3]}"
    f" artifact_sha256={sys.argv[4]}"
)
PY
