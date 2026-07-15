"""
Unit tests for ubs-psn's prune.py.

tmp_path bronze trees with the ubs-psn layout: run dirs
``<UTC-ts>/`` holding a flat set of ``<ORDERTYPE>.zip`` files, an
optional ``run.json`` status marker, and — only under `download
--debug` — a ``screenshots/`` SFTP-listing capture (``debug_subdirs``).
The load-bearing invariant is SAFETY: the PSN zips are
irreplaceable (UBS deletes each file server-side on download), so a
run dir holding ANY zip must never be deleted, even when a crash left
``status="in-progress"`` or no manifest at all. Only a truly zip-less
shell may be reclaimed once quiescent — and only the listing may ever
be reclaimed from a dump that holds zips.

Covers:
  * zip-bearing dumps kept in every completeness state — complete /
    statusless / no-manifest / in-progress(crash) / admin-zip-only —
    with every zip surviving; only a --debug listing is reclaimed
  * the trap: a zip-bearing in-progress crash is KEPT (has-zip
    short-circuits before the status field is consulted)
  * has-zip beats slug age and the in-flight guard
  * zip-less shells deleted whole once quiescent; kept while fresh /
    in-flight (guard keyed on write activity, not slug age)
  * unreadable / corrupt run.json is UNKNOWN → skipped, never deleted
  * calendar-invalid slug skipped, not a crash
  * symlinked run dirs left alone
  * --dry-run deletes nothing
  * non-run entries at the bronze root (silver DB) never touched
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

ZIP_BYTES = b"PK\x03\x04synthetic-zip"


def fresh_slug(age_s: float = 0.0) -> str:
    dt = datetime.fromtimestamp(time.time() - age_s, tz=timezone.utc)
    return dt.strftime(FRESH_FMT)


def make_dump(root: Path, slug: str, *,
              zips: tuple[str, ...] = ("ZAH.zip", "Z40.zip"),
              status: str | None = "complete", run_json: bool = True,
              screenshots: bool = False, age_s: float = 0.0) -> Path:
    """Build a ubs-psn bronze run dir.

    ``zips`` are the flat ``<ORDERTYPE>.zip`` files inside the run dir
    (empty tuple → a zip-less shell). ``status`` is the run.json status
    field (``None`` writes a statusless ``{}`` manifest); ``run_json``
    False omits the manifest entirely.
    ``screenshots`` adds the ``--debug`` SFTP listing, off by default to
    mirror a pull without ``--debug``. ``age_s`` backdates every mtime so
    the write-activity guard sees an abandoned dump; the default (0)
    leaves it fresh.
    """
    d = root / slug
    d.mkdir(parents=True)
    for z in zips:
        (d / z).write_bytes(ZIP_BYTES)
    if screenshots:
        (d / "screenshots").mkdir()
        (d / "screenshots" / "sftp-listing.txt").write_text(
            "host-key: SHA256:synthetic\n\ndownload/ZAH/: ZAH.zip (4 bytes)\n")
    if run_json:
        (d / "run.json").write_text(
            json.dumps({"status": status} if status is not None else {}))
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
# Zip-bearing dumps: always kept, nothing pruned
# ============================================================

def test_complete_zip_dump_kept_nothing_pruned(tmp_path):
    d = make_dump(tmp_path, OLD_TS, status="complete", age_s=STALE_S)
    assert run_main(tmp_path) == 0
    assert d.exists()
    assert (d / "ZAH.zip").exists()
    assert (d / "Z40.zip").exists()
    assert (d / "run.json").exists()


def test_statusless_zip_dump_kept(tmp_path):
    # A run.json with no `status` key (or, here, an empty object) predates
    # the status lifecycle; the has-zip guard keeps it regardless.
    d = make_dump(tmp_path, OLD_TS, status=None, age_s=STALE_S)
    run_main(tmp_path)
    assert d.exists()
    assert (d / "ZAH.zip").exists()


def test_no_manifest_zip_dump_kept(tmp_path):
    # The common case for EVERY existing ubs-psn dump: no run.json at all.
    # has-zip → COMPLETE → kept, load inputs intact.
    d = make_dump(tmp_path, OLD_TS, run_json=False, age_s=STALE_S)
    run_main(tmp_path)
    assert d.exists()
    assert (d / "ZAH.zip").exists()
    assert (d / "Z40.zip").exists()


def test_in_progress_crash_with_zips_kept(tmp_path):
    # THE TRAP: a crash mid-download leaves status="in-progress" alongside
    # already-downloaded (irreplaceable) zips. The has-zip check MUST
    # short-circuit to COMPLETE *before* the status field is consulted;
    # otherwise the shared engine would classify NON_COMPLETE and rmtree
    # the whole dir, destroying data `load` consumes.
    d = make_dump(tmp_path, OLD_TS, status="in-progress", age_s=STALE_S)
    run_main(tmp_path)
    assert d.exists()
    assert (d / "ZAH.zip").exists()
    assert (d / "Z40.zip").exists()


def test_admin_zip_only_dump_kept(tmp_path):
    # A dir holding only the non-Z EBICS admin zips (HAC/PTK) — raw bronze
    # data even though load never reads them. The guard globs "*.zip", not
    # "Z*.zip", so these keep the dir COMPLETE and undeleted.
    d = make_dump(tmp_path, OLD_TS, zips=("HAC.zip", "PTK.zip"),
                  run_json=False, age_s=STALE_S)
    run_main(tmp_path)
    assert d.exists()
    assert (d / "HAC.zip").exists()
    assert (d / "PTK.zip").exists()


def test_zip_dump_kept_despite_fresh_slug(tmp_path):
    # has-zip protection is independent of freshness: a fresh, finalised
    # dump is kept, zips and all.
    d = make_dump(tmp_path, fresh_slug(age_s=60), status="complete")
    run_main(tmp_path)
    assert d.exists()
    assert (d / "ZAH.zip").exists()


def test_complete_dump_debug_listing_pruned_zips_kept(tmp_path):
    # The --debug listing is a diagnostic, not a load input, so prune
    # reclaims it while the irreplaceable zips beside it survive.
    d = make_dump(tmp_path, OLD_TS, status="complete", screenshots=True)
    assert run_main(tmp_path) == 0
    assert not (d / "screenshots").exists()
    assert (d / "ZAH.zip").exists()
    assert (d / "Z40.zip").exists()


def test_zipless_empty_shell_from_debug_pull_deleted_when_quiescent(tmp_path):
    # A --debug pull that found nothing queued keeps its shell + listing so
    # the listing is inspectable; prune reclaims the whole thing once it
    # goes quiescent — the normal debug-artefact lifecycle.
    d = make_dump(tmp_path, OLD_TS, zips=(), status="empty",
                  screenshots=True, age_s=STALE_S)
    assert run_main(tmp_path) == 0
    assert not d.exists()


# ============================================================
# Zip-less shells: deleted once quiescent
# ============================================================

def test_zipless_in_progress_shell_deleted_when_quiescent(tmp_path):
    # A run dir minted (in-progress marker written) but abandoned before
    # the first file landed. No irreplaceable data → prunable once stale.
    d = make_dump(tmp_path, OLD_TS, zips=(), status="in-progress",
                  age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


def test_zipless_no_manifest_shell_deleted(tmp_path):
    d = make_dump(tmp_path, OLD_TS, zips=(), run_json=False, age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


def test_zipless_dry_run_status_shell_deleted(tmp_path):
    d = make_dump(tmp_path, OLD_TS, zips=(), status="dry-run", age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


def test_zipless_shell_deleted_with_zero_min_age(tmp_path):
    d = make_dump(tmp_path, OLD_TS, zips=(), run_json=False, age_s=STALE_S)
    run_main(tmp_path, "--min-age-hours", "0")
    assert not d.exists()


# ============================================================
# In-flight guard: keyed on write activity, not slug age
# ============================================================

def test_zipless_shell_kept_while_fresh(tmp_path):
    d = make_dump(tmp_path, fresh_slug(age_s=60), zips=(),
                  status="in-progress")
    run_main(tmp_path)
    assert d.exists()


def test_zipless_long_walk_old_slug_fresh_writes_kept(tmp_path):
    # A download in flight: slug minted hours ago but the marker was just
    # written (fresh mtime, no zips yet). The write-activity guard must NOT
    # classify it abandoned — a real download about to write its first zip.
    d = make_dump(tmp_path, OLD_TS, zips=(), status="in-progress", age_s=0.0)
    run_main(tmp_path)
    assert d.exists()


# ============================================================
# Unreadable / corrupt manifests: never deleted
# ============================================================

def test_corrupt_run_json_zip_dump_kept(tmp_path):
    d = make_dump(tmp_path, OLD_TS, age_s=STALE_S)
    (d / "run.json").write_text("{not valid json")
    run_main(tmp_path)
    assert d.exists()
    assert (d / "ZAH.zip").exists()


def test_corrupt_run_json_zipless_shell_kept(tmp_path):
    # Even a zip-less shell with a corrupt manifest is UNKNOWN, not proof of
    # incompleteness — skip, never delete.
    d = make_dump(tmp_path, OLD_TS, zips=(), age_s=STALE_S)
    (d / "run.json").write_text("{not valid json")
    run_main(tmp_path)
    assert d.exists()


def test_unreadable_run_json_kept(tmp_path):
    # run.json is a directory → a deterministic OSError on read → UNKNOWN.
    d = make_dump(tmp_path, OLD_TS, zips=(), run_json=False, age_s=STALE_S)
    (d / "run.json").mkdir()
    run_main(tmp_path)
    assert d.exists()


def test_invalid_calendar_slug_skipped_not_crash(tmp_path):
    # Feb 30 matches the slug regex but is not a real date: skip (can't age
    # it), don't abort the whole prune.
    bad = make_dump(tmp_path, "20260230T010000Z", zips=(), run_json=False,
                    age_s=STALE_S)
    good = make_dump(tmp_path, OLD_TS, zips=(), run_json=False, age_s=STALE_S)
    assert run_main(tmp_path) == 0
    assert bad.exists()          # unparseable age → left alone
    assert not good.exists()     # the valid stale shell still pruned


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
    shell = make_dump(tmp_path, "20260102T010000Z", zips=(), run_json=False,
                      age_s=STALE_S)
    run_main(tmp_path, "--dry-run")
    assert (complete / "ZAH.zip").exists()
    assert shell.exists()


def test_non_run_entries_never_touched(tmp_path):
    make_dump(tmp_path, OLD_TS, status="complete")
    (tmp_path / "ubs-psn.db").write_bytes(b"sqlite fake")
    stray = tmp_path / "notes"
    stray.mkdir()
    (stray / "readme.txt").write_text("keep me")
    run_main(tmp_path)
    assert (tmp_path / "ubs-psn.db").exists()
    assert (stray / "readme.txt").exists()


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
    (tmp_path / OLD_TS / "ZAH.zip").write_bytes(ZIP_BYTES)
    with pytest.raises(SystemExit):
        prune.validate_target(tmp_path / "notes", tmp_path)
    with pytest.raises(SystemExit):
        # a zip inside a run dir is a load input, never a target
        prune.validate_target(tmp_path / OLD_TS / "ZAH.zip", tmp_path)
    with pytest.raises(SystemExit):
        prune.validate_target(tmp_path.parent / OLD_TS, tmp_path)


def test_validate_target_refuses_symlink(tmp_path):
    external = tmp_path / "ext"
    external.mkdir()
    link = tmp_path / OLD_TS
    link.symlink_to(external, target_is_directory=True)
    with pytest.raises(SystemExit):
        prune.validate_target(link, tmp_path)
