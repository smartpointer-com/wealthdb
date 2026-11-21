"""What stops a load, what a load only advises, and the one rebuild path.

A load is incremental and keeps every loaded silver loadable. An SQLite
error while loading surfaces as the error itself. A load stops, through
:func:`stop` so every collector words it the same way, for:

* a silver newer than the code (:func:`migration_path_stop`);
* an invariant over its bronze or its rows that the loader checks.

A stop leaves silver as it was, readable.

A change whose new column only bronze can fill for old rows does not stop
a load. It adds an entry to the loader's ``REBUILD_ADVICE``, and
:func:`rebuild_advice` prints one line for it at every load of a silver
built before the entry, until a rebuild.

:func:`rebuild` is that rebuild: a fresh silver beside the old one, put in
place only once every run is in. It replays the bronze that exists, so the
rows of a loaded run whose run dir is gone from bronze (pruned, say) are
not in the new silver. :func:`runs_without_bronze` names those runs before
a rebuild starts, and the advice names their count.

Imported as ``from collectorkit import rebuild``; it is not imported by
the package itself.
"""
from __future__ import annotations

import logging
import os
import sqlite3
from collections.abc import Callable, Sequence
from datetime import datetime, timezone
from pathlib import Path
from typing import NoReturn, TypeVar

from collectorkit import bronze, silver

log = logging.getLogger(__name__)

T = TypeVar("T")

# The table that holds a silver's rebuild level: the highest
# REBUILD_ADVICE level its code knew when the silver was built from
# empty. A collector that advises creates it in a migration of its own,
# with this statement as it stands, and inserts no row: a migration never
# moves the level. No row reads as level 0 in a silver that has loaded a
# run.
REBUILD_LEVEL_DDL = """\
CREATE TABLE rebuild_level (
    level INTEGER NOT NULL
)"""


class LoadStop(Exception):
    """A load that stopped. Silver is as it was and stays readable."""


def stop(where, reason: str, *, command: str | None = None,
         upgrade: bool = False) -> NoReturn:
    """Raise the one stop of a load.

    `where` names the silver (or the Item, the tree); `reason` says what
    was found. The message then says that the load stopped and how to go
    on: the rebuild `command`, or, with `upgrade`, a newer collector.
    """
    remedy = ("Upgrade the collector to a version that knows this silver."
              if upgrade else f"`{command}` rebuilds it from bronze.")
    raise LoadStop(f"{where}: {reason}. The load stopped and left the "
                   f"silver as it was. {remedy}")


def no_migration_path(where, found: int, known: int) -> NoReturn:
    """Stop for a silver at schema version `found`, newer than the `known`
    this code's migrations reach. The message names the upgrade."""
    stop(where, f"silver is at schema version {found}, newer than the "
                f"{known} this code knows", upgrade=True)


def _loaded_a_run(conn: sqlite3.Connection) -> bool:
    return (silver.has_table(conn, "dump_runs") and conn.execute(
        "SELECT 1 FROM dump_runs LIMIT 1").fetchone() is not None)


# How many run slugs a warning names before it gives only the count.
_NAMED_RUNS = 10


def _loaded_runs_gone(conn: sqlite3.Connection, bronze_dir: Path) -> list[str]:
    """The slugs of the runs `dump_runs` holds whose run dir is not under
    `bronze_dir`, oldest first. A run is its instant (`snapshot_at`), so
    the path a loader stored does not matter. A row whose stored run dir
    is no timestamped run dir (a load-clock stamp) is no run."""
    if not silver.has_table(conn, "dump_runs"):
        return []
    present = set()
    for d in bronze.iter_run_dirs(bronze_dir):
        try:
            present.add(bronze.parse_run_ts(d.name))
        except ValueError:      # a slug that names no real instant
            continue
    has_run_dir = any(row[1] == "run_dir" for row in conn.execute(
        "PRAGMA table_info(dump_runs)"))
    rows = conn.execute(
        "SELECT snapshot_at, " + ("run_dir" if has_run_dir else "NULL")
        + " FROM dump_runs ORDER BY snapshot_at")
    return [bronze.ts_slug(datetime.fromtimestamp(ts, timezone.utc))
            for ts, run_dir in rows
            if ts not in present
            and (run_dir is None
                 or bronze.RUN_DIR_RE.match(Path(run_dir).name))]


