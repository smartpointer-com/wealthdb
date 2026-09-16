"""Unit tests for the chase silver loader — synthetic bronze only (no real
account numbers, balances, or payees). Exercises the QFX/CSV parsers for both
products, the deposit CSV↔QFX balance join and the card CSV↔QFX field join,
id synthesis for both ledgers (content keys that hold only what the two
exports state identically, the occurrence index that keeps identical same-day
rows apart, the card ledger's duplicate FITIDs, and convergence when only one
format arrives), the field authority that repairs a card row the pair reaches
after one format alone did — stated by the join and additive, so a column the
incoming export leaves blank is never blanked in silver — the casefolded
column lookups and the drift report that keep a re-cased export header from
silently emptying a ledger, the file-content routing the exports and the
statement pass share, the
per-account export seam, unconditional ingest of a partial dump, the document
inventory, the idempotent per-run load, the refusal to load incrementally
into a pre-0002 silver DB, and the flag that makes the tree-wide post passes
re-runnable after one of them failed.

Also the two statement passes and the balance rules they answer to: which
balances are the provider's own (the deposit CSV column, unmarked) and which
are reconstructed (every other one, marked); the card statement pass's period
balances, its per-period transaction gate and the coverage flag on the period
that straddles the export seam; and the export-era balance reconstruction,
including the two agreeing signals it demands before it will rewrite an
account's balances at all.
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

# Synthetic account ids + masks (placeholders, never a real Chase id).
EXT = "900001"
CARD_EXT = "900002"
EXT2 = "900003"

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
# QFX doesn't carry. The check row's `Description` is NOT the QFX `<NAME>` —
# the deposit exports state the descriptor differently, which is why no
# descriptor is in the deposit content key.
CSV = """Details,Posting Date,Description,Amount,Type,Balance,Check or Slip #
DEBIT,07/15/2026,COFFEE BAR,-42.50,ACH_DEBIT,957.50,
CREDIT,07/16/2026,PAYROLL,1500.00,ACH_CREDIT,2457.50,
CHECK,07/17/2026,CHECK 1088,-200.00,CHECK_PAID,2257.50,1088
DEBIT,07/18/2026,PENDING COFFEE,-9.99,DEBIT_CARD,,
"""
CSV_HEADER = CSV.splitlines()[0] + "\n"
# The same file without the pending row, so the two exports cover exactly the
# same three events — the shape the format-independence tests need.
CSV_POSTED = "\n".join(CSV.splitlines()[:4]) + "\n"
DEPOSIT_ROWS = 3

# A card export, in the credit-card OFX wrapper. Five rows, post-date
# descending like the real export, covering the two id hazards:
#   * C003 is issued TWICE — a same-day credit that offsets the charge it
#     reverses reuses its FITID (4 distinct fitids over 5 rows);
#   * the two C001/C002 rows are identical in (post date, amount, descriptor),
#     so a content-derived key collides on them.
# TRNTYPE is only ever DEBIT/CREDIT; the descriptors keep their commas, which
# the CSV replaces with spaces.
CARD_QFX = """OFXHEADER:100
DATA:OFXSGML
VERSION:102

