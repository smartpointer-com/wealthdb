"""Unit tests for statements.py — the per-LP capital-statement parser.

Synthetic text blocks in both source phrasings (the quarterly report's
"Partner's Capital Statement" section and the dedicated year-end
"Limited Partner's Capital Statement" PDF). All figures synthetic.
"""
from __future__ import annotations

import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import statements  # noqa: E402

QUARTERLY = """\
                 Example Fund of Funds, LP
              Partner's Capital Statement (Unaudited)

Beginning Balance                                  $    91,000 $   41,000 $        —
Called capital                                              —      60,000     202,000
Net change in unrealized gain/(loss)                     (800)      7,700       7,700
Distributions                                               —        (990)       (990)
Capital account balance at September 30, 2025      $    88,800 $   88,800 $    88,800

                                                     As of September 30, 2025
Committed capital                                  $   540,000
Paid in capital, since inception                       202,000
Unfunded commitment                                    338,000
Distributions, since inception                            (990)
"""

DEDICATED = """\
                 Example Fund of Funds, LP and
                 Example Fund of Funds QP, LP
              Limited Partner's Capital Statement (Unaudited)

Beginning Capital Account Balance                        $    88,800 $    41,000 $         —
Capital Contributions                                         60,000     202,000     338,000
Net Change in Unrealized Gains (Losses) on Investments         2,600       9,300       9,300
Capital Account Balance, December 31, 2025               $   233,300 $   233,300 $   233,300

                                    Commitment Summary
Capital Commitment                                       $   540,000
Capital Called Through December 31, 2025                 $   338,000
Distributions Through December 31, 2025                  $      (770)
"""


def test_parse_quarterly_phrasing():
    p = statements.parse_partner_capital_statement(QUARTERLY)
    assert p == {"ending_capital_cents": 8880000,
                 "contributed_cents": 20200000,
                 "distributions_cents": 99000}


def test_parse_dedicated_phrasing():
    p = statements.parse_partner_capital_statement(DEDICATED)
    assert p == {"ending_capital_cents": 23330000,
                 "contributed_cents": 33800000,
                 "distributions_cents": 77000}


def test_parse_rejects_non_statement_text():
    assert statements.parse_partner_capital_statement("Schedule of Investments\n") is None
    assert statements.parse_partner_capital_statement("") is None


def test_cents():
    assert statements._cents("1,234.56") == 123456
    assert statements._cents("(273)") == -27300
    assert statements._cents("—") is None
    assert statements._cents("") is None
