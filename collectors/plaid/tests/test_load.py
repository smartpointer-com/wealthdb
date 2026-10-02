"""Tests for the load verb: bronze runs into one silver database per Item.
Every fixture is synthetic, built here in the shape Plaid answers."""
from __future__ import annotations

import json
import sqlite3
from pathlib import Path

import pytest

import load

UTC_DAY = 86_400
JAN_2 = 1_767_312_000            # 2026-01-02T00:00:00Z


# ---- building bronze ----------------------------------------------------------------

def account(n, type_="depository", subtype="checking", current=100.5,
            **balances):
    return {"account_id": f"acc-{n}", "name": f"Synthetic {subtype}",
            "official_name": None, "mask": f"{n:04d}", "type": type_,
            "subtype": subtype,
            "balances": {"current": current, "available": None,
                         "limit": None, "iso_currency_code": "USD",
                         "unofficial_currency_code": None, **balances}}


def security(n, type_="equity", ticker="SYN"):
    return {"security_id": f"sec-{n}", "name": f"Synthetic {type_} {n}",
            "ticker_symbol": ticker, "type": type_, "subtype": None,
            "iso_currency_code": "USD", "unofficial_currency_code": None,
            "cusip": None, "isin": None, "figi": None, "cfi_code": None,
            "market_identifier_code": None, "is_cash_equivalent": False,
            "institution_security_id": None, "proxy_security_id": None}


def holding(account_n, security_n, quantity=2, value=21.5):
    return {"account_id": f"acc-{account_n}",
            "security_id": f"sec-{security_n}", "quantity": quantity,
            "institution_price": 10.75, "institution_price_as_of":
            "2026-01-30", "institution_value": value, "cost_basis": 20,
            "iso_currency_code": "USD", "unofficial_currency_code": None,
            "vested_quantity": None, "vested_value": None, "tax_lots": []}


def tx(n, account_n=1, amount=12.34, date="2026-01-20", pending=False,
       detailed="FOOD_AND_DRINK_GROCERIES"):
    return {"transaction_id": f"tx-{n}", "account_id": f"acc-{account_n}",
            "amount": amount, "date": date, "authorized_date": date,
            "iso_currency_code": "USD", "unofficial_currency_code": None,
            "name": "Synthetic Grocer", "merchant_name": "Synthetic Grocer",
            "original_description": "SYNTHETIC GROCER 0001",
            "pending": pending, "pending_transaction_id": None,
            "personal_finance_category": {
                "primary": detailed.split("_")[0], "detailed": detailed,
                "confidence_level": "HIGH", "version": "v2"},
            "payment_channel": "in store", "transaction_code": None,
            "check_number": None, "merchant_category_code": "5411"}


def itx(n, account_n=2, amount=105.0, type_="buy", subtype="buy",
        security_n=1, date="2026-01-20"):
    return {"investment_transaction_id": f"itx-{n}",
            "account_id": f"acc-{account_n}",
            "security_id": f"sec-{security_n}", "date": date,
            "transaction_datetime": None, "name": f"{type_} synthetic",
            "type": type_, "subtype": subtype, "amount": amount,
            "quantity": 10, "price": 10.5, "fees": 0,
            "iso_currency_code": "USD", "unofficial_currency_code": None,
            "cancel_transaction_id": None}


def item_doc(transactions_at="2026-01-31T05:00:00.123456789Z",
             investments_at="2026-01-31T06:00:00Z"):
    return {"item": {"item_id": "item-synthetic-1", "error": None,
                     "institution_id": "ins_000",
                     "institution_name": "Synthetic Bank",
                     "products": ["investments", "liabilities",
                                  "transactions"],
                     "consent_expiration_time": None},
            "status": {"transactions": {
                           "last_successful_update": transactions_at},
                       "investments": {
                           "last_successful_update": investments_at}}}


def write_run(tree: Path, slug: str, *, status="complete", item=None,
              accounts=None, holdings=None, securities=None,
              transactions=None, investment_transactions=None,
              liabilities=None, since="2026-01-01", until="2026-01-31",
              history="HISTORICAL_UPDATE_COMPLETE", item_id="item-synthetic-1",
              environment="sandbox", products=None) -> Path:
    """One bronze run, the way download writes it. A product left None is
    recorded as not linked; `products` overrides any entry. A bank ledger
    whose `history` is not complete is recorded as partial."""
    run = tree / slug
    run.mkdir(parents=True)
    entries = {}

    def put(name, doc):
        (run / name).write_text(json.dumps(doc))
        return name

    put("item.json", item or item_doc())
    accts = accounts if accounts is not None else [account(1)]
    entries["accounts"] = {"status": "fetched", "rows": len(accts),
                           "files": [put("accounts.json",
                                         {"accounts": accts})]}
    if holdings is not None:
        entries["holdings"] = {"status": "fetched", "rows": len(holdings),
                               "files": [put("holdings.json", {
                                   "accounts": accts, "holdings": holdings,
                                   "securities": securities or []})]}
    for product, rows, key in (
            ("transactions", transactions, "total_transactions"),
            ("investment_transactions", investment_transactions,
             "total_investment_transactions")):
        if rows is not None:
            entries[product] = {
                "status": "fetched", "since": since, "until": until,
                "rows": len(rows),
                "files": [put(f"{product}-0001.json", {
                    "accounts": accts, product: rows, key: len(rows),
                    "securities": securities or []})]}
            if product == "transactions":
                entries[product]["history"] = history
                if history != "HISTORICAL_UPDATE_COMPLETE":
                    entries[product]["status"] = "partial"
    if liabilities is not None:
        entries["liabilities"] = {
            "status": "fetched",
            "rows": sum(len(v or []) for v in liabilities.values()),
            "files": [put("liabilities.json", {"liabilities": liabilities})]}
    for product in ("holdings", "investment_transactions", "transactions",
                    "liabilities"):
        entries.setdefault(product, {"status": "not_linked"})
    entries.update(products or {})
    put("run.json", {"status": status, "item": tree.name,
                     "environment": environment, "item_id": item_id,
                     "since": since, "until": until, "products": entries})
    return run


