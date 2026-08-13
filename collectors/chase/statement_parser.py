#!/usr/bin/env python3
"""Parse a Chase deposit-account statement PDF into transactions + balances.

Chase renders statements with the OpenText engine, which wraps each block in
`*start*<section>*` / `*end*<section>*` markers and lays the transaction detail
out in clean DATE | DESCRIPTION | AMOUNT columns that `pdftotext -layout`
preserves. This reads, per statement:

  - the statement period (`Mon DD, YYYY through Mon DD, YYYY`), which dates each
    `MM/DD` row unambiguously even across a year boundary;
  - the product segments: a combined statement renders one block of sections
    per product, each opened by a `*start*global product*` header carrying its
    own summary (beginning/ending balance) and transaction sections — the
    section names repeat across segments, so rows must be attributed
    per-segment, never pooled;
  - per segment, transactions from the deposit / withdrawal / check / fee
    sections, signed by section (deposits +, everything else -), and the
    beginning + ending balance, to reconstruct a per-row running balance and
    to validate the parse (Σ amounts must carry beginning → ending).

Statements are the only source of transactions older than Chase's 24-month
export cap (DESIGN.md §E): `download` captures the PDFs, this reads them,
and the loader attributes segments to accounts by balance chaining. The
text parsing is pure and unit-tested; `parse_statement_pdf` is the thin
`pdftotext` wrapper around it.
"""
from __future__ import annotations

import re
import subprocess
from dataclasses import dataclass, field
from datetime import date
from decimal import Decimal
from pathlib import Path

_MONTHS = {m: i for i, m in enumerate(
    ["january", "february", "march", "april", "may", "june", "july",
     "august", "september", "october", "november", "december"], 1)}

# The statement period line, e.g. "March 19, 2026 through April 17, 2026".
_PERIOD_RE = re.compile(
    r"([A-Z][a-z]+)\s+(\d{1,2}),\s*(\d{4})\s+through\s+"
    r"([A-Z][a-z]+)\s+(\d{1,2}),\s*(\d{4})")
_SECTION_RE = re.compile(r"\*start\*([a-z0-9 &]+)")
# A transaction row always ends in the amount, but the date sits in one of two
# places: at the line start for deposits / withdrawals (optionally behind a
# check number), or immediately before the amount in the Checks Paid section
# (`CHECK# … MM/DD AMOUNT`). Try the start-date shape first, then the
# date-before-amount shape. The leading `\d+ ` only matches a standalone check
# number — a date's own digits ("03/20") have no space before the slash.
_ROW_START = re.compile(
    r"^\s*(?:\d+\s+)?(?P<mm>\d{2})/(?P<dd>\d{2})\s+(?P<desc>.*?)\s*"
    r"\$?(?P<amt>-?[\d,]+\.\d{2})\s*$")
_ROW_TRAILING_DATE = re.compile(
    r"^\s*(?P<desc>.*?)(?P<mm>\d{2})/(?P<dd>\d{2})\s+"
    r"\$?(?P<amt>-?[\d,]+\.\d{2})\s*$")
_AMOUNT_RE = r"\$?(-?[\d,]+\.\d{2})"


@dataclass
class StatementTxn:
    posted_at: date
    amount: Decimal        # signed: + deposit/addition, - withdrawal/check/fee
    description: str


@dataclass
class StatementSegment:
    """One product's block on the statement: its summary balances plus the
    transaction rows of its own sections."""
    beginning_balance: Decimal | None
    ending_balance: Decimal | None
    transactions: list[StatementTxn] = field(default_factory=list)


@dataclass
class ParsedStatement:
    period_start: date
    period_end: date
    # Per-product segments, always ≥1 — a single-product statement is one
    # segment. Pooling rows across products is meaningless (section names
    # repeat per segment), so there is no statement-level transaction list.
    segments: list[StatementSegment] = field(default_factory=list)


def _dec(s: str) -> Decimal:
    return Decimal(s.replace(",", "").replace("$", ""))


def _section_sign(name: str) -> int | None:
    """+1 / -1 for a transaction section, or None for a non-transaction block.
    Keyword-based so section-name drift across years (e.g. "atm & debit card
    withdrawals", "fees and other withdrawals") still classifies correctly, and
    prose blocks that merely mention "fee" ("post fees message") are excluded."""
    n = name.lower()
    if any(k in n for k in ("message", "summary", "disclosure", "product",
                            "address", "notice")):
        return None
    if "deposit" in n or "addition" in n:
        return 1
    if any(k in n for k in ("withdrawal", "check", "fee", "debit", "card")):
        return -1
    return None


def _period(lines: list[str]) -> tuple[date | None, date | None]:
    for ln in lines:
        m = _PERIOD_RE.search(ln)
        if m:
            s = date(int(m.group(3)), _MONTHS[m.group(1).lower()], int(m.group(2)))
            e = date(int(m.group(6)), _MONTHS[m.group(4).lower()], int(m.group(5)))
            return s, e
    return None, None


def _labelled_balances(lines: list[str], label: str) -> list[Decimal]:
    rx = re.compile(re.escape(label) + r"\s+" + _AMOUNT_RE, re.IGNORECASE)
    return [_dec(m.group(1)) for ln in lines if (m := rx.search(ln))]


