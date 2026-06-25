"""PDF parsers for UBS Switzerland statement archive.

Two parsers, both built on pdfplumber's `extract_text()`:

  parse_statement_of_assets(pdf_path, doc_token, label)
      → list of position snapshots from one Statement-of-assets PDF.

  parse_account_statement(pdf_path, doc_token, label)
      → list of cash-balance snapshots from one Account-Statement PDF.

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
# portfolio-block line, not the right-hand consolidated column
# (synthetic example):
#   "Precious metals & commodities   12 345   12 345   75.00 ..."
_OVERVIEW_PORTFOLIO_RE = re.compile(r"^Portfolio\s+(?P<no>\d{2})\b")
_OVERVIEW_PRECIOUS_METALS_RE = re.compile(
    r"^Precious metals & commodities\s+"
    r"(?P<mv>\d{1,3}(?:[ ']\d{3})*(?:\.\d+)?)\b"
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
    # UBS issues no Detailed-positions page for the precious-metals
    # custody portfolio, so the gold bar has no per-instrument row in
    # any PDF — only the relationship overview's asset-class total.
    # We recover that value as a synthetic asset-class-level position.
    # The overview prints the same holding once per portfolio-currency
    # PDF (USD / CHF / EUR), so we emit only from USD-valued PDFs (the
    # relationship's reporting currency); the 3 USD copies collapse to
    # one row on the silver PK, giving a single deterministic value
    # that gold converts at query time. The synthetic instrument key
    # is deliberately not ISIN-shaped — the gold adapter detects that
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


def parse_account_statement(pdf_path: Path, doc_token: str,
                            label: str) -> list[dict]:
    """Walk an Account-Statement PDF and emit ONE row with opening +
    closing balance + period bounds for the covered cash account."""
    with pdfplumber.open(pdf_path) as pdf:
        text = "\n".join(
            (p.extract_text(x_tolerance=2) or "") for p in pdf.pages[:2]
        )
    return parse_account_statement_text(text, doc_token)


def parse_account_statement_text(text: str, doc_token: str) -> list[dict]:
    """Pure-text variant of parse_account_statement — same row shape,
    but takes already-extracted PDF text so tests can exercise the
    regex layer without a real PDF on disk."""
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
        # product; classify with the user-facing 'variable' tag
        # rather than 'saron' so the canonical taxonomy stays
        # rate-basis (fixed vs. variable), not product-name (SARON
        # vs. older flavours).
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
