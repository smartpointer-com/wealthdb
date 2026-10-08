"""
Unit tests for the closed-lots load (migrations 0010, 0012): the
Consolidated 1099 pass and the supplied statements' sales.

The 1099 parser is replaced by a stub that reads each fixture "PDF" as
JSON naming the form it is, so a test can lay down any sequence of
copies, corrections and dumps without a real PDF. Every account
number, security and amount is invented.
"""

from __future__ import annotations

import json
import shutil
import sqlite3
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import load  # noqa: E402
import pdf_parsers_1099  # noqa: E402

MIGRATIONS_DIR = Path(__file__).resolve().parent.parent / "migrations"
ACCT = "100000001"


@pytest.fixture
def migrated():
    c = sqlite3.connect(":memory:")
    load.apply_migrations(c, MIGRATIONS_DIR)
    yield c
    c.close()


# ============================================================
# Consolidated 1099 — a stub parser over JSON fixtures
# ============================================================

def _lot(proceeds, cost, **extra):
    return {
        "form_8949_box": "A", "term": "short", "covered": True,
        "description": "EXAMPLE CORP COM", "symbol": "EXMP",
        "cusip": "000000AA0", "action": "Sale", "corrected": False,
        "quantity": 1.0, "acquired_date": "2025-01-02",
        "date_sold": "2025-03-04", "proceeds": proceeds, "cost_basis": cost,
        "accrued_market_discount": None, "wash_sale_disallowed": None,
        "gain_loss": proceeds - cost, "federal_tax_withheld": None,
        "footnotes": {}, **extra,
    }


@pytest.fixture
def stub_1099(monkeypatch):
    """Read each fixture PDF as the JSON form it names; record which
    paths were parsed in full."""
    parsed = []

    def identity(path):
        form = json.loads(Path(path).read_text())
        if form.get("broken"):
            raise ValueError("unreadable")
        return {k: form.get(k) for k in (
            "tax_year", "account_external_id", "prepared", "corrected")} | {
            "path": str(path)}

    def full(path):
        form = json.loads(Path(path).read_text())
        if form.get("broken_lots"):
            raise ValueError("unreadable")
        parsed.append(Path(path).parent.parent.name + "/" + Path(path).name)
        return identity(path) | {"lots": form["lots"]}

    monkeypatch.setattr(pdf_parsers_1099, "read_consolidated_1099_identity",
                        identity)
    monkeypatch.setattr(pdf_parsers_1099, "parse_consolidated_1099_pdf", full)
    return parsed


def _form(dump, name, *, prepared, lots, year=2025, acct=ACCT,
          copy=0, **extra):
    """Lay one Consolidated 1099 copy into ``dump/documents``. ``copy``
    varies the bytes the way a re-rendered download does."""
    docs = dump / "documents"
    docs.mkdir(parents=True, exist_ok=True)
    (docs / name).write_text(json.dumps({
        "tax_year": year, "account_external_id": acct, "prepared": prepared,
        "corrected": extra.pop("corrected", None), "lots": lots,
        "copy": copy, **extra}))
    return dump


def _held(conn):
    return conn.execute(
        "SELECT tax_year, form_prepared, proceeds, cost_basis, source_sha256 "
        "FROM closed_lots WHERE document_kind = 'form_1099b' "
        "ORDER BY tax_year, proceeds").fetchall()


def test_a_corrected_form_replaces_the_original(migrated, tmp_path, stub_1099):
    d1 = _form(tmp_path / "20260301T000000Z", "Consolidated_Form_1099.pdf",
               prepared="2026-02-15", lots=[_lot(100.0, 90.0), _lot(50.0, 60.0)])
    d2 = _form(tmp_path / "20260501T000000Z",
               "CORRECTED_Consolidated_Form_1099.pdf", prepared="2026-03-19",
               corrected="2026-03-23", lots=[_lot(100.0, 80.0)])
    _form(d2, "Consolidated_Form_1099.pdf", prepared="2026-02-15", copy=1,
          lots=[_lot(100.0, 90.0), _lot(50.0, 60.0)])

    load._load_consolidated_1099s(migrated, [d1, d2], rederive=True)

    rows = _held(migrated)
    assert [(r[1], r[2], r[3]) for r in rows] == [("2026-03-19", 100.0, 80.0)]
    assert stub_1099 == ["20260501T000000Z/CORRECTED_Consolidated_Form_1099.pdf"]


