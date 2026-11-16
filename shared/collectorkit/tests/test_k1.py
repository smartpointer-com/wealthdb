"""
Unit tests for the Schedule K-1 face page reader (collectorkit.k1): item
L, boxes 8, 9a and 19, the heading's tax year and a fiscal year's period,
over synthetic word boxes laid out as the form prints them. Synthetic
data only.
"""
from __future__ import annotations

from collectorkit import k1
from collectorkit.words import Extracted, Page, Word, params_key


# ---- Schedule K-1 face page ------------------------------------------------

def _w(x0: float, y0: float, text: str) -> Word:
    return Word(text, x0, x0 + 3.5 * len(text), y0, y0 + 8)


def _phrase(x0: float, y0: float, phrase: str) -> list[Word]:
    out = []
    for word in phrase.split():
        out.append(_w(x0, y0, word))
        x0 += 3.5 * len(word) + 2
    return out


_K1_CAPTIONS = {"heading": "Partner's Capital Account Analysis",
                "net_income": "Current year net income (loss)",
                "withdrawals": "Withdrawals and distributions"}
# Item L as forms before 2020 print it.
_K1_CAPTIONS_PRE_2020 = {"heading": "Partner's capital account analysis:",
                         "net_income": "Current year increase (decrease)",
                         "withdrawals": "Withdrawals & distributions"}


def _k1_face(captions: dict[str, str] = _K1_CAPTIONS) -> list[Word]:
    """A synthetic federal face page, laid out as the form prints it: item L
    on the left (x < 330), boxes 1-13 in the middle (from x 333), boxes
    14-21 on the right (from x 454). Amounts sit a few points off their
    caption line, as preparers print them. `captions` words item L."""
    p = []
    p += _phrase(36, 60, "Schedule K-1")
    p += _phrase(231, 84, "For calendar year 2098, or tax year")
    p += _phrase(337, 100, "1 Ordinary business income (loss)")
    p += _phrase(454, 100, "14 Self-employment earnings (loss)")
    p += [_w(431, 108, "389.")]
    p += _phrase(454, 304, "19 Distributions")
    p += [_w(454, 312, "A"), _w(525, 312, "1,529.")]
    p += [_w(454, 324, "C"), _w(525, 324, "2,089.")]
    p += _phrase(454, 340, "20 Other information")
    p += [_w(454, 348, "A"), _w(525, 348, "917.")]
    p += _phrase(337, 364, "8 Net short-term capital gain (loss)")
    p += [_w(405, 372, "(1,247.)")]
    p += _phrase(333, 388, "9a Net long-term capital gain (loss)")
    p += [_w(410, 396, "12,483.")]
    p += _phrase(333, 412, "9b Collectibles (28%) gain (loss)")
    p += _phrase(333, 436, "9c Unrecaptured section 1250 gain")
    p += [_w(115, 468, "9.3176000"), _w(180, 468, "%")]
    p += _phrase(112, 616, captions["heading"])
    p += _phrase(54, 628, "Beginning capital account ~~~ $")
    p += [_w(266, 624, "104,286.")]
    p += _phrase(54, 640, "Capital contributed during the year ~~ $")
    p += [_w(273, 636, "51,374.")]
    p += _phrase(54, 652, captions["net_income"] + " ~~ $")
    p += [_w(273, 648, "-3,127.")]
    p += _phrase(54, 664, "Other increase (decrease) (attach explanation) ~ $")
    p += _phrase(54, 676, captions["withdrawals"] + " ~~ $ (")
    p += [_w(280, 672, "3,618."), _w(321, 676, ")")]
    p += _phrase(54, 688, "Ending capital account ~~ $")
    p += [_w(266, 684, "148,915.")]
    p += _phrase(54, 736, "Beginning ~~~ $")
    p += [_w(266, 732, "7,731.")]
    return p


def _bbox(*pages: list[Word]) -> Extracted:
    """The pages as ``collectorkit.extract.pdftotext_bbox`` returns them."""
    return Extracted("pdftotext-bbox", tuple(
        Page(" ".join(w.text for w in ws), ((params_key({}), tuple(ws)),))
        for ws in pages))


