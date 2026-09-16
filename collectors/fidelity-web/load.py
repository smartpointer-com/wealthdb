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
import pdf_parsers_daf
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
# fingerprint. The supplied-statement parser's namespace additionally
# folds in the signature guard (see _supplied_parser_version) because
# that guard changes the parsed rows.
_STATEMENT_PARSER_VERSION = (
    f"stmt529.v{pdf_parsers.PARSER_VERSION}."
    + srcfp.parser_fingerprint([pdf_parsers], _EXTRACTOR_DISTS)
)
_SUPPLIED_PARSER_FINGERPRINT = (
    f"supplied.v{pdf_parsers_supplied.PARSER_VERSION}."
    + srcfp.parser_fingerprint([pdf_parsers_supplied], _EXTRACTOR_DISTS)
)
_DAF_STATEMENT_PARSER_VERSION = (
    f"dafstmt.v{pdf_parsers_daf.PARSER_VERSION}."
    + srcfp.parser_fingerprint([pdf_parsers_daf], _EXTRACTOR_DISTS)
)

# Those three values are also the PARSER GENERATIONS stamped into
# `parser_generations` (migration 0008) once a pass has run, so silver can be
# asked in plain SQL which parser produced the rows it is holding.
#
# All three passes key their rows on text read off the page — a holding's
# description is part of the `historical_position_snapshots` primary key, an
# activity row's is inside its id — so a parser edit re-keys them and
# `INSERT OR REPLACE` has nothing left to replace. The scraped feed already
# paid for this lesson: migration 0005 rebuilt `transactions` because
# Fidelity's mutable description text sat inside the identity and a re-label
# duplicated the rows.
#
# What makes a re-parse REPLACE is applied at whichever grain each row family
# can actually be addressed at:
#
#   * activity rows — scope-wide (`_drop_supplied_activity`), gated on the
#     stamp and held until a statement has actually parsed. The `stmt_`
#     prefix names exactly the supplied pass's rows, so nothing escapes the
#     purge.
#   * holdings rows — per document (`_drop_document_holdings`), every time
#     the document is re-parsed. Three passes share that table and only
#     `source_sha256` says which PDF a row came from, so the document is the
#     largest scope that can be named without reaching into another pass.
STATEMENT_GENERATION_SCOPE = "statement_529"
DAF_GENERATION_SCOPE = "daf_statement"
SUPPLIED_GENERATION_SCOPE = "supplied_statement"


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
    # Donor-Advised Fund event kinds (§12): a grant/gift is cash out to
    # a charity, a pool exchange nets across pools — none carries a
    # security. A CONTRIBUTION of stock DOES carry a CUSIP and is stored
    # with an instrument_key; the cash-contribution case is exempt here.
    "GRANT", "GIFT", "EXCHANGE",
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
#   529            → automated (the plan offers only percentage-wise
#                    allocation across a small menu of funds and
#                    age-based strategies — a model portfolio, not
#                    free security selection).
#   trust_managed  → discretionary (a third-party manager places
#                    trades; custodian executes).
# 'other' / unknown labels stay NULL — gold handles them.
MANAGEMENT_STYLE_BY_KIND = {
    "529": "automated",
    "trust_managed": "discretionary",
    # A Fidelity DAF invests in model pools (§12.3) — automated, like
    # the 529.
    "daf": "automated",
}

# The portfolio grouping the DAF phase lands under. The retail
# portfolios key on the account-selector group label; the DAF bronze
# comes from the charitable API, not the selector, so the loader stamps
# this stable label (matching the selector's 'Fidelity Charitable®
# Giving' section) with kind='daf'.
DAF_PORTFOLIO_LABEL = "Fidelity Charitable® Giving"


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
        help=("Directory of statement PDFs supplied out-of-band "
              "(filenames `<registration> <M>.<YY> Statement.PDF`; "
              "Fidelity's naming convention for legacy monthly "
              "statements). Monthly statements are parsed via "
              "pdf_parsers_supplied and their per-account holdings land "
              "in `historical_position_snapshots`. Accounts whose "
              "statements Fidelity does not serve are outside the live "
              "web-scraper's reach, so this is the only path to "
              "populate their pre-toolkit-era snapshots. Year-end "
              "statements in the same directory are skipped (redundant "
              "with the December monthly statement). DEFAULT: "
              "`<bronze-dir>/supplied-statements` — keeping the PDFs "
              "under the bronze tree makes silver reproducible from "
              "bronze, so `--force` rebuilds and nightly reloads "
              "re-ingest them automatically. No-op when the directory "
              "doesn't exist."),
    )
    p.add_argument(
        "--statement-signature", type=str, default=None,
        help=("Substring that must appear on a supplied statement's "
              "page-1 text (typically the account registration in "
              "upper case) for the file to be ingested. Defends "
              "against PDFs that match the filename pattern but "
              "belong to an unrelated account (misfiled or sent in "
              "error); mismatched files are logged + skipped. When "
              "omitted, falls back to the first line of "
              "`<supplied-statements-dir>/signature.txt` if present "
              "(keeps the registration out of argv / shell history "
              "and out of any committed orchestration script). No "
              "guard is applied if neither is supplied."),
    )
    p.add_argument(
        "--parse-cache-dir", type=Path, default=None,
        help=("Directory for the content-addressed PDF parse cache: "
              "parsed 529 + supplied statement holdings keyed by "
              "content hash + "
              "parser version, so a `--force` rebuild or nightly reload "
              "replays unchanged PDFs instead of re-extracting them. "
              "DEFAULT: `$XDG_CACHE_HOME/wealthdb/fidelity-web/"
              "parse-cache` (falls back to `~/.cache/...`). A derived "
              "cache, kept outside the bronze tree and outside "
              "`~/.secrets`; safe to delete — it is rebuilt on demand."),
    )
    cli.add_standard_args(p, verb="load")
    return p.parse_args(argv)


def main(argv=None):
    args = parse_args(argv or sys.argv[1:])
    cli.configure_logging(args.verbose)
    args.silver_db.parent.mkdir(parents=True, exist_ok=True)
    if args.force:
        silver.reset(args.silver_db)
    conn = sqlite3.connect(str(args.silver_db))
    silver.own_only(args.silver_db)
    conn.execute("PRAGMA foreign_keys = ON")
    try:
        migrations_dir = Path(__file__).parent / "migrations"
        silver.apply_migrations(conn, migrations_dir)
        schema_version = silver.current_schema_version(conn)
        dumps = scan_bronze(args.bronze_dir)

        # Default the supplied-statements dir to a bronze-resident
        # location so a `--force` rebuild (which wipes silver and
        # reloads from bronze) re-ingests the historical snapshots
        # automatically — they'd otherwise be lost, since they live
        # only in silver and are sourced from outside the
        # timestamped-dump tree. Keeping them under <bronze-dir>
        # restores the "silver is reproducible from bronze" invariant.
        # Resolve it (and its signature guard) up front so the
        # supplied PDFs join the same shared parse pool as the
        # per-dump statement PDFs.
        supplied_dir = args.supplied_statements_dir
        if supplied_dir is None:
            supplied_dir = args.bronze_dir / "supplied-statements"
        signature = args.statement_signature
        if signature is None and supplied_dir.is_dir():
            signature = _read_signature_sidecar(supplied_dir)

        cache = ParseCache(
            args.parse_cache_dir or _default_parse_cache_dir())
        with PdfParseCoordinator(cache, os.cpu_count() or 1) as coord:
            # Enqueue every pending dump's statement PDFs and the
            # supplied PDFs, deduplicated by content, then dispatch once so all
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
                if schema_version >= 7:
                    for path in _daf_statement_pdf_candidates(dump):
                        coord.enqueue(
                            coord.sha_for(path),
                            _DAF_STATEMENT_PARSER_VERSION,
                            _parse_daf_statement_pdf_worker, str(path))
            if schema_version >= 4:
                supplied_version = _supplied_parser_version(signature)
                for path in _supplied_pdf_candidates(supplied_dir):
                    coord.enqueue(
                        coord.sha_for(path), supplied_version,
                        _parse_supplied_statement_pdf_worker,
                        (str(path), signature))
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
            _load_supplied_statements_oneshot(
                conn, supplied_dir, schema_version,
                signature=signature, coord=coord,
            )
        validate(conn)
    finally:
        conn.close()
    return 0


