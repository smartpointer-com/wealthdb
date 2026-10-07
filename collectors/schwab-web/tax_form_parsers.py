"""Parsers for Schwab year-end tax documents: the 1099 Composite, the
Year-End Summary and the Gain/Loss Report.

The **1099-B** section of the 1099 Composite covers sales /
dispositions, the authoritative annual record of *what was sold*,
carrying **cost basis and acquisition date per lot**. The
brokerage-statement parser only sees sales that print as activity
rows; a sale that shows up only as a position delta is invisible to
it. The 1099-B closes that gap and adds the cost-basis-vs-proceeds
discriminator the gold returns layer needs for transferred, gifted and
long-held positions.

Schwab ships the 1099 Composite in three bronze formats: a PDF (the
human copy), an OFX-2.x **XML**, and a flat **CSV**. The XML and CSV
are machine-readable twins of the same data. We prefer the **XML**
(cleaner per-field structure, an explicit "Various" acquisition flag,
and a `TAXYEAR` element) and fall back to the CSV. The PDF's 1099-B
pages are not read; its Year-End Summary half is.

Output: one normalised row dict per 1099-B lot, shaped for the silver
`transactions` loader (`source='form_1099b'`). See `_lot_row` for the
field contract.

The Year-End Summary and the Gain/Loss Report are PDFs only. Their
realized-lot sections also list the lots the 1099-B leaves out; see
"Realized-lot reports" below.

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

from collectorkit.pdf import extract_text_pdfium
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
    derived = _modal_year([lot["date_sold"] for lot in lots])
    if tax_year is None:
        tax_year = derived
        for lot in lots:
            lot["tax_year"] = tax_year
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
    tax_year = _modal_year([lot["date_sold"] for lot in lots])
    for lot in lots:
        lot["tax_year"] = tax_year
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


# ============================================================
# Realized-lot reports: Year-End Summary and Gain/Loss Report
# ============================================================
#
# Two PDF reports list every realized lot of a tax year, the ones the
# 1099-B leaves out included: sales in accounts that get no 1099-B, and
# lots "not reported on Form 1099-B" whose basis the 1099-B omits.
#
#   * The Year-End Summary, on its own or as the second half of the
#     1099 Composite PDF, has "Short-Term / Long-Term Realized Gain or
#     (Loss)" sections. A section's subtitle says whether its lots are
#     covered and which Form 8949 box they belong in. A lot prints as
#       [DESCRIPTION] [CUSIP] QTY ACQUIRED SOLD $ PROCEEDS $ COST
#           (-- | $ WASH) [$ MARKET DISCOUNT] $ GAIN
#     where the description can wrap onto the lines above, an option
#     prints its contract ("XMPL 01/16/2026 50.00 C") instead, and the
#     amounts after the cost can wrap onto the next line.
#   * The Gain/Loss Report has one "Realized Gain or (Loss)" section with
#     Short-Term and Long-Term parts, headed by the account's cost-basis
#     methods ("Mutual Funds: First In First Out"). A lot prints as
#       [DESCRIPTION: TICKER] QTY ACQUIRED SOLD $PROCEEDS $COST $GAIN
#
# Both print endnote letters among the amounts, glued to one ("$ 100.00t",
# "t($5.00)") or standing alone, and an "S" beside a short sale's
# quantity. Page furniture (the holder block, page numbers) sits between
# the pages of a section and is skipped.

_REPORT_LOT_RE = re.compile(
    r"^(?P<head>.*?)\s*(?P<qty>[\d,]*\.\d+)\s*(?P<short>S?)\s*"
    r"(?P<acq>\d{2}/\d{2}/\d{2}|Various|VARIOUS)\s+(?P<sold>\d{2}/\d{2}/\d{2})"
    r"\s+(?P<cells>[A-Za-z]?[($].*)$")
# One amount cell: "$ 1,234.56", "$1,234.56", "($12.00)", "$ (12.00)",
# with an endnote letter glued before or after; or a dash / "Missing"
# where the report prints no amount. An endnote letter can also stand
# alone among the cells.
_REPORT_CELL_RE = re.compile(
    r"(?P<pre>[A-Za-z]?)(?P<amt>\(?\$\s*\(?[\d,]+\.\d{2}\)?)(?P<post>[A-Za-z]{0,2})"
    r"|(?P<none>--|Missing)|(?<!\S)(?P<mark>[A-Za-z])(?!\S)")
_REPORT_CUSIP_RE = re.compile(r"^[A-Z0-9]{8}\d$")
_REPORT_OPTION_RE = re.compile(
    r"^(?P<under>[A-Z][A-Z0-9./]*)\s+(?P<exp>\d{2}/\d{2}/\d{4})\s+"
    r"(?P<strike>[\d,]*\.\d+)\s+(?P<cp>[CP])$")
_REPORT_TICKER_RE = re.compile(r"^[A-Z][A-Z0-9./]*$")
# An option's OCC symbol, as a Gain/Loss Report prints it after the
# underlying: "XMPL 260116C00050000" (expiry YYMMDD, call or put, strike
# in thousandths).
_REPORT_OCC_RE = re.compile(
    r"^(?P<under>[A-Z][A-Z0-9./]*) (?P<yy>\d{2})(?P<mm>\d{2})(?P<dd>\d{2})"
    r"(?P<cp>[CP])(?P<strike>\d{8})$")
_YES_HEADER_RE = re.compile(
    r"^(?P<term>Short|Long)-Term Realized Gain or \(Loss\)(?: \(continued\))?$")
_YES_TOTAL_RE = re.compile(r"^Total (?:Short|Long)-Term\b")
_YES_TAX_YEAR_RE = re.compile(r"^TAX YEAR (\d{4})\b", re.M)
_GLR_HEADER_RE = re.compile(r"^Realized Gain or \(Loss\)(?: \(continued\))?$")
_GLR_TERM_RE = re.compile(
    r"^(?P<term>Short|Long)-Term(?: \(continued\))? Quantity/Par$")
_GLR_METHOD_RE = re.compile(r"^(?P<cls>[A-Z][\w ]*?):\s*(?P<method>\S.*?)\s*$")
_GLR_TAX_YEAR_RE = re.compile(r"^(\d{4}) Year-End Schwab Gain/Loss Report\b", re.M)
_PAGE_RE = re.compile(r"^Page \d+ of \d+$")
_BOX_RE = re.compile(r"\bBox ([A-F])\b")


def _report_cells(text: str) -> tuple[list[float | None], list[str]] | None:
    """The amount cells of `text` in print order, plus the endnote letters
    among them; None when `text` holds anything else."""
    cells: list[float | None] = []
    marks: list[str] = []
    pos = 0
    for m in _REPORT_CELL_RE.finditer(text):
        if text[pos:m.start()].strip():
            return None
        pos = m.end()
        if m["mark"]:
            marks.append(m["mark"])
            continue
        if m["none"]:
            cells.append(None)
            continue
        cells.append(parse_amount(m["amt"].replace("$", "").replace(" ", "")))
        marks.extend(x for x in (m["pre"], m["post"]) if x)
    if not cells or text[pos:].strip():
        return None
    return cells, marks


def _report_iso(token: str) -> str | None:
    """A report's MM/DD/YY date as ISO; 'Various' as printed."""
    if token.lower() == "various":
        return "Various"
    try:
        return datetime.strptime(token, "%m/%d/%y").date().isoformat()
    except ValueError:
        return None


