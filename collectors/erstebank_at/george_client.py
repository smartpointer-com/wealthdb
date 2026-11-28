#!/usr/bin/env python3
"""George wire contract — the browserless core shared by login.py and
download.py.

George is an SPA over a REST/JSON data API (DESIGN.md §3-Surface). Three
hosts are in play:

  * `login.sparkasse.at` — the OAuth implicit-grant sign-in (§A): the
    user-name form, the push wait screen and its status poll (§B), and the
    redirect that hands the bearer to the SPA in the URL fragment.
  * `george.sparkasse.at` — the SPA itself.
  * `api.sparkasse.at` — the data API (§C, §E, §G). download.py calls it
    over the browser context's `request` API, which shares the cookie jar,
    with the bearer added to each call.

Everything here touches no browser and no network: endpoint builders, the
fragment-token parse, the paging rules and the response projections. All
pure, so it is unit-tested against synthetic payloads.
"""
from __future__ import annotations

import re
from datetime import date, datetime
from urllib.parse import parse_qsl, quote, urlencode, urlsplit
from zoneinfo import ZoneInfo

# --- origins --------------------------------------------------------------

START_URL = "https://george.sparkasse.at/"
GEORGE_HOST = "george.sparkasse.at"
LOGIN_HOST = "login.sparkasse.at"
API_ORIGIN = "https://api.sparkasse.at"

NETBANKING = f"{API_ORIGIN}/rest/netbanking/my"
G_API = f"{API_ORIGIN}/proxy/g/api/my"

# --- the sign-in page (§A, §B) --------------------------------------------

SEL_USER = "form#credentials input#user"
SEL_SUBMIT = "form#credentials #submitButton"
SEL_COOKIE_CLOSE = "#cookiebutton"           # a notice's "Close", no consent
SEL_VERIFICATION_CODE = "b#verificationCode"
SEL_ERRORS = ("#errorContainer", "#error")

# The push poll the wait screen runs on its own (§B).
SECONDFACTOR_PATH = "/sts/secapp/secondfactor"
SECONDFACTOR_PENDING = "PENDING"
SECONDFACTOR_DONE = "DONE"

_CODE_RE = re.compile(r"^[A-Za-z0-9]{4}$")


def looks_like_code(text: str) -> bool:
    """True for the 4-character comparison code the wait screen shows."""
    return bool(_CODE_RE.match((text or "").strip()))


def secondfactor_status(body: object) -> str | None:
    """`secondFactorStatus` of a push-poll answer, or None."""
    if isinstance(body, dict):
        status = body.get("secondFactorStatus")
        if isinstance(status, str):
            return status
    return None


def token_from_url(url: str) -> str | None:
    """The bearer from the redirect that ends the sign-in:
    `https://george.sparkasse.at/index.html#access_token=…&token_type=…`.
    Only a George URL counts, and only a bearer-typed token, so a stray
    fragment elsewhere is never mistaken for one."""
    parts = urlsplit(url or "")
    if parts.hostname != GEORGE_HOST or not parts.fragment:
        return None
    params = dict(parse_qsl(parts.fragment.lstrip("/")))
    token = params.get("access_token")
    if not token:
        return None
    token_type = params.get("token_type", "bearer").lower()
    return token if token_type == "bearer" else None


def bearer_from_header(value: str | None) -> str | None:
    """The token of an `Authorization: Bearer …` header, or None."""
    if value and value.lower().startswith("bearer "):
        token = value.split(" ", 1)[1].strip()
        return token or None
    return None


# --- data endpoints (§C, §E, §G) ------------------------------------------

def _q(value: str) -> str:
    return quote(str(value), safe="")


def accounts_url(page: int = 0) -> str:
    """The checking roster, paged (`pageNumber` / `pageCount`). The first
    page is asked for bare, as the SPA does; a later one by `page`, and the
    caller checks the answer's `pageNumber` (`page_number_is`)."""
    if page == 0:
        return f"{NETBANKING}/accounts"
    return f"{NETBANKING}/accounts?{urlencode({'page': page})}"


def cards_url() -> str:
    """The card roster. The query is the SPA's own."""
    return f"{NETBANKING}/cards?platformKey=IOS-640&featureSet=ALL"


HISTORY_PAGE_SIZE = 50                       # the SPA's own page size


def transactions_url(product_id: str, page: int,
                     page_size: int = HISTORY_PAGE_SIZE) -> str:
    """One page of a product's history, newest first. `product_id` is a
    checking account's id, or `<shadowAccountId>-<cardId>` for a credit card
    (`card_product_id`)."""
    return f"{G_API}/transactions?" + urlencode({
        "id": product_id, "page": page, "pageSize": page_size,
        "includeContainedTransactions": "false",
        "continuousCardIdFiltering": "false",
    })


STATEMENT_PAGE_SIZE = 60                     # the SPA's own page size


def statements_url(account_id: str, page: int = 0,
                   size: int = STATEMENT_PAGE_SIZE) -> str:
    return (f"{NETBANKING}/accounts/{_q(account_id)}/statements?"
            + urlencode({"page": page, "size": size,
                         "sort": "statementDate", "order": "DESC"}))


STATEMENT_FORMAT = "PDF_A4"


