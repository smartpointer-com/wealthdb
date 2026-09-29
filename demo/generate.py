#!/usr/bin/env python3
"""Generate the demo household into a demo root.

    python3 demo/generate.py --root DIR [--as-of YYYY-MM-DD] [--seed S]
                             [--append] [--with-findings]

Writes DIR/silver/<source>.db (the synthetic silver kind),
DIR/wealthdb.cfg and DIR/overrides/*.csv. Reads only demo/household.json,
demo/catalogue/ and the kind's schema file in the engine's source tree;
never the environment, a real config or a real data root. Writes only
into a DIR that is new, empty, or marked as a demo root by an earlier
build.

A full build (the default) rewrites every silver file from scratch and
removes DIR's gold file. A rebuild at the same as-of carries the change
number the old gold already read, so that gold would skip it.

An append run replays the whole history in memory and adds only the
days after the as-of the files already reached. It refuses when the
generator or its inputs changed since the files were written.
"""

import argparse
import datetime as dt
import pathlib
import sys

sys.path.insert(0, str(pathlib.Path(__file__).resolve().parent))

from demohouse import config, dates, silver, spec  # noqa: E402
from demohouse.household import Simulation  # noqa: E402

GOLD_FILES = ("wealthdb.db", "wealthdb.db.wal")


class Refused(Exception):
    pass


def _placeholder(path):
    """The engine wrapper creates an empty wealthdb.cfg in the config dir
    it is given, so a root the engine saw before any build holds one."""
    return path.name == "wealthdb.cfg" and path.is_file() and path.stat().st_size == 0


def check_root(root):
    """A root is safe to write into when it is new, empty, or marked as
    a demo root by an earlier build."""
    if not root.exists():
        return
    if not root.is_dir():
        raise Refused(f"{root} is not a directory")
    if (root / config.DEMO_MARKER).exists():
        return
    found = sorted(p.name for p in root.iterdir() if p.name != ".DS_Store" and not _placeholder(p))
    if found:
        shown = ", ".join(found[:5]) + (", ..." if len(found) > 5 else "")
        raise Refused(f"{root} holds {shown} but no {config.DEMO_MARKER} marker; "
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
    if not append:
        for name in GOLD_FILES:
            (root / name).unlink(missing_ok=True)
    lines = []
    for src in sources:
        path = silver_dir / f"{src}.db"
        if append and prev[src] == as_of:
            # A run cut short leaves some files appended and some not; the
            # next run completes the rest.
            lines.append(f"{src}: already at {as_of}")
            continue
        rows = silver.source_rows(sim, src, as_of)
        if append:
            n = silver.append(path, rows, meta, prev[src], as_of)
            lines.append(f"{src}: +{n} rows after {prev[src]}")
        else:
            n = silver.write_full(path, rows, meta, dates.epoch(sim.fx_start), as_of)
            lines.append(f"{src}: {n} rows")
    _write_config(root, inputs, sim)
    return lines


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
