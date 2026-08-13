#!/usr/bin/env python3
"""chase bronze scrape — the `walk()` half of the one-shot download.

`login.py` establishes an authenticated Camoufox `page` (Chase tears the
session down when Firefox closes, so login + scrape happen in one browser
lifetime — the schwab-web model) and hands it here. `walk()` captures, into
a UTC-stamped bronze run dir:

    <run>/
      run.json                      status manifest (in-progress → complete)
      accounts.json                 roster parsed from the download-options /svc/ call
      raw/*.json                    raw /svc/ response bodies (provenance)
      transactions/<ext_id>.csv     activity export — per-row running balance
      transactions/<ext_id>.qfx     activity export — stable FITID (DESIGN.md §E)
      statements/<ext_id>/*.pdf     statement PDFs (skipped with --no-documents)

The scrape drives the account UI the way the discovery sessions did
(DESIGN.md §C–§E): the browser holds the session cookie and sets the
`x-jpmc-*` / CSRF headers, so exports are triggered through the UI rather
than by replaying the raw `/svc/` POST. The account roster is read from the
`account/activity/download/options/list` response the download page fetches.

The selectors and routes are validated live (DESIGN.md §E/§C); the browserless
helpers (run layout, the export window, filename sanitising, the manifest) are
unit-tested.

Read-only: this only navigates, filters, and exports. Per CLAUDE.md it never
touches Pay & transfer / Zelle / card / settings, and only the deposit
accounts (checking / savings), never the credit cards.
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
ROUTE_TX_DOWNLOAD = f"{BASE}/web/auth/dashboard#/dashboard/transactions/downloads/{{ext}}/DDA/CHK"
ROUTE_DOCUMENTS = f"{BASE}/web/auth/dashboard#/dashboard/documents/myDocs/index;documentType=STATEMENTS"

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
# "All transactions" = Chase's widest export window for this control (last
# 24 months); its option data-testid begins with this, label as shown.
OPT_ACTIVITY_ALL = "showing-activity-select-all transactions"
OPT_ACTIVITY_ALL_LABEL = "All transactions"
# The download-transactions page's trigger is <button id="downloadButton">
# (data-testid "downloadButton"); it is enabled with the default selection.
SEL_DOWNLOAD_BUTTON = "#downloadButton"

# The /svc/ endpoints that carry the deposit-account roster (DESIGN.md §B/§E).
# The overview fires account/detail/dda/list per account; the download page
# fires account/activity/download/{options,dda}/list. Captured by URL so the
# roster survives a response-body shape that the key heuristics don't predict.
ROSTER_URL_RE = re.compile(
    r"/account/detail/dda/list"
    r"|/account/activity/download/(?:options|dda)/list")

# Statements & documents centre (DESIGN.md §C). Classic (non-MDS) light-DOM:
# a per-doc-type accordion over a plain table, a year filter, and a per-row
# "Save as PDF" dropdown. All ids are stable and light-DOM, so plain clicks
# work (no shadow piercing). {n} is the 0-based row index.
SEL_STATEMENTS_ACCORDION = "#button-accountsAccordian-STATEMENTS"
SEL_STATEMENTS_YEAR_FILTER = "#header-filterstyledselect-0"
STMT_DATE_CELL = "#accountsTable-STATEMENTS-row{n}-cell0"
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
    roster) to deposit accounts. Only DDA (checking/savings) rows are kept —
    the credit-card (CARD) rows are out of scope (CLAUDE.md / DESIGN.md §4).
    Never records the full number; keeps Chase's own last-4 `mask`."""
    out = []
    for opt in (body.get("downloadAccountActivityOptions") or []):
        if str(opt.get("summaryType", "")).upper() != "DDA":
            continue
        out.append({
            "account_external_id": str(opt.get("accountId", "")),
            "account_type": opt.get("detailType"),      # 'CHK', 'SAV', …
            "nickname": opt.get("nickName"),
            "mask": _mask(opt.get("mask")),
            "currency": "USD",
        })
    return out


