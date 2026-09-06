"""Bronze -> silver for the credit-card surface.

Covers card_parsers (pure) and load.py's card pass (against a real
SQLite silver built from the migrations). Every value is invented.

The properties that carry weight:

* a RESERVED row is never a transaction, and its magnitude is not lost;
* the row key is the API's `_id`, so a re-download converges;
* a partial run never zeroes what it did not cover;
* the invoice identity is checked, and a period that fails it is kept
  and marked rather than dropped.
"""
from __future__ import annotations

import json
import sqlite3
from pathlib import Path

import pytest

import card_parsers
import load


# --------------------------------------------------------------------
# Synthetic bronze
# --------------------------------------------------------------------

ACCOUNT = "CARDACCT-0001"
OTHER_ACCOUNT = "CARDACCT-0002"
# What silver keys on. The API's account id above is a session handle;
# the account NUMBER is the identity that survives a re-login.
ACCOUNT_NO = "0000 0000 0001"
OTHER_ACCOUNT_NO = "0000 0000 0002"


def _money(amount, currency="CHF"):
    return {"amount": amount, "currency": currency}


def _booked(row_id, value_date, amount, **over):
    row = {
        "_id": row_id,
        "transactionStatus": "BOOKED",
        "bookedAccountId": ACCOUNT,
        "transactionNr": "7",           # repeats across rows by design
        "transactionDate": f"{value_date}T09:15:00Z",
        "valueDate": value_date,
        "postingAmount": _money(amount),
        "originalAmount": _money(amount),
        "details": "EXAMPLE SHOP  EXAMPLETOWN  CHE",
        "merchantName": "Grocery stores",
        "merchantGroupCode": "0009",
        "cardNr": "0000XXXXXXXX0000",
        "settledInInvoice": False,
    }
    row.update(over)
    return row


def _reserved(amount, **over):
    row = {
        "transactionStatus": "RESERVED",
        "bookedAccountId": ACCOUNT,
        "originalAmount": _money(amount),
        "details": "EXAMPLE CAFE  EXAMPLETOWN  CHE",
        "merchantName": "Restaurants",
        "merchantGroupCode": "0005",
    }
    row.update(over)
    return row


def _page(rows):
    return {"_embedded": {"transactions": rows}}


def _roster(*accounts):
    return {"creditCardAccounts": list(accounts)}


CARD = "CARD-0001"
OTHER_CARD = "CARD-0002"


def _card_node(card_id=CARD, account_id=ACCOUNT, balance="-250.00",
               with_reserved=None, currency="CHF"):
    """One physical card under an account, as the roster nests it.

    `liableAccountId` is the account it charges — the mapping the ledger
    needs, because a ledger row names the CARD.
    """
    node = {
        "_id": card_id,
        "liableAccountId": account_id,
        "cardNumber": "0000XXXXXXXX0000",
        "balance": _money(balance, currency),
    }
    if with_reserved is not None:
        node["balanceIncludingReserved"] = _money(with_reserved, currency)
    return node


def _account_node(account_id=ACCOUNT, cards=None, **over):
    node = {
        "id": account_id,
        "accountType": "CREDIT_CARD_ACCOUNT",
        "accountNumber": f"0000 0000 {account_id[-4:]}",
        "balance": _money("-250.00"),
        "available": _money("4750.00"),
        "limit": _money("5000.00"),
        "productName": "Example Card",
        "cardType": "EXCA",
        "cardStatus": "ACTIVE",
        "structureType": "COMPLEX_TLA",
        "_embedded": {"creditCards": cards if cards is not None
                      else [_card_node(account_id=account_id)]},
    }
    node.update(over)
    return node


def _invoice(invoice_id="INV-1", start="2026-01-01", end="2026-01-31",
             **over):
    node = {
        "id": invoice_id,
        "creditCardAccountId": ACCOUNT,
        "periodFrom": start,
        "periodTo": end,
        "invoicingDate": end,
        "debitingDate": "2026-02-15",
        "dueOn": "2026-02-20",
        "dueAmount": _money("-250.00"),
        "minimalDueAmount": _money("-25.00"),
        "statementType": "INVOICE",
        "paymentMethod": "LSV",
        # The API wraps this in an envelope. The fixture carries the real
        # shape: a bare string here is what let a dict reach a TEXT bind.
        "invoiceStatus": {"statusCode": "OPEN"},
    }
    node.update(over)
    return node


