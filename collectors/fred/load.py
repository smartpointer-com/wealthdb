#!/usr/bin/env python3
"""
FRED FX-rate silver loader.

Parses bronze FRED runs into the silver `fx_rates` table: one row per
(observation date, base, quote). Rates are UPSERTED (INSERT OR REPLACE)
so a fresh fetch overwrites any FRED-revised dates and appends new ones;
the bronze run itself is recorded in dump_runs so an already-loaded run
is skipped (override with --force). The base/quote direction for each
series is read from the run's run.json manifest, so the silver is
reproducible from bronze alone.

Usage:
    load.py --silver-db <db> --bronze-dir <dir> [--force]
"""

from __future__ import annotations

import argparse
import json
import logging
import sys
from datetime import datetime, timezone
from pathlib import Path

from collectorkit import bronze, cli, silver

log = logging.getLogger("fred.load")

HERE = Path(__file__).resolve().parent
MIGRATIONS = HERE / "migrations"

# FRED's no-data / market-holiday sentinel.
NO_DATA = {".", None, ""}


def date_to_epoch(s: str) -> int:
    """An observation date 'YYYY-MM-DD' -> Unix seconds at 00:00 UTC."""
    return int(datetime.strptime(s, "%Y-%m-%d")
               .replace(tzinfo=timezone.utc).timestamp())


def load_run(conn, run_dir: Path) -> int:
    """Upsert every (date, base, quote) rate in one bronze run. Returns
    the number of rows written."""
    manifest_path = run_dir / "run.json"
    if not manifest_path.is_file():
        log.warning("no run.json in %s; skipping run", run_dir)
        return 0
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    written = 0
    for sid, meta in manifest.get("series", {}).items():
        base, quote = meta.get("base"), meta.get("quote")
        if not base or not quote:
            log.warning("series %s has no base/quote in manifest; skipping", sid)
            continue
        sfile = run_dir / f"{sid}.json"
        if not sfile.is_file():
            log.warning("series file %s missing; skipping", sfile.name)
            continue
        doc = json.loads(sfile.read_text(encoding="utf-8"))
        for obs in doc.get("observations", []):
            val = obs.get("value")
            if val in NO_DATA:
                continue
            snap = date_to_epoch(obs["date"])
            payload = json.dumps({"series_id": sid, "value": val},
                                 separators=(",", ":"))
            conn.execute(
                "INSERT OR REPLACE INTO fx_rates "
                "(snapshot_at, base_currency_iso, quote_currency_iso, mid, "
                "payload) VALUES (?, ?, ?, ?, ?)",
                (snap, base, quote, val, payload),
            )
            written += 1
    return written


def main(argv: list[str] | None = None) -> int:
    p = argparse.ArgumentParser(description=__doc__.strip())
    p.add_argument("--silver-db", type=Path, required=True,
                   help="Path to the silver SQLite DB (created if missing).")
    p.add_argument("--bronze-dir", type=Path, required=True,
                   help="Directory containing bronze run subdirectories.")
    p.add_argument("--force", action="store_true",
                   help="Reload all bronze runs, even ones already loaded.")
    cli.add_common_args(p)
    args = p.parse_args(sys.argv[1:] if argv is None else argv)
    cli.configure_logging(args.verbose)

    conn = silver.open_db(args.silver_db)
    version = silver.apply_migrations(conn, MIGRATIONS)
    loaded = set() if args.force else silver.loaded_snapshots(conn)

    n_runs = n_rows = 0
    for run_dir in bronze.iter_run_dirs(args.bronze_dir):
        run_ts = bronze.parse_run_ts(run_dir.name)
        if run_ts in loaded:
            log.debug("run %s already loaded; skipping", run_dir.name)
            continue
        conn.execute("BEGIN")
        try:
            rows = load_run(conn, run_dir)
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
        n_rows += rows
        log.info("loaded run %s: %d fx rows", run_dir.name, rows)

    log.info("done: %d run(s), %d fx rows -> %s", n_runs, n_rows, args.silver_db)
    return 0


if __name__ == "__main__":
    sys.exit(main())
