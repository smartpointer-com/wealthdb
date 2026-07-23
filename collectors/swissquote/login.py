#!/usr/bin/env python3
"""
Swissquote e-banking session minter.

Drives headless Chromium through the Swissquote login form and the
Mobile Level 3 MFA gate, then persists the resulting Playwright
storageState.json. Subsequent download.py runs reuse that file until
Swissquote invalidates the session.

The `--check` mode validates an existing state file against a live
landmark URL without re-logging in (no MFA push). See CLAUDE.md §2:
non-check invocations must be explicitly authorised.

Usage:
    login.py [--state-path <file>] [--username <name>] [--check]
             [--mfa-timeout <sec>] [--screenshot-dir <dir>] [--trace]
"""

from __future__ import annotations

import argparse
import getpass
import logging
import os
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

import landmarks as sq  # local module: DOM landmarks + URL constants

import subprocess

from collectorkit import cli, envfile, launch, session

log = logging.getLogger("swissquote.login")

# Real Chrome UA, not HeadlessChrome. Banks commonly sniff
# `HeadlessChrome` and either block or add extra anti-bot steps; this
# string matches the Chrome major version that Playwright 1.59 ships,
# so it's plausible without being deceptive about capabilities. The
# only signal we strip is the "Headless" qualifier.
USER_AGENT = (
    "Mozilla/5.0 (X11; Linux x86_64) "
    "AppleWebKit/537.36 (KHTML, like Gecko) "
    "Chrome/147.0.0.0 Safari/537.36"
)

# Playwright timeouts (milliseconds). Generous defaults — the WAN
# latency to Swissquote from arbitrary cloud regions can be high.
NAV_TIMEOUT_MS = 60_000
LANDMARK_TIMEOUT_MS = 30_000

# Canonical storageState location: the wrapper mounts the secrets
# dir (default ~/.secrets, overridable via SWISSQUOTE_SECRETS_DIR /
# WEALTHDB_SECRETS_DIR) at /secrets, so the session state lives
# there by default and survives across container runs.
DEFAULT_STATE_PATH = Path("/secrets/swissquote-state.json")
# Fallback state location (underscore) — read when the canonical file is absent,
# so a session stored under this name keeps working. login writes the canonical name.
LEGACY_STATE_PATH = Path("/secrets/swissquote_state.json")

# Default credentials env-file locations (the wrapper mounts ~/.secrets at
# /secrets). --env-file wins; else the first existing candidate.
DEFAULT_ENV_FILE_CANDIDATES = (
    Path("/secrets/swissquote.env"),
    Path.home() / ".secrets" / "swissquote.env",
)


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__.strip())
    p.add_argument(
        "--state-path", type=Path, default=DEFAULT_STATE_PATH,
        help=("Path to read/write the Playwright storageState.json file "
              "(default: %(default)s, in the wrapper's /secrets mount)."),
    )
    p.add_argument(
        "--username", default=None,
        help="Swissquote login username. Falls back to SWISSQUOTE_USERNAME env.",
    )
    p.add_argument(
        "--env-file", default=None, type=Path,
        help=("KEY=VALUE env file with SWISSQUOTE_USERNAME / "
              "SWISSQUOTE_PASSWORD, sourced before resolving credentials. "
              "Defaults to /secrets/swissquote.env, else ~/.secrets/swissquote.env."),
    )
    p.add_argument(
        "--check", action="store_true",
        help="Validate the existing state file against a landmark URL. "
             "No new login, no MFA push.",
    )
    p.add_argument(
        "--mfa-timeout", type=int, default=300,
        help="Seconds to wait for approval of the Mobile Level 3 "
             "push (default: 300).",
    )
    p.add_argument(
        "--screenshot-dir", default=None, type=Path,
        help="If set, write a screenshot at each navigation landmark. "
             "Useful for debugging on a headless remote host. NEVER "
             "commit these — see CLAUDE.md §4.",
    )
    p.add_argument(
        "--trace", action="store_true",
        help="Capture a Playwright trace bundle. Requires --screenshot-dir; "
             "the bundle is written there alongside screenshots. Never "
             "auto-writes to the secrets dir.",
    )
    cli.add_standard_args(p, verb="login")
    return p.parse_args(argv)


