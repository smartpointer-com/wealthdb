"""collectorkit.statement_period — a period printed as two month-name dates,
with each statement family's own separator and prefix. Synthetic text."""
import re
from datetime import date

import pytest

from collectorkit import statement_period as sp


def test_month_numbers():
    assert sp.MONTH_NUMS["January"] == 1 and sp.MONTH_NUMS["December"] == 12
    assert len(sp.MONTH_NUMS) == 12


def test_a_hyphenated_pair():
    pat = sp.month_range_pattern(r"\s*[-–]\s*")
    text = "INVESTMENT REPORT\nJanuary 1, 2098 - March 31, 2098\n"
    assert sp.read_month_range(text, pat) == (date(2098, 1, 1), date(2098, 3, 31))
    # An en dash and no spaces read the same.
    assert sp.read_month_range("April 1, 2098–June 30, 2098", pat) == (
        date(2098, 4, 1), date(2098, 6, 30))


def test_a_prefixed_pair_in_any_case():
    pat = sp.month_range_pattern(
        r"\s+TO\s+", prefix=r"STATEMENT\s+FOR\s+THE\s+PERIOD\s+",
        flags=re.IGNORECASE)
    text = "STATEMENT FOR THE PERIOD JANUARY 1, 2098 TO MARCH 31, 2098"
    assert sp.read_month_range(text, pat) == (date(2098, 1, 1), date(2098, 3, 31))
    # Without the prefix the pair is not this family's header.
    assert sp.read_month_range("JANUARY 1, 2098 TO MARCH 31, 2098", pat) is None


def test_case_is_the_family_s_choice():
    pat = sp.month_range_pattern(r"\s+to\s+")
    assert sp.read_month_range("December 1, 2098 to December 31, 2098", pat) == (
        date(2098, 12, 1), date(2098, 12, 31))
    assert sp.read_month_range("DECEMBER 1, 2098 to DECEMBER 31, 2098", pat) is None


def test_no_pair_is_none():
    pat = sp.month_range_pattern(r"\s*[-–]\s*")
    assert sp.read_month_range("no period header here", pat) is None


def test_an_impossible_date_is_none_unless_strict():
    pat = sp.month_range_pattern(r"\s+to\s+")
    text = "February 30, 2098 to March 31, 2098"
    assert sp.read_month_range(text, pat) is None
    with pytest.raises(ValueError):
        sp.read_month_range(text, pat, strict=True)


def test_the_first_pair_wins():
    pat = sp.month_range_pattern(r"\s*-\s*")
    text = "May 1, 2098 - May 31, 2098 ... June 1, 2098 - June 30, 2098"
    assert sp.read_month_range(text, pat) == (date(2098, 5, 1), date(2098, 5, 31))
