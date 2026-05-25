#!/usr/bin/env python3
"""
Fidelity client-web scraper — bronze fetch.

Invoked from login.py's keep-alive loop with a live Camoufox
BrowserContext + the post-auth Page + the parsed trigger-file
config. Drives the read-only export surfaces on
``digital.fidelity.com`` and writes a timestamped bronze directory
under ``<dest>/<UTC-ts>/``.

Five phases, each gated by ``config['mode']`` (``all`` runs them
in order, otherwise the named single phase):

* ``positions`` — Switch to the all-accounts view, pick a preset
  (``Overview`` then ``DividendView``), kebab → Download for each;
  Fidelity returns one CSV per view containing rows for every
  visible account.

* ``activity`` — Navigate to Activity & Orders once, drive the
  page-level timepicker filter (preset 'Past 90 days' for the
  rolling default; Custom-tab + per-window bisection for a
  historic ``--since/--until`` backfill), then for each window
  click the Download trigger and grab the CSV. The export is
  consolidated across all visible accounts — the
  ``Account Number`` column inside the CSV is the per-row
  account discriminator; clicking the account-selector before
  the export does NOT scope it.

* ``documents`` — Visit the Statements sub-page and walk the
  per-row download buttons; visit the Tax-forms sub-page and walk
  the direct download anchors across every year in the TimeFilter
  select. Prospectuses + supplementary material are out of scope
  (DESIGN.md §8.5).

* ``balances`` — Persist ``balances.html``. Fidelity offers no
  direct CSV / Excel export on this surface; the on-page values
  live in ``data-testid$='-totalaccountvalue-label'`` for silver
  to scrape.

* ``performance`` — Persist ``performance.html``. Pure Highcharts
  UI with no structured export.

This module is NOT a standalone script. The host-side wrapper's
``./fidelity-web-dump download`` writes the trigger file; the
login keep-alive loop polls that file and calls :func:`walk`.
"""

from __future__ import annotations

import hashlib
import json
import logging
import re
import time
from datetime import datetime, timedelta, timezone
from pathlib import Path


log = logging.getLogger("fidelity-web-dump.download")


# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------

URL_PORTFOLIO_SUMMARY = "https://digital.fidelity.com/ftgw/digital/portfolio/summary"
URL_POSITIONS = "https://digital.fidelity.com/ftgw/digital/portfolio/positions"
URL_ACTIVITY = "https://digital.fidelity.com/ftgw/digital/portfolio/activity"
URL_DOCUMENTS = "https://digital.fidelity.com/ftgw/digital/portfolio/documents"
URL_BALANCES = "https://digital.fidelity.com/ftgw/digital/portfolio/balances"
URL_PERFORMANCE = "https://digital.fidelity.com/ftgw/digital/portfolio/performance"
POST_AUTH_PREFIX = "https://digital.fidelity.com/ftgw/digital/portfolio/"

# Account-selector account-link testid pattern is
# ``ap143528-accounts-selector-account-link-<account-id>``, with
# the id being 9-digit for brokerage / trust / 529 and 7-digit
# for a Fidelity Charitable DAF (auto-excluded by length in
# walk()). The pattern is used inline by
# ``enumerate_account_dimensions``; the named selectors below
# cover the rest of the positions phase.
SEL_ALL_ACCOUNTS = ".acct-selector__all-accounts"
SEL_KEBAB_MENU = "[data-testid='kebab-menu']"
SEL_PRESET_VIEW_SELECT = "[data-testid='preset-views-dropdown'] select"

# Timing.
KEBAB_OPEN_WAIT_S = 2.0
TABLE_RENDER_WAIT_S = 3.0
SPA_HYDRATE_WAIT_S = 5.0
DOWNLOAD_TIMEOUT_MS = 30_000

# Fidelity's documented per-request cap on activity exports.
# Custom-range requests longer than this are bisected into
# ≤MAX_ACTIVITY_WINDOW_DAYS-day windows, one CSV per window.
MAX_ACTIVITY_WINDOW_DAYS = 93


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

def ts_slug():
    return datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")


def account_key(account_external_id):
    """First 16 hex chars of sha256(id) — the on-disk filename
    discriminator per DESIGN §2. Keeps ``ls`` of a bronze dir from
    leaking real Fidelity account numbers."""
    return hashlib.sha256(
        account_external_id.encode("ascii")
    ).hexdigest()[:16]


def parse_exclude_list(value):
    if not value:
        return set()
    return {x.strip() for x in value.split(",") if x.strip()}


def live_url(page):
    """``page.evaluate('() => location.href')`` — workaround for the
    Camoufox 135 + Playwright 1.49 ``frameNavigated`` propagation
    bug. ``page.url`` stays stuck on the pre-redirect URL after
    navigation; ``location.href`` returns Firefox's actual document
    URL. (Same workaround login.py uses.)"""
    try:
        result = page.evaluate("() => location.href")
        if isinstance(result, str) and result:
            return result
    except Exception:
        pass
    return page.url


def enumerate_account_dimensions(page):
    """Return one dict per account: ``{account_id, portfolio,
    nickname}``. The portfolio is the account-selector group
    label rendered above each block of accounts (e.g.
    ``"Education"`` for 529 accounts, ``"Authorized"`` for trust
    accounts); the nickname is the user-visible link text inside
    each ``apex-kit-web-link`` element. Mirrors UBS PSN's
    relationship + nickname dimensions.

    Walks ``<section aria-label="<portfolio>">`` containers whose
    descendants carry the ``ap143528-accounts-selector-account-
    link-<id>`` testid pattern. Accounts that fall outside any
    such section (rare) get ``portfolio = null`` and end up under
    a synthetic ``"(unknown)"`` bucket in run.json."""
    # The link's textContent runs nickname / account-number /
    # balance / gains-losses on one line. We only want the
    # nickname prefix — split on the account-id digit sequence
    # (which we already have from the testid) and trim. Falls
    # back to the first newline-delimited line if the split
    # misses.
    js = """
    () => {
      const out = [];
      const seen = new Set();
      const extract_nickname = (raw, aid) => {
        const parts = raw.split(aid);
        let cand = parts.length > 1 ? parts[0] : raw.split(/\\n/)[0];
        cand = (cand || '').trim();
        // Strip trailing punctuation / connectors.
        cand = cand.replace(/[\\s,:;\\-]+$/, '').trim();
        return cand.slice(0, 80);
      };
      // Primary path: each portfolio is a <section aria-label="X">
      // containing apex-kit-web-link account links.
      for (const sec of document.querySelectorAll('section[aria-label]')) {
        const portfolio = sec.getAttribute('aria-label');
        const links = sec.querySelectorAll(
          '[data-testid^="ap143528-accounts-selector-account-link-"]'
        );
        for (const link of links) {
          const tid = link.getAttribute('data-testid') || '';
          const m = tid.match(
            /^ap143528-accounts-selector-account-link-(\\d+)$/
          );
          if (!m) continue;
          const aid = m[1];
          if (seen.has(aid)) continue;
          seen.add(aid);
          out.push({
            account_id: aid,
            portfolio: portfolio,
            nickname: extract_nickname(link.textContent || '', aid),
          });
        }
      }
      // Sweep up any account-link not yet captured (no parent
      // section) so the inventory stays complete even if Fidelity
      // re-shuffles the selector layout.
      for (const link of document.querySelectorAll(
        '[data-testid^="ap143528-accounts-selector-account-link-"]'
      )) {
        const tid = link.getAttribute('data-testid') || '';
        const m = tid.match(
          /^ap143528-accounts-selector-account-link-(\\d+)$/
        );
        if (!m) continue;
        const aid = m[1];
        if (seen.has(aid)) continue;
        seen.add(aid);
        out.push({
          account_id: aid,
          portfolio: null,
          nickname: extract_nickname(link.textContent || '', aid),
        });
      }
      return out;
    }
    """
    try:
        return page.evaluate(js) or []
    except Exception as e:
        log.warning("account-dimension enumeration failed: %s", e)
        return []


