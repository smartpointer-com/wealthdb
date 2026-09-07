#!/usr/bin/env python3
"""
Schwab client-web scrape helpers.

Module-only — the public entry is `walk(page, dest, ...)`, called
by login.py once it's driven Firefox through the CLI-MFA flow and
the post-auth landing page is ready. No standalone CLI: the
wrapper's `download` subcommand always goes through login.py
because Schwab invalidates the persistent profile's session
within seconds of Firefox closing.

walk() drives two read-only surfaces (CLAUDE.md §1):

  Statements & Tax Forms — enumerate the accounts, apply
  the document-type chip filter, paginate the results, and fetch
  every PDF (Statements / Tax Forms / Letters / Reports & Plans;
  Trade Confirms intentionally skipped).

  Transaction History — drive the Export modal to save CSV / JSON
  / XML of the full tx-history per account; by default (unless
  --no-more-detail), also click each row's "More" link and capture
  the per-row detail modal (Settle Date / CUSIP / Principal /
  Commission / Industry Fee) into a sidecar more-details.json the loader merges into
  silver.

Bronze tree: <bronze-dir>/<UTC-timestamp>/ with one PDF per document
under statements/<account>/ plus a run.json manifest.
"""

from __future__ import annotations

import contextlib
import hashlib
import json
import logging
import os
import re
import signal
import tempfile
import threading
import time
from datetime import date, datetime, timedelta, timezone
from pathlib import Path

import landmarks as schwab
from collectorkit import bronze, debugcap

log = logging.getLogger("schwab-web.download")

# Same as login.py — generous defaults for the heavy SPA.
NAV_TIMEOUT_MS = 60_000
LANDMARK_TIMEOUT_MS = 60_000
# A pagination swap re-fetches the page's rows server-side, which on
# wide date ranges (Last5Years / All) regularly outlives the old 10s
# wait — hence its own, longer budget.
PAGE_SWAP_TIMEOUT_MS = 30_000

# More-detail walk pacing. A modal-heavy page costs ~1-2 min at
# baseline (~25 rows x open/read/close), and a degraded SPA (runaway
# change detection starving every protocol call) can stretch a lap
# several-fold — or, at the extreme, wedge it entirely, which no
# per-call timeout catches because page.evaluate has none. The lap
# deadline is the hard ceiling for that case; the heartbeat keeps a
# slow lap visibly alive; the notice threshold flags degradation
# without treating it as an error.
PAGE_DETAIL_DEADLINE_S = 600
DETAIL_HEARTBEAT_S = 30
SLOW_LAP_NOTICE_S = 180
MAX_DETAIL_RECOVERIES = 2
# Consecutive intercepted More-clicks (with the modal overlay still
# open after a dismissal attempt) before the walk declares the overlay
# wedged and takes the reload-recovery path, instead of burning the
# click timeout on every remaining row until the lap deadline fires.
WEDGED_ROW_FAILS = 3
RECOVERY_DEADLINE_S = 180

# Bronze run-directory naming: <bronze-dir>/<UTC-timestamp>/
RUN_DIR_FMT = "%Y%m%dT%H%M%SZ"

# ============================================================
# Helpers
# ============================================================

def maybe_screenshot(page, screenshot_dir: Path | None, label: str) -> None:
    """HTML + best-effort screenshot. See login.maybe_screenshot
    for the Firefox "fonts never load" rationale."""
    if screenshot_dir is None:
        return
    try:
        screenshot_dir.mkdir(parents=True, exist_ok=True)
    except Exception as e:
        log.warning("could not create screenshot dir %s: %s", screenshot_dir, e)
        return
    ts = bronze.ts_slug()
    try:
        html_path = screenshot_dir / f"{ts}-{label}.html"
        try:
            html = page.content()
        except Exception:
            html = page.evaluate(
                "() => document.documentElement.outerHTML",
            )
        # Scrubbed: a landmark on the sign-in page serializes the form
        # with the typed password in a `value` attribute.
        html_path.write_text(debugcap.scrub_dom(html), encoding="utf-8")
        log.debug("wrote HTML %s", html_path)
    except Exception as e:
        log.warning("html capture %s failed: %s", label, e)
    try:
        png_path = screenshot_dir / f"{ts}-{label}.png"
        page.screenshot(
            path=str(png_path), full_page=False,
            timeout=3_000, animations="disabled",
        )
        log.debug("wrote screenshot %s", png_path)
    except Exception as e:
        log.debug("screenshot %s failed (HTML saved): %s", label, e)

# ============================================================
# Account enumeration
# ============================================================

# Account-suffix-only matcher for sanitising filenames / dir names.
# Schwab renders the dropdown right-column as `<U+2026>NNN`
# (Unicode ellipsis followed by the last-3-to-5 digits of the
# account). Match both that form and the ASCII `...NNN` form
# defensively — copy-paste of the live label can normalise either
# way depending on the rendering path.
_SUFFIX_RE = re.compile(r"(?:…|\.{3})(\d{3,5})")

def _suffix_of(label: str) -> str:
    m = _SUFFIX_RE.search(label)
    if not m:
        # Fall back to a stable hash of the label so we still
        # produce a per-account directory; not pretty, but never
        # collides.
        return "x" + hashlib.sha256(label.encode("utf-8")).hexdigest()[:8]
    return m.group(1)

def enumerate_accounts(page) -> list[dict]:
    """Open the account selector dropdown and read every entry.

    Returns a list of `{"label": <full text>, "suffix": <last3>,
    "entry_id": <DOM id>}` dicts. Closes the dropdown when done.

    The DOM ids follow `account-selector-header-0-account-<N>`
    — stable across the sample HTML drops we've seen. We anchor
    on the prefix and let N grow with the account count.
    """
    log.info("enumerating accounts via account selector")
    selector_button = page.locator(
        f"button.{schwab.ACCOUNT_SELECTOR_BUTTON_CLASS}"
    ).first
    selector_button.wait_for(state="visible", timeout=LANDMARK_TIMEOUT_MS)
    selector_button.click()

    # Wait for at least one account entry to render.
    page.wait_for_selector(
        f'[id^="{schwab.ACCOUNT_SELECTOR_ENTRY_ID_PREFIX}"]',
        timeout=LANDMARK_TIMEOUT_MS,
    )

    entries = page.locator(
        f'[id^="{schwab.ACCOUNT_SELECTOR_ENTRY_ID_PREFIX}"]'
    ).all()
    accounts = []
    for el in entries:
        try:
            eid = el.get_attribute("id") or ""
        except Exception:
            continue
        # The selector matches both the outer container id and
        # its inner -left/-right halves. Pick only the outer
        # container — its id ends in -account-<digit> with no
        # further suffix.
        if not re.fullmatch(
            rf"{re.escape(schwab.ACCOUNT_SELECTOR_ENTRY_ID_PREFIX)}\d+",
            eid,
        ):
            continue
        try:
            label = (el.inner_text() or "").strip()
        except Exception:
            label = ""
        if not label:
            continue
        # Collapse internal whitespace.
        label = re.sub(r"\s+", " ", label)
        suffix = _suffix_of(label)
        accounts.append({"label": label, "suffix": suffix, "entry_id": eid})

    # Close the dropdown.
    selector_button.click()
    log.info("found %d account(s)", len(accounts))
    for a in accounts:
        log.debug("  - %s (…%s, id=%s)", a["label"], a["suffix"], a["entry_id"])
    return accounts

def select_account(page, entry_id: str) -> None:
    """Open the account selector and pick the entry with `entry_id`.

    Starts by dismissing any modal a previous account's walk may have
    leaked (observed live: an Escape-immune wire-details overlay
    intercepted this very click for every account after the first), so
    one account's stuck dialog can never cascade across the rest of
    the loop. The open click is bounded, with a dispatch_event
    fallback if something still intercepts it.

    Uses `dispatch_event('click')` for the entry rather than a
    real pointer click. Schwab can surface an overlay panel (e.g.
    "W-8 Form Status") that lands on top of the dropdown the
    moment it opens, blocking pointer-event delivery to the
    entry beneath. dispatch_event fires the click handler in JS
    directly, sidestepping the z-index race entirely.
    """
    _dismiss_open_modal(page)
    selector_button = page.locator(
        f"button.{schwab.ACCOUNT_SELECTOR_BUTTON_CLASS}"
    ).first
    try:
        selector_button.click(timeout=10_000)
    except Exception as e:
        log.warning("account-selector click intercepted (%s); dismissing "
                    "any overlay and dispatching the click directly",
                    str(e).splitlines()[0])
        # Something demonstrably covers the page — wait for it and
        # dismiss it properly before falling back.
        _dismiss_open_modal(page, expect_modal=True)
        selector_button.dispatch_event("click")
    page.wait_for_selector(f"#{entry_id}", timeout=LANDMARK_TIMEOUT_MS)
    page.locator(f"#{entry_id}").dispatch_event("click")

# ============================================================
# Filter configuration
# ============================================================

