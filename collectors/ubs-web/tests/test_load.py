"""Bronze→silver tests for the ubs-web collector's load.py.

Seeds a minimal synthetic UBS web positions export (the semicolon
CSV with the "Valued in:" footer) and runs the positions loader,
asserting the relationship / portfolio / position silver rows.
Synthetic relationship prefix / portfolio / ISIN only — NOT a real
banking relationship.
"""
from __future__ import annotations

import itertools
import sqlite3
import sys
from pathlib import Path

import pytest

HERE = Path(__file__).resolve().parent
COLLECTOR = HERE.parent
sys.path.insert(0, str(COLLECTOR))

import load as loader  # noqa: E402
from collectorkit import bronze, silver  # noqa: E402

REL = "1234 00000001"   # synthetic "<4-digit branch> <8-digit base>"

HEADER = ";".join(loader.POSITIONS_COLS)
# One securities holding row (ISIN + Number/Amt + Market value, no IBAN).
ROW = ("1234 00000001;Portfolio ABC;Equities;Share;CHF;10;;12345;"
       "CH0000000001;;;;;Example Share;;;;;;;;;;;;;;;1500.00;;;")
CSV = HEADER + "\r\n" + ROW + "\r\nValued in: CHF\r\n"


MIGRATIONS_DIR = COLLECTOR / "migrations"


def _fresh_db(tmp_path: Path) -> sqlite3.Connection:
    conn = sqlite3.connect(str(tmp_path / "ubs-web.db"))
    conn.row_factory = sqlite3.Row
    silver.apply_migrations(conn, MIGRATIONS_DIR)
    return conn


def _seed_bronze(root: Path) -> Path:
    dump = root / "20240101T000000Z"
    (dump / "positions").mkdir(parents=True, exist_ok=True)
    (dump / "positions" / "positions_test.csv").write_text(
        CSV, encoding="utf-8")
    return dump


def test_load_positions(tmp_path):
    dump = _seed_bronze(tmp_path / "bronze")
    conn = _fresh_db(tmp_path)
    with conn:
        n = loader._load_positions(conn, 1700000000, dump)
    assert n == 1

    # banking_relationships row from the synthetic prefix.
    rel = conn.execute(
        "SELECT banking_relationship_id FROM banking_relationships").fetchall()
    assert len(rel) == 1
    assert rel[0]["banking_relationship_id"] == REL

    # portfolios row linked to the relationship.
    port = conn.execute(
        "SELECT banking_relationship_id, base_currency FROM portfolios").fetchall()
    assert len(port) == 1
    assert port[0]["banking_relationship_id"] == REL
    assert port[0]["base_currency"] == "CHF"

    # position row keyed by ISIN.
    pos = conn.execute("SELECT instrument_isin FROM positions").fetchall()
    assert len(pos) == 1
    assert pos[0]["instrument_isin"] == "CH0000000001"


def test_positions_base_currency_footer(tmp_path):
    dump = _seed_bronze(tmp_path / "bronze")
    csv_path = dump / "positions" / "positions_test.csv"
    assert loader._read_positions_base_currency(csv_path) == "CHF"


# ---- mortgage rows ('Pro memoria - Mortgages') --------------------

# A synthetic mortgage row: Group of products = 'Pro memoria -
# Mortgages', the IBAN column carries a 'dd.mm.yyyy - dd.mm.yyyy'
# term, Number/Amt. is the negative principal.
# Obviously-synthetic placeholder term + principal. NOT the real
# the mortgage dates / balance.
SYN_TERM = "01.01.2020 - 31.12.2024"
SYN_PRINCIPAL = "-1234567.89"


def _mortgage_row(rate_descriptor: str = "UBS Fixed-Rate Mortgage",
                  term: str = SYN_TERM) -> str:
    cells = {
        "Banking relationship": REL,
        "Portfolio": "1234 00000001 R001",
        "Group of products": "Pro memoria - Mortgages",
        "Product": "1234 00000001.MMM 0000",
        "Ccy.": "CHF",
        "Number/Amt.": SYN_PRINCIPAL,
        "Description": f"{rate_descriptor}, EXAMPLE ROAD 1, 0000 EXAMPLECITY",
        "Description 1": rate_descriptor,
        "Description 2": "Properties",
        "Description 3": "EXAMPLE ROAD 1, 0000 EXAMPLECITY",
        "IBAN": term,
    }
    return ";".join(cells.get(col, "") for col in loader.POSITIONS_COLS)


def test_load_mortgage_row(tmp_path):
    """A 'Pro memoria - Mortgages' row should be inserted into the
    `mortgages` table (not `accounts` / `positions`), with the
    fixed-rate term parsed out of the IBAN column."""
    dump = tmp_path / "bronze" / "20240101T000000Z"
    (dump / "positions").mkdir(parents=True)
    (dump / "positions" / "positions_test.csv").write_text(
        ";".join(loader.POSITIONS_COLS) + "\r\n"
        + _mortgage_row() + "\r\n"
        + "Valued in: CHF\r\n",
        encoding="utf-8",
    )
    conn = _fresh_db(tmp_path)
    with conn:
        loader._load_positions(conn, 1700000000, dump)

    mort = conn.execute(
        "SELECT account_external_id, currency_iso, outstanding_balance, "
        "       start_date, end_date, rate_type "
        "  FROM mortgages"
    ).fetchall()
    assert len(mort) == 1
    row = mort[0]
    assert row["account_external_id"] == "1234 00000001.MMM 0000"
    assert row["currency_iso"] == "CHF"
    assert row["outstanding_balance"] == float(SYN_PRINCIPAL)
    # Round-trip via datetime to assert calendar parsing rather than
    # hard-coding a Unix epoch (silently mis-asserting on leap-year
    # arithmetic).
    from datetime import datetime, timezone
    assert datetime.fromtimestamp(row["start_date"], timezone.utc).date() \
        == datetime(2020, 1, 1).date()
    assert datetime.fromtimestamp(row["end_date"], timezone.utc).date() \
        == datetime(2024, 12, 31).date()
    assert row["rate_type"] == "fixed"

    # And the row must not have leaked into accounts / positions.
    assert conn.execute("SELECT COUNT(*) FROM accounts").fetchone()[0] == 0
    assert conn.execute("SELECT COUNT(*) FROM positions").fetchone()[0] == 0


def test_load_mortgage_variable_rate(tmp_path):
    """The variable-rate variant detected via Description 1."""
    dump = tmp_path / "bronze" / "20240101T000000Z"
    (dump / "positions").mkdir(parents=True)
    (dump / "positions" / "positions_test.csv").write_text(
        ";".join(loader.POSITIONS_COLS) + "\r\n"
        + _mortgage_row(
            rate_descriptor="UBS Variable-Rate Mortgage",
            term="",  # variable-rate has no fixed term
        ) + "\r\n"
        + "Valued in: CHF\r\n",
        encoding="utf-8",
    )
    conn = _fresh_db(tmp_path)
    with conn:
        loader._load_positions(conn, 1700000000, dump)
    row = conn.execute(
        "SELECT rate_type, start_date, end_date FROM mortgages"
    ).fetchone()
    assert row["rate_type"] == "variable"
    assert row["start_date"] is None
    assert row["end_date"] is None


# ================================================================
# Pre-2024 transaction backfill: per-account MT940 cut-over + dedup
# ================================================================

# Synthetic IBANs (all-zero placeholders, valid CH-IBAN shape).
_IBAN_A = "CH0000000000000000001"   # has an MT940 floor (2024-01-02)
_IBAN_B = "CH0000000000000000002"   # NO MT940 coverage → PDF owns all


def _write_cash_csv(dump: Path, iban_spaced: str, from_date: str,
                    hash_id: str) -> None:
    """Write a minimal synthetic MT940 cash CSV carrying just the
    IBAN: and From: metadata lines the floor scanner reads."""
    tdir = dump / "transactions"
    tdir.mkdir(parents=True, exist_ok=True)
    body = (
        f"Product:;UBS personal account;\r\n"
        f"IBAN:;{iban_spaced};\r\n"
        f"From:;{from_date};\r\n"
        f"To:;2026-01-01;\r\n"
        "Trade date;Booking date;Value date;Currency;Debit;Credit;"
        "Transaction no.;Description1;Description2\r\n"
    )
    (tdir / f"cash_{hash_id}_x.csv").write_text(body, encoding="utf-8-sig")


