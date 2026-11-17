"""Tests for collectorkit.statement over invented statements: the year of
a printed row date, the amount after a label, and the two reconciliation
checks with their problem lists."""
from __future__ import annotations

from datetime import date
from decimal import Decimal
from typing import NamedTuple

from collectorkit.statement import (identity_problems, labelled_amounts,
                                    row_date, section_problems)


def test_row_date_branches():
    start, end = date(2024, 12, 20), date(2025, 1, 19)
    assert row_date(12, 28, start, end) == date(2024, 12, 28)   # inside
    assert row_date(12, 15, start, end) == date(2024, 12, 15)   # just before
    assert row_date(1, 25, start, end) == date(2025, 1, 25)     # just after
    assert row_date(13, 2, start, end) is None                  # no date
    assert row_date(0, 2, start, end) is None
    assert row_date(2, 30, start, end) is None
    assert row_date(2, 29, date(2025, 2, 1), date(2025, 2, 28)) == \
        date(2024, 2, 29)                                       # leap year back
    assert row_date(2, 29, date(2024, 2, 1), date(2024, 2, 29)) == \
        date(2024, 2, 29)                                       # leap day inside
    assert row_date(7, 4, date(2025, 6, 29), date(2025, 7, 28)) == \
        date(2025, 7, 4)


def test_an_equal_distance_goes_to_the_earlier_year():
    # 12/16 lies 77 days before the start in 2023 and 77 days after the
    # end in 2024.
    start, end = date(2024, 3, 2), date(2024, 9, 30)
    assert (start - date(2023, 12, 16)).days == \
        (date(2024, 12, 16) - end).days
    assert row_date(12, 16, start, end) == date(2023, 12, 16)
    start, end = date(2025, 3, 1), date(2025, 9, 30)
    assert row_date(12, 15, start, end) == date(2024, 12, 15)


# An invented summary block and its labels.
AMOUNT = r"[+-]?\$[+-]?[\d,]+(?:\.\d{2})?"
LABELS = {
    "previous": "Previous Balance",
    "payments": "Payments, Credits",
    "purchases": "Purchases",
    "fees": "Fees",
    "interest": "Interest Charged",
    "new": "New Balance",
    "limit": "Credit Line",
}
BLOB = ("SUMMARY Previous Balance $1,234.56 Payments, Credits -$40.00 "
        "Purchases +$79.99 Fees $-0.50 Interest Charged $1.10 Late Payment "
        "Warning: text New Balance $1,275.15 Credit Line $5,000 Fees none")


def test_labelled_amounts_finds_the_first_amount_after_each_label():
    assert labelled_amounts(BLOB, LABELS, AMOUNT) == {
        "previous": "$1,234.56",
        "payments": "-$40.00",
        "purchases": "+$79.99",
        "fees": "$-0.50",
        "interest": "$1.10",
        "new": "$1,275.15",
        "limit": "$5,000",
    }


def test_a_label_followed_by_no_amount_is_none():
    assert labelled_amounts("Fees none", {"fees": "Fees"}, AMOUNT) == \
        {"fees": None}
    assert labelled_amounts("Fees\n  $3.00", {"fees": "Fees"}, AMOUNT) == \
        {"fees": "$3.00"}
    assert labelled_amounts("", {"fees": "Fees"}, AMOUNT) == {"fees": None}
    # A label is matched literally, its punctuation included.
    assert labelled_amounts("Payments Credits $1.00",
                            {"p": "Payments, Credits"}, AMOUNT) == {"p": None}


class Row(NamedTuple):
    kind: str
    amount: Decimal


D = Decimal
GOOD = [Row("PAYMENT", D("-40.00")), Row("PURCHASE", D("80.00")),
        Row("PURCHASE", D("-0.01")), Row("INTEREST", D("1.10"))]
KIND_TO_FIELD = {"PAYMENT": "payments", "CREDIT": "payments",
                 "PURCHASE": "purchases", "INTEREST": "interest"}
STATED = {"payments": D("-40.00"), "purchases": D("79.99"),
          "interest": D("1.10"), "cash": D("0.00")}


def test_identity_problems():
    terms = [D("100.00"), D("-40.00"), D("79.99"), D("1.10")]
    assert identity_problems("summary", terms, D("141.09")) == []
    assert identity_problems("summary", terms, D("141.10")) == \
        ["summary: does not add up"]
    assert identity_problems("summary", [*terms[:-1], None], D("141.09")) == \
        ["summary: a figure is missing"]
    assert identity_problems("summary", terms, None) == \
        ["summary: a figure is missing"]
    assert identity_problems("empty", [], D("0")) == []


def _sections(rows, stated=STATED, zero=("cash",)):
    return section_problems(rows, KIND_TO_FIELD, stated, zero)


def test_section_problems_per_variant():
    assert _sections(GOOD) == []
    # Kinds that share a field sum together: a payment read as a credit
    # still holds.
    assert _sections([Row("CREDIT", D("-40.00")), *GOOD[1:]]) == []
    assert _sections([*GOOD, Row("ODD", D("0.00"))]) == \
        ["a row of kind 'ODD' has no summary figure"]
    assert _sections([Row("PAYMENT", D("80.00")), GOOD[0], *GOOD[2:]]) == [
        "payments: its rows do not add up to it",
        "purchases: its rows do not add up to it"]
    assert _sections(GOOD[:-1]) == ["interest: its rows do not add up to it"]
    assert _sections([]) == [
        "payments: its rows do not add up to it",
        "purchases: its rows do not add up to it",
        "interest: its rows do not add up to it"]
    assert _sections(GOOD, {**STATED, "purchases": None}) == \
        ["purchases: the figure is missing"]
    assert _sections(GOOD, {**STATED, "cash": D("5.00")}) == \
        ["cash: not zero, and no section reads it"]
    assert _sections(GOOD, {k: v for k, v in STATED.items() if k != "cash"}) \
        == ["cash: the figure is missing"]


def test_problem_messages_name_fields_not_amounts():
    problems = _sections([*GOOD, Row("PURCHASE", D("123.45"))],
                         {**STATED, "cash": D("678.90")})
    assert problems == ["purchases: its rows do not add up to it",
                        "cash: not zero, and no section reads it"]
    assert not any(a in p for p in problems for a in ("123.45", "678.90"))