@pytest.fixture(autouse=True)
def info_logs(caplog):
    caplog.set_level("INFO")


@pytest.fixture
def tree(tmp_path):
    return tmp_path / "plaid" / "bank"


def load_tree(tree, *argv):
    return load.main(["--bronze-dir", str(tree.parent), *argv])


def rows(tree, sql, *params):
    conn = sqlite3.connect(tree / f"{tree.name}.db")
    try:
        return conn.execute(sql, params).fetchall()
    finally:
        conn.close()


# ---- values -----------------------------------------------------------------------

@pytest.mark.parametrize("value,text", [
    (23631.9805, "23631.9805"), (0.1, "0.1"), (1e-05, "0.00001"),
    (100, "100"), (-0.0, "0"), (0, "0"), (None, None), (True, None),
    (1.5e+20, "150000000000000000000")])
def test_a_number_is_stored_as_plaid_stated_it(value, text):
    assert load.decimal_text(value) == text


@pytest.mark.parametrize("value,text", [
    (12.34, "-12.34"), (-500, "500"), (0, "0"), (None, None)])
def test_a_ledger_amount_is_negated_exactly(value, text):
    assert load.negated_text(value) == text


def test_a_date_is_midnight_utc():
    assert load.day("2026-01-02") == JAN_2
    assert load.day(None) is None


# ---- a run ------------------------------------------------------------------------

def test_a_run_loads_every_fetched_product(tree):
    write_run(tree, "20260131T070000Z",
              accounts=[account(1), account(2, "investment", "ira", 321.5),
                        account(3, "credit", "credit card", 410)],
              holdings=[holding(2, 1)], securities=[security(1)],
              transactions=[tx(1, account_n=1), tx(2, account_n=3)],
              investment_transactions=[itx(1)],
              liabilities={"credit": [{"account_id": "acc-3", "aprs": [
                  {"apr_type": "purchase_apr", "apr_percentage": 12.5}],
                  "last_statement_balance": 400.1,
                  "minimum_payment_amount": 20,
                  "next_payment_due_date": "2026-02-20"}],
                  "mortgage": None, "student": []})

    assert load_tree(tree) == 0

    assert rows(tree, "SELECT count(*) FROM dump_runs") == [(1,)]
    assert rows(tree, "SELECT account_id, type, subtype, balance_current "
                      "FROM accounts ORDER BY account_id") == [
        ("acc-1", "depository", "checking", "100.5"),
        ("acc-2", "investment", "ira", "321.5"),
        ("acc-3", "credit", "credit card", "410")]
    assert rows(tree, "SELECT account_id, security_id, quantity, "
                      "institution_value FROM holdings") == [
        ("acc-2", "sec-1", "2", "21.5")]
    assert rows(tree, "SELECT security_id, ticker_symbol, type FROM "
                      "securities") == [("sec-1", "SYN", "equity")]
    assert rows(tree, "SELECT transaction_id, amount, category_detailed, "
                      "pending FROM transactions ORDER BY 1") == [
        ("tx-1", "-12.34", "FOOD_AND_DRINK_GROCERIES", 0),
        ("tx-2", "-12.34", "FOOD_AND_DRINK_GROCERIES", 0)]
    assert rows(tree, "SELECT investment_transaction_id, amount, quantity, "
                      "type, subtype FROM investment_transactions") == [
        ("itx-1", "-105.0", "10", "buy", "buy")]
    assert rows(tree, "SELECT account_id, kind, interest_rate, "
                      "last_statement_balance, next_payment_amount, "
                      "next_payment_due_date FROM liabilities") == [
        ("acc-3", "credit", "12.5", "400.1", "20", JAN_2 + 49 * UTC_DAY)]
    assert rows(tree, "SELECT product, status FROM run_products ORDER BY 1") \
        == [("accounts", "fetched"), ("holdings", "fetched"),
            ("investment_transactions", "fetched"),
            ("liabilities", "fetched"), ("transactions", "fetched")]


def test_the_payload_keeps_plaids_row_and_sign(tree):
    write_run(tree, "20260131T070000Z", transactions=[tx(1, amount=12.34)])
    load_tree(tree)
    ((text,),) = rows(tree, "SELECT payload FROM transactions")
    assert json.loads(text)["amount"] == 12.34


def test_the_silver_is_owner_only(tree):
    write_run(tree, "20260131T070000Z")
    load_tree(tree)
    assert (tree / "bank.db").stat().st_mode & 0o777 == 0o600


# ---- one instant per run ------------------------------------------------------------

RUN_AT = JAN_2 + 29 * UTC_DAY + 7 * 3600          # 20260131T070000Z


def test_a_run_stamps_every_snapshot_with_its_start(tree):
    write_run(tree, "20260131T070000Z",
              accounts=[account(1), account(2, "investment", "ira")],
              holdings=[holding(2, 1)], securities=[security(1)],
              liabilities={"credit": [{"account_id": "acc-1"}]})
    load_tree(tree)
    assert rows(tree, "SELECT DISTINCT snapshot_at FROM accounts") == [
        (RUN_AT,)]
    assert rows(tree, "SELECT snapshot_at FROM holdings") == [(RUN_AT,)]
    assert rows(tree, "SELECT snapshot_at FROM liabilities") == [(RUN_AT,)]
    assert rows(tree, "SELECT snapshot_at FROM dump_runs") == [(RUN_AT,)]


