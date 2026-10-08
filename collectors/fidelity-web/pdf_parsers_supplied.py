"""
Parsers for legacy statement PDFs supplied out-of-band.

Fidelity does not serve these statements through the positions /
activity scraper feeds, so the historical archive is reconstructed by
parsing PDFs dropped into the supplied-statements directory. The
layout differs from the retail 529 statements parsed in
`pdf_parsers.py`:

* The page header is a private-wealth masthead rather than the
  retail Fidelity layout.
* A single PDF covers **multiple sub-accounts**: one
  Portfolio Summary spread, then one per-account section per
  sub-account, each section running over many pages (an
  ``Account #`` header is re-stamped on every page of the
  section). The parser glues consecutive same-account pages into
  one logical block before extracting holdings.
* Holdings come in two flavours sharing one Holdings heading:
    - Equity / fund / core layout (7 trailing numerics:
      ``Qty | Price | MV | Cost | UnrealizedG/L | EAI | EY%``).
      The instrument **ticker is parenthesised** at the end of
      the description, sometimes wrapping 1-2 continuation
      lines, which the parser glues forward.
    - Bond layout (also 7 trailing numerics: ``Qty | Price |
      MV | Cost | UnrealizedG/L | EAI | Coupon%`` — the
      maturity column is a date that doesn't match the numeric
      regex and stays with the description). The instrument
      key is the **CUSIP**, exposed on a metadata line below
      the data row (``FIXED COUPON MOODYS Aa2 … CUSIP:
      NNNNNNNNN``). The parser looks ahead a few lines for the
      ``CUSIP:`` marker.
    - The Core Account row substitutes ``not applicable`` for
      the cost / unrealized columns since money-market sweeps
      have no cost basis.
* An ``Assets Held Away`` block appears within each account's
  Holdings — these are not Fidelity-custodied and are out of
  scope for this ingest (held-away positions are tracked
  through other channels).

Coverage scope:

* Monthly statements only. Year-end reports use a richer
  per-asset-class layout (additional Maturity/Coupon/Accrued
  Interest columns rendered separately; descriptions span 3+
  lines with CUSIPs on dedicated lines) and are redundant with
  the December monthly statement at the same period end, so
  they're skipped.
* Holdings sections per account. Of the Activity blocks only
  Withdrawals, Deposits and Fees and Charges are read as activity
  (see ``ACTIVITY_SECTIONS``), and the sales of Securities Bought &
  Sold as closed lots (see ``parse_sales_block``); the income and
  transfer blocks are not, nor are Income Summary / Estimated Cash
  Flow.
* That limit is invisible on an account the live activity feed
  also covers, which carries the rest. For an account the feed no
  longer returns, such as a closed one, the ledger is whatever
  those three sections held. A closing account's wind-up belongs
  under Exchanges Out on the statement of the month it closes,
  but that statement no longer lists the account; the gold adapter
  mirrors the out-leg from the receiving account's side, which
  names it (wealthdb/internal/silver/fidelity/windup.go).
* Assets Held Away is excluded by design.

Architecture:

The text-level parsers (``parse_statement_period``,
``parse_account_blocks``, ``parse_holdings_block``) are pure
functions of strings so tests exercise them against hand-crafted
fixtures without a real PDF. ``parse_supplied_statement_pdf(path)``
is the orchestration entry-point that opens the PDF via
pdfplumber and delegates to the text-level parsers.

Performance note: this module is why fidelity-web stays on
pdfplumber. In a trial swap (2026-07) to collectorkit's
``extract_text_pdfium`` (measured ~27x faster), ``pdf_parsers``
round-tripped byte-identically but this module's parsed rows
differed. Reworking the line heuristics to hold under PDFium's
text layout unlocks the swap for both modules (see the note in
``pdf_parsers``).
"""

from __future__ import annotations

import re
from dataclasses import dataclass, field

from collectorkit.pdf import extract_text_pdfplumber as _extract_pdf_text

from datetime import date as date_cls

from pdf_common import _ACCOUNT_HEADER_RE, parse_statement_period

# A coarse, human-readable epoch for load.py's parse-cache namespace.
# Automatic invalidation is handled by the source fingerprint (see
# load._SUPPLIED_PARSER_FINGERPRINT / collectorkit.srcfp), which re-keys the
# cache whenever this module or its import closure changes, so an edit to
# the text-level parsers cannot be replayed under stale cache entries.
# Bumping this constant is an optional manual override to force a
# re-parse without a code change.
PARSER_VERSION = "1"


