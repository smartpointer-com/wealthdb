"""Unit tests for the firstcitizens silver loader — synthetic bronze only (no
real account ids, balances, or payees). Exercises the history-JSON parser
(signed amount from `isDebit`, running balance, stable id, malformed-row
skipping), the roster projection (mask, balance pick), the statement document
inventory, and the idempotent per-run load.
"""
from __future__ import annotations

import json
import sys
from datetime import datetime, timezone
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import load  # noqa: E402

MIGRATIONS = Path(__file__).resolve().parent.parent / "migrations"

# Synthetic opaque Q2 account id + a masked account number (placeholders).
ACCT_ID = "900001"
MASKED_NUMBER = "XXXXXXXX6789"


def _epoch(y, m, d):
    return int(datetime(y, m, d, tzinfo=timezone.utc).timestamp())


# ============================================================
# Scalar parsers
# ============================================================

def test_parse_posted_date_variants():
    assert load.parse_posted_date("7/15/2026") == _epoch(2026, 7, 15)
    assert load.parse_posted_date("07/15/2026") == _epoch(2026, 7, 15)
    assert load.parse_posted_date("2026-07-15") == _epoch(2026, 7, 15)
    assert load.parse_posted_date("2026-07-15T12:34:56Z") == _epoch(2026, 7, 15)
    # epoch milliseconds and seconds both land on the same UTC day.
    assert load.parse_posted_date(_epoch(2026, 7, 15) * 1000) == _epoch(2026, 7, 15)
    assert load.parse_posted_date(_epoch(2026, 7, 15)) == _epoch(2026, 7, 15)
    assert load.parse_posted_date("garbage") is None
    assert load.parse_posted_date(None) is None


def test_parse_money_variants():
    assert load.parse_money("$1,234.56") == 1234.56
    assert load.parse_money("(50.00)") == -50.00
    assert load.parse_money(-42.5) == -42.5
    assert load.parse_money("") is None
    assert load.parse_money(None) is None


def test_signed_amount_uses_isdebit():
    # isDebit forces the sign regardless of the raw magnitude's sign.
    assert load._signed_amount({"amount": "42.50", "isDebit": True}) == -42.50
    assert load._signed_amount({"amount": "42.50", "isDebit": False}) == 42.50
    assert load._signed_amount({"amount": "-42.50", "isDebit": True}) == -42.50
    assert load._signed_amount({"amount": "42.50", "isDebit": "true"}) == -42.50
    # No isDebit → trust the raw sign.
    assert load._signed_amount({"amount": "-42.50"}) == -42.50
    assert load._signed_amount({"description": "no amount"}) is None


# ============================================================
# History rows
# ============================================================

def _tx(tid, date_str, amount, is_debit, balance, desc, check=None):
    row = {"transactionId": tid, "postedDate": date_str, "amount": amount,
           "isDebit": is_debit, "runningBalance": balance, "description": desc}
    if check is not None:
        row["checkNumber"] = check
    return row


def test_history_rows_project_and_sign():
    txs = [
        _tx("T1", "7/15/2026", "42.50", True, "957.50", "COFFEE BAR"),
        _tx("T2", "7/16/2026", "1500.00", False, "2457.50", "PAYROLL"),
        _tx("T3", "7/17/2026", "200.00", True, "2257.50", "CHECK", check="1088"),
    ]
    rows = load.history_rows(ACCT_ID, txs)
    assert [r["fitid"] for r in rows] == ["T1", "T2", "T3"]
    assert rows[0]["amount"] == -42.50
    assert rows[0]["kind"] == "DEBIT"
    assert rows[0]["balance"] == 957.50
    assert rows[0]["posted_at"] == _epoch(2026, 7, 15)
    assert rows[1]["amount"] == 1500.00
    assert rows[1]["kind"] == "CREDIT"
    assert rows[2]["check_number"] == "1088"
    # Net of the signed amounts reconciles with the last running balance from a
    # zero start: -42.50 + 1500 - 200 = 1257.50 of movement.
    assert round(sum(r["amount"] for r in rows), 2) == 1257.50


def test_history_rows_skip_malformed_and_synthesize_id():
    txs = [
        {"transactionId": "T1", "postedDate": "bad", "amount": "1.00"},   # no date
        {"transactionId": "T2", "postedDate": "7/1/2026"},                 # no amount
        _tx(None, "7/2/2026", "9.99", True, "10.00", "PENDING"),           # no id
    ]
    rows = load.history_rows(ACCT_ID, txs)
    assert len(rows) == 1
    assert rows[0]["fitid"].startswith("syn_")
    # The synthetic id is stable across re-parses.
    again = load.history_rows(ACCT_ID, txs)
    assert again[0]["fitid"] == rows[0]["fitid"]


# ============================================================
# Roster projection
# ============================================================

def test_pick_balance_prefers_current():
    balances = [{"description": "Available Balance", "value": "1,000.00"},
                {"description": "Current Balance", "value": "1,200.00"}]
    assert load._pick_balance(balances) == 1200.00
    assert load._pick_balance([{"description": "Available", "value": "5.00"}]) == 5.00
    assert load._pick_balance([]) is None


