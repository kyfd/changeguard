#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 6 ]; then
  printf 'usage: %s ENV_FILE RELEASE_LINK DATA_FILE WITNESS_FILE EXPECTED_USER RELEASE_ROOT\n' "$0" >&2
  exit 64
fi

env_file="$1"
release_link="$2"
data_file="$3"
witness_file="$4"
expected_user="$5"
release_root="$6"

fail() {
  printf 'core_preflight=failed check=%s\n' "$1" >&2
  exit 1
}

for path in "$env_file" "$release_link" "$data_file" "$witness_file" "$release_root"; do
  case "$path" in
    /*) ;;
    *) fail absolute_paths_required ;;
  esac
done

actual_uid="$(id -u)"
actual_user="$(id -un)"
[ "$actual_uid" -ne 0 ] || fail runtime_user_must_not_be_root
[ "$actual_user" = "$expected_user" ] || fail unexpected_runtime_user

[ -f "$env_file" ] || fail canonical_env_missing
[ ! -L "$env_file" ] || fail canonical_env_must_not_be_symlink
[ -r "$env_file" ] || fail canonical_env_unreadable
[ ! -w "$env_file" ] || fail canonical_env_writable_by_runtime_user

resolved_root="$(readlink -f -- "$release_root")"
resolved_release="$(readlink -f -- "$release_link")"
[ -d "$resolved_root" ] || fail release_root_missing
[ -d "$resolved_release" ] || fail release_missing
case "$resolved_release" in
  "$resolved_root"/*) ;;
  *) fail release_outside_release_root ;;
esac
[ ! -w "$resolved_release" ] || fail release_writable_by_runtime_user

for required in dbguard SHA256SUMS release-manifest.json verification.json; do
  [ -f "$resolved_release/$required" ] || fail "release_file_${required//[^A-Za-z0-9]/_}_missing"
  [ -r "$resolved_release/$required" ] || fail "release_file_${required//[^A-Za-z0-9]/_}_unreadable"
done
[ -x "$resolved_release/dbguard" ] || fail core_binary_not_executable
[ ! -w "$resolved_release/dbguard" ] || fail core_binary_writable_by_runtime_user
(
  cd "$resolved_release"
  sha256sum -c SHA256SUMS >/dev/null
) || fail release_sha256_mismatch

data_dir="$(dirname -- "$data_file")"
[ -d "$data_dir" ] || fail data_directory_missing
[ ! -L "$data_dir" ] || fail data_directory_must_not_be_symlink
[ -r "$data_dir" ] && [ -w "$data_dir" ] && [ -x "$data_dir" ] || fail data_directory_permissions
if [ -e "$data_file" ]; then
  [ -f "$data_file" ] && [ ! -L "$data_file" ] || fail data_file_type
  [ -r "$data_file" ] && [ -w "$data_file" ] || fail data_file_permissions
fi

marker_file="$witness_file.required"
witness_exists=0
marker_exists=0
[ -f "$witness_file" ] && witness_exists=1
[ -f "$marker_file" ] && marker_exists=1
[ "$witness_exists" -eq "$marker_exists" ] || fail migration_witness_pair_incomplete
if [ "$witness_exists" -eq 1 ]; then
  [ ! -L "$witness_file" ] && [ ! -L "$marker_file" ] || fail migration_witness_symlink
  [ -r "$witness_file" ] && [ -w "$witness_file" ] || fail migration_witness_permissions
  [ -r "$marker_file" ] && [ -w "$marker_file" ] || fail migration_marker_permissions
  [ "$(cat -- "$marker_file")" = "changeguard-migration-witness-required/v1" ] || fail migration_marker_invalid
fi

printf 'core_preflight=passed user=%s release=%s data_file=%s witness_pair=%s\n' \
  "$actual_user" "$resolved_release" "$data_file" "$witness_exists"
