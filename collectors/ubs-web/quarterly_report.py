"""A Private Market Letter's quarterly report: the capital account and
the per-investment table."""
from __future__ import annotations

import json
import re

from collectorkit.words import Extracted

from pdf_common import (
    ISIN_SHAPE, LONG_DATE, document_text, long_date_to_unix, to_float,
)


# A quarterly report reaches the archive as a "Private Market Letter"
# whose cover page is titled "Quarterly Report". The fund
# administrator's report follows. Two parts of it are read.
#
# The capital account: the investor's position in the feeder fund, in
# two columns, the fund's and the investor's. The investor's figure is
# the last on each line (synthetic example):
#
#   Capital Account as at 31 March 2030
#   Net Commitment Net Commitment
#   USD 72,500,000 USD 175,000
#   ISIN - XX0000000011 Example Feeder Fund II Investor
#   Capital called 7,250,000 17,500
#   Cumulative expenses (290,000) (735)
#   Valuation of investment in Example Feeder Fund II 7,105,000 17,150
#   Shares in issue 7,250,000.000 17,500.000
#   Example Feeder Fund II - NAV (per shares in issue) 0.9800 0.9800
#
# The per-investment table: one line per investment of the fund behind
# the feeder, in the table's own unit (often millions). Its header names
# the columns. The reader handles three layouts:
#
#   with a sector:  name, region, a date, sector, then invested,
#                   realised, residual and total value, MOIC;
#   with a NAV:     name, geography, strategy, a date, then commitment,
#                   its share, invested, funded, distributed, DPI,
#                   current NAV, total value, MOIC, IRR;
#   with a stage:   name, [currency, vintage,] geography, stage, then
#                   committed, invested, funded, realised and total
#                   value, MOIC.
#
# The residual value is the table's own column, the current NAV, or the
# total value less the realised value. A table runs across pages and
# closes on a "Total" line. The lines count only as a whole: their
# residual values must sum to that total's, within the table's rounding,
# or the report says its lines are not reconciled. Pie charts are never
# read. One family prints them as a share of NAV, another as a share of
# invested capital, and their labels scramble in the text layer.

_REPORT_TITLE_RE = re.compile(r"^Quarterly Report\s*$", re.M)
_REPORT_ASOF_RE = re.compile(rf"^Capital Account as at\s+{LONG_DATE}\s*$", re.M)
_REPORT_ISIN_RE = re.compile(
    rf"^ISIN\s*[-–]\s*(?P<isin>{ISIN_SHAPE})\s+(?P<name>.+?)\s+Investor\s*$",
    re.M)
_REPORT_NUM = r"\(?-?\d[\d,]*(?:\.\d+)?\)?|-"
_REPORT_COMMITMENT_RE = re.compile(
    rf"^(?P<ccy>[A-Z]{{3}})\s*[\d,]+\s+(?P=ccy)\s*(?P<v>{_REPORT_NUM})\s*$")
_REPORT_FIGURES = {
    "capital_called": re.compile(
        rf"^Capital called\s+(?:{_REPORT_NUM})\s+(?P<v>{_REPORT_NUM})\s*$", re.M),
    "cumulative_expenses": re.compile(
        rf"^Cumulative expenses\s+(?:{_REPORT_NUM})\s+(?P<v>{_REPORT_NUM})\s*$",
        re.M),
    "valuation": re.compile(
        rf"^Valuation of investment in .+?\s+(?:{_REPORT_NUM})\s+"
        rf"(?P<v>{_REPORT_NUM})\s*$", re.M),
    "shares_in_issue": re.compile(
        rf"^Shares in issue\s+(?:{_REPORT_NUM})\s+(?P<v>{_REPORT_NUM})\s*$", re.M),
    "nav_per_share": re.compile(
        rf"- NAV \(per shares in issue\)\s+(?:{_REPORT_NUM})\s+"
        rf"(?P<v>{_REPORT_NUM})\s*$", re.M),
}
# The table's own date, which can lag the capital account's ("last
# reported"). A footnote mark may cling to the year.
_REPORT_LINES_ASOF_RE = re.compile(
    r"(?:Portfolio Overview|Portfolio level performance)\b.*?\bas of\s+"
    r"(?P<day>\d{1,2})\s+(?P<month>[A-Z][a-z]+)\s*(?P<year>\d{4})")
# A table header starts with the name column. Its labels may stack over
# several lines, so the lines below it are read with it.
_REPORT_HEADER_RE = re.compile(r"^(?:Company|Investment|Fund)\b")
_REPORT_HEADER_DEPTH = 4

