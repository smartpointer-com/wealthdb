#!/bin/bash
# Container entrypoint. Dispatches a single positional subcommand to the
# corresponding Python under /app/.
#
# Chase invalidates the session when Firefox closes and challenges 2FA at
# every sign-in (DESIGN.md §F), so — like schwab-web — `download` is a
# one-shot: login + scrape in one Firefox lifetime, driven by hand over VNC
# (the human answers whatever factor Chase presents). `login` has nothing
# durable to persist, so it folds into `download` (except `login --check`, a
# read-only session probe). `load` / `prune` are pure Python, no browser.
#
# Xvfb is started directly (not via xvfb-run) — the Noble xvfb-run's SIGUSR1
# ready-signalling hangs under a non-root parent; socket-existence polling is
# version-independent.

set -euo pipefail

# shellcheck source=/dev/null
source /opt/entrypoint-lib.sh

case "${1:-help}" in
    explore)
        # Discovery harness (retained): Camoufox over VNC + trace capture.
        start_xvfb
        start_x11vnc explore
        shift
        exec python3 /app/explore.py "$@"
        ;;
    download)
        # One-shot: pre-fill → 2FA driven from the TERMINAL (no VNC) →
        # scrape. Headed Camoufox under Xvfb for stealth, but x11vnc is NOT
        # started — login.py drives the challenge over stdin. Falls back to
        # `vnc-login` when a challenge control can't be driven.
        start_xvfb
        shift
        exec python3 /app/login.py --cli-mfa --bronze-dir /data "$@"
        ;;
    vnc-login)
        # Fallback: expose VNC and let Sign In + 2FA be completed by hand in
        # the browser, then scrape. Use when the CLI-MFA drive can't find a
        # challenge control (the challenge UI selectors are trace-derived —
        # DESIGN.md §5).
        start_xvfb
        start_x11vnc vnc-login
        shift
        exec python3 /app/login.py --no-cli-mfa --bronze-dir /data "$@"
        ;;
    login)
        # `login --check` is a real read-only session probe (headless under
        # Xvfb, no VNC, no 2FA push). A bare `login` has nothing to persist
        # separately — it folds into `download` — so it is a clean no-op that
        # an orchestrator's login→download→load can call without tripping.
        shift
        for a in "$@"; do
            if [[ "$a" == "--check" ]]; then
                start_xvfb
                exec python3 /app/login.py "$@"
            fi
        done
        echo "chase: 'login' folds into 'download' — nothing to persist" \
             "(the session dies with Firefox; §F)." >&2
        echo "  Run 'download' (one-shot login + scrape), or 'login --check'" \
             "to probe a session." >&2
        exit 0
        ;;
    load)
        # Silver loader — pure SQLite + Python, no browser.
        shift
        exec python3 /app/load.py --bronze-dir /data "$@"
        ;;
    prune)
        # Bronze reclaim — pure file walk. Also reachable host-side via the
        # wrapper (the usual path).
        shift
        exec python3 /app/prune.py "$@"
        ;;
    sh|bash)
        shift
        exec /bin/bash "$@"
        ;;
    help|--help|-h)
        cat <<'EOF'
chase container

Usage:
  <wrapper> <subcommand> [args...]

Subcommands:
  explore     Discovery harness: Camoufox over VNC + HAR/trace/click log
              under /debug. See DESIGN.md "Observed".
  download    One-shot login + scrape (deposit accounts), 2FA driven from
              the TERMINAL — no VNC. Pre-fills + submits the sign-in form,
              prompts for the code on stdin (push / SMS / voice), then
              exports each account's activity (CSV + QFX) and statement
              PDFs into a UTC-stamped bronze run dir under /data.
              --lookback bounds the window; --no-documents skips statements;
              --dry-run walks without exporting.
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
    *)
        exec "$@"
        ;;
esac
