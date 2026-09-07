#!/usr/bin/env python3
"""cointracking bronze download driver.

Headless Playwright Firefox (no Camoufox, no Xvfb, no VNC) drives
the per-portfolio loop:

  1. Discover the linked-account portfolio list:
       - master account ID from the `ctfa<id>` session cookie
       - the linked-account IDs + display names from the in-page
         `<a href*="change_user=N">` anchors on /enter_coins.php
       Union of the two = the full list of linked portfolios.

  2. For each portfolio (cu_<id>):
       a. GET /enter_coins.php?change_user=<id>      → activates portfolio
       b. set <select name="extended"> value="2"     → "Extended with
                                                        additional
                                                        columns" mode
       c. click Export → CSV (Full Export)           → 19-column
                                                        trade history blob
       d. GET /balance_by_exchange.php?change_user=<id>
       e. click Export → CSV                         → current per-
                                                        wallet balance
                                                        blob

  3. Write run.json manifest under the bronze run dir.

Each saved CSV export is zstd-compressed in place as it lands
(`trades.csv` → `trades.csv.zst`, via collectorkit.compress with a
decompress-and-verify pass before the plain file is removed).
Compression is best-effort: on failure the plain CSV stays and the
run still succeeds — load.py resolves either form, and DuckDB reads
.csv.zst natively.

The persistent profile dir is shared with login.py + explore.py.
--dry-run does the navigation + portfolio discovery but skips the
export-button clicks (no downloads fire, no bandwidth spent).
"""
from __future__ import annotations

import argparse
import json
import logging
import re
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

from collectorkit import bronze, cli, compress, debugcap, launch

log = logging.getLogger("cointracking.download")

BASE = "https://cointracking.info"
ENTER_COINS_URL = f"{BASE}/enter_coins.php"
BALANCE_URL = f"{BASE}/balance_by_exchange.php"
OVERVIEW_URL = f"{BASE}/overview.php"

DEFAULT_PROFILE_DIR = Path("/secrets/cointracking-profile")
DEFAULT_BRONZE_DIR = Path("/data")

# `select[name="extended"]` value="2" maps to "Table View: Extended
# with additional columns" — the 19-column CSV mode (Trade ID,
# Imported From, Add Date, address/hash fields). value=0 / 1 give
# simpler column sets.
TABLE_MODE_VALUE = "2"

# Same un-auth marker login.py uses for the probe.
UNAUTH_MARKER = "text=You need to be logged in"

# Selectors. Both <select name="extended"> instances on the page
# control the same setting (top + bottom of table); .first wins.
MODE_SELECT = 'select[name="extended"]'
EXPORT_BUTTON = 'button:has-text("Export")'
# Two "CSV (Full Export)" items appear in the menu (top toolbar +
# dropdown). Either fires the same download. .first wins.
TRADES_CSV_ITEM = ':text("CSV (Full Export)")'
# Balance page export menu uses plain "CSV" (no Full Export
# variant). Need an exact text match so it doesn't grab
# "CSV (Full Export)" if that ever shows up there too.
BALANCE_CSV_ITEM = 'a:text-is("CSV"), button:text-is("CSV"), :text-is("CSV")'
# /overview.php's Export menu has a different shape: the "CSV"
# button is a DataTables `buttons-collection` (sub-menu trigger),
# not a direct download. Click it to expand, then pick the
# comma-separated variant from the .dt-button-collection that
# appears.
OVERVIEW_CSV_BUTTON = 'button.buttons-collection:has-text("CSV"), button:has-text("CSV")'
OVERVIEW_CSV_ITEM = '.dt-button-collection button:has-text("Comma separated")'


def is_authenticated(page) -> bool:
    """True iff the current page does not show the unauthenticated
    marketing marker."""
    return page.locator(UNAUTH_MARKER).count() == 0


CHANGE_USER_ANCHOR_SELECTOR = "a[href*='change_user=']"


