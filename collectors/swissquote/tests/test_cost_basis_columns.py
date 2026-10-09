"""
Tests for the cost-basis columns of migration 0006.

The loader fills the columns from each source shape:
  - a live Positions export row: average_cost, price_quote 'unit' and
    the two CHF columns;
  - a Portfolio Performance statement row: average_cost, and
    price_quote 'percent' when the statement prints the price with "%";
  - a transactions CSV row: quantity, unit_price, price_quote, fees
    and accrued_interest.
A blank or placeholder cell is NULL and a printed zero stays 0. The
migration backfills rows loaded before it to the same values the
loader writes.

Every fixture is synthetic.
"""

from __future__ import annotations

import json
import shutil
import sys
import tempfile
import unittest
from pathlib import Path
from unittest import mock

import xlwt

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT))

import load  # noqa: E402

MIGRATIONS = ROOT / "migrations"
ACCOUNT = "SYN-0001"
SNAPSHOT_AT = 1_700_000_000
CSV_HEADER = ";".join(load.EXPECTED_CSV_HEADER)

TX_COLUMNS = ("quantity", "unit_price", "price_quote", "fees",
              "accrued_interest")
POSITION_COLUMNS = ("average_cost", "price_quote", "market_value_chf",
                    "unrealized_gain_loss_chf")


def _csv_row(date: str, kind: str, *, order: str = "00000000",
             symbol: str = "", isin: str = "", quantity: str = "",
             unit_price: str = "", costs: str = "", accrued: str = "",
             net: str = "0.0", currency: str = "CHF") -> str:
    """One CSV line in the export's column order."""
    return ";".join([
        date, order, kind, symbol, "", isin, quantity, unit_price, costs,
        accrued, net, "0.0", currency,
    ])


# One row per shape: a bond buy quoted in percent with accrued
# interest, an equity sell with costs, a cash row with printed zeros,
# a row with blank cells, a placeholder cell and a decimal comma.
TX_ROWS = [
    _csv_row("02-01-2026 10:00:00", "Buy", order="00000001",
             symbol="FAKEBOND", isin="XX0000000001", quantity="10000.0",
             unit_price="101.25 %", costs="12.5", accrued="-30.0",
             net="-10167.5"),
    _csv_row("03-01-2026 11:00:00", "Sell", order="00000002",
             symbol="FAKEETF", isin="XX0000000002", quantity="4.0",
             unit_price="50.5", costs="1.95", accrued="0.0",
             net="200.05", currency="USD"),
    _csv_row("04-01-2026 12:00:00", "Payment", quantity="0.0",
             unit_price="0.0", costs="0.0", accrued="0.0", net="500.0"),
    _csv_row("05-01-2026 13:00:00", "Credit", net="1.0"),
    _csv_row("06-01-2026 14:00:00", "Custody Fees", quantity="1.0",
             unit_price="--", costs="N/A", accrued="", net="-5.0"),
    _csv_row("07-01-2026 15:00:00", "Dividend", symbol="FAKEETF",
             isin="XX0000000002", quantity="4,0", unit_price="0,5",
             costs="0,3", accrued="0,0", net="1,7"),
]

POSITIONS_BODY = [
    ["ETFs"],
    ["", "FAKEETF", 10.0, 95.0, 1000.0, 1.0, 0.1, 100.0, "USD",
     50.0, 5.0, 900.0, 60.0],
    ["Bonds"],
    # The export prints a bond's prices per unit of nominal.
    ["", "FAKEBOND", 10000.0, 1.02, 10100.0, 0.0, 0.0, 1.01, "CHF",
     -100.0, -1.0, 10100.0, 30.0],
    ["Other"],
    # Blank unit cost and P&L cells.
    ["", "FAKENOCOST", 1.0, "", 500.0, "", "", 500.0, "CHF",
     "", "", 500.0, 10.0],
    ["", "Total", "", "", "", "", "", "", "", "", "", 11500.0, 100.0],
]

