#!/usr/bin/env python3
"""
Swissquote bronze -> silver loader.

Walks a bronze directory tree (as produced by download.py), applies
any pending schema migrations, then loads each not-yet-loaded dump
into a SQLite silver database. One dump = one transaction; the
`dump_runs` row is the last INSERT before COMMIT, so failures
mid-load roll the whole dump back and re-runs retry idempotently.

Also scans `<bronze-dir>/manual/` for out-of-band artefacts
(typically e-tax statement PDFs) and indexes new files into the
`documents` table by content sha256.

Usage:
    load.py --silver-db <file> --bronze-dir <dir> [-v]
"""

from __future__ import annotations

import argparse
import csv
import json
import logging
import re
import sqlite3
import sys
from datetime import datetime, timezone
from pathlib import Path
from zoneinfo import ZoneInfo

from collectorkit import bronze, cli, silver

# Re-export for backward compatibility with existing tests that call
# load.apply_migrations(...) / load.current_schema_version(...) directly.
apply_migrations = silver.apply_migrations
current_schema_version = silver.current_schema_version

log = logging.getLogger("swissquote.load")

# All transaction times in Swissquote CSVs are wall-clock Europe/Zurich
# (CET in winter, CEST in summer). The loader converts to UTC epoch
# at insert time; the original string is retained in payload.
SWISSQUOTE_TZ = ZoneInfo("Europe/Zurich")

# CSV transactions: a literal "00000000" Order # is the source's
# placeholder for non-trade rows; we normalise to NULL.
ORDER_NUM_PLACEHOLDER = "00000000"

# A 3-letter uppercase string in the first column of the List of
# Assets XLS denotes a currency row; the last row reads "Total CHF"
# and is dropped.
ISO_CCY_RE = re.compile(r"^[A-Z]{3}$")


# ============================================================
# DB plumbing
# ============================================================

# Silver connections use the manual-transaction model (isolation_level=None;
# explicit BEGIN/COMMIT per window) with row_factory=Row — exactly what
# collectorkit's silver.open_db provides.
open_db = silver.open_db


def canonical_json(obj) -> str:
    """Stable JSON for content-based dedup. Sorted keys, no spaces."""
    return json.dumps(obj, sort_keys=True, separators=(",", ":"),
                      ensure_ascii=False)


# ============================================================
# Bronze scanner
# ============================================================

def _dump_is_complete(run_dir: Path) -> bool:
    """Whether ``run_dir`` holds a completed dump, per download.py's
    run.json status lifecycle.

    download.py writes ``{"status": "in-progress"}`` when it creates
    the run dir and atomically overwrites run.json with the terminal
    manifest carrying ``status == "complete"`` at the end. So:

    * ``status == "complete"``             -> complete (load it);
    * any other status (``"in-progress"``)  -> not complete (a crashed
      or still-running walk — do NOT ingest its partial artefacts);
    * a statusless run.json                 -> complete (predates the
      status field, where the manifest was written only at the end so
      its presence alone marked completion);
    * no run.json / unreadable / corrupt    -> not complete.

    Twin of download.dump_is_complete.
    """
    try:
        raw = (run_dir / "run.json").read_text(encoding="utf-8")
    except OSError:
        return False  # absent, or unreadable (e.g. run.json is a dir)
    try:
        meta = json.loads(raw)
    except ValueError:
        return False  # corrupt bytes — not evidence of completeness
    if not isinstance(meta, dict):
        return False
    status = meta.get("status")
    if status is None:
        return True  # legacy statusless manifest == complete
    return status == "complete"


def find_pending_dumps(
    conn: sqlite3.Connection, bronze_dir: Path,
) -> list[Path]:
    """Return complete run-dirs in bronze not yet recorded in dump_runs.

    A dump is complete only once its run.json carries
    ``status == "complete"`` — the terminal manifest download.py writes
    atomically at the end of a successful walk (a legacy statusless
    run.json counts too; see `_dump_is_complete`). A timestamp-named
    dir with no run.json, or one still carrying the in-progress marker
    download.py drops at run-dir creation, is a crashed/interrupted or
    still-running download; ingesting its partial artefacts would leak
    a partial balance snapshot into silver (and thence gold). Such dirs
    are skipped with a warning rather than aborting the whole load, so
    one bad download never blocks loading the good dumps around it.
    """
    loaded = {
        row["snapshot_at"]
        for row in conn.execute("SELECT snapshot_at FROM dump_runs;")
    }
    out = []
    for child in sorted(bronze_dir.iterdir()):
        if not child.is_dir():
            continue
        if not bronze.RUN_DIR_RE.match(child.name):
            continue
        ts = _run_dir_to_epoch(child.name)
        if ts in loaded:
            continue
        if not _dump_is_complete(child):
            log.warning(
                "Skipping incomplete dump %s (run.json absent or "
                "status != 'complete' — likely a crashed, interrupted, "
                "or still-running download)", child.name,
            )
            continue
        out.append(child)
    return out


