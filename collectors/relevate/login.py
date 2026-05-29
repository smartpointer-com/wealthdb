#!/usr/bin/env python3
"""
REST auth for Relevate (portal.pens-expert.ch).

Replays the three-step Airlock IAM flow:

  1. GET  /auth/ui/app/auth/flow/b2c/password?lang=en
     — primes the cookie jar (AL_SESS-S, CSRFT759-S,
       AL_LoginFromNewDevice).
  2. POST /auth/rest/public/authentication/applications/b2c/access
     — null body; returns 401 with the JSON:API challenge
       envelope that declares the current step.
  3. POST /auth/rest/public/authentication/password/check
     — body {"username": ..., "password": ...}; on 200 Airlock
       sends an mTAN to the registered phone and returns the
       phone number + nextAuthStep.
  4. POST /auth/rest/public/authentication/mtan/otp/check
     — body {"otp": ...}; on 200 the session cookie is promoted
       server-side from "anonymous" to "authenticated". No
       bearer token is issued — the SPA's Authorization header
       literally carries the string "bearer undefined".
  5. GET  /auth/rest/protected/self-service/ui/configuration/portal
     — landmark probe to confirm the session is live before
       persisting the state file.

Persists the cookie jar to ~/.secrets/relevate-state.json (chmod
600). Subsequent `download.py` runs load the jar and reuse the
session until Airlock expires it server-side.

--check loads an existing state file and probes the landmark
without re-authenticating. Reports ALIVE / DEAD / MISSING. Allowed
without explicit user permission (no credential submit, no mTAN
push).

Credentials are read from the RELEVATE_LOGIN / RELEVATE_PASSWORD
env vars. The host wrapper sources ~/.secrets/relevate.env before
invoking docker, so an `export KEY='value'` line in that file is
enough. No --password flag.
"""

from __future__ import annotations

import argparse
import json
import logging
import os
import sys
import textwrap
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import requests
from requests.exceptions import RequestException

from collectorkit import session

BASE = "https://portal.pens-expert.ch"

LOGIN_PAGE = f"{BASE}/auth/ui/app/auth/flow/b2c/password?lang=en"
B2C_ACCESS = f"{BASE}/auth/rest/public/authentication/applications/b2c/access"
PASSWORD_CHECK = f"{BASE}/auth/rest/public/authentication/password/check"
MTAN_CHECK = f"{BASE}/auth/rest/public/authentication/mtan/otp/check"
PROBE = f"{BASE}/auth/rest/protected/self-service/ui/configuration/portal"

DEFAULT_STATE_PATH = Path("/secrets/relevate-state.json")

# Realistic Chrome-on-macOS UA + matching client-hint headers.
# Airlock didn't appear to fingerprint beyond accepting valid
# headers, but the SPA sends these values so we mirror them.
USER_AGENT = (
    "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) "
    "AppleWebKit/537.36 (KHTML, like Gecko) "
    "Chrome/147.0.0.0 Safari/537.36"
)
SEC_CH_UA = (
    '"Not_A Brand";v="99", "Chromium";v="147", "Google Chrome";v="147"'
)

logger = logging.getLogger("login")


class LoginError(Exception):
    pass


# ----------------------------------------------------------------------
# CookieJar persistence (manual JSON; no pickle, no LWP — readable
# and chmod-600-friendly).
# ----------------------------------------------------------------------


def jar_to_state(jar: requests.cookies.RequestsCookieJar) -> list[dict[str, Any]]:
    out: list[dict[str, Any]] = []
    for c in jar:
        out.append({
            "name": c.name,
            "value": c.value,
            "domain": c.domain,
            "path": c.path,
            "secure": bool(c.secure),
            "expires": c.expires,  # int | None (seconds since epoch)
            "rest": dict(c._rest) if hasattr(c, "_rest") else {},
        })
    return out


