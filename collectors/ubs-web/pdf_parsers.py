"""PDF parsers for UBS Switzerland statement archive.

The parsers are built on pdfplumber's text and word extraction:

  parse_statement_of_assets(pdf_path, doc_token, label)
      → (position snapshots, transaction-list bookings) from one
        Statement-of-assets PDF.

  parse_account_statement_combined(pdf_path, doc_token)
      → (cash-balance rows, movement rows) from one Account-Statement
        PDF — opened once and laid out for both passes.

  parse_maturity_notice / parse_payment_advice
      → a mortgage's outstanding principal; a payment's movement row.

  parse_capital_call / parse_contract_note
      → the price a holding was bought at, for the `advices` table.

Each returns plain dicts ready for the loader to insert into silver.

Parsing strategy: PDF tables in UBS statements are not real PDF
tables (no row / column structure for pdfplumber to detect — see
the empty `extract_tables()` output during probing). They are
visually-aligned text columns. Two tables tell their columns apart
only by position, and are read from word positions instead: the
Account-Statement movement ledger and a Statement of assets'
transaction list (see their sections). Everything else is read from
the page text, line by line, anchoring on identifiable markers:

  - "Statement of assets as of <DDMMYYYY>" in the label → as_of_date,
    or the document's own "As of <D Month YYYY>" header where there is
    no listing row to carry a label
  - "Portfolio number BBB-AAAAAAAA-NN" in body → portfolio number
  - "Valued in <CCY>" header → portfolio base currency
  - "Valor <num> - ISIN <code>" line → securities position anchor
  - IBAN-shaped line → cash position anchor
  - "Account Statement / DD.MM.YYYY - DD.MM.YYYY" → period range
  - "Opening balance / Closing balance" lines → cash deltas

Performance note: pypdfium2 extracts text about 10× faster than
pdfplumber, and schwab-web reads its statements with it. ubs-web stays
on pdfplumber because the visual-line reconstruction PDFium needs
(count_rects() returns either cell-level granularity that splits a row
across many lines, or column-shared-baseline rects that merge columns
that pdfplumber keeps apart) cannot reproduce the line layout these
parsers read. The Account-Statement balance summary's squish-and-match
reading does round-trip byte-identical under pypdfium2, so that one
parser could move on its own.
"""
from __future__ import annotations

import json
import re
from datetime import date, datetime, timezone
from pathlib import Path

import pdfplumber


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
        "as_of_date": _to_unix(as_of),
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

# A date as the documents spell it out: "7 March 2024".
_LONG_DATE = r"(?P<day>\d{1,2})\s+(?P<month>[A-Z][a-z]+)\s+(?P<year>\d{4})"
_BODY_STMT_OF_ASSETS_ASOF_RE = re.compile(rf"^\s*As of\s+{_LONG_DATE}\s*$", re.M)

# The documents are issued in English throughout the archive. The month
# names are spelled out here rather than handed to strptime("%d %B %Y"),
# whose names come from the process locale: the loader runs in a
# container whose locale is whatever the base image sets, and a date that
# parses on one machine and not another would silently drop rows. Keyed
# in lower case, as UBS varies the casing within one archive.
_MONTHS = {
    name: n for n, name in enumerate(
        ("january", "february", "march", "april", "may", "june", "july",
         "august", "september", "october", "november", "december"), start=1)
}


def _long_date(m: re.Match) -> date | None:
    """The date a match's `day`, `month` and `year` groups spell out; None
    for a month name this table does not know."""
    month = _MONTHS.get(m["month"].lower())
    return date(int(m["year"]), month, int(m["day"])) if month else None


def statement_of_assets_body_meta(full_text: str) -> dict | None:
    """The same metadata `parse_label_statement_of_assets` reads, taken
    from the document's own first page instead of its listing row.

    Returns None unless all three anchors are present, so a PDF that is
    not a Statement of assets — or one whose text extraction failed —
    is declined rather than half-identified. Measured against the whole
    archive, the two roads agree on every document that has both; the
    header's `As of` line is the one the label states, not the
    `valued as of` note some year-end statements print a day or two
    earlier.
    """
    if not _BODY_STMT_OF_ASSETS_TITLE_RE.search(full_text):
        return None
    as_of_m = _BODY_STMT_OF_ASSETS_ASOF_RE.search(full_text)
    portfolio_m = _BODY_STMT_OF_ASSETS_PORTFOLIO_RE.search(full_text)
    if not as_of_m or not portfolio_m:
        return None
    as_of = _long_date(as_of_m)
    if as_of is None:
        return None
    return {
        "as_of_date": _to_unix(as_of),
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


def _to_unix(d: date) -> int:
    return int(datetime(d.year, d.month, d.day, tzinfo=timezone.utc).timestamp())


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
_IBAN_LINE_RE = re.compile(
    r"\b(?P<iban>CH\d{2}(?:\s?[A-Z0-9]{4}){4}\s?[A-Z]{1})\b"
)

# An ISIN: country code, nine alphanumerics, check digit.
_ISIN_SHAPE = r"[A-Z]{2}[A-Z0-9]{9}\d"

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
# "12 345 12 345 75.00" reads as 12 345 12 345. We anchor the Market
# value by requiring the identical Total column to follow it (the
# (?P=mv) backreference) and the %NA after that, so only the first
# column is captured (synthetic example):
#   "Precious metals & commodities 12 345 12 345 75.00 ..."
_OVERVIEW_PORTFOLIO_RE = re.compile(r"^Portfolio\s+(?P<no>\d{2})\b")
_OVERVIEW_PRECIOUS_METALS_RE = re.compile(
    r"^Precious metals & commodities\s+"
    r"(?P<mv>\d{1,3}(?:[ ']\d{3})*)\s+(?P=mv)\s+-?\d+\.\d{2}\b"
)


def _page_text(page) -> str:
    """One page's text, laid out as every text-walking parser here reads
    it."""
    return page.extract_text(x_tolerance=2) or ""


def _pdf_text(pdf_path: Path, max_pages: int | None = None) -> str:
    """The text of a PDF's first `max_pages` pages, or of all of them,
    one page after another."""
    with pdfplumber.open(pdf_path) as pdf:
        return "\n".join(_page_text(p) for p in pdf.pages[:max_pages])


def statement_of_assets_text(pdf_path: Path) -> str:
    """The text a Statement-of-assets walk reads, laid out as the walker
    expects it. Separate from the walk so a caller that only needs to know
    WHICH document this is can ask without parsing its positions."""
    return _pdf_text(pdf_path)


def parse_statement_of_assets(pdf_path: Path, doc_token: str, label: str
                              ) -> tuple[list[dict], list[dict]]:
    """Open a Statement-of-assets PDF once and read both of its parts: the
    positions (one row per holding or cash line, for
    `historical_position_snapshots`) and the transaction list (one row per
    listed booking, for `statement_trades`). Returns (positions, trades)."""
    with pdfplumber.open(pdf_path) as pdf:
        return parse_statement_of_assets_pages(pdf, doc_token, label)


def parse_statement_of_assets_pages(pdf, doc_token: str, label: str
                                    ) -> tuple[list[dict], list[dict]]:
    """`parse_statement_of_assets` on an open pdfplumber document, so tests
    can feed a synthetic one. The positions are read from the page text,
    the transaction list from the word positions of its own pages."""
    texts = [_page_text(p) for p in pdf.pages]
    full_text = "\n".join(texts)
    positions = parse_statement_of_assets_text(full_text, doc_token, label)
    list_pages = [(text, page.extract_words(x_tolerance=2))
                  for page, text in zip(pdf.pages, texts, strict=True)
                  if _TL_HEADER_TEXT in text]
    trades = parse_transaction_list(list_pages, full_text, doc_token, label)
    return positions, trades


def parse_statement_of_assets_text(full_text: str, doc_token: str,
                                   label: str) -> list[dict]:
    """The positions half of `parse_statement_of_assets`, on text already
    extracted, so the regex / line-walk layer can be exercised without a
    real PDF on disk."""
    label_meta = _statement_of_assets_meta(full_text, label)
    if label_meta is None:
        return []
    psn_portfolio = psn_portfolio_external_id(label_meta)
    base_ccy = _base_currency(full_text)

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
                "units": _to_float(am["units"]),
                "desc": am["desc"].strip(),
                "market_value": _to_float(am["market_value"]),
                "fx": _to_float(am["fx"]) if am["fx"] else None,
            }
            continue
        im = _IBAN_LINE_RE.search(line)
        if im and pending_cash is not None:
            iban = im["iban"].replace(" ", "")
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
        market_value = _to_float(headline["market_value"])
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
            "units": _to_float(headline["units"]),
            "market_value": market_value,
            "description": headline["desc"].strip(),
        }
        if headline_is_pm:
            row["market_price"] = (None if headline["price"] == "n.a."
                                   else _to_float(headline["price"]))
            row["payload"] = json.dumps({
                "valor": vi["valor"],
                "isin": isin,
                "kind": "private_market",
                "headline": headline["text"],
            })
        else:
            row["cost_price"] = _to_float(headline["cost_price"])
            row["market_price"] = _to_float(headline["market_price"])
            row["sector"] = sector
            row["payload"] = json.dumps({
                "valor": vi["valor"],
                "isin": isin,
                "headline": headline["text"],
            })
        results.append(row)

    # --- Overview-only asset classes: precious metals / commodities.
    # UBS issues no Detailed-positions page for some portfolio types,
    # so such a portfolio (e.g. precious-metals custody) has no
    # per-instrument row in any PDF — only the relationship overview's
    # asset-class total. We recover that value as a synthetic
    # asset-class-level position. The overview prints the same holding
    # once per portfolio-currency PDF (USD / CHF / EUR); rows are
    # emitted only from USD-valued PDFs (the reporting-currency
    # baseline), so per-currency copies collapse to one row on the
    # silver PK, giving a single deterministic value that gold
    # converts at query time. The synthetic instrument key is
    # deliberately not ISIN-shaped — the gold adapter detects that
    # and leaves the canonical ISIN null. ---
    if base_ccy == "USD":
        results.extend(_overview_precious_metals(
            full_text, label_meta, base_ccy, doc_token))

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
            if cost is None or market is None or not _to_float(cost):
                continue
            implied = (_to_float(market) / _to_float(cost) - 1) * 100
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
            detail["acquisition_fx_rate"] = _to_float(m["acquisition"])
            detail["current_fx_rate"] = _to_float(m["current"])
            detail["accrued_interest"] = _to_float(m["accrued"])
            after_fx = k + 1
            break

    line3 = None
    if private_market:
        m = _TRAILING_DATE_RE.search(lines[1]) if len(lines) > 1 else None
        if m:
            line3 = 1
            detail["nav_date"] = _dmy_to_iso(m["date"])
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
            detail["last_purchase_date"] = _dmy_to_unix(m["date"])
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
        cost = _to_float("".join(groups[start:]))
        if cost is None or cost == 0:
            continue
        if abs(cost * factor - market_value) <= 1 + abs(cost) * 1e-4:
            return cost
    return None


