"""Shared test setup for carta.

Puts the collector dir on `sys.path` so the tests can `import load`,
`download`, `k1` and `prune`, and provides the migrated in-memory silver,
one run loaded through the load frame, and the bronze documents dir
several suites build. Synthetic data only.
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
from collectorkit import loader, silver  # noqa: E402

MIGRATIONS_DIR = Path(__file__).resolve().parent.parent / "migrations"


@pytest.fixture(autouse=True)
def extract_dir(tmp_path, monkeypatch) -> Path:
    """The loader's extraction cache, inside the test's own directory."""
    path = tmp_path / "extract-cache"
    monkeypatch.setattr(load, "_extract_dir", lambda: path)
    return path


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
    objs = [b"<</Type/Catalog/Pages 2 0 R>>",
            b"<</Type/Pages/Kids[%s]/Count %d>>" % (
                b" ".join(b"%d 0 R" % (4 + 2 * i) for i in range(len(pages))),
                len(pages)),
            b"<</Type/Font/Subtype/Type1/BaseFont/Helvetica"
            b"/Encoding/WinAnsiEncoding>>"]
    for i, words in enumerate(pages):
        body = b"".join(
            b"BT /F1 8 Tf %g %g Td (%s) Tj ET\n" % (
                x, 792 - y, t.encode("cp1252").replace(b"\\", b"\\\\")
                .replace(b"(", b"\\(").replace(b")", b"\\)"))
            for x, y, t in words)
        objs.append(b"<</Type/Page/Parent 2 0 R/MediaBox[0 0 612 792]"
                    b"/Resources<</Font<</F1 3 0 R>>>>/Contents %d 0 R>>"
                    % (5 + 2 * i))
        objs.append(b"<</Length %d>>stream\n%sendstream" % (len(body), body))
    out = bytearray(b"%PDF-1.4\n")
    offsets = []
    for n, obj in enumerate(objs, 1):
        offsets.append(len(out))
        out += b"%d 0 obj\n%s\nendobj\n" % (n, obj)
    xref = len(out)
    out += b"xref\n0 %d\n0000000000 65535 f \n" % (len(objs) + 1)
    out += b"".join(b"%010d 00000 n \n" % o for o in offsets)
    out += b"trailer\n<</Size %d/Root 1 0 R>>\nstartxref\n%d\n%%%%EOF\n" % (
        len(objs) + 1, xref)
    return bytes(out)


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

