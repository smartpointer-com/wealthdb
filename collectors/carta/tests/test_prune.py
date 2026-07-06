"""
Unit tests for prune.py + the run.json status lifecycle it keys on.

tmp_path bronze trees, synthetic data only (no real ids/holdings). carta
writes NO bronze-resident debug artefact (its browser diagnostics land in
/debug via `./carta explore`), so debug_subdirs is empty and a complete
dump has nothing to prune — the only category prune removes is a whole
non-complete run dir. Covers:
  * complete dump: kept entirely, every load input untouched
  * statusless legacy manifest: classified complete (carta wrote run.json
    only at the end of a successful walk), kept
  * non-complete dumps (absent run.json / status="in-progress") deleted
    whole once quiescent
  * in-flight guard keyed on write activity, not slug age: a long walk
    (old slug, fresh writes, no terminal run.json) is protected
  * unreadable / corrupt run.json is UNKNOWN → skipped, never deleted
  * calendar-invalid slug is skipped, not a crash
  * symlinked run dir is left alone
  * --dry-run deletes nothing
  * non-run entries at the bronze root (silver DB, side-loaded override
    CSVs) are never touched
  * validate_target refuses paths outside the expected shapes

Plus the load-side half of the lifecycle (load.load_run):
  * a status="in-progress" dump is NOT loaded (the guard keeps a crashed
    walk's partial capture out of silver)
  * a status="complete" dump and a statusless legacy dump ARE loaded
"""

from __future__ import annotations

import json
import os
import sqlite3
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import prune  # noqa: E402
import load   # noqa: E402

MIGRATIONS_DIR = Path(__file__).resolve().parent.parent / "migrations"


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
              run_json: bool = True, age_s: float = 0.0,
              entities: bool = True, documents: bool = True) -> Path:
    """Build a synthetic carta bronze run dir mirroring download.py's
    layout: bootstrap/ + entities/<slug>/ (meta + holdings + a security
    list) + documents/ (index + a PDF) + run.json. ``age_s`` backdates
    every mtime so the write-activity guard sees an abandoned dump; the
    default (0) leaves it fresh. ``status`` controls the run.json status
    field (``None`` → a statusless legacy manifest)."""
    d = root / slug
    boot = d / "bootstrap"
    boot.mkdir(parents=True)
    (boot / "investments.json").write_text("[]")
    (boot / "navigation-config.json").write_text("{}")
    if entities:
        e = d / "entities" / "corp_100"
        (e / "vesting").mkdir(parents=True)
        (e / "meta.json").write_text(json.dumps(
            {"corporation_id": "100", "is_fund_investment": False}))
        (e / "holdings-dashboard.json").write_text(
            json.dumps({"held_since": "2025-01-15"}))
        (e / "shares.json").write_text(json.dumps({"rows": [], "totals": {}}))
    if documents:
        docs = d / "documents"
        docs.mkdir(parents=True)
        (docs / "index.json").write_text(
            json.dumps({"count": 0, "results": []}))
        (docs / "doc_100.pdf").write_bytes(b"%PDF-1.4 fake")
    if run_json:
        manifest: dict = {"schema": 1, "dry_run": False,
                          "individual_id": "1", "firm_id": "9",
                          "entities": [], "documents": {"indexed": 0}}
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


def assert_inputs_intact(d: Path) -> None:
    assert (d / "bootstrap" / "investments.json").exists()
    assert (d / "entities" / "corp_100" / "meta.json").exists()
    assert (d / "entities" / "corp_100" / "shares.json").exists()
    assert (d / "documents" / "index.json").exists()
    assert (d / "documents" / "doc_100.pdf").exists()
    assert (d / "run.json").exists()


# ============================================================
# Complete dumps: kept entirely (no debug_subdirs to reclaim)
# ============================================================

def test_complete_dump_kept_entirely(tmp_path):
    d = make_dump(tmp_path, OLD_TS)
    assert run_main(tmp_path) == 0
    assert d.exists()
    assert_inputs_intact(d)


def test_fresh_complete_dump_kept_entirely(tmp_path):
    # The write-activity guard protects non-complete dumps only; a
    # finalised run.json means the walk is over. carta has nothing to
    # reclaim from a complete dump, so it is simply left whole.
    d = make_dump(tmp_path, fresh_slug(age_s=60))
    assert run_main(tmp_path) == 0
    assert d.exists()
    assert_inputs_intact(d)


