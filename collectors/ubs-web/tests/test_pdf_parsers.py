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
    parse_maturity_notice_text,
    parse_statement_of_assets_text,
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
            self, monkeypatch):
        """If the assembly ever drifts from 16 chars (regex change,
        upstream label format shift, etc.), the parser must raise
        rather than silently inserting a bad row. Exercised through
        the text helper — the assembly happens there, before any PDF
        content is consulted."""
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
        with pytest.raises(ValueError, match=r"length != 16"):
            parse_statement_of_assets_text("", "<doc-token>", "<label>")


# ---- Statement-of-assets securities: listed + alternatives --------

class TestStatementOfAssetsSecurities:
    """The Statement-of-assets securities walker must capture three
    row shapes off the Valor/ISIN anchor:

      1. Listed securities — the cost/market/gain% triple.
      2. Listed securities whose market price carries a one-letter
         qualifier (e.g. a structured product's "120.00 B 20.00%").
      3. Private-markets / SPV holdings — a single FX rate (or
         "n.a.") in place of the triple; the funded "Outstanding
         Shares" row carries the NAV, the n.a. commitment rows are 0.

    All identifiers are synthetic placeholders per CLAUDE.md §4 —
    the ISIN-shaped tokens use the reserved 'XX' prefix and repdigit
    bodies so they are obviously not real instruments."""

    # Synthetic Statement-of-assets listing label (3-digit branch,
    # all-zero base, portfolio 06). Same shape as the docs page emits.
    LABEL = (
        "‍ Statement of assets as of 31032026 "
        "02.04.2026 02 April 2026 P. Placeholder 999-00000000-06 300 KB"
    )

    def _soa_text(self, base_ccy: str = "USD") -> str:
        """A minimal Statement-of-assets text body: a Portfolio-01
        overview block (precious-metals asset-class total, no detail
        page) followed by a Detailed-positions section carrying one
        listed equity, one flagged structured product, one funded
        private-markets holding and one unfunded-commitment row."""
        return "\n".join([
            f"Valued in {base_ccy}",
            "Portfolio 01",
            "Liquidity 11 111 11 111 22.58",
            # market value + (equal) total + %NA, all single-space
            # separated like pdfplumber emits. The 6-digit value is
            # deliberate: its groups line up so a naive capture would
            # slurp both equal columns into one doubled number
            # (987 654 987 654) — the bug this fixture guards against.
            "Precious metals & commodities 987 654 987 654 75.00",
            "Net assets 1 245 678",
            "Detailed positions",
            # 1. listed equity (cost / market / gain% triple)
            "100 Reg.shs Placeholder Equity AG USD 10.000000 12.50 25.00% 1 250 5.00",
            "Financials",
            "Valor 111 - ISIN XX0000000011",
            # 2. structured product with a one-letter price flag ('B')
            "200 Example Structured Note USD 100.000000 120.00 B 20.00% 24 000 10.00",
            "Valor 222 - ISIN XX0000000022",
            # 3. funded private-markets Outstanding Shares (FX rate, NAV)
            "300 MVPX Placeholder Fund USD 1.2500 9 999 4.00",
            "Valor 333 - ISIN XX0000000033",
            # 4. unfunded commitment (n.a. price, 0 value) -> skipped
            "400 MVPX Placeholder Fund USD n.a. 0 0.00",
            "Valor 444 - ISIN XX0000000044",
            "Additional information Abbreviations",
        ])

    def _by_isin(self, rows):
        return {r["instrument_isin"]: r for r in rows
                if r["instrument_isin"]}

    def test_listed_equity_still_parses(self):
        """Regression guard: the cost/market/gain% triple path is
        unchanged by the added flag/PM handling."""
        rows = parse_statement_of_assets_text(
            self._soa_text(), "<doc-token>", self.LABEL)
        eq = self._by_isin(rows)["XX0000000011"]
        assert eq["market_value"] == pytest.approx(1250.0)
        assert eq["cost_price"] == pytest.approx(10.0)
        assert eq["market_price"] == pytest.approx(12.5)
        assert eq["sector"] == "Financials"

    def test_structured_product_with_price_flag(self):
        """The one-letter qualifier after the market price ('120.00 B')
        must not break the headline match."""
        rows = parse_statement_of_assets_text(
            self._soa_text(), "<doc-token>", self.LABEL)
        amc = self._by_isin(rows)["XX0000000022"]
        assert amc["market_value"] == pytest.approx(24000.0)
        assert amc["cost_price"] == pytest.approx(100.0)

    def test_private_market_outstanding_shares_captured(self):
        """A funded private-markets row (FX rate + NAV, no gain%) is
        captured via the PM-headline fallback, with the FX rate kept
        in exchange_rate_to_base and no cost/market price."""
        rows = parse_statement_of_assets_text(
            self._soa_text(), "<doc-token>", self.LABEL)
        pm = self._by_isin(rows)["XX0000000033"]
        assert pm["market_value"] == pytest.approx(9999.0)
        assert pm["units"] == pytest.approx(300.0)
        assert pm["exchange_rate_to_base"] == pytest.approx(1.25)
        assert pm["cost_price"] is None
        assert pm["market_price"] is None

    def test_unfunded_commitment_row_skipped(self):
        """The 'n.a.'-priced 0-value commitment row adds no position."""
        rows = parse_statement_of_assets_text(
            self._soa_text(), "<doc-token>", self.LABEL)
        assert "XX0000000044" not in self._by_isin(rows)

    def test_overview_precious_metals_synthesised_for_usd(self):
        """The precious-metals portfolio has no Detailed-positions
        page; its overview asset-class total is recovered as a
        synthetic position with a non-ISIN-shaped key, attributed to
        portfolio 01."""
        rows = parse_statement_of_assets_text(
            self._soa_text("USD"), "<doc-token>", self.LABEL)
        pm = [r for r in rows
              if r["description"] == "Precious metals & commodities"]
        assert len(pm) == 1
        row = pm[0]
        # Only the Market value column — NOT the doubled mv+total run.
        assert row["market_value"] == pytest.approx(987654.0)
        assert row["market_value"] != pytest.approx(987654987654.0)
        assert row["currency_iso"] == "USD"
        assert row["portfolio_external_id"].endswith("0001")
        # Synthetic key must not look like a real ISIN (len != 12).
        assert len(row["instrument_isin"]) != 12
        assert row["instrument_isin"].startswith("PM-")

    def test_overview_precious_metals_skipped_for_non_usd(self):
        """Only the USD-valued copy emits the synthetic, so the same
        holding (printed once per portfolio currency) collapses to a
        single deterministic row across the relationship's PDFs."""
        rows = parse_statement_of_assets_text(
            self._soa_text("CHF"), "<doc-token>", self.LABEL)
        assert not [r for r in rows
                    if r["description"] == "Precious metals & commodities"]


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


