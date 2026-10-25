"""Unit tests for `bronze.record_run_listing` and `bronze.backfill_run_listings`,
the `run_accounts` writers a content-deduping loader shares. Synthetic ids."""
from __future__ import annotations

import logging
import sqlite3

import pytest

from collectorkit import bronze

RUN_A = "20260801T120000Z"
RUN_B = "20260802T120000Z"
EPOCH_A = bronze.parse_run_ts(RUN_A)
EPOCH_B = bronze.parse_run_ts(RUN_B)


@pytest.fixture
def conn():
    c = sqlite3.connect(":memory:", isolation_level=None)
    c.execute("CREATE TABLE dump_runs (snapshot_at INTEGER PRIMARY KEY)")
    c.execute("CREATE TABLE run_accounts (snapshot_at INTEGER NOT NULL, "
              "account_external_id TEXT NOT NULL, "
              "PRIMARY KEY (snapshot_at, account_external_id))")
    return c


def _listings(conn) -> list[tuple[int, str]]:
    return [tuple(r) for r in conn.execute(
        "SELECT snapshot_at, account_external_id FROM run_accounts ORDER BY 1, 2")]


def _loaded(conn, *epochs: int) -> None:
    conn.executemany("INSERT INTO dump_runs VALUES (?)", [(e,) for e in epochs])


def _reader(by_run: dict[str, list[str]]):
    return lambda run_dir: by_run.get(run_dir.name, [])


def test_record_run_listing_lands_each_id_once(conn):
    bronze.record_run_listing(conn, EPOCH_A, ["acct-1", "acct-2", "acct-1"])
    bronze.record_run_listing(conn, EPOCH_A, ["acct-1"])
    assert _listings(conn) == [(EPOCH_A, "acct-1"), (EPOCH_A, "acct-2")]


def test_record_run_listing_drops_empty_ids_and_an_empty_run(conn):
    bronze.record_run_listing(conn, EPOCH_A, ["", "acct-1"])
    bronze.record_run_listing(conn, EPOCH_B, [])
    assert _listings(conn) == [(EPOCH_A, "acct-1")]


def test_backfill_fills_a_loaded_run_without_a_listing(conn, tmp_path):
    (tmp_path / RUN_A).mkdir()
    _loaded(conn, EPOCH_A)
    bronze.backfill_run_listings(conn, tmp_path, _reader({RUN_A: ["acct-1", "acct-1"]}))
    assert _listings(conn) == [(EPOCH_A, "acct-1")]


def test_backfill_leaves_a_recorded_run_alone(conn, tmp_path):
    (tmp_path / RUN_A).mkdir()
    (tmp_path / RUN_B).mkdir()
    _loaded(conn, EPOCH_A, EPOCH_B)
    bronze.record_run_listing(conn, EPOCH_A, ["acct-1"])
    bronze.backfill_run_listings(
        conn, tmp_path, _reader({RUN_A: ["other"], RUN_B: ["acct-2"]}))
    assert _listings(conn) == [(EPOCH_A, "acct-1"), (EPOCH_B, "acct-2")]


def test_backfill_ignores_an_unloaded_run_and_a_non_run_dir(conn, tmp_path):
    for name in (RUN_A, "20260230T120000Z", "notes"):
        (tmp_path / name).mkdir()
    read = _reader({RUN_A: ["acct-1"], "20260230T120000Z": ["acct-2"], "notes": ["acct-3"]})
    bronze.backfill_run_listings(conn, tmp_path, read)
    assert _listings(conn) == []
    _loaded(conn, EPOCH_B)
    bronze.backfill_run_listings(conn, tmp_path, read)
    assert _listings(conn) == []


def test_backfill_skips_an_unparseable_slug(conn, tmp_path):
    # The slug has the run-dir shape but names no real instant.
    (tmp_path / "20260230T120000Z").mkdir()
    (tmp_path / RUN_A).mkdir()
    _loaded(conn, EPOCH_A)
    bronze.backfill_run_listings(conn, tmp_path, _reader(
        {RUN_A: ["acct-1"], "20260230T120000Z": ["acct-2"]}))
    assert _listings(conn) == [(EPOCH_A, "acct-1")]


def test_backfill_with_the_bronze_dir_gone_logs_the_runs_left(conn, tmp_path, caplog):
    _loaded(conn, EPOCH_A, EPOCH_B)
    with caplog.at_level(logging.INFO, logger="collectorkit.bronze"):
        bronze.backfill_run_listings(conn, tmp_path / "gone", _reader({}))
    assert _listings(conn) == []
    assert "2 loaded run(s) still have no listing" in caplog.text


def test_backfill_logs_only_the_runs_still_without_a_listing(conn, tmp_path, caplog):
    (tmp_path / RUN_A).mkdir()
    (tmp_path / RUN_B).mkdir()
    _loaded(conn, EPOCH_A, EPOCH_B)
    with caplog.at_level(logging.INFO, logger="collectorkit.bronze"):
        bronze.backfill_run_listings(conn, tmp_path, _reader({RUN_A: ["acct-1"]}))
    assert _listings(conn) == [(EPOCH_A, "acct-1")]
    assert "1 loaded run(s) still have no listing" in caplog.text


def test_backfill_is_silent_when_every_loaded_run_has_a_listing(conn, tmp_path, caplog):
    (tmp_path / RUN_A).mkdir()
    _loaded(conn, EPOCH_A)
    bronze.record_run_listing(conn, EPOCH_A, ["acct-1"])
    with caplog.at_level(logging.INFO, logger="collectorkit.bronze"):
        bronze.backfill_run_listings(conn, tmp_path, _reader({}))
    assert caplog.text == ""