def _mv(account: str, booking: int, *, credit=None, debit=None,
        desc="CREDIT", occ=0, reconciled=True, value=None) -> dict:
    return {
        "booking_date": booking,
        "value_date": value if value is not None else booking,
        "account_external_id": account,
        "currency_iso": "CHF",
        "amount_debit": debit,
        "amount_credit": credit,
        "description_kind": desc,
        "counterparty": None,
        "counter_account": None,
        "occurrence": occ,
        "reconciled": reconciled,
        "payload": "{}",
    }


# 2024-01-02 and a pre/post reference point, as Unix seconds UTC.
_FLOOR = loader.ts_from_iso("2024-01-02")
_PRE = loader.ts_from_iso("2023-12-15")    # below the floor
_POST = loader.ts_from_iso("2024-03-06")   # at/after the floor


def test_mt940_floors_by_account(tmp_path: Path):
    root = tmp_path / "bronze"
    # Account A appears in two dumps; take the EARLIEST From:.
    _write_cash_csv(root / "20260101T000000Z", "CH00 0000 0000 0000 0000 1",
                    "2024-01-02", "aaaa")
    _write_cash_csv(root / "20260201T000000Z", "CH00 0000 0000 0000 0000 1",
                    "2026-02-20", "bbbb")
    floors = loader._mt940_floors_by_account(root / "20260201T000000Z")
    assert floors.get(_IBAN_A) == loader.ts_from_iso("2024-01-02")
    # Account B never appears → no floor (PDF owns all its history).
    assert _IBAN_B not in floors


def test_cutover_gate_per_account(tmp_path: Path):
    conn = _fresh_db(tmp_path)
    floors = {_IBAN_A: _FLOOR}   # A has a floor; B absent
    rows = [
        _mv(_IBAN_A, _PRE, credit=100.0),    # below floor → INGEST
        _mv(_IBAN_A, _POST, credit=200.0),   # at/after floor → SKIP (MT940)
        _mv(_IBAN_B, _POST, credit=300.0),   # B has no floor → INGEST
    ]
    with conn:
        n, rejected = loader._insert_hist_transactions(conn, 1, rows, floors)
    assert rejected == 0
    got = conn.execute(
        "SELECT account_external_id, booking_date, amount_credit "
        "FROM transactions ORDER BY amount_credit").fetchall()
    assert n == 2
    assert [r["amount_credit"] for r in got] == [100.0, 300.0]


def test_reconciliation_failure_rejects_whole_statement(tmp_path: Path):
    conn = _fresh_db(tmp_path)
    rows = [_mv(_IBAN_A, _PRE, credit=100.0, reconciled=False),
            _mv(_IBAN_A, _PRE, debit=50.0, reconciled=False, occ=1)]
    with conn:
        n, rejected = loader._insert_hist_transactions(conn, 1, rows, {})
    assert n == 0 and rejected == 1
    assert conn.execute("SELECT COUNT(*) c FROM transactions").fetchone()["c"] == 0


def test_content_id_dedups_overlapping_statements(tmp_path: Path):
    """The same booking on the monthly AND the annual statement (or in
    a post-closing trailer) must collapse to one row; two genuinely
    distinct identical same-day bookings must both survive."""
    conn = _fresh_db(tmp_path)
    same = dict(account=_IBAN_A, booking=_PRE, credit=100.0, desc="CREDIT")
    with conn:
        loader._insert_hist_transactions(conn, 1, [_mv(**same, occ=0)], {})
        # Re-ingest identical row (as if from the annual statement).
        loader._insert_hist_transactions(conn, 2, [_mv(**same, occ=0)], {})
        # A genuinely distinct second identical same-day booking (occ=1).
        loader._insert_hist_transactions(conn, 2, [_mv(**same, occ=1)], {})
    assert conn.execute(
        "SELECT COUNT(*) c FROM transactions").fetchone()["c"] == 2


def _leg(account: str, booking: int, *, debit, index, count,
         parent_debit, desc="MULTI E-BANKING ORDER", occ=0) -> dict:
    """One payment out of a batch the parser split: it carries its own
    share, and the batch total the movement is identified by."""
    r = _mv(account, booking, debit=debit, desc=desc, occ=occ)
    r.update({"multi_leg_index": index,
              "multi_parent_debit": parent_debit,
              "multi_parent_credit": None})
    return r


def test_a_batch_payments_ids_are_the_movements_id_plus_a_position():
    """Legs share the movement the statement printed and differ only by
    position, so identical amounts inside one batch stay apart and the
    same batch on the monthly and the annual statement still dedups."""
    legs = [_leg(_IBAN_A, _PRE, debit=50.0, index=i, count=3,
                 parent_debit=150.0) for i in (1, 2, 3)]
    ids = [loader._stmt_txn_id(_IBAN_A, r) for r in legs]
    assert len(set(ids)) == 3                      # equal amounts stay apart
    batch = loader._stmt_batch_txn_id(_IBAN_A, legs[0])
    assert all(i == f"{batch}#{n}" for n, i in enumerate(ids, start=1))
    # Re-reading the same batch from another statement mints the same ids.
    assert [loader._stmt_txn_id(_IBAN_A, dict(r)) for r in legs] == ids


def test_an_unsplit_rows_id_is_unchanged_by_the_batch_columns():
    """Every id minted before batches were split must still be minted,
    or the overlap dedup breaks and silver keeps both spellings."""
    r = _mv(_IBAN_A, _PRE, credit=100.0)
    assert loader._stmt_txn_id(_IBAN_A, r) == loader._stmt_batch_txn_id(_IBAN_A, r)


def test_the_batch_row_an_earlier_load_wrote_is_retired(tmp_path: Path):
    """A load that predates the split wrote the batch total as one row.
    Once the payments carry it, that row must go, or the total is counted
    twice — once whole, once as its parts."""
    conn = _fresh_db(tmp_path)
    batch = _mv(_IBAN_A, _PRE, debit=150.0, desc="MULTI E-BANKING ORDER")
    with conn:
        loader._insert_hist_transactions(conn, 1, [batch], {})
    assert conn.execute(
        "SELECT COUNT(*) c FROM transactions").fetchone()["c"] == 1

    legs = [_leg(_IBAN_A, _PRE, debit=50.0, index=i, count=3,
                 parent_debit=150.0) for i in (1, 2, 3)]
    with conn:
        loader._insert_hist_transactions(conn, 2, legs, {})
    rows = conn.execute(
        "SELECT transaction_external_id t, amount_debit d FROM transactions "
        "ORDER BY t").fetchall()
    assert [r["d"] for r in rows] == [50.0, 50.0, 50.0]
    assert loader._stmt_batch_txn_id(_IBAN_A, batch) not in [r["t"] for r in rows]


def test_the_batch_row_survives_where_the_cut_over_skips_its_payments(
        tmp_path: Path):
    """Above the MT940 floor the payments are not inserted, so retiring
    the batch row would drop the movement entirely rather than replace
    it."""
    conn = _fresh_db(tmp_path)
    batch = _mv(_IBAN_A, _POST, debit=150.0, desc="MULTI E-BANKING ORDER")
    with conn:
        # Written while no floor applied, as an earlier load would have.
        loader._insert_hist_transactions(conn, 1, [batch], {})
    legs = [_leg(_IBAN_A, _POST, debit=50.0, index=i, count=3,
                 parent_debit=150.0) for i in (1, 2, 3)]
    with conn:
        loader._insert_hist_transactions(conn, 2, legs, {_IBAN_A: _FLOOR})
    rows = conn.execute("SELECT amount_debit d FROM transactions").fetchall()
    assert [r["d"] for r in rows] == [150.0]


def test_stmt_txn_id_stable_and_namespaced():
    r = _mv(_IBAN_A, _PRE, credit=100.0)
    a = loader._stmt_txn_id(_IBAN_A, r)
    b = loader._stmt_txn_id(_IBAN_A, r)
    assert a == b and a.startswith("stmt:")
    # Different occurrence → different id.
    assert loader._stmt_txn_id(_IBAN_A, {**r, "occurrence": 1}) != a


# ============================================================
# Parser generations — a re-parse replaces, it does not accumulate
# ============================================================

