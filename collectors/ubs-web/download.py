#!/usr/bin/env python3
"""
UBS Switzerland e-banking bronze downloader.

Reuses the Playwright session minted by login.py to export:
  - per-account transactions CSV   — `button-csvExport`
  - per-account transactions MT940 — `button-swiftMt940Export`
  - bank-document PDFs             — `/api/v1/digital-banking/files/…`
  - the credit-card surface        — the SPA's own card API (cards.py):
    the roster, each card account's ledger, its billing periods and
    their statement PDFs

Files land in <bronze-dir>/<UTC-timestamp>/<artefact>. Read-only — see
CLAUDE.md §1. Per CLAUDE.md §2, non-dry-run invocations must be
explicitly authorised.

Usage:
    download.py [--state-path <file>] [--bronze-dir <dir>]
                [--lookback PRESET|YYYY-MM-DD]
                [--dry-run] [--debug] [--screenshot-dir <dir>] [--trace]
"""

from __future__ import annotations

import argparse
import hashlib
import logging
import re
import sys
import time
from datetime import date, datetime, timedelta
from pathlib import Path
from urllib.parse import urlsplit, parse_qs

from collectorkit import bronze, cli, debugcap, launch, session

import cards  # local module
import landmarks as ubs  # local module

log = logging.getLogger("ubs-web.download")

# Hold the UA constant across login.py and download.py so UBS's
# anti-bot heuristics see a single session.
USER_AGENT = (
    "Mozilla/5.0 (X11; Linux x86_64) "
    "AppleWebKit/537.36 (KHTML, like Gecko) "
    "Chrome/151.0.0.0 Safari/537.36"
)

# The walk's passes, in the order it runs them. `--only` names one.
PASSES = ("positions", "transactions", "portfolio-transactions",
          "documents", "cards")

NAV_TIMEOUT_MS = 60_000
LANDMARK_TIMEOUT_MS = 30_000
DOWNLOAD_TIMEOUT_MS = 60_000
# The transaction list's chooser is behind two loads, not one: the SPA
# resolves the hash route, and the legacy application it hosts then
# builds its own frame and fetches the list into it. That is slower than
# any single landmark on the SPA's own pages, so it gets its own budget.
PORTFOLIO_SCOPE_TIMEOUT_MS = 90_000
# How long the list takes to come back after the filter is submitted.
PANEL_SETTLE_MS = 2_500
# How long the header's portfolio switcher takes to open or settle.
SWITCHER_SETTLE_MS = 1_000

# Canonical storageState location — must match login.py, which
# mints the file there (the wrapper's /secrets mount).
DEFAULT_STATE_PATH = Path("/secrets/ubs-web-state.json")
# Fallback state location (underscore) — read when the canonical file is absent,
# so a session stored under this name keeps working. login writes the canonical name.
LEGACY_STATE_PATH = Path("/secrets/ubs_web_state.json")

# The --lookback window resolves through collectorkit.cli (default
# DEFAULT_LOOKBACK_DAYS = 90). Important context for UBS: every
# surface is driven to the resolved window rather than left on its
# own default, because those defaults disagree with each other and
# with the requested window.
# - transactions UI defaults to "Maximum (current year and last 2
#   years)" — left alone it would fetch ~3y on every cron-style run.
# - documents page defaults to last 3 months — left alone it would
#   miss older PDFs on a wider run.

# Minimum window when bisecting either the documents list (999-row
# cap) or the MT940 export (1000-trx cap). If a single day still
# exceeds the cap, give up and warn — manual intervention needed.
WINDOW_MIN_DAYS = 1

# Maximum recursion depth for window-bisect, as an additional safety
# net against infinite recursion if the cap detection logic is wrong.
WINDOW_MAX_DEPTH = 20

# Emit a heartbeat every N document rows inside a window. Without it a
# window of several hundred PDFs logs nothing between its start and its
# end, and a walk that is merely slow reads as a hung one.
PROGRESS_EVERY = 25

# UBS's hard cap on a transaction export, in either format. The MT940
# variant-chooser dialog shows this number explicitly in its info
# banner; exceeding it replaces the chooser with the "max 1000
# transactions" info-only dialog (no Export button). CSV binds to the
# same cap and answers an over-cap click with a dialog of its own
# rather than a file, so both formats bisect their window on it.
TRX_EXPORT_CAP = 1000


# ============================================================
# CLI
# ============================================================

def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument("--state-path", type=Path, default=DEFAULT_STATE_PATH,
                   help="Path to the Playwright storageState.json minted by "
                        "login.py (default: %(default)s).")
    p.add_argument("--bronze-dir", type=Path, default=Path("/data"),
                   help="Bronze tree root (default: %(default)s, the wrapper's "
                        "/data mount); a UTC-timestamped subdir is created per run.")
    # Shared date-window contract: a single --lookback naming the
    # start of the [start..today] window. UBS's UI caps its own
    # 'Maximum' preset to ~3 years; older transactions are reached by
    # an ISO start date (e.g. --lookback 2015-01-01) or
    # --lookback all (~30y).
    cli.add_standard_args(p, verb="download")
    # Heavy passes are on by default and opted OUT of, per the fleet's
    # CLI contract: a bare `download` pulls everything there is.
    p.add_argument("--no-cards", action="store_true",
                   help="Skip the credit-card surface (roster, ledgers, "
                        "invoices and statement PDFs). Cards are fetched "
                        "by default.")
    p.add_argument("--no-card-statements", action="store_true",
                   help="Fetch the card invoices' figures but not their "
                        "statement PDFs. Narrower than --no-cards: the "
                        "periods and their balances still land, only the "
                        "rendered documents are skipped.")
    p.add_argument("--only", choices=sorted(PASSES), default=None,
                   metavar="PASS",
                   help=(f"Run one pass and skip the rest ({', '.join(sorted(PASSES))}). "
                         "The dump it writes is a partial one and is marked as "
                         "such, so `load` and `prune` treat it as the fragment "
                         "it is; it is for working on a single surface without "
                         "paying for the whole walk."))
    p.add_argument("--dry-run", action="store_true",
                   help="Validate session and selectors; do not export "
                        "anything. Use to confirm the UI hasn't shifted "
                        "before triggering a real run.")
    p.add_argument("--screenshot-dir", default=None, type=Path,
                   help="If set, write a screenshot at each landmark.")
    p.add_argument("--trace", action="store_true",
                   help="Capture a Playwright trace bundle. Requires "
                        "--screenshot-dir; the bundle lands there alongside "
                        "screenshots.")
    p.add_argument("--debug", action="store_true",
                   help="Save opt-in debug captures (DOM + screenshot) INSIDE "
                        "the bronze run dir under <run>/screenshots/: the "
                        "homepage the account and portfolio anchors are "
                        "scraped from, the documents list once its filters "
                        "render, and the page any account's transaction export "
                        "failed on. Off by default — the captures are never "
                        "read by `load`, and `prune` reclaims "
                        "<run>/screenshots/ from complete dumps. No-op under "
                        "--dry-run, which persists nothing to bronze. Distinct "
                        "from --screenshot-dir / --trace, whose per-landmark "
                        "screenshots and trace bundle land in the /debug "
                        "mount, outside bronze.")
    return p.parse_args(argv)


# ============================================================
# Screenshot / trace helpers
# ============================================================

def maybe_screenshot(page, screenshot_dir: Path | None, label: str) -> None:
    if screenshot_dir is None:
        return
    screenshot_dir.mkdir(parents=True, exist_ok=True)
    path = screenshot_dir / f"{bronze.ts_slug()}-{label}.png"
    page.screenshot(path=str(path), full_page=True)
    log.debug("wrote screenshot %s", path)


# ============================================================
# Playwright plumbing
# ============================================================

def _new_context(p, state_path: Path):
    if not state_path.is_file():
        raise SystemExit(
            f"No state file at {state_path}. Run login.py first."
        )
    browser = p.chromium.launch(
        headless=True,
        args=launch.chromium_args(
            "--no-sandbox",
            "--disable-dev-shm-usage",
            "--disable-blink-features=AutomationControlled",
        ),
    )
    context = browser.new_context(
        storage_state=str(state_path), user_agent=USER_AGENT,
        accept_downloads=True,
    )
    return browser, context


def _verify_session(page) -> None:
    """Navigate to the post-auth landing page and assert we are in."""
    page.goto(ubs.LOGIN_ENTRY_URL, wait_until="domcontentloaded")
    # Wait for either the post-auth SPA URL to settle OR the login
    # form to appear. Either way we have an answer in <= 30s.
    try:
        page.wait_for_function(
            "() => /\\/app\\/.*\\/ebanking\\/spa\\.html/.test(location.href) || "
            "      !!document.querySelector('input[name=\"loginalias\"]')",
            timeout=LANDMARK_TIMEOUT_MS,
        )
    except Exception:
        pass
    url = page.url
    if not ubs.is_post_auth_url(url):
        raise SystemExit(
            f"Session is dead (landed at {url}). Re-run login.py."
        )
    log.info("session OK, landed at %s", url)


# ============================================================
# Account enumeration (homepage scrape)
# ============================================================

