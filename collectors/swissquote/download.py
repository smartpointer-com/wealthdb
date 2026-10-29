#!/usr/bin/env python3
"""
Swissquote e-banking bronze downloader.

Reuses the Playwright session minted by login.py to export:
  - accounts.json              — eBanking #accountOverview/main (DOM scrape)
  - transactions CSV(s)        — Trading Platform #transactions
  - positions XLS              — Trading Platform #portfoliooverview
  - position_details.json      — Trading Platform #portfoliooverview (DOM scrape: long name + ISIN)
  - list_of_assets XLS         — Trading Platform #portfoliooverview
  - account_overview PDF       — Trading Platform #portfoliooverview
  - eDocuments PDFs            — eBanking #documents (REST endpoint)
  - run.json                   — metadata index for the dump

Files land in <bronze-dir>/<UTC-timestamp>/<artefact>. Read-only — see
AGENTS.md §1. Per AGENTS.md §2, non-dry-run invocations must be
explicitly authorised.

Usage:
    download.py [--state-path <file>] [--bronze-dir <dir>]
                [--lookback PRESET|YYYY-MM-DD]
                [--dry-run] [--debug] [--screenshot-dir <dir>] [--trace]
"""

from __future__ import annotations

import argparse
import json
import logging
import re
import shutil
import sys
from datetime import date
from pathlib import Path

import landmarks as sq  # local module
import login  # local module: the shared browser knobs + state paths

from collectorkit import bronze, cli, debugcap, session

log = logging.getLogger("swissquote.download")

LANDMARK_TIMEOUT_MS = login.LANDMARK_TIMEOUT_MS
DOWNLOAD_TIMEOUT_MS = 60_000

# The storageState login.py mints (the wrapper's /secrets mount).
DEFAULT_STATE_PATH = login.DEFAULT_STATE_PATH

# The --lookback default (90 days) suits incremental runs: load replaces
# each CSV's own row span, and a document whose id a complete earlier
# dump holds is not fetched again.

# Customer ID is captured from the Positions XLS download filename
# (`Positions_<cust>_<ddmmyyyy>_<hh>_<mm>.xls`). This regex pulls it
# out and serves as a sanity check that the expected file arrived.
POSITIONS_FILENAME_RE = re.compile(
    r"^Positions_(?P<customer>\d+)_\d{8}_\d{2}_\d{2}\.xls$"
)


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__.strip())
    p.add_argument("--state-path", type=Path, default=DEFAULT_STATE_PATH,
                   help="Path to the Playwright storageState.json minted by "
                        "login.py (default: %(default)s).")
    p.add_argument("--bronze-dir", type=Path, default=Path("/data"),
                   help="Bronze tree root (default: %(default)s, the wrapper's "
                        "/data mount); a UTC-timestamped subdir is created per run.")
    # Shared date-window contract: a single --lookback naming the
    # start of the [start..today] window that transactions and
    # documents alike are fetched for. Swissquote enforces no window
    # cap, so an older ISO date (or --lookback all) triggers a bulk
    # backfill.
    cli.add_standard_args(p, verb="download")
    p.add_argument("--dry-run", action="store_true",
                   help="Validate session and selectors; do not export "
                        "anything. Use to confirm the UI hasn't shifted "
                        "before triggering a real run.")
    p.add_argument("--screenshot-dir", default=None, type=Path,
                   help="If set, write a screenshot at each landmark.")
    p.add_argument("--trace", action="store_true",
                   help="Capture a Playwright trace bundle. Requires "
                        "--screenshot-dir; the bundle is written there "
                        "alongside screenshots.")
    p.add_argument("--debug", action="store_true",
                   help="Save opt-in debug captures (DOM + screenshot) "
                        "INSIDE the bronze run dir under <run>/screenshots/, "
                        "at the three scraped landmarks: the portfolio "
                        "overview, the transactions route, and the documents "
                        "list once its Period filter is widened. Also keeps a "
                        "crashed run dir instead of removing it, so the "
                        "captures survive the failure they explain (`load` "
                        "skips it; `prune` reclaims it). Off by default — the "
                        "captures are never read by `load`, and `prune` "
                        "reclaims <run>/screenshots/ from complete dumps. "
                        "No-op under --dry-run, which creates no run dir. "
                        "Distinct from --screenshot-dir / --trace, whose "
                        "landmark screenshots and trace bundles write OUTSIDE "
                        "bronze.")
    return p.parse_args(argv)


