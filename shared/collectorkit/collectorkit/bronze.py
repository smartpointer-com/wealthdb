"""Bronze-artifact helpers shared by collector download.py.

UTC-timestamped run directories, atomic writes (tmp + rename so an
interrupted run never leaves a half-written artifact), and the
canonical-JSON form used for content-based dedup.
"""
from __future__ import annotations

import hashlib
import json
import os
import re
from datetime import datetime, timezone
from pathlib import Path

# A bronze run directory is named with a UTC timestamp, e.g. 20260529T071530Z.
RUN_DIR_RE = re.compile(r"^\d{8}T\d{6}Z$")


def ts_slug(now: datetime | None = None) -> str:
    """UTC slug for a bronze run dir, e.g. `20260529T071530Z`."""
    return (now or datetime.now(timezone.utc)).strftime("%Y%m%dT%H%M%SZ")


def parse_run_ts(name: str) -> int:
    """Parse a run-dir slug (`YYYYmmddTHHMMSSZ`) to Unix epoch seconds (UTC).

    Inverse of `ts_slug`; raises ValueError if the slug doesn't match the
    expected format. Every silver loader uses this to stamp records with
    the bronze run's timestamp."""
    dt = datetime.strptime(name, "%Y%m%dT%H%M%SZ").replace(tzinfo=timezone.utc)
    return int(dt.timestamp())


def run_dir(dest: Path, slug: str | None = None) -> Path:
    """`<dest>/<slug>` (slug defaults to a fresh UTC timestamp)."""
    return Path(dest) / (slug or ts_slug())


def ensure_writable_dir(path: Path) -> Path:
    """Validate that `path` exists and is writable (used for --dest before
    a download begins, so a late write failure can't lose fetched data)."""
    path = Path(path)
    if not path.is_dir():
        raise SystemExit(f"Destination directory does not exist: {path}")
    if not os.access(path, os.W_OK):
        raise SystemExit(f"Destination directory is not writable: {path}")
    return path


def iter_run_dirs(dest: Path):
    """Yield the timestamped bronze run dirs under `dest`, sorted."""
    dest = Path(dest)
    if not dest.is_dir():
        return
    for child in sorted(dest.iterdir()):
        if child.is_dir() and RUN_DIR_RE.match(child.name):
            yield child


def atomic_write_bytes(path: Path, data: bytes) -> None:
    """Write `data` to `path` via a sibling .tmp file + rename."""
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.write_bytes(data)
    tmp.rename(path)


def atomic_write_json(path: Path, obj) -> None:
    """Pretty-print `obj` as JSON (sorted keys) and write it atomically."""
    text = json.dumps(obj, indent=2, sort_keys=True, default=str) + "\n"
    atomic_write_bytes(Path(path), text.encode("utf-8"))


def canonical_json(obj) -> str:
    """Stable, compact JSON for content-based dedup (sorted keys, no
    spaces). `default=str` lets non-JSON-native scalars a silver
    payload may carry — Decimal, date/datetime — serialise as their
    string form rather than raising, matching the local copies the
    collectors used before adopting this helper.
    """
    return json.dumps(obj, sort_keys=True, separators=(",", ":"),
                      ensure_ascii=False, default=str)


def sha256_file(path: Path, chunk_size: int = 1 << 20) -> tuple[str, int]:
    """Return (hex sha256, byte size) for a file, read in chunks so
    large bronze blobs don't load into memory. Callers that only
    want the digest take ``[0]``.
    """
    h = hashlib.sha256()
    size = 0
    with Path(path).open("rb") as f:
        while True:
            chunk = f.read(chunk_size)
            if not chunk:
                break
            h.update(chunk)
            size += len(chunk)
    return h.hexdigest(), size
