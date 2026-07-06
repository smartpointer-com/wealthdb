"""Unit tests for collectorkit.prune — the shared bronze-prune engine.

Ports the fidelity-web prune coverage onto the parameterized helper, and
adds coverage for the variation the helper exists to absorb:

  * complete dump: debug subdirs deleted, load inputs untouched
  * complete dump without a debug subdir: no-op
  * non-complete dumps (absent manifest / status != complete) deleted
    whole once quiescent
  * in-flight guard keyed on write activity, not slug age: a long walk
    (old slug, fresh writes) is protected
  * unreadable / corrupt / non-object manifest is UNKNOWN → skipped,
    never deleted (a read failure is not evidence of incompleteness)
  * calendar-invalid slug is skipped, not a crash
  * symlinked debug subdir / run dirs are left alone
  * --dry-run deletes nothing
  * non-run entries at the bronze root are never touched
  * validate_target refuses paths outside the configured shapes
  * empty debug_subdirs: only non-complete whole dirs are pruned
  * manifest_name=None collectors classify from a terminal artefact
  * legacy statusless-complete dumps keep their inputs
  * a file-shaped debug artefact (trace.zip) is pruned
  * multiple debug subdirs
  * status_classification unit behaviour
"""

from __future__ import annotations

import json
import os
import time
from datetime import datetime, timezone
from pathlib import Path

import pytest

from collectorkit import prune

OLD_TS = "20260101T010000Z"          # older than any guard by slug
STALE_S = 3 * 3600                    # comfortably past the 1h default
FRESH_FMT = "%Y%m%dT%H%M%SZ"


# ============================================================
# Test configs
# ============================================================

def _legacy_complete(run_dir, meta):
    # Statusless dumps are complete iff the terminal artefact is present.
    return run_dir is not None and (run_dir / "positions" / "positions.csv").exists()


def _is_complete(run_dir, meta):
    return prune.status_classification(
        meta, run_dir=run_dir, legacy_complete=_legacy_complete)


# A fidelity-web-shaped collector: one debug subdir, run.json manifest.
CFG = prune.PruneConfig(
    debug_subdirs=("screenshots",),
    is_complete=_is_complete,
)

# A collector that writes no bronze-resident debug artefact.
CFG_NO_DEBUG = prune.PruneConfig(
    debug_subdirs=(),
    is_complete=_is_complete,
)

# A collector with no manifest file at all — completeness keys purely on
# the terminal artefact (mirrors schwab-api / ubs-psn, which write no
# run.json historically).
CFG_NO_MANIFEST = prune.PruneConfig(
    debug_subdirs=(),
    manifest_name=None,
    is_complete=lambda run_dir, meta: prune.status_classification(
        meta, run_dir=run_dir,
        legacy_complete=lambda rd, m: (rd / "accounts_positions.json").exists()),
)

# A collector with a debug subdir *and* a file-shaped debug artefact.
CFG_MULTI = prune.PruneConfig(
    debug_subdirs=("screenshots", "trace.zip"),
    is_complete=_is_complete,
)


# ============================================================
# Fixtures
# ============================================================

def fresh_slug(age_s: float = 0.0) -> str:
    dt = datetime.fromtimestamp(time.time() - age_s, tz=timezone.utc)
    return dt.strftime(FRESH_FMT)


def make_dump(root: Path, slug: str, status: str | None = "complete",
              screenshots: bool = True, run_json: bool = True,
              age_s: float = 0.0, terminal: bool = True) -> Path:
    """Build a bronze run dir. ``age_s`` backdates every mtime so the
    write-activity guard sees an abandoned dump; the default (0) leaves it
    fresh. ``terminal`` writes the legacy terminal artefact (positions
    CSV) that the statusless-complete fallback keys on."""
    d = root / slug
    (d / "positions").mkdir(parents=True)
    if terminal:
        (d / "positions" / "positions.csv").write_text("a,b\n1,2\n")
    (d / "documents").mkdir()
    (d / "documents" / "Statement_2026.pdf").write_bytes(b"%PDF-1.4 fake")
    if screenshots:
        shots = d / "screenshots"
        shots.mkdir()
        (shots / "20260101T010101Z-landed.html").write_text("<html>x</html>")
        (shots / "20260101T010101Z-landed.png").write_bytes(b"\x89PNG fake")
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