def configure_doc_type_filter(page) -> None:
    """Ensure WANTED chips are selected and the rest are not.

    Addresses each chip by its `lookupid` attribute (the user-
    visible text labels live inside the Stencil <sdps-chips>
    shadow DOM, so `:has-text(...)` matches nothing). Reads
    selection state from the `selected` HTML attribute, NOT a CSS
    class — the rendered DOM has no `sdps-chips--selected` class.
    Clicks via dispatch_event
    so an overlay (W-8 banner et al.) can't intercept the toggle.
    """
    chips_container = page.locator(".sdps-chips__container").first
    chips_container.wait_for(state="visible", timeout=LANDMARK_TIMEOUT_MS)

    for label, lookupid in schwab.DOC_TYPES:
        chip = chips_container.locator(
            f'sdps-chips[lookupid="{lookupid}"]'
        ).first
        try:
            sel_attr = chip.get_attribute(schwab.CHIP_SELECTED_ATTR)
        except Exception:
            log.warning("chip %r (lookupid=%s) not found; skipping",
                        label, lookupid)
            continue
        is_selected = sel_attr is not None
        want_selected = label in schwab.DOC_TYPES_WANTED
        if is_selected == want_selected:
            log.debug("chip %r already %s",
                      label, "on" if want_selected else "off")
            continue
        log.info(
            "chip %r: toggling %s → %s",
            label,
            "on" if is_selected else "off",
            "on" if want_selected else "off",
        )
        # Real OS-level click with `force=True`: bypasses Playwright's
        # actionability checks (the Stencil <sdps-chips> host's
        # bounding box can be reported as zero-size during the
        # initial hydration) while still emitting a TRUSTED click
        # event. dispatch_event would emit isTrusted=false, and
        # Schwab's chip handler ignores untrusted clicks.
        chip.click(force=True)

def click_search(page) -> None:
    page.get_by_role(
        "button", name=schwab.SEARCH_BUTTON_TEXT, exact=True,
    ).first.click()

def select_date_range(page, value: str) -> None:
    """Set every `<select id="date-range-select-id">` on the page
    to `value` and fire a change/input event so the SPA picks it
    up.

    Driven via JS evaluate rather than locator.select_option:
    (1) Tx-history's select renders styled-but-hidden inside a
    Stencil wrapper; locator.select_option waits for
    actionability and times out. (2) Some pages have multiple
    selects with the same id (one per filter modal variant);
    setting all matching selects is safer than picking `.first`.
    The native `change`/`input` events bubble out of
    `<sdps-dropdown>` and the SPA's change handler runs as if
    the option was picked from the styled dropdown.

    Accepts either Statements or Tx-history option values — the
    two pages share the select id but expose disjoint option
    sets. See landmarks.DATE_RANGE_VALUES /
    TX_DATE_RANGE_VALUES for the canonical lists.
    """
    valid = set(schwab.DATE_RANGE_VALUES) | set(schwab.TX_DATE_RANGE_VALUES)
    if value not in valid:
        raise ValueError(
            f"unknown date range {value!r}; valid: {sorted(valid)}"
        )
    if value == "SpecifyDateRange":
        # The custom mode needs its two date inputs filled —
        # that's fill_custom_date_range's job. Selecting it bare
        # would leave the filter dateless, so fail loud.
        raise NotImplementedError(
            f"custom date-range mode {value!r} takes dates; "
            "use fill_custom_date_range, or pass a preset"
        )
    n_set = _set_date_range_raw(page, value)
    if not n_set:
        raise RuntimeError(
            f"no <option value={value!r}> found in any "
            f"#{schwab.DATE_RANGE_SELECT_ID}"
        )
    log.debug("date range set to %s on %d <select> element(s)", value, n_set)


def _set_date_range_raw(page, value: str) -> int:
    """JS-set every date-range <select> to `value` (no validation, no
    custom-mode guard) and fire change/input. Returns how many selects
    took the value. select_date_range is the guarded public face; the
    --debug custom-mode probe uses this directly."""
    return page.evaluate(
        """(args) => {
            const els = document.querySelectorAll('#' + args.id);
            let n = 0;
            for (const el of els) {
                const opt = el.querySelector(
                    'option[value="' + args.value + '"]'
                );
                if (!opt) continue;
                // Setting opt.selected is more reliable against
                // Stencil/Angular wrappers than el.value alone —
                // some frameworks debounce or reject value-set,
                // but mark a specific option as selected is the
                // canonical native API.
                opt.selected = true;
                el.value = args.value;
                el.dispatchEvent(new Event('input', { bubbles: true }));
                el.dispatchEvent(new Event('change', { bubbles: true }));
                if (el.value === args.value) n++;
            }
            return n;
        }""",
        {"id": schwab.DATE_RANGE_SELECT_ID, "value": value},
    )


def fill_custom_date_range(page, since, until) -> None:
    """Exact window: select SpecifyDateRange and fill the two
    datepickers the SPA mounts (From then To in DOM order; both
    inner inputs share id "datepicker-input", so selection is
    positional). Dates are typed as mm/dd/yyyy — the format the
    widget's helper text names. Statements and Tx-history share the
    option value and widget; the Search click that follows applies
    the filter, same as the preset path."""
    if not _set_date_range_raw(page, "SpecifyDateRange"):
        raise RuntimeError(
            "date-range select offers no SpecifyDateRange option")
    page.wait_for_function(
        "() => document.querySelectorAll('#datepicker-input').length >= 2",
        timeout=LANDMARK_TIMEOUT_MS,
    )
    inputs = page.locator("#datepicker-input")
    for nth, d in ((0, since), (1, until)):
        el = inputs.nth(nth)
        el.fill(f"{d.month:02d}/{d.day:02d}/{d.year}")
        # Playwright's fill fires input events; Angular's model sync
        # additionally wants a change on blur.
        el.dispatch_event("change")
    log.info("tx-history custom range set: %s..%s", since, until)


def _date_range_facts(page) -> list:
    """Structured snapshot of every date-range filter widget: the
    select's state plus the attributes and values of any input-like
    elements in its enclosing container. Dates and widget metadata
    only — no result rows, no account data."""
    return page.evaluate(
        """(id) => {
            const out = [];
            for (const el of document.querySelectorAll('#' + id)) {
                let wrap = el;
                for (let i = 0; i < 3 && wrap.parentElement; i++)
                    wrap = wrap.parentElement;
                const inputs = [];
                for (const inp of wrap.querySelectorAll(
                        'input, sdps-datepicker, [role="textbox"]')) {
                    inputs.push({
                        tag: inp.tagName.toLowerCase(),
                        id: inp.id || null,
                        name: inp.getAttribute('name'),
                        type: inp.getAttribute('type'),
                        placeholder: inp.getAttribute('placeholder'),
                        ariaLabel: inp.getAttribute('aria-label'),
                        value: inp.value ?? null,
                        visible: !!(inp.offsetWidth || inp.offsetHeight),
                    });
                }
                out.push({
                    selected: el.value,
                    options: Array.from(el.options).map(o => o.value),
                    inputs,
                    containerText:
                        wrap.textContent.replace(/\\s+/g, ' ')
                            .trim().slice(0, 400),
                    containerHtml: wrap.outerHTML,
                });
            }
            // Custom-mode date inputs mount in a sibling subtree, not
            // near the select — sweep the whole page for them too.
            const pickers = [];
            for (const dp of document.querySelectorAll(
                    'sdps-datepicker, #datepicker-input,' +
                    ' input[name="datepicker"]')) {
                pickers.push({
                    tag: dp.tagName.toLowerCase(),
                    id: dp.id || null,
                    value: dp.value ?? null,
                    nearText: (dp.closest('sdps-datepicker')?.parentElement
                               ?? dp.parentElement)
                        ?.textContent.replace(/\\s+/g, ' ')
                        .trim().slice(0, 120) ?? null,
                });
            }
            if (pickers.length)
                out.push({ pageDatepickers: pickers });
            return out;
        }""",
        schwab.DATE_RANGE_SELECT_ID,
    )


_CUSTOM_PROBES_DONE: set = set()


def probe_custom_date_range(page, screenshot_dir, custom_value: str,
                            restore_value: str) -> None:
    """--debug discovery aid for the custom date-range mode (both
    pages share the "SpecifyDateRange" value): momentarily selects it,
    records
    what the SPA mounts (input ids/names/placeholders/values, container
    text, a DOM fragment + screenshot when a screenshot dir is given),
    then restores `restore_value`. Runs once per mode per process,
    before any Search/Apply — the filter is never applied in the custom
    state. Best-effort: failures warn and the walk continues."""
    if custom_value in _CUSTOM_PROBES_DONE:
        return
    _CUSTOM_PROBES_DONE.add(custom_value)
    try:
        before = _date_range_facts(page)
        log.info("date-range-probe[%s] preset facts: %s",
                 restore_value,
                 json.dumps([{k: v for k, v in f.items()
                               if k != "containerHtml"} for f in before]))
        if not _set_date_range_raw(page, custom_value):
            log.info("date-range-probe[%s]: no select offers this option",
                     custom_value)
            return
        page.wait_for_timeout(750)
        after = _date_range_facts(page)
        log.info("date-range-probe[%s] custom facts: %s",
                 custom_value,
                 json.dumps([{k: v for k, v in f.items()
                               if k != "containerHtml"} for f in after]))
        if screenshot_dir is not None:
            try:
                screenshot_dir.mkdir(parents=True, exist_ok=True)
                frag = screenshot_dir / (
                    f"{bronze.ts_slug()}-date-range-{custom_value}"
                    "-fragment.html")
                frag.write_text(
                    "\n\n".join(f["containerHtml"] for f in after
                                if "containerHtml" in f),
                    encoding="utf-8")
                log.info("date-range-probe[%s]: fragment -> %s",
                         custom_value, frag)
            except Exception as e:
                log.warning("date-range-probe[%s]: fragment write "
                            "failed: %s", custom_value, e)
            maybe_screenshot(page, screenshot_dir,
                             f"date-range-{custom_value}")
    except Exception as e:
        log.warning("date-range-probe[%s] failed: %s", custom_value, e)
    finally:
        try:
            _set_date_range_raw(page, restore_value)
            page.wait_for_timeout(250)
        except Exception as e:
            log.warning("date-range-probe[%s]: restore to %r failed: %s",
                        custom_value, restore_value, e)

