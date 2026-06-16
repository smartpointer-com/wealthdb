"""Silver SQLite plumbing shared by every collector's load.py.

All collectors use the same conventions: a `schema_meta` table whose
`silver_schema_version` column tracks the applied schema version, and
`NNNN_*.sql` migration files that each end by inserting their own version
into `schema_meta`.
"""
from __future__ import annotations

import logging
import re
import sqlite3
from pathlib import Path

log = logging.getLogger(__name__)

# Migration filenames look like `0001_initial.sql`, `12_add_x.sql`, etc.
MIGRATION_FILE_RE = re.compile(r"^(\d+)_.*\.sql$")


def reset(path: Path) -> None:
    """Delete the silver DB at `path` (with its SQLite ``-wal`` / ``-shm`` /
    ``-journal`` or DuckDB ``.wal`` sidecars) so the next load rebuilds it
    from scratch. This is the uniform implementation behind ``load --force``:
    silver is reproducible from bronze alone, so a clean rebuild is always
    safe and sidesteps every collector's own skip / upsert logic. No-op when
    the DB doesn't exist yet.
    """
    path = Path(path)
    for sidecar in ("", "-wal", "-shm", "-journal", ".wal"):
        p = path.with_name(path.name + sidecar) if sidecar else path
        try:
            p.unlink()
        except FileNotFoundError:
            pass
    log.info("reset (force): cleared silver DB %s", path)


def open_db(path: Path) -> sqlite3.Connection:
    """Open (creating parent dirs) the silver DB with manual transaction
    control (`isolation_level=None`): callers issue BEGIN/COMMIT per dump
    so each source load is atomic. Foreign keys and WAL are per-connection
    PRAGMAs, re-set on every connection.

    Provided for collectors that already use this model; collectors with a
    different transaction style keep their own open_db. `apply_migrations`
    below works regardless of which model the passed connection uses.
    """
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(str(path), isolation_level=None)
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA foreign_keys = ON;")
    conn.execute("PRAGMA journal_mode = WAL;")
    return conn


def current_schema_version(conn: sqlite3.Connection) -> int:
    """MAX(silver_schema_version) from schema_meta, or 0 if it's absent."""
    row = conn.execute(
        "SELECT name FROM sqlite_master "
        "WHERE type='table' AND name='schema_meta'"
    ).fetchone()
    if not row:
        return 0
    row = conn.execute(
        "SELECT COALESCE(MAX(silver_schema_version), 0) AS v FROM schema_meta"
    ).fetchone()
    return int(row[0])


def apply_migrations(conn: sqlite3.Connection, migrations_dir: Path) -> int:
    """Apply `NNNN_*.sql` migrations newer than the on-disk version, in
    numeric order, and return the final schema version.

    Each migration must end by inserting its own `silver_schema_version`
    into `schema_meta`; we verify the version advanced and fail loudly if
    not (a migration that forgets would otherwise re-run forever).

    Transaction-model agnostic: we `commit()` after each migration, which
    persists the change under default-isolation connections and is a no-op
    under manual (`isolation_level=None`) connections. Migrations are
    independent of per-dump load atomicity, which each collector still
    manages itself.
    """
    migrations_dir = Path(migrations_dir)
    if not migrations_dir.is_dir():
        raise SystemExit(f"Migrations dir not found: {migrations_dir}")
    files = []
    for f in migrations_dir.iterdir():
        m = MIGRATION_FILE_RE.match(f.name)
        if m:
            files.append((int(m.group(1)), f))
    files.sort()
    current = current_schema_version(conn)
    log.info("Schema version on disk: %d; %d migration file(s) found",
             current, len(files))
    for n, path in files:
        if n <= current:
            continue
        log.info("Applying migration %s", path.name)
        # In autocommit mode executescript() commits each statement; a
        # crash mid-migration leaves a partial schema (recovery: delete
        # the .db and re-run from bronze).
        conn.executescript(path.read_text(encoding="utf-8"))
        conn.commit()  # persist under default isolation; no-op under manual
        applied = current_schema_version(conn)
        if applied < n:
            raise SystemExit(
                f"migration {path.name} did not advance schema_meta "
                f"(still {applied}); every migration must INSERT its "
                f"silver_schema_version"
            )
        current = applied
    log.info("Schema version after migrations: %d", current)
    return current


def loaded_snapshots(conn: sqlite3.Connection) -> set[int]:
    """Set of `snapshot_at` values already recorded in `dump_runs`, so a
    loader can skip bronze runs it has already ingested. Empty if the
    table doesn't exist yet.
    """
    row = conn.execute(
        "SELECT name FROM sqlite_master "
        "WHERE type='table' AND name='dump_runs'"
    ).fetchone()
    if not row:
        return set()
    return {
        int(r[0])
        for r in conn.execute("SELECT snapshot_at FROM dump_runs").fetchall()
    }