# ============================================================
# Playwright plumbing
# ============================================================

def _new_context(p, state_path: Path):
    """Launch Chromium and load the persisted session into a context."""
    if not state_path.is_file():
        raise SystemExit(
            f"No state file at {state_path}. Run login.py first."
        )
    return login.new_context(p, storage_state=state_path,
                             accept_downloads=True)


# JS run in the page to clear any active Pendo in-app guide. Tries
# Pendo's own stopGuides() API first (the clean dismissal, which also
# marks the guide seen so it doesn't re-fire), then strips any
# residual overlay nodes that intercept pointer events. Returns the
# number of overlay nodes removed so the caller can log it. Pure
# client-side DOM cleanup — see landmarks.PENDO_OVERLAY_SELECTOR.
_DISMISS_GUIDE_OVERLAYS_JS = """
(selector) => {
    try {
        if (window.pendo && typeof window.pendo.stopGuides === 'function') {
            window.pendo.stopGuides();
        }
    } catch (e) { /* best-effort */ }
    const nodes = document.querySelectorAll(selector);
    nodes.forEach((el) => el.remove());
    return nodes.length;
}
"""


def dismiss_guide_overlays(page) -> None:
    """Remove any active Pendo in-app guide overlay (best-effort).

    Swissquote occasionally shows a Pendo product-tour walkthrough
    whose full-page backdrop intercepts pointer events, blocking
    clicks on otherwise-visible export buttons. It is cleared before
    each export interaction. Never raises — if there's no overlay,
    or the evaluate fails, the run proceeds unchanged.
    """
    try:
        removed = page.evaluate(
            _DISMISS_GUIDE_OVERLAYS_JS, sq.PENDO_OVERLAY_SELECTOR
        )
        if removed:
            log.info("Dismissed in-app guide overlay (%d node(s))", removed)
    except Exception as e:  # noqa: BLE001 - best-effort, never fatal
        log.debug("Guide-overlay dismissal skipped: %s", e)


def _maybe_start_trace(context, enabled: bool):
    if enabled:
        context.tracing.start(screenshots=True, snapshots=True, sources=True)


def _verify_session(page) -> None:
    """Hit an F5-protected URL; raise unless the page lands at the post-auth URL.

    Navigating DIRECTLY to the Trading Platform proves nothing (F5 lets
    it load as a blank SPA when unauthenticated), so the probe is always
    the eBanking root, which F5 protects properly: a valid session
    settles on a post-auth SPA — that root, or the Trading Platform F5
    redirects on to when it is the account's landing app — while an
    invalid one ends up at /my.policy. `is_post_auth_url` draws exactly
    that line, and only for a URL reached this way.
    """
    from playwright.sync_api import TimeoutError as PWTimeout
    log.info("Verifying session via %s", sq.LOGIN_TRIGGER_URL)
    page.goto(sq.LOGIN_TRIGGER_URL, wait_until="domcontentloaded")
    try:
        page.wait_for_load_state("networkidle", timeout=LANDMARK_TIMEOUT_MS)
    except PWTimeout:
        pass
    if not sq.is_post_auth_url(page.url):
        raise SystemExit(
            f"Session expired (final URL: {page.url}). "
            "Re-run login.py to mint a fresh session."
        )
    log.info("Session OK (settled at %s)", page.url)


# ============================================================
# Transactions
# ============================================================

def _set_date_picker(page, picker_index: int, d: date) -> None:
    """Set the (day, month, year) inputs of one date picker.

    Index 0 selects the "from" date, 1 the "to" date — the page
    renders the pickers in DOM order.

    Swissquote's date inputs are React-controlled. Both `.fill()`
    and `.press_sequentially()` failed to update the bound state:
    the visible value reverted as soon as the page re-rendered. The
    trick that works is to bypass React's input wrapper by calling
    the *native* HTMLInputElement value setter and then dispatching
    a synthetic `input` event — the React onChange handler listens
    for that and accepts the value.
    """
    page.evaluate(
        """({idx, day, month, year}) => {
            const nativeSetter = Object.getOwnPropertyDescriptor(
                window.HTMLInputElement.prototype, "value"
            ).set;
            const fire = (input, value) => {
                nativeSetter.call(input, value);
                input.dispatchEvent(new Event("input", { bubbles: true }));
                input.dispatchEvent(new Event("change", { bubbles: true }));
            };
            const days = document.querySelectorAll("input.InputDate__input--day");
            const months = document.querySelectorAll("input.InputDate__input--month");
            const years = document.querySelectorAll("input.InputDate__input--year");
            fire(days[idx], day);
            fire(months[idx], month);
            fire(years[idx], year);
        }""",
        {
            "idx": picker_index,
            "day": f"{d.day:02d}",
            "month": f"{d.month:02d}",
            "year": f"{d.year:04d}",
        },
    )


