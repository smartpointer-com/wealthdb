#!/usr/bin/env python3
"""cointracking session minter — headless Playwright Firefox, CLI-MFA.

Pure-HTTP login proved unviable: /login.php's POST 1 sends the
password as a session-encrypted blob (32 chars of mixed-case ASCII
including `#`/`%`/`^` — not any standard hash or encoding) computed
by JS on the page. The server accepts md5(password) in POST 1 far
enough to return the 2FA challenge form, but the post-1 session
state isn't fully valid and POST 2 gets bounced back to /login.php.
Reverse-engineering the JS encryption would be fragile.

So we drive a real browser instead. Vanilla Playwright Firefox in
headless mode — no Camoufox stealth, no Xvfb, no VNC, no display.
The browser runs the page's JS-side encryption for free. CLI-MFA
prompt unchanged (stdin); the browser fills the field.

Same CLI surface as the rest of the cointracking subcommands:

  --profile-dir PATH   Persistent Firefox profile (default
                       /secrets/cointracking-profile/, the same
                       dir explore.py uses). Holds the multi-year
                       ctfa<user_id> device-trust cookie so
                       future runs skip the 2FA push.
  --env-file PATH      Bash-sourced env with COINTRACKING_USERNAME
                       / COINTRACKING_PASSWORD.
  --check              Probe the existing profile via GET
                       /dashboard, exit 0 if authenticated, 1 if
                       not. No credentials posted, no 2FA push —
                       safe for cron healthchecks.
"""
from __future__ import annotations

import argparse
import logging
import os
import re
import sys
from pathlib import Path

from collectorkit import cli, envfile, launch

log = logging.getLogger("cointracking.login")

BASE = "https://cointracking.info"
LOGIN_URL = f"{BASE}/login.php"
DASHBOARD_URL = f"{BASE}/dashboard"

USER_ENV = "COINTRACKING_USERNAME"
PASS_ENV = "COINTRACKING_PASSWORD"

DEFAULT_PROFILE_DIR = Path("/secrets/cointracking-profile")
DEFAULT_ENV_FILE = Path("/secrets/cointracking.env")

# Form selectors. USER_SELECTOR + PWD_SELECTOR are the same
# heuristic the explore harness's MutationObserver landed on
# (anchored on standard HTML conventions). 2FA selector and
# dont_ask_again are specific to cointracking's known markup.
USER_SELECTOR = (
    "input[type='email'], "
    "input[autocomplete='username'], "
    "input[autocomplete='email'], "
    "input[name*='email' i], "
    "input[name*='user' i], "
    "input[name*='login' i]"
)
PWD_SELECTOR = "input[type='password']"
CODE_2FA_SELECTOR = "input[name='code_2fa']"
DONT_ASK_SELECTOR = "input#dont_ask_again, input[name='dont_ask_again']"
SUBMIT_SELECTOR = (
    "button[type='submit'], input[type='submit'], "
    "button:has-text('Login')"
)
# Unauthenticated /dashboard renders a marketing/demo view containing
# the literal text "You need to be logged in: to see this page".
# Authenticated /dashboard does NOT contain this text. This is the
# canonical negative marker for the probe — far more reliable than
# checking the URL (cointracking serves both states under the same
# /dashboard URL with no redirect).
UNAUTH_MARKER = "text=You need to be logged in"


def prompt_for_2fa() -> str:
    """Print a prompt to stderr (visible under output redirect) and
    read one line from stdin. Strips whitespace."""
    sys.stderr.write("\n" + "=" * 60 + "\n")
    sys.stderr.write(
        "CoinTracking 2FA: enter your authenticator code, "
        "then press Enter.\n> "
    )
    sys.stderr.flush()
    try:
        line = sys.stdin.readline()
    except KeyboardInterrupt:
        sys.stderr.write("\n")
        raise
    sys.stderr.write("=" * 60 + "\n")
    sys.stderr.flush()
    return line.strip()


