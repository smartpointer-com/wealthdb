#!/bin/bash
# Container entrypoint. Dispatches a single positional subcommand
# to the corresponding Python script under /app/. The browser-
# driving subcommands (download, vnc-login) start an Xvfb virtual
# X11 display first so Camoufox can run headed.
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
        # One-shot: login → walk → logout → exit. Drives Camoufox
        # through the Fidelity login form, prompts for the 2FA
        # code on stdin (Duo / Google Authenticator / Symantec VIP,
        # whichever is configured), runs the requested
        # walk phases, attempts a clean logout, exits.
        start_xvfb
        shift
        exec python3 /app/download.py "$@"
        ;;
    vnc-login)
        # First-time profile-dir seed: drive Camoufox to the
        # pre-filled login form, then HAND OFF to the operator via
        # VNC. Start x11vnc on the Xvfb display; the wrapper publishes
        # the port on 127.0.0.1 only (typically 5900, but moves +1
        # each time 5900 is already taken on the host) — tunnel from
        # your laptop with ssh -L. A VNC password is required
        # regardless (macOS Screen Sharing refuses no-auth servers);
        # we generate a fresh one every launch.
        #
        # By default the walk runs after the VNC-driven login lands;
        # pass `--mode none` to just seed the profile dir and exit
        # without scraping.
        start_xvfb
        start_x11vnc vnc-login
        shift
        exec python3 /app/download.py --vnc "$@"
        ;;
    load)
        shift
        exec python3 /app/load.py "$@"
        ;;
    prune)
        # Delete debug artefacts (<run>/screenshots/) and
        # non-complete dumps from the bronze tree. No browser, no
        # Xvfb. Also reachable host-side via the wrapper.
        shift
        exec python3 /app/prune.py "$@"
        ;;
    recompress)
        # One-time backlog sweep: replace plain HTML/CSV bronze
        # (balances/performance HTML, positions/activity/statement CSV)
        # inside complete dumps with verified .zst twins (the form
        # download now writes). No browser, no Xvfb. Manual only —
        # never schedule; review --dry-run first. Also reachable
        # host-side via the wrapper.
        shift
        exec python3 /app/recompress.py "$@"
        ;;
    sh|bash)
        shift
        exec /bin/bash "$@"
        ;;
    help|--help|-h)
        cat <<'EOF'
fidelity-web container

Usage:
  <wrapper> <subcommand> [args...]

Subcommands:
  download    One-shot login → walk → logout → exit. Prompts on stdin
              for the Fidelity 2FA code; auto-skips MFA when the
              device-trust cookie in --profile-dir is still valid.
              Pass --check to only validate the session (no walk).
  vnc-login   First-time profile-dir seed: pre-fills credentials,
              hands off to a VNC client for the human-driven click +
              2FA, then continues with the walk. Starts x11vnc on
              the first free host port in 127.0.0.1:5900-6000 (the
              wrapper picks; printed at handoff). Pass `--mode none`
              to skip the walk and just seed cookies.
  load        Parse bronze into the silver SQLite database.
  prune       Delete debug artefacts and non-complete dumps from
              the bronze tree. --dry-run prints the plan first.
  recompress  Convert the pre-compression bronze backlog: replace
              plain HTML/CSV inside complete dumps with sha256-
              verified .zst twins. Manual one-time sweep; --dry-run
              prints the plan first.
  sh|bash     Open an interactive shell inside the container.
  help        Show this message.

Run "<wrapper> <subcommand> --help" for subcommand-specific flags.
EOF
        ;;
    login)
        # `login` is a natural thing to type (most collectors have
        # it) but fidelity-web folds it into `download`: the Fidelity
        # session lives only for the browser's lifetime, so a login that
        # exits leaves nothing reusable — a clean no-op (exit 0). Catch it
        # explicitly — otherwise it falls through to the pass-through below
        # and execs the container's /bin/login (util-linux), which aborts
        # with the baffling "Cannot possibly work without effective root"
        # (it needs euid 0; the container runs as a non-root user). The host
        # wrapper traps it first; this closes the direct-`docker run` path.
        echo "fidelity-web: 'login' folds into 'download' — nothing to persist." >&2
        echo "  The Fidelity session is browser-lifetime only, so" >&2
        echo "  there's nothing to persist from a login-and-exit." >&2
        echo "  Use instead:" >&2
        echo "    vnc-login   first-time / re-auth via VNC-assisted MFA" >&2
        echo "    download    one-shot login -> walk (login is folded in)" >&2
        exit 0
        ;;
    *)
        # Pass-through for ad-hoc commands inside the container,
        # e.g. `docker run fidelity-web python -c '...'`. Note this
        # execs the literal argv, so an unrecognised subcommand-like
        # word runs as a system command (see the `login` arm above
        # for why we special-case the most likely such mistake).
        exec "$@"
        ;;
esac
