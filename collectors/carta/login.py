#!/usr/bin/env python3
"""carta session minter — Camoufox + CLI-2FA, persistent profile.

Drives the Carta holder-UI SPA login (`login.app.carta.com`), solves the
2FA challenge (code read from stdin), and persists the authenticated
session to a Camoufox profile dir so `download` can reuse it. Mirrors the
cointracking / fidelity-web login pattern, with two carta specifics:

  * **Camoufox, not vanilla Firefox.** Carta's login is fronted by
    Cloudflare (a `/cdn-cgi/challenge-platform/` Turnstile JS challenge).
    The explore phase confirmed Camoufox clears it; vanilla Playwright
    Firefox would risk a block. Run headed under the container's Xvfb (the
    entrypoint starts it), no VNC — 2FA is entered on stdin, not by hand.
  * **Single-step form + single-field 2FA.** Email + password render on one
    screen (`input[type=email]` + `input[type=password]`); submit is
    `#login-btn`. The 2FA code is one `#two-factor-code-input`; ticking
    `#enable-bypass` ("remember this device") plants a device-trust cookie
    so later logins skip the 2FA push. Submit is `#verify-challenge-btn`.

CLI surface (shared with the other carta subcommands):

  --profile-dir PATH   Persistent Camoufox profile (default
                       /secrets/carta-profile/, shared with explore /
                       download). Holds the session + device-trust cookie.
                       Treat the dir as a credential.
  --env-file PATH      Bash-sourced env with CARTA_EMAIL / CARTA_PASSWORD.
  --check              Probe the existing profile (load app.carta.com), exit
                       0 if authenticated, 1 if not. No credentials posted,
                       no 2FA push — safe for cron healthchecks.

Auth discipline (root CLAUDE.md §3): never weaken or skip 2FA, never add a
--password flag, never lower the profile dir below 0700. The fix for a
bot-challenge is a better Camoufox profile, never an auth bypass.
"""
from __future__ import annotations

import argparse
import logging
import os
import re
import sys
from pathlib import Path

from collectorkit import cli, envfile

log = logging.getLogger("carta.login")

BASE = "https://app.carta.com"
# app.carta.com redirects: authenticated → /investors/individual/<id>/...;
# unauthenticated → login.app.carta.com/credentials/login/.
LOGIN_HOST = "login.app.carta.com"

USER_ENV = "CARTA_EMAIL"
PASS_ENV = "CARTA_PASSWORD"

DEFAULT_PROFILE_DIR = Path("/secrets/carta-profile")
DEFAULT_ENV_FILE = Path("/secrets/carta.env")

# Login-form selectors. The email/password heuristic is the same one the
# explore harness's pre-fill matched on Carta's form. The 2FA + submit ids
# are the concrete ids observed in the explore click log.
USER_SELECTOR = (
    "input[type='email'], "
    "input[autocomplete='username'], "
    "input[autocomplete='email'], "
    "input[name*='email' i], "
    "input[name*='user' i], "
    "input[name*='login' i]"
)
PWD_SELECTOR = "input[type='password']"
LOGIN_BTN_SELECTOR = "#login-btn, button[type='submit']"
CODE_2FA_SELECTOR = (
    "#two-factor-code-input, "
    "input[autocomplete='one-time-code'], "
    "input[name*='code' i]"
)
REMEMBER_SELECTOR = "#enable-bypass, input[name*='bypass' i]"
VERIFY_BTN_SELECTOR = "#verify-challenge-btn, button[type='submit']"


