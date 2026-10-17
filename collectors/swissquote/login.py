#!/usr/bin/env python3
"""
Swissquote e-banking session minter.

Drives headless Chromium through the Swissquote login form and the
Mobile Level 3 MFA gate, then persists the resulting Playwright
storageState.json. Subsequent download.py runs reuse that file until
Swissquote invalidates the session.

The `--check` mode validates an existing state file against a live
landmark URL without re-logging in (no MFA push). See AGENTS.md §2:
non-check invocations must be explicitly authorised.

Usage:
    login.py [--state-path <file>] [--username <name>] [--env-file <file>]
             [--check] [--mfa-timeout <sec>] [--screenshot-dir <dir>]
             [--trace] [-v]
"""

from __future__ import annotations

import argparse
import getpass
import logging
import os
import sys
import time
from pathlib import Path

import landmarks as sq  # local module: DOM landmarks + URL constants

from collectorkit import bronze, cli, debugcap, envfile, launch, session

log = logging.getLogger("swissquote.login")

# Real Chrome UA, not HeadlessChrome. Banks commonly sniff
# `HeadlessChrome` and either block or add extra anti-bot steps; this
# string matches the Chrome major version that Playwright 1.62 ships,
# so it's plausible without being deceptive about capabilities. The
# only signal stripped is the "Headless" qualifier. download.py uses
# the same UA: the anti-bot heuristics may key on it across requests
# within a session, so it is held constant.
USER_AGENT = (
    "Mozilla/5.0 (X11; Linux x86_64) "
    "AppleWebKit/537.36 (KHTML, like Gecko) "
    "Chrome/151.0.0.0 Safari/537.36"
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
        help=("Bash-sourced env file with SWISSQUOTE_USERNAME / "
              "SWISSQUOTE_PASSWORD, read before the credentials are resolved "
              "(default: /secrets/swissquote.env, else "
              "~/.secrets/swissquote.env)."),
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
             "commit these — see AGENTS.md §4.",
    )
    p.add_argument(
        "--trace", action="store_true",
        help="Capture a Playwright trace bundle. Requires --screenshot-dir; "
             "the bundle is written there alongside screenshots. Never "
             "auto-writes to the secrets dir.",
    )
    cli.add_standard_args(p, verb="login")
    return p.parse_args(argv)


def resolve_password() -> str:
    """Read password from env, else prompt interactively (echo off).

    Password is never accepted as a CLI flag — AGENTS.md §3.
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


def screenshot(page, screenshot_dir: Path | None, name: str) -> None:
    """Full-page screenshot into `screenshot_dir`; a no-op when unset."""
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
        # Scrubbed: a dump taken on the sign-in page would otherwise
        # persist the typed password as a `value` attribute.
        path.write_text(debugcap.scrub_dom(page.content()), encoding="utf-8")
        log.info("Page HTML: %s", path)
    except Exception as e:  # noqa: BLE001 - best-effort debug aid
        log.warning("HTML dump %s failed: %s", path, e)


def report_mfa_api_calls(seen: dict, screenshot_dir: Path | None) -> None:
    """Log what the MFA page called, longest call first, and save it as
    ``mfa_api_calls.txt`` in `screenshot_dir`.

    The page's approval channel is its own status endpoint, whose route
    can move. The list is what re-derives it (see
    `debugcap.report_api_calls`).
    """
    debugcap.report_api_calls(seen, screenshot_dir, log=log,
                              title="MFA page", filename="mfa_api_calls.txt")


# How often the trigger URL is re-navigated while waiting for approval. This
# is the only detector that does not depend on the MFA page navigating itself,
# and it re-fires the push when approval has not landed yet — so the interval
# must comfortably exceed the time to verify the code and approve, or a run
# costs the phone more than one live request.
MFA_APPROVAL_PROBE_SECONDS = 60


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


def new_context(p, *, storage_state: Path | None,
                accept_downloads: bool = False):
    """Construct a Chromium browser + context with the standard knobs,
    shared with download.py.

    --no-sandbox is required because the container runs as a non-root
    UID without the user-namespace privileges Chromium normally uses
    for its sandbox. Acceptable in this single-trusted-origin context
    — see AGENTS.md and Dockerfile commentary.
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
    if accept_downloads:
        ctx_kwargs["accept_downloads"] = True
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
    """Start a sign-in trace: screenshots and sources, no DOM snapshots.

    A trace's DOM snapshots record every input's value, a hand-typed
    password included, and nothing can redact a trace after the fact. The
    per-action screenshots still show the flow — the browser draws a
    password field as dots — and the network records are the redacted ones.
    """
    if enabled:
        context.tracing.start(screenshots=True, snapshots=False, sources=True)


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

    ts = bronze.ts_slug()
    with sync_playwright() as p:
        browser, context = new_context(p, storage_state=state_path)
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
            screenshot(page, args.screenshot_dir, f"check_{ts}_after_goto")

            # Authentication signal: asking for the protected eBanking
            # root settles on a post-auth SPA — either that root or the
            # Trading Platform F5 may redirect on to — rather than the
            # auth form. `is_post_auth_url`
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
            debugcap.stop_trace_if_active(context, args.trace,
                                          args.screenshot_dir, "check",
                                          log=log)
            context.close()
            browser.close()


