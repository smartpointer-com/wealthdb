#!/usr/bin/env python3
"""American Express bronze download — the whole run, sign-in included.

**`login` folds into this verb** (DESIGN.md §L). Amex's sign-in budget is
small enough that a captcha fires at roughly seven in twenty minutes (§K),
and a separate `login` verb doubled the cost of every routine run for
nothing: the device trust it minted is minted just as well here, by the same
code, on the same profile. So this is the one verb that signs in — it
launches the persistent Camoufox profile, submits the form (a trusted device
is signed in with no challenge, §G), answers a passcode from the terminal
when there is one and registers the device while it is there, and then
fetches the card data over REST via `page.request`, because no data endpoint
carries a bot-defense sensor header (§A). No DOM scraping.

Without a terminal the unattended contract still holds: a challenge raises
`NeedsLogin` and fails loudly rather than blocking on a prompt nobody will
answer. A captcha is never answerable here — `vnc-login` is the verb for
that, and it runs this same walk behind a hand-driven sign-in.

Captured, into a UTC-stamped bronze run dir (the fleet layout):

    <run>/
      run.json                      status manifest (in-progress → complete)
      accounts.json                 the card roster
      activity/<key>.json           the merged activity ledger: every row in
                                    the window, the category code → label
                                    map, and the cycle balance block
      transactions/<key>.<ext>      activity exports (per --format)
      statements/<key>/<date>.pdf   statement PDFs (--no-documents skips)
      statements/<key>/yes-<yr>.pdf year-end summary PDFs
      raw/*.json                    raw roster / statement-archive bodies
                                    (provenance)

The activity JSON is the ledger of record — it alone carries the stable row
id, both charge and post dates, the merchant category, pending rows and the
cycle balances (DESIGN.md §D). The exports are captured as provenance and
for the columns the JSON lacks.

Read-only (CLAUDE.md): only the card accounts are touched (the roster is
filtered to the card product type), and every call is a read — the exports
and document fetches return copies of already-authorized data. Never a
payment, rewards, offers, or settings surface.
"""
from __future__ import annotations

import argparse
import contextlib
import logging
import re
import sys
from datetime import date, datetime, timedelta, timezone
from pathlib import Path

from collectorkit import bronze, cli

import amexclient
import login

log = logging.getLogger("amex.download")

# Safety cap on activity pagination (100 rows/page → 50k rows). A real card
# is far under this; the cap only bounds a runaway loop.
MAX_ACTIVITY_PAGES = 500


def log_id(value: str) -> str:
    """An account key shortened for the LOG.

    The key is a 32-hex account identifier, and terminal output is this
    fleet's primary debugging channel — it gets pasted into chats and issue
    threads. A prefix identifies the account in a run's own output without
    carrying the whole identifier along with it. Files on disk keep the full
    key (safe_stem), because that is what the loader joins on."""
    stem = safe_stem(value)
    return stem if len(stem) <= 12 else stem[:8] + "…"


def safe_stem(value: str) -> str:
    """A filesystem-safe stem from an account key / statement date: keep word
    chars, dash, dot; collapse the rest to '-'; strip leading/trailing dots
    and dashes so no path separator or traversal / dotfile survives. Never
    empty."""
    stem = re.sub(r"[^\w.-]+", "-", str(value)).strip("-.")
    return stem or "item"


def clamp_since(since: date | None, until: date) -> tuple[date | None, bool]:
    """Clamp `since` to the structured channels' 24-month horizon.

    The activity JSON and every export stop there (`maxAvailableMonths`,
    DESIGN.md §E); asking for more returns the capped window silently. The
    fleet rule is to narrow to what the source can honour and **warn** rather
    than either failing or pretending. Returns (since, was_clamped)."""
    if since is None:
        return None, False
    # 30-day months: ~10 days short of 24 calendar months, deliberately —
    # the server caps the window anyway, so erring narrow costs nothing while
    # erring wide would ask for a range it silently truncates.
    floor = until - timedelta(days=amexclient.MAX_AVAILABLE_MONTHS * 30)
    if since < floor:
        return floor, True
    return since, False


