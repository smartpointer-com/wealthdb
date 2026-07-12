#!/usr/bin/env python3
"""
PDF parsers for Relevate quarterly-report documents.

These parsers extract historical position + cash data from the
PDFs that download.py already saves in the bronze tree. The
loader (load.py) reads from the silver `documents` table to find
which PDFs to parse, then writes the results into
`historical_position_snapshots` + `historical_cash_balances`.

Library choice: pypdf with `extract_text(extraction_mode='layout')`.
The Quartalsbericht is iText-produced with a column-aligned
tabular layout that pypdf's layout-mode preserves reliably. The
PDF library bake-off in /tmp showed all candidates (pdfplumber,
pypdf, pypdfium2) extract the same row counts; pypdf was picked
for project consistency (Swissquote uses it), pure-Python install
(no native deps in the Docker image), and adequate speed (~30 ms
per PDF).
"""

from __future__ import annotations

import hashlib
import logging
import re
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

log = logging.getLogger(__name__)


# ============================================================
# Shared regexes
# ============================================================

# ISIN: ISO 6166, 2-letter country code + 9 alphanumerics + check digit.
ISIN_RE = re.compile(r"\b([A-Z]{2}[A-Z0-9]{9}\d)\b")

# Swiss number format: optional minus, digits with thousands separator,
# decimal point + 1+ decimals. The thousands separator in the PDF is the
# typographic right-single-quote U+2019 ('); the loader-side normalisation
# also accepts the ASCII apostrophe (') so synthetic test fixtures don't
# have to embed the Unicode character.
SWISS_THSEP = r"[’']"
NUM_RE_SOURCE = rf"-?\d{{1,3}}(?:{SWISS_THSEP}\d{{3}})*\.\d+"

# 12-char account external id: NNNN.NNNNNN.N
ACCOUNT_RE = re.compile(r"\b(\d{4}\.\d{6}\.\d)\b")

# Swiss date format: DD.MM.YYYY (from 'Valuation date' / 'Bewertungsdatum').
DATE_RE = re.compile(r"\b(\d{2})\.(\d{2})\.(\d{4})\b")

# Page-header noise we strip from the Portfolio Detail walk.
PAGE_NOISE = re.compile(
    r"^\s*(Quartalsbericht|Quarterly Report)\s+.*Page\s+\d+/\d+\s*$"
)

# End-of-section markers in the Portfolio Detail walk.
PORTFOLIO_DETAIL_END = re.compile(
    r"^\s*(Performance|Portfolio activities|Legend|Disclaimer)\s*$"
)


# Position row: after the security name + ISIN, the right side has
# currency (3 chars), units, performance%, allocation%, allocation in CHF.
# A few PDFs miss the units column (zero-unit position?) — handle both
# 5-column and 4-column tails with a relaxed alternation.
POSITION_TAIL = re.compile(
    rf"\s+(?P<ccy>[A-Z]{{3}})"
    rf"\s+(?P<units>{NUM_RE_SOURCE})"
    rf"\s+(?P<perf>{NUM_RE_SOURCE})"
    rf"\s+(?P<alloc_pct>{NUM_RE_SOURCE})"
    rf"\s+(?P<value>{NUM_RE_SOURCE})\s*$"
)

# Cash 'Liquidity CHF' row. Layout-mode text repeats CHF: the security
# name is 'Liquidity CHF' AND the currency column is also 'CHF', with
# variable whitespace between. No units, no performance — just trailing
# allocation_pct + value. Use a non-greedy gap between the name and the
# trailing two numbers to absorb the repeated currency column.
CASH_TAIL = re.compile(
    rf"^\s*Liquidity\s+(?P<ccy>[A-Z]{{3}})\b.*?"
    rf"(?P<alloc_pct>{NUM_RE_SOURCE})"
    rf"\s+(?P<value>{NUM_RE_SOURCE})\s*$"
)

# 'Accrued interest' row: no currency col, no units, just two trailing numbers.
ACCRUED_TAIL = re.compile(
    rf"^\s*Accrued interest"
    rf"\s+(?P<alloc_pct>{NUM_RE_SOURCE})"
    rf"\s+(?P<value>{NUM_RE_SOURCE})\s*$"
)


