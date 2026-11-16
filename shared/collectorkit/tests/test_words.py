"""Tests for collectorkit.words: the document types and the word
geometry (rows, columns, cell_of).

Each geometry test runs a reader's own clusterer or column rule on
invented words and checks that the kit function, with the parameters
that reader would pass, gives the same answer.
"""
from __future__ import annotations

import random

import collector_modules
import pytest

from collectorkit import pdf as kitpdf
from collectorkit.words import Extracted, Page, Word, cell_of, columns, rows

ubs = collector_modules.load("ubs-web", "pdf_parsers", ("pdfplumber",))
fidelity = collector_modules.load("fidelity-web", "pdf_parsers_1099")
carta = collector_modules.load("carta", "k1")
schwab = collector_modules.load("schwab-web", "tax_form_parsers")


def _page(rng: random.Random, n: int = 40) -> list[Word]:
    """Invented words in a top-down frame: lines a few points apart, a
    jitter around each that sometimes reaches the tolerance exactly, and
    some equal x0 values."""
    words = []
    for i in range(n):
        line = rng.randrange(8) * rng.choice((2.5, 3.0, 4.0, 6.0))
        top = 100 + line + rng.choice((0.0, 0.5, 1.0, 1.5, 2.0, 2.9, 3.0,
                                       3.1, 0.1 + 0.2))
        x0 = rng.choice((10.0, 20.0, 20.0, 35.5, 50.25)) + rng.randrange(5) * 40
        words.append(Word(f"w{i}", x0, x0 + rng.choice((8.0, 12.5)), top,
                          top + rng.choice((7.0, 9.0))))
    return words


SEEDS = range(300)


@pytest.mark.parametrize("ytol", [3.0, 1.0])
def test_rows_is_ubs_webs_ledger_clusterer(ytol):
    for seed in SEEDS:
        words = _page(random.Random(seed))
        theirs = ubs._stmt_cluster_rows(
            [{"text": w.text, "x0": w.x0, "x1": w.x1, "top": w.top,
              "bottom": w.bottom} for w in words], ytol)
        ours = rows(words, ytol)
        assert [(r.top, [w.text for w in r.words]) for r in ours] == \
            [(r["top"], [w["text"] for w in r["words"]]) for r in theirs], seed


class _Page:
    def __init__(self, words):
        self._words = words

    def extract_words(self):
        return [{"text": w.text, "x0": w.x0, "x1": w.x1, "top": w.top,
                 "bottom": w.bottom} for w in self._words]


def test_rows_is_fidelity_1099s_line_reader():
    for seed in SEEDS:
        words = _page(random.Random(seed))
        theirs = fidelity._page_lines(_Page(words))
        ours = [[fidelity.Word(w.text, w.x0, w.x1) for w in r.words]
                for r in rows(words, fidelity._LINE_TOLERANCE)]
        assert ours == theirs, seed


def test_rows_is_cartas_k1_line_reader():
    for seed in SEEDS:
        words = _page(random.Random(seed))
        theirs = carta._lines([carta.Word(w.x0, w.top, w.x1, w.text)
                               for w in words])
        ours = [[carta.Word(w.x0, w.top, w.x1, w.text) for w in r.words]
                for r in rows(words, carta._SAME_LINE)]
        assert ours == theirs, seed


def test_rows_is_the_ocr_line_grouper():
    """Dyadic coordinates, so a run's centre and height survive the trip
    through a box exactly. Distinct x within a page, since the grouper
    orders equal x by text."""
    for seed in SEEDS:
        rng = random.Random(seed)
        runs = []
        for i, x in enumerate(rng.sample(range(64), 20)):
            centre = rng.randrange(16, 48) / 64 + rng.choice((0, 1 / 128))
            height = rng.choice((1, 2, 3, 4)) / 64
            runs.append(kitpdf.TextRun(f"r{i}", x / 64, centre, height))
        theirs = kitpdf.group_ocr_lines(runs, kitpdf.OCR_LINE_TOLERANCE)
        words = [Word(r.text, r.x, r.x, r.centre - r.height / 2,
                      r.centre + r.height / 2) for r in runs]
        ours = rows(words, kitpdf.OCR_LINE_TOLERANCE, "mean", by="mid",
                    relative=True)
        assert [" ".join(w.text for w in r.words) for r in ours] == theirs, seed


