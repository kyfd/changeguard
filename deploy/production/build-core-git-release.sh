#!/usr/bin/env bash
set -euo pipefail

script_directory="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
repository="$(cd "$script_directory/../.." && pwd)"
version="${CHANGEGUARD_VERSION:?CHANGEGUARD_VERSION is required}"
release_tag="${CHANGEGUARD_RELEASE_TAG:?CHANGEGUARD_RELEASE_TAG is required}"
release_directory="${CHANGEGUARD_RELEASE_DIRECTORY:?CHANGEGUARD_RELEASE_DIRECTORY is required}"
verification_source="${CHANGEGUARD_VERIFICATION_EVIDENCE:?CHANGEGUARD_VERIFICATION_EVIDENCE is required}"

command -v git >/dev/null 2>&1 || { printf 'git is required\n' >&2; exit 1; }
command -v go >/dev/null 2>&1 || { printf 'go is required\n' >&2; exit 1; }
command -v python3 >/dev/null 2>&1 || { printf 'python3 is required\n' >&2; exit 1; }
git -C "$repository" rev-parse --is-inside-work-tree >/dev/null 2>&1 || { printf 'repository is not a Git worktree\n' >&2; exit 1; }

worktree_status="$(git -C "$repository" status --porcelain=v1 --untracked-files=all)"
[ -z "$worktree_status" ] || { printf 'release worktree is not clean:\n%s\n' "$worktree_status" >&2; exit 1; }
git -C "$repository" diff --check --cached
git -C "$repository" diff --check

commit="$(git -C "$repository" rev-parse --verify HEAD^{commit})"
tag_commit="$(git -C "$repository" rev-list -n 1 "$release_tag" 2>/dev/null || true)"
[ "$tag_commit" = "$commit" ] || { printf 'release tag does not resolve to HEAD\n' >&2; exit 1; }
[ "$(git -C "$repository" cat-file -t "$release_tag")" = "tag" ] || { printf 'release tag must be annotated\n' >&2; exit 1; }
[ "${release_directory#/}" != "$release_directory" ] || { printf 'release directory must be absolute\n' >&2; exit 1; }
[ "$release_directory" != "/" ] || { printf 'release directory must not be the filesystem root\n' >&2; exit 1; }
[ ! -e "$release_directory" ] || { printf 'release directory already exists\n' >&2; exit 1; }

release_parent="$(dirname "$release_directory")"
release_leaf="$(basename "$release_directory")"
[ "$release_leaf" != "." ] && [ "$release_leaf" != ".." ] || { printf 'invalid release directory leaf\n' >&2; exit 1; }
install -d -m 0755 "$release_parent"
release_parent="$(cd -P "$release_parent" && pwd)"
release_directory="$release_parent/$release_leaf"
[ ! -e "$release_directory" ] || { printf 'resolved release directory already exists\n' >&2; exit 1; }
case "$release_directory/" in
  "$repository/"*) printf 'release directory must be outside the Git worktree\n' >&2; exit 1 ;;
esac

[ -f "$verification_source" ] || { printf 'verification evidence file does not exist\n' >&2; exit 1; }
source_sha256="$("$script_directory/source-tree-sha256.sh" "$repository")"
python3 - "$verification_source" "$version" "$release_tag" "$commit" "$source_sha256" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as handle:
    evidence = json.load(handle)
if evidence.get("schema") != "changeguard-core-verification/v1":
    raise SystemExit("unexpected verification evidence schema")
if evidence.get("status") != "passed":
    raise SystemExit("verification evidence is not passed")
expected = {
    "version": sys.argv[2],
    "tag": sys.argv[3],
    "commit": sys.argv[4],
    "source_sha256": sys.argv[5],
    "target": "linux/amd64",
}
for field, value in expected.items():
    if evidence.get(field) != value:
        raise SystemExit(f"verification evidence {field} mismatch")
commands = evidence.get("commands")
if not isinstance(commands, list) or not commands:
    raise SystemExit("verification evidence commands are missing")
for command in commands:
    if not isinstance(command, dict) or command.get("status") != "passed" or not command.get("command"):
        raise SystemExit("verification evidence contains an invalid command result")
PY

install -d -m 0755 "$release_directory"
incomplete_marker="$release_directory/.incomplete"
printf 'release assembly did not complete\n' > "$incomplete_marker"
report_incomplete_release() {
  if [ -e "$incomplete_marker" ]; then
    printf 'release directory retained for audit with .incomplete marker: %s\n' "$release_directory" >&2
  fi
}
trap report_incomplete_release EXIT

commit_epoch="$(git -C "$repository" show -s --format=%ct "$commit")"
built_at="$(date -u -d "@$commit_epoch" +%Y-%m-%dT%H:%M:%SZ)"
artifact="$release_directory/dbguard"

env \
  CHANGEGUARD_VERSION="$version" \
  CHANGEGUARD_COMMIT="$commit" \
  CHANGEGUARD_BUILT_AT="$built_at" \
  CHANGEGUARD_SOURCE_SHA256="$source_sha256" \
  CHANGEGUARD_OUTPUT="$artifact" \
  "$script_directory/build-core-release.sh" > "$release_directory/build.log"

