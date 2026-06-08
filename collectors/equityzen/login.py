#!/usr/bin/env python3
"""equityzen session minter — headless, CLI-driven login with a stdin TOTP
prompt (no VNC).

Drives EquityZen's investor-portal login entirely headless: fills the
email + password form (the `submitLogIn` GraphQL mutation), prompts for the
6-digit authenticator code on stdin, submits it (the `loginTotp` mutation),
and persists the authenticated session to the Camoufox profile dir so
`download` can reuse it.

Flow:

  1. Launch a persistent Camoufox context on the profile dir at
     /secrets/equityzen-profile/. The browser runs headed in the
     container's Xvfb virtual display (no VNC) — matching the exact
     stealth fingerprint the explore session proved EquityZen accepts,
     rather than risking a headless bot-challenge on the one flow that
     submits credentials. The operator sees only the CLI; the browser is
     invisible. Firefox's password manager is disabled so a saved
     credential can't autofill on top of our fill.
  2. Navigate to /accounts/login/. EquityZen keeps a session alive until an
     explicit logout (TOTP-only 2FA, no "remember this device" checkbox),
     so if the profile already holds a valid session the portal bounces
     straight to the dashboard — we detect that and return without touching
     credentials or firing a 2FA push (the nightly-cron short-circuit).
  3. Otherwise fill EQUITYZEN_USERNAME (alias EQUITYZEN_EMAIL) /
     EQUITYZEN_PASSWORD (clear → fill → verify each field), submit, wait for
     the TOTP field, read the 6-digit code from stdin, submit it.
  4. Wait for the authenticated dashboard, then context.close() to flush
     cookies + storage back to the profile.

Selectors are from the explore capture: email `input#email`, password
`input#password`, TOTP `input#oneTimePassword`. Submit is an Enter
keypress on the active field (what the SPA's form listens for).

CLI surface:

  --profile-dir PATH   Persistent browser profile (default
                       /secrets/equityzen-profile/, shared with explore /
                       download). Holds the session. Treat as a credential.
  --env-file PATH      Bash-sourced env with EQUITYZEN_USERNAME /
                       EQUITYZEN_EMAIL / EQUITYZEN_PASSWORD.
  --check              Probe the existing profile (load the dashboard),
                       exit 0 if authenticated, 1 if not. No credentials
                       posted, no 2FA push — safe for cron healthchecks.
  --totp CODE          Supply the 6-digit code non-interactively (for
                       automation). Omit to be prompted on stdin.

Auth discipline (root CLAUDE.md §3): never weaken or skip 2FA, never add a
--password flag, never lower the profile dir below 0700. The fix for a
bot-challenge is a better stealth profile, never an auth bypass. Never log
out at the end — that would force a fresh 2FA push next run.
"""
from __future__ import annotations

import argparse
import json
import logging
import os
import re
import sys
from pathlib import Path
from urllib.parse import urlparse

from collectorkit import cli, envfile

log = logging.getLogger("equityzen.login")

LOGIN_URL = "https://equityzen.com/accounts/login/"
DASHBOARD_URL = "https://equityzen.com/welcome/"
LOGIN_PATH = "/accounts/login"

DEFAULT_PROFILE_DIR = Path("/secrets/equityzen-profile")
DEFAULT_ENV_FILE = Path("/secrets/equityzen.env")
DEFAULT_DEBUG_DIR = Path("/debug")

USER_ENVS = ("EQUITYZEN_USERNAME", "EQUITYZEN_EMAIL")
PASS_ENV = "EQUITYZEN_PASSWORD"

EMAIL_SEL = "input#email"
PWD_SEL = "input#password"
TOTP_SEL = "input#oneTimePassword"

# Firefox prefs that stop the built-in password manager from autofilling a
# saved credential on top of our programmatic fill (which concatenated the
# password field and got the login rejected). See
# collectors/equityzen/DESIGN.md / explore.py.
FIREFOX_PREFS = {
    "signon.rememberSignons": False,
    "signon.autofillForms": False,
    "signon.generation.enabled": False,
    "signon.management.page.breach-alerts.enabled": False,
}

