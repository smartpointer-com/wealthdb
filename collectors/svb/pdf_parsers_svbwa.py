"""
Parser for SVB Wealth Advisory / NFS brokerage-statement PDFs.

A third statement layout, distinct from the 529 statements
(``fidelity-web/pdf_parsers.py``) and the supplied statements
(``fidelity-web/pdf_parsers_supplied.py``):

* Masthead is ``SVB WEALTH ADVISORY, INC.`` on the earlier
  statements and ``SVB INVESTMENT SERVICES, INC.`` on the later
  ones — the same brokerage family either way, carried by
  National Financial Services LLC (every page footer reads
  ``Account carried with National Financial Services LLC``).
  There is **no** ``svb> Private | Wealth | Trust | Banking``
  banner. Because the masthead moves, the family is recognised by
  the period header plus the ``Account Number: SV[MRT]-NNNNNN``
  header instead — see :func:`classify_statement_text`.
* **One account per PDF** (unlike the multi-account supplied
  statements), but the ``Account Number:`` header is still
  re-stamped on every page; the parser splits on that header and
  keeps the single logical account.
* The account id keeps its literal SVB form — ``SV[MRT]-NNNNNN``
  (e.g. ``SVM-000000``) — dash and letters preserved, *not*
  collapsed to 9 digits like the supplied-statement parser does.
* Period line is upper-case with the word ``TO`` and may span a
  quarter, not just a calendar month:
  ``STATEMENT FOR THE PERIOD JANUARY 1, 2020 TO MARCH 31, 2020``.
* Holdings live under a ``Holdings`` heading, in asset-class
  sub-sections each opened by a banner
  (``CASH AND CASH EQUIVALENTS - N% …``, ``HOLDINGS > EQUITIES …``,
  ``HOLDINGS > EXCHANGE TRADED PRODUCTS …``,
  ``HOLDINGS > FIXED INCOME …``, ``HOLDINGS > OPTIONS …``), each
  possibly repeated with a ``… continued`` banner across pages.
  The column header repeats per section/page:
  ``Description | Symbol/Cusip | Account Type | Quantity |
  Price on MM/DD/YY | Current Market Value | Estimated Annual
  Income``.

Row shapes (only 3-4 trailing numeric columns — Qty, $Price, $MV,
optional $EAI; there is **no** cost-basis / unrealized column, so
those keys are always ``None`` in the output):

* **Equity / ETP / fund / money-market**: a single data line
  ``DESCRIPTION  SYMBOL  qty  $price  $mv  [$eai]``. The symbol is
  a *mid-line* column (the token immediately before the numeric
  tail), **not** parenthesised at the description tail as in the
  supplied layout. ``instrument_key`` is that symbol. Continuation
  sub-lines carrying ``Estimated Yield …`` / ``Dividend Option …``
  / ``Capital Gain Option …`` / ``7 DAY YIELD …`` (and a bare
  ``CASH`` / ``MARGIN`` account-type token) are skipped.
* **Cash**: a bare ``NET CASH POSITION  $amount`` line — market
  value only, no symbol / qty / price. Emitted as a cash holding
  (``instrument_key``/``quantity``/``price`` all ``None``).
* **Fixed income (bonds)**: ``DESCRIPTION … CUSIP9  qty  $price
  $mv  [$eai]`` — the 9-char CUSIP sits *inline at the end of the
  description* (same mid-line position as an equity symbol, just
  9 alphanumerics). ``instrument_key`` is that CUSIP. Continuation
  sub-lines (coupon%+maturity, ``MOODY'S … /S&P …``, ``CPN PMT …``,
  ``Next Interest Payable:``, call schedule, ``Accrued Interest``,
  and a ``… CUSIP continued`` echo line) are skipped.
* **Options** (long and short legs): each option spans **three**
  physical lines —
  ::

      CALL (AAAA) … JAN 18 30       (4)   $20.00   ($8,000.00)
      $100 (100 SHS)                MARGIN
      <OCC symbol e.g. AAAA300118C100>

  ``instrument_key`` is the OCC symbol (line 3). **Parenthesised
  numbers are NEGATIVE**: quantity ``(4)`` → ``-4``, market
  value ``($8,000.00)`` → ``-8000.00``. Long legs print plain.
  Mapping parens → negative for *both* quantity and market value is
  essential — a short option leg read as positive overstates the
  total by the entire option premium. (The supplied-statement parser
  does not do this, which is why it cannot be reused here.)
* **Closing / $0 statements** render no table, just the sentence
  ``There were no positions in your account at the close of the
  statement period.`` → the account is returned with an empty
  ``holdings`` list and the correct ``period_end``. Whether that
  means $0 is NOT inferred from the empty table: the statement's
  own ``TOTAL VALUE OF YOUR PORTFOLIO`` / ``ENDING VALUE`` line is
  parsed (``parse_statement_total``) and only a stated zero is
  recorded as one. This is not an error.

Beyond Holdings, every statement carries an **Activity** region
(``parse_activity_block``) of dated, signed money movements,
sub-sectioned as additions/withdrawals, income, taxes+fees,
misc. & corporate actions, core-fund sweeps, other activity, the
trade blotter, and two informational sections (pending
distributions, trades pending settlement). Rows are
``MM/DD/YY  CASH|MARGIN  <TRANSACTION>  <description>  [qty]
$amount`` with **parens → negative**, exactly as in the option
holdings rows; the ``TRANSACTION`` column is a closed verb
vocabulary (``_ACTIVITY_VERBS``) because the text extraction
preserves no column gaps to split on. Each section prints its own
``TOTAL <section> $amount`` line, returned alongside the rows so a
loader can reconcile what it parsed against what the statement
states.

The text-level parsers (``parse_statement_period``,
``parse_account_blocks``, ``parse_holdings_block``,
``parse_activity_block``, ``parse_statement_total``,
``classify_statement_text``) are pure functions of strings,
exercised by unit tests against synthetic fixtures.
``parse_svbwa_statement_pdf(path, expected_signatures=…)`` is the
orchestration entry-point; it opens the PDF via pdfplumber and
returns the same dict shape as the supplied-statement parser's
``parse_supplied_statement_pdf``, plus the svb-specific
``family`` / ``stated_total`` / per-account ``activity`` keys.
"""

