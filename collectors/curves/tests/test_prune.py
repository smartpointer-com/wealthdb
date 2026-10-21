"""
Unit tests for the curves collector's prune.py.

curves' only bronze-resident debug artefact is the HTTP trace `download
--debug` writes under <run>/screenshots/ (debug_subdirs); every other file
in a complete run dir (run.json + each <portal>.csv) is a load input. The
shared engine's own suite (shared/collectorkit/tests/test_prune.py)
covers the age guard, symlinks, corrupt manifests and --dry-run. These
tests cover what curves' PruneConfig decides.

tmp_path bronze trees, invented values only.
"""

from __future__ import annotations

import json
import os
import time
from pathlib import Path

import pytest

import prune

OLD_TS = "20260101T010000Z"
STALE_S = 3 * 3600                    # comfortably past the 1h default


def make_dump(root: Path, slug: str, status: str | None = "complete",
              run_json: bool = True, screenshots: bool = False,
              age_s: float = 0.0) -> Path:
    """A bronze run dir shaped like a real curves dump: each portal's CSV
    answer plus a run.json manifest. age_s backdates every mtime."""
    d = root / slug
    d.mkdir(parents=True)
    if screenshots:
        shots = d / "screenshots"
        shots.mkdir()
        (shots / "http-trace.jsonl").write_text(
            '{"method": "GET", "url": "https://x.invalid/a", "status": 200}\n')
    (d / "ecb.csv").write_text("KEY,TIME_PERIOD,OBS_VALUE\n")
    (d / "snb.csv").write_text('"Date";"LAUFZEIT";"Value"\n')
    if run_json:
        manifest = {"slug": slug, "portals": {
            "ecb": {"file": "ecb.csv"}, "snb": {"file": "snb.csv"}},
            "series": {}}
        if status is not None:
            manifest["status"] = status
        (d / "run.json").write_text(json.dumps(manifest))
    if age_s:
        t = time.time() - age_s
        for p in list(d.rglob("*")) + [d]:
            os.utime(p, (t, t), follow_symlinks=False)
    return d


def run_main(root: Path, *extra: str) -> int:
    return prune.main(["--bronze-dir", str(root), *extra])


def test_main_reclaims_debug_trace_and_noncomplete_dumps(tmp_path):
    complete = make_dump(tmp_path, OLD_TS, screenshots=True)
    crashed = make_dump(tmp_path, "20260102T010000Z", run_json=False,
                        age_s=STALE_S)
    in_progress = make_dump(tmp_path, "20260103T010000Z",
                            status="in-progress", age_s=STALE_S)
    assert run_main(tmp_path) == 0
    assert not (complete / "screenshots").exists()
    for name in ("run.json", "ecb.csv", "snb.csv"):
        assert (complete / name).exists()
    assert not crashed.exists()
    assert not in_progress.exists()


def test_statusless_manifest_kept_as_complete(tmp_path):
    d = make_dump(tmp_path, OLD_TS, status=None, age_s=STALE_S)
    assert run_main(tmp_path) == 0
    assert (d / "run.json").exists() and (d / "snb.csv").exists()


def test_validate_target_accepts_debug_subdir_only(tmp_path):
    (tmp_path / OLD_TS / "screenshots").mkdir(parents=True)
    prune.CONFIG.validate_target(tmp_path / OLD_TS / "screenshots", tmp_path)
    for name in ("run.json", "ecb.csv"):
        with pytest.raises(SystemExit):
            prune.CONFIG.validate_target(tmp_path / OLD_TS / name, tmp_path)