def export_transactions_window(
    page, run_dir: Path, window_start: date, window_end: date,
) -> dict:
    """Export the whole window as one CSV, transactions_000.csv, and
    return its run.json entry. The dropdown's CSV menu item is
    discovered at click-time.
    """
    log.info("Transactions window %s -> %s", window_start, window_end)

    dismiss_guide_overlays(page)
    _set_date_picker(page, 0, window_start)
    _set_date_picker(page, 1, window_end)

    # After the dates are set, the page has to apply them. The
    # Filters panel has an "Apply" button that becomes enabled once
    # the range changes; clicking it before opening the export
    # dropdown ensures the export covers the intended window.
    try:
        apply_btn = page.get_by_role("button", name="Apply")
        if apply_btn.is_enabled(timeout=2000):
            apply_btn.click()
            page.wait_for_load_state("networkidle", timeout=LANDMARK_TIMEOUT_MS)
    except Exception as e:  # noqa: BLE001 - best-effort
        log.debug("No 'Apply' button to click, or already applied: %s", e)

    page.locator(sq.TXN_EXPORT_DROPDOWN_TRIGGER).click()
    with page.expect_download(timeout=DOWNLOAD_TIMEOUT_MS) as dl_info:
        page.locator(sq.TXN_EXPORT_MENU_CSV).click()
    dl = dl_info.value

    target = run_dir / "transactions_000.csv"
    dl.save_as(str(target))
    log.info("Saved %s", target)

    return {
        "file": target.name,
        "window_start": window_start.isoformat(),
        "window_end": window_end.isoformat(),
        "source_filename": dl.suggested_filename,
    }


# ============================================================
# Portfolio / Positions / List of Assets
# ============================================================

def export_positions(page, run_dir: Path) -> tuple[Path, str | None]:
    """Click the top-right Positions Export button; save positions.xls.

    Returns (path, customer_id_or_None). The customer ID is captured
    from the suggested filename, which Swissquote formats as
    `Positions_<customer>_<ddmmyyyy>_<hh>_<mm>.xls`.
    """
    dismiss_guide_overlays(page)
    button = page.locator(sq.POSITIONS_EXPORT_BUTTON)
    with page.expect_download(timeout=DOWNLOAD_TIMEOUT_MS) as dl_info:
        button.click()
    dl = dl_info.value

    target = run_dir / "positions.xls"
    dl.save_as(str(target))
    log.info("Saved %s", target)

    m = POSITIONS_FILENAME_RE.match(dl.suggested_filename)
    customer_id = m.group("customer") if m else None
    return target, customer_id


def scrape_accounts(page, run_dir: Path) -> list[dict]:
    """Scrape the eBanking #accountOverview/main account list.

    Each account is rendered as `<TYPE> <CUSTOMER_ID>` text inside
    `li.AccountListItem .AccountDetails__portfolioTitle`. Those
    strings are parsed into structured entries and written to
    `accounts.json` in the run dir.

    The list drives silver's `accounts.account_product`; the gold
    adapter maps it to a tax wrapper (wealthdb/docs/adapters/swissquote.md).
    The schema holds any number of accounts per customer.
    """
    page.goto(sq.EBANKING_BASE_URL, wait_until="domcontentloaded")
    page.wait_for_load_state("networkidle", timeout=LANDMARK_TIMEOUT_MS)
    page.evaluate(f"window.location.hash = '{sq.ROUTE_ACCOUNT_OVERVIEW}'")
    page.wait_for_selector(sq.ACCOUNT_LIST_ROW, timeout=LANDMARK_TIMEOUT_MS)

    raw_lines = page.eval_on_selector_all(
        f"{sq.ACCOUNT_LIST_ROW} {sq.ACCOUNT_PORTFOLIO_TITLE}",
        "els => els.map(el => el.textContent.trim())",
    )
    accounts: list[dict] = []
    parse_re = re.compile(r"^(?P<type>[A-Z][\w \-]*?)\s+(?P<id>\d{6,8})$")
    for line in raw_lines:
        m = parse_re.match(line)
        if not m:
            # Digits masked: the entry carries the account number.
            log.warning("Skipping unparseable account-list entry: %r",
                        re.sub(r"\d", "#", line))
            continue
        accounts.append({
            "account_product": m.group("type").strip(),
            "account_external_id": m.group("id"),
        })
    target = run_dir / "accounts.json"
    target.write_text(
        json.dumps(accounts, indent=2, sort_keys=True) + "\n",
        encoding="utf-8",
    )
    log.info("Saved %s (%d account(s))", target, len(accounts))
    return accounts


