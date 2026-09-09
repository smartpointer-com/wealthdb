#!/usr/bin/env python3
"""chase bronze scrape — the `walk()` half of the one-shot download.

`login.py` establishes an authenticated Camoufox `page` (Chase tears the
session down when Firefox closes, so login + scrape happen in one browser
lifetime — the schwab-web model) and hands it here. `walk()` captures, into
a UTC-stamped bronze run dir:

    <run>/
      run.json                      status manifest + per-product coverage
      accounts.json                 roster + per-product detail, from the /svc/ calls
      raw/*.json                    raw /svc/ response bodies (provenance)
      transactions/<ext_id>.csv     activity export — per-row running balance
      transactions/<ext_id>.qfx     activity export — stable FITID (DESIGN.md §E)
      statements/<ext_id>/*.pdf     statement PDFs (skipped with --no-documents)

Both account kinds are walked: the deposit accounts (checking / savings) and
the credit cards, which the roster discriminates by `summaryType` and every
record carries as `product` ('dda' | 'card'). Card **reads** came into scope
with the 2026-09-04 amendment (DESIGN.md §4); card *management* stays
forbidden.

The scrape drives the account UI the way the discovery sessions did
(DESIGN.md §C–§E): the browser holds the session cookie and sets the
`x-jpmc-*` / CSRF headers, so exports are triggered through the UI rather
than by replaying the raw `/svc/` request. That matters more for cards: their
export is a GET whose query string carries the CSRF token, so replaying it
out-of-band would mean lifting the token out of the session. The account
roster is read from the `account/activity/download/options/list` response the
download page fetches, and card detail from the dashboard envelope
(`parse_card_overview`).

**Per-product completeness.** run.json's `coverage` block scores each product
on its own — accounts discovered, accounts exported, statement passes
completed, and a `complete` flag — while `status` says only that the walk
finished. `load` skips a non-complete run WHOLESALE, so folding a card
shortfall into the status would cost that night's *deposit* ledger its load
and hand it to `prune`. Nothing gates on the coverage block: it records which
product fell short, for the run log and for anyone reading run.json.

The selectors and routes are validated live for the deposit path (DESIGN.md
§E/§C) and taken from the card exploration capture for the card path. The
browserless helpers (roster reducers, run layout, the export window, filename
sanitising, the manifest) are unit-tested, and so are the browser-side steps
whose failure would be silent rather than loud: the export form's account
guard and the statement pass's failure accounting, against a stub page.

Read-only: this only navigates, filters, and exports. Per CLAUDE.md it never
touches Pay & transfer / Zelle / settings, nor any card *management* surface
(payments, autopay, limits, disputes, lock/unlock).
"""
from __future__ import annotations

import contextlib
import logging
import re
import time
from datetime import date
from pathlib import Path

import mdsui
from collectorkit import bronze

log = logging.getLogger("chase.download")

# Authenticated SPA host + hash routes (DESIGN.md §B–§E). {ext} is the
# account's digital-account-identifier.
BASE = "https://secure.chase.com"
ROUTE_OVERVIEW = f"{BASE}/web/auth/dashboard#/dashboard/overview"
# The trailing `/{summary}/{detail}` is the product tuple, not a constant:
# deposits route as DDA/CHK, cards as CARD/BAC.
ROUTE_TX_DOWNLOAD = (f"{BASE}/web/auth/dashboard"
                     "#/dashboard/transactions/downloads/{ext}/{summary}/{detail}")
ROUTE_CARD_SUMMARY = f"{BASE}/web/auth/dashboard#/dashboard/summary/{{ext}}/CARD/BAC"
# The relationship-wide documents centre (deposit statements) and its
# account-scoped form — card statements are per card, never combined, so each
# card is paged on its own accountId.
ROUTE_DOCUMENTS = f"{BASE}/web/auth/dashboard#/dashboard/documents/myDocs/index;documentType=STATEMENTS"
ROUTE_ACCOUNT_DOCUMENTS = (f"{BASE}/web/auth/dashboard#/dashboard/documents/myDocs/index"
                           ";accountId={ext};documentType=STATEMENTS;mode=accounts")

# Roster `summaryType` → the `product` discriminator stamped on every record.
# Anything else is not a fundable account and is dropped: the documents menu
# also carries entries that are not accounts at all (an unknown summaryType,
# 'UKN', on a non-account type such as 'ATM').
PRODUCTS = {"DDA": "dda", "CARD": "card"}
# `product` → the export route's (summaryType, detailType) tuple. Savings
# accounts route through DDA/CHK like checking (live-validated).
PRODUCT_ROUTE = {"dda": ("DDA", "CHK"), "card": ("CARD", "BAC")}
# `detail.detailType` → product, for the per-account detail responses. BOTH
# sides are an explicit set: an error or partial body that carries an accountId
# but no usable detail must reduce to nothing, never fall through to the
# deposit shape — a card reduced as a deposit loads a debt as a cash asset.
DETAIL_PRODUCTS = {"CHK": "dda", "SAV": "dda", "BAC": "card"}

# The download form's MDS controls, from the explore DOM capture (DESIGN.md
# §E). Only CSV + QFX are fetched: CSV alone has the running balance, QFX
# alone has the FITID; QBO duplicates QFX and QIF is poorest. Each format is
# an option under the file-select whose data-testid begins with this prefix.
EXPORT_FORMATS = {
    "csv": "showing-file-select-spreadsheet",
    "qfx": "showing-file-select-quicken web connect",
}
# Each option's visible label — the accessible name for the get_by_role("option")
# path (the proven MDS click), keyed the same as EXPORT_FORMATS.
EXPORT_FORMAT_LABELS = {
    "csv": "Spreadsheet (Excel, CSV)",
    "qfx": "Quicken Web Connect (QFX)",
}
SEL_FILE_SELECT = "#showing-file-select-selector-no-label"
SEL_ACTIVITY_SELECT = "#showing-activity-select-selector-no-label"
# The export form also carries an account picker that switches the exported
# account WITHOUT a route change. It is read to confirm which account the form
# is bound to (its `value` is the account's external id) and, when the hash-only
# route change left the form bound to the previous account, to correct it
# without paying for a reload.
SEL_ACCOUNT_SELECT = "#account_options_id-selector-no-label"
OPT_ACCOUNT = "#account_options_id-{ext}"
# A second variant of the same picker: same `data-testid`, but its options are
# keyed by POSITIONAL INDEX rather than by account id, so neither its `value`
# nor its option ids say which account the form is bound to. It belongs to a
# different surface in the captures, and it may never render on the export
# form — but if it does, the binding is unknowable and the export must refuse
# rather than read the exact-id picker's absence as "no picker at all".
SEL_ACCOUNT_SELECT_INDEXED = "#account_options_id-selector-label"
# How long to poll for the picker before concluding the form has none. A
# hash-only route change re-renders the form asynchronously while the previous
# account's controls are still mounted, so an immediate read sees no picker on
# a page that is about to grow one.
PICKER_WAIT_SECS = 8
# The picker's three states (`_export_picker_state`). Only ABSENT may proceed:
# "no picker, so the route decides" and "a picker whose binding can't be read"
# are the difference between an export filed under the right account and one
# filed under another account's id.
PICKER_BOUND = "bound"
PICKER_ABSENT = "absent"
PICKER_UNREADABLE = "unreadable"
# "All transactions" = Chase's widest export window for this control (last
# 24 months); its option data-testid begins with this, label as shown.
OPT_ACTIVITY_ALL = "showing-activity-select-all transactions"
OPT_ACTIVITY_ALL_LABEL = "All transactions"
# The download-transactions page's trigger is <button id="downloadButton">
# (data-testid "downloadButton"); it is enabled with the default selection.
SEL_DOWNLOAD_BUTTON = "#downloadButton"