# ============================================================
# Result-table walking
# ============================================================

# Result-count banner format:
#   "123 document(s) found from 05/19/2016 to 05/19/2026"
# We just need the leading integer.
_COUNT_RE = re.compile(r"^\s*(\d+)\s*document")

def wait_for_results(page) -> int:
    """Wait for the search-result count to appear; return the count."""
    # The "X document(s) found" <p> sits inside
    # app-statements-search-results.
    locator = page.locator(
        "app-statements-search-results p.sdps-text-headline"
    ).first
    locator.wait_for(state="visible", timeout=LANDMARK_TIMEOUT_MS)
    text = locator.inner_text().strip()
    m = _COUNT_RE.search(text)
    if not m:
        log.warning("could not parse result count from %r", text)
        return -1
    return int(m.group(1))

def iter_visible_rows(page):
    """Yield one dict per row visible on the current results page.

    Cells (in this order): Date, Type, Account, Document, Inserts,
    Download. We extract the text of the first four and enumerate
    every download button in the Download cell — Schwab exposes
    one per available format (PDF for all, +XML +CSV for 1099
    Composite et al.), all sharing the
    `Click to Download <FORMAT>` aria-label prefix.
    """
    rows = page.locator(schwab.RESULT_ROW_SELECTOR).all()
    for row in rows:
        cells = row.locator("sdps-table-cell").all()
        if len(cells) < 6:
            continue
        try:
            date_str = cells[0].inner_text().strip()
            type_str = cells[1].inner_text().strip()
            # Account cell contains both nickname and …NNN suffix
            # as two <p>s; collapse whitespace.
            acct_str = re.sub(r"\s+", " ", cells[2].inner_text().strip())
            # Document cell contains the document name button.
            doc_str = cells[3].inner_text().strip()
            download_buttons = []
            for btn in cells[5].locator(
                f'button[aria-label^="{schwab.DOWNLOAD_ARIA_PREFIX}"]'
            ).all():
                label = btn.get_attribute("aria-label") or ""
                fmt = label[len(schwab.DOWNLOAD_ARIA_PREFIX):].strip().lower()
                if fmt:
                    download_buttons.append((fmt, btn))
        except Exception as e:
            log.warning("could not parse a row: %s", e)
            continue
        if not download_buttons:
            log.warning("row has no Download cell buttons: %s | %s | %s",
                        date_str, type_str, doc_str)
            continue
        yield {
            "date": date_str,
            "type": type_str,
            "account": acct_str,
            "document": doc_str,
            "download_buttons": download_buttons,  # [(fmt, locator), ...]
        }

def click_next_page(page, pagination_id: str) -> bool:
    """Advance to the next page of results.

    Targets the NUMBERED page link (`#pagination-{N+1}-link`)
    at document scope, ignoring the outer pagination host's id.
    Anchoring on the `<sdps-pagination id="…">` host breaks on
    Statements: Schwab renders it as `id="document-pagination"`
    on page 1 but **renames it to `id="pagination"`** after the
    first click (the same id tx-history uses). The inner
    `#pagination-N-link` ids are stable across the rename, so we
    query for them directly and skip the host-element lookup
    entirely. `pagination_id` is kept only for diagnostic logging.

    Returns True after a click; False when we're on the last
    page (no `#pagination-{N+1}-link` exists).
    """
    # Read the currently-selected page number from
    # `<a aria-current="page" id="pagination-N-link">`.
    n = _current_page_num(page)
    if n is None:
        log.info("click_next_page (%s): no aria-current page marker "
                 "— single-page result set or no pagination",
                 pagination_id)
        return False
    next_n = n + 1
    next_link = page.locator(f"#pagination-{next_n}-link").first
    if next_link.count() == 0:
        # Schwab truncates visible numbered links (1, 2, 3, Next)
        # — on page 3 there's no #pagination-4-link rendered yet
        # even though more pages exist. Fall back to the "Next"
        # stepper, whose id is stable and visible whenever
        # there's a next page.
        next_link = page.locator(
            f"#{schwab.PAGINATION_NEXT_LINK_ID}"
        ).first
        if next_link.count() == 0:
            log.info(
                "click_next_page: neither #pagination-%d-link nor "
                "#%s present — last page",
                next_n, schwab.PAGINATION_NEXT_LINK_ID,
            )
            return False
        # Confirm the Next stepper isn't display-hidden (the
        # parent <li> gets `sdps-hide` on the actual last page).
        try:
            hidden = next_link.evaluate(
                "(el, cls) => el.closest('li')?.classList.contains(cls) ?? false",
                schwab.PAGINATION_HIDDEN_LI_CLASS,
            )
        except Exception:
            hidden = False
        if hidden:
            log.info("click_next_page: Next stepper hidden — last page")
            return False
        log.debug("click_next_page: falling back to Next stepper for page %d",
                  next_n)
    # Hit-test guard: a forced click still goes to the topmost
    # element at the link's coordinates, so a lingering modal
    # overlay (see _dismiss_open_modal) silently eats the flip.
    # Name the covering element, dismiss, and re-check once —
    # the named element is the diagnostic when flips still fail.
    try:
        next_link.scroll_into_view_if_needed(timeout=5_000)
    except Exception:
        pass
    try:
        covering = next_link.evaluate(
            """el => {
                const r = el.getBoundingClientRect();
                const top = document.elementFromPoint(
                    r.x + r.width / 2, r.y + r.height / 2);
                if (!top || el.contains(top) || top.contains(el))
                    return null;
                const c = top.closest(
                    '[class*="overlay"], [role="dialog"]') || top;
                return (c.tagName + ' ' + (c.id || '') + ' '
                        + (c.className || '')).trim().slice(0, 120);
            }"""
        )
    except Exception:
        covering = None
    if covering:
        # A routine, handled condition (the More-detail modal's
        # overlay re-shows between the row loop and the flip);
        # WARNING is reserved for a dismissal that fails or a page
        # that never advances.
        log.info("click_next_page: pagination link covered by %r; "
                 "dismissing before the click", covering)
        _dismiss_open_modal(page)
    # Real click with force=True: bypasses Playwright's
    # actionability waits (the anchor's bounding rect can flicker
    # during the Angular page swap) while still emitting a
    # TRUSTED click. A click that lands but produces no swap gets
    # one retry (an overlay mid-animation or a re-render race can
    # eat one); a still-stuck page returns False so the caller
    # stops instead of re-scraping the page it is already on.
    for attempt in (1, 2):
        try:
            next_link.click(force=True, timeout=10_000)
        except Exception as e:
            log.warning("click_next_page: click on page %d link failed: %s",
                        next_n, e)
            return False
        # Wait for the new page to become current — guards against
        # the next iteration reading the old page's already-detached
        # rows. Polled via the id ATTRIBUTE: the refreshed marker can
        # sit display-hidden inside the Stencil wrapper, where a
        # visibility wait times out even though the swap succeeded.
        try:
            page.wait_for_function(
                """(nid) => document.querySelector(
                       'a[aria-current="page"][id^="pagination-"]'
                   )?.id === nid""",
                arg=f"pagination-{next_n}-link",
                timeout=PAGE_SWAP_TIMEOUT_MS,
            )
            return True
        except Exception:
            pass
        landed = _current_page_num(page)
        if landed == next_n:
            return True
        if attempt == 1:
            log.info("click_next_page: still on page %s after clicking "
                     "for page %d; retrying once", landed, next_n)
    log.warning(
        "click_next_page: page %d never became current (still on %s) — "
        "stopping this walk rather than re-scraping the current page",
        next_n, landed,
    )
    return False


def _current_page_num(page) -> int | None:
    """Page number from the `<a aria-current="page"
    id="pagination-N-link">` marker, or None when no pagination (or an
    unrecognised id scheme) is rendered. Attribute reads only — the
    marker can be display-hidden inside the Stencil wrapper."""
    current = page.locator(
        'a[aria-current="page"][id^="pagination-"]'
    ).first
    if current.count() == 0:
        return None
    try:
        cid = current.get_attribute("id") or ""
    except Exception:
        cid = ""
    m = re.match(r"pagination-(\d+)-link", cid)
    return int(m.group(1)) if m else None

# ============================================================
# Per-document download
# ============================================================

# Filename-safe replacement for the parts we want to embed.
_FN_SAFE = re.compile(r"[^A-Za-z0-9._-]+")

def _safe(s: str, fallback: str = "x") -> str:
    out = _FN_SAFE.sub("-", s).strip("-")
    return out or fallback

