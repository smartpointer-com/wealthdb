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
  the row's structural identity + a per-file occurrence index; see
  _synthesise_activity_id). The key is file-independent and immune
  to Fidelity's description re-labels, so the same transaction
  re-downloaded across overlapping windows / runs collapses onto
  one row.
* Documents are deduped by content_sha256; the first dump that
  observed a file's bytes wins snapshot_at for that row.

After every load run, the loader validates that:
  * Every positions row has an instrument_key.
  * Every non-cash transaction has an instrument_key.
  * Every classified portfolio is logged with its account count.

Failures are logged but don't fail the run — investigate, fix,
and re-load.

Usage:
    load.py --silver-db <file> --bronze-dir <dir> [-v]
"""

from __future__ import annotations

import argparse
import csv
import hashlib
import json
import logging
import os
import re
import sqlite3
import sys
from concurrent.futures import ProcessPoolExecutor
from datetime import datetime, timezone
from pathlib import Path

from collectorkit import bronze, cli, compress, silver, srcfp

import pdf_parsers
import pdf_parsers_supplied

# Re-export for backward compatibility with existing tests that call
# load.apply_migrations(...) directly.
apply_migrations = silver.apply_migrations

log = logging.getLogger("fidelity-web.load")


# A parse-cache entry must be invalidated whenever the logic that
# produced it changes. srcfp.parser_fingerprint captures that logic
# precisely: the normalised source (comment-, format- and
# docstring-invariant) of the parser's whole first-party import
# closure — pdf_parsers itself plus the collectorkit.pdf extractor and
# the pdf_common helpers it imports, an edit to any of which shifts the
# parsed rows — together with the installed versions of the third-party
# extraction stack (pdfplumber on pdfminer.six, whose >=0.11,<1 pin
# allows a text-shifting minor bump) and the running Python version. It
# returns hex, so it drops straight into a sidecar filename.
_EXTRACTOR_DISTS = ("pdfplumber", "pdfminer.six")

# Parse-cache namespace for the 529 statement parser: a coarse manual
# PARSER_VERSION (a deliberate epoch lever) plus the automatic logic
# fingerprint. The trust parser's namespace additionally folds in the
# signature guard (see _trust_parser_version) because that guard changes
# the parsed rows.
_STATEMENT_PARSER_VERSION = (
    f"stmt529.v{pdf_parsers.PARSER_VERSION}."
    + srcfp.parser_fingerprint([pdf_parsers], _EXTRACTOR_DISTS)
)
_TRUST_PARSER_FINGERPRINT = (
    f"trust.v{pdf_parsers_supplied.PARSER_VERSION}."
    + srcfp.parser_fingerprint([pdf_parsers_supplied], _EXTRACTOR_DISTS)
)


def _logical_bronze_path(path):
    """Strip a compression suffix (`.zst` / `.gz`) so silver records the
    LOGICAL (uncompressed) name/path of a bronze artefact.

    download + recompress may write an HTML/CSV artefact as
    `<name>.zst`; a `documents` row (and every other silver fact) must
    be byte-identical whether the on-disk file is `balances.html` or
    `balances.html.zst`, so the loader keys on the logical name. Plain
    paths (and PDFs, never compressed) pass through unchanged."""
    for suffix in compress.VARIANT_SUFFIXES:
        if path.name.endswith(suffix):
            return path.with_name(path.name[:-len(suffix)])
    return path


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
    # load.py runs HOST-SIDE for fidelity-web (the wrapper's PYTHONPATH
    # shortcut), so the defaults are host paths — not /data.
    p.add_argument("--silver-db", type=Path,
                   default=cli.default_data_root() / "fidelity-web" / "fidelity-web.db",
                   help="Path to the silver SQLite database "
                        "(default: %(default)s). Created if missing.")
    p.add_argument("--bronze-dir", type=Path,
                   default=cli.default_data_root() / "fidelity-web",
                   help="Directory containing UTC-timestamped bronze dump dirs "
                        "(default: %(default)s).")
    p.add_argument(
        "--supplied-statements-dir", type=Path, default=None,
        help=("Directory of user-supplied trust statement PDFs "
              "(filenames `<trust-name> <M>.<YY> Statement.PDF`; "
              "Fidelity's naming convention for legacy monthly "
              "statements). Monthly statements are parsed via "
              "pdf_parsers_supplied and their per-account holdings land "
              "in `historical_position_snapshots`. Trust accounts "
              "are outside the live web-scraper's reach, so this is "
              "the only path to populate their pre-toolkit-era "
              "snapshots. Year-end statements in the same directory "
              "are skipped (redundant with the December monthly "
              "statement). DEFAULT: `<bronze-dir>/supplied-statements` "
              "— keeping the PDFs under the bronze tree makes silver "
              "reproducible from bronze, so `--force` rebuilds and "
              "nightly reloads re-ingest them automatically. No-op "
              "when the directory doesn't exist."),
    )
    p.add_argument(
        "--supplied-statement-signature", type=str, default=None,
        help=("Substring that must appear on a trust statement's "
              "page-1 text (typically the trust's name in upper "
              "case) for the file to be ingested. Defends against "
              "PDFs that match the filename pattern but belong to "
              "an unrelated account (misfiled or sent in "
              "error by Fidelity); mismatched files are logged + "
              "skipped. When omitted, falls back to the first line "
              "of `<supplied-statements-dir>/signature.txt` if present "
              "(keeps the trust name out of argv / shell history "
              "and out of any committed orchestration script). No "
              "guard is applied if neither is supplied."),
    )
    p.add_argument(
        "--parse-cache-dir", type=Path, default=None,
        help=("Directory for the content-addressed PDF parse cache: "
              "parsed statement/trust holdings keyed by content hash + "
              "parser version, so a `--force` rebuild or nightly reload "
              "replays unchanged PDFs instead of re-extracting them. "
              "DEFAULT: `$XDG_CACHE_HOME/wealthdb/fidelity-web/"
              "parse-cache` (falls back to `~/.cache/...`). A derived "
              "cache, kept outside the bronze tree and outside "
              "`~/.secrets`; safe to delete — it is rebuilt on demand."),
    )
    p.add_argument("-v", "--verbose", action="store_true",
                   help="DEBUG-level logging.")
    cli.add_force_arg(p)
    return p.parse_args(argv)


def main(argv=None):
    args = parse_args(argv or sys.argv[1:])
    cli.configure_logging(args.verbose)
    args.silver_db.parent.mkdir(parents=True, exist_ok=True)
    if args.force:
        silver.reset(args.silver_db)
    conn = sqlite3.connect(str(args.silver_db))
    conn.execute("PRAGMA foreign_keys = ON")
    try:
        migrations_dir = Path(__file__).parent / "migrations"
        silver.apply_migrations(conn, migrations_dir)
        schema_version = silver.current_schema_version(conn)
        dumps = scan_bronze(args.bronze_dir)

        # Default the supplied-statements dir to a bronze-resident
        # location so a `--force` rebuild (which wipes silver and
        # reloads from bronze) re-ingests the trust historical
        # snapshots automatically — they'd otherwise be lost, since
        # they live only in silver and are sourced from outside the
        # timestamped-dump tree. Keeping them under <bronze-dir>
        # restores the "silver is reproducible from bronze" invariant.
        # Resolve it (and its signature guard) up front so the trust
        # PDFs join the same shared parse pool as the per-dump
        # statement PDFs.
        trust_dir = args.supplied_statements_dir
        if trust_dir is None:
            trust_dir = args.bronze_dir / "supplied-statements"
        trust_signature = args.supplied_statement_signature
        if trust_signature is None and trust_dir.is_dir():
            trust_signature = _read_signature_sidecar(trust_dir)

        cache = ParseCache(
            args.parse_cache_dir or _default_parse_cache_dir())
        with PdfParseCoordinator(cache, os.cpu_count() or 1) as coord:
            # Enqueue every pending dump's statement PDFs and the trust
            # PDFs, deduplicated by content, then dispatch once so all
            # unique parses run across a single pool (workers import
            # pdfplumber once, all cores stay busy) instead of a fresh
            # pool per dump. Cache hits are never enqueued, so a
            # fully-warm run creates no pool at all.
            for dump in dumps:
                if already_loaded(conn, dump):
                    continue
                for path in _statement_pdf_candidates(dump):
                    coord.enqueue(
                        coord.sha_for(path), _STATEMENT_PARSER_VERSION,
                        _parse_statement_pdf_worker, str(path))
            if schema_version >= 4:
                trust_version = _trust_parser_version(trust_signature)
                for path in _trust_pdf_candidates(trust_dir):
                    coord.enqueue(
                        coord.sha_for(path), trust_version,
                        _parse_supplied_statement_pdf_worker,
                        (str(path), trust_signature))
            coord.dispatch()

            loaded = skipped = 0
            for dump in dumps:
                if already_loaded(conn, dump):
                    log.debug("skipping already-loaded %s", dump.name)
                    skipped += 1
                    continue
                try:
                    conn.execute("BEGIN")
                    load_dump(conn, dump, schema_version, coord)
                    conn.commit()
                    loaded += 1
                except Exception:
                    conn.rollback()
                    log.exception("load of %s failed; rolled back", dump.name)
            log.info("loaded=%d skipped=%d total=%d",
                     loaded, skipped, len(dumps))
            _load_trust_statements_oneshot(
                conn, trust_dir, schema_version,
                signature=trust_signature, coord=coord,
            )
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
    return list(bronze.iter_run_dirs(bronze_dir))


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
    return bronze.parse_run_ts(name)


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

def load_dump(conn, dump_dir, schema_version, coord=None):
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
    hist_pos_count = _load_historical_from_pdfs(conn, dump_dir, coord)

    log.info(
        "loaded %s: portfolios=%d accounts=%d positions=%d "
        "transactions=%d documents=%d hist_positions=%d",
        dump_dir.name, portfolio_count, account_count, pos_count,
        txn_count, doc_count, hist_pos_count,
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
        # download / recompress may have written positions_<view>.csv
        # as .csv.zst; resolve whichever variant is on disk (plain wins).
        path = compress.resolve_variant(pos_dir / fname)
        if path is None:
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
# Word-boundary "ETF" in the Description field. Fidelity groups
# ETFs with stocks (no structured signal), but the fund sponsors
# put "ETF" in the security name itself. The word boundary
# matters: a substring match would also catch N-ETF-LIX.
_ETF_DESC_RE = re.compile(r"\bETF\b", re.IGNORECASE)


def _classify_asset_class(instrument_key, description, is_core_position):
    """Heuristic asset-class derivation from the Fidelity Symbol +
    Description fields. Order matters — first match wins. Returns
    one of 'money_market' / 'bond' / 'plan_fund' / 'mutual_fund' /
    'etf' / 'equity'. Gold can override via the config's
    instrument_overrides; this populates the column for the common
    cases."""
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
    if description and _ETF_DESC_RE.search(description):
        # Exchange-traded funds trade like stocks and Fidelity
        # groups them with equities; the security name is the only
        # signal. Misses name-shy exchange-traded products (e.g. a
        # commodity trust whose name says "SHS") — those are what
        # the gold config's instrument_overrides are for.
        return "etf"
    return "equity"


def _iter_positions_rows(csv_path):
    """Yield meaningful holding rows from a Fidelity positions
    export CSV. Stops at the first 'Brokerage services...'
    disclaimer line (Fidelity glues legalese onto the end with
    rows that pollute the data otherwise).

    Some rows have a literal 'Pending Activity' or 'Account Total'
    in the Symbol column — skip those too."""
    SKIP_INSTRUMENTS = {"Pending Activity", "Account Total"}
    # csv_path may be plain .csv or a .csv.zst variant — open_text
    # decompresses by suffix in memory. Keep utf-8-sig (BOM strip) +
    # newline="" so a compressed load reads byte-identical rows.
    with compress.open_text(csv_path, encoding="utf-8-sig", newline="") as f:
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
    # Collect LOGICAL activity names (strip any .zst/.gz), then resolve
    # each to its on-disk variant — so a plain + .zst twin that coexist
    # (a recompress interrupted between verify and unlink) ingest once,
    # with the plain original winning. ACTIVITY_FILE_RE matches the
    # logical `activity_*.csv` name.
    logical_names = set()
    for path in act_dir.iterdir():
        if not path.is_file():
            continue
        logical = _logical_bronze_path(path)
        if ACTIVITY_FILE_RE.match(logical.name):
            logical_names.add(logical.name)
    inserted = 0
    for name in sorted(logical_names):
        resolved = compress.resolve_variant(act_dir / name)
        if resolved is None:
            continue
        inserted += _ingest_activity_csv(conn, snapshot_at, resolved)
    return inserted


def _ingest_activity_csv(conn, snapshot_at, csv_path):
    # source_sha256 is the DECOMPRESSED content hash, not the on-disk
    # file's hash: a .csv.zst and its plain twin must produce identical
    # transactions rows (the convergence invariant), and source_sha256
    # is a stored column. decompressed_sha256 hashes raw bytes for a
    # plain file, so pre-compression dumps are unaffected.
    src_sha = compress.decompressed_sha256(csv_path)[0]
    inserted = 0
    with compress.open_text(csv_path, encoding="utf-8-sig", newline="") as f:
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
    # Per-file occurrence counter, keyed by the row's structural
    # identity. See _synthesise_activity_id for why this — rather
    # than the file sha256 + global CSV row index — is the dedup key.
    occ_counter: dict[str, int] = {}
    for row in reader:
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
            quantity = parse_decimal(row.get("Quantity"))
            price = parse_decimal(row.get("Price ($)"))
            amount = parse_decimal(row.get("Amount ($)"))
            settlement = ts_from_mdy(row.get("Settlement Date"))
            payload = normalize_payload(dict(row))
            identity = _activity_identity(
                account_ext, ts, kind, symbol, quantity, price, amount,
                settlement)
            occ = occ_counter.get(identity, 0)
            occ_counter[identity] = occ + 1
            activity_id = _synthesise_activity_id(identity, occ)
            conn.execute(
                "INSERT OR REPLACE INTO transactions ("
                "activity_id, timestamp, account_external_id, kind, "
                "instrument_key, quantity, price, amount, "
                "settlement_date, source_sha256, payload, currency"
                ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                (
                    activity_id, ts, account_ext, kind, symbol,
                    quantity, price, amount, settlement,
                    src_sha,
                    payload,
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


def _activity_identity(account_ext, ts, kind, symbol, quantity, price,
                       amount, settlement_ts):
    """Structural fingerprint of one activity row: exactly the
    parsed columns the transactions table stores, none of the
    free-text ones.

    Fidelity re-labels securities between exports — the same
    transaction's Action/Description text drifts (e.g. "SPONSORED
    ADR" one month, an abbreviated form the next), so any text
    column in the identity breaks cross-file dedup. The economics
    of a transaction (who, when, what verb, which symbol, how many,
    at what price, for how much, settling when) never drift, so
    only those participate. Numbers enter parsed (not as raw CSV
    strings) so formatting changes ("1,250.00" vs "1250.00") can't
    split the key either.
    """
    return json.dumps(
        [account_ext, ts, kind, symbol, quantity, price, amount,
         settlement_ts],
        separators=(",", ":"))


def _synthesise_activity_id(identity, occurrence):
    """Stable, file-independent dedup key for one activity row.

    Fidelity's Activity export is consolidated across all accounts
    and the backfill downloads it window-by-window (≤93-day chunks)
    on every run. The SAME real-world transaction therefore lands
    in many distinct CSV files — different windows, different runs.
    The dedup key must depend only on the transaction's intrinsic
    content so those copies collapse onto one row.

    The fingerprint is the row's structural identity (see
    _activity_identity) plus a per-file ``occurrence`` index. The
    occurrence index disambiguates genuinely-repeated identical
    rows within a single export (e.g. two same-day, same-amount
    fills) without breaking cross-file convergence: every file that
    covers a given day sees that day's complete set of rows, so the
    Nth identical copy is assigned the same occurrence index N in
    every file.

    NOTE: two earlier schemes failed. Folding the source-file
    sha256 + global CSV row index into the key made every file's
    copy unique — the table grew a fresh copy of every transaction
    on each backfill run. Hashing the full normalized payload fixed
    that but still leaked Fidelity's mutable description text into
    the identity, so a security re-label between exports duplicated
    its transactions (migration 0005 rebuilt the table onto the
    structural key). source_sha256 is still stored as a column for
    provenance, just not in the identity.
    """
    h = hashlib.sha256()
    h.update(identity.encode("utf-8"))
    h.update(b"|")
    h.update(str(occurrence).encode("ascii"))
    return h.hexdigest()[:32]


# ------------------------------------------------------------
# Documents
# ------------------------------------------------------------

TAX_FORM_YEAR_RE = re.compile(r"^(\d{4})-")


def _load_documents(conn, snapshot_at, dump_dir, run_meta):
    """Index every PDF / HTML capture in the dump dir into the
    documents table. Dedups on content_sha256; the first dump that
    surfaced a given file's bytes wins the snapshot_at column."""
    inserted = 0
    # Statements + tax forms.
    docs_dir = dump_dir / "documents"
    if docs_dir.is_dir():
        # PDFs are never compressed (already internally compressed) —
        # hashed + sized by their raw bytes, unchanged.
        for pdf in sorted(docs_dir.glob("*.pdf")):
            inserted += _ingest_document(
                conn, snapshot_at, pdf,
                _classify_documents_pdf(pdf.name),
            )
        # Statement CSV companions ARE compressible: a `.csv.zst` has
        # suffix `.zst`, so match on the LOGICAL name (strip any
        # .zst/.gz) and resolve the on-disk variant (plain wins). The
        # decompressed-hash path keeps the documents row identical to a
        # plain-CSV load.
        csv_logical = set()
        for other in docs_dir.iterdir():
            if not other.is_file():
                continue
            logical = _logical_bronze_path(other)
            if logical.suffix.lower() == ".csv":
                csv_logical.add(logical.name)
        for name in sorted(csv_logical):
            resolved = compress.resolve_variant(docs_dir / name)
            if resolved is None:
                continue
            inserted += _ingest_document(
                conn, snapshot_at, resolved,
                {"doc_kind": "statement", "file_format": "csv"},
                compressible=True,
            )
    # Balances HTML (compressible: balances.html or balances.html.zst).
    bal = compress.resolve_variant(dump_dir / "balances" / "balances.html")
    if bal is not None:
        inserted += _ingest_document(
            conn, snapshot_at, bal,
            {"doc_kind": "balances_html", "file_format": "html"},
            compressible=True,
        )
    # Performance HTML (compressible).
    perf = compress.resolve_variant(
        dump_dir / "performance" / "performance.html")
    if perf is not None:
        inserted += _ingest_document(
            conn, snapshot_at, perf,
            {"doc_kind": "performance_html", "file_format": "html"},
            compressible=True,
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


def _ingest_document(conn, snapshot_at, path, classification, *,
                     compressible=False):
    """Index one bronze artefact into the documents table.

    PDFs (``compressible=False``, the default — PDFs are never
    compressed) are hashed and sized by their raw on-disk bytes, exactly
    as before.

    Compressible artefacts (HTML / CSV, which download + recompress may
    have written as ``<name>.zst``) are keyed on their DECOMPRESSED
    content: content_sha256 + size_bytes are of the logical uncompressed
    bytes, and file_path / file_name drop the compression suffix. That
    makes a documents row byte-identical whether the file on disk is
    ``balances.html`` or ``balances.html.zst`` — the convergence
    invariant a ``load --force`` on a compressed vs plain bronze tree
    relies on, and the reason the content_sha256 dedup still collapses
    the same artefact across runs regardless of compression state."""
    if compressible:
        sha, size = compress.decompressed_sha256(path)
        logical = _logical_bronze_path(path)
    else:
        sha = bronze.sha256_file(path)[0]
        size = path.stat().st_size
        logical = path
    info = dict(classification)
    info.setdefault("file_format", logical.suffix.lstrip(".").lower() or "bin")
    info.setdefault("doc_kind", "statement")
    try:
        conn.execute(
            "INSERT INTO documents ("
            "content_sha256, snapshot_at, file_path, file_name, "
            "size_bytes, doc_kind, file_format, tax_year, "
            "account_external_id, payload"
            ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
            (
                sha, snapshot_at, str(logical.resolve()), logical.name,
                size, info["doc_kind"], info["file_format"],
                info.get("tax_year"),
                info.get("account_external_id"),
                normalize_payload({"filename": logical.name, **info}),
            ),
        )
        return 1
    except sqlite3.IntegrityError:
        # Same bytes already loaded from an earlier dump.
        return 0


# ============================================================
# Historical position snapshots (529 statement-PDF parsing)
# ============================================================
#
# Fidelity's positions UI is point-in-time; the only available
# source for pre-toolkit-era snapshots is the statement PDF
# archive. Two distinct PDF layouts feed two distinct loader
# paths into the same `historical_position_snapshots` table:
#
#   1. 529 statements — quarterly + year-end PDFs that download.py
#      scrapes into `<dump>/documents/Statement<MMDDYYYY>.pdf`.
#      Text-level parsing in `pdf_parsers.py`. Runs once per dump.
#
#   2. Trust statements — monthly PDFs obtained directly
#      from Fidelity (the web scraper doesn't surface them; see
#      DESIGN.md §4.5). These default to `<bronze-dir>/trust-
#      statements/` (override with `--supplied-statements-dir`) and
#      are read once per load run regardless of dump cadence.
#      Text-level parsing in `pdf_parsers_supplied.py`.
#
#      Keeping the trust PDFs UNDER the bronze tree is deliberate:
#      it preserves the "silver is reproducible from bronze alone"
#      invariant that `silver.reset()` (i.e. `load --force`) relies
#      on. An earlier design sourced them from an arbitrary
#      external dir reachable only via the CLI flag, so every
#      `--force` rebuild or flag-less nightly reload silently
#      dropped the trust history. The bronze-resident default makes
#      the ingest self-healing — no flag, no orchestration change.
#
# PDF text extraction is CPU-bound, so both paths route their PDFs
# through the run's shared PdfParseCoordinator (one ProcessPool + a
# content-addressed parse cache) and insert the resulting rows
# serially. See the coordinator section below.

def _parse_statement_pdf_worker(path):
    """ProcessPoolExecutor target: parse one PDF and return its
    parsed dict (or ``{"_error": "<repr>"}`` so the parent can
    log and continue rather than crashing the whole pool).
    Module-level so it pickles cleanly under spawn (macOS)."""
    try:
        return pdf_parsers.parse_statement_pdf(path)
    except Exception as e:
        return {"_error": repr(e), "path": str(path)}


# ------------------------------------------------------------
# Content-addressed parse cache + shared parse pool
# ------------------------------------------------------------
#
# PDF text extraction is the load's dominant cost. Two mechanisms
# cut it without changing what silver contains:
#
#   * A parse cache keyed on (content_sha256, parser_version).
#     Each unique PDF content is parsed once and its parsed dict
#     replayed on every later sighting — across dumps within one
#     `--force` rebuild (in-process) and across separate load runs
#     (a JSON sidecar), so a nightly reload never re-extracts an
#     unchanged trust statement. The cache stores exactly the
#     parser's output, so a replayed insert is byte-identical to a
#     fresh parse.
#
#   * One ProcessPoolExecutor per load run. Every unique PDF is
#     submitted once as the dumps are scanned and resolved at each
#     dump's insertion point, so workers import pdfplumber once and
#     all cores stay busy — versus a fresh pool per dump (and
#     single-PDF dumps parsed serially in the parent) before.

def _default_parse_cache_dir():
    """Persistent parse-cache location: ``$XDG_CACHE_HOME`` (or
    ``~/.cache``)``/wealthdb/fidelity-web/parse-cache``. A derived
    cache — not source data — so it sits outside the bronze tree
    and outside ``~/.secrets``; content-addressed, so a stale entry
    is never mistaken for a different PDF."""
    base = os.environ.get("XDG_CACHE_HOME") or (Path.home() / ".cache")
    return Path(base) / "wealthdb" / "fidelity-web" / "parse-cache"


class ParseCache:
    """Maps ``(content_sha256, parser_version)`` to a parsed dict via
    an in-process map plus an optional JSON sidecar directory
    (``<dir>/<sha>.<version>.json``).

    ``get`` checks memory then the sidecar; ``put`` writes both.
    Sidecar I/O failures degrade to memory-only (logged at debug),
    so a missing or read-only cache directory never fails a load.
    Bumping a parser's version changes the key, leaving older
    entries unreachable (harmless — they are simply never read)."""

    def __init__(self, sidecar_dir=None):
        self._mem = {}
        self._dir = Path(sidecar_dir) if sidecar_dir is not None else None
        self._dir_ready = None

    def _sidecar_path(self, sha, version):
        if self._dir is None:
            return None
        return self._dir / f"{sha}.{version}.json"

    def get(self, sha, version):
        key = (sha, version)
        cached = self._mem.get(key)
        if cached is not None:
            return cached
        path = self._sidecar_path(sha, version)
        if path is None or not path.is_file():
            return None
        try:
            data = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, ValueError) as e:
            log.debug("parse-cache: unreadable %s: %s", path, e)
            return None
        self._mem[key] = data
        return data

    def put(self, sha, version, parsed):
        self._mem[(sha, version)] = parsed
        path = self._sidecar_path(sha, version)
        if path is None or not self._ensure_dir():
            return
        tmp = path.with_name(path.name + ".tmp")
        try:
            tmp.write_text(json.dumps(parsed, ensure_ascii=False),
                           encoding="utf-8")
            tmp.replace(path)
        except OSError as e:
            log.debug("parse-cache: could not write %s: %s", path, e)
            tmp.unlink(missing_ok=True)

    def _ensure_dir(self):
        if self._dir_ready is None:
            try:
                self._dir.mkdir(parents=True, exist_ok=True)
                self._dir_ready = True
            except OSError as e:
                log.debug("parse-cache: disabled (cannot create %s): %s",
                          self._dir, e)
                self._dir_ready = False
        return self._dir_ready


