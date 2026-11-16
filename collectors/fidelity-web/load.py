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
import hashlib
import json
import logging
import os
import sqlite3
import sys
from datetime import datetime, timezone
from pathlib import Path

from collectorkit import bronze, cli, parsecache, silver, srcfp
from collectorkit.parsecache import ParseCache, ParsePool
from collectorkit.parse import iso_date_to_epoch

import pdf_parsers
import pdf_parsers_1099
import pdf_parsers_daf
import pdf_parsers_supplied

# Each family of silver rows has a module of its own: silver_retail
# (master, positions, activity, documents), silver_lots, silver_daf,
# silver_statements and silver_1099. This module holds the load frame
# and the document passes, the code that knows a parser's generation.
from silver_1099 import (
    _consolidated_1099_candidates, _file_1099_documents, _held_1099_forms,
    _latest_1099_copies, _replace_1099_lots,
)
from silver_daf import (
    _daf_account_for_statement, _daf_statement_pdf_candidates, _load_daf,
    _positions_completeness_gate,
)
from silver_lots import (
    _drop_supplied_closed_lots, _insert_supplied_closed_lots,
    _load_closed_positions, _load_open_lots, _read_lot_step,
)
from silver_retail import (
    _fill_instrument_keys, _load_documents, _load_master, _load_positions,
    _load_transactions, _parse_positions, normalize_payload,
)
from silver_statements import (
    _crosswalk_description_to_instrument, _drop_document_holdings,
    _drop_supplied_activity, _insert_historical_rows,
    _insert_supplied_activity_rows, _insert_supplied_historical_rows,
    _read_signature_sidecar, _statement_pdf_candidates,
    _supplied_pdf_candidates, _synthesize_missing_account_masters,
)

# Names the tests, and the kit's comparisons against this collector,
# read through `load`.
from silver_daf import DAF_PORTFOLIO_LABEL  # noqa: F401
from silver_retail import (  # noqa: F401
    MANAGEMENT_STYLE_BY_KIND, _activity_identity, _classify_action,
    _classify_asset_class, _classify_documents_pdf, _ingest_document,
    _synthesise_activity_id, instrument_from_action, parse_decimal,
    ts_from_mdy,
)
from silver_statements import (  # noqa: F401
    _ACTIVITY_MATCH_DAYS, _FEE_MATCH_DAYS, _claim_feed_match, _feed_legs,
)

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
# The Consolidated 1099 pass reads each PDF twice under two namespaces:
# first only which form it is, then the lots of the copy it keeps.
_CONSOLIDATED_1099_PARSER_VERSION = (
    f"1099.v{pdf_parsers_1099.PARSER_VERSION}."
    + srcfp.parser_fingerprint([pdf_parsers_1099], _EXTRACTOR_DISTS)
)
_CONSOLIDATED_1099_IDENTITY_VERSION = (
    _CONSOLIDATED_1099_PARSER_VERSION + ".identity")

# Those values are also the PARSER GENERATIONS stamped into
# `parser_generations` (migration 0008) once a pass has run, so silver can be
# asked in plain SQL which parser produced the rows it is holding.
#
# The three statement passes key their rows on text read off the page — a
# holding's description is part of the `historical_position_snapshots`
# primary key, an activity row's is inside its id — so a parser edit re-keys
# them and `INSERT OR REPLACE` has nothing left to replace. The scraped feed
# already paid for this lesson: migration 0005 rebuilt `transactions` because
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
#   * closed lots — by `document_kind`, which names the pass that wrote
#     them: the supplied pass's with its activity rows
#     (`_drop_supplied_closed_lots`), and the 1099 pass's when its own
#     generation moves (`_load_consolidated_1099s`), which is also what
#     sends that pass back over every dump in bronze. The positions
#     page's closed lots need no generation: each fetch replaces its
#     closed position's rows (`_load_closed_positions`).
STATEMENT_GENERATION_SCOPE = "statement_529"
DAF_GENERATION_SCOPE = "daf_statement"
SUPPLIED_GENERATION_SCOPE = "supplied_statement"
CONSOLIDATED_1099_GENERATION_SCOPE = "consolidated_1099"