def _dmy_to_date(dmy: str) -> date | None:
    """DD.MM.YYYY → date; None when not a date."""
    try:
        d, mo, y = (int(x) for x in dmy.split("."))
        return date(y, mo, d)
    except ValueError:
        return None


def _dmy_to_unix(dmy: str) -> int | None:
    """DD.MM.YYYY → Unix seconds UTC midnight; None when not a date."""
    d = _dmy_to_date(dmy)
    return _to_unix(d) if d else None


def _dmy_to_iso(dmy: str) -> str | None:
    """DD.MM.YYYY → 'YYYY-MM-DD'; None when not a date."""
    d = _dmy_to_date(dmy)
    return d.isoformat() if d else None


def _overview_precious_metals(full_text: str, label_meta: dict,
                              base_ccy: str, doc_token: str) -> list[dict]:
    """Emit one synthetic precious-metals position per portfolio whose
    overview block carries a 'Precious metals & commodities' total.
    Attribution uses the most recent 'Portfolio NN' header. Deduped
    within the PDF by portfolio so a repeated overview block doesn't
    double-emit; cross-PDF dedup is handled by the silver PK."""
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
        mv = _to_float(pmm["mv"])
        if not mv:
            continue
        # Synthetic, intentionally non-ISIN-shaped instrument key
        # (len != 12) so the gold adapter routes it as a synthetic
        # instrument with a null canonical ISIN.
        synth_key = f"PM-{port16}"
        rows.append({
            **_position_row(label_meta["as_of_date"], port16, base_ccy,
                            doc_token),
            "instrument_isin": synth_key,
            "currency_iso": base_ccy,
            "market_value": mv,
            "description": "Precious metals & commodities",
            "payload": json.dumps({
                "kind": "overview_asset_class",
                "asset_class": "precious_metals",
                "portfolio_no": current_no,
                "market_value": pmm["mv"],
            }),
        })
    return rows


# ============================================================
# Statement of assets: the transaction list
# ============================================================
#
# A Statement of assets may close with a "Transaction list": every
# securities booking in the statement's period, each as a block of
# printed rows. The column header stacks several labels per column, one
# per row of a block:
#
#   A            B (booking text | Number/Amount)  C              D                   E                   F                G
#   Trade date   Booking text  Number/Amount       Description    Cost/Purchase price Transaction price   Transaction gain Transaction value
#   Trade time                 Tax                 Custody account Exchange rate      Exchange rate       Exchange gain    Accrued interest
#   Value date                 Various             Account        Cost value                              Realized P/L     Settlement amount
#                              Brokerage                          Place of execution                                       in account currency
#                              Stock exchange
#                              Third-party executions
#                              Foreign Financial Transaction Tax
#
# So a figure means what the label in the same column and the same row
# of the header says. The plain text cannot tell those apart: a block's
# rows hold only the figures that apply, and an empty cell leaves no
# trace in it. The parser therefore works on word positions. Columns B
# and D to G are right-aligned to the right edge of their row-1 label,
# A and C are left-aligned, and the booking text is left-aligned at its
# label inside column B. A block starts at a row that carries a date in
# column A and a booking text, and its row number is its distance from
# that row in units of the header's row pitch. A booking text that wraps
# past two rows pushes the rest of its block down by as many rows: every
# cell below the first row moves, except the trade time, which stays
# under the trade date.
#
# Column C carries the description, then the settlement and order
# numbers and the Valor/ISIN line, and below them the custody account
# and the cash account. Their rows vary with the description's length,
# so they are told apart by their shape rather than by row.
#
# Every figure is stored as printed, signs included. What each column
# means, and which currency it is in, is in migration 0014's header and
# DESIGN.md §3.10.

_TL_HEADER_TEXT = "Trade date Booking text"
# The list's period opens a line of the page header, which may share it
# with the next header line.
_TL_PERIOD_RE = re.compile(
    r"^\s*From\s+(?P<start>\d{2}\.\d{2}\.\d{4})\s+to\s+"
    r"(?P<end>\d{2}\.\d{2}\.\d{4})\b", re.M)
_TL_DATE_RE = re.compile(r"^\d{2}\.\d{2}\.\d{4}$")
_TL_TIME_RE = re.compile(r"^\d{2}:\d{2}:\d{2}$")
# A list's closing totals, and the page footer below them.
_TL_END_RE = re.compile(r"^(?:Subtotal|Total)\b|^[A-Z0-9]{8}/\d{6}/")
# A figure, optionally led by its currency and followed by a one-letter
# price qualifier.
_TL_FIGURE_RE = re.compile(
    r"^(?:(?P<ccy>[A-Z]{3})\b\s*)?(?P<v>-?\d[\d ']*(?:\.\d+)?)?"
    r"(?:\s+[A-Za-z])?$")
_TL_PCT_RE = re.compile(r"^(?P<v>-?\d+(?:\.\d+)?)%$")
_TL_VALOR_ISIN_RE = re.compile(
    rf"\bValor\s+(?P<valor>\S+)\s+-\s+ISIN\s+(?P<isin>{_ISIN_SHAPE})\b")
_TL_SETTLEMENT_NO_RE = re.compile(r"\bSettlement no\.:\s*(?P<no>\S+)")
_TL_ORDER_NO_RE = re.compile(r"\bOrder no\.:\s*(?P<no>\S+)")
_TL_CUSTODY_ACCOUNT_RE = re.compile(r"^\d{3,4}-\d+\.[A-Z0-9]+$")
_TL_CASH_ACCOUNT_RE = re.compile(r"^CH\d{2}[A-Z0-9]{17}$")
# The rows of column B below the quantity, by the header's labels.
_TL_CHARGES = (
    (1, "taxes"),                       # Tax
    (2, "fees"),                        # Various
    (3, "commission"),                  # Brokerage
    (4, "stock_exchange_fees"),         # Stock exchange
    (5, "third_party_fees"),            # Third-party executions
    (6, "financial_transaction_tax"),   # Foreign Financial Transaction Tax
)
# How far a word's edge may sit from its column's edge.
_TL_SLACK = 3.0


def _tl_label_at(words: list[dict], first: str, second: str,
                 start: int = 0) -> int | None:
    """Index of the two-word label `first second` in a header row."""
    for k in range(start, len(words) - 1):
        if words[k]["text"] == first and words[k + 1]["text"] == second:
            return k
    return None


def _tl_anchors(rows: list[dict]) -> dict | None:
    """The column edges, read from the header's first row, and the row
    pitch, read from the distance to its second. None when the page
    carries no transaction-list header."""
    at = next((i for i, row in enumerate(rows)
               if [w["text"] for w in row["words"][:4]]
               == ["Trade", "date", "Booking", "text"]), None)
    if at is None:
        return None
    ws = rows[at]["words"]
    number = next((w for w in ws if w["text"] == "Number/Amount"), None)
    desc = next((w for w in ws if w["text"] == "Description"), None)
    d = _tl_label_at(ws, "Cost/Purchase", "price")
    e = _tl_label_at(ws, "Transaction", "price", d or 0)
    f = _tl_label_at(ws, "Transaction", "gain", e or 0)
    g = _tl_label_at(ws, "Transaction", "value", f or 0)
    if None in (number, desc, d, e, f, g):
        return None
    pitch = rows[at + 1]["top"] - rows[at]["top"] if at + 1 < len(rows) else 0
    return {
        "top": rows[at]["top"],
        "pitch": pitch if pitch > 0 else 10.0,
        "booking_x0": ws[2]["x0"],
        "number_x1": number["x1"],
        "desc_x0": desc["x0"],
        "d_x0": ws[d]["x0"],
        "right": {"D": ws[d + 1]["x1"], "E": ws[e + 1]["x1"],
                  "F": ws[f + 1]["x1"], "G": ws[g + 1]["x1"]},
    }


def _tl_cells(row_words: list[dict], anchors: dict) -> dict[str, list[dict]]:
    """Sort one printed row's words into columns: 'A' to 'G', and 'text'
    for the booking text inside column B."""
    cells: dict[str, list[dict]] = {}
    booking_run = False
    prev_x1 = None
    for w in sorted(row_words, key=lambda w: w["x0"]):
        if w["x0"] < anchors["booking_x0"] - _TL_SLACK:
            col = "A"
        elif w["x1"] <= anchors["number_x1"] + _TL_SLACK:
            # The booking text is a run of words starting at its label's
            # left edge; anything else this far left is a right-aligned
            # figure of column B.
            if abs(w["x0"] - anchors["booking_x0"]) <= _TL_SLACK:
                booking_run = True
            elif booking_run and prev_x1 is not None and w["x0"] - prev_x1 > 6:
                booking_run = False
            col = "text" if booking_run else "B"
        elif (w["x0"] >= anchors["desc_x0"] - _TL_SLACK
              and w["x1"] < anchors["d_x0"]):
            col = "C"
        else:
            col = next((c for c, edge in anchors["right"].items()
                        if w["x1"] <= edge + _TL_SLACK), "G")
        prev_x1 = w["x1"]
        cells.setdefault(col, []).append(w)
    return cells


def _tl_blocks(rows: list[dict], anchors: dict
               ) -> list[dict[tuple[str, int], str]]:
    """Group the rows below the header into bookings. Each booking maps
    (column, row number) to the text printed there."""
    blocks: list[dict[tuple[str, int], str]] = []
    current: dict[tuple[str, int], str] | None = None
    start_top = 0.0
    for row in rows:
        if row["top"] <= anchors["top"] + 7.5 * anchors["pitch"]:
            continue                                 # the header itself
        cells = _tl_cells(row["words"], anchors)
        first_column = [w["text"] for w in cells.get("A", [])]
        if _TL_END_RE.match(" ".join(first_column)):
            current = None
            continue
        if (first_column and _TL_DATE_RE.match(first_column[0])
                and "text" in cells):
            current = {}
            blocks.append(current)
            start_top = row["top"]
        if current is None:
            continue
        k = round((row["top"] - start_top) / anchors["pitch"])
        for col, ws in cells.items():
            text = " ".join(w["text"] for w in ws)
            key = (col, k)
            current[key] = f"{current[key]} {text}" if key in current else text
    return blocks


