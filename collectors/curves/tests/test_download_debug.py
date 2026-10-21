"""Tests for `download.py --debug`, the bronze-resident HTTP trace.

The transport is stubbed. The trace records every request of the run,
the latest-update probe and a failed request included, and nothing is
captured without --debug.
"""
from __future__ import annotations

import json

import download
from conftest import FakePortals


def _trace(tmp_path) -> list[dict]:
    hits = list(tmp_path.glob("*/screenshots/http-trace.jsonl"))
    return [json.loads(x) for x in hits[0].read_text().splitlines()] if hits else []


def test_without_debug_nothing_is_captured(monkeypatch, tmp_path):
    monkeypatch.setattr(download.urllib.request, "urlopen", FakePortals())
    assert download.main(["--bronze-dir", str(tmp_path)]) == 0
    assert _trace(tmp_path) == []


def test_debug_traces_every_request_a_failed_one_too(monkeypatch, tmp_path):
    monkeypatch.setattr(download.urllib.request, "urlopen",
                        FakePortals(failing={"ecb"}))
    assert download.main(["--bronze-dir", str(tmp_path), "--debug"]) == 0
    lines = _trace(tmp_path)
    assert [line.get("status") for line in lines] == [500, 200, 200]
    assert "data-api.ecb.europa.eu" in lines[0]["url"]
    assert lines[1]["url"].endswith("/lastUpdate")
    assert all("body" not in line for line in lines)
