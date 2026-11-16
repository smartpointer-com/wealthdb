#!/usr/bin/env python3
"""PDF parsers for EquityZen fund documents — capital-account statements and
Schedule K-1s — used by load.py to extract fund valuations + tax figures.

Text is extracted with `pdftotext -layout` (poppler-utils). The capital-
account statement is a clean label/value table and parses reliably (the
"Net Ending [Net] Capital Account Balance" line is the fund NAV). The K-1 is
an IRS Form-1065 grid: its Item L capital-account analysis ("Ending capital
account") is the valuable tax-basis NAV. It is read from the text, and a
figure the text misses is read from the face page's word boxes by the
fleet's K-1 reader (collectorkit.k1). The Part III box amounts are not
kept, and neither is the full K-1 text (it carries SSN/EIN/address) — see
`parse_k1`.

All functions are pure (text or word boxes in, dict out) except `pdf_text`
which shells to pdftotext. No values are hard-coded; nothing here carries
source data.
"""
from __future__ import annotations

import datetime as dt
import re
from pathlib import Path

from collectorkit import k1, pdftotext
from collectorkit.words import Extracted


def pdf_text(path: Path) -> str:
    """Extract layout-preserving text from a PDF via pdftotext. Empty on any
    failure (missing poppler, encrypted PDF, zip, …) — the caller treats an
    unparseable doc as metadata-only."""
    try:
        return pdftotext.layout_text(path, timeout=120)
    except pdftotext.ExtractionError:
        return ""


def classify(text: str) -> str | None:
    """'capital_account' | 'k1' | None, from the document title text."""
    tu = text.upper()
    if "STATEMENT OF CAPITAL ACCOUNT" in tu:
        return "capital_account"
    if "SCHEDULE K-1" in tu or "PARTNER'S SHARE OF" in tu.replace("’", "'"):
        return "k1"
    return None


# ---- number / label helpers ----------------------------------------------

def _num(rest: str):
    """First numeric token in `rest`: `1,234.56`, `$1,234`, `(123.45)` →
    negative, a bare `-` → 0.0. None if no number."""
    s = rest.replace("$", " ").replace(",", "")
    m = re.search(r"\(\s*[\d.]+\s*\)|[\d][\d.]*|(?<=\s)-(?=\s|$)", s)
    if not m:
        return None
    t = m.group(0).replace(" ", "")
    if t == "-":
        return 0.0
    try:
        return -float(t.strip("()")) if t.startswith("(") else float(t)
    except ValueError:
        return None


def _field(text: str, label_regex: str):
    """Value immediately after the first line matching `label_regex`."""
    for line in text.splitlines():
        m = re.search(label_regex, line)
        if m:
            return _num(line[m.end():])
    return None


def _period_end(text: str) -> str | None:
    """'For the Quarter Ended June 30 2025' → '2025-06-30' (ISO)."""
    m = re.search(r"Ended\s+([A-Z][a-z]+)\s+(\d{1,2}),?\s+(\d{4})", text)
    if not m:
        return None
    try:
        return dt.datetime.strptime(
            f"{m.group(1)} {m.group(2)} {m.group(3)}", "%B %d %Y").date().isoformat()
    except ValueError:
        return None


# ---- capital-account statement (the fund NAV source) ---------------------

# Label drifted across years: early "Net Ending Capital Account Balance",
# later "Net Ending Net Capital Account Balance". Either is the final NAV.
NAV_LABEL = r"Net Ending (?:Net )?Capital Account Balance"


def parse_capital_account(text: str) -> dict:
    return {
        "period_end": _period_end(text),
        "beginning_balance": _field(text, r"Beginning Net Capital Account Balance"),
        "contributions": _field(text, r"Capital Contributions"),
        "withdrawals": _field(text, r"Capital (?:Withdrawals|Distributions)"),
        "transfers": _field(text, r"(?:Capital )?Transfers"),
        "profit_loss": _field(text, r"Profit \(Loss\)"),
        "carried_interest": _field(text, r"Accrued Carried Interest"),
        "ending_nav": _field(text, NAV_LABEL),
    }


# ---- Schedule K-1 (tax figures; Item L is the reliable part) --------------

def _tax_year(text: str) -> int | None:
    for pat in (r"Calendar Year\s+(20\d{2})",
                r"For\s+calendar\s+year\s+(20\d{2})",
                r"tax year\s+(20\d{2})"):
        m = re.search(pat, text, re.I)
        if m:
            return int(m.group(1))
    return None


# A K-1 amount after its caption: wrapped in parentheses, digits with an
# optional minus sign printed against them, or a bare dash (zero).
_K1_AMOUNT = re.compile(r"\(\s*[\d.]+\s*\)|-?[\d][\d.]*|(?<=\s)-(?=\s|$)")


def _k1_field(text: str, label_regex: str) -> float | None:
    """The amount after the first line matching `label_regex`, as `_field`
    reads it, except that a printed minus sign (`-3,127.`) is kept."""
    for line in text.splitlines():
        m = re.search(label_regex, line)
        if m:
            rest = line[m.end():].replace("$", " ").replace(",", "")
            n = _K1_AMOUNT.search(rest)
            if not n:
                return None
            t = n.group(0).replace(" ", "")
            if t == "-":
                return 0.0
            try:
                return (-float(t.strip("()")) if t.startswith("(")
                        else float(t))
            except ValueError:
                return None
    return None


def _figure(raw: str | None, *, negate: bool = False) -> float | None:
    """A K-1 figure as collectorkit.k1 reads it (a decimal string) as a
    float, negated where the text reading gives it a minus sign."""
    if raw is None:
        return None
    return -float(raw) if negate else float(raw)


# Item L as read from the text, and the collectorkit.k1 figure that fills
# each one the text leaves empty: (key, caption, k1 column, negated).
_ITEM_L = (
    ("beginning_capital", r"Beginning capital account",
     "beginning_capital", False),
    ("current_year_income",
     r"Current year (?:net income \(loss\)|increase \(decrease\))",
     "net_income", False),
    ("withdrawals_distributions", r"Withdrawals and distributions",
     "distributions", True),
    ("ending_capital", r"Ending capital account", "ending_capital", False),
)


def parse_k1(text: str, boxes: Extracted | None) -> dict:
    """Extract the reliable part of a K-1: the tax year, the final-K-1 mark
    and Item L — the partner's capital-account analysis, whose
    `ending_capital` is the tax-basis NAV.

    Item L is read from the text first: the first amount after each
    caption, a printed minus sign kept. Where a preparer prints the amount
    off its caption's line, the text leaves that field empty, and the face
    page's word boxes fill it (`boxes`, as `pdftotext -bbox` extracts the
    PDF; None when it cannot): collectorkit.k1 places an amount on the
    caption line nearest it. The withdrawals line, which the form prints
    inside parentheses, is negative either way. The Part III box amounts
    (income / gains / distributions) are not kept, and the full K-1 text is
    deliberately NOT kept either (it carries SSN/EIN/address)."""
    out = {
        "tax_year": _tax_year(text),
        "is_final": bool(re.search(r"Final\s+K-?1\s*\n?\s*X", text, re.I)),
    }
    face = None
    for key, caption, column, negate in _ITEM_L:
        out[key] = _k1_field(text, caption)
        if out[key] is None and boxes is not None:
            if face is None:
                face = k1.parse(boxes) or {}
            out[key] = _figure(face.get(column), negate=negate)
    return out
