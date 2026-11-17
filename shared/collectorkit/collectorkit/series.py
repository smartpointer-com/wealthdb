"""One loader for a collector of dated series (fred, curves).

A series collector's bronze run holds observations, each a (date,
value) pair of one named series, and a run.json manifest that lists the
series and what each one is. The loader writes each observation into
the table of the series' family, one row per (date, series), as an
upsert: a later run overwrites a revised day and appends new ones.

A collector states its families as :class:`Family` records and its way
of reading a series' observations from a run; :func:`load_series` does
the rest. A series whose manifest entry lacks a field of its family's
meaning is skipped with a warning, and so is one of a family the
collector does not know. Warnings go to the collector's own logger.

Imported as ``from collectorkit import series``; it is not imported by
the package itself.
"""
from __future__ import annotations

import argparse
import json
import logging
import sqlite3
from collections.abc import Callable, Iterable, Mapping
from dataclasses import dataclass
from datetime import datetime
from pathlib import Path

from collectorkit import loader, parse, silver

# The columns of the `rates` table both series collectors write.
RATE_COLUMNS = ("as_of", "series_id", "value", "payload")


def date_to_epoch(s: str) -> int:
    """An observation date 'YYYY-MM-DD' -> Unix seconds at 00:00 UTC.
    Strict: anything else raises."""
    return parse.epoch_day(datetime.strptime(s, "%Y-%m-%d"))


def payload(fields: dict) -> str:
    """A payload column's value: compact JSON, keys in the given order."""
    return json.dumps(fields, separators=(",", ":"))


def _meaning(sid: str, meta: dict, fields: tuple[str, ...],
            log: logging.Logger) -> dict | None:
    """The manifest entry's `fields`, in order. None, with a warning, when
    any of them is missing or empty."""
    found = {k: meta.get(k) for k in fields}
    if not all(found.values()):
        log.warning("series %s has no %s in manifest; skipping", sid,
                    "/".join(fields))
        return None
    return found


# The rows of one series: (series id, its meaning, its dated observations
# as (Unix day, value) pairs) -> the rows of its family's table.
Rows = Callable[[str, dict, list[tuple[int, str]]], list[tuple]]


def rate_rows(sid: str, means: dict,
              dated: list[tuple[int, str]]) -> list[tuple]:
    """A series' `rates` rows: its meaning is each row's payload."""
    p = payload(means)
    return [(day, sid, value, p) for day, value in dated]


@dataclass(frozen=True)
class Family:
    """A family of series: the table its rows go to, the manifest fields
    that say what a series of it is, and how its rows are built."""
    table: str
    columns: tuple[str, ...]
    meaning: tuple[str, ...]
    rows: Rows = rate_rows


def load_series(conn: sqlite3.Connection, manifest: dict,
                families: Mapping[str, Family],
                family_of: Callable[[dict], str],
                observations: Callable[[str, dict], Iterable[tuple[str, str]]],
                log: logging.Logger) -> tuple[int, ...]:
    """Upsert every observation of the series `manifest` lists into its
    family's table. Returns the row count written per family, in the
    order of `families`.

    `family_of(entry)` names a manifest entry's family.
    `observations(sid, entry)` yields its ('YYYY-MM-DD', value) pairs; it
    is called only for a series whose meaning is complete."""
    rows: dict[str, list[tuple]] = {name: [] for name in families}
    for sid, meta in manifest.get("series", {}).items():
        name = family_of(meta)
        family = families.get(name)
        if family is None:
            log.warning("series %s has unknown family %r in manifest; "
                        "skipping", sid, name)
            continue
        means = _meaning(sid, meta, family.meaning, log)
        if means is None:
            continue
        rows[name] += family.rows(
            sid, means,
            [(date_to_epoch(day), value)
             for day, value in observations(sid, meta)])
    return tuple(silver.upsert_rows(conn, f.table, f.columns, rows[name])
                 for name, f in families.items())


def _parser(description: str) -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(description=description)
    p.add_argument("--silver-db", type=Path, required=True,
                   help="Path to the silver SQLite DB (created if missing).")
    p.add_argument("--bronze-dir", type=Path, required=True,
                   help="Directory containing bronze run subdirectories.")
    return p


def loader_spec(name: str, schema: Path,
                load_run: Callable[[sqlite3.Connection, loader.Run],
                                   tuple[int, ...] | int],
                counts: tuple[str, ...],
                description: str) -> loader.LoaderSpec:
    """The load frame of a series collector. Its command line takes
    ``--silver-db`` and ``--bronze-dir``, both required, under
    `description`.

    The run.json is required: one that cannot be parsed ends the load. A
    run the frame does not list (no run.json yet, or a status other than
    complete) gets no dump_runs row, so a later load picks it up once its
    walk finalises."""
    return loader.LoaderSpec(
        name=name, schema=schema, load_run=load_run,
        parser=lambda: _parser(description), counts=counts,
        manifest=loader.MANIFEST_REQUIRED)
