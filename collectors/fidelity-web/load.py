#!/usr/bin/env python3
"""
fidelity-web silver loader.

Walks the bronze directory laid down by download.py, applies any
pending schema migrations, and loads each new dump into the silver
SQLite database defined by migrations/0001_initial.sql.

Load semantics
--------------
* Bronze dumps are identified by their YYYYMMDDTHHMMSSZ subdir
  names. Each dump is loaded atomically (one transaction); on
  failure the partial dump is rolled back.
* Already-loaded dumps are skipped via the dump_runs table.
* Positions + accounts + portfolios take a new row per snapshot_at
  (snapshot-table dedup is by PK alone — every dump's view of the
  master data is preserved).
* Transactions UPSERT by synthetic activity_id (SHA-256 prefix of
  the row's promoted columns + source file sha256 + row index).
  Re-loading the same activity window converges.
* Documents are deduped by content_sha256; the first dump that
  observed a file's bytes wins snapshot_at for that row.

After every load run, the loader validates that:
  * Every positions row has an instrument_key.
  * Every non-cash transaction has an instrument_key.
  * Every classified portfolio is logged with its account count.

Failures are logged but don't fail the run — the user is expected
to investigate, fix, and re-load.

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

from collectorkit import cli, silver

# Re-export for backward compatibility with existing tests that call
# load.apply_migrations(...) directly.
apply_migrations = silver.apply_migrations

log = logging.getLogger("fidelity-web.load")


DUMP_DIR_RE = re.compile(r"^\d{8}T\d{6}Z$")
MIGRATION_FILE_RE = re.compile(r"^(\d+)_[a-z0-9_-]+\.sql$", re.IGNORECASE)

# Fidelity activity rows whose Action column starts with one of
# these tokens are pure-cash and legitimately have no instrument.
# Any other Action with an empty Symbol is a validation failure.
CASH_ONLY_ACTIONS = {
    "DEPOSIT", "WITHDRAWAL", "TRANSFER", "TRANSFERRED",
    "INTEREST", "FEE", "JOURNAL", "JOURNALED", "ELECTRONIC",
    "ACH", "WIRE", "DEBIT", "CREDIT", "CHECK",
    "DISTRIBUTION", "CONTRIBUTION", "ROLLOVER",
    "ADJUSTMENT", "ADJUST",
}

# Fidelity selector group labels we know how to classify. Anything
# else falls through to 'other' so the loader doesn't fail on a
# future label change.
PORTFOLIO_KIND = {
    "Education": "529",
    "Authorized": "trust_managed",
}

# Per-portfolio-kind default management style. Fidelity does not
# emit a per-account style indicator (see DESIGN.md §11.6), but
# kind alone pins the style for the categories silver models:
#   529            → self_directed (holder picks the investment
#                    option from the plan menu; no manager).
#   trust_managed  → discretionary (a third-party manager places trades;
#                    custodian executes).
# 'other' / unknown labels stay NULL — gold handles them.
MANAGEMENT_STYLE_BY_KIND = {
    "529": "self_directed",
    "trust_managed": "discretionary",
}


# ============================================================
# CLI
# ============================================================

def parse_args(argv):
    p = argparse.ArgumentParser(description=__doc__.strip())
    p.add_argument("--silver-db", required=True, type=Path,
                   help="Path to the silver SQLite database. Created if missing.")
    p.add_argument("--bronze-dir", required=True, type=Path,
                   help="Directory containing UTC-timestamped bronze dump dirs.")
    p.add_argument("-v", "--verbose", action="store_true",
                   help="DEBUG-level logging.")
    return p.parse_args(argv)


def main(argv=None):
    args = parse_args(argv or sys.argv[1:])
    cli.configure_logging(args.verbose)
    args.silver_db.parent.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(str(args.silver_db))
    conn.execute("PRAGMA foreign_keys = ON")
    try:
        migrations_dir = Path(__file__).parent / "migrations"
        silver.apply_migrations(conn, migrations_dir)
        schema_version = silver.current_schema_version(conn)
        dumps = scan_bronze(args.bronze_dir)
        loaded = skipped = 0
        for dump in dumps:
            if already_loaded(conn, dump):
                log.debug("skipping already-loaded %s", dump.name)
                skipped += 1
                continue
            try:
                conn.execute("BEGIN")
                load_dump(conn, dump, schema_version)
                conn.commit()
                loaded += 1
            except Exception:
                conn.rollback()
                log.exception("load of %s failed; rolled back", dump.name)
        log.info("loaded=%d skipped=%d total=%d",
                 loaded, skipped, len(dumps))
        validate(conn)
    finally:
        conn.close()
    return 0


# ============================================================
# Migrations
# ============================================================

# Schema versioning + the migration runner now live in
# collectorkit.silver (transaction-model agnostic). The silver
# connection is created inline in main() with default isolation.


# ============================================================
# Bronze scan
# ============================================================

def scan_bronze(bronze_dir):
    if not bronze_dir.is_dir():
        raise SystemExit(f"--bronze-dir does not exist: {bronze_dir}")
    return sorted(
        (p for p in bronze_dir.iterdir()
         if p.is_dir() and DUMP_DIR_RE.match(p.name)),
        key=lambda p: p.name,
    )


def already_loaded(conn, dump_dir):
    snapshot_at = ts_from_dir(dump_dir.name)
    cur = conn.execute(
        "SELECT 1 FROM dump_runs WHERE snapshot_at = ?", (snapshot_at,),
    )
    return cur.fetchone() is not None


# ============================================================
# Helpers
# ============================================================

def ts_from_dir(name):
    dt = datetime.strptime(name, "%Y%m%dT%H%M%SZ").replace(tzinfo=timezone.utc)
    return int(dt.timestamp())


def ts_from_iso(s):
    if not s:
        return None
    try:
        dt = datetime.strptime(s, "%Y-%m-%d").replace(tzinfo=timezone.utc)
        return int(dt.timestamp())
    except (TypeError, ValueError):
        return None


def ts_from_mdy(s):
    """Fidelity's CSV dates are MM/DD/YYYY (US) at midnight UTC."""
    if not s or s == "--" or s.strip() == "":
        return None
    try:
        dt = datetime.strptime(s.strip(), "%m/%d/%Y").replace(tzinfo=timezone.utc)
        return int(dt.timestamp())
    except (TypeError, ValueError):
        return None


