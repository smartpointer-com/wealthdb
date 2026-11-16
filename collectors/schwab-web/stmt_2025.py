"""The 2025+ layout of a Schwab brokerage statement.

Activity sits in one "Transaction Details" section, holdings in
"Positions - <Section>" blocks, and the cash flow in one
"Transactions - Summary" line.
"""

from __future__ import annotations

import re
from datetime import date

from numparse import parse_amount
from stmt_tokens import (
    TransactionRow,
    _CUSIP_RE,
    _NUM_RE,
    _TICKER_RE,
    _as_num,
    _parse_number,
    _peel_position_columns,
    _row_date,
    _strip_margin_marker,
)


# ============================================================
# Transaction Details rows
# ============================================================

_TX_DETAILS_HEADER_RE = re.compile(
    r"^\s*Transaction\s*Details\b", re.IGNORECASE,
)
_TX_END_RE = re.compile(
    # Match in either the tight form ("TotalTransactions",
    # "Pending/Open Activity") or the spaced form ("Total
    # Transactions", "Pending / Open Activity"). The whitespace
    # tolerance matters: without it, custodial-account statements
    # (which don't emit a Pending block and only show "Total
    # Transactions") would leave the section open forever and the
    # parser would sweep the trailing Endnotes/disclosure paragraphs
    # — including the page-header repeats with the custodian
    # name and account number — into the last row's description.
    r"^\s*(?:"
    r"Total\s*Transactions"
    r"|Pending\s*/?\s*Open\s*Activity"
    r"|Pending\s+Corporate\s+Actions"
    r"|Endnotes"
    r"|Terms\s+and\s+Conditions"
    r")\b",
    re.IGNORECASE,
)

# A row's leading token is either MM/DD (a new date), a category
# keyword (same date as the previous row), or "(continued)" /
# noise. The category whitelist is wide rather than a closed
# set, since Schwab adds new ones occasionally.
#
# A row's date is printed without a year. A line that opens with a
# full MM/DD/YY date is a description continuation instead — an
# option's expiry wrapped onto a line of its own — and not a row.
_DATE_RE = re.compile(r"^(\d{2}/\d{2})(?!/\d)\b")
_CATEGORY_KEYWORDS = (
    "Sale", "Purchase", "Withdrawal", "Deposit",
    "Dividend", "Interest", "Reinvest", "Reinvestment",
    "Journal", "Transfer", "Fee", "Tax", "Adjustment",
    "Redemption", "Exchange", "Distribution", "Split",
    "Merger", "Spin-Off",
)
_CAT_RE = re.compile(r"^(?P<cat>" + "|".join(_CATEGORY_KEYWORDS) + r")\b")
# The realised gain/loss column: a number tagged ",(ST)" or "(LT)".
_REALIZED_RE = re.compile(
    r"(?P<num>\(?-?[\d,]+\.\d+\)?)\s*,?\s*\((?P<term>ST|LT)\)"
)


def _line_starts_new_row(line: str) -> bool:
    """True if `line` starts with either a date or a known
    category keyword (i.e. it's a row header, not a continuation
    of the previous row)."""
    if _DATE_RE.match(line):
        return True
    if _CAT_RE.match(line):
        return True
    return False


def _extract_realized(text: str) -> tuple[str, float | None, str | None]:
    """Pull off the ",(ST)" / ",(LT)"-tagged realised-gain
    column, if any. Returns
        (text_with_marker_removed, realized_amount, term)
    where the returned text keeps everything that was BEFORE
    AND AFTER the match (the latter matters for block-joined
    input where a description continuation like "NOTE DUE12/31/99"
    follows the realised-gain marker)."""
    m = _REALIZED_RE.search(text)
    if not m:
        return text, None, None
    realized = _parse_number(m.group("num"))
    term = m.group("term")
    before = text[: m.start()].rstrip().rstrip(",").rstrip()
    after = text[m.end():].lstrip()
    rest = (before + " " + after).strip() if after else before
    return rest, realized, term


