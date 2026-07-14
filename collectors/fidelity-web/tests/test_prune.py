"""
Unit tests for prune.py.

tmp_path bronze trees. Covers:
  * complete dump: screenshots/ deleted, load inputs untouched
  * complete dump without screenshots/: no-op
  * non-complete dumps (absent run.json / status != complete)
    deleted whole once quiescent
  * in-flight guard keyed on write activity, not slug age: a long
    walk (old slug, fresh writes) is protected
  * unreadable / corrupt run.json is UNKNOWN → skipped, never
    deleted (a read failure is not evidence of incompleteness)
  * calendar-invalid slug is skipped, not a crash
  * symlinked screenshots / run dirs are left alone
  * --dry-run deletes nothing
  * non-run entries at the bronze root are never touched
  * validate_target refuses paths outside the expected shapes
"""

from __future__ import annotations

import json
import os
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import prune  # noqa: E402


# ============================================================
# Fixtures
# ============================================================

OLD_TS = "20260101T010000Z"          # older than any guard by slug
STALE_S = 3 * 3600                    # comfortably past the 1h default
FRESH_FMT = "%Y%m%dT%H%M%SZ"


def fresh_slug(age_s: float = 0.0) -> str:
    dt = datetime.fromtimestamp(time.time() - age_s, tz=timezone.utc)
    return dt.strftime(FRESH_FMT)


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


def test_complete_dump_without_screenshots_untouched(tmp_path):
    d = make_dump(tmp_path, OLD_TS, screenshots=False)
    assert run_main(tmp_path) == 0
    assert d.exists()
    assert (d / "positions" / "positions_summary.csv").exists()


def test_fresh_complete_dump_screenshots_still_pruned(tmp_path):
    # The write-activity guard protects non-complete dumps only; a
    # finalised run.json means the walk is over and its captures are
    # fair game regardless of freshness.
    d = make_dump(tmp_path, fresh_slug(age_s=60))
    run_main(tmp_path)
    assert d.exists()
    assert not (d / "screenshots").exists()


# ============================================================
# Non-complete dumps: deleted once quiescent
# ============================================================

