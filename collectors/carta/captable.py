"""The cap table: securities at each event-dated snapshot, grant vesting,
and the cap-table cash flows (exercises, convertible purchases, the exit)."""
from __future__ import annotations

from pathlib import Path

from common import (_b, _cj, _date_ts, _f, _insert_cash_flow, _read_json,
                    _read_transactions_csv, _s)


# bronze security-type filename -> silver security_type label.
SECURITY_FILES = {
    "shares": "share",
    "options": "option",
    "rsu": "rsu",
    "rsa": "rsa",
    "warrants": "warrant",
    "convertibles": "convertible",
    "sar": "sar",
    "piu": "piu",
    "equity-grants": "equity_grant",
}


def _entity_canceled_date(edir: Path) -> str | None:
    """Acquisition / cancellation date for a cap-table entity, read from its
    option-grant vesting-data (`canceled_date`). One acquisition cancels the
    whole cap table on a single close date; return the latest such date
    string, or None for a live (non-exited) holding."""
    vdir = edir / "vesting"
    if not vdir.is_dir():
        return None
    best_ts, best_s = None, None
    for vf in vdir.glob("grant_*.json"):
        v = _read_json(vf)
        if not isinstance(v, dict):
            continue
        s = v.get("canceled_date")
        ts = _date_ts(s)
        if ts is not None and (best_ts is None or ts > best_ts):
            best_ts, best_s = ts, s
    return best_s


def _read_valuation_csv(path: Path) -> list[tuple[int, float]]:
    """Read a side-loaded valuation-override CSV (named
    `<entity_external_id>-valuations.csv` in the bronze root): rows of
    `YYYY-MM-DD,fmv_per_share_usd`, '#' / blank lines ignored. Returns
    [(snapshot_ts, fmv)] sorted ascending — the single source of truth for the
    position's per-share fair-market-value, each value carried forward until the
    next. Empty list if the file is absent."""
    if not path.is_file():
        return []
    out: list[tuple[int, float]] = []
    for line in path.read_text(encoding="utf-8").splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        parts = line.split(",")
        if len(parts) < 2:
            continue
        ts, fmv = _date_ts(parts[0].strip()), _f(parts[1].strip())
        if ts is not None and fmv is not None:
            out.append((ts, fmv))
    out.sort()
    return out


def _fmv_as_of(timeline: list[tuple[int, float]], ts: int) -> float | None:
    """The FMV in effect at `ts` — the latest row on/before it (carry-forward);
    None if `ts` precedes the first row. `timeline` must be sorted ascending."""
    val = None
    for t, fmv in timeline:
        if t <= ts:
            val = fmv
        else:
            break
    return val


def _insert_security(conn, snap: int, entity_id, sectype: str, row: dict, *,
                     canceled, market_value: float, position_status: str,
                     exercises: dict) -> int:
    """Write one `securities` delta row (one position at one snapshot).
    `exercises` maps a certificate id to the exercise detail behind it
    (_match_exercises); a matched certificate carries that exercise's date
    and fair-market-value."""
    ex = exercises.get(row.get("id")) if sectype == "share" else None
    conn.execute(
        "INSERT OR REPLACE INTO securities "
        "(snapshot_at, entity_external_id, security_type, "
        " security_external_id, label, issuable_type, stock_type, "
        " status, issue_date, currency, quantity, exercise_price, "
        " cost, exercised, vested, exercisable, has_vesting, "
        " is_canceled, is_expired, is_terminated, is_fully_exercised, "
        " market_value, position_status, fund_name, exercise_type, "
        " exercise_date, exercise_fmv, payload) "
        "VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
        (snap, entity_id, sectype, row.get("id"),
         _s(row.get("label")), _s(row.get("issuable_type")),
         _s(row.get("stock_type")), _s(row.get("status")),
         _s(row.get("issue_date")), _s(row.get("currency")),
         _f(row.get("quantity")), _f(row.get("exercise_price")),
         _f(row.get("cost")), _f(row.get("exercised")),
         _f(row.get("vested")), _f(row.get("exercisable")),
         _b(row.get("has_vesting")), canceled,
         _b(row.get("is_expired")), _b(row.get("is_terminated")),
         _b(row.get("is_fully_exercised")), market_value, position_status,
         _s(row.get("fund_name")), _s(row.get("exercise_type")),
         ex["date"] if ex else None, _f(ex.get("fmv")) if ex else None,
         _cj(row)),
    )
    return 1


