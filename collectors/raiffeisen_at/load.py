#!/usr/bin/env python3
"""raiffeisen_at silver loader: bronze REST captures → source-shaped SQLite.

Parses each bronze run dir (see download.py for the layout) into the silver
schema (migrations/): the account roster (enriched with the account-info
detail), the transaction ledger, the daily closing-balance series, and the
statement inventory. Idempotent — an already-loaded dump is skipped, and
transactions INSERT OR IGNORE on their stable id, so re-running converges.

The `kontoumsaetze` history JSON (`history/<iban>.json`) is the authoritative
ledger: every row carries a stable `id`, a signed `betrag.amount`, and both a
booking (`buchungstag`) and value (`valuta`) date. Unlike the US siblings the
history has **no per-row running balance**; the closing-balance time series
comes from the `kontostaende` daily series (`balances/<iban>.json`) into the
`daily_balances` table. Statements are ingested as documents (PDFs belong in
bronze), not as a transaction source — the archive reaches only ~5 months
past the history floor, so there is no older tail to reconstruct (DESIGN.md
§G).

Browserless: imports only collectorkit + stdlib, so the `load` runtime never
pulls in login.py's Playwright dependency.

Bronze run-dir layout consumed:

    <run>/
      run.json                      status manifest
      accounts.json                 [{iban, type, balance:{amount,currency}, …}, …]
      details/<iban>.json           account-information detail (detailgruppen)
      history/<iban>.json           {iban, minBuchungstag, transactions:[…]}
      balances/<iban>.json          {tagessalden:[{tag, saldo}], kontostand, …}
      statements/<iban>/<date>_<sys>_<id>[_v<n>].pdf
"""
from __future__ import annotations

import argparse
import hashlib
import json
import logging
import re
import sqlite3
import sys
from datetime import datetime, timezone
from pathlib import Path

from collectorkit import bronze, cli, silver

log = logging.getLogger("raiffeisen_at.load")

MIGRATIONS_DIR = Path(__file__).resolve().parent / "migrations"

# Re-export so tests + siblings can call load.apply_migrations directly.
apply_migrations = silver.apply_migrations

# transactions.source tag. The kontoumsaetze JSON ledger is the single source
# of record, so there is exactly one value.
SOURCE_HISTORY = "history"


# ============================================================
# Date / money parsing
# ============================================================

def _epoch_day(d) -> int:
    """Unix seconds UTC at midnight of the given datetime or date."""
    return int(datetime(d.year, d.month, d.day, tzinfo=timezone.utc).timestamp())


def parse_date(raw) -> int | None:
    """An ISO date (`YYYY-MM-DD`, optionally with a time) → epoch-midnight
    seconds. Mein ELBA renders `buchungstag` / `valuta` / `tag` as ISO dates.
    None on anything unparseable."""
    if raw is None:
        return None
    m = re.match(r"(\d{4})-(\d{2})-(\d{2})", str(raw).strip())
    if not m:
        return None
    try:
        return _epoch_day(datetime(int(m[1]), int(m[2]), int(m[3])))
    except ValueError:
        return None


def parse_money(raw) -> float | None:
    """Coerce a money value to float. The REST amounts arrive as native
    numbers (`betrag.amount`, `saldo`); a numeric string is tolerated. None on
    blank/unparseable. (German-formatted display strings never reach a numeric
    column — those live only in the detail payload.)"""
    if raw is None:
        return None
    if isinstance(raw, (int, float)):
        return float(raw)
    s = str(raw).strip()
    if not s:
        return None
    try:
        return float(s)
    except ValueError:
        return None


# ============================================================
# History JSON → ledger rows
# ============================================================

def _first(d: dict, *keys, default=None):
    """First present, non-None value among `keys` — tolerance for minor wire
    naming drift."""
    for k in keys:
        if k in d and d[k] is not None:
            return d[k]
    return default


def _amount_and_currency(row: dict) -> tuple[float | None, str | None]:
    """The signed amount + currency from a history row's `betrag`
    (`{amount, currency}`). `amount` is already signed at the source (negative
    = debit)."""
    betrag = row.get("betrag")
    if not isinstance(betrag, dict):
        return None, None
    return parse_money(betrag.get("amount")), (
        betrag.get("currency") or betrag.get("currencyCode"))


