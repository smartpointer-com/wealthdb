"""Shared test setup for carta.

Puts the collector dir on `sys.path` so the tests can `import load`,
`download`, `k1` and `prune`, and provides the migrated in-memory silver
and the bronze documents dir several suites build. Synthetic data only.
"""
from __future__ import annotations

import json
import sqlite3
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import load  # noqa: E402

MIGRATIONS_DIR = Path(__file__).resolve().parent.parent / "migrations"


@pytest.fixture
def migrated():
    """Fresh in-memory silver with every migration applied."""
    c = sqlite3.connect(":memory:")
    c.execute("PRAGMA foreign_keys = ON")
    load.silver.apply_migrations(c, MIGRATIONS_DIR)
    yield c
    c.close()


def docs_dir(root: Path, rows: list[dict], *, name: str = "documents") -> Path:
    """A documents dir `root/name` holding an index of `rows` and one stub
    `doc_<id>.pdf` per row. Each stub starts with the PDF magic number, which
    is all the loader checks before it parses; the parsers are stubbed per
    test, so the bytes past it never matter. A row without a
    `document_name` gets one in the index, since silver requires it."""
    docs = root / name
    docs.mkdir(parents=True, exist_ok=True)
    for row in rows:
        (docs / f"doc_{row['id']}.pdf").write_bytes(
            f"%PDF- {row['id']}".encode())
    indexed = [{"document_name": f"document {row['id']}", **row}
               for row in rows]
    (docs / "index.json").write_text(json.dumps({"results": indexed}))
    return docs