def _run_dir_to_epoch(name: str) -> int:
    """Convert a YYYYMMDDTHHMMSSZ run-dir name to a UTC epoch second."""
    return bronze.parse_run_ts(name)


# ============================================================
# Transactions CSV parser
# ============================================================

# Column order in the Swissquote transactions CSV, in the order the
# loader expects to see them. If a future CSV adds/reorders columns
# the loader fails loud rather than silently dropping data — see
# `_check_csv_header`.
EXPECTED_CSV_HEADER = [
    "Date", "Order #", "Transaction", "Symbol", "Name", "ISIN",
    "Quantity", "Unit price", "Costs", "Accrued Interest",
    "Net Amount", "Balance", "Currency",
]


def _check_csv_header(actual: list[str], path: Path) -> None:
    if [c.strip() for c in actual] != EXPECTED_CSV_HEADER:
        raise SystemExit(
            f"Unexpected transactions CSV header in {path}:\n"
            f"  expected: {EXPECTED_CSV_HEADER}\n"
            f"  actual:   {actual}\n"
            "If Swissquote changed the schema, update EXPECTED_CSV_HEADER "
            "in load.py and add a migration if silver needs new columns."
        )


def _zurich_to_utc_epoch(s: str) -> int:
    dt = datetime.strptime(s.strip(), "%d-%m-%Y %H:%M:%S")
    return int(dt.replace(tzinfo=SWISSQUOTE_TZ).timestamp())


def _norm_blank(s: str) -> str | None:
    """Map empty/whitespace strings to None; keep substance intact."""
    s = s.strip() if s is not None else ""
    return s or None


def parse_transactions_csv(path: Path) -> list[dict]:
    """Read one transactions_NNN.csv into a list of row dicts."""
    with path.open("r", encoding="utf-8", newline="") as fh:
        reader = csv.reader(fh, delimiter=";")
        header = next(reader)
        _check_csv_header(header, path)
        rows = []
        for raw in reader:
            if not raw or all(not c.strip() for c in raw):
                continue  # tolerate trailing blank lines
            if len(raw) != len(EXPECTED_CSV_HEADER):
                raise SystemExit(
                    f"{path}: row has {len(raw)} fields, expected "
                    f"{len(EXPECTED_CSV_HEADER)}: {raw}"
                )
            d = dict(zip(EXPECTED_CSV_HEADER, raw, strict=True))
            rows.append(d)
    return rows


def load_transactions_csv(
    conn: sqlite3.Connection,
    customer_id: str,
    csv_path: Path,
) -> int:
    """Span-DELETE-then-INSERT for one transactions CSV.

    The DELETE clears every row of this account whose occurred_at
    falls between the CSV's own first and last row, inclusive. The
    INSERT replays the CSV's rows. Any upstream amendment inside that
    span (date shift, amount change, row removal) converges to truth
    on reload.

    The span comes from the rows, not from the window the export was
    requested for: an export can hold fewer days than its requested
    window, or no rows at all. Deleting the requested window would
    drop rows that an earlier dump loaded and this one does not
    replace. An empty CSV deletes nothing.
    """
    rows = parse_transactions_csv(csv_path)
    if not rows:
        return 0
    occurred = [_zurich_to_utc_epoch(r["Date"]) for r in rows]
    conn.execute(
        "DELETE FROM transactions "
        "WHERE account_external_id = ? "
        "  AND occurred_at >= ? AND occurred_at <= ?;",
        (customer_id, min(occurred), max(occurred)),
    )

    n = 0
    for r, occurred_at in zip(rows, occurred, strict=True):
        order_num = _norm_blank(r["Order #"])
        if order_num == ORDER_NUM_PLACEHOLDER:
            order_num = None
        isin = _norm_blank(r["ISIN"])
        symbol = _norm_blank(r["Symbol"])
        currency = r["Currency"].strip()
        # Net Amount: signed decimal, no thousands separator.
        net_amount = float(r["Net Amount"].replace(",", "."))
        payload = canonical_json({
            "date_raw": r["Date"],
            "order_num_raw": r["Order #"],
            "transaction": r["Transaction"].strip(),
            "symbol": r["Symbol"].strip(),
            "name": r["Name"].strip(),
            "isin": r["ISIN"].strip(),
            "quantity": r["Quantity"].strip(),
            "unit_price": r["Unit price"].strip(),     # may end in ' %' for bonds
            "costs": r["Costs"].strip(),
            "accrued_interest": r["Accrued Interest"].strip(),
            "net_amount": r["Net Amount"].strip(),
            "balance": r["Balance"].strip(),
            "currency": r["Currency"].strip(),
        })
        conn.execute(
            "INSERT INTO transactions("
            " account_external_id, occurred_at, transaction_type,"
            " order_num, isin, symbol, currency, net_amount, payload) "
            "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?);",
            (
                customer_id, occurred_at, r["Transaction"].strip(),
                order_num, isin, symbol, currency, net_amount, payload,
            ),
        )
        n += 1
    return n