def make_activity_windows(since_date, until_date):
    """Bisect ``[since_date, until_date]`` into
    ``MAX_ACTIVITY_WINDOW_DAYS``-day windows (oldest first) so each
    fits under Fidelity's per-export cap on the Custom range tab.
    Returns a list of ``(start, end)`` date tuples."""
    if until_date < since_date:
        return []
    windows = []
    cursor = since_date
    while cursor <= until_date:
        end = min(
            cursor + timedelta(days=MAX_ACTIVITY_WINDOW_DAYS - 1),
            until_date,
        )
        windows.append((cursor, end))
        cursor = end + timedelta(days=1)
    return windows


def parse_iso_date(value):
    """Parse YYYY-MM-DD; return ``date``. ``None`` if value is
    empty / unset."""
    if not value:
        return None
    return datetime.strptime(value, "%Y-%m-%d").date()


def capture(page, capture_dir, label):
    """Save HTML + PNG of the current page state. Never raises."""
    if capture_dir is None:
        return
    try:
        capture_dir.mkdir(parents=True, exist_ok=True)
    except Exception as e:
        log.warning("mkdir %s: %s", capture_dir, e)
        return
    ts = ts_slug()
    try:
        try:
            html = page.content()
        except Exception:
            html = page.evaluate(
                "() => document.documentElement.outerHTML"
            )
        (capture_dir / f"{ts}-{label}.html").write_text(
            html, encoding="utf-8",
        )
    except Exception as e:
        log.warning("html capture %s: %s", label, e)
    try:
        page.screenshot(
            path=str(capture_dir / f"{ts}-{label}.png"),
            full_page=False, timeout=5_000, animations="disabled",
        )
    except Exception as e:
        log.debug("png capture %s: %s", label, e)


def goto_and_wait(page, url, wait_selector=None, wait_timeout_s=30):
    """Navigate, then wait for an optional selector (proxy for 'SPA
    hydrated'). Returns the live URL after settle."""
    log.info("navigating to %s", url)
    page.goto(url, wait_until="domcontentloaded")
    if wait_selector:
        deadline = time.monotonic() + wait_timeout_s
        while time.monotonic() < deadline:
            if page.locator(wait_selector).count() > 0:
                break
            time.sleep(0.5)
    time.sleep(1.0)
    return live_url(page)


def safe_save_download(dl, out_dir, fallback_name):
    """Save a Playwright Download to a non-clobbering path under
    ``out_dir``. Returns the file path written."""
    suggested = (
        dl.suggested_filename or fallback_name
    ).replace("/", "_").replace("\\", "_")
    out_path = out_dir / suggested
    base, _, ext = suggested.rpartition(".")
    k = 1
    while out_path.exists():
        out_path = out_dir / (
            f"{base}__{k}.{ext}" if ext else f"{suggested}__{k}"
        )
        k += 1
    dl.save_as(str(out_path))
    return out_path


# ---------------------------------------------------------------------------
# Positions — one CSV per view (Overview, DividendView)
# ---------------------------------------------------------------------------

def _select_preset_view(page, value, capture_dir):
    """Switch positions to a preset view via the native ``<select>``.
    ``value`` is the Fidelity-internal preset name (``Overview``,
    ``DividendView``, ``FundPerfView``, ``ClosedPositionsView``,
    ``MyView``)."""
    select = page.locator(SEL_PRESET_VIEW_SELECT)
    if select.count() == 0:
        raise RuntimeError(
            f"preset-views <select> not found via "
            f"{SEL_PRESET_VIEW_SELECT!r}"
        )
    select.first.select_option(value=value)
    time.sleep(TABLE_RENDER_WAIT_S)
    capture(page, capture_dir, f"positions-preset-{value}")


def _click_all_accounts(page, capture_dir):
    """Ensure the positions table aggregates across all accounts.
    Best-effort: if the toggle isn't present (already in
    all-accounts mode), continue silently."""
    loc = page.locator(SEL_ALL_ACCOUNTS)
    if loc.count() == 0:
        log.info(
            "all-accounts toggle absent; assuming all-accounts active"
        )
        return
    try:
        if loc.first.is_visible(timeout=1_000):
            loc.first.click(timeout=5_000)
            time.sleep(TABLE_RENDER_WAIT_S)
            log.info("clicked all-accounts toggle")
            capture(page, capture_dir, "positions-all-accounts")
    except Exception as e:
        log.debug("all-accounts click skipped: %s", e)


def _open_positions_kebab(page, capture_dir, label_suffix):
    """Open the kebab menu on the positions table."""
    capture(page, capture_dir, f"kebab-before-{label_suffix}")
    kebab = page.locator(SEL_KEBAB_MENU)
    if kebab.count() == 0:
        raise RuntimeError(
            f"kebab-menu selector {SEL_KEBAB_MENU} not found"
        )
    kebab.first.click(timeout=10_000)
    time.sleep(KEBAB_OPEN_WAIT_S)
    capture(page, capture_dir, f"kebab-open-{label_suffix}")


def _click_positions_download(page, capture_dir, label_suffix):
    """In an open positions kebab, click the Download menuitem and
    intercept the download via ``page.expect_download``."""
    candidates = (
        "role=menuitem[name=/download/i]",
        "[role=menuitem]:has-text('Download')",
        "[role=menuitemradio]:has-text('Download')",
        "button:has-text('Download')",
        "a:has-text('Download')",
        "[role=menuitem]:has-text('Export')",
    )
    for sel in candidates:
        try:
            loc = page.locator(sel).first
            if loc.count() == 0:
                continue
            if not loc.is_visible(timeout=500):
                continue
            log.info(
                "positions Download menuitem via %r (%s)",
                sel, label_suffix,
            )
            with page.expect_download(
                timeout=DOWNLOAD_TIMEOUT_MS,
            ) as dl_info:
                loc.click(timeout=5_000)
            return dl_info.value
        except Exception as e:
            log.debug(
                "positions Download candidate %r: %s", sel, e,
            )
    capture(
        page, capture_dir, f"positions-download-no-match-{label_suffix}",
    )
    raise RuntimeError(
        "no Download menuitem in positions kebab"
    )


def scrape_positions(page, bronze_dir, capture_dir):
    """Export the Overview + DividendView positions CSVs."""
    results = []
    positions_dir = bronze_dir / "positions"
    positions_dir.mkdir(parents=True, exist_ok=True)

    goto_and_wait(page, URL_POSITIONS, wait_selector=SEL_KEBAB_MENU)
    _click_all_accounts(page, capture_dir)

    for view_key, view_value in (
        ("summary",  "Overview"),
        ("dividend", "DividendView"),
    ):
        try:
            _select_preset_view(page, view_value, capture_dir)
            _open_positions_kebab(page, capture_dir, view_key)
            dl = _click_positions_download(page, capture_dir, view_key)
            csv_path = positions_dir / f"positions_{view_key}.csv"
            dl.save_as(str(csv_path))
            log.info(
                "saved positions/positions_%s.csv (%d bytes)",
                view_key, csv_path.stat().st_size,
            )
            results.append({
                "view": view_key,
                "file": str(csv_path.relative_to(bronze_dir)),
                "ok": True,
            })
        except Exception as e:
            log.exception("positions view %s failed", view_key)
            results.append({
                "view": view_key, "ok": False, "error": str(e),
            })

    return results