def _seed_txn(conn, txn_id, account="CH0000000001"):
    conn.execute(
        "INSERT OR REPLACE INTO transactions (transaction_external_id, "
        "account_external_id, snapshot_at, value_date, currency_iso, payload) "
        "VALUES (?, ?, 1700000000, 1700000000, 'CHF', '{}')",
        (txn_id, account))


def test_the_purge_takes_the_statement_era_and_leaves_the_csv_export(tmp_path):
    # DESIGN.md §3.6: the two id spaces are disjoint by construction, and
    # the `stmt:` prefix is the era marker. Only the statement era is the
    # PDF parser's to re-derive.
    conn = _fresh_db(tmp_path)
    _seed_txn(conn, "stmt:0001")
    _seed_txn(conn, "stmt:0002")
    _seed_txn(conn, "9876543210")          # UBS's own Transaction no.

    loader._purge_stale_document_rows(conn)

    assert [r[0] for r in conn.execute(
        "SELECT transaction_external_id FROM transactions").fetchall()
    ] == ["9876543210"]
    conn.close()


def test_an_unmoved_document_parser_drops_nothing(tmp_path):
    conn = _fresh_db(tmp_path)
    _seed_txn(conn, "stmt:0001")
    silver.stamp_generation(conn, loader.DOCUMENT_GENERATION_SCOPE,
                            loader._document_generation())

    assert loader._purge_stale_document_rows(conn) == 0
    assert conn.execute(
        "SELECT COUNT(*) FROM transactions").fetchone()[0] == 1
    conn.close()


def test_a_moved_parser_re_derives_without_a_new_dump(tmp_path, monkeypatch):
    """The generation fingerprint has to be reachable in steady state.

    The archive walk sits behind the already-loaded skip, so once every
    dump is loaded a changed parser could never announce itself: the
    same bronze produced different silver depending on whether a dump
    happened to arrive that night. This is the path that closes it.
    """
    conn = _fresh_db(tmp_path)
    dump = tmp_path / "20260101T000000Z"
    (dump / "documents").mkdir(parents=True)

    walked: list[Path] = []

    def _fake_walk(_conn, _snapshot_at, dump_dir, _cache):
        walked.append(dump_dir)
        return (0, 0, 0, 0)

    monkeypatch.setattr(loader, "_load_historical_from_pdfs", _fake_walk)

    # The parsers have moved: the archive is re-derived.
    loader._derive_documents_without_a_new_dump(conn, [dump], {})
    assert walked == [dump]

    # And having moved once, they have not moved twice: an unchanged
    # fingerprint must not re-parse the archive on every load.
    silver.stamp_generation(conn, loader.DOCUMENT_GENERATION_SCOPE,
                            loader._document_generation())
    walked.clear()
    loader._derive_documents_without_a_new_dump(conn, [dump], {})
    assert walked == []
    conn.close()


def test_the_re_derive_needs_an_archive_to_walk(tmp_path, monkeypatch):
    """A dump with no `documents/` directory is not a place to walk from."""
    conn = _fresh_db(tmp_path)
    dump = tmp_path / "20260101T000000Z"
    dump.mkdir()

    monkeypatch.setattr(loader, "_load_historical_from_pdfs",
                        lambda *a: pytest.fail("walked a dump with no archive"))
    loader._derive_documents_without_a_new_dump(conn, [dump], {})
    conn.close()


# ============================================================
# Historical cash rows collapse instead of accumulating
# ============================================================

def _hist_row(isin=None, ccy="CHF", value=1000.0, date=1700000000,
              account="CH00 0000 0000 0000 0000 0", doc="doc-1"):
    """One parsed row as `_insert_hist_positions` takes them. A cash line is
    exactly a position row with no ISIN."""
    return {
        "as_of_date": date, "portfolio_external_id": "0000A0000000000",
        "account_external_id": account, "instrument_isin": isin,
        "currency_iso": ccy, "units": value, "market_value": value,
        "market_value_currency": "CHF", "cost_price": None,
        "market_price": None, "accrued_interest": None,
        "current_fx_rate": None, "acquisition_fx_rate": None,
        "cost_basis": None, "last_purchase_date": None,
        "description": "Account",
        "sector": None, "source_doc_token": doc, "payload": "{}",
    }


def _hist_count(conn, isin_is_null=True):
    op = "IS NULL" if isin_is_null else "IS NOT NULL"
    return conn.execute(
        f"SELECT COUNT(*) FROM historical_position_snapshots "
        f"WHERE instrument_isin {op}").fetchone()[0]


def test_a_cash_row_re_derived_does_not_accumulate(tmp_path):
    # The pass re-lists the whole document archive on every dump, so every
    # cash row is re-derived once per dump. The primary key ends in the
    # ISIN, a cash line has none, and SQLite treats NULLs in a key as
    # distinct — so INSERT OR REPLACE never collapsed them and the table
    # grew by one copy of every cash row per dump, without limit.
    conn = _fresh_db(tmp_path)
    for _ in range(3):
        loader._insert_hist_positions(conn, [_hist_row()])

    assert _hist_count(conn) == 1
    conn.close()


def test_a_re_derived_cash_row_keeps_the_latest_figure(tmp_path):
    # Collapsing must not freeze the first copy: cash is last-writer-wins,
    # the same as a securities row under the key.
    conn = _fresh_db(tmp_path)
    loader._insert_hist_positions(conn, [_hist_row(value=1000.0)])
    loader._insert_hist_positions(conn, [_hist_row(value=2500.0)])

    assert _hist_count(conn) == 1
    assert conn.execute(
        "SELECT market_value FROM historical_position_snapshots"
    ).fetchone()[0] == 2500.0
    conn.close()


def test_cash_rows_that_are_genuinely_different_stay_apart(tmp_path):
    # Two currencies on one account at one date are two facts, and a
    # security is not cash at all — the collapse must reach neither.
    conn = _fresh_db(tmp_path)
    loader._insert_hist_positions(conn, [
        _hist_row(ccy="CHF"), _hist_row(ccy="USD"),
        _hist_row(isin="CH0000000001"),
    ])
    loader._insert_hist_positions(conn, [_hist_row(ccy="CHF")])

    assert _hist_count(conn) == 2
    assert _hist_count(conn, isin_is_null=False) == 1
    conn.close()


# ================================================================
# Payment advices: the leg the export never saw
# ================================================================
#
# A Credit/Debit Advice is keyed by UBS's own transaction number, the
# same one the CSV export puts on the twin leg, so these rows land in
# the same id space as the export's — which is what the compound
# primary key was built for and what every case below is about.

# A synthetic transaction number in the export's spelling.
_ADVICE_TXN = "0000036ED0000123"


def _advice(account: str, *, credit=None, debit=None, booking=_PRE,
            txn=_ADVICE_TXN, purpose="Increase Example Mandate") -> dict:
    """One parsed advice row as `_insert_advice_transactions` takes them."""
    return {
        "transaction_external_id": txn,
        "booking_date": booking,
        "value_date": booking,
        "account_external_id": account,
        "currency_iso": "CHF",
        "amount_debit": debit,
        "amount_credit": credit,
        "counterparty": purpose,
        "description_kind": None,
        "counter_account": None,
        "source_doc_token": "synthetic-token",
        "payload": ('{"booking_type": null, "internal_transfer": true, '
                    '"source": "account_statement_pdf", '
                    '"document": "payment_advice_pdf"}'),
    }


def _seed_export_row(conn, txn_id, account, *, debit=None, credit=None):
    """An export row as `_ingest_transactions_csv` writes one: the CSV
    line verbatim in the payload, a booking type, and the sheet's own
    signed figures."""
    conn.execute(
        "INSERT INTO transactions (transaction_external_id, "
        "account_external_id, snapshot_at, trade_date, booking_date, "
        "value_date, currency_iso, amount_debit, amount_credit, "
        "counterparty, description_kind, payload) "
        "VALUES (?, ?, 1700000000, ?, ?, ?, 'CHF', ?, ?, 'Example AG', "
        "'Special payment order', '{\"Transaction no.\": \"x\"}')",
        (txn_id, account, _PRE, _PRE, _PRE, debit, credit))


