#!/bin/bash
# Container entrypoint. Dispatches a single positional subcommand
# to the corresponding Python script under /app/. The toolkit is
# REST-only — no browser, no Xvfb, no VNC; everything runs
# headlessly against VIAC's JSON API.

set -euo pipefail

# The verb engine, baked into the base image (shared/images/entrypoint-lib.sh).
# shellcheck source=/dev/null
source /opt/entrypoint-lib.sh

verb="${1:-help}"
[[ $# -gt 0 ]] && shift

case "$verb" in
    login)    run plain login.py -- "$@" ;;
    download) run plain download.py -- "$@" ;;
    load)     run plain load.py -- "$@" ;;
    prune|sh|bash) entrypoint_common "$verb" "$@" ;;
    help|--help|-h)
        cat <<'EOF'
viac container

Usage:
  <wrapper> <subcommand> [args...]

Subcommands:
  login     Mint or refresh the session cookie jar +
            CSRF metadata in /secrets/viac-state.json.
            Prompts on stdin for the mTAN code sent to
            your phone. Pass --check to probe the existing
            session without triggering a new mTAN.
  download  Bronze dump against /rest/web/. Requires a
            session minted by `login`. Use --dry-run to
            walk JSON endpoints without fetching PDFs. The
            per-event TRANSACTION PDFs download by default;
            --no-transaction-documents skips them.
  load      Parse bronze into silver SQLite. Idempotent:
            skips dumps already loaded.
  prune     Delete non-complete dumps (crashed/in-progress
            walks and legacy --dry-run shells) and `download
            --debug` HTTP traces from the bronze tree.
            --dry-run prints the plan first.
  sh|bash   Open an interactive shell inside the container.
  help      Show this message.

Run "<wrapper> <subcommand> --help" for subcommand-specific flags.
EOF
        ;;
    *) exec "$verb" "$@" ;;
esac
