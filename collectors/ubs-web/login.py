#!/usr/bin/env python3
"""
UBS Switzerland e-banking session minter.

Drives headless Chromium through the UBS login dialog and the
Access App QR challenge, then persists the resulting Playwright
storageState.json. Subsequent download.py runs reuse that file
until UBS invalidates the session.

The `--check` mode validates an existing state file against a live
landmark URL without re-logging in (no QR challenge). See AGENTS.md
§2: non-check invocations must be explicitly authorised.
"""

from __future__ import annotations

import argparse
import logging
import os
import sys
import time
from pathlib import Path

import landmarks as ubs  # local module: URL + DOM landmarks
# The QR challenge's image rules; each name is bound here too.
from qr import (  # noqa: F401
    decode_data_url_png,
    render_qr_to_terminal,
    QR_PNG_SCALE,
    _upscale_qr_png,
)

from collectorkit import cli, debugcap, envfile, launch, session

log = logging.getLogger("ubs-web.login")

# Real desktop Chrome UA, not HeadlessChrome. Banks commonly sniff
# `HeadlessChrome` and either block or add anti-bot steps; this
# string matches the Chrome major version Playwright 1.65 ships, so
# it's plausible without being deceptive about capabilities. The
# only signal stripped is the "Headless" qualifier.
#
# The major version matters: UBS server-side redirects UAs below a
# minimum Chrome version to a "browser outdated" page, so this string
# must move forward in lockstep with the Playwright base image, which
# `make update` does.
USER_AGENT = (
    "Mozilla/5.0 (X11; Linux x86_64) "
    "AppleWebKit/537.36 (KHTML, like Gecko) "
    "Chrome/159.0.0.0 Safari/537.36"
)

# Playwright timeouts (milliseconds). Generous defaults — WAN
# latency from arbitrary cloud regions to UBS can be high.
NAV_TIMEOUT_MS = 60_000
LANDMARK_TIMEOUT_MS = 30_000

# Default env-file locations (the wrapper mounts ~/.secrets at /secrets).
# ubs-web owns `<source>.env` (ubs-web.env) for the contract number; the
# bank-level `ubs.env` is a fallback, shared with any ubs-* sibling. First
# existing wins. Host paths let the script run outside the container for
# local dev.
DEFAULT_ENV_FILE_CANDIDATES = (
    Path("/secrets/ubs-web.env"),
    Path.home() / ".secrets" / "ubs-web.env",
    Path("/secrets/ubs.env"),
    Path.home() / ".secrets" / "ubs.env",
)

# Env var the contract number is read from. The env file (see
# DEFAULT_ENV_FILE_CANDIDATES) holds a line of the form
# `UBS_CONTRACT_NUMBER=<digits>`.
CONTRACT_NUMBER_ENV = "UBS_CONTRACT_NUMBER"

# Canonical storageState location: the wrapper mounts the secrets
# dir (default ~/.secrets, overridable via UBS_WEB_SECRETS_DIR /
# WEALTHDB_SECRETS_DIR) at /secrets, so the session state lives
# there by default and survives across container runs.
DEFAULT_STATE_PATH = Path("/secrets/ubs-web-state.json")
# Fallback state location (underscore) — read when the canonical file is absent,
# so a session stored under this name keeps working. New logins write
# DEFAULT_STATE_PATH, migrating the session to the hyphenated name.
LEGACY_STATE_PATH = Path("/secrets/ubs_web_state.json")


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument(
        "--state-path", type=Path, default=DEFAULT_STATE_PATH,
        help=("Path to read/write the Playwright storageState.json file "
              "(default: %(default)s, in the wrapper's /secrets mount)."),
    )
    p.add_argument(
        "--contract-number", default=None,
        help=("UBS contract number (8-digit customer ID). Falls back to "
              f"${CONTRACT_NUMBER_ENV} env var, which is typically "
              "sourced from ~/.secrets/ubs-web.env — see --env-file."),
    )
    p.add_argument(
        "--env-file", default=None, type=Path,
        help=("Path to a bash env file to source before resolving "
              "the contract number. Defaults to the first that exists "
              "of ubs-web.env, then the bank-level ubs.env, under "
              "/secrets or ~/.secrets."),
    )
    p.add_argument(
        "--check", action="store_true",
        help=("Validate the existing state file against a landmark URL. "
              "No new login, no QR challenge."),
    )
    p.add_argument(
        "--mfa-timeout", type=int, default=300,
        help="Seconds to wait for Access-App approval (default: 300).",
    )
    p.add_argument(
        "--qr-png", default=None, type=Path,
        help=("Write the QR PNG to this path (useful for sending the "
              "QR to your phone via scp/AirDrop if terminal-rendered "
              "QR is unreadable). If unset, no PNG is written."),
    )
    p.add_argument(
        "--no-terminal-qr", action="store_true",
        help="Skip printing the QR to the terminal. Requires --qr-png.",
    )
    p.add_argument(
        "--screenshot-dir", default=None, type=Path,
        help=("If set, write a screenshot at each navigation landmark. "
              "Useful for debugging on a headless remote host. NEVER "
              "commit these — see the root AGENTS.md §4."),
    )
    p.add_argument(
        "--trace", action="store_true",
        help=("Capture a Playwright trace bundle. Requires "
              "--screenshot-dir; the bundle lands there alongside "
              "screenshots. Never auto-writes to the secrets dir."),
    )
    cli.add_standard_args(p, verb="login")
    return p.parse_args(argv)