from __future__ import annotations

import re
from dataclasses import dataclass
from datetime import date

from collectorkit.pdf import extract_text_pdfplumber as _extract_pdf_text

from statement_tokens import iso_from_short_date, parse_money


# ============================================================
# Statement period
# ============================================================

# Upper-case month names joined by the word "TO". Periods may span
# a quarter, e.g. "JANUARY 1, 2021 TO MARCH 31, 2021".
_PERIOD_RE = re.compile(
    r"STATEMENT\s+FOR\s+THE\s+PERIOD\s+"
    r"(?P<m1>JANUARY|FEBRUARY|MARCH|APRIL|MAY|JUNE|JULY|AUGUST|"
    r"SEPTEMBER|OCTOBER|NOVEMBER|DECEMBER)\s+"
    r"(?P<d1>\d{1,2}),\s*(?P<y1>\d{4})\s+TO\s+"
    r"(?P<m2>JANUARY|FEBRUARY|MARCH|APRIL|MAY|JUNE|JULY|AUGUST|"
    r"SEPTEMBER|OCTOBER|NOVEMBER|DECEMBER)\s+"
    r"(?P<d2>\d{1,2}),\s*(?P<y2>\d{4})",
    re.IGNORECASE,
)

_MONTH_NUMS = {
    "JANUARY": 1, "FEBRUARY": 2, "MARCH": 3, "APRIL": 4,
    "MAY": 5, "JUNE": 6, "JULY": 7, "AUGUST": 8,
    "SEPTEMBER": 9, "OCTOBER": 10, "NOVEMBER": 11, "DECEMBER": 12,
}


def parse_statement_period(text):
    """Return ``(start_date, end_date)`` from the page-1 ``STATEMENT
    FOR THE PERIOD … TO …`` header, or ``None`` if absent."""
    m = _PERIOD_RE.search(text)
    if not m:
        return None
    try:
        start = date(int(m["y1"]), _MONTH_NUMS[m["m1"].upper()], int(m["d1"]))
        end = date(int(m["y2"]), _MONTH_NUMS[m["m2"].upper()], int(m["d2"]))
    except (KeyError, ValueError):
        return None
    return start, end


# ============================================================
# Stated portfolio total
# ============================================================

# A money column: always carries "$", which is what separates it
# from a bare Quantity. Parens mean negative, as everywhere else in
# this layout.
_MONEY = r"\(?-?\$-?[\d,]*\d(?:\.\d+)?\)?"
_MONEY_TOKEN_RE = re.compile(rf"^{_MONEY}$")

# The account's own closing value, stated twice: on page 1 next to
# the contact block, and again in the Account Overview's change-in-
# value table as the current-period column of ENDING VALUE (whose
# second column is the year-to-date figure, identical to it).
_TOTAL_VALUE_RE = re.compile(
    rf"TOTAL\s+VALUE\s+OF\s+YOUR\s+PORTFOLIO\s+(?P<amt>{_MONEY})")
_ENDING_VALUE_RE = re.compile(
    rf"ENDING\s+VALUE\s*\([^)]*\)\s+(?P<amt>{_MONEY})")


def parse_statement_total(text):
    """Return the closing portfolio value the statement STATES, or
    ``None`` when neither stated form is readable or the two
    disagree.

    Both forms are read and required to agree, so a mis-parse of
    one cannot pass as a value. ``None`` means "the statement did
    not say", which is materially different from "the statement
    said zero" — the loader records a zero only from a stated one,
    never from an empty holdings table.
    """
    seen = []
    for rx in (_TOTAL_VALUE_RE, _ENDING_VALUE_RE):
        m = rx.search(text)
        if m:
            val = parse_money(m["amt"])
            if val is not None:
                seen.append(val)
    if not seen:
        return None
    if any(v != seen[0] for v in seen[1:]):
        return None
    return seen[0]


# ============================================================
# Per-account block
# ============================================================

# Account header: "Account Number: SVM-000000". Keep the literal
# SV[MRT]-NNNNNN form (dash + letters) as account_external_id.
_ACCOUNT_HEADER_RE = re.compile(
    r"Account\s+Number:\s*(?P<acct>SV[MRT]-\d{6})"
)


@dataclass
class AccountBlock:
    account_external_id: str  # literal "SVM-000000" form
    text: str                 # glued text across all pages


def parse_account_blocks(text):
    """Split a statement's full text into ``AccountBlock`` runs.

    The ``Account Number:`` header is re-stamped on every page, so a
    naive split would emit one block per page. The parser glues all
    consecutive headers sharing the same account id into one logical
    block. SVB-WA statements carry exactly one account, so this
    normally yields a single block, but the run-length grouping is
    kept (mirroring the supplied-statement parser) so a stray
    multi-account drop would still split correctly.
    """
    matches = list(_ACCOUNT_HEADER_RE.finditer(text))
    if not matches:
        return []
    blocks = []
    cur_acct = None
    cur_start = None
    last_end = None
    for i, m in enumerate(matches):
        acct = m["acct"]
        nxt_start = matches[i + 1].start() if i + 1 < len(matches) else len(text)
        if acct == cur_acct:
            last_end = nxt_start
            continue
        if cur_acct is not None:
            blocks.append(AccountBlock(cur_acct, text[cur_start:last_end]))
        cur_acct = acct
        cur_start = m.start()
        last_end = nxt_start
    if cur_acct is not None:
        blocks.append(AccountBlock(cur_acct, text[cur_start:last_end]))
    return blocks


# ============================================================
# Document family
# ============================================================

# Three statement families share this archive's layout. Only the
# brokerage family is text-extractable and only it is this parser's
# business; the deposit and mortgage families are image-only
# print-stream renderings whose page-1 text is empty, so they are
# recognised as such rather than misread as an unparseable
# brokerage statement.
FAMILY_BROKERAGE = "brokerage"
FAMILY_IMAGE_ONLY = "image_only"
FAMILY_UNKNOWN = "unknown"

# Text below this many non-whitespace characters is a scanned page
# with nothing but incidental glyphs, not a text-layer statement.
_IMAGE_ONLY_MAX_CHARS = 32