def _detail(invoice_id="INV-1", balance_forward="-100.00",
            total_debit="-200.00", total_credit="50.00",
            due="-250.00", **over):
    node = _invoice(invoice_id, **over)
    node.update({
        "balanceForward": _money(balance_forward),
        "totalDebit": _money(total_debit),
        "totalCredit": _money(total_credit),
        "dueAmount": _money(due),
    })
    return node


# --------------------------------------------------------------------
# card_parsers — transactions
# --------------------------------------------------------------------

def test_booked_rows_are_promoted_with_a_content_id_as_the_key():
    rows, reserved = card_parsers.parse_transactions(
        [_page([_booked("ROW-1", "2026-01-05", "-42.50")])])
    assert reserved == {}
    assert len(rows) == 1
    row = rows[0]
    # NOT the API's `_id`: that is re-minted at every login.
    assert row["transaction_external_id"].startswith("card:")
    assert "ROW-1" not in row["transaction_external_id"]
    assert "_handle" not in row
    assert row["amount"] == -42.50
    assert row["merchant"] == "EXAMPLE SHOP  EXAMPLETOWN  CHE"
    # The API's `merchantName` is the CATEGORY; the promotion corrects it.
    assert row["merchant_category"] == "Grocery stores"


def test_reserved_rows_never_become_transactions():
    rows, reserved = card_parsers.parse_transactions(
        [_page([_booked("ROW-1", "2026-01-05", "-10.00"),
                _reserved("-7.00"), _reserved("-3.00")])])
    assert len(rows) == 1
    # A count, not a sum: their only figure is in the merchant's currency.
    assert reserved[ACCOUNT] == 2


def test_a_re_login_does_not_duplicate_the_ledger(tmp_path):
    """The regression. UBS re-mints every id in the card surface at
    login — account, card, ledger row, invoice — so two dumps a day
    apart shared not one ledger id, and the second load
    inserted a whole second copy of the history instead of re-observing
    it. Gold then counted every card purchase twice."""
    conn = _silver(tmp_path)
    purchases = [_booked("SESSION-A-1", "2026-01-05", "-42.50"),
                 _booked("SESSION-A-2", "2026-01-06", "-17.00")]
    load._load_cards(conn, 1000, _write_bronze(
        tmp_path / "20260101T000000Z", roster=_roster(_account_node()),
        pages=[_page(purchases)],
        invoices={"invoices": [_invoice()]}))

    # The same two purchases, every handle re-minted, nothing else moved.
    relogged = [dict(p, _id=p["_id"].replace("SESSION-A", "SESSION-B"))
                for p in purchases]
    load._load_cards(conn, 2000, _write_bronze(
        tmp_path / "20260102T000000Z", roster=_roster(_account_node()),
        pages=[_page(relogged)],
        invoices={"invoices": [_invoice("INV-RELOGGED")]}, short="bbbb"))

    assert conn.execute(
        "SELECT COUNT(*) FROM card_transactions").fetchone()[0] == 2
    assert conn.execute(
        "SELECT COUNT(*) FROM card_invoices").fetchone()[0] == 1
    assert conn.execute(
        "SELECT COUNT(DISTINCT account_external_id) FROM card_accounts"
    ).fetchone()[0] == 1


def test_two_identical_purchases_on_one_day_stay_two_rows():
    # The other half of a content id: same card, same day, same amount,
    # same merchant is two coffees, not one row observed twice.
    rows, _ = card_parsers.parse_transactions([_page([
        _booked("ROW-1", "2026-01-05", "-4.50"),
        _booked("ROW-2", "2026-01-05", "-4.50"),
    ])])
    assert len({r["transaction_external_id"] for r in rows}) == 2


def test_a_repeated_row_is_stored_once():
    # Overlapping fetch windows return the same row twice.
    row = _booked("ROW-1", "2026-01-05", "-42.50")
    rows, _ = card_parsers.parse_transactions([_page([row]), _page([row])])
    assert len(rows) == 1


def test_rows_across_pages_are_all_kept():
    rows, _ = card_parsers.parse_transactions([
        _page([_booked("ROW-1", "2026-01-05", "-1.00")]),
        _page([_booked("ROW-2", "2026-01-06", "-2.00")]),
    ])
    assert len({r["transaction_external_id"] for r in rows}) == 2


