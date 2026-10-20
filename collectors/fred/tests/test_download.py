"""Tests for a `download.py` run against a fake FRED.

The transport is stubbed: a fake urlopen answers every request with an
invented observations document, or fails the series it is told to, and
records which series was asked for. These assert what a run writes to
bronze, per family, that load reads it back, and which --base-url values
a run refuses. No network, no real data — round synthetic values only.
"""
from __future__ import annotations

import io
import json
import logging
import sqlite3
import urllib.error
import urllib.parse
from pathlib import Path

import pytest

import download
import load
from conftest import FakeResponse

API_KEY = "SYNTHETIC_KEY"

# Two dated values around a '.' holiday sentinel, which no count includes.
OBSERVATIONS = {"observations": [
    {"date": "2020-01-01", "value": "."},
    {"date": "2020-01-02", "value": "1.50"},
    {"date": "2020-01-03", "value": "1.60"},
]}


class _FakeFred:
    """A urlopen stand-in: answers each request with OBSERVATIONS, or with
    an HTTP 500 for a series in `failing`, and records the series_id and
    host it was asked for, in request order."""

    def __init__(self, failing=()):
        self.failing = set(failing)
        self.requested: list[str] = []
        self.hosts: list[str] = []

    def __call__(self, req, timeout=None):
        url = urllib.parse.urlsplit(req.full_url)
        sid = urllib.parse.parse_qs(url.query)["series_id"][0]
        self.requested.append(sid)
        self.hosts.append(url.netloc)
        if sid in self.failing:
            raise urllib.error.HTTPError(
                url="https://api.example.invalid/fred/series/observations",
                code=500, msg="Internal Server Error", hdrs=None,
                fp=io.BytesIO(b'{"error_message":"synthetic"}'))
        return FakeResponse(json.dumps(OBSERVATIONS).encode())


@pytest.fixture
def fred(monkeypatch) -> _FakeFred:
    fake = _FakeFred()
    monkeypatch.setattr(download.urllib.request, "urlopen", fake)
    return fake


def _download(bronze: Path, *extra: str) -> int:
    return download.main(["--bronze-dir", str(bronze), "--api-key", API_KEY,
                          *extra])


def _run_dir(bronze: Path) -> Path:
    (run,) = bronze.iterdir()
    return run


def _manifest(bronze: Path) -> dict:
    return json.loads((_run_dir(bronze) / "run.json").read_text())


def test_the_two_maps_share_no_series_id():
    # A series in both maps would have two meanings and two tables.
    assert not set(download.FX_SERIES) & set(download.RATE_SERIES)


def test_a_run_fetches_every_series_of_both_families(fred, tmp_path):
    assert _download(tmp_path) == 0
    everything = [*download.FX_SERIES, *download.RATE_SERIES]
    assert fred.requested == everything

    run = _run_dir(tmp_path)
    assert sorted(p.name for p in run.iterdir()) == sorted(
        ["run.json", *(f"{sid}.json" for sid in everything)])
    manifest = _manifest(tmp_path)
    assert manifest["status"] == "complete"
    assert list(manifest["series"]) == sorted(everything)


def test_an_fx_entry_keeps_its_shape(fred, tmp_path):
    assert _download(tmp_path) == 0
    series = _manifest(tmp_path)["series"]
    for sid, (base, quote) in download.FX_SERIES.items():
        assert series[sid] == {"base": base, "quote": quote, "rows": 2}, sid


def test_a_rates_entry_names_its_family_and_meaning(fred, tmp_path):
    assert _download(tmp_path) == 0
    series = _manifest(tmp_path)["series"]
    for sid, (currency, tenor, basis) in download.RATE_SERIES.items():
        assert series[sid] == {"family": "rates", "currency": currency,
                               "tenor": tenor, "basis": basis,
                               "rows": 2}, sid
    assert series["DGS10"]["tenor"] == "10Y"
    assert series["DTB3"]["basis"] == "discount"


def test_series_may_mix_the_families(fred, tmp_path):
    assert _download(tmp_path, "--series", "DGS10, DEXSZUS") == 0
    assert fred.requested == ["DGS10", "DEXSZUS"]
    series = _manifest(tmp_path)["series"]
    assert set(series) == {"DGS10", "DEXSZUS"}
    assert series["DGS10"]["family"] == "rates"
    assert "family" not in series["DEXSZUS"]


