#!/usr/bin/env python3
"""First Citizens login driver — the interactive `login` verb, and the
shared authenticate() the unattended `download` reuses.

The login is a Q2 single-page app. The homepage carries the sign-in form in
a modal (opened by a "Log In" button); submitting it navigates into the SPA
(`uux.aspx#/login`), which makes the Akamai-sensor-bearing
`preLogonUser`/`logonUser` calls (DESIGN.md §3) and then either lands
signed-in or presents a Secure Access Code (2FA) challenge:

* **Trusted device** — the SPA routes straight to `#/landingPage`
  (`logonUser` 200, no 2FA). `download` relies on this: a persisted Camoufox
  profile skips 2FA on later runs.
* **Untrusted device** — the SPA routes to `#/login/mfa/*` (a Secure Access
  Code by text or call). `login` drives those Q2/Stencil screens **from the
  terminal** (`auth_dialog.py` picks the delivery method, reads the code on
  stdin), then registers the device so future runs land on the trusted
  path; `vnc-login` completes the same 2FA by hand as a fallback.
  `download`, which has no terminal, raises `NeedsLogin` here.

Authentication is detected from the **live SPA route**, never a bare URL and
never a single response event: the pinned Camoufox can drop `.on()` events
across navigations (the chase/schwab-web lesson), so the signal is
`page.url` reaching `#/landingPage` (authed) or `#/login/mfa/*` (2FA),
polled. A `GET accounts` probe backs it up only *off* the login flow — never
mid-login, where it would poison the challenge session (DESIGN.md §4.2).

Read-only (CLAUDE.md): the browser only ever touches the sign-in form and
the access-code challenge; the deposit-account data is fetched over REST by
download.py. Never a money-movement, card, or settings surface.
"""
from __future__ import annotations

import argparse
import contextlib
import logging
import os
import shutil
import sys
import time
from pathlib import Path

from collectorkit import cli, debugcap, envfile, launch, session
from collectorkit.launch import page_url, pump, wait_for

import auth_dialog
import q2client
# Reuse explore's validated, frame-aware, origin-gated login pre-fill rather
# than a second copy — same collector, same selectors.
from explore import _maybe_prefill_login, USER_ENV, PASS_ENV

log = logging.getLogger("firstcitizens.login")

DEFAULT_PROFILE_DIR = Path("/secrets/firstcitizens-profile")
DEFAULT_ENV_FILE = Path("/secrets/firstcitizens.env")
DEFAULT_STATE_PATH = Path("/secrets/firstcitizens-state.json")

# The three ways a 2FA challenge is answered (DESIGN.md §4.2):
TWOFACTOR_CLI = "cli"     # drive the Q2 MFA screens, code read from the terminal
TWOFACTOR_VNC = "vnc"     # human completes the Secure Access Code over VNC
TWOFACTOR_NONE = "none"   # unattended download: a challenge is a hard error

# How long to wait for the SPA to reach a login outcome (authed or MFA)
# after the form is submitted.
OUTCOME_TIMEOUT_S = 120


class NeedsLogin(RuntimeError):
    """Raised by authenticate(two_factor='none') when the device is no longer
    trusted and a 2FA challenge appeared — `download` cannot answer it, so it
    surfaces this to tell the operator to run `login`."""


# --- browser plumbing -----------------------------------------------------

@contextlib.contextmanager
def camoufox(profile_dir: Path, fresh: bool = False):
    """Open a persistent Camoufox context on the profile dir (headed under
    the entrypoint's Xvfb; the container exposes VNC only for vnc-login). The
    persistent profile is what carries the device-trust across runs
    (DESIGN.md §3). `fresh` wipes it first, so the next logon is treated as an
    unrecognised device and the full Secure Access Code (2FA) challenge fires.
    Yields (context, page)."""
    if fresh and profile_dir.exists():
        log.warning("--fresh: wiping profile dir %s — the next login will "
                    "hit the full 2FA challenge (untrusted device)",
                    profile_dir)
        shutil.rmtree(profile_dir)
    with launch.persistent_camoufox(profile_dir) as (context, page):
        yield context, page