def resolve_username(cli_value: str | None) -> str:
    """Prefer CLI, fall back to env var, raise if neither set."""
    if cli_value:
        return cli_value
    env = os.environ.get("SWISSQUOTE_USERNAME")
    if env:
        return env
    raise SystemExit(
        "Missing username: pass --username or set SWISSQUOTE_USERNAME."
    )


def resolve_password() -> str:
    """Read password from env, else prompt interactively (echo off).

    Password is never accepted as a CLI flag — CLAUDE.md §3.
    """
    env = os.environ.get("SWISSQUOTE_PASSWORD")
    if env:
        return env
    if not sys.stdin.isatty():
        raise SystemExit(
            "No SWISSQUOTE_PASSWORD env var, and stdin is not a TTY. "
            "Either set SWISSQUOTE_PASSWORD or run interactively."
        )
    return getpass.getpass("Swissquote password: ")


def _screenshot(page, screenshot_dir: Path | None, name: str) -> None:
    if not screenshot_dir:
        return
    screenshot_dir.mkdir(parents=True, exist_ok=True)
    path = screenshot_dir / f"{name}.png"
    try:
        page.screenshot(path=str(path), full_page=True)
        log.info("Screenshot: %s", path)
    except Exception as e:  # noqa: BLE001 - best-effort debug aid
        log.warning("Screenshot %s failed: %s", path, e)


def _dump_html(page, screenshot_dir: Path | None, name: str) -> None:
    """Save the page's rendered HTML next to the screenshot. Useful for
    pinning a selector against the live DOM (a screenshot can't be
    grepped). Best-effort; only when a screenshot dir is configured."""
    if not screenshot_dir:
        return
    screenshot_dir.mkdir(parents=True, exist_ok=True)
    path = screenshot_dir / f"{name}.html"
    try:
        path.write_text(page.content(), encoding="utf-8")
        log.info("Page HTML: %s", path)
    except Exception as e:  # noqa: BLE001 - best-effort debug aid
        log.warning("HTML dump %s failed: %s", path, e)


def _log_feedback_poll(
    screenshot_dir: Path | None, name: str, *,
    status: int | str, ok: bool, elapsed: float, body: str,
) -> None:
    """Append one SmartL3 feedback long-poll result to a debug file.

    Records status, how early it returned, and the response body so we can
    confirm the approved-vs-keep-alive response shape against the timing
    heuristic the wait loop uses. Best-effort; only when a debug dir is set.
    The body is auth-status only and lands in the supplied debug dir,
    never in the repo."""
    if not screenshot_dir:
        return
    try:
        screenshot_dir.mkdir(parents=True, exist_ok=True)
        path = screenshot_dir / f"{name}_smartl3_feedback.log"
        with path.open("a", encoding="utf-8") as fh:
            fh.write(
                f"+{elapsed:7.1f}s  status={status} ok={ok}  "
                f"{len(body)}B  {body[:400]!r}\n"
            )
    except Exception:  # noqa: BLE001 - logging must never break login
        pass


# Primary approval detector: long-poll the SmartL3 feedback endpoint (see
# landmarks.smartl3_listen_url). Observed behaviour (from live logins):
#   - Before approval, the server HOLDS each poll open ~to the timeout we ask
#     for, then returns a keep-alive (~18-20s for a 20s request).
#   - The instant the phone approves, the server returns immediately, and
#     every subsequent poll then also returns immediately (the approval state
#     is sticky).
#   - The response BODY is identical in both cases (just a constant redirect
#     target), so timing is the only client-side signal: a poll that returns
#     well before the hold means approval landed. We then settle the F5
#     session with one trigger nav — no push, since approval is already
#     recorded (only the challenge endpoint fires a push).
#   LISTEN_SERVER_HOLD_MS  — the `timeout` we ask the server to hold each poll.
#   LISTEN_CLIENT_TIMEOUT_MS — our ceiling on one poll; must exceed the hold so
#                              a normal keep-alive return isn't treated as error.
#   LISTEN_EARLY_RETURN_SECONDS — returns faster than this count as approval.
#       Set to half the hold: comfortably below the ~18-20s keep-alive band
#       (so a keep-alive is never misread as approval, which would fire a stray
#       push), yet detection stays snappy because an approval landing in the
#       upper half is re-caught on the very next poll via the sticky state.
LISTEN_SERVER_HOLD_MS = 20000
LISTEN_CLIENT_TIMEOUT_MS = 28000
LISTEN_EARLY_RETURN_SECONDS = LISTEN_SERVER_HOLD_MS / 1000 / 2

