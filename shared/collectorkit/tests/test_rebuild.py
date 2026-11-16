"""Tests for `collectorkit.rebuild`: the rebuild advice, the two stops of
a load and the one rebuild path. Invented silvers only."""
from __future__ import annotations

import argparse
import json
import logging
import sqlite3
from pathlib import Path

import pytest

from collectorkit import loader, rebuild, silver

SCHEMA = """
CREATE TABLE schema_meta (silver_schema_version INTEGER PRIMARY KEY,
                          applied_at INTEGER NOT NULL);
CREATE TABLE dump_runs (snapshot_at INTEGER PRIMARY KEY,
                        silver_schema_version INTEGER NOT NULL,
                        run_dir TEXT NOT NULL);
CREATE TABLE items (name TEXT PRIMARY KEY);
INSERT INTO schema_meta VALUES (1, 0);
"""
# The migration that ships the advice: it creates the level's table and
# moves no level.
ADVISING = rebuild.REBUILD_LEVEL_DDL + ";\nINSERT INTO schema_meta VALUES (2, 0);\n"

ADVICE = ((1, "the colour of old items"), (2, "the size of old items"))
LINE_1 = ("this silver was built before it held the colour of old items; "
          "`toy load --force` rebuilds it from bronze and fills it")
LINE_2 = ("this silver was built before it held the size of old items; "
          "`toy load --force` rebuilds it from bronze and fills it")


@pytest.fixture
def migrations(tmp_path):
    d = tmp_path / "migrations"
    d.mkdir()
    (d / "0001_initial.sql").write_text(SCHEMA)
    return d


def _loaded(conn, n):
    conn.execute("INSERT INTO dump_runs VALUES (?, 1, 'run')", (n,))


# ---- the advice -------------------------------------------------------------

def test_a_silver_built_from_empty_records_the_level_and_gets_no_line(
        migrations, tmp_path, caplog):
    (migrations / "0002_advice.sql").write_text(ADVISING)
    conn = silver.open_db(tmp_path / "toy.db")
    silver.apply_migrations(conn, migrations)
    with caplog.at_level(logging.WARNING):
        assert rebuild.rebuild_advice(conn, ADVICE, "toy load --force") == []
    _loaded(conn, 1)
    assert rebuild.rebuild_advice(conn, ADVICE, "toy load --force") == []
    assert [tuple(r) for r in conn.execute("SELECT level FROM rebuild_level")] == [(2,)]
    assert "built before" not in caplog.text


def test_an_older_silver_gets_a_line_per_entry_until_a_rebuild(
        migrations, tmp_path, caplog):
    conn = silver.open_db(tmp_path / "toy.db")
    silver.apply_migrations(conn, migrations)
    _loaded(conn, 1)
    # The release that ships the advice: its migration moves no level.
    (migrations / "0002_advice.sql").write_text(ADVISING)
    silver.apply_migrations(conn, migrations)
    for _ in range(2):
        caplog.clear()
        with caplog.at_level(logging.WARNING):
            lines = rebuild.rebuild_advice(conn, ADVICE, "toy load --force")
        assert lines == [LINE_1, LINE_2]
        assert caplog.messages == [LINE_1, LINE_2]
    # A silver built at level 1 lacks only the second entry.
    conn.execute("INSERT INTO rebuild_level VALUES (1)")
    assert rebuild.rebuild_advice(conn, ADVICE, "toy load --force") == [LINE_2]


def test_the_advice_changes_neither_silver_nor_the_load(migrations, tmp_path):
    conn = silver.open_db(tmp_path / "toy.db")
    silver.apply_migrations(conn, migrations)
    _loaded(conn, 1)
    (migrations / "0002_advice.sql").write_text(ADVISING)
    silver.apply_migrations(conn, migrations)
    conn.close()
    before = (tmp_path / "toy.db").read_bytes()
    conn = silver.open_db(tmp_path / "toy.db")
    assert rebuild.rebuild_advice(conn, ADVICE, "toy load --force")
    conn.close()
    assert (tmp_path / "toy.db").read_bytes() == before


def test_no_table_or_no_advice_advises_nothing(migrations, tmp_path):
    conn = silver.open_db(tmp_path / "toy.db")
    silver.apply_migrations(conn, migrations)
    _loaded(conn, 1)
    assert rebuild.rebuild_advice(conn, ADVICE, "toy load --force") == []
    (migrations / "0002_advice.sql").write_text(ADVISING)
    silver.apply_migrations(conn, migrations)
    assert rebuild.rebuild_advice(conn, (), "toy load --force") == []