class _LogonWatch:
    """Capture the `logonUser` response (status + parsed body) as the SPA
    fires it — a secondary signal for authenticate(). It is *not* relied on
    alone: the pinned Camoufox can drop the event, so the SPA route + a REST
    probe are the primary signals."""

    def __init__(self):
        self.status: int | None = None
        self.body: dict | None = None

    def attach(self, context):
        context.on("response", self._on_response)
        return self

    def _on_response(self, resp):
        try:
            if resp.url.rstrip("/").endswith("/logonUser"):
                if resp.request.method.upper() == "OPTIONS":
                    # A CORS preflight to the same URL, answering 200 with an
                    # empty body — and classify_logon reads a 200 carrying no
                    # targets as AUTHENTICATED, so taking one for the outcome
                    # would declare a sign-in that never happened. The Q2 app
                    # is same-origin today and sends none; the guard costs
                    # nothing and the amex sibling hit exactly this live.
                    return
                self.status = resp.status
                with contextlib.suppress(Exception):
                    body = resp.json()
                    if isinstance(body, dict):
                        self.body = body
        except Exception:            # pragma: no cover — defensive
            pass

    def outcome(self) -> q2client.LogonOutcome | None:
        if self.status is None:
            return None
        return q2client.classify_logon(self.status, self.body or {})


# --- login-form driving ---------------------------------------------------

def _reveal_login_form(page) -> None:
    """Open the homepage login modal (the form is behind a "Log In" button).
    Best-effort: if the modal trigger isn't present (already on the SPA, or a
    layout change), the prefill poll below still finds the fields once
    visible."""
    with contextlib.suppress(Exception):
        trigger = page.locator(q2client.SEL_LOGIN_TRIGGER).first
        if trigger.count():
            trigger.click(timeout=5000)


def _prefill(page, username: str, password: str) -> bool:
    """Poll explore's frame-aware prefill until the modal form is filled (it
    mounts / becomes visible after the reveal click). Returns True once
    filled."""
    prefilled: set = set()
    for _ in range(12):
        if _maybe_prefill_login(page, username, password, prefilled):
            return True
        pump(page, 1000)
    return False


def _submit(page) -> bool:
    """Submit the login form — click the digital-banking submit button,
    falling back to Enter in the password field. The SPA then makes the
    sensor-bearing preLogonUser / logonUser calls."""
    with contextlib.suppress(Exception):
        btn = page.locator(q2client.SEL_SUBMIT).first
        if btn.count():
            btn.click(timeout=5000)
            return True
    with contextlib.suppress(Exception):
        pwd = page.locator(q2client.SEL_PASSWORD).first
        if pwd.count():
            pwd.press("Enter")
            return True
    return False


# --- REST over the browser context ---------------------------------------

def q2_headers(context) -> dict:
    """The `q2token` header the authenticated mobilews calls require, read
    from the session cookie of the same name (DESIGN.md §3). Accept-only if
    the cookie isn't set yet."""
    with contextlib.suppress(Exception):
        for c in context.cookies():
            if c.get("name") == q2client.Q2TOKEN and c.get("value"):
                return {q2client.Q2TOKEN: c["value"], "Accept": "application/json"}
    return {"Accept": "application/json"}


def q2_get_json(context, url: str) -> tuple[int, dict | None]:
    """GET `url` over the browser context (shares cookies) with the q2token
    header; return (status, parsed-json-or-None)."""
    resp = context.request.get(url, headers=q2_headers(context))
    return _resp_json(resp)


def _resp_json(resp) -> tuple[int, dict | None]:
    body = None
    with contextlib.suppress(Exception):
        parsed = resp.json()
        if isinstance(parsed, dict):
            body = parsed
    return resp.status, body


def _probe_authenticated(context) -> bool:
    """A REST auth probe that does not depend on any browser event: a
    `GET accounts` returns 200 with a data array only once the session is
    signed in (pre-auth it 401s / redirects)."""
    with contextlib.suppress(Exception):
        status, body = q2_get_json(context, q2client.accounts_url())
        return status == 200 and isinstance((body or {}).get("data"), list)
    return False


def _authed_cheap(page, watch: "_LogonWatch") -> bool:
    """The event-free, no-network half of the signed-in signal: a captured
    logonUser said authenticated, or the SPA route reached the landing page.
    Checked every poll tick (the REST probe is throttled separately)."""
    o = watch.outcome()
    if o is not None and o.authenticated:
        return True
    return q2client.is_authed_url(page_url(page))


