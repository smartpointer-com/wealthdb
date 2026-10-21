"""Tests for `download.py --check`, the probe behind `curves login
--check`.

No portal has a session or a key, so the probe asks each one for its
cheapest answer: the ECB for one observation, the SNB for its latest
update, since the SNB asks clients not to poll its data. The transport
is stubbed: these assert what the probe asks and how it maps the answers
to its exit code.
"""
from __future__ import annotations

import urllib.parse

import pytest

import download
from conftest import FakePortals


def test_check_needs_no_bronze_dir():
    args = download.parse_args(["--check"])
    assert args.check is True and args.bronze_dir is None


def test_a_bare_download_still_requires_a_bronze_dir():
    with pytest.raises(SystemExit):
        download.main([])


def test_check_asks_the_ecb_for_one_observation_and_the_snb_its_update(
        monkeypatch, tmp_path):
    monkeypatch.chdir(tmp_path)
    portals = FakePortals()
    monkeypatch.setattr(download.urllib.request, "urlopen", portals)
    assert download.check() == 0
    ecb, snb = map(urllib.parse.urlsplit, portals.urls)
    assert ecb.path.endswith(".SV_C_YM.SR_10Y")
    assert urllib.parse.parse_qs(ecb.query)["lastNObservations"] == ["1"]
    assert snb.path == "/api/warehouse/cube/SNB1A.SNB.NSS.KZS.EID/lastUpdate"
    assert not list(tmp_path.iterdir())


def test_a_failing_portal_fails_the_check(monkeypatch):
    for failing in ("ecb", "snb"):
        portals = FakePortals(failing={failing})
        monkeypatch.setattr(download.urllib.request, "urlopen", portals)
        assert download.check() == 1
        assert len(portals.urls) == 2   # the other portal is still asked


@pytest.mark.parametrize("bodies", [
    {"ecb": b""},                              # no observation
    {"ecb": b"TIME_PERIOD,OBS_VALUE\r\n2026-01-05,2.5\r\n"},  # no KEY column
])
def test_an_ecb_answer_without_an_observation_fails_the_check(monkeypatch,
                                                               bodies):
    monkeypatch.setattr(download.urllib.request, "urlopen",
                        FakePortals(bodies=bodies))
    assert download.check() == 1


def test_an_snb_update_without_its_date_fails_the_check(monkeypatch):
    monkeypatch.setattr(download.urllib.request, "urlopen",
                        FakePortals(published=""))
    assert download.check() == 1


def test_main_check_writes_nothing(monkeypatch, tmp_path):
    monkeypatch.chdir(tmp_path)
    monkeypatch.setattr(download.urllib.request, "urlopen", FakePortals())
    assert download.main(["--check"]) == 0
    assert not list(tmp_path.iterdir())