def enumerate_accounts(page, screenshot_dir: Path | None) -> list[dict]:
    """Return a list of {account_id, kind, route, label} dicts.

    The homepage tile widget renders one anchor per account with a
    `target=…-transactions&accountId=<token>` query in its hash
    route. We scrape these and pair them with the visible row text
    (account display string), which UBS partially redacts but is
    enough to confirm correctness in run.json.
    """
    home_url = _build_spa_url(page.url, ubs.ROUTE_HOME)
    page.goto(home_url, wait_until="domcontentloaded")
    # Homepage is busy; wait for at least one transactions anchor
    # to be present rather than relying on networkidle.
    try:
        page.wait_for_selector(ubs.HOME_CASH_ACCOUNT_LINK_SELECTOR,
                               timeout=LANDMARK_TIMEOUT_MS)
    except Exception as e:
        maybe_screenshot(page, screenshot_dir, "home-no-cash-anchors")
        raise SystemExit(
            f"Could not find any cash-account links on the homepage. "
            f"UBS may have redesigned the tile widget. ({e})"
        ) from e
    maybe_screenshot(page, screenshot_dir, "home-rendered")

    # Cash accounts only: the card area is a separate family of surfaces
    # and silver models no card yet. `explore.py` is where that surface
    # is being mapped.
    accounts: dict[str, dict] = {}
    for selector, kind in (
        (ubs.HOME_CASH_ACCOUNT_LINK_SELECTOR, "cash"),
    ):
        rows = page.evaluate(
            """(sel) => Array.from(document.querySelectorAll(sel))
                 .map(a => ({
                     href: a.getAttribute('href'),
                     label: (a.closest('[role="region"], section, div') || a.parentElement)
                              ?.innerText?.trim()?.replace(/\\s+/g, ' ')?.slice(0, 200) || '',
                 }))""",
            selector,
        )
        for row in rows:
            href = row["href"] or ""
            account_id = _extract_account_id(href)
            if not account_id:
                continue
            if account_id in accounts:
                continue
            accounts[account_id] = {
                "account_id": account_id,
                "kind": kind,
                "route": _href_to_route(href),
                "label": row["label"],
            }
    log.info("discovered %d cash account(s)", len(accounts))
    return list(accounts.values())


def _extract_account_id(href: str) -> str | None:
    """Pull accountId=… out of a hash-fragment query."""
    # Hash-fragment URLs do not parse cleanly with urlsplit's query
    # field, so we do it manually.
    if "#" in href:
        _, frag = href.split("#", 1)
    else:
        frag = href
    if "?" not in frag:
        return None
    _, qs = frag.split("?", 1)
    for kv in qs.split("&"):
        if kv.startswith("accountId="):
            return kv[len("accountId="):]
    return None


def _href_to_route(href: str) -> str:
    """Reduce a full or relative href down to its hash route."""
    if "#" in href:
        return "#" + href.split("#", 1)[1]
    return href


def _origin(url: str) -> str:
    """Return the `scheme://host[:port]` of a URL."""
    s = urlsplit(url)
    return f"{s.scheme}://{s.netloc}"


def _build_spa_url(current_url: str, hash_route: str) -> str:
    """Replace just the fragment of the post-auth SPA URL."""
    s = urlsplit(current_url)
    base = f"{s.scheme}://{s.netloc}{s.path}"
    if s.query:
        base += "?" + s.query
    return base + (hash_route if hash_route.startswith("#") else "#" + hash_route)


# ============================================================
# Per-account transactions export
# ============================================================

def export_transactions(page, account: dict, since: date, until: date,
                        run_dir: Path, screenshot_dir: Path | None) -> dict:
    """Download CSV + MT940 for one account: one file per format per
    window, and more than one window where the period exceeds the
    1000-transaction export cap."""
    route = account["route"]
    url = _build_spa_url(page.url, route)
    log.info("navigating to transactions for %s account %s",
             account["kind"], account["account_id"][:12])
    # Dismiss any lingering overlay before we navigate. A leftover
    # MT940 confirmation dialog (or notification popup, KYC survey,
    # etc.) intercepts clicks on the next page's filter buttons.
    _dismiss_open_overlays(page)
    page.goto(url, wait_until="domcontentloaded")
    _dismiss_open_overlays(page)
    try:
        page.wait_for_selector(ubs.TXN_COUNT_SELECTOR,
                               timeout=LANDMARK_TIMEOUT_MS)
    except Exception:
        maybe_screenshot(page, screenshot_dir, f"txn-no-count-{account['account_id'][:8]}")
        log.warning("transactions count selector did not appear; "
                    "proceeding without explicit readiness")

    out_dir = run_dir / "transactions"
    out_dir.mkdir(parents=True, exist_ok=True)

    # The count over the whole requested window, for run.json and the
    # filtered-view capture. Each export below re-reads it per window.
    _set_transaction_period_custom(
        page, since, until, screenshot_dir, account["account_id"],
    )
    full_count = _read_transaction_count(page)
    maybe_screenshot(page, screenshot_dir, f"txn-filtered-{account['account_id'][:8]}")

    # Both formats bisect on the same export cap.
    csv_paths, _, csv_gaps = _export_with_split(
        page, account, since, until, out_dir, screenshot_dir,
        depth=0, fmt="CSV", export=_export_csv,
    )
    mt940_paths, _, mt940_gaps = _export_with_split(
        page, account, since, until, out_dir, screenshot_dir,
        depth=0, fmt="MT940", export=_export_mt940,
    )

    log.info("exported %s account %s: %s transactions; "
             "csv chunks=%d; mt940 chunks=%d",
             account["kind"], account["account_id"][:12],
             full_count if full_count is not None else "?",
             len(csv_paths), len(mt940_paths))
    if csv_gaps or mt940_gaps:
        log.error("incomplete transaction export for %s account %s: "
                  "csv missing %s; mt940 missing %s. The statement PDFs "
                  "are the only record of those windows, and they carry "
                  "a poorer narrative than the export does.",
                  account["kind"], account["account_id"][:12],
                  _windows(csv_gaps) or "nothing",
                  _windows(mt940_gaps) or "nothing")
    return {
        **account,
        "since": since.isoformat(),
        "until": until.isoformat(),
        "transaction_count": full_count,
        "csv_filenames": [p.name for p in csv_paths],
        "csv_gaps": _windows(csv_gaps),
        "mt940_filenames": [p.name for p in mt940_paths],
        "mt940_gaps": _windows(mt940_gaps),
    }


def _windows(gaps: list[tuple[date, date]]) -> list[str]:
    """Un-exported windows as `YYYY-MM-DD..YYYY-MM-DD` strings, for the
    manifest and the log. Always written, empty list included, so a
    reader can tell a run that covered everything from one taken before
    the field existed."""
    return [f"{a.isoformat()}..{b.isoformat()}" for a, b in gaps]


def _export_with_split(page, account: dict, since: date, until: date,
                       out_dir: Path,
                       screenshot_dir: Path | None,
                       depth: int, fmt: str,
                       export) -> tuple[list[Path], int | None,
                                        list[tuple[date, date]]]:
    """Export one account's transactions, bisecting the window on the
    1000-trx cap. Returns the files written, the transaction count of
    the whole window this call was given, and the windows it could not
    export at all.

    That third value is why this function reports rather than warns.
    An export that yields nothing leaves the account's history to the
    statement-PDF reconstruction, which is a strictly poorer record of
    the same bookings — it prints upper-case and drops every accent a
    payee's name carries. The loss is invisible in the row count, so
    the gap is carried out to the manifest instead of ending in a log
    line nobody reads.

    The count is read from the filtered table BEFORE the export is
    attempted, because over the cap UBS answers the click with an
    info dialog instead of a file: waiting for a download that is
    never produced costs DOWNLOAD_TIMEOUT_MS and yields nothing. The
    format differ only in how they are driven, so `export` is the
    per-window exporter and `fmt` names it in the log.

    Each bisect window does a FULL page.goto to the account URL
    before setting the period. The UBS DatePicker remembers prior
    Custom-mode values across popover open/close cycles within the
    same SPA route, and Playwright's React-fill doesn't reliably
    update the React `_valueTracker` after the first set. Navigating
    fresh forces the entire component tree (including the popover's
    DatePicker) to re-mount with default state, after which the fill
    + Apply path works as on the first call.
    """
    if depth > WINDOW_MAX_DEPTH:
        log.warning("%s max bisect depth at [%s..%s]; chunk skipped",
                    fmt, since, until)
        return [], None, [(since, until)]
    _navigate_to_account_fresh(page, account)
    applied_since, applied_until = _set_transaction_period_custom(
        page, since, until, screenshot_dir, account["account_id"],
    )
    count = _read_transaction_count(page)
    log.debug("%s window [%s..%s] (applied [%s..%s]) trx count: %s",
              fmt, since, until, applied_since, applied_until, count)
    if count is not None and count > TRX_EXPORT_CAP:
        if (applied_until - applied_since).days <= WINDOW_MIN_DAYS:
            log.error("%s cap exceeded in 1-day window [%s..%s] "
                      "(%d trx). Apply additional filters manually.",
                      fmt, applied_since, applied_until, count)
            return [], count, [(applied_since, applied_until)]
        # Bisect within the APPLIED window — UBS may have clamped
        # `since` (or `until`) due to its retention limit, and
        # bisecting the original window would just keep hitting the
        # same clamp on every recursion.
        mid = applied_since + (applied_until - applied_since) // 2
        left, _, left_gaps = _export_with_split(
            page, account, applied_since, mid, out_dir, screenshot_dir,
            depth + 1, fmt, export)
        right, _, right_gaps = _export_with_split(
            page, account, mid + timedelta(days=1), applied_until, out_dir,
            screenshot_dir, depth + 1, fmt, export)
        return left + right, count, left_gaps + right_gaps
    path = export(page, account, out_dir, applied_since, applied_until)
    if path is None:
        return [], count, [(applied_since, applied_until)]
    return [path], count, []