def test_copies_of_one_form_keep_the_later_dumps(migrated, tmp_path, stub_1099):
    d1 = _form(tmp_path / "20260301T000000Z", "Consolidated_Form_1099.pdf",
               prepared="2026-02-15", lots=[_lot(100.0, 90.0)])
    d2 = _form(tmp_path / "20260302T000000Z", "Consolidated_Form_1099__1.pdf",
               prepared="2026-02-15", copy=1, lots=[_lot(100.0, 90.0)])

    load._load_consolidated_1099s(migrated, [d1, d2], rederive=True)

    assert stub_1099 == ["20260302T000000Z/Consolidated_Form_1099__1.pdf"]
    assert len(_held(migrated)) == 1


def test_a_form_already_held_is_not_parsed_again(migrated, tmp_path, stub_1099):
    d1 = _form(tmp_path / "20260301T000000Z", "Consolidated_Form_1099.pdf",
               prepared="2026-02-15", lots=[_lot(100.0, 90.0)])
    load._load_consolidated_1099s(migrated, [d1], rederive=True)
    before = _held(migrated)

    # The nightly download serves the same form again, re-rendered.
    d2 = _form(tmp_path / "20260302T000000Z", "Consolidated_Form_1099.pdf",
               prepared="2026-02-15", copy=1, lots=[_lot(100.0, 90.0)])
    load._load_consolidated_1099s(migrated, [d2])

    assert len(stub_1099) == 1
    assert _held(migrated) == before


def test_an_older_form_never_replaces_a_newer_one(migrated, tmp_path, stub_1099):
    new = _form(tmp_path / "20260501T000000Z", "CORRECTED_Consolidated_Form_1099.pdf",
                prepared="2026-03-19", lots=[_lot(100.0, 80.0)])
    load._load_consolidated_1099s(migrated, [new], rederive=True)
    old = _form(tmp_path / "20260502T000000Z", "Consolidated_Form_1099.pdf",
                prepared="2026-02-15", lots=[_lot(100.0, 90.0)])
    load._load_consolidated_1099s(migrated, [old])
    assert [r[3] for r in _held(migrated)] == [80.0]


def test_forms_for_other_years_and_accounts_are_left_alone(
        migrated, tmp_path, stub_1099):
    d1 = _form(tmp_path / "20260301T000000Z", "Consolidated_Form_1099.pdf",
               prepared="2025-02-15", year=2024, lots=[_lot(10.0, 9.0)])
    _form(d1, "Consolidated_Form_1099__1.pdf", prepared="2026-02-15",
          lots=[_lot(100.0, 90.0)])
    _form(d1, "Consolidated_Form_1099__2.pdf", prepared="2026-02-15",
          acct="100000002", lots=[_lot(5.0, 4.0)])
    load._load_consolidated_1099s(migrated, [d1], rederive=True)

    d2 = _form(tmp_path / "20260501T000000Z", "CORRECTED_Consolidated_Form_1099.pdf",
               prepared="2026-03-19", lots=[_lot(100.0, 80.0)])
    load._load_consolidated_1099s(migrated, [d2])

    assert migrated.execute(
        "SELECT account_external_id, tax_year, cost_basis FROM closed_lots "
        "ORDER BY 1, 2").fetchall() == [
        (ACCT, 2024, 9.0), (ACCT, 2025, 80.0), ("100000002", 2025, 4.0)]