def test_plaids_update_times_are_kept_beside_the_run(tree):
    write_run(tree, "20260131T070000Z", accounts=[account(
        1, last_updated_datetime="2026-01-30T12:00:00Z")])
    load_tree(tree)
    assert rows(tree, "SELECT balance_updated_at FROM accounts") == [
        (JAN_2 + 28 * UTC_DAY + 12 * 3600,)]
    assert rows(tree, "SELECT run_at, item_id, environment, "
                      "transactions_updated_at, investments_updated_at "
                      "FROM item_states") == [
        (RUN_AT, "item-synthetic-1", "sandbox",
         JAN_2 + 29 * UTC_DAY + 5 * 3600, JAN_2 + 29 * UTC_DAY + 6 * 3600)]


def test_every_run_restates_every_account(tree):
    # A quiet account must stay in each run's snapshot, or gold would read
    # it as closed.
    write_run(tree, "20260131T070000Z", accounts=[account(1), account(2)])
    write_run(tree, "20260131T080000Z", accounts=[account(1), account(2)])
    load_tree(tree)
    assert rows(tree, "SELECT snapshot_at, count(*) FROM accounts GROUP BY 1 "
                      "ORDER BY 1") == [(RUN_AT, 2), (RUN_AT + 3600, 2)]
    assert rows(tree, "SELECT count(*) FROM dump_runs") == [(2,)]


# ---- snapshots that change ------------------------------------------------------------

def test_each_run_holds_its_own_holdings(tree):
    accts = [account(2, "investment", "ira")]
    write_run(tree, "20260131T070000Z", accounts=accts,
              holdings=[holding(2, 1), holding(2, 2)],
              securities=[security(1), security(2)])
    write_run(tree, "20260131T080000Z", accounts=accts,
              holdings=[holding(2, 1)], securities=[security(1)])
    load_tree(tree)
    assert rows(tree, "SELECT snapshot_at, security_id FROM holdings "
                      "ORDER BY 1, 2") == [
        (RUN_AT, "sec-1"), (RUN_AT, "sec-2"), (RUN_AT + 3600, "sec-1")]


def test_an_account_that_holds_nothing_has_no_holding_row(tree):
    accts = [account(2, "investment", "ira")]
    write_run(tree, "20260131T070000Z", accounts=accts,
              holdings=[holding(2, 1)], securities=[security(1)])
    write_run(tree, "20260131T080000Z", accounts=accts, holdings=[])
    load_tree(tree)
    assert rows(tree, "SELECT count(*) FROM holdings WHERE snapshot_at = ?",
                RUN_AT + 3600) == [(0,)]
    assert rows(tree, "SELECT status, rows FROM run_products WHERE run_at = ? "
                      "AND product = 'holdings'", RUN_AT + 3600) == [
        ("fetched", 0)]


def test_two_holdings_of_one_security_are_both_kept(tree):
    write_run(tree, "20260131T070000Z",
              accounts=[account(2, "investment", "ira")],
              holdings=[holding(2, 1, quantity=1), holding(2, 1, quantity=2)],
              securities=[security(1)])
    load_tree(tree)
    assert rows(tree, "SELECT seq, quantity FROM holdings ORDER BY seq") == [
        (0, "1"), (1, "2")]


def test_a_security_keeps_its_newest_description_and_widens_its_range(tree):
    accts = [account(2, "investment", "ira")]
    renamed = security(1)
    renamed["name"] = "Synthetic renamed"
    write_run(tree, "20260130T070000Z", accounts=accts,
              holdings=[holding(2, 1)], securities=[security(1)],
              item=item_doc(transactions_at="2026-01-30T05:00:00Z",
                            investments_at="2026-01-30T06:00:00Z"))
    write_run(tree, "20260131T070000Z", accounts=accts,
              holdings=[holding(2, 1)], securities=[renamed])
    load_tree(tree)
    (row,) = rows(tree, "SELECT name, first_seen_at, last_seen_at FROM "
                        "securities")
    assert row[0] == "Synthetic renamed"
    assert row[2] - row[1] == UTC_DAY


# ---- the ledgers' windows ---------------------------------------------------------------

def test_a_row_plaid_no_longer_lists_leaves_silver(tree):
    # A pending charge that posts is listed again under a new id.
    write_run(tree, "20260130T070000Z",
              transactions=[tx(1, pending=True), tx(2)])
    write_run(tree, "20260131T070000Z", transactions=[tx(3), tx(2)])
    load_tree(tree)
    assert rows(tree, "SELECT transaction_id FROM transactions ORDER BY 1") \
        == [("tx-2",), ("tx-3",)]


def test_rows_outside_the_window_stay(tree):
    write_run(tree, "20260130T070000Z", since="2025-01-01",
              transactions=[tx(1, date="2025-06-01"), tx(2)])
    write_run(tree, "20260131T070000Z", since="2026-01-01",
              transactions=[tx(2)])
    load_tree(tree)
    assert rows(tree, "SELECT transaction_id FROM transactions ORDER BY 1") \
        == [("tx-1",), ("tx-2",)]


def test_an_account_no_longer_listed_keeps_its_rows(tree):
    write_run(tree, "20260130T070000Z",
              accounts=[account(1), account(3, "credit", "credit card")],
              transactions=[tx(1, account_n=1), tx(2, account_n=3)])
    write_run(tree, "20260131T070000Z", accounts=[account(1)],
              transactions=[tx(1, account_n=1)])
    load_tree(tree)
    assert rows(tree, "SELECT transaction_id FROM transactions ORDER BY 1") \
        == [("tx-1",), ("tx-2",)]