TOTP_RE = re.compile(r"^\d{6}$")


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument(
        "--profile-dir", type=Path, default=DEFAULT_PROFILE_DIR,
        help=("Persistent browser profile dir. Holds the session cookie. "
              "Treat as a credential. Default: %(default)s."),
    )
    p.add_argument(
        "--env-file", type=Path, default=DEFAULT_ENV_FILE,
        help=("Bash-sourced env file with EQUITYZEN_USERNAME / "
              "EQUITYZEN_EMAIL / EQUITYZEN_PASSWORD. Skipped silently if "
              "absent. Default: %(default)s."),
    )
    p.add_argument(
        "--check", action="store_true",
        help=("Probe the stored profile. Exits 0 if authenticated, 1 if "
              "not. No credentials posted, no 2FA push — safe to call from "
              "cron / healthcheck."),
    )
    p.add_argument(
        "--totp", default=None, metavar="CODE",
        help=("Supply the 6-digit authenticator code non-interactively. "
              "Omit to be prompted on stdin."),
    )
    p.add_argument(
        "--timeout", type=int, default=45,
        help="Per-step navigation timeout in seconds. Default: %(default)s.",
    )
    p.add_argument(
        "--debug", action="store_true",
        help=("On a failed login, save a screenshot to the debug dir and log "
              "extra detail (which submit mechanism fired, visible buttons)."),
    )
    p.add_argument(
        "--debug-dir", type=Path, default=DEFAULT_DEBUG_DIR,
        help="Where --debug writes screenshots. Default: %(default)s.",
    )
    cli.add_common_args(p)
    return p.parse_args(argv)


def _attach_graphql_logger(context) -> None:
    """Log the HTTP status + any GraphQL errors for the two auth mutations,
    so a failed login says whether submitLogIn / loginTotp fired and what the
    server returned. Only auth ops are inspected — no other traffic is read,
    and no request/response bodies are logged (the password / TOTP live in
    those bodies)."""
    def on_response(resp) -> None:
        try:
            if "/api/graphql" not in resp.url:
                return
            ops = []
            try:
                body = json.loads(resp.request.post_data or "")
                for it in (body if isinstance(body, list) else [body]):
                    if isinstance(it, dict) and it.get("operationName"):
                        ops.append(it["operationName"])
            except Exception:
                pass
            if not any(o in ("submitLogIn", "loginTotp") for o in ops):
                return
            errs = ""
            try:
                j = resp.json()
                for it in (j if isinstance(j, list) else [j]):
                    e = isinstance(it, dict) and it.get("errors")
                    if e:
                        errs += "; ".join(str(x.get("message", ""))[:80] for x in e)
            except Exception:
                pass
            log.info("graphql %s -> HTTP %s%s", ",".join(ops), resp.status,
                     f"  ERRORS: {errs}" if errs else "  (no errors)")
        except Exception:
            pass
    context.on("response", on_response)


def _authenticated(page) -> bool:
    """True when the current page is an authenticated surface. EquityZen
    keeps the whole login + TOTP flow on /accounts/login/ and only leaves
    that path once fully authenticated (→ /welcome/), so the URL path is the
    reliable signal. We deliberately do NOT treat "#email field absent" as
    authenticated — the TOTP step is also on /accounts/login/ with no #email,
    and that false-positive previously reported a half-finished login as
    complete."""
    try:
        return LOGIN_PATH not in (urlparse(page.url).path or "")
    except Exception:
        return False


def _await_settled_auth(page, timeout_ms: int) -> bool:
    """After a goto, wait until the SPA has either rendered the login form
    (`#email`) or redirected off the login path, then report whether we are
    authenticated. This defeats the render race: right after
    domcontentloaded the React form has not mounted, so an immediate
    `#email`-absent check would look falsely authenticated."""
    try:
        page.wait_for_function(
            "() => !!document.querySelector('input#email') "
            "|| !location.pathname.includes('/accounts/login')",
            timeout=timeout_ms,
        )
    except Exception:
        # Neither condition settled in time — fall through to a best-effort
        # read of whatever state the page is in.
        pass
    return _authenticated(page)


