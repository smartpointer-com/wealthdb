"""
Unit tests for statement_tokens.py — how this bank writes a number and
a date, shared by both parsers.

These are the conventions a mistake in does not fail loudly: a
parenthesis read as decoration reverses a movement, and a two-digit
year pivoted wrongly moves it a century. Every fixture is synthetic.
"""

from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))

from statement_tokens import (  # noqa: E402
    iso_from_short_date,
    parse_money,
)


# ============================================================
# parse_money
# ============================================================

def test_parentheses_mean_negative():
    # The single most consequential rule here: a debit read as a credit
    # reverses a movement instead of failing.
    assert parse_money("($1,234.56)") == -1234.56
    assert parse_money("$1,234.56") == 1234.56


def test_an_inline_sign_means_negative_too():
    # The deposit ledger signs a withdrawal inline rather than in
    # parentheses; both spellings reach the same reader.
    assert parse_money("$-2,345.00") == -2345.0
    assert parse_money("-2345.00") == -2345.0


def test_an_unmatched_parenthesis_is_not_a_number():
    # Reading it as positive would turn a debit into a credit. None
    # lets the caller's own arithmetic refuse the row instead.
    assert parse_money("($1.00") is None
    assert parse_money("$1.00)") is None


def test_a_trailing_percent_is_dropped():
    # The brokerage ledger prints a margin rate in the description.
    assert parse_money("3.250%") == 3.25


def test_a_lone_sign_is_not_a_number():
    for token in ("-", "+", "", "   ", "$", None):
        assert parse_money(token) is None, token


def test_thousands_separators_and_currency_marks_are_noise():
    assert parse_money("  $12,345,678.90  ") == 12345678.90


# ============================================================
# iso_from_short_date
# ============================================================

def test_either_separator_reads_the_same():
    assert iso_from_short_date("01/24/22") == "2022-01-24"
    assert iso_from_short_date("01-24-22") == "2022-01-24"


def test_a_colon_or_a_dot_is_accepted_because_ocr_makes_that_mistake():
    # A hyphen read as a colon or a dot is a recognition slip, not a
    # different date; losing the row over it would break a
    # running-balance chain.
    assert iso_from_short_date("09:07", year=2021) == "2021-09-07"
    assert iso_from_short_date("09.07", year=2021) == "2021-09-07"


def test_a_year_is_supplied_only_where_the_token_lacks_one():
    # A deposit ledger dates a row MM-DD and leaves the year to the
    # statement period; a token carrying its own year keeps it.
    assert iso_from_short_date("12-06", year=2021) == "2021-12-06"
    assert iso_from_short_date("01/24/22", year=2021) == "2022-01-24"


def test_the_pivot_puts_these_archives_in_this_century():
    assert iso_from_short_date("01/01/69") == "2069-01-01"
    assert iso_from_short_date("01/01/70") == "1970-01-01"


def test_a_day_that_does_not_exist_is_not_a_date():
    assert iso_from_short_date("02/30/22") is None
    assert iso_from_short_date("13/01/22") is None


def test_what_is_not_a_date_at_all():
    for token in ("", "nonsense", "2022-01-24", None):
        assert iso_from_short_date(token) is None, token
    # A stray token in a ledger must not become a date just because a
    # year was on offer.
    assert iso_from_short_date("nonsense", year=2021) is None