# ============================================================
# Bronze scan
# ============================================================

# How far the newest transaction may lag the newest activity-covering
# dump before `validate` says so. Generous on purpose: a quiet month
# is ordinary, and this check is a hint rather than a rule. Three
# weeks is short enough to catch a silent stall well before it becomes
# a season's worth, and long enough that an ordinary lull stays quiet.
TXN_STALE_SECONDS = 21 * 86400


def txn_staleness_days(latest_act_dump, latest_txn):
    """How far the newest transaction trails the newest dump that
    covered the activity phase, or None when nothing is amiss.

    None also when no such dump has ever landed — with nothing
    claiming to have fetched transactions there is nothing to be
    stale against. A dump that landed against an EMPTY transactions
    table is always stale, however long ago it was: an activity phase
    has run and the table has nothing to show for it.
    """
    if latest_act_dump is None:
        return None
    if latest_txn is not None and latest_act_dump - latest_txn <= TXN_STALE_SECONDS:
        return None
    return (latest_act_dump - (latest_txn or 0)) // 86400


def scan_bronze(bronze_dir):
    """The dumps worth loading: every run dir whose manifest does not
    say it is unfinished.

    A status other than `complete` marks a crashed or still-running
    dump whose partial artefacts must not be ingested — the check
    eleven of the fleet's loaders already make, and this one did not.
    A dump with no manifest at all predates the field and is admitted,
    as `bronze.run_status` documents.

    A phase that came back SHORT is not this gate's business: such a
    dump is finished, its status is `complete`, and its artefacts are
    good — what is missing is recorded in the manifest's `coverage`
    block and shouted by the download's exit code."""
    if not bronze_dir.is_dir():
        raise SystemExit(f"--bronze-dir does not exist: {bronze_dir}")
    keep = []
    for run_dir in bronze.iter_run_dirs(bronze_dir):
        status = bronze.run_status(run_dir / "run.json")
        if status not in (None, "complete"):
            log.info("skipping %s: status=%s", run_dir.name, status)
            continue
        keep.append(run_dir)
    return keep


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


def _row_ci(row):
    """Case-insensitive view of a CSV row, keyed on casefolded header
    names. Fidelity re-cased its positions-export headers in 2026-07
    ('Account Number' → 'Account number', 'Last Price' → 'Last price',
    …) which silently zeroed the positions load for weeks; casefolded
    lookups read both eras identically. The original row (source-cased
    keys) still lands in payload."""
    return {(k or "").strip().casefold(): v for k, v in row.items()
            if k is not None}


def _ci_get(ci_row, *names):
    """First present value among casefolded column ``names`` — for
    columns Fidelity has renamed outright (e.g. the dividend view's
    'Dist. yield' → 'Dist. rate')."""
    for name in names:
        v = ci_row.get(name)
        if v is not None:
            return v
    return None


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

    # Positions are all-or-nothing per dump — see
    # _positions_completeness_gate for the invariant and its signals.
    merged = _parse_positions(dump_dir)
    skip_positions, skip_reason = _positions_completeness_gate(
        dump_dir, run_meta, merged, schema_version)
    if skip_positions:
        log.warning(
            "%s: skipping ALL positions from this dump — %s "
            "(partial holdings observation; the positions anchor "
            "stays on the last complete dump)",
            dump_dir.name, skip_reason)
        pos_count = 0
    else:
        pos_count = _load_positions(conn, snapshot_at, merged)
    txn_count = _load_transactions(conn, snapshot_at, dump_dir)
    doc_count = _load_documents(conn, snapshot_at, dump_dir, run_meta)
    hist_pos_count = _load_historical_from_pdfs(conn, dump_dir, coord)

    # Donor-Advised Fund phase (schema v7+ allows portfolios.kind='daf').
    daf = {}
    daf_hist_count = 0
    if schema_version >= 7:
        daf = _load_daf(conn, snapshot_at, dump_dir,
                        skip_positions=skip_positions)
        daf_hist_count = _load_daf_historical(conn, dump_dir, coord)

    log.info(
        "loaded %s: portfolios=%d accounts=%d positions=%d "
        "transactions=%d documents=%d hist_positions=%d%s",
        dump_dir.name, portfolio_count + daf.get("portfolios", 0),
        account_count + daf.get("accounts", 0),
        pos_count + daf.get("positions", 0),
        txn_count + daf.get("transactions", 0),
        doc_count + daf.get("documents", 0),
        hist_pos_count + daf_hist_count,
        (f" [daf: accounts={daf.get('accounts', 0)} "
         f"positions={daf.get('positions', 0)} "
         f"transactions={daf.get('transactions', 0)} "
         f"documents={daf.get('documents', 0)} "
         f"hist_positions={daf_hist_count}]") if daf else "",
    )


def _read_run_json(dump_dir):
    path = dump_dir / "run.json"
    if not path.exists():
        return {}
    return json.loads(path.read_text(encoding="utf-8"))


def _phase_present(dump_dir, subdir):
    """Whether a phase actually PRODUCED something in this dump.

    The directory alone does not answer that. Every walk mkdirs its
    phase directory before its first navigation, so a phase that
    exported nothing leaves an empty directory behind and a flag read
    off ``is_dir()`` records a 1 for it. Fifty-five consecutive
    activity failures were recorded as `activity_present=1` that way —
    exactly the column an audit would have trusted to notice them.

    Whether the phase covered its whole window is a different question
    and lives in run.json's `coverage` block; this flag answers only
    "are there artefacts here"."""
    d = dump_dir / subdir
    if not d.is_dir():
        return 0
    return int(any(d.iterdir()))


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
            _phase_present(dump_dir, "positions"),
            _phase_present(dump_dir, "activity"),
            _phase_present(dump_dir, "documents"),
        ),
    )


# ------------------------------------------------------------
# Master data: portfolios + accounts (from run.json's
# account_dimensions, which captures the account-selector groups
# and per-account nicknames the bronze fetch read at walk-start).
# ------------------------------------------------------------