def is_dashboard(page) -> bool:
    """Return True if `page` is currently the AUTHENTICATED
    dashboard. cointracking serves the same /dashboard URL to both
    auth states; the unauthenticated view contains a marketing demo
    with the literal text 'You need to be logged in'. Absence of
    that marker is the canonical positive signal."""
    if "/dashboard" not in page.url:
        return False
    if page.locator(UNAUTH_MARKER).count() > 0:
        return False
    # Belt-and-braces: if cointracking ever switches to embedding
    # the login form on /dashboard, detect that too.
    if (page.locator(USER_SELECTOR).count() > 0
            and page.locator(PWD_SELECTOR).count() > 0):
        return False
    return True


def probe(page) -> bool:
    """Navigate to /dashboard. True iff the session is authenticated
    (page settles on /dashboard without surfacing a login form)."""
    page.goto(DASHBOARD_URL, wait_until="domcontentloaded", timeout=30_000)
    return is_dashboard(page)


def login(page, username: str, password: str) -> None:
    """Drive the headless browser through cointracking's login.
    Mutates the page's persistent context (cookies + storage) in
    place. Raises RuntimeError on any unrecoverable failure."""
    page.goto(LOGIN_URL, wait_until="domcontentloaded", timeout=30_000)

    # Renewal short-circuit: if the device-trust cookie is still
    # valid, /login.php redirects straight to /dashboard.
    if is_dashboard(page):
        log.info("device-trust cookie already valid; no login needed")
        return

    log.info("filling credentials")
    page.locator(USER_SELECTOR).first.fill(username, timeout=10_000)
    page.locator(PWD_SELECTOR).first.fill(password, timeout=10_000)

    log.info("submitting initial login form")
    page.locator(SUBMIT_SELECTOR).first.click(timeout=10_000)

    # Wait for either the 2FA prompt to render OR the dashboard
    # to land directly (if device-trust was valid but the page
    # auto-routed through /login.php briefly).
    try:
        page.wait_for_selector(
            CODE_2FA_SELECTOR, state="visible", timeout=20_000,
        )
    except Exception:
        if is_dashboard(page):
            log.info("device-trust cookie valid; bypassed 2FA")
            return
        raise RuntimeError(
            f"timed out waiting for 2FA prompt. Current URL: "
            f"{page.url}. The login schema may have changed; "
            f"re-run explore."
        )

    code = prompt_for_2fa()
    if not code:
        raise RuntimeError("no 2FA code entered; aborting login")
    if not re.fullmatch(r"\d{6}", code):
        log.warning("2FA code is %d chars (%r) — expected 6 digits, "
                    "submitting anyway", len(code), code)

    page.locator(CODE_2FA_SELECTOR).fill(code, timeout=10_000)

    # Tick the "Don't ask again" checkbox. Plants the multi-year
    # ctfa<user_id> cookie so subsequent runs skip the 2FA push.
    #
    # cointracking styles this as a custom checkbox: the native
    # <input> has display:none and a sibling label/div carries the
    # visible UI. Playwright's check() / set_checked() — even with
    # force=True — refuse to "click" an invisible element. So we
    # drive it through JS directly: set .checked and dispatch a
    # change event so any client-side listeners see the state
    # change. Form submission picks up the checked property from
    # the native input regardless of display state.
    dont_ask = page.locator(DONT_ASK_SELECTOR).first
    if dont_ask.count() > 0:
        try:
            dont_ask.evaluate("""el => {
                el.checked = true;
                el.dispatchEvent(new Event('change', { bubbles: true }));
            }""")
            # Verify it stuck — is_checked() reads the .checked
            # property regardless of visibility.
            if dont_ask.is_checked():
                log.info("'don't ask again' ticked (long-lived device cookie)")
            else:
                log.warning("'don't ask again' did not stay ticked "
                            "after JS set; future runs may require "
                            "a fresh 2FA push")
        except Exception as exc:
            # Non-fatal — login still works, just no long-lived cookie.
            log.warning("could not tick 'don't ask again': %s", exc)
    else:
        log.warning("'don't ask again' checkbox not found; future "
                    "runs may require a fresh 2FA push")

    log.info("submitting 2FA form")
    page.locator(SUBMIT_SELECTOR).first.click(timeout=10_000)

    try:
        page.wait_for_url("**/dashboard*", timeout=30_000)
    except Exception:
        # Dump the current page so we can see exactly what went
        # wrong (cointracking sometimes re-renders with a visible
        # error: "Invalid 2FA code", "Session expired", etc.).
        body = page.content()[:2000].replace("\n", " ").strip()
        raise RuntimeError(
            f"did not land on /dashboard after 2FA submit. "
            f"Current URL: {page.url}. Page body (first 2000 chars):"
            f"\n{body}"
        )

    log.info("login successful — landed on %s", page.url)


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument(
        "--profile-dir", type=Path, default=DEFAULT_PROFILE_DIR,
        help=("Persistent Firefox profile dir. Playwright stores "
              "cookies + localStorage here across runs; the "
              "long-lived ctfa<user_id> device-trust cookie lives "
              "in here. Treat the dir as a credential. Default: "
              "%(default)s."),
    )
    p.add_argument(
        "--env-file", type=Path, default=DEFAULT_ENV_FILE,
        help=("Bash-sourced env file with COINTRACKING_USERNAME / "
              "COINTRACKING_PASSWORD. Skipped silently if absent. "
              "Default: %(default)s."),
    )
    p.add_argument(
        "--check", action="store_true",
        help=("Probe the stored profile by GET /dashboard. Exits 0 "
              "if authenticated, 1 if not. No credentials posted, "
              "no 2FA push — safe to call from cron / healthcheck."),
    )
    cli.add_standard_args(p, verb="login")
    return p.parse_args(argv)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)

    envfile.source_env_file(args.env_file, prefer_file=True)
    args.profile_dir.mkdir(parents=True, exist_ok=True)
    # 0700 on the profile dir. The wrapper bind-mounts ~/.secrets/
    # (already 0700 by convention) onto /secrets/, but inside the
    # container the dir may have been created with whatever umask;
    # explicitly tightening here is cheap insurance.
    try:
        args.profile_dir.chmod(0o700)
    except PermissionError:
        # If we don't own it, the host-side perms already apply.
        pass

    # Imported lazily so `--help` doesn't pay the Playwright import
    # cost.
    from playwright.sync_api import sync_playwright

    with sync_playwright() as pw:
        context = pw.firefox.launch_persistent_context(
            user_data_dir=str(args.profile_dir),
            headless=True,
            viewport={"width": 1280, "height": 800},
            firefox_user_prefs=launch.firefox_prefs(),
        )
        try:
            page = context.new_page()

            if args.check:
                ok = probe(page)
                if ok:
                    log.info("session OK — /dashboard reached without "
                             "login form")
                    return 0
                log.error("session invalid — /dashboard not reached "
                          "or login form is showing")
                return 1

            if probe(page):
                log.info("existing profile is still valid — no "
                         "re-login needed")
                return 0

            username = os.environ.get(USER_ENV)
            password = os.environ.get(PASS_ENV)
            if not username or not password:
                log.error("%s / %s not set in env; cannot log in. "
                          "Populate %s and retry.",
                          USER_ENV, PASS_ENV, args.env_file)
                return 1

            try:
                login(page, username, password)
            except RuntimeError as exc:
                log.error("login failed: %s", exc)
                return 1

            if not probe(page):
                log.error("login appeared to succeed but the "
                          "follow-up probe failed; the profile may "
                          "be corrupted")
                return 1

            log.info("session minted in %s", args.profile_dir)
            return 0
        finally:
            # context.close() writes any in-flight profile updates
            # (cookies, localStorage) back to disk. Without this the
            # session cookie would not persist across runs.
            context.close()


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
