#!/usr/bin/env python3
"""
UBS web-scrape silver loader.

Walks the bronze directory laid down by `download.py`, applies any
pending schema migrations, and loads each new dump into the silver
SQLite database defined by `migrations/`.

Load semantics
--------------
- Bronze dumps are identified by their `YYYYMMDDTHHMMSSZ` subdir
  names. Each dump is loaded atomically (one transaction). A dump that
  fails is rolled back and logged, the later dumps still load, and the
  load exits non-zero. The next load runs the failed dump again; it
  adds the rows silver lacks and overwrites no row a newer dump wrote.
- A dump is loaded when its run.json status is `complete` or `partial`
  (a `download --only` pass), or when it is a statusless legacy
  manifest; any other dump is skipped (see `skip_reason`).
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
"""

from __future__ import annotations

import argparse
import json
import logging
import os
import sqlite3
import sys
from pathlib import Path

from collectorkit import bronze, cli, parsecache, silver
from collectorkit.parsecache import ParseCache

import card_parsers  # local module
from cards_load import _load_cards
from documents_load import (
    DOCUMENT_GENERATION_SCOPE,
    DocumentParses,
    _document_generation,
    _extract_dir,
    _load_documents,
    _load_historical_from_pdfs,
    _load_supplied_documents,
)
from export_load import (
    _load_portfolio_transactions,
    _load_positions,
    _load_transactions,
)
from rates_load import _load_rates

# Re-exported: the names the tests reach through this module.
from documents_load import (  # noqa: F401
    ADVICE_DOC_TYPES,
    CONTRACT_NOTE_DOC_TYPE,
    PRIVATE_MARKET_LETTER_DOC_TYPE,
    SUPPLIED_DOC_TOKEN_PREFIX,
    SUPPLIED_STMT_OF_ASSETS_DOC_TYPE,
    _ADVICE_COLUMNS,
    _OVERVIEW_TOTAL_COLUMNS,
    _REPORT_COLUMNS,
    _REPORT_LINE_COLUMNS,
    _STATEMENT_TRADE_COLUMNS,
    _document_scope,
    _insert_hist_mortgages,
    _insert_hist_positions,
    _insert_private_market_reports,
    _namespaces,
    _parse_key,
    _parse_one_pdf,
    _purge_stale_document_rows,
)
from export_load import (  # noqa: F401
    PORTFOLIO_TXN_COLUMNS,
    POSITIONS_COLS,
    parse_grouped_decimal,
    _read_positions_base_currency,
)
from load_rows import _mortgage_rate_type  # noqa: F401
from statement_ledger import (  # noqa: F401
    _insert_advice_transactions,
    _insert_hist_transactions,
    _stmt_batch_txn_id,
    _stmt_txn_id,
)
from txn_ids import (  # noqa: F401
    names_a_portfolio,
    portfolio_account_canonical,
    portfolio_id_canonical,
)

log = logging.getLogger("ubs-web.load")

# Bank-delivered PDFs — documents the e-banking archive never listed, so
# `download` cannot reach them and they arrive by hand. They live in their
# own directory at the bronze root rather than outside the tree, which is
# what keeps silver reproducible from bronze alone: sourced from elsewhere,
# every `--force` rebuild would silently drop them. The name is not a run-dir
# slug, so `scan_bronze` does not walk it and `prune` cannot delete it.
SUPPLIED_DOCUMENTS_DIRNAME = "supplied-documents"


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
# Bronze scan
# ============================================================

def scan_bronze(bronze_dir: Path) -> list[Path]:
    """Return bronze dump subdirs in chronological order. A slug that
    names no real instant (a 30 February) is skipped with a warning."""
    if not bronze_dir.is_dir():
        raise SystemExit(f"--bronze-dir does not exist: {bronze_dir}")
    dumps = []
    for dump_dir in bronze.iter_run_dirs(bronze_dir):
        try:
            bronze.parse_run_ts(dump_dir.name)
        except ValueError:
            log.warning("skipping %s: not a valid run timestamp",
                        dump_dir.name)
            continue
        dumps.append(dump_dir)
    return dumps


# The run.json status of a `download --only PASS` dump. It covers one
# surface, so it is not "complete", but what it holds is a finished pass
# and `load` reads it.
PARTIAL_STATUS = "partial"