def test_a_product_not_fetched_leaves_earlier_rows(tree):
    write_run(tree, "20260130T070000Z", transactions=[tx(1)])
    write_run(tree, "20260131T070000Z", products={"transactions": {
        "status": "failed", "error_code": "INTERNAL_SERVER_ERROR"}})
    load_tree(tree)
    assert rows(tree, "SELECT transaction_id FROM transactions") == [
        ("tx-1",)]
    assert rows(tree, "SELECT run_at, status, error_code FROM run_products "
                      "WHERE product = 'transactions' ORDER BY run_at")[-1][
        1:] == ("failed", "INTERNAL_SERVER_ERROR")


def test_a_partial_ledger_removes_no_posted_row(tree):
    # Plaid holds only part of the history: absence proves nothing yet.
    write_run(tree, "20260130T070000Z", transactions=[tx(1), tx(2)])
    write_run(tree, "20260131T070000Z", history="INITIAL_UPDATE_COMPLETE",
              transactions=[tx(2, amount=20), tx(3)])
    load_tree(tree)
    assert rows(tree, "SELECT transaction_id, amount FROM transactions "
                      "ORDER BY 1") == [
        ("tx-1", "-12.34"), ("tx-2", "-20"), ("tx-3", "-12.34")]
    assert rows(tree, "SELECT status, history FROM run_products WHERE "
                      "product = 'transactions' ORDER BY run_at") == [
        ("fetched", "HISTORICAL_UPDATE_COMPLETE"),
        ("partial", "INITIAL_UPDATE_COMPLETE")]


def test_a_partial_read_replaces_stale_pending_rows(tree):
    # The charge posted under a new id while Plaid still assembled the
    # history; the pending row must not stand beside its posted twin.
    write_run(tree, "20260130T070000Z", history="INITIAL_UPDATE_COMPLETE",
              transactions=[tx(1, pending=True), tx(2)])
    write_run(tree, "20260131T070000Z", history="INITIAL_UPDATE_COMPLETE",
              transactions=[tx(3)])
    load_tree(tree)
    assert rows(tree, "SELECT transaction_id FROM transactions ORDER BY 1") \
        == [("tx-2",), ("tx-3",)]


def test_a_complete_ledger_after_a_partial_one_settles_the_window(tree):
    write_run(tree, "20260130T070000Z", history="INITIAL_UPDATE_COMPLETE",
              transactions=[tx(1, pending=True)])
    write_run(tree, "20260131T070000Z", transactions=[tx(2)])
    load_tree(tree)
    assert rows(tree, "SELECT transaction_id FROM transactions") == [
        ("tx-2",)]


def test_a_pending_row_older_than_the_window_leaves_on_a_full_read(tree):
    # It posted under a new id; a later run's window no longer reaches it.
    write_run(tree, "20260130T070000Z", since="2025-12-01",
              transactions=[tx(1, date="2025-12-20", pending=True)])
    write_run(tree, "20260131T070000Z", since="2026-01-01",
              transactions=[tx(2, date="2026-01-05")])
    load_tree(tree)
    assert rows(tree, "SELECT transaction_id FROM transactions") == [
        ("tx-2",)]


def test_the_window_is_inclusive_and_covers_listed_accounts(tree):
    # An account the answer lists, with no row this time, loses the rows
    # dated in the window; both ends of the window count.
    accts = [account(1), account(3, "credit", "credit card")]
    write_run(tree, "20260130T070000Z", accounts=accts, since="2026-01-01",
              until="2026-01-31", transactions=[
                  tx(1, account_n=1, date="2026-01-01"),
                  tx(2, account_n=1, date="2026-01-31"),
                  tx(3, account_n=3, date="2026-01-15"),
                  tx(4, account_n=3, date="2025-12-31")])
    write_run(tree, "20260131T070000Z", accounts=accts, since="2026-01-01",
              until="2026-01-31", transactions=[])
    load_tree(tree)
    assert rows(tree, "SELECT transaction_id FROM transactions") == [
        ("tx-4",)]


def test_run_products_keeps_each_window_and_history(tree):
    # Gold's change window spans every window a ledger read covered.
    write_run(tree, "20260131T070000Z", since="2025-06-01",
              until="2026-01-31", transactions=[tx(1)],
              investment_transactions=[itx(1)], securities=[security(1)],
              history="INITIAL_UPDATE_COMPLETE", liabilities={},
              products={"holdings": {"status": "failed",
                                     "error_code": "INTERNAL_SERVER_ERROR"}})
    load_tree(tree)
    window = (load.day("2025-06-01"), load.day("2026-01-31"))
    assert rows(tree, "SELECT product, status, window_start, window_end, "
                      "history FROM run_products ORDER BY product") == [
        ("accounts", "fetched", None, None, None),
        ("holdings", "failed", None, None, None),
        ("investment_transactions", "fetched", *window, None),
        ("liabilities", "fetched", None, None, None),
        ("transactions", "partial", *window, "INITIAL_UPDATE_COMPLETE")]


def test_every_page_of_a_ledger_is_loaded(tree):
    run = write_run(tree, "20260131T070000Z", transactions=[tx(1)])
    manifest = json.loads((run / "run.json").read_text())
    (run / "transactions-0002.json").write_text(json.dumps({
        "accounts": [account(1)], "transactions": [tx(2)],
        "total_transactions": 2}))
    manifest["products"]["transactions"]["files"].append(
        "transactions-0002.json")
    (run / "run.json").write_text(json.dumps(manifest))
    load_tree(tree)
    assert rows(tree, "SELECT transaction_id FROM transactions ORDER BY 1") \
        == [("tx-1",), ("tx-2",)]


