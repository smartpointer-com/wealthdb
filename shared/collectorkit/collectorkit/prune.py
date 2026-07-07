"""Shared bronze-prune engine for collector ``prune.py`` verbs.

Every collector's ``prune`` deletes the same two categories from its
bronze tree, with the same safety envelope. Factoring the walk here —
rather than copying it into each collector — keeps the one irreversible
operation (``rmtree`` of a bronze path) in a single reviewed, unit-tested
place, and guarantees the semantics are byte-identical everywhere.

Two categories are removed, across every timestamped run dir under the
bronze root:

* **debug artefacts inside a complete dump** — the ``debug_subdirs``
  the collector nominates (e.g. ``screenshots/``, a Playwright
  ``trace.zip``), plus any ``debug_globs`` *files* matched deeper in the
  tree (e.g. a legacy per-account HTML capture stranded inside a
  load-input dir). These are diagnostics a run writes for
  troubleshooting; ``load`` never reads them. Deleting them leaves
  silver byte-identical. Collectors that write no bronze-resident debug
  artefact pass an empty ``debug_subdirs``/``debug_globs`` — then only
  the second category applies.

* **whole run dirs that are not complete dumps** — a crashed or
  interrupted download, or a ``--dry-run`` shell. ``load`` would
  otherwise keep re-ingesting whatever partial artefacts such a dir
  holds; after pruning one, the next ``load --force`` rebuild reflects
  the removal.

The safety envelope is non-negotiable and identical for every collector:

* **A ``load`` input is never deleted.** The only paths ever removed are
  ``<run>/<debug-subdir>`` entries inside a *complete* dump, and whole
  *non-complete* run dirs. A complete dump's data (positions/activity
  CSVs, document PDFs, JSON payloads, the manifest) is out of scope by
  construction. Each collector's ``debug_subdirs`` must list only paths
  its ``load.py`` never reads.

* **An unreadable or corrupt manifest is UNKNOWN → never deleted.** A
  manifest that could not be *read* (permission / I/O error) or *parsed*
  (corrupt bytes) is an environmental failure, not proof the dump is
  partial. It is classified UNKNOWN and skipped before the collector's
  completeness predicate is even consulted, so a stray EIO or a
  root-owned ``run.json`` never costs a complete dump its load inputs.

* **Symlinks are never followed or deleted** — not the run dir, not a
  ``debug_subdirs`` entry, not a ``debug_globs`` match, and not any
  intermediate directory a deep ``debug_globs`` pattern traverses (a
  symlinked component would let a match resolve outside the tree, so it is
  refused). Nothing at the bronze root that isn't a timestamped run dir (a
  silver ``.db``, a shared ``manual/`` tree) is ever touched.

* **In-flight downloads are protected.** A running download mints its
  run-dir slug at the start but only writes a terminal manifest at the
  end, so slug age alone would misjudge a long backfill as abandoned.
  The guard instead keys on the newest mtime anywhere in the dir, which
  an active walk keeps fresh as it writes each artefact; a whole-dir
  deletion additionally rechecks completeness + quiescence immediately
  before the ``rmtree``.

Completeness is per-collector (each records it differently — a ``status``
field, a ``dry_run`` flag, an ``errors`` list, the mere presence of a
terminal artefact), so the collector supplies an ``is_complete`` predicate
via :class:`PruneConfig`. :func:`status_classification` implements the
normalized ``status``-field convention with a per-collector legacy
fallback, which most predicates delegate to.
"""

from __future__ import annotations

import argparse
import json
import shutil
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Callable

from collectorkit import bronze

# Classification states returned by an is_complete predicate and by
# classify().
COMPLETE = "complete"          # keep load inputs; prune only debug artefacts
NON_COMPLETE = "non-complete"  # whole-dir deletion candidate (once quiescent)
UNKNOWN = "unknown"            # manifest unreadable/corrupt — never a candidate

# A predicate returns (state, reason); reason is surfaced in the output.
Classification = tuple  # (state: str, reason: str)


