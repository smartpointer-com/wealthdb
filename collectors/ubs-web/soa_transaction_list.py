"""A Statement of assets' transaction list, read from word positions."""
from __future__ import annotations

import json
import re

from collectorkit.words import Page, Row, Word
from collectorkit.words import rows as word_rows

from pdf_common import ISIN_SHAPE, dmy_to_unix, to_float


# A Statement of assets may close with a "Transaction list": every
# securities booking in the statement's period, each as a block of
# printed rows. The column header stacks several labels per column, one
# per row of a block:
#
#   A            B (booking text | Number/Amount)  C              D                   E                   F                G
#   Trade date   Booking text  Number/Amount       Description    Cost/Purchase price Transaction price   Transaction gain Transaction value
#   Trade time                 Tax                 Custody account Exchange rate      Exchange rate       Exchange gain    Accrued interest
#   Value date                 Various             Account        Cost value                              Realized P/L     Settlement amount
#                              Brokerage                          Place of execution                                       in account currency
#                              Stock exchange
#                              Third-party executions
#                              Foreign Financial Transaction Tax
#
# So a figure means what the label in the same column and the same row
# of the header says. The plain text cannot tell those apart: a block's
# rows hold only the figures that apply, and an empty cell leaves no
# trace in it. The parser therefore works on word positions. Columns B
# and D to G are right-aligned to the right edge of their row-1 label,
# A and C are left-aligned, and the booking text is left-aligned at its
# label inside column B. A block starts at a row that carries a date in
# column A and a booking text, and its row number is its distance from
# that row in units of the header's row pitch. A booking text that wraps
# past two rows pushes the rest of its block down by as many rows: every
# cell below the first row moves, except the trade time, which stays
# under the trade date.
#
# Column C carries the description, then the settlement and order
# numbers and the Valor/ISIN line, and below them the custody account
# and the cash account. Their rows vary with the description's length,
# so they are told apart by their shape rather than by row.
#
# Every figure is stored as printed, signs included. What each column
# means, and which currency it is in, is in migration 0014's header and
# DESIGN.md §3.10.

# The text a page carrying the list's header prints.
HEADER_TEXT = "Trade date Booking text"
# The list's period opens a line of the page header, which may share it
# with the next header line.
_TL_PERIOD_RE = re.compile(
    r"^\s*From\s+(?P<start>\d{2}\.\d{2}\.\d{4})\s+to\s+"
    r"(?P<end>\d{2}\.\d{2}\.\d{4})\b", re.M)
_TL_DATE_RE = re.compile(r"^\d{2}\.\d{2}\.\d{4}$")
# A list's closing totals, and the page footer below them.
_TL_END_RE = re.compile(r"^(?:Subtotal|Total)\b|^[A-Z0-9]{8}/\d{6}/")
# A figure, optionally led by its currency and followed by a one-letter
# price qualifier.
_TL_FIGURE_RE = re.compile(
    r"^(?:(?P<ccy>[A-Z]{3})\b\s*)?(?P<v>-?\d[\d ']*(?:\.\d+)?)?"
    r"(?:\s+[A-Za-z])?$")
_TL_PCT_RE = re.compile(r"^(?P<v>-?\d+(?:\.\d+)?)%$")
_TL_VALOR_ISIN_RE = re.compile(
    rf"\bValor\s+(?P<valor>\S+)\s+-\s+ISIN\s+(?P<isin>{ISIN_SHAPE})\b")
_TL_SETTLEMENT_NO_RE = re.compile(r"\bSettlement no\.:\s*(?P<no>\S+)")
_TL_ORDER_NO_RE = re.compile(r"\bOrder no\.:\s*(?P<no>\S+)")
_TL_CUSTODY_ACCOUNT_RE = re.compile(r"^\d{3,4}-\d+\.[A-Z0-9]+$")
_TL_CASH_ACCOUNT_RE = re.compile(r"^CH\d{2}[A-Z0-9]{17}$")
# The rows of column B below the quantity, by the header's labels.
_TL_CHARGES = (
    (1, "taxes"),                       # Tax
    (2, "fees"),                        # Various
    (3, "commission"),                  # Brokerage
    (4, "stock_exchange_fees"),         # Stock exchange
    (5, "third_party_fees"),            # Third-party executions
    (6, "financial_transaction_tax"),   # Foreign Financial Transaction Tax
)
# How far a word's edge may sit from its column's edge.
_TL_SLACK = 3.0


def _tl_label_at(words: tuple[Word, ...], first: str, second: str,
                 start: int = 0) -> int | None:
    """Index of the two-word label `first second` in a header row."""
    for k in range(start, len(words) - 1):
        if words[k].text == first and words[k + 1].text == second:
            return k
    return None