def test_a_security_named_only_by_trades_is_kept(tree):
    accts = [account(2, "investment", "ira")]
    write_run(tree, "20260131T070000Z", accounts=accts, holdings=[],
              investment_transactions=[itx(1, security_n=7)],
              securities=[security(7, ticker="EXZ")])
    load_tree(tree)
    assert rows(tree, "SELECT security_id, ticker_symbol FROM securities") \
        == [("sec-7", "EXZ")]


def test_the_columns_gold_reads_hold_their_own_values(tree):
    write_run(tree, "20260131T070000Z",
              accounts=[account(2, "investment", "ira", 321.5,
                                available=12.25, limit=99)],
              holdings=[{**holding(2, 1, quantity=3, value=31.5),
                         "institution_price": 10.5, "cost_basis": 28.75}],
              securities=[security(1)])
    load_tree(tree)
    assert rows(tree, "SELECT balance_current, balance_available, "
                      "balance_limit FROM accounts") == [
        ("321.5", "12.25", "99")]
    assert rows(tree, "SELECT quantity, institution_price, institution_value, "
                      "cost_basis FROM holdings") == [
        ("3", "10.5", "31.5", "28.75")]


def test_investment_rows_follow_the_same_window_rule(tree):
    accts = [account(2, "investment", "ira")]
    write_run(tree, "20260130T070000Z", accounts=accts,
              investment_transactions=[itx(1), itx(2)],
              securities=[security(1)])
    write_run(tree, "20260131T070000Z", accounts=accts,
              investment_transactions=[itx(2)], securities=[security(1)])
    load_tree(tree)
    assert rows(tree, "SELECT investment_transaction_id FROM "
                      "investment_transactions") == [("itx-2",)]


# ---- liabilities -----------------------------------------------------------------------

def test_liability_terms_are_restated_by_every_run(tree):
    terms = {"account_id": "acc-3", "interest_rate": {"percentage": 3.99,
             "type": "fixed"}, "next_monthly_payment": 1500.25,
             "origination_principal_amount": 300000,
             "origination_date": "2020-01-02"}
    changed = {**terms, "next_monthly_payment": 1510}
    accts = [account(3, "loan", "mortgage")]
    for day_, row in ((29, terms), (30, terms), (31, changed)):
        write_run(tree, f"202601{day_}T070000Z", accounts=accts,
                  liabilities={"mortgage": [row]})
    load_tree(tree)
    assert rows(tree, "SELECT kind, interest_rate, next_payment_amount, "
                      "origination_date FROM liabilities ORDER BY "
                      "snapshot_at") == [
        ("mortgage", "3.99", "1500.25", load.day("2020-01-02")),
        ("mortgage", "3.99", "1500.25", load.day("2020-01-02")),
        ("mortgage", "3.99", "1510", load.day("2020-01-02"))]


def test_a_card_and_a_student_loan_keep_their_statement(tree):
    write_run(tree, "20260131T070000Z", liabilities={
        "credit": [{"account_id": "acc-3", "aprs": [
            {"apr_type": "cash_apr", "apr_percentage": 25},
            {"apr_type": "purchase_apr", "apr_percentage": 19.9}],
            "last_statement_balance": 410.5,
            "last_statement_issue_date": "2026-01-10",
            "minimum_payment_amount": 25}],
        "student": [{"account_id": "acc-4", "interest_rate_percentage": 5.25,
                     "last_statement_balance": 900,
                     "last_statement_issue_date": "2026-01-05",
                     "minimum_payment_amount": 75}]})
    load_tree(tree)
    assert rows(tree, "SELECT kind, interest_rate, last_statement_balance, "
                      "last_statement_issue_date, next_payment_amount FROM "
                      "liabilities ORDER BY kind") == [
        ("credit", "19.9", "410.5", load.day("2026-01-10"), "25"),
        ("student", "5.25", "900", load.day("2026-01-05"), "75")]


def test_a_liability_without_an_account_is_skipped(tree, caplog):
    write_run(tree, "20260131T070000Z", liabilities={"student": [{
        "account_id": None, "interest_rate_percentage": 5}]})
    load_tree(tree)
    assert rows(tree, "SELECT count(*) FROM liabilities") == [(0,)]
    assert "names no account" in caplog.text


# ---- which runs, and in which order ----------------------------------------------------

@pytest.mark.parametrize("status", ["in-progress", "failed", "dry-run"])
def test_a_run_that_did_not_complete_is_not_loaded(tree, status):
    write_run(tree, "20260131T070000Z", status=status)
    load_tree(tree)
    assert rows(tree, "SELECT count(*) FROM dump_runs") == [(0,)]


def test_a_second_load_is_a_no_op(tree):
    write_run(tree, "20260131T070000Z", transactions=[tx(1)])
    load_tree(tree)
    before = rows(tree, "SELECT * FROM transactions")
    loaded = rows(tree, "SELECT loaded_at FROM dump_runs")
    assert load_tree(tree) == 0
    assert rows(tree, "SELECT * FROM transactions") == before
    assert rows(tree, "SELECT loaded_at FROM dump_runs") == loaded


def test_force_rebuilds_the_same_silver(tree):
    write_run(tree, "20260130T070000Z", transactions=[tx(1, pending=True)])
    write_run(tree, "20260131T070000Z", transactions=[tx(2)])
    load_tree(tree)
    before = rows(tree, "SELECT transaction_id, amount FROM transactions")
    assert load_tree(tree, "--force") == 0
    assert rows(tree, "SELECT transaction_id, amount FROM transactions") \
        == before