# The /svc/ endpoints that carry roster or account detail (DESIGN.md §B/§E).
# The overview fires account/detail/dda/list per deposit account; the download
# page fires account/activity/download/{options,dda}/list; the documents centre
# fires the documents menu. Captured by URL so the roster survives a response
# body shape that the key heuristics don't predict.
#
# `dashboard/module/list` is an ENVELOPE: the card roster
# (overview/card/v2/list) and card detail (account/detail/card/list) are only
# ever delivered nested under its `cache[].response`, never as standalone
# requests, so a URL-keyed hook that didn't capture the envelope would never
# see them. `expand_module_cache` unwraps it; the two inner URLs are matched
# here too so a build that does put them on the wire is picked up unchanged.
ROSTER_URL_RE = re.compile(
    r"/account/detail/(?:dda|card)/list"
    r"|/account/activity/download/(?:options|dda)/list"
    r"|/overview/card/v2/list"
    r"|/dashboard/module/list"
    r"|/documents/secure/v1/menu/list")
# The envelope's inner URLs worth unwrapping — everything else it caches
# (offers, tiles, credit-journey status) is noise.
CACHED_ROSTER_URL_RE = re.compile(
    r"/account/detail/card/list|/overview/card/v2/list")

# Statements & documents centre (DESIGN.md §C). Classic (non-MDS) light-DOM:
# a per-doc-type accordion over a plain table and a year filter. All ids are
# stable and light-DOM, so plain clicks work (no shadow piercing). {n} is the
# 0-based row index.
SEL_STATEMENTS_ACCORDION = "#button-accountsAccordian-STATEMENTS"
STMT_DATE_CELL = "#accountsTable-STATEMENTS-row{n}-cell0"
# The row's save control, newest shape first: a direct "Saves document" anchor
# whose clickable target is the icon inside it.
STMT_DOWNLOAD_ANCHOR = ("#icon-accountsTable-STATEMENTS-row{n}-cell3"
                        "-requestThisDocumentLink-download")
# The legacy shape: a per-row dropdown, then a "Save as PDF" item. It appears
# in NONE of the card-exploration DOM snapshots — but that session never opened
# the deposit documents view, so whether the anchor is a card-only shape or a
# global change that already replaced the dropdown everywhere is unverified.
# Neither may be assumed, so both are coded and the anchor is preferred.
STMT_DOWNLOAD_TRIGGER = ("#header-accountsTable-STATEMENTS-row{n}-cell3"
                         "-downloadDocumentDropdown")
STMT_PDF_OPTION = "#item-0-{n}-downloadPDFOption"


# ============================================================
# Browserless helpers (unit-tested)
# ============================================================

def safe_stem(ext_id: str) -> str:
    """A filesystem-safe stem for an account's export files. The external id
    is Chase-generated but treated as untrusted: keep word chars, collapse
    the rest to '_'."""
    stem = re.sub(r"[^0-9A-Za-z._-]+", "_", str(ext_id)).strip("_")
    return stem or "account"


def parse_download_options(body: dict) -> list[dict]:
    """Reduce a `download/options/list` response (the all-accounts download
    roster) to roster records, stamping each with its `product` — 'dda' for
    the deposit accounts, 'card' for the credit cards. A row whose
    `summaryType` is neither is not a fundable account and is dropped.
    Never records the full number; keeps Chase's own last-4 `mask`."""
    if not isinstance(body, dict):
        return []
    out = []
    for opt in (body.get("downloadAccountActivityOptions") or []):
        if not isinstance(opt, dict):
            continue
        product = PRODUCTS.get(str(opt.get("summaryType", "")).upper())
        if product is None:
            continue
        out.append({
            "account_external_id": str(opt.get("accountId", "")),
            "account_type": opt.get("detailType"),      # 'CHK', 'SAV', 'BAC'
            "nickname": opt.get("nickName"),
            "mask": _mask(opt.get("mask")),
            "currency": "USD",
            "product": product,
        })
    return out


def parse_account_detail(body: dict) -> list[dict]:
    """Reduce an `account/detail/{dda,card}/list` response — one account — to a
    roster record. The two share a top-level shape (accountId + nickname +
    mask + detail) but nothing below it: a card carries none of the deposit
    balance keys, so it takes the card branch instead of silently reducing to
    `balance: None` and losing the liability.

    Both branches are chosen POSITIVELY, off `detail.detailType`. A body that
    claims neither shape — an error payload or a partial render that still
    carries an accountId — yields nothing at all: reducing it as a deposit
    would stamp `product: 'dda'` on what may be a card, and a card loaded as a
    deposit is a debt carried as an asset. The account is not lost by this: the
    download-options roster rosters it from `summaryType`, and only the balance
    this body would have carried is missing. Returns 0 or 1 record."""
    if not isinstance(body, dict) or "accountId" not in body:
        return []
    detail = body.get("detail") or {}
    detail_type = str(detail.get("detailType", "")).upper()
    product = DETAIL_PRODUCTS.get(detail_type)
    if product is None:
        log.debug("account detail with unmapped detailType %r ignored",
                  detail_type)
        return []
    if product == "card":
        return [card_record(body.get("accountId"), body.get("nickname"),
                            body.get("mask"), detail)]
    balance = detail.get("presentBalance")
    if balance is None:
        balance = detail.get("available")
    return [{
        "account_external_id": str(body.get("accountId", "")),
        "account_type": detail.get("detailType"),       # 'CHK', 'SAV'
        "nickname": body.get("nickname"),
        "mask": _mask(body.get("mask")),
        "currency": "USD",
        "product": "dda",
        "balance": balance,
    }]


