"""Parsers for Schwab year-end tax forms (the 1099 Composite).

Today this covers the **1099-B** section — sales / dispositions, the
authoritative annual record of *what was sold*, carrying **cost basis
and acquisition date per lot**. The brokerage-statement parser only
sees sales that print as activity rows; a sale that shows up only as
a position delta is invisible to it. The 1099-B closes that gap and
adds the cost-basis-vs-proceeds discriminator the gold returns layer
needs for transferred, gifted and long-held positions.

Schwab ships the 1099 Composite in three bronze formats: a PDF (the
human copy), an OFX-2.x **XML**, and a flat **CSV**. The XML and CSV
are machine-readable twins of the same data. We prefer the **XML**
(cleaner per-field structure, an explicit "Various" acquisition flag,
and a `TAXYEAR` element) and fall back to the CSV; the PDF is left as
an opaque document.

Output: one normalised row dict per 1099-B lot, shaped for the silver
`transactions` loader (`source='form_1099b'`). See `_lot_row` for the
field contract.

No PII lives in this module — it parses whatever the bronze file holds.
Real names / numbers land only in the *local* silver DB, never in
tracked source or tests; the test fixtures are wholly synthetic.
"""
from __future__ import annotations

import csv
import io
import logging
import re
import xml.etree.ElementTree as ET
from collections import Counter
from datetime import datetime
from pathlib import Path

from numparse import parse_amount

log = logging.getLogger("schwab-web.tax_form_parsers")

_YYYYMMDD_RE = re.compile(r"^\s*(\d{4})(\d{2})(\d{2})\s*$")
_MDY_RE = re.compile(r"^\s*(\d{1,2})/(\d{1,2})/(\d{4})\s*$")
# Leading quantity in a 1a "Description of property" cell, e.g.
# "100.00 EXAMPLE CORP CLASS A" or "1,234.5 SOME FUND".
_QTY_NAME_RE = re.compile(r"^\s*([\d,]+(?:\.\d+)?)\s+(.*\S)\s*$")
_TRUTHY = {"Y", "YES", "TRUE", "1", "X", "CHECKED"}


# ============================================================
# Small value helpers
# ============================================================

def _to_float(s) -> float | None:
    """Parse a 1099 numeric cell to float. Returns None for
    blank / non-numeric input. Strips $ and thousands commas."""
    return parse_amount(s, dollar_first=True)


def _iso_from_yyyymmdd(s: str | None) -> str | None:
    """OFX date `YYYYMMDD` → ISO `YYYY-MM-DD`. None on bad input."""
    if not s:
        return None
    m = _YYYYMMDD_RE.match(str(s))
    if not m:
        # Some OFX writers append a time / TZ ("20211001120000[0:GMT]").
        digits = re.match(r"\s*(\d{8})", str(s))
        if not digits:
            return None
        m = _YYYYMMDD_RE.match(digits.group(1))
        if not m:
            return None
    y, mo, d = m.groups()
    try:
        datetime(int(y), int(mo), int(d))
    except ValueError:
        return None
    return f"{y}-{mo}-{d}"


def _iso_from_mdy(s: str | None) -> str | None:
    """CSV date `MM/DD/YYYY` → ISO `YYYY-MM-DD`. None on bad input."""
    if not s:
        return None
    m = _MDY_RE.match(str(s))
    if not m:
        return None
    mo, d, y = m.groups()
    try:
        datetime(int(y), int(mo), int(d))
    except ValueError:
        return None
    return f"{int(y):04d}-{int(mo):02d}-{int(d):02d}"


def _norm_term(s: str | None) -> str | None:
    """Normalise the holding-period label to 'LONG' / 'SHORT'.

    XML `LONGSHORT` is already 'LONG'/'SHORT'; CSV box 2 renders
    'Long Term' / 'Short Term'. Anything else passes through
    upper-cased (e.g. 'ORDINARY' / 'UNKNOWN')."""
    if not s:
        return None
    t = s.strip().upper()
    if t.startswith("LONG"):
        return "LONG"
    if t.startswith("SHORT"):
        return "SHORT"
    return t or None


def _split_qty_name(desc: str) -> tuple[float | None, str]:
    """Split a 1a description ('100.00 EXAMPLE CORP') into
    (quantity, security_name). When there is no leading quantity,
    returns (None, whole-string)."""
    if not desc:
        return None, ""
    m = _QTY_NAME_RE.match(desc)
    if not m:
        return None, desc.strip()
    return _to_float(m.group(1)), m.group(2).strip()


def _modal_year(iso_dates: list[str | None]) -> int | None:
    """The dominant calendar year across a list of ISO dates — the
    1099-B tax year derived from the form's *content* (every lot on a
    1099-B for tax year N was sold in year N). Used when no explicit
    TAXYEAR is available (CSV) or as a cross-check."""
    years = Counter()
    for d in iso_dates:
        if d and len(d) >= 4 and d[:4].isdigit():
            years[int(d[:4])] += 1
    if not years:
        return None
    return years.most_common(1)[0][0]


