"""The silver shape is one contract in three places.

migrations/ is the schema the collector writes. The Go adapter's test
fixture (wealthdb/internal/silver/curves/testdata/silver_schema.sql) is a
minimal mirror of it, without schema_meta. Its `rates` table is the fred
collector's, column for column, so one engine reader serves both kinds.
These tests fail when they drift apart.
"""

from __future__ import annotations

import sqlite3
from pathlib import Path

HERE = Path(__file__).resolve().parent
COLLECTOR = HERE.parent
REPO = COLLECTOR.parent.parent


def _migrations(collector: Path) -> list[Path]:
    return sorted((collector / "migrations").glob("[0-9][0-9][0-9][0-9]_*.sql"))


GO_COPY = REPO / "wealthdb" / "internal" / "silver" / "curves" / "testdata" / "silver_schema.sql"
FRED = REPO / "collectors" / "fred"


def shape(*scripts: Path) -> dict[str, list[tuple]]:
    """Every table's columns (name, type, not-null, default, key) after
    running the scripts in order."""
    con = sqlite3.connect(":memory:")
    try:
        for script in scripts:
            con.executescript(script.read_text())
        tables = [r[0] for r in con.execute(
            "SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name")]
        return {t: [tuple(c[1:]) for c in con.execute(f"PRAGMA table_info({t})")] for t in tables}
    finally:
        con.close()


def test_the_go_fixture_mirrors_every_migrated_table():
    migrated, fixture = shape(*_migrations(COLLECTOR)), shape(GO_COPY)
    assert fixture
    assert set(migrated) - {"schema_meta"} == set(fixture)
    for table, columns in fixture.items():
        assert migrated[table] == columns, table


def test_the_rates_table_is_freds():
    assert shape(*_migrations(COLLECTOR))["rates"] == shape(*_migrations(FRED))["rates"]
