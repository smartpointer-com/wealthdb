#!/bin/bash
# Container entrypoint. Dispatches a single positional subcommand
# to the corresponding Python script under /app/. The toolkit is
# REST-only — no browser, no Xvfb, no VNC; everything runs
# headlessly against VIAC's JSON API.

set -euo pipefail

case "${1:-help}" in
    login)
        # Mint or refresh the session cookie jar + CSRF metadata
        # in /secrets/viac-state.json. Prompts on stdin for the
        # mTAN code sent to the user's phone. --check probes an
        # existing session without triggering a new mTAN push.
        shift
        exec python3 /app/login.py "$@"
        ;;
    download)
        # Bronze dump against /rest/web/. Loads the persisted
        # cookie jar from /secrets/viac-state.json (mint with
        # `login` first). --dry-run walks JSON endpoints but
        # skips PDF binaries; --with-transaction-documents
        # downloads the per-event TRANSACTION PDFs in addition
        # to the default-tier documents.
        shift
        exec python3 /app/download.py "$@"
        ;;
    load)
        # Parse bronze into silver SQLite. Idempotent: skips
        # dumps already loaded (tracked via dump_runs.snapshot_at).
        shift
        exec python3 /app/load.py "$@"
        ;;
    sh|bash)
        shift
        exec /bin/bash "$@"
        ;;
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
            walk JSON endpoints without fetching PDFs;
            --with-transaction-documents to also download
            the per-event TRANSACTION PDFs.
  load      Parse bronze into silver SQLite. Idempotent:
            skips dumps already loaded.
  sh|bash   Open an interactive shell inside the container.
  help      Show this message.

Run "<wrapper> <subcommand> --help" for subcommand-specific flags.
EOF
        ;;
    *)
        # Pass-through for ad-hoc commands inside the container,
        # e.g. `docker run viac python -c '...'`.
        exec "$@"
        ;;
esac