def prompt_for_2fa() -> str:
    """Print a prompt to stderr (visible under output redirect) and read one
    line from stdin. Strips whitespace."""
    sys.stderr.write("\n" + "=" * 60 + "\n")
    sys.stderr.write(
        "Carta 2FA: enter your authentication code, then press Enter.\n> "
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


def is_authenticated(page) -> bool:
    """True if `page` is currently on the authenticated app (not bounced to
    the Cloudflare-fronted login host and not showing a login form). Carta
    sends app.carta.com → login.app.carta.com when the session is missing,
    and → /investors/individual/<id>/portfolio/ when it is valid."""
    url = page.url
    if LOGIN_HOST in url or "/accounts/login" in url or "/credentials/" in url:
        return False
    if "app.carta.com" not in url:
        return False
    # Belt-and-braces: a stray login form on an app.carta.com URL = not in.
    if (page.locator(PWD_SELECTOR).count() > 0
            and page.locator(USER_SELECTOR).count() > 0):
        return False
    return True


def probe(page) -> bool:
    """Load app.carta.com and report whether the session is authenticated.
    Carta resolves the redirect chain client-side, so we wait for the app
    (or the login host) to settle rather than trusting the first response."""
    page.goto(BASE, wait_until="domcontentloaded", timeout=45_000)
    # Give the SPA redirect chain a moment to resolve to its final host.
    for _ in range(20):
        if is_authenticated(page):
            return True
        if LOGIN_HOST in page.url:
            return False
        page.wait_for_timeout(500)
    return is_authenticated(page)


def login(page, email: str, password: str) -> None:
    """Drive the headed (Xvfb) Camoufox browser through Carta's login.
    Mutates the persistent context (cookies + storage) in place. Raises
    RuntimeError on any unrecoverable failure."""
    page.goto(BASE, wait_until="domcontentloaded", timeout=45_000)

    # Renewal short-circuit: a still-valid device-trust cookie lands us
    # straight in the app without a login form.
    if is_authenticated(page):
        log.info("existing session still valid; no login needed")
        return

    log.info("waiting for the login form")
    try:
        page.wait_for_selector(PWD_SELECTOR, state="visible", timeout=30_000)
    except Exception:
        raise RuntimeError(
            f"login form did not appear. Current URL: {page.url}. Carta may "
            f"be showing a Cloudflare interactive challenge or the markup "
            f"changed — re-run `./carta explore` to re-map."
        )

    log.info("filling credentials")
    page.locator(USER_SELECTOR).first.fill(email, timeout=10_000)
    page.locator(PWD_SELECTOR).first.fill(password, timeout=10_000)

    log.info("submitting login form")
    page.locator(LOGIN_BTN_SELECTOR).first.click(timeout=10_000)

    # Wait for either the 2FA prompt OR a direct landing (device-trust still
    # valid and the password step re-issued the session without a challenge).
    try:
        page.wait_for_selector(
            CODE_2FA_SELECTOR, state="visible", timeout=25_000,
        )
    except Exception:
        if is_authenticated(page):
            log.info("device-trust cookie valid; 2FA bypassed")
            return
        body = page.content()[:1500].replace("\n", " ").strip()
        raise RuntimeError(
            f"timed out waiting for the 2FA prompt. Current URL: {page.url}. "
            f"Wrong credentials, or the login schema changed. Page head: {body}"
        )

    code = prompt_for_2fa()
    if not code:
        raise RuntimeError("no 2FA code entered; aborting login")
    if not re.fullmatch(r"\d{6}", code):
        log.warning("2FA code is %d chars — expected 6 digits, "
                    "submitting anyway", len(code))

    page.locator(CODE_2FA_SELECTOR).first.fill(code, timeout=10_000)

    # Tick "remember this device" (#enable-bypass) so future logins skip the
    # 2FA push. Carta styles it as a custom checkbox (the native <input> is
    # visually replaced by a styled label), so we set .checked via JS and
    # fire a change event rather than relying on a click landing on the
    # invisible input. Non-fatal if it doesn't stick — login still works,
    # just without the long-lived device cookie.
    remember = page.locator(REMEMBER_SELECTOR).first
    if remember.count() > 0:
        try:
            remember.evaluate("""el => {
                el.checked = true;
                el.dispatchEvent(new Event('change', { bubbles: true }));
            }""")
            if remember.is_checked():
                log.info("'remember this device' ticked (skips future 2FA)")
            else:
                log.warning("'remember this device' did not stay ticked; "
                            "future runs may require a fresh 2FA push")
        except Exception as exc:
            log.warning("could not tick 'remember this device': %s", exc)
    else:
        log.warning("'remember this device' checkbox not found; future runs "
                    "may require a fresh 2FA push")

    log.info("submitting 2FA form")
    page.locator(VERIFY_BTN_SELECTOR).first.click(timeout=10_000)

    # Land back in the app. Carta resolves the post-2FA redirect to
    # app.carta.com/investors/...; wait for the login host to drop away.
    try:
        page.wait_for_function(
            "() => !location.host.includes('login.app.carta.com')",
            timeout=45_000,
        )
    except Exception:
        body = page.content()[:1500].replace("\n", " ").strip()
        raise RuntimeError(
            f"did not leave the login host after 2FA. Current URL: "
            f"{page.url}. Likely an invalid 2FA code. Page head: {body}"
        )

    log.info("login successful — landed on %s", page.url.split("?")[0])


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument(
        "--profile-dir", type=Path, default=DEFAULT_PROFILE_DIR,
        help=("Persistent Camoufox profile dir (cookies + device-trust). "
              "Treat as a credential. Default: %(default)s."),
    )
    p.add_argument(
        "--env-file", type=Path, default=DEFAULT_ENV_FILE,
        help=("Bash-sourced env file with CARTA_EMAIL / CARTA_PASSWORD. "
              "Skipped silently if absent. Default: %(default)s."),
    )
    p.add_argument(
        "--check", action="store_true",
        help=("Probe the stored profile by loading app.carta.com. Exits 0 if "
              "authenticated, 1 if not. No credentials posted, no 2FA push — "
              "safe to call from cron / healthcheck."),
    )
    cli.add_common_args(p)
    return p.parse_args(argv)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)

    envfile.source_env_file(args.env_file)
    args.profile_dir.mkdir(parents=True, exist_ok=True)
    # 0700 on the profile dir — it holds the session + device-trust cookie.
    try:
        args.profile_dir.chmod(0o700)
    except PermissionError:
        pass  # host-side perms already apply if we don't own it

    # Imported lazily so `--help` doesn't pay the Camoufox import cost.
    from camoufox.sync_api import Camoufox

    with Camoufox(
        persistent_context=True,
        user_data_dir=str(args.profile_dir),
        os="macos",
        window=(1280, 800),
        headless=False,   # headed under the entrypoint's Xvfb; clears Cloudflare
        humanize=True,
        geoip=True,
    ) as context:
        page = context.new_page()

        if args.check:
            if probe(page):
                log.info("session OK — app.carta.com reached, authenticated")
                return 0
            log.error("session invalid — bounced to the login host or a "
                      "login form is showing")
            return 1

        if probe(page):
            log.info("existing profile is still valid — no re-login needed")
            return 0

        email = os.environ.get(USER_ENV)
        password = os.environ.get(PASS_ENV)
        if not email or not password:
            log.error("%s / %s not set in env; cannot log in. Populate %s "
                      "and retry.", USER_ENV, PASS_ENV, args.env_file)
            return 1

        try:
            login(page, email, password)
        except RuntimeError as exc:
            log.error("login failed: %s", exc)
            return 1

        if not probe(page):
            log.error("login appeared to succeed but the follow-up probe "
                      "failed; the profile may be corrupted")
            return 1

        log.info("session minted in %s", args.profile_dir)
        return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