@dataclass(frozen=True)
class PruneConfig:
    """Per-collector prune configuration.

    ``debug_subdirs`` — names of *top-level* entries inside a run dir that
    are debug artefacts (dirs or files), deleted from *complete* dumps.
    Must never include a ``load`` input. Empty for collectors that write no
    bronze-resident debug artefact.

    ``debug_globs`` — run-dir-relative glob patterns matching debug-artefact
    *files* to delete from *complete* dumps, for artefacts that live deeper
    than the top level a ``debug_subdirs`` name can address (e.g.
    ``transactions/*/page-*.html`` — a legacy per-account capture stranded
    inside a load-input dir). Only regular files are ever matched (a whole
    subtree is ``debug_subdirs``' job); each pattern must be crafted so it
    can NEVER match a ``load`` input sibling. Empty by default, so a
    collector that declares none is byte-for-byte unaffected.

    ``is_complete`` — ``(run_dir, meta) -> (state, reason)`` where ``meta``
    is the parsed manifest dict (or ``None`` when the manifest file is
    absent), and ``state`` is one of :data:`COMPLETE` /
    :data:`NON_COMPLETE` / :data:`UNKNOWN`. The predicate is only invoked
    once the manifest has been read successfully (or found absent); a
    manifest that fails to read/parse is classified UNKNOWN centrally, so
    the predicate never has to defend against that.

    ``manifest_name`` — the manifest filename to read and hand to the
    predicate (default ``run.json``). ``None`` for a collector with no
    manifest file, in which case ``meta`` is always ``None`` and the
    predicate keys purely on artefacts present in the run dir.
    """

    debug_subdirs: tuple[str, ...] = ()
    debug_globs: tuple[str, ...] = ()
    is_complete: Callable[[Path, dict | None], Classification] = None  # type: ignore[assignment]
    manifest_name: str | None = "run.json"
    # Human label for the kind of thing a non-complete whole-dir deletion
    # removes; only affects output wording.
    dump_label: str = "dump"


# ---------------------------------------------------------------------------
# Completeness helpers
# ---------------------------------------------------------------------------

def status_classification(
    meta: dict | None,
    *,
    run_dir: Path | None = None,
    legacy_complete: Callable[[Path | None, dict | None], bool] | None = None,
) -> Classification:
    """Classify a dump from the normalized ``status`` field, with a
    per-collector legacy fallback.

    The forward convention (written by every ``download`` after this
    change): ``status`` is ``"in-progress"`` while the walk runs, then
    atomically overwritten with ``"complete"`` / ``"dry-run"`` at the end.
    So ``status == "complete"`` ⇒ COMPLETE, any other present status
    (``"in-progress"``, ``"dry-run"``, …) ⇒ NON_COMPLETE.

    Existing dumps predate the ``status`` field. For a statusless dump,
    ``legacy_complete(run_dir, meta)`` decides: it inspects the
    collector's original terminal signal (a specific artefact present, an
    empty ``errors`` list, ``dry_run: false``, …) and returns True when
    the dump looks finished. Absent a legacy predicate, a statusless dump
    is treated as NON_COMPLETE.

    A missing manifest (``meta is None``) is NON_COMPLETE unless
    ``legacy_complete`` says otherwise — a collector whose terminal signal
    is an artefact rather than the manifest (e.g. one that never wrote a
    ``run.json``) supplies a ``legacy_complete`` that checks that artefact.

    ``legacy_complete(run_dir, meta)`` is consulted in BOTH the
    missing-manifest branch (``meta is None``) and the statusless-manifest
    branch (``meta`` is a dict with no ``status`` key). Return True only
    when your terminal completeness signal is genuinely present, so it
    stays False for a crashed download:

    * terminal signal *is* the manifest (the walk writes ``run.json`` only
      at the end): ``legacy_complete=lambda rd, m: m is not None``.
    * terminal signal is an artefact (collector may have written no
      manifest historically): ``legacy_complete=lambda rd, m:
      (rd / "<terminal-artefact>").exists()``.
    """
    if meta is None:
        if legacy_complete is not None and legacy_complete(run_dir, meta):
            return COMPLETE, "complete (no manifest; legacy artefact signal)"
        return NON_COMPLETE, "no manifest (crashed/interrupted download)"
    status = meta.get("status")
    if status == COMPLETE:
        return COMPLETE, "complete"
    if status is not None:
        return NON_COMPLETE, f"status={status!r}"
    # Statusless legacy dump: defer to the collector's original signal.
    if legacy_complete is not None and legacy_complete(run_dir, meta):
        return COMPLETE, "complete (legacy dump, no status field)"
    return NON_COMPLETE, "no status field; legacy completeness signal absent"