def _schwab_word(text, x0, bottom, height=8.0):
    """A word in PDFium's frame: y grows up, so top is the larger."""
    return schwab._Word(text, x0, x0 + 10, bottom, bottom + height)


def test_rows_is_schwabs_same_row_on_separate_lines():
    """schwab-web asks for the row of one word: every word whose middle
    lies within that word's height. On lines of one height, well apart,
    that is the line rows() groups with half a height of tolerance."""
    lines = (700.0, 690.0, 671.5)
    theirs_words = [_schwab_word(f"w{i}{j}", 10 + 30 * j, bottom)
                    for i, bottom in enumerate(lines) for j in range(4)]
    words = [Word(w.text, w.x0, w.x1, w.top, w.bottom) for w in theirs_words]
    by_text = {}
    for row in rows(words, 4.0, by="mid"):
        for w in row.words:
            by_text[w.text] = [v.text for v in row.words]
    for w in theirs_words:
        assert [v.text for v in schwab._same_row(theirs_words, w)] == \
            by_text[w.text]


def test_schwabs_same_row_is_not_a_partition():
    """A taller word's band reaches two lines that rows() keeps apart.
    So schwab-web's reader is a band query, and rows() does not stand in
    for it."""
    tall = _schwab_word("tall", 100, 688.0, height=22.0)
    theirs_words = [_schwab_word("a", 10, 700.0), _schwab_word("b", 10, 690.0),
                    tall]
    band = [w.text for w in schwab._same_row(theirs_words, tall)]
    assert set(band) == {"a", "b", "tall"}
    grouped = rows([Word(w.text, w.x0, w.x1, w.top, w.bottom)
                    for w in theirs_words], 4.0, by="mid")
    assert len(grouped) > 1


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
# columns and cell_of
# ------------------------------------------------------------------

def _row(*cells):
    """Header words from (text, x0, x1), on one line."""
    return [Word(t, x0, x1, 50.0, 58.0) for t, x0, x1 in cells]


LEDGER_HEADER = _row(("Date", 20, 40), ("Information", 60, 110),
                     ("Debits", 300, 330), ("Credits", 380, 415),
                     ("Value", 440, 465), ("date", 466, 485),
                     ("Balance", 520, 560))
LEDGER_LABELS = {"Debits": "Debits", "Credits": "Credits",
                 "ValueDate": "Value", "Balance": "Balance"}


def _dict(w: Word) -> dict:
    return {"text": w.text, "x0": w.x0, "x1": w.x1, "top": w.top,
            "bottom": w.bottom}


def test_columns_and_cell_of_are_ubs_webs_ledger_rule():
    anchors = ubs._stmt_find_header(
        ubs._stmt_cluster_rows([_dict(w) for w in LEDGER_HEADER]))
    cols = columns(LEDGER_HEADER, LEDGER_LABELS, "centre")
    assert cols == {k: v for k, v in anchors.items() if k != "top"}
    for x in range(250, 620, 3):
        for width in (5, 20, 41):
            w = Word("1.00", x, x + width, 70.0, 78.0)
            assert cell_of(w, cols, by="centre") == \
                ubs._stmt_assign_numeric(_dict(w), anchors), (x, width)


TL_HEADER = _row(("Trade", 10, 30), ("date", 32, 50), ("Booking", 60, 90),
                 ("text", 92, 108), ("Number/Amount", 140, 200),
                 ("Description", 220, 270), ("Cost/Purchase", 320, 370),
                 ("price", 372, 395), ("Transaction", 420, 470),
                 ("price", 472, 495), ("Transaction", 520, 570),
                 ("gain", 572, 590), ("Transaction", 620, 670),
                 ("value", 672, 695))
TL_RIGHT = {"D": "Cost/Purchase price", "E": "Transaction price",
            "F": "Transaction gain", "G": "Transaction value"}


