#!/usr/bin/env python3
"""
Load the plaid bronze runs into one silver database per Item.

Each Item tree <bronze-dir>/<item>/ gets its own database,
<bronze-dir>/<item>/<item>.db, so each Item can be a gold source of its
own. A run is loaded once, oldest first, and only when its run.json says
it is complete. A tree whose runs belong to more than one Item is
refused.

Every snapshot a run stores (accounts and their balances, holdings,
liabilities) carries the run's start as its instant. The times Plaid
last updated the Item are kept beside it.

A product is loaded as far as the run read it:

  fetched   its snapshot is stored. A ledger replaces, per account, every
            row dated within the run's window, so a row Plaid no longer
            lists leaves silver too. A pending charge that posts under a
            new id is such a row.
  partial   the bank and card ledger while Plaid still assembles its
            history: its rows are added or updated, and only stale
            pending rows are removed.
  others    nothing; the rows of earlier runs stay as they are.

A run older than the newest one loaded, say one restored from a backup,
cannot be replayed on top of newer windows. The Item's database is then
rebuilt from all of its runs, as `--force` does: into a fresh file, put
in place only once every run is in.

Usage:
    load.py --bronze-dir DIR [--item NAME ...] [--silver-db PATH] [--force]
"""

from __future__ import annotations

import argparse
import json
import logging
import os
import sqlite3
import sys
import time
from datetime import datetime, timezone
from decimal import Decimal
from pathlib import Path

from collectorkit import bronze, cli, silver

import items
import trees
from plaidapi import instant

log = logging.getLogger("plaid.load")

MIGRATIONS = Path(__file__).resolve().parent / "migrations"


LIABILITY_KINDS = ("credit", "mortgage", "student")


class RunBroken(Exception):
    """A complete run that cannot be loaded as it stands."""


# ---- reading Plaid's values ---------------------------------------------------------

def decimal_text(value) -> str | None:
    """A JSON number as a decimal string, never in exponent form. A JSON
    number is read as a float. Its repr is the shortest decimal that reads
    back as that float. That is Plaid's own figure, up to 15 significant
    digits, without trailing zeros."""
    if value is None or isinstance(value, bool):
        return None
    if isinstance(value, int):
        return str(value)
    d = Decimal(repr(value)) if isinstance(value, float) else Decimal(value)
    return "0" if d == 0 else format(d, "f")


def negated_text(value) -> str | None:
    text = decimal_text(value)
    return None if text is None else decimal_text(-Decimal(text))


def day(value) -> int | None:
    """A date Plaid states as YYYY-MM-DD, at 00:00 UTC."""
    if not value:
        return None
    return int(datetime.strptime(value[:10], "%Y-%m-%d")
               .replace(tzinfo=timezone.utc).timestamp())


def currency(obj: dict) -> str | None:
    return obj.get("iso_currency_code") or obj.get("unofficial_currency_code")


def payload(obj) -> str:
    return silver.canonical_json(obj)


def read_json(path: Path):
    return json.loads(path.read_text(encoding="utf-8"))


# ---- the Item -----------------------------------------------------------------------

def load_item_state(conn, doc: dict, manifest: dict, run_at: int) -> None:
    item = doc.get("item") or {}
    status = doc.get("status") or {}

    def updated(product):
        return instant((status.get(product) or {}).get(
            "last_successful_update"))

    conn.execute(
        "INSERT INTO item_states (run_at, item_id, environment, "
        "institution_id, institution_name, products, consent_expires_at, "
        "transactions_updated_at, investments_updated_at, payload) "
        "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
        (run_at, item.get("item_id") or manifest.get("item_id"),
         manifest.get("environment"), item.get("institution_id"),
         item.get("institution_name"),
         payload(sorted(item.get("products") or [])),
         instant(item.get("consent_expiration_time")),
         updated("transactions"), updated("investments"), payload(doc)))


# ---- one product at a time -----------------------------------------------------------

def load_accounts(conn, doc: dict, snapshot_at: int) -> None:
    """Every account of the Item at the run's start. A run restates them
    all, so an account that no longer appears has left the Item."""
    for account in doc.get("accounts") or []:
        balances = account.get("balances") or {}
        conn.execute(
            "INSERT INTO accounts (snapshot_at, account_id, name, "
            "official_name, mask, type, subtype, currency, balance_current, "
            "balance_available, balance_limit, balance_updated_at, payload) "
            "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
            (snapshot_at, account["account_id"],
             account.get("name"), account.get("official_name"),
             account.get("mask"), account.get("type"), account.get("subtype"),
             currency(balances), decimal_text(balances.get("current")),
             decimal_text(balances.get("available")),
             decimal_text(balances.get("limit")),
             instant(balances.get("last_updated_datetime")),
             payload(account)))


