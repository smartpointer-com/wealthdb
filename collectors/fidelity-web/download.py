#!/usr/bin/env python3
"""
Fidelity client-web one-shot fetch: login → walk → logout → exit.

Boots Camoufox (a stealth-patched Firefox fork) against
``digital.fidelity.com``, runs the read-only login + MFA flow,
walks whichever export phases the user selected, attempts a clean
logout, and exits. Bronze artefacts land in a timestamped
``<dest>/<UTC-ts>/`` directory.

Five export phases, gated by ``--mode`` (``all`` runs them in
order, otherwise the named single phase):

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

Anti-bot posture: rung 3 of DESIGN.md §6 — Camoufox with
``os='macos'`` + ``humanize=True`` + ``geoip=True``, behind Xvfb.
The first-ever login from a fresh profile dir needs a one-time
human VNC handoff (``--vnc``) so Akamai sees a real
mousedown/mouseup at credential submit; after that the profile
dir's trust cookies let scripted submits through.

Modes:
  --check      open the profile dir, navigate to the post-auth
               landing URL to verify the session is alive. No
               credential submit, no 2FA, no MFA push.
  --vnc        pre-fill the credentials, then HAND OFF to the
               operator via VNC (x11vnc on 127.0.0.1:5900,
               started by entrypoint.sh). The operator clicks
               Log In and completes 2FA manually. The script
               polls for the post-auth URL and (when no walk
               phases are requested) exits once it lands.
  (default)    full credential + CLI-MFA flow + walk + logout.

Usage:
    download.py --profile-dir /secrets/fidelity-web-profile
                [--env-file /secrets/fidelity-web.env]
                [--dest /data]
                [--mode all|positions|activity|documents|balances|performance]
                [--since YYYY-MM-DD] [--until YYYY-MM-DD]
                [--exclude-accounts <a,b>]
                [--dry-run]
                [--screenshot-dir /debug/<dir>]
                [-v]
"""

from __future__ import annotations

import argparse
import contextlib
import hashlib
import json
import logging
import os
import re
import sys
import time
from datetime import datetime, timedelta, timezone
from pathlib import Path

from collectorkit import bronze, cli


log = logging.getLogger("fidelity-web.download")


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
# Login + session constants
# ---------------------------------------------------------------------------

LANDMARK_TIMEOUT_MS = 60_000
PROFILE_DIR_MODE = 0o700

USERNAME_ENV = "FIDELITY_USERNAME"
PASSWORD_ENV = "FIDELITY_PASSWORD"
# Credentials whose file value overrides anything inherited from the
# host env (defeats the source-mangling-on-$ pitfall — see
# load_env_file docstring).
_CRED_OVERRIDE_VARS = (USERNAME_ENV, PASSWORD_ENV)

DEFAULT_ENV_FILE_CANDIDATES = (
    Path("/secrets/fidelity-web.env"),
    Path.home() / ".secrets" / "fidelity-web.env",
)

SIGNIN_URL = "https://digital.fidelity.com/prgw/digital/signin/"

# Login form selectors. Mirrors the captured DOM; see DESIGN.md §8.2.
# Username field: a <select> on returning devices ("remember my
# username" cookie present), a text <input> on first visit.
SEL_USERNAME_SELECT = "#dom-select-username"
SEL_USERNAME_OTHER_OPTION_VALUE = "default"
SEL_USERNAME_TEXT_INPUT_CANDIDATES = (
    "input#userId-input",
    "input[autocomplete=username]",
    "input[name=userId]",
    "input[aria-labelledby=dom-username-label]",
)
SEL_PASSWORD_INPUT = "#dom-pswd-input"
SEL_LOGIN_BUTTON = "#dom-login-button"

# International Usage Agreement interstitial — Fidelity interposes
# this page for non-US-locale clients (Camoufox geoip=True can trip
# it, depending on the egress IP). User must accept before the
# signin form is served. The accept link runs
# javascript:acceptAgreement() which then routes onward.
SEL_IUA_ACCEPT_CANDIDATES = (
    "a.accept-link",
    "a[title='I Accept']",
    "a:has-text('I Accept')",
)

# 2FA selectors.
SEL_2FA_CODE_INPUT = "#dom-totp-security-code-input"
SEL_2FA_SUBMIT_CANDIDATES = (
    "button[type=submit]:has-text('Submit')",
    "button[type=submit]:has-text('Continue')",
    "button[type=submit]:has-text('Verify')",
    "button[type=submit]",
)

# Trust-this-browser checkbox: the <input> is overlaid by a PVD
# styled <label>::before that intercepts pointer events. Clicking
# the label (not the input) is the only reliable activation path.
SEL_TRUST_BROWSER_CHECKBOX = "#dom-trust-device-checkbox"
SEL_TRUST_BROWSER_LABEL = "label[for='dom-trust-device-checkbox']"

