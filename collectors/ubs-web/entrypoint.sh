#!/bin/bash
# Container entrypoint. Dispatches a single positional subcommand to
# the corresponding Python script under /app/.

set -euo pipefail

# Shared Xvfb / x11vnc bootstrap (start_xvfb, start_x11vnc), baked into
# base-playwright. Only `explore` uses it: login and download drive
# Chromium headless, and the QR challenge is rendered to the terminal.
# shellcheck source=/dev/null
source /opt/entrypoint-lib.sh

case "${1:-help}" in
    login)
        shift
        exec python3 /app/login.py "$@"
        ;;
    download)
        shift
        exec python3 /app/download.py "$@"
        ;;
    load)
        shift
        exec python3 /app/load.py "$@"
        ;;
    explore)
        # Discovery harness: headed Chromium on a virtual display, served
        # over VNC so the session is driven by hand. The harness only
        # records — every navigation and click is the operator's. Artefacts
        # land in the /debug mount, never bronze.
        start_xvfb
        start_x11vnc explore
        shift
        exec python3 /app/explore.py "$@"
        ;;
    prune)
        # Delete non-complete dumps (crashed walks, --dry-run shells)
        # from the bronze tree. No browser, no Xvfb — a pure file walk
        # over the /data mount.
        shift
        exec python3 /app/prune.py "$@"
        ;;
    sh|bash)
        shift
        exec /bin/bash "$@"
        ;;
    help|--help|-h)
        cat <<'EOF'
ubs-web container

Usage:
  <wrapper> <subcommand> [args...]

Subcommands:
  login     Mint or refresh the Playwright session state.
  download  Export bronze artefacts from the netbanking UI.
  load      Parse bronze into the silver SQLite database.
  explore   Record a hand-driven session over VNC into /debug.
  prune     Delete non-complete dumps from the bronze tree.
            --dry-run prints the plan first.
  sh|bash   Open an interactive shell inside the container.
  help      Show this message.

Run "<wrapper> <subcommand> --help" for subcommand-specific flags.
EOF
        ;;
    *)
        # Pass-through for ad-hoc commands inside the container,
        # e.g. `docker run ubs-web python -c '...'`.
        exec "$@"
        ;;
esac
