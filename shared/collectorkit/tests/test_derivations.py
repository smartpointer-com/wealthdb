"""Tests for `collectorkit.derivations`, the re-derive protocol per
document and parser generation. Invented documents only."""
from __future__ import annotations

import logging
import sqlite3

import pytest

from collectorkit import derivations, silver

DOCUMENTS = "SELECT sha256, body FROM documents ORDER BY sha256"


@pytest.fixture
def conn():
    c = sqlite3.connect(":memory:", isolation_level=None)
    c.execute(derivations.DOCUMENT_DERIVATIONS_DDL)
    c.execute("CREATE TABLE documents (sha256 TEXT PRIMARY KEY, body TEXT)")
    c.execute("CREATE TABLE lines (source_sha256 TEXT, scope TEXT, text TEXT)")
    c.executemany("INSERT INTO documents VALUES (?, ?)",
                  [("aa", "one two"), ("bb", "three")])
    return c


class Parser:
    """Splits a document into lines, counting its calls."""

    def __init__(self, log=None):
        self.calls = 0
        self.log = log

    def __call__(self, body):
        self.calls += 1
        if self.log:
            self.log.info("parsing")
        return body.split()


def derive(conn, scope, generation, parse):
    """The protocol as a collector's pass runs it."""
    with silver.transaction(conn):
        for sha, body in derivations.pending_derivations(
                conn, scope, generation, DOCUMENTS):
            conn.execute("DELETE FROM lines WHERE source_sha256 = ? AND "
                         "scope = ?", (sha, scope))
            rows = parse(body)
            conn.executemany("INSERT INTO lines VALUES (?, ?, ?)",
                             [(sha, scope, r) for r in rows])
            derivations.record_derivation(conn, sha, scope, generation,
                                          len(rows))


def lines(conn, scope):
    return conn.execute("SELECT source_sha256, text FROM lines WHERE "
                        "scope = ? ORDER BY 1, 2", (scope,)).fetchall()


def test_a_second_pass_derives_nothing(conn):
    parse = Parser()
    derive(conn, "words", "g1", parse)
    assert parse.calls == 2
    assert lines(conn, "words") == [("aa", "one"), ("aa", "two"),
                                    ("bb", "three")]
    derive(conn, "words", "g1", parse)
    assert parse.calls == 2
    assert conn.execute("SELECT content_sha256, scope, generation, row_count "
                        "FROM document_derivations ORDER BY 1").fetchall() == [
        ("aa", "words", "g1", 2), ("bb", "words", "g1", 1)]


def test_a_new_document_is_derived_alone(conn):
    parse = Parser()
    derive(conn, "words", "g1", parse)
    conn.execute("INSERT INTO documents VALUES ('cc', 'four')")
    derive(conn, "words", "g1", parse)
    assert parse.calls == 3


def test_a_changed_generation_re_derives_exactly_its_scope(conn):
    words, other = Parser(), Parser()
    derive(conn, "words", "g1", words)
    derive(conn, "other", "h1", other)
    derive(conn, "words", "g2", lambda body: words(body.upper()))
    derive(conn, "other", "h1", other)
    assert (words.calls, other.calls) == (4, 2)
    assert lines(conn, "words") == [("aa", "ONE"), ("aa", "TWO"),
                                    ("bb", "THREE")]
    assert lines(conn, "other") == [("aa", "one"), ("aa", "two"),
                                    ("bb", "three")]


def test_the_log_names_the_scope_and_count_before_it_starts(conn, caplog):
    log = logging.getLogger("toy.load")
    with caplog.at_level(logging.INFO):
        derive(conn, "words", "g1", Parser(log))
    assert caplog.messages[0] == (
        "words: deriving 2 document(s) under parser generation g1")
    assert caplog.messages[1:] == ["parsing", "parsing"]
    caplog.clear()
    with caplog.at_level(logging.INFO):
        derive(conn, "words", "g1", Parser(log))
    assert caplog.messages == []


def test_a_pass_that_dies_leaves_its_documents_pending(conn):
    def dies(body):
        if body == "three":
            raise RuntimeError("a parser that fails")
        return body.split()

    with pytest.raises(RuntimeError):
        derive(conn, "words", "g1", dies)
    assert lines(conn, "words") == []
    assert len(derivations.pending_derivations(conn, "words", "g1",
                                               DOCUMENTS)) == 2


def test_a_moved_generation_is_seen_per_scope(conn):
    assert not derivations.generation_moved(conn, "lines", "g1")
    derive(conn, "lines", "g1", Parser())
    derive(conn, "other", "g7", Parser())
    assert not derivations.generation_moved(conn, "lines", "g1")
    assert derivations.generation_moved(conn, "lines", "g2")
    assert not derivations.generation_moved(conn, "other", "g7")