def card_record(account_id, nickname, mask, detail: dict,
                account_type: str | None = None) -> dict:
    """Reduce one card's detail block to a roster record. Shared by the card
    dashboard's `account/detail/card/list` and the overview's
    `overview/card/v2/list`, which carry the same keys under different names
    for the balance.

    Amounts are stored exactly as Chase reports them, in particular the
    **owed balance is positive** — `creditLimit - currentBalance ==
    availableCredit` holds on every row, and a zero-balance card reports 0.0,
    never a negative. Negating a liability is the gold adapter's job, not the
    collector's; bronze stays provider-verbatim. Dates arrive as `YYYYMMDD`
    and are normalised to ISO, the same reduction the mask already gets.

    `pending_charges_amount` is the unposted activity already reflected in
    `balance` but absent from the exports, which carry posted rows only. It is
    the whole gap between a balance reconstructed from the transaction history
    and the balance the card reports live, so keeping it turns that gap from an
    untestable drift into an identity. Every field but the identifiers is
    optional: a detail block that omits one leaves None there, never a 0 that
    would read as a real figure."""
    detail = detail or {}
    balance = detail.get("currentBalance")
    if balance is None:
        balance = detail.get("outstandingBalance")
    return {
        "account_external_id": str(account_id if account_id is not None else ""),
        "account_type": account_type or detail.get("detailType") or "BAC",
        "nickname": nickname,
        "mask": _mask(mask),
        "currency": "USD",
        "product": "card",
        "balance": balance,
        "credit_limit": detail.get("creditLimit"),
        "available_credit": detail.get("availableCredit"),
        "pending_charges_amount": detail.get("pendingChargesAmount"),
        "last_statement_balance": detail.get("lastStmtBalance"),
        "last_statement_date": _ymd(detail.get("lastStmtDate")),
        "next_payment_due_date": _ymd(detail.get("nextPaymentDueDate")),
        "next_payment_amount": detail.get("nextPaymentAmount"),
        "next_closing_date": _ymd(detail.get("nextClosingDate")),
    }


def parse_card_overview(body: dict) -> list[dict]:
    """Reduce an `overview/card/v2/list` response — every card in one payload,
    detail included — to roster records. This is the cheapest source of card
    detail: the overview fetches it for all cards at once, where the card
    dashboard fires one `account/detail/card/list` per card."""
    if not isinstance(body, dict):
        return []
    out = []
    for group in (body.get("cardAccountOverviews") or []):
        if not isinstance(group, dict):
            continue
        for card in (group.get("cardAccounts") or []):
            if not isinstance(card, dict):
                continue
            out.append(card_record(
                card.get("accountId"), card.get("nickname"), card.get("mask"),
                card.get("cardAccountDetail") or {},
                account_type=card.get("accountType")))
    return out


def parse_documents_menu(body: dict) -> list[dict]:
    """Reduce a `documents/secure/v1/menu/list` response to the accounts the
    documents centre lists, as {account_external_id, product, doc_types}.

    Rows whose `summaryType` is neither DDA nor CARD are dropped — the menu
    can carry entries that are not accounts at all (an unknown summaryType,
    'UKN', on a non-account type such as 'ATM', whose documents are receipts
    rather than statements)."""
    if not isinstance(body, dict):
        return []
    out = []
    for item in (body.get("items") or []):
        if not isinstance(item, dict):
            continue
        product = PRODUCTS.get(str(item.get("summaryType", "")).upper())
        if product is None:
            continue
        out.append({
            "account_external_id": str(item.get("accountId", "")),
            "product": product,
            "doc_types": [str(d) for d in (item.get("docItems") or [])],
        })
    return out


def statement_accounts(bodies: list) -> set[str] | None:
    """The account ids the documents menu offers STATEMENTS for, or None when
    no menu response was captured — the caller then filters nothing rather
    than skipping every account on missing evidence."""
    seen = False
    out: set[str] = set()
    for body in bodies:
        if not (isinstance(body, dict) and "items" in body):
            continue
        for rec in parse_documents_menu(body):
            seen = True
            if "STATEMENTS" in rec["doc_types"] and rec["account_external_id"]:
                out.add(rec["account_external_id"])
    return out if seen else None


def expand_module_cache(bodies: list) -> list:
    """Flatten `dashboard/module/list` envelopes: yield every captured body,
    plus the roster/detail responses cached inside one under
    `cache[].response`. Those inner calls never hit the wire on their own, so
    without this pass the card roster and card detail are invisible to a
    URL-keyed response hook."""
    out = []
    for body in bodies:
        out.append(body)
        if not isinstance(body, dict):
            continue
        for entry in (body.get("cache") or []):
            if not isinstance(entry, dict):
                continue
            inner = entry.get("response")
            if isinstance(inner, dict) and CACHED_ROSTER_URL_RE.search(
                    str(entry.get("url", ""))):
                out.append(inner)
    return out


def unwrap_roster_bodies(body) -> list:
    """The bodies worth keeping from one captured roster response.

    A `dashboard/module/list` envelope is replaced by the roster/detail
    responses cached inside it: persisting the envelope whole would file a
    dozen unrelated cached responses (offers, tiles, credit-journey status)
    into `raw/` on every run for the two bodies the roster is actually built
    from. An envelope whose inner URLs no longer match is kept whole, so a
    shape drift stays diagnosable from the run that hit it."""
    inner = [b for b in expand_module_cache([body]) if b is not body]
    return inner or [body]


def collect_accounts(bodies: list) -> list[dict]:
    """Build the account roster from the captured /svc/ bodies — the
    per-account `account/detail/{dda,card}/list` shape, the all-cards
    `overview/card/v2/list` shape, and the all-accounts
    `download/options/list` shape — deduped by account id.

    The download-options record wins on overlap (it is the authoritative
    downloadable set) but only field by field: it carries no balance or card
    detail, so whatever a detail response already established survives.

    `product` is the one field that may be established but never downgraded:
    an account once seen as a card stays a card, so no later body can flip it
    to the deposit shape and have gold carry a debt as an asset."""
    by_id: dict[str, dict] = {}

    def _merge(rec: dict) -> None:
        ext = rec["account_external_id"]
        if not ext:
            return
        prior = by_id.get(ext)
        if prior is None:
            by_id[ext] = rec
            return
        merged = dict(prior)
        merged.update({k: v for k, v in rec.items()
                       if v is not None or k not in merged})
        if prior.get("product") == "card" and merged.get("product") != "card":
            log.warning("refusing to reclassify a card account as %r",
                        merged.get("product"))
            merged["product"] = "card"
        by_id[ext] = merged

    bodies = expand_module_cache(bodies)
    for body in bodies:
        for rec in parse_account_detail(body):
            _merge(rec)
        for rec in parse_card_overview(body):
            _merge(rec)
    for body in bodies:
        for rec in parse_download_options(body):
            _merge(rec)
    return list(by_id.values())


def accounts_by_product(accounts: list[dict]) -> dict[str, list[dict]]:
    """Group a roster by `product`, defaulting a record without one to 'dda'
    (the shape every pre-card bronze run wrote)."""
    out: dict[str, list[dict]] = {}
    for acct in accounts:
        out.setdefault(acct.get("product") or "dda", []).append(acct)
    return out


def export_route(acct: dict) -> str:
    """The export page's hash route for one account. Its trailing
    `/{summaryType}/{detailType}` is a product tuple — DDA/CHK for the deposit
    accounts, CARD/BAC for the cards — not a constant."""
    summary, detail = PRODUCT_ROUTE.get(acct.get("product") or "dda",
                                        PRODUCT_ROUTE["dda"])
    return ROUTE_TX_DOWNLOAD.format(ext=acct["account_external_id"],
                                    summary=summary, detail=detail)


