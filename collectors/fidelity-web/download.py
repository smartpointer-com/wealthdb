#!/usr/bin/env python3
"""
Fidelity client-web one-shot fetch: login → walk → logout → exit.

Boots Camoufox (a stealth-patched Firefox fork) against
``digital.fidelity.com``, runs the read-only login + MFA flow,
walks whichever export phases were selected, attempts a clean
logout, and exits. Bronze artefacts land in a timestamped
``<bronze-dir>/<UTC-ts>/`` directory.

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
  visible account. Then the lot step: per account, the page's own
  positions and closed-positions queries, and the lots of each new
  or changed position (``--refresh-lots`` fetches every one). See
  ``scrape_lots``.

* ``activity`` — Navigate to Activity & Orders once, drive the
  page-level timepicker's Custom tab once per window of the
  ``--lookback`` range, and for each window click the Download
  trigger and grab the CSV. The export is
  consolidated across all visible accounts — the
  ``Account Number`` column inside the CSV is the per-row
  account discriminator; clicking the account-selector before
  the export does NOT scope it.

* ``documents`` — In the document center, walk the Statements and
  the Tax forms types across every TimeFilter year from the
  ``--lookback`` start year, and fetch each '(pdf)' row's document
  through the page's own download response. Prospectuses + supplementary material are out of scope
  (DESIGN.md §8.5).

* ``balances`` — Persist ``balances.html``; silver indexes it as a
  document. Fidelity offers no direct CSV / Excel export on this
  surface.

* ``performance`` — Persist ``performance.html``. Pure Highcharts
  UI with no structured export.

When the login carries a Fidelity Charitable donor-advised fund, the
``positions`` and ``all`` modes also run the DAF phase over the
charitable API (``scrape_daf``): the pool snapshot under
``positions``, the full history, exports and documents under ``all``.

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
  --vnc        pre-fill the credentials, then HAND OFF to a VNC
               session (x11vnc, started by entrypoint.sh on a host
               port the wrapper prints), where Log In and 2FA are completed
               by hand. The script polls for the post-auth URL and
               (when no walk phases are requested) exits once it
               lands.
  (default)    full credential + CLI-MFA flow + walk + logout.

Usage:
    download.py --profile-dir /secrets/fidelity-web-profile
                [--env-file /secrets/fidelity-web.env]
                [--bronze-dir /data]
                [--mode all|positions|activity|documents|balances|performance]
                [--lookback PRESET|YYYY-MM-DD]
                [--exclude-accounts <a,b>]
                [--refresh-lots]
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
import functools
import hashlib
import json
import logging
import os
import re
import signal
import sys
import time
from collections.abc import Callable
from dataclasses import dataclass
from datetime import date, datetime, timedelta, timezone
from pathlib import Path
from urllib.parse import quote

from collectorkit import bronze, cli, compress, debugcap, envfile, launch

import lot_parsers
# The walk's landmarks and its pure rules live in the modules below.
# Each name is bound here too, so an importer of download finds it.
from landmarks import (  # noqa: F401
    URL_PORTFOLIO_SUMMARY,
    URL_POSITIONS,
    URL_ACTIVITY,
    URL_DOCUMENTS,
    URL_BALANCES,
    URL_PERFORMANCE,
    POST_AUTH_PREFIX,
    DOCCENTER_PREFIX,
    SEL_ALL_ACCOUNTS,
    SEL_KEBAB_MENU,
    SEL_ACCOUNT_LINK,
    SEL_PRESET_VIEW_SELECT,
    KEBAB_OPEN_WAIT_S,
    TABLE_RENDER_WAIT_S,
    SPA_HYDRATE_WAIT_S,
    DOWNLOAD_TIMEOUT_MS,
    MAX_ACTIVITY_WINDOW_DAYS,
    ACTIVITY_RETENTION_FLOOR_DAYS,
    ACTIVITY_RANGE_REFUSED,
    ACTIVITY_OUTSIDE_RETENTION,
    ACTIVITY_EXPORT_ATTEMPTS,
    ACTIVITY_EXPORT_RETRY_SECONDS,
    LANDMARK_TIMEOUT_MS,
    USERNAME_ENVS,
    PASSWORD_ENVS,
    DEFAULT_ENV_FILE_CANDIDATES,
    SIGNIN_URL,
    SEL_USERNAME_SELECT,
    SEL_USERNAME_OTHER_OPTION_VALUE,
    SEL_USERNAME_TEXT_INPUT_CANDIDATES,
    SEL_PASSWORD_INPUT,
    SEL_LOGIN_BUTTON,
    SEL_IUA_ACCEPT_CANDIDATES,
    SEL_2FA_CODE_INPUT,
    SEL_2FA_SUBMIT_CANDIDATES,
    SEL_TRUST_BROWSER_CHECKBOX,
    SEL_TRUST_BROWSER_LABEL,
    SEL_LOGOUT_CANDIDATES,
    account_key,
    parse_exclude_list,
    POSWEB_API,
    POSWEB_READ_ENDPOINTS,
    LOTS_PAGE_SIZE,
    LOT_REQUEST_PAUSE_S,
    LOT_LONG_PAUSE_EVERY,
    LOT_LONG_PAUSE_S,
    LOT_MAX_FETCHES_PER_RUN,
    LOT_CONTEXT_TIMEOUT_S,
    LOT_MAX_CONSECUTIVE_FAILURES,
    SEL_TIMEPICKER_PILL,
    SEL_TIMEPICKER_OPEN,
    SEL_ACTIVITY_FROM_DATE,
    SEL_ACTIVITY_TO_DATE,
    SEL_ACTIVITY_APPLY,
    SEL_ACTIVITY_CSV_SELECTORS,
    SEL_ACTIVITY_CSV_ITEM,
    SEL_TIMEPICKER_LABEL,
    SEL_DOCCENTER_TIMEFILTER,
    SEL_DOCCENTER_APPLY,
    DAF_HOST,
    DAF_ORIGIN,
    DAF_SSO_URL,
    DAF_SPA_URL,
    DAF_API,
    DAF_IDENTITY_URL,
    DAF_JWT_HEADER,
    DAF_DOCUMENT_TYPES,
    DAF_PAGE_SIZE,
    _daf_ep,
)
from windows import (  # noqa: F401
    make_activity_windows,
    mark_outside_retention,
    clamp_activity_window,
    activity_csv_coverage,
    activity_export_matches,
)
from phase_coverage import (  # noqa: F401
    EXIT_PHASE_INCOMPLETE,
    ENTRY_FAIL_STATUS,
    phase_coverage,
    exit_code_for_coverage,
)
from lot_queries import (  # noqa: F401
    lot_position_rows,
    lot_position_id,
    open_lot_signature,
    closed_lot_signature,
    _row_symbol,
    closedpositions_body,
    closedlots_body,
    openlots_body,
    openlots_last_page,
    decode_pico,
    lot_ineligible_accounts,
    previous_lot_index,
    write_lot_index,
    LotFetchError,
    _LotBundle,
    LotStepAborted,
    _lot_pause,
)
from documents import (  # noqa: F401
    _statement_label_year,
    _write_doc_bytes,
    _pdf_from_docapi_body,
    _is_docapi_download_response,
    _doc_stem,
)
from daf import (  # noqa: F401
    _login_has_daf,
    _daf_doc_stem,
    _daf_year_span,
    _daf_parse_establish_date,
)


log = logging.getLogger("fidelity-web.download")


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


# The page's URL as the browser shows it. Bound here so a test can
# replace it for this module alone.
live_url = launch.live_url


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
    # balance / gains-losses on one line. Only the nickname prefix
    # is wanted — split on the account-id digit sequence (already
    # known from the testid) and trim. Falls
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


def parse_iso_date(value):
    """Parse YYYY-MM-DD; return ``date``. ``None`` if value is
    empty / unset."""
    if not value:
        return None
    return datetime.strptime(value, "%Y-%m-%d").date()


def capture(page, capture_dir, label):
    """Save HTML + PNG of the current page state into `capture_dir`
    (None captures nothing), the credentials and the session masked out
    of the markup. Never raises.

    The walk phases pass ``<run>/screenshots/`` (under ``--debug``); the
    sign-in, MFA, ``--check`` and logout landmarks pass the
    ``--screenshot-dir`` host path. The screenshot is capped at 5 s.
    """
    debugcap.landmark_capturer(
        capture_dir, user_env=USERNAME_ENVS, pass_env=PASSWORD_ENVS,
        session=True, log=log, png_timeout_ms=5_000)(page, label)


def dump_dom_inventory(page, capture_dir, label):
    """Exploration diagnostic: write a flat inventory of every
    identifiable element — piercing open shadow roots and
    same-origin iframes — to ``<ts>-<label>.dominv.json``.

    page.content() only serialises the light DOM of the top
    document, so SPAs that render lists inside shadow DOM or nested
    iframes (e.g. Fidelity's Enterprise Document Center) come
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
        # A lingering async route from the previous phase can race this
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
            final = compress.compress_best_effort(csv_path, log)
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
# Open and closed lots — the positions page's own read-only lot queries
# ---------------------------------------------------------------------------
#
# The positions page fetches its table and each position's lot table
# from a JSON API under POSWEB_API. Every call is a POST, but each one
# in POSWEB_READ_ENDPOINTS is a read-only query the page itself sends
# when a table is opened (AGENTS.md §1). The queries run as `fetch`
# inside the page, so they leave through the browser's own network
# stack with its TLS fingerprint and bot-manager cookies, like the
# page's own; Playwright's request API would send them from a different
# client, which Akamai can tell apart. Nothing here builds a request
# the page does not: the session's CSRF token and its account context
# (`pico`) are copied from the page's own positions request, and every
# body mirrors the page's.


class _LotFailures:
    """Counts consecutive lot-request failures; ``record`` raises
    LotStepAborted once LOT_MAX_CONSECUTIVE_FAILURES are reached. Also
    holds what is left of the run's LOT_MAX_FETCHES_PER_RUN."""

    def __init__(self):
        self.run = 0
        self.fetches_left = LOT_MAX_FETCHES_PER_RUN

    def record(self, ok):
        self.run = 0 if ok else self.run + 1
        if self.run >= LOT_MAX_CONSECUTIVE_FAILURES:
            raise LotStepAborted(
                f"{self.run} lot requests failed in a row")


def _capture_positions_context(page):
    """Load the positions page and copy what its own positions request
    carries: the CSRF header, the preset-view header and the JSON body
    (with its `pico` account context). Returns None when the page sends
    no such request in time."""
    seen = {}

    def on_request(request):
        if (request.method == "POST"
                and request.url.split("?", 1)[0] == f"{POSWEB_API}/positions"
                and request.post_data and "body" not in seen):
            try:
                seen["body"] = json.loads(request.post_data)
            except ValueError:
                return
            seen["request"] = request

    page.on("request", on_request)
    try:
        goto_and_wait(page, URL_POSITIONS, wait_selector=SEL_KEBAB_MENU)
        deadline = time.monotonic() + LOT_CONTEXT_TIMEOUT_S
        while "body" not in seen and time.monotonic() < deadline:
            page.wait_for_timeout(500)
    finally:
        page.remove_listener("request", on_request)
    if "body" not in seen:
        return None
    # Read outside the event handler: all_headers() is a round trip to
    # the browser, and it is the call that also returns the headers the
    # browser added.
    try:
        headers = seen["request"].all_headers()
    except Exception:  # noqa: BLE001
        headers = seen["request"].headers
    return {
        "body": seen["body"],
        "csrf": headers.get("x-csrf-token"),
        "preset_view": headers.get("preset-view"),
    }


# Runs in the page: the browser adds the origin, referer and cookies
# itself, as for the page's own queries.
_IN_PAGE_POST_JS = """async ({url, headers, body}) => {
    const r = await fetch(url, {method: "POST", headers, body,
                                credentials: "include"});
    return {status: r.status, text: await r.text()};
}"""


def _poswebex_post(page, ctx, endpoint, body):
    """One read-only poswebex query, sent by the page itself with
    `fetch`, paced. The page must be on the positions page, the origin
    the query belongs to. Returns the response text; raises
    LotFetchError on a failed fetch or a non-2xx status."""
    if endpoint not in POSWEB_READ_ENDPOINTS:
        raise ValueError(f"poswebex endpoint not allow-listed: {endpoint}")
    headers = {
        "content-type": "application/json",
        "accept": "application/json, text/html",
    }
    if ctx.get("csrf"):
        headers["x-csrf-token"] = ctx["csrf"]
    if endpoint == "positions" and ctx.get("preset_view"):
        headers["preset-view"] = ctx["preset_view"]
    _lot_pause()
    try:
        resp = page.evaluate(_IN_PAGE_POST_JS, {
            "url": f"{POSWEB_API}/{endpoint}", "headers": headers,
            "body": json.dumps(body)})
    except Exception as e:  # noqa: BLE001
        raise LotFetchError(f"{endpoint}: {e}") from e
    if not 200 <= resp["status"] < 300:
        raise LotFetchError(f"{endpoint}: HTTP {resp['status']}")
    return resp["text"]


def _fetch_account_json(page, ctx, endpoint, body):
    """One JSON query: ``(parsed, text)``."""
    text = _poswebex_post(page, ctx, endpoint, body)
    try:
        return json.loads(text), text
    except ValueError as e:
        raise LotFetchError(f"{endpoint}: body not JSON: {e}") from e


def _fetch_open_lots(page, ctx, account, row, bundle, key):
    """Every page of one position's lot table, into ``bundle``. Returns
    the record ids."""
    pid = lot_position_id(row)
    records = []
    page_num, last = 1, 1
    while page_num <= last:
        html = _poswebex_post(page, ctx, "openlots",
                              openlots_body(account, row, page_num))
        stated = openlots_last_page(html)
        if stated is None:
            raise LotFetchError("openlots: response is not a lot table")
        last = stated if page_num == 1 else last
        records.append(bundle.add("openlots", html, account=key,
                                  position=pid, page=page_num))
        page_num += 1
    log.info("  open lots of %s: %d page(s)", pid, len(records))
    return records


def _fetch_closed_lots(page, ctx, account, row, tax_year, bundle, key):
    """One closed position's lots, into ``bundle``. Returns the record
    id in a list, like ``_fetch_open_lots``."""
    pid = lot_position_id(row)
    lots, text = _fetch_account_json(
        page, ctx, "closedlots", closedlots_body(ctx, account, pid, tax_year))
    if not isinstance(lots, list):
        raise LotFetchError("closedlots: response is not a lot list")
    log.info("  closed lots of %s (%d): %d lot(s)", pid, tax_year, len(lots))
    return [bundle.add("closedlots", text, account=key, position=pid,
                       tax_year=tax_year)]


def _keep_or_fetch(item, sig, prev, fetch, counts, entry, failures, slug):
    """Point ``item`` at the fetch ``prev`` came from when its
    signature is unchanged, else call ``fetch`` for new records. A failed
    fetch, or one past the run's fetch budget, leaves ``item`` without
    `fetched_in`, so the next run tries again."""
    if prev and prev.get("fetched_in") and all(
            prev.get(k) == v for k, v in sig.items()):
        item.update(fetched_in=prev["fetched_in"],
                    records=prev.get("records") or [])
        counts["kept"] += 1
        return
    if failures.fetches_left <= 0:
        counts["deferred"] += 1
        return
    failures.fetches_left -= 1
    try:
        item["records"] = fetch()
    except LotFetchError as e:
        log.warning("lots: a position in account %s failed: %s",
                    entry["account"], e)
        entry["failed"].append(item["position"])
        entry["status"] = "error"
        failures.record(False)
        return
    item["fetched_in"] = slug
    counts["fetched"] += 1
    failures.record(True)


def scrape_lots(page, bronze_dir, accounts, prev_index, *, refresh=False,
                tax_years=None):
    """Fetch the open lots of every new or changed position, and the
    lots of every new or changed closed position of ``tax_years``
    (default: the previous and the current year).

    The previous year stays in so that a sale after a year's last run,
    or a basis the broker corrects after year end, still arrives;
    earlier years come from the Consolidated 1099s (DESIGN.md §4.6). Per
    account, one positions query lists its positions and one
    closedpositions query per tax year its closed positions. A
    position's lots are fetched when it is new, when its signature
    (quantity and cost basis total; for a closed position, proceeds,
    cost basis and gain) differs from the fetch its lots came from, or
    always with ``refresh``. The index (`lots/index.json`) records every
    lot-eligible position with the dump its lots were fetched in and
    their record ids in that dump's bundle (`lots/lots.jsonl`), both
    compressed, so an unchanged position points at an earlier dump and
    costs no request.

    Returns the phase result: an envelope status and one entry per
    account. A failed account or position is recorded and the walk
    goes on; the index still lists the position, without a fetch, so
    the next run fetches it. Past LOT_MAX_FETCHES_PER_RUN, new and
    changed positions are deferred the same way: a full refresh takes
    several runs, and the step still counts as complete. After
    LOT_MAX_CONSECUTIVE_FAILURES in a row the step stops; accounts it
    did not reach are missing from the index, so the next run fetches
    them in full."""
    lots_dir = bronze_dir / "lots"
    lots_dir.mkdir(parents=True, exist_ok=True)
    slug = bronze_dir.name
    if tax_years is None:
        this_year = datetime.now(timezone.utc).year
        tax_years = (this_year - 1, this_year)
    result = {"status": "complete", "refresh": refresh,
              "tax_years": list(tax_years), "accounts": {}}
    ctx = _capture_positions_context(page)
    if ctx is None:
        log.warning("lots: the positions page sent no positions query; "
                    "skipping the lot step")
        return {"status": "error", "refresh": refresh,
                "error": "no positions query to copy the session from"}
    ineligible = lot_ineligible_accounts(decode_pico(ctx["body"].get("pico")))
    prev_open, prev_closed = {}, {}
    if prev_index and not refresh:
        prev_open = {(e.get("account"), e.get("position")): e
                     for e in prev_index.get("open") or []}
        prev_closed = {(e.get("account"), e.get("tax_year"),
                        e.get("position")): e
                       for e in prev_index.get("closed") or []}
    index = {"snapshot_at": slug, "refresh": refresh,
             "tax_years": list(tax_years), "open": [], "closed": []}
    failures = _LotFailures()
    bundle = _LotBundle(lots_dir / lot_parsers.LOTS_BUNDLE)
    try:
        for account in accounts:
            if account in ineligible:
                continue
            key = account_key(account)
            entry = {"account": key, "status": "ok", "failed": [],
                     "open": {"fetched": 0, "kept": 0, "deferred": 0},
                     "closed": {"fetched": 0, "kept": 0, "deferred": 0}}
            result["accounts"][key] = entry
            try:
                positions, text = _fetch_account_json(
                    page, ctx, "positions",
                    dict(ctx["body"], accts=[account], isRefresh=False))
                bundle.add("positions", text, account=key)
                closed = []
                for year in tax_years:
                    answer, text = _fetch_account_json(
                        page, ctx, "closedpositions",
                        closedpositions_body(ctx, account, year, ineligible))
                    bundle.add("closedpositions", text, account=key,
                               tax_year=year)
                    closed += [(year, row) for row in lot_position_rows(answer)]
                failures.record(True)
            except LotFetchError as e:
                log.warning("lots: positions of account %s failed: %s",
                            key, e)
                entry.update(status="error", error=str(e))
                failures.record(False)
                continue
            for row in lot_position_rows(positions):
                pid = lot_position_id(row)
                sig = open_lot_signature(row)
                item = {"account": key, "position": pid,
                        **_row_symbol(row), **sig}
                index["open"].append(item)
                _keep_or_fetch(
                    item, sig, prev_open.get((key, pid)),
                    functools.partial(
                        _fetch_open_lots, page, ctx, account, row, bundle,
                        key),
                    entry["open"], entry, failures, slug)
            for year, row in closed:
                pid = lot_position_id(row)
                sig = closed_lot_signature(row)
                item = {"account": key, "position": pid,
                        "tax_year": year, **_row_symbol(row), **sig}
                index["closed"].append(item)
                _keep_or_fetch(
                    item, sig, prev_closed.get((key, year, pid)),
                    functools.partial(
                        _fetch_closed_lots, page, ctx, account, row, year,
                        bundle, key),
                    entry["closed"], entry, failures, slug)
            log.info("lots: account %s open fetched=%d kept=%d deferred=%d, "
                     "closed fetched=%d kept=%d deferred=%d, failed=%d", key,
                     entry["open"]["fetched"], entry["open"]["kept"],
                     entry["open"]["deferred"], entry["closed"]["fetched"],
                     entry["closed"]["kept"], entry["closed"]["deferred"],
                     len(entry["failed"]))
    except LotStepAborted as e:
        log.warning("lots: step stopped early: %s", e)
        result["error"] = str(e)
    finally:
        if result.get("error") or any(
                a["status"] != "ok" for a in result["accounts"].values()):
            result["status"] = "partial"
        index["status"] = result["status"]
        bundle.close()
        write_lot_index(lots_dir, index)
    return result


# ---------------------------------------------------------------------------
# Activity & Orders — one consolidated CSV per date-window
# ---------------------------------------------------------------------------


def _activity_filter_shows_range(page, since_date, until_date):
    """True when the time-filter control's label names both endpoints
    of the requested range.

    The label renders the dates in the session's own locale, and the
    site is inconsistent about it: day-first where its CSV is
    month-first, and dot-separated on one render where the same
    control was slash-separated on another. So the separator is
    normalised away and BOTH field orders are accepted, rather than
    parsing one spelling and trusting it — a matcher that is strict
    about presentation reports a filter that took as one that did
    not, which costs the window. A label naming neither rendering of
    an endpoint is a filter that really did not take."""
    try:
        el = page.locator(SEL_TIMEPICKER_LABEL).first
        if el.count() == 0:
            return None          # not this generation of the control
        label = el.evaluate(
            "el => el.getAttribute('fds-native-button-attributes') "
            "  || el.getAttribute('aria-label') || el.textContent || ''"
        ) or ""
    except Exception as e:
        log.debug("time-filter label read failed: %s", e)
        return None
    flat = re.sub(r"[.\-]", "/", label)
    for dt in (since_date, until_date):
        if not any(dt.strftime(f) in flat
                   for f in ("%d/%m/%Y", "%m/%d/%Y")):
            log.debug(
                "time-filter label does not name %s", dt.isoformat(),
            )
            return False
    return True


def _ensure_timepicker_open(page):
    """Open the activity time-period dropdown unless it already is.

    Clicking the pill TOGGLES the dropdown, so a blind click on an
    already-open picker closes it — and the dropdown's contents (the
    Recent/Custom radios + Custom date inputs) exist in the DOM only
    while it's open. The bounds probe opens the picker and leaves it
    open, then the custom-range driver runs immediately after; a
    second blind pill-click there would close it and lose the Custom
    tab (it reads as a spurious "Custom tab not found"). Guard on the
    open-state probe so re-entry is idempotent. Returns True when the
    dropdown ends up open."""
    if page.locator(SEL_TIMEPICKER_OPEN).count() > 0:
        return True
    pill = page.locator(SEL_TIMEPICKER_PILL)
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

    Three generations of this control have shipped, and the locators
    below cover all of them so a partial rollback on Fidelity's side
    does not break the walk. Returns True when a Custom control was found
    and clicked.

    The current one (`fds-*` design system) is a segmented
    control of ``<input class="fds-segment__radio" type="radio"
    value="custom">``. Its ids are GENERATED PER RENDER
    (``segment-444046273391``), so nothing may key on one; the value
    is the only stable handle. It is matched case-insensitively
    (``[value='custom' i]``) because the generation before it spelled
    the same value ``Custom`` — one locator covers both, and the next
    flip of that shift key costs nothing.

    Before that came a PVD radio group keyed on ``id="Custom"``, and
    before that an ``apex-kit-segment`` web component. Both are kept
    below. Whichever matches first, the radio itself (or the input
    nested in the legacy wrapper) is clicked."""
    for sel in (
        "input.fds-segment__radio[type='radio'][value='custom' i]",
        "input[type='radio'][value='custom' i]",
        "input#Custom[type='radio']",
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
    chunker can clamp the requested ``--lookback`` window to
    Fidelity's available retention window. Without the clamp,
    ``--lookback all`` walks one refused window per month from the
    requested start up to the retention floor.
    """
    if not _ensure_timepicker_open(page):
        log.debug("timepicker pill not found; bounds probe aborted")
        return None, None
    time.sleep(0.8)
    capture(page, capture_dir, "activity-timepicker-open")
    if not _click_custom_timeperiod_tab(page):
        log.debug("Custom tab not found; bounds probe aborted")
        return None, None
    time.sleep(0.8)
    capture(page, capture_dir, "activity-custom-bounds-probe")
    from_input = page.locator(SEL_ACTIVITY_FROM_DATE).first
    to_input = page.locator(SEL_ACTIVITY_TO_DATE).first
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
    with the given dates, click the panel's Apply button,
    wait for the table re-fetch.

    Returns the range tag (``"<since>__<until>"``) on success or
    ``None`` on failure. All clicks dispatch via JS to bypass
    Camoufox's actionability quirks (the picker contents are
    marked 'outside viewport' even when on-screen).

    NOTE: callers MUST chunk longer requested windows via
    ``make_activity_windows`` before invoking this function.
    Fidelity caps a Custom range at 93 days, but the chunk width is
    MAX_ACTIVITY_WINDOW_DAYS and is narrower than that for a
    correctness reason — see its comment.
    """
    # Open the dropdown idempotently — the bounds probe ran just
    # before this and left it open; a blind pill-click here would
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
    # "date">`` and want ISO YYYY-MM-DD. Where the input carries
    # min/max attributes bounding the per-export retention window,
    # the dates are clamped to them, so an over-eager since= doesn't
    # silently truncate; the current generation sets neither, in which case
    # there is nothing to clamp to and the panel's own validation is
    # what refuses an out-of-range request.
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

    from_input = page.locator(SEL_ACTIVITY_FROM_DATE).first
    to_input = page.locator(SEL_ACTIVITY_TO_DATE).first
    if from_input.count() == 0 or to_input.count() == 0:
        capture(
            page, capture_dir,
            f"activity-custom-inputs-missing-{label_suffix}",
        )
        log.warning(
            "Custom-tab date inputs not found "
            f"({SEL_ACTIVITY_FROM_DATE} / {SEL_ACTIVITY_TO_DATE})"
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
    # the window is outside Fidelity's retention — fetching would return
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

    apply_btn = page.locator(SEL_ACTIVITY_APPLY)
    if apply_btn.count() == 0:
        log.warning(
            "no Apply button for the Custom range (%s)", SEL_ACTIVITY_APPLY
        )
        return None
    # Apply, and wait for the DATA REQUEST it triggers rather than for
    # the control to look right.
    #
    # The export is generated from whatever the table currently holds,
    # and the filter's label updates the moment Apply is pressed —
    # long before the rows behind it arrive. Downloading on the label
    # alone exports the PREVIOUS range under this window's name: for a
    # wide window the fetch is slow enough that the file lands, the
    # run reports ok, and the rows are quietly the last 30 days.
    #
    # The predicate is any XHR/fetch rather than a named endpoint, so
    # it survives the SPA moving its data URL — which is the kind of
    # change that broke every selector above.
    try:
        with page.expect_response(
            lambda r: r.request.resource_type in ("xhr", "fetch"),
            timeout=20_000,
        ):
            apply_btn.first.evaluate("el => el.click()")
        log.debug("Custom-range Apply triggered a data request")
    except Exception as e:
        # A timeout here is not fatal on its own — the range may
        # already be the one loaded, and the label check below still
        # has to agree before anything is exported. Anything else is
        # a click that did not land.
        if "Timeout" not in type(e).__name__:
            log.warning("apply Custom click failed: %s", e)
            return None
        log.debug("Custom-range Apply triggered no data request")

    try:
        page.wait_for_load_state("networkidle", timeout=20_000)
    except Exception:
        time.sleep(8.0)

    # Wait for the page to SAY it is showing the range asked for.
    # `networkidle` cannot answer that — prior unrelated requests may
    # already have gone idle while the new range fetch has not
    # started — and downloading a window early exports the PREVIOUS
    # filter's rows under this window's name. That is worse than
    # failing: the file lands, the run reports ok, and the rows are
    # silently the wrong ones. A control that does not carry a label
    # (an older generation) returns None and is trusted.
    deadline = time.monotonic() + 20.0
    shown = None
    while time.monotonic() < deadline:
        shown = _activity_filter_shows_range(page, since_date, until_date)
        if shown is not False:
            break
        time.sleep(0.5)
    capture(
        page, capture_dir,
        f"activity-custom-applied-{label_suffix}",
    )
    if shown is False:
        log.warning(
            "activity Custom range %s..%s did not take — the filter "
            "still names a different range; refusing to export this "
            "window under a range it is not showing",
            since_date.isoformat(), until_date.isoformat(),
        )
        return None
    range_tag = (
        f"{since_date.strftime('%Y%m%d')}"
        f"__{until_date.strftime('%Y%m%d')}"
    )
    log.info(
        "activity Custom range set: %s..%s",
        since_date.isoformat(), until_date.isoformat(),
    )
    return range_tag


def _click_activity_download(page, capture_dir, label_suffix):
    """Activity's Download is a two-step popover: the visible button
    (``aria-label='Download'`` with ``aria-controls='downloadContent'``)
    opens an in-page menu that contains the CSV download button.
    Click trigger → click CSV → intercept download.

    The time-period for the export is whatever the **page-level**
    timepicker filter has selected at the time of the click — that
    has to be set BEFORE this function (via
    ``_select_activity_custom_range``): the popover's own timepicker
    shares the page-level dropdown, and a click in that dropdown
    counts as outside the popover and dismisses it."""
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
    # button briefly disabled at the click. Poll for the
    # disabled state to clear before clicking.
    #
    # The state is read from BOTH generations of the control: the
    # native `disabled` on the button itself, and `fds-disabled` on
    # the custom element that wraps it. Reading only the native
    # one reports "enabled" for a busy button, because the custom
    # element never sets the native attribute.
    deadline = time.monotonic() + 20.0
    while time.monotonic() < deadline:
        try:
            disabled = trigger.evaluate(
                "el => { const host = el.closest('fds-button') || el; "
                "  return el.disabled === true "
                "    || el.getAttribute('disabled') !== null "
                "    || host.getAttribute('fds-disabled') === 'true'; }"
            )
        except Exception:
            disabled = False
        if not disabled:
            break
        time.sleep(0.5)

    # Opening the popover is what puts the CSV item in the DOM, so
    # wait for the ITEM rather than for a constant: a wide window
    # keeps the table re-fetching well past any sleep, and the click
    # that lands too early opens nothing. A popover that never opened
    # would read as "no CSV item" — the same message as a renamed
    # item — so the two are told apart here and the trigger gets one
    # more press before giving up.
    for attempt in (1, 2):
        trigger.click(timeout=10_000)
        opened_by = time.monotonic() + 15.0
        while time.monotonic() < opened_by:
            if page.locator(SEL_ACTIVITY_CSV_ITEM).count() > 0:
                break
            time.sleep(0.4)
        if page.locator(SEL_ACTIVITY_CSV_ITEM).count() > 0:
            break
        log.debug(
            "activity Download popover did not open on attempt %d", attempt,
        )
    capture(
        page, capture_dir, f"activity-download-open-{label_suffix}",
    )

    # The popover's CSV item, current generation first.
    #
    # It is an `fds-button` custom element whose native <button> lives
    # in a SHADOW ROOT, and `#download-csv-button` is that element's
    # own light-DOM id — the one stable handle on this control, since
    # every id the shadow tree generates is per-render. The click is
    # dispatched at the native button THROUGH the shadow root: an
    # actionability click reads the item as hidden while the popover
    # animates, and a plain CSS match for the inner button can miss a
    # shadow root altogether. That combination is what made the
    # previous generation of this loop fail without a word.
    #
    # The generation before was a link inside `#downloadContent`, kept
    # below. Every skip is logged, so the next drift names the locator
    # that stopped matching instead of failing silently.
    for sel in (
        *SEL_ACTIVITY_CSV_SELECTORS,
        "#downloadContent button:has-text('CSV')",
        "#downloadContent a:has-text('CSV')",
        "#downloadContent [role=menuitem]:has-text('CSV')",
        "a:has-text('Download as CSV')",
        "button:has-text('Download as CSV')",
    ):
        try:
            loc = page.locator(sel).first
            if loc.count() == 0:
                log.debug("activity CSV item %r: no match", sel)
                continue
            log.info(
                "activity CSV item via %r (%s)",
                sel, label_suffix,
            )
            with page.expect_download(
                timeout=DOWNLOAD_TIMEOUT_MS,
            ) as dl_info:
                loc.evaluate(
                    "el => { const b = (el.shadowRoot "
                    "  && el.shadowRoot.querySelector('button')) "
                    "  || el.querySelector('button') || el; "
                    "  b.click(); }"
                )
            return dl_info.value
        except Exception as e:
            log.debug("activity CSV item %r: %s", sel, e)
    capture(
        page, capture_dir, f"activity-csv-no-match-{label_suffix}",
    )
    raise RuntimeError("no CSV item in activity Download popover")


def _activity_csv_for_window(page, since_date, until_date,
                              bronze_dir, capture_dir):
    """Drive the Custom-range tab to [since..until] (a single
    window), open the Download popover, save the CSV.
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
            "error": ACTIVITY_RANGE_REFUSED,
        }
    try:
        # Download, then check the FILE against the window it is
        # being saved as.
        #
        # The export is generated from the table, and the table lags
        # the filter by a whole apply: a freshly-applied range hands
        # back the PREVIOUS one's rows, exactly and repeatably. Nothing
        # on the page reports that — the filter's label, `networkidle`
        # and the request Apply fires are all satisfied while the old
        # rows are still loaded — so the only honest test is the
        # content itself. An export whose rows fall outside the window
        # it was asked for is stale; wait for the table and pull it
        # again.
        csv_path = activity_dir / f"activity_{range_tag}.csv"
        rows = 0
        first = last = None
        for attempt in range(1, ACTIVITY_EXPORT_ATTEMPTS + 1):
            dl = _click_activity_download(
                page, capture_dir, f"{win_suffix}-try{attempt}",
            )
            dl.save_as(str(csv_path))
            rows, first, last = activity_csv_coverage(csv_path)
            if activity_export_matches(
                    rows, first, last, since_date, until_date):
                break
            log.warning(
                "activity export for %s..%s came back holding %s..%s "
                "— the table had not caught up with the filter; "
                "retrying (attempt %d of %d)",
                since_date.isoformat(), until_date.isoformat(),
                first.isoformat(), last.isoformat(),
                attempt, ACTIVITY_EXPORT_ATTEMPTS,
            )
            time.sleep(ACTIVITY_EXPORT_RETRY_SECONDS)
        stale = not activity_export_matches(
            rows, first, last, since_date, until_date)
        log.info(
            "saved activity/%s (%d bytes, range=%s, rows=%d, covers %s..%s)",
            csv_path.name, csv_path.stat().st_size, range_tag, rows,
            first.isoformat() if first else "-",
            last.isoformat() if last else "-",
        )
        final = compress.compress_best_effort(csv_path, log)
        result = {
            "window": [since_date.isoformat(),
                       until_date.isoformat()],
            "rows": rows,
            "covers": [first.isoformat() if first else None,
                       last.isoformat() if last else None],
            "file": str(final.relative_to(bronze_dir)),
            "ok": not stale,
        }
        if stale:
            # The rows are real and they load — an activity id is
            # derived from its content — but this window is NOT
            # covered, and saying otherwise is how a hole hides.
            result["error"] = (
                f"export held {first.isoformat()}..{last.isoformat()}, "
                "outside the requested window"
            )
            log.error(
                "activity window %s..%s never came back with its own "
                "rows after %d attempts; recording it as a gap",
                since_date.isoformat(), until_date.isoformat(),
                ACTIVITY_EXPORT_ATTEMPTS,
            )
        return result
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
    discriminated by the ``Account Number`` column. So the page is
    driven once per date-window, not per-account.

    The resolved ``--lookback`` window [since_date..until_date] is
    bisected into ≤``MAX_ACTIVITY_WINDOW_DAYS``-day windows via
    ``make_activity_windows``, and the Custom timepicker tab is
    driven for each — one CSV per window. Bounded by whatever
    the Custom tab publishes in its date inputs' ``min``/``max``,
    and where it publishes neither — which the current generation of
    the panel does not — by ``ACTIVITY_RETENTION_FLOOR_DAYS``."""
    activity_dir = bronze_dir / "activity"
    activity_dir.mkdir(parents=True, exist_ok=True)
    results = []

    try:
        goto_and_wait(page, URL_ACTIVITY, wait_selector=SEL_KEBAB_MENU)
        capture(page, capture_dir, "activity-landed")
    except Exception as e:
        log.exception("activity-nav failed")
        return [{"ok": False, "error": f"navigate: {e}"}]

    # Pre-flight: clamp the requested range to the retention the
    # Custom tab publishes, or to ACTIVITY_RETENTION_FLOOR_DAYS when
    # it publishes none (clamp_activity_window).
    fmin, fmax = _probe_activity_date_bounds(page, capture_dir)
    since_date, until_date = clamp_activity_window(
        since_date, until_date, fmin, fmax, date.today())
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
    return mark_outside_retention(results)


# ---------------------------------------------------------------------------
# Enterprise Document Center (digitalservices.fidelity.com/navigate/
# ent-documentcenter) — statements + tax forms.
#
# The document center is a Stencil/PVD component SPA on its own host.
# Statements and Tax forms share one shape:
#   * a left-rail nav link picks the document TYPE (relative hrefs:
#     'statements' = Personal statements, 'tax-forms'); the default
#     landing is an empty "Interested party statements". The Stencil
#     <select> ignores programmatic value writes, so the type is
#     switched by clicking the rail link, not by driving the select.
#   * a `#options-select-TimeFilter` <select> + Apply set the year.
#   * each document is an <ent-ds-link> whose text ends in "(pdf)";
#     clicking it fires an authenticated POST to
#     .../financial-documents/download whose JSON response carries the
#     PDF as base64, which is decoded directly (see
#     _doccenter_download_row); any popup tab the click spawns is
#     closed.
# ---------------------------------------------------------------------------


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


def _doccenter_download_row(page, context, row_loc):
    """Click one '(pdf)' row and return the PDF bytes.

    The doc center is API-driven: clicking a row fires an
    authenticated POST to ``.../financial-documents/download`` that
    returns the PDF as base64-in-JSON, which the SPA then renders as
    an in-memory blob. Chasing the rendered blob is fragile (revoked
    URLs, viewer-context fetch errors), so the walk waits for that
    JSON response (canonical expect_response) and decodes it. Returns the
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
        # bodies inside an ad-hoc event handler), then decode it. The
        # predicate skips the CORS preflight the same URL draws.
        with page.expect_response(_is_docapi_download_response,
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


def scrape_statements(page, context, docs_dir, capture_dir, min_year=None):
    """Walk the Enterprise Document Center's personal Statements.
    Switches to the Personal statements type (rail link -> relative
    href 'statements'), then iterates the TimeFilter years from the
    current year back to ``min_year``, downloading each '(pdf)'
    document (quarterly statements + year-end investment reports)
    (_doccenter_download_row). The page filters by whole year, so the
    floor is ``min_year`` (derived by walk() from the ``--lookback``
    window's start year)."""
    return _doccenter_walk_type(
        page, context, "statements", "Personal", "statements",
        docs_dir, capture_dir, min_year)


def scrape_tax_forms(page, context, docs_dir, capture_dir, min_year=None):
    """Walk the Enterprise Document Center's Tax forms: switch to the
    tax-forms type, iterate the TimeFilter years >= ``min_year``, and
    download each '(pdf)' form (Consolidated 1099s, one per account
    group). Same row mechanism as statements."""
    return _doccenter_walk_type(
        page, context, "tax-forms", "Tax forms", "tax-forms",
        docs_dir, capture_dir, min_year)


def scrape_documents(page, context, bronze_dir, capture_dir,
                      min_year=None, explore=False):
    """Walk Statements + Tax forms in the document center. Each
    sub-page is exercised independently so a failure in one
    category doesn't block the other. ``context`` is needed to close
    any popup tab a document click spawns.

    ``min_year`` (forwarded to both sub-scrapers) drops year
    selections older than that — see walk()'s default-derivation
    from the ``--lookback`` window's start year.

    ``explore`` (``--explore``) additionally writes a shadow-DOM-
    and iframe-piercing element inventory at each landmark — the
    Enterprise Document Center renders its content where an
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

def _save_rendered_page(page, bronze_dir, capture_dir, *, url, name,
                        ready_selector=None):
    """Visit `url` and save its rendered, scrubbed DOM as
    ``<bronze_dir>/<name>/<name>.html`` (zstd-compressed best-effort):
    the shared body of the balances and performance phases, neither of
    which offers a structured export. With `ready_selector`, waits up to
    15 s for a matching element before the DOM is read.

    Returns the phase result: ``explored-no-export`` with the saved
    file, so run.json records the (intentional) gap, or an ``error`` /
    ``session-timeout`` status."""
    out_dir = bronze_dir / name
    out_dir.mkdir(parents=True, exist_ok=True)
    try:
        goto_and_wait(page, url, wait_timeout_s=30)
        time.sleep(SPA_HYDRATE_WAIT_S)
    except Exception as e:
        log.exception("%s nav failed", name)
        return {"status": "error", "error": str(e)}
    capture(page, capture_dir, f"{name}-landed")
    landed = live_url(page)
    if not landed.startswith(POST_AUTH_PREFIX):
        log.warning(
            "%s nav landed at %s — likely session-timeout "
            "redirect; aborting", name, landed,
        )
        return {"status": "session-timeout", "landed_url": landed}
    if ready_selector is not None:
        deadline = time.monotonic() + 15.0
        while time.monotonic() < deadline:
            if page.locator(ready_selector).count() > 0:
                break
            time.sleep(0.5)
    try:
        html = page.content()
    except Exception:
        html = page.evaluate(
            "() => document.documentElement.outerHTML"
        )
    out_path = out_dir / f"{name}.html"
    # Scrubbed like every serialized DOM: a post-auth page carries no
    # password input, so the markup is written byte for byte.
    out_path.write_text(debugcap.scrub_dom(html), encoding="utf-8")
    log.info(
        "saved %s/%s (%d bytes; no structured export on this surface)",
        name, out_path.name, out_path.stat().st_size,
    )
    final = compress.compress_best_effort(out_path, log)
    return {
        "status": "explored-no-export",
        "file": str(final.relative_to(bronze_dir)),
    }


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

    The page is saved as rendered and indexed in `documents`
    (DESIGN.md §4.7); the Balance Letter wizard is never opened.
    The save waits for a per-account total to render, so the saved
    page carries the figures."""
    return _save_rendered_page(
        page, bronze_dir, capture_dir, url=URL_BALANCES, name="balances",
        ready_selector="[data-testid$='-totalaccountvalue-label']")


def scrape_performance(page, bronze_dir, capture_dir):
    """Visit the Performance page and save the rendered DOM of its
    metrics tiles.

    The page has NO structured-data export — no CSV, no PDF, no
    kebab/menu Download item. The data surfaces as a Highcharts SVG +
    a column of collapsible info tiles (return percentages by period,
    benchmark deltas). The load input for this surface is
    ``performance/performance.html``, which silver indexes as a
    document. The separate ``screenshots/performance-landed`` HTML +
    PNG is an opt-in ``--debug`` diagnostic that ``prune`` deletes.
    Compare to ``balances``, which similarly has no CSV but does offer
    a PDF 'Balance Letter' via its actions menu."""
    return _save_rendered_page(
        page, bronze_dir, capture_dir, url=URL_PERFORMANCE,
        name="performance")


# ---------------------------------------------------------------------------
# Donor-Advised Fund (Fidelity Charitable) phase
# ---------------------------------------------------------------------------
#
# The DAF sits behind a distinct SPA on charitablegift.fidelity.com,
# reached by an SSO hop that consumes the existing .fidelity.com
# session cookie (DESIGN.md §12). Unlike the retail phases, which
# scrape a rendered UI, the DAF surface is a clean JSON REST API under
# /fc-services/api/v1/ — so this phase drives it over ``page.request``
# (same cookie jar as the browser context), the firstcitizens REST
# pattern. The retail account enumeration auto-excludes the DAF by its
# 7-digit id length; this phase is what actually ingests it, from the
# charitable API's own account roster.


def _daf_get(page, url, jwt):
    """One authenticated GET against the charitable API over
    ``page.request`` (shares the browser cookie jar). Returns the
    Playwright APIResponse, or None on transport failure. The JWT is
    sent as the fid-cgf-auth-jwt header the SPA uses; cookies alone may
    suffice, but the header matches what the live UI sends."""
    headers = {"accept": "application/json, text/plain, */*"}
    if jwt:
        headers[DAF_JWT_HEADER] = jwt
    try:
        return page.request.get(url, headers=headers, timeout=30_000)
    except Exception as e:  # noqa: BLE001
        log.warning("  DAF GET %s failed: %s", _daf_ep(url), e)
        return None


def _daf_navigate_sso(page):
    """Take the SSO hop into the donor SPA so page.request inherits the
    charitablegift.fidelity.com session cookies. Returns True once a
    charitable host has loaded, False if the hop never lands there
    (e.g. a login with no Fidelity Charitable relationship, or a
    session-timeout bounce back to signin)."""
    try:
        goto_and_wait(page, DAF_SSO_URL, wait_timeout_s=30)
    except Exception as e:
        log.warning("DAF SSO nav failed: %s", e)
        return False
    # The hop redirects CGFLogon → fc-donor SPA; both are on DAF_HOST.
    # Give the redirect a moment to settle, then confirm the host.
    for _ in range(20):
        url = live_url(page)
        if DAF_HOST in url:
            return True
        if "/prgw/digital/signin" in url:
            log.warning("DAF SSO bounced to signin (session timeout?)")
            return False
        time.sleep(0.5)
    log.warning("DAF SSO did not land on %s (at %s)", DAF_HOST,
                live_url(page))
    return False


def _daf_bootstrap(page):
    """POST identity/self to mint the session JWT and read the donor's
    partyId. Returns (jwt, party_id); either may be None if the call
    fails. The JWT rides subsequent GETs; the partyId keys the account
    roster. The browser must already be on the DAF host (cookies set)
    for this to authenticate."""
    try:
        resp = page.request.post(
            DAF_IDENTITY_URL,
            data=json.dumps({"channelID": "DONOR"}),
            headers={
                "content-type": "application/json",
                "accept": "application/json, text/plain, */*",
                "origin": DAF_ORIGIN,
            },
            timeout=30_000,
        )
    except Exception as e:  # noqa: BLE001
        log.warning("DAF identity bootstrap failed: %s", e)
        return None, None
    if not resp.ok:
        log.warning("DAF identity/self returned HTTP %d", resp.status)
        return None, None
    jwt = resp.headers.get(DAF_JWT_HEADER)
    party_id = None
    try:
        party_id = resp.json().get("partyId")
    except Exception as e:  # noqa: BLE001
        log.debug("DAF identity/self body parse: %s", e)
    if not jwt:
        log.warning("DAF identity/self carried no %s header; will try "
                    "cookie-only auth", DAF_JWT_HEADER)
    return jwt, party_id


def _daf_fetch_json(page, url, jwt):
    """GET a JSON endpoint, returning (parsed_obj, raw_bytes) or
    (None, None). raw_bytes is what lands in bronze (the JSON is the
    primary silver source; storing it verbatim keeps every field)."""
    resp = _daf_get(page, url, jwt)
    if resp is None or not resp.ok:
        if resp is not None:
            log.warning("  DAF %s → HTTP %d", _daf_ep(url), resp.status)
        return None, None
    raw = resp.body()
    try:
        return json.loads(raw), raw
    except Exception as e:  # noqa: BLE001
        log.warning("  DAF %s body not JSON: %s", _daf_ep(url), e)
        return None, raw


def _daf_fetch_paged(page, base_url, jwt):
    """Fetch every page of a paginated charitable-history envelope
    (``{totalItems, itemsPerPage, currentItemCount, items:[...]}``) and
    return the flattened item list. ``base_url`` already carries the
    filter params; this appends ``page``/``size``. Stops when the
    collected count reaches totalItems, a page comes back empty, or a
    hard page cap trips (a runaway-loop backstop)."""
    sep = "&" if "?" in base_url else "?"
    items = []
    page_no = 1
    total = None
    while page_no <= 1000:
        url = f"{base_url}{sep}page={page_no}&size={DAF_PAGE_SIZE}"
        obj, _ = _daf_fetch_json(page, url, jwt)
        if obj is None:
            break
        batch = obj.get("items") or []
        items.extend(batch)
        if total is None:
            total = obj.get("totalItems")
        if not batch or (total is not None and len(items) >= total):
            break
        page_no += 1
    return items


def _daf_save_json(dest_dir, name, raw_bytes):
    """Persist a raw JSON body under ``dest_dir``/``name`` (bronze;
    never compressed — kept greppable and small). No-op (returns None)
    when ``dest_dir`` is None (dry-run) or the body is None, so callers
    can invoke it unconditionally."""
    if dest_dir is None or raw_bytes is None:
        return None
    out = dest_dir / name
    out.write_bytes(raw_bytes)
    return out


def _daf_save_items(dest_dir, name, items):
    """Persist a merged item list (paginated envelopes flattened, or
    per-year unions) as pretty JSON under ``dest_dir``/``name``. No-op
    (returns None) when ``dest_dir`` is None (dry-run), so callers can
    invoke it unconditionally."""
    if dest_dir is None:
        return None
    out = dest_dir / name
    bronze.atomic_write_json(out, items)
    return out


def _daf_fetch_csv(page, url, jwt, dest_dir, stem):
    """GET a server-side CSV export and persist it zstd-compressed
    (parity with the retail CSV bronze). Returns the on-disk path or
    None. The CSV is corroborating bronze — the JSON is the primary
    silver source — so a failure is a warning, not fatal."""
    resp = _daf_get(page, url, jwt)
    if resp is None or not resp.ok:
        if resp is not None:
            log.warning("  DAF CSV %s → HTTP %d", stem, resp.status)
        return None
    out = dest_dir / f"{stem}.csv"
    out.write_bytes(resp.body())
    return compress.compress_best_effort(out, log)


def _daf_scrape_documents(page, jwt, account_nbr, since_date, acct_dir):
    """List and download the DAF's PDF documents across every type.

    Queries the document listing per type over [since_date, today],
    then fetches each row's PDF via document/download (the endpoint
    returns application/pdf directly — no base64-in-JSON dance like the
    retail document center). Dedups on the PDF content hash so a
    document that appears under several types / windows is stored once.
    The PDFs land in ``acct_dir/documents/`` and the listing metadata in
    ``acct_dir/documents_index.json``. Pass ``acct_dir=None`` for a
    dry-run: only the listing is read (for the count), nothing is
    downloaded, and nothing is written. Returns a result dict."""
    dry_run = acct_dir is None
    docs_dir = None if dry_run else acct_dir / "documents"
    if docs_dir is not None:
        docs_dir.mkdir(parents=True, exist_ok=True)
    today = datetime.now(timezone.utc).date()
    index = []
    seen_hashes = set()
    saved = 0
    errors = []
    for dtype in DAF_DOCUMENT_TYPES:
        list_url = (
            f"{DAF_API}/document?accountNumber={account_nbr}"
            f"&documentType={dtype}"
            f"&fromDate={since_date.isoformat()}&endDate={today.isoformat()}"
        )
        obj, _ = _daf_fetch_json(page, list_url, jwt)
        rows = obj if isinstance(obj, list) else []
        log.info("  DAF documents[%s]: %d listed", dtype, len(rows))
        for row in rows:
            key = row.get("legacyDocumentKey")
            if not key:
                continue
            index.append({
                "documentType": row.get("documentType"),
                "documentName": row.get("documentName"),
                "correspondenceDate": row.get("correspondenceDate"),
                "generatedDate": row.get("generatedDate"),
                "id": row.get("id"),
            })
            if dry_run:
                # Listing only — the PDF fetch is a read-only export
                # trigger the dry-run contract forbids.
                continue
            dl_url = (
                f"{DAF_API}/document/download?accountNumber={account_nbr}"
                f"&legacyDocumentKey={quote(key, safe='')}"
                f"&documentType={row.get('documentType') or dtype}"
            )
            resp = _daf_get(page, dl_url, jwt)
            if resp is None or not resp.ok:
                # Label the failure by type, never by the legacyDocumentKey
                # (it embeds account-scoped tokens — kept out of run.json).
                errors.append(row.get("documentName") or f"{dtype} document")
                continue
            body = resp.body()
            if body[:4] != b"%PDF":
                log.warning("  DAF document %s not a PDF (%d bytes); "
                            "skipping", row.get("documentName"), len(body))
                errors.append(row.get("documentName") or f"{dtype} document")
                continue
            digest = hashlib.sha256(body).hexdigest()
            if digest in seen_hashes:
                continue
            seen_hashes.add(digest)
            stem = _daf_doc_stem(row, dtype)
            _write_doc_bytes(docs_dir, f"{stem}.pdf", body)
            saved += 1
    _daf_save_items(acct_dir, "documents_index.json", index)
    return {"listed": len(index), "saved_pdfs": saved,
            "errors": errors}


def _daf_scrape_account(page, jwt, account, since_date, daf_root, *,
                        dry_run=False, positions_only=False):
    """Scrape one giving account: the JSON surfaces (master, pools,
    grants, contributions, exchanges, gifts, adjustments), the
    server-side CSV exports, and the PDF document set. Bronze lands
    under ``daf_root/<account_key>/``. Returns a per-account result
    dict.

    ``positions_only`` (the --mode positions slice) stops after the
    account master + pool balances, so a positions-bearing dump is a
    complete holdings observation without pulling the whole event /
    document archive.

    On ``dry_run`` it does the pure-read JSON enumeration (so the plan
    log carries real counts and any auth/SSO failure surfaces) but
    fetches no CSV export or PDF and writes no bronze — honouring the
    root AGENTS.md §2 "export nothing" dry-run contract."""
    account_nbr = str(account.get("accountNbr") or account.get("accountNumber")
                      or "")
    if not account_nbr:
        return {"status": "error", "error": "roster row had no accountNbr"}
    # On a dry-run every bronze dir is None; the _daf_save_* helpers and
    # _daf_scrape_documents all no-op on a None dir, so the enumeration
    # reads run unguarded and simply write nothing.
    if dry_run:
        acct_dir = exports_dir = None
    else:
        acct_dir = daf_root / account_key(account_nbr)
        exports_dir = acct_dir / "exports"
        exports_dir.mkdir(parents=True, exist_ok=True)
    today = datetime.now(timezone.utc).date()
    result = {"status": "dry-run" if dry_run else "complete"}

    # Account master (balance + pending buckets + establish date).
    master, master_raw = _daf_fetch_json(
        page, f"{DAF_API}/givingAccounts/{account_nbr}", jwt)
    _daf_save_json(acct_dir, "account.json", master_raw)
    establish = _daf_parse_establish_date(master)
    result["establish_date"] = establish.isoformat() if establish else None

    # Pool positions (a snapshot; numberOfDays=1 = today's holdings).
    pools, pools_raw = _daf_fetch_json(
        page,
        f"{DAF_API}/poolBalances?accountNumber={account_nbr}"
        f"&endDate={today.isoformat()}&numberOfDays=1",
        jwt)
    _daf_save_json(acct_dir, "pool_balances.json", pools_raw)
    # poolBalances is a list of date buckets, each with a poolInfoList;
    # count the pools in the latest bucket for the run manifest.
    result["pools"] = (
        len((pools[0] or {}).get("poolInfoList") or [])
        if isinstance(pools, list) and pools else 0
    )
    # Without the master or the pools the account's holdings were not
    # observed, which the loader's completeness gate must see.
    missing = [name for name, obj in (("account master", master),
                                      ("pool balances", pools))
               if obj is None]
    if missing:
        result["status"] = "error"
        result["error"] = f"{' and '.join(missing)} not fetched"
    if positions_only:
        # The --mode positions slice stops here: master + pool only.
        # History / exports / documents belong to the full phase.
        return result

    # History surfaces honour the resolved window (--lookback) exactly
    # like the retail activity/documents phases: with a window it filters
    # fromDate/endDate; with none (only the helper/tests reach this) it
    # falls back to the source's SINCE_INCEPTION filter. Silver is
    # cumulative — a 90-day nightly window keeps recent events fresh
    # while an initial ``--lookback all`` backfills the whole history.
    if since_date:
        window = (f"&fromDate={since_date.isoformat()}T00:00:00.000Z"
                  f"&endDate={today.isoformat()}T23:59:59.999Z")
    else:
        window = "&viewingFilter=SINCE_INCEPTION"

    grants = _daf_fetch_paged(
        page, f"{DAF_API}/transactionHistory/grants?accountId={account_nbr}"
        f"{window}", jwt)
    _daf_save_items(acct_dir, "grants.json", grants)
    result["grants"] = len(grants)

    contribs = _daf_fetch_paged(
        page,
        f"{DAF_API}/transactionHistory/contributions?accountId={account_nbr}"
        f"{window}", jwt)
    _daf_save_items(acct_dir, "contributions.json", contribs)
    result["contributions"] = len(contribs)

    gifts = _daf_fetch_paged(
        page, f"{DAF_API}/gift?donorAccountNumber={account_nbr}", jwt)
    _daf_save_items(acct_dir, "gifts.json", gifts)
    result["gifts"] = len(gifts)

    # Year-scoped surfaces: union across the account's active years.
    years = _daf_year_span(since_date, establish)
    exchanges = []
    adjustments = []
    for year in years:
        yx, _ = _daf_fetch_json(
            page,
            f"{DAF_API}/poolExchange?accountNumber={account_nbr}&year={year}",
            jwt)
        if isinstance(yx, list):
            exchanges.extend(yx)
        adj = _daf_fetch_paged(
            page,
            f"{DAF_API}/transactionHistory/adjustment?accountNumber="
            f"{account_nbr}&year={year}", jwt)
        adjustments.extend(adj)
    _daf_save_items(acct_dir, "pool_exchanges.json", exchanges)
    _daf_save_items(acct_dir, "adjustments.json", adjustments)
    result["exchanges"] = len(exchanges)
    result["adjustments"] = len(adjustments)

    # PDF documents (statements, grant confirmations, contribution
    # confirmations, Form 8283) from the establish date (or the
    # requested window start) forward. On a dry-run (acct_dir None) only
    # the listing is read for the plan count; nothing is downloaded.
    doc_since = since_date or establish or (
        today - timedelta(days=365 * 10))
    result["documents"] = _daf_scrape_documents(
        page, jwt, account_nbr, doc_since, acct_dir)
    if dry_run:
        # A dry-run stops here: the CSV exports below are read-only
        # export triggers, which "export nothing" forbids.
        return result

    # Server-side CSV exports (corroborating bronze; JSON is primary).
    # Same window as the JSON above so the two views agree.
    _daf_fetch_csv(
        page,
        f"{DAF_API}/transactionHistory/grants/download?accountId="
        f"{account_nbr}{window}&format=csv", jwt, exports_dir, "grants")
    _daf_fetch_csv(
        page,
        f"{DAF_API}/transactionHistory/contributions/download?accountId="
        f"{account_nbr}{window}&format=csv", jwt, exports_dir,
        "contributions")
    _daf_fetch_csv(
        page,
        f"{DAF_API}/poolBalances/download?accountNumber={account_nbr}"
        f"&numberOfDays=1&endDate={today.isoformat()}&format=csv",
        jwt, exports_dir, "pool_balances")
    _daf_fetch_csv(
        page,
        f"{DAF_API}/gift/download?donorAccountNumber={account_nbr}"
        f"&asOfDate={today.isoformat()}&format=csv", jwt, exports_dir,
        "gifts")
    for year in years:
        _daf_fetch_csv(
            page,
            f"{DAF_API}/poolExchange/download?accountNumber={account_nbr}"
            f"&year={year}&format=csv", jwt, exports_dir,
            f"pool_exchanges_{year}")
        _daf_fetch_csv(
            page,
            f"{DAF_API}/transactionHistory/adjustment/download?accountNumber="
            f"{account_nbr}&format=csv&year={year}", jwt, exports_dir,
            f"adjustments_{year}")

    return result


def scrape_daf(page, bronze_dir, capture_dir, since_date, *,
               dry_run=False, positions_only=False):
    """Ingest the Fidelity Charitable Donor-Advised Fund surface.

    Takes the SSO hop into the donor SPA, bootstraps the session JWT +
    partyId, reads the giving-account roster, and scrapes each account
    over the JSON REST API (plus CSV exports and PDF documents). Bronze
    lands under ``<run>/daf/``. ``positions_only`` restricts each
    account to the master + pool-balances slice (the --mode positions
    path); ``dry_run`` still hops, bootstraps, and reads the roster +
    per-account counts but fetches no export and writes no bronze
    (``bronze_dir`` may be None).

    Iterates every giving account the roster returns, so a login that
    holds more than one DAF is covered (DESIGN.md §12). A login with no
    charitable relationship yields
    ``status: 'no-daf'`` and writes nothing."""
    if not _daf_navigate_sso(page):
        return {"status": "no-daf",
                "note": "SSO hop did not reach the charitable host"}
    capture(page, capture_dir, "daf-spa-landed")
    jwt, party_id = _daf_bootstrap(page)
    if party_id is None:
        return {"status": "error",
                "error": "identity bootstrap returned no partyId"}

    roster_obj, roster_raw = _daf_fetch_json(
        page, f"{DAF_API}/user/{party_id}/accounts", jwt)
    accounts = roster_obj if isinstance(roster_obj, list) else []
    if not accounts:
        return {"status": "no-daf", "note": "empty giving-account roster"}
    daf_root = None
    if not dry_run:
        daf_root = bronze_dir / "daf"
        daf_root.mkdir(parents=True, exist_ok=True)
        _daf_save_json(daf_root, "accounts.json", roster_raw)
    log.info("DAF: %d giving account(s) in roster%s", len(accounts),
             " (dry-run: enumerate only, no writes)" if dry_run else "")

    per_account = {}
    for account in accounts:
        nbr = str(account.get("accountNbr") or account.get("accountNumber")
                  or "")
        if not nbr:
            continue
        log.info("DAF: %s giving account %s",
                 "enumerating" if dry_run else "scraping", account_key(nbr))
        try:
            per_account[account_key(nbr)] = _daf_scrape_account(
                page, jwt, account, since_date, daf_root,
                dry_run=dry_run, positions_only=positions_only)
        except Exception as e:  # noqa: BLE001
            log.exception("DAF account scrape failed")
            per_account[account_key(nbr)] = {"status": "error",
                                             "error": str(e)}
    failed = sum(1 for r in per_account.values()
                 if r.get("status") == "error")
    result = {"status": "dry-run" if dry_run else "complete",
              "accounts": len(accounts),
              "per_account": per_account}
    if failed:
        result["status"] = "error"
        result["error"] = f"{failed} giving account(s) failed"
    return result


# ---------------------------------------------------------------------------
# walk() — orchestrates the per-phase scrapes against a live session
# ---------------------------------------------------------------------------

@dataclass(frozen=True)
class Pass:
    """One phase of the walk.

    `name` is the phase as `coverage` names it, and `key` the run.json
    key its result lands under. A key is present exactly when the phase
    was requested and attempted, so the key, not the value of --mode,
    says a phase ran. `mode` is the --mode word that runs it,
    besides `all`. `plan` is the dry run's step for the phase, or None
    when the dry run has nothing to do for it. `run` scrapes; None from
    it leaves the key out. A failure of an `isolated` phase becomes a
    failed result in the phase's own shape. `ok_status` lists the
    statuses that mean the phase did its job; None means its result is
    a list of attempts, each with its own `ok`.
    """

    name: str
    key: str
    mode: str
    plan: Callable[[_Walk], None] | None
    run: Callable[[_Walk], object]
    isolated: bool
    ok_status: tuple[str, ...] | None


@dataclass(frozen=True)
class _Walk:
    """What the phases of one walk share."""

    context: object
    page: object
    config: dict
    mode: str
    dest_root: Path
    slug: str
    bronze_dir: Path | None
    capture_dir: Path | None
    dimensions: list[dict]
    in_scope: list[str]
    since_date: date | None
    until_date: date | None
    docs_min_year: int


def _plan_daf(w):
    """The DAF phase lives behind an SSO hop + its own JSON API, so
    unlike the retail phases (whose surfaces the enumeration already
    reached) a dry-run has to exercise it explicitly to cover that
    surface — the read-only enumeration hops, bootstraps, reads the
    roster + per-account counts, and writes nothing."""
    if _login_has_daf(w.dimensions):
        daf_plan = scrape_daf(
            w.page, None, None, w.since_date, dry_run=True,
            positions_only=(w.mode == "positions"))
        log.info("dry-run: DAF plan — %s", json.dumps(daf_plan,
                 default=str))


def _run_daf(w):
    """The DAF rides every positions-bearing mode so each such dump
    is a COMPLETE holdings observation (retail + DAF) — gold's
    per-source latest-snapshot anchor treats it as one, and a
    partial dump would read as the uncovered accounts having
    emptied. --mode all runs the full DAF phase (events, exports,
    documents); --mode positions runs the master + pool slice.
    There is deliberately no DAF-only mode for the same reason."""
    if not _login_has_daf(w.dimensions):
        log.info("no Fidelity Charitable relationship enumerated; "
                 "skipping DAF phase")
        return None
    daf_results = scrape_daf(
        w.page, w.bronze_dir, w.capture_dir, w.since_date,
        positions_only=(w.mode == "positions"),
    )
    if daf_results.get("status") == "no-daf":
        # The account enumeration said this login HAS a DAF, so a
        # no-daf outcome here is a failure (SSO bounce, empty
        # roster), not an absence — escalate so the loader's
        # completeness guard treats the dump's positions as
        # partial rather than loading a retail-only observation.
        daf_results = {"status": "error",
                       "error": "DAF expected (enumerated in the "
                                "account selector) but the "
                                "charitable walk found none",
                       **{k: v for k, v in daf_results.items()
                          if k != "status"}}
        log.warning("DAF phase failed: %s", daf_results["error"])
    return daf_results


# The walk's phases, in the order it runs them. A lambda looks its
# scrape up when it runs, so a test can replace it.
PASSES = (
    Pass("positions", "positions_results", mode="positions", plan=None,
         run=lambda w: scrape_positions(w.page, w.bronze_dir, w.capture_dir),
         isolated=True, ok_status=None),
    Pass("lots", "lots_results", mode="positions", plan=None,
         run=lambda w: scrape_lots(
             w.page, w.bronze_dir, w.in_scope,
             previous_lot_index(w.dest_root, w.slug),
             refresh=w.config.get("refresh_lots", "false").lower() == "true",
         ),
         isolated=True, ok_status=("complete",)),
    Pass("activity", "activity_results", mode="activity", plan=None,
         run=lambda w: scrape_activity(
             w.page, w.since_date, w.until_date,
             w.bronze_dir, w.capture_dir,
         ),
         isolated=True, ok_status=None),
    Pass("documents", "documents_results", mode="documents", plan=None,
         run=lambda w: scrape_documents(
             w.page, w.context, w.bronze_dir, w.capture_dir,
             min_year=w.docs_min_year,
             explore=w.config.get("explore", "false").lower() == "true",
         ),
         isolated=True, ok_status=("walked",)),
    Pass("balances", "balances_results", mode="balances", plan=None,
         run=lambda w: scrape_balances(w.page, w.bronze_dir, w.capture_dir),
         isolated=True, ok_status=("explored-no-export",)),
    Pass("performance", "performance_results", mode="performance",
         plan=None,
         run=lambda w: scrape_performance(
             w.page, w.bronze_dir, w.capture_dir,
         ),
         isolated=True, ok_status=("explored-no-export",)),
    # `no-daf` is a success: a login with no donor-advised fund has
    # nothing to fetch, and _run_daf has already turned a no-daf that
    # contradicts the enumerated accounts into an error.
    Pass("daf", "daf_results", mode="positions", plan=_plan_daf,
         run=_run_daf, isolated=True, ok_status=("complete", "no-daf")),
)


def _run_phase(phase, w):
    """Run one phase's scrape. An exception it raises becomes a failed
    result in the phase's own shape, so the walk goes on to the later
    phases, finishes its manifest, and `phase_coverage` reports the
    gap."""
    try:
        return phase.run(w)
    except Exception as e:
        log.exception("%s phase failed", phase.name)
        if phase.ok_status is None:
            return [{"ok": False, "error": str(e)}]
        return {"status": "error", "error": str(e)}


def walk(context, page, config):
    """Run the requested phase(s) against a logged-in session.
    Called from ``run_oneshot()`` after ``login()`` lands on the
    post-auth URL. Runs the phases of ``PASSES`` that
    ``config['mode']`` selects, in table order. Each isolated phase
    runs under _run_phase, so a failure in one phase does not block
    the others."""
    dest_root = Path(config.get("dest") or "/data")
    dry_run = config.get("dry_run", "false").lower() == "true"
    debug = config.get("debug", "false").lower() == "true"
    slug = bronze.ts_slug()
    if dry_run:
        # A dry-run is a read-only walk: reach the export surfaces,
        # enumerate account_dimensions, and log the plan. It persists
        # nothing under the bronze dest, not even run.json (root
        # AGENTS.md §2 "export nothing"). No dir means `load` and
        # `prune` never see one.
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
        # can reclaim once it goes quiescent). Until it lands, the run
        # dir has no run.json, which `load` skips as still being
        # written.
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

    # Ensure the page is on a portfolio surface where the account
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
    # Auto-exclude non-9-digit accounts from the retail phases: the
    # Fidelity Charitable DAF uses a 7-digit id, and the DAF phase
    # reads it from the charitable API's own roster instead.
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
    # the window starts in. --lookback widens it.
    documents_since = since_date or (
        datetime.now(timezone.utc).date() - timedelta(days=90))
    docs_min_year = documents_since.year

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

    w = _Walk(
        context=context, page=page, config=config, mode=mode,
        dest_root=dest_root, slug=slug, bronze_dir=bronze_dir,
        capture_dir=capture_dir, dimensions=dimensions, in_scope=in_scope,
        since_date=since_date, until_date=until_date,
        docs_min_year=docs_min_year,
    )
    selected = [p for p in PASSES if mode in ("all", p.mode)]

    if dry_run:
        # Log the plan (what a real run WOULD fetch: counts, scope,
        # windows). No run dir was created and no run.json is written —
        # the dry-run persists nothing under the bronze dest. (run_json
        # above is built solely to shape this plan log.)
        log.info(
            "dry-run: enumerated %d account(s), %d in scope; "
            "phases=%s activity_window=%s documents_since=%s. "
            "Skipping all artefact downloads — nothing written to "
            "bronze under %s.",
            len(all_accounts), len(in_scope), mode,
            run_json["activity_window"], documents_since.isoformat(),
            dest_root,
        )
        for phase in selected:
            if phase.plan is not None:
                phase.plan(w)
        return

    for phase in selected:
        result = _run_phase(phase, w) if phase.isolated else phase.run(w)
        if result is not None:
            run_json[phase.key] = result
    # What each attempted phase actually got. Always written, so an
    # empty `gaps` list is a positive statement of coverage rather
    # than the absence of a field.
    run_json["coverage"] = phase_coverage(
        PASSES, run_json, (config.get("since"), config.get("until")),
    )

    # The status stays `complete` whatever the phases did, and that is
    # deliberate rather than an oversight. `complete` means "the walk
    # reached its end", and it is what keeps the dump loadable and out
    # of prune's delete path — a run whose activity phase failed still
    # holds good positions, balances, documents and DAF artefacts, and
    # downgrading the status would hand all of it to prune and block
    # the positions load besides. Phase completeness and DUMP
    # completeness are different axes; this one is the dump's, and
    # `coverage` above plus the process exit code carry the other.
    run_json["status"] = "complete"

    # Atomic (tmp + rename) so a prune racing the finalisation never
    # reads a half-written manifest, and the in-progress marker is
    # replaced in one step.
    run_path = bronze_dir / "run.json"
    bronze.atomic_write_json(run_path, run_json)
    log.info("walk: wrote %s", run_path)
    return run_json["coverage"]


# ---------------------------------------------------------------------------
# Env-file loader
# ---------------------------------------------------------------------------

def maybe_source_env_files(args):
    envfile.load(args.env_file, candidates=DEFAULT_ENV_FILE_CANDIDATES,
                 logger=log)


# ---------------------------------------------------------------------------
# Camoufox launch
# ---------------------------------------------------------------------------

def open_camoufox_context(profile_dir, trace, snapshots=False):
    """Headed Camoufox on the persistent profile, yielding the
    BrowserContext (:func:`collectorkit.launch.camoufox_context`).

    The fleet's sign-in fingerprint stays on in full: the humanized
    cursor fires the mousemove/mousedown/mouseup events Akamai's
    bot-scoring observes, and the geoip-matched timezone and locale keep
    its geo-anomaly heuristic quiet. Every action and navigation waits
    up to ``LANDMARK_TIMEOUT_MS``.

    ``snapshots`` defaults off because the sign-in shares this context:
    a trace's DOM snapshots carry every input's value and no redactor
    reaches a trace. A caller that types no credential passes True, and
    the sign-in path turns them on for the walk with
    :func:`restart_trace_after_signin`.
    """
    return launch.camoufox_context(
        profile_dir, trace=trace, trace_snapshots=snapshots,
        timeout_ms=LANDMARK_TIMEOUT_MS, logger=log)


def open_page(context):
    """Return a Page in the given context, reusing the existing one
    (Camoufox's persistent_context always opens one) or creating a
    fresh one if needed."""
    pages = context.pages
    if pages:
        return pages[0]
    return context.new_page()


def restart_trace_after_signin(context, trace, screenshot_dir) -> None:
    """Close the snapshot-free sign-in trace and open a snapshotting one.

    The DOM snapshots that make a trace worth reading are the same ones
    that would carry the typed password, so they are off until the
    credential screens are done and on for the walk. Best effort: a
    trace that will not restart costs diagnostics, never the run.
    """
    if not debugcap.stop_trace_if_active(context, trace, screenshot_dir,
                                         "signin", log=log):
        return
    try:
        context.tracing.start(screenshots=True, snapshots=True, sources=True)
    except Exception as e:
        log.warning("restarting the trace after sign-in failed: %s", e)


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


def accept_iua(accept_loc):
    accept_loc.click()
    log.info("clicked I Accept on International Usage Agreement")


def _username_input(page):
    """The signin form's username input, whichever generation is
    served: ``(how, locator)``, or ``(None, None)`` if nothing
    matches. The last-resort branch is the page's only visible text
    input, which is right today but keyed on nothing, so it names
    itself in the log — a run filling through it is a run whose real
    selector has drifted and is one render away from filling the
    wrong box."""
    for sel in SEL_USERNAME_TEXT_INPUT_CANDIDATES:
        loc = page.locator(sel)
        try:
            if loc.count() > 0 and loc.first.is_visible(timeout=2_000):
                return sel, loc.first
        except Exception as e:
            log.debug("username candidate %s failed: %s", sel, e)
    fallback = page.locator(
        "input[type=text]:visible, input:not([type]):visible"
    )
    if fallback.count() > 0:
        return "last-resort visible text input", fallback.first
    return None, None


# How many times _fill_verified fills a field before giving up.
FILL_ATTEMPTS = 3


def _fill_verified(loc, value, what):
    """Fill ``loc`` and read the value back, up to FILL_ATTEMPTS times.
    True when the field ends up holding it.

    A PVD input is a web component, and the framework can overwrite
    its value when it hydrates or re-renders. A fill that lands just
    before that happens leaves the field EMPTY while ``Locator.fill``
    reports success — so the form submits blank credentials, Fidelity
    serves the signin page again, and fifteen seconds later the run
    reports "neither 2FA input nor post-auth URL" with nothing naming
    the real cause. Reading the value back is what turns that into a
    retry.

    Only LENGTHS are logged, never the value: this runs on a username
    and a password."""
    for attempt in range(1, FILL_ATTEMPTS + 1):
        try:
            loc.fill(value)
            got = loc.input_value(timeout=2_000)
        except Exception as e:
            log.debug("%s fill/read-back failed: %s", what, e)
            got = ""
        if got == value:
            return True
        log.debug(
            "%s did not stick on attempt %d (field holds %d chars, "
            "want %d) — the component probably re-rendered under the "
            "fill; retrying",
            what, attempt, len(got), len(value),
        )
        time.sleep(0.6)
    return False


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
    how, loc = _username_input(page)
    if loc is None:
        raise SystemExit(
            "could not locate the username input field. Check the "
            "captured HTML in --screenshot-dir for what Fidelity served."
        )
    if not _fill_verified(loc, username, "username"):
        raise SystemExit(
            "the username field would not hold its value — Fidelity's "
            "signin form re-rendered under the fill. Nothing was "
            "submitted; re-run, and capture with --screenshot-dir if "
            "it persists."
        )
    log.info("filled username via %s", how)


def fill_password(page, password):
    if not _fill_verified(
            page.locator(SEL_PASSWORD_INPUT).first, password, "password"):
        raise SystemExit(
            "the password field would not hold its value — Fidelity's "
            "signin form re-rendered under the fill. Nothing was "
            "submitted; re-run, and capture with --screenshot-dir if "
            "it persists."
        )
    log.info("filled password")


def click_login(page):
    """Submit — but never with an empty field.

    A blank submit is not harmless: Fidelity counts it as a failed
    sign-in attempt, so a run that makes one nightly walks the account
    towards a lockout while reporting only that no post-auth URL
    appeared. The two fills each verify their own field; this checks
    both once more, because the re-render that empties one can happen
    between them."""
    _, user_loc = _username_input(page)
    checks = [("password", page.locator(SEL_PASSWORD_INPUT).first)]
    if user_loc is not None:
        checks.append(("username", user_loc))
    for what, loc in checks:
        try:
            filled = bool(loc.input_value(timeout=1_000))
        except Exception as e:
            log.debug("%s pre-submit check: %s", what, e)
            continue
        if not filled:
            raise SystemExit(
                f"refusing to submit the signin form: the {what} field "
                "is empty. Fidelity's form re-rendered under the fill, "
                "and a blank submit would count as a failed sign-in "
                "attempt against the account."
            )
    page.locator(SEL_LOGIN_BUTTON).click(timeout=LANDMARK_TIMEOUT_MS)
    log.info("clicked %s", SEL_LOGIN_BUTTON)


def wait_for_mfa_input_or_post_auth(page, timeout_s):
    """Poll for whichever appears first: the 2FA code input (a code
    is still needed) or the post-auth URL (device trusted, MFA
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


def ensure_trust_browser_checked(page):
    """Best-effort: tick the 'Trust this browser' checkbox so the
    device-trust cookie suppresses MFA on subsequent runs.

    PVD wraps the <input> in a <label> whose ::before overlay
    intercepts pointer events on the input. .check() would retry
    for the full 60s default — burning TOTP validity. The LABEL is
    clicked instead. Bounded to 2s; non-fatal on failure."""
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
    Returns True on success, False on failure."""
    log.info("navigating to %s", SIGNIN_URL)
    page.goto(SIGNIN_URL, wait_until="domcontentloaded")
    capture(page, args.screenshot_dir, "01-initial-load")

    kind, iua_loc = wait_for_signin_or_iua(page, timeout_s=30)
    if kind is None:
        capture(page, args.screenshot_dir, "01b-load-timeout")
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
        capture(page, args.screenshot_dir, "01a-iua-page")
        accept_iua(iua_loc)
        try:
            page.locator(SEL_PASSWORD_INPUT).wait_for(
                state="visible", timeout=30_000,
            )
        except Exception as e:
            capture(
                page, args.screenshot_dir, "01c-post-iua-no-form",
            )
            log.error(
                "signin form did not appear within 30s after IUA "
                "acceptance: %s", e,
            )
            return False
        capture(
            page, args.screenshot_dir, "01d-signin-after-iua",
        )
    else:
        log.info("signin form rendered directly (no IUA)")
        capture(page, args.screenshot_dir, "01e-signin-direct")

    try:
        fill_username(page, username)
        fill_password(page, password)
    except Exception as e:
        capture(page, args.screenshot_dir, "02-prefill-failed")
        log.error("credential pre-fill failed: %s", e)
        return False
    capture(page, args.screenshot_dir, "02-prefilled")

    if args.vnc:
        sys.stderr.write("\n" + "=" * 60 + "\n")
        sys.stderr.write(
            "READY: Camoufox is up and the login form is pre-filled.\n"
            "Connect with your VNC client now (the host port and "
            "password are printed at the top of this output). Then "
            "in the VNC window:\n"
            "  1. Click 'Log in'.\n"
            "  2. Complete the 2FA challenge.\n"
            "  3. Wait for the post-auth landing page to load.\n"
            f"This script will detect the post-auth URL and "
            f"continue automatically (up to "
            f"{args.mfa_timeout:.0f}s wait).\n"
        )
        sys.stderr.write("=" * 60 + "\n")
        sys.stderr.flush()
        if wait_for_post_auth_url(
            page, timeout_s=args.mfa_timeout,
        ):
            capture(page, args.screenshot_dir, "08-post-auth")
            return True
        capture(
            page, args.screenshot_dir, "vnc-post-auth-timeout",
        )
        log.error(
            "did not see %s* within %.0fs after VNC handoff.",
            POST_AUTH_PREFIX, args.mfa_timeout,
        )
        return False

    try:
        click_login(page)
    except Exception as e:
        capture(page, args.screenshot_dir, "03-submit-failed")
        log.error("login submit failed: %s", e)
        return False
    capture(page, args.screenshot_dir, "03-submit-clicked")
    time.sleep(1.0)
    capture(page, args.screenshot_dir, "04-submit-settled")

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
        capture(page, args.screenshot_dir, "08-post-auth")
        return True
    if kind == "mfa":
        capture(page, args.screenshot_dir, "05-mfa-page")
        if args.trust_this_browser:
            ensure_trust_browser_checked(page)
        else:
            log.info(
                "--no-trust-this-browser: leaving the checkbox "
                "unticked"
            )
        code = cli.prompt_on_stderr(
            "Fidelity 2FA: enter your security code, then press Enter.")
        if not code:
            log.error("no 2FA code entered; aborting")
            return False
        if not submit_mfa_code(page, code_loc, code):
            capture(
                page, args.screenshot_dir, "06-mfa-submit-failed",
            )
            return False
        capture(page, args.screenshot_dir, "06-mfa-submitted")
        log.info("2FA submitted; waiting for post-auth landing URL")
        if not wait_for_post_auth_url(
            page, timeout_s=args.nav_timeout,
        ):
            capture(
                page, args.screenshot_dir, "07-post-auth-timeout",
            )
            log.error(
                "did not see %s* within %.0fs after 2FA submit",
                POST_AUTH_PREFIX, args.nav_timeout,
            )
            return False
        capture(page, args.screenshot_dir, "08-post-auth")
        return True
    capture(
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
    still the cleanup and Fidelity's idle timeout (~15 min)
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
            capture(page, screenshot_dir, "99-logout")
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
    # snapshots=True: --check navigates to the landing page and types
    # nothing, so there is no credential for a DOM snapshot to catch.
    with open_camoufox_context(args.profile_dir, args.trace,
                               snapshots=True) as context:
        try:
            page = open_page(context)
            landing = POST_AUTH_PREFIX + "summary"
            log.info("navigating to %s", landing)
            try:
                page.goto(landing, wait_until="domcontentloaded")
            except Exception as e:
                log.error("navigation failed: %s", e)
                return cli.EXIT_NO_VERDICT
            capture(page, args.screenshot_dir, "check-landed")
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
            debugcap.stop_trace_if_active(
                context, args.trace, args.screenshot_dir, "check", log=log,
            )


def run_oneshot(args):
    """Full path: login → walk → logout → exit."""
    username = next((os.environ[k] for k in USERNAME_ENVS if os.environ.get(k)), None)
    password = next((os.environ[k] for k in PASSWORD_ENVS if os.environ.get(k)), None)
    if not username or not password:
        log.error(
            "%s and %s must be set (or sourced from --env-file / "
            "default env-file paths). See README.md.",
            USERNAME_ENVS[0], PASSWORD_ENVS[0],
        )
        return 2
    log.info(
        "creds loaded: %s (len=%d), %s (len=%d)",
        USERNAME_ENVS[0], len(username), PASSWORD_ENVS[0], len(password),
    )
    with open_camoufox_context(args.profile_dir, args.trace) as context:
        try:
            page = open_page(context)
            if not login(page, args, username, password):
                return cli.EXIT_NO_VERDICT
            # Sign-in done: from here a DOM snapshot can only catch account
            # data, which the trace already holds, so the walk gets the
            # richer artefact.
            restart_trace_after_signin(context, args.trace,
                                       args.screenshot_dir)
            if args.vnc and args.mode == "none":
                # VNC handoff that's just seeding the profile dir —
                # no walk requested.
                log.info(
                    "VNC handoff complete; persistent profile state "
                    "at %s", args.profile_dir,
                )
                return 0
            since, until = cli.resolve_lookback(args)
            config = {
                "dest": str(args.bronze_dir),
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
            }
            if args.exclude_accounts:
                config["exclude_accounts"] = args.exclude_accounts
            if args.refresh_lots:
                config["refresh_lots"] = "true"
            try:
                coverage = walk(context, page, config)
            finally:
                logout(page, args.screenshot_dir)
            return exit_code_for_coverage(coverage)
        finally:
            debugcap.stop_trace_if_active(
                context, args.trace, args.screenshot_dir, "download", log=log,
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
        help=("Bash-sourced env file. When omitted, falls back to "
              "/secrets/fidelity-web.env then "
              "$HOME/.secrets/fidelity-web.env. Supplies "
              "FIDELITY_WEB_USERNAME / FIDELITY_WEB_PASSWORD (the "
              "unprefixed FIDELITY_* names are a legacy fallback)."),
    )
    p.add_argument(
        "--check", action="store_true",
        help=("Verify the session is alive (navigate to post-auth "
              "landing, no credential submit, no MFA). Exits 0 if "
              "alive, non-zero otherwise. Skips walk + logout."),
    )
    p.add_argument(
        "--vnc", action="store_true",
        help=("Pre-fill credentials, then WAIT for Log In and 2FA to "
              "be completed by hand via VNC. The walk still runs "
              "after the VNC-driven login lands (unless --mode "
              "none)."),
    )
    p.add_argument(
        "--mfa-timeout", type=float, default=300.0,
        help="Seconds to wait for the human login + MFA over VNC. "
             "Default 300 (5 min).",
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
        help=("Seconds to wait for the post-auth landing URL after "
              "a scripted 2FA submit. Default 60."),
    )
    # --- Walk args ---
    p.add_argument(
        "--bronze-dir", type=Path, default=Path("/data"),
        help="Bronze tree root. Default: /data.",
    )
    p.add_argument(
        "--mode", default="all",
        choices=("all", "positions", "activity", "documents",
                 "balances", "performance", "none"),
        help=("Which phase to run. 'all' (default) runs every phase, "
              "including the full Donor-Advised Fund phase when the "
              "login carries one; 'positions' includes the DAF pool "
              "snapshot. Every positions-bearing dump covers both "
              "channels — hence no DAF-only mode (DESIGN.md §12.3). "
              "'none' is for --vnc handoffs that only seed the "
              "profile dir."),
    )
    # Shared date-window contract. Fidelity bisects the activity
    # range into MAX_ACTIVITY_WINDOW_DAYS chunks internally; the statements +
    # tax-forms walk filters at YEAR granularity (row labels are
    # mixed monthly / quarterly / annual).
    cli.add_standard_args(p, verb="download")
    p.add_argument(
        "--exclude-accounts", default=None,
        help="Comma-separated account ids to skip.",
    )
    p.add_argument(
        "--refresh-lots", action="store_true",
        help=("Fetch the lots of every lot-eligible open and closed "
              "position, not only of the new and changed ones, up to "
              "the per-run limit; the runs after it fetch the rest. "
              "The positions phase (--mode positions or all) runs the "
              "lot step; the first run is always a full fetch."),
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
        help="Capture a Playwright trace bundle into --screenshot-dir "
             "(required). A run that signs in writes two: a "
             "'<ts>-signin-trace.zip' with DOM snapshots off, because a "
             "snapshot records every input's value and no redactor "
             "reaches a trace after the fact, and a "
             "'<ts>-download-trace.zip' with them on for the walk, where "
             "they are what makes a trace worth reading. --check types no "
             "credential and writes one bundle, snapshots on. UNREDACTED "
             "either way — never commit one.",
    )
    return p.parse_args(argv)


def _exit_on_sigterm(signum, frame):
    """`docker stop` sends SIGTERM to this process, which runs as PID 1
    in the container, where the default action is to ignore it until
    the SIGKILL that follows. Raising instead unwinds through
    run_oneshot's ``finally``, so an interrupted walk still logs out."""
    raise KeyboardInterrupt(f"signal {signum}")


def main(argv=None):
    signal.signal(signal.SIGTERM, _exit_on_sigterm)
    args = parse_args(argv or sys.argv[1:])
    cli.configure_logging(args.verbose)
    if args.trace and args.screenshot_dir is None:
        log.error("--trace requires --screenshot-dir")
        return 2
    maybe_source_env_files(args)
    if args.check:
        return run_check(args)
    return run_oneshot(args)


if __name__ == "__main__":
    sys.exit(main())
