"""Statement periods printed as a pair of month-name dates.

Many statements state their period on page 1 as two full dates, each a
month name, a day, a comma and a year — ``January 1, 2026 - March 31,
2026`` or ``JANUARY 1, 2021 TO MARCH 31, 2021``. What sits between the two
dates, and any wording that introduces the pair, belongs to the statement
family, so a family builds its pattern here with its own separator and
prefix and reads the pair with :func:`read_month_range`.

This module is inside the fingerprint (``srcfp``) of every parser that
imports it: a code change here re-parses those collectors' documents.
It is deliberately not re-exported from the package ``__init__``.
"""
from __future__ import annotations

import re
from datetime import date

MONTHS = ("January", "February", "March", "April", "May", "June", "July",
          "August", "September", "October", "November", "December")

# Month name → number, keyed by the name as MONTHS spells it.
MONTH_NUMS = {name: n for n, name in enumerate(MONTHS, start=1)}

_MONTH = "|".join(MONTHS)


def month_range_pattern(separator: str, *, prefix: str = "",
                        flags: int = 0) -> re.Pattern[str]:
    """The pattern for ``<prefix>Month D, YYYY<separator>Month D, YYYY``.

    `separator` and `prefix` are regular-expression source. The two dates
    are captured as ``m1 d1 y1`` and ``m2 d2 y2``. Under ``re.IGNORECASE``
    the month names match in any case.
    """
    return re.compile(
        prefix
        + rf"(?P<m1>{_MONTH})\s+(?P<d1>\d{{1,2}}),\s*(?P<y1>\d{{4}})"
        + separator
        + rf"(?P<m2>{_MONTH})\s+(?P<d2>\d{{1,2}}),\s*(?P<y2>\d{{4}})",
        flags,
    )


def read_month_range(text: str, pattern: re.Pattern[str], *,
                     strict: bool = False) -> tuple[date, date] | None:
    """The first date pair `pattern` finds in `text`, or None when it
    finds none.

    A pair naming a day its month does not have (``February 30``) is None
    as well, unless `strict`, when the ``ValueError`` is raised.
    """
    m = pattern.search(text)
    if not m:
        return None
    try:
        return (
            date(int(m["y1"]), MONTH_NUMS[m["m1"].capitalize()], int(m["d1"])),
            date(int(m["y2"]), MONTH_NUMS[m["m2"].capitalize()], int(m["d2"])),
        )
    except ValueError:
        if strict:
            raise
        return None
