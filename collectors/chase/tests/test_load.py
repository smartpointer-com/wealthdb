"""Unit tests for the chase silver loader — synthetic bronze only (no real
account numbers, balances, or payees). Exercises the QFX/CSV parsers, the
CSV↔QFX balance join, id synthesis, the document inventory, and the
idempotent per-run load.
"""
from __future__ import annotations

import json
import sqlite3
import sys
from datetime import date, datetime, timezone
from decimal import Decimal
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import load  # noqa: E402
import statement_parser as sp  # noqa: E402

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
        "nickname": "Example Checking", "mask": "…1234",
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


# ============================================================
# Statement-PDF transactions — seam + import
# ============================================================

def _seed_export(conn, rows):
    for posted, amt in rows:
        load._insert_transaction(conn, EXT, {
            "fitid": f"F{posted}_{amt}", "posted_at": posted, "amount": amt,
            "kind": None, "description": "x", "check_number": None,
            "balance": None, "source": "qfx", "payload": {}})
    conn.commit()


def test_export_seam_anchors_to_export_rows_only():
    conn = _conn()
    e1, e2 = _epoch(2024, 8, 15), _epoch(2026, 1, 1)
    _seed_export(conn, [(e1, -10.0), (e2, 20.0)])
    assert load._export_seam(conn) == e1
    # an earlier STATEMENT-sourced row must not drag the seam down.
    load._insert_transaction(conn, EXT, {
        "fitid": "stmt_x", "posted_at": _epoch(2020, 1, 1), "amount": 5.0,
        "kind": None, "description": None, "check_number": None,
        "balance": None, "source": "statement", "payload": {}})
    conn.commit()
    assert load._export_seam(conn) == e1


def test_export_seam_none_without_export():
    assert load._export_seam(_conn()) is None


def test_import_statement_gates_at_seam_and_reconstructs_balance():
    conn = _conn()
    seam = _epoch(2024, 8, 11)
    parsed = _seg("100.00", "320.00", [
        (date(2024, 7, 20), "50.00", "deposit"),        # before seam → kept
        (date(2024, 8, 5), "-30.00", "withdrawal"),     # before seam → kept
        (date(2024, 8, 15), "200.00", "post-seam"),     # on/after seam → dropped
    ])
    assert load._import_statement(conn, EXT, parsed, seam) == 2
    rows = conn.execute("SELECT posted_at, balance, source FROM transactions "
                        "ORDER BY posted_at").fetchall()
    assert [r[0] for r in rows] == [_epoch(2024, 7, 20), _epoch(2024, 8, 5)]
    assert all(r[2] == "statement" for r in rows)
    assert rows[0][1] == 150.0 and rows[1][1] == 120.0   # 100+50, then -30


def test_import_statement_disambiguates_identical_same_day():
    conn = _conn()
    parsed = _seg("0.00", "10.00", [
        (date(2023, 3, 1), "5.00", "COFFEE"),
        (date(2023, 3, 1), "5.00", "COFFEE"),            # identical → both survive
    ])
    assert load._import_statement(conn, EXT, parsed, None) == 2
    assert conn.execute("SELECT COUNT(*) FROM transactions WHERE source='statement'"
                        ).fetchone()[0] == 2


def test_statement_fitid_stable_and_occurrence_distinct():
    a = load._statement_fitid(EXT, 100, -5.0, "x", 0)
    assert a == load._statement_fitid(EXT, 100, -5.0, "x", 0)   # deterministic
    assert a != load._statement_fitid(EXT, 100, -5.0, "x", 1)   # occ disambiguates
    assert a.startswith("stmt_")


# ============================================================
# Statement segments — anchor + balance chain
# ============================================================

def _seg(begin, end, txns=()):
    return sp.StatementSegment(
        beginning_balance=Decimal(begin), ending_balance=Decimal(end),
        transactions=[sp.StatementTxn(posted_at=d, amount=Decimal(a),
                                      description=desc) for d, a, desc in txns])


def _multi(ps, pe, segs):
    return sp.ParsedStatement(period_start=ps, period_end=pe, segments=segs)


def _seed_export_balance(conn, posted, amount, balance):
    load._insert_transaction(conn, EXT, {
        "fitid": f"F{posted}", "posted_at": posted, "amount": amount,
        "kind": None, "description": "x", "check_number": None,
        "balance": balance, "source": "qfx", "payload": {}})
    conn.commit()


