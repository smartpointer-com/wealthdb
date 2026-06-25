"""Unit tests for the VIAC Reporting-PDF parser's row grammar.

Exercises parse_investment_report_text against a synthetic statement
text shaped like pypdfium2's extraction of a real report — no real
data: synthetic portfolio numbers / ISINs / fund names / amounts.
"""
from __future__ import annotations

import sys
from pathlib import Path

HERE = Path(__file__).resolve().parent
COLLECTOR = HERE.parent
sys.path.insert(0, str(COLLECTOR))

import pdf_parsers  # noqa: E402

# Synthetic report text. Mirrors the structure the parser relies on:
# an as-of line, per-portfolio `Portfolio <NUM> "<name>"` anchors,
# asset-class section headers immediately preceding their holdings
# rows, and the "3a Account" liquidity row. Multi-word sub-classes
# ("Developed Markets") and multi-word fund names ("Synthetic World
# Fund") are included to exercise the ISIN/FX-anchored column split.
SYNTHETIC = """\
Reporting as of 31.12.2099
For Test Person
Mandate Pillar 3a
... portfolio overview noise ...
Reporting date 31.12.2099 Portfolio 3.111.222.333.01 "Synthetic Portfolio A"
Asset classes Share in % Share in CHF
Liquidity 5.00% 100.00
Equity 90.00% 1860.00
Bonds 5.00% 100.00
Securities overview "Synthetic Portfolio A" column headers Share in CHF
Liquidity
3a Account CHF 100.00 0.30% Interest 5.00% 100.00
Equity
Switzerland CHF 2.50 Synthetic SMI Fund CH0000000011 1000.00 1200.00 20.00% 57.00% 1200.00
Developed Markets USD 1.20 Synthetic World Fund IE0000000022 500.00 550.00 10.00% 31.00% 660.00
Bonds
Switzerland CHF 0.80 Synthetic Bond Fund CH0000000033 900.00 950.00 5.56% 5.00% 760.00
Reporting date 31.12.2099 Portfolio 3.111.222.333.02 "Synthetic Portfolio B"
Securities overview "Synthetic Portfolio B" column headers Share in CHF
Liquidity
3a Account CHF 25.00 0.30% Interest 2.50% 25.00
Equity
Switzerland CHF 1.00 Synthetic SMI Fund CH0000000011 1000.00 1150.00 15.00% 97.50% 975.00
"""


def test_as_of_date():
    res = pdf_parsers.parse_investment_report_text(SYNTHETIC)
    assert res.as_of_date == "2099-12-31"


def test_position_counts_per_portfolio():
    res = pdf_parsers.parse_investment_report_text(SYNTHETIC)
    by_acct: dict[str, int] = {}
    for p in res.positions:
        by_acct[p.account_external_id] = by_acct.get(p.account_external_id, 0) + 1
    assert by_acct == {"3.111.222.333.01": 3, "3.111.222.333.02": 1}


def test_equity_row_fields():
    res = pdf_parsers.parse_investment_report_text(SYNTHETIC)
    p = next(p for p in res.positions
             if p.account_external_id == "3.111.222.333.01"
             and p.isin == "CH0000000011")
    assert p.name == "Synthetic SMI Fund"
    assert p.currency_code == "CHF"
    assert p.quantity == 2.50
    assert p.asset_price == 1200.00
    assert p.acquisition_price == 1000.00
    assert p.market_value_chf == 1200.00
    assert p.rate_of_return == 20.00
    assert abs(p.ratio - 0.57) < 1e-9
    assert p.asset_class == "equity"
    assert p.sub_asset_class == "Switzerland"


def test_multiword_name_and_subclass_and_foreign_ccy():
    res = pdf_parsers.parse_investment_report_text(SYNTHETIC)
    p = next(p for p in res.positions if p.isin == "IE0000000022")
    assert p.name == "Synthetic World Fund"
    assert p.sub_asset_class == "Developed Markets"
    assert p.currency_code == "USD"
    assert p.market_value_chf == 660.00


def test_section_header_drives_asset_class():
    res = pdf_parsers.parse_investment_report_text(SYNTHETIC)
    bond = next(p for p in res.positions if p.isin == "CH0000000033")
    # 'Switzerland' sub-class under the 'Bonds' section → bond, not
    # equity (the section header, not the sub-class, decides).
    assert bond.asset_class == "bond"
    assert bond.sub_asset_class == "Switzerland"


def test_cash_rows_routed_separately():
    res = pdf_parsers.parse_investment_report_text(SYNTHETIC)
    cash = {c.account_external_id: c.amount for c in res.cash}
    assert cash == {"3.111.222.333.01": 100.00, "3.111.222.333.02": 25.00}
    # The 3a Account liquidity row is NOT also emitted as a position.
    assert all(p.isin for p in res.positions)


def test_per_portfolio_reconciliation():
    # securities + cash per portfolio (a synthetic stand-in for the
    # real "Balance in CHF" reconciliation the parser passes on live
    # reports).
    res = pdf_parsers.parse_investment_report_text(SYNTHETIC)
    cash = {c.account_external_id: c.amount for c in res.cash}
    sec: dict[str, float] = {}
    for p in res.positions:
        sec[p.account_external_id] = sec.get(p.account_external_id, 0.0) + p.market_value_chf
    assert sec["3.111.222.333.01"] + cash["3.111.222.333.01"] == 2720.00
    assert sec["3.111.222.333.02"] + cash["3.111.222.333.02"] == 1000.00


def test_empty_text_is_safe():
    res = pdf_parsers.parse_investment_report_text("no portfolios here")
    assert res.as_of_date is None
    assert res.positions == []
    assert res.cash == []
