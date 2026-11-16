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
    load.py --silver-db <file> --bronze-dir <dir> [--force] [-v]
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

from collectorkit import bronze, derivations, loader, silver, srcfp

import pp_parser
from pp_parser import parse_portfolio_performance

log = logging.getLogger("swissquote.load")

# All transaction times in Swissquote CSVs are wall-clock Europe/Zurich
# (CET in winter, CEST in summer). The loader converts to UTC epoch
# at insert time; the original string is retained in payload.
SWISSQUOTE_TZ = ZoneInfo("Europe/Zurich")

# CSV transactions: a literal "00000000" Order # is the source's
# placeholder for non-trade rows; the loader normalises it to NULL.
ORDER_NUM_PLACEHOLDER = "00000000"

# A 3-letter uppercase string in the first column of the List of
# Assets XLS denotes a currency row; the last row reads "Total CHF"
# and is dropped.
ISO_CCY_RE = re.compile(r"^[A-Z]{3}$")


# ============================================================
# DB plumbing
# ============================================================

# Silver connections use the manual-transaction model (isolation_level=None;
# explicit BEGIN/COMMIT per dump) with row_factory=Row — exactly what
# collectorkit's silver.open_db provides.
open_db = silver.open_db
canonical_json = silver.canonical_json


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


def _csv_number(s: str) -> float | None:
    """A CSV figure as a float, or None when the cell states none.

    A blank cell, or one without a digit (a placeholder such as "--"),
    is None. A decimal comma reads as a point, as for Net Amount.
    """
    s = s.strip()
    if not any(ch.isdigit() for ch in s):
        return None
    return float(s.replace(",", "."))


def _csv_unit_price(s: str) -> tuple[float | None, str | None]:
    """The "Unit price" cell as (price, price_quote).

    The export marks a price in percent of nominal (a bond trade) with
    a trailing "%". The quote is None when the cell states no price.
    """
    s = s.strip()
    percent = s.endswith("%")
    price = _csv_number(s.removesuffix("%"))
    if price is None:
        return None, None
    return price, "percent" if percent else "unit"


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
        unit_price, price_quote = _csv_unit_price(r["Unit price"])
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
            " order_num, isin, symbol, currency, net_amount,"
            " quantity, unit_price, price_quote, fees, accrued_interest,"
            " payload) "
            "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);",
            (
                customer_id, occurred_at, r["Transaction"].strip(),
                order_num, isin, symbol, currency, net_amount,
                _csv_number(r["Quantity"]), unit_price, price_quote,
                _csv_number(r["Costs"]),
                _csv_number(r["Accrued Interest"]),
                payload,
            ),
        )
        n += 1
    return n


# ============================================================
# Positions XLS parser
# ============================================================

# Labelled columns of the Positions XLS header. The leading "" is the
# unlabelled section/asset-class column and is positional. Some
# exports carry one trailing blank cell after "Positions %" (14
# cells), others none (13 cells), with the labelled columns the same;
# `_check_xls_header` ignores trailing blanks, so both load identically.
POSITIONS_EXPECTED_HEADER = [
    "", "Symbol", "Quantity", "Unit cost", "Total value",
    "Daily change", "Daily chg. %", "Price", "CCY",
    "P&L Nominal CHF", "P&L % CHF", "Total value CHF",
    "Positions %",
]

# Labelled columns some positions exports add after "Positions %". The
# loader reads no field from them, so their presence changes nothing.
POSITIONS_OPTIONAL_TAIL = ["Comment"]


def _xls_row(sheet, r: int) -> list:
    return [sheet.cell_value(r, c) for c in range(sheet.ncols)]


def _strip_cells(row: list) -> list:
    return [c.strip() if isinstance(c, str) else c for c in row]


def _xls_number(v) -> float | None:
    """An XLS cell as a float, or None when the cell holds no number.

    xlrd returns a number cell as float and a blank one as ''.
    """
    if isinstance(v, (int, float)) and not isinstance(v, bool):
        return float(v)
    return None


def _trim_trailing_blanks(cells: list) -> list:
    """Drop trailing empty-string cells (leading/interior ones stay)."""
    end = len(cells)
    while end and cells[end - 1] == "":
        end -= 1
    return cells[:end]