def classify_statement_text(text):
    """Return the document family of a statement from its extracted
    text — ``brokerage`` / ``image_only`` / ``unknown``.

    Keyed on the text, never the filename: the archive's filenames
    are hand-assigned and inconsistent. A brokerage statement is
    identified by its page-1 ``STATEMENT FOR THE PERIOD`` header
    (upper-case only on page 1; later pages re-stamp a title-case
    echo) together with an ``Account Number: SV[MRT]-NNNNNN``
    header — both present on every statement of the family across
    both mastheads it was issued under.

    A deposit or mortgage statement carries no text layer at all, so
    it is reported as ``image_only`` — a single bucket, because with
    no text there is nothing to tell the two apart until an OCR pass
    supplies some.
    """
    if _PERIOD_RE.search(text) and _ACCOUNT_HEADER_RE.search(text):
        return FAMILY_BROKERAGE
    if len(re.sub(r"\s+", "", text)) <= _IMAGE_ONLY_MAX_CHARS:
        return FAMILY_IMAGE_ONLY
    return FAMILY_UNKNOWN


# ============================================================
# Holdings rows
# ============================================================

_HOLDINGS_START_RE = re.compile(r"^\s*Holdings\s*$", re.MULTILINE)

# End-of-Holdings markers. The first of these closes the in-scope
# rows; everything after (Activity, portfolio totals, footnotes) is
# out of scope.
_HOLDINGS_END_RE = re.compile(
    r"^\s*(?:Activity|TOTAL PORTFOLIO VALUE|Total Securities|"
    r"Miscellaneous Footnotes)\b",
    re.MULTILINE,
)

# A statement with no positions renders this sentence instead of a
# table. Detected so we return an empty (not failed) account.
_NO_POSITIONS_RE = re.compile(
    r"There were no positions in your account at the close",
    re.IGNORECASE,
)

# The cash row label. pdfplumber sometimes letter-spaces this label
# ("N E T C A S H P O S I T I O N") as a kerning artefact, so the
# detector tolerates optional whitespace between every character.
_NET_CASH_RE = re.compile(
    r"^N\s*E\s*T\s+C\s*A\s*S\s*H\s+P\s*O\s*S\s*I\s*T\s*I\s*O\s*N\b"
)

# A trailing numeric/money token: optional surrounding parens
# (=> negative), optional "$"/"-", digits with thousands commas,
# optional decimal, optional "%". A bare "%" / "-" never matches.
_NUM_TOKEN_RE = re.compile(
    r"^\(?-?\$?-?\d[\d,]*(?:\.\d+)?\)?%?$"
)

# Equity / ETP / fund / money-market symbol: 1-6 upper-case
# alphanumerics (e.g. a stock ticker or a money-market fund symbol).
_SYMBOL_RE = re.compile(r"^[A-Z][A-Z0-9]{0,5}$")

# CUSIP: exactly 9 alphanumerics, at least one digit (distinguishes
# it from an all-letter ticker / word).
_CUSIP_RE = re.compile(r"^(?=.*\d)[A-Z0-9]{9}$")

# An OCC option symbol on an option's third physical line, e.g.
# "AAAA300118C100": 1-6 char root, YYMMDD, C/P, strike digits.
_OCC_RE = re.compile(r"^[A-Z]{1,6}\d{6}[CP]\d+$")

# An option's first physical line opens with CALL/PUT.
_OPTION_OPEN_RE = re.compile(r"^(?:CALL|PUT)\b")

# Section banners (opening a sub-section) and other non-data lines
# that must never be read as holdings rows.
_BANNER_PREFIXES = (
    "CASH AND CASH EQUIVALENTS",
    "HOLDINGS >",
    "HOLDINGS",
    "EQUITIES",
    "EXCHANGE TRADED PRODUCTS",
    "FIXED INCOME",
    "OPTIONS",
)

# Sub-headers / boilerplate within the Holdings block — skipped.
_NONDATA_LINE_PREFIXES = (
    "Total ",
    "Description",
    "Symbol/Cusip",
    "Price on",
    "Estimated",
    "Account Type",
    "For additional information",
    "For an explanation",
    "Copyright",
    "Moody's",
    "Ratings",
    "AI (Accrued",
    "ALERT:",
    "Dividend Option",
    "Capital Gain Option",
    "7 DAY YIELD",
    "CPN PMT",
    "Next Interest Payable",
    "Accrued Interest",
    "MOODY'S",
    "S&P ",
    "ON ",
    "CONTINUOUSLY CALLABLE",
    "CALLABLE ON",
    "SUBJECT TO",
    "Statement for the Period",
    "Account Number:",
    "Separate Acc",
    "Investment Discipline",
    "SVB Wealth Advisory",
    "MN _",
    "ENV#",
    "There were no positions",
)

# Two of the prefixes above are short enough to match the opening of a
# security's own description rather than the prose they were written for, and
# would drop the holding without a word. What they exist to catch never
# carries a symbol column before a numeric tail: a credit rating ("S&P A") and
# a bond's coupon-date line ("ON JAN 01, JUL 01"). So for these two, and only
# these two, a line that parses as a holdings row wins. The rest of the list is
# structural labels, which DO tokenise like a data row now and then ("Total
# Pending Accrued Dividends $… TOTAL 100.0% $…"), so they keep precedence.
_AMBIGUOUS_LINE_PREFIXES = ("S&P ", "ON ")

# Account-class words that appear alone on a line (group sub-headers
# inside a section) — skipped.
_GROUP_HEADERS = {
    "Cash", "Equity", "Money Markets", "Corporate Bonds",
    "Municipal Bonds", "US Treasury", "Government Bonds",
    "Treasuries", "Bonds", "Stocks",
}


@dataclass
class SvbwaHoldingRow:
    """One holdings line.

    ``instrument_key`` is the equity/fund symbol, the bond CUSIP, or
    the OCC option symbol; ``None`` for a bare cash position. SVB-WA
    statements never print cost-basis / unrealized columns, so those
    two fields are always ``None``.
    """
    description: str
    instrument_key: str | None
    quantity: float | None
    price: float | None
    market_value: float | None
    cost_basis: float | None      # always None (column absent)
    unrealized_gain: float | None  # always None (column absent)