def _navigate_to_docs_fresh(page) -> None:
    """Fresh navigation to the documents page, closing any open
    dialog and waiting for the filter row to render."""
    _dismiss_open_overlays(page)
    url = _build_spa_url(page.url, ubs.ROUTE_DOCUMENTS)
    page.goto(url, wait_until="domcontentloaded")
    _dismiss_open_overlays(page)
    try:
        page.wait_for_selector(ubs.DOC_FILTER_BUTTON_SELECTOR,
                               timeout=LANDMARK_TIMEOUT_MS)
    except Exception:
        pass


def _navigate_to_account_fresh(page, account: dict) -> None:
    """Navigate to the account's transactions URL and wait for the
    count selector to render. Always closes any open dialog first so
    a leftover MT940 confirm doesn't intercept the navigation."""
    _dismiss_open_overlays(page)
    url = _build_spa_url(page.url, account["route"])
    page.goto(url, wait_until="domcontentloaded")
    _dismiss_open_overlays(page)
    try:
        page.wait_for_selector(ubs.TXN_COUNT_SELECTOR,
                               timeout=LANDMARK_TIMEOUT_MS)
    except Exception:
        # Count selector isn't required for the export to work; let
        # the period-set code raise if anything's truly broken.
        pass


def _read_transaction_count(page) -> int | None:
    """Parse the '(NNNN)' count rendered next to the table heading."""
    try:
        text = page.locator(ubs.TXN_COUNT_SELECTOR).first.inner_text(
            timeout=LANDMARK_TIMEOUT_MS,
        )
    except Exception:
        return None
    m = re.search(r"(\d+)", text)
    return int(m.group(1)) if m else None


def _set_transaction_period_custom(page, since: date, until: date,
                                   screenshot_dir: Path | None,
                                   account_id: str) -> tuple[date, date]:
    """Open the Period combobox, switch to Custom, fill from/to.

    Returns the actually-applied (since, until) window. UBS may
    clamp `since` (or `until`) to its accepted range — the
    transactions UI is hard-capped at ~28 months — and the caller
    needs the clamped values to bisect correctly.
    """
    _dismiss_open_overlays(page)
    page.locator(ubs.TXN_PERIOD_FILTER_SELECTOR).first.click()
    _click_custom_radio(page, screenshot_dir, f"txn-custom-radio-missing-{account_id[:8]}")
    applied_since, applied_until = _fill_custom_dates(
        page, since, until, screenshot_dir,
        f"txn-date-inputs-missing-{account_id[:8]}",
    )
    # Give the table a moment to re-render with the new window.
    page.wait_for_timeout(1500)
    return applied_since, applied_until


def _fmt_dmy(d: date) -> str:
    return d.strftime("%d.%m.%Y")


def _clamp_dates_to_popover_limit(page, since: date,
                                  until: date) -> tuple[date, date] | None:
    """Inspect the open Period popover for inline date-range errors
    and return clamped `(since, until)` if any are found.

    Returns None if no error is visible (dates are accepted as-is)."""
    try:
        text = page.locator("dialog[open]").first.inner_text(timeout=2000)
    except Exception:
        return None
    new_since, new_until = since, until
    changed = False
    m = ubs.PERIOD_POPOVER_MIN_DATE_RE.search(text)
    if m:
        min_date = datetime.strptime(m.group(1), "%d.%m.%Y").date()
        # "must be LATER than" → first allowed day is min_date + 1.
        candidate = min_date + timedelta(days=1)
        if candidate > new_since:
            new_since = candidate
            changed = True
    m = ubs.PERIOD_POPOVER_MAX_DATE_RE.search(text)
    if m:
        max_date = datetime.strptime(m.group(1), "%d.%m.%Y").date()
        candidate = max_date - timedelta(days=1)
        if candidate < new_until:
            new_until = candidate
            changed = True
    if not changed:
        return None
    if new_since > new_until:
        return None
    return (new_since, new_until)


def _react_fill(locator, value: str) -> None:
    """Set the value of a React-controlled input so React state
    actually updates.

    React tracks its own copy of an input's value via an internal
    `_valueTracker`. Plain DOM `value=...` (which Playwright's
    `fill()` ultimately calls) writes to the element but doesn't
    update the tracker, so React's onChange handler short-circuits
    and the form submits the previous value. The standard fix is to
    call the prototype's native value setter — which bypasses React's
    own descriptor — and then dispatch a bubbling `input` event."""
    locator.evaluate(
        """(el, value) => {
            const proto = Object.getPrototypeOf(el);
            const setter = Object.getOwnPropertyDescriptor(proto, 'value').set;
            setter.call(el, '');
            el.dispatchEvent(new Event('input', { bubbles: true }));
            setter.call(el, value);
            el.dispatchEvent(new Event('input', { bubbles: true }));
            el.dispatchEvent(new Event('change', { bubbles: true }));
        }""",
        value,
    )


def _click_custom_radio(page, screenshot_dir: Path | None,
                        screenshot_label: str) -> None:
    """Click the 'Custom' radio in the open Period popover.

    Scoped to `dialog[open]` so we don't accidentally pick a
    `[role="radio"]` elsewhere on the page. We deliberately do NOT
    pre-click the Predefined radio: that auto-stages Maximum (the
    first predefined preset) and Apply then submits Maximum, silently
    overriding the Custom dates we typed afterwards.

    The "subsequent-open staleness" problem (DatePicker remembers
    previous values, so React doesn't see the new fill as a change)
    is worked around by the caller: each MT940 bisect window does a
    full `page.goto` to the account URL first, so the popover is
    always re-mounted in a fresh state.
    """
    try:
        page.locator(
            f'dialog[open] {ubs.PERIOD_POPOVER_CUSTOM_RADIO}'
        ).first.click(timeout=LANDMARK_TIMEOUT_MS)
    except Exception:
        maybe_screenshot(page, screenshot_dir, screenshot_label)
        raise SystemExit(
            "Could not click the 'Custom' radio in the Period popover."
        ) from None
    page.wait_for_timeout(200)


def _fill_custom_dates(page, since: date, until: date,
                       screenshot_dir: Path | None,
                       screenshot_label: str) -> tuple[date, date]:
    """Fill the From/To date inputs in an open Period popover and
    click Apply. Returns the actually-applied (since, until) tuple
    — which may differ from the input if UBS clamps the dates.

    UBS's Apply button only enables when the form values actually
    differ from the currently-applied filter. If a previous call
    already set the same period, Apply stays disabled — that means
    "no change needed", so we close the popover instead of waiting
    forever on Apply.

    Both surfaces render two text inputs with placeholder
    "DD.MM.YYYY"; we locate by placeholder and fill .nth(0) /
    .nth(1). The Apply button is `[data-name="filter-item-submit-
    button"]` on the transactions surface and a plain
    `<button type="submit">` on the documents surface — we prefer
    the data-name and fall back to the submit button.
    """
    inputs = page.locator(ubs.PERIOD_POPOVER_DATE_INPUTS)
    try:
        inputs.nth(0).wait_for(state="visible", timeout=LANDMARK_TIMEOUT_MS)
    except Exception:
        maybe_screenshot(page, screenshot_dir, screenshot_label)
        raise SystemExit(
            "Could not find the date inputs in the Period popover."
        ) from None
    n = inputs.count()
    if n < 2:
        maybe_screenshot(page, screenshot_dir, screenshot_label)
        raise SystemExit(
            f"Expected two date inputs in the Period popover, found {n}."
        )
    # UBS's date inputs are React-controlled. Plain Playwright
    # `fill()` updates the DOM value but does NOT trigger React's
    # internal `_valueTracker`, so the DatePicker treats the value
    # as unchanged and Apply stays disabled. We use the
    # React-native-setter trick (set DOM value via the native
    # property setter, then dispatch input + change events) which
    # is the canonical workaround for React-controlled inputs.
    _react_fill(inputs.nth(0), _fmt_dmy(since))
    _react_fill(inputs.nth(1), _fmt_dmy(until))
    inputs.nth(1).press("Tab")

    # If the popover validation rejects `since` for being too old
    # (UBS caps transactions at ~28 months), it shows an inline
    # error like "The start date must be later than DD.MM.YYYY".
    # Clamp `since` to the offered minimum and retry — once.
    applied_since, applied_until = since, until
    clamped = _clamp_dates_to_popover_limit(page, since, until)
    if clamped is not None:
        new_since, new_until = clamped
        log.info("popover rejected dates; clamping to [%s..%s]",
                 new_since, new_until)
        _react_fill(inputs.nth(0), _fmt_dmy(new_since))
        _react_fill(inputs.nth(1), _fmt_dmy(new_until))
        inputs.nth(1).press("Tab")
        applied_since, applied_until = new_since, new_until

    # Pick the Apply button. Try the explicit data-name first; fall
    # back to the submit button inside the popover form.
    primary = page.locator(ubs.PERIOD_POPOVER_APPLY_BUTTON_PRIMARY).first
    apply_btn = primary if primary.count() > 0 else \
        page.locator(ubs.PERIOD_POPOVER_APPLY_BUTTON_FALLBACK).first
    # Wait up to a short window for Apply to enable. If it never
    # enables, the popover already shows the requested range — close
    # it and return without erroring.
    try:
        page.wait_for_function(
            "([primarySel, fallbackSel]) => {"
            "  const b = document.querySelector(primarySel)"
            "        || document.querySelector(fallbackSel);"
            "  return b && !b.disabled;"
            "}",
            arg=[ubs.PERIOD_POPOVER_APPLY_BUTTON_PRIMARY,
                 ubs.PERIOD_POPOVER_APPLY_BUTTON_FALLBACK],
            timeout=3000,
        )
    except Exception:
        log.debug("Apply button never enabled — period already set; "
                  "closing popover without applying")
        # Send Escape to close the dialog without applying.
        page.keyboard.press("Escape")
        page.wait_for_timeout(300)
        return applied_since, applied_until
    apply_btn.click(timeout=LANDMARK_TIMEOUT_MS)
    return applied_since, applied_until


