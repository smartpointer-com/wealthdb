#!/usr/bin/env python3
"""
Bronze → silver loader for schwab-web.

Walks a bronze tree (one or more `<UTC-ts>/` dirs produced by
download.walk()), applies any pending schema migrations, and
loads each bronze dump into the silver SQLite database defined
by `migrations/0001_initial.sql`.

The silver schema mirrors `schwab-api`'s conventions so the
gold layer can splice the two feeds with minimal special-casing.
See migration 0001 for the full identifier-convention rationale,
including the irreconcilable differences (account-id space,
activity-id space) that the gold layer must bridge manually.

Idempotency:
  * `dump_runs` is keyed by snapshot_at (Unix seconds UTC parsed
    from the bronze dir name) — re-loading the same dir is a
    no-op.
  * `documents` is keyed by `sha256` — a PDF/XML/CSV file that
    appears in multiple bronze dumps collapses to one row.
  * `transactions` is keyed by a deterministic synthetic
    `activity_id` (see _synthesize_activity_id below) that is
    sha256-independent — re-parsing the same statement (even from
    a Schwab-regenerated PDF with a new sha256) converges to the
    same rows via INSERT OR IGNORE.
  * `parsed_documents` marks each logical statement, 1099,
    realized-lot report and distribution letter (account + doc_date +
    filename, the `logical_doc_key`) parsed under the current parser
    generation, so re-downloads of the same logical document are
    skipped, whatever rows its parse yielded. Tx-history exports are
    skipped once they have transaction rows. Use --reparse to force
    re-ingestion of a logical document (deletes by logical_doc_key, then
    re-inserts).
  * The statement snapshot tables are keyed by account and the
    period end a statement prints; the first statement to write a
    key owns it (see _write_statement_snapshots).

Usage:
    load.py --silver-db <path.db> --bronze-dir <root>
            [--migrations-dir <dir>] [--reparse] [--workers N]
            [--force] [-v]
"""

from __future__ import annotations

import argparse
import hashlib
import json
import logging
import os
import re
import sqlite3
import sys
import time
from collections.abc import Callable, Iterator
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path

from collectorkit import (
    bronze, cli, derivations, extract, parsecache, rebuild, silver, srcfp,
)
from collectorkit.parse import iso_date_to_epoch

import extraction
import form_1099b
import letters
import more_detail
import pdf_parsers as pp
import realized_reports
import stmt_parse
import tax_form_parsers as tf
from numparse import parse_amount

# Each document kind is a scope (PASSES). Its generation is the kind's code
# and the keys of the extractions it reads: an engine upgrade moves it as a
# parser edit does. A pass records each document it parses, the copy it
# read, under its generation in `document_derivations`
# (collectorkit.derivations, migration 0015), beside the document's parse
# marker. A parse that fails is recorded with no rows and gets no marker.
# A kind holding a record under another generation has moved: the load
# purges that kind's markers (and, for statements, the snapshot tables)
# and re-derives that kind's documents from every run; the rest of the
# walk is not repeated. A load that fails nowhere then drops the kind's
# records under another generation (_drop_older_records): the copies the
# re-derive did not read again. The kind then reads as current.
#
# The walk is how a run's manifest and its exports become rows. It is not
# a set of documents, so its stamp is the `documents` scope of
# `parser_generations` (migration 0005). A migration that changes the walk
# clears the stamp, and an empty silver has none. A stale walk implies
# `--reparse` for the whole invocation — the single lever the run gate,
# the per-document gates and the per-document deletes all already read —
# plus a purge of the statement snapshot tables, which have no delete path
# of their own, and of the parse markers, which vouch for an older reading.
WALK_SCOPE = "documents"
WALK_GENERATION = "1"


def _generation(fingerprint: str, *extractors) -> str:
    """A kind's generation: its code's fingerprint and the extraction
    keys of what it reads."""
    parts = [fingerprint, *(extract.extraction_key(e) for e in extractors)]
    return hashlib.sha256("\n".join(parts).encode("utf-8")).hexdigest()[:32]


GENERATIONS = {
    "statement": _generation(
        srcfp.parser_fingerprint([stmt_parse], python=False),
        extraction.TEXT),
    "third_party_distribution": _generation(
        srcfp.parser_fingerprint([letters], python=False), extraction.TEXT),
    "form_1099b": _generation(
        srcfp.parser_fingerprint([form_1099b], python=False)),
    "realized_report": _generation(
        srcfp.parser_fingerprint([realized_reports], python=False),
        extraction.TEXT, extraction.WORDS),
}

# What a `load --force` fills that an incremental load leaves empty in
# rows loaded before: (level, what) pairs, extended by hand in the change
# that leaves such rows (collectorkit.rebuild.rebuild_advice).
REBUILD_ADVICE = (
    (1, "the More dialog's detail on its tx-history rows"),
)
REBUILD_COMMAND = "schwab-web load --force"

# Written exclusively by the statement passes, and keyed on parser output
# (`as_of_date` / `period_end`, `instrument_key`), so a moved capture
# strands the old row under a key nothing will write again.
_SNAPSHOT_TABLES = ("historical_position_snapshots", "historical_cash_balances",
                    "open_lots")

log = logging.getLogger("schwab-web.load")

# Manifest doc dates and tx-history dates come from the Schwab UI as
# MM/DD/YYYY. A tx-history row booked against an earlier day reads
# "MM/DD/YYYY as of MM/DD/YYYY"; the first date is the day it posted, and
# the whole string stays in the row's payload.
_DOC_DATE_RE = re.compile(
    r"^(\d{1,2})/(\d{1,2})/(\d{4})(?:\s+as\s+of\s+\d{1,2}/\d{1,2}/\d{4})?$")

# Where to look for migrations when --migrations-dir isn't passed.
DEFAULT_MIGRATIONS_DIRS = (
    Path("/app/migrations"),                              # in-container
    Path(__file__).resolve().parent / "migrations",       # local dev
)

# Manifest doc_type → silver doc_kind. Trade Confirms are
# explicitly skipped upstream by download.py; the mapping stays for
# forward-compat.
_DOC_KIND_BY_TYPE = {
    "Statements":       "statement",
    "Tax Forms":        "tax_form",
    "Letters":          "letter",
    "Reports & Plans":  "report_or_plan",
    "Trade Confirms":   "trade_confirm",
}


# ============================================================
# CLI
# ============================================================

def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument(
        "--silver-db", type=Path,
        default=Path("/data/schwab-web.db"),
        help=("Path to the silver SQLite DB (default: %(default)s, "
              "the wrapper's /data mount). Created with the current "
              "schema if absent."),
    )
    p.add_argument(
        "--bronze-dir", type=Path, default=Path("/data"),
        help=("Bronze tree root — the same --bronze-dir download writes to. "
              "Default: %(default)s. The loader scans every <UTC-ts>/ "
              "subdir under it."),
    )
    p.add_argument(
        "--migrations-dir", default=None, type=Path,
        help=("Directory of migration SQL files. Defaults to "
              "/app/migrations (in-container) or ./migrations "
              "(local dev)."),
    )
    p.add_argument(
        "--reparse", action="store_true",
        help=("Re-parse every document (one copy per logical "
              "document), even one already parsed under the current "
              "parser generation. Its rows are replaced once the new "
              "parse succeeds. A changed parser re-parses its own "
              "kind's documents by itself."),
    )
    p.add_argument(
        "--workers", type=int, default=None,
        help=("Number of parallel worker processes for PDF parsing. "
              "Default: os.cpu_count(). PDF parsing is CPU-bound "
              "(text extraction via PDFium runs in C++ but each PDF "
              "is one shot, so the speedup comes from sharding across "
              "cores). Pass --workers 1 to force serial — useful for "
              "debugging."),
    )
    cli.add_standard_args(p, verb="load")
    return p.parse_args(argv)


# ============================================================
# Parsing: one pool and one parse cache per load
# ============================================================
#
# A document is parsed by a module-level function of one argument, so a
# process pool pickles it under spawn as well as under fork. The statement
# pass parses its PDFs through the pool; the other passes parse each
# document in turn. Every parse reads and fills the parse cache, keyed on
# the document's content and file name and on its kind's generation.

def _parse_worker(job: tuple) -> object:
    """Parse one document: `job` is (pass, path, the parse's arguments,
    the extraction cache). A failure comes back as {"_error": repr,
    "_message": text} so the parent can log it and go on rather than
    the pool crashing."""
    kind, path, args, extract_dir = job
    extraction.CACHE_DIR = extract_dir
    try:
        return PASSES[kind].parse(path, *args)
    except Exception as e:
        return {"_error": repr(e), "_message": str(e)}


def _resolve_worker_count(workers: int | None) -> int:
    """Parse-worker process count: the requested value when positive,
    otherwise one per CPU. PDF text extraction (PDFium in C++) is
    CPU-bound, so the speedup comes from sharding PDFs across cores."""
    if workers and workers > 0:
        return workers
    return os.cpu_count() or 1


def _parse_pool(cache: parsecache.ParseCache | None,
                workers: int | None) -> parsecache.ParsePool:
    """The parse pool of a load: in-process with --workers 1 (the path
    unit tests take so a monkeypatched parser is honoured — pool workers
    run in subprocesses that do not inherit the patch), else one worker
    per CPU or as asked."""
    return parsecache.ParsePool(
        cache, 1 if workers == 1 else _resolve_worker_count(workers))


# ============================================================
# Helpers
# ============================================================

def canonical_json(obj) -> str:
    """The payload and content-dedup encoding: stable key order, no
    whitespace, non-ASCII characters kept as UTF-8 (unlike schwab-api,
    which escapes them)."""
    return silver.canonical_json(obj)


def parse_snapshot_at(dump_dir_name: str) -> int:
    """Parse a bronze run-dir name `YYYYMMDDTHHMMSSZ` → Unix seconds UTC;
    ValueError for any other name."""
    if not bronze.RUN_DIR_RE.match(dump_dir_name):
        raise ValueError(f"bad bronze dir name: {dump_dir_name!r}")
    return bronze.parse_run_ts(dump_dir_name)


def parse_doc_date(s: str) -> int | None:
    """Parse "MM/DD/YYYY", or "MM/DD/YYYY as of MM/DD/YYYY" (the first
    date), → Unix seconds UTC at midnight. Returns None for
    blank/unparseable input — the loader treats that as a parser-output
    gap and skips the row."""
    if not s:
        return None
    m = _DOC_DATE_RE.match(s.strip())
    if not m:
        return None
    mm, dd, yyyy = (int(x) for x in m.groups())
    try:
        return int(datetime(yyyy, mm, dd, tzinfo=timezone.utc).timestamp())
    except ValueError:
        return None


def _resolve_migrations_dir(arg: Path | None) -> Path:
    if arg is not None:
        if not arg.is_dir():
            raise SystemExit(f"--migrations-dir does not exist: {arg}")
        return arg
    for cand in DEFAULT_MIGRATIONS_DIRS:
        if cand.is_dir():
            return cand
    raise SystemExit(
        "no migrations dir found; pass --migrations-dir or create "
        f"one of: {[str(p) for p in DEFAULT_MIGRATIONS_DIRS]}"
    )


# ============================================================
# Bronze inventory
# ============================================================

def discover_bronze_runs(bronze_dir: Path) -> list[Path]:
    """Return the subdirs under `bronze_dir` whose names match
    the run-ts format. Sorted by name (== chronological)."""
    if not bronze_dir.is_dir():
        raise SystemExit(f"--bronze-dir does not exist: {bronze_dir}")
    return list(bronze.iter_run_dirs(bronze_dir))


def already_loaded(conn: sqlite3.Connection, snapshot_at: int) -> bool:
    return silver.snapshot_loaded(conn, snapshot_at)


def _doc_kind(doc: dict) -> str:
    """A manifest document's silver doc_kind, from its `type`."""
    raw_type = doc.get("type") or "Unknown"
    return _DOC_KIND_BY_TYPE.get(raw_type, raw_type.lower())


def _format_from_filename(filename: str) -> str:
    """A document's format, from its file extension; a file without one
    counts as a PDF."""
    ext = Path(filename).suffix.lstrip(".").lower()
    return ext or "pdf"