def _write_run(bronze: Path, slug: str, names):
    run = bronze / slug
    run.mkdir(parents=True)
    (run / "run.json").write_text(json.dumps({"status": "complete"}))
    (run / "items.json").write_text(json.dumps(names))


def _load_items(conn, run):
    conn.executemany("INSERT INTO items VALUES (?)",
                     [(n,) for n in json.loads((run.dir / "items.json")
                                               .read_text())])


def _parser():
    p = argparse.ArgumentParser()
    p.add_argument("--silver-db", type=Path, required=True)
    p.add_argument("--bronze-dir", type=Path, required=True)
    return p


def test_the_load_frame_advises_until_load_force(migrations, tmp_path, caplog):
    db, bronze = tmp_path / "toy.db", tmp_path / "bronze"
    argv = ["--silver-db", str(db), "--bronze-dir", str(bronze)]
    _write_run(bronze, "20260101T000000Z", ["a"])
    spec = loader.LoaderSpec(name="toy", schema=migrations,
                             load_run=_load_items, parser=_parser)
    assert loader.main(spec, argv) == 0
    (migrations / "0002_advice.sql").write_text(ADVISING)
    advising = loader.LoaderSpec(name="toy", schema=migrations,
                                 load_run=_load_items, parser=_parser,
                                 advice=ADVICE)
    _write_run(bronze, "20260102T000000Z", ["b"])
    for _ in range(2):
        caplog.clear()
        assert loader.main(advising, argv) == 0
        assert LINE_1 in caplog.messages and LINE_2 in caplog.messages
    caplog.clear()
    assert loader.main(advising, [*argv, "--force"]) == 0
    assert loader.main(advising, argv) == 0
    assert "built before" not in caplog.text
    conn = sqlite3.connect(db)
    assert conn.execute("SELECT count(*) FROM items").fetchone() == (2,)
    conn.close()


# ---- the two stops ----------------------------------------------------------

def test_the_corruption_stop_names_the_check_the_table_and_the_rebuild():
    with pytest.raises(rebuild.LoadStop) as e:
        rebuild.corruption("/x/toy.db", "one balance per account and day",
                           "balances", "toy load --force")
    assert str(e.value) == (
        "/x/toy.db: one balance per account and day does not hold in "
        "balances. The load stopped and left the silver as it was. "
        "`toy load --force` rebuilds it from bronze.")


def test_a_newer_silver_stop_names_both_versions_and_the_upgrade():
    with pytest.raises(rebuild.LoadStop) as e:
        rebuild.no_migration_path("/x/toy.db", 9, 7, "toy load --force")
    assert str(e.value) == (
        "/x/toy.db: silver is at schema version 9, newer than the 7 this "
        "code knows. The load stopped and left the silver as it was. "
        "Upgrade the collector to a version that knows this silver.")


def test_a_silver_no_migration_starts_from_names_the_rebuild():
    with pytest.raises(rebuild.LoadStop) as e:
        rebuild.no_migration_path("/x/toy.db", 3, 7, "toy load --force")
    assert str(e.value) == (
        "/x/toy.db: silver is at schema version 3, which no migration of "
        "this code starts from (it knows up to 7). The load stopped and left "
        "the silver as it was. `toy load --force` rebuilds it from bronze.")


# ---- the rebuild ------------------------------------------------------------

def _replay(names):
    def replay(conn, version):
        assert version == 1
        conn.executemany("INSERT INTO items VALUES (?)", [(n,) for n in names])
        return len(names)
    return replay


def test_a_rebuild_puts_a_fresh_silver_in_place(migrations, tmp_path):
    db = tmp_path / "toy.db"
    assert rebuild.rebuild(db, migrations, _replay(["a"])) == 1
    assert rebuild.rebuild(db, migrations, _replay(["b", "c"])) == 2
    conn = sqlite3.connect(db)
    assert conn.execute("SELECT name FROM items ORDER BY name").fetchall() == [
        ("b",), ("c",)]
    conn.close()
    assert sorted(p.name for p in tmp_path.iterdir()) == ["migrations", "toy.db"]
    assert db.stat().st_mode & 0o777 == 0o600