def state_into_jar(state: list[dict[str, Any]], jar: requests.cookies.RequestsCookieJar) -> None:
    for c in state:
        jar.set(
            c["name"], c["value"],
            domain=c.get("domain"),
            path=c.get("path", "/"),
            secure=c.get("secure", False),
            expires=c.get("expires"),
            rest=c.get("rest", {}),
        )


def save_state(path: Path, jar: requests.cookies.RequestsCookieJar) -> None:
    payload = {
        "minted_at": session.iso_now(),
        "issuer": "portal.pens-expert.ch",
        "schema_version": 1,
        "cookies": jar_to_state(jar),
    }
    session.save_state(path, payload)
    logger.info("state saved: %s (%d cookies)", path, len(payload["cookies"]))


def load_state(path: Path) -> dict[str, Any] | None:
    return session.load_state(path)


# ----------------------------------------------------------------------
# Session factory
# ----------------------------------------------------------------------

def new_session() -> requests.Session:
    s = requests.Session()
    s.headers.update({
        "User-Agent": USER_AGENT,
        "Accept": "application/vnd.api+json, application/json, */*",
        "Accept-Language": "en-US,en;q=0.9",
        "Origin": BASE,
        "Referer": LOGIN_PAGE,
        "Sec-Ch-Ua": SEC_CH_UA,
        "Sec-Ch-Ua-Mobile": "?0",
        "Sec-Ch-Ua-Platform": '"macOS"',
        "Sec-Fetch-Dest": "empty",
        "Sec-Fetch-Mode": "cors",
        "Sec-Fetch-Site": "same-origin",
        "X-Same-Domain": "1",
    })
    return s


def csrf_header(session: requests.Session) -> dict[str, str]:
    """
    Read the current CSRFT759-S cookie value and return it as the
    X-CSRFT759 header dict. Airlock uses double-submit-cookie CSRF:
    the cookie must be echoed in the header on every state-changing
    POST.
    """
    for c in session.cookies:
        if c.name == "CSRFT759-S":
            return {"X-CSRFT759": c.value}
    raise LoginError(
        "CSRFT759-S cookie missing; did the initial GET succeed?",
    )


def mask_phone(phone: str) -> str:
    """
    Mask a phone number for display. Keeps country-code prefix +
    one operator digit + the last two digits, redacts the middle.
    "+41NXXXXXXNN" -> "+41N······NN".  Short / unexpected values
    are returned with the suffix shown only.
    """
    if not phone:
        return phone
    # Strip spaces so the slicing is deterministic regardless of
    # whether Airlock returns "+41 79 ..." or "+4179...".
    cleaned = phone.replace(" ", "")
    if len(cleaned) <= 6:
        # Too short to mask meaningfully — show only last 2.
        return "·" * max(0, len(cleaned) - 2) + cleaned[-2:]
    prefix = cleaned[:4]   # e.g. "+417"
    suffix = cleaned[-2:]
    return f"{prefix}{'·' * (len(cleaned) - 6)}{suffix}"


# ----------------------------------------------------------------------
# Flow steps
# ----------------------------------------------------------------------

def step_prime_cookies(session: requests.Session) -> None:
    logger.info("priming cookies via login page GET")
    resp = session.get(LOGIN_PAGE, timeout=30)
    resp.raise_for_status()
    have = {c.name for c in session.cookies}
    missing = {"AL_SESS-S", "CSRFT759-S"} - have
    if missing:
        raise LoginError(
            f"login page did not set the expected cookies; missing: {missing}",
        )


def step_b2c_access(session: requests.Session) -> dict[str, Any]:
    """
    POST with no body. Expected: 401 with a JSON:API envelope
    declaring the next step (PASSWORD_CHECK / similar). Captures
    the challenge metadata even though we don't strictly need to
    thread it back.
    """
    logger.info("POST /b2c/access (probe)")
    resp = session.post(
        B2C_ACCESS,
        headers={
            **csrf_header(session),
            "Content-Type": "application/json",
        },
        data=b"",
        timeout=30,
    )
    if resp.status_code != 401:
        raise LoginError(
            f"/b2c/access expected 401, got {resp.status_code}: "
            f"{resp.text[:200]!r}",
        )
    try:
        return resp.json()
    except ValueError:
        return {}