def test_a_row_that_cannot_be_keyed_or_dated_is_refused():
    no_id = _booked("X", "2026-01-05", "-1.00")
    del no_id["_id"]
    no_date = _booked("ROW-2", "2026-01-05", "-1.00")
    del no_date["valueDate"]
    rows, _ = card_parsers.parse_transactions([_page([no_id, no_date])])
    assert rows == []


def test_foreign_currency_rows_keep_both_amounts_and_the_rate():
    rows, _ = card_parsers.parse_transactions([_page([
        _booked("ROW-1", "2026-01-05", "-10.80",
                postingAmount=_money("-10.80", "CHF"),
                originalAmount=_money("-9.96", "EUR"),
                exchangeRate="1.0843")])])
    row = rows[0]
    assert (row["amount"], row["currency_iso"]) == (-10.80, "CHF")
    assert (row["original_amount"], row["original_currency_iso"]) == (-9.96, "EUR")
    assert row["exchange_rate"] == pytest.approx(1.0843)


def test_amounts_keep_the_source_sign():
    # UBS signs a card negative-when-owed, which is already gold's
    # convention for a liability; nothing is flipped in silver.
    rows, _ = card_parsers.parse_transactions([_page([
        _booked("SPEND", "2026-01-05", "-42.50"),
        _booked("REFUND", "2026-01-06", "12.00"),
    ])])
    by_day = {r["value_date"]: r["amount"] for r in rows}
    assert sorted(by_day.values()) == [-42.50, 12.00]


# --------------------------------------------------------------------
# card_parsers — accounts and invoices
# --------------------------------------------------------------------

def test_nested_accounts_are_all_promoted():
    roster = _roster(_account_node(
        ACCOUNT, relatedCardAccounts=[_account_node(OTHER_ACCOUNT)]))
    rows = card_parsers.parse_accounts(roster)
    assert {r["account_external_id"] for r in rows} == {ACCOUNT_NO,
                                                       OTHER_ACCOUNT_NO}


def test_reserved_totals_land_on_the_account():
    roster = _roster(_account_node(ACCOUNT, cards=[
        _card_node(CARD, balance="-100.00", with_reserved="-112.50")]))
    rows = card_parsers.parse_accounts(roster, {ACCOUNT_NO: 3})
    assert rows[0]["reserved_amount"] == pytest.approx(-12.5)
    assert rows[0]["reserved_count"] == 3


def test_invoice_detail_supplies_the_opening_balance_and_reconciles():
    rows = card_parsers.parse_invoices(
        {"invoices": [_invoice()]}, [_detail()])
    row = rows[0]
    assert row["balance_forward"] == -100.0
    assert row["reconciles"] == 1


def test_a_period_whose_figures_disagree_is_kept_and_marked():
    rows = card_parsers.parse_invoices(
        {"invoices": [_invoice()]},
        [_detail(total_credit="999.00")])   # breaks the identity
    assert len(rows) == 1
    assert rows[0]["reconciles"] == 0


def test_reconciles_is_none_when_there_was_nothing_to_check():
    rows = card_parsers.parse_invoices({"invoices": [_invoice()]}, [])
    assert rows[0]["reconciles"] is None
    assert rows[0]["balance_forward"] is None


def test_the_open_period_has_no_debiting_date():
    rows = card_parsers.parse_invoices(
        {"invoices": [_invoice(statementType="STATEMENT", debitingDate=None,
                               dueOn=None)]}, [])
    assert rows[0]["debiting_date"] is None
    assert rows[0]["statement_type"] == "STATEMENT"


def test_reconcile_tolerance_admits_a_rounding_cent_but_not_a_real_gap():
    assert card_parsers.reconciles(-100.0, -200.0, 50.0, -250.004) == 1
    assert card_parsers.reconciles(-100.0, -200.0, 50.0, -250.50) == 0


# --------------------------------------------------------------------
# load.py — the card pass against a real silver
# --------------------------------------------------------------------

def _silver(tmp_path) -> sqlite3.Connection:
    conn = sqlite3.connect(":memory:")
    conn.execute("PRAGMA foreign_keys = ON;")
    for path in sorted((Path(load.__file__).parent / "migrations").glob("*.sql")):
        conn.executescript(path.read_text())
    return conn


