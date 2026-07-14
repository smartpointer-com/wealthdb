"""
Parser for SVB Wealth Advisory / NFS brokerage-statement PDFs.

A third statement layout, distinct from the 529 statements
(``pdf_parsers.py``) and the supplied statements
(``pdf_parsers_supplied.py``):

* Masthead is ``SVB WEALTH ADVISORY, INC.`` (a brokerage carried
  by National Financial Services LLC — every page footer reads
  ``Account carried with National Financial Services LLC``). There
  is **no** ``svb> Private | Wealth | Trust | Banking`` banner.
* **One account per PDF** (unlike the multi-account supplied
  statements), but the ``Account Number:`` header is still
  re-stamped on every page; the parser splits on that header and
  keeps the single logical account.
* The account id keeps its literal SVB form — ``SV[MRT]-NNNNNN``
  (e.g. ``SVM-000000``) — dash and letters preserved, *not*
  collapsed to 9 digits like the supplied-statement parser does.
* Period line is upper-case with the word ``TO`` and may span a
  quarter, not just a calendar month:
  ``STATEMENT FOR THE PERIOD JANUARY 1, 2021 TO MARCH 31, 2021``.
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
  ``holdings`` list and the correct ``period_end`` so a terminal
  $0 snapshot can still be recorded. This is not an error.

The text-level parsers (``parse_statement_period``,
``parse_account_blocks``, ``parse_holdings_block``) are pure
functions of strings, exercised by unit tests against synthetic
fixtures. ``parse_svbwa_statement_pdf(path, expected_signature=…)``
is the orchestration entry-point; it opens the PDF via pdfplumber
and returns the same dict shape as the supplied-statement parser's
``parse_supplied_statement_pdf``.
"""

from __future__ import annotations

import re
from dataclasses import dataclass
from datetime import date


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

        if _is_boilerplate(stripped):
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
    qty = _parse_number(nums[0])
    price = _parse_number(nums[1])
    mv = _parse_number(nums[2])
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
    qty = _parse_number(nums[0])
    price = _parse_number(nums[1])
    mv = _parse_number(nums[2])
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
    # The per-page re-stamped registration line ("<name(s)> -
    # <ownership type>").
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
            return _parse_number(tok)
    return None


def _parse_number(tok):
    """Parse one money/numeric token. Strips ``$`` and thousands
    commas; maps surrounding parens to a negative sign; ``%`` is
    dropped. Returns ``None`` for non-numeric / empty tokens."""
    if tok is None:
        return None
    t = tok.strip()
    negative = False
    if t.startswith("(") and t.endswith(")"):
        negative = True
        t = t[1:-1]
    t = t.replace("$", "").replace(",", "").rstrip("%").strip()
    if t.startswith("-"):
        negative = True
        t = t[1:]
    if not t or t in ("-", "+"):
        return None
    try:
        val = float(t)
    except ValueError:
        return None
    return -val if negative else val


# ============================================================
# PDF orchestration
# ============================================================

def parse_svbwa_statement_pdf(path, *, expected_signature=None):
    """Open an SVB-WA statement PDF and return a structured dict
    with the same shape as the supplied-statement parser::

        {
            "path": "<absolute path>",
            "period_start": "YYYY-MM-DD" | None,
            "period_end":   "YYYY-MM-DD" | None,
            "accounts": [
                {"account_external_id": "SVM-000000",
                 "holdings": [{...}, ...]},
                ...
            ],
        }

    ``expected_signature`` is an optional page-1 substring (e.g. the
    personal registration line) the concatenated text must contain;
    on mismatch the function returns
    ``{"_error": "signature-mismatch", …}`` so the loader can log
    and skip a misfiled PDF without bailing the whole run.

    A no-positions / $0 statement still returns its account (with an
    empty ``holdings`` list) and the correct ``period_end`` so a
    terminal $0 snapshot can be recorded.

    pdfplumber is imported inside :func:`_extract_pdf_text` so the
    text-level parsers stay importable in environments without it
    (e.g. unit tests with synthetic fixtures).
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
        rows = parse_holdings_block(block.text)
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
        })
    return {
        "path": str(path),
        "period_start": period[0].isoformat() if period else None,
        "period_end": period[1].isoformat() if period else None,
        "accounts": accounts_out,
    }


def _extract_pdf_text(path):
    """Concatenate every page's text via pdfplumber, joined with
    newlines so ``parse_account_blocks`` can split on the
    per-page-restamped ``Account Number:`` header."""
    import pdfplumber
    with pdfplumber.open(str(path)) as pdf:
        return "\n".join(
            (page.extract_text() or "") for page in pdf.pages
        )


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
        "--signature", default=None,
        help="Optional page-1 substring guard.",
    )
    p.add_argument(
        "--json-out", default="-",
        help="Output path for the JSON array (default: stdout).",
    )
    args = p.parse_args(argv)
    out = [
        parse_svbwa_statement_pdf(pp, expected_signature=args.signature)
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
