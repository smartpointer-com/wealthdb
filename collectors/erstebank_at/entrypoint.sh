#!/bin/bash
# Container entrypoint. Dispatches a single positional subcommand to the
# corresponding Python under /app/.
#
# A George sign-in lasts a few hours and no device trust skips the push
# after it (DESIGN.md §F), so `download` is a one-shot: a live session
# reused, or a sign-in with the push approved on the phone (nothing to type,
# no VNC), then the REST fetch, in one Camoufox lifetime. `login` folds into
# `download` (except `login --check`, a read-only session probe).
# `vnc-login` is the by-hand fallback. `load` is not built yet (exits 2).

set -euo pipefail

# shellcheck source=/dev/null
source /opt/entrypoint-lib.sh

case "${1:-help}" in
    explore)
        # Discovery harness: Camoufox over VNC + HAR/network log/click log/
        # DOM snapshots/response bodies under /debug.
        shift
        exec_if_help explore.py "$@"
        start_xvfb
        start_x11vnc explore
        exec python3 /app/explore.py "$@"
        ;;
    download)
        # One-shot: submit the user name → push approved on the phone →
        # REST fetch. Headed Camoufox under Xvfb, no VNC (nothing to type).
        shift
        exec_if_help login.py "$@"
        start_xvfb
        exec python3 /app/login.py --cli-mfa --bronze-dir /data "$@"
        ;;
    vnc-login)
        # Fallback: expose VNC and do the whole sign-in by hand, then fetch.
        shift
        exec_if_help login.py "$@"
        start_xvfb
        start_x11vnc vnc-login
        exec python3 /app/login.py --no-cli-mfa --bronze-dir /data "$@"
        ;;
    login)
        # `login --check` is a read-only session probe (no push). A bare
        # `login` folds into `download`, which signs in when needed, so it
        # is a clean no-op an orchestrator's login → download → load can
        # call without tripping.
        shift
        exec_if_help login.py "$@"
        for a in "$@"; do
            if [[ "$a" == "--check" ]]; then
                start_xvfb
                exec python3 /app/login.py "$@"
            fi
        done
        echo "erstebank_at: 'login' folds into 'download', which signs in" \
             "only when no session is alive (DESIGN.md §F)." >&2
        echo "  Run 'download' (one-shot sign-in + fetch), or 'login --check'" \
             "to probe a session." >&2
        exit 0
        ;;
    load)
        echo "erstebank_at: 'load' is not built yet." >&2
        exit 2
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
erstebank_at container

Usage:
  <wrapper> <subcommand> [args...]

Subcommands:
  explore     Discovery harness: Camoufox over VNC + HAR/network log/click
              log/DOM snapshots/response bodies under /debug. See
              DESIGN.md §2-§3.
  download    One-shot sign-in + REST fetch (checking accounts and credit
              cards), the push approved in the George app — no VNC,
              nothing to type. Fetches the rosters, each history, and the
              statement PDFs into a UTC-stamped bronze run dir under /data.
              --lookback bounds the window; --no-documents skips PDFs;
              --dry-run reads the rosters only (it still signs in).
  vnc-login   Fallback for download: expose VNC and do the sign-in by
              hand, then fetch.
  login       Folds into download, which signs in only without a live
              session. 'login --check' probes the session read-only
              (exit 0 alive / 1 dead; no push).
  load        Not built yet (exits 2).
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