def _hash_txn_id(iban: str, posted_at: int, amount: float,
                 description: str | None) -> str:
    """Stable synthetic id for a row with no `id` (never observed, but
    tolerated so a malformed row is not silently dropped). Deterministic, so
    re-loads converge on it."""
    basis = "|".join([iban, str(posted_at), f"{amount:.2f}",
                      (description or "").strip()])
    return "syn_" + hashlib.sha256(basis.encode("utf-8")).hexdigest()[:24]


def history_rows(iban: str, transactions: list) -> list[dict]:
    """Project the `kontoumsaetze` `transactions` list into silver ledger
    rows. A row missing a parseable booking date or amount is skipped;
    everything else is preserved in `payload`."""
    out = []
    for row in transactions:
        if not isinstance(row, dict):
            continue
        posted_at = parse_date(row.get("buchungstag"))
        amount, currency = _amount_and_currency(row)
        if posted_at is None or amount is None:
            continue
        description = _first(row, "verwendungszweckZeile1", "verwendungszweck")
        description = str(description).strip() if description else None
        txn_id = row.get("id")
        txn_id = str(txn_id).strip() if txn_id is not None else ""
        if not txn_id:
            txn_id = _hash_txn_id(iban, posted_at, amount, description)
        counterparty = _first(row, "transaktionsteilnehmerZeile1")
        out.append({
            "txn_id": txn_id,
            "posted_at": posted_at,
            "value_at": parse_date(row.get("valuta")),
            "amount": amount,
            "currency": currency,
            "kind": "DEBIT" if amount < 0 else "CREDIT",
            "category": _first(row, "kategorieCode", "autoKategorieCode"),
            "description": description,
            "counterparty": str(counterparty).strip() if counterparty else None,
            "source": SOURCE_HISTORY,
            "payload": row,
        })
    return out


# ============================================================
# Account-information detail → curated attributes
# ============================================================

# Deposit-relevant detail rows, keyed by their German `bezeichnung`. The card
# block (Karteninformationen) and the account-holder name (Kontobezeichnung)
# are deliberately NOT promoted — deposit-only scope, and the name is PII that
# needn't leave the detail out of an account attribute.
_DETAIL_FIELDS = {
    "Kontoart": "kontoart",
    "Währung": "currency",
    "Kontoführendes Institut": "institution",
    "BIC": "bic",
    "Aktueller Zinssatz Haben": "interest_credit",
    "Aktueller Zinssatz Soll": "interest_debit",
}


def parse_details(details: dict) -> dict:
    """Curate the account-information `details` payload (DESIGN.md §C) into the
    handful of deposit-relevant attributes silver keeps: account type,
    currency, holding institution + BIC, and the credit / debit interest-rate
    strings. Tolerant of a missing/misshaped payload (returns {})."""
    if not isinstance(details, dict):
        return {}
    out = {}
    for gruppe in details.get("detailgruppen") or []:
        if not isinstance(gruppe, dict):
            continue
        for row in gruppe.get("details") or []:
            if not isinstance(row, dict):
                continue
            key = _DETAIL_FIELDS.get(str(row.get("bezeichnung", "")))
            if not key:
                continue
            inhalt = row.get("inhalt")
            if isinstance(inhalt, list):
                inhalt = " ".join(str(x) for x in inhalt if x)
            value = str(inhalt).strip() if inhalt else None
            if value:
                out[key] = value
    return out


# ============================================================
# Balances → daily series rows
# ============================================================

def balance_rows(balances: dict) -> list[dict]:
    """Project a `kontostaende` payload's `tagessalden` (`[{tag, saldo}]`) into
    `{balance_date, balance}` rows. A row with an unparseable date or saldo is
    skipped."""
    out = []
    if not isinstance(balances, dict):
        return out
    for entry in balances.get("tagessalden") or []:
        if not isinstance(entry, dict):
            continue
        day = parse_date(entry.get("tag"))
        saldo = parse_money(entry.get("saldo"))
        if day is None or saldo is None:
            continue
        out.append({"balance_date": day, "balance": saldo})
    return out


