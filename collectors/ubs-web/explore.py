#!/usr/bin/env python3
"""
UBS Switzerland e-banking discovery harness.

Launches headed Chromium on the container's Xvfb display and serves it over
VNC, so a UBS netbanking session can be driven **by hand** while everything
it produces is recorded. `download.py` covers cash accounts, portfolios and
the eDocuments archive; the surfaces it does not reach — the card area, the
per-transaction detail view, the card invoice archive — have to be walked
before they can be automated, and a walk a human improvises reaches pages no
scripted one would think to visit.

The harness **never navigates and never clicks**. It opens the login entry
point once, and every action from there is driven by hand. That is the whole
safety model: a recorder cannot stray onto a payment form or a card-management
control, because it issues no interaction at all. CLAUDE.md §1 lists what the
hand-driven walk must stay out of.

It records the network and click logs, the HAR, downloads, and a DOM
snapshot of every distinct screen across the UBS frames (the artefacts are
described in collectorkit.explore). The network log is the primary
artefact: UBS's SPA reads its own JSON, and the endpoint shapes behind each
screen are read off it.

The session is the one thing the harness does touch. An existing state file is
loaded so a live session lands post-auth immediately, and the state is saved
back on exit (``--no-save-state`` opts out) so a login done here is not paid
for twice. The contract number is pre-filled into the login form when the env
file supplies it, and never submitted.

Where that value is masked, stated exactly: ``network.jsonl``,
``clicks.jsonl``, ``dom/*/frame*.html``, ``dom/*/url.txt`` and
``network.har``. Where it is not: ``trace.zip`` / ``trace-chunks/`` and
``dom/*/screen.png`` — a rendered page cannot be edited without destroying
what it is for, and a trace is a zip of driver-written blobs.

Artefacts carry real financial data and unredacted account identifiers. They
land in the ``/debug`` mount, outside bronze and outside the repo, under the
same NEVER-commit contract as the screenshots.

Per CLAUDE.md §2 this drives a live session and runs only when asked.

Usage:
    explore.py [--state-path <file>] [--debug-dir <dir>] [--url <url>]
               [--max-duration SECONDS] [--dom-interval SECONDS]
               [--no-prefill] [--no-save-state] [--trace]
"""

from __future__ import annotations

import argparse
import contextlib
import logging
import os
import re
import sys
from pathlib import Path
from urllib.parse import urlparse

from collectorkit import cli, debugcap, envfile, explore, launch, session

# The state-file locations and the user agent are download.py's, imported
# rather than restated: a session minted or reused here is the one
# `download` picks up, and UBS's anti-bot heuristics must see one UA
# across all three scripts.
from download import DEFAULT_STATE_PATH, LEGACY_STATE_PATH, USER_AGENT
import landmarks as ubs

log = logging.getLogger("ubs-web.explore")

# Every UBS host the session can legitimately touch: the numbered
# e-banking front ends, the auth gateway, and the public secure.ubs.com
# pages the SPA links out to. Used to decide which frames are worth a DOM
# snapshot — never to restrict what may be opened.
UBS_HOST_RE = re.compile(r'(^|\.)ubs\.com$', re.I)

# Console-message prefix the init script tags its events with, so page
# logging and harness events don't have to be told apart by guesswork.
EVENT_PREFIX = "__UBS_EXPLORE__ "