def _export_csv(page, account: dict, out_dir: Path,
                since: date, until: date) -> Path | None:
    """CSV export — a click downloads the file directly, as long as
    the period is within the export cap. Over it, UBS answers with an
    info dialog and no download ever arrives; the caller bisects the
    window to stay under, and the dialog handling here is the net for
    a cap this code failed to predict."""
    button = page.locator(ubs.TXN_BUTTON_CSV_SELECTOR).first
    try:
        button.wait_for(state="visible", timeout=LANDMARK_TIMEOUT_MS)
    except Exception:
        log.warning("no CSV export button for %s account %s",
                    account["kind"], account["account_id"][:12])
        return None
    try:
        with page.expect_download(timeout=DOWNLOAD_TIMEOUT_MS) as dl_info:
            button.click()
        download = dl_info.value
    except Exception as e:
        log.warning("CSV export click did not produce a download for "
                    "%s account %s over [%s..%s] — likely the "
                    "%d-transaction cap: %s",
                    account["kind"], account["account_id"][:12],
                    since, until, TRX_EXPORT_CAP, e)
        # Whatever it opened instead of downloading would intercept
        # the next click, so clear it before handing the page back.
        _dismiss_open_overlays(page)
        return None
    return _save_download(download, account, out_dir, since, until,
                          default_ext="csv")


def _export_mt940(page, account: dict, out_dir: Path,
                  since: date, until: date) -> Path | None:
    """MT940 export — clicks the button, handles the modal dialog,
    selects the enriched variant, and saves the resulting download.

    UBS opens a `<dialog>` titled "Select the file variant to export"
    with two radios (light / enriched) and an Export button. When
    the period contains more than 1000 transactions, UBS replaces
    the variant chooser with an "A maximum of 1000 transactions can
    be exported" info dialog (Close only); we detect that by the
    Export button being absent and skip cleanly.
    """
    _dismiss_open_overlays(page)
    button = page.locator(ubs.TXN_BUTTON_MT940_SELECTOR).first
    if button.count() == 0:
        # MT940 button absent — account doesn't support the format.
        # Should not happen for cash accounts in normal use; bail
        # cleanly so the surrounding flow can record the omission.
        return None
    try:
        button.wait_for(state="visible", timeout=LANDMARK_TIMEOUT_MS)
    except Exception:
        return None
    button.click()
    # Wait for either Export button OR the cap-exceeded info dialog.
    export_btn = page.locator(ubs.MT940_DIALOG_EXPORT_BUTTON).first
    try:
        # The Export button starts disabled; just wait for the
        # element to render (visible state is True even when disabled).
        export_btn.wait_for(state="attached", timeout=LANDMARK_TIMEOUT_MS)
    except Exception:
        # Variant dialog never appeared — most likely the 1000-cap
        # info-only dialog. Try to close it and bail.
        log.warning("MT940 dialog did not show an Export button for "
                    "%s account %s — likely the 1000-transaction cap; "
                    "narrow the period and re-run",
                    account["kind"], account["account_id"][:12])
        _close_mt940_dialog(page)
        return None
    # Select the enriched variant (more fields than the light one).
    # Click the label rather than the native radio — the radio
    # itself is visually hidden, so Playwright's strict visibility
    # check rejects it.
    label = page.locator(ubs.MT940_DIALOG_RADIO_ENRICHED_LABEL).first
    label.wait_for(state="visible", timeout=LANDMARK_TIMEOUT_MS)
    label.click()
    # Wait for Export to become enabled.
    try:
        page.wait_for_function(
            "() => { const b = document.querySelector("
            "  '[data-testid=\"export-button\"]'); "
            "  return b && !b.disabled; }",
            timeout=LANDMARK_TIMEOUT_MS,
        )
    except Exception:
        log.warning("MT940 Export button did not enable after radio click")
        _close_mt940_dialog(page)
        return None
    # Now Export becomes enabled; click + capture the download.
    try:
        with page.expect_download(timeout=DOWNLOAD_TIMEOUT_MS) as dl_info:
            export_btn.click()
        download = dl_info.value
    except Exception as e:
        log.warning("MT940 Export click did not produce a download: %s", e)
        _close_mt940_dialog(page)
        return None
    return _save_download(download, account, out_dir, since, until,
                          default_ext="mt940")


def _close_mt940_dialog(page) -> None:
    """Best-effort close of an open MT940 modal dialog."""
    for sel in (ubs.MT940_DIALOG_CANCEL_BUTTON, ubs.MT940_DIALOG_CLOSE_BUTTON):
        try:
            loc = page.locator(sel).first
            if loc.count() > 0 and loc.is_visible():
                loc.click(timeout=2000)
                page.wait_for_timeout(300)
                return
        except Exception:
            continue


def _dismiss_open_overlays(page, max_attempts: int = 3) -> None:
    """Close any open `<dialog>` overlay. Idempotent — silently no-op
    when nothing is open. Used before navigating between accounts so
    a lingering MT940 / KYC / notification popup from the previous
    surface doesn't intercept clicks on the next one."""
    for _ in range(max_attempts):
        try:
            open_count = page.evaluate(
                "() => document.querySelectorAll('dialog[open]').length",
            )
        except Exception:
            return
        if open_count == 0:
            return
        # Try Escape first (closes most native and ARIA-modal dialogs).
        try:
            page.keyboard.press("Escape")
            page.wait_for_timeout(300)
        except Exception:
            pass
        # If still open, click any Close / Cancel button inside.
        for sel in (
            'dialog[open] [aria-label="Close"]',
            'dialog[open] [data-testid="cancel-download-button"]',
            'dialog[open] button:has-text("Cancel")',
            'dialog[open] button:has-text("Close")',
        ):
            try:
                loc = page.locator(sel).first
                if loc.count() > 0 and loc.is_visible():
                    loc.click(timeout=1000)
                    page.wait_for_timeout(200)
                    break
            except Exception:
                continue


def _save_download(download, account: dict, out_dir: Path,
                   since: date, until: date, default_ext: str) -> Path:
    """Persist a Playwright Download to bronze.

    UBS suggests filenames that may embed the account number; we
    rename to use a hash of the full opaque account-id token plus
    the window bounds. Hashing avoids the long-common-prefix
    collision: UBS account tokens all start with the same ~12-char
    customer prefix, plus a ~22-char depot prefix shared by multiple
    accounts of the same customer, so simple slice-based shortening
    collides silently."""
    short_id = bronze.short_token(account["account_id"])
    ext = _suggest_extension(download.suggested_filename, default_ext)
    window = f"{since:%Y%m%d}_{until:%Y%m%d}"
    out_path = out_dir / f"{account['kind']}_{short_id}_{window}.{ext}"
    download.save_as(str(out_path))
    return out_path


def _suggest_extension(suggested: str | None, fmt: str) -> str:
    """Pick a sane file extension. UBS suggests filenames; trust them
    when they end in something obvious, otherwise fall back to fmt."""
    if suggested:
        ext = Path(suggested).suffix.lstrip(".").lower()
        if ext:
            return ext
    return {"csv": "csv", "mt940": "mt940", "pdf": "pdf"}.get(fmt, "bin")


# ============================================================
# Bank documents harvest
# ============================================================