def _tl_figure(text: str | None) -> tuple[str | None, float | None]:
    """(currency, value) of a printed figure such as 'HKD -1 234.56'."""
    m = _TL_FIGURE_RE.match(text or "")
    if not m:
        return None, None
    return m["ccy"], _to_float(m["v"])


def _tl_pct(text: str | None) -> float | None:
    m = _TL_PCT_RE.match(text or "")
    return float(m["v"]) if m else None


def _tl_trade(block: dict[tuple[str, int], str]) -> dict:
    """The columns of one booking, from its cells. A cell is asked for
    by its header row; a booking text longer than two rows shifts it."""
    extra = max(0, max(k for col, k in block if col == "text") - 1)

    def cell(key: tuple[str, int]) -> str | None:
        col, k = key
        if k > (1 if col == "A" else 0):
            k += extra
        return block.get((col, k))

    ccy, cost_price = _tl_figure(cell(("D", 0)))
    _, quantity = _tl_figure(cell(("B", 0)))
    _, price = _tl_figure(cell(("E", 0)))
    _, value = _tl_figure(cell(("G", 0)))
    _, accrued = _tl_figure(cell(("G", 1)))
    settlement_ccy, settlement = _tl_figure(cell(("G", 3)) or cell(("G", 2)))
    trade = {
        "trade_date": _dmy_to_unix(cell(("A", 0)) or ""),
        "trade_time": cell(("A", 1)),
        "value_date": _dmy_to_unix(cell(("A", 2)) or ""),
        "booking_text": " ".join(
            block[key] for key in sorted(k for k in block if k[0] == "text")),
        "quantity": quantity,
        "currency_iso": ccy,
        "cost_price": cost_price,
        "acquisition_fx_rate": _to_float(cell(("D", 1))),
        "cost_basis": _to_float(cell(("D", 2))),
        "place_of_execution": cell(("D", 3)),
        "transaction_price": price,
        "transaction_fx_rate": _to_float(cell(("E", 1))),
        "transaction_gain_pct": _tl_pct(cell(("F", 0))),
        "exchange_gain_pct": _tl_pct(cell(("F", 1))),
        "realized_pl_pct": _tl_pct(cell(("F", 2))),
        "transaction_value": value,
        "accrued_interest": accrued,
        "settlement_amount": settlement,
        "settlement_currency_iso": settlement_ccy,
        "charges_currency_iso": None,
        "valor": None, "isin": None, "settlement_no": None, "order_no": None,
        "custody_account": None, "account_iban": None,
    }
    for k, col in _TL_CHARGES:
        charge_ccy, trade[col] = _tl_figure(cell(("B", k)))
        trade["charges_currency_iso"] = (trade["charges_currency_iso"]
                                         or charge_ccy)

    # Column C, top to bottom: the description up to the first of the
    # settlement number, the order number and the Valor/ISIN line; then
    # the accounts, told apart by their shape.
    name: list[str] = []
    reached_ids = False
    for key in sorted((k for k in block if k[0] == "C"), key=lambda k: k[1]):
        line = block[key]
        m = _TL_VALOR_ISIN_RE.search(line)
        if m:
            trade["valor"], trade["isin"] = m["valor"], m["isin"]
        m = _TL_SETTLEMENT_NO_RE.search(line)
        if m:
            trade["settlement_no"] = m["no"]
        m = _TL_ORDER_NO_RE.search(line)
        if m:
            trade["order_no"] = m["no"]
        if trade["valor"] or trade["settlement_no"] or trade["order_no"]:
            reached_ids = True
        if _TL_CUSTODY_ACCOUNT_RE.match(line):
            trade["custody_account"] = line
        elif _TL_CASH_ACCOUNT_RE.match(line.replace(" ", "")):
            trade["account_iban"] = line.replace(" ", "")
        elif not reached_ids:
            name.append(line)
    trade["security_name"] = " ".join(name) or None
    return trade


def parse_transaction_list(list_pages: list[tuple[str, list[dict]]],
                           full_text: str, doc_token: str,
                           label: str) -> list[dict]:
    """One `statement_trades` row per booking a Statement of assets lists.

    `list_pages` are the (text, words) of the pages that carry the
    transaction-list header; `full_text` is the whole document's text, for
    the metadata every row shares. The row's `seq` is its place in the
    list, which keys it within its document.
    """
    meta = _statement_of_assets_meta(full_text, label)
    if meta is None or not list_pages:
        return []
    shared = {
        "source_doc_token": doc_token,
        "as_of_date": meta["as_of_date"],
        "portfolio_external_id": psn_portfolio_external_id(meta),
        "reporting_currency_iso": _base_currency(full_text),
        "period_start": None,
        "period_end": None,
    }
    m = _TL_PERIOD_RE.search(list_pages[0][0])
    if m:
        shared["period_start"] = _dmy_to_unix(m["start"])
        shared["period_end"] = _dmy_to_unix(m["end"])

    trades: list[dict] = []
    for _text, words in list_pages:
        rows = _stmt_cluster_rows(words)
        anchors = _tl_anchors(rows)
        if anchors is None:
            continue
        for block in _tl_blocks(rows, anchors):
            trades.append({
                **shared,
                **_tl_trade(block),
                "seq": len(trades) + 1,
                "payload": json.dumps(
                    {f"{col}{k}": text for (col, k), text in sorted(
                        block.items(), key=lambda kv: (kv[0][1], kv[0][0]))},
                    ensure_ascii=False),
            })
    return trades


# ============================================================
# Account Statement parser
# ============================================================

_IBAN_HEADER_RE = re.compile(
    r"IBAN\s*(?P<iban>CH\d{2}[A-Z0-9]{17})\b"
)
_PERIOD_RE = re.compile(
    r"(?P<from_day>\d{2})\.(?P<from_month>\d{2})\.(?P<from_year>\d{4})\s*-\s*"
    r"(?P<to_day>\d{2})\.(?P<to_month>\d{2})\.(?P<to_year>\d{4})"
)
# UBS Account-Statement headers come in a few variants, all
# rendered with no internal whitespace, e.g. 'UBSSavingsAccountCHF'.
# The common shape is "<account-type-words><CCY>" with the CCY
# being the trailing three uppercase letters. We anchor on the
# substring 'Account' and capture the trailing ISO code.
_CCY_HEADER_RE = re.compile(
    r"[A-Za-z]*[Aa]ccount[A-Za-z]*(?P<ccy>[A-Z]{3})\b"
)
# The four balance/total regexes run against the squished `flat`
# text (whitespace already removed). UBS uses thin spaces as
# thousand separators in the rendered PDF (e.g. "1 234.56"), so
# after the `replace(" ", "")` pass the value has neither spaces
# nor apostrophes between digits. We keep `'` in the value class
# defensively in case some historical statements use the Swiss
# apostrophe convention and pdfplumber preserves it. The decimal
# part is optional: zero-balance / closed-account statements
# render the value as a bare `0`, not `0.00`.
_VAL = r"-?[\d']+(?:\.\d{2})?"
_OPENING_BAL_RE = re.compile(rf"Openingbalance(?P<v>{_VAL})")
_CLOSING_BAL_RE = re.compile(rf"Closingbalance(?P<v>{_VAL})")
_TOTAL_CREDITS_RE = re.compile(rf"Totalcredits(?P<v>{_VAL})")
_TOTAL_DEBITS_RE = re.compile(rf"Totaldebits(?P<v>{_VAL})")


def parse_account_statement_text(text: str, doc_token: str) -> list[dict]:
    """Account-Statement balance-summary parser: from the first-two-page
    text emit ONE row with opening + closing balance + period bounds for
    the covered cash account. Takes already-extracted PDF text so the
    same text can serve both the summary and movement passes (and so
    tests can exercise the regex layer without a real PDF on disk)."""
    # UBS Account-Statement PDFs render text with all the
    # whitespace squished out within tokens (e.g. `IBANCHKK...`,
    # `Openingbalance1234.56`). pdfplumber preserves that. We
    # match all the headers against the squished text.
    flat = text.replace(" ", "").replace(" ", "")  #   = NBSP
    iban = None
    m = _IBAN_HEADER_RE.search(flat)
    if m:
        iban = m["iban"].upper()
    if not iban or len(iban) != 21:
        return []

    period = _PERIOD_RE.search(flat)
    if not period:
        return []
    period_start = date(int(period["from_year"]), int(period["from_month"]),
                        int(period["from_day"]))
    period_end = date(int(period["to_year"]), int(period["to_month"]),
                      int(period["to_day"]))

    ccy_m = _CCY_HEADER_RE.search(flat)
    currency = ccy_m["ccy"] if ccy_m else None
    opening = _OPENING_BAL_RE.search(flat)
    closing = _CLOSING_BAL_RE.search(flat)
    credits = _TOTAL_CREDITS_RE.search(flat)
    debits = _TOTAL_DEBITS_RE.search(flat)

    return [{
        "period_end": _to_unix(period_end),
        "account_external_id": iban,
        "currency_iso": currency,
        "period_start": _to_unix(period_start),
        "opening_balance": _to_float(opening["v"]) if opening else None,
        "closing_balance": _to_float(closing["v"]) if closing else None,
        "total_debits": _to_float(debits["v"]) if debits else None,
        "total_credits": _to_float(credits["v"]) if credits else None,
        "source_doc_token": doc_token,
        "payload": json.dumps({
            "currency_header_match": bool(ccy_m),
            "period_start": period_start.isoformat(),
            "period_end": period_end.isoformat(),
        }),
    }]


# ============================================================
# Maturity Notice parser (mortgage interest-roll PDFs)
# ============================================================
#
# UBS issues one "Maturity notice" PDF per mortgage per fixed-rate
# / SARON interest-roll period (typically quarterly). The PDF lays
# the salient fields out as labelled lines:
#
#     UBS SARON Mortgage CHF
#     Account no. <branch>-<base>.<MMM> <suffix>
#     Category <collateral address>
#     ...
#     As at DD.MM.YYYY
#     ...
#     Current debt capital N NNN NNN.NN
#
# We extract the outstanding principal as the historical balance at
# the "As at" date and surface the product / currency for the gold
# adapter to roll up into a mortgage Position.
#
# The "Account no." in the maturity notice is the compact zero-
# stripped form (`BBB-AAAAAA.MMM NNNN`); the live positions.csv
# table normalises the same account to the padded 4-digit branch /
# 8-digit base form (`BBBB AAAAAAAA.MMM NNNN`).
# We re-pad on parse so the resulting account_external_id matches
# `mortgages.account_external_id` exactly — gold's instruments/
# accounts upsert is keyed by ID, so cross-snapshot identity has
# to be byte-equal.

