"""Linux/root-only upgrade regressions; real installer, synthetic archives and I/O.

Run: sudo python3 -B -m unittest discover -s tests/deploy -v
All fixtures live under the project's ignored Temp directory. No real systemd
services, network, production data, backup or restore operations are involved.
"""

import hashlib
import http.server
import io
import json
import os
import threading
from pathlib import Path
import shutil
import signal
import subprocess
import sys
import tarfile
import tempfile
import time
import unittest


ROOT = Path(__file__).resolve().parents[2]
INSTALL = ROOT / "deploy/production/changeguard-core-install.sh"
UPGRADE = ROOT / "deploy/upgrade/changeguard-upgrade.sh"
WATCHER = ROOT / "deploy/upgrade/changeguard-upgrade-watcher.sh"
PREFLIGHT = ROOT / "deploy/upgrade/changeguard-upgrade-preflight.sh"
NEW = "changeguard-3-1-3"
OLD = "changeguard-3-1-2"


def digest(body):
    return hashlib.sha256(body).hexdigest()


@unittest.skipUnless(sys.platform == "linux" and hasattr(os, "geteuid") and os.geteuid() == 0,
                     "requires Linux root (installer enforces real ownership and modes)")
class UpgradeTests(unittest.TestCase):
    def setUp(self):
        base = ROOT / "Temp/tests/upgrade"
        base.mkdir(parents=True, exist_ok=True)
        self.temp = tempfile.TemporaryDirectory(prefix="case-", dir=base)
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.releases = self.root / "releases"
        self.releases.mkdir(mode=0o755)
        self.releases.chmod(0o755)
        # Fail, do not silently skip a runner that cannot enforce POSIX modes.
        self.assertEqual(self.releases.stat().st_mode & 0o777, 0o755,
                         "tests require a filesystem supporting POSIX permissions")
        self.old = self.releases / OLD
        self.old.mkdir()
        # Preflight requires an executable, checksum-verified dbguard in the
        # rollback target.
        (self.old / "dbguard").write_text("#!/bin/sh\nexit 0\n")
        (self.old / "dbguard").chmod(0o755)
        import hashlib as _hashlib
        real_digest = _hashlib.sha256(b"#!/bin/sh\nexit 0\n").hexdigest()
        (self.old / "SHA256SUMS").write_text(f"{real_digest}  dbguard\n")
        self.current = self.root / "current"
        self.current.symlink_to(self.old, target_is_directory=True)
        self.pending = self.root / "upgrades/pending"
        self.pending.mkdir(parents=True)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.events = self.root / "events.log"
        self.archive = self.make_archive()
        self.env = dict(os.environ, PATH=str(self.bin) + os.pathsep + os.environ["PATH"],
                        TMPDIR=str(self.root), PYTHONDONTWRITEBYTECODE="1",
                        TEST_CURRENT=str(self.current), TEST_ARCHIVE=str(self.archive),
                        TEST_EVENTS=str(self.events), TEST_MODE="ok")
        # The rewritten preflight reads the core identity with Python urllib,
        # not curl: serve the readiness payload with a real local HTTP stub.
        self.httpd = http.server.HTTPServer(("127.0.0.1", 0), HealthHandler)
        self.httpd.test_mode = "ok"
        self.health_port = self.httpd.server_address[1]
        self.health_thread = threading.Thread(target=self.httpd.serve_forever, daemon=True)
        self.health_thread.start()
        self.addCleanup(self.httpd.shutdown)
        self.addCleanup(self.httpd.server_close)
        self.core_url = f"http://127.0.0.1:{self.health_port}/health/ready"
        self.stub("systemctl", '''#!/usr/bin/env bash
set -eu
[ "$1" = restart ] && [ "$2" = changeguard-test ] || exit 90
active="$(basename "$(readlink -f "$TEST_CURRENT")")"
printf 'restart:%s\n' "$active" >> "$TEST_EVENTS"
case "$TEST_MODE:$active" in
  restart-fail:changeguard-3-1-3|both-restart-fail:*) exit 1 ;;
esac
''')
        self.stub("curl", '''#!/usr/bin/env bash
set -eu
while [ "$#" -gt 0 ]; do
  if [ "$1" = -o ]; then
    cp "$TEST_ARCHIVE" "$2"
    printf 'download\n' >> "$TEST_EVENTS"
    exit 0
  fi
  shift
done
active="$(basename "$(readlink -f "$TEST_CURRENT")")"
printed_version="${active#changeguard-}"
printed_version="${printed_version//-/.}"
printf 'health:%s\n' "$active" >> "$TEST_EVENTS"
case "$TEST_MODE:$active" in
  health-fail:changeguard-3-1-3) exit 1 ;;
  both-health-fail:*)
    # Unhealthy after rollback too: the first probe (preflight, no restart yet)
    # succeeds, so a failing probe after any restart means rollback stayed down.
    if grep -q '^restart:' "$TEST_EVENTS" 2>/dev/null; then exit 1; fi
    ;;
esac
# Covers both the CLI health probe and the preflight build-identity read:
# a healthy instance reports a non-"unknown" version, like the real service does.
printf '{"status":"ok","build":{"version":"%s","commit":"%s","source_sha256":"%s","built_at":"2026-10-06T00:00:00Z"}}\n' \
  "$printed_version" "$(printf 'a%.0s' $(seq 1 40))" "$(printf 'b%.0s' $(seq 1 64))"
''')

    def stub(self, name, body):
        path = self.bin / name
        path.write_text(body, encoding="utf-8")
        path.chmod(0o755)

    def make_archive(self, version="3-1-3", tag="v3.1.3", evidence_status="passed",
                     corrupt=False, unsafe=False, mismatched_evidence=False):
        # Synthetic provenance tests the protocol, not real build evidence.
        identity = dict(version=version, tag=tag, commit="a" * 40, source_sha256="b" * 64)
        files = {name: b"synthetic fixture\n" for name in (
            "source.bundle", "source.tar.gz", "modules.txt", "module-verify.txt",
            "binary-buildinfo.txt", "bundle-verify.txt", "build.log")}
        files["dbguard"] = b"#!/bin/sh\nexit 0\n"
        files["release-manifest.json"] = json.dumps(dict(
            identity, schema="changeguard-core-release/v2",
            files={"dbguard": digest(files["dbguard"])})).encode()
        verification = dict(identity, schema="changeguard-core-verification/v1", status=evidence_status)
        if mismatched_evidence:
            verification["commit"] = "c" * 40
        files["verification.json"] = json.dumps(verification).encode()
        files["SHA256SUMS"] = "".join(
            f"{digest(body)}  {name}\n" for name, body in files.items()).encode()
        if corrupt:
            files["dbguard"] += b"tampered\n"
        if unsafe:
            files["../outside"] = b"must not extract\n"
        archive = self.root / "fixture.tar.gz"
        with tarfile.open(archive, "w:gz") as handle:
            for name, body in files.items():
                member = tarfile.TarInfo(f"{NEW}/{name}")
                member.size = len(body)
                member.mode = 0o644
                handle.addfile(member, io.BytesIO(body))
        return archive

    def run_upgrade(self, *extra):
        result = subprocess.run([
            "bash", str(UPGRADE), "--version", "3.1.3",
            "--archive-url", "https://example.invalid/release.tar.gz",
            "--expected-sha256", digest(self.archive.read_bytes()),
            "--release-root", str(self.releases), "--current-link", str(self.current),
            "--service", "changeguard-test", "--health-timeout", "1",
            "--keep-archives", "2", "--core-only",
            "--core-health-url", self.core_url,
            "--health-url", "http://127.0.0.1:1/health/ready", *extra],
            env=self.env, text=True, capture_output=True, timeout=25)
        self.assertFalse(list(self.releases.glob(".download-*")), result.stderr)
        return result

    def read_events(self):
        return self.events.read_text().splitlines() if self.events.exists() else []

    def test_success_normalizes_version_and_preserves_previous_release(self):
        stale = self.releases / "changeguard-3-0-10"
        stale.mkdir()
        # Previous is intentionally oldest; it must survive pruning.
        os.utime(self.old, (1, 1))
        result = self.run_upgrade()
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertEqual(self.current.resolve(), self.releases / NEW)
        self.assertTrue(self.old.is_dir())
        self.assertFalse(stale.exists())
        self.assertIn("upgrade_status=ok version=3-1-3", result.stdout)
        self.assertIn(f"health:{NEW}", self.read_events())
        self.assertEqual((self.releases / NEW / "dbguard").stat().st_mode & 0o777, 0o755)

    def test_hyphenated_version_is_compatible(self):
        result = self.run_upgrade("--version", "3-1-3")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def assert_rollback(self, mode, successful):
        self.env["TEST_MODE"] = mode
        result = self.run_upgrade()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.current.resolve(), self.old)
        self.assertTrue((self.releases / NEW).is_dir(), "retain failed release for diagnosis")
        self.assertIn(f"restart:{OLD}", self.read_events())
        if successful:
            self.assertIn("upgrade_status=rolled_back", result.stderr)
            self.assertIn(f"health:{OLD}", self.read_events())
        else:
            self.assertIn("upgrade_status=rollback_failed", result.stderr)
            self.assertIn("manual_intervention_required=true", result.stderr)
            self.assertNotIn("upgrade_status=rolled_back", result.stderr)
        self.assertNotIn("upgrade_status=ok", result.stdout)

    def test_restart_failure_rolls_back(self):
        self.assert_rollback("restart-fail", True)

    def test_health_failure_rolls_back(self):
        self.assert_rollback("health-fail", True)

    def test_rollback_restart_failure_is_not_success(self):
        self.assert_rollback("both-restart-fail", False)

    def test_rollback_health_failure_is_not_success(self):
        self.assert_rollback("both-health-fail", False)

    def assert_no_switch(self, result):
        self.assertNotEqual(result.returncode, 0, result.stdout)
        self.assertEqual(self.current.resolve(), self.old)
        self.assertFalse((self.releases / NEW).exists())
        self.assertFalse(any(e.startswith("restart:") for e in self.read_events()))

    def test_checksum_failure_never_switches(self):
        result = self.run_upgrade("--expected-sha256", "0" * 64)
        self.assert_no_switch(result)
        self.assertIn("SHA256 mismatch", result.stderr)

    def test_release_directory_must_match_manifest_version(self):
        # Preflight rejects the mismatch first; --skip-preflight still exercises
        # the installer's own identity check, so both layers stay covered.
        self.make_archive(version="3-1-2", tag="v3.1.2")
        result = self.run_upgrade()
        self.assert_no_switch(result)
        self.assertIn("preflight failed", result.stderr)

        result = self.run_upgrade("--skip-preflight")
        self.assert_no_switch(result)
        self.assertIn("release directory does not match manifest version", result.stderr)

    def test_tag_must_match_manifest_version(self):
        # Preflight rejects the mismatch first; --skip-preflight still exercises
        # the installer's own identity check, so both layers stay covered.
        self.make_archive(tag="v3.1.2")
        result = self.run_upgrade()
        self.assert_no_switch(result)
        self.assertIn("preflight failed", result.stderr)
        self.assertIn("manifest_tag_version_mismatch", result.stderr)

        result = self.run_upgrade("--skip-preflight")
        self.assert_no_switch(result)
        self.assertIn("release tag does not match manifest version", result.stderr)

    def test_unpassed_evidence_is_rejected(self):
        self.make_archive(evidence_status="NOT_RUN")
        result = self.run_upgrade()
        self.assert_no_switch(result)
        self.assertIn("evidence is not passed", result.stderr)

    def test_mismatched_evidence_is_rejected(self):
        self.make_archive(mismatched_evidence=True)
        result = self.run_upgrade()
        self.assert_no_switch(result)
        self.assertIn("release identity mismatch: commit", result.stderr)

    def test_corrupt_member_is_rejected(self):
        self.make_archive(corrupt=True)
        self.assert_no_switch(self.run_upgrade())

    def test_path_traversal_is_rejected(self):
        # Preflight rejects the unsafe member first; --skip-preflight still
        # exercises the installer's own path safety check.
        self.make_archive(unsafe=True)
        result = self.run_upgrade()
        self.assert_no_switch(result)
        self.assertIn("archive_error=unsafe_member", result.stderr)
        self.assertIn("preflight failed", result.stderr)
        self.assertFalse((self.releases / "outside").exists())

        result = self.run_upgrade("--skip-preflight")
        self.assert_no_switch(result)
        self.assertIn("path traversal", result.stderr)
        self.assertFalse((self.releases / "outside").exists())

    def test_missing_previous_release_fails_before_download(self):
        self.current.unlink()
        result = self.run_upgrade()
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(self.read_events(), [])
        self.assertFalse(self.current.exists())

    def test_invalid_arguments_fail_before_download(self):
        for args in (("--version", "../bad"), ("--health-timeout", "0"),
                     ("--keep-archives", "1"), ("--archive-url", "http://example.invalid/a"),
                     ("--version",)):
            with self.subTest(args=args):
                self.assert_no_switch(self.run_upgrade(*args))
                self.assertEqual(self.read_events(), [])

    def test_concurrent_upgrade_is_rejected_before_download(self):
        import fcntl
        with (self.releases / ".upgrade.lock").open("w") as lock:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
            result = self.run_upgrade()
        self.assert_no_switch(result)
        self.assertIn("another upgrade is running", result.stderr)
        self.assertEqual(self.read_events(), [])

    def run_watcher(self, mode="ok", archive_name=None):
        self.env.update(TEST_MODE=mode, DBGUARD_UPGRADE_DIR=str(self.pending.parent),
                        CHANGEGUARD_RELEASE_ROOT=str(self.releases),
                        CHANGEGUARD_CURRENT_LINK=str(self.current),
                        CHANGEGUARD_SERVICE="changeguard-test", CHANGEGUARD_HEALTH_TIMEOUT="1",
                        CHANGEGUARD_INSTALL_SCRIPT=str(INSTALL), CHANGEGUARD_POLL_INTERVAL="0.05",
                        CHANGEGUARD_PREFLIGHT_SCRIPT=str(PREFLIGHT),
                        CHANGEGUARD_CORE_ONLY="1",
                        CHANGEGUARD_HEALTH_URL=self.core_url)
        archive_name = archive_name or f"{NEW}.tar.gz"
        shutil.copyfile(self.archive, self.pending / f"{NEW}.tar.gz")
        status_file = self.pending.parent / "status.json"
        history_file = self.pending.parent / "history.json"
        status_file.write_text(json.dumps(dict(state="uploaded", version="3-1-3",
                                              archive_sha256=digest(self.archive.read_bytes()))))
        (self.pending.parent / "apply.requested").write_text(archive_name)
        log = self.root / "watcher.log"
        with log.open("w") as output:
            process = subprocess.Popen(["bash", str(WATCHER)], env=self.env, stdout=output,
                                       stderr=output, start_new_session=True)
            try:
                deadline = time.monotonic() + 25
                while time.monotonic() < deadline:
                    try:
                        state = json.loads(status_file.read_text())
                        history = json.loads(history_file.read_text()) if history_file.exists() else []
                    except (json.JSONDecodeError, FileNotFoundError):
                        time.sleep(0.02)
                        continue
                    terminal = state.get("state") in {"success", "rollback", "failed"}
                    # Normally wait for history too so the watcher finished recording evidence.
                    if terminal and (history or archive_name.startswith("../")):
                        self.assertIsNone(process.poll(), log.read_text())
                        return state, history
                    if process.poll() is not None:
                        self.fail("watcher exited before recording outcome:\n" + log.read_text())
                    time.sleep(0.02)
                self.fail("watcher timeout:\n" + log.read_text())
            finally:
                if process.poll() is None:
                    os.killpg(process.pid, signal.SIGTERM)
                process.wait(timeout=5)

    def test_watcher_success(self):
        state, history = self.run_watcher()
        self.assertEqual(state["state"], "success")
        self.assertEqual(history[0]["state"], "success")
        self.assertEqual(self.current.resolve(), self.releases / NEW)

    def test_watcher_restart_failure_rolls_back_and_stays_alive(self):
        state, history = self.run_watcher("restart-fail")
        self.assertEqual(state["state"], "rollback")
        self.assertEqual(history[0]["state"], "rollback")
        self.assertEqual(self.current.resolve(), self.old)
        self.assertIn(f"health:{OLD}", self.read_events())

    def test_watcher_health_failure_rolls_back(self):
        state, history = self.run_watcher("health-fail")
        self.assertEqual(state["state"], "rollback")
        self.assertEqual(history[0]["state"], "rollback")
        self.assertEqual(self.current.resolve(), self.old)

    def test_watcher_failed_rollback_is_not_reported_as_rollback_success(self):
        state, history = self.run_watcher("both-restart-fail")
        self.assertEqual(state["state"], "failed")
        self.assertEqual(history[0]["state"], "failed")
        self.assertIn("人工", state["message"])
        self.assertEqual(self.current.resolve(), self.old)

    def test_watcher_unhealthy_rollback_is_not_reported_as_success(self):
        state, history = self.run_watcher("both-health-fail")
        self.assertEqual(state["state"], "failed")
        self.assertEqual(history[0]["state"], "failed")
        self.assertIn(f"health:{OLD}", self.read_events())

    def test_watcher_install_failure_keeps_previous_release(self):
        self.make_archive(evidence_status="NOT_RUN")
        state, history = self.run_watcher()
        self.assertEqual(state["state"], "failed")
        self.assertEqual(history[0]["state"], "failed")
        self.assertEqual(self.current.resolve(), self.old)
        # Preflight probes core health before installing, so a health event is
        # expected; no restart may happen and no new release may be installed.
        self.assertFalse(any(e.startswith("restart:") for e in self.read_events()))
        self.assertFalse((self.releases / NEW).exists())

    def test_watcher_missing_previous_release_rejects_upgrade(self):
        self.current.unlink()
        state, history = self.run_watcher()
        self.assertEqual(state["state"], "failed")
        self.assertEqual(history[0]["state"], "failed")
        self.assertFalse(self.current.exists())
        self.assertEqual(self.read_events(), [])

    def test_watcher_rejects_unsafe_archive_name(self):
        state, _ = self.run_watcher(archive_name="../outside.tar.gz")
        self.assertEqual(state["state"], "failed")
        self.assertEqual(self.current.resolve(), self.old)
        self.assertEqual(self.read_events(), [])

