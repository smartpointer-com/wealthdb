#!/usr/bin/env python3
"""
UBS web-scrape silver loader.

Walks the bronze directory laid down by `download.py`, applies any
pending schema migrations, and loads each new dump into the silver
SQLite database defined by `migrations/0001_initial.sql`.

Load semantics
--------------
- Bronze dumps are identified by their `YYYYMMDDTHHMMSSZ` subdir
  names. Each dump is loaded atomically (one transaction); on
  failure the partial dump is rolled back and the loader can retry
  on the next run.
- Already-loaded dumps are skipped via the `dump_runs` table.
- Transactions are UPSERTed on UBS Transaction no.
  (`transaction_external_id`). Re-running a window converges to
  UBS's current view.
- Snapshot tables (banking_relationships, portfolios, accounts,
  positions) take a new row per `snapshot_at` (dedup-by-PK only).
- Documents are indexed by `doc_token` (UBS API token); the binary
  itself stays on disk under bronze.

Usage:
    load.py --silver-db <file> --bronze-dir <dir> [-v]
"""

from __future__ import annotations

import argparse
import csv
import json
import logging
import os
import re
import sqlite3
import sys
from concurrent.futures import ProcessPoolExecutor, as_completed
from datetime import date, datetime, timezone
from pathlib import Path

from collectorkit import bronze, cli, silver

log = logging.getLogger("ubs-web.load")

# UTC timestamp directory pattern from download.py's ts_slug().
DUMP_DIR_RE = re.compile(r"^\d{8}T\d{6}Z$")

# Migration file pattern: NNNN_<slug>.sql, sorted numerically.
MIGRATION_FILE_RE = re.compile(r"^(\d+)_[a-z0-9_-]+\.sql$", re.IGNORECASE)

# UBS positions.csv columns we care about (semicolon-delimited,
# UTF-8 BOM, CRLF). Header row defines them in the order below.
POSITIONS_COLS = [
    "Banking relationship", "Portfolio", "Group of products",
    "Product", "Ccy.", "Number/Amt.", "Intraday number/amount",
    "Valor", "ISIN", "Cost price", "Buy exchange rate",
    "Sector", "Rating", "Description", "Description 1",
    "Description 2", "Description 3", "Description 4",
    "Description 5", "Lending value", "Intraday lending value",
    "Lending value ratio", "Intraday lending value ratio",
    "IBAN", "Category", "Interest", "Date", "Duration",
    "Market value", "% of market value", "Accrued interest",
    "% of accrued interest",
]

# Transactions CSV header (UTF-8 BOM, semicolon, CRLF). Header row
# is the 9th line; first 8 are metadata.
TXN_DATA_HEADER = (
    "Trade date;Trade time;Booking date;Value date;Currency;"
    "Debit;Credit;Individual amount;Balance;Transaction no.;"
    "Description1;Description2;Description3;Footnotes;"
)
TXN_HEADER_FROM = "From:"
TXN_HEADER_IBAN = "IBAN:"


# ============================================================
# CLI
# ============================================================

def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__.strip())
    p.add_argument("--silver-db", type=Path,
                   default=Path("/data/ubs-web.db"),
                   help="Path to the silver SQLite database "
                        "(default: %(default)s, the wrapper's /data mount). "
                        "Created if missing.")
    p.add_argument("--bronze-dir", type=Path, default=Path("/data"),
                   help="Directory containing UTC-timestamped bronze dump dirs "
                        "(default: %(default)s).")
    p.add_argument("-v", "--verbose", action="store_true",
                   help="DEBUG-level logging.")
    cli.add_force_arg(p)
    return p.parse_args(argv)


# ============================================================
# Migrations
# ============================================================

# Schema versioning + the migration runner now live in
# collectorkit.silver (transaction-model agnostic). The silver
# connection is created inline in main() with default isolation.


# ============================================================
# Bronze scan
# ============================================================

def scan_bronze(bronze_dir: Path) -> list[Path]:
    """Return bronze dump subdirs in chronological order."""
    if not bronze_dir.is_dir():
        raise SystemExit(f"--bronze-dir does not exist: {bronze_dir}")
    return sorted(
        (p for p in bronze_dir.iterdir()
         if p.is_dir() and DUMP_DIR_RE.match(p.name)),
        key=lambda p: p.name,
    )


def already_loaded(conn: sqlite3.Connection, dump_dir: Path) -> bool:
    snapshot_at = ts_from_dir(dump_dir.name)
    cur = conn.execute(
        "SELECT 1 FROM dump_runs WHERE snapshot_at = ?", (snapshot_at,),
    )
    return cur.fetchone() is not None