_NUMERIC_CLEAN_RE = re.compile(r"[\s,$]+")


def parse_decimal(s):
    """Parse a Fidelity-emitted dollar / percent / count cell.

    Strips '$', commas, and whitespace. Returns None for empty,
    '--', or unparseable strings. '+12.34' is fine; '+12.34%' is
    converted to 0.1234 (fraction)."""
    if s is None:
        return None
    raw = s.strip() if isinstance(s, str) else str(s)
    if not raw or raw == "--":
        return None
    is_pct = raw.endswith("%")
    if is_pct:
        raw = raw[:-1]
    cleaned = _NUMERIC_CLEAN_RE.sub("", raw)
    if not cleaned or cleaned in ("+", "-"):
        return None
    try:
        v = float(cleaned)
    except ValueError:
        return None
    return v / 100.0 if is_pct else v


def sha256_of(path):
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(64 * 1024), b""):
            h.update(chunk)
    return h.hexdigest()


def normalize_payload(data):
    # csv.DictReader bundles extra columns (trailing commas) under
    # a None key. Drop those — they're not safely sortable in JSON.
    return json.dumps(
        _strip_none_keys(data),
        ensure_ascii=False, sort_keys=True, default=str,
    )


def _strip_none_keys(data):
    if isinstance(data, dict):
        return {k: _strip_none_keys(v) for k, v in data.items()
                if k is not None}
    if isinstance(data, list):
        return [_strip_none_keys(v) for v in data]
    return data


