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
* Holdings sections per account; Activity / Income Summary /
  Estimated Cash Flow blocks are ignored.
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
from dataclasses import dataclass

from collectorkit.pdf import extract_text_pdfplumber as _extract_pdf_text

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
    eai_str = right_tokens[0]
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
        if not rows:
            continue
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


# ============================================================
# CLI for standalone use
# ============================================================

def _main(argv):
    import argparse
    import json as _json
    p = argparse.ArgumentParser(
        description="Extract per-account Holdings rows from one or "
                    "more legacy supplied statement PDFs "
                    "and emit JSON.",
    )
    p.add_argument("pdf", nargs="+", help="One or more PDF paths.")
    p.add_argument(
        "--json-out", default="-",
        help="Output path for the JSON array (default: stdout).",
    )
    args = p.parse_args(argv)
    out = [parse_supplied_statement_pdf(pp) for pp in args.pdf]
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
