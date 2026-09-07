#!/usr/bin/env python3
"""Parse an American Express card statement PDF into transactions + balances.

Statements are the ONLY source of card history older than the ~24 months the
activity JSON and every structured export reach (DESIGN.md §E), and the only
place a historic card balance is ever stated — no Amex channel carries a
running-balance column. `download` captures the PDFs, this reads them, and
`load` imports the rows strictly older than the account's export seam.

Layout facts, pinned from the captured statements — whose oldest and newest
renders agree — and re-checked against every one of them:

  * The document states only a `Closing Date MM/DD/YY`. There is NO opening
    date anywhere, so a period's START cannot come from its own document; the
    loader chains it from the previous period's end.
  * The ACCOUNT SUMMARY is a right-hand column interleaved line-by-line with
    unrelated legal text ("Late Payment Warning: …"), so `pdftotext -layout`
    yields lines carrying half a sentence and half a summary row. It is
    therefore read from a whitespace-collapsed blob, label by label, never
    line by line.
  * Summary amounts carry EXPLICIT signs: `Payments/Credits -$100.00`,
    `New Charges +$60.00`. The identity is
    previous + payments/credits + new charges + fees + interest == new balance,
    with the printed signs — so the addends are summed as they read.
  * The activity sections are bare headings on their own line (`New Charges`,
    `Fees`, `Interest Charged`, …). The same words ALSO appear in the summary
    followed by an amount, which is why a heading must match a line with
    nothing else on it.
  * `Card Ending N-XXXXX` sub-divides `New Charges` by card member. It is NOT
    a kind heading: rows under it keep the enclosing section's kind, which is
    what makes a supplementary card's charges land as charges. The rows all
    belong to the ONE account either way, so — unlike a chase combined deposit
    statement — nothing has to be attributed between accounts.
  * A row is `MM/DD/YY[*] description … $amount`, the amount last and carrying
    a currency symbol (unlike chase, where the symbol distinguishes a total
    from a row; here a total is excluded by not starting with a date). The `*`
    marks a posting date.
  * The trailing year-to-date block (`Total Fees in YYYY`, `Total Interest in
    YYYY`) and the APR table follow the activity and must not be read as part
    of it.

Amounts keep the DOCUMENT's convention — a charge or fee is POSITIVE (it
increases the balance owed) and a payment or credit is NEGATIVE — which is the
inverse of silver's. The loader converts, exactly as it does for the activity
JSON. Balances are the printed positive amount owed, matching the roster's.

The text parsing is pure and unit-tested; `parse_card_statement_pdf` is a thin
`pdftotext` wrapper around it.
"""
from __future__ import annotations

import re
import subprocess
from dataclasses import dataclass, field
from datetime import date
from decimal import Decimal, InvalidOperation
from pathlib import Path

# Section heading → the `kind` its rows are stamped with. Stamping the section
# is what keeps the statement era from collapsing to one unmapped kind: the
# deep era carries no spend category, so the section is the ONLY thing that
# separates a bill payment from a statement credit from a purchase.
#
# `Payments` and `Credits` are sub-headings inside `Payments and Credits`; the
# parent is mapped too, for a render that omits them.
CARD_SECTION_KINDS = {
    "PAYMENTS AND CREDITS": "STMT_PAYMENT",
    "PAYMENTS": "STMT_PAYMENT",
    "CREDITS": "STMT_CREDIT",
    "NEW CHARGES": "STMT_PURCHASE",
    "FEES": "STMT_FEE",
    "INTEREST CHARGED": "STMT_INTEREST",
}

# Each kind → the summary figure its rows must sum to. This is what makes a
# MISSING section visible: drop a heading from CARD_SECTION_KINDS and its rows
# silently keep the preceding section's kind, which leaves the whole-period
# total untouched and is invisible to a check that tests only the total.
# Payments and credits share one summary figure, so they are checked together.
CARD_SECTION_TOTALS = {
    "STMT_PAYMENT": "payments_credits",
    "STMT_CREDIT": "payments_credits",
    "STMT_PURCHASE": "new_charges",
    "STMT_FEE": "fees",
    "STMT_INTEREST": "interest_charged",
}

