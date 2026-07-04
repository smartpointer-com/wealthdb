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
  / XML of the full tx-history per account; with --with-more-detail,
  also click each row's "More" link and capture the per-row detail
  modal (Settle Date / CUSIP / Principal / Commission / Industry
  Fee) into a sidecar more-details.json the loader merges into
  silver.

Bronze tree: <dest>/<UTC-timestamp>/ with one PDF per document
under statements/<account>/ plus a run.json manifest.
"""

from __future__ import annotations

import hashlib
import json
import logging
import os
import re
import time
from datetime import date, datetime, timedelta, timezone
from pathlib import Path

import landmarks as schwab
from collectorkit import bronze

log = logging.getLogger("schwab-web.download")

# Same as login.py — generous defaults for the heavy SPA.
NAV_TIMEOUT_MS = 60_000
LANDMARK_TIMEOUT_MS = 60_000

# Bronze run-directory naming: <dest>/<UTC-timestamp>/
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
        html_path.write_text(html, encoding="utf-8")
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

    Uses `dispatch_event('click')` for the entry rather than a
    real pointer click. Schwab can surface an overlay panel (e.g.
    "W-8 Form Status") that lands on top of the dropdown the
    moment it opens, blocking pointer-event delivery to the
    entry beneath. dispatch_event fires the click handler in JS
    directly, sidestepping the z-index race entirely.
    """
    selector_button = page.locator(
        f"button.{schwab.ACCOUNT_SELECTOR_BUTTON_CLASS}"
    ).first
    selector_button.click()
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
    class — the rendered DOM has no `sdps-chips--selected` class
    despite what older notes suggested. Clicks via dispatch_event
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
    if value in ("Custom", "SpecifyDateRange"):
        # Custom modes (one per page) expose extra date inputs
        # we don't have a sample of yet. Fail loud rather than
        # silently leaving the default in place.
        raise NotImplementedError(
            f"custom date-range mode {value!r} not implemented; "
            "pass a preset"
        )
    n_set = page.evaluate(
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
    if not n_set:
        raise RuntimeError(
            f"no <option value={value!r}> found in any "
            f"#{schwab.DATE_RANGE_SELECT_ID}"
        )
    log.debug("date range set to %s on %d <select> element(s)", value, n_set)

def click_visible_button(page, text: str, timeout_s: int = 5) -> bool:
    """Click the first visible button on the page whose text is
    exactly `text`. Returns True on click, False if no visible
    match was found.

    Used when the same button text appears multiple times in the
    DOM (e.g. Apply lives in both the brokerage-filter-modal and
    the charitable-filter-modal; only one is mounted/visible at
    any time per account).
    """
    btns = page.get_by_role("button", name=text, exact=True).all()
    for btn in btns:
        try:
            if btn.is_visible():
                btn.click(timeout=timeout_s * 1000)
                return True
        except Exception:
            continue
    return False

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
    We previously anchored on `<sdps-pagination id="…">` — that
    breaks on Statements: Schwab renders the host as
    `id="document-pagination"` on page 1 but **renames it to
    `id="pagination"`** after the first click (the same id
    tx-history uses). The inner `#pagination-N-link` ids are
    stable across the rename, so we query for them directly and
    skip the host-element lookup entirely. `pagination_id` is
    kept only for diagnostic logging.

    Returns True after a click; False when we're on the last
    page (no `#pagination-{N+1}-link` exists).
    """
    # Read the currently-selected page number from
    # `<a aria-current="page" id="pagination-N-link">`.
    current = page.locator(
        'a[aria-current="page"][id^="pagination-"]'
    ).first
    if current.count() == 0:
        log.info("click_next_page (%s): no aria-current page marker "
                 "— single-page result set or no pagination",
                 pagination_id)
        return False
    try:
        cid = current.get_attribute("id") or ""
    except Exception:
        cid = ""
    m = re.match(r"pagination-(\d+)-link", cid)
    if not m:
        log.info("click_next_page: unexpected current-page id %r; "
                 "stopping", cid)
        return False
    next_n = int(m.group(1)) + 1
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
    # Real click with force=True: bypasses Playwright's
    # actionability waits (the anchor's bounding rect can flicker
    # during the Angular page swap) while still emitting a
    # TRUSTED click — Schwab's pagination handler appears to
    # ignore untrusted (JS dispatch_event) clicks after the first.
    try:
        next_link.click(force=True, timeout=10_000)
    except Exception as e:
        log.warning("click_next_page: click on page %d link failed: %s",
                    next_n, e)
        return False
    # Wait for the new page to actually become current — guards
    # against the next iteration's iter_visible_rows reading the
    # old page's already-DOM-detached rows.
    try:
        page.wait_for_selector(
            f'#pagination-{next_n}-link[aria-current="page"]',
            timeout=10_000,
        )
    except Exception as e:
        log.warning(
            "click_next_page: page %d did not become current "
            "after click: %s", next_n, e,
        )
    return True

# ============================================================
# Per-document download
# ============================================================

# Filename-safe replacement for the parts we want to embed.
_FN_SAFE = re.compile(r"[^A-Za-z0-9._-]+")

def _safe(s: str, fallback: str = "x") -> str:
    out = _FN_SAFE.sub("-", s).strip("-")
    return out or fallback

def _dismiss_open_modal(page) -> None:
    """Press Escape to dismiss any currently-open Schwab modal.

    sdps-modal honors Escape per WAI-ARIA dialog conventions.
    No-op if nothing is open. Used between Transaction History
    accounts to make sure a still-open filter modal can't
    intercept the next `selector_button.click()` with its
    `.sdps-modal__overlay--open` z-index 101003 overlay.
    """
    try:
        page.keyboard.press("Escape")
        page.wait_for_timeout(300)
    except Exception:
        pass

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
                     date_range: str) -> dict:
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
    select_date_range(page, date_range)
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
    paginated view. The Export modal gives us the FULL set in
    one machine-readable file per format. We grab CSV + JSON +
    XML so silver can prefer whichever turns out to have the
    richest field set; CSV is what most consumers will want, but
    JSON sometimes carries nested detail (per-row "More" data
    that doesn't fit CSV's flat shape) and XML occasionally
    carries different metadata. Empirically TBD — see
    DESIGN.md §7.
    """

    entries: list[dict] = []
    out_dir.mkdir(parents=True, exist_ok=True)
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

def _scrape_more_details(page, account_suffix: str) -> list[dict]:
    """Walk every pagination page of the tx-history table,
    scroll-load all virtualised rows, click each row's "More"
    link, capture the per-row detail modal contents.

    Returns a list of `{row_key, row_cells, fields, raw_text}`
    dicts where `row_key` matches load._tx_history_row_key so
    silver can merge details by key without per-source ordering.

    Best-effort throughout: rows without a "More" link are
    silently skipped (Dividend / Interest rows typically have
    no More link), modal-open failures retry once then move on,
    and a per-row exception doesn't abort the per-account
    scrape.
    """

    details: list[dict] = []
    seen_keys: set[str] = set()
    page_n = 0
    while True:
        page_n += 1
        log.info("more-detail …%s: scraping page %d", account_suffix, page_n)
        _scroll_tx_table(page)
        rows = page.locator(schwab.TX_ROW_SELECTOR).all()
        log.debug("more-detail …%s pg %d: %d rendered rows",
                  account_suffix, page_n, len(rows))
        for row_idx, row in enumerate(rows):
            cells = _extract_tx_row_cells(row)
            if not cells:
                continue
            row_key = hashlib.sha256(
                "|".join(cells).encode("utf-8")
            ).hexdigest()[:16]
            if row_key in seen_keys:
                continue
            seen_keys.add(row_key)
            more_btn = row.locator(
                'a:has-text("More"), button:has-text("More")'
            ).first
            if more_btn.count() == 0:
                continue
            try:
                more_btn.click(timeout=5_000)
            except Exception as e:
                log.debug("more-detail row %d click failed: %s", row_idx, e)
                continue
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
                _dismiss_open_modal(page)
        if not click_next_page(page, schwab.TX_PAGINATION_ELEMENT_ID):
            break
        page.wait_for_load_state("domcontentloaded", timeout=NAV_TIMEOUT_MS)
        page.wait_for_timeout(800)
    log.info("more-detail …%s: captured %d record(s) across %d page(s)",
             account_suffix, len(details), page_n)
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
# Transaction History (per-page HTML capture, paginated)
# ============================================================
#
# Schwab's Transaction History SPA reuses the same chrome as
# Statements (account selector, date-range <select>, paginated
# results) with a different pagination element id ("pagination"
# vs "document-pagination") and an "Apply" button instead of
# "Search". Currently we save one rendered-HTML page per
# pagination step into the bronze tree; the row-parsing /
# JSON-emit pass lives in load.py against those snapshots so the
# scraper and the parser can iterate independently.

def capture_transactions(page, account: dict, dest_dir: Path,
                         screenshot_dir: Path | None,
                         date_range: str,
                         with_more_detail: bool = False) -> dict:
    """Per-account: select the account, set the date-range
    filter, click Search, then drive the Export modal to save
    CSV + JSON + XML of the full filtered transaction set under
    <dest>/transactions/<suffix>/. Also captures one HTML
    snapshot of the rendered landing page as a debug baseline.

    When `with_more_detail=True`, additionally walks every
    pagination page, scrolls the virtualised table to force-load
    each row, clicks the row's "More" link if present, and
    captures the per-row detail modal contents (Settle Date,
    CUSIP, Principal, Commission, Industry Fee, etc.) into a
    sidecar `more-details.json`. Off by default — adds ~1 click
    per transaction, which is several thousand extra clicks for
    a multi-year backfill. The silver loader merges these
    details into the transaction's payload when the sidecar is
    present (see load._load_more_details).
    """
    log.info("=== tx-history: %s (…%s) ===",
             account["label"], account["suffix"])
    select_account(page, account["entry_id"])
    page.wait_for_load_state("domcontentloaded", timeout=NAV_TIMEOUT_MS)
    page.wait_for_timeout(1500)

    # The date-range <select> and Apply button live inside a
    # filter modal that's hidden until the launcher is clicked.
    # Open it, set the range, then click the modal's Apply.
    # Best-effort throughout — partial HTML capture is still
    # useful if any step fails on a UI tweak.
    # Tx-history's date-range <select> is on the main page (not
    # inside a modal — the "Filter by Transaction Types"
    # launcher is for the transaction-type checkboxes, a
    # different concern). select_date_range JS-sets the value;
    # we then click the Search button (id=lbl_search-button) to
    # apply the filter, same shape as the Statements page's
    # Search.
    #
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
    try:
        select_date_range(page, tx_range)
    except Exception as e:
        log.warning("tx-history …%s: could not set date range to %r: %s",
                    account["suffix"], tx_range, e)
    try:
        page.locator(f"#{schwab.TX_SEARCH_BUTTON_ID}").first.click(
            force=True, timeout=10_000,
        )
    except Exception as e:
        log.warning("tx-history …%s: could not click Search: %s",
                    account["suffix"], e)
    page.wait_for_load_state("domcontentloaded", timeout=NAV_TIMEOUT_MS)
    page.wait_for_timeout(2000)

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
    # the opt-in `with_more_detail` scrape below.
    exports = _export_tx_history(page, account["suffix"], out_dir)

    # Also capture one HTML snapshot of the search-results
    # landing page as a debug baseline — useful for diffing
    # against the structured exports if any field looks missing.
    html_pages = 0
    try:
        html_path = out_dir / "page-001.html"
        html_path.write_text(page.content(), encoding="utf-8")
        html_pages = 1
    except Exception as e:
        log.warning("tx-history …%s: landing HTML capture failed: %s",
                    account["suffix"], e)

    more_details_count = 0
    if with_more_detail:
        details = _scrape_more_details(page, account["suffix"])
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
                     with_more_detail: bool = False) -> list[dict]:
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
                with_more_detail=with_more_detail,
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
        results.append(entry)
    return results

