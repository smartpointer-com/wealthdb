#!/usr/bin/env python3
"""erstebank_at silver loader: bronze REST captures → source-shaped SQLite.

Parses each bronze run dir (download.py has the layout) into the silver
schema (migrations/): the checking accounts and credit cards, the accounts
each run listed, the transaction ledger, the statement listings and the
statement PDF inventory. Then it rebuilds the daily closing balances from
the ledger. Idempotent: an already-loaded run is skipped, and every write
converges on its key, so re-running is a no-op.

The history JSON is the ledger (DESIGN.md §C, §D): every row carries a
stable `id`, a signed amount, booking and value dates, and the running
balance after it. The SIGN CONVENTION in migrations/0001_initial.sql is the
one place the loader changes a figure: a credit card's balance is negated
into the positive amount owed.

Browserless: imports only collectorkit, the stdlib and george_client, so
the `load` runtime never pulls in login.py's Playwright dependency.
"""
from __future__ import annotations

import argparse
import itertools
import json
import logging
import sqlite3
import sys
from collections import defaultdict
from datetime import date, datetime, timezone
from pathlib import Path

from collectorkit import bronze, cli, silver

import george_client as george

log = logging.getLogger("erstebank_at.load")

MIGRATIONS_DIR = Path(__file__).resolve().parent / "migrations"

KIND_CHECKING = "checking"
KIND_CARD = "credit_card"

# Roster fields left out of an account's payload: the holder's name and
# address blocks, and the card image.
_DROPPED_FIELDS = ("owners", "owner", "cardHolder", "imageData")


# ============================================================
# Scalars
# ============================================================

def money(raw: object) -> float | None:
    """George money `{value, precision, currency}` in major units."""
    if not isinstance(raw, dict):
        return None
    value, precision = raw.get("value"), raw.get("precision")
    if isinstance(value, bool) or not isinstance(value, int):
        return None
    if isinstance(precision, bool) or not isinstance(precision, int):
        precision = 0
    return value / 10 ** precision


def money_minor(raw: object) -> int | None:
    """George money in minor units, the exact form for balance arithmetic.
    None for a precision other than 2."""
    if (isinstance(raw, dict) and isinstance(raw.get("value"), int)
            and raw.get("precision") == 2):
        return raw["value"]
    return None


def day_epoch(day: date | None) -> int | None:
    """A calendar day as Unix seconds at its UTC midnight."""
    if day is None:
        return None
    return int(datetime(day.year, day.month, day.day,
                        tzinfo=timezone.utc).timestamp())


def _negate(value):
    return None if value is None else -value


def _mask(number: object) -> str | None:
    digits = "".join(ch for ch in str(number or "") if ch.isalnum())
    return f"…{digits[-4:]}" if len(digits) >= 4 else None


def _iban(acct: dict) -> str | None:
    accountno = acct.get("accountno")
    iban = accountno.get("iban") if isinstance(accountno, dict) else None
    return "".join(str(iban).split()).upper() if iban else None


def _payload(row: dict) -> dict:
    return {k: v for k, v in row.items() if k not in _DROPPED_FIELDS}


# ============================================================
# Projections
# ============================================================

def checking_row(acct: dict) -> dict | None:
    """The `accounts` row of a checking-roster entry, or None without an
    IBAN."""
    iban = _iban(acct)
    if not iban:
        return None
    overdraft = acct.get("overdraftData")
    return {
        "account_external_id": iban,
        "account_kind": KIND_CHECKING,
        "source_id": acct.get("id"),
        "product": acct.get("product"),
        "display_name": acct.get("productI18N") or acct.get("description"),
        "mask": _mask(iban),
        "currency": acct.get("currency")
        or (acct.get("balance") or {}).get("currency"),
        "balance": money(acct.get("balance")),
        "credit_limit": money(overdraft.get("approvedLimit"))
        if isinstance(overdraft, dict) else None,
        "payload": _payload(acct),
    }


def card_row(card: dict) -> dict | None:
    """The `accounts` row of a credit card from the card roster, or None
    for any other card. The balance is negated into the amount owed."""
    shadow = card.get("creditCardShadowAccountId")
    if card.get("type") != "CREDIT" or not shadow:
        return None
    balance = card.get("balance")
    return {
        "account_external_id": str(shadow),
        "account_kind": KIND_CARD,
        "source_id": card.get("id"),
        "product": card.get("product"),
        "display_name": card.get("productI18N") or card.get("product"),
        "mask": _mask(card.get("number")),
        "currency": (balance or {}).get("currency")
        if isinstance(balance, dict) else None,
        "balance": _negate(money(balance)),
        "credit_limit": money(card.get("limit")),
        "payload": _payload(card),
    }


