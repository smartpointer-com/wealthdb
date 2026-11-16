"""The 2020-2024 layout of a Schwab brokerage statement, and the
activity rows of 2017-2024.

Holdings sit in "Investment Detail - <Section>" blocks with one line
per tax lot, and the cash flow in a "Cash Transactions Summary"
block. Activity sits in "Transaction Detail" sections, which the
2017-2019 layout shares.
"""

from __future__ import annotations

import re
from datetime import date, datetime

from stmt_tokens import (
    TransactionRow,
    _NUM_RE,
    _POSITION_FOOTNOTE_RE,
    _TICKER_RE,
    _as_num,
    _is_trailing_col_token,
    _parse_number,
    _peel_position_columns,
    _row_date,
    _strip_margin_marker,
)


# ============================================================
# Legacy transaction parser (2017-2024 statement layouts)
# ============================================================
#
# Pre-2025 statements split transactions across one or more
# "Transaction Detail" sections, with sub-headers labelling
# the activity type ("Equities Activity", "Cash, Bank Sweep,
# and Money Market Funds Activity", …). Row format:
#
#   <settle-date> <trade-date> <Action+Type> <Description...>
#   [<Quantity>] [<UnitPrice>] [<Charges>] <Amount>
#
# 2017-2019 uses MM/DD dates (no year); 2020-2024 uses MM/DD/YY.
# The same parser handles both because the year is either
# supplied by the caller or inferred from the period header.
#
# Description continuations follow the data row and frequently
# carry the ticker via a "<NAME>: <TICKER>" pattern (e.g.
# "CLASS A: EXMP"), extracted as the row's symbol.