# ============================================================
# Roster projection
# ============================================================

def _mask(iban: str) -> str | None:
    """A human-facing last-4 mask ("…1234") from the IBAN. Never more."""
    digits = re.sub(r"\D", "", str(iban or ""))
    if len(digits) >= 4:
        return "…" + digits[-4:]
    return None


def account_row(acct: dict, details: dict | None) -> dict | None:
    """Merge a roster account (`accounts.json`) with its curated detail
    attributes into the silver account projection. Returns None if the row
    carries no IBAN."""
    iban = str(acct.get("iban") or "").strip()
    if not iban:
        return None
    curated = parse_details(details or {})
    bal = acct.get("balance") if isinstance(acct.get("balance"), dict) else {}
    currency = curated.get("currency") or bal.get("currency")
    return {
        "account_external_id": iban,
        "account_type": curated.get("kontoart") or acct.get("type") or None,
        "nickname": None,
        "mask": _mask(iban),
        "currency": currency,
        "balance": parse_money(bal.get("amount")),
        # Content-dedup payload: the roster row folded with the curated detail.
        "payload": {**acct, "details": curated},
    }


# ============================================================
# DB inserts
# ============================================================

def _insert_account(conn, snapshot_at: int, row: dict) -> None:
    """Upsert one roster account, content-deduped: a new snapshot row lands
    only when the canonical payload changed since the last one."""
    ext = row["account_external_id"]
    payload = silver.canonical_json(row["payload"])
    prev = conn.execute(
        "SELECT payload FROM accounts WHERE account_external_id=? "
        "ORDER BY snapshot_at DESC LIMIT 1", (ext,)).fetchone()
    if prev is not None and prev[0] == payload:
        return  # content-dedup: unchanged since the last snapshot
    conn.execute(
        "INSERT OR REPLACE INTO accounts (snapshot_at, account_external_id, "
        "account_type, nickname, mask, currency, balance, payload) "
        "VALUES (?,?,?,?,?,?,?,?)",
        (snapshot_at, ext, row["account_type"], row["nickname"], row["mask"],
         row["currency"], row["balance"], payload))


def _insert_transaction(conn, iban: str, tx: dict) -> None:
    conn.execute(
        "INSERT OR IGNORE INTO transactions (txn_id, account_external_id, "
        "posted_at, value_at, amount, currency, kind, category, description, "
        "counterparty, source, payload) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
        (tx["txn_id"], iban, tx["posted_at"], tx["value_at"], tx["amount"],
         tx["currency"], tx["kind"], tx["category"], tx["description"],
         tx["counterparty"], tx["source"], silver.canonical_json(tx["payload"])))


def _insert_balance(conn, snapshot_at: int, iban: str, row: dict) -> None:
    conn.execute(
        "INSERT OR REPLACE INTO daily_balances (account_external_id, "
        "balance_date, balance, snapshot_at) VALUES (?,?,?,?)",
        (iban, row["balance_date"], row["balance"], snapshot_at))


def _insert_document(conn, snapshot_at: int, iban: str, pdf: Path) -> None:
    sha, size = bronze.sha256_file(pdf)
    conn.execute(
        "INSERT OR IGNORE INTO documents (sha256, snapshot_at, "
        "account_external_id, doc_date, doc_kind, file_format, filename, "
        "size_bytes, payload) VALUES (?,?,?,?,?,?,?,?,?)",
        (sha, snapshot_at, iban, _statement_date_from_name(pdf.name),
         "statement", "pdf", pdf.name, size,
         silver.canonical_json({"source_name": pdf.name})))


# The statement filename is `<YYYY-MM-DD>_<system>_<id>[_v<n>].pdf`
# (download.py stems the document's creation date + its stable key).
_DATE_YMD_RE = re.compile(r"(20\d{2})-(\d{2})-(\d{2})")