# Fallback detector: if the feedback poll proves unusable here (repeated
# errors, or an "early return" that turns out NOT to be an approval), we
# revert to re-navigating to the trigger URL on this interval. That detects
# approval too, but re-fires the push when approval hasn't landed yet — so the
# interval must comfortably exceed the time to verify the code and approve, so
# the common case still fires exactly one push.
MFA_APPROVAL_PROBE_SECONDS = 30


def _announce_operation_code(page, *, waiting_note: str = "") -> None:
    """Scrape the on-screen Mobile Level 3 operation code and print it, so it
    can be compared against the phone before the push is approved. Called when
    the MFA page first appears and again whenever the push is re-triggered,
    so the printed code never goes stale relative to the phone."""
    tan = None
    try:
        tan = page.locator(
            sq.MFA_OPERATION_CODE_SELECTOR
        ).inner_text(timeout=5000).strip()
    except Exception as e:  # noqa: BLE001 - non-fatal
        log.warning("Could not scrape Operation No.: %s", e)
    if tan:
        msg = (f"Operation No. on screen: {tan}\n"
               "Verify this matches your phone, then approve.")
    else:
        msg = "Approve the Mobile Level 3 push on your phone."
    if waiting_note:
        msg = f"{msg} {waiting_note}"
    print(msg, flush=True)


def _new_context(p, *, storage_state: Path | None):
    """Construct a Chromium browser + context with our standard knobs.

    --no-sandbox is required because the container runs as a non-root
    UID without the user-namespace privileges Chromium normally uses
    for its sandbox. Acceptable in this single-trusted-origin context
    — see CLAUDE.md and Dockerfile commentary.
    """
    browser = p.chromium.launch(
        headless=True,
        # --disable-blink-features=AutomationControlled stops Chromium
        # from advertising itself as automated in CDP-exposed headers
        # and DOM hooks. Swissquote's MFA-wait page is a polling SPA
        # that quietly stops polling when it detects automation; with
        # this flag (plus the navigator.webdriver override below) it
        # proceeds normally.
        args=launch.chromium_args(
            "--no-sandbox",
            "--disable-dev-shm-usage",
            "--disable-blink-features=AutomationControlled",
        ),
    )
    ctx_kwargs = {
        "user_agent": USER_AGENT,
        "locale": "en-CH",
        "timezone_id": "Europe/Zurich",
        "viewport": {"width": 1440, "height": 900},
    }
    if storage_state is not None and storage_state.is_file():
        ctx_kwargs["storage_state"] = str(storage_state)
    context = browser.new_context(**ctx_kwargs)
    # `navigator.webdriver` is set to true by Playwright/CDP and is
    # the cheapest fingerprint anti-bot code keys on. Override it to
    # undefined (matching a real Chrome session) on every new page.
    context.add_init_script(
        "Object.defineProperty(navigator, 'webdriver', "
        "{ get: () => undefined });"
    )
    context.set_default_timeout(NAV_TIMEOUT_MS)
    return browser, context


def _maybe_start_trace(context, enabled: bool):
    if enabled:
        context.tracing.start(screenshots=True, snapshots=True, sources=True)


def _maybe_stop_trace(context, enabled: bool, trace_path: Path):
    if enabled:
        trace_path.parent.mkdir(parents=True, exist_ok=True)
        context.tracing.stop(path=str(trace_path))
        log.info("Wrote Playwright trace: %s", trace_path)