def transaction_row(account: str, kind: str, row: dict) -> dict | None:
    """The `transactions` row of a history row, or None when it has no id,
    booking date or amount. A card row's running balance is negated into
    the amount owed (SIGN CONVENTION)."""
    txn_id = row.get("id")
    posted = george.epoch_ms_date(row.get("bookingDate"))
    amount = money(row.get("amount"))
    if not txn_id or posted is None or amount is None:
        return None
    balance = money(row.get("balance"))
    if kind == KIND_CARD:
        balance = _negate(balance)
    texts = row.get("additionalTexts")
    lines = texts.get("lineItems") if isinstance(texts, dict) else None
    return {
        "txn_id": str(txn_id),
        "account_external_id": account,
        "posted_at": day_epoch(posted),
        "value_at": day_epoch(george.epoch_ms_date(row.get("valuationDate"))),
        "amount": amount,
        "currency": (row.get("amount") or {}).get("currency"),
        "balance": balance,
        "balance_relevant": int("IS_BAL_RELEVANT" in (row.get("flags") or [])),
        "tx_type": row.get("txType"),
        "direction": row.get("txDirection"),
        "description": row.get("title"),
        "counterparty": row.get("partnerName") or row.get("merchantName"),
        "reference": " ".join(str(x) for x in lines if x) if lines else None,
        "payload": row,
    }


def statement_row(account: str, row: dict) -> dict | None:
    """The `statements` row of a checking statement listing entry."""
    if not row.get("id"):
        return None
    return {
        "account_external_id": account,
        "statement_id": str(row["id"]),
        "statement_kind": "statement",
        "period_start": day_epoch(george.iso_day(row.get("minBookingDate"))),
        "period_end": day_epoch(george.iso_day(row.get("maxBookingDate"))),
        "statement_date": day_epoch(george.iso_day(row.get("statementDate"))),
        "payload": row,
    }


def invoice_row(account: str, row: dict) -> dict | None:
    """The `statements` row of a card statement listing entry."""
    if not row.get("billId"):
        return None
    return {
        "account_external_id": account,
        "statement_id": str(row["billId"]),
        "statement_kind": "card_statement",
        "period_start": day_epoch(george.iso_day(row.get("from"))),
        "period_end": day_epoch(george.iso_day(row.get("to"))),
        "statement_date": day_epoch(george.iso_day(row.get("billingDate"))),
        "payload": row,
    }


# ============================================================
# Daily closing balances
# ============================================================

def daily_closes(rows: list[tuple[int, int, int]]) -> tuple[dict, int]:
    """The closing balance of each day, from `(day, amount, balance)`
    tuples in minor units, one per balance-relevant row.

    The rows of one day can come back in any order (DESIGN.md §C), so the
    close is not "the newest row's balance". It is the one balance of the
    day that no other row of that day starts from (its balance minus its
    amount). A day where that leaves no single answer takes its close from
    a neighbouring day and the day's amounts: close(d) = close(d-1) +
    Σ amounts(d). A day neither way resolves gets no close.

    Returns `(closes, breaks)`: `{day: balance}`, and how many consecutive
    days disagree with that sum — a gap in the ledger or a source anomaly,
    logged by the caller, never patched over.
    """
    by_day: dict[int, list[tuple[int, int]]] = defaultdict(list)
    for day, amount, balance in rows:
        by_day[day].append((amount, balance))
    days = sorted(by_day)
    sums = {d: sum(a for a, _ in by_day[d]) for d in days}

    closes: dict[int, int] = {}
    for d in days:
        entries = by_day[d]
        candidates = set()
        for i, (_, balance) in enumerate(entries):
            starts = [b - a for j, (a, b) in enumerate(entries) if j != i]
            if balance not in starts:
                candidates.add(balance)
        if len(candidates) == 1:
            closes[d] = candidates.pop()

    changed = True
    while changed:
        changed = False
        for i, d in enumerate(days):
            if d in closes:
                continue
            if i > 0 and days[i - 1] in closes:
                closes[d] = closes[days[i - 1]] + sums[d]
                changed = True
            elif i + 1 < len(days) and days[i + 1] in closes:
                closes[d] = closes[days[i + 1]] - sums[days[i + 1]]
                changed = True

    breaks = sum(1 for prev, d in itertools.pairwise(days)
                 if prev in closes and d in closes
                 and closes[d] != closes[prev] + sums[d])
    return closes, breaks