# The summary labels, verbatim → field name. Read from the collapsed blob.
CARD_SUMMARY_LABELS = {
    "previous_balance": "Previous Balance",
    "payments_credits": "Payments/Credits",
    "new_charges": "New Charges",
    "fees": "Fees",
    "interest_charged": "Interest Charged",
    "new_balance": "New Balance",
}
# The figures the summary identity adds up to `New Balance`.
CARD_SUMMARY_ADDENDS = ("payments_credits", "new_charges", "fees",
                        "interest_charged")

# `Closing Date MM/DD/YY` — the only date the period is stated by.
_CLOSING_DATE_RE = re.compile(r"Closing Date\s+(\d{2})/(\d{2})/(\d{2})")

# A summary amount: an explicit sign may sit on either side of the symbol.
_SUMMARY_AMOUNT = r"[+-]?\$[+-]?[\d,]+\.\d{2}"

# An activity row: MM/DD/YY, an optional posting-date star, a description, and
# a currency amount as the last token.
_CARD_ROW_RE = re.compile(
    r"^\s*(?P<mm>\d{2})/(?P<dd>\d{2})/(?P<yy>\d{2})(?P<star>\*?)\s+"
    r"(?P<rest>\S.*?)\s+(?P<amt>-?\$-?[\d,]+\.\d{2})\s*$")

# Where the activity ends: the year-to-date block and the APR table that
# follow it. `Total Interest Charged for this Period` is the last thing INSIDE
# the activity, and is excluded from the row pattern by having no leading date.
_ACTIVITY_END_RE = re.compile(
    r"^\s*(?:Total (?:Fees|Interest) in \d{4}\b"
    r"|\d{4} Totals Year-to-Date\b"
    r"|Interest Charge Calculation\b)")

# A section heading is the words alone on their line — the same words appear in
# the summary followed by an amount.
_HEADING_RE = re.compile(r"^\s*([A-Za-z][A-Za-z /]*[A-Za-z])\s*$")


@dataclass
class StatementTxn:
    """One activity row, in the DOCUMENT's convention: a charge is positive, a
    payment or credit negative."""
    when: date
    description: str
    amount: Decimal
    kind: str
    posting_date: bool = False


@dataclass
class ParsedCardStatement:
    """One card statement: its closing date, its summary figures and its
    activity rows. `period_start` is deliberately absent — the document never
    states one (see the module docstring); the loader chains it."""
    period_end: date | None = None
    previous_balance: Decimal | None = None
    payments_credits: Decimal | None = None
    new_charges: Decimal | None = None
    fees: Decimal | None = None
    interest_charged: Decimal | None = None
    new_balance: Decimal | None = None
    transactions: list[StatementTxn] = field(default_factory=list)


def _dec(s: str) -> Decimal | None:
    """A printed amount → Decimal, keeping its sign. Handles a sign on either
    side of the currency symbol (`-$5.00`, `$-5.00`, `+$5.00`)."""
    s = s.strip()
    neg = s.startswith("-") or "$-" in s
    plain = re.sub(r"[+\-$,]", "", s)
    try:
        value = Decimal(plain)
    except InvalidOperation:
        return None
    return -value if neg else value


def _norm(line: str) -> str:
    """A line reduced to the form the layout facts are stated in: whitespace
    runs collapsed, ends trimmed."""
    return re.sub(r"\s+", " ", line).strip()


def pdf_to_text(path: Path) -> str:
    """`pdftotext -layout` over the whole document. -layout is what preserves
    the column structure every pattern here depends on."""
    out = subprocess.run(["pdftotext", "-layout", str(path), "-"],
                         capture_output=True, check=True)
    return out.stdout.decode("utf-8", errors="replace")


def _closing_date(text: str) -> date | None:
    """The statement's closing date. It is printed on several pages; they
    agree, so the first is taken."""
    m = _CLOSING_DATE_RE.search(text)
    if not m:
        return None
    mm, dd, yy = (int(g) for g in m.groups())
    try:
        return date(2000 + yy, mm, dd)
    except ValueError:
        return None


