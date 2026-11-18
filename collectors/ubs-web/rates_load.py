"""The ubs-web rate pass: the cash accounts' interest conditions."""

from __future__ import annotations

import logging
import sqlite3
from pathlib import Path

import rate_parsers  # local module
from load_rows import _read_artefact_json, normalize_payload

log = logging.getLogger("ubs-web.load")

_RATE_COLUMNS = ("snapshot_at", "account_external_id", "side", "tier",
                 "rate_pct", "upper_limit", "valid_from", "surface",
                 "payload")


def _load_rates(conn: sqlite3.Connection, snapshot_at: int,
                dump_dir: Path) -> int:
    """Load `<dump>/rates/` into `account_rates`.

    The account API's JSON keys on the IBAN the response states; a
    legacy page on the IBAN its account chooser selects. An account the
    API stated conditions for keeps the API's, and a legacy page for it
    is not read. A response without `interestItems` states none, so it
    claims no account. A file that states no condition writes no row,
    so such an account reads as stating no rate. A file that does not
    read, or names no IBAN, warns and is skipped. A dump with no
    `rates/` dir loads as zero. The dump's earlier rows go first, so a
    re-read that states fewer tiers leaves none of the old ones behind.
    """
    conn.execute("DELETE FROM account_rates WHERE snapshot_at = ?",
                 (snapshot_at,))
    rates_dir = dump_dir / "rates"
    if not rates_dir.is_dir():
        return 0
    by_account: dict[str, tuple[str, list[dict]]] = {}
    for path in sorted(rates_dir.glob("*.json")):
        payload = _read_artefact_json(path, "rate")
        if payload is None:
            continue
        if not rate_parsers.api_states_conditions(payload):
            log.warning("rate artefact %s states no conditions; skipped",
                        path.name)
            continue
        iban = rate_parsers.api_iban(payload)
        if iban is None:
            log.warning("rate artefact %s states no IBAN; skipped", path.name)
            continue
        by_account[iban] = ("api", rate_parsers.api_conditions(payload))
    for path in sorted(rates_dir.glob("legacy-*.html")):
        try:
            html = path.read_text(encoding="utf-8")
        except OSError as e:
            log.warning("unreadable rate artefact %s: %s", path.name, e)
            continue
        iban = rate_parsers.legacy_iban(html)
        if iban is None:
            log.warning("rate page %s states no IBAN; skipped", path.name)
            continue
        if iban not in by_account:
            by_account[iban] = ("legacy",
                                rate_parsers.legacy_conditions(html))

    rows = [(snapshot_at, iban, c["side"], c["tier"], c["rate_pct"],
             c["upper_limit"], c["valid_from"], surface,
             normalize_payload(c["source"]))
            for iban, (surface, conditions) in sorted(by_account.items())
            for c in conditions]
    conn.executemany(
        f"INSERT INTO account_rates ({', '.join(_RATE_COLUMNS)}) "
        f"VALUES ({', '.join('?' * len(_RATE_COLUMNS))})", rows)
    return len(rows)
