"""Shared storage-dedup engine for a bronze tree.

Reclaims local disk by pointing byte-identical files across a collector's
run dirs at shared storage. Two strategies:

* **hardlink** (default) — the duplicate and the canonical copy become one
  inode, so the duplicate's blocks are freed. Portable: it works on any
  filesystem that supports hard links (ext4, XFS, btrfs, ZFS, APFS, HFS+,
  …), on Linux and macOS alike. Trivially idempotent ("same inode" ==
  "already shared") and preserved by hardlink-aware backup tools.
* **clone** (``--strategy clone``) — a copy-on-write reflink: an
  independent file that shares storage until one side is written. Needs a
  filesystem that supports it (btrfs / XFS reflinks on Linux, APFS clones
  on macOS) and fails loudly where it doesn't — so hardlink is the
  portable default.

Both are transparent to ``load --force``: no loader inspects inodes
(``st_ino`` / ``samefile`` are grep-clean across the collectors), so every
path still resolves to byte-identical content. Neither shrinks *remote*
backups — a backup tool that doesn't understand the shared storage
re-uploads full bytes per path; the reclaim is local disk.

**Re-running.** The "already shared" test is inode identity, so the
default hardlink strategy is idempotent and is the stable state: once two
files share an inode, any later sweep (either strategy) recognises it and
does nothing. A copy-on-write clone keeps a *distinct* inode, so clone
sharing is not recognised — re-running ``--strategy clone`` (or following a
clone sweep with the default) re-processes the files and reports storage it
already freed as fresh reclaim. This is harmless (content is never
touched), but treat ``--strategy clone`` as a deliberate one-shot; a
following default sweep converts its clones to hardlinks and settles the
tree.

This rewrites **load inputs** (the actual data files), which is why it is a
separate sweep and not part of ``prune`` (whose contract is *never touch a
load input*). It is safe to run between ``download`` and ``load`` in
orchestration — it is idempotent (hardlink) and content-preserving — an
interim reclaim of the byte-dup each re-download creates, which becomes
redundant once download-avoidance stops the re-fetch at the source. Its
safety comes from a different place than prune's: it never removes content,
only re-backs a byte-identical duplicate onto the canonical copy, and it
does so under the same envelope prune uses plus a verify-before-replace
step:

* **Complete, quiescent dumps only.** Non-complete / in-progress / dry-run
  dumps and anything touched within ``--min-age-hours`` are skipped, so a
  live download is never raced. Classification is the normalized
  ``run.json`` status (a statusless-but-readable manifest is legacy
  complete); this is generic, so one sweep serves every collector with no
  per-collector config.
* **Symlinks are never followed or replaced** — not a run dir, not a file,
  not an intermediate directory a walk descends through.
* **Same filesystem only** (a hard link can't span devices); files already
  sharing the canonical inode are skipped (idempotent; coexists with
  viac's download-time hardlinks).
* **Atomic, verified replace.** The shared file is materialised under a
  sibling ``.tmp`` name and ``os.replace``d over the duplicate; a clone is
  byte-compared against the canonical first. A crash leaves either the
  intact duplicate or the verified shared copy — never a partial file.
* **Own byte accounting.** ``du`` double-counts shared storage, so the
  verb reports reclaimed = Σ size × (distinct duplicate inodes) itself.

Runs **host-side** as a single sweep over the whole tree.
"""
from __future__ import annotations

import argparse
import os
import subprocess
import sys
import time
from dataclasses import dataclass, field
from pathlib import Path

from collectorkit import bronze, prune

# Files below one filesystem allocation block (typically 4 KiB) can't free
# a whole block, and hashing a swarm of tiny JSON/marker files costs more
# than it reclaims.
DEFAULT_MIN_SIZE = 4096


def _legacy_complete(run_dir, meta):
    # Generic, conservative completeness: a readable manifest (with or
    # without a status field) means the dump finished. Missing / unreadable
    # / non-complete-status dumps are skipped — dedup never needs full
    # coverage, and skipping is always safe (just less reclaimed).
    return meta is not None


def _is_complete(run_dir, meta):
    return prune.status_classification(
        meta, run_dir=run_dir, legacy_complete=_legacy_complete)


# A generic PruneConfig purely so we can reuse prune.classify's central
# manifest read + UNKNOWN-safety for the completeness decision.
_CLASSIFY_CFG = prune.PruneConfig(debug_subdirs=(), is_complete=_is_complete)


@dataclass
class _FileRef:
    path: Path
    size: int
    ino: int
    dev: int


