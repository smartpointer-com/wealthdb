"""
Unit tests for prune.py (viac).

tmp_path bronze trees, synthetic fixtures only (placeholder portfolio
numbers / doc ids / bytes — no real holdings or account data). viac drives
no browser, so its only bronze-resident debug artefact is the HTTP trace
`download --debug` writes under <run>/screenshots/ (``debug_subdirs``);
prune reclaims that plus whole *non-complete* run dirs, and never a load
input. Covers:

  * complete dump (status="complete" or a legacy statusless manifest):
    every load input kept, only a --debug trace reclaimed
  * non-complete dumps (absent run.json / status="in-progress" /
    status="dry-run" / legacy statusless dry_run:true) deleted whole
    once quiescent
  * in-flight guard keyed on write activity, not slug age: a long walk
    (old slug, fresh writes, no terminal run.json) is protected
  * unreadable / corrupt run.json is UNKNOWN -> skipped, never deleted
  * calendar-invalid slug is skipped, not a crash
  * hard-linked PDFs: deleting a non-complete dump preserves the inode
    while a complete dump still links it (no load input lost)
  * symlinked run dirs are left alone
  * --dry-run deletes nothing
  * non-run entries at the bronze root are never touched
  * validate_target refuses paths outside the expected shape
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
PORT = "3.111.222.333.01"            # synthetic p3a portfolio number
DOCID = "DOC0000001"                 # synthetic document number


def fresh_slug(age_s: float = 0.0) -> str:
    dt = datetime.fromtimestamp(time.time() - age_s, tz=timezone.utc)
    return dt.strftime(FRESH_FMT)


def make_dump(root: Path, slug: str, *, status: str | None = "complete",
              dry_run: bool = False, run_json: bool = True,
              screenshots: bool = False,
              pdf_link_from: Path | None = None, age_s: float = 0.0) -> Path:
    """Build a synthetic viac bronze run dir with representative load
    inputs. ``status=None`` writes a statusless (legacy) manifest;
    ``run_json=False`` writes none at all. ``screenshots`` adds the
    ``--debug`` HTTP trace, off by default to mirror a download without
    ``--debug``. ``pdf_link_from`` hard-links the document PDF from another
    dump (mirrors fetch_pdf's os.link dedup). ``age_s`` backdates every
    mtime so the write-activity guard sees an abandoned dump; the default
    (0) leaves it fresh."""
    d = root / slug
    (d / "wealth").mkdir(parents=True)
    _write_json(d / "wealth" / "portfolio-inventory.json",
                {"p3a": [{"number": PORT, "name": "Portfolio", "state": "ACTIVE"}],
                 "pvb": [], "inv": []})
    _write_json(d / "wealth" / "summary.json", {"dailyWealth": []})
    _write_json(d / "wealth" / "allocation.json", {})          # bronze, not a load input
    (d / "positions" / PORT).mkdir(parents=True)
    _write_json(d / "positions" / PORT / "strategy.json", {"currentStrategy": {}})
    _write_json(d / "positions" / PORT / "assets.json",
                {"cashAmount": 0, "assetsByClasses": {}})
    _write_json(d / "positions" / PORT / "fees.json", {})      # bronze, not a load input
    _write_json(d / "customer.json", {"id": "synthetic"})      # bronze, not a load input
    (d / "transactions").mkdir()
    _write_json(d / "transactions" / "all.json", {"transactions": {}})
    (d / "documents").mkdir()
    _write_json(d / "documents" / "index.json",
                [{"documentNumber": DOCID, "type": "STATEMENT"}])
    pdf = d / "documents" / f"{DOCID}.pdf"
    if pdf_link_from is not None:
        os.link(pdf_link_from, pdf)
    else:
        pdf.write_bytes(b"%PDF-1.4 synthetic")
    if screenshots:
        (d / "screenshots").mkdir()
        (d / "screenshots" / "http-trace.jsonl").write_text(
            '{"method": "GET", "url": "https://x.invalid/a", "status": 200}\n')
    if run_json:
        manifest: dict = {"timestamp": slug, "dry_run": dry_run,
                          "documents": {"total": 1}}
        if status is not None:
            manifest["status"] = status
        (d / "run.json").write_text(json.dumps(manifest))
    if age_s:
        backdate(d, age_s)
    return d


def _write_json(path: Path, obj) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(obj), encoding="utf-8")


def backdate(path: Path, age_s: float) -> None:
    t = time.time() - age_s
    for p in list(path.rglob("*")) + [path]:
        os.utime(p, (t, t), follow_symlinks=False)


def run_main(root: Path, *extra: str) -> int:
    return prune.main(["--bronze-dir", str(root), *extra])


def _load_inputs_present(d: Path) -> bool:
    return all((d / rel).exists() for rel in (
        "run.json",
        "wealth/portfolio-inventory.json",
        "wealth/summary.json",
        f"positions/{PORT}/strategy.json",
        f"positions/{PORT}/assets.json",
        "transactions/all.json",
        "documents/index.json",
        f"documents/{DOCID}.pdf",
    ))


# ============================================================
# Complete dumps: inputs kept, the --debug trace reclaimed
# ============================================================

def test_complete_dump_untouched(tmp_path):
    d = make_dump(tmp_path, OLD_TS, status="complete")
    assert run_main(tmp_path) == 0
    assert d.exists()
    assert _load_inputs_present(d)


def test_complete_dump_debug_trace_pruned_inputs_kept(tmp_path):
    # The --debug HTTP trace is a diagnostic, not a load input, so prune
    # reclaims it while every artefact beside it survives.
    d = make_dump(tmp_path, OLD_TS, status="complete", screenshots=True)
    assert run_main(tmp_path) == 0
    assert not (d / "screenshots").exists()
    assert _load_inputs_present(d)


def test_statusless_manifest_kept_as_legacy_complete(tmp_path):
    # A run.json with no `status` key predates the status lifecycle,
    # where the manifest was written only at the end, so its presence
    # (with dry_run:false) means the dump finished: keep it and all its
    # load inputs. Current walks always carry a status key, so this
    # branch only ever sees such dumps.
    d = make_dump(tmp_path, OLD_TS, status=None, dry_run=False, age_s=STALE_S)
    run_main(tmp_path)
    assert d.exists()
    assert _load_inputs_present(d)


def test_fresh_complete_dump_untouched(tmp_path):
    # The write-activity guard protects non-complete dumps only; a
    # finalised run.json means the walk is over regardless of freshness.
    d = make_dump(tmp_path, fresh_slug(age_s=60), status="complete")
    run_main(tmp_path)
    assert d.exists()
    assert _load_inputs_present(d)


# ============================================================
# Non-complete dumps: deleted once quiescent
# ============================================================

def test_missing_run_json_dump_deleted(tmp_path):
    d = make_dump(tmp_path, OLD_TS, run_json=False, age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


def test_dry_run_status_dump_deleted(tmp_path):
    d = make_dump(tmp_path, OLD_TS, status="dry-run", age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


def test_in_progress_status_dump_deleted_when_stale(tmp_path):
    # An abandoned walk leaves status="in-progress" (the marker
    # download.py drops at run-dir creation); once quiescent it is
    # prunable.
    d = make_dump(tmp_path, OLD_TS, status="in-progress", age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


def test_legacy_dry_run_shell_deleted(tmp_path):
    # A statusless manifest carrying dry_run:true is the shape a legacy
    # --dry-run left behind. It is NON_COMPLETE (matching the forward
    # status="dry-run") and prunable once quiescent.
    d = make_dump(tmp_path, OLD_TS, status=None, dry_run=True, age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


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
    assert (d / "transactions" / "all.json").exists()


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
    assert _load_inputs_present(d)


def test_unreadable_run_json_kept(tmp_path):
    # An I/O error reading run.json (here: run.json is a directory, a
    # deterministic OSError) must not classify the dump non-complete and
    # delete its load inputs.
    d = make_dump(tmp_path, OLD_TS, run_json=False, age_s=STALE_S)
    (d / "run.json").mkdir()
    run_main(tmp_path)
    assert d.exists()
    assert (d / "wealth" / "portfolio-inventory.json").exists()


def test_invalid_calendar_slug_skipped_not_crash(tmp_path):
    # A slug that matches the regex but is not a real date (Feb 30) must
    # be skipped, not abort the whole prune.
    bad = make_dump(tmp_path, "20260230T010000Z", run_json=False,
                    age_s=STALE_S)
    good = make_dump(tmp_path, OLD_TS, run_json=False, age_s=STALE_S)
    assert run_main(tmp_path) == 0
    assert bad.exists()          # unparseable age → left alone
    assert not good.exists()     # the valid stale dump still pruned


# ============================================================
# Hard-linked PDFs: deleting a non-complete dump keeps the inode alive
# ============================================================

def test_hardlinked_pdf_survives_noncomplete_dir_deletion(tmp_path):
    # download.py hard-links a PDF that already exists in a prior dump
    # rather than re-fetching. Deleting a whole NON-complete dump that
    # holds such a link must not cost the complete dump its copy — the
    # inode survives while the complete dump still links it.
    complete = make_dump(tmp_path, OLD_TS, status="complete")
    complete_pdf = complete / "documents" / f"{DOCID}.pdf"
    crashed = make_dump(tmp_path, "20260102T010000Z", run_json=False,
                        pdf_link_from=complete_pdf, age_s=STALE_S)
    assert crashed.exists()
    run_main(tmp_path)
    assert not crashed.exists()
    assert complete_pdf.exists()
    assert complete_pdf.read_bytes() == b"%PDF-1.4 synthetic"


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
    assert _load_inputs_present(complete)
    assert crashed.exists()


def test_non_run_entries_never_touched(tmp_path):
    make_dump(tmp_path, OLD_TS, status="complete")
    manual = tmp_path / "manual"
    manual.mkdir()
    (manual / "upload.pdf").write_bytes(b"%PDF synthetic")
    (tmp_path / "viac.db").write_bytes(b"sqlite fake")
    run_main(tmp_path)
    assert (manual / "upload.pdf").exists()
    assert (tmp_path / "viac.db").exists()


def test_missing_bronze_dir_exits(tmp_path):
    with pytest.raises(SystemExit):
        prune.main(["--bronze-dir", str(tmp_path / "nope")])


# ============================================================
# Target validation
# ============================================================

def test_validate_target_accepts_run_dir(tmp_path):
    (tmp_path / OLD_TS).mkdir()
    prune.validate_target(tmp_path / OLD_TS, tmp_path)


def test_validate_target_accepts_debug_subdir(tmp_path):
    (tmp_path / OLD_TS / "screenshots").mkdir(parents=True)
    prune.validate_target(tmp_path / OLD_TS / "screenshots", tmp_path)


def test_validate_target_refuses_stray_paths(tmp_path):
    with pytest.raises(SystemExit):
        prune.validate_target(tmp_path / "manual", tmp_path)
    with pytest.raises(SystemExit):
        # A load-input subdir of a run dir is never a valid target — only
        # the screenshots/ debug subdir is — so it must be refused.
        prune.validate_target(tmp_path / OLD_TS / "documents", tmp_path)
    with pytest.raises(SystemExit):
        prune.validate_target(tmp_path.parent / OLD_TS, tmp_path)


def test_validate_target_refuses_symlink(tmp_path):
    external = tmp_path / "ext"
    external.mkdir()
    link = tmp_path / OLD_TS
    link.symlink_to(external, target_is_directory=True)
    with pytest.raises(SystemExit):
        prune.validate_target(link, tmp_path)
