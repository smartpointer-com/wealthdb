"""Tests for load.py — the svb builder's discovery and routing, its activity →
transactions pass, and its real-zero (never inferred, never terminal) policy.
The PDF parser is mocked, so no real statements are needed and all data is
synthetic."""
import calendar
import sqlite3
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

import load as B  # noqa: E402
from derived_marks import DERIVED_SHA  # noqa: E402
import pdf_parsers_svbdep as D  # noqa: E402
import pdf_parsers_svbwa as P  # noqa: E402

MIGRATIONS = Path(__file__).parent / "migrations"

# Canned parser output keyed by filename stem, nested so discovery has a
# folder layout to walk. SVM-000000 plays an account held early whose
# later statement loses its holdings table without stating a zero (it must
# carry forward, never zero). SVM-000001 plays an account that states $0 and
# then reports a residual the following month — a zero is not a closure.
_CANNED = {
    "SVM-000000/2021-12-31": {
        "family": P.FAMILY_BROKERAGE,
        "period_end": "2021-12-31",
        "stated_total": 50.0,
        "accounts": [{"account_external_id": "SVM-000000", "holdings": [
            {"description": "AAAA EQUITY", "instrument_key": "AAAA",
             "quantity": 10.0, "price": 5.0, "market_value": 50.0}]}],
    },
    "SVM-000000/2022-06-30": {  # holdings lost, no stated zero → carry forward
        "family": P.FAMILY_BROKERAGE,
        "period_end": "2022-06-30",
        "stated_total": None,
        "accounts": [{"account_external_id": "SVM-000000", "holdings": []}],
    },
    "SVM-000001/2022-12-31": {
        "family": P.FAMILY_BROKERAGE,
        "period_end": "2022-12-31",
        "stated_total": 0.0,
        "accounts": [{"account_external_id": "SVM-000001", "holdings": []}],
    },
    "SVM-000001/2023-01-31": {  # residual AFTER the stated zero
        "family": P.FAMILY_BROKERAGE,
        "period_end": "2023-01-31",
        "stated_total": 200.0,
        "accounts": [{"account_external_id": "SVM-000001", "holdings": [
            {"description": "BBBB FUND", "instrument_key": "BBBB",
             "quantity": 20.0, "price": 10.0, "market_value": 200.0}]}],
    },
    # The OCR families. The text-layer pass reports these as image-only and
    # the loader sends them round again; the canned results below are what the
    # OCR parser hands back.
    "Deposit 0000000000/2022-12-31": {
        "family": D.FAMILY_DEPOSIT,
        "period_end": "2022-12-31",
        "accounts": [{
            "account_external_id": "0000000000",
            "holdings": [{"description": D.CASH_DESC, "instrument_key": None,
                          "quantity": None, "price": None,
                          "market_value": 1500.0}],
            "activity": [
                {"date": "2022-12-05", "section": D.SECTION_LEDGER,
                 "account_type": "", "verb": "DEPOSIT", "description": "A PAYER",
                 "quantity": None, "amount": 2000.0, "ordinal": 0},
                {"date": "2022-12-09", "section": D.SECTION_LEDGER,
                 "account_type": "", "verb": "WITHDRAWAL",
                 "description": "A PAYEE", "quantity": None,
                 "amount": -500.0, "ordinal": 1},
            ],
            "activity_totals": {},
        }],
    },
    "Deposit 0000000001/2022-12-31": {   # a section its own arithmetic refused
        "family": D.FAMILY_DEPOSIT,
        "period_end": "2022-12-31",
        "accounts": [{
            "account_external_id": "0000000001",
            "holdings": [], "activity": [], "activity_totals": {},
            "_error": "balance summary does not close: 1.00 vs stated 2.00",
        }],
    },
    "Mortgage 0000000002/2022-01-14": {
        "family": D.FAMILY_MORTGAGE,
        "period_end": "2022-01-14",
        "accounts": [{
            "account_external_id": "0000000002",
            "holdings": [{"description": D.LOAN_DESC, "instrument_key": None,
                          "quantity": None, "price": None,
                          "market_value": -900000.0,
                          "payment_breakdown": {"interest": {
                              "paid_last_month": 1000.0,
                              "paid_year_to_date": 2000.0}}}],
            "activity": [], "activity_totals": {},
        }],
    },
    "Annual 0000000002/2022": {          # recognised, counted, not parsed
        "family": D.FAMILY_ANNUAL_LOAN,
        "accounts": [],
    },
}

# Which families the text-layer pass sees. It reads a text layer, so it
# reports every OCR family as image-only and the loader re-parses.
_OCR_FAMILIES = {D.FAMILY_DEPOSIT, D.FAMILY_MORTGAGE, D.FAMILY_ANNUAL_LOAN}


def _write_bronze(tmp_path, stems=_CANNED, distinct_bytes=False):
    bronze = tmp_path / "bronze"
    for stem in stems:
        pdf = bronze / f"{stem}.pdf"
        pdf.parent.mkdir(parents=True, exist_ok=True)
        pdf.write_bytes(f"%PDF-{stem}".encode() if distinct_bytes
                        else b"%PDF-fake\n")
    return bronze


def _stem(path, bronze):
    return Path(path).relative_to(bronze).with_suffix("").as_posix()


def _patch_parsers(monkeypatch, bronze, canned=None):
    """Stand in for both passes. The text-layer parser reports an OCR family's
    document as image-only, exactly as it does against real bytes, so the
    loader's dispatch to the OCR parser is what the tests exercise."""
    canned = canned if canned is not None else _CANNED

    def text_layer(path, expected_signatures=()):
        parsed = dict(canned[_stem(path, bronze)])
        if parsed["family"] in _OCR_FAMILIES:
            return {"path": str(path), "family": P.FAMILY_IMAGE_ONLY,
                    "accounts": []}
        return parsed

    def ocr(path, expected_signatures=()):
        return dict(canned[_stem(path, bronze)])

    monkeypatch.setattr(B.pdf_parsers_svbwa, "parse_svbwa_statement_pdf",
                        text_layer)
    monkeypatch.setattr(B.pdf_parsers_svbdep, "parse_svbdep_statement_pdf", ocr)


