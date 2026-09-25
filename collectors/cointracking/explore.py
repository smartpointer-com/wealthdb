#!/usr/bin/env python3
"""cointracking discovery harness.

Launches Camoufox in the container's Xvfb display on cointracking.info and
records the session driven over VNC — the HAR, the network and click logs,
and downloads (the artefacts are described in collectorkit.explore) — so
login.py and download.py can be written from real traces. The HAR is the
primary record of the internal REST endpoints the SPA hits.

With `COINTRACKING_USERNAME` / `COINTRACKING_PASSWORD` set (sourced from
`/secrets/cointracking.env`) the login form is pre-filled; login and 2FA
are still driven by hand. `--no-prefill` skips the fill.

Recording stops when the last browser window is closed
(Camoufox's persistent context fires `close`) or after
`--max-duration` (default 1h) as a safety net. Artefacts land under
`/debug/<UTC-ts>/` so the bronze + silver tree under `/data` stays
clean.
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

log = logging.getLogger("cointracking.explore")

DEFAULT_URL = "https://cointracking.info"
DEFAULT_PROFILE_DIR = Path("/secrets/cointracking-profile")
DEFAULT_ENV_FILE = Path("/secrets/cointracking.env")
USER_ENV = "COINTRACKING_USERNAME"
PASS_ENV = "COINTRACKING_PASSWORD"
EVENT_PREFIX = "__CT_EVENT__ "

# Pre-fill is restricted to this host (and subdomains) so credentials
# never leak into an embedded third-party iframe.
HOST_RE = re.compile(r"(^|\.)cointracking\.info$", re.I)

# Locators tried in order. Deliberately broad — this harness runs when
# cointracking.info's form has moved and login.py's concrete selectors
# no longer match, so it anchors on standard HTML conventions
# (input[type=email] / autocomplete=username) plus name-substring
# fallbacks rather than on any one observed id. Same for the password
# input.
USER_SELECTOR = (
    "input[type='email'], "
    "input[autocomplete='username'], "
    "input[autocomplete='email'], "
    "input[name*='email' i], "
    "input[name*='user' i], "
    "input[name*='login' i]"
)
PWD_SELECTOR = "input[type='password']"

# Init script. Two responsibilities:
#   1. Click recorder — log every DOM click to console.log() so the
#      Python side picks it up via page.on('console'). Necessary
#      because VNC mouse events bypass Playwright's action API.
#      Password-input values are redacted before logging.
#   2. Login-form detector — once cointracking.info renders a login
#      form (SPA: may be after the initial DOM), signal Python via a
#      console.log() sentinel so the Python side can fill it. The
#      detector is host-gated so it can't fire on third-party
#      iframes, and uses a window-scoped sentinel to fire once per
#      page-load.
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
    console.log('__CT_EVENT__ ' + JSON.stringify(data));
  }, true);

  // Login-form detector. Host-gated; fires once per window.
  if (!/(^|\.)cointracking\.info$/i.test(location.hostname)) return;
  const detect = () => {
    if (window.__CT_LOGIN_DETECTED__) return;
    const user = document.querySelector(
      "input[type='email'], input[autocomplete='username'], " +
      "input[autocomplete='email'], input[name*='email' i], " +
      "input[name*='user' i], input[name*='login' i]"
    );
    const pwd = document.querySelector("input[type='password']");
    if (user && pwd) {
      window.__CT_LOGIN_DETECTED__ = true;
      console.log('__CT_EVENT__ ' + JSON.stringify({
        kind: 'login-form-detected',
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


def _maybe_prefill_login(page, username: str, password: str) -> bool:
    """If the current page is on cointracking.info AND a login form
    is present + visible AND nothing has been typed into the
    fields yet, fill the credentials. Returns True iff the fill
    actually happened. Never submits — Login + 2FA are driven
    manually in the VNC session."""
    try:
        host = urlparse(page.url).hostname or ""
    except Exception:
        return False
    if not HOST_RE.search(host):
        return False
    user_field = page.locator(USER_SELECTOR).first
    pwd_field = page.locator(PWD_SELECTOR).first
    try:
        # input_value() auto-waits for the element to exist + be
        # actionable, with the provided timeout. If neither field
        # is here we move on (no login form on this page).
        existing_user = user_field.input_value(timeout=2000)
        existing_pwd = pwd_field.input_value(timeout=1000)
    except Exception:
        return False
    if existing_user or existing_pwd:
        # Something was already typed here; don't overwrite it.
        return False
    try:
        user_field.fill(username, timeout=2000)
        pwd_field.fill(password, timeout=2000)
    except Exception as e:
        log.debug("login-form fill failed: %s", e)
        return False
    return True


# What the person at the VNC session is asked to do.
WALK = ("log in via VNC, click through the pages we want to scrape.")


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)
    explore.prepare_profile(args.profile_dir, fresh=args.fresh, log=log,
                            note="2FA will be required on next login")
    username, password, prefill = explore.load_credentials(
        args.env_file, (USER_ENV,), (PASS_ENV,),
        no_prefill=args.no_prefill, host="cointracking.info", log=log)

    with contextlib.ExitStack() as stack:
        session = explore.Session.from_args(
            stack, args, redact=debugcap.secret_redactor(username, password),
            log=log)
        context = session.open_camoufox(stack, args.profile_dir)
        session.attach(
            context, init_js=CLICK_RECORDER_JS, event_prefix=EVENT_PREFIX,
            prefill=(lambda page: _maybe_prefill_login(page, username, password)) if prefill else None)
        session.open(args.url)
        session.record(WALK, max_duration=args.max_duration,
                       poll_prefill=False)
    session.report()
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
