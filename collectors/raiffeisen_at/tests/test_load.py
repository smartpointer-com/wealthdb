"""Unit tests for the raiffeisen_at silver loader — synthetic bronze only
(no real IBANs, balances, or payees). Exercises the history-JSON parser
(signed amount, value date, category, stable id, malformed-row skipping), the
account-detail curation (dropping the card block + holder name), the daily
closing-balance series, the statement inventory, the idempotent per-run load,
and the supplied-listing pass end to end through `main` (DESIGN.md §I).
"""
from __future__ import annotations

import json
import sqlite3
import sys
from datetime import date, datetime, timezone
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))
sys.path.insert(0, str(Path(__file__).resolve().parent))

import load  # noqa: E402
from listing_fixtures import DAY, IBAN as LEDGER_IBAN, Ledger, ledger  # noqa: E402

MIGRATIONS = Path(__file__).resolve().parent.parent / "migrations"

# Synthetic IBAN: placeholder-letter body (root AGENTS.md §4) with a numeric
# tail so the last-4 mask has something to show. Never a real IBAN.
IBAN = "ATkkBBBBBKKKKKKK1234"


def _epoch(y, m, d):
    return int(datetime(y, m, d, tzinfo=timezone.utc).timestamp())


def _day(epoch):
    return datetime.fromtimestamp(epoch, tz=timezone.utc).date()


# ============================================================
# Scalar parsers
# ============================================================

def test_parse_date_variants():
    assert load.parse_date("2026-07-15") == _epoch(2026, 7, 15)
    assert load.parse_date("2026-07-15T01:02:03.4") == _epoch(2026, 7, 15)
    assert load.parse_date("garbage") is None
    assert load.parse_date(None) is None


def test_parse_money_variants():
    assert load.parse_money(-12.34) == -12.34
    assert load.parse_money("500.0") == 500.0
    assert load.parse_money("") is None
    assert load.parse_money(None) is None


def test_amount_and_currency_is_signed_at_source():
    assert load._amount_and_currency(
        {"betrag": {"amount": -12.34, "currency": "EUR"}}) == (-12.34, "EUR")
    assert load._amount_and_currency(
        {"betrag": {"amount": 500.0, "currencyCode": "EUR"}}) == (500.0, "EUR")
    assert load._amount_and_currency({"betrag": None}) == (None, None)
    assert load._amount_and_currency({}) == (None, None)


# ============================================================
# History rows
# ============================================================

def _tx(tid, buchung, amount, purpose, party="ACME GmbH",
        category="income_other", valuta=None):
    return {"id": tid, "buchungstag": buchung, "valuta": valuta or buchung,
            "betrag": {"amount": amount, "currency": "EUR"},
            "verwendungszweckZeile1": purpose,
            "transaktionsteilnehmerZeile1": party, "kategorieCode": category}


def test_history_rows_project_and_sign():
    txs = [
        _tx(1001, "2026-07-15", -12.34, "TV/PHONE", category="tv_phone_internet"),
        _tx(1002, "2026-07-31", 500.00, "SALARY", valuta="2026-08-01"),
    ]
    rows = load.history_rows(IBAN, txs)
    assert [r["txn_id"] for r in rows] == ["1001", "1002"]
    assert rows[0]["amount"] == -12.34 and rows[0]["kind"] == "DEBIT"
    assert rows[0]["currency"] == "EUR"
    assert rows[0]["category"] == "tv_phone_internet"
    assert rows[0]["description"] == "TV/PHONE"
    assert rows[0]["counterparty"] == "ACME GmbH"
    assert rows[0]["posted_at"] == _epoch(2026, 7, 15)
    assert rows[1]["amount"] == 500.00 and rows[1]["kind"] == "CREDIT"
    # The value date is kept distinct from the booking date.
    assert rows[1]["posted_at"] == _epoch(2026, 7, 31)
    assert rows[1]["value_at"] == _epoch(2026, 8, 1)


def test_history_rows_skip_malformed_and_synthesize_id():
    txs = [
        {"id": 1, "buchungstag": "bad", "betrag": {"amount": 1.0}},   # no date
        {"id": 2, "buchungstag": "2026-07-01"},                        # no amount
        _tx(None, "2026-07-02", -9.99, "PENDING"),                     # no id
    ]
    rows = load.history_rows(IBAN, txs)
    assert len(rows) == 1
    assert rows[0]["txn_id"].startswith("syn_")
    # The synthetic id is stable across re-parses.
    assert load.history_rows(IBAN, txs)[0]["txn_id"] == rows[0]["txn_id"]