def _fill_verify(page, selector: str, value: str, label: str, timeout_ms: int) -> None:
    """Clear → fill → verify a field holds exactly `value` (one retry).
    Defeats autofill / append-style stacking; raises if it won't stick."""
    field = page.locator(selector).first
    field.wait_for(state="visible", timeout=timeout_ms)
    for _ in range(2):
        field.fill("", timeout=timeout_ms)
        field.fill(value, timeout=timeout_ms)
        if field.input_value(timeout=timeout_ms) == value:
            return
    raise RuntimeError(
        f"{label} field did not accept the value cleanly — aborting rather "
        f"than submit a corrupted {label}."
    )


def _read_totp(supplied: str | None) -> str:
    if supplied is not None:
        code = supplied.strip()
    else:
        if not sys.stdin.isatty():
            log.warning("stdin is not a TTY; reading the TOTP code from a "
                        "piped line. Use --totp for automation.")
        code = input("EquityZen 6-digit authenticator code: ").strip()
    if not TOTP_RE.match(code):
        raise SystemExit(f"TOTP code must be exactly 6 digits (got {len(code)} chars).")
    return code


def _wait_off_login(page, timeout_ms: int) -> bool:
    """Wait until EquityZen redirects off the login path (→ authenticated),
    up to timeout_ms. Returns the resulting auth state."""
    try:
        page.wait_for_url(lambda u: LOGIN_PATH not in u, timeout=timeout_ms)
    except Exception:
        pass
    return _authenticated(page)


def _list_buttons(page) -> list:
    """Visible buttons / submit inputs on the current step (text/type/id) —
    a diagnostic for when no known submit control matches the 2FA form."""
    try:
        return page.eval_on_selector_all(
            "button, input[type='submit']",
            "els => els.filter(e => e.offsetParent !== null).map(e => ({"
            "text:(e.innerText||e.value||'').trim().slice(0,30), "
            "type:e.getAttribute('type'), id:e.id||null, disabled:!!e.disabled}))",
        )
    except Exception:
        return []


def _submit_totp(page, code: str, timeout_ms: int, debug: bool) -> bool:
    """Type the code and submit it. EquityZen's 2FA card submits via a
    `<button type="button">Submit</button>` onClick handler — NOT a form
    submit and NOT auto-submit-on-6-digits, so neither Enter nor typing
    alone fires `loginTotp` (both were observed to hang). The button click
    is therefore the primary path; Enter and a bare wait are kept as
    fallbacks against a future UI change. Returns True once authenticated."""
    totp = page.locator(TOTP_SEL).first
    totp.click()
    try:
        totp.fill("")
    except Exception:
        pass
    # Type as real per-key events so a React-controlled input registers the
    # value the way a human's keystrokes would.
    try:
        totp.press_sequentially(code, delay=80)
    except Exception:
        pass  # field may detach if a future variant auto-submits mid-type
    # Primary: click the 2FA submit button.
    for sel in ("button:has-text('Submit')", "button:has-text('Verify')",
                "button:has-text('Continue')", "button:has-text('Confirm')",
                "button[type='submit']"):
        try:
            b = page.locator(sel).first
            if b.count() and b.is_visible() and b.is_enabled():
                b.click(timeout=2000)
                if debug:
                    log.info("2FA submitted via %s", sel)
                if _wait_off_login(page, timeout_ms):
                    return True
        except Exception:
            continue
    # Fallbacks: Enter, then a plain wait for a form that auto-submits.
    try:
        if totp.count():
            totp.press("Enter")
    except Exception:
        pass
    if _wait_off_login(page, 3000):
        return True
    log.info("2FA did not submit via known controls; visible buttons: %s",
             _list_buttons(page))
    return _authenticated(page)


