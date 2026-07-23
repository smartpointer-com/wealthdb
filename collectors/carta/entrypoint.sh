#!/bin/bash
# Container entrypoint. Dispatches a single positional subcommand to
# the corresponding Python script under /app/.
#
# - `explore`: drives Camoufox over VNC for discovery — needs Xvfb +
#   x11vnc started below.
# - `login`:   SPA login form + 2FA, headed Camoufox under Xvfb (no VNC;
#   the 2FA code is read from stdin). Camoufox clears Carta's Cloudflare
#   challenge.
# - `download`: reuses the login session profile; headed Camoufox under
#   Xvfb, then the JSON endpoints via the context request API. No VNC.
# - `load`:    pure SQLite + Python, no browser.
# - `prune`:   pure file walk, no browser.
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
carta container

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
  download    Walk the holder REST/JSON API and capture bronze for both
              holding families: the cap-table side (holdings, grants,
              vesting, per-grant exercise-detail xlsx) and the fund-LP
              side (capital account, cap calls), plus the document
              archive. Pass --dry-run to verify discovery without
              writing; --no-documents skips the document pass.
  load        Ingest bronze snapshots into the SQLite silver: entities,
              securities, vesting_schedules + vesting_events,
              fund_metrics, cap_calls, capital_events, cash_flows,
              documents. Pass --force to delete the silver DB and
              rebuild it from all bronze.
  prune       Delete non-complete dumps (crashed walks) from the bronze
              tree, and strip screenshots/ (the download --debug captures)
              from complete dumps, whose load inputs are never touched.
              --dry-run prints the plan first. Usually run host-side via
              the wrapper.
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
