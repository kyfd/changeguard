#!/usr/bin/env bash
set -euo pipefail

repository="${1:-.}"
repository="$(cd "$repository" && pwd)"
temporary="$(mktemp)"

cleanup() {
  rm -f "$temporary"
}
trap cleanup EXIT

cd "$repository"
for required in cmd internal go.mod go.sum; do
  [ -e "$required" ] || { printf 'source digest input missing: %s/%s\n' "$repository" "$required" >&2; exit 1; }
done

{
  find cmd internal -type f -print0
  printf 'go.mod\0go.sum\0'
} | LC_ALL=C sort -z | while IFS= read -r -d '' file; do
  sha256sum "$file"
done > "$temporary"

sha256sum "$temporary" | awk '{print $1}'