def scrape_position_details(page, run_dir: Path) -> list[dict]:
    """Scrape per-position long name + ISIN off the Portfolio Overview.

    The Positions XLS export only carries the ticker. The same page
    surfaces:
      - ISIN as the first path segment in each row's FullQuote link
        href (`…#fullQuote/{ISIN}/{type}_{CCY}`), available at rest.
      - Long instrument name in a hover tooltip on the symbol cell.
        Not in the DOM at rest — each cell is hovered once.

    Writes one entry per (symbol, currency) position to
    `position_details.json`. The loader joins on (symbol, currency)
    to promote `name` and `isin` columns in the silver positions
    table. Forward-fill only — older bronze dumps that predate this
    artefact load with NULL for these columns.

    The Buy/Sell buttons inside each row are intentionally NOT
    interacted with; see AGENTS.md §1.
    """
    # Defensive: expand any collapsed widgets so the Positions table
    # is in the DOM. Idempotent on already-expanded widgets.
    for c in page.locator(sq.WIDGET_COLLAPSED).all():
        try:
            c.locator(sq.WIDGET_HEADER).first.click(timeout=2000)
        except Exception as e:  # noqa: BLE001 - best-effort
            log.debug("Skipped collapsed widget (no header click): %s", e)
    page.wait_for_timeout(800)

    containers = page.locator(sq.POSITION_SYMBOL_CONTAINER)
    n = containers.count()
    log.info("Scraping detail for %d position(s)", n)
    isin_re = re.compile(
        r"#fullQuote/(?P<isin>[A-Z0-9]{8,12})/[^/]+$"
    )

    entries: list[dict] = []
    for i in range(n):
        container = containers.nth(i)
        link = container.locator(sq.POSITION_SYMBOL_LINK).first
        try:
            symbol = link.inner_text(timeout=2000).strip()
            href = link.get_attribute("href", timeout=2000) or ""
        except Exception as e:  # noqa: BLE001
            log.warning(
                "Skip position container %d (no symbol link): %s", i, e,
            )
            continue
        m = isin_re.search(href)
        isin = m.group("isin") if m else None

        # Currency lives in a sibling column. Cheaper to derive at
        # load time by joining against positions.xls on symbol, but
        # the row's column is recorded for the loader's safety net.
        # The href's trailing fragment (`…/4_CHF`) encodes the
        # market+currency; the currency suffix is extracted.
        ccy = None
        ccy_re = re.search(r"_([A-Z]{3})$", href)
        if ccy_re:
            ccy = ccy_re.group(1)

        # Hover to expose the tooltip. The tooltip is portal-rendered;
        # after hover settles, the popup appears in the DOM root.
        name = None
        try:
            container.locator(sq.POSITION_TOOLTIP_TARGET).first.hover(timeout=3000)
            page.wait_for_selector(
                sq.POSITION_TOOLTIP_POPUP, state="visible", timeout=3000,
            )
            # First visible tooltip popup wins.
            popups = page.locator(sq.POSITION_TOOLTIP_POPUP)
            for j in range(popups.count()):
                p = popups.nth(j)
                if p.is_visible():
                    name = p.inner_text().strip() or None
                    break
        except Exception as e:  # noqa: BLE001 - non-fatal; record symbol+ISIN
            log.warning(
                "Tooltip miss for %s — only symbol+isin captured: %s",
                symbol, e,
            )
        # Move the mouse to a corner so the tooltip dismisses before
        # the next row is hovered.
        page.mouse.move(0, 0)
        page.wait_for_timeout(150)

        entries.append({
            "symbol": symbol,
            "currency": ccy,
            "isin": isin,
            "name": name,
        })

    target = run_dir / "position_details.json"
    target.write_text(
        json.dumps(entries, indent=2, sort_keys=True, ensure_ascii=False) + "\n",
        encoding="utf-8",
    )
    log.info("Saved %s (%d entr(ies))", target, len(entries))
    return entries


