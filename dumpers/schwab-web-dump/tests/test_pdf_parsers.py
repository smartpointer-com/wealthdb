"""
Unit tests for pdf_parsers.py.

Hand-curated synthetic text mimics what
pdfplumber.extract_text() returns on a real Schwab brokerage
statement. No real PDFs touched — these tests are safe to ship
and run in CI without any account data.

A separate test module (test_pdf_parsers_fixture.py) handles the
fixture-driven case against a real local PDF; that one
auto-skips when the fixture isn't present.
"""

from __future__ import annotations

import sys
from datetime import date
from pathlib import Path

import pytest

# Import target under test. tests/ is a sibling of pdf_parsers.py
# in the repo root.
sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import pdf_parsers as pp  # noqa: E402


# ============================================================
# parse_statement_period
# ============================================================

class TestStatementPeriod:
    def test_single_month_period(self):
        text = (
            "Schwab One International Account of AccountNickname\n"
            "AccountNickname\n"
            "StatementPeriod\n"
            "February 1-28, 2026\n"
        )
        result = pp.parse_statement_period(text)
        assert result == (date(2026, 2, 1), date(2026, 2, 28))

    def test_cross_month_period(self):
        text = "Period: January 30-February 5, 2025\n"
        result = pp.parse_statement_period(text)
        assert result == (date(2025, 1, 30), date(2025, 2, 5))

    def test_period_at_year_end(self):
        text = "December 1-31, 2023 footer noise\n"
        result = pp.parse_statement_period(text)
        assert result == (date(2023, 12, 1), date(2023, 12, 31))

    def test_no_period_returns_none(self):
        text = "Some statement without a parseable period header"
        assert pp.parse_statement_period(text) is None

    def test_period_with_spaces_after_hyphen(self):
        # Schwab is inconsistent about whitespace around hyphens.
        text = "March 1 - 31, 2024"
        assert pp.parse_statement_period(text) == (date(2024, 3, 1), date(2024, 3, 31))


# ============================================================
# parse_transactions row parsing
# ============================================================

# Minimal valid wrapper: "Transaction Details" header + rows +
# "TotalTransactions" terminator. parse_transactions ignores
# everything outside this section.
PERIOD_HEADER = "February 1-28, 2026\n"


def _wrap(rows_text: str) -> str:
    """Wrap synthetic transaction rows in the minimal headers
    parse_transactions expects."""
    return (
        PERIOD_HEADER
        + "Transaction Details\n"
        + "Symbol/ Price/Rate Charges/ Realized\n"
        + "Date Category Action CUSIP Description Quantity perShare($) Interest($) Amount($) Gain/(Loss)($)\n"
        + rows_text
        + "TotalTransactions ($1.00) $2.00\n"
    )


class TestSaleRows:
    def test_sale_with_realized_gain_short_term(self):
        text = _wrap(
            "02/02 Sale SYN1 SYNTHETICONEFUND (100.0000) 100.0000 0.10 10,000.00 500.00,(ST)\n"
        )
        rows = pp.parse_transactions(text)
        assert len(rows) == 1
        r = rows[0]
        assert r.date == date(2026, 2, 2)
        assert r.category == "Sale"
        assert r.symbol == "SYN1"
        assert "SYNTHETICONEFUND" in r.description
        assert r.quantity == -100.0
        assert r.price == 100.0
        assert r.charges == 0.10
        assert r.amount == 10000.0
        assert r.realized_gain_loss == pytest.approx(500.00)
        assert r.term == "ST"

    def test_sale_with_realized_loss(self):
        text = _wrap(
            "03/15 Sale ABCD SOMECOMPANYINC (100.0000) 50.0000 0.05 5,000.00 (250.50),(LT)\n"
        )
        rows = pp.parse_transactions(text)
        assert len(rows) == 1
        r = rows[0]
        assert r.realized_gain_loss == pytest.approx(-250.50)
        assert r.term == "LT"

    def test_sale_of_cusip_with_continuation(self):
        # Treasury sales render with a CUSIP and the description
        # wraps onto a second line ("NOTE DUE12/31/99").
        text = _wrap(
            "02/06 Sale 000000AA0 SYNTHETICBONDNT (100,000.0000) 95.0000 50.00 95,000.00 100.00,(ST)\n"
            "NOTE DUE12/31/99\n"
        )
        rows = pp.parse_transactions(text)
        assert len(rows) == 1
        r = rows[0]
        assert r.symbol == "000000AA0"  # CUSIP
        assert "SYNTHETICBONDNT" in r.description
        assert "DUE12/31/99" in r.description  # continuation merged in
        assert r.quantity == -100000.0
        assert r.realized_gain_loss == pytest.approx(100.00)