<OFX><CREDITCARDMSGSRSV1><CCSTMTTRNRS><CCSTMTRS>
<CURDEF>USD
<CCACCTFROM><ACCTID>000000000-0002</CCACCTFROM>
<BANKTRANLIST>
<STMTTRN><TRNTYPE>CREDIT<DTPOSTED>20260718<TRNAMT>240.00<FITID>CP01<NAME>EXAMPLE PAYMENT THANK YOU</STMTTRN>
<STMTTRN><TRNTYPE>DEBIT<DTPOSTED>20260716<TRNAMT>-19.99<FITID>C001<NAME>EXAMPLE MERCHANT, INC</STMTTRN>
<STMTTRN><TRNTYPE>DEBIT<DTPOSTED>20260716<TRNAMT>-19.99<FITID>C002<NAME>EXAMPLE MERCHANT, INC</STMTTRN>
<STMTTRN><TRNTYPE>DEBIT<DTPOSTED>20260715<TRNAMT>-42.50<FITID>C003<NAME>EXAMPLE CAFE</STMTTRN>
<STMTTRN><TRNTYPE>CREDIT<DTPOSTED>20260715<TRNAMT>42.50<FITID>C003<NAME>EXAMPLE CAFE</STMTTRN>
</BANKTRANLIST></CCSTMTRS></CCSTMTTRNRS></CREDITCARDMSGSRSV1></OFX>
"""

# The same five rows in the card CSV, whose header is not the deposit
# export's: a transaction date, a post date, a category, the 5-way Type, and
# no balance or currency column. Category is empty on the Payment row only;
# Memo is empty throughout. Signs are provider-verbatim — spend negative.
CARD_CSV = """Transaction Date,Post Date,Description,Category,Type,Amount,Memo
07/17/2026,07/18/2026,EXAMPLE PAYMENT THANK YOU,,Payment,240.00,
07/15/2026,07/16/2026,EXAMPLE MERCHANT  INC,Shopping,Sale,-19.99,
07/15/2026,07/16/2026,EXAMPLE MERCHANT  INC,Shopping,Sale,-19.99,
07/14/2026,07/15/2026,EXAMPLE CAFE,Food & Drink,Sale,-42.50,
07/14/2026,07/15/2026,EXAMPLE CAFE,Food & Drink,Adjustment,42.50,
"""
CARD_ROWS = 5


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
    # The QFX rows gain the CSV running balance; their provider FITID is
    # carried in the payload, not as the identity.
    by_fitid = {m["payload"]["fitid"]: m for m in merged}
    assert by_fitid["F001"]["balance"] == 957.50
    assert by_fitid["F002"]["balance"] == 2457.50
    assert by_fitid["F003"]["balance"] == 2257.50
    assert all(by_fitid[f]["source"] == "qfx" for f in ("F001", "F002", "F003"))
    # The CSV-only pending row survives, reporting no provider id at all.
    pending = [m for m in merged if m["source"] == "csv"]
    assert len(pending) == 1
    assert pending[0]["description"] == "PENDING COFFEE"
    assert pending[0]["balance"] is None
    assert pending[0]["payload"]["fitid"] is None


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


def test_merge_joins_when_only_the_csv_carries_a_check_number():
    # The two exports disagree about the check number: the CSV can carry one
    # the QFX omits. With it in the join key the row failed to match, so the
    # QFX row landed under its FITID and the SAME row landed again as a
    # synthetic — a double-counted withdrawal in the ledger that feeds the
    # spending base.
    qfx = load.parse_qfx(
        "<STMTTRN><DTPOSTED>20260701<TRNAMT>-75.00<FITID>K1<NAME>CHECK</STMTTRN>")
    csv = load.parse_csv(
        "Details,Posting Date,Description,Amount,Type,Balance,Check or Slip #\n"
        "CHECK,07/01/2026,CHECK 2051,-75.00,CHECK_PAID,900.00,2051\n")
    merged = load.merge_transactions(EXT, qfx, csv)
    assert len(merged) == 1                            # one row, not two
    assert merged[0]["payload"]["fitid"] == "K1"
    assert merged[0]["balance"] == 900.00              # and it joined


def test_merge_prefers_the_matching_check_number_inside_a_bucket():
    # Two same-day same-amount checks share a (date, amount) bucket. The check
    # number is the tie-break, so each row takes ITS OWN balance rather than
    # whichever candidate happens to sit first.
    qfx = load.parse_qfx(
        "<STMTTRN><DTPOSTED>20260701<TRNAMT>-50.00<FITID>K1<NAME>CHECK"
        "<CHECKNUM>2002</STMTTRN>"
        "<STMTTRN><DTPOSTED>20260701<TRNAMT>-50.00<FITID>K2<NAME>CHECK"
        "<CHECKNUM>2001</STMTTRN>")
    csv = load.parse_csv(
        "Details,Posting Date,Description,Amount,Type,Balance,Check or Slip #\n"
        "CHECK,07/01/2026,CHECK 2001,-50.00,CHECK_PAID,500.00,2001\n"
        "CHECK,07/01/2026,CHECK 2002,-50.00,CHECK_PAID,450.00,2002\n")
    merged = load.merge_transactions(EXT, qfx, csv)
    assert {m["payload"]["fitid"]: m["balance"] for m in merged} == \
        {"K1": 450.00, "K2": 500.00}


# ============================================================
# Deposit transaction ids — namespaced + occurrence-indexed
# ============================================================

def test_deposit_ids_are_namespaced_and_content_derived():
    merged = load.merge_transactions(EXT, load.parse_qfx(QFX), load.parse_csv(CSV))
    assert all(m["fitid"].startswith(f"dda:{EXT}:") for m in merged)
    # Never the provider's FITID, which is payload only.
    assert not any(m["payload"]["fitid"] and m["payload"]["fitid"] in m["fitid"]
                   for m in merged)
    # …and the two products' id spaces cannot collide, even on identical
    # content, because the product and the account namespace every id.
    card = load.merge_card_transactions(CARD_EXT, load.parse_qfx(CARD_QFX),
                                        load.parse_card_csv(CARD_CSV))
    assert not {m["fitid"] for m in merged} & {m["fitid"] for m in card}


def test_deposit_ids_do_not_depend_on_which_format_arrived():
    # Each format is fetched separately and can fail on its own, so the same
    # rows can arrive CSV-only, QFX-only, or as a pair. All three must key
    # identically — a FITID-keyed id gave the QFX rows one identity and the
    # CSV rows another, so a CSV-only run followed by a complete one landed
    # every row twice and never converged.
    qfx, csv = load.parse_qfx(QFX), load.parse_csv(CSV_POSTED)
    pair = {m["fitid"] for m in load.merge_transactions(EXT, qfx, csv)}
    csv_only = {m["fitid"] for m in load.merge_transactions(EXT, [], csv)}
    qfx_only = {m["fitid"] for m in load.merge_transactions(EXT, qfx, [])}
    assert pair == csv_only == qfx_only
    assert len(pair) == DEPOSIT_ROWS


def test_the_deposit_key_holds_no_descriptor_the_two_exports_disagree_on():
    # The check row is the fixture's descriptor divergence: the QFX `<NAME>` is
    # "CHECK" while the CSV `Description` spells out the check number. A key
    # containing the descriptor would give that one row two identities.
    qfx = [r for r in load.parse_qfx(QFX) if r["description"] == "CHECK"]
    csv = [r for r in load.parse_csv(CSV_POSTED)
           if r["description"] == "CHECK 1088"]
    assert qfx and csv                              # the fixture still diverges
    assert [m["fitid"] for m in load.merge_transactions(EXT, qfx, [])] == \
        [m["fitid"] for m in load.merge_transactions(EXT, [], csv)]


def test_the_deposit_key_holds_no_check_number_either():
    # The CSV can carry a check number the QFX omits (the same disagreement
    # that keeps it out of the join key), so it cannot be in the id key.
    qfx = load.parse_qfx(
        "<STMTTRN><DTPOSTED>20260701<TRNAMT>-75.00<FITID>K1<NAME>CHECK</STMTTRN>")
    csv = load.parse_csv(
        CSV_HEADER + "CHECK,07/01/2026,CHECK 2051,-75.00,CHECK_PAID,900.00,2051\n")
    assert [m["fitid"] for m in load.merge_transactions(EXT, qfx, [])] == \
        [m["fitid"] for m in load.merge_transactions(EXT, [], csv)]


def test_deposit_identical_same_day_rows_keep_distinct_ids():
    # Two rows identical in date and amount are ordinary on a deposit account.
    # Without an occurrence index a content key collapses them into ONE id and
    # the ledger silently loses a row.
    csv = load.parse_csv(
        CSV_HEADER +
        "DEBIT,07/01/2026,STORE A,-5.00,DEBIT_CARD,100.00,\n"
        "DEBIT,07/01/2026,STORE B,-5.00,DEBIT_CARD,95.00,\n")
    merged = load.merge_transactions(EXT, [], csv)
    key = load.deposit_content_key(EXT, _epoch(2026, 7, 1), -5.00)
    assert [m["fitid"] for m in merged] == [
        load._txn_id(load.PRODUCT_DEPOSIT, EXT, key, 0),
        load._txn_id(load.PRODUCT_DEPOSIT, EXT, key, 1)]
    assert {m["balance"] for m in merged} == {100.00, 95.00}


def test_deposit_ids_converge_when_the_qfx_covers_only_some_rows():
    # A partial QFX splits a colliding content key across the merge's two
    # passes (QFX-matched first, then the CSV leftovers). The occurrence
    # counter spans both, so the id set is still exactly the CSV-only one.
    csv = load.parse_csv(
        CSV_HEADER +
        "DEBIT,07/01/2026,STORE A,-5.00,DEBIT_CARD,100.00,\n"
        "DEBIT,07/01/2026,STORE B,-5.00,DEBIT_CARD,95.00,\n")
    partial = load.parse_qfx(
        "<STMTTRN><DTPOSTED>20260701<TRNAMT>-5.00<FITID>A<NAME>STORE A</STMTTRN>")
    assert {m["fitid"] for m in load.merge_transactions(EXT, partial, csv)} == \
        {m["fitid"] for m in load.merge_transactions(EXT, [], csv)}


def test_deposit_ids_are_stable_across_reparses(tmp_path):
    # Re-PARSE from files rather than re-calling the merge on one parsed list:
    # only a fresh parse can catch an id that depends on parse order or on
    # object identity rather than on content.
    (tmp_path / "d.qfx").write_text(QFX)
    (tmp_path / "d.csv").write_text(CSV)

    def _ids(ext):
        return [m["fitid"] for m in load.merge_transactions(
            ext,
            load.parse_qfx((tmp_path / "d.qfx").read_text()),
            load.parse_csv((tmp_path / "d.csv").read_text()))]

    assert _ids(EXT) == _ids(EXT)
    # …and are namespaced per account, so two deposit accounts never share one.
    assert not set(_ids(EXT)) & set(_ids(EXT2))


# ============================================================
# Full per-run load (idempotent)
# ============================================================

def _make_run(root: Path, slug: str, *, status="complete", card=False,
              coverage=None, card_formats=("qfx", "csv"),
              deposit_formats=("qfx", "csv")) -> Path:
    """A synthetic bronze run. Without `card` the roster is the pre-card shape
    (no `product` key and no coverage block at all); with it, every record is
    stamped and a card account gets its own export + statement. `coverage`
    writes the manifest's per-product coverage block — which the loader does
    not read, but which must not change what it ingests. `card_formats` /
    `deposit_formats` narrow an account's export to one file, as a run whose
    other download failed leaves behind."""
    d = root / slug
    (d / "transactions").mkdir(parents=True)
    (d / "statements" / EXT).mkdir(parents=True)
    manifest = {"schema": 1, "status": status}
    if coverage is not None:
        manifest["coverage"] = coverage
    (d / "run.json").write_text(json.dumps(manifest))
    deposit = {
        "account_external_id": EXT, "account_type": "CHK",
        "nickname": "Example Checking", "mask": "…1234",
        "currency": "USD", "balance": 2257.50,
    }
    roster = [deposit]
    if card:
        deposit["product"] = "dda"
        roster.append({
            "account_external_id": CARD_EXT, "product": "card",
            "account_type": "BAC", "nickname": "Example Card",
            "mask": "…5678", "currency": "USD", "balance": 431.00,
            "credit_limit": 5000.00, "available_credit": 4569.00,
            "pending_charges_amount": 12.25,
        })
        if "qfx" in card_formats:
            (d / "transactions" / f"{CARD_EXT}.qfx").write_text(CARD_QFX)
        if "csv" in card_formats:
            (d / "transactions" / f"{CARD_EXT}.csv").write_text(CARD_CSV)
        (d / "statements" / CARD_EXT).mkdir(parents=True)
        (d / "statements" / CARD_EXT / "2026-07-31-statement.pdf") \
            .write_bytes(b"%PDF-1.4 fake card")
    (d / "accounts.json").write_text(json.dumps(roster))
    if "qfx" in deposit_formats:
        (d / "transactions" / f"{EXT}.qfx").write_text(QFX)
    if "csv" in deposit_formats:
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
            "SELECT balance FROM transactions "
            "WHERE json_extract(payload, '$.fitid') = 'F002'").fetchone()[0]
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


def test_main_refuses_a_db_still_holding_pre_0002_ids(tmp_path):
    # Migration 0002 moved both ledgers onto content-derived ids and nothing
    # rewrites the rows already stored, so an incremental load would insert
    # every one of them a second time under its new id — silently doubling
    # the ledger every spending figure is computed from. --force is the
    # repair, so it must still succeed.
    root = tmp_path / "bronze"
    root.mkdir()
    _make_run(root, "20260801T120000Z")
    db = tmp_path / "chase.db"
    conn = sqlite3.connect(db)
    load.apply_migrations(conn, MIGRATIONS)
    _seed_export(conn, [(_epoch(2026, 7, 15), -42.50)])     # bare-FITID id
    conn.close()

    args = ["--bronze-dir", str(root), "--silver-db", str(db)]
    try:
        load.main(args)
        raise AssertionError("an incremental load must not proceed")
    except SystemExit as exc:
        assert "pre-0002" in str(exc) and "--force" in str(exc)
    assert load.main(args + ["--force"]) == 0


def _post_pass_calls(monkeypatch, *, failing=None):
    """Record which post passes ran, optionally making one raise."""
    ran = []

    def spy(name):
        def run(conn, bronze_dir):
            ran.append(name)
            if name == failing:
                raise RuntimeError("pdf tooling unavailable")
            return 0
        return run
    for name in ("load_statement_transactions", "load_card_statements",
                 "derive_card_balances"):
        monkeypatch.setattr(load, name, spy(name))
    return ran


def test_post_passes_rerun_after_one_of_them_failed(tmp_path, monkeypatch):
    # The three passes are tree-wide, and ingest has already committed each
    # run into `dump_runs` by the time they start. Gated on "this invocation
    # ingested something", a load that died inside one of them left silver
    # permanently without its statement backfill and its reconstructed card
    # balances, with nothing reporting it. The pending flag survives the
    # crash, so the next ordinary load runs all three again.
    root = tmp_path / "bronze"
    root.mkdir()
    _make_run(root, "20260801T120000Z")
    db = tmp_path / "chase.db"
    args = ["--bronze-dir", str(root), "--silver-db", str(db)]

    ran = _post_pass_calls(monkeypatch, failing="load_card_statements")
    try:
        load.main(args)
        raise AssertionError("the failing pass must propagate")
    except RuntimeError:
        pass
    assert ran == ["load_statement_transactions", "load_card_statements"]

    # Same bronze, nothing new to ingest — the passes still owe a run.
    ran = _post_pass_calls(monkeypatch)
    assert load.main(args) == 0
    assert ran == ["load_statement_transactions", "load_card_statements",
                   "derive_card_balances"]

    # …and once they finish, a no-op reload skips their PDF parsing again.
    ran = _post_pass_calls(monkeypatch)
    assert load.main(args) == 0
    assert ran == []


def _interrupt_when_the_first_run_returns(monkeypatch):
    """Let the first run land for real, then interrupt the instant it
    returns — the narrowest window there is between a run being committed
    and the loop moving on."""
    real = load.load_run

    def flaky(conn, run_dir):
        real(conn, run_dir)
        raise RuntimeError("interrupted")
    monkeypatch.setattr(load, "load_run", flaky)


def test_the_pending_flag_is_raised_by_the_run_that_lands(tmp_path,
                                                          monkeypatch):
    # `load_run` commits its own `dump_runs` row, so a flag written after it
    # returns — even one statement later — leaves a window: an interrupt in
    # that window strands the run permanently ingested with the tree-wide
    # passes never owed and nothing left able to notice. Raising the flag
    # INSIDE the run's own transaction closes it, and the interrupt that
    # proves it is the one on the very first run to land: there is no earlier
    # moment at which anything is committed.
    root = tmp_path / "bronze"
    root.mkdir()
    _make_run(root, "20260801T120000Z")
    _make_run(root, "20260901T120000Z")
    db = tmp_path / "chase.db"
    args = ["--bronze-dir", str(root), "--silver-db", str(db)]

    _interrupt_when_the_first_run_returns(monkeypatch)
    try:
        load.main(args)
        raise AssertionError("the interrupt must propagate")
    except RuntimeError:
        pass

    conn = sqlite3.connect(db)
    assert conn.execute("SELECT COUNT(*) FROM dump_runs").fetchone()[0] == 1
    assert load._post_passes_pending(conn) is True
    conn.close()

    # The next ordinary load ingests the second run and finishes the passes.
    monkeypatch.undo()
    ran = _post_pass_calls(monkeypatch)
    assert load.main(args) == 0
    assert len(ran) == 3
    assert load._post_passes_pending(sqlite3.connect(db)) is False


# ============================================================
# Card exports — parse, route, join
# ============================================================

def test_parse_card_csv_rows():
    rows = load.parse_card_csv(CARD_CSV)
    assert len(rows) == CARD_ROWS
    assert rows[0]["posted_at"] == _epoch(2026, 7, 18)      # Post Date
    assert rows[0]["txn_date"] == _epoch(2026, 7, 17)       # Transaction Date
    assert rows[0]["kind"] == "Payment"
    assert rows[0]["category"] is None            # empty on payments only
    assert rows[0]["amount"] == 240.00            # reduces the balance owed
    assert rows[1]["category"] == "Shopping"
    assert rows[1]["amount"] == -19.99            # spend is negative
    assert all(r["memo"] is None for r in rows)


def test_a_re_cased_csv_header_parses_identically():
    # Re-casing a column renames nothing, but an exact-match lookup reads it
    # as absent and yields a parse with zero rows — which on a card silently
    # drops every row out of gold's spending population. Lookups are
    # casefolded, so both spellings parse the same.
    recased = CARD_CSV.replace("Post Date", "Post date") \
                      .replace("Transaction Date", "Transaction date") \
                      .replace("Category", "CATEGORY")
    assert load.parse_card_csv(recased) == load.parse_card_csv(CARD_CSV)
    assert load.parse_csv(CSV.replace("Posting Date", "POSTING DATE")) == \
        load.parse_csv(CSV)


def test_a_missing_date_column_yields_no_rows_and_says_so(caplog):
    # A genuinely absent post-date column is drift the parser cannot absorb.
    # It is reported and yields nothing, rather than raised on: this loader
    # walks a whole bronze tree unattended, so one drifted export must not
    # abort every other account's and every other run's load.
    with caplog.at_level("WARNING"):
        assert load.parse_card_csv(
            CARD_CSV.replace("Post Date", "Settled")) == []
        assert load.parse_csv(CSV.replace("Posting Date", "Settled")) == []
    assert [r.getMessage() for r in caplog.records] == [
        "unexpected card CSV header: no 'Post Date' column; 0 rows",
        "unexpected deposit CSV header: no 'Posting Date' column; 0 rows",
    ]
    # An empty file is not drift, and neither is a header with no data rows.
    assert load.parse_csv("") == [] and load.parse_card_csv("") == []


def test_only_a_csv_with_data_lines_counts_as_drift():
    assert load._csv_yielded_nothing("", []) is False
    assert load._csv_yielded_nothing(CSV_HEADER, []) is False     # no activity
    assert load._csv_yielded_nothing(CSV, []) is True
    assert load._csv_yielded_nothing(CSV, load.parse_csv(CSV)) is False


def test_load_run_names_the_account_whose_csv_parsed_to_nothing(
        tmp_path, caplog):
    run = _make_run(tmp_path, "20260801T120000Z", card=True)
    (run / "transactions" / f"{CARD_EXT}.csv").write_text(
        CARD_CSV.replace("Post Date", "Settled"))
    conn = _conn()
    with caplog.at_level("WARNING"):
        assert load.load_run(conn, run) is True
    assert any(CARD_EXT in r.getMessage() and "date format has drifted" in
               r.getMessage() for r in caplog.records)
    # The QFX still lands the rows, without what only the CSV carries.
    assert conn.execute(
        "SELECT COUNT(*) FROM transactions WHERE account_external_id=? "
        "AND category IS NOT NULL", (CARD_EXT,)).fetchone()[0] == 0


def test_card_export_is_routed_off_the_files_not_the_roster():
    assert load.is_card_export(CARD_QFX, CARD_CSV) is True
    assert load.is_card_export("", CARD_CSV) is True        # CSV header alone
    assert load.is_card_export(CARD_QFX, "") is True        # message set alone
    assert load.is_card_export(QFX, CSV) is False
    assert load.is_card_export("", "") is False


def test_merge_card_joins_csv_fields_onto_qfx_rows():
    merged = load.merge_card_transactions(
        CARD_EXT, load.parse_qfx(CARD_QFX), load.parse_card_csv(CARD_CSV))
    assert len(merged) == CARD_ROWS
    payment = merged[0]
    # QFX supplies the descriptor of record; CSV the date, category and type.
    assert payment["kind"] == "Payment"           # not the QFX 'CREDIT'
    assert payment["txn_date"] == _epoch(2026, 7, 17)
    assert payment["category"] is None
    assert payment["source"] == "qfx"
    sale = merged[1]
    assert sale["merchant"] == "EXAMPLE MERCHANT, INC"      # commas intact
    assert sale["description"] == sale["merchant"]
    assert sale["category"] == "Shopping"
    assert sale["amount"] == -19.99               # provider-verbatim sign
    # No running balance exists on either card export, and no currency.
    assert all(m["balance"] is None and m["currency"] is None for m in merged)
    assert all(m["check_number"] is None for m in merged)
    # The lossy CSV descriptor is preserved rather than dropped.
    assert json.loads(json.dumps(sale["payload"]))["csv_description"] == \
        "EXAMPLE MERCHANT  INC"


def test_merge_card_keeps_a_qfx_only_row():
    merged = load.merge_card_transactions(
        CARD_EXT, load.parse_qfx(CARD_QFX), [])
    assert len(merged) == CARD_ROWS
    # With no CSV to join, the 5-way type falls back to the QFX TRNTYPE.
    assert {m["kind"] for m in merged} == {"DEBIT", "CREDIT"}
    assert all(m["txn_date"] is None and m["category"] is None for m in merged)


def test_merge_card_keeps_a_csv_only_row():
    merged = load.merge_card_transactions(
        CARD_EXT, [], load.parse_card_csv(CARD_CSV))
    assert len(merged) == CARD_ROWS
    assert all(m["source"] == "csv" for m in merged)
    assert {m["kind"] for m in merged} == {"Payment", "Sale", "Adjustment"}


# ============================================================
# Card transaction ids — namespaced + occurrence-indexed
# ============================================================

def test_card_ids_are_namespaced_by_product_and_account():
    merged = load.merge_card_transactions(
        CARD_EXT, load.parse_qfx(CARD_QFX), load.parse_card_csv(CARD_CSV))
    assert all(m["fitid"].startswith(f"card:{CARD_EXT}:") for m in merged)
    # The provider FITID never appears in the id, only in the payload.
    assert not any(m["payload"]["fitid"] in m["fitid"] for m in merged)


def test_card_duplicate_fitid_keeps_both_legs():
    # A same-day credit reuses the FITID of the charge it offsets: 5 rows,
    # 4 distinct FITIDs. Keying on the bare FITID would drop a leg and inflate
    # net spend — a content key never collapses two rows that differ in it.
    qfx = load.parse_qfx(CARD_QFX)
    assert len({q["fitid"] for q in qfx}) == CARD_ROWS - 1
    merged = load.merge_card_transactions(CARD_EXT, qfx,
                                          load.parse_card_csv(CARD_CSV))
    assert len({m["fitid"] for m in merged}) == CARD_ROWS
    # The FITID is carried for traceability, never as the identity — both legs
    # of the reversal report the same one and still have distinct ids.
    legs = [m for m in merged if m["payload"]["fitid"] == "C003"]
    assert len(legs) == 2 and legs[0]["fitid"] != legs[1]["fitid"]
    assert all("C003" not in m["fitid"] for m in legs)
    # Both legs reach silver, so the pair still nets to zero.
    conn = _conn()
    for tx in merged:
        load._insert_transaction(conn, CARD_EXT, tx)
    assert conn.execute("SELECT COUNT(*) FROM transactions").fetchone()[0] == \
        CARD_ROWS
    assert conn.execute(
        "SELECT ROUND(SUM(amount), 2) FROM transactions "
        "WHERE description = 'EXAMPLE CAFE'").fetchone()[0] == 0.0


def test_card_content_collision_gets_an_occurrence_index():
    # Two rows identical in (post date, amount, descriptor) share a content
    # key; the occurrence index separates them.
    merged = load.merge_card_transactions(
        CARD_EXT, [], load.parse_card_csv(CARD_CSV))
    ids = [m["fitid"] for m in merged]
    assert len(set(ids)) == CARD_ROWS
    key = load.card_content_key(CARD_EXT, _epoch(2026, 7, 16), -19.99,
                                "EXAMPLE MERCHANT  INC")
    assert [load._txn_id(load.PRODUCT_CARD, CARD_EXT, key, 0),
            load._txn_id(load.PRODUCT_CARD, CARD_EXT, key, 1)] == ids[1:3]


def test_card_descriptor_key_is_the_form_both_exports_agree_on():
    # The QFX keeps a descriptor's commas, the CSV replaces them with spaces.
    # The key must hash the form both produce, or the id would depend on which
    # file arrived.
    assert load.card_descriptor_key("EXAMPLE MERCHANT, INC") == \
        load.card_descriptor_key("EXAMPLE MERCHANT  INC")
    assert load.card_descriptor_key(None) == ""


def test_card_ids_do_not_depend_on_which_format_arrived():
    # Each format is fetched separately and can fail on its own, so the same
    # rows can arrive CSV-only, QFX-only, or as a pair. All three must key
    # identically — otherwise a later complete run re-inserts every row under
    # a second id and permanently doubles the card's ledger.
    qfx, csv = load.parse_qfx(CARD_QFX), load.parse_card_csv(CARD_CSV)
    pair = {m["fitid"] for m in load.merge_card_transactions(CARD_EXT, qfx, csv)}
    csv_only = {m["fitid"] for m in load.merge_card_transactions(CARD_EXT, [], csv)}
    qfx_only = {m["fitid"] for m in load.merge_card_transactions(CARD_EXT, qfx, [])}
    assert pair == csv_only == qfx_only
    assert len(pair) == CARD_ROWS


def test_card_ids_converge_when_the_qfx_covers_only_some_rows():
    # A partial QFX splits a colliding content key across the merge's two
    # passes (QFX-matched first, then the CSV leftovers). The occurrence
    # counter spans both, so the id set is still exactly the CSV-only one.
    csv = load.parse_card_csv(CARD_CSV)
    partial = load.parse_qfx(
        "<STMTTRN><TRNTYPE>DEBIT<DTPOSTED>20260716<TRNAMT>-19.99<FITID>C002"
        "<NAME>EXAMPLE MERCHANT, INC</STMTTRN>"
        "<STMTTRN><TRNTYPE>CREDIT<DTPOSTED>20260718<TRNAMT>240.00<FITID>CP01"
        "<NAME>EXAMPLE PAYMENT THANK YOU</STMTTRN>")
    assert {m["fitid"] for m in
            load.merge_card_transactions(CARD_EXT, partial, csv)} == \
        {m["fitid"] for m in load.merge_card_transactions(CARD_EXT, [], csv)}


def test_card_ids_are_stable_across_reparses(tmp_path):
    # Re-PARSE from files rather than re-calling the merge on one parsed list:
    # only a fresh parse can catch an id that depends on parse order or on
    # object identity rather than on content.
    (tmp_path / "c.qfx").write_text(CARD_QFX)
    (tmp_path / "c.csv").write_text(CARD_CSV)

    def _ids(ext):
        return [m["fitid"] for m in load.merge_card_transactions(
            ext,
            load.parse_qfx((tmp_path / "c.qfx").read_text()),
            load.parse_card_csv((tmp_path / "c.csv").read_text()))]

    assert _ids(CARD_EXT) == _ids(CARD_EXT)
    # …and are namespaced per account, so two cards never share an id.
    assert not set(_ids(CARD_EXT)) & set(_ids(EXT2))


# ============================================================
# Card records in a full run
# ============================================================

def test_load_run_ingests_cards_alongside_deposits(tmp_path):
    run = _make_run(tmp_path, "20260801T120000Z", card=True)
    conn = _conn()
    assert load.load_run(conn, run) is True
    assert dict(conn.execute(
        "SELECT account_external_id, product FROM accounts")) == \
        {EXT: "dda", CARD_EXT: "card"}
    # A card's balance is stored provider-verbatim: the POSITIVE amount owed.
    assert conn.execute("SELECT balance, pending_charges FROM accounts "
                        "WHERE account_external_id=?",
                        (CARD_EXT,)).fetchone() == (431.00, 12.25)
    assert dict(conn.execute(
        "SELECT account_external_id, COUNT(*) FROM transactions "
        "GROUP BY account_external_id")) == {EXT: 4, CARD_EXT: CARD_ROWS}
    # Both products' statements are inventoried.
    assert {r[0] for r in conn.execute(
        "SELECT account_external_id FROM documents")} == {EXT, CARD_EXT}
    # Card-only columns are populated for cards and NULL for deposits.
    assert conn.execute(
        "SELECT COUNT(*) FROM transactions WHERE account_external_id=? "
        "AND (txn_date IS NOT NULL OR merchant IS NOT NULL)",
        (EXT,)).fetchone()[0] == 0


def test_load_run_with_cards_is_idempotent(tmp_path):
    run = _make_run(tmp_path, "20260801T120000Z", card=True)
    conn = _conn()
    assert load.load_run(conn, run) is True
    n = conn.execute("SELECT COUNT(*) FROM transactions").fetchone()[0]
    # A second bronze run with identical exports must converge, not duplicate.
    run2 = _make_run(tmp_path, "20260901T120000Z", card=True)
    assert load.load_run(conn, run2) is True
    assert conn.execute("SELECT COUNT(*) FROM transactions").fetchone()[0] == n


def test_record_without_product_key_loads_as_deposit(tmp_path):
    # Bronze runs predating the card work carry no `product` key at all.
    assert load.account_product({}) == load.PRODUCT_DEPOSIT
    assert load.card_account_ids([{"account_external_id": EXT}]) == set()
    run = _make_run(tmp_path, "20260801T120000Z")
    assert "product" not in json.loads((run / "accounts.json").read_text())[0]
    conn = _conn()
    assert load.load_run(conn, run) is True
    assert conn.execute("SELECT COUNT(*) FROM accounts").fetchone()[0] == 1
    assert conn.execute("SELECT product FROM accounts").fetchone()[0] == "dda"
    assert conn.execute("SELECT COUNT(*) FROM transactions").fetchone()[0] == 4


def test_statement_pass_never_parses_a_card_statement(tmp_path, monkeypatch):
    # The statement pass is deposit-shaped; a card's PDFs are inventoried but
    # not parsed here, even once the card has export rows to seam against.
    conn = _conn()
    _seed_export_balance(conn, _epoch(2024, 8, 11), -10.0, 1400.0)
    _seed_export_balance(conn, _epoch(2024, 8, 11), -10.0, 900.0, ext=CARD_EXT)
    run = tmp_path / "20260801T000000Z"
    (run / "statements" / EXT).mkdir(parents=True)
    (run / "statements" / CARD_EXT).mkdir(parents=True)
    (run / "accounts.json").write_text(json.dumps([
        {"account_external_id": EXT, "product": "dda"},
        {"account_external_id": CARD_EXT, "product": "card"},
    ]))
    (run / "statements" / EXT / "a.pdf").write_bytes(b"%PDF fake a")
    (run / "statements" / CARD_EXT / "b.pdf").write_bytes(b"%PDF fake b")
    parsed_names = []

    def fake_parse(path):
        parsed_names.append(path.name)
        return _multi(date(2024, 7, 20), date(2024, 8, 19), [
            _seg("1310.00", "1400.00", [(date(2024, 7, 25), "90.00", "deposit")])])

    monkeypatch.setattr(load.statement_parser, "parse_statement_pdf", fake_parse)
    assert load.load_statement_transactions(conn, tmp_path) == 1
    assert parsed_names == ["a.pdf"]      # the card's PDF is never opened


def test_statement_routing_survives_an_unreadable_roster(tmp_path, monkeypatch):
    # Exports are routed by what the FILES say; the statement pass has to
    # agree, or a run whose accounts.json is missing or corrupt feeds a card's
    # PDFs to the deposit parser (different period line, different signs).
    conn = _conn()
    _seed_export_balance(conn, _epoch(2024, 8, 11), -10.0, 1400.0)
    _seed_export_balance(conn, _epoch(2024, 8, 11), -10.0, 900.0, ext=CARD_EXT)
    run = tmp_path / "20260801T000000Z"
    (run / "transactions").mkdir(parents=True)
    (run / "statements" / EXT).mkdir(parents=True)
    (run / "statements" / CARD_EXT).mkdir(parents=True)
    (run / "accounts.json").write_text("{ truncated")      # unreadable roster
    (run / "transactions" / f"{EXT}.qfx").write_text(QFX)
    (run / "transactions" / f"{CARD_EXT}.qfx").write_text(CARD_QFX)
    (run / "statements" / EXT / "a.pdf").write_bytes(b"%PDF fake a")
    (run / "statements" / CARD_EXT / "b.pdf").write_bytes(b"%PDF fake b")
    assert load.card_account_ids_in_tree(tmp_path) == {CARD_EXT}
    parsed_names = []

    def fake_parse(path):
        parsed_names.append(path.name)
        return _multi(date(2024, 7, 20), date(2024, 8, 19), [
            _seg("1310.00", "1400.00", [(date(2024, 7, 25), "90.00", "deposit")])])

    monkeypatch.setattr(load.statement_parser, "parse_statement_pdf", fake_parse)
    assert load.load_statement_transactions(conn, tmp_path) == 1
    assert parsed_names == ["a.pdf"]


def test_the_two_card_signals_are_separable(tmp_path):
    # The ROUTING set unions the two signals (mis-routing a statement costs a
    # parse, so either one is enough); the DESTRUCTIVE pass intersects them.
    # That only works if the file-content signal is available on its own.
    run = tmp_path / "20260801T000000Z"
    (run / "transactions").mkdir(parents=True)
    (run / "transactions" / f"{CARD_EXT}.qfx").write_text(CARD_QFX)
    (run / "transactions" / f"{EXT}.qfx").write_text(QFX)
    # a third account the roster calls a card with no export file present
    (run / "accounts.json").write_text(json.dumps(
        [{"account_external_id": EXT2, "product": "card"}]))
    assert load.card_export_ids_in_tree(tmp_path) == {CARD_EXT}
    assert load.card_account_ids_in_tree(tmp_path) == {CARD_EXT, EXT2}


# ============================================================
# A partial dump is ingested, never withheld
# ============================================================
#
# Silver ingest is additive, so closure lives in gold; these pin the two
# things a silver-side coverage gate got wrong when it existed.

_PARTIAL_COVERAGE = {"dda": {"accounts": 1, "exported": 1, "statements": 0,
                             "complete": False},
                     "card": {"accounts": 2, "exported": 1, "statements": 1,
                              "complete": False}}


def test_a_failed_statement_pass_still_publishes_the_balances(tmp_path):
    # Coverage is composite — it also requires the statement pass — but a
    # balance comes from the ROSTER. A flaky statement PDF must therefore not
    # withhold the accounts snapshot, or gold projects the previous run's
    # balance indefinitely.
    run = _make_run(tmp_path, "20260801T120000Z", card=True,
                    coverage=_PARTIAL_COVERAGE)
    conn = _conn()
    assert load.load_run(conn, run) is True
    assert dict(conn.execute(
        "SELECT account_external_id, balance FROM accounts")) == \
        {EXT: 2257.50, CARD_EXT: 431.00}


def test_a_products_rows_never_land_without_its_roster_row(tmp_path):
    # The gold adapter classifies a transaction by looking its account's
    # product up in `accounts`. A snapshot withheld while the same product's
    # rows were admitted therefore filed card spend into the deposit ledger —
    # so every account with rows must also have a roster row.
    run = _make_run(tmp_path, "20260801T120000Z", card=True,
                    coverage=_PARTIAL_COVERAGE)
    conn = _conn()
    assert load.load_run(conn, run) is True
    assert conn.execute(
        "SELECT COUNT(*) FROM transactions t WHERE NOT EXISTS "
        "(SELECT 1 FROM accounts a "
        " WHERE a.account_external_id = t.account_external_id)").fetchone()[0] == 0
    assert dict(conn.execute(
        "SELECT account_external_id, product FROM accounts")) == \
        {EXT: "dda", CARD_EXT: "card"}


def test_coverage_block_does_not_change_what_is_ingested(tmp_path):
    # Whatever run.json scores, silver ingests the same rows: the flag is
    # gold's input, not a silver gate.
    conn_partial, conn_none = _conn(), _conn()
    load.load_run(conn_partial, _make_run(tmp_path / "a", "20260801T120000Z",
                                          card=True,
                                          coverage=_PARTIAL_COVERAGE))
    load.load_run(conn_none, _make_run(tmp_path / "b", "20260801T120000Z",
                                       card=True))
    def _dump(c):
        return (sorted(c.execute("SELECT account_external_id, product, balance "
                                 "FROM accounts")),
                sorted(c.execute("SELECT fitid FROM transactions")))
    assert _dump(conn_partial) == _dump(conn_none)


def test_card_landing_csv_only_converges_when_the_pair_arrives(tmp_path):
    # Each export format is fetched separately and can fail on its own, so a
    # CSV-only card run is reachable. The next complete run must CONVERGE on
    # the same ids — a format-dependent key re-inserted every row under a
    # second id and permanently doubled the card's ledger and its net spend.
    conn = _conn()
    assert load.load_run(conn, _make_run(tmp_path, "20260801T120000Z",
                                         card=True, card_formats=("csv",))) is True
    before = sorted(r[0] for r in conn.execute(
        "SELECT fitid FROM transactions WHERE account_external_id=?",
        (CARD_EXT,)))
    net = conn.execute("SELECT ROUND(SUM(amount), 2) FROM transactions "
                       "WHERE account_external_id=?", (CARD_EXT,)).fetchone()[0]
    assert len(before) == CARD_ROWS

    assert load.load_run(conn, _make_run(tmp_path, "20260901T120000Z",
                                         card=True)) is True
    after = sorted(r[0] for r in conn.execute(
        "SELECT fitid FROM transactions WHERE account_external_id=?",
        (CARD_EXT,)))
    assert after == before                              # identical ids
    assert conn.execute("SELECT ROUND(SUM(amount), 2) FROM transactions "
                        "WHERE account_external_id=?",
                        (CARD_EXT,)).fetchone()[0] == net


def _card_kinds(conn):
    """The card ledger's kinds, counted — what a `Type` drift would flatten."""
    out = {}
    for (kind,) in conn.execute(
            "SELECT kind FROM transactions WHERE account_external_id=?",
            (CARD_EXT,)):
        out[kind] = out.get(kind, 0) + 1
    return out


