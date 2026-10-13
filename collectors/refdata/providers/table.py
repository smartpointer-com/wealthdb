"""
A holdings table, as an issuer's spreadsheet or CSV lays it out: a few
preamble lines (the fund, the day), a header row, one row per holding,
then footnotes. The header row is found by its words, not its position,
and each column is read by what its header says, so one parser serves
every issuer layout whose headers it knows.

A SEDOL column is never read: SEDOL codes are licensed.
"""

from __future__ import annotations

import re
from datetime import date, datetime

from .base import blank_to_none, dec

# Header words per line field, lower case. The first header that names a
# field wins.
COLUMNS = {
    "isin": ("isin",),
    "cusip": ("cusip",),
    "identifier": ("identifier", "security identifier"),
    "ticker": ("ticker", "issuer ticker", "local ticker", "ticker symbol"),
    "name": ("security name", "name", "holding", "holding name", "description", "issuer name"),
    "weight": ("percent of fund", "weight", "weight (%)", "% of net assets", "weighting", "% weight",
               "percentage of fund", "% of fund"),
    "quantity": ("number of shares", "shares held", "shares", "quantity", "quantity/shares"),
    "market_value": ("base market value", "market value", "market value (base)", "market_value"),
    "currency": ("currency", "local currency", "market currency", "trading currency"),
    "country": ("trade country name", "location", "country", "country of risk"),
    "sector": ("sector classification", "sector", "industry sector"),
    "industry": ("industry classification", "industry"),
    "asset_type": ("asset class", "type of security", "security type", "asset type", "asset_type"),
    "exchange": ("exchange",),
}
NEVER = ("sedol",)
# What an issuer writes in a cell that has no value.
PLACEHOLDERS = {"-", "--", "—", "n/a", "na", "none"}
ISIN_RE = re.compile(r"^[A-Z]{2}[A-Z0-9]{9}[0-9]$")
CUSIP_RE = re.compile(r"^[0-9A-Z]{9}$")


def _norm(s) -> str:
    return " ".join(str(s or "").strip().lower().split())


def header_index(rows) -> int | None:
    """The index of the header row: the first row that names a holding and
    its weight."""
    names = set(COLUMNS["name"])
    weights = set(COLUMNS["weight"])
    for i, row in enumerate(rows):
        cells = {_norm(c) for c in row}
        if cells & names and cells & weights:
            return i
    return None


def preamble(rows, end: int) -> dict:
    """The `Label: value` pairs above the header row, labels lower case
    without the colon. A pair may sit in two cells or in one."""
    out = {}
    for row in rows[:end]:
        cells = [str(c).strip() for c in row if str(c).strip()]
        if not cells:
            continue
        if len(cells) >= 2 and cells[0].endswith(":"):
            out[_norm(cells[0][:-1])] = cells[1]
        elif ":" in cells[0]:
            k, _, v = cells[0].partition(":")
            out[_norm(k)] = v.strip()
        else:
            out.setdefault("_lines", []).append(" ".join(cells))
    return out


def parse_day(s) -> date | None:
    """A day as issuers write it: 12-Oct-2026, Oct 12 2026, 10/12/2026,
    2026-10-12, with or without a leading "As of"."""
    s = re.sub(r"(?i)^\s*as of\s*", "", str(s or "")).strip().rstrip(".")
    for fmt in ("%d-%b-%Y", "%b %d %Y", "%b %d, %Y", "%d %b %Y", "%m/%d/%Y", "%Y-%m-%d", "%d/%b/%Y",
                "%d.%m.%Y"):
        try:
            return datetime.strptime(s, fmt).date()
        except ValueError:
            continue
    return None


def _ident(value):
    v = (blank_to_none(value) or "").upper()
    if ISIN_RE.match(v):
        return "isin", v
    if CUSIP_RE.match(v) and any(ch.isdigit() for ch in v):
        return "cusip", v
    return None, None


def lines(rows, start: int, *, ref: str, as_of: date, basis: str = "holdings") -> list:
    """Silver line rows below the header row at `start`, up to the first
    row with fewer than two filled cells: a blank row, or a footnote in
    one cell. A row with neither a name nor a weight is skipped. A column
    no header word names lands in the payload."""
    header = [_norm(c) for c in rows[start]]
    col = {}
    for field, words in COLUMNS.items():
        for w in words:
            if w in header and field not in col:
                col[field] = header.index(w)
                break
    known = set(col.values()) | {i for i, h in enumerate(header) if h in NEVER}
    out = []
    for row in rows[start + 1:]:
        if sum(1 for c in row if str(c).strip()) < 2:
            break

        def cell(field, row=row):
            i = col.get(field)
            v = blank_to_none(row[i]) if i is not None and i < len(row) else None
            return None if v is not None and v.lower() in PLACEHOLDERS else v

        name = cell("name")
        if name is None and dec(cell("weight")) is None:
            continue
        isin = cell("isin")
        isin = isin.upper() if isin and ISIN_RE.match(isin.upper()) else None
        cusip = cell("cusip")
        cusip = cusip.upper() if cusip and CUSIP_RE.match(cusip.upper()) else None
        kind, ident = _ident(cell("identifier"))
        if kind == "isin" and isin is None:
            isin = ident
        elif kind == "cusip" and cusip is None:
            cusip = ident
        out.append({
            "fund_ref": ref, "as_of": as_of, "basis": basis, "line_no": len(out) + 1,
            "isin": isin, "cusip": cusip, "figi": None, "ticker": cell("ticker"), "exchange": cell("exchange"),
            "name": name, "asset_type": cell("asset_type"), "weight": dec(cell("weight")),
            "quantity": dec(cell("quantity")), "market_value": dec(cell("market_value")),
            "currency": cell("currency"), "country": cell("country"), "risk_country": None,
            "sector": cell("sector"), "industry": cell("industry"), "maturity": None, "coupon": None,
            "long_currency": None, "long_amount": None, "short_currency": None, "short_amount": None,
            "payload": {header[i]: str(v).strip() for i, v in enumerate(row)
                        if i not in known and i < len(header) and header[i] and str(v).strip()},
        })
    return out


def composition(ref: str, as_of: date, lns: list, *, provider: str, basis: str = "holdings",
                source_kind: str = "daily_file", fund_name=None, currency=None, net_assets=None) -> dict:
    weights = [float(x["weight"]) for x in lns if x["weight"] is not None]
    return {"fund_ref": ref, "as_of": as_of, "basis": basis, "provider": provider, "source_kind": source_kind,
            "fund_name": fund_name, "currency": currency, "net_assets": net_assets, "line_count": len(lns),
            "weight_sum": dec(round(sum(weights), 8)) if weights else None, "units_outstanding": None,
            "payload": {}}
