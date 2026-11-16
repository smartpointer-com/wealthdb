"""
fidelity-web silver: open and closed lots.

The positions page's lot step lands in `open_lots` and, as
'closed_positions', in `closed_lots`. The one `closed_lots` writer here
serves the statement and the 1099 passes too.
"""

from __future__ import annotations

import hashlib
import json
import logging

from collectorkit import compress

import lot_parsers
from silver_retail import _occurrence_id, account_key, normalize_payload

log = logging.getLogger("fidelity-web.load")


# ------------------------------------------------------------
# Open lots (migration 0011)
# ------------------------------------------------------------

# How far a position's lots may sum from the figures the page states for
# the position before the load says so: (index field, lot column,
# tolerance). The lot tables print cents and thousandths, so honest
# rounding stays well inside these.
_LOT_QUANTITY_TOLERANCE = 0.0005
_LOT_COST_TOLERANCE = 0.01
_OPEN_LOT_SUMS = (
    ("quantity", "quantity", _LOT_QUANTITY_TOLERANCE),
    ("cost_basis_total", "cost_basis", _LOT_COST_TOLERANCE),
)
_CLOSED_LOT_SUMS = (
    ("proceeds", "proceeds", _LOT_COST_TOLERANCE),
    ("cost_basis", "cost_basis", _LOT_COST_TOLERANCE),
)


def _read_lot_step(dump_dir, run_meta):
    """This dump's lot step as ``(index, records, hash_to_id)``, or None
    when it has none.

    ``records`` maps each record id in the bundle (`lots/lots.jsonl`,
    compressed or not) to its line. A line that does not parse, the tail
    of a run killed mid-write, is skipped with a warning; an index entry
    that points at it then fails to load on its own."""
    lots_dir = dump_dir / "lots"
    index = lot_parsers.read_lot_index(lots_dir)
    if not index:
        return None
    records = {}
    path = compress.resolve_variant(lots_dir / lot_parsers.LOTS_BUNDLE)
    if path is not None:
        with compress.open_text(path) as fh:
            for line_no, line in enumerate(fh, 1):
                try:
                    rec = json.loads(line)
                    records[rec["id"]] = rec
                except (ValueError, KeyError, TypeError):
                    log.warning("%s: lots.jsonl line %d does not parse; "
                                "skipped", dump_dir.name, line_no)
    hash_to_id = {account_key(aid): aid
                  for aid in run_meta.get("accounts_enumerated") or []}
    return index, records, hash_to_id


def _lot_bodies(records, item):
    """The response bodies an index entry points at, in order. Raises
    ValueError when one is missing from the bundle."""
    bodies = []
    for rid in item.get("records") or []:
        rec = records.get(rid)
        if rec is None or not isinstance(rec.get("body"), str):
            raise ValueError(f"record {rid} is missing")
        bodies.append(rec["body"])
    return bodies


