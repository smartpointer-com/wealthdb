#!/usr/bin/env python3
"""angellist discovery harness.

Launches Camoufox in the container's Xvfb display, navigates to the
AngelList Investor Portal, and records every action taken in the VNC
session so download.py can be written from real traces:

  - **HAR** (`network.har`)        — every request + response with
                                     headers and bodies. The primary
                                     artefact for finding the internal
                                     JSON/XHR endpoints the React SPA
                                     hits.
  - **network.jsonl**              — a crash-safe, line-flushed mirror
                                     of the request/response stream
                                     (HAR only flushes on a clean
                                     close, which a browser-X close
                                     does not survive).
  - **Playwright trace**
    (`trace.zip` + trace-chunks/)  — screenshots + DOM snapshots +
                                     network events at every action.
                                     Open with `playwright show-trace`.
  - **Click log**
    (`clicks.jsonl`)               — one JSON object per click on the
                                     page (timestamp, URL, tag, id,
                                     text, xpath). Captured via a
                                     `document.addEventListener
                                     ('click', …)` init script because
                                     user-driven VNC clicks bypass the
                                     Playwright API. Plus a
                                     `MutationObserver` watch that
                                     signals when a login form has
                                     mounted — the Python side then
                                     pre-fills the form from
                                     `ANGELLIST_USERNAME` /
                                     `ANGELLIST_PASSWORD` (sourced from
                                     `/secrets/angellist.env` inside the
                                     container). Operator still clicks
                                     Login + handles 2FA.

What this run needs to map (see DESIGN.md): the login host (possibly a
fund-branded subdomain) + form selectors, the 2FA factor + any
"trust this device" option, whether the authenticated surface needs
Camoufox stealth, and the page/endpoint surfaces holding the LP
portfolio summary, per-vehicle capital-account detail, the
funding-account cash ledger, and the tax-document / K-1 centre.

Recording stops when the last browser window is closed (Camoufox's
persistent context fires `close`) or after `--max-duration` (default
1h) as a safety net. Artefacts land under `/debug/<UTC-ts>/` so the
bronze + silver tree under `/data` stays clean.

Read-only: this is for observation. Per CLAUDE.md, do not click any
mutate/confirm/submit control beyond the login + 2FA forms, and stay
out of any syndicate-lead / fund-admin surface the login may expose.
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

from collectorkit import cli, envfile

log = logging.getLogger("angellist.explore")

DEFAULT_URL = "https://venture.angellist.com/v/login"
DEFAULT_PROFILE_DIR = Path("/secrets/angellist-profile")
DEFAULT_DEBUG_ROOT = Path("/debug")
DEFAULT_ENV_FILE = Path("/secrets/angellist.env")
USER_ENV = "ANGELLIST_USERNAME"
PASS_ENV = "ANGELLIST_PASSWORD"
EVENT_PREFIX = "__AL_EVENT__ "

# Pre-fill is restricted to this host (and subdomains) so credentials
# never leak into an embedded third-party iframe. angellist.com covers
# the main site and any fund-branded *.angellist.com investor portal; a
# white-label domain (if any) simply won't pre-fill and the operator
# types the credentials by hand.
HOST_RE = re.compile(r"(^|\.)angellist\.com$", re.I)

# Locators tried in order. The explore phase will tell us which one
# actually matches the Investor Portal's form — we keep a few common
# variants as a starting set, anchored on standard HTML conventions
# (input[type=email] / autocomplete=username) plus name-substring
# fallbacks. Same approach for the password input.
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
#   2. Login-form detector — once the portal renders a login form
#      (SPA: may be after the initial DOM), signal Python via a
#      console.log() sentinel so the Python side can fill it. The
#      detector is host-gated so it can't fire on third-party iframes,
#      and uses a window-scoped sentinel to fire once per page-load.
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
    console.log('__AL_EVENT__ ' + JSON.stringify(data));
  }, true);

  // Login-form detector. Host-gated; fires once per window.
  if (!/(^|\.)angellist\.com$/i.test(location.hostname)) return;
  const detect = () => {
    if (window.__AL_LOGIN_DETECTED__) return;
    const user = document.querySelector(
      "input[type='email'], input[autocomplete='username'], " +
      "input[autocomplete='email'], input[name*='email' i], " +
      "input[name*='user' i], input[name*='login' i]"
    );
    const pwd = document.querySelector("input[type='password']");
    if (user && pwd) {
      window.__AL_LOGIN_DETECTED__ = true;
      console.log('__AL_EVENT__ ' + JSON.stringify({
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
    p.add_argument(
        "--profile-dir", type=Path, default=DEFAULT_PROFILE_DIR,
        help=("Persistent Camoufox profile dir. The login cookie + any "
              "'remember this device' state lives here so subsequent "
              "runs can skip the MFA prompt. Default: %(default)s."),
    )
    p.add_argument(
        "--debug-dir", type=Path, default=None,
        help=("Where to write HAR + trace + click log. Defaults to "
              "/debug/<UTC-ts>/ (mounted from "
              "$HOME/.cache/angellist-debug on the host)."),
    )
    p.add_argument(
        "--url", default=DEFAULT_URL,
        help="Initial URL to open. Default: %(default)s.",
    )
    p.add_argument(
        "--max-duration", type=int, default=3600,
        help=("Safety net: auto-close the recording after N seconds "
              "even if the browser is left open. "
              "Default: %(default)s (1 hour)."),
    )
    p.add_argument(
        "--env-file", type=Path, default=DEFAULT_ENV_FILE,
        help=("Path to a bash-sourced env file with "
              "ANGELLIST_USERNAME / ANGELLIST_PASSWORD. "
              "Skipped silently if absent. Default: %(default)s."),
    )
    p.add_argument(
        "--no-prefill", action="store_true",
        help=("Skip pre-filling the Investor Portal login form. "
              "Use when you want to verify the form selectors by "
              "typing the credentials yourself, or when "
              "$ANGELLIST_USERNAME / $ANGELLIST_PASSWORD are "
              "intentionally unset. Recommended against the anti-bot "
              "challenge: a programmatic .fill() has no keystroke/timing "
              "signals and scores as automation — type by hand instead."),
    )
    p.add_argument(
        "--no-recorder", action="store_true",
        help=("Don't inject the click-recorder init script. Reduces "
              "page-side tampering when fighting an invisible anti-bot "
              "challenge (Turnstile / reCAPTCHA); network.jsonl + the "
              "Playwright trace still capture everything passively."),
    )
    p.add_argument(
        "--warmup", action="store_true",
        help=("Visit google.com and dwell briefly before opening the "
              "target, to age the session + accrue cookies so the "
              "anti-bot score isn't computed on a stone-cold profile."),
    )
    p.add_argument(
        "--cookies", type=Path, default=None,
        help=("Inject cookies from this JSON file (Playwright "
              "add_cookies format, as written by extract_cookies.py) "
              "before navigating — the BYO-session path that lands on "
              "the authenticated portal instead of the bot-walled login."),
    )
    p.add_argument(
        "--dump-links", action="store_true",
        help=("After the page settles, log every in-app angellist.com "
              "<a href> link and write them to links.txt — discovers the "
              "portal's section routes (portfolio/documents/…) during "
              "BYO discovery."),
    )
    p.add_argument(
        "--fresh", action="store_true",
        help=("Wipe the persistent Camoufox profile dir before launching. "
              "Use this to force a fresh login: clears cookies including "
              "any long-lived 'remember device' cookie that lets the "
              "portal skip 2FA. The next session will require typing the "
              "2FA code."),
    )
    p.add_argument(
        "--chunk-interval", type=int, default=30,
        help=("Seconds between incremental trace-chunk saves. Lower = "
              "less data loss on abrupt close, more disk I/O. Default: "
              "%(default)s."),
    )
    cli.add_common_args(p)
    return p.parse_args(argv)


def _now_iso() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="seconds")


def _maybe_prefill_login(page, username: str, password: str) -> bool:
    """If the current page is on angellist.com AND a login form is
    present + visible AND nothing has been typed into the fields yet,
    fill the credentials. Returns True iff the fill actually happened.
    Never submits — Login + 2FA are driven manually in the VNC
    session."""
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
        # Operator has already typed something; don't overwrite.
        return False
    try:
        user_field.fill(username, timeout=2000)
        pwd_field.fill(password, timeout=2000)
    except Exception as e:
        log.debug("login-form fill failed: %s", e)
        return False
    return True


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)

    ts = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    debug_dir = args.debug_dir or (DEFAULT_DEBUG_ROOT / ts)
    debug_dir.mkdir(parents=True, exist_ok=True)
    downloads_dir = debug_dir / "downloads"
    downloads_dir.mkdir(parents=True, exist_ok=True)
    trace_chunks_dir = debug_dir / "trace-chunks"
    trace_chunks_dir.mkdir(parents=True, exist_ok=True)
    har_path = debug_dir / "network.har"
    trace_path = debug_dir / "trace.zip"
    clicks_path = debug_dir / "clicks.jsonl"
    network_path = debug_dir / "network.jsonl"

    # --fresh wipes the persistent profile so the next launch hits the
    # 2FA challenge again. Useful for capturing the 2FA selectors and
    # the full first-login traffic in the trace. Must run BEFORE the
    # profile-dir mkdir below.
    if args.fresh and args.profile_dir.exists():
        log.warning("--fresh: wiping profile dir %s "
                    "(2FA will be required on next login)", args.profile_dir)
        shutil.rmtree(args.profile_dir)
    args.profile_dir.mkdir(parents=True, exist_ok=True)

    log.info("debug dir:   %s", debug_dir)
    log.info("profile dir: %s", args.profile_dir)
    log.info("initial URL: %s", args.url)

    # Source the env file (no-op if absent). setdefault semantics
    # mean a host-set value wins, so the env file can be overridden
    # by an explicit `export ANGELLIST_PASSWORD=…` ahead of the
    # invocation.
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
        log.info("pre-fill ready (origin-gated to angellist.com)")

    from camoufox.sync_api import Camoufox
    # PlaywrightTimeoutError is the exception type wait_for_event
    # raises when the per-iteration timeout expires; importing here
    # so the module loads without playwright installed at import time.
    from playwright.sync_api import TimeoutError as PlaywrightTimeoutError

    # Track which downloads we've sequenced into filenames so two
    # downloads with the same suggested_filename don't collide.
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
        # password in the form body; without this it would land in the
        # debug log in plaintext. Defence-in-depth — debug-dir files
        # are not under .secrets/, so a debug-dir leak is a real risk.
        secrets_to_redact = [s for s in (username, password) if s]
        def redact(s):
            if not s or not secrets_to_redact:
                return s
            for sec in secrets_to_redact:
                s = s.replace(sec, "<REDACTED>")
            return s

        # Camoufox launched with persistent_context returns a
        # BrowserContext directly. record_har_path enables HAR capture
        # for the whole context's lifetime. block_webrtc stops a STUN
        # WebRTC probe from leaking the container's internal 172.x IP,
        # which mismatches the public egress IP and reads as a bot
        # signal to reCAPTCHA / Turnstile.
        #
        # Entered manually (not stack.enter_context) so the close-time
        # callback can swallow the TargetClosedError that Camoufox's
        # __exit__ raises on the browser-X close path: browser.close()
        # runs against an already-dead browser. All artefacts are
        # already flushed by then (line-buffered jsonl + periodic trace
        # chunks), so a close-time error is benign and must not turn a
        # good capture into a non-zero exit.
        _camoufox_cm = Camoufox(
            persistent_context=True,
            user_data_dir=str(args.profile_dir),
            os="macos",
            window=(1280, 800),
            headless=False,
            humanize=True,
            geoip=True,
            block_webrtc=True,
            record_har_path=str(har_path),
        )
        context = _camoufox_cm.__enter__()

        def _close_camoufox():
            try:
                _camoufox_cm.__exit__(None, None, None)
            except Exception as exc:
                log.debug("benign Camoufox close-time error: %r", exc)
        stack.callback(_close_camoufox)

        # BYO-session cookie injection: load a real-browser session
        # (lifted from Firefox by extract_cookies.py) so navigation lands
        # on the authenticated portal instead of the bot-walled login.
        if args.cookies:
            byo = json.loads(Path(args.cookies).read_text(encoding="utf-8"))
            context.add_cookies(byo)
            log.info("injected %d BYO cookie(s) from %s (names: %s)",
                     len(byo), args.cookies,
                     ", ".join(sorted(c.get("name", "?") for c in byo)))
        context.tracing.start(
            screenshots=True, snapshots=True, sources=True,
        )
        # Use chunks so we can flush partial traces every N seconds
        # during the polling loop. Worst-case data loss on browser-X
        # close is bounded to args.chunk_interval seconds.
        context.tracing.start_chunk()
        if not args.no_recorder:
            context.add_init_script(CLICK_RECORDER_JS)
        else:
            log.info("--no-recorder: click-recorder init script disabled "
                     "(less page tampering vs the anti-bot challenge; "
                     "network.jsonl + trace still capture passively)")

        # Network logger — manual replacement for HAR. Playwright's
        # record_har_path only flushes on context.close(), which
        # doesn't survive browser-X close (the underlying browser
        # process is already dead by the time the Python side
        # reacts). network.jsonl is line-flushed per event so the
        # log is crash-safe.
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
                # binary payloads. The downloaded files end up under
                # downloads/ via the page.on('download') hook anyway,
                # so we don't need them duplicated here.
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

        # Periodic trace chunk save. Each chunk is an independent
        # playwright trace zip — open with `playwright show-trace
        # chunk-NNN.zip`. The polling loop below drives the cadence.
        chunk_seq = {"n": 0}
        def save_trace_chunk(label="periodic") -> bool:
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
            # login-form-detected handler can issue the Playwright
            # fill against the right tab.
            def on_console(msg) -> None:
                text = msg.text
                if not text.startswith(EVENT_PREFIX):
                    return
                try:
                    payload = json.loads(text[len(EVENT_PREFIX):])
                except ValueError:
                    return
                # The init-script's MutationObserver fires this when
                # a login form mounts on angellist.com. Bounce the
                # fill back through Playwright so credentials never
                # reach the page's JS context as plain strings.
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
                # save_as() handles both. Without this call the bytes
                # are discarded when the Download object is GC'd.
                # Sequence prefix prevents collisions when the portal
                # reuses suggested_filename across vehicles.
                download_seq["n"] += 1
                seq = download_seq["n"]
                suggested = download.suggested_filename or f"download-{seq}"
                # Strip path separators from suggested name as a
                # defence-in-depth measure (portal-generated, but
                # still untrusted input).
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

            if not args.no_recorder:
                page.on("console", on_console)
            page.on("framenavigated", lambda f: f == page.main_frame and write_event({
                "kind": "navigation",
                "ts": _now_iso(),
                "url": f.url,
            }))
            page.on("download", on_download)

        context.on("page", on_page)

        # SIGTERM (docker stop) sets a flag the polling loop picks
        # up. SIGINT (Ctrl-C) is left on Python's default handler so
        # it raises KeyboardInterrupt, which unwinds the with-block
        # cleanly — tracing.stop() runs in the finally and Camoufox's
        # context-manager exit flushes the HAR.
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
        if args.warmup:
            log.info("--warmup: visiting google.com to age the session + "
                     "accrue cookies before opening the target")
            try:
                page.goto("https://www.google.com",
                          wait_until="domcontentloaded", timeout=30_000)
                time.sleep(10)
            except Exception as exc:
                log.warning("warmup navigation failed (continuing): %s", exc)
        try:
            page.goto(args.url, wait_until="domcontentloaded", timeout=45_000)
        except Exception as exc:
            # A bad path / redirect loop / slow SPA shouldn't abort the
            # capture — record it and keep recording whatever loaded.
            log.warning("initial goto(%s) failed: %s — continuing to "
                        "capture whatever loaded", args.url,
                        str(exc).splitlines()[0] if str(exc) else exc)
        write_event({
            "kind": "started",
            "ts": _now_iso(),
            "url": args.url,
        })
        if args.dump_links:
            time.sleep(8)  # let the SPA render its nav
            try:
                hrefs = page.evaluate(
                    "() => [...new Set([...document.querySelectorAll("
                    "'a[href]')].map(a => a.href))]")
            except Exception as exc:
                hrefs = []
                log.warning("dump-links eval failed: %s", exc)
            al = sorted(h for h in hrefs if "angellist.com" in h)
            (debug_dir / "links.txt").write_text("\n".join(al) + "\n")
            log.info("dump-links: %d angellist link(s) -> %s",
                     len(al), debug_dir / "links.txt")
            for h in al:
                log.info("  link: %s", h)
        # Belt-and-braces: try an immediate pre-fill in case the form
        # is already in the initial DOM (the init-script's detect()
        # runs on script attach, but on some pages the framework's
        # first render hasn't fired yet when the script executes).
        if prefill_enabled and _maybe_prefill_login(page, username, password):
            write_event({
                "kind": "credentials-prefilled",
                "ts": _now_iso(),
                "url": page.url,
            })
            log.info("login form pre-filled on initial page")
        log.info("recording started — log in via VNC, click through the "
                 "LP portfolio / per-vehicle / activity pages we want to "
                 "scrape, then EITHER close the browser window OR Ctrl-C "
                 "the terminal to stop. Both paths flush artefacts: trace "
                 "chunks every %ds + the line-buffered clicks.jsonl / "
                 "network.jsonl land on disk continuously.",
                 args.chunk_interval)

        # Wait for the browser to close OR SIGTERM (done flag) OR
        # SIGINT (KeyboardInterrupt) OR max-duration timeout. Polled
        # in 1-second chunks so SIGTERM is responsive and
        # KeyboardInterrupt can propagate out of wait_for_event at the
        # next iteration boundary. Trace chunks are rotated inside the
        # same loop on a separate cadence.
        deadline = time.monotonic() + args.max_duration
        last_chunk_at = time.monotonic()
        exit_reason = "timeout"
        try:
            while time.monotonic() < deadline:
                if done.is_set():
                    exit_reason = "sigterm"
                    break
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

        # Finally: ALWAYS write the trace + the stop event, even on
        # signal / exception. For Ctrl-C / SIGTERM / timeout the
        # context is still alive so the final chunk + tracing.stop()
        # both succeed; for browser-closed the context is already
        # dead, but the periodic chunks captured incrementally above
        # are already on disk (worst-case loss bounded to
        # args.chunk_interval seconds). The Camoufox `with` block's
        # exit attempts the HAR write on context.close() — best-effort
        # for browser-closed and reliable otherwise.
        write_event({
            "kind": "stopped",
            "ts": _now_iso(),
            "reason": exit_reason,
        })
        final_ok = save_trace_chunk(label="final")
        log.info("stopping (reason: %s) — %d trace chunk(s) saved "
                 "(final chunk: %s)", exit_reason, chunk_seq["n"],
                 "ok" if final_ok else "browser dead, last periodic chunk is most-recent")
        with contextlib.suppress(Exception):
            context.tracing.stop(path=str(trace_path))

    log.info("artefacts written:")
    log.info("  clicks:        %s  (events + lifecycle)", clicks_path)
    log.info("  network:       %s  (requests + responses, crash-safe)",
             network_path)
    log.info("  trace-chunks/: %s/  (%d chunk(s); open with "
             "`playwright show-trace chunk-NNN.zip`)",
             trace_chunks_dir, chunk_seq["n"])
    log.info("  HAR:           %s  (best-effort; complete on Ctrl-C/SIGTERM exit, may be missing on browser-X close)",
             har_path)
    log.info("  trace.zip:     %s  (best-effort; final-flush attempt; "
             "trace-chunks/ is the durable record)", trace_path)
    log.info("  downloads:     %s/  (%d file(s))",
             downloads_dir, download_seq["n"])
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
