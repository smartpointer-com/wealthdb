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
- Transactions are UPSERTed on the compound key
  (`transaction_external_id`, `account_external_id`). Re-running a
  window converges to UBS's current view. UBS's own transaction number
  is that id, except where the bank stamps one number on several
  movements, in which case one row keeps it and the rest take a suffix
  — which row, and whether any row in the window keeps it at all,
  depends on what silver already holds; see `_assign_export_txn_ids`.
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
import functools
import hashlib
import io
import json
import logging
import os
import re
import sqlite3
import sys
from collections.abc import Iterator
from concurrent.futures import ProcessPoolExecutor, as_completed
from datetime import datetime, timezone
from pathlib import Path

from collectorkit import bronze, cli, silver, srcfp

import card_parsers  # local module
import landmarks as ubs  # local module: the export's shared vocabulary

log = logging.getLogger("ubs-web.load")

# `parser_generations` (migration 0009) records which generation of
# `pdf_parsers` produced the document-derived rows in hand, so that a moved
# parser drops them before re-deriving instead of landing new ones beside
# them. See `_purge_stale_document_rows`.
DOCUMENT_GENERATION_SCOPE = "documents"

# Bank-delivered PDFs — documents the e-banking archive never listed, so
# `download` cannot reach them and they arrive by hand. They live in their
# own directory at the bronze root rather than outside the tree, which is
# what keeps silver reproducible from bronze alone: sourced from elsewhere,
# every `--force` rebuild would silently drop them. The name is not a run-dir
# slug, so `scan_bronze` does not walk it and `prune` cannot delete it.
SUPPLIED_DOCUMENTS_DIRNAME = "supplied-documents"

# What a supplied document's `doc_token` is built from — the content hash,
# since there is no UBS token to record. The prefix keeps the two origins
# legible in the table and lets the supplied rows be found without a join.
SUPPLIED_DOC_TOKEN_PREFIX = "supplied:"

# The `doc_type` a supplied Statement of assets is indexed under: the title
# the document prints on its own first page. Scraped statements of assets
# carry no doc_type at all — DOC_LABEL_RE needs letters where their listing
# label has digits — so this admits the supplied ones to the archive walk
# without changing the road any scraped document travels.
SUPPLIED_STMT_OF_ASSETS_DOC_TYPE = "Statement of assets"

# The tables the PDF passes own. Each `historical_*` table is written by
# exactly one of them and holds nothing else; `transactions` is shared with
# the live CSV export, whose ids are UBS's own transaction numbers, so the
# statement era is named by its `stmt:` prefix (DESIGN.md §3.6).
#
# The advice pass is the exception the prefix rule cannot cover. An advice
# row is deliberately keyed by UBS's own transaction number — that is the
# whole point of it, since the number is what carries the row to the twin
# leg the export already holds, and why `_assign_export_txn_ids` leaves that
# number on the payment rather than on its fee — so from the id alone it is
# indistinguishable
# from an export row, and a prefix delete would either miss it or take the
# export with it. It is recognised by the marker the parser writes into its
# payload instead (`document` = `payment_advice_pdf`), which no export row
# carries — an export payload being the CSV line verbatim — and no statement
# row spells.
_DOCUMENT_PASS_DELETES = (
    "DELETE FROM historical_position_snapshots",
    "DELETE FROM historical_cash_balances",
    "DELETE FROM historical_mortgages",
    "DELETE FROM transactions WHERE transaction_external_id LIKE 'stmt:%'",
    "DELETE FROM transactions WHERE payload LIKE '%payment_advice_pdf%'",
    "DELETE FROM advices",
    "DELETE FROM statement_trades",
)

# The `doc_type` spellings of the per-movement payment advices. UBS labels
# them inconsistently — one archive holds both 'Debit Advice' and 'Debit
# advice' — so the match is case-folded and the next casing it invents still
# routes. 'UBS Advice' (a fee the bank bills itself) and 'Advice _ Statement'
# (a securities confirmation) are deliberately NOT in the set: neither is a
# payment, and neither carries the movement block this pass reads.
ADVICE_DOC_TYPES = frozenset({"credit advice", "debit advice"})

# The `doc_type`s that carry a securities advice: what a holding was
# bought for, where the statement of assets states no price (`advices`,
# migration 0013). A Private Market Letter is a capital call only when its
# cover page says so, and the parser declines the quarterly reports and
# other letters filed under the same label.
CONTRACT_NOTE_DOC_TYPE = "Contract note"
PRIVATE_MARKET_LETTER_DOC_TYPE = "Private Market Letter"


@functools.lru_cache(maxsize=1)
def _document_generation() -> str:
    """Fingerprint of the PDF parsing logic.

    `pdf_parsers` is imported lazily here as it is at its other call site:
    pdfplumber is slow to import, and a load whose dumps carry no documents
    never needs it.
    """
    import pdf_parsers  # noqa: PLC0415
    return srcfp.parser_fingerprint(
        [pdf_parsers], ("pdfplumber", "pdfminer.six"))


def _purge_stale_document_rows(conn: sqlite3.Connection) -> int:
    """Drop the document-derived rows when the parser that produced them has
    moved, so the pass re-derives rather than adds to them.

    Whole-pass rather than per-document, though every row carries a
    `source_doc_token`: the precious-metals overview row is written by
    several documents that collapse onto one key, so a per-document delete
    would remove a row another document still owns.
    """
    if not silver.stale_generation(conn, DOCUMENT_GENERATION_SCOPE,
                                   _document_generation()):
        return 0
    dropped = sum(conn.execute(sql).rowcount for sql in _DOCUMENT_PASS_DELETES)
    log.info("the document parsers have changed since these rows were "
             "written; dropped %d for re-derivation", dropped)
    return dropped

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
    p.add_argument("--supplied-documents-dir", type=Path, default=None,
                   help="Directory of bank-delivered PDFs to ingest beside "
                        "the scraped archive (default: "
                        f"<bronze-dir>/{SUPPLIED_DOCUMENTS_DIRNAME}). Absent "
                        "or empty is the normal case.")
    cli.add_standard_args(p, verb="load")
    args = p.parse_args(argv)
    if args.supplied_documents_dir is None:
        args.supplied_documents_dir = args.bronze_dir / SUPPLIED_DOCUMENTS_DIRNAME
    return args


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
         if p.is_dir() and bronze.RUN_DIR_RE.match(p.name)),
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
              schema_version: int, parse_cache: dict) -> None:
    """Load one bronze dump into silver. Single transaction.

    `parse_cache` is shared across the whole load run so each document
    in the cumulative archive is parsed once (see
    `_load_historical_from_pdfs`)."""
    snapshot_at = ts_from_dir(dump_dir.name)
    log.info("loading %s (snapshot_at=%d)", dump_dir.name, snapshot_at)

    run_meta = _read_run_json(dump_dir)

    _insert_dump_run(conn, snapshot_at, schema_version, dump_dir, run_meta)

    pos_count = _load_positions(conn, snapshot_at, dump_dir)
    txn_count = _load_transactions(conn, snapshot_at, dump_dir)
    ptxn_count = _load_portfolio_transactions(conn, snapshot_at, dump_dir)
    doc_count = _load_documents(conn, snapshot_at, dump_dir, run_meta)
    hist_pos, hist_cash, hist_mort, hist_txn = _load_historical_from_pdfs(
        conn, snapshot_at, dump_dir, parse_cache)
    card_acc, card_txn, card_inv, card_stmt = _load_cards(
        conn, snapshot_at, dump_dir)

    log.info("loaded %s: positions=%d transactions=%d "
             "portfolio_transactions=%d documents=%d "
             "hist_positions=%d hist_cash_balances=%d "
             "hist_mortgages=%d hist_transactions=%d "
             "card_accounts=%d card_transactions=%d card_invoices=%d "
             "card_statements=%d",
             dump_dir.name, pos_count, txn_count, ptxn_count, doc_count,
             hist_pos, hist_cash, hist_mort, hist_txn,
             card_acc, card_txn, card_inv, card_stmt)


def _read_run_json(dump_dir: Path) -> dict:
    path = dump_dir / "run.json"
    if not path.exists():
        return {}
    return json.loads(path.read_text(encoding="utf-8"))