def parse_holdings_block(account_text):
    """Extract every holdings row from one account's section.

    Returns ``[]`` for a no-positions / $0 statement (the absence of
    rows, not an error). Otherwise walks the clipped Holdings region
    line by line, dispatching on row shape:

    * ``NET CASH POSITION`` → cash row (MV only).
    * ``CALL/PUT …`` opener → three-line option (parens → negative).
    * any other line ending in 3-4 numerics → equity/ETP/fund/bond,
      with the symbol/CUSIP as the token just before the numeric
      tail.
    """
    m_start = _HOLDINGS_START_RE.search(account_text)
    if not m_start:
        return []
    after = m_start.end()
    m_end = _HOLDINGS_END_RE.search(account_text, after)
    block = account_text[after:m_end.start() if m_end else len(account_text)]

    if _NO_POSITIONS_RE.search(block):
        return []

    lines = [ln.rstrip() for ln in block.splitlines()]
    rows = []
    i = 0
    n = len(lines)
    while i < n:
        stripped = lines[i].strip()
        if not stripped:
            i += 1
            continue

        # Bare cash position: market value only.
        if _NET_CASH_RE.match(stripped):
            mv = _last_number(stripped)
            if mv is not None:
                rows.append(SvbwaHoldingRow(
                    description="NET CASH POSITION",
                    instrument_key=None,
                    quantity=None, price=None,
                    market_value=mv,
                    cost_basis=None, unrealized_gain=None,
                ))
            i += 1
            continue

        # Three-line option block.
        if _OPTION_OPEN_RE.match(stripped):
            row, consumed = _parse_option_block(lines, i)
            if row is not None:
                rows.append(row)
                i += consumed
                continue
            # Fall through if it didn't look like a real option row.

        if _is_boilerplate(stripped) and not _shadows_a_holding(stripped):
            i += 1
            continue

        # Equity / ETP / fund / money-market / bond data row:
        # symbol or CUSIP immediately before a 3-4 numeric tail.
        row = _parse_security_row(stripped)
        if row is not None:
            rows.append(row)
        i += 1
    return rows


def _parse_security_row(line):
    """Parse an equity/ETP/fund/money-market/bond data line of the
    form ``DESC … SYMBOL  qty  $price  $mv  [$eai]``. Returns a
    :class:`SvbwaHoldingRow` or ``None`` if the line is not a data
    row (too few trailing numerics, or no symbol/CUSIP before them).
    """
    tokens = line.split()
    tail = _trailing_numeric_count(tokens)
    # A CUSIP is an identifier, not a figure, but an all-digit one reads as
    # numeric, so it joins the trailing run and pushes the row past the width
    # the layout allows. A description that itself ends in a numeral — an ADR
    # "… SPON ADS EACH REPR 2" — pushes it further still, so the CUSIP is not
    # necessarily the run's first token. Cut the run at the CUSIP wherever it
    # sits and let it be the key.
    if tail > 4:
        run_start = len(tokens) - tail
        for offset in range(tail):
            if _CUSIP_RE.match(tokens[run_start + offset]):
                tail -= offset + 1
                break
    # Data rows carry 3 (qty, price, mv) or 4 (… + eai) numerics.
    if tail < 3 or tail > 4:
        return None
    key_idx = len(tokens) - tail - 1
    if key_idx < 1:
        # Need at least one description token before the key.
        return None
    key_tok = tokens[key_idx]
    desc_tokens = tokens[:key_idx]
    if _CUSIP_RE.match(key_tok):
        instrument_key = key_tok
    elif _SYMBOL_RE.match(key_tok):
        instrument_key = key_tok
    else:
        # Mangled-CUSIP recovery. pdfplumber sometimes glues a bond's
        # last description word onto its CUSIP with no space
        # ("…MAKE WHOLE11111AAA1") and then mis-splits the run on a
        # kerning boundary ("WHOLE11 111AAA1"), so neither the key
        # token nor the token before it is a clean 9-char CUSIP.
        # Re-join the key token with the preceding description token
        # and pull a 9-char CUSIP off the tail; restore the leading
        # remainder (the real description word) to the description.
        recovered = _recover_split_cusip(desc_tokens, key_tok)
        if recovered is None:
            # No symbol/CUSIP column: not a holdings data row (e.g. a
            # stray total or a wrapped-description fragment).
            return None
        instrument_key, desc_tokens = recovered
    desc = " ".join(desc_tokens).strip()
    nums = tokens[key_idx + 1:]
    qty = parse_money(nums[0])
    price = parse_money(nums[1])
    mv = parse_money(nums[2])
    if qty is None and price is None and mv is None:
        return None
    return SvbwaHoldingRow(
        description=desc,
        instrument_key=instrument_key,
        quantity=qty, price=price, market_value=mv,
        cost_basis=None, unrealized_gain=None,
    )


def _recover_split_cusip(desc_tokens, key_tok):
    """Recover a CUSIP that pdfplumber glued onto (and mis-split
    across) the description's last word, e.g. ``["…", "WHOLE11"]``
    + key ``"111AAA1"`` → CUSIP ``"11111AAA1"`` with the leading
    ``"WHOLE"`` restored to the description.

    Returns ``(cusip, new_desc_tokens)`` or ``None`` if no 9-char
    CUSIP can be recovered from the tail of the joined run.
    """
    if not desc_tokens:
        joined = key_tok
        prefix_tokens = []
    else:
        joined = desc_tokens[-1] + key_tok
        prefix_tokens = desc_tokens[:-1]
    # The CUSIP is the last 9 characters of the joined run; whatever
    # precedes it is the spilled-over description word.
    if len(joined) < 9:
        return None
    cand = joined[-9:]
    if not _CUSIP_RE.match(cand):
        return None
    spilled = joined[:-9]
    new_desc = list(prefix_tokens)
    if spilled:
        new_desc.append(spilled)
    return cand, new_desc


