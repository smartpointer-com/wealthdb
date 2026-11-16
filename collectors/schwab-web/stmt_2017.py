"""The 2017-2019 layout of a Schwab brokerage statement: holdings and
cash under a bare "Investment Detail" heading.
"""

from __future__ import annotations

import re

from stmt_tokens import _NUM_RE, _TICKER_RE, _parse_number


# ============================================================
# Very-old position parser (2017-2019 statement layout)
# ============================================================
#
# 2017-2019 statements use a much simpler structure than either
# the 2020-2024 or 2025+ era:
#
#   Investment Detail
#   Description Starting Balance Ending Balance
#   Cash and Bank Sweep
#   BANK SWEEP X,Z 5,000.00 6,000.00          ← cash row
#   CASH 0.00 0.00                            ← cash row
#   Description Symbol Quantity Price Market Value
#   Investments
#   SYNTHETIC ONE INC SYN1 100.0000 10.00000 1,000.00   ← position row
#   CLASS A                                   ← description continuation
#   SYNTHETIC TWO INC SYN2 50.0000 20.00000 1,000.00
#   Total Account Value 8,000.00              ← terminator
#
# Position rows carry exactly three trailing numerics
# (Quantity / Price / Market Value). The TICKER is the token
# immediately preceding those numerics. No cost basis, no
# unrealized gain, no yield, no annual income, no % of account
# in this layout. Continuation lines (e.g. "CLASS A",
# "MARKET ETF") have no numerics and append to the previous
# row's description.

_VERY_OLD_INVDETAIL_RE = re.compile(r"^Investment\s+Detail\s*$")
_VERY_OLD_INVESTMENTS_RE = re.compile(r"^Investments\s*$")
_VERY_OLD_FOOTER_RE = re.compile(
    r"^Total\s+Account\s+Value\b"
)
# A position row's last three tokens are decimals — the parser
# tests this with _NUM_RE.fullmatch in a tight loop below.


def _parse_positions_very_old(text: str) -> list[dict]:
    """Parse the 2017-2019 "Investment Detail" / "Investments"
    layout. Returns the same dict shape as the other tiers, with
    cost_basis / unrealized_gain_loss / accrued_interest / est_yield
    / est_annual_income / pct_of_acct all NULL (the source PDF
    simply doesn't carry them)."""
    lines = [ln.rstrip() for ln in text.split("\n")]
    rows: list[dict] = []
    in_section = False    # past "Investment Detail" header
    in_investments = False  # past "Investments" sub-header
    block: list[str] = []

    def _flush():
        nonlocal block
        if block:
            parsed = _parse_very_old_position_block(block)
            if parsed is not None:
                rows.append(parsed)
        block = []

    for raw in lines:
        line = raw.strip()
        if not line:
            continue
        if _VERY_OLD_INVDETAIL_RE.match(line):
            in_section = True
            in_investments = False
            continue
        if not in_section:
            continue
        if _VERY_OLD_INVESTMENTS_RE.match(line):
            in_investments = True
            continue
        if _VERY_OLD_FOOTER_RE.match(line):
            _flush()
            in_section = False
            in_investments = False
            continue
        if not in_investments:
            # Cash and Bank Sweep rows, column headers, etc. —
            # skip; cash positions go to the cash-summary parser.
            continue
        tokens = line.split()
        if not tokens:
            continue
        # Row detection: trailing exactly 3 decimal-numeric
        # tokens AND a token immediately before them that looks
        # like a ticker. Continuation lines (no numerics) feed
        # the previous block's description.
        cnt = 0
        for tok in reversed(tokens):
            if _NUM_RE.fullmatch(tok):
                cnt += 1
            else:
                break
        is_main = (
            cnt >= 3
            and len(tokens) > cnt
            and _TICKER_RE.match(tokens[-cnt - 1]) is not None
        )
        if is_main:
            _flush()
            block = [line]
        elif block:
            block.append(line)
        # else: pre-data chrome (column header etc.) — skip.
    _flush()
    return rows


