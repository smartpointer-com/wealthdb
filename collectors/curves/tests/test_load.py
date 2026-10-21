"""
Bronze->silver tests for the curves collector's load.py.

Seeds bronze runs (run.json manifest + each portal's CSV answer, in the
portals' real layout) and runs the real loader, asserting the silver
`rates` rows. The parsers are tested on the quirks of each answer. No
real data: invented values only.
"""

from __future__ import annotations

import json
import logging
import sqlite3
from datetime import datetime, timezone
from pathlib import Path

import pytest

import load as loader
from collectorkit import silver
from conftest import ecb_csv, snb_csv

RUN_SLUG = "20260105T030405Z"
LATER_SLUG = "20260106T030405Z"
EUR_10Y = "YC.B.U2.EUR.4F.G_N_A.SV_C_YM.SR_10Y"
EUR_3M = "YC.B.U2.EUR.4F.G_N_A.SV_C_YM.SR_3M"
CHF_10Y = "SNB1A.SNB.NSS.KZS.EID.J10M0"

EUR_10Y_MEANS = {"portal": "ecb", "currency": "EUR", "tenor": "10Y",
                 "basis": "spot", "compounding": "continuous"}
EUR_3M_MEANS = {**EUR_10Y_MEANS, "tenor": "3M"}
CHF_10Y_MEANS = {"portal": "snb", "currency": "CHF", "tenor": "10Y",
                 "basis": "spot", "compounding": "annual"}


def _epoch(d: str) -> int:
    return int(datetime.strptime(d, "%Y-%m-%d")
               .replace(tzinfo=timezone.utc).timestamp())


def _payload(means: dict) -> str:
    return json.dumps({k: means[k] for k in loader.RATE_MEANING},
                      separators=(",", ":"))


def _seed_run(root: Path, slug: str, *, ecb=None, snb=None,
              series=None, status="complete") -> Path:
    """One bronze run. ecb: [(series id, date, value)], snb: [(maturity
    item, date, value)]; series: {series id: manifest entry less rows}."""
    run = root / slug
    run.mkdir(parents=True)
    portals = {}
    if ecb is not None:
        (run / "ecb.csv").write_text(ecb_csv(ecb), encoding="utf-8")
        portals["ecb"] = {"url": "https://x.invalid/ecb", "file": "ecb.csv"}
    if snb is not None:
        (run / "snb.csv").write_text(snb_csv(snb), encoding="utf-8")
        portals["snb"] = {"url": "https://x.invalid/snb", "file": "snb.csv",
                          "published": "20260106_0830"}
    (run / "run.json").write_text(json.dumps({
        "slug": slug, "since": "2026-01-01", "until": "2026-01-06",
        "status": status, "portals": portals,
        "series": {sid: {**m, "rows": 0} for sid, m in (series or {}).items()},
    }), encoding="utf-8")
    return run


def _seed_both(root: Path) -> Path:
    return _seed_run(
        root, RUN_SLUG,
        ecb=[(EUR_10Y, "2026-01-05", "2.5000000001"),
             (EUR_10Y, "2026-01-06", "2.51"),
             (EUR_3M, "2026-01-05", "")],
        snb=[("J10M0", "2026-01-05", "0.5"), ("J10M0", "2026-01-06", "")],
        series={EUR_10Y: EUR_10Y_MEANS, EUR_3M: EUR_3M_MEANS,
                CHF_10Y: CHF_10Y_MEANS})


def _rates(db: Path) -> list[tuple]:
    conn = sqlite3.connect(db)
    try:
        return conn.execute("SELECT as_of, series_id, value, payload FROM "
                            "rates ORDER BY series_id, as_of").fetchall()
    finally:
        conn.close()


def _load(db: Path, bronze: Path, *extra: str) -> int:
    return loader.main(["--silver-db", str(db), "--bronze-dir", str(bronze),
                        *extra])


# --- the parsers --------------------------------------------------------------

def test_parse_ecb_keys_rows_by_the_whole_key():
    text = ecb_csv([(EUR_10Y, "2026-01-05", "2.5"), (EUR_3M, "2026-01-05", ""),
                    (EUR_10Y, "2026-01-06", "2.6")])
    assert loader.parse_ecb(text) == {
        EUR_10Y: [("2026-01-05", "2.5"), ("2026-01-06", "2.6")]}
    assert loader.parse_ecb("") == {}