def _overlay_open(page) -> bool:
    """Whether a modal overlay or dialog is up AND visible, asked
    through Playwright's selector engine — the same view its
    hit-testing uses. A raw document.querySelector probe can disagree
    with hit-testing on this SPA; one such disagreement reported "all
    clear" milliseconds before an in-flight dialog materialized and ate
    a click. Visibility matters on both branches: sdps can leave the
    `--open` class on an overlay it has already hidden (the cleanup
    rides the close animation, which the lagging SPA can drop), and a
    hidden overlay intercepts nothing — counting it only produces
    cry-wolf "still open" warnings."""
    try:
        if page.locator(".sdps-modal__overlay--open:visible").count():
            return True
        return bool(page.locator('[role="dialog"]:visible').count())
    except Exception:
        return False


def _modal_gone_settled(page, settle_s: float = 0.7) -> bool:
    """Clean now AND still clean after a settle. A single instant probe
    races the SPA's laggy event queue, where a dialog can materialize
    seconds after the click that opened it."""
    if _overlay_open(page):
        return False
    time.sleep(settle_s)
    return not _overlay_open(page)


def _capture_dialog_html(page, screenshot_dir: Path | None) -> None:
    """Dump the visible dialog's outerHTML to the debug dir — ground
    truth for refining MODAL_CLOSE_SELECTORS when a dismissal fails.
    Lands OUTSIDE bronze under the screenshots' NEVER-commit contract;
    a no-op without --screenshot-dir."""
    if screenshot_dir is None:
        return
    try:
        html = page.locator('[role="dialog"]:visible').first.evaluate(
            "el => el.outerHTML")
        screenshot_dir.mkdir(parents=True, exist_ok=True)
        path = screenshot_dir / f"{bronze.ts_slug()}-wedged-dialog.html"
        path.write_text(html, encoding="utf-8")
        log.info("wedged dialog DOM captured to %s", path)
    except Exception as e:
        log.debug("wedged-dialog capture failed: %s", e)


def _click_modal_close_control(page) -> bool:
    """Click the open dialog's dismiss-only close control ("X" /
    Close), trying each MODAL_CLOSE_SELECTORS candidate. Returns True
    once one was clicked. Never touches OK / Continue / action buttons
    — on an unknown dialog those could confirm an action (read-only
    contract, CLAUDE.md §1)."""
    for sel in schwab.MODAL_CLOSE_SELECTORS:
        try:
            btn = page.locator(sel).first
            if btn.count() == 0 or not btn.is_visible(timeout=300):
                continue
            btn.click(timeout=3_000)
            log.info("dismissed modal via its close control (%s)", sel)
            return True
        except Exception as e:
            log.debug("modal close control %s failed: %s", sel, e)
    return False


def _dismiss_open_modal(page, expect_modal: bool = False) -> None:
    """Dismiss any open Schwab modal and wait until it is verifiably
    gone.

    sdps-modal honors Escape per WAI-ARIA dialog conventions, but its
    `.sdps-modal__overlay--open` (z-index 101003) outlives the dialog
    for the close animation, some variants ignore Escape outright
    (observed live: the wire-details modal), and the SPA under load
    applies queued input late — a dialog can materialize seconds after
    the click that opened it, so an instant "nothing open" probe is a
    race, not a verdict (one such race left a dialog standing through
    an account's whole export phase). Hence: every clean verdict is
    double-checked after a settle, `expect_modal` callers — who just
    opened or clicked into a dialog — wait for it to materialize
    first, and dismissal escalates from Escape probes to the dialog's
    own dismiss-only close control before warning.
    """
    if expect_modal:
        try:
            page.locator('[role="dialog"]:visible').first.wait_for(
                state="visible", timeout=3_000)
        except Exception:
            pass  # never materialized (or came and went) — probed below
    for _ in range(3):
        if _modal_gone_settled(page):
            return
        try:
            page.keyboard.press("Escape")
        except Exception:
            pass
        time.sleep(0.5)
    # Escape didn't clear it: click the dialog's close control, then
    # give the close animation a bounded grace either way.
    clicked = _click_modal_close_control(page)
    deadline = time.monotonic() + 3.0
    while time.monotonic() < deadline:
        if _modal_gone_settled(page):
            return
        time.sleep(0.3)
    log.warning("modal overlay still open after repeated Escape%s — "
                "the next click may be intercepted",
                " and its close control" if clicked
                else " (no close control found)")

def _confirm_export_tax_modal(page) -> None:
    """If Schwab popped its "Export Tax Data" confirmation modal,
    click the green "Download" button to release the actual file.
    Best-effort: if the modal never appears (the download was a
    PDF, or Schwab dropped the gate), silently return.

    The modal is a `[role="dialog"]` containing the literal text
    "Export Tax Data"; we scope the Download-button lookup to it
    so we don't collide with the "Download" column header in the
    results table.
    """
    try:
        modal = page.locator(
            '[role="dialog"]:has-text("Export Tax Data")'
        ).first
        modal.wait_for(state="visible", timeout=3_000)
    except Exception:
        return
    try:
        modal.get_by_role(
            "button", name="Download", exact=True,
        ).first.click()
        log.debug("dismissed Export Tax Data modal")
    except Exception as e:
        log.warning("failed to click Download on tax-data modal: %s", e)

# Per-format retry policy. Schwab occasionally fails the first
# download click — the inline "Download" link briefly shows a
# "Download failed" pill but stays clickable, and the second
# click usually goes through. Three attempts with a 2-second
# breather covers what's been observed; if it's still failing
# at attempt 3, the row gets logged + skipped at the caller.
DOWNLOAD_ATTEMPT_TIMEOUT_MS = 30_000
DOWNLOAD_MAX_ATTEMPTS = 3
DOWNLOAD_RETRY_PAUSE_MS = 2_000

def _fetch_one_format(page, button, fmt: str, target_dir: Path,
                      is_tax_form: bool):
    """Click the format's download button once, capture the
    resulting download, and write it to disk under a collision-
    safe filename. Returns the on-disk Path. Raises on timeout
    (caller retries)."""
    with page.expect_download(timeout=DOWNLOAD_ATTEMPT_TIMEOUT_MS) as dl_info:
        button.click()
        # Tax-form downloads pop an "Export Tax Data"
        # confirmation modal — observed on XML, CSV, AND PDF
        # variants of Year-End Summary / 1099 Composite. Gating
        # by row type rather than per-format keeps the ~3s wait
        # off the non-tax hot path.
        if is_tax_form:
            _confirm_export_tax_modal(page)
    download = dl_info.value
    suggested = download.suggested_filename or f"document.{fmt}"
    suggested_safe = _safe(suggested, f"document.{fmt}")
    target = target_dir / suggested_safe
    if target.exists():
        stem = target.stem
        ext = target.suffix
        i = 2
        while (target.parent / f"{stem}.{i}{ext}").exists():
            i += 1
        target = target.parent / f"{stem}.{i}{ext}"
    download.save_as(str(target))
    return target

def download_one(page, row: dict, target_dir: Path) -> list[dict]:
    """Fetch every available format for `row` into target_dir.

    Returns one manifest entry per file successfully written:
    `{"date","type","document","format","filename","size","sha256"}`.
    The on-disk filename is the Schwab-supplied download name
    verbatim (it already includes the account suffix and document
    period), suffixed `.N` on re-run collisions.

    Tax forms like the 1099 Composite expose PDF + XML + CSV;
    every other doc type exposes PDF only. The format is read
    from the button's `aria-label` ("Click to Download <FORMAT>"),
    not inferred from the filename, because the PDF/XML/CSV
    variants of the same 1099 sometimes share a stem.

    Each format-button click is retried up to
    DOWNLOAD_MAX_ATTEMPTS times — observed: Schwab silently fails
    the first click with no error toast roughly 5% of the time,
    and a re-click on the same button succeeds. If all retries
    are exhausted, the format is skipped (logged) and the loop
    continues to the next format / row.
    """

    target_dir.mkdir(parents=True, exist_ok=True)
    entries: list[dict] = []
    is_tax_form = row.get("type") == "Tax Forms"
    for fmt, button in row["download_buttons"]:
        target = None
        for attempt in range(1, DOWNLOAD_MAX_ATTEMPTS + 1):
            try:
                target = _fetch_one_format(
                    page, button, fmt, target_dir, is_tax_form,
                )
                break
            except Exception as e:
                if attempt < DOWNLOAD_MAX_ATTEMPTS:
                    log.warning(
                        "download attempt %d/%d failed for "
                        "%s/%s/%s [.%s]: %s — retrying",
                        attempt, DOWNLOAD_MAX_ATTEMPTS,
                        row.get("date"), row.get("type"),
                        row.get("document"), fmt, e,
                    )
                    page.wait_for_timeout(DOWNLOAD_RETRY_PAUSE_MS)
                else:
                    log.error(
                        "download FAILED after %d attempts for "
                        "%s/%s/%s [.%s]: %s — skipping",
                        DOWNLOAD_MAX_ATTEMPTS,
                        row.get("date"), row.get("type"),
                        row.get("document"), fmt, e,
                    )
        if target is None:
            continue

        h = hashlib.sha256()
        size = 0
        with target.open("rb") as fh:
            for chunk in iter(lambda: fh.read(1024 * 64), b""):
                h.update(chunk)
                size += len(chunk)
        # Schwab has been observed serving a CSV from a row whose
        # download button was labelled "PDF" (one known case: a
        # stray 1099 Composite CSV slotted into a Brokerage
        # Statement row). When that happens, trust the actual
        # file extension over the button label so the manifest
        # doesn't lie to the loader.
        actual_ext = target.suffix.lstrip(".").lower()
        if actual_ext and actual_ext != fmt:
            log.warning(
                "row %s/%s/%s aria-label said %r but downloaded "
                "as %s; recording the actual extension",
                row.get("date"), row.get("type"),
                row.get("document"), fmt, target.name,
            )
            recorded_fmt = actual_ext
        else:
            recorded_fmt = fmt
        entries.append({
            "date": row["date"],
            "type": row["type"],
            "document": row["document"],
            "format": recorded_fmt,
            "filename": target.name,
            "size": size,
            "sha256": h.hexdigest(),
        })
        # Tiny breather between same-row formats.
        page.wait_for_timeout(100)
    return entries

