#!/usr/bin/env python3
"""
Prune debug artefacts and non-complete dumps from the bronze tree.

Two categories are removed, across every timestamped run dir under
``--bronze-dir``:

* ``<run>/screenshots/`` — debug captures (HTML DOM dumps, PNG
  screenshots, ``--explore`` DOM inventories). Written only when
  ``download`` runs with ``--debug`` / ``--explore``; never read
  by ``load``. Deleting them leaves silver byte-identical.

* whole run dirs that are not complete dumps: ``run.json`` is
  missing or unparseable (the walk crashed or was interrupted
  before finalising), or its ``status`` is anything other than
  ``"complete"`` (e.g. a ``--dry-run`` shell). ``load`` would
  otherwise keep re-ingesting whatever partial artefacts such a
  dir holds; after pruning one, the next ``load --force`` rebuild
  reflects the removal.

An in-flight guard skips non-complete dumps that were written
within ``--min-age-hours`` (default 1). A running ``download``
mints its run-dir slug at the start of the walk but only writes a
terminal ``run.json`` at the end, so slug age alone would misjudge
a long backfill as abandoned; the guard instead keys on the newest
mtime anywhere in the dir, which an active walk keeps fresh as it
writes each artefact.

Everything else is structurally out of scope — the only paths ever
deleted are ``<run>/screenshots/`` subtrees and whole non-complete
run dirs. Load inputs inside complete dumps (positions / activity
CSVs, document PDFs, balances / performance HTML, ``run.json``)
and non-run entries at the bronze root (``supplied-statements/``, the
silver DB) are never touched.

``--dry-run`` prints the deletion plan without removing anything.

Usage:
    prune.py [--bronze-dir /data] [--dry-run] [--min-age-hours N]
"""

from __future__ import annotations

import argparse
import json
import shutil
import sys
import time
from pathlib import Path

from collectorkit import bronze

SCREENSHOTS_DIR = "screenshots"

# dump_status() states.
COMPLETE = "complete"        # run.json parses with status == "complete"
NON_COMPLETE = "non-complete"  # definitively absent / non-complete status
UNKNOWN = "unknown"          # manifest unreadable — never a delete candidate


def dir_stats(path: Path) -> tuple[int, int, float]:
    """(file_count, total_bytes, newest_mtime) under `path`.

    Symlinks are neither followed nor counted. ``newest_mtime``
    spans every entry (files *and* subdirs), so it reflects the
    most recent write anywhere in the tree — the signal the
    in-flight guard keys on to tell a long-running walk from an
    abandoned one."""
    files = 0
    size = 0
    newest = 0.0
    for p in path.rglob("*"):
        try:
            st = p.lstat()
        except OSError:
            continue
        newest = max(newest, st.st_mtime)
        if p.is_file() and not p.is_symlink():
            files += 1
            size += st.st_size
    return files, size, newest


def human_size(n: int) -> str:
    for unit in ("B", "KiB", "MiB", "GiB"):
        if n < 1024 or unit == "GiB":
            return f"{n:.1f} {unit}" if unit != "B" else f"{n} B"
        n /= 1024
    return f"{n:.1f} GiB"


def dump_status(run_dir: Path) -> tuple[str, str]:
    """(state, reason). ``state`` is one of COMPLETE, NON_COMPLETE,
    UNKNOWN.

    Deletion requires positive evidence of incompleteness: a
    ``run.json`` that is definitively absent (crashed before the
    walk finalised) or that parses with a non-complete status.
    A manifest that merely could not be *read* (permission / I/O
    error) or *parsed* (corrupt bytes) is UNKNOWN — an
    environmental failure, not proof the dump is partial — and is
    always skipped, so a stray EIO or a root-owned run.json never
    costs a complete dump its load inputs."""
    path = run_dir / "run.json"
    try:
        raw = path.read_text(encoding="utf-8")
    except FileNotFoundError:
        return NON_COMPLETE, "no run.json (crashed/interrupted walk)"
    except OSError as e:
        return UNKNOWN, f"run.json unreadable ({e.__class__.__name__})"
    try:
        meta = json.loads(raw)
    except ValueError:
        return UNKNOWN, "run.json is not valid JSON (corrupt?)"
    status = meta.get("status")
    if status == COMPLETE:
        return COMPLETE, "complete"
    return NON_COMPLETE, f"status={status!r}"


def _quiescent_age_s(run_dir: Path, newest_mtime: float,
                     now: float) -> float | None:
    """Seconds since the run dir was last touched — max of its slug
    timestamp, its own mtime, and the newest mtime under it. None if
    the slug is calendar-invalid (so the caller skips rather than
    guesses). An in-flight walk keeps this near zero (it writes
    artefacts continuously); an abandoned dump ages past the guard."""
    try:
        slug_ts = float(bronze.parse_run_ts(run_dir.name))
    except ValueError:
        return None
    try:
        own_mtime = run_dir.stat().st_mtime
    except OSError:
        own_mtime = 0.0
    return now - max(slug_ts, own_mtime, newest_mtime)