def _is_authed(context, page, watch: "_LogonWatch") -> bool:
    """Full signed-in signal: the cheap check OR the REST probe succeeds."""
    return _authed_cheap(page, watch) or _probe_authenticated(context)


def _is_mfa(page, watch: "_LogonWatch") -> bool:
    """2FA-challenge signal: the SPA reached a /login/mfa route, OR a captured
    logonUser said a challenge is required."""
    o = watch.outcome()
    if o is not None and o.needs_2fa:
        return True
    return q2client.is_mfa_url(page_url(page))


# --- terminal 2FA: drive the Q2 Secure Access Code screens ----------------

def _click_q2btn(loc) -> bool:
    """Click a Q2/Stencil `q2-btn` by its **inner `<button>`** (the reliable
    clickable element), reached through the open shadow root, falling back to
    the host — the standard way to drive a Stencil button (the chase
    shadow-DOM lesson)."""
    with contextlib.suppress(Exception):
        inner = loc.locator("button").first
        if inner.count():
            inner.click(timeout=5000)
            return True
    with contextlib.suppress(Exception):
        loc.click(timeout=5000)
        return True
    return False


def _read_mfa_targets(page) -> tuple:
    """Read the delivery targets from the on-screen `btnTacTarget` buttons —
    the ground truth on the targets screen, robust to a dropped `logonUser`
    response event (the pinned Camoufox drops `.on()` events). Each target's
    `value` is its button index; `display` is the button's own (last-4-bearing)
    label; `kind` is sms/voice from the "Text:"/"Call:" prefix."""
    targets = []
    with contextlib.suppress(Exception):
        btns = page.locator(q2client.SEL_MFA_TARGET)
        for i in range(btns.count()):
            with contextlib.suppress(Exception):
                text = " ".join(btns.nth(i).inner_text().split())
                low = text.lower()
                kind = ("sms" if low.startswith("text")
                        else "voice" if low.startswith("call") else "other")
                targets.append(q2client.AccessCodeTarget(
                    value=str(i), display=text, kind=kind))
    return tuple(targets)


def _click_mfa_target(page, target) -> bool:
    """Click the chosen delivery button (`btnTacTarget`) — by its DOM index
    (the target's `value`, set by _read_mfa_targets), falling back to its
    "Text:"/"Call:" prefix. Clicks the inner `<button>` (Stencil), which
    sends the code and advances to the code-entry screen."""
    btns = page.locator(q2client.SEL_MFA_TARGET)
    with contextlib.suppress(Exception):
        n = btns.count()
        idx = None
        if target.value.isdigit() and int(target.value) < n:
            idx = int(target.value)
        else:
            prefix = q2client.MFA_TARGET_PREFIX.get(target.kind, "")
            for i in range(n):
                with contextlib.suppress(Exception):
                    if not prefix or prefix.lower() in btns.nth(i).inner_text().lower():
                        idx = i
                        break
        if idx is not None:
            return _click_q2btn(btns.nth(idx))
    return False


def _enter_mfa_code(page, code: str) -> bool:
    """Type the Secure Access Code into `#tacEntry` (its inner `<input>`) and
    click Submit. Returns True once both happen."""
    field = None
    for sel in (f"{q2client.SEL_MFA_CODE} input", q2client.SEL_MFA_CODE):
        loc = page.locator(sel).first
        with contextlib.suppress(Exception):
            if loc.count():
                field = loc
                break
    if field is None:
        return False
    try:
        field.click(timeout=4000)
        field.fill("")
        field.fill(code)
        if field.input_value() != code:            # Stencil rejected a bulk fill
            page.keyboard.type(code, delay=30)
    except Exception as exc:
        log.debug("code entry failed: %r", exc)
        return False
    return _click_q2btn(page.locator(q2client.SEL_MFA_SUBMIT).first)


