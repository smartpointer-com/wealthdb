"""Tests for a `download.py` run against fake portals.

The transport is stubbed (conftest.FakePortals): each portal answers with
invented values in its real layout, or fails when told to, and records
what it was asked. These assert the series maps, what a run asks each
portal and writes to bronze, that load reads it back, and when a run
leaves the SNB's data unasked. No network, no real data.
"""
from __future__ import annotations

import json
import re
import sqlite3
import urllib.parse
from datetime import date
from pathlib import Path

import pytest

import download
import load
from conftest import FakePortals, ecb_csv, snb_csv

ECB_HOST, SNB_HOST = "data-api.ecb.europa.eu", "data.snb.ch"
EUR_10Y = "YC.B.U2.EUR.4F.G_N_A.SV_C_YM.SR_10Y"
CHF_10Y = "SNB1A.SNB.NSS.KZS.EID.J10M0"


def _download(bronze: Path, *extra: str) -> int:
    return download.main(["--bronze-dir", str(bronze), *extra])


def _runs(bronze: Path) -> list[Path]:
    return sorted(p for p in bronze.iterdir() if p.is_dir())


def _manifest(run: Path) -> dict:
    return json.loads((run / "run.json").read_text())


def _of(currency: str) -> dict[str, tuple]:
    return {sid: m for sid, m in download.RATE_SERIES.items()
            if m[1] == currency}


# --- the series maps ----------------------------------------------------------

def test_the_euro_curve_is_the_ecbs_aaa_spot_curve_3m_to_30y():
    eur = _of("EUR")
    assert [m[2] for m in eur.values()] == [
        "3M", "6M", "9M", *(f"{y}Y" for y in range(1, 31))]
    assert eur[EUR_10Y] == ("ecb", "EUR", "10Y", "spot", "continuous")
    assert all(sid.startswith("YC.B.U2.EUR.4F.G_N_A.SV_C_YM.SR_") for sid in eur)


def test_the_franc_curve_is_the_snbs_spot_curve_1y_to_30y_by_half_year():
    chf = _of("CHF")
    assert len(chf) == 59
    assert chf[CHF_10Y] == ("snb", "CHF", "10Y", "spot", "annual")
    assert chf["SNB1A.SNB.NSS.KZS.EID.J01M6"][2] == "18M"
    assert chf["SNB1A.SNB.NSS.KZS.EID.J01M0"][2] == "1Y"
    assert chf["SNB1A.SNB.NSS.KZS.EID.J30M0"][2] == "30Y"
    assert chf["SNB1A.SNB.NSS.KZS.EID.J29M6"][2] == "354M"


def test_every_series_is_asked_under_its_portals_key():
    # ecb_url and snb_url ask each portal under one key.
    for sid, (portal, *_) in download.RATE_SERIES.items():
        prefix = {"ecb": f"{download.ECB_FLOW}.{download.ECB_KEY}.",
                  "snb": f"{download.SNB_CUBE}."}[portal]
        assert sid.startswith(prefix) and "." not in sid[len(prefix):], sid


def test_every_tenor_is_one_the_config_reads_and_unique_per_currency():
    # The config's tenor is a whole number and M or Y, at most 50 years
    # (canonical.TenorMonths).
    for currency in ("EUR", "CHF"):
        months = []
        for _, _, tenor, _, _ in _of(currency).values():
            assert re.fullmatch(r"[1-9][0-9]{0,2}[MY]", tenor), tenor
            months.append(int(tenor[:-1]) * (12 if tenor[-1] == "Y" else 1))
        assert len(months) == len(set(months)), currency
        assert max(months) <= 600


# --- a run --------------------------------------------------------------------

def test_a_run_asks_each_portal_once_and_keeps_its_answer(portals, tmp_path):
    assert _download(tmp_path, "--lookback", "2026-01-01") == 0
    (ecb,) = portals.requested(ECB_HOST)
    keys = ecb.path.rsplit(".", 1)[1].split("+")
    assert keys == [f"SR_{t}" for t in download.ECB_TENORS]
    query = urllib.parse.parse_qs(ecb.query)
    assert query["startPeriod"] == ["2026-01-01"]
    assert query["format"] == ["csvdata"] and query["detail"] == ["dataonly"]

    # The SNB: its latest update first, then the data of every maturity
    # with each fixed dimension at its one item.
    last, data = portals.requested(SNB_HOST)
    assert last.path == "/api/warehouse/cube/SNB1A.SNB.NSS.KZS.EID/lastUpdate"
    assert data.path == "/api/warehouse/cube/SNB1A.SNB.NSS.KZS.EID/data/csv/en"
    dims = urllib.parse.parse_qs(data.query)["dimSel"][0]
    assert dims.startswith("LAUFZEIT(J01M0,J01M6,J02M0,")
    assert dims.endswith(",J30M0),ZEITPUNKT(A1100),frequency(P1D_L),"
                         "AGGREGATIONSMETHODE(ZZ)")
    assert urllib.parse.parse_qs(data.query)["fromDate"] == ["2026-01-01"]

    (run,) = _runs(tmp_path)
    assert sorted(p.name for p in run.iterdir()) == ["ecb.csv", "run.json",
                                                     "snb.csv"]
    manifest = _manifest(run)
    assert manifest["status"] == "complete"
    assert manifest["portals"]["snb"]["published"] == "20260106_0830"
    assert manifest["portals"]["ecb"]["file"] == "ecb.csv"
    assert sorted(manifest["series"]) == sorted(download.RATE_SERIES)
    assert manifest["series"][EUR_10Y] == {
        "portal": "ecb", "currency": "EUR", "tenor": "10Y", "basis": "spot",
        "compounding": "continuous", "rows": 2}
    assert manifest["series"][CHF_10Y]["rows"] == 2