def test_an_advice_fills_the_leg_the_export_never_carried(tmp_path):
    # The export covers the paying account only; the receiving mandate
    # account has no export at all. Both legs end up under one id,
    # apart by account — the shape the compound key exists for.
    conn = _fresh_db(tmp_path)
    _seed_export_row(conn, _ADVICE_TXN, _IBAN_A, debit=-12345.67)
    with conn:
        n = loader._insert_advice_transactions(
            conn, 1, [_advice(_IBAN_B, credit=12345.67)])

    assert n == 1
    legs = conn.execute(
        "SELECT account_external_id, amount_debit, amount_credit "
        "FROM transactions WHERE transaction_external_id = ? "
        "ORDER BY account_external_id", (_ADVICE_TXN,)).fetchall()
    assert [r["account_external_id"] for r in legs] == [_IBAN_A, _IBAN_B]
    assert legs[1]["amount_credit"] == 12345.67
    conn.close()


def test_an_advice_never_overwrites_the_row_the_export_wrote(tmp_path):
    # An advice states four facts and the export states all of them
    # plus the booking type, the counterparty and the trade date. The
    # two feeds meet on the SAME key here, so an upsert would thin the
    # richer row out on every document pass.
    conn = _fresh_db(tmp_path)
    _seed_export_row(conn, _ADVICE_TXN, _IBAN_A, debit=-12345.67)
    with conn:
        n = loader._insert_advice_transactions(
            conn, 2, [_advice(_IBAN_A, debit=12345.67)])

    assert n == 0
    row = conn.execute(
        "SELECT description_kind, counterparty, amount_debit, snapshot_at "
        "FROM transactions WHERE transaction_external_id = ?",
        (_ADVICE_TXN,)).fetchone()
    assert row["description_kind"] == "Special payment order"
    assert row["counterparty"] == "Example AG"
    assert row["amount_debit"] == -12345.67       # the sheet's own sign
    assert row["snapshot_at"] == 1700000000
    conn.close()


def test_the_mt940_floor_does_not_gate_an_advice(tmp_path):
    # The floor keeps the statement era, whose ids are minted, from
    # restating a booking the feed already carries under an id nothing
    # can match. An advice carries the feed's own id, so the key
    # resolves an overlap and the floor has nothing left to protect.
    root = tmp_path / "bronze"
    _write_cash_csv(root / "20260101T000000Z", "CH00 0000 0000 0000 0000 1",
                    "2024-01-02", "aaaa")
    floors = loader._mt940_floors_by_account(root / "20260101T000000Z")
    assert floors[_IBAN_A] < _POST          # the row below is above it

    conn = _fresh_db(tmp_path)
    with conn:
        n = loader._insert_advice_transactions(
            conn, 1, [_advice(_IBAN_A, credit=1000.0, booking=_POST)])

    assert n == 1
    conn.close()


def test_an_advice_with_an_unusable_account_is_dropped(tmp_path):
    conn = _fresh_db(tmp_path)
    with conn:
        n = loader._insert_advice_transactions(
            conn, 1, [_advice("not-an-iban", credit=100.0)])

    assert n == 0
    assert conn.execute(
        "SELECT COUNT(*) c FROM transactions").fetchone()["c"] == 0
    conn.close()


def test_the_purge_takes_the_advice_era_and_leaves_the_export(tmp_path):
    # An advice row is keyed by UBS's transaction number and so looks
    # exactly like an export row from the id alone; the payload marker
    # is what tells them apart when the parser moves and the rows have
    # to be re-derived.
    conn = _fresh_db(tmp_path)
    _seed_export_row(conn, "9876543210", _IBAN_A, debit=-10.0)
    _seed_txn(conn, "stmt:0001")
    with conn:
        loader._insert_advice_transactions(
            conn, 1, [_advice(_IBAN_B, credit=12345.67)])

    loader._purge_stale_document_rows(conn)

    assert [r[0] for r in conn.execute(
        "SELECT transaction_external_id FROM transactions").fetchall()
    ] == ["9876543210"]
    conn.close()


# ---- document routing ---------------------------------------------

def _route(monkeypatch, tmp_path, doc_type: str, label: str = "") -> str:
    """Which parser `_parse_one_pdf` hands a document to, with every
    parser stubbed so no PDF is opened."""
    import pdf_parsers
    called = []
    monkeypatch.setattr(pdf_parsers, "parse_payment_advice",
                        lambda *a: called.append("advice") or [])
    monkeypatch.setattr(pdf_parsers, "parse_maturity_notice",
                        lambda *a: called.append("mortgage") or [])
    monkeypatch.setattr(pdf_parsers, "parse_statement_of_assets",
                        lambda *a: called.append("positions") or [])
    monkeypatch.setattr(pdf_parsers, "parse_account_statement_combined",
                        lambda *a: called.append("statement") or ([], []))
    monkeypatch.setattr(pdf_parsers, "parse_contract_note",
                        lambda *a: called.append("contract_note") or [])
    monkeypatch.setattr(pdf_parsers, "parse_capital_call",
                        lambda *a: called.append("capital_call") or [])
    pdf = tmp_path / "doc.pdf"
    pdf.write_bytes(b"%PDF-1.4\n")
    loader._parse_one_pdf(("token", str(pdf), label, doc_type))
    return called[0]


def test_an_advice_label_routes_to_the_advice_parser(monkeypatch, tmp_path):
    # UBS spells the label inconsistently within one archive, so the
    # match is case-folded.
    assert _route(monkeypatch, tmp_path, "Credit Advice") == "advice"
    assert _route(monkeypatch, tmp_path, "Debit Advice") == "advice"
    assert _route(monkeypatch, tmp_path, "Debit advice") == "advice"


def test_a_label_that_only_reads_like_an_advice_is_left_alone(monkeypatch,
                                                              tmp_path):
    # 'UBS Advice' is a fee the bank bills itself and 'Advice _
    # Statement' a securities confirmation. Neither is a payment, and
    # neither may be pulled out of the Account-Statement path.
    assert _route(monkeypatch, tmp_path, "UBS Advice") == "statement"
    assert _route(monkeypatch, tmp_path, "Advice _ Statement") == "statement"
    assert _route(monkeypatch, tmp_path, "Maturity notice") == "mortgage"


# ---- an advice must not restate the statement ledger ---------------
#
# The advice and the statement walker read the SAME archive, and most
# of the advices are issued for payments the account's own statement
# went on to print in its ledger. The statement mints its id, so the
# primary key cannot recognise the pair — the movement itself has to.

def _stmt_row(conn, account, *, booking=_PRE, credit=None, debit=None,
              value=None, desc="CREDIT"):
    """One statement-era row, written by the real statement path so it
    carries that era's id and its printed (unsigned) amount column."""
    with conn:
        loader._insert_hist_transactions(
            conn, 1700000000,
            [_mv(account, booking, credit=credit, debit=debit, desc=desc,
                 value=value)],
            {})


def test_an_advice_does_not_restate_what_the_statement_printed(tmp_path):
    # The duplicate the compound key cannot catch: one payment, two ids,
    # one account — and two withdrawals in every consumer that sums them.
    conn = _fresh_db(tmp_path)
    _stmt_row(conn, _IBAN_B, debit=12345.67, desc="PAYMENT ORDER")
    with conn:
        n = loader._insert_advice_transactions(
            conn, 1, [_advice(_IBAN_B, debit=12345.67)])

    assert n == 0
    ids = [r[0] for r in conn.execute(
        "SELECT transaction_external_id FROM transactions").fetchall()]
    assert len(ids) == 1 and ids[0].startswith("stmt:")
    conn.close()


def test_the_match_reads_the_column_and_never_the_stored_sign(tmp_path):
    # DESIGN.md §3.6: the export writes the sheet's signed cell and the
    # PDF eras the unsigned figure they print. The same movement is the
    # same movement whichever convention recorded it.
    conn = _fresh_db(tmp_path)
    _seed_export_row(conn, "9876543210", _IBAN_B, debit=-12345.67)
    with conn:
        n = loader._insert_advice_transactions(
            conn, 1, [_advice(_IBAN_B, debit=12345.67)])

    assert n == 0
    conn.close()


def test_an_advice_for_a_movement_of_its_own_still_lands(tmp_path):
    # The check is on the movement, not on the day: a second payment on
    # the same account and day is a row the ledger does not hold.
    conn = _fresh_db(tmp_path)
    _stmt_row(conn, _IBAN_B, debit=12345.67, desc="PAYMENT ORDER")
    with conn:
        n = loader._insert_advice_transactions(
            conn, 1, [_advice(_IBAN_B, debit=999.99)])

    assert n == 1
    assert conn.execute(
        "SELECT COUNT(*) c FROM transactions").fetchone()["c"] == 2
    conn.close()