# ============================================================
# Main flow
# ============================================================

def walk(page, dest_root: Path, *, mode: str = "both",
         dry_run: bool = False,
         screenshot_dir: Path | None = None,
         date_range: str = schwab.DATE_RANGE_DEFAULT,
         with_more_detail: bool = False) -> dict:
    """Run the configured scrape against an already-authenticated
    page. Returns the manifest dict.

    Caller is responsible for:
      - having `page` on a logged-in client.schwab.com URL,
      - keeping the Playwright context alive for the duration,
      - closing the context afterwards.

    Side effects: creates `<dest_root>/<UTC-ts>/`, writes bronze
    PDFs + a `run.json` manifest. The manifest is persisted
    incrementally so a crash mid-walk preserves whatever was
    downloaded.
    """
    run_ts = bronze.ts_slug()
    run_dir = dest_root / run_ts
    run_dir.mkdir(parents=True, exist_ok=True)
    log.info("bronze run dir: %s", run_dir)

    run_summary = {
        "run_ts": run_ts,
        "dest": str(run_dir),
        "mode": mode,
        "dry_run": dry_run,
        "date_range": date_range,
        "doc_types_wanted": sorted(schwab.DOC_TYPES_WANTED),
        "doc_types_unwanted": [
            label for label, _ in schwab.DOC_TYPES
            if label not in schwab.DOC_TYPES_WANTED
        ],
        "statements": [],
        "transactions": [],
    }

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

    if mode in ("statements", "both"):
        for acct in accounts:
            try:
                per_acct = download_account(
                    page, acct, run_dir, dry_run, screenshot_dir,
                    date_range,
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

    if mode in ("transactions", "both"):
        tx_entries = run_transactions(
            page, accounts, run_dir, screenshot_dir, date_range,
            with_more_detail=with_more_detail,
        )
        run_summary["transactions"] = tx_entries
        _write_manifest(run_dir, run_summary)

    log.info("scrape complete; manifest at %s/run.json", run_dir)
    return run_summary

def _write_manifest(run_dir: Path, summary: dict) -> None:
    path = run_dir / "run.json"
    tmp = run_dir / "run.json.tmp"
    with tmp.open("w", encoding="utf-8") as fh:
        json.dump(summary, fh, indent=2, sort_keys=True)
    os.replace(tmp, path)