def _card_row(conn, merchant_like):
    return conn.execute(
        "SELECT merchant, category, txn_date, kind, source FROM transactions "
        "WHERE account_external_id=? AND merchant LIKE ?",
        (CARD_EXT, merchant_like)).fetchone()


def test_a_card_row_landed_csv_only_is_repaired_by_the_pair(tmp_path):
    # Converging on the id is not converging on the CONTENT: the second load
    # re-mints an id silver already holds, so a plain INSERT OR IGNORE kept
    # the CSV's comma-mangled descriptor as the merchant forever and only
    # `load --force` undid it. The QFX is authoritative for the descriptor,
    # so the pair repairs the stored row in place.
    conn = _conn()
    load.load_run(conn, _make_run(tmp_path, "20260801T120000Z", card=True,
                                  card_formats=("csv",)))
    assert _card_row(conn, "EXAMPLE MERCHANT%")[0] == "EXAMPLE MERCHANT  INC"

    load.load_run(conn, _make_run(tmp_path, "20260901T120000Z", card=True))
    merchant, category, txn_date, kind, source = \
        _card_row(conn, "EXAMPLE MERCHANT%")
    assert merchant == "EXAMPLE MERCHANT, INC"          # the QFX spelling
    assert (category, kind, source) == ("Shopping", "Sale", "qfx")
    assert txn_date == _epoch(2026, 7, 15)
    assert conn.execute("SELECT COUNT(*) FROM transactions "
                        "WHERE account_external_id=?",
                        (CARD_EXT,)).fetchone()[0] == CARD_ROWS