def test_series_takes_ids_and_currencies(portals, tmp_path):
    assert _download(tmp_path, "--series", f"CHF, {EUR_10Y}") == 0
    (ecb,) = portals.requested(ECB_HOST)
    assert ecb.path.endswith(".SV_C_YM.SR_10Y")
    series = _manifest(_runs(tmp_path)[0])["series"]
    assert set(series) == {EUR_10Y, *_of("CHF")}


def test_one_currency_asks_one_portal(portals, tmp_path):
    assert _download(tmp_path, "--series", "EUR") == 0
    assert portals.requested(SNB_HOST) == []
    assert set(_manifest(_runs(tmp_path)[0])["portals"]) == {"ecb"}


def test_an_unknown_series_is_refused_before_any_request(portals, tmp_path):
    with pytest.raises(SystemExit) as caught:
        _download(tmp_path, "--series", "EUR,EXMP")
    assert "EXMP" in str(caught.value) and "RATE_SERIES" in str(caught.value)
    assert portals.urls == []
    assert not list(tmp_path.iterdir())


def test_a_downloaded_run_loads_with_each_series_meaning(portals, tmp_path):
    bronze, db = tmp_path / "bronze", tmp_path / "curves.db"
    assert _download(bronze, "--series", f"{EUR_10Y},{CHF_10Y}") == 0
    assert load.main(["--silver-db", str(db), "--bronze-dir", str(bronze)]) == 0
    conn = sqlite3.connect(db)
    try:
        rows = conn.execute("SELECT series_id, value, payload FROM rates "
                            "ORDER BY series_id, as_of").fetchall()
    finally:
        conn.close()
    chf = '{"currency":"CHF","tenor":"10Y","basis":"spot","compounding":"annual"}'
    eur = '{"currency":"EUR","tenor":"10Y","basis":"spot","compounding":"continuous"}'
    assert rows == [(CHF_10Y, "0.5", chf), (CHF_10Y, "0.5", chf),
                    (EUR_10Y, "2.5", eur), (EUR_10Y, "2.5", eur)]


def test_a_failed_portal_is_left_out_and_the_run_completes(monkeypatch,
                                                           tmp_path):
    portals = FakePortals(failing={"ecb"})
    monkeypatch.setattr(download.urllib.request, "urlopen", portals)
    assert _download(tmp_path) == 0
    run = _runs(tmp_path)[0]
    manifest = _manifest(run)
    assert manifest["status"] == "complete"
    assert set(manifest["portals"]) == {"snb"}
    assert set(manifest["series"]) == set(_of("CHF"))
    assert not (run / "ecb.csv").exists()


def test_a_run_with_no_answer_exits_nonzero(monkeypatch, tmp_path):
    monkeypatch.setattr(download.urllib.request, "urlopen",
                        FakePortals(failing={"ecb", "snb"}))
    assert _download(tmp_path) == 5
    assert _manifest(_runs(tmp_path)[0])["series"] == {}


def test_an_empty_ecb_window_has_no_rows(monkeypatch, tmp_path):
    # A window of no business day: the ECB answers 200 with no body.
    monkeypatch.setattr(download.urllib.request, "urlopen",
                        FakePortals(bodies={"ecb": b""}))
    assert _download(tmp_path, "--series", "EUR") == 0
    series = _manifest(_runs(tmp_path)[0])["series"]
    assert {e["rows"] for e in series.values()} == {0}


def test_a_not_found_ecb_window_has_no_rows(monkeypatch, tmp_path):
    # SDMX's answer to a window with no observation.
    monkeypatch.setattr(download.urllib.request, "urlopen",
                        FakePortals(failing={"ecb"}, code=404))
    assert _download(tmp_path, "--series", "EUR") == 0
    run = _runs(tmp_path)[0]
    assert {e["rows"] for e in _manifest(run)["series"].values()} == {0}
    assert (run / "ecb.csv").read_bytes() == b""


def test_a_dry_run_asks_and_writes_nothing(portals, tmp_path):
    assert _download(tmp_path, "--dry-run") == 0
    assert len(portals.requested(ECB_HOST)) == 1
    assert len(portals.requested(SNB_HOST)) == 2
    assert not list(tmp_path.iterdir())