def _load_master(conn, snapshot_at, dump_dir, run_meta):
    # A dump only vouches for the accounts its phases covered. A
    # mode='daf' dump ran no retail phase — its account_dimensions is
    # an enumeration-only capture of the account selector — so writing
    # retail master rows at that snapshot would pair accounts with no
    # value rows, which latest-snapshot-wins consumers read as the
    # accounts having emptied. Partial coverage must never masquerade
    # as observation (the DAF master rows come from _load_daf, whose
    # phase did run).
    mode = (run_meta.get("cli_config") or {}).get("mode")
    if mode == "daf":
        log.debug("mode=daf dump: retail master load skipped "
                  "(enumeration-only capture)")
        return 0, 0
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


def _parse_positions(dump_dir):
    """Parse both positions views into a (account, instrument) →
    {view: row} map without touching silver — so `load_dump` can judge
    the dump's positions completeness BEFORE anything is inserted (the
    all-or-nothing guard)."""
    pos_dir = dump_dir / "positions"
    merged = {}
    if not pos_dir.is_dir():
        return merged
    for view, fname in POSITIONS_FILES.items():
        # download / recompress may have written positions_<view>.csv
        # as .csv.zst; resolve whichever variant is on disk (plain wins).
        path = compress.resolve_variant(pos_dir / fname)
        if path is None:
            continue
        for row in _iter_positions_rows(path):
            ci = _row_ci(row)
            account_ext = (ci.get("account number") or "").strip()
            instr = (ci.get("symbol") or "").strip()
            if not account_ext or not instr:
                continue
            key = (account_ext, instr)
            entry = merged.setdefault(key, {})
            entry[view] = row
    return merged


def _load_positions(conn, snapshot_at, merged):
    # The summary view's columns land first; dividend-view columns
    # merge over (``merged`` comes from _parse_positions).
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
        # Case-insensitive views for lookups (payload keeps the
        # source-cased rows); 'Dist. yield' / 'Distribution yield …'
        # were renamed to 'rate' spellings in the same 2026-07 format
        # change that re-cased every header.
        ci_sum = _row_ci(summary)
        ci_div = _row_ci(dividend)
        ci_pri = ci_sum or ci_div
        # Prefer summary's quantity/value/cost; fall back to dividend.
        description = ci_pri.get("description") or None
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
                parse_decimal(ci_pri.get("quantity")),
                parse_decimal(ci_pri.get("last price")),
                parse_decimal(ci_pri.get("current value")),
                parse_decimal(ci_sum.get("cost basis total")),
                parse_decimal(ci_sum.get("average cost basis")),
                ci_pri.get("type") or None,
                ts_from_mdy(ci_div.get("ex-date")),
                parse_decimal(ci_div.get("amount per share")),
                ts_from_mdy(ci_div.get("pay date")),
                parse_decimal(_ci_get(ci_div, "dist. yield", "dist. rate")),
                parse_decimal(ci_div.get("sec yield")),
                parse_decimal(ci_div.get("est. annual income")),
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
        ci = _row_ci(row)
        sym = (ci.get("symbol") or "").strip()
        acct = (ci.get("account number") or "").strip()
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
    # is a stored column. read_text_and_sha hashes those decompressed
    # bytes while reading them, so pre-compression dumps are unaffected
    # and the file is decompressed once.
    text, src_sha, _ = compress.read_text_and_sha(csv_path, encoding="utf-8-sig")
    inserted = 0
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
            # Case-insensitive lookups (_row_ci): the 2026-07 format
            # change re-cased the positions headers and silently
            # zeroed that load for weeks — the activity headers were
            # spared then, but the same drift is one release away.
            # Values (which the activity identity hashes) are
            # untouched, so ids are stable across header re-casings.
            ci = _row_ci(row)
            run_date = (ci.get("run date") or "").strip()
            # Footer "Date downloaded..." rows surface as a single
            # field that doesn't match Fidelity's data shape.
            if not run_date or not re.match(r"^\d{2}/\d{2}/\d{4}$", run_date):
                continue
            account_ext = (ci.get("account number") or "").strip()
            if not account_ext:
                continue
            ts = ts_from_mdy(run_date)
            if ts is None:
                continue
            action = (ci.get("action") or "").strip()
            kind = _classify_action(action)
            symbol = (ci.get("symbol") or "").strip() or None
            quantity = parse_decimal(ci.get("quantity"))
            price = parse_decimal(ci.get("price ($)"))
            amount = parse_decimal(ci.get("amount ($)"))
            settlement = ts_from_mdy(ci.get("settlement date"))
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


def _drop_document_holdings(conn, sha):
    """Drop the `historical_position_snapshots` rows one source PDF
    materialised, so re-parsing that PDF REPLACES them instead of leaving
    the old ones beside them.

    A holding's DESCRIPTION — read off the page — is part of that table's
    primary key, so a parser edit re-keys the row and `INSERT OR REPLACE`
    has nothing to replace. Scoped by the document rather than by the pass
    because three passes share the table and nothing on a row says which of
    them wrote it, while `source_sha256` says exactly which PDF did. The
    document is also the only unit any of them can re-derive.

    Called only once a parse has SUCCEEDED. A statement that fails to parse,
    fails its signature guard or fails reconciliation is never dropped:
    stale rows beat no rows when nothing can re-derive them.

    The one case this grain cannot reach: a row whose last writer was a
    statement since removed from bronze carries THAT statement's sha, so the
    statement still present does not delete it and a re-key would leave both.
    Removing a supplied statement is a deliberate act; `load --force` is the
    repair.
    """
    conn.execute(
        "DELETE FROM historical_position_snapshots WHERE source_sha256 = ?",
        (sha,))


def _drop_supplied_activity(conn):
    """Drop every statement-derived activity row, for a re-derivation of all
    of them.

    Scope-wide rather than per-document, because it can be: the `stmt_`
    prefix names exactly the rows the supplied pass writes into
    `transactions`, so unlike the holdings above there is no row this cannot
    reach — including one whose statement has since left the tree. Rows of
    the scraped feed carry structural ids and are untouched.
    """
    return conn.execute(
        r"DELETE FROM transactions WHERE activity_id LIKE 'stmt\_%' "
        r"ESCAPE '\'").rowcount


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
# Donor-Advised Fund (Fidelity Charitable) — §12
# ============================================================
#
# The DAF bronze under <dump>/daf/ is the charitable API's own JSON,
# not the retail CSV/HTML exports. It lands in the SAME silver tables
# as the retail data so the gold adapter composes uniformly:
#   * one portfolio row  (kind='daf')            per dump
#   * one accounts row   (management_style='automated')  per giving account
#   * positions rows      from the investment pools (instrument = poolId)
#   * transactions rows   from grants / contributions / gifts / pool
#                         exchanges / adjustments (stable id per source id)
#   * documents rows      from the statement / confirmation / tax-form PDFs
# The rich per-event fields the retail columns don't model live in the
# JSON `payload`. See DESIGN.md §12.


def _read_json_file(path):
    """Read + parse a JSON bronze file, or None if absent/unreadable."""
    if not path.is_file():
        return None
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (ValueError, OSError) as e:
        log.warning("DAF: could not read %s: %s", path.name, e)
        return None


