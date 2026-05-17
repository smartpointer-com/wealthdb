#!/usr/bin/env python3
"""
Swissquote bronze -> silver loader.

Walks a bronze directory tree (as produced by download.py), applies
any pending schema migrations, then loads each not-yet-loaded dump
into a SQLite silver database. One dump = one transaction; the
`dump_runs` row is the last INSERT before COMMIT, so failures
mid-load roll the whole dump back and re-runs retry idempotently.

Also scans `<bronze-dir>/manual/` for user-uploaded artefacts
(typically e-tax statement PDFs) and indexes new files into the
`documents` table by content sha256.

Usage:
    load.py --silver-db <file> --bronze-dir <dir> [-v]
"""

from __future__ import annotations

import argparse
import csv
import hashlib
import json
import logging
import re
import sqlite3
import sys
from datetime import datetime, timezone
from pathlib import Path
from zoneinfo import ZoneInfo

log = logging.getLogger("swissquote-dump.load")

# All transaction times in Swissquote CSVs are wall-clock Europe/Zurich
# (CET in winter, CEST in summer). The loader converts to UTC epoch
# at insert time; the original string is retained in payload.
SWISSQUOTE_TZ = ZoneInfo("Europe/Zurich")

# Bronze dump-run directory name format. Same as Schwab/UBS for
# uniformity across the toolkit family.
RUN_DIR_RE = re.compile(r"^\d{8}T\d{6}Z$")

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

def open_db(path: Path) -> sqlite3.Connection:
    """Open (or create) the silver DB with sensible defaults."""
    path.parent.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(str(path), isolation_level=None)  # autocommit; we BEGIN/COMMIT explicitly
    conn.row_factory = sqlite3.Row
    # foreign_keys is a per-connection PRAGMA; it must be re-set on
    # every new connection regardless of what's in the schema file.
    conn.execute("PRAGMA foreign_keys = ON;")
    conn.execute("PRAGMA journal_mode = WAL;")
    return conn


def current_schema_version(conn: sqlite3.Connection) -> int:
    """Read MAX(silver_schema_version), or 0 if schema_meta is absent."""
    row = conn.execute(
        "SELECT name FROM sqlite_master "
        "WHERE type='table' AND name='schema_meta'"
    ).fetchone()
    if not row:
        return 0
    row = conn.execute(
        "SELECT COALESCE(MAX(silver_schema_version), 0) AS v FROM schema_meta"
    ).fetchone()
    return int(row["v"])


def apply_migrations(conn: sqlite3.Connection, migrations_dir: Path) -> None:
    """Apply migrations whose number exceeds the current schema version."""
    if not migrations_dir.is_dir():
        raise SystemExit(f"Migrations dir not found: {migrations_dir}")
    files = []
    for f in migrations_dir.iterdir():
        m = re.match(r"^(\d+)_.*\.sql$", f.name)
        if m:
            files.append((int(m.group(1)), f))
    files.sort()
    current = current_schema_version(conn)
    log.info("Schema version on disk: %d; %d migration file(s) found",
             current, len(files))
    for n, path in files:
        if n <= current:
            continue
        log.info("Applying migration %s", path.name)
        sql = path.read_text(encoding="utf-8")
        # In autocommit mode (isolation_level=None), executescript()
        # commits each statement individually. We rely on the final
        # `INSERT INTO schema_meta` statement to mark the migration
        # complete; a crash before that insert leaves the partially-
        # applied schema visible. Recovery: delete the .db, re-run.
        conn.executescript(sql)
    final = current_schema_version(conn)
    log.info("Schema version after migrations: %d", final)


def canonical_json(obj) -> str:
    """Stable JSON for content-based dedup. Sorted keys, no spaces."""
    return json.dumps(obj, sort_keys=True, separators=(",", ":"),
                      ensure_ascii=False)


# ============================================================
# Bronze scanner
# ============================================================

