"""Bronze helpers shared by collectors.

UTC-timestamped run directories, the one classification of a run that
loaders and prune share (:func:`classify_run`), atomic writes (tmp +
rename so an interrupted run never leaves a half-written artifact), and
the canonical-JSON form used for content-based dedup.
"""
from __future__ import annotations

import hashlib
import json
import logging
import os
import re
from collections.abc import Callable, Iterable
from datetime import datetime, timezone
from pathlib import Path

from .silver import loaded_snapshots

# Single implementation lives in silver; re-exported here so
# download-side callers get it without touching the silver module.
from .silver import canonical_json  # noqa: F401

log = logging.getLogger("collectorkit.bronze")

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


def read_manifest(path: Path) -> dict | None:
    """The JSON object in the run manifest at `path`, or ``None`` when the
    file is absent or unreadable, or holds no JSON object."""
    try:
        meta = json.loads(Path(path).read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None
    return meta if isinstance(meta, dict) else None


def run_status(run_json_path: Path) -> str | None:
    """The ``status`` field of a bronze run's ``run.json``, or ``None`` when
    the manifest is absent, unreadable/corrupt or carries no ``status``
    key. A loader tells an absent manifest from a statusless one with
    :func:`run_is_loadable`.

    ``download`` stamps ``status="in-progress"`` at run-dir creation and
    atomically overwrites it with ``"complete"`` / ``"dry-run"`` at the
    end, so a present status other than ``"complete"`` marks a crashed /
    still-running / dry-run dump whose partial artefacts must not be
    ingested (:func:`is_loadable_status`).
    """
    return (read_manifest(run_json_path) or {}).get("status")


# The run.json status of a finished dump.
COMPLETE_STATUS = "complete"


def is_loadable_status(status: str | None) -> bool:
    """Whether a run with this run.json ``status`` is a load input.

    ``"complete"`` is, and so is no status at all. Any other status is
    not: ``"in-progress"`` from a walk that is running or crashed,
    ``"dry-run"``, or a status this code does not know. It is the rule
    :func:`status_classification` applies to a present status, so a run
    a loader skips is a run ``prune`` reclaims.
    """
    return status is None or status == COMPLETE_STATUS


# The three answers to "is this run a finished dump?", from
# :func:`classify_run` and from every completeness predicate.
COMPLETE = "complete"          # keep load inputs; prune only debug artefacts
NON_COMPLETE = "non-complete"  # whole-dir deletion candidate (once quiescent)
UNKNOWN = "unknown"            # manifest unreadable/corrupt — never a candidate

# A predicate returns (state, reason); reason is surfaced in the output.
Classification = tuple  # (state: str, reason: str)


def status_classification(
    meta: dict | None,
    *,
    run_dir: Path | None = None,
    legacy_complete: Callable[[Path | None, dict | None], bool] | None = None,
) -> Classification:
    """Classify a dump from the normalized ``status`` field, with a
    per-collector legacy fallback.

    The convention every ``download`` writes: ``status`` is
    ``"in-progress"`` while the walk runs, then atomically overwritten
    with ``"complete"`` / ``"dry-run"`` at the end.
    So ``status == "complete"`` ⇒ COMPLETE, any other present status
    (``"in-progress"``, ``"dry-run"``, …) ⇒ NON_COMPLETE.

    A dump can carry no ``status`` field at all. For such a statusless
    dump, ``legacy_complete(run_dir, meta)`` decides: it inspects the
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
    when the collector's terminal completeness signal is present, so it
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
    if status is not None:
        # The loaders' rule, so a run they skip is one prune may reclaim.
        if is_loadable_status(status):
            return COMPLETE, "complete"
        return NON_COMPLETE, f"status={status!r}"
    # Statusless legacy dump: defer to the collector's original signal.
    if legacy_complete is not None and legacy_complete(run_dir, meta):
        return COMPLETE, "complete (legacy dump, no status field)"
    return NON_COMPLETE, "no status field; legacy completeness signal absent"


def _lenient_legacy_complete(run_dir: Path | None, meta: dict | None) -> bool:
    """Generic, conservative completeness: a readable manifest (with or
    without a ``status`` field) means the dump finished. Missing / unreadable
    / non-``complete``-status dumps fail this, which for the reclaim engines
    (dedup / docdedup / recompress) is always safe — skipping a run just means
    a document is re-fetched or a byte-dup is left unshared, never data loss."""
    return meta is not None


def lenient_classification(run_dir: Path | None,
                           meta: dict | None) -> Classification:
    """Completeness classification for the reclaim engines: the normalized
    ``status`` field with the lenient legacy fallback (a readable statusless
    manifest counts as complete). Shared so dedup / docdedup / recompress can
    never disagree about what "complete" means.

    It is also the ``is_complete`` predicate of a collector whose manifest
    is written once at the end or carries a status, and the default of
    :func:`classify_run`."""
    return status_classification(
        meta, run_dir=run_dir, legacy_complete=_lenient_legacy_complete)


def classify_run(
    run_dir: Path,
    is_complete: Callable[[Path, dict | None], Classification] = lenient_classification,
    *,
    manifest_name: str | None = "run.json",
) -> Classification:
    """``(state, reason)`` for a run dir: the one answer to "is this run a
    finished dump?".

    Reads the manifest (when `manifest_name` names one) and enforces the
    UNKNOWN-never-deleted invariant *before* the predicate runs: a
    manifest that is present but unreadable (I/O / permission error) or
    unparseable (bytes that are not UTF-8 JSON) short-circuits to UNKNOWN.
    Only a cleanly read manifest (or a cleanly absent one) reaches
    `is_complete`. A predicate that returns any other state yields UNKNOWN,
    so a misbehaving one fails safe.
    """
    meta: dict | None = None
    if manifest_name:
        path = Path(run_dir) / manifest_name
        try:
            raw = path.read_bytes()
        except FileNotFoundError:
            meta = None
        except OSError as e:
            return UNKNOWN, f"{manifest_name} unreadable ({e.__class__.__name__})"
        else:
            try:
                meta = json.loads(raw.decode("utf-8"))
            except ValueError:  # undecodable bytes or invalid JSON
                return UNKNOWN, f"{manifest_name} is not valid JSON (corrupt?)"
            if not isinstance(meta, dict):
                return UNKNOWN, f"{manifest_name} is not a JSON object"
    state, reason = is_complete(run_dir, meta)
    if state not in (COMPLETE, NON_COMPLETE, UNKNOWN):
        return UNKNOWN, f"predicate returned unknown state {state!r}"
    return state, reason


def run_skip_reason(run_dir: Path) -> str | None:
    """Why a loader skips `run_dir`, or ``None`` when it reads it.

    A loader reads a run whose ``run.json`` is a file and that
    :func:`classify_run` does not call NON_COMPLETE. A run dir without a
    manifest is still being written, or its download crashed before the
    first manifest write; ``prune`` reclaims it, so no loader reads it
    either. An UNKNOWN run (an unreadable or corrupt manifest) stays
    loadable, so the load surfaces the error.
    """
    if not (Path(run_dir) / "run.json").is_file():
        return "no run.json (still writing?)"
    state, reason = classify_run(run_dir)
    return f"run.json {reason}" if state == NON_COMPLETE else None


def run_is_loadable(run_dir: Path) -> bool:
    """Whether a loader reads `run_dir`: :func:`run_skip_reason` gives no
    reason to skip it."""
    return run_skip_reason(run_dir) is None


def manifest_is_complete(run_dir: Path, manifest_name: str = "run.json") -> bool:
    """Whether `run_dir` holds a finished dump: :func:`classify_run` calls
    it COMPLETE.

    A missing or unreadable manifest is not complete: the download that
    writes it crashed or is still running. A statusless one is, since a
    download that writes no status writes its manifest only at the end.
    """
    return classify_run(run_dir, manifest_name=manifest_name)[0] == COMPLETE


def pending_run_dirs(conn, bronze_dir: Path, *, log) -> list[Path]:
    """The run dirs under `bronze_dir` that silver has not loaded, oldest
    first.

    A run dir without a ``run.json`` is still being written, and one whose
    status is not loadable (:func:`is_loadable_status`) never finished;
    both are skipped with a line on `log` (:func:`run_skip_reason`), and so
    is a slug that names no real instant (a 30 February). A statusless or
    unreadable manifest stays loadable (see :func:`run_is_loadable`).
    """
    if not Path(bronze_dir).is_dir():
        return []
    loaded = loaded_snapshots(conn)
    pending: list[Path] = []
    for d in iter_run_dirs(bronze_dir):
        try:
            ts = parse_run_ts(d.name)
        except ValueError:
            log.warning("skipping %s — not a valid run timestamp", d.name)
            continue
        if ts in loaded:
            continue
        reason = run_skip_reason(d)
        if reason is not None:
            log.info("skipping %s — %s", d.name, reason)
            continue
        pending.append(d)
    return pending


def record_run_listing(conn, snapshot_at: int, ids: Iterable[str]) -> None:
    """Record the account ids a run's roster listed in `run_accounts`.

    A loader that content-dedups its `accounts` roster keeps no row for an
    unchanged account, so this table is what shows that a run still listed
    it. A repeated id lands once and an empty id is dropped. The caller owns
    the transaction.
    """
    conn.executemany(
        "INSERT OR IGNORE INTO run_accounts (snapshot_at, account_external_id) "
        "VALUES (?,?)",
        [(snapshot_at, ext) for ext in set(ids) if ext])


def backfill_run_listings(conn, bronze_dir: Path,
                          read_ids: Callable[[Path], Iterable[str]]) -> None:
    """Record the listing of each loaded run that has none, from its bronze
    run dir.

    `read_ids(run_dir)` returns the account ids that run's roster listed. A
    run with a recorded listing is left alone, and so is a run dir whose
    slug names no real instant. A loaded run still without a listing
    afterwards (its bronze is gone, or it listed no accounts) is counted in
    one log line. The caller owns the transaction.
    """
    have = {r[0] for r in conn.execute("SELECT DISTINCT snapshot_at FROM run_accounts")}
    missing = loaded_snapshots(conn) - have
    if not missing:
        return
    for d in iter_run_dirs(bronze_dir):
        try:
            snapshot_at = parse_run_ts(d.name)
        except ValueError:
            continue
        if snapshot_at in missing:
            record_run_listing(conn, snapshot_at, read_ids(d))
    left = missing - {r[0] for r in conn.execute("SELECT DISTINCT snapshot_at FROM run_accounts")}
    if left:
        log.info("%d loaded run(s) still have no listing (no accounts in their bronze)",
                 len(left))


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


_UNSAFE_STEM_RE = re.compile(r"[^\w.-]+")


def safe_stem(value, fallback: str = "item") -> str:
    """A filesystem-safe file or dir stem from a source-supplied value (an
    account id, a document id, a statement date).

    Word characters, dot and dash are kept; every other run becomes one
    dash. Leading and trailing dots and dashes are stripped, so no path
    separator, traversal or dotfile survives. An empty result is
    `fallback`. Stable, so a loader can map a name back to its id.
    """
    stem = _UNSAFE_STEM_RE.sub("-", str(value)).strip("-.")
    return stem or fallback


def log_id(value) -> str:
    """An identifier masked for the LOG: an ellipsis and the last four
    characters of its :func:`safe_stem`, whatever its length.

    Terminal output gets pasted into chats and issue threads, and a short
    account number is no less an identifier than a long token. The last
    four still tell a run's accounts apart in its own output, as a
    statement does. Files on disk keep the full stem.
    """
    return "…" + safe_stem(value)[-4:]


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