def test_two_identical_lots_on_one_form_stay_two(migrated, tmp_path, stub_1099):
    d1 = _form(tmp_path / "20260301T000000Z", "Consolidated_Form_1099.pdf",
               prepared="2026-02-15", lots=[_lot(100.0, 90.0)] * 2)
    load._load_consolidated_1099s(migrated, [d1], rederive=True)
    assert len(_held(migrated)) == 2


def test_lot_columns_are_promoted(migrated, tmp_path, stub_1099):
    lot = _lot(400.0, 500.0, form_8949_box="E", term="long", covered=False,
               symbol=None, acquired_date="Various", wash_sale_disallowed=25.0,
               accrued_market_discount=1.5, federal_tax_withheld=4.0,
               corrected=True)
    d1 = _form(tmp_path / "20260301T000000Z", "Consolidated_Form_1099.pdf",
               prepared="2026-02-15", lots=[lot])
    load._load_consolidated_1099s(migrated, [d1], rederive=True)
    row = migrated.execute(
        "SELECT instrument_key, cusip, acquired_date, disposed_date, term, "
        "covered, form_8949_box, wash_sale_disallowed, "
        "accrued_market_discount, federal_tax_withheld, realized_gain_loss, "
        "corrected, settlement_date, specific_share_id, currency "
        "FROM closed_lots").fetchone()
    assert row == ("000000AA0", "000000AA0", "Various", "2025-03-04", "LONG",
                   0, "E", 25.0, 1.5, 4.0, -100.0, 1, None, None, "USD")


def test_rederiving_drops_the_rows_of_an_earlier_parser(
        migrated, tmp_path, stub_1099):
    d1 = _form(tmp_path / "20260301T000000Z", "Consolidated_Form_1099.pdf",
               prepared="2026-02-15", lots=[_lot(100.0, 90.0)])
    load._load_consolidated_1099s(migrated, [d1], rederive=True)
    # A lot an earlier parser read differently, under another id.
    migrated.execute(
        "UPDATE closed_lots SET lot_id = 'stale', cost_basis = 1.0")
    migrated.commit()

    load._load_consolidated_1099s(migrated, [d1], rederive=True)

    assert [r[3] for r in _held(migrated)] == [90.0]


def test_a_run_where_nothing_parses_keeps_the_rows_and_the_stamp_owed(
        migrated, tmp_path, stub_1099):
    d1 = _form(tmp_path / "20260301T000000Z", "Consolidated_Form_1099.pdf",
               prepared="2026-02-15", lots=[_lot(100.0, 90.0)])
    load._load_consolidated_1099s(migrated, [d1], rederive=True)
    migrated.execute("DELETE FROM parser_generations")
    migrated.commit()

    _form(d1, "Consolidated_Form_1099.pdf", prepared="2026-02-15",
          lots=[], broken_lots=True)
    load._load_consolidated_1099s(migrated, [d1], rederive=True)

    assert len(_held(migrated)) == 1
    assert migrated.execute(
        "SELECT COUNT(*) FROM parser_generations").fetchone()[0] == 0


def test_a_form_that_names_no_account_is_skipped(migrated, tmp_path, stub_1099):
    d1 = _form(tmp_path / "20260301T000000Z", "Consolidated_Form_1099.pdf",
               prepared="2026-02-15", acct=None, lots=[_lot(100.0, 90.0)])
    _form(d1, "Consolidated_Form_1099__1.pdf", prepared="2026-02-15",
          lots=[], broken=True)
    load._load_consolidated_1099s(migrated, [d1], rederive=True)
    assert _held(migrated) == []


