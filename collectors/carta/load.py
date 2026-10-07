#!/usr/bin/env python3
"""carta silver loader.

Parses the bronze JSON + PDF archive captured by download.py into the
source-shaped SQLite silver DB (the input contract to the gold engine).
Idempotent: a bronze run whose snapshot_at is already in `dump_runs` is
skipped unless --force. Each run loads atomically (one transaction); a
failure mid-run rolls back to the prior state.

Each download is reconstructed into an event-dated snapshot *series* — one
snapshot per capital-event / statement day, not a single download-dated
snapshot (DESIGN.md §5.1); `dump_runs` stays at the download time for
idempotency only.

Bronze → silver mapping (schema in migrations/):
  bootstrap/ + run.json        -> dump_runs (ids, counts, provenance)
  entities/<e>/meta.json
            + holdings-dashboard.json
                               -> entities (one row per snapshot per entity)
  entities/<e>/<sectype>.json  -> securities ({rows} of each security-type file)
  entities/<e>/vesting/grant_*.json
                               -> vesting_schedules + vesting_events
  <entity_external_id>-valuations.csv
                               -> valuation override (bronze root, optional):
                                  per-date FMV, the single source of truth for
                                  held-share value when present (DESIGN.md §5.1)
  <entity_external_id>-transactions.csv
                               -> supplied transactions (bronze root,
                                  optional): for a company the exit legs
                                  (a sale + its withdrawals), overriding the
                                  $0 exit; for a fund the capital calls made
                                  before Carta's coverage (§5.2)
  entities/<e>/exercises/grant_*_er_*.xlsx
                               -> FMV at last exercise (fallback held-share
                                  value), and each exercise's date + FMV on
                                  the share certificate it produced
  entities/<e>/fund-admin/partner-metrics.json
                               -> fund_metrics (latest LP capital account)
  entities/<e>/fund-cap-calls.json
                               -> cap_calls
  documents/index.json + doc_*.pdf
                               -> documents (content-deduped on sha256);
                                  capital-account statements also parsed
                                  -> fund_metrics quarterly NAV history;
                                  K-1s -> k1_capital_accounts (k1.py)

SQLite + JSON1 (the repo default — no DuckDB need here; this is shape
transformation, not a window-function replay). collectorkit.silver provides
the migration runner + dump_runs idempotency; collectorkit.bronze provides
run-dir discovery + snapshot timestamp parsing.
"""
from __future__ import annotations

import argparse
import datetime as dt
import hashlib
import json
import logging
import re
import sys
import xml.etree.ElementTree as ET
import zipfile
from pathlib import Path

from collectorkit import bronze, cli, pdftotext, silver

import k1

log = logging.getLogger("carta.load")

HERE = Path(__file__).resolve().parent
MIGRATIONS_DIR = HERE / "migrations"

DEFAULT_BRONZE_DIR = Path("/data")
DEFAULT_SILVER_DB = Path("/data/carta.db")

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


# ---- timeline / valuation helpers (event-driven snapshots, DESIGN.md §5) ----

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


def _last_exercise_price(edir: Path) -> float | None:
    """Fallback valuation basis when no exercise-detail xlsx was captured —
    the highest strike among option grants with any exercised quantity. The
    primary basis is _last_exercise_fmv (the fair-market-value at the last
    exercise). None if nothing was exercised."""
    body = _read_json(edir / "options.json")
    rows = body.get("rows") if isinstance(body, dict) else None
    prices = [
        _f(r.get("exercise_price")) for r in (rows or [])
        if isinstance(r, dict) and (_f(r.get("exercised")) or 0) > 0
        and _f(r.get("exercise_price")) is not None
    ]
    return max(prices) if prices else None


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


def _fund_sharing_date(edir: Path) -> str | None:
    """The LP capital account's effective / sharing date (partner-metrics)."""
    body = _read_json(edir / "fund-admin" / "partner-metrics.json")
    if isinstance(body, list) and body and isinstance(body[0], dict):
        return body[0].get("sharing_date")
    return None


_XL_NS = "{http://schemas.openxmlformats.org/spreadsheetml/2006/main}"


def _parse_exercise_xlsx(path: Path) -> dict | None:
    """Parse a Carta exercise-detail xlsx into {grant_label, date, shares,
    exercise_price, fmv}. The sheet opens with a title naming the grant
    ('Exercise details for <grant label> (<holder>)'), then label/value rows
    ('Grant exercised on <date>', 'Shares exercised', 'Exercise price', 'Fair
    market value on exercise'); the value trails its label cell. xlsx == zip
    of XML, parsed with the stdlib. None if unreadable / no date."""
    try:
        z = zipfile.ZipFile(path)
        try:
            shared = [(t.text or "") for t in ET.fromstring(
                z.read("xl/sharedStrings.xml")).iter(_XL_NS + "t")]
        except KeyError:
            shared = []
        sheet = ET.fromstring(z.read("xl/worksheets/sheet1.xml"))
    except (OSError, zipfile.BadZipFile, ET.ParseError, KeyError) as exc:
        log.warning("could not read exercise xlsx %s: %s", path.name, exc)
        return None
    cells: list[str] = []
    for c in sheet.iter(_XL_NS + "c"):
        v = c.find(_XL_NS + "v")
        is_ = c.find(_XL_NS + "is")
        if is_ is not None:
            txt = "".join(x.text or "" for x in is_.iter(_XL_NS + "t"))
        elif v is not None:
            txt = shared[int(v.text)] if c.get("t") == "s" else v.text
        else:
            txt = ""
        cells.append((txt or "").strip())

    def value_after(label: str):
        for i, cell in enumerate(cells):
            if cell.startswith(label):
                for nxt in cells[i + 1:i + 4]:
                    if re.fullmatch(r"[\d.]+", nxt):
                        return nxt
        return None

    blob = "\n".join(cells)
    m = re.search(r"exercised on\s+(\d{2}/\d{2}/\d{4})", blob)
    if not m:
        return None
    title = re.search(r"Exercise details for\s+(\S+)", blob)
    return {"grant_label": title.group(1) if title else None,
            "date": m.group(1), "shares": value_after("Shares exercised"),
            "exercise_price": value_after("Exercise price"),
            "fmv": value_after("Fair market value")}