def _paginate(context, account_token: str, *, since: date | None,
              until: date | None) -> dict:
    """Fetch every activity row in the window, merging the pages.

    `offset` is 1-based, but whether it counts ROWS or PAGES is not settled
    by the captures — the SPA only ever sent offset=1 (DESIGN.md §D). Rather
    than guess, this walks with a row-based step and checks the result: a
    continuation that returns no row it has not already seen means the step
    was wrong, so it retries that page with the page-based step and keeps
    whichever works, logging which one it settled on. Dedup on the row id
    makes both readings safe regardless.

    Returns the merged ledger plus the category map and balance block."""
    rows: list[dict] = []
    seen: set[str] = set()
    categories: dict = {}
    balances: dict = {}
    total: int | None = None
    limit = amexclient.ACTIVITY_PAGE_SIZE
    offset, page_index, mode = 1, 1, None

    for _ in range(MAX_ACTIVITY_PAGES):
        body_req = amexclient.activity_body(
            account_token, view=amexclient.VIEW_DATE_RANGE,
            since=since, until=until, limit=limit, offset=offset)
        status, body = login.fn_post(
            context, amexclient.FN_ACCOUNT_ACTIVITY, body_req)
        if status != 200 or body is None:
            if not rows:
                raise RuntimeError(
                    f"activity fetch failed (HTTP {status})")
            log.warning("activity page at offset %d: HTTP %s (stopping)",
                        offset, status)
            break
        if total is None:
            total = amexclient.activity_total(body)
            balances = amexclient.activity_balances(body)
        categories.update(amexclient.activity_categories(body))
        page_rows = amexclient.activity_transactions(body)
        new = [t for t in page_rows
               if amexclient.transaction_id(t) not in seen]
        if not new:
            if mode is None and page_index == 2 and page_rows:
                # The first continuation came back with rows already seen,
                # so the row-based step was wrong and the offset counts
                # pages. Retry this same continuation the other way; the
                # dedup above makes the wasted request harmless.
                mode = "pages"
                offset = 2
                continue
            break
        if mode is None and page_index > 1:
            mode = "rows"
        rows.extend(new)
        seen.update(amexclient.transaction_id(t) for t in new)
        if len(page_rows) < limit:
            break
        if isinstance(total, int) and len(rows) >= total:
            break
        page_index += 1
        offset = (page_index if mode == "pages"
                  else (page_index - 1) * limit + 1)
    if mode:
        # Which unit `offset` counts was never settled by a capture, so the
        # first run that needs a continuation is the one that measures it.
        # Say so at INFO and record it in the manifest: burying a measurement
        # at DEBUG means paying for it again.
        log.info("activity pagination: offsets count %s (%d page(s))",
                 "PAGES" if mode == "pages" else "ROWS", page_index)
    return {
        "accountToken": account_token,
        "paginationMode": mode,
        "totalTransactionCount": total,
        "since": since.isoformat() if since else None,
        "until": until.isoformat() if until else None,
        "categories": categories,
        "balancesDetails": balances,
        "transactions": rows,
    }


def _export_account(context, acct: dict, run_dir: Path,
                    formats: tuple[str, ...], *, since: date | None,
                    until: date | None) -> int:
    """Fetch each requested export format for one card over the window.

    The export URL is constructed (every parameter is known) rather than
    lifted from the payload, so the file carries the window this run asked
    for. Returns the number of files written."""
    key = acct["account_key"]
    written = 0
    for fmt in formats:
        url = amexclient.export_url(key, fmt, since=since, until=until)
        try:
            resp = context.request.get(url, headers={"Accept": "*/*"})
            if not 200 <= resp.status < 300:
                log.warning("export %s/%s: HTTP %s",
                            log_id(key), fmt, resp.status)
                continue
            ext = amexclient.EXPORT_EXTENSIONS[fmt]
            out = run_dir / "transactions" / f"{safe_stem(key)}.{ext}"
            bronze.atomic_write_bytes(out, resp.body())
            written += 1
        except Exception as exc:
            log.warning("export %s/%s failed: %r", log_id(key), fmt, exc)
    return written


def _statement_in_window(end_date: str, since: date | None) -> bool:
    """Keep a statement period ending on or after `since` — `--lookback`
    applied to documents. An unparseable date is kept (fail-open)."""
    if since is None:
        return True
    with contextlib.suppress(ValueError):
        return datetime.strptime(end_date, "%Y-%m-%d").date() >= since
    return True