@dataclass
class DupGroup:
    """A set of byte-identical files. ``canonical`` keeps its inode; every
    entry in ``dups`` is re-backed onto it. ``reclaim`` is the logical bytes
    freed (size × distinct duplicate inodes)."""
    sha: str
    size: int
    canonical: Path
    dups: list[Path] = field(default_factory=list)
    reclaim: int = 0


def _eligible_run_dirs(bronze_dir: Path, min_age_s: float, now: float,
                       skipped: list):
    """Yield COMPLETE, quiescent, non-symlink run dirs; record the rest."""
    for run_dir in bronze.iter_run_dirs(bronze_dir):
        if run_dir.is_symlink():
            skipped.append((run_dir, "symlinked run dir"))
            continue
        state, reason = prune.classify(run_dir, _CLASSIFY_CFG)
        if state != prune.COMPLETE:
            # prune's reason attributes a missing run.json to a crash; for
            # dedup a manifestless dump (schwab-api / ubs-psn legacy) may be
            # perfectly complete — we just can't confirm it, so say that
            # honestly rather than implying a crash.
            if reason.startswith("no manifest"):
                reason = "no run.json — completeness not confirmed"
            skipped.append((run_dir, reason))
            continue
        _, _, newest = prune.entry_stats(run_dir)
        age_s = prune._quiescent_age_s(run_dir, newest, now)
        if age_s is None:
            skipped.append((run_dir, "unparseable timestamp slug"))
            continue
        if age_s < min_age_s:
            skipped.append((run_dir, f"only {age_s / 60:.0f} min old "
                                     "— possibly in flight"))
            continue
        yield run_dir


def _walk_files(run_dir: Path, min_size: int):
    """Yield regular non-symlink files >= min_size under ``run_dir``,
    never descending through a symlinked directory."""
    stack = [run_dir]
    while stack:
        d = stack.pop()
        try:
            entries = list(os.scandir(d))
        except OSError:
            continue
        for e in entries:
            try:
                if e.is_symlink():
                    continue
                if e.is_dir():
                    stack.append(Path(e.path))
                elif e.is_file():
                    # Skip our own transient (a hard kill can strand one):
                    # never group/replace/count an engine artefact.
                    if e.name.endswith(".dedup-tmp"):
                        continue
                    st = e.stat()
                    if st.st_size >= min_size:
                        yield _FileRef(Path(e.path), st.st_size,
                                       st.st_ino, st.st_dev)
            except OSError:
                continue


def plan(bronze_dir: Path, *, min_age_s: float, min_size: int,
         now: float | None = None) -> tuple[list[DupGroup], list]:
    """Build the dedup plan for one collector subtree.

    Returns ``(groups, skipped)``. Files are grouped by size first (a unique
    size can't have a duplicate, so it is never hashed), then by sha256
    within each size collision, then by device (a link/clone can't span
    filesystems). Each per-device set with duplicate inodes becomes a
    :class:`DupGroup`.
    """
    now = time.time() if now is None else now
    skipped: list = []
    by_size: dict[int, list[_FileRef]] = {}
    for run_dir in _eligible_run_dirs(bronze_dir, min_age_s, now, skipped):
        for ref in _walk_files(run_dir, min_size):
            by_size.setdefault(ref.size, []).append(ref)

    groups: list[DupGroup] = []
    for size, refs in by_size.items():
        if len(refs) < 2:
            continue  # unique size — cannot collide, skip the hash
        by_sha: dict[str, list[_FileRef]] = {}
        for ref in refs:
            try:
                sha = bronze.sha256_file(ref.path)[0]
            except OSError:
                continue
            by_sha.setdefault(sha, []).append(ref)
        for sha, group in by_sha.items():
            if len(group) < 2:
                continue
            group.sort(key=lambda r: str(r.path))
            # A hard link / clone can't span devices, so dedup within each
            # filesystem independently — one DupGroup per device.
            by_dev: dict[int, list[_FileRef]] = {}
            for r in group:
                by_dev.setdefault(r.dev, []).append(r)
            for drefs in by_dev.values():
                if len(drefs) < 2:
                    continue
                # Deterministic canonical: the lexicographically-smallest
                # path on this device (the oldest run dir sorts first).
                # Re-back every file on a DIFFERENT inode (same-inode copies
                # already share storage — viac's hardlinks, a prior sweep).
                canon = drefs[0]
                dup_inodes: set[int] = set()
                dups: list[Path] = []
                for r in drefs[1:]:
                    if r.ino == canon.ino:
                        continue
                    dups.append(r.path)
                    dup_inodes.add(r.ino)
                if dups:
                    groups.append(DupGroup(
                        sha=sha, size=size, canonical=canon.path, dups=dups,
                        reclaim=size * len(dup_inodes)))
    groups.sort(key=lambda g: -g.reclaim)
    return groups, skipped


