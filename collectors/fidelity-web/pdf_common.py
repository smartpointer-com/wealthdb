#!/usr/bin/env python3
"""Statement-period and account-header helpers shared by the two
statement parsers (``pdf_parsers`` for the 529 statements Fidelity
serves, ``pdf_parsers_supplied`` for statements supplied
out-of-band).

Both statement families carry the period as ``Month D, YYYY -
Month D, YYYY`` on page 1 and stamp a ``Account # NNN-NNNNNN``
header on their per-account pages, so the period reader and the
account-header regex are identical across the two; the period is read
by ``collectorkit.statement_period`` with this family's separator.
The per-account *splitting* differs (the supplied-statement parser
glues headers re-stamped on every page), so ``parse_account_blocks``
stays in each parser module and only anchors on
``_ACCOUNT_HEADER_RE`` from here.
"""
from __future__ import annotations

import re

from collectorkit import statement_period

# Fidelity statement headers carry the period as
# ``January 1, 2026 - March 31, 2026`` (quarterly) or
# ``January 1, 2025 - December 31, 2025`` (annual). The
# leading ``<year> YEAR-END`` prefix on annual reports is
# harmless — we anchor on the inner date pair regardless.
_PERIOD_RE = statement_period.month_range_pattern(r"\s*[-–]\s*")


def parse_statement_period(text):
    """Return ``(start_date, end_date)`` from a statement's page-1
    period header, or ``None`` if no header is present."""
    return statement_period.read_month_range(text, _PERIOD_RE)


# ``Account # NNN-NNNNNN`` marks the start of a per-account section.
_ACCOUNT_HEADER_RE = re.compile(r"Account\s+#\s+(?P<acct>\d{3}-\d{6})")
