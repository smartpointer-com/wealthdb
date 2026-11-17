"""Compare two silver databases table by table, by names and counts only.

Two silvers built from the same bronze by two versions of a loader must
hold the same rows. This tool reads both and reports where they differ:

* a table on one side only, with its row count;
* a column on one side only;
* per table, the row counts and the rows on one side only;
* per column, how many rows that match on the primary key hold a
  different value.

A table with no primary key, or one with a NULL in a key column (SQLite
lets such a key repeat), is compared as a multiset of rows over its
shared columns, so only its one-sided row counts are reported.

It prints table names, column names and counts, never a value. Clock
columns that differ on every load are left out, and so are the path
columns when the two bronze roots differ (``--ignore-paths``). A column
that is a clock in one collector only is left out when a silver file is
named after that collector (``<name>.db``); in every other collector it
holds a bronze fact and is compared.

Both files must be SQLite, or both DuckDB (cointracking). DuckDB needs
the ``duckdb`` package of the interpreter that runs this tool.

Exit status: 0 when the two agree, 1 when they differ, 2 on a usage
error or when a database cannot be read.
"""
from __future__ import annotations

import argparse
import sys
from pathlib import Path

# The kit beside this tool, which runs as `python3 -I` with no kit installed.
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
from collectorkit.incremental import CLOCK_COLUMNS as _LOAD_CLOCKS  # noqa: E402

# Columns that hold the time of a load, not a fact of the bronze: the load
# clocks every silver has (`collectorkit.incremental`), and the clocks of
# one collector or one store. An entry `collector:table.column` is a clock
# in that collector only.
CLOCK_COLUMNS = (
    *sorted(_LOAD_CLOCKS),
    "angellist:tax_documents.retrieved_at",
    "load_runs.load_at",
    "swissquote:documents.first_seen_at",
    "coin_prices.fetched_at",
    "coin_mapping.mapped_at",
)

# Columns that hold a path under the bronze root.
PATH_COLUMNS = (
    "dump_runs.run_dir",
    "documents.file_path",
    "card_statements.file_path",
    "load_runs.bronze_dir",
)

SQLITE_MAGIC = b"SQLite format 3\x00"
DUCKDB_MAGIC = b"DUCK"  # at byte offset 8


def clock_columns(old: Path, new: Path) -> set[str]:
    """The clock columns of two silvers: every unscoped entry, and a scoped
    one when either file is named after its collector."""
    names = {old.stem, new.stem}
    out = set()
    for entry in CLOCK_COLUMNS:
        collector, sep, column = entry.rpartition(":")
        if not sep or collector in names:
            out.add(column)
    return out


def _kind(path: Path) -> str:
    with path.open("rb") as fh:
        head = fh.read(16)
    if head.startswith(SQLITE_MAGIC):
        return "sqlite"
    if head[8:12] == DUCKDB_MAGIC:
        return "duckdb"
    raise ValueError(f"{path}: neither a SQLite nor a DuckDB database")


def _q(name: str) -> str:
    return '"' + name.replace('"', '""') + '"'