# ============================================================
# Contract number
# ============================================================

def resolve_contract_number(args: argparse.Namespace) -> str:
    """CLI flag, else env var, else exit; honours --env-file.

    AGENTS.md §3: the contract number is passed by value or read from
    the environment, never through a password-style flag.
    """
    # The first existing candidate wins (ubs-web.env over the bank-level
    # ubs.env); only one is sourced, so ubs.env never overrides it.
    envfile.load(args.env_file, candidates=DEFAULT_ENV_FILE_CANDIDATES,
                 logger=log)

    if args.contract_number:
        return args.contract_number.strip()
    env = os.environ.get(CONTRACT_NUMBER_ENV)
    if env:
        return env.strip()
    raise SystemExit(
        f"Missing contract number: pass --contract-number, set "
        f"${CONTRACT_NUMBER_ENV}, or put it in ~/.secrets/ubs-web.env."
    )


# ============================================================
# Screenshot / trace helpers
# ============================================================

def maybe_screenshot(page, screenshot_dir: Path | None, label: str) -> None:
    """Capture the scrubbed HTML and a viewport PNG at a landmark into
    the `--screenshot-dir` host path (None captures nothing), the
    contract number and the session masked out of the markup. Never
    raises."""
    debugcap.landmark_capturer(screenshot_dir, user_env=CONTRACT_NUMBER_ENV,
                               session=True, log=log)(page, label)


def _start_trace(context) -> None:
    """Start a Playwright trace without DOM snapshots.

    A trace's DOM snapshots record every input's value — here the
    contract number typed into the login form — and nothing can redact a
    trace after the fact. The per-action screenshots still show the
    flow, and the network records are the redacted ones.
    """
    context.tracing.start(screenshots=True, snapshots=False, sources=True)


# ============================================================
# Playwright flows
# ============================================================

