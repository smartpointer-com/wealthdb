#!/usr/bin/env python3
"""firstcitizens discovery harness.

Launches Camoufox in the container's Xvfb display on the First Citizens
retail UI and records the session driven over VNC — the HAR, the network
and click logs, downloads, and a DOM snapshot of every distinct screen
(the artefacts are described in collectorkit.explore) — so login.py and
download.py can be written from real traces. A login form or 2FA challenge
in a cross-origin iframe never reaches the top-document click listener, so
selectors are pinned from the DOM snapshots, not the clicks.

With `FIRSTCITIZENS_USERNAME` / `FIRSTCITIZENS_PASSWORD` set (sourced from
`/secrets/firstcitizens.env`) the login form is pre-filled; sign-in and
2FA are still driven by hand. `--no-prefill` skips the fill.

The session should walk the three discovery flows DESIGN.md §3 details:
the login (2FA challenge + any "remember this device" control), the
statements/documents area (how monthly statement PDFs are listed and
fetched), and each account's full transaction history plus every export
format offered.

This is read-only observation. Per CLAUDE.md, never click a Pay /
Transfer / Zelle / Send / Deposit / confirm control, stay out of card
management, settings, and the secure message center, and keep to the
retail checking + savings surfaces — never any wealth-management, trust,
or brokerage console the login may also expose.

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

log = logging.getLogger("firstcitizens.explore")

# Where the sign-in actually lives is a §3 open question: the public site
# is www.firstcitizens.com, but the digital-banking app may sit on a
# dedicated subdomain (or even a vendor host) behind a redirect. Starting
# at the public homepage records whatever redirect chain exists; --url can
# point straight at the observed entry once a capture names it.
DEFAULT_URL = "https://www.firstcitizens.com/"
DEFAULT_PROFILE_DIR = Path("/secrets/firstcitizens-profile")
DEFAULT_ENV_FILE = Path("/secrets/firstcitizens.env")
USER_ENV = "FIRSTCITIZENS_USERNAME"
PASS_ENV = "FIRSTCITIZENS_PASSWORD"
EVENT_PREFIX = "__FIRSTCITIZENS_EVENT__ "

# Pre-fill is restricted to firstcitizens.com (and subdomains) so
# credentials never leak into an embedded third-party frame. If the first
# capture shows the login form rendering on a different host, the
# credentials are typed by hand that run and the gate is widened to the
# observed host afterwards — never pre-emptively.
HOST_RE = re.compile(r"(^|\.)firstcitizens\.com$", re.I)

# Locators tried in order. Deliberately broad — no real captures exist yet
# (this harness produces them), so they anchor on standard HTML
# conventions rather than any one observed id. US retail sign-ins are
# username-keyed (not an email), hence the name/id substring hooks lead
# and the email conventions trail as defence.
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
#   2. Login-form + OTP detectors — once a firstcitizens.com frame renders
#      a login form (SPA: may be after the initial DOM), signal Python via
#      a console.log() sentinel so the Python side can fill it. Host-gated
#      so neither detector can fire on a third-party frame; there is no
#      path gate because no captures exist yet to name the logon routes —
#      the Python side only fills when username + password fields co-exist
#      in one frame, which is what keeps credentials out of lone fields.
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
    console.log('__FIRSTCITIZENS_EVENT__ ' + JSON.stringify(data));
  }, true);

  // Login-form + OTP detectors. Host-gated to firstcitizens.com frames.
  if (!/(^|\.)firstcitizens\.com$/i.test(location.hostname)) return;
  const USER_SEL =
    "input[autocomplete='username'], input[name*='user' i], " +
    "input[id*='user' i], input[name*='login' i], input[id*='login' i], " +
    "input[type='email'], input[autocomplete='email']";
  const detect = () => {
    const user = document.querySelector(USER_SEL);
    const pwd = document.querySelector("input[type='password']");
    const sig = (user ? 'u' : '') + (pwd ? 'p' : '');
    if (sig && sig !== window.__FIRSTCITIZENS_LOGIN_SIG__) {
      window.__FIRSTCITIZENS_LOGIN_SIG__ = sig;
      console.log('__FIRSTCITIZENS_EVENT__ ' + JSON.stringify({
        kind: 'login-form-detected',
        fields: sig,
        ts: new Date().toISOString(),
        url: location.href,
      }));
    }
    // OTP / one-time-code field detector — emits the field's STATIC
    // descriptor (never its value) once it mounts, so the 2FA surface
    // (SMS / voice / email OTP expected — observed, not assumed) is
    // recorded even when no click lands on the field itself.
    const otp = document.querySelector(
      "input[autocomplete='one-time-code'], input[inputmode='numeric'], " +
      "input[name*='otp' i], input[name*='code' i], input[id*='otp' i], " +
      "input[id*='code' i], input[maxlength='6'], input[maxlength='8']"
    );
    if (otp && !window.__FIRSTCITIZENS_OTP_DETECTED__) {
      window.__FIRSTCITIZENS_OTP_DETECTED__ = true;
      console.log('__FIRSTCITIZENS_EVENT__ ' + JSON.stringify({
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
                         filled: set) -> bool:
    """Fill the username + password fields of a First Citizens login form,
    each AT MOST ONCE per page (tracked in `filled`, keyed by
    (id(page), kind)).

    Frame-aware: the form is looked for in every frame whose host is under
    firstcitizens.com — covering both a form in the main frame and one
    embedded in a same-brand iframe (which entry shape the site uses is a
    §3 open question). Both fields must co-exist in the SAME frame before
    either is touched, so the password can never land in a standalone
    field (a 2FA code entry is a lone input). Each fill is verified by
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
        # The first firstcitizens.com frame carrying both fields is the
        # login form; other frames (marketing embeds) don't get a second
        # pass.
        break
    return did


# What the person at the VNC session is asked to walk (DESIGN.md §3).
WALK = ("walk the three discovery flows (DESIGN.md §3): 1) log in via VNC, "
        "noting the 2FA factor offered and any 'remember this device' "
        "control; 2) open the statements/documents area and fetch at least "
        "one statement PDF; 3) open each account's full transaction history "
        "and run every export format offered (CSV/QFX/OFX).")


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)
    explore.prepare_profile(args.profile_dir, fresh=args.fresh, log=log,
                            note="the full 2FA challenge will fire on next "
                                 "login")
    username, password, prefill = explore.load_credentials(
        args.env_file, (USER_ENV,), (PASS_ENV,), no_prefill=args.no_prefill,
        host="firstcitizens.com", log=log)
    filled: set = set()

    with contextlib.ExitStack() as stack:
        session = explore.Session.from_args(
            stack, args, redact=debugcap.secret_redactor(username, password),
            log=log, observe=HOST_RE, label="firstcitizens")
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
