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

Files land in <dest>/<UTC-timestamp>/<artefact>. Read-only — see
CLAUDE.md §1. Per CLAUDE.md §2, non-dry-run invocations must be
authorised by the user.

Usage:
    download.py --state-path <file> --dest <dir>
                [--since YYYY-MM-DD] [--until YYYY-MM-DD]
                [--documents-since YYYY-MM-DD] [--documents-until YYYY-MM-DD]
                [--dry-run] [--screenshot-dir <dir>] [--trace]
"""

from __future__ import annotations

import argparse
import json
import logging
import re
import sys
from datetime import date, datetime, timedelta, timezone
from pathlib import Path

import landmarks as sq  # local module

from collectorkit import cli

log = logging.getLogger("swissquote.download")

# Same UA as login.py — Swissquote's anti-bot heuristics may key on
# the UA across requests within a session, so we hold it constant.
USER_AGENT = (
    "Mozilla/5.0 (X11; Linux x86_64) "
    "AppleWebKit/537.36 (KHTML, like Gecko) "
    "Chrome/147.0.0.0 Safari/537.36"
)

NAV_TIMEOUT_MS = 60_000
LANDMARK_TIMEOUT_MS = 30_000
DOWNLOAD_TIMEOUT_MS = 60_000

# --since / --until / --lookback / --documents-* defaults are all
# resolved through collectorkit.cli.resolve_lookback (default
# DEFAULT_LOOKBACK_DAYS = 90). Sensible for incremental runs because
# silver's window-DELETE-INSERT replaces overlapping rows on each
# load, and the content-sha256 dedup means a wider re-run does not
# re-download already-captured PDFs.

# Customer ID is captured from the Positions XLS download filename
# (`Positions_<cust>_<ddmmyyyy>_<hh>_<mm>.xls`). This regex pulls it
# out and serves as a sanity check that we got the expected file.
POSITIONS_FILENAME_RE = re.compile(
    r"^Positions_(?P<customer>\d+)_\d{8}_\d{2}_\d{2}\.xls$"
)


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__.strip())
    p.add_argument("--state-path", required=True, type=Path,
                   help="Path to the Playwright storageState.json.")
    p.add_argument("--dest", type=Path, default=Path("/data"),
                   help="Output directory (default: %(default)s, the wrapper's "
                        "/data mount); a UTC-timestamped subdir is created per run.")
    # Shared date-window contract: --since/--until/--lookback +
    # --documents-since/--documents-until. Swissquote enforces no
    # window cap, so an explicit older --since (or --lookback all)
    # triggers a bulk backfill.
    cli.add_lookback_args(p)
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
    p.add_argument("-v", "--verbose", action="store_true",
                   help="DEBUG-level logging.")
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
    browser = p.chromium.launch(
        headless=True,
        # See login.py for rationale on the anti-detection knobs.
        args=[
            "--no-sandbox",
            "--disable-dev-shm-usage",
            "--disable-blink-features=AutomationControlled",
        ],
    )
    context = browser.new_context(
        storage_state=str(state_path),
        user_agent=USER_AGENT,
        locale="en-CH",
        timezone_id="Europe/Zurich",
        viewport={"width": 1440, "height": 900},
        accept_downloads=True,
    )
    context.add_init_script(
        "Object.defineProperty(navigator, 'webdriver', "
        "{ get: () => undefined });"
    )
    context.set_default_timeout(NAV_TIMEOUT_MS)
    return browser, context


def _screenshot(page, screenshot_dir: Path | None, name: str) -> None:
    if not screenshot_dir:
        return
    screenshot_dir.mkdir(parents=True, exist_ok=True)
    path = screenshot_dir / f"{name}.png"
    try:
        page.screenshot(path=str(path), full_page=True)
        log.info("Screenshot: %s", path)
    except Exception as e:  # noqa: BLE001 - debug aid only
        log.warning("Screenshot %s failed: %s", path, e)


def _maybe_start_trace(context, enabled: bool):
    if enabled:
        context.tracing.start(screenshots=True, snapshots=True, sources=True)


def _maybe_stop_trace(context, enabled: bool, trace_path: Path) -> None:
    if enabled:
        trace_path.parent.mkdir(parents=True, exist_ok=True)
        context.tracing.stop(path=str(trace_path))
        log.info("Wrote Playwright trace: %s", trace_path)


def _verify_session(page) -> None:
    """Hit an F5-protected URL; raise unless we land at the post-auth URL.

    The Trading Platform URL itself isn't a reliable login indicator
    (F5 lets it load as a blank SPA when unauthenticated). We probe
    the eBanking SPA root, which F5 protects properly: a valid
    session settles inside `/sqc-web-client-portal/`; an invalid one
    ends up at /my.policy. The `is_post_auth_url` predicate matches
    only the former.
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
    a synthetic `input` event ourselves — the React onChange handler
    listens for that and accepts our value.
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
    page, run_dir: Path, n: int, window_start: date, window_end: date,
) -> dict:
    """Export one (window_start, window_end) chunk as transactions_NNN.csv.

    Returns the metadata entry for run.json. The dropdown's CSV menu
    item is discovered at click-time; the dropdown is closed before
    we return, so subsequent windows start from a clean state.
    """
    log.info("Transactions window %s -> %s", window_start, window_end)

    _set_date_picker(page, 0, window_start)
    _set_date_picker(page, 1, window_end)

    # After setting dates we also need the page to apply them. The
    # Filters panel has an "Apply" button that becomes enabled once
    # the range changes; clicking it before opening the export
    # dropdown ensures the export covers our intended window.
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

    target = run_dir / f"transactions_{n:03d}.csv"
    dl.save_as(str(target))
    log.info("Saved %s (source filename: %s)", target, dl.suggested_filename)

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
    button = page.locator(sq.POSITIONS_EXPORT_BUTTON)
    with page.expect_download(timeout=DOWNLOAD_TIMEOUT_MS) as dl_info:
        button.click()
    dl = dl_info.value

    target = run_dir / "positions.xls"
    dl.save_as(str(target))
    log.info("Saved %s (source filename: %s)", target, dl.suggested_filename)

    m = POSITIONS_FILENAME_RE.match(dl.suggested_filename)
    customer_id = m.group("customer") if m else None
    return target, customer_id


