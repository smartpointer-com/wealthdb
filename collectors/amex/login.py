#!/usr/bin/env python3
"""American Express sign-in driver — the machinery `download` signs in with,
and the cheap `login --check` device-trust probe.

**There is no `login` verb.** It folded into `download` (DESIGN.md §L): the
sign-in budget is small enough that a captcha fires at roughly seven in
twenty minutes (§K), and a separate `login` doubled the cost of every
routine run for nothing — the device trust it minted is minted just as well
inside `download`, by this same code, on the same profile. The wrapper traps
a bare `login` host-side as a no-op so an orchestrator's
login → download → load never trips.

The sign-in form is classic light DOM on www.americanexpress.com, and
submitting it posts to a legacy logon endpoint whose JSON response is the
authoritative outcome (DESIGN.md §B). Akamai Bot Manager is cookie-borne and
its script runs in the page, so the logon must go through the real browser —
but no data endpoint carries a sensor header, so once signed in the data is
fetched over REST (download.py).

Session and device trust answer differently here (DESIGN.md §G):

* the **session cookies are `Discard`-scoped**, so they die with the browser
  and every run signs in afresh — reopening the browser can never continue a
  session;
* the **`device-id` cookie persists** (about a year) in the Camoufox profile
  and skips the passcode, so a run on a trusted device is unattended.

That durable trust is what `login --check` reports, and it reports it by
reading the profile's own Firefox cookie jar — no browser, no sign-in, no
network, no MFA — because on this source a probe that signs in costs exactly
what the whole design is trying to save.

Authentication is decided by the **logon response**, never by a URL: the
pre-auth and post-auth pages share origins, and the SPA route is not a
credential. A REST probe backs it up, gated to fire only once off the login
flow — probing an authenticated endpoint mid-challenge poisons the challenge
server-side (the firstcitizens root cause, DESIGN.md §4.2 there).

Read-only (AGENTS.md): the browser only ever touches the sign-in form and
the passcode challenge; the card data is fetched over REST by download.py.
Never a payment, rewards, offers, or settings surface.
"""
from __future__ import annotations

import argparse
import contextlib
import logging
import os
import re
import shutil
import sqlite3
import sys
import tempfile
import time
from datetime import datetime, timezone
from pathlib import Path
from urllib.parse import urlparse

from collectorkit import cli, debugcap, envfile, launch
from collectorkit.launch import page_url, pump, wait_for

import amexclient
import auth_dialog
# Reuse explore's validated, frame-aware, origin-gated login pre-fill rather
# than a second copy — same collector, same selectors.
from explore import _maybe_prefill_login, PASS_ENV, USER_ENV

log = logging.getLogger("amex.login")

DEFAULT_PROFILE_DIR = Path("/secrets/amex-profile")
DEFAULT_ENV_FILE = Path("/secrets/amex.env")

# The three ways a passcode challenge is answered:
TWOFACTOR_CLI = "cli"     # drive the challenge screens, code read from stdin
TWOFACTOR_VNC = "vnc"     # human completes the challenge in the browser
TWOFACTOR_NONE = "none"   # unattended download: a challenge is a hard error

# How long to wait for a logon outcome (signed in, or a challenge) after the
# form is submitted.
OUTCOME_TIMEOUT_S = 120


class NeedsLogin(RuntimeError):
    """Raised by authenticate(two_factor='none') when the device is no longer
    trusted and a passcode challenge appeared — an unattended `download` has
    no terminal to answer it, so it surfaces this to say the challenge needs
    a run that can: `download` with a TTY, or `vnc-login`."""


class LogonFailed(RuntimeError):
    """The provider refused the credentials outright (not a challenge). Its
    own error code / message rides along — surfaced verbatim, never
    reinterpreted (the fleet's no-cleverness-in-auth rule)."""


# --- browser plumbing -----------------------------------------------------

