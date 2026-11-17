"""Tests for `collectorkit.incremental`, the check that runs loaded one at
a time build the silver one `load --force` builds. Invented loaders."""
from __future__ import annotations

import argparse
import json
import sqlite3
from pathlib import Path

import pytest

from collectorkit import incremental, loader

SCHEMA = """
CREATE TABLE schema_meta (silver_schema_version INTEGER PRIMARY KEY,
                          applied_at INTEGER NOT NULL);
CREATE TABLE dump_runs (snapshot_at INTEGER PRIMARY KEY,
                        silver_schema_version INTEGER NOT NULL,
                        run_dir TEXT NOT NULL);
CREATE TABLE items (name TEXT PRIMARY KEY, first_load INTEGER);
INSERT INTO schema_meta VALUES (1, CAST(strftime('%s','now') AS INTEGER));
"""


def _parser():
    p = argparse.ArgumentParser()
    p.add_argument("--silver-db", type=Path, required=True)
    p.add_argument("--bronze-dir", type=Path, required=True)
    return p


def _argv(db, bronze):
    return ["--silver-db", str(db), "--bronze-dir", str(bronze)]


@pytest.fixture
def runs(tmp_path):
    out = []
    for slug, names in (("20260101T000000Z", ["a"]),
                        ("20260102T000000Z", ["a", "b"])):
        run = tmp_path / "fixture" / slug
        run.mkdir(parents=True)
        (run / "run.json").write_text(json.dumps({"status": "complete"}))
        (run / "items.json").write_text(json.dumps(names))
        out.append(run)
    return out


def _spec(tmp_path, load_run):
    d = tmp_path / "migrations"
    d.mkdir(exist_ok=True)
    (d / "0001_initial.sql").write_text(SCHEMA)
    spec = loader.LoaderSpec(name="toy", schema=d, load_run=load_run,
                             parser=_parser)
    return lambda argv: loader.main(spec, argv)


def _names(run):
    return json.loads((run.dir / "items.json").read_text())


def test_a_loader_whose_silver_follows_its_runs_passes(tmp_path, runs):
    def load_run(conn, run):
        conn.executemany("INSERT OR REPLACE INTO items VALUES (?, ?)",
                         [(n, run.snapshot_at) for n in _names(run)])

    incremental.check(_spec(tmp_path, load_run), _argv, runs,
                      tmp_path / "work")


def test_a_loader_that_reads_how_runs_were_split_fails(tmp_path, runs):
    # It stamps a new row with the number of runs in bronze when it lands:
    # a run loaded before the next one arrived stamps fewer.
    seen = []

    def load_run(conn, run):
        seen.append(run.dir.name)
        present = len(list(run.dir.parent.iterdir()))
        conn.executemany("INSERT OR IGNORE INTO items VALUES (?, ?)",
                         [(n, present) for n in _names(run)])

    with pytest.raises(AssertionError, match=r"rows differ in \['items'\]"):
        incremental.check(_spec(tmp_path, load_run), _argv, runs,
                          tmp_path / "work")
    assert len(seen) == 4


def test_the_load_time_columns_are_left_out(tmp_path):
    db = tmp_path / "s.db"
    conn = sqlite3.connect(db)
    conn.executescript("CREATE TABLE schema_meta (silver_schema_version, "
                       "applied_at); INSERT INTO schema_meta VALUES (1, 99);")
    conn.commit()
    conn.close()
    assert incremental.silver_rows(db) == {"schema_meta": ["(1,)"]}


def test_a_silver_of_another_store_passes_its_own_reader(tmp_path, runs):
    def load_run(conn, run):
        conn.executemany("INSERT OR REPLACE INTO items VALUES (?, ?)",
                         [(n, run.snapshot_at) for n in _names(run)])

    read = []

    def rows(db, ignore):
        read.append(Path(db).name)
        return {"items": sorted(incremental.silver_rows(db, ignore)["items"])}

    incremental.check(_spec(tmp_path, load_run), _argv, runs,
                      tmp_path / "work", rows=rows)
    assert read == ["one-by-one.db", "at-once.db"]