def upsert_securities(conn, securities: list, seen_at: int) -> None:
    """One row per security. A later run's description wins; the seen range
    widens."""
    for sec in securities or []:
        conn.execute(
            "INSERT INTO securities (security_id, name, ticker_symbol, type, "
            "subtype, currency, cusip, isin, figi, cfi_code, "
            "market_identifier_code, is_cash_equivalent, "
            "institution_security_id, proxy_security_id, first_seen_at, "
            "last_seen_at, payload) "
            "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) "
            "ON CONFLICT(security_id) DO UPDATE SET "
            "name = excluded.name, ticker_symbol = excluded.ticker_symbol, "
            "type = excluded.type, subtype = excluded.subtype, "
            "currency = excluded.currency, cusip = excluded.cusip, "
            "isin = excluded.isin, figi = excluded.figi, "
            "cfi_code = excluded.cfi_code, "
            "market_identifier_code = excluded.market_identifier_code, "
            "is_cash_equivalent = excluded.is_cash_equivalent, "
            "institution_security_id = excluded.institution_security_id, "
            "proxy_security_id = excluded.proxy_security_id, "
            "first_seen_at = MIN(first_seen_at, excluded.first_seen_at), "
            "last_seen_at = MAX(last_seen_at, excluded.last_seen_at), "
            "payload = excluded.payload",
            (sec["security_id"], sec.get("name"), sec.get("ticker_symbol"),
             sec.get("type"), sec.get("subtype"), currency(sec),
             sec.get("cusip"), sec.get("isin"), sec.get("figi"),
             sec.get("cfi_code"), sec.get("market_identifier_code"),
             None if sec.get("is_cash_equivalent") is None
             else int(bool(sec["is_cash_equivalent"])),
             sec.get("institution_security_id"),
             sec.get("proxy_security_id"), seen_at, seen_at, payload(sec)))


def load_holdings(conn, doc: dict, snapshot_at: int) -> None:
    """The holdings of every investment account at the run's start."""
    upsert_securities(conn, doc.get("securities"), snapshot_at)
    seq: dict = {}
    for h in doc.get("holdings") or []:
        key = (h["account_id"], h["security_id"])
        seq[key] = seq.get(key, -1) + 1
        conn.execute(
            "INSERT INTO holdings (snapshot_at, account_id, security_id, seq, "
            "quantity, institution_price, institution_price_as_of, "
            "institution_value, cost_basis, currency, vested_quantity, "
            "vested_value, tax_lots, payload) "
            "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
            (snapshot_at, h["account_id"], h["security_id"], seq[key],
             decimal_text(h.get("quantity")),
             decimal_text(h.get("institution_price")),
             day(h.get("institution_price_as_of")),
             decimal_text(h.get("institution_value")),
             decimal_text(h.get("cost_basis")), currency(h),
             decimal_text(h.get("vested_quantity")),
             decimal_text(h.get("vested_value")),
             payload(h.get("tax_lots") or []), payload(h)))


def _ledger_rows(conn, table: str, id_column: str, pages: list,
                 window: tuple[int, int] | None) -> list[dict]:
    """The rows of a ledger read, each once. With a window, first clear it
    for every account the answer covers. An account the Item no longer
    lists keeps its rows: its history is still true, and absence from one
    answer proves nothing.

    A pending row is provisional, and Plaid lists it under a new id once
    it posts. Every read of the bank and card ledger therefore replaces
    the covered accounts' pending rows wholesale, wherever they are dated,
    so a stale pending row never stands beside its posted twin. A partial
    read does so too: the newest rows are the ones Plaid holds first."""
    rows: dict = {}
    covered = {a["account_id"] for page in pages
               for a in page.get("accounts") or []}
    for page in pages:
        for row in page.get(table) or []:
            covered.add(row["account_id"])
            rows[row[id_column]] = row
    pending = table == "transactions"
    for account_id in sorted(covered):
        if window is not None:
            start, end = window
            conn.execute(f"DELETE FROM {table} WHERE account_id = ? AND "
                         f"(posted_at BETWEEN ? AND ?"
                         f"{' OR pending = 1' if pending else ''})",
                         (account_id, start, end))
        elif pending:
            conn.execute("DELETE FROM transactions WHERE account_id = ? AND "
                         "pending = 1", (account_id,))
    return list(rows.values())


