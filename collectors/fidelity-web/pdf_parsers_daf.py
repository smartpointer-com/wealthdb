"""
Parser for Fidelity Charitable Donor-Advised Fund statement PDFs.

The DAF walk saves the quarterly + year-end Giving Account statements
under ``<dump>/daf/<account_key>/documents/STATEMENT_*.pdf`` (DESIGN.md
§12). Page 1 of every statement carries an ``Account Summary`` (a
beginning → ending value reconciliation for the period) and a
``GivingAccount Pool Holdings`` table with per-pool units, unit price,
and period-begin/-end market values — enough to reconstruct
point-in-time pool positions for every quarter the archive covers.

Layout (stable across the archive; digit shapes observed live):

    Beginning Value as of April 1, 2023 $ 1,234.56
    Contributions Settled After Previous Period End $ 0.00
    Grant Distributions $ ( 1,234.56 )
    Miscellaneous Transactions $ 0.00
    Change in Account Value $ 123.45
    Ending Value as of June 30, 2023 $ 1,234.56
    GivingAccount Pool Holdings:
    Units Price per Unit Total Value Total Value
    June 30, 2023 June 30, 2023 April 1, 2023 June 30, 2023
    <Pool name> 1,234.567 $ 12.34 $ 1,234.56 $ 1,234.56
    Total Value $ 1,234.56 $ 1,234.56

A pool name can wrap onto the following line (the numeric columns stay
with the first line), and parenthesized amounts are negative. The
import gate is the fleet reconciliation rule: the pool end-values must
sum to the table's Total Value, which must equal the summary's Ending
Value — a statement that fails the gate is skipped and logged, never
imported (see load._load_daf_historical).

Architecture mirrors ``pdf_parsers``: the text-level parsers are pure
functions of strings (tested against hand-crafted fixtures);
``parse_daf_statement_pdf(path)`` opens the PDF via pdfplumber and
delegates.
"""

from __future__ import annotations

import re
from dataclasses import dataclass

from collectorkit.pdf import extract_text_pdfplumber as _extract_pdf_text

from collectorkit.statement_period import MONTH_NUMS as _MONTH_NUMS

# Coarse manual epoch for load.py's parse-cache namespace; automatic
# invalidation rides the source fingerprint (see
# load._DAF_STATEMENT_PARSER_VERSION / collectorkit.srcfp).
PARSER_VERSION = "1"

# "Ending Value as of June 30, 2023 $ 1,234.56". The same shape with
# "Beginning" anchors the period start; the other summary lines carry
# a label + one amount.
_ENDING_VALUE_RE = re.compile(
    r"^Ending Value as of "
    r"(?P<month>January|February|March|April|May|June|July|August|"
    r"September|October|November|December)\s+"
    r"(?P<day>\d{1,2}),\s*(?P<year>\d{4})\s*"
    r"\$\s*(?P<amount>\(?\s*[\d,]+\.\d{2}\s*\)?)$"
)

# A pool holdings row: name (may itself contain digits/percent signs,
# e.g. "Asset Allocation 85%"), then units (3 decimals), then three
# $-amounts: unit price, period-begin value, period-end value.
_POOL_ROW_RE = re.compile(
    r"^(?P<name>.+?)\s+"
    r"(?P<units>[\d,]+\.\d{3})\s+"
    r"\$\s*(?P<price>[\d,]+\.\d{2})\s+"
    r"\$\s*(?P<begin>\(?\s*[\d,]+\.\d{2}\s*\)?)\s+"
    r"\$\s*(?P<end>\(?\s*[\d,]+\.\d{2}\s*\)?)$"
)

# The table's closing line: "Total Value $ <begin> $ <end>".
_TOTAL_VALUE_RE = re.compile(
    r"^Total Value\s+\$\s*(?P<begin>[\d,]+\.\d{2})\s+"
    r"\$\s*(?P<end>[\d,]+\.\d{2})$"
)

_HOLDINGS_HEADER = "GivingAccount Pool Holdings:"

# A wrapped-name continuation line: letters/punctuation only — no
# digits, no dollar signs (e.g. the "Equity" tail of
# "Asset Allocation 85% Equity").
_CONTINUATION_RE = re.compile(r"^[A-Za-z][A-Za-z &/\-().]*$")


