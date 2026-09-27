"""Unit tests for the browserless wire contract: endpoint builders, the
logon classifier, and the roster / activity / statement projections.

Synthetic payloads only — every id, mask and amount here is made up from
scratch, never derived from a real capture.
"""
from __future__ import annotations

import sys
from datetime import date
from pathlib import Path
from urllib.parse import parse_qs, urlsplit

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import amexclient  # noqa: E402

KEY = "0123456789ABCDEF0123456789ABCDEF"
TOKEN = "AAAA1B2C3D4E5F6"


# ============================================================
# Endpoint builders
# ============================================================

def test_export_url_carries_the_window_and_format():
    url = amexclient.export_url(KEY, "csv",
                                since=date(2026, 1, 1), until=date(2026, 3, 1))
    parts = urlsplit(url)
    q = parse_qs(parts.query)
    assert parts.netloc == "global.americanexpress.com"
    assert parts.path == amexclient.DOCUMENTS_PATH
    assert q["account_key"] == [KEY]
    assert q["file_format"] == ["csv"]
    assert q["start_date"] == ["2026-01-01"]
    assert q["end_date"] == ["2026-03-01"]
    # additional_fields is what widens the CSV to the extended-details
    # columns; status=posted mirrors the UI.
    assert q["additional_fields"] == ["true"]
    assert q["status"] == ["posted"]


def test_export_url_without_a_window_omits_the_dates():
    q = parse_qs(urlsplit(amexclient.export_url(KEY, "qfx")).query)
    assert "start_date" not in q and "end_date" not in q
    assert q["file_format"] == ["quicken"]


def test_export_handles_map_to_provider_formats_and_extensions():
    # Every handle the CLI accepts must resolve both ways, or a fetched file
    # lands under a name the loader cannot route.
    assert set(amexclient.EXPORT_FORMATS) == set(amexclient.EXPORT_EXTENSIONS)
    for handle in amexclient.DEFAULT_EXPORT_FORMATS:
        assert handle in amexclient.EXPORT_FORMATS


def test_unknown_export_handle_raises():
    import pytest
    with pytest.raises(KeyError):
        amexclient.export_url(KEY, "ofx")


def test_absolute_url_resolves_a_site_relative_download_option():
    assert amexclient.absolute_url("/api/servicing/v1/x?a=1") == (
        "https://global.americanexpress.com/api/servicing/v1/x?a=1")


def test_an_absent_document_url_stays_absent():
    # Resolving "" against the app origin would yield a fetchable page, so a
    # period offering no PDF would store the SPA's own HTML under a .pdf name.
    assert amexclient.absolute_url("") == ""
    entry = amexclient.StatementEntry(end_date="2026-01-12", options={})
    assert entry.pdf_url == ""


def test_absolute_url_leaves_an_absolute_one_alone():
    url = "https://global.americanexpress.com/api/servicing/v1/y"
    assert amexclient.absolute_url(url) == url


@pytest.mark.parametrize("url", [
    "https://example.net/api/servicing/v1/x",                  # another site
    "//example.net/api/servicing/v1/x",                        # protocol-rel
    "http://global.americanexpress.com/api/servicing/v1/x",    # not https
    "https://www.americanexpress.com/api/servicing/v1/x",      # sibling host
    "https://global.americanexpress.com/myca/x",               # off-surface
])
def test_a_document_url_off_the_servicing_surface_is_refused(url):
    # The document fetches carry the session jar, so where they may reach is
    # decided by the contract (AGENTS.md §1), not by the provider payload.
    # The host is matched exactly: sibling americanexpress.com hosts receive
    # the domain cookies too. A refusal reads as "no document".
    assert amexclient.absolute_url(url) == ""


def test_function_headers_carry_the_source_and_a_fresh_correlation_id():
    a = amexclient.function_headers("WEB_OVERVIEW")
    b = amexclient.function_headers("WEB_OVERVIEW")
    assert a["ce-source"] == "WEB_OVERVIEW"
    assert a["Origin"] == amexclient.APP_ORIGIN
    assert a["one-data-correlation-id"] != b["one-data-correlation-id"]


def test_function_headers_carry_no_credential():
    # Authentication is the cookie jar — there is no bearer token and no
    # CSRF header anywhere in the capture, and inventing one would be a
    # guess (DESIGN.md §A).
    headers = amexclient.function_headers("WEB")
    lowered = {k.lower() for k in headers}
    assert not lowered & {"authorization", "x-csrf-token", "cookie"}


# ============================================================
# Activity request bodies
# ============================================================

def test_activity_body_date_range_carries_iso_dates_and_paging():
    body = amexclient.activity_body(
        TOKEN, view=amexclient.VIEW_DATE_RANGE,
        since=date(2026, 2, 1), until=date(2026, 3, 1), offset=101)
    assert body["accountToken"] == TOKEN
    assert body["view"] == "DATE_RANGE"
    assert body["dateRange"] == {"startDate": "2026-02-01",
                                 "endDate": "2026-03-01"}
    assert body["transactionFilters"]["offset"] == 101