def _daf_ts(value):
    """Parse a DAF date (ISO 'YYYY-MM-DD', ISO datetime, or US
    'MM/DD/YYYY') to Unix seconds UTC, or None."""
    if not value or not isinstance(value, str):
        return None
    return ts_from_iso(value[:10]) or ts_from_mdy(value)


def _daf_first(rec, keys):
    """First present, non-empty value among ``keys`` in ``rec``."""
    for k in keys:
        v = rec.get(k)
        if v not in (None, "", "--"):
            return v
    return None


def _daf_activity_id(kind, account_ext, source_id, payload):
    """Deterministic transactions PK for a DAF event. Keyed on the
    source's own stable id (grantId, contributionId, …) when present so
    a re-download collapses onto one row; falls back to a hash of the
    account + normalized payload when the source offers no id."""
    basis = (f"{kind}:{account_ext}:{source_id}" if source_id
             else f"{kind}:{account_ext}:{normalize_payload(payload)}")
    return "daf-" + hashlib.sha256(basis.encode("utf-8")).hexdigest()[:28]


# One spec per DAF event file. ``unwrap`` names a nested object the row
# fields live under (grants wrap them in "grant"); ``sign`` fixes the
# amount direction (grants/gifts out = -1, contributions in = +1,
# exchanges/adjustments kept as-is = 0). Candidate key lists absorb
# field-name variation across the endpoints without over-fitting a
# single observed shape.
_DAF_EVENT_SPECS = (
    dict(file="grants.json", kind="GRANT", unwrap="grant", sign=-1,
         id_keys=("grantId",), amount_keys=("amount",),
         date_keys=("approvalDate", "settlementDate", "submitDate",
                    "creationDate", "updateDate"),
         instrument_keys=()),
    dict(file="contributions.json", kind="CONTRIBUTION", unwrap=None, sign=1,
         id_keys=("contributionId", "id"),
         amount_keys=("estimatedAmount", "fmv", "FMV", "netProceeds",
                      "amount"),
         date_keys=("receivedDate", "tradeDate", "settlementDate",
                    "submittedDate", "submitDate"),
         instrument_keys=("cusip", "CUSIP", "symbol", "Symbol")),
    dict(file="gifts.json", kind="GIFT", unwrap=None, sign=-1,
         id_keys=("id", "giftId"), amount_keys=("giftAmount", "amount"),
         date_keys=("processed", "processedDate", "date"),
         instrument_keys=()),
    dict(file="pool_exchanges.json", kind="EXCHANGE", unwrap=None, sign=0,
         id_keys=("id", "exchangeId"), amount_keys=("amount",),
         date_keys=("processDate", "submitDate", "date"),
         instrument_keys=()),
    dict(file="adjustments.json", kind="ADJUSTMENT", unwrap=None, sign=0,
         id_keys=("id", "adjustmentId"), amount_keys=("amount",),
         date_keys=("processed", "processedDate", "date"),
         instrument_keys=()),
)


def _daf_pool_count(dump_dir):
    """Count parseable pool entries across the dump's DAF account
    dirs, without touching silver — the completeness gate's DAF-side
    signal."""
    daf_dir = dump_dir / "daf"
    if not daf_dir.is_dir():
        return 0
    n = 0
    for acct_dir in daf_dir.iterdir():
        if not acct_dir.is_dir():
            continue
        pb = _read_json_file(acct_dir / "pool_balances.json")
        if isinstance(pb, list) and pb:
            n += len((pb[0] or {}).get("poolInfoList") or [])
    return n


def _positions_completeness_gate(dump_dir, run_meta, merged,
                                 schema_version):
    """Decide whether this dump's positions may enter silver at all.
    Returns (skip, reason); (False, None) means the observation is
    complete and both retail and DAF pool rows load.

    A positions-bearing snapshot is read downstream (gold's per-source
    latest-snapshot anchor) as a COMPLETE holdings observation, so a
    dump that observed only one channel must contribute NO positions:
    stale beats partial, and the anchor stays on the last complete
    dump. Events, documents, and master data are unaffected (keyed
    rows, not snapshots). See DESIGN.md §12.3.

    Partiality signals:
      * mode='daf' — the retired DAF-only mode; such legacy dumps
        cover a single account and are partial by construction.
      * daf_results.status='error' — the walk expected a DAF (the
        account selector enumerated one) but the charitable phase
        failed; loading the retail rows alone would zero the pool.
      * retail rows parse to zero while the dump carries DAF pool
        rows and the roster says retail accounts were in scope — the
        retail export failed (or its format drifted); loading the
        pool alone would zero the retail holdings.
    """
    mode = (run_meta.get("cli_config") or {}).get("mode")
    if mode == "daf":
        return True, "legacy mode=daf dump covers only the DAF"
    daf_res = run_meta.get("daf_results")
    if merged and isinstance(daf_res, dict) \
            and daf_res.get("status") == "error":
        return True, "the DAF phase failed while retail positions landed"
    pool_rows = _daf_pool_count(dump_dir) if schema_version >= 7 else 0
    if not merged and pool_rows and (run_meta.get("accounts_in_scope")
                                     or []):
        return True, ("retail positions parsed to zero rows while the "
                      "DAF pool landed")
    return False, None


def _load_daf(conn, snapshot_at, dump_dir, *, skip_positions=False):
    """Load the Donor-Advised Fund bronze (<dump>/daf/) into silver.
    Returns a per-table count dict (empty if the dump has no DAF).
    ``skip_positions`` (the completeness gate) suppresses the pool
    position rows while master / events / documents still load."""
    daf_dir = dump_dir / "daf"
    if not daf_dir.is_dir():
        return {}
    counts = {"portfolios": 0, "accounts": 0, "positions": 0,
              "transactions": 0, "documents": 0}
    portfolio_written = False
    for acct_dir in sorted(p for p in daf_dir.iterdir() if p.is_dir()):
        master = _read_json_file(acct_dir / "account.json")
        if not isinstance(master, dict):
            continue
        account_ext = str(master.get("accountNbr")
                          or master.get("accountNumber") or "").strip()
        if not account_ext:
            continue
        if not portfolio_written:
            conn.execute(
                "INSERT OR REPLACE INTO portfolios ("
                "snapshot_at, portfolio_external_id, kind, payload"
                ") VALUES (?, ?, 'daf', ?)",
                (snapshot_at, DAF_PORTFOLIO_LABEL,
                 normalize_payload({"source": "daf/accounts.json"})),
            )
            portfolio_written = True
            counts["portfolios"] = 1
        conn.execute(
            "INSERT OR REPLACE INTO accounts ("
            "snapshot_at, account_external_id, portfolio_external_id, "
            "nickname, payload, management_style"
            ") VALUES (?, ?, ?, ?, ?, 'automated')",
            (snapshot_at, account_ext, DAF_PORTFOLIO_LABEL,
             master.get("gaName"),
             normalize_payload({"source": "daf/account.json", **master})),
        )
        counts["accounts"] += 1
        if not skip_positions:
            counts["positions"] += _load_daf_pools(
                conn, snapshot_at, account_ext, acct_dir)
        counts["transactions"] += _load_daf_events(
            conn, snapshot_at, account_ext, acct_dir)
        counts["documents"] += _load_daf_documents(
            conn, snapshot_at, account_ext, acct_dir)
    return counts


