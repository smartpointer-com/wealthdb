"""Tests for `collectorkit.loader`, the one load frame, over an invented
collector: a run holds `items.json`, a list of names, and each name
becomes a row of `items`."""
from __future__ import annotations

import argparse
import json
import logging
import sqlite3
from dataclasses import replace
from pathlib import Path

import pytest

from collectorkit import loader, rebuild, silver

SCHEMA = """
CREATE TABLE schema_meta (silver_schema_version INTEGER PRIMARY KEY,
                          applied_at INTEGER NOT NULL);
CREATE TABLE dump_runs (snapshot_at INTEGER PRIMARY KEY,
                        silver_schema_version INTEGER NOT NULL,
                        run_dir TEXT NOT NULL, note TEXT);
CREATE TABLE items (name TEXT PRIMARY KEY, snapshot_at INTEGER NOT NULL);
INSERT INTO schema_meta VALUES (1, 0);
"""

RUN_1, RUN_2, RUN_3 = "20260101T000000Z", "20260102T000000Z", "20260103T000000Z"


@pytest.fixture
def migrations(tmp_path):
    d = tmp_path / "migrations"
    d.mkdir()
    (d / "0001_initial.sql").write_text(SCHEMA)
    return d


def write_run(bronze: Path, slug: str, names, status="complete") -> Path:
    run = bronze / slug
    run.mkdir(parents=True)
    (run / "run.json").write_text(json.dumps({"status": status}))
    (run / "items.json").write_text(json.dumps(names))
    return run


def load_items(conn, run: loader.Run) -> int:
    names = json.loads((run.dir / "items.json").read_text())
    if "boom" in names:
        conn.execute("INSERT INTO items VALUES ('half-written', 0)")
        raise RuntimeError("a run that does not load")
    if "skip" in names:
        conn.execute("INSERT INTO items VALUES ('half-written', 0)")
        raise loader.Skip()
    conn.executemany("INSERT OR REPLACE INTO items VALUES (?, ?)",
                     [(n, run.snapshot_at) for n in names])
    return len(names)


def parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(description="invented loader")
    p.add_argument("--silver-db", type=Path, required=True)
    p.add_argument("--bronze-dir", type=Path, required=True)
    return p


@pytest.fixture
def spec(migrations):
    return loader.LoaderSpec(name="toy", schema=migrations,
                             load_run=load_items, parser=parser,
                             counts=("items",))


@pytest.fixture
def paths(tmp_path):
    return tmp_path / "toy.db", tmp_path / "bronze"


def run_main(spec, paths, *extra):
    db, bronze = paths
    return loader.main(spec, ["--silver-db", str(db), "--bronze-dir",
                              str(bronze), *extra])


def rows(db, sql):
    conn = sqlite3.connect(db)
    try:
        return conn.execute(sql).fetchall()
    finally:
        conn.close()


def test_each_run_loads_once_with_its_run_row_last(spec, paths, caplog):
    db, bronze = paths
    r1 = write_run(bronze, RUN_1, ["a", "b"])
    write_run(bronze, RUN_2, ["c"])
    with caplog.at_level(logging.INFO, logger="toy.load"):
        assert run_main(spec, paths) == 0
    assert rows(db, "SELECT name FROM items ORDER BY name") == [
        ("a",), ("b",), ("c",)]
    assert rows(db, "SELECT * FROM dump_runs ORDER BY snapshot_at")[0] == (
        1767225600, 1, str(r1.resolve()), None)
    assert f"loaded run {RUN_1}: 2 items" in caplog.text
    assert f"done: 2 run(s), 3 items -> {db}" in caplog.text
    caplog.clear()
    with caplog.at_level(logging.INFO, logger="toy.load"):
        assert run_main(spec, paths) == 0
    assert f"done: 0 run(s), 0 items -> {db}" in caplog.text


def test_a_run_reads_the_loaders_own_flags(spec, paths):
    db, bronze = paths
    write_run(bronze, RUN_1, ["a"])

    def flagged():
        p = parser()
        p.add_argument("--tag", default="untagged")
        return p

    def tagged(conn, run):
        conn.execute("INSERT INTO items VALUES (?, ?)",
                     (run.args.tag, run.snapshot_at))
        return 1

    assert run_main(replace(spec, parser=flagged, load_run=tagged), paths,
                    "--tag", "given") == 0
    assert rows(db, "SELECT name FROM items") == [("given",)]


