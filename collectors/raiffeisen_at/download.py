#!/usr/bin/env python3
"""Mein ELBA bronze fetch — the REST walk over an authenticated session.

Mein ELBA fires a pushTAN at every sign-in (DESIGN.md §F), so login and the
fetch run in one browser lifetime: the wrapper's `download` verb is
`login.py --bronze-dir /data`, which authenticates (§A) and then calls
:func:`walk` here. This module owns the data pass and its projections; it
does no DOM scraping — the deposit data is a clean per-widget REST/JSON API
replayed over the browser context's `request` API with the harvested OIDC
Bearer (DESIGN.md §C).

Captured, into a UTC-stamped bronze run dir (the firstcitizens layout):

    <run>/
      run.json                     status manifest (in-progress → complete)
      accounts.json                deposit roster (produkte, type == KONTO)
      details/<iban>.json          account-information detail (type, currency,
                                   institution/BIC, interest rates)
      history/<iban>.json          full kontoumsaetze ledger (stable id per row)
      balances/<iban>.json         kontostaende daily closing-balance series
      statements/<iban>/*.pdf      Kontoauszug PDFs (skipped with --no-documents)
      raw/*.json                   raw roster / listing bodies (provenance)

Read-only (CLAUDE.md): only the deposit accounts (checking / savings) are
touched — the roster is filtered to `type == "KONTO"` — and the only POSTs
are the read-only history search, the document filter, and the document
download. Never a money-movement, card, or settings surface.
"""
from __future__ import annotations

import logging
import re
import sys
from datetime import date
from pathlib import Path

from collectorkit import bronze

import elba_client as elba
# login provides the browser-session REST helpers (api_get_json / api_post_json
# / api_headers) the walk fetches through. The dependency is one-way at import
# time — login imports download only lazily, inside its fetch path — so a
# module-level import here is safe.
import login

log = logging.getLogger("raiffeisen_at.download")


def safe_stem(value: str) -> str:
    """A filesystem-safe stem from an IBAN / document id: keep word chars,
    dash, dot; collapse the rest to '-'; strip leading/trailing dots and
    dashes so no path separator or traversal / dotfile survives. Never
    empty."""
    stem = re.sub(r"[^\w.-]+", "-", str(value)).strip("-.")
    return stem or "item"


def build_manifest(status: str, *, accounts: list[dict], counts: dict,
                   since: date | None, until: date | None) -> dict:
    """The run.json body. `status` is 'in-progress' at creation, overwritten
    with 'complete' (or 'dry-run') at the end. IBANs only — no balances or
    counterparties (the roster's own account key)."""
    return {
        "source": "raiffeisen_at",
        "status": status,
        "created_at": bronze.ts_slug(),
        "since": since.isoformat() if since else None,
        "until": until.isoformat() if until else None,
        "account_ibans": [a.get("iban") for a in accounts],
        "counts": counts,
    }


def _fetch_history(context, watch, iban: str,
                   since: date | None) -> dict | None:
    """Paginate `kontoumsaetze` (newest-first, keyset cursor) and merge the
    pages into `{iban, minBuchungstag, complete, transactions: [...]}`
    (DESIGN.md §C). `since` floors the window server-side via `buchungVon`.
    `complete` is true only when the walk reached the last page — a later
    page's failure, a missing cursor or the page cap leave the oldest days
    of the window unfetched, and the loader must not treat them as covered
    (DESIGN.md §I). Returns None if page 1 fails."""
    all_tx: list = []
    cursor = None
    min_buchungstag = None
    complete = False
    for pg in range(1, elba.MAX_HISTORY_PAGES + 1):
        body_req = elba.kontoumsaetze_body(iban, buchung_von=since,
                                           cursor=cursor)
        st, body = login.api_post_json(context, watch,
                                       elba.kontoumsaetze_url(), body_req)
        if st != 200 or not isinstance(body, dict):
            if pg == 1:
                return None
            log.warning("history %s page %d: HTTP %s (stopping)",
                        safe_stem(iban), pg, st)
            break
        rows = elba.umsaetze_list(body)
        all_tx.extend(rows)
        if min_buchungstag is None:
            min_buchungstag = elba.umsaetze_min_buchungstag(body)
        if not elba.umsaetze_has_more(body) or not rows:
            complete = True
            break
        cursor = elba.next_cursor(rows[-1])
        if cursor is None:
            log.warning("history %s: no pagination cursor on page %d "
                        "(stopping)", safe_stem(iban), pg)
            break
    return {"iban": iban, "minBuchungstag": min_buchungstag,
            "complete": complete, "transactions": all_tx}


