#!/bin/bash
# Container entrypoint. Dispatches a single positional subcommand
# to the corresponding Python script under /app/. The browser-
# driving subcommands (login, vnc-login, download) start an Xvfb
# virtual X11 display first so Chromium can run headed when needed
# — vanilla Playwright headless will be the starting position
# (see DESIGN.md §6), but Xvfb is available either way so we can
# escalate without rebuilding.
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
# time by `python3 -m camoufox fetch` and copied to /opt) into the
# runtime user's $HOME/.cache/camoufox so camoufox skips its 700MB
# first-run download. HOME is /tmp inside the container (set by the
# wrapper for the non-root user), and we ALWAYS overwrite the
# symlink so a stale /tmp/.cache/camoufox from a previous container
# with a different runtime uid doesn't shadow the staged tree.
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
    login)
        # Default flow: drive Chromium through the Fidelity login
        # form, prompt for the 2FA code on stdin (Duo / Google
        # Authenticator / Symantec VIP, whichever the user has
        # configured), persist storageState.json under /secrets.
        # Subsequent `download` runs reuse the cookie until Fidelity
        # invalidates it.
        start_xvfb
        shift
        exec python3 /app/login.py "$@"
        ;;
    download)
        # In-container fallback: write the trigger file the running
        # `login` keep-alive loop is polling. The host wrapper
        # normally handles `download` directly (host-side file
        # write, no docker spawn) — this case fires only when the
        # container's entrypoint is invoked without the wrapper.
        # No browser needed; the scrape runs in the login process.
        shift
        exec python3 /app/download_trigger.py "$@"
        ;;
    vnc-login)
        # Fallback to a VNC-driven login when CLI-MFA selectors
        # drift or the user has to satisfy a non-code challenge
        # (security question, knowledge-based questions, etc.) that
        # the stdin prompt can't drive. Start x11vnc on the Xvfb
        # display; the wrapper publishes 127.0.0.1:5900 so the VNC
        # port is only reachable via a host-side SSH tunnel. A VNC
        # password is required regardless (macOS Screen Sharing
        # refuses no-auth servers); we generate a fresh one every
        # launch so the same string is never reusable.
        start_xvfb
        VNC_PASSWORD=$(openssl rand -hex 8)
        x11vnc -display ":$VFB_DISPLAY" -passwd "$VNC_PASSWORD" \
            -forever -shared -rfbport 5900 -bg \
            -o /tmp/x11vnc.log >/dev/null 2>&1
        echo "vnc-login: VNC ready on 127.0.0.1:5900" >&2
        echo "vnc-login: password (single-use):  $VNC_PASSWORD" >&2
        echo "vnc-login: tunnel from your laptop with" >&2
        echo "vnc-login:   ssh -L 5900:127.0.0.1:5900 <host>" >&2
        echo "vnc-login: then on the laptop:" >&2
        echo "vnc-login:   open vnc://localhost:5900" >&2
        shift
        exec python3 /app/login.py --vnc "$@"
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
fidelity-web-dump container

Usage:
  <wrapper> <subcommand> [args...]

Subcommands:
  login       Mint or refresh the Playwright session state. Prompts
              on stdin for the Fidelity 2FA code (Duo / Authenticator
              / Symantec VIP). Long timeout — the user does not need
              to be at the keyboard immediately.
  download    Export bronze artefacts (positions, transactions,
              statements) from fidelity.com.
  load        Parse bronze into the silver SQLite database.
  vnc-login   Fallback to a VNC-driven login when CLI-MFA selectors
              drift or a non-code challenge is required. Starts
              x11vnc on 127.0.0.1:5900; tunnel + connect from your
              VNC client.
  sh|bash     Open an interactive shell inside the container.
  help        Show this message.

Run "<wrapper> <subcommand> --help" for subcommand-specific flags.
EOF
        ;;
    *)
        # Pass-through for ad-hoc commands inside the container,
        # e.g. `docker run fidelity-web-dump python -c '...'`.
        exec "$@"
        ;;
esac
