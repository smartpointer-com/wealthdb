"""One load frame for a collector whose silver is built run by run.

A loader states what is its own in a :class:`LoaderSpec`: its command
line, its migrations, how one bronze run becomes rows, and the passes
around the runs. :func:`main` does the rest the same way for every
collector:

1. parse the command line and set up logging;
2. open the silver; stop, before anything is written, when its schema
   version is above the code's last migration; apply the migrations;
3. find the runs silver has not loaded (``spec.pending``);
4. load each run in one transaction, its ``dump_runs`` row written last,
   so a run that fails leaves silver as it was and loads again next time;
5. log each run's counts, then the totals.

``--force`` does the same into a fresh silver beside the old one, from
every run, and puts it in place only once the load returns
(:func:`collectorkit.rebuild.rebuild`). A failed ``--force`` leaves the
old silver as it was: an error, a stop, or a non-zero status from
``after_runs``.

A failure inside a run rolls that run back and ends the load: the error
surfaces and the exit status is not zero. A stop
(:class:`collectorkit.rebuild.LoadStop`) is logged as an error and exits
1. :class:`Skip` is the one way to pass over a run, and it writes no
``dump_runs`` row, so the run is read again by the next load.

Imported as ``from collectorkit import loader``; it is not imported by
the package itself.
"""
from __future__ import annotations

import argparse
import json
import logging
import sqlite3
import sys
from collections.abc import Callable, Mapping
from dataclasses import dataclass, field
from pathlib import Path

from collectorkit import bronze, cli, rebuild, silver

# What the frame does with a run whose run.json cannot be read.
MANIFEST_OPTIONAL = "optional"   # `Run.manifest` is None; the run loads
MANIFEST_REQUIRED = "required"   # the read error ends the load
MANIFEST_SKIP = "skip"           # a warning; the run is passed over


class _Failed(Exception):
    """A ``--force`` whose ``after_runs`` returned a non-zero status."""

    def __init__(self, status: int):
        super().__init__(status)
        self.status = status


class Skip(Exception):
    """Raised by `load_run` to pass over a run: the frame rolls back what
    the run wrote, records no ``dump_runs`` row and goes on. The next load
    reads the run again. `load_run` logs why."""


@dataclass
class Run:
    """One pending bronze run, as `load_run` gets it."""
    dir: Path
    snapshot_at: int
    # The silver schema version after the migrations.
    version: int
    # run.json as `read_run_manifest` reads it under the spec's policy.
    manifest: dict | None
    # The command line, parsed: the loader's own flags with the standard
    # ones.
    args: argparse.Namespace
    # The run's `dump_runs` row, column to value, from `spec.run_columns`.
    # `load_run` may add or change columns; the frame writes the row after
    # `load_run` returns.
    row: dict = field(default_factory=dict)


@dataclass
class Load:
    """One call of the command, as the passes around the runs see it."""
    args: argparse.Namespace
    silver_db: Path
    bronze_dir: Path
    log: logging.Logger
    version: int = 0
    # The runs this call loaded.
    loaded: int = 0


def default_run_columns(run: Run) -> dict:
    """The `dump_runs` row most loaders write: the run's instant, the
    schema version and the run dir's resolved path."""
    return {"snapshot_at": run.snapshot_at,
            "silver_schema_version": run.version,
            "run_dir": str(run.dir.resolve())}


def given_run_columns(run: Run) -> dict:
    """The `dump_runs` row of :func:`default_run_columns` with the run
    dir's path as given, not resolved."""
    return {"snapshot_at": run.snapshot_at,
            "silver_schema_version": run.version,
            "run_dir": str(run.dir)}


def given_paths(args: argparse.Namespace) -> tuple[Path, Path]:
    """The silver DB and the bronze dir as ``--silver-db`` and
    ``--bronze-dir`` name them."""
    return args.silver_db, args.bronze_dir


@dataclass(frozen=True)
class LoaderSpec:
    """What one collector's loader is made of.

    `parser` builds the command line with the loader's own flags, help
    texts and defaults; the frame adds the standard load flags
    (``-v``, ``--force``). `defaults` turns the parsed flags into the
    silver DB path and the bronze dir.

    `load_run(conn, run)` writes one run's rows inside the frame's
    transaction. It returns the run's counts, one number per label of
    `counts` and in that order (a bare number for one label), or None
    when `counts` is empty. The log names them: with ``counts=("fx
    rows",)`` a run that returns 3 logs ``3 fx rows``, and the closing
    line gives the totals. Counts of another shape raise TypeError
    inside the run's transaction, so the run rolls back.

    The hooks around the runs, each optional:

    * ``preflight(args)`` before anything is opened or reset; a number it
      returns is the exit status, and nothing is loaded;
    * ``before_runs(conn, load)`` after the migrations;
    * ``after_runs(conn, load)`` after the last run; a number it returns
      is the exit status. Under ``--force`` a non-zero one also discards
      the fresh silver and keeps the old one.

    `pending(conn, bronze_dir, log=)` lists the runs to load, oldest
    first, none of them loaded yet. `open_db` opens the silver; a
    default-isolation connection works too.
    """
    name: str
    schema: Path
    load_run: Callable[[sqlite3.Connection, Run], tuple[int, ...] | int | None]
    parser: Callable[[], argparse.ArgumentParser]
    counts: tuple[str, ...] = ()
    defaults: Callable[[argparse.Namespace], tuple[Path, Path]] = given_paths
    after_runs: Callable[[sqlite3.Connection, Load], int | None] | None = None
    before_runs: Callable[[sqlite3.Connection, Load], None] | None = None
    preflight: Callable[[argparse.Namespace], int | None] | None = None
    pending: Callable[..., list[Path]] = bronze.pending_run_dirs
    run_columns: Callable[[Run], dict] = default_run_columns
    open_db: Callable[[Path], sqlite3.Connection] = silver.open_db
    manifest: str = MANIFEST_OPTIONAL


