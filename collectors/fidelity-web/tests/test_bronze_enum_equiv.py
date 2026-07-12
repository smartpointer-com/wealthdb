"""Equivalence guard for the bronze run-dir enumeration swap.

`scan_bronze()` used to enumerate dump dirs with a local
`sorted((p for p in iterdir() if p.is_dir() and DUMP_DIR_RE.match(...)),
key=lambda p: p.name)` comprehension; it now defers to
`collectorkit.bronze.iter_run_dirs`. The one subtlety is the sort key:
the original sorted by `p.name`, the helper sorts full `Path`s — for
siblings under a single parent these orders coincide, and this test
pins that. It inlines a byte-for-byte copy of the original and asserts
`scan_bronze` yields the identical ordered list over a mixed fixture
tree, plus that `ts_from_dir` matches the original strptime. Synthetic
slugs only.
"""
from __future__ import annotations

import re
import sys
from datetime import datetime, timezone
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import load  # noqa: E402

# --- verbatim copy of the pre-refactor local logic ------------------
_ORIG_RE = re.compile(r"^\d{8}T\d{6}Z$")


def _orig_scan_bronze(bronze_dir: Path) -> list[Path]:
    if not bronze_dir.is_dir():
        raise SystemExit(f"--bronze-dir does not exist: {bronze_dir}")
    return sorted(
        (p for p in bronze_dir.iterdir()
         if p.is_dir() and _ORIG_RE.match(p.name)),
        key=lambda p: p.name,
    )


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
    "notarun",
    "latest",
    "20240101",
    "20240101T000000Z_bak",
    "020240101T000000Z",
]


def _build_tree(root: Path) -> None:
    for slug in VALID_SLUGS:
        (root / slug).mkdir(parents=True)
    for name in NON_MATCHING_DIRS:
        (root / name).mkdir()
    (root / "20250101T000000Z").write_text("stray", encoding="utf-8")
    (root / "README.txt").write_text("x", encoding="utf-8")


def test_scan_bronze_identical(tmp_path):
    _build_tree(tmp_path)
    assert load.scan_bronze(tmp_path) == _orig_scan_bronze(tmp_path)
    assert [p.name for p in load.scan_bronze(tmp_path)] == sorted(VALID_SLUGS)


def test_ts_from_dir_identical():
    for slug in VALID_SLUGS:
        assert load.ts_from_dir(slug) == _orig_parse_ts(slug)
