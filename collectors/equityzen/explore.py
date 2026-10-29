#!/usr/bin/env python3
"""equityzen discovery harness.

Launches Camoufox in the container's Xvfb display on the EquityZen
investor portal and records the session driven over VNC — the HAR, the
network and click logs, and downloads (the artefacts are described in
collectorkit.explore) — so login.py and download.py can be written from
real traces. EquityZen's backend is Django + GraphQL (the public GitHub
org forks graphene / graphene-django), so the SPA most likely talks to a
single GraphQL POST endpoint keyed by operation name rather than REST
resources; the HAR is the primary artefact for finding it and its response
shapes.

With `EQUITYZEN_USERNAME` / `EQUITYZEN_PASSWORD` set (sourced from
`/secrets/equityzen.env`) the login form is pre-filled; login and the TOTP
code are still driven by hand. `--no-prefill` skips the fill.

Recording stops when the last browser window is closed (Camoufox's
persistent context fires `close`) or after `--max-duration` (default 1h)
as a safety net. Artefacts land under `/debug/<UTC-ts>/` so the bronze +
silver tree under `/data` stays clean.

Read-only: explore is for observation. EquityZen is a live secondary
marketplace — never click an "Invest" / "Place order" / "Express Deal" /
"Sell" / "Accept" / e-sign / confirm control. The only writes allowed are
the login form submit + the TOTP challenge, both driven manually in the
VNC session.
"""
from __future__ import annotations

import argparse
import contextlib
import logging
import re
import sys
from pathlib import Path
from urllib.parse import urlparse

from collectorkit import cli, debugcap, explore

log = logging.getLogger("equityzen.explore")

DEFAULT_URL = "https://equityzen.com/accounts/login/"
DEFAULT_PROFILE_DIR = Path("/secrets/equityzen-profile")
DEFAULT_ENV_FILE = Path("/secrets/equityzen.env")
# The login identifier is an email address. EQUITYZEN_USERNAME is the
# repo-wide convention (every collector uses <SOURCE>_USERNAME);
# EQUITYZEN_EMAIL is accepted as an alias since it reads more naturally
# for an email-keyed login and is what the env file may already use.
# Resolved in order — the first set wins.
USER_ENVS = ("EQUITYZEN_USERNAME", "EQUITYZEN_EMAIL")
PASS_ENV = "EQUITYZEN_PASSWORD"
EVENT_PREFIX = "__EZ_EVENT__ "

# Pre-fill is restricted to this host (and subdomains) so credentials
# never leak into an embedded third-party iframe.
HOST_RE = re.compile(r"(^|\.)equityzen\.com$", re.I)

# Locators for EquityZen's login form, confirmed by a headless DOM probe
# of the (public) login page: the email input is
# `<input type="text" id="email" placeholder="Email" autocomplete="off">`
# — note type=text with NO name attribute, so the id / placeholder are
# the only stable hooks — and the password input is
# `<input type="password" id="password" placeholder="Password">`. The
# EquityZen-specific `#email` / placeholder selectors lead; the standard
# conventions (type=email, autocomplete, name-substring) trail as
# defence against a future form rework.
USER_SELECTOR = (
    "input#email, "
    "input[type='email'], "
    "input[placeholder*='email' i], "
    "input[autocomplete='username'], "
    "input[autocomplete='email'], "
    "input[name*='email' i], "
    "input[name*='user' i], "
    "input[name*='login' i]"
)
PWD_SELECTOR = "input#password, input[type='password']"

# Pre-fill is confined to the login surface. Without this gate the email
# selector can match an unrelated field on the authenticated app (the
# explore trace caught it typing the email into a dashboard input on `/`),
# and a lone 2FA/TOTP step could get the password typed into it.
LOGIN_PATH_RE = re.compile(r"/accounts/|login|sign[-_]?in", re.I)

# The TOTP field, recognised by the usual one-time-code hooks plus
# EquityZen's own `totp` / `2fa` names.
OTP_SELECTOR = (
    "input[autocomplete='one-time-code'], input[inputmode='numeric'], "
    "input[name*='otp' i], input[name*='totp' i], input[name*='2fa' i], "
    "input[name*='code' i], input[id*='otp' i], input[id*='totp' i], "
    "input[id*='code' i], input[maxlength='6']"
)

# The in-page detector: the click recorder, a login-form signal that
# triggers the pre-fill, and the TOTP field's static descriptor (never its
# value) once it mounts, so login.py can target the 2FA input. Gated to
# the login surface of equityzen.com frames, as the pre-fill is.
CLICK_RECORDER_JS = explore.click_recorder_js(
    EVENT_PREFIX, host_re=HOST_RE, path_re=LOGIN_PATH_RE,
    user_selector=USER_SELECTOR, password_selector=PWD_SELECTOR,
    otp_selector=OTP_SELECTOR, otp_kind="totp-field-detected")


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    explore.add_args(p, url=DEFAULT_URL, env_file=DEFAULT_ENV_FILE,
                     profile_dir=DEFAULT_PROFILE_DIR, dom_snapshots=False)
    return p.parse_args(argv)


def _prefill(page, username: str, password: str, filled: set) -> bool:
    """Pre-fill the email and password on EquityZen's login form, each at
    most once per page.

    Gated to the login surface (LOGIN_PATH_RE) on top of the shared
    host gate. Content already in a field is overwritten once: a stray
    autofill must never stack onto the value. Never submits: login and
    the TOTP are driven by hand in the VNC session.
    """
    try:
        path = urlparse(page.url).path or ""
    except Exception:  # noqa: BLE001
        return False
    if not LOGIN_PATH_RE.search(path):
        return False
    return explore.prefill_login(
        page, username, password, filled, host_re=HOST_RE,
        user_selector=USER_SELECTOR, password_selector=PWD_SELECTOR,
        overwrite=True, log=log)


# What the person at the VNC session is asked to do.
WALK = ("log in via VNC, click through the pages the download needs.")


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)
    explore.prepare_profile(args.profile_dir, fresh=args.fresh, log=log,
                            note="TOTP will be required on next login")
    username, password, prefill = explore.load_credentials(
        args.env_file, USER_ENVS, (PASS_ENV,),
        no_prefill=args.no_prefill, host="equityzen.com", log=log)
    filled: set = set()

    with contextlib.ExitStack() as stack:
        session = explore.Session.from_args(
            stack, args, redact=debugcap.secret_redactor(username, password),
            log=log)
        context = session.open_camoufox(stack, args.profile_dir)
        session.attach(
            context, init_js=CLICK_RECORDER_JS, event_prefix=EVENT_PREFIX,
            prefill=(lambda page: _prefill(
                page, username, password, filled)) if prefill else None)
        session.open(args.url)
        session.record(WALK, max_duration=args.max_duration,
                       poll_prefill=False)
    session.report()
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