def _write_bronze(dump: Path, *, pages=None, roster=None, invoices=None,
                  details=None, statements=None, short="aaaa"):
    cards = dump / "cards"
    cards.mkdir(parents=True, exist_ok=True)
    if roster is not None:
        (cards / "accounts.json").write_text(json.dumps(roster))
    if pages is not None:
        (cards / f"transactions_{short}_20260101_20260331.json").write_text(
            json.dumps({"pages": pages}))
    if invoices is not None:
        (cards / f"invoices_{short}.json").write_text(json.dumps(invoices))
    if details is not None:
        (cards / f"invoice-details_{short}.json").write_text(
            json.dumps({"invoices": details}))
    if statements:
        sdir = cards / "statements"
        sdir.mkdir(exist_ok=True)
        mapping = {}
        for name, invoice_id in statements.items():
            (sdir / name).write_bytes(b"%PDF-1.4 synthetic")
            mapping[name] = invoice_id
        (cards / f"statements_{short}.json").write_text(
            json.dumps({"files": mapping}))
    return dump


def test_a_dump_without_cards_loads_as_zeros(tmp_path):
    conn = _silver(tmp_path)
    dump = tmp_path / "20260101T000000Z"
    dump.mkdir()
    assert load._load_cards(conn, 1, dump) == (0, 0, 0, 0)


def test_a_whole_card_dump_lands_in_silver(tmp_path):
    conn = _silver(tmp_path)
    dump = _write_bronze(
        tmp_path / "20260101T000000Z",
        roster=_roster(_account_node()),
        pages=[_page([_booked("ROW-1", "2026-01-05", "-42.50"),
                      _reserved("-9.00")])],
        invoices={"invoices": [_invoice()]},
        details=[_detail()],
        statements={"a" * 64 + ".pdf": "INV-1"},
    )
    acc, txn, inv, stmt = load._load_cards(conn, 1700000000, dump)
    assert (acc, txn, inv, stmt) == (1, 1, 1, 1)

    row = conn.execute(
        "SELECT amount, merchant_category, currency_iso FROM card_transactions"
    ).fetchone()
    assert row == (-42.5, "Grocery stores", "CHF")
    assert conn.execute(
        "SELECT reserved_count FROM card_accounts").fetchone()[0] == 1
    assert conn.execute(
        "SELECT reconciles, balance_forward FROM card_invoices"
    ).fetchone() == (1, -100.0)
    # The PDF is filed under the invoice's STORED id, not the handle the
    # capture recorded it against.
    assert conn.execute(
        "SELECT s.invoice_external_id = i.invoice_external_id "
        "  FROM card_statements s, card_invoices i").fetchone()[0] == 1


def test_reloading_the_same_dump_changes_nothing(tmp_path):
    conn = _silver(tmp_path)
    dump = _write_bronze(
        tmp_path / "20260101T000000Z",
        roster=_roster(_account_node()),
        pages=[_page([_booked("ROW-1", "2026-01-05", "-42.50")])],
        invoices={"invoices": [_invoice()]}, details=[_detail()],
    )
    load._load_cards(conn, 1700000000, dump)
    before = conn.execute("SELECT COUNT(*) FROM card_transactions").fetchone()[0]
    load._load_cards(conn, 1700000000, dump)
    after = conn.execute("SELECT COUNT(*) FROM card_transactions").fetchone()[0]
    assert before == after == 1


def test_snapshot_at_records_first_sight_not_the_latest_load(tmp_path):
    conn = _silver(tmp_path)
    dump = _write_bronze(
        tmp_path / "20260101T000000Z",
        roster=_roster(_account_node()),
        pages=[_page([_booked("ROW-1", "2026-01-05", "-42.50")])])
    load._load_cards(conn, 1000, dump)
    load._load_cards(conn, 2000, dump)
    assert conn.execute(
        "SELECT snapshot_at FROM card_transactions").fetchone()[0] == 1000