class PdfParseCoordinator:
    """One ProcessPool + one ParseCache shared across a whole load run.

    Lifecycle: ``enqueue`` every PDF as the dumps are scanned, call
    ``dispatch`` once, then ``resolve`` each PDF at its insertion
    point. Cache hits are never enqueued; work is deduplicated by
    (sha, version); with fewer than two misses no pool is created —
    a tiny corpus or a fully-warm cache parses inline. Used as a
    context manager so the pool is always shut down.

    Direct callers (unit tests) may skip enqueue/dispatch and call
    ``resolve`` straight away — it parses inline when a content has
    neither a cache entry nor a submitted future."""

    def __init__(self, cache, max_workers):
        self._cache = cache
        self._max_workers = max(1, max_workers)
        self._work = {}       # (sha, version) -> (worker, arg)
        self._futures = {}    # (sha, version) -> Future
        self._sha_by_path = {}
        self._pool = None

    def sha_for(self, path):
        """sha256 of a PDF, memoized by path so the same file is
        hashed once across enqueue, resolve and the row insert."""
        key = str(path)
        sha = self._sha_by_path.get(key)
        if sha is None:
            sha = bronze.sha256_file(path)[0]
            self._sha_by_path[key] = sha
        return sha

    def enqueue(self, sha, version, worker, arg):
        if self._cache.get(sha, version) is not None:
            return
        self._work.setdefault((sha, version), (worker, arg))

    def dispatch(self):
        if len(self._work) < 2:
            return  # serial fallback: resolve() parses inline
        workers = min(len(self._work), self._max_workers)
        self._pool = ProcessPoolExecutor(max_workers=workers)
        for key, (worker, arg) in self._work.items():
            self._futures[key] = self._pool.submit(worker, arg)

    def resolve(self, sha, version, worker, arg):
        cached = self._cache.get(sha, version)
        if cached is not None:
            return cached
        future = self._futures.get((sha, version))
        result = future.result() if future is not None else worker(arg)
        # Never cache an error dict — a parse failure or signature
        # mismatch must be re-evaluated on the next run, not pinned.
        if not (isinstance(result, dict) and "_error" in result):
            self._cache.put(sha, version, result)
        return result

    def close(self):
        if self._pool is not None:
            self._pool.shutdown()
            self._pool = None

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        self.close()
        return False


