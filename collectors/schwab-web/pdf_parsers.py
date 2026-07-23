"""
Parsers for Schwab monthly brokerage statement PDFs.

The primary use case is reconstructing transaction history for
ACCOUNTS THAT HAVE BEEN CLOSED — closed accounts disappear from
the Transaction History page, so archived monthly statements are
the only retrievable record (fetchable via download.py while any
open account shares the login).

For active accounts the Transaction History page is the
authoritative source; PDF parsing remains for cross-checks and
backfill.

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

import logging
import re
from dataclasses import dataclass, field, asdict
from datetime import date, datetime

from collectorkit.pdf import extract_text_pdfium as _extract_pdf_text
from numparse import parse_amount

log = logging.getLogger("schwab-web.pdf_parsers")


# ============================================================
# Statement-period header
# ============================================================

# 2020+: "February 1-28, 2026" or "January 30-February 5, 2026"
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
# 2017-2019: "Statement Period: December 1, 2019 to December 31, 2019"
# (two full dates separated by " to ").
_PERIOD_RE_LONG = re.compile(
    r"(?P<m1>January|February|March|April|May|June|July|August|"
    r"September|October|November|December)\s+"
    r"(?P<d1>\d{1,2}),\s*"
    r"(?P<y1>\d{4})\s+to\s+"
    r"(?P<m2>January|February|March|April|May|June|July|August|"
    r"September|October|November|December)\s+"
    r"(?P<d2>\d{1,2}),\s*"
    r"(?P<y2>\d{4})"
)

_MONTH_NUMS = {
    "January": 1, "February": 2, "March": 3, "April": 4,
    "May": 5, "June": 6, "July": 7, "August": 8,
    "September": 9, "October": 10, "November": 11, "December": 12,
}


# ============================================================
# Account-registration header
# ============================================================
#
# Every Schwab statement names the account registration ("Schwab
# One® Account", "Contributory IRA", "Schwab One® Custodial
# Account", "Education Savings", etc.) at the very top of page
# 1, adjacent to the account number. This is the only Schwab-
# side signal for the per-account tax wrapper — the Trader API
# doesn't surface it (see schwab-api DESIGN.md §4.10).
# wealthdb's gold adapter maps the verbatim string we return
# here to its canonical `tax_wrapper` enum.
#
# Three layout eras to handle, each with a distinct anchor:
#
#   2025+         "<LABEL> of Account Nickname"      (single line)
#   2020-2024     "<LABEL> of"                       (label only;
#                                                    holder name on
#                                                    the next line)
#   2017-2019     bare "<LABEL>" immediately above
#                 "Account Number: <NNNN-NNNN>"      (the colon-and-
#                                                    value form is
#                                                    unique to this
#                                                    era — newer
#                                                    statements put
#                                                    "Account Number"
#                                                    and the value on
#                                                    separate lines)

_REG_RE_2025 = re.compile(
    r"^(?P<reg>[A-Za-z][^\n]*?)\s+of\s+Account\s+Nickname\s*$",
)
_REG_RE_2020 = re.compile(
    r"^(?P<reg>[A-Za-z][^\n]*?)\s+of\s*$",
)
_REG_ACCOUNT_NUMBER_INLINE_RE = re.compile(
    r"^Account\s+Number:\s*\S+",
)
# Substrings that mark a plausible registration label. Used to
# filter out lines that happen to end in " of" (e.g. disclosure
# prose) but aren't the registration header. The Schwab labels
# observed so far, plus the catalogue of
# registrations the wealthdb adapter expects to see — see
# DESIGN.md §8.
_REG_KEYWORDS = (
    "Account",
    "IRA",
    "ESA",
    "Education Savings",
    "Coverdell",
    "Custodial",
    "Trust",
    "401",
    "529",
    "Annuity",
)
# Schwab decorates some labels with a registered-mark glyph or
# stray whitespace that doesn't carry information. Stripping
# keeps the column values comparable across statements (the
# 2024 statement uses "Schwab One® International Account" while
# the 2026 one uses "Schwab One International® Account" — same
# wrapper, but the ® migrates by one word).
_REG_TRIM_RE = re.compile(r"\s+")


def parse_account_registration(text: str) -> str | None:
    """Extract the account-registration label from the top of
    page 1 of a Schwab statement. Returns the raw label string
    Schwab printed ("Schwab One® International Account",
    "Contributory IRA", "Schwab One® Custodial Account",
    "Education Savings", ...) verbatim — extended with a
    " (UTMA)" / " (UGMA)" suffix for custodial accounts, since
    Schwab's header line "Schwab One® Custodial Account" is
    the same string for both sub-types but the holder block
    immediately below carries a "<state>UTMA" / "<state>UGMA"
    marker that resolves the ambiguity. Silver does this
    refinement so the wealthdb gold adapter doesn't need to
    re-read bronze.

    Returns None when no recognisable header is found; the
    loader leaves the column NULL in that case.
    """
    # Constrain the scan to the top of the document — the
    # registration header always sits in the first ~80 lines on
    # page 1. This also avoids accidental matches against later
    # disclosure prose that happens to end in " of".
    lines = [ln.strip() for ln in text.split("\n")[:80]]

    label = _detect_registration_label(lines)
    if label is None:
        return None
    return _augment_custodial_subtype(label, lines)


def _detect_registration_label(lines: list[str]) -> str | None:
    """Scan the first ~80 lines for the registration header
    line, trying each layout era's anchor in turn. Returns the
    raw label or None."""
    # Era 1 (2025+): "<LABEL> of Account Nickname"
    for ln in lines:
        if not ln:
            continue
        m = _REG_RE_2025.match(ln)
        if m:
            return _clean_registration(m.group("reg"))

    # Era 2 (2020-2024): "<LABEL> of" (the holder name follows
    # on the next line). Many lines on page 1 end in " of" so
    # we additionally require a registration keyword to land
    # on the legitimate header.
    for ln in lines:
        if not ln:
            continue
        m = _REG_RE_2020.match(ln)
        if not m:
            continue
        cand = m.group("reg").strip()
        if any(kw in cand for kw in _REG_KEYWORDS):
            return _clean_registration(cand)

    # Era 3 (2017-2019): bare "<LABEL>" immediately above an
    # inline "Account Number: <value>" line.
    for i, ln in enumerate(lines):
        if not _REG_ACCOUNT_NUMBER_INLINE_RE.match(ln):
            continue
        # Scan backwards up to 5 lines for a plausible
        # registration label.
        for j in range(i - 1, max(-1, i - 5), -1):
            prev = lines[j]
            if prev and any(kw in prev for kw in _REG_KEYWORDS):
                return _clean_registration(prev)
    return None


def _augment_custodial_subtype(label: str, lines: list[str]) -> str:
    """If `label` is the generic Schwab custodial registration,
    promote it to "<label> (UTMA)" / "<label> (UGMA)" using the
    account-holder-block markers Schwab prints in the top-left
    corner of every statement ("<NAME> CUST FOR / <NAME>
    UCAUTMA / UNTIL AGE 18", where "UCAUTMA" = California UTMA,
    "NYUTMA" = New York UTMA, etc.; UGMA accounts use the
    matching "<state>UGMA" marker).

    The header line "Schwab One® Custodial Account of" is the
    same string for UTMA and UGMA accounts, so it's the only
    place in silver where we look BEYOND the header for a
    resolution. Doing it here keeps wealthdb's gold adapter
    out of bronze and gives it a single column to key off.

    If neither marker is present (defensive — no Schwab
    statement we've observed lacks one for a custodial
    account) or the registration isn't custodial, `label` is
    returned unchanged.
    """
    if "Custodial" not in label:
        return label
    # Substring scan: matches CAUTMA, NYUTMA, UCAUTMA, plain UTMA, etc.
    # UTMA is checked first explicitly; there's no UCAUGMA-substring-
    # of-UCAUTMA collision but the ordering keeps intent obvious.
    for ln in lines:
        u = ln.upper()
        if "UTMA" in u:
            return f"{label} (UTMA)"
        if "UGMA" in u:
            return f"{label} (UGMA)"
    return label


def _clean_registration(s: str) -> str:
    """Collapse internal whitespace runs into single spaces but
    otherwise return the label verbatim — the ® / & / parenthesis
    characters are part of Schwab's official label and the
    wealthdb adapter keys off the exact text."""
    return _REG_TRIM_RE.sub(" ", s).strip()


# ============================================================
# Full account number (page-1 header)
# ============================================================
#
# Every statement prints the full account number ("NNNN-NNNN")
# near the top of page 1, next to the registration header above.
# It is the only place the web feed carries more than the 3-to-5-
# digit UI suffix, which makes it the bridge key between this
# silver and schwab-api's (whose account_external_id is Schwab's
# opaque hashValue) — see INTEROP.md §1. The same two anchor
# shapes as the registration eras:
#
#   2020+         "Account Number" label line, the NNNN-NNNN
#                 value on the following line
#   2017-2019     inline "Account Number: NNNN-NNNN"
#
# The value is returned exactly as printed (dash kept). The gold
# bridge normalises both sides to digits-only when joining
# against schwab-api's undashed accountNumber, so silver stores
# the verbatim form.

_ACCT_NUM_INLINE_RE = re.compile(
    r"^\s*account\s+number\s*:?\s*(?P<num>\d{4}-\d{4})\b",
    re.IGNORECASE,
)
_ACCT_NUM_LABEL_RE = re.compile(
    r"^\s*account\s+number\s*:?\s*$",
    re.IGNORECASE,
)
_ACCT_NUM_VALUE_RE = re.compile(r"^\s*(?P<num>\d{4}-\d{4})\s*$")


def parse_account_number(text: str) -> str | None:
    """Extract the full account number from the top of page 1 of
    a Schwab statement, verbatim ("NNNN-NNNN", dash kept).

    Handles the inline colon form (2017-2019) and the label-and-
    value-on-consecutive-lines form (2020+; one intervening line
    tolerated for layout jitter). Returns None when neither shape
    appears in the first ~80 lines — a header layout the anchors
    don't recognise leaves the account without a bridge key, and
    the gold layer falls back to suffix matching.
    """
    lines = [ln.strip() for ln in text.split("\n")[:80]]
    for i, ln in enumerate(lines):
        m = _ACCT_NUM_INLINE_RE.match(ln)
        if m:
            return m.group("num")
        if _ACCT_NUM_LABEL_RE.match(ln):
            for nxt in lines[i + 1:i + 3]:
                v = _ACCT_NUM_VALUE_RE.match(nxt)
                if v:
                    return v.group("num")
    return None


def parse_statement_account_number(path) -> str | None:
    """Header-only variant of parse_statement_pdf: extract the
    document text and return just the full account number. The
    loader's reconcile pass uses this to re-read statement
    headers from bronze — for rows ingested before the
    account-number parser existed, and to re-verify a freshly
    set key against every statement — without re-running the
    row parsers."""
    return parse_account_number(_extract_pdf_text(path))


def parse_statement_period(text: str) -> tuple[date, date] | None:
    """Return (start_date, end_date) for the first period header
    found in `text` (the entire PDF text or page 1 will do).

    Handles both header conventions Schwab has shipped:
    - 2020+ short form: "February 1-28, 2026"
    - 2017-2019 long form: "December 1, 2019 to December 31, 2019"
    """
    m = _PERIOD_RE.search(text)
    if m:
        year = int(m.group("year"))
        m1 = _MONTH_NUMS[m.group("m1")]
        m2 = _MONTH_NUMS[m.group("m2") or m.group("m1")]
        d1 = int(m.group("d1"))
        d2 = int(m.group("d2"))
        return date(year, m1, d1), date(year, m2, d2)
    m = _PERIOD_RE_LONG.search(text)
    if m:
        y1 = int(m.group("y1"))
        y2 = int(m.group("y2"))
        m1 = _MONTH_NUMS[m.group("m1")]
        m2 = _MONTH_NUMS[m.group("m2")]
        d1 = int(m.group("d1"))
        d2 = int(m.group("d2"))
        return date(y1, m1, d1), date(y2, m2, d2)
    return None


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
    # Match in either pdfplumber's tight form ("TotalTransactions",
    # "Pending/Open Activity") or pypdfium2's spaced form ("Total
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
    return parse_amount(s)


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

    Caller can pass a `statement_year` override; if None we read
    it from the period header in `text`.
    """
    if statement_year is None:
        period = parse_statement_period(text)
        if period is None:
            raise ValueError(
                "could not find statement period header; pass "
                "statement_year explicitly"
            )
        statement_year = period[1].year  # use end-of-period year
    rows = _parse_transactions_new(text, statement_year)
    if rows:
        return rows
    return _parse_transactions_legacy(text, statement_year)


