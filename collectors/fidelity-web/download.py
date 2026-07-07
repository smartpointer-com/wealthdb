#!/usr/bin/env python3
"""
Fidelity client-web one-shot fetch: login → walk → logout → exit.

Boots Camoufox (a stealth-patched Firefox fork) against
``digital.fidelity.com``, runs the read-only login + MFA flow,
walks whichever export phases were selected, attempts a clean
logout, and exits. Bronze artefacts land in a timestamped
``<dest>/<UTC-ts>/`` directory.

Each HTML/CSV artefact is zstd-compressed in place as it lands
(``balances.html`` → ``balances.html.zst``, ``positions_*.csv`` /
``activity_*.csv`` likewise), via ``collectorkit.compress`` with a
decompress-and-verify pass before the plain file is removed.
Compression is best-effort: on failure the plain file stays and the
run still succeeds — ``load`` resolves either form. PDFs are left raw
(already internally compressed); ``run.json`` and the ``--debug``
``screenshots/`` tree are never compressed.

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
                [--debug]
                [--screenshot-dir /debug/<dir>]
                [-v]

Diagnostics are opt-in: ``--debug`` saves walk-phase captures
(HTML DOM dump + PNG per landmark) under ``<run>/screenshots/``;
``--screenshot-dir`` does the same for the pre-walk login /
session phases into an explicit host path. Neither is a load
input; ``prune`` deletes the former wholesale.
"""

from __future__ import annotations

import argparse
import base64
import contextlib
import hashlib
import json
import logging
import os
import re
import sys
import time
import zlib
from datetime import datetime, timedelta, timezone
from pathlib import Path

from collectorkit import bronze, cli, compress


log = logging.getLogger("fidelity-web.download")


def compress_export(path):
    """Compress a freshly saved HTML/CSV bronze artefact in place
    (`balances.html` → `balances.html.zst`), returning the on-disk path.

    Best-effort by design: `load` resolves either form, so a compression
    failure (disk full, missing codec) downgrades to a warning and the
    plain file stays — never a lost artefact. `compress.compress_file`
    decompress-and-sha256-verifies the twin before unlinking the
    original, so the window in which data could be lost is nil. PDFs are
    NEVER routed here — they are already internally compressed and are a
    load input in raw form."""
    try:
        final = compress.compress_file(path)
        log.info("  compressed %s → %s (%d bytes)", path.name,
                 final.name, final.stat().st_size)
        return final
    except Exception as exc:  # noqa: BLE001 — best-effort by design
        log.warning("  could not compress %s (%s); keeping the plain file",
                    path.name, exc)
        return path


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
# Fidelity migrated the document center off the portfolio host onto
# a separate "Enterprise Document Center" SPA. Navigating to
# URL_DOCUMENTS now 302s here, so the documents phase accepts this
# prefix as a valid post-auth landing (it is NOT a session-timeout
# bounce, which would redirect to the /prgw/digital/signin login).
DOCCENTER_PREFIX = "https://digitalservices.fidelity.com/navigate/ent-documentcenter/"

# Account-selector account-link testid pattern is
# ``ap143528-accounts-selector-account-link-<account-id>``, with
# the id being 9-digit for brokerage / trust / 529 and 7-digit
# for a Fidelity Charitable DAF (auto-excluded by length in
# walk()). The pattern is used inline by
# ``enumerate_account_dimensions``; the named selectors below
# cover the rest of the positions phase.
SEL_ALL_ACCOUNTS = ".acct-selector__all-accounts"
SEL_KEBAB_MENU = "[data-testid='kebab-menu']"
# Any account-selector account-link (the enumeration target). Used
# as a hydration proxy when retrying enumeration on the positions
# surface — the links render in the DOM even with the dropdown
# collapsed.
SEL_ACCOUNT_LINK = (
    "[data-testid^='ap143528-accounts-selector-account-link-']"
)
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


