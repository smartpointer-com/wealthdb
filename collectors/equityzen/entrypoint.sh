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
# - `prune`:   pure file walk, no browser.

set -euo pipefail

# The verb dispatch (run, login_folds, entrypoint_common) and the Xvfb /
# x11vnc / camoufox-cache bootstrap, baked into the base image at
# /opt/entrypoint-lib.sh.
# shellcheck source=/dev/null
source /opt/entrypoint-lib.sh

verb="${1:-help}"
[[ $# -gt 0 ]] && shift

case "$verb" in
    explore)  run vnc:explore explore.py -- "$@" ;;
    login)    run xvfb login.py -- "$@" ;;
    download) run xvfb download.py -- "$@" ;;
    load)     run plain load.py -- "$@" ;;
    prune|sh|bash) entrypoint_common "$verb" "$@" ;;
    help|--help|-h)
        cat <<'EOF'
equityzen container

Usage:
  <wrapper> <subcommand> [args...]

Subcommands:
  explore     Launch Camoufox in the container's Xvfb display and record
              every action taken in the VNC session (redacted HAR +
              network/click logs under /debug; add --trace for a
              Playwright trace, which nothing redacts). Use during the
              discovery phase. Starts x11vnc on the first free host port in
              127.0.0.1:5900-6000 (printed at handoff).
  login       Mint an EquityZen session (headed Camoufox under Xvfb, no
              VNC). Prompts for the 6-digit TOTP code on stdin; persists to
              the profile dir at /secrets/equityzen-profile and renews
              silently if still valid. Pass --check to probe the existing
              session without asking for a code.
  download    Browse the investor surfaces and capture bronze: offerings +
              positions + cash flows (getBuyerInvestments per stage +
              getMyInvestmentDetails per offering). Pass --dry-run for a
              read-only smoke test (writes nothing); document blobs +
              tax-centre metadata are recorded by default, --no-documents skips them.
  load        Ingest bronze snapshots into the SQLite silver: offerings,
              positions, cash_flows, tax_documents,
              capital_account_statements, k1_documents. Pass --force to
              rebuild it from all bronze.
  prune       Delete non-complete dumps (crashed downloads with no
              terminal run.json) from the bronze tree, and strip
              screenshots/ (the download --debug captures) from complete
              dumps, whose load inputs are never touched. --dry-run prints
              the plan first.
  sh|bash     Open an interactive shell inside the container.
  help        Show this message.

Run "<wrapper> <subcommand> --help" for subcommand-specific flags.
EOF
        ;;
    *) exec "$verb" "$@" ;;
esac