# ---------------------------------------------------------------------------
# Filesystem stats
# ---------------------------------------------------------------------------

def entry_stats(path: Path) -> tuple[int, int, float]:
    """``(file_count, total_bytes, newest_mtime)`` for a file or a dir tree.

    Symlinks are neither followed nor counted toward file_count/bytes, but
    every entry's mtime (files *and* subdirs, and the path itself)
    contributes to ``newest_mtime`` — the signal the in-flight guard keys
    on to tell a long-running walk from an abandoned one.
    """
    try:
        st = path.lstat()
    except OSError:
        return 0, 0, 0.0
    if path.is_symlink():
        # Never traverse into a symlink; report its own mtime only.
        return 0, 0, st.st_mtime
    if path.is_file():
        return 1, st.st_size, st.st_mtime
    files = 0
    size = 0
    newest = st.st_mtime
    for p in path.rglob("*"):
        try:
            pst = p.lstat()
        except OSError:
            continue
        newest = max(newest, pst.st_mtime)
        if p.is_file() and not p.is_symlink():
            files += 1
            size += pst.st_size
    return files, size, newest


# Back-compat alias — the reference collector named this dir_stats.
dir_stats = entry_stats


def human_size(n: int) -> str:
    for unit in ("B", "KiB", "MiB", "GiB"):
        if n < 1024 or unit == "GiB":
            return f"{n:.1f} {unit}" if unit != "B" else f"{n} B"
        n /= 1024
    return f"{n:.1f} GiB"


# ---------------------------------------------------------------------------
# Classification (central manifest read + UNKNOWN safety)
# ---------------------------------------------------------------------------

def classify(run_dir: Path, config: PruneConfig) -> Classification:
    """``(state, reason)`` for a run dir.

    Reads the manifest (when the collector declares one) and enforces the
    UNKNOWN-never-deleted invariant *before* the collector predicate runs:
    a manifest that is present but unreadable (I/O / permission error) or
    unparseable (corrupt bytes) short-circuits to UNKNOWN. Only a cleanly
    read manifest (or a cleanly absent one) reaches ``config.is_complete``.
    """
    meta: dict | None = None
    if config.manifest_name:
        path = run_dir / config.manifest_name
        try:
            raw = path.read_text(encoding="utf-8")
        except FileNotFoundError:
            meta = None
        except OSError as e:
            return UNKNOWN, f"{config.manifest_name} unreadable ({e.__class__.__name__})"
        else:
            try:
                meta = json.loads(raw)
            except ValueError:
                return UNKNOWN, f"{config.manifest_name} is not valid JSON (corrupt?)"
            if not isinstance(meta, dict):
                return UNKNOWN, f"{config.manifest_name} is not a JSON object"
    state, reason = config.is_complete(run_dir, meta)
    if state not in (COMPLETE, NON_COMPLETE, UNKNOWN):
        # A misbehaving predicate must fail safe, never delete.
        return UNKNOWN, f"predicate returned unknown state {state!r}"
    return state, reason


def _quiescent_age_s(run_dir: Path, newest_mtime: float,
                     now: float) -> float | None:
    """Seconds since the run dir was last touched — the max of its slug
    timestamp, its own mtime, and the newest mtime under it. ``None`` if
    the slug is calendar-invalid (the caller then skips rather than
    guesses). An in-flight walk keeps this near zero (it writes artefacts
    continuously); an abandoned dump ages past the guard.
    """
    try:
        slug_ts = float(bronze.parse_run_ts(run_dir.name))
    except ValueError:
        return None
    try:
        own_mtime = run_dir.stat().st_mtime
    except OSError:
        own_mtime = 0.0
    return now - max(slug_ts, own_mtime, newest_mtime)


# ---------------------------------------------------------------------------
# Planning
# ---------------------------------------------------------------------------