def test_a_ledger_row_excuses_only_one_advice(tmp_path):
    # Two distinct transfers of one amount, into one account, on one day
    # is a shape the archive really holds (an FX leg and a same-currency
    # leg arriving together). The ledger row that explains the first must
    # not swallow the second.
    conn = _fresh_db(tmp_path)
    _stmt_row(conn, _IBAN_B, credit=12345.67)
    with conn:
        n = loader._insert_advice_transactions(conn, 1, [
            _advice(_IBAN_B, credit=12345.67, txn="0000036ED0000123"),
            _advice(_IBAN_B, credit=12345.67, txn="0000036ED0000456"),
        ])

    assert n == 1
    assert conn.execute(
        "SELECT COUNT(*) c FROM transactions").fetchone()["c"] == 2
    conn.close()


def test_a_back_valued_advice_matches_on_either_date(tmp_path):
    # The two eras do not agree on which date they file a back-valued
    # payment under, so either lining up is enough to call it one
    # movement — the alternative is writing it twice.
    conn = _fresh_db(tmp_path)
    _stmt_row(conn, _IBAN_B, booking=_PRE, value=_POST, debit=12345.67,
              desc="PAYMENT ORDER")
    row = _advice(_IBAN_B, debit=12345.67, booking=_PRE)
    row["value_date"] = loader.ts_from_iso("2024-06-01")
    with conn:
        n = loader._insert_advice_transactions(conn, 1, [row])

    assert n == 0
    conn.close()


def test_the_advice_pass_does_not_depend_on_the_parse_order(tmp_path):
    # The pass hands the advices over in whatever order the PDF workers
    # finished. Two loads of one archive must still reach one silver.
    batch = [
        _advice(_IBAN_B, credit=12345.67, txn="0000036ED0000123"),
        _advice(_IBAN_B, credit=12345.67, txn="0000036ED0000456"),
        _advice(_IBAN_A, debit=500.0, txn="0000036ED0000789"),
    ]

    def written(rows):
        home = tmp_path / rows[0]["transaction_external_id"]
        home.mkdir(parents=True, exist_ok=True)
        conn = _fresh_db(home)
        _stmt_row(conn, _IBAN_B, credit=12345.67)
        with conn:
            loader._insert_advice_transactions(conn, 1, rows)
        got = sorted(r[0] for r in conn.execute(
            "SELECT transaction_external_id FROM transactions "
            "WHERE payload LIKE '%payment_advice_pdf%'").fetchall())
        conn.close()
        return got

    assert written(batch) == written(list(reversed(batch)))


def test_a_second_load_of_one_archive_adds_nothing(tmp_path):
    conn = _fresh_db(tmp_path)
    rows = [_advice(_IBAN_B, credit=12345.67)]
    with conn:
        assert loader._insert_advice_transactions(conn, 1, rows) == 1
    with conn:
        assert loader._insert_advice_transactions(conn, 2, rows) == 0
    assert conn.execute(
        "SELECT COUNT(*) c FROM transactions").fetchone()["c"] == 1
    conn.close()


def test_the_conflict_clause_still_guards_a_row_the_content_check_misses(
        tmp_path):
    # The export restated the same transaction number with a corrected
    # amount, so nothing about the movement lines up any more — and the
    # key, which is what the two feeds share, has to hold the line.
    conn = _fresh_db(tmp_path)
    _seed_export_row(conn, _ADVICE_TXN, _IBAN_B, debit=-999.99)
    with conn:
        n = loader._insert_advice_transactions(
            conn, 1, [_advice(_IBAN_B, debit=12345.67)])

    assert n == 0
    row = conn.execute(
        "SELECT amount_debit, description_kind FROM transactions").fetchone()
    assert row["amount_debit"] == -999.99
    assert row["description_kind"] == "Special payment order"
    conn.close()


_walk_probe = itertools.count()


def _archive_walk_saw(tmp_path, doc_type: str) -> bool:
    """Whether the document walk listed a `doc_type` as parseable.

    Read from the one side effect a walk with work to do has and an
    empty one does not: the generation purge, which runs only below the
    walk's "nothing to parse" return. The PDFs themselves are absent, so
    every listed document comes back a skip and nothing is re-derived."""
    home = tmp_path / f"walk{next(_walk_probe)}"
    home.mkdir(parents=True, exist_ok=True)
    conn = _fresh_db(home)
    conn.execute(
        "INSERT INTO documents (doc_token, content_sha256, file_path, "
        "size_bytes, snapshot_at, doc_type, label) "
        "VALUES ('t', 'sha', 'documents/x.pdf', 1, 1700000000, ?, ?)",
        (doc_type, f"{doc_type} 01.01.2020"))
    _seed_txn(conn, "stmt:0001")
    dump = home / "20260101T000000Z"
    (dump / "documents").mkdir(parents=True)

    loader._load_historical_from_pdfs(conn, 1700000000, dump, {})

    purged = conn.execute(
        "SELECT COUNT(*) c FROM transactions").fetchone()["c"] == 0
    conn.close()
    return purged


def test_every_advice_spelling_the_router_takes_is_listed(tmp_path):
    # The SELECT that lists the archive and the router that dispatches it
    # read one set, so a casing UBS invents is admitted and routed by the
    # same edit — rather than being routed but never listed, which shows
    # up as nothing at all happening.
    for spelling in loader.ADVICE_DOC_TYPES:
        assert _archive_walk_saw(tmp_path, spelling.title())
        assert _archive_walk_saw(tmp_path, spelling.upper())
    # And a label that only reads like one stays out of the walk.
    assert not _archive_walk_saw(tmp_path, "Advice _ Statement")


def _seed_doc(conn, token: str, sha: str, doc_type: str) -> None:
    conn.execute(
        "INSERT INTO documents (doc_token, content_sha256, file_path, "
        "size_bytes, snapshot_at, doc_type, label) "
        "VALUES (?, ?, 'documents/x.pdf', 1, 1700000000, ?, ?)",
        (token, sha, doc_type, f"{doc_type} 01.01.2020"))


def test_the_walk_writes_the_advices_after_the_statement_ledger(tmp_path):
    # The advice is listed FIRST here, which is the order that breaks a
    # pass that writes each document as it comes back: the movement it
    # restates would not be in silver yet to be recognised, and the load
    # would book the payment twice — or not, depending on which PDF
    # worker happened to finish first.
    conn = _fresh_db(tmp_path)
    _seed_doc(conn, "adv", "sha-adv", "Credit Advice")
    _seed_doc(conn, "stmt", "sha-stmt", "Account Statement")
    dump = tmp_path / "bronze" / "20260101T000000Z"
    (dump / "documents").mkdir(parents=True)
    # Pre-parsed, so the walk replays these instead of opening a PDF.
    cache = {
        "sha-adv": ("adv", "payment_advice", "adv.pdf",
                    [_advice(_IBAN_B, credit=12345.67)], None),
        "sha-stmt": ("stmt", "account_statement", "stmt.pdf",
                     {"cash": [], "transactions": [
                         _mv(_IBAN_B, _PRE, credit=12345.67)]}, None),
    }

    with conn:
        loader._load_historical_from_pdfs(conn, 1700000000, dump, cache)

    ids = [r[0] for r in conn.execute(
        "SELECT transaction_external_id FROM transactions").fetchall()]
    assert len(ids) == 1 and ids[0].startswith("stmt:")
    conn.close()


# ============================================================
# One transaction number, several movements
# ============================================================
#
# UBS mints the "Transaction no." per BOOKING EVENT as the bank models
# it, and the bank sometimes models several movements as one. Keying
# silver on the bare number let those rows overwrite each other, and an
# ON CONFLICT upsert cannot be told from a re-load of the same row — so
# the loss was silent and a deposit product's whole ledger could reduce
# to whichever row the export happened to print last.
#
# Every value below is synthetic: an IBAN-shaped placeholder, an
# invented transaction number, and an impossible year (AGENTS.md §4).

TXN_IBAN = "CH00 0000 0000 0000 00AA A"
TXN_ACCT = "CH0000000000000000AAA"