# ---------------------------------------------------------------------------
# Activity & Orders — one consolidated CSV per date-window
# ---------------------------------------------------------------------------

def _select_activity_custom_range(page, since_date, until_date,
                                     capture_dir, label_suffix):
    """Drive the page-level time-period filter pill to its
    'Custom' segmented-control tab, fill the since/until inputs
    with the given dates, click 'Apply Customized Time Period',
    wait for the table re-fetch.

    Returns the range tag (``"<since>__<until>"``) on success or
    ``None`` on failure. All clicks dispatch via JS to bypass
    Camoufox's actionability quirks (the picker contents are
    marked 'outside viewport' even when on-screen).

    NOTE: Fidelity's Custom range is capped at 93 days per
    request; callers MUST chunk longer requested windows via
    ``make_activity_windows`` before invoking this function.
    """
    pill = page.locator("[data-testid='ap143528-timeperiod-filter']")
    if pill.count() == 0:
        log.debug("page-level timepicker pill not found")
        return None
    try:
        pill.first.evaluate(
            "el => { (el.querySelector('button') || el).click(); }"
        )
    except Exception as e:
        log.debug("timepicker pill open click: %s", e)
        return None
    time.sleep(1.0)

    # Switch to the Custom segmented-control tab.
    custom_segment = page.locator(
        "apex-kit-segment[pvd-value='Custom']"
    )
    if custom_segment.count() == 0:
        log.warning("Custom tab not found in timepicker; aborting")
        return None
    try:
        custom_segment.first.evaluate(
            "el => { "
            "  const inp = el.querySelector('input[type=radio]'); "
            "  if (inp) inp.click(); else el.click(); "
            "}"
        )
    except Exception as e:
        log.warning("Custom tab click failed: %s", e)
        return None
    time.sleep(1.0)
    capture(
        page, capture_dir,
        f"activity-custom-tab-open-{label_suffix}",
    )

    # Fill since + until dates. Inputs are HTML5 ``<input type=
    # "date">`` and want ISO YYYY-MM-DD; Fidelity gates them with
    # min/max attributes that bound the per-export retention
    # window (currently ~4 years back). We clamp to those bounds
    # so an over-eager since= doesn't silently truncate.
    def _fill_date(input_locator, dt):
        # Read the input's min/max attributes and clamp; the SPA
        # rejects out-of-bound values silently which produces a
        # CSV at the page default range.
        try:
            bounds = input_locator.evaluate(
                "el => ({min: el.min || null, max: el.max || null})"
            )
        except Exception:
            bounds = {"min": None, "max": None}
        clamped = dt
        try:
            if bounds.get("min"):
                lo = datetime.strptime(bounds["min"], "%Y-%m-%d").date()
                if clamped < lo:
                    log.info(
                        "clamping date %s to Fidelity min %s",
                        clamped.isoformat(), lo.isoformat(),
                    )
                    clamped = lo
            if bounds.get("max"):
                hi = datetime.strptime(bounds["max"], "%Y-%m-%d").date()
                if clamped > hi:
                    log.info(
                        "clamping date %s to Fidelity max %s",
                        clamped.isoformat(), hi.isoformat(),
                    )
                    clamped = hi
        except Exception as e:
            log.debug("bounds-clamp failed: %s", e)
        formatted = clamped.strftime("%Y-%m-%d")
        try:
            input_locator.evaluate(
                "(el, val) => { "
                "  const setter = Object.getOwnPropertyDescriptor("
                "    window.HTMLInputElement.prototype, 'value').set; "
                "  setter.call(el, val); "
                "  el.dispatchEvent(new Event('input', {bubbles:true})); "
                "  el.dispatchEvent(new Event('change', {bubbles:true})); "
                "  el.dispatchEvent(new Event('blur', {bubbles:true})); "
                "}",
                formatted,
            )
            return clamped
        except Exception as e:
            log.debug("date-fill failed: %s", e)
            return None

    from_input = page.locator("#customized-timeperiod-from-date").first
    to_input = page.locator("#customized-timeperiod-to-date").first
    if from_input.count() == 0 or to_input.count() == 0:
        capture(
            page, capture_dir,
            f"activity-custom-inputs-missing-{label_suffix}",
        )
        log.warning(
            "Custom-tab date inputs not found "
            "(#customized-timeperiod-from-date / -to-date)"
        )
        return None
    filled_since = _fill_date(from_input, since_date)
    filled_until = _fill_date(to_input, until_date)
    if not filled_since or not filled_until:
        capture(
            page, capture_dir,
            f"activity-custom-fill-failed-{label_suffix}",
        )
        log.warning(
            "Custom-range date fill failed (since=%s, until=%s)",
            filled_since, filled_until,
        )
        return None
    since_date, until_date = filled_since, filled_until

    apply_btn = page.locator(
        "button[aria-label='Apply Customized Time Period']"
    )
    if apply_btn.count() == 0:
        log.warning(
            "no 'Apply Customized Time Period' button for Custom range"
        )
        return None
    try:
        apply_btn.first.evaluate("el => el.click()")
        log.debug("clicked 'Apply Customized Time Period'")
    except Exception as e:
        log.warning("apply Custom click failed: %s", e)
        return None

    try:
        page.wait_for_load_state("networkidle", timeout=20_000)
    except Exception:
        time.sleep(8.0)
    capture(
        page, capture_dir,
        f"activity-custom-applied-{label_suffix}",
    )
    range_tag = (
        f"{since_date.strftime('%Y%m%d')}"
        f"__{until_date.strftime('%Y%m%d')}"
    )
    log.info(
        "activity Custom range set: %s..%s",
        since_date.isoformat(), until_date.isoformat(),
    )
    return range_tag


def _select_activity_page_timeperiod(page, preferred_values,
                                       capture_dir, label_suffix):
    """Drive the **page-level** time-period filter pill on the
    Activity & Orders page (``[data-testid='ap143528-timeperiod-
    filter']``) and select the radio with the largest preferred
    ``pvd-value`` (day-count string, e.g. ``'90'`` for Past 90
    days).

    This must be done BEFORE opening the Download popover —
    although the popover contains its own timepicker control,
    that control shares the page-level ``#timeperiod-select-
    container`` dropdown, and clicking a radio in the dropdown is
    treated as 'outside the Download popover' which dismisses it.
    Setting the time-period on the page first means the Download
    popover (when opened later) picks up the new range with no
    cross-element interaction.

    Returns the value selected (as ``"past_<N>_days"``) or
    ``None`` if the pill is absent or no preferred value matches.
    Both the pill and the radios dispatch via JS — Camoufox marks
    them as 'outside viewport' even when on-screen."""
    pill = page.locator("[data-testid='ap143528-timeperiod-filter']")
    if pill.count() == 0:
        # Fallback: the in-popover button if the page-level pill
        # isn't rendered for some reason.
        pill = page.locator("#timeperiod-select-button")
        if pill.count() == 0:
            log.debug(
                "neither page-level timeperiod pill nor "
                "#timeperiod-select-button found"
            )
            return None
    try:
        pill.first.evaluate(
            "el => { (el.querySelector('button') || el).click(); }"
        )
    except Exception as e:
        log.debug("activity timeperiod pill click: %s", e)
        return None
    time.sleep(1.0)
    capture(
        page, capture_dir,
        f"activity-page-timeperiod-open-{label_suffix}",
    )
    for value in preferred_values:
        sel = f"helios-radio[pvd-value='{value}']"
        try:
            loc = page.locator(sel).first
            if loc.count() == 0:
                continue
            # Inside <helios-radio> sits the actual <input
            # type="radio">. Clicking the input fires the SPA's
            # change handler that re-filters the activity table.
            loc.evaluate(
                "el => { "
                "  const inp = el.querySelector('input[type=radio]'); "
                "  if (inp) inp.click(); else el.click(); "
                "}"
            )
            time.sleep(0.5)
            # The radio selection alone is NOT enough — there's an
            # 'Apply Recent Time Period' submit button at the bottom
            # of the dropdown that has to be clicked to commit the
            # change. Without it the pill stays on its prior title
            # and the export uses the prior range.
            apply_btn = page.locator(
                "button[aria-label='Apply Recent Time Period']"
            )
            if apply_btn.count() == 0:
                # Fallback (e.g. user picked Custom tab elsewhere):
                # any visible Apply button at the bottom of the
                # picker.
                apply_btn = page.locator(
                    "button[aria-label^='Apply']"
                )
            try:
                apply_btn.first.evaluate("el => el.click()")
                log.debug("clicked Apply on time-period picker")
            except Exception as e:
                log.warning(
                    "Apply button click failed: %s; the radio "
                    "selection won't commit", e,
                )
            # Wait for the table re-fetch.
            try:
                page.wait_for_load_state(
                    "networkidle", timeout=15_000,
                )
            except Exception:
                time.sleep(6.0)
            capture(
                page, capture_dir,
                f"activity-timeperiod-applied-{label_suffix}",
            )
            try:
                applied_title = page.locator(
                    "#timeperiod-select-button"
                ).first.get_attribute("title")
                log.info(
                    "activity timeperiod set to value=%s; "
                    "pill title after Apply: %r",
                    value, applied_title,
                )
            except Exception:
                log.info(
                    "activity timeperiod set to value=%s "
                    "(could not verify pill title)", value,
                )
            return f"past_{value}_days"
        except Exception as e:
            log.debug(
                "timeperiod value=%s: %s", value, e,
            )
    log.info(
        "no preferred timeperiod radio matched %r; leaving default",
        preferred_values,
    )
    return None


