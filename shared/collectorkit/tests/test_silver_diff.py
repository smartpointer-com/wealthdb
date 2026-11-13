"""Tests for tools/silver_diff.py over invented silvers.

The tool's output is names and counts. Each fixture holds one marker
value, and every test checks that the marker reaches no line of the
output. The checks compare booleans, so a failure message cannot carry a
value either.
"""
from __future__ import annotations

import importlib.util
import sqlite3
import subprocess
import sys
from pathlib import Path

import pytest

TOOL = Path(__file__).resolve().parents[1] / "tools" / "silver_diff.py"
_spec = importlib.util.spec_from_file_location("silver_diff", TOOL)
silver_diff = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(silver_diff)

MARKER = "MARKER-7f3a"

SCHEMA = """
CREATE TABLE schema_meta (silver_schema_version INTEGER PRIMARY KEY,
                          applied_at INTEGER NOT NULL);
CREATE TABLE dump_runs (snapshot_at INTEGER PRIMARY KEY, run_dir TEXT NOT NULL);
CREATE TABLE accounts (snapshot_at INTEGER NOT NULL, external_id TEXT NOT NULL,
                       label TEXT, balance,
                       PRIMARY KEY (snapshot_at, external_id));
CREATE TABLE notes (body TEXT, n INTEGER);
"""


def _silver(path: Path, *edits: str, applied_at: int = 1, run_dir: str = "/a") -> Path:
    conn = sqlite3.connect(path)
    conn.executescript(SCHEMA)
    conn.execute("INSERT INTO schema_meta VALUES (1, ?)", (applied_at,))
    conn.execute("INSERT INTO dump_runs VALUES (100, ?)", (run_dir,))
    conn.executemany("INSERT INTO accounts VALUES (?,?,?,?)", [
        (100, "acct-1", MARKER, 1.5),
        (100, "acct-2", "two", 2.0),
        (200, "acct-1", "one", 3.5),
    ])
    conn.executemany("INSERT INTO notes VALUES (?,?)",
                     [(MARKER, 1), ("x", 2), ("x", 2)])
    for sql in edits:
        conn.execute(sql)
    conn.commit()
    conn.close()
    return path


def _run(capsys, *argv) -> tuple[int, list[str]]:
    rc = silver_diff.main([str(a) for a in argv])
    out, err = capsys.readouterr()
    leaked = MARKER in out or MARKER in err
    assert not leaked, "a fixture value reached the output"
    return rc, out.splitlines()


@pytest.fixture
def old(tmp_path):
    return _silver(tmp_path / "old.db")


def test_equal_silvers_report_nothing(tmp_path, old, capsys):
    new = _silver(tmp_path / "new.db")
    assert _run(capsys, old, new) == (0, [])


def test_clock_columns_are_ignored(tmp_path, old, capsys):
    new = _silver(tmp_path / "new.db", applied_at=2)
    assert _run(capsys, old, new) == (0, [])


def test_path_columns_differ_unless_ignored(tmp_path, old, capsys):
    new = _silver(tmp_path / "new.db", run_dir="/b")
    assert _run(capsys, old, new) == (1, ["dump_runs.run_dir: 1 rows changed"])
    assert _run(capsys, old, new, "--ignore-paths") == (0, [])


def test_one_changed_cell_is_counted_by_column(tmp_path, old, capsys):
    new = _silver(tmp_path / "new.db", "UPDATE accounts SET label = 'other'"
                  " WHERE external_id = 'acct-1' AND snapshot_at = 100")
    assert _run(capsys, old, new) == (1, ["accounts.label: 1 rows changed"])
    assert _run(capsys, old, new, "--ignore", "accounts.label") == (0, [])


def test_an_integer_for_an_equal_real_is_a_change(tmp_path, old, capsys):
    new = _silver(tmp_path / "new.db",
                  "UPDATE accounts SET balance = 2 WHERE external_id = 'acct-2'")
    assert _run(capsys, old, new) == (1, ["accounts.balance: 1 rows changed"])


def test_one_missing_row_is_counted(tmp_path, old, capsys):
    new = _silver(tmp_path / "new.db", "DELETE FROM accounts WHERE snapshot_at = 200")
    assert _run(capsys, old, new) == (1, [
        "accounts: 3 rows in old, 2 in new; 1 only in old, 0 only in new (by key)"])


def test_a_table_without_a_key_compares_as_a_multiset(tmp_path, old, capsys):
    new = _silver(tmp_path / "new.db", "DELETE FROM notes WHERE rowid = 3",
                  "INSERT INTO notes VALUES ('y', 3)")
    assert _run(capsys, old, new) == (1, [
        "notes: 3 rows in old, 3 in new; 1 only in old, 1 only in new (as rows)"])


