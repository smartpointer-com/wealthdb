"""Guard on the bronze run-dir enumeration load.py depends on.

load.py's `main()` enumerates dump directories via
`collectorkit.bronze.iter_run_dirs`. This test carries a local reference
implementation of the slug rule and asserts the shared helper yields the
identical ordered list of paths across a mixed fixture tree (valid run
dirs out of lexical order, a non-matching dir, a stray matching-named
*file*, a look-alike suffix), plus that `bronze.parse_run_ts` matches a
reference strptime for every valid slug. Synthetic slugs only.
"""
from __future__ import annotations

import re
import sys
from datetime import datetime, timezone
from pathlib import Path

HERE = Path(__file__).resolve().parent
COLLECTOR = HERE.parent
sys.path.insert(0, str(COLLECTOR))

from collectorkit import bronze  # noqa: E402

# --- reference implementation of the run-dir slug rule --------------
_ORIG_RE = re.compile(r"^(\d{8}T\d{6}Z)$")


def _orig_enumerate(bronze_dir: Path) -> list[Path]:
    return [
        d for d in sorted(bronze_dir.iterdir())
        if d.is_dir() and _ORIG_RE.match(d.name)
    ]


def _orig_parse_ts(name: str) -> int:
    m = _ORIG_RE.match(name)
    assert m
    dt = datetime.strptime(m.group(1), "%Y%m%dT%H%M%SZ").replace(
        tzinfo=timezone.utc)
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
    "20240101",                 # too short
    "20240101T000000Z_bak",     # valid prefix, extra suffix
    "020240101T000000Z",        # extra leading digit
]


def _build_tree(root: Path) -> None:
    for slug in VALID_SLUGS:
        (root / slug).mkdir(parents=True)
    for name in NON_MATCHING_DIRS:
        (root / name).mkdir()
    # A stray file whose name *would* match, but it is not a directory.
    (root / "20250101T000000Z").write_text("stray", encoding="utf-8")
    # A stray non-matching file.
    (root / "README.txt").write_text("x", encoding="utf-8")


def test_enumeration_identical(tmp_path):
    _build_tree(tmp_path)
    expected = _orig_enumerate(tmp_path)
    actual = list(bronze.iter_run_dirs(tmp_path))
    assert actual == expected
    # And the set is exactly the four valid run dirs, in timestamp order.
    assert [p.name for p in actual] == sorted(VALID_SLUGS)


def test_parse_ts_identical():
    for slug in VALID_SLUGS:
        assert bronze.parse_run_ts(slug) == _orig_parse_ts(slug)


def test_missing_dir_yields_empty(tmp_path):
    assert list(bronze.iter_run_dirs(tmp_path / "nope")) == []