def parse_account_detail(body: dict) -> list[dict]:
    """Reduce an `account/detail/dda/list` response — one deposit account —
    to a roster record. This is the /svc/ call the overview fires per account
    (DESIGN.md §B), carrying accountId + nickname + mask + detail.detailType +
    balance. Returns 0 or 1 record."""
    if not isinstance(body, dict) or "accountId" not in body:
        return []
    detail = body.get("detail") or {}
    balance = detail.get("presentBalance")
    if balance is None:
        balance = detail.get("available")
    return [{
        "account_external_id": str(body.get("accountId", "")),
        "account_type": detail.get("detailType"),       # 'CHK', 'SAV', …
        "nickname": body.get("nickname"),
        "mask": _mask(body.get("mask")),
        "currency": "USD",
        "balance": balance,
    }]


def collect_accounts(bodies: list) -> list[dict]:
    """Build the deposit-account roster from the captured /svc/ bodies, from
    either the per-account `account/detail/dda/list` shape or the all-accounts
    `download/options/list` shape, deduped by account id. The download-options
    record wins on overlap (it is the authoritative downloadable set), so it
    is applied last."""
    by_id: dict[str, dict] = {}
    for body in bodies:
        for rec in parse_account_detail(body):
            if rec["account_external_id"]:
                by_id[rec["account_external_id"]] = rec
    for body in bodies:
        for rec in parse_download_options(body):
            if rec["account_external_id"]:
                # keep a balance already discovered from account-detail.
                rec.setdefault("balance", by_id.get(
                    rec["account_external_id"], {}).get("balance"))
                by_id[rec["account_external_id"]] = rec
    return list(by_id.values())


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


def build_manifest(status: str, *, accounts: list[dict], counts: dict,
                   since: date | None, until: date | None,
                   dry_run: bool, documents: bool) -> dict:
    """The run.json body. `status` is 'in-progress' at creation, overwritten
    with 'complete' (or 'dry-run') at the end."""
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
                captured_bodies.append(body)
                raw_seq["n"] += 1
                bronze.atomic_write_json(
                    run_dir / "raw" / f"account-{raw_seq['n']:02d}.json", body)
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
    page.goto(ROUTE_OVERVIEW, wait_until="domcontentloaded",
              timeout=nav_timeout_ms)

    accounts = _discover_accounts(page, captured_bodies, nav_timeout_ms)
    log.info("discovered %d deposit account(s)", len(accounts))
    # The roster the loader reads (bronze contract): [{account_external_id,
    # account_type, nickname, mask, currency, balance}].
    bronze.atomic_write_json(run_dir / "accounts.json", accounts)

    counts = {"transactions_files": 0, "statements": 0}
    for acct in accounts:
        if dry_run:
            log.info("  [dry-run] would export CSV+QFX for %s",
                     acct.get("mask") or acct["account_external_id"])
            continue
        counts["transactions_files"] += _export_account(
            page, acct, run_dir, since, until, nav_timeout_ms)

    if documents and not dry_run:
        counts["statements"] = _download_statements(
            page, accounts, run_dir, since, until, nav_timeout_ms)

    status = "dry-run" if dry_run else "complete"
    bronze.atomic_write_json(run_dir / "run.json", build_manifest(
        status, accounts=accounts, counts=counts, since=since, until=until,
        dry_run=dry_run, documents=documents))
    log.info("scrape %s: %s", status, counts)
    return {"run_dir": str(run_dir), "accounts": len(accounts), **counts}


def _discover_accounts(page, captured_bodies: list, timeout_ms: int) -> list[dict]:
    """Build the deposit-account roster from the /svc/ calls the overview
    fires (DESIGN.md §B): `account/detail/dda/list` per account, and — when
    reachable — the all-accounts `download/options/list`. The caller has
    already navigated to the overview; settle for the XHRs, then reduce."""
    _settle(page)
    roster = collect_accounts(captured_bodies)
    # After login the SPA is already mounted, so the same-route goto that got
    # us here won't re-fire the overview's per-account XHRs. A reload does —
    # it replays account/detail/dda/list, which the capture hook records.
    if not roster:
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
            page.goto(ROUTE_TX_DOWNLOAD.format(
                          ext=roster[0]["account_external_id"]),
                      wait_until="domcontentloaded", timeout=timeout_ms)
            _settle(page)
            roster = collect_accounts(captured_bodies)
        except Exception as exc:
            log.debug("download-options nudge failed (continuing): %r", exc)
    if not roster:
        log.warning("no deposit accounts discovered — confirm the overview "
                    "fires account/detail/dda/list or download/options/list "
                    "(DESIGN.md §B); the run writes an empty bronze.")
    return roster


