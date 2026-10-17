"""The silver DB the ubs-web loader tests write into.

Migrated by the production runner (`silver.apply_migrations`), so a test
runs on the schema a real load builds, `schema_meta` included.
"""
from __future__ import annotations

import sqlite3
from pathlib import Path

from collectorkit import silver

MIGRATIONS_DIR = Path(__file__).resolve().parent.parent / "migrations"


def fresh_db(directory: Path) -> sqlite3.Connection:
    """A migrated `ubs-web.db` in `directory`, rows readable by name."""
    conn = sqlite3.connect(str(Path(directory) / "ubs-web.db"))
    conn.row_factory = sqlite3.Row
    silver.apply_migrations(conn, MIGRATIONS_DIR)
    return conn
