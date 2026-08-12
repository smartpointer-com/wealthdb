"""Unit tests for the chase silver loader — synthetic bronze only (no real
account numbers, balances, or payees). Exercises the QFX/CSV parsers, the
CSV↔QFX balance join, id synthesis, the document inventory, and the
idempotent per-run load.
"""
from __future__ import annotations

import json
import sqlite3
import sys
from datetime import datetime, timezone
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import load  # noqa: E402

MIGRATIONS = Path(__file__).resolve().parent.parent / "migrations"

# Synthetic account id + mask (placeholders, never a real Chase id).
EXT = "900001"

QFX = """OFXHEADER:100
DATA:OFXSGML
VERSION:102

<OFX><BANKMSGSRSV1><STMTTRNRS><STMTRS>
<CURDEF>USD
<BANKACCTFROM><ACCTID>900001<ACCTTYPE>CHECKING</BANKACCTFROM>
<BANKTRANLIST>
<STMTTRN><TRNTYPE>DEBIT<DTPOSTED>20260715120000<TRNAMT>-42.50<FITID>F001<NAME>COFFEE BAR<MEMO>card 1234</STMTTRN>
<STMTTRN><TRNTYPE>CREDIT<DTPOSTED>20260716<TRNAMT>1500.00<FITID>F002<NAME>PAYROLL</STMTTRN>
<STMTTRN><TRNTYPE>CHECK<DTPOSTED>20260717<TRNAMT>-200.00<FITID>F003<NAME>CHECK<CHECKNUM>1088</STMTTRN>
</BANKTRANLIST></STMTRS></STMTTRNRS></BANKMSGSRSV1></OFX>
"""

# Same three posted rows (with running balance), plus one pending row the
# QFX doesn't carry.
CSV = """Details,Posting Date,Description,Amount,Type,Balance,Check or Slip #
DEBIT,07/15/2026,COFFEE BAR,-42.50,ACH_DEBIT,957.50,
CREDIT,07/16/2026,PAYROLL,1500.00,ACH_CREDIT,2457.50,
CHECK,07/17/2026,CHECK 1088,-200.00,CHECK_PAID,2257.50,1088
DEBIT,07/18/2026,PENDING COFFEE,-9.99,DEBIT_CARD,,
"""


def _epoch(y, m, d):
    return int(datetime(y, m, d, tzinfo=timezone.utc).timestamp())


# ============================================================
# Parsers
# ============================================================

def test_parse_qfx_rows():
    rows = load.parse_qfx(QFX)
    assert [r["fitid"] for r in rows] == ["F001", "F002", "F003"]
    assert rows[0]["amount"] == -42.50
    assert rows[0]["posted_at"] == _epoch(2026, 7, 15)
    assert rows[0]["kind"] == "DEBIT"
    assert rows[0]["description"] == "COFFEE BAR"
    assert rows[2]["check_number"] == "1088"


def test_parse_csv_rows_and_balance():
    rows = load.parse_csv(CSV)
    assert len(rows) == 4
    assert rows[0]["balance"] == 957.50
    assert rows[3]["balance"] is None          # pending row has no balance
    assert rows[3]["description"] == "PENDING COFFEE"


def test_parse_money_variants():
    assert load.parse_money("$1,234.56") == 1234.56
    assert load.parse_money("(50.00)") == -50.00
    assert load.parse_money("") is None
    assert load.parse_money(None) is None


def test_parse_dates():
    assert load.parse_ofx_date("20260715120000") == _epoch(2026, 7, 15)
    assert load.parse_csv_date("07/15/2026") == _epoch(2026, 7, 15)
    assert load.parse_ofx_date("garbage") is None


# ============================================================
# Join
# ============================================================

def test_merge_attaches_balance_and_keeps_pending():
    merged = load.merge_transactions(EXT, load.parse_qfx(QFX), load.parse_csv(CSV))
    by_id = {m["fitid"]: m for m in merged}
    # QFX rows keep their FITID and gain the CSV running balance.
    assert by_id["F001"]["balance"] == 957.50
    assert by_id["F002"]["balance"] == 2457.50
    assert by_id["F003"]["balance"] == 2257.50
    assert all(by_id[f]["source"] == "qfx" for f in ("F001", "F002", "F003"))
    # The CSV-only pending row survives under a synthetic id.
    synth = [m for m in merged if m["fitid"].startswith("syn_")]
    assert len(synth) == 1
    assert synth[0]["description"] == "PENDING COFFEE"
    assert synth[0]["balance"] is None
    assert synth[0]["source"] == "csv"


