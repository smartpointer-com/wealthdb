"""Compare two silver databases table by table, by names and counts only.

Two silvers built from the same bronze by two versions of a loader must
hold the same rows. This tool reads both and reports where they differ:

* a table on one side only, with its row count;
* a column on one side only;
* per table, the row counts and the rows on one side only;
* per column, how many rows that match on the primary key hold a
  different value.

A table with no primary key is compared as a multiset of rows over its
shared columns, so only its one-sided row counts are reported.

It prints table names, column names and counts, never a value. Clock
columns that differ on every load are left out, and so are the path
columns when the two bronze roots differ (``--ignore-paths``).

Both files must be SQLite, or both DuckDB (cointracking). DuckDB needs
the ``duckdb`` package of the interpreter that runs this tool.

Exit status: 0 when the two agree, 1 when they differ, 2 on a usage
error.
"""
from __future__ import annotations

import argparse
import sys
from pathlib import Path

# Columns that hold the time of a load, not a fact of the bronze.
CLOCK_COLUMNS = (
    "schema_meta.applied_at",
    "parser_generations.stamped_at",
    "dump_runs.loaded_at",
    "tax_documents.retrieved_at",
    "load_runs.load_at",
    "documents.first_seen_at",
    "coin_prices.fetched_at",
    "coin_mapping.mapped_at",
)

# Columns that hold a path under the bronze root.
PATH_COLUMNS = (
    "dump_runs.run_dir",
    "documents.file_path",
    "card_statements.file_path",
)

SQLITE_MAGIC = b"SQLite format 3\x00"
DUCKDB_MAGIC = b"DUCK"  # at byte offset 8


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


def _keyed(cat: _Catalog, table: str, key: list[str],
           cols: list[str]) -> tuple[int, int, dict[str, int]]:
    """Rows only in old, only in new, and per-column change counts on rows
    whose key matches."""
    t = _q(table)
    on = " AND ".join(f"a.{_q(k)} IS NOT DISTINCT FROM b.{_q(k)}" for k in key)

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
    on = " AND ".join(f"a.v{i} IS NOT DISTINCT FROM b.v{i}"
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
               + ". With --ignore-paths also: " + ", ".join(PATH_COLUMNS) + ".")
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
    ignore = set(CLOCK_COLUMNS) | set(args.ignore)
    if args.ignore_paths:
        ignore |= set(PATH_COLUMNS)
    try:
        lines = diff(args.old, args.new, ignore)
    except ValueError as e:
        print(f"silver_diff: {e}", file=sys.stderr)
        return 2
    for line in lines:
        print(line)
    return 1 if lines else 0


if __name__ == "__main__":
    raise SystemExit(main())
