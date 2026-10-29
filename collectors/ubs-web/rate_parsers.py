#!/usr/bin/env python3
"""
Bronze → silver parsers for the cash accounts' interest conditions.

Pure functions over what `rates.py` reads and stores: the modern account
JSON, the legacy contract list that leads to an account, and the legacy
"Account information" page. No database handle and no filesystem, so the
capture and the load read a page the same way and each is tested against
an invented payload. DESIGN.md §3.11 has the shapes.

A condition becomes a row dict of `side`, `tier`, `rate_pct`,
`upper_limit`, `valid_from` and the source fields it was read from.
Tiers count from 1 per side, in the order the source lists them. A
value that does not read drops its row rather than standing in as a
zero, so a misread page never states a rate.
"""

from __future__ import annotations

import re
from datetime import datetime
from html.parser import HTMLParser

import landmarks as ubs  # local module

SIDE_CREDIT = "credit"
SIDE_DEBIT = "debit"

# The API's `conditionType` values, mapped to the silver side.
_API_SIDES = {"CREDIT": SIDE_CREDIT, "DEBIT": SIDE_DEBIT}

# The legacy page's table captions, mapped to the silver side.
_LEGACY_TABLES = {"credit interest": SIDE_CREDIT,
                  "debit interest": SIDE_DEBIT}
# The columns every legacy rate table carries, in its header row.
_LEGACY_COLUMNS = ("valid from", "interest rate", "limit")

# A CH IBAN as printed: two letters, two check digits, then 17
# characters, in groups of four or run together.
_IBAN_RE = re.compile(r"\bCH\d{2}(?:\s?[0-9A-Z]){17}\b")
# The account page's chooser row for its own account. The chooser
# lists every other account beside it, so only this row names the page's.
_SELECTED_ROW_CLASSES = {"bzeSelectDialogRow", "selected"}

_PCT_RE = re.compile(r"^([+-]?\d+(?:\.\d+)?)\s*%?$")
_DMY_RE = re.compile(r"^(\d{2})\.(\d{2})\.(\d{4})$")
_ISO_RE = re.compile(r"^(\d{4})-(\d{2})-(\d{2})")

# The contract list's "Account details" menu item opens this prefix,
# the row's id under the details key, and this suffix.
_DETAILS_LINK_RE = re.compile(
    r'window\.open\(\\?"([^"\\]+)\\?"\s*\+\s*getLinkId\(\)\.'
    + re.escape(ubs.LEGACY_ACCOUNT_DETAILS_KEY)
    + r'\s*\+\s*\\?"([^"\\]*)\\?"')
# A row's ids, keyed by menu item, as its action button sets them.
_ROW_IDS_RE = re.compile(r"setLinkId\(\{([^}]*)\}\)")
_ROW_ID_RE = re.compile(r"(\w+)\s*:\s*'(AV\d+)'")


def parse_pct(text) -> float | None:
    """`4.188`, `4.188%` or a number -> 4.188; anything else -> None."""
    if isinstance(text, bool) or text is None:
        return None
    if isinstance(text, (int, float)):
        return float(text)
    m = _PCT_RE.match(str(text).strip())
    return float(m.group(1)) if m else None


def parse_amount(text) -> float | None:
    """An amount as printed (`100'000.00`, `CHF 100,000.00`, `250000`) or
    a number -> a float; blank -> None. A leading currency code and
    thousands separators go, the decimal point stays. Distinct from
    `load.parse_grouped_decimal`, which reads a trailing unit and no
    currency."""
    if isinstance(text, bool) or text is None:
        return None
    if isinstance(text, (int, float)):
        return float(text)
    s = re.sub(r"[\s'’,]", "", str(text))
    s = re.sub(r"^[A-Z]{3}", "", s)
    if not s:
        return None
    try:
        return float(s)
    except ValueError:
        return None


def parse_date(text) -> str | None:
    """`dd.mm.yyyy` or `YYYY-MM-DD[...]` -> `YYYY-MM-DD`; else None."""
    if not text:
        return None
    s = str(text).strip()
    m = _DMY_RE.match(s)
    if m:
        d, mo, y = m.groups()
    else:
        m = _ISO_RE.match(s)
        if not m:
            return None
        y, mo, d = m.groups()
    try:
        return datetime(int(y), int(mo), int(d)).strftime("%Y-%m-%d")
    except ValueError:
        return None


# --------------------------------------------------------------------
# The modern API: GET /api/v3/cash-accounts/<accountId>
# --------------------------------------------------------------------

