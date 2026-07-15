"""
Unit tests for prune.py.

tmp_path bronze trees over angellist's layout. Two categories are
removed: `screenshots/` (the `download --debug` per-route captures) from
a complete dump, and whole run dirs that are not complete dumps. The
`explore` verb's diagnostics live outside bronze and prune never sees
them.

Covers:
  * complete dump (status="complete"): screenshots/ reclaimed, load
    inputs (captures.jsonl, run.json) plus the identity-convenience
    viewer.json all survive
  * statusless run.json is a pre-`status` complete dump (download.py
    historically wrote run.json only at the end): classified COMPLETE,
    kept whole
  * non-complete dumps (absent run.json / status="in-progress" /
    status="dry-run") deleted whole once quiescent
  * in-flight guard keyed on write activity, not slug age: a long
    walk (old slug, fresh writes) is protected
  * unreadable / corrupt run.json is UNKNOWN → skipped, never deleted
  * calendar-invalid slug is skipped, not a crash
  * symlinked run dirs are left alone
  * --dry-run deletes nothing
  * the bronze-ROOT sibling angellist-documents/ (a K-1/PDF load input)
    and the silver angellist.db are never touched — they are not
    timestamped run dirs
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

# One synthetic captured GraphQL exchange — the primary load input. No
# real IDs/balances/names: a placeholder op with an empty data object.
CAPTURE_LINE = json.dumps(
    {"op": "PortfolioDashboardQuery", "variables": {}, "data": {}}) + "\n"
# Synthetic identity blob (viewer.json) — kept, never a prune target.
VIEWER_BLOB = json.dumps({"currentUser": {"slug": "synthetic-user"}})


def fresh_slug(age_s: float = 0.0) -> str:
    dt = datetime.fromtimestamp(time.time() - age_s, tz=timezone.utc)
    return dt.strftime(FRESH_FMT)


def make_dump(root: Path, slug: str, status: str | None = "complete",
              run_json: bool = True, captures: bool = True,
              viewer: bool = True, screenshots: bool = True,
              age_s: float = 0.0) -> Path:
    """Build a bronze run dir with angellist's artefacts. ``status=None``
    with ``run_json=True`` writes a statusless (pre-`status`) manifest.
    ``screenshots`` adds the `--debug` capture dir. ``age_s`` backdates
    every file/dir mtime (and the run dir's own) to now-age_s, so the
    write-activity guard sees an abandoned dump; the default (0) leaves it
    fresh."""
    d = root / slug
    d.mkdir(parents=True)
    if captures:
        (d / "captures.jsonl").write_text(CAPTURE_LINE)
    if viewer:
        (d / "viewer.json").write_text(VIEWER_BLOB)
    if screenshots:
        shots = d / "screenshots"
        shots.mkdir()
        (shots / "10-bootstrap.html").write_text("<html>bootstrap</html>")
        (shots / "20-acct1-portfolio.png").write_bytes(b"\x89PNG fake")
    if run_json:
        payload = ({"status": status} if status is not None
                   else {"source": "angellist", "ops": {}, "had_errors": False})
        (d / "run.json").write_text(json.dumps(payload))
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
# Complete dumps: screenshots reclaimed, load inputs kept
# ============================================================

def test_complete_dump_screenshots_pruned_inputs_kept(tmp_path):
    d = make_dump(tmp_path, OLD_TS, status="complete")
    assert run_main(tmp_path) == 0
    assert d.exists()
    assert not (d / "screenshots").exists()
    assert (d / "captures.jsonl").exists()
    assert (d / "run.json").exists()
    assert (d / "viewer.json").exists()


def test_complete_dump_without_screenshots_untouched(tmp_path):
    # The common case: --debug was off, so there is no capture dir.
    d = make_dump(tmp_path, OLD_TS, status="complete", screenshots=False)
    assert run_main(tmp_path) == 0
    assert d.exists()
    assert (d / "captures.jsonl").exists()
    assert (d / "run.json").exists()


def test_fresh_complete_dump_screenshots_still_pruned(tmp_path):
    # A finalised run.json means the walk is over; freshness is irrelevant
    # to a complete dump (the age guard protects non-complete dumps only),
    # so its captures are reclaimable however recently written.
    d = make_dump(tmp_path, fresh_slug(age_s=60), status="complete")
    assert run_main(tmp_path) == 0
    assert not (d / "screenshots").exists()
    assert d.exists()
    assert (d / "captures.jsonl").exists()


def test_statusless_manifest_kept_as_legacy_complete(tmp_path):
    # A run.json with no `status` key predates the status lifecycle.
    # download.py historically wrote run.json only once (at the end, after
    # viewer.json + captures.jsonl), so its presence means the dump
    # finished: classify COMPLETE, keep its inputs. (New walks always carry
    # a status key, so this branch only ever sees pre-change dumps — which
    # is also why they carry no screenshots: --debug wrote none back then.)
    d = make_dump(tmp_path, OLD_TS, status=None, age_s=STALE_S,
                  screenshots=False)
    assert run_main(tmp_path) == 0
    assert d.exists()
    assert (d / "captures.jsonl").exists()
    assert (d / "run.json").exists()


# ============================================================
# Non-complete dumps: deleted whole once quiescent
# ============================================================

def test_missing_run_json_dump_deleted(tmp_path):
    # A pre-change crashed walk: captures.jsonl written, no run.json. load
    # would otherwise keep re-ingesting it (it gates on captures.jsonl, not
    # run.json), so reclaiming it whole is real cleanup.
    d = make_dump(tmp_path, OLD_TS, run_json=False, age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


def test_in_progress_status_dump_deleted_when_stale(tmp_path):
    # An abandoned walk leaves status="in-progress" (the marker download.py
    # drops at run-dir creation); once quiescent it is prunable.
    d = make_dump(tmp_path, OLD_TS, status="in-progress", age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


def test_dry_run_status_dump_deleted(tmp_path):
    # angellist's --dry-run never creates a run dir, but the engine still
    # treats any non-"complete" status as non-complete — assert the guard
    # holds for a stray dry-run shell.
    d = make_dump(tmp_path, OLD_TS, status="dry-run", age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


# ============================================================
# In-flight guard: keyed on write activity, not slug age
# ============================================================

def test_long_walk_old_slug_fresh_writes_kept(tmp_path):
    # The load-bearing fix: a walk whose slug is hours old but which is
    # still writing artefacts (fresh mtimes, no terminal run.json) must NOT
    # be classified abandoned and deleted.
    d = make_dump(tmp_path, OLD_TS, run_json=False, age_s=0.0)
    run_main(tmp_path)
    assert d.exists()
    assert (d / "captures.jsonl").exists()


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
    # Corrupt bytes are UNKNOWN, not evidence of incompleteness — skip,
    # never delete (could be a complete dump with a mangled manifest).
    d = make_dump(tmp_path, OLD_TS, run_json=False, age_s=STALE_S)
    (d / "run.json").write_text("{not valid json")
    run_main(tmp_path)
    assert d.exists()
    assert (d / "captures.jsonl").exists()


def test_unreadable_run_json_kept(tmp_path):
    # An I/O error reading run.json (here: run.json is a directory, a
    # deterministic OSError) must not classify the dump as non-complete
    # and delete its load inputs.
    d = make_dump(tmp_path, OLD_TS, run_json=False, age_s=STALE_S)
    (d / "run.json").mkdir()
    run_main(tmp_path)
    assert d.exists()
    assert (d / "captures.jsonl").exists()


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
    run_main(tmp_path)
    assert link.is_symlink()
    assert (external / "keep.txt").exists()


# ============================================================
# --dry-run and root-level safety
# ============================================================

def test_dry_run_deletes_nothing(tmp_path):
    complete = make_dump(tmp_path, OLD_TS, status="complete")
    crashed = make_dump(tmp_path, "20260102T010000Z", run_json=False,
                        age_s=STALE_S)
    run_main(tmp_path, "--dry-run")
    assert (complete / "captures.jsonl").exists()
    assert crashed.exists()


def test_documents_sibling_and_db_never_touched(tmp_path):
    # RISK 1: angellist-documents/ is a CRITICAL load input (K-1 CSVs/PDFs
    # feed load_documents) but sits at the bronze ROOT as a sibling of the
    # run dirs, NOT under one. It (and the silver angellist.db) are safe
    # ONLY because the engine iterates via bronze.iter_run_dirs, which
    # filters by the timestamped-run-dir slug — neither name matches. Guard
    # that even alongside a pruned non-complete dump.
    make_dump(tmp_path, OLD_TS, run_json=False, age_s=STALE_S)  # gets pruned
    docs = tmp_path / "angellist-documents"
    docs.mkdir()
    (docs / "2024 Schedule K-1 Some SPV.csv").write_text("a,b\n1,2\n")
    (docs / "2024 Schedule K-1 Some SPV.pdf").write_bytes(b"%PDF-1.4 fake")
    (tmp_path / "angellist.db").write_bytes(b"sqlite fake")
    run_main(tmp_path)
    assert not (tmp_path / OLD_TS).exists()          # the crashed dump went
    assert (docs / "2024 Schedule K-1 Some SPV.csv").exists()
    assert (docs / "2024 Schedule K-1 Some SPV.pdf").exists()
    assert (tmp_path / "angellist.db").exists()


def test_missing_bronze_dir_exits(tmp_path):
    with pytest.raises(SystemExit):
        prune.main(["--bronze-dir", str(tmp_path / "nope")])


# ============================================================
# Target validation
# ============================================================

def test_validate_target_accepts_run_dir(tmp_path):
    # With no debug_subdirs, the only valid target shape is a whole run dir.
    (tmp_path / OLD_TS).mkdir()
    prune.validate_target(tmp_path / OLD_TS, tmp_path)


def test_validate_target_accepts_screenshots_subdir(tmp_path):
    # The one subdir shape prune may target: the --debug capture dir.
    (tmp_path / OLD_TS / "screenshots").mkdir(parents=True)
    prune.validate_target(tmp_path / OLD_TS / "screenshots", tmp_path)


def test_validate_target_refuses_stray_paths(tmp_path):
    (tmp_path / OLD_TS).mkdir()
    (tmp_path / OLD_TS / "captures.jsonl").write_text(CAPTURE_LINE)
    with pytest.raises(SystemExit):
        # a load input inside a run dir is never a target
        prune.validate_target(tmp_path / OLD_TS / "captures.jsonl", tmp_path)
    with pytest.raises(SystemExit):
        # the bronze-root documents sibling is never a target
        prune.validate_target(tmp_path / "angellist-documents", tmp_path)
    with pytest.raises(SystemExit):
        # a run dir outside this bronze root is never a target
        prune.validate_target(tmp_path.parent / OLD_TS, tmp_path)


def test_validate_target_refuses_symlink(tmp_path):
    external = tmp_path / "ext"
    external.mkdir()
    link = tmp_path / OLD_TS
    link.symlink_to(external, target_is_directory=True)
    with pytest.raises(SystemExit):
        prune.validate_target(link, tmp_path)