def harvest_documents(page, since: date, until: date, run_dir: Path,
                      context, screenshot_dir: Path | None,
                      debug: bool = False, mask=None) -> list[dict]:
    """Scrape document URLs across [since, until], paginating
    through the 999-row UBS cap by bisecting the window.

    ``debug`` additionally writes the settled documents page into
    ``run_dir/screenshots/`` — the DOM the window-bisect walk reads its
    counts and rows out of, masked through ``mask`` (the run's session
    mask) on the way. Only reached on a real run, so ``run_dir`` is always
    a real dir here."""
    if mask is None:
        mask = debugcap.SessionMask()
    docs_dir = run_dir / "documents"
    docs_dir.mkdir(parents=True, exist_ok=True)
    _dismiss_open_overlays(page)
    docs_url = _build_spa_url(page.url, ubs.ROUTE_DOCUMENTS)
    page.goto(docs_url, wait_until="domcontentloaded")
    _dismiss_open_overlays(page)
    try:
        page.wait_for_selector(ubs.DOC_FILTER_BUTTON_SELECTOR,
                               timeout=LANDMARK_TIMEOUT_MS)
    except Exception:
        maybe_screenshot(page, screenshot_dir, "docs-no-filters")
        if debug:
            debugcap.capture_page(page, run_dir, "30-documents", log=log,
                                  redact=mask.for_page(page))
        raise SystemExit(
            "Documents page filter buttons did not appear. Session "
            "may have expired, or UBS may have redesigned the page."
        ) from None
    maybe_screenshot(page, screenshot_dir, "docs-rendered")
    if debug:
        debugcap.capture_page(page, run_dir, "30-documents", log=log,
                              redact=mask.for_page(page))

    seen_tokens: set[str] = set()
    harvested: list[dict] = []
    _walk_window(page, context, since, until,
                 seen_tokens, harvested, docs_dir, screenshot_dir, depth=0)
    log.info("harvested %d documents across [%s..%s]",
             len(harvested), since.isoformat(), until.isoformat())
    return harvested


def _walk_window(page, context, since: date, until: date,
                 seen_tokens: set[str], harvested: list[dict],
                 docs_dir: Path, screenshot_dir: Path | None,
                 depth: int) -> None:
    """Recursive window-bisect against UBS's 999-doc cap.

    Each bisect window navigates fresh to the documents URL before
    applying the period filter. Same rationale as
    `_export_with_split`: the docs page's DatePicker has the
    same React-controlled-input staleness as the transactions page,
    so applying a new period without a fresh navigate silently
    leaves the previous filter in place.
    """
    if depth > WINDOW_MAX_DEPTH:
        log.warning("max window depth reached at [%s..%s]; some "
                    "documents in this window may be missed",
                    since, until)
        return
    log.info("docs window: [%s..%s] depth=%d", since, until, depth)
    _navigate_to_docs_fresh(page)
    applied_since, applied_until = _apply_docs_period_custom(
        page, since, until, screenshot_dir,
    )
    if (applied_since, applied_until) != (since, until):
        log.info("docs popover clamped to [%s..%s]",
                 applied_since, applied_until)
    count = _read_doc_count(page)
    log.info("docs window [%s..%s] reports %s rows",
             applied_since, applied_until, count)
    if count is not None and count >= ubs.DOC_LIST_CAP:
        if (applied_until - applied_since).days <= WINDOW_MIN_DAYS:
            log.error("999-cap hit at a 1-day window [%s..%s] — UBS is "
                      "returning more than %d docs in a single day. "
                      "Apply additional filters manually.",
                      applied_since, applied_until, ubs.DOC_LIST_CAP)
            return
        mid = applied_since + (applied_until - applied_since) // 2
        _walk_window(page, context, applied_since, mid,
                     seen_tokens, harvested, docs_dir,
                     screenshot_dir, depth + 1)
        _walk_window(page, context, mid + timedelta(days=1), applied_until,
                     seen_tokens, harvested, docs_dir,
                     screenshot_dir, depth + 1)
        return

    # Below the cap → harvest. Pull every <a href*="/api/v1/digital-
    # banking/files/…">; UBS renders both the document-name link and
    # the trailing download icon as anchors pointing at the same URL,
    # so we dedup by token.
    rows = page.evaluate(
        """(sel) => Array.from(document.querySelectorAll(sel))
             .map(a => ({
                 href: a.getAttribute('href'),
                 label: (a.closest('[role="row"], li, tr, .UWR_ListItem_listItem_VfPXY') || a)
                          ?.innerText?.trim()?.replace(/\\s+/g, ' ')?.slice(0, 240) || '',
             }))""",
        ubs.DOC_LINK_SELECTOR,
    )
    page_origin = _origin(page.url)
    new_in_window = 0
    total_rows = len(rows)
    for n, row in enumerate(rows, start=1):
        # A window can hold hundreds of PDFs, each one its own request.
        # Logging only at the window boundary leaves the walk silent for
        # minutes at a stretch, which is indistinguishable from a hang.
        if total_rows >= PROGRESS_EVERY and n % PROGRESS_EVERY == 0:
            log.info("docs window [%s..%s]: %d/%d rows examined",
                     since, until, n, total_rows)
        href = row["href"] or ""
        m = ubs.DOC_FILES_API_URL_RE.match(href)
        if not m:
            continue
        token = m.group(1)
        if token in seen_tokens:
            continue
        seen_tokens.add(token)
        # Resolve relative hrefs against the page's origin.
        abs_url = href if href.startswith("http") else page_origin + href
        meta = _fetch_document(context, abs_url, token, row["label"], docs_dir)
        if meta:
            harvested.append(meta)
            new_in_window += 1
    log.info("docs window [%s..%s] harvested %d new (%d total)",
             since, until, new_in_window, len(harvested))


def _apply_docs_period_custom(page, since: date, until: date,
                              screenshot_dir: Path | None) -> tuple[date, date]:
    """Open the docs Period filter, switch to Custom, fill from/to.

    Returns the actually-applied (since, until) — UBS may clamp if
    the dates fall outside the docs retention horizon."""
    _dismiss_open_overlays(page)
    page.locator(ubs.DOC_FILTER_BUTTON_SELECTOR).nth(
        ubs.DOC_FILTER_INDEX_PERIOD,
    ).click()
    _click_custom_radio(page, screenshot_dir, "docs-custom-radio-missing")
    applied = _fill_custom_dates(page, since, until, screenshot_dir,
                                 "docs-date-inputs-missing")
    # Give the list a moment to re-render before we scrape.
    page.wait_for_timeout(2000)
    return applied


def _read_doc_count(page) -> int | None:
    """Parse 'Bank documents (NNN)' — the count rendered next to
    the section heading on the documents page. Returns None if the
    counter has not yet rendered."""
    try:
        node = page.locator("h2,h3").filter(
            has_text=re.compile(r"\(\s*\d+\s*\)"),
        ).first
        text = node.inner_text(timeout=5000)
    except Exception:
        return None
    m = re.search(r"\((\s*\d+)\s*\)", text)
    return int(m.group(1)) if m else None


def _fetch_document(context, href: str, token: str,
                    label: str, docs_dir: Path) -> dict | None:
    """Fetch one PDF and persist it to bronze. Returns metadata or
    None on failure. The PDF is stored under a content-addressed name
    (`<sha256>.pdf`), so an unchanged document lands at the same filename
    every run instead of accreting a fresh per-session-token name each
    re-download. Its logical identity (`content_sha256` + `label`) is
    recorded in the returned metadata for later dedup/skip tooling. The
    caller already dedups by token before calling, and each run writes a
    fresh dir, so there is no pre-fetch skip to preserve.

    The href is the per-row download URL, including a query
    `apikey=<tenant-key>` and `Accept=application/pdf`. Cookies are
    inherited from the Playwright context, so no auth gymnastics."""
    try:
        resp = context.request.get(href, timeout=DOWNLOAD_TIMEOUT_MS)
    except Exception as e:
        # The href carries an apikey in its query and a request error
        # reproduces the whole call log; neither belongs in a log line.
        log.warning("doc fetch failed for token %s…: %s",
                    token[:12], debugcap.safe_error(e))
        return None
    if not resp.ok:
        log.warning("doc fetch HTTP %d for token %s…", resp.status, token[:12])
        return None
    body = resp.body()
    if not body or not body.startswith(b"%PDF-"):
        log.warning("doc fetch returned non-PDF for token %s… (%d bytes, "
                    "first 8: %r)", token[:12], len(body or b""), body[:8])
        return None
    sha = hashlib.sha256(body).hexdigest()
    out_path = docs_dir / f"{sha}.pdf"
    # Two rows in one run can resolve to identical bytes (the same
    # statement reachable under two tokens): same bytes, same path, so a
    # redundant write is a no-op we skip.
    if not (out_path.exists() and out_path.stat().st_size == len(body)):
        out_path.write_bytes(body)
    log.debug("wrote %s (%d bytes)", out_path.name, len(body))
    return {
        "token": token,
        "filename": out_path.name,
        "content_sha256": sha,
        "label": label,
        "url": _strip_apikey(href),
        "size_bytes": len(body),
    }


def _strip_apikey(href: str) -> str:
    """Don't persist the apikey query param in run.json — it is a
    long-lived tenant secret. Token + Accept are enough to re-fetch
    by re-running download.py (which gets a fresh apikey from the
    rendered DOM)."""
    s = urlsplit(href)
    qs = parse_qs(s.query)
    qs.pop("apikey", None)
    flat = "&".join(f"{k}={v[0]}" for k, v in qs.items())
    return f"{s.scheme}://{s.netloc}{s.path}" + (f"?{flat}" if flat else "")


# ============================================================
# Card surface
# ============================================================

def _capture_cards(context, page, apikey_holder: dict, run_dir: Path,
                   since: date, until: date, *, statements: bool) -> dict | None:
    """Capture the card roster, ledgers, invoices and statements.

    Returns the manifest block, or None when the surface could not be
    reached. A card failure never ends the run: the cash exports and the
    document archive are already on disk by this point, and a dump that
    is short one facet and says so beats a dump that does not exist.
    """
    apikey = cards.pick_apikey(apikey_holder)
    if not apikey:
        log.warning("no apikey observed on the SPA's own API calls; "
                    "skipping the card surface. (It is a request header, "
                    "harvested from live traffic — a session that served "
                    "no API call cannot yield one.)")
        return None
    api = cards.CardApi(context, _origin(page.url), apikey)
    try:
        return cards.capture(api, run_dir, since, until, statements=statements)
    except Exception as e:  # noqa: BLE001 — one facet, not the run
        # Not log.exception: a request client's traceback re-renders its
        # call log, which carries the api key and the session cookie.
        log.warning("card capture failed: %s", debugcap.safe_error(e))
        return {"error": debugcap.safe_error(e)}


