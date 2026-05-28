#!/usr/bin/env python3
"""
UBS Switzerland e-banking bronze downloader.

Reuses the Playwright session minted by login.py to export:
  - per-account transactions CSV   — `button-csvExport`
  - per-account transactions MT940 — `button-swiftMt940Export`
  - bank-document PDFs             — `/api/v1/digital-banking/files/…`

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
import hashlib
import json
import logging
import re
import sys
from datetime import date, datetime, timedelta, timezone
from pathlib import Path
from urllib.parse import urlsplit, parse_qs

import landmarks as ubs  # local module

log = logging.getLogger("ubs-web-dump.download")

# Hold the UA constant across login.py and download.py so UBS's
# anti-bot heuristics see a single session.
USER_AGENT = (
    "Mozilla/5.0 (X11; Linux x86_64) "
    "AppleWebKit/537.36 (KHTML, like Gecko) "
    "Chrome/147.0.0.0 Safari/537.36"
)

NAV_TIMEOUT_MS = 60_000
LANDMARK_TIMEOUT_MS = 30_000
DOWNLOAD_TIMEOUT_MS = 60_000

# Default lookback when --since is missing: 90 days. UBS's
# transactions UI defaults to "Maximum (current year and last 2
# years)" — so without --since we'd accidentally fetch ~3y of data
# on every cron-style run. 90d matches the swissquote-dump default
# for parity; users doing a one-off historic backfill must pass
# --since explicitly (e.g. --since 2015-01-01).
DEFAULT_LOOKBACK_DAYS = 90

# Documents page default window is shorter (last 3 months). Without
# --documents-since we'd miss anything older. We default both to
# --since/--until to keep one knob for the common case.

# Minimum window when bisecting either the documents list (999-row
# cap) or the MT940 export (1000-trx cap). If a single day still
# exceeds the cap, give up and warn — manual intervention needed.
WINDOW_MIN_DAYS = 1

# Maximum recursion depth for window-bisect, as an additional safety
# net against infinite recursion if the cap detection logic is wrong.
WINDOW_MAX_DEPTH = 20

# UBS's hard cap on MT940 export. The variant-chooser dialog shows
# this number explicitly in its info banner. Exceeding it triggers
# the "max 1000 transactions" info-only dialog (no Export button).
MT940_TRX_CAP = 1000


# ============================================================
# CLI
# ============================================================

def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument("--state-path", required=True, type=Path,
                   help="Path to the Playwright storageState.json.")
    p.add_argument("--dest", required=True, type=Path,
                   help="Output dir; a UTC-timestamped subdir is created per run.")
    p.add_argument("--since", type=date.fromisoformat, default=None,
                   help=("Earliest transaction date (YYYY-MM-DD). "
                         f"Default: {DEFAULT_LOOKBACK_DAYS} days before --until. "
                         "UBS's UI caps the 'Maximum' preset to ~3y; for older "
                         "data pass --since explicitly (e.g. 2015-01-01)."))
    p.add_argument("--until", type=date.fromisoformat, default=None,
                   help="Latest transaction date (YYYY-MM-DD, inclusive). "
                        "Default: today (UTC).")
    p.add_argument("--documents-since", type=date.fromisoformat, default=None,
                   help="Earliest document date (YYYY-MM-DD). Default: "
                        "same as --since. The Documents page filter defaults "
                        "to the last 3 months; without this flag older PDFs "
                        "would be missed.")
    p.add_argument("--documents-until", type=date.fromisoformat, default=None,
                   help="Latest document date (YYYY-MM-DD, inclusive). "
                        "Default: same as --until.")
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
    p.add_argument("-v", "--verbose", action="store_true",
                   help="DEBUG-level logging.")
    return p.parse_args(argv)


def resolve_windows(args: argparse.Namespace) -> tuple[date, date, date, date]:
    """Fill in defaults for --since/--until/--documents-since/-until."""
    today = datetime.now(timezone.utc).date()
    until = args.until or today
    since = args.since or (until - timedelta(days=DEFAULT_LOOKBACK_DAYS))
    if since > until:
        raise SystemExit(f"--since {since} is after --until {until}")
    docs_until = args.documents_until or until
    docs_since = args.documents_since or since
    if docs_since > docs_until:
        raise SystemExit(
            f"--documents-since {docs_since} is after --documents-until {docs_until}"
        )
    return since, until, docs_since, docs_until


# ============================================================
# Screenshot / trace helpers
# ============================================================

def ts_slug() -> str:
    return datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")


def maybe_screenshot(page, screenshot_dir: Path | None, label: str) -> None:
    if screenshot_dir is None:
        return
    screenshot_dir.mkdir(parents=True, exist_ok=True)
    path = screenshot_dir / f"{ts_slug()}-{label}.png"
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
        args=[
            "--no-sandbox",
            "--disable-dev-shm-usage",
            "--disable-blink-features=AutomationControlled",
        ],
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
        )
    maybe_screenshot(page, screenshot_dir, "home-rendered")

    # Cash accounts only. Card accounts (credit-card transactions) are
    # intentionally skipped — this is a wealth-management toolkit, not
    # a personal-finance one. Card data is not modelled in silver.
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
    """Download CSV (one file) + MT940 (one or more files, depending
    on whether the period exceeds the 1000-trx cap) for one account."""
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

    # --- CSV: one shot over the full window (no observed cap) ---
    applied_since, applied_until = _set_transaction_period_custom(
        page, since, until, screenshot_dir, account["account_id"],
    )
    full_count = _read_transaction_count(page)
    maybe_screenshot(page, screenshot_dir, f"txn-filtered-{account['account_id'][:8]}")
    csv_path = _export_csv(page, account, out_dir, applied_since, applied_until)

    # --- MT940: bisect on the 1000-trx cap ---
    mt940_paths = _export_mt940_with_split(
        page, account, since, until, out_dir, screenshot_dir, depth=0,
    )

    log.info("exported %s account %s: %s transactions; csv=%s; mt940 chunks=%d",
             account["kind"], account["account_id"][:12],
             full_count if full_count is not None else "?",
             csv_path.name if csv_path else None, len(mt940_paths))
    return {
        **account,
        "since": since.isoformat(),
        "until": until.isoformat(),
        "transaction_count": full_count,
        "csv_filename": csv_path.name if csv_path else None,
        "mt940_filenames": [p.name for p in mt940_paths],
    }


def _export_mt940_with_split(page, account: dict, since: date, until: date,
                             out_dir: Path,
                             screenshot_dir: Path | None,
                             depth: int) -> list[Path]:
    """MT940 export with automatic window bisection on the 1000-trx cap.

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
        log.warning("MT940 max bisect depth at [%s..%s]; chunk skipped",
                    since, until)
        return []
    _navigate_to_account_fresh(page, account)
    applied_since, applied_until = _set_transaction_period_custom(
        page, since, until, screenshot_dir, account["account_id"],
    )
    count = _read_transaction_count(page)
    log.debug("MT940 window [%s..%s] (applied [%s..%s]) trx count: %s",
              since, until, applied_since, applied_until, count)
    if count is not None and count > MT940_TRX_CAP:
        if (applied_until - applied_since).days <= WINDOW_MIN_DAYS:
            log.error("MT940 cap exceeded in 1-day window [%s..%s] "
                      "(%d trx). Apply additional filters manually.",
                      applied_since, applied_until, count)
            return []
        # Bisect within the APPLIED window — UBS may have clamped
        # `since` (or `until`) due to its retention limit, and
        # bisecting the original window would just keep hitting the
        # same clamp on every recursion.
        mid = applied_since + (applied_until - applied_since) // 2
        return (
            _export_mt940_with_split(page, account, applied_since, mid,
                                     out_dir, screenshot_dir, depth + 1)
            + _export_mt940_with_split(page, account, mid + timedelta(days=1),
                                       applied_until, out_dir,
                                       screenshot_dir, depth + 1)
        )
    path = _export_mt940(page, account, out_dir, applied_since, applied_until)
    return [path] if path else []


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
        )
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
        )
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
    """CSV export — direct download on click (no dialog)."""
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
        log.warning("CSV export click did not produce a download: %s", e)
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
    short_id = _account_short_id(account["account_id"])
    ext = _suggest_extension(download.suggested_filename, default_ext)
    window = f"{since:%Y%m%d}_{until:%Y%m%d}"
    out_path = out_dir / f"{account['kind']}_{short_id}_{window}.{ext}"
    download.save_as(str(out_path))
    return out_path


