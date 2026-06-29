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
            [--migrations <dir>] [--reparse] [-v]
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
from concurrent.futures import ProcessPoolExecutor
from datetime import datetime, timezone
from pathlib import Path

from collectorkit import cli, silver

# Re-export for backward compatibility with existing tests that call
# load.apply_migrations(...) / load._current_schema_version(...) directly.
apply_migrations = silver.apply_migrations
_current_schema_version = silver.current_schema_version

import pdf_parsers as pp

log = logging.getLogger("schwab-web.load")

# Bronze run-dir names look like `20260520T120000Z`. Parsed into
# Unix seconds UTC for snapshot_at.
_RUN_TS_RE = re.compile(r"^(\d{4})(\d{2})(\d{2})T(\d{2})(\d{2})(\d{2})Z$")

# Manifest doc dates come from the Schwab UI as MM/DD/YYYY. We
# convert to Unix seconds UTC at midnight.
_DOC_DATE_RE = re.compile(r"^(\d{1,2})/(\d{1,2})/(\d{4})$")

# Where to look for migrations when --migrations isn't passed.
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
        help=("Bronze tree root (the same path passed to download.py "
              "--dest). Default: %(default)s. The loader scans every "
              "<UTC-ts>/ subdir under it."),
    )
    p.add_argument(
        "--migrations", default=None, type=Path,
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
    p.add_argument(
        "-v", "--verbose", action="store_true",
        help="DEBUG-level logging.",
    )
    cli.add_force_arg(p)
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
            raise SystemExit(f"--migrations dir does not exist: {arg}")
        return arg
    for cand in DEFAULT_MIGRATIONS_DIRS:
        if cand.is_dir():
            return cand
    raise SystemExit(
        "no migrations dir found; pass --migrations or create "
        f"one of: {[str(p) for p in DEFAULT_MIGRATIONS_DIRS]}"
    )


# apply_migrations now lives in collectorkit.silver (same numeric-order
# logic + the "migration must advance schema_meta" guard).


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

    Returns True if a row was inserted, False if dedup skipped it."""
    payload = canonical_json(payload_dict)
    row = conn.execute(
        "SELECT payload FROM accounts WHERE account_external_id = ? "
        "ORDER BY snapshot_at DESC LIMIT 1",
        (account_external_id,),
    ).fetchone()
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


def _insert_dump_run(conn: sqlite3.Connection, snapshot_at: int,
                     run_dir: Path) -> None:
    conn.execute(
        "INSERT INTO dump_runs"
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
    sr-only echo, leaving just the user's chosen name.

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
             workers: int | None = None) -> dict:
    """Load one bronze-run dir. Returns a stats dict for logging."""
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
        "positions_inserted": 0,
        "cash_balances_inserted": 0,
        "statements_logical_deduped": 0,
        "account_registration_updated": 0,
    }
    # Per-run dedup set: (account_suffix, doc_date, doc_kind,
    # filename). Schwab regenerates statement PDFs on every
    # download (different sha256 each time — see INTEROP.md §3),
    # but the parser output is byte-identical for the same logical
    # document. We parse the first sha256 we see per logical doc,
    # then skip subsequent re-downloads for positions / cash /
    # transactions within THIS run. Across runs, each table uses
    # its own natural gate (see below).
    seen_logical_docs: set[tuple] = set()

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

            # Only the Statements PDFs are parsed into rows today.
            # Tax-form XML / CSV parsing is a follow-up — they're
            # captured as opaque-blob documents for now.
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

    # Parallel-parse every collected statement PDF, then insert
    # serially. PDF text extraction (pypdfium2 → PDFium C++) is
    # the expensive step per job; SQLite's single-writer model
    # makes inserts serial anyway, so the gain comes from
    # sharding the parse work across cores. Worker count
    # defaults to os.cpu_count(); pass workers=1 for serial.
    if parse_jobs:
        worker_count = workers if workers and workers > 0 else (os.cpu_count() or 1)
        worker_count = max(1, min(worker_count, len(parse_jobs)))
        log.info(
            "parallel-parsing %d statement PDF(s) across %d worker(s)",
            len(parse_jobs), worker_count,
        )
        t0 = time.monotonic()
        if worker_count == 1:
            parsed_results = [
                _parse_pdf_worker((j["pdf_path"], j["year_hint"]))
                for j in parse_jobs
            ]
        else:
            with ProcessPoolExecutor(max_workers=worker_count) as pool:
                parsed_results = list(pool.map(
                    _parse_pdf_worker,
                    [(j["pdf_path"], j["year_hint"]) for j in parse_jobs],
                ))
        log.info(
            "parsed %d PDF(s) in %.2fs (%.3fs/PDF wall)",
            len(parse_jobs), time.monotonic() - t0,
            (time.monotonic() - t0) / max(1, len(parse_jobs)),
        )

        # Harvest the account-registration label from one
        # parsed statement per account. The label is stable
        # across statements for a given account; we take the
        # first non-null value we see. The wealthdb gold
        # adapter keys off this string (verbatim) for its
        # `tax_wrapper` enum mapping — see migration 0003
        # comment + DESIGN.md §8.
        registration_by_acct: dict[str, str] = {}

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

    # Tx-history exports: per-account CSV/JSON/XML + a landing
    # HTML capture. JSON is the canonical source for silver rows;
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
        # download.py when --with-more-detail is set). We merge
        # its fields into matching transactions' payload.
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

    `download.py --with-more-detail` writes
    `<acct_dir>/more-details.json` as a list of
    `{"row_key": "...", "fields": {...}}` records. row_key is a
    deterministic SHA-256 prefix over the row's promoted columns
    (date|amount|description|symbol|action) that the loader can
    re-derive from the JSON export to merge details in. See
    download._scrape_more_details_for_page for the row_key
    derivation.
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
    matching what download._scrape_more_details_for_page derives
    from the rendered row. Used to merge More-modal detail into
    the JSON export rows."""
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
    if s is None:
        return None
    txt = str(s).strip()
    if not txt:
        return None
    neg = txt.startswith("-") or (txt.startswith("(") and txt.endswith(")"))
    txt = txt.strip("()-").lstrip("$").replace(",", "")
    try:
        v = float(txt)
    except ValueError:
        return None
    return -v if neg else v


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
        # instrument_key: per the silver convention, prefer CUSIP
        # over ticker. Statement PDFs only expose the ticker in
        # the activity-rows section; CUSIP is in the positions
        # block. For now, use whatever pdf_parsers gave us in
        # `symbol`; the gold layer can resolve to CUSIP via the
        # api silver's instruments table.
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

def run_load(args: argparse.Namespace) -> int:
    migrations_dir = _resolve_migrations_dir(args.migrations)
    args.silver_db.parent.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(str(args.silver_db))
    try:
        conn.execute("PRAGMA foreign_keys = ON")
        silver.apply_migrations(conn, migrations_dir)

        runs = discover_bronze_runs(args.bronze_dir)
        log.info("found %d bronze run(s) under %s", len(runs), args.bronze_dir)

        for run_dir in runs:
            snapshot_at = parse_snapshot_at(run_dir.name)
            if already_loaded(conn, snapshot_at) and not args.reparse:
                log.info("skipping %s (already loaded)", run_dir.name)
                continue
            log.info("loading %s (snapshot_at=%d)", run_dir.name, snapshot_at)
            try:
                stats = load_run(
                    conn, run_dir, reparse=args.reparse,
                    workers=args.workers,
                )
                conn.commit()
                log.info(
                    "loaded %s: accts +%d/-%d, docs +%d/-%d, "
                    "tx +%d (reparsed %d), positions +%d, cash +%d, "
                    "logical-dup %d, registrations %d, pdf errors %d",
                    run_dir.name,
                    stats["accounts_inserted"], stats["accounts_deduped"],
                    stats["documents_new"], stats["documents_dup"],
                    stats["transactions_inserted"],
                    stats["transactions_reparsed"],
                    stats["positions_inserted"],
                    stats["cash_balances_inserted"],
                    stats["statements_logical_deduped"],
                    stats["account_registration_updated"],
                    stats["pdf_parse_errors"],
                )
            except Exception:
                conn.rollback()
                log.exception("load failed for %s; rolled back", run_dir.name)

        _log_registration_histogram(conn)
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


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)
    if args.force:
        silver.reset(args.silver_db)
    return run_load(args)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