def _click_activity_download(page, capture_dir, label_suffix):
    """Activity's Download is a two-step popover: the visible button
    (``aria-label='Download'`` with ``aria-controls='downloadContent'``)
    opens an in-page menu that contains the CSV download button.
    Click trigger → click CSV → intercept download.

    The time-period for the export is whatever the **page-level**
    timepicker filter has selected at the time of the click — that
    has to be set BEFORE this function (via
    ``_select_activity_page_timeperiod``); see the helper's
    docstring for why."""
    trigger_sel = "button[aria-label='Download']"
    trigger = page.locator(trigger_sel).first
    if trigger.count() == 0:
        raise RuntimeError(
            f"activity Download trigger not found via {trigger_sel!r}"
        )
    # The Download button is disabled while the SPA re-fetches the
    # activity table after a Custom-range Apply. `networkidle` is
    # too lenient: prior unrelated requests may have already gone
    # idle while the new range fetch hasn't started, leaving the
    # button briefly disabled when we try to click. Poll for the
    # disabled attribute to clear (up to ~15s) before clicking.
    deadline = time.monotonic() + 15.0
    while time.monotonic() < deadline:
        try:
            disabled = trigger.evaluate(
                "el => el.disabled || el.getAttribute('disabled') !== null"
            )
        except Exception:
            disabled = False
        if not disabled:
            break
        time.sleep(0.5)
    trigger.click(timeout=10_000)
    time.sleep(1.0)
    capture(
        page, capture_dir, f"activity-download-open-{label_suffix}",
    )

    for sel in (
        "#downloadContent button:has-text('CSV')",
        "#downloadContent a:has-text('CSV')",
        "#downloadContent [role=menuitem]:has-text('CSV')",
        "a:has-text('Download as CSV')",
    ):
        try:
            loc = page.locator(sel).first
            if loc.count() == 0:
                continue
            if not loc.is_visible(timeout=500):
                continue
            log.info(
                "activity CSV item via %r (%s)",
                sel, label_suffix,
            )
            with page.expect_download(
                timeout=DOWNLOAD_TIMEOUT_MS,
            ) as dl_info:
                loc.click(timeout=5_000)
            return dl_info.value
        except Exception as e:
            log.debug("activity CSV item %r: %s", sel, e)
    capture(
        page, capture_dir, f"activity-csv-no-match-{label_suffix}",
    )
    raise RuntimeError("no CSV item in activity Download popover")


# Preferred day-count radios on the activity timepicker's 'Recent'
# tab (under ``#timeperiod-select-container``), longest first.
# Each option is a ``<helios-radio pvd-value="<N>">`` whose inner
# ``<input type=radio>`` is the actual control.
ACTIVITY_TIMEPERIOD_PREFERENCE = ("90", "60", "30")


def _activity_csv_for_window(page, since_date, until_date,
                              bronze_dir, capture_dir):
    """Drive the Custom-range tab to [since..until] (a single
    ≤93-day window), open the Download popover, save the CSV.
    Returns a single result dict.

    The CSV is **consolidated across all visible accounts** — the
    account-selector state does NOT scope the Activity export.
    The ``Account Number`` column inside the CSV is the per-row
    account discriminator; downstream silver parsing splits on
    that to produce per-account fact rows."""
    activity_dir = bronze_dir / "activity"
    win_suffix = (
        f"{since_date.strftime('%Y%m%d')}"
        f"__{until_date.strftime('%Y%m%d')}"
    )
    range_tag = _select_activity_custom_range(
        page, since_date, until_date,
        capture_dir, win_suffix,
    )
    if range_tag is None:
        return {
            "window": [since_date.isoformat(),
                       until_date.isoformat()],
            "ok": False,
            "error": "custom-range not applied",
        }
    try:
        dl = _click_activity_download(
            page, capture_dir, win_suffix,
        )
        csv_path = activity_dir / f"activity_{range_tag}.csv"
        dl.save_as(str(csv_path))
        log.info(
            "saved activity/%s (%d bytes, range=%s)",
            csv_path.name, csv_path.stat().st_size, range_tag,
        )
        return {
            "window": [since_date.isoformat(),
                       until_date.isoformat()],
            "file": str(csv_path.relative_to(bronze_dir)),
            "ok": True,
        }
    except Exception as e:
        log.exception(
            "activity Custom-range download failed for %s",
            win_suffix,
        )
        return {
            "window": [since_date.isoformat(),
                       until_date.isoformat()],
            "ok": False, "error": str(e),
        }


