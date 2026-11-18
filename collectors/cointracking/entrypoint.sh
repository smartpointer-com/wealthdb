#!/bin/bash
# Container entrypoint. Dispatches a single positional subcommand
# to the corresponding Python script under /app/.
#
# - `explore`: drives Camoufox over VNC for discovery — needs Xvfb
#   + x11vnc started below.
# - `login`: headless Playwright Firefox driving the SPA login form,
#   2FA code on stdin. No Xvfb, no VNC (headless Firefox needs no
#   display).
# - `download`: headless Playwright Firefox per-portfolio loop, same
#   persistent profile as `login`. No Xvfb, no VNC.
# - `load`: pure DuckDB + Python, no browser.

set -euo pipefail

# The verb engine, baked into the base image (shared/images/entrypoint-lib.sh).
# shellcheck source=/dev/null
source /opt/entrypoint-lib.sh

verb="${1:-help}"
[[ $# -gt 0 ]] && shift

case "$verb" in
    explore)      run vnc:explore explore.py -- "$@" ;;
    # Headless Playwright Firefox: no display.
    login)        run plain login.py -- "$@" ;;
    download)     run plain download.py -- "$@" ;;
    load)         run plain load.py --scratch-dir /tmp -- "$@" ;;
    recompress)   run plain recompress.py -- "$@" ;;
    fetch-prices) run plain fetch_prices.py -- "$@" ;;
    prune|sh|bash) entrypoint_common "$verb" "$@" ;;
    help|--help|-h)
        cat <<'EOF'
cointracking container

Usage:
  <wrapper> <subcommand> [args...]

Subcommands:
  explore     Launch Camoufox in the container's Xvfb display and
              record every action taken in the VNC session (redacted
              HAR + network/click logs under /debug; add --trace for a
              Playwright trace, which nothing redacts). Use during
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
              by exchange CSV + the daily balance overview CSV per
              portfolio. Writes a UTC-timestamped
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
              bronze; --force to rebuild it from all bronze.
              Missing prices are filled after
              ingest by default, crypto from Binance and fiat from the
              ECB via Frankfurter; --no-fetch-prices skips it.
  fetch-prices Fetch prices for every held coin: crypto
              (USDT-denominated) from Binance public spot, fiat from
              the ECB via Frankfurter. --missing fills gaps (same
              as the default `load` price fill); no flag re-fetches the
              full held range (corruption recovery). Either mode
              always re-fetches the latest priced day so an intraday
              snapshot from a previous run gets upgraded to the
              close price. Stablecoins (USDT, USDC, DAI, …) emit
              synthetic 1.0 USD prices.
  prune       Delete non-complete dumps (crashed / in-progress) from
              the bronze tree, and strip screenshots/ (the download
              --debug captures) from complete dumps, whose load inputs
              are kept intact. --debug-dir also reclaims the /debug
              cache. --dry-run prints the plan first.
  recompress  Convert the pre-compression bronze backlog: replace
              plain cu_<id>/*.csv inside complete dumps with
              sha256-verified .csv.zst twins. Manual one-time sweep;
              --dry-run prints the plan first.
  sh|bash     Open an interactive shell inside the container.
  help        Show this message.

Run "<wrapper> <subcommand> --help" for subcommand-specific flags.
EOF
        ;;
    *) exec "$verb" "$@" ;;
esac