def _load_daf_pools(conn, snapshot_at, account_ext, acct_dir):
    """Load the investment-pool positions from pool_balances.json. The
    latest date bucket's poolInfoList is the current holding snapshot;
    each pool is one positions row keyed on poolId."""
    pb = _read_json_file(acct_dir / "pool_balances.json")
    if not isinstance(pb, list) or not pb:
        return 0
    bucket = pb[0] or {}
    price_date = bucket.get("poolPriceDate")
    inserted = 0
    for pool in bucket.get("poolInfoList") or []:
        instrument_key = str(pool.get("poolId")
                             or pool.get("poolName") or "").strip()
        if not instrument_key:
            continue
        conn.execute(
            "INSERT OR REPLACE INTO positions ("
            "snapshot_at, account_external_id, instrument_key, "
            "description, quantity, last_price, current_value, "
            "type, currency, asset_class, is_core_position, payload"
            ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'USD', 'daf_pool', 0, ?)",
            (snapshot_at, account_ext, instrument_key,
             pool.get("poolName"),
             parse_decimal(pool.get("unitQuantity")),
             parse_decimal(pool.get("poolUnitPrice")),
             parse_decimal(pool.get("marketValue")),
             pool.get("poolCategory"),
             normalize_payload({"pool_price_date": price_date, **pool})),
        )
        inserted += 1
    return inserted


def _load_daf_events(conn, snapshot_at, account_ext, acct_dir):
    """Load grants / contributions / gifts / pool-exchanges / adjustments
    into the transactions table, one row per event, keyed on a stable
    per-source id (see _daf_activity_id)."""
    inserted = 0
    for spec in _DAF_EVENT_SPECS:
        path = acct_dir / spec["file"]
        items = _read_json_file(path)
        if not isinstance(items, list) or not items:
            continue
        source_sha = bronze.sha256_file(path)[0]
        for item in items:
            if not isinstance(item, dict):
                continue
            rec = item.get(spec["unwrap"]) if spec["unwrap"] else item
            if not isinstance(rec, dict):
                rec = item
            amount = parse_decimal(_daf_first(rec, spec["amount_keys"]))
            if amount is not None and spec["sign"]:
                amount = spec["sign"] * abs(amount)
            ts = _daf_ts(_daf_first(rec, spec["date_keys"])) or snapshot_at
            instr = _daf_first(rec, spec["instrument_keys"])
            source_id = _daf_first(rec, spec["id_keys"])
            activity_id = _daf_activity_id(
                spec["kind"], account_ext, source_id, item)
            conn.execute(
                "INSERT OR REPLACE INTO transactions ("
                "activity_id, timestamp, account_external_id, kind, "
                "instrument_key, amount, currency, source_sha256, payload"
                ") VALUES (?, ?, ?, ?, ?, ?, 'USD', ?, ?)",
                (activity_id, ts, account_ext, spec["kind"],
                 (str(instr).strip() or None) if instr else None,
                 amount, source_sha, normalize_payload(item)),
            )
            inserted += 1
    return inserted


# DAF document filename stems (download._daf_doc_stem): '<TYPE>_<date>',
# TYPE ∈ {STATEMENT, GRANT, CONTRIBUTION, FORM_8283}. The kind is
# namespaced 'daf_*' so DAF documents never mix with the retail
# statement / tax-form rows.
_DAF_DOC_KINDS = {
    "STATEMENT": "daf_statement",
    "FORM_8283": "daf_tax_form",
    "GRANT": "daf_grant_confirmation",
    "CONTRIBUTION": "daf_contribution_confirmation",
}
_DAF_DOC_STEM_RE = re.compile(
    r"^(STATEMENT|FORM_8283|GRANT|CONTRIBUTION)_(\d{4})")


def _classify_daf_pdf(filename, account_ext):
    """Kind + tax_year + account for a DAF documents/*.pdf, from the
    '<TYPE>_<YYYY>_...' stem download.py writes."""
    info = {"file_format": "pdf", "doc_kind": "daf_statement",
            "account_external_id": account_ext}
    m = _DAF_DOC_STEM_RE.match(filename)
    if m:
        info["doc_kind"] = _DAF_DOC_KINDS.get(m.group(1), "daf_statement")
        if info["doc_kind"] == "daf_tax_form":
            info["tax_year"] = int(m.group(2))
    return info


def _load_daf_documents(conn, snapshot_at, account_ext, acct_dir):
    """Index the DAF PDF archive into the documents table (deduped on
    content_sha256 like every other document)."""
    docs_dir = acct_dir / "documents"
    if not docs_dir.is_dir():
        return 0
    inserted = 0
    for pdf in sorted(docs_dir.glob("*.pdf")):
        inserted += _ingest_document(
            conn, snapshot_at, pdf,
            _classify_daf_pdf(pdf.name, account_ext))
    return inserted


# ------------------------------------------------------------
# DAF historical position snapshots (statement-PDF parsing)
# ------------------------------------------------------------
#
# The quarterly + year-end Giving Account statements carry per-pool
# end-of-period units / unit price / market value (DESIGN.md §12),
# so they back-fill `historical_position_snapshots` for the DAF
# exactly the way the 529 statements do for 529 accounts (§4.5) —
# same table, same parse-cache pool, its own parser
# (`pdf_parsers_daf`) and reconciliation gate.


def _parse_daf_statement_pdf_worker(path):
    """ProcessPoolExecutor target — module-level so it pickles
    cleanly under spawn (macOS); errors return a dict rather than
    crashing the pool."""
    try:
        return pdf_parsers_daf.parse_daf_statement_pdf(path)
    except Exception as e:
        return {"_error": repr(e), "path": str(path)}


def _daf_statement_pdf_candidates(dump_dir):
    """The DAF statement PDFs across a dump's giving-account dirs
    (``daf/<account_key>/documents/STATEMENT_*.pdf``), sorted for
    deterministic INSERT OR REPLACE ordering."""
    daf_dir = dump_dir / "daf"
    if not daf_dir.is_dir():
        return []
    return sorted(daf_dir.glob("*/documents/STATEMENT_*.pdf"))


def _daf_account_for_statement(path):
    """The giving-account id owning a statement PDF, from the
    ``account.json`` beside its documents dir; None when absent."""
    master = _read_json_file(path.parents[1] / "account.json")
    if not isinstance(master, dict):
        return None
    aid = str(master.get("accountNbr") or master.get("accountNumber")
              or "").strip()
    return aid or None