# ============================================================
# Helpers
# ============================================================

def ts_from_dir(name: str) -> int:
    """Parse 'YYYYMMDDTHHMMSSZ' -> Unix seconds UTC."""
    return bronze.parse_run_ts(name)


def ts_from_iso(s: str | None) -> int | None:
    """Parse 'YYYY-MM-DD' (or full ISO timestamp) -> Unix seconds UTC."""
    if not s:
        return None
    # Accept both 'YYYY-MM-DD' and 'YYYYMMDDTHHMMSSZ' forms.
    try:
        if re.match(r"^\d{4}-\d{2}-\d{2}$", s):
            dt = datetime.strptime(s, "%Y-%m-%d")
        else:
            dt = datetime.fromisoformat(s.replace("Z", "+00:00"))
    except ValueError:
        return None
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    return int(dt.timestamp())


def ts_from_dmy(s: str | None) -> int | None:
    """Parse 'DD.MM.YYYY' -> Unix seconds UTC, midnight."""
    if not s:
        return None
    try:
        dt = datetime.strptime(s, "%d.%m.%Y").replace(tzinfo=timezone.utc)
    except ValueError:
        return None
    return int(dt.timestamp())


def parse_decimal(s: str | None) -> float | None:
    """Parse a UBS decimal cell. Web CSVs use '.' as decimal sep;
    return None for empty / whitespace cells."""
    if s is None:
        return None
    s = s.strip()
    if not s:
        return None
    try:
        return float(s)
    except ValueError:
        return None


CH_IBAN_CANONICAL_RE = re.compile(r"^CH\d{2}[A-Z0-9]{17}$")


def iban_canonical(iban: str | None) -> str | None:
    """Strip whitespace, uppercase, validate the CH-IBAN shape.

    Returns the canonical string on success, None if the input
    doesn't pass the basic shape check. UBS occasionally re-uses
    the IBAN column for non-IBAN fields on mortgages and structured
    products (e.g. it puts the maturity date / loan term there); we
    reject those rather than letting them into the accounts table.
    """
    if not iban:
        return None
    c = re.sub(r"\s+", "", iban).upper()
    if not CH_IBAN_CANONICAL_RE.match(c):
        return None
    return c


def iban_to_psn_acct_id(iban_c: str | None) -> str | None:
    """Compute the 21-char PSN AcctId form from a canonical CH IBAN.

    Mapping (UBS Switzerland personal accounts):
        CH<2chk><bank:4><branch:4><base:8><chk:1>     (21 chars)
        -> <branch:4>0000 00<base:8>0000<chk:1>        (21 chars)
    """
    if not iban_c or len(iban_c) != 21 or not iban_c.startswith("CH"):
        return None
    branch = iban_c[8:12]
    base = iban_c[12:20]
    chk = iban_c[20]
    return branch + "0000" + "00" + base + "0000" + chk


def relationship_prefix_from_iban(iban_c: str | None) -> str | None:
    """Extract the banking-relationship account-number prefix
    from a canonical IBAN. For a canonical IBAN of the shape
    'CHKKBBBBRRRRAAAAAAAAC' the prefix is 'RRRR AAAAAAAA' (branch +
    8-char account base). Used as a proxy join key when the opaque
    bankingRelationId tokens differ across sessions."""
    if not iban_c or len(iban_c) != 21:
        return None
    branch = iban_c[8:12]
    base8 = iban_c[12:20]
    return f"{branch} {base8}"


def portfolio_external_id_from_full(full: str | None) -> str | None:
    """Extract the trailing portfolio code from
    `<prefix> <portfolio_code>` (e.g. 'BBBB AAAAAAAA RNNN' -> 'RNNN')."""
    if not full:
        return None
    parts = full.split()
    return parts[-1] if parts else None


def normalize_payload(data: dict) -> str:
    """Canonical JSON for embedding in payload columns."""
    return json.dumps(data, ensure_ascii=False, sort_keys=True, default=str)


# ============================================================
# Loaders
# ============================================================

def load_dump(conn: sqlite3.Connection, dump_dir: Path,
              schema_version: int) -> None:
    """Load one bronze dump into silver. Single transaction."""
    snapshot_at = ts_from_dir(dump_dir.name)
    log.info("loading %s (snapshot_at=%d)", dump_dir.name, snapshot_at)

    run_meta = _read_run_json(dump_dir)

    _insert_dump_run(conn, snapshot_at, schema_version, dump_dir, run_meta)

    pos_count = _load_positions(conn, snapshot_at, dump_dir)
    txn_count = _load_transactions(conn, snapshot_at, dump_dir)
    doc_count = _load_documents(conn, snapshot_at, dump_dir, run_meta)
    hist_pos, hist_cash, hist_mort = _load_historical_from_pdfs(
        conn, dump_dir)

    log.info("loaded %s: positions=%d transactions=%d documents=%d "
             "hist_positions=%d hist_cash_balances=%d "
             "hist_mortgages=%d",
             dump_dir.name, pos_count, txn_count, doc_count,
             hist_pos, hist_cash, hist_mort)


