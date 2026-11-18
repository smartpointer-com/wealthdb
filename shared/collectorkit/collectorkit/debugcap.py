"""Bronze-resident diagnostic captures — the `--debug` gate's payload.

`--debug` is the fleet's uniform "tell me what the source actually said"
switch. What is worth capturing differs by transport, so this module offers
one helper per kind rather than one shape for all:

  * :func:`capture_page`  — a browser page's DOM + screenshot (Playwright /
    Camoufox collectors).
  * :class:`HttpTrace`    — request metadata for a REST collector: what was
    asked, what came back, how long it took. :func:`traced_get` is the
    urllib GET that records every exchange into one.
  * :class:`BodyCapture`  — the exception to the bronze-resident rule:
    buffered response bodies for one-off *login*-flow diagnosis, landing in
    a debug dir outside bronze (there is no bronze run to house them), under
    the screenshots' NEVER-commit contract.
  * :func:`tee_debug_log` — the run's full DEBUG log, beside the captures.

Sign-in diagnostics land in a debug dir the run is handed
(``--screenshot-dir``), since a sign-in has no bronze run either:

  * :func:`capture_landmark` — a timestamped DOM + screenshot of the page
    at a step of the flow.
  * :func:`stop_trace_if_active` — the Playwright trace, saved as a zip.
  * :func:`sample_api_calls` / :func:`merge_api_sample` /
    :func:`report_api_calls` — which XHR/fetch calls a page made, and for
    how long each was held, id-masked.

The contract for the bronze-resident pair, :func:`capture_page` and
:class:`HttpTrace`:

  * Captures land in ``<run>/screenshots/`` — the same subdir name the
    fleet's `prune` already nominates via ``debug_subdirs``, so a complete
    dump's captures are reclaimed wholesale and no collector needs a new
    prune category.
  * ``load`` never reads them. They are diagnostics, not inputs, so deleting
    them can never change silver.
  * **Best effort.** A capture that fails warns and returns; it must never
    take down a download that was otherwise working. Diagnostics that break
    the run they are diagnosing are worse than no diagnostics.
  * **No secrets.** Bronze holds real financial data and is private, so a
    capture may contain account data — that is no worse than the artefacts
    beside it. A credential is different: repo AGENTS.md §3 forbids
    persisting one to disk at all. URLs are redacted (fred's API key travels
    in the query string) and response headers are whitelisted, never
    blanket-copied, because that is where cookies and bearer tokens live.

The masks themselves (:func:`secret_redactor`, :func:`scrub_dom`,
:func:`redact_har` and the rest) are :mod:`collectorkit.redact`, a pure
text library. This module re-exports every name of it.

What this module cannot clean, said plainly so no caller reads a capture as
safe that is not: a **Playwright trace** (``trace.zip`` / ``trace-chunks/``)
is written by the driver as a zip of sha1-named blobs, DOM frame-snapshots
that carry every input's value, and per-action screenshots. Nothing here
rewrites it, and a value-based redactor could not: a password typed by hand
is unknown to the process capturing it. A trace holds credentials — keep it
under the NEVER-commit debug dir, and record a sign-in with
``snapshots=False`` so the DOM snapshots that carry the typed value are
never taken. A **HAR** is the tractable one: it is JSON, so
:func:`redact_har` rewrites it in place once the context close has written
it.
"""
from __future__ import annotations

import json
import logging
import re
import time
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

from collectorkit.bronze import ts_slug
from collectorkit.cli import LOG_FORMAT
# The redaction library, re-exported: every capture site reads it from here.
from collectorkit.redact import (  # noqa: F401
    REDACTED,
    SessionMask,
    _SECRET_HEADERS,
    _SECRET_PARAMS,
    env_redactor,
    redact_body,
    redact_har,
    redact_headers,
    redact_url,
    safe_error,
    scrub_dom,
    secret_redactor,
    secret_variants,
    session_redactor,
    url_shape,
)

# The subdir every collector's `prune` already nominates as debug artefacts.
# Keeping the name identical fleet-wide is what lets prune reclaim captures
# without a per-collector category.
SCREENSHOTS_DIR = "screenshots"

# Response headers worth keeping. A whitelist, not a blocklist: the useful
# ones are few and known, while the dangerous ones (Set-Cookie,
# Authorization, WWW-Authenticate) are exactly what a blocklist forgets.
# Rate-limit headers earn their place — they explain a slow or truncated
# walk better than anything else the run records.
_SAFE_RESPONSE_HEADERS = (
    "content-type", "content-length", "content-encoding", "date", "server",
    "retry-after", "x-ratelimit-limit", "x-ratelimit-remaining",
    "x-ratelimit-reset", "x-request-id", "cf-ray", "age", "etag",
    "last-modified",
)