def test_the_pass_reads_every_dump_only_when_its_parser_moved(
        migrated, tmp_path, stub_1099):
    dumps, pending = ["d1", "d2"], ["d2"]
    assert load._dumps_for_1099s(migrated, dumps, pending, 10) == (dumps, True)

    load.silver.stamp_generation(
        migrated, load.CONSOLIDATED_1099_GENERATION_SCOPE,
        load._CONSOLIDATED_1099_PARSER_VERSION)
    assert load._dumps_for_1099s(migrated, dumps, pending, 10) == (pending, False)
    assert load._dumps_for_1099s(migrated, dumps, pending, 9) == ([], False)


def test_a_completed_pass_stamps_its_generation(migrated, tmp_path, stub_1099):
    d1 = _form(tmp_path / "20260301T000000Z", "Consolidated_Form_1099.pdf",
               prepared="2026-02-15", lots=[_lot(100.0, 90.0)])
    load._load_consolidated_1099s(migrated, [d1], rederive=True)
    assert not load.silver.stale_generation(
        migrated, load.CONSOLIDATED_1099_GENERATION_SCOPE,
        load._CONSOLIDATED_1099_PARSER_VERSION)


def test_both_namings_of_a_consolidated_1099_are_candidates(tmp_path):
    docs = tmp_path / "documents"
    docs.mkdir()
    for name in ("2024-Placeholder-0001-Consolidated-Form-1099.pdf",
                 "2025-Placeholder-0001-CORRECTED-Consolidated-Form-1099.pdf",
                 "CORRECTED_Consolidated_Form_1099__1.pdf",
                 "Consolidated_Form_1099.pdf",
                 "Form_1099_Q_Instructions.pdf",
                 "Statement_Jan_March_2026.pdf"):
        (docs / name).write_bytes(b"%PDF")
    assert [p.name for p in load._consolidated_1099_candidates(tmp_path)] == [
        "2024-Placeholder-0001-Consolidated-Form-1099.pdf",
        "2025-Placeholder-0001-CORRECTED-Consolidated-Form-1099.pdf",
        "CORRECTED_Consolidated_Form_1099__1.pdf",
        "Consolidated_Form_1099.pdf",
    ]


# ============================================================
# Tax-form documents
# ============================================================

def test_every_1099_naming_is_filed_as_a_tax_form():
    assert load._classify_documents_pdf("Consolidated_Form_1099__2.pdf") == {
        "file_format": "pdf", "doc_kind": "tax_form"}
    assert load._classify_documents_pdf(
        "2025-Placeholder-0001-Consolidated-Form-1099.pdf")["tax_year"] == 2025
    assert load._classify_documents_pdf(
        "Statement_Jan_March_2026.pdf")["doc_kind"] == "statement"


def test_migration_0010_refiles_1099s_catalogued_as_statements(tmp_path):
    old = tmp_path / "migrations"
    old.mkdir()
    for f in sorted(MIGRATIONS_DIR.glob("000*.sql")):
        (old / f.name).write_text(f.read_text(encoding="utf-8"))
    conn = sqlite3.connect(":memory:")
    load.apply_migrations(conn, old)
    for sha, name in (("a", "Consolidated_Form_1099__1.pdf"),
                      ("b", "Form_1099_Q_Instructions.pdf"),
                      ("c", "Statement_Jan_March_2026.pdf")):
        conn.execute(
            "INSERT INTO documents (content_sha256, snapshot_at, file_path, "
            "file_name, size_bytes, doc_kind, file_format, payload) "
            "VALUES (?, 0, ?, ?, 1, 'statement', 'pdf', '{}')",
            (sha, name, name))
    conn.commit()

    load.apply_migrations(conn, MIGRATIONS_DIR)

    assert conn.execute(
        "SELECT content_sha256, doc_kind FROM documents ORDER BY 1"
    ).fetchall() == [("a", "tax_form"), ("b", "tax_form"), ("c", "statement")]


def _catalogue(conn, dump):
    """File each PDF of ``dump`` into `documents` the way a dump load does."""
    for path in sorted((dump / "documents").glob("*.pdf")):
        load._ingest_document(conn, 1000, path,
                              load._classify_documents_pdf(path.name))
    conn.commit()