def _txn_csv(rows: list[str], iban: str = TXN_IBAN) -> str:
    """An export-era transactions CSV: 8 metadata lines, the header,
    then the data rows the caller gives."""
    head = "\r\n".join([
        "Account number:;0000 00000000.00;",
        f"IBAN:;{iban};",
        "From:;2098-01-01;",
        "To:;2098-12-31;",
        "Currency:;CHF;",
        "Valued in:;CHF;",
        ";",
        ";",
    ])
    return "﻿" + head + "\r\n" + loader.TXN_DATA_HEADER + "\r\n" + \
        "\r\n".join(rows) + "\r\n"


def _txn_row(day: str, debit: str, credit: str, balance: str, txn_no: str,
             d1: str, d2: str) -> str:
    return (f"2098-{day};;2098-{day};2098-{day};CHF;{debit};{credit};;"
            f"{balance};{txn_no};\"{d1}\";{d2};;;")


def _seed_txn_bronze(root: Path, rows: list[str], name: str = "cash_x") -> Path:
    dump = root / "20980101T000000Z"
    (dump / "transactions").mkdir(parents=True, exist_ok=True)
    (dump / "transactions" / f"{name}.csv").write_text(
        _txn_csv(rows), encoding="utf-8")
    return dump


def test_a_products_whole_ledger_survives_one_transaction_number(tmp_path):
    """The defect, end to end: a deposit product stamps ONE number on
    every movement of its life. All of them must land."""
    conn = _fresh_db(tmp_path)
    rows = [
        _txn_row("07-12", "", "222000.00", "222000.00", "GZ00000YQ0000000",
                 "Example Call Deposit; Serial no. 00000", "Call Deposit Repayment"),
        _txn_row("07-12", "", "56.78", "222056.78", "GZ00000YQ0000000",
                 "Example Call Deposit; Serial no. 00000", "Call Deposit Interest Payment"),
        _txn_row("03-04", "-333000.00", "", "555000.00", "GZ00000YQ0000000",
                 "Example Call Deposit; Serial no. 00000", "Call Deposit Increase"),
    ]
    loader._load_transactions(conn, 1000, _seed_txn_bronze(tmp_path, rows))

    got = conn.execute(
        "SELECT COUNT(*) FROM transactions WHERE account_external_id = ?",
        (TXN_ACCT,)).fetchone()[0]
    assert got == 3, f"{3 - got} movement(s) were overwritten by a sibling"
    # And the figures are the ones the bank printed, not one of them
    # three times.
    amounts = sorted(
        (r[0] or 0) + (r[1] or 0) for r in conn.execute(
            "SELECT amount_debit, amount_credit FROM transactions "
            "WHERE account_external_id = ?", (TXN_ACCT,)))
    assert amounts == [-333000.00, 56.78, 222000.00]


def test_the_largest_movement_keeps_the_banks_own_number(tmp_path):
    """A payment and the correspondent's fee share one number. The
    PAYMENT must keep the bare number: the advice pass is keyed by it
    (load.py's header note), and an advice names the payment."""
    conn = _fresh_db(tmp_path)
    rows = [
        _txn_row("02-11", "-444.44", "", "1000.00", "ZD00000TI0000000",
                 "Example Payee; XX EXAMPLECITY 0000", "e-banking payment order"),
        _txn_row("02-11", "-98.76", "", "901.24", "ZD00000TI0000000",
                 "Third-Party Charges", ""),
    ]
    loader._load_transactions(conn, 1000, _seed_txn_bronze(tmp_path, rows))

    bare = conn.execute(
        "SELECT amount_debit FROM transactions "
        "WHERE transaction_external_id = ?", ("ZD00000TI0000000",)).fetchone()
    assert bare[0] == -444.44, "the fee took the number its payment needs"
    suffixed = conn.execute(
        "SELECT COUNT(*) FROM transactions "
        "WHERE transaction_external_id LIKE ?", ("ZD00000TI0000000#%",)).fetchone()[0]
    assert suffixed == 1


def test_a_number_used_once_is_the_id_unchanged(tmp_path):
    """Every row the export has ever loaded but the collided ones keeps
    the id it had, so the fix moves nothing it did not have to."""
    conn = _fresh_db(tmp_path)
    rows = [_txn_row("01-02", "-10.00", "", "990.00", "AA00000TO0000001",
                     "Example Payee", "e-banking payment order")]
    loader._load_transactions(conn, 1000, _seed_txn_bronze(tmp_path, rows))
    ids = [r[0] for r in conn.execute(
        "SELECT transaction_external_id FROM transactions")]
    assert ids == ["AA00000TO0000001"]


def test_the_ids_do_not_move_when_the_export_reprints_the_group(tmp_path):
    """The suffix is derived from the row's own content, not from its
    position in the file, so a re-dump that prints the group in another
    order converges on the same rows rather than doubling them."""
    conn = _fresh_db(tmp_path)
    rows = [
        _txn_row("03-15", "", "111000.00", "666000.00", "GZ00000YQ0000000",
                 "Example Call Deposit; Serial no. 00000", "Call Deposit Decrease"),
        _txn_row("06-13", "", "444000.00", "777000.00", "GZ00000YQ0000000",
                 "Example Call Deposit; Serial no. 00000", "Call Deposit Decrease"),
        _txn_row("03-04", "-333000.00", "", "555000.00", "GZ00000YQ0000000",
                 "Example Call Deposit; Serial no. 00000", "Call Deposit Increase"),
    ]
    loader._load_transactions(conn, 1000, _seed_txn_bronze(tmp_path, rows))
    first = sorted(r[0] for r in conn.execute(
        "SELECT transaction_external_id FROM transactions"))

    loader._load_transactions(
        conn, 2000, _seed_txn_bronze(tmp_path, list(reversed(rows))))
    again = sorted(r[0] for r in conn.execute(
        "SELECT transaction_external_id FROM transactions"))
    assert again == first, "a reprint of the same group minted new ids"


def test_a_group_split_across_two_files_is_numbered_once(tmp_path):
    """UBS splits one account's history across several CSVs. Grouping
    per file would let a product whose movements straddle the split
    mint the same ids twice."""
    conn = _fresh_db(tmp_path)
    dump = tmp_path / "20980101T000000Z"
    (dump / "transactions").mkdir(parents=True, exist_ok=True)
    (dump / "transactions" / "cash_a_early.csv").write_text(_txn_csv([
        _txn_row("03-04", "-333000.00", "", "555000.00", "GZ00000YQ0000000",
                 "Example Call Deposit; Serial no. 00000", "Call Deposit Increase"),
    ]), encoding="utf-8")
    (dump / "transactions" / "cash_a_late.csv").write_text(_txn_csv([
        _txn_row("07-12", "", "333000.00", "222000.00", "GZ00000YQ0000000",
                 "Example Call Deposit; Serial no. 00000", "Call Deposit Repayment"),
    ]), encoding="utf-8")
    loader._load_transactions(conn, 1000, dump)
    assert conn.execute(
        "SELECT COUNT(*) FROM transactions").fetchone()[0] == 2


def test_identical_rows_sharing_a_number_stay_apart(tmp_path):
    """Two rows a group cannot tell apart by any movement field hash
    alike. An ordinal keeps them, rather than letting one eat the
    other — which is the whole defect."""
    conn = _fresh_db(tmp_path)
    twin = _txn_row("05-05", "-43.21", "", "100.00", "ZD00000TI0000009",
                    "Third-Party Charges", "")
    rows = [
        _txn_row("05-05", "-943.21", "", "143.21", "ZD00000TI0000009",
                 "Example Payee", "e-banking payment order"),
        twin, twin,
    ]
    loader._load_transactions(conn, 1000, _seed_txn_bronze(tmp_path, rows))
    assert conn.execute(
        "SELECT COUNT(*) FROM transactions").fetchone()[0] == 3


