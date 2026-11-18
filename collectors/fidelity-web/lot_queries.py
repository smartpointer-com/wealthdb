"""The lot step's pure half: reading the positions page's lot queries.

Row readers and signatures, the bodies of the four read-only queries,
the decoded account context, the lot index and bundle the step writes,
and its pacing. The step itself, which sends the
queries from the page, is `download.scrape_lots`.
"""

from __future__ import annotations

import base64
import json
import logging
import random
import re
import time
import zlib

from collectorkit import bronze, compress

import lot_parsers
from landmarks import (
    LOT_LONG_PAUSE_EVERY, LOT_LONG_PAUSE_S, LOT_REQUEST_PAUSE_S,
    LOTS_PAGE_SIZE,
)

# The walk's own logger: a record reads the same whichever module
# writes it.
log = logging.getLogger("fidelity-web.download")


def _row_val(row, *path):
    """The numeric `val` at ``path`` in a poswebex row, or None. A cell
    is ``{"val": …, "disp": …}``; an empty one carries ``""`` for val."""
    cell = row
    for key in path:
        if not isinstance(cell, dict):
            return None
        cell = cell.get(key)
    val = cell.get("val") if isinstance(cell, dict) else None
    return val if isinstance(val, (int, float)) else None


def lot_position_rows(positions):
    """The rows of a poswebex positions response whose lots can be
    opened: position rows the page marks `isEligibleForLots`."""
    for row in (positions or {}).get("rowData") or []:
        if not str(row.get("rowType", "")).startswith("POSITION"):
            continue
        if (row.get("meta") or {}).get("isEligibleForLots"):
            yield row


def lot_position_id(row):
    """A position's key within its account: the CUSIP, else the
    symbol."""
    sym = row.get("sym") or {}
    return sym.get("cusip") or sym.get("name")


def open_lot_signature(row):
    """What decides whether a position's lots must be fetched again: its
    quantity and cost basis total. A buy, a sale, a reinvestment or a
    basis adjustment changes one of them."""
    return {
        "quantity": _row_val(row, "qty"),
        "cost_basis_total": _row_val(row, "cstBasStk", "top"),
    }


def closed_lot_signature(row):
    """What decides whether a closed position's lots must be fetched
    again: its proceeds, cost basis and gain. A further sale of the
    same security in the year changes them."""
    return {
        "proceeds": _row_val(row, "proceedsAmt"),
        "cost_basis": _row_val(row, "cstBasCloPos"),
        "gain_loss": _row_val(row, "totGLCloPos"),
    }


def _row_symbol(row):
    sym = row.get("sym") or {}
    return {"symbol": sym.get("name"), "cusip": sym.get("cusip")}


def closedpositions_body(ctx, account, tax_year, ineligible):
    """The closedpositions query the page sends for one account's
    closed positions in a tax year. ``ineligible`` is the page's list
    of accounts without lots."""
    return {
        "dateType": "TAX_YEAR",
        "taxYear": str(tax_year),
        "accts": [account],
        "pico": ctx["body"].get("pico"),
        "externalCustId": ctx["body"].get("externalCustId"),
        "settings": {"isUnadjusted": False, "filter": ""},
        "ineligibleAccounts": list(ineligible),
    }


def closedlots_body(ctx, account, cusip, tax_year):
    """The closedlots query the page sends for one closed position."""
    return {
        "acctNum": account,
        "cusip": cusip,
        "pageNum": 1,
        "startDate": f"{tax_year}-01-01",
        "endDate": f"{tax_year}-12-31",
        "pico": ctx["body"].get("pico"),
        "isUnadjusted": False,
        "isDownLoad": True,
    }


def openlots_body(account, row, page_num):
    """The openlots request the page sends for one position's lot
    table, one page at a time."""
    meta = row.get("meta") or {}
    sym = row.get("sym") or {}
    return {
        "acctNum": account,
        "acctType": meta.get("acctType"),
        "cusip": sym.get("cusip"),
        "symbol": sym.get("name"),
        "holdingType": meta.get("holdingType"),
        "isUnadjusted": False,
        "isCostBasisAdjusted": bool(meta.get("isFixedIncome")),
        "pageNum": page_num,
        "pageSize": LOTS_PAGE_SIZE,
        "returnBasketTaxLotDetail": False,
        "positionDetail": {
            "lastPrice": _row_val(row, "lstPrStk", "top"),
            "securityType": meta.get("securityType"),
            "currentValue": _row_val(row, "curVal"),
            "quantity": _row_val(row, "qty"),
            "costBasis": meta.get("cstbasType"),
            "isCostBasisEditable": False,
            "isNotQuotable": bool(meta.get("isNotQuotable")),
            "isCrypto": bool(meta.get("isCrypto")),
        },
    }