def dump_dom_inventory(page, capture_dir, label):
    """Exploration diagnostic: write a flat inventory of every
    identifiable element — piercing open shadow roots and
    same-origin iframes — to ``<ts>-<label>.dominv.json``.

    page.content() only serialises the light DOM of the top
    document, so SPAs that render lists inside shadow DOM or nested
    iframes (e.g. Fidelity's new Enterprise Document Center) come
    back empty in an ordinary capture. This walks shadow roots and
    same-origin iframe documents and records each element's tag /
    id / data-testid / role / aria-label / name / type / href and a
    short text snippet, so a UI-drift investigation can see the real
    interactive surface. Opt-in (``--explore``) — never raises."""
    if capture_dir is None:
        return
    js = r"""
    () => {
      const out = [];
      const SKIP = new Set(['SCRIPT','STYLE','NOSCRIPT','LINK','META',
                            'svg','path','DEFS','USE','SYMBOL']);
      const attr = (el, n) => (el.getAttribute ? el.getAttribute(n) : null);
      const rec = (root, frame) => {
        let els;
        try { els = root.querySelectorAll('*'); } catch (e) { return; }
        for (const el of els) {
          const tag = el.tagName;
          if (!tag || SKIP.has(tag)) continue;
          const tid = attr(el, 'data-testid');
          const role = attr(el, 'role');
          const aria = attr(el, 'aria-label');
          const href = attr(el, 'href');
          const id = el.id || null;
          const tagInteresting = ['A','BUTTON','INPUT','SELECT','OPTION',
            'IFRAME'].includes(tag) || tag.includes('-');
          if (tid || role || aria || href || id || tagInteresting) {
            let txt = '';
            try {
              txt = (el.textContent || '').trim()
                      .replace(/\s+/g, ' ').slice(0, 60);
            } catch (e) {}
            out.push({
              frame, tag, id, testid: tid, role, aria,
              name: attr(el, 'name'), type: attr(el, 'type'),
              href: href ? href.slice(0, 120) : null,
              cls: (el.className && el.className.toString)
                     ? el.className.toString().slice(0, 60) : null,
              text: txt,
            });
          }
          if (el.shadowRoot) rec(el.shadowRoot, frame + '>shadow');
        }
      };
      rec(document, 'top');
      for (const f of document.querySelectorAll('iframe')) {
        const tag = 'iframe:' + ((attr(f, 'title') || attr(f, 'name')
                       || f.src || '?').slice(0, 50));
        let doc = null;
        try { doc = f.contentDocument; } catch (e) {}
        if (doc) rec(doc, tag);
        else out.push({frame: 'iframe-CROSSORIGIN',
                       href: (f.src || '').slice(0, 120)});
      }
      return out;
    }
    """
    try:
        inv = page.evaluate(js)
    except Exception as e:
        log.warning("dom inventory %s: %s", label, e)
        return
    try:
        capture_dir.mkdir(parents=True, exist_ok=True)
        path = capture_dir / f"{bronze.ts_slug()}-{label}.dominv.json"
        path.write_text(json.dumps(inv, indent=1), encoding="utf-8")
        log.info("explore: wrote %d-element inventory to %s",
                 len(inv), path.name)
    except Exception as e:
        log.warning("dom inventory write %s: %s", label, e)


def _doccenter_settle(page):
    """Give the document-center list a chance to (re)render after a
    nav or filter change: wait for network-idle, scroll the document
    (and any scrollable containers) to force row materialisation,
    then scroll back to the top. Best-effort."""
    try:
        page.wait_for_load_state("networkidle", timeout=10_000)
    except Exception:
        pass
    try:
        page.evaluate(
            "() => { window.scrollTo(0, document.body.scrollHeight);"
            " for (const el of document.querySelectorAll('*')) {"
            "   if (el.scrollHeight > el.clientHeight + 50)"
            "     el.scrollTop = el.scrollHeight; } }"
        )
    except Exception:
        pass
    time.sleep(3.0)
    try:
        page.evaluate("() => window.scrollTo(0, 0)")
    except Exception:
        pass
    time.sleep(1.0)


