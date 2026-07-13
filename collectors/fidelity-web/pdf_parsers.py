"""
Parsers for Fidelity 529 College Investing Plan statement PDFs.

The bronze fetch saves quarterly and annual statement PDFs under
``<dump-dir>/documents/`` (DESIGN.md §4.5). For 529 accounts
those PDFs contain a per-account ``Holdings`` block from which we can
reconstruct point-in-time position snapshots for any quarter
the statement archive covers, going back well before the toolkit
itself started running.

Coverage scope:

* 529 accounts only. Trust accounts get no Fidelity statement —
  see DESIGN.md §4.5 — and so cannot be backfilled this way.
* Per-account ``Holdings`` table is parsed; the ``College
  Investment Details`` and ``Contribution Elections`` blocks are
  ignored.
* The statement period header gives the as-of date (period
  end); the per-account headers give the 9-digit account
  number.

Architecture:

The text-level parsers (``parse_statement_period``,
``parse_account_blocks``, ``parse_holdings_block``) are pure
functions of strings, so tests exercise them against
hand-crafted fixtures without needing a real PDF.
``parse_statement_pdf(path)`` is the orchestration entry-point
that opens the PDF via pdfplumber and delegates to the
text-level parsers.
"""

from __future__ import annotations

import re
from dataclasses import dataclass

from collectorkit.pdf import extract_text_pdfplumber as _extract_pdf_text

from pdf_common import _ACCOUNT_HEADER_RE, parse_statement_period

# Identifies the parsed-holdings contract of this module for load.py's
# content-addressed parse cache. Bump whenever a change to the
# text-level parsers alters the parsed dict for the same PDF content,
# so cached entries written by an older parser are keyed differently
# and are never replayed under the new logic.
PARSER_VERSION = "1"


# ============================================================
# Per-account blocks
# ============================================================
#
# The statement period parser, month map and the ``Account #``
# header regex (``_ACCOUNT_HEADER_RE``) are shared with the trust
# parser — see pdf_common. ``parse_account_blocks`` below splits on
# a fresh header per 529 account (one section per account); the next
# header of the same shape ends it.


@dataclass
class AccountBlock:
    account_external_id: str  # 9-digit canonical form, no dash
    text: str                 # text from the header through the next header


def parse_account_blocks(text):
    """Split a statement's full text into one ``AccountBlock`` per
    529 account that appears in the per-account pages. The
    Portfolio Summary on page 2 mentions every account but
    doesn't carry a ``Holdings`` table; only the per-account
    pages do, and those carry a fresh ``Account #`` header that
    we anchor on.

    Summary-only references (no subsequent ``Holdings`` block)
    still produce an ``AccountBlock`` here;
    ``parse_holdings_block`` returns an empty list for those and
    the loader treats it as a no-op.
    """
    matches = list(_ACCOUNT_HEADER_RE.finditer(text))
    if not matches:
        return []
    blocks = []
    for i, m in enumerate(matches):
        start = m.start()
        end = matches[i + 1].start() if i + 1 < len(matches) else len(text)
        chunk = text[start:end]
        acct = m["acct"].replace("-", "")
        blocks.append(AccountBlock(account_external_id=acct, text=chunk))
    return blocks


# ============================================================
# Holdings rows
# ============================================================

# Per-account Holdings block boundary. Body sits between the
# literal heading ``Holdings`` and the ``Total Market Value``
# closing summary. The end pattern requires a ``$`` token after
# the literal so a column-header line that happens to contain
# the substring (e.g. ``... Total Market Value`` as the last
# table column) doesn't end the block prematurely.
_HOLDINGS_START_RE = re.compile(r"\bHoldings\b")
_HOLDINGS_END_RE = re.compile(r"Total Market Value\s+\$")

# A numeric token in the Holdings table — possibly $-prefixed,
# possibly comma-grouped, possibly with a trailing %.
_NUM_TOKEN_RE = re.compile(r"\$?[\d,]+(?:\.\d+)?%?")


@dataclass
class HoldingRow:
    description: str        # fund name verbatim
    quantity: float | None
    price: float | None     # per-unit, USD
    market_value: float | None  # USD
    percent_of_total: float | None  # fraction, 0..1


def parse_holdings_block(account_text):
    """Extract the ``Holdings`` rows from a per-account section.

    Layout depends on the statement type:

    * Quarterly statements emit five numeric tokens per row::

        <fund>  <pct>%  $<beg_mv>  <qty>  $<price>  $<end_mv>

    * Year-end statements drop the beginning-market-value
      column, leaving four::

        <fund>  <pct>%  <qty>  $<price>  $<value>

    We tokenize each candidate line by whitespace, identify the
    contiguous run of trailing numeric tokens, and route on its
    length (4 vs 5). Everything before the numeric tail is the
    fund description. The ``%`` and ``$`` glyphs are optional on
    every column — Fidelity's PDF renderer drops them on
    continuation rows — so the regex tolerates either.
    """
    m_start = _HOLDINGS_START_RE.search(account_text)
    if not m_start:
        return []
    m_end = _HOLDINGS_END_RE.search(account_text, m_start.end())
    if not m_end:
        return []
    block = account_text[m_start.end():m_end.start()]
    rows = []
    for raw in block.splitlines():
        line = raw.strip()
        if not line:
            continue
        row = _parse_holdings_line(line)
        if row is not None:
            rows.append(row)
    return rows


