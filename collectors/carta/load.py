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
  <account_external_id>-valuations.csv
                               -> valuation override (bronze root, optional):
                                  per-date FMV, the single source of truth for
                                  held-share value when present (DESIGN.md §5.1)
  <account_external_id>-transactions.csv
                               -> explicit exit transactions (bronze root,
                                  optional): the final sale + bank/escrow
                                  withdrawals, overriding the $0 exit (§5.2)
  entities/<e>/exercises/grant_*_er_*.xlsx
                               -> FMV at last exercise (fallback held-share value)
  entities/<e>/fund-admin/partner-metrics.json
                               -> fund_metrics (latest LP capital account)
  entities/<e>/fund-cap-calls.json
                               -> cap_calls
  documents/index.json + doc_*.pdf
                               -> documents (content-deduped on sha256);
                                  capital-account statements also parsed
                                  -> fund_metrics quarterly NAV history

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
import subprocess
import sys
import xml.etree.ElementTree as ET
import zipfile
from pathlib import Path

from collectorkit import bronze, cli, silver

log = logging.getLogger("carta.load")

HERE = Path(__file__).resolve().parent
MIGRATIONS_DIR = HERE / "migrations"

DEFAULT_BRONZE_DIR = Path("/data")
DEFAULT_DB = Path("/data/carta.db")

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
    """Parse a Carta exercise-detail xlsx into {date, shares, exercise_price,
    fmv}. The sheet is label/value rows ('Grant exercised on <date>', 'Shares
    exercised', 'Exercise price', 'Fair market value on exercise'); the value
    trails its label cell. xlsx == zip of XML, parsed with the stdlib. None if
    unreadable / no date."""
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
    return {"date": m.group(1), "shares": value_after("Shares exercised"),
            "exercise_price": value_after("Exercise price"),
            "fmv": value_after("Fair market value")}


def _last_exercise_fmv(edir: Path) -> float | None:
    """The fair-market-value at the most recent exercise — the holder's
    after-exercise valuation basis (shares exercised at a strike below FMV
    are worth FMV). Parsed from the exercise-detail
    xlsx; None if none were captured."""
    xdir = edir / "exercises"
    if not xdir.is_dir():
        return None
    best_ts, best_fmv = None, None
    for xl in xdir.glob("*.xlsx"):
        ex = _parse_exercise_xlsx(xl)
        if not ex or ex.get("fmv") is None:
            continue
        ts = _date_ts(ex.get("date"))
        if ts is not None and (best_ts is None or ts > best_ts):
            best_ts, best_fmv = ts, _f(ex["fmv"])
    return best_fmv