def scrape_activity(page, since_date, until_date,
                      bronze_dir, capture_dir):
    """Consolidated activity CSV(s). Fidelity's Activity & Orders
    export is **all-accounts** regardless of the account-selector
    state — every visible account's rows show up in a single CSV
    discriminated by the ``Account Number`` column. So we drive
    the page once per date-window, not per-account.

    * If ``since_date`` and ``until_date`` are both supplied:
      bisect the range into ≤``MAX_ACTIVITY_WINDOW_DAYS``-day
      windows via ``make_activity_windows`` and drive the Custom
      timepicker tab for each — one CSV per window.

    * Otherwise: use the longest preset that
      ``ACTIVITY_TIMEPERIOD_PREFERENCE`` matches (typically 'Past
      90 days') for a single CSV at Fidelity's rolling default.

    The preset path is the fast option for routine recurring
    dumps; the Custom-range path is the historic-backfill option
    a user runs once over an explicit ``--since <YYYY-MM-DD>
    --until <YYYY-MM-DD>`` window. Bounded by Fidelity's
    documented ~4-year retention on Activity exports (the Custom
    tab's date-input ``min`` attribute is the authoritative
    boundary; ``_select_activity_custom_range`` clamps to it)."""
    activity_dir = bronze_dir / "activity"
    activity_dir.mkdir(parents=True, exist_ok=True)
    results = []

    try:
        goto_and_wait(page, URL_ACTIVITY, wait_selector=SEL_KEBAB_MENU)
        capture(page, capture_dir, "activity-landed")
    except Exception as e:
        log.exception("activity-nav failed")
        return [{"ok": False, "error": f"navigate: {e}"}]

    # Custom-range path — historic backfill, one CSV per ≤93d window.
    if since_date is not None and until_date is not None:
        windows = make_activity_windows(since_date, until_date)
        log.info(
            "activity backfill: windows=%d (since=%s until=%s)",
            len(windows),
            since_date.isoformat(), until_date.isoformat(),
        )
        for (w_start, w_end) in windows:
            results.append(_activity_csv_for_window(
                page, w_start, w_end, bronze_dir, capture_dir,
            ))
        return results

    # Preset path — single rolling CSV.
    selected_range = _select_activity_page_timeperiod(
        page, ACTIVITY_TIMEPERIOD_PREFERENCE,
        capture_dir, "default",
    )
    try:
        dl = _click_activity_download(
            page, capture_dir, "default",
        )
        suffix = (selected_range or "default").lower().replace(" ", "_")
        csv_path = activity_dir / f"activity_{suffix}.csv"
        dl.save_as(str(csv_path))
        log.info(
            "saved activity/%s (%d bytes, range=%r)",
            csv_path.name, csv_path.stat().st_size, selected_range,
        )
        results.append({
            "timeperiod": selected_range or "default",
            "file": str(csv_path.relative_to(bronze_dir)),
            "ok": True,
        })
    except Exception as e:
        log.exception("activity download failed")
        results.append({"ok": False, "error": str(e)})
    return results


# ---------------------------------------------------------------------------
# Documents — Statements + Tax forms
# ---------------------------------------------------------------------------

def _click_sidebar_link(page, text, capture_dir, label):
    """Click a left-rail sidebar link by visible text. Returns True
    on success, False on no-match."""
    for sel in (
        f"a.sidebar-link:has-text('{text}')",
        f"a.sidebar-link:has-text(' {text}')",
        f"a:has-text('{text}')",
        f"[role=link]:has-text('{text}')",
    ):
        try:
            loc = page.locator(sel).first
            if loc.count() == 0:
                continue
            if not loc.is_visible(timeout=500):
                continue
            loc.click(timeout=5_000)
            time.sleep(2.5)
            log.info("clicked sidebar %r via %r", text, sel)
            capture(page, capture_dir, f"sidebar-{label}")
            return True
        except Exception as e:
            log.debug("sidebar link %r %r: %s", text, sel, e)
    return False


def _enumerate_statement_row_labels(page):
    """Return the aria-label of each statement-row description cell
    (e.g. ``"Jan-March 2026 — Statement (pdf)"``) in document order
    so we can dispatch downloads by index."""
    js = """
    () => {
      const out = [];
      const cells = document.querySelectorAll(
        'td.gridData.link[aria-label]'
      );
      for (const c of cells) {
        const label = c.getAttribute('aria-label') || '';
        if (!/\\(pdf\\)$/i.test(label)) continue;
        out.push(label);
      }
      return out;
    }
    """
    try:
        return page.evaluate(js) or []
    except Exception as e:
        log.warning("statement row enumeration failed: %s", e)
        return []


def _click_and_collect(page, context, click_locator,
                        wait_seconds=20):
    """Click ``click_locator``, then wait up to ``wait_seconds`` for
    either a Playwright ``download`` event (file download) or a
    new tab to appear in ``context.pages`` (popup). Returns
    ``("download", <Download>)``, ``("popup", <Page>)``, or
    ``(None, None)`` on timeout.

    Why not ``page.expect_event('popup')``: Camoufox's juggler patch
    doesn't reliably propagate ``popup`` events to ad-hoc
    listeners — analogous to the ``frameNavigated`` bug ``live_url``
    works around. But the new page DOES land in ``context.pages``,
    so polling that list is reliable.

    The ``download`` event side IS reliable; we use the canonical
    ``page.expect_download`` with a short inner timeout, then fall
    back to the polling loop for the popup case.
    """
    pages_before = set(context.pages)
    try:
        with page.expect_download(timeout=3_000) as dl_info:
            click_locator.click(timeout=5_000)
        return ("download", dl_info.value)
    except Exception:
        # Either the download didn't fire within 3s (typical when
        # Fidelity rendered the PDF in a popup tab instead), or
        # the click failed. Move to the popup-polling path; if the
        # click also failed, the wait will simply time out.
        pass

    deadline = time.monotonic() + wait_seconds
    while time.monotonic() < deadline:
        new_pages = [p for p in context.pages if p not in pages_before]
        if new_pages:
            return ("popup", new_pages[-1])
        time.sleep(0.3)
    return (None, None)


def _fetch_popup_pdf(context, popup_page, out_dir,
                      fallback_name, timeout_s=20):
    """A popup that landed on a Fidelity PDF URL hasn't given us a
    Playwright Download — Firefox opened it in the browser's PDF
    viewer. Grab the URL via ``location.href`` (live_url avoids the
    Camoufox URL-cache bug) and re-fetch it through the context's
    request API, which inherits the session cookies. Save the
    response bytes to ``out_dir``. Returns the saved path."""
    # Give the popup a moment to settle on its final URL.
    deadline = time.monotonic() + timeout_s
    pdf_url = None
    while time.monotonic() < deadline:
        try:
            url = live_url(popup_page)
            if url and url not in ("about:blank", ""):
                pdf_url = url
                break
        except Exception:
            pass
        time.sleep(0.3)
    if not pdf_url:
        raise RuntimeError(
            "popup page never settled on a URL within "
            f"{timeout_s}s"
        )
    log.info("fetching popup PDF via context.request: %s",
             pdf_url[:100])
    resp = context.request.get(pdf_url)
    if not resp.ok:
        raise RuntimeError(
            f"context.request.get returned status {resp.status} "
            f"for {pdf_url[:100]}"
        )
    body = resp.body()
    # Pick a filename: try Content-Disposition first, fall back to
    # URL path component, fall back to the supplied default.
    cd = resp.headers.get("content-disposition", "")
    cd_match = re.search(r'filename="?([^"]+)"?', cd or "")
    if cd_match:
        name = cd_match.group(1)
    else:
        path_tail = pdf_url.rstrip("/").rsplit("/", 1)[-1] or fallback_name
        # Strip query string + url-escapes; basic sanity.
        name = path_tail.split("?", 1)[0] or fallback_name
        if not name.lower().endswith(".pdf"):
            name = fallback_name
    name = name.replace("/", "_").replace("\\", "_")
    out_path = out_dir / name
    base, _, ext = name.rpartition(".")
    k = 1
    while out_path.exists():
        out_path = out_dir / (
            f"{base}__{k}.{ext}" if ext else f"{name}__{k}"
        )
        k += 1
    out_path.write_bytes(body)
    return out_path