def _wait_for_anchor_stability(page, max_wait_s: float = 15.0) -> int:
    """Poll the change_user anchor count until it stays the same
    across two consecutive 500 ms checks (or until max_wait_s
    elapses). cointracking injects the linked-user dropdown
    anchors via JS that doesn't reliably complete by networkidle —
    this is the root of the flaky-portfolio-discovery issue. The
    poll loop catches the late-injection case. Returns the final
    observed count; the caller decides whether to fall back to
    the cache when the count is suspicious."""
    prev_count = -1
    stable_polls = 0
    deadline = time.monotonic() + max_wait_s
    while time.monotonic() < deadline:
        count = page.locator(CHANGE_USER_ANCHOR_SELECTOR).count()
        if count > 0 and count == prev_count:
            stable_polls += 1
            if stable_polls >= 2:
                return count
        else:
            stable_polls = 0
        prev_count = count
        page.wait_for_timeout(500)
    return max(0, prev_count)


def discover_portfolios(page) -> list[dict]:
    """Return a list of dicts [{id, name, is_master}, ...] covering
    every portfolio reachable from the logged-in session in the
    live scrape. The "current" portfolio (master account) is read
    from the `ctfa<id>` cookie name; the linked accounts come from
    in-page anchor hrefs. Raises if no portfolios are discovered.

    Uses networkidle plus a follow-up stability poll because
    cointracking injects the portfolio-switcher anchors via JS that
    doesn't always complete by networkidle. The caller normally
    merges this live scrape with the persistent known-portfolios
    cache so a transient miss doesn't drop a portfolio from the
    download set."""
    page.goto(ENTER_COINS_URL, wait_until="networkidle", timeout=45_000)
    if not is_authenticated(page):
        raise RuntimeError(
            "session unauthenticated; run `./cointracking login` first"
        )

    # Wait for late-JS anchor injection to settle before scraping.
    _wait_for_anchor_stability(page)

    # Linked accounts from <a href="?...change_user=N">
    anchors = page.locator(CHANGE_USER_ANCHOR_SELECTOR).all()
    seen: set[str] = set()
    portfolios: list[dict] = []
    for a in anchors:
        href = a.get_attribute("href") or ""
        m = re.search(r"change_user=(\d+)", href)
        if not m:
            continue
        cu_id = m.group(1)
        if cu_id in seen:
            continue
        seen.add(cu_id)
        name = (a.text_content() or "").strip() or f"cu_{cu_id}"
        portfolios.append({"id": cu_id, "name": name, "is_master": False})

    # Master account ID from the ctfa<id> session cookie. Only one
    # ctfa cookie should exist; if there's more than one, pick the
    # first deterministically.
    master_id = None
    for ck in page.context.cookies():
        m = re.match(r"^ctfa(\d+)$", ck["name"])
        if m:
            master_id = m.group(1)
            break

    if master_id and master_id not in seen:
        portfolios.append({
            "id": master_id, "name": f"master_{master_id}", "is_master": True,
        })
    elif master_id:
        # Tag the existing entry as master.
        for p in portfolios:
            if p["id"] == master_id:
                p["is_master"] = True
                break

    if not portfolios:
        raise RuntimeError(
            "no portfolios discovered (anchors=0, ctfa cookie=0); "
            "the page DOM or session shape may have changed"
        )

    log.info("discovered %d portfolios", len(portfolios))
    return portfolios


KNOWN_PORTFOLIOS_FILENAME = "known_portfolios.json"


def load_known_portfolios(bronze_dir: Path) -> list[dict]:
    """Read the persistent set of portfolios this collector has
    ever observed across all prior runs. Returns [] when the file
    doesn't exist yet (first-ever run) or when it can't be parsed
    (one-off corruption shouldn't lock the collector out — the
    live scrape still drives the run). The cache lives next to the
    snapshot subdirs at `<bronze_dir>/known_portfolios.json`, not
    inside any one snapshot, so it survives across runs."""
    path = bronze_dir / KNOWN_PORTFOLIOS_FILENAME
    if not path.is_file():
        return []
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
        portfolios = data.get("portfolios") or []
        return [p for p in portfolios
                if isinstance(p, dict) and "id" in p]
    except (json.JSONDecodeError, OSError) as exc:
        log.warning("known_portfolios.json unreadable (%s); ignoring",
                    exc)
        return []


