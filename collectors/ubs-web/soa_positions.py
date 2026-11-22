"""A Statement of assets: the positions, the precious-metals totals of
the relationship overview, and (soa_transaction_list.py) the
transaction list.

Positions are read from the page text, line by line, anchored on the
"Valor <num> - ISIN <code>" line of each holding and the IBAN line of
each cash account. The metadata comes from the document's listing label
(as-of date, portfolio number), or from its own first page for a
document delivered by hand.
"""
from __future__ import annotations

import json
import re

from datetime import date

from collectorkit.words import Extracted

import soa_transaction_list
from pdf_common import (
    LONG_DATE, document_text, dmy_to_iso, dmy_to_unix, iban_in, long_date,
    to_float, to_unix,
)


# ============================================================
# Label parsing
# ============================================================

_LABEL_STMT_OF_ASSETS_RE = re.compile(
    r"Statement of assets as of "
    r"(?P<day>\d{2})(?P<month>\d{2})(?P<year>\d{4})\s+"
    r"\d{2}\.\d{2}\.\d{4}\s+"
    r"\d{2}\s\S+\s\d{4}\s+.*?"
    r"\b(?P<acct_no>\d{3,4}-\d+)-(?P<portfolio_no>\d+)\b"
)


def parse_label_statement_of_assets(label: str) -> dict | None:
    """Extract as-of date and portfolio number from the listing
    label of a Statement-of-assets PDF."""
    m = _LABEL_STMT_OF_ASSETS_RE.search(label)
    if not m:
        return None
    as_of = date(int(m["year"]), int(m["month"]), int(m["day"]))
    return {
        "as_of_date": to_unix(as_of),
        "as_of_str": as_of.isoformat(),
        "account_number_prefix": m["acct_no"],     # e.g. 'BBB-AAAAAAAA'
        "portfolio_number": m["portfolio_no"],     # e.g. '01'
    }


# The same three facts, printed by the document itself on page 1:
#
#   Statement of assets
#   As of 7 March 2024
#   Portfolio 999-1234567-42, valued in Swiss Franc (CHF)
#
# A statement UBS delivered by hand rather than through the e-banking
# archive carries no listing row, so there is no label to read it from —
# see `statement_of_assets_body_meta`.
_BODY_STMT_OF_ASSETS_TITLE_RE = re.compile(r"^\s*Statement of assets\s*$", re.M)
_BODY_STMT_OF_ASSETS_PORTFOLIO_RE = re.compile(
    r"^\s*Portfolio\s+(?P<acct_no>\d{3,4}-\d+)-(?P<portfolio_no>\d+)\b", re.M
)

_BODY_STMT_OF_ASSETS_ASOF_RE = re.compile(rf"^\s*As of\s+{LONG_DATE}\s*$", re.M)


def statement_of_assets_body_meta(full_text: str) -> dict | None:
    """The same metadata `parse_label_statement_of_assets` reads, taken
    from the document's own first page instead of its listing row.

    Returns None unless all three anchors are present, so a PDF that is
    not a Statement of assets — or one whose text extraction failed —
    is declined rather than half-identified. The two roads agree on a
    document that has both; the header's `As of` line is the one the
    label states, not the `valued as of` note a year-end statement can
    print a day or two earlier.
    """
    if not _BODY_STMT_OF_ASSETS_TITLE_RE.search(full_text):
        return None
    as_of_m = _BODY_STMT_OF_ASSETS_ASOF_RE.search(full_text)
    portfolio_m = _BODY_STMT_OF_ASSETS_PORTFOLIO_RE.search(full_text)
    if not as_of_m or not portfolio_m:
        return None
    as_of = long_date(as_of_m)
    if as_of is None:
        return None
    return {
        "as_of_date": to_unix(as_of),
        "as_of_str": as_of.isoformat(),
        "account_number_prefix": portfolio_m["acct_no"],
        "portfolio_number": portfolio_m["portfolio_no"],
    }