def run_main(cfg, root: Path, *extra: str) -> int:
    return prune.main(cfg, ["--bronze-dir", str(root), *extra])


# ============================================================
# Complete dumps: debug pruned, inputs kept
# ============================================================

def test_complete_dump_debug_pruned_inputs_kept(tmp_path):
    d = make_dump(tmp_path, OLD_TS)
    assert run_main(CFG, tmp_path) == 0
    assert not (d / "screenshots").exists()
    assert (d / "positions" / "positions.csv").exists()
    assert (d / "documents" / "Statement_2026.pdf").exists()
    assert (d / "run.json").exists()


def test_complete_dump_without_debug_untouched(tmp_path):
    d = make_dump(tmp_path, OLD_TS, screenshots=False)
    assert run_main(CFG, tmp_path) == 0
    assert d.exists()
    assert (d / "positions" / "positions.csv").exists()


def test_fresh_complete_dump_debug_still_pruned(tmp_path):
    d = make_dump(tmp_path, fresh_slug(age_s=60))
    run_main(CFG, tmp_path)
    assert d.exists()
    assert not (d / "screenshots").exists()


def test_empty_debug_subdirs_keeps_complete_dump_whole(tmp_path):
    d = make_dump(tmp_path, OLD_TS)
    run_main(CFG_NO_DEBUG, tmp_path)
    assert (d / "screenshots").exists()      # not in debug_subdirs → kept
    assert (d / "positions" / "positions.csv").exists()


# ============================================================
# Non-complete dumps: deleted once quiescent
# ============================================================

def test_missing_run_json_dump_deleted(tmp_path):
    d = make_dump(tmp_path, OLD_TS, run_json=False, terminal=False,
                  age_s=STALE_S)
    run_main(CFG, tmp_path)
    assert not d.exists()


def test_dry_run_status_dump_deleted(tmp_path):
    d = make_dump(tmp_path, OLD_TS, status="dry-run", screenshots=False,
                  age_s=STALE_S)
    run_main(CFG, tmp_path)
    assert not d.exists()


def test_in_progress_status_dump_deleted_when_stale(tmp_path):
    d = make_dump(tmp_path, OLD_TS, status="in-progress",
                  screenshots=False, age_s=STALE_S)
    run_main(CFG, tmp_path)
    assert not d.exists()


def test_status_missing_key_no_legacy_signal_deleted(tmp_path):
    # Statusless AND no terminal artefact → NON_COMPLETE → deleted.
    d = make_dump(tmp_path, OLD_TS, status=None, screenshots=False,
                  terminal=False, age_s=STALE_S)
    run_main(CFG, tmp_path)
    assert not d.exists()


def test_statusless_legacy_complete_dump_inputs_kept(tmp_path):
    # Statusless but the terminal artefact is present → COMPLETE via the
    # legacy fallback: prune its debug, keep its inputs.
    d = make_dump(tmp_path, OLD_TS, status=None, age_s=STALE_S)
    run_main(CFG, tmp_path)
    assert d.exists()
    assert not (d / "screenshots").exists()
    assert (d / "positions" / "positions.csv").exists()


# ============================================================
# No-manifest collector (schwab-api / ubs-psn shape)
# ============================================================

def test_no_manifest_terminal_artefact_present_kept(tmp_path):
    d = tmp_path / OLD_TS
    d.mkdir()
    (d / "accounts_positions.json").write_text("{}")
    backdate(d, STALE_S)
    run_main(CFG_NO_MANIFEST, tmp_path)
    assert d.exists()
    assert (d / "accounts_positions.json").exists()