def runs_without_bronze(db_path: Path, bronze_dir: Path) -> list[str]:
    """Log, and return, the runs a rebuild of the silver at `db_path` from
    `bronze_dir` would drop: the loaded runs whose run dir is gone.

    Called before a rebuild starts. It only reads the old silver; no
    silver, or one that has loaded no run, gives none. A read-write
    connection, so its close leaves no SQLite sidecar behind. The
    warning names the runs, and the rebuild goes on. An old silver that
    cannot be read (not a database, or locked) gives a warning and no
    run: a rebuild is how such a silver is replaced.
    """
    db_path = Path(db_path)
    if not db_path.is_file():
        return []
    try:
        conn = sqlite3.connect(db_path)
        try:
            gone = _loaded_runs_gone(conn, bronze_dir)
        finally:
            conn.close()
    except sqlite3.Error as e:
        log.warning("%s: cannot read its loaded runs (%s); the rebuild goes "
                    "on", db_path, e)
        return []
    if gone:
        named = ", ".join(gone[:_NAMED_RUNS])
        more = (f" and {len(gone) - _NAMED_RUNS} more"
                if len(gone) > _NAMED_RUNS else "")
        log.warning("%s: %d loaded run(s) have no bronze under %s any more "
                    "(%s%s); this rebuild drops their rows", db_path,
                    len(gone), bronze_dir, named, more)
    return gone


def rebuild_advice(conn: sqlite3.Connection,
                   advice: Sequence[tuple[int, str]],
                   command: str, *,
                   bronze_dir: Path | None = None) -> list[str]:
    """Log, and return, one line per `advice` entry this silver lacks.

    `advice` is the loader's ``REBUILD_ADVICE``: ``(level, what a rebuild
    fills)`` pairs, a level per entry, extended by hand in the change that
    leaves old rows empty. Called after the migrations and before the
    first run of a load.

    A silver that has loaded no run is being built from empty: it records
    the highest level of `advice` and gets no line. A silver whose level
    is below an entry's gets that entry's line, naming `command`; the
    line repeats at every load until a rebuild records the level. Given
    the `bronze_dir` the rebuild reads, a line also counts the loaded
    runs whose bronze is gone, whose rows that rebuild drops. The load
    goes on either way. Without the `rebuild_level` table (see
    REBUILD_LEVEL_DDL) nothing is advised.
    """
    if not advice or not silver.has_table(conn, "rebuild_level"):
        return []
    level = conn.execute("SELECT MAX(level) FROM rebuild_level").fetchone()[0]
    if level is None and not _loaded_a_run(conn):
        conn.execute("INSERT INTO rebuild_level (level) VALUES (?)",
                     (max(n for n, _ in advice),))
        conn.commit()
        return []
    owed = [what for n, what in sorted(advice) if n > (level or 0)]
    gone = (_loaded_runs_gone(conn, bronze_dir)
            if owed and bronze_dir is not None else [])
    loss = (f", but drops the rows of {len(gone)} loaded run(s) whose "
            f"bronze is gone" if gone else "")
    lines = [f"this silver was built before it held {what}; `{command}` "
             f"rebuilds it from bronze and fills it{loss}" for what in owed]
    for line in lines:
        log.warning("%s", line)
    return lines


def _discard(path: Path, *, main: bool = True) -> None:
    sidecars = silver.SQLITE_SIDECARS
    for sidecar in ("", *sidecars) if main else sidecars:
        path.with_name(path.name + sidecar).unlink(missing_ok=True)


def migration_path_stop(conn: sqlite3.Connection, migrations: Path,
                        where) -> None:
    """Stop (:func:`no_migration_path`) when the silver on `conn` is at a
    schema version above the last of the `NNNN_*.sql` files in
    `migrations`. Reads only, so a stop leaves the silver as it was."""
    known = max((n for n, _ in silver.migration_files(migrations)),
                default=0)
    found = silver.current_schema_version(conn)
    if found > known:
        no_migration_path(where, found, known)


def rebuild(db_path: Path, migrations: Path,
            replay: Callable[[sqlite3.Connection, int], T], *,
            open_db: Callable[[Path], sqlite3.Connection] = silver.open_db,
            ) -> T:
    """Build the silver at `db_path` afresh and return what `replay`
    returns.

    The new silver is built at ``<db>.rebuild``: opened with `open_db`,
    migrated from empty, then ``replay(conn, version)`` loads every run
    into it. Only once that returns does it replace the old file, in one
    rename, so a silver is in place at every moment. The old file's
    sidecars are removed just before. When anything raises, the fresh
    file is removed and the old one is left as it was, byte for byte.
    """
    db_path = Path(db_path)
    fresh = db_path.with_name(db_path.name + ".rebuild")
    _discard(fresh)
    try:
        conn = open_db(fresh)
        try:
            result = replay(conn, silver.apply_migrations(conn, migrations))
        finally:
            conn.close()
        _discard(db_path, main=False)
        os.replace(fresh, db_path)
    except BaseException:
        _discard(fresh)
        raise
    return result