def _insert_dump_run(conn: sqlite3.Connection, snapshot_at: int,
                     schema_version: int, dump_dir: Path,
                     run_meta: dict) -> None:
    # One window per run (run.json `window`). Older dumps carry a per-facet
    # window on each block instead; their `transactions` pair is the
    # equivalent, so that bronze still loads.
    window = run_meta.get("window") or run_meta.get("transactions") or {}
    conn.execute(
        "INSERT INTO dump_runs ("
        "snapshot_at, silver_schema_version, run_dir, "
        "window_since, window_until"
        ") VALUES (?, ?, ?, ?, ?)",
        (
            snapshot_at, schema_version, str(dump_dir),
            ts_from_iso(window.get("since")),
            ts_from_iso(window.get("until")),
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


def _iter_positions_rows(csv_path: Path) -> Iterator[dict]:
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
# Portfolio securities transactions
# ----------------------------------------------------------------

# The export's columns, in the order it writes them. Two are headed
# `Ccy.`: the first sits beside the ISIN and is always empty, the second
# carries the settlement currency. csv.DictReader would keep only one of
# them, so the header is matched positionally against this list instead
# and the duplicate resolved by position.
PORTFOLIO_TXN_COLUMNS = (
    "Valuation date", "Banking relationship", "Portfolio", "Product",
    "Trade date", "Trade time", "Booking", "Value date",
    "Description 1", "Description 2", "Description 3",
    "Valor", "ISIN", "ISIN currency", "Number/Amt.", "Settlement currency",
    "Trans. price", "Exchange rate", "Valuation currency", "Trans. value",
    "Accrued interest", "Realized P/L in %", "Realized P/L",
    "Order no.", "External reference",
    "Asset class", "Sub-asset class", "Instrument category",
)
# The two headings the export repeats, renamed above by position.
_PORTFOLIO_TXN_RAW_HEADER = tuple(
    "Ccy." if c in ("ISIN currency", "Settlement currency") else c
    for c in PORTFOLIO_TXN_COLUMNS
)


def _load_portfolio_transactions(conn: sqlite3.Connection, snapshot_at: int,
                                 dump_dir: Path) -> int:
    """Parse every `portfolio_transactions/*.csv`; UPSERT into
    portfolio_transactions (migration 0012).

    A dump taken before this surface was harvested has no such dir and
    loads as it always did."""
    src_dir = dump_dir / "portfolio_transactions"
    if not src_dir.is_dir():
        return 0
    inserted = 0
    for csv_path in sorted(src_dir.glob("*.csv")):
        try:
            rows = _read_portfolio_txn_csv(csv_path)
        except ValueError as e:
            log.warning("portfolio transactions %s: %s", csv_path.name, e)
            continue
        for row in rows:
            inserted += _ingest_portfolio_txn_row(conn, snapshot_at, row)
    return inserted


def _read_portfolio_txn_csv(csv_path: Path) -> list[dict]:
    """Rows of one export, as dicts keyed by PORTFOLIO_TXN_COLUMNS.

    The file closes with a footer line stating the window it covers;
    that line and anything after it is not data. A header that is not
    the one this parser was written against raises rather than being
    read positionally anyway — a column UBS inserts would otherwise
    shift every value one place to the left in silence."""
    text = csv_path.read_text(encoding="utf-8-sig", errors="replace")
    reader = csv.reader(io.StringIO(text), delimiter=";")
    try:
        header = next(reader)
    except StopIteration:
        return []
    header = [h.strip() for h in header]
    if tuple(header) != _PORTFOLIO_TXN_RAW_HEADER:
        raise ValueError(
            f"unexpected header ({len(header)} columns); the export's "
            f"layout has changed and the parser must be revisited")
    out: list[dict] = []
    for raw in reader:
        if not raw or not raw[0].strip():
            continue
        if raw[0].startswith(ubs.TXN_EXPORT_FOOTER_PREFIX):
            break
        # A short row is padded rather than dropped: the export omits
        # trailing empties on some rows.
        raw = raw + [""] * (len(PORTFOLIO_TXN_COLUMNS) - len(raw))
        out.append({k: raw[i].strip()
                    for i, k in enumerate(PORTFOLIO_TXN_COLUMNS)})
    return out


def parse_grouped_decimal(s: str | None) -> float | None:
    """Parse a decimal written with apostrophe thousands separators
    ("-98'765", "1'234.50"). Distinct from `parse_decimal`, which
    reads the plain form the cash CSVs use.

    The portfolio export annotates some figures with the unit they are
    counted in — a quantity in pieces ("4'000 p"), a price per unit
    ("250.75 a"), a bond or deposit quoted in percent ("100%"). The
    number is the same number whatever unit it is stated in, so the
    annotation is read and dropped; refusing it left a real trade with
    no quantity at all.

    An FX leg states a PAIR in one cell ("50'000 / -45'000.5": bought
    the one, sold the other), and that is not a figure this returns.
    Taking the first half would silently book one leg's amount against
    the other leg's currency, so the pair reads as no value and the
    two numbers stay in the payload where both are visible."""
    if s is None:
        return None
    s = s.strip().replace("'", "").replace("’", "")
    if not s:
        return None
    m = _GROUPED_DECIMAL_RE.fullmatch(s)
    if not m:
        return None
    try:
        return float(m.group(1))
    except ValueError:
        return None


# A signed decimal, optionally followed by the unit it is counted in.
_GROUPED_DECIMAL_RE = re.compile(r"([+-]?\d*\.?\d+)\s*(?:%|[a-zA-Z]{1,3})?")


def portfolio_account_canonical(product: str | None) -> str | None:
    """Canonical safekeeping-account id from the export's "Product".

    The export writes the spaced, dotted form (`1234 00000001.S5`);
    the PSN feed keys the same account `12340000000001S5`,
    zero-padding the middle group to ten digits. Both feeds naming one
    account the same way is what lets the adapter tell a trade it
    already holds from a new one, so a shape this cannot convert
    returns None rather than a near-miss id."""
    if not product:
        return None
    m = _PRODUCT_RE.fullmatch(product.strip())
    if not m:
        return None
    head, body, suffix = m.group(1), m.group(2), m.group(3)
    return f"{head}{body.zfill(10)}{suffix}"


_PRODUCT_RE = re.compile(r"(\d{4})\s+(\d{1,10})\.([A-Z0-9]{1,6})")
_PORTFOLIO_RE = re.compile(r"(\d{4})\s+(\d{1,10})\s+([A-Z0-9]{1,6})")


def portfolio_id_canonical(portfolio: str | None) -> str | None:
    """Canonical portfolio id from the export's "Portfolio" column.

    `1234 00000001 0006` is the same identifier the sibling feed keys
    portfolios by as `1234000000010006`. Unlike an account id (above),
    a portfolio id is NOT zero-padded: the two identifier spaces look
    alike and pad differently, and a padded portfolio id joins to
    nothing."""
    if not portfolio:
        return None
    m = _PORTFOLIO_RE.fullmatch(portfolio.strip())
    if not m:
        return None
    return "".join(m.groups())


def portfolio_txn_id(account: str, row: dict) -> str:
    """Stable id for one export row.

    Every actual trade carries UBS's own "External reference", which is
    what the id is built from. Corporate actions and FX legs are
    published without one, so those hash the columns the bank does fill
    — enough of them that two distinct bookings of the same type on the
    same security and day do not collide.

    Only columns that state the BOOKING may enter the hash. The
    valuation is not one of them: each scope values the same booking in
    its own reporting currency, so "Trans. value" (and the currency
    beside it) differ between a portfolio's own export and a
    consolidated view's, and hashing them gave one booking two ids —
    which defeated the dedupe below and let a trade reach the ledger
    twice."""
    ref = (row.get("External reference") or "").strip()
    if ref:
        return f"ptx:{ref}"
    parts = "|".join(str(row.get(k, "") or "") for k in (
        "Product", "Trade date", "Trade time", "Booking", "Value date",
        "Description 1", "Description 2", "Valor", "ISIN",
        "Number/Amt.", "Order no.",
    ))
    digest = hashlib.sha256(f"{account}|{parts}".encode()).hexdigest()
    return f"ptx:{digest[:16]}"


def names_a_portfolio(portfolio_external_id: str | None) -> bool:
    """Whether an id names one of the numbered portfolios.

    UBS files consolidated views beside the real portfolios and offers
    them in the same chooser. A consolidated view reports every row it
    covers under its own id, which is lettered rather than numbered
    (`…R001`) and belongs to no portfolio — so a row carrying one
    states which custody account moved but not which cash account
    settled, and the pair that answers that is portfolio + currency."""
    return bool(portfolio_external_id) and portfolio_external_id.isdigit()


def _ingest_portfolio_txn_row(conn: sqlite3.Connection, snapshot_at: int,
                              row: dict) -> int:
    account = portfolio_account_canonical(row.get("Product"))
    portfolio = portfolio_id_canonical(row.get("Portfolio"))
    value_date = ts_from_dmy(row.get("Value date"))
    booking_type = (row.get("Description 1") or "").strip()
    # The three columns without which a row cannot be placed, keyed or
    # classified. The export pads its own footer area with blank-ish
    # lines, so this is also what keeps those out of silver.
    if not account or value_date is None or not booking_type:
        return 0
    txn_id = portfolio_txn_id(account, row)
    # The same booking is published under every scope that covers it, so
    # a consolidated copy arrives keyed identically to the real
    # portfolio's own. Whichever lands second would otherwise win and
    # take the portfolio id down with it. A copy that names a portfolio
    # is kept over one that does not; a booking only a consolidated
    # scope reported is still kept, because the alternative is losing
    # it.
    if not names_a_portfolio(portfolio):
        held = conn.execute(
            "SELECT portfolio_external_id FROM portfolio_transactions "
            "WHERE transaction_external_id = ? "
            "  AND safekeeping_account_external_id = ?",
            (txn_id, account),
        ).fetchone()
        if held is not None and names_a_portfolio(held[0]):
            return 0
    conn.execute(
        "INSERT OR REPLACE INTO portfolio_transactions ("
        "transaction_external_id, safekeeping_account_external_id, "
        "portfolio_external_id, snapshot_at, trade_date, booking_date, "
        "value_date, booking_type, security_name, valor, isin, quantity, "
        "settlement_currency_iso, trans_price, exchange_rate, "
        "valuation_currency_iso, trans_value, accrued_interest, "
        "realized_pl, order_no, external_reference, asset_class, "
        "sub_asset_class, instrument_category, payload"
        ") VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
        (
            txn_id, account,
            portfolio or "", snapshot_at,
            ts_from_dmy(row.get("Trade date")),
            ts_from_dmy(row.get("Booking")),
            value_date, booking_type,
            row.get("Description 2") or None,
            row.get("Valor") or None,
            row.get("ISIN") or None,
            parse_grouped_decimal(row.get("Number/Amt.")),
            row.get("Settlement currency") or None,
            parse_grouped_decimal(row.get("Trans. price")),
            parse_grouped_decimal(row.get("Exchange rate")),
            row.get("Valuation currency") or None,
            parse_grouped_decimal(row.get("Trans. value")),
            parse_grouped_decimal(row.get("Accrued interest")),
            parse_grouped_decimal(row.get("Realized P/L")),
            row.get("Order no.") or None,
            row.get("External reference") or None,
            row.get("Asset class") or None,
            row.get("Sub-asset class") or None,
            row.get("Instrument category") or None,
            normalize_payload(row),
        ),
    )
    return 1


# ----------------------------------------------------------------
# Transactions
# ----------------------------------------------------------------

def _load_transactions(conn: sqlite3.Connection, snapshot_at: int,
                       dump_dir: Path) -> int:
    """Parse every `transactions/cash_*.csv` in the dump dir;
    UPSERT into transactions keyed by (transaction_external_id,
    account_external_id).

    Every CSV of the dump is parsed before anything is written, because
    the id a row gets depends on the other rows sharing its transaction
    number (`_assign_export_txn_ids`) and UBS splits one account's
    history across several files. Grouping per file would let a product
    whose movements straddle a split boundary mint the same ids twice.
    """
    txn_dir = dump_dir / "transactions"
    if not txn_dir.is_dir():
        return 0
    rows: list[dict] = []
    for csv_path in sorted(txn_dir.glob("cash_*.csv")):
        rows.extend(_parse_transactions_csv(csv_path))
    ids = _assign_export_txn_ids(rows, _bare_number_holders(conn, rows))
    for txn_id, row in zip(ids, rows, strict=True):
        _upsert_export_transaction(conn, snapshot_at, txn_id, row)
    return len(rows)


# UBS's "Transaction no." is not one per movement. It is one per
# BOOKING EVENT as the bank models it, and the bank sometimes models
# several movements as one:
#
#   * a deposit product (a call deposit, a fixed-term deposit) stamps
#     every increase, decrease, repayment and monthly interest payment
#     with the number derived from the product's own serial, so the
#     whole life of the product shares ONE number;
#   * a cross-border payment carries the correspondent bank's
#     third-party charge under the number of the payment it belongs to,
#     so a payment and its fee share one number.
#
# Keying silver on the bare number therefore made the rows of such a
# group overwrite each other, and an ON CONFLICT upsert cannot tell
# that from a re-load of the same row: no parse failed, nothing was
# logged, and the survivor looked like a complete account. A deposit
# product's whole ledger reduced to whichever row the export printed
# last.
#
# The fix follows the statement era, which has always suffixed the
# rows of a split movement (`_stmt_txn_id`). One row of the group keeps
# UBS's bare number — the advice pass needs it, since an advice is
# keyed by the number that carries it to its twin leg (see the note at
# the head of this module) — and the rest take a suffix.
#
# Which row keeps it is decided against silver, not against the dump.
# A row that already holds the bare number keeps it, whatever else this
# window carries; a group whose holder this window does not cover
# leaves that number alone and suffixes every member present; and only
# a group nobody holds yet picks, there by largest absolute amount, so
# the PAYMENT takes the number and its fee the suffix, which is the
# pairing the advice pass wants (`_bare_number_holders`).
#
# The suffix is derived from the row's own content rather than its
# position in the file — bar the ordinal that separates two rows
# identical in every movement field — so a dump whose window covers a
# different slice of the same group still mints the same ids.


def _export_row_fingerprint(row: dict) -> str:
    """The movement fields that distinguish two rows sharing a
    transaction number. Raw cell text, not parsed values, so a
    formatting change in how an amount is rendered cannot move an id."""
    return "|".join((_export_movement_fingerprint(row), row["description1"]))


def _export_movement_fingerprint(row: dict) -> str:
    """`_export_row_fingerprint` without the name the row is booked
    under. The bank restates a security's name on rows it has already
    exported (a company renamed, a share class retitled), so the same
    movement can come back under a different Description1; this is what
    still recognises it."""
    return "|".join((
        row["booking_date_raw"], row["value_date_raw"],
        row["debit_raw"], row["credit_raw"],
        row["description_kind"] or "",
    ))


def _bare_number_holders(conn: sqlite3.Connection,
                         rows: list[dict]) -> dict[tuple[str, str], dict]:
    """(account, transaction no.) -> the movement fields of the row
    silver ALREADY holds under the bare number, for the groups this dump
    touches.

    Which row of a group keeps the bank's bare number cannot be decided
    from the dump alone. UBS clamps its transactions UI and the
    collector's own `--lookback` is a window, so a later run routinely
    sees only PART of a group — and a rule that picks the largest member
    of whatever it can see hands the number to a different row each
    time. That is not a cosmetic churn: the row holding the number is
    overwritten with the newcomer's data and the newcomer is stored a
    second time under its suffix, which is the silent loss this whole id
    scheme exists to prevent, arriving by the back door.

    So the question is asked of the union of what is stored and what
    arrived: whoever holds the number keeps it, and only a group with no
    holder yet picks one. Advice rows are ignored — they carry the bare
    number by design (see `_insert_advice_transactions`) and state four
    facts where an export row states all of them, so an export row takes
    the number from one rather than yielding to it."""
    keys = {(r["account_external_id"], r["txn_no"]) for r in rows}
    if not keys:
        return {}
    held: dict[tuple[str, str], dict] = {}
    accounts = sorted({a for a, _ in keys})
    numbers = sorted({n for _, n in keys})
    q = ("SELECT account_external_id, transaction_external_id, payload"
         "  FROM transactions"
         f" WHERE account_external_id IN ({','.join('?' * len(accounts))})"
         f" AND transaction_external_id IN ({','.join('?' * len(numbers))})")
    for account, _txn_no, payload in conn.execute(q, (*accounts, *numbers)):
        key = (account, _txn_no)
        if key not in keys:
            continue
        try:
            cells = json.loads(payload)
        except (TypeError, ValueError):
            continue
        if not isinstance(cells, dict) or "payment_advice_pdf" in str(payload):
            continue
        held[key] = {
            "booking_date_raw": (cells.get("Booking date") or "").strip(),
            "value_date_raw": ((cells.get("Value date") or "").strip()
                               or (cells.get("Trade date") or "").strip()),
            "debit_raw": (cells.get("Debit") or "").strip(),
            "credit_raw": (cells.get("Credit") or "").strip(),
            "description_kind": (cells.get("Description2") or "").strip() or None,
            "description1": (cells.get("Description1") or "").strip(),
        }
    return held


def _assign_export_txn_ids(rows: list[dict],
                           held: dict[tuple[str, str], dict] | None = None) -> list[str]:
    """Return one id per row, positionally aligned with `rows`.

    A transaction number used once is its row's id unchanged, which is
    every row the export has ever loaded but the collided ones."""
    held = held or {}
    groups: dict[tuple[str, str], list[int]] = {}
    for i, row in enumerate(rows):
        groups.setdefault((row["account_external_id"], row["txn_no"]), []).append(i)

    ids: list[str] = [""] * len(rows)
    for key, members in groups.items():
        txn_no = key[1]
        if len(members) == 1 and key not in held:
            ids[members[0]] = txn_no
            continue
        # Whoever already holds the bare number keeps it, whatever else
        # this dump happens to carry. Only a group nobody holds yet
        # picks one, and then the largest movement takes it: the advice
        # pass is keyed by the number and an advice names a payment
        # rather than the fee beside it.
        holder = held.get(key)
        incumbent = [i for i in members if holder is not None
                     and _export_row_fingerprint(rows[i])
                     == _export_row_fingerprint(holder)]
        if not incumbent and holder is not None:
            # The holder may have come back under a restated name. The
            # one member whose movement is the holder's IS the holder;
            # suffixed, it would be stored a second time beside itself.
            # Two such members cannot be told apart, so neither claims.
            restated = [i for i in members
                        if _export_movement_fingerprint(rows[i])
                        == _export_movement_fingerprint(holder)]
            if len(restated) == 1:
                incumbent = restated
        if incumbent:
            primary = incumbent[0]
        elif key in held:
            # The holder is a row this window does not cover. It is not
            # ours to move, and nothing here may take its number.
            primary = None
        else:
            primary = max(members, key=lambda i: (
                abs(rows[i]["amount_debit"] or 0.0) + abs(rows[i]["amount_credit"] or 0.0),
                _export_row_fingerprint(rows[i]),
            ))
        # Two rows of a group that are identical in every movement
        # field hash alike; an ordinal keeps them apart rather than
        # letting one eat the other, which is the whole defect.
        used: dict[str, int] = {}
        for i in members:
            if primary is not None and i == primary:
                ids[i] = txn_no
                continue
            digest = hashlib.sha256(
                _export_row_fingerprint(rows[i]).encode("utf-8")).hexdigest()[:8]
            seen = used.get(digest, 0)
            used[digest] = seen + 1
            ids[i] = f"{txn_no}#{digest}" + (f".{seen}" if seen else "")
        if len(members) > 1:
            log.info("transaction no. %s names %d movements on %s; "
                     "%d kept under suffixed ids",
                     txn_no, len(members), key[0],
                     len(members) - (1 if primary is not None else 0))
    return ids


def _upsert_export_transaction(conn: sqlite3.Connection, snapshot_at: int,
                               txn_id: str, row: dict) -> None:
    """Write one parsed export row under the id `_assign_export_txn_ids`
    gave it."""
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
            txn_id, row["account_external_id"], snapshot_at,
            row["trade_date"], row["booking_date"], row["value_date"],
            row["currency_iso"], row["amount_debit"], row["amount_credit"],
            row["counterparty"], row["description_kind"], row["payload"],
        ),
    )