def _load_open_lots(conn, snapshot_at, dump_name, lot_step):
    """Load the open lots this dump fetched.

    Only the positions whose `fetched_in` is this dump are read; an
    unchanged position points at the earlier dump that fetched its
    lots, which loaded them then. Each fetched position is checked
    against the figures the positions query reported for it: a lot
    table whose quantities or costs do not add up to the position is
    loaded as printed, and the load warns."""
    index, records, hash_to_id = lot_step
    inserted = 0
    for item in index.get("open") or []:
        if item.get("fetched_in") != dump_name:
            continue
        aid = hash_to_id.get(item.get("account"))
        instr = (item.get("symbol") or item.get("position") or "").rstrip("*")
        if not aid or not instr:
            log.warning("%s: lot index entry for account %s names no "
                        "known account or symbol; skipped",
                        dump_name, item.get("account"))
            continue
        lots = []
        try:
            for page_num, html in enumerate(_lot_bodies(records, item), 1):
                sha = hashlib.sha256(html.encode("utf-8")).hexdigest()
                lots += [(lot, page_num, sha)
                         for lot in lot_parsers.parse_open_lots(html)]
        except ValueError as e:
            log.warning("%s: open lots of %s not loaded: %s",
                        dump_name, instr, e)
            continue
        conn.execute(
            "DELETE FROM open_lots WHERE snapshot_at = ? "
            "AND account_external_id = ? AND instrument_key = ?",
            (snapshot_at, aid, instr))
        for lot_index, (lot, page_num, sha) in enumerate(lots):
            conn.execute(
                "INSERT INTO open_lots ("
                "snapshot_at, account_external_id, instrument_key, "
                "lot_index, cusip, quantity, unit_cost, cost_basis, "
                "acquired_date, unrealized_gain_loss, current_value, term, "
                "source_sha256, payload"
                ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                (snapshot_at, aid, instr, lot_index, item.get("cusip"),
                 lot.get("quantity"), lot.get("unit_cost"),
                 lot.get("cost_basis"), lot.get("acquired_date"),
                 lot.get("unrealized_gain_loss"), lot.get("current_value"),
                 lot.get("term"), sha,
                 normalize_payload({"cells": lot["cells"],
                                    "page": page_num})),
            )
        inserted += len(lots)
        _check_lot_sums(dump_name, "open", instr, item,
                        [lot for lot, _, _ in lots], _OPEN_LOT_SUMS)
    return inserted


def _check_lot_sums(dump_name, kind, instr, item, lots, checks):
    """Warn when a position's ``kind`` ('open' or 'closed') lots miss a
    figure the page stated for the position, per ``(field, column,
    tolerance)`` of ``checks``. Each check runs only when the position
    states its figure."""
    for field, column, tolerance in checks:
        stated = item.get(field)
        if not isinstance(stated, (int, float)):
            continue
        total = sum(lot.get(column) or 0.0 for lot in lots)
        if abs(total - stated) > tolerance:
            log.warning("%s: the %s lots of %s sum to %s %.4f, the "
                        "position states %.4f", dump_name, kind, instr,
                        column, total, stated)


# ============================================================
# Closed lots (migrations 0010, 0012)
# ============================================================
#
# Three sources state realized lots: the Form 1099-B pages of a
# Consolidated 1099 (`document_kind = 'form_1099b'`), the sales in a
# supplied statement's Securities Bought & Sold section
# (`document_kind = 'statement'`), and the positions page's closed lots
# (`document_kind = 'closed_positions'`). All land in `closed_lots` as
# printed; the same sale in two of them stays twice, for gold to
# reconcile.

_CLOSED_LOT_COLUMNS = (
    "lot_id", "document_kind", "account_external_id", "tax_year",
    "form_prepared", "security_name", "instrument_key", "cusip", "action",
    "quantity", "acquired_date", "disposed_date", "settlement_date", "proceeds",
    "cost_basis", "accrued_market_discount",
    "wash_sale_disallowed", "realized_gain_loss", "fees", "federal_tax_withheld",
    "term", "covered", "form_8949_box", "specific_share_id", "corrected",
    "source_sha256", "payload",
)
_INSERT_CLOSED_LOT = (
    f"INSERT OR REPLACE INTO closed_lots ({', '.join(_CLOSED_LOT_COLUMNS)}) "
    f"VALUES ({', '.join('?' * len(_CLOSED_LOT_COLUMNS))})")


def _insert_closed_lot(conn, prefix, identity, occurrence, row):
    """Insert one `closed_lots` row under the id `_occurrence_id` derives
    from the lot's own content. ``row`` maps column names to values; the
    rest default to NULL."""
    row = dict(row, lot_id=_occurrence_id(prefix, identity, occurrence))
    conn.execute(_INSERT_CLOSED_LOT,
                 tuple(row.get(c) for c in _CLOSED_LOT_COLUMNS))


def _closed_lot_term(term):
    """A parser's 'short' / 'long' as the column's 'SHORT' / 'LONG'."""
    return term.upper() if term else None


def _bool_column(value):
    return None if value is None else int(bool(value))