def test_a_run_that_is_not_complete_waits(spec, paths):
    db, bronze = paths
    write_run(bronze, RUN_1, ["a"], status="in-progress")
    assert run_main(spec, paths) == 0
    assert rows(db, "SELECT count(*) FROM dump_runs") == [(0,)]


def test_a_failed_run_rolls_back_and_ends_the_load(spec, paths):
    db, bronze = paths
    write_run(bronze, RUN_1, ["a"])
    write_run(bronze, RUN_2, ["boom"])
    write_run(bronze, RUN_3, ["c"])
    with pytest.raises(RuntimeError, match="does not load"):
        run_main(spec, paths)
    assert rows(db, "SELECT name FROM items") == [("a",)]
    assert rows(db, "SELECT count(*) FROM dump_runs") == [(1,)]


@pytest.mark.parametrize("wrong", [None, (1, 2), "1", [1, 2]])
def test_counts_of_the_wrong_shape_roll_the_run_back(spec, paths, wrong):
    db, bronze = paths
    write_run(bronze, RUN_1, ["a"])
    assert run_main(spec, paths) == 0
    write_run(bronze, RUN_2, ["b"])
    shaped = replace(spec, load_run=lambda c, r: load_items(c, r) and wrong)
    with pytest.raises(TypeError, match=r"toy: load_run returned .* for "
                                        r"counts \('items',\)"):
        run_main(shaped, paths)
    assert rows(db, "SELECT name FROM items") == [("a",)]
    assert rows(db, "SELECT count(*) FROM dump_runs") == [(1,)]


def test_counts_with_no_labels_roll_the_run_back(spec, paths):
    db, bronze = paths
    write_run(bronze, RUN_1, ["a"])
    with pytest.raises(TypeError, match=r"load_run returned 1 for counts"):
        run_main(replace(spec, counts=()), paths)
    assert rows(db, "SELECT count(*) FROM dump_runs") == [(0,)]


def test_a_list_of_counts_reads_as_a_tuple(spec, paths, caplog):
    db, bronze = paths
    write_run(bronze, RUN_1, ["a", "b"])
    listed = replace(spec, counts=("items", "extras"),
                     load_run=lambda c, r: [load_items(c, r), 1])
    with caplog.at_level(logging.INFO, logger="toy.load"):
        assert run_main(listed, paths) == 0
    assert "done: 1 run(s), 2 items, 1 extras" in caplog.text


def _stop_on(name):
    def load(conn, run):
        if name in json.loads((run.dir / "items.json").read_text()):
            conn.execute("INSERT INTO items VALUES ('half-written', 0)")
            rebuild.stop(run.dir.name, "an invariant does not hold",
                         command="toy load --force")
        return load_items(conn, run)
    return load


def test_a_stop_in_a_run_keeps_the_runs_before_it(spec, paths, caplog):
    db, bronze = paths
    write_run(bronze, RUN_1, ["a"])
    write_run(bronze, RUN_2, ["halt"])
    write_run(bronze, RUN_3, ["c"])
    assert run_main(replace(spec, load_run=_stop_on("halt")), paths) == 1
    assert (f"{RUN_2}: an invariant does not hold. The load stopped"
            in caplog.text)
    assert rows(db, "SELECT name FROM items") == [("a",)]
    assert rows(db, "SELECT count(*) FROM dump_runs") == [(1,)]


def test_a_stop_before_the_runs_loads_nothing(spec, paths):
    db, bronze = paths
    write_run(bronze, RUN_1, ["a"])

    def halt(conn, load):
        rebuild.stop(load.silver_db, "an invariant does not hold",
                     command="toy load --force")

    assert run_main(replace(spec, before_runs=halt), paths) == 1
    assert rows(db, "SELECT count(*) FROM dump_runs") == [(0,)]


def _leaves_only(tmp_path):
    return sorted(p.name for p in tmp_path.iterdir())


def test_a_stop_under_force_keeps_the_old_silver(spec, paths, tmp_path):
    db, bronze = paths
    write_run(bronze, RUN_1, ["a"])
    assert run_main(spec, paths) == 0
    before = db.read_bytes()
    write_run(bronze, RUN_2, ["halt"])
    assert run_main(replace(spec, load_run=_stop_on("halt")), paths,
                    "--force") == 1
    assert db.read_bytes() == before
    assert _leaves_only(tmp_path) == ["bronze", "migrations", "toy.db"]