@contextlib.contextmanager
def camoufox(profile_dir: Path, fresh: bool = False):
    """Open a persistent Camoufox context on the profile dir (headed under
    the entrypoint's Xvfb; VNC is exposed only for vnc-login). The persistent
    profile is what carries `device-id`, the trust cookie, across runs
    (DESIGN.md §G).

    `fresh` moves the profile ASIDE rather than deleting it — the fleet
    lesson — so a run that forces the untrusted path can be undone, and a
    mistaken --fresh does not cost a real passcode permanently. Yields
    (context, page)."""
    if fresh and profile_dir.exists():
        stamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
        aside = profile_dir.with_name(f"{profile_dir.name}.pre-fresh-{stamp}")
        shutil.move(str(profile_dir), str(aside))
        log.warning("--fresh: moved the profile to %s — the next login is "
                    "an unrecognised device and will fire a real passcode. "
                    "Move it back to undo.", aside)
    with launch.persistent_camoufox(profile_dir) as (context, page):
        yield context, page


class _LogonWatch:
    """Capture the logon response (status + parsed body) as the page fires
    it. This is the primary auth signal — the response says outright whether
    the login succeeded, needs a passcode, or failed (DESIGN.md §B) — but it
    is not relied on ALONE: the pinned Camoufox can drop `.on()` events
    across navigations, so a REST probe backs it up.

    **Only the POST counts.** The browser sends a CORS **preflight** to the
    very same URL, and it answers `200` with an empty body ABOUT A SECOND
    BEFORE the real response arrives. Matching on the URL alone therefore
    reads the preflight as the outcome, finds no `statusCode` in its empty
    body, and reports a sign-in the provider never refused — which is exactly
    what the first live run did."""

    def __init__(self):
        self.status: int | None = None
        self.body: dict | None = None

    def attach(self, context):
        context.on("response", self._on_response)
        return self

    def _on_response(self, resp):
        try:
            if amexclient.LOGON_PATH not in resp.url:
                return
            if resp.request.method.upper() != "POST":
                return          # the CORS preflight — see the class docstring
            self.status = resp.status
            with contextlib.suppress(Exception):
                body = resp.json()
                if isinstance(body, dict):
                    self.body = body
        except Exception:            # pragma: no cover — defensive
            pass

    def outcome(self) -> amexclient.LogonOutcome | None:
        if self.status is None:
            return None
        return amexclient.classify_logon(self.status, self.body or {})

    def reset(self) -> None:
        """Forget the captured response. The challenge flow ends with a
        SECOND logon call, and the first one's 'needs a passcode' verdict
        must not be mistaken for the second one's."""
        self.status = None
        self.body = None


def _on_login_flow(page) -> bool:
    """True while the page is still on a sign-in / challenge route. The REST
    probe is gated on this being False: an authenticated request fired
    mid-challenge invalidates the challenge server-side."""
    url = page_url(page)
    return ("/login" in url or "/logon" in url
            or url.rstrip("/") == amexclient.WWW_ORIGIN)


# --- REST over the browser context ---------------------------------------

def fn_post(context, fn: tuple[str, str], body: dict | None = None):
    """POST one named BFF function over the browser context (so it shares
    the cookie jar). `fn` is an (endpoint, ce-source) pair from
    amexclient. Returns (status, parsed-json-or-None)."""
    name, ce_source = fn
    resp = context.request.post(
        amexclient.function_url(name),
        headers=amexclient.function_headers(ce_source),
        data=body if body is not None else {})
    return _resp_json(resp)


def _resp_json(resp) -> tuple[int, dict | None]:
    body = None
    with contextlib.suppress(Exception):
        parsed = resp.json()
        if isinstance(parsed, dict):
            body = parsed
    return resp.status, body


def probe_authenticated(context) -> bool:
    """The cheapest authenticated read-only call the source allows: the
    customer overview returns 200 signed in and 401 signed out (measured
    both ways, DESIGN.md §G). This is what proves authentication — never a
    URL or an SPA route."""
    with contextlib.suppress(Exception):
        status, body = fn_post(context, amexclient.FN_CUSTOMER_OVERVIEW)
        return status == 200 and isinstance(body, dict) and bool(body)
    return False


def _authed_cheap(watch: _LogonWatch) -> bool:
    """The event-free, no-network half of the signed-in signal: a captured
    logon response said authenticated. Checked every poll tick; the REST
    probe is throttled and gated separately."""
    o = watch.outcome()
    return o is not None and o.authenticated