# ============================================================
# Positions XLS parser
# ============================================================

# Labelled columns of the Positions XLS header. The leading "" is the
# unlabelled section/asset-class column and is positional. Exports up
# to 2026-08-15 also carried one trailing blank cell after
# "Positions %" (14 cells); the 2026-08-19 export dropped it (13
# cells) with the labelled columns unchanged, so `_check_xls_header`
# ignores trailing blanks and both variants load identically.
POSITIONS_EXPECTED_HEADER = [
    "", "Symbol", "Quantity", "Unit cost", "Total value",
    "Daily change", "Daily chg. %", "Price", "CCY",
    "P&L Nominal CHF", "P&L % CHF", "Total value CHF",
    "Positions %",
]


def _xls_row(sheet, r: int) -> list:
    return [sheet.cell_value(r, c) for c in range(sheet.ncols)]


def _strip_cells(row: list) -> list:
    return [c.strip() if isinstance(c, str) else c for c in row]


def _trim_trailing_blanks(cells: list) -> list:
    """Drop trailing empty-string cells (leading/interior ones stay)."""
    end = len(cells)
    while end and cells[end - 1] == "":
        end -= 1
    return cells[:end]


def _check_xls_header(header: list, expected: list, path: Path,
                      kind: str) -> None:
    """Fail loud unless the header's labelled columns match `expected`
    exactly. Trailing blank cells are ignored: the XLS export engine
    has flapped on emitting one (see POSITIONS_EXPECTED_HEADER), and a
    cosmetic blank must not block the load — but any change to a
    labelled column (rename, reorder, add, remove) still aborts.
    """
    if _trim_trailing_blanks(header) != expected:
        raise SystemExit(
            f"Unexpected {kind} XLS header in {path}:\n"
            f"  expected: {expected} (+ any trailing blank cells)\n"
            f"  actual:   {header}"
        )


def parse_positions_xls(path: Path) -> list[dict]:
    """Parse positions.xls into a list of {symbol, currency, asset_class, ...} dicts.

    Section header rows ('ETFs', 'Bonds', ...) set the asset_class
    state for following position rows. Subtotal and Total rows are
    dropped. Position rows are recognised by a non-empty Symbol AND
    a numeric Quantity in their respective columns.
    """
    import xlrd  # local import — xlrd is only needed by this codepath
    wb = xlrd.open_workbook(str(path))
    sheet = wb.sheet_by_index(0)
    if sheet.nrows == 0:
        raise SystemExit(f"{path}: empty sheet")
    header = _strip_cells(_xls_row(sheet, 0))
    _check_xls_header(header, POSITIONS_EXPECTED_HEADER, path, "positions")

    out = []
    asset_class = None
    for r in range(1, sheet.nrows):
        row = _xls_row(sheet, r)
        stripped = _strip_cells(row)
        col0, col1 = stripped[0], stripped[1] if isinstance(stripped[1], str) else None
        # Section header: column 0 has a class name (e.g. "ETFs"),
        # everything else is blank/space.
        if isinstance(col0, str) and col0 and not (isinstance(col1, str) and col1):
            asset_class = col0
            continue
        # Subtotal: column 1 contains "subtotal".
        if isinstance(col1, str) and "subtotal" in col1.lower():
            continue
        # Total: column 1 is literally "Total".
        if col1 == "Total":
            continue
        # Position row: numeric Quantity at index 2.
        if isinstance(stripped[2], (int, float)) and col1:
            out.append({
                "asset_class": asset_class,
                "symbol": col1,
                "quantity": stripped[2],
                "unit_cost": stripped[3],
                "total_value": stripped[4],
                "daily_change": stripped[5],
                "daily_chg_pct": stripped[6],
                "price": stripped[7],
                "currency": stripped[8] if isinstance(stripped[8], str) else "",
                "pl_nominal_chf": stripped[9],
                "pl_pct_chf": stripped[10],
                "total_value_chf": stripped[11],
                "positions_pct": stripped[12],
            })
            continue
        log.debug("positions xls: ignoring row %d: %r", r, stripped)
    return out