_LEGACY_TX_SECTION_HEADER_RE = re.compile(
    r"^Transaction\s+Detail"
    r"(?:\s*[-–]\s*(?P<category>[A-Za-z &/+\-]+?))?"
    r"(?:\s+\(continued\))?\s*$"
)
_LEGACY_TX_SECTION_END_RE = re.compile(
    r"^(?:Total\s+Account\s+Value\b|Endnotes\s+For\s+Your\s+Account\b)"
)
# Hard boundary inside a transactions section. A row's continuation
# absorption STOPS when one of these matches the line — even though
# the section itself hasn't ended. The legacy layout interleaves a
# transaction sub-section with page-header repeats, disclosure
# paragraphs, Pending Corporate Actions, Open Orders, etc.; without
# these stops a Bank-Interest row whose data lives on the very last
# row of a sub-section would absorb the entire trailing page chrome
# (full account-holder name, account number, disclosure boilerplate,
# pending trades — i.e. a PII leak into silver). `in_section` is not
# dropped: more legitimate data rows may follow on a later page.
# Page-chrome / sibling-section markers that flush the current
# row in the legacy parser. The keep-list mixes structural
# anchors (Statement Period / Account Number) and the
# custodial-account-specific headers Schwab uses for ESA / UTMA
# / IRA pages — those carry the full custodian + beneficiary
# names ("Education Savings of <NAME> FBO <NAME> ED SAVINGS
# ACCT CHARLES SCHWAB & CO INC CUST", "<NAME> CUST FOR <NAME>
# UCAUTMA UNTIL AGE <N>") which must never bleed into the
# description column.
_LEGACY_TX_ROW_STOP_RE = re.compile(
    r"^(?:"
    r"Page\s+\d+\s+of\s+\d+\b"
    r"|Account\s+Number\b"
    r"|Statement\s+Period\b"
    r"|Please\s+see\b"
    r"|For\s+(?:the\s+)?Schwab\s+One\b"
    r"|Schwab\s+One\b"
    r"|Education\s+Savings\b"
    r"|Contributory\s+IRA\b"
    r"|Roth\s+IRA\b"
    r"|Traditional\s+IRA\b"
    r"|Inherited\s+IRA\b"
    r"|Designated\s+Bene\b"
    r"|Charles\s+Schwab\b"
    r"|Pending\s+Corporate\s+Actions\b"
    r"|Open\s+Orders\b"
    r"|Margin\s+Loan\s+Information\b"
    r"|Asset\s+Composition\b"
    r"|Investment\s+Detail\b"
    r"|Cash\s+Transactions\s+Summary\b"
    r"|Opening\s+Balance\b"
    r"|Ending\s+Balance\b"
    r"|Bank\s+Sweep:"
    r"|Total\s+Cash\s+Transaction\s+Detail\b"
    # The sub-section's closing summary ("The total deposits activity
    # for the statement period was $…") and the next sub-section's
    # banner when it runs on past the header pattern. Left to absorb
    # them, the last row of a sub-section carries the period's totals
    # in its description, which then reads as a different payee.
    r"|The\s+total\s+(?:deposits|withdrawals)\s+activity\b"
    r"|Transaction\s+Detail\b"
    # The margin disclosures a sub-section closes with, likewise.
    r"|Margin\s+interest\s+charged\s+to\s+your\s+Account\b"
    r"|The\s+opening\s+margin\s+loan\s+balance\b"
    r"|Latest\s+Price\b"
    r"|©"  # copyright line
    r")"
    # Also stop on any line containing custodial-header markers
    # ("FBO" / "CUST FOR" / a state-coded "<state>UTMA" or
    # "<state>UGMA", for any state) anywhere — Schwab emits them as
    # the FIRST page-header line for ESA / UTMA / UGMA accounts.
    r"|.*\b(?:FBO|CUST\s+FOR|U?[A-Z]{2}U[TG]MA|U[TG]MA)\b",
    re.IGNORECASE,
)
# Belt-and-suspenders cap. A legitimate transaction description in
# this layout is at most a couple of short uppercase fragments
# ("CLASS A", "SPONSORED ADR", "1 ADR REPS 8 ORD SHS", "NOTE
# DUE12/31/99"); anything past ~200 chars is parser drift. The
# cap drops further continuation lines once the description has
# hit it.
_LEGACY_TX_DESC_MAX_CHARS = 200
# Cap on the number of continuation lines per row. Real
# descriptions wrap onto at most 2-3 lines; 5 is generous enough
# to handle edge cases without swallowing a page of chrome.
_LEGACY_TX_MAX_CONTINUATIONS = 5
# Activity sub-header: "<AssetClass> Activity" — used as the
# row's category hint when the main section header is the
# bare 2017-2019 "Transaction Detail".
_LEGACY_TX_ACTIVITY_SUBHEADER_RE = re.compile(
    r"^(?P<activity>[A-Za-z][A-Za-z, &]*?)\s+Activity\s*(?:\(continued\))?\s*$"
)
# Settle / trade date pair at start of a row: MM/DD or MM/DD/YY
# (with 2 or 4 digit year) — Schwab is inconsistent across eras.
_LEGACY_TX_DATE_RE = re.compile(
    r"^(?P<settle>\d{1,2}/\d{1,2}(?:/\d{2,4})?)\s+"
    r"(?P<trade>\d{1,2}/\d{1,2}(?:/\d{2,4})?)\s+(?P<rest>.+)$"
)
# In-description "<NAME>: <TICKER>" pattern. Ticker is the LAST
# colon-led uppercase token on the line (the whole logical row,
# continuation lines included, is searched).
_LEGACY_TX_SYMBOL_IN_DESC_RE = re.compile(
    r":\s+([A-Z][A-Z0-9./-]{0,8})\b"
)
# Known transaction-type phrases — longest first so multi-word
# phrases match before their single-word shortcuts (e.g.
# "Cash Dividend" wins over "Dividend"). Each value is the
# `kind` surfaced in the silver row; the gold layer maps it to
# its canonical taxonomy.
_LEGACY_TX_KIND_PHRASES: list[tuple[str, str]] = [
    ("Reinvested Shares",       "Reinvest"),
    ("Reinvestment Adjustment", "Reinvest"),
    ("Div For Reinvest",        "Reinvest"),
    ("Cash Dividend",           "Dividend"),
    ("Qualified Dividend",      "Dividend"),
    ("Bank Interest",           "Interest"),
    ("Credit Interest",         "Interest"),
    ("Margin Interest",         "Interest"),
    ("Interest Paid",           "Interest"),
    ("Reverse Split",           "Split"),
    ("Forward Split",           "Split"),
    ("Funds Received",          "Deposit"),
    ("Funds Paid",              "Withdrawal"),
    ("MoneyLink Deposit",       "Deposit"),
    ("MoneyLink Return",        "Transfer"),
    ("MoneyLink Txn",           "Transfer"),
    ("Auto Transfer",           "Transfer"),
    ("Journal",                 "Journal"),
    ("Spin-Off",                "Spin-Off"),
    ("Merger",                  "Merger"),
    ("NRA Tax",                 "Tax"),
    ("Tax Withholding",         "Tax"),
    ("Bought",                  "Purchase"),
    ("Sold",                    "Sale"),
    ("Sale",                    "Sale"),
    ("Purchase",                "Purchase"),
    ("Withdrawal",              "Withdrawal"),
    ("Deposit",                 "Deposit"),
    ("Dividend",                "Dividend"),
    ("Interest",                "Interest"),
    ("Tax",                     "Tax"),
    ("Redemption",              "Redemption"),
    ("Distribution",            "Distribution"),
    ("Exchange",                "Exchange"),
    ("Adjustment",              "Adjustment"),
    ("Fee",                     "Fee"),
    ("Transfer",                "Transfer"),
    ("Split",                   "Split"),
]

