"""Unit tests for the firstcitizens silver loader — synthetic bronze only (no
real account ids, balances, or payees). Exercises the history-JSON parser
(signed amount from `isDebit`, running balance, stable id, malformed-row
skipping), the roster projection (mask, balance pick), the statement document
inventory, the statement-rate pass, and the idempotent per-run load.
"""
from __future__ import annotations

import json
import subprocess
import sys
from datetime import datetime, timezone
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import load  # noqa: E402

MIGRATIONS = Path(__file__).resolve().parent.parent / "migrations"

# Synthetic opaque Q2 account id + a masked account number (placeholders).
ACCT_ID = "900001"
MASKED_NUMBER = "XXXXXXXX6789"


@pytest.fixture(autouse=True)
def _no_pdftotext(monkeypatch):
    """Keep `load.main`'s statement pass off the real pdftotext: every
    statement reads as a text with no period. The rate tests stub it
    with their own texts."""
    monkeypatch.setattr(load.statement_parser.pdftotext, "layout_text",
                        lambda path, timeout=None: "")


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


def test_history_rows_skip_a_memo_posted_item():
    """A memo-posted item posts again under a new transactionId, so only the
    posted copy is the ledger's."""
    memo = _tx("M1", "3/3/2098", "300.00", False, "", "EXAMPLE PAYMENT ***")
    memo["transactionType"] = "Memo"
    posted = _tx("H1", "3/3/2098", "300.00", False, "1300.00", "Example Payment")
    posted["transactionType"] = "History"
    rows = load.history_rows(ACCT_ID, [memo, posted])
    assert [r["fitid"] for r in rows] == ["H1"]


def test_a_movement_back_under_a_new_id_is_stored_once(tmp_path):
    """A movement in the open statement cycle can return under a fresh
    transactionId on every run; the host transaction number says it is
    the same one, and the first id keeps it."""
    conn = _load(tmp_path)
    def moved(tid):
        row = _tx(tid, "5/5/2098", "500.00", True, "1000.00", "Transfer To 0000")
        row["transactionType"] = "History"
        row["hostTranNumber"] = "DDA000000000000000000000000000001"
        return row
    other = _tx("T9", "5/5/2098", "500.00", True, "500.00", "Transfer To 0000")
    other["hostTranNumber"] = "DDA000000000000000000000000000002"
    for tid in ("T1", "T2", "T3"):
        for tx in load.history_rows(ACCT_ID, [moved(tid)]):
            load._insert_transaction(conn, ACCT_ID, tx)
    for tx in load.history_rows(ACCT_ID, [other]):
        load._insert_transaction(conn, ACCT_ID, tx)
    ids = sorted(r[0] for r in conn.execute("SELECT fitid FROM transactions"))
    assert ids == ["T1", "T9"]


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
    load.load_run(conn, bronze_dir / "20260801T120000Z")

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


def _main(tmp_path: Path, bronze_dir: Path) -> load.sqlite3.Connection:
    """Run `load.main` over `bronze_dir`, then open its silver DB."""
    db = tmp_path / "firstcitizens.db"
    assert load.main(["--bronze-dir", str(bronze_dir),
                      "--silver-db", str(db)]) == 0
    return load.silver.open_db(db)


def _counts(conn) -> dict[str, int]:
    return {t: conn.execute(f"SELECT COUNT(*) FROM {t}").fetchone()[0]
            for t in ("accounts", "transactions", "documents", "dump_runs")}


def test_load_is_idempotent(tmp_path):
    bronze_dir = tmp_path / "bronze"
    _write_bronze_run(bronze_dir, "20260801T120000Z")
    _main(tmp_path, bronze_dir).close()
    # Re-loading the same dump is a no-op (already-loaded snapshot).
    conn = _main(tmp_path, bronze_dir)
    assert _counts(conn) == {"accounts": 1, "transactions": 3,
                             "documents": 1, "dump_runs": 1}