def _read_run_json(dump_dir: Path) -> dict:
    path = dump_dir / "run.json"
    if not path.exists():
        return {}
    return json.loads(path.read_text(encoding="utf-8"))


def _insert_dump_run(conn: sqlite3.Connection, snapshot_at: int,
                     schema_version: int, dump_dir: Path,
                     run_meta: dict) -> None:
    txn = run_meta.get("transactions") or {}
    docs = run_meta.get("documents") or {}
    conn.execute(
        "INSERT INTO dump_runs ("
        "snapshot_at, silver_schema_version, run_dir, "
        "transactions_since, transactions_until, "
        "documents_since, documents_until"
        ") VALUES (?, ?, ?, ?, ?, ?, ?)",
        (
            snapshot_at, schema_version, str(dump_dir),
            ts_from_iso(txn.get("since")),
            ts_from_iso(txn.get("until")),
            ts_from_iso(docs.get("since")),
            ts_from_iso(docs.get("until")),
        ),
    )


# ----------------------------------------------------------------
# Positions
# ----------------------------------------------------------------

def _load_positions(conn: sqlite3.Connection, snapshot_at: int,
                    dump_dir: Path) -> int:
    """Parse every `positions/*.csv` in the dump dir; upsert into
    banking_relationships, portfolios, accounts, positions.

    Each per-portfolio CSV is parsed independently. The portfolio
    base currency for the `market_value` column is extracted from
    the CSV's "Valued in: …" footer line.
    """
    pos_dir = dump_dir / "positions"
    if not pos_dir.is_dir():
        return 0
    inserted = 0
    seen_accounts: set[str] = set()
    seen_portfolios: set[str] = set()
    seen_relationships: set[str] = set()
    seen_positions: set[tuple[str, str | None]] = set()
    seen_mortgages: set[str] = set()
    # Process per-portfolio CSVs first (`positions_<sha>.csv`), then
    # the consolidated default view (`positions.csv`). The default
    # view files unassigned accounts under a synthetic catch-all
    # portfolio code; loading it last means the `INSERT OR IGNORE`
    # on accounts keeps the real per-portfolio mapping where one
    # exists, and only the truly-unassigned accounts inherit the
    # catch-all. For positions we additionally track (account, isin)
    # pairs already inserted from a per-portfolio CSV and skip the
    # consolidated row for them, otherwise every holding ends up
    # double-counted (once under its real portfolio, once under the
    # catch-all).
    per_portfolio_csvs = sorted(pos_dir.glob("positions_*.csv"))
    consolidated_csvs = sorted(pos_dir.glob("positions.csv"))
    for csv_path in per_portfolio_csvs + consolidated_csvs:
        base_ccy = _read_positions_base_currency(csv_path)
        for row in _iter_positions_rows(csv_path):
            inserted += _ingest_positions_row(
                conn, snapshot_at, row, base_ccy,
                seen_accounts, seen_portfolios, seen_relationships,
                seen_positions, seen_mortgages,
            )
    return inserted


VALUED_IN_RE = re.compile(r"^Valued in:\s*([A-Z]{3})\b", re.MULTILINE)


def _read_positions_base_currency(csv_path: Path) -> str | None:
    """Extract the 'Valued in: <CCY>' footer line; default None."""
    text = csv_path.read_text(encoding="utf-8-sig", errors="replace")
    m = VALUED_IN_RE.search(text)
    return m.group(1) if m else None


RELATIONSHIP_PREFIX_RE = re.compile(r"^\d{4}\s+\d{8}$")