def step_password_check(
    session: requests.Session, username: str, password: str,
) -> dict[str, Any]:
    """
    POST {"username": ..., "password": ...}. Expected: 200 with
    `data.attributes.{nextAuthStep, phoneNumber, resendPossible}`
    and Airlock sends the mTAN to the registered number.
    """
    logger.info("POST /password/check")
    resp = session.post(
        PASSWORD_CHECK,
        headers={
            **csrf_header(session),
            "Content-Type": "application/json",
            "X-Continue-Flow": "true",
        },
        json={"username": username, "password": password},
        timeout=30,
    )
    if resp.status_code != 200:
        # Don't include resp.text in the exception — Airlock may
        # echo the username back, and we don't want it in a stack
        # trace destined for stderr.
        raise LoginError(
            f"/password/check expected 200, got {resp.status_code}",
        )
    try:
        body = resp.json()
    except ValueError:
        body = {}
    attrs = (body.get("data") or {}).get("attributes") or {}
    return {
        "next_step": attrs.get("nextAuthStep"),
        "phone_number": attrs.get("phoneNumber"),
        "resend_possible": attrs.get("resendPossible"),
    }


def step_mtan_check(
    session: requests.Session, otp: str,
) -> None:
    """
    POST {"otp": ...}. On 200 the session is promoted. On 401 /
    422 (TBC) the OTP was wrong — caller retries.
    """
    logger.info("POST /mtan/otp/check")
    resp = session.post(
        MTAN_CHECK,
        headers={
            **csrf_header(session),
            "Content-Type": "application/json",
            "X-Continue-Flow": "true",
        },
        json={"otp": otp},
        timeout=30,
    )
    if resp.status_code != 200:
        raise LoginError(
            f"/mtan/otp/check expected 200, got {resp.status_code}",
        )


def step_probe(session: requests.Session) -> bool:
    """
    GET the protected landmark. 200 → authenticated, anything
    else → session not authenticated (most likely 401 or a
    redirect back to the login page).
    """
    logger.info("probe %s", PROBE)
    try:
        resp = session.get(PROBE, timeout=15, allow_redirects=False)
    except RequestException as exc:
        logger.warning("probe failed: %s", exc)
        return False
    return resp.status_code == 200


# ----------------------------------------------------------------------
# Public entry points
# ----------------------------------------------------------------------

def do_check(state_path: Path) -> int:
    state = load_state(state_path)
    if state is None:
        print("MISSING", flush=True)
        return 1
    session = new_session()
    state_into_jar(state.get("cookies", []), session.cookies)
    minted = state.get("minted_at", "?")
    cookie_count = len(state.get("cookies", []))
    logger.info(
        "loaded state: %d cookies, minted_at=%s",
        cookie_count, minted,
    )
    if step_probe(session):
        print("ALIVE", flush=True)
        return 0
    print("DEAD", flush=True)
    return 2