def test_synth_fitid_is_deterministic():
    a = load.synth_fitid(EXT, _epoch(2026, 7, 18), -9.99, "PENDING COFFEE", None)
    b = load.synth_fitid(EXT, _epoch(2026, 7, 18), -9.99, "PENDING COFFEE", None)
    assert a == b and a.startswith("syn_")


def test_merge_pairs_duplicate_same_day_amounts_one_to_one():
    # Two identical-amount same-day QFX rows must each consume a distinct CSV
    # balance, not both grab the first.
    qfx = load.parse_qfx(
        "<STMTTRN><DTPOSTED>20260701<TRNAMT>-5.00<FITID>A</STMTTRN>"
        "<STMTTRN><DTPOSTED>20260701<TRNAMT>-5.00<FITID>B</STMTTRN>")
    csv = load.parse_csv(
        "Details,Posting Date,Description,Amount,Type,Balance,Check or Slip #\n"
        "DEBIT,07/01/2026,X,-5.00,D,100.00,\n"
        "DEBIT,07/01/2026,X,-5.00,D,95.00,\n")
    merged = load.merge_transactions(EXT, qfx, csv)
    balances = sorted(m["balance"] for m in merged)
    assert balances == [95.00, 100.00]
    assert len(merged) == 2


# ============================================================
# Full per-run load (idempotent)
# ============================================================

def _make_run(root: Path, slug: str, *, status="complete") -> Path:
    d = root / slug
    (d / "transactions").mkdir(parents=True)
    (d / "statements" / EXT).mkdir(parents=True)
    manifest = {"schema": 1, "status": status}
    (d / "run.json").write_text(json.dumps(manifest))
    (d / "accounts.json").write_text(json.dumps([{
        "account_external_id": EXT, "account_type": "CHK",
        "nickname": "TOTAL CHECKING", "mask": "…1234",
        "currency": "USD", "balance": 2257.50,
    }]))
    (d / "transactions" / f"{EXT}.qfx").write_text(QFX)
    (d / "transactions" / f"{EXT}.csv").write_text(CSV)
    (d / "statements" / EXT / "2026-07-31-statement.pdf").write_bytes(b"%PDF-1.4 fake")
    return d


def _conn():
    c = sqlite3.connect(":memory:")
    load.apply_migrations(c, MIGRATIONS)
    return c


def test_load_run_populates_all_tables():
    import tempfile
    with tempfile.TemporaryDirectory() as tmp:
        root = Path(tmp)
        run = _make_run(root, "20260801T120000Z")
        conn = _conn()
        assert load.load_run(conn, run) is True
        assert conn.execute("SELECT COUNT(*) FROM accounts").fetchone()[0] == 1
        assert conn.execute("SELECT COUNT(*) FROM transactions").fetchone()[0] == 4
        assert conn.execute("SELECT COUNT(*) FROM documents").fetchone()[0] == 1
        assert conn.execute("SELECT COUNT(*) FROM dump_runs").fetchone()[0] == 1
        bal = conn.execute(
            "SELECT balance FROM transactions WHERE fitid='F002'").fetchone()[0]
        assert bal == 2457.50
        acct = conn.execute(
            "SELECT account_type, mask FROM accounts").fetchone()
        assert acct == ("CHK", "…1234")


def test_load_run_is_idempotent():
    import tempfile
    with tempfile.TemporaryDirectory() as tmp:
        root = Path(tmp)
        run = _make_run(root, "20260801T120000Z")
        conn = _conn()
        assert load.load_run(conn, run) is True
        assert load.load_run(conn, run) is False       # already loaded
        assert conn.execute("SELECT COUNT(*) FROM transactions").fetchone()[0] == 4


def test_load_run_skips_in_progress():
    import tempfile
    with tempfile.TemporaryDirectory() as tmp:
        root = Path(tmp)
        run = _make_run(root, "20260801T120000Z", status="in-progress")
        conn = _conn()
        assert load.load_run(conn, run) is False
        assert conn.execute("SELECT COUNT(*) FROM transactions").fetchone()[0] == 0


def test_main_force_rebuild_equals_incremental(tmp_path):
    root = tmp_path / "bronze"
    root.mkdir()
    _make_run(root, "20260801T120000Z")
    _make_run(root, "20260901T120000Z")
    db = tmp_path / "chase.db"
    assert load.main(["--bronze-dir", str(root), "--silver-db", str(db)]) == 0
    n1 = sqlite3.connect(db).execute("SELECT COUNT(*) FROM transactions").fetchone()[0]
    assert load.main(["--bronze-dir", str(root), "--silver-db", str(db), "--force"]) == 0
    n2 = sqlite3.connect(db).execute("SELECT COUNT(*) FROM transactions").fetchone()[0]
    assert n1 == n2 == 4