def _select_balances(begins: list[Decimal], ends: list[Decimal],
                     net: Decimal) -> tuple[Decimal | None, Decimal | None]:
    """Pick the account's beginning/ending balance. A combined statement carries
    a CONSOLIDATED pair (checking + the cards on the statement) before the
    CHECKING pair, so prefer the (begin, end) pair that reconciles with the
    parsed net; fall back to the last of each (the account summary follows the
    consolidated one)."""
    for b in begins:
        for e in ends:
            if b + net == e:
                return b, e
    return (begins[-1] if begins else None), (ends[-1] if ends else None)


def _row_date(mm: int, dd: int, start: date | None, end: date | None) -> date | None:
    """Date the MM/DD row: pick the candidate year whose date falls in the
    statement period (handles a December row in a January statement)."""
    years = []
    if end:
        years.append(end.year)
    if start and (not end or start.year != end.year):
        years.append(start.year)
    for y in years or [date.today().year]:
        try:
            d = date(y, mm, dd)
        except ValueError:
            continue
        if not start or not end or start <= d <= end:
            return d
    # No candidate landed in-period (a stray/edge row); fall back to end year.
    try:
        return date((end or start or date.today()).year, mm, dd)
    except ValueError:
        return None


def _parse_transactions(lines: list[str], start: date | None,
                        end: date | None) -> list[StatementTxn]:
    """Walk the sections, collecting the signed transaction rows (folding each
    row's continuation lines into its description)."""
    txns: list[StatementTxn] = []
    sign: int | None = None
    pending: StatementTxn | None = None

    def flush() -> None:
        nonlocal pending
        if pending is not None:
            txns.append(pending)
            pending = None

    for ln in lines:
        m = _SECTION_RE.search(ln.lower())
        if m:
            flush()
            sign = _section_sign(m.group(1).strip())
            continue
        if "*end*" in ln.lower():
            flush()
            sign = None
            continue
        if sign is None:
            continue
        row = _ROW_START.match(ln) or _ROW_TRAILING_DATE.match(ln)
        if row:
            flush()
            d = _row_date(int(row.group("mm")), int(row.group("dd")), start, end)
            if d is None:
                continue
            pending = StatementTxn(
                posted_at=d, amount=sign * _dec(row.group("amt")),
                description=" ".join(row.group("desc").split()))
        elif pending is not None and ln.strip() \
                and not ln.strip().lower().startswith("total"):
            # A continuation line (trailing reference) for the current row.
            pending.description = (pending.description + " " + ln.strip()).strip()
    flush()
    return txns


def _split_segments(lines: list[str]) -> list[list[str]]:
    """Split the statement into per-product line blocks on the
    `*start*global product*` headers that open each product's sections. The
    preamble before the first header (period line, consolidated summary) is
    not a segment; a statement without the marker is one segment."""
    marks = [i for i, ln in enumerate(lines)
             if "*start*global product" in ln.lower()]
    if not marks:
        return [lines]
    return [lines[a:b] for a, b in zip(marks, marks[1:] + [len(lines)])]


def _parse_segment(lines: list[str], start: date | None,
                   end: date | None) -> StatementSegment:
    txns = _parse_transactions(lines, start, end)
    net = sum((t.amount for t in txns), Decimal(0))
    beginning, ending = _select_balances(
        _labelled_balances(lines, "Beginning Balance"),
        _labelled_balances(lines, "Ending Balance"), net)
    return StatementSegment(beginning_balance=beginning,
                            ending_balance=ending, transactions=txns)


def parse_statement_text(text: str) -> ParsedStatement:
    """Parse the `pdftotext -layout` text of one statement. Pure + unit-tested."""
    lines = text.splitlines()
    start, end = _period(lines)
    return ParsedStatement(
        period_start=start, period_end=end,
        segments=[_parse_segment(seg, start, end)
                  for seg in _split_segments(lines)])


def running_balances(seg: StatementSegment) -> list[tuple[StatementTxn, Decimal | None]]:
    """Each transaction paired with the running balance after it, reconstructed
    from the beginning balance in posted-date order. The end-of-day balance
    (the value the gold cash series uses) is exact regardless of intra-day
    order; None when no beginning balance was parsed."""
    if seg.beginning_balance is None:
        return [(t, None) for t in seg.transactions]
    bal = seg.beginning_balance
    out = []
    for t in sorted(seg.transactions, key=lambda t: t.posted_at):
        bal = bal + t.amount
        out.append((t, bal))
    return out


def segment_reconciles(seg: StatementSegment) -> bool:
    """True when beginning + Σ amounts == ending — a self-check that the parse
    caught every row of the segment. Unknown (True) when either balance is
    missing."""
    if seg.beginning_balance is None or seg.ending_balance is None:
        return True
    total = sum((t.amount for t in seg.transactions), Decimal(0))
    return seg.beginning_balance + total == seg.ending_balance


def balance_reconciles(stmt: ParsedStatement) -> bool:
    """True when every segment of the statement reconciles."""
    return all(segment_reconciles(s) for s in stmt.segments)


def parse_statement_pdf(path: Path) -> ParsedStatement:
    """Extract one statement PDF to text via `pdftotext -layout`, then parse it.
    Raises CalledProcessError if pdftotext (poppler) is unavailable."""
    out = subprocess.run(
        ["pdftotext", "-layout", str(path), "-"],
        check=True, capture_output=True, text=True)
    return parse_statement_text(out.stdout)
