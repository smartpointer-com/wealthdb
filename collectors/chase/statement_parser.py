#!/usr/bin/env python3
"""Parse a Chase statement PDF into transactions + balances.

Two layouts live here, one per product, sharing only the `pdftotext -layout`
extraction and the MM/DD row dating. `parse_statement_text` reads the
**deposit** statement, `parse_card_statement_text` the **credit-card** one;
see the card section below for why neither can read the other's document.

Deposit statements
------------------

Chase renders them with the OpenText engine, which wraps each block in
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

Card statements
---------------

Card statements carry none of that machinery — no OpenText markers, one
account summary, one activity block, one card per document — so they are
parsed by their own entry point (see the "Card statements" section for the
layout facts and the hazards they impose). They are also the only place a
card's historic balance is ever stated: the card export has no running-balance
column, so the statement's `Previous Balance` / `New Balance` pair is what
anchors a reconstructed card balance series.

Both parsers keep their document's own sign convention — a deposit row is
signed by its section, a card row prints its own sign — and the loader
converts to silver's convention. Statements are the only source of
transactions older than Chase's 24-month export cap (DESIGN.md §E):
`download` captures the PDFs, this reads them, and the loader attributes
deposit segments to accounts by balance chaining (a card document needs no
attribution — it belongs to one card by construction). The text parsing is
pure and unit-tested; `parse_statement_pdf` / `parse_card_statement_pdf` are
thin `pdftotext` wrappers around it.
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
    amount: Decimal        # signed as the document prints it (see the module
                           # docstring): deposit + addition / - withdrawal,
                           # card + charge / - credit.
    description: str
    # Which activity section the row was printed under, on the layouts that
    # have named sections worth keeping (the card statement's
    # `CARD_SECTION_KINDS`). None on a deposit row, whose section only ever
    # supplied the sign.
    kind: str | None = None


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


def pdf_to_text(path: Path) -> str:
    """One statement PDF as `pdftotext -layout` text — the column geometry
    both layouts are parsed from. Raises CalledProcessError if pdftotext
    (poppler) is unavailable."""
    return subprocess.run(
        ["pdftotext", "-layout", str(path), "-"],
        check=True, capture_output=True, text=True).stdout


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
    caught every row of the segment.

    False when either balance is missing: a segment with no balance pair has
    nothing to check against, and a segment that reached here already knowing
    its parse is degraded is exactly the one whose rows must not be trusted —
    a section heading that drifted takes its whole section's rows with it
    (`_section_sign` returns None for a name it does not know), and the sum
    check is what would have caught it. This is the card path's rule
    (`card_summary_reconciles`), so the two products answer the same question
    the same way. `load` logs and counts the skip."""
    if seg.beginning_balance is None or seg.ending_balance is None:
        return False
    total = sum((t.amount for t in seg.transactions), Decimal(0))
    return seg.beginning_balance + total == seg.ending_balance


def parse_statement_pdf(path: Path) -> ParsedStatement:
    """Extract one statement PDF to text via `pdftotext -layout`, then parse it.
    Raises CalledProcessError if pdftotext (poppler) is unavailable."""
    return parse_statement_text(pdf_to_text(path))


# ============================================================
# Card statements
# ============================================================
#
# A card statement is a different document, not a variant of the deposit one,
# and every deposit assumption fails on it:
#
#   * NO OpenText `*start*`/`*end*` markers, one ACCOUNT SUMMARY and one
#     ACCOUNT ACTIVITY block, one card per document — so the segment splitting
#     and balance chaining that attribute a combined deposit statement to an
#     account are not just unnecessary but meaningless here. A card statement
#     belongs to the card whose directory it was captured under.
#   * The period line is `Opening/Closing Date  MM/DD/YY - MM/DD/YY`, which
#     the deposit "… through …" period regex does not match at all.
#   * Rows print their own sign (credits negative, purchases and fees bare
#     positive), so the deposit `_section_sign` must never be applied — it
#     would negate the credits section a second time.
#
# The layout is stable across statement generations: the same section names,
# the same summary labels, the same column geometry.
#
# Hazards the row regex is shaped around:
#
#   * A page break repeats `ACCOUNT ACTIVITY (CONTINUED)` and the column
#     header but NOT the section heading, so section state has to survive it
#     or every continued row lands unsectioned and is dropped.
#   * A row's amount can drop the leading zero (`.99`), so the amount pattern
#     admits an empty integer part.
#   * A foreign purchase prints two continuation lines under it, and a foreign
#     transaction fee one: the first of the pair STARTS with the MM/DD date
#     the currency converted on, and the fee's ENDS in the original amount.
#     Each is a phantom row for a parser that tests only one end, so a row has
#     to satisfy BOTH (starts with MM/DD and ends in a bare amount).
#     Continuation lines are dropped rather than folded into the description:
#     between two rows sits the page furniture of a page break (footer, name,
#     statement date), indistinguishable from a genuine continuation once a
#     row's own line has been consumed.
#   * `TOTAL FEES FOR THIS PERIOD` / `TOTAL INTEREST FOR THIS PERIOD` sit
#     INSIDE their transaction sections; they carry a `$` and no leading date,
#     which the row pattern excludes on both counts.
#   * Some renders split a summary label across two lines around a stray
#     backtick (`Balance` / `` `      Transfers ``), and another render
#     generation sprinkles `Table Summary` / `empty cell` artifacts through
#     the page. The summary is therefore read from a whitespace-collapsed,
#     backtick-stripped blob of the ACCOUNT SUMMARY block rather than line
#     by line; the artifacts match neither a section heading nor a row and
#     fall away.