# --- terminal 2FA: drive the passcode challenge screens -------------------

def read_challenge_targets(page) -> tuple:
    """Read the delivery options from the on-screen option buttons — the
    ground truth on the challenge screen, robust to a dropped response event.
    Each option's `value` is its DOM index; `display` is the button's own
    (masked-destination) label."""
    targets = []
    with contextlib.suppress(Exception):
        btns = page.locator(amexclient.SEL_CHALLENGE_OPTION)
        for i in range(btns.count()):
            with contextlib.suppress(Exception):
                text = " ".join(btns.nth(i).inner_text().split())
                targets.append(amexclient.ChallengeTarget(
                    value=str(i), display=text,
                    kind=amexclient.target_kind(text)))
    return tuple(targets)


def _click_challenge_target(page, target) -> bool:
    """Click the chosen delivery option by its DOM index."""
    with contextlib.suppress(Exception):
        btns = page.locator(amexclient.SEL_CHALLENGE_OPTION)
        if target.value.isdigit() and int(target.value) < btns.count():
            btns.nth(int(target.value)).click(timeout=5000)
            return True
    return False


def looks_like_captcha(page) -> bool:
    """True when a bot-defense challenge is on screen.

    A captcha cannot be answered from a terminal, so the only useful thing to
    do with it is stop immediately and say which verb CAN answer it. See
    amexclient.SEL_CAPTCHA for why the detection is broad rather than pinned.
    """
    with contextlib.suppress(Exception):
        return page.locator(amexclient.SEL_CAPTCHA).count() > 0
    return False


def describe_screen(page) -> str:
    """A one-line description of what is actually on screen, for a failure
    message.

    The fleet lesson is that a login failure path must be self-diagnosing —
    the next drift should debug itself from a pasted log rather than needing
    a fresh live run. So this is printed unconditionally, not behind --debug.

    It reports element IDENTIFIERS (test-id / id / name) and never element
    text: a challenge screen's labels carry the masked destination, and this
    string is written to be pasted into a chat.
    """
    parts = []
    with contextlib.suppress(Exception):
        parts.append("path=" + urlparse(page_url(page)).path)
    with contextlib.suppress(Exception):
        parts.append(f"title={page.title()[:60]!r}")
    controls: list[str] = []
    with contextlib.suppress(Exception):
        loc = page.locator("button, input, [data-testid]")
        for i in range(min(loc.count(), 25)):
            with contextlib.suppress(Exception):
                el = loc.nth(i)
                if not el.is_visible():
                    continue
                ident = (el.get_attribute("data-testid")
                         or el.get_attribute("id")
                         or el.get_attribute("name"))
                if ident:
                    controls.append(ident[:40])
    parts.append("controls=" + (",".join(dict.fromkeys(controls)) or "none"))
    return "; ".join(parts)


def _otp_boxes(page) -> list:
    """The six single-digit passcode inputs, in order. Empty when the
    code-entry screen is not up."""
    boxes = []
    for i in range(amexclient.OTP_DIGITS):
        loc = page.locator(amexclient.SEL_OTP_INPUT.format(i=i)).first
        with contextlib.suppress(Exception):
            if loc.count():
                boxes.append(loc)
    return boxes


def _clear_otp(boxes) -> None:
    """Empty every box. Always run before typing — a retry over a partly
    filled control is exactly what produced the observed HTTP 400
    (DESIGN.md §B)."""
    for box in boxes:
        with contextlib.suppress(Exception):
            box.fill("")