def _parse_transactions_csv(csv_path: Path) -> list[dict]:
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
        return []
    account_ext = iban_canonical(iban) or ""
    if not account_ext:
        log.warning("no IBAN in %s; skipping", csv_path.name)
        return []
    header = [c.strip() for c in lines[header_idx].split(";")]
    idx = {name: i for i, name in enumerate(header) if name}
    rows: list[dict] = []
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
        description1 = _cell(raw, idx, "Description1")
        rows.append({
            "txn_no": txn_no,
            "account_external_id": account_ext,
            "trade_date": ts_from_iso(trade_date),
            "booking_date": ts_from_iso(_cell(raw, idx, "Booking date")),
            "value_date": value_date_ts,
            "currency_iso": _cell(raw, idx, "Currency"),
            "amount_debit": parse_decimal(_cell(raw, idx, "Debit")),
            "amount_credit": parse_decimal(_cell(raw, idx, "Credit")),
            # description1's first semi-line is usually the counterparty
            "counterparty": (description1.split(";", 1)[0] or None),
            "description_kind": _cell(raw, idx, "Description2") or None,
            "payload": normalize_payload(dict(zip(header, raw, strict=False))),
            # Raw cells, kept only to fingerprint a row against its
            # siblings when UBS gives several of them one number.
            "booking_date_raw": _cell(raw, idx, "Booking date"),
            "value_date_raw": value_date_s,
            "debit_raw": _cell(raw, idx, "Debit"),
            "credit_raw": _cell(raw, idx, "Credit"),
            "description1": description1,
        })
    return rows


