#!/usr/bin/env bash
set -euo pipefail

if [ "$#" -ne 1 ]; then
  printf 'usage: %s check|apply\n' "$0" >&2
  exit 64
fi

action="$1"
service_user="${CHANGEGUARD_SERVICE_USER:-changeguard}"
service_group="${CHANGEGUARD_SERVICE_GROUP:-changeguard}"
script_directory="$(cd "$(dirname "$0")" && pwd)"
sysusers_source="${CHANGEGUARD_SYSUSERS_FILE:-$script_directory/changeguard.sysusers.conf}"
sysusers_destination="${CHANGEGUARD_SYSUSERS_DESTINATION:-/etc/sysusers.d/changeguard.conf}"
environment_directory="${CHANGEGUARD_ENV_DIRECTORY:-/etc/changeguard}"
libexec_directory="${CHANGEGUARD_LIBEXEC_DIRECTORY:-/usr/local/libexec/changeguard}"

fail() {
  printf 'core_host_prepare=failed check=%s\n' "$1" >&2
  exit 1
}

case "$action" in
  check|apply) ;;
  *) printf 'usage: %s check|apply\n' "$0" >&2; exit 64 ;;
esac

for path in "$sysusers_source" "$sysusers_destination" "$environment_directory" "$libexec_directory"; do
  case "$path" in
    /*) ;;
    *) fail absolute_paths_required ;;
  esac
done

check_identity() {
  getent passwd "$service_user" >/dev/null || fail service_user_missing
  getent group "$service_group" >/dev/null || fail service_group_missing
  passwd_entry="$(getent passwd "$service_user")"
  user_uid="$(printf '%s' "$passwd_entry" | cut -d: -f3)"
  user_gid="$(printf '%s' "$passwd_entry" | cut -d: -f4)"
  user_home="$(printf '%s' "$passwd_entry" | cut -d: -f6)"
  user_shell="$(printf '%s' "$passwd_entry" | cut -d: -f7)"
  group_gid="$(getent group "$service_group" | cut -d: -f3)"
  [ "$user_uid" -ne 0 ] || fail service_user_must_not_be_root
  [ "$user_gid" = "$group_gid" ] || fail service_user_primary_group_mismatch
  [ "$user_home" = /opt/changeguard/data ] || fail service_user_home_mismatch
  case "$user_shell" in
    */nologin|*/false) ;;
    *) fail service_user_shell_is_interactive ;;
  esac
}

check_directory() {
  path="$1"
  expected_owner="$2"
  expected_group="$3"
  expected_mode="$4"
  check_name="$5"
  [ -d "$path" ] && [ ! -L "$path" ] || fail "${check_name}_directory_invalid"
  actual="$(stat -c '%U:%G:%a' "$path")"
  [ "$actual" = "$expected_owner:$expected_group:$expected_mode" ] || fail "${check_name}_directory_metadata"
}

check_prepared() {
  check_identity
  [ -f "$sysusers_destination" ] && [ ! -L "$sysusers_destination" ] || fail sysusers_destination_invalid
  [ "$(stat -c '%U:%G:%a' "$sysusers_destination")" = root:root:644 ] || fail sysusers_destination_metadata
  check_directory "$environment_directory" root "$service_group" 750 environment
  check_directory "$libexec_directory" root root 755 libexec
  printf 'core_host_prepare=passed user=%s group=%s environment_directory=%s libexec_directory=%s production_data_untouched=true service_untouched=true\n' \
    "$service_user" "$service_group" "$environment_directory" "$libexec_directory"
}

if [ "$action" = check ]; then
  check_prepared
  exit 0
fi

[ "$(id -u)" -eq 0 ] || fail installer_must_run_as_root
[ -f "$sysusers_source" ] && [ ! -L "$sysusers_source" ] || fail sysusers_source_invalid
source_mode="$(stat -c '%a' "$sysusers_source")"
if [ $((8#$source_mode & 8#022)) -ne 0 ]; then
  fail sysusers_source_is_group_or_world_writable
fi
grep -Eq "^u[[:space:]]+$service_user[[:space:]]" "$sysusers_source" || fail sysusers_source_identity_mismatch

install -D -o root -g root -m 0644 -- "$sysusers_source" "$sysusers_destination"
systemd-sysusers "$sysusers_destination" >/dev/null
check_identity

if [ -e "$environment_directory" ]; then
  [ -d "$environment_directory" ] && [ ! -L "$environment_directory" ] || fail environment_directory_invalid
  [ "$(stat -c '%U:%G' "$environment_directory")" = "root:$service_group" ] || fail environment_directory_owner_mismatch
else
  install -d -o root -g "$service_group" -m 0750 -- "$environment_directory"
fi
chmod 0750 -- "$environment_directory"

if [ -e "$libexec_directory" ]; then
  [ -d "$libexec_directory" ] && [ ! -L "$libexec_directory" ] || fail libexec_directory_invalid
  [ "$(stat -c '%U:%G' "$libexec_directory")" = root:root ] || fail libexec_directory_owner_mismatch
else
  install -d -o root -g root -m 0755 -- "$libexec_directory"
fi
chmod 0755 -- "$libexec_directory"

check_prepared