def test_a_partial_window_does_not_re_auction_the_bare_number(tmp_path):
    """The regression that matters most, because it is routine rather
    than exotic: UBS clamps its transactions UI and `--lookback` is a
    window, so a later run sees only PART of a group.

    Picking the largest member of whatever the dump can see hands the
    bare number to a different row each time — overwriting the row that
    held it and storing the newcomer twice, which is the silent loss the
    whole id scheme exists to prevent."""
    conn = _fresh_db(tmp_path)
    pay = _txn_row("02-11", "-6000.00", "", "1000.00", "ZD00000TI0000000",
                   "Example Payee; XX EXAMPLECITY 0000", "e-banking payment order")
    fee = _txn_row("02-11", "-25.00", "", "975.00", "ZD00000TI0000000",
                   "Third-Party Charges", "")
    loader._load_transactions(conn, 1000, _seed_txn_bronze(tmp_path / "r0", [pay, fee]))

    def state():
        return {r[0]: r[1] for r in conn.execute(
            "SELECT transaction_external_id, amount_debit FROM transactions")}
    full = state()
    assert full["ZD00000TI0000000"] == -6000.00, "the payment should hold the bank's number"

    # Three more runs whose window covers only the fee.
    for i, snap in enumerate((2000, 3000, 4000)):
        loader._load_transactions(
            conn, snap, _seed_txn_bronze(tmp_path / f"r{i + 1}", [fee]))
        assert state() == full, f"run {i} moved the bare number or lost a row"

    # And the full window returning changes nothing either.
    loader._load_transactions(conn, 5000, _seed_txn_bronze(tmp_path / "r9", [pay, fee]))
    assert state() == full


def test_a_restated_name_keeps_the_movement_under_its_number(tmp_path):
    """The bank restates a security's name on rows it has already
    exported. The movement that comes back under the new name is the
    one silver holds, not a second one: it keeps the bare number and
    takes the new name, rather than landing again under a suffix."""
    conn = _fresh_db(tmp_path)
    old = _txn_row("06-15", "", "500.00", "1500.00", "AW00000KE0000000",
                   "Reg.shs Example Old Name AG (XMPL)", "Dividend")
    loader._load_transactions(conn, 1000, _seed_txn_bronze(tmp_path / "r0", [old]))
    new = _txn_row("06-15", "", "500.00", "1500.00", "AW00000KE0000000",
                   "Reg.shs Example New Name AG (XMPL)", "Dividend")
    loader._load_transactions(conn, 2000, _seed_txn_bronze(tmp_path / "r1", [new]))

    rows = conn.execute(
        "SELECT transaction_external_id, json_extract(payload, '$.Description1')"
        "  FROM transactions").fetchall()
    assert len(rows) == 1, rows
    assert rows[0][0] == "AW00000KE0000000"
    assert "New Name" in rows[0][1]


def test_a_restated_name_two_members_could_claim_takes_no_number(tmp_path):
    """Where two rows of the dump match the holder's movement, neither
    can be shown to be the holder, so neither takes its number."""
    conn = _fresh_db(tmp_path)
    held = _txn_row("05-05", "-43.21", "", "100.00", "ZD00000TI0000008",
                    "Example Charge", "")
    loader._load_transactions(conn, 1000, _seed_txn_bronze(tmp_path / "r0", [held]))
    twin_a = _txn_row("05-05", "-43.21", "", "100.00", "ZD00000TI0000008",
                      "Example Charge A", "")
    twin_b = _txn_row("05-05", "-43.21", "", "100.00", "ZD00000TI0000008",
                      "Example Charge B", "")
    loader._load_transactions(
        conn, 2000, _seed_txn_bronze(tmp_path / "r1", [twin_a, twin_b]))

    rows = {r[0]: r[1] for r in conn.execute(
        "SELECT transaction_external_id, json_extract(payload, '$.Description1')"
        "  FROM transactions")}
    assert rows["ZD00000TI0000008"] == "Example Charge", "the holder was restated"
    assert len(rows) == 3, rows


def test_a_group_nobody_holds_yet_gives_the_number_to_the_payment(tmp_path):
    """The advice pass is keyed by UBS's number and an advice names a
    payment rather than the fee beside it, so an unclaimed group hands
    the number to the largest movement."""
    conn = _fresh_db(tmp_path)
    rows = [
        _txn_row("02-11", "-25.00", "", "975.00", "ZD00000TI0000000",
                 "Third-Party Charges", ""),
        _txn_row("02-11", "-6000.00", "", "1000.00", "ZD00000TI0000000",
                 "Example Payee; XX EXAMPLECITY 0000", "e-banking payment order"),
    ]
    loader._load_transactions(conn, 1000, _seed_txn_bronze(tmp_path, rows))
    bare = conn.execute(
        "SELECT amount_debit FROM transactions WHERE transaction_external_id = ?",
        ("ZD00000TI0000000",)).fetchone()
    assert bare[0] == -6000.00


def test_a_holder_this_window_cannot_see_keeps_its_number(tmp_path):
    """A group whose holder is outside the window is not this dump's to
    re-key. Nothing it carries may take that number, even where the
    dump's own largest would otherwise have claimed it."""
    conn = _fresh_db(tmp_path)
    big = _txn_row("02-11", "-9000.00", "", "1000.00", "ZD00000TI0000001",
                   "Example Payee; XX EXAMPLECITY 0000", "e-banking payment order")
    loader._load_transactions(conn, 1000, _seed_txn_bronze(tmp_path / "r0", [big]))

    other = _txn_row("05-05", "-40.00", "", "960.00", "ZD00000TI0000001",
                     "Third-Party Charges", "")
    loader._load_transactions(conn, 2000, _seed_txn_bronze(tmp_path / "r1", [other]))

    rows = {r[0]: r[1] for r in conn.execute(
        "SELECT transaction_external_id, amount_debit FROM transactions")}
    assert rows["ZD00000TI0000001"] == -9000.00, "the absent holder lost its number"
    assert len(rows) == 2, rows


# ============================================================
# Documents the bank delivers by hand
# ============================================================
#
# UBS produces some statements only on request: they never appear in the
# e-banking archive, so `download` cannot reach them and no listing row
# describes them. They are placed in `<bronze>/supplied-documents/` and
# identified from their own text. Every identifier below is synthetic per
# AGENTS.md §4.

_SUPPLIED_HEADER = "\n".join([
    "UBS Switzerland AG",
    "Statement of assets",
    "As of 7 March 2024",
    "Portfolio 999-00000000-06, valued in Swiss Franc (CHF)",
])


def _supplied_dir(tmp_path: Path, *names: str) -> Path:
    """A supplied-documents dir holding `names`. The bytes differ per name
    so each file has its own content hash; the text they stand for is
    supplied by the monkeypatched extractor, not by these bytes."""
    d = tmp_path / loader.SUPPLIED_DOCUMENTS_DIRNAME
    d.mkdir(parents=True, exist_ok=True)
    for n in names:
        (d / n).write_bytes(f"%PDF-1.4 {n}".encode())
    return d


def _texts(monkeypatch, mapping: dict[str, str]) -> None:
    """Stand in for pdfplumber: map each file NAME to the text it yields."""
    monkeypatch.setattr("pdf_parsers.statement_of_assets_text",
                        lambda path: mapping[Path(path).name])


def test_a_supplied_statement_is_indexed_from_its_own_text(
        tmp_path, monkeypatch):
    conn = _fresh_db(tmp_path)
    d = _supplied_dir(tmp_path, "whatever-it-was-called.pdf")
    _texts(monkeypatch, {"whatever-it-was-called.pdf": _SUPPLIED_HEADER})

    assert loader._load_supplied_documents(conn, d) == 1

    row = conn.execute("SELECT * FROM documents").fetchone()
    assert row["doc_token"].startswith(loader.SUPPLIED_DOC_TOKEN_PREFIX)
    assert row["doc_type"] == loader.SUPPLIED_STMT_OF_ASSETS_DOC_TYPE
    assert row["portfolio_external_id"] == "0999000000000006"
    # 2024-03-07, the date the document states — not the file's name,
    # which says nothing, and not the day it was placed.
    assert row["doc_date"] == 1709769600
    assert row["label"] == ""
    conn.close()


def test_a_supplied_pdf_the_loader_cannot_identify_is_not_indexed(
        tmp_path, monkeypatch, caplog):
    """Loud, and with no row: a document catalogued under no type would
    sit in the archive looking ingested while nothing could ever parse it."""
    conn = _fresh_db(tmp_path)
    d = _supplied_dir(tmp_path, "mystery.pdf")
    _texts(monkeypatch, {"mystery.pdf": "Tax Report\nFor the year 2024"})

    with caplog.at_level("WARNING"):
        assert loader._load_supplied_documents(conn, d) == 0

    assert "mystery.pdf" in caplog.text
    assert conn.execute("SELECT COUNT(*) c FROM documents").fetchone()["c"] == 0
    conn.close()


