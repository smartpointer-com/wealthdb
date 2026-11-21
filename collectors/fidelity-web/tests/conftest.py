"""Fixtures shared by the fidelity-web test modules."""

from __future__ import annotations

import sqlite3
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import load  # noqa: E402

MIGRATIONS_DIR = Path(__file__).resolve().parent.parent / "migrations"


@pytest.fixture
def migrated():
    """An in-memory silver DB at the current schema, with foreign keys
    on as `load.main` runs it."""
    c = sqlite3.connect(":memory:")
    c.execute("PRAGMA foreign_keys = ON")
    load.silver.apply_migrations(c, MIGRATIONS_DIR)
    yield c
    c.close()


def undo_document_derivations(conn):
    """Put a silver back as the code before migration 0016 left it: each
    scope's one generation stamped in parser_generations, as its pass
    wrote it, and no document_derivations."""
    assert all(n == 1 for (n,) in conn.execute(
        "SELECT COUNT(DISTINCT generation) FROM document_derivations "
        "GROUP BY scope"))
    conn.executescript("""
        CREATE TABLE parser_generations (
            scope      TEXT    NOT NULL PRIMARY KEY,
            generation TEXT    NOT NULL,
            stamped_at INTEGER NOT NULL
        );
        INSERT INTO parser_generations
            SELECT scope, MIN(generation), 0 FROM document_derivations
             GROUP BY scope;
        DROP TABLE document_derivations;
        DELETE FROM schema_meta WHERE silver_schema_version = 16;
    """)
