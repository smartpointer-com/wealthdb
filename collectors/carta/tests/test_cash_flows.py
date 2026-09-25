"""
Unit tests for load.py's cash-flow ledger (migration 0003).

Covers the inception-to-date statement parsing, the per-period differencing,
the cap-table cash-flow synthesis (exercises + exit), and the fund ledger:
notices, the pre-coverage residue, and a supplied file that itemises it.
In-memory SQLite + a tmp_path bronze edir. Synthetic data only — no real
holdings or figures.
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


# ============================================================
# the fund ledger: notices reconciled against statements
# ============================================================

def _fund_docs(tmp_path: Path, rows: list[dict]) -> Path:
    """A documents dir holding an index and one (empty) PDF per row. The
    parsers are stubbed per test, so the bytes never matter — only that the
    file the index names is on disk, which is what _fund_cash_flows checks."""
    docs = tmp_path / "docs"
    docs.mkdir()
    (docs / "index.json").write_text(json.dumps({"results": rows}))
    for row in rows:
        (docs / f"doc_{row['id']}.pdf").write_bytes(b"")
    return docs


def _stub_parsers(monkeypatch, statements: dict, notices: dict):
    """Keyed by document id: `statements` yields (contributions, distributions)
    inception-to-date, `notices` yields a parsed notice."""
    monkeypatch.setattr(load, "_parse_statement_flows",
                        lambda pdf: statements[pdf.stem.removeprefix("doc_")])
    monkeypatch.setattr(load, "_parse_notice",
                        lambda pdf: notices.get(pdf.stem.removeprefix("doc_")))


def _ledger(conn) -> list[tuple]:
    return conn.execute(
        "SELECT cash_flow_external_id, kind, flow_date, amount FROM cash_flows "
        "ORDER BY kind, cash_flow_external_id").fetchall()


_STMT_ROW = {"id": "s1", "document_type": "Capital account statement",
             "document_date": "12/31/2098"}
_CALL_A = {"id": "n1", "document_type": "Capital call notice"}
_CALL_B = {"id": "n2", "document_type": "Capital call notice"}


def _two_call_notices() -> dict:
    """The `_CALL_NOTICE` fixture above, already parsed, plus a second call
    three months later: 61250 + 93750 noticed, and 156250 the earliest one
    says preceded it."""
    return {
        "n1": {"kind": "capital_call", "date": "06/15/2098",
               "issued": "05/20/2098", "amount": 61250.0, "cumulative": 217500.0},
        "n2": {"kind": "capital_call", "date": "09/16/2098",
               "issued": "08/20/2098", "amount": 93750.0, "cumulative": 311250.0},
    }


def test_fund_notices_win_and_the_remainder_is_one_residue_row(migrated, tmp_path,
                                                               monkeypatch):
    # Per kind, the better source wins: calls come from the notices, which
    # state the day the money was due, while the statements' inception-to-date
    # total is only reconciled against them. What the notices do not account
    # for is ONE residue row — everything called before Carta shared anything
    # — dated at the earliest notice's issue date, the last day it is known to
    # have been called. Distributions have no notices here, so they fall back
    # to statement differencing and keep the period-end date that implies.
    docs = _fund_docs(tmp_path, [_STMT_ROW, _CALL_A, _CALL_B])
    _stub_parsers(monkeypatch, {"s1": (281250.0, 46250.0)}, _two_call_notices())

    assert load._fund_cash_flows(migrated, 42, docs, 1_700_000_000, tmp_path) == 4
    assert _ledger(migrated) == [
        ("call:42:notice:n1", "capital_call", "06/15/2098", 61250.0),
        ("call:42:notice:n2", "capital_call", "09/16/2098", 93750.0),
        # the fund's own figure (called post-call less the call), not the
        # 126250 the statements imply, and dated at the notice's issue date
        ("call:42:pre:n1", "capital_call", "05/20/2098", 156250.0),
        ("dist:42:s1", "distribution", "12/31/2098", 46250.0),
    ]
    desc = migrated.execute(
        "SELECT description FROM cash_flows WHERE cash_flow_external_id = "
        "'call:42:pre:n1'").fetchone()[0]
    assert "not itemised" in desc


def test_fund_falls_back_to_statement_differencing_without_notices(migrated,
                                                                   tmp_path,
                                                                   monkeypatch):
    # No notices at all: the statements are all there is, and each positive
    # jump in the inception-to-date figure becomes one flow at the period end.
    rows = [_STMT_ROW, {"id": "s2", "document_type": "Capital account statement",
                        "document_date": "12/31/2099"}]
    docs = _fund_docs(tmp_path, rows)
    _stub_parsers(monkeypatch,
                  {"s1": (281250.0, 0.0), "s2": (358750.0, 46250.0)}, {})

    assert load._fund_cash_flows(migrated, 42, docs, 1_700_000_000, tmp_path) == 3
    assert _ledger(migrated) == [
        ("call:42:s1", "capital_call", "12/31/2098", 281250.0),
        ("call:42:s2", "capital_call", "12/31/2099", 77500.0),
        ("dist:42:s2", "distribution", "12/31/2099", 46250.0),
    ]


def test_fund_ledger_is_rebuilt_not_accumulated(migrated, tmp_path, monkeypatch):
    # An earlier run whose notice copies were unreadable emitted
    # statement-differenced calls under different ids. Left to accumulate,
    # both shapes survive and the fund's called capital doubles — so the run
    # clears its own two kinds first, and only those.
    migrated.execute(
        "INSERT INTO cash_flows(cash_flow_external_id, entity_external_id, "
        "snapshot_at, kind, currency, payload) VALUES "
        "('call:42:s1', 42, 1, 'capital_call', 'USD', '{}'), "
        "('exercise:42:c1', 42, 1, 'exercise', 'USD', '{}')")
    docs = _fund_docs(tmp_path, [_STMT_ROW, _CALL_A, _CALL_B])
    _stub_parsers(monkeypatch, {"s1": (281250.0, 0.0)}, _two_call_notices())

    load._fund_cash_flows(migrated, 42, docs, 1_700_000_000, tmp_path)
    ids = [r[0] for r in _ledger(migrated)]
    assert "call:42:s1" not in ids                 # the superseded shape is gone
    assert "exercise:42:c1" in ids                 # another kind is untouched
    assert ids.count("call:42:pre:n1") == 1


def test_fund_warns_when_the_two_sources_leave_capital_unaccounted(
        migrated, tmp_path, monkeypatch, caplog):
    # The notice's own running total wins over the differenced figure. Below
    # it is the ordinary case of a notice issued since the last statement, and
    # says nothing; above it means called capital neither source accounts for.
    docs = _fund_docs(tmp_path, [_STMT_ROW, _CALL_A, _CALL_B])
    _stub_parsers(monkeypatch, {"s1": (406250.0, 0.0)}, _two_call_notices())

    with caplog.at_level("WARNING"):
        load._fund_cash_flows(migrated, 42, docs, 1_700_000_000, tmp_path)
    assert "accounted for by neither" in caplog.text
    residue = migrated.execute(
        "SELECT amount FROM cash_flows WHERE cash_flow_external_id = "
        "'call:42:pre:n1'").fetchone()[0]
    assert residue == 156250.0                     # the fund's figure, not 251250


# ============================================================
# a supplied file itemises the lump before Carta's coverage
# ============================================================

def _supplied(tmp_path: Path, *rows: str) -> None:
    """The fund's supplied `<eid>-transactions.csv` in the bronze root."""
    (tmp_path / "42-transactions.csv").write_text(
        "# calls made before the platform's coverage\n" + "\n".join(rows) + "\n")