# ---- Maturity Notice (mortgage interest-roll) PDFs ----------------

class TestMaturityNotice:
    """Parses the per-mortgage quarterly maturity-notice PDFs.
    Fixture text mirrors the line structure pdfplumber produces; the
    bold-rendered headers ('AAss aatt …') are the actual on-page
    artefact pdfplumber's character-level extraction emits."""

    # Synthetic placeholder mortgage account number — NOT real.
    ACCT_LINE = "Account no. 999-12345678.MMM 0001"
    # Synthetic placeholder address.
    COLLATERAL_LINE = "Category EXAMPLE ROAD 1, 0000 EXAMPLECITY"

    def test_fixed_rate_quarter_end(self):
        text = (
            "UBS Fixed-Rate Mortgage CHF\n"
            f"{self.ACCT_LINE}\n"
            f"{self.COLLATERAL_LINE}\n"
            "MMaattuurriittyy nnoottiiccee\n"
            "AAss aatt 3300..0099..22002233\n"
            "General information Amount in CHF\n"
            "Current debt capital 1 234 567.89\n"
        )
        rows = parse_maturity_notice_text(text, "<doc-token>")
        assert len(rows) == 1
        row = rows[0]
        # Branch zfill 3 → 4, base zfill 8 → 8, hyphen → space.
        assert row["account_external_id"] == "0999 12345678.MMM 0001"
        assert row["currency_iso"] == "CHF"
        # Outstanding goes in as a liability (negative sign).
        assert row["outstanding_balance"] == pytest.approx(-1234567.89)
        assert row["product_name"] == "UBS Fixed-Rate Mortgage"
        assert row["rate_type"] == "fixed"
        assert row["collateral_description"] == \
            "EXAMPLE ROAD 1, 0000 EXAMPLECITY"
        # 2023-09-30 UTC midnight.
        from datetime import datetime, timezone
        assert datetime.fromtimestamp(
            row["as_of_date"], timezone.utc).date() \
            == datetime(2023, 9, 30).date()

    def test_saron_classified_as_variable(self):
        """SARON Mortgage is UBS's variable-rate product; rate_type
        should map to 'variable', not 'saron', so the canonical
        taxonomy stays rate-basis."""
        text = (
            "UBS SARON Mortgage CHF\n"
            f"{self.ACCT_LINE}\n"
            f"{self.COLLATERAL_LINE}\n"
            "MMaattuurriittyy nnoottiiccee\n"
            "AAss aatt 3311..1122..22002233\n"
            "Current debt capital 500 000.00\n"
        )
        rows = parse_maturity_notice_text(text, "<doc-token>")
        assert len(rows) == 1
        assert rows[0]["product_name"] == "UBS SARON Mortgage"
        assert rows[0]["rate_type"] == "variable"

    def test_non_mortgage_maturity_notice_returns_empty(self):
        """The doc_type 'Maturity notice' is also UBS-side used for
        bond / time-deposit notices that aren't mortgages. Without a
        recognised product line + Account no. + debt-capital line
        the parser must return [], not invent a row."""
        text = (
            "Some Other Notice\n"
            "Account no. unrelated\n"
            "As at 30.09.2023\n"
            "Total amount due 100.00\n"
        )
        assert parse_maturity_notice_text(text, "<doc-token>") == []

    def test_undouble_passthrough_when_not_bold(self):
        """Normal (non-doubled) header lines must still match."""
        text = (
            "UBS Fixed-Rate Mortgage CHF\n"
            f"{self.ACCT_LINE}\n"
            f"{self.COLLATERAL_LINE}\n"
            "As at 30.09.2023\n"
            "Current debt capital 100 000.00\n"
        )
        rows = parse_maturity_notice_text(text, "<doc-token>")
        assert len(rows) == 1
        from datetime import datetime, timezone
        assert datetime.fromtimestamp(
            rows[0]["as_of_date"], timezone.utc).date() \
            == datetime(2023, 9, 30).date()