def test_a_supplied_statement_the_archive_already_holds_is_not_doubled(
        tmp_path, monkeypatch):
    """The same document from both roads is one document. Were it two,
    its positions would be derived twice under two tokens."""
    conn = _fresh_db(tmp_path)
    d = _supplied_dir(tmp_path, "q1.pdf")
    _texts(monkeypatch, {"q1.pdf": _SUPPLIED_HEADER})
    sha = bronze.sha256_file(d / "q1.pdf")[0]
    conn.execute(
        "INSERT INTO documents (doc_token, content_sha256, file_path, "
        "size_bytes, snapshot_at, label) "
        "VALUES ('scraped-token', ?, 'documents/x.pdf', 1, 1700000000, ?)",
        (sha, "Statement of assets as of 07032024 ..."))

    assert loader._load_supplied_documents(conn, d) == 0

    tokens = [r["doc_token"] for r in conn.execute(
        "SELECT doc_token FROM documents")]
    assert tokens == ["scraped-token"]
    conn.close()


def test_re_indexing_a_supplied_document_is_a_no_op(tmp_path, monkeypatch):
    conn = _fresh_db(tmp_path)
    d = _supplied_dir(tmp_path, "q1.pdf")
    _texts(monkeypatch, {"q1.pdf": _SUPPLIED_HEADER})

    assert loader._load_supplied_documents(conn, d) == 1
    assert loader._load_supplied_documents(conn, d) == 0
    assert conn.execute("SELECT COUNT(*) c FROM documents").fetchone()["c"] == 1
    conn.close()


def test_no_supplied_directory_is_the_ordinary_case(tmp_path):
    conn = _fresh_db(tmp_path)
    assert loader._load_supplied_documents(conn, tmp_path / "absent") == 0
    conn.close()


def test_a_supplied_statement_enters_the_archive_walk(tmp_path):
    """Indexing it is half the job; the walk has to list it too."""
    assert _archive_walk_saw(tmp_path, loader.SUPPLIED_STMT_OF_ASSETS_DOC_TYPE)


def test_a_new_supplied_document_derives_without_a_new_dump(
        tmp_path, monkeypatch):
    """The archive walk sits behind the already-loaded skip, so on a night
    when every dump is loaded a newly placed document would be indexed and
    then never read."""
    conn = _fresh_db(tmp_path)
    dump = tmp_path / "20260101T000000Z"
    (dump / "documents").mkdir(parents=True)
    silver.stamp_generation(conn, loader.DOCUMENT_GENERATION_SCOPE,
                            loader._document_generation())
    conn.commit()

    walked: list[Path] = []
    monkeypatch.setattr(loader, "_load_historical_from_pdfs",
                        lambda _c, _t, d, _cache: walked.append(d) or (0, 0, 0, 0))

    # Nothing new: the parsers have not moved and nothing was supplied.
    loader._derive_documents_without_a_new_dump(conn, [dump], {})
    assert walked == []

    loader._derive_documents_without_a_new_dump(conn, [dump], {}, supplied=1)
    assert walked == [dump]
    conn.close()


def test_the_supplied_directory_is_not_a_bronze_dump(tmp_path):
    """It sits beside the run dirs, and must never be walked as one."""
    (tmp_path / loader.SUPPLIED_DOCUMENTS_DIRNAME).mkdir()
    (tmp_path / "20260101T000000Z").mkdir()
    assert [d.name for d in loader.scan_bronze(tmp_path)] == ["20260101T000000Z"]


# ============================================================
# Statement holdings' cost side, and the securities advices
# ============================================================

def test_0013_moves_a_private_market_price_and_keeps_a_cash_rate(tmp_path):
    """Rows loaded before migration 0013 carry the private-markets NAV
    per unit in the exchange-rate column. The migration moves it to
    `market_price`, where the parser writes it, and leaves a cash line's
    rate where it was, under the column's new name."""
    before = tmp_path / "migrations-0012"
    before.mkdir()
    for f in MIGRATIONS_DIR.glob("*.sql"):
        if int(f.name[:4]) <= 12:
            (before / f.name).write_text(f.read_text(encoding="utf-8"),
                                         encoding="utf-8")
    conn = sqlite3.connect(str(tmp_path / "ubs-web.db"))
    conn.row_factory = sqlite3.Row
    silver.apply_migrations(conn, before)
    insert = (
        "INSERT INTO historical_position_snapshots (as_of_date, "
        "portfolio_external_id, account_external_id, instrument_isin, "
        "currency_iso, units, market_value, market_value_currency, "
        "market_price, exchange_rate_to_base, source_doc_token, payload) "
        "VALUES (1900000000, '0000000000000001', ?, ?, ?, ?, ?, 'USD', "
        "NULL, ?, 'doc', ?)")
    conn.execute(insert, ("", "XX0000000033", "USD", 300.0, 375.0, 1.25,
                          '{"kind": "private_market"}'))
    conn.execute(insert, ("CH0000000000000000000A", None, "GBP", 1000.0,
                          1300.0, 1.3, '{"raw": {}}'))
    conn.commit()

    silver.apply_migrations(conn, MIGRATIONS_DIR)

    pm, cash = (dict(r) for r in conn.execute(
        "SELECT * FROM historical_position_snapshots "
        "ORDER BY instrument_isin IS NULL"))
    assert pm["market_price"] == pytest.approx(1.25)
    assert pm["current_fx_rate"] is None
    assert cash["current_fx_rate"] == pytest.approx(1.3)
    assert cash["market_price"] is None
    for row in (pm, cash):
        assert "exchange_rate_to_base" not in row
        assert row["acquisition_fx_rate"] is None
        assert row["cost_basis"] is None
        assert row["last_purchase_date"] is None
    conn.close()


def test_a_holdings_cost_side_reaches_silver(tmp_path):
    conn = _fresh_db(tmp_path)
    loader._insert_hist_positions(conn, [{
        **_hist_row(isin="XX0000000055", ccy="GBP"),
        "current_fx_rate": 1.3, "acquisition_fx_rate": 1.25,
        "cost_basis": 25000.0, "last_purchase_date": 1899763200,
    }])
    row = conn.execute(
        "SELECT current_fx_rate, acquisition_fx_rate, cost_basis, "
        "last_purchase_date FROM historical_position_snapshots").fetchone()
    assert tuple(row) == (1.3, 1.25, 25000.0, 1899763200)
    conn.close()


def test_securities_advices_route_to_their_parsers(monkeypatch, tmp_path):
    assert _route(monkeypatch, tmp_path, "Contract note") == "contract_note"
    assert _route(monkeypatch, tmp_path,
                  "Private Market Letter") == "capital_call"


def test_the_walk_lists_the_securities_advices(tmp_path):
    assert _archive_walk_saw(tmp_path, loader.CONTRACT_NOTE_DOC_TYPE)
    assert _archive_walk_saw(tmp_path, loader.PRIVATE_MARKET_LETTER_DOC_TYPE)


def _securities_advice(token: str, amount: float) -> dict:
    """One parsed `advices` row as `pdf_parsers` emits them."""
    row = dict.fromkeys(loader._ADVICE_COLUMNS)
    row.update(source_doc_token=token, kind="capital_call",
               instrument_isin="XX0000000011", currency_iso="USD",
               amount=amount, payload="{}")
    return row


def test_the_walk_writes_securities_advices_and_re_derives_them(tmp_path):
    conn = _fresh_db(tmp_path)
    _seed_doc(conn, "call", "sha-call", loader.PRIVATE_MARKET_LETTER_DOC_TYPE)
    dump = tmp_path / "bronze" / "20260101T000000Z"
    (dump / "documents").mkdir(parents=True)

    def walk(amount: float) -> None:
        cache = {"sha-call": ("call", "securities_advice", "call.pdf",
                              [_securities_advice("call", amount)], None)}
        with conn:
            loader._load_historical_from_pdfs(conn, 1700000000, dump, cache)

    walk(12345.67)
    walk(13579.24)      # a re-derive replaces the row rather than adding one
    rows = conn.execute("SELECT source_doc_token, amount FROM advices").fetchall()
    assert [tuple(r) for r in rows] == [("call", 13579.24)]
    conn.close()


def test_the_purge_takes_the_securities_advices(tmp_path):
    conn = _fresh_db(tmp_path)
    loader._insert_advices(conn, [_securities_advice("call", 12345.67)])

    loader._purge_stale_document_rows(conn)

    assert conn.execute("SELECT COUNT(*) FROM advices").fetchone()[0] == 0
    conn.close()
