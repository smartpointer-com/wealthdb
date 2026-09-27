#!/usr/bin/env python3
"""First Citizens bronze download — unattended logon + REST fetch.

`login.py` establishes the device-trust once (interactive 2FA). This verb
runs unattended: it launches the same persistent Camoufox profile, submits
the login form (the trusted device makes `logonUser` return 200 with no
2FA — DESIGN.md §3), and then fetches the deposit-account data over the
Q2 `mobilews` REST API via `page.request` (the data calls carry no Akamai
sensor, only the session cookie + `q2token`, so no DOM scraping is needed).

If the device-trust has expired, `logonUser` returns a 2FA challenge and
authenticate() raises `NeedsLogin` — `download` has no terminal to answer
it, so it fails loudly telling the operator to run `login`.

Captured, into a UTC-stamped bronze run dir (chase's layout):

    <run>/
      run.json                      status manifest (in-progress → complete)
      accounts.json                 deposit roster (hydraProductTypeCode == D)
      history/<id>.json             accountHistory JSON — the richest ledger
                                    (stable transactionId + runningBalance)
      transactions/<id>.<fmt>       activity exports (csv, qfx, … per --format)
      statements/<id>/<period>.pdf  statement PDFs (skipped with --no-documents)
      raw/*.json                    raw roster / listing bodies (provenance)

Read-only (AGENTS.md): only the deposit accounts (checking / savings) are
touched — the roster is filtered to `hydraProductTypeCode == "D"` — and the
only POSTs are the read-only export and statement-PDF triggers. Never a
money-movement, card, or settings surface.
"""
from __future__ import annotations

import argparse
import contextlib
import logging
import re
import sys
from datetime import date, datetime
from pathlib import Path

from collectorkit import bronze, cli

import login
import q2client

log = logging.getLogger("firstcitizens.download")

# Safety cap on history pagination (100 rows/page → 50k transactions). A real
# deposit account is far under this; the cap only bounds a runaway loop.
MAX_HISTORY_PAGES = 500


def _fetch_history(context, account_id: str,
                   posted_date: str | None = None) -> dict | None:
    """Paginate `accountHistory` (newest-first) and merge the pages into one
    dict — `{accountId, transactionCount, oldestTransactionDate,
    transactions: [...]}`. `posted_date` narrows the fetch server-side to the
    `--lookback` window (DESIGN.md §3); without it the account's complete
    history comes back. The JSON is the authoritative ledger (stable
    transactionId + runningBalance per row). Returns None if page 1 fails."""
    all_tx: list = []
    count = oldest = None
    for pg in range(1, MAX_HISTORY_PAGES + 1):
        url = q2client.account_history_page_url(account_id, pg,
                                                posted_date=posted_date)
        st, body = login.q2_get_json(context, url)
        if st != 200 or body is None:
            if pg == 1:
                return None
            log.warning("history %s page %d: HTTP %s (stopping)",
                        account_id, pg, st)
            break
        if count is None:
            d = q2client.history_data(body)
            count = d.get("transactionCount")
            oldest = d.get("oldestTransactionDate")
        txs = q2client.history_transactions(body)
        all_tx.extend(txs)
        if len(txs) < q2client.HISTORY_PAGE_SIZE:
            break
        if isinstance(count, int) and len(all_tx) >= count:
            break
    return {"accountId": account_id, "transactionCount": count,
            "oldestTransactionDate": oldest, "transactions": all_tx}


def safe_stem(value: str) -> str:
    """A filesystem-safe stem from an account id / statement period: keep
    word chars, dash, dot; collapse the rest to '-'; strip leading/trailing
    dots and dashes so no path separator or traversal / dotfile survives.
    Never empty."""
    stem = re.sub(r"[^\w.-]+", "-", str(value)).strip("-.")
    return stem or "item"


def build_manifest(status: str, *, accounts: list[dict], counts: dict,
                   since: date | None, until: date | None,
                   formats: tuple[str, ...]) -> dict:
    """The run.json body. `status` is 'in-progress' at creation, overwritten
    with 'complete' (or 'dry-run') at the end. Account ids only — no balances
    or numbers beyond the already-masked external id the roster carries."""
    return {
        "source": "firstcitizens",
        "status": status,
        "created_at": bronze.ts_slug(),
        "since": since.isoformat() if since else None,
        "until": until.isoformat() if until else None,
        "formats": list(formats),
        "account_ids": [a.get("id") for a in accounts],
        "counts": counts,
    }