def _build_all(tmp_path, monkeypatch):
    """Build and return one connection per statement family, keyed as the
    parsers report it. Each family gets its own silver DB (and its own gold
    source id), so a test looks at the one it is about."""
    bronze = _write_bronze(tmp_path)
    _patch_parsers(monkeypatch, bronze)
    db = tmp_path / "svb.db"
    # cache_dir=None + max_workers=1: parse the mocked parser in-process, per
    # file. These fixtures deliberately give distinct logical statements the
    # SAME bytes, which the content-keyed cache would (correctly, for real
    # bronze) collapse; the modelling tests therefore run cache-off, and
    # test_parse_cache_reuse covers the cache with distinct content.
    B.build(db, bronze, signatures=(), migrations_dir=MIGRATIONS,
            cache_dir=None, max_workers=1)
    return {family: sqlite3.connect(str(path))
            for family, path in B.silver_paths(db).items()}


def _build(tmp_path, monkeypatch):
    """The brokerage family's silver, which most of these tests are about."""
    return _build_all(tmp_path, monkeypatch)[P.FAMILY_BROKERAGE]


def test_migrations_lockstep_with_fidelity_web():
    """svb.db is read by the shared Fidelity gold adapter (kind:"fidelity"), so
    these migrations MUST stay byte-identical to fidelity-web's silver schema.
    This guard fails if either side drifts (see CLAUDE.md / DESIGN.md)."""
    fidelity = MIGRATIONS.parent.parent / "fidelity-web" / "migrations"
    for mig in sorted(MIGRATIONS.glob("*.sql")):
        twin = fidelity / mig.name
        assert twin.is_file(), f"{mig.name} has no fidelity-web counterpart"
        assert mig.read_bytes() == twin.read_bytes(), (
            f"{mig.name} drifted from fidelity-web/migrations — re-sync them")


# ============================================================
# Discovery and routing
# ============================================================

def test_discovery_is_recursive_and_ordered(tmp_path):
    """Discovery walks whatever folders it is given, and inserts replay its
    order under INSERT OR REPLACE, so the order must be fixed."""
    bronze = _write_bronze(tmp_path)
    (bronze / "signature.txt").write_text("EXAMPLE HOLDER - Example Property\n")
    found = B.discover_statements(bronze)
    assert [_stem(p, bronze) for p in found] == [
        "Annual 0000000002/2022",
        "Deposit 0000000000/2022-12-31",
        "Deposit 0000000001/2022-12-31",
        "Mortgage 0000000002/2022-01-14",
        "SVM-000000/2021-12-31",
        "SVM-000000/2022-06-30",
        "SVM-000001/2022-12-31",
        "SVM-000001/2023-01-31",
    ]


def test_empty_bronze_never_clobbers_existing_silver(tmp_path, monkeypatch):
    """A mis-pointed / empty bronze dir must abort BEFORE touching the
    existing svb.db (regression: --bronze-dir pointed at the data-dir root
    replaced a good silver with an empty rebuild, zeroing the source out of
    gold). A nested dir tree with no PDFs in it is just as empty."""
    import pytest

    conn = _build(tmp_path, monkeypatch)  # good silver from canned bronze
    conn.close()
    db = tmp_path / "svb.db"
    before = db.read_bytes()
    (tmp_path / "empty").mkdir()
    (tmp_path / "empty" / "nested" / "deeper").mkdir(parents=True)
    for bad in (tmp_path / "empty", tmp_path / "does-not-exist"):
        with pytest.raises(SystemExit, match="no statement PDFs"):
            B.build(db, bad, signatures=(), migrations_dir=MIGRATIONS,
                    cache_dir=None, max_workers=1)
        assert db.read_bytes() == before, "existing silver was modified"


def test_every_document_is_counted_by_family(tmp_path, monkeypatch, caplog):
    import logging

    with caplog.at_level(logging.INFO, logger="svb"):
        _build(tmp_path, monkeypatch)
    summary = [r.getMessage() for r in caplog.records
               if "svb silver built" in r.message]
    assert len(summary) == 1
    # The census names every family it saw, so a skip cannot pass unnoticed.
    for expected in ("brokerage=4", "deposit=2", "mortgage=1", "annual_loan=1",
                     "unreadable-section=1"):
        assert expected in summary[0], summary[0]


def test_each_family_lands_in_its_own_silver(tmp_path, monkeypatch):
    """Sharing one source id would let each family's next statement read as
    the others' closure, so each gets its own DB and its own gold source."""
    conns = _build_all(tmp_path, monkeypatch)
    seen = {family: {r[0] for r in conn.execute(
        "SELECT DISTINCT account_external_id FROM historical_position_snapshots")}
        for family, conn in conns.items()}
    assert seen[P.FAMILY_BROKERAGE] == {"SVM-000000", "SVM-000001"}
    # The refused deposit section reached silver as nothing at all.
    assert seen[D.FAMILY_DEPOSIT] == {"0000000000"}
    assert seen[D.FAMILY_MORTGAGE] == {"0000000002"}


def test_silver_paths_sit_beside_the_named_one(tmp_path):
    paths = B.silver_paths(tmp_path / "svb.db")
    assert paths[P.FAMILY_BROKERAGE].name == "svb.db"
    assert paths[D.FAMILY_DEPOSIT].name == "svb-deposit.db"
    assert paths[D.FAMILY_MORTGAGE].name == "svb-mortgage.db"


def test_signatures_read_from_sidecar_lines(tmp_path):
    bronze = tmp_path / "bronze"
    bronze.mkdir()
    (bronze / "signature.txt").write_text(
        "# a comment\nEXAMPLE HOLDER - Example Property\n\n"
        "EXAMPLE COMPANY LLC - Example Business\n")
    assert B.read_signatures(bronze, None) == (
        "EXAMPLE HOLDER - Example Property",
        "EXAMPLE COMPANY LLC - Example Business",
    )
    # An explicit override replaces the sidecar rather than adding to it.
    assert B.read_signatures(bronze, ["ONLY THIS"]) == ("ONLY THIS",)
    assert B.read_signatures(tmp_path / "nope", None) == ()