def api_iban(payload) -> str | None:
    """The account's IBAN, from the response's own `iban` field.

    Read at the top level first, then one level down, since the account
    may sit in a wrapper object. Only a field named `iban` counts: an
    IBAN-shaped value elsewhere could be a counterparty's.
    """
    if not isinstance(payload, dict):
        return None
    for scope in [payload, *(v for v in payload.values() if isinstance(v, dict))]:
        for key, value in scope.items():
            if key.lower() != "iban":
                continue
            if isinstance(value, dict):
                value = next((v for v in value.values() if isinstance(v, str)
                              and ubs.iban_canonical(v)), None)
            if iban := ubs.iban_canonical(
                    value if isinstance(value, str) else None):
                return iban
    return None


def api_states_conditions(payload) -> bool:
    """Whether the response states the account's conditions at all.

    The API leaves `interestItems` out unless asked for it. An empty
    list says the account has no conditions; a response without the
    list says nothing about them.
    """
    return isinstance(payload, dict) and isinstance(
        payload.get("interestItems"), list)


def api_conditions(payload) -> list[dict]:
    """The `interestItems` of one account response, as condition rows.

    An item whose condition type, rate or (where present) limit does
    not read is dropped. An empty `upperLimit` is the last tier: no
    limit.
    """
    items = payload.get("interestItems") if isinstance(payload, dict) else None
    out: list[dict] = []
    tiers = {SIDE_CREDIT: 0, SIDE_DEBIT: 0}
    for item in items if isinstance(items, list) else []:
        if not isinstance(item, dict):
            continue
        side = _API_SIDES.get(str(item.get("conditionType") or "").upper())
        rate = parse_pct(item.get("interestRate"))
        raw_limit = item.get("upperLimit")
        limit = parse_amount(raw_limit)
        if side is None or rate is None or (
                limit is None and str(raw_limit or "").strip()):
            continue
        tiers[side] += 1
        out.append({
            "side": side,
            "tier": tiers[side],
            "rate_pct": rate,
            "upper_limit": limit,
            "valid_from": parse_date(item.get("validFrom")),
            "source": item,
        })
    return out


# --------------------------------------------------------------------
# The legacy asset view
# --------------------------------------------------------------------

class _PageTables(HTMLParser):
    """The page's tables.

    Each table is kept as its rows of cell texts, with the last few
    pieces of text before it as its lead, where a heading sits. Tables
    nest, so each open table keeps its own row and cell, and a nested
    table's text is also its cell's text in the table around it: the
    page puts a rate table's caption in a table of its own, in the first
    row.
    """

    def __init__(self) -> None:
        super().__init__(convert_charrefs=True)
        self.tables: list[dict] = []
        self._open: list[dict] = []
        self._lead: list[str] = []

    def handle_starttag(self, tag, attrs):
        if tag == "table":
            self._open.append({"lead": self._lead[-5:], "rows": [],
                               "row": None, "cell": None})
            self._lead = []
        elif self._open and tag == "tr":
            self._open[-1]["row"] = []
        elif self._open and tag in ("td", "th") \
                and self._open[-1]["row"] is not None:
            self._open[-1]["cell"] = []

    def handle_endtag(self, tag):
        if not self._open:
            return
        t = self._open[-1]
        if tag in ("td", "th") and t["cell"] is not None:
            t["row"].append(" ".join(" ".join(t["cell"]).split()))
            t["cell"] = None
        elif tag == "tr" and t["row"] is not None:
            t["rows"].append(t["row"])
            t["row"] = None
        elif tag == "table":
            self.tables.append(self._open.pop())
            if self._open and self._open[-1]["cell"] is not None:
                self._open[-1]["cell"].extend(
                    c for row in t["rows"] for c in row)

    def handle_data(self, data):
        if self._open and self._open[-1]["cell"] is not None:
            self._open[-1]["cell"].append(data)
        if data.strip():
            self._lead.append(" ".join(data.split()))

    def close(self):
        super().close()
        while self._open:
            self.tables.append(self._open.pop())


def _tables(html: str) -> list[dict]:
    p = _PageTables()
    p.feed(html)
    p.close()
    return p.tables


class _SelectedRow(HTMLParser):
    """The text of the account chooser's selected row.

    The row ends at the end tag that matches its own, counted over its
    own tag name alone, so a tag left open inside it never carries the
    read on into the next row.
    """

    def __init__(self) -> None:
        super().__init__(convert_charrefs=True)
        self.text: list[str] | None = None
        self._tag: str | None = None
        self._depth = 0

    def handle_starttag(self, tag, attrs):
        if self._depth:
            if tag == self._tag:
                self._depth += 1
        elif self.text is None and _SELECTED_ROW_CLASSES <= set(
                (dict(attrs).get("class") or "").split()):
            self.text, self._tag, self._depth = [], tag, 1

    def handle_endtag(self, tag):
        if self._depth and tag == self._tag:
            self._depth -= 1

    def handle_data(self, data):
        if self._depth:
            self.text.append(data)