def test_a_card_row_landed_qfx_only_is_repaired_by_the_pair(tmp_path):
    # The mirror failure: without the CSV a card row carries no provider
    # category, no transaction date, and the QFX's two-way TRNTYPE as its
    # kind — which the gold adapter maps to `other`, which the spending
    # population excludes. Every one of those is the CSV's to supply.
    conn = _conn()
    load.load_run(conn, _make_run(tmp_path, "20260801T120000Z", card=True,
                                  card_formats=("qfx",)))
    merchant, category, txn_date, kind, _ = _card_row(conn, "EXAMPLE CAFE")
    assert merchant == "EXAMPLE CAFE"
    assert (category, txn_date, kind) == (None, None, "DEBIT")

    load.load_run(conn, _make_run(tmp_path, "20260901T120000Z", card=True))
    merchant, category, txn_date, kind, _ = _card_row(conn, "EXAMPLE CAFE")
    assert merchant == "EXAMPLE CAFE"
    assert category == "Food & Drink"
    assert txn_date == _epoch(2026, 7, 14)
    assert kind == "Sale"                       # the 5-way Type, not 'DEBIT'
    assert conn.execute("SELECT COUNT(*) FROM transactions "
                        "WHERE account_external_id=?",
                        (CARD_EXT,)).fetchone()[0] == CARD_ROWS


def test_repairing_a_card_row_keeps_its_derived_balance(tmp_path):
    # `derive_card_balances` stores its marker inside `payload`, so the
    # repair merges the payload key by key instead of replacing the blob —
    # and never touches `balance`, which no card export carries.
    conn = _conn()
    load.load_run(conn, _make_run(tmp_path, "20260801T120000Z", card=True,
                                  card_formats=("qfx",)))
    fitid = conn.execute(
        "SELECT fitid FROM transactions WHERE account_external_id=? "
        "AND merchant='EXAMPLE CAFE' LIMIT 1", (CARD_EXT,)).fetchone()[0]
    load._apply_derived_balances(conn, {fitid: 12345})

    load.load_run(conn, _make_run(tmp_path, "20260901T120000Z", card=True))
    balance, payload = conn.execute(
        "SELECT balance, payload FROM transactions WHERE fitid=?",
        (fitid,)).fetchone()
    assert balance == 123.45
    assert json.loads(payload)["balance_basis"] == "derived"
    assert json.loads(payload)["category"] == "Food & Drink"


def test_a_renamed_csv_column_never_blanks_what_is_already_stored(tmp_path):
    # A drift that renames ONE column still parses every row, so neither the
    # drift warning nor the per-run drift count can see it. A writer that
    # adopted its whole authoritative set unconditionally would then NULL the
    # very columns gold's provider tier reads, moving a whole card onto the
    # paid model backlog. The repair is additive: an absent value states
    # nothing, so it never wins over a stored one.
    conn = _conn()
    load.load_run(conn, _make_run(tmp_path, "20260801T120000Z", card=True))
    assert _card_row(conn, "EXAMPLE CAFE")[1] == "Food & Drink"

    run = _make_run(tmp_path, "20260901T120000Z", card=True)
    (run / "transactions" / f"{CARD_EXT}.csv").write_text(
        CARD_CSV.replace(",Category,", ",Grouping,"))
    assert load.load_run(conn, run) is True
    assert _card_row(conn, "EXAMPLE CAFE")[1] == "Food & Drink"

    # The transaction date is the same story in the other column.
    run = _make_run(tmp_path, "20261001T120000Z", card=True)
    (run / "transactions" / f"{CARD_EXT}.csv").write_text(
        CARD_CSV.replace("Transaction Date,", "Trade Date,"))
    assert load.load_run(conn, run) is True
    assert _card_row(conn, "EXAMPLE CAFE")[2] == _epoch(2026, 7, 14)


def test_a_blanked_category_cell_never_blanks_the_stored_one(tmp_path):
    # The same hazard with no header drift at all: the export simply
    # re-issues a row with its `Category` cell empty.
    conn = _conn()
    load.load_run(conn, _make_run(tmp_path, "20260801T120000Z", card=True))
    run = _make_run(tmp_path, "20260901T120000Z", card=True)
    (run / "transactions" / f"{CARD_EXT}.csv").write_text(
        CARD_CSV.replace("EXAMPLE CAFE,Food & Drink,Sale",
                         "EXAMPLE CAFE,,Sale"))
    assert load.load_run(conn, run) is True
    assert _card_row(conn, "EXAMPLE CAFE")[1] == "Food & Drink"


def test_a_card_csv_row_with_a_blank_type_still_repairs_its_columns(tmp_path):
    # Provenance is STATED by the join, never sniffed off a payload cell the
    # export may leave blank: reading it off `Type` refused to adopt the
    # category and transaction date the very same row did carry, leaving the
    # column NULL while the payload blob held the value.
    conn = _conn()
    load.load_run(conn, _make_run(tmp_path, "20260801T120000Z", card=True,
                                  card_formats=("qfx",)))
    assert _card_row(conn, "EXAMPLE CAFE")[1] is None

    run = _make_run(tmp_path, "20260901T120000Z", card=True)
    (run / "transactions" / f"{CARD_EXT}.csv").write_text(
        CARD_CSV.replace("EXAMPLE CAFE,Food & Drink,Sale",
                         "EXAMPLE CAFE,Food & Drink,"))
    assert load.load_run(conn, run) is True
    _, category, txn_date, kind, _ = _card_row(conn, "EXAMPLE CAFE")
    assert category == "Food & Drink"
    assert txn_date == _epoch(2026, 7, 14)
    assert kind == "DEBIT"          # no `Type` stated; the QFX TRNTYPE stands


def test_a_renamed_type_column_never_overwrites_the_stored_kinds(tmp_path):
    # `kind` is CSV-authoritative, but the merged row falls back to the QFX
    # two-way TRNTYPE when the CSV `Type` cell is absent, and the fallback is
    # indistinguishable from a stated value by the time the writer sees it. A
    # drift that renames ONLY `Type` still parses every row, so no drift
    # signal fires — and the whole card's 5-way kinds would silently become
    # DEBIT/CREDIT, which the gold adapter maps to `other` and drops out of
    # the spending base. The stated cell in the payload is what says whether
    # the CSV had a kind to offer.
    conn = _conn()
    load.load_run(conn, _make_run(tmp_path, "20260801T120000Z", card=True))
    before = _card_kinds(conn)
    assert before == {"Adjustment": 1, "Payment": 1, "Sale": 3}

    run = _make_run(tmp_path, "20260901T120000Z", card=True)
    (run / "transactions" / f"{CARD_EXT}.csv").write_text(
        CARD_CSV.replace(",Type,", ",Classification,"))
    assert load.load_run(conn, run) is True
    assert _card_kinds(conn) == before


def test_a_blanked_type_cell_never_overwrites_the_stored_kind(tmp_path):
    # The same hazard with no header drift at all: one row re-issued with an
    # empty `Type` cell. The row is still CSV-backed and still repairs the
    # columns it does carry.
    conn = _conn()
    load.load_run(conn, _make_run(tmp_path, "20260801T120000Z", card=True))
    run = _make_run(tmp_path, "20260901T120000Z", card=True)
    (run / "transactions" / f"{CARD_EXT}.csv").write_text(
        CARD_CSV.replace("EXAMPLE CAFE,Food & Drink,Sale",
                         "EXAMPLE CAFE,Coffee,"))
    assert load.load_run(conn, run) is True
    _, category, _, kind, _ = _card_row(conn, "EXAMPLE CAFE")
    assert kind == "Sale"           # no `Type` stated; the stored kind stands
    assert category == "Coffee"     # the columns it did state still repair


def test_deposit_landing_csv_only_converges_when_the_pair_arrives(tmp_path):
    # The same reachable failure on the deposit ledger: the downloader catches
    # each format's failure on its own, so a CSV-only deposit run happens. With
    # the QFX FITID as the identity, the next complete run re-inserted every
    # posted row under a second id — the ledger that feeds the spending base
    # doubled and never converged.
    conn = _conn()
    assert load.load_run(conn, _make_run(tmp_path, "20260801T120000Z",
                                         deposit_formats=("csv",))) is True
    before = sorted(r[0] for r in conn.execute(
        "SELECT fitid FROM transactions WHERE account_external_id=?", (EXT,)))
    net = conn.execute("SELECT ROUND(SUM(amount), 2) FROM transactions "
                       "WHERE account_external_id=?", (EXT,)).fetchone()[0]
    assert len(before) == 4                    # 3 posted + the pending row

    assert load.load_run(conn, _make_run(tmp_path, "20260901T120000Z")) is True
    assert sorted(r[0] for r in conn.execute(
        "SELECT fitid FROM transactions WHERE account_external_id=?",
        (EXT,))) == before                     # identical ids, no new rows
    assert conn.execute("SELECT ROUND(SUM(amount), 2) FROM transactions "
                        "WHERE account_external_id=?", (EXT,)).fetchone()[0] == net


# ============================================================
# Statement-PDF transactions — seam + import
# ============================================================

def _seed_export(conn, rows, ext=EXT):
    for posted, amt in rows:
        load._insert_transaction(conn, ext, {
            "fitid": f"F{ext}_{posted}_{amt}", "posted_at": posted,
            "amount": amt, "kind": None, "description": "x",
            "check_number": None, "balance": None, "source": "qfx",
            "payload": {}})
    conn.commit()


def test_export_seam_anchors_to_export_rows_only():
    conn = _conn()
    e1, e2 = _epoch(2024, 8, 15), _epoch(2026, 1, 1)
    _seed_export(conn, [(e1, -10.0), (e2, 20.0)])
    assert load._export_seams(conn) == {EXT: e1}
    # an earlier STATEMENT-sourced row must not drag the seam down.
    load._insert_transaction(conn, EXT, {
        "fitid": "stmt_x", "posted_at": _epoch(2020, 1, 1), "amount": 5.0,
        "kind": None, "description": None, "check_number": None,
        "balance": None, "source": "statement", "payload": {}})
    conn.commit()
    assert load._export_seams(conn) == {EXT: e1}


def test_export_seam_empty_without_export():
    assert load._export_seams(_conn()) == {}


def test_single_account_seam_equals_the_global_minimum():
    # A one-product ledger's per-account seam is exactly the old global
    # MIN(posted_at), so the deposit path is unaffected by the split.
    conn = _conn()
    e1, e2 = _epoch(2024, 8, 15), _epoch(2026, 1, 1)
    _seed_export(conn, [(e2, 20.0), (e1, -10.0)])
    global_min = conn.execute(
        "SELECT MIN(posted_at) FROM transactions "
        "WHERE source IN ('qfx','csv')").fetchone()[0]
    assert load._export_seams(conn) == {EXT: global_min}


def test_seams_are_independent_between_accounts():
    # Export depth differs by product and by onboarding: one global minimum
    # would gate the shallow account at the deep account's seam.
    conn = _conn()
    deep, shallow = _epoch(2020, 3, 1), _epoch(2025, 6, 1)
    _seed_export(conn, [(deep, -10.0)])
    _seed_export(conn, [(shallow, -20.0)], ext=EXT2)
    assert load._export_seams(conn) == {EXT: deep, EXT2: shallow}


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


def test_reconstructed_deposit_balances_are_marked_derived():
    # A deposit statement row's balance is ROLLED FORWARD from the segment's
    # printed opening figure — it is not a number the provider stated for
    # that row. Only the CSV export's own running-balance column is, and
    # "carries no marker" has to mean exactly that, or a consumer reading the
    # marker is wrong about most of the deposit ledger.
    conn = _conn()
    seg = _seg("100.00", "150.00", [(date(2024, 7, 20), "50.00", "deposit")])
    assert load._import_statement(conn, EXT, seg, None) == 1
    row = conn.execute("SELECT balance, payload FROM transactions").fetchone()
    assert row[0] == 150.0
    assert json.loads(row[1])["balance_basis"] == load.BALANCE_BASIS_DERIVED


def test_a_statement_row_with_no_balance_carries_no_basis():
    # A segment with no printed opening balance reconstructs nothing; an
    # absent balance must not be labelled as anything.
    conn = _conn()
    seg = sp.StatementSegment(
        beginning_balance=None, ending_balance=None,
        transactions=[sp.StatementTxn(date(2024, 7, 20), Decimal("50.00"), "d")])
    assert load._import_statement(conn, EXT, seg, None) == 1
    row = conn.execute("SELECT balance, payload FROM transactions").fetchone()
    assert row[0] is None
    assert "balance_basis" not in json.loads(row[1])


def test_a_statement_check_row_carries_its_number_into_silver():
    # Statements are the only source of a check older than the export window,
    # and the number goes in the column: gold reads that, not the narrative.
    # A payee-less check has no description at all, and an empty one is stored
    # as NULL like every other absent value.
    conn = _conn()
    seg = sp.StatementSegment(
        beginning_balance=Decimal("100.00"), ending_balance=Decimal("50.00"),
        transactions=[sp.StatementTxn(date(2024, 7, 20), Decimal("-50.00"), "",
                                      check_number="9042")])
    assert load._import_statement(conn, EXT, seg, None) == 1
    assert conn.execute(
        "SELECT check_number, description FROM transactions").fetchone() \
        == ("9042", None)


def test_the_csv_export_column_is_the_only_unmarked_balance():
    # The other side of the same invariant: the balances joined off the
    # deposit CSV's running-balance column are the provider's own, and are
    # the only ones that go in unmarked.
    rows = load.merge_transactions(EXT, load.parse_qfx(QFX), load.parse_csv(CSV))
    with_balance = [r for r in rows if r["balance"] is not None]
    assert len(with_balance) == 3
    assert all("balance_basis" not in r["payload"] for r in with_balance)


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