def test_a_dry_run_makes_no_bronze_dir(portals, tmp_path):
    assert _download(tmp_path / "bronze", "--dry-run") == 0
    assert not (tmp_path / "bronze").exists()


def test_a_parse_error_fails_its_portal_alone(monkeypatch, tmp_path):
    # An ECB answer without its KEY column, and an SNB answer for another
    # cube: each portal is skipped, the other completes the run.
    for portal, body in (("ecb", b"TIME_PERIOD,OBS_VALUE\r\n2026-01-05,2.5\r\n"),
                         ("snb", snb_csv([("J10M0", "2026-01-05", "0.5")],
                                         cube="SNB1A@SNB.NSS.KZS.PFI").encode())):
        bronze = tmp_path / portal
        monkeypatch.setattr(download.urllib.request, "urlopen",
                            FakePortals(bodies={portal: body}))
        assert _download(bronze) == 0
        manifest = _manifest(_runs(bronze)[0])
        assert manifest["status"] == "complete"
        assert set(manifest["portals"]) == {"ecb", "snb"} - {portal}


def test_a_value_that_is_no_number_is_left_out(monkeypatch, tmp_path):
    body = ecb_csv([(EUR_10Y, "2026-01-05", "NaN"), (EUR_10Y, "2026-01-06", "2.5")])
    monkeypatch.setattr(download.urllib.request, "urlopen",
                        FakePortals(bodies={"ecb": body.encode()}))
    assert _download(tmp_path, "--series", EUR_10Y) == 0
    assert _manifest(_runs(tmp_path)[0])["series"][EUR_10Y]["rows"] == 1


# --- the SNB's latest update --------------------------------------------------

def _a_first_run(tmp_path, portals, *series):
    """One complete run of the series from 2026-01-01, its dir renamed to
    sort before the next run's."""
    assert _download(tmp_path, "--lookback", "2026-01-01",
                     "--series", ",".join(series)) == 0
    _runs(tmp_path)[0].rename(tmp_path / "20260101T000000Z")
    portals.urls.clear()


def test_an_unchanged_publication_is_not_asked_again(portals, tmp_path):
    _a_first_run(tmp_path, portals, "EUR", "CHF")
    assert _download(tmp_path, "--lookback", "2026-01-01",
                     "--series", "CHF") == 0
    (last,) = portals.requested(SNB_HOST)
    assert last.path.endswith("/lastUpdate")
    second = _runs(tmp_path)[-1]
    manifest = _manifest(second)
    assert manifest["portals"]["snb"] == {
        "published": "20260106_0830", "unchanged_since": "20260101T000000Z"}
    assert manifest["series"] == {}
    assert not (second / "snb.csv").exists()


def test_a_new_publication_is_asked(portals, tmp_path):
    _a_first_run(tmp_path, portals, CHF_10Y)
    portals.published = "20260107_0830"
    assert _download(tmp_path, "--lookback", "2026-01-01",
                     "--series", CHF_10Y) == 0
    assert len(portals.requested(SNB_HOST)) == 2


def test_a_wider_window_is_asked(portals, tmp_path):
    _a_first_run(tmp_path, portals, CHF_10Y)
    assert _download(tmp_path, "--lookback", "2025-12-01",
                     "--series", CHF_10Y) == 0
    assert len(portals.requested(SNB_HOST)) == 2


def test_more_series_than_a_run_holds_are_asked(portals, tmp_path):
    _a_first_run(tmp_path, portals, CHF_10Y)
    assert _download(tmp_path, "--lookback", "2026-01-01",
                     "--series", "CHF") == 0
    assert len(portals.requested(SNB_HOST)) == 2


def test_snb_held_reads_only_complete_runs_that_fetched(tmp_path):
    def run(slug, status, file="snb.csv", rows=2):
        # A run that answered with no row (an answer the parser did not
        # read) holds nothing.
        d = tmp_path / slug
        d.mkdir()
        (d / "run.json").write_text(json.dumps({
            "since": "2026-01-01", "status": status,
            "portals": {"snb": {"published": "P", "file": file}},
            "series": {CHF_10Y: {"portal": "snb", "rows": rows}}}))

    run("20260101T000000Z", "complete")
    run("20260102T000000Z", "in-progress")
    run("20260103T000000Z", "complete", file=None)
    run("20260104T000000Z", "complete", rows=0)
    held = download.snb_held(tmp_path, "P", date(2026, 1, 1), [CHF_10Y])
    assert held == "20260101T000000Z"
    assert download.snb_held(tmp_path, "Q", date(2026, 1, 1), [CHF_10Y]) is None
    assert download.snb_held(tmp_path, "P", date(2025, 12, 31), [CHF_10Y]) is None


def test_a_latest_update_without_its_date_fails_the_snb_alone(monkeypatch,
                                                              tmp_path):
    portals = FakePortals(published="")
    monkeypatch.setattr(download.urllib.request, "urlopen", portals)
    assert _download(tmp_path) == 0
    manifest = _manifest(_runs(tmp_path)[0])
    assert set(manifest["portals"]) == {"ecb"}
    assert len(portals.requested(SNB_HOST)) == 1