# ============================================================
# Account-detail curation
# ============================================================

def _details():
    return {"konto": {"kontoart": "G"}, "detailgruppen": [
        {"ueberschrift": "Kontoinformationen", "details": [
            {"bezeichnung": "IBAN", "inhalt": ["AT.. ....  ....  1234"]},
            {"bezeichnung": "BIC", "inhalt": ["RZTESTXXXXX"]},
            {"bezeichnung": "Kontoführendes Institut", "inhalt": ["Test Bank"]},
            {"bezeichnung": "Währung", "inhalt": ["EUR"]},
            {"bezeichnung": "Kontobezeichnung", "inhalt": ["Doe John"]},
            {"bezeichnung": "Kontoart", "inhalt": ["Gehaltekonto"]},
        ]},
        {"ueberschrift": "Zinsinformationen", "details": [
            {"bezeichnung": "Aktueller Zinssatz Haben",
             "inhalt": ["0,000% ab 01.01.2020"]},
            {"bezeichnung": "Aktueller Zinssatz Soll",
             "inhalt": ["8,500% ab 01.01.2020"]},
        ]},
        {"ueberschrift": "Karteninformationen", "details": [
            {"bezeichnung": "Karte", "inhalt": ["Debit Mastercard", "Nr. 999"]},
        ]},
    ]}


def test_parse_details_curates_and_drops_cards_and_name():
    d = load.parse_details(_details())
    assert d["kontoart"] == "Gehaltekonto"
    assert d["currency"] == "EUR"
    assert d["institution"] == "Test Bank"
    assert d["bic"] == "RZTESTXXXXX"
    assert d["interest_credit"] == "0,000% ab 01.01.2020"
    assert d["interest_debit"] == "8,500% ab 01.01.2020"
    # The card block and the account-holder name are NOT promoted.
    assert "Karte" not in d and "Debit Mastercard" not in json.dumps(d)
    assert "Doe John" not in json.dumps(d)


def test_parse_details_tolerates_junk():
    assert load.parse_details(None) == {}
    assert load.parse_details({"detailgruppen": ["x", {}]}) == {}


# ============================================================
# Daily balances + mask + statement date
# ============================================================

def test_balance_rows():
    rows = load.balance_rows({"tagessalden": [
        {"tag": "2026-08-10", "saldo": 1000.00},
        {"tag": "2023-07-31", "saldo": 1500.00},
        {"tag": "bad", "saldo": 1.0},          # skipped
    ]})
    assert rows == [
        {"balance_date": _epoch(2026, 8, 10), "balance": 1000.00},
        {"balance_date": _epoch(2023, 7, 31), "balance": 1500.00},
    ]


def test_mask_is_last_four_only():
    assert load._mask(IBAN) == "…1234"
    assert load._mask("ATkkBBBBB") is None      # no 4 digits → None


def test_statement_date_from_name():
    assert load._statement_date_from_name("2026-08-01_EAZ_ELOOE_9.pdf") == _epoch(2026, 8, 1)
    assert load._statement_date_from_name("2023-04-01_KDM_RBGO9_v1.pdf") == _epoch(2023, 4, 1)
    assert load._statement_date_from_name("no-date.pdf") is None


# ============================================================
# End-to-end per-run load
# ============================================================

def _write_bronze_run(root: Path, slug: str) -> Path:
    run = root / slug
    for sub in ("details", "history", "balances"):
        (run / sub).mkdir(parents=True)
    (run / "statements" / IBAN).mkdir(parents=True)
    (run / "accounts.json").write_text(json.dumps([{
        "iban": IBAN, "type": "KONTO",
        "balance": {"amount": 1000.00, "currency": "EUR"},
        "available": {"amount": 1000.00, "currency": "EUR"}}]))
    (run / "details" / f"{IBAN}.json").write_text(json.dumps(_details()))
    (run / "history" / f"{IBAN}.json").write_text(json.dumps({
        "iban": IBAN, "minBuchungstag": "2023-08-01", "transactions": [
            _tx(1001, "2026-07-15", -12.34, "TV/PHONE",
                category="tv_phone_internet"),
            _tx(1002, "2026-07-31", 500.00, "SALARY"),
        ]}))
    (run / "balances" / f"{IBAN}.json").write_text(json.dumps({
        "von": "2023-07-31", "bis": "2026-08-15", "kontostand": 1000.00,
        "tagessalden": [{"tag": "2026-07-31", "saldo": 1000.00},
                        {"tag": "2026-07-15", "saldo": 900.00}]}))
    (run / "statements" / IBAN / "2026-08-01_EAZ_ELOOE_9.pdf").write_bytes(
        b"%PDF-1.4 synthetic")
    (run / "run.json").write_text(json.dumps(
        {"source": "raiffeisen_at", "status": "complete"}))
    return run


