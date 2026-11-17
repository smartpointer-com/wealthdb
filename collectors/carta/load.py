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
  entities/<e>/<sectype>.json  -> securities ({rows} of each security-type file);
                                  shares + convertibles also -> cash_flows
                                  (exercises, convertible purchases, the exit)
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
                               -> documents (content-deduped on sha256).
                                  A fund reads the rows whose `fund_id`
                                  names it: capital-account statements ->
                                  its fund_metrics quarterly NAV history;
                                  statements + notices -> its cash_flows.
                                  K-1s -> k1_capital_accounts
                                  (collectorkit.k1)

SQLite + JSON1 (the repo default — no DuckDB need here; this is shape
transformation, not a window-function replay). collectorkit.loader runs it:
run discovery, one transaction per run, the dump_runs row and --force.
"""
from __future__ import annotations

import argparse
import logging
import sys
from pathlib import Path

from collectorkit import loader

from captable import (_captable_cash_flows, _entity_canceled_date,
                      _read_valuation_csv, load_securities,
                      load_securities_valued, load_vesting)
from common import _b, _cj, _date_ts, _f, _read_json, _s
from exercises import (_last_exercise_fmv, _last_exercise_price,
                       _match_exercises, _read_exercises)
from fund_documents import (_fund_cash_flows, _warn_unowned_fund_documents,
                            load_documents, load_k1_capital_accounts,
                            load_statement_nav, sweep_extract_cache)

log = logging.getLogger("carta.load")

HERE = Path(__file__).resolve().parent
MIGRATIONS_DIR = HERE / "migrations"

DEFAULT_BRONZE_DIR = Path("/data")
DEFAULT_SILVER_DB = Path("/data/carta.db")


def _fund_sharing_date(edir: Path) -> str | None:
    """The LP capital account's effective / sharing date (partner-metrics)."""
    body = _read_json(edir / "fund-admin" / "partner-metrics.json")
    if isinstance(body, list) and body and isinstance(body[0], dict):
        return body[0].get("sharing_date")
    return None


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


def load_cash_flows(conn, run_dir: Path, snap: int,
                    entities: list[tuple]) -> int:
    """Build the dated cash-flow ledger (migration 0003): a row per cash event
    — stock exercises / exit from the cap-table certs + cancellation, and fund
    capital calls / distributions from each fund's notices and
    capital-account statements. `entities` is the run's (entity id, entity
    dir, is a fund) in load_run's order. Amounts are positive magnitudes; the
    gold adapter projects each as a balanced double-entry pair on the
    custody account (DESIGN.md §6)."""
    docs_dir = run_dir / "documents"
    bronze_root = run_dir.parent  # side-loaded `<entity_external_id>-*.csv` live here
    n = 0
    for eid, edir, is_fund in entities:
        if is_fund:
            n += _fund_cash_flows(conn, eid, docs_dir, snap, bronze_root)
        else:
            n += _captable_cash_flows(conn, eid, edir, snap, bronze_root)
    return n


# ---- per-run driver ---------------------------------------------------------

# The per-run counts, in load_run's order, as the log names them.
RUN_COUNTS = ("securities", "event(s)", "grant vesting", "fund-metric",
              "NAV-history", "cap-call", "cash-flow(s)", "new doc(s)",
              "new K-1(s)")


def run_columns(run: loader.Run) -> dict:
    """The run's `dump_runs` row: keyed at the download time for idempotency
    and provenance, with the manifest's ids and counts."""
    manifest = run.manifest
    return {"snapshot_at": run.snapshot_at,
            "silver_schema_version": run.version,
            "run_dir": run.dir.name,
            "individual_id": _s(manifest.get("individual_id")),
            "firm_id": _s(manifest.get("firm_id")),
            "dry_run": _b(manifest.get("dry_run")) or 0,
            "entities_total": len(manifest.get("entities") or []),
            "documents_total": (manifest.get("documents") or {}).get("indexed", 0),
            "errors_total": len(manifest.get("errors") or []),
            "payload": _cj(manifest)}


def load_run(conn, run: loader.Run) -> tuple[int, ...]:
    """Load one bronze dump as an event-dated snapshot series (DESIGN.md §5).
    A single download is reconstructed into a snapshot per capital event /
    statement date: each cap-table entity gets a 'held' snapshot at
    held_since and (if exited) a 'cancelled' snapshot at the acquisition
    date; each fund entity gets a snapshot at the LP statement's effective
    (sharing) date. Returns the run's counts, in RUN_COUNTS order."""
    manifest, run_dir = run.manifest, run.dir
    # A dry run writes no manifest, so this covers older bronze only.
    if manifest.get("dry_run"):
        log.info("skipping %s: dry-run dump (bootstrap only, no holdings)",
                 run_dir.name)
        raise loader.Skip
    dump_snap = run.snapshot_at
    ind_id = _s(manifest.get("individual_id"))
    firm_id = _s(manifest.get("firm_id"))

    n_sec = n_vest = n_fund = n_call = n_evt = n_navh = 0
    entities = []           # (eid, edir, is a fund), in dir-name order
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
            is_fund = bool(meta.get("is_fund_investment"))
            entities.append((eid, edir, is_fund))

            if is_fund:
                # Fund LP: snapshot at the capital-account statement's
                # effective (sharing) date — its NAV is a quarter-end
                # figure, not a download-time one. The full quarterly NAV
                # history comes from the statement PDFs (load_statement_nav,
                # after this loop).
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
                    # Side-loaded `<eid>-valuations.csv` is the single
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
                        n_sec += load_securities(conn, cancel_ts, eid, edir,
                                                 held=False,
                                                 val_price=val_price,
                                                 exercises=matched)
                        events.append((cancel_ts, eid, "disposition",
                                       cancel_s,
                                       "acquisition: securities cancelled"))
                    n_evt += load_capital_events(conn, events)
                n_vest += load_vesting(conn, dump_snap, eid, edir)

    docs_dir = run_dir / "documents"
    n_doc = load_documents(conn, dump_snap, run_dir.name, docs_dir)
    n_k1 = load_k1_capital_accounts(conn, docs_dir)
    fund_eids = [eid for eid, _edir, is_fund in entities if is_fund]
    for eid in fund_eids:
        n_navh += load_statement_nav(conn, docs_dir, eid)
    _warn_unowned_fund_documents(docs_dir, fund_eids)
    n_cf = load_cash_flows(conn, run_dir, dump_snap, entities)
    return (n_sec, n_evt, n_vest, n_fund, n_navh, n_call, n_cf, n_doc, n_k1)


def _parser() -> argparse.ArgumentParser:
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
    return p


# bronze.pending_run_dirs skips a run whose status is not loadable; an
# unreadable manifest stays loadable there, but this loader needs the
# manifest's ids, so the frame passes over such a run with a warning.
SPEC = loader.LoaderSpec(
    name="carta", schema=MIGRATIONS_DIR, load_run=load_run, parser=_parser,
    counts=RUN_COUNTS, run_columns=run_columns,
    after_runs=sweep_extract_cache, manifest=loader.MANIFEST_SKIP)


def main(argv: list[str]) -> int:
    return loader.main(SPEC, argv)


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