def run_check(state_path: Path, screenshot_dir: Path | None,
              trace: bool) -> int:
    """Validate that the persisted state still authenticates.

    Hits a single protected URL with the stored cookies; if UBS
    redirects the page into the workbench (`?navitemid=...`), the
    session is alive. If it serves the contract-entry dialog or
    a Nevis logout interstitial, the session has expired.
    """
    from playwright.sync_api import sync_playwright, TimeoutError as PWTimeout

    if not state_path.exists():
        log.error("state file does not exist: %s", state_path)
        return 1

    log.info("validating session at %s", ubs.LOGIN_ENTRY_URL)
    with sync_playwright() as pw:
        browser = pw.chromium.launch(
            headless=True,
            args=launch.chromium_args("--no-sandbox", "--disable-dev-shm-usage"),
        )
        context = browser.new_context(
            storage_state=str(state_path), user_agent=USER_AGENT,
        )
        if trace:
            _start_trace(context)
        page = context.new_page()
        page.set_default_navigation_timeout(NAV_TIMEOUT_MS)
        try:
            page.goto(ubs.LOGIN_ENTRY_URL, wait_until="domcontentloaded")
            # Give the SPA a moment to either route the page into the
            # workbench (cookie valid) or render the login dialog
            # (cookie dead). Wait on either side's anchor selector
            # rather than networkidle.
            try:
                page.wait_for_function(
                    "() => /\\/app\\/.*\\/ebanking\\/spa\\.html/.test(location.href) || "
                    "      /[?&]navitemid=/.test(location.href) || "
                    "      !!document.querySelector('input[name=\"loginalias\"]')",
                    timeout=LANDMARK_TIMEOUT_MS,
                )
            except PWTimeout:
                pass
            url = page.url
            maybe_screenshot(page, screenshot_dir, "check-final")
            if ubs.is_post_auth_url(url):
                log.info("session OK: %s", url)
                rc = 0
            else:
                log.error("session DEAD: landed at %s — run login.py (no --check)", url)
                rc = 2
        except PWTimeout as e:
            maybe_screenshot(page, screenshot_dir, "check-timeout")
            log.error("timeout during --check: %s", e)
            rc = 3
        finally:
            debugcap.stop_trace_if_active(context, trace, screenshot_dir,
                                          "check", log=log)
            context.close()
            browser.close()
        return rc


def run_login(contract_number: str, state_path: Path, mfa_timeout: int,
              qr_png: Path | None, terminal_qr: bool,
              screenshot_dir: Path | None, trace: bool) -> int:
    """Mint a fresh session via contract + Access App QR challenge."""
    from playwright.sync_api import sync_playwright, TimeoutError as PWTimeout

    log.info("opening UBS login")
    with sync_playwright() as pw:
        browser = pw.chromium.launch(
            headless=True,
            args=launch.chromium_args("--no-sandbox", "--disable-dev-shm-usage"),
        )
        context = browser.new_context(user_agent=USER_AGENT)
        if trace:
            _start_trace(context)
        page = context.new_page()
        page.set_default_navigation_timeout(NAV_TIMEOUT_MS)

        try:
            # ---------- Stage 1: contract-number entry ----------
            page.goto(ubs.LOGIN_ENTRY_URL, wait_until="domcontentloaded")

            # Wait on the contract input rather than networkidle —
            # UBS's SPA keeps long-lived analytics + fingerprinting
            # requests open and `networkidle` rarely fires within
            # 30s. The input being visible is a stronger signal
            # that Stage 1 finished rendering.
            contract_input = page.locator(f'input[name="{ubs.CONTRACT_INPUT_NAME}"]').first
            contract_input.wait_for(state="visible", timeout=LANDMARK_TIMEOUT_MS)

            template = _read_initial_props_template(page)
            log.debug("stage-1 template: %s", template)
            if template != ubs.TEMPLATE_CONTRACT_NR:
                maybe_screenshot(page, screenshot_dir, "stage1-unexpected")
                _fail_on_unexpected_template(template)
            contract_input.fill(contract_number)
            maybe_screenshot(page, screenshot_dir, "stage1-filled")

            # Submit the form. The contract-entry card has exactly
            # one primary button; click whichever is visible. Falling
            # back to Enter is unreliable: in some flows it submits
            # cleanly, in others it leaves the page on a "Login starten"
            # interstitial (see Stage 1b below).
            _click_primary_button(page, contract_input)

            # ---------- Stage 1b: optional "Login starten" interstitial ----------
            # UBS may render a confirmation card between contract
            # entry and the QR challenge. It is resolved by waiting for
            # whichever shows up first: the QR image (no interstitial)
            # or the QR card title (interstitial present). On the
            # interstitial, click its primary button and loop back to
            # the same wait.
            qr_locator = page.locator(f'[data-testid="{ubs.QR_IMG_TESTID}"]')
            _advance_through_access_app_interstitial(page, qr_locator)

            # ---------- Stage 2: QR challenge ----------
            # Wait for the QR <img> to be rendered with a real
            # base64 data URL (not the empty placeholder).
            qr_locator.wait_for(state="visible", timeout=LANDMARK_TIMEOUT_MS)
            template = _read_initial_props_template(page)
            log.debug("stage-2 template: %s", template)
            if template != ubs.TEMPLATE_QR:
                maybe_screenshot(page, screenshot_dir, "stage2-unexpected")
                _fail_on_unexpected_template(template)

            # Wait until the SPA has populated the data URL. The
            # initial render has src="" or a placeholder; the
            # first /check_status poll fills it.
            _wait_for_qr_data_url(page, qr_locator)
            maybe_screenshot(page, screenshot_dir, "stage2-qr-shown")

            print(file=sys.stdout)
            print(f"UBS Access App login challenge — contract {contract_number[:2]}…",
                  file=sys.stdout)
            print("Open UBS Access App on your phone and scan the QR below.",
                  file=sys.stdout)
            print(f"Waiting up to {mfa_timeout}s for approval.",
                  file=sys.stdout)
            sys.stdout.flush()

            last_data_url = _render_qr_from_locator(
                qr_locator, qr_png, terminal_qr,
            )

            # ---------- Stage 3: wait for approval ----------
            deadline = time.monotonic() + mfa_timeout
            last_seen_url = ""
            while time.monotonic() < deadline:
                url = page.url
                if url != last_seen_url:
                    log.debug("page url: %s", url)
                    last_seen_url = url
                if ubs.is_post_auth_url(url):
                    log.info("login completed: %s", url)
                    break
                # Watch for QR rotation. The SPA replaces the img
                # src in place on REFRESH_CONTINUE.
                try:
                    current = qr_locator.get_attribute("src", timeout=1000) or ""
                except PWTimeout:
                    current = ""
                if current and current != last_data_url and current.startswith("data:image/"):
                    log.info("QR rotated; re-rendering")
                    last_data_url = _render_qr_from_locator(
                        qr_locator, qr_png, terminal_qr,
                    )
                # Short sleep — match the SPA's 2s cadence.
                page.wait_for_timeout(ubs.QR_POLL_INTERVAL_SECONDS * 1000)
            else:
                maybe_screenshot(page, screenshot_dir, "stage3-timeout")
                log.error("MFA timeout after %ds without approval", mfa_timeout)
                return 4

            # ---------- Stage 4: confirm + persist ----------
            # Don't wait_for networkidle here either — the workbench
            # loads many analytics requests and they're noise.
            if not ubs.is_post_auth_url(page.url):
                maybe_screenshot(page, screenshot_dir, "stage4-bad-landing")
                log.error("expected post-auth URL, got %s", page.url)
                return 5

            maybe_screenshot(page, screenshot_dir, "stage4-workbench")
            state_path.parent.mkdir(parents=True, exist_ok=True)
            context.storage_state(path=str(state_path))
            session.secure_file(state_path)
            log.info("session state written to %s (chmod 0600)", state_path)
            return 0

        except PWTimeout as e:
            maybe_screenshot(page, screenshot_dir, "timeout")
            log.error("playwright timeout: %s", e)
            return 6
        finally:
            debugcap.stop_trace_if_active(context, trace, screenshot_dir,
                                          "login", log=log)
            context.close()
            browser.close()


