#!/bin/bash
# Container entrypoint. Dispatches a single positional subcommand to
# the corresponding Python script under /app/.

set -euo pipefail

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
    prune)
        # Delete non-complete dumps from the bronze tree (swissquote
        # writes no bronze-resident debug artefacts). No browser needed.
        shift
        exec python3 /app/prune.py "$@"
        ;;
    sh|bash)
        shift
        exec /bin/bash "$@"
        ;;
    help|--help|-h)
        cat <<'EOF'
swissquote container

Usage:
  <wrapper> <subcommand> [args...]

Subcommands:
  login     Mint or refresh the Playwright session state.
  download  Export bronze artefacts from the e-banking UI.
  load      Parse bronze into the silver SQLite database.
  prune     Delete non-complete dumps from the bronze tree.
            --dry-run prints the plan first.
  sh|bash   Open an interactive shell inside the container.
  help      Show this message.

Run "<wrapper> <subcommand> --help" for subcommand-specific flags.
EOF
        ;;
    *)
        # Pass-through for ad-hoc commands inside the container,
        # e.g. `docker run swissquote python -c '...'`.
        exec "$@"
        ;;
esac
