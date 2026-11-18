#!/usr/bin/env python3
"""cointracking discovery harness.

Launches Camoufox in the container's Xvfb display on cointracking.info and
records the session driven over VNC — the HAR, the network and click logs,
and downloads (the artefacts are described in collectorkit.explore) — so
login.py and download.py can be written from real traces. The HAR is the
primary record of the internal REST endpoints the SPA hits.

With `COINTRACKING_USERNAME` / `COINTRACKING_PASSWORD` set (sourced from
`/secrets/cointracking.env`) the login form is pre-filled; login and 2FA
are still driven by hand. `--no-prefill` skips the fill.

Recording stops when the last browser window is closed
(Camoufox's persistent context fires `close`) or after
`--max-duration` (default 1h) as a safety net. Artefacts land under
`/debug/<UTC-ts>/` so the bronze + silver tree under `/data` stays
clean.
"""
from __future__ import annotations

import argparse
import re
import sys
from pathlib import Path

from collectorkit import explore

DEFAULT_URL = "https://cointracking.info"
DEFAULT_PROFILE_DIR = Path("/secrets/cointracking-profile")
DEFAULT_ENV_FILE = Path("/secrets/cointracking.env")
USER_ENV = "COINTRACKING_USERNAME"
PASS_ENV = "COINTRACKING_PASSWORD"
EVENT_PREFIX = "__CT_EVENT__ "

# Pre-fill is restricted to this host (and subdomains) so credentials
# never leak into an embedded third-party iframe.
HOST_RE = re.compile(r"(^|\.)cointracking\.info$", re.I)

# Locators tried in order. Deliberately broad — this harness runs when
# cointracking.info's form has moved and login.py's concrete selectors
# no longer match, so it anchors on standard HTML conventions
# (input[type=email] / autocomplete=username) plus name-substring
# fallbacks rather than on any one observed id. Same for the password
# input.
USER_SELECTOR = (
    "input[type='email'], "
    "input[autocomplete='username'], "
    "input[autocomplete='email'], "
    "input[name*='email' i], "
    "input[name*='user' i], "
    "input[name*='login' i]"
)
PWD_SELECTOR = "input[type='password']"

# The in-page detector: the click recorder, and a login-form signal that
# triggers the pre-fill. Host-gated, so it never fires on a third-party
# frame. The 2FA field is not watched for.
CLICK_RECORDER_JS = explore.click_recorder_js(
    EVENT_PREFIX, host_re=HOST_RE, user_selector=USER_SELECTOR,
    password_selector=PWD_SELECTOR, otp_selector=None)


# What the person at the VNC session is asked to do.
WALK = ("log in via VNC, click through the pages download scrapes.")


SITE = explore.Site(
    name="cointracking", url=DEFAULT_URL, profile_dir=DEFAULT_PROFILE_DIR,
    env_file=DEFAULT_ENV_FILE, user_envs=(USER_ENV,), pass_envs=(PASS_ENV,),
    host="cointracking.info", host_re=HOST_RE, user_selector=USER_SELECTOR,
    password_selector=PWD_SELECTOR, event_prefix=EVENT_PREFIX,
    init_js=CLICK_RECORDER_JS, walk=WALK,
    fresh_note="2FA will be required on next login",
    dom_snapshots=False, poll_prefill=False)


def parse_args(argv: list[str]) -> argparse.Namespace:
    return explore.parser(SITE, __doc__).parse_args(argv)


def main(argv: list[str]) -> int:
    return explore.main(SITE, argv, __doc__)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