def legacy_iban(html: str) -> str | None:
    """The account's IBAN, read off the chooser's selected row.

    The page names its account nowhere else: the chooser lists every
    account the client holds, and marks the page's own as selected.
    """
    p = _SelectedRow()
    p.feed(html)
    p.close()
    m = _IBAN_RE.search(" ".join(p.text or []))
    return ubs.iban_canonical(m.group(0)) if m else None


def _rate_table_side(table: dict) -> str | None:
    """The side a table states, or None when it is not a rate table.

    A rate table has the three rate columns in a header row. The side
    is named by the nearest text above that row: a caption row inside
    the table, else the heading before it.
    """
    rows = table["rows"]
    header = next((i for i, r in enumerate(rows)
                   if tuple(c.lower() for c in r[:3]) == _LEGACY_COLUMNS), None)
    if header is None:
        return None
    labels = [*table["lead"], *(" ".join(r) for r in rows[:header])]
    for label in reversed(labels):
        for name, side in _LEGACY_TABLES.items():
            if name in label.lower():
                return side
    return None


def legacy_conditions(html: str) -> list[dict]:
    """The credit and debit conditions of one legacy account page.

    The page repeats its tables in its print variants, so each side is
    read from the first table that states it and later ones are
    ignored. A table with its headers and no rows states nothing.
    """
    out: list[dict] = []
    seen: set[str] = set()
    for table in _tables(html):
        side = _rate_table_side(table)
        if side is None or side in seen:
            continue
        seen.add(side)
        rows = table["rows"]
        start = next(i for i, r in enumerate(rows)
                     if tuple(c.lower() for c in r[:3]) == _LEGACY_COLUMNS) + 1
        tier = 0
        for cells in rows[start:]:
            if len(cells) < 3:
                continue
            valid_from = parse_date(cells[0])
            rate = parse_pct(cells[1])
            limit = parse_amount(cells[2])
            if valid_from is None or rate is None or (
                    limit is None and cells[2].strip()):
                continue
            tier += 1
            out.append({
                "side": side,
                "tier": tier,
                "rate_pct": rate,
                "upper_limit": limit,
                "valid_from": valid_from,
                "source": {"valid_from": cells[0], "interest_rate": cells[1],
                           "limit": cells[2]},
            })
    return out


class _AccountRows(HTMLParser):
    """Each table row whose action button sets the row's ids, with the
    text of that row. A row is the innermost one around the button."""

    def __init__(self) -> None:
        super().__init__(convert_charrefs=True)
        self.rows: list[tuple[str, str]] = []
        self._open: list[dict] = []

    def handle_starttag(self, tag, attrs):
        if tag == "tr":
            self._open.append({"text": [], "ids": None})
        elif self._open:
            m = _ROW_IDS_RE.search(dict(attrs).get("onclick") or "")
            if m and self._open[-1]["ids"] is None:
                self._open[-1]["ids"] = m.group(1)

    def handle_endtag(self, tag):
        if tag == "tr" and self._open:
            row = self._open.pop()
            if row["ids"] is not None:
                self.rows.append((row["ids"],
                                  " ".join(" ".join(row["text"]).split())))
            if self._open:
                self._open[-1]["text"].extend(row["text"])

    def handle_data(self, data):
        if self._open:
            self._open[-1]["text"].append(data)


def legacy_account_rows(html: str) -> list[dict]:
    """The contract list's account rows, each with the IBAN it prints
    (None when it prints none) and the reference its "Account details"
    item opens.

    The reference is the page's own: the menu item's link prefix and
    suffix around the row's id under the details key. The row's other
    ids (its payment and transfer items among them) are never read. A
    row with no details id, and every row of a page with no details
    item, is left out. Each reference appears once, in list order.
    """
    template = _DETAILS_LINK_RE.search(html)
    if template is None:
        return []
    prefix, suffix = template.groups()
    p = _AccountRows()
    p.feed(html)
    p.close()
    out: list[dict] = []
    seen: set[str] = set()
    for ids, text in p.rows:
        link_id = dict(_ROW_ID_RE.findall(ids)).get(
            ubs.LEGACY_ACCOUNT_DETAILS_KEY)
        if link_id is None:
            continue
        ref = f"{prefix}{link_id}{suffix}"
        if ref in seen:
            continue
        seen.add(ref)
        m = _IBAN_RE.search(text)
        out.append({"iban": ubs.iban_canonical(m.group(0)) if m else None,
                    "ref": ref})
    return out