def _account_documents(manifest: dict, doc_kind: str):
    """Each (account suffix, manifest document) of a run's statements
    area whose doc_kind is `doc_kind`, in manifest order."""
    for acct in manifest.get("statements", []):
        suffix = acct.get("suffix")
        if not suffix:
            continue
        for doc in acct.get("documents", []):
            if _doc_kind(doc) == doc_kind:
                yield suffix, doc


def _document_date(doc: dict) -> int | None:
    """A manifest document's date, or None with a warning when it does
    not read; the document is then skipped."""
    doc_date = parse_doc_date(doc.get("date") or "")
    if doc_date is None:
        log.warning("document %s has unparseable date %r; skipping",
                    doc.get("filename"), doc.get("date"))
    return doc_date


def _bronze_file(run_dir: Path, suffix: str, filename: str,
                 stats: dict) -> Path | None:
    """A manifest document's file in the run's statements area, or None,
    counted as missing, when it is not on disk."""
    path = run_dir / "statements" / suffix / filename
    if path.is_file():
        return path
    log.warning("document in manifest but missing on disk: %s", path)
    stats["documents_missing_on_disk"] += 1
    return None


def _record_document(conn: sqlite3.Connection, stats: dict,
                     row: tuple) -> None:
    """Add a `documents` row (`silver.DOCUMENT_COLUMNS`) unless its sha256
    is already held, and count it as new or as a duplicate."""
    new = silver.record_document(conn, row)
    stats["documents_new" if new else "documents_dup"] += 1


# ============================================================
# Row builders
# ============================================================

def _synthesize_activity_id(account_external_id: str,
                            tx: dict,
                            index: int) -> str:
    """Deterministic synthetic id for a statement-parsed transaction.

    Stable across re-downloads of the same logical statement, even when
    Schwab regenerates the PDF with a different sha256 (see INTEROP.md
    §3). Depends ONLY on transaction content + its ordinal position within
    the logical statement — NOT on the source document's sha256.

    `index` is a 0-based ordinal within the source document: a document
    row's position in the parsed list (stable because PDFium's text
    extraction is deterministic for the same logical PDF content), or for
    a tx-history export the count of earlier rows sharing the hashed
    fields (_insert_tx_history_transactions). It disambiguates
    genuinely-distinct transactions that share the same (date, amount,
    description, symbol) — e.g. two identical $50 fees booked on the same
    day.

    Hex SHA-256, first 32 chars (128 bits — collision probability is
    negligible at this scale)."""
    parts = [
        account_external_id,
        tx.get("date") or "",
        str(tx.get("amount") or ""),
        tx.get("description") or "",
        tx.get("symbol") or "",
        str(index),
    ]
    return hashlib.sha256("|".join(parts).encode("utf-8")).hexdigest()[:32]


def _logical_doc_key(account_external_id: str,
                     doc_date: int,
                     filename: str) -> str:
    """Stable opaque key for a logical document (same account +
    period + filename regardless of which sha256 the download produced).
    Keys the parse markers that gate re-parsing, the rows a document
    writes, and the --reparse delete that clears all sha256-churn
    variants at once.

    Format: "<account_external_id>|<doc_date_int>|<filename>". Schwab
    filenames contain only letters, digits, hyphens, underscores, and
    dots — no pipe chars — so the delimiter is safe. The format is
    intentionally human-readable (aids debugging) and computable in SQL
    (needed for the backfill migration)."""
    return f"{account_external_id}|{doc_date}|{filename}"


def _upsert_account(conn: sqlite3.Connection, snapshot_at: int,
                    account_external_id: str, payload_dict: dict) -> bool:
    """Insert (snapshot_at, account_external_id) row only when its
    canonical payload differs from the most recent row for the
    same account_external_id. Mirrors schwab-api.load_accounts.

    The manifest-built payload never carries `account_number_full`
    — that key is harvested from statement-PDF headers after the
    manifest walk (see _apply_account_number). Carrying the most
    recent row's value forward here keeps an unchanged account
    content-deduping against its number-bearing row, and keeps a
    label/nickname change from shedding the key. The registration label,
    also harvested from statements, carries forward the same way.

    Returns True if a row was inserted, False if dedup skipped it."""
    row = conn.execute(
        "SELECT payload, account_registration FROM accounts"
        " WHERE account_external_id = ?"
        " ORDER BY snapshot_at DESC LIMIT 1",
        (account_external_id,),
    ).fetchone()
    registration = None
    if row is not None:
        registration = row[1]
        prev_number = json.loads(row[0]).get("account_number_full")
        if prev_number is not None:
            payload_dict = dict(payload_dict,
                                account_number_full=prev_number)
    payload = canonical_json(payload_dict)
    if row is not None and row[0] == payload:
        return False
    nickname = payload_dict.get("nickname")
    conn.execute(
        "INSERT OR REPLACE INTO accounts"
        " (snapshot_at, account_external_id, nickname, payload,"
        "  account_registration)"
        " VALUES (?, ?, ?, ?, ?)",
        (snapshot_at, account_external_id, nickname, payload, registration),
    )
    return True


def _stored_account_number(conn: sqlite3.Connection,
                           account_external_id: str) -> str | None:
    """The `account_number_full` payload value on the most recent
    accounts row for `account_external_id`, or None."""
    row = conn.execute(
        "SELECT payload FROM accounts WHERE account_external_id = ? "
        "ORDER BY snapshot_at DESC LIMIT 1",
        (account_external_id,),
    ).fetchone()
    if row is None:
        return None
    return json.loads(row[0]).get("account_number_full")


def _write_account_number(conn: sqlite3.Connection,
                          account_external_id: str,
                          number: str | None) -> bool:
    """Set (or, with None, remove) `account_number_full` in the
    payload of EVERY accounts row for `account_external_id`, so
    the key reads the same regardless of which snapshot row a
    consumer picks. Returns True when any payload changed."""
    changed = False
    rows = conn.execute(
        "SELECT snapshot_at, payload FROM accounts"
        " WHERE account_external_id = ?",
        (account_external_id,),
    ).fetchall()
    for snapshot_at, payload in rows:
        d = json.loads(payload)
        if number is None:
            if "account_number_full" not in d:
                continue
            del d["account_number_full"]
        else:
            if d.get("account_number_full") == number:
                continue
            d["account_number_full"] = number
        conn.execute(
            "UPDATE accounts SET payload = ?"
            " WHERE snapshot_at = ? AND account_external_id = ?",
            (canonical_json(d), snapshot_at, account_external_id),
        )
        changed = True
    return changed


def _apply_account_number(conn: sqlite3.Connection,
                          account_external_id: str,
                          numbers: set[str]) -> bool:
    """Reconcile the full account numbers harvested from statement
    headers (`numbers`) with any previously stored value, then
    write the result into `accounts.payload.account_number_full`.

    The key is set only when every observation agrees: a suffix
    maps to exactly one full account number, so a disagreement —
    across this harvest, or against a value stored by an earlier
    load — means at least one header mis-parsed, and the key is
    cleared rather than pinned to a possibly-wrong value. A key
    set from a previously-absent state rests only on the
    observations passed in, so it is provisional until run_load's
    reconcile pass re-checks it against every statement in bronze
    (_reconcile_account_numbers) — that pass is what makes the
    end-of-load key depend only on the bronze evidence, never on
    load order. Returns True when any payload changed."""
    candidates = set(numbers)
    stored = _stored_account_number(conn, account_external_id)
    if stored is not None:
        candidates.add(stored)
    if not candidates:
        return False
    if len(candidates) > 1:
        log.warning(
            "account %s: %d conflicting full account numbers across "
            "statement headers; leaving account_number_full unset",
            bronze.log_id(account_external_id), len(candidates),
        )
        return _write_account_number(conn, account_external_id, None)
    return _write_account_number(conn, account_external_id,
                                 candidates.pop())


def _insert_dump_run(conn: sqlite3.Connection, snapshot_at: int,
                     run_dir: Path) -> None:
    """Record the run in the dump_runs audit table. OR REPLACE because
    --reparse deliberately revisits already-loaded runs (the normal
    path is gated by already_loaded, so a duplicate snapshot_at can
    only be a reparse) — the row re-stamps under the current
    silver_schema_version instead of colliding with the PK."""
    conn.execute(
        "INSERT OR REPLACE INTO dump_runs"
        " (snapshot_at, silver_schema_version, run_dir)"
        " VALUES (?, ?, ?)",
        (snapshot_at, silver.current_schema_version(conn), str(run_dir)),
    )