def _register_device(page) -> None:
    """Click "Register Device" so this device is trusted on future runs. The
    step is optional — a failure here still leaves an authenticated session —
    so it is best-effort."""
    wait_for(lambda: q2client.URL_MARK_MFA_REGISTER in page_url(page)
             or page.locator(q2client.SEL_MFA_REGISTER).count(), page, 20)
    with contextlib.suppress(Exception):
        reg = page.locator(q2client.SEL_MFA_REGISTER).first
        if reg.count() and _click_q2btn(reg):
            log.info("registered this device for future unattended runs")


def _drive_mfa_cli(page, watch: "_LogonWatch", args) -> bool:
    """Drive the Q2 Secure Access Code screens from the terminal: pick the
    delivery method (read from the on-screen buttons), send the code, read it
    from stdin, submit it, and register the device. Selectors are pinned from
    the explore capture (DESIGN.md §3). Returns True once the code is
    submitted; the caller confirms the authenticated session."""
    # Wait for the delivery screen, then read the targets from the buttons
    # themselves (not the possibly-dropped logonUser response event).
    if not wait_for(lambda: page.locator(q2client.SEL_MFA_TARGET).count() > 0
                    or q2client.URL_MARK_MFA_ENTER in page_url(page), page, 30):
        _capture(page, args, "mfa-no-targets")
        log.error("the 2FA delivery screen did not appear. Retry with "
                  "vnc-login. (DOM captured with --debug.)")
        return False
    _capture(page, args, "mfa-targets")
    targets = _read_mfa_targets(page)
    picked = None
    if targets:
        target = auth_dialog.choose_target(targets)
        if not _click_mfa_target(page, target):
            _capture(page, args, "mfa-target-failed")
            log.error("could not select the 2FA delivery method on the page. "
                      "Retry with vnc-login. (DOM captured with --debug.)")
            return False
        picked = target.display or target.kind
    # else: a single target auto-sends and jumps straight to code entry.
    if not wait_for(lambda: q2client.URL_MARK_MFA_ENTER in page_url(page)
                    or page.locator(f"{q2client.SEL_MFA_CODE} input").count(),
                    page, 30):
        _capture(page, args, "mfa-no-code-field")
        log.error("the code-entry screen did not appear. Retry with "
                  "vnc-login. (DOM captured with --debug.)")
        return False
    if picked:
        log.info("access code sent via %s", picked)
    _capture(page, args, "mfa-entercode")
    code = auth_dialog.read_otp()
    if not _enter_mfa_code(page, code):
        _capture(page, args, "mfa-code-entry-failed")
        log.error("could not enter the Secure Access Code. Retry with "
                  "vnc-login. (DOM captured with --debug.)")
        return False
    _register_device(page)
    _capture(page, args, "mfa-submitted")
    return True


# --- the shared authenticate() -------------------------------------------

def authenticate(context, page, watch: "_LogonWatch", *, two_factor: str,
                 mfa_timeout: int = 600, args=None) -> bool:
    """Take a submitted login to an authenticated session.

    Reveal + prefill + submit are the caller's job (drive_to_auth); this polls
    the SPA route (with a REST probe only once off the login flow — never
    mid-login, §4.2) for the outcome and finishes it (DESIGN.md §4.2):

    * trusted device (lands signed-in) → return True, regardless of mode;
    * 2FA challenge + `cli` → drive the Q2 Secure Access Code screens,
      reading the code from the terminal, then confirm;
    * 2FA challenge + `vnc` → wait for the human to complete the Secure
      Access Code in the browser, then confirm;
    * 2FA challenge + `none` → raise NeedsLogin (download's case);
    * no outcome within the window → return False.
    """
    # Detect the outcome from the SPA route (free, event-safe). A REST
    # `GET /accounts` probe backs it up ONLY once off the login flow — firing
    # that probe while still on a `/login*` route poisons the challenge
    # session server-side, so the later Secure-Access-Code click sends but
    # the SPA never advances (root cause of the CLI-2FA no-navigation bug,
    # diagnosed 2026-08-15). So the probe is gated on having left `/login`.
    state, last_probe = None, 0.0
    deadline = time.monotonic() + OUTCOME_TIMEOUT_S
    while time.monotonic() < deadline:
        now = time.monotonic()
        if _authed_cheap(page, watch):
            state = "authed"
            break
        if _is_mfa(page, watch):
            state = "mfa"
            break
        if now - last_probe >= 3 and "/login" not in page_url(page):
            last_probe = now
            if _probe_authenticated(context):
                state = "authed"
                break
        pump(page)

    if state is None:
        log.error("no login outcome after submit (neither signed in nor a "
                  "2FA challenge within %ds). Retry with --debug for a DOM "
                  "capture, or vnc-login.", OUTCOME_TIMEOUT_S)
        return False

    if state == "authed":
        log.info("authenticated")
        return True

    # state == "mfa"
    if two_factor == TWOFACTOR_NONE:
        raise NeedsLogin(
            "device trust has expired — the login hit a Secure Access Code "
            "challenge. Run `login` to re-register this device.")

    if two_factor == TWOFACTOR_CLI:
        try:
            if not _drive_mfa_cli(page, watch, args):
                return False
        except auth_dialog.ChallengeError as exc:
            log.error("2FA dialog ended: %s — retry with vnc-login.", exc)
            return False
    else:   # TWOFACTOR_VNC
        log.info("Secure Access Code required — complete the 2FA in the "
                 "browser over VNC (up to %ds). It registers this device for "
                 "next time.", mfa_timeout)

    if not wait_for(lambda: _is_authed(context, page, watch), page, mfa_timeout):
        log.error("2FA submitted but no authenticated session appeared")
        return False
    log.info("2FA complete — session authenticated")
    return True