# ============================================================
# Per-account blocks
# ============================================================
#
# The statement period parser, month map and the ``Account #``
# header regex (``_ACCOUNT_HEADER_RE``) are shared with the 529
# parser — see pdf_common. ``parse_account_blocks`` below glues the
# header Fidelity re-stamps on every page of an account section.


@dataclass
class AccountBlock:
    account_external_id: str  # 9-digit canonical form, no dash
    text: str                 # glued text across all pages of this account


def parse_account_blocks(text):
    """Split a statement's full text into one ``AccountBlock`` per
    sub-account.

    Fidelity re-stamps the ``Account #`` header on **every page**
    of an account's section, so a naive split would emit one block
    per page (one account's section can run to many pages).
    The parser glues all
    consecutive blocks sharing the same account id into one
    logical block — the Portfolio Summary page on page 2 is the
    one exception (it lists every account but holds no per-account
    pages itself) and is filtered out because no two consecutive
    summary mentions ever share the same account id; the run-
    length grouping handles it naturally.
    """
    matches = list(_ACCOUNT_HEADER_RE.finditer(text))
    if not matches:
        return []
    blocks = []
    cur_acct = None
    cur_start = None
    last_match_end = None
    for i, m in enumerate(matches):
        acct = m["acct"].replace("-", "")
        nxt_start = matches[i + 1].start() if i + 1 < len(matches) else len(text)
        if acct == cur_acct:
            last_match_end = nxt_start
            continue
        if cur_acct is not None:
            blocks.append(AccountBlock(
                account_external_id=cur_acct,
                text=text[cur_start:last_match_end],
            ))
        cur_acct = acct
        cur_start = m.start()
        last_match_end = nxt_start
    if cur_acct is not None:
        blocks.append(AccountBlock(
            account_external_id=cur_acct,
            text=text[cur_start:last_match_end],
        ))
    return blocks


# ============================================================
# Holdings rows
# ============================================================

_HOLDINGS_START_RE = re.compile(r"^\s*Holdings\s*$", re.MULTILINE)

# End-of-Holdings markers we recognise inside an account section.
# The first ``Assets Held Away`` line ends the in-scope rows even
# if the Holdings table continues — those are not Fidelity-
# custodied so we drop them by design.
_HOLDINGS_END_RE = re.compile(
    r"^\s*(?:Activity|Assets Held Away)\s*$", re.MULTILINE,
)

# Ticker in parens at end of a (possibly multi-line) description.
_TICKER_RE = re.compile(r"\(([A-Z0-9]{1,8})\)\s*$")

# CUSIP on a metadata line below a bond data row. CUSIPs are
# 9 alphanumeric characters; the metadata line is e.g.
# ``FIXED COUPON MOODYS Aa2 S&P AA- SEMIANNUALLY CUSIP: 123456AA7``.
_CUSIP_RE = re.compile(r"\bCUSIP:\s*([A-Z0-9]{9})\b")

# Trailing numeric token: optional ``$``/``-``, digits with
# commas, optional decimal, optional ``%``. Also matches a bare
# ``-`` or ``--`` — Fidelity prints those for unknown / N/A
# numeric cells (e.g. dividend yield on a non-dividend-paying
# stock). The parser treats them as None when extracting numbers.
_NUM_TOKEN_RE = re.compile(r"^(?:--|-|-?\$?-?[\d,]+(?:\.\d+)?%?)$")

# Sub-headers and boilerplate lines that may appear within the
# Holdings block — skipped without contributing rows.
_SECTION_HEADERS = {
    "Core Account", "Mutual Funds", "Stock Funds", "Bond Funds",
    "Exchange Traded Products", "Equity ETPs", "Bond ETPs",
    "Bonds", "Stocks", "Common Stock", "Common Stocks",
    "Preferred Stocks", "Cash", "Other",
    "Municipal Bonds", "Corporate Bonds", "US Treasury/Agency Securities",
    "U.S. Treasury Bonds", "Government Bonds",
}

_NONDATA_LINE_PREFIXES = (
    "Total ",
    "Includes ",
    "Description Quantity",
    "Price Total ",
    "Total income earned",
    "Accrued Interest (AI)",
    "Total Including Accrued",
    "AI (Accrued",
    "All positions held",
    "B See ",
    "Cost Basis - the original",
    "Total Cost Basis does not",
    "EAI Estimated",
    "EAI Estimated Annual",
    "& EY ",
    "Holdings",
    "INVESTMENT REPORT",
    "Account #",
    "Separate Account Manager",
)