def test_no_manifest_terminal_artefact_absent_deleted(tmp_path):
    d = tmp_path / OLD_TS
    d.mkdir()
    (d / "transactions_000.json").write_text("[]")   # partial: no terminal
    backdate(d, STALE_S)
    run_main(CFG_NO_MANIFEST, tmp_path)
    assert not d.exists()


# ============================================================
# In-flight guard: keyed on write activity, not slug age
# ============================================================

def test_long_walk_old_slug_fresh_writes_kept(tmp_path):
    d = make_dump(tmp_path, OLD_TS, run_json=False, terminal=False,
                  age_s=0.0)
    run_main(CFG, tmp_path)
    assert d.exists()


def test_fresh_incomplete_dump_kept_by_age_guard(tmp_path):
    d = make_dump(tmp_path, fresh_slug(age_s=60), run_json=False,
                  terminal=False)
    run_main(CFG, tmp_path)
    assert d.exists()


def test_stale_incomplete_dump_deleted_with_zero_min_age(tmp_path):
    d = make_dump(tmp_path, OLD_TS, run_json=False, terminal=False,
                  age_s=STALE_S)
    run_main(CFG, tmp_path, "--min-age-hours", "0")
    assert not d.exists()


# ============================================================
# Unreadable / corrupt / non-object manifests: never deleted
# ============================================================

def test_corrupt_json_run_json_kept(tmp_path):
    d = make_dump(tmp_path, OLD_TS, screenshots=False, terminal=False,
                  age_s=STALE_S)
    (d / "run.json").write_text("{not valid json")
    run_main(CFG, tmp_path)
    assert d.exists()
    assert (d / "positions").exists()


def test_non_object_json_run_json_kept(tmp_path):
    # A JSON array/scalar is not a manifest object → UNKNOWN, never deleted.
    d = make_dump(tmp_path, OLD_TS, screenshots=False, terminal=False,
                  age_s=STALE_S)
    (d / "run.json").write_text("[1, 2, 3]")
    run_main(CFG, tmp_path)
    assert d.exists()


def test_unreadable_run_json_kept(tmp_path):
    # run.json is a directory → deterministic OSError on read → UNKNOWN.
    d = make_dump(tmp_path, OLD_TS, run_json=False, screenshots=False,
                  terminal=False, age_s=STALE_S)
    (d / "run.json").mkdir()
    run_main(CFG, tmp_path)
    assert d.exists()


def test_invalid_calendar_slug_skipped_not_crash(tmp_path):
    bad = make_dump(tmp_path, "20260230T010000Z", run_json=False,
                    screenshots=False, terminal=False, age_s=STALE_S)
    good = make_dump(tmp_path, OLD_TS, run_json=False, screenshots=False,
                     terminal=False, age_s=STALE_S)
    assert run_main(CFG, tmp_path) == 0
    assert bad.exists()
    assert not good.exists()


# ============================================================
# Symlinks: left alone
# ============================================================

def test_symlinked_debug_not_followed(tmp_path):
    external = tmp_path / "external"
    external.mkdir()
    (external / "keep.txt").write_text("precious")
    d = make_dump(tmp_path, OLD_TS, screenshots=False)
    (d / "screenshots").symlink_to(external, target_is_directory=True)
    run_main(CFG, tmp_path)
    assert (d / "screenshots").is_symlink()
    assert (external / "keep.txt").exists()


def test_symlinked_run_dir_skipped(tmp_path):
    external = tmp_path / "external_run"
    external.mkdir()
    (external / "keep.txt").write_text("precious")
    link = tmp_path / OLD_TS
    link.symlink_to(external, target_is_directory=True)
    run_main(CFG, tmp_path)
    assert link.is_symlink()
    assert (external / "keep.txt").exists()


