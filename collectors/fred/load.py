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

import json
import logging
import sys
from pathlib import Path

from collectorkit import loader, series

log = logging.getLogger("fred.load")

HERE = Path(__file__).resolve().parent
MIGRATIONS = HERE / "migrations"

# FRED's no-data / market-holiday sentinel.
NO_DATA = {".", None, ""}

FX_COLUMNS = ("snapshot_at", "base_currency_iso", "quote_currency_iso",
              "mid", "payload")

# The manifest fields that say what a series is, per family. An FX entry
# names no family, the shape every FX entry in bronze has. A rates entry
# names "rates", and each rates row's payload holds its fields in this
# order.
FX_MEANING = ("base", "quote")
RATE_MEANING = ("currency", "tenor", "basis")


def dated_observations(doc: dict) -> list[dict]:
    """The observations of a FRED document that carry a value; a NO_DATA
    value is FRED's no-data / holiday sentinel."""
    return [o for o in doc.get("observations", [])
            if o.get("value") not in NO_DATA]


def _dated(run_dir: Path, sid: str) -> list[tuple[str, str]]:
    """(observation date, value) for each dated observation of one series
    in a run. Empty, with a warning, when the run holds no file for the
    series."""
    sfile = run_dir / f"{sid}.json"
    if not sfile.is_file():
        log.warning("series file %s missing; skipping", sfile.name)
        return []
    doc = json.loads(sfile.read_text(encoding="utf-8"))
    return [(o["date"], o["value"]) for o in dated_observations(doc)]


def _fx_rows(sid: str, means: dict,
             dated: list[tuple[int, str]]) -> list[tuple]:
    base, quote = means["base"], means["quote"]
    return [(day, base, quote, val,
             series.payload({"series_id": sid, "value": val}))
            for day, val in dated]


# The two families, in the order load_run counts them.
FAMILIES = {
    "fx": series.Family("fx_rates", FX_COLUMNS, FX_MEANING, _fx_rows),
    "rates": series.Family("rates", series.RATE_COLUMNS, RATE_MEANING),
}


def load_run(conn, run: loader.Run) -> tuple[int, int]:
    """Upsert every dated observation in one bronze run into its family's
    table. Returns the (fx_rates, rates) row counts written."""
    return series.load_series(
        conn, run.manifest, FAMILIES,
        # An FX entry carries no family; download.py names only the rates.
        family_of=lambda meta: meta.get("family", "fx"),
        observations=lambda sid, meta: _dated(run.dir, sid), log=log)


SPEC = series.loader_spec("fred", MIGRATIONS, load_run,
                          ("fx rows", "rates rows"), __doc__.strip())


def main(argv: list[str] | None = None) -> int:
    return loader.main(SPEC, argv)


if __name__ == "__main__":
    sys.exit(main())