def test_an_unknown_series_is_refused_naming_both_maps(fred, tmp_path):
    with pytest.raises(SystemExit) as caught:
        _download(tmp_path, "--series", "DGS10,EXMP")
    message = str(caught.value)
    assert "EXMP" in message
    assert "FX_SERIES" in message and "RATE_SERIES" in message
    assert fred.requested == []
    assert not list(tmp_path.iterdir())


def test_a_downloaded_run_loads_into_both_tables(fred, tmp_path):
    bronze, db = tmp_path / "bronze", tmp_path / "fred.db"
    assert _download(bronze, "--series", "DEXSZUS,DGS2,DTB3") == 0
    assert load.main(["--silver-db", str(db), "--bronze-dir", str(bronze)]) == 0

    conn = sqlite3.connect(db)
    try:
        fx = conn.execute("SELECT base_currency_iso, quote_currency_iso, mid "
                          "FROM fx_rates ORDER BY snapshot_at").fetchall()
        rates = conn.execute("SELECT series_id, value, payload FROM rates "
                             "ORDER BY series_id, as_of").fetchall()
    finally:
        conn.close()
    assert fx == [("CHF", "USD", "1.50"), ("CHF", "USD", "1.60")]
    two_year = '{"currency":"USD","tenor":"2Y","basis":"constant_maturity"}'
    bill = '{"currency":"USD","tenor":"3M","basis":"discount"}'
    assert rates == [("DGS2", "1.50", two_year), ("DGS2", "1.60", two_year),
                     ("DTB3", "1.50", bill), ("DTB3", "1.60", bill)]


def test_a_failed_series_is_left_out_and_the_run_completes(monkeypatch,
                                                           tmp_path):
    # One bad series must not abort the run. Every other series is
    # fetched, written and recorded, and the manifest is complete.
    fred = _FakeFred(failing={"DGS10"})
    monkeypatch.setattr(download.urllib.request, "urlopen", fred)
    assert _download(tmp_path) == 0
    everything = [*download.FX_SERIES, *download.RATE_SERIES]
    assert fred.requested == everything

    rest = [sid for sid in everything if sid != "DGS10"]
    manifest = _manifest(tmp_path)
    assert manifest["status"] == "complete"
    assert list(manifest["series"]) == sorted(rest)
    assert sorted(p.name for p in _run_dir(tmp_path).iterdir()) == sorted(
        ["run.json", *(f"{sid}.json" for sid in rest)])


# --- --base-url ---------------------------------------------------------------

def test_base_url_points_the_run_at_another_endpoint(fred, tmp_path):
    endpoint = "http://127.0.0.1:8080/fred/series/observations"
    assert _download(tmp_path, "--series", "DGS10",
                     "--base-url", endpoint) == 0
    assert fred.hosts == ["127.0.0.1:8080"]
    assert _manifest(tmp_path)["base_url"] == endpoint


@pytest.mark.parametrize("base_url", [
    "api.example.invalid/x",                # no scheme
    "ftp://api.example.invalid/x",          # not http(s)
    "https:///x",                           # no host
    "https://:443/x",                       # a port but no host
    "https://api.example.invalid:port/x",   # a malformed port
    "https://[::1/x",                       # an unbalanced IPv6 bracket
    "https://api.example.invalid/a b",      # a space
    "https://api.example.invalid/x\n",      # a control character
])
def test_a_malformed_base_url_is_refused_without_showing_the_key(
        fred, tmp_path, capsys, caplog, base_url):
    # Some of these make urllib quote the whole request URL, api_key
    # included, in its error. Each one is refused at argument parsing,
    # before any request, and the refusal never echoes the URL.
    with caplog.at_level(logging.DEBUG), pytest.raises(SystemExit) as caught:
        _download(tmp_path, "--base-url", base_url)
    assert caught.value.code != 0
    assert fred.requested == []
    assert not list(tmp_path.iterdir())
    out = capsys.readouterr()
    assert "--base-url" in out.err
    assert API_KEY not in out.out + out.err + caplog.text
    assert "api.example.invalid" not in out.err