# Section heading → the `kind` its rows are stamped with. Stamping the
# section is what keeps the statement era from collapsing to one unmapped
# kind: without it a statement-era payment is indistinguishable from a
# purchase, which both loses the spend semantics and lets a card payment be
# double-counted against the deposit account that funded it.
CARD_SECTION_KINDS = {
    "PAYMENTS AND OTHER CREDITS": "STMT_PAYMENT",
    "PURCHASE": "STMT_PURCHASE",              # singular, unlike the APR table
    "FEES CHARGED": "STMT_FEE",
    "INTEREST CHARGED": "STMT_INTEREST",
}

# Each section's `kind` → the ACCOUNT SUMMARY figure its rows must sum to.
# This is what makes a MISSING section visible: drop a heading from
# `CARD_SECTION_KINDS` and its rows keep the preceding section's kind, which
# leaves the whole-period total untouched and is invisible to a check that
# only tests the total (worse, a section netting to zero moves no total at
# all). Every mapped kind must appear here — `card_rows_reconcile` tests
# section by section, so an unmapped one would go unchecked.
CARD_SECTION_TOTALS = {
    "STMT_PAYMENT": "payments_credits",
    "STMT_PURCHASE": "purchases",
    "STMT_FEE": "fees_charged",
    "STMT_INTEREST": "interest_charged",
}

# The summary addends NO section maps to. Chase prints a cash advance and a
# balance transfer as their own activity sections, and neither heading is in
# CARD_SECTION_KINDS — so rather than guess at headings this parser cannot
# check itself against, the reconciliation ASSERTS both figures are zero. A
# non-zero one means a section is being printed that nothing here parses, and
# the statement is refused rather than imported short.
_CARD_UNMAPPED_SUMMARY_FIELDS = ("cash_advances", "balance_transfers")

# Where the activity block ends. `INTEREST CHARGES` (plural) opens the APR
# table and is NOT the `INTEREST CHARGED` transaction section; the rest are
# the trailing blocks — the year-to-date totals, the promotional block and the
# account-message block — any of which can interrupt the activity across a
# page break.
_CARD_ACTIVITY_END_RE = re.compile(
    r"^(?:INTEREST CHARGES|IMPORTANT NEWS|YOUR ACCOUNT MESSAGES"
    r"|\d{4} Totals Year-to-Date)\b")

_CARD_ACTIVITY = "ACCOUNT ACTIVITY"
_CARD_CONTINUED = "(CONTINUED)"

# `Opening/Closing Date  MM/DD/YY - MM/DD/YY` — two-digit years, hyphen.
_CARD_PERIOD_RE = re.compile(
    r"Opening/Closing Date\s+(\d{2})/(\d{2})/(\d{2})\s*-\s*"
    r"(\d{2})/(\d{2})/(\d{2})")

# A summary figure always carries a currency symbol and may carry an explicit
# sign on either side of it (`-$40.00`, `+$79.99`); the credit-line figures
# print whole dollars with no decimals.
_CARD_SUMMARY_AMOUNT = r"[+-]?\$[+-]?[\d,]+(?:\.\d{2})?"

# An activity row: MM/DD, a description, and a bare amount as the last token.
# The amount carries no currency symbol (which is what excludes the section
# TOTAL lines) and may drop its leading zero.
_CARD_ROW_RE = re.compile(
    r"^\s*(?P<mm>\d{2})/(?P<dd>\d{2})\s+(?P<desc>\S.*?)\s+"
    r"(?P<amt>-?[\d,]*\.\d{2})\s*$")