def _held_market_value(sectype: str, qty: float | None,
                       price: float | None, row: dict) -> float:
    """Market value of a HELD cap-table security lot: a share at
    quantity x FMV; a SAFE / convertible note at its principal (the `cost` —
    Carta surfaces no share-based FMV for a convertible, and it carries at par
    until a priced round marks it or it converts); every other line
    (unexercised options, warrants, ...) at 0, its value folded into the
    shares/convertible it will become."""
    if sectype == "share" and qty is not None and price is not None:
        return qty * price
    if sectype == "convertible":
        cost = _f(row.get("cost"))
        return cost if cost is not None else 0.0
    return 0.0


def load_securities(conn, snap: int, entity_id, edir: Path, *,
                    held: bool, val_price: float | None,
                    exercises: dict) -> int:
    """Carta-derived fallback (no valuation override): write the cap-table
    securities at one snapshot. `held=True` is the live state — is_canceled
    forced 0, held shares valued at quantity x val_price (the FMV at the last
    exercise), options / other lines at 0. `held=False` is the exited state:
    is_canceled from the source, market_value 0. `exercises` as for
    _insert_security."""
    n = 0
    for fname, sectype in SECURITY_FILES.items():
        body = _read_json(edir / f"{fname}.json")
        rows = body.get("rows") if isinstance(body, dict) else None
        for row in rows or []:
            if not isinstance(row, dict) or row.get("id") is None:
                continue
            qty = _f(row.get("quantity"))
            if held:
                canceled, pstatus = 0, "held"
                mv = _held_market_value(sectype, qty, val_price, row)
            else:
                canceled, pstatus = _b(row.get("is_canceled")), "exited"
                mv = 0.0
            n += _insert_security(conn, snap, entity_id, sectype, row,
                                  canceled=canceled, market_value=mv,
                                  position_status=pstatus, exercises=exercises)
    return n


def load_securities_valued(conn, entity_id, edir: Path, *,
                           fmv_timeline: list[tuple[int, float]],
                           cancel_ts: int | None, exercises: dict) -> int:
    """Side-loaded valuation override: each share certificate is held from its
    issue date and re-valued at every FMV step in the timeline
    (quantity x FMV-as-of), so the share count *and* the per-share price both
    move correctly over time. Options / other lines carry value 0. A cancelled
    entity exits every line at cancel_ts (dropped from gold positions).
    `exercises` as for _insert_security. Returns the number of delta rows
    written."""
    fmv_dates = [t for t, _ in fmv_timeline]
    n = 0
    for fname, sectype in SECURITY_FILES.items():
        body = _read_json(edir / f"{fname}.json")
        rows = body.get("rows") if isinstance(body, dict) else None
        for row in rows or []:
            if not isinstance(row, dict) or row.get("id") is None:
                continue
            qty = _f(row.get("quantity"))
            issue_ts = _date_ts(row.get("issue_date"))
            if issue_ts is None:
                issue_ts = fmv_dates[0] if fmv_dates else cancel_ts
            # Re-value at issue + every FMV step strictly after it and before
            # any exit; the gold layer forward-fills between these.
            dates = {issue_ts}
            dates.update(t for t in fmv_dates
                         if issue_ts is not None and t > issue_ts
                         and (cancel_ts is None or t < cancel_ts))
            for t in sorted(d for d in dates if d is not None):
                fmv = _fmv_as_of(fmv_timeline, t) if sectype == "share" else None
                mv = _held_market_value(sectype, qty, fmv, row)
                n += _insert_security(conn, t, entity_id, sectype, row,
                                      canceled=0, market_value=mv,
                                      position_status="held",
                                      exercises=exercises)
            if cancel_ts is not None:
                n += _insert_security(conn, cancel_ts, entity_id, sectype, row,
                                      canceled=_b(row.get("is_canceled")),
                                      market_value=0.0,
                                      position_status="exited",
                                      exercises=exercises)
    return n


