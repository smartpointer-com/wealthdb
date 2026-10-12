#!/usr/bin/env python3
"""angellist discovery harness.

Launches Camoufox in the container's Xvfb display on the AngelList
Investor Portal and records the session driven over VNC — the HAR, the
network and click logs, and downloads (the artefacts are described in
collectorkit.explore) — so download.py can be written from real traces.
The HAR is the primary record of the internal JSON/XHR endpoints the React
SPA hits.

With `ANGELLIST_USERNAME` / `ANGELLIST_PASSWORD` set (sourced from
`/secrets/angellist.env`) the login form is pre-filled; login and 2FA are
still driven by hand. `--no-prefill` skips the fill. Against an anti-bot
wall, `--cookies` starts from a session carried over from a real browser,
and `--no-recorder` / `--warmup` reduce what the challenge can score.

Re-run this harness whenever AngelList moves its UI or GraphQL
operations: the fresh HAR + trace pinpoint what changed against the
endpoint map in DESIGN.md, so download.py can be updated from real
captures.

Recording stops when the last browser window is closed (Camoufox's
persistent context fires `close`) or after `--max-duration` (default
1h) as a safety net. Artefacts land under `/debug/<UTC-ts>/` so the
bronze + silver tree under `/data` stays clean.

Read-only: this is for observation. Per AGENTS.md, do not click any
mutate/confirm/submit control beyond the login + 2FA forms, and stay
out of any syndicate-lead / fund-admin surface the login may expose.
"""
from __future__ import annotations

import argparse
import contextlib
import json
import logging
import re
import sys
import time
from pathlib import Path

from collectorkit import cli, debugcap, explore

log = logging.getLogger("angellist.explore")

DEFAULT_URL = "https://venture.angellist.com/v/login"
DEFAULT_PROFILE_DIR = Path("/secrets/angellist-profile")
DEFAULT_ENV_FILE = Path("/secrets/angellist.env")
USER_ENV = "ANGELLIST_USERNAME"
PASS_ENV = "ANGELLIST_PASSWORD"
EVENT_PREFIX = "__AL_EVENT__ "

# Pre-fill is restricted to this host (and subdomains) so credentials
# never leak into an embedded third-party iframe. angellist.com covers
# the main site and any fund-branded *.angellist.com investor portal; a
# white-label domain (if any) simply won't pre-fill, leaving the
# credentials to be typed by hand.
HOST_RE = re.compile(r"(^|\.)angellist\.com$", re.I)

# Locators tried in order: several variants anchored on standard HTML
# conventions (input[type=email] / autocomplete=username) plus
# name-substring fallbacks, so a restyled Investor Portal form still
# matches. Same approach for the password input.
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
# frame. AngelList's 2FA field is not watched for.
CLICK_RECORDER_JS = explore.click_recorder_js(
    EVENT_PREFIX, host_re=HOST_RE, user_selector=USER_SELECTOR,
    password_selector=PWD_SELECTOR, otp_selector=None)


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    explore.add_args(p, url=DEFAULT_URL, env_file=DEFAULT_ENV_FILE,
                     profile_dir=DEFAULT_PROFILE_DIR)
    p.add_argument(
        "--no-recorder", action="store_true",
        help=("Don't inject the click-recorder init script. Reduces "
              "page-side tampering when fighting an invisible anti-bot "
              "challenge (Turnstile / reCAPTCHA); network.jsonl + the "
              "Playwright trace still capture everything passively."),
    )
    p.add_argument(
        "--warmup", action="store_true",
        help=("Visit google.com and dwell briefly before opening the "
              "target, to age the session + accrue cookies so the "
              "anti-bot score isn't computed on a stone-cold profile."),
    )
    p.add_argument(
        "--cookies", type=Path, default=None,
        help=("Inject cookies from this JSON file (Playwright "
              "add_cookies format, as written by extract_cookies.py) "
              "before navigating — the BYO-session path that lands on "
              "the authenticated portal instead of the bot-walled login."),
    )
    p.add_argument(
        "--dump-links", action="store_true",
        help=("After the page settles, log every in-app angellist.com "
              "<a href> link and write them to links.txt — discovers the "
              "portal's section routes (portfolio/documents/…) during "
              "BYO discovery."),
    )
    return p.parse_args(argv)