# A layout-mode asset-allocation page: a bond priced in percent, a
# fund priced per unit.
STATEMENT_PAGE = """\
  Bonds in    CHF
          Quantity     Security                     ISIN              Average price      Market Price     Price date      Valuation in CHF       %
                                                                                                                     incl. accrued interests
         10'000      FAKE Bond 1.000% 31.12.2030    XX0000000001       102.500%           101.000%      31.12.2025         10'100.00      40.00
                                                                                                                                 25.00
  Funds in          CHF
          Quantity     Security                     ISIN              Average price      Market Price     Price date      Valuation in CHF       %
             100       Fictional Equity Fund        XX0000000003        95.000            150.000       31.12.2025         15'000.00      60.00
"""


def _write_xls(path: Path, rows: list[list]) -> Path:
    wb = xlwt.Workbook()
    sheet = wb.add_sheet("export")
    for r, row in enumerate(rows):
        for c, val in enumerate(row):
            sheet.write(r, c, val)
    wb.save(str(path))
    return path


def _statement(_pdf_path: Path) -> dict:
    """Stand-in for parse_portfolio_performance: the PDF read is
    covered elsewhere; this feeds the synthetic page to the parser."""
    return {
        "snapshot_date": "2025-12-31",
        "account_external_id": ACCOUNT,
        "positions": load._pp_parse_asset_allocation(
            STATEMENT_PAGE, Path("synthetic.pdf")),
    }


class _DbCase(unittest.TestCase):
    def setUp(self):
        self._tmp = tempfile.TemporaryDirectory()
        self.tmp = Path(self._tmp.name)
        self.conn = load.open_db(self.tmp / "swissquote.db")
        load.apply_migrations(self.conn, MIGRATIONS)

    def tearDown(self):
        self.conn.close()
        self._tmp.cleanup()

    def load_fixtures(self) -> None:
        """Load every fixture shape through the loader."""
        csv_path = self.tmp / "transactions.csv"
        csv_path.write_text("\n".join([CSV_HEADER, *TX_ROWS]) + "\n",
                            encoding="utf-8")
        load.load_transactions_csv(self.conn, ACCOUNT, csv_path)
        xls_path = _write_xls(self.tmp / "positions.xls",
                              [load.POSITIONS_EXPECTED_HEADER,
                               *POSITIONS_BODY])
        load.load_positions(self.conn, SNAPSHOT_AT, ACCOUNT, xls_path)
        self.conn.execute(
            "INSERT INTO documents(content_sha256, source,"
            " swissquote_doc_id, account_external_id, document_type,"
            " first_seen_at, bronze_path, payload) "
            "VALUES ('00ff', 'auto', 'doc-0001', ?, 'Portfolio performance',"
            " ?, 'run/documents/doc-0001.pdf', '{}');",
            (ACCOUNT, SNAPSHOT_AT),
        )
        with mock.patch.object(load, "parse_portfolio_performance",
                               _statement):
            load.load_portfolio_performance_docs(self.conn, self.tmp)

    def tx(self, kind: str) -> dict:
        row = self.conn.execute(
            f"SELECT {', '.join(TX_COLUMNS)}, payload FROM transactions "
            "WHERE transaction_type = ?;", (kind,)).fetchone()
        return dict(row)

    def position(self, isin_or_symbol: str) -> dict:
        row = self.conn.execute(
            f"SELECT {', '.join(POSITION_COLUMNS)}, payload FROM positions "
            "WHERE symbol = ? OR isin = ?;",
            (isin_or_symbol, isin_or_symbol)).fetchone()
        return dict(row)