def _check_xls_header(header: list, expected: list, path: Path,
                      kind: str, optional_tail: list = ()) -> None:
    """Fail loud unless the header's labelled columns match `expected`
    exactly, or `expected` followed by `optional_tail`. Trailing blank
    cells are ignored: the XLS export engine emits one on some exports
    and not on others (see POSITIONS_EXPECTED_HEADER), and a cosmetic
    blank must not block the load — but any other change to a labelled
    column (rename, reorder, add, remove) still aborts.
    """
    labelled = _trim_trailing_blanks(header)
    if labelled not in (expected, expected + list(optional_tail)):
        tail = f" (+ optional {list(optional_tail)})" if optional_tail else ""
        raise SystemExit(
            f"Unexpected {kind} XLS header in {path}:\n"
            f"  expected: {expected}{tail} (+ any trailing blank cells)\n"
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
    _check_xls_header(header, POSITIONS_EXPECTED_HEADER, path, "positions",
                      POSITIONS_OPTIONAL_TAIL)

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
# Portfolio Performance positions (parsed by pp_parser.py)
# ============================================================

# The generation of the Portfolio Performance parser: the fingerprint of
# pp_parser.py and the pypdf that extracts the text. A new generation
# parses every statement again at the next load.
PP_SCOPE = "portfolio_performance"
PP_GENERATION = srcfp.parser_fingerprint([pp_parser], ("pypdf",))

_PP_DOCUMENTS_SQL = (
    "SELECT content_sha256, swissquote_doc_id, account_external_id,"
    " bronze_path FROM documents "
    "WHERE document_type = 'Portfolio performance' "
    "  AND swissquote_doc_id IS NOT NULL")


def load_portfolio_performance_docs(
    conn: sqlite3.Connection, bronze_root: Path,
) -> int:
    """Parse every Portfolio Performance doc the current parser
    generation has not read, and write its source-tagged positions rows.

    A doc's rows carry the tag `pp:<doc_id>`. A parse replaces them, a
    DELETE then an INSERT under that tag, and records the doc under the
    generation, in one transaction. A doc whose parse fails keeps its
    rows and is parsed again at the next load.

    Returns the number of newly-loaded docs.
    """
    docs = derivations.pending_derivations(
        conn, PP_SCOPE, PP_GENERATION, _PP_DOCUMENTS_SQL)
    loaded = 0
    for r in docs:
        source_tag = f"pp:{r['swissquote_doc_id']}"
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

        with silver.transaction(conn):
            conn.execute(
                "DELETE FROM positions WHERE source = ?;", (source_tag,),
            )
            for pos in result["positions"]:
                # PP doesn't surface the live-XLS-style ticker; the
                # human-readable security name is the best symbol
                # value. wealthdb-side joins should key on ISIN.
                symbol = pos["name"]
                currency = pos["currency"] or ""
                # price_quote is a column only; the payload keeps
                # the parsed row without it.
                fields = {k: v for k, v in pos.items() if k != "price_quote"}
                # The statement states no P&L, so the CHF P&L columns
                # stay NULL; a bond's accrued interest is its own column.
                conn.execute(
                    "INSERT INTO positions("
                    " snapshot_at, account_external_id, symbol, currency,"
                    " name, isin, average_cost, price_quote,"
                    " accrued_interest_chf, payload, source) "
                    "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);",
                    (snapshot_at, account_id, symbol, currency,
                     pos["name"], pos["isin"], pos["avg_price"],
                     pos["price_quote"], pos.get("accrued_interest_chf"),
                     canonical_json(fields), source_tag),
                )
            derivations.record_derivation(
                conn, r["content_sha256"], PP_SCOPE, PP_GENERATION,
                len(result["positions"]))
        loaded += 1
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
        # The export prints every price per unit; a bond's unit is one
        # unit of nominal.
        conn.execute(
            "INSERT INTO positions("
            " snapshot_at, account_external_id, symbol, currency,"
            " name, isin, average_cost, price_quote,"
            " market_value_chf, unrealized_gain_loss_chf, payload) "
            "VALUES (?, ?, ?, ?, ?, ?, ?, 'unit', ?, ?, ?);",
            (snapshot_at, customer_id, symbol, currency, name, isin,
             _xls_number(r["unit_cost"]), _xls_number(r["total_value_chf"]),
             _xls_number(r["pl_nominal_chf"]), payload),
        )
        n += 1
    return n


# ============================================================
# List of Assets XLS parser
# ============================================================

# Labelled columns of the List of Assets XLS header. The sheet comes
# from the same export engine as positions.xls, whose trailing blank
# cell varies (see POSITIONS_EXPECTED_HEADER), so the check tolerates
# one the same way.
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
    with silver.transaction(conn):
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

def load_run(conn: sqlite3.Connection, run: loader.Run) -> None:
    """Load the dump in `run.dir`, whose parsed run.json is
    `run.manifest`. The frame makes the dump one transaction and writes
    its `dump_runs` row last."""
    # A run dir sits directly under the bronze root.
    run_dir, run_meta, snapshot_at = run.dir, run.manifest, run.snapshot_at
    bronze_root = run_dir.parent
    log.info("Loading dump %s (snapshot_at=%d)", run_dir.name, snapshot_at)

    customer_id = run_meta.get("customer_id")
    if not customer_id:
        raise SystemExit(
            f"{run_dir / 'run.json'}: customer_id not set. The dump cannot "
            f"be loaded without knowing which account it belongs to."
        )

    # Account list — from the accounts.json bronze artefact, or, for a
    # dump without one, a single account synthesised from customer_id.
    # Older dumps name the product `account_type`; read either key.
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


# ============================================================
# Entry point
# ============================================================

def _check_bronze_dir(args: argparse.Namespace) -> None:
    if not args.bronze_dir.is_dir():
        raise SystemExit(f"--bronze-dir not found: {args.bronze_dir}")


def _pending(conn: sqlite3.Connection, bronze_dir: Path, *, log) -> list[Path]:
    """Only complete dumps load: a dir with no run.json, or one still
    carrying the in-progress marker, is a crashed, interrupted or
    still-running download, and its partial artefacts would leak a
    partial snapshot into silver. It is skipped without a dump_runs row,
    so it loads once its walk finalises."""
    pending = bronze.pending_run_dirs(conn, bronze_dir, log=log)
    log.info("%d pending dump(s) to load", len(pending))
    return pending


def _run_columns(run: loader.Run) -> dict:
    """The run's `dump_runs` row; its `run_dir` is the path as given."""
    return {"snapshot_at": run.snapshot_at,
            "silver_schema_version": run.version, "run_dir": str(run.dir)}


def _document_passes(conn: sqlite3.Connection, load: loader.Load) -> None:
    # Manual dir is scanned every invocation, regardless of new dumps.
    n_manual = ingest_manual_dir(conn, load.bronze_dir)
    if n_manual:
        log.info("Indexed %d new manual document(s)", n_manual)

    # Portfolio Performance docs may now be in the documents table
    # but not yet parsed into positions snapshots. Same call is
    # idempotent — re-parses replace under their `pp:<doc_id>` tag.
    n_pp = load_portfolio_performance_docs(conn, load.bronze_dir)
    if n_pp:
        log.info(
            "Reconstructed positions from %d Portfolio Performance "
            "PDF(s)", n_pp,
        )


def _parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(description=__doc__.strip())
    p.add_argument("--silver-db", type=Path,
                   default=Path("/data/swissquote.db"),
                   help="Path to the silver SQLite database "
                        "(default: %(default)s, the wrapper's /data mount). "
                        "Created if missing.")
    p.add_argument("--bronze-dir", type=Path, default=Path("/data"),
                   help="Directory containing bronze dump subdirectories and "
                        "the manual/ subdirectory (default: %(default)s).")
    return p


# A run.json that cannot be read skips its dump with a warning and no
# dump_runs row, so the dumps after it still load and the next load
# retries it. prune keeps such a dir.
SPEC = loader.LoaderSpec(
    name="swissquote", schema=Path(__file__).resolve().parent / "migrations",
    load_run=load_run, parser=_parser, preflight=_check_bronze_dir,
    after_runs=_document_passes, pending=_pending, run_columns=_run_columns,
    open_db=open_db, manifest=loader.MANIFEST_SKIP)


def main(argv: list[str] | None = None) -> int:
    return loader.main(SPEC, argv)


if __name__ == "__main__":
    sys.exit(main())
