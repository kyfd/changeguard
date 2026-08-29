#!/usr/bin/env bash
set -euo pipefail

script_directory="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repository="$(cd "$script_directory/../.." && pwd)"
version="${CHANGEGUARD_VERSION:?CHANGEGUARD_VERSION is required}"
commit="${CHANGEGUARD_COMMIT:?CHANGEGUARD_COMMIT is required}"
built_at="${CHANGEGUARD_BUILT_AT:-$(date -u +%Y-%m-%dT%H:%M:%SZ)}"
output="${CHANGEGUARD_OUTPUT:-$repository/dist/dbguard}"
source_sha256="$(bash "$script_directory/source-tree-sha256.sh" "$repository")"

[[ "$version" =~ ^[0-9A-Za-z][0-9A-Za-z._+-]{0,63}$ ]] || { printf 'invalid CHANGEGUARD_VERSION\n' >&2; exit 1; }
[[ "$commit" =~ ^[0-9a-f]{7,64}$ ]] || { printf 'invalid CHANGEGUARD_COMMIT\n' >&2; exit 1; }
[[ "$built_at" =~ ^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z$ ]] || { printf 'invalid CHANGEGUARD_BUILT_AT\n' >&2; exit 1; }
if [ -n "${CHANGEGUARD_SOURCE_SHA256:-}" ] && [ "$CHANGEGUARD_SOURCE_SHA256" != "$source_sha256" ]; then
  printf 'source digest mismatch expected=%s actual=%s\n' "$CHANGEGUARD_SOURCE_SHA256" "$source_sha256" >&2
  exit 1
fi

install -d -m 0755 "$(dirname "$output")"
temporary="${output}.tmp.$$"
cleanup() {
  rm -f "$temporary"
}
trap cleanup EXIT

ldflags="-s -w -X github.com/liufengxi/dbguard/internal/buildinfo.Version=$version -X github.com/liufengxi/dbguard/internal/buildinfo.Commit=$commit -X github.com/liufengxi/dbguard/internal/buildinfo.BuiltAt=$built_at -X github.com/liufengxi/dbguard/internal/buildinfo.SourceSHA256=$source_sha256"
(
  cd "$repository"
  env CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags "$ldflags" -o "$temporary" ./cmd/dbguard
)
chmod 0755 "$temporary"
mv -f "$temporary" "$output"
temporary=""

printf 'build_status=ok\n'
printf 'version=%s\n' "$version"
printf 'commit=%s\n' "$commit"
printf 'built_at=%s\n' "$built_at"
printf 'source_sha256=%s\n' "$source_sha256"
printf 'artifact_sha256=%s\n' "$(sha256sum "$output" | awk '{print $1}')"
printf 'output=%s\n' "$output"