# ============================================================
# Per-account download
# ============================================================

def download_account(page, account: dict, dest_dir: Path,
                     dry_run: bool,
                     screenshot_dir: Path | None,
                     date_range: str,
                     debug: bool = False,
                     exact_window: tuple | None = None) -> dict:
    """Walk every page of results for `account`, downloading each
    document into <dest_dir>/statements/<suffix>/. Returns a
    per-account summary for the run manifest.
    """
    log.info("=== account: %s (…%s) ===", account["label"], account["suffix"])
    select_account(page, account["entry_id"])
    # Give the page time to refresh after selection.
    page.wait_for_load_state("domcontentloaded", timeout=NAV_TIMEOUT_MS)
    page.wait_for_timeout(1500)

    configure_doc_type_filter(page)
    custom_set = False
    if exact_window is not None:
        try:
            fill_custom_date_range(page, exact_window[0], exact_window[1])
            custom_set = True
        except Exception as e:
            log.warning("statements …%s: custom range failed (%s); "
                        "falling back to preset %r", account["suffix"],
                        e, date_range)
    if not custom_set:
        select_date_range(page, date_range)
        if debug:
            probe_custom_date_range(page, screenshot_dir,
                                    "SpecifyDateRange", date_range)
    click_search(page)

    expected = wait_for_results(page)
    log.info(
        "search returned %d document(s) for …%s",
        expected, account["suffix"],
    )
    maybe_screenshot(
        page, screenshot_dir,
        f"statements-{account['suffix']}-results",
    )

    if dry_run:
        # In dry-run mode we still walk pages so we can verify the
        # row parser, but we never click a download button.
        rows_seen = 0
        while True:
            for row in iter_visible_rows(page):
                rows_seen += 1
                log.debug(
                    "row: %s | %s | %s | %s",
                    row["date"], row["type"], row["account"], row["document"],
                )
            if not click_next_page(page, schwab.PAGINATION_ELEMENT_ID):
                break
            page.wait_for_timeout(500)
        log.info("dry-run: would have downloaded %d / %d docs", rows_seen, expected)
        return {
            "label": account["label"],
            "suffix": account["suffix"],
            "expected": expected,
            "downloaded": 0,
            "skipped": rows_seen,
            "documents": [],
        }

    account_dir = dest_dir / "statements" / account["suffix"]
    documents: list[dict] = []
    rows_downloaded = 0
    rows_processed = 0
    while True:
        # Snapshot the rows on this page; clicking next mutates the DOM.
        rows = list(iter_visible_rows(page))
        for row in rows:
            rows_processed += 1
            try:
                entries = download_one(page, row, account_dir)
            except Exception as e:
                log.error(
                    "download failed for %s/%s/%s: %s",
                    row["date"], row["type"], row["document"], e,
                )
                continue
            rows_downloaded += 1
            documents.extend(entries)
            if rows_downloaded % 25 == 0:
                log.info(
                    "  …%s progress: %d/%d rows (%d files)",
                    account["suffix"], rows_downloaded, expected,
                    len(documents),
                )
            # Tiny breather to keep Schwab happy — they have no
            # published per-document rate limit but we shouldn't
            # hammer.
            page.wait_for_timeout(150)
        if not click_next_page(page, schwab.PAGINATION_ELEMENT_ID):
            break
        page.wait_for_load_state("domcontentloaded", timeout=NAV_TIMEOUT_MS)
        page.wait_for_timeout(800)

    log.info(
        "account …%s done: %d rows downloaded (%d files), "
        "%d rows seen, %d expected",
        account["suffix"], rows_downloaded, len(documents),
        rows_processed, expected,
    )
    return {
        "label": account["label"],
        "suffix": account["suffix"],
        "expected": expected,
        "downloaded": rows_downloaded,
        "files": len(documents),
        "skipped": rows_processed - rows_downloaded,
        "documents": documents,
    }

# ============================================================
# Transaction History export-modal driver
# ============================================================

def _export_tx_history(page, account_suffix: str, out_dir: Path) -> list[dict]:
    """Drive the "Export Transactions Data" modal and save one
    file per format under `out_dir`. Returns one manifest entry
    per saved file (`{format, filename, size, sha256}`).

    Tx-history's results table is virtualized — only a handful
    of rendered rows are in the DOM at any moment, even on a
    paginated view. The Export modal gives the FULL set in one
    machine-readable file per format. All three carry the same
    row set; load ingests the JSON (it adds AcctgRuleCd), CSV/XML
    are kept as opaque documents. None carry the per-row "More"
    modal extras — those come from the default-on more-detail
    scrape.
    """

    entries: list[dict] = []
    out_dir.mkdir(parents=True, exist_ok=True)
    # A leftover overlay from an earlier step would silently eat the
    # Export click (observed live: a wedged wire-details overlay cost a
    # run every export after the first account) — clear the field first.
    _dismiss_open_modal(page)
    for fmt_label, radio_id, ext in schwab.TX_EXPORT_FORMATS:
        # Open the Export modal. The main-page Export button has
        # the same text as the modal's Export button, so scope by
        # visibility: when the modal is closed, only one Export
        # button is visible (the main-page one).
        if not _click_visible_export_button(page, in_modal=False):
            log.warning("tx-history …%s: main Export button not visible",
                        account_suffix)
            break
        # Wait for the modal.
        modal = page.locator(
            f'[role="dialog"][aria-labelledby="export-modal-modal-title"]'
        )
        try:
            modal.wait_for(state="visible", timeout=10_000)
        except Exception as e:
            log.warning("tx-history …%s: export modal did not open: %s",
                        account_suffix, e)
            break
        # Explicitly JS-set the target radio for EVERY format
        # (including CSV). Schwab's modal state leaks across
        # accounts: after one account's XML download, the next
        # account's modal opens with XML still checked, so a
        # naive "rely on CSV default" produces back-to-back
        # XML downloads. Setting `.checked = true` + dispatching
        # `change` reaches Stencil's <sdps-list-item-selectable>
        # host handler the same way a real click would — and
        # works even when the radio is scrolled out of view (the
        # case for the lower options under list-virtualisation).
        ok = page.evaluate(
            """(rid) => {
                const el = document.getElementById(rid);
                if (!el) return false;
                el.checked = true;
                el.dispatchEvent(new Event('input', { bubbles: true }));
                el.dispatchEvent(new Event('change', { bubbles: true }));
                return el.checked === true;
            }""",
            radio_id,
        )
        if not ok:
            log.warning(
                "tx-history …%s: could not select %s radio",
                account_suffix, radio_id,
            )
            _dismiss_open_modal(page)
            continue
        # Modal submit. Scoping by-role inside the modal makes
        # sure we don't hit the main-page Export button which is
        # also still in DOM beneath the overlay.
        target = None
        for attempt in range(1, DOWNLOAD_MAX_ATTEMPTS + 1):
            try:
                with page.expect_download(
                    timeout=DOWNLOAD_ATTEMPT_TIMEOUT_MS,
                ) as dl_info:
                    modal.get_by_role(
                        "button", name="Export", exact=True,
                    ).first.click()
                download = dl_info.value
                suggested = download.suggested_filename or f"transactions.{ext}"
                suggested_safe = _safe(suggested, f"transactions.{ext}")
                target = out_dir / suggested_safe
                if target.exists():
                    stem = target.stem
                    sfx = target.suffix
                    i = 2
                    while (target.parent / f"{stem}.{i}{sfx}").exists():
                        i += 1
                    target = target.parent / f"{stem}.{i}{sfx}"
                download.save_as(str(target))
                break
            except Exception as e:
                if attempt < DOWNLOAD_MAX_ATTEMPTS:
                    log.warning(
                        "tx-history …%s [.%s] attempt %d/%d failed: %s — retrying",
                        account_suffix, ext, attempt,
                        DOWNLOAD_MAX_ATTEMPTS, e,
                    )
                    page.wait_for_timeout(DOWNLOAD_RETRY_PAUSE_MS)
                else:
                    log.error(
                        "tx-history …%s [.%s] FAILED after %d attempts: %s",
                        account_suffix, ext,
                        DOWNLOAD_MAX_ATTEMPTS, e,
                    )
        # Make sure the modal is closed before the next format
        # iteration (or before the caller proceeds). Some Schwab
        # flows close it on download; others don't.
        _dismiss_open_modal(page)

        if target is None:
            continue

        h = hashlib.sha256()
        size = 0
        with target.open("rb") as fh:
            for chunk in iter(lambda: fh.read(1024 * 64), b""):
                h.update(chunk)
                size += len(chunk)
        entries.append({
            "format": ext,
            "filename": target.name,
            "size": size,
            "sha256": h.hexdigest(),
        })
        page.wait_for_timeout(300)
    return entries

