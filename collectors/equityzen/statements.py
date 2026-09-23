#!/usr/bin/env python3
"""PDF parsers for EquityZen fund documents — capital-account statements and
Schedule K-1s — used by load.py to extract fund valuations + tax figures.

Text is extracted with `pdftotext -layout` (poppler-utils). The capital-
account statement is a clean label/value table and parses reliably (the
"Net Ending [Net] Capital Account Balance" line is the fund NAV). The K-1 is
an IRS Form-1065 grid: its Item L capital-account analysis ("Ending capital
account") parses cleanly and is the valuable tax-basis NAV; the Part III box
amounts sit in a form grid a naive scan misreads, so they are not extracted,
and the full K-1 text is not retained (it carries SSN/EIN/address) — see
`parse_k1`.

All functions are pure (text in, dict out) except `pdf_text` which shells to
pdftotext. No values are hard-coded; nothing here carries source data.
"""
from __future__ import annotations

import datetime as dt
import re
from pathlib import Path

from collectorkit import pdftotext


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


def parse_k1(text: str) -> dict:
    """Extract the reliable part of a K-1: the tax year and Item L — the
    partner's capital-account analysis, whose `ending_capital` is the
    tax-basis NAV. The Part III box amounts (income / gains / distributions)
    are NOT extracted: in `pdftotext` output they sit in a form grid and a
    naive label→value scan grabs adjacent box *numbers*, not amounts (e.g.
    "14"/"15"). Rather than store wrong figures we leave them out; they can be
    re-parsed from the bronze PDF when a grid-aware extractor exists. We also
    deliberately do NOT keep the full K-1 text (it carries SSN/EIN/address)."""
    return {
        "tax_year": _tax_year(text),
        "is_final": bool(re.search(r"Final\s+K-?1\s*\n?\s*X", text, re.I)),
        "beginning_capital": _field(text, r"Beginning capital account"),
        "current_year_income": _field(
            text, r"Current year (?:net income \(loss\)|increase \(decrease\))"),
        "withdrawals_distributions": _field(text, r"Withdrawals and distributions"),
        "ending_capital": _field(text, r"Ending capital account"),
    }