def statement_click_paths(n: int) -> list[list[str]]:
    """Click sequences that fire statement row `n`'s PDF download, in
    preference order; each is clicked in turn and the download fires on the
    LAST click of a sequence.

    Two shapes are coded because only one of them is evidenced. The card
    documents view offers a direct download anchor per row; the legacy
    dropdown + "Save as PDF" item that the deposit path was built on appears
    nowhere in the card capture — but that session never opened the deposit
    documents view, so whether the anchor replaced the dropdown everywhere or
    only on cards is unverified, and neither shape may be assumed gone."""
    return [
        [STMT_DOWNLOAD_ANCHOR.format(n=n)],
        [STMT_DOWNLOAD_TRIGGER.format(n=n), STMT_PDF_OPTION.format(n=n)],
    ]


def _url_stem(url: str) -> str:
    """A short filesystem-safe stem from a /svc/ URL's last two path segments,
    for naming a debug body dump. No query string (it can carry ids)."""
    path = re.sub(r"\?.*$", "", str(url))
    tail = "-".join(p for p in path.split("/")[-2:] if p)
    return re.sub(r"[^0-9A-Za-z._-]+", "_", tail).strip("_")[:48] or "svc"


def _mask(raw) -> str | None:
    if raw in (None, ""):
        return None
    s = str(raw)
    return s if s.startswith("…") else f"…{s}"


def _ymd(raw) -> str | None:
    """Chase's `YYYYMMDD` date fields as ISO `YYYY-MM-DD`. Anything that isn't
    a real 8-digit date — absent, empty, or a zero sentinel — is None."""
    s = str(raw or "")
    if not re.fullmatch(r"\d{8}", s):
        return None
    try:
        return date(int(s[:4]), int(s[4:6]), int(s[6:])).isoformat()
    except ValueError:
        return None


_STMT_MONTHS = {m: i for i, m in enumerate(
    ["jan", "feb", "mar", "apr", "may", "jun",
     "jul", "aug", "sep", "oct", "nov", "dec"], 1)}


def parse_statement_date(text: str) -> date | None:
    """Parse a statement row's date cell ('Dec 17, 2025') to a date. Returns
    None for anything that doesn't match, so a stray header/blank row is
    skipped rather than crashing the loop."""
    m = re.match(r"\s*([A-Za-z]{3})[a-z]*\.?\s+(\d{1,2}),\s*(\d{4})", text or "")
    if not m:
        return None
    month = _STMT_MONTHS.get(m.group(1).lower())
    if not month:
        return None
    try:
        return date(int(m.group(3)), month, int(m.group(2)))
    except ValueError:
        return None


def statement_years(since: date | None, until: date | None,
                    available: list[int] | None = None) -> list[int]:
    """Years to page the statements filter through, newest first. Bounded by
    the export window [since, until]; intersected with `available` (the years
    the filter actually offers) when known, so a request never selects a year
    Chase doesn't list."""
    hi = (until or date.today()).year
    lo = since.year if since else (min(available) if available else hi)
    years = list(range(hi, lo - 1, -1))
    if available is not None:
        years = [y for y in years if y in available]
    return years


def new_coverage(by_product: dict[str, list[dict]]) -> dict:
    """A zeroed per-product coverage block: how many accounts of each product
    were discovered, and how many of them were fully exported / had their
    statement pass complete. `score_coverage` stamps the verdict."""
    return {product: {"accounts": len(accts), "exported": 0, "statements": 0}
            for product, accts in by_product.items()}


def score_coverage(coverage: dict, *, documents: bool) -> dict:
    """Stamp each product's counts with a `complete` flag — every discovered
    account of that product exported, and (with documents on) its statement
    pass finished without a failed row.

    A product's shortfall is recorded HERE rather than in `status`, because
    `load` skips a non-complete run WHOLESALE — one failed card export would
    otherwise cost that night's deposit ledger its load and expose it to
    `prune`. Nothing gates on the flag: a short dump still loads (silver
    ingest is additive), and closure is decided in gold, per account, where
    absence never wins."""
    scored = {}
    for product, counts in coverage.items():
        counts = dict(counts)
        discovered = counts.get("accounts", 0)
        complete = bool(discovered) and counts.get("exported", 0) >= discovered
        if complete and documents:
            complete = counts.get("statements", 0) >= discovered
        counts["complete"] = complete
        scored[product] = counts
    return scored


def build_manifest(status: str, *, accounts: list[dict], counts: dict,
                   since: date | None, until: date | None,
                   dry_run: bool, documents: bool,
                   coverage: dict | None = None) -> dict:
    """The run.json body. `status` is 'in-progress' at creation, overwritten
    with 'complete' (or 'dry-run') at the end — it says the walk finished, and
    is what `load` and `prune` read. `coverage` scores each product on its own
    (`score_coverage`), so a product that fell short is flagged there instead
    of costing the whole run its status."""
    return {
        "schema": 1,
        "source": "chase",
        "status": status,
        "dry_run": dry_run,
        "documents": documents,
        "window": {
            "since": since.isoformat() if since else None,
            "until": until.isoformat() if until else None,
        },
        "account_external_ids": [a["account_external_id"] for a in accounts],
        "counts": counts,
        "coverage": coverage or {},
    }


# ============================================================
# Scrape
# ============================================================

