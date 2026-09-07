#!/usr/bin/env python3
"""carta discovery harness.

Launches Camoufox in the container's Xvfb display, navigates to the Carta
holder UI, and records every action taken in the VNC session so login.py +
download.py can be written from real traces:

  - **HAR** (`network.har`)        — every request + response with
                                     headers and bodies. The primary
                                     artefact for finding the internal
                                     JSON/XHR endpoints the SPA hits.
                                     Read it against the observed
                                     endpoint map in DESIGN.md §3 —
                                     internal shapes differ from the
                                     public /v1alpha1/ map of §2.
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
                                     `CARTA_USERNAME` / `CARTA_PASSWORD`
                                     (sourced from `/secrets/carta.env`).
                                     Sign in + 2FA are still driven by hand.
                                     Pass --no-prefill to skip the pre-fill
                                     and type the credentials manually.

This is read-only observation. Per CLAUDE.md, never click an Exercise /
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

log = logging.getLogger("carta.explore")

# app.carta.com is the holder app; it redirects to the login page when the
# session is missing. Override with --url if explore reveals a different
# entry host.
DEFAULT_URL = "https://app.carta.com"
DEFAULT_PROFILE_DIR = Path("/secrets/carta-profile")
DEFAULT_DEBUG_ROOT = Path("/debug")
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
    p.add_argument(
        "--profile-dir", type=Path, default=DEFAULT_PROFILE_DIR,
        help=("Persistent Camoufox profile dir. The login cookie + any "
              "'remember this device' state lives here so subsequent runs "
              "can skip the 2FA prompt. Default: %(default)s."),
    )
    p.add_argument(
        "--debug-dir", type=Path, default=None,
        help=("Where to write HAR + trace + click log. Defaults to "
              "/debug/<UTC-ts>/ (mounted from "
              "~/.cache/wealthdb/debug/carta on the host)."),
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
        help=("Path to a bash-sourced env file with CARTA_USERNAME / "
              "CARTA_PASSWORD. Skipped silently if absent. "
              "Default: %(default)s."),
    )
    p.add_argument(
        "--no-prefill", action="store_true",
        help=("Skip pre-filling the Carta login form. Use when you want to "
              "verify the form selectors by typing the credentials "
              "yourself (recommended for the first run against Carta's "
              "Cloudflare-Turnstile-fronted login), or when $CARTA_EMAIL / "
              "$CARTA_PASSWORD are intentionally unset."),
    )
    p.add_argument(
        "--fresh", action="store_true",
        help=("Wipe the persistent Camoufox profile dir before launching. "
              "Use this to force a fresh login: clears cookies including "
              "any long-lived 'remember device' cookie that lets Carta skip "
              "2FA. The next session will require typing the 2FA code."),
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


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)

    ts = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    debug_dir = args.debug_dir or (DEFAULT_DEBUG_ROOT / ts)
    debug_dir.mkdir(parents=True, exist_ok=True)
    downloads_dir = debug_dir / "downloads"
    downloads_dir.mkdir(parents=True, exist_ok=True)
    trace_chunks_dir = debug_dir / "trace-chunks"
    if args.trace:
        trace_chunks_dir.mkdir(parents=True, exist_ok=True)
    har_path = debug_dir / "network.har"
    trace_path = debug_dir / "trace.zip"
    clicks_path = debug_dir / "clicks.jsonl"
    network_path = debug_dir / "network.jsonl"

    # --fresh wipes the persistent profile so the next launch hits the 2FA
    # challenge again. Useful for capturing the 2FA selectors and the full
    # first-login traffic in the trace. Must run BEFORE the profile-dir
    # prep below.
    if args.fresh and args.profile_dir.exists():
        log.warning("--fresh: wiping profile dir %s "
                    "(2FA will be required on next login)", args.profile_dir)
        shutil.rmtree(args.profile_dir)
    launch.prepare_profile_dir(args.profile_dir)

    log.info("debug dir:   %s", debug_dir)
    log.info("profile dir: %s", args.profile_dir)
    log.info("initial URL: %s", args.url)

    # Source the env file (no-op if absent). setdefault semantics mean a
    # host-set value wins, so the env file can be overridden by an explicit
    # `export CARTA_PASSWORD=…` ahead of the invocation.
    if envfile.source_env_file(args.env_file):
        log.info("env file:    %s (sourced)", args.env_file)
    username = next((os.environ[k] for k in USER_ENVS if os.environ.get(k)), "")
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
        log.info("pre-fill ready (origin-gated to carta.com)")

    from camoufox.sync_api import Camoufox
    # PlaywrightTimeoutError is the exception type wait_for_event raises
    # when the per-iteration timeout expires; importing here so the module
    # loads without playwright installed at import time.
    from playwright.sync_api import TimeoutError as PlaywrightTimeoutError

    # Track which downloads we've sequenced into filenames so two downloads
    # with the same suggested_filename don't collide.
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

        # Credential redaction for network.jsonl. Login POSTs carry the
        # password in the form body; without this it would land in the debug
        # log in plaintext. Defence-in-depth — debug-dir files are not under
        # .secrets/, so a debug-dir leak is a real risk.
        # The shared redactor knows every spelling a credential
        # takes on the wire — percent-encoded in a form body,
        # escaped in a JSON one — because a literal-substring
        # masker let a percent-encoded password through into a
        # capture in cleartext.
        redact = debugcap.secret_redactor(username, password)

        # Camoufox launched with persistent_context returns a BrowserContext
        # directly. record_har_path enables HAR capture for the whole
        # context's lifetime. We enter it manually (not via enter_context) so
        # the unwind can swallow the TargetClosedError its __exit__ raises on
        # the browser-X-closed path — browser.close() against an already-dead
        # browser. By then the artefacts are flushed (line-buffered logs +
        # periodic trace chunks), so that error is pure noise and shouldn't
        # turn a clean discovery session into a non-zero exit.
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
        TEXT_CONTENT_HINTS = (
            "json", "html", "plain", "csv", "xml", "urlencoded",
        )

        def on_request(request) -> None:
            try:
                if request.resource_type in SKIP_RESOURCE_TYPES:
                    return
                write_network_event({
                    "kind": "request",
                    "ts": _now_iso(),
                    "method": request.method,
                    "url": request.url,
                    "resource_type": request.resource_type,
                    "headers": {k: redact(v) for k, v in request.headers.items()},
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
                    "url": response.url,
                    "method": response.request.method,
                    "status": response.status,
                    "resource_type": response.request.resource_type,
                    "headers": {k: redact(v) for k, v in response.headers.items()},
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
                # form mounts on carta.com. Bounce the fill back through
                # Playwright so credentials never reach the page's JS context
                # as plain strings.
                if (prefill_enabled
                        and payload.get("kind") == "login-form-detected"):
                    write_event(payload)
                    if _maybe_prefill_login(page, username, password):
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
                # prevents collisions when Carta reuses suggested_filename
                # (e.g. statement.pdf) across issuers/funds.
                download_seq["n"] += 1
                seq = download_seq["n"]
                suggested = download.suggested_filename or f"download-{seq}"
                # Strip path separators from suggested name as a
                # defence-in-depth measure (Carta-generated, but still
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
        if prefill_enabled and _maybe_prefill_login(page, username, password):
            write_event({
                "kind": "credentials-prefilled",
                "ts": _now_iso(),
                "url": page.url,
            })
            log.info("login form pre-filled on initial page")
        log.info("recording started — log in via VNC, click through the "
                 "pages we want to scrape (the stock-comp plan AND the fund "
                 "LP statements), then EITHER close the browser window OR "
                 "Ctrl-C the terminal to stop. Both paths flush artefacts: "
                 "%sthe line-buffered clicks.jsonl / network.jsonl land on "
                 "disk continuously.",
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
        exit_reason = "timeout"
        try:
            while time.monotonic() < deadline:
                if done.is_set():
                    exit_reason = "sigterm"
                    break
                # Periodic login pre-fill. The JS console detector only fires
                # when BOTH fields are present at once (single-step forms);
                # this poll covers two-step (email → password) forms and SPA
                # route changes by filling whichever field is present + empty.
                # No-op once the fields are filled or absent, so it's cheap.
                if prefill_enabled and time.monotonic() - last_prefill_at >= 1.5:
                    last_prefill_at = time.monotonic()
                    for pg in list(context.pages):
                        try:
                            if _maybe_prefill_login(pg, username, password):
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
    log.info("  HAR:           %s  (best-effort; complete on Ctrl-C/SIGTERM exit, may be missing on browser-X close)",
             har_path)
    if args.trace:
        log.info("  trace-chunks/: %s/  (%d chunk(s); open with "
                 "`playwright show-trace chunk-NNN.zip`)",
                 trace_chunks_dir, chunk_seq["n"])
        log.info("  trace.zip:     %s  (best-effort; final-flush attempt; "
                 "trace-chunks/ is the durable record)", trace_path)
    log.info("  downloads:     %s/  (%d file(s))",
             downloads_dir, download_seq["n"])
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