def _download_statement_format(page, context, row_index,
                                 format_label, docs_dir,
                                 capture_dir):
    """Open the row's downloadIconButton popover, click the
    'Download as <format_label>' menuitem, save the resulting
    artefact. Handles both the CSV (download event) and PDF
    (popup tab → fetch via context.request) paths.

    Returns the saved Path. Raises on failure."""
    # Dismiss any stale popover state from a prior iteration.
    # Two Escape presses cover the case where a popover-from-the-
    # previous-row is still visible plus any browser-PDF-viewer
    # popup state that ate the first press.
    try:
        page.keyboard.press("Escape")
        time.sleep(0.2)
        page.keyboard.press("Escape")
        time.sleep(0.2)
    except Exception:
        pass

    btn_sel = "button[aria-label='download statement']"
    btn = page.locator(btn_sel).nth(row_index)
    # Lower-indexed rows are near the top of the viewport and
    # click fine; later rows (Oct-Dec 2025 / row 2 in the current
    # rendering) sit further down. Playwright's actionability
    # check fails intermittently on those because the row is
    # technically visible but partially obscured by sticky-headers
    # or below the fold. Scroll into view explicitly, then fall
    # back to a JS-dispatched click if the actionability check
    # still rejects.
    try:
        btn.scroll_into_view_if_needed(timeout=5_000)
    except Exception as e:
        log.debug("scroll_into_view row %d: %s", row_index, e)
    try:
        btn.click(timeout=5_000)
    except Exception as e:
        log.warning(
            "row %d native click failed (%s); falling back to "
            "JS-dispatched click", row_index, str(e)[:80],
        )
        btn.evaluate("el => el.click()")
    time.sleep(0.7)
    capture(page, capture_dir,
            f"statement-popover-row{row_index}-{format_label}")

    # The popover is scoped to the row's downloadDropdownContainer;
    # if Fidelity reshuffles the DOM to render it elsewhere, fall
    # back to a global search by literal text.
    for item_sel in (
        f".downloadDropdownContainer li.modal-options"
        f":has-text('Download as {format_label}')",
        f"li.modal-options:has-text('Download as {format_label}')",
    ):
        item = page.locator(item_sel).first
        if item.count() > 0:
            break
    else:
        raise RuntimeError(
            f"no 'Download as {format_label}' menuitem in popover"
        )

    kind, value = _click_and_collect(
        page, context, item, wait_seconds=20,
    )
    if kind == "download":
        ext = format_label.lower()
        return safe_save_download(
            value, docs_dir, f"statement_{row_index}.{ext}",
        )
    if kind == "popup":
        try:
            return _fetch_popup_pdf(
                context, value, docs_dir,
                f"statement_{row_index}.pdf",
            )
        finally:
            try:
                value.close()
            except Exception:
                pass
    raise RuntimeError(
        f"neither download nor popup fired within 20s after "
        f"clicking 'Download as {format_label}'"
    )


def scrape_statements(page, context, docs_dir, capture_dir):
    """Walk the Statements sub-page; for each ``(pdf)`` row open
    the per-row download popover and grab both formats Fidelity
    offers ('Download as PDF' / 'Download as CSV'). The CSV path
    comes down as a regular file download; the PDF path opens a
    popup tab that the browser would normally render in its PDF
    viewer — we fetch the popup's URL via context.request so the
    bytes land on disk regardless of viewer behaviour.

    Not every row offers both formats — annual investment reports
    typically only offer PDF. The CSV variant for those rows just
    fails the menuitem lookup and is recorded as not-available."""
    results = []
    labels = _enumerate_statement_row_labels(page)
    log.info("statements: %d (pdf) rows visible", len(labels))
    for i, label in enumerate(labels):
        # Brief inter-row settle so the SPA's popover state from the
        # prior iteration's failed click / closed popup doesn't bleed
        # into this row's icon click.
        if i > 0:
            time.sleep(1.5)
        for format_label in ("PDF", "CSV"):
            try:
                log.info(
                    "statements: row %d %s — %r",
                    i, format_label, label[:60],
                )
                out_path = _download_statement_format(
                    page, context, i, format_label,
                    docs_dir, capture_dir,
                )
                log.info(
                    "statements: saved %s (%d bytes)",
                    out_path.name, out_path.stat().st_size,
                )
                results.append({
                    "row_index": i,
                    "row_label": label,
                    "format": format_label,
                    "file": out_path.name,
                    "ok": True,
                })
            except Exception as e:
                log.warning(
                    "statement row %d %s (%r) failed: %s",
                    i, format_label, label[:60], e,
                )
                capture(
                    page, capture_dir,
                    f"statement-row{i}-{format_label}-failed",
                )
                results.append({
                    "row_index": i,
                    "row_label": label,
                    "format": format_label,
                    "ok": False, "error": str(e),
                })
    return results


def _enumerate_tax_form_downloads(page):
    """Use JS evaluate to enumerate direct download anchors on the
    Tax forms sub-page. Each form has up to three associated
    anchors in the DOM: an icon link with the form-name aria-label
    ending in ``(pdf)``, a redundant 'click to download form'
    text link, and an external-host link to the matching IRS
    instructions PDF. They all carry ``href='javascript:void(0)'``
    for the in-app downloads (so href-dedup collapses unrelated
    forms across accounts) and the form-name aria-label is shared
    across accounts for the same form type (so aria-label dedup
    also collapses distinct forms).

    We enumerate ONLY the (pdf)/(PDF) aria-label anchors — one
    per form per account — and use their unique ``id`` as the
    click selector. The 'click to download form' link is
    functionally redundant; the IRS-instructions anchors are
    out of scope per DESIGN §8.5."""
    js = """
    () => {
      const out = [];
      const selectors = [
        'a[aria-label$=" (pdf)"]',
        'a[aria-label$=" (PDF)"]',
      ];
      const seen_ids = new Set();
      for (const sel of selectors) {
        for (const el of document.querySelectorAll(sel)) {
          const id = el.getAttribute('id') || '';
          // De-dup by id: same element matched by overlapping
          // selectors (case differences) should only count once.
          if (id && seen_ids.has(id)) continue;
          if (id) seen_ids.add(id);
          out.push({
            aria_label: el.getAttribute('aria-label') || '',
            href: el.getAttribute('href') || '',
            link_id: id,
            text: (el.textContent || '').trim().slice(0, 80),
          });
        }
      }
      return out;
    }
    """
    try:
        return page.evaluate(js) or []
    except Exception as e:
        log.warning("tax-form enumeration failed: %s", e)
        return []


SEL_TAX_YEAR_SELECT = "#options-select-TimeFilter"


def _enumerate_tax_form_years(page):
    """Read the ``<option>`` values from the Tax-forms TimeFilter
    select. Returns a list of year strings (e.g.
    ``["2025","2024",...,"2019"]``) in DOM order, which is
    Fidelity's most-recent-first."""
    js = (
        "() => Array.from(document.querySelectorAll("
        " '#options-select-TimeFilter option'))"
        " .map(o => o.value)"
        " .filter(v => /^[0-9]{4}$/.test(v))"
    )
    try:
        return page.evaluate(js) or []
    except Exception as e:
        log.warning("tax-year enumeration failed: %s", e)
        return []


