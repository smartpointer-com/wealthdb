#!/usr/bin/env python3
"""fidelity-web discovery harness — Donor-Advised Fund (Fidelity
Charitable) surface.

Launches Camoufox in the container's Xvfb display on the standard
Fidelity signin page and records every action taken in the VNC session,
so DAF surfaces can be mapped from real traces. The DAF sits behind a
different web UI, reached by an SSO hop from the retail session into
the charitable SPA (DESIGN.md §12); the `download` walk's `daf` phase
drives its JSON API, and this harness maps a surface before it joins
that phase's allow-list (AGENTS.md §1a).

It records the HAR, the network and click logs, downloads, and a DOM
snapshot of every distinct screen of each Fidelity-family frame (the
artefacts are described in collectorkit.explore); selectors are pinned
from the snapshots, not the click log. With `FIDELITY_WEB_USERNAME` /
`FIDELITY_WEB_PASSWORD` set (sourced from `/secrets/fidelity-web.env`) the
login form is pre-filled; sign-in and 2FA are still driven by hand.

The session should walk the DAF discovery flows DESIGN.md §12 details:
sign in on the standard Fidelity form, open the DAF from the account
selector (recording the SSO hop), then tour the read-only surfaces —
balances, investment pools/positions, grant history, contribution
history, statements/confirmations — exercising every export control
offered. This is read-only observation. Per AGENTS.md, never click
Grant, Contribute, Exchange, or any other submit control: on a DAF,
"recommend a grant" and "contribute" MOVE REAL MONEY, and pool
exchanges reallocate investments.

Recording stops when the last browser window is closed (Camoufox's
persistent context fires `close`) or after `--max-duration` (default
1h) as a safety net. Artefacts land under `/debug/<UTC-ts>/` so the
bronze + silver tree under `/data` stays clean.
"""
from __future__ import annotations

import argparse
import contextlib
import logging
import re
import sys
from pathlib import Path

from collectorkit import cli, debugcap, explore

log = logging.getLogger("fidelity-web.explore")

# The standard signin — the same entry download.py drives. The DAF hop
# is taken from the post-auth session, so recording starts on known
# ground and captures the full redirect chain from there. --url can
# point straight at the DAF portal instead.
DEFAULT_URL = "https://digital.fidelity.com/prgw/digital/signin/"
# The download.py profile dir, on purpose: the Akamai trust + Fidelity
# device-trust cookies it holds are what let a scripted signin through,
# so an explore run costs at most a 2FA prompt, not a fresh VNC seed.
DEFAULT_PROFILE_DIR = Path("/secrets/fidelity-web-profile")
DEFAULT_ENV_FILE = Path("/secrets/fidelity-web.env")
USERNAME_ENVS = ("FIDELITY_WEB_USERNAME", "FIDELITY_USERNAME")
PASSWORD_ENVS = ("FIDELITY_WEB_PASSWORD", "FIDELITY_PASSWORD")
EVENT_PREFIX = "__FIDELITY_EVENT__ "

# DOM/network observation covers the whole Fidelity family: the retail
# hosts (digital.fidelity.com, digitalservices.fidelity.com, …) plus
# the Fidelity Charitable public/portal hosts, wherever the SSO chain
# lands. The gate is deliberately wide so a surface on a new host is
# captured rather than filtered out.
OBSERVE_HOST_RE = re.compile(
    r"(^|\.)(fidelity\.com|fidelitycharitable\.(org|com))$", re.I)

# Credential pre-fill is gated to fidelity.com proper — the signin form
# download.py already drives. If the DAF portal presents its own
# credential form on another host, the credentials are typed by hand
# that run and the gate is widened to the observed host afterwards —
# never pre-emptively.
PREFILL_HOST_RE = re.compile(r"(^|\.)fidelity\.com$", re.I)

# The known signin form (DESIGN.md §8.2): password always at
# #dom-pswd-input; the username is a text input on fresh devices and a
# remember-my-username <select> on returning ones. Generic conventions
# trail as defence in case the form drifts.
USER_SELECTOR = (
    "input#dom-username-input, "
    "input[autocomplete~=username], "
    "input#userId-input, "
    "input[name='userId'], "
    "input[aria-labelledby='dom-username-label'], "
    "input[name*='user' i], "
    "input[id*='user' i]"
)
USER_SELECT_SELECTOR = "select#dom-select-username"
PWD_SELECTOR = "#dom-pswd-input, input[type='password']"

# The in-page click recorder and login-form / one-time-code detectors
# (collectorkit.explore.click_recorder_js), host-gated to fidelity.com
# frames like the pre-fill. The username detector also matches the
# remember-my-username <select>; the code detector puts the known 2FA
# field ahead of the generic conventions.
OTP_SELECTOR = ("#dom-totp-security-code-input, "
                + explore.DEFAULT_OTP_SELECTOR)
CLICK_RECORDER_JS = explore.click_recorder_js(
    EVENT_PREFIX, host_re=PREFILL_HOST_RE,
    user_selector=f"{USER_SELECTOR}, {USER_SELECT_SELECTOR}",
    password_selector=PWD_SELECTOR, otp_selector=OTP_SELECTOR)


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    explore.add_args(p, url=DEFAULT_URL, env_file=DEFAULT_ENV_FILE,
                     profile_dir=DEFAULT_PROFILE_DIR, dom_snapshots=True)
    return p.parse_args(argv)


# What the person at the VNC session is asked to walk (DESIGN.md §12).
WALK = ("walk the DAF discovery flows (DESIGN.md §12): 1) sign in on the "
        "standard Fidelity form (2FA by hand); 2) open the Fidelity "
        "Charitable / Giving account from the account selector and let the "
        "SSO hop complete; 3) tour the read-only DAF surfaces — balances, "
        "investment pools/positions, grant history, contribution history, "
        "statements/confirmations — and run every export/download control "
        "offered. NEVER click Grant, Contribute, Exchange, or any submit "
        "control (they move real money).")


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)
    explore.prepare_profile(args.profile_dir, fresh=args.fresh, log=log,
                            note="Akamai trust + device trust are discarded; "
                                 "complete the login fully by hand over VNC "
                                 "to re-seed both")
    username, password, prefill = explore.load_credentials(
        args.env_file, USERNAME_ENVS, PASSWORD_ENVS,
        no_prefill=args.no_prefill, host="fidelity.com", log=log)
    filled: set = set()

    with contextlib.ExitStack() as stack:
        session = explore.Session.from_args(
            stack, args, redact=debugcap.secret_redactor(username, password),
            log=log, observe=OBSERVE_HOST_RE, label="fidelity")
        context = session.open_camoufox(stack, args.profile_dir)
        session.attach(
            context, init_js=CLICK_RECORDER_JS, event_prefix=EVENT_PREFIX,
            prefill=(lambda page: explore.prefill_login(
                page, username, password, filled, host_re=PREFILL_HOST_RE,
                user_selector=USER_SELECTOR, password_selector=PWD_SELECTOR,
                user_alt_selector=USER_SELECT_SELECTOR,
                log=log)) if prefill else None)
        session.open(args.url)
        session.record(WALK, max_duration=args.max_duration)
    session.report()
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