class _ReportLots:
    """Collects the lots of one realized-lot report as its lines go by.

    A lot line may arrive with its amounts cut short; the amount-only
    lines after it complete it. A lot still short when another kind of
    line arrives, or one with more amounts than a lot prints, is dropped
    and counted in `incomplete`."""

    def __init__(self, n_cells: tuple[int, ...]):
        self.n_cells = n_cells     # the cell counts a complete lot prints
        self.lots: list[dict] = []
        self.incomplete = 0
        self.desc: list[str] = []
        self.pending: dict | None = None

    def drop_pending(self) -> None:
        if self.pending is not None:
            self.incomplete += 1
            self.pending = None

    def lot_line(self, m: re.Match, cells: list, marks: list[str],
                 raw: str, **section) -> None:
        self.drop_pending()
        head = " ".join([*self.desc, m["head"]]).split()
        self.desc = []
        self.pending = {
            "head": head, "quantity": _to_float(m["qty"]),
            "acquired_date": _report_iso(m["acq"]),
            "disposed_date": _report_iso(m["sold"]),
            "cells": cells, "footnotes": (["S"] if m["short"] else []) + marks,
            "raw_lines": [raw], **section,
        }
        self._maybe_emit()

    def amount_line(self, cells: list, marks: list[str], raw: str) -> None:
        """Extend a pending lot with an amount-only line. Without one the
        line is a subtotal's amounts, and is ignored."""
        if self.pending is None:
            return
        self.pending["cells"] += cells
        self.pending["footnotes"] += marks
        self.pending["raw_lines"].append(raw)
        self._maybe_emit()

    def text_line(self, line: str) -> None:
        self.drop_pending()
        self.desc.append(line)

    def unreadable_lot_line(self) -> None:
        """A lot line whose amounts do not read as amount cells."""
        self.reset_description()
        self.incomplete += 1

    def reset_description(self) -> None:
        self.drop_pending()
        self.desc = []

    def _maybe_emit(self) -> None:
        n = len(self.pending["cells"])
        if n < min(self.n_cells):
            return
        if n not in self.n_cells:
            self.drop_pending()
            return
        self.lots.append(self.pending)
        self.pending = None


