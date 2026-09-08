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


def test_captable_convertible_purchase(migrated, tmp_path):
    # A SAFE / convertible note is a cash purchase (no shares), emitted as one
    # `convertible_purchase` event = deposit+buy in gold, at its principal.
    edir = _captable_edir(tmp_path)  # 2 share certs -> 2 exercises
    (edir / "convertibles.json").write_text(json.dumps({"rows": [
        {"id": 9, "cost": 100000.0, "issue_date": "06/30/2026", "quantity": 0},
    ]}))
    n = load._captable_cash_flows(migrated, 7, edir, 1_700_000_000, tmp_path)
    assert n == 3  # 2 exercises + 1 convertible_purchase
    assert migrated.execute(
        "SELECT kind, flow_date, amount, shares, price_per_share, description "
        "FROM cash_flows WHERE kind='convertible_purchase'").fetchone() == (
        "convertible_purchase", "06/30/2026", 100000.0, None, None,
        "SAFE / convertible purchase")


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


# ============================================================
# capital-call / distribution notices
# ============================================================

_CALL_NOTICE = """
Example Fund I L.P.
Capital Call Notice

Initiated by                                          Example Fund I L.P.
Date of notice                                                May 20, 2098
Due date                                                     June 15, 2098

Capital Call details
Contribution                                                    $61,250.00
Amount due to fund                                              $61,250.00

Commitment summary
Commitment                                                     $625,000.00
Called capital (post call)                                     $217,500.00
Remaining uncalled of commitment (post call)                   $407,500.00
"""

_DIST_NOTICE = """
Example Fund I L.P.
Distribution Notice

Date of notice                                             January 15, 2099
Distribution date                                          January 15, 2099

Distribution details
Distribution                                                     $1,234.56
Amount due to investor                                           $1,234.56

Commitment summary
Commitment                                                     $625,000.00
Distributed capital to date (post distribution)                  $1,234.56
"""


def test_a_call_notice_yields_its_due_date_and_amount():
    # The point of reading notices at all: a statement can only place a call
    # in the period it fell in, so every call lands at the period end that
    # follows it. The notice states the day the money was due.
    got = load.parse_notice_text(_CALL_NOTICE)
    assert got["kind"] == "capital_call"
    assert got["date"] == "06/15/2098"       # due date, not the notice date
    assert got["issued"] == "05/20/2098"     # kept for dating the residue
    assert got["amount"] == 61250.0
    assert got["cumulative"] == 217500.0


def test_a_distribution_notice_reads_its_own_labels():
    got = load.parse_notice_text(_DIST_NOTICE)
    assert got["kind"] == "distribution"
    assert got["date"] == "01/15/2099"
    assert got["amount"] == 1234.56          # to the cent; the statement rounds
    assert got["cumulative"] == 1234.56


def test_the_amount_label_does_not_read_the_line_below_it():
    # "Distribution" must not match "Distribution date", and the
    # "Amount due to ..." restatement under it is not the figure.
    got = load.parse_notice_text(_DIST_NOTICE.replace("$1,234.56", "$1.00", 1))
    assert got["amount"] == 1.00


def test_a_document_that_is_neither_notice_is_skipped():
    assert load.parse_notice_text("Capital Account Statement\nEnding balance $10") is None


def test_the_residue_is_what_the_earliest_notice_says_preceded_it():
    # Called capital post-call, less this call, is everything called before
    # Carta shared anything — stated by the fund, not derived.
    notice = load.parse_notice_text(_CALL_NOTICE)
    assert load._residue_from_cumulative(notice) == 156250.0
    assert load._residue_from_cumulative({"amount": 1.0, "cumulative": None}) is None
