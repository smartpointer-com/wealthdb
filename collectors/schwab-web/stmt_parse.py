"""A Schwab brokerage statement, read as one document kind.

:func:`parse` reads an extracted statement into its period,
transactions, positions, cash summary, account value, registration
label and account number. It takes the text PDFium extracts
(``collectorkit.extract.pdfium``), one row per line, top to bottom.

Schwab's tabular layout in the source PDF survives reasonably
well into the line stream because each row starts at the same x
and columns are space-separated when there is no value collision.
Where rows wrap (long descriptions, "IndustryFee$0.16" notes,
continuation symbols), the continuation is attached to the
previously-seen row via heuristics on the leading character set.

The Date column is MM/DD — no year. The statement period, parsed from
the first page header, supplies the correct year.

Three layouts exist, each read by its own module: ``stmt_2025``,
``stmt_legacy`` (2020-2024) and ``stmt_2017``. Each part of a
statement is read by trying the layouts in turn. :func:`detect_era`
names a statement's layout from its section headings; the parse does
not depend on it.
"""

from __future__ import annotations

import re

from collectorkit.words import Extracted
from stmt_2017 import (
    _VERY_OLD_INVDETAIL_RE,
    _parse_cash_summary_very_old,
    _parse_positions_very_old,
)
from stmt_2025 import (
    _POSITIONS_HEADER_RE,
    _TX_DETAILS_HEADER_RE,
    _parse_cash_summary_new,
    _parse_positions_new,
    _parse_transactions_new,
)
from stmt_header import (
    parse_account_number,
    parse_account_registration,
    parse_account_value,
    parse_statement_period,
)
from stmt_legacy import (
    _LEGACY_POSITIONS_HEADER_RE,
    _parse_cash_summary_legacy,
    _parse_positions_legacy,
    _parse_transactions_legacy,
)
from stmt_tokens import TransactionRow, _NUM_RE, _parse_number


# ============================================================
# The statement
# ============================================================

def parse(extracted: Extracted, statement_year: int | None = None) -> dict:
    """An extracted Schwab brokerage statement as a dict with the
    statement period, transactions, positions, cash summary, account
    value, registration label and account number. The cash summary
    carries the rate its cash earns (parse_cash_interest_rate) under
    `interest_rate` and `interest_rate_line`, None where the statement
    states none.

    `statement_year` dates the MM/DD activity rows of a statement
    whose period header does not read (parse_transactions); a printed
    period wins over it. The loader passes the year of the manifest
    doc-date.
    """
    full_text = extracted.text()
    period = parse_statement_period(full_text)
    txs = parse_transactions(full_text, statement_year=statement_year)
    positions = parse_positions(full_text)
    cash = parse_cash_summary(full_text)
    if cash is not None:
        cash.update(parse_cash_interest_rate(full_text)
                    or {"interest_rate": None, "interest_rate_line": None})
    account_value = parse_account_value(full_text)
    registration = parse_account_registration(full_text)
    account_number = parse_account_number(full_text)
    return {
        "period_start": period[0].isoformat() if period else None,
        "period_end": period[1].isoformat() if period else None,
        "transactions": [t.to_dict() for t in txs],
        "positions": positions,
        "cash_summary": cash,
        "account_value": account_value,
        "account_registration": registration,
        "account_number": account_number,
    }


# ============================================================
# The layouts
# ============================================================

# Each layout's readers of the three parts: transactions, positions
# and cash. The 2017-2019 layout prints its activity in the
# "Transaction Detail" sections of 2020-2024.
ERA_READERS = {
    "2025": (_parse_transactions_new, _parse_positions_new,
             _parse_cash_summary_new),
    "2020": (_parse_transactions_legacy, _parse_positions_legacy,
             _parse_cash_summary_legacy),
    "2017": (_parse_transactions_legacy, _parse_positions_very_old,
             _parse_cash_summary_very_old),
}


def detect_era(text: str) -> str | None:
    """A statement's layout from its section headings: "2025" for a
    "Positions - <Section>" or "Transaction Details" heading, "2020"
    for "Investment Detail - <Section>", "2017" for a bare
    "Investment Detail"; None when it prints none of them."""
    lines = [ln.strip() for ln in text.split("\n")]
    for era, found in (
            ("2025", lambda ln: (_POSITIONS_HEADER_RE.match(ln)
                                 or _TX_DETAILS_HEADER_RE.match(ln))),
            ("2020", _LEGACY_POSITIONS_HEADER_RE.match),
            ("2017", _VERY_OLD_INVDETAIL_RE.match)):
        if any(found(ln) for ln in lines):
            return era
    return None