def parse_position_details(path: Path) -> dict[tuple[str, str | None], dict]:
    """Read position_details.json and build a (symbol, currency) lookup.

    The lookup falls back to (symbol, None) when the JSON entry has
    no currency — caller can prefer the exact-currency match first.
    """
    if not path.is_file():
        return {}
    entries = json.loads(path.read_text(encoding="utf-8"))
    out: dict[tuple[str, str | None], dict] = {}
    for e in entries:
        sym = (e.get("symbol") or "").strip()
        ccy = e.get("currency")
        if sym:
            out[(sym, ccy)] = e
    return out


# ============================================================
# Portfolio Performance PDF parser
#
# Each Portfolio Performance PDF is an annual snapshot — its page-2
# "Asset allocation" table lists every held position at year-end
# (date in the title: "Portfolio performance at DD.MM.YYYY") with
# quantity, ISIN, average cost, market price, and CHF valuation.
# We parse it into a `pp:<doc_id>`-tagged set of `positions` rows
# so the silver `positions` table also covers historical year-ends
# rather than only the cluster of recent live snapshots.
#
# Cash rows are intentionally skipped — cash belongs in
# `currency_balances` (silver). Securities-only here, matching the
# `positions` table's existing scope.
# ============================================================

_PP_ISIN_RE = re.compile(r"\b([A-Z]{2}[A-Z0-9]{10})\b")
_PP_DATE_RE = re.compile(r"\b(\d{2})\.(\d{2})\.(\d{4})\b")
_PP_TITLE_DATE_RE = re.compile(
    r"Portfolio performance at\s+(\d{2})\.(\d{2})\.(\d{4})"
)
_PP_ACCOUNT_RE = re.compile(r"Account(?:\s*number)?\s+(\d{6,10})")
# Section header on the asset-allocation page: "Bonds in CHF",
# "Funds in CHF", "Equities in CHF", "Metals in CHF", etc.
_PP_SECTION_RE = re.compile(
    r"^\s*(Bonds|Funds|Equities|Metals|Structured products|Options|Other)\s+in\s+([A-Z]{3})\s*$"
)


def _pp_to_number(s: str) -> float:
    """Parse a Swiss-locale number like "1'234'567.89" or "106.150%"."""
    return float(s.replace("'", "").rstrip("%").strip())


def parse_portfolio_performance(pdf_path: Path) -> dict:
    """Parse a Portfolio Performance PDF into a snapshot dict.

    Returns: {
        "snapshot_date":        "YYYY-MM-DD",       # as-of date from title
        "account_external_id":  "<customer id>",
        "positions":            [{
            "asset_class":    "Bonds" | "Funds" | ...,
            "currency":       "CHF",
            "name":           "<security description>",
            "isin":           "<ISIN>",
            "quantity":       <float>,
            "avg_price":      <float>,
            "market_price":   <float>,
            "price_date":     "YYYY-MM-DD",
            "valuation_chf":  <float>,
            "account_pct":    <float>,
        }, ...]
    }

    Raises SystemExit on structural surprises (missing title, no
    asset-allocation page, unparseable row, etc.) — the goal is to
    fail loud rather than silently drop data.
    """
    import pypdf  # local import — only this code path needs it

    reader = pypdf.PdfReader(str(pdf_path))

    # Title + account ID can appear on any page; grep across all.
    snapshot_date = account_id = None
    for page in reader.pages:
        t = page.extract_text()
        if not snapshot_date:
            m = _PP_TITLE_DATE_RE.search(t)
            if m:
                snapshot_date = f"{m.group(3)}-{m.group(2)}-{m.group(1)}"
        if not account_id:
            m = _PP_ACCOUNT_RE.search(t)
            if m:
                account_id = m.group(1)
    if not snapshot_date:
        raise SystemExit(
            f"{pdf_path}: no 'Portfolio performance at <date>' title found"
        )
    if not account_id:
        raise SystemExit(f"{pdf_path}: no 'Account <number>' field found")

    # The asset-allocation table is on a single page; layout-mode
    # extraction keeps columns aligned enough for line-based parse.
    # Multiple pages may match the surface "Asset allocation" text
    # (the Table of Contents on page 0 includes it). We try every
    # candidate and return the first that yields rows; the TOC will
    # parse to zero rows and quietly skip.
    positions: list[dict] = []
    for page in reader.pages:
        layout = page.extract_text(extraction_mode="layout")
        if "Asset allocation" not in layout:
            continue
        rows = _pp_parse_asset_allocation(layout, pdf_path)
        if rows:
            positions = rows
            break
    if not positions:
        raise SystemExit(
            f"{pdf_path}: no asset-allocation page yielded any "
            "positions — PDF layout may have shifted."
        )

    return {
        "snapshot_date": snapshot_date,
        "account_external_id": account_id,
        "positions": positions,
    }


