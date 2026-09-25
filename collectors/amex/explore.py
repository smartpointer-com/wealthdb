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

This is read-only observation. Per CLAUDE.md, never click a Pay /
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
from urllib.parse import urlparse

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
PWD_SELECTOR = "input[type='password']"

# Init script. Two responsibilities:
#   1. Click recorder — log every DOM click to console.log() so the Python
#      side picks it up via page.on('console'). Necessary because VNC mouse
#      events bypass Playwright's action API. Password-input values are
#      redacted before logging.
#   2. Login-form + OTP detectors — once an americanexpress.com frame
#      renders a login form (SPA: may be after the initial DOM), signal
#      Python via a console.log() sentinel so the Python side can fill it.
#      Host-gated so neither detector can fire on a third-party frame;
#      there is no path gate: the form is reached from the homepage's own
#      login control (§B), so no single route identifies it — the Python
#      side only fills when username + password fields co-exist in one
#      frame, which is what keeps credentials out of a lone field like the
#      2FA code entry. Init scripts run in every frame, so a sign-in iframe
#      is covered too.
CLICK_RECORDER_JS = r"""
(() => {
  const xpathOf = (el) => {
    if (!el) return '';
    const segs = [];
    while (el && el.nodeType === Node.ELEMENT_NODE) {
      let i = 1, sib = el.previousElementSibling;
      while (sib) {
        if (sib.tagName === el.tagName) i++;
        sib = sib.previousElementSibling;
      }
      segs.unshift(el.tagName.toLowerCase() + '[' + i + ']');
      el = el.parentElement;
    }
    return '/' + segs.join('/');
  };
  document.addEventListener('click', (e) => {
    const t = e.target;
    // Never log a form VALUE. innerText is empty on INPUT/TEXTAREA, so a
    // button or link keeps its label while a field can contribute nothing
    // — which is what keeps a filled username, or a password field a
    // show-password toggle flipped to type=text, out of the click log.
    const safeText = (t && t.tagName === 'INPUT' && t.type === 'password')
      ? '<redacted>'
      : (t.innerText || '').toString().slice(0, 80);
    const data = {
      kind: 'click',
      ts: new Date().toISOString(),
      url: location.href,
      tag: t.tagName,
      id: t.id || null,
      cls: (typeof t.className === 'string' ? t.className : null),
      text: safeText,
      xpath: xpathOf(t),
      x: e.clientX, y: e.clientY,
    };
    console.log('__AMEX_EVENT__ ' + JSON.stringify(data));
  }, true);

  // Login-form + OTP detectors. Host-gated to americanexpress.com frames.
  if (!/(^|\.)americanexpress\.com$/i.test(location.hostname)) return;
  const USER_SEL =
    "input[autocomplete='username'], input[name*='user' i], " +
    "input[id*='user' i], input[name*='login' i], input[id*='login' i], " +
    "input[type='email'], input[autocomplete='email']";
  const detect = () => {
    const user = document.querySelector(USER_SEL);
    const pwd = document.querySelector("input[type='password']");
    const sig = (user ? 'u' : '') + (pwd ? 'p' : '');
    if (sig && sig !== window.__AMEX_LOGIN_SIG__) {
      window.__AMEX_LOGIN_SIG__ = sig;
      console.log('__AMEX_EVENT__ ' + JSON.stringify({
        kind: 'login-form-detected',
        fields: sig,
        ts: new Date().toISOString(),
        url: location.href,
      }));
    }
    // OTP / one-time-code field detector — emits the field's STATIC
    // descriptor (never its value) once it mounts, so the 2FA surface is
    // recorded even when no click lands on the field itself.
    const otp = document.querySelector(
      "input[autocomplete='one-time-code'], input[inputmode='numeric'], " +
      "input[name*='otp' i], input[name*='code' i], input[id*='otp' i], " +
      "input[id*='code' i], input[maxlength='6'], input[maxlength='8']"
    );
    if (otp && !window.__AMEX_OTP_DETECTED__) {
      window.__AMEX_OTP_DETECTED__ = true;
      console.log('__AMEX_EVENT__ ' + JSON.stringify({
        kind: 'otp-field-detected',
        tag: otp.tagName,
        id: otp.id || null,
        name: otp.getAttribute('name'),
        type: otp.getAttribute('type'),
        placeholder: otp.getAttribute('placeholder'),
        inputmode: otp.getAttribute('inputmode'),
        autocomplete: otp.getAttribute('autocomplete'),
        maxlength: otp.getAttribute('maxlength'),
        ts: new Date().toISOString(),
        url: location.href,
      }));
    }
  };
  detect();
  // MutationObserver covers SPAs that mount the form after initial load.
  // We attach to documentElement so we see the whole subtree.
  const target = document.documentElement || document;
  if (target && typeof MutationObserver !== 'undefined') {
    new MutationObserver(detect).observe(target,
      {childList: true, subtree: true});
  }
})();
"""


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
    """Fill the username + password fields of an Amex login form, each AT
    MOST ONCE per page (tracked in `filled`, keyed by (id(page), kind)).

    Frame-aware: the form is looked for in every frame whose host is under
    americanexpress.com — covering both a form in the main frame and one
    embedded in a same-brand iframe (the site uses the main frame, §B; the
    iframe path is kept as cheap defence). Both fields must co-exist in the
    SAME frame before either is touched, so the password can never land in
    a standalone field (a 2FA code entry is a lone input). Each fill is
    verified by read-back, with one clear-and-retry, so a stray autofill
    can never concatenate onto a correct value; the field is marked done
    regardless of the outcome so later DOM mutations never trigger a
    re-fill that fights a hand-typed value.

    `overwrite` decides what happens to a field that ALREADY has content.
    Explore leaves it alone (the default): the human may have typed it, and
    fighting that is worse than skipping. login.py sets it, because on a
    trusted device Amex pre-fills a MASKED user id (DESIGN.md §G) —
    submitting that works only while the device trust holds, so the real
    `AMEX_USERNAME` is written over it deliberately rather than inherited.

    Never submits: explore leaves Sign in + 2FA to the VNC session, and
    login.py, which reuses this fill, submits separately. Returns True iff
    at least one field was newly filled and verified."""
    did = False
    for frame in page.frames:
        try:
            host = urlparse(frame.url).hostname or ""
        except Exception:
            continue
        if not HOST_RE.search(host):
            continue
        try:
            user_field = frame.locator(USER_SELECTOR).first
            pwd_field = frame.locator(PWD_SELECTOR).first
            # count() / is_visible() are instant (no auto-wait), so a
            # frame without the form costs nothing per poll instead of
            # blocking on a timeout.
            if user_field.count() == 0 or pwd_field.count() == 0:
                continue
            if not (user_field.is_visible() and pwd_field.is_visible()):
                continue
        except Exception as exc:
            log.debug("prefill frame probe failed: %r", exc)
            continue
        for kind, field, value in (("user", user_field, username),
                                   ("pwd", pwd_field, password)):
            key = (id(page), kind)
            if key in filled:
                continue
            try:
                if field.input_value(timeout=1000) and not overwrite:
                    # Hand-typed (or already pre-filled) — leave it alone.
                    filled.add(key)
                    continue
                field.fill(value, timeout=2000)
                got = field.input_value(timeout=1500)
                if got != value:
                    field.fill("", timeout=1500)
                    field.fill(value, timeout=2000)
                    got = field.input_value(timeout=1500)
                filled.add(key)
                if got == value:
                    did = True
                else:
                    log.warning(
                        "pre-fill of %s field did not stick (got len %d, "
                        "want %d) — type it manually in the VNC session",
                        kind, len(got or ""), len(value))
            except Exception as exc:
                # safe_error, never %r: a failed fill() renders the typed
                # value verbatim inside Playwright's Call log block, so
                # --debug would print the password to stderr.
                log.debug("prefill %s field failed: %s", kind,
                          debugcap.safe_error(exc))
        # The first americanexpress.com frame carrying both fields is the
        # login form; other frames (marketing embeds) don't get a second
        # pass.
        break
    return did


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
