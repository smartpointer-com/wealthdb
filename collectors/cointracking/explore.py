#!/usr/bin/env python3
"""cointracking discovery harness.

Launches Camoufox in the container's Xvfb display, navigates to
cointracking.info, and records everything the operator does via VNC
so login.py + download.py can be written from real traces:

  - **HAR** (`network.har`)        — every request + response with
                                     headers and bodies. The primary
                                     artefact for finding the internal
                                     REST endpoints the SPA hits.
  - **Playwright trace**
    (`trace.zip`)                  — screenshots + DOM snapshots +
                                     network events at every action.
                                     Open with `playwright show-trace`.
  - **Click log**
    (`clicks.jsonl`)               — one JSON object per click the
                                     operator makes (timestamp, URL,
                                     tag, id, text, xpath). Captured
                                     via a `document.addEventListener
                                     ('click', …)` init script
                                     because user-driven VNC clicks
                                     bypass the Playwright API. Plus
                                     a `MutationObserver` watch that
                                     signals when a login form has
                                     mounted — the Python side then
                                     pre-fills the form from
                                     `COINTRACKING_USERNAME` /
                                     `COINTRACKING_PASSWORD` (sourced
                                     from `/secrets/cointracking.env`
                                     inside the container). Operator
                                     still clicks Login + handles 2FA.

Recording stops when the operator closes the last browser window
(Camoufox's persistent context fires `close`) or after
`--max-duration` (default 1h) as a safety net. Artefacts land under
`/debug/<UTC-ts>/` so the bronze + silver tree under `/data` stays
clean.

Once enough traces exist to author login.py + download.py from, the
explore command and the VNC entrypoint case can be dropped.
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

log = logging.getLogger("cointracking.explore")

DEFAULT_URL = "https://cointracking.info"
DEFAULT_PROFILE_DIR = Path("/secrets/cointracking-profile")
DEFAULT_DEBUG_ROOT = Path("/debug")
DEFAULT_ENV_FILE = Path("/secrets/cointracking.env")
USER_ENV = "COINTRACKING_USERNAME"
PASS_ENV = "COINTRACKING_PASSWORD"
EVENT_PREFIX = "__CT_EVENT__ "

# Pre-fill is restricted to this host (and subdomains) so credentials
# never leak into an embedded third-party iframe.
HOST_RE = re.compile(r"(^|\.)cointracking\.info$", re.I)

# Locators tried in order. The explore phase will tell us which one
# actually matches cointracking.info's form — we keep a few common
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
              "$HOME/.cache/cointracking-debug on the host)."),
    )
    p.add_argument(
        "--url", default=DEFAULT_URL,
        help="Initial URL to open. Default: %(default)s.",
    )
    p.add_argument(
        "--max-duration", type=int, default=3600,
        help=("Safety net: auto-close the recording after N seconds "
              "even if the operator leaves the browser open. "
              "Default: %(default)s (1 hour)."),
    )
    p.add_argument(
        "--env-file", type=Path, default=DEFAULT_ENV_FILE,
        help=("Path to a bash-sourced env file with "
              "COINTRACKING_USERNAME / COINTRACKING_PASSWORD. "
              "Skipped silently if absent. Default: %(default)s."),
    )
    p.add_argument(
        "--no-prefill", action="store_true",
        help=("Skip pre-filling the cointracking.info login form. "
              "Use when you want to verify the form selectors by "
              "typing the credentials yourself, or when "
              "$COINTRACKING_USERNAME / $COINTRACKING_PASSWORD are "
              "intentionally unset."),
    )
    p.add_argument(
        "--fresh", action="store_true",
        help=("Wipe the persistent Camoufox profile dir before launching. "
              "Use this to force a fresh login: clears cookies including "
              "the long-lived 'remember device' cookie that lets "
              "cointracking skip 2FA. The next session will require "
              "typing the 2FA code."),
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
    """If the current page is on cointracking.info AND a login form
    is present + visible AND the operator hasn't typed there yet,
    fill the credentials. Returns True iff the fill actually
    happened. Never submits — the operator clicks Login + handles
    2FA in VNC."""
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
    # mean a host-set value wins, so the operator can override via
    # an explicit `export COINTRACKING_PASSWORD=…` before invocation.
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
        log.info("pre-fill ready (origin-gated to cointracking.info)")

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
        # for the whole context's lifetime.
        context = stack.enter_context(Camoufox(
            persistent_context=True,
            user_data_dir=str(args.profile_dir),
            os="macos",
            window=(1280, 800),
            headless=False,
            humanize=True,
            geoip=True,
            record_har_path=str(har_path),
        ))
        context.tracing.start(
            screenshots=True, snapshots=True, sources=True,
        )
        # Use chunks so we can flush partial traces every N seconds
        # during the polling loop. Worst-case data loss on browser-X
        # close is bounded to args.chunk_interval seconds.
        context.tracing.start_chunk()
        context.add_init_script(CLICK_RECORDER_JS)

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
                # a login form mounts on cointracking.info. Bounce
                # the fill back through Playwright so credentials
                # never reach the page's JS context as plain strings.
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
                # are discarded when the Download object is GC'd,
                # which was the bug behind the previous run's missing
                # CSV files. Sequence prefix prevents collisions when
                # cointracking reuses suggested_filename across
                # portfolios.
                download_seq["n"] += 1
                seq = download_seq["n"]
                suggested = download.suggested_filename or f"download-{seq}"
                # Strip path separators from suggested name as a
                # defence-in-depth measure (cointracking-generated,
                # but still untrusted input).
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

        # SIGTERM (docker stop) sets a flag the polling loop picks
        # up. SIGINT (Ctrl-C) is left on Python's default handler so
        # it raises KeyboardInterrupt, which unwinds the with-block
        # cleanly — tracing.stop() runs in the finally and Camoufox's
        # context-manager exit flushes the HAR. The previous code
        # replaced SIGINT with a flag-only handler, which left the
        # main thread blocked in wait_for_event until max-duration.
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
        # Belt-and-braces: try an immediate pre-fill in case the form
        # is already in the initial DOM (the init-script's
        # detect() runs on script attach, but on some pages the
        # framework's first render hasn't fired yet when the script
        # executes).
        if prefill_enabled and _maybe_prefill_login(page, username, password):
            write_event({
                "kind": "credentials-prefilled",
                "ts": _now_iso(),
                "url": page.url,
            })
            log.info("login form pre-filled on initial page")
        log.info("recording started — log in via VNC, click through "
                 "the pages we want to scrape, then EITHER close the "
                 "browser window OR Ctrl-C the terminal to stop. Both "
                 "paths flush artefacts: trace chunks every %ds + the "
                 "line-buffered clicks.jsonl / network.jsonl land on "
                 "disk continuously.", args.chunk_interval)

        # Wait for the browser to close OR SIGTERM (done flag) OR
        # SIGINT (KeyboardInterrupt) OR max-duration timeout.
        # Polled in 1-second chunks so SIGTERM is responsive and
        # KeyboardInterrupt can propagate out of wait_for_event
        # at the next iteration boundary even if the underlying
        # sync_api call is mid-greenlet-switch. Trace chunks are
        # rotated inside the same loop on a separate cadence.
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
        # exit attempts the HAR write on context.close() — that's
        # best-effort for browser-closed and reliable otherwise.
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
