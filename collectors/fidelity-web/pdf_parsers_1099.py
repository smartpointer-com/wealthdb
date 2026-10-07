"""
Parser for Fidelity's Consolidated Form 1099 PDF: the Form 1099-B lots.

A Consolidated 1099 is one account's tax reporting statement for one
tax year. Its 1099-B pages list every lot the account sold, grouped
by the Form 8949 box the lot is reported under, and per security:

    FORM 1099-B* ...
    Short-term transactions for which basis is reported to the IRS
        --report on Form 8949 with Box A checked ...
    Action  Quantity 1b Date 1c Date Sold 1d Proceeds 1e Cost or 1f Accrued
                     Acquired or Disposed  Other Basis (b) Market ...
    EXAMPLE CORP COM,TICK1,000000AA0
    Sale    10.000 01/02/24 03/04/25  1,100.00  1,000.00   100.00
    Subtotals                         1,100.00  1,000.00

The section line says the term (short or long) and whether the basis
is reported to the IRS (a covered lot) or not. A security line ends
in its symbol and CUSIP, the symbol absent for a security that has
none. Each row is one lot: the action, the quantity, the acquired
date (or ``VARIOUS``) and the sale date, then the money columns.

The money columns are the reason this parser reads WORDS rather than
text. The form leaves an empty cell blank instead of printing a zero,
so a row with an accrued market discount and a row with a wash sale
both print four numbers, and only the position on the page tells the
1f column from the 1g one. Each number is assigned to the column whose
box label (``1d``, ``1e``, ``1f``, ``1g``, ``Gain/Loss``, ``4``) it
sits under, by its right edge: the figures are right-aligned and each
column starts at its label.

The document also says who and when: the year (``2025 TAX REPORTING
STATEMENT``), the account (``Account No. NNN-NNNNNN``), the date it
was prepared (the page footer ``MM/DD/YYYY <id> Pages 1 of N``) and,
on a corrected form, ``CORRECTED MM/DD/YYYY``. Fidelity serves the
same form again on every download, with new bytes, so the loader keys
a form on its account and year and keeps the latest prepared one.

Architecture mirrors the statement parsers: ``parse_1099b_lines`` is a
pure function over lines of placed words, so tests build them by hand;
``parse_consolidated_1099_pdf(path)`` opens the PDF with pdfplumber
and delegates.
"""

from __future__ import annotations

import re
from dataclasses import asdict, dataclass, field
from datetime import date as date_cls

# A coarse, human-readable epoch for load.py's parse-cache namespace.
# The source fingerprint (collectorkit.srcfp) re-keys the cache on any
# edit to this module; bumping this forces a re-parse without one.
PARSER_VERSION = "1"


@dataclass(frozen=True)
class Word:
    """One word on a page, with its horizontal extent in PDF points."""
    text: str
    x0: float
    x1: float


def _line_text(line):
    return " ".join(w.text for w in line)


# ============================================================
# Document identity
# ============================================================

_YEAR_RE = re.compile(r"^(?P<year>\d{4}) TAX REPORTING STATEMENT\b")
_ACCOUNT_RE = re.compile(r"\bAccount No\.\s*(?P<acct>\d{3}-\d{6})\b")
_FOOTER_RE = re.compile(
    r"^(?P<mm>\d{2})/(?P<dd>\d{2})/(?P<yyyy>\d{4}) \d+ Pages \d+ of \d+$")
_CORRECTED_RE = re.compile(r"^CORRECTED (?P<mm>\d{2})/(?P<dd>\d{2})/(?P<yyyy>\d{4})$")


def _ymd(m):
    return date_cls(int(m["yyyy"]), int(m["mm"]), int(m["dd"]))


def parse_identity(lines):
    """Return ``(tax_year, account_external_id, prepared, corrected)``
    from the document's lines; each is None when the document does not
    state it. ``prepared`` is the first page-footer date and
    ``corrected`` the date of the ``CORRECTED`` stamp, both
    ``datetime.date``."""
    year = acct = prepared = corrected = None
    for line in lines:
        text = _line_text(line)
        if year is None and (m := _YEAR_RE.match(text)):
            year = int(m["year"])
        if acct is None and (m := _ACCOUNT_RE.search(text)):
            acct = m["acct"].replace("-", "")
        if prepared is None and (m := _FOOTER_RE.match(text)):
            prepared = _ymd(m)
        if corrected is None and (m := _CORRECTED_RE.match(text)):
            corrected = _ymd(m)
    return year, acct, prepared, corrected


# ============================================================
# 1099-B lots
# ============================================================

_FORM_1099B_RE = re.compile(r"^FORM 1099-B\b")
_FORM_ANY_RE = re.compile(r"^F ?orm [0-9A-Z-]+\b", re.IGNORECASE)

