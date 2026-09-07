#!/usr/bin/env python3
"""American Express wire contract — the browserless core shared by
login.py and download.py.

The signed-in app is a React SPA on `global.americanexpress.com` over two
API surfaces (DESIGN.md §A):

* a **BFF of named "functions"** at `functions.americanexpress.com` — every
  call a `POST` with a JSON body, the endpoint named rather than a REST
  resource path (`ReadCustomerOverview.web.v2`, `ReadAccountActivity.web.v1`);
* a **servicing REST API** at `global.americanexpress.com/api/servicing/…`
  for exports and documents.

This module holds the parts that touch no browser and no network: endpoint
builders, the rule that reads the logon response as authenticated /
challenged / failed, the roster and activity projections, and the selectors
the login screens are driven by. Everything here is pure, so it is unit
tested against synthetic payloads.

The **logon itself is not here**. Akamai Bot Manager is cookie-borne and its
script runs in the page, so the sign-in must go through the real browser;
login.py drives that. Once signed in, no data endpoint carries a sensor
header — only the session cookie — so download.py replays the calls over
`page.request` (DESIGN.md §A).
"""
from __future__ import annotations

import uuid
from dataclasses import dataclass, field
from datetime import date
from urllib.parse import urlencode, urljoin

# --- origins and entry points --------------------------------------------

# The public site: the sign-in form lives here, in the main frame, behind a
# "Log In" link (DESIGN.md §B).
WWW_ORIGIN = "https://www.americanexpress.com"
START_URL = f"{WWW_ORIGIN}/"

# The signed-in SPA and the servicing REST API.
APP_ORIGIN = "https://global.americanexpress.com"

# The named-function BFF.
FUNCTIONS_ORIGIN = "https://functions.americanexpress.com"

# The legacy logon endpoint the sign-in form posts to. Its JSON response is
# the authoritative login outcome (see classify_logon).
LOGON_PATH = "/myca/logon/us/action/login"
LOGON_URL = f"{APP_ORIGIN}{LOGON_PATH}"

# --- sign-in and challenge selectors (pinned from the captures, §B) -------

# The homepage link that opens the sign-in form.
SEL_LOGIN_TRIGGER = "#gnav_login"
# Classic light DOM, main frame — no iframe. `SEL_USER` is the pinned record
# of the username field; nothing selects on it, because the pre-fill is
# explore's frame-aware one and matches on standard HTML conventions rather
# than on a single id, which survives a rename. It is kept because the next
# person debugging a drifted form wants the observed id, and DESIGN.md §B
# points here for it.
SEL_USER = "#eliloUserID"
SEL_PASSWORD = "#eliloPassword"
SEL_SUBMIT = "#loginSubmit"
# The form also carries a "Remember Me" checkbox (#rememberMe) — a stored
# preference for the user id, unrelated to device trust. It is deliberately
# NOT driven: toggling a preference is a write on a read-only surface, and
# what actually carries the trust is the post-challenge device registration
# (SEL_REGISTER_DEVICE below, DESIGN.md §G).

# The one-time-passcode challenge. Every control carries a data-testid.
SEL_CHALLENGE_LIST = "[data-testid='challenge-options-list']"
SEL_CHALLENGE_OPTION = f"{SEL_CHALLENGE_LIST} button"
SEL_CONTINUE = "[data-testid='continue-button']"

# The passcode is entered into SIX separate single-digit inputs, not one
# field (DESIGN.md §B). Each has maxlength=6 because the control accepts a
# paste into the first box and distributes it.
OTP_DIGITS = 6
SEL_OTP_INPUT = "[data-testid='otp-input-{i}']"

# A bot-defense challenge — a captcha standing in place of, or ahead of, the
# passcode screen (DESIGN.md §K). **Not pinned from a capture**: no explore
# session ever saw one, and the run that did was driven by hand over VNC with
# no DOM dump. So this is a deliberately broad net over the shapes a captcha
# widget takes — a vendor iframe, or an element naming itself — and it is used
# only to ABORT with the right instruction, never to drive anything. A false
# positive costs one run that says "use vnc-login" when the terminal would
# have done; a false negative costs the 30-second wait it costs today.
SEL_CAPTCHA = ", ".join((
    "iframe[src*='recaptcha']",
    "iframe[src*='hcaptcha']",
    "iframe[src*='captcha']",
    "iframe[title*='captcha' i]",
    "[id*='captcha' i]",
    "[class*='captcha' i]",
    "[data-testid*='captcha' i]",
))