def test_account_content_dedup_across_runs(tmp_path):
    bronze_dir = tmp_path / "bronze"
    _write_bronze_run(bronze_dir, "20260801T120000Z")
    _write_bronze_run(bronze_dir, "20260802T120000Z")   # identical roster
    conn = _load(tmp_path)
    for slug in ("20260801T120000Z", "20260802T120000Z"):
        load.load_run(conn, bronze_dir / slug)
    # Unchanged roster payload → a single accounts row despite two runs.
    assert conn.execute("SELECT COUNT(*) FROM accounts").fetchone()[0] == 1


def _listings(conn) -> list[tuple[int, str]]:
    return [tuple(r) for r in conn.execute(
        "SELECT snapshot_at, account_external_id FROM run_accounts ORDER BY 1, 2")]


def test_each_run_records_the_accounts_it_listed(tmp_path):
    # The roster is content-deduped, so run_accounts is what shows that a
    # run still listed an unchanged account, and that a later run left one
    # out.
    bronze_dir = tmp_path / "bronze"
    _write_bronze_run(bronze_dir, "20260801T120000Z")
    second = _write_bronze_run(bronze_dir, "20260802T120000Z")
    third = _write_bronze_run(bronze_dir, "20260803T120000Z")
    for run, ids in ((second, [ACCT_ID, "900002"]), (third, ["900002"])):
        roster = json.loads((run / "accounts.json").read_text())
        (run / "accounts.json").write_text(json.dumps(
            [{**roster[0], "id": i} for i in ids] + [{"nickname": "no id"}]))
    conn = _main(tmp_path, bronze_dir)
    day = 86400
    first = _epoch(2026, 8, 1) + 12 * 3600
    assert _listings(conn) == [(first, ACCT_ID), (first + day, ACCT_ID),
                               (first + day, "900002"), (first + 2 * day, "900002")]
    assert conn.execute("SELECT COUNT(*) FROM accounts WHERE account_external_id = ?",
                        (ACCT_ID,)).fetchone()[0] == 1


def test_load_backfills_a_missing_listing_by_the_account_id(tmp_path):
    # The next load reads a loaded run's missing listing from its bronze dir
    # by the roster's `id`, and leaves a recorded run alone.
    bronze_dir = tmp_path / "bronze"
    _write_bronze_run(bronze_dir, "20260801T120000Z")
    _write_bronze_run(bronze_dir, "20260802T120000Z")
    conn = _main(tmp_path, bronze_dir)
    before = _listings(conn)
    conn.execute("DELETE FROM run_accounts WHERE snapshot_at = ?", (before[0][0],))
    conn.close()
    (bronze_dir / "20260802T120000Z" / "accounts.json").write_text("[]")
    assert _listings(_main(tmp_path, bronze_dir)) == before


def test_non_complete_run_skipped(tmp_path):
    bronze_dir = tmp_path / "bronze"
    run = _write_bronze_run(bronze_dir, "20260801T120000Z")
    (run / "run.json").write_text(json.dumps({"status": "in-progress"}))
    conn = _main(tmp_path, bronze_dir)
    assert _counts(conn) == dict.fromkeys(_counts(conn), 0)


def test_run_without_run_json_is_neither_loaded_nor_recorded(tmp_path):
    # A run dir whose manifest is not yet written is still being written;
    # prune reclaims it, so load must not record it as loaded either.
    bronze_dir = tmp_path / "bronze"
    run = _write_bronze_run(bronze_dir, "20260801T120000Z")
    (run / "run.json").unlink()
    conn = _main(tmp_path, bronze_dir)
    assert _counts(conn) == dict.fromkeys(_counts(conn), 0)


