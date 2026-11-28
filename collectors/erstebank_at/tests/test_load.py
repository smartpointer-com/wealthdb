"""Unit tests for the erstebank_at silver loader — synthetic bronze only.
Covers the projections and their signs, the Vienna dates, the daily-close
resolver (rows of a day out of order), the per-run load end to end through
`main`, idempotency and --force."""
from __future__ import annotations

import json
import sqlite3
import sys
from datetime import date, datetime, timedelta, timezone
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import load  # noqa: E402

# Synthetic ids: the IBAN uses the placeholder-letter pattern with a numeric
# tail, so the last-4 mask has something to show. Never a real value.
IBAN = "ATkkBBBBBRRRRRRR1234"
ACCOUNT_ID = "ACCT0000000000A1"
CARD_ID = "CARD0000000000C1"
SHADOW = "SHDW0000000000S1"
VIENNA_OFFSET = timedelta(hours=1)          # winter: midnight is 23:00 UTC


def _ms(day: date) -> int:
    """A Vienna midnight in epoch ms, as George sends dates (winter)."""
    midnight = datetime(day.year, day.month, day.day, tzinfo=timezone.utc)
    return int((midnight - VIENNA_OFFSET).timestamp() * 1000)


def _epoch(day: date) -> int:
    return int(datetime(day.year, day.month, day.day,
                        tzinfo=timezone.utc).timestamp())


def _m(cents: int) -> dict:
    return {"value": cents, "precision": 2, "currency": "EUR"}


def _tx(txn_id, day, amount, balance, *, tx_type="POSPAYMENT",
        relevant=True):
    return {"id": txn_id, "bookingDate": _ms(day), "valuationDate": _ms(day),
            "amount": _m(amount), "balance": _m(balance), "txType": tx_type,
            "txDirection": "OUTGOING" if amount < 0 else "INCOMING",
            "title": "Synthetic Shop", "partnerName": "Synthetic Partner",
            "additionalTexts": {"lineItems": ["ref one", None, "ref two"]},
            "flags": ["IS_VISIBLE"] + (["IS_BAL_RELEVANT"] if relevant else [])}


def _write(path: Path, obj) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(obj))


def make_run(root: Path, slug: str, *, status="complete", account_rows=None,
             card_rows=None, account_balance=100000, card_balance=-25000,
             pdf=b"%PDF-1.4 synthetic statement"):
    run = root / slug
    _write(run / "run.json", {"status": status})
    _write(run / "accounts.json", [{
        "id": ACCOUNT_ID, "accountno": {"iban": IBAN, "bic": "SYNTHBICXXX"},
        "currency": "EUR", "product": "SYNTH_GIRO",
        "productI18N": "Synthetic Giro", "balance": _m(account_balance),
        "overdraftData": {"approvedLimit": _m(50000)},
        "owners": [{"name": "Synthetic Holder"}]}])
    _write(run / "raw" / "cards.json", {"cards": [
        {"id": "DEBIT00000000001", "type": "BANK_CARD"},
        {"id": CARD_ID, "type": "CREDIT", "creditCardShadowAccountId": SHADOW,
         "number": "000000XXXXXX4321", "product": "SYNTH_CARD",
         "productI18N": "Synthetic Card", "balance": _m(card_balance),
         "limit": _m(300000), "cardHolder": "Synthetic Holder",
         "imageData": "x"}]})
    product = f"{SHADOW}-{CARD_ID}"
    _write(run / "history" / f"{ACCOUNT_ID}.json", {
        "product_id": ACCOUNT_ID, "kind": "account", "complete": True,
        "transactions": account_rows if account_rows is not None else [
            _tx("t3", date(2026, 1, 15), -1000, 99000),
            _tx("t2", date(2026, 1, 14), 50000, 100000),
            _tx("info", date(2026, 1, 14), 0, 100000, tx_type="BANKINFO",
                relevant=False),
            _tx("t1", date(2026, 1, 13), -2000, 50000)]})
    _write(run / "history" / f"{product}.json", {
        "product_id": product, "kind": "card", "complete": True,
        "transactions": card_rows if card_rows is not None else [
            _tx("k2", date(2026, 1, 14), 5000, -20000, tx_type="CREDITCARD"),
            _tx("k1", date(2026, 1, 13), -25000, -25000,
                tx_type="CREDITCARD")]})
    _write(run / "statements" / ACCOUNT_ID / "listing.json", [
        {"id": "100000000000000000001", "minBookingDate": "2025-01-01",
         "maxBookingDate": "2025-12-31",
         "statementDate": "2026-01-01T00:00:00Z",
         "previousBalance": _m(1000)}])
    (run / "statements" / ACCOUNT_ID /
     "2026-01-01_100000000000000000001.pdf").write_bytes(pdf)
    _write(run / "invoices" / product / "listing.json", [
        {"billId": "2000001", "from": _ms(date(2025, 12, 1)),
         "to": _ms(date(2025, 12, 31)), "billingDate": _ms(date(2026, 1, 2))}])
    (run / "invoices" / product / "2026-01-02_2000001.pdf").write_bytes(
        b"%PDF-1.4 synthetic card statement")
    return run