def _parse_option_block(lines, idx):
    """Parse a three-physical-line option position starting at
    ``lines[idx]``. Returns ``(row, lines_consumed)`` or
    ``(None, 0)`` if the shape doesn't match.

    Line 1: ``CALL/PUT (ROOT) DESCRIPTION  (qty)  $price  ($mv)``
            — parenthesised qty / MV mean a short leg → NEGATIVE.
    Line 2: ``$strike (100 SHS)`` next to ``CASH``/``MARGIN``.
    Line 3: the OCC symbol (the instrument key).
    """
    line1 = lines[idx].strip()
    tokens = line1.split()
    tail = _trailing_numeric_count(tokens)
    if tail < 3:
        return None, 0
    # The option's data tail is exactly three columns:
    # ``(qty)  $price  ($mv)``. The description, however, ends in an
    # expiry like ``JAN 18 30`` whose bare ``18`` / ``30`` are
    # numeric too, so the trailing-numeric run can be longer than 3.
    # Take the LAST three numeric tokens as the data columns and
    # push the rest (the expiry day/year) back into the description.
    nums = tokens[-3:]
    desc = " ".join(tokens[:len(tokens) - 3]).strip()
    qty = parse_money(nums[0])
    price = parse_money(nums[1])
    mv = parse_money(nums[2])
    if qty is None and mv is None:
        return None, 0

    # Look ahead up to a few lines for the OCC symbol (line 3). The
    # intervening line (``$strike (100 SHS)  MARGIN``) is skipped.
    occ = None
    consumed = 1
    look = idx + 1
    while look < len(lines) and look <= idx + 3:
        cand = lines[look].strip()
        consumed = look - idx + 1
        if not cand:
            look += 1
            continue
        first = cand.split()[0] if cand.split() else ""
        if _OCC_RE.match(first):
            occ = first
            break
        look += 1
    if occ is None:
        # Not a recognisable option block after all; consume only
        # the opener so the caller can re-examine following lines.
        return None, 0
    return SvbwaHoldingRow(
        description=desc,
        instrument_key=occ,
        quantity=qty, price=price, market_value=mv,
        cost_basis=None, unrealized_gain=None,
    ), consumed


def _shadows_a_holding(line):
    """True when a boilerplate prefix has matched a line that is really a
    holdings row — see :data:`_AMBIGUOUS_LINE_PREFIXES`."""
    return (line.startswith(_AMBIGUOUS_LINE_PREFIXES)
            and _parse_security_row(line) is not None)


def _is_boilerplate(line):
    if line in _GROUP_HEADERS:
        return True
    upper = line.upper()
    for p in _BANNER_PREFIXES:
        if upper.startswith(p):
            return True
    for p in _NONDATA_LINE_PREFIXES:
        if line.startswith(p):
            return True
    # A "… CUSIP continued" / "… continued" echo line carrying no
    # numeric tail is a section-continuation marker, not a row.
    if line.endswith("continued") and _trailing_numeric_count(line.split()) == 0:
        return True
    # The per-page re-stamped "<name(s)> - <ownership type>"
    # registration line (an ownership type ending in "Property").
    # Matched on the generic ownership-type suffix so no name is
    # hard-coded; it has no numeric tail anyway, but skipping it
    # explicitly keeps it out of any future description-glue logic.
    if " - " in line and line.split(" - ", 1)[1].rstrip().endswith("Property"):
        return True
    # Account-type-only line (a stray "CASH" / "MARGIN").
    if line in ("CASH", "MARGIN"):
        return True
    # Single-character page-frame noise.
    if len(line) == 1:
        return True
    return False


def _trailing_numeric_count(tokens):
    n = 0
    for tok in reversed(tokens):
        if _NUM_TOKEN_RE.match(tok):
            n += 1
        else:
            break
    return n


def _last_number(line):
    """Return the value of the last numeric token on ``line`` (with
    parens mapped to negative), or ``None``."""
    for tok in reversed(line.split()):
        if _NUM_TOKEN_RE.match(tok):
            return parse_money(tok)
    return None


# ============================================================
# Activity rows
# ============================================================

# The Activity region opens on a bare ``Activity`` heading (once
# per statement; later pages re-stamp ``ACTIVITY continued``) and
# runs to the closing boilerplate.
_ACTIVITY_START_RE = re.compile(r"^\s*Activity\s*$", re.MULTILINE)
_ACTIVITY_END_RE = re.compile(
    r"^\s*(?:Miscellaneous Footnotes|GLOSSARY)", re.MULTILINE)

# Canonical section keys. The first six carry settled money
# movements; the last three are read (so nothing goes missing
# unnoticed) but are the loader's to leave unbooked — the trade
# blotter is a separate surface, and the two pending sections are
# projections that settle into a later statement.
SECTION_ADDITIONS = "additions_withdrawals"
SECTION_INCOME = "income"
SECTION_TAXES_FEES = "taxes_fees"
SECTION_MISC = "misc_corporate"
SECTION_CORE_FUND = "core_fund"
SECTION_OTHER = "other_activity"
SECTION_TRADES = "trades"
SECTION_TRADES_PENDING = "trades_pending"
SECTION_PENDING_DISTRIBUTIONS = "pending_distributions"

# Section banners, keyed on the banner text with whitespace and
# commas removed — the extraction kerns those inconsistently
# ("TAXES, FEES" vs "TAXES,FEES"). Matched as a prefix, so the
# "… continued" page echoes and the sub-section suffixes
# ("INCOME > TAXABLE INCOME") resolve to the same section.
_SECTION_PREFIXES = (
    ("ADDITIONSANDWITHDRAWALS", SECTION_ADDITIONS),
    ("INCOME", SECTION_INCOME),
    ("TAXESFEESANDEXPENSES", SECTION_TAXES_FEES),
    ("MISC.&CORPORATEACTIONS", SECTION_MISC),
    ("MISCELLANEOUS&CORPORATEACTIONS", SECTION_MISC),
    ("COREFUNDACTIVITY", SECTION_CORE_FUND),
    ("OTHERACTIVITY", SECTION_OTHER),
    ("PURCHASESSALESANDREDEMPTIONS", SECTION_TRADES),
    ("TRADESPENDINGSETTLEMENT", SECTION_TRADES_PENDING),
    ("PENDINGDISTRIBUTIONS", SECTION_PENDING_DISTRIBUTIONS),
)

