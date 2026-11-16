"""Tests for `collectorkit.series`, the loader of dated series, over an
invented collector: a run's manifest lists series of two families, and
each series' observations sit in the manifest entry itself."""
from __future__ import annotations

import json
import logging
import sqlite3

import pytest

from collectorkit import loader, series

SCHEMA = """
CREATE TABLE schema_meta (silver_schema_version INTEGER PRIMARY KEY,
                          applied_at INTEGER NOT NULL);
CREATE TABLE dump_runs (snapshot_at INTEGER PRIMARY KEY,
                        silver_schema_version INTEGER NOT NULL,
                        run_dir TEXT NOT NULL);
CREATE TABLE rates (as_of INTEGER NOT NULL, series_id TEXT NOT NULL,
                    value TEXT NOT NULL, payload TEXT NOT NULL,
                    PRIMARY KEY (as_of, series_id));
CREATE TABLE levels (as_of INTEGER NOT NULL, series_id TEXT NOT NULL,
                     unit TEXT NOT NULL, value TEXT NOT NULL,
                     PRIMARY KEY (as_of, series_id));
INSERT INTO schema_meta VALUES (1, 0);
"""

DAY_1, DAY_2 = 1704067200, 1704153600  # 2024-01-01, 2024-01-02 UTC

log = logging.getLogger("demo.load")


def _level_rows(sid, means, dated):
    return [(day, sid, means["unit"], value) for day, value in dated]


FAMILIES = {
    "rates": series.Family("rates", series.RATE_COLUMNS, ("currency", "tenor")),
    "levels": series.Family("levels", ("as_of", "series_id", "unit", "value"),
                            ("unit",), _level_rows),
}


def _observations(sid, meta):
    return meta["obs"]


def _load(conn, manifest):
    return series.load_series(conn, manifest, FAMILIES,
                              family_of=lambda meta: meta.get("family", "rates"),
                              observations=_observations, log=log)


@pytest.fixture
def conn():
    c = sqlite3.connect(":memory:")
    c.executescript(SCHEMA)
    yield c
    c.close()


def test_each_series_lands_in_its_familys_table(conn):
    manifest = {"series": {
        "R1": {"currency": "USD", "tenor": "1Y",
               "obs": [("2024-01-01", "1.5"), ("2024-01-02", "1.6")]},
        "L1": {"family": "levels", "unit": "index",
               "obs": [("2024-01-02", "100")]},
    }}
    assert _load(conn, manifest) == (2, 1)
    assert conn.execute("SELECT * FROM rates ORDER BY as_of").fetchall() == [
        (DAY_1, "R1", "1.5", '{"currency":"USD","tenor":"1Y"}'),
        (DAY_2, "R1", "1.6", '{"currency":"USD","tenor":"1Y"}')]
    assert conn.execute("SELECT * FROM levels").fetchall() == [
        (DAY_2, "L1", "index", "100")]


def test_a_later_run_overwrites_a_revised_day(conn):
    entry = {"currency": "USD", "tenor": "1Y"}
    _load(conn, {"series": {"R1": {**entry, "obs": [("2024-01-01", "1.5")]}}})
    _load(conn, {"series": {"R1": {**entry, "obs": [("2024-01-01", "1.4")]}}})
    assert conn.execute("SELECT value FROM rates").fetchall() == [("1.4",)]


def test_an_incomplete_meaning_skips_the_series_unread(conn, caplog):
    def observations(sid, meta):
        raise AssertionError("read a series it skips")

    with caplog.at_level(logging.WARNING, logger="demo.load"):
        counts = series.load_series(
            conn, {"series": {"R1": {"currency": "USD", "tenor": ""}}},
            FAMILIES, family_of=lambda meta: "rates",
            observations=observations, log=log)
    assert counts == (0, 0)
    assert caplog.records[0].name == "demo.load"
    assert caplog.messages == [
        "series R1 has no currency/tenor in manifest; skipping"]


def test_an_unknown_family_is_skipped(conn, caplog):
    with caplog.at_level(logging.WARNING, logger="demo.load"):
        counts = _load(conn, {"series": {"X": {"family": "other", "obs": []}}})
    assert counts == (0, 0)
    assert caplog.messages == [
        "series X has unknown family 'other' in manifest; skipping"]


def test_a_date_of_another_shape_raises(conn):
    with pytest.raises(ValueError):
        _load(conn, {"series": {"R1": {"currency": "USD", "tenor": "1Y",
                                       "obs": [("2024/01/01", "1")]}}})


def test_the_spec_loads_runs_through_the_frame(tmp_path):
    migrations = tmp_path / "migrations"
    migrations.mkdir()
    (migrations / "0001_initial.sql").write_text(SCHEMA)
    run = tmp_path / "bronze" / "20240103T000000Z"
    run.mkdir(parents=True)
    (run / "run.json").write_text(json.dumps({"status": "complete", "series": {
        "R1": {"currency": "CHF", "tenor": "2Y",
               "obs": [["2024-01-01", "0.5"]]}}}))

    spec = series.loader_spec(
        "demo", migrations, lambda c, r: _load(c, r.manifest),
        ("rates rows", "level rows"), "The demo loader.")
    p = spec.parser()
    assert p.description == "The demo loader."
    db = tmp_path / "demo.db"
    argv = ["--silver-db", str(db), "--bronze-dir", str(tmp_path / "bronze")]
    assert loader.main(spec, argv) == 0
    with sqlite3.connect(db) as c:
        assert c.execute("SELECT as_of, value FROM rates").fetchall() == [
            (DAY_1, "0.5")]
    with pytest.raises(SystemExit):
        p.parse_args([])


def test_payload_is_compact_and_keeps_key_order():
    assert series.payload({"b": 1, "a": "x"}) == '{"b":1,"a":"x"}'
    assert series.date_to_epoch("2024-01-02") == DAY_2
