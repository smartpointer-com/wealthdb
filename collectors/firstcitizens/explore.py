#!/usr/bin/env python3
"""firstcitizens discovery harness.

Launches Camoufox in the container's Xvfb display on the First Citizens
retail UI and records the session driven over VNC — the HAR, the network
and click logs, downloads, and a DOM snapshot of every distinct screen
(the artefacts are described in collectorkit.explore) — so login.py and
download.py can be written from real traces. A login form or 2FA challenge
in a cross-origin iframe never reaches the top-document click listener, so
selectors are pinned from the DOM snapshots, not the clicks.

With `FIRSTCITIZENS_USERNAME` / `FIRSTCITIZENS_PASSWORD` set (sourced from
`/secrets/firstcitizens.env`) the login form is pre-filled; sign-in and
2FA are still driven by hand. `--no-prefill` skips the fill.

The session should walk the three discovery flows DESIGN.md §3 details:
the login (2FA challenge + any "remember this device" control), the
statements/documents area (how monthly statement PDFs are listed and
fetched), and each account's full transaction history plus every export
format offered.

This is read-only observation. Per AGENTS.md, never click a Pay /
Transfer / Zelle / Send / Deposit / confirm control, stay out of card
management, settings, and the secure message center, and keep to the
retail checking + savings surfaces — never any wealth-management, trust,
or brokerage console the login may also expose.

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

import q2client

log = logging.getLogger("firstcitizens.explore")

# The public homepage, whose modal carries the sign-in form; starting there
# records the whole redirect chain into the digital-banking app. --url can
# point straight at another entry.
DEFAULT_URL = q2client.START_URL
DEFAULT_PROFILE_DIR = Path("/secrets/firstcitizens-profile")
DEFAULT_ENV_FILE = Path("/secrets/firstcitizens.env")
USER_ENV = "FIRSTCITIZENS_USERNAME"
PASS_ENV = "FIRSTCITIZENS_PASSWORD"
EVENT_PREFIX = "__FIRSTCITIZENS_EVENT__ "

# Pre-fill is restricted to firstcitizens.com (and subdomains) so
# credentials never leak into an embedded third-party frame. A login form
# rendering on another host gets its credentials typed by hand; the gate is
# widened to a host only after a capture shows the form there — never
# pre-emptively.
HOST_RE = re.compile(r"(^|\.)firstcitizens\.com$", re.I)

# Locators tried in order. Deliberately broad: they anchor on standard HTML
# conventions rather than any one known id, so a reworked form still
# pre-fills. US retail sign-ins are username-keyed (not an email), hence the
# name/id substring hooks lead and the email conventions trail as defence.
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
# detectors are host-gated to firstcitizens.com frames; init scripts run in
# every frame, so an embedded sign-in iframe is covered too. The Python side
# fills only when username and password co-exist in one frame, which keeps
# credentials out of a lone field.
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
                         filled: set) -> bool:
    """Fill a First Citizens login form at most once per page
    (collectorkit.explore.prefill_login on the firstcitizens.com frames).
    Never submits: that is login.py's step, or the human's in an explore
    session."""
    return explore.prefill_login(page, username, password, filled,
                                 host_re=HOST_RE, user_selector=USER_SELECTOR,
                                 log=log)


# What the person at the VNC session is asked to walk (DESIGN.md §3).
WALK = ("walk the three discovery flows (DESIGN.md §3): 1) log in via VNC, "
        "noting the 2FA factor offered and any 'remember this device' "
        "control; 2) open the statements/documents area and fetch at least "
        "one statement PDF; 3) open each account's full transaction history "
        "and run every export format offered (CSV/QFX/OFX).")


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)
    explore.prepare_profile(args.profile_dir, fresh=args.fresh, log=log,
                            note="the full 2FA challenge will fire on next "
                                 "login")
    username, password, prefill = explore.load_credentials(
        args.env_file, (USER_ENV,), (PASS_ENV,), no_prefill=args.no_prefill,
        host="firstcitizens.com", log=log)
    filled: set = set()

    with contextlib.ExitStack() as stack:
        session = explore.Session.from_args(
            stack, args, redact=debugcap.secret_redactor(username, password),
            log=log, observe=HOST_RE, label="firstcitizens")
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