def legacy_manifest_is_complete(meta: dict | None) -> bool:
    """Whether a statusless (legacy) run.json marks a finished dump.

    A legacy manifest was written once, at the end of a walk, with a
    `dry_run` bool. So it marks a finished dump when it is present and
    does not record a `--dry-run` shell, which holds no exports.
    """
    return meta is not None and not meta.get("dry_run", False)


def skip_reason(dump_dir: Path) -> str | None:
    """Why `load` skips `dump_dir`, or None when it loads it.

    `load` reads a `complete` dump, a `partial` one and a statusless
    legacy one. It skips a dir with no run.json (a crashed or running
    walk), an `in-progress` or `dry-run` status, any status it does not
    know, and a legacy `--dry-run` shell. `prune` keeps exactly the
    dumps `load` reads. An unreadable run.json is loaded, so its error
    surfaces in the load.
    """
    run_json = dump_dir / "run.json"
    if not run_json.is_file():
        return "no run.json (crashed or still writing)"
    status = bronze.run_status(run_json)
    if status is None:
        meta = bronze.read_manifest(run_json)
        if meta is not None and not legacy_manifest_is_complete(meta):
            return "legacy --dry-run shell"
        return None
    if bronze.is_loadable_status(status) or status == PARTIAL_STATUS:
        return None
    return f"run.json status={status!r}"


def loadable_dumps(dumps: list[Path]) -> list[Path]:
    """The dumps of `dumps` that `load` reads (see `skip_reason`); each
    skipped one is logged."""
    out = []
    for dump_dir in dumps:
        reason = skip_reason(dump_dir)
        if reason is None:
            out.append(dump_dir)
        else:
            log.info("skipping %s: %s", dump_dir.name, reason)
    return out


def already_loaded(conn: sqlite3.Connection, dump_dir: Path) -> bool:
    return silver.snapshot_loaded(conn, bronze.parse_run_ts(dump_dir.name))


# ============================================================
# Loaders
# ============================================================

def load_dump(conn: sqlite3.Connection, dump_dir: Path,
              schema_version: int, parses: DocumentParses) -> None:
    """Load one bronze dump into silver. Single transaction.

    `parses` is shared across the whole load run so each document in the
    cumulative archive is parsed once."""
    snapshot_at = bronze.parse_run_ts(dump_dir.name)
    log.info("loading %s (snapshot_at=%d)", dump_dir.name, snapshot_at)

    run_meta = _read_run_json(dump_dir)

    _insert_dump_run(conn, snapshot_at, schema_version, dump_dir, run_meta)

    pos_count = _load_positions(conn, snapshot_at, dump_dir)
    txn_count = _load_transactions(conn, snapshot_at, dump_dir)
    ptxn_count = _load_portfolio_transactions(conn, snapshot_at, dump_dir)
    doc_count = _load_documents(conn, snapshot_at, dump_dir, run_meta)
    hist_pos, hist_cash, hist_mort, hist_txn = _load_historical_from_pdfs(
        conn, snapshot_at, dump_dir, parses)
    card_acc, card_txn, card_inv, card_stmt = _load_cards(
        conn, snapshot_at, dump_dir)
    rate_count = _load_rates(conn, snapshot_at, dump_dir)

    log.info("loaded %s: positions=%d transactions=%d "
             "portfolio_transactions=%d documents=%d "
             "hist_positions=%d hist_cash_balances=%d "
             "hist_mortgages=%d hist_transactions=%d "
             "card_accounts=%d card_transactions=%d card_invoices=%d "
             "card_statements=%d account_rates=%d",
             dump_dir.name, pos_count, txn_count, ptxn_count, doc_count,
             hist_pos, hist_cash, hist_mort, hist_txn,
             card_acc, card_txn, card_inv, card_stmt, rate_count)


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
            card_parsers.ts(window.get("since")),
            card_parsers.ts(window.get("until")),
        ),
    )


# ============================================================
# Main
# ============================================================