@dataclass
class SuppliedHoldingRow:
    """One holdings line from a per-account section.

    ``instrument_key`` is the ticker for equities/funds (extracted
    from the parenthesised tail of the description) or the CUSIP
    for bonds (extracted from the metadata line below the data
    row). ``None`` when neither is available.
    """
    description: str                  # fund / security name verbatim
    instrument_key: str | None        # ticker or CUSIP
    quantity: float | None
    price: float | None               # per-unit, USD
    market_value: float | None        # USD
    cost_basis: float | None          # USD (None for core / cash)
    unrealized_gain: float | None     # USD


# ============================================================
# Per-account ACTIVITY
# ============================================================
#
# Only three of the statement's Activity sections are read, and the
# omissions are deliberate. Everything else it prints there is
# security-level and already arrives through the scraped activity
# feed: `Dividends, Interest & Other Income`, the corporate actions
# under `Other Activity In` / `Out`, and the inter-account journals
# under `Exchanges In` / `Out`.
#
# What the feed does NOT carry is money entering or leaving the
# account, or the fees charged for holding it — wires, cheques,
# tax payments and account fees. The statement prints all of it
# and the feed omits it, so this is the only route to it.
#
# The gap is easy to miss because the feed DOES book the core-account
# redemption that raises the cash for such a payment. The money
# appears to move and then stops: a credit into settled cash with
# nothing spending it.
# The value is the silver `kind`, in the collector's existing
# upper-case vocabulary — `FEE` is the one the scraped feed already
# uses, and gold's kindFor maps all three.
ACTIVITY_SECTIONS = {
    "Withdrawals": "WITHDRAWAL",
    "Deposits": "DEPOSIT",
    "Fees and Charges": "FEE",
}

# A row opens with its MM/DD and closes with the amount; everything
# between is the description, which the statement spreads over as
# many columns as it likes (`Reference` and `Description` on the two
# money sections, `Description` alone on the fee one). pdfplumber
# collapses those columns to single spaces, so the text between the
# two anchors is taken whole rather than split by position.
_ACTIVITY_ROW_RE = re.compile(
    r"^(?P<mm>\d{2})/(?P<dd>\d{2})\s+(?P<desc>.*?)\s+"
    r"(?P<amt>-?\$?-?[\d,]+\.\d{2})$")

# Only the two money sections wrap. A `Fees and Charges` row is one
# line by construction — `Date Description Amount` — so there is
# nothing below a fee row for a fold to pick up. Page furniture is a
# separate concern and is handled for every section by the
# `_is_boilerplate` branch in `parse_activity_block`.
_ACTIVITY_WRAPS = {"WITHDRAWAL", "DEPOSIT"}


@dataclass
class SuppliedActivityRow:
    """One account-level activity line.

    ``description`` is the statement's own words, with any
    continuation lines folded in: on a wire those name the
    beneficiary and the receiving bank, which is the only thing that
    says what the payment was for.
    """
    date: date_cls                    # resolved against the statement period
    section: str                      # ACTIVITY_SECTIONS value
    description: str
    amount: float                     # USD, signed as the statement prints it


def _activity_row_date(mm, dd, period):
    """Date an MM/DD row against the statement period, which may span
    a year boundary (a December statement listing a January
    settlement) or a whole year (the year-end statement).

    A row whose MM/DD lands inside the period takes that year. One
    that does not is charged in ARREARS — a fee for an earlier
    dividend can print on a January statement dated 11/12 — so it
    resolves BACKWARDS to the most recent such day on or before
    the period end. Resolving it forwards instead would date the fee
    in the future.
    """
    if not period:
        return None
    start, end = period
    for y in (end.year, start.year, end.year - 1):
        try:
            d = date_cls(y, mm, dd)
        except ValueError:
            continue
        if start <= d <= end:
            return d
    for y in (end.year, end.year - 1):
        try:
            d = date_cls(y, mm, dd)
        except ValueError:
            continue
        if d <= end:
            return d
    return None