# ============================================================
# File-shaped debug artefacts + multiple subdirs
# ============================================================

def test_file_shaped_debug_artefact_pruned(tmp_path):
    d = make_dump(tmp_path, OLD_TS, screenshots=True)
    (d / "trace.zip").write_bytes(b"PK\x03\x04 fake-trace")
    run_main(CFG_MULTI, tmp_path)
    assert not (d / "screenshots").exists()
    assert not (d / "trace.zip").exists()
    assert (d / "positions" / "positions.csv").exists()


# ============================================================
# --dry-run and root-level safety
# ============================================================

def test_dry_run_deletes_nothing(tmp_path):
    complete = make_dump(tmp_path, OLD_TS)
    crashed = make_dump(tmp_path, "20260102T010000Z", run_json=False,
                        terminal=False, age_s=STALE_S)
    run_main(CFG, tmp_path, "--dry-run")
    assert (complete / "screenshots").exists()
    assert crashed.exists()


def test_non_run_entries_never_touched(tmp_path):
    make_dump(tmp_path, OLD_TS)
    shared = tmp_path / "manual"
    shared.mkdir()
    (shared / "note.txt").write_text("sig")
    (tmp_path / "collector.db").write_bytes(b"sqlite fake")
    run_main(CFG, tmp_path)
    assert (shared / "note.txt").exists()
    assert (tmp_path / "collector.db").exists()


def test_missing_bronze_dir_exits(tmp_path):
    with pytest.raises(SystemExit):
        prune.main(CFG, ["--bronze-dir", str(tmp_path / "nope")])


# ============================================================
# Target validation
# ============================================================

def test_validate_target_accepts_expected_shapes(tmp_path):
    (tmp_path / OLD_TS).mkdir()
    (tmp_path / OLD_TS / "screenshots").mkdir()
    prune.validate_target(tmp_path / OLD_TS, tmp_path, CFG)
    prune.validate_target(tmp_path / OLD_TS / "screenshots", tmp_path, CFG)


def test_validate_target_refuses_stray_paths(tmp_path):
    with pytest.raises(SystemExit):
        prune.validate_target(tmp_path / "manual", tmp_path, CFG)
    with pytest.raises(SystemExit):
        prune.validate_target(tmp_path / OLD_TS / "positions", tmp_path, CFG)
    with pytest.raises(SystemExit):
        prune.validate_target(
            tmp_path.parent / OLD_TS / "screenshots", tmp_path, CFG)


def test_validate_target_refuses_symlink(tmp_path):
    external = tmp_path / "ext"
    external.mkdir()
    link = tmp_path / OLD_TS
    link.symlink_to(external, target_is_directory=True)
    with pytest.raises(SystemExit):
        prune.validate_target(link, tmp_path, CFG)


# ============================================================
# status_classification unit behaviour
# ============================================================

def test_status_classification_states():
    assert prune.status_classification({"status": "complete"})[0] == prune.COMPLETE
    assert prune.status_classification({"status": "in-progress"})[0] == prune.NON_COMPLETE
    assert prune.status_classification({"status": "dry-run"})[0] == prune.NON_COMPLETE
    assert prune.status_classification(None)[0] == prune.NON_COMPLETE
    assert prune.status_classification({})[0] == prune.NON_COMPLETE


def test_status_classification_legacy_fallback():
    ok = prune.status_classification(
        {}, run_dir=Path("/x"), legacy_complete=lambda rd, m: True)
    assert ok[0] == prune.COMPLETE
    none_ok = prune.status_classification(
        None, run_dir=Path("/x"), legacy_complete=lambda rd, m: True)
    assert none_ok[0] == prune.COMPLETE
    # A present non-complete status is never overridden by the fallback.
    still_nc = prune.status_classification(
        {"status": "dry-run"}, legacy_complete=lambda rd, m: True)
    assert still_nc[0] == prune.NON_COMPLETE