def test_supplied_calls_itemise_the_residue_before_the_earliest_notice(
        migrated, tmp_path, monkeypatch):
    # The earliest notice says 156250 was called before it and dates none of
    # it. The holder's own records do: each supplied call lands at its own
    # date, and a residue fully accounted for leaves no row behind.
    docs = _fund_docs(tmp_path, [_STMT_ROW, _CALL_A, _CALL_B])
    _stub_parsers(monkeypatch, {"s1": (281250.0, 0.0)}, _two_call_notices())
    _supplied(tmp_path,
              "2097-03-01,capital_call,90000,,first call",
              "2097-11-15,capital_call,66250,,")

    assert load._fund_cash_flows(migrated, 42, docs, 1_700_000_000, tmp_path) == 4
    assert _ledger(migrated) == [
        ("call:42:notice:n1", "capital_call", "06/15/2098", 61250.0),
        ("call:42:notice:n2", "capital_call", "09/16/2098", 93750.0),
        ("call:42:supplied:0", "capital_call", "2097-03-01", 90000.0),
        ("call:42:supplied:1", "capital_call", "2097-11-15", 66250.0),
    ]
    descs = dict(migrated.execute(
        "SELECT cash_flow_external_id, description FROM cash_flows").fetchall())
    assert descs["call:42:supplied:0"] == "first call"
    assert descs["call:42:supplied:1"] == "fund capital call"