# ============================================================
# Loaders
# ============================================================

def load_dump(conn, dump_dir, schema_version):
    snapshot_at = ts_from_dir(dump_dir.name)
    log.info("loading %s (snapshot_at=%d)", dump_dir.name, snapshot_at)

    run_meta = _read_run_json(dump_dir)
    _insert_dump_run(conn, snapshot_at, schema_version, dump_dir, run_meta)

    portfolio_count, account_count = _load_master(
        conn, snapshot_at, dump_dir, run_meta,
    )
    pos_count = _load_positions(conn, snapshot_at, dump_dir)
    txn_count = _load_transactions(conn, snapshot_at, dump_dir)
    doc_count = _load_documents(conn, snapshot_at, dump_dir, run_meta)

    log.info(
        "loaded %s: portfolios=%d accounts=%d positions=%d "
        "transactions=%d documents=%d",
        dump_dir.name, portfolio_count, account_count, pos_count,
        txn_count, doc_count,
    )


def _read_run_json(dump_dir):
    path = dump_dir / "run.json"
    if not path.exists():
        return {}
    return json.loads(path.read_text(encoding="utf-8"))


def _insert_dump_run(conn, snapshot_at, schema_version, dump_dir, run_meta):
    cfg = run_meta.get("cli_config", {}) or {}
    window = run_meta.get("activity_window") or {}
    # balances / performance presence is recoverable from the
    # documents table via doc_kind IN ('balances_html',
    # 'performance_html'); we only persist the explicit flags
    # for the structured-data phases.
    conn.execute(
        "INSERT INTO dump_runs ("
        "snapshot_at, silver_schema_version, run_dir, mode, "
        "activity_since, activity_until, "
        "positions_present, activity_present, documents_present"
        ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
        (
            snapshot_at, schema_version, str(dump_dir),
            cfg.get("mode"),
            ts_from_iso(window.get("since")),
            ts_from_iso(window.get("until")),
            int((dump_dir / "positions").is_dir()),
            int((dump_dir / "activity").is_dir()),
            int((dump_dir / "documents").is_dir()),
        ),
    )


# ------------------------------------------------------------
# Master data: portfolios + accounts (from run.json's
# account_dimensions, which captures the account-selector groups
# and per-account nicknames the bronze fetch read at walk-start).
# ------------------------------------------------------------

def _load_master(conn, snapshot_at, dump_dir, run_meta):
    dims = run_meta.get("account_dimensions") or {}
    if not dims:
        log.debug("no account_dimensions in run.json; skipping master load")
        return 0, 0
    # account_dimensions is keyed by sha256-prefix; we want the raw
    # 9-digit id, which lives in accounts_in_scope + accounts_enumerated.
    enumerated = set(run_meta.get("accounts_enumerated") or [])
    in_scope = set(run_meta.get("accounts_in_scope") or [])
    # Build hash → raw-id map.
    hash_to_id = {account_key(aid): aid for aid in enumerated}

    portfolios_inserted = 0
    accounts_inserted = 0
    seen_portfolios = set()
    for hashed, entry in dims.items():
        aid = hash_to_id.get(hashed)
        if not aid:
            log.debug("account_dimensions key %s not in enumerated set", hashed)
            continue
        if aid not in in_scope:
            # DAF / auto-excluded — don't pollute silver.
            continue
        portfolio_ext = entry.get("portfolio")
        nickname = entry.get("nickname")
        kind = PORTFOLIO_KIND.get(portfolio_ext, "other") if portfolio_ext else None
        management_style = MANAGEMENT_STYLE_BY_KIND.get(kind) if kind else None
        if portfolio_ext and portfolio_ext not in seen_portfolios:
            seen_portfolios.add(portfolio_ext)
            conn.execute(
                "INSERT OR REPLACE INTO portfolios ("
                "snapshot_at, portfolio_external_id, kind, payload"
                ") VALUES (?, ?, ?, ?)",
                (snapshot_at, portfolio_ext, kind,
                 normalize_payload({"source": "run.json/account_dimensions"})),
            )
            portfolios_inserted += 1
        conn.execute(
            "INSERT OR REPLACE INTO accounts ("
            "snapshot_at, account_external_id, portfolio_external_id, "
            "nickname, payload, management_style"
            ") VALUES (?, ?, ?, ?, ?, ?)",
            (snapshot_at, aid, portfolio_ext, nickname,
             normalize_payload({"source": "run.json/account_dimensions",
                                "hash": hashed}),
             management_style),
        )
        accounts_inserted += 1
    return portfolios_inserted, accounts_inserted