class _Catalog:
    """Both databases attached to one in-memory connection as ``o`` (old)
    and ``n`` (new), read-only."""

    def __init__(self, kind: str, old: Path, new: Path):
        self.kind = kind
        if kind == "sqlite":
            import sqlite3
            self.conn = sqlite3.connect("file::memory:", uri=True)
            for alias, path in (("o", old), ("n", new)):
                uri = path.resolve().as_uri() + "?mode=ro"
                self.conn.execute(f"ATTACH DATABASE ? AS {alias}", (uri,))
        else:
            import duckdb
            self.conn = duckdb.connect()
            for alias, path in (("o", old), ("n", new)):
                self.conn.execute(
                    f"ATTACH {_sql_literal(str(path))} AS {alias} (READ_ONLY)")

    def one(self, sql: str, params=()):
        return self.conn.execute(sql, params).fetchone()

    def tables(self, db: str) -> list[str]:
        if self.kind == "sqlite":
            sql = (f"SELECT name FROM {db}.sqlite_master WHERE type = 'table'"
                   " AND name NOT LIKE 'sqlite\\_%' ESCAPE '\\' ORDER BY name")
            return [r[0] for r in self.conn.execute(sql).fetchall()]
        sql = ("SELECT table_name FROM duckdb_tables() WHERE database_name = ?"
               " AND schema_name = 'main' ORDER BY table_name")
        return [r[0] for r in self.conn.execute(sql, (db,)).fetchall()]

    def columns(self, db: str, table: str) -> tuple[list[str], list[str]]:
        """The table's columns in order, and its primary-key columns."""
        if self.kind == "sqlite":
            rows = self.conn.execute(
                f"PRAGMA {db}.table_info({_q(table)})").fetchall()
            cols = [r[1] for r in rows]
            pk = [r[1] for r in sorted((r for r in rows if r[5]),
                                       key=lambda r: r[5])]
            return cols, pk
        rows = self.conn.execute(
            "SELECT column_name FROM duckdb_columns() WHERE database_name = ?"
            " AND schema_name = 'main' AND table_name = ? ORDER BY column_index",
            (db, table)).fetchall()
        pk_rows = self.conn.execute(
            "SELECT constraint_column_names FROM duckdb_constraints()"
            " WHERE database_name = ? AND schema_name = 'main'"
            " AND table_name = ? AND constraint_type = 'PRIMARY KEY'",
            (db, table)).fetchall()
        return [r[0] for r in rows], (list(pk_rows[0][0]) if pk_rows else [])

    def same(self, a: str, b: str) -> str:
        """SQL that is true when two values are equal or both NULL. SQLite's
        ``IS`` works on every version; ``IS NOT DISTINCT FROM`` needs 3.39."""
        if self.kind == "sqlite":
            return f"{a} IS {b}"
        return f"{a} IS NOT DISTINCT FROM {b}"

    def distinct(self, a: str, b: str) -> str:
        """SQL that is true when two values differ, in value or in type."""
        if self.kind == "sqlite":
            return f"({a} IS NOT {b} OR typeof({a}) <> typeof({b}))"
        return f"({a} IS DISTINCT FROM {b})"

    def multiset_exprs(self, alias: str, cols: list[str]) -> list[str]:
        exprs = [f"{alias}.{_q(c)}" for c in cols]
        if self.kind == "sqlite":
            exprs += [f"typeof({alias}.{_q(c)})" for c in cols]
        return exprs


def _sql_literal(text: str) -> str:
    return "'" + text.replace("'", "''") + "'"


def _count(cat: _Catalog, db: str, table: str) -> int:
    return cat.one(f"SELECT COUNT(*) FROM {db}.{_q(table)}")[0]


def _null_in_key(cat: _Catalog, db: str, table: str, key: list[str]) -> bool:
    """Whether a row holds NULL in a key column. SQLite lets such rows
    repeat a key, so they cannot be paired by it."""
    nulls = " OR ".join(f"{_q(k)} IS NULL" for k in key)
    return bool(cat.one(
        f"SELECT EXISTS (SELECT 1 FROM {db}.{_q(table)} WHERE {nulls})")[0])


def _keyed(cat: _Catalog, table: str, key: list[str],
           cols: list[str]) -> tuple[int, int, dict[str, int]]:
    """Rows only in old, only in new, and per-column change counts on rows
    whose key matches."""
    t = _q(table)
    on = " AND ".join(cat.same(f"a.{_q(k)}", f"b.{_q(k)}") for k in key)

    def one_sided(this: str, other: str) -> int:
        return cat.one(
            f"SELECT COUNT(*) FROM {this}.{t} a WHERE NOT EXISTS"
            f" (SELECT 1 FROM {other}.{t} b WHERE {on})")[0]

    only_old = one_sided("o", "n")
    only_new = one_sided("n", "o")
    changed: dict[str, int] = {}
    values = [c for c in cols if c not in key]
    if values:
        sums = ", ".join(
            f"COALESCE(SUM(CASE WHEN {cat.distinct(f'a.{_q(c)}', f'b.{_q(c)}')}"
            " THEN 1 ELSE 0 END), 0)" for c in values)
        row = cat.one(f"SELECT {sums} FROM o.{t} a JOIN n.{t} b ON {on}")
        changed = {c: int(n) for c, n in zip(values, row, strict=True) if n}
    return only_old, only_new, changed


def _multiset(cat: _Catalog, table: str, cols: list[str]) -> tuple[int, int]:
    """Rows only in old and only in new, comparing whole rows over `cols`
    with duplicates counted."""
    t = _q(table)

    def grouped(db: str) -> str:
        exprs = cat.multiset_exprs("x", cols)
        named = ", ".join(f"{e} AS v{i}" for i, e in enumerate(exprs))
        groups = ", ".join(str(i + 1) for i in range(len(exprs)))
        return (f"SELECT {named}, COUNT(*) AS cnt FROM {db}.{t} x"
                f" GROUP BY {groups}")

    width = len(cat.multiset_exprs("x", cols))
    on = " AND ".join(cat.same(f"a.v{i}", f"b.v{i}")
                      for i in range(width)) or "1 = 1"

    def one_sided(this: str, other: str) -> int:
        return int(cat.one(
            f"SELECT COALESCE(SUM(a.cnt - COALESCE(b.cnt, 0)), 0)"
            f" FROM ({grouped(this)}) a LEFT JOIN ({grouped(other)}) b ON {on}"
            f" WHERE a.cnt > COALESCE(b.cnt, 0)")[0])

    return one_sided("o", "n"), one_sided("n", "o")