def _transient_coordinator():
    """A pool-less, sidecar-less coordinator for direct callers of
    the loader functions (unit tests): ``resolve`` parses inline and
    nothing persists to disk."""
    return PdfParseCoordinator(ParseCache(sidecar_dir=None), max_workers=1)


def _statement_pdf_candidates(dump_dir):
    """The 529 statement PDFs in a dump's ``documents/`` — files
    matching ``Statement<MMDDYYYY>.pdf`` (the CSV companions carry
    only cash-flow lines, tax forms use a ``<YYYY>-…`` prefix), in
    the sorted order the rows are inserted."""
    docs_dir = dump_dir / "documents"
    if not docs_dir.is_dir():
        return []
    return [
        p for p in sorted(docs_dir.glob("Statement*.pdf"))
        if not p.name.lower().endswith(".csv")
    ]


def _load_historical_from_pdfs(conn, dump_dir, coord=None):
    """Parse every 529 statement PDF in this dump's ``documents/``
    and insert the holdings rows into ``historical_position_snapshots``
    keyed by ``(as_of_date, account_external_id, description)``.

    Parsing goes through the shared ``coord`` (cache + pool); when
    called without one (direct test callers), a transient inline
    coordinator is used. Rows are inserted in the sorted candidate
    order so the ``INSERT OR REPLACE`` keeping the last writer per
    key is deterministic."""
    candidates = _statement_pdf_candidates(dump_dir)
    if not candidates:
        return 0
    coord = coord or _transient_coordinator()
    log.info("historical: ingesting %d statement PDF(s) from %s",
             len(candidates), dump_dir.name)
    inserted = 0
    for path in candidates:
        sha = coord.sha_for(path)
        result = coord.resolve(
            sha, _STATEMENT_PARSER_VERSION,
            _parse_statement_pdf_worker, str(path),
        )
        if "_error" in result:
            log.warning(
                "historical: PDF parse failed for %s: %s",
                path.name, result["_error"],
            )
            continue
        inserted += _insert_historical_rows(conn, path, result, sha)
    return inserted


