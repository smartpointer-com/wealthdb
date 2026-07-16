#!/bin/bash
# Container entrypoint. Dispatches a single positional
# subcommand to the corresponding Python script under /app/.
#
# - `explore`: drives Camoufox over VNC for discovery — needs Xvfb +
#   x11vnc started below.
# - `login`:   the auth path — a genuine stock Firefox driven by hand
#   over VNC (the SPA login is bot-walled), lifting the session cookie
#   on close. Needs Xvfb + x11vnc.
# - `download`: headless Camoufox + the injected cookie; needs a display.
# - `load`:    pure SQLite + Python, no browser.
#
# When Xvfb IS started, it's started directly rather than via `xvfb-run`:
# the Ubuntu Noble xvfb-run script's SIGUSR1 ready-signaling hangs under
# a non-root parent. Socket-existence polling is a version-independent
# ready signal.

set -euo pipefail

# Xvfb / x11vnc / camoufox-cache bootstrap (VFB_DISPLAY, start_xvfb,
# start_x11vnc) is shared across the camoufox collectors, baked into
# base-camoufox at /opt/entrypoint-lib.sh.
# shellcheck source=/dev/null
source /opt/entrypoint-lib.sh

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
        # The auth path (a BYO-session bootstrap).
        # AngelList's venture login is gated by an invisible Turnstile/
        # reCAPTCHA challenge that flags the Camoufox/Playwright automation
        # stack, so there is no unattended login.
        #
        # Fast path: if the saved profile holds a non-expired session cookie
        # AND a headless probe confirms the server still accepts it, lift the
        # cookie and exit 0 — no VNC sign-in needed. The probe matters: a
        # cookie can be unexpired yet server-rejected (stale), which would
        # otherwise make `download` fail after login wrongly reported success.
        # Flags:
        #   --check  probe only; report whether the session is valid, never
        #            open Firefox (exit 1 if not).
        #   --fresh  skip the check and always re-login (the `load --force`
        #            spelling is reserved for the delete-and-rebuild loader).
        # Otherwise launch a genuine, un-instrumented stock Firefox under
        # Xvfb + VNC so the login is cleared by hand; on a clean close,
        # lift the session from its plaintext cookies.sqlite.
        shift
        # Fixed in-container paths under the /secrets mount. To relocate
        # them, override the mount itself (ANGELLIST_SECRETS_DIR) — an env
        # var read only here would never reach the container.
        FXPROFILE="/secrets/angellist-fxprofile"
        COOKIES="/secrets/angellist-cookies.json"
        login_mode=auto
        case "${1:-}" in
            --check) login_mode=check; shift ;;
            --fresh) login_mode=fresh; shift ;;
            -h|--help)
                # A help request must never probe the server or open Firefox.
                echo "angellist login — BYO-cookie auth path." >&2
                echo "  --check  probe the saved session (exit 0 valid / 1 stale); never opens Firefox." >&2
                echo "  --fresh  skip the check and always re-login by hand over VNC." >&2
                echo "  (no flag) probe; if stale, open Firefox under VNC for a by-hand login." >&2
                exit 0 ;;
            -*) echo "angellist login: unknown flag '$1' (try --check / --fresh / --help)." >&2
                exit 2 ;;
        esac
        if [[ "$login_mode" != "fresh" ]]; then
            # 1. Cheap client-side filter: lift the cookie only if a non-expired
            #    session cookie is present in the saved profile.
            if python3 /app/extract_cookies.py \
                    --db "$FXPROFILE/cookies.sqlite" --out "$COOKIES" --require-valid; then
                # 2. Authoritative server probe: a cookie can be unexpired yet
                #    rejected by AngelList (stale/revoked session). Confirm it
                #    actually establishes identity (headless, no VNC) before
                #    skipping the sign-in — otherwise `download` would fail.
                if python3 /app/download.py --cookies "$COOKIES" --check; then
                    echo "login: existing AngelList session still valid — cookie lifted to" >&2
                    echo "login:   $COOKIES. No VNC login needed (pass --fresh to re-login)." >&2
                    exit 0
                fi
                echo "login: saved cookie is unexpired but the server rejected it (stale session)." >&2
            fi
            if [[ "$login_mode" == "check" ]]; then
                echo "login --check: no valid saved session — run \`login\` to refresh." >&2
                exit 1
            fi
            echo "login: no valid saved session — opening Firefox to log in…" >&2
        fi
        start_xvfb
        start_x11vnc login
        # Seed the profile's user.js (a stock Firefox takes no Playwright
        # prefs) and ensure the documents dir exists. The prefs come from
        # the shared collectorkit.launch set plus AngelList's own — see
        # fxprofile.py; nothing pref-shaped belongs in this entrypoint.
        python3 /app/fxprofile.py --profile-dir "$FXPROFILE"
        # Firefox's content-process sandbox needs a user namespace, which
        # Colima's default seccomp profile blocks (EPERM) — left on, page
        # rendering crashes. Disable it (env here + the matching pref in
        # fxprofile.py). This is an internal process-isolation setting,
        # invisible to web content, so it has no bearing on the anti-bot
        # fingerprint.
        export MOZ_DISABLE_CONTENT_SANDBOX=1
        echo "login: opening Firefox -> https://venture.angellist.com/v/login" >&2
        echo "login: log in (+2FA) in the VNC window, confirm you reach your" >&2
        echo "login: portfolio. OPTIONAL: open 'Taxes & Documents' and download" >&2
        echo "login: your K-1 CSVs/PDFs save to angellist-documents/ in your" >&2
        echo "login:   wealthdb data dir (mounted here at /data)." >&2
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
    prune)
        # Delete non-complete dumps (crashed / interrupted downloads)
        # from the bronze tree. Pure file walk, no browser, no display.
        # Also reachable host-side via the wrapper (the preferred route).
        shift
        exec python3 /app/prune.py "$@"
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
  login       The auth path. Fast path: if the saved
              profile holds a valid session — cookie unexpired AND a headless
              probe confirms the server still accepts it — it's lifted to
              /secrets/angellist-cookies.json and login exits — no VNC.
              Otherwise launch a genuine, stock Mozilla Firefox under VNC so
              you clear AngelList's invisible anti-bot login by hand (+2FA);
              on a clean Firefox close, the session cookie is lifted. There is
              no unattended login (the SPA is bot-walled). Optionally grab
              K-1 / financial docs while logged in (they save to the mounted
              documents dir). Flags: --check (probe only, never opens Firefox,
              exit 1 if stale), --fresh (always re-login).
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
  prune       Delete non-complete dumps (crashed / interrupted downloads
              that never wrote a terminal run.json) from the bronze tree.
              --dry-run prints the plan first.
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