# The geographies the tables print, as printed. The last one on a line
# is the column, since a fund's own name may carry a region word too.
_REPORT_GEOGRAPHIES = (
    "North America", "N. America", "Latin America", "Western Europe",
    "Europe", "Asia-Pacific", "Asia Pacific", "APAC", "Asia", "Australia",
    "Middle East", "Africa", "Rest of World", "ROW", "Global",
    "United States", "USA", "Canada", "Brazil", "United Kingdom", "UK",
    "Germany", "France", "Nordics", "Israel", "India", "China", "Japan",
    "Korea",
)
_REPORT_GEO_RE = re.compile(
    r"(?<![\w.])(?:" + "|".join(re.escape(g) for g in _REPORT_GEOGRAPHIES)
    + r")(?![\w-])")
# An investment date, "Aug-23" or "04-Nov-22".
_REPORT_DATE_RE = re.compile(r"(?<!\S)(?:\d{2}-)?[A-Z][a-z]{2}-\d{2}(?!\S)")
# A currency and a vintage year between a name and its geography.
_REPORT_VINTAGE_RE = re.compile(r"\s*(?:\b[A-Z]{3}\s+)?(?:19|20)\d{2}$")
_REPORT_CELL_RE = re.compile(
    r"^(?:\(?-?\d[\d,]*(?:\.\d+)?%?\)?x?|-{1,2}|n/a|N/A|NA|NM|nm)$")

# Per layout: the fewest cells a line prints, the cell that is its cost,
# and how its residual value reads from the cells.
_REPORT_LAYOUTS = {
    "sector": (4, 0, lambda c: c[2]),
    "nav": (8, 2, lambda c: c[6]),
    "stage": (5, 1, lambda c: None if c[4] is None or c[3] is None
              else c[4] - c[3]),
}


def _report_layout(header: str) -> str | None:
    """The layout a table header names, or None for one this reader does
    not know: a header names a region or a geography, and the column
    that says which layout it is."""
    if not re.search(r"\b(?:Region|Geography)\b", header):
        return None
    for layout, word in (("sector", "Sector"), ("nav", "Strategy"),
                         ("stage", "Stage")):
        if re.search(rf"\b{word}\b", header):
            return layout
    return None


def _report_closes(text: str, cells: list[str], layout: str) -> bool:
    """Whether a table line is its closing total: a "Total" line, or,
    where a table prints no label on it, the line at 100% of the
    commitments."""
    if text:
        return text.startswith("Total")
    return layout == "nav" and cells[1] in ("100%", "100.0%")


def _report_amount(s: str) -> float | None:
    """A capital-account or table figure: a dash is zero, parentheses
    are negative, and n/a and NM state nothing."""
    s = s.rstrip("x%")
    if s in ("n/a", "N/A", "NA", "NM", "nm"):
        return None
    if set(s) <= {"-"}:
        return 0.0
    v = to_float(s.strip("()"))
    return -v if v is not None and s.startswith("(") else v


def _report_split(line: str) -> tuple[str, list[str]]:
    """A table line as its text and its trailing cells."""
    tokens = line.split()
    k = len(tokens)
    while k > 0 and _REPORT_CELL_RE.match(tokens[k - 1]):
        k -= 1
    return " ".join(tokens[:k]), tokens[k:]


def _report_line(text: str, layout: str, wrapped: str = "") -> dict | None:
    """The name, geography, and sector or strategy of a table line's
    text, or None when it names no geography or no name. `wrapped` is the
    name for a line that prints none of its own."""
    geos = list(_REPORT_GEO_RE.finditer(text))
    if not geos:
        return None
    geo = geos[-1]
    dates = list(_REPORT_DATE_RE.finditer(text))
    name = _REPORT_VINTAGE_RE.sub(
        "", text[:min([geo.start()] + [d.start() for d in dates])].rstrip())
    name = name or wrapped
    if not name:
        return None
    if layout == "sector":
        # The sector follows both the region and the date.
        rest = text[max([geo.end()] + [d.end() for d in dates]):]
    else:
        # The strategy follows the geography, around the date.
        rest = _REPORT_DATE_RE.sub(" ", text[geo.end():])
    rest = " ".join(rest.split()) or None
    return {
        "name": name, "region": geo.group(0),
        "sector": rest if layout == "sector" else None,
        "strategy": None if layout == "sector" else rest,
    }