# Product line of the form "UBS SARON Mortgage CHF" or
# "UBS Fixed-Rate Mortgage CHF". `name` swallows everything between
# 'UBS ' and the trailing ' CCY'; the trailing 3-letter token is
# the currency.
_PRODUCT_LINE_RE = re.compile(
    r"^UBS\s+(?P<name>.+?)\s+Mortgage\s+(?P<ccy>[A-Z]{3})\s*$"
)

# Account no. line. Branch may be 3 or 4 digits, base 6-8 digits.
# `mmm` is the mortgage-type code; `suffix` is the trailing sub-
# account number.
_ACCT_NO_LINE_RE = re.compile(
    r"^Account\s+no\.\s+"
    r"(?P<branch>\d{3,4})-(?P<base>\d{6,8})"
    r"\.(?P<mmm>[A-Z0-9]{2,4})\s+(?P<suffix>\d+)\s*$"
)

# Collateral line. UBS prefixes the property address with the
# literal label `Category` in the side-column of the PDF.
_COLLATERAL_LINE_RE = re.compile(r"^Category\s+(?P<addr>.+\S)\s*$")

# Balance date. The maturity notice's primary date marker is
# `As at DD.MM.YYYY`.
_AS_AT_LINE_RE = re.compile(
    r"^As\s+at\s+(?P<d>\d{2})\.(?P<m>\d{2})\.(?P<y>\d{4})\s*$"
)

# Outstanding principal. A loose pattern because pdfplumber
# occasionally collapses multiple spaces between label and value.
_CURRENT_DEBT_RE = re.compile(
    r"^Current\s+debt\s+capital\s+(?P<v>[\d\s']+\.\d{2})\s*$"
)


def parse_maturity_notice(pdf_path: Path, doc_token: str) -> list[dict]:
    """Walk a 'Maturity notice' PDF and emit ONE row capturing the
    mortgage's outstanding principal at the notice's `As at` date."""
    return parse_maturity_notice_text(_pdf_text(pdf_path, 2), doc_token)


def parse_maturity_notice_text(text: str, doc_token: str) -> list[dict]:
    """Pure-text variant of parse_maturity_notice for fixture-based
    tests. Returns [] when the PDF doesn't actually look like a
    mortgage maturity notice (so non-mortgage 'Maturity notice'
    doc_types — bonds, time deposits, etc. — are skipped gracefully).
    """
    product_name = None
    currency = None
    account_external_id = None
    collateral = None
    as_of = None
    outstanding = None

    for raw_line in text.splitlines():
        ln = _undouble_bold(raw_line.strip())
        if not ln:
            continue
        if product_name is None:
            m = _PRODUCT_LINE_RE.match(ln)
            if m:
                product_name = m["name"].strip()
                currency = m["ccy"]
                continue
        if account_external_id is None:
            m = _ACCT_NO_LINE_RE.match(ln)
            if m:
                branch = m["branch"].zfill(4)
                base = m["base"].zfill(8)
                account_external_id = (
                    f"{branch} {base}.{m['mmm']} {m['suffix']}"
                )
                continue
        if collateral is None:
            m = _COLLATERAL_LINE_RE.match(ln)
            if m:
                collateral = m["addr"]
                continue
        if as_of is None:
            m = _AS_AT_LINE_RE.match(ln)
            if m:
                as_of = date(int(m["y"]), int(m["m"]), int(m["d"]))
                continue
        if outstanding is None:
            m = _CURRENT_DEBT_RE.match(ln)
            if m:
                outstanding = _to_float(m["v"])
                continue

    if (product_name is None or account_external_id is None or
            as_of is None or outstanding is None or currency is None):
        return []

    rate_type = _mortgage_rate_type_from_product(product_name)
    return [{
        "as_of_date": _to_unix(as_of),
        "account_external_id": account_external_id,
        "currency_iso": currency,
        # Liability sign: source PDF prints the principal as a
        # positive amount; gold expects negative market_value.
        "outstanding_balance": -abs(outstanding),
        "product_name": f"UBS {product_name} Mortgage",
        "rate_type": rate_type,
        "collateral_description": collateral,
        "source_doc_token": doc_token,
        "payload": json.dumps({
            "as_of": as_of.isoformat(),
            "product_name": product_name,
            "rate_type": rate_type,
        }),
    }]


def _undouble_bold(line: str) -> str:
    """pdfplumber renders some bold headers as every-character-
    doubled ('MMaattuurriittyy nnoottiiccee',
    'AAss aatt 3311..0033..22002233'). Collapse the line — but only
    when the WHOLE line passes the doubled test. A line that mixes
    doubled and non-doubled tokens isn't bold-rendered, and a
    legitimate all-same-digit field ('0000') would otherwise be
    silently shortened to '00'."""
    if not line:
        return line
    flat = line.replace(" ", "")
    if len(flat) < 2 or len(flat) % 2 != 0:
        return line
    for i in range(0, len(flat), 2):
        if flat[i] != flat[i + 1]:
            return line
    parts = []
    for token in line.split(" "):
        if not token:
            parts.append(token)
            continue
        if len(token) % 2 != 0:
            return line
        parts.append("".join(token[i] for i in range(0, len(token), 2)))
    return " ".join(parts)


def _mortgage_rate_type_from_product(product_name: str) -> str | None:
    """Map a UBS mortgage product-line caption to a canonical
    rate_type tag. Mirrors the live mortgage classifier in load.py
    but takes the post-'UBS ' middle slice so both kinds of caption
    (positions.csv 'Description 1' and maturity-notice product line)
    converge."""
    low = product_name.lower()
    if "saron" in low:
        # SARON-indexed mortgages are UBS's current variable-rate
        # product; classify with the 'variable' tag rather than
        # 'saron' so the canonical taxonomy stays rate-basis (fixed
        # vs. variable), not product-name (SARON vs. older flavours).
        return "variable"
    if "fixed-rate" in low or "fixed rate" in low or "festhypothek" in low:
        return "fixed"
    if "variable" in low or "variabel" in low or "variabler" in low:
        return "variable"
    return None


# ============================================================
# Helpers
# ============================================================

def _to_float(s: str | None) -> float | None:
    """Parse a UBS-formatted number ('1 234.56', '1'234.56', '-1234')."""
    if s is None:
        return None
    s = s.replace(" ", "").replace("'", "").replace(",", "")
    if not s:
        return None
    try:
        return float(s)
    except ValueError:
        return None


# ============================================================
# Account-Statement MOVEMENT parser (per-transaction ledger rows)
# ============================================================
#
# The summary parser above (`parse_account_statement_text`) reads
# only the opening/closing balances. This parser walks the ledger and
# emits every booking (movement) row, so transaction history from
# before the CSV feed's earliest available date can be backfilled
# from the PDF archive.
#
# Layout (see docs + probing): the ledger is a visually-aligned,
# NOT a real PDF table. Columns are:
#
#   Date | Information | Debits | Credits | Value date | Balance
#
# Only ONE of Debits / Credits is populated per row, and — crucially
# — the plain text gives no marker of WHICH column an amount sits in.
# We therefore work at the WORD level (`extract_words`) and bucket
# each numeric token by its x-position against the ledger header's
# per-column anchors. Alphabetic tokens are the booking-type text
# (Information); they never poison the amount columns. This is
# validated against the running Balance column: for every statement
# in the archive `opening + Σ(credit − debit) == balance` chains
# row-by-row to the printed Closing balance.
#
# Conventions handled: space (thin-space / NBSP) thousands
# separators ('199 993.01'); trailing-minus negatives ('16.05-');
# 2-digit years (DD.MM.YY); multi-page ledgers with a repeated
# header per page; the "not included in the closing balance"
# post-closing trailer (future-period bookings shown for
# information — flagged `post_closing`, excluded from the
# opening→closing reconciliation, deduped across statements by the
# loader's content id).

_STMT_ROW_DATE_RE = re.compile(r"^\d{2}\.\d{2}\.\d{2}$")   # DD.MM.YY
_STMT_IBAN_RE = re.compile(r"CH\d{2}(?:[ ]?[A-Z0-9]){17}")
# A counter-account reference in a continuation line: either a full
# CH-IBAN (inter-account e-banking transfer) or a UBS mortgage
# account stamp ("HYPOTHEK <base>.<type> <sub-account>").
_STMT_HYPO_RE = re.compile(r"HYPOTHEK\s+[\d ]+\.[A-Z0-9]+\s+\d+")

# Booking types that MOVE cash (become deposits/withdrawals in gold).
# Only these are eligible to be re-tagged as an internal transfer —
# securities settlements / dividends / fees are already excluded from
# flows, so they must never be touched.
_STMT_FLOW_TYPES = frozenset({
    "CREDIT", "DEBIT",
    "E-BANKING PAYMENT ORDER", "E-BANKING CREDIT",
    "MULTI E-BANKING ORDER",
    "PAYMENT ORDER", "PAYMENT ORDER BY TELEPHONE", "SPECIAL PAYMENT ORDER",
    "PAYNET ORDER", "MULTI PAYNET ORDER",
})

# Name-free markers of an INTRA-portfolio cash move (funding /
# reducing a managed mandate, or an explicit book-transfer). These
# reshuffle cash between the holder's OWN UBS accounts/mandates and
# so are NOT external capital — re-tag them so gold nets them out
# instead of double-counting them as deposits/withdrawals. Genuine
# external credits (an incoming bank transfer, a SIC payment, salary)
# carry none of these tokens and stay a deposit. All tokens are
# generic banking terms — no personal identifiers.
_STMT_INTERNAL_MARKERS = (
    "UEBERTRAG", "UMBUCHUNG",              # DE: transfer / rebooking
    "MANDAT", "MANAGE",                    # managed-mandate operations
    "PORTFOLIO", "REDUK", "REDUCTION", "INCREAS",
)


def _stmt_is_internal_transfer(desc: str, cont_lines: list[str]) -> bool:
    """True when a cash-moving booking is an intra-portfolio reshuffle
    (funding/reducing a managed mandate, or an explicit book transfer)
    rather than external capital."""
    if desc not in _STMT_FLOW_TYPES:
        return False
    blob = " ".join(cont_lines).upper()
    return any(m in blob for m in _STMT_INTERNAL_MARKERS)