# `Short-term transactions for which basis is reported to the IRS
# --report on Form 8949 with Box A checked ...`. A section whose term is
# unknown names no single box (`Box B or E`). Text extraction can drop
# the space between two words here (`basisis`), so the gaps are optional.
_SECTION_RE = re.compile(
    r"^(?:(?P<term>Short|Long)-term\s*transactions|Transactions)\s*for\s*which\s*"
    r"basis\s*is\s*(?P<not>not\s*)?reported\s*to\s*the\s*IRS")
_BOX_RE = re.compile(r"Form 8949 with Box (?P<box>[A-F]) checked")

# The money columns, keyed by the box label that heads each one on the
# first header line, in the order they print.
_COLUMN_LABELS = (
    ("1d", "proceeds"),
    ("1e", "cost_basis"),
    ("1f", "accrued_market_discount"),
    ("1g", "wash_sale_disallowed"),
    ("Gain/Loss", "gain_loss"),
    ("4", "federal_tax_withheld"),
    ("14", "state"),
)

# `DESCRIPTION,SYMBOL,CUSIP` or `DESCRIPTION,CUSIP`.
_SECURITY_RE = re.compile(
    r"^(?P<desc>\S.*?),(?:(?P<symbol>[^,\s]+),)?(?P<cusip>[0-9A-Z]{9})$")

# Everything left of the money columns: `[!C]<action> <quantity>
# <acquired> <sold>`. `!C` marks a lot the correction changed, and can
# print glued to the action.
_ROW_LEFT_RE = re.compile(
    r"^(?P<corrected>!C)?\s*(?P<action>[A-Za-z][A-Za-z ]*?)\s+"
    r"(?P<qty>-?[\d,]*\.?\d+)\s+"
    r"(?P<acquired>\d{2}/\d{2}/\d{2}|[A-Za-z]+)\s+"
    r"(?P<sold>\d{2}/\d{2}/\d{2})$")

# A money cell: an amount, optionally followed by footnote letters in
# parentheses, e.g. `1,234.56(f)`.
_MONEY_RE = re.compile(r"^(?P<num>-?[\d,]+\.\d{2})(?P<notes>(?:\([a-z]\))*)$")


@dataclass
class LotRow:
    """One Form 1099-B lot, as printed.

    ``acquired_date`` is ISO or the form's own word (``Various``,
    ``Unknown``); ``date_sold`` is ISO. Money is USD; a blank cell is None.
    ``footnotes`` maps a column to the footnote letters printed beside
    its figure.
    """
    form_8949_box: str | None
    term: str | None                  # 'short' | 'long' | None (unknown)
    covered: bool
    description: str
    symbol: str | None
    cusip: str | None
    action: str
    corrected: bool
    quantity: float | None
    acquired_date: str | None
    date_sold: str | None
    proceeds: float | None = None
    cost_basis: float | None = None
    accrued_market_discount: float | None = None
    wash_sale_disallowed: float | None = None
    gain_loss: float | None = None
    federal_tax_withheld: float | None = None
    footnotes: dict = field(default_factory=dict)


def _column_starts(line):
    """The x0 of each money column's box label on a header line, or
    None when the line is not the column header."""
    texts = [w.text for w in line]
    if "Action" not in texts or "Quantity" not in texts:
        return None
    starts = []
    for label, column in _COLUMN_LABELS:
        w = next((w for w in line if w.text == label), None)
        if w is None:
            return None
        starts.append((w.x0, column))
    return sorted(starts)


def _column_for(x1, starts):
    """The money column a right-aligned figure ending at ``x1`` sits in."""
    column = None
    for x0, name in starts:
        if x1 >= x0:
            column = name
    return column


def _iso_from_mdyy(token, tax_year):
    """``MM/DD/YY`` → ISO. A lot is never acquired after the year it is
    sold in, so a two-digit year above the tax year's is last century."""
    mm, dd, yy = (int(p) for p in token.split("/"))
    century = 2000 if tax_year is None or yy <= tax_year % 100 else 1900
    return date_cls(century + yy, mm, dd).isoformat()


def _number(token):
    return float(token.replace(",", ""))