def test_cache_key_is_insensitive_to_signature_order():
    a = B._cache_key("sha", "fp", ("one", "two"))
    b = B._cache_key("sha", "fp", ("two", "one"))
    assert a == b
    assert B._cache_key("sha", "fp", ("one",)) != a


# ============================================================
# Real zeros — never inferred, never terminal
# ============================================================

def test_lost_holdings_table_carries_forward(tmp_path, monkeypatch):
    conn = _build(tmp_path, monkeypatch)
    rows = conn.execute(
        "SELECT as_of_date, market_value FROM historical_position_snapshots "
        "WHERE account_external_id='SVM-000000' ORDER BY as_of_date").fetchall()
    # Only the 2021-12-31 holding row — the 2022-06 statement stated no total,
    # so its empty table is read as "unknown", not as a real zero.
    assert len(rows) == 1
    assert rows[0] == (B.ts_from_iso("2021-12-31"), 50.0)


def test_stated_zero_is_recorded_as_a_real_row(tmp_path, monkeypatch):
    conn = _build(tmp_path, monkeypatch)
    row = conn.execute(
        "SELECT market_value, description, source_sha256 FROM "
        "historical_position_snapshots WHERE account_external_id='SVM-000001' "
        "AND as_of_date=?", (B.ts_from_iso("2022-12-31"),)).fetchone()
    assert row[0] == 0.0
    assert row[1] == B._ZERO_DESC
    # It carries the statement's own sha: it is an observation, not a marker.
    assert len(row[2]) == 64


def test_zero_is_not_a_closure(tmp_path, monkeypatch):
    """A $0 month can be followed by a residual (a late dividend landing), so
    nothing is stamped as terminal and the series ends where statements end."""
    conn = _build(tmp_path, monkeypatch)
    rows = conn.execute(
        "SELECT as_of_date, market_value FROM historical_position_snapshots "
        "WHERE account_external_id='SVM-000001' ORDER BY as_of_date").fetchall()
    assert rows == [(B.ts_from_iso("2022-12-31"), 0.0),
                    (B.ts_from_iso("2023-01-31"), 200.0)]


def test_every_row_names_where_it_came_from(tmp_path, monkeypatch):
    """A row carries either its statement's sha256 or the marker of the one
    other source allowed to supply a value — the advisor workbook. Nothing is
    synthesised from thin air; telling the two apart at row level is covered
    by test_derived_marks_fill_the_archive_gaps."""
    conn = _build(tmp_path, monkeypatch)
    shas = {r[0] for r in conn.execute(
        "SELECT DISTINCT source_sha256 FROM historical_position_snapshots")}
    assert shas, "the fixture builds some rows"
    # These fixtures ship no workbook, so every row here is a statement's.
    assert all(len(s) == 64 for s in shas)


def test_masters_synthesised_neutral(tmp_path, monkeypatch):
    conn = _build(tmp_path, monkeypatch)
    accts = dict(conn.execute(
        "SELECT account_external_id, portfolio_external_id FROM accounts"))
    assert set(accts) == {"SVM-000000", "SVM-000001"}
    assert all(p == B._SYNTHETIC_PORTFOLIO for p in accts.values())
    kinds = [r[0] for r in conn.execute("SELECT kind FROM portfolios")]
    assert kinds and all(k == "other" for k in kinds)  # neutral → taxable default
    # the sleeve's master sits at its latest as_of, so the gold adapter
    # projects it across the account's whole history.
    latest = conn.execute(
        "SELECT snapshot_at FROM accounts WHERE account_external_id='SVM-000001'"
    ).fetchone()[0]
    assert latest == B.ts_from_iso("2023-01-31")


def test_duplicate_option_descriptions_all_survive(tmp_path, monkeypatch):
    # Two short option legs share a description (strike is off-description) but
    # differ by OCC instrument_key; both must persist, so the negative legs
    # aren't collapsed and the signed total stays correct.
    canned = {"SVM-000099/s": {
        "family": P.FAMILY_BROKERAGE, "period_end": "2021-12-31",
        "stated_total": 890.0, "accounts": [
            {"account_external_id": "SVM-000099", "holdings": [
                {"description": "CALL (XYZ) WIDGET JAN 18 30",
                 "instrument_key": "XYZ300118C100", "quantity": -10.0,
                 "price": 5.0, "market_value": -50.0},
                {"description": "CALL (XYZ) WIDGET JAN 18 30",
                 "instrument_key": "XYZ300118C110", "quantity": -20.0,
                 "price": 3.0, "market_value": -60.0},
                {"description": "WIDGET INC", "instrument_key": "XYZ",
                 "quantity": 100.0, "price": 10.0, "market_value": 1000.0}]}]}}
    bronze = _write_bronze(tmp_path, canned)
    _patch_parsers(monkeypatch, bronze, canned)
    db = tmp_path / "svb.db"
    B.build(db, bronze, signatures=(), migrations_dir=MIGRATIONS,
            cache_dir=None, max_workers=1)
    conn = sqlite3.connect(str(db))
    rows = conn.execute(
        "SELECT market_value FROM historical_position_snapshots "
        "WHERE account_external_id='SVM-000099' ORDER BY market_value").fetchall()
    assert len(rows) == 3, "all three legs must survive (no PK collapse)"
    assert sum(r[0] for r in rows) == 1000.0 - 50.0 - 60.0 == 890.0


# ============================================================
# Activity → transactions
# ============================================================

def _activity_row(**over):
    row = {"date": "2022-01-08", "section": P.SECTION_ADDITIONS,
           "account_type": "CASH", "verb": "WIRE TRANS TO BANK",
           "description": "WD00 000002 1 ST PTY TRF TO EXAMPLE",
           "quantity": None, "amount": -4000.0, "ordinal": 0}
    row.update(over)
    return row


