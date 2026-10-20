"""
Bronze->silver tests for the fred collector's load.py.

Seeds synthetic FRED bronze runs (run.json manifest + per-series
observation JSON, mirroring the real API shape) and runs the real loader,
asserting the projected silver `fx_rates` and `rates` rows. No real data —
round synthetic values only.
"""

from __future__ import annotations

import json
import logging
import sqlite3
from datetime import datetime, timezone
from pathlib import Path

import load as loader
from collectorkit import silver

RUN_SLUG = "20240102T030405Z"
LATER_SLUG = "20240109T030405Z"


def _epoch(d: str) -> int:
    return int(datetime.strptime(d, "%Y-%m-%d")
               .replace(tzinfo=timezone.utc).timestamp())


def _obs_doc(rows):
    return {"observations": [{"date": d, "value": v} for d, v in rows]}


def _seed_bronze(root: Path) -> Path:
    """One bronze run: DEXSZUS (CHF/USD) + DEXUSEU (USD/EUR) in the
    canonical convention — (base, quote, mid) means '1 quote = mid base' —
    with a '.' holiday sentinel that must be skipped. Its manifest holds
    FX entries only, which carry no family."""
    run = root / RUN_SLUG
    run.mkdir(parents=True)
    (run / "run.json").write_text(json.dumps({
        "slug": RUN_SLUG,
        "since": "2020-01-01", "until": "2020-01-03",
        "series": {
            "DEXSZUS": {"base": "CHF", "quote": "USD", "rows": 2},
            "DEXUSEU": {"base": "USD", "quote": "EUR", "rows": 2},
        },
    }), encoding="utf-8")
    (run / "DEXSZUS.json").write_text(json.dumps(_obs_doc([
        ("2020-01-01", "."),          # New Year holiday -> skipped
        ("2020-01-02", "0.9000"),     # 1 USD = 0.90 CHF
        ("2020-01-03", "0.9100"),
    ])), encoding="utf-8")
    (run / "DEXUSEU.json").write_text(json.dumps(_obs_doc([
        ("2020-01-02", "1.1200"),     # 1 EUR = 1.12 USD
        ("2020-01-03", "1.1300"),
    ])), encoding="utf-8")
    return run


# The manifest entries download.py writes for two rates series.
DGS10 = {"family": "rates", "currency": "USD", "tenor": "10Y",
         "basis": "constant_maturity"}
DTB3 = {"family": "rates", "currency": "USD", "tenor": "3M",
        "basis": "discount"}


def _seed_run(root: Path, slug: str, series: dict) -> Path:
    """One complete bronze run: {series_id: (manifest entry without its
    row count, [(date, value), ...])}."""
    run = root / slug
    run.mkdir(parents=True)
    (run / "run.json").write_text(json.dumps({
        "slug": slug,
        "since": "2020-01-01", "until": "2020-01-03",
        "status": "complete",
        "series": {sid: {**entry, "rows": len(obs)}
                   for sid, (entry, obs) in series.items()},
    }), encoding="utf-8")
    for sid, (_, obs) in series.items():
        (run / f"{sid}.json").write_text(json.dumps(_obs_doc(obs)),
                                         encoding="utf-8")
    return run


def _seed_both_families(root: Path) -> Path:
    """One bronze run holding an FX series and two rates series, as a
    default download writes it."""
    return _seed_run(root, RUN_SLUG, {
        "DEXSZUS": ({"base": "CHF", "quote": "USD"}, [
            ("2020-01-02", "0.9000"),
        ]),
        "DGS10": (DGS10, [
            ("2020-01-01", "."),       # New Year holiday -> skipped
            ("2020-01-02", "1.80"),
            ("2020-01-03", "1.90"),
        ]),
        "DTB3": (DTB3, [
            ("2020-01-02", "1.50"),
            ("2020-01-03", ""),        # no-data sentinel -> skipped
        ]),
    })


def _select(db: Path, sql: str) -> list[dict]:
    conn = sqlite3.connect(db)
    conn.row_factory = sqlite3.Row
    try:
        return [dict(r) for r in conn.execute(sql)]
    finally:
        conn.close()


def _rows(db: Path) -> list[dict]:
    return _select(db, "SELECT snapshot_at, base_currency_iso, "
                       "quote_currency_iso, mid FROM fx_rates "
                       "ORDER BY base_currency_iso, snapshot_at")


def _rate_rows(db: Path) -> list[dict]:
    return _select(db, "SELECT as_of, series_id, value, payload FROM rates "
                       "ORDER BY series_id, as_of")


def _load(db: Path, bronze: Path, *extra: str) -> int:
    return loader.main(["--silver-db", str(db), "--bronze-dir", str(bronze),
                        *extra])