def _stmt_amount(tokens: list[str]) -> float | None:
    """Join a ledger column's numeric word-fragments into a float.
    Handles space/apostrophe thousands separators and trailing-minus
    negatives ('16.05-' → -16.05). Returns None for an empty column
    or a non-numeric fragment (e.g. leaked Information text)."""
    if not tokens:
        return None
    s = "".join(tokens)
    for ch in (" ", " ", " ", "'", "’", ","):
        s = s.replace(ch, "")
    neg = s.endswith("-")
    s = s.rstrip("-").rstrip("+")
    if not re.match(r"^\d+(?:\.\d+)?$", s):
        return None
    return -float(s) if neg else float(s)


def _stmt_cluster_rows(words: list[dict], ytol: float = 3.0) -> list[dict]:
    """Group extract_words() output into visual rows by `top`."""
    rows: list[dict] = []
    for w in sorted(words, key=lambda w: (w["top"], w["x0"])):
        for r in rows:
            if abs(r["top"] - w["top"]) <= ytol:
                r["words"].append(w)
                break
        else:
            rows.append({"top": w["top"], "words": [w]})
    for r in rows:
        r["words"].sort(key=lambda w: w["x0"])
    return rows


def _stmt_find_header(rows: list[dict]) -> dict | None:
    """Return the ledger header's per-column anchor x-centres.

    The REAL ledger header carries the full label set; a decoy
    summary box ("Your account at a glance  Debits Credits Balance")
    lacks Date/Information/Value, so require them all."""
    for r in rows:
        texts = [w["text"] for w in r["words"]]
        if all(t in texts for t in
               ("Date", "Information", "Value", "Debits", "Credits",
                "Balance")):
            def centre(label: str) -> float | None:
                for w in r["words"]:  # noqa: B023
                    if w["text"] == label:
                        return (w["x0"] + w["x1"]) / 2
                return None
            return {
                "Debits": centre("Debits"),
                "Credits": centre("Credits"),
                "ValueDate": centre("Value"),
                "Balance": centre("Balance"),
                "top": r["top"],
            }
    return None


def _stmt_assign_numeric(word: dict, anchors: dict) -> str | None:
    """Nearest NUMERIC-column anchor for a digit-leading token."""
    cx = (word["x0"] + word["x1"]) / 2
    best, best_d = None, 1e9
    for col in ("Debits", "Credits", "ValueDate", "Balance"):
        a = anchors.get(col)
        if a is None:
            continue
        d = abs(cx - a)
        if d < best_d:
            best_d, best = d, col
    return best


def _stmt_counter_account(cont_lines: list[str]) -> str | None:
    """Pull a counter-account reference (CH-IBAN or HYPOTHEK stamp)
    from a movement's continuation lines, for the gold engine's
    internal-transfer / mortgage netting. Whitespace-normalised."""
    for c in cont_lines:
        m = _STMT_IBAN_RE.search(c)
        if m:
            return re.sub(r"\s+", "", m.group(0))
    for c in cont_lines:
        m = _STMT_HYPO_RE.search(c)
        if m:
            return re.sub(r"\s+", " ", m.group(0)).strip()
    return None


# An order or settlement reference — a letter, a date and a number,
# "V 01.01.2020 12345678". Skipped before the valor scan rather than
# left to it: the reference opens a trade's continuation, so it is
# reached first, and its number passes the valor test.
_STMT_REFERENCE_RE = re.compile(r"^[A-Z]\s+\d{2}\.\d{2}\.\d{4}\b")
# The valor closing a security line: the last whitespace-separated
# token, all digits, with something bearing a letter in front of it
# ("EXAMPLEETF WORLD 1234567"). A turnover trailer ends in a decimal
# group and falls out on its own.
_STMT_VALOR_RE = re.compile(r"^(?P<caption>.*[A-Za-z].*?)\s+(?P<valor>\d{4,12})$")
# The same, where a long caption left no room for the separator and the
# valor is printed hard against it ("Example Group Rg199999991"). Six
# digits, not four: with no separator a short run is far likelier to be
# the tail of a name than a valor.
_STMT_GLUED_VALOR_RE = re.compile(r"^(?P<caption>.*[A-Za-z].*?)(?P<valor>\d{6,12})$")


def _stmt_security(cont_lines: list[str]) -> tuple[str | None, str | None]:
    """The instrument caption and Swiss VALOR a securities movement names.

    The valor identifies one security line, which is what lets the gold
    engine match the movement to an instrument by identity rather than
    by name. Returns (caption, valor), or (None, None) where no
    continuation line closes with one — every movement that is not a
    trade, and any whose caption itself ends in digits, which mints a
    number that matches no instrument and so resolves to nothing.

    The spaced form is tried across every line before the glued one, so
    the glued form reads only a movement no line of which closes with a
    spaced valor.
    """
    lines = [c.strip() for c in cont_lines if not _STMT_REFERENCE_RE.match(c.strip())]
    for pattern in (_STMT_VALOR_RE, _STMT_GLUED_VALOR_RE):
        for c in lines:
            m = pattern.match(c)
            if m:
                caption = re.sub(r"\s+", " ", m.group("caption")).strip()
                if caption:
                    return caption, m.group("valor")
    return None, None


# A bundled payment order. The statement books a batch of e-banking
# payments as ONE movement carrying the batch total, and prints the
# beneficiaries under it, closed by a "<N> times <rail>" trailer. With
# N > 1 that single row is several unrelated payments — different
# beneficiaries, different purposes — added together, and no consumer
# downstream can take them apart again: the gold description becomes
# every beneficiary concatenated, and one merchant signature stands for
# the lot.
#
# The trailer is what makes the batch legible, and it is printed by
# booking type rather than belonging to one: MULTI E-BANKING ORDER and
# MULTI PAYNET ORDER both bundle, and the same trailer closes an
# ordinary single order with "1 times". Matching the SHAPE rather than
# the booking type therefore splits every bundle the statement can
# print, including types not yet seen, and leaves every single order
# alone by construction.
_STMT_MULTI_TRAILER_RE = re.compile(r"^(\d+)\s+times\s+(\S.*)$", re.I)

# A leg's own amount, at the end of its first line ("EXAMPLE AG 1 234.50").
# The decimals are required: an address line ends in a postcode, and a
# bare integer would open a leg that does not exist. Thousands are
# grouped with a space or an apostrophe, as elsewhere in the ledger.
_STMT_LEG_AMOUNT_RE = re.compile(r"\s(\d{1,3}(?:[ '\u2019]\d{3})*|\d+)\.(\d{2})$")


def _stmt_multi_trailer(cont_lines: list[str]) -> tuple[int, str, int] | None:
    """The batch trailer as (count, rail, line index), or None when the
    movement carries none."""
    for i, line in enumerate(cont_lines):
        m = _STMT_MULTI_TRAILER_RE.match(str(line).strip())
        if m:
            return int(m.group(1)), m.group(2).strip(), i
    return None


def _stmt_split_multi(cont_lines: list[str],
                      total: float | None) -> list[dict] | None:
    """Split a bundled order's continuation into one entry per payment,
    as {amount, lines}. None when the movement is not a bundle, or when
    the split cannot be proved — a shape that does not add up is left
    whole rather than guessed at, because a wrong split moves money
    between beneficiaries.

    A line ending in an amount opens a payment and the amount is taken
    off its text; the lines under it are that beneficiary's name and
    address overflow, which run to as many lines as the beneficiary
    needs. Anything after the trailer is the page's own furniture and
    belongs to no payment.

    Proved means both of the trailer's claims hold: as many payments
    were found as it counts, and they add up to the movement's printed
    total. Either alone is too weak — equal counts with a misread
    amount still moves money, and an accidental sum with the wrong
    count still merges two payments.
    """
    trailer = _stmt_multi_trailer(cont_lines)
    if trailer is None or total is None:
        return None
    count, _rail, at = trailer
    if count < 2:
        return None
    legs: list[dict] = []
    for line in cont_lines[:at]:
        line = str(line).rstrip()
        m = _STMT_LEG_AMOUNT_RE.search(line)
        if m:
            legs.append({"amount": float(re.sub(r"[ '\u2019]", "",
                                                m.group(1) + "." + m.group(2))),
                         "lines": [line[:m.start()].rstrip()]})
        elif legs:
            legs[-1]["lines"].append(line)
        else:
            return None          # text before the first amount: unknown shape
    if len(legs) != count:
        return None
    if abs(round(sum(x["amount"] for x in legs), 2) - round(total, 2)) > 0.005:
        return None
    return legs


def parse_account_statement_combined(
        pdf_path: Path, doc_token: str) -> tuple[list[dict], list[dict]]:
    """Open an Account-Statement PDF once and run both passes over it:
    the movement-ledger pass (one dict per booking row) and the
    balance-summary pass (opening/closing balance + period bounds). The
    summary pass reuses the first-two-page text the movement pass
    already extracted, so the PDF is laid out once instead of once per
    pass. Returns (cash-balance rows, movement rows)."""
    with pdfplumber.open(pdf_path) as pdf:
        movements, head_text = parse_account_statement_transactions_pages(
            pdf, doc_token, return_head_text=True)
    cash = parse_account_statement_text(head_text, doc_token)
    return cash, movements