# ============================================================
# run.json writing
# ============================================================

def write_run_json(run_dir: Path, since: date, until: date,
                   accounts: list[dict], documents: list[dict],
                   positions: list[dict],
                   cards_meta: dict | None = None,
                   portfolio_transactions: list[dict] | None = None,
                   only: str | None = None) -> None:
    payload = {
        "dump_started_at": bronze.ts_slug(),
        # Terminal status for the run.json lifecycle: this function is
        # only reached at the end of a real walk (--dry-run writes no
        # bronze at all — see _prepare_run_dir), so the run finalises as
        # "complete". The atomic write below overwrites the
        # "in-progress" marker dropped at run-dir creation. `prune` keys
        # on this field; the `dry_run` bool is kept alongside it because
        # the statusless-manifest fallback classifies on it (see
        # prune._is_complete).
        # A walk that ran one pass covers one surface, and saying
        # "complete" of it would tell every reader the others were
        # looked at and found empty. It is named for what it is, which
        # also puts it among the dumps `prune` reclaims: it exists to
        # work on a surface, not to be the record of a day.
        "status": "complete" if only is None else "partial",
        **({"partial_pass": only} if only is not None else {}),
        "dry_run": False,
        # One window for the whole run: transactions, documents and
        # positions are all fetched over it. (Dumps predating the single
        # --lookback flag carry a separate since/until on each block.)
        "window": {
            "since": since.isoformat(),
            "until": until.isoformat(),
        },
        "transactions": {
            "accounts": accounts,
        },
        # The managed portfolios' own movements, one export per
        # portfolio scope. Absent on a dump taken before the surface was
        # harvested, which is what tells a reader the dump predates it
        # rather than that the portfolios were idle.
        **({"portfolio_transactions": {
            "count": len(portfolio_transactions),
            "items": portfolio_transactions,
        }} if portfolio_transactions is not None else {}),
        "documents": {
            "count": len(documents),
            "items": documents,
        },
        "positions": {
            "captured_at": bronze.ts_slug(),
            "count": len(positions),
            "items": positions,
        },
        # None when --no-cards was passed or the surface was unreachable;
        # a reader tells "not fetched" from "fetched and empty" by the
        # key being absent rather than by an empty account list.
        **({"cards": cards_meta} if cards_meta is not None else {}),
    }
    bronze.atomic_write_json(run_dir / "run.json", payload)


def _prepare_run_dir(bronze_dir: Path, dry_run: bool) -> Path | None:
    """Create the bronze run dir and drop the "in-progress" marker.

    Real run: make `bronze_dir`, create a fresh UTC-timestamped run dir
    under it, and atomically write ``run.json`` with
    ``status="in-progress"`` up front — so a walk that crashes before
    ``write_run_json`` leaves a non-complete dump that `prune` reclaims
    and `load` can classify.

    Dry-run: create NOTHING under `bronze_dir` and return ``None``. A
    dry-run is the read-only walk (CLAUDE.md §2) and must persist nothing
    to bronze — not even a `run.json` shell, since `load`'s `scan_bronze`
    has no status guard and would otherwise ingest it as a dump run.
    """
    if dry_run:
        log.info(
            "dry-run: nothing written to bronze (--bronze-dir %s untouched)",
            bronze_dir)
        return None
    bronze_dir.mkdir(parents=True, exist_ok=True)
    run_dir = bronze_dir / bronze.ts_slug()
    run_dir.mkdir(parents=True, exist_ok=False)
    log.info("bronze dir: %s", run_dir)
    bronze.atomic_write_json(run_dir / "run.json", {"status": "in-progress"})
    return run_dir


# ============================================================
# Positions snapshot
# ============================================================

def export_positions(page, run_dir: Path,
                     screenshot_dir: Path | None) -> list[dict]:
    """Download positions.csv per portfolio.

    UBS's positions page renders ONE portfolio at a time. The default
    (`preselectFirstPortfolio=true`) only covers the first portfolio
    of the active banking relationship; users with multiple portfolios
    (for example a managed one beside a self-directed one) need a
    per-portfolio sweep. We enumerate (portfolioUid,
    bankingRelationId) pairs from the homepage and download one CSV
    per portfolio. The flat default-positions CSV is still captured
    as a fallback when no portfolioUids are discoverable."""
    out_dir = run_dir / "positions"
    out_dir.mkdir(parents=True, exist_ok=True)
    portfolios = _enumerate_portfolios(page, screenshot_dir)
    results: list[dict] = []
    # Always pull the consolidated default-portfolio view, even when
    # we have explicit portfolioUids. UBS may file a number of
    # customer-facing cash accounts under a synthetic catch-all
    # portfolio code in the default view rather than under a named
    # portfolio. Skipping the consolidated view leaves those
    # accounts entirely out of silver.
    log.info("downloading default consolidated positions")
    meta = _download_positions_csv(
        page, ubs.ROUTE_POSITIONS_DEFAULT, out_dir,
        filename="positions.csv", label="default",
        screenshot_dir=screenshot_dir,
    )
    if meta:
        results.append(meta)
    if not portfolios:
        log.warning("no portfolioUid anchors found on homepage — "
                    "default view is the only positions snapshot")
        return results
    log.info("discovered %d portfolio(s) on homepage", len(portfolios))
    for p in portfolios:
        route = ubs.positions_url_for_portfolio(
            p["portfolio_uid"], p["banking_relation_id"],
        )
        short = bronze.short_token(p["portfolio_uid"])
        meta = _download_positions_csv(
            page, route, out_dir,
            filename=f"positions_{short}.csv",
            label=short,
            screenshot_dir=screenshot_dir,
        )
        if meta:
            meta["portfolio_uid"] = p["portfolio_uid"]
            meta["banking_relation_id"] = p["banking_relation_id"]
            results.append(meta)
    return results


def _enumerate_portfolios(page,
                          screenshot_dir: Path | None) -> list[dict]:
    """Scrape (portfolioUid, bankingRelationId) pairs from homepage."""
    _dismiss_open_overlays(page)
    home_url = _build_spa_url(page.url, ubs.ROUTE_HOME)
    page.goto(home_url, wait_until="domcontentloaded")
    try:
        page.wait_for_selector(ubs.HOME_PORTFOLIO_LINK_SELECTOR,
                               timeout=LANDMARK_TIMEOUT_MS)
    except Exception:
        maybe_screenshot(page, screenshot_dir, "home-no-portfolio-anchors")
        return []
    rows = page.evaluate(
        """(sel) => Array.from(document.querySelectorAll(sel))
             .map(a => a.getAttribute('href'))
             .filter(h => h)""",
        ubs.HOME_PORTFOLIO_LINK_SELECTOR,
    )
    seen: set[tuple[str, str]] = set()
    portfolios: list[dict] = []
    for href in rows:
        uid = _extract_query_param(href, "portfolioUid")
        rel = _extract_query_param(href, "bankingRelationId")
        if not uid or not rel:
            continue
        key = (uid, rel)
        if key in seen:
            continue
        seen.add(key)
        portfolios.append({"portfolio_uid": uid, "banking_relation_id": rel})
    return portfolios


def _extract_query_param(href: str, name: str) -> str | None:
    """Pull a query-string param out of a URL whose query lives in
    the hash fragment (SPA-style)."""
    if "#" in href:
        _, frag = href.split("#", 1)
    else:
        frag = href
    if "?" not in frag:
        return None
    _, qs = frag.split("?", 1)
    for kv in qs.split("&"):
        if "=" not in kv:
            continue
        k, v = kv.split("=", 1)
        if k == name:
            return v
    return None


def _download_positions_csv(page, hash_route: str, out_dir: Path,
                            filename: str, label: str,
                            screenshot_dir: Path | None) -> dict | None:
    """Navigate to a positions page (default or per-portfolio) and
    save its CSV to `out_dir/filename`."""
    _dismiss_open_overlays(page)
    url = _build_spa_url(page.url, hash_route)
    log.info("navigating to positions: %s", label)
    page.goto(url, wait_until="domcontentloaded")
    _dismiss_open_overlays(page)
    csv_btn = page.locator(ubs.POSITIONS_BUTTON_CSV_SELECTOR).first
    try:
        csv_btn.wait_for(state="visible", timeout=LANDMARK_TIMEOUT_MS)
    except Exception:
        maybe_screenshot(page, screenshot_dir, f"positions-{label}-no-csv-button")
        log.warning("positions CSV button did not appear for %s; skipping", label)
        return None
    maybe_screenshot(page, screenshot_dir, f"positions-{label}-rendered")
    try:
        with page.expect_download(timeout=DOWNLOAD_TIMEOUT_MS) as dl_info:
            csv_btn.click()
        download = dl_info.value
    except Exception as e:
        log.warning("positions CSV click did not produce a download (%s): %s",
                    label, e)
        return None
    out_path = out_dir / filename
    download.save_as(str(out_path))
    log.info("positions CSV saved: %s (%d bytes)",
             out_path.name, out_path.stat().st_size)
    return {
        "filename": out_path.name,
        "size_bytes": out_path.stat().st_size,
        "captured_at": bronze.ts_slug(),
    }


