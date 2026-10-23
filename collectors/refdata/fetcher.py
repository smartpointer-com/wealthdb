"""
One small HTTP client for every provider: reads only, https only, a
User-Agent that names the tool, and a pace per host.

Every host gets one request a second. Where a source states a limit,
the client keeps under it:

- the SEC's hosts count as one source, which allows ten a second; the
  client sends eight;
- OpenFIGI allows 25 a minute without a key; the client sends 24;
- GLEIF allows 60 a minute; the client sends 50.

The client sends GET, and a POST only for a lookup whose API has no GET
form (OpenFIGI's mapping, Amundi's product API), a read either way, or
for a disclaimer a source shows before its pages (swissfunddata's
investor category). It keeps cookies only for a source that holds that
statement in a session, and never signs in anywhere.

A redirect is a request of its own: it keeps the pace and the refusals
of the host it leads to, and that host's User-Agent, so the SEC contact
never follows a redirect to another host.

A source that refuses a request for its rate (HTTP 429, or the SEC's
403) is not asked again in the same run: every later request to it
fails at once, and the run records each one.

Every exchange is traced when the run asks for it (`--debug`):
collectorkit.debugcap.HttpTrace writes one line per request, its URL,
status, timing and size, never a body.
"""

from __future__ import annotations

import http.cookiejar
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
INTERVALS = {"sec": 0.125, "api.openfigi.com": 60 / 24, "api.gleif.org": 1.2}

# Sources whose 403 means "too fast or undeclared", not "no such page".
REFUSES_WITH_403 = {"sec"}

# Domains that keep a visitor's stated investor category in a session
# cookie. No other host gets a cookie back.
SESSION_DOMAINS = (".swissfunddata.ch",)

# Sources that require a contact in the User-Agent. Every other host gets
# the tool's name alone: the address goes nowhere it is not asked for.
WANTS_CONTACT = {"sec"}


class FetchError(Exception):
    """A request failed: no answer, an HTTP error status, or an answer
    that is not what it should be."""

    def __init__(self, url, status=None, reason=""):
        self.url, self.status, self.reason = url, status, reason
        super().__init__(f"{url}: {status or 'no answer'} {reason}".strip())


@dataclass
class Response:
    url: str
    status: int
    body: bytes

    def json(self):
        """The body as JSON, a byte order mark allowed. A body that is
        not JSON fails the request."""
        try:
            return json.loads(self.body.decode("utf-8-sig"))
        except ValueError as e:
            raise FetchError(self.url, self.status, f"not JSON: {e}") from e

    def text(self, encoding="utf-8"):
        return self.body.decode(encoding, "replace")


def source_of(url: str) -> str:
    """The source a URL goes to: its host, or the group the host is in."""
    host = urllib.parse.urlsplit(url).hostname or ""
    return HOST_GROUPS.get(host, host)


class _Redirects(urllib.request.HTTPRedirectHandler):
    """Follows a redirect as a request of the client's own: traced, and
    sent only where the client's rules allow it, with the User-Agent of
    the host it leads to."""

    def __init__(self, client: Client):
        self.client = client

    def redirect_request(self, req, fp, code, msg, headers, newurl):
        self.client.trace.record(req.get_method(), req.full_url, status=code, headers=headers)
        new = super().redirect_request(req, fp, code, msg, headers, newurl)
        try:
            source = self.client.admit(new.full_url)
        except FetchError:
            fp.close()
            raise
        new.add_header("User-Agent", self.client.user_agent(source))
        return new


def build_opener(client: Client, https=None) -> urllib.request.OpenerDirector:
    """The client's opener: HTTPS only, the session cookies, redirects
    through the client's rules, and an HTTP error status raised as an
    error. `https` stands in for the network in tests."""
    opener = urllib.request.OpenerDirector()
    for handler in (https or urllib.request.HTTPSHandler(), urllib.request.HTTPCookieProcessor(client.cookies),
                    _Redirects(client), urllib.request.HTTPErrorProcessor(),
                    urllib.request.HTTPDefaultErrorHandler(), urllib.request.UnknownHandler()):
        opener.add_handler(handler)
    return opener


