"""Guards on bronze run-dir enumeration in `list_pending_dumps()`.

`list_pending_dumps()` defers to `collectorkit.bronze.iter_run_dirs` for
the enumeration and layers its own filters (run.json presence + status)
on top. Two guards:

  * the shared iterator matches a local reference implementation of the
    slug rule over a mixed fixture tree, and
  * the surrounding run.json / status filter drops an in-progress /
    dry-run / manifest-less dump.

Synthetic slugs / ids only.
"""
from __future__ import annotations

import json
import re
import sys
from datetime import datetime, timezone
from pathlib import Path

HERE = Path(__file__).resolve().parent
COLLECTOR = HERE.parent
sys.path.insert(0, str(COLLECTOR))

import load as loader  # noqa: E402
from collectorkit import bronze, silver  # noqa: E402

# --- reference implementation of the run-dir slug rule --------------
_ORIG_RE = re.compile(r"^\d{8}T\d{6}Z$")


def _orig_enumerate(bronze_dir: Path) -> list[Path]:
    out = []
    for d in sorted(bronze_dir.iterdir()):
        if not d.is_dir() or not _ORIG_RE.match(d.name):
            continue
        out.append(d)
    return out


def _orig_parse_ts(name: str) -> int:
    assert _ORIG_RE.match(name)
    dt = datetime.strptime(name, "%Y%m%dT%H%M%SZ").replace(tzinfo=timezone.utc)
    return int(dt.timestamp())


VALID_SLUGS = [
    "20240301T120000Z",
    "20240101T000000Z",
    "20241231T235959Z",
    "20240101T000001Z",
]
NON_MATCHING_DIRS = [
    "notarun", "latest", "20240101", "20240101T000000Z_bak",
    "020240101T000000Z",
]


def _build_tree(root: Path) -> None:
    for slug in VALID_SLUGS:
        (root / slug).mkdir(parents=True)
    for name in NON_MATCHING_DIRS:
        (root / name).mkdir()
    (root / "20250101T000000Z").write_text("stray", encoding="utf-8")
    (root / "README.txt").write_text("x", encoding="utf-8")


def test_enumeration_identical(tmp_path):
    _build_tree(tmp_path)
    assert list(bronze.iter_run_dirs(tmp_path)) == _orig_enumerate(tmp_path)


def test_parse_ts_identical():
    for slug in VALID_SLUGS:
        assert bronze.parse_run_ts(slug) == _orig_parse_ts(slug)


def _run_json(root: Path, slug: str, status) -> None:
    d = root / slug
    d.mkdir(parents=True)
    obj = {} if status is None else {"status": status}
    (d / "run.json").write_text(json.dumps(obj), encoding="utf-8")


def test_status_filter_preserved(tmp_path):
    """The run.json + status guard around the shared iterator drops
    incomplete dumps."""
    bronze_dir = tmp_path / "bronze"
    bronze_dir.mkdir()
    _run_json(bronze_dir, "20240101T000000Z", "complete")
    _run_json(bronze_dir, "20240102T000000Z", "in-progress")
    _run_json(bronze_dir, "20240103T000000Z", "dry-run")
    _run_json(bronze_dir, "20240104T000000Z", None)          # statusless: kept
    (bronze_dir / "20240105T000000Z").mkdir()                # no run.json: skip

    conn = loader.open_db(tmp_path / "relevate.db")
    silver.apply_migrations(conn, loader.MIGRATIONS_DIR)
    pending = loader.list_pending_dumps(conn, bronze_dir)
    conn.close()

    assert [p.name for p in pending] == [
        "20240101T000000Z", "20240104T000000Z",
    ]
