#!/usr/bin/env python3
"""
Compact the refdata bronze tree: give the files of the complete runs
the form download stores them in.

- trim: each filing archive the dividend calendar stored whole keeps
  only the documents the calendar reads (sec_calendar.trimmed()),
  compressed. The archive is replaced only once its documents read the
  same, byte for byte, as the whole archive's.
- compress: each plain text file a provider stored (JSON, HTML, CSV,
  XML) is replaced by a zstd-compressed copy, with collectorkit
  recompress's engine. meta.json, run.json, the inbox's copies and the
  `--debug` traces stay as they are.

No step removes anything a parser reads: load reads either form of a
file, so a `load --force` rebuild gives the same silver.

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
import io
import logging
import os
import sys
import time
from dataclasses import dataclass, field
from pathlib import Path

import zstandard
from collectorkit import bronze, cli, compress, prune, recompress
from collectorkit.prune import human_size

from prune import CONFIG as PRUNE_CONFIG
from providers import sec_calendar
from providers.base import META, TEXT_SUFFIXES, ProviderError, read_meta

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


def archives(run_dir: Path) -> list:
    """(path, primary document, whether to keep it whole) of each plain
    filing archive of a complete run, <accession>-xbrl.zip, that a
    request's meta.json names in its `calendar`, with the filing's
    primary document (None where the filing names none, as download
    reads it). An archive two filings name with two primary documents
    is kept whole."""
    found = {}
    manifest = bronze.read_manifest(run_dir / "run.json") or {}
    for e in manifest.get("requests") or ():
        if e.get("status") != "ok" or not e.get("dir"):
            continue
        folder = run_dir / e["dir"]
        try:
            calendar = read_meta(folder).get("calendar")
        except (ProviderError, OSError, ValueError, AttributeError):
            continue
        for f in (calendar.get("filings") or ()) if isinstance(calendar, dict) else ():
            name = f.get("file") if isinstance(f, dict) else None
            path = folder / name if isinstance(name, str) else None
            if path is not None and path.name.endswith("-xbrl.zip") and path.is_file() and not path.is_symlink():
                found.setdefault(path, set()).add(f.get("document"))
    return [(path, next(iter(primaries)), False) if len(primaries) == 1 else (path, None, True)
            for path, primaries in found.items()]


def trim_archives(bronze_dir: Path, targets: list, *, dry_run: bool) -> Tally:
    """Each archive of `targets` (archives()) as <name>.zst with only the
    documents the calendar reads (sec_calendar.trim()): one whose
    documents read otherwise trimmed, or that does not trim, or that two
    filings name with two primary documents, is kept whole, compressed,
    and a warning names it. The kept file is written and verified beside the archive,
    which is removed last; an archive that fails to be written stays as
    it is, and a warning names it."""
    tally = Tally()
    for path, primary, whole in targets:
        rel = path.relative_to(bronze_dir)
        log.debug("trim %s", rel)
        data = path.read_bytes()
        if whole:
            log.warning("%s: named with two primary documents; kept whole", rel)
            kept = data
        else:
            kept, _ = sec_calendar.trim(data, str(rel), primary)
        if dry_run:
            tally.add(path)
            tally.after += packed_size(io.BytesIO(kept))
            continue
        recompress.validate_target(path, bronze_dir)
        st = path.stat()
        work = path.with_name(path.name + ".trim")
        final = path.with_name(path.name + compress.ZSTD_SUFFIX)
        try:
            bronze.atomic_write_bytes(work, kept)
            os.utime(work, (st.st_atime, st.st_mtime))
            os.replace(compress.compress_file(work, level=CONFIG.level), final)
        except (OSError, RuntimeError) as e:
            log.warning("%s: not compacted: %s", rel, e)
            continue
        finally:
            work.unlink(missing_ok=True)
        tally.add(path)
        path.unlink()
        tally.after += final.lstat().st_size
    return tally


def packed_size(src) -> int:
    """The bytes compress_file stores a stream in."""
    return zstandard.ZstdCompressor(level=CONFIG.level).copy_stream(src, _Sink(), read_size=CHUNK)[1]


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
                tally.after += packed_size(src)
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
    """Plans every step from the runs eligible at the start, as a step's
    writes make a run look young to the next; then runs them in order."""
    if not bronze_dir.is_dir():
        raise SystemExit(f"--bronze-dir does not exist: {bronze_dir}")
    min_age_s = min_age_hours * 3600.0
    eligible = {e.run_dir for e in prune.iter_run_eligibility(bronze_dir, PRUNE_CONFIG, min_age_s, time.time())
                if e.verdict == prune.ELIGIBLE}
    texts, skipped = recompress.plan_recompress(bronze_dir, CONFIG, min_age_s)
    texts = [t for t in texts if provider_file(t["path"], bronze_dir) and run_of(t["path"], bronze_dir) in eligible]
    zips = [a for run_dir in sorted(eligible) for a in archives(run_dir)]
    for s in skipped:
        age = f"; {s['age_s'] / 60:.0f} min old" if s["age_s"] is not None else ""
        print(f"skipping  {s['path'].relative_to(bronze_dir)}/  [{s['reason']}{age}]")
    steps = [("trim", trim_archives(bronze_dir, zips, dry_run=dry_run)),
             ("compress", compress_text(bronze_dir, texts, dry_run=dry_run))]
    for name, tally in steps:
        print(tally.line(name, dry_run))
    if not dry_run and any(tally.files for _, tally in steps):
        print("a `load --force` rebuild gives the same silver")
    return 0


def run_of(path: Path, bronze_dir: Path) -> Path:
    """The run dir a file of the bronze tree is in."""
    return bronze_dir / path.relative_to(bronze_dir).parts[0]


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