def _load_daf_historical(conn, dump_dir, coord=None):
    """Parse every DAF statement PDF in this dump and insert the
    period-end pool rows into ``historical_position_snapshots``. The
    reconciliation gate is enforced here: a statement whose pool sum
    doesn't tie out to its own stated ending value is skipped and
    logged, never imported."""
    candidates = _daf_statement_pdf_candidates(dump_dir)
    if not candidates:
        return 0
    coord = coord or _transient_coordinator()
    log.info("daf historical: ingesting %d statement PDF(s) from %s",
             len(candidates), dump_dir.name)
    inserted = 0
    for path in candidates:
        sha = coord.sha_for(path)
        parsed = coord.resolve(
            sha, _DAF_STATEMENT_PARSER_VERSION,
            _parse_daf_statement_pdf_worker, str(path),
        )
        if "_error" in parsed:
            log.warning("daf historical: parse failed for %s: %s",
                        path.name, parsed["_error"])
            continue
        if not parsed.get("reconciled"):
            log.warning(
                "daf historical: %s failed reconciliation (%s); skipped",
                path.name, parsed.get("reconcile_error"))
            continue
        as_of = ts_from_iso(parsed.get("as_of_date"))
        aid = _daf_account_for_statement(path)
        if as_of is None or not aid:
            log.warning("daf historical: %s missing as-of date or "
                        "account master; skipped", path.name)
            continue
        _drop_document_holdings(conn, sha)
        for pool in parsed.get("pools", []):
            desc = (pool.get("description") or "").strip()
            if not desc:
                continue
            instrument_key = _crosswalk_description_to_instrument(
                conn, aid, desc)
            conn.execute(
                "INSERT OR REPLACE INTO historical_position_snapshots ("
                "as_of_date, account_external_id, description, "
                "instrument_key, quantity, price, market_value, "
                "percent_of_total, currency, source_sha256, payload"
                ") VALUES (?, ?, ?, ?, ?, ?, ?, NULL, 'USD', ?, ?)",
                (
                    as_of, aid, desc, instrument_key,
                    pool.get("quantity"), pool.get("price"),
                    pool.get("market_value"),
                    sha, normalize_payload(pool),
                ),
            )
            inserted += 1
    if inserted:
        silver.stamp_generation(conn, DAF_GENERATION_SCOPE,
                                _DAF_STATEMENT_PARSER_VERSION)
    return inserted


# ============================================================
# Historical position snapshots (529 statement-PDF parsing)
# ============================================================
#
# Fidelity's positions UI is point-in-time; the only available
# source for pre-toolkit-era snapshots is the statement PDF
# archive. Three distinct PDF layouts feed three loader paths into
# the same `historical_position_snapshots` table — the two below,
# plus the DAF Giving Account statements (`_load_daf_historical`,
# beside the other DAF loaders):
#
#   1. 529 statements — quarterly + year-end PDFs that download.py
#      scrapes into `<dump>/documents/Statement<MMDDYYYY>.pdf`.
#      Text-level parsing in `pdf_parsers.py`. Runs once per dump.
#
#   2. Supplied statements — monthly PDFs obtained out-of-band
#      (the web scraper doesn't surface them; see DESIGN.md §4.5).
#      These default to `<bronze-dir>/supplied-statements/`
#      (override with `--supplied-statements-dir`) and are read once
#      per load run regardless of dump cadence. Text-level parsing
#      in `pdf_parsers_supplied.py`.
#
#      Keeping the supplied PDFs UNDER the bronze tree is
#      deliberate: it preserves the "silver is reproducible from
#      bronze alone" invariant that `silver.reset()` (i.e.
#      `load --force`) relies on. Source them from an external dir
#      reachable only via the CLI flag and every `--force` rebuild
#      or flag-less nightly reload silently drops that history. The
#      bronze-resident default makes the ingest self-healing — no
#      flag, no orchestration change.
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
#     unchanged supplied statement. The cache stores exactly the
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
        _drop_document_holdings(conn, sha)
        inserted += _insert_historical_rows(conn, path, result, sha)
    if inserted:
        silver.stamp_generation(conn, STATEMENT_GENERATION_SCOPE,
                                _STATEMENT_PARSER_VERSION)
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
# Supplied statements (legacy monthly statements)
# ------------------------------------------------------------

# Monthly supplied statements follow Fidelity's legacy naming
# convention: ``<Registration> <M>.<YY> Statement.PDF`` (e.g.
# ``Example 1.24 Statement.PDF`` for January 2024). Year-end
# statements in the same directory carry ``Year End`` between the
# registration and ``Statement.PDF`` (e.g. ``Example 2024 Year End
# Statement.PDF``) — those use a different per-asset-class layout
# the parser doesn't handle yet and are excluded here (the
# December monthly statement covers the same period end).
_SUPPLIED_STATEMENT_FILENAME_RE = re.compile(
    r"^[A-Za-z][A-Za-z\s]*?\s+\d{1,2}\.\d{2}\s+Statement\.pdf$",
    re.IGNORECASE,
)


_SIGNATURE_SIDECAR = "signature.txt"


