"""
Unit tests for the fred collector's prune.py.

fred's only bronze-resident debug artefact is the HTTP trace `download
--debug` writes under <run>/screenshots/ (debug_subdirs); every other file
in a complete run dir (run.json + each <series_id>.json) is a load input.
The shared engine's own suite (shared/collectorkit/tests/test_prune.py)
covers the age guard, symlinks, corrupt manifests and --dry-run. These
tests cover what fred's PruneConfig decides:

  * main() reclaims the --debug trace of a complete dump and deletes a
    stale non-complete dump whole; load inputs are kept
  * a statusless manifest is COMPLETE (lenient classification), kept
  * a complete dump that fetched no series is kept
  * validate_target accepts a run dir's screenshots/, never a load input

tmp_path bronze trees, synthetic round rates only — no real FX data.
"""

from __future__ import annotations

import json
import os
import time
from pathlib import Path

import pytest

import prune


# ============================================================
# Fixtures
# ============================================================

OLD_TS = "20260101T010000Z"          # older than any guard by slug
STALE_S = 3 * 3600                    # comfortably past the 1h default


def _obs_doc(rows):
    return {"observations": [{"date": d, "value": v} for d, v in rows]}


def make_dump(root: Path, slug: str, status: str | None = "complete",
              run_json: bool = True, series: bool = True,
              screenshots: bool = False, age_s: float = 0.0) -> Path:
    """Build a bronze run dir shaped like a real fred dump: one
    ``<series_id>.json`` observations doc per fetched series plus a
    ``run.json`` manifest. ``age_s`` backdates every mtime so the
    write-activity guard sees an abandoned dump; the default (0) leaves it
    fresh. ``status=None`` writes a statusless (pre-`status`) manifest.
    ``screenshots`` adds the ``--debug`` HTTP trace — off by default,
    mirroring a download without ``--debug``."""
    d = root / slug
    d.mkdir(parents=True)
    if screenshots:
        shots = d / "screenshots"
        shots.mkdir()
        (shots / "http-trace.jsonl").write_text(
            '{"method": "GET", "url": "https://x.invalid/a", "status": 200}\n')
    if series:
        (d / "DEXSZUS.json").write_text(json.dumps(_obs_doc([
            ("2020-01-02", "0.9000"), ("2020-01-03", "0.9100")])))
        (d / "DEXUSEU.json").write_text(json.dumps(_obs_doc([
            ("2020-01-02", "1.1200")])))
    if run_json:
        manifest = {
            "slug": slug,
            "series": {
                "DEXSZUS": {"base": "CHF", "quote": "USD", "rows": 2},
                "DEXUSEU": {"base": "USD", "quote": "EUR", "rows": 1},
            },
        }
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


def _series_and_manifest_intact(d: Path) -> None:
    assert (d / "run.json").exists()
    assert (d / "DEXSZUS.json").exists()
    assert (d / "DEXUSEU.json").exists()


# ============================================================
# What fred's PruneConfig decides
# ============================================================

def test_main_reclaims_debug_trace_and_noncomplete_dumps(tmp_path):
    # The --debug HTTP trace is a diagnostic, not a load input, so prune
    # reclaims it while the observations documents beside it survive. A
    # stale dump with no run.json is not complete and goes whole.
    complete = make_dump(tmp_path, OLD_TS, screenshots=True)
    crashed = make_dump(tmp_path, "20260102T010000Z", run_json=False,
                        age_s=STALE_S)
    assert run_main(tmp_path) == 0
    assert not (complete / "screenshots").exists()
    _series_and_manifest_intact(complete)
    assert not crashed.exists()


def test_statusless_manifest_kept_as_legacy_complete(tmp_path):
    # A statusless run.json is written once, at the end of the walk, so its
    # presence means the dump finished: classify COMPLETE, keep it.
    d = make_dump(tmp_path, OLD_TS, status=None, age_s=STALE_S)
    assert run_main(tmp_path) == 0
    assert d.exists()
    _series_and_manifest_intact(d)


def test_total_failure_complete_dump_kept(tmp_path):
    # A run where every series failed writes run.json status="complete"
    # with an empty series dict (the walk finished, just fetched nothing).
    # It is a complete dump and must be kept.
    d = tmp_path / OLD_TS
    d.mkdir()
    (d / "run.json").write_text(json.dumps(
        {"slug": OLD_TS, "series": {}, "status": "complete"}))
    backdate(d, STALE_S)
    assert run_main(tmp_path) == 0
    assert d.exists()
    assert (d / "run.json").exists()


def test_validate_target_accepts_debug_subdir(tmp_path):
    (tmp_path / OLD_TS / "screenshots").mkdir(parents=True)
    prune.CONFIG.validate_target(tmp_path / OLD_TS / "screenshots", tmp_path)


def test_validate_target_refuses_load_inputs(tmp_path):
    (tmp_path / OLD_TS).mkdir()
    (tmp_path / OLD_TS / "run.json").write_text("{}")
    # A load input inside a run dir is never a valid target; only the
    # screenshots/ debug subdir is.
    with pytest.raises(SystemExit):
        prune.CONFIG.validate_target(tmp_path / OLD_TS / "run.json", tmp_path)
    with pytest.raises(SystemExit):
        prune.CONFIG.validate_target(tmp_path / OLD_TS / "DEXSZUS.json", tmp_path)