def test_the_residue_keeps_what_the_supplied_calls_leave(migrated, tmp_path,
                                                         monkeypatch):
    docs = _fund_docs(tmp_path, [_STMT_ROW, _CALL_A, _CALL_B])
    _stub_parsers(monkeypatch, {"s1": (281250.0, 0.0)}, _two_call_notices())
    _supplied(tmp_path, "2097-03-01,capital_call,90000,,")

    load._fund_cash_flows(migrated, 42, docs, 1_700_000_000, tmp_path)
    ledger = {r[0]: r[3] for r in _ledger(migrated)}
    assert ledger["call:42:supplied:0"] == 90000.0
    assert ledger["call:42:pre:n1"] == 66250.0


def test_a_supplied_call_after_the_lump_is_not_part_of_it(migrated, tmp_path,
                                                          monkeypatch, caplog):
    # The lump is everything before the earliest notice; a later call is the
    # notices' to state, and counting it would double what they already do.
    docs = _fund_docs(tmp_path, [_STMT_ROW, _CALL_A, _CALL_B])
    _stub_parsers(monkeypatch, {"s1": (281250.0, 0.0)}, _two_call_notices())
    _supplied(tmp_path, "2098-07-01,capital_call,40000,,")

    with caplog.at_level("WARNING"):
        load._fund_cash_flows(migrated, 42, docs, 1_700_000_000, tmp_path)
    assert "not dated on or before" in caplog.text
    ledger = {r[0]: r[3] for r in _ledger(migrated)}
    assert "call:42:supplied:0" not in ledger
    assert ledger["call:42:pre:n1"] == 156250.0


def test_supplied_calls_beyond_the_lump_are_kept_and_the_excess_logged(
        migrated, tmp_path, monkeypatch, caplog):
    # The supplied rows are dated and the lump is not, so they stand; the
    # fund's own figure is what says something is off, and the log says so.
    docs = _fund_docs(tmp_path, [_STMT_ROW, _CALL_A, _CALL_B])
    _stub_parsers(monkeypatch, {"s1": (281250.0, 0.0)}, _two_call_notices())
    _supplied(tmp_path, "2097-03-01,capital_call,210000,,")

    with caplog.at_level("WARNING"):
        load._fund_cash_flows(migrated, 42, docs, 1_700_000_000, tmp_path)
    assert "exceed what the fund reports" in caplog.text
    ledger = {r[0]: r[3] for r in _ledger(migrated)}
    assert ledger["call:42:supplied:0"] == 210000.0
    assert "call:42:pre:n1" not in ledger


def test_supplied_calls_itemise_the_first_statement_without_notices(
        migrated, tmp_path, monkeypatch):
    # Without notices the first statement's inception-to-date figure is the
    # lump; later statements' deltas are per period and stay as they are.
    rows = [_STMT_ROW, {"id": "s2", "document_type": "Capital account statement",
                        "document_date": "12/31/2099"}]
    docs = _fund_docs(tmp_path, rows)
    _stub_parsers(monkeypatch,
                  {"s1": (281250.0, 0.0), "s2": (358750.0, 0.0)}, {})
    _supplied(tmp_path, "2097-03-01,capital_call,210000,,")

    load._fund_cash_flows(migrated, 42, docs, 1_700_000_000, tmp_path)
    assert _ledger(migrated) == [
        ("call:42:s1", "capital_call", "12/31/2098", 71250.0),
        ("call:42:s2", "capital_call", "12/31/2099", 77500.0),
        ("call:42:supplied:0", "capital_call", "2097-03-01", 210000.0),
    ]


def test_a_fund_ignores_a_supplied_row_it_cannot_pair(migrated, tmp_path,
                                                      monkeypatch, caplog):
    # A fund's ledger holds calls and distributions; a company's kinds in its
    # file would reach gold as unpaired legs.
    docs = _fund_docs(tmp_path, [_STMT_ROW, _CALL_A, _CALL_B])
    _stub_parsers(monkeypatch, {"s1": (281250.0, 0.0)}, _two_call_notices())
    _supplied(tmp_path, "2097-03-01,deposit,90000,,")

    with caplog.at_level("WARNING"):
        load._fund_cash_flows(migrated, 42, docs, 1_700_000_000, tmp_path)
    assert "not a capital_call or distribution with an amount" in caplog.text
    assert not [r for r in _ledger(migrated) if ":supplied:" in r[0]]