def _insert_historical_rows(conn, pdf_path, parsed, sha):
    """Insert one ``historical_position_snapshots`` row per
    holding in the parsed statement. Cross-walks the human-
    readable fund description to an `instrument_key` when a
    matching `positions.description` exists in silver; leaves
    `instrument_key` NULL otherwise (gold can resolve)."""
    period_end = parsed.get("period_end")
    if not period_end:
        log.debug(
            "historical: no period in %s; skipping", pdf_path.name,
        )
        return 0
    as_of = ts_from_iso(period_end)
    if as_of is None:
        return 0
    inserted = 0
    for account in parsed.get("accounts", []):
        aid = account.get("account_external_id")
        if not aid:
            continue
        for holding in account.get("holdings", []):
            desc = (holding.get("description") or "").strip()
            if not desc:
                continue
            instrument_key = _crosswalk_description_to_instrument(
                conn, aid, desc,
            )
            try:
                conn.execute(
                    "INSERT OR REPLACE INTO historical_position_snapshots ("
                    "as_of_date, account_external_id, description, "
                    "instrument_key, quantity, price, market_value, "
                    "percent_of_total, currency, source_sha256, payload"
                    ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                    (
                        as_of, aid, desc, instrument_key,
                        holding.get("quantity"),
                        holding.get("price"),
                        holding.get("market_value"),
                        holding.get("percent_of_total"),
                        "USD", sha,
                        normalize_payload(holding),
                    ),
                )
                inserted += 1
            except sqlite3.IntegrityError as e:
                log.debug(
                    "historical: insert skipped for %s @ %s: %s",
                    aid, period_end, e,
                )
    return inserted


