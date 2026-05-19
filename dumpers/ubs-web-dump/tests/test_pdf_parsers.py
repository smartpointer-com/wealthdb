"""Tests for pdf_parsers.

Tests work at the regex / text-assembly layer using synthetic
labels and synthetic pre-extracted text fixtures, so they don't
require a real PDF on disk or pdfplumber's heavy lifting. All
identifiers are placeholders per CLAUDE.md §4.
"""
from __future__ import annotations

import pytest

from pdf_parsers import parse_label_statement_of_assets


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