def _with_activity(tmp_path, monkeypatch, rows, totals=None, account="SVM-000000"):
    canned = {"SVM-000000/a": {
        "family": P.FAMILY_BROKERAGE, "period_end": "2022-01-31",
        "stated_total": 0.0,
        "accounts": [{"account_external_id": account, "holdings": [],
                      "activity": rows, "activity_totals": totals or {}}]}}
    bronze = _write_bronze(tmp_path, canned)
    _patch_parsers(monkeypatch, bronze, canned)
    db = tmp_path / "svb.db"
    B.build(db, bronze, signatures=(), migrations_dir=MIGRATIONS,
            cache_dir=None, max_workers=1)
    return sqlite3.connect(str(db))


def test_transactions_written_with_source_sign(tmp_path, monkeypatch):
    conn = _with_activity(tmp_path, monkeypatch, [
        _activity_row(),
        _activity_row(verb="WIRE TRANS FROM BANK", amount=1000.0,
                      description="WR00 000001", ordinal=1),
    ])
    rows = conn.execute(
        "SELECT kind, amount, json_extract(payload,'$.Action') "
        "FROM transactions ORDER BY amount").fetchall()
    # Both wires book as WIRE; the adapter reads the direction off the sign,
    # so the sign has to survive the parse intact in both directions.
    assert rows[0][0] == rows[1][0] == "WIRE"
    assert rows[0][1] == -4000.0
    assert rows[1][1] == 1000.0
    assert rows[0][2].startswith("WIRE TRANS TO BANK WD00")


def test_transaction_kind_map_covers_every_verb():
    """Every verb the parser can return has a gold-readable kind, and the four
    verbs whose obvious kind would invert them stay source-signed."""
    for verb in P._ACTIVITY_VERBS:
        assert verb in B._KIND_BY_VERB, verb
    # Each of these prints in the reverse of its obvious kind's direction, and
    # that kind's canonical sign is pinned — taking it would flip the row.
    assert B._KIND_BY_VERB["ADJ NON-RESIDENT TAX"] == "ADJUSTMENT"
    assert B._KIND_BY_VERB["DIVIDEND ADJUSTMENT"] == "ADJUSTMENT"
    assert B._KIND_BY_VERB["CANCELLED BUY"] == "ADJUSTMENT"
    assert B._KIND_BY_VERB["CANCELLED SELL"] == "ADJUSTMENT"


def test_core_fund_rows_book_as_sweeps(tmp_path, monkeypatch):
    # The core-fund section reuses the blotter's verbs for what is really the
    # cash ↔ money-market sweep, so the section decides the kind.
    conn = _with_activity(tmp_path, monkeypatch, [
        _activity_row(section=P.SECTION_CORE_FUND, verb="YOU SOLD",
                      amount=900.0, quantity=-900.0, ordinal=0),
        _activity_row(section=P.SECTION_CORE_FUND, verb="REINVESTMENT",
                      amount=-100.0, quantity=100.0, ordinal=1),
    ])
    kinds = dict(conn.execute("SELECT kind, amount FROM transactions"))
    assert kinds == {"CASH_SWEEP_OUT": 900.0, "CASH_SWEEP_IN": -100.0}


def test_the_pending_sections_are_skipped(tmp_path, monkeypatch):
    """Both pending sections are projections that settle into a later
    statement, which books them again."""
    conn = _with_activity(tmp_path, monkeypatch, [
        _activity_row(section=P.SECTION_PENDING_DISTRIBUTIONS, verb="",
                      amount=175.0),
        _activity_row(section=P.SECTION_TRADES_PENDING, verb="BOUGHT",
                      amount=-800.0, ordinal=1),
    ])
    assert conn.execute("SELECT COUNT(*) FROM transactions").fetchone()[0] == 0


def test_the_trade_blotter_is_booked(tmp_path, monkeypatch):
    """A buy and a sell are what moved the money inside the account. Left
    out, a sale at one custodian and the purchase it funded at another read
    as capital appearing from nowhere."""
    conn = _with_activity(tmp_path, monkeypatch, [
        _activity_row(section=P.SECTION_TRADES, verb="YOU BOUGHT",
                      amount=-4000.0, quantity=100.0),
        _activity_row(section=P.SECTION_TRADES, verb="YOU SOLD",
                      amount=6000.0, quantity=50.0, ordinal=1),
    ])
    rows = conn.execute(
        "SELECT kind, amount, quantity FROM transactions ORDER BY amount"
    ).fetchall()
    assert rows == [("BUY", -4000.0, 100.0), ("SELL", 6000.0, 50.0)]


def test_a_core_fund_sweep_is_not_a_trade(tmp_path, monkeypatch):
    """The same verbs appear in the core-fund section, where they are the
    account's cash <-> money-market sweep rather than a trade. The section is
    what tells them apart, so booking the blotter must not blur the two."""
    conn = _with_activity(tmp_path, monkeypatch, [
        _activity_row(section=P.SECTION_CORE_FUND, verb="YOU BOUGHT",
                      amount=-4000.0),
        _activity_row(section=P.SECTION_TRADES, verb="YOU BOUGHT",
                      amount=-4000.0, ordinal=1),
    ])
    kinds = sorted(k for (k,) in conn.execute("SELECT kind FROM transactions"))
    assert kinds == ["BUY", "CASH_SWEEP_IN"]


