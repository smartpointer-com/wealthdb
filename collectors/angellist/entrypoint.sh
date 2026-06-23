#!/bin/bash
# Container entrypoint. Dispatches a single positional
# subcommand to the corresponding Python script under /app/.
#
# - `explore`: drives Camoufox over VNC for discovery — needs Xvfb +
#   x11vnc started below.
# - `login`:   the auth path (formerly `byo-login`) — a genuine stock
#   Firefox the operator drives by hand over VNC (the SPA login is
#   bot-walled), lifting the session cookie on close. Needs Xvfb + x11vnc.
# - `download`: headless Camoufox + the injected cookie; needs a display.
# - `load`:    pure SQLite + Python, no browser.
#
# When Xvfb IS started, it's started directly rather than via `xvfb-run`:
# the Ubuntu Noble xvfb-run script's SIGUSR1 ready-signaling hangs under
# a non-root parent. Socket-existence polling is a version-independent
# ready signal.

set -euo pipefail

VFB_DISPLAY=99

# Symlink the pre-staged camoufox cache (populated at image build time by
# `python3 -m camoufox fetch` and copied to /opt) into the runtime user's
# $HOME/.cache/camoufox so camoufox skips its first-run download. HOME is
# /tmp inside the container (set by the wrapper for the non-root user); we
# always overwrite the symlink so a stale /tmp/.cache/camoufox from a
# previous container with a different runtime uid doesn't shadow it.
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

start_x11vnc() {
    # Single-use password. openssl rand -hex 8 is a single command, so
    # `set -euo pipefail` does not trip on SIGPIPE the way a piped
    # `tr -dc ... | head -c 16` would.
    VNC_PASSWORD=$(openssl rand -hex 8)
    x11vnc -display ":$VFB_DISPLAY" -passwd "$VNC_PASSWORD" \
        -forever -shared -rfbport 5900 -bg \
        -o /tmp/x11vnc.log >/dev/null 2>&1
    # The wrapper picks the host-side port (it knows which are free); we
    # publish it through VNC_HOST_PORT so the messages below print the
    # real port to tunnel through. Defaults to 5900 for direct
    # `docker run` invocations that skip the wrapper.
    local label="$1"
    local host_port="${VNC_HOST_PORT:-5900}"
    echo "$label: VNC ready on 127.0.0.1:${host_port}" >&2
    echo "$label: password (single-use):  $VNC_PASSWORD" >&2
    echo "$label: tunnel from your laptop with" >&2
    echo "$label:   ssh -L ${host_port}:127.0.0.1:${host_port} <host>" >&2
    echo "$label: then on the laptop:" >&2
    echo "$label:   open vnc://localhost:${host_port}" >&2
}

case "${1:-help}" in
    explore)
        # Discovery harness. Drives Camoufox over VNC, records every
        # click + every network fetch into /debug/<ts>/ so download.py
        # can be written from real traces.
        start_xvfb
        start_x11vnc explore
        shift
        exec python3 /app/explore.py "$@"
        ;;
    login)
        # The auth path (a BYO-session bootstrap — formerly `byo-login`).
        # AngelList's venture login is gated by an invisible Turnstile/
        # reCAPTCHA challenge that flags the Camoufox/Playwright automation
        # stack, so there is no unattended login.
        #
        # Fast path: if the saved Firefox profile still holds a non-expired
        # session cookie, lift it and exit 0 — no VNC sign-in needed. Flags:
        #   --check  probe only; report whether the saved session is valid,
        #            never open Firefox (exit 1 if not).
        #   --force  skip the check and always re-login.
        # Otherwise launch a genuine, un-instrumented stock Firefox under
        # Xvfb + VNC so the operator clears the login by hand; on a clean
        # close, lift the session from its plaintext cookies.sqlite.
        shift
        FXPROFILE="${ANGELLIST_FXPROFILE:-/secrets/angellist-fxprofile}"
        COOKIES="${ANGELLIST_COOKIES:-/secrets/angellist-cookies.json}"
        login_mode=auto
        case "${1:-}" in
            --check) login_mode=check; shift ;;
            --force) login_mode=force; shift ;;
        esac
        if [[ "$login_mode" != "force" ]]; then
            if python3 /app/extract_cookies.py \
                    --db "$FXPROFILE/cookies.sqlite" --out "$COOKIES" --require-valid; then
                echo "login: existing AngelList session still valid — cookie lifted to" >&2
                echo "login:   $COOKIES. No VNC login needed (pass --force to re-login)." >&2
                exit 0
            fi
            if [[ "$login_mode" == "check" ]]; then
                echo "login --check: no valid saved session — run \`login\` to refresh." >&2
                exit 1
            fi
            echo "login: no valid saved session — opening Firefox to log in…" >&2
        fi
        start_xvfb
        start_x11vnc login
        mkdir -p "$FXPROFILE"
        # Seed prefs: persist session cookies on shutdown (restore-
        # session), and skip onboarding/default-browser/telemetry noise so
        # the operator lands straight on the login page.
        cat > "$FXPROFILE/user.js" <<'PREFS'
