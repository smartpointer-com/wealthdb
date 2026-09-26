"""The silver ``documents`` index a run's downloaded PDFs are recorded in.

A collector that downloads documents keeps them on disk under
``<run>/documents/<id>.pdf`` and indexes them in a silver ``documents``
table keyed by ``content_sha256``: the bytes are the row's identity, the
provider's document id and its metadata ride along, and ``bronze_path``
points at the newest copy (relative to the bronze root, so the row
resolves inside a container and on the host alike). This module writes
that index; the columns beyond the shared ones are the collector's.
"""
from __future__ import annotations

import sqlite3
from collections.abc import Callable, Iterable
from dataclasses import dataclass
from pathlib import Path

from . import bronze
from .silver import canonical_json


@dataclass
class IndexCounts:
    """What one :func:`index_documents` pass did."""
    inserted: int = 0   # new content
    restated: int = 0   # rows a re-issued document superseded
    refreshed: int = 0  # content already indexed, seen again
    missing: int = 0    # indexed by the source, not on disk


def index_documents(
    conn: sqlite3.Connection,
    snapshot_at: int,
    run_dir: Path,
    entries: Iterable[dict],
    *,
    id_of: Callable[[dict], object],
    columns_of: Callable[[dict, object, int], dict],
    supersede_on: str | None = None,
) -> IndexCounts:
    """Index the run's downloaded documents in the ``documents`` table.

    Each entry of the source's document index is one document: `id_of`
    gives its id (None skips the entry) and `columns_of(entry, doc_id,
    file_size)` the collector's own columns for a new row. An entry whose
    ``documents/<id>.pdf`` is not on disk is counted missing.

    Content already indexed has its ``last_seen_at``, ``bronze_path`` and
    ``payload`` refreshed. New content is inserted; where the table allows
    one row per document id, `supersede_on` names that id column, and the
    row an earlier version left there gives way first (a restatement).
    """
    counts = IndexCounts()
    for entry in entries:
        doc_id = id_of(entry)
        if doc_id is None:
            continue
        pdf_path = run_dir / "documents" / f"{doc_id}.pdf"
        if not pdf_path.is_file():
            counts.missing += 1
            continue
        sha, size = bronze.sha256_file(pdf_path)
        bronze_path = pdf_path.relative_to(run_dir.parent).as_posix()
        payload = canonical_json(entry)
        known = conn.execute(
            "SELECT 1 FROM documents WHERE content_sha256 = ?", (sha,)).fetchone()
        if known is not None:
            conn.execute(
                """
                UPDATE documents SET
                    last_seen_at = MAX(last_seen_at, ?),
                    bronze_path = ?,
                    payload = ?
                WHERE content_sha256 = ?
                """,
                (snapshot_at, bronze_path, payload, sha),
            )
            counts.refreshed += 1
            continue
        if supersede_on is not None:
            counts.restated += conn.execute(
                f"DELETE FROM documents WHERE {supersede_on} = ?",
                (doc_id,)).rowcount
        row = {
            "content_sha256": sha,
            **columns_of(entry, doc_id, size),
            "bronze_path": bronze_path,
            "first_seen_at": snapshot_at,
            "last_seen_at": snapshot_at,
            "payload": payload,
        }
        conn.execute(
            f"INSERT INTO documents ({', '.join(row)}) "
            f"VALUES ({', '.join('?' * len(row))})",
            tuple(row.values()),
        )
        counts.inserted += 1
    return counts
