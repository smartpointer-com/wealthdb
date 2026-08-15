"""Unit tests for the raiffeisen_at silver loader — synthetic bronze only
(no real IBANs, balances, or payees). Exercises the history-JSON parser
(signed amount, value date, category, stable id, malformed-row skipping), the
account-detail curation (dropping the card block + holder name), the daily
closing-balance series, the statement inventory, and the idempotent per-run
load.
"""
from __future__ import annotations

import json
import sys
from datetime import datetime, timezone
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import load  # noqa: E402

MIGRATIONS = Path(__file__).resolve().parent.parent / "migrations"

# Synthetic IBAN: placeholder-letter body (root CLAUDE.md §4) with a numeric
# tail so the last-4 mask has something to show. Never a real IBAN.
IBAN = "ATkkBBBBBKKKKKKK1234"


def _epoch(y, m, d):
    return int(datetime(y, m, d, tzinfo=timezone.utc).timestamp())


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
