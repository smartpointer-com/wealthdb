"""Unit tests for george_client: the fragment-token parse, the endpoint
builders, the paging rules and the projections. Synthetic values only."""
from __future__ import annotations

import sys
from datetime import date, datetime, timezone
from pathlib import Path
from urllib.parse import parse_qs, urlsplit

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import george_client as george  # noqa: E402

ACCOUNT = "ACCT0000000000A1"
CARD = "CARD0000000000C1"
SHADOW = "SHDW0000000000S1"


def _ms(day: date) -> int:
    return int(datetime(day.year, day.month, day.day, 12,
                        tzinfo=timezone.utc).timestamp() * 1000)


# --- the sign-in ----------------------------------------------------------

def test_token_from_the_george_redirect():
    url = ("https://george.sparkasse.at/index.html#access_token=SYNTH-TOKEN"
           "&token_type=bearer&scope=x&expires_in=3599")
    assert george.token_from_url(url) == "SYNTH-TOKEN"


def test_token_ignored_off_george_or_not_bearer():
    assert george.token_from_url(
        "https://login.sparkasse.at/x#access_token=SYNTH") is None
    assert george.token_from_url(
        "https://george.sparkasse.at/#access_token=SYNTH&token_type=mac"
    ) is None
    assert george.token_from_url("https://george.sparkasse.at/#/overview") \
        is None
    assert george.token_from_url("") is None


def test_bearer_from_header():
    assert george.bearer_from_header("Bearer SYNTH") == "SYNTH"
    assert george.bearer_from_header("bearer SYNTH") == "SYNTH"
    assert george.bearer_from_header("Basic SYNTH") is None
    assert george.bearer_from_header("Bearer ") is None
    assert george.bearer_from_header(None) is None


def test_comparison_code_shape():
    assert george.looks_like_code("A1B2")
    assert george.looks_like_code(" 9Z9Z ")
    assert not george.looks_like_code("A1B")
    assert not george.looks_like_code("A1B2C")
    assert not george.looks_like_code("")


def test_secondfactor_status():
    assert george.secondfactor_status(
        {"secondFactorStatus": "PENDING", "pollingIntervalMs": 3000}) \
        == george.SECONDFACTOR_PENDING
    assert george.secondfactor_status({"secondFactorStatus": "DONE"}) \
        == george.SECONDFACTOR_DONE
    assert george.secondfactor_status({}) is None
    assert george.secondfactor_status(None) is None


# --- endpoints ------------------------------------------------------------

def test_roster_first_page_is_bare_and_later_pages_name_it():
    assert george.accounts_url() == \
        "https://api.sparkasse.at/rest/netbanking/my/accounts"
    assert george.accounts_url(2).endswith("/accounts?page=2")


def test_transactions_url_carries_the_product_and_page():
    url = george.transactions_url(f"{SHADOW}-{CARD}", 3)
    q = parse_qs(urlsplit(url).query)
    assert urlsplit(url).path == "/proxy/g/api/my/transactions"
    assert q["id"] == [f"{SHADOW}-{CARD}"]
    assert q["page"] == ["3"]
    assert q["pageSize"] == [str(george.HISTORY_PAGE_SIZE)]


def test_document_urls():
    s = george.statement_download_url(ACCOUNT, "100000000000000000001")
    q = parse_qs(urlsplit(s).query)
    assert urlsplit(s).path == (
        f"/rest/netbanking/my/accounts/{ACCOUNT}/statements/download")
    assert q == {"format": ["PDF_A4"],
                 "statementIds": ["100000000000000000001"],
                 "includeVoucher": ["false"]}
    assert george.invoice_pdf_url(CARD, "2000001").endswith(
        f"/rest/netbanking/my/cards/{CARD}/invoices/2000001/pdf")
    assert george.invoices_url(f"{SHADOW}-{CARD}").endswith(
        f"/accounts/{SHADOW}-{CARD}/invoices?numberOfMonths=36")


def test_ids_are_path_quoted():
    assert "/accounts/a%2Fb/statements" in george.statements_url("a/b")


def test_render_pending():
    body = {"errors": [{"scope": "ASYNCHRON", "error": "FIELD_INVALID"}]}
    assert george.is_render_pending(422, body)
    assert not george.is_render_pending(422, {"errors": [
        {"scope": "OTHER", "error": "FIELD_INVALID"}]})
    assert not george.is_render_pending(500, body)
    assert not george.is_render_pending(422, None)


# --- projections ----------------------------------------------------------

def test_paging_rules():
    body = {"pageNumber": 0, "pageCount": 2, "accounts": [{"id": ACCOUNT}]}
    assert george.roster_accounts(body) == [{"id": ACCOUNT}]
    assert george.page_has_more(body, 0)
    assert not george.page_has_more(body, 1)
    assert george.page_number_is(body, 0)
    assert not george.page_number_is(body, 1)
    assert george.history_has_more({"totalPages": 3}, 1)
    assert not george.history_has_more({"totalPages": 3}, 2)
    assert not george.history_has_more({}, 0)


def test_credit_cards_keep_credit_only_with_both_ids():
    body = {"cards": [
        {"id": "DEBIT00000000001", "type": "BANK_CARD"},
        {"id": CARD, "type": "CREDIT", "creditCardShadowAccountId": SHADOW},
        {"id": "CARD0000000000C2", "type": "CREDIT"},
    ]}
    assert george.credit_cards(body) == [{
        "card_id": CARD, "shadow_account_id": SHADOW,
        "product_id": f"{SHADOW}-{CARD}"}]
    assert george.credit_cards({}) == []


def test_oldest_booking_reads_epoch_ms():
    rows = [{"bookingDate": _ms(date(2026, 3, 2))},
            {"bookingDate": _ms(date(2026, 1, 15))},
            {"bookingDate": None}]
    assert george.oldest_booking(rows) == date(2026, 1, 15)
    assert george.oldest_booking([]) is None


def test_statement_window():
    since = date(2026, 1, 1)
    assert george.statement_in_window(
        {"maxBookingDate": "2026-01-31"}, since)
    assert not george.statement_in_window(
        {"maxBookingDate": "2025-12-31"}, since)
    assert george.statement_in_window(
        {"statementDate": "2026-02-01T00:00:00Z"}, since)
    assert george.statement_in_window({}, since)
    assert george.statement_in_window({"maxBookingDate": "2000-01-01"}, None)


def test_invoice_window_reads_epoch_ms():
    since = date(2026, 1, 1)
    assert george.invoice_in_window({"to": _ms(date(2026, 1, 20))}, since)
    assert not george.invoice_in_window({"to": _ms(date(2025, 12, 20))},
                                        since)
    assert george.invoice_rows([{"billId": "1"}, "x"]) == [{"billId": "1"}]
    assert george.invoice_rows({}) == []