# ============================================================
# Lot → normalised silver row
# ============================================================

def _lot_row(*, security_name: str, sale_description: str | None,
             quantity: float | None, proceeds: float | None,
             cost_basis_raw: float | None, basis_not_shown: bool,
             noncovered: bool, acquired: str | None, date_sold: str | None,
             term: str | None, wash_sale: float | None,
             accrued: float | None, form_8949_code: str | None,
             tax_year: int | None, source_format: str) -> dict:
    """Build the normalised row dict the silver loader consumes for
    `source='form_1099b'`.

    The promoted `cost_basis` is set to None when the basis is a
    *placeholder* — Schwab renders `0.00` for noncovered lots whose
    basis it does not know (BASISNOTSHOWN). A genuine $0 basis on a
    *covered* lot is preserved.
    The raw value and both flags are always kept in the payload so the
    gold layer can tell a real zero from an unknown one.

    `date`/`amount`/`description` feed the loader's synthetic-activity_id
    derivation; `kind='Sale'` maps to the gold sell taxonomy."""
    if cost_basis_raw is not None and basis_not_shown and cost_basis_raw == 0:
        cost_basis = None
    else:
        cost_basis = cost_basis_raw
    desc = sale_description or (
        f"{quantity} {security_name}".strip() if security_name else security_name
    )
    return {
        "date": date_sold,            # ISO — silver timestamp + id input
        "amount": proceeds,           # proceeds = flow magnitude + id input
        "description": desc,          # id input
        "symbol": None,               # 1099-B carries no ticker / CUSIP
        "kind": "Sale",               # gold webKind: Sale → TxKindSell
        "instrument_key": None,       # name-only; gold resolves by name
        "security_name": security_name,
        "sale_description": sale_description,
        "quantity": quantity,
        "proceeds": proceeds,
        "cost_basis": cost_basis,
        "cost_basis_raw": cost_basis_raw,
        "basis_not_shown": basis_not_shown,
        "noncovered": noncovered,
        "acquired_date": acquired,    # ISO | 'Various' | None
        "date_sold": date_sold,
        "term": term,                 # 'LONG' | 'SHORT' | other | None
        "wash_sale_disallowed": wash_sale,
        "accrued_market_discount": accrued,
        "form_8949_code": form_8949_code,
        "tax_year": tax_year,
        "source_format": source_format,
    }


# ============================================================
# XML (OFX 2.x) — the preferred source
# ============================================================

def parse_1099b_xml(xml_text: str) -> dict:
    """Parse the 1099-B section of an OFX-2.x 1099 Composite XML.

    Returns {"tax_year": int|None, "lots": [row, ...]}. A 1099 with
    no 1099-B section (e.g. interest/dividends only) yields an empty
    lot list. Each lot is one `PROCDET_V100` (proceeds detail)."""
    try:
        root = ET.fromstring(xml_text)
    except ET.ParseError as e:
        log.warning("1099 XML parse error: %s", e)
        return {"tax_year": None, "lots": []}

    b = next(iter(root.iter("TAX1099B_V100")), None)
    if b is None:
        return {"tax_year": None, "lots": []}

    ty_text = (b.findtext("TAXYEAR") or "").strip()
    tax_year = int(ty_text) if ty_text.isdigit() else None

    lots: list[dict] = []
    for pd in b.iter("PROCDET_V100"):
        date_sold = _iso_from_yyyymmdd(pd.findtext("DTSALE"))
        if date_sold is None:
            log.debug("1099-B XML lot with no parseable DTSALE — skipping")
            continue
        dtaqd = (pd.findtext("DTAQD") or "").strip()
        if dtaqd:
            acquired = _iso_from_yyyymmdd(dtaqd)
        elif pd.find("DTVAR") is not None:
            acquired = "Various"
        else:
            acquired = None
        bns = (pd.findtext("BASISNOTSHOWN") or "").strip().upper() in _TRUTHY
        nonc = (pd.findtext("NONCOVEREDSECURITY") or "").strip().upper() in _TRUTHY
        lots.append(_lot_row(
            security_name=(pd.findtext("SECNAME") or "").strip(),
            sale_description=(pd.findtext("SALEDESCRIPTION") or "").strip() or None,
            quantity=_to_float(pd.findtext("NUMSHRS")),
            proceeds=_to_float(pd.findtext("SALESPR")),
            cost_basis_raw=_to_float(pd.findtext("COSTBASIS")),
            basis_not_shown=bns,
            noncovered=nonc,
            acquired=acquired,
            date_sold=date_sold,
            term=_norm_term(pd.findtext("LONGSHORT")),
            wash_sale=_to_float(pd.findtext("WASHSALELOSSDISALLOWED")),
            accrued=_to_float(pd.findtext("ACCRUEDMKTDISCOUNT")),
            form_8949_code=(pd.findtext("FORM8949CODE") or "").strip() or None,
            tax_year=tax_year,
            source_format="xml",
        ))

    # Fall back to (and cross-check against) the content-derived year.
    derived = _modal_year([l["date_sold"] for l in lots])
    if tax_year is None:
        tax_year = derived
        for l in lots:
            l["tax_year"] = tax_year
    elif derived is not None and derived != tax_year:
        log.warning("1099-B XML TAXYEAR=%s but sold-date year=%s; "
                    "keeping TAXYEAR", tax_year, derived)
    return {"tax_year": tax_year, "lots": lots}