def find_pending_dumps(
    conn: sqlite3.Connection, bronze_dir: Path,
) -> list[Path]:
    """Return run-dirs in bronze that aren't yet recorded in dump_runs."""
    loaded = {
        row["snapshot_at"]
        for row in conn.execute("SELECT snapshot_at FROM dump_runs;")
    }
    out = []
    for child in sorted(bronze_dir.iterdir()):
        if not child.is_dir():
            continue
        if not RUN_DIR_RE.match(child.name):
            continue
        ts = _run_dir_to_epoch(child.name)
        if ts not in loaded:
            out.append(child)
    return out


def _run_dir_to_epoch(name: str) -> int:
    """Convert a YYYYMMDDTHHMMSSZ run-dir name to a UTC epoch second."""
    dt = datetime.strptime(name, "%Y%m%dT%H%M%SZ").replace(tzinfo=timezone.utc)
    return int(dt.timestamp())


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
            d = dict(zip(EXPECTED_CSV_HEADER, raw))
            rows.append(d)
    return rows


def load_transactions_window(
    conn: sqlite3.Connection,
    customer_id: str,
    csv_path: Path,
    window_start_iso: str,
    window_end_iso: str,
) -> int:
    """Window-DELETE-then-INSERT for one transactions CSV.

    The DELETE clears every row whose occurred_at falls inside the
    declared window for this account. The INSERT replays the CSV's
    rows. Any upstream amendment (date shift, amount change, row
    removal) converges to truth on reload.
    """
    # Convert window dates to epoch bounds in Europe/Zurich. The CSV
    # `Date` is also wall-clock CH time, so bounds must be in the
    # same frame to clip rows correctly.
    ws_epoch = int(datetime.fromisoformat(window_start_iso)
                   .replace(tzinfo=SWISSQUOTE_TZ).timestamp())
    we_epoch = int((datetime.fromisoformat(window_end_iso)
                    .replace(hour=23, minute=59, second=59,
                             tzinfo=SWISSQUOTE_TZ)).timestamp())
    conn.execute(
        "DELETE FROM transactions "
        "WHERE account_external_id = ? "
        "  AND occurred_at >= ? AND occurred_at <= ?;",
        (customer_id, ws_epoch, we_epoch),
    )

    rows = parse_transactions_csv(csv_path)
    n = 0
    for r in rows:
        occurred_at = _zurich_to_utc_epoch(r["Date"])
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

POSITIONS_EXPECTED_HEADER = [
    "", "Symbol", "Quantity", "Unit cost", "Total value",
    "Daily change", "Daily chg. %", "Price", "CCY",
    "P&L Nominal CHF", "P&L % CHF", "Total value CHF",
    "Positions %", "",
]


def _xls_row(sheet, r: int) -> list:
    return [sheet.cell_value(r, c) for c in range(sheet.ncols)]


def _strip_cells(row: list) -> list:
    return [c.strip() if isinstance(c, str) else c for c in row]


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
    if header != POSITIONS_EXPECTED_HEADER:
        raise SystemExit(
            f"Unexpected positions XLS header in {path}:\n"
            f"  expected: {POSITIONS_EXPECTED_HEADER}\n"
            f"  actual:   {header}"
        )

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


def load_positions(
    conn: sqlite3.Connection,
    snapshot_at: int,
    customer_id: str,
    xls_path: Path,
) -> int:
    rows = parse_positions_xls(xls_path)
    n = 0
    for r in rows:
        symbol = r["symbol"] or ""
        currency = r["currency"] or ""
        if not currency:
            raise SystemExit(
                f"positions row missing CCY: {r}. The XLS schema may "
                f"have shifted; investigate."
            )
        payload = canonical_json(r)
        conn.execute(
            "INSERT INTO positions("
            " snapshot_at, account_external_id, symbol, currency, payload) "
            "VALUES (?, ?, ?, ?, ?);",
            (snapshot_at, customer_id, symbol, currency, payload),
        )
        n += 1
    return n


# ============================================================
# List of Assets XLS parser
# ============================================================

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
    if header != LOA_EXPECTED_HEADER:
        raise SystemExit(
            f"Unexpected list-of-assets XLS header in {path}:\n"
            f"  expected: {LOA_EXPECTED_HEADER}\n"
            f"  actual:   {header}"
        )
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