def account_key(account_external_id):
    """Mirrors download.account_key() — first 16 hex of sha256."""
    return hashlib.sha256(
        account_external_id.encode("ascii"),
    ).hexdigest()[:16]


# ------------------------------------------------------------
# Positions
# ------------------------------------------------------------

POSITIONS_FILES = {
    "summary": "positions_summary.csv",
    "dividend": "positions_dividend.csv",
}


def _load_positions(conn, snapshot_at, dump_dir):
    pos_dir = dump_dir / "positions"
    if not pos_dir.is_dir():
        return 0
    # Parse both views into a (account, instrument) → {view: row} map,
    # then merge into one silver row each. The summary view's columns
    # land first; dividend-view columns merge over.
    merged = {}
    for view, fname in POSITIONS_FILES.items():
        path = pos_dir / fname
        if not path.exists():
            continue
        for row in _iter_positions_rows(path):
            account_ext = row.get("Account Number", "").strip()
            instr = row.get("Symbol", "").strip()
            if not account_ext or not instr:
                continue
            key = (account_ext, instr)
            entry = merged.setdefault(key, {})
            entry[view] = row
    inserted = 0
    for (account_ext, raw_instr), views in merged.items():
        # Strip trailing '*' chars Fidelity appends to money-market
        # core-position symbols (e.g. 'CORE_X**' → 'CORE_X'). The
        # asterisks are a channel signal we promote to the
        # is_core_position flag; the silver instrument_key joins
        # cleanly against transactions where the same fund appears
        # without the suffix.
        is_core = 1 if raw_instr.endswith("*") else 0
        instr = raw_instr.rstrip("*")
        summary = views.get("summary", {})
        dividend = views.get("dividend", {})
        # Prefer summary's quantity/value/cost; fall back to dividend.
        primary = summary or dividend
        description = primary.get("Description") or None
        asset_class = _classify_asset_class(instr, description, is_core)
        conn.execute(
            "INSERT OR REPLACE INTO positions ("
            "snapshot_at, account_external_id, instrument_key, "
            "description, quantity, last_price, current_value, "
            "cost_basis_total, average_cost_basis, type, "
            "ex_date, amount_per_share, pay_date, "
            "distribution_yield, sec_yield, est_annual_income, "
            "payload, currency, asset_class, is_core_position"
            ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
            (
                snapshot_at, account_ext, instr,
                description,
                parse_decimal(primary.get("Quantity")),
                parse_decimal(primary.get("Last Price")),
                parse_decimal(primary.get("Current Value")),
                parse_decimal(summary.get("Cost Basis Total")),
                parse_decimal(summary.get("Average Cost Basis")),
                primary.get("Type") or None,
                ts_from_mdy(dividend.get("Ex-date")),
                parse_decimal(dividend.get("Amount per share")),
                ts_from_mdy(dividend.get("Pay date")),
                parse_decimal(dividend.get("Dist. yield")),
                parse_decimal(dividend.get("SEC yield")),
                parse_decimal(dividend.get("Est. annual income")),
                normalize_payload({"summary": summary, "dividend": dividend}),
                "USD",
                asset_class,
                is_core,
            ),
        )
        inserted += 1
    return inserted


