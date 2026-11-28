#!/usr/bin/env python3
"""George bronze fetch — the REST walk over a signed-in session.

The sign-in and the fetch run in one browser lifetime (DESIGN.md §F): the
wrapper's `download` verb is `login.py --bronze-dir /data`, which reuses a
live session or signs in, then calls :func:`walk`
here with a `login.Api` (the browser context's `request` API plus the
bearer). This module does no DOM scraping.

Captured, into a UTC-stamped bronze run dir:

    <run>/
      run.json                       status manifest (in-progress → complete)
      accounts.json                  checking roster rows (§C)
      cards.json                     credit-card ids: card, shadow account,
                                     product (§G)
      history/<product>.json         transaction rows, newest first, with
                                     the paging facts (§C)
      statements/<account>/listing.json + <date>_<id>.pdf   (§E)
      invoices/<product>/listing.json + <date>_<billId>.pdf  (§G)
      raw/*.json                     the roster bodies as served

A product is a checking account (its id) or a credit card
(`<shadowAccountId>-<cardId>`).

Read-only (AGENTS.md): every call is a GET except the two document
downloads, whose POST body carries only the bearer.
"""
from __future__ import annotations

import logging
from dataclasses import dataclass
from datetime import date
from pathlib import Path

from collectorkit import bronze, debugcap

import george_client as george

log = logging.getLogger("erstebank_at.download")

MAX_ROSTER_PAGES = 20
MAX_HISTORY_PAGES = 400                      # 400 * 50 = 20 000 rows
MAX_STATEMENT_PAGES = 20
# A statement renders on demand (§E): the download answers 422 ASYNCHRON
# until the file is ready, a few seconds later. Asked again this often,
# this far apart, as the SPA does when the button is pressed again.
RENDER_ATTEMPTS = 6
RENDER_WAIT_MS = 3000
# Statements still rendering after their attempts are asked for again once,
# after the rest of the run and this wait.
LATE_RETRY_WAIT_MS = 30_000


def build_manifest(status: str, *, accounts: list[str], cards: list[str],
                   counts: dict, since: date | None,
                   until: date | None) -> dict:
    """The run.json body. `status` is 'in-progress' at creation, then
    'complete' or 'dry-run'. Product ids only — no balances or names."""
    return {
        "source": "erstebank_at",
        "status": status,
        "created_at": bronze.ts_slug(),
        "since": since.isoformat() if since else None,
        "until": until.isoformat() if until else None,
        "account_ids": accounts,
        "card_products": cards,
        "counts": counts,
    }


def fetch_roster(api, run_dir: Path) -> list[dict]:
    """Every checking-roster page, merged. A page that is not the one asked
    for ends the walk with a warning, since the roster would repeat."""
    rows: list[dict] = []
    for page in range(MAX_ROSTER_PAGES):
        status, body = api.get_json(george.accounts_url(page))
        if status != 200 or not isinstance(body, dict):
            raise RuntimeError(f"could not read the account roster page "
                               f"{page} (HTTP {status})")
        bronze.atomic_write_json(run_dir / "raw" / f"accounts-{page}.json",
                                 body)
        if page and not george.page_number_is(body, page):
            log.warning("the roster ignored page=%d; keeping the pages "
                        "before it", page)
            break
        rows.extend(george.roster_accounts(body))
        if not george.page_has_more(body, page):
            break
    return rows


def fetch_history(api, product_id: str, since: date | None) -> dict | None:
    """Page a product's history, newest first, until the last page or a
    page that reaches before `since`. `complete` is true only then: a
    failed later page or the page cap leave the oldest part of the window
    unread, and the loader must not treat it as covered. Returns None if
    the first page fails."""
    rows: list[dict] = []
    complete = False
    total = None
    for page in range(MAX_HISTORY_PAGES):
        status, body = api.get_json(george.transactions_url(product_id, page))
        if status != 200 or not isinstance(body, dict):
            if page == 0:
                return None
            log.warning("history %s page %d: HTTP %s (stopping)",
                        bronze.log_id(product_id), page, status)
            break
        if total is None:
            total = body.get("totalElements")
        page_rows = george.history_rows(body)
        rows.extend(page_rows)
        oldest = george.oldest_booking(page_rows)
        if (not page_rows or not george.history_has_more(body, page)
                or (since is not None and oldest is not None
                    and oldest < since)):
            complete = True
            break
    else:
        log.warning("history %s: stopped at the %d-page cap",
                    bronze.log_id(product_id), MAX_HISTORY_PAGES)
    return {"product_id": product_id,
            "since": since.isoformat() if since else None,
            "complete": complete,
            "total_elements": total,
            "transactions": rows}


