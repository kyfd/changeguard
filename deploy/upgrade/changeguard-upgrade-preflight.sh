#!/usr/bin/env bash
# ChangeGuard upgrade preflight (read-only).
#
# Purpose: verify what the installer cannot see BEFORE switching the service:
#   1. the running core identity (version / commit / source digest / health);
#   2. the running Python Agent identity and whether both are FULLY matched;
#   3. whether a rollback target really exists (including integrity);
#   4. the upgrade package identity (schema / version / tag / root directory).
#
# This script is READ-ONLY: no download, no install, no symlink switch, no
# service restart, no business data writes, and no changes to any version
# directory inside release-root. The in-package manifest is parsed with a
# BOUNDED read-only tarfile extractfile (never extracted to disk).
#
# Fail-closed by default:
#   - missing/mismatched identity, no rollback target, package mismatch -> failed;
#   - checks not run without an explicit declaration -> incomplete, and callers
#     MUST NOT install or switch on exit code 3. "Not checked" is never "OK".
#
# Usage: see --help. Exit codes: 0=passed 1=failed 3=incomplete 64=usage error.
set -euo pipefail
umask 022

core_health_url="http://127.0.0.1:8080/health/ready"
agent_health_url="${CHANGEGUARD_AGENT_HEALTH_URL:-}"
agent_token_env="AGENT_UPSTREAM_TOKEN"
archive=""
expected_sha256=""
target_version=""
release_root="/opt/changeguard/releases"
current_link="/opt/changeguard/current"
core_only=0
timeout=5

# --help must print real usage, never the script body.
usage() {
  cat <<'USAGE'
Usage: changeguard-upgrade-preflight.sh [options]

Options:
  --core-health-url URL      Core readiness endpoint (default http://127.0.0.1:8080/health/ready)
  --agent-health-url URL     Agent healthz endpoint; declares this deployment has an Agent.
                             Empty (default): Agent checks are reported as not_run (incomplete).
  --agent-token-env NAME     Environment variable holding the Agent shared token
                             (default AGENT_UPSTREAM_TOKEN). The token is read from the
                             environment, never placed in argv and never echoed.
  --archive PATH             Upgrade package to verify (requires --expected-sha256)
  --expected-sha256 HASH     64 hex chars; must match the package digest
  --target-version X.Y.Z     Target version to compare with the in-package manifest
  --release-root PATH        Release root directory (default /opt/changeguard/releases)
  --current-link PATH        Current-version symlink (default /opt/changeguard/current)
  --core-only                This deployment has no Agent: Agent/pairing checks are
                             reported as not_applicable and do not block
  --timeout SECONDS          Per-request HTTP budget, 1..999 (default 5)
  -h, --help                 Show this help

Exit codes: 0 all checks passed; 1 one or more checks failed;
3 checks not run (callers must NOT install/switch on 3); 64 usage error.
USAGE
}

fail() { printf 'preflight_error=%s\n' "$*" >&2; exit 64; }

while [ "$#" -gt 0 ]; do
  case "$1" in
    -h|--help) usage; exit 0 ;;
    --core-health-url|--agent-health-url|--agent-token-env|--archive|--expected-sha256|\
    --target-version|--release-root|--current-link|--timeout)
      [ "$#" -ge 2 ] && [ -n "$2" ] || fail "missing value for $1"
      case "$1" in
        --core-health-url) core_health_url="$2" ;;
        --agent-health-url) agent_health_url="$2" ;;
        --agent-token-env) agent_token_env="$2" ;;
        --archive) archive="$2" ;;
        --expected-sha256) expected_sha256="$2" ;;
        --target-version) target_version="$2" ;;
        --release-root) release_root="$2" ;;
        --current-link) current_link="$2" ;;
        --timeout) timeout="$2" ;;
      esac
      shift 2 ;;
    --core-only) core_only=1; shift ;;
    *) fail "unknown option: $1" ;;
  esac
done

for tool in python3 sha256sum tar; do
  command -v "$tool" >/dev/null 2>&1 || fail "$tool is required"