def _linking_archive(tmp_path, monkeypatch, *, stated_opening=0.0):
    """One statement of one account: a trade the holdings prove, a round
    trip in a security the account never held at a statement end, a dividend
    and a core-fund sweep. Dated 2099 so nothing can match a real row."""
    canned = {"SVM-000000/2099-01": {
        "family": P.FAMILY_BROKERAGE,
        "period_start": "2099-01-01", "period_end": "2099-01-31",
        "stated_opening": stated_opening, "stated_total": 1000.0,
        "accounts": [{
            "account_external_id": "SVM-000000",
            "holdings": [{"description": "EXAMPLE COMPANY CL A",
                          "instrument_key": "AAAA", "quantity": 10.0,
                          "price": 100.0, "market_value": 1000.0}],
            "activity": [
                _activity_row(date="2099-01-05", section=P.SECTION_TRADES,
                              verb="YOU BOUGHT", quantity=10.0,
                              description="EXAMPLE COMPANY CL A @ 100.00",
                              amount=-1000.0, ordinal=0),
                _activity_row(date="2099-01-06", section=P.SECTION_TRADES,
                              verb="YOU BOUGHT", quantity=5.0,
                              description="OTHER EXAMPLE INC COM",
                              amount=-500.0, ordinal=1),
                _activity_row(date="2099-01-07", section=P.SECTION_TRADES,
                              verb="YOU SOLD", quantity=-5.0,
                              description="OTHER EXAMPLE INC COM",
                              amount=510.0, ordinal=2),
                _activity_row(date="2099-01-20", section=P.SECTION_INCOME,
                              verb="DIVIDEND RECEIVED",
                              description="EXAMPLE COMPANY CL A",
                              amount=10.0, ordinal=3),
                _activity_row(date="2099-01-21", section=P.SECTION_CORE_FUND,
                              verb="REINVESTMENT", quantity=10.0,
                              description="EXAMPLE GOVERNMENT MONEY MARKET",
                              amount=-10.0, ordinal=4),
            ],
            "activity_totals": {}}]}}
    bronze = _write_bronze(tmp_path, canned)
    _patch_parsers(monkeypatch, bronze, canned)
    db = tmp_path / "svb.db"
    B.build(db, bronze, signatures=(), migrations_dir=MIGRATIONS,
            cache_dir=None, max_workers=1)
    return sqlite3.connect(str(db))


def _links_by_verb_and_amount(conn):
    return {(verb, amount): (key, hint) for verb, amount, key, hint in conn.execute(
        "SELECT json_extract(payload,'$.Transaction'), amount, instrument_key, "
        "json_extract(payload,'$.InstrumentHint') FROM transactions")}


def test_a_trade_carries_the_key_its_holdings_prove(tmp_path, monkeypatch):
    got = _links_by_verb_and_amount(_linking_archive(tmp_path, monkeypatch))
    assert got[("YOU BOUGHT", -1000.0)] == ("AAAA", None)


def test_an_unproved_trade_states_what_it_was_looked_up_by(tmp_path, monkeypatch):
    got = _links_by_verb_and_amount(_linking_archive(tmp_path, monkeypatch))
    assert got[("YOU BOUGHT", -500.0)] == (None, "OTHEREXAMPLEINCCOM")
    assert got[("YOU SOLD", 510.0)] == (None, "OTHEREXAMPLEINCCOM")


def test_rows_that_move_no_security_are_neither_linked_nor_hinted(
        tmp_path, monkeypatch):
    # A dividend carries no quantity to prove anything with, and a core-fund
    # sweep moves the account's cash, not an investment.
    got = _links_by_verb_and_amount(_linking_archive(tmp_path, monkeypatch))
    assert got[("DIVIDEND RECEIVED", 10.0)] == (None, None)
    assert got[("REINVESTMENT", -10.0)] == (None, None)


def test_only_a_stated_zero_opens_an_accounts_first_window(tmp_path, monkeypatch):
    got = _links_by_verb_and_amount(
        _linking_archive(tmp_path, monkeypatch, stated_opening=500.0))
    assert got[("YOU BOUGHT", -1000.0)] == (None, "EXAMPLECOMPANYCLA@100.00")


def test_linking_leaves_the_activity_ids_alone(tmp_path, monkeypatch):
    import hashlib

    conn = _linking_archive(tmp_path, monkeypatch)
    sha = hashlib.sha256(b"%PDF-fake\n").hexdigest()
    row = _activity_row(date="2099-01-05", section=P.SECTION_TRADES,
                        verb="YOU BOUGHT", quantity=10.0,
                        description="EXAMPLE COMPANY CL A @ 100.00",
                        amount=-1000.0, ordinal=0)
    (key,) = conn.execute("SELECT instrument_key FROM transactions "
                          "WHERE activity_id = ?",
                          (B.activity_id(sha, "SVM-000000", row),)).fetchone()
    assert key == "AAAA"


def test_the_build_reports_its_links(tmp_path, monkeypatch, caplog):
    import logging

    with caplog.at_level(logging.INFO, logger="svb"):
        _linking_archive(tmp_path, monkeypatch)
    assert any("instrument links: 1 activity row(s) linked, 2 not "
               "[no candidate=2]" in r.message for r in caplog.records)


def _trade(date, verb, quantity, amount, ordinal,
           description="EXAMPLE COMPANY CL A"):
    return _activity_row(date=date, section=P.SECTION_TRADES, verb=verb,
                         quantity=quantity, description=description,
                         amount=amount, ordinal=ordinal)


def _statement(month, holdings, activity):
    """A 2099 statement of SVM-000000 that opens where the last one closed."""
    return {
        "family": P.FAMILY_BROKERAGE,
        "period_start": f"2099-{month}-01",
        "period_end": f"2099-{month}-{calendar.monthrange(2099, int(month))[1]}",
        "stated_opening": 0.0 if month == "01" else None,
        "stated_total": sum(q * 100.0 for q in holdings.values()),
        "accounts": [{
            "account_external_id": "SVM-000000",
            "holdings": [{"description": "EXAMPLE COMPANY CL A",
                          "instrument_key": k, "quantity": q, "price": 100.0,
                          "market_value": q * 100.0}
                         for k, q in holdings.items()],
            "activity": activity, "activity_totals": {}}]}