# The ACCOUNT SUMMARY block, verbatim labels → field name.
CARD_SUMMARY_LABELS = {
    "previous_balance": "Previous Balance",
    "payments_credits": "Payment, Credits",
    "purchases": "Purchases",
    "cash_advances": "Cash Advances",
    "balance_transfers": "Balance Transfers",
    "fees_charged": "Fees Charged",
    "interest_charged": "Interest Charged",
    "new_balance": "New Balance",
}
# The figures the summary identity adds up to `New Balance`.
_CARD_SUMMARY_ADDENDS = tuple(f for f in CARD_SUMMARY_LABELS
                              if f != "new_balance")
# The summary block runs from its heading to the Opening/Closing Date line;
# this bounds the search for that line so a document without one cannot pull
# the whole statement into the blob.
_CARD_SUMMARY_MAX_LINES = 30


@dataclass
class ParsedCardStatement:
    """One card statement: its period, its ACCOUNT SUMMARY figures and its
    activity rows. Amounts stay in the document's own convention — a purchase
    or fee is POSITIVE (it increases the balance owed) and a payment or credit
    is NEGATIVE — which is the inverse of the export's. Balances are the
    printed positive amount owed, matching the roster's."""
    period_start: date | None = None
    period_end: date | None = None
    previous_balance: Decimal | None = None
    payments_credits: Decimal | None = None
    purchases: Decimal | None = None
    cash_advances: Decimal | None = None
    balance_transfers: Decimal | None = None
    fees_charged: Decimal | None = None
    interest_charged: Decimal | None = None
    new_balance: Decimal | None = None
    transactions: list[StatementTxn] = field(default_factory=list)


def _card_norm(line: str) -> str:
    """A statement line reduced to the form the layout facts are stated in:
    backticks (a pdftotext artifact that splits labels) dropped, whitespace
    runs collapsed, ends trimmed."""
    return re.sub(r"\s+", " ", line.replace("`", " ")).strip()


def _card_summary_blob(lines: list[str]) -> str:
    """The ACCOUNT SUMMARY block as one normalised line. Joining the block
    rejoins the labels pdftotext split across lines around a backtick, so
    `Balance` + `` `      Transfers  $0.00 `` reads as
    `Balance Transfers $0.00` like every other render."""
    start = next((i for i, ln in enumerate(lines) if "ACCOUNT SUMMARY" in ln),
                 None)
    if start is None:
        return ""
    window = range(start, min(len(lines), start + _CARD_SUMMARY_MAX_LINES))
    period = next((i for i in window if "Opening/Closing Date" in lines[i]),
                  None)
    end = (period + 1) if period is not None else window.stop
    return _card_norm(" ".join(lines[start:end]))


def _card_labelled(blob: str, label: str) -> Decimal | None:
    """The figure printed against `label` in the summary blob, or None when
    the label carries no figure (the next token is not an amount)."""
    m = re.search(re.escape(label) + r"\s+(" + _CARD_SUMMARY_AMOUNT + r")",
                  blob)
    return _dec(m.group(1).replace("+", "")) if m else None


def _card_row_date(mm: int, dd: int, start: date, end: date) -> date | None:
    """Date a card row's MM/DD. The printed date is the TRANSACTION date, which
    routinely falls a few days before the period it is billed in, so an
    in-period test alone cannot resolve the year — instead the candidate year
    closest to the period wins. That resolves the December-in-January wrap
    (12/20 on a Dec-Jan statement is last year's) and the ordinary
    just-before-the-period row (06/28 on a 06/29-07/28 statement is this
    year's) with one rule."""
    best: tuple[int, date] | None = None
    for y in {start.year - 1, start.year, end.year}:
        try:
            d = date(y, mm, dd)
        except ValueError:
            continue                            # 02/29 in a non-leap year
        dist = 0 if start <= d <= end else \
            min(abs((d - start).days), abs((d - end).days))
        if best is None or dist < best[0]:
            best = (dist, d)
    return best[1] if best else None


def _card_period(lines: list[str], blob: str) -> tuple[date | None, date | None]:
    m = _CARD_PERIOD_RE.search(blob)
    if not m:
        for ln in lines:
            m = _CARD_PERIOD_RE.search(_card_norm(ln))
            if m:
                break
    if not m:
        return None, None
    return (date(2000 + int(m.group(3)), int(m.group(1)), int(m.group(2))),
            date(2000 + int(m.group(6)), int(m.group(4)), int(m.group(5))))