def _crosswalk_description_to_instrument(conn, account_external_id,
                                          description):
    """Look the description up against any live ``positions.description``
    for the same account; return the matching ``instrument_key``
    when there's exactly one. NULL when the description never
    appears (e.g. fund was sold before any live snapshot ran) or
    when it's ambiguous."""
    cur = conn.execute(
        "SELECT DISTINCT instrument_key FROM positions "
        " WHERE account_external_id = ? AND description = ? "
        "   AND instrument_key IS NOT NULL "
        " LIMIT 2",
        (account_external_id, description),
    )
    hits = [r[0] for r in cur.fetchall()]
    if len(hits) == 1:
        return hits[0]
    return None


# ------------------------------------------------------------
# Trust statements (legacy monthly statements)
# ------------------------------------------------------------

# Monthly trust statements follow Fidelity's legacy naming
# convention: ``<TrustName> <M>.<YY> Statement.PDF`` (e.g.
# ``Example 1.24 Statement.PDF`` for January 2024). Year-end
# statements in the same directory carry ``Year End`` between the
# trust name and ``Statement.PDF`` (e.g. ``Example 2024 Year End
# Statement.PDF``) — those use a different per-asset-class layout
# the parser doesn't handle yet and are excluded here (the
# December monthly statement covers the same period end).
_TRUST_STATEMENT_FILENAME_RE = re.compile(
    r"^[A-Za-z][A-Za-z\s]*?\s+\d{1,2}\.\d{2}\s+Statement\.pdf$",
    re.IGNORECASE,
)


