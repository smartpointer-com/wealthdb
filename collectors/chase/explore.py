#!/usr/bin/env python3
"""chase discovery harness.

Launches Camoufox in the container's Xvfb display on the Chase retail UI
and records the session driven over VNC — the HAR, the network and click
logs, downloads, and a DOM snapshot of every distinct screen (the
artefacts are described in collectorkit.explore) — so login.py and
download.py can be written from real traces. Chase's login form and MDS
2FA challenge live in a cross-origin iframe whose clicks never reach the
top-document listener, so selectors are pinned from the DOM snapshots, not
the clicks.

With `CHASE_USERNAME` / `CHASE_PASSWORD` set (sourced from
`/secrets/chase.env`) the login form is pre-filled; sign-in and 2FA are
still driven by hand. `--no-prefill` skips the fill.

The session should walk the three discovery flows DESIGN.md §3 details:
the login (2FA challenge + any "remember this device" control), the
statements/documents area (how monthly statement PDFs are listed and
fetched), and each account's full transaction history plus every export
format offered.

This is read-only observation. Per AGENTS.md, never click a Pay /
Transfer / Zelle / Send / Deposit / confirm control, stay out of card
management, settings, and the secure message center, and keep to the
retail deposit and credit-card *read* surfaces — never any investment
console the login may also expose.

Recording stops when the last browser window is closed or after
`--max-duration` (default 1h) as a safety net. Artefacts land under
`/debug/<UTC-ts>/` so the bronze + silver tree under `/data` stays clean.
"""
from __future__ import annotations

import argparse
import contextlib
import logging
import re
import sys
from pathlib import Path

from collectorkit import cli, debugcap, explore

log = logging.getLogger("chase.explore")

# secure.chase.com is the standalone sign-in host — it serves the logon
# form in the MAIN frame. The www.chase.com homepage embeds the same form
# in an iframe served from a secure*.chase.com subdomain; the frame-aware
# pre-fill below handles that shape too, so --url can point either way if
# explore reveals a different entry.
DEFAULT_URL = "https://secure.chase.com"
DEFAULT_PROFILE_DIR = Path("/secrets/chase-profile")
DEFAULT_ENV_FILE = Path("/secrets/chase.env")
USER_ENV = "CHASE_USERNAME"
PASS_ENV = "CHASE_PASSWORD"
EVENT_PREFIX = "__CHASE_EVENT__ "

# Pre-fill is restricted to chase.com (and subdomains: secure.chase.com,
# the numbered secure*.chase.com iframe hosts, …) so credentials never
# leak into an embedded third-party frame.
HOST_RE = re.compile(r"(^|\.)chase\.com$", re.I)

# Locators tried in order. Deliberately broad: this harness has to find a
# form it may never have seen, so they anchor on standard HTML conventions
# rather than the ids DESIGN.md §A pins — and login.py reuses this fill for
# the same reason. The Chase sign-in is username-keyed (not an email), hence
# the name/id substring hooks lead and the email conventions trail as defence.
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
# detectors are host-gated to chase.com frames; init scripts run in every
# frame, so the homepage's sign-in iframe is covered too. The Python side
# fills only when username and password co-exist in one frame, which keeps
# credentials out of a lone field like the 2FA code entry.
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
    """Fill a Chase login form at most once per page
    (collectorkit.explore.prefill_login on the chase.com frames: the
    standalone logon host carries the form in the main frame, the
    www.chase.com homepage in a secure*.chase.com iframe). Never submits:
    explore leaves Sign in + 2FA to the VNC session, and login.py, which
    reuses this fill, submits separately."""
    return explore.prefill_login(page, username, password, filled,
                                 host_re=HOST_RE, user_selector=USER_SELECTOR,
                                 log=log)


# What the person at the VNC session is asked to walk (DESIGN.md §3).
WALK = ("walk the three discovery flows (DESIGN.md §3): 1) log in via VNC, "
        "noting the 2FA factor offered and any 'remember this device' "
        "control; 2) open the statements/documents area and fetch at least "
        "one statement PDF; 3) open each account's full transaction history "
        "and run every export format offered (CSV/QFX/QIF/QBO).")


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)
    explore.prepare_profile(args.profile_dir, fresh=args.fresh, log=log,
                            note="the full 2FA challenge will fire on next "
                                 "login")
    username, password, prefill = explore.load_credentials(
        args.env_file, (USER_ENV,), (PASS_ENV,), no_prefill=args.no_prefill,
        host="chase.com", log=log)
    filled: set = set()

    with contextlib.ExitStack() as stack:
        session = explore.Session.from_args(
            stack, args, redact=debugcap.secret_redactor(username, password),
            log=log, observe=HOST_RE, label="chase")
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
