"""
Tests for the download crash-cleanup trap.

download.py drops a ``{"status": "in-progress"}`` marker when it
creates the run dir and atomically overwrites run.json with the
terminal ``status == "complete"`` manifest at the end. A
crashed/interrupted download therefore leaves a run dir with no
run.json (crash before the marker) or one still carrying the
in-progress marker. download.cleanup_incomplete_run_dir() removes any
run dir that is not a completed dump so orphans don't accumulate in
bronze; a completed dump (status=complete, or a legacy statusless
manifest) is preserved.

run()'s trap calls it on every failure EXCEPT under --debug, which keeps
the dir so its captures survive the crash they explain; `prune` reclaims
it instead. The predicate itself is flag-agnostic — that decision lives
at the call site — so these tests pin the removal rule, and
test_cli_defaults pins the flag surface.

Run from the repo root inside the container:
    python3 -m unittest discover tests
"""

from __future__ import annotations

import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT))

import download  # noqa: E402  (imports playwright + collectorkit; present in image)


class CleanupIncompleteRunDirTests(unittest.TestCase):
    def test_removes_dir_without_marker(self):
        with tempfile.TemporaryDirectory() as tmp:
            run_dir = Path(tmp) / "20260101T120000Z"
            run_dir.mkdir()
            (run_dir / "accounts.json").write_text("[]", encoding="utf-8")
            removed = download.cleanup_incomplete_run_dir(run_dir)
            self.assertTrue(removed)
            self.assertFalse(run_dir.exists())

    def test_removes_dir_with_in_progress_marker(self):
        # A walk that crashed after dropping the in-progress marker
        # but before finalising: run.json exists but is not complete,
        # so it must be removed (the pre-status guard, which keyed on
        # marker *absence*, would wrongly have kept it).
        with tempfile.TemporaryDirectory() as tmp:
            run_dir = Path(tmp) / "20260101T120000Z"
            run_dir.mkdir()
            (run_dir / "run.json").write_text(
                '{"status": "in-progress"}', encoding="utf-8")
            (run_dir / "accounts.json").write_text("[]", encoding="utf-8")
            removed = download.cleanup_incomplete_run_dir(run_dir)
            self.assertTrue(removed)
            self.assertFalse(run_dir.exists())

    def test_keeps_dir_with_complete_status(self):
        with tempfile.TemporaryDirectory() as tmp:
            run_dir = Path(tmp) / "20260101T120000Z"
            run_dir.mkdir()
            (run_dir / "run.json").write_text(
                '{"status": "complete"}', encoding="utf-8")
            removed = download.cleanup_incomplete_run_dir(run_dir)
            self.assertFalse(removed)
            self.assertTrue(run_dir.exists())  # completed dump preserved

    def test_keeps_dir_with_statusless_marker(self):
        # A legacy statusless run.json is a pre-status complete dump.
        with tempfile.TemporaryDirectory() as tmp:
            run_dir = Path(tmp) / "20260101T120000Z"
            run_dir.mkdir()
            (run_dir / "run.json").write_text("{}", encoding="utf-8")
            removed = download.cleanup_incomplete_run_dir(run_dir)
            self.assertFalse(removed)
            self.assertTrue(run_dir.exists())  # completed dump preserved

    def test_noop_when_dir_absent(self):
        with tempfile.TemporaryDirectory() as tmp:
            # Failure before mkdir (or a dry run): nothing to remove.
            run_dir = Path(tmp) / "never_created"
            removed = download.cleanup_incomplete_run_dir(run_dir)
            self.assertFalse(removed)


if __name__ == "__main__":
    unittest.main()