# --- verbs ----------------------------------------------------------------



def _capture(page, args, name: str) -> None:
    """DOM + screenshot to --screenshot-dir, only under --debug (for pinning
    drifted selectors). The credentials are masked out of the markup on the
    way, alongside the password-input blanking every capture gets."""
    if not getattr(args, "debug", False):
        return
    with contextlib.suppress(Exception):
        debugcap.capture_page(page, args.screenshot_dir, name, log=log,
                              redact=debugcap.env_redactor(USER_ENV, PASS_ENV))


def _load_credentials(env_file: Path) -> tuple[str, str]:
    if envfile.source_env_file(env_file):
        log.info("env file: %s (sourced)", env_file)
    username = os.environ.get(USER_ENV, "")
    password = os.environ.get(PASS_ENV, "")
    if not (username and password):
        log.warning("%s / %s not set — the form will not be pre-filled.",
                    USER_ENV, PASS_ENV)
    return username, password


def _reveal_prefill_submit(context, page, args) -> bool:
    """Front half of every login: open the modal, prefill, submit. Returns
    True once the form is submitted (a _LogonWatch is attached first so the
    SPA's logonUser is captured)."""
    username, password = _load_credentials(args.env_file)
    page.goto(q2client.START_URL, wait_until="domcontentloaded", timeout=45_000)
    _reveal_login_form(page)
    if username and password and _prefill(page, username, password):
        log.info("login form pre-filled")
    _capture(page, args, "signin")
    if not _submit(page):
        _capture(page, args, "submit-failed")
        log.error("could not submit the login form. Retry with vnc-login. "
                  "(DOM captured with --debug.)")
        return False
    log.info("submitted — waiting for the login outcome")
    return True


def drive_to_auth(context, page, args, *, two_factor: str) -> bool:
    """Reveal + prefill + submit + authenticate. Returns True on an
    authenticated session. Raises NeedsLogin when an unattended run
    (`two_factor=none`) meets a 2FA challenge (download's case)."""
    watch = _LogonWatch().attach(context)
    if not _reveal_prefill_submit(context, page, args):
        return False
    ok = authenticate(context, page, watch, two_factor=two_factor,
                      mfa_timeout=args.mfa_timeout, args=args)
    _capture(page, args, "post-auth" if ok else "auth-failed")
    return ok


def run_login(args: argparse.Namespace) -> int:
    """Interactive login: register the device (driving the Secure Access Code
    2FA from the terminal when the device isn't trusted, or by hand over VNC
    with --no-cli-mfa), leaving device-trust in the persistent profile for
    unattended `download` runs."""
    two_factor = TWOFACTOR_VNC if not args.cli_mfa else TWOFACTOR_CLI
    with camoufox(args.profile_dir, fresh=args.fresh) as (context, page):
        ok = drive_to_auth(context, page, args, two_factor=two_factor)
        if not ok:
            return 1
        session.save_state(args.state_path, {
            "saved_at": session.iso_now(),
            "source": "firstcitizens",
            "last_login": "ok",
        })
        log.info("login complete; state recorded at %s", args.state_path)
        return 0


