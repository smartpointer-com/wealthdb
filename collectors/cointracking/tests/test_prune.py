"""
Unit tests for prune.py (cointracking).

tmp_path bronze trees mirroring cointracking's layout: a run dir holds
run.json + one cu_<id>/ per portfolio with trades.csv / balance.csv /
overview.csv (all load inputs), plus the screenshots/ dir `download
--debug` writes. The shared engine's own suite
(shared/collectorkit/tests/test_prune.py) covers the age guard,
symlinks, corrupt manifests and --dry-run. These tests cover what
cointracking's PruneConfig decides:

  * main() reclaims screenshots/ from a complete dump and deletes a
    stale non-complete dump whole; load inputs are kept
  * statusless manifest classified legacy-complete → kept intact
  * validate_target accepts screenshots/, never a cu_<id>/ load input
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
CU = "1"


def make_dump(root: Path, slug: str, status: str | None = "complete",
              run_json: bool = True, age_s: float = 0.0,
              cu: str = CU, screenshots: bool = True) -> Path:
    """Build a cointracking bronze run dir: run.json + one cu_<id>/
    holding the three load-input CSVs (trades / balance / overview),
    plus the `--debug` screenshots/ capture dir. ``age_s`` backdates
    every file/dir mtime (and the run dir's own) so the write-activity
    guard sees an abandoned dump; the default (0) leaves it fresh.
    Synthetic ids / amounts only."""
    d = root / slug
    port = d / f"cu_{cu}"
    port.mkdir(parents=True)
    (port / "trades.csv").write_text('"Type","Buy"\n"Trade","0.5"\n')
    (port / "balance.csv").write_text('"Cur.","Amount"\n"BTC","0.5"\n')
    (port / "overview.csv").write_text(
        '"Date","BTC Value"\n"2026-01-01","1"\n')
    if screenshots:
        shots = d / "screenshots"
        shots.mkdir(parents=True)
        (shots / "10-portfolios.html").write_text("<html>discovery</html>")
        (shots / "10-portfolios.png").write_bytes(b"\x89PNG fake")
    if run_json:
        meta: dict = {"portfolios": [{"id": cu, "name": "test"}]}
        if status is not None:
            meta["status"] = status
        (d / "run.json").write_text(json.dumps(meta))
    if age_s:
        backdate(d, age_s)
    return d


def backdate(path: Path, age_s: float) -> None:
    t = time.time() - age_s
    for p in list(path.rglob("*")) + [path]:
        os.utime(p, (t, t), follow_symlinks=False)


def run_main(root: Path, *extra: str) -> int:
    return prune.main(["--bronze-dir", str(root), *extra])


def _inputs_intact(d: Path, cu: str = CU) -> bool:
    csvs = all((d / f"cu_{cu}" / name).exists()
               for name in ("trades.csv", "balance.csv", "overview.csv"))
    return csvs and (d / "run.json").exists()


# ============================================================
# What cointracking's PruneConfig decides
# ============================================================

def test_main_reclaims_screenshots_and_noncomplete_dumps(tmp_path):
    # screenshots/ is the only non-load-input a run dir holds, so it is
    # the only thing a complete dump gives up. A stale dump with no
    # run.json is not complete and goes whole.
    d = make_dump(tmp_path, OLD_TS)
    crashed = make_dump(tmp_path, "20260102T010000Z", run_json=False,
                        age_s=STALE_S)
    assert run_main(tmp_path) == 0
    assert d.exists()
    assert not (d / "screenshots").exists()
    assert _inputs_intact(d)
    assert not crashed.exists()


def test_statusless_manifest_kept_as_legacy_complete(tmp_path):
    # A run.json with no `status` key predates the status lifecycle: such
    # a dump only ever got a run.json at the end, so its presence means
    # the dump finished — classify COMPLETE and keep every load input.
    # Those dumps predate the bronze-resident --debug captures too, hence
    # no screenshots.
    d = make_dump(tmp_path, OLD_TS, status=None, age_s=STALE_S,
                  screenshots=False)
    run_main(tmp_path)
    assert d.exists()
    assert _inputs_intact(d)


def test_validate_target_accepts_screenshots_subdir(tmp_path):
    # The one subdir shape prune may target: the --debug capture dir.
    (tmp_path / OLD_TS / "screenshots").mkdir(parents=True)
    prune.CONFIG.validate_target(tmp_path / OLD_TS / "screenshots", tmp_path)


def test_validate_target_refuses_load_inputs(tmp_path):
    with pytest.raises(SystemExit):
        # a cu_<id>/ subtree is a load input, never a valid target
        prune.CONFIG.validate_target(tmp_path / OLD_TS / f"cu_{CU}", tmp_path)
