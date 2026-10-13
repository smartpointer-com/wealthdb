"""
A small XLSX reader: the first worksheet of a workbook as rows of cell
text. An issuer's holdings file is a plain grid, so shared strings,
inline strings and numbers are all it needs; formulas, styles and dates
stay as the file stores them (a number is its text, a date its serial).
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


def rows(data: bytes) -> list[list[str]]:
    """The first worksheet's rows, each a list of cell texts with empty
    strings where a row skips a column."""
    try:
        z = zipfile.ZipFile(io.BytesIO(data))
        names = set(z.namelist())
        shared = []
        if "xl/sharedStrings.xml" in names:
            shared = [_text(si) for si in ET.fromstring(z.read("xl/sharedStrings.xml")).iter(f"{NS}si")]
        sheet = _first_sheet(z, names)
        root = ET.fromstring(z.read(sheet))
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


def _first_sheet(z: zipfile.ZipFile, names: set) -> str:
    """The path of the workbook's first sheet."""
    wb = ET.fromstring(z.read("xl/workbook.xml"))
    first = next(wb.iter(f"{NS}sheet"), None)
    if first is not None and "xl/_rels/workbook.xml.rels" in names:
        rid = first.get(f"{REL_NS}id")
        rels = ET.fromstring(z.read("xl/_rels/workbook.xml.rels"))
        for rel in rels:
            if rel.get("Id") == rid:
                target = rel.get("Target", "").lstrip("/")
                return target if target.startswith("xl/") else f"xl/{target}"
    return "xl/worksheets/sheet1.xml"