_LOTS_LAST_PAGE_RE = re.compile(r'<meta name="lastPage" content="(\d+)"')


def openlots_last_page(html):
    """The page count an openlots response states, or None when the
    response is not a lot table (an error page, a sign-in bounce)."""
    m = _LOTS_LAST_PAGE_RE.search(html or "")
    return int(m.group(1)) if m else None


def decode_pico(pico):
    """The account list inside the page's `pico` context (base64 of a
    gzipped JSON object), or [] when it does not decode."""
    try:
        obj = json.loads(zlib.decompress(base64.b64decode(pico),
                                         zlib.MAX_WBITS | 16))
    except (TypeError, ValueError, zlib.error):
        return []
    accounts = obj.get("accounts") if isinstance(obj, dict) else None
    return accounts if isinstance(accounts, list) else []


def lot_ineligible_accounts(pico_accounts):
    """The accounts the page treats as having no lots, in the page's
    order: anything that is not a brokerage account, and 529 plans."""
    return [str(a["accountId"]) for a in pico_accounts
            if a.get("accountId")
            and (a.get("cit") or not a.get("brokerageAccount"))]


def previous_lot_index(dest_root, current_slug):
    """The newest lot index in a complete dump before ``current_slug``,
    or None. An index carries forward the fetch each position's lots
    came from, so the newest one alone says what is already in bronze.
    A dump that did not complete, or has no manifest, is passed over:
    `load` never reads it (`bronze.run_is_loadable`), so a fetch it holds
    never reaches silver, and `prune` deletes it."""
    for run_dir in reversed(list(bronze.iter_run_dirs(dest_root))):
        if run_dir.name >= current_slug:
            continue
        if not bronze.run_is_loadable(run_dir):
            continue
        index = lot_parsers.read_lot_index(run_dir / "lots")
        if index is not None:
            return index
    return None


def write_lot_index(lots_dir, index):
    """Write the lot index compact and compressed. It lists every
    lot-eligible position on every run, so it is the step's largest
    file; without indentation and compressed it is about a tenth of
    its pretty-printed size. The plain file is written atomically first,
    so a failed compression leaves a readable index."""
    path = lots_dir / lot_parsers.LOTS_INDEX
    bronze.atomic_write_bytes(path, json.dumps(
        index, separators=(",", ":"), sort_keys=True).encode("utf-8"))
    compress.compress_best_effort(path, log)


class LotFetchError(RuntimeError):
    """A lot request came back as something other than its answer."""


class _LotBundle:
    """Every response of a run's lot step, one JSON line each, in one
    file: ``{"id", "endpoint", "account", "position", "tax_year",
    "page", "body"}``, the body as received.

    One file per response would cost a filesystem block each, and a
    run fetches only new and changed positions, so no response repeats
    an earlier run's bytes for the dedup sweep to share. In one file the
    repeated table markup also compresses once. Lines are flushed as
    they arrive, so a run that dies keeps what it fetched; ``close``
    compresses the finished file."""

    def __init__(self, path):
        self.path = path
        self._fh = open(path, "w", encoding="utf-8")
        self._next = 0

    def add(self, endpoint, body, *, account, position=None,
            tax_year=None, page=None):
        """Append one response; returns its record id."""
        rid = self._next
        self._next += 1
        self._fh.write(json.dumps({
            "id": rid, "endpoint": endpoint, "account": account,
            "position": position, "tax_year": tax_year, "page": page,
            "body": body}, ensure_ascii=False) + "\n")
        self._fh.flush()
        return rid

    def close(self):
        self._fh.close()
        if self._next:
            compress.compress_best_effort(self.path, log)
        else:
            self.path.unlink(missing_ok=True)


class LotStepAborted(RuntimeError):
    """The lot step stopped early; see _LotFailures."""


def _lot_pause():
    if random.randrange(LOT_LONG_PAUSE_EVERY) == 0:
        time.sleep(random.uniform(*LOT_LONG_PAUSE_S))
    else:
        time.sleep(random.uniform(*LOT_REQUEST_PAUSE_S))