def test_an_older_run_found_later_rebuilds_the_item(tree, caplog):
    # Replayed on top of the newer run, the older one would bring back the
    # pending row the newer one had removed.
    write_run(tree, "20260131T070000Z", transactions=[tx(2)])
    load_tree(tree)
    write_run(tree, "20260130T070000Z", transactions=[tx(1, pending=True)])
    assert load_tree(tree) == 0
    assert rows(tree, "SELECT transaction_id FROM transactions") == [
        ("tx-2",)]
    assert rows(tree, "SELECT count(*) FROM dump_runs") == [(2,)]
    assert "rebuilding" in caplog.text


@pytest.mark.parametrize("other", [
    {"item_id": "item-synthetic-2"}, {"environment": "production"}])
def test_a_tree_with_runs_of_two_items_is_refused(tree, caplog, other):
    write_run(tree, "20260130T070000Z", transactions=[tx(1)])
    write_run(tree, "20260131T070000Z", transactions=[tx(2)], **other)
    assert load_tree(tree) == 1
    assert not (tree / "bank.db").exists()
    assert "holds runs that are not" in caplog.text


def test_a_tree_holding_another_collectors_run_is_refused(tree):
    write_run(tree, "20260130T070000Z", transactions=[tx(1)])
    foreign = tree / "20260131T070000Z"
    foreign.mkdir()
    (foreign / "export.csv").write_text("not plaid's\n")
    with pytest.raises(SystemExit, match="holds runs that are not plaid's"):
        load_tree(tree)
    assert not (tree / "bank.db").exists()


def test_a_database_of_another_item_is_refused(tree, tmp_path, caplog):
    write_run(tree, "20260130T070000Z", transactions=[tx(1)])
    load_tree(tree)
    other = tmp_path / "other" / "bank"
    write_run(other, "20260131T070000Z", transactions=[tx(2)],
              item_id="item-synthetic-2")
    target = tree / "bank.db"
    assert load.main(["--bronze-dir", str(other.parent), "--item", "bank",
                      "--silver-db", str(target)]) == 1
    assert rows(tree, "SELECT transaction_id FROM transactions") == [
        ("tx-1",)]
    assert "holds the silver of another Item" in caplog.text
    assert load.main(["--bronze-dir", str(other.parent), "--item", "bank",
                      "--silver-db", str(target), "--force"]) == 0
    assert rows(tree, "SELECT transaction_id FROM transactions") == [
        ("tx-2",)]


def sqlite_file(path, table="foo"):
    conn = sqlite3.connect(path)
    conn.execute(f"CREATE TABLE {table} (x)")
    conn.commit()
    conn.close()
    return path.read_bytes()


def test_load_pointed_at_the_data_root_touches_no_database(tmp_path):
    # A mistyped --bronze-dir: every other collector's dir looks like an
    # Item tree, with its silver where an Item's would be.
    root = tmp_path / "data"
    chase_run = root / "chase" / "20260101T000000Z"
    chase_run.mkdir(parents=True)
    (chase_run / "run.json").write_text(json.dumps({"status": "complete"}))
    chase_db = sqlite_file(root / "chase" / "chase.db")
    (root / "fred").mkdir()
    fred_db = sqlite_file(root / "fred" / "fred.db")
    with pytest.raises(SystemExit, match="holds runs that are not plaid's"):
        load.main(["--bronze-dir", str(root), "--force"])
    assert (root / "chase" / "chase.db").read_bytes() == chase_db
    assert (root / "fred" / "fred.db").read_bytes() == fred_db
    # A root that holds only run-less dirs is no better a target.
    (root / "chase" / "20260101T000000Z" / "run.json").unlink()
    (root / "chase" / "20260101T000000Z").rmdir()
    assert load.main(["--bronze-dir", str(root), "--force"]) == 0
    assert (root / "chase" / "chase.db").read_bytes() == chase_db
    assert (root / "fred" / "fred.db").read_bytes() == fred_db


def test_a_database_that_is_not_plaid_silver_is_left_alone(tree, caplog):
    write_run(tree, "20260131T070000Z", transactions=[tx(1)])
    tree.mkdir(parents=True, exist_ok=True)
    before = sqlite_file(tree / "bank.db")
    for argv in ([], ["--force"]):
        assert load_tree(tree, *argv) == 1
        assert (tree / "bank.db").read_bytes() == before
    assert "is not a plaid silver database" in caplog.text


def test_a_tree_with_no_run_yet_opens_no_database(tmp_path):
    root = tmp_path / "plaid"
    (root / "bank").mkdir(parents=True)
    assert load.main(["--bronze-dir", str(root)]) == 0
    assert not (root / "bank" / "bank.db").exists()


def test_a_run_not_in_the_shape_download_writes_stops_its_tree(tree, caplog):
    write_run(tree, "20260130T070000Z", transactions=[tx(1)])
    bad = write_run(tree, "20260131T070000Z", accounts=[{"name": "no id"}])
    assert load_tree(tree) == 1
    assert rows(tree, "SELECT count(*) FROM dump_runs") == [(1,)]
    assert f"{bad} cannot be loaded (KeyError" in caplog.text


def test_the_rebuild_advice_names_the_database_it_is_about(tree, tmp_path,
                                                          caplog):
    write_run(tree, "20260130T070000Z", transactions=[tx(1)])
    target = tmp_path / "elsewhere.db"
    assert load.main(["--bronze-dir", str(tree.parent), "--item", "bank",
                      "--silver-db", str(target)]) == 0
    other = tmp_path / "other" / "bank"
    write_run(other, "20260131T070000Z", item_id="item-synthetic-2")
    assert load.main(["--bronze-dir", str(other.parent), "--item", "bank",
                      "--silver-db", str(target)]) == 1
    assert (f"`load --force --item bank --silver-db {target}` rebuilds it"
            in caplog.text)