def _seed_export_balance(conn, posted, amount, balance, ext=EXT):
    load._insert_transaction(conn, ext, {
        "fitid": f"F{ext}_{posted}", "posted_at": posted, "amount": amount,
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


def test_an_account_the_statements_never_printed_does_not_warn(caplog):
    # The pool is relationship-wide, so a deposit account opened after the
    # statement era — or held on another relationship — chains against
    # statements it is printed on none of, on every load, forever. Nothing is
    # being given up there, so it must not read as a broken backfill; a chain
    # that anchors and then breaks still warns, because that one does drop
    # rows it could otherwise have imported.
    conn = _conn()
    newest = _multi(date(2024, 7, 20), date(2024, 8, 19),
                    [_seg("1300.00", "1400.00", [])])
    with caplog.at_level("WARNING"):
        assert load._chain_segments(conn, EXT, [newest]) == []
    assert caplog.records == []

    caplog.clear()
    _seed_export_balance(conn, _epoch(2024, 8, 12), -10.0, 1400.0)
    gap = _multi(date(2024, 6, 20), date(2024, 7, 19),
                 [_seg("1000.00", "1111.00", [])])
    with caplog.at_level("WARNING"):
        assert len(load._chain_segments(conn, EXT, [newest, gap])) == 1
    assert any("cannot attribute" in r.getMessage() for r in caplog.records)


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


def test_statement_import_gates_on_the_accounts_own_seam(tmp_path, monkeypatch):
    # One account's export reaches back to 2020, the other's only to 2025.
    # Under a single global MIN(posted_at) the shallow account's whole
    # statement history sits "after the seam" and is silently discarded.
    conn = _conn()
    _seed_export(conn, [(_epoch(2020, 3, 1), -10.0)])
    _seed_export_balance(conn, _epoch(2025, 2, 1), -10.0, 1400.0, ext=EXT2)
    run = tmp_path / "20260801T000000Z"
    (run / "statements" / EXT2).mkdir(parents=True)
    (run / "accounts.json").write_text(json.dumps([
        {"account_external_id": EXT, "product": "dda"},
        {"account_external_id": EXT2, "product": "dda"},
    ]))
    (run / "statements" / EXT2 / "2025-02-19.pdf").write_bytes(b"%PDF fake")
    monkeypatch.setattr(
        load.statement_parser, "parse_statement_pdf",
        lambda path: _multi(date(2025, 1, 20), date(2025, 2, 19), [
            _seg("1310.00", "1400.00",
                 [(date(2025, 1, 25), "90.00", "deposit")])]))
    assert load.load_statement_transactions(conn, tmp_path) == 1
    assert conn.execute(
        "SELECT account_external_id, posted_at FROM transactions "
        "WHERE source='statement'").fetchone() == (EXT2, _epoch(2025, 1, 25))


def test_statement_pass_imports_nothing_for_an_account_with_no_export(
        tmp_path, monkeypatch):
    # An account with no export rows has no seam, so nothing bounds its
    # statement era and the balance chain has nothing to anchor on. Its
    # directory's PDFs still join the relationship-wide pool — a combined
    # statement belongs to every deposit account printed on it — but no row
    # is ever attributed to the unseamed account.
    conn = _conn()
    _seed_export(conn, [(_epoch(2020, 3, 1), -10.0)])            # EXT only
    run = tmp_path / "20260801T000000Z"
    (run / "statements" / EXT2).mkdir(parents=True)
    (run / "accounts.json").write_text(json.dumps([
        {"account_external_id": EXT, "product": "dda"},
        {"account_external_id": EXT2, "product": "dda"},
    ]))
    (run / "statements" / EXT2 / "2019-02-19.pdf").write_bytes(b"%PDF fake")
    monkeypatch.setattr(
        load.statement_parser, "parse_statement_pdf",
        lambda path: _multi(date(2019, 1, 20), date(2019, 2, 19), [
            _seg("1310.00", "1400.00",
                 [(date(2019, 1, 25), "90.00", "deposit")])]))
    assert load.load_statement_transactions(conn, tmp_path) == 0
    assert conn.execute("SELECT COUNT(*) FROM transactions "
                        "WHERE source='statement'").fetchone()[0] == 0


def test_statement_pass_parses_nothing_without_a_deposit_export(
        tmp_path, monkeypatch):
    # No deposit account has an export row at all, so there is no seam to
    # bound any statement era: the PDFs are never opened, which is what keeps
    # a tree with no export off the whole poppler cost.
    conn = _conn()
    run = tmp_path / "20260801T000000Z"
    (run / "statements" / EXT).mkdir(parents=True)
    (run / "accounts.json").write_text(json.dumps(
        [{"account_external_id": EXT, "product": "dda"}]))
    (run / "statements" / EXT / "2019-02-19.pdf").write_bytes(b"%PDF fake")
    parsed = []
    monkeypatch.setattr(load.statement_parser, "parse_statement_pdf",
                        lambda path: parsed.append(path.name))
    assert load.load_statement_transactions(conn, tmp_path) == 0
    assert parsed == []


def test_one_relationship_statement_serves_two_deposit_accounts(
        tmp_path, monkeypatch):
    # The documents surface has no per-account selector, so a combined
    # statement is filed under ONE bucket directory while carrying a segment
    # for every deposit account on it. Attributing per directory gave the
    # second account nothing at all; the pass pools the statements and chains
    # each account against the same pool, so both import their own segment.
    conn = _conn()
    _seed_export_balance(conn, _epoch(2024, 8, 12), -10.0, 1400.0)
    _seed_export_balance(conn, _epoch(2024, 8, 12), -20.0, 330.0, ext=EXT2)
    run = tmp_path / "20260801T000000Z"
    (run / "statements" / EXT).mkdir(parents=True)         # the bucket
    (run / "accounts.json").write_text(json.dumps([
        {"account_external_id": EXT, "product": "dda"},
        {"account_external_id": EXT2, "product": "dda"},
    ]))
    (run / "statements" / EXT / "2024-08-19.pdf").write_bytes(b"%PDF fake")
    monkeypatch.setattr(
        load.statement_parser, "parse_statement_pdf",
        lambda path: _multi(date(2024, 7, 20), date(2024, 8, 19), [
            _seg("1300.00", "1400.00",
                 [(date(2024, 7, 25), "100.00", "first account")]),
            _seg("300.00", "330.00",
                 [(date(2024, 7, 26), "30.00", "second account")]),
        ]))
    assert load.load_statement_transactions(conn, tmp_path) == 2
    assert sorted(conn.execute(
        "SELECT account_external_id, description FROM transactions "
        "WHERE source='statement'")) == [(EXT, "first account"),
                                         (EXT2, "second account")]


# ============================================================
# Card statements — balances every era, transactions below the seam
# ============================================================

def _card_stmt(period_start, period_end, previous, rows=(), *, new=None,
               summary=None):
    """A synthetic parsed card statement.

    `rows` are (date, amount, description, kind) in the STATEMENT's own
    convention — a purchase or fee POSITIVE, a payment or credit NEGATIVE —
    which is the inverse of the export's. By default the summary figures are
    DERIVED from the rows, which is convenient but makes the fixture
    reconcile by construction: a gate deleted outright would still look
    green. `summary` states the figures outright instead (a dict of
    ParsedCardStatement field names to amount strings), so the fixture can
    disagree with its own rows; `new` is the shorthand for stating just the
    closing balance."""
    txns = [sp.StatementTxn(posted_at=d, amount=Decimal(a), description=desc,
                            kind=kind) for d, a, desc, kind in rows]

    def total(kind):
        return sum((t.amount for t in txns if t.kind == kind), Decimal(0))

    previous = Decimal(previous)
    figures = {
        "payments_credits": total("STMT_PAYMENT"),
        "purchases": total("STMT_PURCHASE"),
        "cash_advances": Decimal(0),
        "balance_transfers": Decimal(0),
        "fees_charged": total("STMT_FEE"),
        "interest_charged": total("STMT_INTEREST"),
        "new_balance": Decimal(new) if new is not None
        else previous + sum((t.amount for t in txns), Decimal(0)),
    }
    figures.update({k: Decimal(v) for k, v in (summary or {}).items()})
    return sp.ParsedCardStatement(
        period_start=period_start, period_end=period_end,
        previous_balance=previous, transactions=txns, **figures)


def _card_bronze(root: Path, statements: dict, *, slug="20260801T000000Z",
                 ext=CARD_EXT) -> Path:
    """A bronze tree whose only content is one card's statement PDFs, one per
    key of `statements` (the parse itself is monkeypatched, so the bytes only
    have to differ for the content-hash dedup). The roster stamps the account
    as a card so the pass routes its PDFs to the card parser."""
    run = root / slug
    (run / "statements" / ext).mkdir(parents=True)
    (run / "accounts.json").write_text(json.dumps(
        [{"account_external_id": ext, "product": "card"}]))
    for name in statements:
        (run / "statements" / ext / name).write_bytes(b"%PDF " + name.encode())
    return run


def _patch_card_parse(monkeypatch, statements: dict):
    monkeypatch.setattr(load.statement_parser, "parse_card_statement_pdf",
                        lambda path: statements[path.name])


def _seed_card_export(conn, rows, ext=CARD_EXT, description="x"):
    """Export-sourced card rows in silver's convention (spend negative)."""
    for posted, amount in rows:
        load._insert_transaction(conn, ext, {
            "fitid": load._txn_id(load.PRODUCT_CARD, ext,
                                  f"k{posted}_{amount}", 0),
            "posted_at": posted, "amount": amount, "kind": "Sale",
            "description": description, "check_number": None, "balance": None,
            "source": load.SOURCE_QFX, "payload": {"fitid": None}})
    conn.commit()


def _seed_card_roster(conn, *, balance, pending=None, ext=CARD_EXT,
                      snapshot_at=1_700_000_000):
    load._insert_account(conn, snapshot_at, {
        "account_external_id": ext, "product": "card", "account_type": "BAC",
        "nickname": "Example Card", "mask": "…5678", "currency": "USD",
        "balance": balance, "pending_charges_amount": pending})
    conn.commit()


def test_card_statement_balances_load_for_every_era(tmp_path, monkeypatch):
    # The period balances are what anchors the reconstruction, so they are
    # wanted for a period the export already covers just as much as for one
    # it does not; only the TRANSACTIONS stay behind the seam.
    conn = _conn()
    seam = _epoch(2026, 6, 1)
    _seed_card_export(conn, [(seam, -10.0)])
    stmts = {
        "old.pdf": _card_stmt(date(2026, 4, 2), date(2026, 5, 1), "100.00",
                              [(date(2026, 4, 10), "25.00", "a", "STMT_PURCHASE")]),
        "new.pdf": _card_stmt(date(2026, 6, 2), date(2026, 7, 1), "125.00",
                              [(date(2026, 6, 10), "40.00", "b", "STMT_PURCHASE")]),
    }
    _card_bronze(tmp_path, stmts)
    _patch_card_parse(monkeypatch, stmts)

    assert load.load_card_statements(conn, tmp_path) == 1
    assert conn.execute("SELECT COUNT(*) FROM statement_balances").fetchone()[0] == 2
    # both periods are covered: the older one by the rows just imported, the
    # newer one — which opens after the seam — by the export.
    assert conn.execute(
        "SELECT period_start, period_end, opening, closing, "
        "transactions_covered FROM statement_balances "
        "ORDER BY period_end").fetchall() == [
        (_epoch(2026, 4, 2), _epoch(2026, 5, 1), 100.0, 125.0, 1),
        (_epoch(2026, 6, 2), _epoch(2026, 7, 1), 125.0, 165.0, 1)]
    # only the pre-seam statement contributed a transaction
    assert conn.execute(
        "SELECT description FROM transactions WHERE source='statement'"
        ).fetchall() == [("a",)]


def test_card_statement_rows_carry_the_export_sign_convention(tmp_path,
                                                              monkeypatch):
    # The two sources describe an overlapping period in inverted conventions.
    # Silver keeps ONE — the export's — so a payment is positive and a
    # purchase negative whichever source the row came from.
    conn = _conn()
    seam = _epoch(2026, 6, 1)
    _seed_card_export(conn, [(seam, -19.99), (_epoch(2026, 6, 2), 240.00)])
    stmts = {"s.pdf": _card_stmt(
        date(2026, 4, 2), date(2026, 5, 1), "0.00", [
            (date(2026, 4, 10), "19.99", "EXAMPLE MERCHANT", "STMT_PURCHASE"),
            (date(2026, 4, 20), "-240.00", "EXAMPLE PAYMENT", "STMT_PAYMENT"),
        ])}
    _card_bronze(tmp_path, stmts)
    _patch_card_parse(monkeypatch, stmts)
    assert load.load_card_statements(conn, tmp_path) == 2

    signs = dict(conn.execute(
        "SELECT source, GROUP_CONCAT(amount) FROM ("
        "  SELECT source, amount FROM transactions ORDER BY amount) "
        "GROUP BY source"))
    assert signs[load.SOURCE_QFX] == signs[load.SOURCE_STATEMENT] == \
        "-19.99,240.0"


def test_card_statement_rows_keep_their_section_as_kind(tmp_path, monkeypatch):
    conn = _conn()
    _seed_card_export(conn, [(_epoch(2026, 6, 1), -10.0)])
    stmts = {"s.pdf": _card_stmt(date(2026, 4, 2), date(2026, 5, 1), "0.00", [
        (date(2026, 4, 3), "10.00", "p", "STMT_PURCHASE"),
        (date(2026, 4, 4), "-5.00", "c", "STMT_PAYMENT"),
        (date(2026, 4, 5), "3.00", "f", "STMT_FEE"),
        (date(2026, 4, 6), "2.00", "i", "STMT_INTEREST"),
    ])}
    _card_bronze(tmp_path, stmts)
    _patch_card_parse(monkeypatch, stmts)
    load.load_card_statements(conn, tmp_path)
    assert conn.execute(
        "SELECT kind, COUNT(*) FROM transactions WHERE source='statement' "
        "GROUP BY kind ORDER BY kind").fetchall() == [
        ("STMT_FEE", 1), ("STMT_INTEREST", 1), ("STMT_PAYMENT", 1),
        ("STMT_PURCHASE", 1)]
    # posted_at is the printed TRANSACTION date, and says so in the payload
    row = json.loads(conn.execute(
        "SELECT payload FROM transactions WHERE kind='STMT_FEE'").fetchone()[0])
    assert row["posted_at_basis"] == "transaction_date"
    assert conn.execute(
        "SELECT posted_at, txn_date FROM transactions WHERE kind='STMT_FEE'"
        ).fetchone() == (_epoch(2026, 4, 5), _epoch(2026, 4, 5))


def test_card_statement_rows_carry_no_running_balance(tmp_path, monkeypatch):
    # A statement rolls from its own opening figure, but it dates its rows by
    # TRANSACTION date while the cycle bills by POST date — so a row
    # transacted before its period opened carries this cycle's balance while
    # sitting, by date, inside the previous one. Read as a series the cycles
    # then contradict each other, which is why this era carries no per-row
    # balance at all: `statement_balances` holds its balance truth.
    conn = _conn()
    _seed_card_export(conn, [(_epoch(2026, 6, 1), -10.0)])
    stmts = {"s.pdf": _card_stmt(date(2026, 4, 2), date(2026, 5, 1), "100.00", [
        (date(2026, 3, 30), "25.00", "transacted before the period", "STMT_PURCHASE"),
        (date(2026, 4, 10), "15.00", "b", "STMT_PURCHASE"),
    ])}
    _card_bronze(tmp_path, stmts)
    _patch_card_parse(monkeypatch, stmts)
    assert load.load_card_statements(conn, tmp_path) == 2
    assert conn.execute(
        "SELECT DISTINCT balance FROM transactions WHERE source='statement'"
        ).fetchall() == [(None,)]
    payload = json.loads(conn.execute(
        "SELECT payload FROM transactions WHERE source='statement' LIMIT 1"
        ).fetchone()[0])
    assert "balance_basis" not in payload      # no balance, so no basis
    # the period's balances are still asserted, by the anchor
    assert conn.execute("SELECT opening, closing FROM statement_balances"
                        ).fetchone() == (100.0, 140.0)


def test_card_statement_with_unreconciled_rows_imports_nothing(tmp_path,
                                                               monkeypatch):
    # The summary still adds up, so the anchor is trusted; the rows do not
    # carry the opening balance to the closing one, so none of them land —
    # and the period is flagged as one whose transactions are NOT in silver.
    conn = _conn()
    _seed_card_export(conn, [(_epoch(2026, 6, 1), -10.0)])
    stmt = _card_stmt(date(2026, 4, 2), date(2026, 5, 1), "100.00",
                      [(date(2026, 4, 10), "25.00", "a", "STMT_PURCHASE")])
    stmt.transactions = stmt.transactions[:0]         # a row the parse missed
    stmts = {"s.pdf": stmt}
    _card_bronze(tmp_path, stmts)
    _patch_card_parse(monkeypatch, stmts)
    assert load.load_card_statements(conn, tmp_path) == 0
    assert conn.execute("SELECT closing, transactions_covered FROM "
                        "statement_balances").fetchall() == [(125.0, 0)]


def test_a_period_straddling_the_export_seam_is_flagged_and_logged(
        tmp_path, monkeypatch, caplog):
    # Gating on the whole billing period gives up the one period that opens
    # before the seam and closes after it: its pre-seam rows reach neither
    # source. The trade is deliberate (a row-level gate would double-count),
    # but silver must not then assert that period's two balances while
    # carrying none of its rows — anyone reconciling between two anchors
    # would get an unexplainable residual.
    conn = _conn()
    seam = _epoch(2026, 5, 15)
    _seed_card_export(conn, [(seam, -10.0)])
    stmts = {"s.pdf": _card_stmt(date(2026, 5, 2), date(2026, 6, 1), "100.00", [
        (date(2026, 5, 3), "25.00", "before the seam", "STMT_PURCHASE"),
        (date(2026, 5, 4), "15.00", "also before the seam", "STMT_PURCHASE"),
        (date(2026, 5, 20), "10.00", "after it", "STMT_PURCHASE"),
    ])}
    _card_bronze(tmp_path, stmts)
    _patch_card_parse(monkeypatch, stmts)
    with caplog.at_level("WARNING"):
        assert load.load_card_statements(conn, tmp_path) == 0
    assert conn.execute("SELECT closing, transactions_covered FROM "
                        "statement_balances").fetchall() == [(150.0, 0)]
    msg = next(r.getMessage() for r in caplog.records if "straddles" in r.message)
    assert "2026-05-15" in msg and "2 row(s)" in msg


def test_the_coverage_flag_is_refreshed_when_the_seam_moves(tmp_path,
                                                            monkeypatch):
    # A period's printed balances never change, so they keep their first
    # observation — but coverage is a function of the export seam, which walks
    # backwards as deeper export history lands. A flag frozen at first
    # observation would go on calling a period uncovered after the export
    # grew to cover it.
    conn = _conn()
    _seed_card_export(conn, [(_epoch(2026, 5, 15), -10.0)])
    stmts = {"s.pdf": _card_stmt(date(2026, 5, 2), date(2026, 6, 1), "100.00", [
        (date(2026, 5, 3), "25.00", "a", "STMT_PURCHASE")])}
    _card_bronze(tmp_path, stmts)
    _patch_card_parse(monkeypatch, stmts)
    load.load_card_statements(conn, tmp_path)
    assert conn.execute("SELECT transactions_covered FROM statement_balances"
                        ).fetchone() == (0,)      # straddled the seam

    # a deeper export lands, dropping the seam below the period's opening
    _seed_card_export(conn, [(_epoch(2026, 4, 1), -5.0)], description="deeper")
    assert load.load_card_statements(conn, tmp_path) == 0
    assert conn.execute("SELECT closing, transactions_covered FROM "
                        "statement_balances").fetchall() == [(125.0, 1)]


def test_card_statement_with_a_broken_summary_records_no_balance(tmp_path,
                                                                 monkeypatch):
    conn = _conn()
    _seed_card_export(conn, [(_epoch(2026, 6, 1), -10.0)])
    stmts = {"s.pdf": _card_stmt(
        date(2026, 4, 2), date(2026, 5, 1), "100.00",
        [(date(2026, 4, 10), "25.00", "a", "STMT_PURCHASE")], new="999.00")}
    _card_bronze(tmp_path, stmts)
    _patch_card_parse(monkeypatch, stmts)
    assert load.load_card_statements(conn, tmp_path) == 0
    assert conn.execute("SELECT COUNT(*) FROM statement_balances").fetchone()[0] == 0
    assert conn.execute("SELECT COUNT(*) FROM transactions WHERE "
                        "source='statement'").fetchone()[0] == 0


def test_a_statement_whose_summary_is_stated_not_derived_still_lands(
        tmp_path, monkeypatch):
    # Every other fixture derives its summary from its rows, so it reconciles
    # by construction and cannot tell a working gate from a deleted one. This
    # one states the figures the way the document prints them — five rows over
    # three sections, hand-totalled — so the gates have something to disagree
    # with.
    conn = _conn()
    _seed_card_export(conn, [(_epoch(2026, 6, 1), -10.0)])
    rows = [(date(2026, 4, 3), "40.25", "p1", "STMT_PURCHASE"),
            (date(2026, 4, 4), "9.75", "p2", "STMT_PURCHASE"),
            (date(2026, 4, 9), "-120.00", "pay", "STMT_PAYMENT"),
            (date(2026, 4, 11), "3.50", "fee", "STMT_FEE"),
            (date(2026, 4, 28), "1.50", "int", "STMT_INTEREST")]
    stmts = {"s.pdf": _card_stmt(
        date(2026, 4, 2), date(2026, 5, 1), "200.00", rows, summary={
            "purchases": "50.00", "payments_credits": "-120.00",
            "fees_charged": "3.50", "interest_charged": "1.50",
            "cash_advances": "0.00", "balance_transfers": "0.00",
            "new_balance": "135.00"})}
    _card_bronze(tmp_path, stmts)
    _patch_card_parse(monkeypatch, stmts)
    assert load.load_card_statements(conn, tmp_path) == 5
    assert conn.execute("SELECT opening, closing, transactions_covered FROM "
                        "statement_balances").fetchall() == [(200.0, 135.0, 1)]


def test_a_statement_whose_stated_summary_disagrees_with_a_section_is_refused(
        tmp_path, monkeypatch):
    # Same statement, with the purchases figure a dollar off. The period
    # total still adds up to New − Previous, so only the per-section identity
    # can see it — and it must, or a whole unparsed section could hide the
    # same way.
    conn = _conn()
    _seed_card_export(conn, [(_epoch(2026, 6, 1), -10.0)])
    stmts = {"s.pdf": _card_stmt(
        date(2026, 4, 2), date(2026, 5, 1), "200.00",
        [(date(2026, 4, 3), "50.00", "p", "STMT_PURCHASE"),
         (date(2026, 4, 9), "-10.00", "pay", "STMT_PAYMENT")],
        summary={"purchases": "51.00", "payments_credits": "-11.00",
                 "new_balance": "240.00"})}
    _card_bronze(tmp_path, stmts)
    _patch_card_parse(monkeypatch, stmts)
    assert load.load_card_statements(conn, tmp_path) == 0
    # the summary still adds up, so the anchor lands — flagged uncovered
    assert conn.execute("SELECT closing, transactions_covered FROM "
                        "statement_balances").fetchall() == [(240.0, 0)]


def test_a_statement_with_no_activity_is_accepted(tmp_path, monkeypatch):
    # A billing period can genuinely carry no transactions. That is a
    # reconciling statement, not a mis-parse: its anchor lands and its
    # (empty) transaction set is complete, so the period is covered.
    conn = _conn()
    _seed_card_export(conn, [(_epoch(2026, 6, 1), -10.0)])
    stmts = {"s.pdf": _card_stmt(date(2026, 4, 2), date(2026, 5, 1), "80.00")}
    _card_bronze(tmp_path, stmts)
    _patch_card_parse(monkeypatch, stmts)
    assert load.load_card_statements(conn, tmp_path) == 0
    assert conn.execute("SELECT opening, closing, transactions_covered FROM "
                        "statement_balances").fetchall() == [(80.0, 80.0, 1)]


def test_card_statement_ids_separate_identical_rows_in_different_periods(
        tmp_path, monkeypatch):
    # Two genuinely different charges can share a transaction date, an amount
    # and a descriptor and be billed a cycle apart (one posted late). The
    # occurrence index counts within one statement, so only the period in the
    # id keeps the second from being swallowed as a re-insert of the first.
    conn = _conn()
    _seed_card_export(conn, [(_epoch(2026, 6, 1), -10.0)])
    row = (date(2026, 3, 30), "9.99", "EXAMPLE MERCHANT", "STMT_PURCHASE")
    stmts = {
        "a.pdf": _card_stmt(date(2026, 3, 2), date(2026, 4, 1), "0.00", [row]),
        "b.pdf": _card_stmt(date(2026, 4, 2), date(2026, 5, 1), "9.99", [row]),
    }
    _card_bronze(tmp_path, stmts)
    _patch_card_parse(monkeypatch, stmts)
    assert load.load_card_statements(conn, tmp_path) == 2
    assert conn.execute("SELECT COUNT(*) FROM transactions WHERE "
                        "source='statement'").fetchone()[0] == 2


def test_a_card_with_no_export_imports_no_statement_transactions(
        tmp_path, monkeypatch):
    # With no export loaded there is no seam, and an absent seam bounds
    # nothing: importing here would put a statement copy of every row beside
    # the copy the first export lands. The anchors are still recorded, so the
    # period reads as anchored-but-unpopulated.
    conn = _conn()
    stmts = {"s.pdf": _card_stmt(date(2026, 4, 2), date(2026, 5, 1), "0.00", [
        (date(2026, 4, 10), "19.99", "EXAMPLE MERCHANT", "STMT_PURCHASE")])}
    _card_bronze(tmp_path, stmts)
    _patch_card_parse(monkeypatch, stmts)
    assert load.load_card_statements(conn, tmp_path) == 0
    assert conn.execute(
        "SELECT period_end, closing, transactions_covered FROM "
        "statement_balances").fetchall() == [(_epoch(2026, 5, 1), 19.99, 0)]
    assert conn.execute("SELECT COUNT(*) FROM transactions WHERE "
                        "source='statement'").fetchone()[0] == 0


def test_a_late_export_does_not_double_a_card_ledger(tmp_path, monkeypatch):
    # The run that motivates the gate: the statements load first, then the
    # export arrives and covers the same period. The purchase must exist once.
    conn = _conn()
    stmts = {"s.pdf": _card_stmt(date(2026, 4, 2), date(2026, 5, 1), "0.00", [
        (date(2026, 4, 10), "19.99", "EXAMPLE MERCHANT", "STMT_PURCHASE")])}
    _card_bronze(tmp_path, stmts)
    _patch_card_parse(monkeypatch, stmts)
    load.load_card_statements(conn, tmp_path)
    # the export reaches back before the period, so the period is wholly
    # export-owned once it lands
    _seed_card_export(conn, [(_epoch(2026, 3, 15), -5.00),
                             (_epoch(2026, 4, 10), -19.99)])
    assert load.load_card_statements(conn, tmp_path) == 0
    assert conn.execute(
        "SELECT source, COUNT(*) FROM transactions WHERE amount = -19.99 "
        "GROUP BY source").fetchall() == [(load.SOURCE_QFX, 1)]
    assert conn.execute("SELECT transactions_covered FROM statement_balances"
                        ).fetchall() == [(1,)]


def test_a_card_heals_once_its_export_lands(tmp_path, monkeypatch):
    # The export's reach begins after the statement's period, so that period
    # belongs to the statement era after all — it imports on the next load
    # and its flag flips with it.
    conn = _conn()
    stmts = {"s.pdf": _card_stmt(date(2026, 4, 2), date(2026, 5, 1), "0.00", [
        (date(2026, 4, 10), "19.99", "EXAMPLE MERCHANT", "STMT_PURCHASE")])}
    _card_bronze(tmp_path, stmts)
    _patch_card_parse(monkeypatch, stmts)
    assert load.load_card_statements(conn, tmp_path) == 0
    _seed_card_export(conn, [(_epoch(2026, 6, 1), -10.00)])
    assert load.load_card_statements(conn, tmp_path) == 1
    assert conn.execute(
        "SELECT description, amount FROM transactions WHERE source='statement'"
        ).fetchall() == [("EXAMPLE MERCHANT", -19.99)]
    assert conn.execute("SELECT transactions_covered FROM statement_balances"
                        ).fetchall() == [(1,)]


def _card_run(root: Path, name: str, slug: str, marker: str) -> int:
    """One bronze run holding one card statement PDF, whose bytes carry
    `marker` so two runs' copies of a period hash differently and both reach
    the parser. Returns the run's epoch timestamp."""
    run = _card_bronze(root, {name: None}, slug=slug)
    (run / "statements" / CARD_EXT / name).write_bytes(
        f"%PDF {marker}".encode())
    return load.bronze.parse_run_ts(slug)


def _two_run_card_bronze(root: Path, name: str, first_slug="20260701T000000Z",
                         second_slug="20260801T000000Z") -> tuple[int, int]:
    """The same statement filename downloaded in two runs, both present
    before the load. Returns the two runs' epoch timestamps."""
    return (_card_run(root, name, first_slug, "copy 0"),
            _card_run(root, name, second_slug, "copy 1"))


def _patch_card_parse_by_run(monkeypatch, by_slug: dict):
    """Parse result keyed by the run dir a PDF sits in, so two copies of one
    period can differ."""
    monkeypatch.setattr(load.statement_parser, "parse_card_statement_pdf",
                        lambda path: by_slug[path.parents[2].name])


def test_a_re_downloaded_period_replaces_a_mis_parsed_copy(tmp_path,
                                                           monkeypatch):
    # First-wins would refuse the period forever: the clean copy hashes
    # differently, so it is parsed and would then be discarded behind the
    # broken one. The copy passing more gates wins, and is stamped with its
    # own run.
    conn = _conn()
    _seed_card_export(conn, [(_epoch(2026, 6, 1), -10.0)])
    first_ts, second_ts = _two_run_card_bronze(tmp_path, "s.pdf")
    rows = [(date(2026, 4, 10), "25.00", "a", "STMT_PURCHASE")]
    _patch_card_parse_by_run(monkeypatch, {
        # the summary block does not add up — no anchor at all
        "20260701T000000Z": _card_stmt(date(2026, 4, 2), date(2026, 5, 1),
                                       "0.00", rows,
                                       summary={"purchases": "999.00"}),
        "20260801T000000Z": _card_stmt(date(2026, 4, 2), date(2026, 5, 1),
                                       "0.00", rows),
    })
    assert load.load_card_statements(conn, tmp_path) == 1
    assert conn.execute(
        "SELECT closing, snapshot_at, transactions_covered FROM "
        "statement_balances").fetchall() == [(25.0, second_ts, 1)]


def test_a_copy_passing_both_gates_beats_one_passing_only_the_summary(
        tmp_path, monkeypatch):
    conn = _conn()
    _seed_card_export(conn, [(_epoch(2026, 6, 1), -10.0)])
    _two_run_card_bronze(tmp_path, "s.pdf")
    _patch_card_parse_by_run(monkeypatch, {
        # the summary adds up, but the purchases section is short a row
        "20260701T000000Z": _card_stmt(
            date(2026, 4, 2), date(2026, 5, 1), "0.00",
            [(date(2026, 4, 10), "25.00", "a", "STMT_PURCHASE")],
            summary={"purchases": "40.00", "new_balance": "40.00"}),
        "20260801T000000Z": _card_stmt(
            date(2026, 4, 2), date(2026, 5, 1), "0.00",
            [(date(2026, 4, 10), "25.00", "a", "STMT_PURCHASE"),
             (date(2026, 4, 11), "15.00", "b", "STMT_PURCHASE")]),
    })
    assert load.load_card_statements(conn, tmp_path) == 2
    assert conn.execute("SELECT transactions_covered FROM statement_balances"
                        ).fetchall() == [(1,)]


def test_equal_parses_keep_the_earlier_run(tmp_path, monkeypatch):
    # Nothing moves without cause: two copies that pass the same gates leave
    # the period on its first observation.
    conn = _conn()
    _seed_card_export(conn, [(_epoch(2026, 6, 1), -10.0)])
    first_ts, _ = _two_run_card_bronze(tmp_path, "s.pdf")
    rows = [(date(2026, 4, 10), "25.00", "a", "STMT_PURCHASE")]
    _patch_card_parse_by_run(monkeypatch, {
        "20260701T000000Z": _card_stmt(date(2026, 4, 2), date(2026, 5, 1),
                                       "0.00", rows),
        "20260801T000000Z": _card_stmt(date(2026, 4, 2), date(2026, 5, 1),
                                       "0.00", rows),
    })
    assert load.load_card_statements(conn, tmp_path) == 1
    assert conn.execute(
        "SELECT snapshot_at FROM statement_balances").fetchall() == [(first_ts,)]


def test_a_better_copy_arriving_later_re_records_the_period(tmp_path,
                                                            monkeypatch):
    # The two runs load SEPARATELY, so the mis-parsed copy's anchors are
    # already in silver when the clean copy arrives. They must move with it:
    # a period left on the first copy's figures would assert one copy's
    # balances while flagging the other copy's rows as covered — the
    # unexplainable residual the coverage flag exists to prevent — and the
    # closing figure is what a whole span of derived balances chains from.
    conn = _conn()
    _seed_card_export(conn, [(_epoch(2026, 6, 1), -10.0)])
    _patch_card_parse_by_run(monkeypatch, {
        # the summary adds up, but the purchases section is short a row and
        # the opening date mis-rendered
        "20260701T000000Z": _card_stmt(
            date(2026, 4, 2), date(2026, 5, 1), "0.00",
            [(date(2026, 4, 10), "25.00", "a", "STMT_PURCHASE")],
            summary={"purchases": "60.00", "new_balance": "60.00"}),
        "20260801T000000Z": _card_stmt(
            date(2026, 4, 1), date(2026, 5, 1), "0.00",
            [(date(2026, 4, 10), "25.00", "a", "STMT_PURCHASE"),
             (date(2026, 4, 11), "15.00", "b", "STMT_PURCHASE")]),
    })
    first_ts = _card_run(tmp_path, "s.pdf", "20260701T000000Z", "copy 0")
    assert load.load_card_statements(conn, tmp_path) == 0
    assert conn.execute(
        "SELECT period_start, closing, snapshot_at, transactions_covered "
        "FROM statement_balances").fetchall() == [
            (_epoch(2026, 4, 2), 60.0, first_ts, 0)]

    second_ts = _card_run(tmp_path, "s.pdf", "20260801T000000Z", "copy 1")
    assert load.load_card_statements(conn, tmp_path) == 2
    assert conn.execute(
        "SELECT period_start, closing, snapshot_at, transactions_covered "
        "FROM statement_balances").fetchall() == [
            (_epoch(2026, 4, 1), 40.0, second_ts, 1)]
    # the anchor now agrees with the rows it flags as covered
    assert conn.execute(
        "SELECT sum(amount) FROM transactions WHERE source='statement'"
        ).fetchone()[0] == -40.0


def test_load_card_statements_is_idempotent(tmp_path, monkeypatch):
    conn = _conn()
    _seed_card_export(conn, [(_epoch(2026, 6, 1), -10.0)])
    stmts = {"s.pdf": _card_stmt(date(2026, 4, 2), date(2026, 5, 1), "0.00", [
        (date(2026, 4, 10), "25.00", "a", "STMT_PURCHASE")])}
    _card_bronze(tmp_path, stmts)
    _patch_card_parse(monkeypatch, stmts)
    load.load_card_statements(conn, tmp_path)
    load.load_card_statements(conn, tmp_path)
    assert conn.execute("SELECT COUNT(*) FROM transactions WHERE "
                        "source='statement'").fetchone()[0] == 1
    assert conn.execute("SELECT COUNT(*) FROM statement_balances").fetchone()[0] == 1


def test_deposit_statement_pass_ignores_a_card(tmp_path, monkeypatch):
    # The two passes are disjoint: the deposit pass never opens a card PDF
    # (already pinned above) and the card pass never opens a deposit one.
    conn = _conn()
    _seed_card_export(conn, [(_epoch(2026, 6, 1), -10.0)])
    _seed_export(conn, [(_epoch(2026, 6, 1), -10.0)])
    run = _card_bronze(tmp_path, {})
    (run / "statements" / EXT).mkdir(parents=True)
    (run / "statements" / EXT / "d.pdf").write_bytes(b"%PDF deposit")
    (run / "accounts.json").write_text(json.dumps([
        {"account_external_id": CARD_EXT, "product": "card"},
        {"account_external_id": EXT, "product": "dda"}]))
    opened = []
    monkeypatch.setattr(load.statement_parser, "parse_card_statement_pdf",
                        lambda path: opened.append(path.name))
    assert load.load_card_statements(conn, tmp_path) == 0
    assert opened == []




# ============================================================
# The derived per-transaction card balance
# ============================================================
#
# A span is only walked when its whole window sits at or after the account's
# export seam, so every one of its rows is export-sourced (statement rows are
# dated by transaction date and belong to a different clock). These tests
# therefore seed a "seam" row at or before the oldest anchor: it fixes where
# the export's reach begins, sits in no walked window, and so keeps no
# derived balance — which `_balances` excludes so the assertions read as the
# span itself.
#
# The pass demands TWO agreeing signals before it will rewrite anything: the
# roster (`_seed_card_roster`) and the export files' own shape
# (`_card_export_tree`). `_derive` supplies the second, so a test that omits
# one of them is testing the guard rather than the reconstruction.

def _anchor(conn, period_end, closing, *, ext=CARD_EXT, opening=None):
    conn.execute(
        "INSERT OR REPLACE INTO statement_balances (account_external_id, "
        "period_start, period_end, opening, closing, snapshot_at, "
        "transactions_covered) VALUES (?,?,?,?,?,?,1)",
        (ext, period_end - 30 * 86400, period_end, opening, closing, 1))
    conn.commit()


def _seed_seam(conn, when, ext=CARD_EXT):
    """One export row fixing where the export's reach begins."""
    _seed_card_export(conn, [(when, -1.0)], ext=ext, description="seam")


def _card_export_tree(root: Path, *, ext=CARD_EXT,
                      slug="20260801T000000Z") -> Path:
    """A bronze tree whose export files call `ext` a card. Only the CSV header
    and the OFX message set are read (`is_card_export`); the rows are never
    parsed by this pass."""
    tx = root / slug / "transactions"
    tx.mkdir(parents=True, exist_ok=True)
    (tx / f"{ext}.csv").write_text(CARD_CSV)
    (tx / f"{ext}.qfx").write_text(CARD_QFX)
    return root


def _deposit_export_tree(root: Path, *, ext=EXT,
                         slug="20260801T000000Z") -> Path:
    """The same tree, with a DEPOSIT-shaped export for `ext` — the file
    content that must veto the rewrite however the roster is stamped."""
    tx = root / slug / "transactions"
    tx.mkdir(parents=True, exist_ok=True)
    (tx / f"{ext}.csv").write_text(CSV)
    (tx / f"{ext}.qfx").write_text(QFX)
    return root


def _derive(conn, root):
    """The reconstruction, against a bronze tree that agrees CARD_EXT is a
    card."""
    return load.derive_card_balances(conn, _card_export_tree(root))


def _balances(conn, ext=CARD_EXT):
    return [r[0] for r in conn.execute(
        "SELECT balance FROM transactions WHERE account_external_id = ? "
        "AND description <> 'seam' ORDER BY posted_at, fitid", (ext,))]


def test_derived_balances_roll_between_two_statement_anchors(tmp_path):
    conn = _conn()
    _seed_card_roster(conn, balance=100.0, pending=0.0)
    _seed_seam(conn, _epoch(2026, 5, 1))
    _seed_card_export(conn, [(_epoch(2026, 5, 5), -30.0),
                             (_epoch(2026, 5, 20), -20.0),
                             (_epoch(2026, 5, 25), 50.0)])
    _anchor(conn, _epoch(2026, 5, 1), 100.0)
    _anchor(conn, _epoch(2026, 6, 1), 100.0)          # 100 +30 +20 -50
    # one span; the newest span has no rows after the last anchor at all
    assert _derive(conn, tmp_path) == (3, 1, 0)
    # spend raises the amount owed, a payment lowers it
    assert _balances(conn) == [130.0, 150.0, 100.0]


def test_a_span_that_misses_its_anchor_keeps_no_derived_balance(tmp_path,
                                                                caplog):
    conn = _conn()
    _seed_card_roster(conn, balance=100.0, pending=0.0)
    _seed_seam(conn, _epoch(2026, 5, 1))
    _seed_card_export(conn, [(_epoch(2026, 5, 5), -30.0)])
    _anchor(conn, _epoch(2026, 5, 1), 100.0)
    _anchor(conn, _epoch(2026, 6, 1), 999.0)          # the ledger cannot reach it
    with caplog.at_level("WARNING"):
        assert _derive(conn, tmp_path) == (0, 0, 1)
    assert any("does not carry" in r.message for r in caplog.records)
    assert _balances(conn) == [None]                  # nothing fabricated


def test_a_span_that_misses_its_anchor_by_one_cent_is_discarded(tmp_path):
    # The landing test is exact, in integer cents. A near miss is a ledger
    # with something wrong in it, not a rounding artefact to be waved through
    # — and a span accepted "close enough" would put a wrong number on every
    # row it covers.
    conn = _conn()
    _seed_card_roster(conn, balance=130.0, pending=0.0)
    _seed_seam(conn, _epoch(2026, 5, 1))
    _seed_card_export(conn, [(_epoch(2026, 5, 5), -30.0)])
    _anchor(conn, _epoch(2026, 5, 1), 100.0)
    _anchor(conn, _epoch(2026, 6, 1), 130.01)
    assert _derive(conn, tmp_path) == (0, 0, 1)
    assert _balances(conn) == [None]


def test_the_newest_span_anchors_on_the_roster_balance(tmp_path):
    conn = _conn()
    # 120 owed live, of which 20 has not posted → the posted ledger lands on
    # 100, and that is the identity, not an unexplained gap.
    _seed_card_roster(conn, balance=120.0, pending=20.0)
    _seed_seam(conn, _epoch(2026, 6, 1))
    _seed_card_export(conn, [(_epoch(2026, 6, 10), -40.0),
                             (_epoch(2026, 6, 20), 10.0)])
    _anchor(conn, _epoch(2026, 6, 1), 70.0)
    assert _derive(conn, tmp_path) == (2, 1, 0)
    assert _balances(conn) == [110.0, 100.0]


def test_the_newest_span_is_discarded_when_pending_cannot_explain_the_gap(
        tmp_path, caplog):
    conn = _conn()
    _seed_card_roster(conn, balance=500.0, pending=0.0)
    _seed_seam(conn, _epoch(2026, 6, 1))
    _seed_card_export(conn, [(_epoch(2026, 6, 10), -40.0)])
    _anchor(conn, _epoch(2026, 6, 1), 70.0)
    with caplog.at_level("WARNING"):
        assert _derive(conn, tmp_path) == (0, 0, 1)
    assert any("pending" in r.message for r in caplog.records)
    assert _balances(conn) == [None]


def test_the_newest_span_survives_a_roster_with_no_pending_figure(tmp_path,
                                                                  caplog):
    # Without a pending figure the gap cannot be checked against anything, so
    # the span is kept on its statement anchor and the implied pending amount
    # is logged rather than silently swallowed.
    conn = _conn()
    _seed_card_roster(conn, balance=125.0, pending=None)
    _seed_seam(conn, _epoch(2026, 6, 1))
    _seed_card_export(conn, [(_epoch(2026, 6, 10), -40.0)])
    _anchor(conn, _epoch(2026, 6, 1), 70.0)
    with caplog.at_level("INFO"):
        assert _derive(conn, tmp_path) == (1, 1, 0)
    assert _balances(conn) == [110.0]
    assert "lands 15.00 below" in next(
        r.getMessage() for r in caplog.records if "no pending amount" in r.message)


def test_the_implied_pending_gap_is_logged_even_when_it_is_zero(tmp_path,
                                                                caplog):
    # A roster observation need not carry the pending field at all, so this is
    # the branch a card without one takes. Logging only a NON-zero gap would
    # make the identity observable exactly when it fails, and a run that
    # checked nothing indistinguishable from one that landed exactly.
    conn = _conn()
    _seed_card_roster(conn, balance=110.0, pending=None)
    _seed_seam(conn, _epoch(2026, 6, 1))
    _seed_card_export(conn, [(_epoch(2026, 6, 10), -40.0)])
    _anchor(conn, _epoch(2026, 6, 1), 70.0)
    with caplog.at_level("INFO"):
        assert _derive(conn, tmp_path) == (1, 1, 0)
    assert "lands 0.00 below" in next(
        r.getMessage() for r in caplog.records if "no pending amount" in r.message)


def test_the_newest_span_survives_a_roster_with_no_live_balance(tmp_path,
                                                                caplog):
    # A roster observation can carry no balance at all (a detail block that
    # failed to parse). There is then nothing for the newest span to land on,
    # so it is kept on its statement anchor rather than discarded — but
    # nothing is claimed about it either.
    conn = _conn()
    _seed_card_roster(conn, balance=None, pending=None)
    _seed_seam(conn, _epoch(2026, 6, 1))
    _seed_card_export(conn, [(_epoch(2026, 6, 10), -40.0)])
    _anchor(conn, _epoch(2026, 6, 1), 70.0)
    with caplog.at_level("INFO"):
        assert _derive(conn, tmp_path) == (1, 1, 0)
    assert _balances(conn) == [110.0]
    assert not any("pending" in r.message for r in caplog.records)


def test_derived_balances_are_marked_in_the_payload(tmp_path):
    conn = _conn()
    _seed_card_roster(conn, balance=130.0, pending=0.0)
    _seed_seam(conn, _epoch(2026, 5, 1))
    _seed_card_export(conn, [(_epoch(2026, 5, 5), -30.0)])
    _anchor(conn, _epoch(2026, 5, 1), 100.0)
    _derive(conn, tmp_path)
    payload = json.loads(conn.execute(
        "SELECT payload FROM transactions WHERE description <> 'seam'"
        ).fetchone()[0])
    assert payload["balance_basis"] == load.BALANCE_BASIS_DERIVED
    assert payload["fitid"] is None            # the rest of the payload survives


def test_a_span_reaching_below_the_export_seam_is_not_walked(tmp_path):
    # The oldest anchor sits before the seam, so its span would mix export
    # rows with the pre-seam days neither source covers. It is dropped
    # silently — a structural gap at the export's edge, not an anomaly — and
    # with only one anchor left above the seam there is no span to walk: the
    # account's single export row is the seam itself and keeps no balance.
    conn = _conn()
    _seed_card_roster(conn, balance=130.0, pending=0.0)
    _seed_card_export(conn, [(_epoch(2026, 5, 20), -30.0)])   # the seam
    _anchor(conn, _epoch(2026, 5, 1), 50.0)                   # before it
    _anchor(conn, _epoch(2026, 6, 1), 130.0)
    assert _derive(conn, tmp_path) == (0, 0, 0)
    assert _balances(conn) == [None]


def test_derive_card_balances_is_idempotent(tmp_path):
    conn = _conn()
    _seed_card_roster(conn, balance=150.0, pending=0.0)
    _seed_seam(conn, _epoch(2026, 5, 1))
    _seed_card_export(conn, [(_epoch(2026, 5, 5), -30.0),
                             (_epoch(2026, 5, 20), -20.0)])
    _anchor(conn, _epoch(2026, 5, 1), 100.0)
    assert _derive(conn, tmp_path) == (2, 1, 0)
    assert _derive(conn, tmp_path) == (2, 1, 0)
    assert _balances(conn) == [130.0, 150.0]


def test_a_card_without_export_rows_derives_nothing(tmp_path):
    conn = _conn()
    _seed_card_roster(conn, balance=100.0)
    _anchor(conn, _epoch(2026, 5, 1), 100.0)
    assert _derive(conn, tmp_path) == (0, 0, 0)


def test_a_deposit_accounts_balances_are_never_touched(tmp_path):
    # The reconstruction is a card-only mechanism: a deposit row's balance is
    # the provider's own CSV column and must survive untouched (and unmarked).
    # Here the ROSTER is the guard that holds — the bronze tree's export files
    # for this account are card-shaped, so the file-content signal alone would
    # let the rewrite through.
    conn = _conn()
    _seed_card_roster(conn, balance=130.0, pending=0.0)
    _seed_seam(conn, _epoch(2026, 5, 1))
    _seed_card_export(conn, [(_epoch(2026, 5, 5), -30.0)])
    _anchor(conn, _epoch(2026, 5, 1), 100.0)
    # a deposit account, stamped 'dda' by its roster row, carrying a
    # provider-supplied CSV balance and an anchor of its own
    load._insert_account(conn, 1_700_000_000, {
        "account_external_id": EXT, "product": "dda", "account_type": "CHK",
        "nickname": "Example Checking", "mask": "…1234", "currency": "USD",
        "balance": 2257.50})
    _seed_export_balance(conn, _epoch(2026, 5, 5), -10.0, 2257.50)
    _anchor(conn, _epoch(2026, 5, 1), 100.0, ext=EXT)   # would be an anchor…
    _card_export_tree(tmp_path, ext=EXT)               # …and the files agree…
    assert _derive(conn, tmp_path) == (1, 1, 0)        # …only the card is walked
    row = conn.execute(
        "SELECT balance, payload FROM transactions WHERE "
        "account_external_id = ?", (EXT,)).fetchone()
    assert row[0] == 2257.50
    assert "balance_basis" not in json.loads(row[1])


def test_a_roster_claim_alone_never_authorises_the_rewrite(tmp_path):
    # The other half of the same guard. Here the roster is what is wrong — a
    # deposit account stamped 'card' — and the export files are what still
    # say deposit. The pass destroys balances, so the two signals must AGREE:
    # one of them alone is never enough.
    conn = _conn()
    load._insert_account(conn, 1_700_000_000, {
        "account_external_id": EXT, "product": "card", "account_type": "CHK",
        "nickname": "Example Checking", "mask": "…1234", "currency": "USD",
        "balance": 2257.50})
    _seed_export_balance(conn, _epoch(2026, 5, 1), -10.0, 2267.50)
    _seed_export_balance(conn, _epoch(2026, 5, 5), -10.0, 2257.50)
    _anchor(conn, _epoch(2026, 5, 1), 100.0, ext=EXT)
    _anchor(conn, _epoch(2026, 6, 1), 999.0, ext=EXT)   # a span that cannot land
    _deposit_export_tree(tmp_path)
    assert load.derive_card_balances(conn, tmp_path) == (0, 0, 0)
    assert [r[0] for r in conn.execute(
        "SELECT balance FROM transactions WHERE account_external_id = ? "
        "ORDER BY posted_at", (EXT,))] == [2267.50, 2257.50]


def test_a_later_anchor_withdraws_a_balance_it_no_longer_supports(tmp_path):
    # The pass is authoritative, not additive: once a new statement anchor
    # breaks a span that used to land, the balances that span had derived are
    # cleared rather than left behind as numbers no anchor stands behind.
    conn = _conn()
    _seed_card_roster(conn, balance=130.0, pending=0.0)
    _seed_seam(conn, _epoch(2026, 5, 1))
    _seed_card_export(conn, [(_epoch(2026, 5, 5), -30.0)])
    _anchor(conn, _epoch(2026, 5, 1), 100.0)
    assert _derive(conn, tmp_path) == (1, 1, 0)
    assert _balances(conn) == [130.0]

    _anchor(conn, _epoch(2026, 6, 1), 999.0)   # a statement the ledger misses
    assert _derive(conn, tmp_path) == (0, 0, 1)
    assert _balances(conn) == [None]
    payload = json.loads(conn.execute(
        "SELECT payload FROM transactions WHERE description <> 'seam'"
        ).fetchone()[0])
    assert "balance_basis" not in payload


def test_an_anchor_that_changes_value_between_runs_moves_the_balances(tmp_path):
    # An anchor is not immutable: a period re-parsed from a corrected
    # statement, or one whose first observation was wrong, lands a different
    # closing figure. The pass recomputes from the anchors every time rather
    # than tracking what it already wrote, so the whole span moves with it.
    conn = _conn()
    _seed_card_roster(conn, balance=150.0, pending=0.0)
    _seed_seam(conn, _epoch(2026, 5, 1))
    _seed_card_export(conn, [(_epoch(2026, 5, 5), -30.0),
                             (_epoch(2026, 5, 20), -20.0)])
    _anchor(conn, _epoch(2026, 5, 1), 100.0)
    _anchor(conn, _epoch(2026, 6, 1), 150.0)
    assert _derive(conn, tmp_path) == (2, 1, 0)
    assert _balances(conn) == [130.0, 150.0]

    _anchor(conn, _epoch(2026, 5, 1), 200.0)   # the opening anchor, corrected
    _anchor(conn, _epoch(2026, 6, 1), 250.0)
    assert _derive(conn, tmp_path) == (2, 1, 0)
    assert _balances(conn) == [230.0, 250.0]


# ============================================================
# Parser generations — a re-parse replaces, it does not accumulate
# ============================================================

def _seed_statement_row(conn, fitid="stmt_written_by_an_older_parser"):
    load._insert_transaction(conn, EXT, {
        "fitid": fitid, "posted_at": _epoch(2020, 1, 1), "amount": -5.0,
        "kind": None, "description": "a payee the parser used to read",
        "check_number": None, "balance": None, "source": "statement",
        "payload": {}})
    conn.commit()


def test_a_moved_parser_drops_the_statement_rows_and_nothing_else():
    conn = _conn()
    _seed_export(conn, [(_epoch(2026, 1, 1), 20.0)])
    _seed_statement_row(conn)
    load.silver.stamp_generation(
        conn, load.STATEMENT_GENERATION_SCOPE, "an older parser")

    conn.execute(
        "INSERT INTO statement_balances (account_external_id, period_start, "
        "period_end, opening, closing, snapshot_at) "
        "VALUES (?, 1, 2, 0.0, 1.0, 3)", (CARD_EXT,))

    assert load._purge_stale_statement_rows(conn) == 1
    # The export ledgers key on structural ids that carry no parsed text, so
    # they are not the loader's to re-derive and must survive untouched.
    assert [r[0] for r in conn.execute(
        "SELECT source FROM transactions").fetchall()] == ["qfx"]
    # The period anchors are parser output too — their key holds the period
    # the parser read — and the same pass re-derives them.
    assert conn.execute(
        "SELECT COUNT(*) FROM statement_balances").fetchone()[0] == 0


def test_an_unmoved_parser_drops_nothing():
    conn = _conn()
    _seed_statement_row(conn)
    load.silver.stamp_generation(
        conn, load.STATEMENT_GENERATION_SCOPE, load.STATEMENT_GENERATION)

    assert load._purge_stale_statement_rows(conn) == 0
    assert conn.execute(
        "SELECT COUNT(*) FROM transactions").fetchone()[0] == 1


def test_a_db_that_predates_the_stamp_re_derives_once():
    # No row for the scope at all: the rows in hand came from a parser this
    # DB never recorded, so they cannot be vouched for and are re-derived.
    conn = _conn()
    _seed_statement_row(conn)
    assert load._purge_stale_statement_rows(conn) == 1


def _load_once(tmp_path):
    """A loaded silver DB plus the argv that reloads it."""
    root = tmp_path / "bronze"
    root.mkdir()
    _make_run(root, "20260801T120000Z")
    db = tmp_path / "chase.db"
    args = ["--bronze-dir", str(root), "--silver-db", str(db)]
    assert load.main(args) == 0
    return db, args


def _age_the_stamp(db):
    """Seed what an older parser wrote, and the stamp it left behind."""
    conn = sqlite3.connect(db)
    _seed_statement_row(conn)
    load.silver.stamp_generation(
        conn, load.STATEMENT_GENERATION_SCOPE, "an older parser")
    conn.commit()
    conn.close()


def test_a_moved_parser_re_derives_without_a_new_bronze_run(tmp_path):
    # The whole defect, end to end. A parser-only change lands no bronze run,
    # so the pending flag stays down — the generation itself has to raise the
    # gate, or silver goes on holding rows no parser in the tree produces
    # while the re-keyed ones pile up beside them.
    db, args = _load_once(tmp_path)
    _age_the_stamp(db)

    assert load.main(args) == 0          # nothing new in bronze

    conn = sqlite3.connect(db)
    assert conn.execute(
        "SELECT COUNT(*) FROM transactions WHERE source = 'statement'"
    ).fetchone()[0] == 0
    conn.close()


def test_a_short_re_derivation_is_not_stamped_so_the_next_load_retries(tmp_path):
    # The seeded row cannot come back: the fixture's statement PDF is a stub,
    # so the re-derivation returns fewer rows than silver held. Committing
    # that as the new truth would bury a parser that had broken.
    db, args = _load_once(tmp_path)
    _age_the_stamp(db)
    assert load.main(args) == 0

    conn = sqlite3.connect(db)
    assert conn.execute(
        "SELECT generation FROM parser_generations WHERE scope = ?",
        (load.STATEMENT_GENERATION_SCOPE,)).fetchone()[0] == "an older parser"
    conn.close()

    # One retry, which has nothing better to compare against, settles it.
    assert load.main(args) == 0
    conn = sqlite3.connect(db)
    assert conn.execute(
        "SELECT generation FROM parser_generations WHERE scope = ?",
        (load.STATEMENT_GENERATION_SCOPE,)).fetchone()[0] == \
        load.STATEMENT_GENERATION
    conn.close()