def test_load_projects_fx_rates(tmp_path):
    bronze = tmp_path / "bronze"
    _seed_bronze(bronze)
    db = tmp_path / "fred.db"
    assert _load(db, bronze) == 0

    rows = _rows(db)
    # 2 dated DEXSZUS + 2 DEXUSEU; the '.' row is skipped.
    assert len(rows) == 4
    chf = [r for r in rows if r["base_currency_iso"] == "CHF"]
    assert {r["quote_currency_iso"] for r in chf} == {"USD"}
    assert chf[0]["snapshot_at"] == _epoch("2020-01-02")
    assert chf[0]["mid"] == "0.9000"
    eur = [r for r in rows if r["quote_currency_iso"] == "EUR"]
    assert eur[0]["base_currency_iso"] == "USD"
    assert eur[0]["mid"] == "1.1200"
    # Family-less FX bronze leaves rates empty.
    assert _rate_rows(db) == []


def test_load_is_idempotent(tmp_path):
    bronze = tmp_path / "bronze"
    _seed_bronze(bronze)
    db = tmp_path / "fred.db"
    _load(db, bronze)
    before = _rows(db)
    # Re-running skips the already-loaded run (dump_runs) — no duplication.
    _load(db, bronze)
    assert _rows(db) == before


def test_force_rebuild_picks_up_revision(tmp_path):
    bronze = tmp_path / "bronze"
    run = _seed_bronze(bronze)
    db = tmp_path / "fred.db"
    _load(db, bronze)

    # FRED revises 2020-01-02 CHF; --force deletes the silver and rebuilds
    # from the (revised) bronze, so the new value lands as the single
    # (date, base, quote) row — a rebuild, no stale duplicate.
    (run / "DEXSZUS.json").write_text(json.dumps(_obs_doc([
        ("2020-01-02", "0.9050"),
        ("2020-01-03", "0.9100"),
    ])), encoding="utf-8")
    _load(db, bronze, "--force")

    rows = _rows(db)
    assert len(rows) == 4  # still 4 — rebuilt from bronze, not appended
    revised = [r for r in rows
               if r["base_currency_iso"] == "CHF"
               and r["snapshot_at"] == _epoch("2020-01-02")]
    assert revised[0]["mid"] == "0.9050"


def test_in_progress_dump_not_loaded_then_loads_on_completion(tmp_path):
    bronze = tmp_path / "bronze"
    run = _seed_bronze(bronze)
    # Mark the run as an in-flight / crashed walk (the marker download.py
    # drops at run-dir creation). load must skip it — no partial rows
    # leak — and must NOT record it in dump_runs, so a later completion
    # still loads.
    manifest = json.loads((run / "run.json").read_text(encoding="utf-8"))
    manifest["status"] = "in-progress"
    (run / "run.json").write_text(json.dumps(manifest), encoding="utf-8")

    db = tmp_path / "fred.db"
    assert _load(db, bronze) == 0
    assert _rows(db) == []

    # The walk finalises; the same run loads on the next (non-force) pass
    # because it was never marked loaded.
    manifest["status"] = "complete"
    (run / "run.json").write_text(json.dumps(manifest), encoding="utf-8")
    assert _load(db, bronze) == 0
    assert len(_rows(db)) == 4


def test_run_dir_without_manifest_neither_loaded_nor_recorded(tmp_path):
    # A run dir whose walk has not written run.json yet holds series files
    # but no manifest. load must skip it and record no dump_runs row, so
    # the run loads once its manifest lands.
    bronze = tmp_path / "bronze"
    run = _seed_bronze(bronze)
    manifest = (run / "run.json").read_text(encoding="utf-8")
    (run / "run.json").unlink()

    db = tmp_path / "fred.db"
    assert _load(db, bronze) == 0
    assert _rows(db) == []
    conn = silver.open_db(db)
    try:
        assert silver.loaded_snapshots(conn) == set()
    finally:
        conn.close()

    (run / "run.json").write_text(manifest, encoding="utf-8")
    assert _load(db, bronze) == 0
    assert len(_rows(db)) == 4


def test_schema_meta_version(tmp_path):
    db = tmp_path / "fred.db"
    _load(db, tmp_path / "empty")
    conn = silver.open_db(db)
    try:
        assert silver.current_schema_version(conn) == 2
    finally:
        conn.close()


# --- the rates family ---------------------------------------------------------

def test_load_projects_rates(tmp_path):
    bronze = tmp_path / "bronze"
    _seed_both_families(bronze)
    db = tmp_path / "fred.db"
    assert _load(db, bronze) == 0

    # 2 dated DGS10 + 1 dated DTB3; the '.' and '' sentinels are skipped.
    assert _rate_rows(db) == [
        {"as_of": _epoch("2020-01-02"), "series_id": "DGS10", "value": "1.80",
         "payload": '{"currency":"USD","tenor":"10Y","basis":"constant_maturity"}'},
        {"as_of": _epoch("2020-01-03"), "series_id": "DGS10", "value": "1.90",
         "payload": '{"currency":"USD","tenor":"10Y","basis":"constant_maturity"}'},
        {"as_of": _epoch("2020-01-02"), "series_id": "DTB3", "value": "1.50",
         "payload": '{"currency":"USD","tenor":"3M","basis":"discount"}'},
    ]
    # The FX entry of the same run lands in fx_rates, and only there.
    assert _rows(db) == [{"snapshot_at": _epoch("2020-01-02"),
                          "base_currency_iso": "CHF",
                          "quote_currency_iso": "USD", "mid": "0.9000"}]