def test_parse_ecb_skips_a_value_that_is_no_number(caplog):
    text = ecb_csv([(EUR_10Y, "2026-01-05", "NaN"), (EUR_10Y, "2026-01-06", "-"),
                    (EUR_10Y, "2026-01-07", "2.6")])
    with caplog.at_level(logging.WARNING, logger="curves.load"):
        assert loader.parse_ecb(text) == {EUR_10Y: [("2026-01-07", "2.6")]}
    assert "ecb: skipped 2 value(s) that are no number" in caplog.text


def test_parse_snb_skips_its_preamble_and_empty_days():
    text = snb_csv([("J10M0", "2026-01-05", "0.5"),
                    ("J01M6", "2026-01-05", ""),
                    ("J01M6", "2026-01-06", "-0.1")])
    assert text.startswith('\ufeff"CubeId";"SNB1A@SNB.NSS.KZS.EID"\r\n')
    assert loader.parse_snb(text) == {
        CHF_10Y: [("2026-01-05", "0.5")],
        "SNB1A.SNB.NSS.KZS.EID.J01M6": [("2026-01-06", "-0.1")]}


def test_parse_snb_skips_a_value_that_is_no_number(caplog):
    text = snb_csv([("J10M0", "2026-01-05", "n/a"), ("J10M0", "2026-01-06", "0.5")])
    with caplog.at_level(logging.WARNING, logger="curves.load"):
        assert loader.parse_snb(text) == {CHF_10Y: [("2026-01-06", "0.5")]}
    assert "snb: skipped 1 value(s) that are no number" in caplog.text


def test_parse_snb_reads_only_the_fixed_items():
    # A monthly row of the same maturity is another series of the cube.
    text = (snb_csv([("J10M0", "2026-01-05", "0.5")])
            + '"2025-12";"J10M0";"A1100";"P1M";"ZH";"0.4"\r\n')
    assert loader.parse_snb(text) == {CHF_10Y: [("2026-01-05", "0.5")]}


def test_parse_snb_of_an_answer_without_a_fixed_column():
    # An answer that leaves a one-item dimension out holds that item.
    text = ('\ufeff"CubeId";"SNB1A@SNB.NSS.KZS.EID"\r\n\r\n'
            '"Date";"LAUFZEIT";"Value"\r\n"2026-01-05";"J10M0";"0.5"\r\n')
    assert loader.parse_snb(text) == {CHF_10Y: [("2026-01-05", "0.5")]}


def test_parse_snb_refuses_another_cube():
    with pytest.raises(ValueError, match="SNB1A@SNB.NSS.KZS.PFI"):
        loader.parse_snb(snb_csv([("J10M0", "2026-01-05", "0.5")],
                                 cube="SNB1A@SNB.NSS.KZS.PFI"))


def test_parse_snb_of_a_sparse_history():
    # Before 1998 about one rate a week, and a long tenor with no rate on
    # some days: each series holds only its dated values.
    rows = []
    for week in range(104):
        day = f"{1990 + week // 52}-{1 + week % 52 // 5:02d}-{1 + week % 5 * 5:02d}"
        rows += [("J01M0", day, "3.1"), ("J30M0", day, "4.2" if week % 3 else "")]
    answer = loader.parse_snb(snb_csv(rows))
    assert len(answer[f"{loader.SNB_CUBE}.J01M0"]) == 104
    assert len(answer[f"{loader.SNB_CUBE}.J30M0"]) == 104 - 35


def test_parse_snb_of_no_table_is_empty():
    assert loader.parse_snb('\ufeff"CubeId";"SNB1A@SNB.NSS.KZS.EID"\r\n') == {}


# --- a load -------------------------------------------------------------------

def test_load_projects_both_portals_with_their_meaning(tmp_path):
    bronze = tmp_path / "bronze"
    _seed_both(bronze)
    db = tmp_path / "curves.db"
    assert _load(db, bronze) == 0
    assert _rates(db) == [
        (_epoch("2026-01-05"), CHF_10Y, "0.5", _payload(CHF_10Y_MEANS)),
        (_epoch("2026-01-05"), EUR_10Y, "2.5000000001", _payload(EUR_10Y_MEANS)),
        (_epoch("2026-01-06"), EUR_10Y, "2.51", _payload(EUR_10Y_MEANS)),
    ]
    assert _payload(EUR_10Y_MEANS) == (
        '{"currency":"EUR","tenor":"10Y","basis":"spot",'
        '"compounding":"continuous"}')