def test_failed_load_leaves_silver_unchanged(tmp_path, monkeypatch):
    # One dump is one transaction: a failure part-way rolls back the rows
    # already written and records no dump_runs row, so the next load retries.
    bronze_dir = tmp_path / "bronze"
    run = _write_bronze_run(bronze_dir, "20260801T120000Z")
    conn = _load(tmp_path)

    def boom(*_args, **_kwargs):
        raise OSError("synthetic read failure")

    monkeypatch.setattr(load, "_insert_document", boom)
    with pytest.raises(OSError):
        load.load_run(conn, run)
    for table in ("accounts", "transactions", "documents", "dump_runs"):
        assert conn.execute(f"SELECT COUNT(*) FROM {table}").fetchone()[0] == 0

    monkeypatch.undo()
    load.load_run(conn, run)
    assert conn.execute("SELECT COUNT(*) FROM transactions").fetchone()[0] == 3


# ============================================================
# Statement rates
# ============================================================

STMT_HEADER = ("Statement Period: July 1 , 2026      Thru July 31, 2026"
               "            Account Number :    000000000000\n")


def _stmt_text(rate="3.07%", apy="3.12%"):
    """Synthetic `pdftotext -layout` text of a savings statement; a None
    figure leaves its line out."""
    text = STMT_HEADER + "Beginning Balance        1,000.00+\n"
    if apy:
        text += f"   0  Other Credits   0.00   Annual Percentage Yield Earned*   {apy}\n"
    if rate:
        text += f"Interest Earned    1.00+   Interest Rate on Statement Day    {rate}\n"
    return text


def _with_texts(monkeypatch, texts):
    """Make pdftotext return texts[i] for the i-th file read; returns the
    names read."""
    read: list[str] = []

    def layout_text(path, timeout=None):
        read.append(Path(path).name)
        return texts[len(read) - 1]

    monkeypatch.setattr(load.statement_parser.pdftotext, "layout_text", layout_text)
    return read


def _rates(conn):
    return [tuple(r) for r in conn.execute(
        "SELECT account_external_id, period_end, period_start, interest_rate,"
        " apy_earned, apy FROM statement_rates ORDER BY period_end")]


def test_a_statement_rate_lands_in_silver(tmp_path, monkeypatch):
    bronze_dir = tmp_path / "bronze"
    _write_bronze_run(bronze_dir, "20260801T120000Z")
    read = _with_texts(monkeypatch, [_stmt_text()])
    conn = _main(tmp_path, bronze_dir)
    assert read == ["07-31-2026.pdf"]
    assert _rates(conn) == [(ACCT_ID, _epoch(2026, 7, 31), _epoch(2026, 7, 1),
                             3.07, 3.12, None)]
    sha = conn.execute("SELECT sha256 FROM documents").fetchone()[0]
    assert conn.execute("SELECT source_sha256 FROM statement_rates").fetchone()[0] == sha
    assert load.json.loads(conn.execute("SELECT payload FROM statement_rates")
                           .fetchone()[0])["lines"]["interest_rate"].endswith("3.07%")
    conn.close()
    # A later load reads no statement it has read.
    _main(tmp_path, bronze_dir).close()
    assert read == ["07-31-2026.pdf"]


def test_a_statement_without_a_rate_writes_nothing(tmp_path, monkeypatch):
    bronze_dir = tmp_path / "bronze"
    _write_bronze_run(bronze_dir, "20260801T120000Z")
    _with_texts(monkeypatch, [_stmt_text(rate=None, apy=None)])
    conn = _main(tmp_path, bronze_dir)
    assert _rates(conn) == []
    assert conn.execute("SELECT COUNT(*) FROM parsed_statements").fetchone()[0] == 1


def test_a_yield_the_rate_cannot_give_warns(tmp_path, monkeypatch, caplog):
    bronze_dir = tmp_path / "bronze"
    _write_bronze_run(bronze_dir, "20260801T120000Z")
    _with_texts(monkeypatch, [_stmt_text(rate="2.21%", apy="2.58%")])
    with caplog.at_level("WARNING", logger=load.log.name):
        conn = _main(tmp_path, bronze_dir)
    assert "interest rate 2.21% but apy earned 2.58%" in caplog.text
    assert _rates(conn)[0][3:5] == (2.21, 2.58)