def _statement_of_assets_meta(full_text: str, label: str) -> dict | None:
    """A Statement of assets' metadata. The listing row comes first, so
    every document the archive served is read by its label; the document's
    own header answers only for one delivered by hand, which has no
    listing row at all."""
    return (parse_label_statement_of_assets(label)
            or statement_of_assets_body_meta(full_text))


def psn_portfolio_external_id(meta: dict, portfolio_no: str | None = None
                              ) -> str:
    """The 16-char PSN-aligned id of the portfolio `meta` names, from
    either kind of statement-of-assets metadata: the listing label's or
    the document's own. `portfolio_no` names another portfolio of the
    same relationship instead.

    The id is the 4-digit branch, the 8-digit base and the 4-digit
    portfolio number, all zero-padded. The statement strips the branch's
    leading zero (`BBB-AAAAAAAA-NN`), and the padding restores it, so the
    id joins to PSN's `portfolios.portfolio_external_id` directly. Gold
    joins on it, and an id of the wrong length would silently
    double-count every position, so a length drift raises here with the
    printed parts in the message.
    """
    branch, base = meta["account_number_prefix"].split("-", 1)
    portfolio_no = portfolio_no or meta["portfolio_number"]
    psn_portfolio = f"{branch.zfill(4)}{base.zfill(8)}{portfolio_no.zfill(4)}"
    if len(psn_portfolio) != 16:
        raise ValueError(
            f"portfolio_external_id length != 16: {psn_portfolio!r} "
            f"(from acct_no={meta['account_number_prefix']!r}, "
            f"portfolio_no={portfolio_no!r})"
        )
    return psn_portfolio


# ============================================================
# Statement of assets parser
# ============================================================

# `Valued in EUR` (rendered as `ValuedinEUR` or `Valued in EUR` etc.)
_BASE_CCY_RE = re.compile(r"Valued in (?P<ccy>[A-Z]{3})\b")


def _base_currency(full_text: str) -> str | None:
    """The currency a Statement of assets is valued in."""
    m = _BASE_CCY_RE.search(full_text)
    return m["ccy"] if m else None


# Cash position rows in the "Liquidity - Accounts" section:
#   "CHF 1 234.56 UBS Personal Account CHF 9 876.54 1 234 11.11"
#   "                                                        0.00"   (accrued)
#   "                                CHKK BBBB RRRR AAAA AAAA C"
_CASH_AMOUNT_LINE_RE = re.compile(
    r"^(?P<ccy>[A-Z]{3})\s+(?P<units>-?[\d\s']+(?:\.\d+)?)\s+(?P<desc>.+?)\s+"
    r"(?P<opening>[\d\s']+(?:\.\d+)?)\s+(?:(?P<fx>\d+\.\d+)\s+)?"
    r"(?P<market_value>-?[\d\s']+)\s+(?P<pct>-?\d+\.\d{2})\s*$"
)

# Securities-position anchor line:
#   "Valor 123456 - ISIN DE0000000080"
_VALOR_ISIN_RE = re.compile(
    r"^\s*Valor\s+(?P<valor>\S+)\s+-\s+ISIN\s+(?P<isin>[A-Z0-9]{12})\s*$"
)

# The outer columns every holding headline prints: the units, the
# description and the currency on the left, the % NA at the right end.
# The headline patterns below differ only in the figures between them.
_HEADLINE_LEFT = (r"^\s*(?P<units>-?[\d\s']+(?:\.\d+)?)\s+(?P<desc>.+?)\s+"
                  r"(?P<ccy>[A-Z]{3})\s+")
_HEADLINE_RIGHT = r"\s+(?P<pct_na>-?\d+\.\d{2})\s*$"

# Securities-position headline:
#   "100 Reg.shs Example Equity AG (XMPL) EUR 100.000000 120.5 10.00% 12 050 1.25"
#
# UBS prints a one-letter price qualifier after the market price on
# some rows — e.g. a structured product's estimated/indicative
# price renders as "120.00 B 20.00%" (synthetic example). The optional
# `[A-Za-z]` flag group swallows it; without it the whole headline
# would fail to match and the position would drop silently.
_SECURITY_HEADLINE_RE = re.compile(
    _HEADLINE_LEFT
    + r"(?P<cost_price>[\d\s']+\.\d+)\s+"
    r"(?P<market_price>[\d\s']+\.?\d*)\s+(?:[A-Za-z]\s+)?"
    r"(?P<gain_pct>-?\d+\.\d+%)\s+"
    r"(?P<market_value>-?[\d\s']+)"
    + _HEADLINE_RIGHT
)