def test_load_is_idempotent_and_force_rebuilds(tmp_path):
    bronze = tmp_path / "bronze"
    _seed_both(bronze)
    db = tmp_path / "curves.db"
    _load(db, bronze)
    before = _rates(db)
    assert _load(db, bronze) == 0
    assert _rates(db) == before
    assert _load(db, bronze, "--force") == 0
    assert _rates(db) == before


def test_a_later_run_upserts_a_revised_day(tmp_path):
    bronze = tmp_path / "bronze"
    _seed_run(bronze, RUN_SLUG, ecb=[(EUR_10Y, "2026-01-05", "2.5")],
              series={EUR_10Y: EUR_10Y_MEANS})
    db = tmp_path / "curves.db"
    assert _load(db, bronze) == 0
    _seed_run(bronze, LATER_SLUG, ecb=[(EUR_10Y, "2026-01-05", "2.55"),
                                       (EUR_10Y, "2026-01-06", "2.6")],
              series={EUR_10Y: EUR_10Y_MEANS})
    assert _load(db, bronze) == 0
    assert [(r[0], r[2]) for r in _rates(db)] == [
        (_epoch("2026-01-05"), "2.55"), (_epoch("2026-01-06"), "2.6")]


def test_an_unchanged_snb_run_loads_its_other_portal(tmp_path):
    # The run left the SNB's unchanged publication unasked: no answer and
    # no franc series, while the euro's load as usual.
    bronze = tmp_path / "bronze"
    run = _seed_run(bronze, LATER_SLUG, ecb=[(EUR_10Y, "2026-01-06", "2.6")],
                    series={EUR_10Y: EUR_10Y_MEANS})
    manifest = json.loads((run / "run.json").read_text())
    manifest["portals"]["snb"] = {"published": "20260106_0830",
                                  "unchanged_since": RUN_SLUG}
    (run / "run.json").write_text(json.dumps(manifest))
    db = tmp_path / "curves.db"
    assert _load(db, bronze) == 0
    assert [r[1] for r in _rates(db)] == [EUR_10Y]


def test_an_in_progress_run_is_not_loaded_until_complete(tmp_path):
    bronze = tmp_path / "bronze"
    run = _seed_run(bronze, RUN_SLUG, ecb=[(EUR_10Y, "2026-01-05", "2.5")],
                    series={EUR_10Y: EUR_10Y_MEANS}, status="in-progress")
    db = tmp_path / "curves.db"
    assert _load(db, bronze) == 0
    assert _rates(db) == []
    manifest = json.loads((run / "run.json").read_text())
    manifest["status"] = "complete"
    (run / "run.json").write_text(json.dumps(manifest))
    assert _load(db, bronze) == 0
    assert len(_rates(db)) == 1


def test_an_entry_without_its_meaning_is_skipped(tmp_path, caplog):
    bronze = tmp_path / "bronze"
    means = {k: v for k, v in EUR_3M_MEANS.items() if k != "compounding"}
    _seed_run(bronze, RUN_SLUG, ecb=[(EUR_10Y, "2026-01-05", "2.5"),
                                     (EUR_3M, "2026-01-05", "2.0")],
              series={EUR_10Y: EUR_10Y_MEANS, EUR_3M: means})
    db = tmp_path / "curves.db"
    with caplog.at_level(logging.WARNING, logger="curves.load"):
        assert _load(db, bronze) == 0
    assert [r[1] for r in _rates(db)] == [EUR_10Y]
    assert f"series {EUR_3M} has no currency/tenor/basis/compounding" in caplog.text


def test_a_missing_answer_file_is_skipped(tmp_path, caplog):
    bronze = tmp_path / "bronze"
    run = _seed_both(bronze)
    (run / "snb.csv").unlink()
    db = tmp_path / "curves.db"
    with caplog.at_level(logging.WARNING, logger="curves.load"):
        assert _load(db, bronze) == 0
    assert {r[1] for r in _rates(db)} == {EUR_10Y}
    assert "answer file snb.csv missing" in caplog.text


def test_schema_meta_version(tmp_path):
    db = tmp_path / "curves.db"
    _load(db, tmp_path / "empty")
    conn = silver.open_db(db)
    try:
        assert silver.current_schema_version(conn) == 1
    finally:
        conn.close()