def _iter_positions_rows(csv_path: Path) -> "iter[dict]":
    """Yield meaningful holding rows from a positions.csv.

    Skips:
      - the header row itself
      - footer rows ("Export created on: …", "Number of positions: …")
      - blank rows
      - currency-conversion appendix rows whose column 0 is e.g.
        "EUR/CHF" instead of a "<branch> <base>" prefix.
    """
    with csv_path.open("r", encoding="utf-8-sig", newline="") as f:
        reader = csv.reader(f, delimiter=";")
        try:
            header = next(reader)
        except StopIteration:
            return
        # Header row sanity-check: must contain "Banking relationship".
        if not header or header[0].strip() != POSITIONS_COLS[0]:
            log.warning("unexpected positions.csv header in %s: %r",
                        csv_path.name, header[:3])
            return
        # Map header names to indices defensively (UBS may re-order).
        idx = {name: i for i, name in enumerate(header) if name}
        for raw in reader:
            if not raw or all(not c.strip() for c in raw):
                continue
            relationship = _cell(raw, idx, "Banking relationship")
            # Require the canonical "<4-digit branch> <8-digit base>"
            # form. Filters out:
            #   - real footer rows (no value)
            #   - currency-pair appendix rows (e.g. 'EUR/CHF')
            #   - "Portfolio number: …", "Valued in: …", etc. (text)
            if not RELATIONSHIP_PREFIX_RE.match(relationship):
                continue
            row = {col: _cell(raw, idx, col) for col in POSITIONS_COLS}
            yield row


def _cell(raw: list[str], idx: dict[str, int], name: str) -> str:
    """Defensive column read: returns '' if column index is out of range."""
    i = idx.get(name)
    if i is None or i >= len(raw):
        return ""
    return raw[i].strip()


def _ingest_positions_row(conn: sqlite3.Connection, snapshot_at: int,
                          row: dict, base_currency: str | None,
                          seen_accounts: set[str],
                          seen_portfolios: set[str],
                          seen_relationships: set[str],
                          seen_positions: set[tuple[str, str | None]],
                          seen_mortgages: set[str]) -> int:
    relationship_prefix = row["Banking relationship"] or None
    portfolio_full = row["Portfolio"] or None
    portfolio_ext_id = portfolio_external_id_from_full(portfolio_full)

    # Mortgage rows ('Pro memoria - Mortgages' group) have the
    # IBAN column re-used for the fixed-rate term and the Product
    # column holds the UBS-internal mortgage account number — so
    # they need their own ingest path before the IBAN-based
    # account/position inserts below.
    if (row.get("Group of products") or "").strip() == \
            "Pro memoria - Mortgages":
        return _ingest_mortgage_row(
            conn, snapshot_at, row,
            relationship_prefix, portfolio_ext_id,
            seen_mortgages,
        )

    iban_raw = row["IBAN"] or None
    iban_c = iban_canonical(iban_raw)
    isin = row["ISIN"] or None

    # banking_relationships (one synthetic row per relationship-prefix).
    # We don't see the opaque bankingRelationId token in positions.csv,
    # so the prefix doubles as the id here.
    if relationship_prefix and relationship_prefix not in seen_relationships:
        seen_relationships.add(relationship_prefix)
        conn.execute(
            "INSERT OR IGNORE INTO banking_relationships ("
            "snapshot_at, banking_relationship_id, account_number_prefix, "
            "description, payload"
            ") VALUES (?, ?, ?, ?, ?)",
            (snapshot_at, relationship_prefix, relationship_prefix, None,
             normalize_payload({"source": "positions.csv"})),
        )

    # portfolios
    if portfolio_ext_id and portfolio_ext_id not in seen_portfolios:
        seen_portfolios.add(portfolio_ext_id)
        conn.execute(
            "INSERT OR IGNORE INTO portfolios ("
            "snapshot_at, portfolio_external_id, banking_relationship_id, "
            "portfolio_full_id, portfolio_uid, base_currency, "
            "description, payload"
            ") VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
            (snapshot_at, portfolio_ext_id, relationship_prefix,
             portfolio_full, None, base_currency, None,
             normalize_payload({"source": "positions.csv"})),
        )

    # accounts (cash only — securities-only rows have no IBAN)
    if iban_c and iban_c not in seen_accounts:
        seen_accounts.add(iban_c)
        conn.execute(
            "INSERT OR IGNORE INTO accounts ("
            "snapshot_at, account_external_id, kind, iban, "
            "account_acct_id_psn_form, account_number_raw, "
            "account_opaque_id, banking_relationship_id, "
            "portfolio_external_id, currency_iso, description, payload"
            ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
            (snapshot_at, iban_c, "cash", iban_raw,
             iban_to_psn_acct_id(iban_c),
             row["Product"] or None, None,
             relationship_prefix, portfolio_ext_id,
             row["Ccy."] or None,
             row["Description"] or None,
             normalize_payload({"source": "positions.csv"})),
        )

    # positions
    # Resolve account_external_id: IBAN for cash, '' for securities-only
    # rows (positions table PK includes instrument_isin to disambiguate).
    account_ext = iban_c or ""
    if not isin and not iban_c:
        # Pure aggregate / informational row — skip.
        return 0
    pos_key = (account_ext, isin)
    if pos_key in seen_positions:
        # Already loaded from an earlier (per-portfolio) CSV in this
        # dump. The consolidated view would attach it to the synthetic
        # catch-all portfolio; that's a duplicate, so skip.
        return 0
    seen_positions.add(pos_key)
    conn.execute(
        "INSERT OR REPLACE INTO positions ("
        "snapshot_at, portfolio_external_id, account_external_id, "
        "instrument_isin, valor, currency_iso, units, market_value, "
        "market_value_currency, cost_price, accrued_interest, "
        "lending_value, lending_value_ratio, description, payload"
        ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
        (
            snapshot_at, portfolio_ext_id or "", account_ext,
            isin, row["Valor"] or None, row["Ccy."] or None,
            parse_decimal(row["Number/Amt."]),
            parse_decimal(row["Market value"]),  # base-ccy market value
            base_currency,
            parse_decimal(row["Cost price"]),
            parse_decimal(row["Accrued interest"]),
            parse_decimal(row["Lending value"]),
            parse_decimal(row["Lending value ratio"]),
            row["Description"] or None,
            normalize_payload({k: row[k] for k in POSITIONS_COLS}),
        ),
    )
    return 1