# Regexes for the instrument-shape rules in _classify_asset_class.
# CUSIP-9: 9 alphanumeric chars with a trailing check digit.
_CUSIP9_RE = re.compile(r"^[A-Z0-9]{8}[0-9]$")
# Fidelity's 529-plan investment-option codes: 3 letters + 6 digits
# (an internal Fidelity scheme for target-date / risk-bucket
# sleeves inside a state 529 plan). The strictness matters — some
# ADRs share the first three letters of a state's plan prefix, so
# the regex pins the trailing 6 characters to digits.
_PLAN_FUND_RE = re.compile(r"^[A-Z]{3}[0-9]{6}$")
# Industry mutual-fund convention: 5-char ticker ending in 'X'.
_MUTUAL_FUND_RE = re.compile(r"^[A-Z]{4}X$")


def _classify_asset_class(instrument_key, description, is_core_position):
    """Heuristic asset-class derivation from the Fidelity Symbol +
    Description fields. Order matters — first match wins. Returns
    one of 'money_market' / 'bond' / 'plan_fund' / 'mutual_fund' /
    'equity'. Gold can override via reference data; this populates
    the column for the common cases."""
    if is_core_position:
        return "money_market"
    if not instrument_key:
        return "equity"
    # plan_fund BEFORE the broader CUSIP-9 check because the
    # plan-fund shape ([A-Z]{3}[0-9]{6}) is a stricter subset of
    # CUSIP-9 (any alphanumeric ending in a digit). Real CUSIPs
    # almost never fit the strict 3-letters-then-6-digits pattern.
    if _PLAN_FUND_RE.match(instrument_key):
        return "plan_fund"
    if _CUSIP9_RE.match(instrument_key):
        # 9-char alphanumeric with trailing check digit → CUSIP.
        # Fidelity exposes the CUSIP in the Symbol column for
        # bonds when no ticker exists.
        return "bond"
    if _MUTUAL_FUND_RE.match(instrument_key):
        return "mutual_fund"
    return "equity"


def _iter_positions_rows(csv_path):
    """Yield meaningful holding rows from a Fidelity positions
    export CSV. Stops at the first 'Brokerage services...'
    disclaimer line (Fidelity glues legalese onto the end with
    rows that pollute the data otherwise).

    Some rows have a literal 'Pending Activity' or 'Account Total'
    in the Symbol column — skip those too."""
    SKIP_INSTRUMENTS = {"Pending Activity", "Account Total"}
    with csv_path.open("r", encoding="utf-8-sig", newline="") as f:
        # Read raw lines first so we can stop at the disclaimer.
        text = f.read()
    # Disclaimers start with 'Brokerage services' or '"The data...'.
    cutoff = len(text)
    for marker in ('\n"The data ',
                   '\nBrokerage services',
                   '\nDate downloaded'):
        i = text.find(marker)
        if i != -1 and i < cutoff:
            cutoff = i
    text = text[:cutoff]
    reader = csv.DictReader(text.splitlines())
    for row in reader:
        sym = (row.get("Symbol") or "").strip()
        acct = (row.get("Account Number") or "").strip()
        if not acct:
            continue
        if not sym or sym in SKIP_INSTRUMENTS:
            continue
        yield row


# ------------------------------------------------------------
# Transactions
# ------------------------------------------------------------

ACTIVITY_FILE_RE = re.compile(r"^activity_.*\.csv$")


def _load_transactions(conn, snapshot_at, dump_dir):
    act_dir = dump_dir / "activity"
    if not act_dir.is_dir():
        return 0
    # Cache source-file sha256 per file (each activity CSV is
    # potentially large; one hash per file, then reuse across rows).
    inserted = 0
    for path in sorted(act_dir.iterdir()):
        if not ACTIVITY_FILE_RE.match(path.name):
            continue
        inserted += _ingest_activity_csv(conn, snapshot_at, path)
    return inserted


