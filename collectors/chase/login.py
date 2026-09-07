#!/usr/bin/env python3
"""chase login + scrape driver — the one-shot the wrapper's `download` runs.

Chase invalidates the session when Firefox closes and forces a fresh 2FA
challenge at every sign-in (DESIGN.md §F), so — like the schwab-web twin —
login and scrape happen in **one browser lifetime**: each run pays one 2FA.
There is nothing durable to persist between runs, so the `login` verb folds
into `download`; this script backs both.

Two 2FA modes, mirroring schwab-web:

* **CLI-MFA (default, `download`)** — headed Camoufox under Xvfb with **no
  VNC**. login.py pre-fills the form, submits it, then drives the fraud
  challenge from the **terminal** via `auth_dialog.py`: it prompts which
  number to text, reads the code from stdin, and types it into the page.
  Push and voice are handled too (approve on the phone / read the code
  aloud). The browser holds the cookie and lets Chase's JS finish the
  sign-in, so nothing is replayed out-of-band.
* **VNC (`vnc-login`, `--no-cli-mfa`)** — the fallback when the CLI drive
  can't find a challenge control (the challenge UI selectors are
  trace-derived — DESIGN.md §5): x11vnc is exposed and the human completes
  Sign In + 2FA by hand in the browser.

Authentication is detected from **network traffic**, not the URL: the
pre-auth logon page and the signed-in dashboard share the URL
`/web/auth/dashboard#/dashboard/overview` (§A), so the URL can't tell them
apart. What differs is which `/svc/` calls fire — the post-auth router
(`/user/router/list`) and the account API (`/accounts/secure/…`) fire only
once 2FA has completed.

Modes:
  --check   probe the persisted profile against an authenticated surface and
            exit 0 (alive) / 1 (dead). Dead between runs is expected (§F).
  default   pre-fill, complete 2FA (CLI or VNC), then scrape via
            download.walk() into --bronze-dir.

Read-only (CLAUDE.md): never a money-movement, card-management, or settings
surface. The scrape this drives covers both readable products — the deposit
accounts and the credit cards.
"""
from __future__ import annotations

import argparse
import contextlib
import logging
import os
import sys
import time
from pathlib import Path

from collectorkit import cli, debugcap, envfile, launch

import auth_dialog
import mdsui
# Frame-aware helpers for Chase's iframed, open-shadow MDS UI, shared with
# download.py. Aliased to the `_name`s the rest of this module already uses.
from mdsui import (locate as _locate, first_in_frames as _first_in_frames,
                   activate as _activate, click_text as _click_text,
                   click_role as _click_role, deep_click_text as _deep_click_text)
# Reuse explore's validated, frame-aware login pre-fill rather than a second
# copy — same collector, same selectors.
from explore import _maybe_prefill_login, USER_ENV, PASS_ENV

log = logging.getLogger("chase.login")

DEFAULT_PROFILE_DIR = Path("/secrets/chase-profile")
DEFAULT_ENV_FILE = Path("/secrets/chase.env")
START_URL = "https://secure.chase.com"

# See the module docstring: authenticated iff a signed-in /svc/ call is seen.
AUTHED_SVC_MARKERS = ("/user/router/list", "/accounts/secure/")

# How long to auto-detect an in-app-push approval before falling back to a
# keypress. Long enough for a prompt approval to be caught hands-free; short
# enough not to feel stuck when the pinned camoufox drops the completion event.
PUSH_AUTODETECT_S = 90