user_pref("browser.startup.page", 3);
user_pref("browser.aboutwelcome.enabled", false);
user_pref("browser.shell.checkDefaultBrowser", false);
user_pref("datareporting.policy.dataSubmissionEnabled", false);
user_pref("trailhead.firstrun.didSeeAboutWelcome", true);
user_pref("security.sandbox.content.level", 0);
// Save downloads (K-1 CSV/PDF, financial statements) straight to the
// mounted /data/angellist-documents (= $XDG_DATA_HOME/wealthdb/angellist/angellist-
// documents on the host) instead of the container-ephemeral ~/Downloads,
// so documents grabbed from the Taxes & Documents page persist. (The doc
// endpoints reject our cookie-injection, so a real-browser download here
// is the way to get them onto the host.)
user_pref("browser.download.folderList", 2);
user_pref("browser.download.dir", "/data/angellist-documents");
user_pref("browser.download.useDownloadDir", true);
user_pref("browser.download.manager.showWhenStarting", false);
user_pref("pdfjs.disabled", true);
user_pref("browser.helperApps.neverAsk.saveToDisk", "text/csv,application/pdf,application/octet-stream,application/vnd.ms-excel,application/zip");
PREFS
        mkdir -p /data/angellist-documents
        # Firefox's content-process sandbox needs a user namespace, which
        # Colima's default seccomp profile blocks (EPERM) — left on, page
        # rendering crashes. Disable it (env + pref above). This is an
        # internal process-isolation setting, invisible to web content, so
        # it has no bearing on the anti-bot fingerprint.
        export MOZ_DISABLE_CONTENT_SANDBOX=1
        echo "login: opening Firefox -> https://venture.angellist.com/v/login" >&2
        echo "login: log in (+2FA) in the VNC window, confirm you reach your" >&2
        echo "login: portfolio. OPTIONAL: open 'Taxes & Documents' and download" >&2
        echo "login: your K-1 CSVs/PDFs — they save to" >&2
        echo "login:   $XDG_DATA_HOME/wealthdb/angellist/angellist-documents/" >&2
        echo "login: Then CLOSE the Firefox window to lift the cookie." >&2
        firefox -profile "$FXPROFILE" -no-remote -new-instance \
            "https://venture.angellist.com/v/login" >/tmp/firefox.log 2>&1 || true
        echo "login: Firefox closed — extracting AngelList cookies…" >&2
        exec python3 /app/extract_cookies.py \
            --db "$FXPROFILE/cookies.sqlite" --out "$COOKIES"
        ;;
    download)
        # Per-surface browse + capture loop. Injects the session cookie
        # `login` lifted (/secrets/angellist-cookies.json).
        shift
        exec python3 /app/download.py "$@"
        ;;
    load)
        # SQLite silver loader. Ingests bronze `captures.jsonl` + the
        # downloaded K-1 CSVs / tax docs into the source-shaped silver
        # tables. Pure Python, no browser, no display.
        shift
        exec python3 /app/load.py "$@"
        ;;
    sh|bash)
        shift
        exec /bin/bash "$@"
        ;;
    help|--help|-h)
        cat <<'EOF'
angellist container

Usage:
  <wrapper> <subcommand> [args...]

Subcommands:
  login       The auth path (formerly byo-login). Fast path: if the saved
              profile still holds a valid session cookie, it's lifted to
              /secrets/angellist-cookies.json and login exits — no VNC.
              Otherwise launch a genuine, stock Mozilla Firefox under VNC so
              you clear AngelList's invisible anti-bot login by hand (+2FA);
              on a clean Firefox close, the session cookie is lifted. There is
              no unattended login (the SPA is bot-walled). Optionally grab
              K-1 / financial docs while logged in (they save to the mounted
              documents dir). Flags: --check (probe only, never opens Firefox,
              exit 1 if stale), --force (always re-login).
  download    Headless Camoufox + injected cookie: navigate the LP-portfolio
              routes and capture the venture GraphQL (positions / summary /
              commitments / funding-account cash ledger) the SPA signs
              itself, and download tax documents (K-1 CSV/PDF, financial
              statements), re-fetching incomplete tax years. Pass --dry-run
              to walk without writing bronze.
  load        Ingest bronze + tax docs into the SQLite silver: offerings +
              event-sourced position_snapshots, vehicles, k1_capital_accounts,
              tax_documents, portfolio_summary / timeseries, commitments,
              funding_accounts + funding_transactions. Pass --force to
              re-load snapshots already in dump_runs.
  explore     Discovery harness: Camoufox under VNC with HAR + Playwright
              trace + click log under /debug (route mapping; --cookies loads
              the BYO session). Starts x11vnc on a free 127.0.0.1:5900-6000
              port (printed at handoff).
  sh|bash     Open an interactive shell inside the container.
  help        Show this message.

Run "<wrapper> <subcommand> --help" for subcommand-specific flags.
EOF
        ;;
    *)
        # Pass-through for ad-hoc commands inside the container.
        exec "$@"
        ;;
esac
