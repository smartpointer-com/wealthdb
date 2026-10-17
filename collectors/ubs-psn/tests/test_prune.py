"""
Unit tests for ubs-psn's prune.py.

tmp_path bronze trees with the ubs-psn layout: run dirs
``<UTC-ts>/`` holding a flat set of ``<ORDERTYPE>.zip`` files, an
optional ``run.json`` status marker, a ``listing.json`` pre-pull
record, and — on legacy dumps only — a ``screenshots/`` SFTP-listing
capture (``debug_subdirs``). The load-bearing invariant is SAFETY: a
fetched queue zip cannot be fetched again (UBS deletes it server-side
on download; its dated archive copy ages out after ~2 months), so a
run dir holding ANY zip must never be deleted, even when a crash left
``status="in-progress"`` or no manifest at all. Only a truly zip-less
shell may be reclaimed once quiescent — and only the legacy
``screenshots/`` capture, never ``listing.json``, may be reclaimed
from a dump that holds zips.

Covers what ubs-psn's configuration decides; the shared engine's own
envelope (symlinks, unreadable manifests, the age guard, --dry-run,
target validation) is tested in collectorkit:
  * zip-bearing dumps kept in every completeness state — complete /
    statusless / no-manifest / in-progress(crash) / admin-zip-only —
    with every zip surviving; only a legacy screenshots/ capture is
    reclaimed, and listing.json survives it
  * the trap: a zip-bearing in-progress crash is KEPT (has-zip
    short-circuits before the status field is consulted)
  * zip-less shells (crash shells and the status="empty" shell an
    empty pull leaves) deleted whole once quiescent
  * the debug subdir prune may target is screenshots/
"""

from __future__ import annotations

import json
import os
import sys
import time
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import prune  # noqa: E402


# ============================================================
# Fixtures
# ============================================================

OLD_TS = "20260101T010000Z"          # older than any guard by slug
STALE_S = 3 * 3600                    # comfortably past the 1h default

ZIP_BYTES = b"PK\x03\x04synthetic-zip"


def make_dump(root: Path, slug: str, *,
              zips: tuple[str, ...] = ("ZAH.zip", "Z40.zip"),
              status: str | None = "complete", run_json: bool = True,
              listing: bool = False, screenshots: bool = False,
              age_s: float = 0.0) -> Path:
    """Build a ubs-psn bronze run dir.

    ``zips`` are the flat ``<ORDERTYPE>.zip`` files inside the run dir
    (empty tuple → a zip-less shell). ``status`` is the run.json status
    field (``None`` writes a statusless ``{}`` manifest); ``run_json``
    False omits the manifest entirely. ``listing`` adds the
    ``listing.json`` pre-pull record every download run writes;
    ``screenshots`` adds the legacy ``--debug`` SFTP-listing capture.
    ``age_s`` backdates every mtime so the write-activity guard sees an
    abandoned dump; the default (0) leaves it fresh.
    """
    d = root / slug
    d.mkdir(parents=True)
    for z in zips:
        (d / z).write_bytes(ZIP_BYTES)
    if listing:
        (d / "listing.json").write_text(json.dumps({
            "captured_at": "2026-01-01T01:00:00Z",
            "host_key": "SHA256:synthetic",
            "order_types": {"ZAH": {"status": "listed", "entries": []}},
        }))
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
    # A run.json with no `status` key (here, an empty object): the has-zip
    # guard keeps it regardless.
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
    # already-downloaded (consumed) zips. The has-zip check MUST
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


def test_complete_dump_legacy_capture_pruned_zips_and_listing_kept(tmp_path):
    # The legacy screenshots/ capture is a diagnostic, not a load input, so
    # prune reclaims it while the consumed zips beside it survive — and
    # listing.json, the run's provenance record, survives too.
    d = make_dump(tmp_path, OLD_TS, status="complete", listing=True,
                  screenshots=True)
    assert run_main(tmp_path) == 0
    assert not (d / "screenshots").exists()
    assert (d / "ZAH.zip").exists()
    assert (d / "Z40.zip").exists()
    assert (d / "listing.json").exists()


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


def test_zipless_empty_shell_deleted_when_quiescent(tmp_path):
    # A pull that found nothing queued keeps its shell (run.json +
    # listing.json) so the listing that explains it is inspectable; prune
    # reclaims the whole thing once it goes quiescent.
    d = make_dump(tmp_path, OLD_TS, zips=(), status="empty",
                  listing=True, age_s=STALE_S)
    assert run_main(tmp_path) == 0
    assert not d.exists()


def test_zipless_no_manifest_shell_deleted(tmp_path):
    d = make_dump(tmp_path, OLD_TS, zips=(), run_json=False, age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


# ============================================================
# Target validation: the debug subdir is screenshots/
# ============================================================

def test_validate_target_accepts_debug_subdir(tmp_path):
    (tmp_path / OLD_TS / "screenshots").mkdir(parents=True)
    prune.CONFIG.validate_target(tmp_path / OLD_TS / "screenshots", tmp_path)
