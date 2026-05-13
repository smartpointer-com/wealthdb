#!/usr/bin/env python3
"""
schwab-dump silver loader.

Reads bronze JSON dumps (produced by download.py) and inserts them into
a SQLite silver database. Applies any pending schema migrations on
startup. Each dump is loaded atomically: a failure mid-load rolls back
to the prior state.

Usage:
    load.py --silver-db <path> --bronze-dir <path>

Each immediate subdirectory of <bronze-dir> whose name matches the
schwab-dump timestamp format (YYYYMMDDTHHMMSSZ) is considered a dump.
Already-loaded dumps (recorded in dump_runs) are skipped.
"""

from __future__ import annotations

import argparse
import copy
import json
import logging
import re
import sqlite3
import sys
from datetime import datetime, timezone
from pathlib import Path

log = logging.getLogger("schwab-load")

MIGRATIONS_DIR = Path(__file__).parent / "migrations"
SNAPSHOT_DIR_RE = re.compile(r"^(\d{8}T\d{6}Z)$")
MIGRATION_FILE_RE = re.compile(r"^(\d{4})_.*\.sql$")

# Balance kinds inside securitiesAccount, mapping silver-column value -> source key.
SECURITIES_ACCOUNT_BALANCE_KINDS = (
    ("initial",   "initialBalances"),
    ("current",   "currentBalances"),
    ("projected", "projectedBalances"),
)


# --------------------------------------------------------------------------
# Small helpers
# --------------------------------------------------------------------------

def parse_snapshot_at(dump_dir_name: str) -> int:
    """Parse a dump directory name like '20260512T104753Z' to Unix seconds UTC."""
    m = SNAPSHOT_DIR_RE.match(dump_dir_name)
    if not m:
        raise ValueError(f"Not a snapshot directory name: {dump_dir_name!r}")
    dt = datetime.strptime(m.group(1), "%Y%m%dT%H%M%SZ").replace(tzinfo=timezone.utc)
    return int(dt.timestamp())


def parse_schwab_timestamp(s: str) -> int:
    """Parse Schwab's `time` field (e.g. '2026-04-09T21:18:56+0000') to Unix seconds.

    Schwab emits a numeric-only offset (`+0000`); Python's fromisoformat needs
    `+00:00`. We normalise before parsing."""
    if re.search(r"[+-]\d{4}$", s):
        s = s[:-5] + s[-5:-2] + ":" + s[-2:]
    return int(datetime.fromisoformat(s).timestamp())


def parse_iso_date_start_of_day(s: str) -> int:
    """Parse 'YYYY-MM-DD' to Unix seconds at 00:00:00 UTC."""
    dt = datetime.strptime(s, "%Y-%m-%d").replace(tzinfo=timezone.utc)
    return int(dt.timestamp())


def parse_iso_date_end_of_day(s: str) -> int:
    """Parse 'YYYY-MM-DD' to Unix seconds at 23:59:59 UTC."""
    dt = datetime.strptime(s, "%Y-%m-%d").replace(
        tzinfo=timezone.utc, hour=23, minute=59, second=59,
    )
    return int(dt.timestamp())


def canonical_json(obj) -> str:
    """Return the canonical compact JSON serialization (sorted keys, no spaces).

    Used both for storage (one representation of equivalent objects) and for
    dedup comparison."""
    return json.dumps(obj, sort_keys=True, separators=(",", ":"), default=str)


def read_json(path: Path):
    with path.open(encoding="utf-8") as f:
        return json.load(f)


# --------------------------------------------------------------------------
# Database / migrations
# --------------------------------------------------------------------------

def open_db(path: Path) -> sqlite3.Connection:
    path.parent.mkdir(parents=True, exist_ok=True)
    conn = sqlite3.connect(path)
    conn.execute("PRAGMA foreign_keys = ON")
    return conn


def current_schema_version(conn: sqlite3.Connection) -> int:
    cur = conn.execute(
        "SELECT name FROM sqlite_master WHERE type='table' AND name='schema_meta'"
    )
    if cur.fetchone() is None:
        return 0
    cur = conn.execute(
        "SELECT COALESCE(MAX(silver_schema_version), 0) FROM schema_meta"
    )
    return cur.fetchone()[0]


def discover_migrations() -> list[tuple[int, Path]]:
    items: list[tuple[int, Path]] = []
    for f in sorted(MIGRATIONS_DIR.glob("*.sql")):
        m = MIGRATION_FILE_RE.match(f.name)
        if m:
            items.append((int(m.group(1)), f))
    items.sort(key=lambda v: v[0])
    return items


def apply_migrations(conn: sqlite3.Connection) -> None:
    current = current_schema_version(conn)
    for version, path in discover_migrations():
        if version <= current:
            continue
        log.info("Applying migration %s", path.name)
        conn.executescript(path.read_text(encoding="utf-8"))
        conn.commit()
    final = current_schema_version(conn)
    log.info("Schema at version %d", final)


# --------------------------------------------------------------------------
# Per-artefact loaders. All take `conn` already inside a transaction.
# --------------------------------------------------------------------------

