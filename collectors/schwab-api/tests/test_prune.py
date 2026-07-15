"""
Unit tests for schwab-api's prune.py.

tmp_path bronze trees. A schwab-api bronze run dir is a flat set of JSON
load inputs plus, only under `download --debug`, a screenshots/ HTTP trace
(debug_subdirs) — so prune reclaims that trace and whole non-complete run
dirs, never a load input. Covers:
  * complete dump (run.json status=complete): load inputs untouched, the
    --debug trace reclaimed
  * no run.json, open_orders.json present: COMPLETE, kept, load inputs
    survive
  * statusless manifest + open_orders present → COMPLETE, kept;
    statusless manifest + open_orders absent → NON_COMPLETE
  * non-complete dumps (absent run.json + absent open_orders / status
    != complete) deleted whole once quiescent
  * forward status is authoritative: in-progress + open_orders present
    is still NON_COMPLETE (the terminal manifest is the completeness
    signal, mirroring the fleet convention)
  * in-flight guard keyed on write activity, not slug age: a long
    walk (old slug, fresh writes) is protected
  * unreadable / corrupt run.json is UNKNOWN → skipped, never deleted
  * calendar-invalid slug is skipped, not a crash
  * symlinked run dirs are left alone
  * --dry-run deletes nothing
  * non-run entries at the bronze root (the silver .db) are never touched
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

ACCT_PLAIN = "00000001"
ACCT_HASH = "HASH00000000000000000001"


def fresh_slug(age_s: float = 0.0) -> str:
    dt = datetime.fromtimestamp(time.time() - age_s, tz=timezone.utc)
    return dt.strftime(FRESH_FMT)


def make_dump(root: Path, slug: str, *, status: str | None = "complete",
              run_json: bool = True, open_orders: bool = True,
              screenshots: bool = False, age_s: float = 0.0) -> Path:
    """Build a schwab-api bronze run dir with synthetic load inputs.

    ``run_json`` toggles the manifest (``status`` its status field, or
    a statusless ``{}`` when ``status is None``); ``open_orders`` toggles
    the terminal ``open_orders.json`` artefact (the legacy completeness
    signal); ``screenshots`` adds the ``--debug`` HTTP trace, off by
    default to mirror a download without ``--debug``. ``age_s`` backdates
    every file/dir mtime (and the run dir's own) to now-age_s so the
    write-activity guard sees an abandoned dump; the default (0) leaves it
    fresh."""
    d = root / slug
    d.mkdir(parents=True)
    if screenshots:
        (d / "screenshots").mkdir()
        (d / "screenshots" / "http-trace.jsonl").write_text(
            '{"method": "GET", "url": "https://x.invalid/a", "status": 200}\n')
    (d / "account_numbers.json").write_text(json.dumps(
        [{"accountNumber": ACCT_PLAIN, "hashValue": ACCT_HASH}]))
    (d / "user_preference.json").write_text(json.dumps({"accounts": []}))
    (d / "accounts_positions.json").write_text(json.dumps([
        {"securitiesAccount": {"accountNumber": ACCT_PLAIN,
                               "type": "CASH", "positions": []}}]))
    (d / "transactions_000.json").write_text(json.dumps({
        "account_hash": ACCT_HASH, "window_start": "2026-01-01",
        "window_end": "2026-01-07", "transactions": []}))
    if open_orders:
        (d / "open_orders.json").write_text(json.dumps({"orders": []}))
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


def _inputs_intact(d: Path) -> bool:
    return all((d / name).exists() for name in (
        "account_numbers.json", "accounts_positions.json",
        "user_preference.json", "transactions_000.json", "open_orders.json"))


# ============================================================
# Complete dumps: load inputs untouched, the --debug trace reclaimed
# ============================================================

def test_complete_dump_kept_inputs_untouched(tmp_path):
    d = make_dump(tmp_path, OLD_TS)
    assert run_main(tmp_path) == 0
    assert d.exists()
    assert _inputs_intact(d)
    assert (d / "run.json").exists()


def test_complete_dump_debug_trace_pruned_inputs_kept(tmp_path):
    # The --debug HTTP trace is a diagnostic, not a load input, so prune
    # reclaims it while every artefact beside it survives.
    d = make_dump(tmp_path, OLD_TS, screenshots=True)
    assert run_main(tmp_path) == 0
    assert not (d / "screenshots").exists()
    assert _inputs_intact(d)


def test_fresh_complete_dump_kept(tmp_path):
    # The write-activity guard protects non-complete dumps only; a
    # finalised run.json means the walk is over, so freshness cannot
    # protect the trace — but the load inputs are never at risk.
    d = make_dump(tmp_path, fresh_slug(age_s=60), screenshots=True)
    run_main(tmp_path)
    assert d.exists()
    assert not (d / "screenshots").exists()
    assert _inputs_intact(d)


def test_legacy_complete_dump_no_manifest_kept(tmp_path):
    # No run.json at all (meta is None); the terminal signal is
    # open_orders.json, the last unconditional artefact a complete run
    # writes. Present ⇒ COMPLETE, kept.
    d = make_dump(tmp_path, OLD_TS, run_json=False, open_orders=True,
                  age_s=STALE_S)
    run_main(tmp_path)
    assert d.exists()
    assert _inputs_intact(d)


def test_statusless_manifest_with_open_orders_kept(tmp_path):
    # A run.json with no `status` key + open_orders.json present: the
    # fallback (open_orders exists) classifies COMPLETE.
    d = make_dump(tmp_path, OLD_TS, status=None, open_orders=True,
                  age_s=STALE_S)
    run_main(tmp_path)
    assert d.exists()
    assert _inputs_intact(d)


def test_legacy_complete_dump_compressed_open_orders_kept(tmp_path):
    # Regression: a dump with no run.json whose terminal artefact
    # open_orders.json was recompressed to open_orders.json.zst must
    # still classify COMPLETE. _is_complete resolves the on-disk variant,
    # so the `recompress` sweep does not turn a complete dump into a
    # prune target (which would silently delete its bronze).
    from collectorkit import compress

    d = make_dump(tmp_path, OLD_TS, run_json=False, open_orders=True,
                  age_s=STALE_S)
    compress.compress_file(d / "open_orders.json")   # → open_orders.json.zst
    assert not (d / "open_orders.json").exists()
    assert (d / "open_orders.json.zst").is_file()
    # compress_file unlinks the plain file inside the dir, re-bumping the
    # run-dir mtime to now — which trips prune's quiescence guard and skips
    # the dir BEFORE _is_complete is ever consulted, masking a reverted
    # fix. Re-backdate so the age guard passes and completeness is what
    # actually decides this dump's fate (without this the test is vacuous:
    # it stays green even with the plain-name .exists() bug reintroduced).
    backdate(d, STALE_S)

    run_main(tmp_path)
    assert d.exists()
    assert (d / "account_numbers.json").exists()
    assert (d / "open_orders.json.zst").is_file()


# ============================================================
# Non-complete dumps: deleted once quiescent
# ============================================================

def test_crashed_dump_no_manifest_no_open_orders_deleted(tmp_path):
    # Crashed before open_orders.json and before any manifest existed:
    # no status, fallback signal absent ⇒ NON_COMPLETE.
    d = make_dump(tmp_path, OLD_TS, run_json=False, open_orders=False,
                  age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


def test_statusless_manifest_without_open_orders_deleted(tmp_path):
    # Statusless manifest but no open_orders.json: fallback signal absent
    # ⇒ NON_COMPLETE.
    d = make_dump(tmp_path, OLD_TS, status=None, open_orders=False,
                  age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


def test_in_progress_status_dump_deleted_when_stale(tmp_path):
    # An abandoned walk leaves status="in-progress" (the marker
    # download.py drops at run-dir creation); once quiescent it is
    # prunable.
    d = make_dump(tmp_path, OLD_TS, status="in-progress", open_orders=False,
                  age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


def test_in_progress_status_wins_over_present_open_orders(tmp_path):
    # Forward status is authoritative: even with open_orders.json present
    # (crash after the terminal artefact but before the terminal
    # manifest), status="in-progress" ⇒ NON_COMPLETE. The terminal
    # manifest is the completeness signal — this mirrors the fleet
    # convention (a clean finish always overwrites the marker).
    d = make_dump(tmp_path, OLD_TS, status="in-progress", open_orders=True,
                  age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


def test_dry_run_status_dump_deleted(tmp_path):
    # download.py never creates a run dir for --dry-run, but if a
    # dry-run shell ever appeared prune must still reclaim it.
    d = make_dump(tmp_path, OLD_TS, status="dry-run", open_orders=False,
                  age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


# ============================================================
# In-flight guard: keyed on write activity, not slug age
# ============================================================

def test_long_walk_old_slug_fresh_writes_kept(tmp_path):
    # The load-bearing fix: a walk whose slug is hours old but which is
    # still writing artefacts (fresh mtimes, only the in-progress marker,
    # no open_orders yet) must NOT be classified abandoned and deleted.
    d = make_dump(tmp_path, OLD_TS, status="in-progress", open_orders=False,
                  age_s=0.0)
    run_main(tmp_path)
    assert d.exists()
    assert (d / "account_numbers.json").exists()


def test_fresh_incomplete_dump_kept_by_age_guard(tmp_path):
    d = make_dump(tmp_path, fresh_slug(age_s=60), run_json=False,
                  open_orders=False)
    run_main(tmp_path)
    assert d.exists()


def test_stale_incomplete_dump_deleted_with_zero_min_age(tmp_path):
    d = make_dump(tmp_path, OLD_TS, run_json=False, open_orders=False,
                  age_s=STALE_S)
    run_main(tmp_path, "--min-age-hours", "0")
    assert not d.exists()


# ============================================================
# Unreadable / corrupt / invalid manifests: never deleted
# ============================================================

def test_corrupt_json_run_json_kept(tmp_path):
    # Corrupt bytes are UNKNOWN, not evidence of incompleteness — skip,
    # never delete (could be a complete dump with a mangled manifest).
    d = make_dump(tmp_path, OLD_TS, open_orders=False, age_s=STALE_S)
    (d / "run.json").write_text("{not valid json")
    run_main(tmp_path)
    assert d.exists()
    assert (d / "account_numbers.json").exists()


def test_unreadable_run_json_kept(tmp_path):
    # An I/O error reading run.json (here: run.json is a directory, a
    # deterministic OSError) must not classify the dump as non-complete
    # and delete its load inputs.
    d = make_dump(tmp_path, OLD_TS, run_json=False, open_orders=False,
                  age_s=STALE_S)
    (d / "run.json").mkdir()
    run_main(tmp_path)
    assert d.exists()
    assert (d / "account_numbers.json").exists()


def test_invalid_calendar_slug_skipped_not_crash(tmp_path):
    # A slug that matches the regex but is not a real date (Feb 30) must
    # be skipped, not abort the whole prune.
    bad = make_dump(tmp_path, "20260230T010000Z", run_json=False,
                    open_orders=False, age_s=STALE_S)
    good = make_dump(tmp_path, OLD_TS, run_json=False, open_orders=False,
                     age_s=STALE_S)
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
                        open_orders=False, age_s=STALE_S)
    run_main(tmp_path, "--dry-run")
    assert _inputs_intact(complete)
    assert crashed.exists()


def test_non_run_entries_never_touched(tmp_path):
    # The silver DB sits AT the bronze root next to the run dirs; it is a
    # plain file (not a RUN_DIR_RE dir) so prune never yields it.
    make_dump(tmp_path, OLD_TS)
    (tmp_path / "schwab-api.db").write_bytes(b"sqlite fake")
    sidecar = tmp_path / "notes"
    sidecar.mkdir()
    (sidecar / "readme.txt").write_text("keep me")
    run_main(tmp_path)
    assert (tmp_path / "schwab-api.db").exists()
    assert (sidecar / "readme.txt").exists()


def test_missing_bronze_dir_exits(tmp_path):
    with pytest.raises(SystemExit):
        prune.main(["--bronze-dir", str(tmp_path / "nope")])


# ============================================================
# Target validation
# ============================================================

def test_validate_target_accepts_whole_run_dir(tmp_path):
    (tmp_path / OLD_TS).mkdir()
    prune.validate_target(tmp_path / OLD_TS, tmp_path)


def test_validate_target_accepts_debug_subdir(tmp_path):
    (tmp_path / OLD_TS / "screenshots").mkdir(parents=True)
    prune.validate_target(tmp_path / OLD_TS / "screenshots", tmp_path)


def test_validate_target_refuses_stray_paths(tmp_path):
    (tmp_path / OLD_TS).mkdir()
    (tmp_path / OLD_TS / "open_orders.json").write_text("{}")
    with pytest.raises(SystemExit):
        # a load-input file inside a run dir — never a valid target
        prune.validate_target(tmp_path / OLD_TS / "open_orders.json", tmp_path)
    with pytest.raises(SystemExit):
        # a non-run entry at the bronze root
        prune.validate_target(tmp_path / "schwab-api.db", tmp_path)
    with pytest.raises(SystemExit):
        # a run dir whose parent is not the bronze root
        prune.validate_target(tmp_path.parent / OLD_TS, tmp_path)


def test_validate_target_refuses_symlink(tmp_path):
    external = tmp_path / "ext"
    external.mkdir()
    link = tmp_path / OLD_TS
    link.symlink_to(external, target_is_directory=True)
    with pytest.raises(SystemExit):
        prune.validate_target(link, tmp_path)