def walk(page, bronze_dir: Path, *, since: date | None = None,
         until: date | None = None, documents: bool = True,
         dry_run: bool = False, debug: bool = False,
         nav_timeout_ms: int = 45_000) -> dict:
    """Scrape the authenticated `page` into a fresh bronze run dir under
    `bronze_dir`. Returns a stats dict. On `dry_run`, walks and reports what
    it would fetch but writes no exports and stamps the manifest 'dry-run'."""
    until = until or date.today()
    slug = bronze.ts_slug()
    run_dir = bronze.run_dir(bronze_dir, slug)
    (run_dir / "raw").mkdir(parents=True, exist_ok=True)
    (run_dir / "transactions").mkdir(exist_ok=True)
    if documents:
        (run_dir / "statements").mkdir(exist_ok=True)

    # in-progress marker up front, so a crash leaves a prunable dump.
    bronze.atomic_write_json(run_dir / "run.json", build_manifest(
        "in-progress", accounts=[], counts={}, since=since, until=until,
        dry_run=dry_run, documents=documents))

    # Capture the account-bearing /svc/ JSON responses (roster + provenance).
    # A list, not a URL-keyed dict: `account/detail/dda/list` fires once per
    # account at the SAME URL, so keying by URL would drop all but the last.
    captured_bodies: list = []
    raw_seq = {"n": 0}

    def _on_response(resp):
        try:
            if "/svc/" not in resp.url or "json" not in (
                    resp.headers.get("content-type", "").lower()):
                return
            body = resp.json()
            is_roster = bool(ROSTER_URL_RE.search(resp.url)) or (
                isinstance(body, dict) and (
                    "downloadAccountActivityOptions" in body
                    or "accountId" in body))
            if is_roster:
                # An envelope is unwrapped here, not just at reduce time, so
                # raw/ holds the roster bodies rather than the envelope's whole
                # module cache (see `unwrap_roster_bodies`).
                for kept in unwrap_roster_bodies(body):
                    captured_bodies.append(kept)
                    raw_seq["n"] += 1
                    bronze.atomic_write_json(
                        run_dir / "raw" / f"account-{raw_seq['n']:02d}.json",
                        kept)
            elif debug:
                # Self-documenting: with --debug, every other /svc/ JSON body is
                # kept under raw/svc/ so an unmapped roster/export shape can be
                # pinned from one run instead of a repeat live login.
                raw_seq["n"] += 1
                (run_dir / "raw" / "svc").mkdir(exist_ok=True)
                bronze.atomic_write_json(
                    run_dir / "raw" / "svc"
                    / f"{raw_seq['n']:03d}-{_url_stem(resp.url)}.json", body)
        except Exception as exc:                       # pragma: no cover
            log.debug("response capture skipped: %r", exc)

    page.on("response", _on_response)

    log.info("scrape: window %s → %s, documents=%s, dry_run=%s",
             since or "(all)", until, documents, dry_run)
    if since is not None and (date.today() - since).days > 730:
        log.warning("--lookback exceeds Chase's 24-month export window; the "
                    "export fetches the full 24 months for every account.")
    page.goto(ROUTE_OVERVIEW, wait_until="domcontentloaded",
              timeout=nav_timeout_ms)

    accounts = _discover_accounts(page, captured_bodies, nav_timeout_ms)
    by_product = accounts_by_product(accounts)
    log.info("discovered %d account(s) (%s)", len(accounts),
             ", ".join(f"{p}={len(v)}" for p, v in sorted(by_product.items()))
             or "none")
    # The roster the loader reads (bronze contract): [{account_external_id,
    # account_type, nickname, mask, currency, product, balance}], plus the
    # card detail fields on a product='card' record.
    bronze.atomic_write_json(run_dir / "accounts.json", accounts)

    counts = {"transactions_files": 0, "statements": 0}
    coverage = new_coverage(by_product)
    for acct in accounts:
        if dry_run:
            log.info("  [dry-run] would export %s for %s",
                     "+".join(f.upper() for f in EXPORT_FORMATS),
                     acct.get("mask") or acct["account_external_id"])
            continue
        got = _export_account(page, acct, run_dir, nav_timeout_ms)
        counts["transactions_files"] += got
        if got == len(EXPORT_FORMATS):
            coverage[acct.get("product") or "dda"]["exported"] += 1

    if documents and not dry_run:
        saved, covered = _download_statements(
            page, by_product, captured_bodies, run_dir, since, until,
            nav_timeout_ms)
        counts["statements"] = saved
        for product, n in covered.items():
            coverage[product]["statements"] = n

    # The walk finished, so the run is loadable; a product that fell short is
    # flagged in `coverage`, never by failing the run (see `score_coverage`).
    status = "dry-run" if dry_run else "complete"
    coverage = score_coverage(coverage, documents=documents)
    if not dry_run:
        for product, scored in sorted(coverage.items()):
            if not scored["complete"]:
                log.warning("%s coverage incomplete — run.json flags the "
                            "product, the run stays loadable: %s",
                            product, scored)
    bronze.atomic_write_json(run_dir / "run.json", build_manifest(
        status, accounts=accounts, counts=counts, since=since, until=until,
        dry_run=dry_run, documents=documents, coverage=coverage))
    log.info("scrape %s: %s", status, counts)
    return {"run_dir": str(run_dir), "accounts": len(accounts), **counts}


def _cards_missing_detail(roster: list[dict]) -> list[dict]:
    """Card records whose detail never arrived — no balance was established,
    so the dashboard envelope carrying it has not been seen for them."""
    return [a for a in roster
            if (a.get("product") == "card") and a.get("balance") is None]


def _discover_accounts(page, captured_bodies: list, timeout_ms: int) -> list[dict]:
    """Build the account roster from the /svc/ calls the overview fires
    (DESIGN.md §B): `account/detail/dda/list` per deposit account, the card
    roster + detail nested in the overview's `dashboard/module/list` envelope,
    and — when reachable — the all-accounts `download/options/list`. The caller
    has already navigated to the overview; settle for the XHRs, then reduce."""
    _settle(page)
    roster = collect_accounts(captured_bodies)
    # After login the SPA is already mounted, so the same-route goto that got
    # us here won't re-fire the overview's XHRs. A reload does — it replays
    # account/detail/dda/list and the dashboard envelope (whose cached
    # overview/card/v2/list carries every card's detail in one response),
    # which the capture hook records.
    if not roster or _cards_missing_detail(roster):
        try:
            page.reload(wait_until="domcontentloaded", timeout=timeout_ms)
            _settle(page)
            roster = collect_accounts(captured_bodies)
        except Exception as exc:
            log.debug("overview reload nudge failed (continuing): %r", exc)
    # If accounts are known but the (complete) download-options roster hasn't
    # been seen, visit the first account's download page to trigger it — it
    # returns every downloadable account in one response (DESIGN.md §E).
    have_options = any("downloadAccountActivityOptions" in b
                       for b in captured_bodies)
    if roster and not have_options:
        try:
            page.goto(export_route(roster[0]), wait_until="domcontentloaded",
                      timeout=timeout_ms)
            _settle(page)
            roster = collect_accounts(captured_bodies)
        except Exception as exc:
            log.debug("download-options nudge failed (continuing): %r", exc)
    # Last resort for a card whose detail is still missing: its own dashboard
    # route, which fires the card envelope for that one card.
    for acct in _cards_missing_detail(roster):
        try:
            page.goto(ROUTE_CARD_SUMMARY.format(
                          ext=acct["account_external_id"]),
                      wait_until="domcontentloaded", timeout=timeout_ms)
            _settle(page)
        except Exception as exc:
            log.debug("card detail nudge failed (continuing): %r", exc)
    roster = collect_accounts(captured_bodies) or roster
    if not roster:
        log.warning("no accounts discovered — confirm the overview fires "
                    "account/detail/dda/list, dashboard/module/list or "
                    "download/options/list (DESIGN.md §B); the run writes an "
                    "empty bronze.")
    return roster


