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
  --env-file PATH      Bash-sourced env with CARTA_USERNAME (or legacy CARTA_EMAIL) / CARTA_PASSWORD.
  --check              Probe the existing profile (load app.carta.com), exit
                       0 if authenticated, 1 if not. No credentials posted,
                       no 2FA push — safe for cron healthchecks.

Auth discipline (root AGENTS.md §3): never weaken or skip 2FA, never add a
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

from collectorkit import cli, envfile, launch

log = logging.getLogger("carta.login")

BASE = "https://app.carta.com"
# app.carta.com redirects: authenticated → /investors/individual/<id>/...;
# unauthenticated → login.app.carta.com/credentials/login/.
LOGIN_HOST = "login.app.carta.com"

# Canonical login-id env first, the alias second: read CARTA_USERNAME,
# falling back to CARTA_EMAIL.
USER_ENVS = ("CARTA_USERNAME", "CARTA_EMAIL")
PASS_ENV = "CARTA_PASSWORD"

DEFAULT_PROFILE_DIR = Path("/secrets/carta-profile")
DEFAULT_ENV_FILE = Path("/secrets/carta.env")

# Login-form selectors, matched to the two-step flow observed 2026-08-01:
# step 1 takes the email (#username) behind #email-next-btn, step 2 renders
# #email-display + #password behind #password-continue-btn — and neither
# button is type=submit. The field selectors keep the generic fallbacks so
# a single-step variant of the form still matches; on drift the error
# paths dump a page_summary. The 2FA ids are the concrete ids from the
# explore click log (pre-dating the two-step flow; unverified against it).
USER_SELECTOR = (
    "input[type='email'], "
    "input[autocomplete='username'], "
    "input[autocomplete='email'], "
    "input[name*='email' i], "
    "input[name*='user' i], "
    "input[name*='login' i]"
)
PWD_SELECTOR = "input[type='password']"
EMAIL_NEXT_SELECTOR = "#email-next-btn"
LOGIN_BTN_SELECTOR = "#password-continue-btn, button[type='submit']"
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
    return cli.prompt_on_stderr(
        "Carta 2FA: enter your authentication code, then press Enter.")


def page_summary(page) -> str:
    """Compact page fingerprint for error messages: URL, title, and the
    visible input/button inventory. Enough to tell a bot-detection
    interstitial from a markup change in a headless failure log, without
    dumping raw HTML."""
    try:
        parts = page.evaluate(
            """() => {
              const vis = el => el.offsetWidth || el.offsetHeight;
              return {
                title: document.title,
                inputs: [...document.querySelectorAll('input')].filter(vis)
                  .map(el => el.type + (el.id ? '#' + el.id : '')),
                buttons: [...document.querySelectorAll('button')].filter(vis)
                  .map(el => (el.id ? '#' + el.id : '<' + el.type + '>')
                       + JSON.stringify((el.textContent || '')
                                        .trim().slice(0, 25))),
              };
            }"""
        )
        return (f"Current URL: {page.url}; title {parts['title']!r}; "
                f"visible inputs {parts['inputs']}; buttons {parts['buttons']}")
    except Exception as exc:
        return f"Current URL: {page.url} (page inspection failed: {exc!r})"


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
        page.wait_for_selector(f"{USER_SELECTOR}, {PWD_SELECTOR}",
                               state="visible", timeout=30_000)
    except Exception as exc:
        raise RuntimeError(
            f"login form did not appear. {page_summary(page)}. Carta may "
            f"be showing a Cloudflare interactive challenge or the markup "
            f"changed — re-run `./carta explore` to re-map."
        ) from exc

    if page.locator(PWD_SELECTOR).count() == 0:
        # Two-step flow: the email screen precedes the password screen.
        log.info("submitting email (step 1 of 2)")
        page.locator(USER_SELECTOR).first.fill(email, timeout=10_000)
        page.locator(EMAIL_NEXT_SELECTOR).first.click(timeout=10_000)
        try:
            page.wait_for_selector(PWD_SELECTOR, state="visible",
                                   timeout=25_000)
        except Exception as exc:
            raise RuntimeError(
                f"password step did not appear after the email submit. "
                f"{page_summary(page)}"
            ) from exc
    elif page.locator(USER_SELECTOR).count() > 0:
        # Single-step form: both fields on one screen.
        page.locator(USER_SELECTOR).first.fill(email, timeout=10_000)

    log.info("filling password + submitting")
    page.locator(PWD_SELECTOR).first.fill(password, timeout=10_000)
    page.locator(LOGIN_BTN_SELECTOR).first.click(timeout=10_000)

    # Wait for either the 2FA prompt OR a direct landing (device-trust still
    # valid and the password step re-issued the session without a challenge).
    try:
        page.wait_for_selector(
            CODE_2FA_SELECTOR, state="visible", timeout=25_000,
        )
    except Exception as exc:
        if is_authenticated(page):
            log.info("device-trust cookie valid; 2FA bypassed")
            return
        raise RuntimeError(
            f"timed out waiting for the 2FA prompt. Wrong credentials, or "
            f"the login schema changed. {page_summary(page)}"
        ) from exc

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
    except Exception as exc:
        raise RuntimeError(
            f"did not leave the login host after 2FA (likely an invalid "
            f"code). {page_summary(page)}"
        ) from exc

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
        help=("Bash-sourced env file with CARTA_USERNAME (or legacy CARTA_EMAIL) / CARTA_PASSWORD. "
              "Skipped silently if absent. Default: %(default)s."),
    )
    p.add_argument(
        "--check", action="store_true",
        help=("Probe the stored profile by loading app.carta.com. Exits 0 if "
              "authenticated, 1 if not. No credentials posted, no 2FA push — "
              "safe to call from cron / healthcheck."),
    )
    cli.add_standard_args(p, verb="login")
    return p.parse_args(argv)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)

    envfile.source_env_file(args.env_file, prefer_file=True)
    # 0700 profile dir (holds the session + device-trust cookie), with its
    # regenerable startupCache relocated out of the secrets tree.
    launch.prepare_profile_dir(args.profile_dir)

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
        firefox_user_prefs=launch.firefox_prefs(),
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

        email = next((os.environ[k] for k in USER_ENVS if os.environ.get(k)), None)
        password = os.environ.get(PASS_ENV)
        if not email or not password:
            log.error("%s / %s not set in env; cannot log in. Populate %s "
                      "and retry.", USER_ENVS[0], PASS_ENV, args.env_file)
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
