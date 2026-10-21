#!/usr/bin/env python3
"""
curves silver loader: the euro and franc government yield curves.

Parses bronze curves runs into the silver `rates` table: one row per
(observation date, series), the value in percent as the portal states it.
The table has the columns of the fred collector's `rates`, so the engine
reads both the same way.

A run holds one answer per portal: the ECB's CSV for the euro series and
the SNB's CSV for the franc series. Each series' portal and meaning (its
currency, tenor, basis and compounding) are read from the run's run.json
manifest, so the silver is reproducible from bronze alone. The parsers of
both answers live here; download.py counts with them, so the manifest and
silver's rows cannot drift.

Rows are UPSERTED (INSERT OR REPLACE), so a fresh fetch overwrites a
revised day and appends new ones. The bronze run itself is recorded in
dump_runs, so an already-loaded run is skipped (override with --force).

Usage:
    load.py --silver-db <db> --bronze-dir <dir> [--force]
"""

from __future__ import annotations

import argparse
import csv
import json
import logging
import math
import sys
from datetime import datetime
from pathlib import Path

from collectorkit import bronze, cli, parse, silver

log = logging.getLogger("curves.load")

HERE = Path(__file__).resolve().parent
MIGRATIONS = HERE / "migrations"

RATE_COLUMNS = ("as_of", "series_id", "value", "payload")

# The manifest fields that say what a series is. Each row's payload holds
# them in this order.
RATE_MEANING = ("currency", "tenor", "basis", "compounding")

# The SNB warehouse cube of the Swiss Confederation's spot rates, and the
# one item of each of its other dimensions a run reads: the 11 am
# estimate, business-daily, not aggregated. A series is one item of its
# time-to-maturity dimension (LAUFZEIT). The API's paths name the cube
# with dots (it refuses an `@` there); its answer's CubeId line names it
# SNB_ANSWER_CUBE.
SNB_CUBE = "SNB1A.SNB.NSS.KZS.EID"
SNB_ANSWER_CUBE = "SNB1A@SNB.NSS.KZS.EID"
SNB_FIXED = {"ZEITPUNKT": "A1100", "frequency": "P1D_L",
             "AGGREGATIONSMETHODE": "ZZ"}

# The byte-order mark the SNB's answer opens with.
BOM = "\ufeff"


def date_to_epoch(s: str) -> int:
    """An observation date 'YYYY-MM-DD' -> Unix seconds at 00:00 UTC.
    Strict: anything else raises."""
    return parse.epoch_day(datetime.strptime(s, "%Y-%m-%d"))


def _number(value: str | None) -> str | None:
    """A cell's value as silver keeps it: the decimal string as the
    portal writes it. None for an empty cell or a placeholder that is no
    finite number (NaN, -, n/a), which the engine could not load."""
    value = (value or "").strip()
    try:
        return value if math.isfinite(float(value)) else None
    except ValueError:
        return None


def _warn_skipped(portal: str, skipped: int) -> None:
    if skipped:
        log.warning("%s: skipped %d value(s) that are no number", portal,
                    skipped)


def _payload(fields: dict) -> str:
    """A payload column's value: compact JSON, keys in the given order."""
    return json.dumps(fields, separators=(",", ":"))


def parse_ecb(text: str) -> dict[str, list[tuple[str, str]]]:
    """The ECB's CSV answer (format=csvdata) as {series key: [(date,
    value), ...]}. A series is named by the KEY column, the whole SDMX key
    with its dataflow. An empty answer (no business day in the window) has
    no header and holds nothing. A row whose value is empty or no number
    is skipped, the latter with a warning. An answer without the KEY,
    TIME_PERIOD or OBS_VALUE column raises KeyError."""
    out: dict[str, list[tuple[str, str]]] = {}
    skipped = 0
    for row in csv.DictReader(text.splitlines()):
        raw = row["OBS_VALUE"]
        value = _number(raw)
        if value is None:
            skipped += bool((raw or "").strip())
            continue
        out.setdefault(row["KEY"], []).append((row["TIME_PERIOD"], value))
    _warn_skipped("ecb", skipped)
    return out


