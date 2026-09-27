#!/usr/bin/env python3
"""carta discovery harness.

Launches Camoufox in the container's Xvfb display on the Carta holder UI
and records the session driven over VNC — the HAR, the network and click
logs, and downloads (the artefacts are described in collectorkit.explore)
— so login.py and download.py can be written from real traces. Read the
HAR against the observed endpoint map in DESIGN.md §3 — internal shapes
differ from the public /v1alpha1/ map of §2.

With `CARTA_USERNAME` (or `CARTA_EMAIL`) / `CARTA_PASSWORD` set (sourced
from `/secrets/carta.env`) the login form is pre-filled; sign-in and 2FA
are still driven by hand. `--no-prefill` skips the fill.

This is read-only observation. Per AGENTS.md, never click an Exercise /
Sell / Transfer / Accept / wire / e-sign / confirm control, and stay out
of any issuer / company-admin or fund-admin console the login may surface
— this collector observes the portfolio holder's own holdings only.

Re-run this harness whenever Carta moves its UI or internal endpoints:
the fresh HAR + trace pinpoint what changed against the observed
endpoint map in DESIGN.md §3, so login.py / download.py can be updated
from real captures.

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

log = logging.getLogger("carta.explore")

# app.carta.com is the holder app; it redirects to the login page when the
# session is missing. Override with --url if explore reveals a different
# entry host.
DEFAULT_URL = "https://app.carta.com"
DEFAULT_PROFILE_DIR = Path("/secrets/carta-profile")
DEFAULT_ENV_FILE = Path("/secrets/carta.env")
# Same order login.py resolves: canonical name first, legacy alias second.
# They must agree — an env file carrying only the canonical name would
# otherwise log in fine and silently fail to pre-fill here.
USER_ENVS = ("CARTA_USERNAME", "CARTA_EMAIL")
PASS_ENV = "CARTA_PASSWORD"
EVENT_PREFIX = "__CARTA_EVENT__ "

# Pre-fill is restricted to carta.com (and subdomains: app.carta.com,
# login.app.carta.com, …) so credentials never leak into an embedded
# third-party iframe.
HOST_RE = re.compile(r"(^|\.)carta\.com$", re.I)

# Locators tried in order. Deliberately broad — this harness runs when
# Carta's form has moved and the concrete ids in login.py no longer match,
# so it anchors on standard HTML conventions (input[type=email] /
# autocomplete=username) plus name-substring fallbacks rather than on any
# one observed id. On a two-step form (email screen, then password) both
# fields aren't present at once and pre-fill simply no-ops — the
# credentials get typed by hand, which is fine for discovery.
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
#   1. Click recorder — log every DOM click to console.log() so the Python
#      side picks it up via page.on('console'). Necessary because VNC mouse
#      events bypass Playwright's action API. Password-input values are
#      redacted before logging.
#   2. Login-form detector — once Carta renders a login form (SPA: may be
#      after the initial DOM), signal Python via a console.log() sentinel so
#      the Python side can fill it. Host-gated so it can't fire on
#      third-party iframes; window-scoped sentinel fires once per page-load.
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
    console.log('__CARTA_EVENT__ ' + JSON.stringify(data));
  }, true);

  // Login-form detector. Host-gated; fires once per window.
  if (!/(^|\.)carta\.com$/i.test(location.hostname)) return;
  const detect = () => {
    if (window.__CARTA_LOGIN_DETECTED__) return;
    const user = document.querySelector(
      "input[type='email'], input[autocomplete='username'], " +
      "input[autocomplete='email'], input[name*='email' i], " +
      "input[name*='user' i], input[name*='login' i]"
    );
    const pwd = document.querySelector("input[type='password']");
    if (user && pwd) {
      window.__CARTA_LOGIN_DETECTED__ = true;
      console.log('__CARTA_EVENT__ ' + JSON.stringify({
        kind: 'login-form-detected',
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
                     profile_dir=DEFAULT_PROFILE_DIR, dom_snapshots=False)
    return p.parse_args(argv)


def _maybe_prefill_login(page, email: str, password: str) -> bool:
    """Fill whichever of the email / password fields is present, visible,
    and still empty on the current carta.com page.

    Handles single-step forms, two-step (email screen → password screen)
    forms, and SPA client-side route changes: each field is filled
    independently, so the email lands on step 1 and the password lands on
    step 2 when it mounts. Idempotent — a field that already has content is
    left alone (so it never clobbers a hand-typed value, and re-polling
    is a no-op once filled). Never submits — Sign in + 2FA are driven
    manually in the VNC session. Returns True iff at least one field was
    filled this call."""
    try:
        host = urlparse(page.url).hostname or ""
    except Exception:
        return False
    if not HOST_RE.search(host):
        return False
    filled = False
    for selector, value, label in (
        (USER_SELECTOR, email, "email"),
        (PWD_SELECTOR, password, "password"),
    ):
        try:
            base = page.locator(selector)
            # count() / is_visible() are instant (no auto-wait), so an absent
            # field — e.g. the password input on a two-step email screen —
            # costs nothing instead of blocking on a timeout each poll.
            if base.count() == 0:
                continue
            field = base.first
            if not field.is_visible():
                continue
            if field.input_value(timeout=1000):
                continue  # already filled (by hand, or a prior poll)
            field.fill(value, timeout=2000)
            filled = True
        except Exception as e:
            log.debug("prefill %s field failed: %s", label, e)
    return filled


# What the person at the VNC session is asked to do.
WALK = ("log in via VNC, click through the pages we want to scrape (the "
        "stock-comp plan AND the fund LP statements).")


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)
    explore.prepare_profile(args.profile_dir, fresh=args.fresh, log=log,
                            note="2FA will be required on next login")
    username, password, prefill = explore.load_credentials(
        args.env_file, USER_ENVS, (PASS_ENV,),
        no_prefill=args.no_prefill, host="carta.com", log=log)

    with contextlib.ExitStack() as stack:
        session = explore.Session.from_args(
            stack, args, redact=debugcap.secret_redactor(username, password),
            log=log)
        context = session.open_camoufox(stack, args.profile_dir)
        session.attach(
            context, init_js=CLICK_RECORDER_JS, event_prefix=EVENT_PREFIX,
            prefill=(lambda page: _maybe_prefill_login(page, username, password)) if prefill else None)
        session.open(args.url)
        session.record(WALK, max_duration=args.max_duration)
    session.report()
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