def _ingest_activity_csv(conn, snapshot_at, csv_path):
    src_sha = sha256_of(csv_path)
    inserted = 0
    with csv_path.open("r", encoding="utf-8-sig", newline="") as f:
        text = f.read()
    # Fidelity prefixes a BOM + blank line before the header on
    # every activity export. Skip everything up to the line that
    # starts with "Run Date,".
    lines = text.splitlines()
    header_idx = next(
        (i for i, ln in enumerate(lines) if ln.startswith("Run Date,")),
        None,
    )
    if header_idx is None:
        log.warning("no 'Run Date,' header in %s; skipping", csv_path.name)
        return 0
    body = "\n".join(lines[header_idx:])
    reader = csv.DictReader(body.splitlines())
    if not reader.fieldnames or "Run Date" not in reader.fieldnames:
        log.warning("unexpected activity CSV header in %s; skipping",
                    csv_path.name)
        return 0
    for i, row in enumerate(reader):
            run_date = (row.get("Run Date") or "").strip()
            # Footer "Date downloaded..." rows surface as a single
            # field that doesn't match Fidelity's data shape.
            if not run_date or not re.match(r"^\d{2}/\d{2}/\d{4}$", run_date):
                continue
            account_ext = (row.get("Account Number") or "").strip()
            if not account_ext:
                continue
            ts = ts_from_mdy(run_date)
            if ts is None:
                continue
            action = (row.get("Action") or "").strip()
            kind = _classify_action(action)
            symbol = (row.get("Symbol") or "").strip() or None
            description = (row.get("Description") or "").strip()
            amount = parse_decimal(row.get("Amount ($)"))
            activity_id = _synthesise_activity_id(
                account_ext, run_date, amount, description,
                symbol, src_sha, i,
            )
            conn.execute(
                "INSERT OR REPLACE INTO transactions ("
                "activity_id, timestamp, account_external_id, kind, "
                "instrument_key, quantity, price, amount, "
                "settlement_date, source_sha256, payload, currency"
                ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                (
                    activity_id, ts, account_ext, kind, symbol,
                    parse_decimal(row.get("Quantity")),
                    parse_decimal(row.get("Price ($)")),
                    amount,
                    ts_from_mdy(row.get("Settlement Date")),
                    src_sha,
                    normalize_payload(dict(row)),
                    "USD",
                ),
            )
            inserted += 1
    return inserted


# Fidelity's Action column is free-text. Pull out the user-
# meaningful action verb so silver can filter by `kind` directly.
# Order matters — most specific first; the catch-all is the first
# whitespace-delimited token, uppercased.
_ACTION_PATTERNS = (
    (re.compile(r"^YOU\s+BOUGHT\b", re.I), "BUY"),
    (re.compile(r"^YOU\s+SOLD\b", re.I), "SELL"),
    (re.compile(r"^REINVESTMENT\b", re.I), "REINVESTMENT"),
    (re.compile(r"^DIVIDEND\s+RECEIVED\b", re.I), "DIVIDEND"),
    (re.compile(r"^DISTRIBUTION\b", re.I), "DISTRIBUTION"),
    (re.compile(r"^MUNI\s+EXEMPT\s+INT\b", re.I), "INTEREST"),
    (re.compile(r"^INTEREST\b", re.I), "INTEREST"),
    (re.compile(r"^FOREIGN\s+TAX\b", re.I), "TAX"),
    (re.compile(r"^FEE\b", re.I), "FEE"),
    (re.compile(r"^PURCHASE\s+INTO\s+CORE", re.I), "CASH_SWEEP_IN"),
    (re.compile(r"^REDEMPTION\s+FROM\s+CORE", re.I), "CASH_SWEEP_OUT"),
    (re.compile(r"^TRANSFERRED\b", re.I), "TRANSFER"),
    (re.compile(r"^JOURNALED\b", re.I), "JOURNAL"),
    (re.compile(r"^IN\s+LIEU\s+OF", re.I), "CASH_IN_LIEU"),
    (re.compile(r"^NAME\s+CHANGE", re.I), "NAME_CHANGE"),
    (re.compile(r"^MERGER\b", re.I), "MERGER"),
    (re.compile(r"^REVERSE\s+SPLIT", re.I), "REVERSE_SPLIT"),
    (re.compile(r"^RETURN\s+OF\s+CAPITAL", re.I), "RETURN_OF_CAPITAL"),
    (re.compile(r"^TENDERED\b", re.I), "TENDER"),
    (re.compile(r"^ADJUST(?:MENT)?\b", re.I), "ADJUSTMENT"),
    (re.compile(r"^ADJ\b", re.I), "ADJUSTMENT"),
    (re.compile(r"^(?:SHORT|LONG)-TERM\s+CAP\b", re.I), "DISTRIBUTION"),
    (re.compile(r"^EXPIRED\b", re.I), "EXPIRATION"),
    (re.compile(r"^REDEMPTION\b", re.I), "REDEMPTION"),
)