def test_activity_body_statements_view_has_no_paging_or_range():
    # The statements view returns the archive, not transactions.
    body = amexclient.activity_body(TOKEN, view=amexclient.VIEW_STATEMENTS)
    assert body["view"] == "STATEMENTS"
    assert "transactionFilters" not in body
    assert "dateRange" not in body


# ============================================================
# Logon classification
# ============================================================

def _logon(status_code, *, challenge=False, mfa_id="", trust=False,
           error_code="", http=200):
    body = {
        "statusCode": status_code,
        "errorCode": error_code,
        "errorMessage": "",
        "challenge": challenge,
        "reauth": {"trust": trust, "deviceRemembered": False},
    }
    if mfa_id:
        body["reauth"]["mfaId"] = mfa_id
    return http, body


def test_trusted_device_login_is_authenticated():
    out = amexclient.classify_logon(*_logon(0, trust=True))
    assert out.authenticated and not out.needs_challenge
    assert out.trust is True


def test_challenge_is_detected_from_the_mfa_id():
    out = amexclient.classify_logon(
        *_logon(1, mfa_id="abc1234", error_code="LGON013"))
    assert out.needs_challenge and not out.authenticated
    assert out.mfa_id == "abc1234"
    assert out.error_code == "LGON013"


def test_challenge_is_detected_from_the_challenge_flag_alone():
    out = amexclient.classify_logon(*_logon(1, challenge=True))
    assert out.needs_challenge and not out.authenticated


def test_a_refusal_is_neither_authenticated_nor_a_challenge():
    # A bad credential: the caller surfaces the provider's own words rather
    # than reinterpreting them.
    http, body = _logon(1, error_code="LGON999")
    body["errorMessage"] = "We do not recognize that User ID."
    out = amexclient.classify_logon(http, body)
    assert not out.authenticated and not out.needs_challenge
    assert out.error_code == "LGON999"
    assert "User ID" in out.error_message


def test_a_non_200_is_never_authenticated():
    out = amexclient.classify_logon(*_logon(0, http=503))
    assert not out.authenticated


def test_classify_tolerates_a_junk_body():
    out = amexclient.classify_logon(200, None)   # type: ignore[arg-type]
    assert not out.authenticated and not out.needs_challenge


def test_device_remembered_is_not_read_as_trust():
    # reauth.deviceRemembered read false on a trusted AND an untrusted
    # login; reauth.trust is the field that reports it (DESIGN.md §G).
    _, body = _logon(0, trust=True)
    body["reauth"]["deviceRemembered"] = False
    assert amexclient.classify_logon(200, body).authenticated


# ============================================================
# Roster
# ============================================================

def _overview(*products):
    return {"products": {"data": {"products": {
        p["accountKey"]: p for p in products}}}}


def _card(key=KEY, token=TOKEN, product_type="AEXP_CARD_ACCOUNT"):
    return {
        "accountKey": key,
        "accountToken": token,
        "displayAccountNumber": "-01234",
        "productDisplayName": "Example Card",
        "productType": product_type,
        "subTypes": ["Example"],
        "lineOfBusiness": "CONSUMER",
        "userType": "ACCOUNT_HOLDER",
        "accountStatus": "Active",
        "isPartial": False,
        "balance": {"data": {"amount": "100.00", "currency": "USD",
                             "balanceName": "total_balance_title"}},
        "paymentDueDetails": {"data": {"paymentDueDate": "2026-04-01",
                                       "remainingDaysToPay": 12,
                                       "titleKey": "payment_due_title"}},
    }


def test_parse_accounts_keeps_both_identifiers():
    rows = amexclient.parse_accounts(_overview(_card()))
    assert len(rows) == 1
    row = rows[0]
    # The servicing REST API takes account_key; the BFF takes account_token.
    assert row["account_key"] == KEY
    assert row["account_token"] == TOKEN
    assert row["balance"]["amount"] == "100.00"
    assert row["payment_due"]["date"] == "2026-04-01"


def test_parse_accounts_drops_non_card_products():
    other = _card(key="F" * 32, token="ZZZZ1B2C3D4E5F6",
                  product_type="AEXP_SAVINGS_ACCOUNT")
    rows = amexclient.parse_accounts(_overview(_card(), other))
    assert [r["account_key"] for r in rows] == [KEY]


def test_parse_accounts_can_keep_everything():
    other = _card(key="F" * 32, token="ZZZZ1B2C3D4E5F6",
                  product_type="AEXP_SAVINGS_ACCOUNT")
    rows = amexclient.parse_accounts(_overview(_card(), other),
                                     cards_only=False)
    assert len(rows) == 2


def test_parse_accounts_falls_back_to_the_map_key():
    product = _card()
    del product["accountKey"]
    body = {"products": {"data": {"products": {KEY: product}}}}
    assert amexclient.parse_accounts(body)[0]["account_key"] == KEY


