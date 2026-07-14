#!/bin/bash
# Container entrypoint. Dispatches a single positional subcommand
# to the corresponding Python script under /app/. The browser-
# driving subcommands (login, vnc-login, download) start an Xvfb
# virtual X11 display first so Firefox can run headed (Schwab's
# anti-bot detection flags headless markers, so we don't have
# the option of running without a display).
#
# Xvfb is started directly rather than via `xvfb-run`. The Ubuntu
# Noble xvfb-run script's SIGUSR1 ready-signaling hangs when the
# parent shell is non-root: it sets the parent SIGUSR1 handler to
# `:` rather than SIG_IGN, so Xvfb declines to send the ready
# signal and the parent waits forever. Socket-existence polling
# is a version-independent ready signal.

set -euo pipefail

# Xvfb / x11vnc / camoufox-cache bootstrap (VFB_DISPLAY, start_xvfb,
# start_x11vnc) is shared across the camoufox collectors, baked into
# base-camoufox at /opt/entrypoint-lib.sh.
# shellcheck source=/dev/null
source /opt/entrypoint-lib.sh

case "${1:-help}" in
    download)
        # One-shot: CLI-MFA login → scrape → exit. Schwab
        # invalidates the persistent profile's cookies within
        # seconds of Firefox closing, so login + scrape must
        # happen in one Firefox lifetime — each invocation pays
        # one MFA challenge, in exchange for not having to
        # babysit a long-lived process. Stdin must be a TTY.
        start_xvfb
        shift
        exec python3 /app/login.py \
            --profile-dir /secrets/schwab-web-profile \
            --cli-mfa --bronze-dir /data "$@"
        ;;
    vnc-login)
        # Fallback: start x11vnc on the same Xvfb display and
        # run the login + scrape flow with --no-cli-mfa, so the
        # operator can drive Log In + 2FA from a local VNC
        # client. Use when CLI-MFA selectors drift or a non-
        # code challenge (security question, push-to-device) is
        # required. A fresh VNC password is generated each
        # launch and printed to stderr; the wrapper publishes
        # the port on 127.0.0.1 only (typically 5900, but moves
        # +1 each time 5900 is already taken on the host) —
        # tunnel from your laptop with ssh -L.
        start_xvfb
        start_x11vnc vnc-login
        shift
        exec python3 /app/login.py \
            --profile-dir /secrets/schwab-web-profile \
            --no-cli-mfa --bronze-dir /data "$@"
        ;;
    load)
        shift
        exec python3 /app/load.py "$@"
        ;;
    collapse-statements)
        # Parse-equivalence collapse of the statement bronze: fold
        # re-rendered statement PDFs that parse identically onto the
        # oldest copy. Distinct from the fleet `wealthdb-collect dedup`
        # sweep (byte-identical hardlinks) — this deletes non-identical
        # bytes that merely parse the same. Needs the image's pypdfium2
        # for text extraction, so it runs in-container like `load` (no
        # browser, no Xvfb). --dry-run emits the evidence report and
        # collapses nothing.
        shift
        exec python3 /app/dedup.py "$@"
        ;;
    prune)
        # Delete debug artefacts (<run>/screenshots/) and
        # non-complete dumps from the bronze tree. No browser, no
        # Xvfb. Also reachable host-side via the wrapper.
        shift
        exec python3 /app/prune.py "$@"
        ;;
    sh|bash)
        shift
        exec /bin/bash "$@"
        ;;
    help|--help|-h)
        cat <<'EOF'
schwab-web container

Usage:
  <wrapper> <subcommand> [args...]

Subcommands:
  download    One-shot CLI-MFA login + scrape. Pre-fills the
              form from SCHWAB_LOGIN_ID / SCHWAB_PASSWORD,
              auto-submits, prompts for the 2FA code on stdin,
              runs the statements + tx-history download in the
              same Firefox session, exits. Default range: 3
              months (widen with --lookback; --lookback all for a
              full backfill, capped at Schwab's ~10 years). Stdin
              must be a TTY.
  load        Parse bronze into the silver SQLite database.
  collapse-statements
              Collapse re-rendered statement PDFs that parse
              identically onto the oldest copy (reclaims the
              per-download byte churn the fleet `wealthdb-collect
              dedup` byte-identical sweep can't). --dry-run prints
              the evidence report first.
  prune       Delete debug artefacts (<run>/screenshots/) and
              non-complete dumps from the bronze tree. --dry-run
              prints the plan first.
  vnc-login   Fallback to a VNC-driven login + scrape when the
              CLI-MFA selectors drift or a non-code challenge is
              required. Starts x11vnc on 127.0.0.1:5900; tunnel
              + connect from your local VNC client.
  sh|bash     Open an interactive shell inside the container.
  help        Show this message.

Run "<wrapper> <subcommand> --help" for subcommand-specific flags.
EOF
        ;;
    login)
        # `login` is a natural thing to type, but schwab-web mints its
        # session inside `download` (one-shot CLI-MFA scrape) and persists
        # nothing separately — so login is a clean no-op (exit 0). Catch it
        # explicitly: the host wrapper already traps it, and this closes the
        # direct-`docker run schwab-web login` path, which would otherwise
        # fall through to the container's util-linux /bin/login and die.
        echo "schwab-web: 'login' folds into 'download' — nothing to persist." >&2
        echo "  Run 'download' (one-shot CLI-MFA scrape), or 'vnc-login'." >&2
        exit 0
        ;;
    *)
        # Pass-through for ad-hoc commands inside the container,
        # e.g. `docker run schwab-web python -c '...'`.
        exec "$@"
        ;;
esac
