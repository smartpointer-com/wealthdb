#!/usr/bin/env python3
"""
Parsers for VIAC "Reporting" (INVESTMENT_REPORTING) statement PDFs.

VIAC's REST API only exposes the *current* holdings snapshot
(``assetsOverview``). The periodic "Reporting" PDFs in the document
archive are the only source of *historical* per-position holdings:
they list, per portfolio, every fund held with its ISIN, units,
prices and CHF market value as of a period-end date — going back to
the contract's first year. Parsing them lets the silver layer
reconstruct a position time series long before live scraping began.

Cadence observed: semi-annual through 2023, annual thereafter, plus
the occasional MANUAL_INVESTMENT_REPORTING the customer can request
on demand. One PDF covers every portfolio under the contract.

Text extraction goes through **pypdfium2** (Python bindings to
Google's PDFium). On these A4 reports it is ~3x faster than
pdftotext and ~50x faster than pdfplumber, and emits each holdings
row as a single clean space-delimited line — exactly the shape the
row grammar below wants. PDFium shuffles the page header/footer to
the end of each page's text block, but the holdings *table body*
(asset-class section header immediately followed by its rows) stays
contiguous, which is all the parser relies on.

The holdings table ("Securities overview") row grammar:

    <sub-asset-class…> <FX> <quantity> <name…> <ISIN> \
        <initial_price> <price> <return%> <share%> <market_value_chf>

anchored on the ISIN (a strong [A-Z]{2}[A-Z0-9]{9}[0-9] token in the
middle) and the three-letter FX code (the left boundary between the
sub-class and the quantity). Numerics are parsed from the right end,
so multi-word sub-classes and fund names don't shift the columns.
Asset class comes from the section header (Liquidity / Equity /
Bonds / Real Estate / Commodities / Alternative Investments) that
precedes each block — the sub-class column alone is ambiguous
("Switzerland" appears under both Equity and Real Estate).

The cash position (VIAC's "3a Account" liquidity row, no ISIN) is
returned separately so the loader can route it to ``cash_balances``.

This module has no DB dependency: ``parse_investment_report`` and
``parse_investment_report_text`` return plain dataclasses, so they
unit-test against a captured text fixture without a PDF.
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field
from pathlib import Path

from collectorkit.pdf import extract_text_pdfium as extract_text

# ISIN: ISO 6166 — 2-letter country + 9 alphanumeric + 1 check digit.
_ISIN_RE = re.compile(r"^[A-Z]{2}[A-Z0-9]{9}[0-9]$")
# FX: a bare three-letter currency code (CHF / USD / EUR / CAD / …).
# No VIAC sub-asset-class label is three uppercase letters, so the
# first such token reliably marks the sub-class → quantity boundary.
_FX_RE = re.compile(r"^[A-Z]{3}$")
# Per-portfolio section anchor on the report's evaluation page:
#   `… Portfolio 3.NNN.NNN.NNN.NN "Portfolio 2021"`
# The dotted 5-segment number is the portfolio's account_external_id;
# the quoted string is its display name. The looser `«name»` and
# unquoted overview forms are deliberately NOT matched.
_PORTFOLIO_ANCHOR_RE = re.compile(
    r'Portfolio\s+(\d\.\d{3}\.\d{3}\.\d{3}\.\d{2})\s+"([^"]+)"'
)
# `Reporting as of DD.MM.YYYY` — the report's period-end (as-of) date.
_ASOF_RE = re.compile(r"Reporting as of\s+(\d{2})\.(\d{2})\.(\d{4})")

# Asset-class section headers, mapped to the canonical asset_class
# the silver layer uses (same target values as load.py's
# VIAC_ASSET_CLASS_MAP, keyed here by the report's own English
# section labels rather than the live API's SCREAMING_CASE keys).
_SECTION_CANON: dict[str, str] = {
    "liquidity": "money_market",
    "equity": "equity",
    "equities": "equity",
    "bonds": "bond",
    "real estate": "fund",
    "commodities": "metal",
    "alternative investments": "other",
}
_SECTION_HEADERS = {
    "Liquidity", "Equity", "Equities", "Bonds",
    "Real Estate", "Commodities", "Alternative Investments",
}

# The cash/liquidity holding row VIAC prints under "Liquidity".
_CASH_ROW_PREFIX = "3a Account"


@dataclass
class ReportPosition:
    """One securities holding within one portfolio as of the report
    date. Field names line up with the silver `positions` columns."""
    account_external_id: str
    instrument_external_id: str          # ISIN
    isin: str
    name: str
    currency_code: str                   # native fund currency (FX column)
    quantity: float                      # units held
    market_value_chf: float              # CHF market value ("Share in CHF")
    acquisition_price: float | None      # per-unit cost basis, CHF ("Initial price")
    asset_price: float | None            # per-unit market price, CHF ("Price")
    rate_of_return: float | None         # since inception, percent
    ratio: float | None                  # fraction of portfolio NAV (0..1)
    asset_class: str                     # canonical (equity/bond/fund/…)
    viac_section: str                    # raw section header (Equity/Bonds/…)
    sub_asset_class: str                 # sub-class column (Switzerland/…)


@dataclass
class ReportCash:
    """The 3a-account liquidity balance within one portfolio."""
    account_external_id: str
    currency: str
    amount: float


@dataclass
class ReportParseResult:
    as_of_date: str | None               # ISO 'YYYY-MM-DD', or None if absent
    positions: list[ReportPosition] = field(default_factory=list)
    cash: list[ReportCash] = field(default_factory=list)


def _num(token: str) -> float:
    """Parse a Swiss-formatted number: apostrophe thousands separator
    (1'719.35), optional trailing percent, optional leading sign."""
    return float(token.replace("'", "").replace("%", ""))


def parse_investment_report(pdf_path: Path | str) -> ReportParseResult:
    """Parse a VIAC Reporting PDF into positions + cash. Thin wrapper
    over the text-variant so the row grammar can be unit-tested
    against a captured fixture."""
    return parse_investment_report_text(extract_text(pdf_path))


def parse_investment_report_text(text: str) -> ReportParseResult:
    """Parse already-extracted report text. See module docstring for
    the row grammar."""
    lines = text.splitlines()

    m = _ASOF_RE.search(text)
    as_of = f"{m.group(3)}-{m.group(2)}-{m.group(1)}" if m else None
    result = ReportParseResult(as_of_date=as_of)

    # Segment the document into per-portfolio regions delimited by the
    # `Portfolio <NUM> "<name>"` evaluation-page anchor. PDFium keeps
    # pages in order, so a portfolio's holdings always fall between its
    # anchor and the next portfolio's anchor.
    anchors: list[tuple[int, str]] = []
    seen: set[str] = set()
    for i, line in enumerate(lines):
        am = _PORTFOLIO_ANCHOR_RE.search(line)
        if am and am.group(1) not in seen:
            seen.add(am.group(1))
            anchors.append((i, am.group(1)))
    if not anchors:
        return result
    bounds = anchors + [(len(lines), "")]

    for k in range(len(bounds) - 1):
        start, account = bounds[k]
        end = bounds[k + 1][0]
        _parse_portfolio_region(lines[start:end], account, result)
    return result


def _parse_portfolio_region(
    lines: list[str], account: str, result: ReportParseResult
) -> None:
    current_section: str | None = None
    for raw in lines:
        s = raw.strip()
        if s in _SECTION_HEADERS:
            current_section = s
            continue
        toks = s.split()

        # Cash / liquidity row: "3a Account CHF <amt> <rate>% Interest
        # <share%> <amt>". No ISIN; market value is the trailing token.
        if s.startswith(_CASH_ROW_PREFIX) and current_section == "Liquidity":
            try:
                result.cash.append(
                    ReportCash(account_external_id=account,
                               currency="CHF", amount=_num(toks[-1]))
                )
            except (ValueError, IndexError):
                pass
            continue

        pos = _parse_security_row(toks, account, current_section)
        if pos is not None:
            result.positions.append(pos)


def _parse_security_row(
    toks: list[str], account: str, section: str | None
) -> ReportPosition | None:
    """Parse one securities-overview row, or None if the line isn't a
    holding. Anchors on the ISIN token and the FX token; reads the
    five trailing numerics from the right."""
    isin_idx = next((j for j, t in enumerate(toks) if _ISIN_RE.match(t)), None)
    if isin_idx is None:
        return None
    after = toks[isin_idx + 1:]
    if len(after) < 5:
        return None
    before = toks[:isin_idx]
    fx_idx = next((j for j, t in enumerate(before) if _FX_RE.match(t)), None)
    if fx_idx is None or fx_idx + 1 >= len(before):
        return None
    try:
        market_value = _num(after[-1])
        share = _num(after[-2])
        ret = _num(after[-3])
        price = _num(after[-4])
        initial_price = _num(after[-5])
        quantity = _num(before[fx_idx + 1])
    except ValueError:
        return None
    fx = before[fx_idx]
    sub_class = " ".join(before[:fx_idx])
    name = " ".join(before[fx_idx + 2:])
    canonical = _SECTION_CANON.get((section or "").lower(), "other")
    return ReportPosition(
        account_external_id=account,
        instrument_external_id=toks[isin_idx],
        isin=toks[isin_idx],
        name=name,
        currency_code=fx,
        quantity=quantity,
        market_value_chf=market_value,
        acquisition_price=initial_price,
        asset_price=price,
        rate_of_return=ret,
        ratio=share / 100.0,
        asset_class=canonical,
        viac_section=section or "",
        sub_asset_class=sub_class,
    )


if __name__ == "__main__":
    # Ad-hoc: `python3 pdf_parsers.py <report.pdf>...` prints a
    # per-portfolio summary (no monetary detail) for spot checks.
    import sys

    for arg in sys.argv[1:]:
        res = parse_investment_report(arg)
        by_acct: dict[str, int] = {}
        for p in res.positions:
            by_acct[p.account_external_id] = by_acct.get(p.account_external_id, 0) + 1
        print(f"{arg}: as_of={res.as_of_date} "
              f"portfolios={len(by_acct)} positions={len(res.positions)} "
              f"cash_rows={len(res.cash)}")