MORTGAGE_TERM_RE = re.compile(
    r"^\s*(\d{2})\.(\d{2})\.(\d{4})\s*-\s*(\d{2})\.(\d{2})\.(\d{4})\s*$"
)


def _parse_mortgage_term(s: str | None) -> tuple[int | None, int | None]:
    """Extract (start_ts, end_ts) from the 'IBAN' column when it
    actually carries a 'dd.mm.yyyy - dd.mm.yyyy' fixed-rate term.
    Returns (None, None) on anything that doesn't match exactly.
    """
    if not s:
        return None, None
    m = MORTGAGE_TERM_RE.match(s)
    if not m:
        return None, None
    sd, sm, sy, ed, em, ey = m.groups()
    try:
        start = datetime(int(sy), int(sm), int(sd), tzinfo=timezone.utc)
        end = datetime(int(ey), int(em), int(ed), tzinfo=timezone.utc)
    except ValueError:
        return None, None
    return int(start.timestamp()), int(end.timestamp())


def _mortgage_rate_type(description_1: str | None) -> str | None:
    """Map the Description 1 product name to 'fixed' / 'variable'.
    Returns None when neither cue is present so the row stays
    honest about an unknown rate basis."""
    if not description_1:
        return None
    low = description_1.lower()
    if "fixed-rate" in low or "fixed rate" in low or "festhypothek" in low:
        return "fixed"
    if "variable" in low or "variabel" in low or "variabler" in low:
        return "variable"
    return None


def _ingest_mortgage_row(conn: sqlite3.Connection, snapshot_at: int,
                         row: dict,
                         relationship_prefix: str | None,
                         portfolio_ext_id: str | None,
                         seen_mortgages: set[str]) -> int:
    """Insert a single 'Pro memoria - Mortgages' row into the
    `mortgages` silver table. The CSV uses the IBAN / Category /
    Date columns as overflow slots for mortgage-specific data —
    we promote the parts we know about and stash the rest in
    `payload` for forensics. See migration 0004."""
    account_ext = (row.get("Product") or "").strip()
    if not account_ext:
        return 0
    if account_ext in seen_mortgages:
        return 0
    seen_mortgages.add(account_ext)

    start_ts, end_ts = _parse_mortgage_term(row.get("IBAN"))
    rate_type = _mortgage_rate_type(row.get("Description 1"))
    collateral = row.get("Description 3") or row.get("Sector") or None
    description = row.get("Description") or row.get("Description 1") or None
    currency = (row.get("Ccy.") or "").strip() or None
    if currency is None:
        # `Ccy.` should always be set on UBS mortgages; if it
        # isn't, we'd rather drop than insert a NULL into a NOT
        # NULL column.
        log.warning("mortgage row %r has no currency; skipping",
                    account_ext)
        return 0

    conn.execute(
        "INSERT OR REPLACE INTO mortgages ("
        "snapshot_at, account_external_id, banking_relationship_id, "
        "portfolio_external_id, currency_iso, outstanding_balance, "
        "start_date, end_date, rate_type, collateral_description, "
        "description, payload"
        ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
        (
            snapshot_at, account_ext, relationship_prefix,
            portfolio_ext_id, currency,
            parse_decimal(row.get("Number/Amt.")),
            start_ts, end_ts, rate_type, collateral, description,
            normalize_payload({k: row[k] for k in POSITIONS_COLS}),
        ),
    )
    return 1