# Private-markets / alternatives headline. UBS-sponsored Private Markets
# funds and SPV interests print a single figure (or the literal "n.a.")
# where listed securities print the cost-price / market-price /
# market-gain triple. That figure is the market price, the fund's NAV
# per unit: units × price is the market value. Synthetic examples of the
# two forms:
#   "1 000 Example PE Fund   USD  1.0500  1 050  5.00"
#   "2 000 Example PE Fund   USD  n.a.    0      0.00"
# The first form is the funded "Outstanding Shares" holding (real
# NAV in market_value); the "n.a." form is a Net/Unfunded Commitment
# tracking row with a 0 market value. Tried only after
# _SECURITY_HEADLINE_RE, so a line both read is a listed security.
_PM_HEADLINE_RE = re.compile(
    _HEADLINE_LEFT
    + r"(?P<price>n\.a\.|[\d\s']+\.\d+)\s+"
    r"(?P<market_value>-?[\d\s']+)"
    + _HEADLINE_RIGHT
)

# The two patterns above find where one figure ends and the next begins
# by the decimal point a price prints. Three headline shapes print a
# price without one, or leave a column blank, so the figures run together
# (synthetic examples):
#
#   "10 Reg.shs Example AG  CHF 1 200 1 150 -4.17% 11 500 1.00"
#       an integer cost price (and here an integer market price);
#   "10 Reg.shs Example AG  CHF 43.50 43.50 435 1.00"
#       a blank market gain, printed when the price has not moved;
#   "1 000 Example PE Fund  USD 1 1 000 5.00"
#       an integer private-markets price.
#
# For a block neither pattern reads, `_HEADLINE_FIGURES_RE` takes the
# figures between the currency and the % NA as a run of tokens, and
# `_headline_readings` splits the run every way its digit grouping
# allows (a figure is one to three digits, then groups of three). Each
# reading must agree with itself:
#
#   - a printed market gain is the market price over the cost price,
#     less one;
#   - a blank market gain means the two prices are the same figure;
#   - a private-markets row prints one price and no gain.
#
# A headline is read only when exactly one reading agrees.
_HEADLINE_TOKEN = r"(?:-?\d[\d']*(?:\.\d+)?%?|[A-Za-z])"
_HEADLINE_FIGURES_RE = re.compile(
    _HEADLINE_LEFT
    + rf"(?P<figures>{_HEADLINE_TOKEN}(?:\s+{_HEADLINE_TOKEN})*)"
    + _HEADLINE_RIGHT
)
# One printed figure: digit groups, an optional fraction. No leading zero
# on the first group, so '1 000' cannot also read as '1' then '000'.
_GROUPED_FIGURE_RE = re.compile(
    r"-?(?:0|[1-9]\d{0,2}(?:[ ']\d{3})*)(?P<fraction>\.\d+)?")
# A printed market gain is rounded to 0.01 percentage points, and the
# prices it is computed from may be rounded in print too.
_GAIN_TOLERANCE_PCT = 0.01