done
[[ "$timeout" =~ ^[1-9][0-9]{0,2}$ ]] || fail 'timeout must be 1..999'
[[ "$core_health_url" == http://* || "$core_health_url" == https://* ]] || fail 'invalid core-health-url'
if [ -n "$agent_health_url" ]; then
  [[ "$agent_health_url" == http://* || "$agent_health_url" == https://* ]] || fail 'invalid agent-health-url'
fi
# --core-only and an Agent URL are mutually exclusive declarations.
[ "$core_only" -eq 0 ] || [ -z "$agent_health_url" ] || fail 'core-only contradicts agent-health-url'
if [ -n "$archive" ]; then
  [ -n "$expected_sha256" ] || fail 'archive requires expected-sha256'
  [[ "$expected_sha256" =~ ^[0-9a-f]{64}$ ]] || fail 'expected-sha256 must be 64 hex chars'
  [ -f "$archive" ] && [ ! -L "$archive" ] || fail 'archive must be an existing regular file, not a symlink'
fi
# token-env must be a valid environment variable name; the token travels via
# the environment, never via argv.
[[ "$agent_token_env" =~ ^[A-Za-z_][A-Za-z0-9_]*$ ]] || fail 'agent-token-env must be a valid environment variable name'
[[ "$release_root" == /* && "$current_link" == /* ]] || fail 'release-root and current-link must be absolute'

failures=0
unchecked=0
# Machine-readable failure reasons; private temp file, removed on exit.
error_file="$(mktemp "${TMPDIR:-/tmp}/changeguard-preflight-XXXXXX")"
cleanup() { rm -f -- "$error_file"; }
trap cleanup EXIT
note_check() {
  printf 'preflight_check=%s status=%s detail=%s\n' "$1" "$2" "$3"
}
check_failed() { note_check "$1" failed "$2"; failures=$((failures + 1)); }
check_not_run() { note_check "$1" not_run "$2"; unchecked=$((unchecked + 1)); }
# --core-only is an explicit declaration: neither pass nor fail, not counted.
check_not_applicable() { note_check "$1" not_applicable "$2"; }

normalize_version() { printf '%s' "${1//-/.}"; }

# ---------------------------------------------------------------- identity
# Python performs the strict verification: the payload must be an object with
# status=ok, the version must be X.Y.Z, commit and source_sha256 must be
# complete hex values. The token travels via the environment (never argv);
# proxies and redirects are disabled; time and body size are bounded.
#
# stdout: key=value lines. Empty output means verification failed; the
# machine-readable reason goes to the captured error file.
read_identity() {
  # $1 URL  $2 token_env_name (empty = anonymous read)
  AUTH_ENV="$2" URL="$1" MAXTIME="$timeout" python3 - <<'PY'
import json
import os
import re
import sys
import urllib.request

url = os.environ["URL"]
auth_env = os.environ.get("AUTH_ENV", "")
maxtime = float(os.environ.get("MAXTIME", "5"))
max_body = 1 << 20  # health responses are bounded; >1MB is rejected

token = os.environ.get(auth_env, "") if auth_env else ""
request = urllib.request.Request(url)
if token:
    request.add_header("X-Agent-Upstream-Token", token)

# Fail closed: no proxies, no redirects, bounded time and body size.
class _NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl):
        return None

# 禁用环境代理：显式传入 ProxyHandler({})，否则 build_opener 会加载
# http_proxy/https_proxy，请求可能被重定向到代理而非目标实例。
opener = urllib.request.build_opener(_NoRedirect, urllib.request.ProxyHandler({}))
opener.addheaders = []
try:
    with opener.open(request, timeout=maxtime) as response:
        if response.status != 200:
            raise RuntimeError(f"unexpected status {response.status}")
        body = response.read(max_body + 1)
        if len(body) > max_body:
            raise RuntimeError("response too large")
except Exception as error:
    print(f"identity_error=transport detail={type(error).__name__}", file=sys.stderr)
    raise SystemExit(0)

try:
    payload = json.loads(body)
except Exception:
    print("identity_error=json_parse", file=sys.stderr)
    raise SystemExit(0)
if not isinstance(payload, dict):
    print("identity_error=not_an_object", file=sys.stderr)
    raise SystemExit(0)
if payload.get("status") != "ok":
    # degraded (or anything else) is not a healthy identity.
    print(f"identity_error=status_{payload.get('status', 'missing')}", file=sys.stderr)
    raise SystemExit(0)
build = payload.get("build")
if not isinstance(build, dict):
    print("identity_error=build_missing", file=sys.stderr)
    raise SystemExit(0)

version = build.get("version")
commit = build.get("commit")
source = build.get("source_sha256")
if not all(isinstance(value, str) for value in (version, commit, source)):
    print("identity_error=fields_missing", file=sys.stderr)
    raise SystemExit(0)
version = version.strip()
# The release build writes hyphenated versions (3-1-4); normalize so both
# formats are accepted but must still be a real X.Y.Z.
normalized = version.replace("-", ".")
if version in ("", "unknown", "dev") or not re.fullmatch(r"\d+\.\d+\.\d+", normalized):
    print(f"identity_error=version_invalid value={version!r}", file=sys.stderr)
    raise SystemExit(0)
if not re.fullmatch(r"[0-9a-f]{7,64}", commit):
    print("identity_error=commit_invalid", file=sys.stderr)
    raise SystemExit(0)
if not re.fullmatch(r"[0-9a-f]{64}", source):
    print("identity_error=source_sha256_invalid", file=sys.stderr)
    raise SystemExit(0)
print(f"version={normalized}")
print(f"commit={commit}")
print(f"source_sha256={source}")
PY
}

# Extract one key=value field; missing values print an empty string.
identity_field() {
  printf '%s\n' "$1" | sed -n "s/^$2=//p" | head -1
}

# ---------------------------------------------------------------- core identity
core_version=""
core_commit=""
core_source=""
core_output="$(read_identity "$core_health_url" "" 2>"$error_file" || true)"
core_error="$(cat "$error_file" 2>/dev/null || true)"
if [ -z "$core_output" ]; then
  check_failed core_identity "core identity verification failed (${core_error:-no response})"
else
  core_version="$(identity_field "$core_output" version)"
  core_commit="$(identity_field "$core_output" commit)"
  core_source="$(identity_field "$core_output" source_sha256)"
  note_check core_identity passed "version=$core_version commit=$core_commit source_sha256=$core_source"
fi

# --------------------------------------------------------------- agent identity
agent_version=""
agent_commit=""
agent_source=""
if [ "$core_only" -eq 1 ]; then
  # Explicitly declared Agent-less: not_applicable, neither blocking nor a fake pass.
  check_not_applicable agent_identity "declared Agent-less via --core-only"
  check_not_applicable identity_pairing "no Agent to compare"
elif [ -z "$agent_health_url" ]; then
  check_not_run agent_identity "no Agent health URL declared (--agent-health-url or CHANGEGUARD_AGENT_HEALTH_URL)"
  check_not_run identity_pairing "missing Agent identity, not compared"
else
  if [ -z "${!agent_token_env:-}" ]; then
    check_failed agent_identity "environment variable $agent_token_env is empty; cannot read Agent identity with credentials"
    check_failed identity_pairing "core/Agent comparison impossible (Agent read failed)"
  else
    agent_output="$(read_identity "$agent_health_url" "$agent_token_env" 2>"$error_file" || true)"
    agent_error="$(cat "$error_file" 2>/dev/null || true)"
    if [ -z "$agent_output" ]; then
      check_failed agent_identity "Agent identity verification failed (${agent_error:-no response})"
      check_failed identity_pairing "core/Agent comparison impossible (Agent read failed)"
    else
      agent_version="$(identity_field "$agent_output" version)"
      agent_commit="$(identity_field "$agent_output" commit)"
      agent_source="$(identity_field "$agent_output" source_sha256)"
      note_check agent_identity passed "version=$agent_version commit=$agent_commit source_sha256=$agent_source"
      # Paired means version + commit + source digest all match; comparing the
      # version alone would let "same version, different commit" through.
      if [ "$core_version" = "$agent_version" ] && [ "$core_commit" = "$agent_commit" ] \
         && [ "$core_source" = "$agent_source" ]; then
        note_check identity_pairing passed "core=$core_version/$core_commit agent=$agent_version/$agent_commit"
      else
        check_failed identity_pairing "core/Agent identities differ: core=$core_version/$core_commit/$core_source agent=$agent_version/$agent_commit/$agent_source"
      fi
    fi
  fi
fi

# ------------------------------------------------------------- rollback target
if [ ! -L "$current_link" ]; then
  check_failed rollback_capability "current-link is not a symlink: $current_link"
else
  current_target="$(readlink -e -- "$current_link" 2>/dev/null || true)"
  resolved_root="$(readlink -f -- "$release_root" 2>/dev/null || true)"
  if [ -z "$current_target" ] || [ ! -d "$current_target" ]; then
    check_failed rollback_capability "current-link points to a missing release"
  elif [ -z "$resolved_root" ] || [ "$(dirname "$current_target")" != "$resolved_root" ]; then
    check_failed rollback_capability "current release is not directly inside release-root: $current_target"
  else
    rollback_binary="$current_target/dbguard"
    # Must be a regular, executable binary (not a directory, not a symlink),
    # backed by a checksum manifest that actually matches the files on disk.
    if [ ! -f "$rollback_binary" ] || [ -L "$rollback_binary" ]; then
      check_failed rollback_capability "current release has no regular dbguard binary for rollback"
    elif [ ! -x "$rollback_binary" ]; then
      check_failed rollback_capability "rollback target dbguard is not executable"
    elif [ ! -f "$current_target/SHA256SUMS" ]; then
      check_failed rollback_capability "rollback target has no SHA256SUMS to verify integrity"
    elif ! grep -qE '(^|  |\*)dbguard$' "$current_target/SHA256SUMS"; then
      # 清单必须实际覆盖 dbguard：只校验清单里列出的文件会放过"清单缺项"。
      check_failed rollback_capability "SHA256SUMS does not cover dbguard"
    elif (cd "$current_target" && sha256sum --quiet -c SHA256SUMS >/dev/null 2>&1); then
      note_check rollback_capability passed "current=$(basename "$current_target") binary=regular checksums=verified"
    else
      check_failed rollback_capability "rollback target integrity check failed (SHA256SUMS mismatch)"
    fi
  fi
fi

# ------------------------------------------------------------ package identity
archive_manifest_version=""
if [ -z "$archive" ]; then
  check_not_run archive_identity "no --archive provided; package identity not verified"
else
  actual_sha256="$(sha256sum -- "$archive" | awk '{print $1}')"
  if [ "$actual_sha256" != "$expected_sha256" ]; then
    check_failed archive_identity "archive SHA256 mismatch"
  else
    # Bounded read-only parsing: tarfile extractfile, never extracted to disk.
    # Rejects duplicate/non-regular manifests and unsafe members; verifies
    # schema, version, tag, commit, source digest and the single root directory.
    archive_output="$(python3 - "$archive" <<'PY'
import json
import re
import sys
import tarfile

archive_path = sys.argv[1]
max_manifest = 1 << 20  # manifest is bounded; 1MB is ample
max_members = 64        # a valid package only carries dbguard + checksums + evidence

error = None
manifest = None
root_dir = None
try:
    with tarfile.open(archive_path, "r:gz") as handle:
        # getmembers() 全量加载后再检查数量不是真正有界：流式逐个读取，
        # 超过成员上限立即失败，不把整个包的索引都装进内存。
        handled = 0
        for member in handle:
            handled += 1
            if handled > max_members:
                error = "too_many_members"
                break
            name = member.name
            if name.startswith("/") or ".." in name.split("/"):
                error = "unsafe_member"
                break
            parts = name.split("/", 1)
            if len(parts) != 2 or not parts[1]:
                # A bare root-directory entry ("pkg/") is legal; skip it.
                if member.isdir() and "/" not in name.rstrip("/"):
                    continue
                error = "missing_root_directory"
                break
            if root_dir is None:
                root_dir = parts[0]
            elif parts[0] != root_dir:
                error = "multiple_root_directories"
                break
            if parts[1] == "release-manifest.json":
                if manifest is not None:
                    error = "duplicate_manifest"
                    break
                if not member.isfile():
                    error = "manifest_not_regular"
                    break
                extracted = handle.extractfile(member)
                if extracted is None:
                    error = "manifest_unreadable"
                    break
                data = extracted.read(max_manifest + 1)
                if len(data) > max_manifest:
                    error = "manifest_too_large"
                    break
                manifest = data
        if error is None and manifest is None:
            error = "manifest_missing"
except (tarfile.TarError, OSError) as tar_error:
    error = f"archive_unreadable:{type(tar_error).__name__}"

if error is not None:
    print(f"archive_error={error}", file=sys.stderr)
    raise SystemExit(0)

try:
    data = json.loads(manifest)
except Exception:
    print("archive_error=manifest_json_invalid", file=sys.stderr)
    raise SystemExit(0)
if not isinstance(data, dict):
    print("archive_error=manifest_not_an_object", file=sys.stderr)
    raise SystemExit(0)
if data.get("schema") != "changeguard-core-release/v2":
    print(f"archive_error=schema_invalid value={data.get('schema')!r}", file=sys.stderr)
    raise SystemExit(0)
version = data.get("version")
tag = data.get("tag")
commit = data.get("commit")
source = data.get("source_sha256")
if not all(isinstance(value, str) and value.strip() for value in (version, tag, commit, source)):
    print("archive_error=manifest_fields_missing", file=sys.stderr)
    raise SystemExit(0)
version = version.strip()
# The release pipeline writes hyphenated versions (3-1-4); normalize before
# comparing so both formats are accepted but must still be internally consistent.
normalized = version.replace("-", ".")
if not re.fullmatch(r"\d+\.\d+\.\d+", normalized):
    print(f"archive_error=manifest_version_invalid value={version!r}", file=sys.stderr)
    raise SystemExit(0)
if not re.fullmatch(r"v\d+\.\d+\.\d+", tag.strip()):
    print(f"archive_error=manifest_tag_invalid value={tag.strip()!r}", file=sys.stderr)
    raise SystemExit(0)
if tag.strip() != f"v{normalized}":
    print("archive_error=manifest_tag_version_mismatch", file=sys.stderr)
    raise SystemExit(0)
if not re.fullmatch(r"[0-9a-f]{7,64}", commit.strip().lower()):
    print("archive_error=manifest_commit_invalid", file=sys.stderr)
    raise SystemExit(0)
if not re.fullmatch(r"[0-9a-f]{64}", source.strip().lower()):
    print("archive_error=manifest_source_sha256_invalid", file=sys.stderr)
    raise SystemExit(0)
# The release pipeline names the root directory changeguard-<hyphenated version>;
# the root must correspond to the manifest version.
expected_root = f"changeguard-{version.replace('.', '-')}"
if root_dir is not None and root_dir != expected_root:
    print(f"archive_error=root_version_mismatch root={root_dir!r} expected={expected_root!r}", file=sys.stderr)
    raise SystemExit(0)
print(f"version={normalized}")
print(f"tag={tag.strip()}")
print(f"commit={commit.strip().lower()}")
print(f"source_sha256={source.strip().lower()}")
print(f"root={root_dir}")
PY
)" 2>"$error_file" || true
    archive_error="$(cat "$error_file" 2>/dev/null || true)"
    if [ -z "$archive_output" ]; then
      check_failed archive_identity "package identity verification failed (${archive_error:-unknown})"
    else
      archive_manifest_version="$(identity_field "$archive_output" version)"
      archive_tag="$(identity_field "$archive_output" tag)"
      archive_commit="$(identity_field "$archive_output" commit)"
      archive_source="$(identity_field "$archive_output" source_sha256)"
      archive_root="$(identity_field "$archive_output" root)"
      note_check archive_identity passed "sha256=$actual_sha256 version=$archive_manifest_version tag=$archive_tag commit=$archive_commit root=$archive_root"
    fi
  fi
fi

# ------------------------------------------------------ target version consistency
if [ -z "$target_version" ]; then
  check_not_run target_version "no --target-version provided; target not compared"
elif [ -z "$archive_manifest_version" ]; then
  check_failed target_version "no trusted in-package version to compare with $target_version"
else
  if [ "$(normalize_version "$target_version")" = "$(normalize_version "$archive_manifest_version")" ]; then
    note_check target_version passed "target=$target_version package=$archive_manifest_version"
  else
    check_failed target_version "target version $target_version differs from in-package $archive_manifest_version"
  fi
fi

# -------------------------------------------------------------------- verdict
if [ "$failures" -gt 0 ]; then
  printf 'preflight_status=failed failures=%d unchecked=%d\n' "$failures" "$unchecked"
  exit 1
fi
if [ "$unchecked" -gt 0 ]; then
  # Checks that did not run must not be treated as passed; callers MUST NOT
  # install or switch on this exit code.
  printf 'preflight_status=incomplete failures=0 unchecked=%d\n' "$unchecked"
  exit 3
fi
printf 'preflight_status=passed failures=0 unchecked=0\n'