def save_known_portfolios(bronze_dir: Path, portfolios: list[dict]) -> None:
    """Persist the union of all portfolios ever observed. Caller
    passes the post-merge list (scrape ∪ prior cache). Written
    atomically via a temp + rename so a partial write can't
    corrupt the cache."""
    bronze_dir.mkdir(parents=True, exist_ok=True)
    path = bronze_dir / KNOWN_PORTFOLIOS_FILENAME
    tmp = path.with_suffix(".tmp")
    payload = {"schema": 1, "portfolios": portfolios}
    tmp.write_text(json.dumps(payload, indent=2), encoding="utf-8")
    tmp.replace(path)


def merge_portfolios(scraped: list[dict], cached: list[dict]) -> list[dict]:
    """Union scraped + cached, keyed on portfolio id. Live-scrape
    metadata (name, is_master) overrides whatever's cached on
    overlap — CT can rename a portfolio between runs and the
    refresh shouldn't be sticky. Cached-only entries are
    preserved at the tail so a transient discovery miss doesn't
    drop a portfolio from the download set."""
    by_id: dict[str, dict] = {}
    for p in cached:
        by_id[str(p["id"])] = dict(p)
    for p in scraped:
        by_id[str(p["id"])] = dict(p)
    return list(by_id.values())


def set_table_mode_extended_plus(page) -> None:
    """Set <select name="extended"> to value=2 ('Extended with
    additional columns'). Two complications:

      1. The native <select> is hidden behind a NiceSelect custom
         wrapper (class="nice_select"); same shape as the
         dont_ask_again checkbox in login.py. select_option()
         refuses to interact with invisible elements by default,
         so force=True.

      2. The element has onchange="this.form.submit()" — setting
         the value triggers a full page navigation. expect_navigation
         catches it so the next action runs against the reloaded
         page.

    Short-circuit when the mode is already 2 (re-running across
    portfolios when the setting persists)."""
    select = page.locator(MODE_SELECT).first
    current = select.evaluate("el => el.value")
    if current == TABLE_MODE_VALUE:
        return
    with page.expect_navigation(timeout=20_000):
        select.select_option(value=TABLE_MODE_VALUE,
                             force=True, timeout=10_000)


def compress_export(out_path: Path) -> Path:
    """Compress a freshly saved CSV export in place (`trades.csv` →
    `trades.csv.zst`), best-effort: `load` resolves either form, so a
    compression failure (disk full, missing codec) downgrades to a
    warning and the plain CSV stays — never a lost download. The
    window in which the uncompressed file exists is the compression
    itself; a crash inside it leaves a run dir whose run.json still
    says "in-progress", which `load` skips and `prune` reclaims."""
    try:
        final = compress.compress_file(out_path)
        log.info("  compressed %s → %s (%d bytes)", out_path.name,
                 final.name, final.stat().st_size)
        return final
    except Exception as exc:  # noqa: BLE001 — best-effort by design
        log.warning("  could not compress %s (%s); keeping the plain CSV",
                    out_path.name, exc)
        return out_path


def trigger_export(page, item_selector: str, out_path: Path,
                   dry_run: bool) -> Path | None:
    """Open the Export menu, click `item_selector`, save_as the
    download, compress the saved file. Returns the on-disk path
    (None on --dry-run). The Export menu items are rendered into the
    DOM at click-time — wait for the menu item to become visible
    before clicking it."""
    if dry_run:
        log.info("(dry-run) would click Export → %s → save to %s",
                 item_selector, out_path)
        return None

    page.locator(EXPORT_BUTTON).first.click(timeout=10_000)
    item = page.locator(item_selector).first
    item.wait_for(state="visible", timeout=10_000)
    with page.expect_download(timeout=120_000) as dl_info:
        item.click()
    download = dl_info.value
    out_path.parent.mkdir(parents=True, exist_ok=True)
    download.save_as(str(out_path))
    log.info("  saved %s (%d bytes)", out_path.name, out_path.stat().st_size)
    return compress_export(out_path)