def goto_and_wait(page, url, wait_selector=None, wait_timeout_s=30):
    """Navigate, then wait for an optional selector (proxy for 'SPA
    hydrated'). Returns the live URL after settle."""
    log.info("navigating to %s", url)
    try:
        page.goto(url, wait_until="domcontentloaded")
    except Exception as e:
        # A lingering async route from the previous phase can race our
        # goto ("interrupted by another navigation"); a single retry
        # after a brief settle clears it.
        if "interrupted by another navigation" in str(e):
            log.debug("goto %s interrupted; retrying once", url)
            time.sleep(1.5)
            page.goto(url, wait_until="domcontentloaded")
        else:
            raise
    if wait_selector:
        deadline = time.monotonic() + wait_timeout_s
        while time.monotonic() < deadline:
            if page.locator(wait_selector).count() > 0:
                break
            time.sleep(0.5)
    time.sleep(1.0)
    return live_url(page)


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
            final = compress_export(csv_path)
            results.append({
                "view": view_key,
                "file": str(final.relative_to(bronze_dir)),
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

# The time-period dropdown renders its Recent/Custom radios + date
# inputs only while open, so their presence is a reliable
# open-state probe.
SEL_TIMEPICKER_OPEN = "input#Custom[type='radio'], input#Recent[type='radio']"


def _ensure_timepicker_open(page):
    """Open the activity time-period dropdown unless it already is.

    Clicking the pill TOGGLES the dropdown, so a blind click on an
    already-open picker closes it — and the dropdown's contents (the
    Recent/Custom radios + Custom date inputs) exist in the DOM only
    while it's open. The bounds probe opens the picker and leaves it
    open, then the custom-range driver runs immediately after; a
    second blind pill-click there would close it and lose the Custom
    tab (observed as a spurious "Custom tab not found"). Guard on the
    open-state probe so re-entry is idempotent. Returns True when the
    dropdown ends up open."""
    if page.locator(SEL_TIMEPICKER_OPEN).count() > 0:
        return True
    pill = page.locator("[data-testid='ap143528-timeperiod-filter']")
    if pill.count() == 0:
        log.debug("timepicker pill not found")
        return False
    try:
        pill.first.evaluate(
            "el => { (el.querySelector('button') || el).click(); }"
        )
    except Exception as e:
        log.debug("timepicker pill open click: %s", e)
        return False
    time.sleep(1.0)
    return page.locator(SEL_TIMEPICKER_OPEN).count() > 0


def _click_custom_timeperiod_tab(page):
    """Select the 'Custom' tab in the activity time-period picker.

    Fidelity replaced the old ``apex-kit-segment`` web component with
    a plain PVD radio group — the Custom option is now
    ``<input class="pvd-segment__radio" type="radio" id="Custom">``.
    Try the radio id first, then a value-based radio match, then the
    legacy ``apex-kit-segment`` so a partial rollback on Fidelity's
    side doesn't break us. Returns True when a Custom control was
    found and clicked.

    The id `Custom` is generic enough to risk a collision, so the
    locators pin ``type=radio`` / the segment tag; whichever matches
    first, we click the radio itself (or the input nested in the
    legacy wrapper)."""
    for sel in (
        "input#Custom[type='radio']",
        "input[type='radio'][value='Custom']",
        "apex-kit-segment[pvd-value='Custom']",
    ):
        loc = page.locator(sel)
        if loc.count() == 0:
            continue
        try:
            loc.first.evaluate(
                "el => { "
                "  const inp = el.matches('input') "
                "    ? el : el.querySelector('input[type=radio]'); "
                "  (inp || el).click(); "
                "}"
            )
            return True
        except Exception as e:
            log.debug("custom time-period tab click via %r: %s", sel, e)
    return False


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
    if not _ensure_timepicker_open(page):
        log.debug("timepicker pill not found; bounds probe aborted")
        return None, None
    time.sleep(0.8)
    if not _click_custom_timeperiod_tab(page):
        log.debug("Custom tab not found; bounds probe aborted")
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
    # Open the dropdown idempotently — the bounds probe ran just
    # before us and left it open; a blind pill-click here would
    # toggle it shut and lose the Custom tab.
    if not _ensure_timepicker_open(page):
        log.debug("page-level timepicker pill not found")
        return None

    # Switch to the Custom tab (PVD radio group; legacy
    # apex-kit-segment as fallback — see _click_custom_timeperiod_tab).
    if not _click_custom_timeperiod_tab(page):
        log.warning("Custom tab not found in timepicker; aborting")
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
        final = compress_export(csv_path)
        return {
            "window": [since_date.isoformat(),
                       until_date.isoformat()],
            "file": str(final.relative_to(bronze_dir)),
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
        final = compress_export(csv_path)
        results.append({
            "timeperiod": selected_range or "default",
            "file": str(final.relative_to(bronze_dir)),
            "ok": True,
        })
    except Exception as e:
        log.exception("activity download failed")
        results.append({"ok": False, "error": str(e)})
    return results


# ---------------------------------------------------------------------------
# Documents — Statements + Tax forms
# ---------------------------------------------------------------------------


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


# ---------------------------------------------------------------------------
# Enterprise Document Center (digitalservices.fidelity.com/navigate/
# ent-documentcenter) — statements + tax forms.
#
# Fidelity migrated the document center off the portfolio host onto a
# Stencil/PVD component SPA. Statements and Tax forms share one shape:
#   * a left-rail nav link picks the document TYPE (relative hrefs:
#     'statements' = Personal statements, 'tax-forms'); the default
#     landing is an empty "Interested party statements". The Stencil
#     <select> ignores programmatic value writes, so the type is
#     switched by clicking the rail link, not by driving the select.
#   * a `#options-select-TimeFilter` <select> + Apply set the year.
#   * each document is an <ent-ds-link> whose text ends in "(pdf)";
#     clicking it opens the PDF in a popup tab (no direct href), which
#     we re-fetch via context.request (cookie-authenticated) — the
#     same mechanism the old grid used, just different row selectors.
# ---------------------------------------------------------------------------

SEL_DOCCENTER_TIMEFILTER = "#options-select-TimeFilter"
SEL_DOCCENTER_APPLY = (
    "button.pvd-button--primary:has-text('Apply'), "
    "button:has-text('Apply')"
)


def _doccenter_goto_type(page, rel_href, label, capture_dir, tag):
    """Switch the document type by clicking its left-rail nav link
    (the native <select> can't be driven programmatically — the
    Stencil component ignores value writes). Returns True on a
    successful click."""
    for sel in (f"a[href='{rel_href}']:has-text('{label}')",
                f"a[href='{rel_href}']",
                f"a:has-text('{label}')"):
        loc = page.locator(sel).first
        if loc.count() == 0:
            continue
        try:
            loc.click(timeout=5_000)
            _doccenter_settle(page)
            capture(page, capture_dir, f"doccenter-{tag}")
            return True
        except Exception as e:
            log.debug("doccenter type nav %r: %s", sel, e)
    log.warning("doccenter: %r nav link not found", label)
    return False


def _doccenter_year_options(page):
    """Concrete year options in the TimeFilter <select> (DOM order is
    most-recent-first), e.g. ['2026','2025',...]. Excludes the
    'Last N months' rolling options."""
    try:
        vals = page.eval_on_selector_all(
            SEL_DOCCENTER_TIMEFILTER + " option",
            "els => els.map(o => (o.textContent || '').trim())",
        ) or []
    except Exception as e:
        log.debug("doccenter year options: %s", e)
        return []
    return [v for v in vals if re.fullmatch(r"20\d\d", v)]


def _doccenter_set_year(page, year):
    """Select a concrete year in the TimeFilter <select> and click
    Apply. Returns True if the select accepted the value."""
    try:
        page.select_option(SEL_DOCCENTER_TIMEFILTER, label=str(year))
    except Exception as e:
        log.debug("doccenter set year %s: %s", year, e)
        return False
    time.sleep(0.7)
    loc = page.locator(SEL_DOCCENTER_APPLY).first
    if loc.count() > 0:
        try:
            loc.click(timeout=4_000)
        except Exception as e:
            log.debug("doccenter Apply: %s", e)
    _doccenter_settle(page)
    return True


def _doccenter_pdf_rows(page):
    """Locator over the clickable '(pdf)' document rows — one
    <ent-ds-link> per document. 'Portfolio summary' and other
    non-pdf entries are excluded by the text filter."""
    return page.locator("ent-ds-link").filter(
        has_text=re.compile(r"\(pdf\)\s*$", re.I))


def _write_doc_bytes(docs_dir, name, body):
    """Non-clobbering write of ``body`` under ``docs_dir``/``name``."""
    out_path = docs_dir / name
    base, _, ext = name.rpartition(".")
    k = 1
    while out_path.exists():
        out_path = docs_dir / (f"{base}__{k}.{ext}" if ext else f"{name}__{k}")
        k += 1
    out_path.write_bytes(body)
    return out_path


def _pdf_from_docapi_body(body):
    """Extract the PDF bytes from a ``financial-documents/download``
    JSON response. The endpoint returns the PDF as base64 in
    ``document.docDetail.content`` (``contentType: application/pdf``).
    The ``deflated`` flag is unreliable — some payloads are plain
    base64(PDF), so we decode, use it directly if it's already a PDF,
    and only zlib-inflate as a fallback. Returns bytes or None."""
    try:
        d = json.loads(body)
    except Exception:
        return None
    content = (d.get("document", {}) or {}).get("docDetail", {}) \
                .get("content")
    if not content:
        return None
    try:
        raw = base64.b64decode(content)
    except Exception:
        return None
    if raw[:4] == b"%PDF":
        return raw
    try:
        inflated = zlib.decompress(raw)
        if inflated[:4] == b"%PDF":
            return inflated
    except Exception:
        pass
    return None


def _doccenter_download_row(page, context, row_loc):
    """Click one '(pdf)' row and return the PDF bytes.

    The doc center is API-driven: clicking a row fires an
    authenticated POST to ``.../financial-documents/download`` that
    returns the PDF as base64-in-JSON, which the SPA then renders as
    an in-memory blob. Chasing the rendered blob is fragile (revoked
    URLs, viewer-context fetch errors), so we wait for that JSON
    response (canonical expect_response) and decode it. Returns the
    decoded PDF bytes or raises."""
    pages_before = set(context.pages)
    try:
        try:
            row_loc.scroll_into_view_if_needed(timeout=5_000)
        except Exception as e:
            log.debug("doccenter row scroll: %s", e)
        # The click fires an authenticated POST to
        # .../financial-documents/download; wait for that response via
        # the canonical expect_response (reliable, unlike reading
        # bodies inside an ad-hoc event handler), then decode it.
        with page.expect_response(
                lambda r: "financial-documents/download" in (r.url or ""),
                timeout=25_000) as resp_info:
            try:
                row_loc.click(timeout=5_000)
            except Exception as e:
                log.debug("doccenter row native click (%s); JS fallback", e)
                row_loc.evaluate("el => el.click()")
        pdf = _pdf_from_docapi_body(resp_info.value.body())
        if not pdf:
            raise RuntimeError("download response carried no decodable PDF")
        return pdf
    finally:
        try:
            context.remove_listener("response", _on_response)
        except Exception:
            pass
        # Close any popup tab the click spawned; restore the list tab
        # if the click navigated it away.
        for p in list(context.pages):
            if p not in pages_before:
                try:
                    p.close()
                except Exception:
                    pass
        cur = live_url(page)
        if cur and not cur.startswith(DOCCENTER_PREFIX):
            try:
                page.go_back(timeout=8_000)
                _doccenter_settle(page)
            except Exception:
                pass


def _doc_stem(label, tag):
    """Build a bronze filename stem from a row label. For the
    statements type the stem is forced to begin with ``Statement`` so
    the silver loader's 529 historical path (which globs
    ``Statement*.pdf``) parses the householded Investment Report —
    a combined statement can carry EDUCATION (529) account sections,
    which ``pdf_parsers.parse_statement_pdf`` extracts. Redundant
    ``Statement`` / ``(pdf)`` tokens in the label are dropped first so
    the name stays readable."""
    base = re.sub(r"\(pdf\)", "", label, flags=re.I)
    if tag == "statements":
        base = re.sub(r"statement", "", base, flags=re.I)
    base = re.sub(r"[^A-Za-z0-9]+", "_", base).strip("_")[:60]
    if tag == "statements":
        return ("Statement_" + base).strip("_") if base else "Statement"
    return base or "doc"


def _doccenter_download_visible_rows(page, context, docs_dir, capture_dir,
                                     min_year, seen_hashes, tag):
    """Download every currently-visible '(pdf)' row. Dedup is by PDF
    content hash (``seen_hashes``), NOT label: tax forms repeat one
    label across accounts (distinct documents, distinct bytes), while
    a statement reappearing under multiple year filters is the same
    bytes — so a content hash keeps the former and collapses the
    latter. Rows whose label-year is < ``min_year`` are skipped (only
    statement labels carry a year; tax-form labels don't, and are
    kept). ``tag`` selects the filename scheme (see _doc_stem).
    Returns a list of result dicts."""
    rows = _doccenter_pdf_rows(page)
    try:
        n = rows.count()
    except Exception as e:
        log.warning("doccenter: row enumeration failed: %s", e)
        return []
    log.info("doccenter: %d (pdf) row(s) visible", n)
    results = []
    downloaded = 0
    for i in range(n):
        row = rows.nth(i)
        try:
            label = re.sub(r"\s+", " ",
                           (row.inner_text(timeout=3_000) or "").strip())
        except Exception:
            label = f"row {i}"
        if min_year is not None:
            yr = _statement_label_year(label)
            if yr is not None and yr < min_year:
                continue
        if downloaded > 0:
            time.sleep(1.0)
        try:
            pdf = _doccenter_download_row(page, context, row)
        except Exception as e:
            log.warning("doccenter row %r failed: %s", label[:55], e)
            capture(page, capture_dir, f"doccenter-row{i}-failed")
            results.append({"row_label": label, "ok": False, "error": str(e)})
            continue
        downloaded += 1
        sha = hashlib.sha256(pdf).hexdigest()
        if sha in seen_hashes:
            log.debug("doccenter: row %r duplicate content; skipping write",
                      label[:55])
            continue
        seen_hashes.add(sha)
        out = _write_doc_bytes(docs_dir, _doc_stem(label, tag) + ".pdf", pdf)
        log.info("doccenter: saved %s (%d bytes) <- %r",
                 out.name, out.stat().st_size, label[:55])
        results.append({"row_label": label, "file": out.name, "ok": True})
    return results


def _doccenter_walk_type(page, context, rel_href, label, tag,
                         docs_dir, capture_dir, min_year):
    """Shared walk for one document type: switch to it, iterate the
    TimeFilter years >= min_year (newest first), and download every
    '(pdf)' row. Falls back to the default (year-less) view when the
    select exposes no concrete year options."""
    if not _doccenter_goto_type(page, rel_href, label, capture_dir, tag):
        return [{"ok": False, "error": f"{tag}-nav-not-found"}]
    years = _doccenter_year_options(page)
    if min_year is not None:
        years = [y for y in years if int(y) >= min_year]
    log.info("%s: iterating years %s", tag, years or ["(default view)"])
    seen_hashes = set()
    results = []
    for y in (years or [None]):
        if y is not None and not _doccenter_set_year(page, y):
            continue
        results.extend(_doccenter_download_visible_rows(
            page, context, docs_dir, capture_dir, min_year,
            seen_hashes, tag))
    return results


def scrape_statements(page, context, docs_dir, capture_dir,
                       min_year=None, target_days=None):
    """Walk the Enterprise Document Center's personal Statements.
    Switches to the Personal statements type (rail link -> relative
    href 'statements'), then iterates the TimeFilter years from the
    current year back to ``min_year``, downloading each '(pdf)'
    document (quarterly statements + year-end investment reports) via
    its popup PDF.

    ``target_days`` is kept for signature compatibility; the new UI
    filters by whole year, so the effective floor is ``min_year``
    (derived by walk() from ``--documents-since``)."""
    return _doccenter_walk_type(
        page, context, "statements", "Personal", "statements",
        docs_dir, capture_dir, min_year)


def scrape_tax_forms(page, context, docs_dir, capture_dir, min_year=None):
    """Walk the Enterprise Document Center's Tax forms: switch to the
    tax-forms type, iterate the TimeFilter years >= ``min_year``, and
    download each '(pdf)' form (Consolidated 1099s, one per account
    group). Same row + popup mechanism as statements."""
    return _doccenter_walk_type(
        page, context, "tax-forms", "Tax forms", "tax-forms",
        docs_dir, capture_dir, min_year)


def scrape_documents(page, context, bronze_dir, capture_dir,
                      min_year=None, target_days=None, explore=False):
    """Walk Statements + Tax forms in the document center. Each
    sub-page is exercised independently so a failure in one
    category doesn't block the other. ``context`` is needed for the
    popup-PDF-via-context.request fallback path on statements.

    ``min_year`` (forwarded to both sub-scrapers) drops rows / year
    selections older than that — see walk()'s default-derivation
    from ``--documents-since``. ``target_days`` is the requested
    documents window length, used to pick the Statements page's
    ``Time Period`` filter (narrowest exposed option that covers
    the window; ``None`` widens to the full archive).

    ``explore`` (``--explore``) additionally writes a shadow-DOM-
    and iframe-piercing element inventory at each landmark — the
    new Enterprise Document Center renders its content where an
    ordinary HTML capture can't see it."""
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
    if not (url.startswith(POST_AUTH_PREFIX)
            or url.startswith(DOCCENTER_PREFIX)):
        log.warning(
            "documents nav landed at %s — likely session-timeout "
            "redirect; aborting documents phase", url,
        )
        return {"status": "session-timeout", "landed_url": url}

    if explore:
        # Opt-in DOM inventory of the doc-center landing for future
        # UI-drift debugging — shadow- and iframe-piercing, unlike an
        # ordinary HTML capture (see dump_dom_inventory).
        dump_dom_inventory(page, capture_dir, "documents-landed")

    # Each scraper switches to its own document type via the left-rail
    # nav and iterates the year filter itself — no shared sidebar
    # navigation, so a failure in one doesn't strand the other.
    try:
        results["statements"] = scrape_statements(
            page, context, docs_dir, capture_dir,
            min_year=min_year,
            target_days=target_days,
        )
    except Exception as e:
        log.exception("statements walk failed")
        results["statements_error"] = str(e)

    try:
        results["tax_forms"] = scrape_tax_forms(
            page, context, docs_dir, capture_dir,
            min_year=min_year,
        )
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
    wizard as a follow-up; the formal PDF artefact is
    fetched on demand."""
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
    final = compress_export(out_path)
    return {
        "status": "explored-no-export",
        "file": str(final.relative_to(bronze_dir)),
    }


def scrape_performance(page, bronze_dir, capture_dir):
    """Visit the Performance page and capture a full-page snapshot
    of the rendered metrics tiles.

    Empirically confirmed (2026-05): the page has NO structured-
    data export — no CSV, no PDF, no kebab/menu Download item.
    The data surfaces as a Highcharts SVG + a column of
    collapsible info tiles (return percentages by period,
    benchmark deltas). The load input for this surface is
    ``performance/performance.html`` (persisted unconditionally
    below); downstream silver work either scrapes return
    percentages from that DOM text or accepts the gap (most return
    metrics are derivable from positions + activity time-series
    anyway). The separate ``screenshots/performance-landed`` HTML +
    PNG is an opt-in ``--debug`` diagnostic that ``prune`` deletes.

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
    final = compress_export(out_path)
    return {
        "status": "explored-no-export",
        "file": str(final.relative_to(bronze_dir)),
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
    dry_run = config.get("dry_run", "false").lower() == "true"
    debug = config.get("debug", "false").lower() == "true"
    slug = bronze.ts_slug()
    if dry_run:
        # A dry-run is a read-only walk: reach the export surfaces,
        # enumerate account_dimensions, and log the plan — but persist
        # NOTHING under the bronze dest (root CLAUDE.md §2 "export
        # nothing"). Crucially we do NOT create the run dir or write
        # run.json: even a run.json-only shell is a dump that `load`
        # would ingest, and fidelity-web's `load._load_master` reads
        # account_dimensions from run.json with no dry-run guard — so a
        # persisted dry-run manifest would silently populate silver
        # portfolios/accounts. No dir means `load` never sees one.
        bronze_dir = None
        capture_dir = None
        log.info(
            "walk: dry-run — read-only enumeration only; nothing will "
            "be written to bronze under %s", dest_root)
    else:
        bronze_dir = dest_root / slug
        bronze_dir.mkdir(parents=True, exist_ok=True)
        # Drop an "in-progress" manifest up front and overwrite it with
        # the terminal status at the end. This makes the run's state
        # legible to `prune` while the walk is still running (a crashed
        # walk leaves status="in-progress" — a non-complete dump prune
        # can reclaim once it goes quiescent), and closes the ambiguity
        # of a run dir that has no run.json at all.
        bronze.atomic_write_json(
            bronze_dir / "run.json", {"status": "in-progress"})
        # Debug captures (HTML DOM dump + PNG per landmark, plus the
        # --explore DOM inventories) are opt-in: they are never read by
        # `load`, and at every-landmark granularity they dwarf the
        # actual load inputs. `prune` deletes <run>/screenshots/
        # wholesale on that basis.
        capture_dir = (bronze_dir / "screenshots") if debug else None
        log.info("walk: bronze dir %s%s", bronze_dir,
                 " (debug captures on)" if debug else "")

    # Ensure we're on a portfolio surface where the account
    # selector renders.
    if not live_url(page).startswith(POST_AUTH_PREFIX):
        goto_and_wait(page, URL_PORTFOLIO_SUMMARY, wait_timeout_s=20)

    excluded = parse_exclude_list(config.get("exclude_accounts"))
    dimensions = enumerate_account_dimensions(page)
    # The account-selector links live in the DOM of every portfolio
    # surface (positions / balances / activity) even while the
    # dropdown is collapsed — but Fidelity's current post-login
    # landing page does NOT carry them, so a first pass there finds
    # nothing. Retry on the positions surface (waiting for the
    # selector to hydrate) before giving up; otherwise run.json ends
    # up with zero account dimensions and the silver master load is a
    # no-op (no nicknames, no portfolio grouping).
    if not dimensions:
        log.info(
            "account enumeration empty on landing page; retrying on "
            "the positions surface",
        )
        try:
            goto_and_wait(
                page, URL_POSITIONS,
                wait_selector=SEL_ACCOUNT_LINK,
                wait_timeout_s=20,
            )
            time.sleep(SPA_HYDRATE_WAIT_S)
        except Exception as e:
            log.warning(
                "positions nav for account enumeration failed: %s", e,
            )
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
    # Zero enumerated accounts means the selector-link DOM probe found
    # nothing on EITHER the landing page or the positions-surface
    # retry — almost always a Fidelity selector/testid change, not a
    # real empty login. It silently strands run.json without account
    # dimensions (the silver master load becomes a no-op), so make it
    # loud: this is the canary for the next account-selector UI drift.
    if not all_accounts:
        log.warning(
            "account enumeration found NO accounts — the "
            "account-selector DOM probe matched nothing (likely a "
            "Fidelity selector/testid change). run.json will carry no "
            "account dimensions and the silver master load will be a "
            "no-op; investigate enumerate_account_dimensions / "
            "SEL_ACCOUNT_LINK against the captured page DOM.",
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
        "snapshot_at": slug,
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
        # Log the plan (what a real run WOULD fetch: counts, scope,
        # windows) and stop. No run dir was created and no run.json is
        # written — the dry-run persists nothing under the bronze dest.
        # (run_json above is built solely to shape this plan log.)
        log.info(
            "dry-run: enumerated %d account(s), %d in scope; "
            "phases=%s activity_window=%s documents_since=%s. "
            "Skipping all artefact downloads — nothing written to "
            "bronze under %s.",
            len(all_accounts), len(in_scope), mode,
            run_json["activity_window"], documents_since.isoformat(),
            dest_root,
        )
        return

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
            explore=config.get("explore", "false").lower() == "true",
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

    # Atomic (tmp + rename) so a prune racing the finalisation never
    # reads a half-written manifest, and the in-progress marker is
    # replaced in one step.
    run_path = bronze_dir / "run.json"
    bronze.atomic_write_json(run_path, run_json)
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
    the walk phases): this is the login-/session-flow variant that
    writes to a user-supplied ``--screenshot-dir`` rather than the
    bronze dir's ``screenshots/`` subdir. Both are opt-in and cover
    disjoint phases — the walk captures via ``--debug`` into
    ``<run>/screenshots/``, the pre-walk login / MFA / ``--check`` /
    logout captures via ``--screenshot-dir`` into that host path."""
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
                "explore": "true" if args.explore else "false",
                # --explore implies debug captures: the DOM
                # inventories it exists for land in the same
                # capture dir.
                "debug": (
                    "true" if (args.debug or args.explore) else "false"
                ),
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
    p.add_argument(
        "--explore", action="store_true",
        help=("Diagnostic: at each document-center landmark, also "
              "write a shadow-DOM- and iframe-piercing element "
              "inventory (<ts>-<label>.dominv.json) for adapting "
              "scrapers to UI drift. Read-only; no extra exports. "
              "Implies --debug."),
    )
    # --- Diagnostics ---
    p.add_argument(
        "--debug", action="store_true",
        help=("Save debug captures during the walk: a full-page "
              "HTML DOM dump + PNG screenshot at each navigation "
              "landmark, under <run>/screenshots/. Off by default "
              "— captures are diagnostic-only (load never reads "
              "them) and dominate bronze disk usage when left on. "
              "`prune` deletes them."),
    )
    p.add_argument(
        "--screenshot-dir", type=Path, default=None,
        help=("If set, save HTML + PNG diagnostics for the pre-walk "
              "login / session phases (login form, MFA, --check "
              "probe, logout) into this host path. Walk-phase "
              "captures are governed by --debug instead. Never "
              "defaults under /secrets."),
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