def plan_prune(bronze_dir: Path, config: PruneConfig, min_age_s: float,
               now: float | None = None):
    """Build the deletion plan.

    Returns ``(targets, skipped)``. Each target is a dict with ``path``,
    ``kind`` (``'debug-artefacts'`` | ``'non-complete dump'``),
    ``reason``, ``files``, ``bytes``. ``skipped`` lists run dirs
    deliberately left alone (in-flight by the age guard, unreadable
    manifest, unparseable slug, or a foreign symlink), each with a
    ``reason`` and an ``age_s`` (``None`` when age is not the reason).
    """
    now = time.time() if now is None else now
    targets = []
    skipped = []
    for run_dir in bronze.iter_run_dirs(bronze_dir):
        if run_dir.is_symlink():
            skipped.append({
                "path": run_dir, "age_s": None,
                "reason": "symlinked run dir (foreign; download never "
                          "creates one)"})
            continue
        state, reason = classify(run_dir, config)
        if state == COMPLETE:
            for name in config.debug_subdirs:
                entry = run_dir / name
                if entry.exists() and not entry.is_symlink():
                    files, size, _ = entry_stats(entry)
                    targets.append({
                        "path": entry, "kind": "debug-artefacts",
                        "reason": reason, "files": files, "bytes": size,
                    })
            # Deeper-nested debug FILES a top-level debug_subdirs name
            # cannot address (e.g. transactions/*/page-*.html). Only
            # regular files (never a whole subtree) and never a symlink.
            for pattern in config.debug_globs:
                for f in sorted(run_dir.glob(pattern)):
                    if (f.is_file() and not f.is_symlink()
                            and _no_symlinked_dir_between(run_dir, f)):
                        files, size, _ = entry_stats(f)
                        targets.append({
                            "path": f, "kind": "debug-artefacts",
                            "reason": reason, "files": files, "bytes": size,
                        })
            continue
        if state == UNKNOWN:
            skipped.append({"path": run_dir, "reason": reason,
                            "age_s": None})
            continue
        # NON_COMPLETE: candidate for whole-dir deletion, but only once it
        # is quiescent — an active walk mints its slug at start and only
        # writes a terminal manifest at the end, so slug age alone would
        # misjudge a long backfill as abandoned.
        files, size, newest = entry_stats(run_dir)
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


def _ancestor_run_dir(path: Path, bronze_dir: Path) -> Path | None:
    """The bronze run dir containing ``path`` — the ancestor directly under
    ``bronze_dir`` whose name matches :data:`bronze.RUN_DIR_RE` — or
    ``None`` when ``path`` is not nested inside one."""
    for parent in path.parents:
        if parent.parent == bronze_dir and bronze.RUN_DIR_RE.match(parent.name):
            return parent
    return None


def _no_symlinked_dir_between(run_dir: Path, path: Path) -> bool:
    """True iff no directory component strictly between ``run_dir`` and
    ``path`` is a symlink (``run_dir`` and the leaf ``path`` are checked by
    the caller). ``Path.glob`` follows a symlinked intermediate directory
    when resolving a ``*`` component, so a ``debug_globs`` match could
    otherwise resolve to — and delete — a file OUTSIDE the bronze tree
    (e.g. ``<run>/transactions/999`` symlinked elsewhere), violating the
    engine's never-follow-symlinks envelope. Walk the components and refuse
    if any is a symlink."""
    cur = run_dir
    for part in path.relative_to(run_dir).parts[:-1]:
        cur = cur / part
        if cur.is_symlink():
            return False
    return True


def validate_target(path: Path, bronze_dir: Path, config: PruneConfig) -> None:
    """Refuse anything but ``<bronze>/<run>/<debug-subdir>``, a whole
    ``<bronze>/<run>`` dir, or a ``debug_globs``-matched regular FILE nested
    inside a ``<bronze>/<run>`` dir — and never a symlink. Belt-and-braces
    against a planner bug before an irreversible delete.
    """
    if path.is_symlink():
        raise SystemExit(f"refusing to delete symlink: {path}")
    # Whole run dir, or a top-level debug subdir of one.
    if path.name in config.debug_subdirs and path.parent.parent == bronze_dir:
        run_dir = path.parent
    else:
        run_dir = path
    if run_dir.parent == bronze_dir and bronze.RUN_DIR_RE.match(run_dir.name):
        return
    # A debug_globs-matched regular file, nested arbitrarily deep in a
    # complete run dir. Re-derive the ancestor run dir and re-check the
    # glob independently of the planner: a path is accepted here ONLY when
    # it is a real (non-symlink) file whose run-dir-relative path STILL
    # matches an explicit debug glob. Because every load input (a
    # ``.json``/``.csv``/``.xml``/``more-details.json`` sibling) fails that
    # glob by construction, a planner bug can never route one through here.
    if config.debug_globs and path.is_file():
        anc = _ancestor_run_dir(path, bronze_dir)
        if (anc is not None and anc.is_dir() and not anc.is_symlink()
                and _no_symlinked_dir_between(anc, path)):
            rel = path.relative_to(anc)
            if any(rel.match(pattern) for pattern in config.debug_globs):
                return
    raise SystemExit(f"refusing to delete unexpected path: {path}")


