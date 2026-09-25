#!/usr/bin/env python3
"""equityzen discovery harness.

Launches Camoufox in the container's Xvfb display on the EquityZen
investor portal and records the session driven over VNC — the HAR, the
network and click logs, and downloads (the artefacts are described in
collectorkit.explore) — so login.py and download.py can be written from
real traces. EquityZen's backend is Django + GraphQL (the public GitHub
org forks graphene / graphene-django), so the SPA most likely talks to a
single GraphQL POST endpoint keyed by operation name rather than REST
resources; the HAR is the primary artefact for finding it and its response
shapes.

With `EQUITYZEN_USERNAME` / `EQUITYZEN_PASSWORD` set (sourced from
`/secrets/equityzen.env`) the login form is pre-filled; login and the TOTP
code are still driven by hand. `--no-prefill` skips the fill.

Recording stops when the last browser window is closed (Camoufox's
persistent context fires `close`) or after `--max-duration` (default 1h)
as a safety net. Artefacts land under `/debug/<UTC-ts>/` so the bronze +
silver tree under `/data` stays clean.

Read-only: explore is for observation. EquityZen is a live secondary
marketplace — never click an "Invest" / "Place order" / "Express Deal" /
"Sell" / "Accept" / e-sign / confirm control. The only writes allowed are
the login form submit + the TOTP challenge, both driven manually in the
VNC session.
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

log = logging.getLogger("equityzen.explore")

DEFAULT_URL = "https://equityzen.com/accounts/login/"
DEFAULT_PROFILE_DIR = Path("/secrets/equityzen-profile")
DEFAULT_ENV_FILE = Path("/secrets/equityzen.env")
# The login identifier is an email address. EQUITYZEN_USERNAME is the
# repo-wide convention (every collector uses <SOURCE>_USERNAME);
# EQUITYZEN_EMAIL is accepted as an alias since it reads more naturally
# for an email-keyed login and is what the env file may already use.
# Resolved in order — the first set wins.
USER_ENVS = ("EQUITYZEN_USERNAME", "EQUITYZEN_EMAIL")
PASS_ENV = "EQUITYZEN_PASSWORD"
EVENT_PREFIX = "__EZ_EVENT__ "

# Pre-fill is restricted to this host (and subdomains) so credentials
# never leak into an embedded third-party iframe.
HOST_RE = re.compile(r"(^|\.)equityzen\.com$", re.I)

# Locators for EquityZen's login form, confirmed by a headless DOM probe
# of the (public) login page: the email input is
# `<input type="text" id="email" placeholder="Email" autocomplete="off">`
# — note type=text with NO name attribute, so the id / placeholder are
# the only stable hooks — and the password input is
# `<input type="password" id="password" placeholder="Password">`. The
# EquityZen-specific `#email` / placeholder selectors lead; the standard
# conventions (type=email, autocomplete, name-substring) trail as
# defence against a future form rework.
USER_SELECTOR = (
    "input#email, "
    "input[type='email'], "
    "input[placeholder*='email' i], "
    "input[autocomplete='username'], "
    "input[autocomplete='email'], "
    "input[name*='email' i], "
    "input[name*='user' i], "
    "input[name*='login' i]"
)
PWD_SELECTOR = "input#password, input[type='password']"

# Pre-fill is confined to the login surface. Without this gate the email
# selector can match an unrelated field on the authenticated app (the
# explore trace caught it typing the email into a dashboard input on `/`),
# and a lone 2FA/TOTP step could get the password typed into it.
LOGIN_PATH_RE = re.compile(r"/accounts/|login|sign[-_]?in", re.I)

# Init script. Two responsibilities:
#   1. Click recorder — log every DOM click to console.log() so the
#      Python side picks it up via page.on('console'). Necessary
#      because VNC mouse events bypass Playwright's action API.
#      Password-input values are redacted before logging.
#   2. Login-form detector — once the portal renders a login form (SPA:
#      may be after the initial DOM), signal Python via a console.log()
#      sentinel so the Python side can fill it. The detector is
#      host-gated so it can't fire on third-party iframes, and uses a
#      window-scoped sentinel to fire once per page-load.
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
    console.log('__EZ_EVENT__ ' + JSON.stringify(data));
  }, true);

  // Login-form detector. Host-gated. Fires when an email OR a password
  // field appears, and re-fires whenever the set of present fields
  // changes — so a two-step (email → continue → password) flow gets a
  // signal at each step, and the Python side fills whichever field is
  // currently on screen. The window-scoped signature guards against
  // spamming on every unrelated mutation.
  if (!/(^|\.)equityzen\.com$/i.test(location.hostname)) return;
  // Login surface only — mirrors LOGIN_PATH_RE on the Python side so the
  // detector doesn't fire on the authenticated app's own input fields.
  if (!/\/accounts\/|login|sign[-_]?in/i.test(location.pathname)) return;
  const USER_SEL =
    "input#email, input[type='email'], input[placeholder*='email' i], " +
    "input[autocomplete='username'], input[autocomplete='email'], " +
    "input[name*='email' i], input[name*='user' i], input[name*='login' i]";
  const detect = () => {
    const user = document.querySelector(USER_SEL);
    const pwd = document.querySelector("input#password, input[type='password']");
    const sig = (user ? 'u' : '') + (pwd ? 'p' : '');
    if (sig && sig !== window.__EZ_LOGIN_SIG__) {
      window.__EZ_LOGIN_SIG__ = sig;
      console.log('__EZ_EVENT__ ' + JSON.stringify({
        kind: 'login-form-detected',
        fields: sig,
        ts: new Date().toISOString(),
        url: location.href,
      }));
    }
    // TOTP / one-time-code field detector — emits the field's STATIC
    // descriptor (never its value) once it mounts, so login.py can target
    // the 2FA input without a VNC capture. The value is never read.
    const otp = document.querySelector(
      "input[autocomplete='one-time-code'], input[inputmode='numeric'], " +
      "input[name*='otp' i], input[name*='totp' i], input[name*='2fa' i], " +
      "input[name*='code' i], input[id*='otp' i], input[id*='totp' i], " +
      "input[id*='code' i], input[maxlength='6']"
    );
    if (otp && !window.__EZ_OTP_DETECTED__) {
      window.__EZ_OTP_DETECTED__ = true;
      console.log('__EZ_EVENT__ ' + JSON.stringify({
        kind: 'totp-field-detected',
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
  // load. We attach to documentElement so we see the whole subtree.
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
                     profile_dir=DEFAULT_PROFILE_DIR, dom_snapshots=False)
    return p.parse_args(argv)


def _maybe_prefill_login(page, username: str, password: str,
                         filled: set) -> bool:
    """Pre-fill the email / password fields on EquityZen's login form,
    each field AT MOST ONCE per page (tracked in `filled`, keyed by
    (id(page), kind)). Each fill is clear → fill → verify, so a stray
    autofill or a second invocation can never concatenate onto a field
    that is already correct. Login-surface- and origin-gated; the email
    field must co-exist before the password field is touched, so the
    password can never land in a standalone 2FA input. Returns True iff a
    field was newly filled this call. Never submits — Login + TOTP are
    driven manually in the VNC session."""
    try:
        parsed = urlparse(page.url)
        host = parsed.hostname or ""
    except Exception:
        return False
    if not HOST_RE.search(host):
        return False
    if not LOGIN_PATH_RE.search(parsed.path or ""):
        return False
    user_field = page.locator(USER_SELECTOR).first
    try:
        if user_field.count() == 0:
            return False
    except Exception:
        return False
    did = False
    for kind, field, value in (("user", user_field, username),
                               ("pwd", page.locator(PWD_SELECTOR).first, password)):
        key = (id(page), kind)
        if key in filled:
            continue
        try:
            if field.count() == 0:
                continue  # not present on this step
        except Exception:
            continue
        try:
            # Explicit clear before fill defeats append-style stacking; the
            # read-back verifies the field holds exactly our value, with one
            # retry. Mark the field done regardless so later mutations
            # never trigger a re-fill that fights a hand-typed value.
            field.fill("", timeout=1500)
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
                log.warning("pre-fill of %s field did not stick (got len %d, "
                            "want %d) — type it manually in the VNC session",
                            kind, len(got or ""), len(value))
        except Exception as e:
            log.debug("login-form fill failed for %s: %s", kind, e)
    return did


# What the person at the VNC session is asked to do.
WALK = ("log in via VNC, click through the pages we want to scrape.")


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)
    explore.prepare_profile(args.profile_dir, fresh=args.fresh, log=log,
                            note="TOTP will be required on next login")
    username, password, prefill = explore.load_credentials(
        args.env_file, USER_ENVS, (PASS_ENV,),
        no_prefill=args.no_prefill, host="equityzen.com", log=log)
    filled: set = set()

    with contextlib.ExitStack() as stack:
        session = explore.Session.from_args(
            stack, args, redact=debugcap.secret_redactor(username, password),
            log=log)
        context = session.open_camoufox(stack, args.profile_dir)
        session.attach(
            context, init_js=CLICK_RECORDER_JS, event_prefix=EVENT_PREFIX,
            prefill=(lambda page: _maybe_prefill_login(
                page, username, password, filled)) if prefill else None)
        session.open(args.url)
        session.record(WALK, max_duration=args.max_duration,
                       poll_prefill=False)
    session.report()
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