def _account_short_id(account_id: str) -> str:
    """First 16 hex chars of sha256(account_id). Stable + collision-
    safe across UBS's "all-accounts-share-a-prefix" tokenisation."""
    return hashlib.sha256(account_id.encode("utf-8")).hexdigest()[:16]


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
                      context, screenshot_dir: Path | None) -> list[dict]:
    """Scrape document URLs across [since, until], paginating
    through the 999-row UBS cap by bisecting the window."""
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
        raise SystemExit(
            "Documents page filter buttons did not appear. Session "
            "may have expired, or UBS may have redesigned the page."
        )
    maybe_screenshot(page, screenshot_dir, "docs-rendered")

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
    `_export_mt940_with_split`: the docs page's DatePicker has the
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
    for row in rows:
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
    None on failure.

    The href is the per-row download URL, including a query
    `apikey=<tenant-key>` and `Accept=application/pdf`. Cookies are
    inherited from the Playwright context, so no auth gymnastics."""
    out_path = docs_dir / f"{token[:32]}.pdf"
    if out_path.exists() and out_path.stat().st_size > 0:
        # Already pulled in a previous invocation; skip without
        # re-hitting UBS.
        return {
            "token": token,
            "filename": out_path.name,
            "label": label,
            "url": _strip_apikey(href),
            "skipped_existing": True,
        }
    try:
        resp = context.request.get(href, timeout=DOWNLOAD_TIMEOUT_MS)
    except Exception as e:
        log.warning("doc fetch failed for token %s…: %s", token[:12], e)
        return None
    if not resp.ok:
        log.warning("doc fetch HTTP %d for token %s…", resp.status, token[:12])
        return None
    body = resp.body()
    if not body or not body.startswith(b"%PDF-"):
        log.warning("doc fetch returned non-PDF for token %s… (%d bytes, "
                    "first 8: %r)", token[:12], len(body or b""), body[:8])
        return None
    out_path.write_bytes(body)
    log.debug("wrote %s (%d bytes)", out_path.name, len(body))
    return {
        "token": token,
        "filename": out_path.name,
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
# run.json writing
# ============================================================

def write_run_json(run_dir: Path, since: date, until: date,
                   docs_since: date, docs_until: date,
                   accounts: list[dict], documents: list[dict],
                   positions: list[dict],
                   dry_run: bool) -> None:
    payload = {
        "dump_started_at": ts_slug(),
        "dry_run": dry_run,
        "transactions": {
            "since": since.isoformat(),
            "until": until.isoformat(),
            "accounts": accounts,
        },
        "documents": {
            "since": docs_since.isoformat(),
            "until": docs_until.isoformat(),
            "count": len(documents),
            "items": documents,
        },
        "positions": {
            "captured_at": ts_slug(),
            "count": len(positions),
            "items": positions,
        },
    }
    (run_dir / "run.json").write_text(
        json.dumps(payload, indent=2), encoding="utf-8",
    )


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
        # Filename includes a sha256 prefix of the portfolioUid — the
        # raw token is too long and case-mixed for safe filenames.
        short = hashlib.sha256(p["portfolio_uid"].encode()).hexdigest()[:16]
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
        "captured_at": ts_slug(),
    }


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

    since, until, docs_since, docs_until = resolve_windows(args)
    log.info("transactions window: [%s..%s]; documents window: [%s..%s]",
             since, until, docs_since, docs_until)

    args.dest.mkdir(parents=True, exist_ok=True)
    run_dir = args.dest / ts_slug()
    run_dir.mkdir(parents=True, exist_ok=False)
    log.info("bronze dir: %s", run_dir)

    from playwright.sync_api import sync_playwright

    rc = 0
    try:
        with sync_playwright() as pw:
            browser, context = _new_context(pw, args.state_path)
            if args.trace:
                context.tracing.start(screenshots=True, snapshots=True, sources=True)
            page = context.new_page()
            page.set_default_navigation_timeout(NAV_TIMEOUT_MS)
            try:
                _verify_session(page)
                accounts = enumerate_accounts(page, args.screenshot_dir)
                txn_results: list[dict] = []
                doc_results: list[dict] = []
                positions_meta: list[dict] = []
                if args.dry_run:
                    log.info("--dry-run set; skipping exports")
                    txn_results = accounts  # echo discovery only
                else:
                    positions_meta = export_positions(
                        page, run_dir, args.screenshot_dir,
                    )
                    for account in accounts:
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
                    doc_results = harvest_documents(
                        page, docs_since, docs_until, run_dir,
                        context, args.screenshot_dir,
                    )
                write_run_json(run_dir, since, until, docs_since, docs_until,
                               txn_results, doc_results, positions_meta,
                               args.dry_run)
            finally:
                if args.trace:
                    args.screenshot_dir.mkdir(parents=True, exist_ok=True)
                    trace_path = args.screenshot_dir / f"{ts_slug()}-download-trace.zip"
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