def test_after_runs_raising_under_force_keeps_the_old_silver(spec, paths,
                                                            tmp_path):
    db, bronze = paths
    write_run(bronze, RUN_1, ["a"])
    assert run_main(spec, paths) == 0
    before = db.read_bytes()

    def broken(conn, load):
        raise RuntimeError("a pass after the runs that fails")

    with pytest.raises(RuntimeError, match="fails"):
        run_main(replace(spec, after_runs=broken), paths, "--force")
    assert db.read_bytes() == before
    assert _leaves_only(tmp_path) == ["bronze", "migrations", "toy.db"]


def test_a_failing_status_under_force_keeps_the_old_silver(
        spec, paths, tmp_path, caplog):
    db, bronze = paths
    write_run(bronze, RUN_1, ["a"])
    assert run_main(spec, paths) == 0
    before = db.read_bytes()
    write_run(bronze, RUN_2, ["b"])
    failing = replace(spec, after_runs=lambda conn, load: 3)
    assert run_main(failing, paths, "--force") == 3
    assert db.read_bytes() == before
    assert _leaves_only(tmp_path) == ["bronze", "migrations", "toy.db"]
    assert "the rebuild ended with status 3" in caplog.text
    # A zero status puts the rebuild in place.
    assert run_main(replace(spec, after_runs=lambda conn, load: 0), paths,
                    "--force") == 0
    assert rows(db, "SELECT name FROM items ORDER BY name") == [("a",), ("b",)]


def test_the_given_run_columns_keep_the_path_as_named(spec, paths, tmp_path,
                                                       monkeypatch):
    db, bronze = paths
    write_run(bronze, RUN_1, ["a"])
    monkeypatch.chdir(tmp_path)
    given = replace(spec, run_columns=loader.given_run_columns)
    assert loader.main(given, ["--silver-db", str(db), "--bronze-dir",
                               "bronze"]) == 0
    assert rows(db, "SELECT * FROM dump_runs") == [
        (1767225600, 1, str(Path("bronze") / RUN_1), None)]


def test_a_skipped_run_writes_nothing_and_is_read_again(spec, paths, caplog):
    db, bronze = paths
    write_run(bronze, RUN_1, ["skip"])
    write_run(bronze, RUN_2, ["b"])
    with caplog.at_level(logging.INFO, logger="toy.load"):
        assert run_main(spec, paths) == 0
    assert rows(db, "SELECT name FROM items") == [("b",)]
    assert rows(db, "SELECT count(*) FROM dump_runs") == [(1,)]
    assert "done: 1 run(s), 1 items" in caplog.text
    (bronze / RUN_1 / "items.json").write_text(json.dumps(["a"]))
    assert run_main(spec, paths) == 0
    assert rows(db, "SELECT name FROM items ORDER BY name") == [("a",), ("b",)]


def test_force_rebuilds_from_all_runs(spec, paths):
    db, bronze = paths
    write_run(bronze, RUN_1, ["a"])
    assert run_main(spec, paths) == 0
    conn = sqlite3.connect(db)
    conn.execute("INSERT INTO items VALUES ('stray', 0)")
    conn.commit()
    conn.close()
    assert run_main(spec, paths, "--force") == 0
    assert rows(db, "SELECT name FROM items") == [("a",)]


def test_the_run_row_takes_the_specs_columns_and_the_runs_additions(
        spec, paths):
    def columns(run):
        return {"snapshot_at": run.snapshot_at, "silver_schema_version": 7,
                "run_dir": run.dir.name}

    def load_noting(conn, run):
        run.row["note"] = "noted"
        return load_items(conn, run)

    db, bronze = paths
    write_run(bronze, RUN_1, ["a"])
    assert run_main(replace(spec, run_columns=columns,
                            load_run=load_noting), paths) == 0
    assert rows(db, "SELECT * FROM dump_runs") == [
        (1767225600, 7, RUN_1, "noted")]


def test_a_spec_without_counts_logs_bare_lines(spec, paths, caplog):
    db, bronze = paths
    write_run(bronze, RUN_1, ["a"])
    bare = replace(spec, counts=(), load_run=lambda c, r: load_items(c, r)
                   and None)
    with caplog.at_level(logging.INFO, logger="toy.load"):
        assert run_main(bare, paths) == 0
    assert f"loaded run {RUN_1}\n" in caplog.text + "\n"
    assert f"done: 1 run(s) -> {db}" in caplog.text