def test_missing_run_json_dump_deleted(tmp_path):
    d = make_dump(tmp_path, OLD_TS, run_json=False, age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


def test_dry_run_status_dump_deleted(tmp_path):
    d = make_dump(tmp_path, OLD_TS, status="dry-run", screenshots=False,
                  age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


def test_in_progress_status_dump_deleted_when_stale(tmp_path):
    # An abandoned walk leaves status="in-progress" (the marker
    # download.py drops at walk start); once quiescent it is prunable.
    d = make_dump(tmp_path, OLD_TS, status="in-progress",
                  screenshots=False, age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


def test_statusless_manifest_kept_as_legacy_complete(tmp_path):
    # A run.json with no `status` key predates the status lifecycle. The
    # walk historically wrote run.json only once (at the end), so its
    # presence means the dump finished: classify COMPLETE, keep its load
    # inputs, prune only its debug captures. (New walks always carry a
    # status key, so this branch only ever sees pre-change dumps.)
    d = make_dump(tmp_path, OLD_TS, status=None, screenshots=True,
                  age_s=STALE_S)
    run_main(tmp_path)
    assert d.exists()
    assert not (d / "screenshots").exists()
    assert (d / "positions" / "positions_summary.csv").exists()


# ============================================================
# In-flight guard: keyed on write activity, not slug age
# ============================================================

def test_long_walk_old_slug_fresh_writes_kept(tmp_path):
    # The load-bearing fix: a walk whose slug is hours old but which
    # is still writing artefacts (fresh mtimes, no terminal run.json)
    # must NOT be classified abandoned and deleted.
    d = make_dump(tmp_path, OLD_TS, run_json=False, age_s=0.0)
    run_main(tmp_path)
    assert d.exists()
    assert (d / "positions" / "positions_summary.csv").exists()


def test_fresh_incomplete_dump_kept_by_age_guard(tmp_path):
    d = make_dump(tmp_path, fresh_slug(age_s=60), run_json=False)
    run_main(tmp_path)
    assert d.exists()


def test_stale_incomplete_dump_deleted_with_zero_min_age(tmp_path):
    d = make_dump(tmp_path, OLD_TS, run_json=False, age_s=STALE_S)
    run_main(tmp_path, "--min-age-hours", "0")
    assert not d.exists()


# ============================================================
# Unreadable / corrupt / invalid manifests: never deleted
# ============================================================

def test_corrupt_json_run_json_kept(tmp_path):
    # Corrupt bytes are UNKNOWN, not evidence of incompleteness —
    # skip, never delete (could be a complete dump with a mangled
    # manifest).
    d = make_dump(tmp_path, OLD_TS, screenshots=False, age_s=STALE_S)
    (d / "run.json").write_text("{not valid json")
    run_main(tmp_path)
    assert d.exists()
    assert (d / "positions" / "positions_summary.csv").exists()


def test_unreadable_run_json_kept(tmp_path):
    # An I/O error reading run.json (here: run.json is a directory,
    # a deterministic OSError) must not classify the dump as
    # non-complete and delete its load inputs.
    d = make_dump(tmp_path, OLD_TS, run_json=False, screenshots=False,
                  age_s=STALE_S)
    (d / "run.json").mkdir()
    run_main(tmp_path)
    assert d.exists()
    assert (d / "positions" / "positions_summary.csv").exists()


def test_invalid_calendar_slug_skipped_not_crash(tmp_path):
    # A slug that matches the regex but is not a real date
    # (Feb 30) must be skipped, not abort the whole prune.
    bad = make_dump(tmp_path, "20260230T010000Z", run_json=False,
                    screenshots=False, age_s=STALE_S)
    good = make_dump(tmp_path, OLD_TS, run_json=False, screenshots=False,
                     age_s=STALE_S)
    assert run_main(tmp_path) == 0
    assert bad.exists()          # unparseable age → left alone
    assert not good.exists()     # the valid stale dump still pruned


# ============================================================
# Symlinks: left alone
# ============================================================

def test_symlinked_screenshots_not_followed(tmp_path):
    external = tmp_path / "external"
    external.mkdir()
    (external / "keep.txt").write_text("precious")
    d = make_dump(tmp_path, OLD_TS, screenshots=False)
    (d / "screenshots").symlink_to(external, target_is_directory=True)
    run_main(tmp_path)
    assert (d / "screenshots").is_symlink()
    assert (external / "keep.txt").exists()


def test_symlinked_run_dir_skipped(tmp_path):
    external = tmp_path / "external_run"
    external.mkdir()
    (external / "keep.txt").write_text("precious")
    link = tmp_path / OLD_TS
    link.symlink_to(external, target_is_directory=True)
    run_main(tmp_path)
    assert link.is_symlink()
    assert (external / "keep.txt").exists()


# ============================================================
# --dry-run and root-level safety
# ============================================================

def test_dry_run_deletes_nothing(tmp_path):
    complete = make_dump(tmp_path, OLD_TS)
    crashed = make_dump(tmp_path, "20260102T010000Z", run_json=False,
                        age_s=STALE_S)
    run_main(tmp_path, "--dry-run")
    assert (complete / "screenshots").exists()
    assert crashed.exists()


def test_non_run_entries_never_touched(tmp_path):
    make_dump(tmp_path, OLD_TS)
    supplied = tmp_path / "supplied-statements"
    supplied.mkdir()
    (supplied / "signature.txt").write_text("sig")
    (tmp_path / "fidelity-web.db").write_bytes(b"sqlite fake")
    run_main(tmp_path)
    assert (supplied / "signature.txt").exists()
    assert (tmp_path / "fidelity-web.db").exists()


def test_missing_bronze_dir_exits(tmp_path):
    with pytest.raises(SystemExit):
        prune.main(["--bronze-dir", str(tmp_path / "nope")])


# ============================================================
# Target validation
# ============================================================

def test_validate_target_accepts_expected_shapes(tmp_path):
    (tmp_path / OLD_TS).mkdir()
    (tmp_path / OLD_TS / "screenshots").mkdir()
    prune.validate_target(tmp_path / OLD_TS, tmp_path)
    prune.validate_target(tmp_path / OLD_TS / "screenshots", tmp_path)


def test_validate_target_refuses_stray_paths(tmp_path):
    with pytest.raises(SystemExit):
        prune.validate_target(tmp_path / "supplied-statements", tmp_path)
    with pytest.raises(SystemExit):
        prune.validate_target(
            tmp_path / OLD_TS / "positions", tmp_path)
    with pytest.raises(SystemExit):
        prune.validate_target(
            tmp_path.parent / OLD_TS / "screenshots", tmp_path)


def test_validate_target_refuses_symlink(tmp_path):
    external = tmp_path / "ext"
    external.mkdir()
    link = tmp_path / OLD_TS
    link.symlink_to(external, target_is_directory=True)
    with pytest.raises(SystemExit):
        prune.validate_target(link, tmp_path)
