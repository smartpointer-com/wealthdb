"""The helpers a document-downloading collector shares: the class table
(docdedup.classify), the ISO date gate (parse.iso_date), the pending-run
listing (bronze.pending_run_dirs) and the silver document index
(documents.index_documents). Synthetic fixtures only."""
from __future__ import annotations

import json
import logging
import sqlite3
from datetime import date

import pytest

from collectorkit import bronze, docdedup, documents, parse

# ---- docdedup.classify ---------------------------------------------------

_TABLE = ((docdedup.CLASS_TAX, {"K1"}),
          (docdedup.CLASS_MUTABLE, {"K1", "STATEMENT"}),
          (docdedup.CLASS_IMMUTABLE, {"AGREEMENT"}))


@pytest.mark.parametrize("value, want", [
    ("K1", docdedup.CLASS_TAX),          # the first row naming it wins
    ("STATEMENT", docdedup.CLASS_MUTABLE),
    ("AGREEMENT", docdedup.CLASS_IMMUTABLE),
    ("NOTICE", None),                    # unclassified: fetch-verify
    ("", None),
    (None, None),
])
def test_classify_exact(value, want):
    assert docdedup.classify(value, _TABLE) == want


def test_classify_substring():
    table = ((docdedup.CLASS_TAX, {"1099", "tax"}),
             (docdedup.CLASS_IMMUTABLE, {"annual"}))
    assert docdedup.classify("annual tax summary", table, substring=True) \
        == docdedup.CLASS_TAX
    assert docdedup.classify("annual report", table, substring=True) \
        == docdedup.CLASS_IMMUTABLE
    assert docdedup.classify("annual tax summary", table) is None


# ---- parse.iso_date ------------------------------------------------------

@pytest.mark.parametrize("value, want", [
    ("2098-03-04", date(2098, 3, 4)),
    ("2098-03-04T05:06:07.890", date(2098, 3, 4)),
    ("2098-02-30", None),
    ("2098-03", None),
    ("", None),
    (None, None),
    (20980304, None),
])
def test_iso_date(value, want):
    assert parse.iso_date(value) == want


# ---- bronze.pending_run_dirs ---------------------------------------------

def _run(root, slug, status=..., manifest=True):
    d = root / slug
    d.mkdir()
    if manifest:
        body = {} if status is ... else {"status": status}
        (d / "run.json").write_text(json.dumps(body), encoding="utf-8")
    return d


def test_pending_run_dirs(tmp_path, caplog):
    conn = sqlite3.connect(":memory:")
    conn.execute("CREATE TABLE dump_runs (snapshot_at INTEGER PRIMARY KEY)")
    conn.execute("INSERT INTO dump_runs VALUES (?)",
                 (bronze.parse_run_ts("20980101T000000Z"),))
    _run(tmp_path, "20980101T000000Z")                     # loaded
    done = _run(tmp_path, "20980102T000000Z", "complete")
    _run(tmp_path, "20980103T000000Z", manifest=False)     # still writing
    _run(tmp_path, "20980104T000000Z", "in-progress")
    _run(tmp_path, "20980105T000000Z", "dry-run")
    statusless = _run(tmp_path, "20980106T000000Z")
    corrupt = _run(tmp_path, "20980107T000000Z", manifest=False)
    (corrupt / "run.json").write_text("{not json", encoding="utf-8")
    (tmp_path / "not-a-run").mkdir()

    log = logging.getLogger("test_pending")
    with caplog.at_level(logging.INFO, logger="test_pending"):
        got = bronze.pending_run_dirs(conn, tmp_path, log=log)
    assert got == [done, statusless, corrupt]
    assert "no run.json" in caplog.text
    assert "status=in-progress" in caplog.text and "status=dry-run" in caplog.text


def test_pending_run_dirs_without_a_bronze_dir(tmp_path):
    conn = sqlite3.connect(":memory:")
    assert bronze.pending_run_dirs(conn, tmp_path / "absent",
                                   log=logging.getLogger("x")) == []


# ---- documents.index_documents -------------------------------------------

def _db(unique_id: bool):
    conn = sqlite3.connect(":memory:")
    conn.execute(f"""
        CREATE TABLE documents (
            content_sha256 TEXT PRIMARY KEY,
            doc_id TEXT {"UNIQUE" if unique_id else ""},
            label TEXT, file_size INTEGER, bronze_path TEXT,
            first_seen_at INTEGER, last_seen_at INTEGER, payload TEXT)""")
    return conn


def _pdf(run_dir, doc_id, body: bytes):
    d = run_dir / "documents"
    d.mkdir(parents=True, exist_ok=True)
    (d / f"{doc_id}.pdf").write_bytes(body)


def _index(conn, snapshot_at, run_dir, entries, **kw):
    return documents.index_documents(
        conn, snapshot_at, run_dir, entries,
        id_of=lambda e: e.get("id") or None,
        columns_of=lambda e, doc_id, size: {
            "doc_id": doc_id, "label": e.get("label"), "file_size": size},
        **kw)


def test_index_inserts_refreshes_and_counts_missing(tmp_path):
    conn = _db(unique_id=False)
    run1 = tmp_path / "20980101T000000Z"
    _pdf(run1, "a", b"%PDF-a")
    n = _index(conn, 100, run1, [{"id": "a", "label": "one"},
                                 {"id": "b"},          # indexed, not on disk
                                 {"id": ""}])          # no id: skipped
    assert (n.inserted, n.refreshed, n.missing, n.restated) == (1, 0, 1, 0)
    row = conn.execute("SELECT doc_id, label, file_size, bronze_path, "
                       "first_seen_at, last_seen_at, payload FROM documents").fetchone()
    assert row == ("a", "one", 6, "20980101T000000Z/documents/a.pdf", 100, 100,
                   '{"id":"a","label":"one"}')

    # The same bytes in a later run refresh the row in place.
    run2 = tmp_path / "20980102T000000Z"
    _pdf(run2, "a", b"%PDF-a")
    n = _index(conn, 200, run2, [{"id": "a", "label": "renamed"}])
    assert (n.inserted, n.refreshed) == (0, 1)
    assert conn.execute("SELECT label, bronze_path, first_seen_at, last_seen_at, "
                        "payload FROM documents").fetchone() == (
        "one", "20980102T000000Z/documents/a.pdf", 100, 200,
        '{"id":"a","label":"renamed"}')


def test_index_supersedes_a_restated_document(tmp_path):
    conn = _db(unique_id=True)
    run1 = tmp_path / "20980101T000000Z"
    _pdf(run1, "a", b"%PDF-first")
    _index(conn, 100, run1, [{"id": "a"}], supersede_on="doc_id")
    run2 = tmp_path / "20980102T000000Z"
    _pdf(run2, "a", b"%PDF-second")
    n = _index(conn, 200, run2, [{"id": "a"}], supersede_on="doc_id")
    assert (n.inserted, n.restated) == (1, 1)
    assert conn.execute("SELECT COUNT(*), MIN(first_seen_at) FROM documents"
                        ).fetchone() == (1, 200)


def test_index_without_supersede_keeps_both_versions(tmp_path):
    conn = _db(unique_id=False)
    for snap, slug, body in ((100, "20980101T000000Z", b"%PDF-1"),
                             (200, "20980102T000000Z", b"%PDF-2")):
        _pdf(tmp_path / slug, "a", body)
        _index(conn, snap, tmp_path / slug, [{"id": "a"}])
    assert conn.execute("SELECT COUNT(*) FROM documents").fetchone() == (2,)