def _parse_holdings_line(line):
    """Return a ``HoldingRow`` for a parseable line, ``None`` for
    column-header / continuation / boilerplate lines that happen
    to land inside the Holdings block.

    Tokenises the line, walks the contiguous numeric tail, and
    routes on tail length:
      * 4 numeric tokens → year-end layout (no beginning MV)
      * 5 numeric tokens → quarterly layout
    Other tail lengths or missing values mark the line as not a
    holdings row.
    """
    tokens = line.split()
    if len(tokens) < 5:
        return None
    numeric_tail = []
    for tok in reversed(tokens):
        if _NUM_TOKEN_RE.fullmatch(tok):
            numeric_tail.append(tok)
        else:
            break
    numeric_tail.reverse()
    if len(numeric_tail) not in (4, 5):
        return None
    desc_tokens = tokens[:len(tokens) - len(numeric_tail)]
    if not desc_tokens:
        return None
    description = " ".join(desc_tokens).strip()
    if not description:
        return None
    pct_tok = numeric_tail[0]
    if len(numeric_tail) == 5:
        # quarterly: pct, beg_mv, qty, price, end_mv
        qty_tok = numeric_tail[2]
        price_tok = numeric_tail[3]
        mv_tok = numeric_tail[4]
    else:
        # year-end: pct, qty, price, mv
        qty_tok = numeric_tail[1]
        price_tok = numeric_tail[2]
        mv_tok = numeric_tail[3]
    quantity = _parse_number(qty_tok)
    price = _parse_number(price_tok)
    market_value = _parse_number(mv_tok)
    pct = _parse_percent(pct_tok)
    # Refuse the row if every load-bearing dollars-and-cents
    # number is None — that means the regex matched some column-
    # header fragment we shouldn't keep.
    if quantity is None and price is None and market_value is None:
        return None
    return HoldingRow(
        description=description,
        quantity=quantity,
        price=price,
        market_value=market_value,
        percent_of_total=pct,
    )


def _parse_number(tok):
    if tok is None:
        return None
    cleaned = tok.lstrip("$").rstrip("%").replace(",", "")
    if not cleaned:
        return None
    try:
        return float(cleaned)
    except ValueError:
        return None


def _parse_percent(tok):
    """Parse a ``45%`` / ``45`` token to a 0..1 fraction; ``None``
    if it isn't numeric. We accept the bare-digit form because
    Fidelity's PDF renderer occasionally drops the ``%`` glyph on
    the second-and-subsequent rows of the Holdings table."""
    v = _parse_number(tok)
    if v is None:
        return None
    return v / 100.0


# ============================================================
# PDF orchestration
# ============================================================

def parse_statement_pdf(path):
    """Open the statement PDF and return a structured dict:

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

    pdfplumber is imported inside the function so the text-level
    parsers above remain importable in environments that don't
    have it (e.g. unit tests with hand-crafted fixtures).
    """
    text = _extract_pdf_text(path)
    period = parse_statement_period(text)
    blocks = parse_account_blocks(text)
    accounts_out = []
    for block in blocks:
        rows = parse_holdings_block(block.text)
        if not rows:
            # Summary-page reference to the account; no Holdings
            # table → no positions to emit.
            continue
        accounts_out.append({
            "account_external_id": block.account_external_id,
            "holdings": [
                {
                    "description": r.description,
                    "quantity": r.quantity,
                    "price": r.price,
                    "market_value": r.market_value,
                    "percent_of_total": r.percent_of_total,
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


# PDF text extraction goes through collectorkit.pdf.extract_text_pdfplumber
# (imported at module top as `_extract_pdf_text`).


# ============================================================
# CLI for standalone use
# ============================================================

def _main(argv):
    import argparse
    import json as _json
    p = argparse.ArgumentParser(
        description="Extract per-account Holdings rows from one "
                    "or more Fidelity 529 statement PDFs and emit "
                    "JSON.",
    )
    p.add_argument("pdf", nargs="+", help="One or more PDF paths.")
    p.add_argument(
        "--json-out", default="-",
        help="Output path for the JSON array (default: stdout).",
    )
    args = p.parse_args(argv)
    out = [parse_statement_pdf(pp) for pp in args.pdf]
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