def parse_transactions(text: str, statement_year: int | None = None) -> list[TransactionRow]:
    """Extract transaction rows from a statement's full-text.

    Returns a list of TransactionRow. Handles all three layout
    eras Schwab has shipped:

      * 2025+: single "Transaction Details" section, rich
        per-row columns including realised-gain term tags.
      * 2020-2024: multiple "Transaction Detail - <Category>"
        sub-sections (Purchases & Sales / Deposits & Withdrawals
        / Dividends & Interest / …) each with their own
        "<AssetClass> Activity" sub-headers.
      * 2017-2019: a single "Transaction Detail" section with
        the same per-activity sub-headers but no per-category
        breakdown in the section name.

    A row printed as MM/DD takes the year of the printed period end,
    which also anchors a date across a year end (_row_date). The
    loader dates the statement's snapshots by the same period end,
    whatever the manifest says. `statement_year` dates the rows of a
    statement whose period header does not read; without either,
    this raises ValueError.
    """
    period = parse_statement_period(text)
    if period is not None:
        period_end = period[1]
        statement_year = period_end.year
    elif statement_year is not None:
        period_end = None
    else:
        raise ValueError(
            "could not find statement period header; pass "
            "statement_year explicitly"
        )
    rows = _parse_transactions_new(text, statement_year, period_end)
    if rows:
        return rows
    return _parse_transactions_legacy(text, statement_year, period_end)


def parse_positions(text: str) -> list[dict]:
    """Extract position rows from a statement's full-text.

    Returns a list of dicts — see _parse_position_block for the
    keys. Cash positions are NOT included (they live in the
    cash-balances parser); "Positions - Summary" / cash-variant
    sections are skipped.

    Handles all three statement-layout eras Schwab has shipped:

      * 2025+: section header "Positions - <Section>",
        single-row-per-instrument with eight trailing columns,
        cost basis in the row. Options and Fixed Income wrap their
        columns over several lines (_parse_wrapped_position_block).
      * 2020-2024: section header "Investment Detail - <Section>",
        multi-line-per-instrument with the section's trailing
        columns on the main row plus a separate "Cost Basis N" line, a
        "SYMBOL: TICKER" continuation that holds the real ticker
        (the main row leads with the company name), and one line per
        tax lot, returned under "lots".
      * 2017-2019: section header "Investment Detail" (no
        subsection), with a single "Investments" sub-header
        followed by simple rows of the shape
        "COMPANY NAME TICKER QUANTITY PRICE MARKET_VALUE" —
        only three trailing columns, no cost basis, no unrealized
        gain, no yield.

    Detection is by attempt: try the 2025+ parser first, then
    legacy, then very-old. A single statement only ever uses one
    layout, so the cost of the fallback calls is one regex scan
    against a few KB of text per miss.
    """
    rows = _parse_positions_new(text)
    if rows:
        return rows
    rows = _parse_positions_legacy(text)
    if rows:
        return rows
    return _parse_positions_very_old(text)


def parse_cash_summary(text: str) -> dict | None:
    """Extract the cash-flow numbers from a statement.

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

    Handles all three layout eras:
      * 2025+: single-line "Transactions - Summary" block with
        eight $-prefixed columns.
      * 2020-2024: multi-line "Cash Transactions Summary" block
        with one labelled row per category (Starting Cash /
        Deposits and other Cash Credits / Investments Sold /
        Dividends and Interest / Withdrawals and other Debits /
        Investments Purchased / Fees and Charges / Ending Cash).
      * 2017-2019: cash-flow categories aren't broken out in
        these statements. The parser only recovers opening /
        closing balances (from the "Cash and Bank Sweep"
        sub-section of "Investment Detail") and leaves the
        per-category fields NULL.

    Returns None if no anchor is present.
    """
    cash = _parse_cash_summary_new(text)
    if cash is not None:
        return cash
    cash = _parse_cash_summary_legacy(text)
    if cash is not None:
        return cash
    return _parse_cash_summary_very_old(text)