def _report_line(acc: _ReportLots, line: str, **section) -> None:
    """Feed one line of a realized section's body to `acc`: a lot line,
    an amount-only line, or description text."""
    m_lot = _REPORT_LOT_RE.match(line)
    if m_lot:
        parsed = _report_cells(m_lot["cells"])
        if parsed is None:
            acc.unreadable_lot_line()
        else:
            acc.lot_line(m_lot, *parsed, raw=line, **section)
    elif (parsed := _report_cells(line)) is not None:
        acc.amount_line(*parsed, raw=line)
    else:
        acc.text_line(line)


def _finish_report(acc: _ReportLots, lots: list[dict], year_re: re.Pattern,
                   text: str) -> dict:
    """Stamp every lot with the report's tax year: the one it prints, or
    else the year most of its lots were sold in."""
    m = year_re.search(text)
    tax_year = int(m[1]) if m else _modal_year([lot["disposed_date"] for lot in lots])
    for lot in lots:
        lot["footnotes"] = lot["footnotes"] or None
        lot["tax_year"] = tax_year
    return {"tax_year": tax_year, "lots": lots, "incomplete": acc.incomplete}


def _year_end_summary_lot(lot: dict) -> dict:
    """A Year-End Summary lot in the closed-lot shape."""
    cells = lot.pop("cells")
    head = lot.pop("head")
    cusip = None
    if head and _REPORT_CUSIP_RE.match(head[-1]):
        cusip = head.pop()
    elif (head and len(head[-1]) > 9 and head[-1][:-9].isalpha()
          and _REPORT_CUSIP_RE.match(head[-1][-9:])
          and sum(ch.isdigit() for ch in head[-1][-9:]) >= 3):
        # The description's last word glued to the CUSIP ("CLASS A" +
        # CUSIP prints as "A" + CUSIP).
        head[-1], cusip = head[-1][:-9], head[-1][-9:]
    name = " ".join(head) or None
    m = _REPORT_OPTION_RE.match(name or "")
    return {
        "security_name": name, "cusip": cusip,
        "instrument_key": (f"{m['under']} {m['exp']} {m['strike']} {m['cp']}"
                           if m else cusip),
        "proceeds": cells[0], "cost_basis": cells[1],
        "wash_sale_disallowed": cells[2],
        "accrued_market_discount": cells[3] if len(cells) == 5 else None,
        "realized_gain_loss": cells[-1], **lot,
    }


def _yes_subtitle(text: str) -> dict:
    """Covered flag and Form 8949 boxes from a realized section's
    subtitle. A section of lots not reported on the 1099-B says neither
    covered nor noncovered."""
    low = text.lower()
    covered = 0 if "noncovered" in low else 1 if "covered securities" in low else None
    boxes = dict.fromkeys(_BOX_RE.findall(text))
    return {"covered": covered, "form_8949_box": ",".join(boxes) or None}


def parse_year_end_summary_text(text: str) -> dict:
    """The realized lots of a Year-End Summary, standalone or inside a
    1099 Composite PDF.

    Returns {"tax_year", "lots", "incomplete"}: one dict per lot with
    security_name, cusip, instrument_key, quantity, acquired_date,
    disposed_date, proceeds, cost_basis, wash_sale_disallowed,
    accrued_market_discount, realized_gain_loss, term, covered,
    form_8949_box, footnotes, raw_lines and tax_year; and the count of
    lot lines whose amounts could not be read."""
    acc = _ReportLots(n_cells=(4, 5))
    mode = "out"   # out | subtitle | columns | data
    term = None
    subtitle: list[str] = []
    section: dict = {"covered": None, "form_8949_box": None}
    for raw in text.split("\n"):
        line = raw.strip()
        m_head = _YES_HEADER_RE.match(line)
        if m_head:
            acc.reset_description()
            term = m_head["term"].upper()
            mode, subtitle = "subtitle", []
            continue
        if mode == "out" or not line:
            continue
        if mode == "subtitle":
            if line.startswith("Description OR"):
                if subtitle:
                    section = _yes_subtitle(" ".join(subtitle))
                mode = "columns"
            else:
                subtitle.append(line)
            continue
        if mode == "columns":
            if line == "Gain or (Loss)":
                mode = "data"
            continue
        if line == "Adjusted" and not acc.desc and acc.pending is None:
            continue        # the column headings' last line, on some layouts
        if _YES_TOTAL_RE.match(line) or line.startswith("Please see the"):
            # A section's end, or the page's: furniture until the next
            # section header.
            acc.reset_description()
            mode = "out"
            continue
        if line.startswith("Security Subtotal"):
            acc.reset_description()
            continue
        _report_line(acc, line, term=term, **section)
    acc.drop_pending()
    return _finish_report(acc, [_year_end_summary_lot(lot) for lot in acc.lots],
                          _YES_TAX_YEAR_RE, text)