def rebuild_daily_balances(conn: sqlite3.Connection) -> None:
    """Re-derive `daily_balances` from the whole ledger. The running
    balance is read back in minor units from the payload, so the sums are
    exact; a card's is negated as in `transactions.balance`."""
    conn.execute("DELETE FROM daily_balances")
    kinds = dict(conn.execute(
        "SELECT account_external_id, account_kind FROM accounts"))
    per_account: dict[str, list[tuple[int, int, int]]] = defaultdict(list)
    for account, posted_at, payload in conn.execute(
            "SELECT account_external_id, posted_at, payload FROM transactions"
            " WHERE balance_relevant = 1"):
        row = json.loads(payload)
        amount = money_minor(row.get("amount"))
        balance = money_minor(row.get("balance"))
        if amount is None or balance is None:
            continue
        if kinds.get(account) == KIND_CARD:
            amount, balance = -amount, -balance
        per_account[account].append((posted_at, amount, balance))
    for account, rows in per_account.items():
        closes, breaks = daily_closes(rows)
        if breaks:
            log.warning("%s: %d day(s) where the closing balances do not "
                        "chain; the ledger has a gap there",
                        bronze.log_id(account), breaks)
        silver.upsert_rows(
            conn, "daily_balances",
            ("account_external_id", "balance_date", "balance"),
            [(account, d, c / 100) for d, c in sorted(closes.items())])


# ============================================================
# Writers
# ============================================================

_ACCOUNT_COLUMNS = ("snapshot_at", "account_external_id", "account_kind",
                    "source_id", "product", "display_name", "mask",
                    "currency", "balance", "credit_limit", "payload")
_TXN_COLUMNS = ("txn_id", "account_external_id", "posted_at", "value_at",
                "amount", "currency", "balance", "balance_relevant",
                "tx_type", "direction", "description", "counterparty",
                "reference", "snapshot_at", "payload")
_STATEMENT_COLUMNS = ("account_external_id", "statement_id",
                      "statement_kind", "period_start", "period_end",
                      "statement_date", "snapshot_at", "payload")


def _insert_account(conn, snapshot_at: int, row: dict) -> None:
    """Add a snapshot row unless the payload is unchanged since the last
    one for this account."""
    payload = silver.canonical_json(row["payload"])
    prev = conn.execute(
        "SELECT payload FROM accounts WHERE account_external_id=? "
        "ORDER BY snapshot_at DESC LIMIT 1",
        (row["account_external_id"],)).fetchone()
    if prev is not None and prev[0] == payload:
        return
    silver.upsert_rows(conn, "accounts", _ACCOUNT_COLUMNS,
                       [{**row, "snapshot_at": snapshot_at,
                         "payload": payload}])


def _with(snapshot_at: int, rows: list[dict]) -> list[dict]:
    return [{**r, "snapshot_at": snapshot_at,
             "payload": silver.canonical_json(r["payload"])} for r in rows]


def _insert_documents(conn, snapshot_at: int, account: str, folder: Path,
                      kind: str) -> None:
    for pdf in sorted(folder.glob("*.pdf")):
        sha, size = bronze.sha256_file(pdf)
        stamp, _, key = pdf.stem.partition("_")
        silver.record_document(conn, (
            sha, snapshot_at, account,
            day_epoch(george.iso_day(stamp)), kind, "pdf", pdf.name, size,
            silver.canonical_json({"source_name": pdf.name,
                                   "statement_id": key or None})))


# ============================================================
# Per-run loader
# ============================================================

def _read_json(path: Path, *, quiet: bool = False):
    """The parsed JSON at `path`, or None (with a warning unless `quiet`)
    when it is missing or not valid JSON."""
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        if not quiet:
            log.warning("%s: missing or not valid JSON; skipping", path.name)
        return None


def _list(value) -> list[dict]:
    return [v for v in value if isinstance(v, dict)] \
        if isinstance(value, list) else []


