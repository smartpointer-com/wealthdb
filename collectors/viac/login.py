#!/usr/bin/env python3
"""
viac Phase 2: session minter (pure httpx, no browser).

Replays VIAC's auth flow against `app.viac.ch`, prompts for the
mTAN SMS code on stdin, and persists the resulting cookies +
CSRF metadata to a state file at chmod 0600.

Auth flow (see DESIGN.md §2.1 for the full table):

    GET    /                                                  (sets initial cookies)
    DELETE /external-login/public/authentication/flow/        (clear stale flow)
    POST   /external-login/public/authentication/password/check/
           {"username": <phone-E164>, "password": <pw>}      → 200; SMS sent
    POST   /external-login/public/authentication/mtan/otp/check/
           {"otp": <code>}                                    → 200
    GET    /external-login/public/authentication/             → 200 (session confirmed)
    POST   /rest/web/customer/loginHook                       → 204 (web-side activate)

Credentials are sourced from /secrets/viac.env (or
~/.secrets/viac.env outside the container); never from a CLI
flag — see CLAUDE.md §3. The `username` field is the login
phone number in E.164 format (`+CC<digits>`).

`--check` probes existing state with one cheap GET against the
heartbeat endpoint. No credential submit, no MFA push. Allowed
without operator authorisation per CLAUDE.md §2.

Non-`--check` invocation mints a fresh session AND sends an SMS
to the registered phone. Only allowed with explicit
authorisation per CLAUDE.md §2.
"""

from __future__ import annotations

import argparse
import json
import logging
import os
import shlex
import subprocess
import sys
from pathlib import Path

import httpx

from collectorkit import envfile

from viac_client import ViacClient

log = logging.getLogger("viac.login")

DEFAULT_ENV_FILE_CANDIDATES = (
    Path("/secrets/viac.env"),
    Path.home() / ".secrets" / "viac.env",
)
DEFAULT_STATE_PATH = Path("/secrets/viac-state.json")

LOGIN_ENV = "VIAC_LOGIN"
PASSWORD_ENV = "VIAC_PASSWORD"

def normalize_login(raw: str) -> str:
    """Cosmetic cleanup of an E.164 phone-number login.

    VIAC's API expects the mobile number in E.164 form with a
    country-code prefix. Strip whitespace, dashes, and parens
    after the leading `+` so the operator can use the prettified
    form (e.g. `+CC XX XXX XX XX`) in their viac.env. Validation
    that the result is actually E.164-shaped happens in main()
    via `looks_like_e164`.
    """
    s = raw.strip()
    if s.startswith("+"):
        return "+" + "".join(c for c in s[1:] if c.isdigit())
    # Preserve whatever the operator entered for the error
    # message; main() will reject it as non-E.164.
    return s


def looks_like_e164(s: str) -> bool:
    """True if `s` is a `+` followed by at least 8 digits.
    Doesn't pretend to validate against the full E.164 country-
    code registry — just enough to catch a missing or mangled
    country code."""
    return (
        s.startswith("+")
        and s[1:].isdigit()
        and len(s) >= 9
    )


def _redact_login(s: str) -> str:
    """Mask the middle of a phone-number-shaped string for logs.
    Shows first 4 + last 2 chars so the operator can recognise
    their own number without it landing in terminal scrollback."""
    if len(s) < 6:
        return "<too short>"
    return s[:4] + "*" * (len(s) - 6) + s[-2:]

# Bash bookkeeping vars we filter out when sourcing the env file.
_BASH_VAR_BLOCKLIST = frozenset({"_", "PWD", "OLDPWD", "SHLVL", "PATH"})


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument(
        "--state-path", default=DEFAULT_STATE_PATH, type=Path,
        help=(f"Where to read/write the session-state JSON. Default: "
              f"{DEFAULT_STATE_PATH}. Created at chmod 0600."),
    )
    p.add_argument(
        "--env-file", default=None, type=Path,
        help=("Path to a bash env file with VIAC_LOGIN + VIAC_PASSWORD. "
              "Defaults to /secrets/viac.env, falling back to "
              "~/.secrets/viac.env."),
    )
    p.add_argument(
        "--check", action="store_true",
        help=("Probe an existing state file. No credential submit, "
              "no SMS push. Exits 0 if the session is alive, 1 if dead."),
    )
    p.add_argument(
        "-v", "--verbose", action="store_true",
        help="DEBUG-level logging.",
    )
    return p.parse_args(argv)


