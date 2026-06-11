"""Bronze→silver tests for the ubs-web collector's load.py.

Seeds a minimal synthetic UBS web positions export (the semicolon
CSV with the "Valued in:" footer) and runs the positions loader,
asserting the relationship / portfolio / position silver rows.
Synthetic relationship prefix / portfolio / ISIN only — NOT a real
banking relationship.
"""
from __future__ import annotations

import sqlite3
import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
COLLECTOR = HERE.parent
sys.path.insert(0, str(COLLECTOR))

import load as loader  # noqa: E402
from collectorkit import silver  # noqa: E402

REL = "1234 00000001"   # synthetic "<4-digit branch> <8-digit base>"

HEADER = ";".join(loader.POSITIONS_COLS)
# One securities holding row (ISIN + Number/Amt + Market value, no IBAN).
ROW = ("1234 00000001;Portfolio ABC;Equities;Share;CHF;10;;12345;"
       "CH0000000001;;;;;Example Share;;;;;;;;;;;;;;;1500.00;;;")
CSV = HEADER + "\r\n" + ROW + "\r\nValued in: CHF\r\n"


MIGRATIONS_DIR = COLLECTOR / "migrations"


def _fresh_db(tmp_path: Path) -> sqlite3.Connection:
    conn = sqlite3.connect(str(tmp_path / "ubs-web.db"))
    conn.row_factory = sqlite3.Row
    silver.apply_migrations(conn, MIGRATIONS_DIR)
    return conn


def _seed_bronze(root: Path) -> Path:
    dump = root / "20240101T000000Z"
    (dump / "positions").mkdir(parents=True, exist_ok=True)
    (dump / "positions" / "positions_test.csv").write_text(
        CSV, encoding="utf-8")
    return dump


def test_load_positions(tmp_path):
    dump = _seed_bronze(tmp_path / "bronze")
    conn = _fresh_db(tmp_path)
    with conn:
        n = loader._load_positions(conn, 1700000000, dump)
    assert n == 1

    # banking_relationships row from the synthetic prefix.
    rel = conn.execute(
        "SELECT banking_relationship_id FROM banking_relationships").fetchall()
    assert len(rel) == 1
    assert rel[0]["banking_relationship_id"] == REL

    # portfolios row linked to the relationship.
    port = conn.execute(
        "SELECT banking_relationship_id, base_currency FROM portfolios").fetchall()
    assert len(port) == 1
    assert port[0]["banking_relationship_id"] == REL
    assert port[0]["base_currency"] == "CHF"

    # position row keyed by ISIN.
    pos = conn.execute("SELECT instrument_isin FROM positions").fetchall()
    assert len(pos) == 1
    assert pos[0]["instrument_isin"] == "CH0000000001"


def test_positions_base_currency_footer(tmp_path):
    dump = _seed_bronze(tmp_path / "bronze")
    csv_path = dump / "positions" / "positions_test.csv"
    assert loader._read_positions_base_currency(csv_path) == "CHF"


# ---- mortgage rows ('Pro memoria - Mortgages') --------------------

# A synthetic mortgage row: Group of products = 'Pro memoria -
# Mortgages', the IBAN column carries a 'dd.mm.yyyy - dd.mm.yyyy'
# term, Number/Amt. is the negative principal.
# Obviously-synthetic placeholder term + principal. NOT the real
# user's mortgage dates / balance.
SYN_TERM = "01.01.2020 - 31.12.2024"
SYN_PRINCIPAL = "-1234567.89"


def _mortgage_row(rate_descriptor: str = "UBS Fixed-Rate Mortgage",
                  term: str = SYN_TERM) -> str:
    cells = {
        "Banking relationship": REL,
        "Portfolio": "1234 00000001 R001",
        "Group of products": "Pro memoria - Mortgages",
        "Product": "1234 00000001.MMM 0000",
        "Ccy.": "CHF",
        "Number/Amt.": SYN_PRINCIPAL,
        "Description": f"{rate_descriptor}, EXAMPLE ROAD 1, 0000 EXAMPLECITY",
        "Description 1": rate_descriptor,
        "Description 2": "Properties",
        "Description 3": "EXAMPLE ROAD 1, 0000 EXAMPLECITY",
        "IBAN": term,
    }
    return ";".join(cells.get(col, "") for col in loader.POSITIONS_COLS)


def test_load_mortgage_row(tmp_path):
    """A 'Pro memoria - Mortgages' row should be inserted into the
    `mortgages` table (not `accounts` / `positions`), with the
    fixed-rate term parsed out of the IBAN column."""
    dump = tmp_path / "bronze" / "20240101T000000Z"
    (dump / "positions").mkdir(parents=True)
    (dump / "positions" / "positions_test.csv").write_text(
        ";".join(loader.POSITIONS_COLS) + "\r\n"
        + _mortgage_row() + "\r\n"
        + "Valued in: CHF\r\n",
        encoding="utf-8",
    )
    conn = _fresh_db(tmp_path)
    with conn:
        loader._load_positions(conn, 1700000000, dump)

    mort = conn.execute(
        "SELECT account_external_id, currency_iso, outstanding_balance, "
        "       start_date, end_date, rate_type "
        "  FROM mortgages"
    ).fetchall()
    assert len(mort) == 1
    row = mort[0]
    assert row["account_external_id"] == "1234 00000001.MMM 0000"
    assert row["currency_iso"] == "CHF"
    assert row["outstanding_balance"] == float(SYN_PRINCIPAL)
    # Round-trip via datetime to assert calendar parsing rather than
    # hard-coding a Unix epoch (silently mis-asserting on leap-year
    # arithmetic).
    from datetime import datetime, timezone
    assert datetime.fromtimestamp(row["start_date"], timezone.utc).date() \
        == datetime(2020, 1, 1).date()
    assert datetime.fromtimestamp(row["end_date"], timezone.utc).date() \
        == datetime(2024, 12, 31).date()
    assert row["rate_type"] == "fixed"

    # And the row must not have leaked into accounts / positions.
    assert conn.execute("SELECT COUNT(*) FROM accounts").fetchone()[0] == 0
    assert conn.execute("SELECT COUNT(*) FROM positions").fetchone()[0] == 0


def test_load_mortgage_variable_rate(tmp_path):
    """The variable-rate variant detected via Description 1."""
    dump = tmp_path / "bronze" / "20240101T000000Z"
    (dump / "positions").mkdir(parents=True)
    (dump / "positions" / "positions_test.csv").write_text(
        ";".join(loader.POSITIONS_COLS) + "\r\n"
        + _mortgage_row(
            rate_descriptor="UBS Variable-Rate Mortgage",
            term="",  # variable-rate has no fixed term
        ) + "\r\n"
        + "Valued in: CHF\r\n",
        encoding="utf-8",
    )
    conn = _fresh_db(tmp_path)
    with conn:
        loader._load_positions(conn, 1700000000, dump)
    row = conn.execute(
        "SELECT rate_type, start_date, end_date FROM mortgages"
    ).fetchone()
    assert row["rate_type"] == "variable"
    assert row["start_date"] is None
    assert row["end_date"] is None