def _documents(conn):
    return conn.execute(
        "SELECT file_name, account_external_id, tax_year FROM documents "
        "ORDER BY file_name").fetchall()


def test_each_1099_copy_is_filed_under_the_account_and_year_it_states(
        migrated, tmp_path, stub_1099):
    d1 = _form(tmp_path / "20260301T000000Z", "Consolidated_Form_1099.pdf",
               prepared="2026-02-15", lots=[_lot(100.0, 90.0)])
    # A copy of the same form, which is not parsed for its lots.
    _form(d1, "Consolidated_Form_1099__1.pdf", prepared="2026-02-15",
          copy=1, lots=[_lot(100.0, 90.0)])
    # The older naming states the year, which stays as the name states it.
    _form(d1, "2024-Placeholder-0002-Consolidated-Form-1099.pdf",
          prepared="2025-02-15", year=2023, acct="100000002", lots=[])
    # A form that states no account, and one that does not read at all.
    _form(d1, "Consolidated_Form_1099__2.pdf", prepared="2026-02-15",
          acct=None, lots=[])
    _form(d1, "Consolidated_Form_1099__3.pdf", prepared="2026-02-15",
          lots=[], broken=True)
    (d1 / "documents" / "Form_1099_Q_Instructions.pdf").write_bytes(b"%PDF")
    _catalogue(migrated, d1)

    load._load_consolidated_1099s(migrated, [d1], rederive=True)

    assert _documents(migrated) == [
        ("2024-Placeholder-0002-Consolidated-Form-1099.pdf", "100000002", 2024),
        ("Consolidated_Form_1099.pdf", ACCT, 2025),
        ("Consolidated_Form_1099__1.pdf", ACCT, 2025),
        ("Consolidated_Form_1099__2.pdf", None, 2025),
        ("Consolidated_Form_1099__3.pdf", None, None),
        ("Form_1099_Q_Instructions.pdf", None, None),
    ]


def test_a_copy_already_held_is_filed_by_a_later_pass(
        migrated, tmp_path, stub_1099):
    d1 = _form(tmp_path / "20260301T000000Z", "Consolidated_Form_1099.pdf",
               prepared="2026-02-15", lots=[_lot(100.0, 90.0)])
    _catalogue(migrated, d1)
    load._load_consolidated_1099s(migrated, [d1], rederive=True)
    # A silver whose copies were catalogued before the pass filed them.
    migrated.execute("UPDATE documents SET account_external_id = NULL, "
                     "tax_year = NULL")
    migrated.commit()

    load._load_consolidated_1099s(migrated, [d1])

    assert _documents(migrated) == [("Consolidated_Form_1099.pdf", ACCT, 2025)]
    assert len(stub_1099) == 1


def test_migration_0013_sends_the_1099_pass_over_every_dump(tmp_path):
    old = tmp_path / "migrations"
    old.mkdir()
    for mig in sorted(MIGRATIONS_DIR.glob("*.sql")):
        if mig.name < "0013":
            shutil.copy(mig, old)
    conn = sqlite3.connect(":memory:")
    load.apply_migrations(conn, old)
    for scope in (load.CONSOLIDATED_1099_GENERATION_SCOPE,
                  load.SUPPLIED_GENERATION_SCOPE):
        load.silver.stamp_generation(conn, scope, "generation")
    conn.commit()

    load.apply_migrations(conn, MIGRATIONS_DIR)

    assert load._dumps_for_1099s(conn, ["d1", "d2"], ["d2"], 13) == (
        ["d1", "d2"], True)
    assert conn.execute("SELECT scope FROM parser_generations").fetchall() == [
        (load.SUPPLIED_GENERATION_SCOPE,)]


# ============================================================
# Supplied statements — the sales
# ============================================================