# Challenge-UI controls, from the captured DOM (§A/§5). The step-up page
# ("Confirm Your Identity") is Chase's MDS design system: a list of methods
# `<mds-list-item id=…>` picked by id, then a code field. Each factor maps to
# (id, visible label): the id is duplicated on the page (a hidden template
# alongside the live one), so selection prefers the first *visible* match and
# falls back to the accessible label, which Playwright reaches through the MDS
# open shadow roots. The OTP input follows the standard one-time-code
# descriptors; submit is via Enter, so no button selector is needed.
FACTOR_CONTROL = {
    auth_dialog.FACTOR_INAPP: ("#inAppSend", "Confirm using our mobile app"),
    auth_dialog.FACTOR_OTP_SMS: ("#sms", "Get a text"),
    auth_dialog.FACTOR_OTP_VOICE: ("#voice", "Get a call"),
}
# The visible code field is the inner `#otpInput-input` (a password input with
# autocomplete=one-time-code) inside the `mds-text-input-secure`'s open shadow —
# NOT the host `#otpInput`, and NOT the hidden disabled `[name=otp-input]`
# backing input beside it. The generic descriptors trail as a fallback.
OTP_INPUT_SELECTOR = (
    "#otpInput-input, input[autocomplete='one-time-code'], "
    "input[inputmode='numeric'], input[name*='otp' i]:not([type='hidden']), "
    "input[id*='otp' i]:not([type='hidden']), input[maxlength='8'], "
    "input[type='tel']"
)


def is_authenticated_response(url: str, status: int) -> bool:
    """True when a `/svc/` response marks a completed sign-in (see
    AUTHED_SVC_MARKERS). Browserless, so it is unit-tested."""
    if not 200 <= status < 400:
        return False
    if "/svc/" not in url:
        return False
    return any(m in url for m in AUTHED_SVC_MARKERS)


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip().splitlines()[0],
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument("--profile-dir", type=Path, default=DEFAULT_PROFILE_DIR,
                   help="Persistent Camoufox profile dir. Default: %(default)s.")
    p.add_argument("--bronze-dir", type=Path, default=None,
                   help="Bronze tree root; required for the scrape path "
                        "(the download verb passes /data).")
    p.add_argument("--env-file", type=Path, default=DEFAULT_ENV_FILE,
                   help="Bash-sourced env file with CHASE_USERNAME / "
                        "CHASE_PASSWORD. Default: %(default)s.")
    p.add_argument("--check", action="store_true",
                   help="Probe the persisted session and exit 0/1; no 2FA "
                        "push. Reports DEAD between runs by design (§F).")
    p.add_argument("--cli-mfa", dest="cli_mfa", action="store_true",
                   default=True,
                   help="Drive 2FA from the terminal, no VNC (default).")
    p.add_argument("--no-cli-mfa", dest="cli_mfa", action="store_false",
                   help="Wait for Sign In + 2FA to be done by hand over VNC "
                        "(the vnc-login fallback).")
    p.add_argument("--mfa-timeout", type=int, default=600,
                   help="Seconds to wait for 2FA to complete. Default: "
                        "%(default)s.")
    p.add_argument("--no-documents", action="store_true",
                   help="Skip the statement-PDF pass (the run's heavy part).")
    p.add_argument("--dry-run", action="store_true",
                   help="Walk after login but export nothing.")
    p.add_argument("--debug", action="store_true",
                   help="Capture the login + challenge DOM/screenshots into "
                        "--screenshot-dir (for pinning selectors).")
    p.add_argument("--screenshot-dir", type=Path, default=Path("/debug"),
                   help="Where login diagnostics land (outside bronze). "
                        "Default: %(default)s.")
    # Standard fleet flags for the download verb (-v, --lookback).
    cli.add_standard_args(p, verb="download")
    return p.parse_args(argv)


@contextlib.contextmanager
def _camoufox(profile_dir: Path):
    """Open a persistent Camoufox context on the profile dir (headed under
    the entrypoint's Xvfb — the container exposes VNC only for vnc-login).
    Yields (context, page)."""
    from camoufox.sync_api import Camoufox
    launch.prepare_profile_dir(profile_dir)
    cam = Camoufox(
        persistent_context=True,
        user_data_dir=str(profile_dir),
        os="macos",
        window=(1280, 800),
        headless=False,
        humanize=True,
        geoip=True,
        firefox_user_prefs=launch.firefox_prefs(),
    )
    context = cam.__enter__()
    try:
        page = context.pages[0] if context.pages else context.new_page()
        yield context, page
    finally:
        with contextlib.suppress(Exception):
            cam.__exit__(None, None, None)