def test_a_partial_run_never_zeroes_what_it_did_not_cover(tmp_path):
    """The load-bearing guarantee: a later dump covering one account must
    leave another account's rows, balances and periods untouched."""
    conn = _silver(tmp_path)
    full = _write_bronze(
        tmp_path / "20260101T000000Z",
        roster=_roster(_account_node(ACCOUNT),
                       _account_node(OTHER_ACCOUNT)),
        pages=[_page([
            _booked("A-1", "2026-01-05", "-10.00"),
            _booked("B-1", "2026-01-06", "-20.00",
                    bookedAccountId=OTHER_ACCOUNT)])],
        invoices={"invoices": [_invoice("INV-A"),
                               _invoice("INV-B", start="2026-02-01",
                                        end="2026-02-28",
                                        creditCardAccountId=OTHER_ACCOUNT)]},
        details=[_detail("INV-A")])
    load._load_cards(conn, 1000, full)
    assert conn.execute("SELECT COUNT(*) FROM card_transactions").fetchone()[0] == 2

    # A later run that only reached one account.
    partial = _write_bronze(
        tmp_path / "20260102T000000Z",
        roster=_roster(_account_node(ACCOUNT)),
        pages=[_page([_booked("A-2", "2026-01-07", "-30.00")])],
        invoices={"invoices": [_invoice("INV-A")]},
        details=[_detail("INV-A")], short="bbbb")
    load._load_cards(conn, 2000, partial)

    ids = {r[0] for r in conn.execute(
        "SELECT transaction_external_id FROM card_transactions")}
    assert len(ids) == 3, "the uncovered account lost rows"
    # And the uncovered account's period survives.
    assert conn.execute(
        "SELECT COUNT(*) FROM card_invoices WHERE account_external_id = ?",
        (OTHER_ACCOUNT_NO,)).fetchone()[0] == 1
    # Its earlier snapshot row is still there beside the new one.
    assert conn.execute(
        "SELECT COUNT(*) FROM card_accounts WHERE account_external_id = ?",
        (OTHER_ACCOUNT_NO,)).fetchone()[0] == 1


def test_coverage_needs_the_ledger_to_span_the_whole_period(tmp_path):
    conn = _silver(tmp_path)
    # A ledger that starts inside the period does not cover it.
    dump = _write_bronze(
        tmp_path / "20260101T000000Z",
        roster=_roster(_account_node()),
        pages=[_page([_booked("ROW-1", "2026-01-15", "-1.00")])],
        invoices={"invoices": [_invoice(start="2026-01-01", end="2026-01-31")]})
    load._load_cards(conn, 1000, dump)
    assert conn.execute(
        "SELECT transactions_covered FROM card_invoices").fetchone()[0] == 0

    # Widen it past both edges and the same period becomes covered.
    dump2 = _write_bronze(
        tmp_path / "20260102T000000Z",
        roster=_roster(_account_node()),
        pages=[_page([_booked("ROW-0", "2025-12-20", "-1.00"),
                      _booked("ROW-2", "2026-02-05", "-1.00")])],
        invoices={"invoices": [_invoice(start="2026-01-01", end="2026-01-31")]},
        short="bbbb")
    load._load_cards(conn, 2000, dump2)
    assert conn.execute(
        "SELECT transactions_covered FROM card_invoices").fetchone()[0] == 1


def test_a_statement_without_attribution_is_not_indexed(tmp_path):
    conn = _silver(tmp_path)
    dump = _write_bronze(
        tmp_path / "20260101T000000Z",
        roster=_roster(_account_node()),
        invoices={"invoices": [_invoice()]}, details=[_detail()],
        statements={"b" * 64 + ".pdf": "INV-UNKNOWN"})
    _, _, _, stmt = load._load_cards(conn, 1000, dump)
    assert stmt == 0


def test_a_corrupt_artefact_costs_its_facet_not_the_dump(tmp_path):
    conn = _silver(tmp_path)
    dump = _write_bronze(
        tmp_path / "20260101T000000Z",
        roster=_roster(_account_node()),
        pages=[_page([_booked("ROW-1", "2026-01-05", "-1.00")])])
    (dump / "cards" / "invoices_aaaa.json").write_text("{ not json")
    acc, txn, inv, _ = load._load_cards(conn, 1000, dump)
    assert (acc, txn, inv) == (1, 1, 0)


# --------------------------------------------------------------------
# The real entry point: migrations applied by the shared runner
# --------------------------------------------------------------------