def check_session(state_path: Path) -> int:
    """Probe an existing state file. Returns process exit code."""
    if not state_path.is_file():
        log.error("no state file at %s; run login.py to mint one.", state_path)
        return 1
    try:
        client = ViacClient.from_state(state_path)
    except Exception as e:
        log.error("could not load state file %s: %s", state_path, e)
        return 1
    with client:
        try:
            # `heartbeat` returns 204 on a live session, redirects /
            # 401s on a dead one. Cheap and side-effect-free.
            resp = client.get("/rest/web/heartbeat")
        except httpx.HTTPError as e:
            log.error("heartbeat failed: %s", e)
            print("DEAD")
            return 1
    if resp.status_code == 204:
        print("ALIVE")
        return 0
    log.warning("heartbeat returned %d (expected 204)", resp.status_code)
    print("DEAD")
    return 1


def mint_session(state_path: Path, username: str, password: str) -> int:
    """Perform the full auth flow + persist the resulting state."""
    with ViacClient() as client:
        # Step 1: hit the root to set the initial cookies, including
        # AL_SESS-S (empty session) and CSRFT<N>-S (CSRF token).
        log.info("bootstrap: GET /")
        resp = client.get("/")
        resp.raise_for_status()
        client._ensure_csrf_known()
        if not client.csrf_cookie_name:
            log.error("no CSRFT<N>-S cookie set by GET /; aborting.")
            return 2
        log.info("CSRF: cookie=%s header=%s",
                 client.csrf_cookie_name, client.csrf_header_name)

        # Step 2: clear any stale auth flow.
        log.info("clear prior flow: DELETE /external-login/public/authentication/flow/")
        resp = client.delete("/external-login/public/authentication/flow/")
        if resp.status_code not in (204, 200):
            log.warning("DELETE flow returned %d (expected 204)", resp.status_code)

        # Step 3: submit credentials. Response carries the masked
        # phone number that the SMS goes to — we echo it back so
        # the operator can sanity-check.
        log.info("POST /external-login/.../password/check")
        resp = client.post(
            "/external-login/public/authentication/password/check/",
            json={"username": username, "password": password},
        )
        if resp.status_code != 200:
            _diagnose_auth_error(
                resp,
                step="password/check",
                hint=(
                    "USERNAME_PASSWORD_WRONG: check VIAC_LOGIN includes "
                    "the country code (the web UI hides it behind a "
                    "drop-down but the API does not — e.g. "
                    "`+417XXXXXXXX`), and that VIAC_PASSWORD is "
                    "SINGLE-quoted in ~/.secrets/viac.env so $/`/! aren't "
                    "shell-expanded. RATE_LIMITED or similar: wait a few "
                    "minutes and try again."
                ),
            )
            return 2
        body = resp.json()
        attrs = (body.get("data") or {}).get("attributes") or {}
        phone = attrs.get("phoneNumber", "<unknown>")
        next_step = attrs.get("nextAuthStep")
        if next_step != "MTAN_OTP_REQUIRED":
            log.error("unexpected nextAuthStep %r (expected MTAN_OTP_REQUIRED)",
                      next_step)
            return 2

        # Step 4: prompt the operator for the OTP. Long timeout so
        # they can find their phone — see the [No immediate-response
        # interactive flows] memory.
        print(f"\nVIAC sent an SMS code to {phone}.", file=sys.stderr, flush=True)
        print("Enter the 6-digit OTP (or Ctrl-C to abort):",
              file=sys.stderr, flush=True)
        try:
            otp = input("OTP: ").strip()
        except (EOFError, KeyboardInterrupt):
            print("\naborted by operator.", file=sys.stderr)
            return 130

        # Step 5: submit OTP.
        log.info("POST /external-login/.../mtan/otp/check")
        resp = client.post(
            "/external-login/public/authentication/mtan/otp/check/",
            json={"otp": otp},
        )
        if resp.status_code != 200:
            _diagnose_auth_error(
                resp, step="mtan/otp/check",
                hint=("OTP_WRONG: enter the most recent 6-digit code SMS'd "
                      "to the phone. OTP_EXPIRED: re-run login.py to "
                      "trigger a fresh SMS."),
            )
            return 2

        # Step 6: confirm the session via GET /authentication/.
        log.info("GET /external-login/.../authentication (confirm)")
        resp = client.get("/external-login/public/authentication/")
        if resp.status_code != 200:
            log.error("authentication confirm returned %d", resp.status_code)
            return 2
        confirm = (resp.json().get("data") or {}).get("attributes") or {}
        user_id = confirm.get("userId", "<unknown>")
        log.info("authenticated as user %s", user_id)

        # Step 7: activate the web-side session.
        log.info("POST /rest/web/customer/loginHook")
        resp = client.post("/rest/web/customer/loginHook")
        if resp.status_code != 204:
            log.error("loginHook returned %d (expected 204)", resp.status_code)
            return 2

        # Persist.
        client.save_state(state_path)
        log.info("session persisted to %s (chmod 0600)", state_path)
        return 0