def enter_otp(page, code: str) -> bool:
    """Type the passcode into the six boxes and verify by read-back.

    Two shapes are tried, because the control supports both: a paste of the
    whole code into the first box, which the widget distributes, and one
    digit per box. Either way the boxes are read back before Continue is
    clicked — a partially filled control submits and is rejected, which is
    the failure the first capture caught. Returns False without clicking
    when the code did not land, so the caller can report a clean failure
    rather than burning the passcode."""
    boxes = _otp_boxes(page)
    if len(boxes) != amexclient.OTP_DIGITS:
        log.error("expected %d passcode boxes, found %d — the challenge UI "
                  "has drifted (DOM captured with --debug)",
                  amexclient.OTP_DIGITS, len(boxes))
        return False

    def _readback() -> str:
        out = []
        for box in boxes:
            try:
                out.append((box.input_value(timeout=1500) or "").strip())
            except Exception:
                return ""
        return "".join(out)

    for attempt in ("paste", "per-digit"):
        _clear_otp(boxes)
        try:
            if attempt == "paste":
                boxes[0].click(timeout=4000)
                boxes[0].fill(code, timeout=3000)
            else:
                for box, digit in zip(boxes, code, strict=False):
                    box.click(timeout=4000)
                    box.fill(digit, timeout=3000)
        except Exception as exc:
            # safe_error, never %r: a failed fill() renders the typed value
            # verbatim inside Playwright's Call log block, and the value
            # here is the live one-time passcode.
            log.debug("passcode entry (%s) failed: %s", attempt,
                      debugcap.safe_error(exc))
            continue
        if _readback() == code:
            return _click_continue(page)
        log.debug("passcode entry (%s) did not read back", attempt)
    _clear_otp(boxes)
    log.error("could not enter the passcode into the six boxes")
    return False


def _click_continue(page) -> bool:
    with contextlib.suppress(Exception):
        btn = page.locator(amexclient.SEL_CONTINUE).first
        if btn.count():
            btn.click(timeout=5000)
            return True
    return False


def register_device(page, timeout_s: float = 20) -> None:
    """Click the device-registration control so this browser is trusted on
    later runs.

    Best-effort by nature — a failure still leaves an authenticated session —
    but NOT unimportant: without it every subsequent run pays a passcode, and
    on this source sign-ins are the scarce thing (DESIGN.md §K). So a miss
    reports what was actually on screen rather than just that it happened
    (§M): either the control has drifted, or the provider did not offer it,
    and the two want different responses."""
    wait_for(lambda: page.locator(amexclient.SEL_REGISTER_DEVICE).count() > 0,
             page, timeout_s)
    with contextlib.suppress(Exception):
        btn = page.locator(amexclient.SEL_REGISTER_DEVICE).first
        if btn.count():
            btn.click(timeout=5000)
            log.info("registered this device — later runs skip the passcode")
            return
    log.warning("no device-registration control found, so the next run will "
                "need another passcode. On screen: %s", describe_screen(page))
    log.warning("If those controls include one that registers the device, "
                "its selector has drifted (amexclient.SEL_REGISTER_DEVICE). "
                "If they do not, the provider did not offer registration on "
                "this sign-in.")