def test_load_main_applies_the_card_migration_and_ingests(tmp_path):
    """End to end through load.main(), so the migration runs the way it
    will in production rather than through a hand-rolled executescript."""
    bronze = tmp_path / "bronze"
    dump = _write_bronze(
        bronze / "20260101T000000Z",
        roster=_roster(_account_node()),
        pages=[_page([_booked("ROW-1", "2026-01-05", "-42.50")])],
        invoices={"invoices": [_invoice()]}, details=[_detail()])
    (dump / "run.json").write_text(json.dumps({
        "status": "complete", "dry_run": False,
        "window": {"since": "2026-01-01", "until": "2026-03-31"}}))

    db = tmp_path / "ubs-web.db"
    assert load.main(["--bronze-dir", str(bronze), "--silver-db", str(db)]) == 0

    conn = sqlite3.connect(str(db))
    assert conn.execute(
        "SELECT MAX(silver_schema_version) FROM schema_meta").fetchone()[0] >= 7
    assert conn.execute(
        "SELECT COUNT(*) FROM card_transactions").fetchone()[0] == 1
    # And a second load of the same tree is a clean no-op.
    assert load.main(["--bronze-dir", str(bronze), "--silver-db", str(db)]) == 0
    conn2 = sqlite3.connect(str(db))
    assert conn2.execute(
        "SELECT COUNT(*) FROM card_transactions").fetchone()[0] == 1


# --------------------------------------------------------------------
# Payload enumeration — the reading the capture and the load share
# --------------------------------------------------------------------

def test_every_nested_card_account_is_enumerated():
    # The roster nests: accounts carry related accounts. An id missed
    # here is an account silently not fetched and not loaded.
    roster = {
        "creditCardAccounts": [
            {"id": "ACC-1", "accountType": "CREDIT_CARD_ACCOUNT",
             "relatedCardAccounts": [
                 {"id": "ACC-2", "accountType": "CREDIT_CARD_ACCOUNT"}]},
            {"id": "ACC-3", "accountType": "CREDIT_CARD_ACCOUNT"},
        ],
        "_embedded": {"extra": {"id": "ACC-4",
                                "accountType": "CREDIT_CARD_ACCOUNT"}},
    }
    assert card_parsers.account_ids(roster) == ["ACC-1", "ACC-2", "ACC-3", "ACC-4"]


def test_non_card_accounts_are_not_enumerated():
    roster = {"accounts": [{"id": "CASH-1", "accountType": "CURRENT_ACCOUNT"}]}
    assert card_parsers.account_ids(roster) == []


def test_account_ids_are_deduplicated_but_keep_order():
    roster = {"a": [{"id": "ACC-1", "accountType": "CREDIT_CARD_ACCOUNT"}],
              "b": [{"id": "ACC-1", "accountType": "CREDIT_CARD_ACCOUNT"},
                    {"id": "ACC-2", "accountType": "CREDIT_CARD_ACCOUNT"}]}
    assert card_parsers.account_ids(roster) == ["ACC-1", "ACC-2"]


def test_invoice_ids_come_from_rows_carrying_a_period():
    payload = {"invoices": [
        {"id": "INV-1", "periodFrom": "2020-01-01", "periodTo": "2020-01-31"},
        {"id": "NOT-AN-INVOICE"},
        {"id": "INV-2", "periodFrom": "2020-02-01", "periodTo": "2020-02-29"},
    ]}
    assert card_parsers.invoice_ids(payload) == ["INV-1", "INV-2"]


def test_enumeration_and_parsing_agree_on_what_an_account_is():
    # One predicate, so the capture cannot fetch a set the load then
    # reads differently.
    roster = _roster(_account_node(ACCOUNT), _account_node(OTHER_ACCOUNT))
    # The capture drives the API with handles and the load stores the
    # stable id, so the two agree THROUGH the map, account for account.
    stable = card_parsers.stable_account_ids(roster)
    parsed = [r["account_external_id"] for r in card_parsers.parse_accounts(roster)]
    assert parsed == [stable[a] for a in card_parsers.account_ids(roster)]


# --------------------------------------------------------------------
# The shapes the first live run proved wrong
#
# Each of these pins a defect that shipped because a fixture was
# invented from an assumed payload shape rather than an observed one.
# --------------------------------------------------------------------