# ----------------------------------------------------------------
# Transactions
# ----------------------------------------------------------------

def _load_transactions(conn: sqlite3.Connection, snapshot_at: int,
                       dump_dir: Path) -> int:
    """Parse every `transactions/cash_*.csv` in the dump dir;
    UPSERT into transactions keyed by UBS Transaction no."""
    txn_dir = dump_dir / "transactions"
    if not txn_dir.is_dir():
        return 0
    inserted = 0
    for csv_path in sorted(txn_dir.glob("cash_*.csv")):
        inserted += _ingest_transactions_csv(conn, snapshot_at, csv_path)
    return inserted


def _ingest_transactions_csv(conn: sqlite3.Connection, snapshot_at: int,
                             csv_path: Path) -> int:
    """Parse the 8-line metadata block + 1-line column header + data."""
    with csv_path.open("r", encoding="utf-8-sig", newline="") as f:
        lines = f.read().splitlines()
    # Locate the data header by literal match (UBS may add fields).
    header_idx = None
    iban = None
    for i, ln in enumerate(lines[:15]):
        if ln.startswith(TXN_HEADER_IBAN):
            iban = ln.split(";", 1)[1].rstrip(";").strip() or None
        if ln.startswith("Trade date;"):
            header_idx = i
            break
    if header_idx is None:
        log.warning("no transactions header in %s; skipping", csv_path.name)
        return 0
    account_ext = iban_canonical(iban) or ""
    if not account_ext:
        log.warning("no IBAN in %s; skipping", csv_path.name)
        return 0
    header = [c.strip() for c in lines[header_idx].split(";")]
    idx = {name: i for i, name in enumerate(header) if name}
    inserted = 0
    for raw_line in lines[header_idx + 1:]:
        if not raw_line.strip():
            continue
        raw = next(csv.reader([raw_line], delimiter=";"))
        # The "Turnover total" footer block carries blank columns 1-9
        # and "Turnover total" in description1 — skip.
        trade_date = _cell(raw, idx, "Trade date")
        txn_no = _cell(raw, idx, "Transaction no.")
        if not txn_no:
            continue
        value_date_s = _cell(raw, idx, "Value date") or trade_date
        value_date_ts = ts_from_iso(value_date_s)
        if not value_date_ts:
            log.warning("unparseable value_date %r in %s; skipping row",
                        value_date_s, csv_path.name)
            continue
        conn.execute(
            "INSERT INTO transactions ("
            "transaction_external_id, account_external_id, snapshot_at, "
            "trade_date, booking_date, value_date, currency_iso, "
            "amount_debit, amount_credit, counterparty, "
            "description_kind, payload"
            ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) "
            "ON CONFLICT(transaction_external_id, account_external_id) "
            "DO UPDATE SET "
            "snapshot_at = excluded.snapshot_at, "
            "trade_date = excluded.trade_date, "
            "booking_date = excluded.booking_date, "
            "value_date = excluded.value_date, "
            "currency_iso = excluded.currency_iso, "
            "amount_debit = excluded.amount_debit, "
            "amount_credit = excluded.amount_credit, "
            "counterparty = excluded.counterparty, "
            "description_kind = excluded.description_kind, "
            "payload = excluded.payload",
            (
                txn_no, account_ext, snapshot_at,
                ts_from_iso(trade_date),
                ts_from_iso(_cell(raw, idx, "Booking date")),
                value_date_ts,
                _cell(raw, idx, "Currency"),
                parse_decimal(_cell(raw, idx, "Debit")),
                parse_decimal(_cell(raw, idx, "Credit")),
                # description1's first semi-line is usually the counterparty
                (_cell(raw, idx, "Description1").split(";", 1)[0] or None),
                _cell(raw, idx, "Description2") or None,
                normalize_payload({h: c for h, c in zip(header, raw)}),
            ),
        )
        inserted += 1
    return inserted


# ----------------------------------------------------------------
# Documents
# ----------------------------------------------------------------

# Extract the doc type from a listing-row label of the form
# "<doctype> <DD.MM.YYYY> <DD Month YYYY> P. <name> <...>".
DOC_LABEL_RE = re.compile(
    r"^\s*\W*\s*(?P<type>[A-Za-z][A-Za-z _]+?)\s+"
    r"(?P<date>\d{2}\.\d{2}\.\d{4})\b"
)


