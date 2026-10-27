"""Unit tests for statement_parser.py — synthetic statement text in the
`pdftotext -layout` shape of a First Citizens deposit statement. Every
figure, date and number is invented."""
from __future__ import annotations

import sys
from datetime import date
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import statement_parser as sp  # noqa: E402

HEADER = ("Statement Period: March 1 , 2026         Thru March 31, 2026"
          "                    Account Number :    000000000000\n")

SAVINGS = HEADER + """\
                   Example Money Market Savings
Beginning Balance                       1,000.00+       Statement Period Days                     31
         1  Deposits                      500.00        Average Collected Balance          1,250.00+
         0  Other Credits                   0.00        Annual Percentage Yield Earned*        3.12%
Interest Earned This Period                 2.16+       Interest Rate on Statement Day         3.07%
         0  Checks                          0.00        Total Interest Earned YTD              2.97+
Ending   Balance                        1,502.16+
"""

CHECKING = HEADER + """\
Beginning Balance                       1,000.00+       Statement Period Days                     31
Interest Earned This Period                 0.02+       Annual Percentage Yield Earned*        0.03%
Ending   Balance                        1,000.02+
AS OF ENDING STATEMENT DATE YOUR INTEREST RATE WAS 0.03% AND YOUR ANNUAL PERCENTAGE YIELD WAS 0.03%.
"""

NO_INTEREST = HEADER + """\
Beginning Balance                       1,000.00+
Ending   Balance                          900.00+
"""


def test_the_rate_on_the_statement_day_and_its_check():
    p = sp.parse_statement_text(SAVINGS)
    assert (p["period_start"], p["period_end"]) == (date(2026, 3, 1), date(2026, 3, 31))
    assert (p["interest_rate"], p["apy_earned"], p["apy"]) == (3.07, 3.12, None)
    assert p["lines"] == {
        "interest_rate": "Interest Rate on Statement Day         3.07%",
        "apy_earned": "Annual Percentage Yield Earned*        3.12%",
    }


def test_the_sentence_form():
    p = sp.parse_statement_text(CHECKING)
    assert (p["interest_rate"], p["apy_earned"], p["apy"]) == (0.03, 0.03, 0.03)
    assert p["lines"]["interest_rate"].startswith("YOUR INTEREST RATE WAS 0.03%")


def test_the_sentence_can_wrap():
    p = sp.parse_statement_text(CHECKING.replace(" AND YOUR ANNUAL", "\nAND YOUR ANNUAL"))
    assert (p["interest_rate"], p["apy"]) == (0.03, 0.03)


def test_a_statement_without_rate_lines_states_none():
    p = sp.parse_statement_text(NO_INTEREST)
    assert p["period_end"] == date(2026, 3, 31)
    assert (p["interest_rate"], p["apy_earned"], p["apy"], p["lines"]) == (None, None, None, {})


def test_a_statement_without_a_period_is_none():
    assert sp.parse_statement_text(SAVINGS.replace("Statement Period:", "Period:")) is None


def test_a_period_naming_a_day_its_month_lacks_is_none():
    assert sp.parse_statement_text(SAVINGS.replace("March 31", "February 30", 1)) is None


@pytest.mark.parametrize("rate,apy,disagrees", [
    (3.07, 3.12, False),   # daily compounding
    (3.12, 3.12, False),   # none
    (4.20, 4.27, False),   # near the top of the band
    (3.13, 3.12, False),   # a hundredth of rounding below
    (0.07, 0.06, False),   # the same, where rate - 0.01 lands above 0.06
    (2.02, 2.01, False),
    (3.12, 3.09, True),    # below the rate
    (4.20, 4.36, True),    # above any compounding
    (2.21, 2.58, True),    # the rate moved during the cycle
    (0.00, 0.00, False),
])
def test_rate_disagrees(rate, apy, disagrees):
    assert sp.rate_disagrees(rate, apy) is disagrees