def test_statusless_manifest_kept_as_legacy_complete(tmp_path):
    # A run.json with no `status` key predates the status lifecycle. carta
    # historically wrote run.json only once (at the end of a successful
    # walk), so its presence means the dump finished: classify COMPLETE
    # and keep its load inputs. (New walks always carry a status key, so
    # this branch only ever sees pre-change dumps.)
    d = make_dump(tmp_path, OLD_TS, status=None, age_s=STALE_S)
    assert run_main(tmp_path) == 0
    assert d.exists()
    assert_inputs_intact(d)


# ============================================================
# Non-complete dumps: deleted once quiescent
# ============================================================

def test_missing_run_json_dump_deleted(tmp_path):
    d = make_dump(tmp_path, OLD_TS, run_json=False, age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


def test_in_progress_status_dump_deleted_when_stale(tmp_path):
    # An abandoned walk leaves status="in-progress" (the marker download.py
    # drops at run-dir creation); once quiescent it is prunable.
    d = make_dump(tmp_path, OLD_TS, status="in-progress", age_s=STALE_S)
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
    assert (d / "entities" / "corp_100" / "meta.json").exists()


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
    assert_inputs_intact(d)


def test_unreadable_run_json_kept(tmp_path):
    # An I/O error reading run.json (here: run.json is a directory, a
    # deterministic OSError) must not classify the dump as non-complete and
    # delete its load inputs.
    d = make_dump(tmp_path, OLD_TS, run_json=False, age_s=STALE_S)
    (d / "run.json").mkdir()
    run_main(tmp_path)
    assert d.exists()
    assert (d / "entities" / "corp_100" / "meta.json").exists()


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
    complete = make_dump(tmp_path, OLD_TS)
    crashed = make_dump(tmp_path, "20260102T010000Z", run_json=False,
                        age_s=STALE_S)
    run_main(tmp_path, "--dry-run")
    assert_inputs_intact(complete)
    assert crashed.exists()


def test_non_run_entries_never_touched(tmp_path):
    # The silver DB and the side-loaded override CSVs live at the bronze
    # ROOT (run_dir.parent), not under a run dir; prune only iterates
    # timestamped run dirs, so these are safe by construction.
    make_dump(tmp_path, OLD_TS)
    (tmp_path / "carta.db").write_bytes(b"sqlite fake")
    (tmp_path / "100-valuations.csv").write_text("date,fmv\n2025-01-01,1.0\n")
    (tmp_path / "100-transactions.csv").write_text("date,amount\n")
    run_main(tmp_path)
    assert (tmp_path / "carta.db").exists()
    assert (tmp_path / "100-valuations.csv").exists()
    assert (tmp_path / "100-transactions.csv").exists()


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
        prune.validate_target(tmp_path / "carta.db", tmp_path)
    with pytest.raises(SystemExit):
        # A subdir of a run dir is not a whole run dir and is not a
        # debug_subdir (there are none), so it must be refused.
        prune.validate_target(tmp_path / OLD_TS / "entities", tmp_path)
    with pytest.raises(SystemExit):
        prune.validate_target(tmp_path.parent / OLD_TS, tmp_path)


def test_validate_target_refuses_symlink(tmp_path):
    external = tmp_path / "ext"
    external.mkdir()
    link = tmp_path / OLD_TS
    link.symlink_to(external, target_is_directory=True)
    with pytest.raises(SystemExit):
        prune.validate_target(link, tmp_path)


# ============================================================
# Load-side of the lifecycle: load.load_run gating on status
# ============================================================

@pytest.fixture
def conn():
    c = sqlite3.connect(":memory:")
    c.execute("PRAGMA foreign_keys = ON")
    load.silver.apply_migrations(c, MIGRATIONS_DIR)
    yield c
    c.close()


def _dump_runs_count(conn) -> int:
    return conn.execute("SELECT COUNT(*) FROM dump_runs").fetchone()[0]


def test_load_skips_in_progress_dump(tmp_path, conn):
    # A crashed walk left status="in-progress"; load must not ingest its
    # partial capture as a silver snapshot.
    d = make_dump(tmp_path, OLD_TS, status="in-progress",
                  entities=False, documents=False)
    assert load.load_run(conn, d) is False
    assert _dump_runs_count(conn) == 0


def test_load_loads_complete_dump(tmp_path, conn):
    d = make_dump(tmp_path, OLD_TS, status="complete",
                  entities=False, documents=False)
    assert load.load_run(conn, d) is True
    assert _dump_runs_count(conn) == 1


def test_load_loads_statusless_legacy_dump(tmp_path, conn):
    # Backward compat: a statusless (pre-lifecycle) manifest is still
    # loadable — carta wrote it only at the end of a successful walk.
    d = make_dump(tmp_path, OLD_TS, status=None,
                  entities=False, documents=False)
    assert load.load_run(conn, d) is True
    assert _dump_runs_count(conn) == 1