# Footnote-marker suffix Schwab glues to the transaction-type
# column with no separating whitespace, e.g. "Bank InterestX,Z",
# "Interest PaidX,Z", "Auto TransferX". The markers are single
# letters, comma-separated; the full taxonomy is
# in each statement's "Endnotes For Your Account" section. Used
# to make the kind-phrase match tolerant of the gluing.
_LEGACY_TX_FOOTNOTE_SUFFIX = r"(?:[A-Za-z](?:,[A-Za-z]){0,4})?"

# Compiled once from _LEGACY_TX_KIND_PHRASES: matches a kind
# phrase at the start of the description, optionally followed by
# a glued footnote marker, then whitespace or end-of-string. The
# phrase group looks up the (kind, phrase-len) to surface.
_LEGACY_TX_KIND_RE = re.compile(
    r"^(?P<phrase>" +
    "|".join(re.escape(p) for p, _ in _LEGACY_TX_KIND_PHRASES) +
    r")" + _LEGACY_TX_FOOTNOTE_SUFFIX + r"(?:\s|$)"
)
_LEGACY_TX_KIND_LOOKUP = dict(_LEGACY_TX_KIND_PHRASES)


def _parse_legacy_tx_date(token: str, statement_year: int,
                          period_end: date | None = None) -> date | None:
    """Parse a settle / trade date token like "12/15" or
    "06/17/24". A token without a year takes it from the statement
    (_row_date). Returns None on malformed input."""
    parts = token.split("/")
    try:
        if len(parts) == 2:
            return _row_date(int(parts[0]), int(parts[1]), statement_year, period_end)
        if len(parts) == 3:
            mm, dd, yy = int(parts[0]), int(parts[1]), int(parts[2])
            return date(2000 + yy if yy < 100 else yy, mm, dd)
        return None
    except ValueError:
        return None


def _parse_legacy_tx_row_line(line: str, statement_year: int,
                              period_end: date | None = None):
    """If `line` begins with a Settle Date + Trade Date pair,
    parse it into a TransactionRow with .amount / .date / .symbol
    populated from what can be recovered. Continuation lines (no
    date prefix) return None; the caller appends them to the
    last row's description.
    """
    m = _LEGACY_TX_DATE_RE.match(line)
    if m is None:
        return None
    settle = _parse_legacy_tx_date(m.group("settle"), statement_year, period_end)
    if settle is None:
        return None
    rest = m.group("rest")
    tokens = rest.split()
    if not tokens:
        return None

    # Peel trailing numeric tokens. Use a permissive regex —
    # accept negatives in parens "(1,234.56)" and trailing flags.
    trailing: list[str] = []
    cut = len(tokens)
    while cut > 0 and _NUM_RE.fullmatch(tokens[cut - 1]):
        trailing.append(tokens[cut - 1])
        cut -= 1
    trailing.reverse()
    if not trailing:
        return None
    amount = _parse_number(trailing[-1])
    quantity = price = charges = None
    # Common column counts (after stripping the trailing Amount):
    #   3 → Quantity, UnitPrice, Amount
    #   4 → Quantity, UnitPrice, Charges, Amount
    if len(trailing) >= 4:
        quantity = _parse_number(trailing[-4])
        price    = _parse_number(trailing[-3])
        charges  = _parse_number(trailing[-2])
    elif len(trailing) == 3:
        quantity = _parse_number(trailing[-3])
        price    = _parse_number(trailing[-2])

    desc_tokens = tokens[:cut]
    desc = " ".join(desc_tokens)

    # Pull out the symbol via "<NAME>: <TICKER>" pattern. The
    # last colon-led uppercase token wins (Schwab puts the
    # ticker at the end of the description).
    symbol = None
    matches = list(_LEGACY_TX_SYMBOL_IN_DESC_RE.finditer(desc))
    if matches:
        symbol = matches[-1].group(1)

    # Categorise. The regex matches the longest known phrase at
    # the start of the description, tolerating a glued footnote
    # marker like "X", "X,Z", "M,F" between the phrase and the
    # next token (Schwab renders footnote letters as inline
    # superscripts that pypdfium2 lowers to the baseline with no
    # whitespace — without the tolerance, "Bank InterestX,Z BANK
    # INT ..." wouldn't match "Bank Interest" and the row would
    # land as kind=Unknown).
    kind = None
    m_kind = _LEGACY_TX_KIND_RE.match(desc)
    if m_kind:
        kind = _LEGACY_TX_KIND_LOOKUP[m_kind.group("phrase")]

    row = TransactionRow(
        date=settle,
        category=kind,
        action=None,
        symbol=symbol,
        description=desc,
        quantity=quantity,
        price=price,
        charges=charges,
        amount=amount,
        realized_gain_loss=None,
        term=None,
    )
    return row