def _load(tmp_path: Path):
    conn = load.silver.open_db(tmp_path / "raiffeisen_at.db")
    load.silver.apply_migrations(conn, MIGRATIONS)
    return conn


def test_load_run_end_to_end(tmp_path):
    bronze_dir = tmp_path / "bronze"
    _write_bronze_run(bronze_dir, "20260815T120000Z")
    conn = _load(tmp_path)
    assert load.load_run(conn, bronze_dir / "20260815T120000Z") is True

    acct = conn.execute("SELECT account_external_id, account_type, mask, "
                        "currency, balance, payload FROM accounts").fetchone()
    assert acct["account_external_id"] == IBAN
    assert acct["account_type"] == "Gehaltekonto"
    assert acct["mask"] == "…1234"
    assert acct["currency"] == "EUR"
    assert acct["balance"] == 1000.00
    # The curated interest rate rides in the payload; card + name do not.
    assert "interest_credit" in acct["payload"]
    assert "Debit Mastercard" not in acct["payload"]

    txs = conn.execute("SELECT txn_id, amount, kind, category, value_at, "
                       "counterparty FROM transactions ORDER BY posted_at").fetchall()
    assert [t["txn_id"] for t in txs] == ["1001", "1002"]
    assert txs[0]["amount"] == -12.34 and txs[0]["kind"] == "DEBIT"
    assert txs[0]["category"] == "tv_phone_internet"
    assert txs[1]["amount"] == 500.00 and txs[1]["kind"] == "CREDIT"
    assert all(t["counterparty"] == "ACME GmbH" for t in txs)

    bals = conn.execute("SELECT balance_date, balance FROM daily_balances "
                        "ORDER BY balance_date").fetchall()
    assert [b["balance"] for b in bals] == [900.00, 1000.00]

    doc = conn.execute("SELECT account_external_id, doc_date, doc_kind, "
                       "file_format FROM documents").fetchone()
    assert doc["account_external_id"] == IBAN
    assert doc["doc_date"] == _epoch(2026, 8, 1)
    assert doc["doc_kind"] == "statement" and doc["file_format"] == "pdf"


def test_load_run_is_idempotent(tmp_path):
    bronze_dir = tmp_path / "bronze"
    _write_bronze_run(bronze_dir, "20260815T120000Z")
    conn = _load(tmp_path)
    run = bronze_dir / "20260815T120000Z"
    assert load.load_run(conn, run) is True
    assert load.load_run(conn, run) is False        # already-loaded snapshot
    for table, n in (("transactions", 2), ("accounts", 1),
                     ("daily_balances", 2), ("documents", 1)):
        assert conn.execute(f"SELECT COUNT(*) FROM {table}").fetchone()[0] == n


def test_account_content_dedup_across_runs(tmp_path):
    bronze_dir = tmp_path / "bronze"
    _write_bronze_run(bronze_dir, "20260815T120000Z")
    _write_bronze_run(bronze_dir, "20260816T120000Z")   # identical roster
    conn = _load(tmp_path)
    for slug in ("20260815T120000Z", "20260816T120000Z"):
        load.load_run(conn, bronze_dir / slug)
    # Unchanged roster payload → a single accounts row despite two runs.
    assert conn.execute("SELECT COUNT(*) FROM accounts").fetchone()[0] == 1


def test_non_complete_run_skipped(tmp_path):
    bronze_dir = tmp_path / "bronze"
    run = _write_bronze_run(bronze_dir, "20260815T120000Z")
    (run / "run.json").write_text(json.dumps({"status": "in-progress"}))
    conn = _load(tmp_path)
    assert load.load_run(conn, run) is False
    assert conn.execute("SELECT COUNT(*) FROM transactions").fetchone()[0] == 0


# ============================================================
# Supplied transaction listings (DESIGN.md §I)
# ============================================================
#
# Bronze runs and listings are both cut from one synthetic ledger
# (listing_fixtures), so they agree unless a test says otherwise. A "PDF" here
# holds the listing's text and the extractor is stubbed to read it back.

LEDGER = ledger()                                       # 2024-01-01 .. 2024-06-28


@pytest.fixture(autouse=True)
def _text_extractor(monkeypatch):
    monkeypatch.setattr(load.pdftotext, "layout_text",
                        lambda path: Path(path).read_text(encoding="utf-8"))