# A holding prints up to four lines. The column header names what each
# line carries on the right-hand side:
#
#   Number/Amount Description … Cost price  Market price Market gain Market value % NA
#   Sector         Exchange rate Exchange rate Exchange gain Accrued interest
#   Duration       Cost value    Market price date Unrealized P/L
#   Yield          Last purchase
#
# The headline regexes above read line 1. The lines below it share the
# left-hand side with the wrapped description, the sector, and the
# distribution notes, so each is read from its right end (synthetic
# example of a holding in a currency other than the portfolio's):
#
#   "1 000 Reg.shs Example AG  GBP 20.000000 25.00 25.00% 32 500 4.50"
#   "(XMPL) All sectors 1.25000 1.30000 4.00%"
#   "Distribution: 01.06.2030 25 000 30.00%"
#   "Distribution amount: GBP 0.5 2.00% DY 15.03.2030"
#
# Line 2 carries the two exchange rates only when the holding's currency
# differs from the portfolio's: the average buy rate, then the current
# one. The exchange gain follows them and, where the holding accrues
# interest, the accrued interest. It is printed in the market-value
# column, so it is in the portfolio's currency. A holding in the
# portfolio's currency prints no rates, and nothing then tells an
# accrued interest from a figure that ends the wrapped description, so
# it is read only behind the rates.
#
# Line 3 carries the cost value (the units at their average cost,
# converted at the average buy rate, in the portfolio's currency), an
# optional market-price date, and the unrealized P/L as a percentage of
# the cost value. A holding whose market gain is blank leaves the P/L
# blank too, and line 3 then ends in the cost value
# (`_HOLDING_COST_NO_PL_RE`). Line 4 ends in the last purchase date
# where the statement prints one.
_HOLDING_FX_RE = re.compile(
    r"(?:^|\s)(?P<acquisition>\d+\.\d+)\s+(?P<current>\d+\.\d+)\s+"
    r"-?\d+\.\d+%(?:\s+(?P<accrued>-?\d{1,3}(?:[ ']\d{3})*(?:\.\d+)?))?\s*$"
)
_HOLDING_COST_RE = re.compile(
    r"(?:^|\s)(?P<cost>-?\d{1,3}(?:[ ']\d{3})*)\s+"
    r"(?:\d{2}\.\d{2}\.\d{4}\s+)?(?P<pl>-?\d+\.\d+)%\s*$"
)
_HOLDING_COST_NO_PL_RE = re.compile(
    r"(?:^|\s)(?P<cost>-?\d{1,3}(?:[ ']\d{3})*)\s*$"
)
_TRAILING_DATE_RE = re.compile(r"(?:^|\s)(?P<date>\d{2}\.\d{2}\.\d{4})\s*$")
# A distribution's ex-date, printed in the left-hand column of any line
# below the headline. Its date is the label's, never the last purchase.
_DISTRIBUTION_DATE_RE = re.compile(r"^\s*Distribution:\s+\d{2}\.\d{2}\.\d{4}")

# Overview asset-class line for a portfolio whose securities have no
# Detailed-positions page of their own (UBS does not issue a
# per-position Statement of assets for the precious-metals / custody
# portfolio — only the relationship overview carries its
# asset-class total). Anchored at column 0 so it matches the
# portfolio-block line, not the right-hand consolidated column.
#
# The line reads "<market value> <total> <%NA>", all single-space
# separated by pdfplumber. For precious metals the Total equals the
# Market value (no accrued interest), and a plain thousands-aware
# capture slurps BOTH equal columns into one doubled number whenever
# their digit-groups line up under single-space separation — e.g.
# "12 345 12 345 75.00" reads as 12 345 12 345. The pattern anchors
# the Market value by requiring the identical Total column to follow
# it (the (?P=mv) backreference) and the %NA after that, so only the
# first column is captured (synthetic example):
#   "Precious metals & commodities 12 345 12 345 75.00 ..."
_OVERVIEW_PORTFOLIO_RE = re.compile(r"^Portfolio\s+(?P<no>\d{2})\b")
_OVERVIEW_PRECIOUS_METALS_RE = re.compile(
    r"^Precious metals & commodities\s+"
    r"(?P<mv>\d{1,3}(?:[ ']\d{3})*)\s+(?P=mv)\s+-?\d+\.\d{2}\b"
)