def check_session(args: argparse.Namespace) -> int:
    """Return 0 if the saved session still authenticates, 1 otherwise."""
    from playwright.sync_api import sync_playwright, TimeoutError as PWTimeout

    state_path = session.resolve_state_path(
        args.state_path, DEFAULT_STATE_PATH, LEGACY_STATE_PATH)
    if not state_path.is_file():
        raise SystemExit(
            f"No state file at {state_path}. Nothing to check; "
            f"run login.py without --check to mint one."
        )

    ts = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    with sync_playwright() as p:
        browser, context = _new_context(p, storage_state=state_path)
        _maybe_start_trace(context, args.trace)
        page = context.new_page()
        try:
            log.info("Hitting %s", sq.LOGIN_TRIGGER_URL)
            page.goto(sq.LOGIN_TRIGGER_URL, wait_until="domcontentloaded")
            # Brief settling period — F5's redirect chain may continue
            # for a beat after domcontentloaded.
            try:
                page.wait_for_load_state(
                    "networkidle", timeout=LANDMARK_TIMEOUT_MS,
                )
            except PWTimeout:
                pass
            _screenshot(page, args.screenshot_dir, f"check_{ts}_after_goto")

            # Authentication signal: we land on the protected eBanking
            # SPA path (not the F5 auth form). `is_post_auth_url`
            # checks both halves — a bare "no /my.policy" test would
            # false-positive on F5's transient intermediates.
            log.info("Final URL: %s", page.url)
            if sq.is_post_auth_url(page.url):
                print("session OK")
                return 0
            else:
                print(
                    f"session expired (final URL: {page.url}). "
                    f"Re-run login.py without --check to refresh.",
                    file=sys.stderr,
                )
                return 1
        finally:
            if args.trace:
                _maybe_stop_trace(
                    context, args.trace,
                    args.screenshot_dir / f"trace_check_{ts}.zip",
                )
            context.close()
            browser.close()


