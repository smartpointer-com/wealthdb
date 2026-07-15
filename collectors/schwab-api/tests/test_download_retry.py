"""Unit tests for download.py's transient-retry + timeout wiring, and the
--debug HTTP trace it records from the same choke point.

No network: the schwab-py request callable is a stub that raises or
returns a fake HTTPX response. `time.sleep` is patched out so the
exponential backoff doesn't slow the suite. Synthetic values only.
"""
from __future__ import annotations

import json
import sys
from pathlib import Path

import httpcore
import httpx
import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import download  # noqa: E402
from collectorkit import debugcap  # noqa: E402

URL = "https://api.schwabapi.com/trader/v1/accounts"


class _FakeResponse:
    """The slice of httpx.Response `_request_json` touches — including the
    `request` / `headers` / `content` the --debug trace reads off it."""

    def __init__(self, status_code=200, payload=None, text="", url=URL):
        self.status_code = status_code
        self._payload = {"ok": True} if payload is None else payload
        self.text = text
        self.request = httpx.Request("GET", url)
        self.headers = httpx.Headers({"content-type": "application/json"})
        self.content = json.dumps(self._payload).encode()

    def json(self):
        return self._payload


@pytest.fixture(autouse=True)
def _no_sleep(monkeypatch):
    monkeypatch.setattr(download.time, "sleep", lambda *_a, **_k: None)


def test_retries_transient_timeout_then_succeeds():
    calls = {"n": 0}

    def call():
        calls["n"] += 1
        if calls["n"] < 3:
            raise httpx.ReadTimeout("read timed out")
        return _FakeResponse(payload={"done": calls["n"]})

    out = download._request_json("thing", call, max_retries=5)
    assert out == {"done": 3}
    assert calls["n"] == 3  # failed twice, succeeded on the third


def test_retries_unmapped_httpcore_timeout():
    # A failure seen in the wild: httpcore.ReadTimeout leaking un-mapped
    # past httpx. Must still be treated as retryable.
    calls = {"n": 0}

    def call():
        calls["n"] += 1
        if calls["n"] == 1:
            raise httpcore.ReadTimeout("The read operation timed out")
        return _FakeResponse()

    download._request_json("thing", call, max_retries=3)
    assert calls["n"] == 2


def test_retries_dropped_connection():
    calls = {"n": 0}

    def call():
        calls["n"] += 1
        if calls["n"] == 1:
            raise httpx.ConnectError("connection reset")
        return _FakeResponse()

    download._request_json("thing", call, max_retries=3)
    assert calls["n"] == 2


def test_gives_up_after_max_retries():
    def call():
        raise httpx.ConnectTimeout("connect timed out")

    with pytest.raises(SystemExit) as ei:
        download._request_json("thing", call, max_retries=3)
    assert "failed after 3 attempt" in str(ei.value)


def test_http_status_error_is_not_retried():
    # A non-2xx response is a status error, not a transport fault — it
    # must surface immediately (schwab_get_json), never retried.
    calls = {"n": 0}

    def call():
        calls["n"] += 1
        return _FakeResponse(status_code=429, text="rate limited")

    with pytest.raises(SystemExit) as ei:
        download._request_json("thing", call, max_retries=5)
    assert calls["n"] == 1
    assert "HTTP 429" in str(ei.value)


# --- the --debug HTTP trace, recorded from the same choke point ------------

def _trace(tmp_path):
    return debugcap.HttpTrace(tmp_path, log=download.log, enabled=True)


def _lines(tmp_path):
    p = tmp_path / debugcap.SCREENSHOTS_DIR / debugcap.HttpTrace.FILENAME
    return [json.loads(x) for x in p.read_text().splitlines()] if p.exists() else []


def test_debug_parses_and_defaults_off():
    base = ["--token-path", "/x", "--bronze-dir", "/y"]
    assert download.parse_args(base).debug is False
    assert download.parse_args([*base, "--debug"]).debug is True


def test_trace_records_a_successful_request(tmp_path):
    download._request_json("thing", lambda: _FakeResponse(),
                           trace=_trace(tmp_path))
    entry = _lines(tmp_path)[0]
    assert (entry["method"], entry["status"], entry["url"]) == ("GET", 200, URL)
    assert entry["headers"]["content-type"] == "application/json"


def test_trace_records_every_retry_not_just_the_winner(tmp_path):
    # A timeout retried into a success is the shape of the exchange --debug
    # exists to show; recording only the attempt that worked would hide it.
    calls = {"n": 0}

    def call():
        calls["n"] += 1
        if calls["n"] < 3:
            raise httpx.ReadTimeout("read timed out")
        return _FakeResponse()

    download._request_json("thing", call, max_retries=5, trace=_trace(tmp_path))
    lines = _lines(tmp_path)
    assert len(lines) == 3                      # two failures + the success
    assert [x.get("status") for x in lines] == [None, None, 200]
    assert "ReadTimeout" in lines[0]["error"]


def test_trace_records_a_give_up(tmp_path):
    def call():
        raise httpx.ConnectTimeout("connect timed out")

    with pytest.raises(SystemExit):
        download._request_json("thing", call, max_retries=3,
                               trace=_trace(tmp_path))
    # Every attempt is on record, including the last one before the exit.
    assert len(_lines(tmp_path)) == 3


def test_trace_redacts_a_credential_query_param(tmp_path):
    resp = _FakeResponse(url=f"{URL}?access_token=SYNTHETIC_TOKEN")
    download._request_json("thing", lambda: resp, trace=_trace(tmp_path))
    assert "SYNTHETIC_TOKEN" not in json.dumps(_lines(tmp_path))


def test_request_url_falls_back_to_the_label(tmp_path):
    # An un-mapped httpcore fault carries no request, and httpx's own
    # `.request` raises when it was never set — neither may cost the line.
    assert download._request_url(httpcore.ReadTimeout("t"), "thing") == "thing"
    assert download._request_url(httpx.ReadTimeout("t"), "thing") == "thing"
    attached = httpx.ReadTimeout("t")
    attached.request = httpx.Request("GET", URL)
    assert download._request_url(attached, "thing") == URL


def test_configure_timeout_raises_read_window():
    class _FakeClient:
        def __init__(self):
            self.timeout = None

        def set_timeout(self, t):
            self.timeout = t

    c = _FakeClient()
    download.configure_timeout(c, 90.0)
    assert isinstance(c.timeout, httpx.Timeout)
    assert c.timeout.read == 90.0
    assert c.timeout.connect == 10.0  # connect stays short (fail fast)
