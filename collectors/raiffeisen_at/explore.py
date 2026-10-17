#!/usr/bin/env python3
"""raiffeisen_at discovery harness.

Launches Camoufox in the container's Xvfb display on Mein ELBA (the
Austrian Raiffeisen retail e-banking portal) and records the session
driven over VNC — the HAR, the network and click logs, downloads, and a
DOM snapshot of every distinct screen (the artefacts are described in
collectorkit.explore) — so login.py and download.py can be written from
real traces. A login form or 2FA challenge in a cross-origin iframe never
reaches the top-document click listener — and a server-rendered data path
leaves the DOM snapshots as the only record selectors can be pinned from.

The pre-fill hooks are wired but stay idle here (DESIGN.md §2): the
Verfüger field has no name hook and takes a region-prefixed value, so the
credentials are typed by hand. `--no-prefill` skips the hooks.

The session should walk the three discovery flows DESIGN.md §3 details:
the login (the pushTAN screen and its completion), the document archive
(Dokumente: its listing, one PDF download, and its reach), and each
account's Umsätze view (its paging and date filters; the CSV export may be
run once for comparison).

This is read-only observation. Per AGENTS.md, never click a payment /
transfer / standing-order / confirm control (Überweisung, Auftrag,
Senden, Freigeben, Zeichnen …), stay out of card management, securities,
settings, and the message center, and keep to the retail deposit
surfaces (checking + savings) only.

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

import elba_client as elba

log = logging.getLogger("raiffeisen_at.explore")

DEFAULT_PROFILE_DIR = Path("/secrets/raiffeisen_at-profile")
DEFAULT_ENV_FILE = Path("/secrets/raiffeisen_at.env")
USER_ENV = "RAIFFEISEN_AT_USERNAME"
PASS_ENV = "RAIFFEISEN_AT_PASSWORD"
EVENT_PREFIX = "__RAIFFEISEN_AT_EVENT__ "

# Pre-fill is restricted to raiffeisen.at (and subdomains — the entry
# point mein.elba.raiffeisen.at and the sso.raiffeisen.at login app are
# both under it) so credentials never leak into an embedded third-party
# frame. A login form on any other host is typed by hand; the gate is
# never widened pre-emptively.
HOST_RE = re.compile(r"(^|\.)raiffeisen\.at$", re.I)

# Locators tried in order. Deliberately broad, so the harness can record a
# login form whose shape is not pinned: they anchor on standard HTML
# conventions plus German attribute hooks (`verfueger` / `benutzer`
# id/name substrings lead, the generic/email conventions trail as
# defence). login.py drives the mapped form with its own selectors
# (elba_client). Display-text selectors are avoided throughout (German
# UI; ids/roles only).
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
PWD_SELECTOR = "input[type='password']"

# A one-time-code field: the usual hooks plus a TAN field's `tan` name.
# The expected 2FA is pushTAN (approve in the app, nothing to type), so
# this likely never fires on the happy path. It records any FALLBACK
# factor (a code entry, an SMS-TAN) the challenge screen may offer.
OTP_SELECTOR = (
    "input[autocomplete='one-time-code'], input[inputmode='numeric'], "
    "input[name*='otp' i], input[name*='code' i], input[id*='otp' i], "
    "input[id*='code' i], input[name*='tan' i], input[id*='tan' i], "
    "input[maxlength='6'], input[maxlength='8']"
)

# The in-page detector: the click recorder, a login-form signal that
# triggers the pre-fill, and the code field's static descriptor. Init
# scripts run in every frame, so an embedded sign-in iframe is covered
# too. Host-gated to raiffeisen.at frames. There is no path gate: the
# pre-fill fills only when username and password fields co-exist in one
# frame, which keeps credentials out of lone fields.
CLICK_RECORDER_JS = explore.click_recorder_js(
    EVENT_PREFIX, host_re=HOST_RE, user_selector=USER_SELECTOR,
    password_selector=PWD_SELECTOR, otp_selector=OTP_SELECTOR)


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    explore.add_args(p, url=elba.START_URL, env_file=DEFAULT_ENV_FILE,
                     profile_dir=DEFAULT_PROFILE_DIR, dom_snapshots=True)
    return p.parse_args(argv)


def _prefill(page, username: str, password: str, filled: set) -> bool:
    """Pre-fill the Mein ELBA login form, each field at most once per page.

    Frame-aware: the form may sit in the main frame or in a same-brand
    iframe. Both fields must co-exist in one frame, so a TWO-STEP login
    (identifier first, password later) leaves the pre-fill idle; the
    credentials are then typed by hand. Never submits: sign-in and 2FA are
    driven by hand in the VNC session.
    """
    return explore.prefill_login(
        page, username, password, filled, host_re=HOST_RE,
        user_selector=USER_SELECTOR, password_selector=PWD_SELECTOR, log=log)


# What the person at the VNC session is asked to walk (DESIGN.md §3).
WALK = ("walk the three discovery flows (DESIGN.md §3): 1) log in via VNC, "
        "noting the pushTAN screen and its completion; 2) open the document "
        "archive (Dokumente): its listing, one PDF download, and its reach; "
        "3) open each account's Umsätze view, its paging and date filters; "
        "the CSV export may be run once for comparison.")


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)
    explore.prepare_profile(args.profile_dir, fresh=args.fresh, log=log,
                            note="the full pushTAN challenge will fire on "
                                 "next login")
    username, password, prefill = explore.load_credentials(
        args.env_file, (USER_ENV,), (PASS_ENV,), no_prefill=args.no_prefill,
        host="raiffeisen.at", log=log)
    filled: set = set()

    with contextlib.ExitStack() as stack:
        session = explore.Session.from_args(
            stack, args, redact=debugcap.secret_redactor(username, password),
            log=log, observe=HOST_RE, label="raiffeisen")
        context = session.open_camoufox(stack, args.profile_dir)
        session.attach(
            context, init_js=CLICK_RECORDER_JS, event_prefix=EVENT_PREFIX,
            prefill=(lambda page: _prefill(
                page, username, password, filled)) if prefill else None)
        session.open(args.url)
        session.record(WALK, max_duration=args.max_duration)
    session.report()
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
