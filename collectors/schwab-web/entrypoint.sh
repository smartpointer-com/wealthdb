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

VFB_DISPLAY=99

# Symlink the pre-staged camoufox cache (populated at image build
# time by `python3 -m camoufox fetch` and copied to /opt) into
# the runtime user's $HOME/.cache/camoufox so camoufox skips its
# 700MB first-run download. HOME is /tmp inside the container
# (set by the wrapper for the non-root user), and we ALWAYS
# overwrite the symlink so a stale /tmp/.cache/camoufox from a
# previous container with a different runtime uid doesn't shadow
# the staged tree.
mkdir -p /tmp/.cache
if [[ -d /opt/camoufox-cache && ! -e /tmp/.cache/camoufox ]]; then
    ln -snf /opt/camoufox-cache /tmp/.cache/camoufox
fi

start_xvfb() {
    Xvfb ":$VFB_DISPLAY" -screen 0 1280x800x24 -nolisten tcp \
        >/tmp/xvfb.log 2>&1 &
    for _ in $(seq 1 50); do
        if [[ -S "/tmp/.X11-unix/X$VFB_DISPLAY" ]]; then
            export DISPLAY=":$VFB_DISPLAY"
            return 0
        fi
        sleep 0.1
    done
    echo "entrypoint: Xvfb failed to start within 5s; /tmp/xvfb.log:" >&2
    tail -20 /tmp/xvfb.log >&2 || true
    return 1
}

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
            --cli-mfa --dest /data "$@"
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
        # openssl rand -hex 8 is a single command, no pipe — so
        # `set -euo pipefail` doesn't trip on SIGPIPE the way
        # `tr -dc ... | head -c 16` does.
        VNC_PASSWORD=$(openssl rand -hex 8)
        x11vnc -display ":$VFB_DISPLAY" -passwd "$VNC_PASSWORD" \
            -forever -shared -rfbport 5900 -bg \
            -o /tmp/x11vnc.log >/dev/null 2>&1
        # The wrapper picks the host-side port (it knows which
        # ones are free); we publish it through this env var so
        # the messages below print the actual port the operator
        # needs to tunnel. Defaults to 5900 for direct
        # `docker run` invocations that skip the wrapper.
        host_port="${VNC_HOST_PORT:-5900}"
        echo "vnc-login: VNC ready on 127.0.0.1:${host_port}" >&2
        echo "vnc-login: password (single-use):  $VNC_PASSWORD" >&2
        echo "vnc-login: tunnel from your laptop with" >&2
        echo "vnc-login:   ssh -L ${host_port}:127.0.0.1:${host_port} <mbp-host>" >&2
        echo "vnc-login: then on the laptop:" >&2
        echo "vnc-login:   open vnc://localhost:${host_port}" >&2
        shift
        exec python3 /app/login.py \
            --profile-dir /secrets/schwab-web-profile \
            --no-cli-mfa --dest /data "$@"
        ;;
    load)
        shift
        exec python3 /app/load.py "$@"
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
              months (override with --range; --range Last10Years
              for a full backfill). Stdin must be a TTY.
  load        Parse bronze into the silver SQLite database.
  vnc-login   Fallback to a VNC-driven login + scrape when the
              CLI-MFA selectors drift or a non-code challenge is
              required. Starts x11vnc on 127.0.0.1:5900; tunnel
              + connect from your local VNC client.
  sh|bash     Open an interactive shell inside the container.
  help        Show this message.

Run "<wrapper> <subcommand> --help" for subcommand-specific flags.
EOF
        ;;
    *)
        # Pass-through for ad-hoc commands inside the container,
        # e.g. `docker run schwab-web python -c '...'`.
        exec "$@"
        ;;
esac