def _sale(**extra):
    return {
        "settlement_date": "2026-03-04", "description": "SAMPLE INDS INC",
        "symbol": "000000BB0", "action": "You Sold",
        "specific_share_id": True, "quantity": 4.0, "price": 25.0,
        "cost_basis": 80.0, "transaction_cost": -0.02, "amount": 99.98,
        "term": "short", "gain_loss": 19.98,
        "cells": ["-4.000", "25.00000", "80.00", "-0.02", "99.98"],
        "terms": [["short", 19.98]], **extra,
    }


def _statement(*sales):
    return {"period_end": "2026-03-31", "accounts": [{
        "account_external_id": ACCT, "holdings": [], "activity": [],
        "sales": list(sales)}]}


def test_a_statement_sale_lands_as_a_closed_lot(migrated):
    n = load._insert_supplied_closed_lots(
        migrated, Path("Placeholder 3.26 Statement.PDF"), _statement(_sale()),
        "sha0")
    assert n == 1
    row = migrated.execute(
        "SELECT document_kind, account_external_id, instrument_key, action, "
        "quantity, proceeds, cost_basis, fees, realized_gain_loss, term, "
        "specific_share_id, settlement_date, acquired_date, disposed_date, "
        "tax_year, covered, source_sha256 FROM closed_lots").fetchone()
    assert row == ("statement", ACCT, "000000BB0", "You Sold", 4.0,
                   99.98, 80.0, -0.02, 19.98, "SHORT", 1, "2026-03-04",
                   None, None, None, None, "sha0")
    # The price is quoted per unit or in percent of par; it stays in payload.
    payload = json.loads(migrated.execute(
        "SELECT payload FROM closed_lots").fetchone()[0])
    assert (payload["price"], payload["cells"][2]) == (25.0, "80.00")


def test_a_statement_read_twice_converges(migrated):
    for _ in range(2):
        load._insert_supplied_closed_lots(
            migrated, Path("p.PDF"), _statement(_sale(), _sale()), "sha0")
    # Two identical sales on one statement stay two; a re-read adds none.
    assert migrated.execute("SELECT COUNT(*) FROM closed_lots").fetchone()[0] == 2


def test_the_statement_purge_spares_the_1099_lots(migrated, tmp_path, stub_1099):
    d1 = _form(tmp_path / "20260301T000000Z", "Consolidated_Form_1099.pdf",
               prepared="2026-02-15", lots=[_lot(100.0, 90.0)])
    load._load_consolidated_1099s(migrated, [d1], rederive=True)
    load._insert_supplied_closed_lots(
        migrated, Path("p.PDF"), _statement(_sale()), "sha0")

    assert load._drop_supplied_closed_lots(migrated) == 1

    assert migrated.execute(
        "SELECT document_kind FROM closed_lots").fetchall() == [
            ("form_1099b",)]


def test_migration_0012_renames_rows_written_before_it(tmp_path):
    """A silver at schema 10 keeps every closed lot through 0012, under
    schwab-web's names and values."""
    old = tmp_path / "migrations"
    old.mkdir()
    for mig in sorted(MIGRATIONS_DIR.glob("*.sql")):
        if mig.name < "0011":
            shutil.copy(mig, old)
    conn = sqlite3.connect(":memory:")
    load.apply_migrations(conn, old)
    conn.execute(
        "INSERT INTO closed_lots (lot_id, document_kind, account_external_id, "
        "description, action, date_sold, gain_loss, term, source_sha256, "
        "payload) VALUES ('1099b_x', '1099b', ?, 'EXAMPLE CORP', 'Sale', "
        "'2025-03-04', -5.0, 'long', 'sha', '{}')", (ACCT,))
    load.apply_migrations(conn, MIGRATIONS_DIR)
    assert conn.execute(
        "SELECT lot_id, document_kind, security_name, disposed_date, "
        "realized_gain_loss, term FROM closed_lots").fetchall() == [
        ("1099b_x", "form_1099b", "EXAMPLE CORP", "2025-03-04", -5.0, "LONG")]
