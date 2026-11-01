#!/usr/bin/env python3
"""
Compact the refdata bronze tree: give the files of the complete runs
the form download stores them in.

It replaces each plain text file a provider stored (JSON, HTML, CSV,
XML) by a zstd-compressed copy, with collectorkit recompress's engine.
meta.json, run.json, the inbox's copies and the `--debug` traces stay
as they are. It removes nothing a parser reads: load reads either form
of a file, so a `load --force` rebuild gives the same silver.

It rewrites load inputs, so it is a manual verb, never scheduled, and it
keeps the reclaim engines' safety rules: complete runs only, as prune
classifies them, each quiet for `--min-age-hours`; symlinks refused; a
file replaced only once its new form reads back the same, so a run
stopped half way is safe to run again. It prints each step's files and
their bytes before and after, a file a hard link shares counted once;
`-v` names each file. `--dry-run` prints the same and writes nothing. A
run it changed is quiet again only after `--min-age-hours`. It must not
run while a download or a load does.

Usage:
    compact.py [--bronze-dir DIR] [--dry-run] [--min-age-hours N]
"""

from __future__ import annotations

import argparse
import logging
import sys
from dataclasses import dataclass, field
from pathlib import Path

import zstandard
from collectorkit import bronze, cli, compress, recompress
from collectorkit.prune import human_size

from prune import CONFIG as PRUNE_CONFIG
from providers.base import META, TEXT_SUFFIXES

# The folders of a run that hold the providers' files:
# <kind>/<provider>/<ref>/…
KINDS = ("funds", "securities", "bulk")
# The providers whose files stay as they are: the inbox's are copies of
# what was placed by hand.
KEPT = frozenset({"inbox"})
CONFIG = recompress.RecompressConfig(
    patterns=tuple(f"{kind}/**/*{suffix}" for kind in KINDS for suffix in sorted(TEXT_SUFFIXES)),
    is_complete=PRUNE_CONFIG.is_complete)
# The read size compress_file streams a file with.
CHUNK = 1 << 20

log = logging.getLogger("refdata.compact")


@dataclass
class Tally:
    """A step's files and their bytes before and after. A file whose
    inode another path shares (a hard link) counts its bytes once."""
    files: int = 0
    before: int = 0
    after: int = 0
    inodes: set = field(default_factory=set)

    def add(self, path: Path) -> None:
        st = path.lstat()
        self.files += 1
        if (st.st_dev, st.st_ino) not in self.inodes:
            self.inodes.add((st.st_dev, st.st_ino))
            self.before += st.st_size

    def line(self, step: str, dry_run: bool) -> str:
        verb, frees = ("would change", "frees") if dry_run else ("changed", "freed")
        return (f"{step}: {verb} {self.files} files, {human_size(self.before)} -> {human_size(self.after)}"
                f" ({frees} {human_size(self.before - self.after)})")


def provider_file(path: Path, bronze_dir: Path) -> bool:
    """Whether a file of a run is one a provider stored: in a request's
    folder (<run>/<kind>/<provider>/<ref>/…), of no provider in KEPT,
    and no meta.json."""
    parts = path.relative_to(bronze_dir).parts
    return len(parts) > 4 and parts[1] in KINDS and parts[2] not in KEPT and path.name != META


def compress_text(bronze_dir: Path, targets: list, *, dry_run: bool) -> Tally:
    """Each plain text file of `targets` (recompress.plan_recompress) as
    its .zst twin. A twin left by a run that stopped is kept where it
    decompresses to the original, else made again."""
    tally = Tally()
    for t in targets:
        path = t["path"]
        tally.add(path)
        log.debug("compress %s%s", path.relative_to(bronze_dir), " [finishing a stopped run]" if t["adopt"] else "")
        if dry_run:
            with path.open("rb") as src:
                tally.after += zstandard.ZstdCompressor(level=CONFIG.level).copy_stream(
                    src, _Sink(), read_size=CHUNK)[1]
            continue
        recompress.validate_target(path, bronze_dir)
        twin = path.with_name(path.name + compress.ZSTD_SUFFIX)
        if t["adopt"] and reads_back(twin, path):
            path.unlink()
        else:
            twin = compress.compress_file(path, level=CONFIG.level)
        tally.after += twin.lstat().st_size
    return tally


def reads_back(twin: Path, original: Path) -> bool:
    """Whether a compressed twin decompresses to its original's bytes; a
    twin that does not decompress does not."""
    try:
        return compress.decompressed_sha256(twin)[0] == bronze.sha256_file(original)[0]
    except zstandard.ZstdError:
        return False


class _Sink:
    """A stream that keeps nothing: a dry run counts the bytes a write
    would store, written as compress_file writes them (CHUNK)."""

    def write(self, data) -> int:
        return len(data)


def run(bronze_dir: Path, *, dry_run: bool, min_age_hours: float) -> int:
    if not bronze_dir.is_dir():
        raise SystemExit(f"--bronze-dir does not exist: {bronze_dir}")
    min_age_s = min_age_hours * 3600.0
    targets, skipped = recompress.plan_recompress(bronze_dir, CONFIG, min_age_s)
    for s in skipped:
        age = f"; {s['age_s'] / 60:.0f} min old" if s["age_s"] is not None else ""
        print(f"skipping  {s['path'].relative_to(bronze_dir)}/  [{s['reason']}{age}]")
    text = compress_text(bronze_dir, [t for t in targets if provider_file(t["path"], bronze_dir)],
                         dry_run=dry_run)
    print(text.line("compress", dry_run))
    if not dry_run and text.files:
        print("a `load --force` rebuild gives the same silver")
    return 0


def main(argv=None) -> int:
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    cli.add_common_args(p)
    p.add_argument("--bronze-dir", type=Path, required=True, help="The data dir: the run dirs' root.")
    p.add_argument("--dry-run", action="store_true", help="Print the plan with its bytes; write nothing.")
    p.add_argument("--min-age-hours", type=float, default=1.0,
                   help="Leave a run touched within this window alone. Default: 1.")
    args = p.parse_args(argv)
    cli.configure_logging(args.verbose)
    return run(args.bronze_dir, dry_run=args.dry_run, min_age_hours=args.min_age_hours)


if __name__ == "__main__":
    sys.exit(main())