def do_login(args: argparse.Namespace) -> int:
    username = os.environ.get("RELEVATE_LOGIN")
    password = os.environ.get("RELEVATE_PASSWORD")
    if not username or not password:
        print(
            textwrap.dedent("""\
            login: RELEVATE_LOGIN / RELEVATE_PASSWORD not in env.
            The wrapper sources ~/.secrets/relevate.env before
            running this container. Put two lines there:

                export RELEVATE_LOGIN='your-OASI-or-email'
                export RELEVATE_PASSWORD='your-password'

            and chmod 600 the file.
            """),
            file=sys.stderr,
        )
        return 64

    session = new_session()

    try:
        step_prime_cookies(session)
        step_b2c_access(session)
        pc = step_password_check(session, username, password)
    except LoginError as exc:
        logger.error("auth failed before mTAN: %s", exc)
        return 1
    except RequestException as exc:
        logger.error("network failure before mTAN: %s", exc)
        return 1

    next_step = pc.get("next_step")
    phone = pc.get("phone_number")
    if next_step is None:
        logger.warning(
            "password/check did not return nextAuthStep; "
            "proceeding to mTAN anyway",
        )

    # Surface the phone number so the operator knows which device
    # to look at. Airlock returns the FULL number (unmasked), so
    # mask it here before display — the operator already knows
    # their own number, and a partially-redacted version is enough
    # to confirm "yes, that's the right phone" without leaking
    # digits if the log is shared.
    if phone:
        print(
            f"login: mTAN sent to {mask_phone(phone)}.  next_step={next_step}",
            file=sys.stderr,
        )
    else:
        print(
            "login: credentials accepted; awaiting mTAN.",
            file=sys.stderr,
        )

    # Loop the OTP prompt so a fat-finger doesn't burn the session.
    # The wrapper's wider 1-hour expectation is honoured by the
    # operator simply taking their time at this prompt. Typing
    # "resend" (or "r") re-runs /password/check to ask Airlock for
    # a fresh mTAN — necessary when the first SMS doesn't arrive
    # or when the prior code has timed out. Resends don't count
    # against --max-otp-attempts.
    max_attempts = args.max_otp_attempts
    attempt = 0
    while True:
        attempt += 1
        if attempt > max_attempts:
            logger.error("max OTP attempts reached; giving up")
            return 1
        try:
            otp = input(
                f"Relevate mTAN [{attempt}/{max_attempts}] "
                "(or 'resend' for a fresh code): ",
            ).strip()
        except EOFError:
            logger.error("stdin closed before OTP entered")
            return 1
        except KeyboardInterrupt:
            print("", file=sys.stderr)
            logger.error("aborted by user")
            return 130

        if otp.lower() in ("resend", "r"):
            try:
                step_password_check(session, username, password)
                print(
                    "login: fresh mTAN requested; check your phone.",
                    file=sys.stderr,
                )
            except LoginError as exc:
                logger.error("resend failed: %s", exc)
            attempt -= 1
            continue

        if not otp:
            logger.warning("empty OTP — try again, or type 'resend'")
            attempt -= 1
            continue

        try:
            step_mtan_check(session, otp)
            break
        except LoginError as exc:
            logger.warning("OTP rejected: %s", exc)
            print(
                "login: that OTP didn't work. Type the latest code "
                "from your phone, or 'resend' for a new one.",
                file=sys.stderr,
            )

    if not step_probe(session):
        logger.error(
            "post-mTAN landmark probe failed; session not "
            "actually authenticated. Bug?",
        )
        return 1

    save_state(args.state_path, session.cookies)
    print("login: session established + persisted.", file=sys.stderr)
    return 0


# ----------------------------------------------------------------------
# CLI
# ----------------------------------------------------------------------

def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description="REST auth for Relevate.",
    )
    p.add_argument(
        "--state-path",
        type=Path,
        default=DEFAULT_STATE_PATH,
        help=(
            "Persistent state file (default: %(default)s, mounted "
            "from ~/.secrets/relevate-state.json on the host)."
        ),
    )
    p.add_argument(
        "--check",
        action="store_true",
        help=(
            "Probe the existing session without credential submit "
            "or mTAN. Prints ALIVE / DEAD / MISSING."
        ),
    )
    p.add_argument(
        "--max-otp-attempts",
        type=int,
        default=3,
        help=(
            "Maximum OTP submissions before giving up "
            "(default: 3). 'resend' inputs and empty prompts "
            "don't count against this."
        ),
    )
    p.add_argument(
        "-v", "--verbose",
        action="store_true",
        help="DEBUG-level logging.",
    )
    return p.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(sys.argv[1:] if argv is None else argv)
    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )
    if args.check:
        return do_check(args.state_path)
    return do_login(args)


if __name__ == "__main__":
    raise SystemExit(main())