def _export_account(page, acct: dict, run_dir: Path, since, until,
                    timeout_ms: int) -> int:
    """Export the CSV + QFX activity for one account into
    transactions/<ext_id>.<fmt>. Returns the number of files captured."""
    ext = acct["account_external_id"]
    stem = safe_stem(ext)
    over_window = since is not None and (date.today() - since).days > 730
    got = 0
    # One fresh page load per format: the two exports share no state, so a
    # dialog or overlay left by the first (e.g. the session-timeout prompt)
    # can't strand the second.
    for fmt, opt_prefix in EXPORT_FORMATS.items():
        try:
            page.goto(ROUTE_TX_DOWNLOAD.format(ext=ext),
                      wait_until="domcontentloaded", timeout=timeout_ms)
            _settle(page)
            # A goto to the hash route we're already on doesn't re-render the
            # SPA, so the form can be stale/absent on the second format. Wait
            # for it; force a reload if it didn't render.
            if not _wait_visible(page, SEL_FILE_SELECT, 8):
                page.reload(wait_until="domcontentloaded", timeout=timeout_ms)
                _settle(page)
                _wait_visible(page, SEL_FILE_SELECT, 8)
            # "All transactions" is Chase's widest export window here (last 24
            # months). --lookback narrower is covered; wider Chase cannot
            # export, so warn rather than silently under/over-fetch.
            if over_window:
                log.warning("  --lookback exceeds Chase's 24-month export "
                            "window for %s; fetching the full 24 months.",
                            acct.get("mask") or ext)
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


def _download_statements(page, accounts, run_dir: Path, since, until,
                         timeout_ms: int) -> int:
    """Download deposit statement PDFs into statements/<ext_id>/ (DESIGN.md §C).
    The documents centre lists the deposit relationship's statements as a
    classic table with a per-row 'Save as PDF'; each save fires a download.
    The surface has no per-account selector, so the PDFs are filed under the
    primary deposit account."""
    if not accounts:
        return 0
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
        return 0
    out_dir = run_dir / "statements" / safe_stem(accounts[0]["account_external_id"])
    out_dir.mkdir(parents=True, exist_ok=True)
    return _save_statements(page, out_dir, since, until, timeout_ms)


def _save_statements(page, out_dir: Path, since, until,
                     timeout_ms: int) -> int:  # pragma: no cover
    """Page the statements table through each year in the export window,
    saving every statement PDF whose date falls in [since, until]."""
    _expand_statements(page)
    avail = _available_statement_years(page)
    years = statement_years(since, until, avail)
    log.info("  statements: filter offers %s; paging %s", avail, years)
    got = 0
    seen: set[str] = set()
    for year in years:
        if not _select_statement_year(page, year):
            log.info("  statements: %d not shown, skipping", year)
            continue
        n = _save_visible_statements(page, out_dir, since, until, seen, timeout_ms)
        log.info("  statements: saved %d for %d", n, year)
        got += n
    return got


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


def _save_visible_statements(page, out_dir: Path, since, until,
                             seen: set, timeout_ms: int) -> int:  # pragma: no cover
    """Save every in-window statement PDF from the currently displayed table.
    Rows are contiguous (row0, row1, …); the first missing row ends the walk."""
    got = 0
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
        # Open the row's download dropdown, then click "Save as PDF".
        if not mdsui.click(page, STMT_DOWNLOAD_TRIGGER.format(n=n)):
            continue
        _settle(page, 300)
        try:
            with page.expect_download(timeout=timeout_ms) as dl_info:
                mdsui.click(page, STMT_PDF_OPTION.format(n=n))
            dl_info.value.save_as(str(out_dir / f"{key}.pdf"))
            seen.add(key)
            got += 1
            log.info("  saved statement %s", key)
        except Exception as exc:
            log.warning("  statement %s failed: %r", key, exc)
    return got


# ---- small UI helpers (browser-side; live-validated) ---------------------

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