def _derive_documents_without_a_new_dump(
        conn: sqlite3.Connection, dumps: list[Path], parses: DocumentParses,
        *, supplied: int = 0) -> bool:
    """Derive the document-backed slice when no dump loaded this run
    walked the archive — because the PARSERS have changed, or because a
    supplied document was indexed this run. Returns False when the derive
    failed (it is rolled back and logged), else True.

    The archive walk lives inside the per-dump load, because a dump is
    what indexes its own PDFs. What the walk DERIVES, though, is the
    whole cumulative archive, and `_document_generation` exists so that
    a changed parser re-derives it. The walk sits behind the
    already-loaded skip, and a dump with no `documents/` directory (a
    `--only` pass) skips it too. Without this push a parser change would
    take effect only once a dump carrying an archive arrived, and until
    then the same bronze would produce different silver depending on
    when it was loaded.

    A supplied document reaches the table the same way and needs the same
    push: it is indexed before the dump loop, and when no loaded dump
    walks the archive nothing downstream would read it.

    Runs against the newest dump that has an archive directory. Which
    dump is immaterial to what is derived — the file paths come from the
    `documents` table, which spans every dump — and the newest is the
    right stamp for rows written now. With no such dump there is nothing
    to walk, and the parser fingerprint (which imports pdfplumber) is
    never computed.
    """
    newest = next((d for d in reversed(dumps) if (d / "documents").is_dir()), None)
    if newest is None:
        return True
    stale = silver.stale_generation(conn, DOCUMENT_GENERATION_SCOPE,
                                    _document_generation())
    if not stale and not supplied:
        return True
    log.info("%s and no loaded dump walked the archive; deriving it "
             "against %s",
             "the document parsers have changed" if stale
             else f"{supplied} supplied document(s) were indexed",
             newest.name)
    try:
        conn.execute("BEGIN")
        pos, cash, mort, txn = _load_historical_from_pdfs(
            conn, bronze.parse_run_ts(newest.name), newest, parses)
        conn.execute("COMMIT")
    except Exception:  # noqa: BLE001 — log + rollback, as the dump loop does
        conn.execute("ROLLBACK")
        log.exception("re-deriving the document archive failed")
        return False
    log.info("re-derived from documents: positions=%d cash_balances=%d "
             "mortgages=%d transactions=%d", pos, cash, mort, txn)
    return True


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

    found = scan_bronze(args.bronze_dir)
    log.info("found %d bronze dump dir(s) under %s",
             len(found), args.bronze_dir)
    dumps = loadable_dumps(found)

    # Before the dump loop, so the archive walk inside it already sees the
    # supplied rows — which is what makes a `--force` rebuild pick them up
    # with no special handling. Its own transaction: an index of files on
    # disk should survive a dump that fails to load.
    failed = 0
    try:
        conn.execute("BEGIN")
        n_supplied = _load_supplied_documents(conn, args.supplied_documents_dir)
        conn.execute("COMMIT")
    except Exception:  # noqa: BLE001 — log + rollback, as the dump loop does
        conn.execute("ROLLBACK")
        log.exception("indexing the supplied documents failed")
        n_supplied = 0
        failed += 1

    n_loaded = n_skipped = 0
    # Whether a loaded dump ran the archive walk, which only a dump with
    # a `documents/` directory does.
    walked = False
    # Shared across dumps and kept across loads: parse each archived
    # document once, replay the cached rows for every walk that re-lists it.
    parses = DocumentParses(ParseCache(parsecache.default_dir("ubs-web")),
                            os.cpu_count() or 1, _extract_dir())
    for dump_dir in dumps:
        if already_loaded(conn, dump_dir):
            n_skipped += 1
            log.debug("skipping already-loaded dump %s", dump_dir.name)
            continue
        try:
            conn.execute("BEGIN")
            load_dump(conn, dump_dir, schema_version, parses)
            conn.execute("COMMIT")
            n_loaded += 1
            walked = walked or (dump_dir / "documents").is_dir()
        except Exception:  # noqa: BLE001 — log + rollback + continue
            conn.execute("ROLLBACK")
            log.exception("load failed for %s; skipped", dump_dir.name)
            failed += 1
    if not walked and not _derive_documents_without_a_new_dump(
            conn, dumps, parses, supplied=n_supplied):
        failed += 1
    parses.close()
    conn.close()
    log.info("done: %d dumps loaded, %d already-loaded skipped",
             n_loaded, n_skipped)
    if failed:
        # The failed passes were rolled back and run again at the next
        # load, which leaves every row a newer dump wrote as it is.
        log.error("%d pass(es) failed; the next load runs them again", failed)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