def _db(tmp_path: Path) -> sqlite3.Connection:
    return sqlite3.connect(tmp_path / "erstebank_at.db")


def _load(tmp_path: Path, *extra: str) -> int:
    return load.main(["--bronze-dir", str(tmp_path), *extra])


# --- scalars and projections ----------------------------------------------

def test_money():
    assert load.money(_m(-1234)) == -12.34
    assert load.money({"value": 5, "precision": 0}) == 5
    assert load.money({"value": "5", "precision": 2}) is None
    assert load.money(None) is None
    assert load.money_minor(_m(-1234)) == -1234
    assert load.money_minor({"value": 5, "precision": 3}) is None


def test_card_row_negates_the_balance_into_the_amount_owed():
    row = load.card_row({"id": CARD_ID, "type": "CREDIT",
                         "creditCardShadowAccountId": SHADOW,
                         "number": "000000XXXXXX4321",
                         "balance": _m(-25000), "limit": _m(300000),
                         "cardHolder": "Synthetic Holder"})
    assert row["account_external_id"] == SHADOW
    assert row["balance"] == 250.0 and row["credit_limit"] == 3000.0
    assert row["mask"] == "…4321"
    assert "cardHolder" not in row["payload"]
    assert load.card_row({"id": "D1", "type": "BANK_CARD"}) is None


def test_checking_row_is_keyed_by_iban():
    row = load.checking_row({"id": ACCOUNT_ID,
                             "accountno": {"iban": "at kk bbbb 1234"},
                             "balance": _m(-500), "owners": []})
    assert row["account_external_id"] == "ATKKBBBB1234"
    assert row["balance"] == -5.0 and row["mask"] == "…1234"
    assert "owners" not in row["payload"]
    assert load.checking_row({"id": ACCOUNT_ID}) is None


def test_transaction_row_reads_vienna_dates_and_card_signs():
    row = load.transaction_row(SHADOW, load.KIND_CARD,
                               _tx("k1", date(2026, 1, 13), -2500, -2500))
    assert row["posted_at"] == _epoch(date(2026, 1, 13))
    assert row["amount"] == -25.0 and row["balance"] == 25.0
    assert row["reference"] == "ref one ref two"
    assert row["counterparty"] == "Synthetic Partner"
    pos = {**_tx("p1", date(2026, 1, 13), -100, 0), "partnerName": None,
           "merchantName": "Synthetic Merchant"}
    assert load.transaction_row(IBAN, load.KIND_CHECKING, pos)[
        "counterparty"] == "Synthetic Merchant"
    assert row["balance_relevant"] == 1
    assert load.transaction_row(IBAN, load.KIND_CHECKING,
                                {"id": "x", "amount": _m(1)}) is None


# --- daily closes ---------------------------------------------------------

def test_daily_closes_resolve_rows_out_of_order():
    d1, d2 = 1, 2
    # Day 2: +500 then -200 happened in that order (100 → 600 → 400), but the
    # rows come back with the earlier one first.
    rows = [(d1, 100, 100), (d2, 500, 600), (d2, -200, 400)]
    closes, breaks = load.daily_closes(rows)
    assert closes == {d1: 100, d2: 400} and breaks == 0
    closes, _ = load.daily_closes(list(reversed(rows)))
    assert closes[d2] == 400


def test_daily_closes_fill_an_ambiguous_day_from_its_neighbour():
    # Day 2 has two zero-amount rows at the same balance: no single answer,
    # so its close comes from day 1 plus its amounts.
    rows = [(1, 100, 100), (2, 0, 100), (2, 0, 100), (3, -50, 50)]
    closes, breaks = load.daily_closes(rows)
    assert closes == {1: 100, 2: 100, 3: 50} and breaks == 0


def test_daily_closes_count_a_gap():
    rows = [(1, 100, 100), (5, -10, 500)]       # days 2-4 missing
    closes, breaks = load.daily_closes(rows)
    assert closes == {1: 100, 5: 500} and breaks == 1


# --- end to end -----------------------------------------------------------

