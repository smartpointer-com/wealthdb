"""
Silver-schema tests for the positions name + isin promotion.

Run from the repo root inside the container:
    python3 -m unittest discover tests

Pure stdlib — no pytest, no fixtures binary files. The tests cover:
  - `parse_position_details` builds the right (symbol, currency)
    lookup, including the symbol-only fallback when currency is null.
  - Migrations 0001..0006 apply cleanly in sequence; the positions
    table has the new name + isin columns.
  - A non-ASCII instrument name round-trips through SQLite without
    encoding damage.
"""

from __future__ import annotations

import json
import sqlite3
import sys
import tempfile
import unittest
from pathlib import Path

# Make the repo root importable so we can pull in load.py helpers.
ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT))

import load  # noqa: E402

MIGRATIONS = ROOT / "migrations"


def _apply_all_migrations(db_path: Path) -> sqlite3.Connection:
    conn = load.open_db(db_path)
    load.apply_migrations(conn, MIGRATIONS)
    return conn


class ParsePositionDetailsTests(unittest.TestCase):
    def test_lookup_keys(self):
        with tempfile.TemporaryDirectory() as tmp:
            p = Path(tmp) / "position_details.json"
            p.write_text(json.dumps([
                {"symbol": "FAKE1", "currency": "CHF",
                 "isin": "XX0000000001",
                 "name": "Fake Equity 1 (placeholder fixture)"},
                {"symbol": "NOCCY", "currency": None,
                 "isin": "XX0000000002", "name": "currency-less fallback"},
            ]), encoding="utf-8")
            lookup = load.parse_position_details(p)
            self.assertEqual(lookup[("FAKE1", "CHF")]["isin"], "XX0000000001")
            self.assertEqual(lookup[("NOCCY", None)]["name"],
                             "currency-less fallback")
            # Empty/missing file returns an empty dict.
            self.assertEqual(load.parse_position_details(Path(tmp) / "missing.json"), {})


class MigrationSequenceTests(unittest.TestCase):
    def test_migrations_apply_and_columns_exist(self):
        with tempfile.TemporaryDirectory() as tmp:
            db_path = Path(tmp) / "silver.db"
            conn = _apply_all_migrations(db_path)
            self.assertEqual(load.current_schema_version(conn), 6)
            cols = [r["name"] for r in conn.execute(
                "PRAGMA table_info(positions);")]
            self.assertIn("name", cols)
            self.assertIn("isin", cols)
            self.assertIn("source", cols)
            # accounts table renamed by 0005: account_product, not account_type
            acct_cols = [r["name"] for r in conn.execute(
                "PRAGMA table_info(accounts);")]
            self.assertIn("account_product", acct_cols)
            self.assertNotIn("account_type", acct_cols)
            # name/isin are nullable (added without DEFAULT in 0003).
            for r in conn.execute(
                "SELECT name, type, [notnull], dflt_value "
                "FROM pragma_table_info('positions') "
                "WHERE name IN ('name', 'isin');"
            ):
                self.assertEqual(r["notnull"], 0, f"{r['name']} should be nullable")
                self.assertIsNone(r["dflt_value"])
            conn.close()


class NonAsciiRoundTripTests(unittest.TestCase):
    """A regression guard against UTF-8 mishandling in the load path."""

    def test_non_ascii_name_survives(self):
        non_ascii_name = "Société Générale ČEZ Nestlé Telefónica €"
        with tempfile.TemporaryDirectory() as tmp:
            db_path = Path(tmp) / "silver.db"
            conn = _apply_all_migrations(db_path)
            conn.execute(
                "INSERT INTO positions("
                " snapshot_at, account_external_id, symbol, currency,"
                " name, isin, payload) "
                "VALUES (?, ?, ?, ?, ?, ?, ?);",
                (1_700_000_000, "0000001", "FAKEX", "EUR",
                 non_ascii_name, "XX0000000003",
                 '{"asset_class":"Equities"}'),
            )
            row = conn.execute(
                "SELECT name, isin, currency FROM positions WHERE symbol = 'FAKEX';"
            ).fetchone()
            self.assertEqual(row["name"], non_ascii_name)
            self.assertEqual(row["isin"], "XX0000000003")
            self.assertEqual(row["currency"], "EUR")
            # Belt-and-braces: re-open the connection and re-read, to
            # rule out an in-process cache hiding an encoding bug.
            conn.close()
            conn2 = sqlite3.connect(str(db_path))
            conn2.row_factory = sqlite3.Row
            row2 = conn2.execute(
                "SELECT name FROM positions WHERE symbol = 'FAKEX';"
            ).fetchone()
            self.assertEqual(row2["name"], non_ascii_name)
            conn2.close()


if __name__ == "__main__":
    unittest.main()