class _AuthWatch:
    """Watch the context's responses: flip `ok` on a signed-in `/svc/` call,
    and stash the `challenge-options` body (the factor menu) the CLI 2FA
    needs; `invoked` marks that a code was sent / push fired."""

    def __init__(self):
        self.ok = False
        self.challenge_options = None
        self.invoked = False          # a challenge-invocations response arrived

    def attach(self, context):
        context.on("response", self._on_response)
        return self

    def _on_response(self, resp):
        try:
            url = resp.url
            if resp.request.method.upper() == "OPTIONS":
                # A CORS preflight answers 200 for the URL it precedes, so an
                # auth signal keyed on URL + status would read one as a
                # completed sign-in. The /svc/ calls are same-origin today and
                # send none; the guard costs nothing and the amex sibling hit
                # exactly this live.
                return
            if is_authenticated_response(url, resp.status):
                self.ok = True
            if "/svc/" not in url:
                return
            if "challenge-options" in url:
                body = resp.json()
                if isinstance(body, dict):
                    self.challenge_options = body
            elif "challenge-invocations" in url:
                # The method truly registered: a code was texted/called or a
                # push was sent.
                self.invoked = True
        except Exception:            # pragma: no cover — defensive
            pass


def _pump(page, ms: int = 500) -> None:
    """Advance the Playwright sync event loop so `.on()` handlers fire.

    A bare time.sleep() does NOT deliver Playwright events in the sync API —
    callbacks run only while the main thread is inside a Playwright call. The
    approval navigation can make wait_for_timeout raise (context destroyed);
    fall back to another Playwright call (wait_for_load_state), never a bare
    sleep, so events keep being delivered."""
    try:
        page.wait_for_timeout(ms)
    except Exception:
        with contextlib.suppress(Exception):
            page.wait_for_load_state(timeout=ms)


# The authenticated app shell renders a brand bar + primary nav (a sign-out
# button, the Accounts menu) that the sign-in / challenge pages never do. A
# live DOM poll for these is the robust auth signal: the pinned camoufox drops
# some response/navigation events (see schwab-web `_wait_for_post_auth`), so the
# response watcher alone can miss the completion across the approval navigation.
_AUTHED_DOM_JS = (
    "() => !!document.querySelector("
    "'#brand_bar_sign_in_out, #primaryNavigationBar, #requestAccounts')")


def _authenticated_live(page) -> bool:
    """True when a live DOM check finds the authenticated app shell in a
    chase frame — a poll that doesn't depend on the response events being
    delivered."""
    for frame in mdsui.chase_frames(page):
        with contextlib.suppress(Exception):
            if frame.evaluate(_AUTHED_DOM_JS):
                return True
    return False


def _session_authed(page, watch: "_AuthWatch") -> bool:
    """The one definition of "signed in": the response watcher saw the
    authenticated call OR the live DOM shows the app shell."""
    return watch.ok or _authenticated_live(page)


def _wait_for(predicate, page, timeout_s: int) -> bool:
    """Poll `predicate()` while pumping the event loop, until true or timeout."""
    deadline = time.monotonic() + timeout_s
    while time.monotonic() < deadline:
        if predicate():
            return True
        _pump(page)
    return False


def _capture(page, args, name: str) -> None:
    """DOM + screenshot to --screenshot-dir, only under --debug. Also dumps
    each chase.com **frame** DOM (`<name>-frameN.html`) — the login form and
    challenge live in an iframe, so the main-document capture alone doesn't
    show their controls."""
    if not args.debug:
        return
    debugcap.capture_page(page, args.screenshot_dir, name, log=log)
    with contextlib.suppress(Exception):
        d = debugcap.capture_dir(args.screenshot_dir)
        for i, frame in enumerate(mdsui.chase_frames(page)):
            with contextlib.suppress(Exception):
                # Scrubbed like the main document: the sign-in form lives in
                # one of these frames, so a serialized capture carries the
                # typed password in a `value` attribute.
                (d / f"{name}-frame{i}.html").write_text(
                    debugcap.scrub_dom(frame.content()), encoding="utf-8")


