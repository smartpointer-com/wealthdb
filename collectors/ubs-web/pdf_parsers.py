"""PDF parsers for UBS Switzerland statement archive.

Both parsers are built on pdfplumber's `extract_text()`:

  parse_statement_of_assets(pdf_path, doc_token, label)
      → list of position snapshots from one Statement-of-assets PDF.

  parse_account_statement_combined(pdf_path, doc_token, label)
      → (cash-balance rows, movement rows) from one Account-Statement
        PDF — opened once and laid out for both passes.

Both return plain dicts ready for the loader to insert into the
silver `historical_*` tables.

Parsing strategy: PDF tables in UBS statements are not real PDF
tables (no row / column structure for pdfplumber to detect — see
the empty `extract_tables()` output during probing). They are
visually-aligned text columns. We extract the page text and walk
it line-by-line, anchoring on identifiable markers:

  - "Statement of assets as of <DDMMYYYY>" in the label → as_of_date
  - "Portfolio number 230-AAAAAAAA-NN" in body → portfolio number
  - "Valued in <CCY>" header → portfolio base currency
  - "Valor <num> - ISIN <code>" line → securities position anchor
  - IBAN-shaped line → cash position anchor
  - "Account Statement / DD.MM.YYYY - DD.MM.YYYY" → period range
  - "Opening balance / Closing balance" lines → cash deltas

Performance note: schwab-web migrated this archive type to pypdfium2
for a 10× speedup. ubs-web stays on pdfplumber for now because the
visual-line reconstruction PDFium needs (count_rects() returns
either cell-level granularity that splits a row across many lines,
or column-shared-baseline rects that merge columns that
pdfplumber kept apart) can't be made to round-trip the existing
parser without a substantial regex layer rewrite. parse_account_
statement's squish-and-match approach DOES round-trip
byte-identical under pypdfium2; if perf becomes acute, that one
parser is a candidate for an isolated migration.
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

_LABEL_ACCT_STMT_RE = re.compile(
    r"Account Statement\s+"
    r"(?P<day>\d{2})\.(?P<month>\d{2})\.(?P<year>\d{4})\s+"
    r"\d{2}\s\S+\s\d{4}\s+.*?"
    r"\b\d{3}-\d+\.(?P<acct_suffix>\S+)"
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


def parse_label_account_statement(label: str) -> dict | None:
    """Extract issue date and account suffix from the listing
    label of an Account-Statement PDF."""
    m = _LABEL_ACCT_STMT_RE.search(label)
    if not m:
        return None
    issued = date(int(m["year"]), int(m["month"]), int(m["day"]))
    return {
        "issued_date": _to_unix(issued),
        "issued_str": issued.isoformat(),
        "account_suffix": m["acct_suffix"],        # e.g. '40X' or 'IUN'
    }


def _to_unix(d: date) -> int:
    return int(datetime(d.year, d.month, d.day, tzinfo=timezone.utc).timestamp())


# ============================================================
# Statement of assets parser
# ============================================================

# `Valued in EUR` (rendered as `ValuedinEUR` or `Valued in EUR` etc.)
_BASE_CCY_RE = re.compile(r"Valued in (?P<ccy>[A-Z]{3})\b")

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

# Securities-position anchor line:
#   "Valor 123456 - ISIN DE0000000080"
_VALOR_ISIN_RE = re.compile(
    r"^\s*Valor\s+(?P<valor>\S+)\s+-\s+ISIN\s+(?P<isin>[A-Z0-9]{12})\s*$"
)

# Securities-position headline:
#   "100 Reg.shs Example Equity AG (XMPL) EUR 100.000000 120.5 10.00% 12 050 1.25"
#
# UBS prints a one-letter price qualifier after the market price on
# some rows — e.g. a structured product's estimated/indicative
# price renders as "120.00 B 20.00%" (synthetic example). The optional
# `[A-Za-z]` flag group swallows it so those rows still parse; without
# it the whole headline failed to match and the position dropped
# silently (the same instrument parses fine in periods where UBS omits
# the flag).
_SECURITY_HEADLINE_RE = re.compile(
    r"^\s*(?P<units>-?[\d\s']+(?:\.\d+)?)\s+(?P<desc>.+?)\s+"
    r"(?P<ccy>[A-Z]{3})\s+(?P<cost_price>[\d\s']+\.\d+)\s+"
    r"(?P<market_price>[\d\s']+\.?\d*)\s+(?:[A-Za-z]\s+)?"
    r"(?P<gain_pct>-?\d+\.\d+%)\s+"
    r"(?P<market_value>-?[\d\s']+)\s+(?P<pct_na>-?\d+\.\d{2})\s*$"
)

# Private-markets / alternatives headline. Same outer column anchors
# as _SECURITY_HEADLINE_RE, but the middle pricing block differs:
# UBS-sponsored Private Markets funds and SPV interests carry a single
# exchange rate (or the literal "n.a.") where listed securities print
# the cost-price / market-price / market-gain triple. Synthetic
# examples of the two forms:
#   "1 000 Example PE Fund   USD  1.0500  12 345  5.00"
#   "2 000 Example PE Fund   USD  n.a.    0        0.00"
# The first form is the funded "Outstanding Shares" holding (real
# NAV in market_value); the "n.a." form is a Net/Unfunded Commitment
# tracking row with a 0 market value. Tried only as a fallback after
# _SECURITY_HEADLINE_RE so listed-security parsing is unchanged.
_PM_HEADLINE_RE = re.compile(
    r"^\s*(?P<units>-?[\d\s']+(?:\.\d+)?)\s+(?P<desc>.+?)\s+"
    r"(?P<ccy>[A-Z]{3})\s+(?P<rate>n\.a\.|[\d\s']+\.\d+)\s+"
    r"(?P<market_value>-?[\d\s']+)\s+(?P<pct_na>-?\d+\.\d{2})\s*$"
)

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


def parse_statement_of_assets(pdf_path: Path, doc_token: str,
                              label: str) -> list[dict]:
    """Walk a Statement-of-assets PDF and emit one row per detected
    position. Each row is a dict ready for INSERT into the
    `historical_position_snapshots` table."""
    with pdfplumber.open(pdf_path) as pdf:
        full_text = "\n".join(
            (p.extract_text(x_tolerance=2) or "") for p in pdf.pages
        )
    return parse_statement_of_assets_text(full_text, doc_token, label)


def parse_statement_of_assets_text(full_text: str, doc_token: str,
                                   label: str) -> list[dict]:
    """Pure-text variant of parse_statement_of_assets — same row
    shape, but takes already-extracted PDF text so the regex /
    line-walk layer can be exercised without a real PDF on disk."""
    label_meta = parse_label_statement_of_assets(label)
    if label_meta is None:
        return []

    # PSN-style portfolio identifier: 16 chars = 4-digit branch +
    # 8-digit base + 4-digit portfolio number, all zero-padded.
    # UBS strips the branch's leading zero in the PDF label
    # (`BBB-AAAAAAAA-NN` instead of `BBBB-AAAAAAAA-NN`); the
    # zfill(4) below restores it so the value joins to PSN's
    # `portfolios.portfolio_external_id` directly.
    branch, base = label_meta["account_number_prefix"].split("-", 1)
    psn_portfolio = _assemble_psn_portfolio(
        branch, base, label_meta["portfolio_number"],
        label_meta["account_number_prefix"])

    base_ccy = None
    m = _BASE_CCY_RE.search(full_text)
    if m:
        base_ccy = m["ccy"]

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
                "as_of_date": label_meta["as_of_date"],
                "portfolio_external_id": psn_portfolio,
                "account_external_id": iban,
                "instrument_isin": None,
                "currency_iso": pending_cash["ccy"],
                "units": pending_cash["units"],
                "market_value": pending_cash["market_value"],
                "market_value_currency": base_ccy,
                "cost_price": None,
                "market_price": None,
                "accrued_interest": None,
                "exchange_rate_to_base": pending_cash["fx"],
                "description": pending_cash["desc"],
                "sector": None,
                "source_doc_token": doc_token,
                "payload": json.dumps({"raw": pending_cash, "iban_line": line.strip()}),
            })
            pending_cash = None

    # --- Securities positions: anchor on the Valor/ISIN line, look
    # back up to 10 lines for the headline. The headline is either a
    # listed-security row (_SECURITY_HEADLINE_RE) or a private-markets
    # row (_PM_HEADLINE_RE); the listed form is tried first so its
    # parsing is unchanged. We keep the CLOSEST match above the ISIN
    # line, of either kind, so adjacent blocks don't cross-attribute. ---
    for i, line in enumerate(section):
        vi = _VALOR_ISIN_RE.match(line)
        if not vi:
            continue
        isin = vi["isin"]
        headline = None
        headline_is_pm = False
        sector = None
        for j in range(max(0, i - 10), i):
            prev = section[j]
            hm = _SECURITY_HEADLINE_RE.match(prev)
            if hm:
                headline = hm
                headline_is_pm = False
            else:
                pm = _PM_HEADLINE_RE.match(prev)
                if pm:
                    headline = pm
                    headline_is_pm = True
            # Sector lives on the second line of a listed-security row,
            # usually right after the description (e.g. 'Financials',
            # 'Information Tech.'). Private-markets rows have no sector
            # column, so only scan for it when a listed headline is in
            # play.
            if headline is not None and not headline_is_pm and j > 0:
                stripped = prev.strip()
                if stripped and not any(
                    ch.isdigit() for ch in stripped.split()[-1]
                ) and len(stripped) < 50:
                    sector = stripped
        if headline is None:
            continue

        if headline_is_pm:
            mv = _to_float(headline["market_value"])
            # Skip Net/Unfunded Commitment tracking rows: they print
            # an 'n.a.' price and a 0 market value. The funded
            # "Outstanding Shares" row carries the real NAV.
            if not mv:
                continue
            rate = (None if headline["rate"] == "n.a."
                    else _to_float(headline["rate"]))
            results.append({
                "as_of_date": label_meta["as_of_date"],
                "portfolio_external_id": psn_portfolio,
                "account_external_id": "",
                "instrument_isin": isin,
                "currency_iso": headline["ccy"],
                "units": _to_float(headline["units"]),
                "market_value": mv,
                "market_value_currency": base_ccy,
                "cost_price": None,
                "market_price": None,
                "accrued_interest": None,
                "exchange_rate_to_base": rate,
                "description": headline["desc"].strip(),
                "sector": None,
                "source_doc_token": doc_token,
                "payload": json.dumps({
                    "valor": vi["valor"],
                    "isin": isin,
                    "kind": "private_market",
                    "headline": headline.group(),
                }),
            })
            continue

        results.append({
            "as_of_date": label_meta["as_of_date"],
            "portfolio_external_id": psn_portfolio,
            "account_external_id": "",
            "instrument_isin": isin,
            "currency_iso": headline["ccy"],
            "units": _to_float(headline["units"]),
            "market_value": _to_float(headline["market_value"]),
            "market_value_currency": base_ccy,
            "cost_price": _to_float(headline["cost_price"]),
            "market_price": _to_float(headline["market_price"]),
            "accrued_interest": None,
            "exchange_rate_to_base": None,
            "description": headline["desc"].strip(),
            "sector": sector,
            "source_doc_token": doc_token,
            "payload": json.dumps({
                "valor": vi["valor"],
                "isin": isin,
                "headline": headline.group(),
            }),
        })

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
            full_text, label_meta, branch, base, base_ccy, doc_token))

    return results


def _assemble_psn_portfolio(branch: str, base: str, portfolio_no: str,
                            acct_no_prefix: str) -> str:
    """Build the 16-char PSN-aligned portfolio_external_id and
    loud-fail on length drift. The downstream gold layer joins on
    this column; a wrong length silently double-counts every
    position, so it's caught at parse time with the source row in the
    exception context."""
    psn_portfolio = f"{branch.zfill(4)}{base.zfill(8)}{portfolio_no.zfill(4)}"
    if len(psn_portfolio) != 16:
        raise ValueError(
            f"portfolio_external_id length != 16: {psn_portfolio!r} "
            f"(from acct_no={acct_no_prefix!r}, "
            f"portfolio_no={portfolio_no!r})"
        )
    return psn_portfolio


def _overview_precious_metals(full_text: str, label_meta: dict,
                              branch: str, base: str, base_ccy: str,
                              doc_token: str) -> list[dict]:
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
        port16 = _assemble_psn_portfolio(
            branch, base, current_no, label_meta["account_number_prefix"])
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
            "as_of_date": label_meta["as_of_date"],
            "portfolio_external_id": port16,
            "account_external_id": "",
            "instrument_isin": synth_key,
            "currency_iso": base_ccy,
            "units": None,
            "market_value": mv,
            "market_value_currency": base_ccy,
            "cost_price": None,
            "market_price": None,
            "accrued_interest": None,
            "exchange_rate_to_base": None,
            "description": "Precious metals & commodities",
            "sector": None,
            "source_doc_token": doc_token,
            "payload": json.dumps({
                "kind": "overview_asset_class",
                "asset_class": "precious_metals",
                "portfolio_no": current_no,
                "market_value": pmm["mv"],
            }),
        })
    return rows


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


def parse_maturity_notice(pdf_path: Path, doc_token: str,
                          label: str) -> list[dict]:
    """Walk a 'Maturity notice' PDF and emit ONE row capturing the
    mortgage's outstanding principal at the notice's `As at` date."""
    with pdfplumber.open(pdf_path) as pdf:
        text = "\n".join(
            (p.extract_text(x_tolerance=2) or "") for p in pdf.pages[:2]
        )
    return parse_maturity_notice_text(text, doc_token)


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
# account stamp ("HYPOTHEK <base>.H1D 0002" / ".H1Y 0003").
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
                for w in r["words"]:
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


# A securities line's continuation carries the instrument's caption and,
# closing it, the Swiss VALOR — a globally unique identifier for a
# security line, which is what makes the match to gold's instrument
# dimension an identity rather than a guess.
#
#     "EXAMPLEETF WORLD 1234567"
#
# A valor is the LAST whitespace-separated token, all digits, with
# something bearing a letter in front of it. A turnover trailer
# ("Turnover total 1 111 111.11 ...") ends in a decimal group and falls
# out on its own.
#
# A TRADE'S SETTLEMENT REFERENCE DOES NOT. It is a letter, a date and a
# number — "V 01.01.2020 12345678" — and that number is not a valor. It
# also comes FIRST, so a scan that takes the earliest match takes the
# reference and never reaches the security line below it. Both reference
# shapes are skipped by their own pattern rather than left to the valor
# test to reject, because one of them passes it.
#
# A caption that legitimately ends in digits would still mint a number
# that is not a valor. Nothing here guards that and nothing needs to:
# the consumer looks the number up in the instrument dimension, and a
# number that is not a valor matches nothing.
_STMT_REFERENCE_RE = re.compile(r"^[A-Z]\s+\d{2}\.\d{2}\.\d{4}\b")
_STMT_VALOR_RE = re.compile(r"^(?P<caption>.*[A-Za-z].*?)\s+(?P<valor>\d{4,12})$")
# A long caption leaves the statement no room for the space, and the
# valor is printed hard against it ("Example Group Rg199999991"). Tried
# only where the spaced form found nothing, so an ordinary line is
# parsed exactly as before. The floor is higher here — six digits, not
# four — because with no separator a short run is far likelier to be the
# tail of a name than a valor.
_STMT_GLUED_VALOR_RE = re.compile(r"^(?P<caption>.*[A-Za-z].*?)(?P<valor>\d{6,12})$")


def _stmt_security(cont_lines: list[str]) -> tuple[str | None, str | None]:
    """The instrument caption and valor a securities movement names.

    Returns (caption, valor), or (None, None) where no continuation line
    closes with a valor — which is every movement that is not a trade.
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
        pdf_path: Path, doc_token: str, label: str
        ) -> tuple[list[dict], list[dict]]:
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
        text = page.extract_text(x_tolerance=2) or ""
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
                "booking_date": booking,
                "value_date": value,
                "account_external_id": iban,
                "currency_iso": currency,
                "amount_debit": amount_debit,
                "amount_credit": amount_credit,
                "description_kind": raw_kind,
                "counterparty": cparty,
                "counter_account": caccount,
                "running_balance": balance,
                "post_closing": mv["post_closing"],
                "occurrence": idx,
                "reconciled": reconciled,
                "source_doc_token": doc_token,
            }
            sec_caption, sec_valor = _stmt_security(cont)
            payload = {
                "booking_type": raw_kind,
                "internal_transfer": is_internal,
                "running_balance": balance,
                "value_date": mv["value_dmy"],
                "counter_account": caccount,
                "continuation": cont,
                "post_closing": mv["post_closing"],
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
        # be proved, is emitted whole exactly as before.
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

_ADVICE_BOOKED_RE = re.compile(
    r"^Bookkeeping entry date\s+"
    r"(?P<d>\d{1,2})\s+(?P<mon>[A-Za-z]+)\s+(?P<y>\d{4})\s*$"
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

# The advices are issued in English throughout the archive and print
# the booking date as "<D> <Month> <YYYY>". Spelled out here rather
# than handed to strptime("%d %B %Y"), whose month names come from
# the process locale: the loader runs in a container whose locale is
# whatever the base image happens to set, and a date that parses on
# one machine and not another would silently drop rows.
_ADVICE_MONTHS = {
    "january": 1, "february": 2, "march": 3, "april": 4,
    "may": 5, "june": 6, "july": 7, "august": 8,
    "september": 9, "october": 10, "november": 11, "december": 12,
}


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


def parse_payment_advice(pdf_path: Path, doc_token: str,
                         label: str) -> list[dict]:
    """Walk a Credit/Debit Advice PDF and emit at most ONE movement
    row for the account the advice is addressed to."""
    with pdfplumber.open(pdf_path) as pdf:
        text = "\n".join(
            (p.extract_text(x_tolerance=2) or "") for p in pdf.pages[:2]
        )
    return parse_payment_advice_text(text, doc_token)


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
                month = _ADVICE_MONTHS.get(m["mon"].lower())
                if month:
                    booked = date(int(m["y"]), month, int(m["d"]))
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