def test_mask_is_last_four_only():
    assert load._mask({"account_external_id": MASKED_NUMBER}) == "…6789"
    assert load._mask({"account_external_id": "12"}) is None


def test_statement_date_from_name():
    assert load._statement_date_from_name("07-31-2026.pdf") == _epoch(2026, 7, 31)
    assert load._statement_date_from_name("2026-07-31.pdf") == _epoch(2026, 7, 31)
    assert load._statement_date_from_name("no-date.pdf") is None


# ============================================================
# End-to-end per-run load
# ============================================================

def _write_bronze_run(root: Path, slug: str) -> Path:
    run = root / slug
    (run / "history").mkdir(parents=True)
    (run / "statements" / ACCT_ID).mkdir(parents=True)
    accounts = [{
        "id": ACCT_ID,
        "account_external_id": MASKED_NUMBER,
        "product_type_name": "Checking",
        "nickname": "Example Checking",
        "hydra_product_type_code": "D",
        "balances": [{"description": "Current Balance", "value": "2,257.50"}],
    }]
    (run / "accounts.json").write_text(json.dumps(accounts))
    (run / "history" / f"{ACCT_ID}.json").write_text(json.dumps({
        "accountId": ACCT_ID,
        "transactionCount": 3,
        "oldestTransactionDate": "7/15/2026",
        "transactions": [
            _tx("T1", "7/15/2026", "42.50", True, "957.50", "COFFEE BAR"),
            _tx("T2", "7/16/2026", "1500.00", False, "2457.50", "PAYROLL"),
            _tx("T3", "7/17/2026", "200.00", True, "2257.50", "CHECK", check="1088"),
        ],
    }))
    (run / "statements" / ACCT_ID / "07-31-2026.pdf").write_bytes(b"%PDF-1.4 synthetic")
    (run / "run.json").write_text(json.dumps({"source": "firstcitizens",
                                              "status": "complete"}))
    return run


def _load(tmp_path: Path) -> load.sqlite3.Connection:
    db = tmp_path / "firstcitizens.db"
    conn = load.silver.open_db(db)
    load.silver.apply_migrations(conn, MIGRATIONS)
    return conn


def test_load_run_end_to_end(tmp_path):
    bronze_dir = tmp_path / "bronze"
    _write_bronze_run(bronze_dir, "20260801T120000Z")
    conn = _load(tmp_path)
    run = bronze_dir / "20260801T120000Z"
    assert load.load_run(conn, run) is True

    acct = conn.execute("SELECT account_external_id, account_type, nickname, "
                        "mask, currency, balance FROM accounts").fetchone()
    assert acct["account_external_id"] == ACCT_ID
    assert acct["account_type"] == "Checking"
    assert acct["mask"] == "…6789"
    assert acct["currency"] == "USD"
    assert acct["balance"] == 2257.50

    txs = conn.execute("SELECT fitid, amount, balance, source, "
                       "account_external_id FROM transactions ORDER BY posted_at").fetchall()
    assert [t["fitid"] for t in txs] == ["T1", "T2", "T3"]
    assert all(t["source"] == "history" for t in txs)
    assert all(t["account_external_id"] == ACCT_ID for t in txs)
    assert txs[0]["amount"] == -42.50 and txs[0]["balance"] == 957.50

    doc = conn.execute("SELECT account_external_id, doc_date, doc_kind, "
                       "file_format FROM documents").fetchone()
    assert doc["account_external_id"] == ACCT_ID
    assert doc["doc_date"] == _epoch(2026, 7, 31)
    assert doc["doc_kind"] == "statement" and doc["file_format"] == "pdf"


def test_load_run_is_idempotent(tmp_path):
    bronze_dir = tmp_path / "bronze"
    _write_bronze_run(bronze_dir, "20260801T120000Z")
    conn = _load(tmp_path)
    run = bronze_dir / "20260801T120000Z"
    assert load.load_run(conn, run) is True
    # Re-loading the same dump is a no-op (already-loaded snapshot).
    assert load.load_run(conn, run) is False
    assert conn.execute("SELECT COUNT(*) FROM transactions").fetchone()[0] == 3
    assert conn.execute("SELECT COUNT(*) FROM accounts").fetchone()[0] == 1
    assert conn.execute("SELECT COUNT(*) FROM documents").fetchone()[0] == 1


def test_account_content_dedup_across_runs(tmp_path):
    bronze_dir = tmp_path / "bronze"
    _write_bronze_run(bronze_dir, "20260801T120000Z")
    _write_bronze_run(bronze_dir, "20260802T120000Z")   # identical roster
    conn = _load(tmp_path)
    for slug in ("20260801T120000Z", "20260802T120000Z"):
        load.load_run(conn, bronze_dir / slug)
    # Unchanged roster payload → a single accounts row despite two runs.
    assert conn.execute("SELECT COUNT(*) FROM accounts").fetchone()[0] == 1


def test_non_complete_run_skipped(tmp_path):
    bronze_dir = tmp_path / "bronze"
    run = _write_bronze_run(bronze_dir, "20260801T120000Z")
    (run / "run.json").write_text(json.dumps({"status": "in-progress"}))
    conn = _load(tmp_path)
    assert load.load_run(conn, run) is False
    assert conn.execute("SELECT COUNT(*) FROM transactions").fetchone()[0] == 0