def test_chain_anchors_on_export_balance_and_walks_back():
    conn = _conn()
    # One export row inside the newest statement's period; its running balance
    # identifies the account's segment (the other product's can't match).
    _seed_export_balance(conn, _epoch(2024, 8, 12), -10.0, 1400.0)
    newest = _multi(date(2024, 7, 20), date(2024, 8, 19), [
        _seg("100.00", "130.00", [(date(2024, 8, 1), "30.00", "other product")]),
        _seg("1300.00", "1400.00", [(date(2024, 7, 25), "100.00", "deposit")]),
    ])
    older = _multi(date(2024, 6, 20), date(2024, 7, 19), [
        _seg("90.00", "100.00", []),
        _seg("1250.00", "1300.00", [(date(2024, 7, 1), "50.00", "deposit")]),
    ])
    chained = load._chain_segments(conn, EXT, [newest, older])
    assert [seg.ending_balance for _, seg in chained] == \
        [Decimal("1400.00"), Decimal("1300.00")]


def test_chain_stops_on_ambiguity_or_break():
    conn = _conn()
    _seed_export_balance(conn, _epoch(2024, 8, 12), -10.0, 1400.0)
    newest = _multi(date(2024, 7, 20), date(2024, 8, 19), [
        _seg("1300.00", "1400.00", []),
    ])
    # both segments end at the expected 1300.00 → ambiguous → stop
    tie = _multi(date(2024, 6, 20), date(2024, 7, 19), [
        _seg("1250.00", "1300.00", []), _seg("900.00", "1300.00", []),
    ])
    oldest = _multi(date(2024, 5, 20), date(2024, 6, 19), [
        _seg("1200.00", "1250.00", []),
    ])
    chained = load._chain_segments(conn, EXT, [newest, tie, oldest])
    assert len(chained) == 1                       # tie and older both dropped
    # a gap (no segment ends at the expected balance) stops the walk too
    gap = _multi(date(2024, 6, 20), date(2024, 7, 19), [
        _seg("1000.00", "1111.00", []),
    ])
    assert len(load._chain_segments(conn, EXT, [newest, gap, oldest])) == 1


def test_chain_without_export_anchor_matches_nothing():
    conn = _conn()          # no export rows at all → no anchor candidates
    newest = _multi(date(2024, 7, 20), date(2024, 8, 19),
                    [_seg("1300.00", "1400.00", [])])
    assert load._chain_segments(conn, EXT, [newest]) == []


def test_load_statement_transactions_end_to_end(tmp_path, monkeypatch):
    # Fake PDFs (bytes only matter for sha dedup); the parse is monkeypatched
    # to synthetic statements so no pdftotext is needed.
    conn = _conn()
    seam = _epoch(2024, 8, 11)
    _seed_export_balance(conn, seam, -10.0, 1400.0)
    straddler = _multi(date(2024, 7, 20), date(2024, 8, 19), [
        _seg("100.00", "130.00", [(date(2024, 8, 1), "30.00", "other product")]),
        _seg("1310.00", "1400.00", [
            (date(2024, 7, 25), "100.00", "pre-seam deposit"),
            (date(2024, 8, 15), "-10.00", "post-seam row"),
        ]),
    ])
    older = _multi(date(2024, 6, 20), date(2024, 7, 19), [
        _seg("90.00", "100.00", [(date(2024, 7, 2), "10.00", "other product")]),
        _seg("1250.00", "1310.00", [(date(2024, 7, 1), "60.00", "deposit")]),
    ])
    post_seam = _multi(date(2026, 6, 20), date(2026, 7, 19), [
        _seg("1500.00", "1500.00", []),
    ])
    # 2026-07-19.pdf: its filename date is far past the seam, so the
    # prefilter must skip it without ever invoking the parser.
    by_name = {"a.pdf": straddler, "b.pdf": older, "c.pdf": post_seam,
               "2026-07-19.pdf": post_seam}

    for run in ("20260801T000000Z", "20260802T000000Z"):     # overlapping runs
        d = tmp_path / run / "statements" / EXT
        d.mkdir(parents=True)
        for name in by_name:
            (d / name).write_bytes(b"%PDF fake " + name.encode())
    parsed_names = []

    def fake_parse(path):
        parsed_names.append(path.name)
        return by_name[path.name]

    monkeypatch.setattr(load.statement_parser, "parse_statement_pdf", fake_parse)

    n = load.load_statement_transactions(conn, tmp_path)
    assert "2026-07-19.pdf" not in parsed_names
    assert n == 2                        # pre-seam rows only, imported once
    rows = conn.execute(
        "SELECT posted_at, amount, balance FROM transactions "
        "WHERE source='statement' ORDER BY posted_at").fetchall()
    assert [r[0] for r in rows] == [_epoch(2024, 7, 1), _epoch(2024, 7, 25)]
    # running balances reconstructed within each segment
    assert rows[0][2] == 1310.0 and rows[1][2] == 1410.0
    # nothing from the other product's segments ever lands
    assert conn.execute(
        "SELECT COUNT(*) FROM transactions WHERE description LIKE '%other%'"
        ).fetchone()[0] == 0