class TransactionColumnsTests(_DbCase):
    def setUp(self):
        super().setUp()
        self.load_fixtures()

    def test_bond_trade_in_percent(self):
        r = self.tx("Buy")
        self.assertEqual(r["quantity"], 10000.0)
        self.assertEqual(r["unit_price"], 101.25)
        self.assertEqual(r["price_quote"], "percent")
        self.assertEqual(r["fees"], 12.5)
        self.assertEqual(r["accrued_interest"], -30.0)
        # The payload keeps the cell as printed.
        self.assertEqual(json.loads(r["payload"])["unit_price"], "101.25 %")

    def test_trade_with_costs(self):
        r = self.tx("Sell")
        self.assertEqual(r["quantity"], 4.0)
        self.assertEqual(r["unit_price"], 50.5)
        self.assertEqual(r["price_quote"], "unit")
        self.assertEqual(r["fees"], 1.95)
        self.assertEqual(r["accrued_interest"], 0.0)

    def test_printed_zero_stays_zero(self):
        r = self.tx("Payment")
        self.assertEqual(
            [r[c] for c in TX_COLUMNS], [0.0, 0.0, "unit", 0.0, 0.0])

    def test_blank_cells_are_null(self):
        r = self.tx("Credit")
        self.assertEqual([r[c] for c in TX_COLUMNS], [None] * 5)

    def test_placeholder_cells_are_null(self):
        r = self.tx("Custody Fees")
        self.assertEqual(r["quantity"], 1.0)
        self.assertIsNone(r["unit_price"])
        self.assertIsNone(r["price_quote"])
        self.assertIsNone(r["fees"])
        self.assertIsNone(r["accrued_interest"])

    def test_decimal_comma(self):
        r = self.tx("Dividend")
        self.assertEqual(
            [r[c] for c in TX_COLUMNS], [4.0, 0.5, "unit", 0.3, 0.0])


class PositionColumnsTests(_DbCase):
    def setUp(self):
        super().setUp()
        self.load_fixtures()

    def test_live_equity_row(self):
        r = self.position("FAKEETF")
        self.assertEqual(r["average_cost"], 95.0)
        self.assertEqual(r["price_quote"], "unit")
        self.assertEqual(r["market_value_chf"], 900.0)
        self.assertEqual(r["unrealized_gain_loss_chf"], 50.0)

    def test_live_bond_row_is_per_unit(self):
        r = self.position("FAKEBOND")
        self.assertEqual(r["average_cost"], 1.02)
        self.assertEqual(r["price_quote"], "unit")
        self.assertEqual(r["unrealized_gain_loss_chf"], -100.0)

    def test_live_blank_cells_are_null(self):
        r = self.position("FAKENOCOST")
        self.assertIsNone(r["average_cost"])
        self.assertIsNone(r["unrealized_gain_loss_chf"])
        self.assertEqual(r["market_value_chf"], 500.0)

    def test_statement_bond_row_in_percent(self):
        row = self.conn.execute(
            "SELECT average_cost, price_quote, market_value_chf,"
            " unrealized_gain_loss_chf, payload FROM positions "
            "WHERE source = 'pp:doc-0001' AND isin = 'XX0000000001';"
        ).fetchone()
        self.assertEqual(row["average_cost"], 102.5)
        self.assertEqual(row["price_quote"], "percent")
        self.assertIsNone(row["market_value_chf"])
        self.assertIsNone(row["unrealized_gain_loss_chf"])
        # price_quote is a column only; the payload keeps the parsed
        # fields.
        self.assertNotIn("price_quote", json.loads(row["payload"]))

    def test_statement_bond_reads_its_accrued_interest(self):
        rows = dict(self.conn.execute(
            "SELECT isin, accrued_interest_chf FROM positions "
            "WHERE source = 'pp:doc-0001';").fetchall())
        self.assertEqual(rows["XX0000000001"], 25.0)
        self.assertIsNone(rows["XX0000000003"])

    def test_statement_fund_row_per_unit(self):
        row = self.conn.execute(
            "SELECT average_cost, price_quote FROM positions "
            "WHERE source = 'pp:doc-0001' AND isin = 'XX0000000003';"
        ).fetchone()
        self.assertEqual(row["average_cost"], 95.0)
        self.assertEqual(row["price_quote"], "unit")