# ----------------------------------------------------------------
# Documents
# ----------------------------------------------------------------

# Extract the doc type from a listing-row label of the form
# "<doctype> <DD.MM.YYYY> <DD Month YYYY> P. <name> <...>".
DOC_LABEL_RE = re.compile(
    r"^\s*\W*\s*(?P<type>[A-Za-z][A-Za-z _]+?)\s+"
    r"(?P<date>\d{2}\.\d{2}\.\d{4})\b"
)


# ----------------------------------------------------------------
# Credit cards
# ----------------------------------------------------------------

def _load_cards(conn: sqlite3.Connection, snapshot_at: int,
                dump_dir: Path) -> tuple[int, int, int, int]:
    """Load `<dump>/cards/` into the card_* tables.

    A dump with no `cards/` dir — one taken before the card pass existed,
    or with `--no-cards` — loads as zeros rather than as an error, so
    older bronze keeps loading unchanged.

    Nothing is deleted and nothing is zeroed. Every write is an UPSERT
    keyed on an id the source owns, so a run that covered one account, or
    one window, leaves what it did not cover exactly as it was.
    """
    cards_dir = dump_dir / "cards"
    if not cards_dir.is_dir():
        return 0, 0, 0, 0

    # The roster is read FIRST: it states which account each card
    # charges, and the ledger books its rows against the CARD. Without
    # that map a multi-card account's rows key on an id no other card
    # table carries, and they join to nothing.
    roster = _read_card_json(cards_dir / "accounts.json")
    card_to_account = card_parsers.card_account_map(roster)
    stable_accounts = card_parsers.stable_account_ids(roster)

    txn_rows: list[dict] = []
    reserved_counts: dict[str, int] = {}
    for path in sorted(cards_dir.glob("transactions_*.json")):
        rows, page_reserved = card_parsers.parse_transactions(
            _read_card_json(path).get("pages") or [], card_to_account)
        txn_rows.extend(rows)
        for account, count in page_reserved.items():
            reserved_counts[account] = reserved_counts.get(account, 0) + count

    # The billing periods are read once and used twice: the rows go to
    # `card_invoices`, and the statement index needs the same rows to
    # attribute each PDF to its period.
    invoices = _parse_card_invoices(cards_dir, stable_accounts)

    acc_count = _insert_card_accounts(
        conn, snapshot_at, roster, reserved_counts)
    txn_count = _insert_card_transactions(conn, snapshot_at, txn_rows)
    inv_count = _insert_card_invoices(conn, snapshot_at, invoices)
    stmt_count = _insert_card_statements(conn, snapshot_at, cards_dir, invoices)
    _refresh_invoice_coverage(conn)
    _warn_orphan_card_rows(conn)
    return acc_count, txn_count, inv_count, stmt_count


def _warn_orphan_card_rows(conn: sqlite3.Connection) -> None:
    """Report ledger rows whose account has no `card_accounts` row.

    Such a row loads cleanly and then joins to nothing — no account, no
    invoice, and outside gold's spending scope, which selects from the
    accounts table. It is the signature of a ledger keyed on something
    other than the account id, which is exactly what the card-to-account
    map exists to prevent, so it is worth saying out loud rather than
    leaving to be noticed in a report that is quietly short.
    """
    row = conn.execute(
        "SELECT COUNT(*) FROM card_transactions t "
        " WHERE NOT EXISTS (SELECT 1 FROM card_accounts a "
        "                    WHERE a.account_external_id = t.account_external_id)"
    ).fetchone()
    if row and row[0]:
        log.warning("%d card transaction(s) reference an account with no "
                    "card_accounts row; they will not join to an account "
                    "in gold", row[0])


def _read_card_json(path: Path) -> dict:
    """Read one bronze card artefact. An unreadable file warns and yields
    nothing, so one corrupt artefact costs its own facet rather than the
    whole dump."""
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError) as e:
        log.warning("unreadable card artefact %s: %s", path.name, e)
        return {}


def _insert_card_accounts(conn: sqlite3.Connection, snapshot_at: int,
                          roster: dict, reserved_counts: dict) -> int:
    rows = card_parsers.parse_accounts(roster, reserved_counts)
    for row in rows:
        conn.execute(
            "INSERT OR REPLACE INTO card_accounts ("
            "snapshot_at, account_external_id, account_number, currency_iso, "
            "balance, available, credit_limit, reserved_amount, "
            "reserved_count, product_name, card_type, account_status, "
            "structure_type, payload"
            ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
            (snapshot_at, row["account_external_id"], row["account_number"],
             row["currency_iso"], row["balance"], row["available"],
             row["credit_limit"], row["reserved_amount"],
             row["reserved_count"], row["product_name"], row["card_type"],
             row["account_status"], row["structure_type"], row["payload"]))
    return len(rows)


def _insert_card_transactions(conn: sqlite3.Connection, snapshot_at: int,
                              rows: list[dict]) -> int:
    """UPSERT the ledger, keyed on the content id card_parsers mints.

    `snapshot_at` is deliberately left out of the update clause: the
    column means "the dump that first captured this row", and a later
    dump re-observing it must not rewrite that.
    """
    for row in rows:
        conn.execute(
            "INSERT INTO card_transactions ("
            "transaction_external_id, account_external_id, snapshot_at, "
            "transaction_date, value_date, amount, currency_iso, "
            "original_amount, original_currency_iso, exchange_rate, "
            "merchant, merchant_category, merchant_group_code, card_number, "
            "settled_in_invoice, payload"
            ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) "
            "ON CONFLICT(transaction_external_id) DO UPDATE SET "
            "account_external_id=excluded.account_external_id, "
            "transaction_date=excluded.transaction_date, "
            "value_date=excluded.value_date, amount=excluded.amount, "
            "currency_iso=excluded.currency_iso, "
            "original_amount=excluded.original_amount, "
            "original_currency_iso=excluded.original_currency_iso, "
            "exchange_rate=excluded.exchange_rate, "
            "merchant=excluded.merchant, "
            "merchant_category=excluded.merchant_category, "
            "merchant_group_code=excluded.merchant_group_code, "
            "card_number=excluded.card_number, "
            "settled_in_invoice=excluded.settled_in_invoice, "
            "payload=excluded.payload",
            (row["transaction_external_id"], row["account_external_id"],
             snapshot_at, row["transaction_date"], row["value_date"],
             row["amount"], row["currency_iso"], row["original_amount"],
             row["original_currency_iso"], row["exchange_rate"],
             row["merchant"], row["merchant_category"],
             row["merchant_group_code"], row["card_number"],
             row["settled_in_invoice"], row["payload"]))
    return len(rows)


def _parse_card_invoices(cards_dir: Path,
                         accounts: dict[str, str]) -> dict[str, list[dict]]:
    """Every account's billing periods, keyed by the account's short id.

    One read and one parse per account: the rows are wanted twice — as
    `card_invoices` rows, and as the periods a statement PDF is
    attributed to — and reading the same two files twice per account
    only invites the two answers to differ.
    """
    out: dict[str, list[dict]] = {}
    for listing_path in sorted(cards_dir.glob("invoices_*.json")):
        short = listing_path.stem[len("invoices_"):]
        details = _read_card_json(
            cards_dir / f"invoice-details_{short}.json").get("invoices") or []
        out[short] = card_parsers.parse_invoices(
            _read_card_json(listing_path), details, accounts=accounts)
    return out