class _WalkStalled(RuntimeError):
    """A watchdog deadline expired inside a page walk."""


@contextlib.contextmanager
def _deadline(seconds: int, label: str):
    """SIGALRM watchdog: raises _WalkStalled out of whatever call is
    executing when `seconds` elapse — the only reliable escape from a
    protocol call queued behind a busy page main thread, since
    page.evaluate has no driver-side timeout at all. Real signals
    only fire on the main thread; elsewhere (and on platforms
    without SIGALRM) this is a no-op passthrough."""
    if (not hasattr(signal, "SIGALRM")
            or threading.current_thread() is not threading.main_thread()):
        yield
        return

    def _fire(signum, frame):
        raise _WalkStalled(label)

    prev = signal.signal(signal.SIGALRM, _fire)
    signal.alarm(seconds)
    try:
        yield
    finally:
        signal.alarm(0)
        signal.signal(signal.SIGALRM, prev)


def _recover_tx_page(page, reapply) -> bool:
    """Reload the tx-history page to shed a degraded SPA and
    re-establish the walk's context via `reapply` (account selection,
    date filter, Search). Runs under its own deadline — recovery on a
    wedged page can block exactly like the walk it rescues. Returns
    False when recovery itself fails."""
    try:
        with _deadline(RECOVERY_DEADLINE_S, "tx-history recovery"):
            page.reload(wait_until="domcontentloaded",
                        timeout=NAV_TIMEOUT_MS)
            page.wait_for_timeout(2000)
            reapply()
        return True
    except Exception as e:
        log.warning("tx-history recovery failed: %s", e)
        return False


def _scroll_tx_table(page) -> None:
    """Scroll the tx-history table's scrollable parent to the
    bottom repeatedly to force virtualised rows into the DOM.
    Stops when the table's scrollHeight stops growing.
    """
    last_height = -1
    for _ in range(40):
        new_height = page.evaluate(
            """() => {
                const row = document.querySelector(
                    'sdps-table-row.sdps-tables__row--body'
                );
                if (!row) return 0;
                let el = row.parentElement;
                while (el && el !== document.body) {
                    const s = window.getComputedStyle(el);
                    if (s.overflowY === 'auto' || s.overflowY === 'scroll') {
                        el.scrollTop = el.scrollHeight;
                        return el.scrollHeight;
                    }
                    el = el.parentElement;
                }
                window.scrollTo(0, document.body.scrollHeight);
                return document.body.scrollHeight;
            }"""
        )
        if new_height == last_height:
            return
        last_height = new_height
        page.wait_for_timeout(300)

def _extract_tx_row_cells(row) -> list[str] | None:
    """Return the (Date, Amount, Description, Symbol, Action)
    tuple from a tx-history row's cells. Mirrors the field order
    used by load._tx_history_row_key so detail records key the
    same way during the silver merge.
    """
    cells = row.locator("sdps-table-cell").all()
    if len(cells) < 8:
        return None
    texts = []
    for c in cells:
        try:
            texts.append(c.inner_text().strip())
        except Exception:
            texts.append("")
    return [
        texts[0],  # Date
        texts[7],  # Amount
        texts[3],  # Description
        texts[2],  # Symbol
        texts[1],  # Action / Transaction Type
    ]

def _parse_more_modal_text(text: str) -> dict:
    """Parse Schwab's "More"-modal inner text into a flat
    key→value dict. Lines look like::

        Trade Date            05/25/2022
        Settle Date           05/26/2022
        Action                Sell to Open

    i.e. a label column then a value column, separated by 2+
    spaces. We skip section headers and the modal chrome.
    """
    skip = {
        "Print",
        "Transactions",
        "Trade Details",
        "Transactions Trade Details",
        "Trade Transaction Details",
        "Deposit Details",
        "Check Details",
        "Check Image Details",
        "Money Market Activity Details",
    }
    fields: dict[str, str] = {}
    for line in text.split("\n"):
        line = line.strip()
        if not line or line in skip:
            continue
        m = re.match(r"^(.+?)\s{2,}(.+)$", line)
        if m:
            key = m.group(1).strip()
            val = m.group(2).strip()
            if key:
                fields[key] = val
    return fields

# Rows whose "More" opens the wire-details dialog. That modal
# lazy-loads its content, ignores Escape, and once wedged a whole run
# (its leaked overlay intercepted every later click); it also carries
# none of the securities enrichment fields (Settle Date / CUSIP /
# Principal / Commission) the More walk exists for. Matched on the
# row's cell text and skipped outright.
_WIRE_ROW_RE = re.compile(r"\bwire\b", re.I)


def _scrape_more_details(page, account_suffix: str,
                         reapply=None,
                         screenshot_dir: Path | None = None) -> list[dict]:
    """Walk every pagination page of the tx-history table,
    scroll-load all virtualised rows, click each row's "More"
    link, capture the per-row detail modal contents.

    Returns a list of `{row_key, row_cells, fields, raw_text}`
    dicts where `row_key` matches load._tx_history_row_key so
    silver can merge details by key without per-source ordering.

    Best-effort throughout: rows without a "More" link are
    silently skipped (Dividend / Interest rows typically have
    no More link), wire rows are skipped by design (_WIRE_ROW_RE),
    modal-open failures retry once then move on, and a per-row
    exception doesn't abort the per-account scrape. An intercepted
    More-click attempts a dismissal and drops the row from
    `seen_keys` so a recovery pass can retry it; WEDGED_ROW_FAILS
    consecutive interceptions with the overlay still standing
    capture the dialog's DOM (ground truth for the close-control
    selectors) and take the reload-recovery path immediately.

    Each page lap runs under a hard SIGALRM deadline: the SPA can
    degrade into a busy loop that starves every protocol call, and
    a wedged lap would otherwise block forever with nothing
    logged. On expiry the walk keeps the details it has and, given
    a `reapply` callback, reloads the page to shed the degraded
    SPA and restarts from page 1 — `seen_keys` makes the replay of
    already-scraped pages cheap (no modal is reopened). A
    heartbeat line lands every ~30s so a slow page is visibly slow
    rather than silent.
    """

    details: list[dict] = []
    seen_keys: set[str] = set()
    page_n = 0
    recoveries = 0
    wire_skips = 0
    click_fails = 0
    while True:
        page_n += 1
        log.info("more-detail …%s: scraping page %d", account_suffix, page_n)
        lap_start = last_beat = time.monotonic()
        advanced = False
        try:
            with _deadline(PAGE_DETAIL_DEADLINE_S,
                           f"more-detail page {page_n} exceeded "
                           f"{PAGE_DETAIL_DEADLINE_S}s"):
                _scroll_tx_table(page)
                rows = page.locator(schwab.TX_ROW_SELECTOR).all()
                log.debug("more-detail …%s pg %d: %d rendered rows",
                          account_suffix, page_n, len(rows))
                for row_idx, row in enumerate(rows):
                    if time.monotonic() - last_beat > DETAIL_HEARTBEAT_S:
                        last_beat = time.monotonic()
                        log.info("more-detail …%s pg %d: row %d/%d, "
                                 "%d detail(s) so far", account_suffix,
                                 page_n, row_idx, len(rows), len(details))
                    cells = _extract_tx_row_cells(row)
                    if not cells:
                        continue
                    row_key = hashlib.sha256(
                        "|".join(cells).encode("utf-8")
                    ).hexdigest()[:16]
                    if row_key in seen_keys:
                        continue
                    seen_keys.add(row_key)
                    if _WIRE_ROW_RE.search(" ".join(cells)):
                        wire_skips += 1
                        continue
                    more_btn = row.locator(
                        'a:has-text("More"), button:has-text("More")'
                    ).first
                    if more_btn.count() == 0:
                        continue
                    try:
                        more_btn.click(timeout=5_000)
                    except Exception as e:
                        log.debug("more-detail row %d click failed: %s",
                                  row_idx, e)
                        # Usually an overlay interception: dismiss, and
                        # drop the row so a recovery pass retries it.
                        seen_keys.discard(row_key)
                        click_fails += 1
                        _dismiss_open_modal(page)
                        if (click_fails >= WEDGED_ROW_FAILS
                                and _overlay_open(page)):
                            _capture_dialog_html(page, screenshot_dir)
                            raise _WalkStalled(
                                f"modal overlay wedged ({click_fails} "
                                f"consecutive More clicks intercepted)")
                        continue
                    click_fails = 0
                    try:
                        # Playwright 1.49's Locator.filter() doesn't take
                        # a `visible` kwarg — that's a newer-version API.
                        # Use the Playwright-specific `:visible` CSS
                        # extension so we still pick only the currently-
                        # displayed dialog (a previously-dismissed modal
                        # may still be in the DOM, just `display:none`).
                        modal = page.locator('[role="dialog"]:visible').first
                        modal.wait_for(state="visible", timeout=5_000)
                        raw_text = modal.inner_text()
                        fields = _parse_more_modal_text(raw_text)
                        details.append({
                            "row_key": row_key,
                            "row_cells": cells,
                            "fields": fields,
                            "raw_text": raw_text,
                        })
                    except Exception as e:
                        log.warning("more-detail row %d capture failed: %s",
                                    row_idx, e)
                    finally:
                        # The More click landed, so a dialog is coming
                        # even if it has not painted yet.
                        _dismiss_open_modal(page, expect_modal=True)
                advanced = click_next_page(
                    page, schwab.TX_PAGINATION_ELEMENT_ID)
                if advanced:
                    page.wait_for_load_state("domcontentloaded",
                                             timeout=NAV_TIMEOUT_MS)
                    page.wait_for_timeout(800)
        except _WalkStalled as stall:
            recoveries += 1
            recover = (reapply is not None
                       and recoveries <= MAX_DETAIL_RECOVERIES)
            log.warning(
                "more-detail …%s: %s — %s", account_suffix, stall,
                "reloading to shed the degraded SPA and resuming"
                if recover else "keeping the details captured so far",
            )
            if not recover or not _recover_tx_page(page, reapply):
                break
            page_n = 0
            click_fails = 0
            continue
        lap_s = time.monotonic() - lap_start
        if lap_s > SLOW_LAP_NOTICE_S:
            log.info("more-detail …%s pg %d took %.0fs — SPA under load; "
                     "details keep accruing", account_suffix, page_n, lap_s)
        if not advanced:
            break
    log.info("more-detail …%s: captured %d record(s) across %d page(s)"
             "%s", account_suffix, len(details), page_n,
             f" ({wire_skips} wire row(s) skipped by design)"
             if wire_skips else "")
    return details