def export_account_overview(page, run_dir: Path) -> Path:
    """Click the 'Export account overview' button; save account_overview.pdf.

    Server-side rendered PDF (~35KB) containing the current portfolio
    summary as a printable report. The button uses a distinct class
    (`.srp-ControlsPanel__printInfo`) and aria-label.
    """
    dismiss_guide_overlays(page)
    button = page.locator(sq.ACCOUNT_OVERVIEW_EXPORT_BUTTON)
    with page.expect_download(timeout=DOWNLOAD_TIMEOUT_MS) as dl_info:
        button.click()
    dl = dl_info.value
    target = run_dir / "account_overview.pdf"
    dl.save_as(str(target))
    log.info("Saved %s", target)
    return target


def export_list_of_assets(page, run_dir: Path) -> Path:
    """Click the Export button next to the Assets table; save list_of_assets.xls.

    This button is class `.CaptionButton`, distinct from the
    Positions `.ExportButton`. Both have aria-label="Export".
    """
    dismiss_guide_overlays(page)
    button = page.locator(sq.LIST_OF_ASSETS_EXPORT_BUTTON)
    with page.expect_download(timeout=DOWNLOAD_TIMEOUT_MS) as dl_info:
        button.click()
    dl = dl_info.value
    target = run_dir / "list_of_assets.xls"
    dl.save_as(str(target))
    log.info("Saved %s", target)
    return target


# ============================================================
# Documents
# ============================================================

def collect_existing_doc_ids(bronze_dir: Path) -> set[str]:
    """Walk prior run dirs and collect document IDs already downloaded.

    Files are named `<doc_id>.pdf`. The set is used to skip docs
    already in bronze, avoiding redundant HTTP fetches. The silver
    loader also dedups by content_sha256, so this is a bandwidth
    optimisation, not a correctness mechanism.

    Only complete dumps count: `load` never reads the documents of an
    incomplete one (a crashed walk kept under --debug, or a walk still
    running), so skipping a document held only there would keep it out
    of silver.
    """
    seen: set[str] = set()
    if not bronze_dir.is_dir():
        return seen
    for run_dir in bronze.iter_run_dirs(bronze_dir):
        if bronze.manifest_is_complete(run_dir):
            seen.update(pdf.stem for pdf in run_dir.glob("documents/*.pdf"))
    return seen


def navigate_to_documents(page) -> None:
    """Navigate the eBanking SPA to its #documents route.

    Direct `page.goto(URL#documents)` is unreliable — the SPA's
    bootstrap clobbers the hash and lands on its default route
    (#accountOverview/main). The working approach: load the SPA root
    first, then mutate `window.location.hash` after the SPA is alive.
    The SPA's hashchange handler then routes to the docs view.
    """
    page.goto(sq.EBANKING_BASE_URL, wait_until="domcontentloaded")
    page.wait_for_load_state("networkidle", timeout=LANDMARK_TIMEOUT_MS)
    page.evaluate("window.location.hash = '#documents'")
    page.wait_for_selector(sq.DOC_ROW, timeout=LANDMARK_TIMEOUT_MS)


def set_documents_period(page, since: date, until: date) -> None:
    """Widen the Documents page Period filter from its 30-day default.

    Uses the same React-native-setter approach as the transactions
    date picker — the input selectors are shared across the two
    pages, so _set_date_picker works unchanged. After mutating the
    inputs, click Apply and wait for the table to re-render: the
    `.LoadingTable` spinner appears briefly and disappears once the
    new rows are in the DOM.
    """
    dismiss_guide_overlays(page)
    _set_date_picker(page, 0, since)
    _set_date_picker(page, 1, until)
    page.get_by_role("button", name="Apply").click(timeout=LANDMARK_TIMEOUT_MS)
    # Tiny pause so the spinner has a chance to render before the
    # wait for its disappearance. Without this, wait_for(state="hidden")
    # races and returns immediately because the spinner hasn't been
    # added to the DOM yet.
    page.wait_for_timeout(200)
    page.locator(sq.DOC_TABLE_SPINNER).wait_for(
        state="hidden", timeout=LANDMARK_TIMEOUT_MS,
    )


