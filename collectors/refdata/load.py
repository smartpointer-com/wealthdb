#!/usr/bin/env python3
"""
refdata silver loader: bronze runs in, silver rows out.

Each complete run not loaded yet is parsed request by request: the
provider that fetched a folder parses it (providers/), and its rows land
in silver in one transaction per run. A fund and day a run serves again
replaces what silver held for that fund and day, on every basis. A
fund's rate sensitivities are replaced only by a request that states
them, and its characteristics per provider, so an issuer's file and a
filing for the same day stand side by side. A security answer replaces
the same provider's answer for the same day, and a company's figure for
a period the same provider's figure for that period, unless the one
held was filed later.
The run is recorded in dump_runs with loaded_at, the instant of this
load, which gold's change window reads: a run fetched earlier but loaded
later still reaches gold.

A request whose folder does not parse or load is logged and skipped,
its rows undone; the rest of the run loads. Runs load in the order they
were fetched, so a later fetch wins.

Usage:
    load.py --silver-db DB --bronze-dir DIR [--force]
"""

from __future__ import annotations

import argparse
import contextlib
import logging
import sys
import time
from datetime import date, datetime, timezone
from pathlib import Path

from collectorkit import bronze, cli, silver
from providers import REGISTRY
from providers.base import COLUMNS, FLAGS, Parsed, ProviderError

log = logging.getLogger("refdata.load")

HERE = Path(__file__).resolve().parent
MIGRATIONS = HERE / "migrations"

# The tables a fund and day is replaced in whole, whichever request
# serves it again.
FUND_DAY_TABLES = ("compositions", "lines", "attributes", "counterparties")
# Every table a request writes by key, in the order it writes them.
TABLES = (*FUND_DAY_TABLES, "fund_risk", "characteristics", "securities", "security_facts")

# A company's figure for a period: a row replaces the one silver holds
# for its key unless that one was filed later.
PERIOD_FACTS_UPSERT = (
    f"INSERT INTO period_facts ({', '.join(COLUMNS['period_facts'])}) "
    f"VALUES ({', '.join('?' for _ in COLUMNS['period_facts'])}) "
    "ON CONFLICT (security_key, field, as_of, period_start, provider) DO UPDATE SET "
    + ", ".join(f"{c} = excluded.{c}" for c in COLUMNS["period_facts"]
                if c not in ("security_key", "field", "as_of", "period_start", "provider"))
    + " WHERE excluded.filed IS NULL OR period_facts.filed IS NULL OR excluded.filed >= period_facts.filed")


def epoch(d) -> int:
    """A date as silver stores it: Unix seconds at its UTC midnight."""
    if isinstance(d, str):
        d = date.fromisoformat(d)
    return int(datetime(d.year, d.month, d.day, tzinfo=timezone.utc).timestamp())


def _row(table, r, run_at):
    out = []
    for c in COLUMNS[table]:
        if c == "run_at":
            out.append(run_at)
        elif c == "as_of":
            out.append(epoch(r[c]))
        elif c == "payload":
            out.append(silver.canonical_json(r.get("payload") or {}))
        elif c in FLAGS:
            v = r.get(c)
            out.append(None if v is None else int(bool(v)))
        else:
            v = r.get(c)
            out.append(v.isoformat() if isinstance(v, date) else v)
    return tuple(out)


def write(conn, parsed: Parsed, run_at: int) -> int:
    """Write one request's rows. A fund-day the rows' compositions or
    attributes name is cleared first, on every basis and table, so a
    re-served day replaces the old one. A fund-day's rate sensitivities
    are cleared only by rows that state them, and a provider's
    characteristics for a fund-day only by that provider's: an issuer's
    file for a day leaves a filing's figures for it standing."""
    days = {(c["fund_ref"], epoch(c["as_of"])) for c in parsed.compositions}
    days |= {(a["fund_ref"], epoch(a["as_of"])) for a in parsed.attributes}
    for ref, at in sorted(days):
        for table in FUND_DAY_TABLES:
            conn.execute(f"DELETE FROM {table} WHERE fund_ref = ? AND as_of = ?", (ref, at))
    for ref, at in sorted({(r["fund_ref"], epoch(r["as_of"])) for r in parsed.fund_risk}):
        conn.execute("DELETE FROM fund_risk WHERE fund_ref = ? AND as_of = ?", (ref, at))
    stated = {(c["fund_ref"], epoch(c["as_of"]), c["provider"]) for c in parsed.characteristics}
    for ref, at, provider in sorted(stated):
        conn.execute("DELETE FROM characteristics WHERE fund_ref = ? AND as_of = ? AND provider = ?",
                     (ref, at, provider))
    n = sum(silver.upsert_rows(conn, table, COLUMNS[table], [_row(table, r, run_at) for r in getattr(parsed, table)])
            for table in TABLES)
    facts = [_row("period_facts", r, run_at) for r in parsed.period_facts]
    conn.executemany(PERIOD_FACTS_UPSERT, facts)
    return n + len(facts)