def diff(old: Path, new: Path, ignore: set[str]) -> list[str]:
    """The differences between two silvers, one line each; empty when they
    agree. Every line holds names and counts only."""
    kinds = {_kind(old), _kind(new)}
    if len(kinds) != 1:
        raise ValueError("the two databases are not of the same kind")
    cat = _Catalog(kinds.pop(), old, new)
    out: list[str] = []
    old_tables, new_tables = cat.tables("o"), cat.tables("n")
    for t in old_tables:
        if t not in new_tables:
            out.append(f"table only in old: {t} ({_count(cat, 'o', t)} rows)")
    for t in new_tables:
        if t not in old_tables:
            out.append(f"table only in new: {t} ({_count(cat, 'n', t)} rows)")
    for t in (t for t in old_tables if t in new_tables):
        old_cols, old_pk = cat.columns("o", t)
        new_cols, new_pk = cat.columns("n", t)
        for c in old_cols:
            if c not in new_cols:
                out.append(f"column only in old: {t}.{c}")
        for c in new_cols:
            if c not in old_cols:
                out.append(f"column only in new: {t}.{c}")
        cols = [c for c in old_cols
                if c in new_cols and f"{t}.{c}" not in ignore]
        key = old_pk if old_pk == new_pk and old_pk and all(
            k in cols for k in old_pk) else []
        if key and (_null_in_key(cat, "o", t, key)
                    or _null_in_key(cat, "n", t, key)):
            key = []
        n_old, n_new = _count(cat, "o", t), _count(cat, "n", t)
        if key:
            only_old, only_new, changed = _keyed(cat, t, key, cols)
        elif not cols:
            only_old, only_new = max(n_old - n_new, 0), max(n_new - n_old, 0)
            changed = {}
        else:
            (only_old, only_new), changed = _multiset(cat, t, cols), {}
        if n_old != n_new or only_old or only_new:
            how = "by key" if key else "as rows"
            out.append(f"{t}: {n_old} rows in old, {n_new} in new;"
                       f" {only_old} only in old, {only_new} only in new"
                       f" ({how})")
        for c, n in changed.items():
            out.append(f"{t}.{c}: {n} rows changed")
    return out


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(
        prog="silver_diff.py",
        description="Compare two silver databases by table and column names "
                    "and counts. Never prints a value.",
        epilog="Always ignored (clock columns): " + ", ".join(CLOCK_COLUMNS)
               + ". An entry COLLECTOR:TABLE.COLUMN applies only when a file"
               " is named COLLECTOR.db; for a renamed copy, pass --ignore."
               " With --ignore-paths also: " + ", ".join(PATH_COLUMNS) + ".")
    p.add_argument("old", type=Path, help="The silver built by the old code.")
    p.add_argument("new", type=Path, help="The silver built by the new code.")
    p.add_argument("--ignore", action="append", default=[],
                   metavar="TABLE.COLUMN",
                   help="Leave a column out of the comparison (repeatable).")
    p.add_argument("--ignore-paths", action="store_true",
                   help="Leave out the columns that store a bronze path, for "
                        "two silvers built from bronze roots at different "
                        "paths.")
    args = p.parse_args(argv)
    for spec in args.ignore:
        table, dot, column = spec.partition(".")
        if not (dot and table and column):
            p.error(f"--ignore {spec!r}: expected TABLE.COLUMN")
    for path in (args.old, args.new):
        if not path.is_file():
            p.error(f"{path}: no such file")
    ignore = clock_columns(args.old, args.new) | set(args.ignore)
    if args.ignore_paths:
        ignore |= set(PATH_COLUMNS)
    try:
        lines = diff(args.old, args.new, ignore)
    except ValueError as e:
        print(f"silver_diff: {e}", file=sys.stderr)
        return 2
    except Exception as e:  # no duckdb, a corrupt file, a driver error
        # The class only: a driver message can quote a path or a value.
        print(f"silver_diff: cannot read the databases ({e.__class__.__name__})",
              file=sys.stderr)
        return 2
    for line in lines:
        print(line)
    return 1 if lines else 0


if __name__ == "__main__":
    raise SystemExit(main())