def parse_activity_block(account_text, *, period=None,
                         expected_signature=None):
    """Extract the account-level activity rows from one per-account
    section. Returns [] when the statement prints none, which is
    ordinary — most months move no money.

    A section runs from its heading to its own `Total <heading>`
    line. In the two money sections a line that does not open with
    MM/DD continues the row above it — on a wire those lines name the
    beneficiary and the receiving bank, which is the only thing that
    says what the payment was for. Fee rows never wrap, so nothing is
    folded onto them.

    That fold is bounded by `_is_boilerplate`, which ends the row at the
    first line of page furniture. `parse_account_blocks` glues the pages
    of one account together, so a section spanning a page break has the
    next page's masthead, re-stamped account header, registration line
    and repeated column header sitting between two of its rows — and the
    row above them would otherwise swallow the lot into the description
    that gold reads as the payment's narrative.
    """
    rows = []
    lines = account_text.splitlines()
    section = None
    pending = None

    def flush():
        nonlocal pending
        if pending is not None:
            rows.append(pending)
            pending = None

    for raw in lines:
        ln = raw.strip()
        if not ln:
            continue
        if ln in ACTIVITY_SECTIONS:
            flush()
            section = ACTIVITY_SECTIONS[ln]
            continue
        if section is None:
            continue
        # `Total Fees and Charge` — the statement truncates its own
        # heading here, so the prefix is matched rather than the name.
        if ln.lower().startswith("total "):
            flush()
            section = None
            continue
        m = _ACTIVITY_ROW_RE.match(ln)
        if m:
            flush()
            d = _activity_row_date(int(m.group("mm")), int(m.group("dd")), period)
            if d is None:
                continue
            amount = _parse_number(m.group("amt").replace("$", ""))
            if amount is None:
                continue
            pending = SuppliedActivityRow(
                date=d, section=section,
                description=" ".join(m.group("desc").split()),
                amount=amount,
            )
        elif _is_boilerplate(ln, expected_signature):
            # A money section that spans a page break carries the next
            # page's frame inside the glued block — the masthead, the
            # re-stamped `Account #` header, the registration line, the
            # repeated column header. End the row at the FIRST of them:
            # everything after it then arrives with nothing pending, so the
            # rest of the frame is inert whether or not it is recognised.
            # Skipping instead would need every furniture line matched, and
            # the column header and period line match nothing.
            #
            # flush() only — NOT `section = None`. The section continues on
            # the next page, and clearing it would drop every remaining row.
            flush()
        elif pending is not None and pending.section in _ACTIVITY_WRAPS:
            pending.description = (pending.description + " " + ln).strip()
    flush()
    return rows


# ============================================================
# Per-account SECURITIES BOUGHT & SOLD — the sales
# ============================================================
#
# The section lists every trade the account settled in the period.
# Only the sales are read: a sale is the one row that states a cost
# basis and, on a continuation line, its term and realized gain or loss.
# A purchase states neither, and the scraped activity feed carries the
# trades themselves.
#
# A row reads `[s]MM/DD <security name> <symbol or CUSIP> <action>
# <quantity> <price> <cost basis> <transaction cost> <amount>`. The leading
# `s` marks a security whose basis follows Specific Share
# identification. The date is the settlement date; the statement prints
# no trade date. Text that wraps below a row (the rest of the name,
# trade notes, a lot reference) never opens with a date, so a row ends
# at the next dated line.

_BOUGHT_SOLD_HEADING_RE = re.compile(
    r"^Securities Bought & Sold(?: \(continued\))?$")

# The actions a sale prints under. `Cancelled Sell` is not a sale: it
# reverses one, and `parse_sales_block` pairs it off.
_SALE_ACTIONS = ("You Sold", "Redeemed")
_CANCELLED_SALE = "Cancelled Sell"

_SALE_ROW_RE = re.compile(
    r"^(?P<ssid>s)?(?P<mm>\d{2})/(?P<dd>\d{2})\s+(?P<desc>.+?)\s+"
    r"(?P<symbol>\S+)\s+"
    r"(?P<action>" + "|".join(map(re.escape, (*_SALE_ACTIONS, _CANCELLED_SALE)))
    + r")\s+(?P<tail>.+)$")

# Any trade row, sale or not: it ends the row above it.
_TRADE_ROW_RE = re.compile(r"^s?\d{2}/\d{2}\s")

# `Short-term loss: $12.34` — on its own line or after a wrapped note.
_TERM_RE = re.compile(
    r"\b(?P<term>Short|Long)-term (?P<sign>gain|loss): \$(?P<amt>[\d,]+\.\d{2})")

# Slack, in dollars, when telling a transaction cost from a basis by the
# row's arithmetic: the half cent the printed amount is rounded by. It stays
# below a one-cent cost, the smallest a row prints.
_AMOUNT_TOLERANCE = 0.0051