def discover_documents(page) -> list[dict]:
    """Scrape the rendered DOM of the documents page for fetch URLs.

    Each document row contains an <a class="...downloadDocument">
    whose href is the same REST endpoint the download calls directly.
    The absolute href of every such anchor is pulled, then its path
    and query string are parsed into structured metadata.

    URL format:
      /sqc-ctrp-notifications-plugin/webapi/notifications/
        getPdfDocument/{customer}/{doc_id}
        ?documentType={URL-encoded type, may contain spaces}
        &contractNo={short numeric ID; matches "Reference" column}
        &date={YYYYMMDD}
        &targetUser={PERSON|...}
    """
    from urllib.parse import urlparse, parse_qs, unquote

    hrefs = page.eval_on_selector_all(
        "a[href*='getPdfDocument']",
        "els => els.map(el => el.href)",
    )
    # Anchored at /getPdfDocument/ (the path prefix before is host-
    # routing noise) and at end-of-path. re.search, not re.match,
    # because the full path starts with /sqc-ctrp-notifications-plugin/.
    path_re = re.compile(
        r"/getPdfDocument/(?P<customer>\d+)/(?P<doc_id>[A-F0-9-]+)/?$"
    )
    docs: list[dict] = []
    seen_ids: set[str] = set()
    for href in hrefs:
        parsed = urlparse(href)
        m = path_re.search(parsed.path)
        if not m:
            log.warning("Skipping unrecognised getPdfDocument URL: %s", href)
            continue
        qs = parse_qs(parsed.query)
        doc_id = m.group("doc_id")
        if doc_id in seen_ids:
            continue  # the same doc can be linked from multiple cells per row
        seen_ids.add(doc_id)
        docs.append({
            "doc_id": doc_id,
            "doc_type": unquote(qs.get("documentType", [""])[0]),
            "customer_id": m.group("customer"),
            "contract_no": qs.get("contractNo", [None])[0],
            "date": qs.get("date", [None])[0],
            "target_user": qs.get("targetUser", [None])[0],
            "fetch_url": href,
        })
    log.info("Documents page lists %d document(s)", len(docs))
    return docs


def fetch_document(context, doc: dict, target: Path) -> None:
    """Fetch a single PDF via Playwright's request API (cookie reused)."""
    resp = context.request.get(doc["fetch_url"])
    if not resp.ok:
        raise SystemExit(
            f"Document fetch failed for {doc['doc_id']}: "
            f"HTTP {resp.status} {resp.status_text}"
        )
    target.parent.mkdir(parents=True, exist_ok=True)
    target.write_bytes(resp.body())


# ============================================================
# Orchestration
# ============================================================

def cleanup_incomplete_run_dir(run_dir: Path) -> bool:
    """Remove a partial dump dir that is not a completed dump.

    Called from the download crash-cleanup trap. A dump is complete
    only once run.json carries ``status == "complete"`` (or, for a
    legacy dump, a statusless run.json is present). A run dir with no
    run.json, or one still carrying the ``{"status": "in-progress"}``
    marker this walk drops at creation, is a crashed/interrupted
    download and is removed so orphans don't accumulate in bronze.
    Guarded on dir existence so a failure before mkdir (or a dry run)
    is a no-op. Uses ignore_errors so a cleanup hiccup never masks the
    original exception. Returns True if a dir was removed.
    """
    if run_dir.exists() and not bronze.manifest_is_complete(run_dir):
        shutil.rmtree(run_dir, ignore_errors=True)
        log.warning(
            "Removed incomplete dump dir %s after failure", run_dir.name,
        )
        return True
    return False


