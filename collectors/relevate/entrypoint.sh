#!/bin/bash
# Container entrypoint. Dispatches a single positional subcommand
# to the corresponding Python script under /app/. The toolkit is
# REST-only — no Xvfb, no x11vnc, no Chromium; everything runs
# headlessly.

set -euo pipefail

# The verb engine, baked into the base image (shared/images/entrypoint-lib.sh).
# shellcheck source=/dev/null
source /opt/entrypoint-lib.sh

verb="${1:-help}"
[[ $# -gt 0 ]] && shift

case "$verb" in
    # Prompts on stdin for the mTAN code; --check probes the session.
    login)    run plain login.py -- "$@" ;;
    download) run plain download.py -- "$@" ;;
    load)     run plain load.py -- "$@" ;;
    prune|sh|bash) entrypoint_common "$verb" "$@" ;;
    help|--help|-h)
        cat <<'EOF'
relevate container

Usage:
  <wrapper> <subcommand> [args...]

Subcommands:
  login     Mint or refresh the Airlock session cookies in
            /secrets/relevate-state.json. Prompts on stdin for
            the mTAN code sent to your phone. Pass --check to
            probe the existing session without triggering a new
            mTAN.
  download  Bronze dump against /middlelayer/v2/. Pre-requisite:
            a session minted by `login` is present in
            /secrets/relevate-state.json. Use --dry-run to
            enumerate without per-portfolio / per-document
            fetches. Use --mode + --limit-* for iteration-cheap
            reruns.
  load      Parse bronze into silver SQLite. Idempotent: skips
            dumps already loaded (tracked via dump_runs.snapshot_at).
            Default reads /data, writes /data/relevate.db.
  prune     Delete non-complete dumps (crashed walks + --dry-run
            shells) from the bronze tree. --dry-run prints the plan
            first; --min-age-hours guards in-flight downloads.
  sh|bash   Open an interactive shell inside the container.
  help      Show this message.

Run "<wrapper> <subcommand> --help" for subcommand-specific flags.
EOF
        ;;
    *) exec "$verb" "$@" ;;
esac