# The most accrued interest a bond sale's amount can carry, per unit of par:
# interest accrues over at most one coupon period, so a year of a 15% coupon
# bounds it.
_MAX_ACCRUED_PER_PAR = 0.15

# How a sale row's arithmetic closes, best first: per unit, in percent of
# par with accrued interest on top, or not at all.
_PER_UNIT, _BY_ACCRUED, _OPEN = 2, 1, 0


@dataclass
class SuppliedSaleRow:
    """One sale from the Securities Bought & Sold section, as printed.

    ``quantity`` and ``amount`` are magnitudes: the statement prints the
    quantity of a sale negative. ``cost_basis`` is None where the
    statement prints ``-`` or ``unknown``. ``term`` and ``gain_loss``
    come from the continuation lines, one per term and sign the sale
    realized: ``gain_loss`` is their sum, and ``term`` is set only when
    every line names the same term. Both stay None when there is none.
    """
    settlement_date: date_cls
    description: str
    symbol: str                       # Symbol/CUSIP column as printed
    action: str
    specific_share_id: bool
    quantity: float | None
    price: float | None               # per unit, or percent of par for a bond
    cost_basis: float | None
    transaction_cost: float | None    # signed as printed
    amount: float | None              # transaction amount
    term: str | None = None           # 'short' | 'long'
    gain_loss: float | None = None
    cells: tuple = ()                 # the numeric cells as printed
    terms: list = field(default_factory=list)  # every (term, signed gain) printed


def _sale_cells(tokens):
    """Map a sale row's numeric tail onto its five columns: quantity,
    price, total cost basis, transaction cost, amount.

    pdfplumber drops an empty cell instead of printing it, so a row
    with one blank between price and amount yields four tokens. The
    one in the middle is the transaction cost when the row's arithmetic
    closes better with it as the cost, and the cost basis when it
    closes better without it (see ``_closing``). When both readings
    close equally well, a figure signed against the amount is the
    transaction cost: a cost lowers the amount, and a basis is never
    negative. Any other row leaves the figure unassigned rather than
    guessed.
    """
    vals = [_parse_number(t.replace("$", "")) for t in tokens]
    if len(vals) >= 5:
        return tuple(vals[-5:])
    if len(vals) != 4:
        return None
    qty, price, middle, amount = vals
    if None in (qty, price, middle, amount):
        return qty, price, None, None, amount
    as_cost = _closing(qty, price, middle, amount)
    as_basis = _closing(qty, price, 0.0, amount)
    if as_cost > as_basis or (
            as_cost == as_basis != _OPEN and middle * amount < 0):
        return qty, price, None, middle, amount
    if as_basis > as_cost:
        return qty, price, middle, None, amount
    return qty, price, None, None, amount


def _closing(qty, price, cost, amount):
    """How ``quantity × price + transaction cost = amount`` closes with
    ``cost`` as the transaction cost.

    A share's price is per unit, and the equation holds exactly. A
    bond's is in percent of par, and its amount can add the interest
    accrued since the last coupon: ``quantity × price / 100 + cost``
    then falls short of the amount by no more than
    ``_MAX_ACCRUED_PER_PAR`` of the par sold. A ``Cancelled Sell``
    prints quantity, cost and amount with the opposite signs, so the
    test runs on the figures signed as on a sale.
    """
    sign = 1 if amount > 0 else -1
    units, cost, amount = abs(qty), cost * sign, amount * sign
    if abs(units * price + cost - amount) <= _AMOUNT_TOLERANCE:
        return _PER_UNIT
    accrued = amount - (units * price / 100 + cost)
    if -_AMOUNT_TOLERANCE <= accrued <= units * _MAX_ACCRUED_PER_PAR:
        return _BY_ACCRUED
    return _OPEN