def _drive_challenge_cli(page, watch: _LogonWatch, args) -> bool:
    """Drive the passcode challenge from the terminal: pick the delivery
    option (read from the on-screen buttons), send the code, read it from
    stdin, submit it, and register the device. Returns True once the code is
    submitted; the caller confirms the authenticated session."""
    # Wait for the passcode UI — but stop early on a captcha, which no
    # terminal can answer (DESIGN.md §K).
    ready = wait_for(
        lambda: page.locator(amexclient.SEL_CHALLENGE_OPTION).count() > 0
        or len(_otp_boxes(page)) == amexclient.OTP_DIGITS
        or looks_like_captcha(page), page, 30)
    if ready and looks_like_captcha(page):
        _capture(page, args, "challenge-captcha")
        log.error("a bot-defense challenge (captcha) is on screen, and it "
                  "cannot be answered from a terminal. Run `vnc-login` and "
                  "solve it by hand. Repeated rapid sign-ins are what "
                  "provokes it — give the account a few hours first.")
        return False
    if not ready:
        _capture(page, args, "challenge-no-options")
        log.error("the sign-in reported a challenge, but neither the "
                  "delivery options nor the passcode boxes appeared within "
                  "30s. On screen: %s", describe_screen(page))
        log.error("Run `vnc-login` to complete it by hand. If the controls "
                  "above look like a passcode screen, they have drifted and "
                  "the selectors need re-pinning (DESIGN.md §B).")
        return False
    _capture(page, args, "challenge-options")
    targets = read_challenge_targets(page)
    picked = None
    if targets:
        target = auth_dialog.choose_target(targets)
        if not _click_challenge_target(page, target):
            _capture(page, args, "challenge-option-failed")
            log.error("could not select the passcode delivery option. Retry "
                      "with vnc-login. (DOM captured with --debug.)")
            return False
        # The COARSE kind, never `display`: that label is the masked
        # delivery destination, and this line lands in a run log that gets
        # pasted around (the rule `describe_screen` states).
        picked = target.kind
    # else: the code-entry screen is already up (a single destination
    # auto-sends), which the wait above also accepts.
    if not wait_for(lambda: len(_otp_boxes(page)) == amexclient.OTP_DIGITS,
                    page, 30):
        _capture(page, args, "challenge-no-code-boxes")
        log.error("the passcode entry screen did not appear. Retry with "
                  "vnc-login. (DOM captured with --debug.)")
        return False
    if picked:
        log.info("passcode sent via %s", picked)
    _capture(page, args, "challenge-entercode")
    # The challenge ends with a second logon call; drop the first one's
    # verdict so the poll below reads the new one.
    watch.reset()
    code = auth_dialog.read_otp()
    if not enter_otp(page, code):
        _capture(page, args, "challenge-code-entry-failed")
        log.error("could not submit the passcode. Retry with vnc-login. "
                  "(DOM captured with --debug.)")
        return False
    register_device(page)
    _capture(page, args, "challenge-submitted")
    return True


# --- the shared authenticate() -------------------------------------------

def authenticate(context, page, watch: _LogonWatch, *, two_factor: str,
                 mfa_timeout: int = 600, args=None) -> bool:
    """Take a submitted login to an authenticated session.

    Prefill + submit are the caller's job (drive_to_auth); this polls for the
    logon outcome and finishes it:

    * trusted device (signed in outright) → return True, in every mode;
    * challenge + `cli` → drive the passcode screens from the terminal;
    * challenge + `vnc` → wait for the human to complete it in the browser;
    * challenge + `none` → raise NeedsLogin (download's case);
    * an outright refusal → raise LogonFailed carrying the provider's words;
    * no outcome within the window → return False.
    """
    state, failure, last_probe = None, None, 0.0
    deadline = time.monotonic() + OUTCOME_TIMEOUT_S
    while time.monotonic() < deadline:
        now = time.monotonic()
        # One read per tick. The verdict is a single value, and deriving
        # three answers from it read it three times — harmless while the
        # watcher can only advance inside a Playwright call, and a trap the
        # day that stops being true.
        o = watch.outcome()
        if o is not None:
            if o.authenticated:
                state = "authed"
                break
            if o.needs_challenge:
                state = "challenge"
                break
            failure = o
            break
        # The REST probe backs up a dropped response event, but ONLY once
        # off the sign-in flow: fired mid-challenge it poisons the challenge
        # server-side (the firstcitizens root cause).
        if now - last_probe >= 3 and not _on_login_flow(page):
            last_probe = now
            if probe_authenticated(context):
                state = "authed"
                break
        pump(page)

    if failure is not None:
        if failure.status_code is None:
            # A logon POST whose body carried no verdict. Never seen — every
            # observed response is JSON with a statusCode — so say what was
            # actually observed rather than blaming the credentials.
            raise LogonFailed(
                f"could not read the sign-in outcome: the logon responded "
                f"HTTP {failure.status} with no statusCode. Retry with "
                f"--debug for a DOM capture.")
        raise LogonFailed(
            f"American Express refused the sign-in "
            f"(HTTP {failure.status}, statusCode {failure.status_code}, "
            f"errorCode {failure.error_code or '-'})"
            + (f": {failure.error_message}" if failure.error_message else ""))

    if state is None:
        log.error("no login outcome after submit (neither signed in nor a "
                  "passcode challenge within %ds). Retry with --debug for a "
                  "DOM capture, or vnc-login.", OUTCOME_TIMEOUT_S)
        return False

    if state == "authed":
        log.info("authenticated (device trusted — no passcode needed)")
        return True

    # state == "challenge"
    if two_factor == TWOFACTOR_NONE:
        raise NeedsLogin(
            "device trust has expired — the sign-in hit a one-time-passcode "
            "challenge, which this run does not answer from the terminal. "
            "Re-run `download` from one (without --no-cli-mfa) — it answers "
            "the passcode and "
            "re-registers the device while it is there — or `vnc-login` to "
            "answer it by hand.")

    if two_factor == TWOFACTOR_CLI:
        try:
            if not _drive_challenge_cli(page, watch, args):
                return False
        except auth_dialog.ChallengeError as exc:
            log.error("2FA dialog ended: %s — retry with vnc-login.", exc)
            return False
    else:   # TWOFACTOR_VNC
        log.info("a one-time passcode is required — complete the challenge "
                 "in the browser over VNC (up to %ds), including \"Add This "
                 "Device\" so later runs skip it.", mfa_timeout)

    if not wait_for(lambda: _authed_cheap(watch)
                    or (not _on_login_flow(page)
                         and probe_authenticated(context)),
                    page, mfa_timeout):
        log.error("passcode submitted but no authenticated session appeared")
        return False
    log.info("2FA complete — session authenticated")
    return True


