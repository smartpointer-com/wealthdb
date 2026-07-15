#!/bin/bash
# SCAFFOLD — container entrypoint. Dispatches a single positional
# subcommand to the corresponding Python script under /app/.
#
# - `explore`: drives Camoufox over VNC for discovery — needs Xvfb +
#   x11vnc started below.
# - `login`:   drives the SPA login form + 2FA. Browser engine (Camoufox
#   headed-under-Xvfb vs vanilla Firefox headless) is TBD pending the
#   explore phase; if it ends up needing a display, add `start_xvfb`
#   to the login case (VNC not required).
# - `download` (TBD): same per-surface browse pattern as explore; needs
#   a display if it uses Camoufox, no VNC.
# - `load`:    pure SQLite + Python, no browser.
#
# When Xvfb IS started, it's started directly rather than via `xvfb-run`:
# the Ubuntu Noble xvfb-run script's SIGUSR1 ready-signaling hangs under
# a non-root parent. Socket-existence polling is a version-independent
# ready signal.

set -euo pipefail

# Xvfb / x11vnc / camoufox-cache bootstrap (VFB_DISPLAY, start_xvfb,
# start_x11vnc) is shared across the camoufox collectors, baked into
# base-camoufox at /opt/entrypoint-lib.sh.
# shellcheck source=/dev/null
source /opt/entrypoint-lib.sh

case "${1:-help}" in
    explore)
        # Discovery harness. Drives Camoufox over VNC, records every
        # click + every network fetch into /debug/<ts>/ so login.py +
        # download.py can be written from real traces.
        start_xvfb
        start_x11vnc explore
        shift
        exec python3 /app/explore.py "$@"
        ;;
    login)
        # SPA login form + 2FA, headed Camoufox under Xvfb (no VNC — the
        # 2FA code is read from stdin). Camoufox (not vanilla Firefox) is
        # required to clear Carta's Cloudflare challenge; headed-under-Xvfb
        # matches the proven explore config.
        start_xvfb
        shift
        exec python3 /app/login.py "$@"
        ;;
    download)
        # Reuses the login.py session profile. Drives headed Camoufox under
        # Xvfb (Cloudflare again), then calls the JSON endpoints via the
        # context request API. No VNC.
        start_xvfb
        shift
        exec python3 /app/download.py "$@"
        ;;
    load)
        # SQLite silver loader. Ingests bronze (portfolio/issuer/security
        # JSON, export blobs, document PDFs) into the source-shaped silver
        # tables. Pure Python, no browser, no display.
        shift
        exec python3 /app/load.py "$@"
        ;;
    prune)
        # Delete non-complete dumps (a crashed walk left run.json
        # status="in-progress", or a pre-status run left no run.json) from
        # the bronze tree, plus screenshots/ (the download --debug
        # captures) from complete dumps. Pure file walk — no browser, no
        # Xvfb. A complete dump's load inputs are never touched.
        # Also reachable host-side via the wrapper (the usual path).
        shift
        exec python3 /app/prune.py "$@"
        ;;
    sh|bash)
        shift
        exec /bin/bash "$@"
        ;;
    help|--help|-h)
        cat <<'EOF'
carta container (SCAFFOLD)

Usage:
  <wrapper> <subcommand> [args...]

Subcommands:
  explore     Launch Camoufox in the container's Xvfb display and record
              every action taken in the VNC session (HAR + Playwright
              trace + click log under /debug). Use during the discovery
              phase. Starts x11vnc on the first free host port in
              127.0.0.1:5900-6000 (printed at handoff).
  login       Mint a Carta holder session. Prompts for the 2FA code on
              stdin; persists to the Camoufox profile dir at
              /secrets/carta-profile. Pass --check to probe the existing
              session without firing a 2FA push.
  download    Browse the holder surfaces and capture bronze: portfolios,
              issuers (companies held), securities (option grants w/
              strike+vesting, RSUs, RSAs, shares, SAFEs/notes),
              transactions, 409A FMVs, and tax documents. Pass --dry-run
              to walk the navigation without firing exports/downloads.
  load        Ingest bronze snapshots into the SQLite silver: portfolios,
              issuers, securities, vesting_events, transactions,
              fair_market_values, tax_documents. Pass --force to delete
              the silver DB and rebuild it from all bronze.
  prune       Delete non-complete dumps (crashed walks) from the bronze
              tree, and strip screenshots/ (the download --debug captures)
              from complete dumps, whose load inputs are never touched.
              --dry-run prints the plan first. Usually run host-side via
              the wrapper.
  sh|bash     Open an interactive shell inside the container.
  help        Show this message.

NOTE: this collector is a SCAFFOLD. login / download / load are stubs
that exit without touching carta.com until the explore phase has run and
the implementation lands. See DESIGN.md.

Run "<wrapper> <subcommand> --help" for subcommand-specific flags.
EOF
        ;;
    *)
        # Pass-through for ad-hoc commands inside the container.
        exec "$@"
        ;;
esac
