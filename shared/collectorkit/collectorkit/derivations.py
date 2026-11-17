"""Rows derived from a document, per document and parser generation.

A pass that parses documents into rows records, per document and scope,
the parser generation that derived them (`document_derivations`). A load
then derives only what has no record under the current generation:

* a routine load, with no new document and no parser change, derives
  nothing;
* a new document is derived once;
* a parser change, a new generation for its scope, re-derives that
  scope's documents and no other scope's.

The protocol per scope, inside the caller's transaction:

1. ``pending_derivations(conn, scope, generation, documents_sql)`` lists
   the documents to derive, and logs the scope and their count;
2. per document, the caller replaces the rows it derived before (rows
   carry their document: a ``source_sha256`` or a source tag) with what
   the parser returns now;
3. ``record_derivation(...)`` then records the document under this
   generation. A document that yields nothing, a failed parse included,
   is recorded with ``row_count`` 0, so only a new generation reads it
   again.

:func:`generation_moved` tells a pass whether its parser has changed
since the scope was last derived.

``collectorkit.silver.stale_generation`` is the older per-scope form,
which re-derives a whole scope; a new collector uses this module.

The table belongs to each collector's own migrations (collectorkit owns
no schema): a collector that derives creates it with
:data:`DOCUMENT_DERIVATIONS_DDL` as it stands.

Imported as ``from collectorkit import derivations``; it is not imported
by the package itself.
"""
from __future__ import annotations

import logging
import sqlite3
from collections.abc import Sequence

log = logging.getLogger(__name__)

DOCUMENT_DERIVATIONS_DDL = """\
CREATE TABLE document_derivations (
    content_sha256 TEXT    NOT NULL,
    scope          TEXT    NOT NULL,
    generation     TEXT    NOT NULL,
    derived_at     INTEGER NOT NULL,
    row_count      INTEGER NOT NULL,
    PRIMARY KEY (content_sha256, scope)
)"""


def pending_derivations(conn: sqlite3.Connection, scope: str,
                        generation: str, documents_sql: str,
                        params: Sequence = ()) -> list:
    """The documents of `documents_sql` that `scope` has not derived under
    `generation`, in the query's order.

    `documents_sql` selects one row per document, its content sha256 in
    the first column; the rows come back as the query returns them. A
    document derived under another generation is pending again. When any
    is pending, one line names the scope, the count and the generation
    before the caller starts.
    """
    done = {sha for (sha,) in conn.execute(
        "SELECT content_sha256 FROM document_derivations "
        "WHERE scope = ? AND generation = ?", (scope, generation))}
    pending = [row for row in conn.execute(documents_sql, params)
               if row[0] not in done]
    if pending:
        log.info("%s: deriving %d document(s) under parser generation %s",
                 scope, len(pending), generation)
    return pending


def record_derivation(conn: sqlite3.Connection, content_sha256: str,
                      scope: str, generation: str, row_count: int) -> None:
    """Record that `scope` derived `row_count` rows from the document
    under `generation`. Called once that document's rows were replaced,
    in the same transaction, so a pass that dies half-way leaves the
    document pending."""
    conn.execute(
        "INSERT INTO document_derivations (content_sha256, scope, "
        "generation, derived_at, row_count) "
        "VALUES (?, ?, ?, CAST(strftime('%s','now') AS INTEGER), ?) "
        "ON CONFLICT(content_sha256, scope) DO UPDATE SET "
        "generation = excluded.generation, "
        "derived_at = excluded.derived_at, row_count = excluded.row_count",
        (content_sha256, scope, generation, row_count))


def generation_moved(conn: sqlite3.Connection, scope: str,
                     generation: str) -> bool:
    """Whether `scope` holds a document derived under a generation other
    than `generation`: its parser has changed since. A document with no
    record is new, not moved."""
    return conn.execute(
        "SELECT 1 FROM document_derivations WHERE scope = ? "
        "AND generation != ? LIMIT 1", (scope, generation)).fetchone() \
        is not None