def _summary(text: str) -> dict:
    """The ACCOUNT SUMMARY figures, read from a whitespace-collapsed blob.

    The summary is a right-hand column interleaved with unrelated legal text,
    so a line often carries half a sentence and half a summary row; searching
    the blob for `<label> <amount>` finds each figure wherever the layout put
    it. Each label is searched independently over the unmutated blob, so the
    order they are tried in cannot matter; what a label must be is
    unambiguous on its own — not a substring of another, and not something
    that appears followed by an amount before the summary does, since the
    first `<label> <amount>` in the document wins.
    """
    blob = _norm(text)
    out: dict = {}
    for field_name, label in CARD_SUMMARY_LABELS.items():
        pattern = re.escape(label) + r"\s+(" + _SUMMARY_AMOUNT + r")"
        m = re.search(pattern, blob)
        out[field_name] = _dec(m.group(1)) if m else None
    return out


def _row_date(mm: int, dd: int, yy: int) -> date | None:
    try:
        return date(2000 + yy, mm, dd)
    except ValueError:
        return None


def parse_card_statement_text(text: str) -> ParsedCardStatement:
    """Parse the extracted text of one card statement.

    Rows are stamped with the enclosing section's kind. A heading that is not a
    known section (`Detail`, `Card Ending N-XXXXX`) leaves the current kind
    alone — which is what keeps a supplementary card's charges filed as
    charges — and the section-by-section reconciliation below is what catches a
    section this parser genuinely does not know."""
    parsed = ParsedCardStatement(period_end=_closing_date(text))
    for name, value in _summary(text).items():
        setattr(parsed, name, value)

    # `kind` doubles as the in-activity flag: it is set only by a known
    # section heading and cleared where the activity ends.
    kind: str | None = None
    for raw in text.split("\n"):
        line = _norm(raw)
        if not line:
            continue
        if _ACTIVITY_END_RE.match(raw):
            kind = None
            continue
        heading = _HEADING_RE.match(line)
        if heading:
            found = CARD_SECTION_KINDS.get(heading.group(1).strip().upper())
            if found:
                kind = found
            continue
        if kind is None:
            continue
        m = _CARD_ROW_RE.match(raw)
        if not m:
            continue
        when = _row_date(int(m["mm"]), int(m["dd"]), int(m["yy"]))
        amount = _dec(m["amt"])
        if when is None or amount is None:
            continue
        # The description is the row's leading text; the trailing columns a
        # statement prints beside it (a reference, a city/state) are dropped
        # by taking only what precedes a run of two or more spaces.
        description = re.split(r"\s{2,}", m["rest"].strip())[0].strip()
        parsed.transactions.append(StatementTxn(
            when=when, description=description, amount=amount, kind=kind,
            posting_date=bool(m["star"])))
    return parsed


def summary_reconciles(parsed: ParsedCardStatement) -> bool:
    """The printed summary adds up: previous + the four addends == new balance,
    with the signs as printed. A statement whose own summary does not add up
    was mis-read, and nothing from it may be imported."""
    if parsed.previous_balance is None or parsed.new_balance is None:
        return False
    total = parsed.previous_balance
    for field_name in CARD_SUMMARY_ADDENDS:
        value = getattr(parsed, field_name)
        if value is None:
            return False
        total += value
    return total == parsed.new_balance


def rows_reconcile(parsed: ParsedCardStatement) -> bool:
    """Every section's rows sum to the summary figure printed for it.

    Checked section by section, not as one total: rows under a heading this
    parser does not know silently inherit the preceding section's kind, which
    leaves the whole-period total untouched — and a section netting to zero
    moves no total at all. Payments and credits share one summary figure and
    are therefore checked together.
    """
    if not summary_reconciles(parsed):
        return False
    sums: dict[str, Decimal] = {}
    for txn in parsed.transactions:
        target = CARD_SECTION_TOTALS.get(txn.kind)
        if target is None:
            return False        # a kind with no summary figure to check
        sums[target] = sums.get(target, Decimal(0)) + txn.amount
    for target in set(CARD_SECTION_TOTALS.values()):
        printed = getattr(parsed, target)
        if printed is None:
            return False
        if sums.get(target, Decimal(0)) != printed:
            return False
    return True


def parse_card_statement_pdf(path: Path) -> ParsedCardStatement:
    """Parse one card statement PDF."""
    return parse_card_statement_text(pdf_to_text(path))