def _fetch_pdf(context, url: str, out: Path) -> bool:
    if not url:
        return False
    try:
        resp = context.request.get(url, headers={"Accept": "*/*"})
        if not 200 <= resp.status < 300:
            log.warning("document %s: HTTP %s", out.name, resp.status)
            return False
        bronze.atomic_write_bytes(out, resp.body())
        return True
    except Exception as exc:
        log.warning("document %s failed: %r", out.name, exc)
        return False


def _download_statements(context, acct: dict, run_dir: Path, *,
                         since: date | None) -> tuple[int, int]:
    """List a card's statement archive and fetch each PDF in the window.

    The archive comes from the activity endpoint's STATEMENTS view, in one
    response, and each period carries its own opaque document URL — which is
    used verbatim, since it cannot be constructed (DESIGN.md §E). Returns
    (statement PDFs, year-end summary PDFs) written."""
    key = acct["account_key"]
    status, body = login.fn_post(
        context, amexclient.FN_ACCOUNT_ACTIVITY,
        amexclient.activity_body(acct["account_token"],
                                 view=amexclient.VIEW_STATEMENTS))
    if status != 200 or body is None:
        log.warning("statement list %s: HTTP %s", log_id(key), status)
        return 0, 0
    bronze.atomic_write_json(
        run_dir / "raw" / f"statements-{safe_stem(key)}.json", body)
    out_dir = run_dir / "statements" / safe_stem(key)
    entries = amexclient.parse_statements(body)
    structured = sum(1 for e in entries if e.has_structured_export)
    log.info("  %s: %d statement period(s), %d with a structured export",
             log_id(key), len(entries), structured)
    pdfs = 0
    for entry in entries:
        if not _statement_in_window(entry.end_date, since):
            continue
        out = out_dir / f"{safe_stem(entry.end_date)}.pdf"
        pdfs += _fetch_pdf(context, entry.pdf_url, out)
    summaries = 0
    for row in amexclient.parse_year_end_summaries(body):
        out = out_dir / f"yes-{safe_stem(str(row['year']))}.pdf"
        summaries += _fetch_pdf(context, row["pdf_url"], out)
    return pdfs, summaries


def build_manifest(status: str, *, accounts: list[dict], counts: dict,
                   since: date | None, until: date | None,
                   formats: tuple[str, ...], clamped: bool,
                   documents_since: date | None,
                   pagination: str | None = None) -> dict:
    """The run.json body. `status` is 'in-progress' at creation, overwritten
    with 'complete' (or 'dry-run') at the end. Account keys only — no
    balances, and no card number beyond the mask the roster already
    carries."""
    return {
        "source": "amex",
        "status": status,
        "created_at": bronze.ts_slug(),
        "since": since.isoformat() if since else None,
        "until": until.isoformat() if until else None,
        "window_clamped": clamped,
        # The documents run on the window as requested, so a clamped run
        # covers a wider span in statements than in activity.
        "documents_since": (documents_since.isoformat()
                            if documents_since else None),
        # Which unit the activity endpoint's `offset` counts, as measured by
        # this run — None when one page sufficed and nothing was measured.
        "pagination_mode": pagination,
        "formats": list(formats),
        "account_keys": [a.get("account_key") for a in accounts],
        "counts": counts,
    }