def _parse_transactions_new(text: str, statement_year: int,
                            period_end: date | None = None) -> list[TransactionRow]:
    """2025+ parser — single "Transaction Details" section.

    Block-based: each "logical row" is the run of lines from one
    row-start (a date or category-keyword line) to the next.
    A row can sit on one line or span 3-5 (header line,
    description continuation, numeric columns, "(ST)" / "(LT)"
    realised-gain tag, "Industry Fee $X.XX" charge note). The
    block parser handles both shapes by joining the block lines
    before extracting fields.
    """
    lines = [ln.rstrip() for ln in text.split("\n")]
    rows: list[TransactionRow] = []
    in_section = False
    current_date: date | None = None
    block: list[str] = []
    skip_substrings = (
        "Transaction Details",
        "(continued)",
        "Symbol/", "Date Category Action", "Price/Rate",
    )

    def _flush():
        nonlocal block
        if not block:
            return
        row = _parse_new_tx_block(block, statement_year, current_date, period_end)
        if row is not None and row.amount is not None:
            rows.append(row)
        block = []

    for raw in lines:
        line = raw.strip()
        if not in_section:
            if _TX_DETAILS_HEADER_RE.search(line):
                in_section = True
            continue
        if _TX_END_RE.search(line):
            _flush()
            in_section = False
            continue
        if any(s in line for s in skip_substrings):
            continue
        if not line:
            continue
        # Soft boundary: page-header repeats, mid-section
        # disclosure paragraphs, sibling-section starts. Flush
        # the current block so its description stops absorbing
        # surrounding page chrome (which carries the account
        # holder's name + account number on every page) — same
        # defense as the legacy parser. Stay in_section in case
        # more data rows follow on a later page.
        if _NEW_TX_ROW_STOP_RE.match(line):
            _flush()
            continue
        if _line_starts_new_row(line):
            _flush()
            block = [line]
            # Track the most recent date so category-only rows
            # (which come after a dated row on the same day)
            # can inherit it.
            m_date = _DATE_RE.match(line)
            if m_date:
                mm, dd = m_date.group(1).split("/")
                try:
                    current_date = _row_date(int(mm), int(dd), statement_year, period_end)
                except ValueError:
                    pass
        elif block and len(" ".join(block)) < _NEW_TX_BLOCK_MAX_CHARS:
            block.append(line)
        # else: stray pre-row chrome or block already at cap; skip.
    _flush()
    return rows


# Page-chrome / sibling-section markers that flush the current
# block in the 2025+ parser. Same idea as _LEGACY_TX_ROW_STOP_RE
# but tuned for the new layout (no "Bank Sweep:" / "Investment
# Detail" / etc. — those don't appear in 2025+).
_NEW_TX_ROW_STOP_RE = re.compile(
    r"^(?:"
    r"\d+\s+of\s+\d+\s*$"           # page footer "6 of 8"
    r"|Statement\s+Period\b"
    r"|Account\s+Number\b"
    r"|Schwab\s+One\b"              # page-header repeat
    r"|Education\s+Savings\b"       # ESA page-header repeat
    r"|Contributory\s+IRA\b"        # IRA page-header repeat
    r"|Roth\s+IRA\b"
    r"|Traditional\s+IRA\b"
    r"|Inherited\s+IRA\b"
    r"|Designated\s+Bene\b"
    r"|Charles\s+Schwab\b"
    r"|Please\s+see\b"
    r"|For\s+(?:the\s+)?Schwab\b"   # disclosure paragraph
    r"|Terms\s+and\s+Conditions\b"
    r"|Endnotes\b"
    r")"
    # Catch the custodial-header line by content marker.
    r"|.*\b(?:FBO|CUST\s+FOR|U?[A-Z]{2}U[TG]MA|U[TG]MA)\b",
    re.IGNORECASE,
)
# Cap on the block's joined-line length before _parse_new_tx_block
# is called. Same belt-and-suspenders logic as the legacy parser:
# a future Schwab layout quirk can't smuggle a page of chrome
# through if the block can't grow past this many characters.
_NEW_TX_BLOCK_MAX_CHARS = 400