@unittest.skipUnless(sys.platform == "linux" and hasattr(os, "geteuid") and os.geteuid() == 0,
                     "requires Linux root (preflight checks real ownership and modes)")
class HealthHandler(http.server.BaseHTTPRequestHandler):
    """Serves the core readiness payload; TEST_CORE_MODE controls health."""

    def do_GET(self):
        mode = self.server.test_mode
        if mode != "ok":
            self.send_error(503)
            return
        body = json.dumps({
            "status": "ok",
            "build": {
                "version": "3.1.3",
                "commit": "a" * 40,
                "source_sha256": "b" * 64,
                "built_at": "2026-10-06T00:00:00Z",
            },
        }).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def log_message(self, *args):
        pass

class PreflightTests(unittest.TestCase):
    """只读预检的直接回归：它必须拦住问题，且**绝不**改动 release-root。"""

    def setUp(self):
        base = ROOT / "Temp/tests/preflight"
        base.mkdir(parents=True, exist_ok=True)
        self.temp = tempfile.TemporaryDirectory(prefix="case-", dir=base)
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.releases = self.root / "releases"
        self.releases.mkdir(mode=0o755)
        self.old = self.releases / OLD
        self.old.mkdir()
        (self.old / "dbguard").write_text("#!/bin/sh\nexit 0\n")
        (self.old / "dbguard").chmod(0o755)
        old_digest = hashlib.sha256(b"#!/bin/sh\nexit 0\n").hexdigest()
        (self.old / "SHA256SUMS").write_text(f"{old_digest}  dbguard\n")
        self.current = self.root / "current"
        self.current.symlink_to(self.old, target_is_directory=True)
        self.bin = self.root / "bin"
        self.bin.mkdir()
        self.archive = self.make_archive()
        self.env = dict(os.environ, PATH=str(self.bin) + os.pathsep + os.environ["PATH"],
                        TMPDIR=str(self.root), PYTHONDONTWRITEBYTECODE="1",
                        TEST_CORE_MODE="ok",
                        CHANGEGUARD_AGENT_HEALTH_URL="", AGENT_UPSTREAM_TOKEN="")
        self.httpd = http.server.HTTPServer(("127.0.0.1", 0), HealthHandler)
        self.httpd.test_mode = "ok"
        self.health_port = self.httpd.server_address[1]
        self.health_thread = threading.Thread(target=self.httpd.serve_forever, daemon=True)
        self.health_thread.start()
        self.addCleanup(self.httpd.shutdown)
        self.addCleanup(self.httpd.server_close)
        self.core_url = f"http://127.0.0.1:{self.health_port}/health/ready"

    def stub(self, name, body):
        path = self.bin / name
        path.write_text(body, encoding="utf-8")
        path.chmod(0o755)

    def make_archive(self, version="3-1-3", tag="v3.1.3"):
        files = {
            "release-manifest.json": json.dumps(dict(
                version=version, tag=tag, commit="a" * 40, source_sha256="b" * 64,
                schema="changeguard-core-release/v2", files={})).encode(),
        }
        archive = self.root / "fixture.tar.gz"
        with tarfile.open(archive, "w:gz") as handle:
            for name, body in files.items():
                member = tarfile.TarInfo(f"{NEW}/{name}")
                member.size = len(body)
                member.mode = 0o644
                handle.addfile(member, io.BytesIO(body))
        return archive

    def run_preflight(self, *extra):
        result = subprocess.run([
            "bash", str(PREFLIGHT),
            "--release-root", str(self.releases), "--current-link", str(self.current),
            *extra], env=self.env, text=True, capture_output=True, timeout=25)
        # 只读：任何情况下都不增删版本目录。
        self.assertEqual(sorted(p.name for p in self.releases.iterdir()), [OLD])
        return result

    def archive_args(self, **overrides):
        values = dict(archive=self.archive,
                      sha256=digest(self.archive.read_bytes()),
                      target="3.1.3")
        values.update(overrides)
        return ("--archive", str(values["archive"]),
                "--expected-sha256", values["sha256"],
                "--target-version", values["target"])

    def test_core_only_reports_not_applicable_and_passes(self):
        """显式 core-only：Agent/配套项记 not_applicable，其余检查全过即返回 0。"""
        result = self.run_preflight("--core-only", "--core-health-url", self.core_url, *self.archive_args())
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("preflight_check=core_identity status=passed", result.stdout)
        self.assertIn("preflight_check=rollback_capability status=passed", result.stdout)
        self.assertIn("preflight_check=archive_identity status=passed", result.stdout)
        self.assertIn("preflight_check=target_version status=passed", result.stdout)
        # not_applicable 不是 not_run：显式声明不产生 incomplete。
        self.assertIn("preflight_check=agent_identity status=not_applicable", result.stdout)
        self.assertIn("preflight_check=identity_pairing status=not_applicable", result.stdout)
        self.assertIn("preflight_status=passed", result.stdout)

    def test_missing_agent_declaration_is_incomplete(self):
        """未声明 Agent 且未声明 core-only：incomplete，其余检查仍执行。"""
        result = self.run_preflight("--core-health-url", self.core_url, *self.archive_args())
        self.assertEqual(result.returncode, 3, result.stdout + result.stderr)
        self.assertIn("preflight_check=agent_identity status=not_run", result.stdout)
        self.assertIn("preflight_check=identity_pairing status=not_run", result.stdout)
        self.assertIn("preflight_status=incomplete", result.stdout)
        self.assertNotIn("preflight_status=passed", result.stdout)

    def test_core_identity_without_agent_url_still_verifies_core(self):
        result = self.run_preflight("--core-health-url", self.core_url, *self.archive_args())
        self.assertIn("preflight_check=core_identity status=passed", result.stdout)
        self.assertIn("version=3.1.3", result.stdout)
        self.assertIn("commit=" + "a" * 40, result.stdout)

    def test_unreachable_core_fails_closed(self):
        self.httpd.test_mode = "down"
        result = self.run_preflight("--core-only", "--core-health-url", self.core_url, *self.archive_args())
        self.assertEqual(result.returncode, 1)
        self.assertIn("preflight_check=core_identity status=failed", result.stdout)
        self.assertIn("preflight_status=failed", result.stdout)

    def test_missing_rollback_target_fails_closed(self):
        self.current.unlink()
        result = self.run_preflight("--core-only", "--core-health-url", self.core_url, *self.archive_args())
        self.assertEqual(result.returncode, 1)
        self.assertIn("preflight_check=rollback_capability status=failed", result.stdout)

    def test_current_pointing_outside_release_root_fails_closed(self):
        outside = self.root / "elsewhere"
        outside.mkdir()
        (outside / "dbguard").write_text("#!/bin/sh\nexit 0\n")
        (outside / "dbguard").chmod(0o755)
        self.current.unlink()
        self.current.symlink_to(outside, target_is_directory=True)
        result = self.run_preflight("--core-only", "--core-health-url", self.core_url, *self.archive_args())
        self.assertEqual(result.returncode, 1)
        self.assertIn("not directly inside release-root", result.stdout)

    def test_rollback_sums_missing_dbguard_fails_closed(self):
        """清单不覆盖 dbguard 就不是可核对的回滚目标。"""
        (self.old / "SHA256SUMS").write_text(f"{hashlib.sha256(b'x').hexdigest()}  other-file\n")
        result = self.run_preflight("--core-only", "--core-health-url", self.core_url, *self.archive_args())
        self.assertEqual(result.returncode, 1)
        self.assertIn("SHA256SUMS does not cover dbguard", result.stdout)

    def test_archive_sha_mismatch_fails_closed(self):
        result = self.run_preflight("--core-only", "--core-health-url", self.core_url, *self.archive_args(sha256="0" * 64))
        self.assertEqual(result.returncode, 1)
        self.assertIn("preflight_check=archive_identity status=failed", result.stdout)

    def test_target_version_mismatch_fails_closed(self):
        result = self.run_preflight("--core-only", "--core-health-url", self.core_url, *self.archive_args(target="3.1.2"))
        self.assertEqual(result.returncode, 1)
        self.assertIn("preflight_check=target_version status=failed", result.stdout)

    def test_hyphenated_target_version_is_normalized(self):
        result = self.run_preflight("--core-only", "--core-health-url", self.core_url, *self.archive_args(target="3-1-3"))
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn("preflight_check=target_version status=passed", result.stdout)

    def test_missing_agent_token_fails_closed_when_agent_declared(self):
        """声明了 Agent 却没有凭据，就是核对不了，不能当作通过。"""
        self.env.pop("AGENT_UPSTREAM_TOKEN", None)
        result = self.run_preflight("--agent-health-url", "http://127.0.0.1:8091/api/agent/healthz",
                                    *self.archive_args())
        self.assertEqual(result.returncode, 1)
        self.assertIn("preflight_check=agent_identity status=failed", result.stdout)

    def test_requires_archive_to_be_regular_file(self):
        link = self.root / "linked.tar.gz"
        link.symlink_to(self.archive)
        result = subprocess.run([
            "bash", str(PREFLIGHT), "--core-only", "--archive", str(link),
            "--expected-sha256", digest(self.archive.read_bytes()),
            "--release-root", str(self.releases), "--current-link", str(self.current)],
            env=self.env, text=True, capture_output=True, timeout=25)
        self.assertEqual(result.returncode, 64)
        self.assertIn("preflight_error=", result.stderr)

    def test_rejects_unknown_option(self):
        result = self.run_preflight("--definitely-not-an-option")
        self.assertEqual(result.returncode, 64)
        self.assertIn("preflight_error=unknown option", result.stderr)


if __name__ == "__main__":
    unittest.main()