def _parse_transactions_legacy(text: str,
                                 statement_year: int,
                                 period_end: date | None = None) -> list[TransactionRow]:
    """Parse pre-2025 "Transaction Detail" / "Transaction Detail
    - <Category>" sections. Returns a list of TransactionRow."""
    lines = [ln.rstrip() for ln in text.split("\n")]
    rows: list[TransactionRow] = []
    in_section = False
    current_row: TransactionRow | None = None

    # Per-row continuation counter; reset each time a fresh row
    # starts. Used together with _LEGACY_TX_DESC_MAX_CHARS and
    # _LEGACY_TX_ROW_STOP_RE to bound how much surrounding page
    # chrome a row can absorb.
    continuations = 0
    for raw in lines:
        line = raw.strip()
        if not line:
            continue
        if _LEGACY_TX_SECTION_HEADER_RE.match(line):
            in_section = True
            continue
        if not in_section:
            continue
        if _LEGACY_TX_SECTION_END_RE.match(line):
            if current_row is not None:
                rows.append(current_row)
                current_row = None
            in_section = False
            continue
        # Soft boundary: page-header repeats, disclosure paras,
        # sibling-section starts. Flush any current row so its
        # description stops growing, but stay in the section in
        # case more data rows follow on a later page.
        if _LEGACY_TX_ROW_STOP_RE.match(line):
            if current_row is not None:
                rows.append(current_row)
                current_row = None
                continuations = 0
            continue
        # Skip the multi-line column-header block: "Settle",
        # "Date", "Trade", "Date Transaction Description ...",
        # the per-asset-class "<X> Activity" sub-headers, and
        # any totals lines. The data rows all begin with
        # MM/DD date pairs, so non-matching lines either belong
        # to chrome or to a continuation of the previous row.
        if _LEGACY_TX_ACTIVITY_SUBHEADER_RE.match(line):
            continue
        if line in ("Settle", "Date", "Trade", "Transaction"):
            continue
        if line.startswith(("Total ", "Settle Date", "Trade Date",
                            "Date Transaction", "Charges and",
                            "Interest Total")):
            continue
        parsed = _parse_legacy_tx_row_line(line, statement_year, period_end)
        if parsed is not None:
            if current_row is not None:
                rows.append(current_row)
            current_row = parsed
            current_row.raw_lines.append(raw)
            continuations = 0
        elif current_row is not None:
            # Continuation — append to description, look for
            # an embedded "<NAME>: <TICKER>" that may carry the
            # ticker if the main row didn't. Bounded by both a
            # max-line count and a max char count so a row near
            # the end of a sub-section can't swallow the
            # trailing page of chrome (which contains the
            # account holder's full name + account number in
            # this layout).
            if (continuations >= _LEGACY_TX_MAX_CONTINUATIONS
                    or len(current_row.description) >= _LEGACY_TX_DESC_MAX_CHARS):
                continue
            current_row.raw_lines.append(raw)
            continuations += 1
            if current_row.symbol is None:
                ms = list(_LEGACY_TX_SYMBOL_IN_DESC_RE.finditer(line))
                if ms:
                    current_row.symbol = ms[-1].group(1)
            current_row.description = (
                current_row.description + " " + line
            ).strip()
            # If this line crossed the cap, truncate cleanly so
            # consumers see a deterministic length rather than
            # whatever the last line happened to add.
            if len(current_row.description) > _LEGACY_TX_DESC_MAX_CHARS:
                current_row.description = (
                    current_row.description[:_LEGACY_TX_DESC_MAX_CHARS].rstrip()
                )
    if current_row is not None:
        rows.append(current_row)
    return rows


# ============================================================
# Legacy position parser (2020-2024 statement layout)
# ============================================================
#
# Schwab redesigned the statement template at the 2024/2025
# boundary. Pre-2025 statements use "Investment Detail - X"
# section headers instead of "Positions - X", and each position
# block is a multi-line structure:
#
#   COMPANY NAME [TYPE] [(M)] qty price mv pct% gain yield income
#   <description fragment line>
#   <description fragment line>
#   SYMBOL: TICKER 25.0000 100.00 2,500.00 01/03/22 500.00 ...
#                  50.0000 100.00 5,000.00 02/01/22 1,000.00 ...
#   Cost Basis 6,000.00 [Accrued Dividend: 100.00]
#
# Key differences from 2025+:
#   - First token of the main row is the COMPANY NAME, not the
#     ticker. The real ticker lives on the "SYMBOL: XXX" line.
#   - Cost basis is a SEPARATE line, not a column in the main row.
#   - Each position is followed by one line per tax lot
#     (_parse_legacy_lot_line). A holding with several lots prints
#     their total on the "Cost Basis" line; a holding with one lot
#     prints no total, so its cost basis is the lot's.
#   - Main row carries the section's trailing columns
#     (_LEGACY_DEFAULT_COLUMNS, _LEGACY_SECTION_COLUMNS). Cost basis
#     comes from the "Cost Basis" continuation line, except where
#     Mutual Funds prints it in the row.