def _ledger_run(bronze_dir: Path, since: date, run_day: date, lead: int = 1, *,
                ledger: Ledger = LEDGER, complete: bool = True,
                returned_from: date | None = None, seen_on_run_day=()) -> Path:
    """A download of `ledger` over [since, run_day], as download.py lays it
    out. `returned_from` cuts the history walk short (the oldest days are the
    ones missed); `seen_on_run_day` are the bookings of the run's own day the
    download already saw, with the intraday balance that goes with them."""
    run = bronze_dir / f"{run_day:%Y%m%d}T120000Z"
    for sub in ("details", "history", "balances"):
        (run / sub).mkdir(parents=True)
    (run / "details" / f"{LEDGER_IBAN}.json").write_text(json.dumps(_details()))
    (run / "accounts.json").write_text(json.dumps([{
        "iban": LEDGER_IBAN, "type": "KONTO",
        "balance": {"amount": ledger.balance_at(run_day) / 100, "currency": "EUR"}}]))
    first = returned_from or since
    rows = [{"id": f"L{d:%Y%m%d}{n}", "buchungstag": d.isoformat(),
             "valuta": d.isoformat(), "betrag": {"amount": a / 100, "currency": "EUR"},
             "verwendungszweckZeile1": "ITEM"}
            for d in sorted(ledger.postings) if first <= d < run_day
            for n, a in enumerate(ledger.postings[d])]
    rows += [{"id": f"L{run_day:%Y%m%d}{n}", "buchungstag": run_day.isoformat(),
              "valuta": run_day.isoformat(),
              "betrag": {"amount": a / 100, "currency": "EUR"}}
             for n, a in enumerate(seen_on_run_day)]
    (run / "history" / f"{LEDGER_IBAN}.json").write_text(json.dumps(
        {"iban": LEDGER_IBAN, "minBuchungstag": None, "complete": complete,
         "transactions": rows}))
    von = since - lead * DAY
    salden = [{"tag": von.isoformat(), "saldo": ledger.balance_at(von) / 100}] + [
        {"tag": d.isoformat(), "saldo": ledger.balance_at(d) / 100}
        for d in sorted(ledger.postings) if von < d < run_day]
    if seen_on_run_day:
        salden.append({"tag": run_day.isoformat(), "saldo": (
            ledger.balance_at(run_day - DAY) + sum(seen_on_run_day)) / 100})
    (run / "balances" / f"{LEDGER_IBAN}.json").write_text(json.dumps(
        {"von": von.isoformat(), "bis": run_day.isoformat(), "tagessalden": salden}))
    (run / "run.json").write_text(json.dumps(
        {"source": "raiffeisen_at", "status": "complete",
         "since": since.isoformat(), "until": run_day.isoformat()}))
    return run


def _drop_listing(bronze_dir: Path, name: str, coverage_start: date,
                  print_day: date, *, ledger: Ledger = LEDGER, **kw) -> Path:
    (bronze_dir / "supplied").mkdir(exist_ok=True)
    path = bronze_dir / "supplied" / name
    path.write_text(ledger.listing_text(coverage_start, print_day, **kw), encoding="utf-8")
    return path


def _main(bronze_dir: Path, *extra) -> None:
    assert load.main(["--bronze-dir", str(bronze_dir), *extra]) == 0


def _db(bronze_dir: Path) -> sqlite3.Connection:
    return sqlite3.connect(bronze_dir / "raiffeisen_at.db")


def _supplied(conn) -> list[tuple]:
    return conn.execute("SELECT txn_id, posted_at, amount, payload FROM transactions "
                        "WHERE source='supplied_listing' ORDER BY txn_id").fetchall()


def _count(conn, sql) -> int:
    return conn.execute(sql).fetchone()[0]


def _days(conn, source) -> set:
    return {r[0] for r in conn.execute(
        "SELECT DISTINCT posted_at FROM transactions WHERE source=?", (source,))}