# The device-registration step that plants the `device-id` cookie carrying
# the trust (DESIGN.md §G). It has no data-testid, so it is matched by its
# label — and the label is matched loosely, across several spellings and
# case-insensitively, because a single exact string missed it on the first
# live run (§M) and a miss costs a passcode on every subsequent run.
#
# Playwright's :has-text is substring and case-insensitive, so one entry
# covers "Add This Device" / "Add this device"; the alternatives cover the
# other wordings the same control ships under.
SEL_REGISTER_DEVICE = ", ".join((
    "button:has-text('Add This Device')",
    "button:has-text('Remember This Device')",
    "button:has-text('Remember Me')",
    "[data-testid*='register' i]",
    "[data-testid*='remember' i]",
))

# The persistent cookie that carries the device trust (~396-day Max-Age),
# beside session cookies that die with the browser (DESIGN.md §G). Named
# because `login --check` reports registration from the profile rather than
# spending a sign-in to ask.
DEVICE_TRUST_COOKIE = "device-id"


# --- the functions BFF ----------------------------------------------------

# Each function names the `ce-source` header the SPA sends with it; the BFF
# rejects a call whose source it does not recognise, so these are copied
# from the capture rather than invented.
FN_CUSTOMER_OVERVIEW = ("ReadCustomerOverview.web.v2", "WEB_OVERVIEW")
FN_ACCOUNT_ACTIVITY = ("ReadAccountActivity.web.v1", "WEB")


def function_url(name: str) -> str:
    return f"{FUNCTIONS_ORIGIN}/{name}"


def function_headers(ce_source: str | None) -> dict:
    """The headers a BFF call carries. Authentication is the cookie jar — no
    bearer token and no CSRF header exists anywhere in the capture
    (DESIGN.md §A) — so these are only the content negotiation, the SPA's
    origin, and the per-request correlation id the SPA generates."""
    headers = {
        "Accept": "application/json",
        "Content-Type": "application/json",
        "Origin": APP_ORIGIN,
        "one-data-correlation-id": str(uuid.uuid4()),
    }
    if ce_source:
        headers["ce-source"] = ce_source
    return headers


# --- activity views -------------------------------------------------------

# The `view` discriminator on ReadAccountActivity selects the window
# (DESIGN.md §D). DATE_RANGE is what `--lookback` rides — it expresses every
# window the others do — and STATEMENTS returns the statement archive
# instead of transactions. The rolling and billed views the SPA also uses
# are recorded in DESIGN.md §D rather than here; nothing needs them.
VIEW_DATE_RANGE = "DATE_RANGE"
VIEW_STATEMENTS = "STATEMENTS"

# Rows per activity request. 100 is what the SPA sends; the response carries
# `totalTransactionCount` so the walk knows when it is done.
ACTIVITY_PAGE_SIZE = 100

# The structured channels stop 24 months back (`member.maxAvailableMonths`),
# and a request for more is silently clamped by the server rather than
# refused — so the collector clamps deliberately and warns (DESIGN.md §E).
MAX_AVAILABLE_MONTHS = 24


def activity_body(account_token: str, *, view: str,
                  since: date | None = None, until: date | None = None,
                  limit: int = ACTIVITY_PAGE_SIZE,
                  offset: int = 1) -> dict:
    """The ReadAccountActivity request body for one window.

    `offset` is 1-based. Which unit it counts — rows or pages — is not
    settled by the captures (only offset=1 was ever sent), so download.py
    disambiguates it at runtime by checking whether a continuation actually
    returns new rows; see its `_paginate`."""
    body: dict = {
        "accountToken": account_token,
        "axplocale": "en-US",
        "view": view,
    }
    if view != VIEW_STATEMENTS:
        body["transactionFilters"] = {"limit": limit, "offset": offset}
    if view == VIEW_DATE_RANGE and since and until:
        body["dateRange"] = {"startDate": since.isoformat(),
                             "endDate": until.isoformat()}
    return body


