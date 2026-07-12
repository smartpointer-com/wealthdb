"""Shared backlog-recompression engine for collector ``recompress`` verbs.

One-time (or occasional) sweep that converts the *existing* bronze
backlog to the compressed-at-download-time format new runs use: every
file matching the collector's patterns inside a **complete** run dir
is replaced by a verified ``.zst`` twin via
:func:`collectorkit.compress.compress_file`.

This verb rewrites load inputs — the one thing ``prune`` is forbidden
to touch — so it lives behind the same safety envelope, plus a
verify-before-unlink rule, and must never be wired into a schedule:

* **Complete dumps only.** Non-complete run dirs are ``prune``'s
  business (they get deleted wholesale, so recompressing them would be
  wasted work at best and a race at worst); UNKNOWN (unreadable /
  corrupt manifest) dirs are never touched. Classification is the
  prune engine's, so the two verbs can never disagree.
* **Quiescence guard.** A run dir touched within ``--min-age-hours``
  is skipped, closing the race with a walk that just finalised.
* **Symlinked run dirs are skipped; symlinked files are never
  compressed.** Patterns are validated to resolve inside the run dir.
* **Verify-then-unlink.** The original is removed only after the
  compressed copy has been decompressed and sha256-matched
  (:func:`compress.compress_file`'s contract). An interrupted sweep
  leaves, at worst, a verified-or-doomed ``.zst`` next to its intact
  original; re-running adopts the pair (re-verify, then unlink the
  original) — the engine is idempotent.
* **Own byte accounting.** Compressed twins share nothing with their
  originals, but the whole point is the at-rest delta, so the engine
  reports original vs compressed bytes itself rather than deferring
  to ``du``.

Manifests are deliberately NOT rewritten: a ``run.json`` ``files``
block keeps the filename recorded at download time, and loaders
resolve the on-disk variant through
:func:`collectorkit.compress.resolve_variant`. Mutating a historical
manifest would trade a cosmetic staleness for a write to the one file
every lifecycle tool keys on.
"""
from __future__ import annotations

import argparse
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Callable

from collectorkit import bronze, compress, prune


@dataclass(frozen=True)
class RecompressConfig:
    """Per-collector recompression configuration.

    ``patterns`` — run-dir-relative glob patterns selecting the data
    files to compress (e.g. ``("cu_*/*.csv",)``). Compressed twins
    don't match a ``*.csv`` pattern, so already-converted runs drop
    out naturally.

    ``is_complete`` — the collector's completeness predicate, same
    shape (and normally the same function) as its ``PruneConfig``'s,
    so recompress and prune classify a dump identically.

    ``manifest_name`` / ``level`` — manifest filename for
    classification; zstd level for the sweep.
    """

    patterns: tuple[str, ...]
    is_complete: Callable[[Path, dict | None], prune.Classification]
    manifest_name: str | None = "run.json"
    level: int = compress.DEFAULT_LEVEL


def _prune_config(config: RecompressConfig) -> prune.PruneConfig:
    """The prune-engine view of this config, for classify() reuse."""
    return prune.PruneConfig(debug_subdirs=(),
                             is_complete=config.is_complete,
                             manifest_name=config.manifest_name)


def plan_recompress(bronze_dir: Path, config: RecompressConfig,
                    min_age_s: float, now: float | None = None):
    """Build the sweep plan.

    Returns ``(targets, skipped)``. Each target is a dict with
    ``path`` (a plain file to compress), ``bytes``, and ``adopt``
    (True when a ``.zst`` twin already exists — an interrupted prior
    attempt to finish rather than a fresh compression). ``skipped``
    lists run dirs left alone, each with a ``reason`` and ``age_s``
    (``None`` when age is not the reason).
    """
    now = time.time() if now is None else now
    pcfg = _prune_config(config)
    targets = []
    skipped = []
    for e in prune.iter_run_eligibility(bronze_dir, pcfg, min_age_s, now):
        if e.verdict == prune.SKIP_SYMLINK:
            skipped.append({
                "path": e.run_dir, "age_s": None,
                "reason": "symlinked run dir (foreign; download never "
                          "creates one)"})
            continue
        if e.verdict == prune.SKIP_NOT_COMPLETE:
            skipped.append({"path": e.run_dir, "age_s": None,
                            "reason": f"not a complete dump ({e.reason})"})
            continue
        if e.verdict == prune.SKIP_BAD_SLUG:
            skipped.append({"path": e.run_dir, "age_s": None,
                            "reason": "unparseable timestamp slug"})
            continue
        if e.verdict == prune.SKIP_TOO_YOUNG:
            skipped.append({"path": e.run_dir, "reason": e.reason,
                            "age_s": e.age_s})
            continue
        for pattern in config.patterns:
            for f in sorted(e.run_dir.glob(pattern)):
                if not f.is_file() or f.is_symlink():
                    continue
                twin = f.with_name(f.name + compress.ZSTD_SUFFIX)
                targets.append({
                    "path": f,
                    "bytes": f.lstat().st_size,
                    "adopt": twin.is_file(),
                })
    return targets, skipped