_TRUST_SIGNATURE_SIDECAR = "signature.txt"


def _read_signature_sidecar(trust_dir):
    """Resolve the trust-statement signature from
    ``<trust_dir>/signature.txt`` (first non-empty, non-``#``-comment
    line). This keeps the trust name — which is PII — in the local
    data directory next to the PDFs, rather than in argv / shell
    history or a committed orchestration script. Returns None when
    the file is absent or carries no usable line."""
    sidecar = trust_dir / _TRUST_SIGNATURE_SIDECAR
    if not sidecar.is_file():
        return None
    try:
        for line in sidecar.read_text(encoding="utf-8").splitlines():
            s = line.strip()
            if s and not s.startswith("#"):
                return s
    except OSError as e:
        log.warning("supplied-statements: could not read %s: %s", sidecar, e)
    return None


def _parse_supplied_statement_pdf_worker(args):
    """ProcessPoolExecutor target: parse one trust PDF and return
    its parsed dict, or ``{"_error": "<repr>"}`` so the parent can
    log and continue. Module-level so it pickles under spawn.

    ``args`` is a ``(path, expected_signature)`` tuple — the pool
    only takes a single argument per call."""
    path, expected_signature = args
    try:
        return pdf_parsers_supplied.parse_supplied_statement_pdf(
            path, expected_signature=expected_signature,
        )
    except Exception as e:
        return {"_error": repr(e), "path": str(path)}