def run_check(args: argparse.Namespace) -> int:
    """Probe whether the trusted-device logon still skips 2FA — submit the
    form and read the SPA outcome. No Secure Access Code is ever sent, so this
    fires no MFA (root CLAUDE.md §2). Exit 0 = trusted (signed in), 1 = 2FA
    now required or no session."""
    with camoufox(args.profile_dir) as (context, page):
        watch = _LogonWatch().attach(context)
        if not _reveal_prefill_submit(context, page, args):
            log.info("session state UNKNOWN — could not submit the form")
            return 1
        deadline = time.monotonic() + OUTCOME_TIMEOUT_S
        last_probe = 0.0
        while time.monotonic() < deadline:
            if _authed_cheap(page, watch):
                log.info("device TRUSTED — logon skipped 2FA")
                return 0
            if _is_mfa(page, watch):
                log.info("device trust EXPIRED — logon now requires 2FA "
                         "(run login)")
                return 1
            # Off the login flow only — probing /accounts mid-login poisons
            # the challenge (see authenticate).
            if time.monotonic() - last_probe >= 3 and "/login" not in page_url(page):
                last_probe = time.monotonic()
                if _probe_authenticated(context):
                    log.info("device TRUSTED — logon skipped 2FA")
                    return 0
            pump(page)
        log.info("session state UNKNOWN — no outcome within %ds",
                 OUTCOME_TIMEOUT_S)
        return 1


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip().splitlines()[0],
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument("--profile-dir", type=Path, default=DEFAULT_PROFILE_DIR,
                   help="Persistent Camoufox profile dir (holds the "
                        "device-trust). Default: %(default)s.")
    p.add_argument("--env-file", type=Path, default=DEFAULT_ENV_FILE,
                   help="Bash-sourced env file with FIRSTCITIZENS_USERNAME / "
                        "FIRSTCITIZENS_PASSWORD. Default: %(default)s.")
    p.add_argument("--state-path", type=Path, default=DEFAULT_STATE_PATH,
                   help="Where the login-success marker is written (0600). "
                        "Default: %(default)s.")
    p.add_argument("--check", action="store_true",
                   help="Probe whether the trusted-device logon still skips "
                        "2FA; exit 0/1. Sends no code, fires no MFA.")
    p.add_argument("--fresh", action="store_true",
                   help="Wipe the persistent Camoufox profile before "
                        "launching, so the login is treated as an "
                        "unrecognised device and the full Secure Access Code "
                        "(2FA) challenge fires — the untrusted-device flow. "
                        "Pair with vnc-login to complete that 2FA by hand. "
                        "Not allowed with --check (a probe never wipes "
                        "trust).")
    p.add_argument("--cli-mfa", dest="cli_mfa", action="store_true",
                   default=True,
                   help="Default. Drive 2FA from the terminal: on an "
                        "untrusted device it picks the delivery method and "
                        "reads the Secure Access Code from stdin (needs a "
                        "TTY). Trusted devices skip 2FA entirely.")
    p.add_argument("--no-cli-mfa", dest="cli_mfa", action="store_false",
                   help="Complete Sign In + 2FA by hand over VNC "
                        "(the vnc-login fallback).")
    p.add_argument("--mfa-timeout", type=int, default=600,
                   help="Seconds to wait for 2FA to complete over VNC. "
                        "Default: %(default)s.")
    p.add_argument("--debug", action="store_true",
                   help="Capture login DOM/screenshots into --screenshot-dir.")
    p.add_argument("--screenshot-dir", type=Path, default=Path("/debug"),
                   help="Where login diagnostics land (outside bronze). "
                        "Default: %(default)s.")
    cli.add_common_args(p)
    return p.parse_args(argv)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose or args.debug)
    if args.check:
        if args.fresh:
            log.error("--fresh cannot be combined with --check "
                      "(a read-only probe never wipes device trust).")
            return 2
        return run_check(args)
    return run_login(args)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