class Client:
    def __init__(self, contact: str = "", *, trace: debugcap.HttpTrace | None = None,
                 sleep=time.sleep, clock=time.monotonic, https=None, timeout: int = 60):
        self.contact = contact
        self.trace = trace or debugcap.HttpTrace(None, log=log, enabled=False)
        self._sleep, self._clock = sleep, clock
        self._last: dict[str, float] = {}
        self.refused: dict[str, str] = {}
        self.cookies = http.cookiejar.CookieJar(http.cookiejar.DefaultCookiePolicy(allowed_domains=SESSION_DOMAINS))
        self._open = build_opener(self, https).open
        self.timeout = timeout

    def user_agent(self, source: str = "") -> str:
        ua = f"wealthdb-refdata/{VERSION}"
        return f"{ua} {self.contact}" if self.contact and source in WANTS_CONTACT else ua

    def _pace(self, source: str) -> None:
        interval = INTERVALS.get(source, DEFAULT_INTERVAL)
        last = self._last.get(source)
        if last is not None:
            wait = interval - (self._clock() - last)
            if wait > 0:
                self._sleep(wait)
        self._last[source] = self._clock()

    def admit(self, url: str) -> str:
        """The source a request goes to, once the client's rules let it
        go: an https URL, to a source that has not refused, at its pace."""
        if urllib.parse.urlsplit(url).scheme != "https":
            raise FetchError(url, None, "not sent: the client asks https:// URLs only")
        source = source_of(url)
        if source in self.refused:
            raise FetchError(url, None, f"not sent: {source} refused an earlier request ({self.refused[source]})")
        self._pace(source)
        return source

    def get(self, url: str, *, params=None, headers=None) -> Response:
        if params:
            url = f"{url}?{urllib.parse.urlencode(params)}"
        return self._send("GET", url, None, headers)

    def post_json(self, url: str, payload, *, headers=None) -> Response:
        """A read-only POST: a lookup whose API has no GET form."""
        body = json.dumps(payload).encode("utf-8")
        return self._send("POST", url, body, {"Content-Type": "application/json", **(headers or {})})

    def post_form(self, url: str, fields: dict, *, headers=None) -> Response:
        """A form POST that reads nothing and changes nothing at the source:
        the statement a disclaimer asks for before it shows its pages."""
        body = urllib.parse.urlencode(fields).encode("utf-8")
        return self._send("POST", url, body, {"Content-Type": "application/x-www-form-urlencoded", **(headers or {})})

    def _send(self, method, url, body, headers) -> Response:
        source = self.admit(url)
        req = urllib.request.Request(url, data=body, method=method,
                                     headers={**(headers or {}), "User-Agent": self.user_agent(source)})
        t0 = time.monotonic()
        try:
            with self._open(req, timeout=self.timeout) as resp:
                data = resp.read()
                self.trace.record(method, resp.geturl(), status=resp.status,
                                  elapsed_ms=(time.monotonic() - t0) * 1000, bytes_=len(data), headers=resp.headers)
                return Response(url=resp.geturl(), status=resp.status, body=data)
        except urllib.error.HTTPError as e:
            # A redirect may have led elsewhere: the source that refused
            # is the one that answered.
            self.trace.record(method, e.filename or url, status=e.code, elapsed_ms=(time.monotonic() - t0) * 1000,
                              error=str(e.reason), headers=e.headers)
            refusing = source_of(e.filename or url)
            if e.code == 429 or (e.code == 403 and refusing in REFUSES_WITH_403):
                self.refused[refusing] = f"{e.code} {e.reason}"
                log.warning("%s refused a request (%s %s); not asking it again this run", refusing, e.code, e.reason)
            raise FetchError(e.filename or url, e.code, str(e.reason)) from e
        except OSError as e:
            reason = debugcap.safe_error(e)
            self.trace.record(method, url, elapsed_ms=(time.monotonic() - t0) * 1000, error=reason)
            raise FetchError(url, None, reason) from e