_LEGACY_POSITIONS_HEADER_RE = re.compile(
    r"^Investment\s+Detail\s*[-–]\s*"
    r"(?P<section>[A-Za-z][\w &/+\-]*?)"
    r"(?:\s+\(continued\))?\s*$"
)
_LEGACY_POSITIONS_FOOTER_RE = re.compile(
    r"^Total\s+Investment\s+Detail\b"
)
# Section names that DON'T carry per-instrument securities —
# cash flows live in historical_cash_balances, not positions.
_LEGACY_NON_INSTRUMENT_SECTIONS = frozenset({
    "Cash", "Bank Sweep", "Cash and Bank Sweep", "Total",
})
_LEGACY_SYMBOL_LINE_RE = re.compile(
    r"\bSYMBOL:\s+(?P<ticker>[A-Z][A-Z0-9./-]{0,8})\b"
)
_LEGACY_COST_BASIS_RE = re.compile(
    r"^Cost\s+Basis\s+(?P<cb>\(?[\d,]+\.\d+\)?)"
    r"(?:\s+Accrued\s+(?:Dividend|Interest):\s+(?P<acc>\(?[\d,]+\.\d+\)?))?"
)

# The trailing columns of a legacy main row, in print order. Most
# sections print seven; Mutual Funds prints cost basis in the row and
# no yield or income; Options prints no yield, income or cost basis.
# A row can stop after % of account. So the columns are aligned on the
# % of account column (_LEGACY_PCT_RE), not counted from either end: a
# holding whose name ends in a number ("… EXAMPLE 2000") keeps that
# number in its description, and a row without the later columns still
# parses.
_LEGACY_DEFAULT_COLUMNS = (
    "quantity", "market_price", "market_value", "pct_of_acct",
    "unrealized_gain_loss", "est_yield", "est_annual_income",
)
_LEGACY_SECTION_COLUMNS = {
    "Mutual Funds": (
        "quantity", "market_price", "market_value", "cost_basis",
        "unrealized_gain_loss", "pct_of_acct",
    ),
    "Options": (
        "quantity", "market_price", "market_value", "pct_of_acct",
        "unrealized_gain_loss",
    ),
}
# Columns kept as printed rather than parsed to a number.
_LEGACY_TEXT_COLUMNS = frozenset({"pct_of_acct", "est_yield"})
# The % of account column: a whole percent or "<1%". A yield also ends
# in "%" but prints decimals ("1.25%"), so it never matches.
_LEGACY_PCT_RE = re.compile(r"^(?:<1|\d{1,3})%$")
# The columns every legacy row starts with; a row that stops after % of
# account prints only these, whatever its section prints otherwise.
_LEGACY_LEADING_COLUMNS = (
    "quantity", "market_price", "market_value", "pct_of_acct",
)
# An option's contract on its SYMBOL: line, which can wrap the strike
# and call/put onto the next line past the lot detail:
#   SYMBOL: XMPL 01/16/2026 50.00 C
#   SYMBOL: XMPL 01/16/2026 1.0000 2.0000 200.00 12/01/25 50.00
#   50.00 C
# The key matches the one the transactions parser gives the
# contract's trades.
_LEGACY_OPTION_SYMBOL_RE = re.compile(
    r"\bSYMBOL:\s+(?P<under>[A-Z][A-Z0-9./-]{0,8})\s+(?P<exp>\d{2}/\d{2}/\d{4})\b"
    r".*?(?P<strike>[\d,]*\.\d+)\s+(?P<cp>[CP])\b")


# A tax-lot line. Its columns are units purchased, cost per share, cost
# basis, acquired date, unrealized gain and, on most statements, the
# holding days and the holding period:
#   CLASS A 25.0000 35.0000 875.00 01/15/22 125.00 800 Long-Term
#   SYMBOL: XMPL 10.0000 20.0000 200.00 t 03/01/21 (50.00) 400 Long-Term
#   1.0000 S 6.0000 (600.00) 01/02/26 200.00 30 Short-Term
#   5.0000 12.0000 r 60.00 (2.00) Long-Term
#   10.0000 N/A please provide 04/05/21 N/A
# The line can open with the holding's description or its SYMBOL: tag,
# and that text can hold numbers and dates of its own (an option's
# expiry), so the columns are read from the right. An endnote marker can
# follow the cost basis ("t": basis from a third party, "e": edited by
# the holder). Reinvested dividends print as one summary lot with an "r"
# marker and no acquired date. A short lot prints its units unsigned
# behind an "S" marker and its cost basis negative. A lot whose basis
# Schwab does not know prints "N/A please provide" for cost per share and
# cost basis.
_LEGACY_LOT_DATE_RE = re.compile(r"^\d{2}/\d{2}/\d{2}$")
_LEGACY_LOT_TERMS = {"Long-Term": "LONG", "Short-Term": "SHORT"}