def test_a_ledger_row_is_keyed_to_the_account_not_the_card():
    # On a multi-card account the ledger books against the CARD. Keyed
    # on that, a row joins to no card_accounts row, no invoice, and
    # falls outside gold's spending scope.
    roster = _roster(_account_node(
        ACCOUNT, cards=[_card_node(CARD), _card_node(OTHER_CARD)]))
    mapping = card_parsers.card_account_map(roster)
    # Both cards, and the account's own handle, resolve to the account.
    assert mapping == {CARD: ACCOUNT_NO, OTHER_CARD: ACCOUNT_NO,
                       ACCOUNT: ACCOUNT_NO}

    rows, _ = card_parsers.parse_transactions(
        [_page([_booked("R-1", "2026-01-05", "-10.00", bookedAccountId=CARD),
                _booked("R-2", "2026-01-06", "-20.00", bookedAccountId=OTHER_CARD)])],
        mapping)
    assert {r["account_external_id"] for r in rows} == {ACCOUNT_NO}


def test_an_unmapped_id_passes_through_unchanged():
    # The single-card case, where the card id and the account id coincide
    # and there is nothing to resolve.
    rows, _ = card_parsers.parse_transactions(
        [_page([_booked("R-1", "2026-01-05", "-10.00")])], {})
    assert rows[0]["account_external_id"] == ACCOUNT


def test_reserved_comes_from_the_roster_not_the_ledger():
    # A reserved ledger row carries only the merchant-currency figure, so
    # summing them mixes units. The roster states the amount per card, in
    # the card's own currency.
    roster = _roster(_account_node(ACCOUNT, cards=[
        _card_node(CARD, balance="-100.00", with_reserved="-130.00"),
        _card_node(OTHER_CARD, balance="-50.00", with_reserved="-55.00"),
    ]))
    assert card_parsers.roster_reserved(roster) == pytest.approx(
        {ACCOUNT_NO: -35.0})


def test_reserved_rows_are_counted_but_never_summed():
    _, counts = card_parsers.parse_transactions(
        [_page([_reserved("-7.00"), _reserved("-3.00")])], {})
    # No map given, so the booked-against id passes through as it came.
    assert counts == {ACCOUNT: 2}


def test_the_account_balance_already_includes_reserved():
    # The account's roster balance equals its card's
    # balanceIncludingReserved, so it must be stored as reported.
    roster = _roster(_account_node(ACCOUNT, balance=_money("-130.00"), cards=[
        _card_node(CARD, balance="-100.00", with_reserved="-130.00")]))
    row = card_parsers.parse_accounts(roster, {ACCOUNT_NO: 1})[0]
    assert row["balance"] == -130.0
    assert row["reserved_amount"] == pytest.approx(-30.0)


def test_invoice_status_envelope_is_unwrapped():
    rows = card_parsers.parse_invoices({"invoices": [_invoice()]}, [])
    # A dict here would raise at the TEXT bind and roll back the dump.
    assert rows[0]["invoice_status"] == "OPEN"
    assert isinstance(rows[0]["invoice_status"], str)


def test_a_bare_status_string_still_works():
    rows = card_parsers.parse_invoices(
        {"invoices": [_invoice(invoiceStatus="PAID")]}, [])
    assert rows[0]["invoice_status"] == "PAID"


def test_the_whole_dump_loads_with_the_real_shapes(tmp_path):
    # The end-to-end guard: every shape here is the one the live API
    # actually sends, so a bind error cannot hide behind a fixture.
    conn = _silver(tmp_path)
    dump = _write_bronze(
        tmp_path / "20260101T000000Z",
        roster=_roster(_account_node(ACCOUNT, cards=[
            _card_node(CARD, balance="-100.00", with_reserved="-140.00")])),
        pages=[_page([_booked("R-1", "2026-01-05", "-42.50",
                              bookedAccountId=CARD),
                      _reserved("-40.00", bookedAccountId=CARD)])],
        invoices={"invoices": [_invoice()]}, details=[_detail()])
    acc, txn, inv, _ = load._load_cards(conn, 1700000000, dump)
    assert (acc, txn, inv) == (1, 1, 1)
    # The ledger row resolved onto the account, so it joins.
    assert conn.execute(
        "SELECT COUNT(*) FROM card_transactions t JOIN card_accounts a"
        "  ON a.account_external_id = t.account_external_id").fetchone()[0] == 1
    assert conn.execute(
        "SELECT reserved_amount, reserved_count FROM card_accounts"
    ).fetchone() == (pytest.approx(-40.0), 1)