def capture_dir(run_dir: Path) -> Path:
    """`<run_dir>/screenshots`, created. The one place captures go."""
    d = Path(run_dir) / SCREENSHOTS_DIR
    d.mkdir(parents=True, exist_ok=True)
    return d


def capture_page(page, run_dir: Path, name: str, *,
                 log: logging.Logger, png: bool = True, redact) -> None:
    """Capture a Playwright / Camoufox `page` as ``<name>.html`` (+ ``.png``).

    The DOM is what a parser selector is matched against, so it is the
    capture that explains a scrape that found nothing; the screenshot
    explains the ones the DOM cannot — an overlay, a consent wall, a
    challenge. Each is written independently: a screenshot timing out on a
    busy page must not cost the DOM dump too.

    The markup goes through :func:`scrub_dom` — a captured sign-in page can
    serialize the typed password as a ``value`` attribute. `redact` (a
    :func:`secret_redactor`, or a session mask) masks the credentials the
    site knows elsewhere in the markup too. It has no default: a site that
    masks nothing says so with ``redact=None``.
    """
    d = capture_dir(run_dir)
    try:
        (d / f"{name}.html").write_text(scrub_dom(page.content(), redact),
                                        encoding="utf-8")
    except Exception as e:  # noqa: BLE001 - any driver error, never fatal
        log.warning("--debug: DOM capture %s failed: %s", name, e)
    if not png:
        return
    try:
        page.screenshot(path=str(d / f"{name}.png"), full_page=True)
    except Exception as e:  # noqa: BLE001
        log.warning("--debug: screenshot %s failed: %s", name, e)


def page_capturer(debug_dir: Path | None, *, log: logging.Logger):
    """``capture(page, name)`` for a walk that holds only a session.

    Each call is :func:`capture_page` into `debug_dir`, masked by a
    :class:`SessionMask`. Such a walk is handed no password, only a lifted
    session, so the jar is what a capture could leak: an SPA that echoes
    its session into a meta tag or a bootstrap script puts it in the
    markup. The jar is read at the first capture. A None `debug_dir` (no
    ``--debug``) makes every call a no-op.
    """
    mask = SessionMask()

    def capture(page, name: str) -> None:
        if debug_dir is not None:
            capture_page(page, debug_dir, name, log=log,
                         redact=mask.for_page(page))

    return capture


def capture_landmark(page, out_dir: Path | None, label: str, *,
                     log: logging.Logger, png_timeout_ms: int = 3_000,
                     redact=None) -> None:
    """Save `page` as ``<ts>-<label>.html`` and ``.png`` in `out_dir`.

    The sign-in twin of :func:`capture_page`: `out_dir` is a debug dir the
    run was handed (``--screenshot-dir``), outside bronze, and the
    timestamp keeps a re-visited landmark from overwriting the last one.
    None captures nothing. Never raises.

    The DOM comes first. `page.content()` can fail while an SPA is
    "navigating and changing", so the fallback reads the DOM in-page,
    which has no such guard. The markup goes through :func:`scrub_dom`,
    as in :func:`capture_page`. The screenshot is the viewport only,
    animations off, capped at `png_timeout_ms`: Firefox can wait for
    fonts that never finish loading, so a missed screenshot is logged at
    debug and accepted.
    """
    if out_dir is None:
        return
    try:
        out_dir.mkdir(parents=True, exist_ok=True)
    except OSError as e:
        log.warning("could not create capture dir %s: %s", out_dir, e)
        return
    stem = out_dir / f"{ts_slug()}-{label}"
    try:
        try:
            html = page.content()
        except Exception:  # noqa: BLE001 - any driver error
            html = page.evaluate("() => document.documentElement.outerHTML")
        html_path = stem.with_name(stem.name + ".html")
        html_path.write_text(scrub_dom(html, redact), encoding="utf-8")
        log.debug("wrote HTML %s", html_path)
    except Exception as e:  # noqa: BLE001 - diagnostics never break a run
        log.warning("html capture %s failed: %s", label, e)
    try:
        png_path = stem.with_name(stem.name + ".png")
        page.screenshot(path=str(png_path), full_page=False,
                        timeout=png_timeout_ms, animations="disabled")
        log.debug("wrote screenshot %s", png_path)
    except Exception as e:  # noqa: BLE001
        log.debug("screenshot %s failed (HTML saved): %s", label, e)