class BackfillTests(_DbCase):
    """Rows loaded before migration 0006 get the loader's values."""

    OLD_TX = ("account_external_id", "occurred_at", "transaction_type",
              "order_num", "isin", "symbol", "currency", "net_amount",
              "payload")
    OLD_POSITION = ("snapshot_at", "account_external_id", "symbol",
                    "currency", "name", "isin", "source", "payload")

    def _old_db(self):
        """A silver DB at schema 5 holding the loader's rows, without
        the columns migration 0006 adds."""
        old_migrations = self.tmp / "migrations-0005"
        old_migrations.mkdir()
        for f in MIGRATIONS.glob("000[1-5]_*.sql"):
            shutil.copy(f, old_migrations / f.name)
        old = load.open_db(self.tmp / "old.db")
        load.apply_migrations(old, old_migrations)
        self.assertEqual(load.current_schema_version(old), 5)
        for table, cols in (("transactions", self.OLD_TX),
                            ("positions", self.OLD_POSITION)):
            names = ", ".join(cols)
            marks = ", ".join("?" * len(cols))
            for row in self.conn.execute(f"SELECT {names} FROM {table};"):
                old.execute(
                    f"INSERT INTO {table}({names}) VALUES ({marks});",
                    tuple(row))
        return old

    def test_backfill_matches_loader(self):
        self.load_fixtures()
        old = self._old_db()
        try:
            to_0006 = self.tmp / "migrations-0006"
            to_0006.mkdir()
            for f in MIGRATIONS.glob("000[1-6]_*.sql"):
                shutil.copy(f, to_0006 / f.name)
            load.apply_migrations(old, to_0006)
            self.assertEqual(load.current_schema_version(old), 6)
            for table, key, cols in (
                ("transactions", "occurred_at", TX_COLUMNS),
                ("positions", "source, symbol", POSITION_COLUMNS),
            ):
                q = (f"SELECT {key}, {', '.join(cols)} FROM {table} "
                     f"ORDER BY {key};")
                loaded = [tuple(r) for r in self.conn.execute(q)]
                backfilled = [tuple(r) for r in old.execute(q)]
                self.assertEqual(backfilled, loaded, table)
            # Not vacuous: the bond rows carry their percent quote.
            self.assertEqual(
                old.execute(
                    "SELECT COUNT(*) FROM positions "
                    "WHERE price_quote = 'percent';").fetchone()[0], 1)
            self.assertEqual(
                old.execute(
                    "SELECT unit_price FROM transactions "
                    "WHERE price_quote = 'percent';").fetchone()[0], 101.25)
        finally:
            old.close()



class AccruedMigrationTests(_DbCase):
    """Migration 0007 drops the statement rows so the next load parses
    every statement again; live rows stay."""

    def test_statement_rows_are_parsed_again(self):
        self.load_fixtures()
        before = self.conn.execute(
            "SELECT COUNT(*) FROM positions WHERE source = 'live';").fetchone()[0]
        self.conn.executescript(
            (MIGRATIONS / "0007_accrued_interest.sql").read_text()
            .replace("ALTER TABLE positions ADD COLUMN accrued_interest_chf REAL;", "")
            .replace("VALUES (7,", "VALUES (70,"))
        self.assertEqual(self.conn.execute(
            "SELECT COUNT(*) FROM positions WHERE source LIKE 'pp:%';").fetchone()[0], 0)
        self.assertEqual(self.conn.execute(
            "SELECT COUNT(*) FROM positions WHERE source = 'live';").fetchone()[0], before)
        with mock.patch.object(load, "parse_portfolio_performance", _statement):
            load.load_portfolio_performance_docs(self.conn, self.tmp)
        self.assertEqual(self.conn.execute(
            "SELECT accrued_interest_chf FROM positions WHERE source = 'pp:doc-0001'"
            " AND isin = 'XX0000000001';").fetchone()[0], 25.0)

if __name__ == "__main__":
    unittest.main()