# --- the servicing REST API ----------------------------------------------

# Export formats: the provider's `file_format` values, keyed by the fleet's
# short handles. There is no OFX 1.x — `quicken` and `quickbooks` are both
# OFX 2.02 XML and differ only in their header (DESIGN.md §E).
EXPORT_FORMATS = {
    "csv": "csv",
    "xls": "excel",
    "qfx": "quicken",
    "qbo": "quickbooks",
}
# The extension each format actually arrives as.
EXPORT_EXTENSIONS = {"csv": "csv", "xls": "xlsx", "qfx": "qfx", "qbo": "qbo"}
# CSV carries the merchant category and the address columns; QFX carries the
# OFX field split. Silver reads neither: the activity JSON is the ledger of
# record (DESIGN.md §D) and the exports are archived to bronze as-is, for
# provenance and for the columns the JSON lacks.
DEFAULT_EXPORT_FORMATS = ("csv", "qfx")

DOCUMENTS_PATH = "/api/servicing/v1/financials/documents"


def export_url(account_key: str, fmt_handle: str, *,
               since: date | None = None, until: date | None = None) -> str:
    """A transaction export for `account_key` over an optional date window.

    Constructed rather than taken from the payload's `downloadOptions`: every
    parameter is known (DESIGN.md §E) and a constructed URL carries the
    window the run actually asked for. The per-cycle and statement URLs are
    the opposite case — they carry opaque tokens — and are used verbatim.

    `additional_fields` is what widens the CSV to the extended-details
    columns; `status=posted` mirrors the UI, which never exports a pending
    row. Raises KeyError on an unknown handle."""
    params = [
        ("account_key", account_key),
        ("client_id", "AmexAPI"),
        ("file_format", EXPORT_FORMATS[fmt_handle]),
        ("limit", "ALL"),
        ("status", "posted"),
        ("additional_fields", "true"),
    ]
    if since and until:
        params += [("start_date", since.isoformat()),
                   ("end_date", until.isoformat())]
    return f"{APP_ORIGIN}{DOCUMENTS_PATH}?{urlencode(params)}"


def absolute_url(url: str) -> str:
    """Resolve a `downloadOptions` URL against the app origin. The payload
    gives them site-relative; a value that is already absolute is returned
    unchanged.

    An ABSENT url stays absent. Resolving "" would yield the app origin — a
    perfectly fetchable page — so a period offering no such document would
    quietly store the SPA's own HTML under a `.pdf` name and count it."""
    if not url:
        return ""
    return urljoin(APP_ORIGIN + "/", url)


# --- logon classification -------------------------------------------------

@dataclass(frozen=True)
class LogonOutcome:
    """The logon response reduced to a decision (DESIGN.md §B, §G).

    * `authenticated` — `statusCode == 0` and no challenge: signed in. On a
      trusted device this is the first response; after a passcode it is the
      second.
    * `needs_challenge` — the response carries an `mfaId` or sets
      `challenge`: a one-time passcode is required.
    * neither — a real failure (bad credentials, a locked account). The
      provider's own `errorCode` / `errorMessage` ride along so the caller
      can surface them verbatim rather than guessing at them.

    `trust` echoes `reauth.trust`, the field that actually reports device
    trust; `reauth.deviceRemembered` does NOT (it read false on a trusted
    and an untrusted login alike, DESIGN.md §G)."""
    authenticated: bool
    needs_challenge: bool
    status: int = 0
    status_code: int | None = None
    error_code: str = ""
    error_message: str = ""
    mfa_id: str = ""
    trust: bool = False


