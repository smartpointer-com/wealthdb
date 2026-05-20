"""
Parsers for Schwab monthly brokerage statement PDFs.

The primary use case is reconstructing transaction history for
ACCOUNTS THAT HAVE BEEN CLOSED — those no longer appear in the
Transaction History page, so the only retrievable record is in
their archived monthly statements (and we can fetch the
statements via download.py while at least one open account still
shares the same login).

For active accounts the Transaction History page is the
authoritative source; we keep PDF parsing for cross-checks and
backfill.

Currently implemented:
  - parse_statement_period(text) — extracts (start_date, end_date)
  - parse_transactions(text)     — extracts the "Transaction Details"
                                   rows as a list of dicts

Not yet implemented (well-shaped TODO):
  - parse_positions(text)        — extract per-section position rows
                                   (Cash, Fixed Income, Equities, ETFs)
  - parse_summary(text)          — Account Summary block (begin/end value,
                                   per-period deposits/withdrawals/dividends)

Design notes:

PDF text extraction is line-based via pdfplumber.extract_text().
Schwab's tabular layout in the source PDF survives reasonably well
into the line stream because each row starts at the same x and
columns are space-separated when there is no value collision.
Where rows wrap (long descriptions, "IndustryFee$0.16" notes,
continuation symbols), we attach the continuation to the
previously-seen row via heuristics on the leading character set.

The Date column is MM/DD — no year. We parse the statement period
from the first page header to attach the correct year.
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field, asdict
from datetime import date, datetime


# ============================================================
# Statement-period header
# ============================================================

# "February 1-28, 2026" or "January 30-February 5, 2026"
# (Schwab seems to drop spaces around the hyphen sometimes.)
_PERIOD_RE = re.compile(
    r"(?P<m1>January|February|March|April|May|June|July|August|"
    r"September|October|November|December)\s*"
    r"(?P<d1>\d{1,2})\s*[\-–]\s*"
    r"(?:(?P<m2>January|February|March|April|May|June|July|August|"
    r"September|October|November|December)\s*)?"
    r"(?P<d2>\d{1,2}),\s*"
    r"(?P<year>\d{4})"
)

_MONTH_NUMS = {
    "January": 1, "February": 2, "March": 3, "April": 4,
    "May": 5, "June": 6, "July": 7, "August": 8,
    "September": 9, "October": 10, "November": 11, "December": 12,
}


def parse_statement_period(text: str) -> tuple[date, date] | None:
    """Return (start_date, end_date) for the first period header
    found in `text` (the entire PDF text or page 1 will do)."""
    m = _PERIOD_RE.search(text)
    if not m:
        return None
    year = int(m.group("year"))
    m1 = _MONTH_NUMS[m.group("m1")]
    m2 = _MONTH_NUMS[m.group("m2") or m.group("m1")]
    d1 = int(m.group("d1"))
    d2 = int(m.group("d2"))
    return date(year, m1, d1), date(year, m2, d2)


# ============================================================
# Transaction Details rows
# ============================================================

@dataclass
class TransactionRow:
    """One row from a "Transaction Details" table.

    Fields default to None when the column was blank in the
    source row. `raw_lines` preserves the source text so the
    loader can diff against a future re-parse and detect
    parser drift.
    """
    date: date | None = None
    category: str | None = None   # Sale, Purchase, Withdrawal, Deposit, Dividend, Interest, ...
    action: str | None = None     # MoneyLinkTxn / CashDividend / NRATax / FundsPaid / FundsReceived / ...
    symbol: str | None = None     # Ticker or CUSIP
    description: str = ""         # Wraps over the next line(s) when long
    quantity: float | None = None
    price: float | None = None
    charges: float | None = None  # Industry fee / commission / accrued interest
    amount: float | None = None
    realized_gain_loss: float | None = None
    term: str | None = None       # "ST" or "LT" when present
    raw_lines: list[str] = field(default_factory=list)

    def to_dict(self) -> dict:
        d = asdict(self)
        if self.date is not None:
            d["date"] = self.date.isoformat()
        return d


_TX_DETAILS_HEADER_RE = re.compile(
    r"^\s*Transaction\s*Details\b", re.IGNORECASE,
)
_TX_END_RE = re.compile(
    r"^\s*(TotalTransactions|Pending\s*/\s*Open\s*Activity|Endnotes)\b",
    re.IGNORECASE,
)

# A row's leading token is either MM/DD (a new date), a category
# keyword (same date as the previous row), or "(continued)" /
# noise. We use a wide category whitelist rather than a closed
# set, since Schwab adds new ones occasionally.
_DATE_RE = re.compile(r"^(\d{2}/\d{2})\b")
_CATEGORY_KEYWORDS = (
    "Sale", "Purchase", "Withdrawal", "Deposit",
    "Dividend", "Interest", "Reinvest", "Reinvestment",
    "Journal", "Transfer", "Fee", "Tax", "Adjustment",
    "Redemption", "Exchange", "Distribution", "Split",
    "Merger", "Spin-Off",
)
_CAT_RE = re.compile(r"^(?P<cat>" + "|".join(_CATEGORY_KEYWORDS) + r")\b")

# Numeric fields. We accept comma-grouped numbers, optional
# parens for negatives ((1,234.56)), optional sign, and an
# optional trailing ",(ST)" / "(LT)" tag for the realised
# gain/loss column.
_NUM_RE = re.compile(
    r"\(?-?[\d,]+\.\d+\)?"
)
_REALIZED_RE = re.compile(
    r"(?P<num>\(?-?[\d,]+\.\d+\)?)\s*,?\s*\((?P<term>ST|LT)\)"
)

# Tickers are 1-5 uppercase letters, occasionally with a dot
# (e.g. BRK.B) or slash; CUSIPs are 9 alphanumerics ending in a
# digit. We accept either.
_TICKER_RE = re.compile(r"^[A-Z][A-Z0-9./-]{0,8}$")
_CUSIP_RE = re.compile(r"^[A-Z0-9]{8}\d$")


def _parse_number(s: str) -> float | None:
    if s is None:
        return None
    s = s.strip()
    if not s:
        return None
    neg = s.startswith("(") and s.endswith(")")
    if neg:
        s = s[1:-1]
    s = s.replace(",", "")
    try:
        v = float(s)
    except ValueError:
        return None
    return -v if neg else v


def _line_starts_new_row(line: str) -> bool:
    """True if `line` starts with either a date or a known
    category keyword (i.e. it's a row header, not a continuation
    of the previous row)."""
    if _DATE_RE.match(line):
        return True
    if _CAT_RE.match(line):
        return True
    return False


def _looks_like_continuation(line: str) -> bool:
    """True for lines we should merge into the previous row.
    These are things like 'ETF', 'IndustryFee$0.16', the second
    line of a wrapping description ('NOTE DUE12/31/99'), etc."""
    if not line:
        return False
    if _line_starts_new_row(line):
        return False
    # Common-suffix line types Schwab emits on row two:
    if line.startswith(("IndustryFee", "Commission", "AccruedInterest",
                        "NOTE", "DUE", "ETF", "BETF", "RATEBETF", "(continued)")):
        return True
    # Heuristic fallback: lines that are short and ALL CAPS /
    # symbol-only are usually continuation tokens.
    if len(line) < 40 and line.upper() == line and not any(c.isdigit() for c in line):
        return True
    return False


def _extract_realized(text: str) -> tuple[str, float | None, str | None]:
    """Pull off a trailing ",(ST)" / "(LT)"-tagged number, if any.
    Returns (text_without_realized, realized_amount, term)."""
    m = _REALIZED_RE.search(text)
    if not m:
        return text, None, None
    realized = _parse_number(m.group("num"))
    term = m.group("term")
    text = text[: m.start()].rstrip().rstrip(",").rstrip()
    return text, realized, term


def _parse_row_header(line: str, current_date: date | None,
                      stmt_year: int) -> TransactionRow:
    """Parse the first line of a transaction row.

    The line either starts with MM/DD (new date) or with a
    Category keyword (continuation of the previous date).
    """
    row = TransactionRow()
    rest = line
    m_date = _DATE_RE.match(rest)
    if m_date:
        mm, dd = m_date.group(1).split("/")
        row.date = date(stmt_year, int(mm), int(dd))
        rest = rest[m_date.end():].lstrip()
    else:
        row.date = current_date

    m_cat = _CAT_RE.match(rest)
    if not m_cat:
        # Couldn't identify a category — store raw and return.
        row.description = rest
        return row
    row.category = m_cat.group("cat")
    rest = rest[m_cat.end():].lstrip()

    # Peel off the trailing realised-gain tag if any.
    rest, realized, term = _extract_realized(rest)
    if realized is not None:
        row.realized_gain_loss = realized
        row.term = term

    # Collect trailing numbers; how many depends on category.
    # The columns (in order after Description) are:
    #   Quantity | Price/Rate | Charges/Interest | Amount
    # Some categories (Withdrawal/Deposit) only have Amount.
    # Strategy: pull all trailing numeric tokens off the end and
    # interpret based on count.
    tokens = rest.rsplit()
    trailing_nums = []
    cut = len(tokens)
    while cut > 0 and _NUM_RE.fullmatch(tokens[cut - 1]):
        trailing_nums.append(_parse_number(tokens[cut - 1]))
        cut -= 1
    trailing_nums.reverse()
    leading_tokens = tokens[:cut]

    # Interpret trailing numbers:
    #   4 numbers → quantity, price, charges, amount
    #   3 numbers → quantity, price, amount   (no charge column)
    #   2 numbers → price, amount             (rare — e.g. dividend with rate)
    #   1 number  → amount                    (Withdrawal/Deposit/Dividend)
    if len(trailing_nums) >= 4:
        row.quantity, row.price, row.charges, row.amount = trailing_nums[-4:]
    elif len(trailing_nums) == 3:
        row.quantity, row.price, row.amount = trailing_nums
    elif len(trailing_nums) == 2:
        row.price, row.amount = trailing_nums
    elif len(trailing_nums) == 1:
        row.amount = trailing_nums[0]

    # First leading token is the action subtype OR the symbol.
    # Heuristic: if it looks like a ticker / CUSIP, it's the
    # symbol; otherwise it's an action string (MoneyLinkTxn,
    # CashDividend, NRATax, FundsPaid, FundsReceived, etc.).
    if leading_tokens:
        first = leading_tokens[0]
        if _TICKER_RE.match(first) or _CUSIP_RE.match(first):
            row.symbol = first
            row.description = " ".join(leading_tokens[1:])
        else:
            row.action = first
            # Some rows have action AND symbol (rare); if the
            # second token also looks like a ticker, treat it
            # as the symbol.
            if len(leading_tokens) > 1 and (
                _TICKER_RE.match(leading_tokens[1])
                or _CUSIP_RE.match(leading_tokens[1])
            ):
                row.symbol = leading_tokens[1]
                row.description = " ".join(leading_tokens[2:])
            else:
                row.description = " ".join(leading_tokens[1:])
    return row


def parse_transactions(text: str, statement_year: int | None = None) -> list[TransactionRow]:
    """Extract the "Transaction Details" rows from a statement's
    full-text. Caller can pass a `statement_year` override; if
    None we read it from the period header in `text`.

    Returns a list of TransactionRow.
    """
    if statement_year is None:
        period = parse_statement_period(text)
        if period is None:
            raise ValueError(
                "could not find statement period header; pass "
                "statement_year explicitly"
            )
        statement_year = period[1].year  # use end-of-period year

    lines = [ln.rstrip() for ln in text.split("\n")]
    rows: list[TransactionRow] = []
    in_section = False
    current_row: TransactionRow | None = None
    current_date: date | None = None
    # Lines we always skip inside the section.
    skip_substrings = (
        "Transaction Details",
        "(continued)",
        # Column-header lines:
        "Symbol/", "Date Category Action", "Price/Rate",
    )

    for raw in lines:
        line = raw.strip()
        if not in_section:
            if _TX_DETAILS_HEADER_RE.search(line):
                in_section = True
            continue
        if _TX_END_RE.search(line):
            if current_row is not None:
                rows.append(current_row)
                current_row = None
            in_section = False
            continue
        if any(s in line for s in skip_substrings):
            continue
        if not line:
            continue
        if _line_starts_new_row(line):
            if current_row is not None:
                rows.append(current_row)
            current_row = _parse_row_header(line, current_date, statement_year)
            if current_row.date is not None:
                current_date = current_row.date
            current_row.raw_lines.append(raw)
        else:
            if current_row is None:
                # Stray line — skip.
                continue
            current_row.raw_lines.append(raw)
            if _looks_like_continuation(line):
                # Attach to description, with a single-space joiner
                # and a hint about the line type (we keep raw_lines
                # for full fidelity).
                if current_row.description:
                    current_row.description = (
                        current_row.description + " " + line
                    )
                else:
                    current_row.description = line
    if current_row is not None:
        rows.append(current_row)
    return rows


# ============================================================
# Convenience: open + parse a file path
# ============================================================

def parse_statement_pdf(path, statement_year: int | None = None) -> dict:
    """Open a Schwab brokerage statement PDF and return a dict
    with the statement period and the extracted transactions.

    `statement_year` is the fallback when the period header is
    missing — older quarterly statements (pre-2025) don't render
    a "Period: Month D-DD, YYYY" line the same way and the
    auto-detected period comes back None. The loader passes the
    year extracted from the manifest doc-date so the row parser
    can still resolve MM/DD dates to full timestamps.

    Importing pdfplumber inside this function keeps the module
    importable in environments that don't have pdfplumber
    available (e.g. parser unit tests with hand-crafted text).
    """
    import pdfplumber
    with pdfplumber.open(str(path)) as pdf:
        full_text = "\n".join((p.extract_text() or "") for p in pdf.pages)
    period = parse_statement_period(full_text)
    txs = parse_transactions(full_text, statement_year=statement_year)
    return {
        "path": str(path),
        "period_start": period[0].isoformat() if period else None,
        "period_end": period[1].isoformat() if period else None,
        "transactions": [t.to_dict() for t in txs],
    }


# ============================================================
# CLI for standalone use
# ============================================================

def _main(argv: list[str]) -> int:
    import argparse
    import json
    p = argparse.ArgumentParser(
        description="Extract transactions from one or more Schwab "
                    "brokerage statement PDFs and emit JSON.",
    )
    p.add_argument("pdf", nargs="+", help="One or more PDF paths.")
    p.add_argument(
        "--json-out", default="-",
        help="Output path for the JSON array (default: stdout).",
    )
    args = p.parse_args(argv)

    out = [parse_statement_pdf(pp) for pp in args.pdf]
    blob = json.dumps(out, indent=2, ensure_ascii=False, default=str)
    if args.json_out == "-":
        print(blob)
    else:
        with open(args.json_out, "w", encoding="utf-8") as fh:
            fh.write(blob)
    return 0


if __name__ == "__main__":
    import sys
    sys.exit(_main(sys.argv[1:]))