def stop_trace_if_active(context, trace: bool, out_dir: Path | None,
                         label: str, *, log: logging.Logger) -> bool:
    """Stop the running Playwright trace into
    ``<out_dir>/<ts>-<label>-trace.zip``. Returns whether it was saved,
    which tells a caller that a fresh trace may be started.

    Nothing happens without `trace`. A trace with no `out_dir` is
    discarded with a warning. Best effort: a stop that fails is logged and
    reported as False, never raised. The zip holds what the trace
    recorded, unredacted (see the module docstring).
    """
    if not trace:
        return False
    if out_dir is None:
        log.warning("--trace without --screenshot-dir; trace discarded")
        return False
    try:
        out_dir.mkdir(parents=True, exist_ok=True)
        trace_path = out_dir / f"{ts_slug()}-{label}-trace.zip"
        context.tracing.stop(path=str(trace_path))
    except Exception as e:  # noqa: BLE001 - diagnostics never break a run
        log.warning("stop trace failed: %s", e)
        return False
    log.info("trace saved to %s", trace_path)
    return True


class HttpTrace:
    """Append-only record of a REST collector's requests.

    Lands at ``<run>/screenshots/http-trace.jsonl``, one JSON object per
    request. Deliberately records metadata only — status, timing, size,
    whitelisted headers — never bodies: a successful body is already in
    bronze beside this file, and duplicating it would double the dump for
    nothing. What bronze does NOT record is the shape of the exchange, which
    is exactly what is needed when an endpoint starts rate-limiting,
    redirecting, or answering 200 with an error envelope.

    A no-op when constructed with ``enabled=False``, so call sites read
    ``trace.record(...)`` unconditionally instead of guarding every one.
    """

    FILENAME = "http-trace.jsonl"

    def __init__(self, run_dir: Path | None, *, log: logging.Logger,
                 enabled: bool = True) -> None:
        self._log = log
        self._enabled = bool(enabled and run_dir is not None)
        self._path: Path | None = None
        if self._enabled:
            try:
                self._path = capture_dir(run_dir) / self.FILENAME
            except OSError as e:
                log.warning("--debug: http trace unavailable: %s", e)
                self._enabled = False

    def record(self, method: str, url: str, *, status: int | None = None,
               elapsed_ms: float | None = None, bytes_: int | None = None,
               headers=None, error: str | None = None) -> None:
        """Append one exchange. Never raises."""
        if not self._enabled or self._path is None:
            return
        entry = {
            "ts": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
            "method": method,
            "url": redact_url(url),
        }
        if status is not None:
            entry["status"] = status
        if elapsed_ms is not None:
            entry["elapsed_ms"] = round(elapsed_ms, 1)
        if bytes_ is not None:
            entry["bytes"] = bytes_
        if error is not None:
            entry["error"] = error
        safe = self._safe_headers(headers)
        if safe:
            entry["headers"] = safe
        try:
            with self._path.open("a", encoding="utf-8") as f:
                f.write(json.dumps(entry) + "\n")
        except OSError as e:
            self._log.warning("--debug: http trace write failed: %s", e)
            self._enabled = False

    @staticmethod
    def _safe_headers(headers) -> dict:
        if not headers:
            return {}
        try:
            items = headers.items()
        except AttributeError:
            return {}
        return {k.lower(): v for k, v in items
                if k.lower() in _SAFE_RESPONSE_HEADERS}


def traced_get(url: str, *, user_agent: str, timeout: float,
               trace: HttpTrace) -> bytes:
    """GET one URL with urllib and return its body. Raises on transport
    and HTTP errors.

    The single request choke point of a collector that GETs with urllib,
    so every exchange is traced here: the 200, the HTTP error and the
    transport fault that never reached the source. An HTTPError's body is
    left unread for the caller to log; only its status, reason and headers
    are taken. The URL goes to the trace whole: masking a secret it
    carries is HttpTrace's job, never the call site's. A transport
    error's message is masked here (safe_error)."""
    req = urllib.request.Request(url, headers={"User-Agent": user_agent})
    t0 = time.monotonic()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            body = resp.read()
            trace.record("GET", url, status=resp.status,
                         elapsed_ms=(time.monotonic() - t0) * 1000,
                         bytes_=len(body), headers=resp.headers)
    except urllib.error.HTTPError as e:
        trace.record("GET", url, status=e.code,
                     elapsed_ms=(time.monotonic() - t0) * 1000,
                     error=str(e.reason), headers=e.headers)
        raise
    except OSError as e:
        # URLError, TimeoutError and the socket/TLS errors all land here:
        # no status, because nothing came back. The message can quote the
        # URL, so it is masked as the URL field is.
        trace.record("GET", url, elapsed_ms=(time.monotonic() - t0) * 1000,
                     error=safe_error(e))
        raise
    return body


