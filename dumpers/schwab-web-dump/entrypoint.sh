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
    login)
        # CLI-MFA login + keep-alive. Pre-fills the form, prompts
        # for the 2FA code on stdin, lands on the post-auth URL,
        # then HOLDS Firefox open so the live session survives
        # between `download` invocations. Schwab kills the
        # persistent profile's cookies within seconds of Firefox
        # closing — so the sibling-tool model of "login mints,
        # download reads storageState" doesn't apply here, and
        # the only way to do multiple scrapes per MFA is to keep
        # the Firefox process running.
        #
        # While the login is held, the process polls
        # /data/.download-trigger; each `download` invocation
        # from another terminal writes that file with the scrape
        # config and exits. Ctrl+C ends the session.
        start_xvfb
        shift
        exec python3 /app/login.py \
            --profile-dir /secrets/schwab-web-profile \
            --manual --cli-mfa \
            --download-trigger /data/.download-trigger "$@"
        ;;
    download)
        # Trigger a scrape against an already-running `login`
        # process. Writes the per-scrape config (mode / range /
        # with_more_detail / dest) into the trigger file the
        # login's keep-alive loop polls, then exits immediately.
        # The actual scrape runs in the login process (which has
        # the live Firefox). If no login is running, the trigger
        # file just sits there until one starts. Bronze lands at
        # <dest>/<UTC-ts>/ as produced by the login process.
        shift
        exec python3 /app/download_trigger.py "$@"
        ;;
    vnc-login)
        # Fallback to the older VNC-driven flow: start Xvfb +
        # x11vnc on the same display, then run login.py --manual
        # --no-cli-mfa. Use when CLI-MFA selectors drift or the
        # user has to satisfy a non-code challenge (security
        # question, push-to-device, etc.) that the stdin prompt
        # can't drive. The wrapper publishes 127.0.0.1:5900 so
        # the VNC port is only reachable via a host-side SSH
        # tunnel. A VNC password is required regardless (macOS
        # Screen Sharing refuses no-auth servers); we generate a
        # fresh one every launch so the same string is never
        # reusable.
        start_xvfb
        # openssl rand -hex 8 is a single command, no pipe — so
        # `set -euo pipefail` doesn't trip on SIGPIPE the way
        # `tr -dc ... | head -c 16` does.
        VNC_PASSWORD=$(openssl rand -hex 8)
        x11vnc -display ":$VFB_DISPLAY" -passwd "$VNC_PASSWORD" \
            -forever -shared -rfbport 5900 -bg \
            -o /tmp/x11vnc.log >/dev/null 2>&1
        echo "vnc-login: VNC ready on 127.0.0.1:5900" >&2
        echo "vnc-login: password (single-use):  $VNC_PASSWORD" >&2
        echo "vnc-login: tunnel from your laptop with" >&2
        echo "vnc-login:   ssh -L 5900:127.0.0.1:5900 <mbp-host>" >&2
        echo "vnc-login: then on the laptop:" >&2
        echo "vnc-login:   open vnc://localhost:5900" >&2
        shift
        # Default --profile-dir + --dest so vnc-login is one-arg.
        # Profile lives under the mounted /secrets tree so cookies
        # survive across container runs; --dest=/data triggers the
        # post-login auto-scrape. --no-cli-mfa preserves the
        # all-manual VNC flow (everything past pre-fill is the
        # operator's job) — that's the point of vnc-login. Override
        # either by passing the flag explicitly — argparse takes
        # the last value.
        exec python3 /app/login.py \
            --profile-dir /secrets/schwab-web-profile \
            --manual --no-cli-mfa --dest /data "$@"
        ;;
    download)
        start_xvfb
        shift
        exec python3 /app/download.py "$@"
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
schwab-web-dump container

Usage:
  <wrapper> <subcommand> [args...]

Subcommands:
  login       CLI-MFA login + keep-alive. Auto-submits the form,
              prompts for the 2FA code on stdin, then HOLDS
              Firefox open and polls /data/.download-trigger so
              `download` calls from another terminal scrape
              against the same live session. Ctrl+C ends the
              session. Stdin must be a TTY.
  download    Trigger a scrape against an already-running login.
              Writes the per-scrape config (mode / range /
              with_more_detail / dest) into the trigger file and
              exits. No browser, no MFA. Default range: 3 months
              (override with --range; --range Last10Years for a
              full backfill).
  load        Parse bronze into the silver SQLite database.
  vnc-login   Fallback to a VNC-driven login when CLI-MFA selectors
              drift or a non-code challenge is required. Starts
              x11vnc on 127.0.0.1:5900; tunnel + connect from your
              VNC client.
  download    Export bronze artefacts from the Schwab client UI
              (only useful while a Firefox session is live; Schwab
              kills sessions on Firefox close, so in practice this
              gets chained off vnc-login in a single Firefox).
  load        Parse bronze into the silver SQLite database.
  sh|bash     Open an interactive shell inside the container.
  help        Show this message.

Run "<wrapper> <subcommand> --help" for subcommand-specific flags.
EOF
        ;;
    *)
        # Pass-through for ad-hoc commands inside the container,
        # e.g. `docker run schwab-web-dump python -c '...'`.
        exec "$@"
        ;;
esac