def _click_primary_button(page, focus_fallback) -> None:
    """Click the visible primary submit button on a UBS auth card.

    UBS labels are locale-dependent ("Weiter" / "Continue" /
    "Login starten" / ...). The button is located as a `<button
    type="submit">` inside the auth form; if there are multiple, the
    first visible one is picked. If none is found, fall back to
    pressing Enter on `focus_fallback`.
    """
    candidates = page.locator('form button[type="submit"]:visible')
    n = candidates.count()
    if n >= 1:
        candidates.first.click()
        return
    # Plain submit-typed inputs are not used in this UI but handle
    # them defensively.
    inputs = page.locator('form input[type="submit"]:visible')
    if inputs.count() >= 1:
        inputs.first.click()
        return
    focus_fallback.press("Enter")


def _advance_through_access_app_interstitial(page, qr_locator,
                                             max_clicks: int = 2) -> None:
    """If a confirm-Access-App page sits between Stage 1 and the QR,
    click its primary button. Idempotent — if the QR is already
    visible, returns immediately.
    """
    for _ in range(max_clicks):
        # Wait for the page to settle on one of the two known
        # screens: the QR (success) or the confirm interstitial.
        try:
            page.wait_for_function(
                "(qr) => !!document.querySelector(qr) || "
                "      !!Array.from(document.querySelectorAll("
                "         'form button[type=\"submit\"]'))"
                "         .find(b => b.offsetParent !== null)",
                arg=f'[data-testid="{ubs.QR_IMG_TESTID}"]',
                timeout=LANDMARK_TIMEOUT_MS,
            )
        except Exception:  # noqa: BLE001 — re-raise via the wait below
            return
        if qr_locator.count() > 0:
            return
        # Still on a card with a submit button — assume confirm
        # interstitial and click through.
        log.debug("advancing through Access-App confirm interstitial")
        _click_primary_button(page, qr_locator)