def _dump_debug(page, label: str, debug_dir: Path) -> None:
    try:
        debug_dir.mkdir(parents=True, exist_ok=True)
        shot = debug_dir / f"login-{label}.png"
        page.screenshot(path=str(shot))
        log.info("debug screenshot: %s  (final url: %s)", shot, page.url)
    except Exception as e:
        log.debug("debug dump failed: %s", e)


def _do_login(page, username: str, password: str, totp_supplied: str | None,
              timeout_ms: int, debug: bool, debug_dir: Path) -> None:
    """Run the full fresh-login flow: email/password → submitLogIn →
    TOTP → loginTotp → dashboard."""
    log.info("filling credentials")
    _fill_verify(page, EMAIL_SEL, username, "email", timeout_ms)
    _fill_verify(page, PWD_SEL, password, "password", timeout_ms)
    # The SPA login form submits on Enter (observed: an Enter keypress in the
    # password field fires the submitLogIn mutation).
    page.locator(PWD_SEL).press("Enter")

    # The TOTP field mounting = password accepted. If it never appears,
    # the password was rejected (or a bot-challenge intervened).
    log.info("waiting for the 2FA step")
    try:
        page.wait_for_selector(TOTP_SEL, timeout=timeout_ms)
    except Exception:
        if _authenticated(page):
            return  # no 2FA required (unexpected, but fine)
        if debug:
            _dump_debug(page, "no-totp-step", debug_dir)
        raise SystemExit(
            "The TOTP step never appeared after submitting credentials — "
            "EquityZen rejected the password, or a bot-challenge intervened. "
            "Check EQUITYZEN_PASSWORD in the env file."
        )

    code = _read_totp(totp_supplied)
    log.info("submitting the authenticator code")
    if not _submit_totp(page, code, timeout_ms, debug):
        if debug:
            _dump_debug(page, "totp-failed", debug_dir)
        raise SystemExit(
            "Login did not complete after the TOTP step — the code was wrong "
            "or expired, or the 2FA form needs a submit control we did not "
            "match (see the visible-buttons log above). Re-run `login` with a "
            "fresh code; add --debug for a screenshot + GraphQL trace."
        )


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)

    args.profile_dir.mkdir(parents=True, exist_ok=True)
    timeout_ms = args.timeout * 1000

    username = password = ""
    if not args.check:
        if envfile.source_env_file(args.env_file):
            log.info("env file:    %s (sourced)", args.env_file)
        username = next((os.environ[k] for k in USER_ENVS if os.environ.get(k)), "")
        password = os.environ.get(PASS_ENV, "")
        if not (username and password):
            raise SystemExit(
                f"{'/'.join(USER_ENVS)} and {PASS_ENV} must be set (via "
                f"{args.env_file} or the environment) for a fresh login."
            )

    from camoufox.sync_api import Camoufox

    with Camoufox(
        persistent_context=True,
        user_data_dir=str(args.profile_dir),
        os="macos",
        window=(1280, 800),
        headless=False,
        humanize=True,
        geoip=True,
        firefox_user_prefs=FIREFOX_PREFS,
    ) as context:
        _attach_graphql_logger(context)
        page = context.new_page()
        page.set_default_timeout(timeout_ms)

        if args.check:
            # Probe only — no credentials, no 2FA. Load the dashboard; if
            # the session is dead EquityZen redirects us to the login form.
            page.goto(DASHBOARD_URL, wait_until="domcontentloaded")
            ok = _await_settled_auth(page, timeout_ms)
            log.info("session %s", "valid" if ok else "INVALID / expired")
            return 0 if ok else 1

        page.goto(LOGIN_URL, wait_until="domcontentloaded")
        if _await_settled_auth(page, timeout_ms):
            log.info("existing session still valid — no login needed "
                     "(no 2FA push fired)")
            return 0

        _do_login(page, username, password, args.totp, timeout_ms,
                  args.debug, args.debug_dir)
        log.info("login complete — session persisted to %s", args.profile_dir)
        return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