def parse_sales_block(account_text, *, period=None):
    """Extract the sales from one per-account section's Securities
    Bought & Sold listing. Returns [] when the section is absent or
    lists only purchases.

    A cancelled sale prints twice: the sale, then a ``Cancelled Sell``
    row with the opposite quantity and amount. Neither is a realized
    sale, so a pair matched on symbol, quantity and amount drops out
    together. A cancellation with no sale above it on the statement
    stays, under its own action, so a reader can pair it across
    statements.
    """
    rows = []
    pending = None
    inside = False

    def flush():
        nonlocal pending
        if pending is not None:
            if pending.terms:
                pending.gain_loss = round(sum(v for _, v in pending.terms), 2)
                kinds = {t for t, _ in pending.terms}
                if len(kinds) == 1:
                    pending.term = kinds.pop()
            rows.append(pending)
            pending = None

    for raw in account_text.splitlines():
        ln = raw.strip()
        if not ln:
            continue
        if _BOUGHT_SOLD_HEADING_RE.match(ln):
            inside = True
            continue
        if not inside:
            continue
        if ln.startswith("Total ") or ln.startswith("Net "):
            flush()
            inside = False
            continue
        m = _SALE_ROW_RE.match(ln)
        if m:
            flush()
            d = _activity_row_date(int(m["mm"]), int(m["dd"]), period)
            cells = _sale_cells(m["tail"].split())
            if d is None or cells is None:
                continue
            qty, price, basis, transaction_cost, amount = cells
            pending = SuppliedSaleRow(
                settlement_date=d,
                description=" ".join(m["desc"].split()),
                symbol=m["symbol"],
                action=m["action"],
                specific_share_id=bool(m["ssid"]),
                quantity=abs(qty) if qty is not None else None,
                price=price,
                cost_basis=basis,
                transaction_cost=transaction_cost,
                amount=abs(amount) if amount is not None else None,
                cells=tuple(m["tail"].split()),
            )
            continue
        if _TRADE_ROW_RE.match(ln):
            flush()
            continue
        if pending is not None:
            for t in _TERM_RE.finditer(ln):
                v = float(t["amt"].replace(",", ""))
                pending.terms.append(
                    (t["term"].lower(), -v if t["sign"] == "loss" else v))
    flush()
    return _drop_cancelled_sales(rows)


def _drop_cancelled_sales(rows):
    """Drop each sale together with the ``Cancelled Sell`` row that
    reverses it: same symbol, same quantity, same amount."""
    out = list(rows)
    for cancel in [r for r in rows if r.action == _CANCELLED_SALE]:
        match = next(
            (r for r in out if r.action != _CANCELLED_SALE
             and r.symbol == cancel.symbol and r.quantity == cancel.quantity
             and r.amount == cancel.amount),
            None)
        if match is not None:
            out.remove(match)
            out.remove(cancel)
    return out


def parse_holdings_block(account_text, *, expected_signature=None):
    """Extract every holdings row from a per-account section.

    Algorithm:

    1. Clip the block to ``Holdings`` … ``Activity`` |
       ``Assets Held Away``.
    2. Walk lines, scoring each by its trailing numeric tail.
       Data rows carry exactly 7 trailing numerics (the
       layouts share that shape — equities/funds use ``Qty,
       Price, MV, Cost, UnrealizedG/L, EAI, EY%``; bonds use
       ``Qty, Price, MV, Cost, UnrealizedG/L, EAI, Coupon%``).
       The Core Account row has only 5 trailing numerics due to
       the literal ``not applicable not applicable`` gap, so it
       gets its own branch.
    3. Description = tokens before the numeric tail. If no
       ticker is parenthesised at the end of that description,
       glue forward across non-data continuation lines (up to 3)
       until a ticker appears.
    4. If still no ticker, look ahead a few lines for a
       ``CUSIP:`` marker — bonds carry it on the metadata line
       a row or two below the data row.
    """
    m_start = _HOLDINGS_START_RE.search(account_text)
    if not m_start:
        return []
    after_start = m_start.end()
    m_end = _HOLDINGS_END_RE.search(account_text, after_start)
    block = account_text[after_start:m_end.start() if m_end else len(account_text)]

    rows = []
    raw_lines = [ln.rstrip() for ln in block.splitlines()]
    i = 0
    while i < len(raw_lines):
        stripped = raw_lines[i].strip()
        if not stripped:
            i += 1
            continue
        if _is_boilerplate(stripped, expected_signature):
            i += 1
            continue
        tokens = stripped.split()
        is_core = "not applicable not applicable" in stripped
        if is_core:
            row, consumed = _parse_core_account_row(
                stripped, raw_lines, i, expected_signature,
            )
            if row is not None:
                rows.append(row)
            i += 1 + consumed
            continue
        tail = _trailing_numeric_count(tokens)
        if tail < 7:
            i += 1
            continue
        # When >7 trailing numerics, the surplus is embedded in
        # the description — ADRs read ``... ADR EACH REP 0.20
        # <qty> <price> <mv> ...`` where the ``0.20`` is part of
        # ``REP 0.20 ORD`` (the underlying-share ratio). Always
        # take the last 7 as the data tail and push the rest back
        # onto the description.
        desc_tokens = tokens[: len(tokens) - 7]
        numeric_tokens = tokens[-7:]
        desc = " ".join(desc_tokens).strip()
        ticker = _extract_ticker(desc)
        consumed_extra = 0
        # Glue up to 4 continuation lines onto the description
        # while looking for the ticker. Continuations may carry a
        # short numeric fragment of the description itself (e.g.
        # ADRs that read ``... ADR EACH REPR / 0.50 / ORD (TICKER)``
        # over three physical lines, with ``0.50`` mid-description);
        # only the appearance of another full 7-numeric data row
        # or a recognised boilerplate line ends the glue window.
        while ticker is None and consumed_extra < 4:
            j = i + 1 + consumed_extra
            if j >= len(raw_lines):
                break
            nxt = raw_lines[j].strip()
            if not nxt:
                consumed_extra += 1
                continue
            if _is_boilerplate(nxt, expected_signature):
                break
            if _trailing_numeric_count(nxt.split()) >= 5:
                break
            desc = (desc + " " + nxt).strip()
            consumed_extra += 1
            ticker = _extract_ticker(desc)
        # Bond fallback: no ticker, but the metadata line directly
        # below the data row (often already glued onto desc by the
        # loop above) carries the CUSIP. Scan both the glued
        # description and the next few unparsed lines.
        if ticker is None:
            cusip = _extract_cusip(desc) or _lookahead_cusip(
                raw_lines, i + 1 + consumed_extra,
            )
            if cusip is not None:
                ticker = cusip
        clean_desc = _TICKER_RE.sub("", desc).strip() or desc
        row = SuppliedHoldingRow(
            description=clean_desc,
            instrument_key=ticker,
            quantity=_parse_number(numeric_tokens[0]),
            price=_parse_number(numeric_tokens[1]),
            market_value=_parse_number(numeric_tokens[2]),
            cost_basis=_parse_number(numeric_tokens[3]),
            unrealized_gain=_parse_number(numeric_tokens[4]),
        )
        if (row.quantity is None and row.price is None
                and row.market_value is None):
            i += 1 + consumed_extra
            continue
        rows.append(row)
        i += 1 + consumed_extra
    return rows