def trigger_overview_csv_export(page, out_path: Path,
                                dry_run: bool) -> Path | None:
    """/overview.php's Export menu has a sub-menu pattern: Export →
    CSV → Comma separated. The first CSV click opens the submenu;
    we then pick the comma-separated variant."""
    if dry_run:
        log.info("(dry-run) would click Export → CSV → Comma separated "
                 "→ save to %s", out_path)
        return None

    page.locator(EXPORT_BUTTON).first.click(timeout=10_000)
    csv_btn = page.locator(OVERVIEW_CSV_BUTTON).first
    csv_btn.wait_for(state="visible", timeout=10_000)
    csv_btn.click(timeout=10_000)
    item = page.locator(OVERVIEW_CSV_ITEM).first
    item.wait_for(state="visible", timeout=10_000)
    with page.expect_download(timeout=120_000) as dl_info:
        item.click()
    download = dl_info.value
    out_path.parent.mkdir(parents=True, exist_ok=True)
    download.save_as(str(out_path))
    log.info("  saved %s (%d bytes)", out_path.name, out_path.stat().st_size)
    return compress_export(out_path)


def download_portfolio(page, portfolio: dict, run_dir: Path,
                       dry_run: bool) -> None:
    """Run the per-portfolio scrape: trade CSV + balance CSV."""
    cu_id = portfolio["id"]
    log_prefix = f"[cu={cu_id}]"
    portfolio_dir = run_dir / f"cu_{cu_id}"

    # Trade history. Set the table to the 19-column mode first,
    # then trigger the blob-CSV download.
    log.info("%s GET /enter_coins.php?change_user=…", log_prefix)
    page.goto(f"{ENTER_COINS_URL}?change_user={cu_id}",
              wait_until="domcontentloaded", timeout=30_000)
    if not is_authenticated(page):
        raise RuntimeError(
            f"{log_prefix} unauthenticated after change_user navigation"
        )
    if not dry_run:
        set_table_mode_extended_plus(page)
    trigger_export(page, TRADES_CSV_ITEM, portfolio_dir / "trades.csv",
                   dry_run)

    # Current per-wallet balance.
    log.info("%s GET /balance_by_exchange.php?change_user=…", log_prefix)
    page.goto(f"{BALANCE_URL}?change_user={cu_id}",
              wait_until="domcontentloaded", timeout=30_000)
    trigger_export(page, BALANCE_CSV_ITEM, portfolio_dir / "balance.csv",
                   dry_run)

    # Daily Balance overview — wide-form table with one row per day
    # and a pair of (Value-in-fiat, Amount) columns per coin held.
    # Used by load.py to derive portfolio_prices. Quote currency is
    # the portfolio's "main fiat" setting in CT and varies between
    # portfolios.
    log.info("%s GET /overview.php?change_user=…", log_prefix)
    page.goto(f"{OVERVIEW_URL}?change_user={cu_id}",
              wait_until="networkidle", timeout=45_000)
    trigger_overview_csv_export(page, portfolio_dir / "overview.csv",
                                dry_run)


def _saved_name(run_dir: Path, cu_id: str, kind: str) -> str:
    """Run-dir-relative name of an export as it actually landed on
    disk (`cu_<id>/trades.csv.zst` normally; `…csv` when compression
    fell back). The loader re-resolves via compress.resolve_variant
    rather than trusting this, but the manifest should record what
    the run really wrote."""
    logical = run_dir / f"cu_{cu_id}" / f"{kind}.csv"
    actual = compress.resolve_variant(logical) or logical
    return str(actual.relative_to(run_dir))