# Per-row charge/fee notes, on their own line or glued to the
# previous line with no whitespace. Either way they must not be
# confused with the trailing numeric columns of the data row. Two
# notes on one
# line are separated by "; " ("Commission $0.65; Industry Fee
# $0.01"); the separator goes with the note, or it would stick to
# the row's amount and hide it from the numeric columns.
_NEW_TX_NOISE_RE = re.compile(
    r"\s*(?:Industry\s+Fee|Commission|Accrued\s+Interest)"
    r"\s*\$?\(?[\d,.]+\)?;?",
    re.IGNORECASE,
)


def _parse_new_tx_block(block_lines: list[str],
                         statement_year: int,
                         fallback_date: date | None,
                         period_end: date | None = None) -> TransactionRow | None:
    """Parse one transaction block (one logical row) into a
    TransactionRow. Returns None on unparseable input.

    A block always starts with a line beginning with either an
    MM/DD date or a category keyword (Sale, Purchase, …).
    Continuation lines append description / numerics / charge
    notes.
    """
    if not block_lines:
        return None
    # Join into one logical row, then strip out fee notes —
    # they're already in the dedicated `charges` column on the
    # data line, so leaving them in would double-count.
    joined = " ".join(block_lines)
    joined = _NEW_TX_NOISE_RE.sub("", joined).strip()
    if not joined:
        return None

    row = TransactionRow()
    row.raw_lines = list(block_lines)
    rest = joined

    # Leading MM/DD date (optional — category-only rows inherit
    # the previous block's date).
    m_date = _DATE_RE.match(rest)
    if m_date:
        try:
            mm, dd = m_date.group(1).split("/")
            row.date = _row_date(int(mm), int(dd), statement_year, period_end)
        except ValueError:
            row.date = fallback_date
        rest = rest[m_date.end():].lstrip()
    else:
        row.date = fallback_date

    # Leading category keyword.
    m_cat = _CAT_RE.match(rest)
    if m_cat:
        row.category = m_cat.group("cat")
        rest = rest[m_cat.end():].lstrip()

    # Trailing realised-gain ",(ST)" / ",(LT)" tag (the realized
    # column on Sale rows). _extract_realized handles optional
    # whitespace around the comma — necessary because pypdfium2
    # often splits "227.00," and "(ST)" onto separate lines.
    rest, realized, term = _extract_realized(rest)
    if realized is not None:
        row.realized_gain_loss = realized
        row.term = term

    # Trailing numeric columns. Allow a run of non-numeric
    # tokens AFTER the numeric run (description continuations
    # that wrap below the data row in the source PDF, e.g.
    # "NOTE DUE12/31/99" on a Treasury bond sale row, or "ETF"
    # / "MARKET ETF" on equity rows). Those land in `tail`.
    tokens = rest.split()
    trailing_nums: list[float | None] = []
    tail_tokens: list[str] = []
    in_num_run = False
    i = len(tokens) - 1
    while i >= 0:
        tok = tokens[i]
        if _NUM_RE.fullmatch(tok):
            trailing_nums.append(_parse_number(tok))
            in_num_run = True
        elif in_num_run:
            # First non-num token after the numeric run — that's
            # the boundary between description (above) and
            # numbers (below).
            break
        else:
            tail_tokens.append(tok)
        i -= 1
    trailing_nums.reverse()
    tail_tokens.reverse()
    leading_tokens = tokens[:i + 1]

    if len(trailing_nums) >= 4:
        row.quantity, row.price, row.charges, row.amount = trailing_nums[-4:]
    elif len(trailing_nums) == 3:
        row.quantity, row.price, row.amount = trailing_nums
    elif len(trailing_nums) == 2:
        row.price, row.amount = trailing_nums
    elif len(trailing_nums) == 1:
        row.amount = trailing_nums[0]

    # First leading token is either the symbol (ticker / CUSIP)
    # or an action subtype (MoneyLinkTxn / CashDividend / NRATax
    # / FundsPaid / FundsReceived / …); if it's an action AND
    # the second token is a ticker, both are present.
    desc_main = ""
    if leading_tokens:
        first = leading_tokens[0]
        if _TICKER_RE.match(first) or _CUSIP_RE.match(first):
            row.symbol = first
            desc_main = " ".join(leading_tokens[1:])
        else:
            row.action = first
            if len(leading_tokens) > 1 and (
                _TICKER_RE.match(leading_tokens[1])
                or _CUSIP_RE.match(leading_tokens[1])
            ):
                row.symbol = leading_tokens[1]
                desc_main = " ".join(leading_tokens[2:])
            else:
                desc_main = " ".join(leading_tokens[1:])
    # Description continuations after the numeric run (tail) get
    # appended so callers can still find e.g. "DUE12/31/99" or
    # "MARKET ETF" inside the description.
    if tail_tokens:
        tail_text = " ".join(tail_tokens).strip()
        row.description = (desc_main + " " + tail_text).strip() if desc_main else tail_text
    else:
        row.description = desc_main
    return row


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
# values) — it is explicitly EXCLUDED from position parsing
# because it's not per-instrument data.