def run_check(profile_dir: Path) -> int:
    """Load the dashboard on the persisted profile and watch for an
    authenticated `/svc/` call. Exit 0 if the session is alive, 1 otherwise
    (dead between runs is expected — §F)."""
    with _camoufox(profile_dir) as (ctx, page):
        watch = _AuthWatch().attach(ctx)
        try:
            page.goto(START_URL, wait_until="domcontentloaded", timeout=45_000)
        except Exception as exc:
            log.warning("check navigation failed: %r", exc)
        if _wait_for(lambda: _session_authed(page, watch), page, 20):
            log.info("session ALIVE")
            return 0
    log.info("session DEAD — a fresh login is required (expected between runs)")
    return 1


def _prefill(page, username: str, password: str) -> bool:
    """Poll explore's frame-aware prefill until the form is filled (the SPA /
    iframe form may mount late). Returns True once filled."""
    prefilled: set = set()
    for _ in range(10):
        if _maybe_prefill_login(page, username, password, prefilled):
            return True
        _pump(page, 1000)
    return False


def _submit_signin(page) -> bool:
    """Submit the sign-in form (which lives in a chase.com iframe). Pressing
    Enter in the password field triggers the form's native submit — robust
    without a hard-coded button selector — with a button as fallback."""
    pwd = _locate(page, "input[type='password']")
    if pwd is not None:
        with contextlib.suppress(Exception):
            pwd.press("Enter")
            return True
    for sel in ('button:has-text("Sign in")', 'button[type="submit"]',
                'input[type="submit"]', '#signin-button', '#signin'):
        b = _locate(page, sel)
        if b is not None:
            with contextlib.suppress(Exception):
                b.click(timeout=3000)
                return True
    return False


def _click_factor(page, factor: str) -> bool:
    """Select a 2FA method in the "Confirm Your Identity" MDS list.

    The mds-list-item is a data element with an EMPTY shadow root; the visible
    row is rendered as an `<a href>` (role=link) deeper in the mds-list's OPEN
    shadow, with the handler on that anchor. A real Playwright click on it by
    role+name is the PROVEN path (verified live to send the challenge); the
    deep-shadow / text / host-id clicks are fallbacks."""
    sel, label = FACTOR_CONTROL[factor]
    return (_click_role(page, "link", label)
            or _deep_click_text(page, label)
            or _click_text(page, label)
            or _activate(page, sel))


def _click_next(page) -> bool:
    """Click the challenge "Next" button. It renders as role=button "Next" in
    the mds-button's open shadow (a real Playwright click by role is the
    proven path); the host id / shadow-text clicks are fallbacks."""
    return (_click_role(page, "button", "Next")
            or _activate(page, "#next-content")
            or _deep_click_text(page, "Next"))


def _enter_otp(page, code: str) -> bool:
    """Type the one-time code into the secure code field and submit.

    `#otpInput` is an `mds-text-input-secure` whose real `<input>` lives in an
    OPEN shadow root. Playwright's selector engine pierces open shadow, so the
    reliable path is a real locator on that inner input — click it, then type
    with real key events — not focusing the zero-box host (which does not
    delegate the keystrokes). Returns True only once the code is typed AND the
    submit control is clicked, so a failure surfaces instead of waiting out the
    MFA timeout."""
    field = None
    for frame in mdsui.chase_frames(page):
        with contextlib.suppress(Exception):
            for sel in ("#otpInput-input", OTP_INPUT_SELECTOR):
                loc = frame.locator(sel).first
                if loc.count():
                    field = loc
                    break
        if field is not None:
            break
    if field is None:
        return False
    try:
        field.click(timeout=3000)
        field.fill("")                 # clear any prior / rejected value first
        page.keyboard.type(code, delay=30)
    except Exception as exc:
        log.debug("OTP entry failed: %r", exc)
        return False
    return _submit_otp(page)


