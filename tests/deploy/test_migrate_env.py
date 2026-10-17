"""Regressions for the 4.0 environment migration script.

The script is POSIX bash, so the tests drive it through a Linux bash (WSL on
Windows). They are skipped, never silently passed, when no Linux bash exists.
Fixtures live under the ignored Temp directory and no real environment file is
touched.
"""

import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[2]
SCRIPT = ROOT / "deploy/production/changeguard-migrate-env.sh"
BASH = shutil.which("bash")


def bash_path(path: Path) -> str:
    """Translate a local path so a WSL bash can reach the same file."""
    text = path.resolve().as_posix()
    if len(text) > 2 and text[1] == ":":
        return f"/mnt/{text[0].lower()}{text[2:]}"
    return text


def linux_bash() -> bool:
    if not BASH:
        return False
    try:
        probe = subprocess.run(
            [BASH, "-c", "uname -s"], capture_output=True, text=True, timeout=60
        )
    except (OSError, subprocess.SubprocessError):
        return False
    return probe.returncode == 0 and probe.stdout.strip() == "Linux"


@unittest.skipUnless(linux_bash(), "requires a Linux bash (install WSL or run on Linux)")
class MigrateEnvironmentTests(unittest.TestCase):
    def setUp(self):
        base = ROOT / "Temp/tests/migrate-env"
        base.mkdir(parents=True, exist_ok=True)
        self.temp = tempfile.TemporaryDirectory(prefix="case-", dir=base)
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)

    def write(self, name: str, content: str) -> Path:
        path = self.root / name
        path.write_text(content, encoding="utf-8", newline="\n")
        return path

    def run_script(self, path: Path, *args: str) -> subprocess.CompletedProcess:
        return subprocess.run(
            [BASH, bash_path(SCRIPT), "--file", bash_path(path), *args],
            capture_output=True,
            text=True,
            timeout=120,
        )

    def test_dry_run_reports_keys_without_touching_the_file(self):
        path = self.write(
            "dry.env",
            "# core\nCHANGEGUARD_LISTEN_ADDRESS=127.0.0.1:8080\n"
            "DBGUARD_ENV_PROFILE=production\n\nDBGUARD_WORKERS=2\n",
        )
        before = path.read_bytes()
        result = self.run_script(path, "--dry-run")
        self.assertEqual(result.returncode, 3, result.stderr)
        self.assertEqual(before, path.read_bytes())
        self.assertEqual(result.stdout.count("migrate_key="), 2)
        self.assertIn("DBGUARD_WORKERS->CHANGEGUARD_WORKERS", result.stdout)
        self.assertNotIn("production", result.stdout)
        self.assertFalse((self.root / "dry.env.pre-4.0").exists())

    def test_migration_keeps_comments_values_and_is_idempotent(self):
        path = self.write(
            "real.env",
            "# core\nCHANGEGUARD_LISTEN_ADDRESS=127.0.0.1:8080\n"
            "DBGUARD_ENV_PROFILE=production\n\nDBGUARD_WORKERS=2\n",
        )
        result = self.run_script(path)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("migrate_result=migrated", result.stdout)
        body = path.read_text(encoding="utf-8")
        self.assertNotIn("DBGUARD_", body)
        self.assertIn("# core", body)
        self.assertIn("CHANGEGUARD_ENV_PROFILE=production", body)
        self.assertIn("CHANGEGUARD_WORKERS=2", body)
        self.assertEqual(body.count("\n\n"), 1)
        backup = self.root / "real.env.pre-4.0"
        self.assertTrue(backup.exists())
        self.assertIn("DBGUARD_WORKERS=2", backup.read_text(encoding="utf-8"))
        rerun = self.run_script(path)
        self.assertEqual(rerun.returncode, 0, rerun.stderr)
        self.assertIn("already_canonical", rerun.stdout)

    def test_conflicting_values_fail_closed_without_writing(self):
        path = self.write("clash.env", "CHANGEGUARD_WORKERS=4\nDBGUARD_WORKERS=2\n")
        before = path.read_bytes()
        result = self.run_script(path)
        self.assertEqual(result.returncode, 1)
        self.assertEqual(before, path.read_bytes())
        self.assertIn("CHANGEGUARD_WORKERS and DBGUARD_WORKERS", result.stderr)
        self.assertNotIn("=2", result.stderr)
        self.assertFalse((self.root / "clash.env.pre-4.0").exists())

    def test_identical_values_collapse_to_one_canonical_line(self):
        path = self.write("same.env", "CHANGEGUARD_WORKERS=2\nDBGUARD_WORKERS=2\n")
        result = self.run_script(path)
        self.assertEqual(result.returncode, 0, result.stderr)
        body = path.read_text(encoding="utf-8")
        self.assertEqual(body, "CHANGEGUARD_WORKERS=2\n")

    def test_canonical_file_is_left_untouched(self):
        path = self.write("plain.env", "CHANGEGUARD_WORKERS=2\n")
        result = self.run_script(path)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("already_canonical", result.stdout)
        self.assertFalse((self.root / "plain.env.pre-4.0").exists())

    def test_malformed_and_unsupported_lines_fail_closed(self):
        cases = {
            "bad.env": "CHANGEGUARD_WORKERS=2\nnot-an-assignment\n",
            "export.env": "export DBGUARD_WORKERS=2\n",
            "dup.env": "DBGUARD_WORKERS=2\nDBGUARD_WORKERS=3\n",
            "crlf.env": "DBGUARD_WORKERS=2\r\n",
        }
        for name, content in cases.items():
            with self.subTest(name=name):
                path = self.write(name, content)
                before = path.read_bytes()
                result = self.run_script(path)
                self.assertEqual(result.returncode, 1, result.stdout)
                self.assertEqual(before, path.read_bytes())

    def test_existing_backup_is_never_overwritten(self):
        path = self.write("guarded.env", "DBGUARD_WORKERS=2\n")
        backup = self.root / "guarded.env.pre-4.0"
        backup.write_text("keep-me\n", encoding="utf-8")
        result = self.run_script(path)
        self.assertEqual(result.returncode, 1)
        self.assertEqual(backup.read_text(encoding="utf-8"), "keep-me\n")
        self.assertEqual(path.read_text(encoding="utf-8"), "DBGUARD_WORKERS=2\n")

    def test_usage_errors_and_help(self):
        missing = self.run_script(self.root / "missing.env")
        self.assertEqual(missing.returncode, 1)
        no_file = subprocess.run(
            [BASH, bash_path(SCRIPT)], capture_output=True, text=True, timeout=120
        )
        self.assertEqual(no_file.returncode, 64)
        help_result = subprocess.run(
            [BASH, bash_path(SCRIPT), "--help"], capture_output=True, text=True, timeout=120
        )
        self.assertEqual(help_result.returncode, 0)
        self.assertIn("Usage:", help_result.stdout)


if __name__ == "__main__":
    sys.exit(unittest.main())
