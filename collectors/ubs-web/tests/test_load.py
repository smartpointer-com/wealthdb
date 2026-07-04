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
# the mortgage dates / balance.
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


# ================================================================
# Pre-2024 transaction backfill: per-account MT940 cut-over + dedup
# ================================================================

# Synthetic IBANs (all-zero placeholders, valid CH-IBAN shape).
_IBAN_A = "CH0000000000000000001"   # has an MT940 floor (2024-01-02)
_IBAN_B = "CH0000000000000000002"   # NO MT940 coverage → PDF owns all


def _write_cash_csv(dump: Path, iban_spaced: str, from_date: str,
                    hash_id: str) -> None:
    """Write a minimal synthetic MT940 cash CSV carrying just the
    IBAN: and From: metadata lines the floor scanner reads."""
    tdir = dump / "transactions"
    tdir.mkdir(parents=True, exist_ok=True)
    body = (
        f"Product:;UBS personal account;\r\n"
        f"IBAN:;{iban_spaced};\r\n"
        f"From:;{from_date};\r\n"
        f"To:;2026-01-01;\r\n"
        "Trade date;Booking date;Value date;Currency;Debit;Credit;"
        "Transaction no.;Description1;Description2\r\n"
    )
    (tdir / f"cash_{hash_id}_x.csv").write_text(body, encoding="utf-8-sig")


def _mv(account: str, booking: int, *, credit=None, debit=None,
        desc="CREDIT", occ=0, reconciled=True, value=None) -> dict:
    return {
        "booking_date": booking,
        "value_date": value if value is not None else booking,
        "account_external_id": account,
        "currency_iso": "CHF",
        "amount_debit": debit,
        "amount_credit": credit,
        "description_kind": desc,
        "counterparty": None,
        "counter_account": None,
        "occurrence": occ,
        "reconciled": reconciled,
        "payload": "{}",
    }


# 2024-01-02 and a pre/post reference point, as Unix seconds UTC.
_FLOOR = loader.ts_from_iso("2024-01-02")
_PRE = loader.ts_from_iso("2023-12-15")    # below the floor
_POST = loader.ts_from_iso("2024-03-06")   # at/after the floor


def test_mt940_floors_by_account(tmp_path: Path):
    root = tmp_path / "bronze"
    # Account A appears in two dumps; take the EARLIEST From:.
    _write_cash_csv(root / "20260101T000000Z", "CH00 0000 0000 0000 0000 1",
                    "2024-01-02", "aaaa")
    _write_cash_csv(root / "20260201T000000Z", "CH00 0000 0000 0000 0000 1",
                    "2026-02-20", "bbbb")
    floors = loader._mt940_floors_by_account(root / "20260201T000000Z")
    assert floors.get(_IBAN_A) == loader.ts_from_iso("2024-01-02")
    # Account B never appears → no floor (PDF owns all its history).
    assert _IBAN_B not in floors


def test_cutover_gate_per_account(tmp_path: Path):
    conn = _fresh_db(tmp_path)
    floors = {_IBAN_A: _FLOOR}   # A has a floor; B absent
    rows = [
        _mv(_IBAN_A, _PRE, credit=100.0),    # below floor → INGEST
        _mv(_IBAN_A, _POST, credit=200.0),   # at/after floor → SKIP (MT940)
        _mv(_IBAN_B, _POST, credit=300.0),   # B has no floor → INGEST
    ]
    with conn:
        n, rejected = loader._insert_hist_transactions(conn, 1, rows, floors)
    assert rejected == 0
    got = conn.execute(
        "SELECT account_external_id, booking_date, amount_credit "
        "FROM transactions ORDER BY amount_credit").fetchall()
    assert n == 2
    assert [r["amount_credit"] for r in got] == [100.0, 300.0]


def test_reconciliation_failure_rejects_whole_statement(tmp_path: Path):
    conn = _fresh_db(tmp_path)
    rows = [_mv(_IBAN_A, _PRE, credit=100.0, reconciled=False),
            _mv(_IBAN_A, _PRE, debit=50.0, reconciled=False, occ=1)]
    with conn:
        n, rejected = loader._insert_hist_transactions(conn, 1, rows, {})
    assert n == 0 and rejected == 1
    assert conn.execute("SELECT COUNT(*) c FROM transactions").fetchone()["c"] == 0


def test_content_id_dedups_overlapping_statements(tmp_path: Path):
    """The same booking on the monthly AND the annual statement (or in
    a post-closing trailer) must collapse to one row; two genuinely
    distinct identical same-day bookings must both survive."""
    conn = _fresh_db(tmp_path)
    same = dict(account=_IBAN_A, booking=_PRE, credit=100.0, desc="CREDIT")
    with conn:
        loader._insert_hist_transactions(conn, 1, [_mv(**same, occ=0)], {})
        # Re-ingest identical row (as if from the annual statement).
        loader._insert_hist_transactions(conn, 2, [_mv(**same, occ=0)], {})
        # A genuinely distinct second identical same-day booking (occ=1).
        loader._insert_hist_transactions(conn, 2, [_mv(**same, occ=1)], {})
    assert conn.execute(
        "SELECT COUNT(*) c FROM transactions").fetchone()["c"] == 2


def test_stmt_txn_id_stable_and_namespaced():
    r = _mv(_IBAN_A, _PRE, credit=100.0)
    a = loader._stmt_txn_id(_IBAN_A, r)
    b = loader._stmt_txn_id(_IBAN_A, r)
    assert a == b and a.startswith("stmt:")
    # Different occurrence → different id.
    assert loader._stmt_txn_id(_IBAN_A, {**r, "occurrence": 1}) != a
