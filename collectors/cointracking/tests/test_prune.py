"""
Unit tests for prune.py (cointracking).

tmp_path bronze trees mirroring cointracking's layout: a run dir holds
run.json + one cu_<id>/ per portfolio with trades.csv / balance.csv /
overview.csv (all load inputs). cointracking writes NO bronze-resident
debug artefact, so debug_subdirs is empty — a complete dump is left
entirely intact and only whole non-complete dumps are prunable.

Covers:
  * complete dump: load inputs kept, nothing pruned (no debug subdirs)
  * non-complete dumps (absent run.json / status in-progress / dry-run)
    deleted whole once quiescent
  * statusless manifest classified legacy-complete → kept intact
  * in-flight guard keyed on write activity, not slug age: a long walk
    (old slug, fresh writes) is protected
  * unreadable / corrupt run.json is UNKNOWN → skipped, never deleted
  * calendar-invalid slug is skipped, not a crash
  * symlinked run dir left alone
  * --dry-run deletes nothing
  * root entries (known_portfolios.json, the silver DB) never touched
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
CU = "1"


def fresh_slug(age_s: float = 0.0) -> str:
    dt = datetime.fromtimestamp(time.time() - age_s, tz=timezone.utc)
    return dt.strftime(FRESH_FMT)


def make_dump(root: Path, slug: str, status: str | None = "complete",
              run_json: bool = True, age_s: float = 0.0,
              cu: str = CU) -> Path:
    """Build a cointracking bronze run dir: run.json + one cu_<id>/
    holding the three load-input CSVs (trades / balance / overview).
    ``age_s`` backdates every file/dir mtime (and the run dir's own) so
    the write-activity guard sees an abandoned dump; the default (0)
    leaves it fresh. Synthetic ids / amounts only."""
    d = root / slug
    port = d / f"cu_{cu}"
    port.mkdir(parents=True)
    (port / "trades.csv").write_text('"Type","Buy"\n"Trade","0.5"\n')
    (port / "balance.csv").write_text('"Cur.","Amount"\n"BTC","0.5"\n')
    (port / "overview.csv").write_text(
        '"Date","BTC Value"\n"2026-01-01","1"\n')
    if run_json:
        meta: dict = {"portfolios": [{"id": cu, "name": "test"}]}
        if status is not None:
            meta["status"] = status
        (d / "run.json").write_text(json.dumps(meta))
    if age_s:
        backdate(d, age_s)
    return d


def backdate(path: Path, age_s: float) -> None:
    t = time.time() - age_s
    for p in list(path.rglob("*")) + [path]:
        os.utime(p, (t, t), follow_symlinks=False)


def run_main(root: Path, *extra: str) -> int:
    return prune.main(["--bronze-dir", str(root), *extra])


def _inputs_intact(d: Path, cu: str = CU) -> bool:
    csvs = all((d / f"cu_{cu}" / name).exists()
               for name in ("trades.csv", "balance.csv", "overview.csv"))
    return csvs and (d / "run.json").exists()


# ============================================================
# Complete dumps: inputs kept, nothing pruned
# ============================================================

def test_complete_dump_inputs_kept_nothing_pruned(tmp_path):
    # debug_subdirs is empty, so a complete dump has no prunable
    # artefacts — the whole dir and every load input survive.
    d = make_dump(tmp_path, OLD_TS)
    assert run_main(tmp_path) == 0
    assert d.exists()
    assert _inputs_intact(d)


def test_fresh_complete_dump_kept(tmp_path):
    d = make_dump(tmp_path, fresh_slug(age_s=60))
    assert run_main(tmp_path) == 0
    assert _inputs_intact(d)


# ============================================================
# Non-complete dumps: deleted once quiescent
# ============================================================

def test_missing_run_json_dump_deleted(tmp_path):
    d = make_dump(tmp_path, OLD_TS, run_json=False, age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


def test_in_progress_status_dump_deleted_when_stale(tmp_path):
    # A crashed walk leaves status="in-progress" (the marker download.py
    # drops at run-dir creation); once quiescent it is prunable.
    d = make_dump(tmp_path, OLD_TS, status="in-progress", age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


def test_dry_run_status_dump_deleted(tmp_path):
    # cointracking's --dry-run materialises no run dir, so a "dry-run"
    # shell is not produced in practice — but were one present it would
    # classify NON_COMPLETE and be pruned like any other.
    d = make_dump(tmp_path, OLD_TS, status="dry-run", age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


def test_statusless_manifest_kept_as_legacy_complete(tmp_path):
    # A run.json with no `status` key predates the status lifecycle. The
    # walk historically wrote run.json only once (at the end), so its
    # presence means the dump finished: classify COMPLETE and keep every
    # load input. (New walks always carry a status key, so this branch
    # only ever sees pre-change dumps.)
    d = make_dump(tmp_path, OLD_TS, status=None, age_s=STALE_S)
    run_main(tmp_path)
    assert d.exists()
    assert _inputs_intact(d)


# ============================================================
# In-flight guard: keyed on write activity, not slug age
# ============================================================

def test_long_walk_old_slug_fresh_writes_kept(tmp_path):
    # A walk whose slug is hours old but which is still writing
    # artefacts (fresh mtimes, no terminal run.json) must NOT be
    # classified abandoned and deleted.
    d = make_dump(tmp_path, OLD_TS, run_json=False, age_s=0.0)
    run_main(tmp_path)
    assert d.exists()
    assert (d / f"cu_{CU}" / "trades.csv").exists()


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
    d = make_dump(tmp_path, OLD_TS, age_s=STALE_S)
    (d / "run.json").write_text("{not valid json")
    run_main(tmp_path)
    assert d.exists()
    assert (d / f"cu_{CU}" / "trades.csv").exists()


def test_unreadable_run_json_kept(tmp_path):
    # An I/O error reading run.json (here: run.json is a directory, a
    # deterministic OSError) must not classify the dump as non-complete
    # and delete its load inputs.
    d = make_dump(tmp_path, OLD_TS, run_json=False, age_s=STALE_S)
    (d / "run.json").mkdir()
    run_main(tmp_path)
    assert d.exists()
    assert (d / f"cu_{CU}" / "trades.csv").exists()


def test_invalid_calendar_slug_skipped_not_crash(tmp_path):
    # A slug that matches the regex but is not a real date (Feb 30)
    # must be skipped, not abort the whole prune.
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
    assert _inputs_intact(complete)
    assert crashed.exists()


def test_non_run_entries_never_touched(tmp_path):
    # The persistent scrape cache and the silver DB live at the bronze
    # ROOT (the silver DB defaults to /data == bronze root); prune must
    # never touch a non-run-dir root entry.
    make_dump(tmp_path, OLD_TS)
    (tmp_path / "known_portfolios.json").write_text('{"portfolios": []}')
    (tmp_path / "cointracking.duckdb").write_bytes(b"duckdb fake")
    run_main(tmp_path)
    assert (tmp_path / "known_portfolios.json").exists()
    assert (tmp_path / "cointracking.duckdb").exists()


def test_missing_bronze_dir_exits(tmp_path):
    with pytest.raises(SystemExit):
        prune.main(["--bronze-dir", str(tmp_path / "nope")])


# ============================================================
# Target validation
# ============================================================

def test_validate_target_accepts_run_dir(tmp_path):
    (tmp_path / OLD_TS).mkdir()
    prune.validate_target(tmp_path / OLD_TS, tmp_path)


def test_validate_target_refuses_stray_paths(tmp_path):
    with pytest.raises(SystemExit):
        prune.validate_target(tmp_path / "known_portfolios.json", tmp_path)
    with pytest.raises(SystemExit):
        # a cu_<id>/ subtree is a load input, never a valid target
        prune.validate_target(tmp_path / OLD_TS / f"cu_{CU}", tmp_path)
    with pytest.raises(SystemExit):
        prune.validate_target(tmp_path.parent / OLD_TS, tmp_path)


def test_validate_target_refuses_symlink(tmp_path):
    external = tmp_path / "ext"
    external.mkdir()
    link = tmp_path / OLD_TS
    link.symlink_to(external, target_is_directory=True)
    with pytest.raises(SystemExit):
        prune.validate_target(link, tmp_path)