# The corporate-actions section was re-templated between the 2021 and the
# 2022+ statements, and the spelling of its banner is the marker. The earlier
# one strikes no section total and prints TRAN VALUE in the CASH-EQUIVALENT
# convention — receiving shares reads like paying for them, so it is
# parenthesised (negative) and a delivery is positive. The later one prints
# the value FLOW, which is the direction its own section totals are struck on
# and the one that makes the two legs of an inter-account move cancel. The
# earlier convention is negated into the later one so both read alike; without
# that, an in-kind receipt books as an outflow of the same size.
_LEGACY_MISC_BANNER = "MISCELLANEOUS&CORPORATEACTIONS"

# Sections whose rows carry a Quantity column. Everywhere else the
# column is blank, and reading one anyway would misfire: a
# counterparty account id kerns into fragments
# ("SV R-0001 15 -1") whose tail parses as the number -1.
_QUANTITY_SECTIONS = frozenset(
    {SECTION_MISC, SECTION_CORE_FUND, SECTION_TRADES})

# A data row: settlement/effective date, then the account-type
# column (the only two values the statements print), then the
# Transaction column. Requiring the account type is what keeps the
# dated continuation lines ("10/02/23 RECORD DATE 10/03/23") and
# the two-date trades-pending rows out.
_ACTIVITY_ROW_RE = re.compile(
    r"^(?P<date>\d{2}/\d{2}/\d{2})\s+(?P<acct_type>CASH|MARGIN)"
    r"\s+(?P<rest>\S.*)$"
)

# The checking sub-section of Additions and Withdrawals prints a
# different row: "Date Check Number Description Code Amount", with
# no account-type column for the main pattern above to anchor on.
# Its figure is part of the parent section's stated total, so a
# statement carrying one does not add up without it. Gated on the
# sub-section rather than tried everywhere, because a pattern this
# loose would otherwise match continuation lines.
_CHECK_ROW_RE = re.compile(
    r"^(?P<date>\d{2}/\d{2}/\d{2})\s+(?P<verb>CHECK\s+PAID)\s+"
    r"(?P<number>\d+)\s+(?P<amt>\(?\$?-?[\d,]+\.\d{2}\)?)\s*$"
)

# The Transaction column's closed vocabulary, alphabetical (the
# match order is derived below, so this list is only ever read by a
# human adding to it). The text extraction preserves no column gaps
# — every run of spaces collapses to one — so the verb cannot be
# split off by position and is matched against this list instead. A
# row whose verb is not here still comes back, with an empty verb,
# for the loader to report by statement and leave unbooked.
_ACTIVITY_VERBS = (
    "ADJ NON-RESIDENT TAX",
    "CHECK PAID",
    "ADJUSTMENT",
    "ADVISOR FEE DEDUCTED",
    "BOUGHT",
    "CANCELLED BUY",
    "CANCELLED SELL",
    "DIRECT DEBIT",
    "DIRECT DEPOSIT",
    "DISTRIBUTION",
    "DIVIDEND ADJUSTMENT",
    "DIVIDEND RECEIVED",
    "EXPIRED",
    "FEE PAID",
    "FOREIGN TAX PAID",
    "IN LIEU OF FRX SHARE",
    "INTER BROKER CREDIT",
    "INTER BROKER DEBIT",
    "INTER BROKER DELIVER",
    "INTER BROKER RECEIVE",
    "INTEREST",
    "JOURNALED",
    "MARGIN INTEREST",
    "MERGER",
    "NON-RESIDENT TAX",
    "RECEIVED FROM YOU",
    "REDEEMED",
    "REINVESTMENT",
    "RETURN OF CAPITAL",
    "SOLD",
    "TENDERED",
    "TRANSFERRED FROM",
    "TRANSFERRED TO",
    "WIRE TRANS FROM BANK",
    "WIRE TRANS TO BANK",
    "YOU BOUGHT",
    "YOU SOLD",
)
# Longest first, so "MARGIN INTEREST" wins over "INTEREST" and
# "ADJ NON-RESIDENT TAX" over "NON-RESIDENT TAX".
_ACTIVITY_VERB_TOKENS = tuple(
    sorted((tuple(v.split()) for v in _ACTIVITY_VERBS),
           key=len, reverse=True))

# In-kind corporate-action rows print $0.00 in the Amount column
# and the transferred value on a following ``TRAN VALUE:`` line;
# the section's own total is struck on those values, so they are
# the row's amount.
#
# What really ends the search is the next activity row, which the
# lookahead stops on — the count below is only a backstop against a
# malformed section running away. It is set well clear of the
# continuation a row can carry: a reorganisation names the security
# twice, the ratio, and the reference, which puts its figure five
# lines down. Too small a bound loses the value silently, and the
# section then fails its own total, so err high.
_TRAN_VALUE_RE = re.compile(rf"^TRAN\s+VALUE:\s*(?P<amt>{_MONEY})\s*$")
_TRAN_VALUE_LOOKAHEAD = 8

# An UNDATED amount line inside a section — a bond sleeve's
# "Corporate Accrued Interest Earned $50.00" and the like. It has
# no date and no Transaction column, so it is not a movement, but
# the section's stated total DOES include it. Returned with a null
# date so a reconciliation sees it and a loader booking dated rows
# does not. The label must open on a non-digit, which is what keeps
# a dated row — including the two-date rows of the pending-
# settlement section — off this path.
#
# The label may not END on "@", which is what separates an amount from a
# PRICE: a money-market dividend prints "REINVEST @ $1.00" beneath it, and
# reading that dollar-a-share as an amount adds it to a section the
# statement already totalled without it.
_UNDATED_AMOUNT_RE = re.compile(
    rf"^(?P<label>[^\d$(].*?[^@\s])\s+(?P<amt>{_MONEY})\s*$")

# A section total, e.g. "TOTAL ADDITIONS AND WITHDRAWALS
# ($4,000.00)". Upper-case TOTAL only — the title-case
# "Total Taxable Dividends" lines are per-group subtotals inside a
# section, which the section total already covers.
_SECTION_TOTAL_RE = re.compile(
    rf"^TOTAL\s*(?P<name>[A-Z][^$(]*?)\s*(?P<amt>{_MONEY})\s*$")

