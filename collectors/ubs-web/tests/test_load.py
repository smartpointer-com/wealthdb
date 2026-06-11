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
