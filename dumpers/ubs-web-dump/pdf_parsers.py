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
_SECURITY_HEADLINE_RE = re.compile(
    r"^\s*(?P<units>-?[\d\s']+(?:\.\d+)?)\s+(?P<desc>.+?)\s+"
    r"(?P<ccy>[A-Z]{3})\s+(?P<cost_price>[\d\s']+\.\d+)\s+"
    r"(?P<market_price>[\d\s']+\.?\d*)\s+(?P<gain_pct>-?\d+\.\d+%)\s+"
    r"(?P<market_value>-?[\d\s']+)\s+(?P<pct_na>-?\d+\.\d{2})\s*$"
)


def parse_statement_of_assets(pdf_path: Path, doc_token: str,
                              label: str) -> list[dict]:
    """Walk a Statement-of-assets PDF and emit one row per detected
    position. Each row is a dict ready for INSERT into the
    `historical_position_snapshots` table."""
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
    psn_portfolio = (
        f"{branch.zfill(4)}"
        f"{base.zfill(8)}"
        f"{label_meta['portfolio_number'].zfill(4)}"
    )
    # Loud-fail if the assembly ever drifts from PSN's shape. The
    # downstream gold layer joins on this column; a wrong length
    # silently double-counts every position. Caught at parse time
    # rather than at insert time so the source row is in the
    # exception context.
    if len(psn_portfolio) != 16:
        raise ValueError(
            f"portfolio_external_id length != 16: {psn_portfolio!r} "
            f"(from acct_no={label_meta['account_number_prefix']!r}, "
            f"portfolio_no={label_meta['portfolio_number']!r})"
        )

    results: list[dict] = []
    with pdfplumber.open(pdf_path) as pdf:
        full_text = "\n".join(
            (p.extract_text(x_tolerance=2) or "") for p in pdf.pages
        )
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
    # back up to 10 lines for the headline. ---
    for i, line in enumerate(section):
        vi = _VALOR_ISIN_RE.match(line)
        if not vi:
            continue
        isin = vi["isin"]
        headline = None
        sector = None
        for j in range(max(0, i - 10), i):
            prev = section[j]
            hm = _SECURITY_HEADLINE_RE.match(prev)
            if hm:
                headline = hm
            # Sector lives on the second line of the row, usually
            # right after the description (e.g. 'Financials',
            # 'Information Tech.', 'Communication').
            if headline is not None and j > 0:
                stripped = prev.strip()
                if stripped and not any(
                    ch.isdigit() for ch in stripped.split()[-1]
                ) and len(stripped) < 50:
                    sector = stripped
        if headline is None:
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
    return results


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