def _pp_parse_asset_allocation(layout_text: str, pdf_path: Path) -> list[dict]:
    """Walk the asset-allocation page's text lines, emit one dict per
    securities row. Cash rows are skipped (they belong in
    currency_balances). Section context comes from "<Class> in <CCY>"
    header lines preceding each block.
    """
    out: list[dict] = []
    current_class: str | None = None
    current_currency: str | None = None
    for raw_line in layout_text.splitlines():
        line = raw_line.strip()
        if not line:
            continue

        # Section header? Sets context for following rows.
        m = _PP_SECTION_RE.match(line)
        if m:
            current_class, current_currency = m.group(1), m.group(2)
            continue
        # Plain "Cash" header — skip section entirely; cash is not a
        # security and goes elsewhere in silver.
        if line == "Cash":
            current_class, current_currency = "Cash", None
            continue
        # Total / subtotal / header rows — not positions.
        if line.lower().startswith("total "):
            continue
        if "Currency" in line and "Reference" in line:
            continue
        if "Quantity" in line and "Security" in line:
            continue
        if "incl. accrued interests" in line:
            continue

        # Skip cash-section content rows.
        if current_class == "Cash":
            continue

        # A securities row contains an ISIN. The line layout is
        # quantity | security | ISIN | avg price | market price |
        # price date | valuation_chf | %. Numbers are Swiss-locale.
        m = _PP_ISIN_RE.search(line)
        if not m:
            continue
        isin = m.group(1)
        before, after = line.split(isin, 1)
        # `before` is "<quantity><whitespace><security text>" — the
        # PDF layout extraction collapses inner whitespace so the
        # quantity is the leading token.
        before_tokens = before.split()
        if not before_tokens:
            continue
        try:
            quantity = _pp_to_number(before_tokens[0])
        except ValueError:
            continue
        name = " ".join(before_tokens[1:]).strip()

        # The tail has 5 tokens: avg, market, date, valuation, %.
        # Bonds also carry an accrued-interests value on a separate
        # line (not on this one in layout mode) — we don't capture
        # it here; the valuation column already includes it per the
        # column-header text "Valuation in CHF incl. accrued interests".
        tail_tokens = after.split()
        if len(tail_tokens) < 5:
            raise SystemExit(
                f"{pdf_path}: positions row for {isin} has only "
                f"{len(tail_tokens)} tail tokens, expected 5+: {line!r}"
            )
        avg_price_raw, market_price_raw, price_date_raw = tail_tokens[:3]
        valuation_raw, pct_raw = tail_tokens[3], tail_tokens[4]

        md = _PP_DATE_RE.match(price_date_raw)
        if not md:
            raise SystemExit(
                f"{pdf_path}: unparseable price date {price_date_raw!r}"
                f" in row {line!r}"
            )
        price_date = f"{md.group(3)}-{md.group(2)}-{md.group(1)}"

        out.append({
            "asset_class": current_class,
            "currency": current_currency,
            "name": name,
            "isin": isin,
            "quantity": quantity,
            "avg_price": _pp_to_number(avg_price_raw),
            "market_price": _pp_to_number(market_price_raw),
            "price_date": price_date,
            "valuation_chf": _pp_to_number(valuation_raw),
            "account_pct": _pp_to_number(pct_raw),
        })

    return out


def load_portfolio_performance_docs(
    conn: sqlite3.Connection, bronze_root: Path,
) -> int:
    """Parse every not-yet-ingested Portfolio Performance doc and
    insert source-tagged positions rows.

    A doc is "already ingested" if any positions row has
    source='pp:<doc_id>'. Re-parses are idempotent: we DELETE then
    INSERT under that source tag in a transaction.

    Returns the number of newly-loaded docs.
    """
    docs = list(conn.execute(
        "SELECT swissquote_doc_id, account_external_id, bronze_path "
        "FROM documents "
        "WHERE document_type = 'Portfolio performance' "
        "  AND swissquote_doc_id IS NOT NULL"
    ))
    loaded = 0
    for r in docs:
        doc_id = r["swissquote_doc_id"]
        source_tag = f"pp:{doc_id}"
        already = conn.execute(
            "SELECT 1 FROM positions WHERE source = ? LIMIT 1;",
            (source_tag,),
        ).fetchone()
        if already:
            continue

        pdf_path = bronze_root / r["bronze_path"]
        log.info("Parsing Portfolio Performance: %s", pdf_path.name)
        try:
            result = parse_portfolio_performance(pdf_path)
        except SystemExit as e:
            # One bad PDF shouldn't kill the whole load; record the
            # parse failure and continue. -v surfaces the detail.
            log.error("Skip %s — parser failed: %s", pdf_path.name, e)
            continue

        # Anchor snapshot_at to end-of-day in Europe/Zurich on the
        # snapshot date. Consistent with how live-XLS rows derive
        # snapshot_at from the dump-run timestamp.
        d = datetime.fromisoformat(result["snapshot_date"])
        snapshot_at = int(
            d.replace(hour=23, minute=59, second=59,
                      tzinfo=SWISSQUOTE_TZ).timestamp()
        )
        account_id = r["account_external_id"] or result["account_external_id"]

        conn.execute("BEGIN;")
        try:
            conn.execute(
                "DELETE FROM positions WHERE source = ?;", (source_tag,),
            )
            for pos in result["positions"]:
                # PP doesn't surface the live-XLS-style ticker; the
                # human-readable security name is our best symbol
                # value. wealthdb-side joins should key on ISIN.
                symbol = pos["name"]
                currency = pos["currency"] or ""
                payload = canonical_json(pos)
                conn.execute(
                    "INSERT INTO positions("
                    " snapshot_at, account_external_id, symbol, currency,"
                    " name, isin, payload, source) "
                    "VALUES (?, ?, ?, ?, ?, ?, ?, ?);",
                    (snapshot_at, account_id, symbol, currency,
                     pos["name"], pos["isin"], payload, source_tag),
                )
            conn.execute("COMMIT;")
            loaded += 1
        except Exception:
            conn.execute("ROLLBACK;")
            raise
    return loaded