def _card_transactions(lines: list[str], start: date,
                       end: date) -> list[StatementTxn]:
    """The activity rows, each stamped with the section it was printed under.

    Section state deliberately OUTLIVES the end of the activity block: a page
    break can interpose a totals or account-message block between two rows of
    the same section, and `ACCOUNT ACTIVITY (CONTINUED)` resumes without
    reprinting the heading. Only a fresh, uncontinued `ACCOUNT ACTIVITY`
    clears it."""
    txns: list[StatementTxn] = []
    in_activity = False
    kind: str | None = None
    for ln in lines:
        u = _card_norm(ln)
        if u.startswith(_CARD_ACTIVITY):
            in_activity = True
            if _CARD_CONTINUED not in u:
                kind = None
            continue
        if _CARD_ACTIVITY_END_RE.match(u):
            in_activity = False
            continue
        if u in CARD_SECTION_KINDS:
            kind = CARD_SECTION_KINDS[u]
            continue
        if not in_activity or kind is None:
            continue
        row = _CARD_ROW_RE.match(ln)
        if not row:
            continue                    # continuation, artifact or furniture
        d = _card_row_date(int(row.group("mm")), int(row.group("dd")),
                           start, end)
        if d is None:
            # MM/DD is no date in any candidate year (13/02, 02/30): not a
            # row. A real row lost this way fails the row identity downstream.
            continue
        txns.append(StatementTxn(
            posted_at=d, amount=_dec(row.group("amt")),
            description=" ".join(row.group("desc").split()), kind=kind))
    return txns


def parse_card_statement_text(text: str) -> ParsedCardStatement:
    """Parse the `pdftotext -layout` text of one card statement. Pure +
    unit-tested. A statement whose period cannot be read yields no
    transactions (the MM/DD rows would be undatable) but still reports
    whatever summary figures were found."""
    lines = text.splitlines()
    blob = _card_summary_blob(lines)
    start, end = _card_period(lines, blob)
    stmt = ParsedCardStatement(period_start=start, period_end=end)
    for fieldname, label in CARD_SUMMARY_LABELS.items():
        setattr(stmt, fieldname, _card_labelled(blob, label))
    if start is not None and end is not None:
        stmt.transactions = _card_transactions(lines, start, end)
    return stmt


def card_summary_reconciles(stmt: ParsedCardStatement) -> bool:
    """True when the printed summary is internally consistent:
    Previous Balance + Payment,Credits + Purchases + Cash Advances +
    Balance Transfers + Fees Charged + Interest Charged == New Balance.
    False when any of those figures is missing — a summary that did not parse
    is not a summary that agrees."""
    parts = [getattr(stmt, f) for f in _CARD_SUMMARY_ADDENDS]
    if any(p is None for p in parts) or stmt.new_balance is None:
        return False
    return sum(parts, Decimal(0)) == stmt.new_balance


def card_rows_reconcile(stmt: ParsedCardStatement) -> bool:
    """True when the printed rows account for the whole period, section by
    section. Three identities, all of which must hold:

      * the WHOLE PERIOD — Σ row amounts == New Balance − Previous Balance;
      * each MAPPED SECTION — Σ the rows stamped with a kind == the summary
        figure printed for it (`CARD_SECTION_TOTALS`);
      * each UNMAPPED ADDEND — cash advances and balance transfers are zero
        (`_CARD_UNMAPPED_SUMMARY_FIELDS`).

    The whole-period identity alone is blind to a section this parser does
    not know about, which is the failure mode a layout change actually
    produces: drop a heading and its rows silently inherit the preceding
    section's kind, leaving the total untouched, and a section that nets to
    zero moves no total at all. The per-section identities catch the
    inherited rows (both sections' sums move), and the zero assertions catch
    a section that was never mapped in the first place.

    False when any figure the identities need is missing — an unparsed
    summary cannot agree with anything. This is the check that the parse
    missed no row and invented none; a statement that fails it must not
    contribute transactions."""
    fields = ("previous_balance", "new_balance", *CARD_SECTION_TOTALS.values(),
              *_CARD_UNMAPPED_SUMMARY_FIELDS)
    if any(getattr(stmt, f) is None for f in fields):
        return False
    net = sum((t.amount for t in stmt.transactions), Decimal(0))
    if net != stmt.new_balance - stmt.previous_balance:
        return False
    for kind, fieldname in CARD_SECTION_TOTALS.items():
        section = sum((t.amount for t in stmt.transactions if t.kind == kind),
                      Decimal(0))
        if section != getattr(stmt, fieldname):
            return False
    return all(getattr(stmt, f) == 0 for f in _CARD_UNMAPPED_SUMMARY_FIELDS)


def parse_card_statement_pdf(path: Path) -> ParsedCardStatement:
    """Extract one card statement PDF to text via `pdftotext -layout`, then
    parse it. Raises CalledProcessError if pdftotext (poppler) is
    unavailable."""
    return parse_card_statement_text(pdf_to_text(path))