def test_parse_accounts_tolerates_a_missing_roster():
    assert amexclient.parse_accounts({}) == []
    assert amexclient.parse_accounts({"products": {"data": {}}}) == []


# ============================================================
# Activity projections
# ============================================================

def _activity(transactions, *, total=None, categories=None, balances=None):
    data: dict = {"data": [{"transactions": transactions}]}
    if total is not None:
        data["totalTransactionCount"] = total
    if categories is not None:
        data["categories"] = categories
    if balances is not None:
        data["balancesDetails"] = balances
    return {"activityData": data}


def _tx(identifier="100000000000000001", status="posted", category="C1"):
    return {
        "identifier": identifier,
        "referenceNumber": identifier,
        "categoryCode": category,
        "chargeDate": "2026-03-01",
        "postDate": "2026-03-02",
        "displayDescription": "EXAMPLE MERCHANT",
        "transactionAmount": {"amount": "12.34", "currency": "USD"},
        "type": "DEBIT",
        "status": status,
    }


def test_activity_transactions_flattens_every_block():
    body = {"activityData": {"data": [{"transactions": [_tx("1")]},
                                      {"transactions": [_tx("2")]}]}}
    assert [t["identifier"] for t in amexclient.activity_transactions(body)] \
        == ["1", "2"]


def test_activity_transactions_tolerates_junk_blocks():
    body = {"activityData": {"data": [None, {"transactions": None},
                                      {"transactions": [_tx("1")]}]}}
    assert len(amexclient.activity_transactions(body)) == 1


def test_activity_categories_merges_both_maps():
    body = _activity([], categories={"C1": "Groceries"})
    body["activityData"]["allFilters"] = {"categories": {"C2": "Travel"}}
    assert amexclient.activity_categories(body) == {"C1": "Groceries",
                                                    "C2": "Travel"}


def test_activity_total_and_balances_pass_through():
    body = _activity([_tx()], total=7,
                     balances={"summary": {"standard": {}}})
    assert amexclient.activity_total(body) == 7
    assert amexclient.activity_balances(body) == {"summary": {"standard": {}}}


def test_transaction_id_prefers_the_identifier():
    assert amexclient.transaction_id(_tx("42")) == "42"
    assert amexclient.transaction_id({"referenceNumber": "7"}) == "7"
    assert amexclient.transaction_id({}) == ""


# ============================================================
# Statements
# ============================================================

def _statements(recent=(), older=(), summaries=()):
    return {"billingStatements": {
        "recentStatements": list(recent),
        "olderStatements": list(older),
        "yearEndSummaries": list(summaries),
    }}


def _period(end, *, structured=True):
    opts = {"STATEMENT_PDF": f"/api/servicing/v1/documents/statements/T{end}"}
    if structured:
        opts["CSV"] = f"/api/servicing/v1/financials/documents?d={end}"
    return {"statementEndDate": end, "downloadOptions": opts}


def test_parse_statements_merges_both_groups_newest_first():
    body = _statements(recent=[_period("2026-03-12")],
                       older=[_period("2026-01-12"), _period("2026-02-12")])
    entries = amexclient.parse_statements(body)
    assert [e.end_date for e in entries] == ["2026-03-12", "2026-02-12",
                                             "2026-01-12"]


def test_statement_pdf_url_is_absolute():
    body = _statements(recent=[_period("2026-03-12")])
    entry = amexclient.parse_statements(body)[0]
    assert entry.pdf_url.startswith("https://global.americanexpress.com/")


def test_a_period_reports_whether_it_has_a_structured_export():
    # This is how the export seam is read from the data instead of a
    # hardcoded 24 months (DESIGN.md §E).
    body = _statements(recent=[_period("2026-03-12")],
                       older=[_period("2019-03-12", structured=False)])
    entries = {e.end_date: e for e in amexclient.parse_statements(body)}
    assert entries["2026-03-12"].has_structured_export
    assert not entries["2019-03-12"].has_structured_export


def test_parse_statements_skips_a_period_with_no_end_date():
    body = _statements(recent=[{"downloadOptions": {}}, _period("2026-03-12")])
    assert len(amexclient.parse_statements(body)) == 1


def test_parse_statements_tolerates_a_missing_block():
    assert amexclient.parse_statements({}) == []


def test_year_end_summaries_resolve_their_url():
    body = _statements(summaries=[{"year": 2025, "downloadOptions": {
        "YES_PDF": "/api/servicing/v2/financials/documents?year=2025"}}])
    rows = amexclient.parse_year_end_summaries(body)
    assert rows[0]["year"] == 2025
    assert rows[0]["pdf_url"].startswith("https://global.americanexpress.com/")


def test_year_end_summaries_skip_a_yearless_row():
    body = _statements(summaries=[{"downloadOptions": {"YES_PDF": "/x"}}])
    assert amexclient.parse_year_end_summaries(body) == []
