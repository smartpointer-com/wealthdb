#!/usr/bin/env python3
"""
Swissquote e-banking session minter.

Drives headless Chromium through the Swissquote login form and the
Mobile Level 3 MFA gate, then persists the resulting Playwright
storageState.json. Subsequent download.py runs reuse that file until
Swissquote invalidates the session.

The `--check` mode validates an existing state file against a live
landmark URL without re-logging in (no MFA push). See CLAUDE.md §2:
non-check invocations must be explicitly authorised by the user.

Usage:
    login.py --state-path <file> [--username <name>] [--check]
             [--mfa-timeout <sec>] [--screenshot-dir <dir>] [--trace]
"""

from __future__ import annotations

import argparse
import getpass
import logging
import os
import sys
from datetime import datetime, timezone
from pathlib import Path

import landmarks as sq  # local module: DOM landmarks + URL constants

log = logging.getLogger("swissquote-dump.login")

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

# storageState file mode. CLAUDE.md §3 — never relax below 0600.
STATE_FILE_MODE = 0o600


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__.strip())
    p.add_argument(
        "--state-path", required=True, type=Path,
        help="Path to read/write the Playwright storageState.json file.",
    )
    p.add_argument(
        "--username", default=None,
        help="Swissquote login username. Falls back to SWISSQUOTE_USERNAME env.",
    )
    p.add_argument(
        "--check", action="store_true",
        help="Validate the existing state file against a landmark URL. "
             "No new login, no MFA push.",
    )
    p.add_argument(
        "--mfa-timeout", type=int, default=300,
        help="Seconds to wait for the user to approve the Mobile Level 3 "
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
    p.add_argument(
        "-v", "--verbose", action="store_true", help="DEBUG-level logging.",
    )
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


def _new_context(p, *, storage_state: Path | None):
    """Construct a Chromium browser + context with our standard knobs.

    --no-sandbox is required because the container runs as a non-root
    UID without the user-namespace privileges Chromium normally uses
    for its sandbox. Acceptable in this single-trusted-origin context
    — see CLAUDE.md and Dockerfile commentary.
    """
    browser = p.chromium.launch(
        headless=True,
        args=["--no-sandbox", "--disable-dev-shm-usage"],
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

    if not args.state_path.is_file():
        raise SystemExit(
            f"No state file at {args.state_path}. Nothing to check; "
            f"run login.py without --check to mint one."
        )

    ts = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    with sync_playwright() as p:
        browser, context = _new_context(p, storage_state=args.state_path)
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

            # Authentication signal: we did NOT end up at the F5 auth
            # path. If the session cookie is valid, F5 serves the
            # originally-requested URL directly; if not, F5 redirects
            # to /my.policy to collect credentials.
            on_f5_auth = sq.F5_AUTH_PATH in page.url
            log.info("Final URL: %s", page.url)
            if not on_f5_auth:
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

            log.info("Filling credentials")
            page.locator(sq.LOGIN_USERNAME_INPUT).fill(username)
            page.locator(sq.LOGIN_PASSWORD_INPUT).fill(password)
            page.locator(sq.LOGIN_SUBMIT_BUTTON).click()

            # Two possible next pages: MFA (happy path) or a login
            # error (wrong password / locked account). Wait for the
            # MFA landmark; if it never appears, surface a clear error.
            log.info("Waiting for MFA prompt to appear ...")
            try:
                page.wait_for_selector(
                    f'text="{sq.MFA_PAGE_TEXT_LANDMARK}"',
                    timeout=LANDMARK_TIMEOUT_MS,
                )
            except Exception:
                _screenshot(
                    page, args.screenshot_dir, f"login_{ts}_02_no_mfa",
                )
                raise SystemExit(
                    "Login did not advance to the MFA page. Likely "
                    "causes: wrong username/password, account locked, "
                    "or an unfamiliar interstitial. Re-run with "
                    "--screenshot-dir and --trace to diagnose."
                )
            _screenshot(page, args.screenshot_dir, f"login_{ts}_03_mfa")

            # Scrape the Operation No. (TAN) the page shows next to
            # the approval prompt. The phone app shows the same code;
            # the operator visually confirms they match before tapping
            # approve. Printing it here saves an SCP/screenshot trip.
            tan = None
            try:
                tan = page.locator(
                    sq.MFA_OPERATION_CODE_SELECTOR
                ).inner_text(timeout=LANDMARK_TIMEOUT_MS).strip()
            except Exception as e:  # noqa: BLE001 - non-fatal
                log.warning("Could not scrape Operation No.: %s", e)

            if tan:
                print(
                    f"Operation No. on screen: {tan}\n"
                    f"Verify this matches your phone, then approve. "
                    f"Waiting up to {args.mfa_timeout}s ...",
                    flush=True,
                )
            else:
                print(
                    "Approve the Mobile Level 3 push on your phone. "
                    f"Waiting up to {args.mfa_timeout}s ...",
                    flush=True,
                )

            # F5 keeps the user at /my.policy during both the login
            # and MFA stages. Successful MFA approval is signalled by
            # the URL leaving /my.policy entirely (F5 then redirects
            # to the originally-requested protected URL).
            try:
                page.wait_for_url(
                    lambda url: sq.F5_AUTH_PATH not in url,
                    timeout=args.mfa_timeout * 1000,
                )
            except Exception:
                _screenshot(
                    page, args.screenshot_dir, f"login_{ts}_04_mfa_timeout",
                )
                raise SystemExit(
                    f"MFA was not approved within {args.mfa_timeout}s, "
                    "or F5 did not redirect away from /my.policy. "
                    "Check the screenshots."
                )
            _screenshot(page, args.screenshot_dir, f"login_{ts}_05_landed")

            log.info("Persisting session state to %s", args.state_path)
            context.storage_state(path=str(args.state_path))
            os.chmod(args.state_path, STATE_FILE_MODE)
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