def next_loaded_at(conn) -> int:
    """Now, or one second past the latest load where the clock has not
    moved on: loaded_at strictly increases."""
    last = conn.execute("SELECT MAX(loaded_at) FROM dump_runs").fetchone()[0]
    now = int(time.time())
    return now if last is None or now > last else last + 1


@contextlib.contextmanager
def _undone_on_error(conn):
    """The block's writes, undone where it raises; the run's transaction
    goes on."""
    conn.execute("SAVEPOINT request")
    try:
        yield
    except BaseException:
        conn.execute("ROLLBACK TO request")
        raise
    finally:
        conn.execute("RELEASE request")


def load_run(conn, run_dir: Path, manifest: dict, run_at: int) -> tuple[int, int]:
    """Load one run's requests: (rows written, requests skipped). A
    request that fails is skipped whatever it raises, so one bad folder
    never holds back the run, nor the runs after it."""
    rows = skipped = 0
    for req in manifest.get("requests", []):
        if req.get("status") != "ok" or not req.get("dir"):
            continue
        provider = REGISTRY.get(req["provider"])
        if provider is None:
            log.warning("run %s: no provider %s here; skipping %s", run_dir.name, req["provider"], req["ref"])
            skipped += 1
            continue
        try:
            with _undone_on_error(conn):
                rows += write(conn, provider.parse(run_dir / req["dir"]), run_at)
        except (ProviderError, OSError, ValueError, KeyError) as e:
            log.warning("run %s: %s %s does not parse: %s", run_dir.name, req["provider"], req["ref"], e)
            skipped += 1
        except Exception:  # noqa: BLE001 - one bad folder must not block every later load
            log.exception("run %s: %s %s does not load", run_dir.name, req["provider"], req["ref"])
            skipped += 1
    return rows, skipped


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description=__doc__.strip().split("\n\n")[0])
    p.add_argument("--silver-db", type=Path, required=True, help="The silver SQLite DB (created if missing).")
    p.add_argument("--bronze-dir", type=Path, required=True, help="The directory of bronze run dirs.")
    cli.add_standard_args(p, verb="load")
    args = p.parse_args(sys.argv[1:] if argv is None else argv)
    cli.configure_logging(args.verbose)
    if args.force:
        silver.reset(args.silver_db)
    conn = silver.open_db(args.silver_db)
    version = silver.apply_migrations(conn, MIGRATIONS)
    n_runs = n_rows = n_skipped = 0
    for run_dir in bronze.pending_run_dirs(conn, args.bronze_dir, log=log):
        run_ts = bronze.parse_run_ts(run_dir.name)
        manifest = bronze.read_manifest(run_dir / "run.json")
        if manifest is None:
            log.warning("run %s: run.json cannot be read; skipping", run_dir.name)
            continue
        with silver.transaction(conn):
            rows, skipped = load_run(conn, run_dir, manifest, run_ts)
            conn.execute("INSERT INTO dump_runs (snapshot_at, silver_schema_version, run_dir, loaded_at) "
                         "VALUES (?, ?, ?, ?)", (run_ts, version, str(run_dir.resolve()), next_loaded_at(conn)))
        n_runs += 1
        n_rows += rows
        n_skipped += skipped
        log.info("loaded run %s: %d rows%s", run_dir.name, rows, f", {skipped} skipped" if skipped else "")
    log.info("done: %d run(s), %d rows -> %s", n_runs, n_rows, args.silver_db)
    return 0


if __name__ == "__main__":
    sys.exit(main())