def test_supplied_listings_load_by_default(tmp_path):
    bronze_dir = tmp_path / "bronze"
    _ledger_run(bronze_dir, date(2024, 4, 1), date(2024, 6, 29))
    _drop_listing(bronze_dir, "LISTING.PDF", date(2024, 1, 1), date(2024, 6, 30))
    _main(bronze_dir)                                   # no flag, upper-case suffix
    conn = _db(bronze_dir)
    expected = LEDGER.between(date(2024, 1, 1), date(2024, 3, 31))
    assert len(_supplied(conn)) == len(expected)
    assert _count(conn, "SELECT COUNT(*) FROM daily_balances "
                        "WHERE source='supplied_listing'") == len(
        {p.day for p in expected} - {date(2024, 3, 31)})
    assert not _days(conn, "supplied_listing") & _days(conn, "history")
    doc = conn.execute("SELECT payload FROM documents WHERE doc_kind="
                       "'transaction_listing'").fetchone()
    assert json.loads(doc[0])["status"] == "accepted"
    # this load ingested a download, which already moved the load clock
    assert _count(conn, "SELECT COUNT(*) FROM dump_runs") == 1
    row = conn.execute("SELECT kind, description, category, payload FROM transactions "
                       "WHERE source='supplied_listing' LIMIT 1").fetchone()
    assert row[1] == "ITEM" and row[2] is None
    assert json.loads(row[3])["doc_sha256"]


def test_no_supplied_dir_is_a_no_op(tmp_path):
    bronze_dir = tmp_path / "bronze"
    _ledger_run(bronze_dir, date(2024, 4, 1), date(2024, 6, 29))
    _main(bronze_dir)
    conn = _db(bronze_dir)
    assert _supplied(conn) == []
    assert _count(conn, "SELECT COUNT(*) FROM dump_runs") == 1


def test_an_unchanged_load_writes_nothing(tmp_path):
    bronze_dir = tmp_path / "bronze"
    _ledger_run(bronze_dir, date(2024, 4, 1), date(2024, 6, 29))
    _drop_listing(bronze_dir, "a.pdf", date(2024, 1, 1), date(2024, 6, 30))
    _main(bronze_dir)
    before = _supplied(_db(bronze_dir))
    _main(bronze_dir)
    conn = _db(bronze_dir)
    assert _supplied(conn) == before
    assert _count(conn, "SELECT COUNT(*) FROM dump_runs") == 1


def test_a_listing_added_between_downloads_moves_the_load_clock(tmp_path):
    bronze_dir = tmp_path / "bronze"
    _ledger_run(bronze_dir, date(2024, 4, 1), date(2024, 6, 29))
    _main(bronze_dir)
    _drop_listing(bronze_dir, "a.pdf", date(2024, 1, 1), date(2024, 6, 30))
    _main(bronze_dir)
    conn = _db(bronze_dir)
    stamps = conn.execute("SELECT snapshot_at, run_dir FROM dump_runs "
                          "ORDER BY snapshot_at").fetchall()
    # one past the latest stamp, never the wall clock (a download in flight
    # carries an earlier slug and must still move the clock when it lands)
    assert len(stamps) == 2 and stamps[1][1].endswith("supplied")
    assert stamps[1][0] == stamps[0][0] + 1
    assert _supplied(conn)


def test_other_files_in_supplied_are_skipped(tmp_path, caplog):
    bronze_dir = tmp_path / "bronze"
    _ledger_run(bronze_dir, date(2024, 4, 1), date(2024, 6, 29))
    _drop_listing(bronze_dir, "a.pdf", date(2024, 1, 1), date(2024, 6, 30))
    (bronze_dir / "supplied" / "notice.pdf").write_text("Entgeltmitteilung\n")
    (bronze_dir / "supplied" / "copy.pdf").write_bytes(
        (bronze_dir / "supplied" / "a.pdf").read_bytes())
    (bronze_dir / "supplied" / "readme.txt").write_text("not a pdf")
    _main(bronze_dir)
    conn = _db(bronze_dir)
    assert _count(conn, "SELECT COUNT(*) FROM documents "
                        "WHERE doc_kind='transaction_listing'") == 1
    assert _supplied(conn)
    assert "notice.pdf is not a transaction listing" in caplog.text


def test_a_listing_for_an_unknown_account_or_a_filtered_one_stitches_nothing(tmp_path):
    bronze_dir = tmp_path / "bronze"
    _ledger_run(bronze_dir, date(2024, 4, 1), date(2024, 6, 29))
    _drop_listing(bronze_dir, "other.pdf", date(2024, 1, 1), date(2024, 6, 30),
                  account="9999")
    _drop_listing(bronze_dir, "filtered.pdf", date(2024, 1, 1), date(2024, 6, 30),
                  key_text="Gutschriften", hour=10)
    _main(bronze_dir)
    conn = _db(bronze_dir)
    assert _supplied(conn) == []
    docs = conn.execute("SELECT filename, payload FROM documents "
                        "WHERE doc_kind='transaction_listing'").fetchall()
    assert [d[0] for d in docs] == ["filtered.pdf"]       # unbound: not recorded
    payload = json.loads(docs[0][1])
    assert payload["status"] == "rejected" and "text key" in payload["reason"]