def plan_prune(bronze_dir: Path, min_age_s: float, now: float | None = None):
    """Build the deletion plan.

    Returns (targets, skipped). Each target is a dict with ``path``,
    ``kind`` ('debug-artefacts' | 'non-complete dump'), ``reason``,
    ``files``, ``bytes``. ``skipped`` lists run dirs deliberately
    left alone (in-flight by the age guard, unreadable manifest,
    unparseable slug, or a foreign symlink), each with a ``reason``
    and an ``age_s`` (None when age is not the reason)."""
    now = time.time() if now is None else now
    targets = []
    skipped = []
    for run_dir in bronze.iter_run_dirs(bronze_dir):
        if run_dir.is_symlink():
            skipped.append({
                "path": run_dir, "age_s": None,
                "reason": "symlinked run dir (foreign; download "
                          "never creates one)"})
            continue
        state, reason = dump_status(run_dir)
        if state == COMPLETE:
            shots = run_dir / SCREENSHOTS_DIR
            if shots.is_dir() and not shots.is_symlink():
                files, size, _ = dir_stats(shots)
                targets.append({
                    "path": shots, "kind": "debug-artefacts",
                    "reason": reason, "files": files, "bytes": size,
                })
            continue
        if state == UNKNOWN:
            skipped.append({"path": run_dir, "reason": reason,
                            "age_s": None})
            continue
        # NON_COMPLETE: candidate for whole-dir deletion, but only
        # once it is quiescent — an active walk mints its slug at
        # start and only writes a terminal run.json at the end, so
        # slug age alone would misjudge a long backfill as abandoned.
        files, size, newest = dir_stats(run_dir)
        age_s = _quiescent_age_s(run_dir, newest, now)
        if age_s is None:
            skipped.append({
                "path": run_dir, "age_s": None,
                "reason": f"{reason}; unparseable timestamp slug"})
            continue
        if age_s < min_age_s:
            skipped.append({"path": run_dir, "reason": reason,
                            "age_s": age_s})
            continue
        targets.append({
            "path": run_dir, "kind": "non-complete dump",
            "reason": reason, "files": files, "bytes": size,
        })
    return targets, skipped


def validate_target(path: Path, bronze_dir: Path) -> None:
    """Refuse anything but <bronze>/<run>/screenshots or a whole
    <bronze>/<run> dir, and never a symlink. Belt-and-braces
    against planner bugs before an irreversible rmtree."""
    if path.is_symlink():
        raise SystemExit(f"refusing to delete symlink: {path}")
    if path.name == SCREENSHOTS_DIR:
        run_dir = path.parent
    else:
        run_dir = path
    if run_dir.parent != bronze_dir or not bronze.RUN_DIR_RE.match(run_dir.name):
        raise SystemExit(f"refusing to delete unexpected path: {path}")


def _recheck_target(target: dict, min_age_s: float,
                    now: float | None = None) -> bool:
    """Re-verify a whole-dir deletion immediately before rmtree, to
    close the window between planning and deletion. Debug-artefact
    (screenshots) removals are always safe — they are never load
    inputs — so they pass through. A non-complete dump is deleted
    only if it is STILL non-complete and STILL quiescent; if a walk
    finalised it or resumed writing since planning, skip it."""
    if target["kind"] != "non-complete dump":
        return True
    run_dir = target["path"]
    state, _ = dump_status(run_dir)
    if state != NON_COMPLETE:
        return False
    now = time.time() if now is None else now
    _, _, newest = dir_stats(run_dir)
    age_s = _quiescent_age_s(run_dir, newest, now)
    return age_s is not None and age_s >= min_age_s


def main(argv=None):
    p = argparse.ArgumentParser(
        description=__doc__.strip(),
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    p.add_argument(
        "--bronze-dir", type=Path, default=Path("/data"),
        help="Bronze tree root. Default: /data.",
    )
    p.add_argument(
        "--dry-run", action="store_true",
        help="Print the deletion plan; remove nothing.",
    )
    p.add_argument(
        "--min-age-hours", type=float, default=1.0,
        help=("Leave non-complete dumps written within this window "
              "alone — the guard keys on the newest mtime in the "
              "dir, so a long download in flight is protected while "
              "an abandoned one ages out. Default: 1."),
    )
    args = p.parse_args(argv)

    bronze_dir = args.bronze_dir
    if not bronze_dir.is_dir():
        raise SystemExit(f"--bronze-dir does not exist: {bronze_dir}")

    targets, skipped = plan_prune(bronze_dir, args.min_age_hours * 3600.0)

    min_age_s = args.min_age_hours * 3600.0
    verb = "would delete" if args.dry_run else "deleting"
    total_bytes = 0
    total_files = 0
    dumps_deleted = 0
    for t in targets:
        rel = t["path"].relative_to(bronze_dir)
        label = t["kind"]
        if t["kind"] == "non-complete dump":
            label += f": {t['reason']}"
        print(f"{verb}  {rel}{'/' if t['path'].is_dir() else ''}"
              f"  [{label}]"
              f"  ({t['files']} files, {human_size(t['bytes'])})")
        total_bytes += t["bytes"]
        total_files += t["files"]
        if t["kind"] == "non-complete dump":
            dumps_deleted += 1
        if not args.dry_run:
            validate_target(t["path"], bronze_dir)
            if not _recheck_target(t, min_age_s):
                print(f"  ...changed since planning; skipping {rel}")
                continue
            shutil.rmtree(t["path"])
    for s in skipped:
        if s["age_s"] is None:
            print(f"skipping  {s['path'].relative_to(bronze_dir)}/"
                  f"  [{s['reason']}]")
        else:
            print(f"skipping  {s['path'].relative_to(bronze_dir)}/"
                  f"  [{s['reason']}; only {s['age_s'] / 60:.0f} min "
                  f"old — possibly a download in flight]")

    if not targets:
        print("nothing to prune")
        return 0
    print(f"{'would free' if args.dry_run else 'freed'}: "
          f"{human_size(total_bytes)} ({total_files} files, "
          f"{len(targets)} paths)")
    if dumps_deleted and not args.dry_run:
        print(f"note: {dumps_deleted} non-complete dump(s) removed — "
              "silver rows sourced from them persist until the next "
              "`load --force` rebuild")
    return 0


if __name__ == "__main__":
    sys.exit(main())
