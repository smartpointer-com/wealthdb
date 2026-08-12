#!/usr/bin/env python3
"""chase silver loader: bronze exports → source-shaped SQLite.

Parses each bronze run dir (see download.py for the layout) into the silver
schema (migrations/): the account roster, the transaction ledger, and the
statement inventory. Idempotent — an already-loaded dump is skipped, and
transactions INSERT OR IGNORE on their stable id, so re-running converges.

The transaction ledger is built by **joining the two exports** captured per
account (DESIGN.md §E): the QFX (OFX) export supplies the stable `<FITID>`
key and a clean type/name/memo split, and the CSV export supplies the
per-row running balance the QFX lacks. Rows are keyed on the QFX FITID;
the CSV balance is joined on (date, amount, check number). A row seen only
in the CSV (a pending item QFX omits) is still ingested under a synthetic
id so nothing is dropped.

Bronze run-dir layout consumed:

    <run>/
      run.json                      status manifest
      accounts.json                 [{account_external_id, account_type,
                                      nickname, mask, currency, balance}, …]
      transactions/<ext_id>.qfx     OFX export (authoritative rows)
      transactions/<ext_id>.csv     CSV export (running balance)
      statements/<ext_id>/<name>.pdf
"""
from __future__ import annotations

import argparse
import csv as csvmod
import hashlib
import io
import json
import logging
import re
import sqlite3
import sys
from datetime import datetime, timezone
from pathlib import Path

from collectorkit import bronze, cli, silver

log = logging.getLogger("chase.load")

MIGRATIONS_DIR = Path(__file__).resolve().parent / "migrations"

# Re-export so tests + siblings can call load.apply_migrations directly.
apply_migrations = silver.apply_migrations


# ============================================================
# Date / money parsing
# ============================================================

def _epoch_day(d: datetime) -> int:
    """Unix seconds UTC at midnight of the given date."""
    return int(datetime(d.year, d.month, d.day, tzinfo=timezone.utc).timestamp())


def parse_ofx_date(raw: str) -> int | None:
    """OFX DTPOSTED → epoch-midnight seconds. Accepts `YYYYMMDD` optionally
    followed by time and a `[tz]` suffix; only the date is retained (Chase
    posts at day granularity)."""
    if not raw:
        return None
    m = re.match(r"\s*(\d{4})(\d{2})(\d{2})", raw)
    if not m:
        return None
    try:
        return _epoch_day(datetime(int(m[1]), int(m[2]), int(m[3])))
    except ValueError:
        return None


def parse_csv_date(raw: str) -> int | None:
    """Chase CSV `Posting Date` (MM/DD/YYYY) → epoch-midnight seconds."""
    raw = (raw or "").strip()
    for fmt in ("%m/%d/%Y", "%m/%d/%y"):
        try:
            return _epoch_day(datetime.strptime(raw, fmt))
        except ValueError:
            continue
    return None


def parse_money(raw) -> float | None:
    """Parse a signed money string. Handles thousands separators, a leading
    currency symbol, and parenthesised negatives; returns None on blank."""
    if raw is None:
        return None
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
# QFX (OFX 1.x SGML) parsing
# ============================================================

# OFX 1.x is SGML with unclosed leaf tags, so a per-<STMTTRN> regex sweep is
# the pragmatic parse (no closing-tag assumptions). Value runs to end-of-line
# or the next tag.
_STMTTRN_RE = re.compile(r"<STMTTRN>(.*?)</STMTTRN>", re.S | re.I)


def _ofx_field(block: str, tag: str) -> str | None:
    m = re.search(rf"<{tag}>([^<\r\n]*)", block, re.I)
    return m.group(1).strip() if m else None


def parse_qfx(text: str) -> list[dict]:
    """Parse the STMTTRN rows of an OFX/QFX export into dicts with keys
    fitid, posted_at, amount, kind, description, memo, check_number.
    Malformed rows (no date or amount) are skipped."""
    rows = []
    for block in _STMTTRN_RE.findall(text):
        posted_at = parse_ofx_date(_ofx_field(block, "DTPOSTED") or "")
        amount = parse_money(_ofx_field(block, "TRNAMT"))
        if posted_at is None or amount is None:
            continue
        check = _ofx_field(block, "CHECKNUM")
        rows.append({
            "fitid": _ofx_field(block, "FITID"),
            "posted_at": posted_at,
            "amount": amount,
            "kind": _ofx_field(block, "TRNTYPE"),
            "description": _ofx_field(block, "NAME"),
            "memo": _ofx_field(block, "MEMO"),
            "check_number": check or None,
        })
    return rows


# ============================================================
# CSV parsing
# ============================================================

def parse_csv(text: str) -> list[dict]:
    """Parse the Chase activity CSV into dicts with keys posted_at, amount,
    description, kind, balance, check_number. Column names follow the
    observed header (DESIGN.md §E): Details, Posting Date, Description,
    Amount, Type, Balance, Check or Slip #. Rows without a parseable date +
    amount are skipped."""
    rows = []
    reader = csvmod.DictReader(io.StringIO(text))
    for raw in reader:
        # Normalise header keys (strip whitespace/BOM) so lookups are stable.
        row = {(k or "").strip().lstrip("﻿"): (v or "") for k, v in raw.items()}
        posted_at = parse_csv_date(row.get("Posting Date", ""))
        amount = parse_money(row.get("Amount"))
        if posted_at is None or amount is None:
            continue
        check = (row.get("Check or Slip #") or "").strip()
        rows.append({
            "posted_at": posted_at,
            "amount": amount,
            "description": (row.get("Description") or "").strip() or None,
            "kind": (row.get("Type") or "").strip() or None,
            "balance": parse_money(row.get("Balance")),
            "check_number": check or None,
        })
    return rows


