#!/bin/bash
# Container entrypoint. Dispatches a single positional subcommand to the
# corresponding Python under /app/.
#
# Mein ELBA fires a pushTAN at every sign-in and keeps no session across
# browser restarts (DESIGN.md §F), so — like chase — `download` is a
# one-shot: login + REST fetch in one Firefox lifetime, the pushTAN approved
# on the phone (no code to type, no VNC). `login` has nothing durable to
# persist, so it folds into `download` (except `login --check`, a read-only
# session probe). `vnc-login` is the by-hand fallback. `load` is Phase 4.
# `prune` is a pure file walk.
#
# Xvfb is started directly (not via xvfb-run) — the Noble xvfb-run's SIGUSR1
# ready-signalling hangs under a non-root parent; socket-existence polling is
# version-independent.

set -euo pipefail

# shellcheck source=/dev/null
source /opt/entrypoint-lib.sh

case "${1:-help}" in
    explore)
        # Discovery harness (retained): Camoufox over VNC + HAR/network log/
        # click log/DOM snapshots under /debug.
        start_xvfb
        start_x11vnc explore
        shift
        exec python3 /app/explore.py "$@"
        ;;
    download)
        # One-shot: fill/confirm the login → pushTAN approved on the phone →
        # REST fetch. Headed Camoufox under Xvfb, no VNC (nothing to type).
        # Falls back to `vnc-login` when a control can't be driven.
        start_xvfb
        shift
        exec python3 /app/login.py --cli-mfa --bronze-dir /data "$@"
        ;;
    vnc-login)
        # Fallback: expose VNC and complete the whole login (form/card +
        # pushTAN) by hand, then fetch. Use when the automated form-drive
        # can't find a control.
        start_xvfb
        start_x11vnc vnc-login
        shift
        exec python3 /app/login.py --no-cli-mfa --bronze-dir /data "$@"
        ;;
    login)
        # `login --check` is a real read-only session probe (headless under
        # Xvfb, no VNC, no pushTAN). A bare `login` has nothing to persist
        # separately — it folds into `download` — so it is a clean no-op that
        # an orchestrator's login→download→load can call without tripping.
        shift
        for a in "$@"; do
            if [[ "$a" == "--check" ]]; then
                start_xvfb
                exec python3 /app/login.py "$@"
            fi
        done
        echo "raiffeisen_at: 'login' folds into 'download' — nothing to" \
             "persist (the pushTAN fires every sign-in; §F)." >&2
        echo "  Run 'download' (one-shot login + fetch), or 'login --check'" \
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
raiffeisen_at container

Usage:
  <wrapper> <subcommand> [args...]

Subcommands:
  explore     Discovery harness: Camoufox over VNC + HAR/network log/click
              log/DOM snapshots under /debug. See DESIGN.md §3-Observed.
  download    One-shot login + REST fetch (deposit accounts), the pushTAN
              approved on your phone — no VNC, nothing to type. Fills the
              region/Verfüger/PIN form (or clicks the saved-user card),
              then fetches each account's history + daily balances +
              statement PDFs into a UTC-stamped bronze run dir under /data.
              --lookback bounds the window; --no-documents skips statements;
              --dry-run enumerates without fetching; --fresh forces the cold
              form.
  vnc-login   Fallback for download: expose VNC and complete the login by
              hand (form/card + pushTAN), then fetch.
  login       Folds into download (nothing to persist). 'login --check'
              probes a session read-only (exit 0 alive / 1 dead; dead
              between runs is expected).
  load        Ingest bronze into the SQLite silver (accounts, transactions,
              daily balances, statements). --force rebuilds from all bronze.
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