def load_positions(
    conn: sqlite3.Connection,
    snapshot_at: int,
    customer_id: str,
    xls_path: Path,
    details_path: Path | None = None,
) -> int:
    rows = parse_positions_xls(xls_path)
    details = parse_position_details(details_path) if details_path else {}
    n = 0
    for r in rows:
        symbol = r["symbol"] or ""
        currency = r["currency"] or ""
        if not currency:
            raise SystemExit(
                f"positions row missing CCY: {r}. The XLS schema may "
                f"have shifted; investigate."
            )
        # Look up by (symbol, currency) first; fall back to symbol
        # alone (None-currency entry from position_details.json,
        # which means the href didn't carry a currency suffix).
        match = (
            details.get((symbol, currency))
            or details.get((symbol, None))
            or {}
        )
        name = match.get("name")
        isin = match.get("isin")
        payload = canonical_json(r)
        conn.execute(
            "INSERT INTO positions("
            " snapshot_at, account_external_id, symbol, currency,"
            " name, isin, payload) "
            "VALUES (?, ?, ?, ?, ?, ?, ?);",
            (snapshot_at, customer_id, symbol, currency, name, isin, payload),
        )
        n += 1
    return n


# ============================================================
# List of Assets XLS parser
# ============================================================

# Labelled columns of the List of Assets XLS header. No trailing
# blank observed to date, but the sheet comes from the same export
# engine as positions.xls (whose trailing blank came and went — see
# POSITIONS_EXPECTED_HEADER), so the check tolerates one the same way.
LOA_EXPECTED_HEADER = [
    "Currency", "Rate", "Cash balance", "Positions value",
    "Total value", "Valuation CHF", "Account %",
]


def parse_list_of_assets_xls(path: Path) -> list[dict]:
    import xlrd
    wb = xlrd.open_workbook(str(path))
    sheet = wb.sheet_by_index(0)
    if sheet.nrows == 0:
        raise SystemExit(f"{path}: empty sheet")
    header = _strip_cells(_xls_row(sheet, 0))
    _check_xls_header(header, LOA_EXPECTED_HEADER, path, "list-of-assets")
    out = []
    for r in range(1, sheet.nrows):
        row = _strip_cells(_xls_row(sheet, r))
        ccy = row[0]
        if not isinstance(ccy, str):
            continue
        if not ISO_CCY_RE.match(ccy):
            # Drop "Total CHF" and any other footer/spacer rows.
            log.debug("list_of_assets: dropping non-currency row: %r", row)
            continue
        out.append({
            "currency": ccy,
            "rate_to_chf": row[1],
            "cash_balance": row[2],
            "positions_value": row[3],
            "total_value": row[4],
            "valuation_chf": row[5],
            "account_pct": row[6],
        })
    return out


def load_list_of_assets(
    conn: sqlite3.Connection,
    snapshot_at: int,
    customer_id: str,
    xls_path: Path,
) -> int:
    rows = parse_list_of_assets_xls(xls_path)
    n = 0
    for r in rows:
        conn.execute(
            "INSERT INTO currency_balances("
            " snapshot_at, account_external_id, currency, payload) "
            "VALUES (?, ?, ?, ?);",
            (snapshot_at, customer_id, r["currency"], canonical_json(r)),
        )
        n += 1
    return n


# ============================================================
# Documents indexer
# ============================================================