def statement_download_url(account_id: str, statement_id: str) -> str:
    """The statement PDF. A POST whose form body carries only the bearer."""
    return (f"{NETBANKING}/accounts/{_q(account_id)}/statements/download?"
            + urlencode({"format": STATEMENT_FORMAT,
                         "statementIds": statement_id,
                         "includeVoucher": "false"}))


INVOICE_MONTHS = 36                          # the SPA's own reach


def invoices_url(card_product: str, months: int = INVOICE_MONTHS) -> str:
    return (f"{G_API}/accounts/{_q(card_product)}/invoices?"
            + urlencode({"numberOfMonths": months}))


def invoice_pdf_url(card_id: str, bill_id: str) -> str:
    """The card statement PDF. A POST whose form body carries only the
    bearer."""
    return f"{NETBANKING}/cards/{_q(card_id)}/invoices/{_q(bill_id)}/pdf"


def is_render_pending(status: int, body: object) -> bool:
    """True for the answer a statement download gives while the server
    still renders the file (§E): `422` with an `ASYNCHRON` error."""
    if status != 422 or not isinstance(body, dict):
        return False
    return any(isinstance(e, dict) and e.get("scope") == "ASYNCHRON"
               for e in body.get("errors") or [])


# --- projections ----------------------------------------------------------

def roster_accounts(body: object) -> list[dict]:
    """The `accounts[]` of one roster page."""
    if isinstance(body, dict) and isinstance(body.get("accounts"), list):
        return [a for a in body["accounts"] if isinstance(a, dict)]
    return []


def page_has_more(body: object, page: int) -> bool:
    """True while a later page exists, for the netbanking listings that
    page by `pageCount` (the roster, the statement archive)."""
    if not isinstance(body, dict):
        return False
    count = body.get("pageCount")
    return isinstance(count, int) and page + 1 < count


def page_number_is(body: object, page: int) -> bool:
    """True when a paged answer is the page asked for. A server that
    ignored the page parameter answers page 0 again."""
    return isinstance(body, dict) and body.get("pageNumber") == page


def credit_cards(body: object) -> list[dict]:
    """The credit cards of the card roster, each with the two ids that key
    its data. A debit card is left out: it has no account of its own, and
    its payments are in its checking account's history (§G)."""
    cards = body.get("cards") if isinstance(body, dict) else None
    out = []
    for card in cards or []:
        if not isinstance(card, dict) or card.get("type") != "CREDIT":
            continue
        card_id = card.get("id")
        shadow = card.get("creditCardShadowAccountId")
        if card_id and shadow:
            out.append({"card_id": str(card_id),
                        "shadow_account_id": str(shadow),
                        "product_id": card_product_id(shadow, card_id)})
    return out


def card_product_id(shadow_account_id: str, card_id: str) -> str:
    """The id a credit card's history and invoices are keyed by (§G)."""
    return f"{shadow_account_id}-{card_id}"


def history_rows(body: object) -> list[dict]:
    if isinstance(body, dict) and isinstance(body.get("collection"), list):
        return [r for r in body["collection"] if isinstance(r, dict)]
    return []


def history_has_more(body: object, page: int) -> bool:
    """True while a later history page exists (`totalPages`)."""
    if not isinstance(body, dict):
        return False
    total = body.get("totalPages")
    return isinstance(total, int) and page + 1 < total


# George's dates are local midnights in Vienna, sent as epoch milliseconds
# (22:00 or 23:00 UTC the day before). Read in UTC, every date would land a
# day early.
TZ = ZoneInfo("Europe/Vienna")


def epoch_ms_date(value: object) -> date | None:
    """A George epoch-milliseconds date as the calendar day it names."""
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        return None
    return datetime.fromtimestamp(value / 1000, tz=TZ).date()


def oldest_booking(rows: list[dict]) -> date | None:
    dates = [d for d in (epoch_ms_date(r.get("bookingDate")) for r in rows)
             if d is not None]
    return min(dates) if dates else None


def statement_rows(body: object) -> list[dict]:
    if isinstance(body, dict) and isinstance(body.get("statements"), list):
        return [s for s in body["statements"] if isinstance(s, dict)]
    return []


def iso_day(value: object) -> date | None:
    """A date from an ISO string (`2026-01-31` or with a time part) or an
    epoch-milliseconds number."""
    if isinstance(value, str) and len(value) >= 10:
        try:
            return date.fromisoformat(value[:10])
        except ValueError:
            return None
    return epoch_ms_date(value)


def statement_in_window(row: dict, since: date | None) -> bool:
    """True when a statement reaches into the window: its last booking day
    (or, lacking one, its statement date) is on or after `since`."""
    if since is None:
        return True
    day = iso_day(row.get("maxBookingDate")) or iso_day(
        row.get("statementDate"))
    return day is None or day >= since


def invoice_rows(body: object) -> list[dict]:
    if isinstance(body, list):
        return [r for r in body if isinstance(r, dict)]
    return []


def invoice_in_window(row: dict, since: date | None) -> bool:
    """True when a card statement's period ends on or after `since`."""
    if since is None:
        return True
    day = iso_day(row.get("to")) or iso_day(row.get("billingDate"))
    return day is None or day >= since
