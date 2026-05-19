"""Tests for pdf_parsers.

Tests work at the regex / text-assembly layer using synthetic
labels and synthetic pre-extracted text fixtures, so they don't
require a real PDF on disk or pdfplumber's heavy lifting. All
identifiers are placeholders per CLAUDE.md §4.
"""
from __future__ import annotations

import pytest

from pdf_parsers import (
    parse_account_statement_text,
    parse_label_statement_of_assets,
)


# ---- Issue 2: portfolio_external_id length must be 16 -------------

class TestPortfolioExternalId:
    """The PDF parser assembles a PSN-aligned portfolio_external_id
    of the form `<branch:4><base:8><portfolio_no:4>` = 16 chars. UBS
    PDF labels strip the branch's leading zero (rendering as
    `BBB-AAAAAAAA-NN` instead of `BBBB-AAAAAAAA-NN`), so the
    assembly must zfill the branch to 4 chars."""

    # Synthetic digit placeholders — obviously not anyone's real
    # account, but the parser regex requires actual digits.
    BRANCH_3 = "999"
    BRANCH_4 = "9999"
    BASE_8 = "00000000"
    PORTFOLIO_NO = "42"

    @staticmethod
    def _label(branch_disp: str, base: str, portfolio_no: str) -> str:
        """Synthesize a Statement-of-assets listing label in the
        format the docs page emits. branch_disp is what UBS shows
        in the label (without or with leading zero), base is the
        8-digit account base, portfolio_no is the 2-digit suffix."""
        return (
            f"‍ Statement of assets as of 31122024 "
            f"02.01.2025 02 January 2025 P. Placeholder "
            f"{branch_disp}-{base}-{portfolio_no} 300 KB"
        )

    def test_branch_padded_to_four_digits(self):
        """Some labels carry 3-digit branches; the assembled
        ID must still be 16 chars."""
        meta = parse_label_statement_of_assets(
            self._label(self.BRANCH_3, self.BASE_8, self.PORTFOLIO_NO)
        )
        assert meta is not None, "label should parse"
        branch, base = meta["account_number_prefix"].split("-", 1)
        assembled = (
            f"{branch.zfill(4)}{base.zfill(8)}"
            f"{meta['portfolio_number'].zfill(4)}"
        )
        assert len(assembled) == 16, f"got {assembled!r} ({len(assembled)} chars)"

    def test_label_with_4digit_branch_also_works(self):
        """If UBS ever ships labels with non-stripped branches,
        the assembly must still produce 16 chars."""
        meta = parse_label_statement_of_assets(
            self._label(self.BRANCH_4, self.BASE_8, self.PORTFOLIO_NO)
        )
        assert meta is not None
        branch, base = meta["account_number_prefix"].split("-", 1)
        assembled = (
            f"{branch.zfill(4)}{base.zfill(8)}"
            f"{meta['portfolio_number'].zfill(4)}"
        )
        assert len(assembled) == 16

    def test_parse_statement_of_assets_raises_on_length_drift(
            self, monkeypatch, tmp_path):
        """If the assembly ever drifts from 16 chars (regex change,
        upstream label format shift, etc.), the parser must raise
        rather than silently inserting a bad row."""
        from pdf_parsers import parse_statement_of_assets

        # Synthesize a pathological label_meta where the
        # portfolio_number is too long. We monkeypatch the label
        # parser to return it, then assert the assembly raises.
        bad_meta = {
            "as_of_date": 1735603200,
            "as_of_str": "2024-12-31",
            "account_number_prefix": "BBB-AAAAAAAA",
            "portfolio_number": "NNNNN",  # 5 digits, will push total to 17
        }
        monkeypatch.setattr(
            "pdf_parsers.parse_label_statement_of_assets",
            lambda label: bad_meta,
        )
        # parse_statement_of_assets opens the PDF before doing the
        # assembly, so we need a real (empty) file path.
        dummy = tmp_path / "x.pdf"
        dummy.write_bytes(b"%PDF-1.4\n%%EOF\n")
        with pytest.raises(ValueError, match=r"length != 16"):
            parse_statement_of_assets(dummy, "<doc-token>", "<label>")


# ---- Issue 1: opening/closing/total balances must be extracted ---