def _report_lines(lines: list[str]) -> tuple[list[dict], float | None]:
    """The per-investment lines of a report's tables, and the residual
    value of the last closing total. A line whose name wraps around the
    rest of it is read as one. Either its figures stand alone between the
    two halves of its text, or its columns do, between the two halves of
    its name."""
    out: list[dict] = []
    total = None
    layout = None
    k = 0
    while k < len(lines):
        ln = lines[k]
        k += 1
        text, cells = _report_split(ln)
        if _REPORT_HEADER_RE.match(ln) and not cells:
            header = " ".join(lines[k - 1:k - 1 + _REPORT_HEADER_DEPTH])
            layout = _report_layout(header) or layout
            continue
        if layout is None:
            continue
        least, cost_at, residual_of = _REPORT_LAYOUTS[layout]
        if len(cells) < least:
            continue
        if _report_closes(text, cells, layout):
            total = residual_of([_report_amount(c) for c in cells])
            continue
        printed = ln
        if not text and 2 <= k < len(lines):
            above, below = lines[k - 2], lines[k]
            if (not _report_split(above)[1] and not _report_split(below)[1]
                    and _REPORT_GEO_RE.search(below)):
                text = f"{above} {below}"
                printed = f"{above}\n{ln}\n{below}"
                k += 1
        values = [_report_amount(c) for c in cells]
        row = _report_line(text, layout) if text else None
        if row is None and text and 2 <= k < len(lines):
            above, below = lines[k - 2], lines[k]
            if (not _report_split(above)[1] and not _report_split(below)[1]
                    and not _REPORT_GEO_RE.search(below)):
                row = _report_line(text, layout, f"{above} {below}")
                if row is not None:
                    printed = f"{above}\n{ln}\n{below}"
                    k += 1
        if row is None:
            continue
        row.update(cost=values[cost_at], residual_value=residual_of(values),
                   payload=json.dumps({"line": printed}, ensure_ascii=False))
        out.append(row)
    return out, total


def parse(extracted: Extracted, doc_token: str
          ) -> tuple[list[dict], list[dict]]:
    """Read a quarterly report from a Private Market Letter. Returns
    ([], []) for a letter that is not one, which is told from the cover
    page alone."""
    pages = extracted.pages
    if not pages or not _REPORT_TITLE_RE.search(pages[0].text):
        return [], []
    return parse_quarterly_report_text(document_text(extracted), doc_token)


def parse_quarterly_report_text(text: str, doc_token: str
                                ) -> tuple[list[dict], list[dict]]:
    """Pure-text variant of `parse`: the report row and
    its investment lines. Returns ([], []) unless the document is a
    quarterly report whose capital account states the fund's ISIN and its
    date, the two facts that place the report."""
    if not _REPORT_TITLE_RE.search(text):
        return [], []
    for dash in "‐–":
        text = text.replace(dash, "-")
    isin_m = _REPORT_ISIN_RE.search(text)
    asof_m = _REPORT_ASOF_RE.search(text)
    as_of = long_date_to_unix(asof_m) if asof_m else None
    if isin_m is None or as_of is None:
        return [], []
    lines = [ln.strip() for ln in text.splitlines()]

    printed = {"as_of": asof_m.group(0).strip(), "isin": isin_m.group(0).strip()}
    row = {
        "source_doc_token": doc_token, "instrument_isin": isin_m["isin"],
        "fund_name": isin_m["name"], "as_of": as_of, "lines_as_of": None,
        "currency_iso": None, "commitment": None,
        **dict.fromkeys(_REPORT_FIGURES),
        "line_count": 0, "lines_total": None, "lines_reconciled": 0,
    }
    for k, ln in enumerate(lines[:-1]):
        m = ln.startswith("Net Commitment") and _REPORT_COMMITMENT_RE.match(
            lines[k + 1])
        if m:
            row["currency_iso"] = m["ccy"]
            row["commitment"] = _report_amount(m["v"])
            printed["commitment"] = lines[k + 1]
            break
    for col, rx in _REPORT_FIGURES.items():
        m = rx.search(text)
        if m:
            row[col] = _report_amount(m["v"])
            printed[col] = m.group(0).strip()
    m = _REPORT_LINES_ASOF_RE.search(text)
    if m:
        row["lines_as_of"] = long_date_to_unix(m)

    found, total = _report_lines(lines)
    residuals = [r["residual_value"] for r in found]
    row.update(line_count=len(found), lines_total=total)
    if found and total is not None and None not in residuals:
        # Each figure is rounded to the table's one decimal, so the sum
        # may drift from the total by up to half of it per line.
        slack = 0.05 * len(found) + 0.1
        row["lines_reconciled"] = int(abs(sum(residuals) - total) <= slack)
    row["payload"] = json.dumps(printed, ensure_ascii=False)
    return [row], [{"source_doc_token": doc_token, "line_no": n, **r}
                   for n, r in enumerate(found, start=1)]
