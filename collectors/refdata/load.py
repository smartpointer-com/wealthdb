#!/usr/bin/env python3
"""
refdata silver loader: bronze runs in, silver rows out.

Each complete run not loaded yet is parsed request by request: the
provider that fetched a folder parses it (providers/), and its rows land
in silver in one transaction per run. A fund and day a run serves again
replaces what silver held for that fund and day, on every basis. A
fund's rate sensitivities are replaced only by a request that states
them, and its characteristics per provider and figure, so an issuer's
file and a filing for the same day stand side by side. A security
answer replaces the same provider's answer for the same day, and a
company's figure for a period the same provider's figure for that
period, unless the one held was filed later or states its filing day
where the new one does not. A dividend event replaces the same
provider's row of the same event, unless that one came from a later
run. An answer that lists every distribution of a ref (a fund's
history) also drops the provider's rows of the ref it no longer states,
from the oldest day it lists on: a day the source corrected leaves no
stale event beside the new one in silver. That holds for runs loaded in
the order they ran: an earlier run loaded after a later one can bring
back a row the later answer dropped, as nothing records the drop. A
source that states only some (the latest distribution, a list of days)
adds to what silver holds. A
request the planner asked for a fund's facts alone (its manifest
entry's `facts_only`) loads those facts, never the composition its
document also states. The run is recorded in dump_runs with loaded_at,
the instant of this load, which gold's change window reads: a run
fetched earlier but loaded later still reaches gold.

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
from providers.base import COLUMNS, FLAGS, Parsed, ProviderError, log_notes

log = logging.getLogger("refdata.load")

HERE = Path(__file__).resolve().parent
MIGRATIONS = HERE / "migrations"

# The tables a fund and day is replaced in whole, whichever request
# serves it again.
FUND_DAY_TABLES = ("compositions", "lines", "attributes", "counterparties")
# Every table a request writes by key, in the order it writes them.
TABLES = (*FUND_DAY_TABLES, "fund_risk", "characteristics", "securities", "security_facts")

# A company's figure for a period: a row replaces the one silver holds
# for its key unless that one was filed later, or states its filing day
# where the new one states none (a frame's figure beside a filing's).
PERIOD_FACTS_UPSERT = (
    f"INSERT INTO period_facts ({', '.join(COLUMNS['period_facts'])}) "
    f"VALUES ({', '.join('?' for _ in COLUMNS['period_facts'])}) "
    "ON CONFLICT (security_key, field, as_of, period_start, provider) DO UPDATE SET "
    + ", ".join(f"{c} = excluded.{c}" for c in COLUMNS["period_facts"]
                if c not in ("security_key", "field", "as_of", "period_start", "provider"))
    + " WHERE period_facts.filed IS NULL OR excluded.filed >= period_facts.filed")

# A dividend event: a row replaces the one silver holds for its key
# unless that one came from a later run. A run fetched earlier but loaded
# later leaves it.
EVENTS_KEY = ("ref", "provider", "event_id")
EVENTS_UPSERT = (
    f"INSERT INTO dividend_events ({', '.join(COLUMNS['dividend_events'])}) "
    f"VALUES ({', '.join('?' for _ in COLUMNS['dividend_events'])}) "
    f"ON CONFLICT ({', '.join(EVENTS_KEY)}) DO UPDATE SET "
    + ", ".join(f"{c} = excluded.{c}" for c in COLUMNS["dividend_events"] if c not in EVENTS_KEY)
    + " WHERE excluded.run_at >= dividend_events.run_at")


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


def _first_day(days) -> str | None:
    """The earliest of an event's ex, record and pay days, as YYYY-MM-DD;
    None where it states none."""
    return min((d.isoformat() if isinstance(d, date) else d for d in days if d), default=None)


def _drop_unstated_events(conn, events: list, run_at: int) -> None:
    """For each ref and provider of a complete answer's events
    (Parsed.events_complete), delete the rows silver holds that the
    answer does not state: those whose first day (_first_day) is on or
    after the oldest the answer states, from a run not later than this
    one. A row older than what the source lists stays, and so does what a
    later run stated, when an earlier run loads after it."""
    stated = {}
    for r in events:
        stated.setdefault((r["ref"], r["provider"]), []).append(r)
    for (ref, provider), rows in sorted(stated.items()):
        since = min(filter(None, (_first_day((r["ex_date"], r["record_date"], r["pay_date"])) for r in rows)),
                    default=None)
        if since is None:
            continue
        ids = {r["event_id"] for r in rows}
        held = conn.execute("SELECT event_id, ex_date, record_date, pay_date FROM dividend_events "
                            "WHERE ref = ? AND provider = ? AND run_at <= ?", (ref, provider, run_at)).fetchall()
        for eid, *days in held:
            first = _first_day(days)
            if eid not in ids and first is not None and first >= since:
                conn.execute("DELETE FROM dividend_events WHERE ref = ? AND provider = ? AND event_id = ?",
                             (ref, provider, eid))


def write(conn, parsed: Parsed, run_at: int) -> int:
    """Write one request's rows. A fund-day the rows' compositions or
    attributes name is cleared first, on every basis and table, so a
    re-served day replaces the old one. A fund-day's rate sensitivities
    are cleared only by rows that state them, and a provider's figure for
    a fund-day only by that provider's rows of the same figure, every
    part of a breakdown with it: an issuer's file for a day leaves a
    filing's figures standing, and a page that dates one figure later
    leaves the others' days as they were. A dividend event replaces the
    same provider's same event (EVENTS_UPSERT); a complete answer's
    events also drop the rows it no longer states
    (_drop_unstated_events)."""
    days = {(c["fund_ref"], epoch(c["as_of"])) for c in parsed.compositions}
    days |= {(a["fund_ref"], epoch(a["as_of"])) for a in parsed.attributes}
    for ref, at in sorted(days):
        for table in FUND_DAY_TABLES:
            conn.execute(f"DELETE FROM {table} WHERE fund_ref = ? AND as_of = ?", (ref, at))
    for ref, at in sorted({(r["fund_ref"], epoch(r["as_of"])) for r in parsed.fund_risk}):
        conn.execute("DELETE FROM fund_risk WHERE fund_ref = ? AND as_of = ?", (ref, at))
    stated = {(c["fund_ref"], epoch(c["as_of"]), c["provider"], c["field"]) for c in parsed.characteristics}
    for ref, at, provider, field in sorted(stated):
        conn.execute("DELETE FROM characteristics WHERE fund_ref = ? AND as_of = ? AND provider = ? AND field = ?",
                     (ref, at, provider, field))
    n = sum(silver.upsert_rows(conn, table, COLUMNS[table], [_row(table, r, run_at) for r in getattr(parsed, table)])
            for table in TABLES)
    facts = [_row("period_facts", r, run_at) for r in parsed.period_facts]
    conn.executemany(PERIOD_FACTS_UPSERT, facts)
    if parsed.events_complete:
        _drop_unstated_events(conn, parsed.dividend_events, run_at)
    events = [_row("dividend_events", r, run_at) for r in parsed.dividend_events]
    conn.executemany(EVENTS_UPSERT, events)
    return n + len(facts) + len(events)


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
                parsed = provider.parse(run_dir / req["dir"])
                if req.get("facts_only"):
                    # Asked for its facts alone (download.Request): the
                    # composition the document also states stays out.
                    parsed.compositions, parsed.lines, parsed.attributes, parsed.counterparties = [], [], [], []
                rows += write(conn, parsed, run_at)
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
    log_notes(REGISTRY.values(), log)
    return 0


if __name__ == "__main__":
    sys.exit(main())
