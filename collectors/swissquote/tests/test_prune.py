"""
Unit tests for prune.py (swissquote).

tmp_path bronze trees with swissquote's real run-dir layout and
SYNTHETIC content (no real customer IDs, ISINs, or balances). Swissquote
nominates NO debug_subdirs — its diagnostics land outside bronze — so
the only deletions prune performs are whole non-complete run dirs. These
tests exercise that plus the shared safety envelope:

  * complete dump (status=complete): load inputs untouched, nothing pruned
  * statusless run.json (legacy dump): kept as complete
  * non-complete dumps (no run.json / status=in-progress / status=dry-run)
    deleted whole once quiescent
  * in-flight guard keyed on write activity, not slug age: a long walk
    (old slug, fresh writes, no terminal run.json) is protected
  * unreadable / corrupt run.json is UNKNOWN -> skipped, never deleted
  * calendar-invalid slug is skipped, not a crash
  * symlinked run dir is left alone
  * --dry-run deletes nothing
  * non-run entries at the bronze root (manual/, swissquote.db) untouched
  * validate_target refuses paths outside the whole-run-dir shape
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
              run_json: bool = True, age_s: float = 0.0) -> Path:
    """Build a bronze run dir with swissquote's load inputs.

    ``status`` is the run.json status value (None writes a statusless
    manifest — a legacy dump). ``age_s`` backdates every file/dir mtime
    (and the run dir's own) to now-age_s so the write-activity guard
    sees an abandoned dump; the default (0) leaves it fresh.
    """
    d = root / slug
    d.mkdir(parents=True)
    (d / "accounts.json").write_text('[{"account_product": "Trading", '
                                     '"account_external_id": "1000001"}]\n')
    (d / "positions.xls").write_bytes(b"\xd0\xcf\x11\xe0 fake-xls")
    (d / "list_of_assets.xls").write_bytes(b"\xd0\xcf\x11\xe0 fake-xls")
    (d / "position_details.json").write_text('[]\n')
    (d / "transactions_000.csv").write_text("Date;Order #\n")
    (d / "account_overview.pdf").write_bytes(b"%PDF-1.4 fake")
    docs = d / "documents"
    docs.mkdir()
    (docs / "AAAAAAAA-0000-0000-0000-000000000000.pdf").write_bytes(
        b"%PDF-1.4 fake-doc")
    if run_json:
        meta = {} if status is None else {"status": status}
        (d / "run.json").write_text(json.dumps(meta))
    if age_s:
        backdate(d, age_s)
    return d


def backdate(path: Path, age_s: float) -> None:
    t = time.time() - age_s
    for p in list(path.rglob("*")) + [path]:
        os.utime(p, (t, t), follow_symlinks=False)


def assert_inputs_present(d: Path) -> None:
    assert (d / "run.json").exists()
    assert (d / "accounts.json").exists()
    assert (d / "positions.xls").exists()
    assert (d / "list_of_assets.xls").exists()
    assert (d / "position_details.json").exists()
    assert (d / "transactions_000.csv").exists()
    assert (d / "documents").is_dir()
    assert next((d / "documents").glob("*.pdf"), None) is not None


def run_main(root: Path, *extra: str) -> int:
    return prune.main(["--bronze-dir", str(root), *extra])


# ============================================================
# Complete dumps: nothing to prune (no debug subdirs), inputs kept
# ============================================================

def test_complete_dump_untouched(tmp_path):
    d = make_dump(tmp_path, OLD_TS)
    assert run_main(tmp_path) == 0
    assert d.exists()
    assert_inputs_present(d)


def test_fresh_complete_dump_untouched(tmp_path):
    d = make_dump(tmp_path, fresh_slug(age_s=60))
    assert run_main(tmp_path) == 0
    assert d.exists()
    assert_inputs_present(d)


def test_statusless_manifest_kept_as_legacy_complete(tmp_path):
    # A run.json with no `status` key predates the status lifecycle.
    # download.py historically wrote run.json only once (at the end),
    # so its presence means the dump finished: classify COMPLETE and
    # keep everything.
    d = make_dump(tmp_path, OLD_TS, status=None, age_s=STALE_S)
    assert run_main(tmp_path) == 0
    assert d.exists()
    assert_inputs_present(d)


# ============================================================
# Non-complete dumps: deleted whole once quiescent
# ============================================================

def test_missing_run_json_dump_deleted(tmp_path):
    d = make_dump(tmp_path, OLD_TS, run_json=False, age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


def test_in_progress_status_dump_deleted_when_stale(tmp_path):
    # An abandoned walk leaves status="in-progress" (the marker
    # download.py drops at run-dir creation); once quiescent it is
    # prunable.
    d = make_dump(tmp_path, OLD_TS, status="in-progress", age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


def test_dry_run_status_dump_deleted(tmp_path):
    # download.py never writes a status="dry-run" run dir today (dry
    # runs return before mkdir), but prune must still reclaim one if
    # a future walk or a manual test leaves it behind.
    d = make_dump(tmp_path, OLD_TS, status="dry-run", age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


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
    assert (d / "positions.xls").exists()


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
    d = make_dump(tmp_path, OLD_TS, age_s=STALE_S)
    (d / "run.json").write_text("{not valid json")
    run_main(tmp_path)
    assert d.exists()
    assert_inputs_present(d)


def test_unreadable_run_json_kept(tmp_path):
    # An I/O error reading run.json (here: run.json is a directory, a
    # deterministic OSError) is UNKNOWN — must not delete load inputs.
    d = make_dump(tmp_path, OLD_TS, run_json=False, age_s=STALE_S)
    (d / "run.json").mkdir()
    run_main(tmp_path)
    assert d.exists()
    assert (d / "positions.xls").exists()


def test_invalid_calendar_slug_skipped_not_crash(tmp_path):
    # A slug that matches the regex but is not a real date (Feb 30)
    # must be skipped, not abort the whole prune.
    bad = make_dump(tmp_path, "20260230T010000Z", run_json=False,
                    age_s=STALE_S)
    good = make_dump(tmp_path, OLD_TS, run_json=False, age_s=STALE_S)
    assert run_main(tmp_path) == 0
    assert bad.exists()          # unparseable age -> left alone
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
    assert_inputs_present(complete)
    assert crashed.exists()


def test_non_run_entries_never_touched(tmp_path):
    make_dump(tmp_path, OLD_TS)
    manual = tmp_path / "manual"
    manual.mkdir()
    (manual / "tax_statement_2024.pdf").write_bytes(b"%PDF-1.4 fake")
    (tmp_path / "swissquote.db").write_bytes(b"sqlite fake")
    run_main(tmp_path)
    assert (manual / "tax_statement_2024.pdf").exists()
    assert (tmp_path / "swissquote.db").exists()


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
        prune.validate_target(tmp_path / "manual", tmp_path)
    with pytest.raises(SystemExit):
        # A load input inside a run dir is never a valid whole-dir
        # target (debug_subdirs is empty).
        prune.validate_target(tmp_path / OLD_TS / "positions.xls", tmp_path)
    with pytest.raises(SystemExit):
        prune.validate_target(tmp_path.parent / OLD_TS, tmp_path)


def test_validate_target_refuses_symlink(tmp_path):
    external = tmp_path / "ext"
    external.mkdir()
    link = tmp_path / OLD_TS
    link.symlink_to(external, target_is_directory=True)
    with pytest.raises(SystemExit):
        prune.validate_target(link, tmp_path)


if __name__ == "__main__":
    sys.exit(pytest.main([__file__, "-q"]))
