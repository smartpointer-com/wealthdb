"""
Unit tests for relevate's prune.py.

tmp_path bronze trees, synthetic fixtures only (no real ids /
balances / account numbers). relevate drives no browser, so its only
bronze-resident debug artefact is the HTTP trace `download --debug`
writes under <run>/screenshots/ (``debug_subdirs``); prune reclaims that
plus whole non-complete run dirs, and never a load input. Covers:

  * complete dump (status="complete", or a pre-`status` manifest with
    ended_at stamped on a non-dry run): every load input (accounts/,
    portfolios/, documents/*.pdf) survives; only a --debug trace is
    reclaimed from it
  * non-complete dumps deleted whole once quiescent: absent run.json,
    status in {in-progress, dry-run, incomplete}, and the relevate-
    specific hazard — a pre-`status` crashed walk whose run.json is
    PRESENT but ended_at=null (must NOT be shielded as complete)
  * in-flight guard keyed on write activity, not slug age: a long walk
    (old slug, fresh writes, no terminal status) is protected
  * unreadable / corrupt run.json is UNKNOWN -> skipped, never deleted
  * calendar-invalid slug is skipped, not a crash
  * symlinked run dirs are left alone
  * --dry-run deletes nothing
  * non-run entries at the bronze root (manual/, the silver DB) are
    never touched
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
SLUG = "abcdef0123456789"            # synthetic sha256(externalId)[:16]
DOC_ID = "42"                        # synthetic Relevate document id


def fresh_slug(age_s: float = 0.0) -> str:
    dt = datetime.fromtimestamp(time.time() - age_s, tz=timezone.utc)
    return dt.strftime(FRESH_FMT)


def make_dump(root: Path, slug: str, *,
              status: str | None = "complete",
              ended_at: str | None = "2026-01-01T01:00:05+00:00",
              dry_run: bool = False,
              run_json: bool = True,
              screenshots: bool = False,
              age_s: float = 0.0) -> Path:
    """Build a synthetic relevate bronze run dir with the real load-
    input layout. ``status=None`` writes a pre-`status` (legacy)
    manifest keyed only on ``ended_at`` / ``dry_run``. ``screenshots``
    adds the ``--debug`` HTTP trace, off by default to mirror a download
    without ``--debug``. ``age_s`` backdates every mtime so the
    write-activity guard sees an abandoned dump; the default (0) leaves
    it fresh."""
    d = root / slug
    # ---- load inputs (never prune targets) ----
    ov = d / "accounts" / "investment-overview.json"
    ov.parent.mkdir(parents=True)
    ov.write_text(json.dumps({"portfolios": [
        {"externalId": "ACC1", "id": 100, "cashBalance": 1.0}]}))
    (d / "accounts" / "sso-claims.json").write_text("{}")
    pdir = d / "portfolios" / SLUG
    pdir.mkdir(parents=True)
    (pdir / "modelportfolio.json").write_text(json.dumps({"positions": []}))
    (pdir / "performance.json").write_text(json.dumps({"values": []}))
    (pdir / "deposits.json").write_text(json.dumps({"transactions": []}))
    (pdir / "fees.json").write_text("{}")
    (pdir / "investment-allocation.json").write_text("{}")
    docs = d / "documents"
    docs.mkdir()
    (docs / "index.json").write_text(json.dumps({"documents": [
        {"id": int(DOC_ID), "fileName": "Quartalsbericht.pdf"}]}))
    (docs / f"{DOC_ID}.pdf").write_bytes(b"%PDF-1.4 synthetic")
    # ---- the --debug HTTP trace (never a load input) ----
    if screenshots:
        (d / "screenshots").mkdir()
        (d / "screenshots" / "http-trace.jsonl").write_text(
            '{"method": "GET", "url": "https://x.invalid/a", "status": 200}\n')
    # ---- manifest ----
    if run_json:
        manifest: dict = {
            "tool": "relevate.download",
            "schema_version": 1,
            "started_at": "2026-01-01T01:00:00+00:00",
            "ended_at": ended_at,
            "mode": "all",
            "dry_run": dry_run,
        }
        if status is not None:
            manifest["status"] = status
        (d / "run.json").write_text(json.dumps(manifest, indent=2))
    if age_s:
        backdate(d, age_s)
    return d


def backdate(path: Path, age_s: float) -> None:
    t = time.time() - age_s
    for p in list(path.rglob("*")) + [path]:
        os.utime(p, (t, t), follow_symlinks=False)


def assert_inputs_present(d: Path) -> None:
    assert (d / "run.json").exists()
    assert (d / "accounts" / "investment-overview.json").exists()
    assert (d / "portfolios" / SLUG / "modelportfolio.json").exists()
    assert (d / "portfolios" / SLUG / "performance.json").exists()
    assert (d / "documents" / "index.json").exists()
    assert (d / "documents" / f"{DOC_ID}.pdf").exists()


def run_main(root: Path, *extra: str) -> int:
    return prune.main(["--bronze-dir", str(root), *extra])


# ============================================================
# Complete dumps: load inputs kept, the --debug trace reclaimed
# ============================================================

def test_complete_status_dump_kept_entirely(tmp_path):
    d = make_dump(tmp_path, OLD_TS, status="complete", age_s=STALE_S)
    assert run_main(tmp_path) == 0
    assert d.exists()
    assert_inputs_present(d)


def test_complete_dump_debug_trace_pruned_inputs_kept(tmp_path):
    # The --debug HTTP trace is a diagnostic, not a load input, so prune
    # reclaims it while every artefact beside it survives.
    d = make_dump(tmp_path, OLD_TS, status="complete", screenshots=True,
                  age_s=STALE_S)
    assert run_main(tmp_path) == 0
    assert not (d / "screenshots").exists()
    assert_inputs_present(d)


def test_legacy_complete_dump_kept_entirely(tmp_path):
    # A statusless run.json with ended_at stamped on a non-dry run is a
    # pre-`status` complete dump: keep everything.
    d = make_dump(tmp_path, OLD_TS, status=None,
                  ended_at="2026-01-01T01:00:05+00:00", dry_run=False,
                  age_s=STALE_S)
    assert run_main(tmp_path) == 0
    assert d.exists()
    assert_inputs_present(d)


def test_complete_dump_documents_pdf_survives(tmp_path):
    # Explicit load-input protection: the document PDF is read
    # cross-dump for historical-snapshot / credit-note parsing, so a
    # complete dump's PDF must never be deleted.
    d = make_dump(tmp_path, OLD_TS, status="complete", age_s=STALE_S)
    run_main(tmp_path)
    assert (d / "documents" / f"{DOC_ID}.pdf").read_bytes().startswith(b"%PDF")


# ============================================================
# Non-complete dumps: deleted whole once quiescent
# ============================================================

def test_missing_run_json_dump_deleted(tmp_path):
    d = make_dump(tmp_path, OLD_TS, run_json=False, age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


def test_in_progress_status_dump_deleted_when_stale(tmp_path):
    # A crashed walk leaves the born "in-progress" marker; once
    # quiescent it is prunable.
    d = make_dump(tmp_path, OLD_TS, status="in-progress", age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


def test_incomplete_status_dump_deleted_when_stale(tmp_path):
    # Master enumeration failed -> finish(status="incomplete").
    d = make_dump(tmp_path, OLD_TS, status="incomplete", age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


def test_dry_run_status_dump_deleted(tmp_path):
    d = make_dump(tmp_path, OLD_TS, status="dry-run", age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


def test_legacy_crashed_ended_at_null_deleted(tmp_path):
    # THE relevate-specific hazard: run.json is PRESENT (flushed
    # incrementally from run-dir creation) but ended_at is null because
    # finish() never ran. A naive `m is not None` legacy signal would
    # wrongly shield this as complete and defeat prune forever. It must
    # classify NON_COMPLETE and be deleted once quiescent.
    d = make_dump(tmp_path, OLD_TS, status=None, ended_at=None,
                  age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


def test_legacy_dry_run_deleted(tmp_path):
    # A pre-`status` --dry-run shell: ended_at stamped but dry_run=true.
    d = make_dump(tmp_path, OLD_TS, status=None,
                  ended_at="2026-01-01T01:00:05+00:00", dry_run=True,
                  age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


# ============================================================
# In-flight guard: keyed on write activity, not slug age
# ============================================================

def test_long_walk_old_slug_fresh_writes_kept(tmp_path):
    # A walk whose slug is hours old but which is still writing (fresh
    # mtimes, still "in-progress") must NOT be classified abandoned.
    d = make_dump(tmp_path, OLD_TS, status="in-progress", age_s=0.0)
    run_main(tmp_path)
    assert d.exists()
    assert_inputs_present(d)


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
    d = make_dump(tmp_path, OLD_TS, status="in-progress", age_s=STALE_S)
    (d / "run.json").write_text("{not valid json")
    run_main(tmp_path)
    assert d.exists()
    assert_inputs_present(d)


def test_unreadable_run_json_kept(tmp_path):
    # run.json is a directory -> deterministic OSError on read -> UNKNOWN.
    d = make_dump(tmp_path, OLD_TS, run_json=False, age_s=STALE_S)
    (d / "run.json").mkdir()
    run_main(tmp_path)
    assert d.exists()
    assert (d / "accounts" / "investment-overview.json").exists()


def test_invalid_calendar_slug_skipped_not_crash(tmp_path):
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
    complete = make_dump(tmp_path, OLD_TS, status="complete", age_s=STALE_S)
    crashed = make_dump(tmp_path, "20260102T010000Z", run_json=False,
                        age_s=STALE_S)
    run_main(tmp_path, "--dry-run")
    assert_inputs_present(complete)
    assert crashed.exists()


def test_non_run_entries_never_touched(tmp_path):
    make_dump(tmp_path, OLD_TS, status="complete")
    manual = tmp_path / "manual"
    manual.mkdir()
    (manual / "ad-hoc.pdf").write_bytes(b"%PDF manual")
    (tmp_path / "relevate.db").write_bytes(b"sqlite fake")
    run_main(tmp_path)
    assert (manual / "ad-hoc.pdf").exists()
    assert (tmp_path / "relevate.db").exists()


def test_missing_bronze_dir_exits(tmp_path):
    with pytest.raises(SystemExit):
        prune.main(["--bronze-dir", str(tmp_path / "nope")])


# ============================================================
# Target validation (a run dir or its screenshots/, nothing else)
# ============================================================

def test_validate_target_accepts_run_dir(tmp_path):
    (tmp_path / OLD_TS).mkdir()
    prune.validate_target(tmp_path / OLD_TS, tmp_path)


def test_validate_target_accepts_debug_subdir(tmp_path):
    (tmp_path / OLD_TS / "screenshots").mkdir(parents=True)
    prune.validate_target(tmp_path / OLD_TS / "screenshots", tmp_path)


def test_validate_target_refuses_stray_paths(tmp_path):
    (tmp_path / OLD_TS).mkdir()
    (tmp_path / OLD_TS / "accounts").mkdir()
    with pytest.raises(SystemExit):
        prune.validate_target(tmp_path / "manual", tmp_path)
    with pytest.raises(SystemExit):
        prune.validate_target(tmp_path / OLD_TS / "accounts", tmp_path)
    with pytest.raises(SystemExit):
        prune.validate_target(tmp_path.parent / OLD_TS, tmp_path)


def test_validate_target_refuses_symlink(tmp_path):
    external = tmp_path / "ext"
    external.mkdir()
    link = tmp_path / OLD_TS
    link.symlink_to(external, target_is_directory=True)
    with pytest.raises(SystemExit):
        prune.validate_target(link, tmp_path)