def test_a_deeper_download_takes_days_over_from_a_listing(tmp_path):
    bronze_dir = tmp_path / "bronze"
    _ledger_run(bronze_dir, date(2024, 4, 1), date(2024, 6, 29))
    _drop_listing(bronze_dir, "a.pdf", date(2024, 1, 1), date(2024, 6, 30))
    _main(bronze_dir)
    _ledger_run(bronze_dir, date(2024, 2, 1), date(2024, 6, 30))
    _main(bronze_dir)
    conn = _db(bronze_dir)
    supplied = _days(conn, "supplied_listing")
    assert supplied and max(supplied) < _epoch(2024, 2, 1)
    assert not supplied & _days(conn, "history")
    total = _count(conn, "SELECT COUNT(*) FROM transactions")
    assert total == sum(len(v) for d, v in LEDGER.postings.items() if d < date(2024, 6, 30))


def test_force_rebuilds_the_same_layer(tmp_path):
    bronze_dir = tmp_path / "bronze"
    _ledger_run(bronze_dir, date(2024, 4, 1), date(2024, 6, 29))
    _drop_listing(bronze_dir, "a.pdf", date(2024, 1, 1), date(2024, 6, 30))
    _main(bronze_dir)
    before = _supplied(_db(bronze_dir))
    _main(bronze_dir, "--force")
    assert _supplied(_db(bronze_dir)) == before


def test_a_regressed_parse_keeps_the_layer_until_the_file_is_removed(tmp_path, monkeypatch):
    bronze_dir = tmp_path / "bronze"
    _ledger_run(bronze_dir, date(2024, 4, 1), date(2024, 6, 29))
    path = _drop_listing(bronze_dir, "a.pdf", date(2024, 1, 1), date(2024, 6, 30))
    _main(bronze_dir)
    before = _supplied(_db(bronze_dir))
    # the same file now parses to a broken chain (a parser or poppler change)
    broken = LEDGER.listing_text(date(2024, 1, 1), date(2024, 6, 30),
                                 saldo_offsets={date(2024, 2, 3): 1})
    monkeypatch.setattr(load.pdftotext, "layout_text", lambda p: broken)
    _main(bronze_dir)
    assert _supplied(_db(bronze_dir)) == before
    path.unlink()                                          # an explicit removal
    _main(bronze_dir)
    assert _supplied(_db(bronze_dir)) == []


def test_a_missing_extractor_keeps_the_layer(tmp_path, monkeypatch):
    bronze_dir = tmp_path / "bronze"
    _ledger_run(bronze_dir, date(2024, 4, 1), date(2024, 6, 29))
    _drop_listing(bronze_dir, "a.pdf", date(2024, 1, 1), date(2024, 6, 30))
    _main(bronze_dir)
    before = _supplied(_db(bronze_dir))

    def missing(path):
        raise load.pdftotext.ToolMissing("pdftotext (poppler-utils) is not installed")
    monkeypatch.setattr(load.pdftotext, "layout_text", missing)
    _main(bronze_dir)
    assert _supplied(_db(bronze_dir)) == before


def test_an_absent_dir_keeps_the_layer_and_an_empty_one_clears_it(tmp_path):
    bronze_dir = tmp_path / "bronze"
    _ledger_run(bronze_dir, date(2024, 4, 1), date(2024, 6, 29))
    path = _drop_listing(bronze_dir, "a.pdf", date(2024, 1, 1), date(2024, 6, 30))
    _main(bronze_dir)
    before = _supplied(_db(bronze_dir))
    moved = tmp_path / "a.pdf"
    path.rename(moved)
    (bronze_dir / "supplied").rmdir()
    _main(bronze_dir)
    assert _supplied(_db(bronze_dir)) == before
    (bronze_dir / "supplied").mkdir()
    _main(bronze_dir)
    conn = _db(bronze_dir)
    assert _supplied(conn) == []
    assert _count(conn, "SELECT COUNT(*) FROM daily_balances WHERE source='supplied_listing'") == 0
    assert _count(conn, "SELECT COUNT(*) FROM documents WHERE doc_kind='transaction_listing'") == 0


