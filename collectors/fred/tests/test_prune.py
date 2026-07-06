"""
Unit tests for the fred collector's prune.py.

fred is a pure REST/JSON collector: it writes NO bronze-resident debug
artefacts (debug_subdirs=()) and every file in a complete run dir
(run.json + each <series_id>.json) is a load input. So prune's only
effect is removing WHOLE run dirs that are not complete dumps; a complete
dump is never touched. tmp_path bronze trees, synthetic round rates only
— no real FX data. Covers:

  * complete dump (status="complete"): kept entirely, nothing pruned
  * statusless manifest (pre-`status` legacy dump): COMPLETE, kept
  * non-complete dumps (absent run.json / status="in-progress" /
    status="dry-run") deleted whole once quiescent
  * in-flight guard keyed on write activity, not slug age: a long walk
    (old slug, fresh writes, no terminal run.json) is protected
  * unreadable / corrupt run.json is UNKNOWN -> skipped, never deleted
  * calendar-invalid slug is skipped, not a crash
  * symlinked run dirs are left alone
  * --dry-run deletes nothing
  * non-run entries at the bronze root (the silver fred.db, stray files)
    are never touched
  * validate_target refuses anything but a whole run dir (no debug subdirs)
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


def _obs_doc(rows):
    return {"observations": [{"date": d, "value": v} for d, v in rows]}


def make_dump(root: Path, slug: str, status: str | None = "complete",
              run_json: bool = True, series: bool = True,
              age_s: float = 0.0) -> Path:
    """Build a bronze run dir shaped like a real fred dump: one
    ``<series_id>.json`` observations doc per fetched series plus a
    ``run.json`` manifest. ``age_s`` backdates every mtime so the
    write-activity guard sees an abandoned dump; the default (0) leaves it
    fresh. ``status=None`` writes a statusless (pre-`status`) manifest."""
    d = root / slug
    d.mkdir(parents=True)
    if series:
        (d / "DEXSZUS.json").write_text(json.dumps(_obs_doc([
            ("2020-01-02", "0.9000"), ("2020-01-03", "0.9100")])))
        (d / "DEXUSEU.json").write_text(json.dumps(_obs_doc([
            ("2020-01-02", "1.1200")])))
    if run_json:
        manifest = {
            "slug": slug,
            "series": {
                "DEXSZUS": {"base": "CHF", "quote": "USD", "rows": 2},
                "DEXUSEU": {"base": "USD", "quote": "EUR", "rows": 1},
            },
        }
        if status is not None:
            manifest["status"] = status
        (d / "run.json").write_text(json.dumps(manifest))
    if age_s:
        backdate(d, age_s)
    return d


def backdate(path: Path, age_s: float) -> None:
    t = time.time() - age_s
    for p in list(path.rglob("*")) + [path]:
        os.utime(p, (t, t), follow_symlinks=False)


def run_main(root: Path, *extra: str) -> int:
    return prune.main(["--bronze-dir", str(root), *extra])


def _series_and_manifest_intact(d: Path) -> None:
    assert (d / "run.json").exists()
    assert (d / "DEXSZUS.json").exists()
    assert (d / "DEXUSEU.json").exists()


# ============================================================
# Complete dumps: kept entirely (no debug artefacts to prune)
# ============================================================

def test_complete_dump_kept_entirely(tmp_path):
    d = make_dump(tmp_path, OLD_TS)
    assert run_main(tmp_path) == 0
    assert d.exists()
    _series_and_manifest_intact(d)


def test_fresh_complete_dump_kept(tmp_path):
    # A finalised run.json means the walk is over; freshness is irrelevant
    # because a complete fred dump has nothing prunable inside it.
    d = make_dump(tmp_path, fresh_slug(age_s=60))
    assert run_main(tmp_path) == 0
    assert d.exists()
    _series_and_manifest_intact(d)


def test_statusless_manifest_kept_as_legacy_complete(tmp_path):
    # A run.json with no `status` key predates the status lifecycle. fred
    # historically wrote run.json only once (at the end of the walk), so
    # its presence means the dump finished: classify COMPLETE, keep it.
    d = make_dump(tmp_path, OLD_TS, status=None, age_s=STALE_S)
    assert run_main(tmp_path) == 0
    assert d.exists()
    _series_and_manifest_intact(d)


def test_total_failure_complete_dump_kept(tmp_path):
    # A run where every series failed writes run.json status="complete"
    # with an empty series dict (the walk finished, just fetched nothing).
    # It is a complete dump and must be kept.
    d = tmp_path / OLD_TS
    d.mkdir()
    (d / "run.json").write_text(json.dumps(
        {"slug": OLD_TS, "series": {}, "status": "complete"}))
    backdate(d, STALE_S)
    assert run_main(tmp_path) == 0
    assert d.exists()
    assert (d / "run.json").exists()


# ============================================================
# Non-complete dumps: deleted whole once quiescent
# ============================================================

def test_missing_run_json_dump_deleted(tmp_path):
    d = make_dump(tmp_path, OLD_TS, run_json=False, age_s=STALE_S)
    assert run_main(tmp_path) == 0
    assert not d.exists()


def test_in_progress_status_dump_deleted_when_stale(tmp_path):
    # An abandoned walk leaves status="in-progress" (the marker download.py
    # drops at run-dir creation); once quiescent it is prunable.
    d = make_dump(tmp_path, OLD_TS, status="in-progress", age_s=STALE_S)
    assert run_main(tmp_path) == 0
    assert not d.exists()


def test_dry_run_status_dump_deleted(tmp_path):
    # fred's own --dry-run mints no run dir, but the engine must still
    # classify any non-"complete" status as a non-complete dump.
    d = make_dump(tmp_path, OLD_TS, status="dry-run", age_s=STALE_S)
    assert run_main(tmp_path) == 0
    assert not d.exists()


# ============================================================
# In-flight guard: keyed on write activity, not slug age
# ============================================================

def test_long_walk_old_slug_fresh_writes_kept(tmp_path):
    # The load-bearing fix: a walk whose slug is hours old but which is
    # still writing series files (fresh mtimes, no terminal run.json) must
    # NOT be classified abandoned and deleted.
    d = make_dump(tmp_path, OLD_TS, run_json=False, age_s=0.0)
    assert run_main(tmp_path) == 0
    assert d.exists()
    assert (d / "DEXSZUS.json").exists()


def test_fresh_incomplete_dump_kept_by_age_guard(tmp_path):
    d = make_dump(tmp_path, fresh_slug(age_s=60), run_json=False)
    assert run_main(tmp_path) == 0
    assert d.exists()


def test_stale_incomplete_dump_deleted_with_zero_min_age(tmp_path):
    d = make_dump(tmp_path, OLD_TS, run_json=False, age_s=STALE_S)
    assert run_main(tmp_path, "--min-age-hours", "0") == 0
    assert not d.exists()


# ============================================================
# Unreadable / corrupt / invalid manifests: never deleted
# ============================================================

def test_corrupt_json_run_json_kept(tmp_path):
    # Corrupt bytes are UNKNOWN, not evidence of incompleteness — skip,
    # never delete (could be a complete dump with a mangled manifest).
    d = make_dump(tmp_path, OLD_TS, age_s=STALE_S)
    (d / "run.json").write_text("{not valid json")
    assert run_main(tmp_path) == 0
    assert d.exists()
    assert (d / "DEXSZUS.json").exists()


def test_unreadable_run_json_kept(tmp_path):
    # An I/O error reading run.json (here: run.json is a directory, a
    # deterministic OSError) must not classify the dump as non-complete
    # and delete its load inputs.
    d = make_dump(tmp_path, OLD_TS, run_json=False, age_s=STALE_S)
    (d / "run.json").mkdir()
    assert run_main(tmp_path) == 0
    assert d.exists()
    assert (d / "DEXSZUS.json").exists()


def test_invalid_calendar_slug_skipped_not_crash(tmp_path):
    # A slug that matches the regex but is not a real date (Feb 30) must be
    # skipped, not abort the whole prune.
    bad = make_dump(tmp_path, "20260230T010000Z", run_json=False,
                    age_s=STALE_S)
    good = make_dump(tmp_path, OLD_TS, run_json=False, age_s=STALE_S)
    assert run_main(tmp_path) == 0
    assert bad.exists()          # unparseable age → left alone
    assert not good.exists()     # the valid stale dump still pruned


# ============================================================
# Symlinks: left alone
# ============================================================

def test_symlinked_run_dir_skipped(tmp_path):
    external = tmp_path / "external_run"
    external.mkdir()
    (external / "keep.txt").write_text("precious")
    link = tmp_path / OLD_TS
    link.symlink_to(external, target_is_directory=True)
    assert run_main(tmp_path) == 0
    assert link.is_symlink()
    assert (external / "keep.txt").exists()


# ============================================================
# --dry-run and root-level safety
# ============================================================

def test_dry_run_deletes_nothing(tmp_path):
    complete = make_dump(tmp_path, OLD_TS)
    crashed = make_dump(tmp_path, "20260102T010000Z", run_json=False,
                        age_s=STALE_S)
    assert run_main(tmp_path, "--dry-run") == 0
    assert complete.exists()
    assert crashed.exists()


def test_non_run_entries_never_touched(tmp_path):
    make_dump(tmp_path, OLD_TS)
    # The silver DB lives at the bronze root, plus a stray JSON that is not
    # inside a run dir — neither is a timestamped run dir, so prune must
    # never touch them.
    (tmp_path / "fred.db").write_bytes(b"sqlite fake")
    stray = tmp_path / "notes.json"
    stray.write_text("{}")
    assert run_main(tmp_path) == 0
    assert (tmp_path / "fred.db").exists()
    assert stray.exists()


def test_missing_bronze_dir_exits(tmp_path):
    with pytest.raises(SystemExit):
        prune.main(["--bronze-dir", str(tmp_path / "nope")])


# ============================================================
# Target validation — no debug subdirs, only whole run dirs
# ============================================================

def test_validate_target_accepts_run_dir(tmp_path):
    (tmp_path / OLD_TS).mkdir()
    prune.validate_target(tmp_path / OLD_TS, tmp_path)


def test_validate_target_refuses_stray_and_input_paths(tmp_path):
    (tmp_path / OLD_TS).mkdir()
    (tmp_path / OLD_TS / "run.json").write_text("{}")
    # A load input inside a run dir is never a valid target (empty
    # debug_subdirs), nor is a non-run root entry, nor a foreign path.
    with pytest.raises(SystemExit):
        prune.validate_target(tmp_path / OLD_TS / "run.json", tmp_path)
    with pytest.raises(SystemExit):
        prune.validate_target(tmp_path / OLD_TS / "DEXSZUS.json", tmp_path)
    with pytest.raises(SystemExit):
        prune.validate_target(tmp_path / "fred.db", tmp_path)
    with pytest.raises(SystemExit):
        prune.validate_target(tmp_path.parent / OLD_TS, tmp_path)


def test_validate_target_refuses_symlink(tmp_path):
    external = tmp_path / "ext"
    external.mkdir()
    link = tmp_path / OLD_TS
    link.symlink_to(external, target_is_directory=True)
    with pytest.raises(SystemExit):
        prune.validate_target(link, tmp_path)