def run(args: argparse.Namespace) -> int:
    from playwright.sync_api import sync_playwright

    if not args.bronze_dir.is_dir():
        raise SystemExit(f"--bronze-dir does not exist: {args.bronze_dir}")

    since, until = cli.resolve_lookback(args)

    run_dir = bronze.run_dir(args.bronze_dir)
    run_ts_str = run_dir.name

    existing_doc_ids = collect_existing_doc_ids(args.bronze_dir)
    log.info("Bronze tree contains %d already-downloaded document(s)",
             len(existing_doc_ids))

    # Captures land in the run dir, and --dry-run returns before one is
    # created, so --debug degrades to a warning there rather than
    # materialising a dir the dry run promised not to write. The landmark
    # names mirror the --screenshot-dir ones so the two dirs read alike.
    debug_dir = run_dir if args.debug else None
    if args.debug and args.dry_run:
        log.warning("--debug: --dry-run creates no run dir; "
                    "no captures will be written")
        debug_dir = None

    # Snapshot a landmark into the run dir, masked by the lifted session's
    # jar; a no-op unless --debug.
    capture = debugcap.page_capturer(debug_dir, log=log)

    with sync_playwright() as p:
        browser, context = _new_context(p, session.resolve_state_path(
            args.state_path, DEFAULT_STATE_PATH, login.LEGACY_STATE_PATH))
        _maybe_start_trace(context, args.trace)
        page = context.new_page()
        try:
            _verify_session(page)
            login.screenshot(page, args.screenshot_dir, "00_session_verified")

            if args.dry_run:
                log.info("Dry run — confirming landmarks only")
                page.goto(
                    sq.TRADING_PLATFORM_BASE_URL + sq.ROUTE_TRANSACTIONS,
                    wait_until="domcontentloaded",
                )
                page.wait_for_selector(
                    sq.TXN_EXPORT_DROPDOWN_TRIGGER, timeout=LANDMARK_TIMEOUT_MS,
                )
                login.screenshot(page, args.screenshot_dir, "01_transactions_dry")

                page.goto(
                    sq.TRADING_PLATFORM_BASE_URL + sq.ROUTE_PORTFOLIO_OVERVIEW,
                    wait_until="domcontentloaded",
                )
                page.wait_for_selector(
                    sq.POSITIONS_EXPORT_BUTTON, timeout=LANDMARK_TIMEOUT_MS,
                )
                page.wait_for_selector(
                    sq.LIST_OF_ASSETS_EXPORT_BUTTON, timeout=LANDMARK_TIMEOUT_MS,
                )
                login.screenshot(page, args.screenshot_dir, "02_portfolio_dry")

                log.info("Dry run complete; no artefacts written.")
                return 0

            run_dir.mkdir(parents=True, exist_ok=False)
            # Drop an in-progress marker the moment the run dir exists,
            # overwritten atomically with the terminal manifest at the
            # end. A crashed walk therefore leaves run.json with
            # status="in-progress" rather than no manifest at all —
            # which the crash-cleanup trap and `prune` both key on to
            # tell a partial dump from a finished one. Written atomically
            # so a concurrent `prune` never reads a half-written file.
            bronze.atomic_write_json(
                run_dir / "run.json", {"status": "in-progress"})
            log.info("Writing artefacts to %s", run_dir)

            # --- Accounts list (informal type per account) -----------
            accounts = scrape_accounts(page, run_dir)

            # --- Portfolio: positions + list_of_assets ---------------
            # Doing this before transactions yields the customer_id
            # (parsed from the Positions XLS filename) early, which
            # other steps stamp into run.json.
            page.goto(
                sq.TRADING_PLATFORM_BASE_URL + sq.ROUTE_PORTFOLIO_OVERVIEW,
                wait_until="domcontentloaded",
            )
            page.wait_for_selector(
                sq.POSITIONS_EXPORT_BUTTON, timeout=LANDMARK_TIMEOUT_MS,
            )
            login.screenshot(page, args.screenshot_dir, "10_portfolio_page")
            # The DOM the export buttons and the position rows are matched
            # against — scrape_position_details reads long name + ISIN
            # straight out of it, and nothing else records what it saw.
            capture(page, "10_portfolio_page")

            _, customer_id = export_positions(page, run_dir)
            export_list_of_assets(page, run_dir)
            export_account_overview(page, run_dir)
            # Per-position long name + ISIN (DOM scrape, not in any
            # of the file exports). Runs on the same page as the
            # exports above; ordering after them avoids any risk of
            # hover state interfering with the export-button clicks.
            position_details = scrape_position_details(page, run_dir)
            log.info("Customer ID %s", "read from the XLS filename"
                     if customer_id else "<unknown>")

            # --- Transactions ----------------------------------------
            page.goto(
                sq.TRADING_PLATFORM_BASE_URL + sq.ROUTE_TRANSACTIONS,
                wait_until="domcontentloaded",
            )
            page.wait_for_selector(
                sq.TXN_EXPORT_DROPDOWN_TRIGGER, timeout=LANDMARK_TIMEOUT_MS,
            )
            login.screenshot(page, args.screenshot_dir, "20_transactions_page")
            capture(page, "20_transactions_page")

            # Single CSV covering the whole window — no chunking.
            entry = export_transactions_window(page, run_dir, since, until)
            txn_entries = [entry]

            # --- Documents -------------------------------------------
            navigate_to_documents(page)
            login.screenshot(page, args.screenshot_dir, "30_documents_page_default")
            log.info("Widening documents Period filter to %s..%s",
                     since, until)
            set_documents_period(page, since, until)
            login.screenshot(page, args.screenshot_dir, "31_documents_page_wide")
            # Taken after the Period filter is widened and before the list
            # is read: discover_documents scrapes this DOM, so it is the
            # evidence for a discovery that comes back empty or short.
            capture(page, "31_documents_page_wide")

            docs = discover_documents(page)
            doc_entries = []
            customer_id_from_docs = None
            for doc in docs:
                customer_id_from_docs = doc["customer_id"]
                if doc["doc_id"] in existing_doc_ids:
                    log.debug("Skip already-downloaded %s", doc["doc_id"])
                    continue
                target = run_dir / "documents" / f"{doc['doc_id']}.pdf"
                fetch_document(context, doc, target)
                doc_entries.append({
                    "file": str(target.relative_to(run_dir)),
                    "doc_id": doc["doc_id"],
                    "doc_type": doc["doc_type"],
                    "contract_no": doc.get("contract_no"),
                    "date": doc.get("date"),
                    "target_user": doc.get("target_user"),
                })
            login.screenshot(page, args.screenshot_dir, "32_after_docs")

            customer_id = customer_id or customer_id_from_docs
            if not customer_id:
                log.warning(
                    "Could not determine customer_id from XLS filename or "
                    "document URLs; run.json will omit it."
                )

            # --- run.json --------------------------------------------
            run_meta = {
                "status": "complete",
                "timestamp": run_ts_str,
                "customer_id": customer_id,
                "accounts": {"file": "accounts.json", "entries": accounts},
                "transactions": txn_entries,
                "documents": doc_entries,
                "documents_window": {
                    "start": since.isoformat(),
                    "end": until.isoformat(),
                },
                "positions": {"file": "positions.xls"},
                "position_details": {
                    "file": "position_details.json",
                    "entries": position_details,
                },
                "list_of_assets": {"file": "list_of_assets.xls"},
                "account_overview": {"file": "account_overview.pdf"},
            }
            # Atomic (tmp + rename) so the in-progress marker is
            # replaced in a single step and a `prune` racing the
            # finalisation never reads a half-written manifest.
            bronze.atomic_write_json(run_dir / "run.json", run_meta)
            log.info(
                "Done. %d transaction window(s); %d new document(s).",
                len(txn_entries), len(doc_entries),
            )
            return 0
        except BaseException:
            # Cleanup trap. A crashed or interrupted download (export
            # timeout, SystemExit guard, Ctrl-C) leaves a partial
            # run_dir without the run.json completion marker. Remove
            # it so orphans don't accumulate in bronze, then re-raise
            # the original exception unchanged.
            #
            # --debug suppresses the removal: a crash is the failure the
            # captures exist to explain, and deleting them here would
            # discard the evidence at exactly the moment it is worth
            # having. The retained dir is inert — it carries no terminal
            # run.json, so `load` skips it and `prune` reclaims it once
            # quiescent, the same as any crashed dump. The exists() check
            # keeps a crash from BEFORE the run dir was made (a session
            # failure) on the normal path, where cleanup is a no-op
            # anyway, rather than claiming to keep a dir that never was.
            if debug_dir is not None and run_dir.exists():
                log.warning("--debug: keeping incomplete dump dir %s for "
                            "inspection; `prune` reclaims it", run_dir.name)
            else:
                cleanup_incomplete_run_dir(run_dir)
            raise
        finally:
            debugcap.stop_trace_if_active(context, args.trace,
                                          args.screenshot_dir, "download",
                                          log=log)
            context.close()
            browser.close()


def main(argv: list[str] | None = None) -> int:
    args = parse_args(sys.argv[1:] if argv is None else argv)
    cli.configure_logging(args.verbose)

    if args.trace and not args.screenshot_dir:
        raise SystemExit(
            "--trace requires --screenshot-dir. The trace bundle is "
            "written alongside screenshots; pick a directory that is "
            "NOT your secrets dir."
        )
    return run(args)


if __name__ == "__main__":
    sys.exit(main())