def _parse_legacy_lot_line(line: str) -> dict | None:
    """One tax lot from a legacy lot line, or None when the line is not
    one. The lot's figures are as printed; its quantity is negative for
    a short lot, as the holding's is. `acquired_date` is ISO, None when
    the line prints no date."""
    toks = line.split()

    def num(tok: str) -> bool:
        return bool(_NUM_RE.fullmatch(tok))

    term = _LEGACY_LOT_TERMS.get(toks[-1]) if toks else None
    if term:
        toks.pop()
    holding_days = None
    if term and toks and toks[-1].isdigit():
        holding_days = int(toks.pop())
    if not toks or not (num(toks[-1]) or toks[-1] == "N/A"):
        return None
    gain = _parse_number(toks.pop())
    footnotes: list[str] = []
    acquired = None
    if toks and _LEGACY_LOT_DATE_RE.match(toks[-1]):
        try:
            acquired = datetime.strptime(toks.pop(), "%m/%d/%y").date().isoformat()
        except ValueError:
            return None
        if toks and _POSITION_FOOTNOTE_RE.fullmatch(toks[-1]):
            footnotes.append(toks.pop())
        if toks[-2:] == ["please", "provide"]:
            del toks[-2:]
            cost_basis = unit_cost = None
            if not toks or toks.pop() != "N/A":
                return None
        elif len(toks) >= 3 and num(toks[-1]) and num(toks[-2]):
            cost_basis = _parse_number(toks.pop())
            unit_cost = _parse_number(toks.pop())
        else:
            return None
    elif len(toks) >= 4 and num(toks[-1]) and toks[-2] == "r" and num(toks[-3]):
        cost_basis = _parse_number(toks.pop())
        footnotes.append(toks.pop())
        unit_cost = _parse_number(toks.pop())
    else:
        return None
    short = bool(toks) and toks[-1] == "S"
    if short:
        footnotes.insert(0, toks.pop())
    if not toks or not num(toks[-1]):
        return None
    quantity = _parse_number(toks.pop())
    if short and quantity is not None:
        quantity = -abs(quantity)
    return {
        "quantity": quantity,
        "unit_cost": unit_cost,
        "cost_basis": cost_basis,
        "acquired_date": acquired,
        "unrealized_gain_loss": gain,
        "holding_days": holding_days,
        "term": term,
        "footnotes": footnotes or None,
        "raw_line": line,
    }


def _legacy_columns(section: str) -> tuple[str, ...]:
    """The trailing columns a legacy main row of `section` prints."""
    return _LEGACY_SECTION_COLUMNS.get(section, _LEGACY_DEFAULT_COLUMNS)


def _legacy_row_columns(tokens: list[str], section: str,
                        ) -> tuple[list[str], dict[str, str]] | None:
    """A legacy main row's description tokens and its columns by name;
    None when the line is not a main row.

    The columns are the trailing run of column tokens, read past any
    endnote markers (_peel_position_columns) and aligned so that a % of
    account column falls on the first whole-percent token with all the
    columns before it in the run. Columns after it are taken as far as
    the row prints them. The section's own layout is tried first, then
    the leading columns alone (_LEGACY_LEADING_COLUMNS). A column token
    before the aligned run belongs to the description ("… EXAMPLE 2000").

    A short holding has no % of account but an "S" endnote; its columns
    are the section's without % of account, and its quantity is
    returned negative, the way the 2025+ layout prints it."""
    cols, consumed, footnotes = _peel_position_columns(tokens)
    desc = tokens[: len(tokens) - consumed]
    for layout in (_legacy_columns(section), _LEGACY_LEADING_COLUMNS):
        pct = layout.index("pct_of_acct")
        for i in range(pct, len(cols)):
            if _LEGACY_PCT_RE.match(cols[i]):
                k = i - pct
                return desc + cols[:k], dict(zip(layout, cols[k:], strict=False))
    if not any("S" in f.split(",") for f in footnotes):
        return None
    return _legacy_short_row_columns(tokens, section)


def _legacy_short_row_columns(tokens: list[str], section: str,
                              ) -> tuple[list[str], dict[str, str]] | None:
    """A short legacy row. It prints no % of account; its price carries
    the "S" endnote, glued to the front or standing alone before it; its
    value is negative and its quantity unsigned:
      CALL EXAMPLE CORP 1.0000 S2.50000 (250.00) 100.00
      EXAMPLE CORP (M) 100.0000 S 2.50000 (250.00) (10.00) N/A N/A
    The row is anchored on the marker: the quantity is the token before
    it, the price the token it is glued to or the next one, and the
    section's later columns (bar % of account) follow."""
    layout = [c for c in _legacy_columns(section) if c != "pct_of_acct"]
    for j in range(len(tokens) - 1, 0, -1):
        if tokens[j] == "S":
            price, rest = tokens[j + 1:j + 2], tokens[j + 2:]
        elif tokens[j][:1] == "S" and _is_trailing_col_token(tokens[j][1:]):
            price, rest = [tokens[j][1:]], tokens[j + 1:]
        else:
            continue
        if not price or not rest or not _is_trailing_col_token(tokens[j - 1]):
            return None
        printed = dict(zip(layout, [tokens[j - 1], *price, *rest], strict=False))
        printed["quantity"] = f"({printed['quantity'].strip('()')})"
        return tokens[: j - 1], printed
    return None