def _load_documents(conn: sqlite3.Connection, snapshot_at: int,
                    dump_dir: Path, run_meta: dict) -> int:
    """Index every PDF in dump_dir/documents/ into the documents
    table, with metadata harvested from run.json where present.

    `doc_token` is UBS's full URL-resolvable token (taken from
    run.json), not the truncated filename prefix.
    """
    docs_dir = dump_dir / "documents"
    if not docs_dir.is_dir():
        return 0
    # Index run.json items by their on-disk filename — that is the
    # only stable key between the manifest and the files on disk.
    items_by_filename: dict[str, dict] = {}
    for item in (run_meta.get("documents") or {}).get("items") or []:
        fname = item.get("filename")
        if fname:
            items_by_filename[fname] = item
    inserted = 0
    for pdf in sorted(docs_dir.glob("*.pdf")):
        item = items_by_filename.get(pdf.name, {})
        token = item.get("token") or pdf.stem  # fallback to stem
        label = item.get("label") or ""
        doc_type, doc_date = _parse_doc_label(label)
        sha = bronze.sha256_file(pdf)[0]
        try:
            conn.execute(
                "INSERT INTO documents ("
                "doc_token, content_sha256, file_path, size_bytes, "
                "snapshot_at, doc_type, doc_date, account_external_id, "
                "portfolio_external_id, label"
                ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                (
                    token, sha, str(pdf.resolve()), pdf.stat().st_size,
                    snapshot_at, doc_type, doc_date, None, None,
                    label,
                ),
            )
            inserted += 1
        except sqlite3.IntegrityError:
            # Same doc_token or sha already loaded in a previous dump.
            log.debug("doc %s already in silver; skipping", token[:12])
    return inserted


# ----------------------------------------------------------------
# Historical snapshots from PDF documents
# ----------------------------------------------------------------

def _parse_one_pdf(args: tuple[str, str, str]
                   ) -> tuple[str, str, str, list[dict] | None, str | None]:
    """Worker-side: parse one PDF and return its rows. Pure (no DB
    access) so it can run in a ProcessPoolExecutor worker. Returns
    (token, kind, file_name, rows, error_message); exactly one of
    `rows` or `error_message` is set on every non-skipped call."""
    from pdf_parsers import (
        parse_statement_of_assets, parse_account_statement,
        parse_maturity_notice,
    )
    token, fp, label, doc_type = args
    path = Path(fp)
    if not path.is_file():
        return token, "skip", path.name, None, None
    try:
        if "Statement of assets" in (label or ""):
            rows = parse_statement_of_assets(path, token, label)
            return token, "positions", path.name, rows, None
        if doc_type == "Maturity notice":
            rows = parse_maturity_notice(path, token, label)
            return token, "mortgage", path.name, rows, None
        rows = parse_account_statement(path, token, label)
        return token, "cash", path.name, rows, None
    except Exception as e:  # noqa: BLE001
        return token, "error", path.name, None, f"{type(e).__name__}: {e}"


def _load_historical_from_pdfs(conn: sqlite3.Connection,
                               dump_dir: Path) -> tuple[int, int, int]:
    """Walk every PDF tracked in the documents table whose label
    indicates a Statement of assets, an Account Statement, or a
    Maturity notice; parse it in a worker-pool of subprocesses,
    and upsert into the historical_* tables on the main thread.
    Returns (position_rows, cash_rows, mortgage_rows).

    pdfplumber / pdfminer text extraction is CPU-bound and largely
    GIL-bound, so the speedup comes from real OS processes, not
    threads. SQLite writes stay on the main connection."""
    docs_dir = dump_dir / "documents"
    if not docs_dir.is_dir():
        return 0, 0, 0

    # Pull (doc_token, file_path, label, doc_type) for relevant docs
    # from the documents table — that's where bronze metadata lives.
    cur = conn.execute(
        "SELECT doc_token, file_path, label, doc_type FROM documents "
        "WHERE label LIKE '%Statement of assets%' "
        "   OR doc_type = 'Account Statement' "
        "   OR doc_type = 'Maturity notice'"
    )
    work = cur.fetchall()
    if not work:
        return 0, 0, 0

    pos_rows = 0
    cash_rows = 0
    mortgage_rows = 0
    n_workers = max(1, os.cpu_count() or 1)
    log.info("parsing %d PDFs across %d workers", len(work), n_workers)
    with ProcessPoolExecutor(max_workers=n_workers) as ex:
        futures = [ex.submit(_parse_one_pdf, w) for w in work]
        for fut in as_completed(futures):
            _, kind, name, rows, err = fut.result()
            if err is not None:
                log.warning("PDF parse failed for %s: %s", name, err)
                continue
            if kind == "positions":
                pos_rows += _insert_hist_positions(conn, rows or [])
            elif kind == "cash":
                cash_rows += _insert_hist_cash_balances(conn, rows or [])
            elif kind == "mortgage":
                mortgage_rows += _insert_hist_mortgages(conn, rows or [])
    return pos_rows, cash_rows, mortgage_rows


