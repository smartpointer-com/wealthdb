#!/bin/bash
# Container entrypoint. Dispatches a single positional subcommand to the
# corresponding Python under /app/.
#
# The collector is at its discovery stage: `explore` records a George
# session driven by hand over VNC, and `prune` is a pure file walk.
# `login`, `download` and `load` are accepted and exit 2 until they are
# built from the explore captures (DESIGN.md §F decides their split).

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
    login|download|load)
        echo "erstebank_at: '$1' is not built yet — the collector is at its" \
             "discovery stage (DESIGN.md §H lists what explore must answer)." >&2
        echo "  Run 'explore' to record a George session over VNC." >&2
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
  login       Not built yet (exits 2).
  download    Not built yet (exits 2).
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