def write_manifest(run_dir: Path, portfolios: list[dict], ts: str,
                   snapshot_at: int,
                   failures: list[dict] | None = None) -> None:
    """Write run.json — the manifest the silver loader reads to
    enumerate portfolios + their bronze paths. `portfolios` is the
    list of successfully-downloaded portfolios; only these appear
    in the `files` block. `failures`, when non-empty, records the
    portfolios that were attempted but didn't complete (for
    visibility; the silver loader ignores this block)."""
    manifest = {
        # Terminal completeness signal: the walk reached the end and
        # finalised. Written atomically, overwriting the "in-progress"
        # marker dropped at run-dir creation. `load` skips a run dir
        # whose status is not "complete"; `prune` reclaims one.
        "status": "complete",
        "snapshot_at": snapshot_at,
        "utc": ts,
        "schema": 1,
        "portfolios": portfolios,
        "files": {
            f"cu_{p['id']}": {
                kind: _saved_name(run_dir, p["id"], kind)
                for kind in ("trades", "balance", "overview")
            }
            for p in portfolios
        },
    }
    if failures:
        manifest["failures"] = failures
    bronze.atomic_write_json(run_dir / "run.json", manifest)
    log.info("wrote run.json (%d portfolios)%s",
             len(portfolios),
             f"; {len(failures)} failure(s)" if failures else "")


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument(
        "--profile-dir", type=Path, default=DEFAULT_PROFILE_DIR,
        help=("Persistent Firefox profile dir (shared with login.py "
              "and explore.py). Default: %(default)s."),
    )
    p.add_argument(
        "--bronze-dir", type=Path, default=DEFAULT_BRONZE_DIR,
        help=("Each invocation creates a UTC-timestamped subdir "
              "under this. Default: %(default)s."),
    )
    p.add_argument(
        "--dry-run", action="store_true",
        help=("Discover portfolios and walk the per-portfolio "
              "navigation, but skip every export-button click. "
              "No downloads fire; no bronze tree is materialised. "
              "Use to verify selectors + portfolio discovery "
              "without burning bandwidth or cluttering /data."),
    )
    p.add_argument(
        "--debug", action="store_true",
        help=("Save opt-in debug captures (DOM + screenshot) INSIDE the "
              "bronze run dir under <run>/screenshots/: the portfolio "
              "-discovery page, whose late-injected switcher anchors are "
              "the known-flaky step, plus the page a portfolio was on "
              "when its download failed. Off by default so a routine "
              "dump holds only what `load` reads; `load` never reads "
              "these, and `prune` reclaims <run>/screenshots/. No-op "
              "under --dry-run, which materialises no bronze tree. "
              "External discovery diagnostics live in explore.py's "
              "/debug mount, never in a bronze run dir."),
    )
    cli.add_standard_args(p, verb="download", full_history=True)
    return p.parse_args(argv)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)
    cli.warn_lookback_ignored(
        args.lookback, log,
        what="the complete trade history its holdings replay requires")
    ts = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    run_dir = args.bronze_dir / ts
    if not args.dry_run:
        run_dir.mkdir(parents=True, exist_ok=True)
        # In-progress marker: a crash before write_manifest leaves a
        # run.json carrying status="in-progress", which `load` skips
        # (so a partial dump never reaches silver) and `prune`
        # reclaims once quiescent. write_manifest atomically
        # overwrites it with status="complete" at the end.
        bronze.atomic_write_json(run_dir / "run.json",
                                 {"status": "in-progress"})
    snapshot_at = int(time.time())

    # Captures live in the run dir, and --dry-run deliberately materialises
    # no bronze tree, so --debug degrades to a warning there rather than
    # creating one for diagnostics alone.
    debug_dir: Path | None = run_dir if args.debug else None
    if args.debug and args.dry_run:
        log.warning("--debug: --dry-run materialises no bronze tree; "
                    "no captures will be written")
        debug_dir = None

    # The run's own credential. This walk is handed no password — it is
    # handed a lifted session — so the jar is what a capture could leak,
    # and an SPA that echoes its session into a meta tag or a bootstrap
    # script puts it in the markup a capture serialises.
    # The session lives in the persistent profile, so the jar is the
    # browser's and is read at the first capture rather than here.
    mask = debugcap.SessionMask()

    def capture(page, name: str) -> None:
        """Snapshot a page; a no-op unless --debug supplied a dir."""
        if debug_dir is not None:
            debugcap.capture_page(page, debug_dir, name, log=log,
                                  redact=mask.for_page(page))

    # Relocate the profile's regenerable startupCache out of the secrets
    # tree (idempotent; also migrates a pre-relocation profile) before
    # launching against it.
    launch.prepare_profile_dir(args.profile_dir)

    from playwright.sync_api import sync_playwright

    with sync_playwright() as pw:
        context = pw.firefox.launch_persistent_context(
            user_data_dir=str(args.profile_dir),
            headless=True,
            accept_downloads=True,
            viewport={"width": 1280, "height": 800},
            firefox_user_prefs=launch.firefox_prefs(),
        )
        try:
            page = context.new_page()
            try:
                scraped = discover_portfolios(page)
            finally:
                # Discovery drives the whole run: it is the auth gate, and
                # its switcher anchors are injected by late JS that the
                # merge below exists to paper over. When that list comes
                # back short, this DOM is the only thing that says why —
                # nothing downstream records it. Captured on the raise path
                # too (an unauthenticated session lands here).
                capture(page, "10-portfolios")
            cached = load_known_portfolios(args.bronze_dir)
            portfolios = merge_portfolios(scraped, cached)

            scraped_ids = {str(p["id"]) for p in scraped}
            cache_only = [p for p in portfolios
                          if str(p["id"]) not in scraped_ids]
            if cache_only:
                names = [p.get("name", p["id"]) for p in cache_only]
                log.warning(
                    "%d portfolio(s) in cache but missing from this "
                    "discovery (likely flaky CT linked-user list): %s "
                    "— attempting downloads anyway",
                    len(cache_only), names,
                )
            # Save the union (scraped ∪ cache) so the next run
            # already knows about any newly discovered portfolios
            # even if the current run later fails.
            if not args.dry_run:
                save_known_portfolios(args.bronze_dir, portfolios)

            successes: list[dict] = []
            failures: list[dict] = []
            for portfolio in portfolios:
                log.info(
                    "→ portfolio cu=%s name=%r%s",
                    portfolio["id"], portfolio["name"],
                    " [master]" if portfolio.get("is_master") else "",
                )
                try:
                    download_portfolio(page, portfolio, run_dir,
                                       args.dry_run)
                    successes.append(portfolio)
                except Exception as exc:  # noqa: BLE001 — isolate per-portfolio
                    log.error("cu=%s download failed: %s — continuing "
                              "with next portfolio", portfolio["id"], exc)
                    # Captured on the failure path only: the page is still
                    # on whichever export surface raised, so this is the
                    # DOM the failing selector was matched against. A
                    # capture per portfolio per surface would bury it.
                    capture(page, f"20-cu{portfolio['id']}-failed")
                    failures.append({
                        "id": str(portfolio["id"]),
                        "name": portfolio.get("name"),
                        "error": str(exc),
                    })

            if not args.dry_run:
                write_manifest(run_dir, successes, ts, snapshot_at,
                               failures=failures)
                log.info(
                    "done: %d/%d portfolios → %s%s",
                    len(successes), len(portfolios), run_dir,
                    f"; {len(failures)} failed" if failures else "",
                )
                # Non-zero exit only when EVERY portfolio failed —
                # partial success still progresses the silver state
                # for the portfolios that did download.
                return 1 if successes == [] and failures else 0
            else:
                log.info("dry-run done: %d portfolios visited",
                         len(portfolios))
            return 0
        finally:
            context.close()


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