def _parse_very_old_position_block(block_lines: list[str]) -> dict | None:
    """Parse one 2017-2019 position block: main row + optional
    description-continuation lines."""
    if not block_lines:
        return None
    main = block_lines[0]
    tokens = main.split()
    cnt = 0
    for tok in reversed(tokens):
        if _NUM_RE.fullmatch(tok):
            cnt += 1
        else:
            break
    if cnt < 3:
        return None
    quantity = _parse_number(tokens[-cnt])
    market_price = _parse_number(tokens[-cnt + 1]) if cnt >= 2 else None
    market_value = _parse_number(tokens[-cnt + 2]) if cnt >= 3 else None
    ticker_idx = len(tokens) - cnt - 1
    if ticker_idx < 0:
        return None
    ticker = tokens[ticker_idx]
    if not _TICKER_RE.match(ticker):
        return None
    desc_parts = tokens[:ticker_idx]
    # Description continuations from the rest of the block: take
    # only short uppercase lines, drop anything that looks like
    # noise (long disclosure runs that snuck past the section
    # gate).
    for line in block_lines[1:]:
        toks = line.split()
        if not toks:
            continue
        if len(toks) <= 6 and all(
            re.match(r"^[A-Z][A-Z0-9&./\-]*$", t) for t in toks
        ):
            desc_parts.extend(toks)
    description = " ".join(desc_parts).strip()
    return {
        "instrument_key": ticker,
        "description": description,
        "quantity": quantity,
        "market_price": market_price,
        "market_value": market_value,
        # Pre-2020 statements don't print these.
        "cost_basis": None,
        "unrealized_gain_loss": None,
        "accrued_interest": None,
        "est_yield": None,
        "est_annual_income": None,
        "pct_of_acct": None,
        "section": "Investments",
        "raw_lines": list(block_lines),
    }


# ============================================================
# Very-old cash-summary parser (2017-2019 statement layout)
# ============================================================
#
# 2017-2019 statements show cash position (not flow) inside the
# "Investment Detail" block under a "Cash and Bank Sweep"
# sub-section. Each row is
#   <LABEL> <flags?> <starting_balance> <ending_balance>
# e.g.
#   BANK SWEEP X,Z 5,000.00 1,500.00
#   CASH 0.00 300.00
# The trailing two numerics are starting + ending balance.
# The earliest statements head the sub-section "Cash, Money Market,
# and Deposit Accounts" and print the ending balance alone:
#   DEPOSIT ACCOUNTS X,Z 5,000.00
# so their opening balance is unknown.
# Per-category cash flow (deposits / withdrawals / etc.) is not
# itemised in this layout; only opening/closing are surfaced and
# the per-category fields stay NULL.

_VERY_OLD_CASH_SUBHEADER_RE = re.compile(
    r"^(?:Cash\s+and\s+Bank\s+Sweep"
    r"|Cash,\s+Money\s+Market,\s+and\s+Deposit\s+Accounts)\s*$"
)


def _parse_cash_summary_very_old(text: str) -> dict | None:
    lines = text.split("\n")
    in_section = False
    in_cash = False
    opening_total = 0.0
    opening_known = True
    closing_total = 0.0
    rows_seen = 0
    for raw in lines:
        line = raw.strip()
        if not line:
            continue
        if _VERY_OLD_INVDETAIL_RE.match(line):
            in_section = True
            continue
        if not in_section:
            continue
        if _VERY_OLD_CASH_SUBHEADER_RE.match(line):
            in_cash = True
            continue
        # End of cash sub-section: any other Description- /
        # Investments- / Total-Account-Value marker.
        if in_cash and (
            line.startswith("Description ")
            or _VERY_OLD_INVESTMENTS_RE.match(line)
            or _VERY_OLD_FOOTER_RE.match(line)
        ):
            in_cash = False
            if rows_seen > 0:
                break
            continue
        if not in_cash:
            continue
        # Each cash row: a label, then the starting and ending
        # balance, or the ending balance alone.
        tokens = line.split()
        if len(tokens) < 2 or not _NUM_RE.fullmatch(tokens[-1]):
            continue
        closing = _parse_number(tokens[-1])
        opening = (_parse_number(tokens[-2])
                   if len(tokens) >= 3 and _NUM_RE.fullmatch(tokens[-2])
                   else None)
        if closing is None:
            continue
        if opening is None:
            opening_known = False
        else:
            opening_total += opening
        closing_total += closing
        rows_seen += 1
    if rows_seen == 0:
        return None

    # Period dates are added by the caller; this emits only
    # the cash numbers. Use 2017-2019 NULLs for the per-category
    # fields the source doesn't carry.
    return {
        "opening_balance": opening_total if opening_known else None,
        "closing_balance": closing_total,
        "deposits":           None,
        "withdrawals":        None,
        "purchases":          None,
        "sales_redemptions":  None,
        "dividends_interest": None,
        "expenses":           None,
        "other_activity":     None,
        "total_credits":      None,
        "total_debits":       None,
        "currency_iso":       "USD",
        "raw_line":           "[very-old: cash positions from Investment Detail block]",
    }