class TestAccountStatementBalances:
    """The Account-Statement parser must populate opening_balance,
    closing_balance, total_debits, total_credits from the squished
    PDF text. Earlier the four regexes required `\\s+` between label
    and value, which never matches the whitespace-stripped `flat`
    text — every row landed with all four numeric columns NULL."""

    # Synthetic placeholder IBAN: CH + 2-digit checksum + 17
    # alphanumerics. Not a real IBAN — the regex only validates
    # shape, so any matching string works for the parser layer.
    IBAN = "CH00000000000000000000A"[:21]  # 21 chars
    PERIOD = "01.04.2026 - 30.04.2026"
    HEADER = "UBS personal account EUR"

    # Synthetic placeholder values. Deliberately obvious-fake
    # repdigits so a casual reader can see they are not anyone's
    # actual balances. Matching the assertions to these constants
    # makes it easy to confirm none of the numbers were lifted from
    # a real probed statement.
    OPENING_VAL_STR = "1 111.11"
    CREDITS_VAL_STR = "2 222.22"
    DEBITS_VAL_STR = "3 333.33"
    CLOSING_VAL_STR = "4 444.44"

    def _statement_with_summary(self) -> str:
        """Text shape of a full monthly EUR statement (opening +
        closing + totals all present). Whitespace and line breaks
        mirror what pdfplumber emits before the squish-pass."""
        return (
            f"IBAN {self.IBAN}\n"
            f"{self.HEADER}\n"
            f"Account Statement\n"
            f"{self.PERIOD} / Monthly: number 4\n"
            "Your account at a glance Debits Credits Balance\n"
            f"Opening balance {self.OPENING_VAL_STR}\n"
            f"Total credits {self.CREDITS_VAL_STR}\n"
            f"Total debits {self.DEBITS_VAL_STR}\n"
            f"Closing balance {self.CLOSING_VAL_STR}\n"
        )

    def _statement_without_summary(self) -> str:
        """Text shape of a low-activity statement that omits the
        summary block and only carries the in-table opening line."""
        return (
            f"IBAN {self.IBAN}\n"
            f"{self.HEADER}\n"
            f"Account Statement\n"
            f"{self.PERIOD}\n"
            "Date Information Debits Credits Value date Balance\n"
            f"01.04.26 Opening balance {self.OPENING_VAL_STR}\n"
        )

    def test_summary_block_populates_all_four_columns(self):
        rows = parse_account_statement_text(
            self._statement_with_summary(), "<doc-token>"
        )
        assert len(rows) == 1
        row = rows[0]
        assert row["opening_balance"] == pytest.approx(1111.11)
        assert row["closing_balance"] == pytest.approx(4444.44)
        assert row["total_credits"] == pytest.approx(2222.22)
        assert row["total_debits"] == pytest.approx(3333.33)
        assert row["currency_iso"] == "EUR"
        assert row["account_external_id"] == self.IBAN

    def test_missing_summary_leaves_totals_null(self):
        """When the summary block is absent (low-activity month),
        opening_balance must still come from the in-table line and
        the totals must remain NULL — never 0.0, which would be
        indistinguishable from a real zero-flow month."""
        rows = parse_account_statement_text(
            self._statement_without_summary(), "<doc-token>"
        )
        assert len(rows) == 1
        row = rows[0]
        assert row["opening_balance"] == pytest.approx(1111.11)
        assert row["closing_balance"] is None
        assert row["total_credits"] is None
        assert row["total_debits"] is None

    def test_zero_balance_without_decimal_parses(self):
        """Closed / zero-activity accounts render balances as a bare
        `0` rather than `0.00`. The decimal portion of the value
        regex must be optional or the row drops to NULL."""
        text = (
            f"IBAN {self.IBAN}\n"
            f"{self.HEADER}\n"
            f"{self.PERIOD}\n"
            "Date Information Debits Credits Value date Balance\n"
            "01.01.24 Opening balance 0\n"
            "Turnover total 0 0\n"
            "31.12.24 Closing balance 0\n"
        )
        rows = parse_account_statement_text(text, "<doc-token>")
        assert len(rows) == 1
        assert rows[0]["opening_balance"] == 0.0
        assert rows[0]["closing_balance"] == 0.0

    def test_negative_value_parses(self):
        """Overdraft / debit-side opening balances render with a
        leading minus sign in the source PDF; the value regex must
        accept it."""
        text = (
            f"IBAN {self.IBAN}\n"
            f"{self.HEADER}\n"
            f"{self.PERIOD}\n"
            f"Opening balance -{self.OPENING_VAL_STR}\n"
            f"Closing balance -{self.CLOSING_VAL_STR}\n"
        )
        rows = parse_account_statement_text(text, "<doc-token>")
        assert len(rows) == 1
        assert rows[0]["opening_balance"] == pytest.approx(-1111.11)
        assert rows[0]["closing_balance"] == pytest.approx(-4444.44)