def _trust_parser_version(signature):
    """Parse-cache namespace for the trust parser: the parser-logic
    fingerprint (_TRUST_PARSER_FINGERPRINT) plus a hash of the signature
    guard. The guard is folded in because it changes which lines a
    statement's holdings block yields — a cache entry parsed under one
    guard must not be replayed under another. The signature itself (the
    trust's name, PII) never enters the key, only its hash."""
    sig = hashlib.sha256((signature or "").encode("utf-8")).hexdigest()[:16]
    return f"{_TRUST_PARSER_FINGERPRINT}.{sig}"


def _trust_pdf_candidates(trust_dir):
    """Monthly trust statement PDFs in ``trust_dir`` (matching the
    ``<name> <M>.<YY> Statement.pdf`` convention), in the sorted
    order the rows are inserted. Empty when the dir is None/absent."""
    if trust_dir is None or not trust_dir.is_dir():
        return []
    return [
        p for p in sorted(trust_dir.iterdir())
        if p.is_file() and _TRUST_STATEMENT_FILENAME_RE.match(p.name)
    ]


def _load_trust_statements_oneshot(conn, trust_dir, schema_version, *,
                                    signature=None, coord=None):
    """Load every monthly trust statement from ``trust_dir`` into
    ``historical_position_snapshots``. No-op when ``trust_dir`` is
    None or empty. Idempotent — INSERT OR REPLACE keyed on
    ``(as_of_date, account_external_id, description)`` makes
    re-runs converge.

    Parsing goes through the shared ``coord`` (cache + pool), so a
    nightly reload replays the unchanged statements from cache
    instead of re-extracting them; a transient inline coordinator is
    used when called without one (direct test callers).

    Wrapped in its own transaction so a parser failure on one PDF
    doesn't half-commit and leave silver in an inconsistent state.
    After the inserts land, synthesises a placeholder row in
    ``accounts`` for any trust account that historical statements
    mention but the live scraper hasn't seen; without that row, gold's
    historical-account projection JOIN would drop those positions
    on the floor."""
    if trust_dir is None:
        return
    if not trust_dir.is_dir():
        # The default (<bronze-dir>/supplied-statements) simply not
        # existing is the normal case for deployments without trust
        # accounts — debug, not info, so it isn't noise on every run.
        log.debug("supplied-statements: %s not a directory; skipping", trust_dir)
        return
    if schema_version < 4:
        log.info(
            "supplied-statements: silver schema=%d < 4 (no historical "
            "table); skipping", schema_version,
        )
        return
    if signature is None:
        signature = _read_signature_sidecar(trust_dir)
    if signature is None:
        log.warning(
            "supplied-statements: no signature guard configured "
            "(pass --supplied-statement-signature or add a "
            "signature.txt to %s); ingesting every matching PDF "
            "unverified — a misfiled statement for another account "
            "would be loaded as trust history", trust_dir,
        )
    candidates = _trust_pdf_candidates(trust_dir)
    if not candidates:
        log.info(
            "supplied-statements: no monthly statement PDFs in %s",
            trust_dir,
        )
        return
    coord = coord or _transient_coordinator()
    version = _trust_parser_version(signature)
    log.info("supplied-statements: ingesting %d PDF(s)", len(candidates))
    try:
        conn.execute("BEGIN")
        inserted = skipped = 0
        for path in candidates:
            sha = coord.sha_for(path)
            result = coord.resolve(
                sha, version, _parse_supplied_statement_pdf_worker,
                (str(path), signature),
            )
            err = result.get("_error")
            if err == "signature-mismatch":
                log.warning(
                    "supplied-statements: signature %r not found in %s; "
                    "skipping (likely a misfiled PDF)",
                    result.get("expected_signature"), path.name,
                )
                skipped += 1
                continue
            if err:
                log.warning(
                    "supplied-statements: parse failed for %s: %s",
                    path.name, err,
                )
                skipped += 1
                continue
            inserted += _insert_trust_historical_rows(conn, path, result, sha)
        synth = _synthesize_missing_account_masters(conn)
        conn.commit()
        log.info(
            "supplied-statements: %d holdings rows inserted, %d PDF(s) "
            "skipped, %d account master row(s) synthesised",
            inserted, skipped, synth,
        )
    except Exception:
        conn.rollback()
        log.exception("supplied-statements load failed; rolled back")