def load_vesting(conn, snap: int, entity_id, edir: Path) -> int:
    vdir = edir / "vesting"
    if not vdir.is_dir():
        return 0
    n = 0
    for vf in sorted(vdir.glob("grant_*.json")):
        gid = vf.stem.replace("grant_", "")
        v = _read_json(vf)
        if not isinstance(v, dict):
            continue
        vm = v.get("vesting_manager") or {}
        tmpl = vm.get("vesting_template") or {}
        # Schedule summary (event_data dropped from this payload — it has its
        # own table — to keep the row compact).
        summary = {k: val for k, val in v.items() if k != "vesting_event_data"}
        conn.execute(
            "INSERT OR REPLACE INTO vesting_schedules "
            "(snapshot_at, entity_external_id, grant_external_id, label, "
            " so_type, has_iso_nso_split, vesting_type, vesting_start_date, "
            " vesting_end_date, vested_shares_quantity, "
            " net_total_shares_quantity, payload) "
            "VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
            (snap, entity_id, gid, _s(v.get("label")), _s(v.get("so_type")),
             _b(v.get("has_iso_nso_split")), _s(tmpl.get("vesting_type")),
             _s(v.get("vesting_start_date")), _s(v.get("vesting_end_date")),
             _f(v.get("vested_shares_quantity")),
             _f(v.get("net_total_shares_quantity")), _cj(summary)),
        )
        for seq, ev in enumerate(v.get("vesting_event_data") or []):
            if not isinstance(ev, dict):
                continue
            conn.execute(
                "INSERT OR REPLACE INTO vesting_events "
                "(snapshot_at, grant_external_id, seq, entity_external_id, "
                " vest_date, amount, cumulative, has_vested, vesting_type, "
                " payload) VALUES (?,?,?,?,?,?,?,?,?,?)",
                (snap, gid, seq, entity_id, _s(ev.get("date")),
                 _f(ev.get("amount")), _f(ev.get("cumulative")),
                 _b(ev.get("has_vested")), _s(ev.get("vesting_type")),
                 _cj(ev)),
            )
        n += 1
    return n


def _captable_cash_flows(conn, eid, edir: Path, snap: int,
                         bronze_root: Path) -> int:
    """Cap-table cash flows: one `exercise` (deposit+buy in gold) per share
    certificate — amount = quantity x strike (the cert cost), price the strike —
    and one `convertible_purchase` (also deposit+buy, but no share lot) per
    SAFE / convertible note at its principal (cost).
    The exit is either a side-loaded `<entity_external_id>-transactions.csv` (explicit
    sale + withdrawals — canonical kinds the gold emits 1:1) or,
    absent that file, the auto-derived $0 exit at the cancellation date (Carta
    purges the payout, so the gold then omits the $0 withdrawal leg)."""
    n = 0
    held_shares = 0.0
    body = _read_json(edir / "shares.json")
    rows = body.get("rows") if isinstance(body, dict) else None
    for row in rows or []:
        if not isinstance(row, dict) or row.get("id") is None:
            continue
        qty, cost, issue = (_f(row.get("quantity")), _f(row.get("cost")),
                            _s(row.get("issue_date")))
        if qty is None or cost is None or issue is None:
            continue
        price = (cost / qty) if qty else None
        n += _insert_cash_flow(conn, f"exercise:{eid}:{row.get('id')}", eid,
                               snap, "exercise", issue, cost, qty, price,
                               "share exercise / acquisition")
        held_shares += qty
    # SAFEs / convertible notes: a cash purchase of the instrument (no shares
    # until it converts), so one `convertible_purchase` event = deposit+buy in
    # gold. amount = the principal (cost); no share lot, no exit auto-derived
    # (a conversion or write-off would arrive as its own future delta).
    cbody = _read_json(edir / "convertibles.json")
    crows = cbody.get("rows") if isinstance(cbody, dict) else None
    for row in crows or []:
        if not isinstance(row, dict) or row.get("id") is None:
            continue
        cost, issue = _f(row.get("cost")), _s(row.get("issue_date"))
        if cost is None or issue is None:
            continue
        n += _insert_cash_flow(conn, f"convertible:{eid}:{row.get('id')}", eid,
                               snap, "convertible_purchase", issue, cost,
                               None, None, "SAFE / convertible purchase")
    # Exit: a side-loaded transactions CSV (explicit legs) overrides the
    # auto-derived $0 exit. Clear the superseded rows from any prior load
    # (INSERT OR REPLACE only overwrites rows the current path re-emits, so a
    # toggled-off exit / side-load would otherwise linger).
    side = _read_transactions_csv(bronze_root / f"{eid}-transactions.csv")
    if side:
        conn.execute("DELETE FROM cash_flows WHERE cash_flow_external_id = ?",
                     (f"exit:{eid}",))
        for i, tx in enumerate(side):
            sh, amt = tx["shares"], tx["amount"]
            px = (amt / sh) if (sh and amt is not None) else None
            n += _insert_cash_flow(conn, f"tx:{eid}:{i}", eid, snap, tx["kind"],
                                   tx["flow_date"], amt, sh, px,
                                   tx["description"] or "side-loaded transaction")
        return n
    conn.execute("DELETE FROM cash_flows WHERE cash_flow_external_id LIKE ?",
                 (f"tx:{eid}:%",))
    if (cancel := _entity_canceled_date(edir)) and held_shares:
        n += _insert_cash_flow(conn, f"exit:{eid}", eid, snap, "exit", cancel,
                               0.0, held_shares, 0.0, "acquisition / exit")
    return n
