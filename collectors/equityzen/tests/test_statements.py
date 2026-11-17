"""Unit tests for statements.parse_k1: the K-1's tax year, final mark and
Item L from its text, and an Item L figure the text misses from its face
page's word boxes (collectorkit.k1). Synthetic data only."""
from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import statements  # noqa: E402
from collectorkit.words import Extracted, Page, Word, params_key  # noqa: E402


def _phrase(x0: float, top: float, phrase: str) -> list[Word]:
    out = []
    for word in phrase.split():
        out.append(Word(word, x0, x0 + 3.5 * len(word), top, top + 8))
        x0 += 3.5 * len(word) + 2
    return out


def _no_boxes():
    return None


def _face() -> Extracted:
    words = []
    for x0, top, phrase in [
            (36, 60, "Schedule K-1"),
            (337, 100, "1 Ordinary business income (loss)"),
            (454, 100, "14 Self-employment earnings (loss)"),
            (112, 616, "Partner's Capital Account Analysis"),
            (54, 628, "Beginning capital account $"), (266, 628, "104,286."),
            (54, 652, "Current year net income (loss) $"), (273, 652, "-3,127."),
            (54, 676, "Withdrawals and distributions $ ("), (280, 676, "3,618."),
            (321, 676, ")"),
            (54, 688, "Ending capital account $"), (266, 688, "148,915.")]:
        words += _phrase(x0, top, phrase)
    return Extracted("pdftotext-bbox", (
        Page(" ".join(w.text for w in words), ((params_key({}), tuple(words)),)),))


_HEAD = "Schedule K-1 (Form 1065)\nFor calendar year 2098\n"
# Item L as pdftotext -layout prints it, each amount on its caption's line.
_ITEM_L = ("Beginning capital account . . . $       90,000.\n"
           "Current year net income (loss) . . $       -3,127.\n"
           "Withdrawals and distributions . . $ (       2,000. )\n"
           "Ending capital account . . . . $       84,873.\n")


def test_a_printed_minus_sign_is_kept():
    got = statements.parse_k1(_HEAD + _ITEM_L, _no_boxes)
    assert got == {"tax_year": 2098, "is_final": False,
                   "beginning_capital": 90000.0, "current_year_income": -3127.0,
                   "withdrawals_distributions": -2000.0,
                   "ending_capital": 84873.0}


def test_a_form_the_text_reads_fully_is_not_read_from_its_boxes():
    # The boxes are never extracted.
    def boxes():
        raise AssertionError("the word boxes were extracted")

    assert statements.parse_k1(_HEAD + _ITEM_L, boxes) == (
        statements.parse_k1(_HEAD + _ITEM_L, _no_boxes))


def test_a_figure_off_its_caption_line_comes_from_the_boxes():
    # The beginning and ending amounts print on lines of their own, so the
    # text finds nothing after those captions; the boxes place them.
    text = (_HEAD + "                                     104,286.\n"
            "Beginning capital account . . . $\n"
            "Current year net income (loss) . . $       -3,127.\n"
            "Withdrawals and distributions . . $ (       3,618. )\n"
            "Ending capital account . . . . $\n"
            "                                     148,915.\n")
    calls = []

    def boxes():
        calls.append(1)
        return _face()

    assert statements.parse_k1(text, boxes) == {
        "tax_year": 2098, "is_final": False,
        "beginning_capital": 104286.0, "current_year_income": -3127.0,
        "withdrawals_distributions": -3618.0, "ending_capital": 148915.0}
    assert calls == [1]             # extracted once for both figures


def test_a_k1_without_word_boxes_keeps_what_the_text_states():
    got = statements.parse_k1(_HEAD + "Final K-1\nX\n", _no_boxes)
    assert got["tax_year"] == 2098 and got["is_final"] is True
    assert {got[k] for k in ("beginning_capital", "current_year_income",
                             "withdrawals_distributions",
                             "ending_capital")} == {None}