def _tl_anchors(rows: list[Row]) -> dict | None:
    """The column edges, read from the header's first row, and the row
    pitch, read from the distance to its second. None when the page
    carries no transaction-list header."""
    at = next((i for i, row in enumerate(rows)
               if [w.text for w in row.words[:4]]
               == ["Trade", "date", "Booking", "text"]), None)
    if at is None:
        return None
    ws = rows[at].words
    number = next((w for w in ws if w.text == "Number/Amount"), None)
    desc = next((w for w in ws if w.text == "Description"), None)
    d = _tl_label_at(ws, "Cost/Purchase", "price")
    e = _tl_label_at(ws, "Transaction", "price", d or 0)
    f = _tl_label_at(ws, "Transaction", "gain", e or 0)
    g = _tl_label_at(ws, "Transaction", "value", f or 0)
    if None in (number, desc, d, e, f, g):
        return None
    pitch = rows[at + 1].top - rows[at].top if at + 1 < len(rows) else 0
    return {
        "top": rows[at].top,
        "pitch": pitch if pitch > 0 else 10.0,
        "booking_x0": ws[2].x0,
        "number_x1": number.x1,
        "desc_x0": desc.x0,
        "d_x0": ws[d].x0,
        "right": {"D": ws[d + 1].x1, "E": ws[e + 1].x1,
                  "F": ws[f + 1].x1, "G": ws[g + 1].x1},
    }


def _tl_cells(row_words: tuple[Word, ...], anchors: dict
              ) -> dict[str, list[Word]]:
    """Sort one printed row's words into columns: 'A' to 'G', and 'text'
    for the booking text inside column B."""
    cells: dict[str, list[Word]] = {}
    booking_run = False
    prev_x1 = None
    for w in sorted(row_words, key=lambda w: w.x0):
        if w.x0 < anchors["booking_x0"] - _TL_SLACK:
            col = "A"
        elif w.x1 <= anchors["number_x1"] + _TL_SLACK:
            # The booking text is a run of words starting at its label's
            # left edge; anything else this far left is a right-aligned
            # figure of column B.
            if abs(w.x0 - anchors["booking_x0"]) <= _TL_SLACK:
                booking_run = True
            elif booking_run and prev_x1 is not None and w.x0 - prev_x1 > 6:
                booking_run = False
            col = "text" if booking_run else "B"
        elif (w.x0 >= anchors["desc_x0"] - _TL_SLACK
              and w.x1 < anchors["d_x0"]):
            col = "C"
        else:
            col = next((c for c, edge in anchors["right"].items()
                        if w.x1 <= edge + _TL_SLACK), "G")
        prev_x1 = w.x1
        cells.setdefault(col, []).append(w)
    return cells


def _tl_blocks(rows: list[Row], anchors: dict
               ) -> list[dict[tuple[str, int], str]]:
    """Group the rows below the header into bookings. Each booking maps
    (column, row number) to the text printed there."""
    blocks: list[dict[tuple[str, int], str]] = []
    current: dict[tuple[str, int], str] | None = None
    start_top = 0.0
    for row in rows:
        if row.top <= anchors["top"] + 7.5 * anchors["pitch"]:
            continue                                 # the header itself
        cells = _tl_cells(row.words, anchors)
        first_column = [w.text for w in cells.get("A", [])]
        if _TL_END_RE.match(" ".join(first_column)):
            current = None
            continue
        if (first_column and _TL_DATE_RE.match(first_column[0])
                and "text" in cells):
            current = {}
            blocks.append(current)
            start_top = row.top
        if current is None:
            continue
        k = round((row.top - start_top) / anchors["pitch"])
        for col, ws in cells.items():
            text = " ".join(w.text for w in ws)
            key = (col, k)
            current[key] = f"{current[key]} {text}" if key in current else text
    return blocks


def _tl_figure(text: str | None) -> tuple[str | None, float | None]:
    """(currency, value) of a printed figure such as 'HKD -1 234.56'."""
    m = _TL_FIGURE_RE.match(text or "")
    if not m:
        return None, None
    return m["ccy"], to_float(m["v"])


def _tl_pct(text: str | None) -> float | None:
    m = _TL_PCT_RE.match(text or "")
    return float(m["v"]) if m else None