def _insert_card_invoices(conn: sqlite3.Connection, snapshot_at: int,
                          invoices: dict[str, list[dict]]) -> int:
    """UPSERT the billing periods. `transactions_covered` is left to
    :func:`_refresh_invoice_coverage`, which needs the whole ledger, and
    is carried across the upsert meanwhile."""
    count = 0
    for rows in invoices.values():
        for row in rows:
            conn.execute(
                "INSERT OR REPLACE INTO card_invoices ("
                "account_external_id, period_end, period_start, "
                "invoice_external_id, snapshot_at, invoicing_date, "
                "debiting_date, due_on, due_amount, minimal_due_amount, "
                "currency_iso, balance_forward, total_debit, total_credit, "
                "reconciles, statement_type, payment_method, invoice_status, "
                "transactions_covered, payload"
                ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, "
                "?, ?, COALESCE((SELECT transactions_covered FROM "
                "card_invoices WHERE account_external_id = ? AND "
                "period_end = ?), 0), ?)",
                (row["account_external_id"], row["period_end"],
                 row["period_start"], row["invoice_external_id"], snapshot_at,
                 row["invoicing_date"], row["debiting_date"], row["due_on"],
                 row["due_amount"], row["minimal_due_amount"],
                 row["currency_iso"], row["balance_forward"],
                 row["total_debit"], row["total_credit"], row["reconciles"],
                 row["statement_type"], row["payment_method"],
                 row["invoice_status"], row["account_external_id"],
                 row["period_end"], row["payload"]))
            count += 1
    return count


def _insert_card_statements(conn: sqlite3.Connection, snapshot_at: int,
                            cards_dir: Path,
                            invoices: dict[str, list[dict]]) -> int:
    """Index the statement PDFs against the periods they belong to.

    A statement is content-addressed, so its filename says nothing about
    which period produced it; the capture writes that mapping beside it.
    A file with no mapping is skipped rather than guessed at — a
    statement filed under the wrong period is worse than one not filed.
    """
    stmt_dir = cards_dir / "statements"
    if not stmt_dir.is_dir():
        return 0
    attribution = _statement_attribution(cards_dir, invoices)
    count = 0
    for pdf in sorted(stmt_dir.glob("*.pdf")):
        meta = attribution.get(pdf.name)
        if meta is None:
            log.warning("card statement %s… has no invoice attribution; "
                        "not indexed", pdf.name[:12])
            continue
        conn.execute(
            "INSERT OR REPLACE INTO card_statements ("
            "content_sha256, account_external_id, invoice_external_id, "
            "period_end, file_path, size_bytes, snapshot_at"
            ") VALUES (?, ?, ?, ?, ?, ?, ?)",
            (pdf.stem, meta["account_external_id"],
             meta["invoice_external_id"], meta["period_end"],
             str(pdf.resolve()), pdf.stat().st_size, snapshot_at))
        count += 1
    return count


def _statement_attribution(cards_dir: Path,
                           invoices: dict[str, list[dict]]) -> dict:
    """Statement filename -> the invoice row it belongs to.

    A statement is content-addressed, so its name carries no invoice id;
    the capture writes the mapping beside it — in the API's own invoice
    ids, which is why the lookup is on the parse's `_handle` and not on
    the content id the row is stored under.
    """
    mapping: dict[str, dict] = {}
    for stmts_path in sorted(cards_dir.glob("statements_*.json")):
        short = stmts_path.stem[len("statements_"):]
        by_id = {r["_handle"]: r for r in invoices.get(short, ())}
        for name, invoice_id in (
                _read_card_json(stmts_path).get("files") or {}).items():
            row = by_id.get(invoice_id)
            if row is not None:
                mapping[name] = row
    return mapping


def _refresh_invoice_coverage(conn: sqlite3.Connection) -> None:
    """Recompute `card_invoices.transactions_covered` across the table.

    A period counts as covered when the account's loaded ledger reaches
    past both of its edges. Reaching past both is what stops a period
    only half-covered by a narrow window from claiming to be whole.

    Recomputed rather than accumulated: coverage is a function of how
    much ledger has landed, which grows with every load, so an answer
    kept from an earlier load goes stale in the direction of claiming
    more than is there.
    """
    conn.execute(
        "UPDATE card_invoices SET transactions_covered = ("
        "  SELECT CASE WHEN COUNT(*) > 0"
        "              AND MIN(t.value_date) <= card_invoices.period_start"
        "              AND MAX(t.value_date) >= card_invoices.period_end"
        "         THEN 1 ELSE 0 END"
        "  FROM card_transactions t"
        "  WHERE t.account_external_id = card_invoices.account_external_id"
        ")")


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


def _supplied_document_meta(pdf: Path) -> tuple[str, int, str] | None:
    """Identify a bank-delivered PDF from its own text.

    Returns (doc_type, doc_date, portfolio_external_id), or None when the
    document is not one this loader knows how to read. Only the Statement
    of assets is recognised today; a type joins it once its own text
    carries enough to identify it.

    Reading the PDF rather than a filename is the whole point: a hand-placed
    file can be called anything, and a name is not evidence of what is
    inside it.
    """
    from pdf_parsers import (  # noqa: PLC0415 — pdfplumber is slow to import
        psn_portfolio_external_id, statement_of_assets_body_meta,
        statement_of_assets_text,
    )
    meta = statement_of_assets_body_meta(statement_of_assets_text(pdf))
    if meta is None:
        return None
    return (SUPPLIED_STMT_OF_ASSETS_DOC_TYPE, meta["as_of_date"],
            psn_portfolio_external_id(meta))


def _load_supplied_documents(conn: sqlite3.Connection,
                             supplied_dir: Path) -> int:
    """Index every recognised PDF in `supplied_dir` into the documents
    table. Returns how many rows were NEW.

    Identity is the content hash, so a re-run changes nothing and a
    statement delivered by hand and later published through the archive
    stays one document rather than two deriving the same positions.

    An unrecognised PDF is named in a warning and left out of the table
    entirely, rather than catalogued with no type — a row nothing can parse
    would sit there looking ingested.

    `snapshot_at` records when the copy was placed, there being no dump that
    captured it.
    """
    if not supplied_dir.is_dir():
        # The default (<bronze-dir>/supplied-documents) simply not existing
        # is the normal case, so this is not worth a line on every run.
        log.debug("supplied documents: %s is not a directory", supplied_dir)
        return 0
    pdfs = sorted(p for p in supplied_dir.glob("*.pdf") if p.is_file())
    if not pdfs:
        return 0
    inserted = 0
    for pdf in pdfs:
        sha = bronze.sha256_file(pdf)[0]
        token = SUPPLIED_DOC_TOKEN_PREFIX + sha
        if conn.execute("SELECT 1 FROM documents WHERE content_sha256 = ?",
                        (sha,)).fetchone():
            # Indexed by an earlier run, or served by the scraped archive
            # under its own token. Caught on the hash before the PDF is
            # opened, so a steady-state load reads no document twice.
            log.debug("supplied document %s already in silver; skipping",
                      pdf.name)
            continue
        meta = _supplied_document_meta(pdf)
        if meta is None:
            log.warning("supplied documents: %s is not a document this "
                        "loader recognises; not indexed", pdf.name)
            continue
        doc_type, doc_date, portfolio = meta
        try:
            conn.execute(
                "INSERT INTO documents ("
                "doc_token, content_sha256, file_path, size_bytes, "
                "snapshot_at, doc_type, doc_date, account_external_id, "
                "portfolio_external_id, label"
                ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                (
                    token, sha, str(pdf.resolve()), pdf.stat().st_size,
                    int(pdf.stat().st_mtime), doc_type, doc_date, None,
                    portfolio, "",
                ),
            )
            inserted += 1
        except sqlite3.IntegrityError:
            # The hash check above covers the ordinary case; this catches a
            # token collision, which only a hash collision could produce.
            log.debug("supplied document %s collided on insert; skipping",
                      pdf.name)
    if inserted:
        log.info("supplied documents: indexed %d new of %d in %s",
                 inserted, len(pdfs), supplied_dir)
    return inserted


# ----------------------------------------------------------------
# Historical snapshots from PDF documents
# ----------------------------------------------------------------

def _parse_one_pdf(args: tuple[str, str, str, str | None]
                   ) -> tuple[str, str, str, list[dict] | dict | None, str | None]:
    """Worker-side: parse one PDF and return its rows. Pure (no DB
    access) so it can run in a ProcessPoolExecutor worker. Returns
    (token, kind, file_name, rows, error_message); exactly one of
    `rows` or `error_message` is set on every non-skipped call."""
    from pdf_parsers import (
        parse_account_statement_combined, parse_capital_call,
        parse_contract_note, parse_maturity_notice, parse_payment_advice,
        parse_statement_of_assets,
    )
    token, fp, label, doc_type = args
    path = Path(fp)
    if not path.is_file():
        return token, "skip", path.name, None, None
    try:
        # The listing label names the type for a scraped document; a
        # supplied one has none and is indexed under the title it prints
        # on itself. Either way the parser reads the same document.
        if ("Statement of assets" in (label or "")
                or doc_type == SUPPLIED_STMT_OF_ASSETS_DOC_TYPE):
            positions, trades = parse_statement_of_assets(path, token, label)
            return (token, "statement_of_assets", path.name,
                    {"positions": positions, "trades": trades}, None)
        if doc_type == "Maturity notice":
            rows = parse_maturity_notice(path, token, label)
            return token, "mortgage", path.name, rows, None
        # Before the Account-Statement fallthrough below, which would
        # otherwise take every doc_type the SELECT admits.
        if (doc_type or "").strip().lower() in ADVICE_DOC_TYPES:
            rows = parse_payment_advice(path, token, label)
            return token, "payment_advice", path.name, rows, None
        if doc_type == CONTRACT_NOTE_DOC_TYPE:
            rows = parse_contract_note(path, token, label)
            return token, "securities_advice", path.name, rows, None
        if doc_type == PRIVATE_MARKET_LETTER_DOC_TYPE:
            rows = parse_capital_call(path, token, label)
            return token, "securities_advice", path.name, rows, None
        # Account Statement: a single PDF open yields BOTH the summary
        # balances (for historical_cash_balances) and the per-transaction
        # movement rows (for the transactions backfill), reusing one
        # page layout across both CPU-bound passes.
        cash, txns = parse_account_statement_combined(path, token, label)
        return (token, "account_statement", path.name,
                {"cash": cash, "transactions": txns}, None)
    except Exception as e:  # noqa: BLE001
        return token, "error", path.name, None, f"{type(e).__name__}: {e}"