def test_a_stitch_that_writes_onto_a_live_day_is_refused(tmp_path, monkeypatch):
    bronze_dir = tmp_path / "bronze"
    _ledger_run(bronze_dir, date(2024, 4, 1), date(2024, 6, 29))
    path = _drop_listing(bronze_dir, "a.pdf", date(2024, 1, 1), date(2024, 6, 30))
    _main(bronze_dir)
    before = _supplied(_db(bronze_dir))
    # a listing with a posting live never had, on a day live certifies ...
    extra = dict(LEDGER.postings)
    extra[date(2024, 5, 2)] = [-700]
    path.write_text(Ledger(LEDGER.start, LEDGER.opening, extra).listing_text(
        date(2024, 1, 1), date(2024, 6, 30)), encoding="utf-8")

    def greedy(live, candidates):          # ... and a stitch that takes every day
        return {c.key: load.stitch.Outcome("accepted", owned=[(c.start, c.print_day - DAY)])
                for c in candidates}
    monkeypatch.setattr(load.stitch, "stitch", greedy)
    with pytest.raises(RuntimeError, match="certifies"):
        _main(bronze_dir)
    assert _supplied(_db(bronze_dir)) == before


def test_a_cut_short_download_does_not_take_days_it_never_fetched(tmp_path):
    bronze_dir = tmp_path / "bronze"
    _ledger_run(bronze_dir, date(2024, 4, 1), date(2024, 6, 29))
    _drop_listing(bronze_dir, "a.pdf", date(2024, 1, 1), date(2024, 6, 30))
    _main(bronze_dir)
    # a deeper download whose walk stopped after reaching back only to March
    _ledger_run(bronze_dir, date(2024, 2, 1), date(2024, 6, 30), complete=False,
                returned_from=date(2024, 3, 1))
    _main(bronze_dir)
    conn = _db(bronze_dir)
    supplied = {_day(d) for d in _days(conn, "supplied_listing")}
    assert any(date(2024, 2, 1) <= d < date(2024, 3, 1) for d in supplied)
    assert not any(d > date(2024, 3, 1) for d in supplied)
    total = _count(conn, "SELECT COUNT(*) FROM transactions")
    assert total == sum(len(v) for d, v in LEDGER.postings.items() if d < date(2024, 6, 30))


def test_run_windows_certify_what_the_walk_reached(tmp_path):
    full = _ledger_run(tmp_path, date(2024, 4, 1), date(2024, 6, 29))
    cut = _ledger_run(tmp_path, date(2024, 2, 1), date(2024, 6, 30), complete=False,
                      returned_from=date(2024, 3, 1))
    windows = {w[1]: w[2:] for w in load.run_windows(full)}
    assert windows["transactions"] == (_epoch(2024, 4, 1), _epoch(2024, 6, 29))
    assert windows["balances"] == (_epoch(2024, 3, 31), _epoch(2024, 6, 29))
    first_returned = min(d for d in LEDGER.postings if d >= date(2024, 3, 1))
    txn = next(w for w in load.run_windows(cut) if w[1] == "transactions")
    assert _day(txn[2]) == first_returned + DAY


def _partly_seen_ledger():
    postings = dict(LEDGER.postings)
    postings[date(2024, 1, 31)] = [-1_000, -500]        # morning, afternoon
    return Ledger(LEDGER.start, LEDGER.opening, postings)


def test_a_listing_tops_up_a_day_a_download_saw_only_partly(tmp_path):
    bronze_dir = tmp_path / "bronze"
    split = _partly_seen_ledger()
    _ledger_run(bronze_dir, date(2024, 1, 1), date(2024, 1, 31), ledger=split,
                seen_on_run_day=[-1_000])
    _ledger_run(bronze_dir, date(2024, 4, 1), date(2024, 6, 29), ledger=split)
    _drop_listing(bronze_dir, "a.pdf", date(2024, 1, 1), date(2024, 6, 30), ledger=split)
    _main(bronze_dir)
    conn = _db(bronze_dir)
    day = _epoch(2024, 1, 31)
    by_source = conn.execute("SELECT source, amount FROM transactions WHERE posted_at=? "
                             "ORDER BY source", (day,)).fetchall()
    assert by_source == [("history", -10.0), ("supplied_listing", -5.0)]
    # the listing's closing balance replaces the download's intraday one
    bal = conn.execute("SELECT source, balance FROM daily_balances WHERE balance_date=?",
                       (day,)).fetchone()
    assert bal == ("supplied_listing", split.balance_at(date(2024, 1, 31)) / 100)
    # the gap to the next download is filled too
    assert any(date(2024, 2, 1) <= _day(d) < date(2024, 4, 1)
               for d in _days(conn, "supplied_listing"))
    _main(bronze_dir)                                   # stable
    assert conn.execute("SELECT COUNT(*) FROM dump_runs").fetchone()[0] == 2