def _diagnose_auth_error(resp, *, step: str, hint: str) -> None:
    """Pull `errors[].code` out of a JSON:API error envelope and
    log a human-readable line plus a step-appropriate hint.
    Falls back to the raw body if the envelope is unexpected."""
    code = "<unknown>"
    try:
        body = resp.json()
    except Exception:
        body = None
    if isinstance(body, dict):
        errs = body.get("errors") or []
        if errs and isinstance(errs[0], dict):
            code = errs[0].get("code", code)
    log.error("%s returned HTTP %d, code=%s", step, resp.status_code, code)
    log.error("hint: %s", hint)
    if body is None:
        log.error("raw body: %s", resp.text[:500])


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )

    if args.check:
        return check_session(args.state_path)

    # Full login: source env file, pull credentials.
    env_path = envfile.resolve_env_file(args.env_file, DEFAULT_ENV_FILE_CANDIDATES)
    if env_path is None:
        log.info("no env file path resolved; relying on process env vars.")
    else:
        try:
            if envfile.source_env_file(env_path):
                log.info("env sourced from %s", env_path)
        except (subprocess.CalledProcessError, ValueError) as e:
            log.error("env file %s failed to source: %s", env_path, e)
            return 2

    username_raw = os.environ.get(LOGIN_ENV)
    password = os.environ.get(PASSWORD_ENV)
    if not username_raw or not password:
        log.error(
            "%s / %s not set. Populate ~/.secrets/viac.env (single "
            "quotes around values containing $/!/backtick). %s must "
            "be the mobile number in E.164 form including the "
            "country code (e.g. `+417XXXXXXXX`).",
            LOGIN_ENV, PASSWORD_ENV, LOGIN_ENV)
        return 2

    username = normalize_login(username_raw)
    if not looks_like_e164(username):
        log.error(
            "%s must start with a country code (`+`) and be an "
            "E.164 phone number, e.g. `+CC<digits>`. Got %r. "
            "The web UI hides the country code behind a drop-down; "
            "the API call we replay does not — the country code "
            "must be in viac.env.",
            LOGIN_ENV, _redact_login(username))
        return 2
    log.info("%s: %s", LOGIN_ENV, _redact_login(username))

    try:
        return mint_session(args.state_path, username, password)
    except httpx.HTTPError as e:
        log.error("HTTP error during login: %s", e)
        return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