def _select_tax_year(page, year, capture_dir):
    """Switch the Tax-forms page to the given year via the native
    ``<select>``. Year-switch fires an AJAX fetch that re-renders
    the form list; we poll for the form-list anchors to repopulate
    (or for the in-page spinner to clear) before declaring the
    switch complete. networkidle alone is not enough — the SPA
    keeps stale anchors from the prior year visible during the
    fetch, and enumeration would pick them up under the wrong
    year key.

    Returns ``True`` on success (anchors visible OR confirmed-empty
    after spinner clears); ``False`` on selector failure."""
    sel = page.locator(SEL_TAX_YEAR_SELECT)
    if sel.count() == 0:
        log.warning("tax-year select %r not present", SEL_TAX_YEAR_SELECT)
        return False
    try:
        sel.first.select_option(value=year)
    except Exception as e:
        log.warning("select_option(year=%s) failed: %s", year, e)
        return False
    # Poll up to 30s for the page to stabilise: spinner gone AND
    # (form-list anchors present OR an empty-state message
    # rendered). The empty-state branch handles years where the
    # user truly has no forms (e.g. accounts that didn't exist).
    deadline = time.monotonic() + 30.0
    last_state = "init"
    while time.monotonic() < deadline:
        try:
            state = page.evaluate(
                "() => {"
                "  const spin = document.querySelector("
                "    'pvd-spinner, .pvd-spinner, [class*=spinner]'"
                "  );"
                "  const visible_spin = !!(spin && "
                "    spin.getBoundingClientRect().height > 0);"
                "  const anchors = document.querySelectorAll("
                "    'a[aria-label$=\" (pdf)\"], "
                "     a[aria-label$=\" (PDF)\"], "
                "     a[aria-label=\"click to download form\"]'"
                "  );"
                "  const empty_text = "
                "    document.body.innerText"
                "    .match(/[Nn]o (tax )?forms? (are )?available/);"
                "  return { spin: visible_spin, "
                "           anchors: anchors.length, "
                "           empty: !!empty_text };"
                "}"
            )
        except Exception:
            state = {"spin": True, "anchors": 0, "empty": False}
        last_state = state
        if not state.get("spin") and (
            state.get("anchors", 0) > 0 or state.get("empty")
        ):
            break
        time.sleep(0.5)
    log.debug("tax-year %s settle state: %s", year, last_state)
    capture(page, capture_dir, f"tax-forms-year-{year}")
    return True


def _scrape_tax_year(page, year, docs_dir, capture_dir):
    """Walk all direct-download anchors for the year that's
    currently active in the TimeFilter select."""
    results = []
    candidates = _enumerate_tax_form_downloads(page)
    log.info(
        "tax-forms: year=%s enumerated %d candidates",
        year, len(candidates),
    )
    for i, cand in enumerate(candidates):
        label = cand.get("aria_label", "") or cand.get("text", "")
        link_id = cand.get("link_id", "")
        # Click by the anchor's unique generated id. The
        # form-name aria-label is shared across accounts for the
        # same form type, and every in-app form anchor carries
        # ``href='javascript:void(0)'``, so neither label nor
        # href is unique enough to disambiguate.
        if not link_id:
            results.append({
                "year": year, "index": i, "label": label,
                "ok": False, "error": "no-link-id",
            })
            continue
        anchor_sel = f'a[id={link_id!r}]'
        try:
            anchor = page.locator(anchor_sel).first
            if anchor.count() == 0:
                results.append({
                    "year": year, "index": i, "label": label,
                    "ok": False, "error": "selector-no-match",
                })
                continue
            # Many form anchors are below the fold on the year
            # page; scroll into view to avoid the same
            # actionability flake the statements list had.
            try:
                anchor.scroll_into_view_if_needed(timeout=3_000)
            except Exception:
                pass
            with page.expect_download(
                timeout=DOWNLOAD_TIMEOUT_MS,
            ) as dl_info:
                anchor.click(timeout=10_000)
            out_path = safe_save_download(
                dl_info.value, docs_dir,
                f"taxform_{year}_{i}.bin",
            )
            log.info(
                "tax-forms %s: saved %s (%d bytes)",
                year, out_path.name, out_path.stat().st_size,
            )
            results.append({
                "year": year, "index": i, "label": label,
                "file": out_path.name, "ok": True,
            })
        except Exception as e:
            log.warning(
                "tax-form %s #%d (%r) failed: %s",
                year, i, label[:60], e,
            )
            results.append({
                "year": year, "index": i, "label": label,
                "ok": False, "error": str(e),
            })
    return results


def scrape_tax_forms(page, docs_dir, capture_dir):
    """Walk the Tax-forms sub-page across every selectable year in
    Fidelity's TimeFilter dropdown (typically 7 years going back
    to 2019). Each year's form list is enumerated independently;
    anchors are plain ``<a>`` links so we collect via
    ``page.expect_download`` (no popover indirection).
    Year-empty selections (no forms generated for that year, e.g.
    accounts that didn't yet exist) are logged and skipped."""
    years = _enumerate_tax_form_years(page)
    if not years:
        log.warning(
            "no tax-year options enumerated; falling back to "
            "current selection"
        )
        return _scrape_tax_year(page, "current", docs_dir, capture_dir)
    log.info("tax-forms: iterating %d years: %s", len(years), years)
    results = []
    for year in years:
        if not _select_tax_year(page, year, capture_dir):
            results.append({
                "year": year, "ok": False,
                "error": "year-select-failed",
            })
            continue
        results.extend(_scrape_tax_year(page, year, docs_dir, capture_dir))
    return results


def scrape_documents(page, context, bronze_dir, capture_dir):
    """Walk Statements + Tax forms in the document center. Each
    sub-page is exercised independently so a failure in one
    category doesn't block the other. ``context`` is needed for the
    popup-PDF-via-context.request fallback path on statements."""
    docs_dir = bronze_dir / "documents"
    docs_dir.mkdir(parents=True, exist_ok=True)
    results = {"status": "walked"}

    try:
        goto_and_wait(page, URL_DOCUMENTS, wait_timeout_s=30)
        time.sleep(SPA_HYDRATE_WAIT_S)
        capture(page, capture_dir, "documents-landed")
    except Exception as e:
        log.exception("documents nav failed")
        return {"status": "error", "error": str(e)}

    url = live_url(page)
    if not url.startswith(POST_AUTH_PREFIX):
        log.warning(
            "documents nav landed at %s — likely session-timeout "
            "redirect; aborting documents phase", url,
        )
        return {"status": "session-timeout", "landed_url": url}

    try:
        _click_sidebar_link(page, "Statements", capture_dir, "statements")
        time.sleep(2.0)
        results["statements"] = scrape_statements(
            page, context, docs_dir, capture_dir,
        )
    except Exception as e:
        log.exception("statements walk failed")
        results["statements_error"] = str(e)

    try:
        if _click_sidebar_link(
            page, "Tax forms", capture_dir, "tax-forms",
        ):
            time.sleep(3.0)
            capture(page, capture_dir, "tax-forms-landed")
            results["tax_forms"] = scrape_tax_forms(
                page, docs_dir, capture_dir,
            )
        else:
            log.warning(
                "tax-forms sidebar link not found; skipping"
            )
            results["tax_forms_error"] = "sidebar-link-not-found"
    except Exception as e:
        log.exception("tax-forms walk failed")
        results["tax_forms_error"] = str(e)

    return results


# ---------------------------------------------------------------------------
# Balances + Performance — exploratory + structured export
# ---------------------------------------------------------------------------

