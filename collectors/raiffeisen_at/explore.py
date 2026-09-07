#!/usr/bin/env python3
"""raiffeisen_at discovery harness.

Launches Camoufox in the container's Xvfb display, navigates to Mein ELBA
(the Austrian Raiffeisen retail e-banking portal), and records every
action taken in the VNC session so the later verbs can be written from
real traces:

  - **HAR** (`network.har`)        — every request + response with
                                     headers and bodies. The primary
                                     artefact for finding whatever
                                     XHR/REST traffic backs the accounts
                                     overview, the transaction history,
                                     the CSV export, and the on-demand
                                     statement generator — the ELBA
                                     wiring is unknown, so this decides
                                     scrape-vs-REST per surface.
                                     Credentials, cookies and
                                     query-string tokens are masked out
                                     of it once the close has written it.
  - **Network log**
    (`network.jsonl`)              — crash-safe, line-flushed request +
                                     response log (the HAR only flushes on
                                     a clean context close). Bodies of
                                     text-shaped responses captured up to
                                     200 KB.
  - **Playwright trace**
    (`trace.zip` + `trace-chunks/`)— screenshots + DOM snapshots +
                                     network events at every action.
                                     Open with `playwright show-trace`.
                                     OPT-IN via --trace: the current
                                     Playwright/camoufox pair crashes on
                                     tracing (see the --trace help).
                                     UNREDACTED and unredactable — its
                                     DOM snapshots carry every input's
                                     value, a hand-typed password
                                     included.
  - **Click log**
    (`clicks.jsonl`)               — one JSON object per click on the page
                                     (timestamp, URL, tag, id, text,
                                     xpath). Captured via a
                                     `document.addEventListener('click', …)`
                                     init script because user-driven VNC
                                     clicks bypass the Playwright API. Plus
                                     a `MutationObserver` watch that signals
                                     when a login form has mounted — the
                                     Python side then pre-fills it from
                                     `RAIFFEISEN_AT_USERNAME` /
                                     `RAIFFEISEN_AT_PASSWORD` (sourced from
                                     `/secrets/raiffeisen_at.env`).
                                     Sign in + 2FA are still driven by hand.
                                     Pass --no-prefill to skip the pre-fill
                                     and type the credentials manually.
  - **Downloads**
    (`downloads/`)                 — every file fetched in-session
                                     (statement PDFs, CSV exports),
                                     materialised as the browser hands
                                     them over.
  - **DOM snapshots**
    (`dom/<NNN>/frameK.html`
     + `screen.png`)               — every DISTINCT screen's full DOM, for
                                     every raiffeisen.at frame (including
                                     any login/2FA iframe), plus a
                                     screenshot. Deduped by DOM structure.
                                     This is the record the click log cannot
                                     produce — and if the data path turns
                                     out to be server-rendered HTML, the
                                     only record selectors can be pinned
                                     from at all. `--dom-interval 0`
                                     disables.

The session should walk the three discovery flows DESIGN.md §3 details:
the login (the pushTAN challenge, its completion signal, and any "trust
this browser" control), the on-demand statement generator (its range
parameters, maximum range, and reach), and each account's transaction
history plus the CSV export.

This is read-only observation. Per CLAUDE.md, never click a payment /
transfer / standing-order / confirm control (Überweisung, Auftrag,
Senden, Freigeben, Zeichnen …), stay out of card management, securities,
settings, and the message center, and keep to the retail deposit
surfaces (checking + savings) only.

Recording stops when the last browser window is closed (Camoufox's
persistent context fires `close`) or after `--max-duration` (default 1h) as
a safety net. Artefacts land under `/debug/<UTC-ts>/` so the bronze + silver
tree under `/data` stays clean.
"""
from __future__ import annotations

import argparse
import base64
import contextlib
import json
import logging
import os
import re
import shutil
import signal
import sys
import threading
import time
from datetime import datetime, timezone
from pathlib import Path
from urllib.parse import urlparse

from collectorkit import cli, debugcap, envfile, launch

log = logging.getLogger("raiffeisen_at.explore")

DEFAULT_URL = "https://mein.elba.raiffeisen.at/"
DEFAULT_PROFILE_DIR = Path("/secrets/raiffeisen_at-profile")
DEFAULT_DEBUG_ROOT = Path("/debug")
DEFAULT_ENV_FILE = Path("/secrets/raiffeisen_at.env")
USER_ENV = "RAIFFEISEN_AT_USERNAME"
PASS_ENV = "RAIFFEISEN_AT_PASSWORD"
EVENT_PREFIX = "__RAIFFEISEN_AT_EVENT__ "

