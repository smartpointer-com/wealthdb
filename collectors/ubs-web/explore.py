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
control, because it issues no interaction at all. What is off limits is off
limits to the *operator*, and CLAUDE.md §1 lists it.

Recorded under ``--debug-dir``:

  - **Network log** (``network.jsonl``) — crash-safe, line-flushed request +
    response records with headers, POST bodies and text-shaped response
    bodies. The primary artefact: UBS's SPA reads its own JSON, and this is
    where the endpoint shapes behind each screen are read off.
  - **HAR** (``network.har``) — the same traffic in a standard format, for
    a viewer. Playwright writes it itself and writes it whole, so it is
    rewritten with its credentials out once the close has flushed it; it
    only flushes on a clean context close, which makes ``network.jsonl``
    the durable record and this the convenience.
  - **Click log** (``clicks.jsonl``) — one record per click, plus navigation,
    download and lifecycle events. Captured through a
    ``document.addEventListener`` init script, because clicks made in the VNC
    session never pass through the Playwright API.
  - **DOM snapshots** (``dom/<NNN>/frame<K>.html`` + ``screen.png``) — every
    DISTINCT screen's full DOM across every UBS frame, plus a screenshot.
    Deduped by structure, so an idle session writes nothing and a screen is
    written once rather than every tick. This is what selectors are pinned
    from.
  - **Downloads** (``downloads/``) — every file fetched in-session (statement
    and invoice PDFs, CSV exports), materialised as the browser received it.
  - **Playwright trace** (``trace.zip`` + ``trace-chunks/``) — opt-in via
    ``--trace``; screenshots, DOM snapshots and network per action.
    UNREDACTED, and unredactable after the fact: the driver writes a zip
    of blobs whose DOM snapshots carry every input's value.

Recording stops when the browser window is closed, on Ctrl-C or SIGTERM, or
after ``--max-duration``. Every path flushes: the two JSONL logs are written
line by line as events arrive.

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
import base64
import contextlib
import json
import logging
import os
import re
import signal
import sys
import threading
import time
from datetime import datetime, timezone
from pathlib import Path
from urllib.parse import urlparse

from collectorkit import cli, debugcap, envfile, launch, session

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

DEFAULT_DEBUG_ROOT = Path("/debug")
DEFAULT_ENV_FILE = Path("/secrets/ubs-web.env")
# The bank-level env file ubs-web falls back to (README: shared with a
# future ubs-* sibling).
LEGACY_ENV_FILE = Path("/secrets/ubs.env")
CONTRACT_ENV = "UBS_CONTRACT_NUMBER"

# Resource types that never carry anything worth reading back, and would
# otherwise dominate the log.
SKIP_RESOURCE_TYPES = frozenset({
    "image", "font", "media", "stylesheet", "manifest",
})

# Response content types worth capturing a body for.
TEXT_CONTENT_HINTS = ("json", "html", "plain", "csv", "xml", "urlencoded")