def scrape_balances(page, bronze_dir, capture_dir):
    """Visit the Balances page and save the rendered DOM. Like
    Performance (§4.7), this surface has no direct structured
    export and no single-click PDF download:

    * No CSV / Excel kebab — the page is purely view.
    * The actions menu ('balwebex-actions-menu' or 'more-action-
      menu' depending on build) offers 'Create Balance Letter',
      'Print', 'Glossary of terms'. 'Create Balance Letter'
      opens a multi-step wizard (select letter type → select
      account(s) → Generate → PDF), with the type selector
      offering 'sample letter balance' / 'sample letter balance
      in excess' shapes that aren't a plain-snapshot equivalent
      of the on-screen balance table.
    * 'Print' opens the browser print dialog (not useful).

    The structured balance data IS recoverable from the rendered
    DOM (per-account totalaccountvalue-label data-testids carry
    the dollar figures, accessible to the silver loader). We
    therefore persist the full balances HTML the same way we
    persist performance HTML, and leave the Balance Letter
    wizard as a follow-up if the user later needs the formal
    PDF artefact."""
    out_dir = bronze_dir / "balances"
    out_dir.mkdir(parents=True, exist_ok=True)
    try:
        goto_and_wait(page, URL_BALANCES, wait_timeout_s=30)
        time.sleep(SPA_HYDRATE_WAIT_S)
    except Exception as e:
        log.exception("balances nav failed")
        return {"status": "error", "error": str(e)}
    capture(page, capture_dir, "balances-landed")
    url = live_url(page)
    if not url.startswith(POST_AUTH_PREFIX):
        log.warning(
            "balances nav landed at %s — likely session-timeout "
            "redirect; aborting", url,
        )
        return {"status": "session-timeout", "landed_url": url}
    # Poll for hydration: wait until at least one per-account
    # totalaccountvalue-label is present (those carry the actual
    # dollar figures the silver loader will scrape).
    deadline = time.monotonic() + 15.0
    while time.monotonic() < deadline:
        if page.locator(
            "[data-testid$='-totalaccountvalue-label']"
        ).count() > 0:
            break
        time.sleep(0.5)
    try:
        html = page.content()
    except Exception:
        html = page.evaluate(
            "() => document.documentElement.outerHTML"
        )
    out_path = out_dir / "balances.html"
    out_path.write_text(html, encoding="utf-8")
    log.info(
        "saved balances/%s (%d bytes; no direct export; "
        "per-account values in totalaccountvalue-label testids)",
        out_path.name, out_path.stat().st_size,
    )
    return {
        "status": "explored-no-export",
        "file": str(out_path.relative_to(bronze_dir)),
    }


def scrape_performance(page, bronze_dir, capture_dir):
    """Visit the Performance page and capture a full-page snapshot
    of the rendered metrics tiles.

    Empirically confirmed (2026-05): the page has NO structured-
    data export — no CSV, no PDF, no kebab/menu Download item.
    The data surfaces as a Highcharts SVG + a column of
    collapsible info tiles (return percentages by period,
    benchmark deltas). The HTML capture under
    ``screenshots/performance-landed.html`` is the bronze
    artefact for this surface; downstream silver work either
    scrapes return percentages from the DOM text or accepts the
    gap (most return metrics are derivable from positions +
    activity time-series anyway).

    Returns a result dict with ``status: 'explored-no-export'``
    so run.json captures the (intentional) gap. Compare to
    ``balances``, which similarly has no CSV but does offer a
    PDF 'Balance Letter' via its actions menu."""
    out_dir = bronze_dir / "performance"
    out_dir.mkdir(parents=True, exist_ok=True)
    try:
        goto_and_wait(page, URL_PERFORMANCE, wait_timeout_s=30)
        time.sleep(SPA_HYDRATE_WAIT_S)
    except Exception as e:
        log.exception("performance nav failed")
        return {"status": "error", "error": str(e)}
    capture(page, capture_dir, "performance-landed")
    url = live_url(page)
    if not url.startswith(POST_AUTH_PREFIX):
        log.warning(
            "performance nav landed at %s — likely session-"
            "timeout redirect; aborting", url,
        )
        return {"status": "session-timeout", "landed_url": url}
    # Persist a copy of the rendered HTML under the bronze dir
    # itself (not just screenshots/), so the silver loader can
    # extract returns text without rummaging through the
    # diagnostics directory.
    try:
        html = page.content()
    except Exception:
        html = page.evaluate(
            "() => document.documentElement.outerHTML"
        )
    out_path = out_dir / "performance.html"
    out_path.write_text(html, encoding="utf-8")
    log.info(
        "saved performance/%s (%d bytes; no structured export "
        "available on this surface)",
        out_path.name, out_path.stat().st_size,
    )
    return {
        "status": "explored-no-export",
        "file": str(out_path.relative_to(bronze_dir)),
    }


# ---------------------------------------------------------------------------
# walk() — entry point invoked by login.py's keep-alive loop
# ---------------------------------------------------------------------------

def walk(context, page, config, args):
    """Process a single download trigger. Dispatches to the per-
    phase scrape functions based on ``config['mode']``. Phases
    each have their own try/except inside their dispatcher; a
    failure in one phase does not block the others."""
    dest_root = Path(config.get("dest") or "/data")
    bronze_dir = dest_root / ts_slug()
    bronze_dir.mkdir(parents=True, exist_ok=True)
    capture_dir = bronze_dir / "screenshots"
    log.info("walk: bronze dir %s", bronze_dir)

    # Ensure we're on a portfolio surface where the account
    # selector renders.
    if not live_url(page).startswith(POST_AUTH_PREFIX):
        goto_and_wait(page, URL_PORTFOLIO_SUMMARY, wait_timeout_s=20)

    excluded = parse_exclude_list(config.get("exclude_accounts"))
    dimensions = enumerate_account_dimensions(page)
    all_accounts = sorted({d["account_id"] for d in dimensions})
    # Auto-exclude non-9-digit accounts (the Fidelity Charitable
    # DAF uses a 7-digit id and is out of scope per DESIGN §1.3 /
    # §3.5).
    auto_excluded = {a for a in all_accounts if len(a) != 9}
    excluded |= auto_excluded
    in_scope = [a for a in all_accounts if a not in excluded]
    log.info(
        "accounts visible=%d auto-excluded=%d explicit-excluded=%d "
        "in_scope=%d",
        len(all_accounts), len(auto_excluded),
        len(parse_exclude_list(config.get("exclude_accounts"))),
        len(in_scope),
    )

    # account_dimensions: serialise under hashed keys so run.json
    # doesn't carry raw 9-digit account ids on disk (parity with
    # the bronze CSV filenames which also use account_key()).
    account_dimensions = {}
    for d in dimensions:
        aid = d["account_id"]
        account_dimensions[account_key(aid)] = {
            "portfolio": d.get("portfolio"),
            "nickname": d.get("nickname"),
            "in_scope": aid in in_scope,
        }

    mode = config.get("mode", "all")
    dry_run = config.get("dry_run", "false").lower() == "true"
    since_date = parse_iso_date(config.get("since"))
    until_date = parse_iso_date(config.get("until"))
    if since_date is not None and until_date is None:
        until_date = datetime.now(timezone.utc).date()

    run_json = {
        "snapshot_at": bronze_dir.name,
        "trigger_config": config,
        "accounts_enumerated": all_accounts,
        "accounts_auto_excluded": sorted(auto_excluded),
        "accounts_explicitly_excluded":
            sorted(parse_exclude_list(config.get("exclude_accounts"))),
        "accounts_in_scope": in_scope,
        "account_dimensions": account_dimensions,
        "activity_window": (
            {"since": since_date.isoformat(),
             "until": until_date.isoformat()}
            if since_date and until_date else None
        ),
        "phases_requested": mode,
    }

    if dry_run:
        log.info("dry_run=true; skipping all artefact downloads")
        run_json["status"] = "dry-run"
    else:
        if mode in ("all", "positions"):
            run_json["positions_results"] = scrape_positions(
                page, bronze_dir, capture_dir,
            )
        if mode in ("all", "activity"):
            run_json["activity_results"] = scrape_activity(
                page, since_date, until_date,
                bronze_dir, capture_dir,
            )
        if mode in ("all", "documents"):
            run_json["documents_results"] = scrape_documents(
                page, context, bronze_dir, capture_dir,
            )
        if mode in ("all", "balances"):
            run_json["balances_results"] = scrape_balances(
                page, bronze_dir, capture_dir,
            )
        if mode in ("all", "performance"):
            run_json["performance_results"] = scrape_performance(
                page, bronze_dir, capture_dir,
            )
        run_json["status"] = "complete"

    run_path = bronze_dir / "run.json"
    run_path.write_text(json.dumps(run_json, indent=2, sort_keys=True))
    log.info("walk: wrote %s", run_path)