def _load_historical_from_pdfs(conn: sqlite3.Connection, snapshot_at: int,
                               dump_dir: Path,
                               parse_cache: dict) -> tuple[int, int, int, int]:
    """Walk every PDF tracked in the documents table whose label
    indicates a Statement of assets, an Account Statement, a
    Maturity notice, a payment advice or a securities advice; parse it
    in a worker-pool of subprocesses, and upsert into the historical_* /
    transactions / advices / statement_trades tables on the main thread.
    Returns
    (position_rows, cash_rows, mortgage_rows, transaction_rows) — the
    payment-advice rows are transactions and are counted with the
    statement ones; the securities advices and the statements'
    transaction lists are logged.

    pdfplumber / pdfminer text extraction is CPU-bound and largely
    GIL-bound, so the speedup comes from real OS processes, not
    threads. SQLite writes stay on the main connection.

    The documents table accumulates across dumps, so each dump's SELECT
    re-lists the whole archive. `parse_cache` (keyed by content_sha256,
    shared across the load run) holds each document's successful parse
    result so it is parsed once, by the first dump that references it;
    later dumps replay the cached rows. A skipped (missing-file) or
    errored parse is deliberately left uncached and re-attempted per
    dump, matching the un-cached baseline. The per-dump inserts below
    still run for every dump, so the silver tables stay byte-identical to
    parsing every dump afresh — including the by-design per-dump
    duplication of NULL-ISIN cash rows and the last-dump snapshot_at
    stamping on statement-derived transaction upserts."""
    docs_dir = dump_dir / "documents"
    if not docs_dir.is_dir():
        return 0, 0, 0, 0

    # Pull (doc_token, file_path, label, doc_type, content_sha256) for
    # relevant docs from the documents table — that's where bronze
    # metadata lives. content_sha256 keys the cross-dump parse cache.
    # The advice spellings come from the one set the router reads too, so a
    # casing UBS invents is admitted here and routed there by the same edit.
    advice_types = sorted(ADVICE_DOC_TYPES)
    advice_slots = ", ".join("?" for _ in advice_types)
    cur = conn.execute(
        "SELECT doc_token, file_path, label, doc_type, content_sha256 "
        "FROM documents "
        "WHERE label LIKE '%Statement of assets%' "
        "   OR doc_type = ? "
        "   OR doc_type = 'Account Statement' "
        "   OR doc_type = 'Maturity notice' "
        "   OR doc_type IN (?, ?) "
        f"   OR LOWER(doc_type) IN ({advice_slots})",
        (SUPPLIED_STMT_OF_ASSETS_DOC_TYPE, CONTRACT_NOTE_DOC_TYPE,
         PRIVATE_MARKET_LETTER_DOC_TYPE, *advice_types),
    )
    work = cur.fetchall()
    if not work:
        return 0, 0, 0, 0

    # Inside the function, and below both early returns, because the purge
    # has no refill of its own: a dump with no `documents/` dir, or one
    # whose archive lists nothing parseable, returns above this line — and
    # purging there would empty the historical tables and put nothing back.
    _purge_stale_document_rows(conn)

    # Per-account MT940 cut-over floors: coverage can begin at
    # different dates per account (or be absent), so a single global
    # floor would silently drop movements. PDF movements are ingested
    # only BELOW each account's own MT940 floor; MT940 owns everything
    # from the floor onward. Computed from the bronze tree, so it is
    # independent of dump load order.
    mt940_floors = _mt940_floors_by_account(dump_dir)

    # Split the archive into already-parsed docs (replay cached rows)
    # and docs this run has not parsed yet (submit to the pool once).
    results: list[tuple] = []
    to_parse: dict[str, tuple] = {}
    for token, fp, label, doc_type, sha in work:
        cached = parse_cache.get(sha)
        if cached is not None:
            results.append(cached)
        else:
            to_parse.setdefault(sha, (token, fp, label, doc_type))

    if to_parse:
        n_workers = max(1, os.cpu_count() or 1)
        log.info("parsing %d PDFs across %d workers", len(to_parse), n_workers)
        with ProcessPoolExecutor(max_workers=n_workers) as ex:
            futures = {ex.submit(_parse_one_pdf, args): sha
                       for sha, args in to_parse.items()}
            for fut in as_completed(futures):
                res = fut.result()
                # res = (token, kind, name, rows, err). Cache only a
                # successful parse (err None, rows present); leave a skip
                # (missing file) or an error uncached so a later dump
                # re-attempts it, exactly as the un-cached baseline would.
                # This keeps a transient in-worker failure (e.g. OOM under
                # memory pressure) from being cached and permanently
                # dropping a document's rows for the rest of the run.
                if res[3] is not None and res[4] is None:
                    parse_cache[futures[fut]] = res
                results.append(res)

    pos_rows = 0
    cash_rows = 0
    mortgage_rows = 0
    txn_rows = 0
    txn_reject_stmts = 0
    securities_advice_rows = 0
    trade_rows = 0
    advices: list[dict] = []
    for _token, kind, name, rows, err in results:
        if err is not None:
            log.warning("PDF parse failed for %s: %s", name, err)
            continue
        if kind == "statement_of_assets":
            pos_rows += _insert_hist_positions(
                conn, (rows or {}).get("positions") or [])
            trade_rows += _insert_statement_trades(
                conn, (rows or {}).get("trades") or [])
        elif kind == "mortgage":
            mortgage_rows += _insert_hist_mortgages(conn, rows or [])
        elif kind == "securities_advice":
            securities_advice_rows += _insert_advices(conn, rows or [])
        elif kind == "payment_advice":
            # Collected, not written here: an advice is only worth writing
            # where nothing else recorded the movement, and the statement
            # ledger it has to be measured against is written by the branch
            # below. Inside one loop the two would race — the results arrive
            # in whatever order the parse workers finished — and the same
            # archive would produce a different silver on each load.
            advices.extend(rows or [])
        elif kind == "account_statement":
            cash_rows += _insert_hist_cash_balances(
                conn, (rows or {}).get("cash") or [])
            n, rejected = _insert_hist_transactions(
                conn, snapshot_at, (rows or {}).get("transactions") or [],
                mt940_floors)
            txn_rows += n
            txn_reject_stmts += rejected
    if txn_reject_stmts:
        log.warning("%d Account-Statement PDF(s) failed movement "
                    "reconciliation; their transactions were NOT ingested",
                    txn_reject_stmts)
    advice_rows = _insert_advice_transactions(conn, snapshot_at, advices)
    if advice_rows:
        log.info("%d movement(s) from payment advices", advice_rows)
    txn_rows += advice_rows
    if securities_advice_rows:
        log.info("%d capital call(s) and contract note(s)",
                 securities_advice_rows)
    if trade_rows:
        log.info("%d booking(s) from statement transaction lists", trade_rows)
    # Stamped here rather than by the caller, so it records a walk that
    # actually ran: the early returns above leave the older generation in
    # place and the next dump tries again.
    silver.stamp_generation(conn, DOCUMENT_GENERATION_SCOPE,
                            _document_generation())
    return pos_rows, cash_rows, mortgage_rows, txn_rows


# What identifies a CASH row in `historical_position_snapshots` — the
# primary key with the ISIN taken out, since a cash line has none.
_HIST_CASH_IDENTITY = ("as_of_date", "portfolio_external_id",
                       "account_external_id", "currency_iso")


def _replace_hist_cash_row(conn: sqlite3.Connection, r: dict) -> None:
    """Clear the cash row `r` is about to replace.

    `INSERT OR REPLACE` cannot do it: the table's primary key ends in
    `instrument_isin`, a cash line has none, and SQLite treats NULLs in a
    primary key as DISTINCT — so the upsert never fires and every re-derived
    copy lands as a new row. The pass re-lists the whole document archive on
    every dump, so that is one extra copy of every cash row per dump, without
    limit (migration 0011 collapsed the ones already stored).

    Deleting the match first is exactly what the key would do if NULLs
    compared equal, which keeps last-writer-wins for cash the same as it is
    for securities.
    """
    conn.execute(
        "DELETE FROM historical_position_snapshots WHERE instrument_isin IS NULL"
        + "".join(f" AND {col} = ?" for col in _HIST_CASH_IDENTITY),
        tuple(r[col] for col in _HIST_CASH_IDENTITY),
    )


# The columns `pdf_parsers` fills on every position row it emits.
_HIST_POSITION_COLUMNS = (
    "as_of_date", "portfolio_external_id", "account_external_id",
    "instrument_isin", "currency_iso", "units", "market_value",
    "market_value_currency", "cost_price", "market_price",
    "accrued_interest", "current_fx_rate", "acquisition_fx_rate",
    "cost_basis", "nav_date", "last_purchase_date", "description", "sector",
    "source_doc_token", "payload",
)


def _insert_hist_positions(conn: sqlite3.Connection,
                           rows: list[dict]) -> int:
    n = 0
    for r in rows:
        try:
            if r["instrument_isin"] is None:
                _replace_hist_cash_row(conn, r)
            conn.execute(
                "INSERT OR REPLACE INTO historical_position_snapshots ("
                f"{', '.join(_HIST_POSITION_COLUMNS)}"
                f") VALUES ({', '.join('?' for _ in _HIST_POSITION_COLUMNS)})",
                tuple(r[col] for col in _HIST_POSITION_COLUMNS),
            )
            n += 1
        except sqlite3.IntegrityError as e:
            log.debug("hist position insert failed: %s", e)
    return n