class TestPurchaseRows:
    def test_simple_purchase(self):
        text = _wrap(
            "02/05 Purchase SYN1 SYNTHETICONEFUND 100.0000 100.0000 (10,000.00)\n"
        )
        rows = pp.parse_transactions(text)
        assert len(rows) == 1
        r = rows[0]
        assert r.category == "Purchase"
        assert r.symbol == "SYN1"
        assert r.quantity == 100.0
        assert r.price == 100.0
        assert r.charges is None  # no charge column in this row
        assert r.amount == pytest.approx(-10000.0)
        assert r.realized_gain_loss is None

    def test_purchase_inherits_date_from_previous_row(self):
        # Schwab condenses multi-row dates: rows after the first
        # for a date start with the category, not the date.
        text = _wrap(
            "02/05 Purchase SYN1 SYNTHETICONEFUND 100.0000 100.0000 (10,000.00)\n"
            "Purchase SYN2 SYNTHETICTWOFUND 500.0000 50.0000 (25,000.00)\n"
        )
        rows = pp.parse_transactions(text)
        assert len(rows) == 2
        assert rows[0].date == date(2026, 2, 5)
        assert rows[1].date == date(2026, 2, 5)  # inherited
        assert rows[1].symbol == "SYN2"


class TestWithdrawalsAndDeposits:
    def test_withdrawal_with_money_link(self):
        text = _wrap(
            "02/02 Withdrawal MoneyLinkTxn TfrSYNTHETIC-BANKNA,N/A (10,000.00)\n"
        )
        rows = pp.parse_transactions(text)
        assert len(rows) == 1
        r = rows[0]
        assert r.category == "Withdrawal"
        assert r.action == "MoneyLinkTxn"
        assert r.symbol is None
        assert r.amount == -10000.0
        assert r.quantity is None
        assert r.price is None

    def test_deposit_wired_funds(self):
        text = _wrap(
            "02/04 Deposit FundsReceived WIREDFUNDSRECEIVED 1,000,000.00\n"
        )
        rows = pp.parse_transactions(text)
        assert len(rows) == 1
        r = rows[0]
        assert r.category == "Deposit"
        assert r.action == "FundsReceived"
        assert r.amount == 1000000.0


class TestDividendsAndInterest:
    def test_cash_dividend(self):
        text = _wrap(
            "02/05 Dividend CashDividend SYN3 SYNTHETICTHREEFUND 300.00\n"
        )
        rows = pp.parse_transactions(text)
        assert len(rows) == 1
        r = rows[0]
        assert r.category == "Dividend"
        assert r.action == "CashDividend"
        assert r.symbol == "SYN3"
        assert r.amount == 300.00

    def test_nra_tax_withholding(self):
        text = _wrap(
            "02/05 Dividend NRATax SYN3 SYNTHETICTHREEFUND (45.00)\n"
        )
        rows = pp.parse_transactions(text)
        assert len(rows) == 1
        r = rows[0]
        assert r.category == "Dividend"
        assert r.action == "NRATax"
        assert r.amount == -45.00

    def test_credit_interest(self):
        text = _wrap(
            "02/27 Interest CreditInterest SCHWAB1INT01/01-01/31 1.00\n"
        )
        rows = pp.parse_transactions(text)
        assert len(rows) == 1
        r = rows[0]
        assert r.category == "Interest"
        assert r.action == "CreditInterest"
        assert r.amount == 1.00