def validate_target(path: Path, bronze_dir: Path) -> None:
    """Refuse anything that is not a regular file strictly inside a
    timestamped run dir under ``bronze_dir``. Belt-and-braces between
    the planner and the unlink of an original."""
    if path.is_symlink() or not path.is_file():
        raise SystemExit(f"refusing to touch non-regular file: {path}")
    for parent in path.parents:
        if parent.parent == bronze_dir:
            if bronze.RUN_DIR_RE.match(parent.name):
                return
            break
    raise SystemExit(f"refusing to touch path outside a run dir: {path}")


def run(config: RecompressConfig, bronze_dir: Path, *, dry_run: bool,
        min_age_hours: float) -> int:
    """Execute (or preview) the sweep. Returns an exit code and prints
    per-file lines plus the byte accounting summary."""
    if not bronze_dir.is_dir():
        raise SystemExit(f"--bronze-dir does not exist: {bronze_dir}")

    min_age_s = min_age_hours * 3600.0
    targets, skipped = plan_recompress(bronze_dir, config, min_age_s)

    verb = "would compress" if dry_run else "compressing"
    total_orig = 0
    total_comp = 0
    done = 0
    for t in targets:
        path = t["path"]
        rel = path.relative_to(bronze_dir)
        label = " [finishing interrupted attempt]" if t["adopt"] else ""
        print(f"{verb}  {rel}{label}  ({prune.human_size(t['bytes'])})")
        total_orig += t["bytes"]
        if dry_run:
            continue
        validate_target(path, bronze_dir)
        final = path.with_name(path.name + compress.ZSTD_SUFFIX)
        if t["adopt"]:
            # A twin from an interrupted attempt: trust it only if it
            # verifies against the original; otherwise recompress.
            twin_digest, _ = compress.decompressed_sha256(final)
            orig_digest, _ = bronze.sha256_file(path)
            if twin_digest == orig_digest:
                path.unlink()
            else:
                print(f"  ...existing twin does not verify; recompressing")
                final = compress.compress_file(path, level=config.level)
        else:
            final = compress.compress_file(path, level=config.level)
        total_comp += final.lstat().st_size
        done += 1
    for s in skipped:
        if s["age_s"] is None:
            print(f"skipping  {s['path'].relative_to(bronze_dir)}/"
                  f"  [{s['reason']}]")
        else:
            print(f"skipping  {s['path'].relative_to(bronze_dir)}/"
                  f"  [{s['reason']}; only {s['age_s'] / 60:.0f} min old "
                  f"— possibly still being written]")

    if not targets:
        print("nothing to recompress")
        return 0
    if dry_run:
        print(f"would compress: {len(targets)} files, "
              f"{prune.human_size(total_orig)} before compression")
    else:
        print(f"compressed: {done} files, {prune.human_size(total_orig)} → "
              f"{prune.human_size(total_comp)} "
              f"(freed {prune.human_size(total_orig - total_comp)})")
        print("note: verify with a `load --force` rebuild — silver must "
              "come out identical to the pre-sweep state")
    return 0


def build_parser(description: str,
                 prog: str | None = None) -> argparse.ArgumentParser:
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
        help="Print the sweep plan; rewrite nothing.",
    )
    p.add_argument(
        "--min-age-hours", type=float, default=1.0,
        help=("Leave run dirs touched within this window alone — closes "
              "the race with a download that just finalised. Default: 1."),
    )
    return p


def main(config: RecompressConfig, argv=None, *,
         description: str | None = None, prog: str | None = None) -> int:
    """argparse entry point for a collector's thin ``recompress.py``."""
    parser = build_parser(description or __doc__, prog=prog)
    args = parser.parse_args(argv)
    return run(config, args.bronze_dir, dry_run=args.dry_run,
               min_age_hours=args.min_age_hours)
