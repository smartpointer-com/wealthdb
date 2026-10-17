"""
Unit tests for prune.py (viac).

tmp_path bronze trees, synthetic fixtures only (placeholder portfolio
numbers / doc ids / bytes — no real holdings or account data). viac drives
no browser, so its only bronze-resident debug artefact is the HTTP trace
`download --debug` writes under <run>/screenshots/ (``debug_subdirs``).
The shared engine's own suite (shared/collectorkit/tests/test_prune.py)
covers the age guard, symlinks, corrupt manifests and --dry-run. These
tests cover what viac's PruneConfig decides:

  * main() reclaims a complete dump's --debug trace and deletes a stale
    non-complete dump whole; every load input is kept
  * a legacy statusless manifest is complete, unless it carries
    dry_run:true (a legacy --dry-run shell), which is deleted
  * hard-linked PDFs: deleting a non-complete dump preserves the inode
    while a complete dump still links it (no load input lost)
  * validate_target accepts screenshots/, never a load-input subdir
"""

from __future__ import annotations

import json
import os
import sys
import time
from pathlib import Path

import pytest

sys.path.insert(0, str(Path(__file__).resolve().parent.parent))

import prune  # noqa: E402


# ============================================================
# Fixtures
# ============================================================

OLD_TS = "20260101T010000Z"          # older than any guard by slug
STALE_S = 3 * 3600                    # comfortably past the 1h default
PORT = "3.111.222.333.01"            # synthetic p3a portfolio number
DOCID = "DOC0000001"                 # synthetic document number


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
# What viac's PruneConfig decides
# ============================================================

def test_main_reclaims_debug_trace_and_noncomplete_dumps(tmp_path):
    # The --debug HTTP trace is a diagnostic, not a load input, so prune
    # reclaims it while every artefact beside it survives. A stale dump
    # with no run.json is not complete and goes whole.
    d = make_dump(tmp_path, OLD_TS, status="complete", screenshots=True)
    crashed = make_dump(tmp_path, "20260102T010000Z", run_json=False,
                        age_s=STALE_S)
    assert run_main(tmp_path) == 0
    assert not (d / "screenshots").exists()
    assert _load_inputs_present(d)
    assert not crashed.exists()


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


def test_legacy_dry_run_shell_deleted(tmp_path):
    # A statusless manifest carrying dry_run:true is the shape a legacy
    # --dry-run left behind. It is NON_COMPLETE (matching the forward
    # status="dry-run") and prunable once quiescent.
    d = make_dump(tmp_path, OLD_TS, status=None, dry_run=True, age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


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


def test_validate_target_accepts_debug_subdir(tmp_path):
    (tmp_path / OLD_TS / "screenshots").mkdir(parents=True)
    prune.CONFIG.validate_target(tmp_path / OLD_TS / "screenshots", tmp_path)


def test_validate_target_refuses_load_inputs(tmp_path):
    with pytest.raises(SystemExit):
        # A load-input subdir of a run dir is never a valid target — only
        # the screenshots/ debug subdir is — so it must be refused.
        prune.CONFIG.validate_target(tmp_path / OLD_TS / "documents", tmp_path)