_POSITIONS_HEADER_RE = re.compile(
    r"^Positions\s*-\s*(?P<section>[A-Za-z][\w &/+\-]*)\s*$",
)
# Footer terminator: "TotalEquities", "Total Equities",
# "TotalExchangeTradedFunds", "Total Exchange Traded Funds", etc.
# Both the squashed form (no spaces between Total and the
# section) and the spaced form are accepted. The first total is parenthesised when it is negative
# ("Total Options ($1,000.00)", a short option), and Fixed Income
# prints a quantity total before it ("Total Fixed Income
# 10,000.0000 $9,900.00"). A footer that fails to match leaves the
# section open, and the lines after it parse as holdings.
_POSITIONS_FOOTER_RE = re.compile(
    r"^Total\s*[A-Z][\w &/+\-]*?\s*(?:[\d,]+\.\d+\s+)?\(?\$")

# An option holding's lines (2025+ layout), as pypdfium2 wraps them:
#   XMPL                  underlying, alone on its line
#   01/16/20              expiry, its year split across two lines
#   26 50.00              rest of the year, strike
#   C                     call or put
#   CALL EXAMPLE CORP     description
#   ,
#   $50 EXP 01/16/26
#   1.0000 2.50000 250.00 200.00 50.00 <1%
# A short holding prints its quantity on a line of its own, followed
# by the "S" endnote, before the remaining columns:
#   (1.0000)
#   S
#   2.50000 (250.00) (300.00) 50.00
# A row starts at an underlying alone on its line whose next line
# opens with the expiry; the description word ("CALL") and the
# endnote ("S") fit the ticker shape too, and must not.
_OPTION_EXPIRY_LINE_RE = re.compile(r"^\d{2}/\d{2}/\d{2}")
# The contract, read off the start of the block: underlying, expiry
# (its year possibly split), strike, call or put. The key matches
# the one the transactions parser gives the contract's trades.
_OPTION_CONTRACT_RE = re.compile(
    r"^(?P<under>\S+)\s+(?P<md>\d{2}/\d{2}/)(?P<c>\d{2})\s*(?P<y>\d{2})"
    r"\s+(?P<strike>[\d,]*\.\d+)\s*(?P<cp>[CP])\b")

# Sections whose holdings wrap their columns over several lines, so
# the columns of one holding are gathered from the lines around the
# first line that carries three or more of them.
_WRAPPED_COLUMN_SECTIONS = frozenset({"Options", "Fixed Income"})