def _export_account(page, acct: dict, run_dir: Path, timeout_ms: int) -> int:
    """Export the CSV + QFX activity for one account — deposit or card — into
    transactions/<ext_id>.<fmt>. Returns the number of files captured.

    The card export is a GET whose response is the file, where the deposit
    export is a POST, but both are triggered the same way: through the form,
    so the browser supplies the session cookie and the CSRF token. The only
    per-product difference is the route's product tuple, plus the account
    picker guard below."""
    ext = acct["account_external_id"]
    stem = safe_stem(ext)
    route = export_route(acct)
    got = 0
    # One fresh page load per format: the two exports share no state, so a
    # dialog or overlay left by the first (e.g. the session-timeout prompt)
    # can't strand the second.
    for fmt, opt_prefix in EXPORT_FORMATS.items():
        try:
            page.goto(route, wait_until="domcontentloaded", timeout=timeout_ms)
            _settle(page)
            # A goto to the hash route we're already on doesn't re-render the
            # SPA, so the form can be stale/absent on the second format. Wait
            # for it; force a reload if it didn't render.
            if not _wait_visible(page, SEL_FILE_SELECT, 8):
                page.reload(wait_until="domcontentloaded", timeout=timeout_ms)
                _settle(page)
                _wait_visible(page, SEL_FILE_SELECT, 8)
            # The form's account picker, not the route, decides what gets
            # exported. A hash-only route change can leave it bound to the
            # previous account, so confirm it — and correct it through the
            # picker, which is cheaper than another reload. Refuse the export
            # outright if it can't be confirmed: a file saved under the wrong
            # account is worse than a missing one, which only leaves this
            # product short of coverage in run.json.
            if not _ensure_export_account(page, ext):
                raise RuntimeError(
                    "export form is bound to a different account")
            # "All transactions" is Chase's widest export window here (last 24
            # months, both products). --lookback narrower is covered; wider
            # Chase cannot export, which `walk` warns about once per run.
            _select_option(page, SEL_ACTIVITY_SELECT, OPT_ACTIVITY_ALL,
                           OPT_ACTIVITY_ALL_LABEL)
            _select_option(page, SEL_FILE_SELECT, opt_prefix,
                           EXPORT_FORMAT_LABELS.get(fmt))
            # Re-picking the already-selected format (CSV is the default) leaves
            # the option listbox open, overlaying #downloadButton so its click
            # can't land. Dismiss any open dropdown before triggering.
            _dismiss_dropdown(page)
            with page.expect_download(timeout=timeout_ms) as dl_info:
                if not mdsui.click(page, SEL_DOWNLOAD_BUTTON):
                    mdsui.activate(page, SEL_DOWNLOAD_BUTTON)
            dl_info.value.save_as(str(run_dir / "transactions" / f"{stem}.{fmt}"))
            got += 1
            log.info("  exported %s for %s", fmt, acct.get("mask") or ext)
        except Exception as exc:
            log.warning("  %s export failed for %s: %r", fmt,
                        acct.get("mask") or ext, exc)
    return got


def _download_statements(page, by_product: dict, captured_bodies: list,
                         run_dir: Path, since, until,
                         timeout_ms: int) -> tuple[int, dict]:
    """Download statement PDFs for every product into statements/<ext_id>/.
    Returns (files saved, {product: accounts whose statement pass completed}) —
    the second half feeds the run's coverage counts."""
    saved, covered = 0, {}
    deposits = by_product.get("dda") or []
    if deposits:
        n, ok = _download_deposit_statements(
            page, deposits, run_dir, since, until, timeout_ms)
        saved += n
        covered["dda"] = ok
    cards = by_product.get("card") or []
    if cards:
        n, ok = _download_card_statements(
            page, cards, captured_bodies, run_dir, since, until, timeout_ms)
        saved += n
        covered["card"] = ok
    return saved, covered


def _download_deposit_statements(page, accounts, run_dir: Path, since, until,
                                 timeout_ms: int) -> tuple[int, int]:
    """Download deposit statement PDFs into statements/<ext_id>/ (DESIGN.md §C).
    The documents centre lists the deposit relationship's statements as a
    classic table with a per-row save control; each save fires a download.

    The surface is relationship-wide and has no per-account selector, so a
    statement it yields belongs to every deposit account printed on it, and
    the directory it lands in is a BUCKET rather than a claim about whose it
    is. The bucket is the lowest external id of the deposit set, which makes
    it a deterministic function of the roster rather than of the order the
    /svc/ roster bodies happened to arrive in — under an arbitrary "first
    account" the whole statement set silently moved between directories from
    one run to the next, and the loader's balance chaining, which is anchored
    on one account's own export balances, then found nothing to attribute and
    dropped the entire pre-export backfill. `load.load_statement_transactions`
    pools these PDFs relationship-wide and attributes each account's segments
    by balance, so nothing is duplicated per account here.

    Returns (files saved, accounts covered — 0 or the whole deposit set,
    since one pass covers them all and the loader can reach every account
    from the one bucket). A pass with any failed row covers nothing: coverage
    has to mean the statements are all there, not that the page rendered.
    What it asserts is that the RELATIONSHIP's statement set is complete, not
    that every deposit account is printed on it: an account opened after the
    statement era appears on none of them and imports nothing however
    complete the set is."""
    try:
        page.goto(ROUTE_DOCUMENTS, wait_until="domcontentloaded",
                  timeout=timeout_ms)
        _settle(page)
        # The micro-app can render the table after networkidle; wait for it,
        # and force a reload if a same-route nav left it stale (as with the
        # export form). Without this the year filter finds no options.
        if not _wait_visible(page, SEL_STATEMENTS_ACCORDION, 10):
            page.reload(wait_until="domcontentloaded", timeout=timeout_ms)
            _settle(page)
            _wait_visible(page, SEL_STATEMENTS_ACCORDION, 10)
    except Exception as exc:
        log.warning("documents area unreachable: %r", exc)
        return 0, 0
    out_dir = run_dir / "statements" / safe_stem(
        min(a["account_external_id"] for a in accounts))
    out_dir.mkdir(parents=True, exist_ok=True)
    saved, failed = _save_statements(page, out_dir, since, until, timeout_ms)
    if failed:
        log.warning("  statements: %d row(s) failed on the deposit pass; "
                    "the deposit product is not covered", failed)
    return saved, 0 if failed else len(accounts)


def _download_card_statements(page, cards, captured_bodies: list,
                              run_dir: Path, since, until,
                              timeout_ms: int) -> tuple[int, int]:
    """Download card statement PDFs into statements/<card_ext_id>/.

    Card documents are per card, never combined, so each card is paged on its
    own account-scoped documents route. The year filter reaches further back
    than the 24-month activity export does, which makes statements the only
    route to a card's older history — so the window, not the export cap,
    bounds the years.

    Returns (files saved, cards whose pass completed). A card counts as covered
    only when every in-window row it offered was saved: if the row shape drifts
    the way the dropdown already did, every row fails, and a pass that reported
    coverage anyway would present a card with no statements at all as a closed
    set."""
    saved, covered = 0, 0
    for card in cards:
        ext = card["account_external_id"]
        label = card.get("mask") or ext
        # The documents menu says which accounts carry statements at all; skip
        # a card it lists without them rather than paging an empty table. Read
        # per card, not once: the menu is fetched by the first navigation into
        # the documents centre, so it is unknown until then — and unknown
        # filters nothing.
        with_statements = statement_accounts(captured_bodies)
        if with_statements is not None and ext not in with_statements:
            log.info("  statements: %s lists none, skipping", label)
            covered += 1
            continue
        try:
            page.goto(ROUTE_ACCOUNT_DOCUMENTS.format(ext=ext),
                      wait_until="domcontentloaded", timeout=timeout_ms)
            _settle(page)
            if not _wait_visible(page, SEL_STATEMENTS_ACCORDION, 10):
                page.reload(wait_until="domcontentloaded", timeout=timeout_ms)
                _settle(page)
                if not _wait_visible(page, SEL_STATEMENTS_ACCORDION, 10):
                    raise RuntimeError("statements table did not render")
        except Exception as exc:
            log.warning("  statements unreachable for %s: %r", label, exc)
            continue
        out_dir = run_dir / "statements" / safe_stem(ext)
        out_dir.mkdir(parents=True, exist_ok=True)
        got, failed = _save_statements(page, out_dir, since, until, timeout_ms)
        saved += got
        if failed:
            log.warning("  statements: %d row(s) failed for %s; the card is "
                        "not covered", failed, label)
            continue
        covered += 1
    return saved, covered


