#!/usr/bin/env python3
"""firstcitizens silver loader: bronze REST captures → source-shaped SQLite.

Parses each bronze run dir (see download.py for the layout) into the silver
schema (migrations/): the account roster, the transaction ledger, and the
statement inventory. Idempotent — an already-loaded dump is skipped, and
transactions INSERT OR IGNORE on their id — and a movement that comes back
under a new id is recognised by the bank's host transaction number — so
re-running converges.

Unlike chase, there is **no export join and no statement-PDF backfill**. The
`accountHistory` JSON (`history/<acct>.json`) is the authoritative ledger:
every row carries a stable `transactionId` AND a `runningBalance`, so the
ledger — signed amount, per-row balance, id — comes from one payload
(DESIGN.md §4.3). The CSV/QFX exports are captured for provenance only; a flat
export can carry slightly fewer rows than the JSON (a pending/edge item it
omits), so it is never the source of record. Statements are ingested as
documents (PDFs belong in bronze) but are not a transaction source: the
history reaches the account's full post-SVB-migration lifetime, deeper than
the ~2 years of statements, so there is no older tail to reconstruct.

Bronze run-dir layout consumed:

    <run>/
      run.json                      status manifest
      accounts.json                 [{id, account_external_id, product_type_name,
                                      nickname, balances:[{description,value}], …}, …]
      history/<acct>.json           {accountId, transactionCount,
                                      oldestTransactionDate, transactions:[…]}
      statements/<acct>/<period>.pdf
      transactions/<acct>.{csv,qfx} (provenance only — not read here)
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

log = logging.getLogger("firstcitizens.load")

MIGRATIONS_DIR = Path(__file__).resolve().parent / "migrations"

# Re-export so tests + siblings can call load.apply_migrations directly.
apply_migrations = silver.apply_migrations

# transactions.source tag. The JSON `accountHistory` ledger is the single
# source of record (no export join), so there is exactly one value.
SOURCE_HISTORY = "history"


# ============================================================
# Date / money parsing
# ============================================================

def _epoch_day(d) -> int:
    """Unix seconds UTC at midnight of the given datetime or date."""
    return int(datetime(d.year, d.month, d.day, tzinfo=timezone.utc).timestamp())


def parse_posted_date(raw) -> int | None:
    """A history row's `postedDate` → epoch-midnight seconds. First Citizens
    posts at day granularity; the Q2 API renders the date as `M/D/YYYY` (or
    zero-padded `MM/DD/YYYY`). ISO `YYYY-MM-DD` and an epoch-milliseconds /
    epoch-seconds integer are also accepted so a wire-format variant degrades
    gracefully rather than dropping the row. None on anything unparseable."""
    if raw is None:
        return None
    # Epoch integer (seconds or milliseconds), possibly as a numeric string.
    if isinstance(raw, (int, float)) or (isinstance(raw, str) and raw.strip().isdigit()):
        n = int(raw)
        if n > 10_000_000_000:          # too large for seconds → milliseconds
            n //= 1000
        try:
            return _epoch_day(datetime.fromtimestamp(n, tz=timezone.utc))
        except (ValueError, OSError, OverflowError):
            return None
    s = str(raw).strip()
    if not s:
        return None
    # An ISO datetime may carry a time / offset; only the date is retained.
    m = re.match(r"(\d{4})-(\d{2})-(\d{2})", s)
    if m:
        try:
            return _epoch_day(datetime(int(m[1]), int(m[2]), int(m[3])))
        except ValueError:
            return None
    for fmt in ("%m/%d/%Y", "%m/%d/%y"):
        try:
            return _epoch_day(datetime.strptime(s.split(" ")[0], fmt))
        except ValueError:
            continue
    return None


def parse_money(raw) -> float | None:
    """Parse a signed money value. Accepts a native number or a string with
    thousands separators, a leading currency symbol, and parenthesised
    negatives; returns None on blank/unparseable."""
    if raw is None:
        return None
    if isinstance(raw, (int, float)):
        return float(raw)
    s = str(raw).strip()
    if not s:
        return None
    neg = s.startswith("(") and s.endswith(")")
    s = s.strip("()").replace(",", "").replace("$", "").strip()
    if not s:
        return None
    try:
        val = float(s)
    except ValueError:
        return None
    return -val if neg else val


# ============================================================
# History JSON → ledger rows
# ============================================================

def _first(d: dict, *keys, default=None):
    """First present, non-None value among `keys` — tolerance for minor wire
    naming drift (the capture the field names were pinned from is not kept)."""
    for k in keys:
        if k in d and d[k] is not None:
            return d[k]
    return default


def _signed_amount(row: dict) -> float | None:
    """Signed transaction amount, canonical (positive = balance increase /
    credit, negative = debit). The magnitude comes from `amount`; the sign
    from the explicit `isDebit` flag when present (robust whether the raw
    `amount` is signed or unsigned), else from the raw sign as given."""
    amt = parse_money(_first(row, "amount", "transactionAmount"))
    if amt is None:
        return None
    is_debit = _first(row, "isDebit", "debit")
    if isinstance(is_debit, str):
        is_debit = is_debit.strip().lower() in ("true", "1", "y", "yes", "d")
    if is_debit is None:
        return amt                       # trust the raw sign
    return -abs(amt) if is_debit else abs(amt)


def _hash_fitid(account_external_id: str, posted_at: int, amount: float,
                description: str | None, check_number: str | None) -> str:
    """Stable synthetic id for a row with no `transactionId` (never observed,
    but tolerated so a malformed row is not silently dropped). Deterministic,
    so re-loads converge on it."""
    basis = "|".join([account_external_id, str(posted_at), f"{amount:.2f}",
                      (description or "").strip(), (check_number or "").strip()])
    return "syn_" + hashlib.sha256(basis.encode("utf-8")).hexdigest()[:24]


# `transactionType` of an item the bank has memo-posted but not yet posted to
# history. It comes back under a new `transactionId` as a `History` row once
# it posts, so kept, it would stay in silver beside its posted copy for good.
MEMO_POSTED = "memo"


def history_rows(account_external_id: str, transactions: list) -> list[dict]:
    """Project the `accountHistory` `transactions` list into silver ledger
    rows. A row missing a parseable posted date or amount is skipped (mirrors
    chase's malformed-row handling), and so is a memo-posted item, which the
    posted history books again once it clears; everything else is preserved in
    `payload`."""
    out = []
    for row in transactions:
        if not isinstance(row, dict):
            continue
        if str(row.get("transactionType") or "").strip().lower() == MEMO_POSTED:
            continue
        posted_at = parse_posted_date(_first(row, "postedDate", "date",
                                             "effectiveDate"))
        amount = _signed_amount(row)
        if posted_at is None or amount is None:
            continue
        description = _first(row, "description", "memo", "name")
        check_raw = _first(row, "checkNumber", "checkNum")
        check_number = str(check_raw).strip() or None if check_raw is not None else None
        balance = parse_money(_first(row, "runningBalance", "balance"))
        fitid = _first(row, "transactionId", "id")
        fitid = str(fitid).strip() if fitid is not None else ""
        if not fitid:
            fitid = _hash_fitid(account_external_id, posted_at, amount,
                                description, check_number)
        out.append({
            "fitid": fitid,
            "posted_at": posted_at,
            "amount": amount,
            # A coarse label kept consistent with the (canonical) sign; the
            # richer type the source may carry survives in `payload`.
            "kind": "DEBIT" if amount < 0 else "CREDIT",
            "description": str(description).strip() if description else None,
            "check_number": check_number,
            "balance": balance,
            "source": SOURCE_HISTORY,
            "payload": row,
        })
    return out


# ============================================================
# DB inserts
# ============================================================

def _pick_balance(balances: list) -> float | None:
    """The account's headline balance from the roster's labelled balances,
    preferring the Current, then Available, then the first parseable value."""
    if not isinstance(balances, list):
        return None
    def val(entry):
        return parse_money(entry.get("value")) if isinstance(entry, dict) else None
    for want in ("current", "available"):
        for entry in balances:
            if isinstance(entry, dict) and want in str(entry.get("description", "")).lower():
                v = val(entry)
                if v is not None:
                    return v
    for entry in balances:
        v = val(entry)
        if v is not None:
            return v
    return None


def _mask(acct: dict) -> str | None:
    """A human-facing last-4 mask ("…1234") from the roster's masked account
    number, if one was captured. Never the full number."""
    raw = str(acct.get("account_external_id") or "")
    digits = re.sub(r"\D", "", raw)
    if len(digits) >= 4:
        return "…" + digits[-4:]
    return None


def _insert_account(conn, snapshot_at: int, acct: dict) -> None:
    """Upsert one roster account, content-deduped: a new snapshot row lands
    only when the canonical payload changed since the last one. The silver
    key is the opaque Q2 account id (`id`), the same key the history and
    statement endpoints use."""
    ext = str(acct.get("id") or "").strip()
    if not ext:
        return
    payload = silver.canonical_json(acct)
    prev = conn.execute(
        "SELECT payload FROM accounts WHERE account_external_id=? "
        "ORDER BY snapshot_at DESC LIMIT 1", (ext,)).fetchone()
    if prev is not None and prev[0] == payload:
        return  # content-dedup: unchanged since the last snapshot
    conn.execute(
        "INSERT OR REPLACE INTO accounts (snapshot_at, account_external_id, "
        "account_type, nickname, mask, currency, balance, payload) "
        "VALUES (?,?,?,?,?,?,?,?)",
        (snapshot_at, ext, acct.get("product_type_name") or None,
         acct.get("nickname") or None, _mask(acct),
         acct.get("currency") or "USD", _pick_balance(acct.get("balances")),
         payload))


def _held_under_another_id(conn, account_external_id: str, tx: dict) -> bool:
    """Whether silver already holds this movement under a different
    `transactionId`.

    The id is not as stable as it looks: a movement the account has not
    yet closed a statement cycle on can come back under a fresh
    `transactionId` on every run, which INSERT OR IGNORE on the id then
    stores once per run. The core banking system's own `hostTranNumber`
    stays put, so a row carrying one is the same movement as a stored row
    with the same number, posting date and amount, and the id first seen
    keeps it. A row without the number is keyed on its id alone."""
    host = str(tx["payload"].get("hostTranNumber") or "").strip()
    if not host:
        return False
    return conn.execute(
        "SELECT 1 FROM transactions WHERE account_external_id = ?"
        " AND posted_at = ? AND amount = ? AND fitid <> ?"
        " AND json_extract(payload, '$.hostTranNumber') = ? LIMIT 1",
        (account_external_id, tx["posted_at"], tx["amount"], tx["fitid"],
         host)).fetchone() is not None


def _insert_transaction(conn, account_external_id: str, tx: dict) -> None:
    if _held_under_another_id(conn, account_external_id, tx):
        return
    conn.execute(
        "INSERT OR IGNORE INTO transactions (fitid, posted_at, "
        "account_external_id, amount, kind, description, check_number, "
        "balance, source, payload) VALUES (?,?,?,?,?,?,?,?,?,?)",
        (tx["fitid"], tx["posted_at"], account_external_id, tx["amount"],
         tx["kind"], tx["description"], tx["check_number"], tx["balance"],
         tx["source"], silver.canonical_json(tx["payload"])))


def _insert_document(conn, snapshot_at: int, account_external_id: str,
                     pdf: Path) -> None:
    sha, size = bronze.sha256_file(pdf)
    doc_date = _statement_date_from_name(pdf.name)
    conn.execute(
        "INSERT OR IGNORE INTO documents (sha256, snapshot_at, "
        "account_external_id, doc_date, doc_kind, file_format, filename, "
        "size_bytes, payload) VALUES (?,?,?,?,?,?,?,?,?)",
        (sha, snapshot_at, account_external_id, doc_date, "statement", "pdf",
         pdf.name, size, silver.canonical_json({"source_name": pdf.name})))


# The statement filename is the safe-stemmed cycle date, `MM-DD-YYYY.pdf`
# (download.py stems the `MM/DD/YYYY` period). ISO `YYYY-MM-DD` is also matched
# so a rename can't silently lose the date.
_DATE_MDY_RE = re.compile(r"(\d{2})-(\d{2})-(20\d{2})")
_DATE_YMD_RE = re.compile(r"(20\d{2})-(\d{2})-(\d{2})")


def _statement_date_from_name(name: str) -> int | None:
    """Best-effort statement date from a filename like `07-31-2026.pdf`. None
    when no date is embedded."""
    m = _DATE_MDY_RE.search(name)
    if m:
        mm, dd, yyyy = m[1], m[2], m[3]
    else:
        m = _DATE_YMD_RE.search(name)
        if not m:
            return None
        yyyy, mm, dd = m[1], m[2], m[3]
    try:
        return _epoch_day(datetime(int(yyyy), int(mm), int(dd)))
    except ValueError:
        return None


# ============================================================
# Per-run loader
# ============================================================

_SAFE_STEM_RE = re.compile(r"[^\w.-]+")


def _safe_stem(value: str) -> str:
    """The filesystem-safe stem download.py derives an account id / statement
    dir name from (kept in sync with download.safe_stem). Re-implemented here
    rather than imported so the browserless `load` runtime never pulls in
    login.py's Playwright dependency."""
    stem = _SAFE_STEM_RE.sub("-", str(value)).strip("-.")
    return stem or "item"


def _read_accounts(run_dir: Path) -> list[dict]:
    path = run_dir / "accounts.json"
    if not path.is_file():
        return []
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except ValueError:
        log.warning("%s: accounts.json is not valid JSON; skipping", run_dir.name)
        return []
    if isinstance(data, dict):
        data = data.get("accounts", [])
    return [a for a in data if isinstance(a, dict)]


def _read_history(path: Path) -> tuple[str | None, list]:
    """(accountId, transactions) from a `history/<acct>.json` payload. The
    `accountId` is the raw (un-stemmed) Q2 id, so it matches the roster key."""
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except ValueError:
        log.warning("%s: not valid JSON; skipping", path.name)
        return None, []
    if not isinstance(data, dict):
        return None, []
    acct_id = data.get("accountId")
    txs = data.get("transactions")
    return (str(acct_id) if acct_id is not None else None,
            txs if isinstance(txs, list) else [])


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
        _insert_account(conn, snapshot_at, acct)
    # Map a statement dir's safe-stemmed name back to the raw account id, so a
    # document links to the same account_external_id the roster + history use.
    stem_to_id = {_safe_stem(a.get("id", "")): str(a.get("id", ""))
                  for a in accounts if a.get("id")}

    hist_dir = run_dir / "history"
    if hist_dir.is_dir():
        for hist in sorted(hist_dir.glob("*.json")):
            acct_id, txs = _read_history(hist)
            if not acct_id:
                acct_id = stem_to_id.get(hist.stem, hist.stem)
            for tx in history_rows(acct_id, txs):
                _insert_transaction(conn, acct_id, tx)

    stmt_dir = run_dir / "statements"
    if stmt_dir.is_dir():
        for acct_dir in sorted(p for p in stmt_dir.iterdir() if p.is_dir()):
            acct_id = stem_to_id.get(acct_dir.name, acct_dir.name)
            for pdf in sorted(acct_dir.glob("*.pdf")):
                _insert_document(conn, snapshot_at, acct_id, pdf)

    conn.execute(
        "INSERT OR REPLACE INTO dump_runs (snapshot_at, silver_schema_version,"
        " run_dir) VALUES (?,?,?)",
        (snapshot_at, silver.current_schema_version(conn), str(run_dir)))
    conn.commit()
    log.info("loaded %s (%d accounts)", run_dir.name, len(accounts))
    return True


def main(argv: list[str]) -> int:
    p = argparse.ArgumentParser(description=__doc__.strip().splitlines()[0])
    p.add_argument("--bronze-dir", type=Path, required=True,
                   help="Bronze tree root (holds the <UTC-ts>/ run dirs).")
    p.add_argument("--silver-db", type=Path, default=None,
                   help="Silver SQLite path. Default: <bronze-dir>/firstcitizens.db.")
    cli.add_standard_args(p, verb="load")
    args = p.parse_args(argv)
    cli.configure_logging(args.verbose)

    db_path = args.silver_db or (args.bronze_dir / "firstcitizens.db")
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
