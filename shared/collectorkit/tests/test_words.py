"""Tests for collectorkit.words: the document types and the word
geometry (rows, cell_of), over invented words."""
from __future__ import annotations

import pytest

from collectorkit.words import Extracted, Page, Word, cell_of, rows


def _word(text, x0, top, width=10.0, height=8.0):
    """A word in a top-down frame."""
    return Word(text, x0=x0, x1=x0 + width, top=top, bottom=top + height)


# ------------------------------------------------------------------
# rows
# ------------------------------------------------------------------

def _texts(lines):
    return [[w.text for w in row.words] for row in lines]


def test_a_word_at_the_tolerance_joins_the_line_and_one_past_opens_the_next():
    words = [_word("a", 10, 100.0), _word("b", 30, 102.0),
             _word("c", 50, 104.01)]
    lines = rows(words, 2.0, "first")
    assert _texts(lines) == [["a", "b"], ["c"]]
    assert [row.top for row in lines] == [100.0, 104.01]


def test_the_anchor_is_the_lines_first_word():
    # Each word is within the tolerance of the one before it, but the
    # third is beyond the first's.
    words = [_word("a", 10, 100.0), _word("b", 30, 101.5),
             _word("c", 50, 103.0)]
    assert _texts(rows(words, 2.0)) == [["a", "b"], ["c"]]


def test_a_line_reads_left_to_right_and_equal_x0_keeps_input_order():
    words = [_word("right", 80, 100.5), _word("tie-1", 20, 101.0),
             _word("left", 5, 100.0), _word("tie-2", 20, 101.0)]
    assert _texts(rows(words, 2.0)) == [["left", "tie-1", "tie-2", "right"]]
    swapped = [words[0], words[3], words[2], words[1]]
    assert _texts(rows(swapped, 2.0)) == [["left", "tie-2", "tie-1", "right"]]


def test_lines_come_top_to_bottom_whatever_the_input_order():
    words = [_word("low", 10, 300.0), _word("high", 10, 100.0),
             _word("mid", 10, 200.0)]
    assert _texts(rows(words, 3.0)) == [["high"], ["mid"], ["low"]]
    assert rows([], 3.0) == []


def test_rows_has_one_anchor():
    with pytest.raises(ValueError):
        rows([_word("a", 0, 0)], 1.0, anchor="mean")


# ------------------------------------------------------------------
# The document types
# ------------------------------------------------------------------

def test_page_words_by_their_arguments():
    page = Page("t", (('{"x_tolerance":2}', (Word("a", 1, 2, 3, 4),)),))
    assert page.words(x_tolerance=2) == (Word("a", 1, 2, 3, 4),)
    with pytest.raises(KeyError):
        page.words()
    assert Word("a", 0, 1, 10, 14).mid == 12


def test_text_joins_as_the_engine_did():
    doc = Extracted("x", (Page("a"), Page(""), Page("b")))
    assert doc.text() == "a\n\nb"
    assert doc.text(" | ") == "a |  | b"
    assert Extracted("ocr-x", doc.pages, skip_empty=True).text() == "a\nb"
    assert Extracted("p", (Page("a"), Page("b"), Page("")), "\f").text() == \
        "a\fb\f"


# ------------------------------------------------------------------
# cell_of
# ------------------------------------------------------------------

def test_cell_of_is_the_nearest_right_edge_within_the_slack():
    cols = {"proceeds": 100.0, "cost": 150.0, "gain": 300.0}

    def at(x1):
        return cell_of(Word("1.00", x0=x1 - 20, x1=x1, top=0, bottom=8),
                       cols, 12.0)

    assert at(100.0) == "proceeds"
    assert at(112.0) == "proceeds"                       # at the slack
    assert at(112.01) is None                            # past it
    assert at(138.0) == "cost"
    assert at(137.99) is None
    assert at(225.0) is None                             # between, far off
    assert at(312.0) == "gain"


def test_cell_of_at_its_edges():
    cols = {"a": 10.0, "b": 20.0, "c": None}
    w = Word("x", x0=0, x1=15.0, top=0, bottom=0)
    assert cell_of(w, cols) == "a"                       # equal: the first
    assert cell_of(w, cols, slack=5.0) == "a"
    assert cell_of(w, cols, slack=4.0) is None
    assert cell_of(Word("x", x0=0, x1=1e6, top=0, bottom=0), cols) == "b"
    assert cell_of(w, {"c": None}) is None
    assert cell_of(w, {}) is None