def parse_snb(text: str) -> dict[str, list[tuple[str, str]]]:
    """The SNB's CSV answer as {series id: [(date, value), ...]}, where a
    series id is the cube and its maturity item (SNB_CUBE + ".J10M0").

    The answer opens with a byte-order mark and two lines naming the cube
    (SNB_ANSWER_CUBE) and its publishing date, then a blank line. The
    table follows, semicolon-separated, one row per day and maturity. A
    day the SNB estimates no rate for a maturity has an empty value and is
    skipped; a value that is no number is skipped with a warning. So is a
    row of another item of a fixed dimension; an answer that leaves a
    fixed dimension's column out holds that one item. An answer for
    another cube raises ValueError, one without the Date, LAUFZEIT or
    Value column KeyError."""
    lines = text.removeprefix(BOM).splitlines()
    if lines and lines[0].startswith('"CubeId";'):
        cube = lines[0].split(";", 1)[1].strip('"')
        if cube != SNB_ANSWER_CUBE:
            raise ValueError(f"the SNB answered for cube {cube!r}, "
                             f"not {SNB_ANSWER_CUBE!r}")
    start = next((i for i, line in enumerate(lines)
                  if line.startswith('"Date";')), None)
    if start is None:
        return {}
    out: dict[str, list[tuple[str, str]]] = {}
    skipped = 0
    for row in csv.DictReader(lines[start:], delimiter=";"):
        if any(row.get(k, v) != v for k, v in SNB_FIXED.items()):
            continue
        raw = row["Value"]
        value = _number(raw)
        if value is None:
            skipped += bool((raw or "").strip())
            continue
        out.setdefault(f"{SNB_CUBE}.{row['LAUFZEIT']}", []).append(
            (row["Date"], value))
    _warn_skipped("snb", skipped)
    return out


# The parser of each portal's answer.
PARSERS = {"ecb": parse_ecb, "snb": parse_snb}


def _meaning(sid: str, meta: dict) -> dict | None:
    """The manifest entry's RATE_MEANING fields, in order. None, with a
    warning, when any of them is missing or empty."""
    meaning = {k: meta.get(k) for k in RATE_MEANING}
    if not all(meaning.values()):
        log.warning("series %s has no %s in manifest; skipping", sid,
                    "/".join(RATE_MEANING))
        return None
    return meaning


def _answers(run_dir: Path, manifest: dict) -> dict[str, dict]:
    """Each portal's parsed answer in a run, by portal. A portal the run
    did not ask (its publication was unchanged) has no file and no
    answer. A file the run names but does not hold is skipped with a
    warning."""
    out = {}
    for portal, meta in manifest.get("portals", {}).items():
        name = meta.get("file")
        if not name:
            continue
        parse = PARSERS.get(portal)
        path = run_dir / name
        if parse is None:
            log.warning("portal %r has no parser; skipping %s", portal, name)
        elif not path.is_file():
            log.warning("answer file %s missing; skipping", name)
        else:
            out[portal] = parse(path.read_text(encoding="utf-8"))
    return out


def load_run(conn, run_dir: Path) -> int:
    """Upsert every dated observation in one bronze run into `rates`.
    Returns the row count written."""
    manifest = json.loads((run_dir / "run.json").read_text(encoding="utf-8"))
    answers = _answers(run_dir, manifest)
    rows = []
    for sid, meta in manifest.get("series", {}).items():
        meaning = _meaning(sid, meta)
        if meaning is None:
            continue
        payload = _payload(meaning)
        for day, value in answers.get(meta.get("portal"), {}).get(sid, []):
            rows.append((date_to_epoch(day), sid, value, payload))
    return silver.upsert_rows(conn, "rates", RATE_COLUMNS, rows)


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
    # loaded-snapshots skip below re-ingests every run.
    if args.force:
        silver.reset(args.silver_db)
    conn = silver.open_db(args.silver_db)
    version = silver.apply_migrations(conn, MIGRATIONS)

    n_runs = n_rates = 0
    # A run dir with no run.json, or one whose status is not complete, is
    # skipped without a dump_runs row, so a later load picks it up once
    # its walk finalises.
    for run_dir in bronze.pending_run_dirs(conn, args.bronze_dir, log=log):
        run_ts = bronze.parse_run_ts(run_dir.name)
        with silver.transaction(conn):
            rates = load_run(conn, run_dir)
            conn.execute(
                "INSERT OR REPLACE INTO dump_runs "
                "(snapshot_at, silver_schema_version, run_dir) VALUES (?, ?, ?)",
                (run_ts, version, str(run_dir.resolve())),
            )
        n_runs += 1
        n_rates += rates
        log.info("loaded run %s: %d rates rows", run_dir.name, rates)

    log.info("done: %d run(s), %d rates rows -> %s", n_runs, n_rates,
             args.silver_db)
    return 0


if __name__ == "__main__":
    sys.exit(main())