def read_run_manifest(run_dir: Path, policy: str = MANIFEST_OPTIONAL):
    """The run's run.json, parsed.

    Under MANIFEST_REQUIRED the file's JSON is returned as it is, and a
    file that cannot be read or parsed raises its own error. Under the
    other two policies the manifest is a JSON object, or None when the
    file is absent, unreadable or holds no object.
    """
    path = Path(run_dir) / "run.json"
    if policy == MANIFEST_REQUIRED:
        return json.loads(path.read_text(encoding="utf-8"))
    return bronze.read_manifest(path)


def _counts(labels: tuple[str, ...], numbers) -> str:
    return "".join(f", {n} {label}" for label, n in zip(labels, numbers,
                                                       strict=True))


def _run_counts(spec: LoaderSpec, returned) -> tuple[int, ...]:
    """`load_run`'s counts as a tuple, one per label of `spec.counts`.
    Any other shape raises TypeError."""
    if not spec.counts and returned is None:
        return ()
    counts = (returned,) if isinstance(returned, int) else returned
    if (not isinstance(counts, (tuple, list))
            or len(counts) != len(spec.counts)):
        raise TypeError(f"{spec.name}: load_run returned {returned!r} for "
                        f"counts {spec.counts}")
    return tuple(counts)


def _write_run_row(conn: sqlite3.Connection, row: Mapping) -> None:
    columns = list(row)
    conn.execute(
        f"INSERT INTO dump_runs ({', '.join(columns)}) "
        f"VALUES ({', '.join('?' for _ in columns)})",
        [row[c] for c in columns])


def load_runs(spec: LoaderSpec, conn: sqlite3.Connection,
              load: Load) -> list[int]:
    """Load every pending run, each in its own transaction, and return
    the summed counts, in `spec.counts` order. `load.loaded` counts the
    runs loaded."""
    log = load.log
    totals = [0] * len(spec.counts)
    for run_dir in spec.pending(conn, load.bronze_dir, log=log):
        manifest = read_run_manifest(run_dir, spec.manifest)
        if manifest is None and spec.manifest == MANIFEST_SKIP:
            log.warning("skipping %s — run.json cannot be read", run_dir.name)
            continue
        run = Run(dir=run_dir, snapshot_at=bronze.parse_run_ts(run_dir.name),
                  version=load.version, manifest=manifest, args=load.args)
        run.row = spec.run_columns(run)
        try:
            with silver.transaction(conn):
                counts = _run_counts(spec, spec.load_run(conn, run))
                _write_run_row(conn, run.row)
        except Skip:
            continue
        load.loaded += 1
        if not spec.counts:
            log.info("loaded run %s", run_dir.name)
            continue
        totals = [t + n for t, n in zip(totals, counts, strict=True)]
        log.info("loaded run %s:%s", run_dir.name,
                 _counts(spec.counts, counts)[1:])
    return totals


def main(spec: LoaderSpec, argv: list[str] | None = None) -> int:
    """Run the load command `spec` describes and return its exit status."""
    p = spec.parser()
    cli.add_standard_args(p, verb="load")
    args = p.parse_args(sys.argv[1:] if argv is None else argv)
    cli.configure_logging(args.verbose)
    log = logging.getLogger(f"{spec.name}.load")

    if spec.preflight is not None:
        status = spec.preflight(args)
        if status is not None:
            return status
    silver_db, bronze_dir = spec.defaults(args)
    load = Load(args=args, silver_db=silver_db, bronze_dir=bronze_dir,
                log=log)

    def replay(conn: sqlite3.Connection, version: int) -> int | None:
        load.version = version
        if spec.before_runs is not None:
            spec.before_runs(conn, load)
        totals = load_runs(spec, conn, load)
        status = (spec.after_runs(conn, load) if spec.after_runs is not None
                  else None)
        log.info("done: %d run(s)%s -> %s", load.loaded,
                 _counts(spec.counts, totals), silver_db)
        return status

    def replay_all(conn: sqlite3.Connection, version: int) -> None:
        status = replay(conn, version)
        if status:
            raise _Failed(status)

    try:
        if args.force:
            try:
                rebuild.rebuild(silver_db, spec.schema, replay_all,
                                open_db=spec.open_db)
            except _Failed as e:
                log.error("%s: the rebuild ended with status %d, so the old "
                          "silver stays in place", silver_db, e.status)
                return e.status
            return 0
        conn = spec.open_db(silver_db)
        try:
            rebuild.migration_path_stop(conn, spec.schema, silver_db)
            return replay(conn, silver.apply_migrations(conn, spec.schema)) or 0
        finally:
            conn.close()
    except rebuild.LoadStop as e:
        log.error("%s", e)
        return 1