def run_products(run_dir: Path, *, quiet: bool = False) -> dict:
    """The run's products by bronze stem: `{stem: (external id, kind,
    accounts row)}`. A checking account's history and statements are
    filed under its George id, a card's under `<shadow>-<card>`; silver
    keys them by IBAN and shadow account id."""
    products = {}
    for acct in _list(_read_json(run_dir / "accounts.json", quiet=quiet)):
        row = checking_row(acct)
        if row and acct.get("id"):
            products[bronze.safe_stem(acct["id"])] = (
                row["account_external_id"], KIND_CHECKING, row)
    raw = _read_json(run_dir / "raw" / "cards.json", quiet=True)
    for card in _list(raw.get("cards") if isinstance(raw, dict) else None):
        row = card_row(card)
        if row and card.get("id"):
            stem = bronze.safe_stem(george.card_product_id(
                row["account_external_id"], card["id"]))
            products[stem] = (row["account_external_id"], KIND_CARD, row)
    return products


def _listed_ids(run_dir: Path) -> list[str]:
    return [ext for ext, _, _ in run_products(run_dir, quiet=True).values()]


def load_run(conn: sqlite3.Connection, run_dir: Path) -> bool:
    """Load one bronze run dir. Returns True if ingested, False if skipped
    (a dump that is not complete, or one already loaded). The caller owns
    the transaction (`main` makes each run one)."""
    if not bronze.run_is_loadable(run_dir):
        run_json = run_dir / "run.json"
        log.info("skip %s (%s)", run_dir.name,
                 f"status={bronze.run_status(run_json)}" if run_json.is_file()
                 else "no run.json")
        return False
    try:
        snapshot_at = bronze.parse_run_ts(run_dir.name)
    except ValueError:
        log.warning("skip %s (unparseable run slug)", run_dir.name)
        return False
    if snapshot_at in silver.loaded_snapshots(conn):
        return False

    products = run_products(run_dir)
    for _, _, row in products.values():
        _insert_account(conn, snapshot_at, row)
    bronze.record_run_listing(conn, snapshot_at,
                              [ext for ext, _, _ in products.values()])

    for stem, (account, kind, _) in sorted(products.items()):
        history = _read_json(run_dir / "history" / f"{stem}.json", quiet=True)
        if isinstance(history, dict):
            rows = [transaction_row(account, kind, r)
                    for r in _list(history.get("transactions"))]
            silver.upsert_rows(conn, "transactions", _TXN_COLUMNS,
                               _with(snapshot_at, [r for r in rows if r]))
        if kind == KIND_CHECKING:
            folder, project, doc_kind = (
                run_dir / "statements" / stem, statement_row, "statement")
        else:
            folder, project, doc_kind = (
                run_dir / "invoices" / stem, invoice_row, "card_statement")
        listing = _read_json(folder / "listing.json", quiet=True)
        rows = [project(account, r) for r in _list(listing)]
        silver.upsert_rows(conn, "statements", _STATEMENT_COLUMNS,
                           _with(snapshot_at, [r for r in rows if r]))
        if folder.is_dir():
            _insert_documents(conn, snapshot_at, account, folder, doc_kind)

    conn.execute(
        "INSERT OR REPLACE INTO dump_runs (snapshot_at, silver_schema_version,"
        " run_dir) VALUES (?,?,?)",
        (snapshot_at, silver.current_schema_version(conn), str(run_dir)))
    log.info("loaded %s (%d product(s))", run_dir.name, len(products))
    return True


def main(argv: list[str]) -> int:
    p = argparse.ArgumentParser(description=__doc__.strip().splitlines()[0])
    p.add_argument("--bronze-dir", type=Path, required=True,
                   help="Bronze tree root (holds the <UTC-ts>/ run dirs).")
    p.add_argument("--silver-db", type=Path, default=None,
                   help="Silver SQLite path. Default: "
                        "<bronze-dir>/erstebank_at.db.")
    cli.add_standard_args(p, verb="load")
    args = p.parse_args(argv)
    cli.configure_logging(args.verbose)

    db_path = args.silver_db or (args.bronze_dir / "erstebank_at.db")
    if args.force:
        silver.reset(db_path)
    conn = silver.open_db(db_path)
    try:
        silver.apply_migrations(conn, MIGRATIONS_DIR)
        with silver.transaction(conn):
            bronze.backfill_run_listings(conn, args.bronze_dir, _listed_ids)
        loaded = 0
        for run_dir in bronze.iter_run_dirs(args.bronze_dir):
            with silver.transaction(conn):          # one atomic load per run
                if load_run(conn, run_dir):
                    loaded += 1
        if loaded:
            with silver.transaction(conn):
                rebuild_daily_balances(conn)
        log.info("done: %d run(s) ingested into %s", loaded, db_path)
    finally:
        conn.close()
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
