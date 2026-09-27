#!/usr/bin/env python3
"""
Card-surface capture for ubs-web bronze.

The netbanking SPA reads its card area from a REST API, and that API —
not the toolbar's CSV — is what this module captures. The reasoning is
recorded in DESIGN.md §5; the two facts that decide the shape:

* **The roster is the only complete enumeration.** The homepage tiles a
  subset of the card accounts, so anchors scraped from it under-collect
  with nothing to show for it. Every walk starts at
  ``/api/v2/credit-card-accounts``.
* **The JSON is strictly richer than the export.** It carries a stable
  per-row identity (``_id``), which the CSV has no column for, plus both
  dates, both currencies' amounts, the row's billing state and the
  merchant category. Capturing the CSV as well would cost a request per
  account for a lossy copy, so the export endpoint is deliberately not
  fetched.

Statement PDFs *are* fetched: the eDocuments archive carries no card
category (DESIGN.md §5.5), so the invoice archive is the only place a
card statement exists at all.

Read-only by construction, and enforced rather than intended. The roster
advertises links that would move money or change a card —
``paymentToCard``, ``register`` / ``unregister``, ``reassign``,
``orders`` — so no ``_links`` href is ever followed blindly:
:func:`refuse_path` admits only the read endpoints below, every request
goes through it, and the one relation that is followed (``next``, the
ledger's paging cursor) is checked like any other. AGENTS.md §1 states
the surface; this is where it is held.
"""

from __future__ import annotations

import hashlib
import logging
import re
from datetime import date
from pathlib import Path
from urllib.parse import urlsplit

from collectorkit import bronze, debugcap

# The payload-shape reading lives with the parsers: which nodes in a
# roster or a period listing are entities is one question, and the
# capture and the load must not answer it differently.
import card_parsers

log = logging.getLogger("ubs-web.cards")

# The read endpoints, as anchored path patterns. Anything else — a
# different path, a mutating sibling, a link the API volunteered — is
# refused before a request is made.
_ALLOWED_PATHS = (
    re.compile(r"^/api/v2/credit-card-accounts$"),
    re.compile(r"^/api/v1/credit-card-transactions$"),
    re.compile(r"^/api/v1/credit-card-invoices$"),
    re.compile(r"^/api/v1/credit-card-invoices/[A-Za-z0-9_-]+$"),
    re.compile(r"^/api/v1/credit-card-invoices/[A-Za-z0-9_-]+/extract$"),
)

# A runaway cursor loop would hammer the source; the ledger's reach is
# bounded at roughly two years, so this is far above any real history.
MAX_PAGES = 200

# The window filters on either the purchase date or the booking date,
# and the choice is part of what a window means. BOOKING is used: it is
# the date the balance moved and the date a billing period is drawn on,
# so a window's edges line up with the invoices captured beside it. Each
# row still carries both dates, so nothing is lost to the choice.
TRANSACTION_DATE_TYPE = "BOOKING"

REQUEST_TIMEOUT_MS = 60_000


def refuse_path(path: str) -> str | None:
    """Why `path` may not be requested, or None when it may.

    An allow-list, because the failure being guarded against is a link
    the source offers that this collector must not take. A blocked-name
    list would have to anticipate every such link; this has to
    anticipate none.
    """
    if not path.startswith("/"):
        return f"not a site-relative path: {path!r}"
    if any(p.match(path) for p in _ALLOWED_PATHS):
        return None
    return f"{path!r} is not a card read endpoint"