def _prefill(page, username: str, password: str, filled: set) -> bool:
    """Pre-fill the login form on an angellist.com frame, each field at
    most once per page. Never submits: login and 2FA are driven by hand
    in the VNC session."""
    return explore.prefill_login(
        page, username, password, filled, host_re=HOST_RE,
        user_selector=USER_SELECTOR, password_selector=PWD_SELECTOR, log=log)


# What the person at the VNC session is asked to do.
WALK = ("log in via VNC, click through the LP portfolio / per-vehicle / "
        "activity pages we want to scrape.")


def _dump_links(page, debug_dir: Path) -> None:
    """Log every in-app angellist.com link on the settled page and write
    them to links.txt — the portal's section routes, found during BYO
    discovery."""
    time.sleep(8)  # let the SPA render its nav
    try:
        hrefs = page.evaluate(
            "() => [...new Set([...document.querySelectorAll("
            "'a[href]')].map(a => a.href))]")
    except Exception as exc:
        hrefs = []
        log.warning("dump-links eval failed: %s", debugcap.safe_error(exc))
    links = sorted(h for h in hrefs if "angellist.com" in h)
    (debug_dir / "links.txt").write_text("\n".join(links) + "\n")
    log.info("dump-links: %d angellist link(s) -> %s",
             len(links), debug_dir / "links.txt")
    for h in links:
        log.info("  link: %s", h)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)
    explore.prepare_profile(args.profile_dir, fresh=args.fresh, log=log,
                            note="2FA will be required on next login")
    username, password, prefill = explore.load_credentials(
        args.env_file, (USER_ENV,), (PASS_ENV,),
        no_prefill=args.no_prefill, host="angellist.com", log=log)
    filled: set = set()

    with contextlib.ExitStack() as stack:
        session = explore.Session.from_args(
            stack, args, redact=debugcap.secret_redactor(username, password),
            log=log)
        context = session.open_camoufox(stack, args.profile_dir,
                                        block_webrtc=True)
        if args.cookies:
            byo = json.loads(args.cookies.read_text(encoding="utf-8"))
            context.add_cookies(byo)
            log.info("injected %d BYO cookie(s) from %s (names: %s)",
                     len(byo), args.cookies,
                     ", ".join(sorted(c.get("name", "?") for c in byo)))
        if args.no_recorder:
            log.info("--no-recorder: click-recorder init script disabled "
                     "(less page tampering vs the anti-bot challenge; "
                     "network.jsonl + trace still capture passively)")
        session.attach(
            context,
            init_js=None if args.no_recorder else CLICK_RECORDER_JS,
            event_prefix=None if args.no_recorder else EVENT_PREFIX,
            prefill=(lambda page: _prefill(
                page, username, password, filled)) if prefill else None)

        page = context.new_page()
        if args.warmup:
            log.info("--warmup: visiting google.com to age the session + "
                     "accrue cookies before opening the target")
            try:
                page.goto("https://www.google.com",
                          wait_until="domcontentloaded", timeout=30_000)
                time.sleep(10)
            except Exception as exc:
                log.warning("warmup navigation failed (continuing): %s",
                            debugcap.safe_error(exc))
        try:
            page.goto(args.url, wait_until="domcontentloaded", timeout=45_000)
        except Exception as exc:
            log.warning("initial goto(%s) failed: %s — continuing to capture "
                        "whatever loaded", args.url, debugcap.safe_error(exc))
        session.started(page, args.url)
        if args.dump_links:
            _dump_links(page, session.root)
        session.record(WALK, max_duration=args.max_duration,
                       poll_prefill=False)
    session.report()
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