def login(args: argparse.Namespace) -> int:
    """Mint a fresh storageState.json by driving the live login + MFA."""
    from playwright.sync_api import sync_playwright

    username = envfile.resolve_credential(
        args.username, "SWISSQUOTE_USERNAME", "--username")
    password = resolve_password()
    ts = bronze.ts_slug()

    args.state_path.parent.mkdir(parents=True, exist_ok=True)

    with sync_playwright() as p:
        browser, context = new_context(p, storage_state=None)
        _maybe_start_trace(context, args.trace)
        page = context.new_page()
        try:
            log.info("Hitting login-trigger URL %s", sq.LOGIN_TRIGGER_URL)
            # F5 BIG-IP intercepts and redirects to /my.policy with
            # the login form. The redirect chain is followed until it
            # settles.
            page.goto(sq.LOGIN_TRIGGER_URL, wait_until="domcontentloaded")
            log.info("Landed at %s", page.url)
            screenshot(page, args.screenshot_dir, f"login_{ts}_01_form")
            if sq.F5_AUTH_PATH not in page.url:
                # Surprise: /my.policy was expected. Either the cookie
                # is still valid (rare on a fresh state-less context)
                # or Swissquote changed their auth gateway.
                raise SystemExit(
                    f"Expected F5 to serve the login form at {sq.F5_AUTH_PATH}, "
                    f"but the page settled at {page.url}. Aborting before "
                    f"submitting credentials."
                )

            # Log every main-frame navigation while the auth flow is
            # awaited. F5's redirect chain is opaque; without this
            # nothing tells why a wait-for-URL predicate didn't
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
            # URL — THAT positive landmark is the awaited
            # "logged in" signal. Branch (c) stays at /my.policy
            # forever, and the wait times out cleanly.
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
                screenshot(page, args.screenshot_dir, f"login_{ts}_03_mfa")
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
            # DETECTOR — watch the URL. When the phone approves, the MFA
            # SPA short-polls its own status endpoint
            # (api/thirdlevel/smartL3/check-challenge/<urlId>, seen ~4s
            # apart at ~80ms each) and navigates itself to the post-auth
            # SPA, which the `is_post_auth_url` check below catches within
            # a tick.
            #
            # BACKSTOP — the page does not always self-navigate (it stops
            # polling when it detects automation), so the trigger URL is
            # re-navigated every MFA_APPROVAL_PROBE_SECONDS. If approval has
            # landed that routes straight through; if it has not, F5
            # re-issues the push.
            #
            # No endpoint holds the request open until approval, so there is
            # no long poll to ride; instant detection would mean reading the
            # short poll's response body — see landmarks.py.
            deadline = time.monotonic() + args.mfa_timeout
            last_logged_url = None
            last_heartbeat = 0.0
            last_repoke = time.monotonic()
            api_calls: dict = {}
            last_api_sample = 0.0
            while time.monotonic() < deadline:
                url = page.url
                if time.monotonic() - last_api_sample > 1:
                    # Sampled while the MFA document is still open: the
                    # timing buffer is cleared by the next navigation.
                    debugcap.merge_api_sample(api_calls,
                                              debugcap.sample_api_calls(page))
                    last_api_sample = time.monotonic()
                if url != last_logged_url:
                    log.info("waiting; current URL: %s", url)
                    last_logged_url = url
                elif time.monotonic() - last_heartbeat > 15:
                    log.info("still waiting at %s", url)
                    last_heartbeat = time.monotonic()
                if sq.is_post_auth_url(url):
                    break
                if sq.is_profile_validation_url(url):
                    screenshot(
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

                # Backstop: re-navigate to the trigger URL on a fixed
                # interval, so approval is never missed when the MFA page
                # does not navigate itself. If approval has landed this
                # routes to the post-auth URL; otherwise F5 re-issues the
                # push, so the fresh code is re-printed to keep the terminal
                # in sync with the phone.
                if time.monotonic() - last_repoke > MFA_APPROVAL_PROBE_SECONDS:
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
                screenshot(
                    page, args.screenshot_dir, f"login_{ts}_04_mfa_timeout",
                )
                report_mfa_api_calls(api_calls, args.screenshot_dir)
                raise SystemExit(
                    f"Login did not complete within {args.mfa_timeout}s. "
                    f"Final URL: {page.url}. The push may not have been "
                    f"approved, the credentials may be wrong, or F5 may "
                    f"have shown an unfamiliar interstitial. Re-run with "
                    f"--screenshot-dir -v to diagnose."
                )
            report_mfa_api_calls(api_calls, args.screenshot_dir)
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
            screenshot(page, args.screenshot_dir, f"login_{ts}_05_landed")

            log.info("Persisting session state to %s", args.state_path)
            context.storage_state(path=str(args.state_path))
            session.secure_file(args.state_path)
            print(f"session minted: {args.state_path}", flush=True)
            return 0
        finally:
            debugcap.stop_trace_if_active(context, args.trace,
                                          args.screenshot_dir, "login",
                                          log=log)
            context.close()
            browser.close()


def main(argv: list[str] | None = None) -> int:
    args = parse_args(sys.argv[1:] if argv is None else argv)
    cli.configure_logging(args.verbose)
    # Source the credentials env file before resolving SWISSQUOTE_USERNAME /
    # _PASSWORD, so a plain env file works without a manual `source` and the
    # login isn't forced to getpass-prompt. --env-file wins, else the first
    # existing default candidate.
    env_path = envfile.resolve_env_file(args.env_file, DEFAULT_ENV_FILE_CANDIDATES)
    if env_path is not None:
        try:
            if envfile.source_env_file(env_path, prefer_file=True):
                log.info("env sourced from %s", env_path)
        except ValueError as e:
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
