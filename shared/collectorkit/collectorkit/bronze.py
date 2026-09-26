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

from .silver import loaded_snapshots

# Single implementation lives in silver; re-exported here so
# download-side callers get it without touching the silver module.
from .silver import canonical_json  # noqa: F401

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


def run_dir(bronze_dir: Path, slug: str | None = None) -> Path:
    """`<bronze-dir>/<slug>` (slug defaults to a fresh UTC timestamp)."""
    return Path(bronze_dir) / (slug or ts_slug())


def ensure_writable_dir(path: Path) -> Path:
    """Validate that `path` exists and is writable (used for --bronze-dir before
    a download begins, so a late write failure can't lose fetched data)."""
    path = Path(path)
    if not path.is_dir():
        raise SystemExit(f"Destination directory does not exist: {path}")
    if not os.access(path, os.W_OK):
        raise SystemExit(f"Destination directory is not writable: {path}")
    return path


def iter_run_dirs(bronze_dir: Path):
    """Yield the timestamped bronze run dirs under `bronze_dir`, sorted."""
    bronze_dir = Path(bronze_dir)
    if not bronze_dir.is_dir():
        return
    for child in sorted(bronze_dir.iterdir()):
        if child.is_dir() and RUN_DIR_RE.match(child.name):
            yield child


def run_status(run_json_path: Path) -> str | None:
    """The ``status`` field of a bronze run's ``run.json``, or ``None`` when
    the manifest is unreadable/corrupt or carries no ``status`` key.

    Silver loaders use this to decide loadability: ``download`` stamps
    ``status="in-progress"`` at run-dir creation and atomically overwrites it
    with ``"complete"`` / ``"dry-run"`` at the end, so a present status other
    than ``"complete"`` marks a crashed / still-running / dry-run dump whose
    partial artefacts must not be ingested. A statusless manifest predates the
    lifecycle and stays loadable (its mere presence historically meant the walk
    finished); an unreadable/corrupt manifest also returns ``None`` so the
    caller keeps it pending and surfaces any real error at load time rather
    than skipping it silently here.
    """
    try:
        meta = json.loads(run_json_path.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None
    return meta.get("status") if isinstance(meta, dict) else None


# Statuses a run.json carries while its dump is not one to load: the walk is
# still running (or crashed mid-walk), or it was a dry run.
INCOMPLETE_RUN_STATUSES = ("in-progress", "dry-run")


def pending_run_dirs(conn, bronze_dir: Path, *, log) -> list[Path]:
    """The run dirs under `bronze_dir` that silver has not loaded, oldest
    first.

    A run dir without a ``run.json`` is still being written, and one whose
    status is in :data:`INCOMPLETE_RUN_STATUSES` never finished; both are
    skipped with a line on `log`. A statusless or unreadable manifest stays
    loadable (see :func:`run_status`).
    """
    if not Path(bronze_dir).is_dir():
        return []
    loaded = loaded_snapshots(conn)
    pending: list[Path] = []
    for d in iter_run_dirs(bronze_dir):
        if parse_run_ts(d.name) in loaded:
            continue
        run_json = d / "run.json"
        if not run_json.is_file():
            log.info("skipping %s — no run.json (still writing?)", d.name)
            continue
        status = run_status(run_json)
        if status in INCOMPLETE_RUN_STATUSES:
            log.info("skipping %s — run.json status=%s", d.name, status)
            continue
        pending.append(d)
    return pending


def short_token(value: str, length: int = 16) -> str:
    """A filename-safe short id for an opaque source token.

    The first `length` hex chars of its SHA-256. A hash rather than a
    slice because source tokens routinely share a long prefix — every
    account id under one login can begin with the same customer and
    depot segments — so slicing collides silently, giving two accounts
    one filename and losing one of them.

    Stable across runs, so a re-download of the same entity lands on the
    same name.
    """
    return hashlib.sha256(value.encode("utf-8")).hexdigest()[:length]


def atomic_write_bytes(path: Path, data: bytes) -> None:
    """Write `data` to `path` via a sibling .tmp file + rename, owner-only.

    The mode is stamped on the TMP file, before the rename, so the artefact
    is never briefly readable under its final name — the same ordering
    `session.save_state` uses. The package umask already covers a file
    created here; this also narrows one written before that landed.
    """
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(path.suffix + ".tmp")
    tmp.write_bytes(data)
    tmp.chmod(0o600)
    tmp.rename(path)


def atomic_write_json(path: Path, obj) -> None:
    """Pretty-print `obj` as JSON (sorted keys) and write it atomically."""
    text = json.dumps(obj, indent=2, sort_keys=True, default=str) + "\n"
    atomic_write_bytes(Path(path), text.encode("utf-8"))




# Process-lifetime memo for sha256_file, keyed by file identity + version
# (device, inode, size, mtime_ns) → (hexdigest, size). Bronze trees hardlink
# identical artefacts across run dirs and a single load re-references the same
# file many times, so without this the same content is hashed repeatedly per
# run. Including st_mtime_ns means an in-place rewrite (new mtime) busts the
# entry and forces a re-hash rather than serving a stale digest; the digest of
# unchanged content is always correct.
_SHA256_MEMO: dict[tuple[int, int, int, int], tuple[str, int]] = {}


def sha256_file(path: Path, chunk_size: int = 1 << 20) -> tuple[str, int]:
    """Return (hex sha256, byte size) for a file, read in chunks so
    large bronze blobs don't load into memory. Callers that only
    want the digest take ``[0]``.

    Results are memoised for the process lifetime keyed by file identity
    and version (device, inode, size, mtime_ns): hardlinked duplicates and
    repeat references within one load return the cached tuple without
    re-reading the file. The mtime in the key means rewriting a file in
    place re-hashes it. ``chunk_size`` only affects the cold-path read, so
    a memo hit is transparent regardless of the size a later caller passes.
    """
    st = os.stat(path)
    key = (st.st_dev, st.st_ino, st.st_size, st.st_mtime_ns)
    cached = _SHA256_MEMO.get(key)
    if cached is not None:
        return cached
    h = hashlib.sha256()
    size = 0
    with Path(path).open("rb") as f:
        while True:
            chunk = f.read(chunk_size)
            if not chunk:
                break
            h.update(chunk)
            size += len(chunk)
    result = (h.hexdigest(), size)
    _SHA256_MEMO[key] = result
    return result
