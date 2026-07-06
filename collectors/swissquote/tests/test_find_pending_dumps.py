"""
Tests for find_pending_dumps: only complete bronze dumps are loaded.

download.py writes ``{"status": "in-progress"}`` at run-dir creation
and atomically overwrites run.json with the terminal
``status == "complete"`` manifest at the end. The loader ingests a
dump only once run.json carries ``status == "complete"`` (a legacy
statusless manifest counts too). A dir with no run.json, or one still
carrying the in-progress marker, is a crashed/interrupted or
still-running download; loading its partial artefacts would leak a
partial snapshot into silver, so it is skipped with a warning rather
than aborting the whole load — one bad download never blocks the good
dumps around it.

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

import load  # noqa: E402

MIGRATIONS = ROOT / "migrations"


def _apply_all_migrations(db_path: Path):
    conn = load.open_db(db_path)
    load.apply_migrations(conn, MIGRATIONS)
    return conn


class FindPendingDumpsTests(unittest.TestCase):
    def test_skips_incomplete_dumps(self):
        with tempfile.TemporaryDirectory() as tmp:
            bronze = Path(tmp)
            db_path = bronze / "swissquote.db"
            conn = _apply_all_migrations(db_path)

            # Complete dump — has run.json.
            complete = bronze / "20260101T120000Z"
            complete.mkdir()
            (complete / "run.json").write_text("{}", encoding="utf-8")

            # Incomplete dump — only a stray artefact, no run.json
            # (simulates a crashed download).
            incomplete = bronze / "20260102T120000Z"
            incomplete.mkdir()
            (incomplete / "accounts.json").write_text("[]", encoding="utf-8")

            # Non-dump dir — ignored regardless.
            (bronze / "manual").mkdir()

            pending = load.find_pending_dumps(conn, bronze)
            names = sorted(p.name for p in pending)
            self.assertEqual(names, ["20260101T120000Z"])
            conn.close()

    def test_skips_in_progress_and_dry_run_dumps(self):
        # A crashed walk leaves status="in-progress"; a hypothetical
        # dry-run shell leaves status="dry-run". Neither is loadable —
        # only status="complete" (or a legacy statusless manifest) is.
        with tempfile.TemporaryDirectory() as tmp:
            bronze = Path(tmp)
            conn = _apply_all_migrations(bronze / "swissquote.db")

            complete = bronze / "20260101T120000Z"
            complete.mkdir()
            (complete / "run.json").write_text(
                '{"status": "complete"}', encoding="utf-8")

            legacy = bronze / "20260101T130000Z"       # statusless manifest
            legacy.mkdir()
            (legacy / "run.json").write_text("{}", encoding="utf-8")

            in_progress = bronze / "20260102T120000Z"  # crashed marker
            in_progress.mkdir()
            (in_progress / "run.json").write_text(
                '{"status": "in-progress"}', encoding="utf-8")

            dry = bronze / "20260103T120000Z"
            dry.mkdir()
            (dry / "run.json").write_text(
                '{"status": "dry-run"}', encoding="utf-8")

            pending = sorted(p.name for p in
                             load.find_pending_dumps(conn, bronze))
            self.assertEqual(
                pending, ["20260101T120000Z", "20260101T130000Z"])
            conn.close()

    def test_skips_corrupt_run_json(self):
        # An unparseable run.json is not evidence of completeness; the
        # loader skips it rather than crashing on the bad JSON later.
        with tempfile.TemporaryDirectory() as tmp:
            bronze = Path(tmp)
            conn = _apply_all_migrations(bronze / "swissquote.db")
            d = bronze / "20260101T120000Z"
            d.mkdir()
            (d / "run.json").write_text("{not valid json", encoding="utf-8")
            self.assertEqual(load.find_pending_dumps(conn, bronze), [])
            conn.close()

    def test_already_loaded_dumps_excluded(self):
        with tempfile.TemporaryDirectory() as tmp:
            bronze = Path(tmp)
            db_path = bronze / "swissquote.db"
            conn = _apply_all_migrations(db_path)

            d = bronze / "20260103T120000Z"
            d.mkdir()
            (d / "run.json").write_text("{}", encoding="utf-8")

            # First pass: it's pending.
            self.assertEqual(
                [p.name for p in load.find_pending_dumps(conn, bronze)],
                ["20260103T120000Z"],
            )

            # Record it as loaded; now it must drop out.
            ts = load._run_dir_to_epoch("20260103T120000Z")
            conn.execute(
                "INSERT INTO dump_runs(snapshot_at, silver_schema_version, "
                "run_dir) VALUES (?, ?, ?);",
                (ts, load.silver.current_schema_version(conn), str(d)),
            )
            self.assertEqual(load.find_pending_dumps(conn, bronze), [])
            conn.close()


if __name__ == "__main__":
    unittest.main()