def parse_account_statement_transactions_pages(
        pdf, doc_token: str, return_head_text: bool = False):
    """Emit one dict per booking (movement) row from an open pdfplumber
    document (so tests can feed a synthetic one). The loader maps these
    into the silver `transactions` table (deterministic id + per-account
    MT940 cut-over); rows carry the statement-level reconciliation result
    so the loader can gate on it.

    With `return_head_text` set, also returns the concatenated
    first-two-page `extract_text` — the text the balance-summary parser
    consumes — so a single open serves both passes: returns
    (movement rows, head text) instead of just the movement rows."""
    iban = None
    currency = None
    opening = None
    closing = None
    movements: list[dict] = []
    cur: dict | None = None
    seen_closing = False
    head_texts: list[str] = []

    for page in pdf.pages:
        text = _page_text(page)
        if return_head_text and len(head_texts) < 2:
            head_texts.append(text)
        if iban is None:
            m = _STMT_IBAN_RE.search(text)
            if m:
                iban = re.sub(r"\s+", "", m.group(0)).upper()
        if currency is None:
            cm = _CCY_HEADER_RE.search(text.replace(" ", ""))
            if cm:
                currency = cm["ccy"]

        rows = _stmt_cluster_rows(
            page.extract_words(x_tolerance=1.5, keep_blank_chars=False))
        anchors = _stmt_find_header(rows)
        if not anchors:
            continue

        for r in rows:
            if r["top"] <= anchors["top"] + 1:
                continue
            ws = r["words"]
            # Row date = leading DD.MM.YY token in the far-left Date
            # column (x0 < 80). Everything else is content.
            row_date = None
            content = ws
            if _STMT_ROW_DATE_RE.match(ws[0]["text"]) and ws[0]["x0"] < 80:
                row_date = ws[0]["text"]
                content = ws[1:]

            if row_date is None:
                # Continuation line — attach to the preceding movement.
                if cur is not None:
                    cur["_cont"].append(" ".join(w["text"] for w in ws))
                continue

            # Digit-leading tokens → numeric columns (by nearest
            # anchor); alphabetic tokens → Information booking type.
            buckets: dict[str, list[str]] = {}
            info_words: list[str] = []
            for w in content:
                if w["text"][:1].isdigit():
                    col = _stmt_assign_numeric(w, anchors)
                    buckets.setdefault(col, []).append(w["text"])
                else:
                    info_words.append(w["text"])
            info = " ".join(info_words).strip()

            if info == "Opening balance":
                if opening is None:
                    opening = _stmt_amount(buckets.get("Balance", []))
                cur = None
                continue
            if info == "Closing balance":
                closing = _stmt_amount(buckets.get("Balance", []))
                seen_closing = True
                cur = None
                continue

            vd = None
            for t in buckets.get("ValueDate", []):
                if _STMT_ROW_DATE_RE.match(t):
                    vd = t
                    break
            cur = {
                "booking_dmy": row_date,
                "value_dmy": vd,
                "description_kind": info,
                "amount_debit": _stmt_amount(buckets.get("Debits", [])),
                "amount_credit": _stmt_amount(buckets.get("Credits", [])),
                "running_balance": _stmt_amount(buckets.get("Balance", [])),
                "post_closing": seen_closing,
                "_cont": [],
            }
            movements.append(cur)

    # Statement-level reconciliation: opening + Σ(credit − debit)
    # must chain to each row's Balance and the printed closing, over
    # the MAIN ledger only (post-closing trailer excluded).
    reconciled = True
    if opening is None:
        reconciled = False
    else:
        run = opening
        for mv in movements:
            if mv["post_closing"]:
                continue
            run += (mv["amount_credit"] or 0.0) - (mv["amount_debit"] or 0.0)
            bal = mv["running_balance"]
            if bal is not None and abs(run - bal) > 0.02:
                reconciled = False
                break
        if reconciled and closing is not None and abs(run - closing) > 0.02:
            reconciled = False

    # Finalise each row: dates, counter-account, per-statement
    # occurrence index (disambiguates identical same-day bookings and
    # keeps the id stable across the monthly/annual statement overlap
    # and the post-closing trailer), and the payload.
    occ: dict[tuple, int] = {}
    out: list[dict] = []
    for mv in movements:
        booking = _stmt_dmy_to_unix(mv["booking_dmy"])
        value = _stmt_dmy_to_unix(mv["value_dmy"]) or booking
        counter = _stmt_counter_account(mv["_cont"])
        counterparty = mv["_cont"][0] if mv["_cont"] else None
        raw_kind = mv["description_kind"] or None
        # Mandate-funding / book-transfer marker: preserved as payload
        # metadata for the gold returns adapter's external-vs-internal
        # flow classifier. The silver `description_kind` stays the raw
        # booking type — the flow decision (which also needs the
        # counter-IBAN and the relationship's own-account set) happens
        # in gold, not here.
        internal = _stmt_is_internal_transfer(mv["description_kind"], mv["_cont"])
        key = (booking, value, mv["description_kind"],
               mv["amount_debit"], mv["amount_credit"])
        idx = occ.get(key, 0)
        occ[key] = idx + 1

        def emit(amount_debit, amount_credit, cparty, caccount, cont,
                 is_internal, balance, extra=None):
            row = {
                "booking_date": booking,  # noqa: B023
                "value_date": value,  # noqa: B023
                "account_external_id": iban,
                "currency_iso": currency,
                "amount_debit": amount_debit,
                "amount_credit": amount_credit,
                "description_kind": raw_kind,  # noqa: B023
                "counterparty": cparty,
                "counter_account": caccount,
                "running_balance": balance,
                "post_closing": mv["post_closing"],  # noqa: B023
                "occurrence": idx,  # noqa: B023
                "reconciled": reconciled,
                "source_doc_token": doc_token,
            }
            sec_caption, sec_valor = _stmt_security(cont)
            payload = {
                "booking_type": raw_kind,  # noqa: B023
                "internal_transfer": is_internal,
                "running_balance": balance,
                "value_date": mv["value_dmy"],  # noqa: B023
                "counter_account": caccount,
                "continuation": cont,
                "post_closing": mv["post_closing"],  # noqa: B023
                "source": "account_statement_pdf",
            }
            # Promoted beside `counter_account` and for the same reason:
            # the fact is in the continuation either way, and parsing it
            # once here beats every consumer re-deriving it.
            if sec_valor:
                payload["security_caption"] = sec_caption
                payload["security_valor"] = sec_valor
            if extra:
                row.update(extra["row"])
                payload["multi_leg"] = extra["payload"]
            row["payload"] = json.dumps(payload, ensure_ascii=False)
            out.append(row)

        # A bundle becomes one row per payment and the batch row itself
        # is not emitted: every consumer downstream sees plain single
        # transactions, and the total survives as the sum of the legs.
        # A movement that is not a bundle, or one whose split could not
        # be proved, is emitted whole, as printed.
        total = mv["amount_debit"] if mv["amount_debit"] is not None else mv["amount_credit"]
        legs = _stmt_split_multi(mv["_cont"], total)
        if legs is None:
            emit(mv["amount_debit"], mv["amount_credit"], counterparty,
                 counter, mv["_cont"], internal, mv["running_balance"])
            continue
        count, rail, _at = _stmt_multi_trailer(mv["_cont"])
        debit_side = mv["amount_debit"] is not None
        for i, leg in enumerate(legs, start=1):
            lines = [ln for ln in leg["lines"] if ln.strip()]
            emit(
                leg["amount"] if debit_side else None,
                None if debit_side else leg["amount"],
                lines[0] if lines else None,
                # Each leg is read on its own: a counter-account or a
                # mandate marker belongs to the payment that carries it,
                # not to every payment the batch happened to include.
                _stmt_counter_account(lines),
                lines,
                _stmt_is_internal_transfer(mv["description_kind"], lines),
                # The printed balance is the one after the whole batch
                # posted, so it belongs to the last leg. The balances
                # between legs were never printed, and deriving them
                # would state something the statement does not.
                mv["running_balance"] if i == len(legs) else None,
                extra={
                    "row": {"multi_leg_index": i,
                            "multi_parent_debit": mv["amount_debit"],
                            "multi_parent_credit": mv["amount_credit"]},
                    "payload": {"index": i, "count": count, "rail": rail},
                },
            )
    if return_head_text:
        return out, "\n".join(head_texts)
    return out


def _stmt_dmy_to_unix(dmy: str | None) -> int | None:
    """DD.MM.YY (2-digit year, 20YY) → Unix seconds UTC midnight."""
    if not dmy:
        return None
    try:
        d, mo, yy = (int(x) for x in dmy.split("."))
    except ValueError:
        return None
    return _to_unix(date(2000 + yy, mo, d))


# ============================================================
# Credit / Debit Advice parser (per-movement payment advices)
# ============================================================
#
# UBS issues a one-page advice for every payment it books by hand or
# by order: a Credit Advice to the account the money lands on and a
# Debit Advice to the account it leaves. Both sides of one transfer
# are issued as two separate documents, each addressed to its own
# account, and — this is what makes them worth parsing — both print
# the SAME "TRX-No.", which is the "Transaction no." the CSV export
# uses as its transaction id.
#
# That matters because UBS does not issue the CSV export for every
# account kind — a managed mandate's cash sub-account has none — and
# the MT940 feed reaches back only as far as its own go-live. For a
# transfer into such an account, silver therefore holds the debit
# leaving the payment account and nothing receiving it, and a
# consumer that pairs legs to recognise an internal move sees capital
# leaving the household that never left. The advice is the only
# record of the missing leg, and it names the leg it belongs to with
# the same id its twin already carries.
#
# Layout (pdfplumber extract_text, one line per printed line):
#
#     Cash Account for investment solutions CHF
#     IBAN CH00 0000 0000 0000 0000 0
#     Account no.      000-000000.00A
#     Credit Advice
#     Produced on 2 January 2020
#     Information/References
#     TRX-No.          0000 000 XX 0000000   ABC/ABC
#     Bookkeeping entry date            1 January 2020
#     Description
#     By order of                  <- "Order of <date>" / "Beneficiary"
#     <holder name and address>       on a debit advice
#     Details of payment
#     <free-text purpose>
#     Currency            Amount
#     Total amount        CHF          0 000.00
#     Val.                         01.01.2020
#
# Older advices set the address block and the account block in two
# columns that extract_text merges into one printed line ("Herr IBAN
# CH00 ...") — which is what the IBAN scan below allows for.
#
# Not every document UBS labels an advice is a payment advice: a
# mortgage interest settlement and a safe-deposit-box rental bill
# carry the same label and a different layout altogether. They print
# no TRX-No., so the absence of one is what rejects them — a
# document that does not state the id its row would be keyed by is
# not a movement this parser can place.

# "Credit Advice" / "Debit Advice" / "Debit advice" — the headline is the
# only place the document says which way the money went, so the match has
# to survive the casing UBS varies within one archive. The optional tail is
# the production date, which the header sets in a column of its own: whether
# the two arrive as one printed line depends on how the page's columns
# resolve, and a direction lost to a merged line would drop the movement
# silently, which is the one failure this parser must not have.
_ADVICE_HEADLINE_RE = re.compile(
    r"^(?P<dir>Credit|Debit)\s+[Aa]dvice(?:\s+Produced on\b.*)?$")

# "TRX-No.  0000 000 XX 0000000  ABC/ABC" — the id printed in
# space-separated groups, optionally closed by the booking desk's
# initials. The groups are joined WITHOUT the spaces because that is
# how the CSV export spells the same number in its "Transaction no."
# column, and the two spellings have to be one string for the silver
# primary key to bring the two legs of a transfer together.
_ADVICE_TRX_LINE_RE = re.compile(r"^TRX-No\.\s+(?P<rest>\S.*?)\s*$")
_ADVICE_TRX_GROUP_RE = re.compile(r"^[A-Z0-9]+$")
_ADVICE_TRX_JOINED_RE = re.compile(r"^[A-Z0-9]{12,24}$")