def load_transactions(conn, rows: list, run_at: int) -> None:
    for t in rows:
        pfc = t.get("personal_finance_category") or {}
        conn.execute(
            "INSERT OR REPLACE INTO transactions (transaction_id, account_id, "
            "posted_at, authorized_date, amount, currency, name, merchant_name, "
            "original_description, pending, pending_transaction_id, "
            "category_primary, category_detailed, category_confidence, "
            "category_version, payment_channel, transaction_code, "
            "check_number, merchant_category_code, run_at, payload) "
            "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, "
            "?, ?)",
            (t["transaction_id"], t["account_id"], day(t.get("date")),
             day(t.get("authorized_date")), negated_text(t.get("amount")),
             currency(t), t.get("name"), t.get("merchant_name"),
             t.get("original_description"), int(bool(t.get("pending"))),
             t.get("pending_transaction_id"), pfc.get("primary"),
             pfc.get("detailed"), pfc.get("confidence_level"),
             pfc.get("version"), t.get("payment_channel"),
             t.get("transaction_code"), t.get("check_number"),
             t.get("merchant_category_code"), run_at, payload(t)))


def load_investment_transactions(conn, rows: list, run_at: int) -> None:
    for t in rows:
        conn.execute(
            "INSERT OR REPLACE INTO investment_transactions "
            "(investment_transaction_id, account_id, security_id, posted_at, "
            "transaction_at, name, type, subtype, amount, quantity, price, "
            "fees, currency, cancel_transaction_id, run_at, payload) "
            "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
            (t["investment_transaction_id"], t["account_id"],
             t.get("security_id"), day(t.get("date")),
             instant(t.get("transaction_datetime")), t.get("name"),
             t.get("type"), t.get("subtype"), negated_text(t.get("amount")),
             decimal_text(t.get("quantity")), decimal_text(t.get("price")),
             decimal_text(t.get("fees")), currency(t),
             t.get("cancel_transaction_id"), run_at, payload(t)))


def _interest_rate(kind: str, row: dict):
    if kind == "credit":
        return next((a.get("apr_percentage") for a in row.get("aprs") or []
                     if a.get("apr_type") == "purchase_apr"), None)
    if kind == "mortgage":
        return (row.get("interest_rate") or {}).get("percentage")
    return row.get("interest_rate_percentage")


def load_liabilities(conn, doc: dict, snapshot_at: int) -> None:
    """Card, mortgage and student-loan terms at the run's start."""
    terms = doc.get("liabilities") or {}
    for kind in LIABILITY_KINDS:
        for row in terms.get(kind) or []:
            account_id = row.get("account_id")
            if not account_id:
                log.warning("a %s liability names no account; skipped", kind)
                continue
            conn.execute(
                "INSERT INTO liabilities (snapshot_at, account_id, kind, "
                "interest_rate, last_statement_balance, "
                "last_statement_issue_date, next_payment_amount, "
                "next_payment_due_date, origination_principal_amount, "
                "origination_date, payload) "
                "VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
                (snapshot_at, account_id, kind,
                 decimal_text(_interest_rate(kind, row)),
                 decimal_text(row.get("last_statement_balance")),
                 day(row.get("last_statement_issue_date")),
                 decimal_text(row.get("next_monthly_payment")
                              if kind == "mortgage"
                              else row.get("minimum_payment_amount")),
                 day(row.get("next_payment_due_date")),
                 decimal_text(row.get("origination_principal_amount")),
                 day(row.get("origination_date")), payload(row)))


# ---- one run -------------------------------------------------------------------------