def index_documents(
    conn: sqlite3.Connection,
    snapshot_at: int,
    customer_id: str | None,
    run_dir: Path,
    bronze_root: Path,
    run_meta: dict,
) -> int:
    """Insert any not-yet-seen PDFs from <run_dir>/documents/ into documents."""
    docs_dir = run_dir / "documents"
    if not docs_dir.is_dir():
        return 0
    # Build a {doc_id -> full metadata} map from run.json, since the
    # filename only encodes the doc_id.
    meta_by_id = {
        d.get("doc_id"): d
        for d in run_meta.get("documents", []) or []
    }
    n = 0
    for pdf in sorted(docs_dir.glob("*.pdf")):
        digest = bronze.sha256_file(pdf)[0]
        already = conn.execute(
            "SELECT 1 FROM documents WHERE content_sha256 = ?;",
            (digest,),
        ).fetchone()
        if already:
            continue
        doc_id = pdf.stem
        meta = meta_by_id.get(doc_id, {})
        doc_type = meta.get("doc_type")
        payload = canonical_json({
            "original_filename": pdf.name,
            "doc_id": doc_id,
            "doc_type": doc_type,
            "contract_no": meta.get("contract_no"),
            "date": meta.get("date"),
            "target_user": meta.get("target_user"),
        })
        conn.execute(
            "INSERT INTO documents("
            " content_sha256, source, swissquote_doc_id,"
            " account_external_id, document_type,"
            " first_seen_at, bronze_path, payload) "
            "VALUES (?, 'auto', ?, ?, ?, ?, ?, ?);",
            (
                digest, doc_id, customer_id, doc_type, snapshot_at,
                str(pdf.relative_to(bronze_root)), payload,
            ),
        )
        n += 1
    return n


def ingest_manual_dir(conn: sqlite3.Connection, bronze_root: Path) -> int:
    """Scan <bronze-root>/manual/ for new files; insert into documents."""
    manual = bronze_root / "manual"
    if not manual.is_dir():
        return 0
    now = int(datetime.now(timezone.utc).timestamp())
    n = 0
    conn.execute("BEGIN;")
    try:
        for f in sorted(manual.rglob("*")):
            if not f.is_file():
                continue
            digest = bronze.sha256_file(f)[0]
            already = conn.execute(
                "SELECT 1 FROM documents WHERE content_sha256 = ?;",
                (digest,),
            ).fetchone()
            if already:
                continue
            payload = canonical_json({
                "original_filename": f.name,
                "relative_path": str(f.relative_to(bronze_root)),
            })
            conn.execute(
                "INSERT INTO documents("
                " content_sha256, source, swissquote_doc_id,"
                " account_external_id, document_type,"
                " first_seen_at, bronze_path, payload) "
                "VALUES (?, 'manual', NULL, NULL, NULL, ?, ?, ?);",
                (digest, now, str(f.relative_to(bronze_root)), payload),
            )
            n += 1
        conn.execute("COMMIT;")
    except Exception:
        conn.execute("ROLLBACK;")
        raise
    return n


# ============================================================
# accounts (content-deduped snapshot)
# ============================================================

def upsert_account(
    conn: sqlite3.Connection,
    snapshot_at: int,
    external_id: str,
    account_product: str,
    extra: dict | None = None,
) -> bool:
    """Insert a new accounts row only if the payload differs from the latest.

    Dedup key is (external_id, account_product) — multi-account
    customers would have multiple rows under the same external_id
    with different products. Returns True if a row was inserted,
    False if it was a no-op.
    """
    payload = canonical_json({
        "external_id": external_id,
        "account_product": account_product,
        **(extra or {}),
    })
    last = conn.execute(
        "SELECT payload FROM accounts "
        "WHERE account_external_id = ? AND account_product = ? "
        "ORDER BY snapshot_at DESC LIMIT 1;",
        (external_id, account_product),
    ).fetchone()
    if last and last["payload"] == payload:
        return False
    conn.execute(
        "INSERT INTO accounts("
        " snapshot_at, account_external_id, account_product, payload) "
        "VALUES (?, ?, ?, ?);",
        (snapshot_at, external_id, account_product, payload),
    )
    return True


# ============================================================
# Per-dump orchestration
# ============================================================

