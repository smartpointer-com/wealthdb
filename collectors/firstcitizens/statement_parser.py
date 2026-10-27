"""The interest rate a First Citizens deposit statement states.

A statement PDF covers one account and one cycle. Its text, as
`pdftotext -layout` prints it, carries the cycle on every page header:

    Statement Period: March 1 , 2026     Thru March 31, 2026     Account Number : <number>

and, in the summary box beside the balances, the rate lines:

    Annual Percentage Yield Earned*        3.12%
    Interest Rate on Statement Day         3.07%

An interest checking statement prints the second as a sentence instead:

    AS OF ENDING STATEMENT DATE YOUR INTEREST RATE WAS 3.07% AND YOUR
    ANNUAL PERCENTAGE YIELD WAS 3.12%.

The interest rate on the statement day is the rate, in percent a year.
The annual percentage yield earned is what the cycle's interest came to,
and checks it (rate_disagrees). A statement of an account that earns
nothing prints neither. The account is the one the statement was
downloaded for; the printed account number is never read.
"""
from __future__ import annotations

import re
from pathlib import Path

from collectorkit import pdftotext, statement_period

# statement_period.month_range_pattern allows no space before the comma,
# and this statement prints one ("March 1 , 2026"). The local pattern keeps
# the shared group names and month list for read_month_range.
_M = "|".join(statement_period.MONTHS)
_PERIOD_RE = re.compile(
    rf"Statement Period:\s*(?P<m1>{_M})\s+(?P<d1>\d{{1,2}})\s*,\s*(?P<y1>\d{{4}})"
    rf"\s+Thru\s+(?P<m2>{_M})\s+(?P<d2>\d{{1,2}})\s*,\s*(?P<y2>\d{{4}})")
_PCT = r"(\d+(?:\.\d+)?)%"
_RATE_RE = re.compile(r"Interest Rate on Statement Day\s+" + _PCT)
_APY_EARNED_RE = re.compile(r"Annual Percentage Yield Earned\*?\s+" + _PCT)
_RATE_SENTENCE_RE = re.compile(
    r"YOUR\s+INTEREST\s+RATE\s+WAS\s+" + _PCT
    + r"\s+AND\s+YOUR\s+ANNUAL\s+PERCENTAGE\s+YIELD\s+WAS\s+" + _PCT)

# Each figure prints to a hundredth of a percent, so two of them can sit a
# hundredth apart from rounding alone.
APY_ROUNDING = 0.01

# Slack for binary floats: 0.07 - 0.01 lands just above 0.06.
_FLOAT_SLACK = 1e-9


def parse_statement_text(text: str) -> dict | None:
    """A statement's cycle and rate lines, or None when it prints no
    statement period. Keys:

    - period_start, period_end: the cycle, as dates;
    - interest_rate: the rate on the statement day, percent a year;
    - apy_earned: the annual percentage yield the cycle earned;
    - apy: the annual percentage yield on the statement day, where the
      sentence form states one;
    - lines: the text each figure is read from.

    A figure the statement does not print is None."""
    period = statement_period.read_month_range(text, _PERIOD_RE)
    if period is None:
        return None
    out = {"period_start": period[0], "period_end": period[1], "interest_rate": None,
           "apy_earned": None, "apy": None, "lines": {}}
    if r := _RATE_RE.search(text):
        out["interest_rate"] = float(r[1])
        out["lines"]["interest_rate"] = r[0]
    elif s := _RATE_SENTENCE_RE.search(text):
        out["interest_rate"], out["apy"] = float(s[1]), float(s[2])
        out["lines"]["interest_rate"] = " ".join(s[0].split())
    if a := _APY_EARNED_RE.search(text):
        out["apy_earned"] = float(a[1])
        out["lines"]["apy_earned"] = a[0]
    return out


def apy_of(rate: float) -> float:
    """The annual percentage yield of `rate`, compounded daily: the most a
    rate can come to over a year."""
    return ((1 + rate / 100 / 365) ** 365 - 1) * 100


def rate_disagrees(rate: float, apy: float) -> bool:
    """Whether an annual percentage yield is not one `rate` can give.

    A yield lies between the rate itself (no compounding) and the rate
    compounded daily, each widened by the printed rounding. A yield
    earned outside that band means the rate moved during the cycle, or
    a line was misread."""
    lo = rate - APY_ROUNDING - _FLOAT_SLACK
    hi = apy_of(rate) + APY_ROUNDING + _FLOAT_SLACK
    return not (lo <= apy <= hi)


def parse_statement_pdf(path: Path | str) -> dict | None:
    """parse_statement_text over one PDF's `pdftotext -layout` text.
    Raises collectorkit.pdftotext's errors."""
    return parse_statement_text(pdftotext.layout_text(path, timeout=60))