# ============================================================
# Join + id synthesis
# ============================================================

def _join_key(posted_at: int, amount: float, check_number: str | None) -> tuple:
    # round the amount to cents so float noise never splits a match.
    return (posted_at, round(amount, 2), (check_number or "").strip())


def synth_fitid(account_external_id: str, posted_at: int, amount: float,
                description: str | None, check_number: str | None) -> str:
    """Stable synthetic id for a row with no QFX FITID (a CSV-only pending
    item). Deterministic hex SHA-256 prefix over the identifying fields, so
    re-loads converge."""
    basis = "|".join([
        account_external_id, str(posted_at), f"{amount:.2f}",
        (description or "").strip(), (check_number or "").strip(),
    ])
    return "syn_" + hashlib.sha256(basis.encode("utf-8")).hexdigest()[:24]


def merge_transactions(account_external_id: str, qfx_rows: list[dict],
                       csv_rows: list[dict]) -> list[dict]:
    """Join the CSV running balance onto the QFX rows and return the ledger.

    QFX rows are authoritative (stable FITID); each takes the balance of the
    first CSV row matching (date, amount, check number). A CSV row matched
    this way is consumed, so duplicate same-day/same-amount rows pair up
    one-to-one. CSV rows left unmatched (QFX omitted them, e.g. pending) are
    appended under a synthetic id so nothing is lost."""
    csv_by_key: dict[tuple, list[dict]] = {}
    for r in csv_rows:
        csv_by_key.setdefault(_join_key(r["posted_at"], r["amount"],
                                        r["check_number"]), []).append(r)

    out = []
    for q in qfx_rows:
        key = _join_key(q["posted_at"], q["amount"], q["check_number"])
        bucket = csv_by_key.get(key)
        balance = bucket.pop(0)["balance"] if bucket else None
        fitid = q["fitid"] or synth_fitid(
            account_external_id, q["posted_at"], q["amount"],
            q["description"], q["check_number"])
        out.append({
            "fitid": fitid,
            "posted_at": q["posted_at"],
            "amount": q["amount"],
            "kind": q["kind"],
            "description": q["description"],
            "check_number": q["check_number"],
            "balance": balance,
            "source": "qfx",
            "payload": {k: q[k] for k in ("kind", "description", "memo",
                                          "check_number")},
        })

    # Leftover CSV rows QFX never carried.
    for bucket in csv_by_key.values():
        for r in bucket:
            out.append({
                "fitid": synth_fitid(account_external_id, r["posted_at"],
                                     r["amount"], r["description"],
                                     r["check_number"]),
                "posted_at": r["posted_at"],
                "amount": r["amount"],
                "kind": r["kind"],
                "description": r["description"],
                "check_number": r["check_number"],
                "balance": r["balance"],
                "source": "csv",
                "payload": {"kind": r["kind"], "description": r["description"],
                            "check_number": r["check_number"]},
            })
    return out


# ============================================================
# DB inserts
# ============================================================

def _insert_account(conn, snapshot_at: int, acct: dict) -> None:
    ext = str(acct.get("account_external_id") or "").strip()
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
        (snapshot_at, ext, acct.get("account_type"), acct.get("nickname"),
         acct.get("mask"), acct.get("currency"), parse_money(acct.get("balance")),
         payload))


def _insert_transaction(conn, account_external_id: str, tx: dict) -> None:
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


_DATE_IN_NAME_RE = re.compile(r"(20\d{2})[-_]?(\d{2})[-_]?(\d{2})")


def _statement_date_from_name(name: str) -> int | None:
    """Best-effort statement date from a filename like
    `…-statements-20260731-…pdf`. None when no date is embedded."""
    m = _DATE_IN_NAME_RE.search(name)
    if not m:
        return None
    try:
        return _epoch_day(datetime(int(m[1]), int(m[2]), int(m[3])))
    except ValueError:
        return None


# ============================================================
# Per-run loader
# ============================================================

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

    tx_dir = run_dir / "transactions"
    if tx_dir.is_dir():
        for qfx in sorted(tx_dir.glob("*.qfx")):
            ext_id = qfx.stem
            csv_path = tx_dir / f"{ext_id}.csv"
            qfx_rows = parse_qfx(qfx.read_text(encoding="utf-8", errors="replace"))
            csv_rows = (parse_csv(csv_path.read_text(encoding="utf-8", errors="replace"))
                        if csv_path.is_file() else [])
            for tx in merge_transactions(ext_id, qfx_rows, csv_rows):
                _insert_transaction(conn, ext_id, tx)
        # CSV exports for accounts with no QFX (defensive; QFX is expected).
        for csv_path in sorted(tx_dir.glob("*.csv")):
            if (tx_dir / f"{csv_path.stem}.qfx").is_file():
                continue
            ext_id = csv_path.stem
            rows = merge_transactions(
                ext_id, [], parse_csv(csv_path.read_text(encoding="utf-8",
                                                         errors="replace")))
            for tx in rows:
                _insert_transaction(conn, ext_id, tx)

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
    log.info("loaded %s (%d accounts)", run_dir.name, len(accounts))
    return True


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


def main(argv: list[str]) -> int:
    p = argparse.ArgumentParser(description=__doc__.strip().splitlines()[0])
    p.add_argument("--bronze-dir", type=Path, required=True,
                   help="Bronze tree root (holds the <UTC-ts>/ run dirs).")
    p.add_argument("--silver-db", type=Path, default=None,
                   help="Silver SQLite path. Default: <bronze-dir>/chase.db.")
    cli.add_standard_args(p, verb="load")
    args = p.parse_args(argv)
    cli.configure_logging(args.verbose)

    db_path = args.silver_db or (args.bronze_dir / "chase.db")
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