def classify_logon(status: int, body: dict) -> LogonOutcome:
    """Read a logon response into a LogonOutcome. Never raises on shape: an
    unparseable body is 'neither', which the caller surfaces as a failure."""
    body = body if isinstance(body, dict) else {}
    reauth = body.get("reauth")
    reauth = reauth if isinstance(reauth, dict) else {}
    status_code = body.get("statusCode")
    mfa_id = str(reauth.get("mfaId") or "")
    challenge = bool(body.get("challenge")) or bool(mfa_id)
    authed = (status == 200 and status_code == 0 and not challenge)
    return LogonOutcome(
        authenticated=authed,
        needs_challenge=bool(challenge) and not authed,
        status=status,
        status_code=status_code if isinstance(status_code, int) else None,
        error_code=str(body.get("errorCode") or ""),
        error_message=str(body.get("errorMessage") or ""),
        mfa_id=mfa_id,
        trust=bool(reauth.get("trust")),
    )


@dataclass(frozen=True)
class ChallengeTarget:
    """One passcode destination, read off the on-screen option buttons.

    `value` is the button's DOM index (login.py clicks it by index),
    `display` the label the human sees — which already carries the masked
    destination — and `kind` a coarse sms/email/other read from that label.
    The delivery id itself never reaches this side: the page sends an opaque
    `encryptedValue` (DESIGN.md §B)."""
    value: str
    display: str
    kind: str


def target_kind(label: str) -> str:
    """Classify a delivery option from its own label. Coarse on purpose —
    the label is what the human reads, and the kind only picks a fallback
    wording when a label is empty."""
    low = (label or "").lower()
    if "text" in low or "sms" in low or "message" in low:
        return "sms"
    if "email" in low or "@" in low:
        return "email"
    if "call" in low or "phone" in low:
        return "voice"
    return "other"


# --- roster ---------------------------------------------------------------

# The roster's own marker for a card account. Anything else the login may
# hold is out of scope (CLAUDE.md).
CARD_PRODUCT_TYPE = "AEXP_CARD_ACCOUNT"


def is_card_account(product: dict) -> bool:
    return str(product.get("productType", "")).upper() == CARD_PRODUCT_TYPE


def parse_accounts(body: dict, *, cards_only: bool = True) -> list[dict]:
    """Project a ReadCustomerOverview response to the fields download.py and
    the loader need.

    The roster arrives as a map keyed by `accountKey` under
    `products.data.products` (DESIGN.md §C). Both identifiers are kept: the
    servicing REST API takes `accountKey`, the BFF takes `accountToken`.
    Pure — tested on synthetic payloads."""
    products = (((body or {}).get("products") or {}).get("data") or {}
                ).get("products")
    if not isinstance(products, dict):
        return []
    rows = []
    for key, product in products.items():
        if not isinstance(product, dict):
            continue
        if cards_only and not is_card_account(product):
            continue
        balance = ((product.get("balance") or {}).get("data") or {})
        due = ((product.get("paymentDueDetails") or {}).get("data") or {})
        rows.append({
            "account_key": str(product.get("accountKey") or key or ""),
            "account_token": str(product.get("accountToken") or ""),
            "display_account_number": str(
                product.get("displayAccountNumber") or ""),
            "product_display_name": str(
                product.get("productDisplayName") or ""),
            "product_type": str(product.get("productType") or ""),
            "sub_types": [str(s) for s in (product.get("subTypes") or [])],
            "line_of_business": str(product.get("lineOfBusiness") or ""),
            "user_type": str(product.get("userType") or ""),
            "account_status": str(product.get("accountStatus") or ""),
            "is_partial": bool(product.get("isPartial")),
            "balance": {
                "amount": str(balance.get("amount") or ""),
                "currency": str(balance.get("currency") or ""),
                "name": str(balance.get("balanceName") or ""),
            },
            "payment_due": {
                "date": str(due.get("paymentDueDate") or ""),
                "remaining_days": due.get("remainingDaysToPay"),
                "title_key": str(due.get("titleKey") or ""),
            },
        })
    rows.sort(key=lambda r: r["account_key"])
    return rows


# --- activity -------------------------------------------------------------

def activity_data(body: dict) -> dict:
    d = (body or {}).get("activityData")
    return d if isinstance(d, dict) else {}


