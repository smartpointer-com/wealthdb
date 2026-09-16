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
    same rows via INSERT OR IGNORE. The load gate uses the
    `logical_doc_key` column (account + doc_date + filename) so
    re-downloads of the same logical PDF are skipped. Use
    --reparse to force re-ingestion of a logical document (deletes
    by logical_doc_key, then re-inserts).

Usage:
    load.py --silver-db <path.db> --bronze-dir <root>
            [--migrations-dir <dir>] [--reparse] [-v]
"""

from __future__ import annotations

import argparse
import contextlib
import hashlib
import json
import logging
import os
import re
import sqlite3
import sys
import time
from concurrent.futures import ProcessPoolExecutor
from concurrent.futures.process import BrokenProcessPool
from datetime import datetime, timezone
from pathlib import Path

from collectorkit import cli, silver, srcfp

# Re-export for backward compatibility with existing tests that call
# load.apply_migrations(...) / load._current_schema_version(...) directly.
apply_migrations = silver.apply_migrations
_current_schema_version = silver.current_schema_version

import pdf_parsers as pp
import tax_form_parsers as tf

# `parser_generations` (migration 0005) records which generation of the
# document parsers produced the rows in hand. A stale one implies
# `--reparse` for the whole invocation — the single lever the run gate, the
# per-document gates and the per-document deletes all already read — plus a
# purge of the two snapshot tables, which have no delete path of their own.
DOCUMENT_GENERATION_SCOPE = "documents"

# Written exclusively by the statement passes, and keyed on parser output
# (`as_of_date` / `period_end`, `instrument_key`), so a moved capture
# strands the old row under a key nothing will write again.
_SNAPSHOT_TABLES = ("historical_position_snapshots", "historical_cash_balances")
from numparse import parse_amount

log = logging.getLogger("schwab-web.load")

# Bronze run-dir names look like `20260520T120000Z`. Parsed into
# Unix seconds UTC for snapshot_at.
_RUN_TS_RE = re.compile(r"^(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})Z$")

# Manifest doc dates come from the Schwab UI as MM/DD/YYYY. We
# convert to Unix seconds UTC at midnight.
_DOC_DATE_RE = re.compile(r"^(\d{1,2})/(\d{1,2})/(\d{4})$")

# Where to look for migrations when --migrations-dir isn't passed.
DEFAULT_MIGRATIONS_DIRS = (
    Path("/app/migrations"),                              # in-container
    Path(__file__).resolve().parent / "migrations",       # local dev
)

# Manifest doc_type → silver doc_kind. Trade Confirms are
# explicitly skipped upstream by download.py, but we include the
# mapping for forward-compat.
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
        help=("Re-parse every statement PDF whose sha256 is already "
              "in `documents`, even if `transactions` already has "
              "rows for it. Deletes old transactions for that "
              "source first, then re-inserts. Use after a parser fix."),
    )
    p.add_argument(
        "--workers", type=int, default=None,
        help=("Number of parallel worker processes for PDF parsing. "
              "Default: os.cpu_count(). PDF parsing is CPU-bound "
              "(text extraction via PDFium runs in C++ but each PDF "
              "is one shot, so we get the speedup by sharding across "
              "cores). Pass --workers 1 to force serial — useful for "
              "debugging."),
    )
    cli.add_standard_args(p, verb="load")
    return p.parse_args(argv)


# ============================================================
# Parallel-parse worker
# ============================================================
#
# Module-level so ProcessPoolExecutor can pickle it. Imports
# pdf_parsers afresh in each worker (Python's fork semantics
# share heap state, but spawn on macOS does not — write code
# that works under both).

def _parse_pdf_worker(args: tuple[str, int]) -> dict:
    """Worker target: parse one PDF, return the parsed dict
    (or `{"_error": "<repr>"}` on failure so the parent can log
    and continue rather than crashing the whole pool).
    """
    pdf_path, year_hint = args
    try:
        return pp.parse_statement_pdf(pdf_path, statement_year=year_hint)
    except Exception as e:
        return {"_error": repr(e)}


def _resolve_worker_count(workers: int | None) -> int:
    """Parse-worker process count: the requested value when positive,
    otherwise one per CPU. PDF text extraction (PDFium in C++) is
    CPU-bound, so the speedup comes from sharding PDFs across cores."""
    if workers and workers > 0:
        return workers
    return os.cpu_count() or 1


def _parse_chunksize(n_tasks: int) -> int:
    """Group a few PDFs per pool hand-off on large runs so per-task IPC
    doesn't dominate the tens-of-ms PDFium extraction; a chunksize of 1
    keeps latency low on the small batches later bronze runs produce."""
    return max(1, min(8, n_tasks // 20))


class _ParsePoolManager:
    """A process pool shared across an invocation's bronze runs that heals
    after a worker crash.

    The executor is created lazily on first use and reused for every run
    (one pool per invocation instead of one per run). If a parse batch
    trips BrokenProcessPool — a worker died, which under heavy host
    contention can happen — the poisoned executor is discarded and re-
    raised so the caller can retry that batch serially; the next batch
    spins up a fresh executor. A transient worker death therefore
    degrades to a slower run instead of failing the whole load."""

    def __init__(self, worker_count: int):
        self._worker_count = worker_count
        self._pool: ProcessPoolExecutor | None = None

    def map(self, fn, tasks: list, chunksize: int) -> list:
        if self._pool is None:
            self._pool = ProcessPoolExecutor(max_workers=self._worker_count)
        try:
            return list(self._pool.map(fn, tasks, chunksize=chunksize))
        except BrokenProcessPool:
            self.shutdown()  # drop the poisoned executor; next map recreates
            raise

    def shutdown(self) -> None:
        if self._pool is not None:
            self._pool.shutdown(wait=False, cancel_futures=True)
            self._pool = None

    def __enter__(self) -> "_ParsePoolManager":
        return self

    def __exit__(self, *exc) -> bool:
        self.shutdown()
        return False


def _parse_statements(parse_jobs: list[dict],
                      pool: "_ParsePoolManager | None",
                      workers: int | None) -> list[dict]:
    """Parse every collected statement PDF, returning results aligned
    positionally with `parse_jobs`.

    workers == 1 keeps the parse in-process (the path unit tests take so
    a monkeypatched parser is honoured — pool workers run in subprocesses
    that don't inherit the patch). Otherwise the work fans across a
    process pool: a caller-supplied `pool` is reused across bronze runs
    (one pool per invocation instead of one per run), and when none is
    supplied a run-local pool is created so direct callers still get
    parallelism. map preserves input order, so the serial insert phase —
    and the resulting silver — is identical regardless of worker count.

    If the shared pool loses a worker mid-batch, this run's PDFs are
    re-parsed serially in-process so the run still completes; the manager
    rebuilds the pool for later runs."""
    tasks = [(j["pdf_path"], j["year_hint"]) for j in parse_jobs]
    if workers != 1 and pool is not None:
        try:
            return pool.map(_parse_pdf_worker, tasks, _parse_chunksize(len(tasks)))
        except BrokenProcessPool:
            log.warning("parse pool lost a worker; parsing this run's %d "
                        "PDF(s) serially", len(tasks))
            return [_parse_pdf_worker(t) for t in tasks]
    worker_count = 1 if workers == 1 else min(
        _resolve_worker_count(workers), len(parse_jobs))
    if worker_count == 1:
        return [_parse_pdf_worker(t) for t in tasks]
    with ProcessPoolExecutor(max_workers=worker_count) as local_pool:
        return list(local_pool.map(_parse_pdf_worker, tasks,
                                   chunksize=_parse_chunksize(len(tasks))))


# ============================================================
# Helpers
# ============================================================

def canonical_json(obj) -> str:
    """JSON encoding suitable for content-dedup: stable key order,
    no whitespace, ensure_ascii=False so non-ASCII labels compare
    bit-for-bit. Matches schwab-api's convention."""
    return json.dumps(obj, sort_keys=True, separators=(",", ":"),
                      ensure_ascii=False)


def parse_snapshot_at(dump_dir_name: str) -> int:
    """Parse `YYYYMMDDTHHMMSSZ` → Unix seconds UTC."""
    m = _RUN_TS_RE.match(dump_dir_name)
    if not m:
        raise ValueError(f"bad bronze dir name: {dump_dir_name!r}")
    yyyy, mm, dd, h, mn, s = (int(x) for x in m.groups())
    return int(datetime(yyyy, mm, dd, h, mn, s,
                        tzinfo=timezone.utc).timestamp())


def parse_doc_date(s: str) -> int | None:
    """Parse "MM/DD/YYYY" → Unix seconds UTC at midnight. Returns
    None for blank/unparseable input — the loader treats that as
    a parser-output gap and skips the row."""
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


def parse_iso_date(s: str | None) -> int | None:
    """Parse "YYYY-MM-DD" (pdf_parsers output format) → Unix
    seconds UTC at midnight. Returns None for blank/unparseable."""
    if not s:
        return None
    try:
        return int(datetime.strptime(s, "%Y-%m-%d")
                   .replace(tzinfo=timezone.utc).timestamp())
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
    return sorted(
        p for p in bronze_dir.iterdir()
        if p.is_dir() and _RUN_TS_RE.match(p.name)
    )


def already_loaded(conn: sqlite3.Connection, snapshot_at: int) -> bool:
    row = conn.execute(
        "SELECT 1 FROM dump_runs WHERE snapshot_at = ?", (snapshot_at,),
    ).fetchone()
    return row is not None


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

    `index` is the 0-based position of this transaction within the parsed
    list for a single logical statement (same account + period). It is
    stable because PDFium's text extraction is deterministic for the same
    logical PDF content, and tx-history JSON exports preserve Schwab's
    server-side ordering. This disambiguates genuinely-distinct transactions
    that share the same (date, amount, description, symbol) — e.g. two
    identical $50 fees booked on the same day.

    Hex SHA-256, first 32 chars (128 bits — collision probability is
    negligible at our scale)."""
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
    Used to gate transaction loading: if any transactions for this logical
    document already exist in silver we can skip re-parsing, and for
    --reparse delete to clear all sha256-churn variants at once.

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
    label/nickname change from shedding the key.

    Returns True if a row was inserted, False if dedup skipped it."""
    row = conn.execute(
        "SELECT payload FROM accounts WHERE account_external_id = ? "
        "ORDER BY snapshot_at DESC LIMIT 1",
        (account_external_id,),
    ).fetchone()
    if row is not None:
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
        " (snapshot_at, account_external_id, nickname, payload)"
        " VALUES (?, ?, ?, ?)",
        (snapshot_at, account_external_id, nickname, payload),
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
            "account …%s: %d conflicting full account numbers across "
            "statement headers; leaving account_number_full unset",
            account_external_id, len(candidates),
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


def _account_nickname(label: str | None, suffix: str) -> str | None:
    """Heuristic: extract a human-friendly nickname from a Schwab
    dropdown label.

    The label is rendered as the literal text of the dropdown's
    sdps-account-selector__left-col + …NNN suffix + an "Account
    ending in N N N" sr-only span. We strip the suffix and the
    sr-only echo, leaving just the chosen account name.

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
             pool: "_ParsePoolManager | None" = None,
             seen_logical_docs: set[tuple] | None = None) -> dict:
    """Load one bronze-run dir. Returns a stats dict for logging.

    `pool` and `seen_logical_docs`, when supplied by run_load, are shared
    across all of an invocation's bronze runs so each logical statement
    is parsed once. Left at their defaults (a run-local pool and a fresh
    set) a direct call keeps the original per-run scope.

    A logical doc marked here is recorded in `seen_logical_docs` during
    the manifest walk, before this run's inserts commit. run_load owns
    the rollback: it snapshots the set before the run and restores it if
    the run fails, so a rolled-back run never leaves a doc marked seen
    (which would wrongly skip it on every later run)."""
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
        "positions_inserted": 0,
        "cash_balances_inserted": 0,
        "statements_logical_deduped": 0,
        "account_registration_updated": 0,
        "account_number_updated": 0,
        # Not a counter: suffixes whose account_number_full went
        # from absent to set on this run's parses alone. run_load
        # re-verifies them against the full bronze evidence
        # (_reconcile_account_numbers).
        "account_numbers_pinned": set(),
    }
    # Parse-dedup set: (account_suffix, doc_date, doc_kind, filename).
    # Schwab regenerates statement PDFs on every download (different
    # sha256 each time — see INTEROP.md §3), but the parser output is
    # byte-identical for the same logical document. The first sha256
    # seen per logical doc is parsed; subsequent re-downloads skip the
    # positions / cash / transactions parse. run_load shares this set
    # across the invocation's bronze runs so a statement that yields no
    # transactions (whose transaction gate never closes on its own) is
    # not re-parsed in every later run; a run-local default restores the
    # per-run scope for direct callers.
    if seen_logical_docs is None:
        seen_logical_docs = set()

    # Parse jobs accumulated during the manifest walk. Each entry
    # captures everything the serial-insert phase needs (suffix,
    # sha256, doc_date, gate flags). The actual PDF parse happens
    # in a worker pool once the walk completes — see the
    # parallel-parse block below.
    parse_jobs: list[dict] = []
    manifest_path = run_dir / "run.json"
    if not manifest_path.is_file():
        log.warning("no run.json in %s; skipping", run_dir)
        return stats

    with manifest_path.open("r", encoding="utf-8") as fh:
        manifest = json.load(fh)

    # Skip non-complete dumps. download.walk() writes run.json
    # incrementally with status="in-progress", flipping it to
    # "complete" (or "dry-run") only at the end — so a crashed walk
    # or a --dry-run shell leaves a partial manifest present that we
    # must NOT ingest (partial balances would leak into gold as a
    # snapshot; a dry-run's tx-history exports still fire). A
    # statusless manifest is treated as loadable.
    status = manifest.get("status")
    if status in ("in-progress", "dry-run"):
        log.warning(
            "run.json in %s has status=%r; skipping (not a complete dump)",
            run_dir, status,
        )
        return stats

    _insert_dump_run(conn, snapshot_at, run_dir)

    statements_dir = run_dir / "statements"
    for acct in manifest.get("statements", []):
        suffix = acct.get("suffix")
        if not suffix:
            continue
        nickname = _account_nickname(acct.get("label"), suffix)
        acct_payload = {
            "suffix": suffix,
            "label": acct.get("label"),
            "nickname": nickname,
        }
        if _upsert_account(conn, snapshot_at, suffix, acct_payload):
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

            doc_date = parse_doc_date(doc.get("date") or "")
            if doc_date is None:
                log.warning("doc %s has unparseable date %r; skipping",
                            filename, doc.get("date"))
                continue
            raw_type = doc.get("type") or "Unknown"
            doc_kind = _DOC_KIND_BY_TYPE.get(raw_type, raw_type.lower())
            # The downloader writes the format from the row's
            # `Click to Download <FORMAT>` aria-label, but Schwab
            # has been observed to render a row whose label says
            # "PDF" while the actual download is a CSV (one known
            # case: a stray 1099 Composite CSV slotted into a
            # Brokerage Statement row). Trust the file extension
            # on disk over the manifest claim, so a mis-labelled
            # row doesn't get handed to pypdfium2.
            claimed_fmt = (doc.get("format") or "").lower()
            actual_fmt = _format_from_filename(filename)
            if claimed_fmt and claimed_fmt != actual_fmt:
                log.warning(
                    "manifest format/extension mismatch for %s "
                    "(manifest=%s, on-disk=%s); using on-disk",
                    filename, claimed_fmt, actual_fmt,
                )
            fmt = actual_fmt

            doc_payload = canonical_json({
                "raw_type": raw_type,
                "raw_doc_name": doc.get("document"),
                "format": fmt,
            })

            existing = conn.execute(
                "SELECT 1 FROM documents WHERE sha256 = ?", (sha256,),
            ).fetchone()
            if existing is None:
                conn.execute(
                    "INSERT INTO documents"
                    " (sha256, snapshot_at, account_external_id, doc_date,"
                    "  doc_kind, file_format, filename, size_bytes, payload)"
                    " VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
                    (sha256, snapshot_at, suffix, doc_date, doc_kind,
                     fmt, filename, size, doc_payload),
                )
                stats["documents_new"] += 1
            else:
                stats["documents_dup"] += 1

            # This walk parses only the Statement PDFs (positions,
            # cash, transactions). The 1099-Composite tax forms and
            # 3rd-Party-Distribution letters captured here are parsed
            # in their own later passes (_load_1099b_forms,
            # _load_distribution_letters); everything else stays an
            # opaque-blob document.
            if doc_kind != "statement" or fmt != "pdf":
                continue

            pdf_path = statements_dir / suffix / filename
            if not pdf_path.is_file():
                log.warning("doc in manifest but missing on disk: %s",
                            pdf_path)
                stats["documents_missing_on_disk"] += 1
                continue

            # Three independent gates: transactions (per logical
            # document), positions (per logical statement), cash
            # (per logical statement). Each gate decides if its
            # inserter should run; the PDF is parsed once if ANY
            # gate is open.
            #
            # Transaction gate uses the logical-document key
            # (account + doc_date + filename), NOT the sha256.
            # Schwab regenerates PDFs per download (new sha256
            # each time — INTEROP.md §3), so a sha256-based gate
            # would re-insert duplicates on every re-download.
            # The activity_id is also sha256-independent now
            # (see _synthesize_activity_id), so INSERT OR IGNORE
            # prevents row-level duplication; this gate is just
            # the efficiency optimisation to skip re-parsing.
            logical_key = (suffix, doc_date, doc_kind, filename)
            logical_dup = logical_key in seen_logical_docs
            if logical_dup:
                stats["statements_logical_deduped"] += 1

            ldk = _logical_doc_key(suffix, doc_date, filename)
            tx_already = (
                logical_dup
                or conn.execute(
                    "SELECT 1 FROM transactions WHERE logical_doc_key = ?",
                    (ldk,),
                ).fetchone() is not None
            )

            # Skip positions/cash parsing if we've already done this
            # logical doc THIS run (sha256 churn). Across runs the
            # INSERT OR REPLACE in the inserters keeps things
            # idempotent, but we still gate on existing rows to
            # avoid wasted parsing.
            pos_already = (
                logical_dup
                or conn.execute(
                    "SELECT 1 FROM historical_position_snapshots"
                    " WHERE account_external_id = ? AND as_of_date = ?",
                    (suffix, doc_date),
                ).fetchone() is not None
            )
            cash_already = (
                logical_dup
                or conn.execute(
                    "SELECT 1 FROM historical_cash_balances"
                    " WHERE account_external_id = ? AND period_end = ?",
                    (suffix, doc_date),
                ).fetchone() is not None
            )
            need_pos = (not pos_already) or reparse
            need_cash = (not cash_already) or reparse

            need_tx = (not tx_already) or reparse
            if not (need_tx or need_pos or need_cash):
                continue

            # Mark this logical doc as in-flight so any later
            # churned re-download in THIS run hits logical_dup
            # and stays out of the parse list.
            seen_logical_docs.add(logical_key)

            year_hint = datetime.fromtimestamp(doc_date, tz=timezone.utc).year
            parse_jobs.append({
                "suffix": suffix,
                "sha256": sha256,
                "ldk": ldk,
                "pdf_path": str(pdf_path),
                "doc_date": doc_date,
                "year_hint": year_hint,
                "need_tx": need_tx,
                "need_pos": need_pos,
                "need_cash": need_cash,
                "tx_reparse_delete": need_tx and tx_already and reparse,
            })

    # Parse every collected statement PDF, then insert serially. PDF
    # text extraction (pypdfium2 → PDFium C++) is the expensive step per
    # job; SQLite's single-writer model makes inserts serial anyway, so
    # the gain comes from sharding the parse work across cores (see
    # _parse_statements for the pool / serial dispatch).
    if parse_jobs:
        log.info("parsing %d statement PDF(s)", len(parse_jobs))
        t0 = time.monotonic()
        parsed_results = _parse_statements(parse_jobs, pool, workers)
        elapsed = time.monotonic() - t0
        log.info(
            "parsed %d PDF(s) in %.2fs (%.3fs/PDF wall)",
            len(parse_jobs), elapsed, elapsed / max(1, len(parse_jobs)),
        )

        # Harvest the account-registration label from one
        # parsed statement per account. The label is stable
        # across statements for a given account; we take the
        # first non-null value we see. The wealthdb gold
        # adapter keys off this string (verbatim) for its
        # `tax_wrapper` enum mapping — see migration 0003
        # comment + DESIGN.md §8.
        registration_by_acct: dict[str, str] = {}
        # Full account numbers from the page-1 headers, ALL
        # values per account — _apply_account_number writes the
        # payload key only when every statement agrees (see
        # INTEROP.md §1 for the api↔web bridge it feeds).
        numbers_by_acct: dict[str, set[str]] = {}

        for job, parsed in zip(parse_jobs, parsed_results):
            if parsed.get("_error"):
                log.warning(
                    "PDF parse failed for %s: %s",
                    job["pdf_path"], parsed["_error"],
                )
                stats["pdf_parse_errors"] += 1
                continue

            reg = parsed.get("account_registration")
            if reg and job["suffix"] not in registration_by_acct:
                registration_by_acct[job["suffix"]] = reg

            num = parsed.get("account_number")
            if num:
                numbers_by_acct.setdefault(job["suffix"], set()).add(num)

            if job["tx_reparse_delete"]:
                # Delete by logical_doc_key so all sha256-churn
                # variants of the same statement are cleared at once.
                conn.execute(
                    "DELETE FROM transactions WHERE logical_doc_key = ?",
                    (job["ldk"],),
                )
                stats["transactions_reparsed"] += 1

            if job["need_tx"]:
                n = _insert_statement_transactions(
                    conn, job["suffix"],
                    parsed.get("transactions", []), job["sha256"],
                    job["ldk"],
                )
                stats["transactions_inserted"] += n

            period_end_ts = (
                parse_iso_date(parsed.get("period_end"))
                or job["doc_date"]
            )
            period_start_ts = (
                parse_iso_date(parsed.get("period_start"))
                or job["doc_date"]
            )
            # The snapshot gates above probe `doc_date` — the manifest's
            # date, the only one known BEFORE the parse — while these two
            # inserters write the PARSED period_end. The gate therefore
            # looks for a row under a date the inserter never wrote
            # whenever the two disagree, never closes, and re-parses that
            # statement on every subsequent run.
            #
            # They have never disagreed: across every statement in the
            # archive the parsed period_end equals the manifest date, which
            # is why this is a canary and not a schema change. Carrying a
            # logical_doc_key onto both snapshot tables (as migration 0004
            # did for transactions) is the real fix, and this is the line
            # that says when it has become worth doing.
            if period_end_ts != job["doc_date"]:
                log.warning(
                    "statement %s: parsed period_end differs from the "
                    "manifest date; its positions/cash gate cannot close "
                    "and it will re-parse on every run",
                    job["ldk"],
                )

            if job["need_pos"]:
                n_pos = _insert_position_snapshots(
                    conn, job["suffix"], period_end_ts,
                    parsed.get("positions") or [], job["sha256"],
                )
                stats["positions_inserted"] += n_pos

            if job["need_cash"]:
                n_cash = _insert_cash_balance(
                    conn, job["suffix"], period_end_ts, period_start_ts,
                    parsed.get("cash_summary"), job["sha256"],
                )
                stats["cash_balances_inserted"] += n_cash

        # Pin the account-registration label onto each
        # account's accounts-table row. Stable across runs —
        # the column drifts only if Schwab restyles the
        # registration line.
        for suffix, reg in registration_by_acct.items():
            conn.execute(
                "UPDATE accounts SET account_registration = ?"
                " WHERE account_external_id = ?",
                (reg, suffix),
            )
        n_updated = len(registration_by_acct)

        # Pin the full account number into each account's
        # payload (consistent-or-absent — see
        # _apply_account_number). A key set here where none was
        # stored rests only on this run's parses — bronze may
        # hold older, disagreeing statements this run never saw
        # (dump-run / logical-doc gated, or cleared by an earlier
        # conflict) — so the suffix is queued for re-verification
        # against the full bronze evidence
        # (_reconcile_account_numbers in run_load). Accounts
        # whose statements were all ingested by earlier
        # invocations reach the same pass via their absent key.
        for suffix, nums in numbers_by_acct.items():
            if (len(nums) == 1
                    and _stored_account_number(conn, suffix) is None):
                stats["account_numbers_pinned"].add(suffix)
            if _apply_account_number(conn, suffix, nums):
                stats["account_number_updated"] += 1

        # Tax-form fallback: for any account whose statements
        # didn't surface a registration (e.g. a brand-new
        # account that hasn't received a monthly statement
        # yet, or one whose statement format we don't handle
        # yet), check the documents table for the cleanest
        # tax-form signal — Schwab's 5498 / 5498-ESA filenames
        # carry the account type by definition (the forms are
        # only ever issued for IRA / ESA accounts).
        null_rows = conn.execute(
            "SELECT DISTINCT account_external_id FROM accounts "
            "WHERE account_registration IS NULL"
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

    # 3rd-Party-Distribution letters and 1099-Composite tax forms.
    # Both are already in the documents table (recorded in the
    # statement walk above) but skipped there for row parsing; these
    # two passes add their transaction rows. They iterate the same
    # manifest["statements"] documents and use their own
    # logical_doc_key gates, so they're idempotent across runs and
    # sha256 re-downloads independently of the statement parser.
    _load_distribution_letters(conn, run_dir, manifest, reparse, stats)
    _load_1099b_forms(conn, run_dir, manifest, reparse, stats)

    # Tx-history exports: per-account CSV/JSON/XML. JSON is the
    # canonical source for silver rows;
    # CSV/XML are stored as opaque documents for traceability.
    transactions_dir = run_dir / "transactions"
    for acct in manifest.get("transactions", []):
        suffix = acct.get("suffix")
        if not suffix:
            continue
        acct_dir = transactions_dir / suffix
        if not acct_dir.is_dir():
            continue
        # Optional per-account More-detail sidecar (written by
        # download.py's default per-row detail pass, unless
        # --no-more-detail). We merge its fields into matching
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
            doc_payload = canonical_json({
                "raw_type": "Transaction History Export",
                "format": fmt,
            })
            existing = conn.execute(
                "SELECT 1 FROM documents WHERE sha256 = ?", (sha256,),
            ).fetchone()
            if existing is None:
                conn.execute(
                    "INSERT INTO documents"
                    " (sha256, snapshot_at, account_external_id, doc_date,"
                    "  doc_kind, file_format, filename, size_bytes, payload)"
                    " VALUES (?, ?, ?, ?, 'tx_history_export', ?, ?, ?, ?)",
                    (sha256, snapshot_at, suffix, snapshot_at,
                     fmt, filename, size, doc_payload),
                )
                stats["documents_new"] += 1
            else:
                stats["documents_dup"] += 1

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
            # run produces a uniquely-named file. The gate avoids
            # redundant re-parsing across loaders; INSERT OR IGNORE
            # in _insert_tx_history_transactions is the row-level
            # dedup backstop.
            tx_ldk = _logical_doc_key(suffix, snapshot_at, filename)
            already_has_rows = conn.execute(
                "SELECT 1 FROM transactions WHERE logical_doc_key = ?",
                (tx_ldk,),
            ).fetchone() is not None
            if already_has_rows and not reparse:
                continue
            if already_has_rows and reparse:
                conn.execute(
                    "DELETE FROM transactions WHERE logical_doc_key = ?",
                    (tx_ldk,),
                )
                stats["transactions_reparsed"] += 1
            try:
                with json_path.open("r", encoding="utf-8") as fh:
                    payload = json.load(fh)
            except Exception as e:
                log.warning("tx-history JSON parse failed for %s: %s",
                            json_path, e)
                continue
            txs = payload.get("BrokerageTransactions") or []
            n = _insert_tx_history_transactions(
                conn, suffix, txs, sha256, more_details, tx_ldk,
            )
            stats["transactions_inserted"] += n

    return stats


def _format_from_filename(filename: str) -> str:
    ext = Path(filename).suffix.lstrip(".").lower()
    return ext or "pdf"


def _load_more_details(acct_dir: Path) -> dict:
    """Return a {row_key: detail_dict} map of "More"-modal scrape
    results for this tx-history account, or {} when the sidecar
    is absent.

    download.py's per-row detail pass (default; --no-more-detail skips)
    writes `<acct_dir>/more-details.json` as a list of
    `{"row_key": "...", "fields": {...}}` records. row_key is a
    deterministic SHA-256 prefix over the row's promoted columns
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
        fields = entry.get("fields")
        if key and isinstance(fields, dict):
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

    If `more_details[row_key]` exists for a row, its key-value
    pairs are merged into the row's `payload` JSON under a
    nested `_more` key — silver consumers can pluck e.g.
    `json_extract(payload, '$._more.Settle Date')`.
    """
    inserted = 0
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
        activity_id = _synthesize_activity_id(
            account_external_id, normalised, idx,
        )
        # Merge any matching More-modal detail into payload.
        payload_dict = dict(tx)
        row_key = _tx_history_row_key(tx)
        if row_key in more_details:
            payload_dict["_more"] = more_details[row_key]
        payload = canonical_json(payload_dict)
        cur = conn.execute(
            "INSERT OR IGNORE INTO transactions"
            " (activity_id, timestamp, account_external_id, kind,"
            "  instrument_key, source, source_sha256, logical_doc_key,"
            "  payload)"
            " VALUES (?, ?, ?, ?, ?, 'tx_history_json', ?, ?, ?)",
            (activity_id, timestamp, account_external_id,
             tx.get("Action") or "Unknown",
             tx.get("Symbol") or None,
             source_sha256, logical_doc_key, payload),
        )
        if cur.rowcount:
            inserted += 1
    return inserted


def _insert_statement_transactions(conn: sqlite3.Connection,
                                   account_external_id: str,
                                   transactions: list[dict],
                                   source_sha256: str,
                                   logical_doc_key: str) -> int:
    """INSERT OR IGNORE one row per parsed transaction. Returns
    the count inserted.

    `logical_doc_key` is the sha256-independent key for the
    logical document (account + doc_date + filename), stored on
    every row so the load gate and --reparse delete can operate
    on it without touching source_sha256.

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
        timestamp = parse_iso_date(tx.get("date"))
        if timestamp is None:
            log.warning("skipping transaction with no parseable date: %s",
                        tx.get("date"))
            continue
        kind = tx.get("category") or "Unknown"
        # Statement PDFs expose only the ticker in the activity
        # rows (CUSIP lives in the positions block), so
        # instrument_key carries the parser's ticker; the gold
        # layer resolves to CUSIP via the api silver's
        # instruments table.
        instrument_key = tx.get("symbol")
        payload = canonical_json(tx)
        cur = conn.execute(
            "INSERT OR IGNORE INTO transactions"
            " (activity_id, timestamp, account_external_id, kind,"
            "  instrument_key, source, source_sha256, logical_doc_key,"
            "  payload)"
            " VALUES (?, ?, ?, ?, ?, 'statement_pdf', ?, ?, ?)",
            (activity_id, timestamp, account_external_id, kind,
             instrument_key, source_sha256, logical_doc_key, payload),
        )
        if cur.rowcount:
            inserted += 1
    return inserted


def _insert_parsed_transactions(conn: sqlite3.Connection,
                                account_external_id: str,
                                rows: list[dict],
                                source: str,
                                source_sha256: str,
                                logical_doc_key: str) -> int:
    """INSERT OR IGNORE one row per normalised parser row, for the
    newer feeds (form_1099b, third_party_distribution).

    Mirrors `_insert_statement_transactions` — same synthetic
    activity_id contract (`date`/`amount`/`description`/`symbol` +
    ordinal index), same logical_doc_key bookkeeping — but takes the
    `source` and the silver `kind` / `instrument_key` straight from
    the row dict (the parser already mapped them) and stores the whole
    row as the payload. Rows with no parseable `date` are dropped with
    a warning rather than failing the load."""
    inserted = 0
    for idx, row in enumerate(rows):
        timestamp = parse_iso_date(row.get("date"))
        if timestamp is None:
            log.warning("skipping %s row with no parseable date: %r",
                        source, row.get("date"))
            continue
        activity_id = _synthesize_activity_id(account_external_id, row, idx)
        kind = row.get("kind") or "Unknown"
        instrument_key = row.get("instrument_key")
        payload = canonical_json(row)
        cur = conn.execute(
            "INSERT OR IGNORE INTO transactions"
            " (activity_id, timestamp, account_external_id, kind,"
            "  instrument_key, source, source_sha256, logical_doc_key,"
            "  payload)"
            " VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)",
            (activity_id, timestamp, account_external_id, kind,
             instrument_key, source, source_sha256, logical_doc_key, payload),
        )
        if cur.rowcount:
            inserted += 1
    return inserted


def _gate_logical_doc(conn: sqlite3.Connection, ldk: str,
                      reparse: bool) -> tuple[bool, bool]:
    """Shared transactions load gate for the newer feeds. Returns
    `(should_process, rows_exist)` and deletes nothing.

    The delete-on-reparse is intentionally NOT done here: the caller
    must parse first and only delete on a *successful* parse (via
    `_reparse_delete`), so a parse failure can never drop existing rows
    without a replacement. This mirrors the statement-PDF path, which
    checks the parse result before its window delete."""
    already = conn.execute(
        "SELECT 1 FROM transactions WHERE logical_doc_key = ?", (ldk,),
    ).fetchone() is not None
    if already and not reparse:
        return False, True
    return True, already


def _reparse_delete(conn: sqlite3.Connection, ldk: str, rows_exist: bool,
                    reparse: bool, stats: dict) -> None:
    """Clear all rows for a logical doc immediately before re-insert,
    once its parse has succeeded. No-op unless `--reparse` and rows
    already exist (all sha256-churn variants share one logical_doc_key,
    so this clears them together)."""
    if rows_exist and reparse:
        conn.execute(
            "DELETE FROM transactions WHERE logical_doc_key = ?", (ldk,),
        )
        stats["transactions_reparsed"] += 1


def _load_distribution_letters(conn: sqlite3.Connection, run_dir: Path,
                               manifest: dict, reparse: bool,
                               stats: dict) -> None:
    """Parse 3rd-Party-Distribution letters (doc_kind='letter', the
    "3rd-Party-Distribution_*.PDF" filename) into transfer rows with
    source='third_party_distribution'.

    These letters are already recorded in the documents table by the
    statement walk; this pass only adds the transaction rows. Each
    letter is one logical document (no format twins), so the
    logical_doc_key uses the filename directly."""
    statements_dir = run_dir / "statements"
    for acct in manifest.get("statements", []):
        suffix = acct.get("suffix")
        if not suffix:
            continue
        for doc in acct.get("documents", []):
            filename = doc.get("filename") or ""
            raw_type = doc.get("type") or ""
            doc_kind = _DOC_KIND_BY_TYPE.get(raw_type, raw_type.lower())
            if doc_kind != "letter":
                continue
            if not filename.startswith("3rd-Party-Distribution"):
                continue
            if _format_from_filename(filename) != "pdf":
                continue
            doc_date = parse_doc_date(doc.get("date") or "")
            if doc_date is None:
                log.warning("distribution %s has unparseable date %r; skipping",
                            filename, doc.get("date"))
                continue
            path = statements_dir / suffix / filename
            if not path.is_file():
                log.warning("distribution in manifest but missing on disk: %s",
                            path)
                stats["documents_missing_on_disk"] += 1
                continue
            ldk = _logical_doc_key(suffix, doc_date, filename)
            proceed, rows_exist = _gate_logical_doc(conn, ldk, reparse)
            if not proceed:
                continue
            try:
                rows = pp.parse_distribution_pdf(path)
            except Exception as e:
                log.warning("distribution parse failed for %s: %s", path, e)
                stats["distribution_parse_errors"] += 1
                continue
            # Delete only after a successful parse, so a failure can't
            # drop the prior rows with no replacement.
            _reparse_delete(conn, ldk, rows_exist, reparse, stats)
            n = _insert_parsed_transactions(
                conn, suffix, rows, "third_party_distribution",
                doc.get("sha256") or "", ldk,
            )
            stats["transactions_inserted"] += n
            stats["distribution_transactions_inserted"] += n


def _load_1099b_forms(conn: sqlite3.Connection, run_dir: Path,
                      manifest: dict, reparse: bool, stats: dict) -> None:
    """Parse 1099 Composite tax forms (doc_kind='tax_form', document
    name containing "1099 Composite") into sale rows with
    source='form_1099b'.

    Schwab ships each logical form in up to three formats (XML, CSV,
    PDF) that share a base filename. We prefer the XML, fall back to
    the CSV, and key the logical_doc_key on the **base filename
    (without extension)** so the XML and CSV twins dedup to one set of
    rows. A form available only as PDF is logged as a coverage gap
    (no machine-readable lot detail to parse)."""
    statements_dir = run_dir / "statements"
    for acct in manifest.get("statements", []):
        suffix = acct.get("suffix")
        if not suffix:
            continue
        # Group this account's 1099-Composite docs by base filename
        # (sans extension); within each group keep one doc per format.
        groups: dict[str, dict[str, dict]] = {}
        for doc in acct.get("documents", []):
            raw_type = doc.get("type") or ""
            if _DOC_KIND_BY_TYPE.get(raw_type, raw_type.lower()) != "tax_form":
                continue
            if "1099 Composite" not in (doc.get("document") or ""):
                continue
            filename = doc.get("filename") or ""
            fmt = _format_from_filename(filename)
            if fmt not in ("xml", "csv", "pdf"):
                continue
            base = filename[: -(len(fmt) + 1)] if "." in filename else filename
            groups.setdefault(base, {}).setdefault(fmt, doc)

        for base, by_fmt in groups.items():
            if "xml" in by_fmt:
                fmt, doc = "xml", by_fmt["xml"]
            elif "csv" in by_fmt:
                fmt, doc = "csv", by_fmt["csv"]
            else:
                # PDF-only form — no machine-readable lots to parse.
                log.info("1099 Composite %s available only as PDF; "
                         "no lot detail parsed", base)
                stats["form_1099b_pdf_only"] += 1
                continue

            filename = doc.get("filename") or ""
            doc_date = parse_doc_date(doc.get("date") or "")
            if doc_date is None:
                log.warning("1099 %s has unparseable date %r; skipping",
                            filename, doc.get("date"))
                continue
            path = statements_dir / suffix / filename
            if not path.is_file():
                log.warning("1099 in manifest but missing on disk: %s", path)
                stats["documents_missing_on_disk"] += 1
                continue
            # Format-independent key: the XML and CSV twins share `base`,
            # so whichever we parse first wins and the other is skipped.
            ldk = _logical_doc_key(suffix, doc_date, base)
            proceed, rows_exist = _gate_logical_doc(conn, ldk, reparse)
            if not proceed:
                continue
            try:
                parsed = tf.parse_1099b(path, fmt)
            except Exception as e:
                log.warning("1099-B parse failed for %s: %s", path, e)
                stats["form_1099b_parse_errors"] += 1
                continue
            # Delete only after a successful parse, so a failure can't
            # drop the prior rows with no replacement.
            _reparse_delete(conn, ldk, rows_exist, reparse, stats)
            n = _insert_parsed_transactions(
                conn, suffix, parsed.get("lots", []), "form_1099b",
                doc.get("sha256") or "", ldk,
            )
            stats["transactions_inserted"] += n
            stats["form_1099b_transactions_inserted"] += n


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

    - accounts still lacking the key — rows ingested before the
      account-number parser existed, statements skipped by the
      dump-run / logical-doc gates on every later load, or headers
      that conflicted;
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


def _insert_position_snapshots(conn: sqlite3.Connection,
                                account_external_id: str,
                                as_of_date: int,
                                positions: list[dict],
                                source_sha256: str) -> int:
    """INSERT OR REPLACE one row per parsed position. Returns the
    count of rows written. PK is
    (as_of_date, account_external_id, instrument_key) — a
    re-parse of the same logical statement (different sha256,
    same content) collapses onto the same row.

    Rows missing an instrument_key are dropped (parser failure
    indicator); a position row with no symbol can't be joined
    against anything downstream.
    """
    written = 0
    for pos in positions:
        instrument_key = pos.get("instrument_key")
        if not instrument_key:
            log.warning(
                "skipping position with no instrument_key in payload "
                "(account=%s, as_of=%s)",
                account_external_id, as_of_date,
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
        conn.execute(
            "INSERT OR REPLACE INTO historical_position_snapshots"
            " (as_of_date, account_external_id, instrument_key,"
            "  quantity, market_price, market_value, cost_basis,"
            "  unrealized_gain_loss, accrued_interest,"
            "  source_sha256, payload)"
            " VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
            (
                as_of_date, account_external_id, instrument_key,
                pos.get("quantity"), pos.get("market_price"),
                pos.get("market_value"), pos.get("cost_basis"),
                pos.get("unrealized_gain_loss"),
                pos.get("accrued_interest"),
                source_sha256, payload,
            ),
        )
        written += 1
    return written


def _insert_cash_balance(conn: sqlite3.Connection,
                          account_external_id: str,
                          period_end: int,
                          period_start: int,
                          cash: dict | None,
                          source_sha256: str) -> int:
    """INSERT OR REPLACE one row per (period_end, account,
    currency). Returns 0 (no cash_summary parsed) or 1.

    NULLs are preserved: missing opening / closing / debits /
    credits stay NULL in the row rather than being coerced to
    0.0 — the consumer needs to distinguish "not reported" from
    "actually zero".
    """
    if not cash:
        return 0
    currency = cash.get("currency_iso") or "USD"
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
    conn.execute(
        "INSERT OR REPLACE INTO historical_cash_balances"
        " (period_end, period_start, account_external_id, currency_iso,"
        "  opening_balance, closing_balance, total_debits, total_credits,"
        "  source_sha256, payload)"
        " VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
        (
            period_end, period_start, account_external_id, currency,
            cash.get("opening_balance"), cash.get("closing_balance"),
            cash.get("total_debits"), cash.get("total_credits"),
            source_sha256, payload,
        ),
    )
    return 1


# ============================================================
# Top-level
# ============================================================

def _document_generation() -> str:
    """Fingerprint of every parser that materialises rows into silver: the
    statement / distribution parsers and the tax-form parser, in one value.

    One generation rather than one per pass. They are edited together as
    often as not, a false purge costs a re-parse of an archive the run is
    walking anyway, and a missed one costs a duplicated row.
    """
    return srcfp.parser_fingerprint([pp, tf], ("pypdfium2",))


def _purge_stale_snapshots(conn: sqlite3.Connection) -> int:
    """Drop the statement-derived snapshot tables, which have no delete path
    of their own, so the re-walk refills them rather than adding to them.

    Only ever called once a stale generation has forced `reparse` on, which
    is what guarantees the walk that refills them actually happens.
    """
    dropped = sum(conn.execute(f"DELETE FROM {t}").rowcount
                  for t in _SNAPSHOT_TABLES)
    log.info("the document parsers have changed since these rows were "
             "written; dropped %d snapshot row(s) for re-derivation", dropped)
    return dropped


def run_load(args: argparse.Namespace) -> int:
    migrations_dir = _resolve_migrations_dir(args.migrations_dir)
    args.silver_db.parent.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(str(args.silver_db))
    silver.own_only(args.silver_db)
    try:
        conn.execute("PRAGMA foreign_keys = ON")
        silver.apply_migrations(conn, migrations_dir)

        # A moved parser owes the archive a re-read, and `--reparse` is the
        # lever that already delivers one: it opens the run gate, the
        # per-document gates and the per-document deletes together.
        generation = _document_generation()
        reparse = args.reparse or silver.stale_generation(
            conn, DOCUMENT_GENERATION_SCOPE, generation)
        if reparse and not args.reparse:
            log.info("the document parsers have changed since silver was "
                     "written; re-parsing the archive")
            _purge_stale_snapshots(conn)
            conn.commit()

        runs = discover_bronze_runs(args.bronze_dir)
        log.info("found %d bronze run(s) under %s", len(runs), args.bronze_dir)

        # One parse pool and one parse-dedup set for the whole invocation.
        # A per-run pool paid a spawn/teardown cycle for every bronze run;
        # a per-run dedup set re-parsed every zero-transaction statement in
        # each later run. Hoisting both here parses each logical statement
        # once across a cumulative bronze archive. The pool is skipped when
        # nothing needs loading or when serial parsing is forced.
        seen_logical_docs: set[tuple] = set()
        # Suffixes whose account_number_full was pinned by this
        # invocation's in-run harvest, accumulated only from runs
        # that committed (a rolled-back run's stats are discarded
        # with it). Fed to the reconcile pass below.
        pinned_accounts: set[str] = set()
        pending = [
            r for r in runs
            if reparse or not already_loaded(conn, parse_snapshot_at(r.name))
        ]
        pool_ctx: contextlib.AbstractContextManager = (
            _ParsePoolManager(_resolve_worker_count(args.workers))
            if pending and args.workers != 1
            else contextlib.nullcontext(None)
        )
        with pool_ctx as pool:
            for run_dir in runs:
                snapshot_at = parse_snapshot_at(run_dir.name)
                if already_loaded(conn, snapshot_at) and not reparse:
                    log.info("skipping %s (already loaded)", run_dir.name)
                    continue
                log.info("loading %s (snapshot_at=%d)", run_dir.name, snapshot_at)
                # load_run marks its docs seen during the walk, before the
                # commit below. Snapshot the set so a rolled-back run's
                # marks are undone — otherwise a doc first seen in a run
                # that later fails would be skipped by every later run,
                # silently dropping its rows.
                seen_before = set(seen_logical_docs)
                try:
                    stats = load_run(
                        conn, run_dir, reparse=reparse,
                        workers=args.workers, pool=pool,
                        seen_logical_docs=seen_logical_docs,
                    )
                    conn.commit()
                    pinned_accounts |= stats["account_numbers_pinned"]
                    log.info(
                        "loaded %s: accts +%d/-%d, docs +%d/-%d, "
                        "tx +%d (reparsed %d), positions +%d, cash +%d, "
                        "logical-dup %d, registrations %d, "
                        "acct-numbers %d, pdf errors %d; "
                        "1099-B +%d (pdf-only %d, errors %d), "
                        "distributions +%d (errors %d)",
                        run_dir.name,
                        stats["accounts_inserted"], stats["accounts_deduped"],
                        stats["documents_new"], stats["documents_dup"],
                        stats["transactions_inserted"],
                        stats["transactions_reparsed"],
                        stats["positions_inserted"],
                        stats["cash_balances_inserted"],
                        stats["statements_logical_deduped"],
                        stats["account_registration_updated"],
                        stats["account_number_updated"],
                        stats["pdf_parse_errors"],
                        stats["form_1099b_transactions_inserted"],
                        stats["form_1099b_pdf_only"],
                        stats["form_1099b_parse_errors"],
                        stats["distribution_transactions_inserted"],
                        stats["distribution_parse_errors"],
                    )
                except Exception:
                    conn.rollback()
                    seen_logical_docs = seen_before  # undo this run's marks
                    log.exception("load failed for %s; rolled back", run_dir.name)

        # Settle account_number_full against the full bronze
        # evidence: accounts loaded before the account-number
        # parser existed never passed through the in-run harvest
        # (their runs are dump_runs-skipped above), and a key the
        # harvest pinned this invocation rests only on the
        # statements parsed this time. Header-only re-read from
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
            log.exception("account-number reconcile failed; rolled back")

        silver.stamp_generation(conn, DOCUMENT_GENERATION_SCOPE, generation)
        conn.commit()

        _log_registration_histogram(conn)
        _log_account_number_coverage(conn)
    finally:
        conn.close()
    return 0


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
        log.info("  …%s  %s", acct, reg)
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
        silver.reset(args.silver_db)
    return run_load(args)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