def load_accounts(conn, snapshot_at: int, accounts_data: list[dict]) -> int:
    """Insert account rows when the canonical payload differs from the latest."""
    n = 0
    for acct in accounts_data:
        external_id = acct["hashValue"]
        payload = canonical_json(acct)
        row = conn.execute(
            "SELECT payload FROM accounts WHERE account_external_id = ? "
            "ORDER BY snapshot_at DESC LIMIT 1",
            (external_id,),
        ).fetchone()
        if row is None or row[0] != payload:
            conn.execute(
                "INSERT INTO accounts(snapshot_at, account_external_id, payload) "
                "VALUES (?, ?, ?)",
                (snapshot_at, external_id, payload),
            )
            n += 1
    return n


def _strip_user_preference_noise(pref: dict) -> dict:
    """Drop per-request noise from a user_preference payload.

    Schwab regenerates streamerInfo[*].schwabClientCorrelId on every API
    call. It carries no information about the underlying preferences,
    so silver discards it; bronze keeps the original if anyone needs to
    trace a specific request."""
    out = copy.deepcopy(pref)
    for entry in out.get("streamerInfo") or []:
        entry.pop("schwabClientCorrelId", None)
    return out


def load_user_preference(conn, snapshot_at: int, dump_dir: Path) -> int:
    path = dump_dir / "user_preference.json"
    if not path.exists():
        log.warning("Missing artefact: %s", path)
        return 0
    payload = canonical_json(_strip_user_preference_noise(read_json(path)))
    row = conn.execute(
        "SELECT payload FROM user_preference ORDER BY snapshot_at DESC LIMIT 1"
    ).fetchone()
    if row is not None and row[0] == payload:
        return 0
    conn.execute(
        "INSERT INTO user_preference(snapshot_at, payload) VALUES (?, ?)",
        (snapshot_at, payload),
    )
    return 1


def load_accounts_positions(
    conn, snapshot_at: int, dump_dir: Path, acct_map: dict[str, str]
) -> tuple[int, int]:
    """Returns (positions_inserted, balances_inserted)."""
    path = dump_dir / "accounts_positions.json"
    if not path.exists():
        log.warning("Missing artefact: %s", path)
        return 0, 0

    wrappers = read_json(path)
    pos_rows: list[tuple] = []
    bal_rows: list[tuple] = []
    for wrapper in wrappers:
        sa = wrapper.get("securitiesAccount") or {}
        agg = wrapper.get("aggregatedBalance")
        acct_plain = sa.get("accountNumber")
        if acct_plain is None or acct_plain not in acct_map:
            log.warning("Account-positions wrapper for unknown account %r — skipping",
                        acct_plain)
            continue
        acct_hash = acct_map[acct_plain]

        for kind_value, source_key in SECURITIES_ACCOUNT_BALANCE_KINDS:
            if source_key in sa:
                bal_rows.append((
                    snapshot_at, acct_hash, kind_value,
                    canonical_json(sa[source_key]),
                ))
        if agg is not None:
            bal_rows.append((
                snapshot_at, acct_hash, "aggregated", canonical_json(agg),
            ))

        for pos in sa.get("positions") or []:
            inst = pos.get("instrument") or {}
            instrument_key = inst.get("cusip") or inst.get("symbol")
            if not instrument_key:
                log.warning("Position with no cusip or symbol on account %s — skipping",
                            acct_hash[:8])
                continue
            pos_rows.append((
                snapshot_at, acct_hash, instrument_key, canonical_json(pos),
            ))

    if bal_rows:
        conn.executemany(
            "INSERT INTO account_balances"
            "(snapshot_at, account_external_id, balance_kind, payload) "
            "VALUES (?, ?, ?, ?)",
            bal_rows,
        )
    if pos_rows:
        conn.executemany(
            "INSERT INTO positions"
            "(snapshot_at, account_external_id, instrument_key, payload) "
            "VALUES (?, ?, ?, ?)",
            pos_rows,
        )
    return len(pos_rows), len(bal_rows)


def load_transactions(conn, dump_dir: Path) -> int:
    """Window-DELETE then INSERT for each transactions_NNN.json file.

    Each file declares its own (account_hash, window_start, window_end);
    we replace exactly that range so upstream removals are caught."""
    files = sorted(dump_dir.glob("transactions_*.json"))
    total = 0
    for path in files:
        data = read_json(path)
        acct_hash = data["account_hash"]
        window_start = parse_iso_date_start_of_day(data["window_start"])
        window_end = parse_iso_date_end_of_day(data["window_end"])

        conn.execute(
            "DELETE FROM transactions "
            "WHERE account_external_id = ? AND timestamp >= ? AND timestamp <= ?",
            (acct_hash, window_start, window_end),
        )

        rows = []
        for txn in data.get("transactions") or []:
            rows.append((
                str(txn["activityId"]),
                parse_schwab_timestamp(txn["time"]),
                acct_hash,
                txn["type"],
                canonical_json(txn),
            ))
        if rows:
            conn.executemany(
                "INSERT INTO transactions"
                "(activity_id, timestamp, account_external_id, kind, payload) "
                "VALUES (?, ?, ?, ?, ?)",
                rows,
            )
        total += len(rows)
    return total


