"""
Unit tests for prune.py (equityzen).

tmp_path bronze trees against equityzen's run-dir layout
(investments.json + offerings/<slug>/detail.json + documents/<slug>/*.pdf
+ run.json). equityzen writes NO bronze-resident debug artefact, so
``debug_subdirs`` is empty and the only reclaim category is whole
non-complete run dirs. Covers:

  * complete dump: nothing pruned, every load input untouched
  * complete dump with document blobs: blobs (a load input) kept
  * non-complete dumps (absent run.json / status="in-progress")
    deleted whole once quiescent
  * in-flight guard keyed on write activity, not slug age: a long
    walk (old slug, fresh writes) is protected
  * statusless legacy manifest classified complete (kept)
  * unreadable / corrupt run.json is UNKNOWN -> skipped, never deleted
  * calendar-invalid slug is skipped, not a crash
  * symlinked run dir is left alone
  * --dry-run deletes nothing
  * non-run entries at the bronze root (silver .db) are never touched
  * validate_target refuses paths outside the expected shapes

Synthetic slugs / ids / bytes only — no real deal ids, company names,
share counts, or amounts.
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

# Synthetic deal-slug / doc-slug: sha256(id)[:16]-shaped hex, no real id.
DEAL_SLUG = "0123456789abcdef"
DOC_SLUG = "fedcba9876543210"


def fresh_slug(age_s: float = 0.0) -> str:
    dt = datetime.fromtimestamp(time.time() - age_s, tz=timezone.utc)
    return dt.strftime(FRESH_FMT)


def make_dump(root: Path, slug: str, status: str | None = "complete",
              documents: bool = False, run_json: bool = True,
              age_s: float = 0.0) -> Path:
    """Build a synthetic equityzen bronze run dir. ``age_s`` backdates
    every file/dir mtime (and the run dir's own) to now-age_s, so the
    write-activity guard sees an abandoned dump; the default (0) leaves it
    fresh. ``run_json=False`` models a walk that crashed before the
    in-progress marker; ``status`` sets the run.json status field
    (``None`` writes a statusless legacy manifest)."""
    d = root / slug
    (d / "offerings" / DEAL_SLUG).mkdir(parents=True)
    (d / "investments.json").write_text(json.dumps(
        {"ONGOING": {"data": {"buyer": {"buyerDeals": {"edges": []}}}}}))
    (d / "offerings" / DEAL_SLUG / "detail.json").write_text(json.dumps(
        {"data": {"buyer": {"buyerDeals": {"edges": []}}}}))
    if documents:
        (d / "documents" / DEAL_SLUG).mkdir(parents=True)
        (d / "documents" / DEAL_SLUG / f"{DOC_SLUG}.pdf").write_bytes(
            b"%PDF-1.4 fake")
    if run_json:
        (d / "run.json").write_text(json.dumps(
            {"status": status} if status is not None else {"source": "equityzen"}))
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
# Complete dumps: nothing pruned, load inputs kept
# ============================================================

def test_complete_dump_nothing_pruned_inputs_kept(tmp_path):
    # debug_subdirs is empty, so a complete dump has NOTHING removed.
    d = make_dump(tmp_path, OLD_TS)
    assert run_main(tmp_path) == 0
    assert d.exists()
    assert (d / "investments.json").exists()
    assert (d / "offerings" / DEAL_SLUG / "detail.json").exists()
    assert (d / "run.json").exists()


def test_complete_dump_document_blobs_kept(tmp_path):
    # The re-downloaded document PDF/zip blobs are load inputs (parsed by
    # statements.py) and must survive prune.
    d = make_dump(tmp_path, OLD_TS, documents=True)
    assert run_main(tmp_path) == 0
    assert (d / "documents" / DEAL_SLUG / f"{DOC_SLUG}.pdf").exists()


def test_fresh_complete_dump_untouched(tmp_path):
    # The write-activity guard protects non-complete dumps only; a
    # finalised run.json means the walk is over. Nothing to prune here.
    d = make_dump(tmp_path, fresh_slug(age_s=60))
    assert run_main(tmp_path) == 0
    assert d.exists()
    assert (d / "investments.json").exists()


# ============================================================
# Non-complete dumps: deleted once quiescent
# ============================================================

def test_missing_run_json_dump_deleted(tmp_path):
    d = make_dump(tmp_path, OLD_TS, run_json=False, age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


def test_in_progress_status_dump_deleted_when_stale(tmp_path):
    # An abandoned walk leaves status="in-progress" (the marker
    # download.py drops at run-dir creation); once quiescent it is prunable.
    d = make_dump(tmp_path, OLD_TS, status="in-progress", age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


def test_statusless_manifest_kept_as_legacy_complete(tmp_path):
    # A run.json with no `status` key predates the status lifecycle. The
    # walk historically wrote run.json only once (at the end), so its
    # presence means the dump finished: classify COMPLETE, keep its load
    # inputs. (New walks always carry a status key, so this branch only
    # ever sees pre-change dumps.)
    d = make_dump(tmp_path, OLD_TS, status=None, age_s=STALE_S)
    run_main(tmp_path)
    assert d.exists()
    assert (d / "investments.json").exists()


# ============================================================
# In-flight guard: keyed on write activity, not slug age
# ============================================================

def test_long_walk_old_slug_fresh_writes_kept(tmp_path):
    # The load-bearing fix: a walk whose slug is hours old but which is
    # still writing artefacts (fresh mtimes, no terminal run.json) must
    # NOT be classified abandoned and deleted.
    d = make_dump(tmp_path, OLD_TS, run_json=False, age_s=0.0)
    run_main(tmp_path)
    assert d.exists()
    assert (d / "investments.json").exists()


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
    d = make_dump(tmp_path, OLD_TS, age_s=STALE_S)
    (d / "run.json").write_text("{not valid json")
    run_main(tmp_path)
    assert d.exists()
    assert (d / "investments.json").exists()


def test_unreadable_run_json_kept(tmp_path):
    # An I/O error reading run.json (here: run.json is a directory, a
    # deterministic OSError) must not classify the dump as non-complete
    # and delete its load inputs.
    d = make_dump(tmp_path, OLD_TS, run_json=False, age_s=STALE_S)
    (d / "run.json").mkdir()
    run_main(tmp_path)
    assert d.exists()
    assert (d / "investments.json").exists()


def test_invalid_calendar_slug_skipped_not_crash(tmp_path):
    # A slug that matches the regex but is not a real date (Feb 30) must
    # be skipped, not abort the whole prune.
    bad = make_dump(tmp_path, "20260230T010000Z", run_json=False, age_s=STALE_S)
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
    assert (complete / "investments.json").exists()
    assert crashed.exists()


def test_non_run_entries_never_touched(tmp_path):
    make_dump(tmp_path, OLD_TS, run_json=False, age_s=STALE_S)
    (tmp_path / "equityzen.db").write_bytes(b"sqlite fake")
    run_main(tmp_path)
    assert (tmp_path / "equityzen.db").exists()


def test_missing_bronze_dir_exits(tmp_path):
    with pytest.raises(SystemExit):
        prune.main(["--bronze-dir", str(tmp_path / "nope")])


# ============================================================
# Target validation
# ============================================================

def test_validate_target_accepts_whole_run_dir(tmp_path):
    (tmp_path / OLD_TS).mkdir()
    prune.validate_target(tmp_path / OLD_TS, tmp_path)


def test_validate_target_refuses_stray_paths(tmp_path):
    (tmp_path / OLD_TS / "offerings").mkdir(parents=True)
    with pytest.raises(SystemExit):
        # a load input inside a run dir — never a valid deletion target
        prune.validate_target(tmp_path / OLD_TS / "investments.json", tmp_path)
    with pytest.raises(SystemExit):
        prune.validate_target(tmp_path / OLD_TS / "offerings", tmp_path)
    with pytest.raises(SystemExit):
        prune.validate_target(tmp_path / "equityzen.db", tmp_path)
    with pytest.raises(SystemExit):
        prune.validate_target(tmp_path.parent / OLD_TS, tmp_path)


def test_validate_target_refuses_symlink(tmp_path):
    external = tmp_path / "ext"
    external.mkdir()
    link = tmp_path / OLD_TS
    link.symlink_to(external, target_is_directory=True)
    with pytest.raises(SystemExit):
        prune.validate_target(link, tmp_path)