def _recheck_target(target: dict, config: PruneConfig, min_age_s: float,
                    now: float | None = None) -> bool:
    """Re-verify a whole-dir deletion immediately before ``rmtree``, to
    close the window between planning and deletion. Debug-artefact removals
    are always safe (never load inputs) and pass through. A non-complete
    dump is deleted only if it is STILL non-complete and STILL quiescent;
    if a walk finalised it or resumed writing since planning, skip it.

    Debug artefacts — both ``debug_subdirs`` entries and ``debug_globs``
    files — are never load inputs (``validate_target`` re-checks the glob
    just before the unlink), so they pass through unconditionally.
    """
    if target["kind"] != "non-complete dump":
        return True
    run_dir = target["path"]
    state, _ = classify(run_dir, config)
    if state != NON_COMPLETE:
        return False
    now = time.time() if now is None else now
    _, _, newest = entry_stats(run_dir)
    age_s = _quiescent_age_s(run_dir, newest, now)
    return age_s is not None and age_s >= min_age_s


def _remove(path: Path) -> None:
    """Delete a validated target: a dir tree via ``rmtree``, a file via
    ``unlink`` (a debug artefact may be a single file, e.g. a trace zip)."""
    if path.is_dir() and not path.is_symlink():
        shutil.rmtree(path)
    else:
        path.unlink()


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------

def build_parser(description: str, prog: str | None = None) -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        prog=prog, description=description,
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
        help=("Leave non-complete dumps written within this window alone "
              "— the guard keys on the newest mtime in the dir, so a long "
              "download in flight is protected while an abandoned one ages "
              "out. Default: 1."),
    )
    return p


def run(config: PruneConfig, bronze_dir: Path, *, dry_run: bool,
        min_age_hours: float) -> int:
    """Execute (or preview) the prune against ``bronze_dir``. Returns an
    exit code. Prints the plan/outcome in the format shared by every
    collector's ``prune``."""
    if not bronze_dir.is_dir():
        raise SystemExit(f"--bronze-dir does not exist: {bronze_dir}")

    min_age_s = min_age_hours * 3600.0
    targets, skipped = plan_prune(bronze_dir, config, min_age_s)

    verb = "would delete" if dry_run else "deleting"
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
        if not dry_run:
            validate_target(t["path"], bronze_dir, config)
            if not _recheck_target(t, config, min_age_s):
                print(f"  ...changed since planning; skipping {rel}")
                continue
            _remove(t["path"])
    for s in skipped:
        if s["age_s"] is None:
            print(f"skipping  {s['path'].relative_to(bronze_dir)}/"
                  f"  [{s['reason']}]")
        else:
            print(f"skipping  {s['path'].relative_to(bronze_dir)}/"
                  f"  [{s['reason']}; only {s['age_s'] / 60:.0f} min old "
                  f"— possibly a download in flight]")

    if not targets:
        print("nothing to prune")
        return 0
    print(f"{'would free' if dry_run else 'freed'}: "
          f"{human_size(total_bytes)} ({total_files} files, "
          f"{len(targets)} paths)")
    if dumps_deleted and not dry_run:
        print(f"note: {dumps_deleted} non-complete {config.dump_label}(s) "
              "removed — silver rows sourced from them persist until the "
              "next `load --force` rebuild")
    return 0


def main(config: PruneConfig, argv=None, *, description: str | None = None,
         prog: str | None = None) -> int:
    """argparse entry point for a collector's thin ``prune.py``. Pass the
    collector's module ``__doc__`` as ``description`` so ``--help`` shows
    collector-specific detail."""
    parser = build_parser(description or __doc__, prog=prog)
    args = parser.parse_args(argv)
    return run(config, args.bronze_dir, dry_run=args.dry_run,
               min_age_hours=args.min_age_hours)
