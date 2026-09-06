"""Tests for cards.py — the card-surface bronze capture.

Two things carry weight here. The **endpoint guard**: the roster
advertises links that would move money or alter a card, and the ledger
hands back a cursor the walk follows, so "follow the link" must never
mean "follow any link". And the **enumeration**: the roster nests, and
an id missed while walking it is an account silently not collected —
the exact failure that made the homepage anchors unusable.

Every fixture value is invented.
"""
from __future__ import annotations

import json
from datetime import date

import pytest

import cards


# --------------------------------------------------------------------
# The endpoint guard
# --------------------------------------------------------------------

@pytest.mark.parametrize("path", [
    "/api/v2/credit-card-accounts",
    "/api/v1/credit-card-transactions",
    "/api/v1/credit-card-invoices",
    "/api/v1/credit-card-invoices/INV-0001",
    "/api/v1/credit-card-invoices/INV-0001/extract",
])
def test_read_endpoints_are_admitted(path):
    assert cards.refuse_path(path) is None


@pytest.mark.parametrize("path", [
    # Links the roster itself advertises, every one of them forbidden.
    "/api/v1/credit-card-accounts/ACC-1/payment-to-card",
    "/api/v2/credit-card-accounts/ACC-1/unregister",
    "/api/v2/credit-card-accounts/ACC-1/reassign",
    "/api/v2/credit-card-accounts/ACC-1/orders/search",
    "/api/v1/credit-cards/registrations",
    # Neighbouring surfaces that are not this collector's business.
    "/api/v1/payments/orders/search",
    "/api/v3/trading/cross-product/orders/search",
    # A path that merely starts like an allowed one.
    "/api/v2/credit-card-accounts-admin",
    "/api/v1/credit-card-invoices/INV-1/pay",
    # The ledger's CSV/PDF export. Refused not because it is dangerous
    # but because it is redundant: the JSON it renders from is already
    # captured, and it carries a row id the export drops. The allow-list
    # is where that decision is enforced rather than merely intended.
    "/api/v1/credit-card-transactions/extract",
])
def test_mutating_and_foreign_endpoints_are_refused(path):
    assert cards.refuse_path(path) is not None


def test_absolute_urls_are_refused():
    # A cursor pointing at another host must not be followed.
    assert cards.refuse_path(
        "https://evil.test/api/v1/credit-card-transactions") is not None


def test_guard_runs_before_the_request_is_made():
    class _Ctx:
        class request:
            @staticmethod
            def get(*_a, **_kw):  # pragma: no cover - must not run
                raise AssertionError("issued a refused request")

    api = cards.CardApi(_Ctx(), "https://bank.test", "KEY")
    with pytest.raises(RuntimeError, match="refusing to request"):
        api._get("/api/v2/credit-card-accounts/ACC-1/unregister")


# --------------------------------------------------------------------
# A whole capture, against a scripted API
# --------------------------------------------------------------------

class _FakeApi:
    """Stands in for CardApi with invented payloads."""

    def __init__(self, *, ledger_pages=1, fail_ledger_for=(),
                 statement=b"%PDF-1.4 x", truncated=False):
        self.ledger_pages = ledger_pages
        self.truncated = truncated
        self.fail_ledger_for = set(fail_ledger_for)
        self.statement = statement
        self.calls = []

    def accounts(self):
        self.calls.append("accounts")
        return {"creditCardAccounts": [
            {"id": "ACC-1", "accountType": "CREDIT_CARD_ACCOUNT"},
            {"id": "ACC-2", "accountType": "CREDIT_CARD_ACCOUNT"},
        ]}

    def transactions(self, account_id, since, until):
        self.calls.append(("transactions", account_id))
        if account_id in self.fail_ledger_for:
            raise RuntimeError("HTTP 500")
        return ([{"_embedded": {"transactions": [{"transactionNr": "T1"}]}}
                 for _ in range(self.ledger_pages)], self.truncated)

    def invoices(self, account_id):
        self.calls.append(("invoices", account_id))
        return {"invoices": [
            {"id": f"{account_id}-INV-1", "periodFrom": "2020-01-01",
             "periodTo": "2020-01-31"},
        ]}

    def invoice(self, invoice_id, account_id):
        # The account id is required by the real endpoint; the fake takes
        # it so a caller that forgets it fails here rather than live.
        self.calls.append(("invoice", invoice_id, account_id))
        return {"id": invoice_id, "balanceForward": "10.00"}

    def statement_pdf(self, invoice_id):
        self.calls.append(("statement", invoice_id))
        return self.statement


def test_capture_writes_the_whole_surface(tmp_path):
    api = _FakeApi()
    meta = cards.capture(api, tmp_path, date(2020, 1, 1), date(2020, 3, 31))
    d = tmp_path / "cards"

    assert (d / "accounts.json").is_file()
    assert len(meta["accounts"]) == 2
    assert meta["errors"] == []
    assert meta["transaction_date_type"] == cards.TRANSACTION_DATE_TYPE
    for entry in meta["accounts"]:
        assert (d / entry["transactions_file"]).is_file()
        assert (d / entry["invoices_file"]).is_file()
        assert (d / entry["invoice_details_file"]).is_file()
        assert entry["invoice_count"] == 1
        assert entry["statement_count"] == 1


