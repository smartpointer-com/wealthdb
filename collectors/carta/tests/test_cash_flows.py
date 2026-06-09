"""
Unit tests for load.py's cash-flow ledger (migration 0003).

Covers the inception-to-date statement parsing, the per-period differencing,
and the cap-table cash-flow synthesis (exercises + exit). In-memory SQLite +
a tmp_path bronze edir. Synthetic data only — no real holdings or figures.
"""
from __future__ import annotations

import json
import sqlite3
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import load  # noqa: E402

MIGRATIONS_DIR = Path(__file__).resolve().parent.parent / "migrations"


@pytest.fixture
def migrated():
    c = sqlite3.connect(":memory:")
    c.execute("PRAGMA foreign_keys = ON")
    load.silver.apply_migrations(c, MIGRATIONS_DIR)
    yield c
    c.close()


# ---- statement inception-to-date parsing -----------------------------------

def test_statement_flows_takes_inception_to_date_not_receivable():
    # Three columns (period / YTD / inception); '—' placeholders mis-align the
    # period columns, so the LAST amount (inception-to-date) is the reliable
    # one. The balance-sheet "receivable" line must be ignored.
    text = (
        "Statement of changes in investor's capital\n"
        "                    Statement period   Year to date   Inception to date\n"
        "Capital contributions receivable                            99,999\n"
        "Capital contributions          —              —            100,000\n"
        "Capital distributions          —          (2,500)           (2,500)\n")
    assert load._statement_flows_from_text(text) == (100000.0, 2500.0)


def test_statement_flows_absent_lines():
    assert load._statement_flows_from_text("nothing relevant here") == (None, None)


# ---- per-period differencing of inception-to-date --------------------------

def test_period_deltas_difference_and_sort():
    # Out of order on input; sorted by date. ITD jumps 100k->150k->200k become
    # per-period calls (the first lumps anything earlier); dist 0->2,500 once.
    stmts = [
        ("12/31/2025", "d4", 200000.0, 2500.0),
        ("09/30/2023", "d1", 100000.0, None),
        ("12/31/2024", "d2", 150000.0, None),
        ("03/31/2025", "d3", 150000.0, 2500.0),
    ]
    assert load._period_deltas(stmts) == [
        ("d1", "09/30/2023", "capital_call", 100000.0),
        ("d2", "12/31/2024", "capital_call", 50000.0),
        ("d3", "03/31/2025", "distribution", 2500.0),
        ("d4", "12/31/2025", "capital_call", 50000.0),
    ]


def test_period_deltas_skip_flat_and_none():
    stmts = [("01/01/2024", "a", 100.0, None),
             ("04/01/2024", "b", 100.0, None),   # flat -> no jump
             ("07/01/2024", "c", None, None)]     # missing -> nothing
    assert load._period_deltas(stmts) == [("a", "01/01/2024", "capital_call", 100.0)]


# ---- cap-table cash flows (exercises + exit) -------------------------------

def _captable_edir(tmp_path: Path, canceled: str | None = None) -> Path:
    edir = tmp_path / "entities" / "corp_7"
    (edir / "vesting").mkdir(parents=True)
    (edir / "shares.json").write_text(json.dumps({"rows": [
        {"id": 1, "quantity": 1000, "cost": 500.0, "issue_date": "01/01/2023"},
        {"id": 2, "quantity": 500, "cost": 1000.0, "issue_date": "06/01/2024"},
    ]}))
    if canceled:
        (edir / "vesting" / "grant_1.json").write_text(
            json.dumps({"canceled_date": canceled}))
    return edir


def test_captable_exercises_one_per_cert(migrated, tmp_path):
    n = load._captable_cash_flows(migrated, 7, _captable_edir(tmp_path),
                                  1_700_000_000, tmp_path)
    assert n == 2  # one exercise per share cert; no exit (live holding)
    rows = migrated.execute(
        "SELECT kind, flow_date, amount, shares, price_per_share "
        "FROM cash_flows ORDER BY flow_date").fetchall()
    assert rows == [
        ("exercise", "01/01/2023", 500.0, 1000.0, 0.5),   # price = cost / qty
        ("exercise", "06/01/2024", 1000.0, 500.0, 2.0),
    ]


def test_captable_exit_zero_proceeds(migrated, tmp_path):
    load._captable_cash_flows(
        migrated, 7, _captable_edir(tmp_path, canceled="03/03/2099"),
        1_700_000_000, tmp_path)
    # $0 recorded proceeds (Carta purges the payout); total held shares; the
    # gold adapter then omits the $0 withdrawal leg.
    assert migrated.execute(
        "SELECT kind, flow_date, amount, shares FROM cash_flows WHERE kind='exit'"
    ).fetchone() == ("exit", "03/03/2099", 0.0, 1500.0)


# ---- side-loaded transactions (the final sale + withdrawals) ----------------

def test_read_transactions_csv(tmp_path):
    p = tmp_path / "7-transactions.csv"
    p.write_text("# date,kind,amount,shares,description\n"
                 "2099-03-03,sell,7000.00,1500,sale of all shares\n"
                 "2099-03-03,withdrawal,6000.00,,to bank\n"
                 "2099-03-03,withdrawal,1000.00,,to second bank\n")
    rows = load._read_transactions_csv(p)
    assert rows == [
        {"flow_date": "2099-03-03", "kind": "sell", "amount": 7000.0,
         "shares": 1500.0, "description": "sale of all shares"},
        {"flow_date": "2099-03-03", "kind": "withdrawal", "amount": 6000.0,
         "shares": None, "description": "to bank"},
        {"flow_date": "2099-03-03", "kind": "withdrawal", "amount": 1000.0,
         "shares": None, "description": "to second bank"},
    ]


def test_captable_side_loaded_exit_replaces_auto_exit(migrated, tmp_path):
    # A pre-existing auto $0 exit from an earlier load must be cleared when the
    # side-loaded legs now apply (INSERT OR REPLACE alone would leave it).
    migrated.execute(
        "INSERT INTO cash_flows(cash_flow_external_id, entity_external_id, "
        "snapshot_at, kind, currency, payload) VALUES "
        "('exit:7', 7, 1, 'exit', 'USD', '{}')")
    edir = _captable_edir(tmp_path, canceled="03/03/2099")
    (tmp_path / "7-transactions.csv").write_text(
        "2099-03-03,sell,7000.00,1500,sale\n"
        "2099-03-03,withdrawal,6000.00,,bank\n"
        "2099-03-03,withdrawal,1000.00,,second bank\n")
    load._captable_cash_flows(migrated, 7, edir, 1_700_000_000, tmp_path)

    # No 'exit' row survives; the explicit legs are present and net to 0.
    assert migrated.execute("SELECT COUNT(*) FROM cash_flows WHERE kind='exit'").fetchone()[0] == 0
    legs = migrated.execute(
        "SELECT kind, amount, shares FROM cash_flows "
        "WHERE kind IN ('sell','withdrawal') ORDER BY amount DESC").fetchall()
    assert legs == [("sell", 7000.0, 1500.0),
                    ("withdrawal", 6000.0, None),
                    ("withdrawal", 1000.0, None)]