def parse(extracted: Extracted, doc_token: str, label: str
          ) -> tuple[list[dict], list[dict], list[dict]]:
    """Both parts of a Statement of assets and its overview totals:
    the positions (one row per holding or cash line, for
    `historical_position_snapshots`), the transaction list (one row per
    listed booking, for `statement_trades`) and the relationship
    overview's precious-metals totals (`statement_overview_totals`).
    Returns (positions, trades, totals).

    The positions and the totals are read from the page text, the
    transaction list from the word positions of its own pages."""
    full_text = document_text(extracted)
    meta = _statement_of_assets_meta(full_text, label)
    if meta is None:
        return [], [], []
    base_ccy = _base_currency(full_text)
    positions = _positions(full_text, doc_token, meta, base_ccy)
    totals = _overview_totals(full_text, doc_token, meta, base_ccy)
    trades = soa_transaction_list.parse_transaction_list(
        [page for page in extracted.pages
         if soa_transaction_list.HEADER_TEXT in page.text],
        {"source_doc_token": doc_token,
         "as_of_date": meta["as_of_date"],
         "portfolio_external_id": psn_portfolio_external_id(meta),
         "reporting_currency_iso": base_ccy})
    return positions, trades, totals


def parse_statement_of_assets_text(full_text: str, doc_token: str,
                                   label: str) -> list[dict]:
    """The positions half of `parse`, on text already
    extracted, so the regex / line-walk layer can be exercised without a
    real PDF on disk."""
    label_meta = _statement_of_assets_meta(full_text, label)
    if label_meta is None:
        return []
    return _positions(full_text, doc_token, label_meta,
                      _base_currency(full_text))


def _positions(full_text: str, doc_token: str, label_meta: dict,
               base_ccy: str | None) -> list[dict]:
    """The position rows of a Statement of assets whose metadata is
    `label_meta` and whose base currency is `base_ccy`."""
    psn_portfolio = psn_portfolio_external_id(label_meta)

    # --- Slice the detailed-positions section ---
    in_detail = False
    section: list[str] = []
    for line in full_text.splitlines():
        if "Detailed positions" in line and not in_detail:
            in_detail = True
        elif in_detail and "Additional information" in line and "Abbreviations" in line:
            break
        if in_detail:
            section.append(line)

    results: list[dict] = []

    # --- Cash positions: walk lines, pair amount-line with next-IBAN-line ---
    pending_cash: dict | None = None
    for line in section:
        am = _CASH_AMOUNT_LINE_RE.match(line)
        if am:
            pending_cash = {
                "ccy": am["ccy"],
                "units": to_float(am["units"]),
                "desc": am["desc"].strip(),
                "market_value": to_float(am["market_value"]),
                "fx": to_float(am["fx"]) if am["fx"] else None,
            }
            continue
        iban = iban_in(line)
        if iban and pending_cash is not None:
            results.append({
                **_position_row(label_meta["as_of_date"], psn_portfolio,
                                base_ccy, doc_token),
                "account_external_id": iban,
                "instrument_isin": None,
                "currency_iso": pending_cash["ccy"],
                "units": pending_cash["units"],
                "market_value": pending_cash["market_value"],
                "current_fx_rate": pending_cash["fx"],
                "description": pending_cash["desc"],
                "payload": json.dumps({"raw": pending_cash, "iban_line": line.strip()}),
            })
            pending_cash = None

    # --- Securities positions: anchor on the Valor/ISIN line, look
    # back up to 10 lines for the headline, but never past the previous
    # holding's own Valor/ISIN line: a holding whose headline does not
    # match is left out rather than given the headline of the one above
    # it (see `_closest_headline`). ---
    block_start = 0
    for i, line in enumerate(section):
        vi = _VALOR_ISIN_RE.match(line)
        if not vi:
            continue
        isin = vi["isin"]
        look_from = max(block_start, i - 10)
        block_start = i + 1
        found = _closest_headline(section, look_from, i)
        if found is None:
            continue
        headline_at, headline = found
        headline_is_pm = "price" in headline
        sector = None
        if not headline_is_pm:
            for j in range(max(headline_at, 1), i):
                sector = _sector_line(section[j]) or sector
        market_value = to_float(headline["market_value"])
        # Skip Net/Unfunded Commitment tracking rows: they print an
        # 'n.a.' price and a 0 market value. The funded "Outstanding
        # Shares" row carries the real NAV.
        if headline_is_pm and not market_value:
            continue
        row = {
            **_position_row(label_meta["as_of_date"], psn_portfolio,
                            base_ccy, doc_token),
            **_holding_detail(section[headline_at + 1:i], market_value,
                              private_market=headline_is_pm,
                              gain_blank=(not headline_is_pm
                                          and headline["gain_pct"] is None)),
            "instrument_isin": isin,
            "currency_iso": headline["ccy"],
            "units": to_float(headline["units"]),
            "market_value": market_value,
            "description": headline["desc"].strip(),
        }
        if headline_is_pm:
            row["market_price"] = (None if headline["price"] == "n.a."
                                   else to_float(headline["price"]))
            row["payload"] = json.dumps({
                "valor": vi["valor"],
                "isin": isin,
                "kind": "private_market",
                "headline": headline["text"],
            })
        else:
            row["cost_price"] = to_float(headline["cost_price"])
            row["market_price"] = to_float(headline["market_price"])
            row["sector"] = sector
            row["payload"] = json.dumps({
                "valor": vi["valor"],
                "isin": isin,
                "headline": headline["text"],
            })
        results.append(row)

    return results