# Body-capture ceiling. Generous on purpose: a card ledger's JSON runs to
# hundreds of kilobytes, and the first capture of one was truncated at
# 200 KB — which loses exactly the tail that says how far the history
# reaches. Anything past this is recorded by size alone.
MAX_BODY_BYTES = 4_000_000

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
    p.add_argument(
        "--debug-dir", type=Path, default=None,
        help=("Where the recording lands. Defaults to a UTC-timestamped "
              "subdir of /debug (the wrapper's debug mount). Never bronze — "
              "nothing here is a `load` input."),
    )
    p.add_argument(
        "--url", default=ubs.LOGIN_ENTRY_URL,
        help=("The one page the harness opens. Everything after it is "
              "navigated by hand. Default: %(default)s."),
    )
    p.add_argument(
        "--max-duration", type=int, default=3600,
        help=("Safety net: stop recording after N seconds even if the "
              "browser is left open. Default: %(default)s (1 hour)."),
    )
    p.add_argument(
        "--dom-interval", type=int, default=3,
        help=("Seconds between DOM snapshots. Only structurally new screens "
              "are written, so an idle session costs nothing. 0 disables. "
              "Default: %(default)s."),
    )
    p.add_argument(
        "--env-file", type=Path, default=None,
        help=("Bash-sourced env file supplying %s for the login-form "
              "pre-fill. Defaults to the ubs-web env file under /secrets, "
              "falling back to the bank-level one. Skipped silently when "
              "absent." % CONTRACT_ENV),
    )
    p.add_argument(
        "--no-prefill", action="store_true",
        help=("Do not pre-fill the contract number; type it by hand in the "
              "VNC session. The harness never submits the form either way."),
    )
    p.add_argument(
        "--no-save-state", action="store_true",
        help=("Do not write the session back to --state-path on exit. The "
              "default saves it, so a sign-in done here is reusable by "
              "`download` instead of costing a second challenge."),
    )
    p.add_argument(
        "--trace", action="store_true",
        help=("Record a Playwright trace (per-action screenshots + DOM), "
              "chunked so an abrupt close loses at most one chunk. Off by "
              "default — it is heavy, nothing redacts it (its DOM "
              "snapshots carry every input's value, and its screenshots "
              "carry the screen), and the redacted DOM snapshots plus the "
              "network log already carry what selectors are pinned from."),
    )
    p.add_argument(
        "--chunk-interval", type=int, default=30,
        help=("Seconds between incremental trace-chunk saves, with --trace. "
              "Default: %(default)s."),
    )
    cli.add_common_args(p)
    return p.parse_args(argv)


# ============================================================
# Helpers
# ============================================================

def _now_iso() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="seconds")


def dom_skeleton(html: str) -> str:
    """A structure-only fingerprint of a DOM: tag names and element ids,
    without text or attribute values.

    Two renders of one screen hash equal while balances tick and tokens
    rotate, so a screen is snapshotted once instead of every interval.
    """
    return "".join(re.findall(r'<[a-z][a-z0-9-]*|id="[^"]+"', html, re.I))


def is_ubs_host(url: str) -> bool:
    """Whether `url` is served by UBS — the frames worth snapshotting."""
    try:
        host = urlparse(url).hostname or ""
    except ValueError:
        return False
    return bool(UBS_HOST_RE.search(host))


def safe_download_name(suggested: str | None, seq: int) -> str:
    """A filesystem-safe, collision-free name for a downloaded file.

    UBS reuses suggested names across accounts and periods, and the name
    is source-controlled input, so the sequence number carries uniqueness
    and the path separators come out.
    """
    name = (suggested or "").strip() or f"download-{seq}"
    # An allow-list, not a blocked-character list: the name is whatever the
    # source suggested, and enumerating the dangerous characters is the
    # spelling that keeps missing one. Dot runs collapse to a single dot so
    # no path segment can read as a traversal.
    name = re.sub(r"[^A-Za-z0-9._-]", "_", name)
    name = re.sub(r"\.{2,}", ".", name).strip("._-") or f"download-{seq}"
    return f"{seq:02d}-{name[:120]}"


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


def capture_dom_snapshot(context, dom_dir: Path, seq: int,
                         last_skeleton: str, redact) -> tuple[int, str]:
    """Write every UBS frame's DOM plus a screenshot, if the composite
    structure changed. Returns the new (seq, skeleton).

    A DOM capture holds whatever was on screen, which is the point of it —
    but the contract number is the login identity, and it rides in the
    post-auth URL as well as in the form. `redact` is applied to the
    markup and to the recorded URLs so the one value masked everywhere
    else does not survive here; the screenshot is left alone, since a
    rendered page cannot be edited without destroying what it is for.
    """
    frames = []
    for page in list(context.pages):
        for frame in page.frames:
            if not is_ubs_host(frame.url):
                continue
            try:
                # scrub_dom before `redact`, because they catch
                # different things. `redact` masks the one credential this
                # harness knows, the contract number. scrub_dom is the
                # shared guard that blanks `value=` on every type=password
                # input whatever was typed into it — UBS's own login has
                # none (contract number + Access App QR, see landmarks
                # TEMPLATE_CONTRACT_NR / TEMPLATE_QR), so here it is
                # defence in depth for a password-type field the session
                # meets by hand on a screen the harness never scripts.
                frames.append(debugcap.scrub_dom(frame.content(), redact))
            except Exception:  # noqa: BLE001 — a frame mid-navigation
                continue
    if not frames:
        return seq, last_skeleton
    skeleton = dom_skeleton("".join(frames))
    if skeleton == last_skeleton:
        return seq, last_skeleton
    seq += 1
    snap = dom_dir / f"{seq:03d}"
    snap.mkdir(parents=True, exist_ok=True)
    for i, content in enumerate(frames):
        with contextlib.suppress(Exception):
            (snap / f"frame{i}.html").write_text(content, encoding="utf-8")
    with contextlib.suppress(Exception):
        (snap / "url.txt").write_text(
            "\n".join(redact(p.url) for p in context.pages), encoding="utf-8")
    with contextlib.suppress(Exception):
        context.pages[0].screenshot(path=str(snap / "screen.png"),
                                    full_page=True)
    log.info("dom snapshot %03d (%d UBS frame(s))", seq, len(frames))
    return seq, skeleton


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