def test_a_yield_within_rounding_does_not_warn(tmp_path, monkeypatch, caplog):
    bronze_dir = tmp_path / "bronze"
    _write_bronze_run(bronze_dir, "20260801T120000Z")
    _with_texts(monkeypatch, [_stmt_text()])
    with caplog.at_level("WARNING", logger=load.log.name):
        _main(tmp_path, bronze_dir).close()
    assert caplog.text == ""


def test_a_copy_with_other_rates_is_kept_out(tmp_path, monkeypatch, caplog):
    bronze_dir = tmp_path / "bronze"
    _write_bronze_run(bronze_dir, "20260801T120000Z")
    second = _write_bronze_run(bronze_dir, "20260802T120000Z")
    # The bank renders the statement anew: other bytes, the same cycle.
    (second / "statements" / ACCT_ID / "07-31-2026.pdf").write_bytes(b"%PDF-1.4 other")
    _with_texts(monkeypatch, [_stmt_text(), _stmt_text(rate="4.35%", apy="4.44%")])
    with caplog.at_level("WARNING", logger=load.log.name):
        conn = _main(tmp_path, bronze_dir)
    assert "another copy states other rates" in caplog.text
    assert [r[3] for r in _rates(conn)] == [3.07]


def test_a_copy_with_the_same_rates_writes_nothing(tmp_path, monkeypatch, caplog):
    bronze_dir = tmp_path / "bronze"
    _write_bronze_run(bronze_dir, "20260801T120000Z")
    second = _write_bronze_run(bronze_dir, "20260802T120000Z")
    (second / "statements" / ACCT_ID / "07-31-2026.pdf").write_bytes(b"%PDF-1.4 other")
    read = _with_texts(monkeypatch, [_stmt_text(), _stmt_text()])
    with caplog.at_level("WARNING", logger=load.log.name):
        conn = _main(tmp_path, bronze_dir)
    assert len(read) == 2 and caplog.text == ""
    assert [r[3] for r in _rates(conn)] == [3.07]
    assert conn.execute("SELECT COUNT(*) FROM parsed_statements").fetchone()[0] == 2


def test_the_sentence_form_yield_is_checked(tmp_path, monkeypatch, caplog):
    bronze_dir = tmp_path / "bronze"
    _write_bronze_run(bronze_dir, "20260801T120000Z")
    _with_texts(monkeypatch, [STMT_HEADER
                              + "AS OF ENDING STATEMENT DATE YOUR INTEREST RATE WAS 2.21% AND YOUR\n"
                              + "ANNUAL PERCENTAGE YIELD WAS 2.58%.\n"])
    with caplog.at_level("WARNING", logger=load.log.name):
        conn = _main(tmp_path, bronze_dir)
    assert "interest rate 2.21% but apy 2.58%" in caplog.text
    assert _rates(conn)[0][3:6] == (2.21, None, 2.58)


def test_a_missing_pdftotext_stops_the_pass(tmp_path, monkeypatch):
    bronze_dir = tmp_path / "bronze"
    _write_bronze_run(bronze_dir, "20260801T120000Z")

    def missing(path, timeout=None):
        raise load.pdftotext.ToolMissing("pdftotext not found")

    monkeypatch.setattr(load.statement_parser.pdftotext, "layout_text", missing)
    db = tmp_path / "firstcitizens.db"
    with pytest.raises(load.pdftotext.ToolMissing):
        load.main(["--bronze-dir", str(bronze_dir), "--silver-db", str(db)])
    conn = load.silver.open_db(db)
    assert conn.execute("SELECT COUNT(*) FROM parsed_statements").fetchone()[0] == 0
    assert conn.execute("SELECT COUNT(*) FROM parser_generations"
                        " WHERE scope = ?", (load.STATEMENT_GENERATION_SCOPE,)
                        ).fetchone()[0] == 0


