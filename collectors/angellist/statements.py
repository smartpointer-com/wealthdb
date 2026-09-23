#!/usr/bin/env python3
"""PDF parser for AngelList fund financial reports — the per-LP capital
statement page.

AngelList issues quarterly financial reports (and, at some year-ends, a
dedicated capital-account statement) for proper funds. Both formats embed
a per-LP capital statement — "Partner's Capital Statement" inside the
quarterly report, "Limited Partner's Capital Statement" in the dedicated
PDF — whose ending balance is the partner's capital account at **fair
value** (the roll-forward includes a "Net change in unrealized
gains/(losses)" line). That is the closest on-source FMV mark a fund
position has: the K-1 CSV only carries the tax-basis capital account.

Text is extracted with `pdftotext -layout` (poppler-utils). All functions
are pure (text in, dict out) except `pdf_text`, which shells out. Nothing
here carries source data.
"""
from __future__ import annotations

import re
from pathlib import Path

from collectorkit import pdftotext


def pdf_text(path: Path) -> str:
    """Extract layout-preserving text from a PDF via pdftotext. Empty on any
    failure (missing poppler, encrypted PDF, …) — the caller treats an
    unparseable doc as metadata-only."""
    try:
        return pdftotext.layout_text(path, timeout=120)
    except pdftotext.ExtractionError:
        return ""


def _cents(s: str):
    """'145,761' / '(273)' / '1,234.56' -> minor units (cents); None if not
    numeric. Parenthesised values are negative."""
    s = (s or "").strip()
    neg = s.startswith("(") and s.endswith(")")
    s = s.strip("()").replace("$", "").replace(",", "").strip()
    if not s or s in ("-", "—"):
        return None
    try:
        val = float(s)
    except ValueError:
        return None
    return int(round((-val if neg else val) * 100))


_NUM = r"\(?[\d,]+(?:\.\d+)?\)?"

# The ending-balance row. Both formats phrase it as "Capital account
# balance" followed by either ", <date>" or " at <date>"; the first money
# column is the period figure (the other columns repeat it over longer
# windows).
_BALANCE_RE = re.compile(
    rf"(?im)^\s*capital account balance(?:,| at)[^$\n]*\$\s*({_NUM})")

# Cumulative capital called, both phrasings:
#   "Capital Called Through December 31, 2025   $ 150,000"   (dedicated)
#   "Paid in capital, since inception             100,000"   (quarterly)
_CALLED_RE = re.compile(
    rf"(?im)^\s*(?:capital called through[^$\n]*\$|paid in capital, since inception[^\d(]*)\s*({_NUM})")

# Cumulative distributions, both phrasings (negative in the source —
# outflows to the LP):
#   "Distributions Through December 31, 2025  $ (273)"       (dedicated)
#   "Distributions, since inception             (238)"       (quarterly)
_DIST_RE = re.compile(
    rf"(?im)^\s*distributions(?: through[^$\n]*\$|, since inception[^\d(]*)\s*({_NUM})")


def parse_partner_capital_statement(text: str):
    """The per-LP capital statement figures from a fund report's text:
    {'ending_capital_cents', 'contributed_cents', 'distributions_cents'}
    (the last two None when absent; distributions returned as a positive
    cumulative, matching the K-1 convention). None when no ending-balance
    row is present (not a capital statement)."""
    m = _BALANCE_RE.search(text)
    if not m:
        return None
    ending = _cents(m.group(1))
    if ending is None:
        return None
    called = _CALLED_RE.search(text)
    dist = _DIST_RE.search(text)
    dist_cents = _cents(dist.group(1)) if dist else None
    return {
        "ending_capital_cents": ending,
        "contributed_cents": _cents(called.group(1)) if called else None,
        "distributions_cents": abs(dist_cents) if dist_cents is not None else None,
    }