# Fidelity activity rows whose Action column starts with one of
# these tokens are pure-cash and legitimately have no instrument.
# Any other Action with an empty Symbol is a validation failure.
CASH_ONLY_ACTIONS = {
    "DEPOSIT", "WITHDRAWAL", "TRANSFER", "TRANSFERRED",
    "INTEREST", "FEE", "JOURNAL", "JOURNALED", "ELECTRONIC",
    "ACH", "WIRE", "DEBIT", "CREDIT", "CHECK",
    "DISTRIBUTION", "CONTRIBUTION", "ROLLOVER",
    "ADJUSTMENT", "ADJUST",
    # Fee and tax lines whose first word is not FEE: an advisory fee, an
    # asset-based fee, an IRS direct debit or deposit.
    "ADVISOR", "ASSET", "DIRECT",
    # Donor-Advised Fund event kinds (§12): a grant/gift is cash out to
    # a charity, a pool exchange nets across pools — none carries a
    # security. A CONTRIBUTION of stock DOES carry a CUSIP and is stored
    # with an instrument_key; the cash-contribution case is exempt here.
    "GRANT", "GIFT", "EXCHANGE",
}


# ============================================================
# CLI
# ============================================================

def parse_args(argv):
    p = argparse.ArgumentParser(description=__doc__.strip())
    # load.py runs HOST-SIDE for fidelity-web (the wrapper runs it with
    # the collector's .venv python), so the defaults are host paths —
    # not /data.
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
              "pdf_parsers_supplied: their holdings land in "
              "`historical_position_snapshots`, their account-level "
              "activity in `transactions` and their sales in "
              "`closed_lots` (DESIGN.md §4.5). Accounts whose "
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
              "every parsed statement and Consolidated 1099, keyed by "
              "content hash + parser version, so a `--force` rebuild or "
              "nightly reload replays unchanged PDFs instead of "
              "re-extracting them. "
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

        pending = [d for d in dumps if not already_loaded(conn, d)]
        dumps_1099, rederive_1099s = _dumps_for_1099s(conn, dumps, pending)

        cache = ParseCache(
            args.parse_cache_dir or parsecache.default_dir("fidelity-web"))
        with ParsePool(cache, os.cpu_count() or 1) as coord:
            # Enqueue every pending dump's statement PDFs and the
            # supplied PDFs, deduplicated by content, then dispatch once so all
            # unique parses run across a single pool (workers import
            # pdfplumber once, all cores stay busy) instead of a fresh
            # pool per dump. Cache hits are never enqueued, so a
            # fully-warm run creates no pool at all. A Consolidated 1099
            # is enqueued for its identity only: which copies get parsed
            # in full is known once every identity is.
            for dump in dumps_1099:
                for path in _consolidated_1099_candidates(dump):
                    coord.enqueue(
                        coord.sha_for(path),
                        _CONSOLIDATED_1099_IDENTITY_VERSION,
                        _read_1099_identity_worker, str(path))
            for dump in pending:
                for path in _statement_pdf_candidates(dump):
                    coord.enqueue(
                        coord.sha_for(path), _STATEMENT_PARSER_VERSION,
                        _parse_statement_pdf_worker, str(path))
                for path in _daf_statement_pdf_candidates(dump):
                    coord.enqueue(
                        coord.sha_for(path), _DAF_STATEMENT_PARSER_VERSION,
                        _parse_daf_statement_pdf_worker, str(path))
            supplied_version = _supplied_parser_version(signature)
            for path in _supplied_pdf_candidates(supplied_dir):
                coord.enqueue(
                    coord.sha_for(path), supplied_version,
                    _parse_supplied_statement_pdf_worker,
                    (str(path), signature))
            coord.dispatch()

            loaded = 0
            for dump in pending:
                try:
                    conn.execute("BEGIN")
                    load_dump(conn, dump, schema_version, coord)
                    conn.commit()
                    loaded += 1
                except Exception:
                    conn.rollback()
                    log.exception("load of %s failed; rolled back", dump.name)
            log.info("loaded=%d skipped=%d total=%d",
                     loaded, len(dumps) - len(pending), len(dumps))
            filled = _fill_instrument_keys(conn)
            conn.commit()
            if filled:
                log.info("activity: %d row(s) took their instrument from "
                         "the CUSIP in their action text", filled)
            if dumps_1099:
                _load_consolidated_1099s(
                    conn, dumps_1099, rederive=rederive_1099s, coord=coord)
            _load_supplied_statements_oneshot(
                conn, supplied_dir, signature=signature, coord=coord,
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
    """The dumps worth loading: every run dir with a manifest that does
    not say it is unfinished (`bronze.run_is_loadable`).

    A status other than `complete` marks a crashed or still-running
    dump whose partial artefacts must not be ingested. A manifest with
    no `status` key predates the field and is admitted; a dir with no
    manifest is still being written and is skipped.

    A phase that came back SHORT is not this gate's business: such a
    dump is finished, its status is `complete`, and its artefacts are
    good — what is missing is recorded in the manifest's `coverage`
    block and shouted by the download's exit code."""
    if not bronze_dir.is_dir():
        raise SystemExit(f"--bronze-dir does not exist: {bronze_dir}")
    keep = []
    for run_dir in bronze.iter_run_dirs(bronze_dir):
        if not bronze.run_is_loadable(run_dir):
            run_json = run_dir / "run.json"
            if run_json.is_file():
                log.info("skipping %s: status=%s", run_dir.name,
                         bronze.run_status(run_json))
            else:
                log.info("skipping %s: no run.json (still writing?)",
                         run_dir.name)
            continue
        keep.append(run_dir)
    return keep


def already_loaded(conn, dump_dir):
    return silver.snapshot_loaded(conn, ts_from_dir(dump_dir.name))


# ============================================================
# Helpers
# ============================================================

def ts_from_dir(name):
    return bronze.parse_run_ts(name)


# ============================================================
# Loaders
# ============================================================

def load_dump(conn, dump_dir, schema_version, coord=None):
    snapshot_at = ts_from_dir(dump_dir.name)
    log.info("loading %s (snapshot_at=%d)", dump_dir.name, snapshot_at)

    run_meta = _read_run_json(dump_dir)
    # Positions are all-or-nothing per dump — see
    # _positions_completeness_gate for the invariant and its signals.
    # The gate runs first: a dump whose positions it holds back does not
    # count as one that observed them.
    merged = _parse_positions(dump_dir)
    skip_positions, skip_reason = _positions_completeness_gate(
        dump_dir, run_meta, merged)
    _insert_dump_run(conn, snapshot_at, schema_version, dump_dir, run_meta,
                     positions_present=(0 if skip_positions else
                                        _phase_present(dump_dir, "positions")))

    portfolio_count, account_count = _load_master(
        conn, snapshot_at, run_meta,
    )

    if skip_positions:
        log.warning(
            "%s: skipping ALL positions from this dump — %s "
            "(partial holdings observation; the positions anchor "
            "stays on the last complete dump)",
            dump_dir.name, skip_reason)
        pos_count = 0
    else:
        pos_count = _load_positions(conn, snapshot_at, merged)
    lot_step = _read_lot_step(dump_dir, run_meta)
    lot_count = (_load_open_lots(conn, snapshot_at, dump_dir.name, lot_step)
                 if lot_step else 0)
    closed_lot_count = (_load_closed_positions(conn, dump_dir.name, lot_step)
                        if lot_step else 0)
    txn_count = _load_transactions(conn, dump_dir)
    doc_count = _load_documents(conn, snapshot_at, dump_dir)
    hist_pos_count = _load_historical_from_pdfs(conn, dump_dir, coord)

    # Donor-Advised Fund phase.
    daf = _load_daf(conn, snapshot_at, dump_dir,
                    skip_positions=skip_positions)
    daf_hist_count = _load_daf_historical(conn, dump_dir, coord)

    log.info(
        "loaded %s: portfolios=%d accounts=%d positions=%d open_lots=%d "
        "closed_lots=%d transactions=%d documents=%d hist_positions=%d%s",
        dump_dir.name, portfolio_count + daf.get("portfolios", 0),
        account_count + daf.get("accounts", 0),
        pos_count + daf.get("positions", 0), lot_count, closed_lot_count,
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
    off ``is_dir()`` would record a 1 for it — hiding a failed phase in
    exactly the column an audit would trust to notice it.

    Whether the phase covered its whole window is a different question
    and lives in run.json's `coverage` block; this flag answers only
    "are there artefacts here"."""
    d = dump_dir / subdir
    if not d.is_dir():
        return 0
    return int(any(d.iterdir()))


def _insert_dump_run(conn, snapshot_at, schema_version, dump_dir, run_meta,
                     *, positions_present):
    cfg = run_meta.get("cli_config", {}) or {}
    window = run_meta.get("activity_window") or {}
    # balances / performance presence is recoverable from the
    # documents table via doc_kind IN ('balances_html',
    # 'performance_html'); only the explicit flags are persisted
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
            iso_date_to_epoch(window.get("since")),
            iso_date_to_epoch(window.get("until")),
            positions_present,
            _phase_present(dump_dir, "activity"),
            _phase_present(dump_dir, "documents"),
        ),
    )


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
        as_of = iso_date_to_epoch(parsed.get("as_of_date"))
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
# through the run's shared ParsePool (one process pool + a
# content-addressed parse cache) and insert the resulting rows
# serially. See the parse cache section below.

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
# The parse cache and the parse pool
# ------------------------------------------------------------
#
# PDF text extraction is the load's dominant cost. The kit's
# `collectorkit.parsecache` cuts it without changing what silver holds:
# a ParseCache keyed on (content_sha256, parser_version), whose JSON
# sidecar replays a parsed dict on every later sighting of the same
# bytes (within one run, across runs, and across a `--force` rebuild),
# and one ParsePool per load run that parses every unique miss once
# across all cores. The sidecar is `<sha>.<version>.json`, so a cache
# stays warm across a change of the code around it. An error result is
# never cached.

def _transient_coordinator():
    """A pool-less, sidecar-less parse pool for direct callers of the
    loader functions (unit tests): ``resolve`` parses inline and nothing
    persists to disk."""
    return ParsePool(ParseCache(), 1)


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


def _load_supplied_statements_oneshot(conn, supplied_dir, *,
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
        inserted = skipped = activity = activity_dup = sales = 0
        # One ledger for the whole walk: a feed row absorbed by one
        # statement must not be absorbed again by the next.
        claims = {}
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
                dropped_lots = _drop_supplied_closed_lots(conn)
                log.info(
                    "supplied-statements: the parser has changed since these "
                    "rows were written; dropped %d activity row(s) and %d "
                    "closed lot(s) for re-derivation", dropped, dropped_lots,
                )
                owed_a_purge = False
            _drop_document_holdings(conn, sha)
            inserted += _insert_supplied_historical_rows(conn, path, result, sha)
            act_in, act_dup = _insert_supplied_activity_rows(
                conn, path, result, sha, claims=claims)
            activity += act_in
            activity_dup += act_dup
            sales += _insert_supplied_closed_lots(conn, path, result, sha)
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
            "row(s) inserted (%d already in the scraped feed), %d sale(s) "
            "inserted, %d PDF(s) skipped, %d account master row(s) "
            "synthesised",
            inserted, activity, activity_dup, sales, skipped, synth,
        )
    except Exception:
        conn.rollback()
        log.exception("supplied-statements load failed; rolled back")


def _read_1099_identity_worker(path):
    """ProcessPoolExecutor target: which form one Consolidated 1099 is,
    or ``{"_error": "<repr>"}``."""
    try:
        return pdf_parsers_1099.read_consolidated_1099_identity(path)
    except Exception as e:
        return {"_error": repr(e), "path": str(path)}


def _parse_1099_worker(path):
    """Parse one Consolidated 1099 in full, or ``{"_error": "<repr>"}``."""
    try:
        return pdf_parsers_1099.parse_consolidated_1099_pdf(path)
    except Exception as e:
        return {"_error": repr(e), "path": str(path)}


def _1099_copies(dumps, coord):
    """Every Consolidated 1099 copy in ``dumps`` with the identity read
    off its first pages: ``[(dump, path, sha, identity)]``."""
    copies = []
    for dump in dumps:
        for path in _consolidated_1099_candidates(dump):
            sha = coord.sha_for(path)
            copies.append((dump, path, sha, coord.resolve(
                sha, _CONSOLIDATED_1099_IDENTITY_VERSION,
                _read_1099_identity_worker, str(path))))
    return copies


def _dumps_for_1099s(conn, dumps, pending):
    """Which dumps the 1099 pass reads, and whether it re-derives:
    ``(dumps, rederive)``. It reads the dumps this run loads, or every
    dump in bronze when its generation is stale: its parser has changed
    since the lots silver holds were written, or no generation is
    stamped (none was yet, or migration 0013 dropped it)."""
    if silver.stale_generation(conn, CONSOLIDATED_1099_GENERATION_SCOPE,
                               _CONSOLIDATED_1099_PARSER_VERSION):
        return list(dumps), True
    return list(pending), False


def _load_consolidated_1099s(conn, dumps, *, rederive=False, coord=None):
    """Load the Form 1099-B lots of every Consolidated 1099 in ``dumps``
    into `closed_lots`, one form at a time, and file each copy's account
    and tax year into `documents` (`_file_1099_documents`).

    A form whose copy silver already holds, or holds a later one of, is
    passed over; otherwise its lots replace the ones held. With
    ``rederive`` — the parser has changed since silver's 1099-B rows were
    written, or none were — ``dumps`` is every dump in bronze, and the
    held rows are all dropped before the first form is written, so every
    form is re-derived. The drop waits for a form that parsed, for the
    same reason the supplied pass holds its purge: a run on which no PDF
    parses keeps the rows it has and leaves the generation unstamped."""
    coord = coord or _transient_coordinator()
    try:
        copies = _1099_copies(dumps, coord)
        chosen = _latest_1099_copies(copies)
        conn.execute("BEGIN")
        filed = _file_1099_documents(conn, copies)
        held = {} if rederive else _held_1099_forms(conn)
        owed_a_purge = rederive
        forms = lots = 0
        for key, (prepared, path, sha) in sorted(chosen.items()):
            if held.get(key) is not None and held[key] >= prepared:
                continue
            parsed = coord.resolve(sha, _CONSOLIDATED_1099_PARSER_VERSION,
                                   _parse_1099_worker, str(path))
            if "_error" in parsed:
                log.warning("1099: parse failed for %s: %s",
                            path.name, parsed["_error"])
                continue
            if owed_a_purge:
                dropped = conn.execute(
                    "DELETE FROM closed_lots WHERE document_kind = 'form_1099b'"
                ).rowcount
                log.info("1099: re-deriving every form for this parser "
                         "generation; dropped %d lot(s)", dropped)
                owed_a_purge = False
            lots += _replace_1099_lots(conn, parsed, sha)
            forms += 1
        if not owed_a_purge:
            silver.stamp_generation(conn, CONSOLIDATED_1099_GENERATION_SCOPE,
                                    _CONSOLIDATED_1099_PARSER_VERSION)
        conn.commit()
        log.info("1099: %d form(s) read, %d lot(s) written, %d document(s) "
                 "filed", forms, lots, filed)
    except Exception:
        conn.rollback()
        log.exception("1099 load failed; rolled back")


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
    # drift; see DESIGN.md §8.3). Make it loud.
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
        for ext_id, kind, n in portfolio_rows[:10]:
            log.info("validation: portfolio %s (kind=%s) → %d accounts",
                     ext_id, kind, n)


if __name__ == "__main__":
    sys.exit(main())