def _save_statements(page, out_dir: Path, since, until,
                     timeout_ms: int) -> tuple[int, int]:
    """Page the statements table through each year in the export window,
    saving every statement PDF whose date falls in [since, until]. Returns
    (files saved, failures) — a failure count, not just a success count, so
    the caller can tell a complete pass from one that reached the table and
    saved nothing. A failure is an in-window row that offered no working save
    control, plus the whole pass when not one requested year could be
    selected: the filter reading empty is a shape drift, not an account
    without statements (a readable filter's years are intersected with what it
    offers, so an unavailable year is never requested)."""
    _expand_statements(page)
    avail = _available_statement_years(page)
    years = statement_years(since, until, avail)
    log.info("  statements: filter offers %s; paging %s", avail, years)
    got = fail = shown = 0
    seen: set[str] = set()
    for year in years:
        if not _select_statement_year(page, year):
            log.info("  statements: %d not shown, skipping", year)
            continue
        shown += 1
        n, f = _save_visible_statements(page, out_dir, since, until, seen,
                                        timeout_ms)
        log.info("  statements: saved %d for %d (%d failed)", n, year, f)
        got += n
        fail += f
    if years and not shown:
        log.warning("  statements: no requested year could be selected "
                    "(filter offered %s) — the pass covers nothing", avail)
        fail += 1
    return got, fail


def _expand_statements(page) -> None:  # pragma: no cover
    """Ensure the STATEMENTS accordion is open (it defaults open; a click is a
    no-op belt-and-braces if a build ships it collapsed)."""
    loc = mdsui.first_in_frames(page, SEL_STATEMENTS_ACCORDION)
    with contextlib.suppress(Exception):
        if loc is not None and loc.get_attribute("aria-expanded") == "false":
            mdsui.click(page, SEL_STATEMENTS_ACCORDION)
            _settle(page, 400)


_STMT_YEARS_JS = (
    "() => Array.from(document.querySelectorAll("
    "'[id^=\"container-primary-\"][id$=\"-filterstyledselect-0\"]'))"
    ".map(e => (e.textContent||'').trim())")


def _available_statement_years(page) -> list[int] | None:  # pragma: no cover
    """The years the statements filter offers, or None if the filter is absent.
    The options lag the accordion after a navigation (they arrive together in
    one styled-select), so poll a few seconds for them to populate — reading
    too early truncated the year list and left --lookback all with only the
    current year."""
    for _ in range(8):
        for frame in mdsui.chase_frames(page):
            with contextlib.suppress(Exception):
                texts = frame.evaluate(_STMT_YEARS_JS)
                years = sorted({int(t) for t in texts if t.isdigit()})
                if years:
                    return years
        page.wait_for_timeout(500)
    return None


_SELECT_YEAR_JS = r"""
(year) => {
  const opts = document.querySelectorAll(
    '[id^="container-primary-"][id$="-filterstyledselect-0"]');
  for (const s of opts) {
    if ((s.textContent || '').trim() === String(year)) {
      const a = s.closest('a[id^="container-"]') || s.parentElement;
      if (a) { a.click(); return true; }
    }
  }
  return false;
}
"""


def _table_year(page) -> int | None:  # pragma: no cover
    """The year of the first statement row currently shown, or None if empty."""
    loc = mdsui.first_in_frames(page, STMT_DATE_CELL.format(n=0))
    if loc is None:
        return None
    try:
        d = parse_statement_date(loc.text_content() or "")
    except Exception:
        d = None
    return d.year if d else None


def _select_statement_year(page, year: int) -> bool:  # pragma: no cover
    """Switch the statements table to `year` and CONFIRM it changed. The options
    are light-DOM `<a role=option>` in the styled-select; a native click on the
    one matching the year swaps the table. Returns True only once the table's
    first row is actually in `year` — so an unavailable year, or a click that
    didn't take, returns False rather than leaving the caller to re-scrape the
    year already shown (the bug that made --lookback all save only the current
    year)."""
    if _table_year(page) == year:
        return True
    for _ in range(2):
        clicked = False
        for frame in mdsui.chase_frames(page):
            with contextlib.suppress(Exception):
                if frame.evaluate(_SELECT_YEAR_JS, year):
                    clicked = True
                    break
        if not clicked:
            return False                    # no option for this year
        _settle(page)
        _wait_visible(page, STMT_DATE_CELL.format(n=0), 8)
        if _table_year(page) == year:
            return True
    return False


def _save_visible_statements(page, out_dir: Path, since, until, seen: set,
                             timeout_ms: int) -> tuple[int, int]:
    """Save every in-window statement PDF from the currently displayed table.
    Rows are contiguous (row0, row1, …); the first missing row ends the walk.
    Returns (saved, failed): a row that offered no working save control is
    counted, not merely warned about, so a drifted row shape shows up as an
    uncovered account instead of a silently empty statement directory."""
    got = failed = 0
    for n in range(200):
        date_loc = mdsui.first_in_frames(page, STMT_DATE_CELL.format(n=n))
        if date_loc is None:
            break
        try:
            stmt_date = parse_statement_date(date_loc.text_content() or "")
        except Exception:
            stmt_date = None
        if stmt_date is None:
            continue
        if (since and stmt_date < since) or (until and stmt_date > until):
            continue
        key = stmt_date.isoformat()
        if key in seen:
            continue
        if _save_statement_row(page, n, out_dir / f"{key}.pdf", timeout_ms):
            seen.add(key)
            got += 1
            log.info("  saved statement %s", key)
        else:
            failed += 1
            log.warning("  statement %s: no save control fired", key)
    return got, failed


def _save_statement_row(page, n: int, dest: Path,
                        timeout_ms: int) -> bool:
    """Fire row `n`'s statement PDF download and save it to `dest`.

    Tries each shape from `statement_click_paths` in turn — the direct
    download anchor first, the legacy dropdown + "Save as PDF" item second —
    so the pass survives whichever of them the documents view is serving."""
    for path in statement_click_paths(n):
        *lead, trigger = path
        if not all(mdsui.click(page, sel) for sel in lead):
            continue
        if lead:
            _settle(page, 300)
        # Probe the trigger BEFORE opening the download expectation. Sync
        # Playwright's expectation waits on its future in __exit__ even when
        # the body raised, so entering it for a trigger that isn't there costs
        # the full timeout per row — an hour-scale tax on a deep pass served
        # by the other shape, where the shape that IS served costs nothing.
        if mdsui.locate(page, trigger) is None:
            log.debug("  statement row %d: no %s on this view", n, trigger)
            continue
        try:
            with page.expect_download(timeout=timeout_ms) as dl_info:
                if not mdsui.click(page, trigger):
                    raise RuntimeError(f"no clickable target: {trigger}")
            dl_info.value.save_as(str(dest))
            return True
        except Exception as exc:
            log.debug("  statement row %d via %s failed: %r", n, trigger, exc)
    return False


