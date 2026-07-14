#!/bin/bash
# Container entrypoint. Dispatches a single positional subcommand to the
# corresponding Python script under /app/.
#
# - `explore`: drives Camoufox over VNC for discovery — needs Xvfb +
#   x11vnc started below.
# - `login`:   drives the SPA login form + TOTP via headed Camoufox in the
#   Xvfb display (no VNC) — the proven stealth fingerprint.
# - `download`: same per-surface browse pattern as explore (headed
#   Camoufox under Xvfb, no VNC).
# - `load`:    pure SQLite + Python (+ pdftotext for PDF parsing), no browser.
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
        # SPA login form + TOTP, reading the 2FA code from stdin. Runs
        # headed Camoufox in the Xvfb virtual display (the proven stealth
        # fingerprint) — no VNC. Xvfb is needed for the headed browser; the
        # operator interacts only via the CLI TOTP prompt.
        start_xvfb
        shift
        exec python3 /app/login.py "$@"
        ;;
    download)
        # Per-surface browse + GraphQL-capture loop. Headed Camoufox in the
        # Xvfb display (same engine/fingerprint as login), no VNC. Uses the
        # same persistent profile as login.py so authentication carries over.
        start_xvfb
        shift
        exec python3 /app/download.py "$@"
        ;;
    load)
        # SQLite silver loader. Ingests bronze (offering/position/
        # cash-flow JSON, document PDFs) into the source-shaped silver
        # tables. Pure Python, no browser, no display.
        shift
        exec python3 /app/load.py "$@"
        ;;
    prune)
        # Delete non-complete dumps (a crashed download with no terminal
        # run.json) from the bronze tree. equityzen writes no
        # bronze-resident debug artefacts, so there is nothing else to
        # reclaim. Pure file walk — no browser, no display. --dry-run
        # prints the plan first.
        shift
        exec python3 /app/prune.py "$@"
        ;;
    sh|bash)
        shift
        exec /bin/bash "$@"
        ;;
    help|--help|-h)
        cat <<'EOF'
equityzen container

Usage:
  <wrapper> <subcommand> [args...]

Subcommands:
  explore     Launch Camoufox in the container's Xvfb display and record
              every action taken in the VNC session (HAR + Playwright
              trace + click log under /debug). Use during the discovery
              phase. Starts x11vnc on the first free host port in
              127.0.0.1:5900-6000 (printed at handoff).
  login       Mint an EquityZen session (headed Camoufox under Xvfb, no
              VNC). Prompts for the 6-digit TOTP code on stdin; persists to
              the profile dir at /secrets/equityzen-profile and renews
              silently if still valid. Pass --check to probe the existing
              session without firing a 2FA push.
  download    Browse the investor surfaces and capture bronze: offerings +
              positions + cash flows (getBuyerInvestments per stage +
              getMyInvestmentDetails per offering). Pass --dry-run for a
              read-only smoke test (writes nothing); document blobs +
              tax-centre metadata are recorded by default, --no-documents skips them.
  load        Ingest bronze snapshots into the SQLite silver: offerings,
              positions, cash_flows, tax_documents. Pass --force to
              re-load snapshots already recorded in dump_runs.
  prune       Delete non-complete dumps (crashed downloads with no
              terminal run.json) from the bronze tree. equityzen writes
              no bronze-resident debug artefacts, so that is the only
              reclaim target. --dry-run prints the plan first.
  sh|bash     Open an interactive shell inside the container.
  help        Show this message.

NOTE: all four verbs (explore / login / download / load) are
implemented. See DESIGN.md.

Run "<wrapper> <subcommand> --help" for subcommand-specific flags.
EOF
        ;;
    *)
        # Pass-through for ad-hoc commands inside the container.
        exec "$@"
        ;;
esac