def _sha256_file(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


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
        digest = _sha256_file(pdf)
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
            digest = _sha256_file(f)
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
    account_type: str,
    extra: dict | None = None,
) -> bool:
    """Insert a new accounts row only if the payload differs from the latest.

    Dedup key is (external_id, account_type) — multi-account customers
    would have multiple rows under the same external_id but different
    type. Returns True if a row was inserted, False if it was a no-op.
    """
    payload = canonical_json({
        "external_id": external_id,
        "account_type": account_type,
        **(extra or {}),
    })
    last = conn.execute(
        "SELECT payload FROM accounts "
        "WHERE account_external_id = ? AND account_type = ? "
        "ORDER BY snapshot_at DESC LIMIT 1;",
        (external_id, account_type),
    ).fetchone()
    if last and last["payload"] == payload:
        return False
    conn.execute(
        "INSERT INTO accounts("
        " snapshot_at, account_external_id, account_type, payload) "
        "VALUES (?, ?, ?, ?);",
        (snapshot_at, external_id, account_type, payload),
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

    # Account list — either from the new accounts.json bronze artefact
    # (current download.py), or synthesised from customer_id for older
    # dumps that predate the scrape.
    accounts_path = run_dir / "accounts.json"
    if accounts_path.is_file():
        account_entries = json.loads(accounts_path.read_text(encoding="utf-8"))
    else:
        log.info(
            "No accounts.json in %s; falling back to a single typeless "
            "account derived from customer_id.", run_dir.name,
        )
        account_entries = [
            {"account_external_id": customer_id, "account_type": ""},
        ]

    conn.execute("BEGIN;")
    try:
        # accounts (content-dedup per (external_id, type))
        for entry in account_entries:
            upsert_account(
                conn, snapshot_at,
                entry["account_external_id"],
                entry["account_type"],
            )

        # currency_balances (list_of_assets.xls)
        loa_path = run_dir / "list_of_assets.xls"
        if loa_path.is_file():
            n = load_list_of_assets(conn, snapshot_at, customer_id, loa_path)
            log.info("  +%d currency_balances", n)
        else:
            log.info("  no list_of_assets.xls")

        # positions (positions.xls)
        pos_path = run_dir / "positions.xls"
        if pos_path.is_file():
            n = load_positions(conn, snapshot_at, customer_id, pos_path)
            log.info("  +%d positions", n)
        else:
            log.info("  no positions.xls")

        # transactions (one CSV per window)
        for entry in run_meta.get("transactions", []) or []:
            csv_path = run_dir / entry["file"]
            n = load_transactions_window(
                conn, customer_id, csv_path,
                entry["window_start"], entry["window_end"],
            )
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

    conn = open_db(args.silver_db)
    try:
        apply_migrations(conn, migrations_dir)
        schema_version = current_schema_version(conn)

        pending = find_pending_dumps(conn, args.bronze_dir)
        log.info("%d pending dump(s) to load", len(pending))
        for run_dir in pending:
            load_one_dump(conn, run_dir, args.bronze_dir, schema_version)

        # Manual dir is scanned every invocation, regardless of new dumps.
        n_manual = ingest_manual_dir(conn, args.bronze_dir)
        if n_manual:
            log.info("Indexed %d new manual document(s)", n_manual)
    finally:
        conn.close()
    return 0


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__.strip())
    p.add_argument("--silver-db", required=True, type=Path,
                   help="Path to the silver SQLite database. Created if missing.")
    p.add_argument("--bronze-dir", required=True, type=Path,
                   help="Directory containing bronze dump subdirectories "
                        "and the manual/ subdirectory.")
    p.add_argument("-v", "--verbose", action="store_true",
                   help="DEBUG-level logging.")
    return p.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(sys.argv[1:] if argv is None else argv)
    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )
    return run(args)


if __name__ == "__main__":
    sys.exit(main())