# ============================================================
# Portfolio securities transactions
# ============================================================

def export_portfolio_transactions(page, run_dir: Path,
                                  since: date, until: date,
                                  screenshot_dir: Path | None) -> list[dict]:
    """Download every managed portfolio's securities transactions.

    The cash pass above reaches only the accounts the homepage files as
    cash tiles. A managed portfolio's own movements — its trades, and
    the corporate actions against its holdings — are on the separate
    surface `landmarks.securities_transactions_url_for_portfolio` names,
    a legacy application the SPA hosts in child frames.

    It is reached THROUGH the portfolio overview rather than directly:
    both are hash routes on the same bundle, but arriving at the list
    cold leaves those frames unbuilt, and the walk sees a route that
    rendered with nothing in it.

    The route names one portfolio — the only one the homepage offers a
    `portfolioUid` for — so the rest are reached the way a person
    reaches them, through the switcher in the SPA's header. Each is
    exported under the window this walk asks for, and a portfolio
    already harvested is not fetched twice: the switcher also lists
    consolidated views, which report the same bookings under an id of
    their own."""
    out_dir = run_dir / "portfolio_transactions"
    out_dir.mkdir(parents=True, exist_ok=True)
    portfolios = _enumerate_portfolios(page, screenshot_dir)
    if not portfolios:
        log.warning("no portfolioUid anchors on the homepage; the portfolio "
                    "transaction surface cannot be opened")
        return []
    anchor = portfolios[0]
    _dismiss_open_overlays(page)
    page.goto(_build_spa_url(page.url, ubs.portfolio_overview_url_for_portfolio(
        anchor["portfolio_uid"], anchor["banking_relation_id"])),
        wait_until="domcontentloaded")
    route = ubs.securities_transactions_url_for_portfolio(
        anchor["portfolio_uid"], anchor["banking_relation_id"])
    page.goto(_build_spa_url(page.url, route), wait_until="domcontentloaded")

    titles = _portfolio_switcher_options(page)
    if not titles:
        log.warning("the portfolio switcher offered nothing; only the "
                    "portfolio this route names is harvested")
        titles = [None]
    else:
        log.info("the switcher offers %d portfolio(s)", len(titles))
    results: list[dict] = []
    harvested: set[str] = set()
    for title in titles:
        if title is not None and not _switch_portfolio(page, title):
            continue
        meta = _fetch_portfolio_transactions_csv(
            page, since, until, out_dir, route)
        if not meta:
            continue
        # The switcher lists the relationship and the consolidated views
        # beside the real portfolios, and those answer for a portfolio
        # already fetched. What came back says which it was.
        if meta["portfolio"] and meta["portfolio"] in harvested:
            Path(out_dir / meta["filename"]).unlink(missing_ok=True)
            continue
        if meta["portfolio"]:
            harvested.add(meta["portfolio"])
        results.append(meta)
    return results


def _portfolio_switcher_options(page) -> list[str]:
    """The portfolios the header's switcher offers, by their titles.

    They exist only while it is open, so it is opened to read them and
    closed again — leaving it open would cover the list underneath."""
    try:
        page.click(ubs.PORTFOLIO_SWITCHER_BUTTON, timeout=LANDMARK_TIMEOUT_MS)
        page.wait_for_timeout(SWITCHER_SETTLE_MS)
        titles = page.eval_on_selector_all(
            ubs.PORTFOLIO_SWITCHER_ITEM,
            "els => els.map(e => (e.innerText || '').trim()).filter(Boolean)")
        page.keyboard.press("Escape")
        page.wait_for_timeout(SWITCHER_SETTLE_MS)
    except Exception as e:  # noqa: BLE001
        log.warning("could not read the portfolio switcher (%s)",
                    debugcap.safe_error(e))
        return []
    # The first option is the banking relationship the portfolios hang
    # from, which is not one of them.
    return [t for t in dict.fromkeys(titles)][1:]


def _switch_portfolio(page, title: str) -> bool:
    """Put the SPA on one portfolio, through the header's switcher."""
    try:
        page.click(ubs.PORTFOLIO_SWITCHER_BUTTON, timeout=LANDMARK_TIMEOUT_MS)
        page.wait_for_timeout(SWITCHER_SETTLE_MS)
        page.click(f'{ubs.PORTFOLIO_SWITCHER_ITEM}:text-is("{title}")',
                   timeout=LANDMARK_TIMEOUT_MS)
        # Switching rebuilds the legacy frames under the route, which is
        # what the filter panel and the export are then found in.
        page.wait_for_timeout(PANEL_SETTLE_MS)
        return True
    except Exception as e:  # noqa: BLE001
        log.warning("could not switch to a portfolio (%s)",
                    debugcap.safe_error(e))
        return False


def _fetch_portfolio_transactions_csv(page, since: date, until: date,
                                      out_dir: Path,
                                      route: str) -> dict | None:
    """The loaded portfolio's transactions over [since..until].

    The window is set through the filter panel and the file taken from
    the list's own CSV button, because the surface answers an export
    from the list it is showing rather than from anything a request
    restates — a reconstructed request is answered with the rendered
    page however faithfully it is built.

    The answer states the period it covers, and that is checked: a
    window that was not applied comes back as the list's own default,
    which would otherwise be stored as though it were the answer. A
    start later than asked for is different — that is the archive's
    floor, and what it returns is real."""
    if not _apply_portfolio_window(page, since, until):
        return None
    body = _click_portfolio_export(page)
    if body is None:
        return None
    text = body.decode("utf-8-sig", errors="replace")
    if not text.lstrip().startswith("Valuation date;"):
        log.warning("the portfolio export produced %d bytes that are not the "
                    "CSV header; skipping", len(body))
        return None
    covered = _export_window(text)
    if covered and covered[1] != _fmt_dmy(until):
        log.warning("portfolio export covers %s..%s but %s..%s was requested; "
                    "the period was not applied, so the file is not kept",
                    covered[0], covered[1], _fmt_dmy(since), _fmt_dmy(until))
        return None
    if covered and covered[0] != _fmt_dmy(since):
        log.info("portfolio archive begins %s; requested %s",
                 covered[0], _fmt_dmy(since))
    portfolio = _exported_portfolio(text)
    short = bronze.short_token(portfolio or route)
    out_path = out_dir / f"transactions_{short}.csv"
    out_path.write_bytes(body)
    log.info("portfolio transactions saved: %s (%d bytes)",
             out_path.name, len(body))
    return {
        "filename": out_path.name,
        "portfolio": portfolio,
        "size_bytes": len(body),
        "covered_from": covered[0] if covered else None,
        "covered_to": covered[1] if covered else None,
        "captured_at": bronze.ts_slug(),
    }


def _exported_portfolio(text: str) -> str | None:
    """The portfolio an export says it is for, from its first row.

    The file is named for this rather than for the scope asked of the
    surface, so a dump never claims a portfolio it does not hold."""
    rows = text.splitlines()
    if len(rows) < 2:
        return None
    cells = rows[1].split(";")
    return cells[2].strip() if len(cells) > 2 and cells[2].strip() else None


def _apply_portfolio_window(page, since: date, until: date) -> bool:
    """Put the transaction list on a window, through its own panel.

    The panel is a form in a frame of its own, and the fields carrying
    the window exist only there — so the window is set by filling them
    and submitting, and the page's own script builds the post. The
    surface then keeps that period for the portfolio until it is set
    again, which is also why an export never has to restate it.

    Works on the list the page is already showing. Re-navigating to the
    route here would undo the switch that put it on this portfolio: the
    route carries a `portfolioUid`, and it is the only one the homepage
    offers."""
    deadline = time.monotonic() + PORTFOLIO_SCOPE_TIMEOUT_MS / 1000
    panel = None
    while time.monotonic() < deadline and panel is None:
        for frame in page.frames:
            try:
                if frame.query_selector(ubs.TXN_FILTER_DATE_FROM):
                    panel = frame
                    break
            except Exception:  # noqa: BLE001 — a frame can navigate mid-read
                continue
        if panel is None:
            page.wait_for_timeout(500)
    if panel is None:
        log.warning("the filter panel never appeared; the window cannot be set")
        return False
    try:
        # Manual mode, which is what makes the dates count at all. The
        # radio is styled invisible, so it can be neither clicked nor
        # checked: it is set directly, and the panel's own transition —
        # which only reveals the fields — is run around it.
        panel.evaluate(_PANEL_MANUAL_MODE, ubs.TXN_FILTER_MANUAL_RADIO)
        # The panel is rendered twice, in the header and the sidebar,
        # and the button submits the FIRST form in the document. So the
        # fields filled are that form's own: filling the other copy
        # submits dates nobody touched, which the surface reads as no
        # period at all.
        if not panel.evaluate(_PANEL_MARK_DATE_FIELDS):
            log.warning("the submitted filter form carries no date fields")
            return False
        # Typed, not assigned: these are date-picker widgets that keep
        # their own value, and a value written onto the element changes
        # what the page shows and nothing the submission reads.
        panel.fill(_PANEL_FROM_MARK, _fmt_dmy(since))
        panel.fill(_PANEL_TO_MARK, _fmt_dmy(until))
        # Filling can clear the radio, so it is re-asserted at the last
        # moment before the form goes.
        panel.evaluate(_PANEL_MANUAL_MODE, ubs.TXN_FILTER_MANUAL_RADIO)
        panel.evaluate(_PANEL_SUBMIT)
    except Exception as e:  # noqa: BLE001
        log.warning("could not set the window (%s)", debugcap.safe_error(e))
        return False
    # The submit navigates the list frame in place; the export that
    # follows is answered from what it then holds.
    page.wait_for_timeout(PANEL_SETTLE_MS)
    return True