class BodyCapture:
    """Response-body capture for a Playwright BrowserContext — one-off
    login/flow diagnosis instrumentation. The per-screen DOM captures
    show what rendered; the bodies show what the server actually said.

    Split into two halves because Playwright's sync API forbids blocking
    calls inside event handlers: :meth:`attach` registers a listener
    that only *records* matching responses (cheap, non-blocking), and
    :meth:`flush` — called from the main flow, typically right before
    the context closes or on a terminal error — reads the buffered
    bodies and writes one JSON file per response (redacted URL, status,
    content type, truncated body) named ``body-NNN-<status>-<slug>.json``.

    Auth-host bodies can carry session identifiers, so captures belong
    in a debug dir outside bronze and outside the repo — the same
    NEVER-commit contract as screenshots. `redact` (a
    :func:`secret_redactor`) masks the credentials the call site knows —
    the login id a sign-in endpoint echoes back, the password a form POST
    is answered with — in the body and in the URL the file is NAMED after;
    a session identifier the server minted is not one of them, and the
    location contract above stays its only mitigation. Best effort
    throughout: a capture failure never disturbs the run being diagnosed.

    A no-op when constructed with ``out_dir=None``, so call sites attach
    and flush unconditionally instead of guarding every call.
    """

    def __init__(self, out_dir: Path | None, *, host_markers,
                 log: logging.Logger, redact=None, max_bytes: int = 200_000,
                 max_responses: int = 300) -> None:
        self._log = log
        self._out_dir = Path(out_dir) if out_dir is not None else None
        self._redact = redact
        self._host_markers = tuple(m.lower() for m in host_markers)
        self._max_bytes = max_bytes
        self._max_responses = max_responses
        self._responses: list = []

    def attach(self, context) -> None:
        """Register the recording listener. No-op when disabled."""
        if self._out_dir is None:
            return
        context.on("response", self._record)

    def _record(self, response) -> None:
        """Buffer a matching response. Non-blocking — body reads wait
        for flush(). Never raises."""
        try:
            if len(self._responses) >= self._max_responses:
                return
            host = urllib.parse.urlsplit(response.url).netloc.lower()
            if any(m in host for m in self._host_markers):
                self._responses.append(response)
        except Exception:  # noqa: BLE001 — diagnostics never break the run
            pass

    def flush(self) -> int:
        """Write the buffered bodies; returns how many files landed.
        Bodies that are gone (redirects, evicted buffers) are skipped."""
        if self._out_dir is None or not self._responses:
            return 0
        written = 0
        try:
            self._out_dir.mkdir(parents=True, exist_ok=True)
        except OSError as e:
            self._log.warning("body capture dir unavailable: %s", e)
            return 0
        for n, resp in enumerate(self._responses, start=1):
            try:
                ctype = (resp.headers or {}).get("content-type", "")
                if not any(t in ctype for t in ("json", "html", "text",
                                                "xml")):
                    continue
                # Redact the RAW body and the RAW url, before the slice and
                # before json.dumps: a redactor run on the serialized file
                # would face a doubly-escaped spelling of a secret carrying
                # a quote or a backslash, which secret_variants (one round
                # of JSON escaping) does not know. The slug comes from the
                # path, which redact_url never touches, so an identifier in
                # a path segment would otherwise land in the file NAME.
                body, url = resp.text(), resp.url
                if self._redact is not None:
                    body, url = self._redact(body), self._redact(url)
                body = body[:self._max_bytes]
                slug = re.sub(r"[^A-Za-z0-9._-]+", "-",
                              urllib.parse.urlsplit(url).path).strip("-")
                name = f"body-{n:03d}-{resp.status}-{slug[:60]}.json"
                (self._out_dir / name).write_text(json.dumps({
                    "url": redact_url(url),
                    "status": resp.status,
                    "content_type": ctype,
                    "body": body,
                }, ensure_ascii=False, indent=1), encoding="utf-8")
                written += 1
            except Exception as e:  # noqa: BLE001
                self._log.debug("body capture write failed: %s", e)
        self._responses.clear()
        if written:
            self._log.info("captured %d response bodies to %s",
                           written, self._out_dir)
        return written