def _read_valuation_csv(path: Path) -> list[tuple[int, float]]:
    """Read a side-loaded valuation-override CSV (named
    `<account_id>-valuations.csv` in the bronze root): rows of
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
    """Read a side-loaded transactions CSV (named `<account_id>-transactions.csv`
    in the bronze root): rows of `date,kind,amount,shares,description`, '#' /
    blank lines ignored. `kind` is a canonical transaction kind the gold emits
    1:1 (sell | withdrawal | deposit | buy | contribution); `amount` is a
    positive magnitude (USD). These explicit legs override the auto-derived $0
    exit for the entity (e.g. a sale plus the withdrawals it splits into). Returns the parsed rows in file order; empty if absent."""
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
                     canceled, market_value: float, position_status: str) -> int:
    """Write one `securities` delta row (one position at one snapshot)."""
    conn.execute(
        "INSERT OR REPLACE INTO securities "
        "(snapshot_at, entity_external_id, security_type, "
        " security_external_id, label, issuable_type, stock_type, "
        " status, issue_date, currency, quantity, exercise_price, "
        " cost, exercised, vested, exercisable, has_vesting, "
        " is_canceled, is_expired, is_terminated, is_fully_exercised, "
        " market_value, position_status, fund_name, payload) "
        "VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
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
         _s(row.get("fund_name")), _cj(row)),
    )
    return 1


def load_securities(conn, snap: int, entity_id, edir: Path, *,
                    held: bool, val_price: float | None) -> int:
    """Carta-derived fallback (no valuation override): write the cap-table
    securities at one snapshot. `held=True` is the live state — is_canceled
    forced 0, held shares valued at quantity x val_price (the FMV at the last
    exercise), options / other lines at 0. `held=False` is the exited state:
    is_canceled from the source, market_value 0."""
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
                mv = (qty * val_price) if (sectype == "share" and qty is not None
                                           and val_price is not None) else 0.0
            else:
                canceled, pstatus = _b(row.get("is_canceled")), "exited"
                mv = 0.0
            n += _insert_security(conn, snap, entity_id, sectype, row,
                                  canceled=canceled, market_value=mv,
                                  position_status=pstatus)
    return n


def load_securities_valued(conn, entity_id, edir: Path, *,
                           fmv_timeline: list[tuple[int, float]],
                           cancel_ts: int | None) -> int:
    """Side-loaded valuation override: each share certificate is held from its
    issue date and re-valued at every FMV step in the timeline
    (quantity x FMV-as-of), so the share count *and* the per-share price both
    move correctly over time. Options / other lines carry value 0. A cancelled
    entity exits every line at cancel_ts (dropped from gold positions).
    Returns the number of delta rows written."""
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
                if sectype == "share" and qty is not None:
                    fmv = _fmv_as_of(fmv_timeline, t)
                    mv = qty * fmv if fmv is not None else 0.0
                else:
                    mv = 0.0
                n += _insert_security(conn, t, entity_id, sectype, row,
                                      canceled=0, market_value=mv,
                                      position_status="held")
            if cancel_ts is not None:
                _insert_security(conn, cancel_ts, entity_id, sectype, row,
                                 canceled=_b(row.get("is_canceled")),
                                 market_value=0.0, position_status="exited")
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
    conn.execute(
        "INSERT OR REPLACE INTO fund_metrics "
        "(snapshot_at, entity_external_id, fund_external_id, fund_uuid, "
        " currency, vintage_year, commitment, called_capital, "
        " capital_contributed, capital_contributed_paid, distributions, "
        " net_asset_value, capital_call_liabilities, "
        " prepaid_capital_contribution, sharing_date, payload) "
        "VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
        (snap, entity_id, _s(partner.get("fund_id")),
         _s(partner.get("fund_uuid")), _s(partner.get("fund_currency")),
         m.get("vintage_year"), _s(m.get("commitment")),
         _s(m.get("called_capital")), _s(m.get("capital_contributed")),
         _s(m.get("capital_contributed_paid")), _s(m.get("distributions")),
         _s(m.get("net_asset_value")), _s(m.get("capital_call_liabilities")),
         _s(m.get("prepaid_capital_contribution")), _s(pm.get("sharing_date")),
         _cj(pm)),
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
            # prior broken capture that the document-download fix corrected) —
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


def _parse_statement_nav(pdf: Path) -> str | None:
    """Extract the LP's ending capital balance (= NAV) from a capital-account
    statement PDF, via pdftotext -layout. Returns a digit string, or None if
    the layout doesn't match."""
    try:
        out = subprocess.run(["pdftotext", "-layout", str(pdf), "-"],
                             capture_output=True, text=True, timeout=30)
    except (OSError, subprocess.SubprocessError) as exc:
        log.warning("pdftotext failed on %s: %s", pdf.name, exc)
        return None
    m = re.search(r"Ending balance\s*\$?\s*\(?([\d,]+)\)?", out.stdout)
    return m.group(1).replace(",", "") if m else None


