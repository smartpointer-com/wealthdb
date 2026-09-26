"""The discovery harness behind every collector's `explore` verb.

An explore session opens a headed browser in the container's display, lets
a person drive the source's UI over VNC, and records what happens, so a
collector's login and download can be written from real traces. The
artefacts land in one debug dir, outside bronze:

  - ``network.har``     Playwright's own recording, scrubbed of credentials
                        once the context close has flushed it.
  - ``network.jsonl``   crash-safe, line-flushed requests and responses,
                        with text bodies up to ``MAX_BODY_BYTES``.
  - ``clicks.jsonl``    the in-page detector script's events (clicks, a
                        login form or one-time-code field mounting),
                        navigations, downloads and the session's lifecycle.
  - ``downloads/``      every file fetched in-session.
  - ``dom/NNN/``        every distinct screen's DOM, per frame the site
                        serves, plus a screenshot (``--dom-interval``).
  - ``trace-chunks/``   an opt-in Playwright trace (``--trace``). A trace is
                        UNREDACTED and unredactable: its DOM snapshots carry
                        every input's value (see :mod:`debugcap`).

Everything is masked on the way to disk: known credentials by value, and
headers, query parameters and body fields by name, which is the only way
to catch a secret the site issues at runtime or one typed by hand.

What stays in each collector's own ``explore.py`` is what is particular to
its source: the entry URL, the host gate, the credential env names, the
detector script, and the login-form fill, which its ``login.py`` often
reuses. :class:`Session` composes the rest.

Playwright and Camoufox are imported where they are used, so the library
stays importable without them.
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
import threading
import time
from collections.abc import Callable
from datetime import datetime, timezone
from pathlib import Path
from urllib.parse import urlparse

from collectorkit import cli, debugcap, envfile, launch

DEFAULT_DEBUG_ROOT = Path("/debug")

# Resource types that never carry anything worth reading back, and would
# otherwise dominate the network log.
SKIP_RESOURCE_TYPES = frozenset({
    "image", "font", "media", "stylesheet", "manifest",
})

# Response content types worth capturing a body for. "ofx" / "qfx" catch a
# transaction export that comes back as an XHR body rather than a download.
TEXT_CONTENT_HINTS = (
    "json", "html", "plain", "csv", "xml", "urlencoded", "ofx", "qfx",
)

# Body-capture ceiling. A card ledger's JSON runs to hundreds of kilobytes,
# and a truncated one loses exactly the tail that says how far the history
# reaches. Anything past this is recorded by size alone.
MAX_BODY_BYTES = 4_000_000

# How often the wait loop re-offers the login-form fill. The in-page
# detector fires when a form mounts; the poll covers SPA route changes and
# late-mounting frames.
PREFILL_POLL_SECONDS = 1.5


def now_iso() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="seconds")


def dom_skeleton(html: str) -> str:
    """A structure-only fingerprint of a DOM: tag names and element ids,
    without text or attribute values. Two renders of one screen hash equal
    while balances tick and tokens rotate, so a screen is snapshotted once
    instead of every interval."""
    return "".join(re.findall(r'<[a-z][a-z0-9-]*|id="[^"]+"', html, re.I))


def safe_download_name(suggested: str | None, seq: int) -> str:
    """A filesystem-safe, collision-free name for a downloaded file.

    Sites reuse suggested names across accounts and periods, and the name
    is source-controlled input, so the sequence number carries uniqueness
    and an allow-list decides the characters: enumerating the dangerous
    ones is the spelling that keeps missing one. Dot runs collapse so no
    path segment can read as a traversal.
    """
    name = (suggested or "").strip() or f"download-{seq}"
    name = re.sub(r"[^A-Za-z0-9._-]", "_", name)
    name = re.sub(r"\.{2,}", ".", name).strip("._-") or f"download-{seq}"
    return f"{seq:02d}-{name[:120]}"


def add_args(p: argparse.ArgumentParser, *, url: str,
             env_file: Path | None, profile_dir: Path | None = None,
             dom_snapshots: bool = False, env_help: str | None = None) -> None:
    """The flags every explore harness shares.

    `profile_dir` adds ``--profile-dir`` and ``--fresh`` for a persistent
    Camoufox profile; `dom_snapshots` adds ``--dom-interval``; `env_help`
    replaces ``--env-file``'s help where the default is resolved at run
    time. Site-specific flags are added by the caller.
    """
    if profile_dir is not None:
        p.add_argument(
            "--profile-dir", type=Path, default=profile_dir,
            help=("Persistent Camoufox profile dir. The login cookie and "
                  "any device-trust state live here, so whether trust "
                  "persists between runs is one of the things explore "
                  "measures. Default: %(default)s."),
        )
    p.add_argument(
        "--debug-dir", type=Path, default=None,
        help=("Where the recording lands. Defaults to a UTC-timestamped "
              "subdir of /debug (the wrapper's debug mount). Never bronze "
              "— nothing here is a `load` input."),
    )
    p.add_argument(
        "--url", default=url,
        help="Initial URL to open. Default: %(default)s.",
    )
    p.add_argument(
        "--max-duration", type=int, default=3600,
        help=("Safety net: stop recording after N seconds even if the "
              "browser is left open. Default: %(default)s (1 hour)."),
    )
    p.add_argument(
        "--env-file", type=Path, default=env_file,
        help=env_help or ("Bash-sourced env file supplying the login-form "
                          "pre-fill credentials. Skipped silently when "
                          "absent. Default: %(default)s."),
    )
    if dom_snapshots:
        p.add_argument(
            "--dom-interval", type=int, default=3,
            help=("Seconds between DOM snapshots. Every distinct screen's "
                  "DOM (each of the site's frames, a cross-origin login or "
                  "2FA iframe included) and a screenshot land under "
                  "dom/<NNN>/ — the record the click log misses when a "
                  "control's clicks never reach the top document. Only "
                  "structurally new screens are written. 0 disables. "
                  "Default: %(default)s."),
        )
    p.add_argument(
        "--no-prefill", action="store_true",
        help=("Do not pre-fill the login form; type the credentials by "
              "hand in the VNC session. The harness never submits it "
              "either way."),
    )
    if profile_dir is not None:
        p.add_argument(
            "--fresh", action="store_true",
            help=("Wipe the persistent profile dir before launching, "
                  "discarding every cookie including a device-trust one, "
                  "so the session meets the full sign-in challenge — "
                  "exactly the flow worth capturing."),
        )
    p.add_argument(
        "--trace", action="store_true",
        help=("Record a Playwright trace (per-action screenshots + DOM), "
              "chunked so an abrupt close loses at most one chunk. Off by "
              "default: the pinned Playwright/Camoufox pair crashes on "
              "tracing, and nothing redacts a trace — its DOM snapshots "
              "carry every input's value."),
    )
    p.add_argument(
        "--chunk-interval", type=int, default=30,
        help=("Seconds between incremental trace-chunk saves, with "
              "--trace. Default: %(default)s."),
    )
    cli.add_common_args(p)


def prepare_profile(profile_dir: Path, *, fresh: bool, note: str,
                    log: logging.Logger) -> None:
    """Wipe the profile when `fresh`, then ready it for Camoufox. `note`
    says what the wipe costs the next sign-in."""
    if fresh and profile_dir.exists():
        log.warning("--fresh: wiping profile dir %s (%s)", profile_dir, note)
        shutil.rmtree(profile_dir)
    launch.prepare_profile_dir(profile_dir)
    log.info("profile dir: %s", profile_dir)


def load_credentials(env_file: Path | None, user_envs: tuple[str, ...],
                     pass_envs: tuple[str, ...], *, no_prefill: bool,
                     host: str, log: logging.Logger) -> tuple[str, str, bool]:
    """Source the env file and read the login credentials.

    Each is the first non-empty of its env names; a value already in the
    environment wins over the file's. Returns (username, password,
    prefill), where `prefill` says whether both are there to fill and
    ``--no-prefill`` did not refuse them.
    """
    if env_file is not None and envfile.source_env_file(env_file):
        log.info("env file:    %s (sourced)", env_file)
    username = _first_set(user_envs)
    password = _first_set(pass_envs)
    if no_prefill:
        log.info("--no-prefill: login form pre-fill disabled")
    elif not (username and password):
        log.warning("%s / %s not set; login-form pre-fill disabled. Drop "
                    "credentials into %s to enable.", "/".join(user_envs),
                    "/".join(pass_envs), env_file)
    else:
        log.info("pre-fill ready (origin-gated to %s)", host)
    return username, password, bool(username and password) and not no_prefill


def _first_set(names: tuple[str, ...]) -> str:
    return next((os.environ[k] for k in names if os.environ.get(k)), "")


class Session:
    """The capture side of one explore run.

    Construction lays out the debug dir, opens the two JSONL logs on
    `stack` and queues the HAR scrub there, so every unwind — a crash
    included — leaves the HAR scrubbed: the scrub is queued before
    anything that can close the context, and a stack unwinds LIFO, so it
    runs after the close that flushes the HAR.
    """

    def __init__(self, stack: contextlib.ExitStack, root: Path, *,
                 redact: Callable[[str], str], log: logging.Logger,
                 trace: bool = False, chunk_interval: int = 30,
                 dom_interval: int = 0, observe: re.Pattern | None = None,
                 label: str = "site"):
        self.root = root
        self.downloads_dir = root / "downloads"
        self.dom_dir = root / "dom"
        self.trace_chunks_dir = root / "trace-chunks"
        self.har_path = root / "network.har"
        self.trace_path = root / "trace.zip"
        self.clicks_path = root / "clicks.jsonl"
        self.network_path = root / "network.jsonl"
        self.downloads_dir.mkdir(parents=True, exist_ok=True)
        # Snapshots need a host gate to know which frames are the site's.
        self.dom_interval = dom_interval if observe is not None else 0
        if self.dom_interval > 0:
            self.dom_dir.mkdir(parents=True, exist_ok=True)
        if trace:
            self.trace_chunks_dir.mkdir(parents=True, exist_ok=True)
        self.redact = redact
        self.log = log
        self.trace = trace
        self.chunk_interval = chunk_interval
        self.observe = observe
        self.label = label
        self.context = None
        self.downloads = 0
        self.chunks = 0
        self.screens = 0
        self._skeleton = ""
        self._prefill: Callable | None = None
        self._prefill_event = "credentials-prefilled"
        self._done = threading.Event()

        stack.callback(
            lambda: debugcap.redact_har(self.har_path, redact, log=log))
        self._clicks = stack.enter_context(
            open(self.clicks_path, "w", encoding="utf-8"))
        self._network = stack.enter_context(
            open(self.network_path, "w", encoding="utf-8"))
        log.info("debug dir:   %s", root)

    @classmethod
    def from_args(cls, stack: contextlib.ExitStack,
                  args: argparse.Namespace, **kwargs) -> Session:
        """A session laid out by the shared flags: ``--debug-dir`` (else a
        timestamped dir under /debug), ``--trace``, ``--chunk-interval``
        and, where the harness has it, ``--dom-interval``."""
        ts = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
        return cls(stack, args.debug_dir or (DEFAULT_DEBUG_ROOT / ts),
                   trace=args.trace, chunk_interval=args.chunk_interval,
                   dom_interval=getattr(args, "dom_interval", 0), **kwargs)

    # ---- the two logs ---------------------------------------------------

    def event(self, payload: dict) -> None:
        """Append one event to clicks.jsonl. The whole line goes through the
        redactor: the in-page script's events carry URLs and text this code
        never sees the fields of."""
        self._clicks.write(self.redact(json.dumps(payload, default=str)) + "\n")
        self._clicks.flush()

    def _network_event(self, payload: dict) -> None:
        self._network.write(json.dumps(payload, default=str) + "\n")
        self._network.flush()

    def _on_request(self, request) -> None:
        try:
            if request.resource_type in SKIP_RESOURCE_TYPES:
                return
            body = request.post_data if request.method != "GET" else None
            self._network_event({
                "kind": "request",
                "ts": now_iso(),
                "method": request.method,
                "url": debugcap.redact_url(self.redact(request.url)),
                "resource_type": request.resource_type,
                # Two redactions, because they catch different things:
                # `redact` masks the values known in advance (the
                # credentials), `redact_headers` masks by header NAME — the
                # only way to catch one the site issues at runtime.
                "headers": debugcap.redact_headers(
                    {k: self.redact(v) for k, v in request.headers.items()}),
                # By field name as well as by value: on a run where the
                # password was typed by hand, only its field names it.
                "post_data": debugcap.redact_body(
                    body, request.headers.get("content-type"), self.redact)
                if body else None,
            })
        except Exception as exc:
            self.log.debug("on_request error: %s", debugcap.safe_error(exc))

    def _on_response(self, response) -> None:
        try:
            if response.request.resource_type in SKIP_RESOURCE_TYPES:
                return
            ctype = response.headers.get("content-type", "").lower()
            payload = {
                "kind": "response",
                "ts": now_iso(),
                "url": debugcap.redact_url(self.redact(response.url)),
                "method": response.request.method,
                "status": response.status,
                "resource_type": response.request.resource_type,
                "headers": debugcap.redact_headers(
                    {k: self.redact(v) for k, v in response.headers.items()}),
            }
            # Text-shaped bodies only; a downloaded file lands under
            # downloads/ through the page's download hook anyway.
            if any(t in ctype for t in TEXT_CONTENT_HINTS):
                try:
                    body = response.body()
                except Exception as exc:
                    payload["body_error"] = debugcap.safe_error(exc)
                else:
                    payload["body_size"] = len(body)
                    if len(body) <= MAX_BODY_BYTES:
                        try:
                            payload["body_text"] = self.redact(
                                body.decode("utf-8"))
                        except UnicodeDecodeError:
                            payload["body_b64"] = base64.b64encode(
                                body).decode()
                    else:
                        payload["body_truncated"] = True
            self._network_event(payload)
        except Exception as exc:
            self.log.debug("on_response error: %s", debugcap.safe_error(exc))

    # ---- the browser ----------------------------------------------------

    def open_camoufox(self, stack: contextlib.ExitStack, profile_dir: Path,
                      **options):
        """Launch headed Camoufox on the persistent profile, recording the
        HAR, and return its context. `options` reach Camoufox as given.

        Entered by hand rather than through the stack so the close can
        swallow the error its exit raises when the browser window was
        closed first — browser.close() against a dead browser. The
        artefacts are flushed by then, so that error is noise, not a
        failed session.
        """
        from camoufox.sync_api import Camoufox

        # launch.firefox_prefs() disables Firefox's password manager so a
        # saved credential can never autofill on top of the pre-fill, and
        # keeps the profile down to session state.
        cam = Camoufox(
            persistent_context=True,
            user_data_dir=str(profile_dir),
            os="macos",
            window=(1280, 800),
            headless=False,
            humanize=True,
            geoip=True,
            record_har_path=str(self.har_path),
            firefox_user_prefs=launch.firefox_prefs(),
            **options,
        )
        context = cam.__enter__()

        def close() -> None:
            try:
                cam.__exit__(None, None, None)
            except Exception as exc:
                if "closed" not in repr(exc).lower():
                    raise
                self.log.info("browser already closed on exit (artefacts "
                              "flushed): %s", debugcap.safe_error(exc))
        stack.callback(close)
        return context

    def attach(self, context, *, init_js: str | None = None,
               event_prefix: str | None = None,
               prefill: Callable | None = None,
               prefill_event: str = "credentials-prefilled") -> None:
        """Start recording `context`: its traffic, and on every page its
        detector events, downloads and navigations.

        `init_js` is the collector's in-page detector, which reports
        through console messages tagged `event_prefix`. `prefill(page)`
        fills the login form and says whether it filled anything; it runs
        when the detector reports a form and on the wait loop's poll, and
        each fill is logged as a `prefill_event`.
        """
        self.context = context
        self._prefill = prefill
        self._prefill_event = prefill_event
        if self.trace:
            context.tracing.start(screenshots=True, snapshots=True,
                                  sources=True)
            # Chunked, so an abrupt close loses at most one interval.
            context.tracing.start_chunk()
        if init_js:
            context.add_init_script(init_js)
        context.on("request", self._on_request)
        context.on("response", self._on_response)
        context.on("page", lambda page: self._watch_page(page, event_prefix))

        # SIGTERM (docker stop) sets a flag the wait loop picks up. SIGINT
        # keeps Python's default and raises KeyboardInterrupt, which the
        # loop catches.
        def on_sigterm(signum, frame):  # noqa: ARG001
            self.event({"kind": "signal", "ts": now_iso(),
                        "signal": "SIGTERM"})
            self._done.set()
        signal.signal(signal.SIGTERM, on_sigterm)

    def _watch_page(self, page, event_prefix: str | None) -> None:
        def on_console(msg) -> None:
            text = msg.text
            if not text.startswith(event_prefix):
                return
            try:
                payload = json.loads(text[len(event_prefix):])
            except ValueError:
                return
            self.event(payload)
            # The fill goes through Playwright, so the credentials never
            # reach the page's own JS context as plain strings.
            if payload.get("kind") == "login-form-detected":
                self.try_prefill(page)

        if event_prefix:
            page.on("console", on_console)
        page.on("download", self._save_download)
        page.on("framenavigated",
                lambda f: f == page.main_frame and self.event({
                    "kind": "navigation", "ts": now_iso(), "url": f.url}))

    def _save_download(self, download) -> None:
        # Materialised now: Playwright discards the bytes when the Download
        # object is collected.
        self.downloads += 1
        out = self.downloads_dir / safe_download_name(
            download.suggested_filename, self.downloads)
        event = {
            "kind": "download",
            "ts": now_iso(),
            "url": download.url,
            "suggested_filename": download.suggested_filename,
            "saved_to": str(out),
        }
        try:
            download.save_as(str(out))
        except Exception as exc:
            event["save_error"] = debugcap.safe_error(exc)
        self.event(event)
        self.log.info("download saved: %s", out.name)

    def try_prefill(self, page) -> bool:
        """Offer the login-form fill to `page`; log it when it filled."""
        if self._prefill is None or not self._prefill(page):
            return False
        self.event({"kind": self._prefill_event, "ts": now_iso(),
                    "url": page.url})
        self.log.info("pre-filled login field(s) on %s", page.url[:80])
        return True

    def started(self, page, url: str, **extra) -> None:
        """Record the session's start on its first page, and offer the fill
        at once: the form may already be in the initial DOM before the
        detector's first run."""
        self.event({"kind": "started", "ts": now_iso(), "url": url, **extra})
        self.try_prefill(page)

    def open(self, url: str):
        """Open the first page on `url` and record the start."""
        self.log.info("initial URL: %s", url)
        page = self.context.new_page()
        page.goto(url, wait_until="domcontentloaded")
        self.started(page, url)
        return page

    # ---- DOM snapshots and trace chunks --------------------------------

    def snapshot_dom(self) -> None:
        """Write every observed frame's DOM, plus the page URLs and a
        screenshot, when the composite structure changed since the last
        snapshot. This is the record the click log cannot produce: a login
        form or 2FA challenge in a cross-origin iframe never reaches the
        top document's click listener.

        Each frame's markup goes through ``debugcap.scrub_dom``, which
        blanks every password input's value and masks the known
        credentials: a serialized form carries what was typed into it."""
        frames = []
        for page in list(self.context.pages):
            for frame in page.frames:
                try:
                    host = urlparse(frame.url).hostname or ""
                except ValueError:
                    continue
                if not self.observe.search(host):
                    continue
                try:
                    frames.append(debugcap.scrub_dom(frame.content(),
                                                     self.redact))
                except Exception:  # noqa: BLE001 — a frame mid-navigation
                    continue
        if not frames:
            return
        skeleton = dom_skeleton("".join(frames))
        if skeleton == self._skeleton:
            return
        self._skeleton = skeleton
        self.screens += 1
        snap = self.dom_dir / f"{self.screens:03d}"
        snap.mkdir(parents=True, exist_ok=True)
        for i, content in enumerate(frames):
            with contextlib.suppress(Exception):
                (snap / f"frame{i}.html").write_text(content, encoding="utf-8")
        with contextlib.suppress(Exception):
            (snap / "url.txt").write_text(
                "\n".join(self.redact(p.url) for p in self.context.pages),
                encoding="utf-8")
        with contextlib.suppress(Exception):
            self.context.pages[0].screenshot(path=str(snap / "screen.png"),
                                             full_page=True)
        self.log.info("dom snapshot %03d (%d %s frame(s))", self.screens,
                      len(frames), self.label)

    def _save_trace_chunk(self, label: str = "periodic") -> bool:
        if not self.trace:
            return False
        self.chunks += 1
        path = self.trace_chunks_dir / f"chunk-{self.chunks:03d}-{label}.zip"
        try:
            self.context.tracing.stop_chunk(path=str(path))
            self.context.tracing.start_chunk()
            return True
        except Exception as exc:
            self.log.debug("trace chunk save failed: %s",
                           debugcap.safe_error(exc))
            return False

    # ---- the session's life ---------------------------------------------

    def record(self, walk: str, *, max_duration: int,
               poll_prefill: bool = True) -> str:
        """Record until the browser closes, SIGTERM, Ctrl-C or
        `max_duration` seconds, then write the stop. `walk` tells the
        person at the VNC session what to do. Returns the stop reason.

        The loop polls in one-second steps so SIGTERM is prompt, taking DOM
        snapshots, trace chunks and (with `poll_prefill`) the login-form
        fill on their own cadences.
        """
        from playwright.sync_api import TimeoutError as PlaywrightTimeoutError

        self.log.info("recording started — %s Then EITHER close the browser "
                      "window OR Ctrl-C the terminal to stop. Both paths "
                      "flush artefacts: %sthe line-buffered clicks.jsonl / "
                      "network.jsonl land on disk continuously.", walk,
                      f"trace chunks every {self.chunk_interval}s + "
                      if self.trace else "")
        deadline = time.monotonic() + max_duration
        last_chunk_at = time.monotonic()
        last_prefill_at = last_dom_at = 0.0
        reason = "timeout"
        try:
            while time.monotonic() < deadline:
                if self._done.is_set():
                    reason = "sigterm"
                    break
                if self.dom_interval > 0 and (
                        time.monotonic() - last_dom_at >= self.dom_interval):
                    last_dom_at = time.monotonic()
                    try:
                        self.snapshot_dom()
                    except Exception as exc:
                        self.log.debug("dom snapshot error: %s",
                                       debugcap.safe_error(exc))
                if (poll_prefill and self._prefill is not None and
                        time.monotonic() - last_prefill_at
                        >= PREFILL_POLL_SECONDS):
                    last_prefill_at = time.monotonic()
                    for page in list(self.context.pages):
                        try:
                            self.try_prefill(page)
                        except Exception as exc:
                            self.log.debug("prefill poll error: %s",
                                           debugcap.safe_error(exc))
                try:
                    self.context.wait_for_event("close", timeout=1000)
                    reason = "browser-closed"
                    break
                except PlaywrightTimeoutError:
                    if time.monotonic() - last_chunk_at >= self.chunk_interval:
                        self._save_trace_chunk()
                        last_chunk_at = time.monotonic()
        except KeyboardInterrupt:
            self.event({"kind": "signal", "ts": now_iso(),
                        "signal": "SIGINT"})
            reason = "sigint"
        self._stop(reason)
        return reason

    def _stop(self, reason: str) -> None:
        # One last snapshot so the final screen is on disk — skipped when
        # the browser is already gone and there is nothing to read.
        if self.dom_interval > 0 and reason != "browser-closed":
            with contextlib.suppress(Exception):
                self.snapshot_dom()
        self.event({"kind": "stopped", "ts": now_iso(), "reason": reason})
        if not self.trace:
            self.log.info("stopping (reason: %s)", reason)
            return
        # For Ctrl-C, SIGTERM and the timeout the context is alive and the
        # final chunk saves; on a closed browser the periodic chunks are
        # the record.
        final_ok = self._save_trace_chunk(label="final")
        self.log.info("stopping (reason: %s) — %d trace chunk(s) saved "
                      "(final chunk: %s)", reason, self.chunks,
                      "ok" if final_ok
                      else "browser dead, last periodic chunk is most-recent")
        with contextlib.suppress(Exception):
            self.context.tracing.stop(path=str(self.trace_path))

    def report(self) -> None:
        """Log where the artefacts are. Call once the stack has unwound, so
        the HAR it names has been flushed and scrubbed."""
        log = self.log
        log.info("artefacts written:")
        log.info("  clicks:        %s  (events + lifecycle)", self.clicks_path)
        log.info("  network:       %s  (requests + responses, crash-safe)",
                 self.network_path)
        log.info("  HAR:           %s  (scrubbed after the context close; "
                 "complete on Ctrl-C/SIGTERM exit, absent on browser-X "
                 "close)", self.har_path)
        log.info("  downloads:     %s/  (%d file(s))", self.downloads_dir,
                 self.downloads)
        if self.dom_interval > 0:
            log.info("  dom/:          %s/  (%d distinct screen(s))",
                     self.dom_dir, self.screens)
        if self.trace:
            log.info("  trace-chunks/: %s/  (%d chunk(s); open with "
                     "`playwright show-trace chunk-NNN.zip`). UNREDACTED: a "
                     "trace carries the typed credential — never commit it",
                     self.trace_chunks_dir, self.chunks)