def _submit_otp(page) -> bool:
    """Submit the entered code. The step-up page uses "Next"; a build that
    labels it "Submit"/"Verify" is covered too."""
    return (_click_next(page)
            or _click_role(page, "button", "Submit")
            or _click_role(page, "button", "Verify"))


def _cli_two_factor(page, watch: "_AuthWatch", args) -> int:
    """Drive the fraud challenge from the terminal. Returns 0 on an
    authenticated session, non-zero (with a captured DOM + a pointer to
    vnc-login) when a challenge control can't be driven."""
    # Wait for either the challenge menu or an already-authenticated session
    # (a recently-trusted device may skip the challenge and land signed in —
    # detected via the live DOM check, not just the response event).
    _wait_for(lambda: _session_authed(page, watch)
              or watch.challenge_options is not None, page, 60)
    if _session_authed(page, watch):
        return 0
    if watch.challenge_options is None:
        _capture(page, args, "post-signin-no-challenge")
        log.error("no 2FA challenge or session detected after sign-in. "
                  "Check the credentials, or retry with vnc-login. "
                  "(DOM captured with --debug.)")
        return 1

    _capture(page, args, "challenge")
    menu = auth_dialog.parse_challenge_options(watch.challenge_options)
    try:
        factor = auth_dialog.choose_factor(menu)
    except auth_dialog.ChallengeError as exc:
        log.error("%s — retry with vnc-login.", exc)
        return 1

    # Pick the method in the "Confirm Your Identity" mds-list — the browser
    # only sends a code once it is clicked (the earlier miss: the CLI menu
    # was read from the network response, but nothing was clicked).
    if factor not in FACTOR_CONTROL or not _click_factor(page, factor):
        _capture(page, args, "factor-click-failed")
        log.error("could not select the 2FA method '%s' on the page. "
                  "Retry with vnc-login. (DOM captured with --debug.)", factor)
        return 1
    _capture(page, args, "after-factor")

    # Selecting a method reveals a "Next" button; the challenge is only sent
    # once it is clicked (#next-content, from the captured DOM). No-op if a
    # build sends on selection.
    _wait_for(lambda: _first_in_frames(page, "#next-content") is not None,
              page, 8)
    _click_next(page)
    _capture(page, args, "after-next")

    if factor == auth_dialog.FACTOR_INAPP:
        # Push is sent when the method is selected. Confirm the invocation
        # fired (so a "click" that didn't register isn't mistaken for a sent
        # push) before waiting on the approval.
        if not _wait_for(lambda: watch.invoked or watch.ok, page, 20):
            _capture(page, args, "push-not-sent")
            log.error("selected the app-approval method but Chase sent no "
                      "push — the control didn't register the click. Retry "
                      "with vnc-login. (DOM captured with --debug.)")
            return 1
        device = menu.devices[0].label if menu.devices else "your device"
        log.info("approve the sign-in in the Chase app on %s — it continues "
                 "automatically once approved.", device)
        # Auto-detect the approval (Chase's JS completes the sign-in on its
        # own). The pinned camoufox can drop the completion events across that
        # navigation, so if it isn't confirmed within a short window, fall back
        # to a keypress — by then the page has settled and the shared wait
        # below sees it — rather than sit out the full MFA timeout.
        if not _wait_for(lambda: _session_authed(page, watch),
                         page, PUSH_AUTODETECT_S):
            log.info("if you've approved the notification, press Enter to "
                     "continue…")
            with contextlib.suppress(EOFError):
                input()
    else:
        # SMS / voice: a "which number?" step follows when more than one is on
        # file; picking it triggers the code send. The phone option is another
        # mds-list-item — select it by its light-DOM `label` attribute (the
        # masked number), which is reachable even with a closed shadow root.
        if len(menu.phones) > 1:
            phone = auth_dialog.choose_phone(menu)
            last4 = "".join(ch for ch in phone.label if ch.isdigit())[-4:]
            if not _select_phone(page, last4):
                log.warning("couldn't match the phone option on the page; "
                            "the code may go to Chase's default number.")
            _capture(page, args, "after-phone")
        # The code is sent (invocation fired) before the field renders; wait
        # for that signal or the field itself.
        if not _wait_for(lambda: watch.invoked
                         or _locate(page, OTP_INPUT_SELECTOR) is not None,
                         page, 30):
            _capture(page, args, "code-not-sent")
            log.error("no code was sent — the method/number control didn't "
                      "register. Retry with vnc-login. (DOM captured with "
                      "--debug.)")
            return 1
        _capture(page, args, "before-otp")
        code = auth_dialog.read_otp()
        if not _enter_otp(page, code):
            _capture(page, args, "otp-field-not-found")
            log.error("could not find the code field to enter the OTP. "
                      "Retry with vnc-login. (DOM captured with --debug.)")
            return 1

    if not _wait_for(lambda: _session_authed(page, watch),
                     page, args.mfa_timeout):
        _capture(page, args, "after-2fa-not-authenticated")
        log.error("2FA submitted but no authenticated session appeared. "
                  "Retry with vnc-login. (DOM captured with --debug.)")
        return 1
    return 0