def test_several_counts_log_in_their_order(spec, paths, caplog):
    db, bronze = paths
    write_run(bronze, RUN_1, ["a", "b"])
    write_run(bronze, RUN_2, ["c"])
    two = replace(spec, counts=("items", "extras"),
                  load_run=lambda c, r: (load_items(c, r), 1))
    with caplog.at_level(logging.INFO, logger="toy.load"):
        assert run_main(two, paths) == 0
    assert f"loaded run {RUN_1}: 2 items, 1 extras" in caplog.text
    assert "done: 2 run(s), 3 items, 2 extras" in caplog.text


@pytest.mark.parametrize("policy", [loader.MANIFEST_OPTIONAL,
                                    loader.MANIFEST_SKIP,
                                    loader.MANIFEST_REQUIRED])
def test_a_manifest_that_cannot_be_read(spec, paths, caplog, policy):
    db, bronze = paths
    seen = []

    def load_seeing(conn, run):
        seen.append(run.manifest)
        return load_items(conn, run)

    write_run(bronze, RUN_1, ["a"])
    (bronze / RUN_1 / "run.json").write_text("{")
    write_run(bronze, RUN_2, ["b"])
    strict = replace(spec, manifest=policy, load_run=load_seeing)
    if policy == loader.MANIFEST_REQUIRED:
        with pytest.raises(json.JSONDecodeError):
            run_main(strict, paths)
        assert rows(db, "SELECT count(*) FROM dump_runs") == [(0,)]
        return
    assert run_main(strict, paths) == 0
    if policy == loader.MANIFEST_OPTIONAL:
        assert seen == [None, {"status": "complete"}]
        assert rows(db, "SELECT count(*) FROM dump_runs") == [(2,)]
    else:
        assert seen == [{"status": "complete"}]
        assert f"skipping {RUN_1} — run.json cannot be read" in caplog.text
        assert rows(db, "SELECT count(*) FROM dump_runs") == [(1,)]


def test_the_hooks_run_in_order_and_set_the_exit_status(spec, paths):
    db, bronze = paths
    write_run(bronze, RUN_1, ["a"])
    calls = []

    def before(conn, load):
        calls.append(("before", load.version, load.loaded))

    def after(conn, load):
        calls.append(("after", load.version, load.loaded))
        return 3

    hooked = replace(spec, before_runs=before, after_runs=after)
    assert run_main(hooked, paths) == 3
    assert calls == [("before", 1, 0), ("after", 1, 1)]


def test_a_preflight_status_stops_before_the_silver_is_touched(spec, paths):
    db, bronze = paths
    write_run(bronze, RUN_1, ["a"])
    assert run_main(spec, paths) == 0
    before = db.read_bytes()
    stopped = replace(spec, preflight=lambda args: 1)
    assert run_main(stopped, paths, "--force") == 1
    assert db.read_bytes() == before
    assert run_main(replace(spec, preflight=lambda args: None), paths) == 0


def test_defaults_derive_the_paths(spec, paths):
    _, bronze = paths

    def parser_without_db():
        p = argparse.ArgumentParser()
        p.add_argument("--bronze-dir", type=Path, required=True)
        p.add_argument("--silver-db", type=Path, default=None)
        return p

    write_run(bronze, RUN_1, ["a"])
    derived = replace(spec, parser=parser_without_db, defaults=lambda a: (
        a.silver_db or a.bronze_dir / "toy.db", a.bronze_dir))
    assert loader.main(derived, ["--bronze-dir", str(bronze)]) == 0
    assert rows(bronze / "toy.db", "SELECT name FROM items") == [("a",)]


def test_a_default_isolation_connection_keeps_the_run_atomic(spec, paths):
    db, bronze = paths
    write_run(bronze, RUN_1, ["a"])
    write_run(bronze, RUN_2, ["boom"])
    implicit = replace(spec, open_db=silver.open_db_default_isolation)
    with pytest.raises(RuntimeError):
        run_main(implicit, paths)
    assert rows(db, "SELECT name FROM items") == [("a",)]


def test_pending_is_the_specs(spec, paths):
    db, bronze = paths
    write_run(bronze, RUN_1, ["a"])
    write_run(bronze, RUN_2, ["b"])
    only_last = replace(spec, pending=lambda conn, d, log: [d / RUN_2])
    assert run_main(only_last, paths) == 0
    assert rows(db, "SELECT name FROM items") == [("b",)]


def test_the_standard_flags_parse(spec, paths):
    db, bronze = paths
    write_run(bronze, RUN_1, ["a"])
    assert run_main(spec, paths, "-v", "--force") == 0
    with pytest.raises(SystemExit):
        run_main(spec, paths, "--no-such-flag")
