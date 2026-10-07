"""
Tests for load_transactions_csv: span-DELETE-then-INSERT per CSV.

A transactions export can hold fewer days than the window it was
requested for, or no rows at all. The loader therefore deletes only
between the CSV's own first and last row before inserting, so a narrow
or empty export never drops rows that an earlier dump loaded and this
one does not replace.

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
ACCOUNT = "SYN-0001"
HEADER = ";".join(load.EXPECTED_CSV_HEADER)


def _row(date: str, kind: str, net: str, balance: str) -> str:
    """One CSV line in the export's shape: `Date;Order #;Transaction;...`."""
    return ";".join([
        date, "00000000", kind, "", "", "", "", "", "", "",
        net, balance, "CHF",
    ])


def _write_csv(path: Path, rows: list[str]) -> Path:
    path.write_text("\n".join([HEADER, *rows]) + "\n", encoding="utf-8")
    return path


def _dates(conn) -> list[str]:
    return [
        r["d"] for r in conn.execute(
            "SELECT json_extract(payload, '$.date_raw') AS d "
            "FROM transactions ORDER BY occurred_at;")
    ]


class LoadTransactionsCsvTests(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.tmp = Path(self._tmp.name)
        self.conn = load.open_db(self.tmp / "swissquote.db")
        load.apply_migrations(self.conn, MIGRATIONS)

    def tearDown(self):
        self.conn.close()
        self._tmp.cleanup()

    def _load(self, name: str, rows: list[str]) -> int:
        return load.load_transactions_csv(
            self.conn, ACCOUNT, _write_csv(self.tmp / name, rows))

    def test_narrower_export_keeps_rows_outside_its_span(self):
        # The first export covers three months; the second was requested
        # for an overlapping window but holds only its last two rows.
        self._load("a.csv", [
            _row("05-01-2026 10:00:00", "Credit", "100.00", "100.00"),
            _row("12-02-2026 09:30:00", "Fees", "-5.00", "95.00"),
            _row("10-03-2026 14:00:00", "Interest", "1.00", "96.00"),
            _row("20-03-2026 08:00:00", "Fees", "-2.00", "94.00"),
        ])
        n = self._load("b.csv", [
            _row("10-03-2026 14:00:00", "Interest", "1.00", "96.00"),
            _row("20-03-2026 08:00:00", "Fees", "-2.00", "94.00"),
        ])
        self.assertEqual(n, 2)
        self.assertEqual(_dates(self.conn), [
            "05-01-2026 10:00:00", "12-02-2026 09:30:00",
            "10-03-2026 14:00:00", "20-03-2026 08:00:00",
        ])

    def test_empty_export_deletes_nothing(self):
        self._load("a.csv", [
            _row("05-01-2026 10:00:00", "Credit", "100.00", "100.00"),
        ])
        self.assertEqual(self._load("b.csv", []), 0)
        self.assertEqual(_dates(self.conn), ["05-01-2026 10:00:00"])

    def test_amended_row_inside_span_converges(self):
        # A row the source later drops or rewrites inside the span of a
        # newer export is replaced by what that export holds.
        self._load("a.csv", [
            _row("05-01-2026 10:00:00", "Credit", "100.00", "100.00"),
            _row("12-02-2026 09:30:00", "Fees", "-5.00", "95.00"),
            _row("20-03-2026 08:00:00", "Fees", "-2.00", "93.00"),
        ])
        self._load("b.csv", [
            _row("12-02-2026 09:30:00", "Fees", "-4.00", "96.00"),
            _row("20-03-2026 08:00:00", "Fees", "-2.00", "94.00"),
        ])
        got = [
            (r["d"], r["net_amount"]) for r in self.conn.execute(
                "SELECT json_extract(payload, '$.date_raw') AS d, net_amount "
                "FROM transactions ORDER BY occurred_at;")
        ]
        self.assertEqual(got, [
            ("05-01-2026 10:00:00", 100.0),
            ("12-02-2026 09:30:00", -4.0),
            ("20-03-2026 08:00:00", -2.0),
        ])

    def test_span_is_per_account(self):
        self.conn.execute(
            "INSERT INTO transactions(account_external_id, occurred_at,"
            " transaction_type, currency, net_amount, payload) "
            "VALUES ('SYN-0002', ?, 'Fees', 'CHF', -1.0, '{}');",
            (load._zurich_to_utc_epoch("12-02-2026 09:30:00"),),
        )
        self._load("a.csv", [
            _row("05-01-2026 10:00:00", "Credit", "100.00", "100.00"),
            _row("20-03-2026 08:00:00", "Fees", "-2.00", "98.00"),
        ])
        n = self.conn.execute(
            "SELECT COUNT(*) FROM transactions "
            "WHERE account_external_id = 'SYN-0002';").fetchone()[0]
        self.assertEqual(n, 1)


if __name__ == "__main__":
    unittest.main()