def _classify_action(action):
    if not action:
        return "UNKNOWN"
    for pat, kind in _ACTION_PATTERNS:
        if pat.search(action):
            return kind
    return action.split()[0].upper()


def _synthesise_activity_id(account_ext, run_date, amount,
                              description, symbol, src_sha, row_index):
    h = hashlib.sha256()
    parts = [
        account_ext, run_date,
        f"{amount:.6f}" if amount is not None else "",
        description, symbol or "", src_sha, str(row_index),
    ]
    h.update("|".join(parts).encode("utf-8"))
    return h.hexdigest()[:32]


# ------------------------------------------------------------
# Documents
# ------------------------------------------------------------

TAX_FORM_YEAR_RE = re.compile(r"^(\d{4})-")
TRUST_AGREEMENT_TAIL_RE = re.compile(
    r"Example-(\d+)", re.IGNORECASE,
)


def _load_documents(conn, snapshot_at, dump_dir, run_meta):
    """Index every PDF / HTML capture in the dump dir into the
    documents table. Dedups on content_sha256; the first dump that
    surfaced a given file's bytes wins the snapshot_at column."""
    inserted = 0
    # Statements + tax forms.
    docs_dir = dump_dir / "documents"
    if docs_dir.is_dir():
        for pdf in sorted(docs_dir.glob("*.pdf")):
            inserted += _ingest_document(
                conn, snapshot_at, pdf,
                _classify_documents_pdf(pdf.name),
            )
        for other in sorted(docs_dir.iterdir()):
            if other.suffix.lower() == ".csv":
                inserted += _ingest_document(
                    conn, snapshot_at, other,
                    {"doc_kind": "statement", "file_format": "csv"},
                )
    # Balances HTML.
    bal = dump_dir / "balances" / "balances.html"
    if bal.is_file():
        inserted += _ingest_document(
            conn, snapshot_at, bal,
            {"doc_kind": "balances_html", "file_format": "html"},
        )
    # Performance HTML.
    perf = dump_dir / "performance" / "performance.html"
    if perf.is_file():
        inserted += _ingest_document(
            conn, snapshot_at, perf,
            {"doc_kind": "performance_html", "file_format": "html"},
        )
    return inserted


def _classify_documents_pdf(filename):
    """Heuristic kind + tax_year + account for documents/*.pdf
    based on Fidelity's filename conventions:

      Statement<MMDDYYYY>.pdf
          → statement
      <YYYY>-<Nickname>-<NNNN>-Consolidated-Form-1099.pdf
      <YYYY>-<Nickname>-<NNNN>-CORRECTED-Consolidated-Form-1099.pdf
      <YYYY>-<Nickname>-<NNNN>-Form-1099-Q-Instructions.pdf
          → tax_form, tax_year=<YYYY>
    """
    info = {"file_format": "pdf"}
    if filename.lower().startswith("statement"):
        info["doc_kind"] = "statement"
        return info
    m = TAX_FORM_YEAR_RE.match(filename)
    if m:
        info["doc_kind"] = "tax_form"
        info["tax_year"] = int(m.group(1))
    else:
        info["doc_kind"] = "statement"  # fallback
    return info