def login(args: argparse.Namespace) -> int:
    """Mint a fresh storageState.json by driving the live login + MFA."""
    from playwright.sync_api import sync_playwright

    username = resolve_username(args.username)
    password = resolve_password()
    ts = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")

    args.state_path.parent.mkdir(parents=True, exist_ok=True)

    with sync_playwright() as p:
        browser, context = _new_context(p, storage_state=None)
        _maybe_start_trace(context, args.trace)
        page = context.new_page()
        try:
            log.info("Hitting login-trigger URL %s", sq.LOGIN_TRIGGER_URL)
            # F5 BIG-IP intercepts and redirects us to /my.policy with
            # the login form. We follow the redirect chain to settle.
            page.goto(sq.LOGIN_TRIGGER_URL, wait_until="domcontentloaded")
            log.info("Landed at %s", page.url)
            _screenshot(page, args.screenshot_dir, f"login_{ts}_01_form")
            if sq.F5_AUTH_PATH not in page.url:
                # Surprise: we expected /my.policy. Either the cookie
                # is still valid (rare on a fresh state-less context)
                # or Swissquote changed their auth gateway.
                raise SystemExit(
                    f"Expected F5 to serve the login form at {sq.F5_AUTH_PATH}, "
                    f"but the page settled at {page.url}. Aborting before "
                    f"submitting credentials."
                )

            # Log every main-frame navigation while we're waiting on
            # the auth flow. F5's redirect chain is opaque; without
            # this we can't tell why a wait-for-URL predicate didn't
            # fire. Logged at DEBUG (`-v`) by default.
            page.on(
                "framenavigated",
                lambda f: log.debug("nav: %s", f.url) if f == page.main_frame else None,
            )

            log.info("Filling credentials")
            page.locator(sq.LOGIN_USERNAME_INPUT).fill(username)
            page.locator(sq.LOGIN_PASSWORD_INPUT).fill(password)
            page.locator(sq.LOGIN_SUBMIT_BUTTON).click()

            # After credentials are submitted, F5 either:
            #   (a) shows the MFA wait page (happy path, push to phone)
            #   (b) skips MFA on a recently-trusted device fingerprint
            #   (c) shows an inline error (wrong password etc.)
            # Branches (a) and (b) both end at the post-auth landing
            # URL — we wait for THAT positive landmark as the
            # "logged in" signal. Branch (c) leaves us at /my.policy
            # forever, and we'll time out cleanly.
            #
            # Along the way, opportunistically scrape the TAN if the
            # MFA page renders. Brief (5s) wait — if no MFA page,
            # F5 trusted the fingerprint and skipped the push, and
            # the post-auth wait below will resolve nearly instantly.
            try:
                page.wait_for_selector(
                    f'text="{sq.MFA_PAGE_TEXT_LANDMARK}"',
                    timeout=5000,
                )
                _screenshot(page, args.screenshot_dir, f"login_{ts}_03_mfa")
                _dump_html(page, args.screenshot_dir, f"login_{ts}_03_mfa")
                _announce_operation_code(
                    page,
                    waiting_note=f"Waiting up to {args.mfa_timeout}s ...",
                )
            except Exception:
                log.info(
                    "No MFA page detected — device fingerprint likely "
                    "trusted; waiting for post-auth landing URL."
                )

            # Wait for approval, then for the post-auth landing URL.
            #
            # PRIMARY detector — long-poll the SmartL3 feedback endpoint
            # (sq.smartl3_listen_url). The server holds each poll open until
            # the phone responds, so a poll returning materially earlier than
            # the hold we asked for means approval landed: detected the instant
            # it happens, and WITHOUT re-firing the push. On such an early
            # return we navigate to the trigger URL once to settle the F5
            # session — which does not fire a push, because approval is already
            # recorded.
            #
            # SELF-CORRECTING fallback — if the poll errors repeatedly, or an
            # "early return" lands us back on the MFA page (i.e. it was a
            # keep-alive, not an approval, so the timing heuristic is unsafe
            # here), we stop trusting it and revert to re-navigating to the
            # trigger URL every MFA_APPROVAL_PROBE_SECONDS. That is slower and
            # may fire a second push, but always completes.
            deadline = time.monotonic() + args.mfa_timeout
            last_logged_url = None
            last_heartbeat = 0.0
            last_repoke = time.monotonic()
            use_feedback_poll = True
            feedback_errors = 0
            while time.monotonic() < deadline:
                url = page.url
                if url != last_logged_url:
                    log.info("waiting; current URL: %s", url)
                    last_logged_url = url
                elif time.monotonic() - last_heartbeat > 15:
                    log.info("still waiting at %s", url)
                    last_heartbeat = time.monotonic()
                if sq.is_post_auth_url(url):
                    break
                if sq.is_profile_validation_url(url):
                    _screenshot(
                        page, args.screenshot_dir,
                        f"login_{ts}_04_profile_validation",
                    )
                    raise SystemExit(
                        "Swissquote is asking you to complete a "
                        "profile-validation question (regulatory KYC "
                        "refresh — e.g. the 'executive position' "
                        "prompt). This script cannot answer that on "
                        "your behalf. Log in once via a regular "
                        "browser at https://trade.swissquote.ch/, "
                        "answer the question, then re-run login.py.\n"
                        f"Stuck at: {url}"
                    )
                if "sq-thirdlevel-plugin" not in url:
                    # A transient F5 redirect between the MFA page and the
                    # post-auth URL; keep watching.
                    time.sleep(0.25)
                    continue

                # Fast path: SmartL3 feedback long-poll. Detects approval the
                # instant it lands, without firing a push. A keep-alive return
                # or an error simply falls through to the re-navigation
                # backstop below.
                if use_feedback_poll:
                    listen_url = sq.smartl3_listen_url(
                        url, timeout_ms=LISTEN_SERVER_HOLD_MS,
                    )
                    if listen_url is None:
                        use_feedback_poll = False
                    else:
                        log.info("listening for approval (SmartL3 long-poll, "
                                 "up to %ds)", LISTEN_SERVER_HOLD_MS // 1000)
                        t0 = time.monotonic()
                        resp = None
                        try:
                            resp = page.request.get(
                                listen_url, timeout=LISTEN_CLIENT_TIMEOUT_MS,
                            )
                        except Exception as e:  # noqa: BLE001 - best-effort
                            feedback_errors += 1
                            log.warning("SmartL3 feedback poll failed (%d): %s",
                                        feedback_errors, e)
                            if feedback_errors >= 3:
                                log.warning("disabling feedback fast-path; "
                                            "re-navigation backstop only")
                                use_feedback_poll = False
                        if resp is not None:
                            elapsed = time.monotonic() - t0
                            feedback_errors = 0
                            body = ""
                            try:
                                body = resp.text()
                            except Exception:  # noqa: BLE001
                                pass
                            _log_feedback_poll(
                                args.screenshot_dir, f"login_{ts}",
                                status=resp.status, ok=resp.ok,
                                elapsed=elapsed, body=body,
                            )
                            if resp.ok and elapsed < LISTEN_EARLY_RETURN_SECONDS:
                                # Early return == phone responded. Settle the
                                # F5 session via the trigger URL (no push,
                                # since approval is already recorded).
                                log.info("approval signalled via SmartL3 "
                                         "feedback after %.1fs; finalizing",
                                         elapsed)
                                try:
                                    page.goto(sq.LOGIN_TRIGGER_URL,
                                              wait_until="domcontentloaded")
                                except Exception as e:  # noqa: BLE001
                                    log.warning("finalize navigation "
                                                "failed: %s", e)
                                if "sq-thirdlevel-plugin" in page.url:
                                    # That early return was NOT an approval, so
                                    # the timing heuristic is unreliable here
                                    # (and this nav re-fired the push). Drop the
                                    # fast-path; the backstop takes over.
                                    log.warning("SmartL3 early return was not "
                                                "an approval; disabling "
                                                "feedback fast-path")
                                    use_feedback_poll = False
                                    _announce_operation_code(page)
                                    last_repoke = time.monotonic()
                                # Re-check from the top (post-auth -> break).
                                continue
                            # else keep-alive: fall through to the backstop.

                # Backstop: re-navigate to the trigger URL on a fixed interval.
                # This runs whether or not the fast-path is active, so approval
                # is never missed even if the long-poll never observes it. If
                # approval has landed this routes to the post-auth URL;
                # otherwise F5 re-issues the push, so we re-print the fresh
                # code to keep the terminal in sync with the phone.
                if (
                    time.monotonic() - last_repoke > MFA_APPROVAL_PROBE_SECONDS
                    and "sq-thirdlevel-plugin" in url
                ):
                    log.info("probing for approval (re-navigating to the "
                             "trigger URL)")
                    try:
                        page.goto(
                            sq.LOGIN_TRIGGER_URL, wait_until="domcontentloaded",
                        )
                        if "sq-thirdlevel-plugin" in page.url:
                            _announce_operation_code(page)
                    except Exception as e:  # noqa: BLE001 - best effort
                        log.warning("re-poke navigation failed: %s", e)
                    last_repoke = time.monotonic()
                time.sleep(0.25)
            else:
                _screenshot(
                    page, args.screenshot_dir, f"login_{ts}_04_mfa_timeout",
                )
                raise SystemExit(
                    f"Login did not complete within {args.mfa_timeout}s. "
                    f"Final URL: {page.url}. The push may not have been "
                    f"approved, the credentials may be wrong, or F5 may "
                    f"have shown an unfamiliar interstitial. Re-run with "
                    f"--screenshot-dir -v to diagnose."
                )
            # After the wait fires, give the SPA a beat to settle —
            # F5 may issue follow-up redirects and additional cookies
            # before the session is fully bedded in.
            from playwright.sync_api import TimeoutError as PWTimeout
            try:
                page.wait_for_load_state(
                    "networkidle", timeout=LANDMARK_TIMEOUT_MS,
                )
            except PWTimeout:
                pass
            log.info("Settled at %s", page.url)
            _screenshot(page, args.screenshot_dir, f"login_{ts}_05_landed")

            log.info("Persisting session state to %s", args.state_path)
            context.storage_state(path=str(args.state_path))
            session.secure_file(args.state_path)
            print(f"session minted: {args.state_path}", flush=True)
            return 0
        finally:
            if args.trace:
                _maybe_stop_trace(
                    context, args.trace,
                    args.screenshot_dir / f"trace_login_{ts}.zip",
                )
            context.close()
            browser.close()


def main(argv: list[str] | None = None) -> int:
    args = parse_args(sys.argv[1:] if argv is None else argv)
    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )
    # Source the credentials env file before resolving SWISSQUOTE_USERNAME /
    # _PASSWORD, so a plain env file works without a manual `source` and the
    # login isn't forced to getpass-prompt. --env-file wins, else the first
    # existing default candidate.
    env_path = envfile.resolve_env_file(args.env_file, DEFAULT_ENV_FILE_CANDIDATES)
    if env_path is not None:
        try:
            if envfile.source_env_file(env_path, prefer_file=True):
                log.info("env sourced from %s", env_path)
        except (subprocess.CalledProcessError, ValueError) as e:
            log.error("env file %s failed to source: %s", env_path, e)

    # --trace must be paired with --screenshot-dir. The trace bundle
    # lands inside that directory; there is no implicit fallback to
    # the state-file's directory (which would pollute the secrets
    # dir with debug artefacts).
    if args.trace and not args.screenshot_dir:
        raise SystemExit(
            "--trace requires --screenshot-dir. The trace bundle is "
            "written alongside screenshots; pick a directory that is "
            "NOT your secrets dir."
        )

    if args.check:
        return check_session(args)
    return login(args)


if __name__ == "__main__":
    sys.exit(main())