def _position_row(as_of_date: int, portfolio: str, base_ccy: str | None,
                  doc_token: str) -> dict:
    """A `historical_position_snapshots` row with every column the
    statement may leave unprinted set to None. Each row kind overrides
    the columns it reads, so a column added to the table is added here
    once rather than to every kind."""
    return {
        "as_of_date": as_of_date,
        "portfolio_external_id": portfolio,
        "account_external_id": "",
        "instrument_isin": None,
        "currency_iso": None,
        "units": None,
        "market_value": None,
        "market_value_currency": base_ccy,
        "cost_price": None,
        "market_price": None,
        "accrued_interest": None,
        "current_fx_rate": None,
        "acquisition_fx_rate": None,
        "cost_basis": None,
        "nav_date": None,
        "last_purchase_date": None,
        "description": None,
        "sector": None,
        "source_doc_token": doc_token,
        "payload": "{}",
    }


def _sector_line(line: str) -> str | None:
    """`line` as a sector label, or None when it cannot be one.

    The sector sits on the second line of a listed-security row, usually
    right after the description (e.g. 'Financials', 'Information Tech.').
    A short line whose last word has no digit qualifies. Private-markets
    rows have no sector column.
    """
    stripped = line.strip()
    if stripped and not any(ch.isdigit() for ch in stripped.split()[-1]) \
            and len(stripped) < 50:
        return stripped
    return None


def _closest_headline(section: list[str], start: int,
                      end: int) -> tuple[int, dict] | None:
    """The headline of the holding whose Valor/ISIN line is `section[end]`,
    as (line index, headline), searching `section[start:end]`; None when no
    line there reads as one.

    The closest line either strict pattern reads wins, a listed-security
    row (`_SECURITY_HEADLINE_RE`) before a private-markets one
    (`_PM_HEADLINE_RE`). Only a block neither reads is offered to
    `_fallback_headline`, so the fallback never displaces a headline a
    strict pattern reads. A private-markets headline has a `price` key.
    """
    for j in range(end - 1, start - 1, -1):
        m = (_SECURITY_HEADLINE_RE.match(section[j])
             or _PM_HEADLINE_RE.match(section[j]))
        if m:
            return j, {**m.groupdict(), "text": m.group()}
    for j in range(end - 1, start - 1, -1):
        headline = _fallback_headline(section[j])
        if headline is not None:
            return j, headline
    return None


def _fallback_headline(line: str) -> dict | None:
    """Read a headline whose figures run together (see
    `_HEADLINE_FIGURES_RE`), or None when the line is not one or more
    than one reading of it agrees.

    The result has the keys the walker reads from the two strict
    patterns: a listed reading has `cost_price`, `market_price` and
    `gain_pct` (None when blank), a private-markets one has `price`.
    """
    m = _HEADLINE_FIGURES_RE.match(line)
    if not m:
        return None
    readings = _headline_readings(m["figures"].split())
    if len(readings) != 1:
        return None
    return {"units": m["units"], "desc": m["desc"], "ccy": m["ccy"],
            "text": m.group(), **readings[0]}