def _read_signature_sidecar(supplied_dir):
    """Resolve the supplied-statement signature from
    ``<supplied_dir>/signature.txt`` (first non-empty, non-``#``-comment
    line). This keeps the account registration — which is PII — in the
    local data directory next to the PDFs, rather than in argv / shell
    history or a committed orchestration script. Returns None when
    the file is absent or carries no usable line."""
    sidecar = supplied_dir / _SIGNATURE_SIDECAR
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
    """ProcessPoolExecutor target: parse one supplied PDF and return
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


def _supplied_parser_version(signature):
    """Parse-cache namespace for the supplied-statement parser: the
    parser-logic fingerprint (_SUPPLIED_PARSER_FINGERPRINT) plus a hash of
    the signature guard. The guard is folded in because it changes which
    lines a statement's holdings block yields — a cache entry parsed under
    one guard must not be replayed under another. The signature itself (the
    account registration, PII) never enters the key, only its hash."""
    sig = hashlib.sha256((signature or "").encode("utf-8")).hexdigest()[:16]
    return f"{_SUPPLIED_PARSER_FINGERPRINT}.{sig}"


def _supplied_pdf_candidates(supplied_dir):
    """Monthly supplied statement PDFs in ``supplied_dir`` (matching the
    ``<name> <M>.<YY> Statement.pdf`` convention), in the sorted
    order the rows are inserted. Empty when the dir is None/absent."""
    if supplied_dir is None or not supplied_dir.is_dir():
        return []
    return [
        p for p in sorted(supplied_dir.iterdir())
        if p.is_file() and _SUPPLIED_STATEMENT_FILENAME_RE.match(p.name)
    ]


def _load_supplied_statements_oneshot(conn, supplied_dir, schema_version, *,
                                      signature=None, coord=None):
    """Load every monthly supplied statement from ``supplied_dir``:
    its holdings into ``historical_position_snapshots`` and its
    account-level activity into ``transactions``. No-op when
    ``supplied_dir`` is None or empty. Idempotent — INSERT OR REPLACE
    keyed on ``(as_of_date, account_external_id, description)`` for
    holdings and on a content-derived id for activity makes re-runs
    converge.

    Parsing goes through the shared ``coord`` (cache + pool), so a
    nightly reload replays the unchanged statements from cache
    instead of re-extracting them; a transient inline coordinator is
    used when called without one (direct test callers).

    Wrapped in its own transaction so a parser failure on one PDF
    doesn't half-commit and leave silver in an inconsistent state.
    After the inserts land, synthesises a placeholder row in
    ``accounts`` for any account that historical statements
    mention but the live scraper hasn't seen; without that row, gold's
    historical-account projection JOIN would drop those positions
    on the floor."""
    if supplied_dir is None:
        return
    if not supplied_dir.is_dir():
        # The default (<bronze-dir>/supplied-statements) simply not
        # existing is the normal case for deployments with no
        # out-of-band statements — debug, not info, so it isn't
        # noise on every run.
        log.debug("supplied-statements: %s not a directory; skipping", supplied_dir)
        return
    if schema_version < 4:
        log.info(
            "supplied-statements: silver schema=%d < 4 (no historical "
            "table); skipping", schema_version,
        )
        return
    if signature is None:
        signature = _read_signature_sidecar(supplied_dir)
    if signature is None:
        log.warning(
            "supplied-statements: no signature guard configured "
            "(pass --statement-signature or add a "
            "signature.txt to %s); ingesting every matching PDF "
            "unverified — a misfiled statement for another account "
            "would be loaded as this account's history", supplied_dir,
        )
    candidates = _supplied_pdf_candidates(supplied_dir)
    if not candidates:
        log.info(
            "supplied-statements: no monthly statement PDFs in %s",
            supplied_dir,
        )
        return
    coord = coord or _transient_coordinator()
    version = _supplied_parser_version(signature)
    log.info("supplied-statements: ingesting %d PDF(s)", len(candidates))
    try:
        conn.execute("BEGIN")
        # Held until the FIRST statement parses, never spent before: a purge
        # ahead of the walk would delete the whole statement-derived ledger
        # on the run where poppler broke or the PDFs became unreadable, and
        # then re-derive nothing. Unspent, it also leaves the generation
        # unstamped, so the next load tries the whole thing again.
        owed_a_purge = silver.stale_generation(
            conn, SUPPLIED_GENERATION_SCOPE, version)
        inserted = skipped = activity = activity_dup = 0
        # One ledger for the whole walk: a feed row absorbed by one
        # statement must not be absorbed again by the next.
        claims = _FeedClaims()
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
            if owed_a_purge:
                dropped = _drop_supplied_activity(conn)
                log.info(
                    "supplied-statements: the parser has changed since these "
                    "rows were written; dropped %d activity row(s) for "
                    "re-derivation", dropped,
                )
                owed_a_purge = False
            _drop_document_holdings(conn, sha)
            inserted += _insert_supplied_historical_rows(conn, path, result, sha)
            act_in, act_dup = _insert_supplied_activity_rows(
                conn, path, result, sha, claims=claims)
            activity += act_in
            activity_dup += act_dup
        synth = _synthesize_missing_account_masters(conn)
        if not owed_a_purge:
            # Either the generation had not moved, or it had and the
            # re-derivation ran. A purge still owed means nothing parsed, so
            # the rows in hand are the older parser's and still the best
            # record there is.
            silver.stamp_generation(conn, SUPPLIED_GENERATION_SCOPE, version)
        conn.commit()
        log.info(
            "supplied-statements: %d holdings rows inserted, %d activity "
            "row(s) inserted (%d already in the scraped feed), %d PDF(s) "
            "skipped, %d account master row(s) synthesised",
            inserted, activity, activity_dup, skipped, synth,
        )
    except Exception:
        conn.rollback()
        log.exception("supplied-statements load failed; rolled back")


# How far a statement row's date may sit from a scraped row's and
# still be the same event. The two describe one payment from either
# side of the settlement, so a day or two of drift is ordinary; three
# is generous without being loose enough for two genuinely different
# payments of the same amount on one account to collide.
_ACTIVITY_MATCH_WINDOW = 3 * 86400


class _FeedClaims:
    """One-to-one bookkeeping for the statement-to-feed match, held for
    the whole supplied pass.

    Without it the match is many-to-one: two statement rows sharing an
    account, an amount and the window both resolve to the single feed
    row that carries one of them, and the other payment is dropped with
    nothing in the log to say so. The shape is not hypothetical: two
    payments can share an account, a day and an amount and still be
    different payments, telling apart only by a reference and a payee
    the scraped feed does not carry.

    Only rows the feed ABSORBED are recorded. A row that found no feed
    match is simply inserted, and its id converges on a re-sighting
    through INSERT OR REPLACE exactly as before, so the ledger can never
    suppress an insert.
    """

    def __init__(self):
        self._by_statement = {}   # statement activity_id -> feed activity_id
        self._taken = set()       # feed activity_ids already absorbed

    def claimed_for(self, statement_id):
        return self._by_statement.get(statement_id)

    def claim(self, statement_id, feed_id):
        self._by_statement[statement_id] = feed_id
        self._taken.add(feed_id)

    def is_taken(self, feed_id):
        return feed_id in self._taken


def _unclaimed_feed_match(conn, account_external_id, amount, ts, claims):
    """The SCRAPED feed row that already carries this payment, or None.

    The statement and the feed number their rows differently and
    cannot be joined on an id, so the match is the only thing both
    agree on: one account, the same signed amount to the cent, within
    `_ACTIVITY_MATCH_WINDOW`. Signed, not absolute — a core
    redemption of +450.00 raises the cash that the -450.00 fee then
    spends, and those two must not cancel each other out.

    Statement-derived rows are excluded from the comparison. They
    converge on their own content-derived id instead, which is what
    lets the monthly and year-end statements both carry a row (the
    year-end repeats the whole year) without inserting it twice.

    Only a feed row nothing has claimed yet counts. The match is a
    resemblance rather than an identity, so unclaimed, the one feed row
    carrying one of a same-day same-amount pair would absorb both and
    the payment the feed never carried would vanish.

    Candidates come back closest-in-time first, then by id, because
    with claiming the pick is no longer arbitrary: which row is taken
    now decides what the next statement row can still find.
    """
    for (feed_id,) in conn.execute(
        "SELECT activity_id FROM transactions "
        "WHERE account_external_id = ? "
        "  AND activity_id NOT LIKE 'stmt\\_%' ESCAPE '\\' "
        "  AND ABS(amount - ?) < 0.005 "
        "  AND ABS(timestamp - ?) <= ? "
        "ORDER BY ABS(timestamp - ?), activity_id",
        (account_external_id, amount, ts, _ACTIVITY_MATCH_WINDOW, ts),
    ):
        if claims.is_taken(feed_id):
            log.info(
                "supplied-statements: feed row %s already absorbed another "
                "statement row; %s %+.2f keeps looking",
                feed_id, account_external_id, amount,
            )
            continue
        return feed_id
    return None


def _insert_supplied_activity_rows(conn, pdf_path, parsed, sha, *, claims=None):
    """Insert the statement's ACCOUNT-LEVEL activity into
    ``transactions`` — the money in and out, and the account fees.

    Returns ``(inserted, skipped)``. Skipped rows are the ones the
    scraped feed already carries; see `_unclaimed_feed_match`. `claims`
    is the pass-wide one-to-one ledger; a caller that omits it gets a
    private one, which is right for a single statement read in
    isolation and wrong for a walk (the walk threads its own).

    The id is derived from the row's own content plus an occurrence
    index within the PDF, on the same reasoning as
    `_synthesise_activity_id`: one real payment printed on both the
    monthly and the year-end statement hashes the same and converges,
    while two genuinely distinct payments that share a day and an
    amount differ in their reference and payee, and so keep separate
    rows.
    """
    inserted = skipped = 0
    claims = _FeedClaims() if claims is None else claims
    occurrence: dict[str, int] = {}
    for account in parsed.get("accounts", []):
        aid = account.get("account_external_id")
        if not aid:
            continue
        for row in account.get("activity", []):
            ts = ts_from_iso(row.get("date"))
            amount = row.get("amount")
            if ts is None or amount is None:
                continue
            amount = float(amount)
            desc = (row.get("description") or "").strip()
            identity = "|".join(
                (aid, row["date"], row.get("section") or "", f"{amount:.2f}", desc))
            occ = occurrence.get(identity, 0)
            occurrence[identity] = occ + 1
            activity_id = "stmt_" + hashlib.sha256(
                f"{identity}|#{occ}".encode("utf-8")).hexdigest()[:28]
            # Keyed on the STATEMENT row's own id, not the feed row's:
            # the year-end statement repeats the whole year, so this
            # payment may be offered again later in the pass and hashes
            # the same both times. The second sighting re-uses the claim
            # the first made rather than reaching for a second feed row.
            absorbed_by = claims.claimed_for(activity_id)
            if absorbed_by is None:
                absorbed_by = _unclaimed_feed_match(
                    conn, aid, amount, ts, claims)
                if absorbed_by is not None:
                    claims.claim(activity_id, absorbed_by)
                    # The description stays out of the line: on a wire it
                    # names the beneficiary. The account, date, section,
                    # signed amount and feed id locate the row.
                    log.info(
                        "supplied-statements: %s %s %s %+.2f is already in "
                        "the scraped feed as %s; not deriving it from %s",
                        aid, row["date"], row.get("section") or "other",
                        amount, absorbed_by, pdf_path.name,
                    )
            if absorbed_by is not None:
                skipped += 1
                continue
            conn.execute(
                "INSERT OR REPLACE INTO transactions ("
                "activity_id, timestamp, account_external_id, kind, "
                "instrument_key, amount, currency, source_sha256, payload"
                ") VALUES (?, ?, ?, ?, NULL, ?, 'USD', ?, ?)",
                (
                    activity_id, ts, aid, row.get("section") or "other",
                    amount, sha,
                    # `Action` is the key gold's payloadNarrative
                    # reads, and the statement's own words are the
                    # action: "Wire Tfr To Bank <ref> <beneficiary>".
                    normalize_payload({
                        "Action": desc,
                        "section": row.get("section"),
                        "basis": "supplied_statement",
                        "statement": pdf_path.name,
                    }),
                ),
            )
            inserted += 1
    return inserted, skipped


def _insert_supplied_historical_rows(conn, pdf_path, parsed, sha):
    """Insert one ``historical_position_snapshots`` row per holding
    in the parsed supplied statement. That parser surfaces the
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
            total = parsed.get("portfolio_total")  # supplied parser emits none
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
# rolls up under that portfolio and wrapper. Groups the web document
# center serves statements for need no synthesis.
_SUPPLIED_SYNTHETIC_PORTFOLIO = "Authorized"
_SUPPLIED_SYNTHETIC_KIND = "trust_managed"
_SUPPLIED_SYNTHETIC_MANAGEMENT = "discretionary"


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
    payload = normalize_payload({"source": "supplied-statement-synthetic"})
    inserted = 0
    portfolio_payload = normalize_payload({
        "source": "supplied-statement-synthetic",
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
                (latest_as_of, _SUPPLIED_SYNTHETIC_PORTFOLIO,
                 _SUPPLIED_SYNTHETIC_KIND, portfolio_payload),
            )
            conn.execute(
                "INSERT OR IGNORE INTO accounts ("
                "snapshot_at, account_external_id, portfolio_external_id, "
                "nickname, payload, management_style"
                ") VALUES (?, ?, ?, ?, ?, ?)",
                (latest_as_of, aid, _SUPPLIED_SYNTHETIC_PORTFOLIO,
                 None, payload, _SUPPLIED_SYNTHETIC_MANAGEMENT),
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

    # Positions staleness: dumps whose mode covers the positions phase
    # keep landing while the retail positions table stops growing —
    # the signature of a silently failing export (format/selector
    # drift; see DESIGN.md §8.3's dated note). Make it loud.
    cur = conn.execute(
        "SELECT MAX(snapshot_at) FROM dump_runs "
        "WHERE mode IN ('all', 'positions')"
    )
    latest_pos_dump = cur.fetchone()[0]
    # Retail positions only — a fresher DAF pool row (its own phase,
    # its own cadence) must not mask a stale retail export.
    cur = conn.execute(
        "SELECT MAX(snapshot_at) FROM positions "
        "WHERE account_external_id NOT IN ("
        "  SELECT DISTINCT a.account_external_id FROM accounts a"
        "  JOIN portfolios p ON p.snapshot_at = a.snapshot_at"
        "   AND p.portfolio_external_id = a.portfolio_external_id"
        "  WHERE p.kind = 'daf')"
    )
    latest_pos_row = cur.fetchone()[0]
    if latest_pos_dump is not None and (
            latest_pos_row is None or latest_pos_row < latest_pos_dump):
        gap_days = (latest_pos_dump - (latest_pos_row or 0)) // 86400
        log.warning(
            "validation: the latest positions-covering dump (%s) carries "
            "NO positions rows — newest positions snapshot is %s "
            "(~%d day(s) behind). The positions export is likely failing "
            "silently (selector drift?); check the newest run.json's "
            "positions_results and re-run with --debug.",
            datetime.fromtimestamp(latest_pos_dump, tz=timezone.utc).date(),
            ("none" if latest_pos_row is None else
             datetime.fromtimestamp(latest_pos_row, tz=timezone.utc).date()),
            gap_days,
        )

    # Transaction staleness, the same shape and for the same reason:
    # dumps whose mode covers the activity phase keep landing while
    # the transactions table stops growing. This is the second line of
    # defence, and it catches the one case the download's exit code
    # cannot — an export that SUCCEEDS but parses to nothing, because
    # a column drifted, which the download side has no way to see.
    #
    # A WARNING and never a gate: unlike a positions dump, which
    # should always yield a holdings snapshot, a quiet account
    # legitimately produces no transaction for weeks, so this is
    # heuristic by construction and would false-positive as a rule.
    # The threshold is generous for that reason.
    cur = conn.execute(
        "SELECT MAX(snapshot_at) FROM dump_runs "
        "WHERE mode IN ('all', 'activity')"
    )
    latest_act_dump = cur.fetchone()[0]
    cur = conn.execute("SELECT MAX(timestamp) FROM transactions")
    latest_txn = cur.fetchone()[0]
    gap_days = txn_staleness_days(latest_act_dump, latest_txn)
    if gap_days is not None:
        log.warning(
            "validation: activity-covering dumps keep landing (latest %s) "
            "but the newest transaction is %s (~%d day(s) behind). Either "
            "the accounts really have been quiet, or the activity export "
            "is failing silently; check the newest run.json's `coverage` "
            "block and re-run with --debug.",
            datetime.fromtimestamp(latest_act_dump, tz=timezone.utc).date(),
            ("none" if latest_txn is None else
             datetime.fromtimestamp(latest_txn, tz=timezone.utc).date()),
            gap_days,
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
            log.info("validation: no '529' portfolio derived from "
                     "this dump (no Education group present)")
        if "trust_managed" not in seen_kinds:
            log.info("validation: no 'trust_managed' portfolio derived "
                     "from this dump (no Authorized group present)")


if __name__ == "__main__":
    sys.exit(main())