@dataclass
class SvbwaActivityRow:
    """One money movement from the Activity region.

    ``verb`` is the Transaction column verbatim (empty when the
    column was blank or held no known verb); ``section`` says which
    sub-section it was printed under, which is what decides whether
    it is a settled movement at all. ``ordinal`` is the row's
    position among every row in the account's Activity region, in
    document order — it disambiguates rows that are otherwise
    identical (a same-day pair of transfers from one counterparty)
    and makes the loader's row identity reproducible.

    ``date`` is ``None`` on the undated lines a section can carry —
    a bond sleeve's accrued-interest figure struck for the whole
    period. Those are part of the section's stated total but are
    not movements, so they belong in a reconciliation and not in a
    transactions table.
    """
    date: str | None              # ISO YYYY-MM-DD
    section: str
    account_type: str             # "CASH" / "MARGIN"; empty when undated
    verb: str
    description: str
    quantity: float | None
    amount: float | None
    ordinal: int


def parse_activity_block(account_text):
    """Extract every Activity row from one account's section.

    Returns ``(rows, totals)`` — the rows in document order, and
    the per-section ``TOTAL …`` figures the statement states, keyed
    by the same section keys the rows carry. A caller reconciles
    the two: a section whose rows do not sum to its stated total
    has been misread. Every amount-bearing line in a section is
    returned, including the undated ones, so that sum is complete.

    Returns ``([], {})`` when the statement renders no Activity
    region at all.
    """
    m_start = _ACTIVITY_START_RE.search(account_text)
    if not m_start:
        return [], {}
    after = m_start.end()
    m_end = _ACTIVITY_END_RE.search(account_text, after)
    block = account_text[after:m_end.start() if m_end else len(account_text)]

    lines = [ln.strip() for ln in block.splitlines()]
    rows = []
    totals = {}
    section = None
    legacy_misc = False
    checking = False
    for i, line in enumerate(lines):
        if not line:
            continue
        found = _section_for_banner(line)
        if found is not None:
            section, legacy_misc = found
            checking = "CHECKINGACTIVITY" in _banner_key(line)
            continue
        total = _section_total(line)
        if total is not None:
            key, amount = total
            totals[key] = amount
            continue
        if checking and section == SECTION_ADDITIONS:
            mc = _CHECK_ROW_RE.match(line)
            if mc:
                rows.append(SvbwaActivityRow(
                    date=iso_from_short_date(mc["date"]),
                    account_type="CASH",
                    section=section,
                    verb="CHECK PAID",
                    description=f'CHECK {mc["number"]}',
                    quantity=None,
                    amount=parse_money(mc["amt"]),
                    ordinal=len(rows),
                ))
                continue
        m = _ACTIVITY_ROW_RE.match(line)
        if m:
            row = _parse_activity_row(
                m, section, len(rows),
                _lookahead_tran_value(lines, i, legacy_misc))
        elif section is None:
            continue
        else:
            row = _parse_undated_row(line, section, len(rows))
        if row is not None:
            rows.append(row)
    return rows, totals


def _banner_key(text):
    """Normalise a section name for matching: upper-cased, with the
    ``ACTIVITY >`` prefix, whitespace and commas removed. The
    extraction kerns those inconsistently, so they cannot be part of
    the comparison."""
    return re.sub(r"[\s,]+", "",
                  re.sub(r"^ACTIVITY\s*>\s*", "", text.upper()))


def _section_for_banner(line):
    """Return ``(section key, is-legacy-corporate-actions)`` for a
    banner line, or ``None`` when the line is not one."""
    if line.upper().startswith("TOTAL"):
        return None
    key = _banner_key(line)
    for prefix, name in _SECTION_PREFIXES:
        if key.startswith(prefix):
            return name, key.startswith(_LEGACY_MISC_BANNER)
    return None


def _section_total(line):
    """Return ``(section key, amount)`` for a ``TOTAL …`` line, or
    ``None`` when the line is not one (or names no known section)."""
    m = _SECTION_TOTAL_RE.match(line)
    if not m:
        return None
    amount = parse_money(m["amt"])
    if amount is None:
        return None
    key = _banner_key(m["name"])
    for prefix, name in _SECTION_PREFIXES:
        if key.startswith(prefix):
            return name, amount
    return None


def _parse_activity_row(m, section, ordinal, tran_value):
    """Build one :class:`SvbwaActivityRow` from a matched data
    line. Returns ``None`` when the line carries no money column,
    which no real data row does, or when its date column names no
    real day. ``tran_value`` is the transferred value printed under
    the row, already normalised, and stands in for the Amount
    column on the in-kind rows that print $0.00.

    Refusing a date that names no real day is what keeps the
    statement's own arithmetic in charge: a null date here would
    read exactly like the section components
    :func:`_parse_undated_row` returns on purpose — counted in the
    section's stated total, never booked — so the section would go
    on reconciling while the movement was silently lost. Dropped,
    the row's amount leaves that sum too, the section stops
    matching its stated total, and the loader reports it.
    """
    when = iso_from_short_date(m["date"])
    if when is None:
        return None
    tokens = m["rest"].split()
    verb, rest = _split_activity_verb(tokens)
    if not rest or not _MONEY_TOKEN_RE.match(rest[-1]):
        return None
    amount = parse_money(rest[-1])
    rest = rest[:-1]
    quantity = None
    if (section in _QUANTITY_SECTIONS and rest
            and _NUM_TOKEN_RE.match(rest[-1])
            and "$" not in rest[-1]):
        quantity = parse_money(rest[-1])
        rest = rest[:-1]
    if amount == 0.0 and tran_value is not None:
        amount = tran_value
    return SvbwaActivityRow(
        date=when,
        section=section,
        account_type=m["acct_type"],
        verb=verb,
        description=" ".join(rest).strip(),
        quantity=quantity,
        amount=amount,
        ordinal=ordinal,
    )