def _headline_readings(tokens: list[str]) -> list[dict]:
    """Every way the figure tokens between a headline's currency and its
    % NA read as a whole holding.

    With a market gain printed, the tokens before it are the two prices
    (and an optional one-letter price qualifier) and the tokens after it
    the market value. Without one, they are the two prices then the
    market value, and the two prices must be the same figure; or, for a
    private-markets row, one price then the market value.
    """
    gains = [k for k, t in enumerate(tokens) if t.endswith("%")]
    if len(gains) > 1:
        return []
    if gains:
        at = gains[0]
        prices = tokens[:at]
        if prices and prices[-1].isalpha():
            prices = prices[:-1]
        market_value = _grouped_figure(tokens[at + 1:], whole=True)
        gain = float(tokens[at][:-1])
        if market_value is None:
            return []
        readings = []
        for k in range(1, len(prices)):
            cost, market = (_grouped_figure(prices[:k]),
                            _grouped_figure(prices[k:]))
            if cost is None or market is None or not to_float(cost):
                continue
            implied = (to_float(market) / to_float(cost) - 1) * 100
            if abs(implied - gain) <= _GAIN_TOLERANCE_PCT:
                readings.append({"cost_price": cost, "market_price": market,
                                 "gain_pct": tokens[at],
                                 "market_value": market_value})
        return readings

    if any(t.isalpha() for t in tokens):
        return []
    readings = []
    for k in range(1, len(tokens)):
        price = _grouped_figure(tokens[:k])
        if price is None:
            continue
        market_value = _grouped_figure(tokens[k:], whole=True)
        if market_value is not None:
            readings.append({"price": price, "market_value": market_value})
        if tokens[k:2 * k] == tokens[:k]:
            market_value = _grouped_figure(tokens[2 * k:], whole=True)
            if market_value is not None:
                readings.append({"cost_price": price, "market_price": price,
                                 "gain_pct": None,
                                 "market_value": market_value})
    return readings


def _grouped_figure(tokens: list[str], *, whole: bool = False) -> str | None:
    """`tokens` joined as one printed figure, or None when their digit
    grouping is not one figure's. `whole` refuses a fraction, as a
    market value never prints one."""
    if not tokens:
        return None
    figure = " ".join(tokens)
    m = _GROUPED_FIGURE_RE.fullmatch(figure)
    if not m or (whole and m["fraction"]):
        return None
    return figure


def _holding_detail(lines: list[str], market_value: float | None, *,
                    private_market: bool, gain_blank: bool = False) -> dict:
    """The facts a holding prints below its headline (see
    `_HOLDING_FX_RE`): the average buy and current exchange rates, the
    accrued interest, the cost value, the NAV date of a private-markets
    holding and the last purchase date. `lines` are the printed lines
    between the headline and the Valor/ISIN line. A fact the statement
    does not print stays None.

    The last purchase date is read from the line below line 3, so line 3
    is found first. A listed holding's line 3 is the one ending in its
    cost value and unrealized P/L, or in its cost value alone when
    `gain_blank` says the headline's market gain is blank. A
    private-markets holding prints neither: its line 2 is the wrapped
    fund name and its line 3 carries the market-price (NAV) date alone.
    """
    detail = {"acquisition_fx_rate": None, "current_fx_rate": None,
              "accrued_interest": None, "cost_basis": None,
              "nav_date": None, "last_purchase_date": None}
    after_fx = 0
    for k, ln in enumerate(lines):
        m = _HOLDING_FX_RE.search(ln)
        if m:
            detail["acquisition_fx_rate"] = to_float(m["acquisition"])
            detail["current_fx_rate"] = to_float(m["current"])
            detail["accrued_interest"] = to_float(m["accrued"])
            after_fx = k + 1
            break

    line3 = None
    if private_market:
        m = _TRAILING_DATE_RE.search(lines[1]) if len(lines) > 1 else None
        if m:
            line3 = 1
            detail["nav_date"] = dmy_to_iso(m["date"])
    else:
        for k in range(after_fx, len(lines)):
            m = _HOLDING_COST_RE.search(lines[k])
            if m:
                detail["cost_basis"] = _cost_value(
                    m["cost"], float(m["pl"]), market_value)
                line3 = k
                break
        # With no P/L to anchor line 3, a line is line 3 only when the
        # figure it ends in is the cost value: with the price unchanged,
        # that equals the market value.
        if line3 is None and gain_blank:
            for k in range(after_fx, len(lines)):
                m = _HOLDING_COST_NO_PL_RE.search(lines[k])
                cost = m and _cost_value(m["cost"], 0.0, market_value)
                if cost is not None:
                    detail["cost_basis"] = cost
                    line3 = k
                    break

    if line3 is not None and line3 + 1 < len(lines):
        m = _TRAILING_DATE_RE.search(
            _DISTRIBUTION_DATE_RE.sub("", lines[line3 + 1]))
        if m:
            detail["last_purchase_date"] = dmy_to_unix(m["date"])
    return detail


