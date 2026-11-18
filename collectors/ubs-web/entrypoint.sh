#!/bin/bash
# Container entrypoint. Dispatches a single positional subcommand to
# the corresponding Python script under /app/.

set -euo pipefail

# The verb engine, baked into the base image (shared/images/entrypoint-lib.sh).
# shellcheck source=/dev/null
source /opt/entrypoint-lib.sh

verb="${1:-help}"
[[ $# -gt 0 ]] && shift

case "$verb" in
    # Chromium headless; the QR challenge is rendered to the terminal.
    login)    run plain login.py -- "$@" ;;
    download) run plain download.py -- "$@" ;;
    load)     run plain load.py -- "$@" ;;
    explore)  run vnc:explore explore.py -- "$@" ;;
    prune|sh|bash) entrypoint_common "$verb" "$@" ;;
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
    *) exec "$verb" "$@" ;;
esac