def _insert_supplied_closed_lots(conn, pdf_path, parsed, sha):
    """Insert one `closed_lots` row per sale the parsed supplied statement
    lists under Securities Bought & Sold. Returns the count.

    The statement prints a sale, not its lots: one row carries the sale's
    basis, its realized gain and, when every lot shares one, its term. It
    prints the settlement date only, so `settlement_date` is set and the
    acquired and sold dates stay NULL."""
    inserted = 0
    occurrence: dict[str, int] = {}
    for account in parsed.get("accounts", []):
        aid = account.get("account_external_id")
        if not aid:
            continue
        for sale in account.get("sales", []):
            identity = "|".join(str(sale.get(k)) for k in (
                "settlement_date", "symbol", "action", "quantity", "amount",
                "cost_basis"))
            _insert_closed_lot(conn, "stmt_", f"{aid}|{identity}", occurrence, {
                "document_kind": "statement",
                "account_external_id": aid,
                "security_name": sale.get("description"),
                "instrument_key": sale.get("symbol"),
                "action": sale.get("action"),
                "quantity": sale.get("quantity"),
                "settlement_date": sale.get("settlement_date"),
                "proceeds": sale.get("amount"),
                "cost_basis": sale.get("cost_basis"),
                "realized_gain_loss": sale.get("gain_loss"),
                "fees": sale.get("transaction_cost"),
                "term": _closed_lot_term(sale.get("term")),
                "specific_share_id": _bool_column(sale.get("specific_share_id")),
                "source_sha256": sha,
                "payload": normalize_payload(
                    {**sale, "statement": pdf_path.name}),
            })
            inserted += 1
    return inserted


def _drop_supplied_closed_lots(conn):
    """Drop every statement-derived closed lot, for a re-derivation of all
    of them — the closed-lot counterpart of `_drop_supplied_activity`, and
    spent at the same moment. `document_kind` names exactly the rows the
    supplied pass writes."""
    return conn.execute(
        "DELETE FROM closed_lots WHERE document_kind = 'statement'").rowcount


def _load_closed_positions(conn, dump_name, lot_step):
    """Load the closed-position lots this dump fetched into
    `closed_lots` as 'closed_positions'.

    Each fetch is the page's whole lot list for one closed position in
    one tax year, so it replaces the rows an earlier fetch wrote for
    that position and year. A closed position whose lots do not sum to
    the proceeds or cost basis the page stated for it loads as
    printed, and the load warns."""
    index, records, hash_to_id = lot_step
    inserted = 0
    for item in index.get("closed") or []:
        if item.get("fetched_in") != dump_name:
            continue
        aid = hash_to_id.get(item.get("account"))
        instr = item.get("symbol") or item.get("position")
        year = item.get("tax_year")
        try:
            [body] = _lot_bodies(records, item)
            lots = json.loads(body)
        except ValueError:
            lots = None
        if not aid or not instr or not isinstance(lots, list):
            log.warning("%s: closed lots of a position in account %s not "
                        "loaded", dump_name, item.get("account"))
            continue
        sha = hashlib.sha256(body.encode("utf-8")).hexdigest()
        conn.execute(
            "DELETE FROM closed_lots WHERE document_kind = 'closed_positions' "
            "AND account_external_id = ? AND tax_year = ? "
            "AND instrument_key = ?", (aid, year, instr))
        occurrence: dict[str, int] = {}
        rows = [lot_parsers.closed_lot_row(lot) for lot in lots]
        for lot, row in zip(lots, rows, strict=True):
            identity = "|".join(str(row.get(k)) for k in (
                "acquired_date", "disposed_date", "quantity", "proceeds",
                "cost_basis"))
            _insert_closed_lot(
                conn, "web_", f"{aid}|{year}|{instr}|{identity}", occurrence,
                dict(row, document_kind="closed_positions",
                     account_external_id=aid, tax_year=year,
                     instrument_key=instr, cusip=item.get("cusip"),
                     source_sha256=sha, payload=normalize_payload(lot)))
        inserted += len(rows)
        _check_lot_sums(dump_name, "closed", instr, item, rows,
                        _CLOSED_LOT_SUMS)
    return inserted