RENDERING = "rendering"


def download_document(api, url: str, pump) -> bytes | str | None:
    """POST a document download and return its bytes, `RENDERING` while
    the server still renders it, or None on any other answer. A `422
    ASYNCHRON` answer is the rendering one (§E): the same call is repeated
    after a short wait, a bounded number of times."""
    for attempt in range(RENDER_ATTEMPTS):
        try:
            status, body, error = api.post_token_form(url)
        except Exception as exc:
            # safe_error, never %r: a Playwright call log repeats the
            # request headers, the bearer included.
            log.debug("document failed: %s", debugcap.safe_error(exc))
            return None
        if 200 <= status < 300 and body:
            return body
        if not george.is_render_pending(status, error):
            log.debug("document: HTTP %s", status)
            return None
        if attempt + 1 < RENDER_ATTEMPTS:
            pump(RENDER_WAIT_MS)
    return RENDERING


@dataclass(frozen=True)
class Document:
    """One PDF to fetch: where from, where to, and which count it adds to."""
    url: str
    path: Path
    kind: str                                # 'statements' / 'card_statements'


def _doc_path(out_dir: Path, day, key: str) -> Path:
    stamp = day.isoformat() if isinstance(day, date) else "undated"
    return out_dir / (bronze.safe_stem(f"{stamp}_{key}") + ".pdf")


def statement_documents(api, account_id: str, out_dir: Path,
                        since: date | None) -> list[Document]:
    """Write a checking account's statement listing and return the PDFs in
    the window."""
    listing: list[dict] = []
    for page in range(MAX_STATEMENT_PAGES):
        status, body = api.get_json(george.statements_url(account_id, page))
        if status != 200 or not isinstance(body, dict):
            log.warning("statements %s page %d: HTTP %s",
                        bronze.log_id(account_id), page, status)
            break
        if page and not george.page_number_is(body, page):
            break
        listing.extend(george.statement_rows(body))
        if not george.page_has_more(body, page):
            break
    bronze.atomic_write_json(out_dir / "listing.json", listing)
    return [Document(
        george.statement_download_url(account_id, str(row["id"])),
        _doc_path(out_dir, george.iso_day(row.get("statementDate")),
                  str(row["id"])),
        "statements")
        for row in listing
        if row.get("id") and george.statement_in_window(row, since)]


def invoice_documents(api, card: dict, out_dir: Path,
                      since: date | None) -> list[Document]:
    """Write a credit card's statement listing and return the PDFs in the
    window."""
    status, body = api.get_json(george.invoices_url(card["product_id"]))
    if status != 200:
        log.warning("card statements %s: HTTP %s",
                    bronze.log_id(card["product_id"]), status)
        return []
    listing = george.invoice_rows(body)
    bronze.atomic_write_json(out_dir / "listing.json", listing)
    return [Document(
        george.invoice_pdf_url(card["card_id"], str(row["billId"])),
        _doc_path(out_dir, george.iso_day(row.get("billingDate")),
                  str(row["billId"])),
        "card_statements")
        for row in listing
        if row.get("billId") and george.invoice_in_window(row, since)]