def _validate(path: Path, bronze_dir: Path) -> None:
    """Refuse anything but a regular, non-symlink file nested inside a
    ``<bronze>/<run-dir>`` with no symlinked directory between the run dir
    and the file — belt-and-braces before an irreversible replace (a
    symlinked intermediate dir swapped in after planning could otherwise
    route a write outside the tree)."""
    if path.is_symlink() or not path.is_file():
        raise SystemExit(f"refusing to touch non-regular file: {path}")
    for parent in path.parents:
        if parent.parent == bronze_dir and bronze.RUN_DIR_RE.match(parent.name):
            if not prune._no_symlinked_dir_between(parent, path):
                raise SystemExit(
                    f"refusing to touch path reached via a symlinked dir: "
                    f"{path}")
            return
    raise SystemExit(f"refusing to touch path outside a run dir: {path}")


def _same_bytes(a: Path, b: Path) -> bool:
    ha, _ = bronze.sha256_file(a)
    hb, _ = bronze.sha256_file(b)
    return ha == hb


# Copy-on-write clone invocation, per platform: BSD cp `-c` (clonefile on
# macOS/APFS) vs GNU cp `--reflink=always` (btrfs / XFS on Linux). Both fail
# loudly on a filesystem that can't reflink rather than silently making a
# full copy, so the caller knows to fall back to --strategy hardlink.
_CLONE_CMD = (["cp", "-c"] if sys.platform == "darwin"
              else ["cp", "--reflink=always"])


def _clone_copy(src: Path, dst: Path) -> None:
    r = subprocess.run(_CLONE_CMD + [str(src), str(dst)],
                       capture_output=True, text=True)
    if r.returncode != 0:
        raise RuntimeError(
            f"copy-on-write clone failed (`{' '.join(_CLONE_CMD)}`): "
            f"{r.stderr.strip()} — this filesystem may not support reflinks; "
            f"use --strategy hardlink instead")


def _relink(canonical: Path, dup: Path, strategy: str) -> None:
    """Point ``dup`` at ``canonical``'s content atomically. hardlink →
    one inode; clone → a copy-on-write reflink, byte-verified."""
    tmp = dup.with_name(dup.name + ".dedup-tmp")
    tmp.unlink(missing_ok=True)
    try:
        if strategy == "hardlink":
            os.link(canonical, tmp)
        else:
            _clone_copy(canonical, tmp)
            if not _same_bytes(tmp, canonical):
                raise RuntimeError(f"clone of {canonical} did not verify")
        os.replace(tmp, dup)
    except BaseException:
        tmp.unlink(missing_ok=True)
        raise


def run(bronze_dir: Path, *, dry_run: bool, strategy: str,
        min_age_hours: float, min_size: int) -> tuple[int, int]:
    """Execute (or preview) the sweep for one subtree. Returns
    ``(reclaimed_bytes, dup_file_count)``; prints per-group lines and a
    summary."""
    min_age_s = min_age_hours * 3600.0
    groups, skipped = plan(bronze_dir, min_age_s=min_age_s, min_size=min_size)

    verb = "would share" if dry_run else "sharing"
    reclaimed = 0
    n_dups = 0
    for g in groups:
        rel = g.canonical.relative_to(bronze_dir)
        print(f"{verb}  {len(g.dups)} copy(ies) of {rel}  "
              f"({prune.human_size(g.size)} each, "
              f"reclaim {prune.human_size(g.reclaim)})")
        if dry_run:
            reclaimed += g.reclaim
            n_dups += len(g.dups)
            continue
        freed_inodes: set[int] = set()
        for dup in g.dups:
            _validate(dup, bronze_dir)
            _validate(g.canonical, bronze_dir)
            # Re-confirm identity immediately before replacing (quiescence
            # guard makes drift unlikely, but the window is cheap to close).
            if not _same_bytes(dup, g.canonical):
                print(f"  ...changed since planning; skipping {dup.name}")
                continue
            before = dup.stat().st_ino
            _relink(g.canonical, dup, strategy)
            if dup.stat().st_ino != before:
                n_dups += 1
                # Blocks are freed once per distinct duplicate inode (two
                # paths sharing an inode free it only when the last link is
                # repointed) — count size per inode so the real run matches
                # plan()/--dry-run instead of over-counting shared copies.
                if before not in freed_inodes:
                    freed_inodes.add(before)
                    reclaimed += g.size

    # Summarise skips by reason rather than one line per dump — a whole-tree
    # sweep skips many legacy/no-manifest dumps and the detail is noise.
    if skipped:
        by_reason: dict[str, int] = {}
        for _path, reason in skipped:
            key = ("recently written (possibly in flight)"
                   if reason.startswith("only ") else reason)
            by_reason[key] = by_reason.get(key, 0) + 1
        for reason, count in sorted(by_reason.items(), key=lambda kv: -kv[1]):
            print(f"skipping  {count} dump(s): {reason}")

    if not groups:
        print("no byte-identical duplicates to share")
    else:
        print(f"{'would reclaim' if dry_run else 'reclaimed'}: "
              f"{prune.human_size(reclaimed)} "
              f"({n_dups} duplicate file(s), {len(groups)} group(s), "
              f"strategy={strategy})")
    return reclaimed, n_dups