def activity_transactions(body: dict) -> list:
    """Every transaction across the response's `data` blocks. The payload
    groups rows into blocks (one per billing cycle in the billed views), and
    the ledger wants them flat."""
    out = []
    for block in activity_data(body).get("data") or []:
        if isinstance(block, dict):
            txs = block.get("transactions")
            if isinstance(txs, list):
                out.extend(t for t in txs if isinstance(t, dict))
    return out


def activity_total(body: dict) -> int | None:
    total = activity_data(body).get("totalTransactionCount")
    return total if isinstance(total, int) else None


def activity_categories(body: dict) -> dict:
    """The response's own `categoryCode` → label map (DESIGN.md §F).

    It ships beside the rows, so a category the provider adds later resolves
    on its own rather than becoming an unmapped code — which is why the
    collector carries no hardcoded category table. The map lists only the
    categories present in the rows returned, so it is merged across
    requests, never treated as the full vocabulary."""
    data = activity_data(body)
    merged = {}
    for src in (data.get("categories"),
                (data.get("allFilters") or {}).get("categories")):
        if isinstance(src, dict):
            merged.update({str(k): str(v) for k, v in src.items()})
    return merged


def activity_balances(body: dict) -> dict:
    """The cycle's balance block — the same reconciliation identity a
    statement prints. Which figures it carries is view-dependent (§H):
    a billed view states `statementBalance`, a rolling one `totalBalance`
    plus `pendingBalances`. Passed through as-is for the loader."""
    bd = activity_data(body).get("balancesDetails")
    return bd if isinstance(bd, dict) else {}


def transaction_id(tx: dict) -> str:
    """A row's stable id. Posted rows carry an 18-digit `identifier` that is
    also the QFX FITID; a pending row's is provisional and changes when the
    charge posts (DESIGN.md §D), which the loader's `is_pending` column
    marks."""
    return str(tx.get("identifier") or tx.get("referenceNumber") or "")


# --- statements -----------------------------------------------------------

@dataclass(frozen=True)
class StatementEntry:
    """One period in the statement archive. `options` is the period's own
    `downloadOptions` map — which channels exist for it, and the opaque URLs
    to fetch them. A period older than the structured-export horizon offers
    `STATEMENT_PDF` alone, which is how discovery measured the reach split
    (DESIGN.md §E). Nothing reads it at runtime to find that split: the
    statement fetch is the one channel deliberately NOT bounded by
    `MAX_AVAILABLE_MONTHS` — it takes the window as asked for, which is what
    lets the backfill reach the deep archive — and the import is seamed on
    the activity rows a load actually landed."""
    end_date: str
    options: dict = field(default_factory=dict)

    @property
    def pdf_url(self) -> str:
        return absolute_url(self.options.get("STATEMENT_PDF", ""))

    @property
    def has_structured_export(self) -> bool:
        return any(k in self.options for k in ("CSV", "EXCEL", "QUICKEN",
                                               "QUICKBOOKS"))


def parse_statements(body: dict) -> list[StatementEntry]:
    """Project a STATEMENTS-view response to its periods, newest first.

    `recentStatements` and `olderStatements` differ only in how the UI
    groups them, so they are merged and sorted newest-first."""
    bs = (body or {}).get("billingStatements")
    if not isinstance(bs, dict):
        return []
    out = []
    for key in ("recentStatements", "olderStatements"):
        for row in bs.get(key) or []:
            if not isinstance(row, dict):
                continue
            end = str(row.get("statementEndDate") or "")
            if not end:
                continue
            opts = row.get("downloadOptions")
            out.append(StatementEntry(
                end_date=end,
                options=opts if isinstance(opts, dict) else {}))
    out.sort(key=lambda e: e.end_date, reverse=True)
    return out


def parse_year_end_summaries(body: dict) -> list[dict]:
    """The per-year summary documents a STATEMENTS response also carries."""
    bs = (body or {}).get("billingStatements")
    if not isinstance(bs, dict):
        return []
    out = []
    for row in bs.get("yearEndSummaries") or []:
        if not isinstance(row, dict) or row.get("year") is None:
            continue
        opts = row.get("downloadOptions")
        url = (opts or {}).get("YES_PDF", "") if isinstance(opts, dict) else ""
        out.append({"year": row["year"], "pdf_url": absolute_url(url)})
    return out
