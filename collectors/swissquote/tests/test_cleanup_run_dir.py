"""
Tests for the download crash-cleanup trap.

A crashed/interrupted download leaves a partial run_dir without the
run.json completion marker. download.cleanup_incomplete_run_dir()
removes it so orphans don't accumulate in bronze.

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

    def test_keeps_dir_with_marker(self):
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