def main(argv: list[str]) -> int:
    args = parse_args(argv)
    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format=cli.LOG_FORMAT,
    )

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
    clicks_path = debug_dir / "clicks.jsonl"
    network_path = debug_dir / "network.jsonl"
    har_path = debug_dir / "network.har"

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

    log.info("debug dir:   %s", debug_dir)
    log.info("state file:  %s (%s)", state_path,
             "loaded" if have_state else "absent — sign in by hand")
    log.info("initial URL: %s", args.url)

    # The contract number is the one credential in this flow and it travels
    # in the login POST body. Debug artefacts are not under ~/.secrets, so a
    # log that quoted it would be a real leak — and it reaches that body
    # percent-encoded, which a literal-substring masker writes out in full.
    redact = debugcap.secret_redactor(contract)

    from playwright.sync_api import sync_playwright
    from playwright.sync_api import TimeoutError as PlaywrightTimeoutError

    download_seq = {"n": 0}

    with contextlib.ExitStack() as stack:
        # The HAR is Playwright's own recording, and it records whole: the
        # login POST body, every header, the cookie jar. It is the one
        # capture in this dir written by the driver rather than by the
        # handlers, and it exists only once the context close below has
        # flushed it. Registered on the stack rather than called after the
        # block so a walk that raises is cleaned too — an unwind closes the
        # context, which flushes the HAR, and a run that crashed is exactly
        # the one whose debug dir gets opened. Registered first, so LIFO
        # runs it last, after everything that could still write to the file.
        stack.callback(
            lambda: debugcap.redact_har(har_path, redact, log=log))

        clicks_fp = stack.enter_context(
            open(clicks_path, "w", encoding="utf-8"))
        network_fp = stack.enter_context(
            open(network_path, "w", encoding="utf-8"))

        def write_event(payload: dict) -> None:
            # Every click log entry carries the URL it happened on, and on
            # this source the contract number rides in the post-auth URL as
            # well as in the form. Masking here rather than at each call
            # site makes it structural: the in-page recorder's own records
            # (location.href, from an init script this code does not see
            # the fields of) go through the same choke point as the ones
            # built below.
            if isinstance(payload.get("url"), str):
                payload["url"] = redact(payload["url"])
            clicks_fp.write(json.dumps(payload, default=str) + "\n")
            clicks_fp.flush()

        def write_network(payload: dict) -> None:
            network_fp.write(json.dumps(payload, default=str) + "\n")
            network_fp.flush()

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
            record_har_path=str(har_path),
        )
        if args.trace:
            context.tracing.start(screenshots=True, snapshots=True,
                                  sources=True)
            context.tracing.start_chunk()
        context.add_init_script(CLICK_RECORDER_JS)

        def on_request(request) -> None:
            try:
                if request.resource_type in SKIP_RESOURCE_TYPES:
                    return
                write_network({
                    "kind": "request",
                    "ts": _now_iso(),
                    "method": request.method,
                    "url": debugcap.redact_url(redact(request.url)),
                    "resource_type": request.resource_type,
                    # Two redactions, because they catch different things:
                    # `redact` masks values known in advance (the contract
                    # number), while `redact_headers` masks by header NAME —
                    # which is the only way to catch a credential the site
                    # issues at runtime, like the SPA's own api key.
                    "headers": debugcap.redact_headers(
                        {k: redact(v) for k, v in request.headers.items()}),
                    "post_data": (redact(request.post_data)
                                  if request.method != "GET" else None),
                })
            except Exception as exc:  # noqa: BLE001
                log.debug("on_request error: %r", exc)

        def on_response(response) -> None:
            try:
                if response.request.resource_type in SKIP_RESOURCE_TYPES:
                    return
                ctype = response.headers.get("content-type", "").lower()
                payload = {
                    "kind": "response",
                    "ts": _now_iso(),
                    "url": debugcap.redact_url(redact(response.url)),
                    "method": response.request.method,
                    "status": response.status,
                    "resource_type": response.request.resource_type,
                    "headers": debugcap.redact_headers(
                        {k: redact(v) for k, v in response.headers.items()}),
                }
                if any(t in ctype for t in TEXT_CONTENT_HINTS):
                    try:
                        body = response.body()
                    except Exception as exc:  # noqa: BLE001
                        payload["body_error"] = repr(exc)
                    else:
                        payload["body_size"] = len(body)
                        if len(body) <= MAX_BODY_BYTES:
                            try:
                                payload["body_text"] = redact(
                                    body.decode("utf-8"))
                            except UnicodeDecodeError:
                                payload["body_b64"] = base64.b64encode(
                                    body).decode()
                        else:
                            payload["body_truncated"] = True
                write_network(payload)
            except Exception as exc:  # noqa: BLE001
                log.debug("on_response error: %r", exc)

        context.on("request", on_request)
        context.on("response", on_response)

        chunk_seq = {"n": 0}

        def save_trace_chunk(label: str = "periodic") -> bool:
            if not args.trace:
                return False
            chunk_seq["n"] += 1
            path = trace_chunks_dir / f"chunk-{chunk_seq['n']:03d}-{label}.zip"
            try:
                context.tracing.stop_chunk(path=str(path))
                context.tracing.start_chunk()
                return True
            except Exception as exc:  # noqa: BLE001
                log.debug("trace chunk save failed: %r", exc)
                return False

        prefilled: set = set()

        def on_page(page) -> None:
            def on_console(msg) -> None:
                text = msg.text
                if not text.startswith(EVENT_PREFIX):
                    return
                try:
                    payload = json.loads(text[len(EVENT_PREFIX):])
                except ValueError:
                    return
                write_event(payload)
                if prefill and payload.get("kind") == "login-form-detected":
                    if prefill_contract(page, contract, prefilled):
                        write_event({"kind": "contract-prefilled",
                                     "ts": _now_iso(), "url": page.url})
                        log.info("contract number pre-filled — "
                                 "submit and scan the QR in the VNC session")

            def on_download(download) -> None:
                # Materialise the bytes now: Playwright discards them when
                # the Download object is collected. A statement PDF fetched
                # by hand is one of the artefacts most worth having.
                download_seq["n"] += 1
                out = downloads_dir / safe_download_name(
                    download.suggested_filename, download_seq["n"])
                event = {
                    "kind": "download",
                    "ts": _now_iso(),
                    "url": download.url,
                    "suggested_filename": download.suggested_filename,
                    "saved_to": str(out),
                }
                try:
                    download.save_as(str(out))
                except Exception as exc:  # noqa: BLE001
                    event["save_error"] = repr(exc)
                write_event(event)
                log.info("download saved: %s", out.name)

            page.on("console", on_console)
            page.on("download", on_download)
            page.on("framenavigated",
                    lambda f: f == page.main_frame and write_event({
                        "kind": "navigation",
                        "ts": _now_iso(),
                        "url": f.url,
                    }))

        context.on("page", on_page)

        done = threading.Event()

        def _on_sigterm(signum, frame):  # noqa: ARG001
            write_event({"kind": "signal", "ts": _now_iso(),
                         "signal": "SIGTERM"})
            done.set()

        signal.signal(signal.SIGTERM, _on_sigterm)

        page = context.new_page()
        on_page(page)              # `page` predates the context listener
        page.goto(args.url, wait_until="domcontentloaded")
        write_event({"kind": "started", "ts": _now_iso(),
                     "url": args.url, "had_state": have_state})

        log.info(
            "recording — drive the session in the VNC window. The card "
            "surface is what this session is for: the card roster, a "
            "card's transactions, one transaction's DETAIL view, and the "
            "invoice/statement archive including one statement download. "
            "Read-only surfaces only (CLAUDE.md §1) — never a payment form "
            "and never a card-management control. Press Ctrl-C to stop — "
            "that keeps the session readable, so a sign-in made here is "
            "saved and `download` need not challenge again; closing the "
            "browser window also stops cleanly, but the session is gone by "
            "then.")

        deadline = time.monotonic() + args.max_duration
        last_chunk_at = last_dom_at = last_prefill_at = 0.0
        dom_seq, dom_skel = 0, ""
        reason = "timeout"
        try:
            while time.monotonic() < deadline:
                if done.is_set():
                    reason = "sigterm"
                    break
                now = time.monotonic()
                if args.dom_interval > 0 and (
                        now - last_dom_at >= args.dom_interval):
                    last_dom_at = now
                    try:
                        dom_seq, dom_skel = capture_dom_snapshot(
                            context, dom_dir, dom_seq, dom_skel, redact)
                    except Exception as exc:  # noqa: BLE001
                        log.debug("dom snapshot error: %r", exc)
                # The console detector fires on mount; this poll covers a
                # form that re-renders on an SPA route change. A no-op once
                # the field is marked done.
                if prefill and now - last_prefill_at >= 1.5:
                    last_prefill_at = now
                    for pg in list(context.pages):
                        with contextlib.suppress(Exception):
                            if prefill_contract(pg, contract, prefilled):
                                write_event({"kind": "contract-prefilled",
                                             "ts": _now_iso(), "url": pg.url})
                try:
                    context.wait_for_event("close", timeout=1000)
                    reason = "browser-closed"
                    break
                except PlaywrightTimeoutError:
                    if time.monotonic() - last_chunk_at >= args.chunk_interval:
                        save_trace_chunk()
                        last_chunk_at = time.monotonic()
        except KeyboardInterrupt:
            write_event({"kind": "signal", "ts": _now_iso(),
                         "signal": "SIGINT"})
            reason = "sigint"

        # A last snapshot so the closing screen is always on disk — skipped
        # when the browser is already gone and there is nothing to read.
        if args.dom_interval > 0 and reason != "browser-closed":
            with contextlib.suppress(Exception):
                dom_seq, dom_skel = capture_dom_snapshot(
                    context, dom_dir, dom_seq, dom_skel, redact)

        write_event({"kind": "stopped", "ts": _now_iso(), "reason": reason})
        if args.trace:
            save_trace_chunk(label="final")
            with contextlib.suppress(Exception):
                context.tracing.stop(path=str(debug_dir / "trace.zip"))

        # Save the session back before the context closes. A sign-in done
        # here is then reusable by `download`, which is the difference
        # between one challenge and two.
        if not args.no_save_state and _looks_authenticated(context):
            try:
                context.storage_state(path=str(write_state_path))
                session.secure_file(write_state_path)
                log.info("session state saved to %s", write_state_path)
            except Exception as exc:  # noqa: BLE001
                log.warning("could not save session state: %s", exc)
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

    log.info("stopped (%s). artefacts:", reason)
    log.info("  network:    %s", network_path)
    log.info("  clicks:     %s", clicks_path)
    log.info("  HAR:        %s (redacted)", har_path)
    log.info("  downloads:  %s/ (%d file(s))", downloads_dir,
             download_seq["n"])
    if args.dom_interval > 0:
        log.info("  dom/:       %s/ (%d distinct screen(s))",
                 dom_dir, dom_seq)
    if args.trace:
        log.info("  trace:      %s/ (%d chunk(s)) — UNREDACTED, never "
                 "commit it", trace_chunks_dir, chunk_seq["n"])
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
