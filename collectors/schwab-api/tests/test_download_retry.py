"""Unit tests for download.py's transient-retry + timeout wiring.

No network: the schwab-py request callable is a stub that raises or
returns a fake HTTPX response. `time.sleep` is patched out so the
exponential backoff doesn't slow the suite. Synthetic values only.
"""
from __future__ import annotations

import sys
from pathlib import Path

import httpcore
import httpx
import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import download  # noqa: E402


class _FakeResponse:
    def __init__(self, status_code=200, payload=None, text=""):
        self.status_code = status_code
        self._payload = {"ok": True} if payload is None else payload
        self.text = text

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
    # The failure the user actually hit: httpcore.ReadTimeout leaking
    # un-mapped past httpx. Must still be treated as retryable.
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