# Asset-class section headers under "Portfolio Detail" — one-word
# (or short-phrase) lines that introduce a group of positions.
ASSET_CLASS_HEADERS = {
    "Liquidity":     "Liquidity",
    "Stocks":        "Stocks",
    "Bonds":         "Bonds",
    "Real Estate":   "Real Estate",
    "Alternative":   "Alternative",
    "Alternatives":  "Alternatives",
    # German fallbacks in case Relevate ever serves de-locale PDFs:
    "Liquidität":    "Liquidity",
    "Aktien":        "Stocks",
    "Obligationen":  "Bonds",
    "Immobilien":    "Real Estate",
    "Alternative Anlagen": "Alternative",
}


# ============================================================
# Helpers
# ============================================================

def _swiss_num(s: str) -> float:
    """Parse a Swiss-formatted number string into a float. The PDF uses
    U+2019 ('), tests use ASCII apostrophe (')."""
    return float(s.replace("’", "").replace("'", ""))


def _read_layout_text(pdf_path: Path) -> str:
    """All pages joined by newline, using pypdf's layout extraction.

    pypdf is imported lazily so the pure-text parser can be tested
    on hosts without the dependency installed (CI runs inside the
    Docker image where it's always present)."""
    import pypdf  # noqa: PLC0415
    reader = pypdf.PdfReader(str(pdf_path))
    chunks = [
        p.extract_text(extraction_mode="layout") or ""
        for p in reader.pages
    ]
    return "\n".join(chunks)


def _read_plain_text(pdf_path: Path) -> str:
    """Default-mode pypdf extraction. Layout-mode loses the Gutschrifts-
    anzeige value column entirely (only labels survive), so credit-note
    parsing uses the plain-mode extraction which concatenates the right
    column verbatim after the left."""
    import pypdf  # noqa: PLC0415
    reader = pypdf.PdfReader(str(pdf_path))
    chunks = [p.extract_text() or "" for p in reader.pages]
    return "\n".join(chunks)