def test_load_writes_every_table(tmp_path):
    make_run(tmp_path, "20260116T080000Z")
    make_run(tmp_path, "20260115T080000Z", status="dry-run")
    assert _load(tmp_path) == 0
    db = _db(tmp_path)
    assert db.execute("SELECT COUNT(*) FROM dump_runs").fetchone()[0] == 1
    accounts = dict(db.execute(
        "SELECT account_external_id, account_kind FROM accounts"))
    assert accounts == {IBAN.upper(): "checking", SHADOW: "credit_card"}
    assert db.execute("SELECT balance FROM accounts WHERE account_kind="
                      "'credit_card'").fetchone()[0] == 250.0
    assert db.execute("SELECT COUNT(*) FROM run_accounts").fetchone()[0] == 2

    txns = db.execute("SELECT txn_id, account_external_id, balance_relevant "
                      "FROM transactions ORDER BY txn_id").fetchall()
    assert [t[0] for t in txns] == ["info", "k1", "k2", "t1", "t2", "t3"]
    assert dict((t[0], t[2]) for t in txns)["info"] == 0

    closes = db.execute(
        "SELECT account_external_id, balance_date, balance "
        "FROM daily_balances ORDER BY 1, 2").fetchall()
    assert closes == [
        (IBAN.upper(), _epoch(date(2026, 1, 13)), 500.0),
        (IBAN.upper(), _epoch(date(2026, 1, 14)), 1000.0),
        (IBAN.upper(), _epoch(date(2026, 1, 15)), 990.0),
        (SHADOW, _epoch(date(2026, 1, 13)), 250.0),
        (SHADOW, _epoch(date(2026, 1, 14)), 200.0)]

    stmts = db.execute("SELECT account_external_id, statement_kind, "
                       "period_end FROM statements ORDER BY 2").fetchall()
    assert stmts == [(SHADOW, "card_statement", _epoch(date(2025, 12, 31))),
                     (IBAN.upper(), "statement", _epoch(date(2025, 12, 31)))]
    docs = db.execute("SELECT account_external_id, doc_kind, doc_date "
                      "FROM documents ORDER BY 2").fetchall()
    assert docs == [(SHADOW, "card_statement", _epoch(date(2026, 1, 2))),
                    (IBAN.upper(), "statement", _epoch(date(2026, 1, 1)))]


def test_reload_is_a_no_op_and_force_rebuilds(tmp_path):
    make_run(tmp_path, "20260116T080000Z")
    _load(tmp_path)
    db = _db(tmp_path)
    before = db.execute("SELECT COUNT(*) FROM transactions").fetchone()[0]
    db.close()
    _load(tmp_path)
    db = _db(tmp_path)
    assert db.execute("SELECT COUNT(*) FROM transactions").fetchone()[0] \
        == before
    assert db.execute("SELECT COUNT(*) FROM dump_runs").fetchone()[0] == 1
    db.close()
    _load(tmp_path, "--force")
    db = _db(tmp_path)
    assert db.execute("SELECT COUNT(*) FROM transactions").fetchone()[0] \
        == before


def test_a_later_run_extends_the_ledger_and_dedups_accounts(tmp_path):
    make_run(tmp_path, "20260116T080000Z")
    make_run(tmp_path, "20260117T080000Z", account_rows=[
        _tx("t4", date(2026, 1, 16), -500, 98500),
        _tx("t3", date(2026, 1, 15), -1000, 99000)])
    _load(tmp_path)
    db = _db(tmp_path)
    ids = [r[0] for r in db.execute(
        "SELECT txn_id FROM transactions WHERE account_external_id=? "
        "ORDER BY posted_at", (IBAN.upper(),))]
    assert ids[-1] == "t4" and "t1" in ids
    # Same roster content in both runs: one accounts row each, two listings.
    assert db.execute("SELECT COUNT(*) FROM accounts").fetchone()[0] == 2
    assert db.execute("SELECT COUNT(*) FROM run_accounts").fetchone()[0] == 4
    assert db.execute(
        "SELECT balance FROM daily_balances WHERE balance_date=?",
        (_epoch(date(2026, 1, 16)),)).fetchone()[0] == 985.0


def test_the_same_pdf_in_two_runs_is_one_document(tmp_path):
    make_run(tmp_path, "20260116T080000Z")
    make_run(tmp_path, "20260117T080000Z")
    _load(tmp_path)
    db = _db(tmp_path)
    assert db.execute("SELECT COUNT(*) FROM documents").fetchone()[0] == 2
    assert db.execute("SELECT MIN(snapshot_at) = MAX(snapshot_at) "
                      "FROM documents").fetchone()[0] == 1