# ---------------------------------------------------------------------------
# CLI — a single host-side sweep over every collector subtree
# ---------------------------------------------------------------------------

def _collector_subtrees(data_dir: Path, source: str | None):
    """Immediate child dirs of ``data_dir`` that hold >=1 timestamped run
    dir — i.e. collector bronze trees. Restricted to ``source`` when given.
    Dedup runs per subtree, so a file is never linked across collectors."""
    def _is_tree(p: Path) -> bool:
        return (p.is_dir() and not p.is_symlink()
                and any(bronze.iter_run_dirs(p)))

    if source is not None:
        cand = data_dir / source
        return [cand] if _is_tree(cand) else []
    return [c for c in sorted(data_dir.iterdir()) if _is_tree(c)]


def main(argv=None) -> int:
    p = argparse.ArgumentParser(
        prog="wealthdb-collect dedup",
        description="Reclaim local disk by sharing byte-identical bronze "
                    "files (hardlink by default; --strategy clone for "
                    "copy-on-write reflinks where the filesystem supports "
                    "them). Complete, quiescent dumps only; transparent to "
                    "load; idempotent (hardlink), so safe to run in "
                    "orchestration between download and load.",
        formatter_class=argparse.RawDescriptionHelpFormatter,
    )
    # Data-root resolution matches the collector wrappers (host-lib.sh):
    # the fleet-wide WEALTHDB_DATA_ROOT holds every collector's subtree,
    # falling back to the XDG default. The wrappers append /<collector>;
    # dedup sweeps the whole root, so it uses the root itself.
    data_root = os.environ.get("WEALTHDB_DATA_ROOT")
    if data_root:
        default_data = Path(data_root)
    else:
        xdg = os.environ.get("XDG_DATA_HOME") or str(Path.home() / ".local/share")
        default_data = Path(xdg) / "wealthdb"
    p.add_argument("--data-dir", type=Path, default=default_data,
                   help="Parent dir holding per-collector bronze subtrees "
                        "(default: $WEALTHDB_DATA_ROOT, else "
                        "$XDG_DATA_HOME/wealthdb; here %(default)s).")
    p.add_argument("--source", default=None,
                   help="Limit the sweep to one collector subtree.")
    p.add_argument("--strategy", choices=("hardlink", "clone"),
                   default="hardlink",
                   help="hardlink (default: portable across any hard-link "
                        "filesystem, idempotent — safe to re-run) or clone "
                        "(copy-on-write reflink where supported — "
                        "btrfs/XFS/APFS; a deliberate one-shot, re-running "
                        "reports already-freed space).")
    p.add_argument("--dry-run", action="store_true",
                   help="Print the plan and reclaimable bytes; change nothing.")
    p.add_argument("--min-age-hours", type=float, default=1.0,
                   help="Skip dumps touched within this window (default: 1).")
    p.add_argument("--min-size", type=int, default=DEFAULT_MIN_SIZE,
                   help="Ignore files smaller than this many bytes "
                        "(default: %(default)s).")
    args = p.parse_args(argv)

    if not args.data_dir.is_dir():
        raise SystemExit(f"--data-dir does not exist: {args.data_dir}")
    subtrees = _collector_subtrees(args.data_dir, args.source)
    if not subtrees:
        print(f"no collector bronze subtrees under {args.data_dir}")
        return 0

    total_bytes = 0
    total_dups = 0
    for tree in subtrees:
        print(f"\n== {tree.name} ==")
        reclaimed, n = run(tree, dry_run=args.dry_run, strategy=args.strategy,
                           min_age_hours=args.min_age_hours,
                           min_size=args.min_size)
        total_bytes += reclaimed
        total_dups += n
    if len(subtrees) > 1:
        print(f"\n{'would reclaim' if args.dry_run else 'reclaimed'} across "
              f"{len(subtrees)} collector(s): "
              f"{prune.human_size(total_bytes)} ({total_dups} file(s))")
    return 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
