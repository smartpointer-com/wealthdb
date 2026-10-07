"""
Tests for the Portfolio Performance PDF parser.

Exercises the pure-Python `_pp_parse_asset_allocation` against a
synthetic layout-mode-text fixture (no actual PDF read required —
this isolates the parser from pypdf), plus a non-ASCII regression
guard on security names.

Run from the repo root inside the container:
    python3 -m unittest discover tests
"""

from __future__ import annotations

import sys
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(ROOT))

import load  # noqa: E402


# Synthetic asset-allocation page text. The whitespace mimics what
# pypdf's layout-mode extraction produced on a real Portfolio
# Performance PDF: section header line, column-header line, one
# blank/sub-header line for bonds ("incl. accrued interests"), then
# one row per position. Bond accrued-interest figures sit on their
# own line below the row and are tolerated (skipped) by the parser.
SYNTHETIC_ASSET_ALLOCATION = """\
 2.   Asset allocation

  Cash
         Currency        Reference                   Description                                       Value           Exchange rate                    Valuation in CHF              %
              CHF          1234567 00                   Current Account (  CHF)                       100.00               1.00000                           100.00     0.01

                                                                                              Total cash                                                                                                        100.00     0.01

  Bonds in    CHF
          Quantity     Security                                          ISIN              Average price           Market Price              Price date              Valuation in CHF              %
                                                                                                                                                                  incl. accrued interests
         100'000           FAKE Bond 2.000% 31.12.2035                XX0000000001           102.500%               101.000%             31.12.2025          101'000.00      3.95
                                                                                                                                                                       500.00
                                                                                                       Total bonds in     CHF                                  101'500.00      3.97

  Funds in          CHF
          Quantity     Security                                          ISIN              Average price           Market Price              Price date              Valuation in CHF              %
             5'000         Fictional Equity Fund                       XX0000000002              100.000              120.000             31.12.2025         600'000.00     23.42
            10'000         Fonds Société Fictive €                     XX0000000003              200.000              180.000             31.12.2025       1'800'000.00     70.26
             2'000         Test ETF International                      XX0000000004               50.000               60.000             31.12.2025         120'000.00      4.68

                                                                                                       Total funds in   CHF                                  2'520'000.00     98.36

  Total assets in CHF                                                                                                                                       2'621'600.00
"""


class PortfolioPerformanceParserTests(unittest.TestCase):
    def test_basic_layout(self):
        rows = load._pp_parse_asset_allocation(
            SYNTHETIC_ASSET_ALLOCATION, Path("synthetic.pdf"),
        )
        # 1 bond + 3 funds = 4 securities; cash row is intentionally
        # excluded.
        self.assertEqual(len(rows), 4)
        # Index by ISIN for easier assertions.
        by_isin = {r["isin"]: r for r in rows}
        bond = by_isin["XX0000000001"]
        self.assertEqual(bond["asset_class"], "Bonds")
        self.assertEqual(bond["currency"], "CHF")
        self.assertEqual(bond["quantity"], 100_000.0)
        self.assertEqual(bond["name"], "FAKE Bond 2.000% 31.12.2035")
        self.assertEqual(bond["avg_price"], 102.5)
        self.assertEqual(bond["market_price"], 101.0)
        self.assertEqual(bond["price_date"], "2025-12-31")
        self.assertEqual(bond["valuation_chf"], 101_000.0)
        self.assertEqual(bond["account_pct"], 3.95)

        fund = by_isin["XX0000000002"]
        self.assertEqual(fund["asset_class"], "Funds")
        self.assertEqual(fund["valuation_chf"], 600_000.0)

    def test_non_ascii_name_round_trip(self):
        rows = load._pp_parse_asset_allocation(
            SYNTHETIC_ASSET_ALLOCATION, Path("synthetic.pdf"),
        )
        by_isin = {r["isin"]: r for r in rows}
        # Name with diacritics and euro sign must survive cleanly.
        self.assertEqual(by_isin["XX0000000003"]["name"], "Fonds Société Fictive €")

    def test_price_quote_from_percent_marker(self):
        rows = load._pp_parse_asset_allocation(
            SYNTHETIC_ASSET_ALLOCATION, Path("synthetic.pdf"),
        )
        quotes = {r["isin"]: r["price_quote"] for r in rows}
        self.assertEqual(quotes["XX0000000001"], "percent")
        self.assertEqual(quotes["XX0000000002"], "unit")

    def test_mixed_price_quote_fails(self):
        # One price in percent, the other per unit: the row is not read.
        text = SYNTHETIC_ASSET_ALLOCATION.replace("101.000%", "101.000")
        with self.assertRaises(SystemExit):
            load._pp_parse_asset_allocation(text, Path("synthetic.pdf"))

    def test_cash_rows_excluded(self):
        rows = load._pp_parse_asset_allocation(
            SYNTHETIC_ASSET_ALLOCATION, Path("synthetic.pdf"),
        )
        for r in rows:
            self.assertNotEqual(r["asset_class"], "Cash")

    def test_swiss_number_parser(self):
        # Apostrophe thousand separators, decimal point, %.
        self.assertEqual(load._pp_to_number("1'234'567.89"), 1234567.89)
        self.assertEqual(load._pp_to_number("106.150%"), 106.15)
        self.assertEqual(load._pp_to_number("0.00"), 0.0)


if __name__ == "__main__":
    unittest.main()