def _read_exercises(edir: Path) -> list[dict]:
    """Every exercise-detail xlsx under exercises/, parsed, each with the
    `grant_id` its file name (grant_<gid>_er_<erid>.xlsx) carries. Sorted
    by file name, so the result does not depend on directory order."""
    xdir = edir / "exercises"
    if not xdir.is_dir():
        return []
    out = []
    for xl in sorted(xdir.glob("*.xlsx")):
        ex = _parse_exercise_xlsx(xl)
        if ex:
            m = re.match(r"grant_(\d+)_er_", xl.name)
            out.append({**ex, "grant_id": m.group(1) if m else None})
    return out


def _last_exercise_fmv(exercises: list[dict]) -> float | None:
    """The fair-market-value at the most recent exercise — the holder's
    after-exercise valuation basis (shares exercised at a strike below FMV
    are worth FMV). None if no exercise detail states one."""
    best_ts, best_fmv = None, None
    for ex in exercises:
        if ex.get("fmv") is None:
            continue
        ts = _date_ts(ex.get("date"))
        if ts is not None and (best_ts is None or ts > best_ts):
            best_ts, best_fmv = ts, _f(ex["fmv"])
    return best_fmv


# The security lists whose rows are grants that can be exercised into shares;
# a certificate's `exercise_from` names one of them by its label.
_GRANT_FILES = ("options", "equity-grants", "rsu", "sar")


def _match_exercises(edir: Path, exercises: list[dict]) -> dict:
    """Pair each share certificate born from an exercise with the exercise
    detail behind it: {certificate id: exercise}.

    A certificate names its grant by label (`exercise_from`) and states its
    quantity and issue date; an exercise detail states its grant, the shares
    exercised and the exercise date. The certificate is issued on or some
    days after the exercise, so dates alone do not pair them. Within one
    grant and one quantity, certificates and exercises pair in date order:
    each certificate, oldest first, takes the oldest unpaired exercise dated
    on or before its issue date. A certificate with no such exercise is left
    out."""
    labels: dict[str, str] = {}
    for fname in _GRANT_FILES:
        body = _read_json(edir / f"{fname}.json")
        for row in (body.get("rows") if isinstance(body, dict) else None) or []:
            if isinstance(row, dict) and row.get("id") is not None:
                labels[str(row["id"])] = _s(row.get("label"))
    pool: dict[tuple, list[tuple[int, dict]]] = {}
    for ex in exercises:
        label = labels.get(ex.get("grant_id")) or ex.get("grant_label")
        ts, qty = _date_ts(ex.get("date")), _f(ex.get("shares"))
        if label and ts is not None and qty is not None:
            pool.setdefault((label, qty), []).append((ts, ex))
    for group in pool.values():
        group.sort(key=lambda p: p[0])

    body = _read_json(edir / "shares.json")
    certs = []
    for row in (body.get("rows") if isinstance(body, dict) else None) or []:
        if not isinstance(row, dict) or not row.get("exercise_from"):
            continue
        ts = _date_ts(row.get("issue_date"))
        if row.get("id") is not None and ts is not None:
            certs.append((ts, row))
    out = {}
    for ts, row in sorted(certs, key=lambda c: c[0]):
        group = pool.get((row["exercise_from"], _f(row.get("quantity"))), [])
        for i, (ex_ts, ex) in enumerate(group):
            if ex_ts <= ts:
                out[row["id"]] = ex
                del group[i]
                break
    return out


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


# ---- per-table loaders ------------------------------------------------------