def test_a_later_run_upserts_a_revised_day(tmp_path):
    # FRED revises 2020-01-02 between two fetches. The later run's value
    # replaces the earlier one in place: one row per (day, series).
    bronze = tmp_path / "bronze"
    _seed_run(bronze, RUN_SLUG, {"DGS10": (DGS10, [
        ("2020-01-02", "1.80"),
    ])})
    db = tmp_path / "fred.db"
    assert _load(db, bronze) == 0

    _seed_run(bronze, LATER_SLUG, {"DGS10": (DGS10, [
        ("2020-01-02", "1.85"),
        ("2020-01-03", "1.90"),
    ])})
    assert _load(db, bronze) == 0
    assert [(r["as_of"], r["value"]) for r in _rate_rows(db)] == [
        (_epoch("2020-01-02"), "1.85"),
        (_epoch("2020-01-03"), "1.90"),
    ]


def test_force_rebuilds_both_tables(tmp_path):
    bronze = tmp_path / "bronze"
    _seed_both_families(bronze)
    db = tmp_path / "fred.db"
    _load(db, bronze)
    fx, rates = _rows(db), _rate_rows(db)
    assert fx and rates

    assert _load(db, bronze, "--force") == 0
    assert _rows(db) == fx
    assert _rate_rows(db) == rates


def test_a_rates_entry_without_its_meaning_is_skipped(tmp_path):
    # A rates row's payload is its meaning; with none to copy there is no
    # row to write. The rest of the run still loads.
    bronze = tmp_path / "bronze"
    _seed_run(bronze, RUN_SLUG, {
        "DGS10": ({"family": "rates", "currency": "USD", "tenor": "10Y"},
                  [("2020-01-02", "1.80")]),
        "DTB3": (DTB3, [("2020-01-02", "1.50")]),
    })
    db = tmp_path / "fred.db"
    assert _load(db, bronze) == 0
    assert [r["series_id"] for r in _rate_rows(db)] == ["DTB3"]


def test_an_fx_entry_without_its_meaning_is_skipped(tmp_path, caplog):
    # An FX row needs both currencies to say which way the rate runs.
    # The rest of the run still loads.
    bronze = tmp_path / "bronze"
    _seed_run(bronze, RUN_SLUG, {
        "DEXSZUS": ({"base": "CHF"}, [("2020-01-02", "0.9000")]),
        "DEXUSEU": ({"base": "USD", "quote": "EUR"},
                    [("2020-01-02", "1.1200")]),
    })
    db = tmp_path / "fred.db"
    with caplog.at_level(logging.WARNING, logger="fred.load"):
        assert _load(db, bronze) == 0
    assert [(r["base_currency_iso"], r["quote_currency_iso"])
            for r in _rows(db)] == [("USD", "EUR")]
    assert "series DEXSZUS has no base/quote in manifest" in caplog.text


def test_a_missing_series_file_is_skipped(tmp_path, caplog):
    # The manifest names a series whose file is gone. That series loads
    # no rows; every other series of the run still loads.
    bronze = tmp_path / "bronze"
    run = _seed_both_families(bronze)
    (run / "DGS10.json").unlink()
    db = tmp_path / "fred.db"
    with caplog.at_level(logging.WARNING, logger="fred.load"):
        assert _load(db, bronze) == 0
    assert [r["series_id"] for r in _rate_rows(db)] == ["DTB3"]
    assert len(_rows(db)) == 1
    assert "series file DGS10.json missing" in caplog.text


def test_an_entry_of_unknown_family_is_skipped(tmp_path):
    bronze = tmp_path / "bronze"
    _seed_run(bronze, RUN_SLUG, {
        "EXMP": ({"family": "swaps", "currency": "USD", "tenor": "5Y",
                  "basis": "par"}, [("2020-01-02", "2.00")]),
        "DTB3": (DTB3, [("2020-01-02", "1.50")]),
    })
    db = tmp_path / "fred.db"
    assert _load(db, bronze) == 0
    assert [r["series_id"] for r in _rate_rows(db)] == ["DTB3"]
    assert _rows(db) == []


def test_the_summary_counts_both_families(tmp_path, caplog):
    bronze = tmp_path / "bronze"
    _seed_both_families(bronze)
    with caplog.at_level(logging.INFO, logger="fred.load"):
        assert _load(tmp_path / "fred.db", bronze) == 0
    assert "done: 1 run(s), 1 fx rows, 3 rates rows" in caplog.text