# Per-row leading token must look like a ticker or CUSIP, by
# _TICKER_RE / _CUSIP_RE from the transactions parser.
# Note: section headings sometimes carry a parenthesised marker
# like "(M)" right after the ticker in row body — that's handled
# at parse-time, not via the regex.

# All-caps ADR description-continuation words that happen to fit the
# ticker shape (<= 9 chars) and so must NOT be mistaken for a new
# position-row header. "UNSPONSORED" is 11 chars and already fails
# _TICKER_RE, but is listed for clarity / robustness.
_POSITIONS_CONT_WORDS = frozenset({"SPONSORED", "UNSPONSORED"})


def _parse_positions_new(text: str) -> list[dict]:
    """2025+ parser — "Positions - <Section>" anchors.

    Layout robustness: pypdfium2 preserves the source PDF's
    glyph layout and tends to split each position row across
    THREE lines for Schwab statements ("TICKER COMPANY NAME" /
    "(M)" / "100.0000 50.0 5000 ..."). A line-at-a-time parser
    would mis-segment those, so consecutive lines accumulate
    into a per-row buffer instead; flushing happens whenever the
    next line starts with a fresh ticker (or at a section
    footer / end of input).
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

    for idx, raw in enumerate(lines):
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
        # chars present in token[0]), and most continuation lines
        # ("(M)", "1,234.56 ...") don't either. The exception is an
        # ADR description line "SPONSORED ADR" — "SPONSORED" is nine
        # uppercase chars and so matches _TICKER_RE; without the
        # deny-set below it would split the ADR's block and steal
        # the real ticker's numbers (dropping the ADR itself and
        # emitting a spurious "SPONSORED" holding).
        starts_row = (
            (_TICKER_RE.match(tokens[0]) or _CUSIP_RE.match(tokens[0]))
            and tokens[0] not in _POSITIONS_CONT_WORDS)
        if starts_row and section == "Options":
            nxt = next((ln.strip() for ln in lines[idx + 1:] if ln.strip()), "")
            starts_row = (len(tokens) == 1
                          and bool(_OPTION_EXPIRY_LINE_RE.match(nxt)))
        if starts_row:
            _flush()
            buffer = [line]
            raw_buffer = [raw]
        elif buffer:
            buffer.append(line)
            raw_buffer.append(raw)
        # else: pre-row chrome before the first ticker; skip.
    _flush()
    return rows


def _parse_position_block(block_lines: list[str], section: str) -> dict | None:
    """Parse a multi-line position block (typically 1-3 lines)
    into a position row dict.

    A block is the run of lines from one ticker to the next.
    Within it exactly one line carries the numeric columns
    (>= 3 trailing-col tokens) — that's the "numbers
    line". Everything else is description (free-text tokens
    that may appear before OR after the numbers line, depending
    on whether the source PDF wrapped the description). Cheap
    to identify by scanning trailing-col tokens; cleaner than
    naive string-join because it doesn't collapse description-
    after-numbers continuations into the trailing-column area.
    """
    if not block_lines:
        return None
    # Find the numbers line (footnote-marker-tolerant peel).
    numbers_idx = -1
    trailing_tokens: list[str] = []
    footnotes: list[str] = []
    numbers_consumed = 0
    for i, line in enumerate(block_lines):
        cols, consumed, foot = _peel_position_columns(line.split())
        if len(cols) >= 3:
            numbers_idx = i
            trailing_tokens = cols
            footnotes = foot
            numbers_consumed = consumed
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

    if section in _WRAPPED_COLUMN_SECTIONS:
        return _parse_wrapped_position_block(block_lines, section, numbers_idx)

    # Gather description tokens from every line, excluding the
    # ticker itself and excluding the trailing numeric tokens of
    # the numbers line.
    desc_parts: list[str] = []
    for i, line in enumerate(block_lines):
        toks = line.split()
        if not toks:
            continue
        start = 1 if i == 0 else 0
        end = (len(toks) - numbers_consumed) if i == numbers_idx else len(toks)
        desc_parts.extend(toks[start:end])
    description = " ".join(desc_parts)
    description = _strip_margin_marker(description)

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
        "footnotes": footnotes or None,
        "raw_lines": [],
    }


def _is_column_line(line: str) -> bool:
    """True if every token of `line` is a position column or an
    endnote marker between columns, so the line continues a
    holding's columns rather than its description."""
    toks = line.split()
    _, consumed, _ = _peel_position_columns(toks)
    return bool(toks) and consumed == len(toks)