def scrape_accounts(page, run_dir: Path) -> list[dict]:
    """Scrape the eBanking #accountOverview/main account list.

    Each account is rendered as `<TYPE> <CUSTOMER_ID>` text inside
    `li.AccountListItem .AccountDetails__portfolioTitle`. We parse
    those strings into structured entries and write them to
    `accounts.json` in the run dir.

    The list also drives silver's `accounts.account_product` column
    (added in migration 0002 as `account_type`, renamed in 0005).
    For single-account customers the result is one entry; the schema
    supports multi-account customers natively (the wealthdb gold
    adapter maps these to `tax_wrapper`: "Trading"/"Savings" →
    taxable_personal, "Säule 3a" → pillar_3a, "Freizügigkeit" →
    vested_benefits).
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
            log.warning("Skipping unparseable account-list entry: %r", line)
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
        Not in the DOM at rest — we hover each cell once.

    Writes one entry per (symbol, currency) position to
    `position_details.json`. The loader joins on (symbol, currency)
    to promote `name` and `isin` columns in the silver positions
    table. Forward-fill only — older bronze dumps that predate this
    artefact load with NULL for these columns.

    The Buy/Sell buttons inside each row are intentionally NOT
    interacted with; see CLAUDE.md §1.
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
        # we record the row's column for the loader's safety net.
        # The href's trailing fragment (`…/4_CHF`) encodes the
        # market+currency; we extract the currency suffix.
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
        # we hover the next row.
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
    button = page.locator(sq.ACCOUNT_OVERVIEW_EXPORT_BUTTON)
    with page.expect_download(timeout=DOWNLOAD_TIMEOUT_MS) as dl_info:
        button.click()
    dl = dl_info.value
    target = run_dir / "account_overview.pdf"
    dl.save_as(str(target))
    log.info("Saved %s (source filename: %s)", target, dl.suggested_filename)
    return target


def export_list_of_assets(page, run_dir: Path) -> Path:
    """Click the Export button next to the Assets table; save list_of_assets.xls.

    This button is class `.CaptionButton`, distinct from the
    Positions `.ExportButton`. Both have aria-label="Export".
    """
    button = page.locator(sq.LIST_OF_ASSETS_EXPORT_BUTTON)
    with page.expect_download(timeout=DOWNLOAD_TIMEOUT_MS) as dl_info:
        button.click()
    dl = dl_info.value
    target = run_dir / "list_of_assets.xls"
    dl.save_as(str(target))
    log.info("Saved %s (source filename: %s)", target, dl.suggested_filename)
    return target


# ============================================================
# Documents
# ============================================================

def collect_existing_doc_ids(dest: Path) -> set[str]:
    """Walk prior run dirs and collect document IDs already downloaded.

    Files are named `<doc_id>.pdf`. The set is used to skip docs
    already in bronze, avoiding redundant HTTP fetches. The silver
    loader also dedups by content_sha256, so this is a bandwidth
    optimisation, not a correctness mechanism.
    """
    seen: set[str] = set()
    if not dest.is_dir():
        return seen
    for pdf in dest.glob("*/documents/*.pdf"):
        seen.add(pdf.stem)
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
    _set_date_picker(page, 0, since)
    _set_date_picker(page, 1, until)
    page.get_by_role("button", name="Apply").click(timeout=LANDMARK_TIMEOUT_MS)
    # Tiny pause so the spinner has a chance to render before we
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
    whose href is the same REST endpoint we will call directly. We
    pull absolute hrefs of every such anchor, then parse the path
    and query string into structured metadata.

    URL format observed live:
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

def run(args: argparse.Namespace) -> int:
    from playwright.sync_api import sync_playwright

    if not args.dest.is_dir():
        raise SystemExit(f"Destination does not exist: {args.dest}")

    since, until, documents_since, documents_until = cli.resolve_lookback(args)

    run_ts_str = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    run_dir = args.dest / run_ts_str

    existing_doc_ids = collect_existing_doc_ids(args.dest)
    log.info("Bronze tree contains %d already-downloaded document(s)",
             len(existing_doc_ids))

    with sync_playwright() as p:
        browser, context = _new_context(p, args.state_path)
        _maybe_start_trace(context, args.trace)
        page = context.new_page()
        try:
            _verify_session(page)
            _screenshot(page, args.screenshot_dir, "00_session_verified")

            if args.dry_run:
                log.info("Dry run — confirming landmarks only")
                page.goto(
                    sq.TRADING_PLATFORM_BASE_URL + sq.ROUTE_TRANSACTIONS,
                    wait_until="domcontentloaded",
                )
                page.wait_for_selector(
                    sq.TXN_EXPORT_DROPDOWN_TRIGGER, timeout=LANDMARK_TIMEOUT_MS,
                )
                _screenshot(page, args.screenshot_dir, "01_transactions_dry")

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
                _screenshot(page, args.screenshot_dir, "02_portfolio_dry")

                log.info("Dry run complete; no artefacts written.")
                return 0

            run_dir.mkdir(parents=True, exist_ok=False)
            log.info("Writing artefacts to %s", run_dir)

            # --- Accounts list (informal type per account) -----------
            accounts = scrape_accounts(page, run_dir)

            # --- Portfolio: positions + list_of_assets ---------------
            # Doing this before transactions gives us the customer_id
            # (parsed from the Positions XLS filename) early, which
            # other steps stamp into run.json.
            page.goto(
                sq.TRADING_PLATFORM_BASE_URL + sq.ROUTE_PORTFOLIO_OVERVIEW,
                wait_until="domcontentloaded",
            )
            page.wait_for_selector(
                sq.POSITIONS_EXPORT_BUTTON, timeout=LANDMARK_TIMEOUT_MS,
            )
            _screenshot(page, args.screenshot_dir, "10_portfolio_page")

            _, customer_id = export_positions(page, run_dir)
            export_list_of_assets(page, run_dir)
            export_account_overview(page, run_dir)
            # Per-position long name + ISIN (DOM scrape, not in any
            # of the file exports). Runs on the same page as the
            # exports above; ordering after them avoids any risk of
            # hover state interfering with the export-button clicks.
            position_details = scrape_position_details(page, run_dir)
            log.info("Customer ID (from XLS filename): %s",
                     customer_id or "<unknown>")

            # --- Transactions ----------------------------------------
            page.goto(
                sq.TRADING_PLATFORM_BASE_URL + sq.ROUTE_TRANSACTIONS,
                wait_until="domcontentloaded",
            )
            page.wait_for_selector(
                sq.TXN_EXPORT_DROPDOWN_TRIGGER, timeout=LANDMARK_TIMEOUT_MS,
            )
            _screenshot(page, args.screenshot_dir, "20_transactions_page")

            # Single CSV covering the whole window — no chunking.
            entry = export_transactions_window(page, run_dir, 0, since, until)
            txn_entries = [entry]

            # --- Documents -------------------------------------------
            navigate_to_documents(page)
            _screenshot(page, args.screenshot_dir, "30_documents_page_default")
            log.info("Widening documents Period filter to %s..%s",
                     documents_since, documents_until)
            set_documents_period(page, documents_since, documents_until)
            _screenshot(page, args.screenshot_dir, "31_documents_page_wide")

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
            _screenshot(page, args.screenshot_dir, "32_after_docs")

            customer_id = customer_id or customer_id_from_docs
            if not customer_id:
                log.warning(
                    "Could not determine customer_id from XLS filename or "
                    "document URLs; run.json will omit it."
                )

            # --- run.json --------------------------------------------
            run_meta = {
                "timestamp": run_ts_str,
                "customer_id": customer_id,
                "accounts": {"file": "accounts.json", "entries": accounts},
                "transactions": txn_entries,
                "documents": doc_entries,
                "documents_window": {
                    "start": documents_since.isoformat(),
                    "end": documents_until.isoformat(),
                },
                "positions": {"file": "positions.xls"},
                "position_details": {
                    "file": "position_details.json",
                    "entries": position_details,
                },
                "list_of_assets": {"file": "list_of_assets.xls"},
                "account_overview": {"file": "account_overview.pdf"},
            }
            (run_dir / "run.json").write_text(
                json.dumps(run_meta, indent=2, sort_keys=True) + "\n",
                encoding="utf-8",
            )
            log.info(
                "Done. %d transaction window(s); %d new document(s).",
                len(txn_entries), len(doc_entries),
            )
            return 0
        finally:
            if args.trace:
                _maybe_stop_trace(
                    context, args.trace,
                    args.screenshot_dir / f"trace_download_{run_ts_str}.zip",
                )
            context.close()
            browser.close()


def main(argv: list[str] | None = None) -> int:
    args = parse_args(sys.argv[1:] if argv is None else argv)
    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )
    if args.trace and not args.screenshot_dir:
        raise SystemExit(
            "--trace requires --screenshot-dir. The trace bundle is "
            "written alongside screenshots; pick a directory that is "
            "NOT your secrets dir."
        )
    return run(args)


if __name__ == "__main__":
    sys.exit(main())