def _account_nickname(label: str | None) -> str | None:
    """Heuristic: extract a human-friendly nickname from a Schwab
    dropdown label.

    The label is rendered as the literal text of the dropdown's
    sdps-account-selector__left-col + …NNN suffix + an "Account
    ending in N N N" sr-only span. Any `…NNN` tail and the sr-only
    echo are stripped, leaving just the chosen account name.

    If the label is missing or pure-numeric, return None — the
    nickname column should not store the suffix again.
    """
    if not label:
        return None
    text = label.strip()
    # Drop "…NNN" tail.
    text = re.sub(r"\s*…\d{3,5}\s*", " ", text)
    # Drop "Account ending in N N N..." sr-only tail.
    text = re.sub(r"\s*Account ending in[\s\d]+$", "", text)
    # Strip any duplicated leading copy of the same name (the
    # Schwab label embeds nickname + nickname-as-aria-label).
    parts = text.strip().split()
    if len(parts) >= 4 and parts[: len(parts) // 2] == parts[len(parts) // 2:]:
        text = " ".join(parts[: len(parts) // 2])
    text = text.strip()
    return text or None


# ============================================================
# Per-bronze-run loader
# ============================================================

def load_run(conn: sqlite3.Connection, run_dir: Path,
             reparse: bool = False,
             workers: int | None = None,
             pool: parsecache.ParsePool | None = None,
             seen_logical_docs: set[tuple] | None = None, *,
             reparse_kinds: frozenset[str] | None = None,
             derive_only: frozenset[str] | None = None) -> dict:
    """Load one bronze-run dir. Returns a stats dict for logging.

    A run is read in three steps: its documents are indexed (the dump_runs
    row, the accounts, the documents table), the four document passes run
    in PASSES order, and the tx-history exports are loaded.

    `reparse` re-walks the run as `--reparse` does: every pass parses its
    documents whatever their parse markers say, and replaces their rows.
    `reparse_kinds` names the passes that do so (default: all of them
    under `reparse`, none otherwise). `derive_only` re-derives a run
    already loaded: only the passes it names run, and the run is not
    indexed again.

    `pool` and `seen_logical_docs`, when supplied by run_load, are shared
    across all of an invocation's bronze runs so each logical document
    is parsed once. Left at their defaults (a run-local, memory-cached
    pool and a fresh set) a direct call keeps a per-run scope.

    A logical doc is added to `seen_logical_docs` when a pass takes it
    up, before this run's inserts commit. A pass takes it out again when
    the parse fails, so a later copy is parsed in its place. run_load owns
    the rollback: it snapshots the set before the run and restores it if
    the run fails, so a rolled-back run never leaves a doc seen (which
    would wrongly skip it on every later run)."""
    snapshot_at = parse_snapshot_at(run_dir.name)
    stats = {
        "snapshot_at": snapshot_at,
        "accounts_inserted": 0,
        "accounts_deduped": 0,
        "documents_new": 0,
        "documents_dup": 0,
        "documents_missing_on_disk": 0,
        "transactions_inserted": 0,
        "transactions_reparsed": 0,
        "pdf_parse_errors": 0,
        "distribution_transactions_inserted": 0,
        "distribution_parse_errors": 0,
        "form_1099b_transactions_inserted": 0,
        "form_1099b_parse_errors": 0,
        "form_1099b_pdf_only": 0,
        "closed_lots_inserted": 0,
        "realized_report_parse_errors": 0,
        "realized_lots_unreadable": 0,
        "positions_inserted": 0,
        "cash_balances_inserted": 0,
        "statements_logical_deduped": 0,
        # Statements whose account and period end another statement's
        # rows already hold: with the same content, or with other content.
        "statements_copies": 0,
        "statements_conflicting": 0,
        "account_registration_updated": 0,
        "account_number_updated": 0,
        # Not a counter: suffixes whose account_number_full went
        # from absent to set on this run's parses alone. run_load
        # re-verifies them against the full bronze evidence
        # (_reconcile_account_numbers).
        "account_numbers_pinned": set(),
    }
    # Parse-dedup set: (logical_doc_key, document_kind), the parse-marker
    # key. Schwab regenerates statement PDFs on every download (different
    # sha256 each time — see INTEROP.md §3), but the parser output is
    # byte-identical for the same logical document. The first sha256
    # seen per logical doc is parsed; subsequent re-downloads skip it.
    # Across invocations the parse markers in `parsed_documents` do this
    # job; the set covers what they cannot: `--reparse`, which ignores
    # them, and the runs of one invocation before their marks commit.
    # run_load shares the set across the invocation's bronze runs; a
    # run-local default keeps a per-run scope for direct callers.
    if seen_logical_docs is None:
        seen_logical_docs = set()
    if reparse_kinds is None:
        reparse_kinds = frozenset(PASSES) if reparse else frozenset()

    manifest_path = run_dir / "run.json"
    if not manifest_path.is_file():
        log.warning("no run.json in %s; skipping", run_dir)
        return stats

    with manifest_path.open("r", encoding="utf-8") as fh:
        manifest = json.load(fh)

    # Skip non-complete dumps (bronze.is_loadable_status, the rule prune
    # applies to a present status). download.walk() writes run.json
    # incrementally with status="in-progress", flipping it to "complete"
    # only at the end, so a crashed walk leaves a partial manifest that
    # must NOT be ingested (partial balances would leak into gold as a
    # snapshot). A --dry-run writes nothing under bronze.
    status = manifest.get("status")
    if not bronze.is_loadable_status(status):
        log.warning(
            "run.json in %s has status=%r; skipping (not a complete dump)",
            run_dir, status,
        )
        return stats

    own_pool = pool is None
    if own_pool:
        pool = _parse_pool(None, workers)
    run = _Run(dir=run_dir, snapshot_at=snapshot_at, manifest=manifest,
               stats=stats, seen=seen_logical_docs, pool=pool,
               reparse_kinds=reparse_kinds,
               extract_dir=extraction.CACHE_DIR)
    try:
        if derive_only is None:
            _insert_dump_run(conn, snapshot_at, run_dir)
            _index_documents(conn, run)
        for kind, spec in PASSES.items():
            if derive_only is None or kind in derive_only:
                run_pass(conn, run, spec)
        if derive_only is None:
            _load_tx_history(conn, run, reparse)
    finally:
        if own_pool:
            pool.close()
    return stats


@dataclass
class _Run:
    """One bronze run as its passes see it."""
    dir: Path
    snapshot_at: int
    manifest: dict
    stats: dict
    seen: set[tuple]
    pool: parsecache.ParsePool
    # The passes that parse their documents again whatever their markers.
    reparse_kinds: frozenset[str]
    extract_dir: Path | None
    # The statement pass's harvest: the first registration label and every
    # full account number its statements print, by account.
    registration: dict[str, str] = field(default_factory=dict)
    numbers: dict[str, set[str]] = field(default_factory=dict)


def _index_documents(conn: sqlite3.Connection, run: _Run) -> None:
    """Record the run's accounts and every document of its statements
    area in the documents table. The passes then parse the documents of
    their kind."""
    stats = run.stats
    for acct in run.manifest.get("statements", []):
        suffix = acct.get("suffix")
        if not suffix:
            continue
        nickname = _account_nickname(acct.get("label"))
        acct_payload = {
            "suffix": suffix,
            "label": acct.get("label"),
            "nickname": nickname,
        }
        if _upsert_account(conn, run.snapshot_at, suffix, acct_payload):
            stats["accounts_inserted"] += 1
        else:
            stats["accounts_deduped"] += 1

        for doc in acct.get("documents", []):
            filename = doc.get("filename")
            sha256 = doc.get("sha256")
            size = int(doc.get("size") or 0)
            if not filename or not sha256:
                log.warning("manifest doc missing filename/sha256: %s", doc)
                continue

            doc_date = _document_date(doc)
            if doc_date is None:
                continue
            doc_kind = _doc_kind(doc)
            # The downloader writes the format from the row's
            # `Click to Download <FORMAT>` aria-label, but a row
            # labelled "PDF" can serve a CSV. Trust the file
            # extension on disk over the manifest claim, so a
            # mis-labelled row doesn't get handed to pypdfium2.
            claimed_fmt = (doc.get("format") or "").lower()
            fmt = _format_from_filename(filename)
            if claimed_fmt and claimed_fmt != fmt:
                log.warning(
                    "manifest format/extension mismatch for %s "
                    "(manifest=%s, on-disk=%s); using on-disk",
                    filename, claimed_fmt, fmt,
                )
            _record_document(conn, stats, (
                sha256, run.snapshot_at, suffix, doc_date, doc_kind, fmt,
                filename, size, canonical_json({
                    "raw_type": doc.get("type") or "Unknown",
                    "raw_doc_name": doc.get("document"),
                    "format": fmt,
                }),
            ))


# ============================================================
# The document passes
# ============================================================

@dataclass(frozen=True)
class Doc:
    """One logical document a pass parses: its account, its key, its
    parse-marker kind, the copy's file and manifest sha256, its date, and
    what its parse takes besides the path. `twin` is another copy of the
    same logical document, in another format, that is read when this
    one does not parse."""
    suffix: str
    ldk: str
    kind: str
    path: Path
    sha256: str
    doc_date: int
    args: tuple = ()
    twin: Doc | None = None


@dataclass(frozen=True)
class PassSpec:
    """One kind of document and how a run's copies of it become rows.

    `select(run, gate)` yields the run's documents of the kind, in
    manifest order, that are due; it asks `gate(ldk, kind)` whether a
    logical document is. `parse(path, *doc.args)` reads one copy (in a
    pool worker, so it is module-level). `write(conn, run, doc, parsed)`
    replaces the document's rows with the parse's and returns how many it
    wrote. A failed parse is logged with `error_log` and counted in
    `error_stat`; the log names the path and the error's `error_field`
    (`_error`, its repr, or `_message`, its text).

    A `batch` pass parses every selected document through the pool before
    it writes any. `finish(conn, run)` is called at the end of every
    run's pass, whether or not a document was due. `dedup_stat` counts
    the copies the gate turns away because the invocation took the
    document up already."""
    kind: str
    marker_kinds: tuple[str, ...]
    select: Callable[[_Run, Callable[[str, str], bool]], Iterator[Doc]]
    parse: Callable[..., object]
    write: Callable[[sqlite3.Connection, _Run, Doc, object], int]
    error_log: str
    error_stat: str
    error_field: str = "_message"
    batch: bool = False
    dedup_stat: str | None = None
    finish: Callable[[sqlite3.Connection, _Run], None] | None = None


def run_pass(conn: sqlite3.Connection, run: _Run, spec: PassSpec) -> None:
    """Run one pass over one run: select its documents, parse each logical
    document once, write its rows, mark it parsed and record its
    derivation.

    A logical document is due when the invocation has not taken it up and
    it carries no parse marker of its kind, or when the pass re-parses.
    It is taken up as it is selected, and given back when its parse
    fails, so a later copy is parsed in its place. A pass deletes a
    document's rows only once its parse succeeded."""
    stats = run.stats
    reparse = spec.kind in run.reparse_kinds

    def gate(ldk: str, kind: str) -> bool:
        if (ldk, kind) in run.seen:
            if spec.dedup_stat:
                stats[spec.dedup_stat] += 1
            return False
        return reparse or not _is_parsed(conn, ldk, kind)

    selected: list[Doc] = []
    for doc in spec.select(run, gate):
        run.seen.add((doc.ldk, doc.kind))
        if spec.batch:
            selected.append(doc)
        else:
            _derive(conn, run, spec, *_resolve(run, spec, doc))
    if selected:
        log.info("parsing %d %s document(s)", len(selected), spec.kind)
        t0 = time.monotonic()
        for doc in selected:
            run.pool.enqueue(*_parse_job(run, spec, doc))
        run.pool.dispatch()
        results = [_resolve(run, spec, doc) for doc in selected]
        elapsed = time.monotonic() - t0
        log.info(
            "parsed %d document(s) in %.2fs (%.3fs/document wall)",
            len(selected), elapsed, elapsed / max(1, len(selected)),
        )
        for doc, parsed in results:
            _derive(conn, run, spec, doc, parsed)
    # Also when no document was due: a finish may read more than this
    # run's parses (the statement pass's tax-form fallback reads the
    # documents table).
    if spec.finish is not None:
        spec.finish(conn, run)


def _parse_job(run: _Run, spec: PassSpec, doc: Doc) -> tuple:
    """(cache key, namespace, worker, job) of one document's parse. The
    key is the copy's content, its file name and the parse's arguments:
    all a parse reads. The namespace is the kind's generation."""
    key = hashlib.sha256("|".join(
        [run.pool.sha_for(doc.path), doc.path.name, *map(str, doc.args)]
    ).encode("utf-8")).hexdigest()
    return (key, GENERATIONS[spec.kind], _parse_worker,
            (spec.kind, str(doc.path), doc.args, run.extract_dir))


def _resolve(run: _Run, spec: PassSpec, doc: Doc) -> tuple[Doc, object]:
    """The copy read and its parse: `doc`, or its twin when `doc` does
    not parse."""
    parsed = run.pool.resolve(*_parse_job(run, spec, doc))
    if parsecache.is_error(parsed) and doc.twin is not None:
        log.warning(spec.error_log + "; reading %s instead", doc.path,
                    parsed[spec.error_field], doc.twin.path.name)
        return _resolve(run, spec, doc.twin)
    return doc, parsed


def _derive(conn: sqlite3.Connection, run: _Run, spec: PassSpec, doc: Doc,
            parsed: object) -> None:
    """Write one parsed document, mark it and record the copy read under
    the kind's generation. A failed one is logged, recorded with no rows
    and given back unmarked: a later copy is parsed in its place, and the
    kind's next generation reads it again."""
    if parsecache.is_error(parsed):
        log.warning(spec.error_log, doc.path, parsed[spec.error_field])
        run.stats[spec.error_stat] += 1
        run.seen.discard((doc.ldk, doc.kind))
        derivations.record_derivation(conn, doc.sha256, spec.kind,
                                      GENERATIONS[spec.kind], 0)
        return
    rows = spec.write(conn, run, doc, parsed)
    _mark_parsed(conn, doc.ldk, doc.kind, doc.sha256)
    derivations.record_derivation(conn, doc.sha256, spec.kind,
                                  GENERATIONS[spec.kind], rows)


# ---- Statements ------------------------------------------------------

def _select_statements(run: _Run, gate) -> Iterator[Doc]:
    """The run's statement PDFs (positions, cash, transactions). One
    gate per logical statement (account + doc_date + filename), never the
    sha256: Schwab regenerates PDFs per download (new sha256 each time —
    INTEROP.md §3). A document the index passed over (no filename or
    sha256, no date) is passed over here too, and the index warned."""
    for suffix, doc in _account_documents(run.manifest, "statement"):
        filename = doc.get("filename")
        sha256 = doc.get("sha256")
        if not filename or not sha256:
            continue
        doc_date = parse_doc_date(doc.get("date") or "")
        if doc_date is None or _format_from_filename(filename) != "pdf":
            continue
        pdf_path = _bronze_file(run.dir, suffix, filename, run.stats)
        if pdf_path is None:
            continue
        ldk = _logical_doc_key(suffix, doc_date, filename)
        if gate(ldk, "statement"):
            year_hint = datetime.fromtimestamp(doc_date, tz=timezone.utc).year
            yield Doc(suffix, ldk, "statement", pdf_path, sha256, doc_date,
                      (year_hint,))


def _parse_statement(path: str, year_hint: int) -> dict:
    return pp.parse_statement_pdf(path, statement_year=year_hint)


def _write_statement(conn: sqlite3.Connection, run: _Run, doc: Doc,
                     parsed: dict) -> int:
    """A parsed statement's transactions and snapshots; returns the
    transactions, positions and cash balances written. Its registration
    label and account number are harvested for the run's accounts."""
    stats = run.stats
    reg = parsed.get("account_registration")
    if reg and doc.suffix not in run.registration:
        run.registration[doc.suffix] = reg
    num = parsed.get("account_number")
    if num:
        run.numbers.setdefault(doc.suffix, set()).add(num)

    _reparse_delete(conn, doc.ldk, "statement" in run.reparse_kinds, stats)
    inserted = _insert_statement_transactions(
        conn, doc.suffix, parsed.get("transactions", []),
        doc.sha256, doc.ldk,
    )
    stats["transactions_inserted"] += inserted

    period_end_ts = (
        iso_date_to_epoch(parsed.get("period_end")) or doc.doc_date
    )
    period_start_ts = (
        iso_date_to_epoch(parsed.get("period_start")) or doc.doc_date
    )
    if period_end_ts != doc.doc_date:
        log.info("statement %s prints a period end other than its "
                 "manifest date; its rows take the printed one", doc.ldk)

    _check_account_value(parsed, doc.ldk)
    return inserted + _write_statement_snapshots(
        conn, {"suffix": doc.suffix, "ldk": doc.ldk, "sha256": doc.sha256},
        parsed, period_start_ts, period_end_ts, stats)


def _harvest_account_facts(conn: sqlite3.Connection, run: _Run) -> None:
    """Pin what the run's statements say about their accounts: the
    registration label, the full account number, and a tax-form fallback
    for an account no statement names the registration of."""
    stats = run.stats
    # Harvest the account-registration label from one
    # parsed statement per account. The label is stable
    # across statements for a given account; the first
    # non-null value seen is kept. The wealthdb gold
    # adapter keys off this string (verbatim) for its
    # `tax_wrapper` enum mapping — see migration 0003
    # comment + DESIGN.md §8. Stable across runs — the
    # column drifts only if Schwab restyles the
    # registration line.
    for suffix, reg in run.registration.items():
        conn.execute(
            "UPDATE accounts SET account_registration = ?"
            " WHERE account_external_id = ?",
            (reg, suffix),
        )
    n_updated = len(run.registration)

    # Pin the full account number into each account's
    # payload (consistent-or-absent — see
    # _apply_account_number; INTEROP.md §1 for the api↔web
    # bridge it feeds). A key set here where none was
    # stored rests only on this run's parses — bronze may
    # hold older, disagreeing statements this run never saw
    # (dump-run / logical-doc gated, or cleared by an earlier
    # conflict) — so the suffix is queued for re-verification
    # against the full bronze evidence
    # (_reconcile_account_numbers in run_load). Accounts
    # whose statements were all ingested by earlier
    # invocations reach the same pass via their absent key.
    for suffix, nums in run.numbers.items():
        if (len(nums) == 1
                and _stored_account_number(conn, suffix) is None):
            stats["account_numbers_pinned"].add(suffix)
        if _apply_account_number(conn, suffix, nums):
            stats["account_number_updated"] += 1

    # Tax-form fallback: for any account whose statements
    # didn't surface a registration (e.g. a brand-new
    # account that hasn't received a monthly statement
    # yet, or one whose statement format the parser does not
    # handle yet), check the documents table for the cleanest
    # tax-form signal — Schwab's 5498 / 5498-ESA filenames
    # carry the account type by definition (the forms are
    # only ever issued for IRA / ESA accounts).
    null_rows = conn.execute(
        "SELECT account_external_id FROM accounts"
        " GROUP BY account_external_id"
        " HAVING COUNT(account_registration) = 0"
    ).fetchall()
    for (suffix,) in null_rows:
        reg = _registration_from_tax_forms(conn, suffix)
        if reg is None:
            continue
        conn.execute(
            "UPDATE accounts SET account_registration = ?"
            " WHERE account_external_id = ?",
            (reg, suffix),
        )
        n_updated += 1
    stats["account_registration_updated"] = n_updated


def _load_tx_history(conn: sqlite3.Connection, run: _Run,
                     reparse: bool) -> None:
    """The run's tx-history exports: per-account CSV/JSON/XML. JSON is
    the canonical source for silver rows; CSV/XML are stored as opaque
    documents for traceability."""
    stats = run.stats
    transactions_dir = run.dir / "transactions"
    for acct in run.manifest.get("transactions", []):
        suffix = acct.get("suffix")
        if not suffix:
            continue
        acct_dir = transactions_dir / suffix
        if not acct_dir.is_dir():
            continue
        # Optional per-account More-detail sidecar (written by
        # download.py's default per-row detail pass, unless
        # --no-more-detail). Its parsed dialogs go into matching
        # transactions' payload.
        more_details = _load_more_details(acct_dir)
        for export in acct.get("exports", []):
            sha256 = export.get("sha256")
            filename = export.get("filename")
            fmt = (export.get("format") or "").lower()
            size = int(export.get("size") or 0)
            if not sha256 or not filename or not fmt:
                log.warning("tx-history manifest export missing fields: %s",
                            export)
                continue
            _record_document(conn, stats, (
                sha256, run.snapshot_at, suffix, run.snapshot_at,
                "tx_history_export", fmt, filename, size,
                canonical_json({
                    "raw_type": "Transaction History Export",
                    "format": fmt,
                }),
            ))

            # JSON is the canonical row source — same logical
            # events as the CSV/XML but with a couple of extra
            # fields (AcctgRuleCd) and a structure that's
            # cheaper to parse. Skip CSV/XML for row ingestion;
            # they stay as documents for traceability.
            if fmt != "json":
                continue
            json_path = acct_dir / filename
            if not json_path.is_file():
                log.warning("tx-history JSON missing on disk: %s",
                            json_path)
                stats["documents_missing_on_disk"] += 1
                continue
            # Logical-doc gate for tx-history: keyed on
            # (account, snapshot_at, filename) since each export
            # run produces a uniquely-named file. The gate skips an
            # export already loaded; INSERT OR IGNORE in
            # _insert_tx_history_transactions is the row-level
            # dedup backstop.
            tx_ldk = _logical_doc_key(suffix, run.snapshot_at, filename)
            if not _gate_logical_doc(conn, tx_ldk, reparse):
                continue
            try:
                with json_path.open("r", encoding="utf-8") as fh:
                    payload = json.load(fh)
            except Exception as e:
                log.warning("tx-history JSON parse failed for %s: %s",
                            json_path, e)
                continue
            _reparse_delete(conn, tx_ldk, reparse, stats)
            txs = payload.get("BrokerageTransactions") or []
            n = _insert_tx_history_transactions(
                conn, suffix, txs, sha256, more_details, tx_ldk,
            )
            stats["transactions_inserted"] += n


def _load_more_details(acct_dir: Path) -> dict:
    """Return a {row_key: detail_dict} map of the "More" dialogs of
    this tx-history account that parse to a non-empty dict, or {} when
    the sidecar is absent.

    download.py's per-row detail pass (default; --no-more-detail skips)
    writes `<acct_dir>/more-details.json` as a list of
    `{"row_key", "row_cells", "fields", "raw_text"}` records. The
    dialog is parsed here from `raw_text` (more_detail.parse); the
    record's `fields` stays empty and is not read. row_key is a deterministic SHA-256
    prefix over the row's promoted columns
    (date|amount|description|symbol|action) that the loader can
    re-derive from the JSON export to merge details in. See
    download._scrape_more_details for the row_key derivation.
    """
    path = acct_dir / "more-details.json"
    if not path.is_file():
        return {}
    try:
        with path.open("r", encoding="utf-8") as fh:
            entries = json.load(fh)
    except Exception as e:
        log.warning("more-details parse failed for %s: %s", path, e)
        return {}
    out = {}
    for entry in entries:
        key = entry.get("row_key")
        raw_text = entry.get("raw_text")
        if not key or not isinstance(raw_text, str):
            continue
        fields = more_detail.parse(raw_text)
        if fields:
            out[key] = fields
    log.info("loaded %d more-detail record(s) from %s", len(out), path)
    return out


def _tx_history_row_key(tx: dict) -> str:
    """Deterministic key over a tx-history row's promoted fields,
    matching what download._scrape_more_details derives from the
    rendered row. Used to merge More-modal detail into the JSON
    export rows."""
    parts = [
        str(tx.get("Date") or ""),
        str(tx.get("Amount") or ""),
        str(tx.get("Description") or ""),
        str(tx.get("Symbol") or ""),
        str(tx.get("Action") or ""),
    ]
    return hashlib.sha256("|".join(parts).encode("utf-8")).hexdigest()[:16]


def _parse_money(s) -> float | None:
    """Parse a Schwab money string (e.g. "$1,234.56", "-$5.00",
    "($1.00)") to a signed float. Returns None for blank /
    non-money input. Conservative: silver stores the raw string
    in `payload` regardless; this is for ordering / arithmetic
    indexes only."""
    return parse_amount(s, dollar=True, leading_minus=True)


def _insert_tx_history_transactions(conn: sqlite3.Connection,
                                    account_external_id: str,
                                    transactions: list[dict],
                                    source_sha256: str,
                                    more_details: dict,
                                    logical_doc_key: str) -> int:
    """INSERT OR IGNORE one row per Schwab-JSON-exported
    transaction. Same activity-id derivation contract as the
    statement parser (synthetic SHA-256 prefix), but over the
    JSON export's field names (Title-Cased: Date, Amount, etc.).

    `logical_doc_key` is stored on every row so --reparse can
    delete all rows for a logical export without touching other
    sources.

    If `more_details[row_key]` exists for a row, the parsed dialog is
    stored in the row's `payload` JSON under a nested `_more` key —
    silver consumers can pluck e.g.
    `json_extract(payload, '$._more.settle_date')`. A row an earlier
    export already loaded takes the dialog when it holds none (no
    `_more`, or an empty one); a row that holds one keeps it.

    The ordinal in the activity_id counts only the earlier rows of the
    export that share the row's hashed fields (date, amount,
    description, symbol), not its position in the file. Exports of
    overlapping windows place one row at different positions, but list
    the rows of one day the same way, so the row keeps one id across
    them while two genuine same-day twins stay apart.
    """
    inserted = 0
    filled = 0
    ordinals: dict[tuple, int] = {}
    for idx, tx in enumerate(transactions):
        date_str = tx.get("Date") or ""
        timestamp = parse_doc_date(date_str)
        if timestamp is None:
            log.debug("tx-history row %d: bad date %r — skipping",
                      idx, date_str)
            continue
        # Normalised dict matching _synthesize_activity_id's
        # expected keys so the synthetic id space lines up with
        # the statement_pdf path (helps gold-layer cross-source
        # deduping by ID prefix patterns).
        normalised = {
            "date": date_str,
            "amount": _parse_money(tx.get("Amount")),
            "description": tx.get("Description"),
            "symbol": tx.get("Symbol"),
        }
        twin_key = tuple(normalised.values())
        ordinal = ordinals.get(twin_key, 0)
        ordinals[twin_key] = ordinal + 1
        activity_id = _synthesize_activity_id(
            account_external_id, normalised, ordinal,
        )
        # Merge any matching More-modal detail into payload.
        payload_dict = dict(tx)
        detail = more_details.get(_tx_history_row_key(tx))
        if detail:
            payload_dict["_more"] = detail
        n = _insert_transaction(conn, (
            activity_id, timestamp, account_external_id,
            tx.get("Action") or "Unknown", tx.get("Symbol") or None,
            "tx_history_json", source_sha256, logical_doc_key,
        ), payload_dict)
        inserted += n
        if not n and detail:
            filled += _fill_more_detail(conn, activity_id, detail)
    if filled:
        log.info("filled More detail on %d earlier row(s)", filled)
    return inserted


def _fill_more_detail(conn: sqlite3.Connection, activity_id: str,
                      detail: dict) -> int:
    """Store `detail` as `_more` on the stored tx-history row
    `activity_id` when that row holds no detail (no `_more`, or an
    empty one). Returns 1 when the row was filled, else 0."""
    row = conn.execute(
        "SELECT payload FROM transactions"
        " WHERE activity_id = ? AND source = 'tx_history_json'",
        (activity_id,),
    ).fetchone()
    if row is None:
        return 0
    payload = json.loads(row[0])
    if payload.get("_more"):
        return 0
    payload["_more"] = detail
    conn.execute("UPDATE transactions SET payload = ?"
                 " WHERE activity_id = ? AND source = 'tx_history_json'",
                 (canonical_json(payload), activity_id))
    return 1


def _insert_transaction(conn: sqlite3.Connection, row: tuple,
                        payload: dict) -> int:
    """INSERT OR IGNORE one `transactions` row: (activity_id, timestamp,
    account_external_id, kind, instrument_key, source, source_sha256,
    logical_doc_key), its payload and the payload's columns
    (_transaction_columns). Returns 1 when it was inserted, else 0."""
    return conn.execute(
        "INSERT OR IGNORE INTO transactions"
        " (activity_id, timestamp, account_external_id, kind,"
        "  instrument_key, source, source_sha256, logical_doc_key,"
        "  payload, amount, quantity, price, description, action)"
        " VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
        (*row, canonical_json(payload), *_transaction_columns(payload)),
    ).rowcount


# Whitespace as Go's strings.TrimSpace reads it: the gold adapter read
# these figures and words out of the payload, and the columns hold what it
# read.
_GO_SPACE = ("\t\n\v\f\r \x85\xa0\u1680\u2000\u2001\u2002\u2003\u2004"
             "\u2005\u2006\u2007\u2008\u2009\u200a\u2028\u2029\u202f"
             "\u205f\u3000")
_NUMBER_KEYS = ("amount", "quantity", "price", "market_value", "cash_amount")
_STRING_KEYS = ("Amount", "Quantity", "Price")
_DECIMAL_RE = re.compile(r"(?=.*[0-9])[0-9]*\.?[0-9]*")


def _is_number(v) -> bool:
    return isinstance(v, (int, float)) and not isinstance(v, bool)


def _number_text(v) -> str | None:
    """A JSON number as the payload's JSON prints it."""
    return json.dumps(v) if _is_number(v) else None


def _figure_text(v) -> str | None:
    """A tx-history export's figure ("$1,000.00", "-$5.00", "($1.00)")
    as decimal text: the "$", the thousands commas and the spaces taken
    off, parentheses or a leading minus made one minus; None for a figure
    that is then no decimal."""
    if not isinstance(v, str):
        return None
    s = v.strip(_GO_SPACE)
    negative = len(s) >= 2 and s[0] == "(" and s[-1] == ")"
    if negative:
        s = s[1:-1]
    s = s.replace("$", "").replace(",", "").replace(" ", "")
    if s.startswith("-"):
        negative = not negative
        s = s[1:]
    if not _DECIMAL_RE.fullmatch(s):
        return None
    return f"-{s}" if negative else s


def _word(payload: dict, key: str) -> str | None:
    v = payload.get(key)
    return (v.strip(_GO_SPACE) or None) if isinstance(v, str) else None


def _transaction_columns(payload: dict) -> tuple:
    """(amount, quantity, price, description, action) of a transactions
    row, from its payload in any of the three dialects: the statement's
    numbers (`amount`), the tx-history export's strings (`Amount`), a
    distribution letter's `market_value` or `cash_amount` where it states
    no amount. Figures are decimal text, words as printed, trimmed. The
    action is the movement the row states (see below).

    A payload whose figure keys hold another JSON type has no figures,
    and one whose description keys do, no description: migration 0014
    backfills the same values from the stored payload."""
    typed = (all(payload.get(k) is None or _is_number(payload[k])
                  for k in _NUMBER_KEYS)
             and all(payload.get(k) is None or isinstance(payload[k], str)
                     for k in _STRING_KEYS))
    amount = quantity = price = None
    if typed:
        amount = next((t for t in (
            _number_text(payload.get("amount")),
            _figure_text(payload.get("Amount")),
            _number_text(payload.get("market_value")),
            _number_text(payload.get("cash_amount"))) if t is not None), None)
        quantity = (_number_text(payload.get("quantity"))
                    or _figure_text(payload.get("Quantity")))
        price = (_number_text(payload.get("price"))
                 or _figure_text(payload.get("Price")))
    description = None
    if all(payload.get(k) is None or isinstance(payload[k], str)
           for k in ("description", "Description")):
        description = _word(payload, "description") or _word(payload,
                                                              "Description")
    # The movement the row states: the export's `Action`, a statement
    # row's `action`, else its `category`. "Unknown" is the statement
    # parser's word for a row whose category did not read: no movement.
    action = next((w for w in (_word(payload, "Action"),
                               _word(payload, "action"),
                               _word(payload, "category"))
                   if w and w.encode("utf-8").lower() != b"unknown"), None)
    return amount, quantity, price, description, action


def _insert_statement_transactions(conn: sqlite3.Connection,
                                   account_external_id: str,
                                   transactions: list[dict],
                                   source_sha256: str,
                                   logical_doc_key: str) -> int:
    """INSERT OR IGNORE one row per parsed transaction. Returns
    the count inserted.

    `logical_doc_key` is the sha256-independent key for the
    logical document (account + doc_date + filename), stored on
    every row so the --reparse delete clears all of a logical
    statement's rows at once.

    Skips rows that have no amount (parser failure indicator) —
    a parser regression should drop the bad row, not break the
    whole load."""
    inserted = 0
    for idx, tx in enumerate(transactions):
        if tx.get("amount") is None:
            log.warning("skipping transaction with no amount: %s",
                        {k: tx.get(k) for k in
                         ("date", "category", "symbol")})
            continue
        activity_id = _synthesize_activity_id(
            account_external_id, tx, idx,
        )
        timestamp = iso_date_to_epoch(tx.get("date"))
        if timestamp is None:
            log.warning("skipping transaction with no parseable date: %s",
                        tx.get("date"))
            continue
        # Statement PDFs expose only the ticker in the activity
        # rows (CUSIP lives in the positions block), so
        # instrument_key carries the parser's ticker; the gold
        # layer resolves to CUSIP via the api silver's
        # instruments table.
        inserted += _insert_transaction(conn, (
            activity_id, timestamp, account_external_id,
            tx.get("category") or "Unknown", tx.get("symbol"),
            "statement_pdf", source_sha256, logical_doc_key,
        ), tx)
    return inserted


def _insert_parsed_transactions(conn: sqlite3.Connection,
                                account_external_id: str,
                                rows: list[dict],
                                source: str,
                                source_sha256: str,
                                logical_doc_key: str) -> int:
    """INSERT OR IGNORE one row per normalised parser row, for the
    feeds whose parser maps the row itself (form_1099b,
    third_party_distribution).

    Mirrors `_insert_statement_transactions` — same synthetic
    activity_id contract (`date`/`amount`/`description`/`symbol` +
    ordinal index), same logical_doc_key bookkeeping — but takes the
    `source` and the silver `kind` / `instrument_key` straight from
    the row dict (the parser already mapped them) and stores the whole
    row as the payload. Rows with no parseable `date` are dropped with
    a warning rather than failing the load."""
    inserted = 0
    for idx, row in enumerate(rows):
        timestamp = iso_date_to_epoch(row.get("date"))
        if timestamp is None:
            log.warning("skipping %s row with no parseable date: %r",
                        source, row.get("date"))
            continue
        inserted += _insert_transaction(conn, (
            _synthesize_activity_id(account_external_id, row, idx),
            timestamp, account_external_id, row.get("kind") or "Unknown",
            row.get("instrument_key"), source, source_sha256,
            logical_doc_key,
        ), row)
    return inserted


def _gate_logical_doc(conn: sqlite3.Connection, ldk: str,
                      reparse: bool) -> bool:
    """The load gate of the tx-history exports, which carry no parse
    marker: True when the export is to be parsed, on `--reparse` or
    while it has no transaction rows. Deletes nothing.

    The delete-on-reparse is intentionally NOT done here: the caller
    must parse first and only delete on a *successful* parse (via
    `_reparse_delete`), so a parse failure can never drop existing rows
    without a replacement. This mirrors the statement-PDF path, which
    checks the parse result before its delete."""
    return reparse or conn.execute(
        "SELECT 1 FROM transactions WHERE logical_doc_key = ?", (ldk,),
    ).fetchone() is None


def _reparse_delete(conn: sqlite3.Connection, ldk: str, reparse: bool,
                    stats: dict) -> None:
    """Clear all rows for a logical doc immediately before re-insert,
    once its parse has succeeded. No-op unless `--reparse` (all
    sha256-churn variants share one logical_doc_key, so this clears them
    together)."""
    if reparse and conn.execute(
            "DELETE FROM transactions WHERE logical_doc_key = ?", (ldk,),
    ).rowcount:
        stats["transactions_reparsed"] += 1


def _is_parsed(conn: sqlite3.Connection, ldk: str, document_kind: str) -> bool:
    """Whether the logical document carries a parse marker: it has been
    parsed under the current parser generation."""
    return conn.execute(
        "SELECT 1 FROM parsed_documents"
        " WHERE logical_doc_key = ? AND document_kind = ?",
        (ldk, document_kind),
    ).fetchone() is not None


def _mark_parsed(conn: sqlite3.Connection, ldk: str, document_kind: str,
                 source_sha256: str) -> None:
    """Record that the logical document has been parsed, whatever rows the
    parse yielded, so later loads skip it until the parser generation
    moves or `--reparse`."""
    conn.execute(
        "INSERT OR REPLACE INTO parsed_documents"
        " (logical_doc_key, document_kind, source_sha256) VALUES (?, ?, ?)",
        (ldk, document_kind, source_sha256),
    )


def _select_distribution_letters(run: _Run, gate) -> Iterator[Doc]:
    """3rd-Party-Distribution letters (doc_kind='letter', the
    "3rd-Party-Distribution_*.PDF" filename). Each letter is one logical
    document (no format twins), so the logical_doc_key uses the filename
    directly."""
    for suffix, doc in _account_documents(run.manifest, "letter"):
        filename = doc.get("filename") or ""
        if (not filename.startswith("3rd-Party-Distribution")
                or _format_from_filename(filename) != "pdf"):
            continue
        doc_date = _document_date(doc)
        if doc_date is None:
            continue
        path = _bronze_file(run.dir, suffix, filename, run.stats)
        if path is None:
            continue
        ldk = _logical_doc_key(suffix, doc_date, filename)
        if gate(ldk, "third_party_distribution"):
            yield Doc(suffix, ldk, "third_party_distribution", path,
                      doc.get("sha256") or "", doc_date)


def _parse_distribution_letter(path: str) -> list[dict]:
    return pp.parse_distribution_pdf(path)


def _write_distribution_letter(conn: sqlite3.Connection, run: _Run,
                               doc: Doc, rows: list[dict]) -> int:
    """A letter's transfer rows, source='third_party_distribution';
    returns how many were written."""
    stats = run.stats
    _reparse_delete(conn, doc.ldk,
                    "third_party_distribution" in run.reparse_kinds, stats)
    n = _insert_parsed_transactions(
        conn, doc.suffix, rows, "third_party_distribution", doc.sha256,
        doc.ldk,
    )
    stats["transactions_inserted"] += n
    stats["distribution_transactions_inserted"] += n
    return n


def _select_1099b_forms(run: _Run, gate) -> Iterator[Doc]:
    """1099 Composite tax forms (doc_kind='tax_form', document name
    containing "1099 Composite"), one per logical form.

    Schwab ships each logical form as a PDF plus, where it offers them,
    machine-readable XML and CSV twins. The twins share a base filename;
    the PDF is named apart. All three share the manifest row (date and
    document name). The XML is preferred, and the CSV is read when the XML
    is not on disk or does not parse. The logical_doc_key is keyed on the
    **base filename (without extension)** so the XML and CSV twins dedup
    to one set of rows. A PDF without an XML or CSV on its manifest row
    is logged as a coverage gap (no machine-readable lot detail to
    parse)."""
    stats = run.stats
    twins: dict[tuple[str, str], dict[str, dict]] = {}
    pdfs: list[tuple[str, dict]] = []
    for suffix, doc in _account_documents(run.manifest, "tax_form"):
        if "1099 Composite" not in (doc.get("document") or ""):
            continue
        filename = doc.get("filename") or ""
        fmt = _format_from_filename(filename)
        if fmt == "pdf":
            pdfs.append((suffix, doc))
        elif fmt in ("xml", "csv"):
            base = filename[: -(len(fmt) + 1)]
            twins.setdefault((suffix, base), {}).setdefault(fmt, doc)

    readable_rows = {(suffix, doc.get("date"), doc.get("document"))
                     for (suffix, _), by_fmt in twins.items()
                     for doc in by_fmt.values()}
    for suffix, doc in pdfs:
        if (suffix, doc.get("date"), doc.get("document")) not in readable_rows:
            log.info("1099 Composite %s available only as PDF; "
                     "no lot detail parsed", doc.get("filename"))
            stats["form_1099b_pdf_only"] += 1

    for (suffix, base), by_fmt in twins.items():
        fmts = [f for f in ("xml", "csv") if f in by_fmt]
        doc_date = _document_date(by_fmt[fmts[0]])
        if doc_date is None:
            continue
        # Format-independent key: the XML and CSV twins share `base`,
        # so whichever is parsed first wins and the other is skipped.
        ldk = _logical_doc_key(suffix, doc_date, base)
        if not gate(ldk, "form_1099b"):
            continue
        # The XML, with the CSV as its twin; the CSV alone when the XML
        # is not on disk.
        form = None
        for fmt in reversed(fmts):
            doc = by_fmt[fmt]
            path = _bronze_file(run.dir, suffix, doc["filename"], stats)
            if path is not None:
                form = Doc(suffix, ldk, "form_1099b", path,
                           doc.get("sha256") or "", doc_date, (fmt,), form)
        if form is not None:
            yield form


def _parse_1099b_form(path: str, fmt: str) -> dict:
    return tf.parse_1099b(path, fmt)


def _write_1099b_form(conn: sqlite3.Connection, run: _Run, doc: Doc,
                      parsed: dict) -> int:
    """A form's sale rows (source='form_1099b') and its closed lots;
    returns how many of both were written."""
    stats = run.stats
    _reparse_delete(conn, doc.ldk, "form_1099b" in run.reparse_kinds, stats)
    lots = parsed.get("lots", [])
    n = _insert_parsed_transactions(
        conn, doc.suffix, lots, "form_1099b", doc.sha256, doc.ldk)
    stats["transactions_inserted"] += n
    stats["form_1099b_transactions_inserted"] += n
    written = _replace_closed_lots(
        conn, doc.ldk, "form_1099b", doc.suffix,
        [(_closed_lot_from_1099b(lot), lot) for lot in lots], doc.sha256)
    stats["closed_lots_inserted"] += written
    return n + written


# Realized-lot reports by filename prefix: the Year-End Summary (alone,
# or inside the 1099 Composite PDF) and the Gain/Loss Report.
_REALIZED_REPORT_KINDS = (
    ("1099-Composite-and-Year-End-Summary", "year_end_summary"),
    ("Year-End-Summary", "year_end_summary"),
    ("Year-End-Gain-Loss-Reporting", "gain_loss_report"),
)

# closed_lots columns a parsed lot fills, by the lot's own field names.
_CLOSED_LOT_FIELDS = (
    "tax_year", "security_name", "cusip", "instrument_key", "quantity",
    "acquired_date", "disposed_date", "proceeds", "cost_basis",
    "wash_sale_disallowed", "accrued_market_discount", "realized_gain_loss",
    "term", "covered", "form_8949_box",
)


def _closed_lot_from_1099b(row: dict) -> dict:
    """A 1099-B lot (the `form_1099b` transaction row) in the closed-lot
    shape. A 1099-B lot is covered unless flagged noncovered, prints no
    gain, and names its security by description only."""
    return {
        "tax_year": row.get("tax_year"),
        "security_name": row.get("security_name"),
        "quantity": row.get("quantity"),
        "acquired_date": row.get("acquired_date"),
        "disposed_date": row.get("date_sold"),
        "proceeds": row.get("proceeds"),
        "cost_basis": row.get("cost_basis"),
        "wash_sale_disallowed": row.get("wash_sale_disallowed"),
        "accrued_market_discount": row.get("accrued_market_discount"),
        "term": row.get("term"),
        "covered": 0 if row.get("noncovered") else 1,
        "form_8949_box": row.get("form_8949_code"),
    }


def _replace_closed_lots(conn: sqlite3.Connection, ldk: str, kind: str,
                         account_external_id: str,
                         lots: list[tuple[dict, dict]],
                         source_sha256: str) -> int:
    """Replace one document's `closed_lots` rows with `lots`, numbered in
    print order. Each lot is its closed-lot fields and the payload to
    keep. Returns the rows written."""
    conn.execute(
        "DELETE FROM closed_lots WHERE logical_doc_key = ? AND document_kind = ?",
        (ldk, kind),
    )
    cols = ", ".join(_CLOSED_LOT_FIELDS)
    marks = ", ".join("?" for _ in _CLOSED_LOT_FIELDS)
    for idx, (lot, payload) in enumerate(lots):
        conn.execute(
            "INSERT INTO closed_lots (logical_doc_key, document_kind,"
            f" lot_index, account_external_id, {cols}, footnotes,"
            f" source_sha256, payload) VALUES (?, ?, ?, ?, {marks}, ?, ?, ?)",
            (ldk, kind, idx, account_external_id,
             *(lot.get(f) for f in _CLOSED_LOT_FIELDS),
             ",".join(lot.get("footnotes") or []) or None,
             source_sha256, canonical_json(payload)),
        )
    return len(lots)


def _select_realized_reports(run: _Run, gate) -> Iterator[Doc]:
    """The realized-lot reports (Year-End Summary, Gain/Loss Report; see
    _REALIZED_REPORT_KINDS), each its own logical document."""
    for suffix, doc in _account_documents(run.manifest, "tax_form"):
        filename = doc.get("filename") or ""
        kind = next((k for prefix, k in _REALIZED_REPORT_KINDS
                     if filename.startswith(prefix)), None)
        if kind is None or _format_from_filename(filename) != "pdf":
            continue
        doc_date = _document_date(doc)
        if doc_date is None:
            continue
        ldk = _logical_doc_key(suffix, doc_date, filename)
        if not gate(ldk, kind):
            continue
        path = _bronze_file(run.dir, suffix, filename, run.stats)
        if path is None:
            continue
        yield Doc(suffix, ldk, kind, path, doc.get("sha256") or "",
                  doc_date, (kind,))


def _parse_realized_report(path: str, kind: str) -> dict:
    return tf.parse_realized_report_pdf(path, kind)


def _write_realized_report(conn: sqlite3.Connection, run: _Run, doc: Doc,
                           parsed: dict) -> int:
    """A report's rows in `closed_lots`, and a Gain/Loss Report's
    cost-basis methods in `cost_basis_methods`; returns how many of both
    were written. Each parse replaces the report's rows."""
    stats = run.stats
    if parsed["incomplete"]:
        log.warning("%s: %d realized lot(s) whose amounts could not "
                    "be read were left out", doc.ldk, parsed["incomplete"])
        stats["realized_lots_unreadable"] += parsed["incomplete"]
    written = _replace_closed_lots(
        conn, doc.ldk, doc.kind, doc.suffix,
        [(lot, lot) for lot in parsed["lots"]], doc.sha256)
    stats["closed_lots_inserted"] += written
    conn.execute("DELETE FROM cost_basis_methods WHERE logical_doc_key = ?",
                 (doc.ldk,))
    return written + silver.upsert_rows(
        conn, "cost_basis_methods",
        ("logical_doc_key", "asset_class", "account_external_id",
         "as_of_date", "tax_year", "method", "source_sha256"),
        [(doc.ldk, m["asset_class"], doc.suffix, doc.doc_date,
          parsed["tax_year"], m["method"], doc.sha256)
         for m in parsed.get("methods", [])],
        replace=False)


# The four passes, in the order a run's documents are parsed. A pass's
# key is its generation scope (GENERATIONS), the scope of its records.
PASSES = {
    "statement": PassSpec(
        kind="statement", marker_kinds=("statement",),
        select=_select_statements, parse=_parse_statement,
        write=_write_statement, error_log="PDF parse failed for %s: %s",
        error_stat="pdf_parse_errors", error_field="_error", batch=True,
        dedup_stat="statements_logical_deduped",
        finish=_harvest_account_facts),
    "third_party_distribution": PassSpec(
        kind="third_party_distribution",
        marker_kinds=("third_party_distribution",),
        select=_select_distribution_letters,
        parse=_parse_distribution_letter, write=_write_distribution_letter,
        error_log="distribution parse failed for %s: %s",
        error_stat="distribution_parse_errors"),
    "form_1099b": PassSpec(
        kind="form_1099b", marker_kinds=("form_1099b",),
        select=_select_1099b_forms, parse=_parse_1099b_form,
        write=_write_1099b_form, error_log="1099-B parse failed for %s: %s",
        error_stat="form_1099b_parse_errors"),
    "realized_report": PassSpec(
        kind="realized_report",
        marker_kinds=tuple(dict.fromkeys(
            k for _, k in _REALIZED_REPORT_KINDS)),
        select=_select_realized_reports, parse=_parse_realized_report,
        write=_write_realized_report,
        error_log="realized-lot report parse failed for %s: %s",
        error_stat="realized_report_parse_errors"),
}


def _registration_from_tax_forms(conn: sqlite3.Connection,
                                  account_external_id: str) -> str | None:
    """Fallback registration-label lookup for accounts whose
    statement PDFs didn't yield a parseable header. Schwab
    issues 5498 / 5498-ESA tax forms only for IRA / ESA
    accounts; the filename alone is enough to tell those two
    apart from each other and from anything else. Returns one
    of the same verbatim labels parse_account_registration
    surfaces, or None if no tax-form signal is available.

    Roth / Inherited / SEP / SIMPLE IRA disambiguation requires
    parsing the 5498 body and isn't done here; the loader emits
    the generic "Contributory IRA" label, which the wealthdb
    gold adapter can refine if it needs to (e.g. by re-reading
    the source PDF via documents.sha256).
    """
    rows = conn.execute(
        "SELECT filename FROM documents "
        "WHERE account_external_id = ? AND doc_kind = 'tax_form' "
        "ORDER BY doc_date DESC",
        (account_external_id,),
    ).fetchall()
    for (filename,) in rows:
        f = filename.upper()
        if f.startswith("5498-ESA"):
            return "Education Savings"
        if f.startswith("5498"):
            return "Contributory IRA"
    return None


def _reconcile_account_numbers(conn: sqlite3.Connection,
                               runs: list[Path],
                               verify: set[str] | frozenset[str]
                               = frozenset()) -> int:
    """Settle `accounts.payload.account_number_full` against the
    full bronze evidence, for two kinds of account:

    - accounts still lacking the key — statements skipped by the
      dump-run / logical-doc gates, or headers that conflicted;
    - accounts in `verify` — those whose key this invocation's
      in-run harvest set from a previously-absent state. Such a
      key rests only on the statements parsed this time, so it is
      provisional until checked against every statement in bronze:
      agreement keeps it, a conflict clears it (the same
      consistent-or-absent rule via _apply_account_number). This
      check makes the end-of-load key independent of the order
      the bronze runs were loaded in.

    Statement PDFs are re-read from the bronze tree (header-only;
    the row parsers don't run). Each logical statement (unique
    filename per account) is read once, preferring the bronze run
    that first recorded it and falling back to any run that still
    holds the file (a pruned bronze dir just narrows the
    evidence). Accounts that end up without a key are re-attempted
    on the next load — cheap for accounts with no statements at
    all, and bounded by the account's statement count otherwise.

    Returns the number of accounts whose payload changed."""
    missing = {r[0] for r in conn.execute(
        "SELECT DISTINCT account_external_id FROM accounts"
        " WHERE json_extract(payload, '$.account_number_full') IS NULL"
    ).fetchall()}
    targets = sorted(missing | set(verify))
    if not targets:
        return 0
    run_by_snapshot = {parse_snapshot_at(r.name): r for r in runs}
    n_updated = 0
    for suffix in targets:
        docs = conn.execute(
            "SELECT snapshot_at, filename FROM documents"
            " WHERE account_external_id = ? AND doc_kind = 'statement'"
            "   AND file_format = 'pdf'"
            " ORDER BY snapshot_at, filename",
            (suffix,),
        ).fetchall()
        numbers: set[str] = set()
        seen_filenames: set[str] = set()
        for snapshot_at, filename in docs:
            if filename in seen_filenames:
                continue
            seen_filenames.add(filename)
            first_run = run_by_snapshot.get(snapshot_at)
            candidates = ([first_run] if first_run else []) + [
                r for r in runs if r is not first_run
            ]
            pdf_path = None
            for r in candidates:
                p = r / "statements" / suffix / filename
                if p.is_file():
                    pdf_path = p
                    break
            if pdf_path is None:
                continue
            try:
                num = pp.parse_statement_account_number(pdf_path)
            except Exception as e:
                log.warning("account-number backfill: parse failed "
                            "for %s: %r", pdf_path, e)
                continue
            if num:
                numbers.add(num)
        if numbers and _apply_account_number(conn, suffix, numbers):
            n_updated += 1
    return n_updated


# Position fields that add up when one instrument fills several rows of
# a statement. The rest (price, description, section, yield) are the
# instrument's own and are taken from its first row.
_ADDITIVE_POSITION_FIELDS = (
    "quantity", "market_value", "cost_basis", "unrealized_gain_loss",
    "accrued_interest", "est_annual_income",
)


def _merge_same_instrument(positions: list[dict]) -> list[dict]:
    """One row per instrument_key. A statement can list one instrument on
    more than one row, and silver keys a position by instrument, so the
    rows are summed rather than left to overwrite each other. A sum that
    has a missing part is missing. The rows' lots are kept in print
    order."""
    merged: dict[str, dict] = {}
    for pos in positions:
        key = pos.get("instrument_key")
        if key not in merged:
            merged[key] = dict(pos)
            continue
        into = merged[key]
        for f in _ADDITIVE_POSITION_FIELDS:
            a, b = into.get(f), pos.get(f)
            into[f] = None if a is None or b is None else a + b
        into["raw_lines"] = (into.get("raw_lines") or []) + (pos.get("raw_lines") or [])
        into["lots"] = (into.get("lots") or []) + (pos.get("lots") or [])
        into["footnotes"] = sorted(set(into.get("footnotes") or [])
                                   | set(pos.get("footnotes") or [])) or None
    return list(merged.values())


def _check_account_value(parsed: dict, ldk: str) -> None:
    """Warn when a statement's positions plus closing cash do not add up
    to the account value it prints. Every row the parser misses or
    misreads shows up here, so a new layout variant is caught at load
    rather than in the returns built on it."""
    printed = parsed.get("account_value")
    if printed is None:
        return
    cash = (parsed.get("cash_summary") or {}).get("closing_balance") or 0.0
    total = cash + sum(p.get("market_value") or 0.0
                       for p in parsed.get("positions") or [])
    if abs(total - printed) >= 0.01:
        log.warning(
            "statement %s: positions and cash add up to %.2f, not the "
            "%.2f the statement prints as its account value", ldk, total,
            printed)


# The content columns of a statement's snapshot rows, as the row builders
# below emit them. Each table's rows also carry `source_sha256`, and the
# two snapshot tables `logical_doc_key`, the statement that wrote them.
_POSITION_COLUMNS = (
    "as_of_date", "account_external_id", "instrument_key", "quantity",
    "market_price", "market_value", "cost_basis", "unrealized_gain_loss",
    "accrued_interest", "payload",
)
_OPEN_LOT_COLUMNS = (
    "as_of_date", "account_external_id", "instrument_key", "lot_index",
    "quantity", "unit_cost", "cost_basis", "acquired_date",
    "unrealized_gain_loss", "term", "covered", "footnotes", "payload",
)
_CASH_COLUMNS = (
    "period_end", "period_start", "account_external_id", "currency_iso",
    "opening_balance", "closing_balance", "total_debits", "total_credits",
    "payload", "interest_rate", "interest_rate_line",
)


def _position_rows(account_external_id: str, as_of_date: int,
                   positions: list[dict]) -> tuple[list[tuple], list[tuple]]:
    """One statement's `historical_position_snapshots` rows and the
    `open_lots` rows of their tax lots, in _POSITION_COLUMNS and
    _OPEN_LOT_COLUMNS order.

    Rows of one instrument are summed first (_merge_same_instrument).
    Rows missing an instrument_key are dropped (parser failure
    indicator); a position row with no symbol can't be joined against
    anything downstream. Lots are numbered in print order; their figures
    are as printed, and whether a lot is covered is never printed, so
    `covered` stays NULL."""
    pos_rows: list[tuple] = []
    lot_rows: list[tuple] = []
    for pos in _merge_same_instrument(positions):
        instrument_key = pos.get("instrument_key")
        if not instrument_key:
            log.warning(
                "skipping position with no instrument_key in payload "
                "(account=%s, as_of=%s)",
                bronze.log_id(account_external_id), as_of_date,
            )
            continue
        payload = canonical_json({
            "section": pos.get("section"),
            "description": pos.get("description"),
            "est_yield": pos.get("est_yield"),
            "est_annual_income": pos.get("est_annual_income"),
            "pct_of_acct": pos.get("pct_of_acct"),
            # Schwab Endnote markers on this holding (e.g. "e" = edited
            # by the account holder, "t" = by a third party) — flags a
            # holder-provided valuation. None when unmarked.
            "footnotes": pos.get("footnotes"),
            "raw_lines": pos.get("raw_lines"),
        })
        pos_rows.append((
            as_of_date, account_external_id, instrument_key,
            pos.get("quantity"), pos.get("market_price"),
            pos.get("market_value"), pos.get("cost_basis"),
            pos.get("unrealized_gain_loss"), pos.get("accrued_interest"),
            payload,
        ))
        for idx, lot in enumerate(pos.get("lots") or []):
            lot_rows.append((
                as_of_date, account_external_id, instrument_key, idx,
                lot.get("quantity"), lot.get("unit_cost"),
                lot.get("cost_basis"), lot.get("acquired_date"),
                lot.get("unrealized_gain_loss"), lot.get("term"), None,
                ",".join(lot.get("footnotes") or []) or None,
                canonical_json({"holding_days": lot.get("holding_days"),
                                "raw_line": lot.get("raw_line")}),
            ))
    return pos_rows, lot_rows


def _cash_rows(account_external_id: str, period_end: int, period_start: int,
               cash: dict | None) -> list[tuple]:
    """One statement's `historical_cash_balances` row, in _CASH_COLUMNS
    order: none without a cash summary, else one for its currency.

    NULLs are preserved: missing opening / closing / debits / credits
    stay NULL in the row rather than being coerced to 0.0 — the consumer
    needs to distinguish "not reported" from "actually zero". So does
    the interest rate: NULL where the statement states none."""
    if not cash:
        return []
    payload = canonical_json({
        "deposits":            cash.get("deposits"),
        "withdrawals":         cash.get("withdrawals"),
        "purchases":           cash.get("purchases"),
        "sales_redemptions":   cash.get("sales_redemptions"),
        "dividends_interest":  cash.get("dividends_interest"),
        "expenses":            cash.get("expenses"),
        "other_activity":      cash.get("other_activity"),
        "raw_line":            cash.get("raw_line"),
    })
    return [(
        period_end, period_start, account_external_id,
        cash.get("currency_iso") or "USD",
        cash.get("opening_balance"), cash.get("closing_balance"),
        cash.get("total_debits"), cash.get("total_credits"), payload,
        cash.get("interest_rate"), cash.get("interest_rate_line"),
    )]


def _insert_position_snapshots(conn: sqlite3.Connection,
                                account_external_id: str,
                                as_of_date: int,
                                positions: list[dict],
                                source_sha256: str,
                                logical_doc_key: str) -> int:
    """INSERT OR REPLACE one row per parsed position, and write the
    statement's tax lots to `open_lots`. Returns the count of position
    rows written. PK is (as_of_date, account_external_id,
    instrument_key) — a re-parse of the same logical statement
    (different sha256, same content) collapses onto the same row. The
    statement's lots replace whatever lots silver held for it."""
    pos_rows, lot_rows = _position_rows(account_external_id, as_of_date,
                                        positions)
    conn.execute(
        "DELETE FROM open_lots WHERE account_external_id = ? AND as_of_date = ?",
        (account_external_id, as_of_date),
    )
    silver.upsert_rows(conn, "historical_position_snapshots",
                       (*_POSITION_COLUMNS, "source_sha256", "logical_doc_key"),
                       [(*r, source_sha256, logical_doc_key) for r in pos_rows])
    silver.upsert_rows(conn, "open_lots",
                       (*_OPEN_LOT_COLUMNS, "source_sha256"),
                       [(*r, source_sha256) for r in lot_rows], replace=False)
    return len(pos_rows)


def _insert_cash_balance(conn: sqlite3.Connection,
                          account_external_id: str,
                          period_end: int,
                          period_start: int,
                          cash: dict | None,
                          source_sha256: str,
                          logical_doc_key: str) -> int:
    """INSERT OR REPLACE one row per (period_end, account, currency).
    Returns 0 (no cash_summary parsed) or 1."""
    rows = _cash_rows(account_external_id, period_end, period_start, cash)
    silver.upsert_rows(conn, "historical_cash_balances",
                       (*_CASH_COLUMNS, "source_sha256", "logical_doc_key"),
                       [(*r, source_sha256, logical_doc_key) for r in rows])
    return len(rows)


def _stored_statement_rows(conn: sqlite3.Connection, account_external_id: str,
                           period_end: int) -> tuple[list[tuple], ...]:
    """The content of the snapshot rows silver holds for an account and
    period end, in the row builders' shape and sorted."""
    def rows(table, columns, date_column):
        return sorted(conn.execute(
            f"SELECT {', '.join(columns)} FROM {table}"
            f" WHERE account_external_id = ? AND {date_column} = ?",
            (account_external_id, period_end)).fetchall())
    return (rows("historical_position_snapshots", _POSITION_COLUMNS, "as_of_date"),
            rows("open_lots", _OPEN_LOT_COLUMNS, "as_of_date"),
            rows("historical_cash_balances", _CASH_COLUMNS, "period_end"))


def _snapshot_owner(conn: sqlite3.Connection, account_external_id: str,
                    period_end: int, logical_doc_key: str) -> str | None:
    """Another statement whose snapshot rows hold this account and period
    end, or None."""
    row = conn.execute(
        "SELECT logical_doc_key FROM historical_position_snapshots"
        " WHERE account_external_id = ? AND as_of_date = ?"
        "   AND logical_doc_key != ?"
        " UNION ALL"
        " SELECT logical_doc_key FROM historical_cash_balances"
        " WHERE account_external_id = ? AND period_end = ?"
        "   AND logical_doc_key != ?"
        " LIMIT 1",
        (account_external_id, period_end, logical_doc_key) * 2,
    ).fetchone()
    return row[0] if row else None


def _write_statement_snapshots(conn: sqlite3.Connection, job: dict,
                               parsed: dict, period_start: int,
                               period_end: int, stats: dict) -> int:
    """Write a parsed statement's positions, lots and cash, keyed by
    account and the period end it prints. Returns the position and cash
    rows written.

    The rows of one account and period end belong to the first statement
    that writes them. A later statement with the same account and period
    end writes nothing: when its rows would be the same, it is a copy and
    is counted; when they would differ, a warning names both statements.
    A re-parse of the owning statement replaces its rows."""
    suffix, ldk = job["suffix"], job["ldk"]
    positions = parsed.get("positions") or []
    cash = parsed.get("cash_summary")
    owner = _snapshot_owner(conn, suffix, period_end, ldk)
    if owner is not None:
        mine = (*(sorted(r) for r in _position_rows(suffix, period_end, positions)),
                sorted(_cash_rows(suffix, period_end, period_start, cash)))
        if mine == _stored_statement_rows(conn, suffix, period_end):
            log.info("statement %s repeats the rows of %s; not written again",
                     ldk, owner)
            stats["statements_copies"] += 1
        else:
            log.warning("statement %s has the account and period end of %s "
                        "but other positions or cash; the rows of %s are "
                        "kept", ldk, owner, owner)
            stats["statements_conflicting"] += 1
        return 0
    held = _insert_position_snapshots(
        conn, suffix, period_end, positions, job["sha256"], ldk)
    stats["positions_inserted"] += held
    cash_rows = _insert_cash_balance(
        conn, suffix, period_end, period_start, cash, job["sha256"], ldk)
    stats["cash_balances_inserted"] += cash_rows
    return held + cash_rows


# ============================================================
# Top-level
# ============================================================

def _purge_stale_snapshots(conn: sqlite3.Connection,
                           kinds: tuple[str, ...] | None = None) -> None:
    """Drop the parse markers, so a document whose re-parse does not
    commit is parsed by the next load rather than skipped: all of them, or
    those of `kinds`. With the statements' markers go the
    statement-derived snapshot tables, which have no delete path of their
    own, so the re-walk refills them rather than adding to them.

    Called whenever a generation has moved — never on a bare `--reparse`,
    which re-walks under keys that have not changed and so has nothing to
    collapse. A stale generation also forces the re-parse of what it
    covers, which is what guarantees the walk that refills these tables
    actually happens.
    """
    if kinds is None:
        conn.execute("DELETE FROM parsed_documents")
    else:
        conn.execute(
            "DELETE FROM parsed_documents WHERE document_kind IN"
            f" ({', '.join('?' for _ in kinds)})", kinds)
        if "statement" not in kinds:
            return
    dropped = sum(conn.execute(f"DELETE FROM {t}").rowcount
                  for t in _SNAPSHOT_TABLES)
    log.info("the document parsers have changed since these rows were "
             "written; dropped %d snapshot row(s) for re-derivation", dropped)


def _drop_older_records(conn: sqlite3.Connection) -> None:
    """Drop each kind's records under another generation, once a load has
    failed nowhere. A moved kind has then re-derived every document it
    could read and recorded the copy it read, a failed parse included.
    The records left under another generation are the copies it did not
    read again: gone from bronze, or another copy of a document it read.
    The kind then reads as current. A kind that has not moved holds no
    such record.

    After a failure this is not called, so the older records stay and the
    next load re-derives again."""
    conn.executemany(
        "DELETE FROM document_derivations WHERE scope = ?"
        " AND generation != ?", GENERATIONS.items())


def run_load(args: argparse.Namespace) -> int:
    migrations_dir = _resolve_migrations_dir(args.migrations_dir)
    args.silver_db.parent.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(str(args.silver_db))
    silver.own_only(args.silver_db)
    cache = parsecache.ParseCache(parsecache.default_dir("schwab-web"))
    extraction.CACHE_DIR = extract.default_dir("schwab-web")
    try:
        conn.execute("PRAGMA foreign_keys = ON")
        silver.apply_migrations(conn, migrations_dir)
        rebuild.rebuild_advice(conn, REBUILD_ADVICE, REBUILD_COMMAND,
                               bronze_dir=args.bronze_dir)

        # A moved walk owes the archive a re-read, and `--reparse` is the
        # lever that already delivers one: it opens the run gate, the
        # per-document gates and the per-document deletes together.
        #
        # The two conditions are kept apart on purpose. The purge follows
        # the GENERATION, never the flag: it is the only delete path the
        # snapshot tables have, so gating it on `reparse` would let
        # `load --reparse` over a moved parser re-walk the archive, insert
        # the re-keyed rows beside the old ones, and then settle the
        # generation as current — which puts that miss permanently out of
        # reach of a later load.
        #
        # A moved document kind owes only its own documents a re-read: its
        # pass runs again over every run, and nothing else does.
        walk_stale = silver.stale_generation(conn, WALK_SCOPE, WALK_GENERATION)
        stale = frozenset(
            kind for kind in PASSES
            if derivations.generation_moved(conn, kind, GENERATIONS[kind]))
        rewalk = args.reparse or walk_stale
        if walk_stale:
            log.info("the document parsers have changed since silver was "
                     "written; re-parsing the archive")
            _purge_stale_snapshots(conn)
        elif stale:
            for kind in PASSES:
                if kind in stale:
                    log.info("%s: the parser has changed since silver was "
                             "written; re-deriving its documents from every "
                             "run", kind)
                    _purge_stale_snapshots(conn, PASSES[kind].marker_kinds)
        conn.commit()
        reparse_kinds = frozenset(PASSES) if rewalk else stale
        rederive = frozenset() if rewalk else stale

        runs = discover_bronze_runs(args.bronze_dir)
        log.info("found %d bronze run(s) under %s", len(runs), args.bronze_dir)

        # One parse pool and one parse-dedup set for the whole invocation.
        # A per-run pool would pay a spawn/teardown cycle for every bronze
        # run; a per-run dedup set would re-parse, under `--reparse`, every
        # logical document once per bronze run that carries it. The pool
        # spawns its workers only when a pass has two documents or more to
        # parse, and not at all with --workers 1.
        seen_logical_docs: set[tuple] = set()
        # Suffixes whose account_number_full was pinned by this
        # invocation's in-run harvest, accumulated only from runs
        # that committed (a rolled-back run's stats are discarded
        # with it). Fed to the reconcile pass below.
        pinned_accounts: set[str] = set()
        # Any run or the reconcile pass rolled back: the walk is then left
        # unstamped and a moved kind's older records stay, so the next load
        # re-walks or re-derives rather than skipping a run whose rows a
        # purge removed, and the load exits 1. The other runs still load.
        failed = False
        with _parse_pool(cache, args.workers) as pool:
            for run_dir in runs:
                snapshot_at = parse_snapshot_at(run_dir.name)
                derive_only = None
                if already_loaded(conn, snapshot_at) and not rewalk:
                    if not rederive:
                        log.info("skipping %s (already loaded)", run_dir.name)
                        continue
                    derive_only = rederive
                    log.info("re-deriving %s (snapshot_at=%d)",
                             run_dir.name, snapshot_at)
                else:
                    log.info("loading %s (snapshot_at=%d)", run_dir.name,
                             snapshot_at)
                # load_run marks its docs seen during the walk, before the
                # commit below. Snapshot the set so a rolled-back run's
                # marks are undone — otherwise a doc first seen in a run
                # that later fails would be skipped by every later run,
                # silently dropping its rows.
                seen_before = set(seen_logical_docs)
                try:
                    stats = load_run(
                        conn, run_dir, reparse=rewalk,
                        workers=args.workers, pool=pool,
                        seen_logical_docs=seen_logical_docs,
                        reparse_kinds=reparse_kinds, derive_only=derive_only,
                    )
                    conn.commit()
                    pinned_accounts |= stats["account_numbers_pinned"]
                    log.info(
                        "%s %s: accts +%d/-%d, docs +%d/-%d, "
                        "tx +%d (reparsed %d), positions +%d, cash +%d, "
                        "logical-dup %d, copies %d, conflicts %d, "
                        "registrations %d, "
                        "acct-numbers %d, pdf errors %d; "
                        "1099-B +%d (pdf-only %d, errors %d), "
                        "closed lots +%d (unreadable %d, report errors %d), "
                        "distributions +%d (errors %d)",
                        "loaded" if derive_only is None else "re-derived",
                        run_dir.name,
                        stats["accounts_inserted"], stats["accounts_deduped"],
                        stats["documents_new"], stats["documents_dup"],
                        stats["transactions_inserted"],
                        stats["transactions_reparsed"],
                        stats["positions_inserted"],
                        stats["cash_balances_inserted"],
                        stats["statements_logical_deduped"],
                        stats["statements_copies"],
                        stats["statements_conflicting"],
                        stats["account_registration_updated"],
                        stats["account_number_updated"],
                        stats["pdf_parse_errors"],
                        stats["form_1099b_transactions_inserted"],
                        stats["form_1099b_pdf_only"],
                        stats["form_1099b_parse_errors"],
                        stats["closed_lots_inserted"],
                        stats["realized_lots_unreadable"],
                        stats["realized_report_parse_errors"],
                        stats["distribution_transactions_inserted"],
                        stats["distribution_parse_errors"],
                    )
                except Exception:
                    conn.rollback()
                    seen_logical_docs = seen_before  # undo this run's marks
                    failed = True
                    log.exception("load failed for %s; rolled back", run_dir.name)

        # Settle account_number_full against the full bronze
        # evidence: an account whose statements this invocation did
        # not parse never passed through the in-run harvest (its runs
        # are dump_runs-skipped above), and a key the harvest pinned
        # this invocation rests only on the statements parsed this
        # time. Header-only re-read from
        # bronze for both; no-op once every account carries a
        # verified key and no new pin happened.
        try:
            n_reconciled = _reconcile_account_numbers(
                conn, runs, pinned_accounts)
            if n_reconciled:
                conn.commit()
                log.info("reconciled account_number_full for %d account(s)",
                         n_reconciled)
        except Exception:
            conn.rollback()
            failed = True
            log.exception("account-number reconcile failed; rolled back")

        if failed and (walk_stale or stale):
            log.warning("parser generations left as they were after a "
                        "failure; the next load re-reads what they cover")
        elif failed:
            log.warning("a load step failed and was rolled back; "
                        "the next load runs it again")
        else:
            silver.stamp_generation(conn, WALK_SCOPE, WALK_GENERATION)
            _drop_older_records(conn)
            conn.commit()
        cache.sweep(GENERATIONS.values())
        extract.sweep(extraction.CACHE_DIR,
                      {extract.extraction_key(e)
                       for e in extraction.EXTRACTORS})

        _log_registration_histogram(conn)
        _log_account_number_coverage(conn)
    finally:
        extraction.CACHE_DIR = None
        conn.close()
    return 1 if failed else 0


def _log_registration_histogram(conn: sqlite3.Connection) -> None:
    """Print the per-account registration label landed in
    silver after all runs are loaded. Cheap sanity check that
    the parser kept up with Schwab's layout drift — if a label
    that was present last run is now NULL, the most likely
    cause is a header anchor regressing.
    """
    rows = conn.execute(
        "SELECT account_external_id, account_registration FROM accounts "
        "WHERE account_registration IS NOT NULL "
        "GROUP BY account_external_id "
        "ORDER BY account_external_id"
    ).fetchall()
    null_count = conn.execute(
        "SELECT COUNT(DISTINCT account_external_id) FROM accounts "
        "WHERE account_registration IS NULL"
    ).fetchone()[0]
    log.info("account_registration after load:")
    for acct, reg in rows:
        log.info("  %s  %s", bronze.log_id(acct), reg)
    if null_count:
        log.warning(
            "  %d account(s) have NULL account_registration "
            "(no parseable statement header)", null_count,
        )


def _log_account_number_coverage(conn: sqlite3.Connection) -> None:
    """Report how many accounts carry payload.account_number_full —
    the api↔web bridge key (INTEROP.md §1). The values themselves
    stay out of the log; accounts without the key fall back to
    gold's suffix matching, so a shrinking count here means the
    header anchors drifted or statements started disagreeing."""
    total = conn.execute(
        "SELECT COUNT(DISTINCT account_external_id) FROM accounts"
    ).fetchone()[0]
    missing = conn.execute(
        "SELECT COUNT(DISTINCT account_external_id) FROM accounts "
        "WHERE json_extract(payload, '$.account_number_full') IS NULL"
    ).fetchone()[0]
    log.info("account_number_full present for %d of %d account(s)",
             total - missing, total)
    if missing:
        log.warning(
            "  %d account(s) lack account_number_full "
            "(no consistent statement header)", missing,
        )


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)
    if args.force:
        rebuild.runs_without_bronze(args.silver_db, args.bronze_dir)
        silver.reset(args.silver_db)
    return run_load(args)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