def _export_account(context, acct: dict, run_dir: Path,
                    formats: tuple[str, ...],
                    posted_date: str | None = None) -> int:
    """Fetch each requested export format for one account. The POST body is
    multipart carrying only the q2token; `posted_date` narrows the export to
    the `--lookback` window on the URL, matching the history (DESIGN.md §3).
    Returns the number of files written."""
    acct_id = acct["id"]
    # Binary fetch — override the JSON Accept the shared header sets, so the
    # server returns the file bytes rather than a JSON envelope.
    headers = {**login.q2_headers(context), "Accept": "*/*"}
    token = headers.get(q2client.Q2TOKEN, "")
    written = 0
    for fmt in formats:
        url = q2client.account_export_url(acct_id, fmt, posted_date=posted_date)
        try:
            resp = context.request.post(
                url, headers=headers, multipart={q2client.Q2TOKEN: token})
            if not 200 <= resp.status < 300:
                log.warning("export %s/%s: HTTP %s", acct_id, fmt, resp.status)
                continue
            out = run_dir / "transactions" / f"{safe_stem(acct_id)}.{fmt}"
            bronze.atomic_write_bytes(out, resp.body())
            written += 1
        except Exception as exc:
            log.warning("export %s/%s failed: %r", acct_id, fmt, exc)
    return written


def _statement_in_window(period: str, since: date | None) -> bool:
    """Keep a statement whose `period` (MM/DD/YYYY, its end-of-cycle date)
    falls on or after `since` — the `--lookback` window applied to documents
    (the statement list itself is not date-filterable server-side). An
    unparseable period is kept (fail-open)."""
    if since is None:
        return True
    with contextlib.suppress(ValueError):
        return datetime.strptime(period, "%m/%d/%Y").date() >= since
    return True


def _download_statements(context, acct: dict, run_dir: Path,
                         since: date | None = None) -> int:
    """List an account's statements and fetch each PDF within the window
    (DESIGN.md §3). Returns the number of PDFs written."""
    acct_id = acct["id"]
    status, body = login.q2_get_json(
        context, q2client.account_statement_list_url(acct_id))
    if status != 200 or body is None:
        log.warning("statement list %s: HTTP %s", acct_id, status)
        return 0
    bronze.atomic_write_json(
        run_dir / "raw" / f"statements-{safe_stem(acct_id)}.json", body)
    out_dir = run_dir / "statements" / safe_stem(acct_id)
    # The PDF POST requires the q2token as a multipart form field (like the
    # export); a header-only POST 400s (measured 2026-08-14).
    pdf_headers = {**login.q2_headers(context), "Accept": "*/*"}
    token = pdf_headers.get(q2client.Q2TOKEN, "")
    written = 0
    for row in q2client.parse_statement_list(body):
        if not _statement_in_window(row["period"], since):
            continue
        url = q2client.account_statement_pdf_url(acct_id, row["doc_id"])
        try:
            resp = context.request.post(
                url, headers=pdf_headers, multipart={q2client.Q2TOKEN: token})
            if not 200 <= resp.status < 300:
                log.warning("statement pdf %s/%s: HTTP %s",
                            acct_id, row["period"], resp.status)
                continue
            name = f"{safe_stem(row['period'])}.pdf"
            bronze.atomic_write_bytes(out_dir / name, resp.body())
            written += 1
        except Exception as exc:
            log.warning("statement pdf %s/%s failed: %r",
                        acct_id, row["period"], exc)
    return written


def walk(context, page, bronze_dir: Path, *, since: date | None = None,
         until: date | None = None, formats: tuple[str, ...] = (),
         documents: bool = True, dry_run: bool = False) -> dict:
    """Fetch the deposit-account data into a fresh bronze run dir over the
    authenticated context, narrowed to the `[since, until]` window
    (`--lookback`). --dry-run enumerates the roster + statement lists but
    writes no exports/PDFs and stamps the manifest 'dry-run'."""
    formats = formats or q2client.DEFAULT_EXPORT_FORMATS
    # The server-side window carried on the history + export URLs (§3). None
    # when no window was resolved → the account's full history.
    posted_date = (q2client.posted_date_range(since, until)
                   if since and until else None)
    slug = bronze.ts_slug()
    run_dir = bronze.run_dir(bronze_dir, slug)
    (run_dir / "raw").mkdir(parents=True, exist_ok=True)
    (run_dir / "history").mkdir(exist_ok=True)
    (run_dir / "transactions").mkdir(exist_ok=True)
    if documents:
        (run_dir / "statements").mkdir(exist_ok=True)

    bronze.atomic_write_json(run_dir / "run.json", build_manifest(
        "in-progress", accounts=[], counts={}, since=since, until=until,
        formats=formats))

    status, roster = login.q2_get_json(context, q2client.accounts_url())
    if status != 200 or roster is None:
        raise RuntimeError(f"could not fetch the account roster (HTTP {status})")
    bronze.atomic_write_json(run_dir / "raw" / "accounts.json", roster)
    accounts = q2client.parse_accounts(roster, deposit_only=True)
    bronze.atomic_write_json(run_dir / "accounts.json", accounts)
    log.info("roster: %d deposit account(s)", len(accounts))

    counts = {"transactions": 0, "exports": 0, "statements": 0}
    for acct in accounts:
        acct_id = acct["id"]
        if dry_run:
            # Enumerate only — a single page-1 peek (within the window)
            # reports the totals (read-only), but nothing is written.
            _, body = login.q2_get_json(
                context, q2client.account_history_page_url(
                    acct_id, 1, posted_date=posted_date))
            d = q2client.history_data(body or {})
            log.info("  [dry-run] %s: %s transactions (oldest %s); would "
                     "fetch history + %s exports + statements", acct_id,
                     d.get("transactionCount"), d.get("oldestTransactionDate"),
                     "+".join(formats))
            continue
        # The accountHistory JSON is the richest, authoritative ledger — a
        # stable transactionId AND a runningBalance per row (DESIGN.md
        # §3/§4.3). Paginated in full within the window.
        merged = _fetch_history(context, acct_id, posted_date=posted_date)
        if merged is not None:
            bronze.atomic_write_json(
                run_dir / "history" / f"{safe_stem(acct_id)}.json", merged)
            got, total = len(merged["transactions"]), merged["transactionCount"]
            counts["transactions"] += got
            if isinstance(total, int) and got != total:
                log.warning("history %s: fetched %d of %d transactions",
                            acct_id, got, total)
            else:
                log.info("  %s: %d transactions", acct_id, got)
        else:
            log.warning("history %s: fetch failed", acct_id)

        counts["exports"] += _export_account(context, acct, run_dir, formats,
                                             posted_date=posted_date)
        if documents:
            counts["statements"] += _download_statements(
                context, acct, run_dir, since=since)

    status_str = "dry-run" if dry_run else "complete"
    bronze.atomic_write_json(run_dir / "run.json", build_manifest(
        status_str, accounts=accounts, counts=counts, since=since,
        until=until, formats=formats))
    log.info("download %s: %s", status_str, counts)
    return {"run_dir": str(run_dir), "accounts": len(accounts), **counts}


