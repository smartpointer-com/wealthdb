#!/usr/bin/env python3
"""amex discovery harness.

Launches Camoufox in the container's Xvfb display, navigates to the
American Express card UI, and records every action taken in the VNC
session. login.py + download.py were written from the traces it produced
(DESIGN.md §A–§H), and it is retained for the next question the surface
raises. It records the HAR, the network and click logs, downloads, and a
DOM snapshot of every distinct screen (the artefacts are described in
collectorkit.explore). With `AMEX_USERNAME` / `AMEX_PASSWORD` set (sourced
from `/secrets/amex.env`) the login form is pre-filled; sign-in and the
passcode challenge are still driven by hand.

DESIGN.md §3 records what discovery was pointed at: the login (passcode
challenge + the "Add This Device" step), the card overview and detail,
each card's full transaction history plus every export format offered,
and the statements area — with the device-trust-across-a-browser-restart
probe as a second short run on the same profile.

This is read-only observation. Per AGENTS.md, never click a Pay /
Transfer / Send & Split / confirm control, stay out of card management,
rewards redemption, offers, Plan It, travel, profile/settings, and the
message center, and keep to the card *read* surfaces — never any other
product the login may also expose.

Recording stops when the last browser window is closed (Camoufox's
persistent context fires `close`) or after `--max-duration` (default 1h) as
a safety net. Artefacts land under `/debug/<UTC-ts>/` so the bronze + silver
tree under `/data` stays clean.
"""
from __future__ import annotations

import argparse
import contextlib
import logging
import re
import sys
from pathlib import Path

from collectorkit import cli, debugcap, explore

log = logging.getLogger("amex.explore")

# The sign-in renders on the public site itself — classic light DOM in the
# main frame, off the homepage's `#gnav_login` (DESIGN.md §B). The homepage
# stays the entry because it records the whole redirect chain; --url points
# straight at a deeper surface when a session only needs that one. The
# frame-aware pre-fill below still covers a form in the main frame and one
# in a same-brand iframe alike.
DEFAULT_URL = "https://www.americanexpress.com/"
DEFAULT_PROFILE_DIR = Path("/secrets/amex-profile")
DEFAULT_ENV_FILE = Path("/secrets/amex.env")
USER_ENV = "AMEX_USERNAME"
PASS_ENV = "AMEX_PASSWORD"
EVENT_PREFIX = "__AMEX_EVENT__ "

# Pre-fill is restricted to americanexpress.com (and subdomains) so
# credentials never leak into an embedded third-party frame. The captures
# put the form on the site itself (§B), so the gate holds as written; a form
# that ever renders on another host is typed by hand that run and the gate
# widened to the observed host afterwards — never pre-emptively.
HOST_RE = re.compile(r"(^|\.)americanexpress\.com$", re.I)

# Locators tried in order. Deliberately broad: they anchor on standard HTML
# conventions rather than the ids the captures named (§B), which keeps the
# fill — reused by login.py — working across a rename of the form's ids. The
# sign-in is username-keyed (an email is also accepted), hence the name/id
# substring hooks lead and the email conventions trail as defence.
USER_SELECTOR = (
    "input[autocomplete='username'], "
    "input[name*='user' i], "
    "input[id*='user' i], "
    "input[name*='login' i], "
    "input[id*='login' i], "
    "input[type='email'], "
    "input[autocomplete='email']"
)

# The in-page click recorder plus the login-form and one-time-code detectors
# (collectorkit.explore.click_recorder_js). VNC mouse events bypass
# Playwright's action API, so clicks are logged from the page itself. The
# detectors are host-gated to americanexpress.com frames, with no path gate:
# the form is reached from the homepage's own login control (§B), so no
# single route identifies it. The Python side fills only when username and
# password co-exist in one frame, which keeps credentials out of a lone
# field like the 2FA code entry. Init scripts run in every frame, so a
# sign-in iframe is covered too.
CLICK_RECORDER_JS = explore.click_recorder_js(
    EVENT_PREFIX, host_re=HOST_RE, user_selector=USER_SELECTOR)


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    explore.add_args(p, url=DEFAULT_URL, env_file=DEFAULT_ENV_FILE,
                     profile_dir=DEFAULT_PROFILE_DIR, dom_snapshots=True)
    return p.parse_args(argv)


def _maybe_prefill_login(page, username: str, password: str,
                         filled: set, *, overwrite: bool = False) -> bool:
    """Fill an Amex login form at most once per page
    (collectorkit.explore.prefill_login on the americanexpress.com frames).

    `overwrite` decides what happens to a field that ALREADY has content.
    Explore leaves it alone (the default): the human may have typed it, and
    fighting that is worse than skipping. login.py sets it, because on a
    trusted device Amex pre-fills a MASKED user id (DESIGN.md §G) —
    submitting that works only while the device trust holds, so the real
    `AMEX_USERNAME` is written over it deliberately rather than inherited.

    Never submits: explore leaves Sign in + 2FA to the VNC session, and
    login.py, which reuses this fill, submits separately."""
    return explore.prefill_login(page, username, password, filled,
                                 host_re=HOST_RE, user_selector=USER_SELECTOR,
                                 overwrite=overwrite, log=log)


# What the person at the VNC session is asked to walk (DESIGN.md §3).
WALK = ("walk the discovery flows (DESIGN.md §3): 1) log in via VNC, noting "
        "the 2FA factor offered and any 'remember this device' control; 2) "
        "open the card overview and one card's detail; 3) open that card's "
        "full transaction history and run EVERY export format offered, each "
        "with an explicit date range; 4) open the statements area and fetch "
        "at least one statement PDF.")


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)
    explore.prepare_profile(args.profile_dir, fresh=args.fresh, log=log,
                            note="the full 2FA challenge will fire on next "
                                 "login")
    username, password, prefill = explore.load_credentials(
        args.env_file, (USER_ENV,), (PASS_ENV,),
        no_prefill=args.no_prefill, host="americanexpress.com", log=log)
    filled: set = set()

    with contextlib.ExitStack() as stack:
        session = explore.Session.from_args(
            stack, args, redact=debugcap.secret_redactor(username, password),
            log=log, observe=HOST_RE, label="americanexpress")
        context = session.open_camoufox(stack, args.profile_dir)
        session.attach(
            context, init_js=CLICK_RECORDER_JS, event_prefix=EVENT_PREFIX,
            prefill=(lambda page: _maybe_prefill_login(
                page, username, password, filled)) if prefill else None)
        session.open(args.url)
        session.record(WALK, max_duration=args.max_duration)
    session.report()
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