def walk(context, bronze_dir: Path, *, since: date | None = None,
         until: date | None = None, formats: tuple[str, ...] = (),
         documents: bool = True, dry_run: bool = False) -> dict:
    """Fetch the card data into a fresh bronze run dir over the authenticated
    context, narrowed to the `[since, until]` window (`--lookback`).
    --dry-run enumerates the roster and pages the activity window, but
    writes the roster only — no activity, exports or documents — and stamps
    the manifest 'dry-run'."""
    formats = formats or amexclient.DEFAULT_EXPORT_FORMATS
    until = until or datetime.now(timezone.utc).date()
    # Two windows, not one. The activity JSON and every export stop at the
    # provider's 24-month horizon, so theirs is clamped; the statement
    # archive reaches the provider's full retention, so the documents keep
    # the window as asked for. Clamping both would put the deep backfill
    # (DESIGN.md §E) out of reach of every invocation, which is the only
    # thing that reaches past 24 months.
    documents_since = since if documents else None
    since, clamped = clamp_since(since, until)
    if clamped:
        log.warning("--lookback reaches past the %d-month horizon Amex's "
                    "activity and exports offer; narrowed to %s.%s",
                    amexclient.MAX_AVAILABLE_MONTHS, since,
                    " The statement PDFs reach further and are still fetched."
                    if documents else
                    " --no-documents, so nothing this run reaches further.")

    slug = bronze.ts_slug()
    run_dir = bronze.run_dir(bronze_dir, slug)
    (run_dir / "raw").mkdir(parents=True, exist_ok=True)
    (run_dir / "activity").mkdir(exist_ok=True)
    (run_dir / "transactions").mkdir(exist_ok=True)
    if documents:
        (run_dir / "statements").mkdir(exist_ok=True)

    bronze.atomic_write_json(run_dir / "run.json", build_manifest(
        "in-progress", accounts=[], counts={}, since=since, until=until,
        formats=formats, clamped=clamped,
        documents_since=documents_since))

    status, roster = login.fn_post(context, amexclient.FN_CUSTOMER_OVERVIEW)
    if status != 200 or roster is None:
        raise RuntimeError(f"could not fetch the card roster (HTTP {status})")
    bronze.atomic_write_json(run_dir / "raw" / "overview.json", roster)
    accounts = amexclient.parse_accounts(roster, cards_only=True)
    bronze.atomic_write_json(run_dir / "accounts.json", accounts)
    log.info("roster: %d card account(s)", len(accounts))

    counts = {"transactions": 0, "exports": 0, "statements": 0,
              "year_end_summaries": 0}
    pagination: str | None = None
    for acct in accounts:
        # `stem` names files (the loader joins on the full key); `shown` is
        # the shortened form the log carries.
        stem = safe_stem(acct["account_key"])
        shown = log_id(acct["account_key"])
        if not acct["account_token"]:
            log.warning("account %s has no activity token — skipped", shown)
            continue
        if dry_run:
            merged = _paginate(context, acct["account_token"],
                               since=since, until=until)
            pagination = merged.get("paginationMode") or pagination
            log.info("  [dry-run] %s: %s row(s) in window (total %s); "
                     "would fetch %s exports + statements", shown,
                     len(merged["transactions"]),
                     merged["totalTransactionCount"], "+".join(formats))
            continue
        try:
            merged = _paginate(context, acct["account_token"],
                               since=since, until=until)
        except RuntimeError as exc:
            log.warning("activity %s: %s", shown, exc)
        else:
            pagination = merged.get("paginationMode") or pagination
            bronze.atomic_write_json(
                run_dir / "activity" / f"{stem}.json", merged)
            got = len(merged["transactions"])
            total = merged["totalTransactionCount"]
            counts["transactions"] += got
            if isinstance(total, int) and got < total:
                log.warning("activity %s: fetched %d of %d rows", shown, got,
                            total)
            else:
                log.info("  %s: %d transaction(s)", shown, got)

        counts["exports"] += _export_account(context, acct, run_dir, formats,
                                             since=since, until=until)
        if documents:
            pdfs, summaries = _download_statements(context, acct, run_dir,
                                                   since=documents_since)
            counts["statements"] += pdfs
            counts["year_end_summaries"] += summaries

    status_str = "dry-run" if dry_run else "complete"
    bronze.atomic_write_json(run_dir / "run.json", build_manifest(
        status_str, accounts=accounts, counts=counts, since=since,
        until=until, formats=formats, clamped=clamped,
        documents_since=documents_since, pagination=pagination))
    log.info("download %s: %s", status_str, counts)
    return {"run_dir": str(run_dir), "accounts": len(accounts), **counts}


def resolve_two_factor(cli_mfa: bool | None, *, vnc: bool = False,
                       isatty: bool = False) -> str:
    """Which mode a passcode challenge is answered in.

    Since `login` folded into this verb (DESIGN.md §L), a challenge is a
    routine event on any run rather than an exceptional one — so the default
    is **auto**: drive it from the terminal when stdin is a TTY, and keep the
    unattended contract when it is not, where a cron run fails loudly rather
    than blocking on a prompt nobody will answer.

    `--vnc-mfa` (what the `vnc-login` verb passes) hands the challenge to a
    human in the browser instead; it is the only mode that can answer a
    captcha. `--cli-mfa` / `--no-cli-mfa` force either half of the default.
    """
    if vnc:
        return login.TWOFACTOR_VNC
    if cli_mfa is True:
        return login.TWOFACTOR_CLI
    if cli_mfa is False:
        return login.TWOFACTOR_NONE
    return login.TWOFACTOR_CLI if isatty else login.TWOFACTOR_NONE