def _statement_flows_from_text(text: str) -> tuple[float | None, float | None]:
    """The partner's INCEPTION-TO-DATE capital contributions + distributions
    (USD) from a capital-account statement's pdftotext output. Each line
    carries three columns (statement-period / year-to-date / inception-to-
    date); we take the inception-to-date (LAST) figure — it reads cleanly as
    the line's final amount and is monotonic, whereas the period columns
    mis-align under pdftotext when '—' placeholders are present.
    load_cash_flows differences consecutive statements into per-period flows.
    Returns (contributions_itd, distributions_itd); a component is None if its
    line is absent. Balance-sheet lines (contributions receivable / received in
    advance) are skipped. Split from the PDF call for testability."""
    def inception_to_date(label: str) -> float | None:
        for line in text.splitlines():
            if not re.match(rf"\s*{label}\s", line):
                continue
            if "receivable" in line or "advance" in line:
                continue
            amts = re.findall(r"\(?[\d,]+\)?", line[line.find(label) + len(label):])
            if amts:
                return float(amts[-1].strip("()").replace(",", ""))
        return None

    return (inception_to_date("Capital contributions"),
            inception_to_date("Capital distributions"))


def _parse_statement_flows(pdf: Path) -> tuple[float | None, float | None]:
    """Inception-to-date contributions + distributions from a capital-account
    statement PDF (via pdftotext -layout); see _statement_flows_from_text."""
    try:
        out = subprocess.run(["pdftotext", "-layout", str(pdf), "-"],
                             capture_output=True, text=True, timeout=30).stdout
    except (OSError, subprocess.SubprocessError) as exc:
        log.warning("pdftotext failed on %s: %s", pdf.name, exc)
        return None, None
    return _statement_flows_from_text(out)


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
            statements, key=lambda s: (s[0][6:10], s[0][0:2], s[0][3:5])):
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
    One fund_metrics delta per statement date; the richer structured row
    (loaded at its sharing_date) is left intact (INSERT OR IGNORE)."""
    idx = _read_json(docs_dir / "index.json")
    rows = idx.get("results") if isinstance(idx, dict) else None
    n = 0
    for row in rows or []:
        if "apital account" not in (row.get("document_type") or ""):
            continue
        pdf = docs_dir / f"doc_{row.get('id')}.pdf"
        if not pdf.is_file():
            continue
        nav = _parse_statement_nav(pdf)
        snap = _date_ts(row.get("document_date"))
        if nav is None or snap is None:
            continue
        conn.execute(
            "INSERT OR IGNORE INTO fund_metrics "
            "(snapshot_at, entity_external_id, currency, net_asset_value, "
            " sharing_date, payload) VALUES (?,?,?,?,?,?)",
            (snap, fund_eid, "USD", nav, _s(row.get("document_date")),
             _cj({"source": "capital_account_statement",
                  "document_id": row.get("id"), "net_asset_value": nav})))
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
    certificate — amount = quantity x strike (the cert cost), price the strike.
    The exit is either a side-loaded `<account_id>-transactions.csv` (explicit
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


def _fund_cash_flows(conn, eid, docs_dir: Path, snap: int) -> int:
    """Fund cash flows from the capital-account statements: `capital_call`
    (deposit+contribution in gold) and `distribution` (distribution+withdrawal).
    Each statement reports inception-to-date figures; sorting by date and
    differencing consecutive statements yields the per-period flow. The first
    statement's value lumps any contributions made before the earliest
    available statement, so the running total reconciles to the fund's
    contributed-capital basis."""
    idx = _read_json(docs_dir / "index.json")
    rows = idx.get("results") if isinstance(idx, dict) else None
    stmts = []
    for row in rows or []:
        if "apital account" not in (row.get("document_type") or ""):
            continue
        pdf = docs_dir / f"doc_{row.get('id')}.pdf"
        date = _s(row.get("document_date"))
        if not pdf.is_file() or not date:
            continue
        contrib, dist = _parse_statement_flows(pdf)
        stmts.append((date, row.get("id"), contrib, dist))
    n = 0
    for docid, date, kind, amount in _period_deltas(stmts):
        prefix = "call" if kind == "capital_call" else "dist"
        desc = "fund capital call" if kind == "capital_call" else "fund distribution"
        n += _insert_cash_flow(conn, f"{prefix}:{eid}:{docid}", eid, snap,
                               kind, date, amount, None, None, desc)
    return n


def load_cash_flows(conn, run_dir: Path, snap: int) -> int:
    """Build the dated cash-flow ledger (migration 0003): a row per cash event
    — stock exercises / exit from the cap-table certs + cancellation, and fund
    capital calls / distributions from the capital-account statements. Amounts
    are positive magnitudes; the gold adapter projects each as a balanced
    double-entry pair on the sentinel funding account (DESIGN.md §6)."""
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
            n += _fund_cash_flows(conn, eid, docs_dir, snap)
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
                            cancel_ts=cancel_ts)
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
                        val_price = (_last_exercise_fmv(edir)
                                     or _last_exercise_price(edir))
                        held_ts = _date_ts(hd.get("held_since")) or dump_snap
                        load_entity(conn, held_ts, ind_id, firm_id, edir)
                        n_sec += load_securities(conn, held_ts, eid, edir,
                                                 held=True, val_price=val_price)
                        events = [(held_ts, eid, "acquired",
                                   _s(hd.get("held_since")), "shares first held")]
                        if cancel_ts is not None and cancel_ts > held_ts:
                            load_entity(conn, cancel_ts, ind_id, firm_id, edir)
                            load_securities(conn, cancel_ts, eid, edir,
                                            held=False, val_price=val_price)
                            events.append((cancel_ts, eid, "disposition",
                                           cancel_s,
                                           "acquisition: securities cancelled"))
                        n_evt += load_capital_events(conn, events)
                    n_vest += load_vesting(conn, dump_snap, eid, edir)

        n_doc = load_documents(conn, dump_snap, run_dir.name,
                               run_dir / "documents")
        if fund_eid is not None:
            n_navh = load_statement_nav(conn, run_dir / "documents", fund_eid)
        n_cf = load_cash_flows(conn, run_dir, dump_snap)
        conn.execute("COMMIT")
    except Exception:
        conn.execute("ROLLBACK")
        raise

    log.info("loaded %s: %d securities, %d event(s), %d grant vesting, "
             "%d fund-metric (+%d NAV-history), %d cap-call, %d cash-flow(s), "
             "%d new doc(s)",
             run_dir.name, n_sec, n_evt, n_vest, n_fund, n_navh, n_call, n_cf,
             n_doc)
    return True


def parse_args(argv: list[str]) -> argparse.Namespace:
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument(
        "--dest", type=Path, default=DEFAULT_BRONZE_DIR,
        help="Bronze root to ingest from. Default: %(default)s.",
    )
    p.add_argument(
        "--db", type=Path, default=DEFAULT_DB,
        help="Silver SQLite DB path. Default: %(default)s.",
    )
    p.add_argument(
        "--force", action="store_true",
        help="Re-load snapshots already recorded in dump_runs.",
    )
    cli.add_common_args(p)
    return p.parse_args(argv)


def main(argv: list[str]) -> int:
    args = parse_args(argv)
    cli.configure_logging(args.verbose)

    conn = silver.open_db(args.db)
    silver.apply_migrations(conn, MIGRATIONS_DIR)
    already = silver.loaded_snapshots(conn) if not args.force else set()

    n_loaded = 0
    for run_dir in bronze.iter_run_dirs(args.dest):
        try:
            snap = bronze.parse_run_ts(run_dir.name)
        except ValueError:
            continue
        if snap in already:
            log.debug("skip %s (already loaded)", run_dir.name)
            continue
        if load_run(conn, run_dir):
            n_loaded += 1

    log.info("done: %d bronze run(s) loaded into %s", n_loaded, args.db)
    conn.close()
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