def _is_boilerplate(line, signature=None):
    if line in _SECTION_HEADERS:
        return True
    # The per-account registrant header (the registration name
    # Fidelity re-stamps at every page break) can land inside
    # a Holdings block. Skip it by matching the runtime
    # ``expected_signature`` rather than embedding the name here.
    if signature and line.startswith(signature):
        return True
    if any(line.startswith(p) for p in _NONDATA_LINE_PREFIXES):
        return True
    # Single non-alphanumeric tokens that show up in pdfplumber
    # output as page-frame noise (envelope barcodes, font hints).
    if len(line) == 1:
        return True
    return False


def _parse_core_account_row(line, all_lines, idx, signature=None):
    """Core Account row: ``DESC qty price mv N/A N/A eai ey%``,
    with ``not applicable not applicable`` as a literal in the
    middle. Returns ``(row, extra_lines_consumed)``."""
    parts = line.split("not applicable not applicable", 1)
    if len(parts) != 2:
        return None, 0
    left_tokens = parts[0].split()
    right_tokens = parts[1].split()
    if len(left_tokens) < 4 or len(right_tokens) < 1:
        return None, 0
    qty_str, price_str, mv_str = left_tokens[-3], left_tokens[-2], left_tokens[-1]
    desc_tokens = left_tokens[: len(left_tokens) - 3]
    desc = " ".join(desc_tokens).strip()
    ticker = _extract_ticker(desc)
    consumed = 0
    while ticker is None and consumed < 2:
        j = idx + 1 + consumed
        if j >= len(all_lines):
            break
        nxt = all_lines[j].strip()
        if not nxt or _is_boilerplate(nxt, signature) or _trailing_numeric_count(nxt.split()):
            break
        desc = (desc + " " + nxt).strip()
        consumed += 1
        ticker = _extract_ticker(desc)
    clean_desc = _TICKER_RE.sub("", desc).strip() or desc
    return SuppliedHoldingRow(
        description=clean_desc,
        instrument_key=ticker,
        quantity=_parse_number(qty_str),
        price=_parse_number(price_str),
        market_value=_parse_number(mv_str),
        cost_basis=None,
        unrealized_gain=None,
    ), consumed


def _lookahead_cusip(lines, start, max_lines=4):
    """Scan up to ``max_lines`` lines for a ``CUSIP: NNNNNNNNN``
    marker. Returns the 9-character CUSIP or ``None``."""
    for j in range(start, min(start + max_lines, len(lines))):
        m = _CUSIP_RE.search(lines[j])
        if m:
            return m.group(1)
    return None