def _fetch_details(context, watch, iban: str) -> object:
    """Fetch the account-information detail (`konten/<iban>/details`, §C):
    account type, currency, holding institution + BIC, and interest rates —
    attributes the roster doesn't carry. Returns the parsed body, or None on
    failure."""
    st, body = login.api_get_json(context, watch, elba.konto_details_url(iban))
    if st != 200:
        log.warning("details %s: HTTP %s", safe_stem(iban), st)
        return None
    return body


def _fetch_balances(context, watch, iban: str, since: date,
                    until: date) -> object:
    """Fetch the `kontostaende` daily closing-balance series for the window
    (DESIGN.md §C). Returns the parsed body, or None on failure."""
    st, body = login.api_get_json(
        context, watch, elba.kontostaende_url(iban, since, until))
    if st != 200:
        log.warning("balances %s: HTTP %s", safe_stem(iban), st)
        return None
    return body


DOC_PAGE_LIMIT = 50
MAX_DOC_PAGES = 50                          # cap: 50 * 50 = 2500 documents


def _fetch_document_archive(context, watch) -> list:
    """Page the whole document archive once (DESIGN.md §E). The filter is not
    per-account — every account's documents come back and are filtered
    client-side by IBAN — so this is fetched once per run, not per account.
    The listing is not date-capped server-side; page until a short page or
    the safety cap."""
    all_docs: list = []
    skip = 0
    for _ in range(MAX_DOC_PAGES):
        st, body = login.api_post_json(
            context, watch, elba.dokumente_filter_url(),
            elba.dokumente_filter_body(skip=skip, limit=DOC_PAGE_LIMIT))
        if st != 200 or not isinstance(body, list):
            log.warning("document filter (skip %d): HTTP %s", skip, st)
            break
        all_docs.extend(body)
        if len(body) < DOC_PAGE_LIMIT:
            break
        skip += DOC_PAGE_LIMIT
    return all_docs


def _download_statements(context, watch, all_docs: list, iban: str,
                         run_dir: Path, since: date | None) -> int:
    """Download this account's Kontoauszug PDFs on or after `since` from the
    already-fetched archive `all_docs` (DESIGN.md §E). Returns the number of
    PDFs written.

    The download URL carries the document's `versionsId` when it has one
    (the older KDM-system Kontoauszüge; the newer EAZ ones have none) — the
    fix for the KDM 422s, DESIGN.md §E. A failure is still reported as one
    aggregate line per account rather than a wall of warnings."""
    rows = elba.parse_statements(all_docs, iban, since=since)
    out_dir = run_dir / "statements" / safe_stem(iban)
    headers = {**login.api_headers(watch), "Accept": "*/*"}
    written = 0
    failed_by_system: dict[str, int] = {}
    for row in rows:
        url = elba.dokument_download_url(
            row["system_id"], row["dokument_id"], row.get("version_id"))
        try:
            resp = context.request.post(url, headers=headers, data={})
            if not 200 <= resp.status < 300:
                failed_by_system[row["system_id"]] = (
                    failed_by_system.get(row["system_id"], 0) + 1)
                continue
            # Stable, dedupable name: created-date + the (systemId, docId,
            # versionId) key.
            ver = row.get("version_id")
            key = f"{row['system_id']}_{row['dokument_id']}"
            if ver not in (None, ""):
                key += f"_v{ver}"
            stem = safe_stem(f"{row['created'][:10]}_{key}")
            bronze.atomic_write_bytes(out_dir / f"{stem}.pdf", resp.body())
            written += 1
        except Exception as exc:
            failed_by_system[row["system_id"]] = (
                failed_by_system.get(row["system_id"], 0) + 1)
            log.debug("statement %s/%s failed: %r", safe_stem(iban),
                      row["dokument_id"], exc)
    if failed_by_system:
        total = sum(failed_by_system.values())
        log.warning("%s: %d statement(s) could not be downloaded (by system: "
                    "%s)", safe_stem(iban), total, failed_by_system)
    return written