def _parse_positions_legacy(text: str) -> list[dict]:
    """Parse the 2020-2024 "Investment Detail - X" layout.

    Same return shape as the 2025+ parser, plus each holding's tax
    lots under "lots". A block is the run of lines from one "main
    row" (a line _legacy_row_columns reads columns from, AND a
    leading company-name token) to the next; the SYMBOL:, tax-lot,
    Cost Basis and description lines that follow belong to it.
    """
    lines = [ln.rstrip() for ln in text.split("\n")]
    rows: list[dict] = []
    section: str | None = None
    block: list[str] = []

    def _flush():
        nonlocal block
        if block and section is not None:
            parsed = _parse_legacy_position_block(block, section)
            if parsed is not None:
                rows.append(parsed)
        block = []

    for raw in lines:
        line = raw.strip()
        if not line:
            continue
        m = _LEGACY_POSITIONS_HEADER_RE.match(line)
        if m:
            new_section = m.group("section").strip()
            # A section continued on the next page keeps the open
            # holding: its remaining lots and its Cost Basis line can
            # follow the page break.
            if new_section == section and line.endswith("(continued)"):
                continue
            _flush()
            section = (
                None if new_section in _LEGACY_NON_INSTRUMENT_SECTIONS
                else new_section
            )
            continue
        if section is None:
            continue
        if _LEGACY_POSITIONS_FOOTER_RE.match(line):
            _flush()
            section = None
            continue

        tokens = line.split()
        if not tokens:
            continue
        # A "main row" carries the section's columns through % of
        # account (_legacy_row_columns) AND a leading uppercase token
        # that is NOT a known continuation marker. Tax-lot lines have
        # a date token mid-row which breaks the trailing-token run, so
        # they never reach a % of account column.
        is_main = (
            tokens[0] not in ("SYMBOL:", "Cost", "Accrued", "Total")
            and re.match(r"^[A-Z][A-Z0-9.&'/-]*$", tokens[0])
            and _legacy_row_columns(tokens, section) is not None
        )
        if is_main:
            _flush()
            block = [line]
        elif block:
            block.append(line)
        # else: chrome before any main row — skip.
    _flush()
    return rows


def _parse_legacy_position_block(block_lines: list[str],
                                  section: str) -> dict | None:
    """Parse one multi-line legacy position block. Returns a row
    dict in the same shape as the 2025+ parser's output, or
    None if the block doesn't parse to a usable row.
    """
    if not block_lines:
        return None
    main = block_lines[0]
    tokens = main.split()
    found = _legacy_row_columns(tokens, section)
    if found is None:
        return None
    desc_tokens, printed = found
    description = " ".join(desc_tokens)
    description = _strip_margin_marker(description)

    instrument_key: str | None = None
    cost_basis: float | None = None
    accrued_interest: float | None = None
    extra_desc: list[str] = []
    lots: list[dict] = []

    # Description fragments follow the main row, among its first lots;
    # the first other line (the SYMBOL: tag, page furniture) ends them.
    in_description = True
    for i, line in enumerate(block_lines[1:], start=1):
        m_sym = _LEGACY_SYMBOL_LINE_RE.search(line)
        if m_sym:
            in_description = False
        if m_sym and instrument_key is None:
            instrument_key = m_sym.group("ticker")
            if section == "Options" and (m_opt := _LEGACY_OPTION_SYMBOL_RE.search(
                    " ".join(block_lines[i:i + 2]))):
                instrument_key = (f"{m_opt['under']} {m_opt['exp']} "
                                  f"{m_opt['strike']} {m_opt['cp']}")
            # Don't `continue`: a SYMBOL: line can also carry a lot.
        m_cb = _LEGACY_COST_BASIS_RE.match(line)
        if m_cb:
            cost_basis = _parse_number(m_cb.group("cb"))
            if m_cb.group("acc"):
                accrued_interest = _parse_number(m_cb.group("acc"))
            continue
        lot = _parse_legacy_lot_line(line)
        if lot is not None:
            lots.append(lot)
            continue
        # A description fragment ("CLASS A", "SPONSORED ADR") is a
        # short all-uppercase line, which keeps disclosure text out.
        toks = line.split()
        if (in_description and len(toks) <= 6
                and all(re.match(r"^[A-Z][\w&./:\-]*$", t) for t in toks)):
            extra_desc.append(line.strip())
        else:
            in_description = False
    if extra_desc:
        description = (description + " " + " ".join(extra_desc)).strip()

    # Fallback ticker: if no SYMBOL: line, use the first
    # description token IF it could plausibly be a ticker.
    if instrument_key is None and desc_tokens:
        first = desc_tokens[0]
        if _TICKER_RE.match(first):
            instrument_key = first
    if instrument_key is None:
        return None
    # A holding with one lot prints no "Cost Basis" total: the lot's
    # cost basis is the holding's.
    if cost_basis is None and len(lots) == 1:
        cost_basis = lots[0]["cost_basis"]

    row = {
        "instrument_key": instrument_key,
        "description": description,
        "quantity": None,
        "market_price": None,
        "market_value": None,
        "cost_basis": cost_basis,
        "unrealized_gain_loss": None,
        "accrued_interest": accrued_interest,
        "est_yield": None,
        "est_annual_income": None,
        "pct_of_acct": None,
    }
    for col, tok in printed.items():
        row[col] = tok if col in _LEGACY_TEXT_COLUMNS else _as_num(tok)
    row["section"] = section
    row["lots"] = lots
    row["raw_lines"] = list(block_lines)
    return row


