"""
Tests for find_pending_dumps: incomplete bronze dumps (no run.json)
must be skipped, not fatal.

A crashed/interrupted download leaves a timestamp-named dir without
the final run.json completion marker. The loader skips such dirs
with a warning so one bad download never blocks loading the good
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