def walk(context, watch, bronze_dir: Path, *, since: date | None = None,
         until: date | None = None, documents: bool = True,
         dry_run: bool = False) -> dict:
    """Fetch the deposit-account data into a fresh bronze run dir over the
    authenticated context (`context.request` + the harvested Bearer),
    narrowed to the `[since, until]` window (`--lookback`). --dry-run
    enumerates the roster but writes no history/balances/PDFs and stamps the
    manifest 'dry-run'."""
    slug = bronze.ts_slug()
    run_dir = bronze.run_dir(bronze_dir, slug)
    (run_dir / "raw").mkdir(parents=True, exist_ok=True)
    (run_dir / "history").mkdir(exist_ok=True)
    (run_dir / "balances").mkdir(exist_ok=True)
    (run_dir / "details").mkdir(exist_ok=True)
    if documents:
        (run_dir / "statements").mkdir(exist_ok=True)

    bronze.atomic_write_json(run_dir / "run.json", build_manifest(
        "in-progress", accounts=[], counts={}, since=since, until=until))

    status, roster = login.api_get_json(context, watch, elba.produkte_url())
    if status != 200 or not isinstance(roster, list):
        raise RuntimeError(f"could not fetch the product roster (HTTP {status})")
    bronze.atomic_write_json(run_dir / "raw" / "produkte.json", roster)
    accounts = elba.parse_produkte(roster, deposit_only=True)
    bronze.atomic_write_json(run_dir / "accounts.json", accounts)
    log.info("roster: %d deposit account(s)", len(accounts))

    # The document archive is fetched once (it is not per-account) and
    # filtered per IBAN below.
    all_docs: list = []
    if documents and not dry_run:
        all_docs = _fetch_document_archive(context, watch)
        bronze.atomic_write_json(run_dir / "raw" / "documents.json", all_docs)

    counts = {"transactions": 0, "details": 0, "balances": 0, "statements": 0}
    for acct in accounts:
        iban = acct["iban"]
        stem = safe_stem(iban)
        if dry_run:
            log.info("  [dry-run] %s: would fetch details + history + "
                     "balances%s", stem,
                     " + statements" if documents else "")
            continue

        details = _fetch_details(context, watch, iban)
        if details is not None:
            bronze.atomic_write_json(
                run_dir / "details" / f"{stem}.json", details)
            counts["details"] += 1

        merged = _fetch_history(context, watch, iban, since)
        if merged is not None:
            bronze.atomic_write_json(
                run_dir / "history" / f"{stem}.json", merged)
            n = len(merged["transactions"])
            counts["transactions"] += n
            log.info("  %s: %d transaction(s)", stem, n)
        else:
            log.warning("history %s: fetch failed", stem)

        if since and until:
            bal = _fetch_balances(context, watch, iban, since, until)
            if bal is not None:
                bronze.atomic_write_json(
                    run_dir / "balances" / f"{stem}.json", bal)
                counts["balances"] += 1

        if documents:
            counts["statements"] += _download_statements(
                context, watch, all_docs, iban, run_dir, since)

    status_str = "dry-run" if dry_run else "complete"
    bronze.atomic_write_json(run_dir / "run.json", build_manifest(
        status_str, accounts=accounts, counts=counts, since=since, until=until))
    log.info("download %s: %s", status_str, counts)
    return {"run_dir": str(run_dir), "accounts": len(accounts), **counts}


def main(argv: list[str]) -> int:
    """Standalone entry: the full login + fetch, delegating to login.py (the
    session is minted inside the same browser lifetime — §F). The wrapper's
    `download` verb routes to login.py directly; this makes `python
    download.py …` behave identically."""
    return login.main(["--bronze-dir", "/data", *argv])


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
