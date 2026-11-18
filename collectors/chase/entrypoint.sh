#!/bin/bash
# Container entrypoint. Dispatches a single positional subcommand to the
# corresponding Python under /app/.
#
# Chase invalidates the session when Firefox closes and challenges 2FA at
# every sign-in (DESIGN.md §F), so — like schwab-web — `download` is a
# one-shot: login + scrape in one Firefox lifetime, with the challenge driven
# from the terminal (`vnc-login` is the by-hand fallback). `login` has nothing
# durable to persist, so it folds into `download` (except `login --check`, a
# read-only session probe). `load` / `prune` are pure Python, no browser.

set -euo pipefail

# The verb dispatch (run, login_folds, entrypoint_common) and the Xvfb /
# x11vnc / camoufox-cache bootstrap, baked into the base image at
# /opt/entrypoint-lib.sh.
# shellcheck source=/dev/null
source /opt/entrypoint-lib.sh

verb="${1:-help}"
[[ $# -gt 0 ]] && shift

case "$verb" in
    explore)   run vnc:explore explore.py -- "$@" ;;
    # One-shot: pre-fill, 2FA driven from the TERMINAL, scrape. Headed
    # Camoufox under Xvfb for stealth, no VNC.
    download)  run xvfb login.py --cli-mfa --bronze-dir /data -- "$@" ;;
    # Fallback: Sign In + 2FA by hand over VNC, then scrape.
    vnc-login) run vnc:vnc-login login.py --no-cli-mfa --bronze-dir /data -- "$@" ;;
    login)
        # `login --check` is a read-only session probe under Xvfb.
        login_folds xvfb login.py -- "$@" <<'EOF'
chase: 'login' folds into 'download' — nothing to persist (the session dies with Firefox; §F).
  Run 'download' (one-shot login + scrape), or 'login --check' to probe a session.
EOF
        ;;
    load)      run plain load.py --bronze-dir /data -- "$@" ;;
    prune|sh|bash) entrypoint_common "$verb" "$@" ;;
    help|--help|-h)
        cat <<'EOF'
chase container

Usage:
  <wrapper> <subcommand> [args...]

Subcommands:
  explore     Discovery harness: Camoufox over VNC + HAR/trace/click log
              under /debug. See DESIGN.md "Source behaviour".
  download    One-shot login + scrape (deposit accounts + credit cards), 2FA
              driven from the TERMINAL — no VNC. Pre-fills + submits the form,
              prompts for the code on stdin (push / SMS / voice), then
              exports each account's activity (CSV + QFX) and statement
              PDFs into a UTC-stamped bronze run dir under /data.
              --lookback bounds the statement pass (the activity export
              always fetches Chase's full ~24 months); --no-documents
              skips statements; --dry-run walks without exporting.
  vnc-login   Fallback for download when the CLI 2FA can't drive a challenge
              control: exposes VNC and lets Sign In + 2FA be done by hand.
  login       Folds into download (nothing to persist). 'login --check'
              probes a session read-only (exit 0 alive / 1 dead; dead
              between runs is expected).
  load        Ingest bronze into the SQLite silver (accounts, transactions,
              statements). --force rebuilds from all bronze.
  prune       Delete non-complete dumps from the bronze tree. --dry-run
              prints the plan. Usually run host-side via the wrapper.
  sh|bash     Open an interactive shell.
  help        Show this message.

Run "<wrapper> <subcommand> --help" for subcommand flags.
EOF
        ;;
    *) exec "$verb" "$@" ;;
esac
