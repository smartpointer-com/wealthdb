#!/bin/bash
# Container entrypoint. Dispatches a single positional subcommand
# to the corresponding Python script under /app/.
#
# - `explore`: drives Camoufox over VNC for discovery — needs Xvfb
#   + x11vnc started below.
# - `login`: pure-HTTP CLI-MFA, no browser. Skips Xvfb entirely.
# - `download` (TBD): same SPA blob-download pattern as `explore`,
#   needs Xvfb but not VNC.
# - `load`: pure DuckDB + Python, no browser.
#
# When Xvfb IS started (explore + future download), it's started
# directly rather than via `xvfb-run`. The Ubuntu
# Noble xvfb-run script's SIGUSR1 ready-signaling hangs when the
# parent shell is non-root: it sets the parent SIGUSR1 handler to
# `:` rather than SIG_IGN, so Xvfb declines to send the ready
# signal and the parent waits forever. Socket-existence polling
# is a version-independent ready signal.

set -euo pipefail

# Xvfb / x11vnc / camoufox-cache bootstrap (VFB_DISPLAY, start_xvfb,
# start_x11vnc) is shared across the camoufox collectors, baked into
# base-camoufox at /opt/entrypoint-lib.sh.
# shellcheck source=/dev/null
source /opt/entrypoint-lib.sh

case "${1:-help}" in
    explore)
        # Discovery harness. Drives Camoufox over VNC, records the
        # all clicks + every network fetch into /debug/<ts>/
        # so login.py + download.py can be written from real traces.
        start_xvfb
        start_x11vnc explore
        shift
        exec python3 /app/explore.py "$@"
        ;;
    login)
        # Headless Playwright Firefox driving the SPA login form.
        # No Xvfb, no VNC. Reads the 2FA code from stdin.
        shift
        exec python3 /app/login.py "$@"
        ;;
    download)
        # Headless Playwright Firefox per-portfolio loop.
        # No Xvfb / no VNC — Playwright handles its own headless
        # X (Firefox runs against an in-process Xwayland/Xvfb
        # equivalent that Playwright manages). Uses the same
        # persistent profile as login.py so authentication carries
        # over for free.
        shift
        exec python3 /app/download.py "$@"
        ;;
    load)
        # DuckDB silver loader. Ingests bronze CSVs (trades, balance,
        # overview), recomputes positions_daily via the aggregate-
        # then-window replay (incremental upsert from first deviation
        # day), populates portfolio_prices from overview.csv, and
        # reconciles vs balance.csv per portfolio. With --fetch-prices,
        # also pulls missing USDT-denominated prices from Binance at
        # the end.
        #
        # --scratch-dir /tmp writes the DuckDB silver to the container-
        # local overlay instead of straight onto the /data VirtioFS
        # bind mount: the existing DB is copied in once, DuckDB's
        # per-statement writes stay local, and the finished file is
        # moved back onto /data in a single bulk transfer. It goes
        # before "$@" so an explicit --scratch-dir override still wins
        # (argparse takes the last value).
        shift
        exec python3 /app/load.py --scratch-dir /tmp "$@"
        ;;
    prune)
        # Delete non-complete dumps (crashed / in-progress) from the
        # bronze tree. No browser, no Xvfb. cointracking writes no
        # bronze-resident debug artefacts, so a complete dump is left
        # entirely intact.
        shift
        exec python3 /app/prune.py "$@"
        ;;
    recompress)
        # One-time backlog sweep: replace plain cu_<id>/*.csv in
        # complete dumps with verified .csv.zst twins (the form
        # download now writes). No browser, no Xvfb. Manual only —
        # never schedule; review --dry-run first.
        shift
        exec python3 /app/recompress.py "$@"
        ;;
    fetch-prices)
        # USDT-denominated price backfill from Binance public spot
        # API. --missing for gap-fill (same set as `load --fetch-
        # prices`), no-flag for full re-fetch (corruption recovery).
        # Always re-fetches the latest priced day because that row
        # was an intraday snapshot when first written.
        shift
        exec python3 /app/fetch_prices.py "$@"
        ;;
    sh|bash)
        shift
        exec /bin/bash "$@"
        ;;
    help|--help|-h)
        cat <<'EOF'
cointracking container

Usage:
  <wrapper> <subcommand> [args...]

Subcommands:
  explore     Launch Camoufox in the container's Xvfb display and
              record every action taken in the VNC session (HAR +
              Playwright trace + click log under /debug). Use during
              the discovery phase. Starts x11vnc on the first free
              host port in 127.0.0.1:5900-6000 (printed at handoff).
  login       Mint a long-lived cointracking session via headless
              Playwright Firefox. Prompts for 2FA code on stdin;
              ticks "Don't ask again" so the device-trust cookie
              persists for years. Persists to the same Firefox
              profile dir as explore (/secrets/cointracking-profile).
              Pass --check to probe the existing session without
              firing a 2FA push.
  download    Headless Firefox per-portfolio loop: trade history
              (Extended-with-additional-columns blob CSV) + balance
              by exchange CSV per portfolio. Writes a UTC-timestamped
              bronze subdir + run.json manifest; each CSV export is
              zstd-compressed in place as it lands (.csv.zst). Pass
              --dry-run to walk the navigation without firing exports.
  load        Ingest bronze snapshots into DuckDB silver: refresh
              transactions table, incremental upsert of
              positions_daily (replays from genesis; rewrites
              only from the first deviation day), populate
              portfolio_prices from overview.csv, reconcile final
              balances vs the balance.csv. Pass --replay-only to
              re-run the holdings replay without re-ingesting
              bronze; --force to re-load already-processed
              snapshots; --fetch-prices to also pull missing USDT-
              denominated prices from Binance after ingest.
  fetch-prices Fetch USDT-denominated prices for every held coin
              from Binance public spot. --missing fills gaps (same
              as `load --fetch-prices`); no flag re-fetches the
              full held range (corruption recovery). Either mode
              always re-fetches the latest priced day so an intraday
              snapshot from a previous run gets upgraded to the
              close price. Stablecoins (USDT, USDC, DAI, …) emit
              synthetic 1.0 USD prices.
  prune       Delete non-complete dumps (crashed / in-progress) from
              the bronze tree; complete dumps and their load inputs
              are kept intact. --dry-run prints the plan first.
  recompress  Convert the pre-compression bronze backlog: replace
              plain cu_<id>/*.csv inside complete dumps with
              sha256-verified .csv.zst twins. Manual one-time sweep;
              --dry-run prints the plan first.
  sh|bash     Open an interactive shell inside the container.
  help        Show this message.

Run "<wrapper> <subcommand> --help" for subcommand-specific flags.
EOF
        ;;
    *)
        # Pass-through for ad-hoc commands inside the container.
        exec "$@"
        ;;
esac