# Attributes the walk puts on the two date fields of the form that will
# actually be submitted, so they can be typed into by selector.
_PANEL_FROM_MARK = "[data-wdb-from]"
_PANEL_TO_MARK = "[data-wdb-to]"

_PANEL_MANUAL_MODE = """(sel) => {
    const radio = document.querySelector(sel);
    if (radio) radio.checked = true;
    if (typeof setToManual === 'function') setToManual();
    if (radio) radio.checked = true;
}"""

_PANEL_MARK_DATE_FIELDS = """() => {
    const submit = document.getElementsByName('filterSubmitButton')[0];
    const form = submit && submit.closest('form');
    if (!form) return false;
    const dates = form.querySelectorAll('input[data-date-format]');
    if (dates.length < 2) return false;
    dates[0].setAttribute('data-wdb-from', '1');
    dates[1].setAttribute('data-wdb-to', '1');
    return true;
}"""

_PANEL_SUBMIT = """() => {
    document.getElementsByName('filterSubmitButton')[0].click();
}"""


def _click_portfolio_export(page) -> bytes | None:
    """The list's own CSV export, as the file it produces."""
    frame = None
    deadline = time.monotonic() + LANDMARK_TIMEOUT_MS / 1000
    while time.monotonic() < deadline and frame is None:
        for candidate in page.frames:
            try:
                if candidate.query_selector(ubs.TXN_EXPORT_CSV_BUTTON):
                    frame = candidate
                    break
            except Exception:  # noqa: BLE001 — a frame can navigate mid-read
                continue
        if frame is None:
            page.wait_for_timeout(500)
    if frame is None:
        log.warning("the transaction list offered no CSV export")
        return None
    try:
        with page.expect_download(timeout=DOWNLOAD_TIMEOUT_MS) as info:
            frame.click(ubs.TXN_EXPORT_CSV_BUTTON)
        return Path(info.value.path()).read_bytes()
    except Exception as e:  # noqa: BLE001
        log.warning("the export produced no download (%s)",
                    debugcap.safe_error(e))
        return None


def _export_window(text: str) -> tuple[str, str] | None:
    """The period the export's own footer states, or None when absent."""
    for line in reversed(text.splitlines()):
        if line.startswith(ubs.TXN_EXPORT_FOOTER_PREFIX):
            m = ubs.TXN_EXPORT_FOOTER_RE.search(line)
            if m:
                return m.group(1), m.group(2)
    return None


# ============================================================
# Main
# ============================================================

def main(argv: list[str]) -> int:
    args = parse_args(argv)
    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )

    if args.trace and args.screenshot_dir is None:
        raise SystemExit("--trace requires --screenshot-dir.")

    since, until = cli.resolve_lookback(args)
    log.info("window: [%s..%s]", since, until)

    # A real run gets a bronze run dir plus an "in-progress" manifest,
    # atomically overwritten with the terminal status by write_run_json.
    # This makes a crashed walk — which never reaches write_run_json —
    # legible to `prune` (status="in-progress" ⇒ non-complete dump,
    # reclaimed whole once quiescent) instead of leaving an empty run
    # dir, and closes the window where a run dir carries no run.json at
    # all. A --dry-run persists nothing to bronze, so run_dir is None and
    # every export (and write_run_json) is skipped below.
    run_dir = _prepare_run_dir(args.bronze_dir, args.dry_run)

    # Captures live inside the run dir, so a --dry-run (run_dir is None,
    # by design — see _prepare_run_dir) has nowhere to put them and the
    # gate degrades to a warning rather than materialising a dir the walk
    # promised not to write.
    debug_dir = run_dir if args.debug else None
    if args.debug and run_dir is None:
        log.warning("--debug: --dry-run persists nothing to bronze; "
                    "no captures will be written")

    # The run's own credential. This walk is handed no password — it is
    # handed a lifted session — so the session jar is what a capture could
    # leak, and the SPA bootstraps its state into the markup a capture
    # serialises. Read once, at the first capture, and never fatally: a
    # mask that cannot be built must not take down the walk it exists to
    # diagnose.
    mask = debugcap.SessionMask()

    def capture(page, name: str) -> None:
        """Snapshot a landmark into the run dir; a no-op unless --debug."""
        if debug_dir is not None:
            debugcap.capture_page(page, debug_dir, name, log=log,
                                  redact=mask.for_page(page))

    from playwright.sync_api import sync_playwright

    rc = 0
    try:
        with sync_playwright() as pw:
            browser, context = _new_context(pw, session.resolve_state_path(
                args.state_path, DEFAULT_STATE_PATH, LEGACY_STATE_PATH))
            if args.trace:
                context.tracing.start(screenshots=True, snapshots=True, sources=True)
            # The card API authenticates with a header the SPA attaches to
            # its own calls; recording starts before any navigation so the
            # session-verify and homepage loads below supply it.
            apikey_holder = cards.sniff_apikey(context)
            page = context.new_page()
            page.set_default_navigation_timeout(NAV_TIMEOUT_MS)
            try:
                _verify_session(page)
                accounts = enumerate_accounts(page, args.screenshot_dir)
                # The homepage is scraped twice for anchors it may simply
                # not have — the cash accounts here, the portfolioUids a
                # moment later in export_positions — and both misses only
                # warn. This DOM is what says whether the anchors moved or
                # were genuinely absent.
                capture(page, "10-home")
                txn_results: list[dict] = []
                doc_results: list[dict] = []
                positions_meta: list[dict] = []
                portfolio_txn_meta: list[dict] = []
                cards_meta: dict | None = None
                if args.dry_run:
                    # Read-only walk: session verified and accounts
                    # enumerated above. Log the plan (what a real run
                    # would fetch) but write nothing — run_dir is None,
                    # so there is no bronze dump and load never sees one.
                    log.info("--dry-run set; skipping exports")
                    log.info("dry-run plan: would export positions + "
                             "transactions + documents for %d cash "
                             "account(s), plus one securities-transaction "
                             "export per managed portfolio, in [%s..%s]%s",
                             len(accounts), since, until,
                             "" if args.no_cards else
                             ", plus the card roster, each card's ledger "
                             "and its invoices" +
                             ("" if args.no_card_statements
                              else " and statement PDFs"))
                    txn_results = accounts  # echo discovery only
                else:
                    def run(name):
                        return args.only in (None, name)
                    if run("positions"):
                        positions_meta = export_positions(
                            page, run_dir, args.screenshot_dir,
                        )
                    for account in (accounts if run("transactions") else []):
                        try:
                            meta = export_transactions(
                                page, account, since, until, run_dir,
                                args.screenshot_dir,
                            )
                            txn_results.append(meta)
                        except SystemExit:
                            raise
                        except Exception as e:  # noqa: BLE001
                            log.exception("transactions export failed for %s "
                                          "account %s: %s",
                                          account["kind"],
                                          account["account_id"][:12], e)
                            # Failure path only: the page is still on the
                            # surface that raised, so this is the DOM the
                            # failing step saw. One capture per account
                            # would bury it. Named by the sha256 prefix the
                            # export filenames already use.
                            capture(page, "20-txn-"
                                    f"{bronze.short_token(account['account_id'])}"
                                    "-failed")
                    try:
                        if run("portfolio-transactions"):
                            portfolio_txn_meta = export_portfolio_transactions(
                                page, run_dir, since, until,
                                args.screenshot_dir,
                            )
                    except SystemExit:
                        raise
                    except Exception as e:  # noqa: BLE001
                        log.exception("portfolio transactions export "
                                      "failed: %s", e)
                        capture(page, "25-portfolio-txn-failed")
                    if run("documents"):
                        doc_results = harvest_documents(
                            page, since, until, run_dir,
                            context, args.screenshot_dir, debug=args.debug,
                            mask=mask,
                        )
                    if not args.no_cards and run("cards"):
                        cards_meta = _capture_cards(
                            context, page, apikey_holder, run_dir,
                            since, until,
                            statements=not args.no_card_statements,
                        )
                    write_run_json(run_dir, since, until,
                                   txn_results, doc_results,
                                   positions_meta, cards_meta,
                                   portfolio_txn_meta, args.only)
            finally:
                if args.trace:
                    args.screenshot_dir.mkdir(parents=True, exist_ok=True)
                    trace_path = args.screenshot_dir / f"{bronze.ts_slug()}-download-trace.zip"
                    context.tracing.stop(path=str(trace_path))
                    log.info("trace saved to %s", trace_path)
                context.close()
                browser.close()
    except SystemExit:
        raise
    except Exception as e:  # noqa: BLE001
        log.exception("download failed: %s", e)
        rc = 1
    return rc


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