DEFAULT_ENV_FILE = Path("/secrets/ubs-web.env")
# The bank-level env file ubs-web falls back to (README: shared with a
# future ubs-* sibling).
LEGACY_ENV_FILE = Path("/secrets/ubs.env")
CONTRACT_ENV = "UBS_CONTRACT_NUMBER"

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
  const attrs = (el) => {
    const out = {};
    for (const k of ['data-name', 'data-testid', 'aria-label', 'title',
                     'role', 'name', 'type', 'href']) {
      const v = el.getAttribute && el.getAttribute(k);
      if (v) out[k] = v.slice(0, 200);
    }
    return out;
  };
  document.addEventListener('click', (e) => {
    const t = e.target;
    if (!t || !t.tagName) return;
    // A password field's value must never reach the log. UBS's own login
    // has none — the second factor is a scanned QR — but a click landing
    // on one is recorded by shape rather than content regardless.
    const isSecret = t.tagName === 'INPUT' &&
                     (t.type === 'password' || t.name === 'loginalias');
    console.log('__UBS_EXPLORE__ ' + JSON.stringify({
      kind: 'click',
      ts: new Date().toISOString(),
      url: location.href,
      tag: t.tagName,
      id: t.id || null,
      cls: (typeof t.className === 'string' ? t.className.slice(0, 200) : null),
      attrs: attrs(t),
      // The clicked element's own label is what identifies a control in
      // the DOM capture beside it. Ancestor text is not walked — on this
      // SPA that would pull a whole transaction row's amounts in.
      text: isSecret ? '<redacted>' : (t.innerText || '').toString().trim().slice(0, 120),
      xpath: xpathOf(t),
    }));
  }, true);

  // Login-form detector, so the Python side knows when to pre-fill the
  // contract number. UBS serves the form on its own hosts only.
  if (!/(^|\.)ubs\.com$/i.test(location.hostname)) return;
  const detect = () => {
    const field = document.querySelector("input[name='loginalias']");
    if (field && !window.__UBS_LOGIN_SEEN__) {
      window.__UBS_LOGIN_SEEN__ = true;
      console.log('__UBS_EXPLORE__ ' + JSON.stringify({
        kind: 'login-form-detected',
        ts: new Date().toISOString(),
        url: location.href,
      }));
    }
    // The QR challenge is the second factor. Record that it mounted —
    // never its image, which is the challenge itself.
    const qr = document.querySelector("[data-testid='qr-scanner-image']");
    if (qr && !window.__UBS_QR_SEEN__) {
      window.__UBS_QR_SEEN__ = true;
      console.log('__UBS_EXPLORE__ ' + JSON.stringify({
        kind: 'qr-challenge-detected',
        ts: new Date().toISOString(),
        url: location.href,
      }));
    }
  };
  detect();
  new MutationObserver(detect).observe(
      document.documentElement, {childList: true, subtree: true});
})();
"""


# ============================================================
# CLI
# ============================================================

def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument(
        "--state-path", type=Path, default=None,
        help=("Playwright storageState.json to start from and, unless "
              "--no-save-state, write back to. Defaults to the canonical "
              "login.py location under the /secrets mount; an absent file "
              "means the session is signed in by hand."),
    )
    explore.add_args(
        p, url=ubs.LOGIN_ENTRY_URL, env_file=None, dom_snapshots=True,
        env_help=(f"Bash-sourced env file supplying {CONTRACT_ENV} for the "
                  "login-form pre-fill. Defaults to the ubs-web env file "
                  "under /secrets, falling back to the bank-level one. "
                  "Skipped silently when absent."))
    p.add_argument(
        "--no-save-state", action="store_true",
        help=("Do not write the session back to --state-path on exit. The "
              "default saves it, so a sign-in done here is reusable by "
              "`download` instead of costing a second challenge."),
    )
    return p.parse_args(argv)


# ============================================================
# Helpers
# ============================================================





def is_ubs_host(url: str) -> bool:
    """Whether `url` is served by UBS — the frames worth snapshotting."""
    try:
        host = urlparse(url).hostname or ""
    except ValueError:
        return False
    return bool(UBS_HOST_RE.search(host))




def resolve_env_file(explicit: Path | None) -> Path | None:
    """The env file to source: the explicit one, else ubs-web's, else the
    bank-level fallback the README documents. None when none exists."""
    if explicit is not None:
        return explicit
    for candidate in (DEFAULT_ENV_FILE, LEGACY_ENV_FILE):
        if candidate.is_file():
            return candidate
    return None


def _looks_authenticated(context) -> bool:
    """Whether any open page ended on a post-auth workbench URL.

    Gates the state save. Writing the context out unconditionally would let
    a session that was never signed in — the browser closed at the login
    form, the challenge abandoned — overwrite a state file that still had a
    working session in it. A closed browser leaves no pages to read, which
    reads as unauthenticated: the conservative direction, since the cost is
    re-running `login` rather than a silently broken one.
    """
    try:
        return any(ubs.is_post_auth_url(page.url) for page in context.pages)
    except Exception:  # noqa: BLE001 — a dead context is not authenticated
        return False




def prefill_contract(page, contract: str, filled: set) -> bool:
    """Fill the contract-number field once per page, and never submit.

    Mirrors what login.py fills, so only the QR is left to scan.
    The value is written through Playwright rather than the page's own JS
    context, and read back to confirm; a field already carrying something
    is left alone, so a hand-typed value is never fought over.
    """
    for frame in page.frames:
        if not is_ubs_host(frame.url):
            continue
        try:
            field = frame.locator(
                f"input[name='{ubs.CONTRACT_INPUT_NAME}']").first
            if field.count() == 0 or not field.is_visible():
                continue
        except Exception:  # noqa: BLE001
            continue
        key = (id(page), frame.url)
        if key in filled:
            return False
        filled.add(key)
        try:
            if field.input_value(timeout=1000):
                return False           # hand-typed; leave it
            field.fill(contract, timeout=2000)
            if field.input_value(timeout=1500) == contract:
                return True
            log.warning("contract-number pre-fill did not stick; "
                        "type it in the VNC session")
        except Exception as exc:  # noqa: BLE001
            log.debug("prefill failed: %r", exc)
        return False
    return False


# ============================================================
# Main
# ============================================================

# What the person at the VNC session is asked to walk.
WALK = ("drive the session in the VNC window. The card surface is what this "
        "session is for: the card roster, a card's transactions, one "
        "transaction's DETAIL view, and the invoice/statement archive "
        "including one statement download. Read-only surfaces only "
        "(CLAUDE.md §1) — never a payment form and never a card-management "
        "control. Prefer Ctrl-C: it keeps the session readable, so a "
        "sign-in made here is saved and `download` need not challenge "
        "again.")


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)

    # Read from whichever name holds a session (resolve_state_path honours
    # the pre-rename one); always write to the canonical path, so a session
    # found under the legacy name migrates on the way out.
    write_state_path = args.state_path or DEFAULT_STATE_PATH
    state_path = session.resolve_state_path(
        write_state_path, DEFAULT_STATE_PATH, LEGACY_STATE_PATH)
    have_state = state_path.is_file()

    env_file = resolve_env_file(args.env_file)
    if env_file is not None and envfile.source_env_file(env_file):
        log.info("env file:    %s (sourced)", env_file)
    contract = os.environ.get(CONTRACT_ENV, "")
    prefill = bool(contract) and not args.no_prefill
    if args.no_prefill:
        log.info("--no-prefill: contract-number pre-fill disabled")
    elif not contract:
        log.warning("%s not set; contract-number pre-fill disabled",
                    CONTRACT_ENV)
    log.info("state file:  %s (%s)", state_path,
             "loaded" if have_state else "absent — sign in by hand")

    from playwright.sync_api import sync_playwright

    filled: set = set()
    with contextlib.ExitStack() as stack:
        # The contract number is the one credential in this flow, and it
        # rides in the login POST body and the post-auth URL.
        recording = explore.Session.from_args(
            stack, args, redact=debugcap.secret_redactor(contract), log=log,
            observe=UBS_HOST_RE, label="UBS")
        pw = stack.enter_context(sync_playwright())
        # Headed: the whole point is a display a person can drive. The
        # sandbox flags match download.py's — the container is single-tenant
        # and Chromium's user-namespace sandbox needs privileges the
        # non-root runtime user does not have.
        browser = pw.chromium.launch(
            headless=False,
            args=launch.chromium_args(
                "--no-sandbox",
                "--disable-dev-shm-usage",
                "--disable-blink-features=AutomationControlled",
                "--start-maximized",
            ),
        )
        context = browser.new_context(
            storage_state=str(state_path) if have_state else None,
            user_agent=USER_AGENT,
            accept_downloads=True,
            viewport=None,
            record_har_path=str(recording.har_path),
        )
        recording.attach(
            context, init_js=CLICK_RECORDER_JS, event_prefix=EVENT_PREFIX,
            prefill=(lambda page: prefill_contract(page, contract, filled))
            if prefill else None,
            prefill_event="contract-prefilled")
        page = context.new_page()
        page.goto(args.url, wait_until="domcontentloaded")
        recording.started(page, args.url, had_state=have_state)
        reason = recording.record(WALK, max_duration=args.max_duration)

        # Save the session back before the context closes. A sign-in done
        # here is then reusable by `download`, which is the difference
        # between one challenge and two.
        if not args.no_save_state and _looks_authenticated(context):
            try:
                context.storage_state(path=str(write_state_path))
                session.secure_file(write_state_path)
                log.info("session state saved to %s", write_state_path)
            except Exception as exc:  # noqa: BLE001
                log.warning("could not save session state: %s",
                            debugcap.safe_error(exc))
        elif not args.no_save_state:
            log.info(
                "session state not saved (%s). Stopping with Ctrl-C rather "
                "than by closing the window keeps the session readable, so a "
                "sign-in made here can be reused by `download`.",
                "browser already closed" if reason == "browser-closed"
                else "no page ended on a post-auth URL")

        # close() is what flushes the HAR. It fails when the browser is
        # already gone (the window-closed path), by which point the two
        # JSONL logs are complete on disk.
        with contextlib.suppress(Exception):
            context.close()
        with contextlib.suppress(Exception):
            browser.close()
    recording.report()
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