def test_columns_and_cell_of_are_ubs_webs_transaction_list_rule():
    second = [Word("x", 10, 20, 61.0, 69.0)]
    anchors = ubs._tl_anchors([{"top": 50.0, "words": [_dict(w) for w in
                                                       TL_HEADER]},
                               {"top": 61.0, "words": [_dict(w) for w in
                                                       second]}])
    right = columns(TL_HEADER, TL_RIGHT, "right", ordered=True)
    assert right == anchors["right"]
    assert columns(TL_HEADER, {"n": "Number/Amount"})["n"] == \
        anchors["number_x1"]
    assert columns(TL_HEADER, {"d": "Description"}, "left")["d"] == \
        anchors["desc_x0"]
    assert columns(TL_HEADER, {"D": "Cost/Purchase price"}, "left")["D"] == \
        anchors["d_x0"]
    # Unordered, "Transaction price" and "Transaction gain" still differ,
    # but a label repeated left of its column would be found first.
    assert columns(TL_HEADER, TL_RIGHT, "right") == right
    for x1 in range(330, 720, 2):
        w = Word("1.00", x1 - 25, x1, 80.0, 88.0)
        (col,) = ubs._tl_cells([_dict(w)], anchors)
        assert cell_of(w, right, ubs._TL_SLACK, rule="first_reaching",
                       default="G") == col, x1


FIDELITY_HEADER = _row(("Action", 10, 40), ("Quantity", 50, 90),
                       ("1d", 150, 160), ("1e", 230, 240), ("1f", 300, 310),
                       ("1g", 380, 390), ("Gain/Loss", 450, 500),
                       ("4", 540, 545), ("14", 600, 610))


def test_columns_and_cell_of_are_fidelity_1099s_rule():
    line = [fidelity.Word(w.text, w.x0, w.x1) for w in FIDELITY_HEADER]
    starts = fidelity._column_starts(line)
    labels = {column: label for label, column in fidelity._COLUMN_LABELS}
    cols = columns(FIDELITY_HEADER, labels, "left")
    assert sorted((x, name) for name, x in cols.items()) == starts
    ordered = {name: x for x, name in starts}
    for x1 in range(100, 700, 3):
        w = Word("1.00", x1 - 30, x1, 70.0, 78.0)
        assert cell_of(w, ordered, rule="last_passed") == \
            fidelity._column_for(x1, starts), x1
    assert columns(FIDELITY_HEADER[:-1], labels, "left") is None
    assert fidelity._column_starts(line[:-1]) is None


def test_cell_of_is_schwabs_year_end_rule():
    """schwab-web places an amount at the heading whose right edge is
    nearest its own, within 12 points, and refuses the lot otherwise."""
    cols = {"proceeds": 100.0, "cost": 150.0, "wash": 200.0,
            "discount": 230.0, "gain": 300.0}
    sold = schwab._Word("01/02/25", 10, 50, 500.0, 508.0)

    def amount(x1):
        return schwab._Word("1.00", x1 - 20, x1, 500.0, 508.0)

    fixed = [amount(100.0), amount(150.0), amount(300.0)]
    for x1 in range(156, 262, 2):
        probe = amount(float(x1))
        theirs = schwab._yes_lot_amounts([sold, *fixed, probe], sold, cols)
        col = cell_of(Word("1.00", x1 - 20, x1, 0, 0), cols,
                      schwab._YES_COLUMN_SLACK)
        field = schwab._YES_LAYOUT_FIELDS.get((col, 0))
        if col in (None, "proceeds", "cost", "gain"):
            assert theirs is None, x1
        else:
            assert theirs is not None and field in theirs, x1


def test_cell_of_rules_at_their_edges():
    cols = {"a": 10.0, "b": 20.0, "c": None}
    w = Word("x", 0, 15.0, 0, 0)
    assert cell_of(w, cols) == "a"                       # equal: the first
    assert cell_of(w, cols, slack=4.0) is None
    assert cell_of(w, cols, rule="first_reaching") == "b"
    assert cell_of(w, cols, rule="last_passed") == "a"
    assert cell_of(Word("x", 0, 5.0, 0, 0), cols, rule="last_passed",
                   default="-") == "-"
    with pytest.raises(ValueError):
        cell_of(w, cols, rule="widest")
    with pytest.raises(ValueError):
        rows([w], 1.0, anchor="last")
