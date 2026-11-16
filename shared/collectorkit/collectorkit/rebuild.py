"""What stops a load, what a load only advises, and the one rebuild path.

A load is incremental and keeps every loaded silver loadable. It stops for
two reasons only, both raised by :func:`stop` so every collector words
them the same way:

* corruption found in loaded data (:func:`corruption`): an SQLite error
  while loading, or an invariant over stored rows that the loader checks;
* a silver the code has no migration path from (:func:`no_migration_path`;
  :func:`migration_path_stop` checks for a silver newer than the code).

A stop leaves silver as it was, readable.

A change whose new column only bronze can fill for old rows does not stop
a load. It adds an entry to the loader's ``REBUILD_ADVICE``, and
:func:`rebuild_advice` prints one line for it at every load of a silver
built before the entry, until a rebuild.

:func:`rebuild` is that rebuild: a fresh silver beside the old one, put in
place only once every run is in.

Imported as ``from collectorkit import rebuild``; it is not imported by
the package itself.
"""
from __future__ import annotations

import logging
import os
import sqlite3
from collections.abc import Callable, Sequence
from pathlib import Path
from typing import NoReturn, TypeVar

from collectorkit import silver

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


def corruption(where, check: str, table: str, command: str) -> NoReturn:
    """Stop for corruption: the check `check` does not hold over the rows
    of `table`."""
    stop(where, f"{check} does not hold in {table}", command=command)


def no_migration_path(where, found: int, known: int,
                      command: str) -> NoReturn:
    """Stop for a silver at schema version `found` when this code's
    migrations reach `known`. A newer silver names the upgrade; an older
    one that no migration starts from names the rebuild `command`."""
    if found > known:
        stop(where, f"silver is at schema version {found}, newer than the "
                    f"{known} this code knows", upgrade=True)
    stop(where, f"silver is at schema version {found}, which no migration "
                f"of this code starts from (it knows up to {known})",
         command=command)


def _has_table(conn: sqlite3.Connection, name: str) -> bool:
    return conn.execute(
        "SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?",
        (name,)).fetchone() is not None


def _loaded_a_run(conn: sqlite3.Connection) -> bool:
    return (_has_table(conn, "dump_runs") and conn.execute(
        "SELECT 1 FROM dump_runs LIMIT 1").fetchone() is not None)


def rebuild_advice(conn: sqlite3.Connection,
                   advice: Sequence[tuple[int, str]],
                   command: str) -> list[str]:
    """Log, and return, one line per `advice` entry this silver lacks.

    `advice` is the loader's ``REBUILD_ADVICE``: ``(level, what a rebuild
    fills)`` pairs, a level per entry, extended by hand in the change that
    leaves old rows empty. Called after the migrations and before the
    first run of a load.

    A silver that has loaded no run is being built from empty: it records
    the highest level of `advice` and gets no line. A silver whose level
    is below an entry's gets that entry's line, naming `command`; the
    line repeats at every load until a rebuild records the level. The
    load goes on either way. Without the `rebuild_level` table (see
    REBUILD_LEVEL_DDL) nothing is advised.
    """
    if not advice or not _has_table(conn, "rebuild_level"):
        return []
    level = conn.execute("SELECT MAX(level) FROM rebuild_level").fetchone()[0]
    if level is None and not _loaded_a_run(conn):
        conn.execute("INSERT INTO rebuild_level (level) VALUES (?)",
                     (max(n for n, _ in advice),))
        conn.commit()
        return []
    lines = [f"this silver was built before it held {what}; `{command}` "
             f"rebuilds it from bronze and fills it"
             for n, what in sorted(advice) if n > (level or 0)]
    for line in lines:
        log.warning("%s", line)
    return lines


SIDECARS = ("-wal", "-shm", "-journal")


def _discard(path: Path, *, main: bool = True) -> None:
    for sidecar in ("", *SIDECARS) if main else SIDECARS:
        path.with_name(path.name + sidecar).unlink(missing_ok=True)


def migration_path_stop(conn: sqlite3.Connection, migrations: Path,
                        where, command: str) -> None:
    """Stop (:func:`no_migration_path`) when the silver on `conn` is at a
    schema version above the last of the `NNNN_*.sql` files in
    `migrations`. Reads only, so a stop leaves the silver as it was."""
    known = max((int(m.group(1)) for f in Path(migrations).iterdir()
                 if (m := silver.MIGRATION_FILE_RE.match(f.name))), default=0)
    found = silver.current_schema_version(conn)
    if found > known:
        no_migration_path(where, found, known, command)


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