# "<D> <Month> <YYYY>", with the month's casing left open (`_MONTHS`).
_ADVICE_BOOKED_RE = re.compile(
    r"^Bookkeeping entry date\s+"
    r"(?P<day>\d{1,2})\s+(?P<month>[A-Za-z]+)\s+(?P<year>\d{4})\s*$"
)
_ADVICE_VALUE_DATE_RE = re.compile(
    r"^Val\.\s+(?P<d>\d{2})\.(?P<m>\d{2})\.(?P<y>\d{4})\s*$"
)
_ADVICE_TOTAL_RE = re.compile(
    r"^Total amount\s+(?P<ccy>[A-Z]{3})\s+(?P<v>[\d\s'’]+\.\d{2})\s*$"
)

_ADVICE_DETAILS_LABEL = "Details of payment"
# The details block runs until the amount table starts. "Currency
# Amount" is that table's column header; the other two are the first
# rows of the table itself, in case a document omits the header.
_ADVICE_DETAILS_END = ("Currency", "Total amount", "Val.")


def _advice_trx_no(rest: str) -> str | None:
    """The TRX-No. groups joined into the CSV export's spelling, or
    None when the line does not hold one.

    The trailing token is the initials of the desk that booked the
    payment ("ABC/ABC", "ABC/XYZ") and is not part of the number; the
    slash is what marks it, and many advices carry none at all. Every
    token before it has to be a bare alphanumeric group — anything
    else means this is not the line we think it is, and a wrong id
    would key a row onto a transfer it has nothing to do with.
    """
    groups: list[str] = []
    for token in rest.split():
        if "/" in token:
            break
        if not _ADVICE_TRX_GROUP_RE.match(token):
            return None
        groups.append(token)
    joined = "".join(groups)
    return joined if _ADVICE_TRX_JOINED_RE.match(joined) else None


def _advice_is_internal_transfer(details: list[str]) -> bool:
    """True when the advice's free-text purpose names an intra-
    portfolio reshuffle (mandate funding / reduction, book transfer).

    Reads the SAME name-free vocabulary as the Account-Statement
    walker (`_STMT_INTERNAL_MARKERS`) but without its booking-type
    gate: that gate exists to keep a securities settlement or a
    dividend from being re-tagged, and an advice prints no booking
    type at all — the only text it gives is the purpose line, which
    is written for exactly this kind of move ("Uebertrag <portfolio>",
    "Increase <name> Mandate", "Reduktion <name> Mandat"). A genuine
    outbound payment's purpose line names the order and the
    beneficiary and carries none of the markers.
    """
    blob = " ".join(details).upper()
    return any(m in blob for m in _STMT_INTERNAL_MARKERS)


def parse_payment_advice(pdf_path: Path, doc_token: str) -> list[dict]:
    """Walk a Credit/Debit Advice PDF and emit at most ONE movement
    row for the account the advice is addressed to."""
    return parse_payment_advice_text(_pdf_text(pdf_path, 2), doc_token)


def parse_payment_advice_text(text: str, doc_token: str) -> list[dict]:
    """Pure-text variant of `parse_payment_advice`, for fixture-based
    tests. Returns [] for any document that does not state every fact
    a movement row needs — the direction, the transaction number, the
    account, the booked date and the amount — so the advice labels
    that are really a mortgage settlement or a safe-box rental bill
    fall out here rather than landing as a row with holes in it."""
    direction = None
    trx_no = None
    trx_printed = None
    booked = None
    value_dmy = None
    currency = None
    amount = None
    iban = None
    details: list[str] = []
    in_details = False

    for raw_line in text.splitlines():
        ln = _undouble_bold(raw_line.strip())
        if not ln:
            continue
        if iban is None:
            # Anywhere in the line, not anchored to its start: the
            # older layout sets the addressee column and the account
            # column on one printed line. Every advice in the archive
            # names exactly one IBAN, the account it is addressed to.
            m = _STMT_IBAN_RE.search(ln)
            if m:
                iban = re.sub(r"\s+", "", m.group(0)).upper()
        if in_details:
            if ln.startswith(_ADVICE_DETAILS_END):
                in_details = False
            else:
                details.append(ln)
                continue
        if ln == _ADVICE_DETAILS_LABEL:
            in_details = True
            continue
        if direction is None:
            m = _ADVICE_HEADLINE_RE.match(ln)
            if m:
                direction = m["dir"].lower()
                continue
        if trx_no is None:
            m = _ADVICE_TRX_LINE_RE.match(ln)
            if m:
                trx_no = _advice_trx_no(m["rest"])
                if trx_no:
                    trx_printed = m["rest"]
                continue
        if booked is None:
            m = _ADVICE_BOOKED_RE.match(ln)
            if m:
                booked = _long_date(m)
                continue
        if amount is None:
            m = _ADVICE_TOTAL_RE.match(ln)
            if m:
                currency = m["ccy"]
                amount = _to_float(m["v"])
                continue
        if value_dmy is None:
            m = _ADVICE_VALUE_DATE_RE.match(ln)
            if m:
                value_dmy = date(int(m["y"]), int(m["m"]), int(m["d"]))
                continue

    if (direction is None or not trx_no or booked is None or iban is None
            or amount is None or not currency):
        return []

    booking_date = _to_unix(booked)
    # The advice prints its amount as an unsigned total and says which
    # way it moved in the headline, so the figure goes in the column
    # the direction names — the same convention the Account-Statement
    # walker keeps, where the statement prints a debit as a positive
    # figure in its debit column and the column, not the sign, carries
    # the direction (DESIGN.md §3.6).
    debit = abs(amount) if direction == "debit" else None
    credit = abs(amount) if direction == "credit" else None
    internal = _advice_is_internal_transfer(details)
    return [{
        # UBS's own Transaction no., not a collector-minted id: the
        # advice states the number its twin leg already carries in the
        # CSV export, and storing it verbatim is what puts the two
        # legs under one id in silver's compound primary key.
        "transaction_external_id": trx_no,
        "booking_date": booking_date,
        "value_date": _to_unix(value_dmy) if value_dmy else booking_date,
        "account_external_id": iban,
        "currency_iso": currency,
        "amount_debit": debit,
        "amount_credit": credit,
        # The purpose line, which is what the statement era puts here
        # too (its first continuation line). Deliberately NOT the "By
        # order of" / "Beneficiary" block: on an advice for a move
        # between the holder's own accounts that block is the holder,
        # and the purpose is the only text that says anything about
        # the movement.
        "counterparty": details[0] if details else None,
        # An advice prints no booking type and no counter IBAN. Both
        # stay empty rather than being invented: gold reads the
        # booking type to promote a row to external capital, and a
        # type this document never printed would do exactly that.
        "description_kind": None,
        "counter_account": None,
        "source_doc_token": doc_token,
        "payload": json.dumps({
            "booking_type": None,
            "internal_transfer": internal,
            "counter_account": None,
            "continuation": details,
            # `source` names the RAIL a row arrived on rather than the
            # document it was read from. Gold's web reader asks two
            # questions of this value and both are "PDF archive, or
            # MT940/CSV feed?": it dates the feed from the rows that
            # do NOT carry this marker, and it gates the conservative
            # external-vs-internal classifier on the rows that do. An
            # advice belongs on the PDF side of both — it is not a
            # feed row, and it is at least as counterparty-poor as a
            # statement row. A value of its own here would date the
            # MT940 feed to the oldest advice in the archive instead
            # of to the feed's own first day, and promote years of
            # deep-era backfill rows to external capital on the way.
            # Renaming it is a coordinated change with the gold
            # adapter, not a collector-side one.
            "source": "account_statement_pdf",
            # Which document the row was read from — and the marker
            # the loader's document pass deletes on, since an advice
            # row is keyed by UBS's transaction number and so carries
            # no id prefix to recognise it by (load.py
            # `_DOCUMENT_PASS_DELETES`).
            "document": "payment_advice_pdf",
            "advice_direction": direction,
            # The id as the advice PRINTS it. The normalisation from
            # this to the export's spelling is the whole hinge of the
            # pairing, so the printed form travels with the row and a
            # bad join can be read back out of silver.
            "trx_no_printed": trx_printed,
        }, ensure_ascii=False),
    }]


# ============================================================
# Securities advices: capital calls and contract notes
# ============================================================
#
# Two document kinds state the price a holding was bought at where the
# statement of assets and the MT535 feed state none. Each yields at most
# one `advices` row, every figure as printed.
#
# A capital call reaches the archive as a "Private Market Letter": a UBS
# cover page titled "Capital Call", then the fund administrator's
# notice. Quarterly reports and other letters share that label, so the
# cover title is what selects a call. The notice is the administrator's
# own prose with no fixed layout, so each figure is read from the
# sentence that states it (synthetic example, whitespace collapsed):
#
#   Example Fund ("EF") - Capital Call No. 23 Investing in Example LP
#   ISIN - XX0000000000
#   … will now make Capital Call No. 23 of USD 12,345.67, which
#   represents 7.25% of your Net Commitment of USD 170,285.10. …
#   … makes the amount of USD 12,345.67 available in your account by
#   value date 15 March 2030. …
#   Total 1,234,567.00 12,345.67
#
# The breakdown's "Total" line ends in the investor's column: the called
# amount plus anything charged on top of it, such as equalisation
# interest on a late closing. That is the cash the call takes.
#
# A contract note confirms a purchase outside the exchange (synthetic):
#
#   Contract note
#   Produced on 30 March 2030
#   New issue purchase
#   Trade date: 15.03.2030 Place of transaction Issuer
#   Settlement date: 30.03.2030
#   Quantity Security 1234567 ISIN XX0000000000 Price
#   1 000 Example Fund SICAV USD 90.00
#   E-USD-capitalisation
#   Market value in trading currency USD 90 000.00
#   Placement Fee USD 900.00
#   Swiss federal stamp duty USD 120.00
#   To the debit of account 0000 00000000.XX USD Value date 30.03.2030 USD 91 020.00
#
# A prepayment towards a subscription prints no quantity line: the ISIN
# stands on a line of its own below the fund name, and the conversion
# rate reads "USD / CHF at 0.90000". The subscription that settles
# against it prints "Minus your prepayment" and debits the difference.

_PRODUCED_ON_RE = re.compile(rf"^Produced on\s+{_LONG_DATE}\s*$", re.M)

