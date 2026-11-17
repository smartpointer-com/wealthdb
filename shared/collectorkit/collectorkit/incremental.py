"""A loader test: runs loaded one at a time build the silver one
``load --force`` builds.

A load is incremental. It reads the runs silver has not seen, on top of
what earlier loads wrote. So the silver must not depend on how the runs
were split across loads. :func:`check` loads a collector's fixture runs
one load per run, then all of them in one ``load --force``, into a second
silver beside the first, and compares the two table by table.

Both silvers read the same bronze dir, so the paths they store agree. The
columns that hold the time of a load are left out (:data:`CLOCK_COLUMNS`).

Imported as ``from collectorkit import incremental`` by a collector's
tests; it is not imported by the package itself.
"""
from __future__ import annotations

import shutil
import sqlite3
from collections.abc import Callable, Iterable, Mapping, Sequence
from pathlib import Path

# `table.column` entries that hold the time of a load, not a fact of the
# bronze.
CLOCK_COLUMNS = frozenset({
    "schema_meta.applied_at",
    "parser_generations.stamped_at",
    "dump_runs.loaded_at",
    "document_derivations.derived_at",
})


def silver_rows(db: Path, ignore: Iterable[str] = CLOCK_COLUMNS
                ) -> dict[str, list[str]]:
    """Every table of the SQLite silver at `db` as a sorted list of its
    rows, each row the repr of its values with the `ignore` columns left
    out."""
    ignore = set(ignore)
    conn = sqlite3.connect(f"{Path(db).resolve().as_uri()}?mode=ro", uri=True)
    try:
        tables = [r[0] for r in conn.execute(
            "SELECT name FROM sqlite_master WHERE type = 'table' "
            "AND name NOT LIKE 'sqlite\\_%' ESCAPE '\\' ORDER BY name")]
        out = {}
        for table in tables:
            cols = [r[1] for r in conn.execute(
                f'PRAGMA table_info("{table}")')
                if f"{table}.{r[1]}" not in ignore]
            sql = ", ".join(f'"{c}"' for c in cols)
            out[table] = sorted(repr(row) for row in conn.execute(
                f'SELECT {sql} FROM "{table}"'))
        return out
    finally:
        conn.close()


def check(load: Callable[[list[str]], int],
          argv: Callable[[Path, Path], list[str]],
          runs: Sequence[Path], work: Path,
          ignore: Iterable[str] = CLOCK_COLUMNS,
          rows: Callable[[Path, Iterable[str]], Mapping[str, list]]
          = silver_rows) -> None:
    """Assert that `runs`, loaded one at a time, give the silver that one
    ``load --force`` of all of them gives.

    `load(argv)` is the loader's `main`; `argv(silver_db, bronze_dir)`
    is its command line for those two paths. Each run dir of `runs` is
    copied into ``<work>/bronze`` in turn, and a load follows each copy.
    Then ``--force`` builds a second silver from the whole dir.

    `rows(db, ignore)` reads a silver as table to sorted rows; the
    default, :func:`silver_rows`, reads SQLite. A silver in another
    store passes its own reader.
    """
    bronze = Path(work) / "bronze"
    bronze.mkdir(parents=True)
    one_by_one, at_once = Path(work) / "one-by-one.db", Path(work) / "at-once.db"
    for run in runs:
        shutil.copytree(run, bronze / Path(run).name)
        assert load(argv(one_by_one, bronze)) == 0, f"loading {Path(run).name}"
    assert load([*argv(at_once, bronze), "--force"]) == 0, "load --force"
    a, b = rows(one_by_one, ignore), rows(at_once, ignore)
    assert sorted(a) == sorted(b), "the two silvers hold other tables"
    differ = [t for t in a if a[t] != b[t]]
    assert not differ, f"rows differ in {differ}"
