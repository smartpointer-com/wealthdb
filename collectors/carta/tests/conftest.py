"""Shared test setup for carta.

Puts the collector dir on `sys.path` so the tests can import the loader's
modules, `download` and `prune`, and provides the migrated in-memory
silver, one run loaded through the load frame, and the bronze documents
dir several suites build. Synthetic data only.
"""
from __future__ import annotations

import dataclasses
import json
import sqlite3
import sys
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import load  # noqa: E402
from collectorkit import extract, loader, silver, testpdf  # noqa: E402

MIGRATIONS_DIR = Path(__file__).resolve().parent.parent / "migrations"


@pytest.fixture(autouse=True)
def extract_dir(tmp_path, monkeypatch) -> Path:
    """The loader's extraction cache, inside the test's own directory."""
    monkeypatch.setenv("WEALTHDB_CACHE_DIR", str(tmp_path / "cache"))
    return extract.default_dir("carta")


@pytest.fixture
def migrated():
    """Fresh in-memory silver with every migration applied."""
    c = sqlite3.connect(":memory:")
    c.execute("PRAGMA foreign_keys = ON")
    silver.apply_migrations(c, MIGRATIONS_DIR)
    yield c
    c.close()


def load_one(conn, run_dir: Path) -> bool:
    """Load one bronze run through the load frame, as `load` does each
    pending run, into `conn` (migrated). True when the run loaded; False
    when the frame passed over it, which writes no `dump_runs` row."""
    spec = dataclasses.replace(
        load.SPEC, pending=lambda _conn, _bronze, log=None: [run_dir])
    run = loader.Load(args=None, silver_db=None, bronze_dir=run_dir.parent,
                      log=load.log,
                      version=silver.current_schema_version(conn))
    loader.load_runs(spec, conn, run)
    return run.loaded == 1


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


def pdf_bytes(pages: list[list[tuple[float, float, str]]]) -> bytes:
    """A minimal PDF in one font, Helvetica at 8 points: per page, (x, y,
    text) triples, y measured down from the top of a letter page to the
    baseline. pdftotext reads it as any other PDF."""
    return testpdf.from_top(pages, size=8)


def text_page(text: str) -> list[tuple[float, float, str]]:
    """`text` as one PDF page that `pdftotext -layout` gives back line by
    line: each run of characters between two or more spaces is placed at
    its column."""
    out = []
    for i, line in enumerate(text.splitlines()):
        col = 0
        for part in line.split("  "):
            if part.strip():
                out.append((40 + 4.6 * col, 60 + 12 * i, part.strip()))
            col += len(part) + 2
    return out