def _insert_trust_historical_rows(conn, pdf_path, parsed, sha):
    """Insert one ``historical_position_snapshots`` row per holding
    in the parsed trust statement. The trust parser surfaces the
    ticker (or CUSIP) directly as ``instrument_key`` so no
    cross-walk against live ``positions`` is needed. ``sha`` is the
    source PDF's content hash, stored in ``source_sha256``."""
    period_end = parsed.get("period_end")
    if not period_end:
        log.debug(
            "supplied-statements: no period in %s; skipping", pdf_path.name,
        )
        return 0
    as_of = ts_from_iso(period_end)
    if as_of is None:
        return 0
    inserted = 0
    for account in parsed.get("accounts", []):
        aid = account.get("account_external_id")
        if not aid:
            continue
        for holding in account.get("holdings", []):
            desc = (holding.get("description") or "").strip()
            if not desc:
                continue
            mv = holding.get("market_value")
            total = parsed.get("portfolio_total")  # currently unused
            pct = None
            if mv is not None and total:
                pct = mv / total
            try:
                conn.execute(
                    "INSERT OR REPLACE INTO historical_position_snapshots ("
                    "as_of_date, account_external_id, description, "
                    "instrument_key, quantity, price, market_value, "
                    "percent_of_total, currency, source_sha256, payload"
                    ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                    (
                        as_of, aid, desc,
                        holding.get("instrument_key"),
                        holding.get("quantity"),
                        holding.get("price"),
                        mv, pct, "USD", sha,
                        normalize_payload(holding),
                    ),
                )
                inserted += 1
            except sqlite3.IntegrityError as e:
                log.debug(
                    "supplied-statements: insert skipped for %s @ %s: %s",
                    aid, period_end, e,
                )
    return inserted


# Default portfolio + management classification for an account
# synthesised from supplied historical statements. The defaults are
# the `Authorized` group's: `_load_master` maps that selector label
# to portfolios.kind = 'trust_managed', and the gold adapter
# promotes the kind to TaxWrapperTrustNonGrantor +
# ManagementStyleDiscretionary. The synthesised row pre-fills the
# same shape, so an account that no live download returns still
# rolls up under that portfolio and wrapper.
_TRUST_SYNTHETIC_PORTFOLIO = "Authorized"
_TRUST_SYNTHETIC_KIND = "trust_managed"
_TRUST_SYNTHETIC_MANAGEMENT = "discretionary"


def _synthesize_missing_account_masters(conn):
    """For every ``account_external_id`` mentioned in
    ``historical_position_snapshots`` but absent from
    ``accounts``, insert one synthetic accounts row at the
    account's latest historical ``as_of_date``. Gold's
    `appendHistoricalAccounts` joins on account_external_id alone
    (taking MAX(snapshot_at) per id), so a single row is enough
    for the master projection to fire.

    Returns the number of synthetic rows inserted. Idempotent —
    INSERT OR IGNORE means a re-run after the live download
    finally observes the account is a no-op (the live row's
    snapshot_at outranks the synthetic one, and gold uses MAX)."""
    cur = conn.execute("""
SELECT h.account_external_id, MAX(h.as_of_date)
  FROM historical_position_snapshots h
 WHERE NOT EXISTS (
       SELECT 1 FROM accounts a
        WHERE a.account_external_id = h.account_external_id
 )
 GROUP BY h.account_external_id
""")
    missing = cur.fetchall()
    if not missing:
        return 0
    payload = normalize_payload({"source": "trust-statement-synthetic"})
    inserted = 0
    portfolio_payload = normalize_payload({
        "source": "trust-statement-synthetic",
    })
    for aid, latest_as_of in missing:
        # Portfolio master too: the historical projection joins
        # accounts → portfolios on (snapshot_at, portfolio_external_id),
        # so the synthetic accounts row needs a same-snapshot_at
        # portfolio companion. INSERT OR IGNORE so we don't trample
        # a real portfolio row that the live loader already wrote.
        try:
            conn.execute(
                "INSERT OR IGNORE INTO portfolios ("
                "snapshot_at, portfolio_external_id, kind, payload"
                ") VALUES (?, ?, ?, ?)",
                (latest_as_of, _TRUST_SYNTHETIC_PORTFOLIO,
                 _TRUST_SYNTHETIC_KIND, portfolio_payload),
            )
            conn.execute(
                "INSERT OR IGNORE INTO accounts ("
                "snapshot_at, account_external_id, portfolio_external_id, "
                "nickname, payload, management_style"
                ") VALUES (?, ?, ?, ?, ?, ?)",
                (latest_as_of, aid, _TRUST_SYNTHETIC_PORTFOLIO,
                 None, payload, _TRUST_SYNTHETIC_MANAGEMENT),
            )
            inserted += 1
            log.info(
                "supplied-statements: synthesised accounts master for "
                "%s @ %s (absent from every live download)",
                aid, latest_as_of,
            )
        except sqlite3.IntegrityError as e:
            log.debug(
                "supplied-statements: synthesise skipped for %s: %s", aid, e,
            )
    return inserted


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
