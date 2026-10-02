"""The gold adapter keeps a copy of this collector's silver schema: its
tests run in a container that sees only the gold module. This test fails
when the copy and the migrations differ in a column or an index."""
from __future__ import annotations

import sqlite3
from pathlib import Path

import load
from collectorkit import silver

REPO = Path(__file__).resolve().parents[3]
GOLD_COPY = (REPO / "wealthdb" / "internal" / "silver" / "plaid" / "testdata"
             / "silver_schema.sql")


def shape(conn: sqlite3.Connection) -> dict:
    """Each table's columns (name, type, not-null, default, primary-key
    place) and its indexes (name, unique, columns)."""
    names = [n for (n,) in conn.execute(
        "SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name")]
    out = {}
    for name in names:
        columns = [(c[1], c[2], c[3], c[4], c[5])
                   for c in conn.execute(f"PRAGMA table_info({name})")]
        indexes = sorted(
            (i[1], i[2], tuple(c[2] for c in conn.execute(
                f"PRAGMA index_info({i[1]})")))
            for i in conn.execute(f"PRAGMA index_list({name})"))
        out[name] = (columns, indexes)
    return out


def test_the_gold_adapters_copy_is_the_migrated_schema(tmp_path):
    migrated = sqlite3.connect(tmp_path / "migrated.db")
    silver.apply_migrations(migrated, load.MIGRATIONS)
    copy = sqlite3.connect(tmp_path / "copy.db")
    copy.executescript(GOLD_COPY.read_text())
    assert shape(copy) == shape(migrated)
