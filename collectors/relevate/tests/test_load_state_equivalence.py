"""Equivalence proof for the P14 refactor: download.load_state now
delegates to collectorkit.session.load_state instead of carrying its own
inline reader. This asserts the delegating version returns the identical
value to the ORIGINAL inline implementation (reproduced below) across a
battery of inputs — missing file, valid JSON, corrupt JSON, empty file,
non-dict JSON, and the pathological directory-at-path case.

The original and the shared helper differ only in two non-return-value
respects (a warning-log side effect on a non-file path, and the warning
text/logger on the error path); both produce the identical return value
for every input, which is what callers consume.
"""
from __future__ import annotations

import json
import logging
import sys
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import download  # noqa: E402


# Verbatim copy of download.load_state as it stood BEFORE the P14 refactor,
# used purely as the equivalence oracle.
def original_load_state(path: Path):
    logger = logging.getLogger("download")
    if not path.exists():
        return None
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        logger.warning("could not load state %s: %s", path, exc)
        return None


def _cases(tmp_path: Path) -> list[Path]:
    missing = tmp_path / "missing.json"

    valid = tmp_path / "valid.json"
    valid.write_text(
        json.dumps({"minted_at": "2026-07-12T00:00:00+00:00",
                    "cookies": [{"name": "AL_SESS-S", "value": "x"}]}),
        encoding="utf-8",
    )

    corrupt = tmp_path / "corrupt.json"
    corrupt.write_text("{not valid json", encoding="utf-8")

    empty = tmp_path / "empty.json"
    empty.write_text("", encoding="utf-8")

    non_dict = tmp_path / "list.json"
    non_dict.write_text(json.dumps([1, 2, 3]), encoding="utf-8")

    a_dir = tmp_path / "adir.json"
    a_dir.mkdir()

    nested_missing = tmp_path / "nope" / "deep.json"

    return [missing, valid, corrupt, empty, non_dict, a_dir, nested_missing]


def test_delegation_matches_original(tmp_path):
    for path in _cases(tmp_path):
        assert download.load_state(path) == original_load_state(path), path


def test_valid_roundtrip_value(tmp_path):
    # Pin the happy-path return so the oracle itself can't silently drift.
    p = tmp_path / "state.json"
    payload = {"minted_at": "2026-07-12T00:00:00+00:00", "cookies": []}
    p.write_text(json.dumps(payload), encoding="utf-8")
    assert download.load_state(p) == payload


def test_missing_and_directory_return_none(tmp_path):
    assert download.load_state(tmp_path / "missing.json") is None
    d = tmp_path / "d.json"
    d.mkdir()
    assert download.load_state(d) is None
