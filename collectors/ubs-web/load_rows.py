"""The row helpers the ubs-web load's passes share: the DD.MM.YYYY date
spelling, payload canonicalisation, the mortgage rate type, the rule
that no older dump overwrites a newer row, the head of every write to
`transactions`, and the bronze artefact reader."""

from __future__ import annotations

import json
import logging
import sqlite3
from datetime import datetime, timezone
from pathlib import Path

log = logging.getLogger("ubs-web.load")


def ts_from_dmy(s: str | None) -> int | None:
    """Parse 'DD.MM.YYYY' -> Unix seconds UTC, midnight."""
    if not s:
        return None
    try:
        dt = datetime.strptime(s, "%d.%m.%Y").replace(tzinfo=timezone.utc)
    except ValueError:
        return None
    return int(dt.timestamp())


def normalize_payload(data: dict) -> str:
    """Canonical JSON for embedding in payload columns."""
    return json.dumps(data, ensure_ascii=False, sort_keys=True, default=str)


def _mortgage_rate_type(description_1: str | None) -> str | None:
    """Map a mortgage's product name to 'fixed' / 'variable': the
    positions export's Description 1, or the product a maturity notice
    prints. Returns None when neither cue is present so the row stays
    honest about an unknown rate basis.

    One reading for both feeds, so a mortgage gets one rate type: a
    SARON mortgage is UBS's variable-rate product."""
    if not description_1:
        return None
    low = description_1.lower()
    if "saron" in low:
        return "variable"
    if "fixed-rate" in low or "fixed rate" in low or "festhypothek" in low:
        return "fixed"
    if "variable" in low or "variabel" in low or "variabler" in low:
        return "variable"
    return None


# A dump that failed to load is loaded again by a later load, after dumps
# newer than it. It then adds the rows silver lacks and leaves every row a
# newer dump wrote as that dump wrote it: a table that stamps the dump that
# wrote a row compares the stamps (`_written_by_a_newer_dump`, and the
# `WHERE` of the `transactions` upserts); the card ledger, whose stamp is
# the first dump that saw a row, updates no row once a newer dump is
# loaded.
_NEWER_DUMP_LOADED = ("EXISTS (SELECT 1 FROM dump_runs "
                      "WHERE dump_runs.snapshot_at > excluded.snapshot_at)")


def _written_by_a_newer_dump(conn: sqlite3.Connection, table: str,
                             key: dict, snapshot_at: int) -> bool:
    """Whether `table` holds the row `key` names, stamped by a dump newer
    than `snapshot_at`."""
    where = " AND ".join(f"{col} = ?" for col in key)
    return conn.execute(
        f"SELECT 1 FROM {table} WHERE {where} AND snapshot_at > ?",
        (*key.values(), snapshot_at)).fetchone() is not None


# The head every write to `transactions` shares; each era closes it with
# its own conflict action.
_TXN_INSERT = (
    "INSERT INTO transactions ("
    "transaction_external_id, account_external_id, snapshot_at, "
    "trade_date, booking_date, value_date, currency_iso, "
    "amount_debit, amount_credit, counterparty, "
    "description_kind, payload"
    ") VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) "
    "ON CONFLICT(transaction_external_id, account_external_id) "
)
# The condition of the eras that update: no dump older than the row's
# writer overwrites it.
_TXN_NOT_OLDER = "WHERE excluded.snapshot_at >= transactions.snapshot_at"


def _read_artefact_json(path: Path, kind: str):
    """Read one bronze JSON artefact, or None when it does not read. An
    unreadable file warns, so one corrupt artefact costs its own facet
    rather than the whole dump."""
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, ValueError) as e:
        log.warning("unreadable %s artefact %s: %s", kind, path.name, e)
        return None