# ---- small UI helpers (browser-side) -------------------------------------
# Live-validated on the deposit path; the account-picker helpers below are
# taken from the card exploration capture (their selectors and the picker's
# option shape) and are verified against the form on the first live run.

def _settle(page, ms: int = 1500) -> None:  # pragma: no cover
    """Give the SPA a moment to fire its XHRs after a route change."""
    try:
        page.wait_for_load_state("networkidle", timeout=8000)
    except Exception:
        time.sleep(ms / 1000)


def _wait_visible(page, selector: str, secs: float) -> bool:  # pragma: no cover
    """Poll (sync-Playwright-safe) up to `secs` for a visible match of
    `selector` across the chase frames. Returns True once seen."""
    for _ in range(int(secs * 2)):
        if mdsui.locate(page, selector) is not None:
            return True
        page.wait_for_timeout(500)
    return False


def _wait_for_export_picker(page, secs: float = PICKER_WAIT_SECS) -> None:
    """Poll up to `secs` for either variant of the export form's account
    picker to mount. A hash-only navigation swaps the form under the previous
    account's controls, so the file-select the export already waits on can be
    the OLD one — the picker has to be waited for in its own right, or its
    absence is read off a form that hasn't rendered yet."""
    for _ in range(int(secs * 2)):
        if (mdsui.locate(page, SEL_ACCOUNT_SELECT) is not None
                or mdsui.locate(page, SEL_ACCOUNT_SELECT_INDEXED) is not None):
            return
        page.wait_for_timeout(500)


def _export_picker_state(page) -> tuple[str, str | None]:
    """The export form's account picker as (state, bound account id).

    Three outcomes, deliberately kept apart because the caller may proceed on
    exactly one of them:

    * `(PICKER_BOUND, '<ext>')` — mounted, and reporting the account whose
      activity the form would export.
    * `(PICKER_UNREADABLE, None)` — a picker is on the page but says nothing
      usable: its value is empty, reading it raised (a node detached
      mid-rerender), or the variant rendered keys its options by POSITION
      instead of by account id, so nothing in it names an account.
    * `(PICKER_ABSENT, None)` — no picker at all; the route alone decides the
      account, which is how the deposit path has always worked."""
    host = mdsui.first_in_frames(page, SEL_ACCOUNT_SELECT)
    if host is None:
        if mdsui.first_in_frames(page, SEL_ACCOUNT_SELECT_INDEXED) is not None:
            log.warning("  export form renders the positional account picker; "
                        "its binding cannot be read")
            return PICKER_UNREADABLE, None
        return PICKER_ABSENT, None
    try:
        value = host.get_attribute("value")
    except Exception as exc:
        log.debug("  account picker read failed: %r", exc)
        return PICKER_UNREADABLE, None
    value = str(value or "").strip()
    return (PICKER_BOUND, value) if value else (PICKER_UNREADABLE, None)


def _select_account_option(page, ext: str) -> bool:
    """Pick the account picker's option for account `ext`, through the same
    three-step activation `_select_option` uses — a real click, then the
    role-based click, then the synthetic path — with the option's id in place
    of its data-testid. That testid is built from the option's visible label
    rather than from the account id, so it cannot be derived here; the option
    carries the label as an attribute, which is enough to reach `click_role`,
    the activation mdsui documents as the proven one for an MDS option."""
    option = OPT_ACCOUNT.format(ext=ext)
    if mdsui.click(page, option):
        return True
    label = None
    node = mdsui.first_in_frames(page, option)
    if node is not None:
        with contextlib.suppress(Exception):
            label = node.get_attribute("label")
    if label and mdsui.click_role(page, "option", label):
        return True
    return mdsui.activate(page, option)


def _ensure_export_account(page, ext: str) -> bool:
    """Confirm the export form is bound to account `ext`, switching the picker
    if it isn't.

    True when the form is (or was made) bound to `ext`, and when the page
    genuinely has no picker — the route then decides the account on its own.
    Everything else refuses, because the failure this guards is silent: the
    form exports the account it is bound to, and a file saved under a
    different account's id corrupts the ledger downstream where a missing file
    merely leaves a gap. So absence is only believed after polling for the
    picker, and a picker that is present but unreadable is a refusal."""
    _wait_for_export_picker(page)
    state, current = _export_picker_state(page)
    if state == PICKER_ABSENT:
        log.debug("  export form has no account picker; the route decides")
        return True
    if state == PICKER_UNREADABLE:
        log.warning("  export form's account binding is unreadable; refusing "
                    "the export rather than guessing the account")
        return False
    if current == str(ext):
        return True
    log.info("  export form bound elsewhere; switching the account picker")
    if not mdsui.click(page, SEL_ACCOUNT_SELECT):
        mdsui.activate(page, SEL_ACCOUNT_SELECT)
    _settle(page, 400)
    if not _select_account_option(page, ext):
        log.debug("  no option activated for the requested account")
    _settle(page, 400)
    _dismiss_dropdown(page)
    # The picker read a moment ago, so anything but an exact match now — an
    # unreadable picker included — is a switch that did not take.
    return _export_picker_state(page) == (PICKER_BOUND, str(ext))


def _dismiss_dropdown(page) -> None:  # pragma: no cover
    """Close any open MDS `<mds-select>` listbox by pressing Escape in the
    chase frame, so an overlay left open by re-picking the current value does
    not sit on top of the download trigger."""
    for frame in mdsui.chase_frames(page):
        with contextlib.suppress(Exception):
            frame.locator("body").press("Escape", timeout=2000)
            return


def _css_attr_value(s: str) -> str:
    """Quote a string for use inside a CSS attribute selector — the MDS
    data-testids contain spaces, parentheses and commas."""
    return '"' + s.replace("\\", "\\\\").replace('"', '\\"') + '"'


def _select_option(page, select_sel: str, option_testid_prefix: str,
                   option_label: str | None = None) -> bool:
    """Open an MDS `<mds-select>` and pick the `<mds-select-option>` whose
    data-testid starts with `option_testid_prefix`. The select and its options
    render in an OPEN shadow tree (like the 2FA list), so a real Playwright
    click is the reliable activation — a synthesised dispatch does not fire the
    component handler. Falls back to the option's accessible name, then to the
    synthetic path, for resilience."""
    if not mdsui.click(page, select_sel):         # open the dropdown
        mdsui.activate(page, select_sel)
    _settle(page, 400)
    testid_sel = f"[data-testid^={_css_attr_value(option_testid_prefix)}]"
    return (mdsui.click(page, testid_sel)
            or (option_label is not None
                and mdsui.click_role(page, "option", option_label))
            or mdsui.activate(page, testid_sel))
