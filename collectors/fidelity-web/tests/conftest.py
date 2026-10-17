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