def parse_1099b_lines(lines, *, tax_year=None):
    """Extract every Form 1099-B lot from a document's lines, each line
    a list of ``Word`` in reading order. Returns [] when the document
    has no 1099-B pages.

    The page is read top to bottom carrying three pieces of state: the
    section (box, term, covered), the column layout from the most
    recent header, and the current security. A row is read only once
    all three are known; anything else on a 1099-B page (the masthead,
    the legend, subtotals, the dashed rule) matches none of the row
    shapes and is passed over. Another form's title ends the 1099-B
    pages.
    """
    lots = []
    in_1099b = False
    section = None
    starts = None
    security = None
    for line in lines:
        if not line:
            continue
        text = _line_text(line)
        if _FORM_1099B_RE.match(text):
            in_1099b = True
            continue
        if _FORM_ANY_RE.match(text):
            in_1099b = False
            section = starts = security = None
            continue
        if not in_1099b:
            continue
        if m := _SECTION_RE.match(text):
            box = _BOX_RE.search(text)
            new = (
                box["box"] if box else None,
                m["term"].lower() if m["term"] else None,
                not m["not"],
            )
            # Every page reprints its section line; only a new section
            # ends the security, which may run on across the page break.
            if new != section:
                section, security = new, None
            continue
        if (cols := _column_starts(line)) is not None:
            starts = cols
            continue
        if section is None or starts is None:
            continue
        if text.upper().startswith(("SUBTOTALS", "TOTALS", "TOTAL ")):
            continue
        left_edge = starts[0][0]
        left = [w for w in line if w.x1 < left_edge]
        money = [w for w in line if w.x1 >= left_edge]
        row = _ROW_LEFT_RE.match(_line_text(left)) if left else None
        if row is None:
            if sec := _SECURITY_RE.match(text):
                security = (" ".join(sec["desc"].split()),
                            sec["symbol"], sec["cusip"])
            continue
        if security is None:
            continue
        lot = LotRow(
            form_8949_box=section[0], term=section[1], covered=section[2],
            description=security[0], symbol=security[1], cusip=security[2],
            action=" ".join(row["action"].split()),
            corrected=bool(row["corrected"]),
            quantity=_number(row["qty"]),
            acquired_date=(
                _iso_from_mdyy(row["acquired"], tax_year)
                if "/" in row["acquired"] else row["acquired"].capitalize()),
            date_sold=_iso_from_mdyy(row["sold"], tax_year),
        )
        for w in money:
            cell = _MONEY_RE.match(w.text)
            column = _column_for(w.x1, starts)
            if cell is None or column is None or column == "state":
                continue
            setattr(lot, column, _number(cell["num"]))
            if cell["notes"]:
                lot.footnotes[column] = re.findall(r"[a-z]", cell["notes"])
        lots.append(lot)
    return lots


# ============================================================
# PDF orchestration
# ============================================================

# Words closer than this many points vertically share a line.
_LINE_TOLERANCE = 2.0


def _page_lines(page):
    """Group a pdfplumber page's words into lines, each sorted left to
    right, the lines top to bottom."""
    lines = []
    for w in sorted(page.extract_words(), key=lambda w: (w["top"], w["x0"])):
        if lines and abs(lines[-1][0] - w["top"]) <= _LINE_TOLERANCE:
            lines[-1][1].append(w)
        else:
            lines.append((w["top"], [w]))
    return [
        [Word(w["text"], w["x0"], w["x1"])
         for w in sorted(ws, key=lambda w: w["x0"])]
        for _, ws in lines
    ]


# The identity sits on the first page of the statement proper; a
# corrected form opens with a two-page cover. Reading stops here at
# the latest, so a document that states no identity costs little.
_IDENTITY_MAX_PAGES = 6


def _identity_dict(path, year, acct, prepared, corrected):
    return {
        "path": str(path),
        "tax_year": year,
        "account_external_id": acct,
        "prepared": prepared.isoformat() if prepared else None,
        "corrected": corrected.isoformat() if corrected else None,
    }


def read_consolidated_1099_identity(path):
    """Read only which form a Consolidated 1099 PDF is: the
    ``parse_consolidated_1099_pdf`` dict without its ``lots``.

    Fidelity serves the same form again on every download, so the
    loader reads this first and parses the lots of only the copy it
    keeps. The pages are read until the year, the account and the
    prepared date are all known."""
    import pdfplumber

    lines = []
    year = acct = prepared = corrected = None
    with pdfplumber.open(str(path)) as pdf:
        for page in pdf.pages[:_IDENTITY_MAX_PAGES]:
            lines.extend(_page_lines(page))
            year, acct, prepared, corrected = parse_identity(lines)
            if year and acct and prepared:
                break
    return _identity_dict(path, year, acct, prepared, corrected)


def parse_consolidated_1099_pdf(path):
    """Open a Consolidated Form 1099 PDF and return:

        {
            "path": "<path>",
            "tax_year": 2025 | None,
            "account_external_id": "NNNNNNNNN" | None,
            "prepared": "YYYY-MM-DD" | None,
            "corrected": "YYYY-MM-DD" | None,
            "lots": [{...}, ...],
        }
    """
    import pdfplumber

    lines = []
    with pdfplumber.open(str(path)) as pdf:
        for page in pdf.pages:
            lines.extend(_page_lines(page))
    year, acct, prepared, corrected = parse_identity(lines)
    lots = parse_1099b_lines(lines, tax_year=year)
    return {
        **_identity_dict(path, year, acct, prepared, corrected),
        "lots": [asdict(lot) for lot in lots],
    }


if __name__ == "__main__":
    import sys

    from collectorkit import parser_cli
    raise SystemExit(parser_cli.dump_json(
        sys.argv[1:], parse_consolidated_1099_pdf,
        description="Extract the Form 1099-B lots from one or more "
                    "Consolidated Form 1099 PDFs and emit JSON."))
