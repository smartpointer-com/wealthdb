"""What carta's loader modules share: the safe converters, the JSON and
supplied-CSV readers, Carta's two date shapes, and the one writer of a
cash_flows row."""
from __future__ import annotations

import datetime as dt
import json
import logging
from pathlib import Path

log = logging.getLogger("carta.load")


# ---- safe converters --------------------------------------------------------

def _f(x):
    """float(x) or None — tolerates strings, None, '', and stray types."""
    if x is None or x == "":
        return None
    try:
        return float(x)
    except (TypeError, ValueError):
        return None


def _b(x):
    """bool -> 0/1, None -> None."""
    return None if x is None else (1 if x else 0)


def _s(x):
    return None if x is None else str(x)


def _cj(obj) -> str:
    return json.dumps(obj, sort_keys=True, default=str)


def _read_json(path: Path):
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        log.warning("could not read %s: %s", path, exc)
        return None


def _date_ts(s) -> int | None:
    """Parse a Carta date ('YYYY-MM-DD' or 'MM/DD/YYYY') to a Unix timestamp
    at UTC midnight — the snapshot grain is at-most-daily. None on empty /
    unparseable."""
    if not isinstance(s, str) or not s.strip():
        return None
    for fmt in ("%Y-%m-%d", "%m/%d/%Y"):
        try:
            return int(dt.datetime.strptime(s.strip(), fmt)
                       .replace(tzinfo=dt.timezone.utc).timestamp())
        except ValueError:
            continue
    return None


def _read_transactions_csv(path: Path) -> list[dict]:
    """Read a side-loaded transactions CSV (named
    `<entity_external_id>-transactions.csv` in the bronze root): rows of
    `date,kind,amount,shares,description`, '#' /
    blank lines ignored; `amount` is a positive magnitude (USD).

    For a company, `kind` is a canonical transaction kind the gold emits 1:1
    (sell | withdrawal | deposit | buy | contribution), and the rows override
    the auto-derived $0 exit (e.g. a sale plus the withdrawals it splits
    into). For a fund, `kind` is `capital_call` or `distribution`,
    and the rows itemise what the fund reports only as a lump before Carta's
    coverage (_fund_cash_flows). Returns the parsed rows in file order; empty
    if absent."""
    if not path.is_file():
        return []
    out: list[dict] = []
    for line in path.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        parts = [p.strip() for p in line.split(",", 4)]
        if len(parts) < 3 or not parts[0] or not parts[1]:
            continue
        out.append({
            "flow_date": parts[0],
            "kind": parts[1],
            "amount": _f(parts[2]),
            "shares": _f(parts[3]) if len(parts) > 3 and parts[3] else None,
            "description": parts[4] if len(parts) > 4 else None,
        })
    return out


def _insert_cash_flow(conn, cfid: str, eid, snap: int, kind: str,
                      flow_date: str | None, amount: float,
                      shares: float | None, price: float | None,
                      description: str) -> int:
    """Write one cash_flows ledger row (a positive-magnitude event)."""
    conn.execute(
        "INSERT OR REPLACE INTO cash_flows "
        "(cash_flow_external_id, entity_external_id, snapshot_at, kind, "
        " flow_date, amount, shares, price_per_share, currency, description, "
        " payload) VALUES (?,?,?,?,?,?,?,?,?,?,?)",
        (cfid, eid, snap, kind, flow_date, amount, shares, price, "USD",
         description, _cj({"kind": kind, "flow_date": flow_date,
                           "amount": amount, "shares": shares,
                           "price_per_share": price})))
    return 1