def _select_phone(page, last4: str) -> bool:
    """Pick the phone whose masked number ends in `last4` from the "which
    number?" MDS list, then advance. The rendered row (role=link) carries the
    number; the light-DOM `label` attribute backs the selector fallback."""
    if not (_click_role(page, "link", last4)
            or _deep_click_text(page, last4, exact=False)
            or _activate(page, f"mds-list-item[label*='{last4}']")):
        return False
    _wait_for(lambda: _first_in_frames(page, "#next-content") is not None,
              page, 8)
    _click_next(page)
    return True


def run_login_and_scrape(args: argparse.Namespace) -> int:
    if envfile.source_env_file(args.env_file):
        log.info("env file: %s (sourced)", args.env_file)
    username = os.environ.get(USER_ENV, "")
    password = os.environ.get(PASS_ENV, "")
    if not (username and password):
        log.warning("%s / %s not set — the form will not be pre-filled.",
                    USER_ENV, PASS_ENV)

    since, until = cli.resolve_standard(args, verb="download", log=log)

    with _camoufox(args.profile_dir) as (ctx, page):
        watch = _AuthWatch().attach(ctx)
        page.goto(START_URL, wait_until="domcontentloaded", timeout=45_000)
        if username and password and _prefill(page, username, password):
            log.info("sign-in form pre-filled")
        _capture(page, args, "signin")

        if args.cli_mfa:
            if not _submit_signin(page):
                _capture(page, args, "signin-submit-failed")
                log.error("could not submit the sign-in form. Retry with "
                          "vnc-login. (DOM captured with --debug.)")
                return 1
            log.info("submitted — driving 2FA from the terminal "
                     "(use vnc-login if a prompt can't find its control)")
            rc = _cli_two_factor(page, watch, args)
            if rc != 0:
                return rc
        else:
            log.info("Complete Sign In + 2FA in the browser over VNC "
                     "(waiting up to %ds). Any factor works.", args.mfa_timeout)
            if not _wait_for(lambda: _session_authed(page, watch),
                             page, args.mfa_timeout):
                log.error("timed out waiting for an authenticated session")
                return 1
        log.info("authenticated — starting scrape")

        if args.bronze_dir is None:
            log.info("no --bronze-dir: login only, nothing to scrape")
            return 0
        import download
        summary = download.walk(
            page, args.bronze_dir, since=since, until=until,
            documents=not args.no_documents, dry_run=args.dry_run,
            debug=args.debug)
        log.info("scrape done: %s", summary)
    return 0


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose or args.debug)
    if args.check:
        return run_check(args.profile_dir)
    return run_login_and_scrape(args)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