def _resolve_formats(raw: list[str] | None) -> tuple[str, ...]:
    """Validate --format handles against amexclient.EXPORT_FORMATS; default
    to DEFAULT_EXPORT_FORMATS. Unknown handles fail loudly (fleet
    convention)."""
    if not raw:
        return amexclient.DEFAULT_EXPORT_FORMATS
    unknown = [f for f in raw if f not in amexclient.EXPORT_FORMATS]
    if unknown:
        raise SystemExit(
            f"unknown export format(s): {', '.join(unknown)} "
            f"(known: {', '.join(sorted(amexclient.EXPORT_FORMATS))})")
    return tuple(raw)


def run_download(args: argparse.Namespace) -> int:
    # Bounded collector: the activity call takes an explicit date range and
    # the export takes start_date/end_date, so `--lookback` narrows at the
    # source (DESIGN.md §E). Past 24 months the structured channels stop and
    # clamp_since warns; the statement PDFs still reach further.
    since, until = cli.resolve_standard(args, verb="download", log=log)
    formats = _resolve_formats(args.format)
    two_factor = resolve_two_factor(args.cli_mfa, vnc=args.vnc_mfa,
                                    isatty=sys.stdin.isatty())
    if two_factor == login.TWOFACTOR_CLI:
        log.info("a passcode challenge, if one fires, will be answered from "
                 "this terminal")
    bronze.ensure_writable_dir(args.bronze_dir)
    with login.camoufox(args.profile_dir, fresh=args.fresh) as (context, page):
        try:
            ok = login.drive_to_auth(context, page, args,
                                     two_factor=two_factor)
        except login.NeedsLogin as exc:
            log.error("%s", exc)
            return 2
        except login.LogonFailed as exc:
            log.error("%s", exc)
            return 1
        if not ok:
            log.error("authentication failed — retry from a terminal, "
                      "which can answer the passcode an expired device "
                      "trust brings back, or `vnc-login` for a captcha")
            return 1
        summary = walk(context, args.bronze_dir, since=since,
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
                        "device-trust cookie). Default: %(default)s.")
    p.add_argument("--bronze-dir", type=Path, required=True,
                   help="Bronze tree root (the wrapper passes /data).")
    p.add_argument("--env-file", type=Path, default=login.DEFAULT_ENV_FILE,
                   help="Bash-sourced env file with the credentials. "
                        "Default: %(default)s.")
    p.add_argument("--format", action="append",
                   help="Export format handle to fetch (repeatable). Known: "
                        f"{', '.join(sorted(amexclient.EXPORT_FORMATS))}. "
                        "Default: "
                        f"{'+'.join(amexclient.DEFAULT_EXPORT_FORMATS)}.")
    p.add_argument("--no-documents", action="store_true",
                   help="Skip the statement-PDF pass (the run's heavy part).")
    p.add_argument("--dry-run", action="store_true",
                   help="Enumerate the roster and activity but export "
                        "nothing; stamp the manifest 'dry-run'.")
    p.add_argument("--cli-mfa", dest="cli_mfa", action="store_true",
                   default=None,
                   help="Answer a passcode challenge from the terminal even "
                        "without a TTY (it will fail fast on EOF). The "
                        "default decides by whether stdin is a TTY.")
    p.add_argument("--no-cli-mfa", dest="cli_mfa", action="store_false",
                   help="Never prompt: a passcode challenge is a hard error. "
                        "The default already does this without a TTY; this "
                        "forces it.")
    p.add_argument("--vnc-mfa", action="store_true",
                   help="Hand a challenge to a human in the browser over VNC "
                        "(what the `vnc-login` verb passes). The only mode "
                        "that can answer a captcha.")
    p.add_argument("--fresh", action="store_true",
                   help="Move the persistent Camoufox profile aside first, so "
                        "the sign-in is treated as an unrecognised device and "
                        "a real passcode fires — the untrusted-device flow. "
                        "Costs a real passcode; never routine.")
    p.add_argument("--mfa-timeout", type=int, default=600,
                   help="Seconds to wait for the authenticated session to "
                        "appear once a challenge has been answered: the "
                        "by-hand VNC window, and the same ceiling after the "
                        "terminal drive submits. The waits for the challenge "
                        "screen itself are fixed, and the terminal prompt "
                        "blocks until answered. Default: %(default)s.")
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
