#!/usr/bin/env bash
# ChangeGuard 4.0 environment migration: DBGUARD_* -> CHANGEGUARD_*.
#
# 4.0 removed the legacy prefix aliases: a host whose environment file still
# carries DBGUARD_* keys fails closed before any component initializes. This
# script rewrites those keys in place and leaves comments, order, blank lines
# and values untouched.
#
# Guarantees:
#   - read-only unless a migration is actually required (--dry-run never writes);
#   - the original file is copied to <file><backup-suffix> before the first
#     write, and an existing backup is never overwritten;
#   - a canonical key that already exists with a different value fails closed
#     instead of silently winning or losing;
#   - values are never printed: output names keys only;
#   - the file is rewritten in place, so owner and mode are preserved.
#
# Usage: see --help. Exit codes: 0=no migration needed or migrated,
# 3=--dry-run found keys to migrate, 1=failed, 64=usage error.
set -euo pipefail

legacy_prefix="DBGUARD_"
canonical_prefix="CHANGEGUARD_"
max_bytes=$((4 * 1024 * 1024))

usage() {
  cat <<'USAGE'
ChangeGuard 4.0 environment migration (DBGUARD_* -> CHANGEGUARD_*).

Usage:
  changeguard-migrate-env.sh --file /etc/changeguard/core.env [--dry-run]
  changeguard-migrate-env.sh /etc/changeguard/core.env

Options:
  --file PATH            Environment file to migrate (absolute path recommended)
  --dry-run              Report the keys that would change, write nothing
  --backup-suffix SUFFIX Suffix for the pre-migration copy (default .pre-4.0)
  --help                 Print this usage

Exit codes: 0=already canonical or migrated, 3=--dry-run found keys,
1=failed, 64=usage error.
USAGE
}

fail() { printf 'migrate_error=%s\n' "$*" >&2; exit 1; }
usage_error() { printf 'migrate_error=%s\n' "$*" >&2; usage >&2; exit 64; }

file=""
dry_run=0
backup_suffix=".pre-4.0"

while [ $# -gt 0 ]; do
  case "$1" in
    --help|-h) usage; exit 0 ;;
    --dry-run) dry_run=1; shift ;;
    --file)
      [ $# -ge 2 ] || usage_error "--file needs a path"
      [ -z "$file" ] || usage_error "--file given more than once"
      file="$2"; shift 2 ;;
    --backup-suffix)
      [ $# -ge 2 ] || usage_error "--backup-suffix needs a value"
      backup_suffix="$2"; shift 2 ;;
    --*) usage_error "unsupported option $1" ;;
    *)
      [ -z "$file" ] || usage_error "unexpected extra argument $1"
      file="$1"; shift ;;
  esac
done

[ -n "$file" ] || usage_error "--file is required"
[ -f "$file" ] || fail "not a regular file: $file"
[ -r "$file" ] || fail "not readable: $file"
[ -n "$backup_suffix" ] || usage_error "--backup-suffix must not be empty"

size=$(wc -c <"$file" | tr -d ' ')
[ "$size" -le "$max_bytes" ] || fail "file is larger than the supported bound of $max_bytes bytes"

mapfile -t lines <"$file"

declare -A canonical_values=()
declare -A seen_keys=()
declare -A legacy_values=()
declare -A legacy_to_canonical=()

for raw in "${lines[@]}"; do
  [[ "$raw" == *$'\r'* ]] && fail "line uses CR line endings; normalize the file first"
  line="${raw#"${raw%%[![:space:]]*}"}"
  [ -z "$line" ] && continue
  case "$line" in \#*) continue ;; esac
  case "$line" in export\ *) fail "unsupported 'export ' syntax; use plain KEY=VALUE lines" ;; esac
  case "$line" in *=*) ;; *) fail "line is not a KEY=VALUE assignment" ;; esac
  key="${line%%=*}"
  value="${line#*=}"
  [[ "$key" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || fail "invalid environment key in file"
  [ -n "${seen_keys[$key]:-}" ] && fail "duplicate key $key in file"
  seen_keys[$key]=1
  canonical_values[$key]="$value"
done

for key in "${!canonical_values[@]}"; do
  case "$key" in
    "$legacy_prefix"*) ;;
    *) continue ;;
  esac
  suffix="${key#"$legacy_prefix"}"
  [ -n "$suffix" ] || fail "legacy key without a suffix: $key"
  target="$canonical_prefix$suffix"
  legacy_values[$suffix]="$key"
  legacy_to_canonical[$key]="$target"
done

if [ "${#legacy_to_canonical[@]}" -eq 0 ]; then
  printf 'migrate_result=already_canonical\n'
  printf 'migrate_file=%s\n' "$file"
  exit 0
fi

for key in "${!legacy_to_canonical[@]}"; do
  target="${legacy_to_canonical[$key]}"
  if [ -n "${canonical_values[$target]+set}" ]; then
    if [ "${canonical_values[$target]}" != "${canonical_values[$key]}" ]; then
      fail "conflicting values for $target and $key; resolve them by hand, nothing was written"
    fi
  fi
done

for key in "${!legacy_to_canonical[@]}"; do
  printf 'migrate_key=%s->%s\n' "$key" "${legacy_to_canonical[$key]}"
done | LC_ALL=C sort

if [ "$dry_run" -eq 1 ]; then
  printf 'migrate_result=dry_run\n'
  printf 'migrate_file=%s\n' "$file"
  exit 3
fi

backup="$file$backup_suffix"
[ -e "$backup" ] && fail "refusing to overwrite the existing backup $backup"

tmp=$(mktemp "${TMPDIR:-/tmp}/changeguard-migrate.XXXXXX") || fail "cannot create a temporary file"
cleanup() { rm -f -- "$tmp"; }
trap cleanup EXIT

: >"$tmp"
for raw in "${lines[@]}"; do
  line="${raw#"${raw%%[![:space:]]*}"}"
  if [ -n "$line" ] && [[ "$line" != \#* ]] && [[ "$line" == *=* ]]; then
    key="${line%%=*}"
    key="${key#"${key%%[![:space:]]*}"}"
    case "$key" in
      "$legacy_prefix"*)
        suffix="${key#"$legacy_prefix"}"
        target="$canonical_prefix$suffix"
        if [ -n "${canonical_values[$target]+set}" ]; then
          # The same value already exists under the canonical name in this file.
          continue
        fi
        printf '%s=%s\n' "$target" "${line#*=}" >>"$tmp"
        continue ;;
    esac
  fi
  printf '%s\n' "$raw" >>"$tmp"
done

cp -p -- "$file" "$backup" || fail "cannot create the backup $backup"
if ! cat -- "$tmp" >"$file"; then
  cp -p -- "$backup" "$file" || true
  fail "cannot write $file; original content restored from $backup"
fi

if grep -q "^${legacy_prefix}" "$file"; then
  cp -p -- "$backup" "$file" || true
  fail "post-write verification found legacy keys in $file; content restored from $backup"
fi

printf 'migrate_result=migrated\n'
printf 'migrate_file=%s\n' "$file"
printf 'migrate_backup=%s\n' "$backup"