def load_run(conn, run_dir: Path, version: int) -> None:
    """Load one complete run, inside the caller's transaction. Raises
    RunBroken, before anything is written, when a file the run lists is
    missing."""
    run_at = bronze.parse_run_ts(run_dir.name)
    manifest = read_json(run_dir / trees.RUN_FILE)
    products = manifest.get("products") or {}
    missing = trees.missing_files(run_dir, products)
    if missing:
        raise RunBroken(
            f"{run_dir} is complete and lacks {', '.join(missing)}, so it "
            f"and the runs after it are not loaded. Moving it out of the "
            f"tree lets them load.")

    for product, entry in sorted(products.items()):
        conn.execute(
            "INSERT INTO run_products (run_at, product, status, rows, "
            "window_start, window_end, history, error_code) "
            "VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
            (run_at, product, entry.get("status"), entry.get("rows"),
             day(entry.get("since")), day(entry.get("until")),
             entry.get("history"), entry.get("error_code")))
    load_item_state(conn, read_json(run_dir / trees.ITEM_FILE), manifest,
                    run_at)

    def files(product, statuses=(trees.FETCHED,)):
        entry = products.get(product) or {}
        if entry.get("status") not in statuses:
            return []
        return [read_json(run_dir / name) for name in entry.get("files") or []]

    def ledger(product):
        """A ledger's pages and its rows, each row once; a full read first
        clears its window."""
        pages = files(product, (trees.FETCHED, trees.PARTIAL))
        entry = products.get(product) or {}
        window = ((day(entry["since"]), day(entry["until"]))
                  if entry.get("status") == trees.FETCHED else None)
        return pages, _ledger_rows(conn, product, trees.LEDGER_IDS[product],
                                   pages, window)

    for doc in files("accounts"):
        load_accounts(conn, doc, run_at)
    for doc in files("holdings"):
        load_holdings(conn, doc, run_at)
    for doc in files("liabilities"):
        load_liabilities(conn, doc, run_at)
    pages, rows = ledger("investment_transactions")
    for page in pages:
        upsert_securities(conn, page.get("securities"), run_at)
    load_investment_transactions(conn, rows, run_at)
    load_transactions(conn, ledger("transactions")[1], run_at)

    conn.execute(
        "INSERT INTO dump_runs (snapshot_at, silver_schema_version, run_dir, "
        "loaded_at) VALUES (?, ?, ?, ?)",
        (run_at, version, str(run_dir.resolve()), int(time.time())))


# ---- one Item ------------------------------------------------------------------------

def complete_runs(tree: Path) -> list[Path]:
    """The runs to load, in order: every complete run, and every run whose
    run.json is there and cannot be read. The second kind stops its tree
    when its turn comes (RunBroken): a damaged manifest may hide a
    complete run, and the runs after it must not load over the gap."""
    runs = []
    for run_dir in bronze.iter_run_dirs(tree):
        meta = trees.manifest(run_dir)
        status = meta.get("status") if meta else None
        if status == trees.COMPLETE or (
                meta is None and (run_dir / trees.RUN_FILE).exists()):
            runs.append(run_dir)
        else:
            log.info("%s/%s: status %s; not loaded", tree.name, run_dir.name,
                     status or "none")
    return runs


# The tables every plaid silver has; a database without them is not one.
SILVER_TABLES = frozenset({"dump_runs", "run_products", "item_states"})


def _foreign_database(db_path: Path) -> bool:
    """Whether `db_path` is a database other than a plaid silver: a file
    that holds tables, and not the ones every plaid silver has. Opened
    read-only, so even a check never writes to it."""
    if not db_path.exists():
        return False
    try:
        conn = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
        try:
            names = {n for (n,) in conn.execute(
                "SELECT name FROM sqlite_master WHERE type = 'table'")}
        finally:
            conn.close()
    except sqlite3.Error:
        return True
    return bool(names) and not SILVER_TABLES <= names


def _silver_item(conn) -> tuple[str, str] | None:
    row = conn.execute("SELECT item_id, environment FROM item_states "
                       "ORDER BY run_at DESC LIMIT 1").fetchone()
    return tuple(row) if row else None


def _discard(path: Path) -> None:
    for sidecar in ("", "-wal", "-shm", "-journal"):
        path.with_name(path.name + sidecar).unlink(missing_ok=True)


def _load_runs(conn, tree: Path, runs: list[Path], version: int) -> None:
    for run_dir in runs:
        try:
            with silver.transaction(conn):
                load_run(conn, run_dir, version)
        except (KeyError, TypeError, ValueError, AttributeError,
                ArithmeticError, OSError, sqlite3.Error) as e:
            # Bronze that is not in the shape download writes.
            raise RunBroken(f"{run_dir} cannot be loaded "
                            f"({type(e).__name__}: {e}), so it and the runs "
                            f"after it are not loaded") from e
        log.info("%s: loaded run %s", tree.name, run_dir.name)