def _extract_cusip(text):
    """Pull a 9-character CUSIP out of a free-form string (the
    bond metadata line, often already glued onto the data row's
    description). Returns the CUSIP or ``None``."""
    m = _CUSIP_RE.search(text)
    return m.group(1) if m else None


def _trailing_numeric_count(tokens):
    n = 0
    for tok in reversed(tokens):
        if _NUM_TOKEN_RE.match(tok):
            n += 1
        else:
            break
    return n


def _extract_ticker(desc):
    m = _TICKER_RE.search(desc)
    return m.group(1) if m else None


def _parse_number(tok):
    if tok is None:
        return None
    cleaned = tok.lstrip("$").rstrip("%").replace(",", "")
    if not cleaned or cleaned in ("-", "+", "--"):
        return None
    try:
        return float(cleaned)
    except ValueError:
        return None


# ============================================================
# PDF orchestration
# ============================================================

def parse_supplied_statement_pdf(path, *, expected_signature=None):
    """Open a supplied statement PDF and return a structured dict:

        {
            "path": "<absolute path>",
            "period_start": "YYYY-MM-DD" | None,
            "period_end":   "YYYY-MM-DD" | None,
            "accounts": [
                {
                    "account_external_id": "NNNNNNNNN",
                    "holdings": [{...}, ...],
                    "activity": [{...}, ...],
                    "sales": [{...}, ...],
                },
                ...
            ],
        }

    ``expected_signature`` (e.g. ``"EXAMPLE REGISTRATION"``) is an
    optional string the page-1 text must contain for the PDF to
    parse. Defends against misfiled statements (a statement for a
    different person dropped into the supplied-statements directory
    will still match the ``<registration> *.PDF`` filename filter);
    the returned dict carries
    ``{"_error": "signature-mismatch", ...}``
    so the loader can log + skip without bailing the whole run.
    """
    text = _extract_pdf_text(path)
    if expected_signature and expected_signature not in text:
        return {
            "_error": "signature-mismatch",
            "expected_signature": expected_signature,
            "path": str(path),
        }
    period = parse_statement_period(text)
    blocks = parse_account_blocks(text)
    accounts_out = []
    for block in blocks:
        rows = parse_holdings_block(
            block.text, expected_signature=expected_signature,
        )
        activity = parse_activity_block(
            block.text, period=period, expected_signature=expected_signature,
        )
        sales = parse_sales_block(block.text, period=period)
        # A block with no holdings, activity or sales is a cover page
        # or a summary spread, not an account section.
        if not rows and not activity and not sales:
            continue
        accounts_out.append({
            "account_external_id": block.account_external_id,
            "sales": [
                {
                    "settlement_date": r.settlement_date.isoformat(),
                    "description": r.description,
                    "symbol": r.symbol,
                    "action": r.action,
                    "specific_share_id": r.specific_share_id,
                    "quantity": r.quantity,
                    "price": r.price,
                    "cost_basis": r.cost_basis,
                    "transaction_cost": r.transaction_cost,
                    "amount": r.amount,
                    "term": r.term,
                    "gain_loss": r.gain_loss,
                    "cells": list(r.cells),
                    "terms": [list(t) for t in r.terms],
                }
                for r in sales
            ],
            "activity": [
                {
                    "date": r.date.isoformat(),
                    "section": r.section,
                    "description": r.description,
                    "amount": r.amount,
                }
                for r in activity
            ],
            "holdings": [
                {
                    "description": r.description,
                    "instrument_key": r.instrument_key,
                    "quantity": r.quantity,
                    "price": r.price,
                    "market_value": r.market_value,
                    "cost_basis": r.cost_basis,
                    "unrealized_gain": r.unrealized_gain,
                }
                for r in rows
            ],
        })
    return {
        "path": str(path),
        "period_start": period[0].isoformat() if period else None,
        "period_end": period[1].isoformat() if period else None,
        "accounts": accounts_out,
    }


# ============================================================
# CLI for standalone use
# ============================================================

if __name__ == "__main__":
    import sys

    from collectorkit import parser_cli
    raise SystemExit(parser_cli.dump_json(
        sys.argv[1:], parse_supplied_statement_pdf,
        description="Extract per-account Holdings rows from one or "
                    "more legacy supplied statement PDFs "
                    "and emit JSON."))