def load_one_dump(
    conn: sqlite3.Connection,
    run_dir: Path,
    bronze_root: Path,
    schema_version: int,
) -> None:
    snapshot_at = _run_dir_to_epoch(run_dir.name)
    log.info("Loading dump %s (snapshot_at=%d)", run_dir.name, snapshot_at)

    run_meta_path = run_dir / "run.json"
    if not run_meta_path.is_file():
        raise SystemExit(
            f"Missing run.json in {run_dir}. The dump is incomplete "
            f"or was produced by a version of download.py that did "
            f"not write metadata."
        )
    run_meta = json.loads(run_meta_path.read_text(encoding="utf-8"))
    customer_id = run_meta.get("customer_id")
    if not customer_id:
        raise SystemExit(
            f"{run_meta_path}: customer_id not set. The dump cannot "
            f"be loaded without knowing which account it belongs to."
        )

    # Account list — either from the accounts.json bronze artefact
    # (current download.py), or synthesised from customer_id for
    # older dumps that predate the scrape. The bronze key was
    # `account_type` before migration 0005's rename; read either.
    accounts_path = run_dir / "accounts.json"
    if accounts_path.is_file():
        account_entries = json.loads(accounts_path.read_text(encoding="utf-8"))
    else:
        log.info(
            "No accounts.json in %s; falling back to a single "
            "product-less account derived from customer_id.",
            run_dir.name,
        )
        account_entries = [
            {"account_external_id": customer_id, "account_product": ""},
        ]

    conn.execute("BEGIN;")
    try:
        # accounts (content-dedup per (external_id, product))
        for entry in account_entries:
            product = (
                entry.get("account_product")
                or entry.get("account_type")  # legacy bronze key
                or ""
            )
            upsert_account(
                conn, snapshot_at,
                entry["account_external_id"], product,
            )

        # currency_balances (list_of_assets.xls)
        loa_path = run_dir / "list_of_assets.xls"
        if loa_path.is_file():
            n = load_list_of_assets(conn, snapshot_at, customer_id, loa_path)
            log.info("  +%d currency_balances", n)
        else:
            log.info("  no list_of_assets.xls")

        # positions (positions.xls + position_details.json sidecar)
        pos_path = run_dir / "positions.xls"
        details_path = run_dir / "position_details.json"
        if pos_path.is_file():
            n = load_positions(
                conn, snapshot_at, customer_id, pos_path,
                details_path if details_path.is_file() else None,
            )
            log.info(
                "  +%d positions (details: %s)",
                n, "present" if details_path.is_file() else "absent",
            )
        else:
            log.info("  no positions.xls")

        # transactions (one CSV per requested window)
        for entry in run_meta.get("transactions", []) or []:
            csv_path = run_dir / entry["file"]
            n = load_transactions_csv(conn, customer_id, csv_path)
            log.info("  +%d transactions for window %s..%s (%s)",
                     n, entry["window_start"], entry["window_end"],
                     entry["file"])

        # documents
        n = index_documents(
            conn, snapshot_at, customer_id, run_dir, bronze_root, run_meta,
        )
        log.info("  +%d documents indexed (auto)", n)

        # dump_runs LAST — its presence is the "fully loaded" marker
        conn.execute(
            "INSERT INTO dump_runs("
            " snapshot_at, silver_schema_version, run_dir) "
            "VALUES (?, ?, ?);",
            (snapshot_at, schema_version, str(run_dir)),
        )

        conn.execute("COMMIT;")
    except Exception:
        conn.execute("ROLLBACK;")
        raise


# ============================================================
# Entry point
# ============================================================

def run(args: argparse.Namespace) -> int:
    if not args.bronze_dir.is_dir():
        raise SystemExit(f"--bronze-dir not found: {args.bronze_dir}")

    here = Path(__file__).resolve().parent
    migrations_dir = here / "migrations"

    if args.force:
        silver.reset(args.silver_db)

    conn = open_db(args.silver_db)
    try:
        silver.apply_migrations(conn, migrations_dir)
        schema_version = silver.current_schema_version(conn)

        pending = find_pending_dumps(conn, args.bronze_dir)
        log.info("%d pending dump(s) to load", len(pending))
        for run_dir in pending:
            load_one_dump(conn, run_dir, args.bronze_dir, schema_version)

        # Manual dir is scanned every invocation, regardless of new dumps.
        n_manual = ingest_manual_dir(conn, args.bronze_dir)
        if n_manual:
            log.info("Indexed %d new manual document(s)", n_manual)

        # Portfolio Performance docs may now be in the documents table
        # but not yet parsed into positions snapshots. Same call is
        # idempotent — re-parses replace under their `pp:<doc_id>` tag.
        n_pp = load_portfolio_performance_docs(conn, args.bronze_dir)
        if n_pp:
            log.info(
                "Reconstructed positions from %d Portfolio Performance "
                "PDF(s)", n_pp,
            )
    finally:
        conn.close()
    return 0


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__.strip())
    p.add_argument("--silver-db", type=Path,
                   default=Path("/data/swissquote.db"),
                   help="Path to the silver SQLite database "
                        "(default: %(default)s, the wrapper's /data mount). "
                        "Created if missing.")
    p.add_argument("--bronze-dir", type=Path, default=Path("/data"),
                   help="Directory containing bronze dump subdirectories and "
                        "the manual/ subdirectory (default: %(default)s).")
    cli.add_standard_args(p, verb="load")
    return p.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(sys.argv[1:] if argv is None else argv)
    cli.configure_logging(args.verbose)
    return run(args)


if __name__ == "__main__":
    sys.exit(main())