def test_a_later_download_that_sees_the_whole_day_takes_it_back(tmp_path):
    bronze_dir = tmp_path / "bronze"
    split = _partly_seen_ledger()
    _ledger_run(bronze_dir, date(2024, 1, 1), date(2024, 1, 31), ledger=split,
                seen_on_run_day=[-1_000])
    _ledger_run(bronze_dir, date(2024, 4, 1), date(2024, 6, 29), ledger=split)
    _drop_listing(bronze_dir, "a.pdf", date(2024, 1, 1), date(2024, 6, 30), ledger=split)
    _main(bronze_dir)
    _ledger_run(bronze_dir, date(2024, 1, 15), date(2024, 6, 30), ledger=split)
    _main(bronze_dir)
    conn = _db(bronze_dir)
    rows = conn.execute("SELECT source, amount FROM transactions WHERE posted_at=?",
                        (_epoch(2024, 1, 31),)).fetchall()
    assert sorted(rows) == [("history", -10.0), ("history", -5.0)]


def test_a_file_with_an_impossible_date_is_skipped(tmp_path):
    bronze_dir = tmp_path / "bronze"
    _ledger_run(bronze_dir, date(2024, 4, 1), date(2024, 6, 29))
    path = _drop_listing(bronze_dir, "bad.pdf", date(2024, 1, 1), date(2024, 6, 30))
    path.write_text(path.read_text().replace("30.06.2024/", "31.02.2024/"))
    _drop_listing(bronze_dir, "good.pdf", date(2024, 1, 1), date(2024, 6, 30), hour=10)
    _main(bronze_dir)
    conn = _db(bronze_dir)
    assert [r[0] for r in conn.execute("SELECT filename FROM documents WHERE "
                                       "doc_kind='transaction_listing'")] == ["good.pdf"]
    assert _supplied(conn)


def test_a_trim_without_a_download_still_moves_the_load_clock(tmp_path):
    bronze_dir = tmp_path / "bronze"
    _ledger_run(bronze_dir, date(2024, 4, 1), date(2024, 6, 29))
    _drop_listing(bronze_dir, "a.pdf", date(2024, 1, 1), date(2024, 6, 30))
    _main(bronze_dir)
    conn = _db(bronze_dir)
    # a download of Feb–Mar that a crashed load recorded but never trimmed for
    conn.execute("INSERT INTO run_windows VALUES (?,?,?,?,?)",
                 (1, LEDGER_IBAN, "transactions", _epoch(2024, 2, 1), _epoch(2024, 4, 1)))
    conn.commit()
    stamps = _count(conn, "SELECT COUNT(*) FROM dump_runs")
    _main(bronze_dir)
    supplied = {_day(d) for d in _days(conn, "supplied_listing")}
    assert supplied and not any(date(2024, 2, 1) <= d < date(2024, 4, 1) for d in supplied)
    assert _count(conn, "SELECT COUNT(*) FROM dump_runs") == stamps + 1


def test_windows_of_runs_loaded_before_they_were_recorded_are_backfilled(tmp_path):
    bronze_dir = tmp_path / "bronze"
    _ledger_run(bronze_dir, date(2024, 4, 1), date(2024, 6, 29))
    _main(bronze_dir)
    conn = _db(bronze_dir)
    conn.execute("DELETE FROM run_windows")
    conn.commit()
    _drop_listing(bronze_dir, "a.pdf", date(2024, 1, 1), date(2024, 6, 30))
    _main(bronze_dir)
    assert _count(conn, "SELECT COUNT(*) FROM run_windows") == 2
    supplied = {_day(d) for d in _days(conn, "supplied_listing")}
    assert supplied and max(supplied) < date(2024, 4, 1)


def test_supplied_balances_give_way_to_a_live_balance(tmp_path):
    bronze_dir = tmp_path / "bronze"
    _ledger_run(bronze_dir, date(2024, 4, 1), date(2024, 6, 29))
    _drop_listing(bronze_dir, "a.pdf", date(2024, 1, 1), date(2024, 6, 30))
    _main(bronze_dir)
    conn = _db(bronze_dir)
    live_days = {r[0] for r in conn.execute(
        "SELECT balance_date FROM daily_balances WHERE source='history'")}
    supplied_days = {r[0] for r in conn.execute(
        "SELECT balance_date FROM daily_balances WHERE source='supplied_listing'")}
    assert supplied_days and not live_days & supplied_days
    assert max(supplied_days) < min(live_days)
