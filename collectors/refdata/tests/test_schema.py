"""The silver shape is one contract in three files.

migrations/ is the schema the collector writes. The Go adapter's test
fixture (wealthdb/internal/silver/refdata/testdata/silver_schema.sql) is
the migrations' text, in order: the Go tests cut it at a migration's
header for an older silver. The synthetic kind repeats every reference
table so a generator can write the same records through the same
reader. These tests fail when the three drift apart.
"""

from __future__ import annotations

import sqlite3
from pathlib import Path

HERE = Path(__file__).resolve().parent
COLLECTOR = HERE.parent
REPO = COLLECTOR.parent.parent
MIGRATIONS = sorted((COLLECTOR / "migrations").glob("[0-9][0-9][0-9][0-9]_*.sql"))
GO_COPY = REPO / "wealthdb" / "internal" / "silver" / "refdata" / "testdata" / "silver_schema.sql"
SYNTHETIC = REPO / "wealthdb" / "internal" / "silver" / "synthetic" / "testdata" / "silver_schema.sql"
# The tables of a silver that hold no reference record: its schema
# version and its runs.
BOOKKEEPING = frozenset({"schema_meta", "dump_runs"})


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


def test_the_go_fixture_is_the_migrations_in_order():
    assert GO_COPY.read_text() == "".join(m.read_text() for m in MIGRATIONS)


def test_the_synthetic_kind_repeats_the_reference_tables():
    refdata, synthetic = shape(*MIGRATIONS), shape(SYNTHETIC)
    tables = sorted(refdata.keys() - BOOKKEEPING)
    assert "compositions" in tables
    for table in tables:
        assert synthetic.get(table) == refdata[table], table
