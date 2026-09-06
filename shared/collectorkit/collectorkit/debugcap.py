"""Bronze-resident diagnostic captures — the `--debug` gate's payload.

`--debug` is the fleet's uniform "tell me what the source actually said"
switch. What is worth capturing differs by transport, so this module offers
one helper per kind rather than one shape for all:

  * :func:`capture_page`  — a browser page's DOM + screenshot (Playwright /
    Camoufox collectors).
  * :class:`HttpTrace`    — request metadata for a REST collector: what was
    asked, what came back, how long it took.
  * :class:`BodyCapture`  — the exception to the bronze-resident rule:
    buffered response bodies for one-off *login*-flow diagnosis, landing in
    a debug dir outside bronze (there is no bronze run to house them), under
    the screenshots' NEVER-commit contract.

The contract, identical for both:

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
    beside it. A credential is different: repo CLAUDE.md §3 forbids
    persisting one to disk at all. URLs are redacted (fred's API key travels
    in the query string) and response headers are whitelisted, never
    blanket-copied, because that is where cookies and bearer tokens live.
"""
from __future__ import annotations

import json
import logging
import re
import time
import urllib.parse
from pathlib import Path

# The subdir every collector's `prune` already nominates as debug artefacts.
# Keeping the name identical fleet-wide is what lets prune reclaim captures
# without a per-collector category.
SCREENSHOTS_DIR = "screenshots"

# Query parameters whose value is a credential. fred puts its API key in the
# query string (collectors/fred/CLAUDE.md §2), and a captured URL would
# otherwise persist it to disk.
_SECRET_PARAMS = frozenset({
    "api_key", "apikey", "key", "token", "access_token", "refresh_token",
    "id_token", "secret", "client_secret", "password", "passwd", "pwd",
    "sig", "signature", "auth", "authorization", "session", "sessionid",
})

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

REDACTED = "<redacted>"

# Header names whose VALUE is a credential. The query-parameter set covers
# most of them (`apikey`, `authorization`, `token`, …); cookies are
# header-only and are the ones that carry a live session.
_SECRET_HEADERS = frozenset(_SECRET_PARAMS | {
    "cookie", "set-cookie", "x-api-key", "x-auth-token", "proxy-authorization",
})


def redact_url(url: str) -> str:
    """Return `url` with credential-bearing query parameters masked.

    Keeps the parameter NAME — that a request carried an `api_key` is the
    diagnostic; its value is the secret. A URL that cannot be parsed is
    dropped entirely rather than guessed at: an unparseable string is
    precisely the case where a naive mask could miss.
    """
    try:
        parts = urllib.parse.urlsplit(url)
    except ValueError:
        return REDACTED
    if not parts.query:
        return url
    pairs = urllib.parse.parse_qsl(parts.query, keep_blank_values=True)
    masked = [(k, REDACTED if k.lower() in _SECRET_PARAMS else v)
              for k, v in pairs]
    return urllib.parse.urlunsplit(
        parts._replace(query=urllib.parse.urlencode(masked)))


def safe_error(exc: BaseException) -> str:
    """An exception rendered safely enough to persist.

    A transport client's exception is not a one-line message. Playwright's
    request errors append a ``Call log:`` block reproducing every request
    header — which for an authenticated session means the API key and the
    whole cookie jar. Anything that writes ``str(exc)`` into a manifest, a
    log file, or a captured artefact therefore writes a live credential
    to disk.

    This keeps what diagnoses (the exception class and its first line,
    which carries the failure and the timeout) and drops what leaks
    (everything from the call log on), then masks credential-bearing query
    parameters in whatever URL survives.
    """
    first = str(exc).split("\n", 1)[0].strip()
    marker = first.find("Call log:")
    if marker >= 0:
        first = first[:marker].strip()
    # A URL in the message can carry its own secret in the query string.
    first = re.sub(r"https?://\S+", lambda m: redact_url(m.group(0)), first)
    return f"{type(exc).__name__}: {first}" if first else type(exc).__name__


def redact_headers(headers) -> dict:
    """Header mapping with credential-bearing values masked by NAME.

    The complement of :func:`redact_url`: that masks a secret whose value
    is known from the query parameter it sits in, this masks one whose
    value is only knowable from the header it arrived in — an ``apikey``
    a site issues at runtime, a ``cookie`` a session sets. Keeping the
    name and dropping the value preserves the diagnostic (that the
    request carried a key) without the key.
    """
    if not headers:
        return {}
    try:
        items = headers.items()
    except AttributeError:
        return {}
    return {k: (REDACTED if k.lower() in _SECRET_HEADERS else v)
            for k, v in items}


def capture_dir(run_dir: Path) -> Path:
    """`<run_dir>/screenshots`, created. The one place captures go."""
    d = Path(run_dir) / SCREENSHOTS_DIR
    d.mkdir(parents=True, exist_ok=True)
    return d


def capture_page(page, run_dir: Path, name: str, *,
                 log: logging.Logger, png: bool = True) -> None:
    """Capture a Playwright / Camoufox `page` as ``<name>.html`` (+ ``.png``).

    The DOM is what a parser selector is matched against, so it is the
    capture that explains a scrape that found nothing; the screenshot
    explains the ones the DOM cannot — an overlay, a consent wall, a
    challenge. Each is written independently: a screenshot timing out on a
    busy page must not cost the DOM dump too.
    """
    d = capture_dir(run_dir)
    try:
        (d / f"{name}.html").write_text(page.content(), encoding="utf-8")
    except Exception as e:  # noqa: BLE001 - any driver error, never fatal
        log.warning("--debug: DOM capture %s failed: %s", name, e)
    if not png:
        return
    try:
        page.screenshot(path=str(d / f"{name}.png"), full_page=True)
    except Exception as e:  # noqa: BLE001
        log.warning("--debug: screenshot %s failed: %s", name, e)


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
    NEVER-commit contract as screenshots. Best effort throughout: a
    capture failure never disturbs the run being diagnosed.

    A no-op when constructed with ``out_dir=None``, so call sites attach
    and flush unconditionally instead of guarding every call.
    """

    def __init__(self, out_dir: Path | None, *, host_markers,
                 log: logging.Logger, max_bytes: int = 200_000,
                 max_responses: int = 300) -> None:
        self._log = log
        self._out_dir = Path(out_dir) if out_dir is not None else None
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
                body = resp.text()[:self._max_bytes]
                slug = re.sub(r"[^A-Za-z0-9._-]+", "-",
                              urllib.parse.urlsplit(resp.url).path).strip("-")
                name = f"body-{n:03d}-{resp.status}-{slug[:60]}.json"
                (self._out_dir / name).write_text(json.dumps({
                    "url": redact_url(resp.url),
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