def _build_statements(tmp_path, monkeypatch, canned):
    bronze = _write_bronze(tmp_path, canned, distinct_bytes=True)
    _patch_parsers(monkeypatch, bronze, canned)
    db = tmp_path / "svb.db"
    B.build(db, bronze, signatures=(), migrations_dir=MIGRATIONS,
            cache_dir=None, max_workers=1)
    conn = sqlite3.connect(str(db))
    return conn.execute(
        "SELECT date(timestamp,'unixepoch'), kind, quantity, amount, "
        "instrument_key FROM transactions ORDER BY timestamp, amount").fetchall()


def test_a_corrected_booking_leaves_only_the_fill_booked_again(
        tmp_path, monkeypatch):
    # Booked, cancelled and booked again on one statement: the account
    # bought once, so silver holds one purchase, which the holdings prove.
    got = _build_statements(tmp_path, monkeypatch, {"SVM-000000/2099-01": _statement(
        "01", {"AAAA": 10.0}, [
            _trade("2099-01-05", "YOU BOUGHT", 10.0, -1000.0, 0),
            _trade("2099-01-05", "CANCELLED BUY", -10.0, 1000.0, 1),
            _trade("2099-01-05", "YOU BOUGHT", 10.0, -1004.0, 2)])})
    assert got == [("2099-01-05", "BUY", 10.0, -1004.0, "AAAA")]


def test_a_booking_cancelled_a_statement_later_is_left_out_too(
        tmp_path, monkeypatch, caplog):
    # Sold in February, cancelled and sold again on March's statement, whose
    # holdings no longer carry the security.
    import logging

    canned = {
        "SVM-000000/2099-01": _statement("01", {"AAAA": 10.0}, [
            _trade("2099-01-05", "YOU BOUGHT", 10.0, -1000.0, 0)]),
        "SVM-000000/2099-02": _statement("02", {}, [
            _trade("2099-02-10", "YOU SOLD", -10.0, 1200.0, 0)]),
        "SVM-000000/2099-03": _statement("03", {}, [
            _trade("2099-02-10", "CANCELLED SELL", 10.0, -1200.0, 0),
            _trade("2099-02-10", "YOU SOLD", -10.0, 1200.0, 1)]),
    }
    with caplog.at_level(logging.INFO, logger="svb"):
        got = _build_statements(tmp_path, monkeypatch, canned)
    assert got == [("2099-01-05", "BUY", 10.0, -1000.0, "AAAA"),
                   ("2099-02-10", "SELL", -10.0, 1200.0, "AAAA")]
    assert any("cancelled trades: 1 booking(s) left out" in r.message
               for r in caplog.records)
    assert any("instrument links: 2 activity row(s) linked, 0 not" in r.message
               for r in caplog.records)


def test_a_cancellation_whose_booking_is_not_in_the_archive_is_kept(
        tmp_path, monkeypatch, caplog):
    import logging

    with caplog.at_level(logging.WARNING, logger="svb"):
        got = _build_statements(tmp_path, monkeypatch, {
            "SVM-000000/2099-01": _statement("01", {}, [
                _trade("2099-01-05", "CANCELLED SELL", 10.0, -1200.0, 0),
                # A booking the other way is not the one cancelled.
                _trade("2099-01-06", "YOU BOUGHT", 10.0, -1200.0, 1),
                _trade("2099-01-07", "YOU SOLD", -10.0, 1200.0, 2)])})
    assert [kind for _, kind, *_ in got] == ["ADJUSTMENT", "BUY", "SELL"]
    assert any("cancels no booking" in r.message for r in caplog.records)


def test_undated_and_verbless_rows_are_not_booked(tmp_path, monkeypatch, caplog):
    import logging

    with caplog.at_level(logging.WARNING, logger="svb"):
        conn = _with_activity(tmp_path, monkeypatch, [
            _activity_row(date=None, verb="", section=P.SECTION_INCOME,
                          description="Corporate Accrued Interest Earned",
                          amount=50.0),
            _activity_row(verb="", section=P.SECTION_INCOME,
                          description="EXAMPLE TREAS BILLS ZERO",
                          amount=7000.0, ordinal=1),
        ])
    assert conn.execute("SELECT COUNT(*) FROM transactions").fetchone()[0] == 0
    # The verbless dated row is reported by statement name rather than guessed at.
    assert any("no recognised transaction verb" in r.message
               for r in caplog.records)


def test_activity_id_is_deterministic_and_ordinal_separated():
    sha = "0" * 64
    a = _activity_row(verb="TRANSFERRED FROM", amount=2000.0, ordinal=3)
    b = _activity_row(verb="TRANSFERRED FROM", amount=2000.0, ordinal=4)
    assert B.activity_id(sha, "SVM-000000", a) == B.activity_id(sha, "SVM-000000", a)
    # Same day, same counterparty, same amount — only the page position differs.
    assert B.activity_id(sha, "SVM-000000", a) != B.activity_id(sha, "SVM-000000", b)
    assert B.activity_id(sha, "SVM-000001", a) != B.activity_id(sha, "SVM-000000", a)
    assert B.activity_id(sha, "SVM-000000", a).startswith("svb-")


def test_same_day_duplicate_transfers_both_survive(tmp_path, monkeypatch):
    conn = _with_activity(tmp_path, monkeypatch, [
        _activity_row(verb="TRANSFERRED FROM", amount=2000.0,
                      description="VS SV M-0000 00-1", ordinal=0),
        _activity_row(verb="TRANSFERRED FROM", amount=2000.0,
                      description="VS SV M-0000 00-1", ordinal=1),
    ])
    assert conn.execute("SELECT COUNT(*) FROM transactions").fetchone()[0] == 2


def test_reconcile_activity_reports_a_section_that_does_not_add_up():
    parsed = {"accounts": [{
        "account_external_id": "SVM-000000",
        "activity": [_activity_row(amount=-4000.0)],
        "activity_totals": {P.SECTION_ADDITIONS: -4000.0},
    }]}
    assert B.reconcile_activity(parsed) == []
    parsed["accounts"][0]["activity_totals"][P.SECTION_ADDITIONS] = -4500.0
    msgs = B.reconcile_activity(parsed)
    assert len(msgs) == 1
    assert "additions_withdrawals" in msgs[0]