_CALL_TITLE_RE = re.compile(r"^Capital Call\s*$", re.M)
# Usually a line of its own; a notice may instead set it at the end of
# the "Investing in" line.
_CALL_ISIN_RE = re.compile(
    rf"(?:^|\s)ISIN\s*[-–]\s*(?P<isin>{_ISIN_SHAPE})\s*$", re.M)
_CALL_LETTER_DATE_RE = re.compile(rf"^{_LONG_DATE}\s*$")
_CALL_AMOUNT_RE = re.compile(
    r"(?P<ccy>[A-Z]{3})\s+(?P<amount>\d{1,3}(?:,\d{3})*\.\d{2}),?\s+"
    r"which represents")
_CALL_VALUE_DATE_RE = re.compile(
    rf"available in your account\s+(?:by\s+)?value date\s+{_LONG_DATE}")
_CALL_PAYABLE_RE = re.compile(
    r"makes the amount of\s+[A-Z]{3}\s+(?P<amount>\d{1,3}(?:,\d{3})*\.\d{2})"
    r"\s+available")
_CALL_TOTAL_RE = re.compile(
    r"^Total\s+\d{1,3}(?:,\d{3})*\.\d{2}\s+"
    r"(?P<amount>\d{1,3}(?:,\d{3})*\.\d{2})\s*$", re.M)

_NOTE_TITLE_RE = re.compile(r"^Contract note\s*$", re.M)
_NOTE_TRADE_DATE_RE = re.compile(r"^Trade date:\s*(?P<date>\d{2}\.\d{2}\.\d{4})\b")
_NOTE_SETTLEMENT_DATE_RE = re.compile(
    r"^Settlement date:\s*(?P<date>\d{2}\.\d{2}\.\d{4})\b")
_NOTE_SECURITY_HEADER_RE = re.compile(
    rf"^Quantity\s+Security\s+(?P<valor>\d+)\s+ISIN\s+(?P<isin>{_ISIN_SHAPE})"
    r"\s+Price\s*$")
_NOTE_SECURITY_RE = re.compile(
    r"^(?P<quantity>\d[\d ']*(?:\.\d+)?)\s+(?P<name>\S.*?)\s+"
    r"(?P<ccy>[A-Z]{3})\s+(?P<price>\d[\d ']*\.\d+)\s*$")
_NOTE_ISIN_LINE_RE = re.compile(rf"^(?P<isin>{_ISIN_SHAPE})\s*$")
_NOTE_AMOUNT = r"\s+(?P<ccy>[A-Z]{3})\s+(?P<v>\d[\d ']*\.\d{2})\s*$"
_NOTE_FIGURES = {
    "amount": re.compile(r"^Market value in trading currency" + _NOTE_AMOUNT),
    "prepayment": re.compile(r"^Minus your prepayment" + _NOTE_AMOUNT),
    "placement_fee": re.compile(r"^Placement Fee" + _NOTE_AMOUNT),
    "stamp_duty": re.compile(r"^Swiss federal stamp duty" + _NOTE_AMOUNT),
}
_NOTE_DEBIT_RE = re.compile(
    r"^To the debit of account\s+.+?\s+[A-Z]{3}\s+Value date\s+"
    r"(?P<date>\d{2}\.\d{2}\.\d{4})" + _NOTE_AMOUNT)
_NOTE_FX_RES = (
    re.compile(r"^(?P<base>[A-Z]{3})\s*/\s*(?P<quote>[A-Z]{3})\s+at\s+"
               r"(?P<rate>\d+\.\d+)\s*$"),
    re.compile(r"^For\s+(?P<base>[A-Z]{3})\s*/\s*(?P<quote>[A-Z]{3})\s+"
               r"conversions, we have used the following rate:\s*"
               r"(?P<rate>\d+\.\d+)\.?\s*$"),
)


def _long_date_to_unix(m: re.Match) -> int | None:
    """`_long_date` as Unix seconds UTC midnight."""
    d = _long_date(m)
    return _to_unix(d) if d else None


def _advice_row(kind: str, doc_token: str) -> dict:
    """An `advices` row with every figure unset."""
    return {
        "source_doc_token": doc_token, "kind": kind, "title": None,
        "doc_date": None, "trade_date": None, "value_date": None,
        "instrument_isin": None, "valor": None, "security_name": None,
        "currency_iso": None, "quantity": None, "price": None,
        "amount": None, "prepayment": None, "placement_fee": None,
        "stamp_duty": None, "settlement_amount": None,
        "settlement_currency_iso": None, "fx_rate": None,
        "fx_rate_pair": None, "payload": "{}",
    }


def parse_capital_call(pdf_path: Path, doc_token: str) -> list[dict]:
    """Read a capital call from a Private Market Letter. Returns [] for a
    letter that is not one, which is told from the cover page alone so a
    long quarterly report is not read in full."""
    with pdfplumber.open(pdf_path) as pdf:
        if not pdf.pages or not _CALL_TITLE_RE.search(_page_text(pdf.pages[0])):
            return []
        text = "\n".join(_page_text(p) for p in pdf.pages[:4])
    return parse_capital_call_text(text, doc_token)


def parse_capital_call_text(text: str, doc_token: str) -> list[dict]:
    """Pure-text variant of `parse_capital_call`. Returns [] unless the
    document is a capital call stating its ISIN and its called amount,
    the two facts the row is about."""
    if not _CALL_TITLE_RE.search(text):
        return []
    lines = [ln.strip() for ln in text.splitlines()]
    flat = " ".join(ln for ln in lines if ln)
    isin_m = _CALL_ISIN_RE.search(text)
    amount_m = _CALL_AMOUNT_RE.search(flat)
    if isin_m is None or amount_m is None:
        return []

    row = _advice_row("capital_call", doc_token)
    printed = {"amount": amount_m.group(0), "isin": isin_m.group(0).strip()}
    row["instrument_isin"] = isin_m["isin"]
    row["currency_iso"] = row["settlement_currency_iso"] = amount_m["ccy"]
    row["amount"] = _to_float(amount_m["amount"])

    # The title is the notice's own heading: the lines between the
    # administrator's dated line and the ISIN line.
    isin_at = next(k for k, ln in enumerate(lines) if _CALL_ISIN_RE.search(ln))
    title: list[str] = []
    for ln in reversed(lines[max(0, isin_at - 4):isin_at]):
        if not ln or _CALL_LETTER_DATE_RE.match(ln):
            break
        title.insert(0, ln)
    row["title"] = " ".join(title) or None

    m = _PRODUCED_ON_RE.search(text)
    if m:
        row["doc_date"] = _long_date_to_unix(m)
    m = _CALL_VALUE_DATE_RE.search(flat)
    if m:
        row["value_date"] = _long_date_to_unix(m)
        printed["value_date"] = m.group(0)
    m = _CALL_TOTAL_RE.search(text) or _CALL_PAYABLE_RE.search(flat)
    if m:
        row["settlement_amount"] = _to_float(m["amount"])
        printed["settlement_amount"] = m.group(0)
    row["payload"] = json.dumps(printed, ensure_ascii=False)
    return [row]


def parse_contract_note(pdf_path: Path, doc_token: str) -> list[dict]:
    """Read the purchase a contract note confirms."""
    return parse_contract_note_text(_pdf_text(pdf_path, 2), doc_token)


def parse_contract_note_text(text: str, doc_token: str) -> list[dict]:
    """Pure-text variant of `parse_contract_note`. Returns [] unless the
    document is a contract note stating an ISIN and a market value."""
    if not _NOTE_TITLE_RE.search(text):
        return []
    lines = [ln.strip() for ln in text.splitlines() if ln.strip()]
    row = _advice_row("contract_note", doc_token)
    printed: dict[str, str] = {}
    name: list[str] = []
    dates_end = None     # the line after the settlement date
    in_name = False      # reading the security name's wrapped lines

    for k, ln in enumerate(lines):
        m = _PRODUCED_ON_RE.match(ln)
        if m and row["doc_date"] is None:
            row["doc_date"] = _long_date_to_unix(m)
            if k + 1 < len(lines):
                row["title"] = lines[k + 1]
            continue
        m = _NOTE_TRADE_DATE_RE.match(ln)
        if m:
            row["trade_date"] = _dmy_to_unix(m["date"])
            continue
        m = _NOTE_SETTLEMENT_DATE_RE.match(ln)
        if m:
            row["value_date"] = _dmy_to_unix(m["date"])
            dates_end = k + 1
            continue
        m = _NOTE_SECURITY_HEADER_RE.match(ln)
        if m:
            row["valor"], row["instrument_isin"] = m["valor"], m["isin"]
            printed["security"] = ln
            continue
        if row["valor"] is not None and row["quantity"] is None:
            m = _NOTE_SECURITY_RE.match(ln)
            if m:
                row["quantity"] = _to_float(m["quantity"])
                row["price"] = _to_float(m["price"])
                row["currency_iso"] = m["ccy"]
                name.append(m["name"])
                printed["quantity"] = ln
                in_name = True
                continue
        m = _NOTE_ISIN_LINE_RE.match(ln)
        if m and row["instrument_isin"] is None and dates_end is not None:
            # A prepayment: the fund name is the lines between the dates
            # and the ISIN, less the phrase that introduces it
            # ("Prepayment for Subscription of").
            row["instrument_isin"] = m["isin"]
            name = [n for n in lines[dates_end:k] if not n.endswith(" of")]
            printed["isin"] = ln
            continue
        col = next((c for c, rx in _NOTE_FIGURES.items() if rx.match(ln)),
                   None)
        if col:
            fm = _NOTE_FIGURES[col].match(ln)
            in_name = False
            row[col] = _to_float(fm["v"])
            row["currency_iso"] = row["currency_iso"] or fm["ccy"]
            printed[col] = ln
            continue
        if in_name:
            name.append(ln)
            continue
        m = _NOTE_DEBIT_RE.match(ln)
        if m:
            row["value_date"] = _dmy_to_unix(m["date"])
            row["settlement_amount"] = _to_float(m["v"])
            row["settlement_currency_iso"] = m["ccy"]
            printed["settlement_amount"] = ln
            continue
        for rx in _NOTE_FX_RES:
            m = rx.match(ln)
            if m:
                row["fx_rate"] = _to_float(m["rate"])
                row["fx_rate_pair"] = f"{m['base']}/{m['quote']}"
                printed["fx_rate"] = ln
                break

    if row["instrument_isin"] is None or row["amount"] is None:
        return []
    row["security_name"] = " ".join(name) or None
    row["payload"] = json.dumps(printed, ensure_ascii=False)
    return [row]