def download_documents(api, docs: list[Document], counts: dict,
                       pump) -> None:
    """Download `docs`, adding to `counts`. A statement still rendering
    after its attempts is asked for once more at the end, after
    `LATE_RETRY_WAIT_MS`: a statement never downloaded before can take
    longer to render than the attempts span (§E)."""
    rendering: list[Document] = []
    failed = 0
    for doc in docs:
        data = download_document(api, doc.url, pump)
        if data == RENDERING:
            rendering.append(doc)
        elif data is None:
            failed += 1
        else:
            bronze.atomic_write_bytes(doc.path, data)
            counts[doc.kind] += 1
    if rendering:
        log.info("%d statement(s) still rendering — asking again in %ds",
                 len(rendering), LATE_RETRY_WAIT_MS // 1000)
        pump(LATE_RETRY_WAIT_MS)
        for doc in rendering:
            data = download_document(api, doc.url, pump)
            if isinstance(data, bytes):
                bronze.atomic_write_bytes(doc.path, data)
                counts[doc.kind] += 1
            else:
                failed += 1
    if failed:
        log.warning("%d statement PDF(s) could not be downloaded; the next "
                    "run asks again", failed)
    counts["documents_failed"] = failed


def walk(api, bronze_dir: Path, *, since: date | None = None,
         until: date | None = None, documents: bool = True,
         dry_run: bool = False, pump=lambda ms: None) -> dict:
    """Fetch the checking accounts and credit cards into a fresh bronze run
    dir, history narrowed to `[since, until]` (`--lookback`). --dry-run
    reads the rosters only and stamps the manifest 'dry-run'. `pump` waits
    `ms` milliseconds while keeping the browser's event loop running."""
    run_dir = bronze.run_dir(bronze_dir, bronze.ts_slug())
    (run_dir / "raw").mkdir(parents=True, exist_ok=True)
    bronze.atomic_write_json(run_dir / "run.json", build_manifest(
        "in-progress", accounts=[], cards=[], counts={}, since=since,
        until=until))

    accounts = fetch_roster(api, run_dir)
    bronze.atomic_write_json(run_dir / "accounts.json", accounts)
    status, card_body = api.get_json(george.cards_url())
    if status != 200 or not isinstance(card_body, dict):
        raise RuntimeError(f"could not read the card roster (HTTP {status})")
    bronze.atomic_write_json(run_dir / "raw" / "cards.json", card_body)
    cards = george.credit_cards(card_body)
    bronze.atomic_write_json(run_dir / "cards.json", cards)
    account_ids = [str(a["id"]) for a in accounts if a.get("id")]
    card_products = [c["product_id"] for c in cards]
    log.info("roster: %d checking account(s), %d credit card(s)",
             len(account_ids), len(cards))

    counts = {"transactions": 0, "statements": 0, "card_statements": 0,
              "documents_failed": 0, "incomplete_histories": 0}
    if dry_run:
        log.info("[dry-run] would fetch history%s for each product",
                 " + statements" if documents else "")
    else:
        products = ([(a, "account") for a in account_ids]
                    + [(c, "card") for c in card_products])
        for product_id, kind in products:
            history = fetch_history(api, product_id, since)
            if history is None:
                log.warning("history %s: fetch failed",
                            bronze.log_id(product_id))
                counts["incomplete_histories"] += 1
                continue
            history["kind"] = kind
            bronze.atomic_write_json(
                run_dir / "history" / f"{bronze.safe_stem(product_id)}.json",
                history)
            counts["transactions"] += len(history["transactions"])
            if not history["complete"]:
                counts["incomplete_histories"] += 1
            log.info("  %s %s: %d transaction(s)", kind,
                     bronze.log_id(product_id), len(history["transactions"]))
        docs: list[Document] = []
        for account_id in account_ids:
            docs += statement_documents(
                api, account_id,
                run_dir / "statements" / bronze.safe_stem(account_id), since)
        for card in cards:
            docs += invoice_documents(
                api, card,
                run_dir / "invoices" / bronze.safe_stem(card["product_id"]),
                since)
        if documents:
            download_documents(api, docs, counts, pump)

    status_str = "dry-run" if dry_run else "complete"
    bronze.atomic_write_json(run_dir / "run.json", build_manifest(
        status_str, accounts=account_ids, cards=card_products, counts=counts,
        since=since, until=until))
    log.info("download %s: %s", status_str, counts)
    return {"run_dir": str(run_dir), **counts}