def _tl_trade(block: dict[tuple[str, int], str]) -> dict:
    """The columns of one booking, from its cells. A cell is asked for
    by its header row; a booking text longer than two rows shifts it."""
    extra = max(0, max(k for col, k in block if col == "text") - 1)

    def cell(key: tuple[str, int]) -> str | None:
        col, k = key
        if k > (1 if col == "A" else 0):
            k += extra
        return block.get((col, k))

    ccy, cost_price = _tl_figure(cell(("D", 0)))
    _, quantity = _tl_figure(cell(("B", 0)))
    _, price = _tl_figure(cell(("E", 0)))
    _, value = _tl_figure(cell(("G", 0)))
    _, accrued = _tl_figure(cell(("G", 1)))
    settlement_ccy, settlement = _tl_figure(cell(("G", 3)) or cell(("G", 2)))
    trade = {
        "trade_date": dmy_to_unix(cell(("A", 0)) or ""),
        "trade_time": cell(("A", 1)),
        "value_date": dmy_to_unix(cell(("A", 2)) or ""),
        "booking_text": " ".join(
            block[key] for key in sorted(k for k in block if k[0] == "text")),
        "quantity": quantity,
        "currency_iso": ccy,
        "cost_price": cost_price,
        "acquisition_fx_rate": to_float(cell(("D", 1))),
        "cost_basis": to_float(cell(("D", 2))),
        "place_of_execution": cell(("D", 3)),
        "transaction_price": price,
        "transaction_fx_rate": to_float(cell(("E", 1))),
        "transaction_gain_pct": _tl_pct(cell(("F", 0))),
        "exchange_gain_pct": _tl_pct(cell(("F", 1))),
        "realized_pl_pct": _tl_pct(cell(("F", 2))),
        "transaction_value": value,
        "accrued_interest": accrued,
        "settlement_amount": settlement,
        "settlement_currency_iso": settlement_ccy,
        "charges_currency_iso": None,
        "valor": None, "isin": None, "settlement_no": None, "order_no": None,
        "custody_account": None, "account_iban": None,
    }
    for k, col in _TL_CHARGES:
        charge_ccy, trade[col] = _tl_figure(cell(("B", k)))
        trade["charges_currency_iso"] = (trade["charges_currency_iso"]
                                         or charge_ccy)

    # Column C, top to bottom: the description up to the first of the
    # settlement number, the order number and the Valor/ISIN line; then
    # the accounts, told apart by their shape.
    name: list[str] = []
    reached_ids = False
    for key in sorted((k for k in block if k[0] == "C"), key=lambda k: k[1]):
        line = block[key]
        m = _TL_VALOR_ISIN_RE.search(line)
        if m:
            trade["valor"], trade["isin"] = m["valor"], m["isin"]
        m = _TL_SETTLEMENT_NO_RE.search(line)
        if m:
            trade["settlement_no"] = m["no"]
        m = _TL_ORDER_NO_RE.search(line)
        if m:
            trade["order_no"] = m["no"]
        if trade["valor"] or trade["settlement_no"] or trade["order_no"]:
            reached_ids = True
        if _TL_CUSTODY_ACCOUNT_RE.match(line):
            trade["custody_account"] = line
        elif _TL_CASH_ACCOUNT_RE.match(line.replace(" ", "")):
            trade["account_iban"] = line.replace(" ", "")
        elif not reached_ids:
            name.append(line)
    trade["security_name"] = " ".join(name) or None
    return trade


def parse_transaction_list(list_pages: list[Page], shared: dict) -> list[dict]:
    """One `statement_trades` row per booking a Statement of assets lists.

    `list_pages` are the pages that carry the transaction-list header;
    `shared` holds the columns every row of the statement shares (its
    token, as-of date, portfolio and reporting currency). The row's `seq`
    is its place in the list, which keys it within its document.
    """
    if not list_pages:
        return []
    shared = {**shared, "period_start": None, "period_end": None}
    m = _TL_PERIOD_RE.search(list_pages[0].text)
    if m:
        shared["period_start"] = dmy_to_unix(m["start"])
        shared["period_end"] = dmy_to_unix(m["end"])

    trades: list[dict] = []
    for page in list_pages:
        # Rows by the top of their words, three points apart: the ledger's
        # reading too (account_statement.py).
        rows = word_rows(page.words(x_tolerance=2), 3.0, "first")
        anchors = _tl_anchors(rows)
        if anchors is None:
            continue
        for block in _tl_blocks(rows, anchors):
            trades.append({
                **shared,
                **_tl_trade(block),
                "seq": len(trades) + 1,
                "payload": json.dumps(
                    {f"{col}{k}": text for (col, k), text in sorted(
                        block.items(), key=lambda kv: (kv[0][1], kv[0][0]))},
                    ensure_ascii=False),
            })
    return trades