# The columns `pdf_parsers` fills on every `advices` row.
_ADVICE_COLUMNS = (
    "source_doc_token", "kind", "title", "doc_date", "trade_date",
    "value_date", "instrument_isin", "valor", "security_name",
    "currency_iso", "quantity", "price", "amount", "prepayment",
    "placement_fee", "stamp_duty", "settlement_amount",
    "settlement_currency_iso", "fx_rate", "fx_rate_pair", "payload",
)


def _insert_advices(conn: sqlite3.Connection, rows: list[dict]) -> int:
    """Upsert capital calls and contract notes, one row per document."""
    for r in rows:
        conn.execute(
            f"INSERT OR REPLACE INTO advices ({', '.join(_ADVICE_COLUMNS)}) "
            f"VALUES ({', '.join('?' for _ in _ADVICE_COLUMNS)})",
            tuple(r[col] for col in _ADVICE_COLUMNS),
        )
    return len(rows)


# The columns `pdf_parsers` fills on every `statement_trades` row.
_STATEMENT_TRADE_COLUMNS = (
    "source_doc_token", "seq", "as_of_date", "portfolio_external_id",
    "reporting_currency_iso", "period_start", "period_end", "trade_date",
    "trade_time", "value_date", "booking_text", "quantity",
    "security_name", "valor", "isin", "currency_iso", "cost_price",
    "acquisition_fx_rate", "cost_basis", "transaction_price",
    "transaction_fx_rate", "transaction_gain_pct", "exchange_gain_pct",
    "realized_pl_pct", "transaction_value", "accrued_interest",
    "settlement_amount", "settlement_currency_iso", "taxes", "fees",
    "commission", "stock_exchange_fees", "third_party_fees",
    "financial_transaction_tax", "charges_currency_iso",
    "place_of_execution", "settlement_no", "order_no", "custody_account",
    "account_iban", "payload",
)


def _insert_statement_trades(conn: sqlite3.Connection,
                             rows: list[dict]) -> int:
    """Upsert the bookings a statement's transaction list prints, keyed by
    the statement and the booking's place in its list."""
    for r in rows:
        conn.execute(
            "INSERT OR REPLACE INTO statement_trades "
            f"({', '.join(_STATEMENT_TRADE_COLUMNS)}) "
            f"VALUES ({', '.join('?' for _ in _STATEMENT_TRADE_COLUMNS)})",
            tuple(r[col] for col in _STATEMENT_TRADE_COLUMNS),
        )
    return len(rows)


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


def _mt940_floors_by_account(dump_dir: Path) -> dict[str, int]:
    """Per-account MT940 coverage floor (Unix seconds), read from the
    `From:` line of every `transactions/cash_*.csv` in the WHOLE
    bronze tree (all dumps), keyed by canonical IBAN.

    The MT940/CSV feed is authoritative from its declared `From:`
    date onward for the account it covers; below that date the PDF
    Account Statements are the only source. Coverage is uneven per
    account, so we take the EARLIEST `From:` seen for each account.
    Accounts that never appear in any CSV are absent from the map —
    the PDF then owns all of their history (no cut-over)."""
    floors: dict[str, int] = {}
    root = dump_dir.parent if dump_dir.parent.is_dir() else dump_dir
    for csv_path in sorted(root.glob("*/transactions/cash_*.csv")):
        iban = None
        frm = None
        try:
            lines = csv_path.read_text(encoding="utf-8-sig").splitlines()
        except OSError:
            continue
        for ln in lines[:15]:
            if ln.startswith(TXN_HEADER_IBAN):
                iban = ln.split(";", 1)[1].rstrip(";").strip() or None
            elif ln.startswith("From:"):
                frm = ln.split(";", 1)[1].rstrip(";").strip() or None
        acct = iban_canonical(iban)
        floor = ts_from_iso(frm)
        if not acct or not floor:
            continue
        if acct not in floors or floor < floors[acct]:
            floors[acct] = floor
    return floors


def _stmt_batch_txn_id(account: str, r: dict) -> str:
    """Content-stable transaction id for the MOVEMENT a PDF row came
    from. Same booking on the monthly AND the annual statement (and in
    a statement's post-closing trailer) hashes identically, so the
    ON CONFLICT upsert dedups the overlap; the per-statement occurrence
    index keeps genuinely-distinct identical same-day bookings apart.

    A split bundle's legs all hash to this same value — they are the
    one movement the statement printed — which is what lets a leg name
    the batch row it replaces."""
    parts = "|".join(str(x) for x in (
        account, r["booking_date"], r["value_date"],
        # A leg carries its own share in amount_debit/amount_credit;
        # the movement is identified by the batch total the statement
        # printed, so the hash reads that where a leg has one.
        r.get("multi_parent_debit", r.get("amount_debit")),
        r.get("multi_parent_credit", r.get("amount_credit")),
        r.get("description_kind") or "", r.get("occurrence", 0),
    ))
    return "stmt:" + hashlib.sha256(parts.encode("utf-8")).hexdigest()[:16]


def _stmt_txn_id(account: str, r: dict) -> str:
    """The row's own id: the movement id, suffixed with the leg's
    position when the movement was a bundle the parser split. An
    unsplit row keeps the bare movement id, so every id minted before
    bundles were split is unchanged."""
    tid = _stmt_batch_txn_id(account, r)
    leg = r.get("multi_leg_index")
    return f"{tid}#{leg}" if leg else tid


def _insert_hist_transactions(conn: sqlite3.Connection, snapshot_at: int,
                              rows: list[dict],
                              mt940_floors: dict[str, int]) -> tuple[int, int]:
    """Insert PDF-derived movement rows into the silver `transactions`
    table, applying the per-account MT940 cut-over and content-id
    dedup. Returns (rows_inserted, statements_rejected).

    A statement whose movements failed the running-balance
    reconciliation is rejected wholesale (its column assignment is
    not trustworthy). Movements at/after their account's MT940 floor
    are skipped — MT940 owns that window."""
    if not rows:
        return 0, 0
    if not rows[0].get("reconciled", False):
        return 0, 1  # whole statement rejected
    inserted = 0
    for r in rows:
        account = iban_canonical(r.get("account_external_id"))
        if not account:
            continue  # non-cash / non-IBAN statement header
        floor = mt940_floors.get(account)
        if floor is not None and r["booking_date"] >= floor:
            continue  # MT940 owns this window for this account
        txn_id = _stmt_txn_id(account, r)
        # The batch row this leg replaces was written by an earlier load,
        # under the id the movement still hashes to. Retiring it as the
        # first leg lands keeps the batch total from being counted a
        # second time beside the legs that now carry it. Deliberately
        # after the MT940 cut-over above: where the legs are skipped,
        # nothing replaces the batch row and it must stay.
        if r.get("multi_leg_index") == 1:
            conn.execute(
                "DELETE FROM transactions WHERE transaction_external_id = ? "
                "  AND account_external_id = ?",
                (_stmt_batch_txn_id(account, r), account))
        try:
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
                "booking_date = excluded.booking_date, "
                "value_date = excluded.value_date, "
                "currency_iso = excluded.currency_iso, "
                "amount_debit = excluded.amount_debit, "
                "amount_credit = excluded.amount_credit, "
                "counterparty = excluded.counterparty, "
                "description_kind = excluded.description_kind, "
                "payload = excluded.payload",
                (
                    txn_id, account, snapshot_at,
                    None,  # trade_date: no statement equivalent
                    r["booking_date"], r["value_date"],
                    r.get("currency_iso") or "",
                    r.get("amount_debit"), r.get("amount_credit"),
                    r.get("counterparty"), r.get("description_kind"),
                    r.get("payload") or "{}",
                ),
            )
            inserted += 1
        except sqlite3.IntegrityError as e:
            log.debug("hist transaction insert failed: %s", e)
    return inserted, 0