git -C "$repository" bundle create "$release_directory/source.bundle" "$release_tag"
git -C "$repository" bundle verify "$release_directory/source.bundle" > "$release_directory/bundle-verify.txt" 2>&1
git -C "$repository" archive --format=tar.gz --output="$release_directory/source.tar.gz" "$commit"
(
  cd "$repository"
  go list -m all > "$release_directory/modules.txt"
)
go version -m "$artifact" > "$release_directory/binary-buildinfo.txt"
cp "$verification_source" "$release_directory/verification.json"

artifact_sha256="$(sha256sum "$artifact" | awk '{print $1}')"
bundle_sha256="$(sha256sum "$release_directory/source.bundle" | awk '{print $1}')"
archive_sha256="$(sha256sum "$release_directory/source.tar.gz" | awk '{print $1}')"
verification_sha256="$(sha256sum "$release_directory/verification.json" | awk '{print $1}')"
modules_sha256="$(sha256sum "$release_directory/modules.txt" | awk '{print $1}')"
binary_buildinfo_sha256="$(sha256sum "$release_directory/binary-buildinfo.txt" | awk '{print $1}')"
bundle_verify_sha256="$(sha256sum "$release_directory/bundle-verify.txt" | awk '{print $1}')"
build_log_sha256="$(sha256sum "$release_directory/build.log" | awk '{print $1}')"
go_mod_sha256="$(sha256sum "$repository/go.mod" | awk '{print $1}')"
go_sum_sha256="$(sha256sum "$repository/go.sum" | awk '{print $1}')"
go_version="$(go version | awk '{print $3}')"

RELEASE_VERSION="$version" RELEASE_TAG="$release_tag" RELEASE_COMMIT="$commit" \
RELEASE_BUILT_AT="$built_at" RELEASE_SOURCE_SHA256="$source_sha256" \
RELEASE_ARTIFACT_SHA256="$artifact_sha256" RELEASE_BUNDLE_SHA256="$bundle_sha256" \
RELEASE_ARCHIVE_SHA256="$archive_sha256" RELEASE_VERIFICATION_SHA256="$verification_sha256" \
RELEASE_MODULES_SHA256="$modules_sha256" RELEASE_BINARY_BUILDINFO_SHA256="$binary_buildinfo_sha256" \
RELEASE_BUNDLE_VERIFY_SHA256="$bundle_verify_sha256" RELEASE_BUILD_LOG_SHA256="$build_log_sha256" \
RELEASE_GO_MOD_SHA256="$go_mod_sha256" RELEASE_GO_SUM_SHA256="$go_sum_sha256" \
RELEASE_GO_VERSION="$go_version" python3 - "$release_directory/release-manifest.json" <<'PY'
import json
import os
import sys

manifest = {
    "schema": "changeguard-core-release/v2",
    "version": os.environ["RELEASE_VERSION"],
    "tag": os.environ["RELEASE_TAG"],
    "commit": os.environ["RELEASE_COMMIT"],
    "commit_time": os.environ["RELEASE_BUILT_AT"],
    "clean_worktree": True,
    "source_sha256": os.environ["RELEASE_SOURCE_SHA256"],
    "go_version": os.environ["RELEASE_GO_VERSION"],
    "target": "linux/amd64",
    "cgo_enabled": False,
    "files": {
        "dbguard": os.environ["RELEASE_ARTIFACT_SHA256"],
        "source.bundle": os.environ["RELEASE_BUNDLE_SHA256"],
        "source.tar.gz": os.environ["RELEASE_ARCHIVE_SHA256"],
        "verification.json": os.environ["RELEASE_VERIFICATION_SHA256"],
        "modules.txt": os.environ["RELEASE_MODULES_SHA256"],
        "binary-buildinfo.txt": os.environ["RELEASE_BINARY_BUILDINFO_SHA256"],
        "bundle-verify.txt": os.environ["RELEASE_BUNDLE_VERIFY_SHA256"],
        "build.log": os.environ["RELEASE_BUILD_LOG_SHA256"],
        "go.mod": os.environ["RELEASE_GO_MOD_SHA256"],
        "go.sum": os.environ["RELEASE_GO_SUM_SHA256"],
    },
}
with open(sys.argv[1], "w", encoding="utf-8", newline="\n") as handle:
    json.dump(manifest, handle, ensure_ascii=False, indent=2, sort_keys=True)
    handle.write("\n")
PY

(
  cd "$release_directory"
  sha256sum dbguard source.bundle source.tar.gz modules.txt binary-buildinfo.txt bundle-verify.txt verification.json build.log release-manifest.json > SHA256SUMS
  sha256sum --check SHA256SUMS
)
chmod 0755 "$artifact"
chmod 0644 "$release_directory"/{source.bundle,source.tar.gz,modules.txt,binary-buildinfo.txt,bundle-verify.txt,verification.json,build.log,release-manifest.json,SHA256SUMS}
rm -f -- "$incomplete_marker"

printf 'git_release_status=ok\n'
printf 'version=%s\n' "$version"
printf 'tag=%s\n' "$release_tag"
printf 'commit=%s\n' "$commit"
printf 'source_sha256=%s\n' "$source_sha256"
printf 'artifact_sha256=%s\n' "$artifact_sha256"
printf 'release_directory=%s\n' "$release_directory"