def _click_visible_export_button(page, in_modal: bool) -> bool:
    """Click the first visible "Export" button. When in_modal is
    False, the modal hasn't opened yet so only the main-page
    Export button is visible; when True, scope to the dialog.
    """
    if in_modal:
        modal = page.locator(
            f'[role="dialog"][aria-labelledby="export-modal-modal-title"]'
        )
        btn = modal.get_by_role("button", name="Export", exact=True).first
    else:
        btn = page.get_by_role(
            "button", name="Export", exact=True,
        ).first
    try:
        if btn.count() == 0 or not btn.is_visible():
            return False
        btn.click(timeout=10_000)
        return True
    except Exception:
        return False

# ============================================================
# Transaction History (filter + export, per-account)
# ============================================================
#
# Schwab's Transaction History SPA reuses the same chrome as
# Statements (account selector, date-range <select>, paginated
# results) with a different pagination element id ("pagination"
# vs "document-pagination") and its own Search button. The date
# filter is applied per account, then the Export modal yields the
# full row set as CSV/JSON/XML (no per-page HTML capture); the
# default-on more-detail walk scrapes the per-row modal extras
# the export lacks.

def _apply_tx_filter(page, account_suffix: str, tx_range: str,
                     exact_window: tuple | None,
                     debug: bool = False,
                     screenshot_dir: Path | None = None) -> None:
    """Set the tx-history date-range filter and click Search.

    Tx-history's date-range <select> sits on the main page (the
    "Filter by Transaction Types" launcher is a different concern);
    select_date_range JS-sets the value and the Search button
    applies it, same shape as the Statements page. Best-effort
    throughout — partial capture is still useful on a UI tweak.

    An exact window (an ISO-date --lookback) beats the nearest-preset
    mapping: SpecifyDateRange + the two datepickers give tx-history
    the window as asked, no over-fetch; any failure falls back to the
    preset that covers the window. Under ``debug`` the custom-mode
    discovery probe runs between the preset set and the Search (once
    per process). Also the re-establishment step after a
    degraded-SPA recovery reload, where ``debug`` stays off so the
    probe never re-fires.
    """
    custom_set = False
    if exact_window is not None:
        try:
            fill_custom_date_range(page, exact_window[0], exact_window[1])
            custom_set = True
        except Exception as e:
            log.warning("tx-history …%s: custom range failed (%s); "
                        "falling back to preset %r", account_suffix,
                        e, tx_range)
    if not custom_set:
        try:
            select_date_range(page, tx_range)
            if debug:
                probe_custom_date_range(page, screenshot_dir,
                                        "SpecifyDateRange", tx_range)
        except Exception as e:
            log.warning("tx-history …%s: could not set date range to %r: %s",
                        account_suffix, tx_range, e)
    try:
        page.locator(f"#{schwab.TX_SEARCH_BUTTON_ID}").first.click(
            force=True, timeout=10_000,
        )
    except Exception as e:
        log.warning("tx-history …%s: could not click Search: %s",
                    account_suffix, e)
    page.wait_for_load_state("domcontentloaded", timeout=NAV_TIMEOUT_MS)
    page.wait_for_timeout(2000)


def capture_transactions(page, account: dict, dest_dir: Path,
                         screenshot_dir: Path | None,
                         date_range: str,
                         with_more_detail: bool = False,
                         debug: bool = False,
                         exact_window: tuple | None = None) -> dict:
    """Per-account: select the account, set the date-range
    filter, click Search, then drive the Export modal to save
    CSV + JSON + XML of the full filtered transaction set under
    <bronze-dir>/transactions/<suffix>/. With ``debug``, also captures
    one HTML snapshot of the rendered landing page as a debug
    baseline under <bronze-dir>/screenshots/ (never read by load;
    reclaimed by `prune`).

    When `with_more_detail=True`, additionally walks every
    pagination page, scrolls the virtualised table to force-load
    each row, clicks the row's "More" link if present, and
    captures the per-row detail modal contents (Settle Date,
    CUSIP, Principal, Commission, Industry Fee, etc.) into a
    sidecar `more-details.json`. On by default; `--no-more-detail`
    opts out of the ~1 extra click per transaction (several
    thousand for a multi-year backfill). The silver loader
    merges these details into the transaction's payload when the
    sidecar is present (see load._load_more_details).
    """
    log.info("=== tx-history: %s (…%s) ===",
             account["label"], account["suffix"])
    select_account(page, account["entry_id"])
    page.wait_for_load_state("domcontentloaded", timeout=NAV_TIMEOUT_MS)
    page.wait_for_timeout(1500)

    # `date_range` from the CLI uses Statements' option values;
    # tx-history exposes a disjoint set (different option names,
    # different granularities). Map each Statements value to the
    # tx-history value with the closest matching coverage; the
    # rare in-tx-only values (CurrentMonth, PreviousMonth, etc.)
    # pass through verbatim.
    _STATEMENT_TO_TX_RANGE = {
        "Today":        "Today",
        "Last7Days":    "Last7Days",
        "Last3Months":  "Last6Months",  # tx has no 3-month preset
        "Last6Months":  "Last6Months",
        "Last5Years":   "All",          # tx maxes out at 6 months / All
        "Last10Years":  "All",
    }
    tx_range = _STATEMENT_TO_TX_RANGE.get(date_range, date_range)
    _apply_tx_filter(page, account["suffix"], tx_range, exact_window,
                     debug=debug, screenshot_dir=screenshot_dir)

    try:
        url = page.evaluate("() => location.href") or page.url
    except Exception:
        url = page.url
    title = page.title()
    log.info("tx-history …%s: url=%s title=%r",
             account["suffix"], url, title)

    out_dir = dest_dir / "transactions" / account["suffix"]
    out_dir.mkdir(parents=True, exist_ok=True)

    # Drive the "Export Transactions Data" modal — Schwab gives
    # us CSV + JSON + XML of the full filtered set, side-stepping
    # the table's row-virtualisation. CSV/JSON/XML all carry the
    # full row set; JSON/XML add `AcctgRuleCd`. None carry the
    # per-row "More" modal extras (Settle Date / CUSIP /
    # Principal / Commission / Industry Fee) — those come from
    # the default-on `with_more_detail` scrape below.
    exports = _export_tx_history(page, account["suffix"], out_dir)

    # Optionally capture one HTML snapshot of the search-results
    # landing page as a debug baseline — useful for diffing
    # against the structured exports if any field looks missing.
    # Opt-in (`--debug`): it is never read by `load`, so it stays
    # out of the bronze tree by default. It lands under
    # <run>/screenshots/ (NOT the transactions/<suffix>/ load-input
    # dir) so `prune` can reclaim it wholesale from a complete dump.
    html_pages = 0
    if debug:
        try:
            shots_dir = dest_dir / "screenshots"
            shots_dir.mkdir(parents=True, exist_ok=True)
            html_path = shots_dir / f"tx-{account['suffix']}-landing.html"
            html_path.write_text(debugcap.scrub_dom(page.content()),
                                 encoding="utf-8")
            html_pages = 1
        except Exception as e:
            log.warning("tx-history …%s: landing HTML capture failed: %s",
                        account["suffix"], e)

    more_details_count = 0
    if with_more_detail:
        def _reapply():
            select_account(page, account["entry_id"])
            page.wait_for_load_state("domcontentloaded",
                                     timeout=NAV_TIMEOUT_MS)
            page.wait_for_timeout(1500)
            _apply_tx_filter(page, account["suffix"], tx_range, exact_window)

        details = _scrape_more_details(page, account["suffix"],
                                       reapply=_reapply,
                                       screenshot_dir=screenshot_dir)
        if details:
            try:
                (out_dir / "more-details.json").write_text(
                    json.dumps(details, indent=2, ensure_ascii=False),
                    encoding="utf-8",
                )
                more_details_count = len(details)
            except Exception as e:
                log.warning("tx-history …%s: more-details write failed: %s",
                            account["suffix"], e)

    log.info(
        "tx-history …%s: exported %d format(s) + %d landing HTML + "
        "%d more-detail record(s) to %s",
        account["suffix"], len(exports), html_pages, more_details_count,
        out_dir,
    )

    return {
        "label": account["label"],
        "suffix": account["suffix"],
        "url": url,
        "title": title,
        "date_range": tx_range,
        "exports": exports,
        "more_details_count": more_details_count,
        "landing_html": html_pages,
        "out_dir": str(out_dir.relative_to(dest_dir)),
    }

