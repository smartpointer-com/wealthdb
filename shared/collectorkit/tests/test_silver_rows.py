"""Unit tests for the silver row writers: `silver.upsert_rows` and
`silver.record_document`. Synthetic rows only."""
from __future__ import annotations

import sqlite3

import pytest

from collectorkit import silver

COLUMNS = ("k", "a", "b")


@pytest.fixture
def conn():
    c = sqlite3.connect(":memory:")
    c.execute("CREATE TABLE t (k TEXT PRIMARY KEY, a INTEGER, b TEXT)")
    c.execute(f"CREATE TABLE documents ({', '.join(silver.DOCUMENT_COLUMNS)},"
              " PRIMARY KEY (sha256))")
    return c


def _rows(conn):
    return conn.execute("SELECT k, a, b FROM t ORDER BY k").fetchall()


def test_upsert_rows_takes_sequences_and_mappings(conn):
    n = silver.upsert_rows(conn, "t", COLUMNS, [
        ("x", 1, "one"),
        {"b": "two", "k": "y", "a": 2, "ignored": "extra key"},
    ])
    assert n == 2
    assert _rows(conn) == [("x", 1, "one"), ("y", 2, "two")]


def test_upsert_rows_replaces_a_held_key(conn):
    silver.upsert_rows(conn, "t", COLUMNS, [("x", 1, "one")])
    silver.upsert_rows(conn, "t", COLUMNS, [("x", 9, "nine")])
    assert _rows(conn) == [("x", 9, "nine")]


def test_upsert_rows_without_replace_refuses_a_held_key(conn):
    silver.upsert_rows(conn, "t", COLUMNS, [("x", 1, "one")], replace=False)
    with pytest.raises(sqlite3.IntegrityError):
        silver.upsert_rows(conn, "t", COLUMNS, [("x", 9, "nine")],
                           replace=False)
    assert _rows(conn) == [("x", 1, "one")]


def test_upsert_rows_writes_a_subset_of_columns(conn):
    silver.upsert_rows(conn, "t", ("k", "b"), [{"k": "x", "b": "one"}])
    assert _rows(conn) == [("x", None, "one")]


def test_upsert_rows_with_no_rows(conn):
    assert silver.upsert_rows(conn, "t", COLUMNS, iter(())) == 0
    assert _rows(conn) == []


def test_record_document_is_true_only_for_a_new_sha256(conn):
    row = ("ab" * 32, 1, "acct", 2, "statement", "pdf", "s.pdf", 10, "{}")
    assert silver.record_document(conn, row) is True
    again = dict(zip(silver.DOCUMENT_COLUMNS, row, strict=True),
                 filename="other.pdf")
    assert silver.record_document(conn, again) is False
    assert conn.execute("SELECT * FROM documents").fetchall() == [row]
