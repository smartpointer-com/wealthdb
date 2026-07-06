"""
Unit tests for prune.py.

tmp_path bronze trees, synthetic artefacts only (no real account
tokens / IBANs / balances). ubs-web writes no bronze-resident debug
artefact (screenshots + traces go to the external --screenshot-dir),
so ``debug_subdirs`` is empty and prune's only deletion category is
whole non-complete run dirs. Covers:

  * complete dump: nothing pruned, every load input kept
  * non-complete dumps (no run.json / status="in-progress" /
    status="dry-run") deleted whole once quiescent
  * the ubs-web divergence: a statusless legacy manifest is COMPLETE
    only when it is NOT a --dry-run shell — dry_run:true ⇒ deleted,
    dry_run:false ⇒ kept
  * in-flight guard keyed on write activity, not slug age: a long
    walk (old slug, fresh writes) is protected
  * unreadable / corrupt run.json is UNKNOWN → skipped, never deleted
  * calendar-invalid slug is skipped, not a crash
  * symlinked run dir is left alone
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

# Sentinel: build the default "complete" manifest.
_DEFAULT = object()


def fresh_slug(age_s: float = 0.0) -> str:
    dt = datetime.fromtimestamp(time.time() - age_s, tz=timezone.utc)
    return dt.strftime(FRESH_FMT)


def make_dump(root: Path, slug: str, manifest=_DEFAULT,
              age_s: float = 0.0) -> Path:
    """Build a synthetic ubs-web bronze run dir with the standard load
    inputs (positions/ consolidated + per-portfolio CSVs, transactions/
    cash CSV + MT940, documents/ PDF).

    ``manifest`` controls run.json: the default sentinel writes a
    terminal ``complete`` manifest; ``None`` writes no run.json at all
    (a crashed walk); any dict is written verbatim (an in-progress
    marker, a dry-run shell, a statusless legacy manifest).

    ``age_s`` backdates every file/dir mtime (and the run dir's own) to
    now-age_s, so the write-activity guard sees an abandoned dump; the
    default (0) leaves it fresh.
    """
    d = root / slug
    (d / "positions").mkdir(parents=True)
    (d / "positions" / "positions.csv").write_text("a;b\n1;2\n")
    (d / "positions" / "positions_0badc0de0badc0de.csv").write_text(
        "a;b\n3;4\n")
    (d / "transactions").mkdir()
    (d / "transactions" / "cash_0badc0de0badc0de_20260101_20260201.csv"
     ).write_text("x;y\n5;6\n")
    (d / "transactions" / "cash_0badc0de0badc0de_20260101_20260201.mt940"
     ).write_text(":20:SYNTHETIC\n")
    (d / "documents").mkdir()
    (d / "documents" / "0badc0de0badc0de.pdf").write_bytes(b"%PDF-1.4 fake")
    if manifest is _DEFAULT:
        manifest = {"status": "complete", "dry_run": False}
    if manifest is not None:
        (d / "run.json").write_text(json.dumps(manifest))
    if age_s:
        backdate(d, age_s)
    return d


def backdate(path: Path, age_s: float) -> None:
    t = time.time() - age_s
    for p in list(path.rglob("*")) + [path]:
        os.utime(p, (t, t), follow_symlinks=False)


def assert_inputs_present(d: Path) -> None:
    assert (d / "run.json").exists()
    assert (d / "positions" / "positions.csv").exists()
    assert (d / "positions" / "positions_0badc0de0badc0de.csv").exists()
    assert (d / "transactions"
            / "cash_0badc0de0badc0de_20260101_20260201.csv").exists()
    assert (d / "transactions"
            / "cash_0badc0de0badc0de_20260101_20260201.mt940").exists()
    assert (d / "documents" / "0badc0de0badc0de.pdf").exists()


def run_main(root: Path, *extra: str) -> int:
    return prune.main(["--bronze-dir", str(root), *extra])


# ============================================================
# Complete dumps: nothing pruned, load inputs kept
# ============================================================

def test_complete_dump_fully_kept(tmp_path):
    # debug_subdirs is empty, so a complete dump has nothing to prune —
    # the whole dir and every load input survive.
    d = make_dump(tmp_path, OLD_TS)
    assert run_main(tmp_path) == 0
    assert d.exists()
    assert_inputs_present(d)


def test_fresh_complete_dump_kept(tmp_path):
    # The write-activity guard protects non-complete dumps only; a
    # finalised (status="complete") manifest means the walk is over, but
    # there is nothing prunable in a complete ubs-web dump regardless.
    d = make_dump(tmp_path, fresh_slug(age_s=60))
    assert run_main(tmp_path) == 0
    assert d.exists()
    assert_inputs_present(d)


# ============================================================
# Non-complete dumps: deleted once quiescent
# ============================================================

def test_missing_run_json_dump_deleted(tmp_path):
    d = make_dump(tmp_path, OLD_TS, manifest=None, age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


def test_dry_run_status_dump_deleted(tmp_path):
    d = make_dump(tmp_path, OLD_TS,
                  manifest={"status": "dry-run", "dry_run": True},
                  age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


def test_in_progress_status_dump_deleted_when_stale(tmp_path):
    # An abandoned walk leaves the in-progress marker download.py drops
    # at run-dir creation ({"status": "in-progress"}); once quiescent it
    # is prunable.
    d = make_dump(tmp_path, OLD_TS, manifest={"status": "in-progress"},
                  age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


# ============================================================
# Statusless legacy manifests: the ubs-web dry_run divergence
# ============================================================

def test_statusless_manifest_kept_as_legacy_complete(tmp_path):
    # A run.json with no `status` key predates the status lifecycle.
    # ubs-web wrote run.json only once, at the end of a real walk, so a
    # statusless manifest with dry_run:false means the dump finished:
    # classify COMPLETE, keep its load inputs.
    d = make_dump(tmp_path, OLD_TS, manifest={"dry_run": False},
                  age_s=STALE_S)
    run_main(tmp_path)
    assert d.exists()
    assert_inputs_present(d)


def test_statusless_dry_run_shell_deleted(tmp_path):
    # The load-bearing ubs-web divergence from fidelity-web: a legacy
    # --dry-run shell also has a full-looking (statusless) manifest, but
    # carries dry_run:true. Bare `m is not None` would wrongly keep it
    # forever; the `not m.get("dry_run")` guard classes it NON_COMPLETE.
    d = make_dump(tmp_path, OLD_TS, manifest={"dry_run": True},
                  age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


# ============================================================
# In-flight guard: keyed on write activity, not slug age
# ============================================================

def test_long_walk_old_slug_fresh_writes_kept(tmp_path):
    # The load-bearing fix: a walk whose slug is hours old but which is
    # still writing artefacts (fresh mtimes, only the in-progress marker)
    # must NOT be classified abandoned and deleted.
    d = make_dump(tmp_path, OLD_TS, manifest={"status": "in-progress"},
                  age_s=0.0)
    run_main(tmp_path)
    assert d.exists()
    assert_inputs_present(d)


def test_fresh_incomplete_dump_kept_by_age_guard(tmp_path):
    d = make_dump(tmp_path, fresh_slug(age_s=60), manifest=None)
    run_main(tmp_path)
    assert d.exists()


def test_stale_incomplete_dump_deleted_with_zero_min_age(tmp_path):
    d = make_dump(tmp_path, OLD_TS, manifest=None, age_s=STALE_S)
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
    assert_inputs_present(d)


def test_unreadable_run_json_kept(tmp_path):
    # An I/O error reading run.json (here: run.json is a directory, a
    # deterministic OSError) must not classify the dump as non-complete
    # and delete its load inputs.
    d = make_dump(tmp_path, OLD_TS, manifest=None, age_s=STALE_S)
    (d / "run.json").mkdir()
    run_main(tmp_path)
    assert d.exists()
    assert (d / "positions" / "positions.csv").exists()


def test_invalid_calendar_slug_skipped_not_crash(tmp_path):
    # A slug that matches the regex but is not a real date (Feb 30) must
    # be skipped, not abort the whole prune.
    bad = make_dump(tmp_path, "20260230T010000Z", manifest=None,
                    age_s=STALE_S)
    good = make_dump(tmp_path, OLD_TS, manifest=None, age_s=STALE_S)
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
    crashed = make_dump(tmp_path, "20260102T010000Z", manifest=None,
                        age_s=STALE_S)
    run_main(tmp_path, "--dry-run")
    assert complete.exists()
    assert_inputs_present(complete)
    assert crashed.exists()


def test_non_run_entries_never_touched(tmp_path):
    make_dump(tmp_path, OLD_TS)
    sideload = tmp_path / "historic"
    sideload.mkdir()
    (sideload / "statement.txt").write_text("sig")
    (tmp_path / "ubs-web.db").write_bytes(b"sqlite fake")
    run_main(tmp_path)
    assert (sideload / "statement.txt").exists()
    assert (tmp_path / "ubs-web.db").exists()


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
    with pytest.raises(SystemExit):
        prune.validate_target(tmp_path / "historic", tmp_path)
    # debug_subdirs is empty, so even a real load-input subdir is refused
    # — prune never targets anything inside a run dir here.
    with pytest.raises(SystemExit):
        prune.validate_target(tmp_path / OLD_TS / "positions", tmp_path)
    with pytest.raises(SystemExit):
        prune.validate_target(tmp_path.parent / OLD_TS, tmp_path)


def test_validate_target_refuses_symlink(tmp_path):
    external = tmp_path / "ext"
    external.mkdir()
    link = tmp_path / OLD_TS
    link.symlink_to(external, target_is_directory=True)
    with pytest.raises(SystemExit):
        prune.validate_target(link, tmp_path)
