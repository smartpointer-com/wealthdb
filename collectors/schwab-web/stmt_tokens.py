"""The shapes every layout of a Schwab brokerage statement shares.

Numbers, tickers and CUSIPs; the date of an activity row printed
without its year; the activity row itself; and the trailing columns
of a holding row, with the endnote letters glued among them.
"""

from __future__ import annotations

import re
from dataclasses import asdict, dataclass, field
from datetime import date

from numparse import parse_amount


# ============================================================
# Activity rows
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


# ============================================================
# Numbers, tickers, CUSIPs and the row date
# ============================================================

# Numeric fields: comma-grouped numbers, optional
# parens for negatives ((1,234.56)), optional sign.
_NUM_RE = re.compile(
    r"\(?-?[\d,]+\.\d+\)?"
)

# Tickers are 1-5 uppercase letters, occasionally with a dot
# (e.g. BRK.B) or slash; CUSIPs are 9 alphanumerics ending in a
# digit. Either is accepted.
_TICKER_RE = re.compile(r"^[A-Z][A-Z0-9./-]{0,8}$")
_CUSIP_RE = re.compile(r"^[A-Z0-9]{8}\d$")


def _parse_number(s: str) -> float | None:
    return parse_amount(s)


def _row_date(mm: int, dd: int, statement_year: int,
              period_end: date | None) -> date:
    """The date of a row printed as MM/DD on a statement of
    `statement_year`. A December statement lists the trades of its
    last days with their settlement dates, which fall in January of
    the NEXT year: a date more than half a year before the period end
    belongs to the year after. Raises ValueError on an impossible
    date."""
    d = date(statement_year, mm, dd)
    if period_end is not None and (period_end - d).days > 183:
        d = date(statement_year + 1, mm, dd)
    return d


# ============================================================
# A holding row's trailing columns
# ============================================================

def _strip_margin_marker(desc: str) -> str:
    """Drop the "(M)" / "(M)," margin-eligibility marker Schwab glues
    into a holding's description, returning the trimmed remainder."""
    return re.sub(r"\(M\),?", "", desc).strip()


def _as_num(s: str) -> float | None:
    """Coerce a trailing position-column token to a float, mapping the
    "N/A" / "<1%" placeholders (and a trailing "%" / ",") to None /
    a bare number. Shared by the position-block parsers."""
    s = s.rstrip("%").rstrip(",")
    if s in ("N/A", "<1%"):
        return None
    return _parse_number(s)


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


# Schwab prints Endnote reference letters (single letters, occasionally
# comma-joined) inline among a holding's numeric columns — e.g. an "e"
# ("Data for this holding has been edited or provided by the account
# holder") sitting between Cost Basis and Unrealized Gain on a
# holder-valued position, or an "S"/"t" on an
# option / short / third-party-edited line. The right-to-left column
# scan must step OVER such a marker instead of stopping at it;
# otherwise the columns shift left (quantity reads the unrealized gain,
# market value reads blank). Only a marker that sits BETWEEN
# two column tokens is skipped, so a description ending in a lone
# letter ("… CLASS A") is never consumed.
_POSITION_FOOTNOTE_RE = re.compile(r"[A-Za-z](?:,[A-Za-z]){0,4}")
# A footnote marker glued (no whitespace) to the FRONT of a numeric
# column token — pypdfium2 emits this when the marked column is a
# parenthesised negative, e.g. "t(1,200.00)" (third-party-edited
# unrealized loss) — or of an N/A placeholder ("iN/A"). Group 1 =
# marker letters, group 2 = the column.
_POSITION_FOOTNOTE_GLUED_RE = re.compile(
    r"^([A-Za-z](?:,[A-Za-z]){0,4})(\(?-?[\d,].*|N/A)$")


def _split_glued_footnote(tok: str) -> tuple[str | None, str | None]:
    """If `tok` is a footnote letter glued to a numeric column token,
    return `(column, marker)`; otherwise `(None, None)`."""
    m = _POSITION_FOOTNOTE_GLUED_RE.match(tok)
    if m and _is_trailing_col_token(m.group(2)):
        return m.group(2), m.group(1)
    return None, None


def _peel_position_columns(
    tokens: list[str],
) -> tuple[list[str], int, list[str]]:
    """Right-to-left, collect a holding row's trailing numeric /
    placeholder column tokens, transparently stepping over inline
    Endnote-marker letters (both whitespace-separated, "… 9,000.00 e
    45,000.00 …", and glued to a parenthesised negative, "… t(1,200.00)
    …").

    Returns `(columns, consumed, footnotes)`: `columns` left-to-right in
    source order; `consumed` the number of source tokens taken from the
    end (markers included, so `tokens[: len(tokens) - consumed]` is the
    description); `footnotes` the marker tokens that were skipped."""
    columns: list[str] = []
    footnotes: list[str] = []
    i = len(tokens) - 1
    while i >= 0:
        tok = tokens[i]
        if _is_trailing_col_token(tok):
            columns.append(tok)
            i -= 1
            continue
        # A standalone marker that sits BETWEEN two column tokens (so a
        # description ending in a lone letter, "… CLASS A", is never
        # eaten).
        if (i > 0 and _POSITION_FOOTNOTE_RE.fullmatch(tok.rstrip(","))
                and _is_trailing_col_token(tokens[i - 1])):
            footnotes.append(tok.rstrip(","))
            i -= 1
            continue
        # A marker glued to the front of a numeric column — only within
        # the numeric run (at least one column already collected).
        if columns:
            col, mark = _split_glued_footnote(tok)
            if col is not None:
                columns.append(col)
                footnotes.append(mark)
                i -= 1
                continue
        break
    columns.reverse()
    footnotes.reverse()
    consumed = len(tokens) - 1 - i
    return columns, consumed, footnotes