class CardApi:
    """Read-only client for the card endpoints, on the browser session.

    Requests go through the Playwright context, so the session cookie
    rides along; the SPA additionally authenticates each call with an
    ``apikey`` header, harvested from the SPA's own traffic rather than
    guessed (see :func:`sniff_apikey`).
    """

    def __init__(self, context, origin: str, apikey: str) -> None:
        self._context = context
        self._origin = origin.rstrip("/")
        self._apikey = apikey

    def _get(self, path_and_query: str, *, accept: str = "application/json"):
        """One guarded GET. Returns the Playwright response.

        The scheme/host check is not redundant with `refuse_path`. The
        ledger's paging cursor is a URL the SERVER composed, and
        `refuse_path` reads only the path component — so an absolute URL
        would present an allowed path while naming a host of the
        server's choosing. Only a site-relative reference is ever
        requested, which is what keeps "follow the link" from meaning
        "follow any link".
        """
        split = urlsplit(path_and_query)
        if split.scheme or split.netloc:
            raise RuntimeError(
                "refusing to request: not a site-relative reference: "
                f"{path_and_query!r}")
        refusal = refuse_path(split.path)
        if refusal is not None:
            raise RuntimeError(f"refusing to request: {refusal}")
        return self._context.request.get(
            self._origin + path_and_query,
            headers={"apikey": self._apikey,
                     "accept": accept,
                     "x-requested-with": "XMLHttpRequest"},
            timeout=REQUEST_TIMEOUT_MS,
        )

    def _get_json(self, path_and_query: str) -> dict:
        resp = self._get(path_and_query)
        if not resp.ok:
            raise RuntimeError(
                f"HTTP {resp.status} for {urlsplit(path_and_query).path}")
        return resp.json()

    def accounts(self) -> dict:
        """The card roster.

        Both `include` flags are passed. They are what the SPA sends when
        the overview is asked to show everything, and without them the
        roster omits inactive and hidden cards — the same silent
        under-collection the homepage anchors cause, one level down.
        """
        return self._get_json(
            "/api/v2/credit-card-accounts?valuationCurrency=CHF"
            "&include=INACTIVES&include=INCLUDE_HIDDEN_CARDS")

    def transactions(self, account_id: str, since: date,
                     until: date) -> tuple[list[dict], bool]:
        """Every ledger page for one card account, and whether it is short.

        Returns `(pages, truncated)`. `truncated` is True when paging
        stopped early — a failed page or the ceiling — so the caller can
        record that the account's ledger is incomplete instead of
        presenting a partial fetch as a whole one.

        The ledger pages with a `next` cursor rather than an offset, so
        the pages are walked by following it. The cursor is a full
        path+query the server composed; it is guarded like any other
        request, which is what keeps "follow the link" from meaning
        "follow any link".
        """
        query = (f"/api/v1/credit-card-transactions"
                 f"?creditCardAccountIds={account_id}"
                 f"&timePeriodFrom={since.isoformat()}"
                 f"&timePeriodTo={until.isoformat()}"
                 f"&transactionDateType={TRANSACTION_DATE_TYPE}")
        pages: list[dict] = []
        for n in range(MAX_PAGES):
            try:
                payload = self._get_json(query)
            except Exception as e:  # noqa: BLE001 — keep what was fetched
                # A page that fails must not discard the pages before
                # it: they are already in hand, and an account with a
                # short ledger is worth more than one with none.
                log.warning("ledger page %d for %s… failed: %s",
                            n + 1, account_id[:12], debugcap.safe_error(e))
                return pages, True
            pages.append(payload)
            nxt = (((payload.get("_links") or {}).get("next") or {})
                   .get("href"))
            if not nxt:
                return pages, False
            query = nxt
            log.debug("ledger page %d for %s…", n + 2, account_id[:12])
        log.warning("ledger paging for %s… hit the %d-page ceiling; "
                    "the capture is short", account_id[:12], MAX_PAGES)
        return pages, True

    def invoices(self, account_id: str) -> dict:
        """The billing periods for one card account."""
        return self._get_json(
            f"/api/v1/credit-card-invoices?creditCardAccountIds={account_id}")

    def invoice(self, invoice_id: str, account_id: str) -> dict:
        """One period's totals — the opening balance and the sums that
        reconcile against it.

        `creditCardAccountId` is required: the SPA never requests this
        endpoint without it, and the bare path does not answer. The
        detail payload carries no account of its own, so the caller's
        account is also what keys the parsed row.
        """
        return self._get_json(
            f"/api/v1/credit-card-invoices/{invoice_id}"
            f"?creditCardAccountId={account_id}")

    def statement_pdf(self, invoice_id: str) -> bytes | None:
        """One period's statement PDF, or None when it does not render."""
        resp = self._get(f"/api/v1/credit-card-invoices/{invoice_id}/extract",
                         accept="application/pdf")
        if not resp.ok:
            log.warning("statement HTTP %d for invoice %s…",
                        resp.status, invoice_id[:12])
            return None
        body = resp.body()
        if not body or not body.startswith(b"%PDF-"):
            log.warning("statement for invoice %s… was not a PDF (%d bytes)",
                        invoice_id[:12], len(body or b""))
            return None
        return body