def _report_symbol_key(symbol: str) -> str | None:
    """The instrument key of a Gain/Loss Report symbol: the ticker, or an
    option's contract in the statements' form ("XMPL 01/16/2026 50.00 C")
    so it matches the holding's key."""
    if _REPORT_TICKER_RE.match(symbol):
        return symbol
    m = _REPORT_OCC_RE.match(symbol)
    if m:
        return (f"{m['under']} {m['mm']}/{m['dd']}/20{m['yy']} "
                f"{int(m['strike']) / 1000:.2f} {m['cp']}")
    return None


def _gain_loss_report_lot(lot: dict) -> dict:
    """A Gain/Loss Report lot in the closed-lot shape. Its description
    ends in ": TICKER"."""
    proceeds, cost, gain = lot.pop("cells")
    name, colon, symbol = " ".join(lot.pop("head")).rpartition(":")
    symbol = symbol.strip()
    if not colon:
        name, symbol = symbol, ""
    return {
        "security_name": name.strip() or None, "cusip": None,
        "instrument_key": _report_symbol_key(symbol),
        "proceeds": proceeds, "cost_basis": cost,
        "wash_sale_disallowed": None, "accrued_market_discount": None,
        "realized_gain_loss": gain, "covered": None, "form_8949_box": None,
        **lot,
    }


def parse_gain_loss_report_text(text: str) -> dict:
    """The realized lots and cost-basis methods of a Gain/Loss Report.

    Returns {"tax_year", "lots", "methods", "incomplete"}: lots in the
    shape parse_year_end_summary_text returns (a Gain/Loss Report prints
    no wash sale, market discount, covered flag or Form 8949 box), and
    the methods as printed, one {"asset_class", "method"} per line of
    the report's "Accounting Method" block."""
    acc = _ReportLots(n_cells=(3,))
    methods: dict[str, str] = {}
    mode = "out"   # out | methods | columns | data
    term = None
    for raw in text.split("\n"):
        line = raw.strip()
        m_term = _GLR_TERM_RE.match(line)
        if m_term:
            acc.reset_description()
            term = m_term["term"].upper()
            mode = "columns"
            continue
        if _GLR_HEADER_RE.match(line):
            acc.reset_description()
            mode = "out"
            continue
        if line == "Accounting Method":
            mode = "methods"
            continue
        if mode == "methods":
            if m := _GLR_METHOD_RE.match(line):
                methods.setdefault(m["cls"], m["method"])
            else:
                mode = "out"    # the block ends at its first other line
            continue
        if mode == "columns":
            if line == "Gain or (Loss)":
                mode = "data"
            continue
        if mode != "data" or not line:
            continue
        if _PAGE_RE.match(line) or line.startswith("Total "):
            acc.reset_description()
            mode = "out"
            continue
        if line.startswith("Security Subtotal"):
            acc.reset_description()
            continue
        _report_line(acc, line, term=term)
    acc.drop_pending()
    result = _finish_report(acc, [_gain_loss_report_lot(lot) for lot in acc.lots],
                            _GLR_TAX_YEAR_RE, text)
    result["methods"] = [{"asset_class": k, "method": v} for k, v in methods.items()]
    return result


def parse_realized_report_pdf(path, kind: str) -> dict:
    """Parse a realized-lot report PDF: `kind` is 'year_end_summary' (a
    Year-End Summary or 1099 Composite PDF) or 'gain_loss_report'."""
    text = extract_text_pdfium(path)
    if kind == "year_end_summary":
        return parse_year_end_summary_text(text)
    if kind == "gain_loss_report":
        return parse_gain_loss_report_text(text)
    raise ValueError(f"unsupported realized-lot report: {kind!r}")
