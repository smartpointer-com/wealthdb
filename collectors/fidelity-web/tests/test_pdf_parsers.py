"""
Unit tests for pdf_parsers.py — text-level parsers only.

The PDF I/O entry-point (``parse_statement_pdf``) is exercised
indirectly by the load tests when they call ``load.load_dump``
against a tmp bronze tree carrying a stub PDF. The functions
covered here are pure functions of strings, so the fixtures are
synthetic statement text — no real PDF bytes or real values.
"""

from __future__ import annotations

import sys
from datetime import date
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import pdf_parsers as pp  # noqa: E402


# ============================================================
# Fixtures — synthetic statement text, no real values
# ============================================================

# Mirror the post-extraction text layout: header (report title +
# period), a per-account header line ("Account # NNN-NNNNNN"), a
# beneficiary/registration line, then a Holdings block bounded by
# "Holdings" and "Total Market Value".

_QUARTERLY_TEXT = """\
INVESTMENT REPORT
January 1, 2026 - March 31, 2026
Page 1 of 8

Account # 100-000001
PLACEHOLDER BENEFICIARY - BENEFICIARY (529)

Holdings
Description
Percent of Total Value
Beginning Market Value
Quantity
Price per Unit
Ending Market Value
PLAN AGE-BASED CONSERVATIVE PORT 60% $1,000.00 100.000 $11.0000 $1,100.00
PLAN AGE-BASED MODERATE PORT 40 $2,000.00 50.000 36.0000 1,800.00
Total Market Value $2,900.00
"""


_YEAR_END_TEXT = """\
2025 YEAR-END INVESTMENT REPORT
January 1, 2025 - December 31, 2025
Page 1 of 8

Account # 200-000002
SECOND PLACEHOLDER - BENEFICIARY (529)

Holdings
Description Percent of Total Value Quantity Price per Unit Total Market Value
PLAN GROWTH PORT 100% 250.000 $40.0000 $10,000.00
Total Market Value $10,000.00
"""


# ============================================================
# Tests
# ============================================================

def test_parse_statement_period_quarterly():
    assert pp.parse_statement_period(_QUARTERLY_TEXT) == (
        date(2026, 1, 1), date(2026, 3, 31),
    )


def test_parse_statement_period_year_end():
    assert pp.parse_statement_period(_YEAR_END_TEXT) == (
        date(2025, 1, 1), date(2025, 12, 31),
    )


def test_parse_statement_period_returns_none_when_absent():
    assert pp.parse_statement_period("no period here") is None


def test_parse_account_blocks_strips_dash():
    blocks = pp.parse_account_blocks(_QUARTERLY_TEXT)
    assert len(blocks) == 1
    assert blocks[0].account_external_id == "100000001"


def test_parse_account_blocks_handles_multiple_accounts():
    text = _QUARTERLY_TEXT + "\n" + _YEAR_END_TEXT
    blocks = pp.parse_account_blocks(text)
    assert [b.account_external_id for b in blocks] == [
        "100000001", "200000002",
    ]


def test_parse_holdings_quarterly_layout():
    blocks = pp.parse_account_blocks(_QUARTERLY_TEXT)
    rows = pp.parse_holdings_block(blocks[0].text)
    assert len(rows) == 2
    # Quarterly layout: pct, beg_mv, qty, price, end_mv
    first = rows[0]
    assert first.description.startswith("PLAN AGE-BASED CONSERVATIVE")
    assert first.percent_of_total == 0.60
    assert first.quantity == 100.000
    assert first.price == 11.00
    assert first.market_value == 1100.00


def test_parse_holdings_year_end_layout():
    blocks = pp.parse_account_blocks(_YEAR_END_TEXT)
    rows = pp.parse_holdings_block(blocks[0].text)
    assert len(rows) == 1
    row = rows[0]
    assert row.description.startswith("PLAN GROWTH PORT")
    assert row.percent_of_total == 1.00
    assert row.quantity == 250.000
    assert row.price == 40.00
    assert row.market_value == 10000.00


def test_parse_holdings_tolerates_missing_percent_glyph():
    """Fidelity's PDF renderer drops the ``%`` on continuation
    rows. The second row of the quarterly fixture has ``40``
    instead of ``40%`` and should still parse."""
    blocks = pp.parse_account_blocks(_QUARTERLY_TEXT)
    rows = pp.parse_holdings_block(blocks[0].text)
    second = rows[1]
    assert second.percent_of_total == 0.40


def test_parse_holdings_skips_non_row_lines():
    """Column-header lines that happen to fall inside the
    Holdings span (e.g. ``Description``, ``Quantity``) must not
    be emitted as holdings rows."""
    blocks = pp.parse_account_blocks(_QUARTERLY_TEXT)
    rows = pp.parse_holdings_block(blocks[0].text)
    descriptions = [r.description for r in rows]
    assert all("Description" not in d for d in descriptions)
    assert all("Quantity" not in d for d in descriptions)


def test_parse_holdings_returns_empty_for_summary_only_block():
    """A per-account section that names the account on the
    Portfolio Summary page (no Holdings block) returns []."""
    text = (
        "Page 2\n"
        "Account # 100-000001\n"
        "Some summary text without Holdings here.\n"
    )
    blocks = pp.parse_account_blocks(text)
    assert pp.parse_holdings_block(blocks[0].text) == []