def _rebuild(tree: Path, db_path: Path, runs: list[Path]) -> int:
    """Build the Item's silver afresh from all its runs, beside the one in
    place, and put it in place only once every run is in. A run that
    cannot be loaded leaves the silver as it was."""
    fresh = db_path.with_name(db_path.name + ".rebuild")
    _discard(fresh)
    try:
        conn = silver.open_db(fresh)
        try:
            version = silver.apply_migrations(conn, MIGRATIONS)
            _load_runs(conn, tree, runs, version)
        finally:
            conn.close()
    except BaseException:
        _discard(fresh)
        raise
    _discard(db_path)
    os.replace(fresh, db_path)
    log.info("%s: rebuilt %s. Its change number is the newest run's start, "
             "as before, so gold takes it in on `wealthdb reload <source>`.",
             tree.name, db_path)
    return len(runs)


def load_item(tree: Path, db_path: Path, *, force: bool,
              rebuild: str) -> int:
    """Bring one Item's silver up to date with its runs, and return the
    number of runs loaded. A tree with no run yet loads nothing and opens
    no database. `rebuild` is the command that rebuilds this database.

    Raises TreeError when:

    - the tree holds another Item's runs;
    - the database is another Item's, or not a plaid silver at all.

    Raises RunBroken when a run cannot be loaded. An update keeps the runs
    loaded before it. A rebuild leaves the silver as it was."""
    identity = trees.identity(tree)
    if identity is None:
        return 0
    if _foreign_database(db_path):
        raise trees.TreeError(
            f"{db_path} is not a plaid silver database, so it is left as it "
            f"is")
    runs = complete_runs(tree)
    if force:
        log.info("%s: rebuilding the Item's silver from all its runs "
                 "(--force)", tree.name)
        return _rebuild(tree, db_path, runs)
    conn = silver.open_db(db_path)
    try:
        version = silver.apply_migrations(conn, MIGRATIONS)
        held = _silver_item(conn)
        if held is not None and held != identity:
            raise trees.TreeError(
                f"{db_path} holds the silver of another Item ({held[1]}); "
                f"{rebuild} rebuilds it from {tree}")
        loaded = silver.loaded_snapshots(conn)
        pending = [r for r in runs if bronze.parse_run_ts(r.name) not in loaded]
        older = bool(pending and loaded
                     and bronze.parse_run_ts(pending[0].name) < max(loaded))
        if not older:
            _load_runs(conn, tree, pending, version)
            return len(pending)
    finally:
        conn.close()
    log.info("%s: run %s is older than the newest loaded run; rebuilding the "
             "Item's silver from all its runs", tree.name, pending[0].name)
    return _rebuild(tree, db_path, runs)


# ---- the command ---------------------------------------------------------------------

def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("--bronze-dir", type=Path, required=True,
                   help="Plaid's own data dir. Each Item has its tree of runs "
                        "in it.")
    p.add_argument("--item", metavar="NAME", action="append",
                   help="Load only this Item. Repeat for more. Default: every "
                        "Item tree under --bronze-dir.")
    p.add_argument("--silver-db", type=Path, default=None,
                   help="Where to write the silver database of the one Item "
                        "--item names. Default: <bronze-dir>/<item>/<item>.db.")
    cli.add_standard_args(p, verb="load")
    args = p.parse_args(argv)
    for name in args.item or []:
        try:
            items.check_name(name)
        except ValueError as e:
            p.error(str(e))
    if args.silver_db is not None and len(set(args.item or [])) != 1:
        p.error("--silver-db (or PLAID_SILVER_DB, through the wrapper) names "
                "one Item's database; name that Item with --item")
    return args


def main(argv: list[str] | None = None) -> int:
    args = parse_args(sys.argv[1:] if argv is None else argv)
    cli.configure_logging(args.verbose)
    trees.check_data_dir(args.bronze_dir, "loaded")
    found = trees.select(args.bronze_dir, args.item)
    if not found:
        log.info("no Item tree under %s; nothing to load", args.bronze_dir)
        return 0
    status = 0
    for tree in found:
        db_path = args.silver_db or tree / f"{tree.name}.db"
        rebuild = (f"`load --force --item {tree.name}"
                   + (f" --silver-db {args.silver_db}" if args.silver_db
                      else "") + "`")
        try:
            n = load_item(tree, db_path, force=args.force, rebuild=rebuild)
        except (trees.TreeError, RunBroken) as e:
            log.error("%s", e)
            status = 1
            continue
        except (OSError, sqlite3.Error) as e:
            log.error("%s: cannot load into %s: %s", tree.name, db_path, e)
            status = 1
            continue
        log.info("%s: %d run(s) loaded into %s", tree.name, n, db_path)
    return status


if __name__ == "__main__":
    sys.exit(main())
