#!/usr/bin/env python3
"""fidelity-web discovery harness — Donor-Advised Fund (Fidelity
Charitable) surface.

Launches Camoufox in the container's Xvfb display on the standard
Fidelity signin page and records every action taken in the VNC session,
so the DAF walk phases can be written from real traces. The scripted
`download` walk never visits the DAF (it is auto-excluded by account-id
length — DESIGN.md §1.2/§3.1); this harness exists to map that surface,
which sits behind a different web UI (expected: an SSO hop from the
account selector's `Fidelity Charitable® Giving` link into the Fidelity
Charitable portal — observed host and chain are §12 open questions).

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
from urllib.parse import urlparse

from collectorkit import cli, debugcap, explore

log = logging.getLogger("fidelity-web.explore")

# The standard signin — the same entry download.py drives. The DAF hop
# is taken from the post-auth account selector, so recording starts on
# known ground and captures the full redirect chain from there. --url
# can point straight at the DAF portal once a capture names its host.
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
# lands. Which of these actually serves the DAF UI is a §12 open
# question — the gate is deliberately wide so the answer is captured
# rather than filtered out.
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
    "input#userId-input, "
    "input[autocomplete='username'], "
    "input[name='userId'], "
    "input[aria-labelledby='dom-username-label'], "
    "input[name*='user' i], "
    "input[id*='user' i]"
)
USER_SELECT_SELECTOR = "select#dom-select-username"
PWD_SELECTOR = "#dom-pswd-input, input[type='password']"

# Init script. Two responsibilities:
#   1. Click recorder — log every DOM click to console.log() so the
#      Python side picks it up via page.on('console'). Necessary because
#      VNC mouse events bypass Playwright's action API. Password-input
#      values are redacted before logging.
#   2. Login-form + OTP detectors — once a fidelity.com frame renders a
#      login form (SPA: may be after the initial DOM), signal Python via
#      a console.log() sentinel so the Python side can fill it.
#      Host-gated so neither detector can fire on a third-party frame.
#      Init scripts run in every frame, so an embedded sign-in iframe is
#      covered too.
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
    // Redact password-input values so they never end up in the
    // trace / click log.
    const safeText = (t && t.tagName === 'INPUT' && t.type === 'password')
      ? '<redacted>'
      : (t.innerText || t.value || '').toString().slice(0, 80);
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
    console.log('__FIDELITY_EVENT__ ' + JSON.stringify(data));
  }, true);

  // Login-form + OTP detectors. Host-gated to fidelity.com frames.
  if (!/(^|\.)fidelity\.com$/i.test(location.hostname)) return;
  const USER_SEL =
    "input#userId-input, input[autocomplete='username'], " +
    "input[name='userId'], input[aria-labelledby='dom-username-label'], " +
    "input[name*='user' i], input[id*='user' i], select#dom-select-username";
  const detect = () => {
    const user = document.querySelector(USER_SEL);
    const pwd = document.querySelector(
      "#dom-pswd-input, input[type='password']");
    const sig = (user ? 'u' : '') + (pwd ? 'p' : '');
    if (sig && sig !== window.__FIDELITY_LOGIN_SIG__) {
      window.__FIDELITY_LOGIN_SIG__ = sig;
      console.log('__FIDELITY_EVENT__ ' + JSON.stringify({
        kind: 'login-form-detected',
        fields: sig,
        ts: new Date().toISOString(),
        url: location.href,
      }));
    }
    // OTP / one-time-code field detector — emits the field's STATIC
    // descriptor (never its value) once it mounts, so the 2FA surface
    // is recorded even when no click lands on the field itself. The
    // known field is #dom-totp-security-code-input; the generic
    // conventions cover drift and any DAF-side challenge.
    const otp = document.querySelector(
      "#dom-totp-security-code-input, " +
      "input[autocomplete='one-time-code'], input[inputmode='numeric'], " +
      "input[name*='otp' i], input[name*='code' i], input[id*='otp' i], " +
      "input[id*='code' i], input[maxlength='6'], input[maxlength='8']"
    );
    if (otp && !window.__FIDELITY_OTP_DETECTED__) {
      window.__FIDELITY_OTP_DETECTED__ = true;
      console.log('__FIDELITY_EVENT__ ' + JSON.stringify({
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
  // MutationObserver covers SPAs that mount the form after initial
  // load. Attached to documentElement so the whole subtree is seen.
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
                         filled: set) -> bool:
    """Fill the username + password fields of a Fidelity login form, each
    AT MOST ONCE per page (tracked in `filled`, keyed by (id(page), kind)).

    Frame-aware and host-gated to fidelity.com, so the credentials can
    never touch a third-party frame. The password is filled only when a
    username control co-exists in the SAME frame — either a text input
    (fresh-device form) or the remember-my-username <select> (returning
    device), whose remembered value is left alone. A lone password-shaped
    field (a 2FA code entry) is never touched. Each fill is verified by
    read-back, with one clear-and-retry, so a stray autofill can never
    concatenate onto a correct value; the field is marked done regardless
    of the outcome so later DOM mutations never trigger a re-fill that
    fights a hand-typed value. A field that already has content is marked
    done untouched. Never submits — Sign in + 2FA are driven manually in
    the VNC session. Returns True iff at least one field was newly filled
    and verified."""
    did = False
    for frame in page.frames:
        try:
            host = urlparse(frame.url).hostname or ""
        except Exception:
            continue
        if not PREFILL_HOST_RE.search(host):
            continue
        try:
            user_field = frame.locator(USER_SELECTOR).first
            user_select = frame.locator(USER_SELECT_SELECTOR).first
            pwd_field = frame.locator(PWD_SELECTOR).first
            # count() / is_visible() are instant (no auto-wait), so a
            # frame without the form costs nothing per poll instead of
            # blocking on a timeout.
            if pwd_field.count() == 0 or not pwd_field.is_visible():
                continue
            has_user_input = (user_field.count() > 0
                              and user_field.is_visible())
            has_user_select = (user_select.count() > 0
                               and user_select.is_visible())
            if not (has_user_input or has_user_select):
                continue
        except Exception as exc:
            log.debug("prefill frame probe failed: %r", exc)
            continue
        targets = [("pwd", pwd_field, password)]
        if has_user_input:
            targets.insert(0, ("user", user_field, username))
        for kind, field, value in targets:
            key = (id(page), kind)
            if key in filled:
                continue
            try:
                if field.input_value(timeout=1000):
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
                log.debug("prefill %s field failed: %r", kind, exc)
        # The first fidelity.com frame carrying the form is the login
        # form; other frames don't get a second pass.
        break
    return did


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
            prefill=(lambda page: _maybe_prefill_login(
                page, username, password, filled)) if prefill else None)
        session.open(args.url)
        session.record(WALK, max_duration=args.max_duration)
    session.report()
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