def test_capture_stores_every_ledger_page(tmp_path):
    api = _FakeApi(ledger_pages=3)
    meta = cards.capture(api, tmp_path, date(2020, 1, 1), date(2020, 3, 31))
    entry = meta["accounts"][0]
    assert entry["transaction_pages"] == 3
    payload = json.loads((tmp_path / "cards" / entry["transactions_file"]).read_text())
    assert len(payload["pages"]) == 3


def test_one_failing_account_does_not_lose_the_others(tmp_path):
    api = _FakeApi(fail_ledger_for=["ACC-1"])
    meta = cards.capture(api, tmp_path, date(2020, 1, 1), date(2020, 3, 31))
    assert len(meta["errors"]) == 1
    assert meta["errors"][0]["stage"] == "transactions"
    # ACC-1 still records its invoices, and ACC-2 is untouched by the failure.
    assert "transactions_file" not in meta["accounts"][0]
    assert meta["accounts"][0]["invoice_count"] == 1
    assert "transactions_file" in meta["accounts"][1]


def test_statements_are_content_addressed_and_written_once(tmp_path):
    api = _FakeApi()
    cards.capture(api, tmp_path, date(2020, 1, 1), date(2020, 3, 31))
    stmts = sorted((tmp_path / "cards" / "statements").iterdir())
    # Both accounts' statements are identical bytes here, so one file.
    assert len(stmts) == 1
    assert stmts[0].name.endswith(".pdf") and len(stmts[0].stem) == 64


def test_statements_can_be_skipped_without_losing_the_figures(tmp_path):
    api = _FakeApi()
    meta = cards.capture(api, tmp_path, date(2020, 1, 1), date(2020, 3, 31),
                         statements=False)
    assert not (tmp_path / "cards" / "statements").exists()
    assert ("statement", "ACC-1-INV-1") not in api.calls
    # The periods and their reconciling figures still land.
    assert meta["accounts"][0]["invoice_count"] == 1


def test_a_non_pdf_statement_is_dropped_not_stored(tmp_path):
    api = _FakeApi(statement=None)
    meta = cards.capture(api, tmp_path, date(2020, 1, 1), date(2020, 3, 31))
    assert meta["accounts"][0]["statement_count"] == 0


# --------------------------------------------------------------------
# The apikey sniffer
# --------------------------------------------------------------------

class _Recorder:
    def __init__(self):
        self.handler = None

    def on(self, event, handler):
        assert event == "request"
        self.handler = handler


class _Req:
    def __init__(self, url, headers):
        self.url = url
        self.headers = headers


def test_sniffer_tallies_a_key_against_its_endpoint_families():
    ctx = _Recorder()
    tally = cards.sniff_apikey(ctx)
    ctx.handler(_Req("https://bank.test/api/v2/credit-card-accounts",
                     {"apikey": "KEY-1"}))
    ctx.handler(_Req("https://bank.test/api/v3/cash-accounts",
                     {"apikey": "KEY-1"}))
    assert tally["KEY-1"] == {"v2/credit-card-accounts", "v3/cash-accounts"}


def test_sniffer_ignores_non_api_traffic():
    ctx = _Recorder()
    tally = cards.sniff_apikey(ctx)
    ctx.handler(_Req("https://bank.test/app/spa.html", {"apikey": "KEY-X"}))
    assert tally == {}


def test_sniffer_never_raises_on_a_bad_request_object():
    class _Broken:
        url = "https://bank.test/api/x"

        @property
        def headers(self):
            raise RuntimeError("detached")

    ctx = _Recorder()
    tally = cards.sniff_apikey(ctx)
    ctx.handler(_Broken())          # must not propagate
    assert tally == {}


def test_the_broadest_key_wins_whatever_the_arrival_order():
    # A single-purpose widget's key can arrive first; the banking key is
    # the one covering many families, and that is the one cards need.
    ctx = _Recorder()
    tally = cards.sniff_apikey(ctx)
    ctx.handler(_Req("https://bank.test/api/v1/quotes/graphql",
                     {"apikey": "WIDGET"}))
    for path in ("v1/portfolios", "v3/cash-accounts",
                 "v2/credit-card-accounts", "v2/online-service"):
        ctx.handler(_Req(f"https://bank.test/api/{path}",
                         {"apikey": "BANKING"}))
    assert cards.pick_apikey(tally) == "BANKING"


def test_picking_from_an_empty_tally_yields_nothing():
    assert cards.pick_apikey({}) is None


def test_a_single_observed_key_is_picked():
    ctx = _Recorder()
    tally = cards.sniff_apikey(ctx)
    ctx.handler(_Req("https://bank.test/api/v1/portfolios", {"apikey": "ONLY"}))
    assert cards.pick_apikey(tally) == "ONLY"