def test_a_failed_rebuild_leaves_the_old_file_byte_identical(migrations,
                                                             tmp_path):
    db = tmp_path / "toy.db"
    rebuild.rebuild(db, migrations, _replay(["a"]))
    before = db.read_bytes()

    def broken(conn, version):
        _replay(["b"])(conn, version)
        raise RuntimeError("a run that cannot be loaded")

    with pytest.raises(RuntimeError):
        rebuild.rebuild(db, migrations, broken)
    assert db.read_bytes() == before
    assert not (tmp_path / "toy.db.rebuild").exists()


def test_the_old_file_stays_in_place_when_the_replace_fails(
        migrations, tmp_path, monkeypatch):
    db = tmp_path / "toy.db"
    rebuild.rebuild(db, migrations, _replay(["a"]))
    before = db.read_bytes()

    def no_rename(src, dst):
        raise OSError("no rename")

    monkeypatch.setattr(rebuild.os, "replace", no_rename)
    with pytest.raises(OSError, match="no rename"):
        rebuild.rebuild(db, migrations, _replay(["b"]))
    assert db.read_bytes() == before
    assert not (tmp_path / "toy.db.rebuild").exists()


def _failing_items(conn, run):
    if "boom" in json.loads((run.dir / "items.json").read_text()):
        raise RuntimeError("a run that cannot be loaded")
    _load_items(conn, run)


def test_a_failed_force_leaves_the_old_silver_byte_identical(migrations,
                                                            tmp_path):
    db, bronze = tmp_path / "toy.db", tmp_path / "bronze"
    argv = ["--silver-db", str(db), "--bronze-dir", str(bronze)]
    spec = loader.LoaderSpec(name="toy", schema=migrations,
                             load_run=_failing_items, parser=_parser)
    _write_run(bronze, "20260101T000000Z", ["a"])
    assert loader.main(spec, argv) == 0
    before = db.read_bytes()
    _write_run(bronze, "20260102T000000Z", ["boom"])
    with pytest.raises(RuntimeError, match="cannot be loaded"):
        loader.main(spec, [*argv, "--force"])
    assert db.read_bytes() == before
    assert sorted(p.name for p in tmp_path.iterdir()) == [
        "bronze", "migrations", "toy.db"]


def test_a_silver_newer_than_the_code_stops_before_anything_is_written(
        migrations, tmp_path, caplog):
    db, bronze = tmp_path / "toy.db", tmp_path / "bronze"
    argv = ["--silver-db", str(db), "--bronze-dir", str(bronze)]
    spec = loader.LoaderSpec(name="toy", schema=migrations,
                             load_run=_load_items, parser=_parser)
    _write_run(bronze, "20260101T000000Z", ["a"])
    (migrations / "0002_advice.sql").write_text(ADVISING)
    assert loader.main(spec, argv) == 0
    # The code of an older release: it knows the first migration only.
    (migrations / "0002_advice.sql").unlink()
    _write_run(bronze, "20260102T000000Z", ["b"])
    before = db.read_bytes()
    assert loader.main(spec, argv) == 1
    assert db.read_bytes() == before
    assert (f"{db}: silver is at schema version 2, newer than the 1 this "
            "code knows. The load stopped" in caplog.text)
    # --force builds a silver this code knows.
    assert loader.main(spec, [*argv, "--force"]) == 0
    assert loader.main(spec, argv) == 0


def test_a_rebuild_clears_a_leftover_fresh_file(migrations, tmp_path):
    db = tmp_path / "toy.db"
    (tmp_path / "toy.db.rebuild").write_bytes(b"left by a killed rebuild")
    rebuild.rebuild(db, migrations, _replay(["a"]))
    assert not (tmp_path / "toy.db.rebuild").exists()


# ---- the table a collector's migration copies ------------------------------

REPO = Path(__file__).resolve().parents[3]


def _columns(conn, table):
    return conn.execute(f"PRAGMA table_info({table})").fetchall()


@pytest.mark.parametrize("table,ddl", [
    ("rebuild_level", rebuild.REBUILD_LEVEL_DDL),
])
def test_every_collector_creates_the_kits_table_as_the_kit_states_it(
        table, ddl):
    # A collector whose migrations create the table gets the one the kit's
    # statement makes.
    want = sqlite3.connect(":memory:")
    want.execute(ddl)
    for migrations in sorted((REPO / "collectors").glob("*/migrations")):
        if not any(f"CREATE TABLE {table}" in sql.read_text(encoding="utf-8")
                   for sql in migrations.glob("*.sql")):
            continue
        got = sqlite3.connect(":memory:")
        silver.apply_migrations(got, migrations)
        assert _columns(got, table) == _columns(want, table), migrations