def _cost_value(run: str, pl_pct: float,
                market_value: float | None) -> float | None:
    """The cost value at the right end of `run`, a run of digit groups.

    The left-hand column of line 3 can end in a figure of its own, such
    as a distribution amount ('Distribution amount: USD 2'), set one
    space before the cost value. The two then read as one grouped number.
    The unrealized P/L printed beside the cost value tells the readings
    apart: it is the market value over the cost value, less one, so the
    longest run of trailing groups that agrees with it is the cost value.
    Both figures are printed rounded, which the tolerance allows for.
    When no reading agrees, or there is no market value to check
    against, the cost value is not read.
    """
    if market_value is None:
        return None
    groups = run.replace("'", " ").split()
    factor = 1 + pl_pct / 100
    for start in range(len(groups)):
        cost = to_float("".join(groups[start:]))
        if cost is None or cost == 0:
            continue
        if abs(cost * factor - market_value) <= 1 + abs(cost) * 1e-4:
            return cost
    return None



def overview_totals(full_text: str, doc_token: str, label: str
                    ) -> list[dict]:
    """One row per portfolio whose overview block prints a 'Precious
    metals & commodities' total: the market value as printed, in the
    currency the document is valued in. Attribution uses the most recent
    'Portfolio NN' header; a repeated overview block is read once.

    UBS issues no Detailed-positions page for some portfolio types, so
    such a portfolio (e.g. precious-metals custody) has no per-instrument
    row in any PDF, and the overview's total is all a statement prints
    of it. The overview prints the total once per portfolio-currency PDF
    (USD / CHF / EUR); every copy is kept, and the gold adapter picks
    the one it turns into the position."""
    label_meta = _statement_of_assets_meta(full_text, label)
    if label_meta is None:
        return []
    return _overview_totals(full_text, doc_token, label_meta,
                            _base_currency(full_text))


def _overview_totals(full_text: str, doc_token: str, label_meta: dict,
                     base_ccy: str | None) -> list[dict]:
    """The overview totals of a Statement of assets whose metadata is
    `label_meta` and whose base currency is `base_ccy`."""
    rows: list[dict] = []
    seen: set[str] = set()
    current_no: str | None = None
    for line in full_text.splitlines():
        hdr = _OVERVIEW_PORTFOLIO_RE.match(line.strip())
        if hdr:
            current_no = hdr["no"]
            continue
        pmm = _OVERVIEW_PRECIOUS_METALS_RE.match(line)
        if not pmm or current_no is None:
            continue
        port16 = psn_portfolio_external_id(label_meta, current_no)
        if port16 in seen:
            continue
        seen.add(port16)
        mv = to_float(pmm["mv"])
        if not mv:
            continue
        rows.append({
            "source_doc_token": doc_token,
            "as_of_date": label_meta["as_of_date"],
            "portfolio_external_id": port16,
            "asset_class": "precious_metals",
            "currency_iso": base_ccy,
            "market_value": mv,
            "payload": json.dumps({
                "kind": "overview_asset_class",
                "asset_class": "precious_metals",
                "portfolio_no": current_no,
                "market_value": pmm["mv"],
            }),
        })
    return rows