def _parse_undated_row(line, section, ordinal):
    """Build a dateless :class:`SvbwaActivityRow` for a labelled
    amount line inside a section, or ``None`` when the line is not
    one.

    A total counted as a component would double its section, so
    every ``Total …`` line is excluded — case-insensitively, since
    the layout strikes the section totals upper-case and the
    per-group subtotals title-case. So is the ``TRAN VALUE:``
    continuation, already folded into the row above it. Group
    headers and page furniture carry no money column and so never
    match in the first place.
    """
    if line.upper().startswith("TOTAL") or _TRAN_VALUE_RE.match(line):
        return None
    m = _UNDATED_AMOUNT_RE.match(line)
    if not m:
        return None
    amount = parse_money(m["amt"])
    if amount is None:
        return None
    return SvbwaActivityRow(
        date=None,
        section=section,
        account_type="",
        verb="",
        description=m["label"].strip(),
        quantity=None,
        amount=amount,
        ordinal=ordinal,
    )


def _split_activity_verb(tokens):
    """Split the Transaction column off the front of a row's
    tokens. Returns ``(verb, remaining tokens)``; the verb is the
    empty string when no known verb starts the run, in which case
    its text stays in the remainder so nothing is silently lost."""
    for cand in _ACTIVITY_VERB_TOKENS:
        if tuple(tokens[:len(cand)]) == cand:
            return " ".join(cand), tokens[len(cand):]
    return "", tokens


def _lookahead_tran_value(lines, idx, legacy_misc):
    """Return the ``TRAN VALUE:`` figure printed under the row at
    ``lines[idx]``, normalised to the value-flow convention, or
    ``None``. Bounded so it can only ever reach the row's own
    continuation lines.

    ``legacy_misc`` marks the earlier template, which prints the
    figure cash-equivalent — negative for a receipt of shares — and
    is negated here so every row in the section reads the same way
    regardless of which template struck it."""
    stop = min(idx + 1 + _TRAN_VALUE_LOOKAHEAD, len(lines))
    for look in range(idx + 1, stop):
        cand = lines[look]
        if not cand:
            continue
        if _ACTIVITY_ROW_RE.match(cand):
            return None
        m = _TRAN_VALUE_RE.match(cand)
        if m:
            value = parse_money(m["amt"])
            if value is None:
                return None
            return -value if legacy_misc else value
    return None


# ============================================================
# PDF orchestration
# ============================================================

def parse_svbwa_statement_pdf(path, *, expected_signatures=()):
    """Open an SVB-WA statement PDF and return a structured dict
    with the same shape as the supplied-statement parser, plus the
    svb-specific ``family`` / ``stated_total`` / ``activity`` keys::

        {
            "path": "<absolute path>",
            "family": "brokerage",
            "period_start": "YYYY-MM-DD" | None,
            "period_end":   "YYYY-MM-DD" | None,
            "stated_total": 12345.67 | None,
            "accounts": [
                {"account_external_id": "SVM-000000",
                 "holdings": [{...}, ...],
                 "activity": [{...}, ...],
                 "activity_totals": {"income": 1.23, ...}},
                ...
            ],
        }

    The family is settled first, off the extracted text, and a
    document that is not a brokerage statement returns immediately
    with no accounts — the brokerage row parsers never see it. The
    image-only families have no text layer for the signature guard
    to read either, so the guard applies only once the family is
    known.

    ``expected_signatures`` is an optional set of page-1
    substrings (the registration lines the archive's accounts are
    titled under); the text must contain at least one. On no match
    the function returns ``{"_error": "signature-mismatch", …}`` so
    the loader can log and skip a misfiled PDF without bailing the
    whole run.

    A no-positions statement still returns its account (with an
    empty ``holdings`` list), the correct ``period_end`` and the
    ``stated_total`` the statement prints, so a real $0 snapshot can
    be recorded from what the statement SAYS rather than inferred
    from the absence of rows.

    pdfplumber is imported inside :func:`_extract_pdf_text` so the
    text-level parsers stay importable in environments without it
    (e.g. unit tests with synthetic fixtures).
    """
    text = _extract_pdf_text(path)
    family = classify_statement_text(text)
    if family != FAMILY_BROKERAGE:
        return {"path": str(path), "family": family, "accounts": []}
    if expected_signatures and not any(s in text for s in expected_signatures):
        return {
            "_error": "signature-mismatch",
            "family": family,
            "path": str(path),
        }
    period = parse_statement_period(text)
    blocks = parse_account_blocks(text)
    accounts_out = []
    for block in blocks:
        rows = parse_holdings_block(block.text)
        activity, activity_totals = parse_activity_block(block.text)
        accounts_out.append({
            "account_external_id": block.account_external_id,
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
            "activity": [
                {
                    "date": a.date,
                    "section": a.section,
                    "account_type": a.account_type,
                    "verb": a.verb,
                    "description": a.description,
                    "quantity": a.quantity,
                    "amount": a.amount,
                    "ordinal": a.ordinal,
                }
                for a in activity
            ],
            "activity_totals": activity_totals,
        })
    return {
        "path": str(path),
        "family": family,
        "period_start": period[0].isoformat() if period else None,
        "period_end": period[1].isoformat() if period else None,
        "stated_total": parse_statement_total(text),
        "accounts": accounts_out,
    }


# ============================================================
# CLI for standalone use
# ============================================================

def _main(argv):
    import argparse
    import json as _json
    p = argparse.ArgumentParser(
        description="Extract per-account Holdings rows from one or "
                    "more SVB Wealth Advisory / NFS statement PDFs "
                    "and emit JSON.",
    )
    p.add_argument("pdf", nargs="+", help="One or more PDF paths.")
    p.add_argument(
        "--signature", action="append", default=None,
        help="Optional page-1 substring guard; repeat to accept "
             "any one of several registrations.",
    )
    p.add_argument(
        "--json-out", default="-",
        help="Output path for the JSON array (default: stdout).",
    )
    args = p.parse_args(argv)
    out = [
        parse_svbwa_statement_pdf(
            pp, expected_signatures=tuple(args.signature or ()))
        for pp in args.pdf
    ]
    blob = _json.dumps(out, indent=2, ensure_ascii=False, default=str)
    if args.json_out == "-":
        print(blob)
    else:
        with open(args.json_out, "w", encoding="utf-8") as fh:
            fh.write(blob)
    return 0


if __name__ == "__main__":
    import sys
    raise SystemExit(_main(sys.argv[1:]))