# ============================================================
# Legacy cash-summary parser (2020-2024 statement layout)
# ============================================================
#
# The pre-2025 layout puts the cash-flow numbers in a multi-line
# block under "Cash Transactions Summary", one labelled row per
# category. Each row carries TWO values: this period and YTD.
# The this-period number is extracted.

_LEGACY_CASH_HEADER_RE = re.compile(
    r"^Cash\s+Transactions\s+Summary\b"
)
# Map source-line label → cash-summary dict key. Schwab is
# consistent across the 2020-2024 era; if a future restyle
# adds new categories the dispatcher simply ignores them.
_LEGACY_CASH_LABELS: list[tuple[str, str]] = [
    ("Starting Cash",                    "opening_balance"),
    ("Deposits and other Cash Credits",  "deposits"),
    ("Investments Sold",                 "sales_redemptions"),
    ("Dividends and Interest",           "dividends_interest"),
    ("Withdrawals and other Debits",     "withdrawals"),
    ("Investments Purchased",            "purchases"),
    ("Fees and Charges",                 "expenses"),
    ("Ending Cash",                      "closing_balance"),
]
# Match a leading $-prefixed number (optionally parenthesised
# negative) on a label line. The label may be followed by a "*"
# (Starting Cash* / Ending Cash*).
_LEGACY_CASH_VALUE_RE = re.compile(
    r"\$?\s*(\(?-?[\d,]+\.\d+\)?)"
)


def _parse_cash_summary_legacy(text: str) -> dict | None:
    lines = text.split("\n")
    in_section = False
    captured: dict[str, float | None] = {}
    for raw in lines:
        line = raw.strip()
        if not in_section:
            if _LEGACY_CASH_HEADER_RE.match(line):
                in_section = True
            continue
        # Section terminator: end of cash block. "Investment
        # Detail - X" headers follow immediately; "Investment
        # Activity" / disclosure boilerplate is also a stop.
        if (
            line.startswith("Investment Detail")
            or line.startswith("Investment Activity")
            or line.startswith("Total Investments")
        ):
            break
        # Try every known label; take the first that matches
        # (longest-label-first so "Ending Cash" wins over a
        # hypothetical "Cash" prefix).
        for label, attr in _LEGACY_CASH_LABELS:
            if line.startswith(label):
                rest = line[len(label):].lstrip("*").lstrip()
                m = _LEGACY_CASH_VALUE_RE.search(rest)
                if m:
                    captured[attr] = _parse_number(m.group(1))
                break
        if "closing_balance" in captured:
            break  # Ending Cash row reached; done.
    if not captured:
        return None

    deposits           = captured.get("deposits")
    sales_redemptions  = captured.get("sales_redemptions")
    dividends_interest = captured.get("dividends_interest")
    withdrawals        = captured.get("withdrawals")
    purchases          = captured.get("purchases")
    expenses           = captured.get("expenses")

    def _sum_or_none(*vs):
        return sum(vs) if all(v is not None for v in vs) else None

    total_credits = _sum_or_none(deposits, sales_redemptions, dividends_interest)
    total_debits_raw = _sum_or_none(withdrawals, purchases, expenses)
    total_debits = -total_debits_raw if total_debits_raw is not None else None

    return {
        "opening_balance":    captured.get("opening_balance"),
        "closing_balance":    captured.get("closing_balance"),
        "deposits":           deposits,
        "withdrawals":        withdrawals,
        "purchases":          purchases,
        "sales_redemptions":  sales_redemptions,
        "dividends_interest": dividends_interest,
        "expenses":           expenses,
        # "Other Activity" is a 2025+ concept; pre-2025
        # statements don't expose it, and silver preserves the
        # absence as NULL rather than 0.0.
        "other_activity":     None,
        "total_credits":      total_credits,
        "total_debits":       total_debits,
        "currency_iso":       "USD",
        "raw_line":           "[legacy multi-line cash summary]",
    }
