#!/usr/bin/env python3
"""
FRED silver loader: FX reference rates and US Treasury yields.

Parses bronze FRED runs into two silver tables, one per family of
series. `fx_rates` holds one row per (observation date, base, quote).
`rates` holds one row per (observation date, series), the value in
percent.

Rows are UPSERTED (INSERT OR REPLACE), so a fresh fetch overwrites any
FRED-revised dates and appends new ones. The bronze run itself is
recorded in dump_runs, so an already-loaded run is skipped (override
with --force). Each series' family and meaning (an FX pair's base/quote
direction, a rate's currency, tenor and basis) is read from the run's
run.json manifest, so the silver is reproducible from bronze alone.

Usage:
    load.py --silver-db <db> --bronze-dir <dir> [--force]
"""

from __future__ import annotations

import argparse
import json
import logging
import sys
from datetime import datetime
from pathlib import Path

from collectorkit import bronze, cli, parse, silver

log = logging.getLogger("fred.load")

HERE = Path(__file__).resolve().parent
MIGRATIONS = HERE / "migrations"

# FRED's no-data / market-holiday sentinel.
NO_DATA = {".", None, ""}

FX_COLUMNS = ("snapshot_at", "base_currency_iso", "quote_currency_iso",
              "mid", "payload")
RATE_COLUMNS = ("as_of", "series_id", "value", "payload")

# The manifest fields that say what a series is, per family. An FX entry
# names no family, the shape every FX entry in bronze has. A rates entry
# names "rates", and each rates row's payload holds its fields in this
# order.
FX_MEANING = ("base", "quote")
RATE_MEANING = ("currency", "tenor", "basis")


def date_to_epoch(s: str) -> int:
    """An observation date 'YYYY-MM-DD' -> Unix seconds at 00:00 UTC."""
    return parse.epoch_day(datetime.strptime(s, "%Y-%m-%d"))


def _payload(fields: dict) -> str:
    """A payload column's value: compact JSON, keys in the given order."""
    return json.dumps(fields, separators=(",", ":"))


def dated_observations(doc: dict) -> list[dict]:
    """The observations of a FRED document that carry a value; a NO_DATA
    value is FRED's no-data / holiday sentinel."""
    return [o for o in doc.get("observations", [])
            if o.get("value") not in NO_DATA]


def _dated(run_dir: Path, sid: str) -> list[tuple[int, str]]:
    """(observation date, value) for each dated observation of one series
    in a run. Empty, with a warning, when the run holds no file for the
    series."""
    sfile = run_dir / f"{sid}.json"
    if not sfile.is_file():
        log.warning("series file %s missing; skipping", sfile.name)
        return []
    doc = json.loads(sfile.read_text(encoding="utf-8"))
    return [(date_to_epoch(o["date"]), o["value"])
            for o in dated_observations(doc)]


def _meaning(sid: str, meta: dict, fields: tuple[str, ...]) -> dict | None:
    """The manifest entry's `fields`, in order. None, with a warning, when
    any of them is missing or empty."""
    meaning = {k: meta.get(k) for k in fields}
    if not all(meaning.values()):
        log.warning("series %s has no %s in manifest; skipping", sid,
                    "/".join(fields))
        return None
    return meaning


def _fx_rows(run_dir: Path, sid: str, meta: dict) -> list[tuple]:
    meaning = _meaning(sid, meta, FX_MEANING)
    if meaning is None:
        return []
    base, quote = meaning["base"], meaning["quote"]
    return [(day, base, quote, val, _payload({"series_id": sid, "value": val}))
            for day, val in _dated(run_dir, sid)]


def _rate_rows(run_dir: Path, sid: str, meta: dict) -> list[tuple]:
    meaning = _meaning(sid, meta, RATE_MEANING)
    if meaning is None:
        return []
    payload = _payload(meaning)
    return [(day, sid, val, payload) for day, val in _dated(run_dir, sid)]


def load_run(conn, run_dir: Path) -> tuple[int, int]:
    """Upsert every dated observation in one bronze run into its family's
    table. Returns the (fx_rates, rates) row counts written."""
    manifest = json.loads((run_dir / "run.json").read_text(encoding="utf-8"))
    fx, rates = [], []
    for sid, meta in manifest.get("series", {}).items():
        # An FX entry carries no family; download.py names only the rates.
        family = meta.get("family", "fx")
        if family == "fx":
            fx += _fx_rows(run_dir, sid, meta)
        elif family == "rates":
            rates += _rate_rows(run_dir, sid, meta)
        else:
            log.warning("series %s has unknown family %r in manifest; "
                        "skipping", sid, family)
    return (silver.upsert_rows(conn, "fx_rates", FX_COLUMNS, fx),
            silver.upsert_rows(conn, "rates", RATE_COLUMNS, rates))


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(description=__doc__.strip())
    p.add_argument("--silver-db", type=Path, required=True,
                   help="Path to the silver SQLite DB (created if missing).")
    p.add_argument("--bronze-dir", type=Path, required=True,
                   help="Directory containing bronze run subdirectories.")
    cli.add_standard_args(p, verb="load")
    args = p.parse_args(sys.argv[1:] if argv is None else argv)
    cli.configure_logging(args.verbose)

    # --force = delete the silver DB, then rebuild from all bronze (the
    # fleet-wide meaning). After a reset the DB is empty, so the normal
    # loaded-snapshots skip below naturally re-ingests every run.
    if args.force:
        silver.reset(args.silver_db)
    conn = silver.open_db(args.silver_db)
    version = silver.apply_migrations(conn, MIGRATIONS)

    n_runs = n_fx = n_rates = 0
    # A run dir with no run.json, or one whose status is not complete, is
    # skipped without a dump_runs row, so a later load picks it up once
    # its walk finalises.
    for run_dir in bronze.pending_run_dirs(conn, args.bronze_dir, log=log):
        run_ts = bronze.parse_run_ts(run_dir.name)
        conn.execute("BEGIN")
        try:
            fx, rates = load_run(conn, run_dir)
            conn.execute(
                "INSERT OR REPLACE INTO dump_runs "
                "(snapshot_at, silver_schema_version, run_dir) VALUES (?, ?, ?)",
                (run_ts, version, str(run_dir.resolve())),
            )
            conn.execute("COMMIT")
        except Exception:
            conn.execute("ROLLBACK")
            raise
        n_runs += 1
        n_fx += fx
        n_rates += rates
        log.info("loaded run %s: %d fx rows, %d rates rows", run_dir.name,
                 fx, rates)

    log.info("done: %d run(s), %d fx rows, %d rates rows -> %s", n_runs,
             n_fx, n_rates, args.silver_db)
    return 0


if __name__ == "__main__":
    sys.exit(main())
