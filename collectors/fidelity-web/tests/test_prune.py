"""
Unit tests for what fidelity-web's prune config decides; the shared
engine (age guard, symlinks, unreadable manifests, --dry-run, target
checks) is tested in collectorkit. The tests drive the collector's
`prune.main` and `CONFIG`.

tmp_path bronze trees. Covers:
  * complete dump: screenshots/ deleted, load inputs untouched
  * no run.json: a non-complete dump, deleted whole once quiescent
  * statusless manifest: classified COMPLETE (lenient), inputs kept
"""

from __future__ import annotations

import json
import os
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import prune  # noqa: E402


# ============================================================
# Fixtures
# ============================================================

OLD_TS = "20260101T010000Z"          # older than any guard by slug
STALE_S = 3 * 3600                    # comfortably past the 1h default


def make_dump(root: Path, slug: str, status: str | None = "complete",
              screenshots: bool = True, run_json: bool = True,
              age_s: float = 0.0) -> Path:
    """Build a bronze run dir. ``age_s`` backdates every file/dir
    mtime (and the run dir's own) to now-age_s, so the write-activity
    guard sees an abandoned dump; the default (0) leaves it fresh."""
    d = root / slug
    (d / "positions").mkdir(parents=True)
    (d / "positions" / "positions_summary.csv").write_text("a,b\n1,2\n")
    (d / "documents").mkdir()
    (d / "documents" / "Statement_Jan_March_2026.pdf").write_bytes(
        b"%PDF-1.4 fake")
    if screenshots:
        shots = d / "screenshots"
        shots.mkdir()
        (shots / "20260101T010101Z-positions-landed.html").write_text(
            "<html>dump</html>")
        (shots / "20260101T010101Z-positions-landed.png").write_bytes(
            b"\x89PNG fake")
    if run_json:
        (d / "run.json").write_text(json.dumps(
            {"status": status} if status is not None else {}))
    if age_s:
        backdate(d, age_s)
    return d


def backdate(path: Path, age_s: float) -> None:
    t = time.time() - age_s
    for p in list(path.rglob("*")) + [path]:
        os.utime(p, (t, t), follow_symlinks=False)


def run_main(root: Path, *extra: str) -> int:
    return prune.main(["--bronze-dir", str(root), *extra])


# ============================================================
# Complete dumps: screenshots pruned, inputs kept
# ============================================================

def test_complete_dump_screenshots_pruned_inputs_kept(tmp_path):
    d = make_dump(tmp_path, OLD_TS)
    assert run_main(tmp_path) == 0
    assert not (d / "screenshots").exists()
    assert (d / "positions" / "positions_summary.csv").exists()
    assert (d / "documents" / "Statement_Jan_March_2026.pdf").exists()
    assert (d / "run.json").exists()


# ============================================================
# Non-complete dumps: deleted once quiescent
# ============================================================

def test_missing_run_json_dump_deleted(tmp_path):
    d = make_dump(tmp_path, OLD_TS, run_json=False, age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


def test_statusless_manifest_kept_as_legacy_complete(tmp_path):
    # A run.json with no `status` key predates the status lifecycle: such
    # a dump only ever got a run.json at the end, so its presence means
    # the dump finished — classify COMPLETE, keep its load inputs, prune
    # only its debug captures.
    d = make_dump(tmp_path, OLD_TS, status=None, screenshots=True,
                  age_s=STALE_S)
    run_main(tmp_path)
    assert d.exists()
    assert not (d / "screenshots").exists()
    assert (d / "positions" / "positions_summary.csv").exists()


# ============================================================
# Target validation
# ============================================================

def test_validate_target_accepts_the_debug_subdir(tmp_path):
    (tmp_path / OLD_TS / "screenshots").mkdir(parents=True)
    prune.CONFIG.validate_target(tmp_path / OLD_TS / "screenshots", tmp_path)