def sniff_apikey(context) -> dict:
    """Start recording the `apikey` headers the SPA sends; return the tally.

    The key is not a session cookie and not in the page's DOM: it is a
    header the SPA attaches to its own API calls. Reading it off real
    traffic is the only way to have the right one without pinning a value
    that rotates.

    There is more than one. Each micro-frontend carries its own, so the
    tally records which endpoint families each key was seen on and
    :func:`pick_apikey` chooses between them; taking the first key seen
    would hand the card calls whichever widget happened to load first.

    Returns a mapping of key to the set of endpoint families it appeared
    on, which fills in as the SPA loads.
    """
    tally: dict[str, set[str]] = {}

    def on_request(request) -> None:
        try:
            if "/api/" not in request.url:
                return
            value = (request.headers or {}).get("apikey")
            if not value:
                return
            path = urlsplit(request.url).path
            family = "/".join(path.split("/api/", 1)[-1].split("/")[:2])
            tally.setdefault(value, set()).add(family)
        except Exception:  # noqa: BLE001 — a listener must never raise
            return

    context.on("request", on_request)
    return tally


def pick_apikey(tally: dict) -> str | None:
    """The key most likely to authenticate the card endpoints.

    The banking micro-frontends share one key across many endpoint
    families — cards, cash accounts, portfolios, contracts — while a
    single-purpose widget (the quotes feed) carries its own for one
    family. So breadth of coverage identifies the banking key without
    naming any endpoint, which is what keeps this from rotting when UBS
    adds or renames one.

    Ties break towards the key seen first, which is insertion order.
    """
    if not tally:
        return None
    return max(tally, key=lambda k: len(tally[k]))


def capture(api: CardApi, run_dir: Path, since: date, until: date, *,
            statements: bool = True) -> dict:
    """Capture the whole card surface into ``<run_dir>/cards/``.

    Returns the manifest block for ``run.json``. Each account is walked
    independently: one account whose ledger or invoices fail is recorded
    as an error and the rest still land, because a partial capture that
    says which part is missing is worth more than none.
    """
    cards_dir = run_dir / "cards"
    cards_dir.mkdir(parents=True, exist_ok=True)

    roster = api.accounts()
    _write_json(cards_dir / "accounts.json", roster)
    account_ids = card_parsers.account_ids(roster)
    log.info("card roster: %d account(s)", len(account_ids))

    window = f"{since:%Y%m%d}_{until:%Y%m%d}"
    result: dict = {
        "window": {"since": since.isoformat(), "until": until.isoformat()},
        "transaction_date_type": TRANSACTION_DATE_TYPE,
        "roster_file": "accounts.json",
        "accounts": [],
        "errors": [],
    }

    for account_id in account_ids:
        short = bronze.short_token(account_id)
        entry: dict = {"account_short_id": short}
        try:
            pages, truncated = api.transactions(account_id, since, until)
            name = f"transactions_{short}_{window}.json"
            _write_json(cards_dir / name, {"pages": pages})
            entry["transactions_file"] = name
            entry["transaction_pages"] = len(pages)
            entry["transactions_complete"] = not truncated
            if truncated:
                result["errors"].append({"account_short_id": short,
                                         "stage": "transactions-page",
                                         "error": "ledger paging stopped early"})
        except Exception as e:  # noqa: BLE001 — one account must not end the walk
            log.warning("card ledger failed for %s…: %s",
                        short[:8], debugcap.safe_error(e))
            result["errors"].append({"account_short_id": short,
                                     "stage": "transactions",
                                     "error": debugcap.safe_error(e)})
        try:
            entry.update(_capture_invoices(api, cards_dir, account_id, short,
                                           result["errors"],
                                           statements=statements))
        except Exception as e:  # noqa: BLE001
            log.warning("card invoices failed for %s…: %s",
                        short[:8], debugcap.safe_error(e))
            result["errors"].append({"account_short_id": short,
                                     "stage": "invoices",
                                     "error": debugcap.safe_error(e)})
        result["accounts"].append(entry)

    log.info("cards: %d account(s), %d invoice(s), %d statement(s), "
             "%d error(s)",
             len(result["accounts"]),
             sum(a.get("invoice_count", 0) for a in result["accounts"]),
             sum(a.get("statement_count", 0) for a in result["accounts"]),
             len(result["errors"]))
    return result