# --- verbs ----------------------------------------------------------------

_INPUT_TAG_RE = re.compile(r"""<input\b(?:[^>"']|"[^"]*"|'[^']*')*>""", re.I)
_VALUE_ATTR_RE = re.compile(r"""\bvalue\s*=\s*("[^"]*"|'[^']*'|[^\s>]*)""",
                            re.I)


def blank_otp(html: str) -> str:
    """Blank the passcode boxes out of a serialized DOM.

    A one-time passcode is a live credential while it is valid, and two of
    the captures below are taken after it has been typed. `scrub_dom`'s
    password rule cannot reach it: the six boxes are plain text inputs, and
    no value-based masker can either, because each holds a single digit. So
    they are blanked by the id they carry.
    """
    def blank(match):
        tag = match.group(0)
        if "otp-input" not in tag.lower():
            return tag
        return _VALUE_ATTR_RE.sub('value=""', tag)
    return _INPUT_TAG_RE.sub(blank, html) if html else html


def _capture(page, args, name: str) -> None:
    """DOM + screenshot to --screenshot-dir, only under --debug (for pinning
    drifted selectors). The credentials and any entered passcode are masked
    out of the markup on the way (collectorkit.debugcap + blank_otp)."""
    if not getattr(args, "debug", False):
        return
    mask = debugcap.secret_redactor(os.environ.get(USER_ENV, ""),
                                    os.environ.get(PASS_ENV, ""))
    with contextlib.suppress(Exception):
        debugcap.capture_page(page, args.screenshot_dir, name, log=log,
                              redact=lambda html: blank_otp(mask(html)))


def _load_credentials(env_file: Path) -> tuple[str, str]:
    if envfile.source_env_file(env_file):
        log.info("env file: %s (sourced)", env_file)
    username = os.environ.get(USER_ENV, "")
    password = os.environ.get(PASS_ENV, "")
    if not (username and password):
        log.warning("%s / %s not set — the form will not be pre-filled.",
                    USER_ENV, PASS_ENV)
    return username, password


def _reveal_login_form(page) -> None:
    """Click the homepage "Log In" link. Best-effort: if the link isn't there
    (a layout change, or already on the sign-in page), the prefill poll below
    still finds the fields once they render."""
    with contextlib.suppress(Exception):
        trigger = page.locator(amexclient.SEL_LOGIN_TRIGGER).first
        if trigger.count():
            trigger.click(timeout=5000)


def _prefill(page, username: str, password: str) -> bool:
    """Poll explore's frame-aware prefill until the form is filled.

    `overwrite=True`: on a trusted device the form arrives carrying a MASKED
    user id, and submitting that works only while the trust holds
    (DESIGN.md §G), so the real username is written over it."""
    prefilled: set = set()
    for _ in range(12):
        if _maybe_prefill_login(page, username, password, prefilled,
                                overwrite=True):
            return True
        pump(page, 1000)
    return False