def _statement_date_from_name(name: str) -> int | None:
    """Best-effort statement date from a filename like
    `2026-08-01_EAZ_....pdf`. None when no date is embedded."""
    m = _DATE_YMD_RE.search(name)
    if not m:
        return None
    try:
        return _epoch_day(datetime(int(m[1]), int(m[2]), int(m[3])))
    except ValueError:
        return None


# ============================================================
# Per-run loader
# ============================================================

def _read_json(path: Path):
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        log.warning("%s: missing or not valid JSON; skipping", path.name)
        return None


def _read_accounts(run_dir: Path) -> list[dict]:
    data = _read_json(run_dir / "accounts.json")
    if isinstance(data, dict):
        data = data.get("accounts", [])
    return [a for a in data if isinstance(a, dict)] if isinstance(data, list) else []


def load_run(conn: sqlite3.Connection, run_dir: Path) -> bool:
    """Load one bronze run dir. Returns True if ingested, False if skipped
    (non-complete dump, or already loaded). Idempotent."""
    status = bronze.run_status(run_dir / "run.json")
    if status not in ("complete", None):
        # in-progress / dry-run shells are not silver inputs.
        log.info("skip %s (status=%s)", run_dir.name, status)
        return False
    try:
        snapshot_at = bronze.parse_run_ts(run_dir.name)
    except ValueError:
        log.warning("skip %s (unparseable run slug)", run_dir.name)
        return False
    if snapshot_at in silver.loaded_snapshots(conn):
        return False

    accounts = _read_accounts(run_dir)
    for acct in accounts:
        iban = str(acct.get("iban") or "").strip()
        if not iban:
            continue
        details = _read_json(run_dir / "details" / f"{iban}.json")
        row = account_row(acct, details)
        if row is not None:
            _insert_account(conn, snapshot_at, row)

    hist_dir = run_dir / "history"
    if hist_dir.is_dir():
        for hist in sorted(hist_dir.glob("*.json")):
            data = _read_json(hist)
            if not isinstance(data, dict):
                continue
            iban = str(data.get("iban") or hist.stem).strip()
            for tx in history_rows(iban, data.get("transactions") or []):
                _insert_transaction(conn, iban, tx)

    bal_dir = run_dir / "balances"
    if bal_dir.is_dir():
        for balf in sorted(bal_dir.glob("*.json")):
            data = _read_json(balf)
            for row in balance_rows(data or {}):
                _insert_balance(conn, snapshot_at, balf.stem, row)

    stmt_dir = run_dir / "statements"
    if stmt_dir.is_dir():
        for acct_dir in sorted(p for p in stmt_dir.iterdir() if p.is_dir()):
            for pdf in sorted(acct_dir.glob("*.pdf")):
                _insert_document(conn, snapshot_at, acct_dir.name, pdf)

    conn.execute(
        "INSERT OR REPLACE INTO dump_runs (snapshot_at, silver_schema_version,"
        " run_dir) VALUES (?,?,?)",
        (snapshot_at, silver.current_schema_version(conn), str(run_dir)))
    conn.commit()
    log.info("loaded %s (%d account(s))", run_dir.name, len(accounts))
    return True


def main(argv: list[str]) -> int:
    p = argparse.ArgumentParser(description=__doc__.strip().splitlines()[0])
    p.add_argument("--bronze-dir", type=Path, required=True,
                   help="Bronze tree root (holds the <UTC-ts>/ run dirs).")
    p.add_argument("--silver-db", type=Path, default=None,
                   help="Silver SQLite path. Default: <bronze-dir>/raiffeisen_at.db.")
    cli.add_standard_args(p, verb="load")
    args = p.parse_args(argv)
    cli.configure_logging(args.verbose)

    db_path = args.silver_db or (args.bronze_dir / "raiffeisen_at.db")
    if args.force:
        silver.reset(db_path)
    conn = silver.open_db(db_path)
    try:
        silver.apply_migrations(conn, MIGRATIONS_DIR)
        loaded = 0
        for run_dir in bronze.iter_run_dirs(args.bronze_dir):
            if load_run(conn, run_dir):
                loaded += 1
        log.info("done: %d run(s) ingested into %s", loaded, db_path)
    finally:
        conn.close()
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