def _description(tokens: list[str]) -> str:
    """A wrapped holding's description: its tokens without the margin
    marker and the lone commas the wrap leaves behind."""
    return " ".join(
        t for t in _strip_margin_marker(" ".join(tokens)).split() if t != ",")


def _parse_wrapped_position_block(
    block_lines: list[str], section: str, numbers_idx: int,
) -> dict:
    """Parse an Options or Fixed Income holding, whose columns wrap.

    The columns are the numbers line plus the column-only lines after
    it, where a bond's original cost basis, unrealized gain, yield,
    income and accrued interest wrap. Peeling the joined lines from
    the right also takes in the column-only lines before it, where a
    short option's quantity and its "S" endnote sit; the peel stops at
    the description, whose last token is a date (an option's expiry,
    a bond's maturity)."""
    hi = numbers_idx + 1
    while hi < len(block_lines) and _is_column_line(block_lines[hi]):
        hi += 1
    tokens = " ".join(block_lines[:hi]).split()
    cols, consumed, footnotes = _peel_position_columns(tokens)
    head = tokens[: len(tokens) - consumed]

    def col(i: int) -> float | None:
        return _as_num(cols[i]) if i < len(cols) else None

    pct = cols[-1] if cols and (cols[-1].endswith("%") or cols[-1] == "<1%") else None
    row = {
        "instrument_key": head[0],
        "description": _description(head[1:]),
        "quantity": col(0),
        "market_price": col(1),
        "market_value": col(2),
        "cost_basis": col(3),
        "unrealized_gain_loss": col(4),
        "accrued_interest": None,
        "est_yield": None,
        "est_annual_income": None,
        "pct_of_acct": None,
        "section": section,
        "footnotes": footnotes or None,
        "raw_lines": [],
    }
    if section == "Options":
        # quantity, price, market value, cost basis, unrealized gain,
        # % of account. Options carry no yield and no income.
        m = _OPTION_CONTRACT_RE.match(" ".join(head))
        if m:
            row["instrument_key"] = (
                f"{m['under']} {m['md']}{m['c']}{m['y']} {m['strike']} {m['cp']}")
            row["description"] = _description(" ".join(head)[m.end():].split())
        row["pct_of_acct"] = pct if len(cols) > 5 else None
    else:
        # quantity/par, price, market value, adjusted cost basis,
        # original cost basis, unrealized gain, yield to maturity,
        # annual income, accrued interest, % of account.
        row["unrealized_gain_loss"] = col(5)
        row["est_yield"] = cols[6] if len(cols) > 6 else None
        row["est_annual_income"] = col(7)
        row["accrued_interest"] = col(8)
        row["pct_of_acct"] = pct if len(cols) > 9 else None
    return row


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


def _parse_cash_summary_new(text: str) -> dict | None:
    """2025+ parser — single-line "Transactions - Summary" block."""
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
        # rows. Capture stops at "Transaction Details", at a blank
        # line after the first captured one, or after five lines.
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

    # Map by position; missing columns leave NULL. Cash cells carry a
    # leading "$" and parenthesised negatives (e.g. "($1,234.56)").
    def _at(i):
        return (
            parse_amount(nums_text[i], dollar=True)
            if i < len(nums_text)
            else None
        )

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
                other_activity = parse_amount(ms[0], dollar=True)
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