# ============================================================
# The cash's interest rate
# ============================================================
#
# What the account's cash earns, in percent a year, as the statement
# states it at the period's end. Two shapes:
#
#   * 2025+: the "Cash and Cash Investments" table has an
#     "Interest/Yield Rate" column, the APY as of the period's last
#     business day. One row per cash line. A "Cash" row prints its
#     figures on its own line. A "Bank Sweep" row prints its bank's name
#     and its endnote markers first and its figures on a later line:
#       Cash , 100.00 250.00 150.00 0.00 1.09% <1%
#       Bank Sweep EXAMPLE
#       BANK,
#       X,Z
#       1,000.00 2,000.00 1,000.00 2.41% 3%
#     The rate is the one figure with decimals that ends in "%". The
#     "% of Acct" column prints a whole percent or "<1%". A line that
#     earns nothing prints no rate.
#   * 2017-2024: a note under the cash lines:
#       Bank Sweep: Interest Rate as of 06/30/21 was 2.41%.
#       Deposit Accounts: Interest rate as of 03/30/18 was 1.09%. ...
#
# The closing cash balance is the sum of the cash lines, so one rate
# stands for it: the rate of the line with the largest ending balance
# among the lines that print one. A money fund is a position, not a
# cash line, and its yield is not read here. A statement that prints no
# rate states none. The "average ... interest rate" in the description
# of a 2023-2024 interest credit is a mean over the interest period, not
# a rate at the period's end, so it is not read either.

_CASH_TABLE_HEADER_RE = re.compile(r"^Cash\s+and\s+Cash\s+Investments$")
_CASH_TABLE_COLUMNS_RE = re.compile(r"^Type\s+Symbol\s+Description\b")
_CASH_TABLE_END_RE = re.compile(r"^Total\s+Cash\s+and\s+Cash\s+Investments\b")
_CASH_TABLE_ROW_RE = re.compile(r"^(?:Cash\s*,|Bank\s+Sweep\b)")
_CASH_RATE_TOKEN_RE = re.compile(r"^\d+\.\d+%$")
# Bounds the lines the table can take: its column headers and rows.
_CASH_TABLE_MAX_LINES = 40
_LEGACY_CASH_RATE_RE = re.compile(
    r"^(?:Bank\s+Sweep|Deposit\s+Accounts):\s+Interest\s+[Rr]ate\s+as\s+of\s+"
    r"\d{2}/\d{2}/\d{2}\s+was\s+(?P<rate>\d+(?:\.\d+)?)%"
)


def parse_cash_interest_rate(text: str) -> dict | None:
    """The rate the statement states for the account's cash, as
    {"interest_rate": percent a year, "interest_rate_line": the line it
    is read from}; None when the statement states none."""
    lines = [raw.strip() for raw in text.split("\n")]
    for i, line in enumerate(lines):
        if (_CASH_TABLE_HEADER_RE.match(line) and i + 1 < len(lines)
                and _CASH_TABLE_COLUMNS_RE.match(lines[i + 1])):
            return _cash_table_rate(lines[i + 2:i + 2 + _CASH_TABLE_MAX_LINES])
    for line in lines:
        m = _LEGACY_CASH_RATE_RE.match(line)
        if m:
            return {"interest_rate": float(m.group("rate")),
                    "interest_rate_line": line}
    return None


def _cash_table_rate(lines: list[str]) -> dict | None:
    """The rate of the 2025+ cash table's rows (see above), or None."""
    best: tuple[float, dict] | None = None
    in_row = False
    for line in lines:
        if _CASH_TABLE_END_RE.match(line):
            break
        m = _CASH_TABLE_ROW_RE.match(line)
        if m:
            in_row = True
        if not in_row:
            continue  # column headers
        toks = line[m.end():].split() if m else line.split()
        amounts = [t for t in toks if _NUM_RE.fullmatch(t)]
        if len(amounts) < 2:
            continue  # a Bank Sweep row's name or markers; its figures follow
        in_row = False
        rates = [t for t in toks if _CASH_RATE_TOKEN_RE.match(t)]
        if not rates:
            continue
        ending = abs(_parse_number(amounts[1]) or 0.0)
        if best is None or ending > best[0]:
            best = (ending, {"interest_rate": float(rates[0][:-1]),
                             "interest_rate_line": line})
    return best[1] if best else None