# ============================================================
# CSV — fallback when no XML twin exists
# ============================================================

_FORM_B_RE = re.compile(r"^\s*Form\s*1099\s*B\b", re.I)
_FORM_ANY_RE = re.compile(r"^\s*Form\b", re.I)


def parse_1099b_csv(csv_text: str) -> dict:
    """Parse the 1099-B section of a multi-section 1099 Composite CSV.

    The section is introduced by a 'Form 1099 B' row, followed by a
    box-number header ('1a','1b',...), then a human-label header, then
    one row per lot. We index columns by box number so column-order
    drift doesn't break the mapping. Returns the same shape as
    `parse_1099b_xml`."""
    rows = list(csv.reader(io.StringIO(csv_text)))

    start = next((i for i, r in enumerate(rows)
                  if r and _FORM_B_RE.match(r[0])), None)
    if start is None:
        return {"tax_year": None, "lots": []}

    # Box-number header: first following row whose first cell is '1a'.
    hdr_idx = None
    for i in range(start + 1, min(start + 5, len(rows))):
        if rows[i] and rows[i][0].strip().lower() == "1a":
            hdr_idx = i
            break
    if hdr_idx is None:
        log.warning("1099-B CSV: found section but no '1a' box header")
        return {"tax_year": None, "lots": []}

    col: dict[str, int] = {}
    for j, c in enumerate(rows[hdr_idx]):
        b = c.strip()
        if b and b not in col:
            col[b] = j

    def cell(r: list[str], box: str) -> str:
        j = col.get(box)
        return r[j].strip() if j is not None and j < len(r) else ""

    # The row after the box-number header is the human-readable label
    # row ("Description of property", ...); data begins after it.
    lots: list[dict] = []
    for r in rows[hdr_idx + 2:]:
        if not r or not r[0].strip():
            break
        if _FORM_ANY_RE.match(r[0]):
            break
        date_sold = _iso_from_mdy(cell(r, "1c"))
        if date_sold is None:
            continue
        qty, name = _split_qty_name(cell(r, "1a"))
        acq_raw = cell(r, "1b")
        if acq_raw.lower().startswith("various"):
            acquired: str | None = "Various"
        else:
            acquired = _iso_from_mdy(acq_raw)
        noncovered = bool(cell(r, "5"))
        # Box 12 = "basis reported to IRS"; unchecked ⇒ basis not shown.
        basis_reported = cell(r, "12").strip().upper() in _TRUTHY
        basis_not_shown = noncovered and not basis_reported
        lots.append(_lot_row(
            security_name=name,
            sale_description=cell(r, "1a") or None,
            quantity=qty,
            proceeds=_to_float(cell(r, "1d")),
            cost_basis_raw=_to_float(cell(r, "1e")),
            basis_not_shown=basis_not_shown,
            noncovered=noncovered,
            acquired=acquired,
            date_sold=date_sold,
            term=_norm_term(cell(r, "2")),
            wash_sale=_to_float(cell(r, "1g")),
            accrued=_to_float(cell(r, "1f")),
            form_8949_code=None,
            tax_year=None,
            source_format="csv",
        ))
    tax_year = _modal_year([l["date_sold"] for l in lots])
    for l in lots:
        l["tax_year"] = tax_year
    return {"tax_year": tax_year, "lots": lots}


# ============================================================
# Dispatch
# ============================================================

def parse_1099b(path, fmt: str | None = None) -> dict:
    """Parse a 1099 Composite file (XML preferred, CSV fallback).

    `fmt` is 'xml' / 'csv'; inferred from the extension when omitted.
    Returns {"tax_year": int|None, "lots": [row, ...]}."""
    p = Path(path)
    fmt = (fmt or p.suffix.lstrip(".")).lower()
    text = p.read_text(encoding="utf-8", errors="replace")
    if fmt == "xml":
        return parse_1099b_xml(text)
    if fmt == "csv":
        return parse_1099b_csv(text)
    raise ValueError(f"unsupported 1099 format: {fmt!r}")