def test_deposit_balance_becomes_a_cash_row(tmp_path, monkeypatch):
    conn = _build_all(tmp_path, monkeypatch)[D.FAMILY_DEPOSIT]
    row = conn.execute(
        "SELECT description, market_value, instrument_key FROM "
        "historical_position_snapshots WHERE account_external_id='0000000000'"
    ).fetchone()
    assert row == (D.CASH_DESC, 1500.0, None)


def test_deposit_ledger_books_by_the_column_it_landed_in(tmp_path, monkeypatch):
    conn = _build_all(tmp_path, monkeypatch)[D.FAMILY_DEPOSIT]
    rows = conn.execute(
        "SELECT kind, amount FROM transactions "
        "WHERE account_external_id='0000000000' ORDER BY amount").fetchall()
    assert rows == [("WITHDRAWAL", -500.0), ("DEPOSIT", 2000.0)]


def test_mortgage_principal_is_carried_negative(tmp_path, monkeypatch):
    # The fidelity adapter passes market_value through unchanged, so a
    # liability that arrives positive reads as an asset of the same size.
    conn = _build_all(tmp_path, monkeypatch)[D.FAMILY_MORTGAGE]
    row = conn.execute(
        "SELECT description, market_value FROM historical_position_snapshots "
        "WHERE account_external_id='0000000002'").fetchone()
    assert row == (D.LOAN_DESC, -900000.0)


def test_a_refused_section_reaches_silver_as_nothing(tmp_path, monkeypatch):
    conn = _build_all(tmp_path, monkeypatch)[D.FAMILY_DEPOSIT]
    for table in ("historical_position_snapshots", "transactions"):
        assert conn.execute(
            f"SELECT COUNT(*) FROM {table} "
            "WHERE account_external_id='0000000001'").fetchone()[0] == 0


def test_unreadable_sections_are_named_for_the_build_to_report():
    parsed = {"accounts": [
        {"account_external_id": "0000000001", "_error": "it did not close"},
        {"account_external_id": "0000000000", "holdings": []},
    ]}
    assert B.unreadable_sections(parsed) == ["0000000001: it did not close"]


def test_derived_marks_fill_the_archive_gaps(tmp_path, monkeypatch):
    """A month the archive misses for one account but covers for its peers
    reads as that account's closure, so its value drops to zero. The advisor
    workbook's mark for that month is what fills it — marked at row level, so
    it is never mistaken for a statement's."""
    canned = {
        "SVM-000000/2022-01-31": {
            "family": P.FAMILY_BROKERAGE, "period_end": "2022-01-31",
            "stated_total": 10.0, "accounts": [
                {"account_external_id": "SVM-000000", "holdings": [
                    {"description": "AAAA", "instrument_key": "AAAA",
                     "market_value": 10.0}]}]},
        "SVM-000000/2022-03-31": {
            "family": P.FAMILY_BROKERAGE, "period_end": "2022-03-31",
            "stated_total": 30.0, "accounts": [
                {"account_external_id": "SVM-000000", "holdings": [
                    {"description": "AAAA", "instrument_key": "AAAA",
                     "market_value": 30.0}]}]},
    }
    bronze = _write_bronze(tmp_path, canned)
    _patch_parsers(monkeypatch, bronze, canned)
    monkeypatch.setattr(B.derived_marks, "read_workbook", lambda path: [
        B.derived_marks.DerivedMark("SVM-000000", "2022-02-28", 20.0),
        # Outside the covered span: must not extend the series.
        B.derived_marks.DerivedMark("SVM-000000", "2022-04-30", 40.0),
    ])
    db = tmp_path / "svb.db"
    B.build(db, bronze, signatures=(), migrations_dir=MIGRATIONS,
            cache_dir=None, max_workers=1, workbook=tmp_path / "marks.xlsx")
    conn = sqlite3.connect(str(db))
    rows = conn.execute(
        "SELECT as_of_date, market_value, description, source_sha256 FROM "
        "historical_position_snapshots ORDER BY as_of_date").fetchall()
    assert [(r[1], r[3] == DERIVED_SHA) for r in rows] == [
        (10.0, False), (20.0, True), (30.0, False)]
    assert rows[1][2] == B.derived_marks.DERIVED_DESC


def test_a_workbook_is_optional(tmp_path, monkeypatch):
    # Absent means the archive stands on its statements alone.
    conn = _build(tmp_path, monkeypatch)
    assert conn.execute(
        "SELECT COUNT(*) FROM historical_position_snapshots "
        "WHERE source_sha256=?", (DERIVED_SHA,)).fetchone()[0] == 0


def test_dump_run_marker_is_written(tmp_path, monkeypatch):
    # The fidelity gold adapter keys its change-trigger on dump_runs, so a
    # historical-only silver needs this marker for `wealthdb load` to see it.
    conn = _build(tmp_path, monkeypatch)
    run = conn.execute(
        "SELECT snapshot_at, run_dir, mode FROM dump_runs").fetchall()
    assert run == [(B.ts_from_iso("2023-01-31"), "svb-sleeves-build",
                    "historical")]


# ============================================================
# Rebuild determinism and the parse cache
# ============================================================

def _snapshot_rows(db):
    conn = sqlite3.connect(str(db))
    try:
        return (
            conn.execute(
                "SELECT as_of_date, account_external_id, description, instrument_key,"
                " quantity, price, market_value, source_sha256, payload "
                "FROM historical_position_snapshots ORDER BY 1,2,3,8").fetchall(),
            conn.execute(
                "SELECT activity_id, timestamp, account_external_id, kind, amount, "
                "payload FROM transactions ORDER BY activity_id").fetchall(),
        )
    finally:
        conn.close()