def sample_api_calls(page) -> list:
    """The current document's XHR/fetch calls, as ``[url, ms]`` pairs.

    Read out of `performance.getEntriesByType('resource')` rather than off
    a Playwright response event: sync-Playwright callbacks fire only inside
    a Playwright call, so a wait loop that sleeps between polls could miss
    what the page did in between. The timing buffer has no such gap, but
    it IS per-document, so a caller samples while the page of interest is
    still open, not once at the end.

    The duration is the point, not just the URL: a server-held long poll
    lasts seconds, a status check milliseconds, and that says which call a
    page is waiting on.

    Best effort: a page mid-navigation yields nothing.
    """
    try:
        return page.evaluate(
            "() => performance.getEntriesByType('resource')"
            "  .filter(e => e.initiatorType === 'xmlhttprequest'"
            "            || e.initiatorType === 'fetch')"
            "  .map(e => [e.name, Math.round(e.duration)])"
        ) or []
    except Exception:  # noqa: BLE001 - diagnostics never break a run
        return []


def merge_api_sample(seen: dict, sample) -> dict:
    """Fold one :func:`sample_api_calls` sample into `seen`, keyed by
    :func:`url_shape`, as ``{shape: {"calls": n, "ms": longest}}``.

    The timing buffer accumulates within a document and resets on
    navigation, so the highest count any single sample reported is the
    per-document peak, and the longest duration ever seen is the one that
    matters. Both are taken as maxima: a sum would multiply the same
    entries by the number of samples.
    """
    counts: dict = {}
    longest: dict = {}
    for name, ms in sample or []:
        shape = url_shape(str(name))
        counts[shape] = counts.get(shape, 0) + 1
        longest[shape] = max(longest.get(shape, 0), int(ms or 0))
    for shape, n in counts.items():
        row = seen.setdefault(shape, {"calls": 0, "ms": 0})
        row["calls"] = max(row["calls"], n)
        row["ms"] = max(row["ms"], longest[shape])
    return seen


def report_api_calls(seen: dict, out_dir: Path | None, *,
                     log: logging.Logger, title: str = "the page",
                     filename: str = "api_calls.txt") -> None:
    """Log the calls :func:`merge_api_sample` gathered, longest first, and
    save the list as `filename` in `out_dir` when one is given.

    The list re-derives an endpoint that moved: it is id-masked, so a line
    can be pasted into a report, and ordered by duration, because the call
    a page is WAITING on is the one to poll. `title` names the page in the
    log line.
    """
    if not seen:
        return
    rows = sorted(seen.items(), key=lambda kv: (-kv[1]["ms"], kv[0]))
    lines = [f'{row["ms"]:>7}ms  x{row["calls"]:<3} {shape}'
             for shape, row in rows]
    log.info("%s issued %d distinct API call shape(s), longest-held first:",
             title, len(rows))
    for line in lines:
        log.info("    %s", line)
    if not out_dir:
        return
    try:
        out_dir.mkdir(parents=True, exist_ok=True)
        (out_dir / filename).write_text("\n".join(lines) + "\n",
                                        encoding="utf-8")
    except Exception as e:  # noqa: BLE001
        log.warning("could not save the API-call list: %s", e)


def tee_debug_log(debug_dir: Path | None, console_level: int, *,
                  log: logging.Logger) -> logging.Handler | None:
    """Write the whole run's log, at DEBUG, to ``<debug_dir>/<ts>-run.log``
    beside the captures, while the console keeps `console_level`. A capture
    dir without the log that led up to it shows where a run failed but not
    why. Returns the handler, or None when there is no dir or it cannot be
    opened — a debug aid never takes down the run."""
    if debug_dir is None:
        return None
    try:
        debug_dir.mkdir(parents=True, exist_ok=True)
        handler = logging.FileHandler(debug_dir / f"{ts_slug()}-run.log",
                                      encoding="utf-8")
    except OSError as e:
        log.warning("could not open the debug log file: %s", e)
        return None
    handler.setLevel(logging.DEBUG)
    handler.setFormatter(logging.Formatter(LOG_FORMAT))
    root = logging.getLogger()
    # Pin existing (console) handlers to their current verbosity, then open
    # the root to DEBUG so the file sees everything.
    for h in root.handlers:
        if h.level == logging.NOTSET:
            h.setLevel(console_level)
    root.setLevel(logging.DEBUG)
    root.addHandler(handler)
    log.info("full debug log: %s", handler.baseFilename)
    return handler

