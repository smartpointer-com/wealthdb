"""
A small XLSX reader: a worksheet of a workbook, the first or one named,
as rows of cell text. An issuer's holdings file is a plain grid, so
shared strings, inline strings and numbers are all it needs; formulas,
styles and dates stay as the file stores them (a number is its text, a
date its serial).
"""

from __future__ import annotations

import io
import xml.etree.ElementTree as ET
import zipfile

NS = "{http://schemas.openxmlformats.org/spreadsheetml/2006/main}"
REL_NS = "{http://schemas.openxmlformats.org/officeDocument/2006/relationships}"


class XlsxError(ValueError):
    """The bytes are not a workbook this reader can read."""


def _col(ref: str) -> int:
    """The zero-based column of a cell reference such as `C12`."""
    n = 0
    for ch in ref:
        if not ch.isalpha():
            break
        n = n * 26 + (ord(ch.upper()) - 64)
    return n - 1


def _text(el) -> str:
    return "".join(t.text or "" for t in el.iter(f"{NS}t"))


def rows(data: bytes, sheet: str | None = None) -> list[list[str]]:
    """The rows of the first worksheet, or of the one named `sheet` (any
    case), each a list of cell texts with empty strings where a row skips
    a column. A workbook without that sheet is an XlsxError."""
    try:
        z = zipfile.ZipFile(io.BytesIO(data))
        names = set(z.namelist())
        shared = []
        if "xl/sharedStrings.xml" in names:
            shared = [_text(si) for si in ET.fromstring(z.read("xl/sharedStrings.xml")).iter(f"{NS}si")]
        root = ET.fromstring(z.read(_sheet_path(z, names, sheet)))
    except (zipfile.BadZipFile, KeyError, ET.ParseError) as e:
        raise XlsxError(f"not a readable workbook: {e}") from e
    out = []
    for row in root.iter(f"{NS}row"):
        cells: dict[int, str] = {}
        for i, c in enumerate(row.iter(f"{NS}c")):
            col = _col(c.get("r") or "") if c.get("r") else i
            kind = c.get("t")
            v = c.find(f"{NS}v")
            if kind == "s" and v is not None:
                cells[col] = shared[int(v.text)]
            elif kind == "inlineStr":
                cells[col] = _text(c)
            else:
                cells[col] = v.text if v is not None and v.text is not None else ""
        width = max(cells) + 1 if cells else 0
        out.append([cells.get(i, "") for i in range(width)])
    return out


def _name(s) -> str:
    return " ".join(str(s or "").lower().split())


def _sheet_path(z: zipfile.ZipFile, names: set, sheet: str | None) -> str:
    """The path of the workbook's first sheet, or of the one named
    `sheet`."""
    wb = ET.fromstring(z.read("xl/workbook.xml"))
    found = next((s for s in wb.iter(f"{NS}sheet") if sheet is None or _name(s.get("name")) == _name(sheet)), None)
    if found is None and sheet is not None:
        raise XlsxError(f"the workbook has no sheet {sheet!r}")
    if found is not None and "xl/_rels/workbook.xml.rels" in names:
        rid = found.get(f"{REL_NS}id")
        rels = ET.fromstring(z.read("xl/_rels/workbook.xml.rels"))
        for rel in rels:
            if rel.get("Id") == rid:
                target = rel.get("Target", "").lstrip("/")
                return target if target.startswith("xl/") else f"xl/{target}"
    if sheet is not None:
        raise XlsxError(f"the workbook does not say where its sheet {sheet!r} lies")
    return "xl/worksheets/sheet1.xml"