def _submit(page) -> bool:
    """Submit the sign-in form — click the submit button, falling back to
    Enter in the password field."""
    with contextlib.suppress(Exception):
        btn = page.locator(amexclient.SEL_SUBMIT).first
        if btn.count():
            btn.click(timeout=5000)
            return True
    with contextlib.suppress(Exception):
        pwd = page.locator(amexclient.SEL_PASSWORD).first
        if pwd.count():
            pwd.press("Enter")
            return True
    return False


def _prefill_and_submit(page, args) -> bool:
    """Front half of every login: open the form, prefill, submit."""
    username, password = _load_credentials(args.env_file)
    page.goto(amexclient.START_URL, wait_until="domcontentloaded",
              timeout=45_000)
    _reveal_login_form(page)
    if username and password and _prefill(page, username, password):
        log.info("sign-in form pre-filled")
    _capture(page, args, "signin")
    if not _submit(page):
        _capture(page, args, "submit-failed")
        log.error("could not submit the sign-in form. Retry with vnc-login. "
                  "(DOM captured with --debug.)")
        return False
    log.info("submitted — waiting for the login outcome")
    return True


def drive_to_auth(context, page, args, *, two_factor: str) -> bool:
    """Prefill + submit + authenticate. Returns True on an authenticated
    session. Raises NeedsLogin when an unattended run (`two_factor=none`)
    meets a challenge, LogonFailed when the provider refuses outright."""
    watch = _LogonWatch().attach(context)
    if not _prefill_and_submit(page, args):
        return False
    ok = authenticate(context, page, watch, two_factor=two_factor,
                      mfa_timeout=args.mfa_timeout, args=args)
    _capture(page, args, "post-auth" if ok else "auth-failed")
    return ok


# Firefox's own cookie jar inside the persistent profile.
COOKIE_DB = "cookies.sqlite"


def _cookie_expiry(raw) -> int | None:
    """`moz_cookies.expiry` as epoch SECONDS, or None for a session cookie.

    Some Firefox builds store the column in milliseconds, which read as
    seconds lands tens of thousands of years out; a value too large to be
    seconds is divided down rather than printed."""
    try:
        value = int(raw or 0)
    except (TypeError, ValueError):
        return None
    if value <= 0:
        return None
    if value > 100_000_000_000:     # too large for seconds → milliseconds
        value //= 1000
    return value


def profile_trust_cookie(profile_dir: Path) -> dict | None:
    """The persisted device-trust cookie, read from the profile on disk.

    `device-id` is what skips the passcode across browser restarts
    (DESIGN.md §G). It is persistent — about a year — so it is written to
    the jar; the `Discard`-scoped session cookies beside it never reach the
    file at all, which is exactly the distinction the probe wants.

    Reads the file rather than opening a browser on it: launching Camoufox
    resolves the egress IP over the network, and creates or relinks profile
    contents, neither of which a probe advertised as free may do. A missing
    profile or jar is simply "not registered".

    The jar is copied with its `-wal` / `-shm` siblings and opened
    read-only, so a browser holding the write lock cannot block the read and
    pending WAL writes are still seen. Returns {"value", "expires"} or None.

    The host match is anchored to the brand's own domain and its subdomains.
    An unanchored suffix would also match a lookalike host, letting anything
    the profile ever visited mint its own "trust" — and a false REGISTERED
    sends an unattended run into a passcode nobody is there to answer.
    """
    db = Path(profile_dir) / COOKIE_DB
    if not db.is_file():
        return None
    tmp = Path(tempfile.mkdtemp(prefix="amex-check-"))
    try:
        for ext in ("", "-wal", "-shm"):
            src = Path(str(db) + ext)
            if src.exists():
                shutil.copy2(src, tmp / (COOKIE_DB + ext))
        conn = sqlite3.connect(f"file:{tmp / COOKIE_DB}?mode=ro", uri=True)
        try:
            row = conn.execute(
                "SELECT value, expiry FROM moz_cookies WHERE name=? AND "
                "(host = 'americanexpress.com' OR "
                "host LIKE '%.americanexpress.com') "
                "ORDER BY expiry DESC LIMIT 1",
                (amexclient.DEVICE_TRUST_COOKIE,)).fetchone()
        finally:
            conn.close()
    except sqlite3.Error as exc:
        log.warning("could not read the profile cookie jar (%s): %s",
                    db, exc)
        return None
    finally:
        shutil.rmtree(tmp, ignore_errors=True)
    if not row or not row[0]:
        # A cookie cleared to "" is the shape a revoked one takes in the
        # jar; reading it as trust would report a device that will be
        # challenged.
        return None
    return {"value": row[0], "expires": _cookie_expiry(row[1])}


