#!/usr/bin/env python3
"""erstebank_at discovery harness.

Launches Camoufox in the container's Xvfb display on George (the retail
e-banking portal of Erste Bank und Sparkassen in Austria) and records the
session driven over VNC — the HAR, the network and click logs, response
bodies, downloads, and a DOM snapshot of every distinct screen (the
artefacts are described in collectorkit.explore) — so login.py and
download.py can be written from real traces.

The sign-in is typed by hand (DESIGN.md §2). The George login form is
expected to ask for a user name only, with the s Identity push completing
the sign-in, and the shared pre-fill fills only a username + password pair
in one frame. The user name is still read from the env file: it feeds the
redactor, so no capture carries it.

The session should walk the discovery flows DESIGN.md §3 details: the
sign-in (the s Identity challenge and its completion), the checking
accounts (transaction history, its paging and filters, each export format
once), the statement archive (its listing, one PDF, its reach), and the
credit cards (where their data is served from).

This is read-only observation. Per AGENTS.md, never click a payment /
transfer / standing-order / confirm control (Überweisung, Auftrag,
Senden, Zeichnen, Freigeben …), stay out of card management, savings,
securities, settings, plugins and the Postfach, and keep to the checking
account and credit card surfaces only.

Recording stops when the last browser window is closed (Camoufox's
persistent context fires `close`) or after `--max-duration` (default 1h) as
a safety net. Artefacts land under `/debug/<UTC-ts>/` so the bronze + silver
tree under `/data` stays clean.
"""
from __future__ import annotations

import argparse
import contextlib
import logging
import os
import re
import sys
from pathlib import Path

from collectorkit import cli, debugcap, envfile, explore

log = logging.getLogger("erstebank_at.explore")

START_URL = "https://george.sparkasse.at/"
DEFAULT_PROFILE_DIR = Path("/secrets/erstebank_at-profile")
DEFAULT_ENV_FILE = Path("/secrets/erstebank_at.env")
USER_ENV = "ERSTEBANK_AT_USERNAME"
EVENT_PREFIX = "__ERSTEBANK_AT_EVENT__ "

# The DOM snapshots and the in-page detector are restricted to sparkasse.at
# (and subdomains — the entry point george.sparkasse.at and, expected, its
# login host are under it). A frame on any other host is still in the HAR,
# but no DOM of it is written. The gate is widened only when a capture
# shows a load-bearing frame elsewhere, never pre-emptively.
HOST_RE = re.compile(r"(^|\.)sparkasse\.at$", re.I)

# Locators tried in order. Deliberately broad, so the detector reports a
# login form whose shape is not pinned: they anchor on standard HTML
# conventions plus German attribute hooks (`verfueger` / `benutzer`
# id/name substrings), with the generic conventions trailing as defence.
# Display-text selectors are avoided throughout (German UI; ids/roles
# only).
USER_SELECTOR = (
    "input[autocomplete='username'], "
    "input[name*='verfueger' i], "
    "input[id*='verfueger' i], "
    "input[name*='benutzer' i], "
    "input[id*='benutzer' i], "
    "input[name*='user' i], "
    "input[id*='user' i], "
    "input[name*='login' i], "
    "input[id*='login' i], "
    "input[type='email'], "
    "input[autocomplete='email']"
)
# Kept so a password field, if the form has one after all, is reported.
PWD_SELECTOR = "input[type='password']"

# A one-time-code field: the usual hooks plus a TAN field's `tan` name. The
# expected factor is an s Identity push (approve in the app, nothing to
# type), so this records any FALLBACK factor the challenge screen offers.
OTP_SELECTOR = (
    "input[autocomplete='one-time-code'], input[inputmode='numeric'], "
    "input[name*='otp' i], input[name*='code' i], input[id*='otp' i], "
    "input[id*='code' i], input[name*='tan' i], input[id*='tan' i], "
    "input[maxlength='6'], input[maxlength='8']"
)

# The in-page detector: the click recorder, a login-form signal and the
# code field's static descriptor. Init scripts run in every frame, so an
# embedded sign-in iframe is covered too. Host-gated to sparkasse.at
# frames.
CLICK_RECORDER_JS = explore.click_recorder_js(
    EVENT_PREFIX, host_re=HOST_RE, user_selector=USER_SELECTOR,
    password_selector=PWD_SELECTOR, otp_selector=OTP_SELECTOR)


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    explore.add_args(
        p, url=START_URL, env_file=DEFAULT_ENV_FILE,
        profile_dir=DEFAULT_PROFILE_DIR, dom_snapshots=True,
        env_help=("Bash-sourced env file supplying the user name the "
                  "captures are masked against. Skipped when absent. "
                  "Default: %(default)s."))
    return p.parse_args(argv)


def load_username(env_file: Path | None) -> str:
    """The George user name, from the environment or `env_file`.

    It is never typed by the harness; it only builds the redactor. Without
    it the captures can hold the user name in clear, so its absence is
    loud.
    """
    if env_file is not None and envfile.source_env_file(env_file):
        log.info("env file:    %s (sourced)", env_file)
    username = os.environ.get(USER_ENV, "")
    if not username:
        log.warning("%s not set: the captures cannot mask the user name. "
                    "Set it in %s.", USER_ENV, env_file)
    return username


# What the person at the VNC session is asked to walk (DESIGN.md §3).
WALK = ("walk the discovery flows (DESIGN.md §3): 1) sign in via VNC, "
        "noting the s Identity screen, any comparison code, and its "
        "completion; 2) open each checking account's transaction list, "
        "scroll its paging, open one transaction's detail, and run each "
        "export format once; 3) open the statement archive: its listing, "
        "one PDF, its reach; 4) open each credit card's transactions and "
        "statements, noting whether they leave George.")


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)
    if args.no_prefill:
        log.warning("--no-prefill has no effect: the sign-in is always "
                    "typed by hand (DESIGN.md §2)")
    explore.prepare_profile(args.profile_dir, fresh=args.fresh, log=log,
                            note="the next sign-in takes the unknown-device "
                                 "path")
    username = load_username(args.env_file)

    with contextlib.ExitStack() as stack:
        session = explore.Session.from_args(
            stack, args, redact=debugcap.secret_redactor(username),
            log=log, observe=HOST_RE, label="george")
        context = session.open_camoufox(stack, args.profile_dir)
        session.attach(context, init_js=CLICK_RECORDER_JS,
                       event_prefix=EVENT_PREFIX)
        session.open(args.url)
        session.record(WALK, max_duration=args.max_duration)
    session.report()
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