def run_transactions(page, accounts: list[dict], dest_dir: Path,
                     screenshot_dir: Path | None,
                     date_range: str,
                     with_more_detail: bool = False,
                     debug: bool = False,
                     exact_window: tuple | None = None) -> list[dict]:
    """Navigate to the Transaction History page, then capture per
    account. Returns the per-account list to merge into the
    run.json manifest."""
    log.info("navigating to %s", schwab.TRANSACTION_HISTORY_URL)
    page.goto(
        schwab.TRANSACTION_HISTORY_URL,
        wait_until="domcontentloaded",
        timeout=NAV_TIMEOUT_MS,
    )
    page.wait_for_load_state("domcontentloaded", timeout=NAV_TIMEOUT_MS)
    page.wait_for_timeout(2000)
    maybe_screenshot(page, screenshot_dir, "tx-history-landed")

    results: list[dict] = []
    for acct in accounts:
        try:
            entry = capture_transactions(
                page, acct, dest_dir, screenshot_dir, date_range,
                with_more_detail=with_more_detail, debug=debug,
                exact_window=exact_window,
            )
        except Exception as e:
            log.error(
                "tx-history capture failed for …%s: %s",
                acct["suffix"], e,
            )
            entry = {
                "label": acct["label"],
                "suffix": acct["suffix"],
                "error": str(e),
            }
        else:
            # The export files are the authoritative tx data; an
            # account without any is a failed capture, not an "ok"
            # entry — a wedged overlay once ate the Export clicks of
            # six accounts while every entry still read as success.
            if not entry.get("exports"):
                log.error(
                    "tx-history …%s: no export captured — the Export "
                    "modal likely never opened (blocked click or UI "
                    "drift); marking the account failed",
                    acct["suffix"],
                )
                entry["error"] = "no export captured"
        results.append(entry)
    return results

# ============================================================
# Main flow
# ============================================================

@contextlib.contextmanager
def _open_run_dir(dest_root: Path, run_ts: str, *, dry_run: bool):
    """Yield the directory the walk persists its bronze artefacts into.

    Real run:  ``<dest_root>/<run_ts>/`` — created under the caller's
    bronze root and left in place for `load` / `prune`.

    Dry run:   a throwaway ``TemporaryDirectory`` that is NOT under
    ``dest_root``. The read-only walk still runs and still fires its
    manifest + tx-export writes (so the session and the export surfaces
    get verified), but nothing lands under the bronze root — honouring
    the repo-wide "``download --dry-run`` persists nothing to bronze"
    contract (root CLAUDE.md §2). The scratch tree is removed on exit,
    even on crash, by the context manager. Because a dry-run never
    writes under ``dest_root``, `load` simply never sees a dump to skip
    — the status guard still matters only for a crashed *real* run.
    """
    if dry_run:
        with tempfile.TemporaryDirectory(prefix="schwab-web-dryrun-") as scratch:
            run_dir = Path(scratch) / run_ts
            run_dir.mkdir(parents=True, exist_ok=True)
            yield run_dir
    else:
        run_dir = dest_root / run_ts
        run_dir.mkdir(parents=True, exist_ok=True)
        yield run_dir

def walk(page, dest_root: Path, *, mode: str = "all",
         dry_run: bool = False,
         screenshot_dir: Path | None = None,
         date_range: str = schwab.DATE_RANGE_DEFAULT,
         with_more_detail: bool = False,
         debug: bool = False,
         exact_window: tuple | None = None) -> dict:
    """Run the configured scrape against an already-authenticated
    page. Returns the manifest dict.

    Caller is responsible for:
      - having `page` on a logged-in client.schwab.com URL,
      - keeping the Playwright context alive for the duration,
      - closing the context afterwards.

    Side effects: on a real run, creates `<dest_root>/<UTC-ts>/` and
    writes bronze PDFs + a `run.json` manifest. The manifest is
    persisted incrementally so a crash mid-walk preserves whatever was
    downloaded. A `--dry-run` persists NOTHING under `dest_root`: it
    walks the same read-only surfaces into a throwaway temp dir that is
    reclaimed on exit (see `_open_run_dir`), so a dry-run never leaves a
    dump for `load`/`prune` to reason about.

    The manifest carries a `status` field: `"in-progress"` from
    run-dir creation, atomically overwritten with `"complete"`
    (or `"dry-run"`) once the walk finishes. That is the forward
    signal `prune` keys on to tell a finished dump from a
    crashed/interrupted one, and the signal `load` checks to keep
    a partial dump out of silver.
    """
    run_ts = bronze.ts_slug()
    # A `--dry-run` walks the same read-only surfaces but must persist
    # nothing under the bronze root (root CLAUDE.md §2): `_open_run_dir`
    # hands it a throwaway temp dir instead of `<dest_root>/<run_ts>/`,
    # so the manifest + any fired tx exports land in scratch and are
    # reclaimed on exit. A real run gets the bronze run dir as before.
    with _open_run_dir(dest_root, run_ts, dry_run=dry_run) as run_dir:
        if dry_run:
            log.info("dry-run: nothing written to bronze "
                     "(scratch run dir %s)", run_dir)
        else:
            log.info("bronze run dir: %s%s", run_dir,
                     " (debug captures on)" if debug else "")

        run_summary = {
            "run_ts": run_ts,
            "dest": str(run_dir),
            "mode": mode,
            "dry_run": dry_run,
            "date_range": date_range,
            # `"in-progress"` until the terminal write below flips it.
            # A crash before that flip leaves this marker in place, so a
            # non-complete dump is legible to both `prune` and `load`.
            "status": "in-progress",
            "doc_types_wanted": sorted(schwab.DOC_TYPES_WANTED),
            "doc_types_unwanted": [
                label for label, _ in schwab.DOC_TYPES
                if label not in schwab.DOC_TYPES_WANTED
            ],
            "statements": [],
            "transactions": [],
        }
        # Drop the in-progress marker up front (belt-and-suspenders) so
        # a crash during account enumeration — before the first
        # incremental manifest write below — still leaves a status
        # marker rather than a run dir with no run.json at all. On a
        # dry-run this (and every write below) lands in the scratch dir.
        _write_manifest(run_dir, run_summary)

        # Enumerate accounts via the Statements page — the selector
        # is global to the SPA so navigating away later doesn't lose
        # the inventory.
        log.info("navigating to %s", schwab.STATEMENTS_URL)
        page.goto(
            schwab.STATEMENTS_URL,
            wait_until="domcontentloaded",
            timeout=NAV_TIMEOUT_MS,
        )
        page.wait_for_load_state("domcontentloaded", timeout=NAV_TIMEOUT_MS)
        page.wait_for_timeout(2000)
        maybe_screenshot(page, screenshot_dir, "statements-landed")

        accounts = enumerate_accounts(page)
        if not accounts:
            log.error("no accounts enumerated")
            maybe_screenshot(page, screenshot_dir, "no-accounts")
            _write_manifest(run_dir, run_summary)
            return run_summary

        if mode in ("statements", "all"):
            for acct in accounts:
                try:
                    per_acct = download_account(
                        page, acct, run_dir, dry_run, screenshot_dir,
                        date_range, debug=debug,
                        exact_window=exact_window,
                    )
                except Exception as e:
                    log.error(
                        "account walk failed for …%s: %s",
                        acct["suffix"], e,
                    )
                    maybe_screenshot(
                        page, screenshot_dir,
                        f"statements-{acct['suffix']}-failure",
                    )
                    per_acct = {
                        "label": acct["label"],
                        "suffix": acct["suffix"],
                        "error": str(e),
                    }
                run_summary["statements"].append(per_acct)
                _write_manifest(run_dir, run_summary)

        if mode in ("transactions", "all"):
            tx_entries = run_transactions(
                page, accounts, run_dir, screenshot_dir, date_range,
                with_more_detail=with_more_detail, debug=debug,
                exact_window=exact_window,
            )
            run_summary["transactions"] = tx_entries
            _write_manifest(run_dir, run_summary)

        # Terminal status flip: `_write_manifest` is tmp+os.replace, so
        # this atomically overwrites the in-progress marker. A
        # `--dry-run` is recorded as `"dry-run"` (a non-complete shell);
        # everything else is `"complete"`. On a dry-run the whole
        # manifest lives in scratch, so `load`/`prune` never meet it.
        run_summary["status"] = "dry-run" if dry_run else "complete"
        _write_manifest(run_dir, run_summary)

        log.info("scrape complete; manifest at %s/run.json", run_dir)
        return run_summary

def _write_manifest(run_dir: Path, summary: dict) -> None:
    path = run_dir / "run.json"
    tmp = run_dir / "run.json.tmp"
    with tmp.open("w", encoding="utf-8") as fh:
        json.dump(summary, fh, indent=2, sort_keys=True)
    os.replace(tmp, path)
