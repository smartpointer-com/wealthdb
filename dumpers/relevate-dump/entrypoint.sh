#!/bin/bash
# Container entrypoint. Dispatches a single positional subcommand
# to the corresponding Python script under /app/. The toolkit is
# REST-only — no Xvfb, no x11vnc, no Chromium; everything runs
# headlessly.

set -euo pipefail

case "${1:-help}" in
    login)
        # Mint or refresh the Airlock cookie jar. Prompts on stdin
        # for the mTAN code. Pass --check to probe the existing
        # session without triggering a new mTAN.
        shift
        exec python3 /app/login.py "$@"
        ;;
    download)
        # Bronze dump against /middlelayer/v2/. Loads the persisted
        # cookie jar from /secrets/relevate-state.json. Use
        # --dry-run to enumerate without per-portfolio /
        # per-document fetches.
        shift
        exec python3 /app/download.py "$@"
        ;;
    load)
        # Parse bronze into silver SQLite.
        shift
        exec python3 /app/load.py "$@"
        ;;
    sh|bash)
        shift
        exec /bin/bash "$@"
        ;;
    help|--help|-h)
        cat <<'EOF'
relevate-dump container

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
  sh|bash   Open an interactive shell inside the container.
  help      Show this message.

Run "<wrapper> <subcommand> --help" for subcommand-specific flags.
EOF
        ;;
    *)
        # Pass-through for ad-hoc commands inside the container,
        # e.g. `docker run relevate-dump python -c '...'`.
        exec "$@"
        ;;
esac