def _insert_hist_positions(conn: sqlite3.Connection,
                           rows: list[dict]) -> int:
    n = 0
    for r in rows:
        try:
            conn.execute(
                "INSERT OR REPLACE INTO historical_position_snapshots ("
                "as_of_date, portfolio_external_id, account_external_id, "
                "instrument_isin, currency_iso, units, market_value, "
                "market_value_currency, cost_price, market_price, "
                "accrued_interest, exchange_rate_to_base, description, "
                "sector, source_doc_token, payload"
                ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                (
                    r["as_of_date"], r["portfolio_external_id"],
                    r["account_external_id"], r["instrument_isin"],
                    r["currency_iso"], r["units"], r["market_value"],
                    r["market_value_currency"], r["cost_price"],
                    r["market_price"], r["accrued_interest"],
                    r["exchange_rate_to_base"], r["description"],
                    r["sector"], r["source_doc_token"], r["payload"],
                ),
            )
            n += 1
        except sqlite3.IntegrityError as e:
            log.debug("hist position insert failed: %s", e)
    return n


def _insert_hist_cash_balances(conn: sqlite3.Connection,
                               rows: list[dict]) -> int:
    n = 0
    for r in rows:
        try:
            conn.execute(
                "INSERT OR REPLACE INTO historical_cash_balances ("
                "period_end, account_external_id, currency_iso, "
                "period_start, opening_balance, closing_balance, "
                "total_debits, total_credits, source_doc_token, payload"
                ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                (
                    r["period_end"], r["account_external_id"],
                    r["currency_iso"], r["period_start"],
                    r["opening_balance"], r["closing_balance"],
                    r["total_debits"], r["total_credits"],
                    r["source_doc_token"], r["payload"],
                ),
            )
            n += 1
        except sqlite3.IntegrityError as e:
            log.debug("hist cash insert failed: %s", e)
    return n


def _insert_hist_mortgages(conn: sqlite3.Connection,
                           rows: list[dict]) -> int:
    n = 0
    for r in rows:
        try:
            conn.execute(
                "INSERT OR REPLACE INTO historical_mortgages ("
                "as_of_date, account_external_id, currency_iso, "
                "outstanding_balance, product_name, rate_type, "
                "collateral_description, source_doc_token, payload"
                ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
                (
                    r["as_of_date"], r["account_external_id"],
                    r["currency_iso"], r["outstanding_balance"],
                    r["product_name"], r["rate_type"],
                    r["collateral_description"], r["source_doc_token"],
                    r["payload"],
                ),
            )
            n += 1
        except sqlite3.IntegrityError as e:
            log.debug("hist mortgage insert failed: %s", e)
    return n


def _parse_doc_label(label: str) -> tuple[str | None, int | None]:
    if not label:
        return None, None
    line = label.splitlines()[0]
    m = DOC_LABEL_RE.search(line)
    if not m:
        return None, None
    return m.group("type").strip(), ts_from_dmy(m.group("date"))


# ============================================================
# Main
# ============================================================

def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)

    args.silver_db.parent.mkdir(parents=True, exist_ok=True)
    if args.force:
        silver.reset(args.silver_db)
    conn = sqlite3.connect(str(args.silver_db))
    conn.execute("PRAGMA foreign_keys = ON;")

    migrations_dir = Path(__file__).parent / "migrations"
    silver.apply_migrations(conn, migrations_dir)
    schema_version = silver.current_schema_version(conn)

    dumps = scan_bronze(args.bronze_dir)
    log.info("found %d bronze dump dir(s) under %s",
             len(dumps), args.bronze_dir)

    n_loaded = n_skipped = 0
    for dump_dir in dumps:
        if already_loaded(conn, dump_dir):
            n_skipped += 1
            log.debug("skipping already-loaded dump %s", dump_dir.name)
            continue
        try:
            conn.execute("BEGIN")
            load_dump(conn, dump_dir, schema_version)
            conn.execute("COMMIT")
            n_loaded += 1
        except Exception:  # noqa: BLE001 — log + rollback + continue
            conn.execute("ROLLBACK")
            log.exception("load failed for %s; skipped", dump_dir.name)
    conn.close()
    log.info("done: %d dumps loaded, %d already-loaded skipped",
             n_loaded, n_skipped)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