def _parse_amount(tok):
    """Parse '$'-column text: '1,234.56' or the parenthesized negative
    '( 1,234.56 )'. Returns float."""
    t = tok.strip()
    neg = t.startswith("(")
    t = t.strip("() ").replace(",", "")
    v = float(t)
    return -v if neg else v


@dataclass
class PoolRow:
    description: str
    units: float
    unit_price: float
    begin_value: float
    end_value: float


def parse_ending_value(text):
    """Return ``(as_of "YYYY-MM-DD", ending_value)`` from the Account
    Summary's Ending Value line, or ``(None, None)``."""
    for line in text.splitlines():
        m = _ENDING_VALUE_RE.match(line.strip())
        if m:
            as_of = (f"{int(m['year']):04d}-{_MONTH_NUMS[m['month']]:02d}-"
                     f"{int(m['day']):02d}")
            return as_of, _parse_amount(m["amount"])
    return None, None


def parse_pool_rows(text):
    """Return ``(rows, total_begin, total_end)`` from the
    ``GivingAccount Pool Holdings`` table. ``rows`` is a list of
    PoolRow; the totals are the table's own Total Value line (None
    when absent). Wrapped pool names are re-joined: a letters-only
    line directly after a parsed row extends that row's description."""
    lines = [ln.strip() for ln in text.splitlines()]
    try:
        start = next(i for i, ln in enumerate(lines)
                     if ln.startswith(_HOLDINGS_HEADER))
    except StopIteration:
        return [], None, None
    rows = []
    total_begin = total_end = None
    for line in lines[start + 1:]:
        m = _TOTAL_VALUE_RE.match(line)
        if m:
            total_begin = _parse_amount(m["begin"])
            total_end = _parse_amount(m["end"])
            break
        m = _POOL_ROW_RE.match(line)
        if m:
            rows.append(PoolRow(
                description=m["name"].strip(),
                units=_parse_amount(m["units"]),
                unit_price=_parse_amount(m["price"]),
                begin_value=_parse_amount(m["begin"]),
                end_value=_parse_amount(m["end"]),
            ))
        elif rows and _CONTINUATION_RE.match(line):
            rows[-1].description += " " + line
    return rows, total_begin, total_end


def reconcile(ending_value, rows, total_end, *, tolerance=0.01):
    """The import gate: pool end-values must sum to the table's Total
    Value, which must equal the summary's Ending Value (each within a
    cent-rounding tolerance per row). Returns (ok, reason)."""
    if ending_value is None:
        return False, "no Ending Value line"
    if total_end is None:
        return False, "no Total Value line"
    tol = tolerance * max(1, len(rows))
    pool_sum = sum(r.end_value for r in rows)
    if abs(pool_sum - total_end) > tol:
        return False, (f"pool sum {pool_sum:.2f} != table total "
                       f"{total_end:.2f}")
    if abs(total_end - ending_value) > tol:
        return False, (f"table total {total_end:.2f} != ending value "
                       f"{ending_value:.2f}")
    return True, None


def parse_daf_statement_pdf(path):
    """Open a DAF statement PDF and return a structured dict:

        {
            "path": "<absolute path>",
            "as_of_date": "YYYY-MM-DD" | None,
            "ending_value": float | None,
            "reconciled": bool,
            "reconcile_error": str | None,
            "pools": [
                {"description", "quantity", "price", "market_value"},
                ...
            ],
        }

    ``pools`` carries the period-END holdings (the snapshot). The
    caller enforces the reconciliation gate — an unreconciled parse is
    returned, not raised, so the loader can log the reason and skip."""
    text = _extract_pdf_text(str(path))
    as_of, ending = parse_ending_value(text)
    rows, _total_begin, total_end = parse_pool_rows(text)
    ok, reason = reconcile(ending, rows, total_end)
    return {
        "path": str(path),
        "as_of_date": as_of,
        "ending_value": ending,
        "reconciled": ok,
        "reconcile_error": reason,
        "pools": [
            {
                "description": r.description,
                "quantity": r.units,
                "price": r.unit_price,
                "market_value": r.end_value,
            }
            for r in rows
        ],
    }
