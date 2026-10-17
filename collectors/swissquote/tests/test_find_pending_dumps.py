"""
Tests for the dumps a load picks: only complete bronze dumps are loaded.

download.py writes ``{"status": "in-progress"}`` at run-dir creation
and atomically overwrites run.json with the terminal
``status == "complete"`` manifest at the end. A dir with no run.json,
or one still carrying the in-progress marker, is a crashed, interrupted
or still-running download; loading its partial artefacts would leak a
partial snapshot into silver. The load skips such a dir without a
dump_runs row, so it loads once its walk finalises. An unreadable
run.json is not skipped: the load fails on it, so the damage surfaces
(prune keeps such a dir too).

Run from the repo root inside the container:
    python3 -m unittest discover tests
"""

from __future__ import annotations

import json
import sys
import tempfile
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT))

import load  # noqa: E402
from tests.helpers import apply_all_migrations  # noqa: E402


def _load(bronze: Path) -> int:
    return load.main(["--silver-db", str(bronze / "swissquote.db"),
                      "--bronze-dir", str(bronze)])


def _loaded(bronze: Path) -> set[int]:
    conn = apply_all_migrations(bronze / "swissquote.db")
    try:
        return load.silver.loaded_snapshots(conn)
    finally:
        conn.close()


class PendingDumpsTests(unittest.TestCase):
    def test_dir_without_run_json_neither_loaded_nor_recorded(self):
        # Only a stray artefact, no run.json: a download that crashed
        # before its first manifest write, or one still writing.
        with tempfile.TemporaryDirectory() as tmp:
            bronze = Path(tmp)
            d = bronze / "20260102T120000Z"
            d.mkdir()
            (d / "accounts.json").write_text("[]", encoding="utf-8")
            (bronze / "manual").mkdir()        # a non-dump dir, ignored

            self.assertEqual(_load(bronze), 0)
            self.assertEqual(_loaded(bronze), set())

    def test_in_progress_and_dry_run_dumps_not_recorded(self):
        # A crashed walk leaves status="in-progress"; a dry-run shell
        # leaves status="dry-run". Neither is loaded or recorded.
        with tempfile.TemporaryDirectory() as tmp:
            bronze = Path(tmp)
            for name, status in (("20260102T120000Z", "in-progress"),
                                 ("20260103T120000Z", "dry-run")):
                d = bronze / name
                d.mkdir()
                (d / "run.json").write_text(
                    json.dumps({"status": status, "customer_id": "SYN-0001"}),
                    encoding="utf-8")

            self.assertEqual(_load(bronze), 0)
            self.assertEqual(_loaded(bronze), set())

    def test_corrupt_run_json_fails_the_load(self):
        # An unparseable or undecodable run.json is not evidence of an
        # unfinished walk: the load fails on it instead of skipping it
        # for ever.
        for body in (b"{not valid json", b'{"status": "\xff\xfe"}'):
            with tempfile.TemporaryDirectory() as tmp:
                bronze = Path(tmp)
                d = bronze / "20260101T120000Z"
                d.mkdir()
                (d / "run.json").write_bytes(body)
                with self.assertRaises(ValueError):
                    _load(bronze)
                self.assertEqual(_loaded(bronze), set())


if __name__ == "__main__":
    unittest.main()