def run_check(args: argparse.Namespace) -> int:
    """Report whether this device is registered — WITHOUT signing in.

    The fleet's usual `--check` makes the cheapest authenticated call the
    source allows, precisely so that a credential which exists but is
    rejected is caught. Here that call would be a full sign-in, and sign-ins
    are the scarce thing this whole design is arranged around (DESIGN.md §K):
    a probe that spends one to answer "can I spend one?" is self-defeating.

    So this reads the profile's own `device-id` cookie out of the Firefox
    jar on disk and reports THAT — no browser, no navigation, no network, no
    MFA. Opening a browser would not be free either: the shared launcher
    resolves the egress IP over the network at launch, so the "touches no
    network" claim only holds while nothing here starts one.

    It answers "is this device registered", which is the question that
    decides whether `download` runs unattended. It does NOT answer "will the
    next sign-in succeed": the provider can revoke trust server-side, and
    only a sign-in would see that. The limit is real, it is stated here and
    in the help, and it is the honest trade for not spending a sign-in on a
    question.

    Exit 0 = registered; 1 = not (the next `download` will be challenged).
    """
    cookie = profile_trust_cookie(args.profile_dir)
    if cookie is None:
        log.info("device NOT registered — no %s cookie in %s. The next "
                 "`download` will be challenged; run it with a terminal "
                 "so it can answer, or `vnc-login` by hand.",
                 amexclient.DEVICE_TRUST_COOKIE, args.profile_dir)
        return 1
    expires = cookie.get("expires")
    when = ""
    if expires:
        when = (" until " + datetime.fromtimestamp(expires, timezone.utc)
                .strftime("%Y-%m-%d"))
    log.info("device REGISTERED%s — `download` should sign in without a "
             "passcode. (Read from the profile: the provider can still "
             "revoke trust server-side, which only a sign-in would see.)",
             when)
    return 0


def parse_args(argv: list[str]) -> argparse.Namespace:
    """The `login --check` surface, and nothing else.

    Everything that signs in lives on `download` now (see the module
    docstring), so this parser carries only what the probe reads. `--check`
    is required rather than defaulted: a bare `login` is trapped as a no-op
    by the wrapper and the entrypoint, and reaching this parser without it
    means something addressed login.py directly expecting a verb that no
    longer exists."""
    p = argparse.ArgumentParser(
        description=__doc__.strip().splitlines()[0],
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument("--profile-dir", type=Path, default=DEFAULT_PROFILE_DIR,
                   help="Persistent Camoufox profile dir (holds the "
                        "device-trust cookie). Default: %(default)s.")
    p.add_argument("--check", action="store_true",
                   help="Report whether this device is registered, read from "
                        "the profile's own device-trust cookie. Opens no "
                        "browser and signs in to nothing: no network, no "
                        "passcode, no MFA. Exit 0 = "
                        "registered. It cannot see a trust the provider "
                        "revoked server-side — only a sign-in would, and on "
                        "this source sign-ins are the scarce thing.")
    p.add_argument("--debug", action="store_true",
                   help="DEBUG-level logging.")
    cli.add_standard_args(p, verb="login")
    return p.parse_args(argv)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose or args.debug)
    if not args.check:
        log.error("amex: `login` folds into `download` — the sign-in budget "
                  "is too small to spend one on a separate verb (DESIGN.md "
                  "§L). Run `download`, or `login --check` to report whether "
                  "this device is registered.")
        return 2
    return run_check(args)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
