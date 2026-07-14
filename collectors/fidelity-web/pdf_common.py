#!/usr/bin/env python3
"""Statement-period and account-header helpers shared by the two
statement parsers (``pdf_parsers`` for the 529 statements Fidelity
serves, ``pdf_parsers_supplied`` for statements supplied
out-of-band).

Both statement families carry the period as ``Month D, YYYY -
Month D, YYYY`` on page 1 and stamp a ``Account # NNN-NNNNNN``
header on their per-account pages, so the period regex, month map,
period parser and account-header regex are identical across the two.
The per-account *splitting* differs (the supplied-statement parser
glues headers re-stamped on every page), so ``parse_account_blocks``
stays in each parser module and only anchors on
``_ACCOUNT_HEADER_RE`` from here.
"""
from __future__ import annotations

import re
from datetime import date

# Fidelity statement headers carry the period as
# ``January 1, 2026 - March 31, 2026`` (quarterly) or
# ``January 1, 2025 - December 31, 2025`` (annual). The
# leading ``<year> YEAR-END`` prefix on annual reports is
# harmless — we anchor on the inner date pair regardless.
_PERIOD_RE = re.compile(
    r"(?P<m1>January|February|March|April|May|June|July|August|"
    r"September|October|November|December)\s+"
    r"(?P<d1>\d{1,2}),\s*"
    r"(?P<y1>\d{4})\s*[-–]\s*"
    r"(?P<m2>January|February|March|April|May|June|July|August|"
    r"September|October|November|December)\s+"
    r"(?P<d2>\d{1,2}),\s*"
    r"(?P<y2>\d{4})"
)

_MONTH_NUMS = {
    "January": 1, "February": 2, "March": 3, "April": 4,
    "May": 5, "June": 6, "July": 7, "August": 8,
    "September": 9, "October": 10, "November": 11, "December": 12,
}


def parse_statement_period(text):
    """Return ``(start_date, end_date)`` from a statement's page-1
    period header, or ``None`` if no header is present."""
    m = _PERIOD_RE.search(text)
    if not m:
        return None
    try:
        start = date(int(m["y1"]), _MONTH_NUMS[m["m1"]], int(m["d1"]))
        end = date(int(m["y2"]), _MONTH_NUMS[m["m2"]], int(m["d2"]))
    except (KeyError, ValueError):
        return None
    return start, end


# ``Account # NNN-NNNNNN`` marks the start of a per-account section.
_ACCOUNT_HEADER_RE = re.compile(r"Account\s+#\s+(?P<acct>\d{3}-\d{6})")