def test_force_is_accepted_noop_rebuild(tmp_path, monkeypatch):
    # svb always rebuilds the silver from bronze, so --force (accepted for
    # fleet uniformity) is a documented no-op: it parses cleanly and yields
    # the identical silver as a plain load of the same bronze. Mock the parse
    # layer in-process — main() otherwise fans out to a process pool the
    # per-file monkeypatch wouldn't reach.
    bronze = _write_bronze(tmp_path)
    monkeypatch.setattr(
        B, "parse_statements",
        lambda pdfs, shas, **kw: [dict(_CANNED[_stem(p, bronze)]) for p in pdfs])
    db = tmp_path / "svb.db"
    argv = ["--silver-db", str(db), "--bronze-dir", str(bronze)]
    assert B.main(argv) == 0
    plain = _snapshot_rows(db)
    assert B.main(argv + ["--force"]) == 0
    assert _snapshot_rows(db) == plain


# Distinct bytes per statement (unlike the shared-bytes fixtures above) so each
# maps to its own content-keyed cache entry.
_DISTINCT_STEMS = ("SVM-000000/2021-12-31", "SVM-000001/2023-01-31")


def _distinct_bronze(tmp_path, monkeypatch):
    bronze = _write_bronze(tmp_path, _DISTINCT_STEMS, distinct_bytes=True)
    calls = []

    def fake(path, expected_signatures=()):
        stem = _stem(path, bronze)
        calls.append(stem)
        return dict(_CANNED[stem])

    monkeypatch.setattr(B.pdf_parsers_svbwa, "parse_svbwa_statement_pdf", fake)
    return bronze, calls


def test_parse_cache_reuse(tmp_path, monkeypatch):
    """A warm run replays every parse from the sidecar (no parser call) and
    re-emits byte-identical silver, transactions included."""
    bronze, calls = _distinct_bronze(tmp_path, monkeypatch)
    cache_dir = tmp_path / "cache"

    cold = tmp_path / "cold.db"
    B.build(cold, bronze, signatures=(), migrations_dir=MIGRATIONS,
            cache_dir=cache_dir, max_workers=1)
    assert sorted(calls) == sorted(_DISTINCT_STEMS)  # cold: each parsed once
    assert (cache_dir / B._PARSE_CACHE_FILE).is_file()

    calls.clear()
    warm = tmp_path / "warm.db"
    B.build(warm, bronze, signatures=(), migrations_dir=MIGRATIONS,
            cache_dir=cache_dir, max_workers=1)
    assert calls == [], "warm run must not re-parse any statement"
    assert _snapshot_rows(cold) == _snapshot_rows(warm)


def test_parser_logic_change_invalidates_cache(tmp_path, monkeypatch):
    """The cache key folds in _parser_logic_fingerprint (both parsers' import
    closure plus both extraction stacks' versions), so any change to it —
    a parser edit or an extraction-stack upgrade — forces a re-parse even though
    the statement bytes are unchanged. What actually moves the fingerprint is
    covered by collectorkit's test_srcfp."""
    bronze, calls = _distinct_bronze(tmp_path, monkeypatch)
    cache_dir = tmp_path / "cache"
    B.build(tmp_path / "a.db", bronze, signatures=(), migrations_dir=MIGRATIONS,
            cache_dir=cache_dir, max_workers=1)
    calls.clear()

    # A different fingerprint (parser edit or library upgrade) misses every
    # prior entry.
    monkeypatch.setattr(B, "_parser_logic_fingerprint", lambda: "different-fingerprint")
    B.build(tmp_path / "b.db", bronze, signatures=(), migrations_dir=MIGRATIONS,
            cache_dir=cache_dir, max_workers=1)
    assert sorted(calls) == sorted(_DISTINCT_STEMS), "changed fingerprint must re-parse"


def test_parse_worker_is_picklable():
    """The pool target must be module-level so it pickles under spawn."""
    import pickle

    assert pickle.loads(pickle.dumps(B._parse_statement)) is B._parse_statement


def _rows(path, table):
    conn = sqlite3.connect(str(path))
    try:
        return conn.execute(f"SELECT COUNT(*) FROM {table}").fetchone()[0]
    finally:
        conn.close()


_BROKERAGE_ONLY = [s for s, c in _CANNED.items()
                   if c["family"] == P.FAMILY_BROKERAGE]


def test_a_family_the_archive_lacks_gets_no_silver(tmp_path, monkeypatch):
    """An archive holding one family alone must not get a DB for the others:
    each would be an empty source, indistinguishable from a family whose
    statements had all been withdrawn."""
    bronze = _write_bronze(tmp_path, stems=_BROKERAGE_ONLY)
    _patch_parsers(monkeypatch, bronze)
    db = tmp_path / "svb-second.db"
    B.build(db, bronze, signatures=(), migrations_dir=MIGRATIONS,
            cache_dir=None, max_workers=1)
    paths = B.silver_paths(db)
    assert paths[P.FAMILY_BROKERAGE].exists()
    assert not paths[D.FAMILY_DEPOSIT].exists()
    assert not paths[D.FAMILY_MORTGAGE].exists()


def test_a_family_that_drops_out_is_emptied_not_left_stale(tmp_path, monkeypatch):
    """A silver that already exists is rebuilt even when its family leaves
    bronze, so its gold source zeroes rather than standing at the last load's
    values — and a path the config already names never goes missing."""
    bronze = _write_bronze(tmp_path)  # every family
    _patch_parsers(monkeypatch, bronze)
    db = tmp_path / "svb.db"
    B.build(db, bronze, signatures=(), migrations_dir=MIGRATIONS,
            cache_dir=None, max_workers=1)
    deposit = B.silver_paths(db)[D.FAMILY_DEPOSIT]
    assert _rows(deposit, "historical_position_snapshots")

    later = _write_bronze(tmp_path / "again", stems=_BROKERAGE_ONLY)
    _patch_parsers(monkeypatch, later)
    B.build(db, later, signatures=(), migrations_dir=MIGRATIONS,
            cache_dir=None, max_workers=1)
    assert deposit.exists()
    assert _rows(deposit, "historical_position_snapshots") == 0