def test_one_refused_tree_does_not_stop_the_others(tmp_path):
    root = tmp_path / "plaid"
    write_run(root / "bank", "20260130T070000Z")
    write_run(root / "bank", "20260131T070000Z", item_id="item-synthetic-2")
    write_run(root / "broker", "20260131T070000Z")
    assert load.main(["--bronze-dir", str(root)]) == 1
    assert (root / "broker" / "broker.db").exists()


def test_a_rebuild_that_cannot_finish_keeps_the_silver_in_place(tree):
    write_run(tree, "20260130T070000Z", transactions=[tx(1)])
    load_tree(tree)
    before = rows(tree, "SELECT transaction_id FROM transactions")
    broken = write_run(tree, "20260131T070000Z", transactions=[tx(2)])
    (broken / "accounts.json").write_text("{")
    assert load_tree(tree, "--force") == 1
    assert rows(tree, "SELECT transaction_id FROM transactions") == before
    assert not (tree / "bank.db.rebuild").exists()
    # An older run found later takes the same way.
    write_run(tree, "20260129T070000Z", transactions=[tx(0)])
    assert load_tree(tree) == 1
    assert rows(tree, "SELECT transaction_id FROM transactions") == before


def test_a_rebuild_puts_an_owner_only_silver_in_place(tree):
    write_run(tree, "20260130T070000Z", transactions=[tx(1)])
    assert load_tree(tree, "--force") == 0
    assert (tree / "bank.db").stat().st_mode & 0o777 == 0o600
    assert rows(tree, "SELECT count(*) FROM dump_runs") == [(1,)]


def test_a_run_file_that_is_not_json_stops_its_tree(tree, caplog):
    run = write_run(tree, "20260131T070000Z", transactions=[tx(1)])
    (run / "transactions-0001.json").write_text("not json")
    assert load_tree(tree) == 1
    assert "cannot be loaded (JSONDecodeError" in caplog.text


def test_a_complete_run_with_a_file_missing_stops_the_tree(tree, caplog):
    write_run(tree, "20260130T070000Z", transactions=[tx(1)])
    broken = write_run(tree, "20260131T070000Z", transactions=[tx(2)])
    write_run(tree, "20260131T080000Z", transactions=[tx(3)])
    (broken / "accounts.json").unlink()
    assert load_tree(tree) == 1
    assert rows(tree, "SELECT snapshot_at FROM dump_runs") == [
        (RUN_AT - UTC_DAY,)]
    assert "lacks accounts.json" in caplog.text


def test_each_item_gets_its_own_database(tmp_path):
    root = tmp_path / "plaid"
    write_run(root / "bank", "20260131T070000Z", transactions=[tx(1)])
    write_run(root / "broker", "20260131T070000Z", transactions=[tx(2)])
    (root / "notes").write_text("not an Item tree")
    assert load.main(["--bronze-dir", str(root)]) == 0
    assert (root / "bank" / "bank.db").exists()
    assert (root / "broker" / "broker.db").exists()
    assert rows(root / "bank", "SELECT transaction_id FROM transactions") \
        == [("tx-1",)]


def test_named_items_and_a_silver_path_for_one(tmp_path):
    root = tmp_path / "plaid"
    write_run(root / "bank", "20260131T070000Z")
    write_run(root / "broker", "20260131T070000Z")
    target = tmp_path / "elsewhere.db"
    assert load.main(["--bronze-dir", str(root), "--item", "broker",
                      "--silver-db", str(target)]) == 0
    assert target.exists()
    assert not (root / "broker" / "broker.db").exists()
    assert not (root / "bank" / "bank.db").exists()


@pytest.mark.parametrize("argv", [
    ["--silver-db", "x.db"],
    ["--silver-db", "x.db", "--item", "a", "--item", "b"],
    ["--item", "Bad Name"],
])
def test_arguments_that_do_not_apply_are_refused(tmp_path, argv):
    with pytest.raises(SystemExit) as caught:
        load.parse_args(["--bronze-dir", str(tmp_path), *argv])
    assert caught.value.code == 2


def test_an_unknown_item_is_refused(tmp_path):
    write_run(tmp_path / "plaid" / "bank", "20260131T070000Z")
    with pytest.raises(SystemExit, match="no Item tree"):
        load.main(["--bronze-dir", str(tmp_path / "plaid"), "--item",
                   "absent"])


def test_nothing_to_load_is_fine(tmp_path):
    assert load.main(["--bronze-dir", str(tmp_path / "absent")]) == 0


# ---- what download writes, and what a tree may hold ---------------------------------

