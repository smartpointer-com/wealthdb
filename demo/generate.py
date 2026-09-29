#!/usr/bin/env python3
"""Generate the demo household into a demo root.

    python3 demo/generate.py --root DIR [--as-of YYYY-MM-DD] [--seed S]
                             [--append] [--with-findings]

Writes DIR/silver/<source>.db (the synthetic silver kind),
DIR/wealthdb.cfg and DIR/overrides/*.csv. Reads only demo/household.json
and demo/catalogue/; never the environment, a real config or a real data
root. Refuses a DIR that holds wealthdb files without the demo marker.

A full build (the default) rewrites every silver file from scratch. An
append run replays the whole history in memory and adds only the days
after the as-of the files already reached; it refuses when the generator
or its inputs changed since the files were written.
"""

import argparse
import datetime as dt
import pathlib
import sys

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

from demohouse import config, silver, spec  # noqa: E402
from demohouse.household import Simulation  # noqa: E402

LIVE_LOOKING = ("wealthdb.db", "wealthdb.cfg", "silver")


class Refused(Exception):
    pass


def check_root(root):
    """A root is safe to write into when it is empty, new, or marked as
    a demo root by an earlier build."""
    if not root.exists():
        return
    if not root.is_dir():
        raise Refused(f"{root} is not a directory")
    if (root / config.DEMO_MARKER).exists():
        return
    found = [n for n in LIVE_LOOKING if (root / n).exists()]
    if found:
        raise Refused(f"{root} holds {', '.join(found)} but no {config.DEMO_MARKER} marker; "
                      "it is not a demo root and nothing is written there")


def build(root, as_of, seed, append=False, findings=False, inputs=None):
    """Run the simulation to as_of and write the demo root. Returns a
    one-line summary per source."""
    root = pathlib.Path(root).resolve()
    check_root(root)
    if append and findings:
        raise Refused("--with-findings builds a one-off picture; it cannot be appended to")
    inputs = inputs or spec.load()
    meta = silver.identity(inputs, seed, findings) | {
        "generator": "demo/generate.py", "history_start": inputs.history_start.isoformat()}
    silver_dir = root / "silver"
    sources = [s["id"] for s in inputs.spec["sources"]]
    prev = {}
    if append:
        for src in sources:
            prev[src] = silver.check_append(silver_dir / f"{src}.db", meta, as_of)
        if all(p == as_of for p in prev.values()):
            return [f"already at {as_of}; nothing to append"]
    sim = Simulation(inputs, seed, as_of, findings=findings).run()
    silver_dir.mkdir(parents=True, exist_ok=True)
    (root / config.DEMO_MARKER).write_text(
        "This directory is a wealthdb demo root written by demo/generate.py.\n")
    lines = []
    for src in sources:
        rows = silver.source_rows(sim, src, as_of)
        path = silver_dir / f"{src}.db"
        if append:
            n = silver.append(path, rows, meta, prev[src], as_of)
            lines.append(f"{src}: +{n} rows after {prev[src]}")
        else:
            silver.write_full(path, rows, meta, _floor(sim), as_of)
            n = sum(len(rows[t]) for t in ("positions", "cash_balances", "fx_rates", "transactions"))
            lines.append(f"{src}: {n} rows")
    _write_config(root, inputs, sim)
    return lines


def _floor(sim):
    from demohouse import dates
    return dates.epoch(sim.fx_start)


def _write_config(root, inputs, sim):
    (root / "wealthdb.cfg").write_text(config.render(inputs.spec))
    over = root / "overrides"
    over.mkdir(exist_ok=True)
    (over / "equity_transfers.csv").write_text(
        config.csv_text(config.EQUITY_TRANSFER_COLUMNS, sim.notes.equity_transfers))
    (over / "spending_pins.csv").write_text(config.csv_text(config.PIN_COLUMNS, sim.notes.spending_pins))
    (over / "transfer_overrides.csv").write_text(
        config.csv_text(config.OVERRIDE_COLUMNS, sim.notes.transfer_overrides))


def main(argv=None):
    p = argparse.ArgumentParser(description=__doc__.split("\n\n")[0])
    p.add_argument("--root", required=True, help="demo root to write into")
    p.add_argument("--as-of", default=None, help="last simulated day, YYYY-MM-DD (default: today, UTC)")
    p.add_argument("--seed", default="harlow-19")
    p.add_argument("--append", action="store_true", help="add the days since the last run")
    p.add_argument("--with-findings", action="store_true", help="plant the diagnostic imperfections")
    a = p.parse_args(argv)
    as_of = dt.date.fromisoformat(a.as_of) if a.as_of else dt.datetime.now(dt.timezone.utc).date()
    try:
        lines = build(a.root, as_of, a.seed, append=a.append, findings=a.with_findings)
    except (Refused, silver.AppendRefused) as e:
        print(f"generate: {e}", file=sys.stderr)
        return 2
    for line in lines:
        print(f"generate: {line}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
