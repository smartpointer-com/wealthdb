#!/usr/bin/env python3
"""cointracking bronze download driver.

Headless Playwright Firefox (no Camoufox, no Xvfb, no VNC) drives
the per-portfolio loop:

  1. Discover the linked-account portfolio list:
       - master account ID from the `ctfa<id>` session cookie
       - 4 linked-account IDs + display names from the in-page
         `<a href*="change_user=N">` anchors on /enter_coins.php
       Union of the two = full list of 5 portfolios.

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

from collectorkit import cli

log = logging.getLogger("cointracking.download")

BASE = "https://cointracking.info"
ENTER_COINS_URL = f"{BASE}/enter_coins.php"
BALANCE_URL = f"{BASE}/balance_by_exchange.php"

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


def is_authenticated(page) -> bool:
    """True iff the current page does not show the unauthenticated
    marketing marker."""
    return page.locator(UNAUTH_MARKER).count() == 0


def discover_portfolios(page) -> list[dict]:
    """Return a list of dicts [{id, name, is_master}, ...] covering
    every portfolio reachable from the logged-in session. The
    "current" portfolio (master account) is read from the
    `ctfa<id>` cookie name; the linked accounts come from in-page
    anchor hrefs. Raises if no portfolios are discovered.

    Uses networkidle (not just domcontentloaded) for this initial
    load because cointracking injects the portfolio-switcher
    anchors via JS after the initial render — domcontentloaded is
    too early and yields an incomplete list."""
    page.goto(ENTER_COINS_URL, wait_until="networkidle", timeout=45_000)
    if not is_authenticated(page):
        raise RuntimeError(
            "session unauthenticated; run `./cointracking login` first"
        )

    # Linked accounts from <a href="?...change_user=N">
    anchors = page.locator("a[href*='change_user=']").all()
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


def trigger_export(page, item_selector: str, out_path: Path,
                   dry_run: bool) -> Path | None:
    """Open the Export menu, click `item_selector`, save_as the
    download. Returns the saved path (None on --dry-run). The
    Export menu items are rendered into the DOM at click-time —
    wait for the menu item to become visible before clicking it."""
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
    return out_path


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


def write_manifest(run_dir: Path, portfolios: list[dict], ts: str,
                   snapshot_at: int) -> None:
    """Write run.json — the manifest the silver loader reads to
    enumerate portfolios + their bronze paths."""
    manifest = {
        "snapshot_at": snapshot_at,
        "utc": ts,
        "schema": 1,
        "portfolios": portfolios,
        "files": {
            f"cu_{p['id']}": {
                "trades": f"cu_{p['id']}/trades.csv",
                "balance": f"cu_{p['id']}/balance.csv",
            }
            for p in portfolios
        },
    }
    (run_dir / "run.json").write_text(
        json.dumps(manifest, indent=2), encoding="utf-8")
    log.info("wrote run.json (%d portfolios)", len(portfolios))


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
        help=("Bronze tree root. Each invocation creates a UTC-"
              "timestamped subdir under this. Default: %(default)s."),
    )
    p.add_argument(
        "--dry-run", action="store_true",
        help=("Discover portfolios and walk the per-portfolio "
              "navigation, but skip every export-button click. "
              "No downloads fire; no bronze tree is materialised. "
              "Use to verify selectors + portfolio discovery "
              "without burning bandwidth or cluttering /data."),
    )
    cli.add_common_args(p)
    return p.parse_args(argv)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)

    ts = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    run_dir = args.bronze_dir / ts
    if not args.dry_run:
        run_dir.mkdir(parents=True, exist_ok=True)
    snapshot_at = int(time.time())

    from playwright.sync_api import sync_playwright

    with sync_playwright() as pw:
        context = pw.firefox.launch_persistent_context(
            user_data_dir=str(args.profile_dir),
            headless=True,
            accept_downloads=True,
            viewport={"width": 1280, "height": 800},
        )
        try:
            page = context.new_page()
            portfolios = discover_portfolios(page)
            for portfolio in portfolios:
                log.info(
                    "→ portfolio cu=%s name=%r%s",
                    portfolio["id"], portfolio["name"],
                    " [master]" if portfolio["is_master"] else "",
                )
                download_portfolio(page, portfolio, run_dir, args.dry_run)

            if not args.dry_run:
                write_manifest(run_dir, portfolios, ts, snapshot_at)
                log.info("done: %d portfolios → %s",
                         len(portfolios), run_dir)
            else:
                log.info("dry-run done: %d portfolios visited",
                         len(portfolios))
            return 0
        finally:
            context.close()


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
