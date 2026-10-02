"""Linux/root-only upgrade regressions; real installer, synthetic archives and I/O.

Run: sudo python3 -B -m unittest discover -s tests/deploy -v
All fixtures live under the project's ignored Temp directory. No real systemd
services, network, production data, backup or restore operations are involved.
"""

import hashlib
import io
import json
import os
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
printf 'health:%s\n' "$active" >> "$TEST_EVENTS"
case "$TEST_MODE:$active" in
  health-fail:changeguard-3-1-3|both-health-fail:*) exit 1 ;;
esac
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
            "--keep-archives", "2", *extra],
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
        self.make_archive(version="3-1-2", tag="v3.1.2")
        result = self.run_upgrade()
        self.assert_no_switch(result)
        self.assertIn("release directory does not match manifest version", result.stderr)

    def test_tag_must_match_manifest_version(self):
        self.make_archive(tag="v3.1.2")
        result = self.run_upgrade()
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
        self.make_archive(unsafe=True)
        result = self.run_upgrade()
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
                        CHANGEGUARD_INSTALL_SCRIPT=str(INSTALL), CHANGEGUARD_POLL_INTERVAL="0.05")
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
        self.assertEqual(self.read_events(), [])

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


if __name__ == "__main__":
    unittest.main()