def _insert_advice_transactions(conn: sqlite3.Connection, snapshot_at: int,
                                rows: list[dict]) -> int:
    """Insert the movement a Credit/Debit Advice states into the silver
    `transactions` table, under UBS's own transaction number. Returns the
    number of rows actually written.

    Why these rows carry no minted id. The advice prints the very
    "Transaction no." the CSV export puts in the id column, and UBS stamps
    BOTH sides of an inter-account transfer with one number — which is
    precisely why silver's primary key is the compound
    (transaction_external_id, account_external_id) in the first place
    (migration 0001). Storing the advice's leg under the bare number and
    its OWN account is therefore not a new id scheme but the shape that key
    was designed to hold: the leg the export never saw lands beside the leg
    it did, and a consumer that pairs legs on a shared transaction number
    finds both.

    Why ON CONFLICT DO NOTHING rather than DO UPDATE. An advice states four
    facts — direction, amount, currency and the two dates — and nothing
    else: no booking type, no counter account, no trade date. The export and
    the MT940 feed state all of those for the same movement. Since the two
    feeds meet on the SAME key here (unlike the statement era, whose minted
    ids can never collide with theirs), an upsert would overwrite a full row
    with a thinner one every time the document pass ran, and the loss would
    be invisible. The advice fills a hole; it does not restate a row that
    already has an owner. Re-derivation when the parser moves comes from the
    pass-level delete instead (`_DOCUMENT_PASS_DELETES`), which drops the
    advice rows by their payload marker so the next run writes them afresh
    — an export row, having no such marker, is not touched.

    Why the MT940 floor does NOT gate these rows. The floor exists because
    the statement era mints content-hashed ids of its own, so a statement
    and the feed recording one booking produce two rows that nothing can
    recognise as one; cutting the PDF off where the feed begins is the only
    way to keep the movement from being counted twice. An advice cannot
    create that duplicate against the FEED: it carries the feed's own id, so
    an overlap there collides on the primary key and the DO NOTHING above
    resolves it in the richer row's favour. Applying the floor as well would
    only ever discard rows the key has already made harmless — and it is
    keyed by account, while the whole value of an advice is the leg on the
    account the feed does not cover at all, which has no floor to be
    measured against.

    Why the key is not enough on its own. Against the STATEMENT era the same
    argument runs backwards: a statement row is keyed by a minted `stmt:`
    hash, so the very movement an advice states can already sit on the same
    account under an id the primary key can never collide with — and most of
    the archive is exactly that, an advice issued for a payment the account's
    own statement went on to print in its ledger. Inserting there does not
    fill a hole, it books the payment a second time, which is the phantom
    flow this whole path exists to remove. So before an advice is written the
    movement is looked for by its CONTENT (`_advice_already_recorded`), and
    an advice that restates a movement silver already holds is dropped. The
    ledger row is the better record anyway: it carries the booking type and
    it reconciled against the statement's printed running balance.
    """
    inserted = 0
    skipped = 0
    # Each existing row may explain at most ONE advice, and the order the
    # advices are considered in decides which. Sorted so that a second run
    # over the same archive reaches the same silver: the pass hands them over
    # in whatever order the parse workers finished. Claims are per (account,
    # id) because that is the row — the two legs of one transfer share the id
    # and a claim on one must not reach across to the other's account.
    claimed: set[tuple[str, str]] = set()
    for r in sorted(rows, key=lambda r: (r.get("transaction_external_id") or "",
                                         r.get("account_external_id") or "")):
        account = iban_canonical(r.get("account_external_id"))
        txn_id = r.get("transaction_external_id")
        if not account or not txn_id:
            continue
        already = _advice_already_recorded(conn, account, r, claimed)
        if already is not None:
            claimed.add((account, already))
            skipped += 1
            continue
        claimed.add((account, txn_id))
        cur = conn.execute(
            "INSERT INTO transactions ("
            "transaction_external_id, account_external_id, snapshot_at, "
            "trade_date, booking_date, value_date, currency_iso, "
            "amount_debit, amount_credit, counterparty, "
            "description_kind, payload"
            ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) "
            "ON CONFLICT(transaction_external_id, account_external_id) "
            "DO NOTHING",
            (
                txn_id, account, snapshot_at,
                None,  # trade_date: an advice prints no trade date
                r["booking_date"], r["value_date"],
                r.get("currency_iso") or "",
                r.get("amount_debit"), r.get("amount_credit"),
                r.get("counterparty"), r.get("description_kind"),
                r.get("payload") or "{}",
            ),
        )
        # 0 when the conflict clause kept the row an earlier, richer feed
        # already wrote; 1 when the advice actually filled a hole.
        if cur.rowcount > 0:
            inserted += 1
    if skipped:
        log.info("%d payment advice(s) restate a movement silver already "
                 "holds; left to the row that has it", skipped)
    return inserted


# Two amounts are the same movement when they agree to the cent. The eras
# print to two decimals and the figures travel through float, so an exact
# comparison would turn a representation artefact into a duplicate row.
_ADVICE_AMOUNT_EPS = 0.005


def _advice_already_recorded(conn: sqlite3.Connection, account: str,
                             r: dict,
                             claimed: set[tuple[str, str]]) -> str | None:
    """The id of the row silver already holds for the movement this advice
    states, or None when nothing holds it.

    Identity is the content, not the id, because the whole difficulty is
    that the eras do not agree on ids: the statement walker mints a `stmt:`
    hash for the very booking the advice names with UBS's transaction
    number. What they do agree on is the account, the day, the currency, the
    COLUMN the figure sits in — and the figure's magnitude. The magnitude is
    compared with abs() on both sides deliberately: the export writes the
    sheet's own signed cell and the PDF eras write the unsigned figure they
    print, so the direction has to be read from the column and never from
    the sign (DESIGN.md §3.6).

    Either date may carry the match. The advice states a bookkeeping entry
    date and a value date; a back-valued payment prints them days apart, and
    which of the two the other era filed the booking under varies with the
    era. Requiring only one to line up keeps such a pair from being written
    twice.

    `claimed` names the rows earlier advices in this pass already matched or
    wrote, so one row can excuse only one advice: two genuinely distinct
    payments of the same amount, on one account, on one day are a shape the
    archive really holds (an FX leg and a same-currency leg landing
    together), and the second of them must still be written."""
    amount = r.get("amount_debit")
    outbound = amount is not None
    if amount is None:
        amount = r.get("amount_credit")
    if amount is None:
        return None
    cur = conn.execute(
        "SELECT transaction_external_id, amount_debit, amount_credit "
        "FROM transactions "
        "WHERE account_external_id = ? AND currency_iso = ? "
        "  AND (value_date = ? OR booking_date = ?) "
        "ORDER BY transaction_external_id",
        (account, r.get("currency_iso") or "",
         r["value_date"], r["booking_date"]),
    )
    for tid, debit, credit in cur.fetchall():
        if (account, tid) in claimed:
            continue
        if (debit is not None) != outbound:
            continue
        figure = debit if outbound else credit
        if figure is None:
            continue
        if abs(abs(figure) - abs(amount)) <= _ADVICE_AMOUNT_EPS:
            return tid
    return None


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

def _derive_documents_without_a_new_dump(
        conn: sqlite3.Connection, dumps: list[Path], parse_cache: dict,
        *, supplied: int = 0) -> None:
    """Derive the document-backed slice when no new dump has arrived to
    carry the work — because the PARSERS have changed, or because a
    supplied document was indexed this run.

    The archive walk lives inside the per-dump load, because a dump is
    what indexes its own PDFs. What the walk DERIVES, though, is the
    whole cumulative archive, and `_document_generation` exists so that
    a changed parser re-derives it. In steady state — every dump loaded,
    no new one yet — that fingerprint could never be read: the walk sat
    behind the already-loaded skip, so a parser improvement took effect
    only on whichever night a new dump happened to arrive, and until
    then the same bronze produced different silver depending on when it
    was loaded.

    A supplied document reaches the table the same way and needs the same
    push: it is indexed before the dump loop, and on a night when every
    dump is already loaded nothing downstream would read it.

    Runs against the newest dump that has an archive directory. Which
    dump is immaterial to what is derived — the file paths come from the
    `documents` table, which spans every dump — and the newest is the
    right stamp for rows written now.
    """
    stale = silver.stale_generation(conn, DOCUMENT_GENERATION_SCOPE,
                                    _document_generation())
    if not stale and not supplied:
        return
    newest = next((d for d in reversed(dumps) if (d / "documents").is_dir()), None)
    if newest is None:
        return
    log.info("%s and no new dump carries the change; deriving the archive "
             "against %s",
             "the document parsers have changed" if stale
             else f"{supplied} supplied document(s) were indexed",
             newest.name)
    try:
        conn.execute("BEGIN")
        pos, cash, mort, txn = _load_historical_from_pdfs(
            conn, ts_from_dir(newest.name), newest, parse_cache)
        conn.execute("COMMIT")
    except Exception:  # noqa: BLE001 — log + rollback, as the dump loop does
        conn.execute("ROLLBACK")
        log.exception("re-deriving the document archive failed")
        return
    log.info("re-derived from documents: positions=%d cash_balances=%d "
             "mortgages=%d transactions=%d", pos, cash, mort, txn)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)

    args.silver_db.parent.mkdir(parents=True, exist_ok=True)
    if args.force:
        silver.reset(args.silver_db)
    conn = sqlite3.connect(str(args.silver_db))
    silver.own_only(args.silver_db)
    conn.execute("PRAGMA foreign_keys = ON;")

    migrations_dir = Path(__file__).parent / "migrations"
    silver.apply_migrations(conn, migrations_dir)
    schema_version = silver.current_schema_version(conn)

    dumps = scan_bronze(args.bronze_dir)
    log.info("found %d bronze dump dir(s) under %s",
             len(dumps), args.bronze_dir)

    # Before the dump loop, so the archive walk inside it already sees the
    # supplied rows — which is what makes a `--force` rebuild pick them up
    # with no special handling. Its own transaction: an index of files on
    # disk should survive a dump that fails to load.
    try:
        conn.execute("BEGIN")
        n_supplied = _load_supplied_documents(conn, args.supplied_documents_dir)
        conn.execute("COMMIT")
    except Exception:  # noqa: BLE001 — log + rollback, as the dump loop does
        conn.execute("ROLLBACK")
        log.exception("indexing the supplied documents failed")
        n_supplied = 0

    n_loaded = n_skipped = 0
    # Shared across dumps: parse each archived document once, replay the
    # cached rows for the later dumps that re-list it.
    parse_cache: dict = {}
    for dump_dir in dumps:
        if already_loaded(conn, dump_dir):
            n_skipped += 1
            log.debug("skipping already-loaded dump %s", dump_dir.name)
            continue
        try:
            conn.execute("BEGIN")
            load_dump(conn, dump_dir, schema_version, parse_cache)
            conn.execute("COMMIT")
            n_loaded += 1
        except Exception:  # noqa: BLE001 — log + rollback + continue
            conn.execute("ROLLBACK")
            log.exception("load failed for %s; skipped", dump_dir.name)
    if not n_loaded:
        _derive_documents_without_a_new_dump(conn, dumps, parse_cache,
                                             supplied=n_supplied)
    conn.close()
    log.info("done: %d dumps loaded, %d already-loaded skipped",
             n_loaded, n_skipped)
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