def test_a_moved_parser_reads_every_statement_again(tmp_path, monkeypatch):
    bronze_dir = tmp_path / "bronze"
    _write_bronze_run(bronze_dir, "20260801T120000Z")
    read = _with_texts(monkeypatch, [_stmt_text(), _stmt_text(rate="4.04%", apy="4.11%")])
    _main(tmp_path, bronze_dir).close()
    monkeypatch.setattr(load, "STATEMENT_GENERATION", "another")
    conn = _main(tmp_path, bronze_dir)
    assert len(read) == 2
    assert [r[3] for r in _rates(conn)] == [4.04]
    assert conn.execute("SELECT generation FROM parser_generations").fetchone()[0] == "another"


def test_a_statement_gone_from_bronze_is_read_later(tmp_path, monkeypatch, caplog):
    bronze_dir = tmp_path / "bronze"
    run = _write_bronze_run(bronze_dir, "20260801T120000Z")
    pdf = run / "statements" / ACCT_ID / "07-31-2026.pdf"
    body = pdf.read_bytes()
    conn = _load(tmp_path)
    load.load_run(conn, run)
    pdf.unlink()
    read = _with_texts(monkeypatch, [_stmt_text()])
    with caplog.at_level("WARNING", logger=load.log.name):
        load.refresh_statement_rates(conn, bronze_dir)
    assert read == [] and "not in bronze" in caplog.text
    pdf.write_bytes(body)
    load.refresh_statement_rates(conn, bronze_dir)
    assert read == ["07-31-2026.pdf"] and len(_rates(conn)) == 1


def test_an_unreadable_statement_is_marked(tmp_path, monkeypatch, caplog):
    bronze_dir = tmp_path / "bronze"
    _write_bronze_run(bronze_dir, "20260801T120000Z")

    def fail(path, timeout=None):
        raise load.pdftotext.ExtractionError("pdftotext exited 1")

    monkeypatch.setattr(load.statement_parser.pdftotext, "layout_text", fail)
    with caplog.at_level("WARNING", logger=load.log.name):
        conn = _main(tmp_path, bronze_dir)
    assert "pdftotext exited 1" in caplog.text
    assert _rates(conn) == []
    assert conn.execute("SELECT COUNT(*) FROM parsed_statements").fetchone()[0] == 1


def test_a_timed_out_statement_is_read_again(tmp_path, monkeypatch, caplog):
    bronze_dir = tmp_path / "bronze"
    _write_bronze_run(bronze_dir, "20260801T120000Z")

    def slow(path, timeout=None):
        try:
            raise subprocess.TimeoutExpired("pdftotext", timeout or 60)
        except subprocess.TimeoutExpired as exc:
            raise load.pdftotext.ExtractionError("pdftotext timed out") from exc

    monkeypatch.setattr(load.statement_parser.pdftotext, "layout_text", slow)
    with caplog.at_level("WARNING", logger=load.log.name):
        conn = _main(tmp_path, bronze_dir)
    assert "timed out" in caplog.text
    assert conn.execute("SELECT COUNT(*) FROM parsed_statements").fetchone()[0] == 0


def test_a_statement_of_an_unknown_layout_warns(tmp_path, monkeypatch, caplog):
    bronze_dir = tmp_path / "bronze"
    _write_bronze_run(bronze_dir, "20260801T120000Z")
    monkeypatch.setattr(load.statement_parser.pdftotext, "layout_text",
                        lambda path, timeout=None: "AN EXAMPLE PAGE WITH NO PERIOD\n")
    with caplog.at_level("WARNING", logger=load.log.name):
        conn = _main(tmp_path, bronze_dir)
    assert "no statement period read" in caplog.text
    assert _rates(conn) == []
