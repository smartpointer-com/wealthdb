"""
Unit tests for relevate's prune.py.

tmp_path bronze trees, synthetic fixtures only (no real ids /
balances / account numbers). relevate drives no browser, so its only
bronze-resident debug artefact is the HTTP trace `download --debug`
writes under <run>/screenshots/ (``debug_subdirs``). The shared engine's
own suite (shared/collectorkit/tests/test_prune.py) covers the age
guard, symlinks, corrupt manifests and --dry-run. These tests cover
what relevate's PruneConfig decides:

  * main() reclaims a complete dump's --debug trace and deletes a stale
    non-complete dump whole; every load input (accounts/, portfolios/,
    documents/*.pdf) survives
  * relevate's own "incomplete" status is not complete
  * a pre-`status` manifest is kept whatever its ended_at or dry_run,
    since load reads it
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
SLUG = "abcdef0123456789"            # synthetic sha256(externalId)[:16]
DOC_ID = "42"                        # synthetic Relevate document id


def make_dump(root: Path, slug: str, *,
              status: str | None = "complete",
              ended_at: str | None = "2026-01-01T01:00:05+00:00",
              dry_run: bool = False,
              run_json: bool = True,
              screenshots: bool = False,
              age_s: float = 0.0) -> Path:
    """Build a synthetic relevate bronze run dir with the real load-
    input layout. ``status=None`` writes a statusless manifest,
    classified on ``ended_at`` / ``dry_run`` alone. ``screenshots``
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
# What relevate's PruneConfig decides
# ============================================================

def test_main_reclaims_debug_trace_and_noncomplete_dumps(tmp_path):
    # The --debug HTTP trace is a diagnostic, not a load input, so prune
    # reclaims it while every artefact beside it survives, the document
    # PDF read cross-dump included. A stale dump with no run.json is not
    # complete and goes whole.
    d = make_dump(tmp_path, OLD_TS, status="complete", screenshots=True,
                  age_s=STALE_S)
    crashed = make_dump(tmp_path, "20260102T010000Z", run_json=False,
                        age_s=STALE_S)
    assert run_main(tmp_path) == 0
    assert not (d / "screenshots").exists()
    assert_inputs_present(d)
    assert (d / "documents" / f"{DOC_ID}.pdf").read_bytes().startswith(b"%PDF")
    assert not crashed.exists()


def test_incomplete_status_dump_deleted_when_stale(tmp_path):
    # Master enumeration failed -> finish(status="incomplete").
    d = make_dump(tmp_path, OLD_TS, status="incomplete", age_s=STALE_S)
    run_main(tmp_path)
    assert not d.exists()


def test_legacy_complete_dump_kept_entirely(tmp_path):
    # A statusless run.json with ended_at stamped on a non-dry run is a
    # pre-`status` complete dump: keep everything.
    d = make_dump(tmp_path, OLD_TS, status=None,
                  ended_at="2026-01-01T01:00:05+00:00", dry_run=False,
                  age_s=STALE_S)
    assert run_main(tmp_path) == 0
    assert d.exists()
    assert_inputs_present(d)


def test_legacy_ended_at_null_kept(tmp_path):
    # A pre-`status` run.json whose ended_at was never stamped. load
    # reads every statusless dump, so prune keeps it: deleting it would
    # orphan rows already loaded from it.
    d = make_dump(tmp_path, OLD_TS, status=None, ended_at=None,
                  age_s=STALE_S)
    run_main(tmp_path)
    assert d.exists()
    assert_inputs_present(d)


def test_legacy_dry_run_kept(tmp_path):
    # A pre-`status` manifest with dry_run=true is kept for the same
    # reason: load reads it.
    d = make_dump(tmp_path, OLD_TS, status=None,
                  ended_at="2026-01-01T01:00:05+00:00", dry_run=True,
                  age_s=STALE_S)
    run_main(tmp_path)
    assert d.exists()


def test_validate_target_accepts_debug_subdir(tmp_path):
    (tmp_path / OLD_TS / "screenshots").mkdir(parents=True)
    prune.CONFIG.validate_target(tmp_path / OLD_TS / "screenshots", tmp_path)


def test_validate_target_refuses_load_inputs(tmp_path):
    (tmp_path / OLD_TS / "accounts").mkdir(parents=True)
    with pytest.raises(SystemExit):
        prune.CONFIG.validate_target(tmp_path / OLD_TS / "accounts", tmp_path)