# Pre-fill is restricted to raiffeisen.at (and subdomains — the entry
# point mein.elba.raiffeisen.at is one, and a group SSO host would be
# another) so credentials never leak into an embedded third-party frame.
# If the first capture shows the login form rendering on a different
# host, the credentials are typed by hand that run and the gate is
# widened to the observed host afterwards — never pre-emptively.
HOST_RE = re.compile(r"(^|\.)raiffeisen\.at$", re.I)

# Locators tried in order. Deliberately broad — no real captures exist yet
# (this harness produces them), so they anchor on standard HTML
# conventions plus German attribute hooks: Mein ELBA logins are keyed on
# a Verfüger number or user name (which one the form asks for is a
# DESIGN.md §3 question), so `verfueger` / `benutzer` id/name substrings
# lead and the generic/email conventions trail as defence. Display-text
# selectors are avoided throughout (German UI; ids/roles only).
USER_SELECTOR = (
    "input[autocomplete='username'], "
    "input[name*='verfueger' i], "
    "input[id*='verfueger' i], "
    "input[name*='benutzer' i], "
    "input[id*='benutzer' i], "
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
#   2. Login-form + code-field detectors — once a raiffeisen.at frame
#      renders a login form (may be after the initial DOM), signal Python
#      via a console.log() sentinel so the Python side can fill it.
#      Host-gated so neither detector can fire on a third-party frame;
#      there is no path gate because no captures exist yet to name the
#      logon routes — the Python side only fills when username + password
#      fields co-exist in one frame, which is what keeps credentials out
#      of lone fields. Init scripts run in every frame, so an embedded
#      sign-in iframe is covered too.
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
    console.log('__RAIFFEISEN_AT_EVENT__ ' + JSON.stringify(data));
  }, true);

  // Login-form + code-field detectors. Host-gated to raiffeisen.at frames.
  if (!/(^|\.)raiffeisen\.at$/i.test(location.hostname)) return;
  const USER_SEL =
    "input[autocomplete='username'], input[name*='verfueger' i], " +
    "input[id*='verfueger' i], input[name*='benutzer' i], " +
    "input[id*='benutzer' i], input[name*='user' i], " +
    "input[id*='user' i], input[name*='login' i], input[id*='login' i], " +
    "input[type='email'], input[autocomplete='email']";
  const detect = () => {
    const user = document.querySelector(USER_SEL);
    const pwd = document.querySelector("input[type='password']");
    const sig = (user ? 'u' : '') + (pwd ? 'p' : '');
    if (sig && sig !== window.__RAIFFEISEN_AT_LOGIN_SIG__) {
      window.__RAIFFEISEN_AT_LOGIN_SIG__ = sig;
      console.log('__RAIFFEISEN_AT_EVENT__ ' + JSON.stringify({
        kind: 'login-form-detected',
        fields: sig,
        ts: new Date().toISOString(),
        url: location.href,
      }));
    }
    // One-time-code field detector — emits the field's STATIC descriptor
    // (never its value) once it mounts. The expected 2FA is pushTAN
    // (approve in the app, nothing to type), so this likely never fires
    // on the happy path — it records any FALLBACK factor (a code entry,
    // an SMS-TAN) the challenge screen may offer.
    const otp = document.querySelector(
      "input[autocomplete='one-time-code'], input[inputmode='numeric'], " +
      "input[name*='otp' i], input[name*='code' i], input[id*='otp' i], " +
      "input[id*='code' i], input[name*='tan' i], input[id*='tan' i], " +
      "input[maxlength='6'], input[maxlength='8']"
    );
    if (otp && !window.__RAIFFEISEN_AT_OTP_DETECTED__) {
      window.__RAIFFEISEN_AT_OTP_DETECTED__ = true;
      console.log('__RAIFFEISEN_AT_EVENT__ ' + JSON.stringify({
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
    p.add_argument(
        "--profile-dir", type=Path, default=DEFAULT_PROFILE_DIR,
        help=("Persistent Camoufox profile dir. The login cookie + any "
              "browser/device-trust state lives here so subsequent runs "
              "could skip the pushTAN — whether any trust actually "
              "persists is one of the things explore measures. "
              "Default: %(default)s."),
    )
    p.add_argument(
        "--debug-dir", type=Path, default=None,
        help=("Where to write HAR + trace + click log. Defaults to "
              "/debug/<UTC-ts>/ (mounted from "
              "~/.cache/wealthdb/debug/raiffeisen_at on the host)."),
    )
    p.add_argument(
        "--url", default=DEFAULT_URL,
        help="Initial URL to open. Default: %(default)s.",
    )
    p.add_argument(
        "--max-duration", type=int, default=3600,
        help=("Safety net: auto-close the recording after N seconds even "
              "if the browser is left open. Default: %(default)s (1 hour)."),
    )
    p.add_argument(
        "--env-file", type=Path, default=DEFAULT_ENV_FILE,
        help=("Path to a bash-sourced env file with RAIFFEISEN_AT_USERNAME "
              "/ RAIFFEISEN_AT_PASSWORD. Skipped silently if absent. "
              "Default: %(default)s."),
    )
    p.add_argument(
        "--dom-interval", type=int, default=3,
        help=("Seconds between DOM snapshots. Every distinct screen's DOM "
              "(all raiffeisen.at frames, incl. any login/2FA iframe) "
              "+ a screenshot land under dom/<NNN>/ — the record the click "
              "log misses when a control lives in a cross-origin iframe "
              "whose clicks don't bubble, and the record selectors are "
              "pinned from if the UI is server-rendered. Deduped by DOM "
              "structure, so only new screens are written. 0 disables. "
              "Default: %(default)s."),
    )
    p.add_argument(
        "--no-prefill", action="store_true",
        help=("Skip pre-filling the login form. Use when you want to "
              "verify the form selectors by typing the credentials "
              "yourself (recommended for the very first run, before the "
              "selectors have been validated against a real capture), or "
              "when $RAIFFEISEN_AT_USERNAME / $RAIFFEISEN_AT_PASSWORD are "
              "intentionally unset."),
    )
    p.add_argument(
        "--fresh", action="store_true",
        help=("Wipe the persistent Camoufox profile dir before launching. "
              "Use this to force a fresh login: clears cookies including "
              "any browser-trust state that would let the site skip the "
              "pushTAN. The next session will present the full challenge "
              "— exactly the flow worth capturing."),
    )
    p.add_argument(
        "--trace", action="store_true",
        help=("Record a Playwright trace (DOM snapshots + screenshots, "
              "chunked zips). OFF by default: the pinned Playwright 1.49 "
              "tracer crashes the camoufox 152.0.4 Firefox build outright "
              "(matched-set drift — navigation works, tracing kills the "
              "browser). Enable only once base-camoufox realigns the pair; "
              "network.jsonl + clicks.jsonl + the HAR cover discovery "
              "meanwhile."),
    )
    p.add_argument(
        "--chunk-interval", type=int, default=30,
        help=("Seconds between incremental trace-chunk saves (with "
              "--trace). Lower = less data loss on abrupt close, more disk "
              "I/O. Default: %(default)s."),
    )
    cli.add_common_args(p)
    return p.parse_args(argv)


def _now_iso() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="seconds")


def _dom_skeleton(html: str) -> str:
    """A structure-only fingerprint of a DOM: its tag names + element ids,
    without text or dynamic attribute values. Two renders of the same screen
    hash equal even as tokens/countdowns change, so the snapshotter writes a
    screen once, not every tick."""
    return "".join(re.findall(r'<[a-z][a-z0-9-]*|id="[^"]+"', html, re.I))


def _capture_dom_snapshot(context, dom_dir, seq, last_skeleton, log,
                          redact=None):
    """Write every raiffeisen.at frame's DOM (+ a screenshot) across all
    pages, but only when the composite structure changed since the last
    snapshot. This is the record the click log can't produce: a login form
    or 2FA challenge in a cross-origin iframe never reaches the
    top-document click listener.

    Each frame's markup goes through ``debugcap.scrub_dom`` on the way to
    disk: a serialized login form can carry the typed password as a
    ``value`` attribute, and a snapshot is written whether the fill was
    programmatic or hand-typed. Returns (seq, skeleton)."""
    frames = []
    for pg in list(context.pages):
        for frame in pg.frames:
            try:
                host = urlparse(frame.url).hostname or ""
            except Exception:
                continue
            if not HOST_RE.search(host):
                continue
            try:
                frames.append((pg, frame.content()))
            except Exception:
                continue
    if not frames:
        return seq, last_skeleton
    skeleton = _dom_skeleton("".join(c for _, c in frames))
    if skeleton == last_skeleton:
        return seq, last_skeleton
    seq += 1
    snap = dom_dir / f"{seq:03d}"
    snap.mkdir(parents=True, exist_ok=True)
    for i, (_pg, content) in enumerate(frames):
        with contextlib.suppress(Exception):
            (snap / f"frame{i}.html").write_text(
                debugcap.scrub_dom(content, redact), encoding="utf-8")
    with contextlib.suppress(Exception):
        context.pages[0].screenshot(path=str(snap / "screen.png"),
                                    full_page=True)
    log.info("dom snapshot %03d (%d raiffeisen frame(s))", seq, len(frames))
    return seq, skeleton


def _maybe_prefill_login(page, username: str, password: str,
                         filled: set) -> bool:
    """Fill the username + password fields of a Mein ELBA login form, each
    AT MOST ONCE per page (tracked in `filled`, keyed by
    (id(page), kind)).

    Frame-aware: the form is looked for in every frame whose host is under
    raiffeisen.at — covering both a form in the main frame and one embedded
    in a same-brand iframe (which entry shape the site uses is a §3 open
    question). Both fields must co-exist in the SAME frame before either is
    touched, so the password can never land in a standalone field (a 2FA
    code entry is a lone input) — note this also means a TWO-STEP login
    (identifier first, password later) leaves the pre-fill idle; the
    credentials are then typed by hand and the pre-fill adapted to the
    observed shape afterwards. Each fill is verified by read-back, with one
    clear-and-retry, so a stray autofill can never concatenate onto a
    correct value; the field is marked done regardless of the outcome so
    later DOM mutations never trigger a re-fill that fights a hand-typed
    value. A field that already has content is marked done untouched.
    Never submits — Sign in + 2FA are driven manually in the VNC session.
    Returns True iff at least one field was newly filled and verified."""
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
        # The first raiffeisen.at frame carrying both fields is the login
        # form; other frames (marketing embeds) don't get a second pass.
        break
    return did


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)

    ts = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    debug_dir = args.debug_dir or (DEFAULT_DEBUG_ROOT / ts)
    debug_dir.mkdir(parents=True, exist_ok=True)
    downloads_dir = debug_dir / "downloads"
    downloads_dir.mkdir(parents=True, exist_ok=True)
    dom_dir = debug_dir / "dom"
    if args.dom_interval > 0:
        dom_dir.mkdir(parents=True, exist_ok=True)
    trace_chunks_dir = debug_dir / "trace-chunks"
    if args.trace:
        trace_chunks_dir.mkdir(parents=True, exist_ok=True)
    har_path = debug_dir / "network.har"
    trace_path = debug_dir / "trace.zip"
    clicks_path = debug_dir / "clicks.jsonl"
    network_path = debug_dir / "network.jsonl"

    # --fresh wipes the persistent profile so the next launch hits the full
    # challenge again. Useful for capturing the pushTAN screen and the full
    # first-login traffic. Must run BEFORE the profile-dir prep below.
    if args.fresh and args.profile_dir.exists():
        log.warning("--fresh: wiping profile dir %s "
                    "(the full pushTAN challenge will fire on next login)",
                    args.profile_dir)
        shutil.rmtree(args.profile_dir)
    launch.prepare_profile_dir(args.profile_dir)

    log.info("debug dir:   %s", debug_dir)
    log.info("profile dir: %s", args.profile_dir)
    log.info("initial URL: %s", args.url)

    # Source the env file (no-op if absent). setdefault semantics mean a
    # host-set value wins, so the env file can be overridden by an explicit
    # `export RAIFFEISEN_AT_PASSWORD=…` ahead of the invocation.
    if envfile.source_env_file(args.env_file):
        log.info("env file:    %s (sourced)", args.env_file)
    username = os.environ.get(USER_ENV, "")
    password = os.environ.get(PASS_ENV, "")
    prefill_enabled = (
        not args.no_prefill
        and bool(username) and bool(password)
    )
    if args.no_prefill:
        log.info("--no-prefill: login form pre-fill disabled")
    elif not (username and password):
        log.warning(
            "%s / %s not set; login-form pre-fill disabled. "
            "Drop credentials into %s to enable.",
            USER_ENV, PASS_ENV, args.env_file,
        )
    else:
        log.info("pre-fill ready (origin-gated to raiffeisen.at)")

    from camoufox.sync_api import Camoufox
    # PlaywrightTimeoutError is the exception type wait_for_event raises
    # when the per-iteration timeout expires; importing here so the module
    # loads without playwright installed at import time.
    from playwright.sync_api import TimeoutError as PlaywrightTimeoutError

    # Track which downloads we've sequenced into filenames so two downloads
    # with the same suggested_filename (banks reuse names like
    # kontoauszug.pdf across accounts and ranges) don't collide.
    download_seq = {"n": 0}

    with contextlib.ExitStack() as stack:
        clicks_fp = stack.enter_context(open(clicks_path, "w", encoding="utf-8"))
        network_fp = stack.enter_context(open(network_path, "w", encoding="utf-8"))

        def write_event(payload: dict) -> None:
            clicks_fp.write(json.dumps(payload) + "\n")
            clicks_fp.flush()

        def write_network_event(payload: dict) -> None:
            network_fp.write(json.dumps(payload, default=str) + "\n")
            network_fp.flush()

        # Credential redaction for network.jsonl. The login POST carries
        # the password in its body; without this it would land in the debug
        # log in plaintext. Defence-in-depth — debug-dir files are not under
        # .secrets/, so a debug-dir leak is a real risk. The shared
        # redactor knows every spelling a credential takes on the wire —
        # percent-encoded in a form body, escaped in a JSON one — because a
        # literal-substring masker let a percent-encoded password through
        # into a capture in cleartext.
        redact = debugcap.secret_redactor(username, password)

        # The HAR is Playwright's own recording, and it records whole: the
        # login POST body, every header, the cookie jar. It is the one
        # capture in this dir written by the driver rather than by the
        # handlers, and it exists only once the context close has flushed
        # it. Registered on the stack rather than called after the block so
        # a walk that raises is cleaned too — an unwind closes the context,
        # which flushes the HAR, and a run that crashed is exactly the one
        # whose debug dir gets opened. LIFO puts this after the close.
        stack.callback(
            lambda: debugcap.redact_har(har_path, redact, log=log))

        # Camoufox launched with persistent_context returns a BrowserContext
        # directly. record_har_path enables HAR capture for the whole
        # context's lifetime. We enter it manually (not via enter_context) so
        # the unwind can swallow the TargetClosedError its __exit__ raises on
        # the browser-X-closed path — browser.close() against an already-dead
        # browser. By then the artefacts are flushed (line-buffered logs +
        # periodic trace chunks), so that error is pure noise and shouldn't
        # turn a clean discovery session into a non-zero exit.
        #
        # launch.firefox_prefs() disables Firefox's password manager
        # (signon.*) so a profile-saved credential can never autofill on
        # top of the programmatic pre-fill and double the field, and keeps
        # the profile down to session state (no disk cache / history /
        # telemetry).
        _cam = Camoufox(
            persistent_context=True,
            user_data_dir=str(args.profile_dir),
            os="macos",
            window=(1280, 800),
            headless=False,
            humanize=True,
            geoip=True,
            record_har_path=str(har_path),
            firefox_user_prefs=launch.firefox_prefs(),
        )
        context = _cam.__enter__()

        def _close_camoufox() -> None:
            try:
                _cam.__exit__(None, None, None)
            except Exception as exc:
                if "closed" in repr(exc).lower():
                    log.info("browser already closed on exit "
                             "(artefacts flushed): %s", exc)
                else:
                    raise
        stack.callback(_close_camoufox)
        if args.trace:
            context.tracing.start(
                screenshots=True, snapshots=True, sources=True,
            )
            # Use chunks so we can flush partial traces every N seconds
            # during the polling loop. Worst-case data loss on browser-X
            # close is bounded to args.chunk_interval seconds.
            context.tracing.start_chunk()
        context.add_init_script(CLICK_RECORDER_JS)

        # Network logger — manual replacement for HAR. Playwright's
        # record_har_path only flushes on context.close(), which doesn't
        # survive browser-X close (the underlying browser process is already
        # dead by the time the Python side reacts). network.jsonl is
        # line-flushed per event so the log is crash-safe.
        SKIP_RESOURCE_TYPES = {
            "image", "font", "media", "stylesheet", "manifest",
        }
        # "ofx" / "qfx" stay in as defence even though CSV is the only
        # export expected here — the capture should notice, not miss, any
        # other structured format the traffic happens to reveal. Direct
        # downloads land under downloads/ via the page.on('download') hook
        # regardless.
        TEXT_CONTENT_HINTS = (
            "json", "html", "plain", "csv", "xml", "urlencoded",
            "ofx", "qfx",
        )

        def on_request(request) -> None:
            try:
                if request.resource_type in SKIP_RESOURCE_TYPES:
                    return
                write_network_event({
                    "kind": "request",
                    "ts": _now_iso(),
                    "method": request.method,
                    "url": debugcap.redact_url(redact(request.url)),
                    "resource_type": request.resource_type,
                    # Two redactions, because they catch different
                    # things: `redact` masks the values known in advance
                    # (the credentials), while `redact_headers` masks by
                    # header NAME — the only way to catch one the site
                    # issues at runtime, like a session cookie or the
                    # SPA's own api key.
                    "headers": debugcap.redact_headers(
                        {k: redact(v) for k, v in request.headers.items()}),
                    "post_data": redact(request.post_data) if request.method == "POST" else None,
                })
            except Exception as exc:
                log.debug("on_request error: %r", exc)

        def on_response(response) -> None:
            try:
                if response.request.resource_type in SKIP_RESOURCE_TYPES:
                    return
                ct = response.headers.get("content-type", "").lower()
                payload = {
                    "kind": "response",
                    "ts": _now_iso(),
                    "url": debugcap.redact_url(redact(response.url)),
                    "method": response.request.method,
                    "status": response.status,
                    "resource_type": response.request.resource_type,
                    # Two redactions, because they catch different
                    # things: `redact` masks the values known in advance
                    # (the credentials), while `redact_headers` masks by
                    # header NAME — the only way to catch one the site
                    # issues at runtime, like a session cookie or the
                    # SPA's own api key.
                    "headers": debugcap.redact_headers(
                        {k: redact(v) for k, v in response.headers.items()}),
                }
                # Body capture: only for likely-interesting text-shaped
                # responses, capped at 200 KB. Skips bundled JS/CSS and
                # binary payloads. Downloaded files end up under downloads/
                # via the page.on('download') hook anyway.
                if any(t in ct for t in TEXT_CONTENT_HINTS):
                    try:
                        body = response.body()
                    except Exception as exc:
                        payload["body_error"] = repr(exc)
                    else:
                        if len(body) < 200_000:
                            try:
                                payload["body_text"] = redact(body.decode("utf-8"))
                            except UnicodeDecodeError:
                                payload["body_b64"] = base64.b64encode(body).decode()
                        else:
                            payload["body_size"] = len(body)
                            payload["body_truncated"] = True
                write_network_event(payload)
            except Exception as exc:
                log.debug("on_response error: %r", exc)

        context.on("request", on_request)
        context.on("response", on_response)

        # Periodic trace chunk save. Each chunk is an independent playwright
        # trace zip — open with `playwright show-trace chunk-NNN.zip`. The
        # polling loop below drives the cadence.
        chunk_seq = {"n": 0}
        def save_trace_chunk(label="periodic") -> bool:
            if not args.trace:
                return False
            chunk_seq["n"] += 1
            chunk_path = trace_chunks_dir / f"chunk-{chunk_seq['n']:03d}-{label}.zip"
            try:
                context.tracing.stop_chunk(path=str(chunk_path))
                context.tracing.start_chunk()
                return True
            except Exception as exc:
                log.debug("trace chunk save failed: %r", exc)
                return False

        # Fields already filled, keyed (id(page), kind) — the fill-once
        # bookkeeping _maybe_prefill_login maintains.
        prefilled: set = set()

        def on_page(page) -> None:
            # The console listener needs `page` in its closure so the
            # login-form-detected handler can issue the Playwright fill
            # against the right tab.
            def on_console(msg) -> None:
                text = msg.text
                if not text.startswith(EVENT_PREFIX):
                    return
                try:
                    payload = json.loads(text[len(EVENT_PREFIX):])
                except ValueError:
                    return
                # The init-script's MutationObserver fires this when a login
                # form mounts in a raiffeisen.at frame. Bounce the fill
                # back through Playwright so credentials never reach the
                # page's JS context as plain strings.
                if (prefill_enabled
                        and payload.get("kind") == "login-form-detected"):
                    write_event(payload)
                    if _maybe_prefill_login(page, username, password,
                                            prefilled):
                        write_event({
                            "kind": "credentials-prefilled",
                            "ts": _now_iso(),
                            "url": page.url,
                        })
                        log.info("login form pre-filled on %s",
                                 page.url[:80])
                    return
                write_event(payload)

            def on_download(download) -> None:
                # Materialise the bytes immediately. blob: URLs and
                # token-gated server downloads alike — Playwright's
                # save_as() handles both. Without this call the bytes are
                # discarded when the Download object is GC'd. Sequence prefix
                # prevents collisions when the site reuses suggested_filename
                # (e.g. kontoauszug.pdf) across accounts and ranges.
                download_seq["n"] += 1
                seq = download_seq["n"]
                suggested = download.suggested_filename or f"download-{seq}"
                # Strip path separators from suggested name as a
                # defence-in-depth measure (site-generated, but still
                # untrusted input).
                safe = suggested.replace("/", "_").replace("\\", "_")
                out_path = downloads_dir / f"{seq:02d}-{safe}"
                event = {
                    "kind": "download",
                    "ts": _now_iso(),
                    "url": download.url,
                    "suggested_filename": suggested,
                    "saved_to": str(out_path),
                }
                try:
                    download.save_as(str(out_path))
                except Exception as exc:
                    event["save_error"] = repr(exc)
                write_event(event)

            page.on("console", on_console)
            page.on("framenavigated", lambda f: f == page.main_frame and write_event({
                "kind": "navigation",
                "ts": _now_iso(),
                "url": f.url,
            }))
            page.on("download", on_download)

        context.on("page", on_page)

        # SIGTERM (docker stop) sets a flag the polling loop picks up. SIGINT
        # (Ctrl-C) is left on Python's default handler so it raises
        # KeyboardInterrupt, which unwinds the with-block cleanly —
        # tracing.stop() runs in the finally and Camoufox's context-manager
        # exit flushes the HAR.
        done = threading.Event()
        def _on_sigterm(signum, frame):  # noqa: ARG001
            write_event({
                "kind": "signal",
                "ts": _now_iso(),
                "signal": "SIGTERM",
            })
            done.set()
        signal.signal(signal.SIGTERM, _on_sigterm)

        page = context.new_page()
        page.goto(args.url, wait_until="domcontentloaded")
        write_event({
            "kind": "started",
            "ts": _now_iso(),
            "url": args.url,
        })
        # Belt-and-braces: try an immediate pre-fill in case the form is
        # already in the initial DOM (the init-script's detect() runs on
        # script attach, but on some pages the framework's first render
        # hasn't fired yet when the script executes).
        if prefill_enabled and _maybe_prefill_login(page, username, password,
                                                    prefilled):
            write_event({
                "kind": "credentials-prefilled",
                "ts": _now_iso(),
                "url": page.url,
            })
            log.info("login form pre-filled on initial page")
        log.info("recording started — walk the three discovery flows "
                 "(DESIGN.md §3): 1) log in via VNC, noting the pushTAN "
                 "challenge's screen + completion signal and any 'trust "
                 "this browser' control; 2) open the statement generator "
                 "(Kontoauszug) and generate at least one PDF over an "
                 "explicit date range, probing its maximum range; 3) open "
                 "each account's transaction history and run the CSV "
                 "export with an explicit date range. Then EITHER close "
                 "the browser window OR Ctrl-C the terminal to stop. Both "
                 "paths flush artefacts: %sthe line-buffered clicks.jsonl "
                 "/ network.jsonl land on disk continuously.",
                 f"trace chunks every {args.chunk_interval}s + "
                 if args.trace else "")

        # Wait for the browser to close OR SIGTERM (done flag) OR SIGINT
        # (KeyboardInterrupt) OR max-duration timeout. Polled in 1-second
        # chunks so SIGTERM is responsive and KeyboardInterrupt can
        # propagate out of wait_for_event at the next iteration boundary.
        # Trace chunks are rotated inside the same loop on a separate
        # cadence.
        deadline = time.monotonic() + args.max_duration
        last_chunk_at = time.monotonic()
        last_prefill_at = 0.0
        last_dom_at = 0.0
        dom_seq = 0
        last_dom_skeleton = ""
        exit_reason = "timeout"
        try:
            while time.monotonic() < deadline:
                if done.is_set():
                    exit_reason = "sigterm"
                    break
                # Periodic DOM snapshot — captures every distinct screen the
                # click log misses (cross-origin login/2FA iframe). Deduped
                # by structure, so idle ticks cost nothing.
                if args.dom_interval > 0 and (
                        time.monotonic() - last_dom_at >= args.dom_interval):
                    last_dom_at = time.monotonic()
                    try:
                        dom_seq, last_dom_skeleton = _capture_dom_snapshot(
                            context, dom_dir, dom_seq, last_dom_skeleton, log,
                            redact)
                    except Exception as exc:
                        log.debug("dom snapshot error: %r", exc)
                # Periodic login pre-fill. The JS console detector only
                # fires on field-set changes; this poll covers SPA route
                # changes and late-mounting frames by re-checking every
                # page. No-op once the fields are marked filled, so it's
                # cheap.
                if prefill_enabled and time.monotonic() - last_prefill_at >= 1.5:
                    last_prefill_at = time.monotonic()
                    for pg in list(context.pages):
                        try:
                            if _maybe_prefill_login(pg, username, password,
                                                    prefilled):
                                write_event({
                                    "kind": "credentials-prefilled",
                                    "ts": _now_iso(),
                                    "url": pg.url,
                                })
                                log.info("pre-filled login field(s) on %s",
                                         pg.url[:70])
                        except Exception as exc:
                            log.debug("prefill poll error: %r", exc)
                try:
                    context.wait_for_event("close", timeout=1000)
                    exit_reason = "browser-closed"
                    break
                except PlaywrightTimeoutError:
                    now = time.monotonic()
                    if now - last_chunk_at >= args.chunk_interval:
                        save_trace_chunk()
                        last_chunk_at = now
        except KeyboardInterrupt:
            write_event({
                "kind": "signal",
                "ts": _now_iso(),
                "signal": "SIGINT",
            })
            exit_reason = "sigint"

        # One last DOM snapshot so the final screen is always captured, even
        # if it changed within the last interval before the browser closed.
        if args.dom_interval > 0 and exit_reason != "browser-closed":
            with contextlib.suppress(Exception):
                dom_seq, last_dom_skeleton = _capture_dom_snapshot(
                    context, dom_dir, dom_seq, last_dom_skeleton, log, redact)

        # Finally: ALWAYS write the trace + the stop event, even on signal /
        # exception. For Ctrl-C / SIGTERM / timeout the context is still
        # alive so the final chunk + tracing.stop() both succeed; for
        # browser-closed the context is already dead, but the periodic
        # chunks captured incrementally above are already on disk.
        write_event({
            "kind": "stopped",
            "ts": _now_iso(),
            "reason": exit_reason,
        })
        if args.trace:
            final_ok = save_trace_chunk(label="final")
            log.info("stopping (reason: %s) — %d trace chunk(s) saved "
                     "(final chunk: %s)", exit_reason, chunk_seq["n"],
                     "ok" if final_ok
                     else "browser dead, last periodic chunk is most-recent")
            with contextlib.suppress(Exception):
                context.tracing.stop(path=str(trace_path))
        else:
            log.info("stopping (reason: %s)", exit_reason)

    log.info("artefacts written:")
    log.info("  clicks:        %s  (events + lifecycle)", clicks_path)
    log.info("  network:       %s  (requests + responses, crash-safe)",
             network_path)
    log.info("  HAR:           %s  (redacted; best-effort — complete on "
             "Ctrl-C/SIGTERM exit, may be missing on browser-X close)",
             har_path)
    if args.trace:
        log.info("  trace-chunks/: %s/  (%d chunk(s); open with "
                 "`playwright show-trace chunk-NNN.zip`)",
                 trace_chunks_dir, chunk_seq["n"])
        log.info("  trace.zip:     %s  (best-effort; final-flush attempt; "
                 "trace-chunks/ is the durable record). UNREDACTED: a "
                 "trace carries the typed credential — never commit it",
                 trace_path)
    log.info("  downloads:     %s/  (%d file(s))",
             downloads_dir, download_seq["n"])
    if args.dom_interval > 0:
        log.info("  dom/:          %s/  (%d distinct screen(s), all frames + "
                 "screenshot)", dom_dir, dom_seq)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