def load_open_orders(
    conn, snapshot_at: int, dump_dir: Path, acct_map: dict[str, str]
) -> int:
    path = dump_dir / "open_orders.json"
    if not path.exists():
        log.warning("Missing artefact: %s", path)
        return 0
    data = read_json(path)
    rows = []
    for order in data.get("orders") or []:
        acct_plain = order.get("accountNumber")
        if acct_plain is None or acct_plain not in acct_map:
            log.warning("Order references unknown account %r — skipping", acct_plain)
            continue
        rows.append((
            snapshot_at,
            acct_map[acct_plain],
            str(order["orderId"]),
            order["status"],
            canonical_json(order),
        ))
    if rows:
        conn.executemany(
            "INSERT INTO open_orders"
            "(snapshot_at, account_external_id, order_external_id, status, payload) "
            "VALUES (?, ?, ?, ?, ?)",
            rows,
        )
    return len(rows)


# --------------------------------------------------------------------------
# Driver
# --------------------------------------------------------------------------

def load_dump(conn: sqlite3.Connection, dump_dir: Path) -> dict:
    """Load a single dump directory. Returns a stats dict.

    Atomic: if any step raises, the entire dump is rolled back and
    dump_runs is *not* written. Re-running will retry the dump."""
    name = dump_dir.name
    snapshot_at = parse_snapshot_at(name)

    already = conn.execute(
        "SELECT 1 FROM dump_runs WHERE snapshot_at = ?", (snapshot_at,)
    ).fetchone()
    if already is not None:
        return {"name": name, "skipped": True}

    log.info("Loading dump %s (snapshot_at=%d)", name, snapshot_at)

    # accounts list is needed up-front to translate plaintext accountNumber
    # to hashValue for the other artefacts.
    accounts_path = dump_dir / "account_numbers.json"
    if not accounts_path.exists():
        raise FileNotFoundError(
            f"Required artefact missing: {accounts_path}. Cannot translate "
            f"plaintext account numbers to hashes."
        )
    accounts_data = read_json(accounts_path)
    acct_map = {a["accountNumber"]: a["hashValue"] for a in accounts_data}

    stats: dict = {"name": name, "snapshot_at": snapshot_at, "skipped": False}

    # Python sqlite3 connection-as-context-manager: BEGIN on entry,
    # COMMIT on clean exit, ROLLBACK on exception.
    with conn:
        stats["accounts"] = load_accounts(conn, snapshot_at, accounts_data)
        stats["user_preference"] = load_user_preference(conn, snapshot_at, dump_dir)
        pos, bal = load_accounts_positions(conn, snapshot_at, dump_dir, acct_map)
        stats["positions"] = pos
        stats["balances"] = bal
        stats["transactions"] = load_transactions(conn, dump_dir)
        stats["open_orders"] = load_open_orders(conn, snapshot_at, dump_dir, acct_map)

        # dump_runs at the END so a mid-load failure leaves no trace.
        conn.execute(
            "INSERT INTO dump_runs"
            "(snapshot_at, silver_schema_version, run_dir) VALUES (?, ?, ?)",
            (snapshot_at, current_schema_version(conn), str(dump_dir.resolve())),
        )
    return stats


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(description=__doc__.strip())
    p.add_argument("--silver-db", required=True, type=Path,
                   help="Path to the silver SQLite database. Created if missing.")
    p.add_argument("--bronze-dir", required=True, type=Path,
                   help="Directory containing snapshot subdirectories.")
    p.add_argument("-v", "--verbose", action="store_true", help="DEBUG-level logging.")
    return p.parse_args(argv)


def main(argv: list[str] | None = None) -> int:
    args = parse_args(sys.argv[1:] if argv is None else argv)
    logging.basicConfig(
        level=logging.DEBUG if args.verbose else logging.INFO,
        format="%(asctime)s %(levelname)s %(name)s: %(message)s",
    )

    conn = open_db(args.silver_db)
    apply_migrations(conn)

    if not args.bronze_dir.is_dir():
        raise SystemExit(f"Bronze directory not found: {args.bronze_dir}")

    dumps = [
        d for d in sorted(args.bronze_dir.iterdir())
        if d.is_dir() and SNAPSHOT_DIR_RE.match(d.name)
    ]
    log.info("Found %d dump directory(s) under %s", len(dumps), args.bronze_dir)

    for d in dumps:
        stats = load_dump(conn, d)
        if stats.get("skipped"):
            log.info("  %s: skipped (already loaded)", stats["name"])
        else:
            log.info(
                "  %s: accounts=%d user_pref=%d positions=%d balances=%d "
                "transactions=%d open_orders=%d",
                stats["name"],
                stats["accounts"], stats["user_preference"],
                stats["positions"], stats["balances"],
                stats["transactions"], stats["open_orders"],
            )

    conn.close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