def _parse_transactions_new(text: str, statement_year: int) -> list[TransactionRow]:
    """2025+ parser — single "Transaction Details" section.

    Block-based: each "logical row" is the run of lines from one
    row-start (a date or category-keyword line) to the next.
    With pdfplumber the whole row lands on one line; with
    pypdfium2 the same row often spans 3-5 lines (header line,
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
        row = _parse_new_tx_block(block, statement_year, current_date)
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
                    current_date = date(statement_year, int(mm), int(dd))
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
    r"|.*\b(?:FBO|CUST\s+FOR|UCA?UTMA|UCAUGMA|UTMA|UGMA)\b",
    re.IGNORECASE,
)
# Cap on the block's joined-line length before _parse_new_tx_block
# is called. Same belt-and-suspenders logic as the legacy parser:
# a future Schwab layout quirk can't smuggle a page of chrome
# through if the block can't grow past this many characters.
_NEW_TX_BLOCK_MAX_CHARS = 400


# Per-row charge/fee notes that pypdfium2 emits on their own
# line; pdfplumber concatenates them to the previous line with
# no whitespace. Either way we don't want them confused with
# the trailing numeric columns of the data row.
_NEW_TX_NOISE_RE = re.compile(
    r"\s*(?:Industry\s+Fee|Commission|Accrued\s+Interest)"
    r"\s*\$?\(?[\d,.]+\)?",
    re.IGNORECASE,
)


def _parse_new_tx_block(block_lines: list[str],
                         statement_year: int,
                         fallback_date: date | None) -> TransactionRow | None:
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
            row.date = date(statement_year, int(mm), int(dd))
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
# "CLASS A: EXMP"). We extract that as the row's symbol.

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
# pending trades — i.e. a PII leak into silver). Note we don't drop
# `in_section`: more legitimate data rows may follow on a later page.
# Page-chrome / sibling-section markers that flush the current
# row in the legacy parser. The keep-list mixes structural
# anchors (Statement Period / Account Number) and the
# custodial-account-specific headers Schwab uses for ESA / UTMA
# / IRA pages — those carry the full custodian + beneficiary
# names ("Education Savings of <NAME> FBO <NAME> ED SAVINGS
# ACCT CHARLES SCHWAB & CO INC CUST", "<NAME> CUST FOR <NAME>
# UCAUTMA UNTIL AGE <N>") which we absolutely don't want
# bleeding into the description column.
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
    r"|Latest\s+Price\b"
    r"|©"  # copyright line
    r")"
    # Also stop on any line containing custodial-header markers
    # ("FBO" / "CUST FOR" / "UCAUTMA") anywhere — Schwab emits
    # them as the FIRST page-header line for ESA / UTMA accounts.
    r"|.*\b(?:FBO|CUST\s+FOR|UCA?UTMA|UCAUGMA|UTMA|UGMA)\b",
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
# colon-led uppercase token on the line (we look at the whole
# logical row, including continuation lines).
_LEGACY_TX_SYMBOL_IN_DESC_RE = re.compile(
    r":\s+([A-Z][A-Z0-9./-]{0,8})\b"
)
# Known transaction-type phrases — longest first so multi-word
# phrases match before their single-word shortcuts (e.g.
# "Cash Dividend" wins over "Dividend"). Each value is the
# `kind` we surface in the silver row; the gold layer maps to
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
# letters, comma-separated, max ~3 in practice; full taxonomy is
# in each statement's "Endnotes For Your Account" section. Used
# to make the kind-phrase match tolerant of the gluing.
_LEGACY_TX_FOOTNOTE_SUFFIX = r"(?:[A-Za-z](?:,[A-Za-z]){0,4})?"

# Compiled once from _LEGACY_TX_KIND_PHRASES: matches a kind
# phrase at the start of the description, optionally followed by
# a glued footnote marker, then whitespace or end-of-string. The
# phrase group lets us look up the (kind, phrase-len) to surface.
_LEGACY_TX_KIND_RE = re.compile(
    r"^(?P<phrase>" +
    "|".join(re.escape(p) for p, _ in _LEGACY_TX_KIND_PHRASES) +
    r")" + _LEGACY_TX_FOOTNOTE_SUFFIX + r"(?:\s|$)"
)
_LEGACY_TX_KIND_LOOKUP = dict(_LEGACY_TX_KIND_PHRASES)


def _parse_legacy_tx_date(token: str, statement_year: int) -> date | None:
    """Parse a settle / trade date token like "12/15" or
    "06/17/24". Returns None on malformed input."""
    parts = token.split("/")
    try:
        if len(parts) == 2:
            mm, dd = int(parts[0]), int(parts[1])
            yr = statement_year
        elif len(parts) == 3:
            mm, dd, yy = int(parts[0]), int(parts[1]), int(parts[2])
            yr = 2000 + yy if yy < 100 else yy
        else:
            return None
        return date(yr, mm, dd)
    except ValueError:
        return None


def _parse_legacy_tx_row_line(line: str, statement_year: int):
    """If `line` begins with a Settle Date + Trade Date pair,
    parse it into a TransactionRow with .amount / .date / .symbol
    populated from what we can recover. Continuation lines (no
    date prefix) return None; the caller appends them to the
    last row's description.
    """
    m = _LEGACY_TX_DATE_RE.match(line)
    if m is None:
        return None
    settle = _parse_legacy_tx_date(m.group("settle"), statement_year)
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
                                 statement_year: int) -> list[TransactionRow]:
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
        parsed = _parse_legacy_tx_row_line(line, statement_year)
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
            # If we just crossed the cap, truncate cleanly so
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
    """Extract position rows from a statement's full-text.

    Returns a list of dicts — see _parse_position_block for the
    keys. Cash positions are NOT included (they live in the
    cash-balances parser); "Positions - Summary" / cash-variant
    sections are skipped.

    Handles all three statement-layout eras Schwab has shipped:

      * 2025+: section header "Positions - <Section>",
        single-row-per-instrument with eight trailing columns,
        cost basis in the row.
      * 2020-2024: section header "Investment Detail - <Section>",
        multi-line-per-instrument with seven trailing columns on
        the main row plus a separate "Cost Basis N" line and a
        "SYMBOL: TICKER" continuation that holds the real ticker
        (the main row leads with the company name).
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