def test_one_extra_table_and_column_are_named(tmp_path, old, capsys):
    new = _silver(tmp_path / "new.db",
                  f"CREATE TABLE extra AS SELECT '{MARKER}' AS v",
                  "ALTER TABLE accounts ADD COLUMN kind TEXT")
    assert _run(capsys, old, new) == (1, [
        "table only in new: extra (1 rows)",
        "column only in new: accounts.kind",
    ])
    assert _run(capsys, new, old)[1][0] == "table only in old: extra (1 rows)"


def test_a_one_collector_clock_is_ignored_in_that_collector_only(tmp_path, capsys):
    edits = ("CREATE TABLE documents (sha TEXT PRIMARY KEY, first_seen_at INTEGER)",)

    def pair(name: str) -> tuple[Path, Path]:
        a, b = tmp_path / "a" / name, tmp_path / "b" / name
        a.parent.mkdir(exist_ok=True)
        b.parent.mkdir(exist_ok=True)
        return (_silver(a, *edits, "INSERT INTO documents VALUES ('d', 1)"),
                _silver(b, *edits, "INSERT INTO documents VALUES ('d', 2)"))

    assert _run(capsys, *pair("swissquote.db")) == (0, [])
    assert _run(capsys, *pair("carta.db")) == (
        1, ["documents.first_seen_at: 1 rows changed"])


def test_the_manual_bronze_dir_is_a_path_column(tmp_path, capsys):
    def build(path: Path, bronze_dir: str) -> Path:
        return _silver(path, "CREATE TABLE load_runs (load_at INTEGER PRIMARY KEY,"
                       " bronze_dir TEXT)",
                       f"INSERT INTO load_runs VALUES (1, '{bronze_dir}')")

    old, new = build(tmp_path / "old.db", "/a"), build(tmp_path / "new.db", "/b")
    assert _run(capsys, old, new)[0] == 1
    assert _run(capsys, old, new, "--ignore-paths") == (0, [])


def test_a_database_that_cannot_be_read_is_exit_2(tmp_path, old, capsys,
                                                   monkeypatch):
    def broken(*_args, **_kwargs):
        raise sqlite3.OperationalError("a driver message")

    monkeypatch.setattr(silver_diff, "diff", broken)
    assert _run(capsys, old, old) == (2, [])


def test_duckdb_files_without_the_package_are_exit_2(tmp_path, capsys,
                                                     monkeypatch):
    for name in ("old.duckdb", "new.duckdb"):
        (tmp_path / name).write_bytes(b"\0" * 8 + b"DUCK" + b"\0" * 8)
    monkeypatch.setitem(sys.modules, "duckdb", None)
    assert _run(capsys, tmp_path / "old.duckdb", tmp_path / "new.duckdb") == (2, [])


def test_usage_errors(tmp_path, old, capsys):
    with pytest.raises(SystemExit) as e:
        silver_diff.main([str(old), str(tmp_path / "absent.db")])
    assert e.value.code == 2
    with pytest.raises(SystemExit) as e:
        silver_diff.main([str(old), str(old), "--ignore", "nodot"])
    assert e.value.code == 2
    text = tmp_path / "text.db"
    text.write_text("not a database")
    assert silver_diff.main([str(old), str(text)]) == 2
    capsys.readouterr()


def test_help_names_the_ignored_columns():
    out = subprocess.run([sys.executable, "-I", str(TOOL), "--help"],
                         capture_output=True, text=True, check=True).stdout
    assert "schema_meta.applied_at" in out
    assert "swissquote:documents.first_seen_at" in out
    assert "dump_runs.run_dir" in out


def test_duckdb_silvers(tmp_path, capsys):
    duckdb = pytest.importorskip("duckdb")

    def build(path: Path, *edits: str) -> Path:
        conn = duckdb.connect(str(path))
        conn.execute("CREATE TABLE coin_prices (coin VARCHAR, day DATE,"
                     " price DECIMAL(38, 18), fetched_at BIGINT,"
                     " PRIMARY KEY (coin, day))")
        conn.execute("CREATE TABLE notes (body VARCHAR)")
        conn.execute(f"INSERT INTO coin_prices VALUES ('{MARKER}', '2024-01-01', 1.5, 1),"
                     " ('c2', '2024-01-01', 2.5, 1)")
        conn.execute("INSERT INTO notes VALUES ('x'), ('x')")
        for sql in edits:
            conn.execute(sql)
        conn.close()
        return path

    old = build(tmp_path / "old.duckdb")
    assert _run(capsys, old, build(tmp_path / "same.duckdb",
                                   "UPDATE coin_prices SET fetched_at = 2")) == (0, [])
    new = build(tmp_path / "new.duckdb",
                "UPDATE coin_prices SET price = 9 WHERE coin = 'c2'",
                "DELETE FROM notes", "INSERT INTO notes VALUES ('x')")
    assert _run(capsys, old, new) == (1, [
        "coin_prices.price: 1 rows changed",
        "notes: 2 rows in old, 1 in new; 1 only in old, 0 only in new (as rows)",
    ])