def _sha256_file(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def _quarter_end_epoch(date_str: str) -> int:
    """Parse 'DD.MM.YYYY' → Unix-seconds UTC midnight."""
    day, month, year = (int(p) for p in date_str.split("."))
    return int(
        datetime(year, month, day, tzinfo=timezone.utc).timestamp()
    )


# ============================================================
# Page-1 metadata extraction
# ============================================================

def _extract_account_external_id(text: str) -> str:
    """Find the 'Reference no.' line and the NNNN.NNNNNN.N value."""
    for line in text.splitlines():
        if "Reference no" in line or "Referenz" in line:
            m = ACCOUNT_RE.search(line)
            if m:
                return m.group(1)
    raise ValueError("Reference no. not found on page 1")


def _extract_valuation_date(text: str) -> int:
    """Find the 'Valuation date' line and parse DD.MM.YYYY → Unix epoch."""
    for line in text.splitlines():
        if "Valuation date" in line or "Bewertungsdatum" in line:
            m = DATE_RE.search(line)
            if m:
                return _quarter_end_epoch(m.group(0))
    raise ValueError("Valuation date not found on page 1")


# ============================================================
# Portfolio Detail section walk
# ============================================================

def _iter_portfolio_detail_lines(text: str):
    """Yield lines inside the Portfolio Detail section, dropping
    page-header noise and the 'Asset Class ... ISIN ... Currency ...'
    column-header line."""
    in_section = False
    for line in text.splitlines():
        if "Portfolio Detail" in line:
            in_section = True
            continue
        if not in_section:
            continue
        if PORTFOLIO_DETAIL_END.match(line):
            in_section = False
            continue
        if PAGE_NOISE.match(line):
            continue
        # Skip the column-header line ("Asset Class ... ISIN ... Currency ...")
        # and a possible "Security ... in % (YTD ...) ... in % ... in CHF"
        # second-line continuation.
        stripped = line.strip()
        if stripped.startswith("Asset Class") and "ISIN" in stripped:
            continue
        if stripped.startswith("Security") and "in %" in stripped:
            continue
        if not stripped:
            continue
        yield line


def _is_asset_class_header(stripped: str) -> str | None:
    """Return the canonical asset_class label if this line is just a
    section header (e.g. 'Stocks', 'Real Estate'), else None."""
    # Direct match.
    if stripped in ASSET_CLASS_HEADERS:
        return ASSET_CLASS_HEADERS[stripped]
    # 'Stocks CHF hedged' — sub-header pattern.
    for needle, canonical in ASSET_CLASS_HEADERS.items():
        if stripped.startswith(needle) and len(stripped) < 30:
            return canonical
    return None


def _parse_position_row(line: str) -> dict | None:
    """Parse a single ISIN-bearing position line into a dict, or None
    if the line doesn't match the expected shape."""
    m_isin = ISIN_RE.search(line)
    if not m_isin:
        return None
    isin = m_isin.group(1)
    isin_start = m_isin.start()

    # Everything before the ISIN is the security name (with some
    # padding whitespace).
    security_name = line[:isin_start].strip()
    if not security_name:
        return None

    # The tail must match: ccy units perf alloc_pct value
    tail = line[m_isin.end():]
    m_tail = POSITION_TAIL.match(tail)
    if not m_tail:
        return None

    return {
        "isin":                isin,
        "security_name":       security_name,
        # The 'currency' field tracks `market_value`'s denomination, NOT
        # the security's trading currency. Relevate's "Allocation in CHF"
        # column is always the portfolio reference (CHF), regardless of
        # whether the instrument trades in USD/EUR/etc. The instrument's
        # native trading currency (from the PDF's "Currency" column) is
        # preserved separately for audit.
        "currency":            "CHF",
        "instrument_currency": m_tail.group("ccy"),
        "units":               _swiss_num(m_tail.group("units")),
        "allocation_pct":      _swiss_num(m_tail.group("alloc_pct")) / 100.0,
        "market_value":        _swiss_num(m_tail.group("value")),
    }


# ============================================================
# Public API
# ============================================================

def _parse_quarterly_report_text(text: str, source_sha256: str) -> dict:
    """Pure text-driven parser, separated from PDF I/O so tests can
    feed synthetic layout-mode text without going through pypdf."""
    account_external_id = _extract_account_external_id(text)
    as_of_date = _extract_valuation_date(text)

    positions: list[dict[str, Any]] = []
    cash: list[dict[str, Any]] = []
    current_asset_class: str = "Unknown"

    for line in _iter_portfolio_detail_lines(text):
        stripped = line.strip()

        # 1. Asset-class section header (advances current_asset_class).
        cls = _is_asset_class_header(stripped)
        if cls is not None:
            current_asset_class = cls
            continue

        # 2. 'Liquidity CHF' cash row.
        m_cash = CASH_TAIL.match(line)
        if m_cash is not None:
            cash.append({
                "balance_kind": "cash",
                "currency":     m_cash.group("ccy"),
                "amount":       _swiss_num(m_cash.group("value")),
                "allocation_pct": _swiss_num(m_cash.group("alloc_pct")) / 100.0,
            })
            continue

        # 3. 'Accrued interest' row.
        m_accrued = ACCRUED_TAIL.match(line)
        if m_accrued is not None:
            # Accrued interest is reported in CHF on the observed
            # corpus (portfolio reference currency).
            cash.append({
                "balance_kind": "accrued_interest",
                "currency":     "CHF",
                "amount":       _swiss_num(m_accrued.group("value")),
                "allocation_pct": _swiss_num(m_accrued.group("alloc_pct")) / 100.0,
            })
            continue

        # 4. ISIN-bearing position row.
        parsed = _parse_position_row(line)
        if parsed is not None:
            parsed["asset_class"] = current_asset_class
            positions.append(parsed)
            continue

        # Anything else under Portfolio Detail is wraparound text
        # we don't need (security-name wrap, totals, footnote refs).

    # Currency is always CHF in the observed corpus; assert from the
    # cash row when present, else from the first position.
    currency = "CHF"
    if cash:
        currency = cash[0]["currency"]
    elif positions:
        currency = positions[0]["currency"]

    return {
        "as_of_date":          as_of_date,
        "account_external_id": account_external_id,
        "currency":            currency,
        "source_sha256":       source_sha256,
        "positions":           positions,
        "cash":                cash,
    }


def parse_quarterly_report(pdf_path: Path) -> dict:
    """
    Parse one Quartalsbericht PDF into structured data.

    Returns a dict with this shape (no values shown in the docs to
    avoid leaking PII, but: see the silver schema in
    migrations/0002_historical_snapshots.sql for the target
    columns):

        {
          "as_of_date":           int,            # Unix-seconds UTC at quarter-end midnight
          "account_external_id":  str,            # 'NNNN.NNNNNN.N' from page-1 'Reference no.'
          "currency":             str,            # 'CHF' in the observed corpus
          "source_sha256":        str,            # hex digest of the PDF bytes
          "positions": [
              {
                "isin":           str,            # ISO 6166
                "security_name":  str,
                "asset_class":    str,            # 'Liquidity' / 'Stocks' / 'Bonds' / 'Real Estate' / 'Alternative'
                "currency":       str,
                "units":          float,
                "allocation_pct": float,          # 0..1 fraction
                "market_value":   float,          # in `currency`
              },
              ...
          ],
          "cash": [                               # zero or more entries (typically 1: 'cash', sometimes also 'accrued_interest')
              {
                "balance_kind":   str,            # 'cash' | 'accrued_interest'
                "currency":       str,
                "amount":         float,
              },
              ...
          ],
        }

    Raises ValueError when page-1 metadata (Reference no.,
    Valuation date) is missing — the loader catches that and skips
    the file.
    """
    text = _read_layout_text(pdf_path)
    source_sha256 = _sha256_file(pdf_path)
    return _parse_quarterly_report_text(text, source_sha256)


# ============================================================
# Credit notes (Gutschriftsanzeige) — contribution events
# ============================================================
#
# These PDFs are visually two-column (label column on the left,
# value column on the right). Layout-mode extraction drops the
# value column entirely; default-mode extraction concatenates the
# right column after the left, so the values arrive in a known
# document order:
#
#     [labels...]
#     CHF <amount>
#     DD.MM.YYYY            (Valuta / settlement date)
#     <foundation name>     (e.g. 'PensFree')
#     NNNN.NNNNNN.N         (Referenznummer / account)
#     [PII fields: name, address, phone, email, birth date, AHV]
#
# We anchor on the 'CHF <amount>' line; the next two non-empty
# value-column lines are valuta + foundation. The account ID lives
# elsewhere on the page but uniquely matches NNNN.NNNNNN.N.
#
# All PII fields are intentionally NOT extracted — neither the
# parser nor the loader needs to see name / address / phone /
# email / birth date / AHV-Nr. to produce the transaction row.


CREDIT_AMOUNT_RE = re.compile(
    rf"\bCHF\s+(?P<amount>{NUM_RE_SOURCE})\s*$"
)


def _parse_credit_note_text(text: str, source_sha256: str) -> dict:
    """Pure text-driven credit-note parser.

    Reads the right-column values (post-label) from pypdf's default
    extraction, returning:

        {
          "occurred_at":          int,    # Unix-seconds UTC at Valuta midnight
          "account_external_id":  str,    # NNNN.NNNNNN.N from 'Referenznummer'
          "currency":             "CHF",
          "amount":               float,  # the 'Betrag' value (gross == net for contributions)
          "kind":                 "contribution",
          "source_sha256":        str,
        }

    Raises ValueError if any of the three required fields is missing.
    """
    # Account id: first NNNN.NNNNNN.N anywhere in the page.
    m_acct = ACCOUNT_RE.search(text)
    if not m_acct:
        raise ValueError("credit note: Referenznummer not found")
    account_external_id = m_acct.group(1)

    # Amount: the 'CHF <num>' line that sits in the right column.
    m_amt = None
    for line in text.splitlines():
        m = CREDIT_AMOUNT_RE.search(line.strip())
        if m is not None:
            m_amt = m
            break
    if m_amt is None:
        raise ValueError("credit note: amount (CHF) line not found")
    amount = _swiss_num(m_amt.group("amount"))

    # Valuta date: the first DD.MM.YYYY that appears AFTER the amount
    # line in document order. Skips the page-header date (none on
    # Gutschriftsanzeige) and the birth-date line (which always
    # follows the Valuta in the right column).
    lines = text.splitlines()
    amt_idx = next(
        i for i, ln in enumerate(lines) if CREDIT_AMOUNT_RE.search(ln.strip())
    )
    valuta_match = None
    for ln in lines[amt_idx + 1:]:
        m = DATE_RE.search(ln)
        if m is not None:
            valuta_match = m
            break
    if valuta_match is None:
        raise ValueError("credit note: Valuta date not found after amount")
    occurred_at = _quarter_end_epoch(valuta_match.group(0))

    return {
        "occurred_at":         occurred_at,
        "account_external_id": account_external_id,
        "currency":            "CHF",
        "amount":              amount,
        "kind":                "contribution",
        "source_sha256":       source_sha256,
    }


def parse_credit_note(pdf_path: Path) -> dict:
    """Parse one Gutschriftsanzeige PDF into a contribution event.

    See `_parse_credit_note_text` for the return shape. Uses the
    default (non-layout) pypdf extraction since layout-mode loses
    the value column on these two-column documents.
    """
    text = _read_plain_text(pdf_path)
    source_sha256 = _sha256_file(pdf_path)
    return _parse_credit_note_text(text, source_sha256)