def _resolve_formats(raw: list[str] | None) -> tuple[str, ...]:
    """Validate --format handles against q2client.EXPORT_FORMATS; default to
    DEFAULT_EXPORT_FORMATS. Unknown handles fail loudly (fleet convention)."""
    if not raw:
        return q2client.DEFAULT_EXPORT_FORMATS
    unknown = [f for f in raw if f not in q2client.EXPORT_FORMATS]
    if unknown:
        raise SystemExit(
            f"unknown export format(s): {', '.join(unknown)} "
            f"(known: {', '.join(sorted(q2client.EXPORT_FORMATS))})")
    return tuple(raw)


def run_download(args: argparse.Namespace) -> int:
    # Bounded collector: `accountHistory` and `accountExport` narrow
    # server-side to the `--lookback` window via `postedDate` (DESIGN.md §3),
    # so resolve_standard's (since, until) is honoured at the source. The
    # default window (~90 days) suits routine refreshes; `--lookback all`
    # pulls the account's complete history for the initial backfill.
    since, until = cli.resolve_standard(args, verb="download", log=log)
    formats = _resolve_formats(args.format)
    bronze.ensure_writable_dir(args.bronze_dir)
    with login.camoufox(args.profile_dir) as (context, page):
        try:
            ok = login.drive_to_auth(context, page, args,
                                     two_factor=login.TWOFACTOR_NONE)
        except login.NeedsLogin as exc:
            log.error("%s", exc)
            return 2
        if not ok:
            log.error("authentication failed — retry, or run `login` if the "
                      "device trust has expired")
            return 1
        summary = walk(context, page, args.bronze_dir, since=since,
                       until=until, formats=formats,
                       documents=not args.no_documents, dry_run=args.dry_run)
    log.info("done: %s", summary)
    return 0


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip().splitlines()[0],
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument("--profile-dir", type=Path,
                   default=login.DEFAULT_PROFILE_DIR,
                   help="Persistent Camoufox profile dir (holds the "
                        "device-trust). Default: %(default)s.")
    p.add_argument("--bronze-dir", type=Path, required=True,
                   help="Bronze tree root (the wrapper passes /data).")
    p.add_argument("--env-file", type=Path, default=login.DEFAULT_ENV_FILE,
                   help="Bash-sourced env file with the credentials. "
                        "Default: %(default)s.")
    p.add_argument("--format", action="append",
                   help="Export format handle to fetch (repeatable). Known: "
                        f"{', '.join(sorted(q2client.EXPORT_FORMATS))}. "
                        f"Default: {'+'.join(q2client.DEFAULT_EXPORT_FORMATS)}.")
    p.add_argument("--no-documents", action="store_true",
                   help="Skip the statement-PDF pass (the run's heavy part).")
    p.add_argument("--dry-run", action="store_true",
                   help="Enumerate the roster + statement lists but export "
                        "nothing; stamp the manifest 'dry-run'.")
    p.add_argument("--mfa-timeout", type=int, default=600,
                   help="Unused on the unattended path (kept for a uniform "
                        "authenticate() signature). Default: %(default)s.")
    p.add_argument("--debug", action="store_true",
                   help="Capture login DOM/screenshots into --screenshot-dir.")
    p.add_argument("--screenshot-dir", type=Path, default=Path("/debug"),
                   help="Where login diagnostics land (outside bronze). "
                        "Default: %(default)s.")
    cli.add_standard_args(p, verb="download")
    return p.parse_args(argv)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose or args.debug)
    return run_download(args)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