_COVER = _phrase(36, 100, "Attached is your Schedule K-1 package.")


def test_k1_face_page_reads_item_l_gains_and_distributions():
    got = k1.parse(_bbox(_COVER, _k1_face()))
    assert {k: v for k, v in got.items() if k != "printed"} == {
        "tax_year": 2098,
        "period_start": None,          # a calendar-year form
        "period_end": None,
        "beginning_capital": "104286",
        "contributions": "51374",
        "net_income": "-3127",
        "other_change": None,          # left blank on the form
        "distributions": "3618",       # inside the form's own parentheses
        "ending_capital": "148915",
        "short_term_gain": "-1247",
        "long_term_gain": "12483",
        "cash_distributions": "1529",
        "property_distributions": "2089",
    }
    assert got["printed"]["short_term_gain"] == "(1,247.)"


def _item_l(got: dict) -> tuple:
    return tuple(got[c] for c in ("beginning_capital", "contributions",
                                  "net_income", "distributions",
                                  "ending_capital"))


def test_a_face_page_from_before_2020_reads_its_item_l():
    got = k1.parse(_bbox(_COVER, _k1_face(_K1_CAPTIONS_PRE_2020)))
    assert _item_l(got) == ("104286", "51374", "-3127", "3618", "148915")


def test_a_typographic_apostrophe_marks_the_face_page_too():
    captions = dict(_K1_CAPTIONS, heading="Partner\u2019s Capital Account Analysis")
    got = k1.parse(_bbox(_COVER, _k1_face(captions)))
    assert _item_l(got) == ("104286", "51374", "-3127", "3618", "148915")


def test_k1_caption_numbers_are_not_amounts():
    lines = k1._lines(_k1_face())
    boxes = k1._boxes(lines, 329, 446)
    assert k1._first_amount(boxes.get(("mid", "9c"), [])) is None
    assert k1._first_amount(boxes.get(("mid", "9b"), [])) is None
    assert k1._first_amount(boxes[("mid", "1")]) == "389."


def test_a_document_with_no_federal_face_page_yields_nothing():
    assert k1.parse(_bbox(_COVER)) is None


def test_k1_decimal():
    assert k1._decimal("1,234.") == "1234"
    assert k1._decimal("(1,234.50)") == "-1234.50"
    assert k1._decimal("-$12.") == "-12"
    assert k1._decimal("(9.)", magnitude=True) == "9"
    assert k1._decimal("STMT") is None


# ---- fiscal-year K-1 --------------------------------------------------------

def _heading_period(start: str | None = None, end: str | None = None) -> list[Word]:
    """The heading's "tax year beginning ... ending ..." line, below the
    calendar year, its two fields filled the way a preparer fills them."""
    p = [_w(112, 102, "beginning"), _w(231, 102, "ending")]
    if start:
        p += [_w(160, 101, start)]
    if end:
        p += [_w(270, 101, end)]
    return p


def test_a_fiscal_year_k1_states_its_period():
    got = k1.parse(_bbox(_COVER, _k1_face()
                              + _heading_period("07/01/2098", "06/30/2099")))
    assert (got["tax_year"], got["period_start"], got["period_end"]) == (
        2098, "2098-07-01", "2099-06-30")
    assert got["printed"]["period_end"] == "06/30/2099"
    assert _item_l(got) == ("104286", "51374", "-3127", "3618", "148915")


def test_a_calendar_year_k1_leaves_its_period_blank():
    # Blank fields, or only the year the form edition preprints in them.
    for face in (_k1_face() + _heading_period(),
                 _k1_face() + _heading_period("2098", "20")):
        got = k1.parse(_bbox(face))
        assert (got["tax_year"], got["period_start"], got["period_end"]) == (
            2098, None, None)
        assert "period_start" not in got["printed"]


def test_k1_date():
    for raw in ("07/01/2098", "7/1/98", "07 / 01 / 2098", "07/01 2098",
                "07-01-2098", "2098-07-01", "July 1, 2098", "Jul. 1, 2098"):
        assert k1._date(raw) == "2098-07-01", raw
    for raw in ("2098", "/ / 20", "13/01/2098", "07/01/298", "STMT 1"):
        assert k1._date(raw) is None, raw
