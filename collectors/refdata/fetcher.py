"""
One small HTTP client for every provider: reads only, a User-Agent that
names the tool, and a pace per host.

Every host gets one request a second. The SEC's hosts count as one
source, which allows ten a second; the client sends eight, a margin for
clock jitter. The client sends GET only, keeps no cookies and never
signs in anywhere.

A source that refuses a request for its rate (HTTP 429, or the SEC's
403) is not asked again in the same run: every later request to it
fails at once, and the run records each one.

Every exchange is traced when the run asks for it (`--debug`):
collectorkit.debugcap.HttpTrace writes one line per request, its URL,
status, timing and size, never a body.
"""

from __future__ import annotations

import json
import logging
import time
import urllib.error
import urllib.parse
import urllib.request
from dataclasses import dataclass

from collectorkit import debugcap

log = logging.getLogger("refdata.fetch")

VERSION = "1"
DEFAULT_INTERVAL = 1.0

# Hosts that share one pace and one refusal: a source with several hosts.
HOST_GROUPS = {"www.sec.gov": "sec", "data.sec.gov": "sec"}

# Seconds between two requests to one host or group, where it differs
# from one a second.
INTERVALS = {"sec": 0.125}

# Sources whose 403 means "too fast or undeclared", not "no such page".
REFUSES_WITH_403 = {"sec"}


class FetchError(Exception):
    """A request failed: no answer, or an HTTP error status."""

    def __init__(self, url, status=None, reason=""):
        self.url, self.status, self.reason = url, status, reason
        super().__init__(f"{url}: {status or 'no answer'} {reason}".strip())


@dataclass
class Response:
    url: str
    status: int
    body: bytes
    content_type: str

    def json(self):
        return json.loads(self.body.decode("utf-8"))

    def text(self, encoding="utf-8"):
        return self.body.decode(encoding, "replace")


class Client:
    def __init__(self, contact: str = "", *, trace: debugcap.HttpTrace | None = None,
                 sleep=time.sleep, clock=time.monotonic, opener=None, timeout: int = 60):
        self.contact = contact
        self.trace = trace or debugcap.HttpTrace(None, log=log, enabled=False)
        self._sleep, self._clock = sleep, clock
        self._last: dict[str, float] = {}
        self.refused: dict[str, str] = {}
        self._open = opener or urllib.request.urlopen
        self.timeout = timeout

    def user_agent(self) -> str:
        ua = f"wealthdb-refdata/{VERSION}"
        return f"{ua} {self.contact}" if self.contact else ua

    def _pace(self, source: str) -> None:
        interval = INTERVALS.get(source, DEFAULT_INTERVAL)
        last = self._last.get(source)
        if last is not None:
            wait = interval - (self._clock() - last)
            if wait > 0:
                self._sleep(wait)
        self._last[source] = self._clock()

    def get(self, url: str, *, params=None, headers=None) -> Response:
        if params:
            url = f"{url}?{urllib.parse.urlencode(params)}"
        return self._send(url, headers)

    def _send(self, url, headers) -> Response:
        host = urllib.parse.urlsplit(url).hostname or ""
        source = HOST_GROUPS.get(host, host)
        if source in self.refused:
            raise FetchError(url, None, f"not sent: {source} refused an earlier request ({self.refused[source]})")
        self._pace(source)
        req = urllib.request.Request(url, method="GET",
                                     headers={"User-Agent": self.user_agent(), **(headers or {})})
        t0 = time.monotonic()
        try:
            with self._open(req, timeout=self.timeout) as resp:
                data = resp.read()
                self.trace.record("GET", url, status=resp.status, elapsed_ms=(time.monotonic() - t0) * 1000,
                                  bytes_=len(data), headers=resp.headers)
                return Response(url=url, status=resp.status, body=data,
                                content_type=resp.headers.get("Content-Type", ""))
        except urllib.error.HTTPError as e:
            self.trace.record("GET", url, status=e.code, elapsed_ms=(time.monotonic() - t0) * 1000,
                              error=str(e.reason), headers=e.headers)
            if e.code == 429 or (e.code == 403 and source in REFUSES_WITH_403):
                self.refused[source] = f"{e.code} {e.reason}"
                log.warning("%s refused a request (%s %s); not asking it again this run", source, e.code, e.reason)
            raise FetchError(url, e.code, str(e.reason)) from e
        except OSError as e:
            self.trace.record("GET", url, elapsed_ms=(time.monotonic() - t0) * 1000,
                              error=f"{type(e).__name__}: {e}")
            raise FetchError(url, None, f"{type(e).__name__}: {e}") from e