def test_a_run_download_writes_is_what_load_reads(tmp_path, monkeypatch):
    # The round trip keeps the fixtures above honest: a run written by
    # download itself, through the scripted Plaid, loads in full.
    from datetime import date

    from conftest import FakePlaid, access_token

    import download
    import items

    fake = FakePlaid("sandbox")
    item = items.Item(name="bank", environment="sandbox",
                      access_token=access_token(), item_id="item-synthetic-1")
    fake.item_docs[access_token()] = item_doc()
    accts = [account(1), account(2, "investment", "ira"),
             account(3, "credit", "credit card")]
    fake.data.update({
        "accounts": {"accounts": accts},
        "holdings": {"accounts": accts, "holdings": [holding(2, 1)],
                     "securities": [security(1)]},
        "liabilities": {"liabilities": {"credit": [{
            "account_id": "acc-3", "last_statement_balance": 410,
            "last_statement_issue_date": "2026-01-10"}]}},
        "transactions": lambda offset: {
            "accounts": accts, "total_transactions": 3,
            "transactions": [tx(1), tx(2, account_n=3), tx(3)][
                offset:offset + 2]},
        "investment_transactions": lambda offset: {
            "accounts": accts, "securities": [security(1)],
            "total_investment_transactions": 1,
            "investment_transactions": [itx(1)][offset:offset + 2]},
    })
    monkeypatch.setattr(download, "_sleep", lambda seconds: None)
    root = tmp_path / "plaid"
    assert download.download_item(fake, item, root, date(2026, 1, 1),
                                  date(2026, 1, 31), debug=False)
    assert load.main(["--bronze-dir", str(root)]) == 0
    tree = root / "bank"
    assert rows(tree, "SELECT count(*) FROM accounts") == [(3,)]
    assert rows(tree, "SELECT security_id FROM holdings") == [("sec-1",)]
    assert rows(tree, "SELECT transaction_id FROM transactions ORDER BY 1") \
        == [("tx-1",), ("tx-2",), ("tx-3",)]
    assert rows(tree, "SELECT investment_transaction_id FROM "
                      "investment_transactions") == [("itx-1",)]
    assert rows(tree, "SELECT kind, last_statement_balance FROM "
                      "liabilities") == [("credit", "410")]
    assert rows(tree, "SELECT product, status, window_start FROM "
                      "run_products WHERE product = 'transactions'") == [
        ("transactions", "fetched", load.day("2026-01-01"))]


def test_runs_that_are_not_complete_are_passed_over(tree):
    write_run(tree, "20260131T070000Z", transactions=[tx(1)])
    statusless = json.dumps({"item": "bank", "item_id": "item-synthetic-1",
                             "environment": "sandbox"})
    in_progress = json.dumps({"status": "in-progress", "item": "bank",
                              "item_id": "item-synthetic-1",
                              "environment": "sandbox"})
    for slug, content in (("20260130T070000Z", statusless),
                          ("20260129T070000Z", in_progress)):
        (tree / slug).mkdir()
        (tree / slug / "run.json").write_text(content)
    stopped = tree / "20260128T070000Z"
    stopped.mkdir()
    (stopped / "run.json.tmp").write_text("{")
    assert load_tree(tree) == 0
    assert rows(tree, "SELECT snapshot_at FROM dump_runs") == [(RUN_AT,)]


def test_a_run_json_that_cannot_be_read_stops_its_tree(tree, caplog):
    # It may hide a complete run; the runs after it must not load over
    # the gap.
    write_run(tree, "20260130T070000Z", transactions=[tx(1)])
    damaged = tree / "20260131T060000Z"
    damaged.mkdir()
    (damaged / "run.json").write_text("{not json")
    write_run(tree, "20260131T070000Z", transactions=[tx(2)])
    assert load_tree(tree) == 1
    assert rows(tree, "SELECT snapshot_at FROM dump_runs") == [
        (RUN_AT - UTC_DAY,)]
    assert f"{damaged} cannot be loaded" in caplog.text


def test_force_on_a_refused_tree_keeps_its_database(tree):
    write_run(tree, "20260130T070000Z", transactions=[tx(1)])
    assert load_tree(tree) == 0
    write_run(tree, "20260131T070000Z", item_id="item-synthetic-2")
    assert load_tree(tree, "--force") == 1
    assert rows(tree, "SELECT count(*) FROM dump_runs") == [(1,)]


def test_a_corrupt_file_stops_only_its_tree(tmp_path):
    root = tmp_path / "plaid"
    run = write_run(root / "bank", "20260131T070000Z", transactions=[tx(1)])
    (run / "transactions-0001.json").write_text("{not json")
    write_run(root / "broker", "20260131T070000Z")
    assert load.main(["--bronze-dir", str(root)]) == 1
    assert (root / "broker" / "broker.db").exists()


def test_a_ledger_row_keeps_every_column_gold_reads(tree):
    t = tx(1, date="2026-01-20", pending=True)
    t.update(authorized_date="2026-01-18", name="SYN POS 01",
             merchant_name="Synthetic Grocer", check_number="1001",
             iso_currency_code=None, unofficial_currency_code="SYN")
    write_run(tree, "20260131T070000Z", transactions=[t])
    load_tree(tree)
    assert rows(tree, "SELECT posted_at, authorized_date, name, "
                      "merchant_name, pending, check_number, currency "
                      "FROM transactions") == [
        (load.day("2026-01-20"), load.day("2026-01-18"), "SYN POS 01",
         "Synthetic Grocer", 1, "1001", "SYN")]


def test_an_investment_row_keeps_every_column_gold_reads(tree):
    i = itx(1, type_="cancel", subtype="cancel", amount=-105.5, security_n=4)
    i.update(transaction_datetime="2026-01-20T14:30:00Z", price=10.55,
             quantity=-10, fees=1.25, iso_currency_code="USD",
             cancel_transaction_id="itx-0")
    write_run(tree, "20260131T070000Z",
              accounts=[account(2, "investment", "ira")],
              investment_transactions=[i], securities=[security(4)])
    load_tree(tree)
    assert rows(tree, "SELECT security_id, posted_at, transaction_at, type, "
                      "amount, quantity, price, fees, currency, "
                      "cancel_transaction_id FROM investment_transactions") == [
        ("sec-4", load.day("2026-01-20"),
         load.day("2026-01-20") + 14 * 3600 + 1800, "cancel", "105.5", "-10",
         "10.55", "1.25", "USD", "itx-0")]