# All-caps ADR description-continuation words that happen to fit the
# ticker shape (<= 9 chars) and so must NOT be mistaken for a new
# position-row header. "UNSPONSORED" is 11 chars and already fails
# _TICKER_RE, but we list it for clarity / robustness.
_POSITIONS_CONT_WORDS = frozenset({"SPONSORED", "UNSPONSORED"})


def _parse_positions_new(text: str) -> list[dict]:
    """2025+ parser — "Positions - <Section>" anchors.

    Layout robustness: pypdfium2 preserves the source PDF's
    glyph layout and tends to split each position row across
    THREE lines for Schwab statements ("TICKER COMPANY NAME" /
    "(M)" / "100.0000 50.0 5000 ..."). A line-at-a-time parser
    would mis-segment those. We instead accumulate consecutive
    lines into a per-row buffer; flushing happens whenever the
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
        # chars present in token[0]), and most continuation lines
        # ("(M)", "1,234.56 ...") don't either. The exception is an
        # ADR description line "SPONSORED ADR" — "SPONSORED" is nine
        # uppercase chars and so matches _TICKER_RE; without the
        # deny-set below it would split the ADR's block and steal
        # the real ticker's numbers (dropping the ADR itself and
        # emitting a spurious "SPONSORED" holding).
        if ((_TICKER_RE.match(tokens[0]) or _CUSIP_RE.match(tokens[0]))
                and tokens[0] not in _POSITIONS_CONT_WORDS):
            _flush()
            buffer = [line]
            raw_buffer = [raw]
        elif buffer:
            buffer.append(line)
            raw_buffer.append(raw)
        # else: pre-row chrome before the first ticker; skip.
    _flush()
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
#   - Each position is followed by per-tax-lot detail lines we
#     skip (the silver schema only needs aggregate cost basis).
#   - Main row carries 7 trailing values: quantity, market_price,
#     market_value, pct_of_acct, unrealized, est_yield,
#     est_annual_income. Cost basis comes from the "Cost Basis"
#     continuation line.

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
_LEGACY_DATE_TOKEN_RE = re.compile(r"^\d{2}/\d{2}/\d{2,4}$")


def _parse_positions_legacy(text: str) -> list[dict]:
    """Parse the 2020-2024 "Investment Detail - X" layout.

    Same return shape as the 2025+ parser. A block is the run of
    lines from one "main row" (>= 7 trailing numeric tokens AND
    a leading company-name token) to the next; tax-lot detail
    lines are absorbed into the block but only the SYMBOL: /
    Cost Basis / accrued-interest information is extracted —
    per-lot detail is parser-skipped.
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
            _flush()
            new_section = m.group("section").strip()
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
        cnt = _count_trailing_col_tokens(tokens)
        # A "main row" has >= 7 trailing tokens AND a leading
        # uppercase token that is NOT a known continuation
        # marker. Tax-lot lines have a date token mid-row which
        # breaks the trailing-token run, so they end up with
        # cnt < 7.
        is_main = (
            cnt >= 7
            and tokens[0] not in ("SYMBOL:", "Cost", "Accrued", "Total")
            and re.match(r"^[A-Z][A-Z0-9]*$", tokens[0])
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
    cnt = _count_trailing_col_tokens(tokens)
    if cnt < 7:
        return None
    trailing = tokens[len(tokens) - cnt:]
    desc_tokens = tokens[: len(tokens) - cnt]
    description = " ".join(desc_tokens)
    description = _strip_margin_marker(description)

    instrument_key: str | None = None
    cost_basis: float | None = None
    accrued_interest: float | None = None
    extra_desc: list[str] = []

    for line in block_lines[1:]:
        m_sym = _LEGACY_SYMBOL_LINE_RE.search(line)
        if m_sym and instrument_key is None:
            instrument_key = m_sym.group("ticker")
            # Don't `continue` — SYMBOL: lines also carry
            # per-lot data, but we're not using it.
        m_cb = _LEGACY_COST_BASIS_RE.match(line)
        if m_cb:
            cost_basis = _parse_number(m_cb.group("cb"))
            if m_cb.group("acc"):
                accrued_interest = _parse_number(m_cb.group("acc"))
            continue
        # Description-only continuation lines have no digits and
        # no SYMBOL: marker. Tax-lot rows always carry a date
        # token (MM/DD/YY) — skip those.
        toks = line.split()
        if any(_LEGACY_DATE_TOKEN_RE.match(t) for t in toks):
            continue
        if m_sym is None:
            # Description fragment (e.g. "CLASS A", "SPONSORED ADR").
            # Only keep all-uppercase short fragments to avoid
            # accidentally pulling in disclosure text.
            if (
                len(toks) <= 6
                and all(re.match(r"^[A-Z][\w&./:\-]*$", t) for t in toks)
            ):
                extra_desc.append(line.strip())
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

    # Trailing-column order in the legacy layout:
    # quantity, market_price, market_value, pct_of_acct,
    # unrealized, est_yield, est_annual_income
    quantity = _as_num(trailing[0])
    market_price = _as_num(trailing[1])
    market_value = _as_num(trailing[2])
    pct_of_acct = (
        trailing[3] if trailing[3].endswith("%") or trailing[3] == "<1%"
        else None
    )
    unrealized = _as_num(trailing[4])
    est_yield = trailing[5]
    est_annual_income = _as_num(trailing[6])

    return {
        "instrument_key": instrument_key,
        "description": description,
        "quantity": quantity,
        "market_price": market_price,
        "market_value": market_value,
        "cost_basis": cost_basis,
        "unrealized_gain_loss": unrealized,
        "accrued_interest": accrued_interest,
        "est_yield": est_yield,
        "est_annual_income": est_annual_income,
        "pct_of_acct": pct_of_acct,
        "section": section,
        "raw_lines": list(block_lines),
    }


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
    cost_basis / unrealized_gain_loss / accrued_interest / yield
    / annual_income / pct_of_acct all NULL (the source PDF
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


def _count_trailing_col_tokens(tokens: list[str]) -> int:
    """Number of trailing tokens (scanning right-to-left) that fit a
    position-row column slot — the run length the legacy-layout parsers
    use to tell a main row from a description/tax-lot continuation."""
    cnt = 0
    for tok in reversed(tokens):
        if _is_trailing_col_token(tok):
            cnt += 1
        else:
            break
    return cnt


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
# market value reads blank). We only skip a marker that sits BETWEEN
# two column tokens, so a description ending in a lone letter
# ("… CLASS A") is never consumed.
_POSITION_FOOTNOTE_RE = re.compile(r"[A-Za-z](?:,[A-Za-z]){0,4}")
# A footnote marker glued (no whitespace) to the FRONT of a numeric
# column token — pypdfium2 emits this when the marked column is a
# parenthesised negative, e.g. "t(1,200.00)" (third-party-edited
# unrealized loss). Group 1 = marker letters, group 2 = the column.
_POSITION_FOOTNOTE_GLUED_RE = re.compile(
    r"^([A-Za-z](?:,[A-Za-z]){0,4})(\(?-?[\d,].*)$")


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


# ============================================================
# Legacy cash-summary parser (2020-2024 statement layout)
# ============================================================
#
# The pre-2025 layout puts the cash-flow numbers in a multi-line
# block under "Cash Transactions Summary", one labelled row per
# category. Each row carries TWO values: this period and YTD.
# We extract the this-period number.

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
        for label, field in _LEGACY_CASH_LABELS:
            if line.startswith(label):
                rest = line[len(label):].lstrip("*").lstrip()
                m = _LEGACY_CASH_VALUE_RE.search(rest)
                if m:
                    captured[field] = _parse_number(m.group(1))
                break
        if "closing_balance" in captured:
            break  # Ending Cash row reached; we're done.
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
# Per-category cash flow (deposits / withdrawals / etc.) is not
# itemised in this layout; we surface only opening/closing and
# leave the per-category fields NULL.

_VERY_OLD_CASH_SUBHEADER_RE = re.compile(
    r"^Cash\s+and\s+Bank\s+Sweep\s*$"
)


def _parse_cash_summary_very_old(text: str) -> dict | None:
    lines = text.split("\n")
    in_section = False
    in_cash = False
    opening_total = 0.0
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
        # Each cash row: trailing two decimal-numeric tokens.
        tokens = line.split()
        if len(tokens) < 3:
            continue
        if not (_NUM_RE.fullmatch(tokens[-1]) and _NUM_RE.fullmatch(tokens[-2])):
            continue
        opening = _parse_number(tokens[-2])
        closing = _parse_number(tokens[-1])
        if opening is None or closing is None:
            continue
        opening_total += opening
        closing_total += closing
        rows_seen += 1
    if rows_seen == 0:
        return None

    # Period dates are added by the caller; here we just emit
    # the cash numbers. Use 2017-2019 NULLs for the per-category
    # fields the source doesn't carry.
    return {
        "opening_balance": opening_total,
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
    """
    full_text = _extract_pdf_text(path)
    period = parse_statement_period(full_text)
    txs = parse_transactions(full_text, statement_year=statement_year)
    positions = parse_positions(full_text)
    cash = parse_cash_summary(full_text)
    registration = parse_account_registration(full_text)
    account_number = parse_account_number(full_text)
    return {
        "path": str(path),
        "period_start": period[0].isoformat() if period else None,
        "period_end": period[1].isoformat() if period else None,
        "transactions": [t.to_dict() for t in txs],
        "positions": positions,
        "cash_summary": cash,
        "account_registration": registration,
        "account_number": account_number,
    }


# ============================================================
# 3rd-Party-Distribution letters
# ============================================================
#
# Schwab files a "3rd Party Distribution" confirmation letter (under
# the Letters document type) for every movement of money or securities
# OUT of an account to a third party. These are the ONLY record of
# securities transferred out — gifts to people / DAFs, transfers to
# other custodians or accounts. The brokerage statements show such a
# move only as an unexplained position drop; the letter itemises it.
#
# Three observed layout families, all OUT, all single-confirmation:
#   1. Wire transfer(s) — cash leaving by wire. Recipient rendered as
#      "To the account of <NAME> at <BANK>" + a cash amount.
#   2. Transfer(s) to Schwab accounts of third parties — CASH to
#      another Schwab account: "Account name: <NAME>" + a cash amount.
#   3. Transfer(s) to Schwab accounts of third parties — SECURITIES:
#      same recipient block plus a "Security(ies) transferred:" table
#      (Symbol / Quantity / Market Value).
#
# Text comes from the same pypdfium2 extractor the statement parser
# uses (one row per line, clean enough that simple line anchors work).

_DIST_MONTHS = {
    "January": 1, "February": 2, "March": 3, "April": 4, "May": 5,
    "June": 6, "July": 7, "August": 8, "September": 9, "October": 10,
    "November": 11, "December": 12,
}
_DIST_BODY_DATE_RE = re.compile(
    r"\b(January|February|March|April|May|June|July|August|September|"
    r"October|November|December)\s+(\d{1,2}),\s+(\d{4})\b"
)
_DIST_FILENAME_DATE_RE = re.compile(r"_(\d{4})-(\d{2})-(\d{2})_")
_DIST_WIRE_RE = re.compile(r"\bWire transfer", re.I)
_DIST_SCHWAB_3P_RE = re.compile(
    r"Transfer\(s\) to Schwab accounts of third parties", re.I)
_DIST_CASH_AMT_RE = re.compile(
    r"Cash transfer amount requested:\s*\$([\d,]+\.\d{2})", re.I)
_DIST_RECIP_SUFFIX_RE = re.compile(
    r"(?:To account ending in|Account ending in):\s*(\d{3,5})", re.I)
_DIST_WIRE_RECIP_RE = re.compile(
    r"^To the account of\s+(.*\S)\s+at\s+(.*\S)\s*$", re.I)
_DIST_ACCT_NAME_RE = re.compile(r"^Account name:\s*(.*\S)\s*$", re.I)
# A securities row: SYMBOL  QUANTITY  $MARKET_VALUE (pypdfium2 renders
# the three columns space-separated on one line).
_DIST_SEC_ROW_RE = re.compile(
    r"^([A-Z][A-Z0-9./]{0,9})\s+([\d,]+\.\d+)\s+\$([\d,]+\.\d+)\s*$")
# Lines that close the multi-line "Account name:" continuation block.
_DIST_NAME_STOP_RE = re.compile(
    r"^(Security\(ies\)|Cash transfer|Total|Thank you|Please|Symbol)\b", re.I)


def _distribution_date(lines: list[str], filename: str | None) -> str | None:
    """Transfer date as ISO. Prefer the body letter date ("Month D,
    YYYY"); fall back to the YYYY-MM-DD embedded in the filename."""
    for ln in lines:
        m = _DIST_BODY_DATE_RE.search(ln)
        if m:
            mon, d, y = m.group(1), int(m.group(2)), int(m.group(3))
            return f"{y:04d}-{_DIST_MONTHS[mon]:02d}-{d:02d}"
    if filename:
        m = _DIST_FILENAME_DATE_RE.search(filename)
        if m:
            return f"{m.group(1)}-{m.group(2)}-{m.group(3)}"
    return None


def _distribution_counterparty(lines: list[str], method: str
                               ) -> tuple[str | None, str | None, str | None]:
    """Return (counterparty, bank, recipient_account_suffix).

    Wire letters render the recipient as "To the account of <NAME> at
    <BANK>"; Schwab-to-Schwab letters render "Account name: <NAME>"
    (which can wrap across lines)."""
    counterparty: str | None = None
    bank: str | None = None
    suffix: str | None = None

    for ln in lines:
        m = _DIST_RECIP_SUFFIX_RE.search(ln)
        if m and suffix is None:
            suffix = m.group(1)

    if method == "wire":
        for ln in lines:
            m = _DIST_WIRE_RECIP_RE.match(ln)
            if m:
                counterparty = m.group(1).strip()
                bank = re.sub(r"\s+and$", "", m.group(2).strip()) or None
                break
    else:
        for i, ln in enumerate(lines):
            m = _DIST_ACCT_NAME_RE.match(ln)
            if m:
                parts = [m.group(1).strip()]
                for cont in lines[i + 1:]:
                    if _DIST_NAME_STOP_RE.match(cont) or ":" in cont:
                        break
                    parts.append(cont.strip())
                counterparty = " ".join(p for p in parts if p) or None
                break
    return counterparty, bank, suffix


def _distribution_securities(lines: list[str]) -> list[tuple[str, float, float]]:
    """Parse the "Security(ies) transferred:" table → list of
    (symbol, quantity, market_value). Empty when there is no table
    (cash-only letters)."""
    out: list[tuple[str, float, float]] = []
    in_table = False
    for ln in lines:
        if ln.lower().startswith("security(ies) transferred"):
            in_table = True
            continue
        if not in_table:
            continue
        if ln.lower().startswith("total market value"):
            break
        m = _DIST_SEC_ROW_RE.match(ln)
        if m:
            out.append((
                m.group(1),
                float(m.group(2).replace(",", "")),
                float(m.group(3).replace(",", "")),
            ))
    return out


def parse_distribution_text(text: str, filename: str | None = None) -> list[dict]:
    """Parse the extracted text of a 3rd-Party-Distribution letter into
    normalised silver rows (source='third_party_distribution').

    One row per security line for a securities transfer; one row for a
    cash transfer. Rows carry the counterparty, direction, method, and
    (for securities) symbol / quantity / market value. Returns [] for
    an unrecognised layout (logged) so a format we haven't seen drops
    cleanly instead of corrupting the load."""
    lines = [ln.strip() for ln in text.splitlines() if ln.strip()]
    joined = "\n".join(lines)

    date_iso = _distribution_date(lines, filename)
    # Every observed letter is a distribution OUT ("moved money out of
    # the account"); detect an inbound variant defensively.
    direction = "in" if re.search(r"\b(received into|moved money in|"
                                  r"deposited into)\b", joined, re.I) else "out"

    if _DIST_WIRE_RE.search(joined):
        method = "wire"
    elif _DIST_SCHWAB_3P_RE.search(joined):
        method = "schwab_third_party"
    else:
        method = "unknown"

    counterparty, bank, recip_suffix = _distribution_counterparty(lines, method)
    sec_rows = _distribution_securities(lines)
    cash = None
    m = _DIST_CASH_AMT_RE.search(joined)
    if m:
        cash = float(m.group(1).replace(",", ""))

    kind = "Transfer Out" if direction == "out" else "Transfer In"
    base = {
        "date": date_iso,
        "direction": direction,
        "method": method,
        "counterparty": counterparty,
        "counterparty_bank": bank,
        "counterparty_account_suffix": recip_suffix,
        "kind": kind,
    }

    rows: list[dict] = []
    if sec_rows:
        for sym, qty, mv in sec_rows:
            rows.append({
                **base,
                "amount": mv,            # market value = flow magnitude + id
                "symbol": sym,
                "instrument_key": sym,
                "description": f"Securities transfer {direction} "
                               f"to {counterparty or 'third party'}",
                "transfer_kind": "securities",
                "security_symbol": sym,
                "quantity": qty,
                "market_value": mv,
            })
    elif cash is not None:
        rows.append({
            **base,
            "amount": cash,
            "symbol": None,
            "instrument_key": None,
            "description": f"Cash transfer {direction} "
                           f"to {counterparty or 'third party'}",
            "transfer_kind": "cash",
            "cash_amount": cash,
        })
    else:
        log.warning("3rd-party-distribution %s: no securities table or cash "
                    "amount recognised (method=%s) — emitting no rows",
                    filename or "<text>", method)
    return rows


def parse_distribution_pdf(path) -> list[dict]:
    """Open a 3rd-Party-Distribution PDF and return normalised silver
    rows. Thin wrapper over `parse_distribution_text` so the row logic
    is unit-testable without a real PDF."""
    text = _extract_pdf_text(path)
    name = str(path).rsplit("/", 1)[-1]
    return parse_distribution_text(text, filename=name)


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
