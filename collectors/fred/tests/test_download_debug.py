"""Tests for `download.py --debug`, the bronze-resident HTTP trace.

The transport is stubbed — these assert what the trace records, not FRED's
behaviour. Two properties carry the weight:

  * the api_key NEVER reaches the trace file. It travels in FRED's query
    string (AGENTS.md §2), and repo AGENTS.md §3 forbids persisting a
    credential to disk at all.
  * a request that FAILED is traced too. A trace that only shows successes
    cannot explain the run it exists for.
"""
from __future__ import annotations

import json
import sys
import urllib.error
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import download  # noqa: E402

SECRET = "SYNTHETIC_KEY_NOT_A_REAL_ONE"


class _FakeResponse:
    """The slice of http.client.HTTPResponse fetch_observations touches."""

    def __init__(self, body: bytes, status: int = 200, headers=None):
        self._body = body
        self.status = status
        self.headers = headers or {"Content-Type": "application/json"}

    def read(self) -> bytes:
        return self._body

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False


def _obs_doc() -> bytes:
    return json.dumps({"observations": [
        {"date": "2020-01-02", "value": "0.9000"}]}).encode()


def _fake_urlopen(resp_or_exc):
    def _open(req, timeout=None):
        if isinstance(resp_or_exc, Exception):
            raise resp_or_exc
        return resp_or_exc
    return _open


def _run(monkeypatch, tmp_path, *extra, resp=None):
    monkeypatch.setattr(
        download.urllib.request, "urlopen",
        _fake_urlopen(_FakeResponse(_obs_doc()) if resp is None else resp))
    return download.main([
        "--bronze-dir", str(tmp_path), "--api-key", SECRET,
        "--series", "DEXSZUS", *extra,
    ])


def _trace_lines(tmp_path) -> list[dict]:
    hits = list(tmp_path.glob("*/screenshots/http-trace.jsonl"))
    if not hits:
        return []
    return [json.loads(x) for x in hits[0].read_text().splitlines()]


# --- the flag itself --------------------------------------------------------

def test_debug_parses_and_defaults_off():
    assert download.parse_args(["--bronze-dir", "/x"]).debug is False
    assert download.parse_args(["--bronze-dir", "/x", "--debug"]).debug is True


def test_without_debug_no_capture_dir_is_written(monkeypatch, tmp_path):
    assert _run(monkeypatch, tmp_path) == 0
    assert not list(tmp_path.glob("*/screenshots"))


# --- what the trace records -------------------------------------------------

def test_debug_traces_a_request_and_redacts_the_api_key(monkeypatch, tmp_path):
    assert _run(monkeypatch, tmp_path, "--debug") == 0
    lines = _trace_lines(tmp_path)
    assert len(lines) == 1
    entry = lines[0]
    assert entry["method"] == "GET"
    assert entry["status"] == 200
    assert entry["bytes"] == len(_obs_doc())
    # The credential must not survive anywhere in the file; that the request
    # carried an api_key at all is the diagnostic worth keeping.
    assert SECRET not in json.dumps(lines)
    assert "api_key=" in entry["url"]
    assert "series_id=DEXSZUS" in entry["url"]


def test_debug_keeps_only_whitelisted_response_headers(monkeypatch, tmp_path):
    # A throttled FRED answers with Retry-After; that header is the whole
    # reason a trace beats the observations documents beside it.
    resp = _FakeResponse(_obs_doc(), headers={
        "Content-Type": "application/json",
        "Retry-After": "30",
        "Set-Cookie": "session=SYNTHETIC_COOKIE",
    })
    assert _run(monkeypatch, tmp_path, "--debug", resp=resp) == 0
    entry = _trace_lines(tmp_path)[0]
    assert entry["headers"]["retry-after"] == "30"
    assert "SYNTHETIC_COOKIE" not in json.dumps(entry)


def test_debug_traces_an_http_error(monkeypatch, tmp_path):
    # The series is skipped, but the exchange that skipped it is exactly
    # what --debug exists to explain.
    err = urllib.error.HTTPError(
        url="https://api.stlouisfed.org/fred/series/observations",
        code=429, msg="Too Many Requests", hdrs=None, fp=None)
    assert _run(monkeypatch, tmp_path, "--debug", resp=err) == 5
    entry = _trace_lines(tmp_path)[0]
    assert entry["status"] == 429
    assert "Too Many Requests" in entry["error"]
    assert SECRET not in json.dumps(entry)


def test_debug_traces_a_transport_fault(monkeypatch, tmp_path):
    # Nothing came back, so there is no status to record — only that the
    # attempt was made and how it died.
    assert _run(monkeypatch, tmp_path, "--debug",
                resp=urllib.error.URLError("no route to host")) == 5
    entry = _trace_lines(tmp_path)[0]
    assert "status" not in entry
    assert "no route to host" in entry["error"]


# --- --dry-run --------------------------------------------------------------

def test_dry_run_with_debug_writes_no_bronze(monkeypatch, tmp_path):
    # A dry-run creates no run dir, so the trace has nowhere to land:
    # HttpTrace(None) swallows the records rather than crashing on a
    # missing dir, and --bronze-dir stays empty.
    assert _run(monkeypatch, tmp_path, "--debug", "--dry-run") == 0
    assert not list(tmp_path.iterdir())