def _ingest_document(conn, snapshot_at, path, classification):
    sha = sha256_of(path)
    info = dict(classification)
    info.setdefault("file_format", path.suffix.lstrip(".").lower() or "bin")
    info.setdefault("doc_kind", "statement")
    try:
        conn.execute(
            "INSERT INTO documents ("
            "content_sha256, snapshot_at, file_path, file_name, "
            "size_bytes, doc_kind, file_format, tax_year, "
            "account_external_id, payload"
            ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
            (
                sha, snapshot_at, str(path.resolve()), path.name,
                path.stat().st_size, info["doc_kind"], info["file_format"],
                info.get("tax_year"),
                info.get("account_external_id"),
                normalize_payload({"filename": path.name, **info}),
            ),
        )
        return 1
    except sqlite3.IntegrityError:
        # Same bytes already loaded from an earlier dump.
        return 0


# ============================================================
# Validation
# ============================================================

def validate(conn):
    """Log warnings if load invariants are violated:
      * every positions row has an instrument_key (silver schema
        enforces NOT NULL, so this is a counter for visibility)
      * every non-cash transaction has an instrument_key
      * every classified portfolio is logged with its account count
    """
    cur = conn.execute("SELECT COUNT(*) FROM positions")
    pos_total = cur.fetchone()[0]
    cur = conn.execute(
        "SELECT COUNT(*) FROM positions WHERE instrument_key IS NULL"
    )
    pos_no_instr = cur.fetchone()[0]
    log.info("validation: positions total=%d, without instrument_key=%d",
             pos_total, pos_no_instr)

    cur = conn.execute("SELECT COUNT(*) FROM transactions")
    txn_total = cur.fetchone()[0]
    cur = conn.execute(
        "SELECT kind, COUNT(*) FROM transactions "
        "WHERE instrument_key IS NULL "
        "GROUP BY kind ORDER BY COUNT(*) DESC"
    )
    no_instr = cur.fetchall()
    unexpected = [(k, n) for k, n in no_instr
                  if k.upper() not in CASH_ONLY_ACTIONS]
    log.info("validation: transactions total=%d, without instrument_key=%d "
             "(unexpected kinds=%s)",
             txn_total, sum(n for _, n in no_instr), unexpected)
    if unexpected:
        log.warning(
            "validation: %d transaction kinds with NULL instrument_key "
            "fell outside the CASH_ONLY_ACTIONS allowlist: %s",
            len(unexpected), unexpected,
        )

    cur = conn.execute(
        "SELECT p.portfolio_external_id, p.kind, "
        "       COUNT(DISTINCT a.account_external_id) AS account_count "
        "FROM portfolios p "
        "LEFT JOIN accounts a "
        "  ON a.snapshot_at = p.snapshot_at "
        " AND a.portfolio_external_id = p.portfolio_external_id "
        "GROUP BY p.portfolio_external_id, p.kind, p.snapshot_at "
        "ORDER BY p.snapshot_at DESC, p.portfolio_external_id"
    )
    portfolio_rows = cur.fetchall()
    if not portfolio_rows:
        log.warning("validation: no portfolios in silver — "
                    "run.json/account_dimensions was empty in every dump?")
    else:
        seen_kinds = {r[1] for r in portfolio_rows}
        for ext_id, kind, n in portfolio_rows[:10]:
            log.info("validation: portfolio %s (kind=%s) → %d accounts",
                     ext_id, kind, n)
        if "529" not in seen_kinds:
            log.warning("validation: no '529' portfolio found "
                        "(expected the Education group)")
        if "trust_managed" not in seen_kinds:
            log.warning("validation: no 'trust_managed' portfolio found "
                        "(expected the Authorized group)")


if __name__ == "__main__":
    sys.exit(main())