def _read_initial_props_template(page) -> str | None:
    """Return `window.initialProps.template` or None if unavailable.

    The Nevis-rendered SPA bootstraps a `var initialProps = {...}`
    in inline <script>. It is read back from the page context.
    """
    try:
        return page.evaluate(
            "() => (window.initialProps && window.initialProps.template) || null"
        )
    except Exception as e:  # noqa: BLE001 — log + return None
        log.debug("could not read initialProps.template: %s", e)
        return None


def _fail_on_unexpected_template(template: str | None) -> None:
    if template in ubs.UNEXPECTED_TEMPLATES:
        raise SystemExit(
            f"UBS routed us to {template!r}, which this script does "
            f"not handle. Complete the step once in a regular browser "
            f"and re-run login.py."
        )
    raise SystemExit(
        f"Unexpected UBS template {template!r} (expected one of "
        f"{ubs.TEMPLATE_CONTRACT_NR}, {ubs.TEMPLATE_QR}). UBS may "
        f"have changed the login flow; capture --screenshot-dir + "
        f"--trace and report."
    )


def _wait_for_qr_data_url(page, locator, timeout_s: int = 30) -> None:
    """Poll the img element until its `src` is a real data URL."""
    deadline = time.monotonic() + timeout_s
    while time.monotonic() < deadline:
        src = locator.get_attribute("src") or ""
        if src.startswith("data:image/") and "base64," in src:
            return
        page.wait_for_timeout(500)
    raise SystemExit(
        "Timed out waiting for the QR image to render. The SPA "
        "may have failed to fetch /check_status; try --screenshot-dir."
    )


def _render_qr_from_locator(locator, qr_png: Path | None,
                            terminal_qr: bool) -> str:
    """Read the img data URL, decode, render to terminal + write PNG.

    Returns the data URL string so the caller can spot rotations.
    """
    data_url = locator.get_attribute("src") or ""
    png_bytes = decode_data_url_png(data_url)
    if qr_png is not None:
        qr_png.parent.mkdir(parents=True, exist_ok=True)
        scaled = _upscale_qr_png(png_bytes, factor=QR_PNG_SCALE)
        qr_png.write_bytes(scaled)
        log.info("QR PNG written to %s (%d bytes, upscaled %dx)",
                 qr_png, len(scaled), QR_PNG_SCALE)
    if terminal_qr:
        render_qr_to_terminal(png_bytes)
    return data_url


# ============================================================
# Main
# ============================================================

def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)

    # `--no-terminal-qr` is only sensible if the QR can still be seen
    # another way, i.e. via `--qr-png`. Enforce that pairing.
    if args.no_terminal_qr and args.qr_png is None and not args.check:
        raise SystemExit(
            "--no-terminal-qr requires --qr-png (otherwise there's "
            "nothing for the Access App to scan)."
        )

    # `--trace` is a paired flag — refuse to silently drop the
    # trace if --screenshot-dir is missing (AGENTS.md §3).
    if args.trace and args.screenshot_dir is None:
        raise SystemExit("--trace requires --screenshot-dir.")

    if args.check:
        return run_check(
            state_path=session.resolve_state_path(
                args.state_path, DEFAULT_STATE_PATH, LEGACY_STATE_PATH),
            screenshot_dir=args.screenshot_dir,
            trace=args.trace,
        )

    contract_number = resolve_contract_number(args)
    return run_login(
        contract_number=contract_number,
        state_path=args.state_path,
        mfa_timeout=args.mfa_timeout,
        qr_png=args.qr_png,
        terminal_qr=not args.no_terminal_qr,
        screenshot_dir=args.screenshot_dir,
        trace=args.trace,
    )


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
