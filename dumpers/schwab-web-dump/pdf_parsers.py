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
  - parse_positions(text)        — extracts per-section position rows
                                   ("Positions - Equities", "Positions -
                                   Exchange Traded Funds", etc.)
  - parse_cash_summary(text)     — extracts the "Transactions -
                                   Summary" cash-flow block
                                   (BeginningCash → EndingCash with the
                                   seven inflow/outflow subtotals)

Design notes:

PDF text extraction goes through pypdfium2 (Python bindings to
Google's PDFium, the renderer Chrome uses). pdfplumber/pdfminer
worked but were CPU-bound on pure-Python loops — pypdfium2 is
roughly 5-10x faster on Schwab statements because it pushes the
text-layout work into PDFium's C++ core.

Schwab's tabular layout in the source PDF survives reasonably
well into the line stream because each row starts at the same x
and columns are space-separated when there is no value collision.
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
# Position snapshots
# ============================================================
#
# Schwab statements list holdings in one block per asset class:
#   "Positions - Equities"
#   "Positions - Exchange Traded Funds"
#   "Positions - Mutual Funds"
#   "Positions - Fixed Income"
#   …
# Each block opens with a header row of column labels and ends
# with a "Total<Section>" footer line. The body rows are
# space-separated columns:
#   Symbol Description Quantity Price MarketValue CostBasis
#   UnrealizedGain EstYield EstAnnualIncome PctOfAcct
# (with `N/A` placeholders for any column the row can't fill —
# typically Yield + AnnualIncome on non-income-bearing equities).
#
# A "Positions - Summary" block also exists (one-line roll-up of
# values) — we explicitly EXCLUDE it from position parsing
# because it's not per-instrument data.

_POSITIONS_HEADER_RE = re.compile(
    r"^Positions\s*-\s*(?P<section>[A-Za-z][\w &/+\-]*)\s*$",
)
# Footer terminator: "TotalEquities", "Total Equities",
# "TotalExchangeTradedFunds", "Total Exchange Traded Funds", etc.
# pdfplumber emits the squashed form (no spaces between Total
# and the section); pypdfium2 emits the spaced form. Accept
# both.
_POSITIONS_FOOTER_RE = re.compile(r"^Total\s*[A-Z][\w &/+\-]*\s*\$")

# Per-row leading token must look like a ticker or CUSIP. We
# reuse _TICKER_RE / _CUSIP_RE from the transactions parser.
# Note: section headings sometimes carry a parenthesised marker
# like "(M)" right after the ticker in row body — that's handled
# at parse-time, not via the regex.


def parse_positions(text: str) -> list[dict]:
    """Extract position rows from the "Positions - <Section>"
    blocks in a statement's full-text.

    Returns a list of dicts — see _parse_position_row for the
    keys carried. Cash positions ("Cash and Cash Investments")
    are NOT included (they live in the cash-balances parser),
    and "Positions - Summary" is skipped (it's a one-line
    roll-up, not per-instrument).

    Layout robustness: pypdfium2 preserves the source PDF's
    glyph layout and tends to split each position row across
    THREE lines for Schwab statements ("TICKER COMPANY NAME" /
    "(M)" / "100.0000 50.0 5000 ..."). A line-at-a-time parser
    would mis-segment those. We instead accumulate consecutive
    lines into a per-row buffer; flushing happens whenever the
    next line starts with a fresh ticker (or at a section
    footer / end of input). _parse_position_row then sees the
    full row joined with single spaces — equivalent to what
    pdfplumber's tighter packing gave us as one line.
    """
    lines = [ln.rstrip() for ln in text.split("\n")]
    rows: list[dict] = []
    section: str | None = None
    buffer: list[str] = []
    raw_buffer: list[str] = []

    def _flush():
        nonlocal buffer, raw_buffer
        if not buffer or section is None:
            buffer, raw_buffer = [], []
            return
        parsed = _parse_position_block(buffer, section)
        if parsed is not None:
            parsed["raw_lines"] = list(raw_buffer)
            rows.append(parsed)
        buffer, raw_buffer = [], []

    for raw in lines:
        line = raw.strip()
        m_head = _POSITIONS_HEADER_RE.match(line)
        if m_head:
            _flush()
            sec_name = m_head.group("section").strip()
            section = None if sec_name.lower() == "summary" else sec_name
            continue
        if section is None:
            continue
        if _POSITIONS_FOOTER_RE.match(line):
            _flush()
            section = None
            continue
        if not line:
            continue
        tokens = line.split()
        if not tokens:
            continue
        # A "new row starts here" signal is a leading token that
        # looks like a ticker or CUSIP. Column-header chrome
        # like "Symbol Description ..." doesn't match (lowercase
        # chars present in token[0]), and continuation lines
        # ("(M)", "SPONSORED ADR", "1,234.56 ...") don't either.
        # If the flush of a stray header buffer fails to produce
        # numeric content, _flush silently discards it.
        if _TICKER_RE.match(tokens[0]) or _CUSIP_RE.match(tokens[0]):
            _flush()
            buffer = [line]
            raw_buffer = [raw]
        elif buffer:
            buffer.append(line)
            raw_buffer.append(raw)
        # else: pre-row chrome before the first ticker; skip.
    _flush()
    return rows


def _is_trailing_col_token(s: str) -> bool:
    """True if `s` fits a Positions-row trailing column slot —
    a number, a number-with-%, an integer, or one of the literal
    placeholders Schwab emits when a column is "not applicable"
    or "less than 1%".
    """
    if s in ("N/A", "<1%"):
        return True
    s = s.rstrip(",")
    if s.endswith("%"):
        core = s[:-1]
        if core == "<1":
            return True
        s = core
    if _NUM_RE.fullmatch(s):
        return True
    if re.fullmatch(r"-?\d+", s):
        return True
    return False


def _parse_position_block(block_lines: list[str], section: str) -> dict | None:
    """Parse a multi-line position block (typically 1-3 lines)
    into a position row dict.

    A block is the run of lines from one ticker to the next.
    Within it exactly one line carries the numeric columns
    (>= 3 trailing trailing-col tokens) — that's the "numbers
    line". Everything else is description (free-text tokens
    that may appear before OR after the numbers line, depending
    on whether the source PDF wrapped the description). Cheap
    to identify by scanning trailing-col tokens; cleaner than
    naive string-join because it doesn't collapse description-
    after-numbers continuations into the trailing-column area.
    """
    if not block_lines:
        return None
    # Find the numbers line.
    numbers_idx = -1
    trailing_tokens: list[str] = []
    for i, line in enumerate(block_lines):
        toks = line.split()
        cnt = 0
        for tok in reversed(toks):
            if _is_trailing_col_token(tok):
                cnt += 1
            else:
                break
        if cnt >= 3:
            numbers_idx = i
            trailing_tokens = toks[len(toks) - cnt:]
            break
    if numbers_idx < 0:
        return None

    # Validate the ticker (must be the first token of the first
    # line of the block).
    first_toks = block_lines[0].split()
    if not first_toks:
        return None
    ticker = first_toks[0]
    if not (_TICKER_RE.match(ticker) or _CUSIP_RE.match(ticker)):
        return None

    # Gather description tokens from every line, excluding the
    # ticker itself and excluding the trailing numeric tokens of
    # the numbers line.
    desc_parts: list[str] = []
    for i, line in enumerate(block_lines):
        toks = line.split()
        if not toks:
            continue
        start = 1 if i == 0 else 0
        end = (len(toks) - len(trailing_tokens)) if i == numbers_idx else len(toks)
        desc_parts.extend(toks[start:end])
    description = " ".join(desc_parts)
    description = re.sub(r"\(M\),?", "", description).strip()

    def _as_num(s):
        s = s.rstrip("%").rstrip(",")
        if s in ("N/A", "<1%"):
            return None
        return _parse_number(s)

    quantity = _as_num(trailing_tokens[0]) if len(trailing_tokens) >= 1 else None
    market_price = _as_num(trailing_tokens[1]) if len(trailing_tokens) >= 2 else None
    market_value = _as_num(trailing_tokens[2]) if len(trailing_tokens) >= 3 else None
    cost_basis = _as_num(trailing_tokens[3]) if len(trailing_tokens) >= 4 else None
    unrealized = _as_num(trailing_tokens[4]) if len(trailing_tokens) >= 5 else None
    est_yield = trailing_tokens[5] if len(trailing_tokens) >= 6 else None
    est_annual_income = (
        _as_num(trailing_tokens[6]) if len(trailing_tokens) >= 7 else None
    )
    pct_of_acct = trailing_tokens[-1] if trailing_tokens and (
        trailing_tokens[-1].endswith("%") or trailing_tokens[-1] == "<1%"
    ) else None

    return {
        "instrument_key": ticker,
        "description": description,
        "quantity": quantity,
        "market_price": market_price,
        "market_value": market_value,
        "cost_basis": cost_basis,
        "unrealized_gain_loss": unrealized,
        "accrued_interest": None,
        "est_yield": est_yield,
        "est_annual_income": est_annual_income,
        "pct_of_acct": pct_of_acct,
        "section": section,
        "raw_lines": [],
    }


def _parse_position_row(line: str, section: str) -> dict | None:
    """Parse one row of a Positions block. Returns the row dict
    or None if `line` doesn't look like a position-row header
    (in which case the caller treats it as a description
    continuation of the previous row).
    """
    tokens = line.split()
    if len(tokens) < 4:
        return None
    first = tokens[0]
    # Must be a plausible ticker or CUSIP.
    if not (_TICKER_RE.match(first) or _CUSIP_RE.match(first)):
        return None

    # Schwab annotates margin-eligible equities with a trailing
    # "(M)" or "(M)," glued to the description. Strip the marker
    # but keep the rest of the description intact.
    # Peel trailing-column tokens off the end. A trailing-column
    # token is one of:
    #   - a decimal number, optionally comma-grouped, optionally
    #     parened-negative ("1,234.56", "(5,000.00)");
    #   - an integer ("100", "27");
    #   - any of those with a "%" suffix ("1%", "27%", "0.86%");
    #   - the literal placeholders "N/A" and "<1%".
    trailing_nums = []
    cut = len(tokens)
    while cut > 0 and _is_trailing_col_token(tokens[cut - 1]):
        trailing_nums.append(tokens[cut - 1])
        cut -= 1
    trailing_nums.reverse()

    desc_tokens = tokens[1:cut]
    description = " ".join(desc_tokens)
    # Strip "(M)" / "(M)," margin marker.
    description = re.sub(r"\(M\),?", "", description).strip()
    # Position rows have at most 8 numeric-ish trailing columns:
    #   Quantity, Price, MarketValue, CostBasis, UnrealizedGain,
    #   EstYield, EstAnnualIncome, PctOfAcct
    # but Yield / AnnualIncome / pct can be "N/A" or "<1%". Map
    # by position from the END (more reliable than from the
    # START when descriptions are missing).
    if len(trailing_nums) < 3:
        # Not a position row.
        return None

    def _as_num(s):
        s = s.rstrip("%").rstrip(",")
        if s in ("N/A", "<1%"):
            return None
        return _parse_number(s)

    # Slot trailing_nums right-aligned into the canonical
    # column order. Position rows we've seen carry either:
    #   - all 8 columns (full equity row with yield + income)
    #   - 5 columns: quantity, price, mv, cb, gain (no yield/income, "N/A" stripped)
    #   - others
    # We map by indexing from the start of `trailing_nums`.
    quantity = _as_num(trailing_nums[0]) if len(trailing_nums) >= 1 else None
    market_price = _as_num(trailing_nums[1]) if len(trailing_nums) >= 2 else None
    market_value = _as_num(trailing_nums[2]) if len(trailing_nums) >= 3 else None
    cost_basis = _as_num(trailing_nums[3]) if len(trailing_nums) >= 4 else None
    unrealized = _as_num(trailing_nums[4]) if len(trailing_nums) >= 5 else None
    # Trailing yield / annual_income / pct_of_acct keep their
    # original printed form when they're percent-shaped — the
    # printed form is what users will compare against the source
    # PDF. Numeric annual_income still gets coerced via _as_num.
    est_yield = trailing_nums[5] if len(trailing_nums) >= 6 else None
    est_annual_income = (
        _as_num(trailing_nums[6]) if len(trailing_nums) >= 7 else None
    )
    pct_of_acct = trailing_nums[-1] if trailing_nums and (
        trailing_nums[-1].endswith("%") or trailing_nums[-1] == "<1%"
    ) else None
    # When pct_of_acct is the last token, est_annual_income may
    # actually be at trailing_nums[-2]. The fixed-position mapping
    # above is the common case for full rows; we'll let the
    # payload preserve the raw line so consumers can re-derive.

    return {
        "instrument_key": first,
        "description": description,
        "quantity": quantity,
        "market_price": market_price,
        "market_value": market_value,
        "cost_basis": cost_basis,
        "unrealized_gain_loss": unrealized,
        "accrued_interest": None,
        "est_yield": est_yield,
        "est_annual_income": est_annual_income,
        "pct_of_acct": pct_of_acct,
        "section": section,
        "raw_lines": [],
    }


# ============================================================
# Cash-balance summary
# ============================================================
#
# Schwab puts a single-line cash-flow block at the top of the
# Activity page, labelled "Transactions - Summary". The line
# below the header carries eight $-prefixed numbers:
#
#   BeginningCash + Deposits + Withdrawals + Purchases
#   + Sales/Redemptions + Dividends/Interest + Expenses
#   = EndingCash
#
# Inflows are positive; outflows print as ($...). Withdrawals
# and Purchases are outflows by convention.

_CASH_SUMMARY_HEADER_RE = re.compile(
    r"^\s*Transactions\s*-\s*Summary\b", re.IGNORECASE,
)
# The data line is the first one matching this pattern after
# the header. It's eight $-amounts in a row.
_CASH_DATA_RE = re.compile(
    r"\$\(?[\d,]+\.\d{2}\)?|\(\$[\d,]+\.\d{2}\)"
)


def parse_cash_summary(text: str) -> dict | None:
    """Extract the cash-flow numbers from a statement's
    "Transactions - Summary" block.

    Returns a dict with keys:
        opening_balance, closing_balance,
        deposits, withdrawals, purchases, sales_redemptions,
        dividends_interest, expenses, other_activity,
        total_credits, total_debits, currency_iso, raw_line
    Numeric values may be None when the column is missing from
    the source. total_credits / total_debits are derived
    (Deposits + Sales/Redemptions + Dividends/Interest, and
    abs(Withdrawals + Purchases + Expenses) respectively) —
    if any input is missing the totals stay None.

    Returns None if the section isn't present in `text`.
    """
    lines = text.split("\n")
    in_section = False
    captured: list[str] = []
    for raw in lines:
        line = raw.strip()
        if not in_section:
            if _CASH_SUMMARY_HEADER_RE.search(line):
                in_section = True
            continue
        # Inside the section: capture every line, the data line
        # may be the next non-blank or two-down because Schwab
        # sometimes splits the column-header line across two
        # rows. We give up once we hit "OtherActivity" or
        # "Transaction Details" or an empty stretch.
        if not line:
            if captured:
                break
            continue
        if "Transaction Details" in line:
            break
        captured.append(line)
        if len(captured) >= 5:
            break
    if not captured:
        return None

    # Find the data line — the first one with at least 7 $-amounts.
    data_line: str | None = None
    for cand in captured:
        nums = _CASH_DATA_RE.findall(cand)
        if len(nums) >= 7:
            data_line = cand
            break
    if data_line is None:
        return None

    nums_text = _CASH_DATA_RE.findall(data_line)

    def _clean(s: str) -> float | None:
        # Strip leading $, leading ( and trailing ).
        if s is None:
            return None
        s = s.strip()
        neg = (s.startswith("(") and s.endswith(")")) or (
            s.startswith("($") and s.endswith(")")
        )
        if neg:
            s = s[1:-1]
        s = s.lstrip("$")
        # Stray paren around $ already removed; "$" might still
        # lead if we had "(${num})" form.
        s = s.lstrip("$").replace(",", "")
        try:
            v = float(s)
        except ValueError:
            return None
        return -v if neg else v

    # Map by position; missing columns leave NULL.
    def _at(i):
        return _clean(nums_text[i]) if i < len(nums_text) else None

    opening = _at(0)
    deposits = _at(1)
    withdrawals = _at(2)
    purchases = _at(3)
    sales_redemptions = _at(4)
    dividends_interest = _at(5)
    expenses = _at(6)
    closing = _at(7)

    # Look for an "OtherActivity" line within the captured
    # block (it carries a single $-amount).
    other_activity = None
    for cand in captured:
        if "OtherActivity" in cand or "Other Activity" in cand:
            ms = _CASH_DATA_RE.findall(cand)
            if ms:
                other_activity = _clean(ms[0])
            break

    def _sum_or_none(*vals):
        if any(v is None for v in vals):
            return None
        return sum(vals)

    total_credits = _sum_or_none(deposits, sales_redemptions, dividends_interest)
    total_debits_raw = _sum_or_none(withdrawals, purchases, expenses)
    # Outflows print as negatives; debits convention is
    # positive magnitude.
    total_debits = (
        -total_debits_raw if total_debits_raw is not None else None
    )

    return {
        "opening_balance": opening,
        "closing_balance": closing,
        "deposits": deposits,
        "withdrawals": withdrawals,
        "purchases": purchases,
        "sales_redemptions": sales_redemptions,
        "dividends_interest": dividends_interest,
        "expenses": expenses,
        "other_activity": other_activity,
        "total_credits": total_credits,
        "total_debits": total_debits,
        "currency_iso": "USD",
        "raw_line": data_line,
    }


# ============================================================
# PDF text extraction (pypdfium2)
# ============================================================

def _extract_pdf_text(path) -> str:
    """Open `path` with pypdfium2 and return the concatenated
    text of every page joined with '\\n'.

    pypdfium2 reads the PDF via PDFium's C++ core; the text we
    get back is layout-ordered (top-to-bottom, left-to-right
    within each page) which is what the line-anchored parsers
    expect. Each page's text is taken via PdfPage.get_textpage()
    and PdfTextPage.get_text_range() — the latter returns the
    full text without coordinate filtering.

    Resources are released explicitly (textpage/page/document
    close()) — PDFium handles are C pointers and Python GC isn't
    deterministic enough to rely on across hundreds of PDFs.
    """
    import pypdfium2 as pdfium
    parts: list[str] = []
    pdf = pdfium.PdfDocument(str(path))
    try:
        for i in range(len(pdf)):
            page = pdf[i]
            try:
                textpage = page.get_textpage()
                try:
                    parts.append(textpage.get_text_range() or "")
                finally:
                    textpage.close()
            finally:
                page.close()
    finally:
        pdf.close()
    return "\n".join(parts)


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

    Importing pypdfium2 inside this function keeps the module
    importable in environments that don't have it (e.g. parser
    unit tests with hand-crafted text).
    """
    full_text = _extract_pdf_text(path)
    period = parse_statement_period(full_text)
    txs = parse_transactions(full_text, statement_year=statement_year)
    positions = parse_positions(full_text)
    cash = parse_cash_summary(full_text)
    return {
        "path": str(path),
        "period_start": period[0].isoformat() if period else None,
        "period_end": period[1].isoformat() if period else None,
        "transactions": [t.to_dict() for t in txs],
        "positions": positions,
        "cash_summary": cash,
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