class TestSectionBoundaries:
    def test_ignores_text_before_section(self):
        text = (
            PERIOD_HEADER
            + "Some preamble about positions etc.\n"
            + "02/01 Purchase XXXX FAKE 1.0 1.0 (1.00)\n"  # outside section, must NOT be parsed
            + "Transaction Details\n"
            + "Symbol/ Price/Rate\n"
            + "02/02 Sale SYN1 SYNTHETICONE (100.0000) 100.0000 0.01 10,000.00 50.00,(ST)\n"
            + "TotalTransactions ($1) $2\n"
        )
        rows = pp.parse_transactions(text)
        assert len(rows) == 1
        assert rows[0].symbol == "SYN1"

    def test_ignores_text_after_terminator(self):
        text = _wrap(
            "02/02 Sale SYN1 SYNTHETICONE (100.0000) 100.0000 0.01 10,000.00 50.00,(ST)\n"
        ) + "02/28 Purchase XXXX SHOULDNOTAPPEAR 1.0 1.0 (1.00)\n"
        rows = pp.parse_transactions(text)
        assert len(rows) == 1

    def test_pending_open_activity_terminates(self):
        text = (
            PERIOD_HEADER
            + "Transaction Details\n"
            + "Symbol/ Price/Rate\n"
            + "02/02 Sale SYN1 SYNTHETICONE (100.0000) 100.0000 0.01 10,000.00 50.00,(ST)\n"
            + "Pending / Open Activity\n"
            + "Pending 02/27 Purchase SYN9 SYNTHETICULTRA 5.0 2.00 (10.00)\n"  # after terminator
        )
        rows = pp.parse_transactions(text)
        assert len(rows) == 1
        assert rows[0].symbol == "SYN1"


class TestNumberParsing:
    @pytest.mark.parametrize("s,expected", [
        ("1.00", 1.0),
        ("-1.00", -1.0),
        ("(1.00)", -1.0),
        ("1,234.56", 1234.56),
        ("(1,234.56)", -1234.56),
        ("0.00", 0.0),
        ("100,000.0000", 100000.0),
        ("", None),
        ("notanumber", None),
        (None, None),
    ])
    def test_parse_number(self, s, expected):
        assert pp._parse_number(s) == expected


class TestRawLinesPreserved:
    def test_raw_lines_contains_source(self):
        text = _wrap(
            "02/02 Sale SYN1 SYNTHETICONE (100.0000) 100.0000 0.01 10,000.00 50.00,(ST)\n"
            "IndustryFee$0.01\n"
        )
        rows = pp.parse_transactions(text)
        assert len(rows) == 1
        r = rows[0]
        # raw_lines preserves the source for debugging / diff
        # against future parser changes.
        assert len(r.raw_lines) >= 1
        assert any("Sale" in ln for ln in r.raw_lines)


class TestToDict:
    def test_to_dict_serializes_date(self):
        text = _wrap(
            "02/02 Sale SYN1 SYNTHETICONE (100.0000) 100.0000 0.01 10,000.00 50.00,(ST)\n"
        )
        rows = pp.parse_transactions(text)
        d = rows[0].to_dict()
        assert d["date"] == "2026-02-02"  # ISO string, not date object
        assert d["category"] == "Sale"
        assert "raw_lines" in d  # raw_lines kept for diff / debug

    def test_to_dict_handles_none_fields(self):
        text = _wrap(
            "02/04 Deposit FundsReceived WIREDFUNDSRECEIVED 1,000,000.00\n"
        )
        rows = pp.parse_transactions(text)
        d = rows[0].to_dict()
        assert d["quantity"] is None
        assert d["realized_gain_loss"] is None
        assert d["term"] is None


class TestStatementYearOverride:
    def test_explicit_year_overrides_period_header(self):
        text = _wrap(
            "02/02 Sale SYN1 SYNTHETICONE (100.0000) 100.0000 0.01 10,000.00 50.00,(ST)\n"
        )
        rows = pp.parse_transactions(text, statement_year=2099)
        assert rows[0].date == date(2099, 2, 2)

    def test_missing_period_raises_without_override(self):
        text = (
            "Transaction Details\n"
            "Symbol/ Price/Rate\n"
            "02/02 Sale SYN1 SYNTHETICONE (100.0000) 100.0000 0.01 10,000.00 50.00,(ST)\n"
            "TotalTransactions ($1) $2\n"
        )
        with pytest.raises(ValueError, match="statement period"):
            pp.parse_transactions(text)