def load_entity(conn, snap: int, ind_id: str, firm_id: str | None,
                edir: Path) -> dict | None:
    """Insert one `entities` row from meta.json + holdings-dashboard.json.
    Returns the parsed meta dict (for fund/captable dispatch) or None."""
    meta = _read_json(edir / "meta.json")
    if not isinstance(meta, dict):
        return None
    hd = _read_json(edir / "holdings-dashboard.json")
    hd = hd if isinstance(hd, dict) else {}
    conn.execute(
        "INSERT OR REPLACE INTO entities "
        "(snapshot_at, entity_external_id, individual_id, firm_id, "
        " is_fund_investment, entity_type, legal_name, dba, held_since, "
        " ownership, cash_cost, payload) "
        "VALUES (?,?,?,?,?,?,?,?,?,?,?,?)",
        (snap, meta.get("corporation_id"), ind_id, firm_id,
         _b(meta.get("is_fund_investment")), _s(meta.get("entity_type")),
         _s(meta.get("legal_name")), _s(meta.get("dba")),
         _s(hd.get("held_since")), _f(hd.get("ownership")),
         _f(hd.get("cash_cost")),
         _cj({"meta": meta, "holdings_dashboard": hd})),
    )
    return meta


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
                _insert_security(conn, cancel_ts, entity_id, sectype, row,
                                 canceled=_b(row.get("is_canceled")),
                                 market_value=0.0, position_status="exited",
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


# fund_metrics columns a partner-metrics row states, in load_fund_metrics'
# value order.
_PARTNER_COLUMNS = ("fund_external_id", "fund_uuid", "currency",
                    "vintage_year", "commitment", "called_capital",
                    "capital_contributed", "capital_contributed_paid",
                    "distributions", "net_asset_value",
                    "capital_call_liabilities", "prepaid_capital_contribution",
                    "sharing_date", "accepted_date", "payload")


def load_fund_metrics(conn, snap: int, entity_id, edir: Path) -> int:
    body = _read_json(edir / "fund-admin" / "partner-metrics.json")
    if not isinstance(body, list) or not body:
        return 0
    if len(body) > 1:
        log.info("entity %s: partner-metrics has %d funds; using the first",
                 entity_id, len(body))
    pm = body[0]
    m = pm.get("metrics") or {}
    partner = pm.get("partner") or {}
    # An upsert over the partner-metrics columns only: the statement's
    # inception-to-date lines merged onto this row (load_statement_nav)
    # survive a later run that shares at the same date but carries no
    # statements, such as a `download --no-documents` run.
    conn.execute(
        "INSERT INTO fund_metrics "
        f"(snapshot_at, entity_external_id, {', '.join(_PARTNER_COLUMNS)}) "
        f"VALUES (?,?,{','.join('?' * len(_PARTNER_COLUMNS))}) "
        "ON CONFLICT (snapshot_at, entity_external_id) DO UPDATE SET "
        + ", ".join(f"{c} = excluded.{c}" for c in _PARTNER_COLUMNS),
        (snap, entity_id, _s(partner.get("fund_id")),
         _s(partner.get("fund_uuid")), _s(partner.get("fund_currency")),
         m.get("vintage_year"), _s(m.get("commitment")),
         _s(m.get("called_capital")), _s(m.get("capital_contributed")),
         _s(m.get("capital_contributed_paid")), _s(m.get("distributions")),
         _s(m.get("net_asset_value")), _s(m.get("capital_call_liabilities")),
         _s(m.get("prepaid_capital_contribution")), _s(pm.get("sharing_date")),
         _s(partner.get("accepted_date")), _cj(pm)),
    )
    return 1


def load_cap_calls(conn, snap: int, entity_id, edir: Path) -> int:
    body = _read_json(edir / "fund-cap-calls.json")
    if not isinstance(body, list):
        return 0
    n = 0
    for i, c in enumerate(body):
        if not isinstance(c, dict):
            continue
        call_id = str(c.get("id") or c.get("uuid") or f"synth_{i}")
        conn.execute(
            "INSERT OR REPLACE INTO cap_calls "
            "(snapshot_at, entity_external_id, call_external_id, due_date, "
            " amount, currency, status, payload) VALUES (?,?,?,?,?,?,?,?)",
            (snap, entity_id, call_id, _s(c.get("due_date")),
             _s(c.get("amount")), _s(c.get("currency")), _s(c.get("status")),
             _cj(c)),
        )
        n += 1
    return n


def load_capital_events(conn, events) -> int:
    """Record the reconstructed capital-event timeline (one row per
    snapshot-defining event). `events`: iterable of
    (snapshot_at, entity_external_id, event_kind, event_date, description)."""
    n = 0
    for snap, eid, kind, date_str, desc in events:
        conn.execute(
            "INSERT OR REPLACE INTO capital_events "
            "(snapshot_at, entity_external_id, event_kind, event_date, "
            " description, payload) VALUES (?,?,?,?,?,?)",
            (snap, eid, kind, _s(date_str), _s(desc), "{}"))
        n += 1
    return n


def load_documents(conn, snap: int, run_name: str, docs_dir: Path) -> int:
    """Index documents/index.json, content-deduping each PDF on its SHA-256.
    bronze_path is stored relative to the bronze root."""
    if not (docs_dir / "index.json").is_file():
        return 0
    idx = _read_json(docs_dir / "index.json")
    rows = idx.get("results") if isinstance(idx, dict) else None
    n = 0
    for row in rows or []:
        if not isinstance(row, dict):
            continue
        doc_id = row.get("id")
        pdf = docs_dir / f"doc_{doc_id}.pdf"
        if not pdf.is_file():
            # Indexed but not downloaded (e.g. dry-run, or a fetch failure) —
            # skip; a later real run will pick it up.
            continue
        data = pdf.read_bytes()
        sha = hashlib.sha256(data).hexdigest()
        bronze_path = f"{run_name}/documents/{pdf.name}"
        existing = conn.execute(
            "SELECT first_seen_at, last_seen_at FROM documents "
            "WHERE content_sha256=?", (sha,)).fetchone()
        if existing is None:
            # New content. Supersede any stale row for this doc_id (e.g. a
            # superseded capture under the same doc id) —
            # doc_id is unique, and the latest fetch is authoritative.
            conn.execute("DELETE FROM documents WHERE doc_id=?", (doc_id,))
            conn.execute(
                "INSERT INTO documents "
                "(content_sha256, doc_id, uuid, document_name, document_type, "
                " document_date, fund_id, fund_name, firm_id, firm_name, "
                " capital_account_name, stakeholder_name, file_size, "
                " bronze_path, first_seen_at, last_seen_at, payload) "
                "VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
                (sha, doc_id, _s(row.get("uuid")), _s(row.get("document_name")),
                 _s(row.get("document_type")), _s(row.get("document_date")),
                 _s(row.get("fund_id")), _s(row.get("fund_name")),
                 _s(row.get("firm_id")), _s(row.get("firm_name")),
                 _s(row.get("capital_account_name")),
                 _s(row.get("stakeholder_name")), len(data), bronze_path,
                 snap, snap, _cj(row)),
            )
            n += 1
        else:
            first, last = existing
            conn.execute(
                "UPDATE documents SET first_seen_at=?, last_seen_at=? "
                "WHERE content_sha256=?",
                (min(first, snap), max(last, snap), sha))
    return n


def _pdf_text(pdf: Path) -> str | None:
    """A PDF's `pdftotext -layout` text, or None (logged) when it cannot be
    extracted."""
    try:
        return pdftotext.layout_text(pdf, timeout=30)
    except pdftotext.ExtractionError as exc:
        log.warning("pdftotext failed on %s: %s", pdf.name, exc)
        return None


def _statement_nav_from_text(text: str) -> str | None:
    """The LP's ending capital balance (= NAV) from a capital-account
    statement's pdftotext output, as a digit string; None if the layout
    doesn't match."""
    m = re.search(r"Ending balance\s*\$?\s*\(?([\d,]+)\)?", text)
    return m.group(1).replace(",", "") if m else None


def _itd_figure(text: str, label: str) -> str | None:
    """The INCEPTION-TO-DATE figure on a capital-account statement line, as
    printed: `(1,234)`, `1,234` or the nil dash `—`. Each line carries three
    columns (statement period / year to date / inception to date); the
    inception-to-date one is the line's last token. None when no line
    starts with `label`. Balance-sheet lines (contributions receivable /
    received in advance) are skipped."""
    for line in text.splitlines():
        if not re.match(rf"\s*{re.escape(label)}\s", line):
            continue
        if "receivable" in line or "advance" in line:
            continue
        toks = re.findall(r"\(?[\d,]+(?:\.\d+)?\)?|—",
                          line[line.find(label) + len(label):])
        if toks:
            return toks[-1]
    return None


def _itd_decimal(raw: str | None) -> str | None:
    """A printed statement figure as a signed decimal string: parentheses
    are negative, the nil dash is zero."""
    if raw is None:
        return None
    if raw == "—":
        return "0"
    digits = raw.strip("()").replace(",", "")
    return "-" + digits if raw.startswith("(") else digits


def _statement_flows_from_text(text: str) -> tuple[float | None, float | None]:
    """The partner's INCEPTION-TO-DATE capital contributions + distributions
    (USD, as magnitudes) from a capital-account statement's pdftotext output.
    The inception-to-date column reads cleanly as the line's last figure and
    is monotonic, whereas the period columns mis-align under pdftotext when
    '—' placeholders are present. load_cash_flows differences consecutive
    statements into per-period flows. Returns (contributions_itd,
    distributions_itd); a component is None if its line is absent. Split from
    the PDF call for testability."""
    def magnitude(label: str) -> float | None:
        dec = _itd_decimal(_itd_figure(text, label))
        return None if dec is None else abs(float(dec))

    return magnitude("Capital contributions"), magnitude("Capital distributions")


# Statement lines carried onto the statement's fund_metrics row, by column:
# each line's inception-to-date figure, signed as the statement prints it.
_STATEMENT_ITD_LINES = {
    "management_fees": "Management fees",
    "net_operating_income": "Net operating income (loss)",
    "realized_gain": "Net realized gain (loss)",
    "unrealized_gain": "Net unrealized gain (loss)",
    "carried_interest": "Carried interest accrued",
}


def _statement_from_text(text: str) -> dict:
    """Everything a capital-account statement's pdftotext output states for
    its fund_metrics row: `net_asset_value` (the ending balance, a digit
    string), `capital_contributed` (inception-to-date contributions, "%.2f")
    and each _STATEMENT_ITD_LINES column. A value is None when its line is
    absent."""
    contributed = _statement_flows_from_text(text)[0]
    out = {
        "net_asset_value": _statement_nav_from_text(text),
        "capital_contributed": (None if contributed is None
                                else f"{contributed:.2f}"),
    }
    for col, label in _STATEMENT_ITD_LINES.items():
        out[col] = _itd_decimal(_itd_figure(text, label))
    return out


def _parse_statement(pdf: Path) -> dict | None:
    """_statement_from_text over one capital-account statement PDF (via
    pdftotext -layout); None when the PDF cannot be read."""
    text = _pdf_text(pdf)
    return None if text is None else _statement_from_text(text)


def _parse_statement_flows(pdf: Path) -> tuple[float | None, float | None]:
    """Inception-to-date contributions + distributions from a capital-account
    statement PDF (via pdftotext -layout); see _statement_flows_from_text."""
    text = _pdf_text(pdf)
    return (None, None) if text is None else _statement_flows_from_text(text)


# A notice's fields, as pdftotext -layout renders them: a label at the left
# and its value flushed right on the same line. The two document kinds share
# the shape and differ in three labels, which is the whole of the difference.
_NOTICE_KINDS = {
    "Capital Call Notice": {
        "kind": "capital_call",
        "date": "Due date",
        "amount": "Contribution",
        "cumulative": "Called capital (post call)",
    },
    "Distribution Notice": {
        "kind": "distribution",
        "date": "Distribution date",
        "amount": "Distribution",
        "cumulative": "Distributed capital to date (post distribution)",
    },
}


def _notice_field(text: str, label: str) -> str | None:
    """The value printed against `label`, or None. Anchored at the start of a
    line so `Distribution` does not also read `Distribution date`, and so the
    `Amount due to ...` restatement below it is never mistaken for the figure
    itself."""
    m = re.search(rf"(?m)^\s*{re.escape(label)}\s{{2,}}(.+?)\s*$", text)
    return m.group(1).strip() if m else None


def _notice_money(raw: str | None) -> float | None:
    if not raw:
        return None
    try:
        return float(raw.replace("$", "").replace(",", "").strip())
    except ValueError:
        return None


def parse_notice_text(text: str) -> dict | None:
    """One capital-call or distribution notice -> its dated amount.

    The notices are why this exists: a capital-account statement reports
    inception-to-date figures, so differencing consecutive ones can only place
    a call in the PERIOD it appeared in, and every call ends up dated at the
    period end that follows it — up to a full statement period after the
    money actually moved. A notice states the date the money was really due
    and the amount to the cent.

    `cumulative` is the fund's own running total AFTER this event. On the
    earliest notice it is what makes the pre-coverage lump computable: called
    capital post-call, less this call, is everything called before Carta
    shared anything.

    Returns None for a document that is neither notice — the caller passes
    every PDF it has.
    """
    for header, spec in _NOTICE_KINDS.items():
        if header not in text:
            continue
        when = _notice_field(text, spec["date"])
        amount = _notice_money(_notice_field(text, spec["amount"]))
        if not when or amount is None:
            log.warning("notice: %s missing its date or amount; skipped", header)
            return None
        try:
            day = dt.datetime.strptime(when, "%B %d, %Y").date()
        except ValueError:
            log.warning("notice: unparseable %s %r; skipped", spec["date"], when)
            return None
        issued = _notice_field(text, "Date of notice")
        try:
            issued = dt.datetime.strptime(issued, "%B %d, %Y").date().strftime("%m/%d/%Y")
        except (TypeError, ValueError):
            issued = None
        return {
            "kind": spec["kind"],
            "date": day.strftime("%m/%d/%Y"),
            "issued": issued,
            "amount": amount,
            "cumulative": _notice_money(_notice_field(text, spec["cumulative"])),
        }
    return None


def _parse_notice(pdf: Path) -> dict | None:
    """parse_notice_text over one PDF; split for testability."""
    text = _pdf_text(pdf)
    return None if text is None else parse_notice_text(text)


def _period_deltas(statements) -> list[tuple]:
    """Difference inception-to-date statement figures into per-period flows.
    `statements` is an iterable of (date 'MM/DD/YYYY', doc_id, contributions_itd,
    distributions_itd). Sorted by date; each positive jump in the cumulative
    becomes a per-period flow. The first statement's value lumps anything before
    the earliest available statement, so the running total reconciles to the
    fund's contributed-capital basis. Yields (doc_id, date, kind, amount)."""
    out: list[tuple] = []
    prev_c = prev_d = 0.0
    for date, docid, contrib, dist in sorted(
            statements, key=lambda s: _flow_sort_key(s[0])):
        if contrib is not None and contrib > prev_c:
            out.append((docid, date, "capital_call", contrib - prev_c))
            prev_c = contrib
        if dist is not None and dist > prev_d:
            out.append((docid, date, "distribution", dist - prev_d))
            prev_d = dist
    return out


def load_statement_nav(conn, docs_dir: Path, fund_eid) -> int:
    """Parse the fund's capital-account-statement PDFs into a quarterly NAV
    time series — the history the structured partner-metrics doesn't carry.
    One fund_metrics delta per statement date, carrying the NAV, the
    inception-to-date capital contributed the same statement states (so the
    row has a book value as well as a value), and its inception-to-date fees,
    operating income, gains and carry.

    A structured partner-metrics row loaded at the same date is richer and
    keeps its own figures; the statement adds only the lines partner-metrics
    lacks (_STATEMENT_ITD_LINES) to it."""
    idx = _read_json(docs_dir / "index.json")
    rows = idx.get("results") if isinstance(idx, dict) else None
    itd_cols = list(_STATEMENT_ITD_LINES)
    n = 0
    for row in rows or []:
        if "apital account" not in (row.get("document_type") or ""):
            continue
        pdf = docs_dir / f"doc_{row.get('id')}.pdf"
        if not pdf.is_file():
            continue
        st = _parse_statement(pdf)
        snap = _date_ts(row.get("document_date"))
        if st is None or st["net_asset_value"] is None or snap is None:
            continue
        nav, contributed = st["net_asset_value"], st["capital_contributed"]
        conn.execute(
            "INSERT INTO fund_metrics "
            "(snapshot_at, entity_external_id, currency, net_asset_value, "
            " capital_contributed, sharing_date, "
            f" {', '.join(itd_cols)}, payload) "
            f"VALUES (?,?,?,?,?,?,{','.join('?' * len(itd_cols))},?) "
            "ON CONFLICT (snapshot_at, entity_external_id) DO UPDATE SET "
            + ", ".join(f"{c} = excluded.{c}" for c in itd_cols),
            (snap, fund_eid, "USD", nav, contributed,
             _s(row.get("document_date")), *(st[c] for c in itd_cols),
             _cj({"source": "capital_account_statement",
                  "document_id": row.get("id"), "net_asset_value": nav,
                  "capital_contributed": contributed})))
        n += 1
    return n


# k1_capital_accounts money columns, in k1.parse_face_page's keys.
_K1_COLUMNS = ("beginning_capital", "contributions", "net_income",
               "other_change", "distributions", "cash_distributions",
               "property_distributions", "ending_capital", "short_term_gain",
               "long_term_gain")


def load_k1_capital_accounts(conn, docs_dir: Path) -> int:
    """Parse each tax document's federal Schedule K-1 face page into a
    k1_capital_accounts row (k1.py), keyed on the PDF's sha256 like
    `documents`. The fund it belongs to is the index row's `fund_id`, which
    is the fund entity's id. The tax year is the form's own, else the
    index's. A document already parsed under its sha256 is skipped; a
    re-issue under the same document id replaces the stale row. A tax
    document with no federal face page (a 1042-S) yields no row, with a
    warning when the index types it as a K-1. Returns the number of rows
    written."""
    idx = _read_json(docs_dir / "index.json")
    rows = idx.get("results") if isinstance(idx, dict) else None
    n = 0
    for row in rows or []:
        if not isinstance(row, dict):
            continue
        if "tax" not in (row.get("document_type") or "").lower():
            continue
        pdf = docs_dir / f"doc_{row.get('id')}.pdf"
        if not pdf.is_file():
            continue
        sha = hashlib.sha256(pdf.read_bytes()).hexdigest()
        if conn.execute("SELECT 1 FROM k1_capital_accounts "
                        "WHERE content_sha256 = ?", (sha,)).fetchone():
            continue
        try:
            parsed = k1.parse_bbox(k1.bbox_xhtml(pdf, timeout=60))
        except pdftotext.ExtractionError as exc:
            log.warning("pdftotext -bbox failed on %s: %s", pdf.name, exc)
            continue
        if parsed is None:
            if "k-1" in row["document_type"].lower():
                log.warning("%s is typed %r but has no federal K-1 face page; "
                            "no k1_capital_accounts row", pdf.name,
                            row["document_type"])
            continue
        year = parsed["tax_year"]
        if year is None and str(row.get("tax_year") or "").isdigit():
            year = int(row["tax_year"])
        fund = row.get("fund_id")
        conn.execute("DELETE FROM k1_capital_accounts WHERE doc_id = ?",
                     (row.get("id"),))
        conn.execute(
            "INSERT INTO k1_capital_accounts "
            "(content_sha256, doc_id, entity_external_id, tax_year, "
            f" {', '.join(_K1_COLUMNS)}, payload) "
            f"VALUES (?,?,?,?,{','.join('?' * len(_K1_COLUMNS))},?)",
            (sha, row.get("id"),
             int(fund) if str(fund or "").isdigit() else None, year,
             *(parsed[c] for c in _K1_COLUMNS), _cj(parsed["printed"])))
        n += 1
    return n


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


def _fund_cash_flows(conn, eid, docs_dir: Path, snap: int,
                     bronze_root: Path) -> int:
    """Fund cash flows: `capital_call` (deposit+contribution in gold) and
    `distribution` (distribution+withdrawal).

    Two sources, and the better one wins per kind. A NOTICE states the day the
    money was due and the amount to the cent, so where the fund has issued
    notices they are the ledger. A capital-account STATEMENT reports only
    inception-to-date figures, so differencing consecutive ones can place a
    flow no more precisely than the period it fell in — every call ends up
    dated at the period end that follows it, up to a full period after the
    money moved — and it is the fallback for a fund that shares no notices.

    The two are reconciled rather than mixed: the statements' inception-to-date
    total is what the fund says was called in all, the notices account for the
    part Carta shares, and the difference is emitted as ONE residue row —
    everything called before Carta shared anything, which is not itemised
    anywhere. The residue is dated at the earliest notice, the last day on
    which it is KNOWN to have been fully called; it is a bound, not an event,
    and its description says so. Without notices the first statement's figure
    is the same kind of lump.

    A supplied `<eid>-transactions.csv` itemises that lump from the holder's
    own records: each `capital_call` / `distribution` row dated on or before
    the lump is emitted at its own date, and the lump keeps only what the
    rows do not account for. A row dated after the lump cannot be part of it
    and is skipped with a warning. Where the fund reports no lump for a kind
    at all, the supplied rows are that kind's whole ledger.
    """
    idx = _read_json(docs_dir / "index.json")
    rows = idx.get("results") if isinstance(idx, dict) else None
    supplied = []
    path = bronze_root / f"{eid}-transactions.csv"
    for i, tx in enumerate(_read_transactions_csv(path)):
        if tx["kind"] not in ("capital_call", "distribution") or not tx["amount"]:
            log.warning("fund %s: supplied row %d is not a capital_call or "
                        "distribution with an amount; skipped", eid, i)
            continue
        supplied.append({**tx, "row": i})
    stmts, notices = [], []
    for row in rows or []:
        dtype = row.get("document_type") or ""
        pdf = docs_dir / f"doc_{row.get('id')}.pdf"
        if not pdf.is_file():
            continue
        if "apital account" in dtype:
            date = _s(row.get("document_date"))
            if not date:
                continue
            contrib, dist = _parse_statement_flows(pdf)
            stmts.append((date, row.get("id"), contrib, dist))
        elif "apital call" in dtype or "istribution" in dtype:
            parsed = _parse_notice(pdf)
            if parsed:
                parsed["docid"] = row.get("id")
                notices.append(parsed)

    # The fund ledger is DERIVED WHOLLY from the documents this run holds, so
    # it is rebuilt rather than accumulated: a run that can read the notices
    # emits dated calls where an earlier run — whose copies of those notices
    # were url envelopes, unreadable — emitted statement-differenced ones under
    # different ids. Left to accumulate, both shapes survive and the fund's
    # called capital doubles. Runs load oldest-first, so the newest view wins,
    # and a run only ever holds MORE documents than its predecessor.
    conn.execute("DELETE FROM cash_flows WHERE entity_external_id = ? "
                 "  AND kind IN ('capital_call', 'distribution')", (eid,))

    deltas = _period_deltas(stmts)
    from_stmts = {"capital_call": 0.0, "distribution": 0.0}
    for _docid, _date, kind, amount in deltas:
        from_stmts[kind] = from_stmts.get(kind, 0.0) + amount
    noticed = {"capital_call": 0.0, "distribution": 0.0}
    for nt in notices:
        noticed[nt["kind"]] = noticed.get(nt["kind"], 0.0) + nt["amount"]

    n = 0
    for kind in ("capital_call", "distribution"):
        prefix = "call" if kind == "capital_call" else "dist"
        desc = "fund capital call" if kind == "capital_call" else "fund distribution"
        mine = sorted((x for x in notices if x["kind"] == kind),
                      key=lambda x: _flow_sort_key(x["date"]))
        mine_supplied = [tx for tx in supplied if tx["kind"] == kind]
        if not mine:
            # No notices for this kind: the statements are all there is, and
            # the first one's figure lumps everything before it.
            periods = [(docid, date, amount) for docid, date, k, amount in deltas
                       if k == kind]
            if periods:
                docid, date, amount = periods[0]
                itemised, rest = _itemise(eid, kind, amount, date, mine_supplied)
                periods[0] = (docid, date, rest)
            else:
                itemised = mine_supplied
            n += _insert_supplied(conn, eid, snap, prefix, desc, itemised)
            for docid, date, amount in periods:
                if amount > 0.01:
                    n += _insert_cash_flow(conn, f"{prefix}:{eid}:{docid}", eid,
                                           snap, kind, date, amount, None, None, desc)
            continue
        for nt in mine:
            n += _insert_cash_flow(conn, f"{prefix}:{eid}:notice:{nt['docid']}",
                                   eid, snap, kind, nt["date"], nt["amount"],
                                   None, None, desc)
        residue = round(from_stmts.get(kind, 0.0) - noticed[kind], 2)
        stated = _residue_from_cumulative(mine[0])
        if stated is not None:
            # The notice's own running total is the fund's statement of what
            # preceded it, so it wins. A statement-derived figure BELOW it is
            # the ordinary case of a notice issued since the last statement —
            # not a disagreement, and not worth saying. Above it means there
            # is called capital that neither the notices nor the residue
            # account for, which is.
            if residue - stated > 0.01:
                log.warning("%s: the statements imply %.2f called before the "
                            "earliest notice, which itself says %.2f; %.2f is "
                            "accounted for by neither",
                            kind, residue, stated, residue - stated)
            residue = stated
        residue_date = mine[0].get("issued") or mine[0]["date"]
        itemised, residue = _itemise(eid, kind, residue, residue_date, mine_supplied)
        n += _insert_supplied(conn, eid, snap, prefix, desc, itemised)
        if residue > 0.01:
            # Dated at the notice, not at its due date: the notice is what
            # STATES the residue was already called, and dating it on the due
            # date would stack it on top of that call — recreating on one day
            # the very lump this exists to take apart.
            n += _insert_cash_flow(
                conn, f"{prefix}:{eid}:pre:{mine[0]['docid']}", eid, snap, kind,
                residue_date, residue, None, None,
                f"{desc} before Carta's coverage (not itemised; dated at the "
                "earliest notice, the last day it is known to have been called)")
    return n


def _itemise(eid, kind: str, lump: float, lump_date: str,
             supplied: list[dict]) -> tuple[list[dict], float]:
    """Split a lump the fund reports without dates into the supplied rows that
    itemise it and what they leave. Rows dated after the lump are not part of
    it and are dropped; rows that together exceed it are kept — the holder's
    records are dated, the lump is not — and the excess is logged."""
    bound = _date_ts(lump_date)
    rows, rest = [], lump
    for tx in supplied:
        at = _date_ts(tx["flow_date"])
        if at is None or bound is None or at > bound:
            log.warning("fund %s: supplied %s of %s is not dated on or before "
                        "the lump it would itemise (%s); skipped",
                        eid, kind, tx["flow_date"], lump_date)
            continue
        rows.append(tx)
        rest -= tx["amount"]
    rest = round(rest, 2)
    if rest < -0.01:
        log.warning("fund %s: the supplied %s rows exceed what the fund reports "
                    "before %s by %.2f", eid, kind, lump_date, -rest)
    return rows, max(rest, 0.0)


def _insert_supplied(conn, eid, snap: int, prefix: str, desc: str,
                     rows: list[dict]) -> int:
    """Write the supplied rows that itemise a fund lump, keyed by file row."""
    n = 0
    for tx in rows:
        n += _insert_cash_flow(conn, f"{prefix}:{eid}:supplied:{tx['row']}", eid,
                               snap, tx["kind"], tx["flow_date"], tx["amount"],
                               None, None, tx["description"] or desc)
    return n


def _flow_sort_key(date: str) -> tuple:
    """`MM/DD/YYYY` -> a sortable tuple, the same reading _period_deltas uses."""
    return (date[6:10], date[0:2], date[3:5])


def _residue_from_cumulative(notice: dict) -> float | None:
    """What the earliest notice says was already called before it: the fund's
    own running total after the event, less the event. Stated by the fund
    rather than derived from a difference of statement figures, so it is the
    better of the two when they disagree."""
    if notice.get("cumulative") is None:
        return None
    return round(notice["cumulative"] - notice["amount"], 2)


def load_cash_flows(conn, run_dir: Path, snap: int) -> int:
    """Build the dated cash-flow ledger (migration 0003): a row per cash event
    — stock exercises / exit from the cap-table certs + cancellation, and fund
    capital calls / distributions from the capital-account statements. Amounts
    are positive magnitudes; the gold adapter projects each as a balanced
    double-entry pair on the custody account (DESIGN.md §6)."""
    entities_dir = run_dir / "entities"
    if not entities_dir.is_dir():
        return 0
    docs_dir = run_dir / "documents"
    bronze_root = run_dir.parent  # side-loaded `<account_id>-*.csv` live here
    n = 0
    for edir in sorted(entities_dir.iterdir()):
        if not edir.is_dir():
            continue
        meta = _read_json(edir / "meta.json")
        if not isinstance(meta, dict):
            continue
        eid = meta.get("corporation_id")
        if meta.get("is_fund_investment"):
            n += _fund_cash_flows(conn, eid, docs_dir, snap, bronze_root)
        else:
            n += _captable_cash_flows(conn, eid, edir, snap, bronze_root)
    return n


# ---- per-run driver ---------------------------------------------------------

def load_run(conn, run_dir: Path) -> bool:
    """Load one bronze dump as an event-dated snapshot series (DESIGN.md §5).
    A single download is reconstructed into a snapshot per capital event /
    statement date: each cap-table entity gets a 'held' snapshot at
    held_since and (if exited) a 'cancelled' snapshot at the acquisition
    date; each fund entity gets a snapshot at the LP statement's effective
    (sharing) date. dump_runs is keyed at the download time for idempotency
    + provenance only. Returns True if loaded, False if skipped."""
    manifest = _read_json(run_dir / "run.json")
    if not isinstance(manifest, dict):
        log.warning("skipping %s: no readable run.json", run_dir.name)
        return False
    # run.json carries a status lifecycle: download stamps "in-progress"
    # at run-dir creation and atomically overwrites it with "complete"
    # when the walk finishes. Skip anything that is not "complete" (a
    # crashed walk left "in-progress") so a partial capture never reaches
    # silver as a snapshot. A statusless manifest predates the field and
    # is loadable: such a dump only ever got a run.json at the end, so its
    # presence means the walk finished.
    status = manifest.get("status")
    if status is not None and status != "complete":
        log.info("skipping %s: run.json status=%r (partial/crashed dump)",
                 run_dir.name, status)
        return False
    if manifest.get("dry_run"):
        log.info("skipping %s: dry-run dump (bootstrap only, no holdings)",
                 run_dir.name)
        return False
    dump_snap = bronze.parse_run_ts(run_dir.name)
    ind_id = _s(manifest.get("individual_id"))
    firm_id = _s(manifest.get("firm_id"))

    conn.execute("BEGIN")
    try:
        conn.execute(
            "INSERT OR REPLACE INTO dump_runs "
            "(snapshot_at, silver_schema_version, run_dir, individual_id, "
            " firm_id, dry_run, entities_total, documents_total, "
            " errors_total, payload) VALUES (?,?,?,?,?,?,?,?,?,?)",
            (dump_snap, silver.current_schema_version(conn), run_dir.name,
             ind_id, firm_id, _b(manifest.get("dry_run")) or 0,
             len(manifest.get("entities") or []),
             (manifest.get("documents") or {}).get("indexed", 0),
             len(manifest.get("errors") or []), _cj(manifest)),
        )

        n_sec = n_vest = n_fund = n_call = n_evt = n_navh = n_cf = 0
        fund_eid = None
        entities_dir = run_dir / "entities"
        if entities_dir.is_dir():
            for edir in sorted(entities_dir.iterdir()):
                if not edir.is_dir():
                    continue
                meta = _read_json(edir / "meta.json")
                if not isinstance(meta, dict):
                    continue
                hd = _read_json(edir / "holdings-dashboard.json")
                hd = hd if isinstance(hd, dict) else {}
                eid = meta.get("corporation_id")

                if meta.get("is_fund_investment"):
                    # Fund LP: snapshot at the capital-account statement's
                    # effective (sharing) date — its NAV is a quarter-end
                    # figure, not a download-time one. The full quarterly NAV
                    # history comes from the statement PDFs (load_statement_nav,
                    # after this loop).
                    fund_eid = eid
                    sharing = _fund_sharing_date(edir)
                    snap = _date_ts(sharing) or dump_snap
                    load_entity(conn, snap, ind_id, firm_id, edir)
                    n_fund += load_fund_metrics(conn, snap, eid, edir)
                    n_call += load_cap_calls(conn, snap, eid, edir)
                    n_vest += load_vesting(conn, dump_snap, eid, edir)
                    n_evt += load_capital_events(conn, [
                        (snap, eid, "statement", sharing,
                         "capital-account statement / NAV")])
                else:
                    # Cap-table: reconstruct the held -> cancelled lifecycle.
                    cancel_s = _entity_canceled_date(edir)
                    cancel_ts = _date_ts(cancel_s)
                    exercises = _read_exercises(edir)
                    matched = _match_exercises(edir, exercises)
                    override = _read_valuation_csv(
                        run_dir.parent / f"{eid}-valuations.csv")
                    if override:
                        # Side-loaded `<account_id>-valuations.csv` is the single
                        # source of truth: each cert held from its issue date and
                        # re-valued at every FMV step, so the share count and
                        # per-share price both move over time (DESIGN.md §5.1).
                        first_ts = override[0][0]
                        load_entity(conn, first_ts, ind_id, firm_id, edir)
                        n_sec += load_securities_valued(
                            conn, eid, edir, fmv_timeline=override,
                            cancel_ts=cancel_ts, exercises=matched)
                        events = [(first_ts, eid, "acquired", None,
                                   "first held (valuation override applies)")]
                        events += [(t, eid, "price_change", None,
                                    "fair-market-value step")
                                   for t, _ in override[1:]]
                        if cancel_ts is not None and cancel_ts > first_ts:
                            events.append((cancel_ts, eid, "disposition",
                                           cancel_s,
                                           "acquisition: securities cancelled"))
                        n_evt += load_capital_events(conn, events)
                    else:
                        # No override: value held shares at the FMV at the last
                        # exercise (after-exercise basis), falling back to the
                        # last strike when no exercise detail was captured.
                        val_price = (_last_exercise_fmv(exercises)
                                     or _last_exercise_price(edir))
                        held_ts = _date_ts(hd.get("held_since")) or dump_snap
                        load_entity(conn, held_ts, ind_id, firm_id, edir)
                        n_sec += load_securities(conn, held_ts, eid, edir,
                                                 held=True, val_price=val_price,
                                                 exercises=matched)
                        events = [(held_ts, eid, "acquired",
                                   _s(hd.get("held_since")), "shares first held")]
                        if cancel_ts is not None and cancel_ts > held_ts:
                            load_entity(conn, cancel_ts, ind_id, firm_id, edir)
                            load_securities(conn, cancel_ts, eid, edir,
                                            held=False, val_price=val_price,
                                            exercises=matched)
                            events.append((cancel_ts, eid, "disposition",
                                           cancel_s,
                                           "acquisition: securities cancelled"))
                        n_evt += load_capital_events(conn, events)
                    n_vest += load_vesting(conn, dump_snap, eid, edir)

        n_doc = load_documents(conn, dump_snap, run_dir.name,
                               run_dir / "documents")
        n_k1 = load_k1_capital_accounts(conn, run_dir / "documents")
        if fund_eid is not None:
            n_navh = load_statement_nav(conn, run_dir / "documents", fund_eid)
        n_cf = load_cash_flows(conn, run_dir, dump_snap)
        conn.execute("COMMIT")
    except Exception:
        conn.execute("ROLLBACK")
        raise

    log.info("loaded %s: %d securities, %d event(s), %d grant vesting, "
             "%d fund-metric (+%d NAV-history), %d cap-call, %d cash-flow(s), "
             "%d new doc(s), %d new K-1(s)",
             run_dir.name, n_sec, n_evt, n_vest, n_fund, n_navh, n_call, n_cf,
             n_doc, n_k1)
    return True


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument(
        "--bronze-dir", type=Path, default=DEFAULT_BRONZE_DIR,
        help="Bronze root to ingest from. Default: %(default)s.",
    )
    p.add_argument(
        "--silver-db", type=Path, default=DEFAULT_SILVER_DB,
        help="Silver SQLite DB path. Default: %(default)s.",
    )
    cli.add_standard_args(p, verb="load")
    return p.parse_args(argv)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)

    # --force = delete the silver DB, then rebuild from all bronze (the
    # fleet-wide meaning). After a reset the DB is empty, so the
    # already-loaded skip below naturally re-ingests every run.
    if args.force:
        silver.reset(args.silver_db)
    conn = silver.open_db(args.silver_db)
    silver.apply_migrations(conn, MIGRATIONS_DIR)
    already = silver.loaded_snapshots(conn)

    n_loaded = 0
    for run_dir in bronze.iter_run_dirs(args.bronze_dir):
        try:
            snap = bronze.parse_run_ts(run_dir.name)
        except ValueError:
            continue
        if snap in already:
            log.debug("skip %s (already loaded)", run_dir.name)
            continue
        if load_run(conn, run_dir):
            n_loaded += 1

    log.info("done: %d bronze run(s) loaded into %s", n_loaded, args.silver_db)
    conn.close()
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