def _capture_invoices(api: CardApi, cards_dir: Path, account_id: str,
                      short: str, errors: list, *, statements: bool) -> dict:
    """The period list, each period's totals, and each statement PDF.

    Every per-invoice call is isolated: one period that fails costs that
    period, never the account's whole invoice facet. The two index files
    are written from a `finally`, so whatever was fetched is recorded
    even when the walk is cut short — a statement PDF already on disk
    with no entry pointing at it is a file nothing can ever attribute.
    """
    listing = api.invoices(account_id)
    list_name = f"invoices_{short}.json"
    _write_json(cards_dir / list_name, listing)

    details: list[dict] = []
    statement_files: dict[str, str] = {}
    detail_failures = 0
    stmt_dir = cards_dir / "statements"
    invoice_ids = card_parsers.invoice_ids(listing)
    try:
        for invoice_id in invoice_ids:
            try:
                details.append(api.invoice(invoice_id, account_id))
            except Exception as e:  # noqa: BLE001 — one period, not the account
                detail_failures += 1
                log.warning("invoice detail failed for %s…: %s",
                            invoice_id[:12], debugcap.safe_error(e))
                errors.append({"account_short_id": short,
                               "stage": "invoice-detail",
                               "error": debugcap.safe_error(e)})
            if not statements:
                continue
            try:
                body = api.statement_pdf(invoice_id)
            except Exception as e:  # noqa: BLE001 — one PDF, not the account
                log.warning("statement fetch failed for %s…: %s",
                            invoice_id[:12], debugcap.safe_error(e))
                errors.append({"account_short_id": short,
                               "stage": "statement",
                               "error": debugcap.safe_error(e)})
                continue
            if body is None:
                continue
            stmt_dir.mkdir(parents=True, exist_ok=True)
            # Content-addressed, as the eDocuments PDFs are: an unchanged
            # statement lands at the same name every run instead of
            # accreting one per fetch, and identical bytes reached twice
            # write once.
            sha = hashlib.sha256(body).hexdigest()
            out = stmt_dir / f"{sha}.pdf"
            if not (out.exists() and out.stat().st_size == len(body)):
                out.write_bytes(body)
            statement_files[out.name] = invoice_id
    finally:
        detail_name = f"invoice-details_{short}.json"
        _write_json(cards_dir / detail_name, {"invoices": details})
        # Which statement file came from which period. A statement is
        # content-addressed, so its name carries no invoice id, and the
        # `cards/` tree has to be self-describing: the loader reads this
        # dir and nothing else.
        statements_name = f"statements_{short}.json"
        _write_json(cards_dir / statements_name, {"files": statement_files})

    if detail_failures:
        log.warning("%d of %d invoice detail(s) failed for %s… — those "
                    "periods carry no opening balance and cannot reconcile",
                    detail_failures, len(invoice_ids), short[:8])
    return {
        "invoices_file": list_name,
        "invoice_details_file": f"invoice-details_{short}.json",
        "statements_file": f"statements_{short}.json",
        "invoice_count": len(details),
        "invoice_ids_seen": len(invoice_ids),
        "invoice_detail_failures": detail_failures,
        "statement_count": len(statement_files),
    }


def _write_json(path: Path, payload) -> None:
    """Persist a payload whole, atomically.

    Stored as the source returned it — nothing filtered or flattened on
    the way in — so a later parser reads what the API said rather than
    what this pass thought worth keeping.
    """
    bronze.atomic_write_json(path, payload)