# Logout: best-effort. The user menu surfaces a 'Log Out' anchor;
# we click it before the Camoufox context tears down so Fidelity
# invalidates the session cookie server-side. If selectors drift
# the context.close() still cleans up locally — Fidelity's idle
# timeout (~15 min) will eventually invalidate the cookie too.
SEL_LOGOUT_CANDIDATES = (
    "a:has-text('Log Out')",
    "a:has-text('Log out')",
    "button:has-text('Log Out')",
    "[aria-label='Log Out']",
)


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

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
    URL."""
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
    ts = bronze.ts_slug()
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

def _probe_activity_date_bounds(page, capture_dir):
    """Open the page-level time-period picker → Custom tab, read the
    ``min`` / ``max`` attributes off the date inputs, return
    ``(min_date, max_date)``. Either may be ``None`` if the probe
    couldn't reach the inputs (selector drift, picker collapsed,
    no bounds set). Doesn't click Apply — purely a read.

    Called once at the start of an activity backfill so the
    chunker can clamp the requested ``--since`` / ``--until`` to
    Fidelity's available retention window. Without this clamp, a
    ``--lookback all`` against multi-decade history burns one
    pointless round-trip per 93-day window walking from the
    requested start up to Fidelity's retention floor (each clamps
    to the same single-day window and overwrites the same CSV
    file).
    """
    pill = page.locator("[data-testid='ap143528-timeperiod-filter']")
    if pill.count() == 0:
        log.debug("timepicker pill not found; bounds probe aborted")
        return None, None
    try:
        pill.first.evaluate(
            "el => { (el.querySelector('button') || el).click(); }"
        )
    except Exception as e:
        log.debug("timepicker pill open click (probe): %s", e)
        return None, None
    time.sleep(0.8)
    custom_segment = page.locator("apex-kit-segment[pvd-value='Custom']")
    if custom_segment.count() == 0:
        log.debug("Custom tab not found; bounds probe aborted")
        return None, None
    try:
        custom_segment.first.evaluate(
            "el => { "
            "  const inp = el.querySelector('input[type=radio]'); "
            "  if (inp) inp.click(); else el.click(); "
            "}"
        )
    except Exception as e:
        log.debug("Custom tab click (probe): %s", e)
        return None, None
    time.sleep(0.8)
    capture(page, capture_dir, "activity-custom-bounds-probe")
    from_input = page.locator("#customized-timeperiod-from-date").first
    to_input = page.locator("#customized-timeperiod-to-date").first
    if from_input.count() == 0 or to_input.count() == 0:
        log.debug("Custom-tab date inputs not found; bounds probe aborted")
        return None, None
    def _read(loc):
        try:
            return loc.evaluate(
                "el => ({min: el.min || null, max: el.max || null})"
            )
        except Exception:
            return {"min": None, "max": None}
    def _parse(s):
        if not s:
            return None
        try:
            return datetime.strptime(s, "%Y-%m-%d").date()
        except (TypeError, ValueError):
            return None
    fb = _read(from_input)
    tb = _read(to_input)
    return _parse(fb.get("min")), _parse(tb.get("max"))


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
    orig_since, orig_until = since_date, until_date
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
    # Defensive: if both endpoints collapsed onto the same boundary
    # after clamping (and the caller's original window was wider),
    # we're outside Fidelity's retention — fetching would return
    # the same single-day CSV over and over, overwriting any real
    # data the same chunker emits for in-range windows. Bail out
    # of just this window; `scrape_activity` does a one-time
    # pre-flight probe (`_probe_activity_date_bounds`) to clamp
    # the overall range upfront, so reaching this branch means
    # Fidelity shifted its retention floor mid-run or the probe
    # failed.
    if filled_since == filled_until and orig_since != orig_until:
        log.warning(
            "activity Custom-range collapsed by clamping "
            "(requested %s..%s, clamped to %s); skipping window",
            orig_since.isoformat(), orig_until.isoformat(),
            filled_since.isoformat(),
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
        # Pre-flight: clamp the requested range to whatever
        # Fidelity exposes in its Custom-tab min/max attributes
        # (typically ~5 years of retention). Without this clamp,
        # a `--lookback all` against a 30-year requested window
        # would chunk into ~120 same-empty-CSV iterations before
        # the cursor reaches the available range.
        fmin, fmax = _probe_activity_date_bounds(page, capture_dir)
        if fmin and since_date < fmin:
            log.info(
                "activity backfill: clamping requested since=%s up "
                "to Fidelity's earliest available %s",
                since_date.isoformat(), fmin.isoformat(),
            )
            since_date = fmin
        if fmax and until_date > fmax:
            log.info(
                "activity backfill: clamping requested until=%s "
                "down to Fidelity's latest available %s",
                until_date.isoformat(), fmax.isoformat(),
            )
            until_date = fmax
        if until_date < since_date:
            log.info(
                "activity backfill: requested range falls entirely "
                "outside Fidelity's available window (probed %s..%s); "
                "skipping activity phase",
                fmin and fmin.isoformat(),
                fmax and fmax.isoformat(),
            )
            return []
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


# Statement row labels are heterogeneous (monthly: "January 2026 — ...",
# quarterly: "Jan-March 2026 — ...", annual: "Annual 2026 — ..."), so we
# anchor on the 4-digit '20YY' year token, which every label form
# contains. Year-precision filtering keeps the per-row download path
# index-stable; finer granularity would need per-label format-specific
# parsing.
_STATEMENT_LABEL_YEAR_RE = re.compile(r"\b(20\d{2})\b")


def _statement_label_year(label: str) -> int | None:
    """Extract the year from a statement-row aria-label, or None if
    no recognisable year token is present (the row is then kept; the
    safer default is to download than to silently skip)."""
    m = _STATEMENT_LABEL_YEAR_RE.search(label)
    return int(m.group(1)) if m else None


# Known Fidelity 'Time Period' options. Each (label, days_covered)
# row is one possible option the listbox may surface. The set is
# customer-specific (account-age-bounded), so the runtime walks
# this ladder and picks the first option that's actually visible.
# Days_covered is the approximate window that label covers; pairs
# of labels that mean the same thing (``Last 24 months`` /
# ``Last 2 years``) share the same days value.
_FIDELITY_PERIOD_LADDER = (
    ("Last 3 months", 92),
    ("Last 6 months", 184),
    ("Last 12 months", 366),
    ("Last 18 months", 549),
    ("Last 24 months", 731),
    ("Last 2 years", 731),
    ("Last 36 months", 1097),
    ("Last 3 years", 1097),
    ("Last 5 years", 1827),
    ("Last 10 years", 3653),
    ("All time", 365 * 30),
    ("All", 365 * 30),
)


def _select_statements_time_period(page, capture_dir, target_days=None):
    """Expand the Statements page's 'Time Period' dropdown and pick
    the option that best matches ``target_days``.

    Fidelity uses a PVD listbox-style dropdown labelled
    ``Time Period`` at the top of the Statements grid. The
    collapsed state shows only the currently-selected option; the
    expansion exposes the full list (``Last 3 months``,
    ``Last 6 months``, ``Last 12 months``, … through the
    customer's account-age maximum). Selecting a new option
    triggers a grid re-fetch.

    Selection policy (mirrors schwab-web's preset mapping):

    * ``target_days`` set — pick the narrowest exposed option that
      fully covers the window. Defaults at the
      collectorkit/shared/wealthdb-refresh layer (90 days) thus
      map to ``Last 3 months`` here, matching schwab-web's
      ``Last3Months`` default. ``--lookback all`` widens through
      the ladder to ``All time``.
    * ``target_days`` None — widest available wins. Used by the
      historical-PDF backfill path, where the caller hasn't
      bounded the window.

    Returns the label of the option selected, or ``None`` if no
    option could be applied (the dropdown's pre-existing selection
    stays in effect, which still produces a working — if possibly
    shallow — dump).

    Defensive: every step degrades silently to the default rather
    than raising, so a future PVD redesign that drifts the
    selectors doesn't break the statements walk wholesale.
    """
    trigger = None
    for sel in (
        "button[aria-label='Time Period']",
        "[aria-label='Time Period'][role='button']",
        "[aria-label='Time Period']",
    ):
        loc = page.locator(sel).first
        try:
            if loc.count() > 0 and loc.is_visible(timeout=500):
                trigger = loc
                break
        except Exception as e:
            log.debug("statements: time-period probe %r: %s", sel, e)
    if trigger is None:
        log.info(
            "statements: no 'Time Period' filter; default window in effect"
        )
        return None
    try:
        trigger.click(timeout=5_000)
        time.sleep(0.8)
    except Exception as e:
        log.warning("statements: time-period dropdown click failed: %s", e)
        return None
    capture(page, capture_dir, "statements-time-period-open")
    if target_days is None:
        # No bound — widest first (used by the unbounded historical-
        # PDF backfill path).
        ordered = sorted(_FIDELITY_PERIOD_LADDER, key=lambda r: -r[1])
    else:
        # Narrowest that fully covers `target_days` wins; non-covering
        # options sink to the end so they're picked only if no
        # covering option is visible at all (Fidelity caps the set
        # to the customer's account age).
        def _key(row):
            _, days = row
            covers = days >= target_days
            return (0 if covers else 1, days if covers else -days)
        ordered = sorted(_FIDELITY_PERIOD_LADDER, key=_key)
    chosen = None
    for label, _days in ordered:
        for sel in (
            f"[role='option']:has-text('{label}')",
            f"li:has-text('{label}')",
            f"button:has-text('{label}')",
        ):
            loc = page.locator(sel).first
            if loc.count() == 0:
                continue
            try:
                if not loc.is_visible(timeout=300):
                    continue
                loc.click(timeout=3_000)
                chosen = label
                break
            except Exception as e:
                log.debug(
                    "statements: time-period option %r via %r: %s",
                    label, sel, e,
                )
        if chosen:
            break
    if chosen is None:
        log.info(
            "statements: no widening Time Period option matched; "
            "default window stays in effect"
        )
        try:
            page.keyboard.press("Escape")
        except Exception:
            pass
        return None
    log.info("statements: Time Period set to %r", chosen)
    try:
        page.wait_for_load_state("networkidle", timeout=15_000)
    except Exception:
        time.sleep(3.0)
    capture(page, capture_dir, "statements-time-period-applied")
    return chosen


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


def scrape_statements(page, context, docs_dir, capture_dir,
                       min_year=None, target_days=None):
    """Walk the Statements sub-page; for each ``(pdf)`` row open
    the per-row download popover and grab both formats Fidelity
    offers ('Download as PDF' / 'Download as CSV'). The CSV path
    comes down as a regular file download; the PDF path opens a
    popup tab that the browser would normally render in its PDF
    viewer — we fetch the popup's URL via context.request so the
    bytes land on disk regardless of viewer behaviour.

    Not every row offers both formats — annual investment reports
    typically only offer PDF. The CSV variant for those rows just
    fails the menuitem lookup and is recorded as not-available.

    Before enumerating rows, the Statements page's ``Time Period``
    filter is widened to cover ``target_days`` (default 90) so
    rows older than the dropdown's default ``Last 6 months`` are
    visible — see ``_select_statements_time_period``.

    If ``min_year`` is set, rows whose label-year is earlier than
    that are skipped (the default scope from walk() is ``documents
    _since.year``)."""
    _select_statements_time_period(
        page, capture_dir, target_days=target_days,
    )
    results = []
    labels = _enumerate_statement_row_labels(page)
    log.info("statements: %d (pdf) rows visible", len(labels))
    in_scope: list[tuple[int, str]] = []
    for i, label in enumerate(labels):
        if min_year is not None:
            yr = _statement_label_year(label)
            if yr is not None and yr < min_year:
                log.info(
                    "statements: skip row %d (year %d < min_year %d): %r",
                    i, yr, min_year, label[:60],
                )
                continue
        in_scope.append((i, label))
    if min_year is not None:
        log.info(
            "statements: %d of %d rows in scope (min_year=%d)",
            len(in_scope), len(labels), min_year,
        )
    for n, (i, label) in enumerate(in_scope):
        # Brief inter-row settle so the SPA's popover state from the
        # prior iteration's failed click / closed popup doesn't bleed
        # into this row's icon click.
        if n > 0:
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


def scrape_tax_forms(page, docs_dir, capture_dir, min_year=None):
    """Walk the Tax-forms sub-page across every selectable year in
    Fidelity's TimeFilter dropdown (typically 7 years going back
    to 2019). Each year's form list is enumerated independently;
    anchors are plain ``<a>`` links so we collect via
    ``page.expect_download`` (no popover indirection).
    Year-empty selections (no forms generated for that year, e.g.
    accounts that didn't yet exist) are logged and skipped.

    If ``min_year`` is set, years older than it are skipped — the
    default scope from walk() is ``documents_since.year``."""
    years = _enumerate_tax_form_years(page)
    if not years:
        log.warning(
            "no tax-year options enumerated; falling back to "
            "current selection"
        )
        return _scrape_tax_year(page, "current", docs_dir, capture_dir)
    if min_year is not None:
        def _yr(y):
            try:
                return int(str(y))
            except (TypeError, ValueError):
                return None
        kept = [y for y in years if _yr(y) is None or _yr(y) >= min_year]
        skipped = [y for y in years if y not in kept]
        if skipped:
            log.info(
                "tax-forms: skipping %d years older than %d: %s",
                len(skipped), min_year, skipped,
            )
        years = kept
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


def scrape_documents(page, context, bronze_dir, capture_dir,
                      min_year=None, target_days=None):
    """Walk Statements + Tax forms in the document center. Each
    sub-page is exercised independently so a failure in one
    category doesn't block the other. ``context`` is needed for the
    popup-PDF-via-context.request fallback path on statements.

    ``min_year`` (forwarded to both sub-scrapers) drops rows / year
    selections older than that — see walk()'s default-derivation
    from ``--documents-since``. ``target_days`` is the requested
    documents window length, used to pick the Statements page's
    ``Time Period`` filter (narrowest exposed option that covers
    the window; ``None`` widens to the full archive)."""
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
            min_year=min_year,
            target_days=target_days,
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
                min_year=min_year,
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
# walk() — orchestrates the per-phase scrapes against a live session
# ---------------------------------------------------------------------------

def walk(context, page, config):
    """Run the requested phase(s) against a logged-in session.
    Called from ``run_oneshot()`` after ``login()`` lands on the
    post-auth URL. Dispatches to the per-phase scrape functions
    based on ``config['mode']``. Each phase has its own
    try/except inside its dispatcher; a failure in one phase does
    not block the others."""
    dest_root = Path(config.get("dest") or "/data")
    bronze_dir = dest_root / bronze.ts_slug()
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

    # Documents scope: default to a 90-day lookback so a forgotten
    # flag doesn't trigger a multi-year backfill of the statements
    # archive + every available tax year. The sub-scrapers filter
    # at year granularity (label parsing for statements; year-
    # selector for tax forms), so the effective floor is the year
    # of documents_since. wealthdb-refresh --lookback widens it.
    documents_since = parse_iso_date(config.get("documents_since"))
    if documents_since is None:
        documents_since = (
            datetime.now(timezone.utc).date() - timedelta(days=90)
        )
    documents_until = parse_iso_date(config.get("documents_until"))
    if documents_until is None:
        documents_until = datetime.now(timezone.utc).date()
    docs_min_year = documents_since.year
    docs_target_days = max(1, (documents_until - documents_since).days)

    run_json = {
        "snapshot_at": bronze_dir.name,
        "cli_config": config,
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
        "documents_window": {
            "since": documents_since.isoformat(),
            "min_year": docs_min_year,
        },
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
                min_year=docs_min_year,
                target_days=docs_target_days,
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


# ---------------------------------------------------------------------------
# Env-file loader
# ---------------------------------------------------------------------------

def load_env_file(path):
    """Source KEY=VALUE pairs from ``path`` into ``os.environ``.

    For credentials (FIDELITY_USERNAME / FIDELITY_PASSWORD) the file
    value wins over an already-set host env var, because the host
    shell's ``source`` does $-expansion on double-quoted values,
    which would silently mangle passwords containing $, !, backtick.
    The file itself, read byte-for-byte by us, has the original
    intact. Single-quoted values defeat the issue at the source.

    Other vars use setdefault (env-file is a fallback for those).
    Outer matching quotes (single OR double) are stripped. Lines
    starting with ``#`` and blank lines are ignored. Malformed lines
    raise.
    """
    log.debug("loading env file: %s", path)
    with path.open("r", encoding="utf-8") as fh:
        for lineno, raw in enumerate(fh, 1):
            line = raw.strip()
            if not line or line.startswith("#"):
                continue
            if line.startswith("export "):
                line = line[len("export "):]
            if "=" not in line:
                raise SystemExit(
                    f"env file {path}:{lineno}: not KEY=VALUE: "
                    f"{raw.rstrip()!r}"
                )
            key, _, value = line.partition("=")
            key = key.strip()
            value = _strip_outer_quotes(value.strip())
            if not key:
                raise SystemExit(
                    f"env file {path}:{lineno}: empty key"
                )
            if key in _CRED_OVERRIDE_VARS:
                prior = os.environ.get(key)
                if prior is not None and prior != value:
                    log.warning(
                        "%s inherited from host env (len=%d) differs "
                        "from %s file value (len=%d); using file value. "
                        "(Use SINGLE quotes around values containing "
                        "$/!/backtick to avoid host `source` mangling.)",
                        key, len(prior), path, len(value),
                    )
                os.environ[key] = value
            else:
                os.environ.setdefault(key, value)


def _strip_outer_quotes(s):
    if len(s) >= 2 and s[0] == s[-1] and s[0] in ("'", '"'):
        return s[1:-1]
    return s


def maybe_source_env_files(args):
    if args.env_file is not None:
        if not args.env_file.exists():
            raise SystemExit(
                f"--env-file does not exist: {args.env_file}"
            )
        load_env_file(args.env_file)
        return
    for path in DEFAULT_ENV_FILE_CANDIDATES:
        if path.exists():
            load_env_file(path)
            return


# ---------------------------------------------------------------------------
# Profile-dir + Camoufox launch
# ---------------------------------------------------------------------------

def prepare_profile_dir(profile_dir):
    """Create the user-data-dir if missing and chmod it 0700."""
    profile_dir.mkdir(parents=True, exist_ok=True)
    try:
        os.chmod(profile_dir, PROFILE_DIR_MODE)
    except OSError as exc:
        log.warning(
            "could not chmod %s to 0%o: %s",
            profile_dir, PROFILE_DIR_MODE, exc,
        )


@contextlib.contextmanager
def open_camoufox_context(profile_dir, trace):
    """Open Camoufox with a persistent profile dir, yielding the
    BrowserContext. The context auto-closes on exit.

    Camoufox is a stealth-patched Firefox fork that masks the
    fingerprint surfaces (canvas, WebGL, audio, fonts, navigator.*,
    TLS) Akamai's bot-scoring uses to detect Playwright-driven
    browsers. ``os='macos'`` runs the full macOS-pretend mode so the
    fingerprint is internally consistent — much stronger than the
    piecemeal UA + navigator.platform overrides you'd set on upstream
    Playwright Firefox.

    Headed + window=(1280, 800) avoids the mobile responsive layout
    Fidelity serves to narrow viewports; the Xvfb display from
    entrypoint.sh provides the X11 surface (camoufox respects DISPLAY
    when set).
    """
    from camoufox.sync_api import Camoufox
    with Camoufox(
        persistent_context=True,
        user_data_dir=str(profile_dir),
        os="macos",
        window=(1280, 800),
        headless=False,
        # humanize: curved, variable-speed cursor movement on every
        # click. Routes Playwright's instant .click() calls through a
        # synthetic-cursor path that fires mousemove + mousedown +
        # mouseup events Akamai's bot-scoring can observe.
        humanize=True,
        # geoip=True uses the camoufox[geoip] MaxMind DB bundled at
        # build time to derive the timezone, locale, lat/lon from the
        # egress IP autodetected at launch. Without this, Camoufox
        # defaults can mismatch the IP's geography (e.g. en-US locale
        # on a non-US egress IP), which is a cheap signal for
        # Akamai's geo-anomaly heuristic. (Note: camoufox expects
        # literal True for autodetect, not the string "auto" — that
        # string gets handed straight to the IP-validator and raises
        # InvalidIP.)
        geoip=True,
    ) as context:
        context.set_default_navigation_timeout(LANDMARK_TIMEOUT_MS)
        context.set_default_timeout(LANDMARK_TIMEOUT_MS)
        if trace:
            context.tracing.start(
                screenshots=True, snapshots=True, sources=True,
            )
        yield context


def open_page(context):
    """Return a Page in the given context, reusing the existing one
    (Camoufox's persistent_context always opens one) or creating a
    fresh one if needed."""
    pages = context.pages
    if pages:
        return pages[0]
    return context.new_page()


def maybe_capture(page, screenshot_dir, label):
    """Save HTML + PNG at a navigation landmark. Never raises.

    Differs from ``capture(page, capture_dir, label)`` above (used by
    the walk phases): this is the login-flow variant that writes to
    a user-supplied ``--screenshot-dir`` rather than the bronze
    dir's ``screenshots/`` subdir. Both exist because the bronze
    capture path is implicit (every walk run) while the login path
    is opt-in (only when --screenshot-dir is passed)."""
    if screenshot_dir is None:
        return
    try:
        screenshot_dir.mkdir(parents=True, exist_ok=True)
    except Exception as e:
        log.warning("create screenshot dir %s: %s", screenshot_dir, e)
        return
    ts = bronze.ts_slug()
    try:
        html_path = screenshot_dir / f"{ts}-{label}.html"
        try:
            html = page.content()
        except Exception:
            html = page.evaluate(
                "() => document.documentElement.outerHTML"
            )
        html_path.write_text(html, encoding="utf-8")
        log.debug("wrote HTML %s", html_path)
    except Exception as e:
        log.warning("html capture %s failed: %s", label, e)
    try:
        png_path = screenshot_dir / f"{ts}-{label}.png"
        page.screenshot(
            path=str(png_path),
            full_page=False, timeout=5_000, animations="disabled",
        )
        log.debug("wrote screenshot %s", png_path)
    except Exception as e:
        log.debug("screenshot %s failed (HTML saved): %s", label, e)


def stop_trace_if_active(context, trace, screenshot_dir, label):
    if not trace:
        return
    if screenshot_dir is None:
        log.warning("--trace without --screenshot-dir; trace discarded")
        return
    screenshot_dir.mkdir(parents=True, exist_ok=True)
    trace_path = screenshot_dir / f"{bronze.ts_slug()}-{label}-trace.zip"
    try:
        context.tracing.stop(path=str(trace_path))
        log.info("trace saved to %s", trace_path)
    except Exception as e:
        log.warning("stop trace failed: %s", e)


# ---------------------------------------------------------------------------
# Login-flow primitives
# ---------------------------------------------------------------------------

def wait_for_signin_or_iua(page, timeout_s):
    """Poll for whichever lands first: the signin form (password
    input visible) or the International Usage Agreement
    interstitial (accept link visible). Returns ('signin', None),
    ('iua', locator), or (None, None) on timeout."""
    deadline = time.monotonic() + timeout_s
    while time.monotonic() < deadline:
        for sel in SEL_IUA_ACCEPT_CANDIDATES:
            try:
                loc = page.locator(sel)
                if loc.count() > 0 and loc.first.is_visible(timeout=300):
                    return ("iua", loc.first)
            except Exception as e:
                log.debug("IUA probe (%s): %s", sel, e)
        try:
            pwd_loc = page.locator(SEL_PASSWORD_INPUT)
            if pwd_loc.count() > 0 and pwd_loc.first.is_visible(timeout=300):
                return ("signin", None)
        except Exception as e:
            log.debug("signin-form probe: %s", e)
        time.sleep(0.5)
    return (None, None)


def accept_iua(page, accept_loc):
    accept_loc.click()
    log.info("clicked I Accept on International Usage Agreement")


def fill_username(page, username):
    """Fill the username field, handling both the text-input form
    (fresh-device case) and the <select> dropdown form (returning-
    device, where Fidelity remembered the username)."""
    sel_locator = page.locator(SEL_USERNAME_SELECT)
    if sel_locator.count() > 0:
        log.info(
            "username <select> present (returning-device path); "
            "selecting 'Enter different username'"
        )
        sel_locator.select_option(SEL_USERNAME_OTHER_OPTION_VALUE)
    for sel in SEL_USERNAME_TEXT_INPUT_CANDIDATES:
        loc = page.locator(sel)
        try:
            if loc.count() > 0 and loc.first.is_visible(timeout=2_000):
                loc.first.fill(username)
                log.info("filled username via %s", sel)
                return
        except Exception as e:
            log.debug("username candidate %s failed: %s", sel, e)
    fallback = page.locator(
        "input[type=text]:visible, input:not([type]):visible"
    )
    if fallback.count() > 0:
        fallback.first.fill(username)
        log.info("filled username via last-resort visible text input")
        return
    raise SystemExit(
        "could not locate the username input field. Check the "
        "captured HTML in --screenshot-dir for what Fidelity served."
    )


def fill_password(page, password):
    page.locator(SEL_PASSWORD_INPUT).fill(password)
    log.info("filled password")


def click_login(page):
    page.locator(SEL_LOGIN_BUTTON).click(timeout=LANDMARK_TIMEOUT_MS)
    log.info("clicked %s", SEL_LOGIN_BUTTON)


def wait_for_mfa_input_or_post_auth(page, timeout_s):
    """Poll for whichever appears first: the 2FA code input (still
    needs the operator) or the post-auth URL (device trusted, MFA
    skipped). Returns ('mfa', locator), ('post_auth', None), or
    (None, None) on timeout."""
    deadline = time.monotonic() + timeout_s
    while time.monotonic() < deadline:
        url = live_url(page)
        if url.startswith(POST_AUTH_PREFIX):
            return ("post_auth", None)
        try:
            loc = page.locator(SEL_2FA_CODE_INPUT)
            if loc.count() > 0 and loc.first.is_visible(timeout=500):
                return ("mfa", loc.first)
        except Exception as e:
            log.debug("MFA probe error: %s", e)
        time.sleep(0.5)
    return (None, None)


def prompt_for_mfa_code():
    """Print a prompt to stderr and read a code from stdin. stderr
    is used so the prompt is visible even when stdout is redirected
    to a log file. Returns the stripped code; empty input returns
    ''. Blocking — Ctrl-C to abort."""
    sys.stderr.write("\n")
    sys.stderr.write("=" * 60 + "\n")
    sys.stderr.write(
        "Fidelity 2FA: enter your security code, then press Enter.\n"
    )
    sys.stderr.write("> ")
    sys.stderr.flush()
    try:
        code = sys.stdin.readline()
    except KeyboardInterrupt:
        sys.stderr.write("\n")
        raise
    sys.stderr.write("=" * 60 + "\n")
    sys.stderr.flush()
    return code.strip()


def ensure_trust_browser_checked(page):
    """Best-effort: tick the 'Trust this browser' checkbox so the
    device-trust cookie suppresses MFA on subsequent runs.

    PVD wraps the <input> in a <label> whose ::before overlay
    intercepts pointer events on the input. .check() would retry
    for the full 60s default — burning TOTP validity. We click the
    LABEL instead. Bounded to 2s; non-fatal on failure."""
    try:
        cb = page.locator(SEL_TRUST_BROWSER_CHECKBOX)
        if cb.count() == 0:
            log.info("no Trust-Browser checkbox on this page")
            return
        if cb.is_checked():
            log.info("Trust-Browser checkbox already checked")
            return
        page.locator(SEL_TRUST_BROWSER_LABEL).click(timeout=2_000)
        log.info("ticked Trust-Browser checkbox via its label")
    except Exception as e:
        log.warning(
            "trust-browser tick failed (%s); proceeding anyway", e,
        )


def submit_mfa_code(page, code_locator, code):
    """Fill the MFA input and submit. Try button candidates; on
    no-match, press Enter inside the input."""
    code_locator.fill(code)
    for sel in SEL_2FA_SUBMIT_CANDIDATES:
        try:
            btn = page.locator(sel).first
            if btn.count() == 0:
                continue
            if not btn.is_visible(timeout=500):
                continue
            btn.click(timeout=10_000)
            log.info("submitted 2FA via %s", sel)
            return True
        except Exception as e:
            log.debug("2FA submit candidate %s failed: %s", sel, e)
    try:
        code_locator.press("Enter")
        log.info("submitted 2FA via Enter key in input")
        return True
    except Exception as e:
        log.error("could not press Enter to submit 2FA: %s", e)
        return False


def wait_for_post_auth_url(page, timeout_s):
    deadline = time.monotonic() + timeout_s
    last_logged = None
    while time.monotonic() < deadline:
        try:
            url = live_url(page)
            if url != last_logged:
                log.debug("waiting for post-auth; live URL: %s", url)
                last_logged = url
            if url.startswith(POST_AUTH_PREFIX):
                log.info("post-auth landing reached: %s", url)
                return True
        except Exception as e:
            log.debug("url probe: %s", e)
        time.sleep(1.0)
    return False


# ---------------------------------------------------------------------------
# Top-level orchestration
# ---------------------------------------------------------------------------

def login(page, args, username, password):
    """Drive the SPA login: IUA → credentials → 2FA → post-auth.
    Returns True on success, False on failure (caller decides
    whether to exit non-zero or fall through to walk anyway)."""
    log.info("navigating to %s", SIGNIN_URL)
    page.goto(SIGNIN_URL, wait_until="domcontentloaded")
    maybe_capture(page, args.screenshot_dir, "01-initial-load")

    kind, iua_loc = wait_for_signin_or_iua(page, timeout_s=30)
    if kind is None:
        maybe_capture(page, args.screenshot_dir, "01b-load-timeout")
        log.error(
            "neither the signin form nor the IUA interstitial "
            "rendered within 30s. The page skeleton may be present "
            "but stuck in Akamai's JS challenge, or Fidelity served "
            "a third page type. Inspect --screenshot-dir captures."
        )
        return False
    if kind == "iua":
        log.info(
            "International Usage Agreement interstitial detected; "
            "clicking I Accept"
        )
        maybe_capture(page, args.screenshot_dir, "01a-iua-page")
        accept_iua(page, iua_loc)
        try:
            page.locator(SEL_PASSWORD_INPUT).wait_for(
                state="visible", timeout=30_000,
            )
        except Exception as e:
            maybe_capture(
                page, args.screenshot_dir, "01c-post-iua-no-form",
            )
            log.error(
                "signin form did not appear within 30s after IUA "
                "acceptance: %s", e,
            )
            return False
        maybe_capture(
            page, args.screenshot_dir, "01d-signin-after-iua",
        )
    else:
        log.info("signin form rendered directly (no IUA)")
        maybe_capture(page, args.screenshot_dir, "01e-signin-direct")

    try:
        fill_username(page, username)
        fill_password(page, password)
    except Exception as e:
        maybe_capture(page, args.screenshot_dir, "02-prefill-failed")
        log.error("credential pre-fill failed: %s", e)
        return False
    maybe_capture(page, args.screenshot_dir, "02-prefilled")

    if args.vnc:
        sys.stderr.write("\n" + "=" * 60 + "\n")
        sys.stderr.write(
            "READY: Camoufox is up and the login form is pre-filled.\n"
            "Connect with your VNC client now (127.0.0.1:5900, "
            "password printed at the top of this output by "
            "entrypoint.sh). Then in the VNC window:\n"
            "  1. Click 'Log in'.\n"
            "  2. Complete the 2FA challenge.\n"
            "  3. Wait for the post-auth landing page to load.\n"
            f"This script will detect the post-auth URL and "
            f"continue automatically (up to "
            f"{args.vnc_wait_timeout:.0f}s wait).\n"
        )
        sys.stderr.write("=" * 60 + "\n")
        sys.stderr.flush()
        if wait_for_post_auth_url(
            page, timeout_s=args.vnc_wait_timeout,
        ):
            maybe_capture(page, args.screenshot_dir, "08-post-auth")
            return True
        maybe_capture(
            page, args.screenshot_dir, "vnc-post-auth-timeout",
        )
        log.error(
            "did not see %s* within %.0fs after VNC handoff.",
            POST_AUTH_PREFIX, args.vnc_wait_timeout,
        )
        return False

    try:
        click_login(page)
    except Exception as e:
        maybe_capture(page, args.screenshot_dir, "03-submit-failed")
        log.error("login submit failed: %s", e)
        return False
    maybe_capture(page, args.screenshot_dir, "03-submit-clicked")
    time.sleep(1.0)
    maybe_capture(page, args.screenshot_dir, "04-submit-settled")

    log.info(
        "waiting up to %.0fs for the 2FA challenge page (or for "
        "direct post-auth landing if device is already trusted)",
        args.mfa_page_timeout,
    )
    kind, code_loc = wait_for_mfa_input_or_post_auth(
        page, timeout_s=args.mfa_page_timeout,
    )
    if kind == "post_auth":
        log.info(
            "post-auth URL reached WITHOUT a 2FA challenge — "
            "device-trust cookie must have been present"
        )
        maybe_capture(page, args.screenshot_dir, "08-post-auth")
        return True
    if kind == "mfa":
        maybe_capture(page, args.screenshot_dir, "05-mfa-page")
        if args.trust_this_browser:
            ensure_trust_browser_checked(page)
        else:
            log.info(
                "--no-trust-this-browser: leaving the checkbox "
                "unticked"
            )
        code = prompt_for_mfa_code()
        if not code:
            log.error("no 2FA code entered; aborting")
            return False
        if not submit_mfa_code(page, code_loc, code):
            maybe_capture(
                page, args.screenshot_dir, "06-mfa-submit-failed",
            )
            return False
        maybe_capture(page, args.screenshot_dir, "06-mfa-submitted")
        log.info("2FA submitted; waiting for post-auth landing URL")
        if not wait_for_post_auth_url(
            page, timeout_s=args.nav_timeout,
        ):
            maybe_capture(
                page, args.screenshot_dir, "07-post-auth-timeout",
            )
            log.error(
                "did not see %s* within %.0fs after 2FA submit",
                POST_AUTH_PREFIX, args.nav_timeout,
            )
            return False
        maybe_capture(page, args.screenshot_dir, "08-post-auth")
        return True
    maybe_capture(
        page, args.screenshot_dir, "05-mfa-timeout-or-block",
    )
    log.error(
        "neither 2FA input nor post-auth URL within %.0fs. Likely "
        "outcomes: anti-bot block (Akamai), selectors drifted, or "
        "Fidelity served a different challenge type.",
        args.mfa_page_timeout,
    )
    return False


def logout(page, screenshot_dir):
    """Best-effort logout: click any visible Log Out link. Never
    raises. If selectors drift the Camoufox context teardown is
    still our cleanup and Fidelity's idle timeout (~15 min)
    invalidates the session cookie server-side anyway."""
    for sel in SEL_LOGOUT_CANDIDATES:
        try:
            loc = page.locator(sel).first
            if loc.count() == 0:
                continue
            if not loc.is_visible(timeout=500):
                continue
            loc.click(timeout=5_000)
            log.info("logout: clicked %r", sel)
            time.sleep(1.5)
            maybe_capture(page, screenshot_dir, "99-logout")
            return
        except Exception as e:
            log.debug("logout candidate %r: %s", sel, e)
    log.info(
        "logout: no matching link visible; relying on context "
        "teardown + server-side idle timeout"
    )


def run_check(args):
    """Validate the existing profile dir by navigating to the
    post-auth landing. Returns 0 if alive, non-zero otherwise."""
    if not args.profile_dir.exists():
        log.error("--profile-dir does not exist: %s", args.profile_dir)
        return 2
    prepare_profile_dir(args.profile_dir)
    with open_camoufox_context(args.profile_dir, args.trace) as context:
        try:
            page = open_page(context)
            landing = POST_AUTH_PREFIX + "summary"
            log.info("navigating to %s", landing)
            try:
                page.goto(landing, wait_until="domcontentloaded")
            except Exception as e:
                log.error("navigation failed: %s", e)
                return 3
            maybe_capture(page, args.screenshot_dir, "check-landed")
            time.sleep(2.0)
            url = live_url(page)
            if url.startswith(POST_AUTH_PREFIX):
                log.info("session ALIVE — landed at %s", url)
                return 0
            log.warning(
                "session DEAD — landed at %s (expected prefix %s)",
                url, POST_AUTH_PREFIX,
            )
            return 1
        finally:
            stop_trace_if_active(
                context, args.trace, args.screenshot_dir, "check",
            )


def run_oneshot(args):
    """Full path: login → walk → logout → exit."""
    username = os.environ.get(USERNAME_ENV)
    password = os.environ.get(PASSWORD_ENV)
    if not username or not password:
        log.error(
            "%s and %s must be set (or sourced from --env-file / "
            "default env-file paths). See README.md.",
            USERNAME_ENV, PASSWORD_ENV,
        )
        return 2
    log.info(
        "creds loaded: %s (len=%d), %s (len=%d)",
        USERNAME_ENV, len(username), PASSWORD_ENV, len(password),
    )
    prepare_profile_dir(args.profile_dir)
    with open_camoufox_context(args.profile_dir, args.trace) as context:
        try:
            page = open_page(context)
            if not login(page, args, username, password):
                return 3
            if args.vnc and args.mode == "none":
                # VNC handoff that's just seeding the profile dir —
                # no walk requested.
                log.info(
                    "VNC handoff complete; persistent profile state "
                    "at %s", args.profile_dir,
                )
                return 0
            since, until, docs_since, docs_until = cli.resolve_lookback(args)
            config = {
                "dest": str(args.dest),
                "mode": args.mode,
                "dry_run": "true" if args.dry_run else "false",
                "since": since.isoformat(),
                "until": until.isoformat(),
                "documents_since": docs_since.isoformat(),
                "documents_until": docs_until.isoformat(),
            }
            if args.exclude_accounts:
                config["exclude_accounts"] = args.exclude_accounts
            try:
                walk(context, page, config)
            finally:
                logout(page, args.screenshot_dir)
            return 0
        finally:
            stop_trace_if_active(
                context, args.trace, args.screenshot_dir, "download",
            )


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------

def parse_args(argv):
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    # --- Session / login args ---
    p.add_argument(
        "--profile-dir", type=Path,
        default=Path("/secrets/fidelity-web-profile"),
        help=("Camoufox persistent profile directory. Holds cookies, "
              "localStorage, and Akamai trust state across runs. "
              "Default: /secrets/fidelity-web-profile."),
    )
    p.add_argument(
        "--env-file", type=Path, default=None,
        help=("KEY=VALUE env file. When omitted, falls back to "
              "/secrets/fidelity-web.env then "
              "$HOME/.secrets/fidelity-web.env. Supplies "
              "FIDELITY_USERNAME / FIDELITY_PASSWORD."),
    )
    p.add_argument(
        "--check", action="store_true",
        help=("Verify the session is alive (navigate to post-auth "
              "landing, no credential submit, no MFA). Exits 0 if "
              "alive, non-zero otherwise. Skips walk + logout."),
    )
    p.add_argument(
        "--vnc", action="store_true",
        help=("Pre-fill credentials, then WAIT for the operator to "
              "click Log In and complete 2FA via VNC. The walk "
              "still runs after the VNC-driven login lands "
              "(unless --mode none)."),
    )
    p.add_argument(
        "--vnc-wait-timeout", type=float, default=600.0,
        help="VNC wait timeout in seconds. Default 600 (10 min).",
    )
    p.add_argument(
        "--trust-this-browser",
        action=argparse.BooleanOptionalAction, default=True,
        help=("Tick the 'Trust this browser' checkbox at 2FA so "
              "subsequent runs skip MFA. Default: True."),
    )
    p.add_argument(
        "--mfa-page-timeout", type=float, default=15.0,
        help="Seconds to wait for 2FA / post-auth after submit.",
    )
    p.add_argument(
        "--nav-timeout", type=float, default=60.0,
        help="Per-navigation timeout in seconds. Default 60.",
    )
    # --- Walk args ---
    p.add_argument(
        "--dest", type=Path, default=Path("/data"),
        help="Bronze tree root. Default: /data.",
    )
    p.add_argument(
        "--mode", default="all",
        choices=("all", "positions", "activity", "documents",
                 "balances", "performance", "none"),
        help=("Which phase to run. 'all' (default) runs every "
              "phase. 'none' is for --vnc handoffs that only seed "
              "the profile dir."),
    )
    # Shared date-window contract. Fidelity bisects the activity
    # range into ≤93-day chunks internally; the statements +
    # tax-forms walk filters at YEAR granularity (row labels are
    # mixed monthly / quarterly / annual).
    cli.add_lookback_args(p)
    p.add_argument(
        "--exclude-accounts", default=None,
        help="Comma-separated account ids to skip.",
    )
    p.add_argument(
        "--dry-run", action="store_true",
        help="Walk + enumerate without writing artefacts.",
    )
    # --- Diagnostics ---
    p.add_argument(
        "--screenshot-dir", type=Path, default=None,
        help=("If set, save HTML + PNG at each navigation landmark. "
              "Never defaults under /secrets."),
    )
    p.add_argument(
        "--trace", action="store_true",
        help="Capture a Playwright trace bundle (requires "
             "--screenshot-dir).",
    )
    p.add_argument(
        "-v", "--verbose", action="store_true",
        help="DEBUG-level logging.",
    )
    return p.parse_args(argv)


def main(argv=None):
    args = parse_args(argv or sys.argv[1:])
    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )
    if args.trace and args.screenshot_dir is None:
        log.error("--trace requires --screenshot-dir")
        return 2
    maybe_source_env_files(args)
    if args.check:
        return run_check(args)
    return run_oneshot(args)


if __name__ == "__main__":
    sys.exit(main())
